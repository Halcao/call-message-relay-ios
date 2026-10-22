import XCTest
@testable import CallRelay

/// HotStandbyController (build 50) tests: composite scorer (RTT is NOT the
/// whole rule), fenced make-before-break promotion, rollback, and the
/// paced-frame-ID transition evidence. The frame IDs make the automated
/// audibility boundary concrete: exactly one render-source switch, no
/// duplicated frames, an honestly measured live-edge drop count.
@MainActor
final class HotStandbyControllerTests: XCTestCase {

    private typealias Kind = HotStandbyController.RouteKind

    /// Scripted collaborator capture.
    private final class Harness {
        let box = MainActorBox()
        var primary: Kind = .relay
        var fresh: [Kind: Bool] = [.direct: true]
        var promoteAck: UInt64? = 7
        var serverRoles: (primary: Kind, epoch: UInt64)?
        var transferResult = true
        var demoteFence: HotStandbyController.StandbyFence?
        var promoteCommandCount = 0
        var transferCount = 0
        var makePromoteCommandAsync = false
        var published: [HotStandbyController.HotStandbyState] = []
        var renderedIDs: [Int64] = []
        var currentRenderOwner: Kind = .relay
        // Build-65 deterministic barriers: park the promote command / the
        // demote await until the test invalidates (a plain Task.yield cannot
        // guarantee the invalidation lands while the promote is pending).
        var parkPromoteCommand = false
        private(set) var promoteCommandEntered = false
        var parkDemote = false
        private(set) var demoteEntered = false
        private var promoteContinuation: CheckedContinuation<UInt64?, Never>?
        private var promoteEnteredContinuation: CheckedContinuation<Void, Never>?
        private var demoteContinuation: CheckedContinuation<HotStandbyController.StandbyFence?, Never>?
        private var demoteEnteredContinuation: CheckedContinuation<Void, Never>?

        /// Barrier: resumes when the promote command callback is ENTERED
        /// (race-safe if it already entered before the call).
        func waitForPromoteCommandEntered() async {
            if promoteCommandEntered { return }
            await withCheckedContinuation { (cont: CheckedContinuation<Void, Never>) in
                promoteEnteredContinuation = cont
                if promoteCommandEntered { cont.resume() }
            }
        }

        func resumePromoteCommand(_ ack: UInt64?) {
            parkPromoteCommand = false
            promoteContinuation?.resume(returning: ack)
            promoteContinuation = nil
        }

        /// Barrier: resumes when the demote callback is ENTERED.
        func waitForDemoteEntered() async {
            if demoteEntered { return }
            await withCheckedContinuation { (cont: CheckedContinuation<Void, Never>) in
                demoteEnteredContinuation = cont
                if demoteEntered { cont.resume() }
            }
        }

        func resumeDemote(_ fence: HotStandbyController.StandbyFence?) {
            parkDemote = false
            demoteContinuation?.resume(returning: fence)
            demoteContinuation = nil
        }

        func make() -> HotStandbyController {
            // Harness callbacks close over MainActor state; build on the main actor.
            MainActor.assumeIsolated {
                HotStandbyController(callbacks: .init(
                currentPrimary: { [weak self] in self?.primary ?? .relay },
                standbyFresh: { [weak self] kind in self?.fresh[kind] ?? false },
                promoteCommand: { [weak self] fence, _ in
                    guard let self else { return nil }
                    if self.parkPromoteCommand {
                        self.promoteCommandEntered = true
                        self.promoteEnteredContinuation?.resume()
                        self.promoteEnteredContinuation = nil
                        return await withCheckedContinuation { cont in
                            self.promoteContinuation = cont
                        }
                    }
                    if self.makePromoteCommandAsync {
                        await Task.yield()
                    }
                    self.promoteCommandCount += 1
                    // The ack carries the demoted primary's fence.
                    self.demoteFence = .init(kind: self.primary, token: "demoted-\(self.primary.rawValue)", epoch: (self.promoteAck ?? 0))
                    return self.promoteAck
                },
                refreshServerRoles: { [weak self] in self?.serverRoles },
                transferAudioOwnership: { [weak self] kind in
                    guard let self else { return false }
                    self.transferCount += 1
                    if self.transferResult {
                        self.primary = kind
                        self.currentRenderOwner = kind
                    }
                    return self.transferResult
                },
                demoteCurrentToStandby: { [weak self] in
                    guard let self else { return nil }
                    if self.parkDemote {
                        self.demoteEntered = true
                        self.demoteEnteredContinuation?.resume()
                        self.demoteEnteredContinuation = nil
                        return await withCheckedContinuation { cont in
                            self.demoteContinuation = cont
                        }
                    }
                    return self.demoteFence
                },
                canMaintainStandby: { true },
                onState: { [weak self] state in self?.published.append(state) }
                ))
            }
        }
    }

