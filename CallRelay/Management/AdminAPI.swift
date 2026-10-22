import Foundation

/// Independent JSON client for the gateway's administrator API
/// (`/api/v2/admin/`, build 68).
///
/// Deliberately SEPARATE from `HTTPGatewayAPI`:
/// * it never uses the device bearer token or the pairing key store, so a
///   compromised or rotated device pairing can never silently become an
///   administrator session;
/// * it carries its own ephemeral URLSession with no cookie storage and no
///   response cache, plus the same strict same-origin redirect policy, so an
///   administrator session can never be replayed to another origin;
/// * its only credential is an administrator session token (kept in the
///   per-device Keychain item), presented as `Authorization: Bearer`, with
///   the session CSRF token in the `X-CallRelay-CSRF` header ONLY on
///   state-changing requests (never in a URL, which would reach logs).
final class AdminAPI: @unchecked Sendable {
    /// Test seam ONLY: when set (tests), every administrator URLSession is
    /// built from this configuration instead of the standard ephemeral one.
    /// Production never sets it.
    static var sessionConfigurationOverride: URLSessionConfiguration?
    /// Test seam ONLY: a concurrent delegate queue lets a deliberately parked
    /// (test-gated) response stop blocking other requests on the same session.
    /// Production keeps the default serial queue.
    static var sessionDelegateQueueOverride: OperationQueue?

    private let origin: GatewayOrigin
    private let session: URLSession
    private let token: String
    private let csrf: String

    init(origin: GatewayOrigin, token: String, csrf: String) {
        self.origin = origin
        self.token = token
        self.csrf = csrf
        self.session = AdminAPI.makeSession(origin: origin)
    }

    private static func makeSession(origin: GatewayOrigin) -> URLSession {
        let config = sessionConfigurationOverride ?? URLSessionConfiguration.ephemeral
        config.timeoutIntervalForRequest = 20
        config.timeoutIntervalForResource = 40
        config.httpShouldSetCookies = false
        config.httpCookieAcceptPolicy = .never
        config.httpCookieStorage = nil
        config.requestCachePolicy = .reloadIgnoringLocalCacheData
        config.urlCache = nil
        return URLSession(configuration: config,
                          delegate: SecureRedirectDelegate(allowedOrigin: origin),
                          delegateQueue: sessionDelegateQueueOverride)
    }

    // MARK: - Authentication (uses no session token)

