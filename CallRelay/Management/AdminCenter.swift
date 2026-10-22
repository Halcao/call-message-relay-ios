import Foundation

/// Observable state + actions behind the native administrator screens.
///
/// One instance per app process. It owns the administrator session (restored
/// from the per-device Keychain), the loaded console sections, and every
/// mutation. All mutations go through `AdminAPI`, so a screen can never
/// fabricate state: a failed action reports the gateway's own reason and the
/// affected section is reloaded from the server afterwards.
@MainActor
final class AdminCenter: ObservableObject {
    enum Section: Hashable, CaseIterable {
        case overview, lines, keys, devices, calls, voicemails, recording, settings, diagnostics

        var title: String {
            switch self {
            case .overview: return "总览"
            case .lines: return "线路"
            case .keys: return "配对密钥"
            case .devices: return "设备"
            case .calls: return "通话与会议"
            case .voicemails: return "语音留言"
            case .recording: return "录音策略"
            case .settings: return "网关设置"
            case .diagnostics: return "诊断与恢复"
            }
        }
    }

    @Published private(set) var session: AdminSession?
    @Published private(set) var overview: AdminWire.Overview?
    @Published private(set) var lines: [AdminWire.Line] = []
    @Published private(set) var keys: [AdminWire.Key] = []
    @Published private(set) var devices: [AdminWire.Device] = []
    @Published private(set) var calls: [AdminWire.Call] = []
    @Published private(set) var conferences: [AdminWire.Conference] = []
    @Published private(set) var voicemails: [AdminWire.Voicemail] = []
    @Published private(set) var voicemailTotalBytes: Int64 = 0
    @Published private(set) var voicemailStorage: AdminWire.VoicemailStorage?
    @Published private(set) var recordingPolicy: AdminWire.RecordingPolicy?
    @Published private(set) var settings: [String: String] = [:]
    @Published private(set) var diagnostics: AdminWire.Diagnostics?
    @Published private(set) var loadingSections: Set<Section> = []
    /// Which session generation inserted each loading flag: cleanup only
    /// removes a flag it owns, so an old refresh can never leave a section
    /// permanently "loading" (which would silently skip every later refresh).
    private var loadingOwner: [Section: UInt64] = [:]
    /// Session generation that owns the in-flight action, if any.
    private var pendingActionOwner: UInt64?
    /// Identifier of the action currently in flight (for row-level spinners).
    @Published private(set) var pendingAction: String?
    @Published private(set) var isSigningIn = false
    @Published private(set) var lastRefresh: Date?
    /// Whether this gateway really serves the administrator API (probed, not
    /// inferred from the wire version).
    @Published private(set) var adminAvailability: AdminAPIAvailability = .unknown
    private var availabilityGeneration: UInt64 = 0
    @Published var errorText: String?
    @Published var noticeText: String?

    /// Monotonic identity of the administrator session state. Every async
    /// read/mutation captures it before awaiting and discards its result when
    /// it no longer matches: a GET that was in flight across a logout, a
    /// gateway switch or a new login can never repopulate state or clear a
    /// newer session's error (root review, build 68).
    private var sessionGeneration: UInt64 = 0
    /// The gateway id the CURRENT session was issued for; a bind to a
    /// different gateway invalidates everything immediately.
    private var boundGatewayId: String?

    var isSignedIn: Bool {
        guard let session else { return false }
        return !session.isExpired
    }

    /// The signed-in administrator's name, for the account row.
    var username: String { session?.username ?? "" }

    func isLoading(_ section: Section) -> Bool { loadingSections.contains(section) }

    // MARK: - Session lifecycle

    #if DEBUG
    /// DEBUG-ONLY screenshot fixture: a synthetic signed-in console rendered
    /// from local sample data. It never authenticates, never reads the
    /// Keychain and never contacts a gateway. Release builds contain neither
    /// the flag nor the seeding code (see `seedPreviewFixture`).
    var previewFixture = false
    #endif

