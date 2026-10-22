import XCTest
@testable import CallRelay

@MainActor final class SMSActionQueueTests: XCTestCase {
    func store() -> SMSActionStore {
        SMSActionStore(scopeIdentifier: nil, explicitURL: FileManager.default.temporaryDirectory
            .appendingPathComponent("sms-actions-test-\(UUID().uuidString)/queue.json"))!
    }
    func message(_ id: String = "line1:001", at: Int64 = 1000, key: String = "line1:+8613800000001") -> MessageRecord {
        MessageRecord(id: id, gatewayID: "test", lineID: "line1", threadKey: key, direction: .inbound,
            peer: "+8613800000001", body: "张三", encoding: "ucs2", status: .sent, createdAt: at)
    }
    func inbox(_ api: FakeGatewayAPI, _ store: SMSActionStore) -> MessageInbox {
        let result = MessageInbox(api: api, tombstoneScope: UUID().uuidString,
            tombstoneDefaults: UserDefaults(suiteName: UUID().uuidString)!, actionStore: store)
        result.isEventStreamHealthy = { true }
        return result
    }
    func waitFor(_ condition: @escaping () -> Bool) async {
        for _ in 0..<200 { if condition() { return }; try? await Task.sleep(nanoseconds: 10_000_000) }
        XCTFail("operation did not settle")
    }
    func testOpeningCachedConversationPersistsReadAndDoesNotSendBeforeStart() async throws {
        let api = FakeGatewayAPI(), journal = store(), msg = message()
        let model = inbox(api, journal)
        model.apply(eventMessage: msg)
        model.openThread(msg.threadKey)
        XCTAssertEqual(try journal.load().count, 1)
        XCTAssertEqual(model.displayThreads.first?.unreadCount, 0)
        XCTAssertTrue(api.smsActionBatches.isEmpty)
        model.closeThread(); model.invalidate(); journal.purge()
    }
    func testBulkQueuesImmediatelyAndRestoresAfterRelaunch() async throws {
        let api = FakeGatewayAPI(), journal = store(), msg = message()
        let first = inbox(api, journal); first.apply(eventMessage: msg)
        let result = await first.deleteThreads([msg.threadKey])
        XCTAssertEqual(result.succeeded, [msg.threadKey])
        XCTAssertTrue(first.displayThreads.isEmpty)
        first.invalidate()
        let restored = inbox(api, journal); restored.apply(eventMessage: msg)
        XCTAssertTrue(restored.displayThreads.isEmpty)
        XCTAssertEqual(try journal.load().first?.throughID, msg.id)
        restored.invalidate(); journal.purge()
    }
    func testBackgroundAndClosedConversationsNeverMarkRead() throws {
        let api = FakeGatewayAPI(), journal = store(), model = inbox(api, journal), msg = message()
        model.conversationIsForeground = { false }
        model.openThread(msg.threadKey); model.apply(eventMessage: msg)
        XCTAssertTrue(try journal.load().isEmpty)
        model.closeThread(); model.conversationIsForeground = { true }
        model.apply(eventMessage: message("line1:002", at: 1100))
        XCTAssertTrue(try journal.load().isEmpty)
        model.invalidate(); journal.purge()
    }
    func testDeleteBoundaryKeepsNewMessageAndSameMillisecondLargerID() async throws {
        let api = FakeGatewayAPI(), journal = store(), model = inbox(api, journal), msg = message()
        model.apply(eventMessage: msg)
        _ = await model.deleteThreads([msg.threadKey])
        let newer = message("line1:002")
        model.apply(eventMessage: newer)
        XCTAssertEqual(model.displayThreads.first?.lastMessage.id, newer.id)
        XCTAssertEqual(model.rows(for: msg.threadKey).count, 1)
        model.invalidate(); journal.purge()
    }
    func testBatchChunkLimitAndStableIDsArePersisted() async throws {
        let api = FakeGatewayAPI(), journal = store(), model = inbox(api, journal)
        api.smsActionsSupported = true
        var keys: [String] = []
        for i in 0..<205 {
            let key = "line1:+861380000\(String(format: "%04d", i))"
            keys.append(key); model.apply(eventMessage: message("line1:\(i)", key: key))
        }
        _ = await model.markThreadsRead(keys)
        let ids = Set(try journal.load().map(\.id))
        model.start(pollInterval: 1000)
        await waitFor { api.smsActionBatches.count == 3 }
        XCTAssertEqual(api.smsActionBatches.map(\.count), [100,100,5])
        XCTAssertEqual(Set(api.smsActionBatches.flatMap { $0 }.map(\.id)), ids)
        model.invalidate(); journal.purge()
    }
    func testPermanentFailureRestoresBadgeAndOffersRetry() async throws {
        let api = FakeGatewayAPI(), journal = store(), model = inbox(api, journal), msg = message()
        api.smsActionsSupported = true
        api.smsActionResult = { .init(results: $0.map { .init(id: $0.id, ok: false, error: .init(code: "denied", message: "denied", retryable: false)) }) }
        api.threads = [MessageThread(key: msg.threadKey, peer: msg.peer, unreadCount: 1, lastMessage: msg)]
        model.apply(eventMessage: msg); _ = await model.markThreadsRead([msg.threadKey])
        model.start(pollInterval: 1000)
        await waitFor { model.actionSyncError != nil }
        XCTAssertEqual(model.displayThreads.first?.unreadCount, 1)
        XCTAssertTrue(try journal.load().first?.failed == true)
        api.smsActionResult = nil; model.retrySMSActions()
        await waitFor { (try? journal.load().allSatisfy { $0.completed }) == true }
        model.invalidate(); journal.purge()
    }
    func testPurgeRemovesJournal() async throws {
        let api = FakeGatewayAPI(), journal = store(), model = inbox(api, journal), msg = message()
        model.apply(eventMessage: msg); _ = await model.markThreadsRead([msg.threadKey])
        model.purgeSMSActions()
        XCTAssertFalse(FileManager.default.fileExists(atPath: journal.fileURL.path))
        model.invalidate()
    }
    func testCorruptJournalCannotBeOverwrittenByNewIntent() async throws {
        let api = FakeGatewayAPI(), journal = store()
        try FileManager.default.createDirectory(at: journal.fileURL.deletingLastPathComponent(), withIntermediateDirectories: true)
        let corrupt = Data("not json".utf8); try corrupt.write(to: journal.fileURL)
        let model = inbox(api, journal), msg = message(); model.apply(eventMessage: msg)
        let result = await model.markThreadsRead([msg.threadKey])
        XCTAssertEqual(result.failed, [msg.threadKey])
        XCTAssertEqual(try Data(contentsOf: journal.fileURL), corrupt)
        model.invalidate(); journal.purge()
    }
    func testWriteFailureDoesNotOptimisticallyHideOrClear() async {
        let api = FakeGatewayAPI()
        let journal = SMSActionStore(scopeIdentifier: nil, explicitURL: URL(fileURLWithPath: "/dev/null/actions.json"))!
        let model = inbox(api, journal), msg = message(); model.apply(eventMessage: msg)
        let result = await model.deleteThreads([msg.threadKey])
        XCTAssertEqual(result.failed, [msg.threadKey])
        XCTAssertEqual(model.displayThreads.count, 1)
        XCTAssertNotNil(model.actionSyncError)
        model.invalidate()
    }
    func testReadACKSurvivesStaleServerBodyAndRestart() async throws {
        let api = FakeGatewayAPI(), journal = store(), msg = message(), model = inbox(api, journal)
        api.smsActionsSupported = true
        let row = MessageThread(key: msg.threadKey, peer: msg.peer, unreadCount: 1, lastMessage: msg)
        api.threads = [row]; model.apply(eventMessage: msg)
        _ = await model.markThreadsRead([msg.threadKey]); model.start(pollInterval: 1000)
        await waitFor { (try? journal.load().first?.completed) == true }
        _ = await model.refreshThreads()
        XCTAssertEqual(model.displayThreads.first?.unreadCount, 0)
        model.invalidate()
        let restarted = inbox(api, journal)
        _ = await restarted.refreshThreads()
        XCTAssertEqual(restarted.displayThreads.first?.unreadCount, 0)
        restarted.apply(eventMessage: message("line1:002", at: 1100))
        XCTAssertEqual(restarted.displayThreads.first?.unreadCount, 1)
        restarted.invalidate(); journal.purge()
    }
    func testTransientFailureRetainsIntentAndWakeRetriesSameID() async throws {
        let api = FakeGatewayAPI(), journal = store(), model = inbox(api, journal), msg = message()
        api.smsActionsSupported = true
        api.smsActionResult = { _ in throw URLError(.networkConnectionLost) }
        api.threads = [MessageThread(key: msg.threadKey, peer: msg.peer, unreadCount: 1, lastMessage: msg)]
        model.apply(eventMessage: msg); _ = await model.markThreadsRead([msg.threadKey])
        model.start(pollInterval: 1000)
        await waitFor { api.smsActionBatches.count == 1 }
        XCTAssertEqual(model.displayThreads.first?.unreadCount, 0)
        api.smsActionResult = nil; model.wakeActionSync()
        await waitFor { api.smsActionBatches.count >= 2 }
        XCTAssertEqual(api.smsActionBatches[0].map(\.id), api.smsActionBatches[1].map(\.id))
        model.invalidate(); journal.purge()
    }

