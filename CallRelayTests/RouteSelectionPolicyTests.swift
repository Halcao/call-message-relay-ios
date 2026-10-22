import XCTest
@testable import CallRelay

/// Build-61 deterministic multi-path route-selection policy tests. Every
/// scenario injects the clock (`now:`) and scripted samples — the policy
/// never reads a hidden `Date()`, so outcomes here are exact. All scenarios
/// run against the two-phase commit model: `evaluate` proposes, the
/// coordinator confirms via `confirmSwitch`.
@MainActor
final class RouteSelectionPolicyTests: XCTestCase {

    // MARK: Fixtures

    /// Deterministic epoch base; `t(4)` is "four seconds in".
    private let base = Date(timeIntervalSince1970: 1_000_000)
    private func t(_ seconds: TimeInterval) -> Date { base.addingTimeInterval(seconds) }

    /// Monotonic fixture ids: every built sample is a DISTINCT measurement
    /// unless a test overrides `id` (the distinct-sample streak rule is
    /// exercised explicitly in RouteSelectionPolicyReviewTests).
    private var nextSampleID: UInt64 = 0

    private func sample(rtt: Double, jitter: Double = 2, loss: Double = 0,
                        reorder: Double = 0, queue: Double = 0,
                        available: Bool = true, confidence: Double = 0.9,
                        mediaReady: Bool = true, epoch: String = "e1",
                        at time: TimeInterval,
                        id: UInt64? = nil) -> RouteCandidateSample {
        nextSampleID += 1
        return RouteCandidateSample(rttMs: rtt, jitterMs: jitter, lossFraction: loss,
                                    reorderFraction: reorder, queueCostMs: queue,
                                    available: available, confidence: confidence,
                                    mediaReady: mediaReady, networkEpoch: epoch,
                                    measuredAt: t(time), sampleID: id ?? nextSampleID)
    }

    /// Shortened anti-flap clocks so switch-timing scenarios stay readable;
    /// the rule set under test is identical to the defaults.
    private func fastConfig() -> RouteSelectionPolicy.Config {
        var config = RouteSelectionPolicy.Config.default
        config.minimumHoldInterval = 4
        config.switchCooldown = 2
        config.sustainedWindowSpan = 30
        config.sustainedMinDuration = 2
        return config
    }

    /// Phase-2 helper: evaluates and immediately confirms whatever the
    /// decision proposed (the coordinator's happy path).
    @discardableResult
    private func evaluateAndConfirm(_ policy: inout RouteSelectionPolicy,
                                    now: Date,
                                    samples: [RouteCandidateKind: RouteCandidateSample],
                                    forced: RouteCandidateKind? = nil) -> RouteSelectionDecision {
        let decision = policy.evaluate(now: now, samples: samples, forced: forced)
        if let proposed = decision.proposed {
            XCTAssertTrue(policy.confirmSwitch(to: proposed,
                                               proposalID: decision.proposalID,
                                               at: now),
                          "a fresh proposal must confirm exactly once")
        }
        return decision
    }

    // MARK: Score formula

    func testScoreFormulaPinsExactTerms() {
        let policy = RouteSelectionPolicy()
        // (rtt + 2×jitter + 1200×loss×scale + 120×reorder + queue)
        // / max(conf, 0.25); with full confidence the divisor is exactly 1.
        let lossy = sample(rtt: 50, jitter: 5, loss: 0.08, confidence: 1.0, at: 0)
        XCTAssertEqual(policy.effectiveScore(lossy), 156, accuracy: 0.0001)
        let clean = sample(rtt: 80, jitter: 8, confidence: 1.0, at: 0)
        XCTAssertEqual(policy.effectiveScore(clean), 96, accuracy: 0.0001)
        // Voice quality (defect 8): 10% loss costs 120ms-equivalent — a 20ms
        // path with 10% loss (140) must NOT beat a 100ms clean path.
        let lossyFast = sample(rtt: 20, jitter: 0, loss: 0.10, confidence: 1.0, at: 0)
        XCTAssertEqual(policy.effectiveScore(lossyFast), 140, accuracy: 0.0001)
        // Min-evidence scaling (defect 8): at 1/8 evidence the loss term is
        // 1/8 weight.
        XCTAssertEqual(policy.effectiveScore(lossyFast, lossScale: 0.125), 35, accuracy: 0.0001)
        // Unavailability dwarfs quality terms but stays finite.
        let dead = sample(rtt: 40, jitter: 0, available: false, confidence: 1.0, at: 0)
        XCTAssertEqual(policy.effectiveScore(dead), 10_040, accuracy: 0.0001)
        // The confidence floor (0.25) bounds the divisor.
        let unproven = sample(rtt: 40, jitter: 0, confidence: 0.2, at: 0)
        XCTAssertEqual(policy.effectiveScore(unproven), 160, accuracy: 0.0001)
        // The confidence divisor penalizes unproven-but-confident paths.
        let halfProven = sample(rtt: 40, jitter: 0, confidence: 0.5, at: 0)
        XCTAssertEqual(policy.effectiveScore(halfProven), 80, accuracy: 0.0001)
        // Queue/backlog ESTIMATE contributes 1:1 at the default weight.
        let queued = sample(rtt: 40, jitter: 0, queue: 12, confidence: 1.0, at: 0)
        XCTAssertEqual(policy.effectiveScore(queued), 52, accuracy: 0.0001)
    }