    /// Password login. The password exists only for the duration of this call
    /// and is never persisted anywhere (not even in memory beyond the call).
    static func login(origin: GatewayOrigin, username: String, password: String) async throws -> AdminWire.LoginAnswer {
        var request = URLRequest(url: origin.apiURL("admin/login"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try? JSONSerialization.data(withJSONObject: [
            "username": username, "password": password,
        ])
        let session = makeSession(origin: origin)
        let data = try await perform(session: session, request: request)
        do {
            return try JSONDecoder().decode(AdminWire.LoginAnswer.self, from: data)
        } catch {
            throw AdminAPIError.decoding
        }
    }

    /// Unauthenticated availability probe for the administrator API.
    ///
    /// `GET /api/v2/admin/session` without a session:
    /// * 401 whose body is EXACTLY the admin API's `{"error":"unauthorized"}`
    ///   contract ⇒ the route EXISTS;
    /// * a 200 whose body decodes as the admin session shape ⇒ the route
    ///   exists (an HTML/proxy 200 does not count);
    /// * 404 / 405 / 501 ⇒ the gateway has no administrator API (older build);
    /// * anything else — a generic 403, an HTML page, a transport failure —
    ///   ⇒ UNKNOWN, so the UI never claims "unsupported" from a hick-up.
    /// No credentials, no side effects, bounded by the session's timeouts.
    static func probeAvailability(origin: GatewayOrigin) async -> AdminAPIAvailability {
        var request = URLRequest(url: origin.apiURL("admin/session"))
        request.httpMethod = "GET"
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        let session = makeSession(origin: origin)
        do {
            let (data, response) = try await session.data(for: request)
            guard let http = response as? HTTPURLResponse else { return .unknown }
            switch http.statusCode {
            case 200:
                // Only a SESSION-SHAPED body proves the route: an HTML
                // captive portal (or any other 200 page) is not the API.
                return (try? JSONDecoder().decode(AdminWire.SessionInfo.self, from: data)) != nil
                    ? .available : .unknown
            case 401:
                // The live contract for an unauthenticated admin request is
                // 401 with {"error":"unauthorized", ...}. A generic 401 (a
                // proxy's own login page, a different API) is NOT evidence.
                guard let body = try? JSONDecoder().decode(AdminWire.ErrorBody.self, from: data),
                      body.error == "unauthorized" else {
                    return .unknown
                }
                return .available
            case 404, 405, 501:
                return .unsupported
            default:
                // 403 (proxy/CSRF), 5xx and everything else: unknown. Never
                // "this gateway does not support it".
                return .unknown
            }
        } catch {
            return .unknown
        }
    }

    // MARK: - Session-relative requests

    func sessionInfo() async throws -> AdminWire.SessionInfo { try await get("session") }
    func logout() async throws { try await postVoid("logout", body: [:]) }

    // MARK: - Reads

    func overview() async throws -> AdminWire.Overview { try await get("overview") }
    func lines() async throws -> [AdminWire.Line] { try await get("lines", as: AdminWire.LineList.self).lines }
    func keys() async throws -> [AdminWire.Key] { try await get("keys", as: AdminWire.KeyList.self).keys }
    func devices() async throws -> [AdminWire.Device] { try await get("devices", as: AdminWire.DeviceList.self).devices }
    func calls() async throws -> [AdminWire.Call] { try await get("calls", as: AdminWire.CallList.self).calls }
    func conferences() async throws -> [AdminWire.Conference] {
        try await get("conferences", as: AdminWire.ConferenceList.self).conferences
    }
    func voicemails() async throws -> AdminWire.VoicemailList { try await get("voicemails") }
    func recordingPolicy() async throws -> AdminWire.RecordingPolicy { try await get("recording") }
    func settings() async throws -> [String: String] {
        try await get("settings", as: AdminWire.SettingsEnvelope.self).settings
    }
    func diagnostics() async throws -> AdminWire.Diagnostics { try await get("diagnostics") }

    /// Authenticated voicemail audio download (the administrator plane never
    /// exposes an unauthenticated media URL).
    func voicemailAudio(id: String) async throws -> Data {
        try await request(method: "GET", path: "voicemails/\(escaped(id))/audio", body: nil)
    }

    // MARK: - Mutations

    func updateLine(id: String, name: String?, enabled: Bool?, allowInbound: Bool?,
                    allowOutbound: Bool?, rejectPolicy: String?, blockedPrefixes: String?,
                    retentionDays: Int?, smsLive: Bool?) async throws {
        var body: [String: Any] = [:]
        if let name { body["name"] = name }
        if let enabled { body["enabled"] = enabled }
        if let allowInbound { body["allowInbound"] = allowInbound }
        if let allowOutbound { body["allowOutbound"] = allowOutbound }
        if let rejectPolicy { body["rejectPolicy"] = rejectPolicy }
        if let blockedPrefixes { body["blockedPrefixes"] = blockedPrefixes }
        if let retentionDays { body["retentionDays"] = retentionDays }
        if let smsLive { body["smsLive"] = smsLive }
        try await postVoid("lines/\(escaped(id))", body: body)
    }

    func setLineNumber(id: String, manualNumber: String) async throws {
        try await postVoid("lines/\(escaped(id))/number", body: ["manualNumber": manualNumber])
    }

    @discardableResult
    func refreshLineIdentity(id: String) async throws -> [String: Any] {
        try await postRaw("lines/\(escaped(id))/refresh-identity", body: [:])
    }

    func createKey(name: String, allLines: Bool, manageNumber: Bool) async throws -> AdminWire.KeyMaterial {
        try await post("keys", body: ["name": name, "allLines": allLines, "manageNumber": manageNumber],
                       as: AdminWire.KeyMaterial.self)
    }

    func revealKey(id: String) async throws -> AdminWire.KeyMaterial {
        try await post("keys/\(escaped(id))/reveal", body: [:], as: AdminWire.KeyMaterial.self)
    }

    func revokeKey(id: String) async throws { try await postVoid("keys/\(escaped(id))/revoke", body: [:]) }
    func deleteKey(id: String) async throws { try await postVoid("keys/\(escaped(id))/delete", body: [:]) }

    func setGrant(keyId: String, lineId: String, receiveSms: Bool, receiveCalls: Bool,
                  sendSms: Bool, dial: Bool) async throws {
        try await postVoid("keys/\(escaped(keyId))/grant", body: [
            "lineId": lineId, "receiveSms": receiveSms, "receiveCalls": receiveCalls,
            "sendSms": sendSms, "dial": dial,
        ])
    }

    func deleteGrant(keyId: String, lineId: String) async throws {
        try await postVoid("keys/\(escaped(keyId))/grant/\(escaped(lineId))/delete", body: [:])
    }

    func setKeyManageNumber(id: String, allowed: Bool) async throws {
        try await postVoid("keys/\(escaped(id))/manage-number", body: ["manageNumber": allowed])
    }

    func revokeDevice(id: String) async throws { try await postVoid("devices/\(escaped(id))/revoke", body: [:]) }
    func allowDeviceReenroll(id: String) async throws {
        try await postVoid("devices/\(escaped(id))/allow-reenroll", body: [:])
    }

    func hangupCall(id: String) async throws { try await postVoid("calls/\(escaped(id))/hangup", body: [:]) }
    func rejectCall(id: String) async throws { try await postVoid("calls/\(escaped(id))/reject", body: [:]) }
    func closeConference(id: String) async throws {
        try await postVoid("conferences/\(escaped(id))/close", body: [:])
    }
    func removeConferenceLeg(conferenceId: String, callId: String) async throws {
        try await postVoid("conferences/\(escaped(conferenceId))/legs/\(escaped(callId))/hangup", body: [:])
    }

    func deleteVoicemail(id: String) async throws {
        try await requestVoid(method: "DELETE", path: "voicemails/\(escaped(id))", body: nil)
    }

    func setRecording(enabled: Bool?, autoMode: String?) async throws {
        var body: [String: Any] = [:]
        if let enabled { body["enabled"] = enabled }
        if let autoMode { body["autoMode"] = autoMode }
        try await postVoid("recording", body: body)
    }

    func updateSettings(maxConcurrentCalls: Int?, disconnectGraceSeconds: Int?, voicemailEnabled: Bool?,
                        voicemailRingSeconds: Int?, voicemailOfflineSeconds: Int?, voicemailMaxSeconds: Int?,
                        voicemailRetentionDays: Int?, voicemailMaxBytesMb: Int?, voicemailMinFreeBytesMb: Int?)
        async throws {
        var body: [String: Any] = [:]
        if let maxConcurrentCalls { body["max_concurrent_calls"] = maxConcurrentCalls }
        if let disconnectGraceSeconds { body["disconnect_grace_seconds"] = disconnectGraceSeconds }
        if let voicemailEnabled { body["voicemailEnabled"] = voicemailEnabled }
        if let voicemailRingSeconds { body["voicemail_ring_seconds"] = voicemailRingSeconds }
        if let voicemailOfflineSeconds { body["voicemail_offline_seconds"] = voicemailOfflineSeconds }
        if let voicemailMaxSeconds { body["voicemail_max_seconds"] = voicemailMaxSeconds }
        if let voicemailRetentionDays { body["voicemail_retention_days"] = voicemailRetentionDays }
        if let voicemailMaxBytesMb { body["voicemailMaxBytesMb"] = voicemailMaxBytesMb }
        if let voicemailMinFreeBytesMb { body["voicemailMinFreeBytesMb"] = voicemailMinFreeBytesMb }
        try await postVoid("settings", body: body)
    }

    func recoverLine(id: String) async throws {
        try await postVoid("diagnostics/recover/\(escaped(id))", body: [:])
    }

    // MARK: - Transport

    private func escaped(_ value: String) -> String {
        value.addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) ?? value
    }

    private func get<T: Decodable>(_ path: String, as type: T.Type) async throws -> T {
        let data = try await request(method: "GET", path: path, body: nil)
        return try decode(data)
    }

    private func get<T: Decodable>(_ path: String) async throws -> T { try await get(path, as: T.self) }

    private func postVoid(_ path: String, body: [String: Any]) async throws {
        try await requestVoid(method: "POST", path: path, body: body)
    }

    private func postRaw(_ path: String, body: [String: Any]) async throws -> [String: Any] {
        let data = try await request(method: "POST", path: path, body: body)
        return (try? JSONSerialization.jsonObject(with: data) as? [String: Any]) ?? [:]
    }

    private func post<T: Decodable>(_ path: String, body: [String: Any], as type: T.Type) async throws -> T {
        let data = try await request(method: "POST", path: path, body: body)
        return try decode(data)
    }

    private func decode<T: Decodable>(_ data: Data) throws -> T {
        do {
            return try JSONDecoder().decode(T.self, from: data)
        } catch {
            throw AdminAPIError.decoding
        }
    }

    private func requestVoid(method: String, path: String, body: [String: Any]?) async throws {
        _ = try await request(method: method, path: path, body: body)
    }

    private func request(method: String, path: String, body: [String: Any]?) async throws -> Data {
        var request = URLRequest(url: origin.apiURL("admin/\(path)"))
        request.httpMethod = method
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if method != "GET" && method != "HEAD" {
            request.setValue(csrf, forHTTPHeaderField: "X-CallRelay-CSRF")
        }
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try? JSONSerialization.data(withJSONObject: body)
        }
        return try await Self.perform(session: session, request: request)
    }

