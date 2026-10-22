import Foundation

/// One selectable media path for build-61 multi-path routing.
///
/// * `quicDirect` — QUIC DATAGRAM direct connection to the gateway (the new
///   transport built in parallel; this policy only consumes its metrics).
/// * `webRTCDirect` — the existing direct WebRTC peer connection.
/// * `wssRelay` — the guaranteed authenticated WSS relay (the safe default).
enum RouteCandidateKind: String, Hashable, CaseIterable {
    case quicDirect
    case webRTCDirect
    case wssRelay
}

/// One measured telemetry sample for a route candidate. Everything the policy
/// needs for ONE evaluation; freshness is derived from `measuredAt` by the
/// caller-supplied `now`, so tests are fully deterministic (no hidden
/// `Date()` anywhere in this file).
struct RouteCandidateSample {
    /// Freshest measured round trip (ms). Must be finite and >= 0.
    var rttMs: Double
    /// Recent inter-arrival / pong spread (ms). Must be finite and >= 0.
    var jitterMs: Double
    /// Fraction lost 0...1 (inclusive). Must be finite.
    var lossFraction: Double
    /// Fraction reordered 0...1 (inclusive). Must be finite.
    var reorderFraction: Double
    /// Audio queue/backlog estimate (ms). This is an ESTIMATE input produced
    /// by the transport layer; presenters must label it "（估计）". The policy
    /// itself treats it as one additive score term and never presents it.
    /// Must be finite and >= 0.
    var queueCostMs: Double
    /// False when the transport reports the path unreachable.
    var available: Bool
    /// 0...1 — driven by sample count/age. Low confidence inflates the
    /// effective score so unproven paths lose to proven ones.
    var confidence: Double
    /// True only when bidirectional GENUINE media proof exists (echo/data-
    /// channel reachability never counts — same contract as the rest of the
    /// routing stack).
    var mediaReady: Bool
    /// Client network-epoch token: opaque path-change detector. A change
    /// fences that candidate's samples and gain counters. Never exported.
    var networkEpoch: String
    /// When this sample was measured (freshness anchor). More than
    /// `Config.maxFutureSkew` ahead of the evaluation clock the sample is
    /// rejected; a bounded 0...maxFutureSkew skew is tolerated, with
    /// freshness computed from min(measuredAt, now) so a skewed sample can
    /// never stay "fresh forever".
    var measuredAt: Date
    /// Opaque transport-assigned measurement ID. Two evaluations fed the
    /// SAME measurement carry the SAME id; gain streaks only grow on DISTINCT
    /// ids, so re-evaluating one cached sample is never sustained evidence.
    var sampleID: UInt64
    /// Build-62 evidence honesty: true when `lossFraction`/`reorderFraction`
    /// are MEASURED. False means the transport cannot observe loss on this
    /// path — the terms are EXCLUDED from the score (an unknown is never
    /// punished as measured loss) and the candidate's confidence is
    /// discounted instead of being credited with a perfect record.
    var hasLossMeasurement: Bool = true
    /// True when `jitterMs` is a real inter-arrival/pong spread; false =
    /// unknown (excluded + discounted, never a fabricated zero).
    var hasJitterMeasurement: Bool = true
    /// True when `queueCostMs` is a real backlog estimate; false = no
    /// backlog visibility (excluded + discounted).
    var hasQueueEstimate: Bool = true
}

/// Why the policy returned what it returned. String-raw so the diagnostics
/// export can persist it verbatim.
enum RouteSelectionReason: String, Equatable {
    /// First evaluation picked the best proven candidate.
    case initialBest
    /// Challenger sustained the full gain rule and was promoted.
    case sustainedGain
    /// Incumbent hard-failed; immediate switch to the best proven candidate.
    case hardFailure
    /// A forced (user/coordinator) choice was honored strictly.
    case forcedMode
    /// Forced candidate unavailable/unproven: incumbent kept, no silent move.
    case forcedUnavailable
    /// A network-epoch change fenced a candidate and affected this decision.
    case epochChange
    /// Not enough evidence (nothing qualified / no usable replacement).
    case insufficientEvidence
    /// A real switch proposal is outstanding and unexpired (the
    /// coordinator's route commit is in flight): the carrier is kept and
    /// the SAME proposalID stays confirmable until expiry or cancellation.
    case switchPending
    /// Incumbent retained: no challenger met the full switch rule
    /// (near-identical quality, gains still accumulating, dwell or cooldown).
    case keptIncumbent
}

/// One routing decision. `detail` is a short plain-language basis (zh-Hans by
/// convention, diagnostics-facing); presenters append "（估计）" labels where a
/// displayed number derives from `queueCostMs`.
struct RouteSelectionDecision: Equatable {
    var chosen: RouteCandidateKind
    var reason: RouteSelectionReason
    /// Two-phase commit (defect 5): the candidate this evaluation PROPOSES
    /// to adopt once the caller committed the route at the gateway — nil
    /// means "keep `chosen`" (a no-change proposal).
    var proposed: RouteCandidateKind?
    /// Unique and monotonic per policy instance. Only the NEWEST outstanding
    /// proposal — and only within `Config.proposalExpiryInterval` — can be
    /// confirmed via ``RouteSelectionPolicy/confirmSwitch(to:proposalID:at:)``.
    var proposalID: UInt64
    var detail: String

    init(chosen: RouteCandidateKind,
         reason: RouteSelectionReason,
         proposed: RouteCandidateKind? = nil,
         proposalID: UInt64 = 0,
         detail: String) {
        self.chosen = chosen
        self.reason = reason
        self.proposed = proposed
        self.proposalID = proposalID
        self.detail = detail
    }
}

