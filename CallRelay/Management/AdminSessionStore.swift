import Foundation

/// Administrator console session held for one gateway on THIS device.
///
/// The password is never stored: only the gateway-issued session token and
/// its CSRF companion, in a per-device Keychain item that is explicitly NOT
/// synchronizable (an administrator session must never follow the iCloud
/// Keychain to another device).
struct AdminSession: Codable, Equatable {
    let gatewayId: String
    let baseURL: String
    let username: String
    let sessionToken: String
    let csrf: String
    let expiresAt: Date

    var isExpired: Bool { Date() >= expiresAt }

    /// A session is only usable for the gateway it was issued by, at the
    /// exact base URL it was issued for.
    func matches(gatewayId: String, baseURL: String) -> Bool {
        self.gatewayId == gatewayId && self.baseURL == baseURL
    }
}

/// Per-device Keychain storage for the administrator session. Mirrors the
/// app's existing device-only Keychain discipline (TokenStore): a generic
/// password item with `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly` and
/// an explicit `kSecAttrSynchronizable = false`, so an administrator session
/// never leaves this device through the iCloud Keychain.
struct AdminSessionStore {
    static let shared = AdminSessionStore()

    private let service = "com.jiangnangenius.callrelay.admin"
    private let account = "gateway-admin-session"
    private let keychain: KeychainWrapping?

    /// `keychain == nil` uses the app's SystemKeychain directly (the same
    /// device-only item). Tests inject an in-memory wrapper: the
    /// administrator session must be testable without touching a real
    /// Keychain.
    init(keychain: KeychainWrapping? = nil) {
        self.keychain = keychain
    }

    func load() -> AdminSession? {
        if let keychain {
            guard let data = keychain.readData(service: service, account: account, synchronizable: false) else {
                return nil
            }
            return try? JSONDecoder().decode(AdminSession.self, from: data)
        }
        return Self.loadSystem(service: service, account: account)
    }

    @discardableResult
    func save(_ session: AdminSession) -> Bool {
        guard let data = try? JSONEncoder().encode(session) else { return false }
        if let keychain {
            do {
                try keychain.saveData(data, service: service, account: account,
                                      accessibility: .afterFirstUnlockThisDeviceOnly,
                                      synchronizable: false)
                return true
            } catch {
                return false
            }
        }
        return Self.saveSystem(data, service: service, account: account)
    }

    func clear() {
        if let keychain {
            keychain.delete(service: service, account: account, synchronizable: false)
            return
        }
        Self.clearSystem(service: service, account: account)
    }

    // MARK: System Keychain (explicit non-synchronizable item)

    private static func baseQuery(service: String, account: String) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecAttrSynchronizable as String: kCFBooleanFalse as Any,
        ]
    }

    private static func loadSystem(service: String, account: String) -> AdminSession? {
        var query = baseQuery(service: service, account: account)
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        guard status == errSecSuccess, let data = item as? Data else { return nil }
        return try? JSONDecoder().decode(AdminSession.self, from: data)
    }

    private static func saveSystem(_ data: Data, service: String, account: String) -> Bool {
        // Upsert: an existing item is updated in place so the accessibility
        // class of the original item is preserved by the update path.
        var query = baseQuery(service: service, account: account)
        let attributes: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        let updateStatus = SecItemUpdate(query as CFDictionary, attributes as CFDictionary)
        if updateStatus == errSecSuccess { return true }
        query.merge(attributes) { _, new in new }
        return SecItemAdd(query as CFDictionary, nil) == errSecSuccess
    }

    private static func clearSystem(service: String, account: String) {
        SecItemDelete(baseQuery(service: service, account: account) as CFDictionary)
    }
}

/// Process-wide administrator session storage used by the console.
enum AdminSessionStorage {
    static var store = AdminSessionStore.shared
}
