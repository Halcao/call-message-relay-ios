import XCTest
@testable import CallRelay

@MainActor
final class MediaRecoveryPolicyTests: XCTestCase {

    func testBackoffSequenceIsImmediateThenOneTwoFourEightCapped() {
        let p = MediaRecoveryPolicy()
        XCTAssertEqual(p.delayBefore(attempt: 0), 0)
        XCTAssertEqual(p.delayBefore(attempt: 1), 1)
        XCTAssertEqual(p.delayBefore(attempt: 2), 2)
        XCTAssertEqual(p.delayBefore(attempt: 3), 4)
        XCTAssertEqual(p.delayBefore(attempt: 4), 8)
        XCTAssertEqual(p.delayBefore(attempt: 5), 8)
        XCTAssertEqual(p.delayBefore(attempt: 20), 8)
    }

    func testAutoRouteOrderCurrentFirstThenAlternate() {
        let p = MediaRecoveryPolicy()
        XCTAssertEqual(p.routesFor(mode: .auto, current: .relay, alternateAvailable: true),
                       [.relay, .direct])
        XCTAssertEqual(p.routesFor(mode: .auto, current: .direct, alternateAvailable: true),
                       [.direct, .relay])
        XCTAssertEqual(p.routesFor(mode: .auto, current: .relay, alternateAvailable: false), [.relay])
    }

    func testForcedModesNeverCrossRoutes() {
        let p = MediaRecoveryPolicy()
        XCTAssertEqual(p.routesFor(mode: .direct, current: .relay, alternateAvailable: true), [.direct])
        XCTAssertEqual(p.routesFor(mode: .relay, current: .direct, alternateAvailable: true), [.relay])
    }

    func testAbsoluteDeadlineIs45Seconds() {
        let p = MediaRecoveryPolicy()
        let start = Date()
        XCTAssertTrue(p.isWithinDeadline(startedAt: start, now: start.addingTimeInterval(44)))
        XCTAssertTrue(p.isWithinDeadline(startedAt: start, now: start.addingTimeInterval(45)))
        XCTAssertFalse(p.isWithinDeadline(startedAt: start, now: start.addingTimeInterval(46)))
    }
}

@MainActor
final class MediaRecoveryControllerTests: XCTestCase {

    private func makeController(window: TimeInterval = 1,
                                backoff: [TimeInterval] = [0.02, 0.02]) -> MediaRecoveryController {
        MediaRecoveryController(policy: MediaRecoveryPolicy(window: window, backoff: backoff, immediate: 0))
    }

