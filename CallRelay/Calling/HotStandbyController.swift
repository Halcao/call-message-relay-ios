import Foundation

/// HotStandbyController (build 50): keeps the NON-primary media transport
/// warm as a hot standby and executes FENCED, make-before-break promotions.
///
/// The gateway mirrors the downlink mix onto the standby transport, so the
/// promotion itself needs no connection setup — the state machine here owns
/// the client side: candidate freshness proof (real frames, never
/// CONNECTED/RTT alone), the fenced promote command (token + expected epoch
/// + idempotent nonce), the single-owner audio handover through the
/// coordinator's existing staged-promotion machinery, and bounded rollback
/// to the still-warm demoted standby on failure.
///
/// Honest gap statement (coordinator review): connection reuse does NOT
/// prove zero audio gap — the gateway swaps the mix source before the
/// client ack and the audio-owner transfer takes real time. The automated
/// evidence this controller produces is: exactly ONE render-source switch,
/// stale-queue discard at promotion (no replayed frames), paced frame IDs
/// continuing on the promoted transport, and a measured switch window in
/// frames. Audibility remains the physical phone gate.
@MainActor
final class HotStandbyController {

    enum RouteKind: String, Equatable {
        case relay, direct
    }

    /// The gateway-facing fencing values of one standby installation.
    struct StandbyFence: Equatable {
        let kind: RouteKind
        var token: String
        var epoch: UInt64
    }

    enum PromotionOutcome: Equatable {
        case promoted(newPrimary: RouteKind)
        case rolledBack(reason: String)
        case unavailable(String)
    }

    /// One promotion's measured evidence (fake-paced-frame-ID driven in
    /// tests; production fills it from the real transports).
    struct TransitionEvidence: Equatable {
        var lastPrimaryFrameID: Int64 = -1
        var firstNewPrimaryFrameID: Int64 = -1
        var switchedAt: Date?
        var renderSourceSwitchCount: Int = 0
        /// Frames dropped at the live edge by the stale-queue discard
        /// (firstNew - lastPrimary - 1 when positive).
        var droppedAtPromotion: Int64 { max(0, firstNewPrimaryFrameID - lastPrimaryFrameID - 1) }
    }

    // MARK: Collaborator surface (the coordinator implements these)

    struct Callbacks {
        /// Current route the call is pinned to.
        let currentPrimary: () -> RouteKind
        /// Freshness proof for the standby of the given kind: true only when
        /// a GENUINE media frame arrived within the freshness window (silent
        /// PCM counts; CONNECTED/RTT never do).
        let standbyFresh: (RouteKind) -> Bool
        /// Executes the fenced promote command. Returns the acked epoch, or
        /// nil when the command failed/rejected (caller rolls back or
        /// refreshes).
        let promoteCommand: (StandbyFence, String) async -> UInt64?
        /// Reconciles against the gateway after a lost/ambiguous ack.
        let refreshServerRoles: () async -> (primary: RouteKind, epoch: UInt64)?
        /// Audio-owner handover, make-before-break: keeps the old path until
        /// the new graph provably runs; returns false when the transfer
        /// failed and the old owner was restored.
        let transferAudioOwnership: (RouteKind) async -> Bool
        /// Demotes the current primary into the standby role (never closes
        /// it): the demoted transport keeps receiving the downlink mirror.
        let demoteCurrentToStandby: () async -> StandbyFence?
        /// True when BOTH transports are healthy enough for a standby to be
        /// maintained (network permits); e.g. not on a degraded single path.
        let canMaintainStandby: () -> Bool
        /// Publishes UI state.
        let onState: (HotStandbyState) -> Void
    }

    struct HotStandbyState: Equatable {
        var primary: RouteKind
        var standby: RouteKind?
        var standbyFresh: Bool
        var standbyRTTMs: Double?
        var promotionInFlight: Bool
        var lastOutcome: PromotionOutcome?
    }

    private let callbacks: Callbacks
    private(set) var fences: [RouteKind: StandbyFence] = [:]
    private(set) var evidence = TransitionEvidence()
    private(set) var promotionInFlight = false
    private var nonceCounter: UInt64 = 0
    /// Bumped on invalidate(); every async boundary re-checks it so a late
    /// ack from a dead call can never operate a new call (coordinator
    /// review). The coordinator invalidates at teardown/generation change.
    /// Invalidation is PERMANENT: once invalidated, no future promote call
    /// is accepted at all (the fences and the in-flight flag are cleared).
    private(set) var invalidationEpoch: UInt64 = 0
    private(set) var invalidated = false

