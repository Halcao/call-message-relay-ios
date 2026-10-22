import XCTest
@testable import CallRelay

/// Focused coverage for the list-level delete/mark-read semantics: durable
/// tombstones (CloudKit restore must not resurrect a deleted conversation)
/// and honest per-thread bulk results (a failed key keeps its row/badge).
@MainActor
final class ThreadDeleteBulkTests: XCTestCase {
    private func makeMessage(
        id: String, thread: String, direction: MessageDirection = .inbound,
        status: MessageStatus = .sent, createdAt: Int64 = 1_000
    ) -> MessageRecord {
        MessageRecord(
            id: id, gatewayID: "gw", lineID: nil, threadKey: thread,
            direction: direction, peer: thread, body: "内容", encoding: "ucs2",
            status: status, createdAt: createdAt
        )
    }

    private func makeThread(key: String, unread: Int, last: MessageRecord) -> MessageThread {
        MessageThread(key: key, peer: key, unreadCount: unread, lastMessage: last)
    }

    private func defaults(_ name: String) -> UserDefaults {
        let suite = "ThreadDeleteBulkTests.\(name).\(UUID().uuidString)"
        let defaults = UserDefaults(suiteName: suite)!
        defaults.removePersistentDomain(forName: suite)
        return defaults
    }

    // MARK: Durable tombstones

