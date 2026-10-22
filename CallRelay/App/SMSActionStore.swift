import Foundation

struct SMSConversationAction: Codable, Equatable, Sendable {
    enum Kind: String, Codable, Sendable { case read, delete }
    let id: String
    let kind: Kind
    let threadKey: String
    let throughCreatedAt: Int64
    let throughID: String
    var completed = false
    var attempted = false
    var failed = false
    var unreadCount = 0

    func covers(_ message: MessageRecord) -> Bool {
        message.createdAt < throughCreatedAt ||
            (message.createdAt == throughCreatedAt && message.id <= throughID)
    }
}
struct SMSActionRequest: Encodable {
    struct Item: Encodable {
        let id: String; let kind: SMSConversationAction.Kind; let threadKey: String
        let throughCreatedAt: Int64; let throughID: String
    }
    let actions: [Item]
    init(actions: [SMSConversationAction]) {
        self.actions = actions.map { Item(id: $0.id, kind: $0.kind, threadKey: $0.threadKey,
            throughCreatedAt: $0.throughCreatedAt, throughID: $0.throughID) }
    }
}
struct SMSActionResponse: Decodable {
    struct Result: Decodable {
        struct Failure: Decodable { let code: String; let message: String; let retryable: Bool }
        let id: String
        let ok: Bool
        let error: Failure?
    }
    let results: [Result]
}

/// Device-bound user intent. Separate from raw history and its validators.
@MainActor final class SMSActionStore {
    let fileURL: URL
    init?(scopeIdentifier: String?, explicitURL: URL? = nil) {
        if let explicitURL { fileURL = explicitURL; return }
        guard let scopeIdentifier, !scopeIdentifier.isEmpty else { return nil }
        let hash = SHA256Lite.hex(Data(scopeIdentifier.utf8)).prefix(20)
        fileURL = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("CallRelay/actions/actions-\(hash).json")
    }
    func load() throws -> [SMSConversationAction] {
        guard FileManager.default.fileExists(atPath: fileURL.path) else { return [] }
        return try JSONDecoder().decode([SMSConversationAction].self, from: Data(contentsOf: fileURL))
    }
    func save(_ actions: [SMSConversationAction]) throws {
        try FileManager.default.createDirectory(at: fileURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        try JSONEncoder().encode(actions).write(to: fileURL, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
        var directory = fileURL.deletingLastPathComponent()
        var values = URLResourceValues(); values.isExcludedFromBackup = true
        try? directory.setResourceValues(values)
    }
    func purge() { try? FileManager.default.removeItem(at: fileURL) }
}