    /// Restores a stored session for the CURRENT gateway. A session issued by
    /// another gateway (or another base URL) is never reused.
    func restore(origin: GatewayOrigin?, gatewayId: String?) async {
        #if DEBUG
        if previewFixture {
            seedPreviewFixture()
            return
        }
        #endif
        guard let origin, let gatewayId else { return }
        let generation = sessionGeneration
        guard let stored = AdminSessionStorage.store.load() else {
            if session != nil { session = nil }
            return
        }
        guard stored.matches(gatewayId: gatewayId, baseURL: origin.baseURL.absoluteString),
              !stored.isExpired else {
            AdminSessionStorage.store.clear()
            session = nil
            return
        }
        guard generation == sessionGeneration else { return }
        // Restoring is establishing a session identity too: older in-flight
        // requests belong to whatever came before, and their operation flags
        // must not survive into the new identity.
        sessionGeneration &+= 1
        clearLoadedState()
        session = stored
        await refreshAll()
    }

    /// Real password login. Nothing is persisted except the issued session.
    func signIn(origin: GatewayOrigin, gatewayId: String, username: String, password: String) async -> Bool {
        guard !isSigningIn else { return false }
        isSigningIn = true
        errorText = nil
        noticeText = nil
        let generation = sessionGeneration
        defer { isSigningIn = false }
        do {
            let answer = try await AdminAPI.login(origin: origin, username: username, password: password)
            // The user unpaired / switched gateway / logged out elsewhere while
            // the password was being checked: this answer belongs to a state
            // that no longer exists. Drop it (and its session material).
            guard generation == sessionGeneration else { return false }
            let session = AdminSession(
                gatewayId: answer.gatewayId.isEmpty ? gatewayId : answer.gatewayId,
                baseURL: origin.baseURL.absoluteString,
                username: answer.username,
                sessionToken: answer.sessionToken,
                csrf: answer.csrf,
                expiresAt: Date(timeIntervalSince1970: TimeInterval(answer.expiresAt) / 1000)
            )
            guard AdminSessionStorage.store.save(session) else {
                errorText = "无法在这台设备上保存管理会话。"
                return false
            }
            // A NEW session identity: every request issued for the previous
            // session (or for no session) becomes stale from here on, so a
            // late 401 from the old session can never clear this one, and no
            // operation flag from before can block the new console.
            sessionGeneration &+= 1
            clearLoadedState()
            self.session = session
            noticeText = "已登录网关管理。"
            await refreshAll()
            return true
        } catch let error as AdminAPIError {
            guard generation == sessionGeneration else { return false }
            errorText = Self.loginFailureText(error)
            return false
        } catch {
            guard generation == sessionGeneration else { return false }
            errorText = "登录失败，请稍后重试。"
            return false
        }
    }

    /// A login against a gateway that has no administrator API must say so
    /// honestly (404/405/501) instead of looking like a wrong password.
    private static func loginFailureText(_ error: AdminAPIError) -> String {
        if case .http(let status, _, let message) = error {
            switch status {
            case 404, 405, 501:
                return "当前网关版本不支持在手机管理，请在网页控制台操作，或升级网关后再试。"
            default:
                if let message, !message.isEmpty { return message }
            }
        }
        return error.friendlyMessage
    }

    /// Ends the session on the gateway AND removes the local Keychain item.
    func signOut(origin: GatewayOrigin?) async {
        let captured = session
        // Invalidate EVERY in-flight request before the awaits below, so a
        // late success can never repopulate the state being cleared.
        sessionGeneration &+= 1
        if let origin, let captured, captured.matches(gatewayId: captured.gatewayId,
                                                      baseURL: origin.baseURL.absoluteString) {
            let api = AdminAPI(origin: origin, token: captured.sessionToken, csrf: captured.csrf)
            try? await api.logout()
        }
        AdminSessionStorage.store.clear()
        session = nil
        overview = nil
        lines = []
        keys = []
        devices = []
        calls = []
        conferences = []
        voicemails = []
        voicemailTotalBytes = 0
        voicemailStorage = nil
        recordingPolicy = nil
        settings = [:]
        diagnostics = nil
        errorText = nil
        noticeText = "已退出网关管理。"
    }

    /// Drops the local session after the gateway rejected it (expired or
    /// revoked elsewhere).
    func invalidateSession(message: String) {
        sessionGeneration &+= 1
        AdminSessionStorage.store.clear()
        session = nil
        clearLoadedState()
        errorText = message
    }