    func testDefaultConfigValues() {
        let config = RouteSelectionPolicy.Config.default
        XCTAssertEqual(config.hysteresisFraction, 0.15)
        XCTAssertEqual(config.minimumHoldInterval, 20)
        XCTAssertEqual(config.switchCooldown, 15)
        XCTAssertEqual(config.sampleFreshnessTTL, 10)
        XCTAssertEqual(config.sustainedWindow, 3)
        XCTAssertEqual(config.sustainedWindowSpan, 12)
        XCTAssertEqual(config.sustainedMinDuration, 4)
        XCTAssertEqual(config.maxFutureSkew, 5)
        XCTAssertEqual(config.proposalExpiryInterval, 8)
        XCTAssertEqual(config.minimumConfidence, 0.4)
        XCTAssertEqual(config.unavailablePenalty, 10_000)
        XCTAssertEqual(config.lossWeightPerPoint, 1200)
        XCTAssertEqual(config.lossGateFraction, 0.05)
        XCTAssertEqual(config.lossGateConfidence, 0.5)
        XCTAssertEqual(config.minSamplesForLossDecisions, 8)
    }

    // MARK: Quality comparison

    func testLowRTTLossyCandidateWinsWhenPenaltyMathSaysSo() {
        var policy = RouteSelectionPolicy()
        // 50ms + 2×5 + 1200×0.08×(1/8 证据) = 75  <  80 + 2×8 = 96.
        let decision = policy.evaluate(now: t(1), samples: [
            .quicDirect: sample(rtt: 50, jitter: 5, loss: 0.08, at: 1),
            .webRTCDirect: sample(rtt: 80, jitter: 8, at: 1),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .quicDirect)
        XCTAssertEqual(decision.reason, .initialBest)
    }

    func testHigherRTTCleanCandidateWinsWhenLossPenaltyDominates() {
        var policy = RouteSelectionPolicy()
        // 50ms + 2×5 + 1200×0.5×(1/8 证据) = 135  >  80 + 2×8 = 96: even a
        // single-sample extreme loss dominates — the clean path wins.
        let decision = policy.evaluate(now: t(1), samples: [
            .quicDirect: sample(rtt: 50, jitter: 5, loss: 0.5, at: 1),
            .webRTCDirect: sample(rtt: 80, jitter: 8, at: 1),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .webRTCDirect)
        XCTAssertEqual(decision.reason, .initialBest)
    }

    func testBurstThenRecoveryKeepsIncumbentWithoutFlapping() {
        var policy = RouteSelectionPolicy()
        var decisions: [RouteSelectionDecision] = []
        // t0: QUIC is the best proven path and becomes incumbent.
        decisions.append(evaluateAndConfirm(&policy, now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
            .wssRelay: sample(rtt: 85, at: 0),
        ]))
        // t1–t3: QUIC bursts (30% loss at reduced single-digit evidence) but
        // does NOT hard-fail; the relay would need a full streak + dwell,
        // and the recovery happens first.
        for time in [1.0, 2.0, 3.0] {
            decisions.append(policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 40, loss: 0.3, at: time),
                .wssRelay: sample(rtt: 85, at: time),
            ], forced: nil))
        }
        // t4: QUIC recovered (fresh post-burst sample).
        decisions.append(policy.evaluate(now: t(4), samples: [
            .quicDirect: sample(rtt: 40, at: 4),
            .wssRelay: sample(rtt: 85, at: 4),
        ], forced: nil))
        XCTAssertEqual(decisions.map(\.chosen),
                       [.quicDirect, .quicDirect, .quicDirect, .quicDirect, .quicDirect])
        XCTAssertEqual(decisions[1].reason, .keptIncumbent)
    }

    func testNearIdenticalPathsRetainIncumbent() {
        var policy = RouteSelectionPolicy()
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
        ])
        for time in 1...5 {
            let decision = policy.evaluate(now: t(TimeInterval(time)), samples: [
                .quicDirect: sample(rtt: 90, at: TimeInterval(time)),
                .wssRelay: sample(rtt: 100, at: TimeInterval(time)),
            ], forced: nil)
            // 94 is only ~10% better — below the 15% hysteresis: never switch.
            XCTAssertEqual(decision.chosen, .wssRelay)
            XCTAssertEqual(decision.reason, .keptIncumbent)
        }
    }

    // MARK: Failure handling

    func testHardFailureSwitchesImmediatelyBypassingDwellAndCooldown() {
        var policy = RouteSelectionPolicy()
        let initial = evaluateAndConfirm(&policy, now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
            .wssRelay: sample(rtt: 85, at: 0),
        ])
        XCTAssertEqual(initial.chosen, .quicDirect)
        policy.noteHardFailure(.quicDirect, at: t(1))
        let decision = policy.evaluate(now: t(1), samples: [
            .quicDirect: sample(rtt: 40, at: 1),   // pre-failure-instant data
            .wssRelay: sample(rtt: 85, at: 1),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .wssRelay)
        XCTAssertEqual(decision.proposed, .wssRelay)
        XCTAssertEqual(decision.reason, .hardFailure)
        XCTAssertEqual(policy.incumbentKind, .quicDirect,
                       "a proposal must not adopt before confirmSwitch")
        XCTAssertTrue(policy.confirmSwitch(to: .wssRelay,
                                           proposalID: decision.proposalID,
                                           at: t(1)))
        XCTAssertEqual(policy.incumbentKind, .wssRelay)
    }

    func testRepeatedFlappingIsBoundedByCooldownAndMinHold() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        var switches = 0
        var lastChosen: RouteCandidateKind?
        func step(_ time: TimeInterval, quicHealthy: Bool, fail: Bool = false) {
            if fail { policy.noteHardFailure(.quicDirect, at: t(time)) }
            let decision = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 40, available: quicHealthy, at: time),
                .wssRelay: sample(rtt: 85, at: time),
            ], forced: nil)
            // The coordinator commits each proposal at the gateway; only a
            // CONFIRMED carrier change counts as a switch.
            if let proposed = decision.proposed {
                XCTAssertTrue(policy.confirmSwitch(to: proposed,
                                                   proposalID: decision.proposalID,
                                                   at: t(time)))
            }
            if let last = lastChosen, decision.chosen != last {
                switches += 1
            }
            lastChosen = decision.chosen
        }
        step(0, quicHealthy: true)            // initial → quic
        step(1, quicHealthy: false, fail: true) // hard failure → relay
        step(2, quicHealthy: true)            // recovery evidence, streak 1
        step(3, quicHealthy: true)            // streak 2
        step(4, quicHealthy: true)            // streak 3, dwell 3<4: blocked
        step(5, quicHealthy: true)            // dwell+cooldown pass → quic
        step(6, quicHealthy: false, fail: true) // hard failure → relay
        step(7, quicHealthy: true)
        step(8, quicHealthy: true)
        step(9, quicHealthy: true)            // dwell 3<4: blocked
        step(10, quicHealthy: true)           // → quic
        step(11, quicHealthy: false, fail: true) // → relay
        // Exactly five carrier changes: two failure demotes are immediate,
        // two recoveries pay the full dwell+cooldown+streak price.
        XCTAssertEqual(switches, 5)
        XCTAssertEqual(lastChosen, .wssRelay)
    }

    func testStaleSamplesAreIgnored() {
        var policy = RouteSelectionPolicy()
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 85, at: 0),
        ])
        // QUIC's only sample is 12 s old (TTL 10): not fresh, not selectable.
        let stale = policy.evaluate(now: t(12), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
            .wssRelay: sample(rtt: 85, at: 12),
        ], forced: nil)
        XCTAssertEqual(stale.chosen, .wssRelay)
        XCTAssertEqual(stale.reason, .keptIncumbent)
        // A fresh sample starts a new streak from 1 (no carry-over).
        let fresh = policy.evaluate(now: t(13), samples: [
            .quicDirect: sample(rtt: 40, at: 13),
            .wssRelay: sample(rtt: 85, at: 13),
        ], forced: nil)
        XCTAssertEqual(fresh.chosen, .wssRelay)
        XCTAssertTrue(fresh.detail.contains("1/3"), fresh.detail)
    }

    func testMissingOrAgedEvidenceNeverHardFailsIncumbent() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        let initial = evaluateAndConfirm(&policy, now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
            .wssRelay: sample(rtt: 85, at: 0),
        ])
        XCTAssertEqual(initial.chosen, .quicDirect)
        // t5: QUIC goes silent (no sample at all). Missing telemetry is NOT
        // a failure (fix 1): the incumbent stays, and even a materially
        // better challenger earns only a single gain — no age-based shortcut.
        let silent = policy.evaluate(now: t(5), samples: [
            .wssRelay: sample(rtt: 20, at: 5),
        ], forced: nil)
        XCTAssertEqual(silent.chosen, .quicDirect)
        XCTAssertEqual(silent.reason, .keptIncumbent)
        XCTAssertEqual(silent.proposed, nil)
        XCTAssertTrue(silent.detail.contains("1/3"), silent.detail)
        // t21: QUIC's evidence is 21s old — far beyond the DELETED 2×TTL
        // hard-stale rule. Aged telemetry is still not failure: the relay's
        // proof is fresh but has only two gains, so the sustained-gain rule
        // (not age) decides — keep the incumbent, no proposal.
        let aged = policy.evaluate(now: t(21), samples: [
            .wssRelay: sample(rtt: 20, at: 21),
        ], forced: nil)
        XCTAssertEqual(aged.chosen, .quicDirect)
        XCTAssertNotEqual(aged.reason, .hardFailure)
        XCTAssertEqual(aged.proposed, nil, "no age-based shortcut to a switch")
        // An EXPLICIT transport failure signal is what moves the route.
        policy.noteHardFailure(.quicDirect, at: t(22))
        let failed = policy.evaluate(now: t(22), samples: [
            .quicDirect: sample(rtt: 40, at: 22),
            .wssRelay: sample(rtt: 20, at: 22),
        ], forced: nil)
        XCTAssertEqual(failed.chosen, .wssRelay)
        XCTAssertEqual(failed.proposed, .wssRelay)
        XCTAssertEqual(failed.reason, .hardFailure)
    }

    func testBlockedUDPNeverChoosesQUIC() {
        var policy = RouteSelectionPolicy()
        for time in 0...3 {
            let decision = policy.evaluate(now: t(TimeInterval(time)), samples: [
                .quicDirect: sample(rtt: 40, available: false, at: TimeInterval(time)),
                .wssRelay: sample(rtt: 85, at: TimeInterval(time)),
            ], forced: nil)
            XCTAssertEqual(decision.chosen, .wssRelay)
        }
    }

    // MARK: Network epoch

    func testNetworkEpochChangeResetsOnlyThatCandidate() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 40, at: 0),
        ])
        // Both direct candidates are materially better; QUIC is the best.
        for time in [4.0, 5.0] {
            let decision = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .webRTCDirect: sample(rtt: 30, at: time),
                .wssRelay: sample(rtt: 40, at: time),
            ], forced: nil)
            XCTAssertEqual(decision.chosen, .wssRelay)
            XCTAssertEqual(decision.reason, .keptIncumbent)
        }
        // t6: QUIC's epoch flips (path change). Without the fence QUIC's
        // full streak would promote it right now (span t4→t6 ≥ 2s,
        // dwell 6 ≥ 4); instead its streak is reset and the UNAFFECTED
        // webRTC candidate (streak 3, span t4→t6) promotes instead —
        // proving the fence hit QUIC only.
        let fenced = policy.evaluate(now: t(6), samples: [
            .quicDirect: sample(rtt: 20, epoch: "e2", at: 6),
            .webRTCDirect: sample(rtt: 30, at: 6),
            .wssRelay: sample(rtt: 40, at: 6),
        ], forced: nil)
        XCTAssertEqual(fenced.chosen, .webRTCDirect)
        XCTAssertEqual(fenced.proposed, .webRTCDirect)
        XCTAssertEqual(fenced.reason, .sustainedGain)
        XCTAssertTrue(policy.confirmSwitch(to: .webRTCDirect,
                                           proposalID: fenced.proposalID,
                                           at: t(6)))
    }

    // MARK: Forced mode

    func testForcedChoiceIsStrictAndNeverScored() {
        var policy = RouteSelectionPolicy()
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 85, at: 0),
        ])
        // A terrible-but-proven forced QUIC path wins over a healthy relay.
        let forced = policy.evaluate(now: t(1), samples: [
            .quicDirect: sample(rtt: 500, loss: 0.5, at: 1),
            .wssRelay: sample(rtt: 85, at: 1),
        ], forced: .quicDirect)
        XCTAssertEqual(forced.chosen, .quicDirect)
        XCTAssertEqual(forced.reason, .forcedMode)
        XCTAssertEqual(policy.incumbentKind, .wssRelay,
                       "proposals never adopt before confirmSwitch")
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: forced.proposalID,
                                           at: t(1)))
        // Forced + transport-reported unavailable: the fence fails and the
        // reason is truthful — forcedUnavailable, not a real switch.
        let forcedDead = policy.evaluate(now: t(2), samples: [
            .quicDirect: sample(rtt: 40, available: false, at: 2),
            .wssRelay: sample(rtt: 85, at: 2),
        ], forced: .quicDirect)
        XCTAssertEqual(forcedDead.chosen, .quicDirect,
                       "the forced path is the incumbent: it stays")
        XCTAssertEqual(forcedDead.reason, .forcedUnavailable)
        XCTAssertEqual(policy.incumbentKind, .quicDirect,
                       "no adoption without a confirm")
    }

    func testForcedUnavailableKeepsIncumbentWithoutSilentlyMoving() {
        var policy = RouteSelectionPolicy()
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 85, at: 0),
        ])
        let unavailable = policy.evaluate(now: t(1), samples: [
            .quicDirect: sample(rtt: 40, available: false, at: 1),
            .wssRelay: sample(rtt: 85, at: 1),
        ], forced: .quicDirect)
        XCTAssertEqual(unavailable.chosen, .wssRelay)
        XCTAssertEqual(unavailable.reason, .forcedUnavailable)
        XCTAssertNil(unavailable.proposed)
        // Unproven (mediaReady == false) forced paths are equally unusable.
        let unproven = policy.evaluate(now: t(2), samples: [
            .quicDirect: sample(rtt: 40, mediaReady: false, at: 2),
            .wssRelay: sample(rtt: 85, at: 2),
        ], forced: .quicDirect)
        XCTAssertEqual(unproven.chosen, .wssRelay)
        XCTAssertEqual(unproven.reason, .forcedUnavailable)
        // Stale forced evidence fails the freshness fence (defect 7).
        let stale = policy.evaluate(now: t(13), samples: [
            .quicDirect: sample(rtt: 40, at: 2),   // 11s old > TTL 10
            .wssRelay: sample(rtt: 85, at: 13),
        ], forced: .quicDirect)
        XCTAssertEqual(stale.chosen, .wssRelay)
        XCTAssertEqual(stale.reason, .forcedUnavailable)
        // An active failure mark blocks the forced candidate even when the
        // current sample looks healthy — recovery evidence must POSTDATE the
        // failure (a pre-failure timestamp never clears the mark).
        policy.noteHardFailure(.quicDirect, at: t(14))
        let marked = policy.evaluate(now: t(15), samples: [
            .quicDirect: sample(rtt: 40, at: 14),   // measured AT the failure
            .wssRelay: sample(rtt: 85, at: 15),
        ], forced: .quicDirect)
        XCTAssertEqual(marked.chosen, .wssRelay)
        XCTAssertEqual(marked.reason, .forcedUnavailable)
    }

    func testMediaReadyFalseIsNeverPromoted() {
        var policy = RouteSelectionPolicy()
        for time in 0...3 {
            let decision = policy.evaluate(now: t(TimeInterval(time)), samples: [
                .quicDirect: sample(rtt: 40, mediaReady: false, at: TimeInterval(time)),
                .wssRelay: sample(rtt: 85, at: TimeInterval(time)),
            ], forced: nil)
            XCTAssertEqual(decision.chosen, .wssRelay,
                           "mediaReady == false must never be promoted")
        }
    }

    func testInitialSelectionPrefersProvenRelayWhenQUICUnproven() {
        var policy = RouteSelectionPolicy()
        // QUIC is fast but below the 0.4 confidence floor: not selectable.
        let decision = policy.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, confidence: 0.3, at: 0),
            .wssRelay: sample(rtt: 85, confidence: 0.9, at: 0),
        ], forced: nil)
        XCTAssertEqual(decision.chosen, .wssRelay)
        XCTAssertEqual(decision.reason, .initialBest)
    }

    // MARK: Sustained gain & symmetric demote

    func testSustainedGainRequiresNConsecutiveEvaluations() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 85, at: 0),
        ])
        let gain1 = policy.evaluate(now: t(4), samples: [
            .quicDirect: sample(rtt: 40, at: 4),
            .wssRelay: sample(rtt: 85, at: 4),
        ], forced: nil)
        XCTAssertEqual(gain1.chosen, .wssRelay)
        let gain2 = policy.evaluate(now: t(5), samples: [
            .quicDirect: sample(rtt: 40, at: 5),
            .wssRelay: sample(rtt: 85, at: 5),
        ], forced: nil)
        // N−1 gains do NOT switch.
        XCTAssertEqual(gain2.chosen, .wssRelay)
        XCTAssertEqual(gain2.reason, .keptIncumbent)
        XCTAssertTrue(gain2.detail.contains("2/3"), gain2.detail)
        let gain3 = policy.evaluate(now: t(6), samples: [
            .quicDirect: sample(rtt: 40, at: 6),
            .wssRelay: sample(rtt: 85, at: 6),
        ], forced: nil)
        XCTAssertEqual(gain3.chosen, .quicDirect)
        XCTAssertEqual(gain3.proposed, .quicDirect)
        XCTAssertEqual(gain3.reason, .sustainedGain)
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: gain3.proposalID,
                                           at: t(6)))
    }

    func testDemoteAfterSwitchIsSymmetricAndRebuildsStreak() {
        var policy = RouteSelectionPolicy(config: fastConfig())
        _ = evaluateAndConfirm(&policy, now: t(0), samples: [
            .wssRelay: sample(rtt: 40, at: 0),
            .quicDirect: sample(rtt: 85, at: 0),
        ])
        // QUIC becomes materially better and earns a full streak.
        for time in [4.0, 5.0] {
            _ = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .wssRelay: sample(rtt: 40, at: time),
            ], forced: nil)
        }
        let promoted = policy.evaluate(now: t(6), samples: [
            .quicDirect: sample(rtt: 20, at: 6),
            .wssRelay: sample(rtt: 40, at: 6),
        ], forced: nil)
        XCTAssertEqual(promoted.chosen, .quicDirect)
        XCTAssertEqual(promoted.reason, .sustainedGain)
        XCTAssertTrue(policy.confirmSwitch(to: .quicDirect,
                                           proposalID: promoted.proposalID,
                                           at: t(6)))
        // Now the relay is the better path. The demote must pay the SAME
        // full price: 2 gains do not move it (streak was zeroed by the
        // CONFIRMED switch), and the minimum hold (4 s from t6) blocks the
        // third.
        for (time, expected) in [(7.0, RouteCandidateKind.quicDirect),
                                 (8.0, RouteCandidateKind.quicDirect),
                                 (9.0, RouteCandidateKind.quicDirect)] {
            let decision = policy.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .wssRelay: sample(rtt: 10, at: time),
            ], forced: nil)
            XCTAssertEqual(decision.chosen, expected)
        }
        let demoted = policy.evaluate(now: t(10), samples: [
            .quicDirect: sample(rtt: 20, at: 10),
            .wssRelay: sample(rtt: 10, at: 10),
        ], forced: nil)
        XCTAssertEqual(demoted.chosen, .wssRelay)
        XCTAssertEqual(demoted.proposed, .wssRelay)
        XCTAssertEqual(demoted.reason, .sustainedGain)
    }
}