/// Deterministic multi-path route-selection policy (build 61, root review
/// defects 5–9).
///
/// This is a PURE scoring/state machine: Foundation dates only, every time
/// enters through the `now:` parameter, and the same inputs always produce
/// the same decision. It CONTINUOUSLY rescores all candidates (unlike the
/// one-shot `MediaRouteAdvisor.decide`) with hysteresis, a sustained-gain
/// window that requires DISTINCT measurements AND a minimum sustained
/// duration, minimum-hold + cooldown anti-flap guards anchored at CONFIRMED
/// adoptions only, network-epoch fencing, a voice-quality loss model with a
/// degraded gate, and a hard-failure fast path driven by EXPLICIT signals
/// only (compatible with the 45 s media-recovery contract — the recovery
/// layer reports hard failures via ``noteHardFailure(_:at:)``).
///
/// Two-phase commit (defect 5): `evaluate(now:samples:forced:)` NEVER adopts.
/// It updates per-candidate evidence caches and returns a decision whose
/// `proposed`/`proposalID` pair is the single outstanding proposal. The
/// coordinator commits the route at the gateway and then calls
/// `confirmSwitch(to:proposalID:at:)`; only the newest, unexpired proposal
/// confirms, exactly once. A failed route commit therefore never starts
/// cooldown/min-hold clocks, and a late/duplicate commit callback can never
/// "confirm" a switch that never happened.
///
/// Hard guarantees:
/// * A candidate with `mediaReady == false` is NEVER proposed as a switch
///   (the only exception is the initial relay fallback, which exists so a
///   call always has a carrier — the relay is the safe default).
/// * Silence, missing, aged or out-of-order telemetry is NEVER failure:
///   availability moves ONLY on explicit transport failure signals —
///   `noteHardFailure` or an `available == false` report. A busy UI, missing
///   pongs or quiet audio must never tear down a working path.
/// * Forced choices are STRICT — scoring never overrides them (a forced
///   evaluation supersedes any pending proposal) — but a forced-but-dead
///   candidate is never promoted (`forcedUnavailable`).
///
/// Pending serialization (root review fix 2): while a REAL switch proposal
/// is outstanding and unexpired, non-forced evaluations keep ingesting
/// evidence (caches/streaks update) but return `.switchPending` reusing the
/// SAME proposalID — periodic probe ticks cannot invalidate a legitimate
/// in-flight commit. Only proposal expiry, explicit
/// ``cancelPendingSwitch(reason:)``, or a hard failure may issue a new
/// proposal.
///
/// Evidence integrity (root review fixes 3–4): an out-of-order sample
/// (older `measuredAt` than the stored one) never regresses freshness and
/// never triggers the epoch fence; a network-epoch change seeds a NEW
/// context (streaks reset, failure marks survive until post-failure
/// recovery evidence), while a sample from a RETIRED (superseded) epoch is
/// a late callback — rejected for storage, history and scoring regardless
/// of how fresh its timestamps look; the distinct-sample history is a FIFO
/// window bounded at ``sampleHistoryLimit``. `evaluate` scores ONLY
/// intake-accepted, current-context evidence (``ingest`` reports
/// per-candidate acceptance) — raw, unvalidated input never feeds
/// score/gain directly.
struct RouteSelectionPolicy {

    // MARK: Configuration

    struct Config: Equatable {
        /// Every field defaults to the production value; tests may shrink
        /// the evidence windows without restating the whole matrix.
        init(hysteresisFraction: Double = Config.default.hysteresisFraction,
             minimumHoldInterval: TimeInterval = Config.default.minimumHoldInterval,
             switchCooldown: TimeInterval = Config.default.switchCooldown,
             sampleFreshnessTTL: TimeInterval = Config.default.sampleFreshnessTTL,
             sustainedWindow: Int = Config.default.sustainedWindow,
             sustainedWindowSpan: TimeInterval = Config.default.sustainedWindowSpan,
             sustainedMinDuration: TimeInterval = Config.default.sustainedMinDuration,
             maxFutureSkew: TimeInterval = Config.default.maxFutureSkew,
             proposalExpiryInterval: TimeInterval = Config.default.proposalExpiryInterval,
             minimumConfidence: Double = Config.default.minimumConfidence,
             confidenceFloor: Double = Config.default.confidenceFloor,
             unavailablePenalty: Double = Config.default.unavailablePenalty,
             jitterWeight: Double = Config.default.jitterWeight,
             lossWeightPerPoint: Double = Config.default.lossWeightPerPoint,
             reorderWeight: Double = Config.default.reorderWeight,
             queueCostWeight: Double = Config.default.queueCostWeight,
             lossGateFraction: Double = Config.default.lossGateFraction,
             lossGateConfidence: Double = Config.default.lossGateConfidence,
             minSamplesForLossDecisions: Int = Config.default.minSamplesForLossDecisions) {
            self.hysteresisFraction = hysteresisFraction
            self.minimumHoldInterval = minimumHoldInterval
            self.switchCooldown = switchCooldown
            self.sampleFreshnessTTL = sampleFreshnessTTL
            self.sustainedWindow = sustainedWindow
            self.sustainedWindowSpan = sustainedWindowSpan
            self.sustainedMinDuration = sustainedMinDuration
            self.maxFutureSkew = maxFutureSkew
            self.proposalExpiryInterval = proposalExpiryInterval
            self.minimumConfidence = minimumConfidence
            self.confidenceFloor = confidenceFloor
            self.unavailablePenalty = unavailablePenalty
            self.jitterWeight = jitterWeight
            self.lossWeightPerPoint = lossWeightPerPoint
            self.reorderWeight = reorderWeight
            self.queueCostWeight = queueCostWeight
            self.lossGateFraction = lossGateFraction
            self.lossGateConfidence = lossGateConfidence
            self.minSamplesForLossDecisions = minSamplesForLossDecisions
        }

        /// Challenger effective score must be below incumbent × (1 − h).
        var hysteresisFraction: Double
        /// Minimum time an incumbent keeps the route before any quality-based
        /// demotion (anti-flap). Anchored at the CONFIRMED adoption.
        var minimumHoldInterval: TimeInterval
        /// Minimum time between two CONFIRMED switches (either direction).
        var switchCooldown: TimeInterval
        /// A sample older than this is not fresh.
        var sampleFreshnessTTL: TimeInterval
        /// Consecutive DISTINCT-sample gains the challenger must hold.
        var sustainedWindow: Int
        /// The consecutive-gain streak must fit inside this span: a streak
        /// whose first gain is older is restarted from 1.
        var sustainedWindowSpan: TimeInterval
        /// The streak's first→last gain timestamps must ALSO span at least
        /// this long: a window count reached in a shorter period is
        /// accumulation, not sustained quality (defect 6).
        var sustainedMinDuration: TimeInterval
        /// A sample measured more than this far AHEAD of the evaluation
        /// clock is rejected outright (defect 7).
        var maxFutureSkew: TimeInterval
        /// An outstanding proposal expires after this interval; expiry is
        /// checked lazily at confirm time — no hidden timers (defect 5).
        var proposalExpiryInterval: TimeInterval
        /// Minimum confidence to be selectable at all.
        var minimumConfidence: Double
        /// Effective score divides by max(confidence, floor): an
        /// unproven-but-confident candidate is never infinitely penalized,
        /// but 0-confidence (missing) candidates are.
        var confidenceFloor: Double
        /// Added to the base score of any unavailable candidate (ms-scale):
        /// an unavailable path is effectively never "better".
        var unavailablePenalty: Double
        // Score-term weights. The exact formula is documented on
        // ``effectiveScore(_:lossScale:)``; these exist so tests can pin
        // terms.
        var jitterWeight: Double
        /// Voice-quality loss model (defect 8): at 1200, 10% loss costs
        /// 120ms-equivalent — a 20ms path with 10% loss (140 total) must
        /// never beat a 100ms clean path.
        var lossWeightPerPoint: Double
        var reorderWeight: Double
        /// Weight of the queue/backlog ESTIMATE term.
        var queueCostWeight: Double
        /// Degraded gate (defect 8): a measured lossFraction above this —
        /// with sufficient loss confidence — marks a candidate degraded.
        var lossGateFraction: Double
        /// Minimum loss confidence for the degraded gate to apply.
        var lossGateConfidence: Double
        /// Distinct samples required before loss inputs apply at full
        /// weight; below that the loss term is scaled by count/8, so one
        /// packet never swings a decision.
        var minSamplesForLossDecisions: Int

