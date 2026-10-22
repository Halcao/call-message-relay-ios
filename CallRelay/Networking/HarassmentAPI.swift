import Foundation

// MARK: - Harassment (骚扰拦截) wire records (v2)

/// Per-enrollment-key master policy. The gateway derives the key from the
/// authenticated device; both devices of one key share this document and a
/// different key on the same physical line is completely isolated.
struct HarassmentSettingsRecord: Decodable, Equatable, Sendable {
    let callBlocking: Bool
    let smsBlocking: Bool
    /// Absent on the first harassment-capable gateways (optional -> false).
    let contactWhitelist: Bool?
    let version: Int64
    let updatedAt: Int64?

    var contactWhitelistEnabled: Bool { contactWhitelist ?? false }
}

/// One number/keyword/regex entry. `mode` is "block" | "trust"; `action` is
/// "reject" | "label" (imported historical lists default to label).
struct HarassmentEntryRecord: Decodable, Equatable, Identifiable, Sendable {
    let id: String
    let number: String
    let matchKind: String
    let mode: String
    let action: String
    let source: String?
    let listId: String?
    let enabled: Bool
    let createdAt: Int64?

    var isTrust: Bool { mode == "trust" }
    var isLabelOnly: Bool { action == "label" }
}

/// Truthful outcome of one entry write. `effective == false` means a broader
/// TRUST rule still allows the number (the API never reports a contextual
/// block as success while it is in fact not blocking).
struct HarassmentEntryAddResult: Decodable, Equatable, Sendable {
    let entry: HarassmentEntryRecord
    var replaced: Int? = nil
    var effective: Bool? = nil
    var note: String? = nil

    var isEffective: Bool { effective ?? true }
}

/// One explicit, user-triggered import batch.
struct HarassmentListRecord: Decodable, Equatable, Identifiable, Sendable {
    let id: String
    let name: String
    let defaultAction: String
    let source: String?
    let entryCount: Int
    let importedAt: Int64?
}

/// One block-history row (key-scoped).
struct HarassmentEventRecord: Decodable, Equatable, Identifiable, Sendable {
    let id: String
    let lineId: String?
    let kind: String
    let peer: String
    let action: String
    let reason: String
    let refId: String?
    let createdAt: Int64
}

/// Result of evaluating the key's CURRENT policy against synthetic input.
struct HarassmentDecisionRecord: Decodable, Equatable, Sendable {
    let allow: Bool
    let action: String
    let reason: String
    let matchedBy: String?
    let pattern: String?
}

/// Recording configuration reported by the gateway: operator auto mode,
/// THIS device's own auto preference, retention and SD storage state.
struct RecordingSettingsRecord: Decodable, Equatable, Sendable {
    let enabled: Bool
    let autoMode: String
    let deviceAutoRecord: Bool
    let retentionDays: Int
    let minimumFreeBytes: Int64?
    let maxBytes: Int64?
    let storage: RecordingStorageStatusRecord?
}

/// Verified SD storage state (fields are metadata only, no file contents).
struct RecordingStorageStatusRecord: Decodable, Equatable, Sendable {
    let mounted: Bool?
    let freeBytes: Int64?
    let error: String?
}

// MARK: - Old-gateway fallback

/// Default implementations keep OLD gateways fully usable: a client that
/// knows about harassment can still pair with a gateway that does not serve
/// these routes, and every call fails with a typed, honest "unsupported"
/// error the UI converts into local-only fallback. New behavior is additive
/// on both sides.
extension GatewayAPI {
    func harassmentSettings() async throws -> HarassmentSettingsRecord {
        throw APIError.notReady("当前网关版本不支持骚扰拦截设置。")
    }

    func updateHarassmentSettings(callBlocking: Bool, smsBlocking: Bool, contactWhitelist: Bool?, version: Int64?) async throws -> HarassmentSettingsRecord {
        throw APIError.notReady("当前网关版本不支持骚扰拦截设置。")
    }

    func harassmentEntries() async throws -> [HarassmentEntryRecord] {
        throw APIError.notReady("当前网关版本不支持骚扰拦截规则。")
    }

    func addHarassmentEntry(number: String, matchKind: String, mode: String, action: String?, replaceOpposite: Bool) async throws -> HarassmentEntryAddResult {
        throw APIError.notReady("当前网关版本不支持骚扰拦截规则。")
    }

    func deleteHarassmentEntry(id: String) async throws {
        throw APIError.notReady("当前网关版本不支持骚扰拦截规则。")
    }

    func harassmentLists() async throws -> [HarassmentListRecord] {
        throw APIError.notReady("当前网关版本不支持导入名单。")
    }

    func importHarassmentList(name: String, numbers: [String], defaultAction: String) async throws -> HarassmentListRecord {
        throw APIError.notReady("当前网关版本不支持导入名单。")
    }

    func deleteHarassmentList(id: String) async throws {
        throw APIError.notReady("当前网关版本不支持导入名单。")
    }

    func harassmentHistory() async throws -> [HarassmentEventRecord] {
        throw APIError.notReady("当前网关版本不支持拦截记录。")
    }

    func clearHarassmentHistory() async throws {
        throw APIError.notReady("当前网关版本不支持拦截记录。")
    }

    func testHarassment(kind: String, number: String, body: String) async throws -> HarassmentDecisionRecord {
        throw APIError.notReady("当前网关版本不支持规则测试。")
    }

    func recordingSettings() async throws -> RecordingSettingsRecord {
        throw APIError.notReady("当前网关版本不支持录音设置。")
    }

    @discardableResult
    func updateDeviceAutoRecord(_ enabled: Bool) async throws -> Bool {
        throw APIError.notReady("当前网关版本不支持录音设置。")
    }
}

// MARK: - Request/envelope wire types

struct V2HarassmentSettingsRequest: Encodable, Equatable {
    let callBlocking: Bool
    let smsBlocking: Bool
    let contactWhitelist: Bool?
    let version: Int64?
}

struct V2HarassmentEntryRequest: Encodable, Equatable {
    let number: String
    let matchKind: String
    let mode: String
    let action: String?
    /// Contextual actions replace the canonically-equivalent opposite-mode
    /// exact rule; bulk imports keep the configured precedence.
    let replaceOpposite: Bool
}

struct V2HarassmentImportRequest: Encodable, Equatable {
    let name: String
    let defaultAction: String
    let numbers: [String]
}

struct V2HarassmentTestRequest: Encodable, Equatable {
    let kind: String
    let number: String
    let body: String
}

/// Build-55 PATCH-presence preference: autoRecord alone never clears other
/// device preferences.
struct V2RecordingPreferencesRequest: Encodable, Equatable {
    var autoRecord: Bool?
}

/// Response of PUT /devices/{id}/preferences (the fields this client reads).
struct V2DevicePreferencesResponse: Decodable, Equatable {
    let autoRecord: Bool?
}

struct V2HarassmentSettingsEnvelope: Decodable {
    let settings: HarassmentSettingsRecord
    let unknownAction: String?
}

struct V2HarassmentEntryEnvelope: Decodable {
    let entry: HarassmentEntryRecord
    let replaced: Int?
    let effective: Bool?
    let note: String?
}

struct V2HarassmentEntriesEnvelope: Decodable {
    let entries: [HarassmentEntryRecord]
}

struct V2HarassmentListEnvelope: Decodable {
    let list: HarassmentListRecord
}

struct V2HarassmentListsEnvelope: Decodable {
    let lists: [HarassmentListRecord]
}

struct V2HarassmentHistoryEnvelope: Decodable {
    let events: [HarassmentEventRecord]
}