// MARK: - DirectTransportPreferenceStore

@MainActor
final class DirectTransportPreferenceStoreTests: XCTestCase {
    private let suiteName = "DirectTransportPreferenceStoreTests"
    private var defaults: UserDefaults!

    override func setUp() {
        super.setUp()
        defaults = UserDefaults(suiteName: suiteName)
        defaults.removePersistentDomain(forName: suiteName)
    }

    override func tearDown() {
        defaults.removePersistentDomain(forName: suiteName)
        super.tearDown()
    }

    func testPersistenceRoundTripPerGateway() {
        let store = DirectTransportPreferenceStore(defaults: defaults)
        // Default is auto, stored as absence.
        XCTAssertEqual(store.choice(for: "gw-1"), .auto)
        store.setChoice(.quic, for: "gw-1")
        store.setChoice(.webRTC, for: "gw-2")
        XCTAssertEqual(store.choice(for: "gw-1"), .quic)
        XCTAssertEqual(store.choice(for: "gw-2"), .webRTC)
        // Back to auto removes the key (same contract as the route-mode store).
        store.setChoice(.auto, for: "gw-1")
        XCTAssertEqual(store.choice(for: "gw-1"), .auto)
        XCTAssertNil(defaults.string(forKey: "directTransport:gw-1"))
        XCTAssertEqual(store.choice(for: "gw-2"), .webRTC,
                       "gateways stay isolated")
    }

