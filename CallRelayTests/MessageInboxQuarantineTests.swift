import XCTest
@testable import CallRelay

/// Build-55 gateway-managed per-key SMS quarantine: folders come from the
/// server, restore/delete are key-scoped API operations, and an old gateway
/// falls back to local classification without breaking.
@MainActor
final class MessageInboxQuarantineTests: XCTestCase {
    private func message(_ id: String, peer: String, body: String) -> MessageRecord {
        MessageRecord(id: id, gatewayID: "gw", lineID: nil, threadKey: peer,
                      direction: .inbound, peer: peer, body: body, encoding: "ucs2",
                      status: .sent, createdAt: Date().unixMilliseconds)
    }

    private func thread(key: String, peer: String, body: String, unread: Int = 1, folder: String?) -> MessageThread {
        MessageThread(key: key, peer: peer, unreadCount: unread,
                      lastMessage: message("m-" + key, peer: peer, body: body), folder: folder)
    }

    private func makeInbox(_ fake: FakeGatewayAPI) -> MessageInbox {
        let filter = SpamFilterStore(storeURL: FileManager.default.temporaryDirectory
            .appendingPathComponent("quarantine-\(UUID().uuidString).json"))
        let inbox = MessageInbox(api: fake, filter: filter)
        inbox.lineReady = { true }
        inbox.gatewayQuarantineAvailable = { true }
        return inbox
    }

    func testGatewayFoldersDriveJunkAndNormalLists() async {
        let fake = FakeGatewayAPI()
        fake.quarantineSupported = true
        let normal = thread(key: "line1:peer-a", peer: "+8613600000021", body: "普通消息", folder: "normal")
        let junk = thread(key: "line1:peer-b", peer: "+8613600000022", body: "推广垃圾", folder: "junk")
        fake.quarantineFolders = ["normal": [normal], "junk": [junk]]

        let inbox = makeInbox(fake)
        await inbox.refreshThreads()

        XCTAssertTrue(inbox.isGatewayQuarantineActive)
        XCTAssertEqual(inbox.junkThreads.map(\.key), [junk.key])
        XCTAssertTrue(inbox.displayThreads.contains { $0.key == normal.key })
        XCTAssertFalse(inbox.displayThreads.contains { $0.key == junk.key })
        XCTAssertTrue(inbox.junkReasons.isEmpty, "gateway mode must not run the local classifier")
    }

    func testRestoreUsesGatewayAndMovesThreadBack() async {
        let fake = FakeGatewayAPI()
        fake.quarantineSupported = true
        let junk = thread(key: "line1:peer-b", peer: "+8613600000022", body: "推广垃圾", folder: "junk")
        fake.quarantineFolders = ["normal": [], "junk": [junk]]

        let inbox = makeInbox(fake)
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.junkThreads.count, 1)

        inbox.restoreJunk(threadKey: junk.key, peer: junk.peer)
        // The restore call is asynchronous; wait for the refresh to converge.
        let deadline = Date().addingTimeInterval(3)
        while Date() < deadline {
            if fake.restoredThreadKeys.contains(junk.key),
               inbox.junkThreads.isEmpty,
               inbox.displayThreads.contains(where: { $0.key == junk.key }) {
                return
            }
            try? await Task.sleep(nanoseconds: 20_000_000)
        }
        XCTFail("gateway restore did not converge: restored=\(fake.restoredThreadKeys) junk=\(inbox.junkThreads)")
    }

    func testDismissUsesKeyScopedDelete() async {
        let fake = FakeGatewayAPI()
        fake.quarantineSupported = true
        let junk = thread(key: "line1:peer-c", peer: "+8613600000023", body: "刷单", folder: "junk")
        fake.quarantineFolders = ["normal": [], "junk": [junk]]

        let inbox = makeInbox(fake)
        await inbox.refreshThreads()
        inbox.dismissJunk(threadKey: junk.key)

        let deadline = Date().addingTimeInterval(3)
        while Date() < deadline {
            if fake.deletedThreadKeys.contains(junk.key) && inbox.junkThreads.isEmpty {
                return
            }
            try? await Task.sleep(nanoseconds: 20_000_000)
        }
        XCTFail("key-scoped delete did not converge: deleted=\(fake.deletedThreadKeys)")
    }

    func testOldGatewayFallsBackToLocalClassification() async {
        let fake = FakeGatewayAPI()
        // quarantineSupported stays false: folder listing throws notReady.
        let loan = thread(key: "line1:peer-d", peer: "+8613600000024",
                          body: "无抵押贷款，低息贷款。", folder: nil)
        fake.threads = [loan]
        let filter = SpamFilterStore(storeURL: FileManager.default.temporaryDirectory
            .appendingPathComponent("fallback-\(UUID().uuidString).json"))
        filter.enable(preset: .loanAndInvestment)
        filter.setSMSBlocking(true, notifyGateway: false)
        let inbox = MessageInbox(api: fake, filter: filter)
        inbox.lineReady = { true }
        inbox.gatewayQuarantineAvailable = { true }

        await inbox.refreshThreads()
        XCTAssertFalse(inbox.isGatewayQuarantineActive)
        XCTAssertEqual(inbox.junkThreads.map(\.key), [loan.key],
                       "old gateway keeps the local classifier fallback")
    }

    func testLocalContactTrustRespectsExplicitBlock() {
        let filter = SpamFilterStore(storeURL: FileManager.default.temporaryDirectory
            .appendingPathComponent("contact-\(UUID().uuidString).json"))
        filter.addRule(kind: .senderExact, value: "13800000030", label: "拦截")
        filter.setSMSBlocking(true, notifyGateway: false)
        // A contact that the owner explicitly blocked must not be trusted.
        XCTAssertTrue(filter.policy().hasSenderBlock(peer: "13800000030"))
        XCTAssertFalse(filter.policy().hasSenderBlock(peer: "13800000031"))
    }
}