    // MARK: - Capability discovery

    /// Probes the gateway for the administrator API. Unauthenticated, async
    /// and generation-fenced: a late answer for a previous gateway/origin can
    /// never label the current one, and a transport failure leaves the state
    /// UNKNOWN (retry later) instead of claiming "unsupported".
    func refreshAdminAvailability(origin: GatewayOrigin?) async {
        guard let origin else {
            availabilityGeneration &+= 1
            adminAvailability = .unknown
            return
        }
        let sessionGenerationAtStart = sessionGeneration
        availabilityGeneration &+= 1
        let probeGeneration = availabilityGeneration
        let result = await AdminAPI.probeAvailability(origin: origin)
        guard sessionGenerationAtStart == sessionGeneration,
              probeGeneration == availabilityGeneration,
              self.origin?.baseURL == origin.baseURL else { return }
        adminAvailability = result
    }

    #if DEBUG
    /// Test/fixture seam: set the availability without a network probe.
    func setAdminAvailabilityForTesting(_ availability: AdminAPIAvailability) {
        availabilityGeneration &+= 1
        adminAvailability = availability
    }
    #endif

    // MARK: - Loading

    func refreshAll() async {
        guard isSignedIn else { return }
        await withTaskGroup(of: Void.self) { group in
            for section in [Section.overview, .lines, .devices, .calls] {
                group.addTask { @MainActor in await self.refresh(section) }
            }
        }
    }

    func refresh(_ section: Section) async {
        #if DEBUG
        if previewFixture { return }
        #endif
        guard let api = api() else { return }
        let generation = sessionGeneration
        if loadingSections.contains(section) { return }
        loadingSections.insert(section)
        loadingOwner[section] = generation
        defer {
            // Only the generation that inserted the flag may remove it: an
            // abandoned refresh can never clear a newer one's flag, and the
            // identity reset clears everything it might have left behind.
            if loadingOwner[section] == generation {
                loadingSections.remove(section)
                loadingOwner.removeValue(forKey: section)
            }
        }
        guard generation == sessionGeneration else { return }
        do {
            // Fetch into locals and commit ONLY after the generation check:
            // a response that outlived its session must not touch the
            // published state (never assign straight from an await).
            switch section {
            case .overview:
                let fetched = try await api.overview()
                let fetchedLines = try await api.lines()
                guard generation == sessionGeneration else { return }
                overview = fetched
                lines = fetchedLines
            case .lines:
                let fetched = try await api.lines()
                guard generation == sessionGeneration else { return }
                lines = fetched
            case .keys:
                let fetched = try await api.keys()
                let fetchedLines = lines.isEmpty ? try await api.lines() : []
                guard generation == sessionGeneration else { return }
                keys = fetched
                if !fetchedLines.isEmpty { lines = fetchedLines }
            case .devices:
                let fetched = try await api.devices()
                guard generation == sessionGeneration else { return }
                devices = fetched
            case .calls:
                let fetchedCalls = try await api.calls()
                let fetchedConferences = try await api.conferences()
                guard generation == sessionGeneration else { return }
                calls = fetchedCalls
                conferences = fetchedConferences
            case .voicemails:
                let fetched = try await api.voicemails()
                guard generation == sessionGeneration else { return }
                voicemails = fetched.voicemails
                voicemailTotalBytes = fetched.totalBytes
                voicemailStorage = fetched.storage
            case .recording:
                let fetched = try await api.recordingPolicy()
                guard generation == sessionGeneration else { return }
                recordingPolicy = fetched
            case .settings:
                let fetched = try await api.settings()
                guard generation == sessionGeneration else { return }
                settings = fetched
            case .diagnostics:
                let fetched = try await api.diagnostics()
                guard generation == sessionGeneration else { return }
                diagnostics = fetched
            }
            guard generation == sessionGeneration else { return }
            lastRefresh = Date()
        } catch let error as AdminAPIError {
            guard generation == sessionGeneration else { return }
            handle(error)
        } catch {
            guard generation == sessionGeneration else { return }
            errorText = "读取失败，请稍后重试。"
        }
    }