        static let `default` = Config(
            hysteresisFraction: 0.15,
            minimumHoldInterval: 20,
            switchCooldown: 15,
            sampleFreshnessTTL: 10,
            sustainedWindow: 3,
            sustainedWindowSpan: 12,
            sustainedMinDuration: 4,
            maxFutureSkew: 5,
            proposalExpiryInterval: 8,
            minimumConfidence: 0.4,
            confidenceFloor: 0.25,
            unavailablePenalty: 10_000,
            jitterWeight: 2.0,
            lossWeightPerPoint: 1200.0,
            reorderWeight: 120.0,
            queueCostWeight: 1.0,
            lossGateFraction: 0.05,
            lossGateConfidence: 0.5,
            minSamplesForLossDecisions: 8)
    }

    // MARK: State

    let config: Config
    private(set) var incumbentKind: RouteCandidateKind?
    /// Dwell clock anchor (defect 5): set ONLY by confirmSwitch.
    private(set) var incumbentSince: Date?
    /// Cooldown clock anchor (defect 5): set ONLY by confirmSwitch.
    private(set) var lastSwitchAt: Date?
    /// Adoption generation: bumped on every confirmed switch; gain evidence
    /// is valid only within the generation that produced it.
    private(set) var policyGeneration: UInt64 = 0
    /// Latest VALID sample per candidate (defect 7: invalid samples are
    /// rejected at intake and never evict prior evidence). A candidate with
    /// no entry is "unknown" — excluded from scoring with confidence 0, its
    /// availability unaffected (nothing is fabricated as zeros).
    private var lastValidSample: [RouteCandidateKind: RouteCandidateSample] = [:]
    /// Rolling distinct-sample history per candidate (newest last, FIFO
    /// eviction at ``sampleHistoryLimit``) — the bounded evidence base for
    /// loss scaling/confidence. Cleared on epoch change.
    private var sampleHistory: [RouteCandidateKind: [UInt64]] = [:]
    /// Retired (superseded) network-epoch tokens per candidate — a bounded
    /// FIFO lineage (fix 4). A sample carrying a retired epoch is a late
    /// callback from an old path context and is rejected outright: it never
    /// overwrites current-context evidence, never resets streaks, never
    /// lifts a failure fence, and never feeds score/gain.
    private var retiredEpochs: [RouteCandidateKind: [String]] = [:]
    private var consecutiveGains: [RouteCandidateKind: Int] = [:]
    private var firstGainAt: [RouteCandidateKind: Date] = [:]
    /// The measurement id that fed the most recent gain (defect 6: a streak
    /// grows only on a FRESH measurement DISTINCT from the previous one).
    private var lastGainSampleID: [RouteCandidateKind: UInt64] = [:]
    private var networkEpoch: [RouteCandidateKind: String] = [:]
    /// Transport-reported failure marks (recovery layer). A mark is cleared
    /// only by recovery evidence: a fresh, available, media-proven sample
    /// measured AFTER the failure, or a network-epoch change.
    private var hardFailedAt: [RouteCandidateKind: Date] = [:]
    /// Two-phase commit state (defect 5, fix 2): only the newest proposal,
    /// while unexpired, can be confirmed; a REAL switch also gates
    /// re-evaluation (.switchPending) so probe ticks cannot invalidate an
    /// in-flight commit.
    private var outstandingProposal: (id: UInt64, proposed: RouteCandidateKind?, issuedAt: Date)?
    private var proposalCounter: UInt64 = 0
    /// Why the last pending switch was cancelled (diagnostics; nil when a
    /// pending switch was never cancelled).
    private(set) var lastPendingCancelReason: String?

    init(config: Config = .default) {
        self.config = config
    }

    /// Rolling distinct-sample history bound per candidate (the oldest ids
    /// are evicted FIFO beyond this limit — the history is never unbounded).
    static let sampleHistoryLimit = 32

    /// Retired-epoch lineage bound per candidate (FIFO). Epoch tokens change
    /// rarely; the bound keeps the lineage finite.
    static let retiredEpochLimit = 4

    /// Distinct-sample history sizes per candidate — exposed read-only for
    /// diagnostics and tests.
    var distinctSampleCounts: [RouteCandidateKind: Int] {
        sampleHistory.mapValues { $0.count }
    }

    // MARK: Sample validation (defect 7)

    /// Validation verdict for one candidate sample. Invalid samples are NOT
    /// stored: the candidate keeps whatever prior (valid) evidence it had.
    enum SampleValidity: Equatable {
        case valid
        case invalid(InvalidReason)

        enum InvalidReason: String, Equatable {
            case nonFinite   // NaN/Inf in any numeric field
            case negative    // negative rtt/jitter/queue term
            case outOfRange  // loss/reorder/confidence outside 0...1
            case futureSkew  // measuredAt beyond maxFutureSkew ahead of now
        }
    }

    /// Numerical hygiene gate for ONE sample. Metrics are never fabricated
    /// as zeros: a rejected sample leaves the candidate's prior evidence
    /// intact and the candidate simply contributes no new measurement.
    func validate(_ sample: RouteCandidateSample, now: Date) -> SampleValidity {
        let msTerms = [sample.rttMs, sample.jitterMs, sample.queueCostMs]
        if msTerms.contains(where: { !$0.isFinite }) { return .invalid(.nonFinite) }
        if msTerms.contains(where: { $0 < 0 }) { return .invalid(.negative) }
        let fractions = [sample.lossFraction, sample.reorderFraction, sample.confidence]
        if fractions.contains(where: { !$0.isFinite }) { return .invalid(.nonFinite) }
        if fractions.contains(where: { $0 < 0 || $0 > 1 }) { return .invalid(.outOfRange) }
        if sample.measuredAt.timeIntervalSince(now) > config.maxFutureSkew {
            return .invalid(.futureSkew)
        }
        return .valid
    }

    // MARK: Scoring