    func testNilAndEmptyGatewayIDsAreIgnored() {
        let store = DirectTransportPreferenceStore(defaults: defaults)
        store.setChoice(.quic, for: nil)
        store.setChoice(.quic, for: "")
        XCTAssertEqual(store.choice(for: nil), .auto)
        XCTAssertEqual(store.choice(for: ""), .auto)
        XCTAssertNil(defaults.string(forKey: "directTransport:"))
    }
}

// MARK: - MultiPathAdvisor

@MainActor
final class MultiPathAdvisorTests: XCTestCase {
    private let base = Date(timeIntervalSince1970: 2_000_000)
    private func t(_ seconds: TimeInterval) -> Date { base.addingTimeInterval(seconds) }
    private var nextSampleID: UInt64 = 0

    private func sample(rtt: Double, loss: Double = 0, at time: TimeInterval,
                        mediaReady: Bool = true, available: Bool = true) -> RouteCandidateSample {
        nextSampleID += 1
        return RouteCandidateSample(rttMs: rtt, jitterMs: 2, lossFraction: loss,
                                    reorderFraction: 0, queueCostMs: 0,
                                    available: available, confidence: 0.9,
                                    mediaReady: mediaReady, networkEpoch: "e1",
                                    measuredAt: t(time), sampleID: nextSampleID)
    }