    // MARK: - Actions

    func updateLine(id: String, name: String?, enabled: Bool?, allowInbound: Bool?,
                    allowOutbound: Bool?, rejectPolicy: String?, blockedPrefixes: String?,
                    retentionDays: Int?, smsLive: Bool?) async -> Bool {
        await perform("line-\(id)") { api in
            try await api.updateLine(id: id, name: name, enabled: enabled, allowInbound: allowInbound,
                                     allowOutbound: allowOutbound, rejectPolicy: rejectPolicy,
                                     blockedPrefixes: blockedPrefixes, retentionDays: retentionDays,
                                     smsLive: smsLive)
        } then: { center in
            center.noticeText = "线路设置已保存。"
            await center.refresh(.lines)
        }
    }

    func setLineNumber(id: String, manualNumber: String) async -> Bool {
        await perform("line-number-\(id)") { api in
            try await api.setLineNumber(id: id, manualNumber: manualNumber)
        } then: { center in
            center.noticeText = manualNumber.isEmpty ? "已恢复自动读取号码。" : "号码已保存。"
            await center.refresh(.lines)
        }
    }

    func refreshLineIdentity(id: String) async -> Bool {
        await perform("line-identity-\(id)") { api in
            _ = try await api.refreshLineIdentity(id: id)
        } then: { center in
            center.noticeText = "已重新读取号码。"
            await center.refresh(.lines)
        }
    }

    func createKey(name: String, allLines: Bool, manageNumber: Bool) async -> AdminWire.KeyMaterial? {
        guard let api = api() else { return nil }
        guard pendingAction == nil else { return nil }
        let createGeneration = sessionGeneration
        pendingAction = "key-create"
        pendingActionOwner = createGeneration
        errorText = nil
        defer {
            if pendingActionOwner == createGeneration {
                pendingAction = nil
                pendingActionOwner = nil
            }
        }
        let generation = sessionGeneration
        do {
            let material = try await api.createKey(name: name, allLines: allLines, manageNumber: manageNumber)
            guard generation == sessionGeneration else { return nil }
            noticeText = "密钥已创建，请立即复制或扫码配对。"
            await refresh(.keys)
            return material
        } catch let error as AdminAPIError {
            guard generation == sessionGeneration else { return nil }
            handle(error)
            return nil
        } catch {
            guard generation == sessionGeneration else { return nil }
            errorText = "创建失败，请稍后重试。"
            return nil
        }
    }

    func revealKey(id: String) async -> AdminWire.KeyMaterial? {
        guard let api = api() else { return nil }
        guard pendingAction == nil else { return nil }
        let generation = sessionGeneration
        pendingAction = "key-reveal-\(id)"
        pendingActionOwner = generation
        errorText = nil
        defer {
            if pendingActionOwner == generation {
                pendingAction = nil
                pendingActionOwner = nil
            }
        }
        do {
            let material = try await api.revealKey(id: id)
            guard generation == sessionGeneration else { return nil }
            return material
        } catch let error as AdminAPIError {
            guard generation == sessionGeneration else { return nil }
            handle(error)
            return nil
        } catch {
            guard generation == sessionGeneration else { return nil }
            errorText = "显示密钥失败，请稍后重试。"
            return nil
        }
    }

    func revokeKey(id: String) async -> Bool {
        await perform("key-revoke-\(id)") { api in
            try await api.revokeKey(id: id)
        } then: { center in
            center.noticeText = "密钥已撤销，关联设备与新注册立即失效。"
            await center.refresh(.keys)
        }
    }

    func deleteKey(id: String) async -> Bool {
        await perform("key-delete-\(id)") { api in
            try await api.deleteKey(id: id)
        } then: { center in
            center.noticeText = "已删除该撤销密钥及其线路授权。"
            await center.refresh(.keys)
        }
    }

    func setGrant(keyId: String, lineId: String, receiveSms: Bool, receiveCalls: Bool,
                  sendSms: Bool, dial: Bool) async -> Bool {
        await perform("grant-\(keyId)-\(lineId)") { api in
            try await api.setGrant(keyId: keyId, lineId: lineId, receiveSms: receiveSms,
                                   receiveCalls: receiveCalls, sendSms: sendSms, dial: dial)
        } then: { center in
            center.noticeText = "线路授权已更新。"
            await center.refresh(.keys)
        }
    }