    func testMalformedResponseBacksOffWithoutDroppingIntent() async throws {
        let api = FakeGatewayAPI(), journal = store(), model = inbox(api, journal), msg = message()
        api.smsActionsSupported = true; api.smsActionResult = { _ in .init(results: []) }
        model.apply(eventMessage: msg); _ = await model.markThreadsRead([msg.threadKey])
        model.start(pollInterval: 1000)
        await waitFor { api.smsActionBatches.count == 1 }
        try await Task.sleep(nanoseconds: 100_000_000)
        XCTAssertEqual(api.smsActionBatches.count, 1)
        XCTAssertFalse(try XCTUnwrap(journal.load().first).completed)
        model.invalidate(); journal.purge()
    }
    func testExpiredOldWorkerCannotClearReplacementWorkerOrSettleQueue() async throws {
        let api = FakeGatewayAPI(), journal = store(), model = inbox(api, journal), msg = message()
        api.smsActionsSupported = true
        var replies: [CheckedContinuation<SMSActionResponse, Error>] = []
        api.smsActionAsyncResult = { _ in try await withCheckedThrowingContinuation { replies.append($0) } }
        model.apply(eventMessage: msg); _ = await model.markThreadsRead([msg.threadKey]); model.start(pollInterval: 1000)
        await waitFor { replies.count == 1 }
        model.expireActionSyncForTest(); model.wakeActionSync()
        await waitFor { replies.count == 2 }
        let action = try XCTUnwrap(journal.load().first)
        replies[0].resume(returning: .init(results: [.init(id: action.id, ok: true, error: nil)]))
        try await Task.sleep(nanoseconds: 30_000_000)
        XCTAssertTrue(model.actionSyncRunningForTest)
        XCTAssertFalse(try XCTUnwrap(journal.load().first).completed)
        replies[1].resume(returning: .init(results: [.init(id: action.id, ok: true, error: nil)]))
        await waitFor { !model.actionSyncRunningForTest }
        XCTAssertTrue(try XCTUnwrap(journal.load().first).completed)
        model.invalidate(); journal.purge()
    }
    func testDifferentCanonicalScopesHaveDifferentJournals() {
        let originA = try! GatewayOrigin.validate("https://example.invalid/a", allowLoopbackHTTP: false, apiVersion: "v2").get()
        let originB = try! GatewayOrigin.validate("https://example.invalid/b", allowLoopbackHTTP: false, apiVersion: "v2").get()
        let a = SMSActionStore(scopeIdentifier: HistoryScope.identifier(origin: originA, gatewayID: "gw", deviceID: "key-device-A"))!
        let b = SMSActionStore(scopeIdentifier: HistoryScope.identifier(origin: originA, gatewayID: "gw", deviceID: "key-device-B"))!
        let c = SMSActionStore(scopeIdentifier: HistoryScope.identifier(origin: originB, gatewayID: "gw", deviceID: "key-device-A"))!
        XCTAssertNotEqual(a.fileURL, b.fileURL); XCTAssertNotEqual(a.fileURL, c.fileURL)
    }
    func testLegacyDeleteNeverCallsUnboundedDelete() async throws {
        let api = FakeGatewayAPI(), journal = store(), model = inbox(api, journal), msg = message()
        api.threads = [MessageThread(key: msg.threadKey, peer: msg.peer, unreadCount: 1, lastMessage: msg)]
        model.apply(eventMessage: msg); _ = await model.deleteThreads([msg.threadKey]); model.start(pollInterval: 1000)
        await waitFor { model.actionSyncError != nil }
        XCTAssertEqual(model.displayThreads.count, 1)
        XCTAssertTrue(try XCTUnwrap(journal.load().first).failed)
        model.invalidate(); journal.purge()
    }

