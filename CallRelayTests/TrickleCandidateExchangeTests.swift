import XCTest
@testable import CallRelay

/// Deterministic tests for the build-68 incremental ICE exchange: the loop
/// that posts the client's remaining candidates and drains the gateway's own
/// incrementally gathered ones. Every dependency is injected, so no socket,
/// server or timing luck is involved.
@MainActor
final class TrickleCandidateExchangeTests: XCTestCase {
    private final class Recorder: @unchecked Sendable {
        var posted: [[String]] = []
        var applied: [String] = []
        var polls: [Int] = []
        var logs: [String] = []
        var localCandidates: [String] = []
        var localComplete = false

        func snapshotPosted() -> [[String]] {
            posted
        }
    }

    private func makeExchange(_ recorder: Recorder) -> TrickleCandidateExchange {
        TrickleCandidateExchange(cadence: .init(intervalSeconds: 0.02, maximumSeconds: 3)) { message in
            recorder.logs.append(message)
        }
    }

    /// The happy path: local candidates are posted once each in bounded
    /// batches, the gateway's candidates are applied in order, and the
    /// exchange finishes as soon as BOTH sides report completion.
    func testExchangePostsAndDrainsUntilComplete() async throws {
        let recorder = Recorder()
        recorder.localCandidates = ["candidate:1", "candidate:2", "candidate:3"]
        recorder.localComplete = true
        let exchange = makeExchange(recorder)
        var pollIndex = 0
        exchange.start(
            attachId: "attach-1",
            initialRemoteCursor: 4,
            localCandidates: { recorder.localCandidates },
            localGatheringComplete: { recorder.localComplete },
            post: { _, candidates in recorder.posted.append(candidates) },
            poll: { _, after in
                XCTAssertEqual(after, pollIndex == 0 ? 4 : 6, "cursor must advance monotonically")
                defer { pollIndex += 1 }
                if pollIndex == 0 {
                    return V2CandidateBatch(candidates: ["candidate:r1", "candidate:r2"],
                                            cursor: 6, complete: false, truncated: nil)
                }
                return V2CandidateBatch(candidates: [], cursor: 6, complete: true, truncated: nil)
            },
            apply: { recorder.applied.append($0) })
        try await waitUntil { exchange.finished }
        XCTAssertEqual(recorder.posted.flatMap { $0 }, ["candidate:1", "candidate:2", "candidate:3"])
        XCTAssertEqual(recorder.applied, ["candidate:r1", "candidate:r2"])
        XCTAssertEqual(exchange.postedCount, 3)
        XCTAssertEqual(exchange.appliedRemoteCount, 2)
    }

    /// Batches never exceed the bounded size even when the peer gathered a
    /// burst of candidates at once.
    func testExchangeBoundsEachBatch() async throws {
        let recorder = Recorder()
        recorder.localCandidates = (1...20).map { "candidate:\($0)" }
        recorder.localComplete = true
        let exchange = makeExchange(recorder)
        var pollCount = 0
        exchange.start(
            attachId: "attach-2",
            initialRemoteCursor: 0,
            localCandidates: { recorder.localCandidates },
            localGatheringComplete: { recorder.localComplete },
            post: { _, candidates in
                recorder.posted.append(candidates)
                XCTAssertLessThanOrEqual(candidates.count, 8)
            },
            poll: { _, _ in
                pollCount += 1
                return V2CandidateBatch(candidates: [], cursor: 0, complete: pollCount > 8, truncated: nil)
            },
            apply: { _ in })
        try await waitUntil { exchange.finished }
        XCTAssertEqual(recorder.posted.count, 3)
        XCTAssertEqual(recorder.posted.map(\.count), [8, 8, 4])
    }

