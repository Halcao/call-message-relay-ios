import Foundation
import Combine

/// Key-scoped harassment policy state as served by a harassment-capable
/// gateway. One instance per pairing session; the gateway always derives the
/// enrollment key from this device's token, so same-key phones see the same
/// document and a different key on the same line is isolated.
@MainActor
final class HarassmentStore: ObservableObject {
    enum Support: Equatable {
        case unknown
        case supported
        /// Old gateway: no harassment routes. The app keeps working with the
        /// LOCAL rules only and says so honestly.
        case unavailable
    }

    @Published private(set) var support: Support = .unknown
    @Published private(set) var callBlocking = false
    @Published private(set) var smsBlocking = false
    @Published private(set) var contactWhitelist = false
    @Published private(set) var version: Int64 = 0
    @Published private(set) var entries: [HarassmentEntryRecord] = []
    @Published private(set) var lists: [HarassmentListRecord] = []
    @Published private(set) var history: [HarassmentEventRecord] = []
    @Published private(set) var lastError: String?
    /// User-visible result of a contextual action (block from Recents/SMS
    /// details). A failed block is NEVER silently treated as success.
    @Published private(set) var actionNotice: String?
    @Published private(set) var busy = false

    /// Session fence: every async operation captures this before its first
    /// await and refuses to mutate shared state when it changed (unpair /
    /// re-pair / demo switch). A delayed response from the OLD pairing must
    /// never populate the new pairing's lists/history/preferences.
    private var generation: UInt64 = 0

    var blockedEntries: [HarassmentEntryRecord] { entries.filter { !$0.isTrust } }
    var trustedEntries: [HarassmentEntryRecord] { entries.filter(\.isTrust) }

    func reset() {
        generation &+= 1
        support = .unknown
        callBlocking = false
        smsBlocking = false
        contactWhitelist = false
        version = 0
        entries = []
        lists = []
        history = []
        lastError = nil
        actionNotice = nil
        busy = false
    }

    func setActionNotice(_ text: String) { actionNotice = text }
    func clearActionNotice() { actionNotice = nil }

    /// Feature probe + full policy load. A 404 / notReady marks the gateway
    /// as unsupported (local fallback); any other failure keeps the last
    /// known state and surfaces an honest error.
    func refresh(api: GatewayAPI?) async {
        guard let api else {
            support = .unknown
            return
        }
        let gen = generation
        do {
            // Load into locals first: nothing shared is touched until the
            // whole result is known and still belongs to this session.
            let settings = try await api.harassmentSettings()
            let loadedEntries = (try? await api.harassmentEntries()) ?? []
            let loadedLists = (try? await api.harassmentLists()) ?? []
            let loadedHistory = (try? await api.harassmentHistory()) ?? []
            guard gen == generation else { return }
            support = .supported
            callBlocking = settings.callBlocking
            smsBlocking = settings.smsBlocking
            contactWhitelist = settings.contactWhitelistEnabled
            version = settings.version
            lastError = nil
            entries = loadedEntries
            lists = loadedLists
            history = loadedHistory
        } catch {
            guard gen == generation else { return }
            if Self.isUnsupported(error) {
                support = .unavailable
                lastError = nil
            } else {
                lastError = (error as? APIError)?.friendlyMessage ?? "无法读取骚扰拦截设置。"
            }
        }
    }

    nonisolated static func isUnsupported(_ error: Error) -> Bool {
        if case APIError.notReady = error { return true }
        if case APIError.http(let status, _, _) = error, status == 404 { return true }
        return false
    }

    func setToggles(api: GatewayAPI?, call: Bool, sms: Bool) async -> Bool {
        guard let api, support == .supported else { return false }
        let gen = generation
        busy = true
        defer { if generation == gen { busy = false } }
        do {
            let settings = try await api.updateHarassmentSettings(
                callBlocking: call, smsBlocking: sms, contactWhitelist: nil, version: version)
            guard gen == generation else { return false }
            callBlocking = settings.callBlocking
            smsBlocking = settings.smsBlocking
            contactWhitelist = settings.contactWhitelistEnabled
            version = settings.version
            lastError = nil
            return true
        } catch {
            guard gen == generation else { return false }
            if case APIError.http(409, _, _) = error {
                lastError = "设置已在其他设备修改，请刷新后重试。"
                await refresh(api: api)
            } else {
                lastError = (error as? APIError)?.friendlyMessage ?? "无法保存骚扰拦截设置。"
            }
            return false
        }
    }

