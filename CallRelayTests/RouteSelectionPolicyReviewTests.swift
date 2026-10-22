import XCTest
@testable import CallRelay

/// Root-critical-review fixes for the build-61 route-selection policy:
/// defect 5 (two-phase proposal/confirm), defect 6 (distinct-sample streaks
/// + sustained duration), defect 7 (numerical hygiene + forced fencing),
/// defect 8 (voice-quality loss model), defect 9 (missing/stale stats never
/// hard-fail the incumbent). Every scenario injects the clock — the policy
/// never reads a hidden `Date()`.
@MainActor
final class RouteSelectionPolicyReviewTests: XCTestCase {

    // MARK: Fixtures

    private let base = Date(timeIntervalSince1970: 1_500_000)
    private func t(_ seconds: TimeInterval) -> Date { base.addingTimeInterval(seconds) }
    private var nextSampleID: UInt64 = 0

    private func sample(rtt: Double, jitter: Double = 2, loss: Double = 0,
                        available: Bool = true, confidence: Double = 0.9,
                        mediaReady: Bool = true, epoch: String = "e1",
                        at time: TimeInterval,
                        id: UInt64? = nil) -> RouteCandidateSample {
        nextSampleID += 1
        return RouteCandidateSample(rttMs: rtt, jitterMs: jitter, lossFraction: loss,
                                    reorderFraction: 0, queueCostMs: 0,
                                    available: available, confidence: confidence,
                                    mediaReady: mediaReady, networkEpoch: epoch,
                                    measuredAt: t(time), sampleID: id ?? nextSampleID)
    }

    private func fastConfig() -> RouteSelectionPolicy.Config {
        var config = RouteSelectionPolicy.Config.default
        config.minimumHoldInterval = 4
        config.switchCooldown = 2
        config.sustainedWindowSpan = 30
        config.sustainedMinDuration = 2
        return config
    }

    @discardableResult
    private func evaluateAndConfirm(_ policy: inout RouteSelectionPolicy,
                                    now: Date,
                                    samples: [RouteCandidateKind: RouteCandidateSample],
                                    forced: RouteCandidateKind? = nil) -> RouteSelectionDecision {
        let decision = policy.evaluate(now: now, samples: samples, forced: forced)
        if let proposed = decision.proposed {
            XCTAssertTrue(policy.confirmSwitch(to: proposed,
                                               proposalID: decision.proposalID,
                                               at: now))
        }
        return decision
    }

    // MARK: Defect 5 / fix 2 — two-phase proposal/confirm with pending serialization