    func invalidate() {
        invalidationEpoch &+= 1
        invalidated = true
        fences.removeAll()
        promotionInFlight = false
    }

    /// Advisor thresholds (composite rule per coordinator review: RTT alone
    /// is NOT the decision — jitter, backlog and freshness matter, with a
    /// cooldown the current-primary failure bypasses).
    var materialAdvantage: Double = 0.20
    var minimumDwell: TimeInterval = 6
    var cooldown: TimeInterval = 30
    private var lastPromotionAt: Date?
    private var advantageSince: Date?

    init(callbacks: Callbacks) {
        self.callbacks = callbacks
    }

    // MARK: Fencing

    func installStandby(_ fence: StandbyFence) {
        fences[fence.kind] = fence
    }

    func removeStandby(_ kind: RouteKind) {
        fences[kind] = nil
    }

    private func nextNonce() -> String {
        nonceCounter += 1
        return "hs-\(nonceCounter)-\(UUID().uuidString)"
    }

    // MARK: Auto choice (composite scorer)

    /// Per-path quality snapshot. RTT is the dominant term but jitter,
    /// backlog and freshness/stability all count (coordinator review).
    struct PathQuality: Equatable {
        var rttMs: Double
        var jitterMs: Double
        var backlogMs: Double
        var freshnessAgeMs: Double
        var stable: Bool

        /// Lower is better. Weights: RTT 0.5, jitter 0.25, backlog 0.15,
        /// freshness 0.10. An UNSTABLE path is never materially better.
        func score() -> Double {
            guard stable else { return .infinity }
            return 0.5 * rttMs + 0.25 * jitterMs + 0.15 * backlogMs + 0.10 * freshnessAgeMs
        }

        /// No real measurement available (coordinator review): unknown is
        /// UNSTABLE, never a fabricated number.
        static let unknown = PathQuality(rttMs: 0, jitterMs: 0, backlogMs: 0,
                                         freshnessAgeMs: 0, stable: false)
    }

    /// Pure promotion decision: the standby may promote only on a sustained
    /// material composite advantage (dwell) outside cooldown — UNLESS the
    /// current primary has failed (failure bypasses the advantage gate, the
    /// dwell AND the cooldown: any finite, fresh standby beats a dead route).
    func shouldPromote(standby: PathQuality,
                       primary: PathQuality,
                       primaryFailed: Bool,
                       now: Date = Date()) -> Bool {
        let s = standby.score()
        guard s.isFinite else { return false }
        if primaryFailed {
            advantageSince = nil
            return true
        }
        let p = primary.score()
        guard p.isFinite, s < p * (1 - materialAdvantage) else {
            advantageSince = nil
            return false
        }
        if advantageSince == nil { advantageSince = now; return false }
        guard now.timeIntervalSince(advantageSince!) >= minimumDwell else { return false }
        if let last = lastPromotionAt, now.timeIntervalSince(last) < cooldown { return false }
        return true
    }

    // MARK: Promotion state machine (make-before-break)

    /// Attempts ONE fenced promotion of the given standby to primary.
    /// Sequencing: fresh proof -> fenced command -> ack -> ownership
    /// transfer -> demote old primary to standby. Any failure rolls back to
    /// the still-healthy (or still-warm) current route. The scorer
    /// (`shouldPromote`) gates AUTOMATIC promotions; a forced route or a
    /// primary failure calls this directly.
    func promoteStandby(_ kind: RouteKind) async -> PromotionOutcome {
        guard !invalidated else { return .unavailable("invalidated") }
        guard !promotionInFlight else { return .unavailable("promotion already in flight") }
        guard var fence = fences[kind] else { return .unavailable("no standby") }
        guard callbacks.standbyFresh(kind) else {
            return .unavailable("standby media not fresh (CONNECTED/RTT is not proof)")
        }
        promotionInFlight = true
        evidence.renderSourceSwitchCount = 0
        let epochAtStart = invalidationEpoch

        let nonce = nextNonce()
        let commandEpoch = await callbacks.promoteCommand(fence, nonce)
        // Identity fence across the await: a hangup/new call/teardown while
        // the HTTP promote was pending must make this attempt inert.
        guard epochAtStart == invalidationEpoch else { return .unavailable("invalidated") }
        guard let newEpoch = commandEpoch else {
            // The command was rejected or its ack was lost. Reconcile
            // BEFORE any client-side change: the swap may have landed
            // server-side while the ack died.
            if let roles = await callbacks.refreshServerRoles(), roles.primary == kind {
                guard epochAtStart == invalidationEpoch else { return .unavailable("invalidated") }
                fence.epoch = roles.epoch
                return await completePromotion(kind: kind, fence: fence, epochAtStart: epochAtStart)
            }
            promotionInFlight = false
            recordPromotionFailure()
            publish()
            return .rolledBack(reason: "promote command rejected")
        }
        fence.epoch = newEpoch
        return await completePromotion(kind: kind, fence: fence, epochAtStart: epochAtStart)
    }