    /// Exact score formula (lower is better):
    ///
    ///     base   = rttMs
    ///            + jitterWeight        × jitterMs             (default 2.0)
    ///            + lossWeightPerPoint  × lossFraction × lossScale
    ///                                                             (default 1200.0)
    ///            + reorderWeight       × reorderFraction      (default 120.0)
    ///            + queueCostWeight     × queueCostMs          (default 1.0; ESTIMATE)
    ///     if !available: base += unavailablePenalty (default 10_000)
    ///     effective = base / max(confidence, confidenceFloor)   (floor 0.25)
    ///
    /// `lossScale` (0...1) is the caller's distinct-sample evidence scale
    /// (defect 8): below `minSamplesForLossDecisions` distinct samples the
    /// loss term is scaled by count/8 so one packet never swings a decision.
    /// Unavailability dwarfs every realistic quality term but stays finite
    /// and ordered; the confidence divisor means a 0.4-confidence candidate
    /// must be >2.5× better in raw terms to tie a fully proven path.
    func effectiveScore(_ sample: RouteCandidateSample, lossScale: Double = 1.0) -> Double {
        // Only MEASURED terms enter the score (build-62): an unknown value
        // is excluded, never credited as zero-cost, and the candidate's
        // confidence is discounted (`dataConfidence`) so a measurable clean
        // path beats an unmeasurable one at equal RTT — while an
        // unmeasurable path can still win on a large real RTT advantage.
        var base = sample.rttMs
        if sample.hasJitterMeasurement {
            base += config.jitterWeight * sample.jitterMs
        }
        if sample.hasLossMeasurement {
            base += config.lossWeightPerPoint * sample.lossFraction * lossScale
            base += config.reorderWeight * sample.reorderFraction
        }
        if sample.hasQueueEstimate {
            base += config.queueCostWeight * sample.queueCostMs
        }
        if !sample.available { base += config.unavailablePenalty }
        return base / max(dataConfidence(sample), config.confidenceFloor)
    }

    /// Confidence actually used for scoring: each term the transport cannot
    /// report scales the sample's confidence down (unknown ≠ good).
    /// 0.8 loss · 0.95 jitter · 0.95 queue — multiplicative, bounded, and
    /// documented here so diagnostics can explain a low effective score.
    func dataConfidence(_ sample: RouteCandidateSample) -> Double {
        var scale = 1.0
        if !sample.hasLossMeasurement { scale *= 0.8 }
        if !sample.hasJitterMeasurement { scale *= 0.95 }
        if !sample.hasQueueEstimate { scale *= 0.95 }
        return sample.confidence * scale
    }

    /// Distinct-sample evidence scale for the loss term (defect 8): below
    /// `minSamplesForLossDecisions` distinct samples the loss term is scaled
    /// by count/8, so one packet never swings a decision. The count comes
    /// from the FIFO-bounded rolling history (fix 3).
    private func lossEvidenceScale(_ kind: RouteCandidateKind) -> Double {
        let count = Double(sampleHistory[kind]?.count ?? 0)
        return min(1, count / Double(config.minSamplesForLossDecisions))
    }

    /// Confidence that a measured loss fraction reflects reality: grows
    /// with the distinct-sample count and decays with sample age.
    private func lossConfidence(_ kind: RouteCandidateKind,
                                sample: RouteCandidateSample,
                                now: Date) -> Double {
        let ageFactor = max(0, 1 - freshnessAge(sample, now: now) / config.sampleFreshnessTTL)
        return lossEvidenceScale(kind) * ageFactor
    }

    /// Degraded gate (defect 8): measured loss above `lossGateFraction`
    /// with sufficient confidence. A degraded candidate may not WIN a
    /// promotion while the incumbent is clean; when every media-proven
    /// candidate is degraded, the least-bad score still wins.
    private func isDegraded(_ kind: RouteCandidateKind,
                            sample: RouteCandidateSample,
                            now: Date) -> Bool {
        sample.lossFraction > config.lossGateFraction
            && lossConfidence(kind, sample: sample, now: now) >= config.lossGateConfidence
    }

    /// The additive score cost attributable to loss (ms-equivalent) —
    /// exposed in decision details as 含丢包代价（估计）. It is a SCORE-TERM
    /// equivalent, never presented as MOS or an absolute audio-delay number.
    private func concealmentCostMs(_ kind: RouteCandidateKind,
                                   sample: RouteCandidateSample) -> Double {
        config.lossWeightPerPoint * sample.lossFraction * lossEvidenceScale(kind)
    }

    /// Freshness age relative to the injected clock, computed from
    /// min(measuredAt, now): a bounded future skew (0...maxFutureSkew) is
    /// tolerated but can never make a sample "fresh forever".
    private func freshnessAge(_ sample: RouteCandidateSample, now: Date) -> TimeInterval {
        max(0, now.timeIntervalSince(sample.measuredAt))
    }

    private func isFresh(_ sample: RouteCandidateSample, now: Date) -> Bool {
        freshnessAge(sample, now: now) <= config.sampleFreshnessTTL
    }

    /// Liveness, separate from score (defect 9): true when the candidate
    /// holds FRESH valid evidence. Silence or missing telemetry makes this
    /// false — it downgrades freshness/confidence only and is never treated
    /// as media failure.
    private func livenessFresh(_ kind: RouteCandidateKind, now: Date) -> Bool {
        guard let sample = lastValidSample[kind] else { return false }
        return isFresh(sample, now: now)
    }

    /// Rule 1: selectable at all this evaluation. A candidate with no valid
    /// sample is "unknown" and excluded — distinct from a present sample
    /// with zero values, and never fabricated as unavailable.
    private func isQualifying(_ sample: RouteCandidateSample, now: Date) -> Bool {
        sample.available
            && sample.mediaReady
            && dataConfidence(sample) >= config.minimumConfidence
            && isFresh(sample, now: now)
    }

    /// Hard failure is driven by EXPLICIT signals ONLY (fix 1): an active
    /// transport-reported mark, or an explicit transport-unavailable report
    /// in the latest valid sample. Missing, aged or out-of-order telemetry
    /// is "unknown"/"stale" — it downgrades confidence and freshness but is
    /// NEVER a failure: a busy UI, missing pongs or quiet audio must never
    /// tear down a working path.
    private func isHardFailed(_ kind: RouteCandidateKind) -> Bool {
        if hardFailedAt[kind] != nil { return true }
        guard let sample = lastValidSample[kind] else { return false }
        return !sample.available
    }

    // MARK: Evidence intake