    /// Mirrors the contact-whitelist switch (contacts themselves are never
    /// uploaded implicitly; only the owner's explicit contact sync does that).
    @discardableResult
    func setContactWhitelist(api: GatewayAPI?, _ enabled: Bool) async -> Bool {
        guard let api, support == .supported else { return false }
        let gen = generation
        do {
            let settings = try await api.updateHarassmentSettings(
                callBlocking: callBlocking, smsBlocking: smsBlocking,
                contactWhitelist: enabled, version: version)
            guard gen == generation else { return false }
            contactWhitelist = settings.contactWhitelistEnabled
            version = settings.version
            lastError = nil
            return true
        } catch {
            guard gen == generation else { return false }
            if case APIError.http(409, _, _) = error {
                lastError = "设置已在其他设备修改，请刷新后重试。"
                await refresh(api: api)
            } else {
                lastError = (error as? APIError)?.friendlyMessage ?? "无法保存通讯录信任开关。"
            }
            return false
        }
    }

    @discardableResult
    func addEntry(api: GatewayAPI?, number: String, matchKind: String, mode: String, action: String?, replaceOpposite: Bool = false) async -> HarassmentEntryAddResult? {
        guard let api, support == .supported else { return nil }
        let gen = generation
        busy = true
        defer { if generation == gen { busy = false } }
        do {
            let result = try await api.addHarassmentEntry(
                number: number, matchKind: matchKind, mode: mode, action: action,
                replaceOpposite: replaceOpposite)
            guard gen == generation else { return nil }
            entries.removeAll { $0.id == result.entry.id }
            entries.insert(result.entry, at: 0)
            if let replaced = result.replaced, replaced > 0 {
                let reloaded = (try? await api.harassmentEntries()) ?? entries
                guard gen == generation else { return nil }
                entries = reloaded
            }
            lastError = nil
            return result
        } catch {
            guard gen == generation else { return nil }
            lastError = (error as? APIError)?.friendlyMessage ?? "无法添加规则。"
            return nil
        }
    }

    @discardableResult
    func deleteEntry(api: GatewayAPI?, id: String) async -> Bool {
        guard let api, support == .supported else { return false }
        let gen = generation
        do {
            try await api.deleteHarassmentEntry(id: id)
            guard gen == generation else { return false }
            entries.removeAll { $0.id == id }
            lastError = nil
            return true
        } catch {
            guard gen == generation else { return false }
            if case APIError.http(404, _, _) = error {
                entries.removeAll { $0.id == id }
                return true
            }
            lastError = (error as? APIError)?.friendlyMessage ?? "无法删除规则。"
            return false
        }
    }

    @discardableResult
    func importList(api: GatewayAPI?, name: String, numbers: [String], defaultAction: String) async -> Bool {
        guard let api, support == .supported else { return false }
        let gen = generation
        busy = true
        defer { if generation == gen { busy = false } }
        do {
            let list = try await api.importHarassmentList(
                name: name, numbers: numbers, defaultAction: defaultAction)
            let reloaded = (try? await api.harassmentEntries()) ?? entries
            guard gen == generation else { return false }
            lists.insert(list, at: 0)
            entries = reloaded
            lastError = nil
            return true
        } catch {
            guard gen == generation else { return false }
            lastError = (error as? APIError)?.friendlyMessage ?? "无法导入名单。"
            return false
        }
    }

    @discardableResult
    func deleteList(api: GatewayAPI?, id: String) async -> Bool {
        guard let api, support == .supported else { return false }
        let gen = generation
        do {
            try await api.deleteHarassmentList(id: id)
            let reloaded = (try? await api.harassmentEntries()) ?? entries
            guard gen == generation else { return false }
            lists.removeAll { $0.id == id }
            entries = reloaded
            lastError = nil
            return true
        } catch {
            guard gen == generation else { return false }
            if case APIError.http(404, _, _) = error {
                lists.removeAll { $0.id == id }
                return true
            }
            lastError = (error as? APIError)?.friendlyMessage ?? "无法删除名单。"
            return false
        }
    }