    private func completePromotion(kind: RouteKind, fence: StandbyFence, epochAtStart: UInt64) async -> PromotionOutcome {
        // Make-before-break: the current primary keeps carrying audio until
        // the candidate's graph provably runs; a failed transfer restores
        // the old owner (the coordinator's staged machinery owns that).
        let transferred = await callbacks.transferAudioOwnership(kind)
        // Identity fence across the transfer await.
        guard epochAtStart == invalidationEpoch else { return .unavailable("invalidated") }
        guard transferred else {
            promotionInFlight = false
            recordPromotionFailure()
            publish()
            return .rolledBack(reason: "audio ownership transfer failed; old owner restored")
        }
        evidence.renderSourceSwitchCount += 1
        // Demote the old primary into the standby slot (kept warm). The
        // identity fence is UNCONDITIONAL (not inside the non-nil branch):
        // an invalidation during the demote await must abort the promotion
        // even when the demote produced no fence — never reaching the
        // fence removal or the published `.promoted` (build-65).
        let demotedFence = await callbacks.demoteCurrentToStandby()
        guard epochAtStart == invalidationEpoch else { return .unavailable("invalidated") }
        if let demotedFence {
            installStandby(demotedFence)
        }
        // The promoted fence belongs to the PRIMARY now: never keep a
        // standby fence for the carrier (coordinator review).
        fences.removeValue(forKey: kind)
        lastPromotionAt = Date()
        let outcome = PromotionOutcome.promoted(newPrimary: kind)
        // Final publish only AFTER the in-flight flag clears so the UI
        // never shows a settled promotion still marked in flight.
        promotionInFlight = false
        publish(outcome)
        return outcome
    }

    /// Build-51: a promotion that ran and FAILED blocks AUTOMATIC
    /// re-promotion for the standard cooldown and resets the advantage
    /// dwell. Without this the cadence loop re-attempted every ~5 s after a
    /// failed local transfer (field build: 7 suspend/resume cutouts in 5 s).
    /// The manual path re-enters through `promoteStandby` directly, which
    /// never consults the cooldown (explicit fresh intent), and the
    /// primary-failure bypass in `shouldPromote` is checked BEFORE the
    /// cooldown, so a genuinely dead primary still takes over immediately.
    private func recordPromotionFailure() {
        lastPromotionAt = Date()
        advantageSince = nil
    }

    /// Called by the transports on render-source switches and paced frames
    /// so the transition evidence is real, not asserted.
    func recordPrimaryFrame(id: Int64) {
        evidence.lastPrimaryFrameID = id
    }

    func recordNewPrimaryFirstFrame(id: Int64) {
        if evidence.switchedAt == nil { evidence.switchedAt = Date() }
        evidence.firstNewPrimaryFrameID = id
    }

    /// Re-publishes the current snapshot (freshness changes call this so
    /// the UI sees standby fresh/stale transitions without a promotion).
    func refreshState() { publish() }

    private func publish(_ outcome: PromotionOutcome? = nil) {
        let primary = callbacks.currentPrimary()
        let standbyKind = fences.keys.first(where: { $0 != primary })
        callbacks.onState(HotStandbyState(
            primary: primary,
            standby: standbyKind,
            standbyFresh: standbyKind.map { callbacks.standbyFresh($0) } ?? false,
            standbyRTTMs: nil,
            promotionInFlight: promotionInFlight,
            lastOutcome: outcome
        ))
    }
}