    /// One evaluation round's intake outcome (fix 4): the evidence cache
    /// AND the score/gain loops must agree on what counts — `evaluate`
    /// scores ONLY intake-accepted, current-context evidence.
    struct IntakeReport {
        /// Candidates whose network epoch genuinely changed this round
        /// (their streaks and evidence counts were fenced).
        var epochChanged: Set<RouteCandidateKind> = []
        /// Candidates whose incoming sample was ACCEPTED into
        /// current-context evidence this round: stored as the freshness
        /// anchor and eligible to feed score/gain. Kinds that were absent,
        /// invalid, out-of-order, or carried a retired/other-context epoch
        /// are NOT included — their streaks cannot grow this round.
        var accepted: Set<RouteCandidateKind> = []
    }

    /// Validates and caches samples; returns the round's intake report.
    /// This is the ONLY mutation `evaluate` performs — adoption state moves
    /// exclusively through confirmSwitch.
    @discardableResult
    private mutating func ingest(_ samples: [RouteCandidateKind: RouteCandidateSample],
                                 now: Date) -> IntakeReport {
        var report = IntakeReport()
        for (kind, sample) in samples {
            guard case .valid = validate(sample, now: now) else { continue }
            // Out-of-order guard (fix 3): an OLDER measurement never regresses
            // the stored freshness/evidence anchor, never triggers the epoch
            // fence, and never clears failure marks. It still counts toward
            // the distinct-sample history when it belongs to the CURRENT
            // epoch (sequence info for loss confidence); an old-context
            // straggler is fully ignored.
            if let stored = lastValidSample[kind], sample.measuredAt < stored.measuredAt {
                if sample.networkEpoch == networkEpoch[kind] {
                    recordSampleID(kind, id: sample.sampleID)
                }
                continue
            }
            if let current = networkEpoch[kind], current != sample.networkEpoch {
                // Retired-context callback (fix 4): the candidate already
                // moved PAST this epoch — a late delivery from an old path
                // context. Rejected for storage, history and scoring no
                // matter how fresh its timestamps look: it never
                // overwrites current-context evidence, never resets
                // streaks, and never lifts a failure fence.
                if retiredEpochs[kind]?.contains(sample.networkEpoch) == true {
                    continue
                }
                // Genuinely NEW epoch (never seen): seed a new context.
                report.epochChanged.insert(kind)
                retireEpoch(kind, current)
                consecutiveGains[kind] = 0
                firstGainAt[kind] = nil
                lastGainSampleID[kind] = nil
                sampleHistory[kind] = []
                // Deliberately NOT clearing hardFailedAt: a path-context
                // change never lifts an active failure fence — only
                // post-failure recovery evidence does (fix 3).
            }
            networkEpoch[kind] = sample.networkEpoch
            lastValidSample[kind] = sample
            recordSampleID(kind, id: sample.sampleID)
            report.accepted.insert(kind)
        }
        return report
    }

    /// Records a superseded epoch in the candidate's bounded retired list
    /// (FIFO eviction at ``retiredEpochLimit``). An epoch evicted from this
    /// window and later seen again is treated as a GENUINELY NEW context —
    /// transports are expected to mint fresh tokens per path change, so a
    /// repeated old token is assumed to be a new transition (documented
    /// ambiguity, resolved in favor of accepting genuine path changes).
    private mutating func retireEpoch(_ kind: RouteCandidateKind, _ epoch: String) {
        var list = retiredEpochs[kind] ?? []
        guard !list.contains(epoch) else { return }
        list.append(epoch)
        if list.count > Self.retiredEpochLimit {
            list.removeFirst(list.count - Self.retiredEpochLimit)
        }
        retiredEpochs[kind] = list
    }

    /// Appends a distinct sample id to the candidate's rolling history,
    /// FIFO-evicting the oldest beyond ``sampleHistoryLimit``.
    private mutating func recordSampleID(_ kind: RouteCandidateKind, id: UInt64) {
        var history = sampleHistory[kind] ?? []
        guard !history.contains(id) else { return }
        history.append(id)
        if history.count > Self.sampleHistoryLimit {
            history.removeFirst(history.count - Self.sampleHistoryLimit)
        }
        sampleHistory[kind] = history
    }

    /// Evidence-only intake seam (coordinator background probes): validates
    /// and caches ONE sample WITHOUT producing a decision — adoption state,
    /// gain streaks and proposals are untouched.
    mutating func noteSample(_ kind: RouteCandidateKind,
                             _ sample: RouteCandidateSample,
                             now: Date) {
        ingest([kind: sample], now: now)
    }

    // MARK: Hard-failure helper

    /// Records a transport-reported hard failure of one candidate (recovery
    /// layer seam — ICE disconnect, socket death, media timeout). The mark
    /// makes the NEXT evaluation demote that candidate through the fast path
    /// (sustained-window, minimum-hold and cooldown are skipped when the
    /// incumbent hard-fails; the replacement only needs to be media-proven).
    /// The mark persists until the candidate presents a fresh, available,
    /// media-proven sample MEASURED AFTER the failure (recovery evidence
    /// must postdate the failure) — a network-epoch change does NOT clear
    /// it (fix 3) — after which the recovered path re-enters through the
    /// normal symmetric switch rule.
    mutating func noteHardFailure(_ kind: RouteCandidateKind, at date: Date) {
        hardFailedAt[kind] = date
    }

    private mutating func clearRecoveredFailureMarks(now: Date) {
        for (kind, sample) in lastValidSample {
            guard let failedAt = hardFailedAt[kind] else { continue }
            if sample.available, sample.mediaReady,
               isFresh(sample, now: now),
               sample.measuredAt > failedAt {
                hardFailedAt.removeValue(forKey: kind)
            }
        }
    }

    // MARK: Evaluation (phase 1 — never adopts)