    @discardableResult
    func clearHistory(api: GatewayAPI?) async -> Bool {
        guard let api, support == .supported else { return false }
        let gen = generation
        do {
            try await api.clearHarassmentHistory()
            guard gen == generation else { return false }
            history = []
            lastError = nil
            return true
        } catch {
            guard gen == generation else { return false }
            lastError = (error as? APIError)?.friendlyMessage ?? "无法清空拦截记录。"
            return false
        }
    }

    func test(api: GatewayAPI?, kind: String, number: String, body: String) async -> HarassmentDecisionRecord? {
        guard let api, support == .supported else { return nil }
        let gen = generation
        do {
            let decision = try await api.testHarassment(kind: kind, number: number, body: body)
            guard gen == generation else { return nil }
            return decision
        } catch {
            guard gen == generation else { return nil }
            lastError = (error as? APIError)?.friendlyMessage ?? "规则测试失败。"
            return nil
        }
    }
}

/// Recording configuration from the gateway: operator auto mode, THIS
/// device's own auto preference (default OFF), retention and storage state.
@MainActor
final class RecordingConfigStore: ObservableObject {
    @Published private(set) var supported: Bool?
    @Published private(set) var enabled = false
    @Published private(set) var autoMode = "off"
    @Published private(set) var deviceAutoRecord = false
    @Published private(set) var retentionDays = 0
    @Published private(set) var storageUnavailable = false
    /// Build-67 switch honesty: a write in flight (the switch shows the
    /// user's intent while the gateway round-trips) and the last rejection,
    /// so an ON→OFF bounce is EXPLAINED instead of silent.
    @Published private(set) var pendingWrite = false
    /// Overlapping writes: the flag clears only when the LAST one returns.
    private var pendingWriteCount = 0
    @Published private(set) var lastError: String?
    /// Last value the gateway CONFIRMED (a rejected write restores this,
    /// never a fake ON state).
    private var lastConfirmedDeviceAutoRecord = false
    /// A refresh that STARTED before the latest write must not overwrite the
    /// value that write established (the ON→OFF bounce class).
    private var writeGeneration: UInt64 = 0

    /// True once the gateway's recording settings were actually read: without
    /// it no retention/auto state may be presented (never a guess).
    @Published private(set) var loaded = false

    /// The switch cannot do anything while the gateway policy forbids it or
    /// the storage is unusable. Unknown state is NOT treated as ineffective
    /// (the switch stays usable until the gateway says otherwise).
    var autoRecordIneffective: Bool {
        guard loaded else { return false }
        return autoMode == "off" || storageUnavailable
    }

    /// Whether the device auto-record switch may be toggled right now.
    ///
    /// A user must ALWAYS be able to opt OUT: a policy that blocks ENABLING
    /// (gateway auto mode off, storage unavailable) disables the switch only
    /// while it is off. A confirmed ON stays switchable so it can be turned
    /// off at any time; a pending write blocks all toggling.
    var autoRecordToggleDisabled: Bool {
        pendingWrite || (autoRecordIneffective && !deviceAutoRecord)
    }

    /// Concise, source-backed reason for the disabled switch, or nil.
    var autoRecordReason: String? {
        guard loaded else { return nil }
        if autoMode == "off" { return String(localized: "网关未允许自动录音。") }
        if storageUnavailable { return String(localized: "录音存储不可用，自动录音不会开始。") }
        return nil
    }

    var autoModeLabel: String {
        switch autoMode {
        case "all": return "全部通话"
        case "incoming": return "仅来电"
        case "outgoing": return "仅去电"
        default: return "关闭"
        }
    }

    /// Session fence, same contract as HarassmentStore: a delayed response
    /// from an old pairing can never set the new pairing's preference.
    private var generation: UInt64 = 0

