import Foundation

/// Wire models of the gateway's additive administrator API
/// (`/api/v2/admin/...`, build 68). The shapes mirror the HTML console's
/// business objects one-for-one; the native screens never scrape HTML.
enum AdminWire {}

extension AdminWire {
    struct Overview: Decodable {
        let gatewayId: String
        let gatewayName: String?
        let fingerprint: String?
        let apiVersion: Int?
        let onlineLines: Int
        let lineCount: Int
        let activeCallCount: Int
        let pushReady: Bool
        let trickleIce: Bool?
    }

    struct Line: Decodable, Identifiable, Equatable {
        let id: String
        let name: String
        let enabled: Bool
        let online: Bool
        let registration: String
        let operatorName: String?
        let smsLive: Bool
        let phoneNumber: String
        let numberSource: String
        let numberKind: String
        let numberLabel: String
        let rejectPolicy: String
        let blockedPrefixes: String
        let retentionDays: Int
        let allowInbound: Bool
        let allowOutbound: Bool
        let bars: Int
        let lastError: String?

        enum CodingKeys: String, CodingKey {
            case id, name, enabled, online, registration, smsLive, phoneNumber
            case numberSource, numberKind, numberLabel, rejectPolicy, blockedPrefixes
            case retentionDays, allowInbound, allowOutbound, bars, lastError
            case operatorName = "operator"
        }
    }

    struct LineList: Decodable { let lines: [Line] }

    struct Grant: Decodable, Identifiable, Equatable {
        let lineId: String
        let lineName: String
        let receiveSms: Bool
        let receiveCalls: Bool
        let sendSms: Bool
        let dial: Bool
        var id: String { lineId }
    }

    struct Key: Decodable, Identifiable, Equatable {
        let id: String
        let name: String
        let allLines: Bool
        let manageNumber: Bool
        let revoked: Bool
        let secretRecoverable: Bool
        let createdAt: Int64
        let revokedAt: Int64?
        let grants: [Grant]
    }

    struct KeyList: Decodable { let keys: [Key] }

    struct KeyMaterial: Decodable {
        let keyId: String
        let enrollmentKey: String
        let pairingPayload: String
        let qrDataUri: String?
    }

    struct Device: Decodable, Identifiable, Equatable {
        let id: String
        let name: String
        let keyId: String
        let keyName: String
        let defaultLineId: String
        let lineCount: Int
        let pushRegistered: Bool
        let revoked: Bool
        let createdAt: Int64
        let revokedAt: Int64?
    }

    struct DeviceList: Decodable { let devices: [Device] }

    struct Call: Decodable, Identifiable, Equatable {
        let id: String
        let lineId: String
        let lineName: String?
        let direction: String
        let peer: String
        let state: String
        let held: Bool
        let conferenceId: String?
        let startedAt: Int64
        let connectedAt: Int64?
        let endedAt: Int64?
        let endReason: String?
        let mediaTransport: String?
    }

    struct CallList: Decodable { let calls: [Call] }

    struct Conference: Decodable, Identifiable, Equatable {
        let id: String
        let hostDeviceId: String
        let hostName: String?
        let state: String
        let createdAt: Int64
        let legs: [Call]
        let recordingState: String?
    }

    struct ConferenceList: Decodable { let conferences: [Conference] }

    struct Voicemail: Decodable, Identifiable, Equatable {
        let id: String
        let lineId: String
        let lineName: String?
        let peer: String
        let state: String
        let durationMs: Int64
        let sizeBytes: Int64
        let createdAt: Int64
        let expiresAt: Int64
    }

    struct VoicemailStorage: Decodable, Equatable {
        let mounted: Bool?
        let device: String?
        let root: String?
        let freeBytes: Int64?
        let usedBytes: Int64?
        let error: String?
    }

    struct VoicemailList: Decodable {
        let voicemails: [Voicemail]
        let totalBytes: Int64
        let storage: VoicemailStorage?
    }

    struct RecordingPolicy: Decodable, Equatable {
        let enabled: Bool
        let autoMode: String
        let retentionDays: Int
        let minimumFreeBytes: Int64
        let maxBytes: Int64
        let storageAvailable: Bool
        let storageError: String?
        let freeBytes: Int64?
        let usedBytes: Int64?
        let configured: Bool
    }

    struct SettingsEnvelope: Decodable { let settings: [String: String] }

    struct Event: Decodable {
        let id: String?
        let kind: String?
        let at: Int64?
        let lineId: String?
        let summary: String?
    }

    struct AuditEntry: Decodable, Identifiable {
        let actor: String
        let action: String
        let target: String
        let result: String
        let at: Int64
        var id: String { "\(at)-\(action)-\(target)-\(result)" }
    }

    struct Diagnostics: Decodable {
        let lines: [Line]
        let events: [Event]?
        let audit: [AuditEntry]?
        let pushReady: Bool
        let turnPolicy: String?
        let settings: [String: String]
    }

    struct LoginAnswer: Decodable {
        let username: String
        let sessionToken: String
        let csrf: String
        let expiresAt: Int64
        let gatewayId: String
    }

    struct SessionInfo: Decodable {
        let username: String
        let csrf: String
        let expiresAt: Int64
        let gatewayId: String
    }

    struct ErrorBody: Decodable {
        let error: String?
        let message: String?
    }
}

/// Whether THIS gateway really serves the administrator API.
///
/// It cannot be inferred from the wire version: a v2 gateway deployed before
/// build 68 answers `/api/v2/admin/...` with 404 (verified live). The only
/// honest evidence is an unauthenticated probe of the route itself.
enum AdminAPIAvailability: Equatable {
    /// Not yet probed, or the probe could not decide (offline/timeout):
    /// NEVER presented as "unsupported".
    case unknown
    /// The route exists (its own 401 JSON / a 200).
    case available
    /// The gateway answered 404/405/501 for the admin route: it has no
    /// administrator API.
    case unsupported
}

/// Administrator API failures, with copy a person can act on. Server messages
/// are already concise Chinese and are surfaced verbatim.
enum AdminAPIError: Error, Equatable {
    case notConfigured
    case unauthorized(String)
    case csrf(String)
    case conflict(code: String?, message: String)
    case http(status: Int, code: String?, message: String?)
    case network(String)
    case decoding

    var friendlyMessage: String {
        switch self {
        case .notConfigured: return "这台设备还没有可用的网关地址。"
        case .unauthorized(let message): return message
        case .csrf(let message): return message
        case .conflict(_, let message): return message
        case .http(let status, _, let message):
            if let message, !message.isEmpty { return message }
            return "网关返回错误（HTTP \(status)）。"
        case .network(let message): return message
        case .decoding: return "网关返回的数据无法识别（协议不匹配）。"
        }
    }

    var requiresReLogin: Bool {
        if case .unauthorized = self { return true }
        return false
    }
}