    func testCandidateKindMapsOntoTwoValuedRouteWorld() {
        XCTAssertEqual(RouteCandidateKind.quicDirect.routeKind, .direct)
        XCTAssertEqual(RouteCandidateKind.webRTCDirect.routeKind, .direct)
        XCTAssertEqual(RouteCandidateKind.wssRelay.routeKind, .relay)
    }

    func testForcedTransportChoiceMapsOntoPolicyForcedCandidate() {
        var advisor = MultiPathAdvisor()
        let samples: [RouteCandidateKind: RouteCandidateSample] = [
            .quicDirect: sample(rtt: 500, at: 0),   // terrible but proven
            .webRTCDirect: sample(rtt: 300, at: 0),
            .wssRelay: sample(rtt: 85, at: 0),
        ]
        let quic = advisor.evaluate(now: t(0), samples: samples,
                                    forcedTransport: .quic)
        XCTAssertEqual(quic.chosen, .quicDirect)
        XCTAssertEqual(quic.reason, .forcedMode)
        let webRTC = advisor.evaluate(now: t(1), samples: samples,
                                      forcedTransport: .webRTC)
        XCTAssertEqual(webRTC.chosen, .webRTCDirect)
        XCTAssertEqual(webRTC.reason, .forcedMode)
        // Auto scores purely: the fastest proven candidate wins.
        var fresh = MultiPathAdvisor()
        let auto = fresh.evaluate(now: t(0), samples: samples, forcedTransport: .auto)
        XCTAssertEqual(auto.chosen, .wssRelay)
        XCTAssertEqual(auto.reason, .initialBest)
    }