    func deleteGrant(keyId: String, lineId: String) async -> Bool {
        await perform("grant-delete-\(keyId)-\(lineId)") { api in
            try await api.deleteGrant(keyId: keyId, lineId: lineId)
        } then: { center in
            center.noticeText = "授权已移除。"
            await center.refresh(.keys)
        }
    }

    func setKeyManageNumber(id: String, allowed: Bool) async -> Bool {
        await perform("manage-number-\(id)") { api in
            try await api.setKeyManageNumber(id: id, allowed: allowed)
        } then: { center in
            center.noticeText = "号码管理授权已更新。"
            await center.refresh(.keys)
        }
    }

    func revokeDevice(id: String) async -> Bool {
        await perform("device-revoke-\(id)") { api in
            try await api.revokeDevice(id: id)
        } then: { center in
            center.noticeText = "设备已撤销，该安装不会自动重新注册。"
            await center.refresh(.devices)
        }
    }

    func allowDeviceReenroll(id: String) async -> Bool {
        await perform("device-reenroll-\(id)") { api in
            try await api.allowDeviceReenroll(id: id)
        } then: { center in
            center.noticeText = "已允许该安装重新注册。"
            await center.refresh(.devices)
        }
    }

    func hangupCall(id: String) async -> Bool {
        await perform("call-hangup-\(id)") { api in
            try await api.hangupCall(id: id)
        } then: { center in
            center.noticeText = "通话已挂断。"
            await center.refresh(.calls)
        }
    }

    func rejectCall(id: String) async -> Bool {
        await perform("call-reject-\(id)") { api in
            try await api.rejectCall(id: id)
        } then: { center in
            center.noticeText = "来电已拒绝。"
            await center.refresh(.calls)
        }
    }

    func closeConference(id: String) async -> Bool {
        await perform("conference-close-\(id)") { api in
            try await api.closeConference(id: id)
        } then: { center in
            center.noticeText = "会议已结束。"
            await center.refresh(.calls)
        }
    }

    func removeConferenceLeg(conferenceId: String, callId: String) async -> Bool {
        await perform("conference-leg-\(callId)") { api in
            try await api.removeConferenceLeg(conferenceId: conferenceId, callId: callId)
        } then: { center in
            center.noticeText = "会议成员已移除。"
            await center.refresh(.calls)
        }
    }

    func deleteVoicemail(id: String) async -> Bool {
        await perform("voicemail-delete-\(id)") { api in
            try await api.deleteVoicemail(id: id)
        } then: { center in
            center.noticeText = "留言已删除。"
            await center.refresh(.voicemails)
        }
    }

    func voicemailAudio(id: String) async -> Data? {
        guard let api = api() else { return nil }
        let generation = sessionGeneration
        do {
            let data = try await api.voicemailAudio(id: id)
            guard generation == sessionGeneration else { return nil }
            return data
        } catch let error as AdminAPIError {
            guard generation == sessionGeneration else { return nil }
            handle(error)
            return nil
        } catch {
            guard generation == sessionGeneration else { return nil }
            errorText = "无法下载留言音频。"
            return nil
        }
    }

    func setRecording(enabled: Bool?, autoMode: String?) async -> Bool {
        await perform("recording") { api in
            try await api.setRecording(enabled: enabled, autoMode: autoMode)
        } then: { center in
            center.noticeText = "录音策略已保存。"
            await center.refresh(.recording)
        }
    }