    func testRepeatedLossDoesNotRestartOrExtendDeadline() async {
        let c = makeController()
        var attempts = 0
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { _ in attempts += 1 }, onExpire: {})
        let first = c.startedUptime
        // Repeated loss while the episode is live is ignored.
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { _ in attempts += 1 }, onExpire: { XCTFail("must not re-arm") })
        XCTAssertEqual(c.startedUptime, first)
        XCTAssertTrue(c.isActive)
        try? await Task.sleep(nanoseconds: 30_000_000)
        c.cancel()
    }

    func testFreshMediaForCurrentAttemptClearsEpisode() async {
        let c = makeController(backoff: [100])
        let gate = expectation(description: "attempt ran")
        gate.assertForOverFulfill = false
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in
            // Accept proof only for THIS full identity (call+episode+attempt).
            XCTAssertTrue(c.succeed(attempt.identity))
            gate.fulfill()
        }, onExpire: { XCTFail("fresh media must cancel the deadline") })
        await fulfillment(of: [gate], timeout: 2)
        // The old identity must no longer succeed after the episode closed.
        let stale = MediaRecoveryController.ProofIdentity(callId: "c1", episodeID: c.episodeID, attemptID: 1)
        XCTAssertFalse(c.succeed(stale))
        XCTAssertFalse(c.isActive)
    }

    func testStaleAttemptProofRejected() async {
        let c = makeController(backoff: [0.01])
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { _ in }, onExpire: {})
        // Wrong call id.
        XCTAssertFalse(c.succeed(.init(callId: "other", episodeID: c.episodeID, attemptID: 1)))
        // Wrong episode (a previous episode's queued callback).
        XCTAssertFalse(c.succeed(.init(callId: "c1", episodeID: UUID(), attemptID: 1)))
        // Future attempt id not current yet.
        XCTAssertFalse(c.succeed(.init(callId: "c1", episodeID: c.episodeID, attemptID: 42)))
        c.cancel()
    }

    func testOldSocketCallbackAfterAttempt2IsRejected() async {
        // Simulates the review scenario: attempt 1's transport is superseded
        // while attempt 2 is in flight; a frame callback carrying attempt 1's
        // captured identity (or the old socket) must not clear attempt 2 —
        // and vice versa the live attempt identity must match exactly.
        let c = makeController(window: 2, backoff: [0.01])
        let attempt2 = expectation(description: "attempt 2 running")
        var firstIdentity: MediaRecoveryController.ProofIdentity?
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in
            if attempt.attemptID == 1 {
                firstIdentity = attempt.identity
            } else if attempt.attemptID == 2 {
                attempt2.fulfill()
            }
        }, onExpire: {})
        await fulfillment(of: [attempt2], timeout: 3)
        // Attempt 1's captured identity can no longer succeed.
        let staleEpisodes = [firstIdentity?.episodeID, UUID()].compactMap { $0 }
        for episode in staleEpisodes {
            XCTAssertFalse(c.succeed(.init(callId: "c1", episodeID: episode, attemptID: 1)))
        }
        // A different episode with a matching attempt number is rejected too.
        XCTAssertFalse(c.succeed(.init(callId: "c1", episodeID: UUID(), attemptID: 2)))
        c.cancel()
    }

    func testNewEpisodeDoesNotAcceptPreviousEpisodeProof() async {
        // Episode ids are minted per begin, so attempt counters resetting
        // (both episodes have attempt 1) can never cross-succeed.
        let first = makeController(window: 2, backoff: [100])
        var firstIdentity: MediaRecoveryController.ProofIdentity?
        first.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                    onAttempt: { attempt in firstIdentity = attempt.identity }, onExpire: {})
        try? await Task.sleep(nanoseconds: 30_000_000)
        first.cancel()
        let second = makeController(window: 2, backoff: [100])
        var secondIdentity: MediaRecoveryController.ProofIdentity?
        second.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                     onAttempt: { attempt in secondIdentity = attempt.identity }, onExpire: {})
        try? await Task.sleep(nanoseconds: 30_000_000)
        XCTAssertNotNil(firstIdentity)
        XCTAssertNotNil(secondIdentity)
        XCTAssertNotEqual(firstIdentity?.episodeID, secondIdentity?.episodeID)
        // The first episode's proof must not succeed the second episode.
        if let firstIdentity {
            XCTAssertFalse(second.succeed(firstIdentity))
        }
        second.cancel()
    }

    func testExpireFiresAtDeadlineWithoutFreshMedia() async {
        let c = makeController(window: 0.1, backoff: [1])
        let expired = expectation(description: "expired")
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { _ in }, onExpire: { expired.fulfill() })
        await fulfillment(of: [expired], timeout: 2)
    }

    func testAttemptsAreSerialized() async {
        // Each onAttempt runs to completion before the next: the second
        // attempt never starts while the first is still in its "attach".
        let c = makeController(window: 2, backoff: [0.01])
        let second = expectation(description: "second attempt")
        var inFlight = false
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in
                    if attempt.attemptID == 1 {
                        inFlight = true
                        try? await Task.sleep(nanoseconds: 150_000_000)
                        inFlight = false
                    } else if attempt.attemptID == 2 {
                        XCTAssertFalse(inFlight, "second attempt raced the first")
                        second.fulfill()
                    }
                }, onExpire: {})
        await fulfillment(of: [second], timeout: 3)
        c.cancel()
    }

    // MARK: - Build-49 independent deadline

    func testHungAttachBeyondDeadlineStillExpiresEpisode() async {
        // The absolute deadline must fire even while onAttempt is still
        // awaiting (build-48 only checked the deadline after the await
        // returned, so a hung offer/socket could park the window forever).
        let c = makeController(window: 0.15, backoff: [100])
        let expired = expectation(description: "expired despite hung attach")
        var parked: CheckedContinuation<Void, Never>?
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { _ in
                    await withCheckedContinuation { parked = $0 }
                }, onExpire: { expired.fulfill() })
        await fulfillment(of: [expired], timeout: 2)
        parked?.resume()
        c.cancel()
    }

    func testProofArrivingAfterDeadlineIsRejected() async {
        // A late frame (proof delivered after the absolute deadline) must not
        // clear the episode; expiry owns the ending.
        let c = makeController(window: 0.15, backoff: [100])
        let expired = expectation(description: "expired")
        var captured: MediaRecoveryController.ProofIdentity?
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in
                    captured = attempt.identity
                    try? await Task.sleep(nanoseconds: 300_000_000)
                }, onExpire: { expired.fulfill() })
        await fulfillment(of: [expired], timeout: 2)
        try? await Task.sleep(nanoseconds: 50_000_000)
        guard let captured else { XCTFail("no identity captured"); return }
        XCTAssertFalse(c.succeed(captured), "post-deadline proof must not succeed")
        XCTAssertFalse(c.isActive)
    }

    func testExpiryFromRecoveredEpisodeNeverFiresLater() async {
        // succeed() cancels the independent deadline task: after recovery, the
        // old episode's expiry must never fire (stale timer cannot end a
        // recovered call), and a NEW episode on the same controller still works.
        let c = makeController(window: 0.2, backoff: [100])
        var firstIdentity: MediaRecoveryController.ProofIdentity?
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in firstIdentity = attempt.identity },
                onExpire: { XCTFail("stale expiry fired after recovery") })
        try? await Task.sleep(nanoseconds: 30_000_000)
        guard let firstProof = firstIdentity else { XCTFail("no identity"); return }
        XCTAssertTrue(c.succeed(firstProof))
        // Wait well past the first episode's deadline.
        try? await Task.sleep(nanoseconds: 400_000_000)
        XCTAssertFalse(c.isActive)
        // A later loss on the same call may open a new episode (begin is
        // allowed once the previous one ended cleanly).
        var secondIdentity: MediaRecoveryController.ProofIdentity?
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in secondIdentity = attempt.identity },
                onExpire: { XCTFail("second episode expired with backoff 100s") })
        try? await Task.sleep(nanoseconds: 30_000_000)
        XCTAssertNotEqual(firstIdentity?.episodeID, secondIdentity?.episodeID)
        if let secondIdentity, let stale = firstIdentity {
            XCTAssertFalse(c.succeed(stale), "old proof must not clear the new episode")
        }
        c.cancel()
    }

    func testCancelBeforeDeadlineSuppressesExpiry() async {
        let c = makeController(window: 0.3, backoff: [100])
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { _ in },
                onExpire: { XCTFail("cancelled episode must not expire") })
        try? await Task.sleep(nanoseconds: 30_000_000)
        c.cancel()
        try? await Task.sleep(nanoseconds: 400_000_000)
        XCTAssertFalse(c.isActive)
    }

    func testNoSilentSettleIsAddedToBackoff() async {
        // Build-49 removed the hidden 1.5 s settle sleep: with immediate
        // attempts and a 1 s window the controller fits MANY more attempts
        // than the old settle-burdened loop (each cycle cost backoff+1.5 s).
        let c = makeController(window: 1.0, backoff: [0.02])
        var attempts = 0
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { _ in attempts += 1 },
                onExpire: {})
        try? await Task.sleep(nanoseconds: 900_000_000)
        // 1 s window, ~0.02 s backoff, zero attempt work: without the 1.5 s
        // settle this runs ~40+ attempts; with it, only ONE could fit.
        XCTAssertGreaterThan(attempts, 5, "backoff still carries a hidden settle sleep")
        c.cancel()
    }

    // MARK: - Build-49 review: monotonic clock + immediate loop cancellation

    /// A deterministic monotonic clock the tests advance by hand.
    private final class FakeClock {
        var now: TimeInterval = 1_000
    }

    private func makeClockedController(clock: FakeClock, window: TimeInterval = 10,
                                       backoff: [TimeInterval] = [1])
    -> MediaRecoveryController {
        let c = MediaRecoveryController(
            policy: MediaRecoveryPolicy(window: window, backoff: backoff, immediate: 0))
        c.uptime = { clock.now }
        return c
    }

    func testExpiryCancelsTheInFlightAttemptLoopImmediately() async {
        // After the deadline fires with an attempt hung, resuming the hung
        // attempt must NOT schedule further attempts or re-enter onAttempt:
        // the loop was cancelled at expiry, deterministically.
        let clock = FakeClock()
        let c = makeClockedController(clock: clock, window: 0.05, backoff: [100])
        let expired = expectation(description: "expired")
        var attemptCalls = 0
        var parked: CheckedContinuation<Void, Never>?
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { _ in
                    attemptCalls += 1
                    await withCheckedContinuation { parked = $0 }
                }, onExpire: { expired.fulfill() })
        // Drive the monotonic clock past the deadline and let the expire
        // task's REAL sleep elapse (it sleeps in wall time but decides on the
        // monotonic clock; 0.05 s window keeps the test fast).
        clock.now += 1.0
        await fulfillment(of: [expired], timeout: 2)
        let callsAtExpiry = attemptCalls
        parked?.resume()
        await pumpMainActor(20)
        XCTAssertEqual(attemptCalls, callsAtExpiry,
                       "a resumed late candidate must not run another attempt")
        XCTAssertFalse(c.isActive)
    }

    func testMonotonicClockWallChangeCannotExtendWindow() async {
        // A wall-clock jump (travel/NTP) does not move the monotonic window:
        // with a fake uptime clock the deadline follows the FAKE clock only.
        let clock = FakeClock()
        let c = makeClockedController(clock: clock, window: 10, backoff: [100])
        var captured: MediaRecoveryController.ProofIdentity?
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in captured = attempt.identity }, onExpire: {})
        await pumpMainActor(5)
        clock.now += 11 // monotonic window exhausted
        try? await Task.sleep(nanoseconds: 20_000_000)
        guard let captured else { XCTFail("no identity"); return }
        XCTAssertFalse(c.succeed(captured),
                       "proof after the MONOTONIC deadline must be rejected")
        c.cancel()
    }

    func testNewLossAfterExpiryOpensNewEpisodeAndOldProofStaysDead() async {
        // The controller contract after expiry: the episode is over; a real
        // new loss (fresh begin on the same call id, the production pattern —
        // the coordinator reuses one controller) opens a NEW episode whose
        // identity differs, and the expired episode's proof can never clear
        // it even after the new episode expires too.
        let clock = FakeClock()
        let c = makeClockedController(clock: clock, window: 0.05, backoff: [100])
        let expired = expectation(description: "first expired")
        var oldIdentity: MediaRecoveryController.ProofIdentity?
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in oldIdentity = attempt.identity },
                onExpire: { expired.fulfill() })
        await pumpMainActor(10) // let attempt 1 of episode 1 run (immediate)
        XCTAssertNotNil(oldIdentity)
        clock.now += 1.0 // monotonic deadline of episode 1 is past
        await fulfillment(of: [expired], timeout: 2)

        let secondExpired = expectation(description: "second expired")
        var newIdentity: MediaRecoveryController.ProofIdentity?
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in newIdentity = attempt.identity },
                onExpire: { secondExpired.fulfill() })
        await pumpMainActor(10) // attempt 1 of episode 2 runs before its deadline
        XCTAssertNotNil(newIdentity)
        if let oldIdentity, let newIdentity {
            XCTAssertNotEqual(oldIdentity.episodeID, newIdentity.episodeID)
            XCTAssertFalse(c.succeed(oldIdentity),
                           "the expired episode's proof must not clear the new one")
        }
        clock.now += 1.0 // monotonic deadline of episode 2 is past
        await fulfillment(of: [secondExpired], timeout: 2)
        if let newIdentity {
            XCTAssertFalse(c.succeed(newIdentity),
                           "proof after the new episode's own deadline must be rejected")
        }
        XCTAssertFalse(c.isActive)
    }

    // MARK: - Build-66 fresh-path nudge (retryNow)

    func testRetryNowShortensOnlyTheInterAttemptWait() async {
        // A long backoff step (5 s) is shortened by a path-change nudge: the
        // NEXT attempt runs promptly, the deadline and serialization are
        // unchanged.
        let c = makeController(window: 5, backoff: [5])
        let first = expectation(description: "attempt 1")
        let second = expectation(description: "attempt 2")
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in
                    if attempt.attemptID == 1 { first.fulfill() }
                    if attempt.attemptID == 2 { second.fulfill() }
                }, onExpire: {})
        await fulfillment(of: [first], timeout: 2)
        // Let the loop park on the 5 s inter-attempt wait, then nudge.
        try? await Task.sleep(nanoseconds: 50_000_000)
        c.retryNow()
        // Must NOT wait the full 5 s (bounded well below it).
        await fulfillment(of: [second], timeout: 1)
        c.cancel()
    }

    func testRetryNowIsIgnoredWhileAnAttemptIsInFlight() async {
        // The single-owner invariant: a nudge can never start a second
        // concurrent attempt; the in-flight attach owns the next step.
        let c = makeController(window: 5, backoff: [0.01])
        let running = expectation(description: "attempt running")
        var release: CheckedContinuation<Void, Never>?
        var attemptCount = 0
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { _ in
                    attemptCount += 1
                    running.fulfill()
                    await withCheckedContinuation { release = $0 }
                }, onExpire: {})
        await fulfillment(of: [running], timeout: 2)
        c.retryNow()
        c.retryNow()
        try? await Task.sleep(nanoseconds: 50_000_000)
        XCTAssertEqual(attemptCount, 1, "a nudge must never race an in-flight attempt")
        release?.resume()
        c.cancel()
    }

    func testRetryNowCooldownAndIdleNoOps() async {
        let c = makeController(window: 5, backoff: [5])
        // Idle: nothing to nudge.
        c.retryNow()
        XCTAssertFalse(c.isActive)
        let first = expectation(description: "attempt 1")
        let second = expectation(description: "attempt 2")
        var attempts = 0
        c.nudgeCooldown = 10
        c.begin(callId: "c1", mode: .relay, current: .relay, alternateAvailable: false,
                onAttempt: { attempt in
                    attempts += 1
                    if attempt.attemptID == 1 { first.fulfill() }
                    if attempt.attemptID == 2 { second.fulfill() }
                }, onExpire: {})
        await fulfillment(of: [first], timeout: 2)
        try? await Task.sleep(nanoseconds: 50_000_000)
        // A burst of nudges inside the cooldown collapses to one prompt
        // attempt (no storm — the second attempt then waits its own backoff).
        for _ in 0..<10 { c.retryNow() }
        await fulfillment(of: [second], timeout: 1)
        XCTAssertEqual(attempts, 2)
        c.cancel()
    }
}