    /// Evaluates all candidates and returns the route decision. `samples`
    /// holds at most one freshest sample per candidate; candidates absent
    /// from the dictionary are "unknown" — excluded from scoring, never
    /// fabricated as unavailable. The returned decision's `proposed`/
    /// `proposalID` is the single outstanding commit proposal; adoption
    /// happens only via ``confirmSwitch(to:proposalID:at:)``.
    mutating func evaluate(now: Date,
                           samples: [RouteCandidateKind: RouteCandidateSample],
                           forced: RouteCandidateKind?,
                           permitted: Set<RouteCandidateKind>? = nil) -> RouteSelectionDecision {
        // 1. Evidence intake (validation + epoch fence). Only
        //    intake-accepted, current-context evidence may feed score/gain.
        let intake = ingest(samples, now: now)
        let epochChanged = intake.epochChanged

        // 2. Clear failure marks superseded by post-failure recovery
        //    evidence — BEFORE the forced branch, so a just-recovered forced
        //    path is evaluated on its merits, not on a stale mark.
        clearRecoveredFailureMarks(now: now)

        // 3. Forced mode is STRICT — scoring never overrides it — but the
        //    forced candidate must itself be ALIVE (defect 7 fencing).
        if let forced {
            return evaluateForced(forced, now: now, epochChanged: epochChanged)
        }

        // 4. Initial selection: no incumbent yet; the pick is a proposal
        //    until the caller confirms the commit. A pending real-switch
        //    proposal (an earlier initial pick still in flight) is reported
        //    as .switchPending instead of being superseded by probe ticks.
        //    Build-62: a user mode change narrows the PERMITTED set; an
        //    incumbent outside the set anchors nothing (the mode change is
        //    an explicit request — the best permitted candidate is proposed
        //    immediately, and a pending proposal toward an excluded target
        //    is cancelled).
        let incumbentKind: RouteCandidateKind? = self.incumbentKind.flatMap {
            (permitted?.contains($0) ?? true) ? $0 : nil
        }
        guard let incumbent = incumbentKind else {
            if let pending = pendingRealSwitch(now: now) {
                if permitted?.contains(pending.target) ?? true {
                    return RouteSelectionDecision(
                        chosen: pending.target,
                        reason: .switchPending,
                        proposed: nil,
                        proposalID: pending.id,
                        detail: "首选用 \(pending.target.rawValue) 进行中（等待确认）")
                }
                cancelPendingSwitch(reason: "mode change removed the pending target")
            }
            return evaluateInitial(now: now, permitted: permitted)
        }

        let incumbentSample = lastValidSample[incumbent]

        // 5. Incumbent hard-failure: EXPLICIT signals only (fix 1).
        let incumbentHardFailed = isHardFailed(incumbent)

        let incumbentEffective: Double = incumbentSample.map {
            effectiveScore($0, lossScale: lossEvidenceScale(incumbent))
        } ?? .infinity
        let incumbentDegraded = incumbentSample
            .map { isDegraded(incumbent, sample: $0, now: now) } ?? false

        // 6. Gain bookkeeping (rules 1–2) for every non-incumbent candidate.
        //    A gain counts only on a FRESH measurement DISTINCT from the one
        //    that fed the previous gain; a full streak additionally needs
        //    the streak's first→last timestamps to span sustainedMinDuration.
        var bestGainChallenger: (kind: RouteCandidateKind, score: Double)?
        var bestReadyChallenger: (kind: RouteCandidateKind, score: Double)?
        var bestGateBlockedChallenger: (kind: RouteCandidateKind, score: Double)?
        // A round with NO samples at all is not evidence about anyone
        // (defect 9: missing telemetry is never a failure signal): streaks
        // and failure marks are untouched, the incumbent simply holds.
        if !samples.isEmpty {
        for kind in RouteCandidateKind.allCases where kind != incumbent {
            // Build-62: a candidate outside the user-selected permitted set
            // is excluded from scoring and its streak resets.
            guard permitted?.contains(kind) ?? true else {
                consecutiveGains[kind] = 0
                firstGainAt[kind] = nil
                lastGainSampleID[kind] = nil
                continue
            }
            // Fix 4: score/gain ONLY intake-accepted, current-context
            // evidence (the stored anchor — for accepted kinds it IS the
            // incoming sample). Raw input that ingest rejected (invalid,
            // out-of-order, or retired-context) can never grow a streak or
            // win selection here.
            guard intake.accepted.contains(kind),
                  let sample = lastValidSample[kind],
                  isQualifying(sample, now: now) else {
                consecutiveGains[kind] = 0
                firstGainAt[kind] = nil
                lastGainSampleID[kind] = nil
                continue
            }
            let score = effectiveScore(sample, lossScale: lossEvidenceScale(kind))
            let gains = score < incumbentEffective * (1 - config.hysteresisFraction)
            if gains {
                if lastGainSampleID[kind] != sample.sampleID {
                    if let first = firstGainAt[kind],
                       now.timeIntervalSince(first) <= config.sustainedWindowSpan {
                        consecutiveGains[kind, default: 0] += 1
                    } else {
                        consecutiveGains[kind] = 1
                        firstGainAt[kind] = now
                    }
                    lastGainSampleID[kind] = sample.sampleID
                }
                // (The same measurement re-evaluated produces ZERO streak
                // growth — the streak simply holds.)
                if isStreakReady(kind, now: now) {
                    if isDegraded(kind, sample: sample, now: now), !incumbentDegraded {
                        if bestGateBlockedChallenger == nil
                            || score < bestGateBlockedChallenger!.score {
                            bestGateBlockedChallenger = (kind, score)
                        }
                    } else if bestReadyChallenger == nil
                                || score < bestReadyChallenger!.score {
                        bestReadyChallenger = (kind, score)
                    }
                }
                if bestGainChallenger == nil || score < bestGainChallenger!.score {
                    bestGainChallenger = (kind, score)
                }
            } else {
                consecutiveGains[kind] = 0
                firstGainAt[kind] = nil
                lastGainSampleID[kind] = nil
            }
        }

        }

        // 6b. Pending proposal gate (fix 2): while a REAL switch is in
        //     flight, probe ticks keep the carrier and the SAME confirmable
        //     proposalID instead of superseding the coordinator's commit.
        //     Evidence (caches/streaks above) keeps updating. Hard failure
        //     bypasses: real liveness death wins immediately.
        if !incumbentHardFailed, let pending = pendingRealSwitch(now: now) {
            return RouteSelectionDecision(
                chosen: incumbent,
                reason: .switchPending,
                proposed: nil,
                proposalID: pending.id,
                detail: "\(incumbent.rawValue) 保持：切换到 \(pending.target.rawValue) 进行中（等待确认）")
        }

        // 7. Hard-failure fast path: propose the best proven replacement;
        //    dwell/cooldown/streak requirements are bypassed on a dead route.
        if incumbentHardFailed {
            if let replacement = bestQualifiedChallenger(now: now, incumbent: incumbent,
                                                         permitted: permitted) {
                return propose(
                    chosen: replacement.kind,
                    reason: epochChanged.contains(incumbent) ? .epochChange : .hardFailure,
                    proposed: replacement.kind,
                    now: now,
                    detail: "\(incumbent.rawValue) 已失败，切换到 \(replacement.kind.rawValue)："
                        + sampleBasis(replacement.kind, lastValidSample[replacement.kind], now: now))
            }
            return propose(
                chosen: incumbent,
                reason: epochChanged.contains(incumbent) ? .epochChange : .insufficientEvidence,
                proposed: nil,
                now: now,
                detail: "\(incumbent.rawValue) 失败，但没有可用替代路径，保持当前")
        }

        // 8. Normal quality path: a full streak (count + sustained duration)
        //    + dwell + cooldown proposes the switch; the degraded gate holds
        //    a lossy challenger behind a clean incumbent.
        if let challenger = bestReadyChallenger {
            let dwellLeft = remaining(config.minimumHoldInterval, since: incumbentSince, now: now)
            let cooldownLeft = remaining(config.switchCooldown, since: lastSwitchAt, now: now)
            if dwellLeft <= 0, cooldownLeft <= 0 {
                return propose(
                    chosen: challenger.kind,
                    reason: .sustainedGain,
                    proposed: challenger.kind,
                    now: now,
                    detail: "切换到 \(challenger.kind.rawValue)：连续 \(config.sustainedWindow) 次更优，"
                        + sampleBasis(challenger.kind, lastValidSample[challenger.kind], now: now))
            }
            return keepIncumbent(incumbent, now: now,
                                 blocker: dwellLeft > 0
                                     ? "稳定期未到（剩 \(Int(ceil(dwellLeft)))s）"
                                     : "切换冷却中（剩 \(Int(ceil(cooldownLeft)))s）",
                                 reason: epochChanged.isEmpty ? .keptIncumbent : .epochChange)
        }

        // 9. A ready-but-degraded challenger loses to a clean incumbent: the
        //    loss gate is the reason, stated plainly (defect 8).
        if let blocked = bestGateBlockedChallenger,
           let sample = lastValidSample[blocked.kind] {
            let cost = concealmentCostMs(blocked.kind, sample: sample)
            return keepIncumbent(incumbent, now: now,
                                 blocker: "\(blocked.kind.rawValue) 更优但丢包受限"
                                     + "（含丢包代价（估计）\(Int(cost.rounded()))ms）",
                                 reason: epochChanged.isEmpty ? .keptIncumbent : .epochChange)
        }

        // 10. Nothing qualified: explain exactly why (plain, short,
        //     assertable).
        if let challenger = bestGainChallenger {
            return keepIncumbent(incumbent, now: now,
                                 blocker: "\(challenger.kind.rawValue) 更优但证据积累中"
                                     + "（\(consecutiveGains[challenger.kind] ?? 0)/\(config.sustainedWindow) 次）",
                                 reason: epochChanged.isEmpty ? .keptIncumbent : .epochChange)
        }
        // Liveness is separate from score (defect 9): when NO candidate
        // holds fresh evidence the honest answer is "insufficient evidence"
        // — silence is never a hard-failure cascade.
        let anyFresh = RouteCandidateKind.allCases.contains { livenessFresh($0, now: now) }
        if !anyFresh {
            return keepIncumbent(incumbent, now: now,
                                 blocker: "证据不足，保持当前路径",
                                 reason: .insufficientEvidence)
        }
        return keepIncumbent(incumbent, now: now, blocker: "无更优可用路径",
                             reason: epochChanged.isEmpty ? .keptIncumbent : .epochChange)
    }