    func testNoteHardFailurePassesThroughToPolicy() {
        var advisor = MultiPathAdvisor()
        let initial = advisor.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
            .wssRelay: sample(rtt: 85, at: 0),
        ], forcedTransport: .auto)
        XCTAssertTrue(advisor.confirmSwitch(to: initial.chosen,
                                            proposalID: initial.proposalID,
                                            at: t(0)))
        advisor.noteHardFailure(.quicDirect, at: t(1))
        let decision = advisor.evaluate(now: t(1), samples: [
            .quicDirect: sample(rtt: 40, at: 1),
            .wssRelay: sample(rtt: 85, at: 1),
        ], forcedTransport: .auto)
        XCTAssertEqual(decision.chosen, .wssRelay)
        XCTAssertEqual(decision.reason, .hardFailure)
        XCTAssertEqual(advisor.incumbentKind, .quicDirect,
                       "still a proposal until confirmed")
        XCTAssertTrue(advisor.confirmSwitch(to: .wssRelay,
                                            proposalID: decision.proposalID,
                                            at: t(1)))
        XCTAssertEqual(advisor.incumbentKind, .wssRelay)
    }

    func testConfirmSwitchForwardsExactlyOnce() {
        var advisor = MultiPathAdvisor()
        let first = advisor.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 40, at: 0),
            .wssRelay: sample(rtt: 85, at: 0),
        ], forcedTransport: .auto)
        let second = advisor.evaluate(now: t(1), samples: [
            .quicDirect: sample(rtt: 40, at: 1),
            .wssRelay: sample(rtt: 85, at: 1),
        ], forcedTransport: .auto)
        XCTAssertEqual(second.proposalID, first.proposalID,
                       "an unconfirmed real-switch proposal stays pending instead of being superseded")
        XCTAssertEqual(second.reason, .switchPending)
        XCTAssertEqual(second.chosen, .quicDirect, "the in-flight pick stays the declared carrier")
        XCTAssertNil(advisor.incumbentKind)
        XCTAssertTrue(advisor.confirmSwitch(to: first.chosen,
                                            proposalID: first.proposalID,
                                            at: t(1)),
                      "the delayed commit still confirms exactly once")
        XCTAssertEqual(advisor.incumbentKind, first.chosen)
        XCTAssertFalse(advisor.confirmSwitch(to: first.chosen,
                                             proposalID: first.proposalID,
                                             at: t(1)),
                       "exactly-once")
    }

    func testCancelPendingSwitchForwardsToPolicy() {
        var advisor = MultiPathAdvisor()
        // Relay becomes the confirmed incumbent.
        let initial = advisor.evaluate(now: t(0), samples: [
            .wssRelay: sample(rtt: 100, at: 0),
            .quicDirect: sample(rtt: 300, at: 0),
        ], forcedTransport: .auto)
        XCTAssertTrue(advisor.confirmSwitch(to: initial.chosen,
                                            proposalID: initial.proposalID,
                                            at: t(0)))
        // QUIC earns a full streak under the default clocks (dwell 20s).
        var proposal = RouteSelectionDecision(chosen: .wssRelay,
                                              reason: .keptIncumbent,
                                              detail: "")
        for time in [21.0, 23.0, 25.0] {   // span 4s ≥ sustainedMinDuration
            proposal = advisor.evaluate(now: t(time), samples: [
                .quicDirect: sample(rtt: 20, at: time),
                .wssRelay: sample(rtt: 100, at: time),
            ], forcedTransport: .auto)
        }
        XCTAssertEqual(proposal.proposed, .quicDirect)
        // The commit failed: cancel the in-flight switch — no adoption, no
        // cooldown — and re-propose on the next tick.
        advisor.cancelPendingSwitch(reason: "route commit failed")
        let retry = advisor.evaluate(now: t(26), samples: [
            .quicDirect: sample(rtt: 20, at: 26),
            .wssRelay: sample(rtt: 100, at: 26),
        ], forcedTransport: .auto)
        XCTAssertNotEqual(retry.proposalID, proposal.proposalID,
                          "after cancellation a fresh proposal may be issued immediately")
        XCTAssertEqual(retry.proposed, .quicDirect)
        XCTAssertEqual(advisor.incumbentKind, .wssRelay, "the real incumbent never moved")
        XCTAssertFalse(advisor.confirmSwitch(to: .quicDirect,
                                             proposalID: proposal.proposalID,
                                             at: t(26)),
                       "a cancelled proposal can never confirm")
    }

    func testNoteSampleForwardsEvidenceWithoutDeciding() {
        var advisor = MultiPathAdvisor()
        // Eight distinct evidence samples with 10% loss: at full evidence
        // the loss term (120ms-equivalent) must dominate the 20ms RTT edge.
        for index in 0..<8 {
            advisor.noteSample(.quicDirect,
                               sample(rtt: 20, loss: 0.10, at: TimeInterval(index)),
                               now: t(TimeInterval(index)))
        }
        let decision = advisor.evaluate(now: t(8), samples: [
            .quicDirect: sample(rtt: 20, loss: 0.10, at: 8),
            .wssRelay: sample(rtt: 100, at: 8),
        ], forcedTransport: .auto)
        XCTAssertEqual(decision.chosen, .wssRelay,
                       "140ms-equivalent lossy path must lose to the 100ms clean relay")
        // Contrast: a single lossy sample carries 1/8 evidence, so the loss
        // term scales to 15ms-equivalent and the fast path still wins.
        var freshAdvisor = MultiPathAdvisor()
        let single = freshAdvisor.evaluate(now: t(0), samples: [
            .quicDirect: sample(rtt: 20, loss: 0.10, at: 0),
            .wssRelay: sample(rtt: 100, at: 0),
        ], forcedTransport: .auto)
        XCTAssertEqual(single.chosen, .quicDirect)
    }
}