    /// Marker so the assumeIsolated above documents itself; tests run on the
    /// main actor already (XCTest main-thread trampoline).
    private final class MainActorBox { init() {} }

    private func makeController() -> (HotStandbyController, Harness) {
        let h = Harness()
        return (h.make(), h)
    }

    // MARK: Scorer

    func testScorerRequiresSustainedMaterialAdvantage() {
        let (c, _) = makeController()
        let standby = HotStandbyController.PathQuality(rttMs: 50, jitterMs: 5, backlogMs: 0, freshnessAgeMs: 100, stable: true)
        let primary = HotStandbyController.PathQuality(rttMs: 100, jitterMs: 10, backlogMs: 20, freshnessAgeMs: 200, stable: true)
        let now = Date()
        // First observation arms the dwell clock, does not promote.
        XCTAssertFalse(c.shouldPromote(standby: standby, primary: primary, primaryFailed: false, now: now))
        // Before dwell: no.
        XCTAssertFalse(c.shouldPromote(standby: standby, primary: primary, primaryFailed: false, now: now.addingTimeInterval(3)))
        // After dwell: yes.
        XCTAssertTrue(c.shouldPromote(standby: standby, primary: primary, primaryFailed: false, now: now.addingTimeInterval(7)))
    }

    func testScorerCooldownBlocksThrashAndFailureBypassesIt() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = true
        let outcome = await c.promoteStandby(.direct)
        XCTAssertEqual(outcome, .promoted(newPrimary: .direct))
        // Cooldown now blocks a quality-driven swap back even with advantage.
        let standby = HotStandbyController.PathQuality(rttMs: 10, jitterMs: 1, backlogMs: 0, freshnessAgeMs: 0, stable: true)
        let primary = HotStandbyController.PathQuality(rttMs: 100, jitterMs: 10, backlogMs: 20, freshnessAgeMs: 200, stable: true)
        let now = Date()
        _ = c.shouldPromote(standby: standby, primary: primary, primaryFailed: false, now: now)
        XCTAssertFalse(c.shouldPromote(standby: standby, primary: primary, primaryFailed: false, now: now.addingTimeInterval(7)))
        // Primary failure bypasses cooldown AND dwell.
        XCTAssertTrue(c.shouldPromote(standby: standby, primary: primary, primaryFailed: true, now: now.addingTimeInterval(7)))
    }

    func testScorerCompositeNotRttOnly() {
        let (c, _) = makeController()
        // Standby has better RTT but is unstable: never material.
        let standby = HotStandbyController.PathQuality(rttMs: 10, jitterMs: 1, backlogMs: 0, freshnessAgeMs: 0, stable: false)
        let primary = HotStandbyController.PathQuality(rttMs: 500, jitterMs: 50, backlogMs: 100, freshnessAgeMs: 500, stable: true)
        let now = Date()
        _ = c.shouldPromote(standby: standby, primary: primary, primaryFailed: false, now: now)
        XCTAssertFalse(c.shouldPromote(standby: standby, primary: primary, primaryFailed: false, now: now.addingTimeInterval(10)))
        // Worse RTT but materially better jitter+backlog+freshness and stable:
        // composite can still NOT cross 20% (weights keep RTT dominant).
        let jittery = HotStandbyController.PathQuality(rttMs: 100, jitterMs: 5, backlogMs: 5, freshnessAgeMs: 50, stable: true)
        _ = c.shouldPromote(standby: jittery, primary: primary, primaryFailed: false, now: now)
        XCTAssertTrue(c.shouldPromote(standby: jittery, primary: primary, primaryFailed: false, now: now.addingTimeInterval(10)))
    }

    // MARK: Fenced promotion

    func testPromotionSuccessSingleSwitchAndDemoteFence() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "tok-d", epoch: 3))
        h.fresh[.direct] = true
        let outcome = await c.promoteStandby(.direct)
        XCTAssertEqual(outcome, .promoted(newPrimary: .direct))
        XCTAssertEqual(h.primary, .direct)
        XCTAssertEqual(h.promoteCommandCount, 1)
        XCTAssertEqual(h.transferCount, 1)
        // The demoted primary's fence is installed for the reverse swap.
        let demoted = c.fences[.relay]
        XCTAssertEqual(demoted?.token, "demoted-relay")
        XCTAssertEqual(demoted?.epoch, 7)
        // Exactly one publish with the outcome.
        XCTAssertEqual(h.published.last?.primary, .direct)
        XCTAssertEqual(h.published.last?.lastOutcome, .promoted(newPrimary: .direct))
    }

    func testPromotionRequiresFreshMediaProof() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = false // CONNECTED/RTT alone is never proof
        let outcome = await c.promoteStandby(.direct)
        XCTAssertEqual(outcome, .unavailable("standby media not fresh (CONNECTED/RTT is not proof)"))
        XCTAssertEqual(h.promoteCommandCount, 0)
        XCTAssertEqual(h.primary, .relay) // untouched
    }

    func testPromotionRejectsWithoutStandby() async {
        let (c, h) = makeController()
        let outcome = await c.promoteStandby(.direct)
        if case .unavailable = outcome {} else { XCTFail("want unavailable, got \(outcome)") }
        XCTAssertEqual(h.promoteCommandCount, 0)
    }

    func testRejectedCommandReconcilesThenRollsBack() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = true
        h.promoteAck = nil // command rejected / ack lost
        h.serverRoles = (primary: .relay, epoch: 1) // server agrees: no swap
        let outcome = await c.promoteStandby(.direct)
        if case .rolledBack = outcome {} else { XCTFail("want rolledBack, got \(outcome)") }
        XCTAssertEqual(h.primary, .relay)
        XCTAssertEqual(h.transferCount, 0)
    }

    func testLostAckWithLandedSwapCompletesViaReconcile() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = true
        h.promoteAck = nil
        h.serverRoles = (primary: .direct, epoch: 2) // the swap DID land
        let outcome = await c.promoteStandby(.direct)
        XCTAssertEqual(outcome, .promoted(newPrimary: .direct))
        XCTAssertEqual(h.transferCount, 1)
        XCTAssertEqual(h.primary, .direct)
    }

    func testTransferFailureRollsBackWithoutOwnerChange() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = true
        h.transferResult = false
        let outcome = await c.promoteStandby(.direct)
        if case .rolledBack(let reason) = outcome {
            XCTAssertTrue(reason.contains("transfer failed"))
        } else { XCTFail("want rolledBack, got \(outcome)") }
        XCTAssertEqual(h.primary, .relay)
    }

    // MARK: Build-51 failure cooldown (no automatic 5s re-promotion)

    /// A promotion that RAN and failed blocks the AUTOMATIC scorer for the
    /// standard cooldown (the cadence loop must not re-attempt every ~5 s
    /// — the field build thrashed 7 suspend/resume cutouts in 5 s), while
    /// the primary-failure bypass still fires immediately and an EXPLICIT
    /// fresh manual promotion re-enters directly (promoteStandby never
    /// consults the cooldown).
    func testFailedPromotionAppliesAutoCooldownAndPreservesManualIntentAndFailureBypass() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = true
        h.transferResult = false
        let failed = await c.promoteStandby(.direct)
        if case .rolledBack = failed {} else { XCTFail("want rolledBack, got \(failed)") }

        let standby = HotStandbyController.PathQuality(
            rttMs: 10, jitterMs: 1, backlogMs: 0, freshnessAgeMs: 0, stable: true)
        let primary = HotStandbyController.PathQuality(
            rttMs: 100, jitterMs: 10, backlogMs: 20, freshnessAgeMs: 200, stable: true)
        let now = Date()
        _ = c.shouldPromote(standby: standby, primary: primary, primaryFailed: false, now: now)
        // Advantage sustained past the dwell — the failure cooldown must
        // still block the AUTOMATIC decision (and the dwell was reset, so
        // it would not pass anyway).
        XCTAssertFalse(c.shouldPromote(standby: standby, primary: primary,
                                       primaryFailed: false, now: now.addingTimeInterval(7)),
                       "a failed promotion must cool down automatic retries")
        // The current-primary FAILURE bypass precedes the cooldown.
        XCTAssertTrue(c.shouldPromote(standby: standby, primary: primary,
                                      primaryFailed: true, now: now.addingTimeInterval(7)))
        // Explicit fresh manual intent: promoteStandby itself never consults
        // the cooldown.
        h.transferResult = true
        let promoted = await c.promoteStandby(.direct)
        XCTAssertEqual(promoted, .promoted(newPrimary: .direct))
        XCTAssertEqual(h.primary, .direct)
    }

    /// A REJECTED promote command applies the same automatic cooldown: the
    /// cadence loop must not hammer the server every ~5 s after a clean
    /// rejection, while a manual attempt is unaffected.
    func testRejectedCommandAppliesAutoCooldown() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = true
        h.promoteAck = nil
        h.serverRoles = (primary: .relay, epoch: 1)
        let outcome = await c.promoteStandby(.direct)
        if case .rolledBack = outcome {} else { XCTFail("want rolledBack, got \(outcome)") }

        let standby = HotStandbyController.PathQuality(
            rttMs: 10, jitterMs: 1, backlogMs: 0, freshnessAgeMs: 0, stable: true)
        let primary = HotStandbyController.PathQuality(
            rttMs: 100, jitterMs: 10, backlogMs: 20, freshnessAgeMs: 200, stable: true)
        let now = Date()
        _ = c.shouldPromote(standby: standby, primary: primary, primaryFailed: false, now: now)
        XCTAssertFalse(c.shouldPromote(standby: standby, primary: primary,
                                       primaryFailed: false, now: now.addingTimeInterval(7)))
        h.promoteAck = 9
        h.serverRoles = nil
        let promoted = await c.promoteStandby(.direct)
        XCTAssertEqual(promoted, .promoted(newPrimary: .direct))
    }

    func testPromotionIsSerialized() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = true
        // A genuinely async command: the first promotion suspends inside the
        // command, yielding the actor so the second must observe the
        // in-flight guard.
        h.makePromoteCommandAsync = true
        async let first = c.promoteStandby(.direct)
        async let second = c.promoteStandby(.direct)
        let outcomes = await [first, second]
        XCTAssertEqual(outcomes.filter { $0 == .promoted(newPrimary: .direct) }.count, 1)
        XCTAssertEqual(outcomes.filter { if case .unavailable = $0 { return true } else { return false } }.count, 1)
        XCTAssertEqual(h.promoteCommandCount, 1)
    }

    func testInvalidationDuringPendingPromote() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = true
        h.parkPromoteCommand = true
        let task = Task { await c.promoteStandby(.direct) }
        // Deterministic barrier: the invalidation provably lands while the
        // promote command is parked pending (a bare Task.yield could let the
        // promote complete first, which is the flake this test used to be).
        await h.waitForPromoteCommandEntered()
        c.invalidate()     // hangup/new call while the promote HTTP is pending
        h.resumePromoteCommand(7)
        let outcome = await task.value
        XCTAssertEqual(outcome, .unavailable("invalidated"))
        XCTAssertEqual(h.transferCount, 0)
        XCTAssertEqual(h.primary, .relay)
        // Invalidation clears the fences (permanent identity isolation, same
        // contract the coordinator suite asserts) — never an old-token reuse.
        XCTAssertNil(c.fences[.direct])
    }

    /// Build-65: an invalidation while the old primary's DEMOTE is pending
    /// must not resurrect its fence into the cleared table nor publish the
    /// promotion — the late callback touches nothing (same contract as every
    /// other await in completePromotion). Parameterized over the demote's
    /// late return (fence vs nil): BOTH must abort before any install or
    /// `.promoted` publish (the fence is unconditional, not in the non-nil
    /// branch).
    func testInvalidationDuringPendingDemoteNeverResurrectsFence() async {
        for demoteReturnsFence in [true, false] {
            let (c, h) = makeController()
            c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
            h.fresh[.direct] = true
            h.parkDemote = true
            let task = Task { await c.promoteStandby(.direct) }
            // Barrier: the audio transfer completed; the demote await is parked.
            await h.waitForDemoteEntered()
            c.invalidate()
            h.resumeDemote(demoteReturnsFence ? h.demoteFence : nil)
            let outcome = await task.value
            XCTAssertEqual(outcome, .unavailable("invalidated"))
            // The demote's late fence must NOT be installed and the promoted
            // outcome must NOT be published.
            XCTAssertNil(c.fences[.relay])
            XCTAssertNil(c.fences[.direct])
            XCTAssertFalse(h.published.contains { $0.lastOutcome == .promoted(newPrimary: .direct) },
                           "an invalidated lifecycle must never publish promoted (demoteFence=\(demoteReturnsFence))")
            // The transfer itself did run (pre-invalidation); the outcome is
            // still invalidated and no second transfer ever happens.
            XCTAssertEqual(h.transferCount, 1)
        }
    }

    func testPromotedFenceRemovedAndFinalStatePublished() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = true
        let outcome = await c.promoteStandby(.direct)
        XCTAssertEqual(outcome, .promoted(newPrimary: .direct))
        // The promoted kind's fence must NOT linger as a standby fence.
        XCTAssertNil(c.fences[.direct])
        // The demoted primary's fence is installed for the reverse swap.
        XCTAssertEqual(c.fences[.relay]?.token, "demoted-relay")
        // The final publish happened with promotionInFlight == false.
        XCTAssertEqual(h.published.last?.promotionInFlight, false)
        XCTAssertEqual(h.published.last?.primary, .direct)
        XCTAssertEqual(h.published.last?.lastOutcome, .promoted(newPrimary: .direct))
    }

    // MARK: Paced-frame-ID transition evidence

    func testTransitionGapAndNoDuplicateFrames() async {
        let (c, h) = makeController()
        c.installStandby(.init(kind: .direct, token: "t", epoch: 1))
        h.fresh[.direct] = true
        // Paced frames 0...99 render on the relay primary.
        for id in Int64(0)...99 { c.recordPrimaryFrame(id: id); h.renderedIDs.append(id) }
        let outcome = await c.promoteStandby(.direct)
        XCTAssertEqual(outcome, .promoted(newPrimary: .direct))
        // Post-switch frames 105... render on the promoted direct transport;
        // frames 100-104 fell at the live-edge stale-queue discard.
        c.recordNewPrimaryFirstFrame(id: 105)
        for id in Int64(105)...110 { h.renderedIDs.append(id) }
        XCTAssertEqual(c.evidence.renderSourceSwitchCount, 1)
        XCTAssertEqual(c.evidence.lastPrimaryFrameID, 99)
        XCTAssertEqual(c.evidence.firstNewPrimaryFrameID, 105)
        XCTAssertEqual(c.evidence.droppedAtPromotion, 5)
        // No duplicates across the boundary.
        XCTAssertEqual(h.renderedIDs, h.renderedIDs.sorted())
        XCTAssertEqual(Set(h.renderedIDs).count, h.renderedIDs.count)
        // The gap is measured, not zero — reported honestly.
        XCTAssertGreaterThan(c.evidence.droppedAtPromotion, 0)
    }
}