    // MARK: Two-phase commit (defect 5)

    /// Applies the ADOPTION half of a decision the caller already committed
    /// at the gateway. Succeeds EXACTLY ONCE for the newest outstanding
    /// proposal, while unexpired and matching its target. Any duplicate,
    /// late, superseded or mismatched confirmation returns false and changes
    /// nothing — a failed route commit therefore never starts
    /// cooldown/min-hold clocks, and a late commit callback can never
    /// "confirm" a switch that never happened. Confirming a keep decision
    /// (no proposed change) is a no-op success.
    /// Seeds a CONFIRMED incumbent without a proposal (call-start wiring):
    /// when the coordinator already adopted an audio owner (warm RTC
    /// fastpath, relay bootstrap) before this policy started deciding, the
    /// policy treats it exactly like a confirmed switch — min-hold/cooldown
    /// anchor at `date`, gain streaks start from zero.
    mutating func seedIncumbent(_ kind: RouteCandidateKind, at date: Date) {
        incumbentKind = kind
        incumbentSince = date
        lastSwitchAt = date
        policyGeneration &+= 1
    }

    mutating func confirmSwitch(to kind: RouteCandidateKind,
                                proposalID: UInt64,
                                at date: Date) -> Bool {
        guard let outstanding = outstandingProposal, outstanding.id == proposalID else {
            return false
        }
        guard isUnexpired(outstanding.issuedAt, now: date) else { return false }
        // The confirmed kind must match the proposal: real proposals confirm
        // their proposed target; keep decisions confirm only the incumbent.
        let target = outstanding.proposed ?? incumbentKind
        guard target == kind else { return false }
        outstandingProposal = nil
        guard let adopted = outstanding.proposed, adopted != incumbentKind else {
            return true // keep decision: no-op success
        }
        incumbentKind = adopted
        incumbentSince = date
        lastSwitchAt = date
        consecutiveGains = [:]
        firstGainAt = [:]
        lastGainSampleID = [:]
        policyGeneration &+= 1
        return true
    }

    /// Coordinator seam (fix 2): abandons an in-flight switch — the route
    /// commit failed or was rolled back. Clears the outstanding proposal
    /// WITHOUT applying adoption, so cooldown/min-hold clocks are never
    /// started by a switch that never happened. No-op when no real switch
    /// is pending (keep proposals need no cancellation).
    mutating func cancelPendingSwitch(reason: String) {
        guard let outstanding = outstandingProposal,
              let target = outstanding.proposed,
              target != incumbentKind else { return }
        outstandingProposal = nil
        lastPendingCancelReason = reason
    }

    // MARK: Pending-proposal helpers

    /// The outstanding proposal when it is a REAL switch (proposed != nil
    /// and != incumbent) still inside its expiry window. While one exists,
    /// non-forced evaluations keep the carrier instead of superseding the
    /// coordinator's in-flight commit.
    private func pendingRealSwitch(now: Date) -> (id: UInt64, target: RouteCandidateKind)? {
        guard let outstanding = outstandingProposal,
              let target = outstanding.proposed,
              target != incumbentKind,
              isUnexpired(outstanding.issuedAt, now: now)
        else { return nil }
        return (outstanding.id, target)
    }

    /// Lazy expiry anchor shared by confirmation and the pending gate: no
    /// hidden timers anywhere (deterministic, injected clock only).
    private func isUnexpired(_ issuedAt: Date, now: Date) -> Bool {
        let elapsed = now.timeIntervalSince(issuedAt)
        return elapsed >= 0 && elapsed <= config.proposalExpiryInterval
    }

    // MARK: Private decision helpers

    private func remaining(_ interval: TimeInterval, since: Date?, now: Date) -> TimeInterval {
        guard let since else { return -.infinity }
        return interval - now.timeIntervalSince(since)
    }

    private func isStreakReady(_ kind: RouteCandidateKind, now: Date) -> Bool {
        guard let first = firstGainAt[kind],
              (consecutiveGains[kind] ?? 0) >= config.sustainedWindow
        else { return false }
        let span = now.timeIntervalSince(first)
        return span >= config.sustainedMinDuration
            && span <= config.sustainedWindowSpan
    }