    func updateSettings(maxConcurrentCalls: Int?, disconnectGraceSeconds: Int?, voicemailEnabled: Bool?,
                        voicemailRingSeconds: Int?, voicemailOfflineSeconds: Int?, voicemailMaxSeconds: Int?,
                        voicemailRetentionDays: Int?, voicemailMaxBytesMb: Int?, voicemailMinFreeBytesMb: Int?)
        async -> Bool {
        await perform("settings") { api in
            try await api.updateSettings(maxConcurrentCalls: maxConcurrentCalls,
                                         disconnectGraceSeconds: disconnectGraceSeconds,
                                         voicemailEnabled: voicemailEnabled,
                                         voicemailRingSeconds: voicemailRingSeconds,
                                         voicemailOfflineSeconds: voicemailOfflineSeconds,
                                         voicemailMaxSeconds: voicemailMaxSeconds,
                                         voicemailRetentionDays: voicemailRetentionDays,
                                         voicemailMaxBytesMb: voicemailMaxBytesMb,
                                         voicemailMinFreeBytesMb: voicemailMinFreeBytesMb)
        } then: { center in
            center.noticeText = "设置已保存。"
            await center.refresh(.settings)
        }
    }

    func recoverLine(id: String) async -> Bool {
        await perform("recover-\(id)") { api in
            try await api.recoverLine(id: id)
        } then: { center in
            center.noticeText = "线路恢复探测完成。"
            await center.refresh(.diagnostics)
        }
    }

    // MARK: - Plumbing

    private func api() -> AdminAPI? {
        #if DEBUG
        if previewFixture { return nil }
        #endif
        guard let origin = origin, let session, !session.isExpired else { return nil }
        return AdminAPI(origin: origin, token: session.sessionToken, csrf: session.csrf)
    }

    /// The origin of the session's own gateway (never the app's current API
    /// client) so an administrator session can only ever talk to the host it
    /// logged into.
    private(set) var origin: GatewayOrigin?

    func bind(origin: GatewayOrigin?, gatewayId: String?) {
        let identityChanged = origin?.baseURL.absoluteString != self.origin?.baseURL.absoluteString
            || gatewayId != boundGatewayId
        self.origin = origin
        self.gatewayId = gatewayId
        boundGatewayId = gatewayId
        guard identityChanged else { return }
        // A new gateway starts UNKNOWN again: the previous gateway's
        // availability is not evidence for this one.
        availabilityGeneration &+= 1
        adminAvailability = .unknown
        // A different (or removed) gateway: every screen must stop showing the
        // previous gateway's data IMMEDIATELY, and any in-flight request that
        // still lands is ignored (generation bump below).
        sessionGeneration &+= 1
        clearLoadedState()
        if let session, !(origin.map { session.matches(gatewayId: gatewayId ?? "", baseURL: $0.baseURL.absoluteString) } ?? false) {
            // The stored session belongs to another gateway: it is not usable
            // here and must not be presented as this gateway's session.
            self.session = nil
        }
    }

    /// Drops every loaded section AND every piece of operation tracking
    /// without touching the stored session. Called whenever the session
    /// identity changes (bind/unpair/gateway switch, sign-in, restore,
    /// session invalidation): operation flags from the previous identity must
    /// never block the new one.
    private func clearLoadedState() {
        loadingSections = []
        loadingOwner = [:]
        pendingAction = nil
        pendingActionOwner = nil
        overview = nil
        lines = []
        keys = []
        devices = []
        calls = []
        conferences = []
        voicemails = []
        voicemailTotalBytes = 0
        voicemailStorage = nil
        recordingPolicy = nil
        settings = [:]
        diagnostics = nil
        lastRefresh = nil
    }

    private(set) var gatewayId: String?

    /// Runs one mutation, then reloads only what it affected. Errors are
    /// surfaced verbatim; a 401 drops the local session so the login screen
    /// appears instead of a screen that silently does nothing.
    private func perform(_ action: String,
                         _ body: @escaping (AdminAPI) async throws -> Void,
                         then after: @escaping (AdminCenter) async -> Void) async -> Bool {
        guard let api = api() else {
            errorText = "管理会话已过期，请重新登录。"
            return false
        }
        guard pendingAction == nil else { return false }
        let generation = sessionGeneration
        pendingAction = action
        pendingActionOwner = generation
        errorText = nil
        defer {
            if pendingActionOwner == generation {
                pendingAction = nil
                pendingActionOwner = nil
            }
        }
        do {
            try await body(api)
            // The session/gateway changed while the mutation was in flight:
            // its result (and its reload) belong to a dead state.
            guard generation == sessionGeneration else { return false }
            await after(self)
            return true
        } catch let error as AdminAPIError {
            guard generation == sessionGeneration else { return false }
            handle(error)
            return false
        } catch {
            guard generation == sessionGeneration else { return false }
            errorText = "操作失败，请稍后重试。"
            return false
        }
    }