    private static func perform(session: URLSession, request: URLRequest) async throws -> Data {
        do {
            let (data, response) = try await session.data(for: request)
            return try validate(data: data, response: response)
        } catch let error as AdminAPIError {
            throw error
        } catch let error as URLError {
            if error.code == .cancelled { throw AdminAPIError.network("操作已取消。") }
            throw AdminAPIError.network("无法连接到网关，请检查网络。")
        } catch {
            throw AdminAPIError.network("无法连接到网关，请检查网络。")
        }
    }

    /// Status/error mapping. 401 means the session must be re-established;
    /// 403 is a CSRF/fence failure; 409 carries the gateway's honest reason
    /// (stale key, active key, revoked key, unavailable action).
    private static func validate(data: Data, response: URLResponse) throws -> Data {
        guard let http = response as? HTTPURLResponse else {
            throw AdminAPIError.network("网关连接异常。")
        }
        guard (200..<300).contains(http.statusCode) else {
            let body = try? JSONDecoder().decode(AdminWire.ErrorBody.self, from: data)
            let code = body?.error
            let message = body?.message ?? ""
            switch http.statusCode {
            case 401:
                throw AdminAPIError.unauthorized(message.isEmpty ? "管理会话已过期，请重新登录。" : message)
            case 403:
                throw AdminAPIError.csrf(message.isEmpty ? "安全校验失败，请重新登录。" : message)
            case 409:
                throw AdminAPIError.conflict(code: code, message: message.isEmpty ? "该操作当前不可用。" : message)
            default:
                throw AdminAPIError.http(status: http.statusCode, code: code, message: message)
            }
        }
        return data
    }
}