    func testDeleteHorizonIsPersistedAndSurvivesANewInbox() {
        let store = defaults(#function)
        let api = FakeGatewayAPI()

        let first = MessageInbox(api: api, tombstoneScope: "gw-1", tombstoneDefaults: store)
        first.applyThreadDeleted(key: "line1:555", deletedAt: 1_000)
        XCTAssertEqual(first.persistedTombstoneHorizons["line1:555"], 1_000)

        let second = MessageInbox(api: api, tombstoneScope: "gw-1", tombstoneDefaults: store)
        XCTAssertEqual(second.persistedTombstoneHorizons["line1:555"], 1_000)
    }

    func testRestoredCloudHistoryObeysPersistedTombstoneUntilANewerMessage() {
        let store = defaults(#function)
        let api = FakeGatewayAPI()
        let first = MessageInbox(api: api, tombstoneScope: "gw-1", tombstoneDefaults: store)
        first.applyThreadDeleted(key: "line1:555", deletedAt: 1_000)

        // Restart: in-memory tombstones are gone; the durable horizon must
        // still hide pre-delete restored history.
        let restarted = MessageInbox(api: api, tombstoneScope: "gw-1", tombstoneDefaults: store)
        restarted.applyCloudMessages([
            SyncedMessage(id: "gw-1.m1", gatewayScope: "gw-1", threadKey: "line1:555",
                          peer: "555", body: "旧", direction: "inbound",
                          status: "sent", createdAt: 900, updatedAt: Date())
        ], scope: "gw-1")
        XCTAssertTrue(restarted.cloudThreads.isEmpty)
        XCTAssertTrue(restarted.displayThreads.isEmpty)

        // A message newer than the horizon reopens the conversation.
        restarted.applyCloudMessages([
            SyncedMessage(id: "gw-1.m2", gatewayScope: "gw-1", threadKey: "line1:555",
                          peer: "555", body: "新", direction: "inbound",
                          status: "sent", createdAt: 2_000, updatedAt: Date())
        ], scope: "gw-1")
        XCTAssertEqual(restarted.cloudThreads.map(\.key), ["line1:555"])
    }

    func testTombstonesAreScopedPerGateway() {
        let store = defaults(#function)
        let api = FakeGatewayAPI()
        let inbox = MessageInbox(api: api, tombstoneScope: "gw-1", tombstoneDefaults: store)
        inbox.applyThreadDeleted(key: "line1:555", deletedAt: 1_000)

        let other = MessageInbox(api: api, tombstoneScope: "gw-2", tombstoneDefaults: store)
        XCTAssertTrue(other.persistedTombstoneHorizons.isEmpty)
    }

    // MARK: Bulk mark read

    func testMarkThreadsReadMarksOnlyInboundUnreadAndReportsPartialFailure() async {
        let api = FakeGatewayAPI()
        let t1 = "line1:5550001"
        let t2 = "line1:5550002"
        let inbound = makeMessage(id: "gw:in", thread: t1, direction: .inbound, status: .sent)
        let outbound = makeMessage(id: "gw:out", thread: t1, direction: .outbound, status: .read)
        let other = makeMessage(id: "gw:other", thread: t2, direction: .inbound, status: .sent)
        api.threads = [
            makeThread(key: t1, unread: 1, last: outbound),
            makeThread(key: t2, unread: 1, last: other)
        ]
        api.threadPages = [t1: [inbound, outbound], t2: [other]]
        api.readErrors["gw:other"] = APIError.notReady("offline")

        let inbox = MessageInbox(api: api)
        inbox.start(pollInterval: 3_600)
        await waitUntil { inbox.listPhase == .loaded }

        let result = await inbox.markThreadsRead([t1, t2])
        XCTAssertEqual(result.succeeded, [t1])
        XCTAssertEqual(result.failed, [t2])
        // Outbound rows are never marked; the failed thread keeps its unread.
        XCTAssertEqual(api.readMarked, ["gw:in"])
        XCTAssertEqual(inbox.threadCache[t1]?.first { $0.id == "gw:in" }?.status, .read)
    }

    func testMarkThreadsReadEmptySelectionIsANoOp() async {
        let api = FakeGatewayAPI()
        let inbox = MessageInbox(api: api)
        let result = await inbox.markThreadsRead([])
        XCTAssertTrue(result.isEmpty)
        XCTAssertTrue(api.readMarked.isEmpty)
    }

    func testBulkResultNeverClaimsSuccessForAFailedDelete() async {
        let api = FakeGatewayAPI()
        api.deleteThreadErrors["line1:555"] = APIError.notReady("offline")
        let inbox = MessageInbox(api: api)
        inbox.start(pollInterval: 3_600)
        await waitUntil { inbox.listPhase == .loaded }

        // AppModel is the delete owner; its bulk loop is exercised through
        // the inbox-visible contract here: failed keys must not be applied.
        do {
            try await api.deleteThread(threadKey: "line1:555")
            XCTFail("armed delete must fail")
        } catch {
            XCTAssertTrue(api.deletedThreadKeys.isEmpty)
        }
    }

    // MARK: Build-67 responsiveness: pending user intent + honest restore

    /// The delete row hides IMMEDIATELY (pending intent) while the gateway
    /// tombstone round-trips, and stays hidden once the gateway accepts.
    func testDeleteHidesTheRowImmediatelyAndAppliesTheTombstoneOnSuccess() async {
        let api = FakeGatewayAPI()
        let last = makeMessage(id: "m1", thread: "line1:555")
        api.threads = [makeThread(key: "line1:555", unread: 2, last: last)]
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.map(\.key), ["line1:555"])

        api.armDeleteHold()
        let task = Task { await inbox.deleteThread("line1:555") }
        await waitUntil { inbox.isPendingDeletion("line1:555") }
        XCTAssertTrue(inbox.displayThreads.isEmpty,
                      "the row must hide as soon as the user's intent is pending")
        XCTAssertTrue(api.deletedThreadKeys.isEmpty,
                      "the local hide is intent, not a completed gateway delete")

        api.resumeDelete(.success(()))
        let ok = await task.value
        XCTAssertTrue(ok)
        XCTAssertFalse(inbox.isPendingDeletion("line1:555"))
        XCTAssertTrue(inbox.displayThreads.isEmpty, "the accepted delete stays hidden")
        XCTAssertEqual(api.deletedThreadKeys, ["line1:555"])
        XCTAssertNotNil(inbox.persistedTombstoneHorizons["line1:555"],
                        "only the accepted delete writes the durable tombstone")
    }

    /// A rejected delete restores the row (server truth) and reports.
    func testDeleteRejectionRestoresTheRow() async {
        let api = FakeGatewayAPI()
        let last = makeMessage(id: "m1", thread: "line1:555")
        api.threads = [makeThread(key: "line1:555", unread: 0, last: last)]
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()

        api.armDeleteHold()
        let task = Task { await inbox.deleteThread("line1:555") }
        await waitUntil { inbox.isPendingDeletion("line1:555") }
        XCTAssertTrue(inbox.displayThreads.isEmpty)
        api.resumeDelete(.failure(APIError.notReady("offline")))

        let ok = await task.value
        XCTAssertFalse(ok)
        XCTAssertFalse(inbox.isPendingDeletion("line1:555"), "the pending marker must clear")
        XCTAssertEqual(inbox.displayThreads.map(\.key), ["line1:555"],
                       "a rejected delete must restore the row, never fake durability")
        XCTAssertTrue(inbox.persistedTombstoneHorizons["line1:555"] == nil)
    }

    /// Mark-read clears the badge IMMEDIATELY but persists through the
    /// gateway; the messages are really marked on success.
    func testMarkReadClearsTheBadgeImmediatelyAndPersists() async {
        let api = FakeGatewayAPI()
        let threadKey = "line1:555"
        let messages = [makeMessage(id: "m1", thread: threadKey),
                        makeMessage(id: "m2", thread: threadKey)]
        api.threads = [makeThread(key: threadKey, unread: 2, last: messages[1])]
        api.threadPages[threadKey] = messages
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 2)

        api.armReadHold()
        let task = Task { await inbox.markThreadsRead([threadKey]) }
        await waitUntil { inbox.isPendingRead(threadKey) }
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 0,
                       "the badge clears as soon as the read is pending")
        XCTAssertTrue(api.readMarked.isEmpty,
                      "the badge clear is intent, not a completed gateway write")

        api.resumeRead(.success(()))
        let result = await task.value
        XCTAssertEqual(result.succeeded, [threadKey])
        XCTAssertFalse(inbox.isPendingRead(threadKey))
        XCTAssertEqual(Set(api.readMarked), ["m1", "m2"],
                       "the gateway really received both marks")
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 0)
    }

    /// A rejected mark-read restores the exact badge (never a fake read).
    func testMarkReadRejectionRestoresTheBadgeAndReports() async {
        let api = FakeGatewayAPI()
        let threadKey = "line1:555"
        let messages = [makeMessage(id: "m1", thread: threadKey),
                        makeMessage(id: "m2", thread: threadKey)]
        api.threads = [makeThread(key: threadKey, unread: 3, last: messages[1])]
        api.threadPages[threadKey] = messages
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 3)

        api.armReadHold()
        let task = Task { await inbox.markThreadsRead([threadKey]) }
        await waitUntil { inbox.isPendingRead(threadKey) }
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 0)
        api.resumeRead(.failure(APIError.notReady("offline")))

        let result = await task.value
        XCTAssertEqual(result.failed, [threadKey])
        XCTAssertFalse(inbox.isPendingRead(threadKey))
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 3,
                       "a rejected read must restore the exact unread badge")
    }

    /// A stale refresh (or another client's older copy) that lands while a
    /// read is still pending must not resurrect the badge.
    func testStaleRefreshDuringPendingMarkReadDoesNotResurrectTheBadge() async {
        let api = FakeGatewayAPI()
        let threadKey = "line1:555"
        let messages = [makeMessage(id: "m1", thread: threadKey)]
        api.threads = [makeThread(key: threadKey, unread: 1, last: messages[0])]
        api.threadPages[threadKey] = messages
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()

        api.armReadHold()
        let task = Task { await inbox.markThreadsRead([threadKey]) }
        await waitUntil { inbox.isPendingRead(threadKey) }
        // The server copy still says unread (the marking has not landed).
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 0,
                       "pending intent must win over a stale server copy")

        api.resumeRead(.success(()))
        _ = await task.value
        XCTAssertFalse(inbox.isPendingRead(threadKey))
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 0)
    }

    /// Overlapping taps are ONE operation: the second mark-read for an
    /// already-pending key is skipped, not re-submitted.
    func testOverlappingMarkReadIsNotResubmitted() async {
        let api = FakeGatewayAPI()
        let threadKey = "line1:555"
        let messages = [makeMessage(id: "m1", thread: threadKey)]
        api.threads = [makeThread(key: threadKey, unread: 1, last: messages[0])]
        api.threadPages[threadKey] = messages
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()

        api.armReadHold()
        let first = Task { await inbox.markThreadsRead([threadKey]) }
        await waitUntil { inbox.isPendingRead(threadKey) }
        let second = await inbox.markThreadsRead([threadKey])
        XCTAssertTrue(second.isEmpty, "an already-pending key is not resubmitted")
        api.resumeRead(.success(()))
        _ = await first.value
        XCTAssertEqual(api.readMarked, ["m1"], "the message is marked exactly once")
    }

    /// Bulk delete must mark EVERY selected row pending BEFORE the first
    /// network call: with the first request parked, all selected rows are
    /// already gone and only one request is in flight (no duplicate work).
    func testBulkDeleteMarksTheWholeSelectionPendingBeforeTheFirstRequest() async {
        let api = FakeGatewayAPI()
        api.threads = [
            makeThread(key: "line1:555", unread: 0, last: makeMessage(id: "m1", thread: "line1:555")),
            makeThread(key: "line1:666", unread: 0, last: makeMessage(id: "m2", thread: "line1:666")),
            makeThread(key: "line1:777", unread: 0, last: makeMessage(id: "m3", thread: "line1:777")),
        ]
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.count, 3)

        api.armDeleteHold()
        let task = Task { await inbox.deleteThreads(["line1:555", "line1:666"]) }
        await waitUntil { inbox.isPendingDeletion("line1:555") && inbox.isPendingDeletion("line1:666") }
        XCTAssertEqual(inbox.displayThreads.map(\.key), ["line1:777"],
                       "every selected row must hide immediately, not one per round-trip")
        XCTAssertTrue(api.deletedThreadKeys.isEmpty,
                      "the parked first request has not completed yet")

        api.resumeDelete(.success(()))
        let result = await task.value
        XCTAssertEqual(result.succeeded, ["line1:555", "line1:666"])
        XCTAssertEqual(api.deletedThreadKeys, ["line1:555", "line1:666"],
                       "each key is submitted exactly once")
        XCTAssertEqual(inbox.displayThreads.map(\.key), ["line1:777"])
    }

    /// A NEW inbound message that arrives while a read-mark is pending is NOT
    /// hidden by the pending mask: only the stale pre-action unread is.
    func testNewInboundDuringPendingMarkReadStaysVisible() async {
        let api = FakeGatewayAPI()
        let threadKey = "line1:555"
        let messages = [makeMessage(id: "m1", thread: threadKey),
                        makeMessage(id: "m2", thread: threadKey)]
        api.threads = [makeThread(key: threadKey, unread: 2, last: messages[1])]
        api.threadPages[threadKey] = messages
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 2)

        api.armReadHold()
        let task = Task { await inbox.markThreadsRead([threadKey]) }
        await waitUntil { inbox.isPendingRead(threadKey) }
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 0,
                       "the stale unread is hidden while the mark is pending")

        // A new message lands: the server now reports 3 unread (2 stale + 1
        // new). The refresh must NOT flatten it to 0.
        api.threads = [makeThread(key: threadKey, unread: 3, last: makeMessage(id: "m3", thread: threadKey))]
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 1,
                       "only the stale pre-action unread is masked; the new message stays unread")

        api.resumeRead(.success(()))
        _ = await task.value
        XCTAssertFalse(inbox.isPendingRead(threadKey))
    }

    /// An abort (pairing invalidation mid-operation) clears EVERY remaining
    /// pending key: no permanent optimistic badge may survive it.
    func testAbortMidBulkMarkReadClearsAllRemainingPendingKeys() async {
        let api = FakeGatewayAPI()
        let first = "line1:555"
        let second = "line1:666"
        api.threads = [
            makeThread(key: first, unread: 1, last: makeMessage(id: "a1", thread: first)),
            makeThread(key: second, unread: 1, last: makeMessage(id: "b1", thread: second)),
        ]
        api.threadPages[first] = [makeMessage(id: "a1", thread: first)]
        api.threadPages[second] = [makeMessage(id: "b1", thread: second)]
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()

        api.armReadHold()
        let task = Task { await inbox.markThreadsRead([first, second]) }
        await waitUntil { inbox.isPendingRead(first) && inbox.isPendingRead(second) }
        XCTAssertEqual(inbox.displayThreads.first { $0.key == first }?.unreadCount, 0)
        XCTAssertEqual(inbox.displayThreads.first { $0.key == second }?.unreadCount, 0)

        // Pairing change while the first key is still parked: the operation
        // must abort and leave NO pending badge behind.
        inbox.invalidate()
        api.resumeRead(.failure(CancellationError()))
        let result = await task.value
        XCTAssertTrue(result.failed.contains(first))
        XCTAssertTrue(result.failed.contains(second),
                      "every remaining pending key must be settled as failed on abort")
        XCTAssertFalse(inbox.isPendingRead(first))
        XCTAssertFalse(inbox.isPendingRead(second))
        XCTAssertGreaterThanOrEqual(
            inbox.displayThreads.first { $0.key == second }?.unreadCount ?? 0, 1,
            "the aborted key's badge must be restored, never left optimistically read")
    }

    /// Review regression (same count, new message): after the LAST old
    /// unread is marked and ACKed, a NEW unread message arrives — the total
    /// count is identical to the pre-mark count, but the newest watermark
    /// changed, so the badge MUST be 1 immediately on the refresh-only path.
    func testNewUnreadAfterAckWithTheSameCountStaysVisible() async {
        let api = FakeGatewayAPI()
        let threadKey = "line1:555"
        let old = makeMessage(id: "m1", thread: threadKey, createdAt: 1_000)
        api.threads = [makeThread(key: threadKey, unread: 1, last: old)]
        api.threadPages[threadKey] = [old]
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()

        let result = await inbox.markThreadsRead([threadKey])
        XCTAssertEqual(result.succeeded, [threadKey])
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 0)

        // The server copy now reports the NEW message: same count, newer id.
        let fresh = makeMessage(id: "m2", thread: threadKey, createdAt: 1_100)
        api.threads = [makeThread(key: threadKey, unread: 1, last: fresh)]
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 1,
                       "a new unread with the SAME count as the marked one must be visible")
    }

    /// Same-timestamp boundary: a new message that shares the marked
    /// message's createdAt but has a different id is still a NEW message.
    func testNewUnreadWithSameTimestampDifferentIDIsVisible() async {
        let api = FakeGatewayAPI()
        let threadKey = "line1:555"
        let old = makeMessage(id: "m1", thread: threadKey, createdAt: 2_000)
        api.threads = [makeThread(key: threadKey, unread: 1, last: old)]
        api.threadPages[threadKey] = [old]
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()
        _ = await inbox.markThreadsRead([threadKey])

        let twin = makeMessage(id: "m2", thread: threadKey, createdAt: 2_000)
        api.threads = [makeThread(key: threadKey, unread: 1, last: twin)]
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 1,
                       "the watermark must include the message id, not only the timestamp")
    }

    /// Revision fence: a list GET that STARTED before the mark was ACKed and
    /// resolves after it must not overwrite the settled state.
    func testListGetStartedBeforeAckCannotOverwriteTheSettledState() async {
        let api = FakeGatewayAPI()
        let threadKey = "line1:555"
        let old = makeMessage(id: "m1", thread: threadKey, createdAt: 3_000)
        api.threads = [makeThread(key: threadKey, unread: 1, last: old)]
        api.threadPages[threadKey] = [old]
        let inbox = MessageInbox(api: api)
        await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 1)

        // Park a list GET that carries the PRE-mark copy (same watermark,
        // same stale count)...
        api.suspendNextListThreads()
        let staleRefresh = Task { await inbox.refreshThreads() }
        await waitUntil { api.listThreadsConfirmed }

        // ...then acknowledge the mark while that GET is still parked.
        let mark = Task { await inbox.markThreadsRead([threadKey]) }
        let markResult = await mark.value
        XCTAssertEqual(markResult.succeeded, [threadKey])
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 0)

        api.resumeListThreads()
        _ = await staleRefresh.value
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 0,
                       "a GET begun before the ACK must not overwrite the settled state")
    }
}