    /// Issues the decision and records it as the single confirmable
    /// proposal. Real-switch proposals are NOT superseded by probe ticks:
    /// while unexpired they gate re-evaluation (.switchPending) until
    /// confirmed, cancelled or expired; keep proposals may be superseded
    /// freely (nothing is in flight).
    private mutating func propose(chosen: RouteCandidateKind,
                                  reason: RouteSelectionReason,
                                  proposed: RouteCandidateKind?,
                                  now: Date,
                                  detail: String) -> RouteSelectionDecision {
        proposalCounter &+= 1
        outstandingProposal = (id: proposalCounter, proposed: proposed, issuedAt: now)
        return RouteSelectionDecision(chosen: chosen, reason: reason,
                                      proposed: proposed, proposalID: proposalCounter,
                                      detail: detail)
    }

    private mutating func keepIncumbent(_ incumbent: RouteCandidateKind,
                                        now: Date,
                                        blocker: String,
                                        reason: RouteSelectionReason = .keptIncumbent) -> RouteSelectionDecision {
        propose(chosen: incumbent, reason: reason, proposed: nil, now: now,
                detail: "\(incumbent.rawValue) 保持：\(blocker)")
    }

    /// Best PROVEN (rule 1) challenger, lowest effective score first. Used by
    /// the hard-failure fast path: on a dead incumbent the quality comparison
    /// is meaningless (any finite, fresh, media-proven path beats a failed
    /// one — same reachability-first contract as the hot-standby scorer), so
    /// no rule-2 advantage gate applies here.
    private func bestQualifiedChallenger(now: Date,
                                         incumbent: RouteCandidateKind,
                                         permitted: Set<RouteCandidateKind>? = nil)
        -> (kind: RouteCandidateKind, score: Double)? {
        var best: (kind: RouteCandidateKind, score: Double)?
        for kind in RouteCandidateKind.allCases where kind != incumbent {
            guard permitted?.contains(kind) ?? true else { continue }
            guard let sample = lastValidSample[kind], isQualifying(sample, now: now) else { continue }
            let score = effectiveScore(sample, lossScale: lossEvidenceScale(kind))
            if best == nil || score < best!.score { best = (kind, score) }
        }
        return best
    }

    private mutating func evaluateForced(_ forced: RouteCandidateKind,
                                         now: Date,
                                         epochChanged: Set<RouteCandidateKind>) -> RouteSelectionDecision {
        // Defect-7 fence: a forced candidate must itself be ALIVE — fresh
        // valid evidence, media-proven, transport-reported available, no
        // active failure mark, and unchanged network context. Otherwise the
        // decision is forcedUnavailable and the INCUMBENT is kept (or the
        // relay is proposed as the safe default): a forced-but-dead
        // candidate is never promoted.
        let sample = lastValidSample[forced]
        let usable = sample.map {
            isFresh($0, now: now)
                && $0.mediaReady
                && $0.available
                && hardFailedAt[forced] == nil
                && !epochChanged.contains(forced)
        } ?? false
        if usable {
            let alreadyIncumbent = incumbentKind == forced
            return propose(
                chosen: forced,
                reason: .forcedMode,
                proposed: alreadyIncumbent ? nil : forced,
                now: now,
                detail: "强制选择 \(forced.rawValue)"
                    + (alreadyIncumbent ? "（当前路径）" : "")
                    + (sample.map { "，\(sampleBasis(forced, $0, now: now))" } ?? ""))
        }
        if let incumbent = incumbentKind {
            return propose(
                chosen: incumbent,
                reason: .forcedUnavailable,
                proposed: nil,
                now: now,
                detail: "强制的 \(forced.rawValue) 不可用，保持 \(incumbent.rawValue)")
        }
        // No incumbent yet and the forced path is unusable: the relay stays
        // the safe default CARRIER only — proposed is nil (strict mode): a
        // forced direct/QUIC startup must never ADOPT relay audio as a
        // policy switch. The coordinator shows the forced target as
        // pending/failed and retries it within the recovery budget; the
        // WSS control plane stays up regardless.
        let fallback: RouteCandidateKind = .wssRelay
        return propose(
            chosen: fallback,
            reason: .forcedUnavailable,
            proposed: nil,
            now: now,
            detail: "强制的 \(forced.rawValue) 不可用，中继仅作临时承载，未采纳")
    }

    private mutating func evaluateInitial(now: Date,
                                          permitted: Set<RouteCandidateKind>? = nil) -> RouteSelectionDecision {
        var best: (kind: RouteCandidateKind, score: Double)?
        for kind in RouteCandidateKind.allCases {
            guard permitted?.contains(kind) ?? true else { continue }
            guard let sample = lastValidSample[kind], isQualifying(sample, now: now) else { continue }
            let score = effectiveScore(sample, lossScale: lossEvidenceScale(kind))
            if best == nil || score < best!.score { best = (kind, score) }
        }
        if let best {
            return propose(
                chosen: best.kind,
                reason: .initialBest,
                proposed: best.kind,
                now: now,
                detail: "首选用 \(best.kind.rawValue)："
                    + sampleBasis(best.kind, lastValidSample[best.kind], now: now))
        }
        // Relay is the safe default incumbent even with no proven candidate;
        // a call always has a carrier. `insufficientEvidence` keeps the
        // recovery layer honest about the lack of proof.
        let fallback: RouteCandidateKind = .wssRelay
        return propose(
            chosen: fallback,
            reason: .insufficientEvidence,
            proposed: fallback,
            now: now,
            detail: "证据不足，中继兜底")
    }

    /// Short plain-language quality basis for a sample: "rtt 180ms
    /// 抖动 8ms 丢包 0.8%（置信 75%），含丢包代价（估计）96ms". The
    /// concealment figure is the SCORE-TERM loss contribution
    /// (ms-equivalent, 估计); queue/backlog stays omitted — presenters that
    /// show it must carry the "（估计）" suffix themselves.
    private func sampleBasis(_ kind: RouteCandidateKind,
                             _ sample: RouteCandidateSample?,
                             now: Date) -> String {
        guard let sample else { return "无样本" }
        let loss = String(format: "%.1f%%", sample.lossFraction * 100)
        let confidence = String(format: "%.0f%%", sample.confidence * 100)
        var basis = "rtt \(Int(sample.rttMs.rounded()))ms 抖动 \(Int(sample.jitterMs.rounded()))ms"
            + " 丢包 \(loss)（置信 \(confidence)）"
        let concealment = concealmentCostMs(kind, sample: sample)
        if concealment > 0 {
            basis += "，含丢包代价（估计）\(Int(concealment.rounded()))ms"
        }
        if isDegraded(kind, sample: sample, now: now) {
            basis += "，丢包受限"
        }
        return basis
    }
}
