import Foundation

/// Conservative, quality-based routing decision between the guaranteed WSS
/// path and a measured direct candidate. Decisions compare ONLY comparable,
/// FRESH end-to-end round trips (selected-pair/echo RTT vs WSS ping RTT) — a
/// candidate is promoted only when it is sustained, materially better, and
/// not exhibiting jitter/loss/stall problems. Missing, stale or insufficient
/// metrics never promote: the healthy transport is retained by default. P2P
/// is never assumed faster.
struct MediaRouteAdvisor {
    struct Metrics {
        /// End-to-end round trips over the candidate (selected pair / echo).
        var candidateRTT: [TimeInterval] = []
        /// End-to-end round trips over the live WSS path (app ping/pong).
        var baselineRTT: [TimeInterval] = []
        /// Inter-sample RTT jitter on the candidate, seconds.
        var candidateJitter: TimeInterval?
        /// Fraction lost 0...1 on the candidate.
        var candidateLoss: Double?
        /// True when the newest sample is inside the freshness window.
        var samplesFresh: Bool = true
        /// Consecutive stalled/failed quality probes on an ACTIVE direct path.
        var stalls: Int = 0
        /// False while the candidate connection is still settling.
        var candidateStable = false
        /// True when the candidate is no longer reachable at all.
        var candidateLost = false

        init(candidateRTT: [TimeInterval] = [],
             baselineRTT: [TimeInterval] = [],
             candidateJitter: TimeInterval? = nil,
             candidateLoss: Double? = nil,
             samplesFresh: Bool = true,
             stalls: Int = 0,
             candidateStable: Bool = false,
             candidateLost: Bool = false) {
            self.candidateRTT = candidateRTT
            self.baselineRTT = baselineRTT
            self.candidateJitter = candidateJitter
            self.candidateLoss = candidateLoss
            self.samplesFresh = samplesFresh
            self.stalls = stalls
            self.candidateStable = candidateStable
            self.candidateLost = candidateLost
        }
    }

    enum Decision: Equatable {
        case keepBaseline
        case promote
        case fallbackToBaseline
    }

    /// Minimum samples on BOTH paths before any comparison is meaningful.
    let minimumSamples: Int
    /// Candidate must beat the baseline median by at least this fraction.
    let improvementThreshold: Double
    /// Time the call must have been up before promotion (anti-flap).
    let minimumDwell: TimeInterval
    /// One promotion plus one fallback per call, ever.
    let maximumPromotions: Int
    let maximumFallbacks: Int
    /// Jitter/loss ceilings above which a faster median does NOT promote.
    let maxAcceptableJitter: TimeInterval
    let maxAcceptableLoss: Double
    /// Consecutive bad evaluations required before an active direct path is
    /// rolled back (hysteresis prevents flapping on a single glitch).
    let sustainedBadCount: Int

    private(set) var promotionsUsed = 0
    private(set) var fallbacksUsed = 0
    private var promoted = false
    private var lastSwitchAt: Date?

    init(
        minimumSamples: Int = 20,
        improvementThreshold: Double = 0.15,
        minimumDwell: TimeInterval = 10,
        maximumPromotions: Int = 1,
        maximumFallbacks: Int = 1,
        maxAcceptableJitter: TimeInterval = 0.15,
        maxAcceptableLoss: Double = 0.08,
        sustainedBadCount: Int = 2
    ) {
        self.minimumSamples = minimumSamples
        self.improvementThreshold = improvementThreshold
        self.minimumDwell = minimumDwell
        self.maximumPromotions = maximumPromotions
        self.maximumFallbacks = maximumFallbacks
        self.maxAcceptableJitter = maxAcceptableJitter
        self.maxAcceptableLoss = maxAcceptableLoss
        self.sustainedBadCount = sustainedBadCount
    }