// MARK: Real adapter ingestion logic (build 50)
//
// These test MediaProbeController.fireFreshMediaArm directly — the SAME
// code the real pollStats timer runs — rather than a fake's trigger, so the
// advance/token semantics the standby proofs rely on are proven against the
// production path.
extension HotStandbyControllerTests {
    func testFreshMediaArmFiresOnlyOnLaterAdvance() {
        var arm: (baseline: UInt64, token: UInt64, callback: () -> Void)?
        var fired = 0
        let token: UInt64 = 1
        // Arm at counter 5: only packets AFTER 5 fire.
        arm = (baseline: 5, token: token, callback: { fired += 1 })
        MediaProbeController.fireFreshMediaArm(&arm, currentToken: token, inboundPackets: 5)
        XCTAssertEqual(fired, 0, "baseline packet must not fire")
        XCTAssertNotNil(arm)
        MediaProbeController.fireFreshMediaArm(&arm, currentToken: token, inboundPackets: 6)
        XCTAssertEqual(fired, 1, "later advance must fire exactly once")
        XCTAssertNil(arm, "a fired arm is consumed")
        MediaProbeController.fireFreshMediaArm(&arm, currentToken: token, inboundPackets: 7)
        XCTAssertEqual(fired, 1, "no double fire")
    }

    func testFreshMediaArmReArmRebaselinesAndStaleTokenNeverFires() {
        var arm: (baseline: UInt64, token: UInt64, callback: () -> Void)?
        var firstFired = 0
        var secondFired = 0
        arm = (baseline: 0, token: 1, callback: { firstFired += 1 })
        // Re-arm with a new baseline + token before any advance.
        arm = (baseline: 10, token: 2, callback: { secondFired += 1 })
        MediaProbeController.fireFreshMediaArm(&arm, currentToken: 2, inboundPackets: 10)
        XCTAssertEqual(firstFired, 0)
        XCTAssertEqual(secondFired, 0, "new baseline must not fire on the old counter")
        MediaProbeController.fireFreshMediaArm(&arm, currentToken: 2, inboundPackets: 11)
        XCTAssertEqual(firstFired, 0, "the stale arm's callback is gone with the re-arm")
        XCTAssertEqual(secondFired, 1)
    }
}