// MARK: - Diagnostics record (route_selection export section)

final class RouteSelectionRecordTests: XCTestCase {
    func testRecordIsSortedAndCarriesNoEpochOrTokens() {
        let decision = RouteSelectionDecision(chosen: .quicDirect,
                                              reason: .sustainedGain,
                                              detail: "quicDirect 持续更优")
        let samples: [RouteCandidateKind: RouteCandidateSample] = [
            .wssRelay: RouteCandidateSample(rttMs: 85, jitterMs: 2, lossFraction: 0,
                                            reorderFraction: 0, queueCostMs: 0,
                                            available: true, confidence: 0.9,
                                            mediaReady: true, networkEpoch: "secret-epoch-token",
                                            measuredAt: Date(timeIntervalSince1970: 0),
                                            sampleID: 7),
            .quicDirect: RouteCandidateSample(rttMs: 40, jitterMs: 2, lossFraction: 0,
                                              reorderFraction: 0, queueCostMs: 0,
                                              available: true, confidence: 0.9,
                                              mediaReady: true, networkEpoch: "secret-epoch-token",
                                              measuredAt: Date(timeIntervalSince1970: 0),
                                              sampleID: 8),
        ]
        let record = RouteSelectionRecord(at: Date(timeIntervalSince1970: 0),
                                          decision: decision, samples: samples)
        XCTAssertEqual(record.chosen, "quicDirect")
        XCTAssertEqual(record.reason, "sustainedGain")
        XCTAssertEqual(record.candidates.map(\.kind), ["quicDirect", "wssRelay"],
                       "candidates are exported in a deterministic order")
        XCTAssertEqual(record.candidates.first?.rttMs, 40)
    }
}