    func reset() {
        generation &+= 1
        writeGeneration &+= 1
        supported = nil
        enabled = false
        autoMode = "off"
        deviceAutoRecord = false
        lastConfirmedDeviceAutoRecord = false
        retentionDays = 0
        storageUnavailable = false
        loaded = false
        pendingWrite = false
        pendingWriteCount = 0
        lastError = nil
    }

    func refresh(api: GatewayAPI?) async {
        guard let api else {
            supported = nil
            return
        }
        let gen = generation
        let writeGen = writeGeneration
        do {
            let settings = try await api.recordingSettings()
            // Fence BOTH the wall-clock generation and the in-flight write:
            // a refresh that started (or returns) while a write is pending
            // carries a PRE-write server value and must never overwrite the
            // user's chosen switch state or the confirmed value.
            guard gen == generation, writeGen == writeGeneration,
                  !pendingWrite else { return }
            supported = true
            loaded = true
            enabled = settings.enabled
            autoMode = settings.autoMode
            deviceAutoRecord = settings.deviceAutoRecord
            lastConfirmedDeviceAutoRecord = settings.deviceAutoRecord
            retentionDays = settings.retentionDays
            let storage = settings.storage
            storageUnavailable = !(storage?.mounted ?? true)
                || (storage?.error?.isEmpty == false)
            lastError = nil
        } catch {
            guard gen == generation, writeGen == writeGeneration, !pendingWrite else { return }
            if HarassmentStore.isUnsupported(error) {
                supported = false
            }
            // Other failures keep the previous view; the in-call control
            // still learns the truth from server errors/events. The error is
            // recorded so the switch's state is never unexplained.
            lastError = (error as? APIError)?.friendlyMessage ?? "无法读取录音设置。"
            DiagnosticsStore.shared.log("settings", "recording settings load failed")
        }
    }

    @discardableResult
    func setDeviceAutoRecord(api: GatewayAPI?, _ enabled: Bool) async -> Bool {
        guard let api else {
            lastError = String(localized: "尚未配对网关，无法保存录音设置。")
            return false
        }
        let gen = generation
        // Pending intent: the switch shows what the user just chose while
        // the gateway round-trips; a refresh racing the write can neither
        // flip it back nor be overwritten by it.
        writeGeneration &+= 1
        let writeID = writeGeneration
        pendingWriteCount += 1
        pendingWrite = true
        deviceAutoRecord = enabled
        lastError = nil
        defer {
            // Only the LAST returning write clears the pending state; an
            // older write's defer must never clear a newer writer's flag.
            pendingWriteCount = max(0, pendingWriteCount - 1)
            if pendingWriteCount == 0 { pendingWrite = false }
        }
        do {
            let result = try await api.updateDeviceAutoRecord(enabled)
            guard gen == generation else { return false }
            // A newer write superseded this one: its outcome owns the state.
            guard writeID == writeGeneration else { return false }
            writeGeneration &+= 1
            deviceAutoRecord = result
            lastConfirmedDeviceAutoRecord = result
            supported = true
            DiagnosticsStore.shared.log("settings", "auto-record update ok=\(result ? 1 : 0)")
            return true
        } catch {
            guard gen == generation else { return false }
            guard writeID == writeGeneration else { return false }
            writeGeneration &+= 1
            if HarassmentStore.isUnsupported(error) {
                supported = false
            }
            // The gateway rejected the change: restore the last CONFIRMED
            // value (never a fake ON) and explain why.
            deviceAutoRecord = lastConfirmedDeviceAutoRecord
            let reason = (error as? APIError)?.friendlyMessage
            lastError = reason.map { String(localized: "录音开关保存失败：\($0)") }
                ?? String(localized: "录音开关保存失败，请检查网络后重试。")
            // Bounded, privacy-safe failure evidence: the HTTP status and the
            // gateway's own error code make the next field export explain
            // exactly WHY the switch could not be saved (no tokens/PII).
            var detail = "auto-record update failed"
            if case APIError.http(let status, let code, _)? = error as? APIError {
                detail += " status=\(status)"
                if let code { detail += " code=\(code)" }
            }
            DiagnosticsStore.shared.log("settings", detail)
            return false
        }
    }
}