    /// - Parameters:
    ///   - callDuration: how long the call has been connected.
    ///   - baselineHealthy: false when the WSS path itself is failing (then
    ///     reachability, not quality, decides — a working candidate wins, but
    ///     only with fresh evidence of connectivity).
    mutating func decide(_ metrics: Metrics, callDuration: TimeInterval, baselineHealthy: Bool) -> Decision {
        if metrics.candidateLost {
            return considerFallback()
        }
        guard !promoted, promotionsUsed < maximumPromotions else { return .keepBaseline }
        // Stale samples or an unsettled candidate never justify a switch.
        guard metrics.samplesFresh, metrics.candidateStable else { return .keepBaseline }
        if let jitter = metrics.candidateJitter, jitter > maxAcceptableJitter { return .keepBaseline }
        if let loss = metrics.candidateLoss, loss > maxAcceptableLoss { return .keepBaseline }
        if baselineHealthy {
            // Quality-based promotion: comparable FRESH e2e round trips only.
            guard callDuration >= minimumDwell else { return .keepBaseline }
            guard metrics.candidateRTT.count >= minimumSamples,
                  metrics.baselineRTT.count >= minimumSamples else { return .keepBaseline }
            let candidate = median(metrics.candidateRTT)
            let baseline = median(metrics.baselineRTT)
            guard candidate > 0, baseline > 0 else { return .keepBaseline }
            guard candidate < baseline * (1 - improvementThreshold) else { return .keepBaseline }
            promotionsUsed += 1
            promoted = true
            lastSwitchAt = Date()
            return .promote
        }
        // Baseline unhealthy: reachability decides (optimization is a luxury;
        // a working candidate beats a dead path). Requires a stable candidate,
        // bounded by the dwell; evidence of connectivity is the selected pair
        // (RTT samples may not have arrived during an outage).
        guard callDuration >= minimumDwell, metrics.candidateStable else { return .keepBaseline }
        promotionsUsed += 1
        promoted = true
        lastSwitchAt = Date()
        return .promote
    }

    /// Evaluates the ACTIVE direct path's continuous quality. Returns
    /// `.fallbackToBaseline` only after `sustainedBadCount` consecutive bad
    /// evaluations (loss/stall/death) and anti-flap dwell since the switch.
    mutating func evaluateActiveDirect(_ metrics: Metrics,
                                       switchedAfter minimumSwitchAge: TimeInterval = 12) -> Decision {
        guard promoted else { return .keepBaseline }
        if let since = lastSwitchAt, Date().timeIntervalSince(since) < minimumSwitchAge {
            return .keepBaseline
        }
        let bad = metrics.candidateLost
            || metrics.stalls >= sustainedBadCount
            || (metrics.candidateLoss.map { $0 > maxAcceptableLoss } ?? false)
            || (metrics.candidateJitter.map { $0 > maxAcceptableJitter } ?? false)
            || !metrics.samplesFresh
        return bad ? considerFallback() : .keepBaseline
    }

    /// Marks the active transport as already on the candidate (e.g. a manual
    /// direct adoption that auto monitoring must now protect). Enables the
    /// one-shot fallback path without counting a promotion.
    mutating func markAlreadyPromoted() {
        promoted = true
        promotionsUsed = max(promotionsUsed, 1)
    }

    var canFallback: Bool { promoted && fallbacksUsed < maximumFallbacks }

    mutating func considerFallback() -> Decision {
        guard promoted, fallbacksUsed < maximumFallbacks else { return .keepBaseline }
        fallbacksUsed += 1
        promoted = false
        return .fallbackToBaseline
    }

    private func median(_ values: [TimeInterval]) -> TimeInterval {
        let sorted = values.sorted()
        guard !sorted.isEmpty else { return 0 }
        let middle = sorted.count / 2
        return sorted.count % 2 == 1 ? sorted[middle] : (sorted[middle - 1] + sorted[middle]) / 2
    }

    /// Inter-sample jitter: median absolute delta of consecutive RTTs.
    static func jitter(of samples: [TimeInterval]) -> TimeInterval? {
        guard samples.count >= 3 else { return nil }
        var deltas: [TimeInterval] = []
        for index in 1..<samples.count { deltas.append(abs(samples[index] - samples[index - 1])) }
        let sorted = deltas.sorted()
        return sorted[sorted.count / 2]
    }
}

// MARK: - Multi-path route selection (build 61)

extension RouteCandidateKind {
    /// Maps the three-valued candidate world onto the two-valued route world
    /// the rest of the routing stack understands: both direct candidates own
    /// the `.direct` route kind; the WSS relay owns `.relay`. Route-kind
    /// consumers (route controller, UI status lines, diagnostics) keep their
    /// existing two-valued semantics while the policy differentiates the
    /// direct transports internally.
    var routeKind: MediaRouteKind {
        switch self {
        case .quicDirect, .webRTCDirect: return .direct
        case .wssRelay: return .relay
        }
    }
}