    func testPendingSwitchSurvivesProbeTicksAndConfirmsOnce() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
        ])
        for time in [4.0, 5.0] {
            _ = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ], forced: nil)
        }
        let proposal = policy.evaluate(now: t(6), samples: [
            .quicDirect: sample(rtt: 20, at: 6),
            .wssRelay: sample(rtt: 100, at: 6),
        ], forced: nil)
        XCTAssertEqual(proposal.proposed, .quicDirect)
        // Probe ticks while the coordinator's commit is in flight: keep the
        // carrier, keep the SAME confirmable proposalID (fix 2).
        for time in [7.0, 8.0] {
            let tick = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ], forced: nil)
            XCTAssertEqual(tick.reason, .switchPending, "tick @\(time)")
            XCTAssertEqual(tick.chosen, .wssRelay)
            XCTAssertNil(tick.proposed)
            XCTAssertEqual(tick.proposalID, proposal.proposalID)
        }
        XCTAssertEqual(policy.incumbentKind, .wssRelay, "still not adopted")
        // The delayed ACK confirms the ORIGINAL proposal exactly once.
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: proposal.proposalID,
                                           at: t(9)))
        XCTAssertEqual(policy.incumbentKind, .quicDirect)
        XCTAssertEqual(policy.incumbentSince, t(9))
        XCTAssertFalse(policy.confirmSwitch(to: .quicDirect,
                                            proposalID: proposal.proposalID,
                                            at: t(9)))
    }

    func testCancelPendingSwitchLeavesIncumbentWithoutPenalty() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
        ])
        for time in [4.0, 5.0] {
            _ = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ], forced: nil)
        }
        let proposal = policy.evaluate(now: t(6), samples: [
            .quicDirect: sample(rtt: 20, at: 6),
            .wssRelay: sample(rtt: 100, at: 6),
        ], forced: nil)
        XCTAssertEqual(proposal.proposed, .quicDirect)
        // The commit failed and rolled back: cancel the in-flight switch —
        // no adoption, no cooldown/min-hold side effects (fix 2).
        policy.cancelPendingSwitch(reason: "route commit failed")
        XCTAssertEqual(policy.lastPendingCancelReason, "route commit failed")
        XCTAssertFalse(policy.confirmSwitch(to: .quicDirect,
                                            proposalID: proposal.proposalID,
                                            at: t(7)),
                       "a cancelled proposal can never confirm")
        // The real incumbent is in place with NO penalty: the next
        // evaluation may propose again immediately.
        let retry = policy.evaluate(now: t(7), samples: [
            .quicDirect: sample(rtt: 20, at: 7),
            .wssRelay: sample(rtt: 100, at: 7),
        ], forced: nil)
        XCTAssertEqual(retry.proposed, .quicDirect, "immediate re-proposal after cancel")
        XCTAssertNotEqual(retry.proposalID, proposal.proposalID)
        XCTAssertEqual(policy.incumbentKind, .wssRelay)
        XCTAssertEqual(policy.incumbentSince, t(0),
                       "dwell clock untouched by the failed switch")
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: retry.proposalID,
                                           at: t(7)))
        XCTAssertEqual(policy.incumbentKind, .quicDirect)
    }

    func testPendingExpiryReallowsFreshProposal() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
        ])
        for time in [4.0, 5.0] {
            _ = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ], forced: nil)
        }
        let proposal = policy.evaluate(now: t(6), samples: [
            .quicDirect: sample(rtt: 20, at: 6),
            .wssRelay: sample(rtt: 100, at: 6),
        ], forced: nil)
        let tick = policy.evaluate(now: t(7), samples: [
            .quicDirect: sample(rtt: 20, at: 7),
            .wssRelay: sample(rtt: 100, at: 7),
        ], forced: nil)
        XCTAssertEqual(tick.reason, .switchPending)
        // t15 is beyond proposalExpiryInterval (8s) from the t6 proposal:
        // the pending gate opens and a FRESH proposal may be issued.
        let expired = policy.evaluate(now: t(15), samples: [
            .quicDirect: sample(rtt: 20, at: 15),
            .wssRelay: sample(rtt: 100, at: 15),
        ], forced: nil)
        XCTAssertEqual(expired.reason, .sustainedGain)
        XCTAssertEqual(expired.proposed, .quicDirect)
        XCTAssertNotEqual(expired.proposalID, proposal.proposalID)
    }

    func testHardFailureBypassesPendingProposal() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
        ])
        for time in [4.0, 5.0] {
            _ = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ], forced: nil)
        }
        let proposal = policy.evaluate(now: t(6), samples: [
            .quicDirect: sample(rtt: 20, at: 6),
            .wssRelay: sample(rtt: 100, at: 6),
        ], forced: nil)
        XCTAssertEqual(proposal.proposed, .quicDirect)
        // Real liveness death wins immediately: the hard-failure fast path
        // supersedes the pending proposal with a new one (fix 2).
        policy.noteHardFailure(.wssRelay, at: t(7))
        let decision = policy.evaluate(now: t(7), samples: [
            .quicDirect: sample(rtt: 20, at: 7),
            .wssRelay: sample(rtt: 100, at: 7),
        ], forced: nil)
        XCTAssertEqual(decision.reason, .hardFailure)
        XCTAssertEqual(decision.proposed, .quicDirect)
        XCTAssertNotEqual(decision.proposalID, proposal.proposalID,
                          "hard failure supersedes the pending proposal")
        XCTAssertFalse(policy.confirmSwitch(to: .quicDirect,
                                            proposalID: proposal.proposalID,
                                            at: t(7)))
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: decision.proposalID,
                                           at: t(7)))
        XCTAssertEqual(policy.incumbentKind, .quicDirect)
    }

    func testUnconfirmedProposalStaysPendingInsteadOfSuperseded() {
        var policy = RouteSelectionPolicy()
        let first = policy.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
            .webRTCDirect: sample(rtt: 60, at: 0),
        ], forced: nil)
        // A probe tick while the initial pick is uncommitted: the SAME
        // proposalID comes back as .switchPending instead of a new proposal,
        // and the wrong kind still cannot confirm.
        let second = policy.evaluate(now: t(1), samples: [
            .quicDirect: sample(rtt: 40, at: 1),
            .webRTCDirect: sample(rtt: 60, at: 1),
        ], forced: nil)
        XCTAssertEqual(second.proposalID, first.proposalID)
        XCTAssertEqual(second.reason, .switchPending)
        XCTAssertEqual(second.chosen, .quicDirect,
                       "the in-flight pick stays the declared carrier")
        XCTAssertNil(second.proposed)
        XCTAssertNil(policy.incumbentKind)
        XCTAssertFalse(policy.confirmSwitch(to: .webRTCDirect,
                                            proposalID: first.proposalID,
                                            at: t(1)),
                       "the confirmed kind must match the proposal's target")
        XCTAssertNil(policy.incumbentKind)
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: first.proposalID,
                                           at: t(1)))
        XCTAssertEqual(policy.incumbentKind, .quicDirect)
    }

    func testExpiredProposalIsRejected() {
        var policy = RouteSelectionPolicy()
        let decision = policy.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
        ], forced: nil)
        XCTAssertFalse(policy.confirmSwitch(to: .quicDirect,
                                            proposalID: decision.proposalID,
                                            at: t(8.5)),
                       "a proposal older than proposalExpiryInterval (8s) is late")
        XCTAssertNil(policy.incumbentKind)
        // Boundary: exactly at expiry the proposal is still confirmable.
        var boundary = RouteSelectionPolicy()
        let fresh = boundary.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
        ], forced: nil)
        XCTAssertTrue(boundary.confirmSwitch(to: .quicDirect,
                                             proposalID: fresh.proposalID,
                                             at: t(8)))
    }

    func testConfirmingKeepDecisionsIsANoOpSuccess() {
        var policy = RouteSelectionPolicy()
        let initial = policy.evaluate(now: t(0), samples: [
            .wssRelay: sample(rtt: 85, at: 0),
        ], forced: nil)
        XCTAssertTrue(policy.confirmSwitch(to: .wssRelay,
                                           proposalID: initial.proposalID,
                                           at: t(0)))
        let keep = policy.evaluate(now: t(1), samples: [
            .wssRelay: sample(rtt: 85, at: 1),
        ], forced: nil)
        XCTAssertNil(keep.proposed)
        XCTAssertTrue(policy.confirmSwitch(to: .wssRelay,
                                           proposalID: keep.proposalID,
                                           at: t(1)),
                      "confirming a keep decision is a no-op success")
        XCTAssertEqual(policy.incumbentSince, t(0),
                       "no-op confirms must not move the dwell clock")
        XCTAssertFalse(policy.confirmSwitch(to: .wssRelay,
                                            proposalID: keep.proposalID,
                                            at: t(1)),
                       "exactly-once applies to keep decisions too")
    }

    // MARK: Defect 6 — distinct samples + sustained duration

    func testSameSampleReevaluatedProducesZeroStreakGrowth() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
        ])
        // One and the SAME measurement (id 42) re-fed three times.
        var repeated = RouteSelectionDecision(chosen: .wssRelay,
                                              reason: .keptIncumbent,
                                              detail: "")
        for time in [4.0, 5.0, 6.0] {
            repeated = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time, id: 42),
                .wssRelay: sample(rtt: 100, at: time),
            ], forced: nil)
        }
        XCTAssertEqual(repeated.chosen, .wssRelay)
        XCTAssertNil(repeated.proposed, "one measurement is not sustained evidence")
        XCTAssertTrue(repeated.detail.contains("1/3"), repeated.detail)
        // A genuinely NEW measurement resumes growth.
        let fresh = policy.evaluate(now: t(7), samples: [
            .quicDirect: sample(rtt: 20, at: 7, id: 43),
            .wssRelay: sample(rtt: 100, at: 7),
        ], forced: nil)
        XCTAssertTrue(fresh.detail.contains("2/3"), fresh.detail)
    }

    func testStreakRequiresSustainedDurationNotJustCount() {
        var config = fastConfig()
        config.sustainedMinDuration = 4   // the production default
        var policy = RouteSelectionPolicy(config: config)
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
        ])
        // Five DISTINCT gains inside 2s: the window COUNT is satisfied but
        // the streak's first→last span (2s) is below sustainedMinDuration.
        var decision = RouteSelectionDecision(chosen: .wssRelay,
                                              reason: .keptIncumbent,
                                              detail: "")
        for time in [4.0, 4.5, 5.0, 5.5, 6.0] {
            decision = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ], forced: nil)
        }
        XCTAssertEqual(decision.chosen, .wssRelay,
                       "count without 4s of sustained gains is not enough")
        XCTAssertNil(decision.proposed)
        // The sixth distinct gain crosses the 4s span → the switch proposes.
        let crossed = policy.evaluate(now: t(8), samples: [
            .quicDirect: sample(rtt: 20, at: 8),
            .wssRelay: sample(rtt: 100, at: 8),
        ], forced: nil)
        XCTAssertEqual(crossed.chosen, .quicDirect)
        XCTAssertEqual(crossed.proposed, .quicDirect)
        XCTAssertEqual(crossed.reason, .sustainedGain)
    }

    func testEpochChangeResetsStreakAndRequiresFreshContext() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
        ])
        var gain2 = RouteSelectionDecision(chosen: .wssRelay,
                                           reason: .keptIncumbent,
                                           detail: "")
        for time in [4.0, 5.0] {
            gain2 = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ], forced: nil)
        }
        XCTAssertTrue(gain2.detail.contains("2/3"), gain2.detail)
        // Epoch flip: the streak is fenced even though the new sample wins —
        // growth restarts from 1 in the NEW connection context.
        let fenced = policy.evaluate(now: t(6), samples: [
            .quicDirect: sample(rtt: 20, epoch: "e2", at: 6),
            .wssRelay: sample(rtt: 100, at: 6),
        ], forced: nil)
        XCTAssertTrue(fenced.detail.contains("1/3"), fenced.detail)
        XCTAssertEqual(fenced.reason, .epochChange)
    }

    func testConfirmedSwitchBumpsPolicyGeneration() {
        var policy = RouteSelectionPolicy()
        XCTAssertEqual(policy.policyGeneration, 0)
        let initial = policy.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
        ], forced: nil)
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: initial.proposalID,
                                           at: t(0)))
        XCTAssertEqual(policy.policyGeneration, 1)
        // Keep decisions and rejected confirms never bump it.
        let keep = policy.evaluate(now: t(1), samples: [
            .quicDirect: sample(rtt: 40, at: 1),
        ], forced: nil)
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: keep.proposalID,
                                           at: t(1)))
        XCTAssertEqual(policy.policyGeneration, 1)
    }

    // MARK: Defect 7 — numerical hygiene

    func testNonFiniteMetricsAreRejectedAndKeepPriorEvidence() {
        var policy = RouteSelectionPolicy()
        _ = policy.evaluate(now: t(0), samples: [
            .wssRelay: sample(rtt: 85, at: 0),
        ], forced: nil)
        policy.noteSample(.quicDirect, sample(rtt: 40, at: 1), now: t(1))
        let nanSample = RouteCandidateSample(rttMs: .nan, jitterMs: 2, lossFraction: 0,
                                             reorderFraction: 0, queueCostMs: 0,
                                             available: true, confidence: 0.9,
                                             mediaReady: true, networkEpoch: "e1",
                                             measuredAt: t(2), sampleID: 500)
        let infSample = RouteCandidateSample(rttMs: .infinity, jitterMs: 2, lossFraction: 0,
                                             reorderFraction: 0, queueCostMs: 0,
                                             available: true, confidence: 0.9,
                                             mediaReady: true, networkEpoch: "e1",
                                             measuredAt: t(2), sampleID: 501)
        XCTAssertEqual(policy.validate(nanSample, now: t(2)), .invalid(.nonFinite))
        XCTAssertEqual(policy.validate(infSample, now: t(2)), .invalid(.nonFinite))
        // A NaN sample must not evict the prior valid evidence: the forced
        // fence still sees the healthy t1 measurement.
        policy.noteSample(.quicDirect, nanSample, now: t(2))
        let forced = policy.evaluate(now: t(2), samples: [
            .quicDirect: nanSample,
            .wssRelay: sample(rtt: 85, at: 2),
        ], forced: .quicDirect)
        XCTAssertEqual(forced.chosen, .quicDirect)
        XCTAssertEqual(forced.reason, .forcedMode,
                       "prior valid evidence is kept; the invalid sample is ignored")
    }

    func testNegativeOutOfRangeAndFutureSkewSamplesAreRejected() {
        let policy = RouteSelectionPolicy()
        func s(rtt: Double = 40, jitter: Double = 2, loss: Double = 0,
               reorder: Double = 0, queue: Double = 0, confidence: Double = 0.9,
               at time: TimeInterval, id: UInt64) -> RouteCandidateSample {
            RouteCandidateSample(rttMs: rtt, jitterMs: jitter, lossFraction: loss,
                                 reorderFraction: reorder, queueCostMs: queue,
                                 available: true, confidence: confidence,
                                 mediaReady: true, networkEpoch: "e1",
                                 measuredAt: t(time), sampleID: id)
        }
        XCTAssertEqual(policy.validate(s(rtt: -1, at: 0, id: 1), now: t(0)),
                       .invalid(.negative))
        XCTAssertEqual(policy.validate(s(jitter: -0.5, at: 0, id: 2), now: t(0)),
                       .invalid(.negative))
        XCTAssertEqual(policy.validate(s(queue: -3, at: 0, id: 3), now: t(0)),
                       .invalid(.negative))
        XCTAssertEqual(policy.validate(s(loss: 1.5, at: 0, id: 4), now: t(0)),
                       .invalid(.outOfRange))
        XCTAssertEqual(policy.validate(s(loss: -0.01, at: 0, id: 5), now: t(0)),
                       .invalid(.outOfRange))
        XCTAssertEqual(policy.validate(s(reorder: 1.2, at: 0, id: 6), now: t(0)),
                       .invalid(.outOfRange))
        XCTAssertEqual(policy.validate(s(confidence: 1.2, at: 0, id: 7), now: t(0)),
                       .invalid(.outOfRange))
        XCTAssertEqual(policy.validate(s(loss: .nan, at: 0, id: 8), now: t(0)),
                       .invalid(.nonFinite))
        // Future skew: up to maxFutureSkew (5s) is tolerated, beyond is not.
        XCTAssertEqual(policy.validate(s(at: 5, id: 9), now: t(0)), .valid)
        XCTAssertEqual(policy.validate(s(at: 5.0001, id: 10), now: t(0)),
                       .invalid(.futureSkew))
        XCTAssertEqual(policy.validate(s(at: 10, id: 11), now: t(0)),
                       .invalid(.futureSkew))
    }

    func testBoundedFutureSkewSampleIsUsable() {
        var policy = RouteSelectionPolicy()
        let skewed = RouteCandidateSample(rttMs: 40, jitterMs: 2, lossFraction: 0,
                                          reorderFraction: 0, queueCostMs: 0,
                                          available: true, confidence: 0.9,
                                          mediaReady: true, networkEpoch: "e1",
                                          measuredAt: t(3), sampleID: 1)
        XCTAssertEqual(policy.validate(skewed, now: t(0)), .valid)
        let decision = policy.evaluate(now: t(0), samples: [
            .quicDirect: skewed,
            .wssRelay: sample(rtt: 85, at: 0, id: 2),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .quicDirect,
                       "a bounded future-skewed sample is usable evidence")
        XCTAssertEqual(decision.reason, .initialBest)
    }

    func testForcedCandidateWithStaleMediaIsRejected() {
        var policy = RouteSelectionPolicy()
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 85, at: 0),
        ])
        let stale = policy.evaluate(now: t(13), samples: [
            .quicDirect: sample(rtt: 40, at: 2),   // 11s old > TTL 10
            .wssRelay: sample(rtt: 85, at: 13),
        ], forced: .quicDirect)
        XCTAssertEqual(stale.chosen, .wssRelay)
        XCTAssertEqual(stale.reason, .forcedUnavailable,
                       "a forced-but-dead candidate is never promoted")
        XCTAssertNil(stale.proposed)
    }

    // MARK: Defect 8 — voice-quality loss model

    func testFullEvidenceTenPercentLossLosesToCleanSlowPath() {
        // The exact defect headline: 20ms + 10% loss at FULL evidence =
        // 140ms-equivalent — it must lose to a 100ms clean path.
        var policy = RouteSelectionPolicy()
        for index in 0..<8 {
            policy.noteSample(.quicDirect,
                              sample(rtt: 20, loss: 0.10, at: TimeInterval(index)),
                              now: t(TimeInterval(index)))
        }
        let decision = policy.evaluate(now: t(8), samples: [
            .quicDirect: sample(rtt: 20, loss: 0.10, at: 8),
            .webRTCDirect: sample(rtt: 100, at: 8),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .webRTCDirect)
        XCTAssertEqual(decision.reason, .initialBest)
    }

    func testLowEvidenceLossTermIsScaled() {
        // One sample with 10% loss is 1/8 evidence: the loss term scales to
        // 15ms-equivalent (20+4+15 = 39 < 104), so the fast path still wins.
        var policy = RouteSelectionPolicy()
        let decision = policy.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 20, loss: 0.10, at: 0),
            .wssRelay: sample(rtt: 100, at: 0),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .quicDirect)
        XCTAssertEqual(decision.reason, .initialBest)
    }

    func testFullEvidenceLossGateBlocksPromotionWhileIncumbentClean() {
        var config = fastConfig()
        config.sustainedWindow = 4
        var policy = RouteSelectionPolicy(config: config)
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),   // clean incumbent
        ])
        // QUIC: 10ms RTT + 6% loss. At full evidence its score (86) beats the
        // relay's 104 through the hysteresis gate — but 6% > 5% with full
        // loss confidence marks it DEGRADED, and the incumbent is clean.
        var decision = RouteSelectionDecision(chosen: .wssRelay,
                                              reason: .keptIncumbent,
                                              detail: "")
        for time in [4.0, 5.0, 6.0, 7.0, 8.0] {
            decision = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 10, loss: 0.06, at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ], forced: nil)
        }
        XCTAssertEqual(decision.chosen, .wssRelay,
                       "a degraded challenger must not beat a clean incumbent")
        XCTAssertNil(decision.proposed)
        XCTAssertEqual(policy.incumbentKind, .wssRelay)
        XCTAssertTrue(decision.detail.contains("丢包受限"), decision.detail)
        XCTAssertTrue(decision.detail.contains("含丢包代价（估计）"), decision.detail)
    }

    func testAllDegradedCandidatesFallBackToLeastBadScore() {
        var config = fastConfig()
        config.sustainedWindow = 4
        var policy = RouteSelectionPolicy(config: config)
        // BOTH paths degraded (relay 8% loss, QUIC 6%): the gate lifts and
        // the least-bad score wins.
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, loss: 0.08, at: 0),
        ])
        var decision = RouteSelectionDecision(chosen: .wssRelay,
                                              reason: .keptIncumbent,
                                              detail: "")
        for time in [4.0, 5.0, 6.0, 7.0] {
            decision = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 10, loss: 0.06, at: time),
                .wssRelay: sample(rtt: 100, loss: 0.08, at: time),
            ], forced: nil)
        }
        XCTAssertEqual(decision.chosen, .quicDirect,
                       "all-degraded: the least-bad score still wins")
        XCTAssertEqual(decision.proposed, .quicDirect)
        XCTAssertEqual(decision.reason, .sustainedGain)
    }

    // MARK: Defect 9 / fix 1 — missing or aged stats never hard-fail

    func testAllCandidatesMissingEvidenceKeepsIncumbentInsufficient() {
        var policy = RouteSelectionPolicy()
        let initial = policy.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
        ], forced: nil)
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: initial.proposalID,
                                           at: t(0)))
        // Total telemetry silence once the incumbent's own evidence has aged
        // beyond the freshness TTL (t15): keep the incumbent and say so —
        // never a hard-failure cascade.
        let decision = policy.evaluate(now: t(15), samples: [:], forced: nil)
        XCTAssertEqual(decision.chosen, .quicDirect)
        XCTAssertEqual(decision.reason, .insufficientEvidence)
        XCTAssertNil(decision.proposed)
        XCTAssertTrue(decision.detail.contains("证据不足"), decision.detail)
    }

    func testStaleStatsBeyondOldHardStaleThresholdKeepIncumbent() {
        var policy = RouteSelectionPolicy()
        let initial = policy.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, mediaReady: true, at: 0),
        ], forced: nil)
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: initial.proposalID,
                                           at: t(0)))
        // 25s of total silence — far beyond the DELETED 2×TTL hard-stale
        // rule — with no fresh proof anywhere: the healthy incumbent stays
        // (fix 1 regression), explicitly NOT via the failure path.
        let decision = policy.evaluate(now: t(25), samples: [:], forced: nil)
        XCTAssertEqual(decision.chosen, .quicDirect)
        XCTAssertNotEqual(decision.reason, .hardFailure)
        XCTAssertNil(decision.proposed, "aged stats never propose a failover")
    }

    func testAgedEvidenceNeverHardFailsOnlyExplicitSignalsDo() {
        var policy = RouteSelectionPolicy()
        let initial = policy.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
            .wssRelay: sample(rtt: 85, at: 0),
        ], forced: nil)
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: initial.proposalID,
                                           at: t(0)))
        // t15: QUIC's only evidence is 15s old — beyond the 10s freshness
        // TTL: the incumbent is KEPT (staleness is not failure).
        let aging = policy.evaluate(now: t(15), samples: [
            .wssRelay: sample(rtt: 85, at: 15),
        ], forced: nil)
        XCTAssertEqual(aging.chosen, .quicDirect)
        XCTAssertEqual(aging.reason, .keptIncumbent)
        XCTAssertNotEqual(aging.reason, .hardFailure)
        // t21: the evidence is 21s old — far beyond the DELETED 2×TTL rule.
        // Still not a failure: keep, no proposal (fix 1).
        let aged = policy.evaluate(now: t(21), samples: [
            .wssRelay: sample(rtt: 85, at: 21),
        ], forced: nil)
        XCTAssertEqual(aged.chosen, .quicDirect)
        XCTAssertNotEqual(aged.reason, .hardFailure)
        XCTAssertNil(aged.proposed)
        // Only an EXPLICIT transport failure signal moves the route now.
        policy.noteHardFailure(.quicDirect, at: t(22))
        let failed = policy.evaluate(now: t(22), samples: [
            .wssRelay: sample(rtt: 85, at: 22),
        ], forced: nil)
        XCTAssertEqual(failed.chosen, .wssRelay)
        XCTAssertEqual(failed.proposed, .wssRelay)
        XCTAssertEqual(failed.reason, .hardFailure)
    }

    // MARK: Fix 3 — evidence integrity

    func testOutOfOrderOlderSampleDoesNotRegressFreshness() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 120, at: 0),
        ])
        // Newest QUIC evidence: rtt 20 measured at t9 (epoch e1).
        policy.noteSample(.quicDirect, sample(rtt: 20, at: 9), now: t(9))
        // An OLDER sample (measured t5) arrives late: it must not regress
        // the stored freshness anchor. It still counts as distinct history
        // because it belongs to the current epoch.
        policy.noteSample(.quicDirect, sample(rtt: 20, at: 5), now: t(10))
        XCTAssertEqual(policy.distinctSampleCounts[.quicDirect], 2)
        // t19: the t9 evidence is exactly TTL-fresh (age 10). If the t5
        // sample had evicted it, the anchor would be 14s old (stale). The
        // forced fence reads the evidence cache: usable only if freshness
        // was not regressed.
        let decision = policy.evaluate(now: t(19), samples: [
            .wssRelay: sample(rtt: 120, at: 19),
        ], forced: .quicDirect)
        XCTAssertEqual(decision.chosen, .quicDirect)
        XCTAssertEqual(decision.reason, .forcedMode,
                       "decisions still use the t9 context, not the regressed t5 sample")
    }

    func testFailureFenceSurvivesStaleEpochSample() {
        var policy = RouteSelectionPolicy()
        let initial = policy.evaluate(now: t(1), samples: [
            .quicDirect: sample(rtt: 40, at: 1),
            .wssRelay: sample(rtt: 85, at: 1),
        ], forced: nil)
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: initial.proposalID,
                                           at: t(1)))
        policy.noteHardFailure(.quicDirect, at: t(12))
        // A sample from a NEW epoch whose timestamp PREDATES the failure: it
        // seeds the new context (streaks reset) but must NOT lift the
        // failure fence (fix 3). Without the surviving mark the incumbent
        // would simply be kept; the fast-path proposal proves the fence held.
        let decision = policy.evaluate(now: t(13), samples: [
            .quicDirect: sample(rtt: 40, epoch: "e2", at: 11),
            .wssRelay: sample(rtt: 85, at: 13),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .wssRelay)
        XCTAssertEqual(decision.proposed, .wssRelay,
                       "the failure fence survived the stale-epoch sample")
        XCTAssertNotEqual(decision.reason, .keptIncumbent)
        XCTAssertEqual(decision.reason, .epochChange,
                       "the incumbent's own epoch also changed this evaluation")
        // Normal recovery still works in the new context: post-failure
        // healthy evidence re-enters through the symmetric switch rule.
        _ = policy.confirmSwitch(to: .wssRelay, proposalID: decision.proposalID, at: t(13))
        let recovered = policy.evaluate(now: t(14), samples: [
            .quicDirect: sample(rtt: 40, epoch: "e2", at: 13),
            .wssRelay: sample(rtt: 85, at: 14),
        ], forced: nil)
        XCTAssertEqual(recovered.chosen, .wssRelay)
        XCTAssertTrue(recovered.detail.contains("1/3"), recovered.detail)
    }

    func testDistinctSampleHistoryIsBoundedFIFO() {
        var policy = RouteSelectionPolicy()
        for index in 0..<40 {
            policy.noteSample(.quicDirect,
                              sample(rtt: 40, at: TimeInterval(index)),
                              now: t(TimeInterval(index)))
        }
        XCTAssertEqual(policy.distinctSampleCounts[.quicDirect], 32,
                       "history is a rolling window: oldest evicted beyond 32")
        // The bound does not affect current evidence: a fresh sample keeps
        // the candidate fully usable.
        policy.noteSample(.quicDirect, sample(rtt: 40, at: 45), now: t(45))
        XCTAssertEqual(policy.distinctSampleCounts[.quicDirect], 32)
        let decision = policy.evaluate(now: t(45), samples: [
            .quicDirect: sample(rtt: 40, at: 45),
            .wssRelay: sample(rtt: 85, at: 45),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .quicDirect,
                       "bounded history does not break freshness")
        XCTAssertEqual(decision.reason, .initialBest)
    }

    // MARK: Fix 4 — evaluate scores only intake-accepted current-context evidence

    func testRetiredEpochSamplesAreRejectedWhileCurrentContextChallengerGrows() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
        ])
        // QUIC's OLD era: epoch e0 establishes the lineage.
        policy.noteSample(.quicDirect, sample(rtt: 20, epoch: "e0", at: 1), now: t(1))
        // t4: QUIC transitions to e1 — a GENUINE new context (e0 retires).
        // The transition itself counts (existing epoch-fence contract).
        let transition = policy.evaluate(now: t(4), samples: [
            .quicDirect: sample(rtt: 20, epoch: "e1", at: 4),
            .wssRelay: sample(rtt: 100, at: 4),
        ], forced: nil)
        XCTAssertEqual(transition.chosen, .wssRelay)
        XCTAssertTrue(transition.detail.contains("1/3"), transition.detail)
        // t5–t7: three late callbacks from the RETIRED e0 era, interleaved
        // with a current-context webRTC challenger at the same low RTT.
        // The retired samples grow NOTHING; the current-context challenger
        // grows normally — the gate is context-based, not a blanket block.
        let rounds: [(time: TimeInterval, includeWebRTC: Bool)] = [(5, true), (6, true), (7, false)]
        for (time, includeWebRTC) in rounds {
            var samples: [RouteCandidateKind: RouteCandidateSample] = [
                .quicDirect: sample(rtt: 20, epoch: "e0", at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ]
            if includeWebRTC {
                samples[.webRTCDirect] = sample(rtt: 20, epoch: "e1", at: time)
            }
            let decision = policy.evaluate(now: t(time), samples: samples, forced: nil)
            XCTAssertEqual(decision.chosen, .wssRelay, "round @\(time)")
            XCTAssertNil(decision.proposed, "round @\(time)")
            if time == 5 {
                XCTAssertTrue(decision.detail.contains("1/3"), decision.detail)
            } else if time == 6 {
                XCTAssertTrue(decision.detail.contains("2/3"), decision.detail)
            } else {
                XCTAssertFalse(decision.detail.contains("3/3"), decision.detail)
                XCTAssertTrue(decision.detail.contains("无更优可用路径"), decision.detail)
            }
        }
        // A current-context quic sample restarts from 1: the three retired
        // rounds produced zero net gain growth.
        let restart = policy.evaluate(now: t(8), samples: [
            .quicDirect: sample(rtt: 20, epoch: "e1", at: 8),
            .wssRelay: sample(rtt: 100, at: 8),
        ], forced: nil)
        XCTAssertTrue(restart.detail.contains("1/3"), restart.detail)
        XCTAssertNil(restart.proposed)
        // Control: the current-context challenger completes its streak and
        // is proposed through the normal sustained-gain rule.
        var proposal = RouteSelectionDecision(chosen: .wssRelay, reason: .keptIncumbent, detail: "")
        for time in [9.0, 10.0, 11.0] {
            proposal = policy.evaluate(now: t(time), samples: [
                .webRTCDirect: sample(rtt: 20, epoch: "e1", at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ], forced: nil)
        }
        XCTAssertEqual(proposal.chosen, .webRTCDirect, "current-context control grows and wins")
        XCTAssertEqual(proposal.proposed, .webRTCDirect)
        XCTAssertEqual(proposal.reason, .sustainedGain)
        XCTAssertEqual(policy.incumbentKind, .wssRelay, "the retired samples never moved the route")
    }

    func testDelayedRetiredCallbackWithNewerTimestampIsRejected() {
        var policy = RouteSelectionPolicy()
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
        ])
        policy.noteSample(.quicDirect, sample(rtt: 20, epoch: "e0", at: 1), now: t(1))
        policy.noteSample(.quicDirect, sample(rtt: 20, epoch: "e1", at: 5), now: t(5))
        policy.noteSample(.quicDirect, sample(rtt: 20, epoch: "e1", at: 15), now: t(15))
        // Late callback from the retired e0 era — with a measuredAt NEWER
        // than the stored current anchor. The old measuredAt-based guard
        // cannot catch this; the retired-epoch gate must.
        policy.noteSample(.quicDirect, sample(rtt: 20, epoch: "e0", at: 18), now: t(20))
        // The e0→e1 transition reset the history (new context), so only the
        // two e1 anchors count. Had the retired callback been accepted the
        // count would be 3 — the bound at 2 is the rejection proof.
        XCTAssertEqual(policy.distinctSampleCounts[.quicDirect], 2,
                       "a rejected retired-context sample never enters the history")
        // t26: the true anchor (e1@t15) is 11s old (stale). Had the e0@t18
        // callback hijacked the anchor it would be 8s old (fresh) and able
        // to gain — the stale true evidence must not score.
        let decision = policy.evaluate(now: t(26), samples: [
            .wssRelay: sample(rtt: 100, at: 26),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .wssRelay)
        XCTAssertFalse(decision.detail.contains("1/3"), decision.detail)
    }

    func testFailureFenceSurvivesRetiredContextSample() {
        var policy = RouteSelectionPolicy()
        let initial = policy.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, epoch: "e0", at: 0),
            .wssRelay: sample(rtt: 85, at: 0),
        ], forced: nil)
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: initial.proposalID,
                                           at: t(0)))
        policy.noteSample(.quicDirect, sample(rtt: 40, epoch: "e1", at: 1), now: t(1))
        policy.noteHardFailure(.quicDirect, at: t(2))
        // Fresh, healthy sample from the RETIRED e0 era, measured AFTER the
        // failure: if it were accepted it would overwrite the current
        // context and clear the mark (it postdates the failure). It must be
        // rejected — the fence stands and the fast path fires.
        let decision = policy.evaluate(now: t(3), samples: [
            .quicDirect: sample(rtt: 40, epoch: "e0", at: 3),
            .wssRelay: sample(rtt: 85, at: 3),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .wssRelay)
        XCTAssertEqual(decision.proposed, .wssRelay,
                       "the rejected retired sample neither cleared the fence nor carried the route")
        XCTAssertEqual(decision.reason, .hardFailure,
                       "no epochChanged this round — the retired sample fired no fence")
    }

    // MARK: Defect 9 — quiet incumbent

    func testQuietIncumbentWithSlowerChallengerKeepsRoute() {
        var policy = RouteSelectionPolicy()
        let initial = policy.evaluate(now: t(0), samples: [
            .wssRelay: sample(rtt: 40, at: 0),
        ], forced: nil)
        XCTAssertTrue(policy.confirmSwitch(to: .wssRelay,
                                           proposalID: initial.proposalID,
                                           at: t(0)))
        // The relay stops sending pongs (no sample), while the only fresh
        // challenger is materially SLOWER: silence is not failure, and slow
        // is not better — the incumbent stays.
        let decision = policy.evaluate(now: t(8), samples: [
            .quicDirect: sample(rtt: 120, at: 8),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .wssRelay)
        XCTAssertEqual(decision.reason, .keptIncumbent)
    }
}