    private func handle(_ error: AdminAPIError) {
        errorText = error.friendlyMessage
        if error.requiresReLogin, session != nil {
            // The gateway rejected THIS session: drop it so every screen shows
            // the login prompt instead of silent no-ops. The generation bump
            // makes any other in-flight request about the old session a no-op.
            sessionGeneration &+= 1
            AdminSessionStorage.store.clear()
            session = nil
            clearLoadedState()
        }
    }
}

// MARK: - Screenshot fixture (DEBUG-only launch argument)

#if DEBUG
extension AdminCenter {
    /// Fills the console with the same shapes the gateway returns, so every
    /// native screen can be reviewed offline. Values are synthetic.
    func seedPreviewFixture() {
        previewFixture = true
        session = AdminSession(gatewayId: "preview-gateway",
                               baseURL: "https://preview.invalid",
                               username: "JiangNanGenius",
                               sessionToken: "preview-token", csrf: "preview-csrf",
                               expiresAt: Date().addingTimeInterval(3600))
        overview = AdminWire.Overview(gatewayId: "preview-gateway", gatewayName: "家庭网关",
                                      fingerprint: "PREVIEW", apiVersion: 2, onlineLines: 2,
                                      lineCount: 2, activeCallCount: 1, pushReady: true,
                                      trickleIce: true)
        lines = [
            AdminWire.Line(id: "line1", name: "主线路", enabled: true, online: true,
                           registration: "registered", operatorName: "中国移动",
                           smsLive: true, phoneNumber: "+61 400 123 456",
                           numberSource: "sim", numberKind: "SIM",
                           numberLabel: "+61 400 123 456", rejectPolicy: "reject_blocked",
                           blockedPrefixes: "400,95", retentionDays: 30, allowInbound: true,
                           allowOutbound: true, bars: 4, lastError: nil),
            AdminWire.Line(id: "line2", name: "备用卡", enabled: false, online: false,
                           registration: "unregistered", operatorName: "—",
                           smsLive: false, phoneNumber: "",
                           numberSource: "empty", numberKind: "",
                           numberLabel: "SIM 未存储号码，可在下方手动设置",
                           rejectPolicy: "none", blockedPrefixes: "", retentionDays: 7,
                           allowInbound: false, allowOutbound: true, bars: 1, lastError: nil),
        ]
        keys = [
            AdminWire.Key(id: "key_primary", name: "我的 iPhone", allLines: false,
                          manageNumber: true, revoked: false, secretRecoverable: true,
                          createdAt: Int64(Date().timeIntervalSince1970 * 1000),
                          revokedAt: nil,
                          grants: [AdminWire.Grant(lineId: "line1", lineName: "主线路",
                                                   receiveSms: true, receiveCalls: true,
                                                   sendSms: true, dial: true)]),
            AdminWire.Key(id: "key_share", name: "家庭共享", allLines: true,
                          manageNumber: false, revoked: true, secretRecoverable: false,
                          createdAt: Int64(Date().addingTimeInterval(-86400).timeIntervalSince1970 * 1000),
                          revokedAt: Int64(Date().addingTimeInterval(-3600).timeIntervalSince1970 * 1000),
                          grants: []),
        ]
        devices = [
            AdminWire.Device(id: "dev_primary", name: "江南的手机", keyId: "key_primary",
                             keyName: "我的 iPhone", defaultLineId: "line1", lineCount: 1,
                             pushRegistered: true, revoked: false,
                             createdAt: Int64(Date().addingTimeInterval(-172800).timeIntervalSince1970 * 1000),
                             revokedAt: nil),
            AdminWire.Device(id: "dev_old", name: "旧手机", keyId: "key_share",
                             keyName: "家庭共享", defaultLineId: "", lineCount: 0,
                             pushRegistered: false, revoked: true,
                             createdAt: Int64(Date().addingTimeInterval(-604800).timeIntervalSince1970 * 1000),
                             revokedAt: Int64(Date().addingTimeInterval(-3600).timeIntervalSince1970 * 1000)),
        ]
        calls = [
            AdminWire.Call(id: "call_ring", lineId: "line1", lineName: "主线路",
                           direction: "inbound", peer: "+61 411 222 333", state: "incoming_ringing",
                           held: false, conferenceId: nil,
                           startedAt: Int64(Date().timeIntervalSince1970 * 1000),
                           connectedAt: nil, endedAt: nil, endReason: nil, mediaTransport: nil),
            AdminWire.Call(id: "call_active", lineId: "line1", lineName: "主线路",
                           direction: "outbound", peer: "+86 139 0000 0000", state: "active",
                           held: false, conferenceId: nil,
                           startedAt: Int64(Date().addingTimeInterval(-95).timeIntervalSince1970 * 1000),
                           connectedAt: Int64(Date().addingTimeInterval(-92).timeIntervalSince1970 * 1000),
                           endedAt: nil, endReason: nil, mediaTransport: "ws"),
        ]
        conferences = [
            AdminWire.Conference(id: "conf_1", hostDeviceId: "dev_primary", hostName: "江南的手机",
                                 state: "active",
                                 createdAt: Int64(Date().addingTimeInterval(-320).timeIntervalSince1970 * 1000),
                                 legs: [AdminWire.Call(id: "call_a", lineId: "line1", lineName: "主线路",
                                                       direction: "outbound", peer: "+86 138 0000 0000",
                                                       state: "active", held: false, conferenceId: "conf_1",
                                                       startedAt: Int64(Date().addingTimeInterval(-300).timeIntervalSince1970 * 1000),
                                                       connectedAt: nil, endedAt: nil, endReason: nil,
                                                       mediaTransport: nil)],
                                 recordingState: nil),
        ]
        voicemails = [
            AdminWire.Voicemail(id: "vm_1", lineId: "line1", lineName: "主线路",
                                peer: "+61 433 444 555", state: "ready", durationMs: 24_000,
                                sizeBytes: 384_000,
                                createdAt: Int64(Date().addingTimeInterval(-7200).timeIntervalSince1970 * 1000),
                                expiresAt: Int64(Date().addingTimeInterval(86400).timeIntervalSince1970 * 1000)),
        ]
        voicemailTotalBytes = 384_000
        voicemailStorage = AdminWire.VoicemailStorage(mounted: true, device: "/dev/mmcblk0p1",
                                                      root: "/srv/call-message-relay",
                                                      freeBytes: 12_000_000_000, usedBytes: 384_000,
                                                      error: nil)
        recordingPolicy = AdminWire.RecordingPolicy(enabled: true, autoMode: "incoming",
                                                    retentionDays: 30, minimumFreeBytes: 268_435_456,
                                                    maxBytes: 524_288_000, storageAvailable: true,
                                                    storageError: nil, freeBytes: 12_000_000_000,
                                                    usedBytes: 384_000, configured: true)
        settings = [
            "max_concurrent_calls": "4", "disconnect_grace_seconds": "45",
            "voicemail_enabled": "1", "voicemail_ring_seconds": "30",
            "voicemail_offline_seconds": "8", "voicemail_max_seconds": "120",
            "voicemail_retention_days": "30", "voicemail_max_bytes": "524288000",
            "voicemail_min_free_bytes": "268435456",
        ]
        diagnostics = AdminWire.Diagnostics(
            lines: lines, events: [], audit: [
                AdminWire.AuditEntry(actor: "JiangNanGenius", action: "line.update",
                                     target: "line1", result: "accepted",
                                     at: Int64(Date().addingTimeInterval(-120).timeIntervalSince1970 * 1000)),
                AdminWire.AuditEntry(actor: "JiangNanGenius", action: "key.create",
                                     target: "key_primary", result: "accepted",
                                     at: Int64(Date().addingTimeInterval(-3600).timeIntervalSince1970 * 1000)),
            ],
            pushReady: true, turnPolicy: "stun", settings: settings)
        lastRefresh = Date()
    }
}
#endif