    /// A stale attach (409) ends the exchange quietly: the negotiation it
    /// belonged to is gone, and retrying would be pointless noise.
    func testStaleAttachStopsTheExchange() async throws {
        let recorder = Recorder()
        recorder.localCandidates = ["candidate:1"]
        let exchange = makeExchange(recorder)
        exchange.start(
            attachId: "attach-3",
            initialRemoteCursor: 0,
            localCandidates: { recorder.localCandidates },
            localGatheringComplete: { false },
            post: { _, _ in throw APIError.http(status: 409, code: "CB-V2-409G", message: "stale") },
            poll: { _, _ in
                XCTFail("poll must not run after a stale attach")
                return V2CandidateBatch(candidates: [], cursor: 0, complete: false, truncated: nil)
            },
            apply: { _ in })
        try await waitUntil { exchange.finished }
        XCTAssertTrue(recorder.logs.contains { $0.contains("attach superseded") })
        XCTAssertEqual(exchange.postedCount, 0)
    }

    /// A throttled poll (429) is tolerated: it must not end the exchange, and
    /// the next cycle must still complete normally.
    func testThrottledPollIsTolerated() async throws {
        let recorder = Recorder()
        recorder.localCandidates = ["candidate:1"]
        recorder.localComplete = true
        let exchange = makeExchange(recorder)
        var throttled = false
        var polls = 0
        exchange.start(
            attachId: "attach-4",
            initialRemoteCursor: 0,
            localCandidates: { recorder.localCandidates },
            localGatheringComplete: { recorder.localComplete },
            post: { _, candidates in recorder.posted.append(candidates) },
            poll: { _, _ in
                polls += 1
                if !throttled {
                    throttled = true
                    throw APIError.http(status: 429, code: "CB-V2-429", message: "slow down")
                }
                return V2CandidateBatch(candidates: ["candidate:r"], cursor: 1, complete: true, truncated: nil)
            },
            apply: { recorder.applied.append($0) })
        try await waitUntil { exchange.finished }
        XCTAssertGreaterThanOrEqual(polls, 2)
        XCTAssertEqual(recorder.applied, ["candidate:r"])
        XCTAssertEqual(recorder.logs.filter { $0.contains("gave up") }.count, 0)
    }

    /// Transient transport failures use a capped backoff and give up after the
    /// bounded number of consecutive failures instead of looping forever.
    func testRepeatedFailuresGiveUpBounded() async throws {
        let recorder = Recorder()
        let exchange = TrickleCandidateExchange(
            cadence: .init(intervalSeconds: 0.01, maximumSeconds: 5,
                           maximumConsecutiveFailures: 3, initialBackoffSeconds: 0.01,
                           maximumBackoffSeconds: 0.02, maximumBatch: 8)) { recorder.logs.append($0) }
        exchange.start(
            attachId: "attach-5",
            initialRemoteCursor: 0,
            localCandidates: { [] },
            localGatheringComplete: { false },
            post: { _, _ in },
            poll: { _, _ in throw URLError(.timedOut) },
            apply: { _ in })
        try await waitUntil { exchange.finished }
        XCTAssertTrue(recorder.logs.contains { $0.contains("gave up") })
    }

    /// Cancelling the exchange stops it at once (a consumed handoff or a call
    /// teardown must not leave a candidate loop running).
    func testCancelStopsImmediately() async throws {
        let recorder = Recorder()
        let exchange = makeExchange(recorder)
        exchange.start(
            attachId: "attach-6",
            initialRemoteCursor: 0,
            localCandidates: { recorder.localCandidates },
            localGatheringComplete: { false },
            post: { _, _ in },
            poll: { _, _ in V2CandidateBatch(candidates: [], cursor: 0, complete: false, truncated: nil) },
            apply: { _ in })
        try await Task.sleep(nanoseconds: 60_000_000)
        exchange.cancel()
        XCTAssertTrue(exchange.finished)
        let appliedBefore = exchange.appliedRemoteCount
        try await Task.sleep(nanoseconds: 100_000_000)
        XCTAssertEqual(exchange.appliedRemoteCount, appliedBefore)
    }

    private func waitUntil(timeout: TimeInterval = 4,
                           _ condition: @escaping () -> Bool) async throws {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if condition() { return }
            try await Task.sleep(nanoseconds: 10_000_000)
        }
        XCTFail("condition not reached within \(timeout)s")
    }
}