/// Build-61 multi-path advisor: a thin, coordinator-consumable wrapper over
/// the deterministic ``RouteSelectionPolicy`` for QUIC-capable gateways.
///
/// This is ADDITIVE and strictly separate from ``decide()`` above: old
/// gateways keep the exact single-promotion/one-fallback behavior they have
/// today; only a gateway that advertises QUIC installs this advisor. It owns
/// CONTINUED rescore with hysteresis across three candidates
/// (QUIC direct / WebRTC direct / WSS relay) and maps the user's
/// ``DirectTransportChoice`` onto the policy's forced candidate:
/// `.auto` ⇒ no force (pure scoring), `.quic` ⇒ forced `quicDirect`,
/// `.webRTC` ⇒ forced `webRTCDirect`.
///
/// Two-phase commit contract (root review defect 5 / fix 2): `evaluate` is
/// phase 1 — it returns a decision plus its `proposed`/`proposalID`
/// proposal but never adopts. After committing the route at the gateway the
/// coordinator calls `confirmSwitch(to:proposalID:at:)` (phase 2); only the
/// newest, unexpired proposal confirms, exactly once, so a failed commit or
/// a late callback never corrupts policy state. While a REAL switch proposal
/// is outstanding, further `evaluate` calls return `.switchPending` reusing
/// the same proposalID (probe ticks cannot invalidate an in-flight commit);
/// `cancelPendingSwitch(reason:`) abandons it after a failed/rolled-back
/// commit without adoption side effects. `noteSample` feeds background probe
/// evidence without producing a decision; `noteHardFailure` is the
/// recovery-layer failure seam.
struct MultiPathAdvisor {
    private var policy: RouteSelectionPolicy

    init(config: RouteSelectionPolicy.Config = .default) {
        policy = RouteSelectionPolicy(config: config)
    }

    /// The candidate the policy currently carries, if any decision was
    /// CONFIRMED. A merely proposed switch does not move this — evaluate
    /// never adopts on its own.
    var incumbentKind: RouteCandidateKind? { policy.incumbentKind }

    /// Phase 1 — one deterministic evaluation. `now` is injected so tests
    /// fully control the clock (the policy never reads a hidden `Date()`).
    /// The returned decision carries the two-phase-commit fields:
    /// `decision.proposed` is the candidate to commit at the gateway (nil =
    /// keep `decision.chosen`), and `decision.proposalID` is the single
    /// confirmable token.
    mutating func evaluate(now: Date,
                           samples: [RouteCandidateKind: RouteCandidateSample],
                           forcedTransport: DirectTransportChoice = .auto) -> RouteSelectionDecision {
        let forced: RouteCandidateKind?
        switch forcedTransport {
        case .auto: forced = nil
        case .quic: forced = .quicDirect
        case .webRTC: forced = .webRTCDirect
        }
        return policy.evaluate(now: now, samples: samples, forced: forced)
    }

    /// Build-62: evaluate against the RESOLVED user mode (forced kind /
    /// permitted set). The mapping owns 中继 ⇒ WSS-only, 直连 ⇒ direct
    /// family, 自动 ⇒ everything; the policy applies it strictly.
    mutating func evaluate(now: Date,
                           samples: [RouteCandidateKind: RouteCandidateSample],
                           mode: RouteModeMapping) -> RouteSelectionDecision {
        policy.evaluate(now: now, samples: samples,
                        forced: mode.forcedKind, permitted: mode.permittedSet)
    }

    /// Seeds a confirmed incumbent (see ``RouteSelectionPolicy``).
    mutating func seedIncumbent(_ kind: RouteCandidateKind, at date: Date) {
        policy.seedIncumbent(kind, at: date)
    }


    /// Phase 2 — applies the adoption half of a decision the coordinator
    /// already committed. Forwards to the policy; see
    /// ``RouteSelectionPolicy/confirmSwitch(to:proposalID:at:)`` for the
    /// exactly-once / pending / expired contract.
    @discardableResult
    mutating func confirmSwitch(to kind: RouteCandidateKind,
                                proposalID: UInt64,
                                at date: Date) -> Bool {
        policy.confirmSwitch(to: kind, proposalID: proposalID, at: date)
    }

    /// Abandons an in-flight switch after a failed/rolled-back route commit
    /// (fix 2): forwards to the policy, which clears the outstanding
    /// proposal WITHOUT applying adoption — cooldown/min-hold clocks are
    /// never started by a switch that never happened.
    mutating func cancelPendingSwitch(reason: String) {
        policy.cancelPendingSwitch(reason: reason)
    }

    /// Evidence-only seam: feeds one validated sample into the policy's
    /// per-candidate evidence cache WITHOUT producing a decision
    /// (coordinator background probes). Gain streaks, adoption state and
    /// outstanding proposals are untouched.
    mutating func noteSample(_ kind: RouteCandidateKind,
                             _ sample: RouteCandidateSample,
                             now: Date) {
        policy.noteSample(kind, sample, now: now)
    }

    /// Recovery-layer seam: records a transport-reported hard failure so the
    /// next evaluation demotes that candidate through the fast path.
    mutating func noteHardFailure(_ kind: RouteCandidateKind, at date: Date) {
        policy.noteHardFailure(kind, at: date)
    }
}