    func testCachedUnreadConversationStillMarksReadWhenPageReturns304() async throws {
        let api = FakeGatewayAPI(), journal = store(), msg = message()
        let history = GatewayHistoryStore(scopeIdentifier: nil, explicitURL: journal.fileURL.deletingLastPathComponent().appendingPathComponent("history.json"))!
        api.threadPages[msg.threadKey] = [msg]
        api.validatorReader = { history.validator(for: $0) }
        let outcome = try await api.listThreadMessagesConditional(threadKey: msg.threadKey, beforeCreatedAt: nil, beforeID: nil, limit: 50)
        guard case .changed(_, let validator) = outcome, let validator else { return XCTFail("expected validator") }
        // Match the inbox's page limit below (the query is part of the validator key).
        history.recordMessages([msg], for: msg.threadKey, validatorUpdates: [validator.key: validator.etag], savedAt: Date())
        history.flush()
        let model = MessageInbox(api: api, historyStore: history, actionStore: journal)
        model.openThread(msg.threadKey)
        await waitFor { api.pageFetch304Count > 0 }
        XCTAssertEqual(try journal.load().first?.kind, .read)
        XCTAssertTrue(model.isPendingRead(msg.threadKey))
        model.closeThread(); model.invalidate(); journal.purge(); history.purge()
    }
    func testPageCallbackAfterConversationClosedDoesNotEnqueueRead() async throws {
        let api = FakeGatewayAPI(), journal = store(), model = inbox(api, journal), msg = message()
        api.threadPages[msg.threadKey] = [msg]
        model.openThread(msg.threadKey); model.closeThread()
        await waitFor { model.rows(for: msg.threadKey).count == 1 }
        XCTAssertTrue(try journal.load().isEmpty)
        model.invalidate(); journal.purge()
    }

    func testExactWireFieldsAndMixedResponseDecode() throws {
        let action = SMSConversationAction(id: "operation", kind: .read, threadKey: "line1:peer", throughCreatedAt: 10, throughID: "line1:1", attempted: true, unreadCount: 9)
        let bytes = try JSONEncoder().encode(SMSActionRequest(actions: [action]))
        let json = try XCTUnwrap(JSONSerialization.jsonObject(with: bytes) as? [String: [[String: Any]]])
        XCTAssertEqual(Set(try XCTUnwrap(json["actions"]?.first).keys), ["id","kind","threadKey","throughCreatedAt","throughID"])
        let data = Data(#"{"results":[{"id":"1","ok":true},{"id":"2","ok":false,"error":{"code":"busy","message":"retry","retryable":true}},{"id":"3","ok":false,"error":{"code":"denied","message":"denied","retryable":false}}]}"#.utf8)
        let response = try JSONDecoder().decode(SMSActionResponse.self, from: data)
        XCTAssertEqual(response.results.count, 3)
        XCTAssertEqual(response.results[1].error?.retryable, true)
        XCTAssertEqual(response.results[2].error?.retryable, false)
    }
}
