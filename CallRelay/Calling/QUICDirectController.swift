import Foundation
import Network

/// Build-62 resolved user routing mode: `MediaRouteMode` (自动/直连/中继)
/// plus the direct sub-preference, mapped onto the policy surface. The
/// coordinator's seam returns this; the policy never sees raw stores.
struct RouteModeMapping: Equatable, Sendable {
    /// Non-nil ⇒ STRICT forced mode (relay-only, or one specific direct
    /// transport). Scoring never overrides it; an unusable forced choice
    /// keeps the incumbent honestly (forcedUnavailable).
    let forcedKind: RouteCandidateKind?
    /// Non-nil together with `forcedKind == nil` ⇒ the permitted candidate
    /// set (the direct family for 直连+自动子偏好). Relay is NEVER in it;
    /// the incumbent outside the set anchors nothing (a user mode change is
    /// an explicit request, not a quality signal).
    let permittedSet: Set<RouteCandidateKind>?
    /// 自动： full scoring, no forcing.
    static let auto = RouteModeMapping(forcedKind: nil, permittedSet: nil)
    /// 强制 QUIC 直连 (strict).
    static let quic = RouteModeMapping(forcedKind: .quicDirect, permittedSet: nil)
    /// 强制现有直连 WebRTC (strict).
    static let webRTC = RouteModeMapping(forcedKind: .webRTCDirect, permittedSet: nil)
    /// 强制中继 (strict; WSS control allowed, direct audio never selected).
    static let relay = RouteModeMapping(forcedKind: .wssRelay, permittedSet: [.wssRelay])
    /// 直连 + 自动子偏好： the direct family only, scored on real evidence.
    static let directFamily = RouteModeMapping(
        forcedKind: nil, permittedSet: [.quicDirect, .webRTCDirect])
}

/// Build-61 QUIC direct-path orchestration for ONE call (protocol
/// `gateway/docs/quic61/protocol.md`): owns the single QUIC standby attempt
/// (bounded ~8 s, truthful nil on ANY failure — the incumbent is never
/// touched), the per-call ``MultiPathAdvisor`` evaluation tick (1 s cadence,
/// started only after BOTH the WSS `ready transport=quic` greeting has been
/// applied AND ≥1 probe sample exists), the two-phase propose→promote→confirm
/// flow through the coordinator seam, and the hard-failure demotion signal.
///
/// The controller NEVER fabricates evidence: QUIC-direct samples come only
/// from the socket's own probe/playback telemetry, WSS-relay samples only
/// from the incumbent's ping/gateway-buffer telemetry, and webRTCDirect is
/// simply absent (no source → unknown → excluded from scoring). Queue-derived
/// numbers are passed through as the policy's ESTIMATE input; presenters
/// label them "（估计）".
@MainActor
final class QUICDirectController {
    /// The coordinated inputs of one QUIC dial attempt: the gateway
    /// candidate from the punch POST, the single-use bind token, and the
    /// phone's own STUN-mapped local endpoint (nil when STUN was
    /// unreachable — the dial still runs, just without the punched port).
    struct QUICAttemptResult {
        let candidate: HostPort
        let bindToken: String
        let localEndpoint: NWEndpoint?
    }

    /// Overall bound for ONE attempt (protocol §8: dial + bind bursts
    /// bounded ~8 s, never blocking the answer).
    var attemptTimeout: TimeInterval = 8
    /// Evaluation cadence once the path is decision-ready (production 1 s;
    /// tests shorten it).
    var evaluationTickInterval: TimeInterval = 1

    // MARK: Seams (coordinator-injected)

    /// The incumbent WSS relay (ping samples + the QUIC control plane).
    private let wsMedia: () -> WebSocketCallMedia?
    /// The resolved user routing mode (build-62: MediaRouteMode + direct
    /// sub-preference mapped by the coordinator; relay forces WSS, direct
    /// forces the direct family — relay is never selected — auto scores
    /// everything). NOT a raw store read: the mapping lives in the seam.
    private var preferredTransport: () -> RouteModeMapping
    /// The coordinator's atomic make-before-break promotion seam
    /// (QUIC-specific: stage → fenced WSS promote command → reply).
    private let promote: (UInt64) async -> Bool
    /// REAL WebRTC-direct evidence (nil = no proven RTC stats; NEVER
    /// fabricated). Fed by the coordinator from the ACTIVE media session
    /// AND the warm standby probe so ONE authority decides across
    /// RTC/QUIC/WSS from every primary.
    var webRTCSample: () -> RouteCandidateSample? = { nil }
    /// Build-62: REAL relay evidence from the hot-standby relay socket when
    /// `wsMedia` is not the carrier (RTC/QUIC primary). nil = no standby
    /// relay telemetry.
    var relayStandbySample: () -> RouteCandidateSample? = { nil }
    /// Target-specific commits for NON-QUIC proposals (relay/direct).
    /// Each is the coordinator's real adoption path (make-before-break);
    /// false = rolled back, the proposal is cancelled, nothing adopts.
    var adoptRelay: (@Sendable () async -> Bool) = { false }
    var adoptDirect: (@Sendable () async -> Bool) = { false }
    /// Injectable clock (tests advance time; production is `Date()`).
    var now: () -> Date = Date.init
    /// `AppModel.publishRouteSelection` (decision + samples + clock).
    private let publish: (RouteSelectionDecision, [RouteCandidateKind: RouteCandidateSample], Date) -> Void
    /// QUIC media construction (tests inject a fake socket/graph).
    private let mediaFactory: @MainActor () -> QUICCallMedia
    /// Build-63 coordinator seam: re-runs the coordinated punch+dial with
    /// the SAME network epoch after a QUIC failure. nil disables retry
    /// (tests). Truthful false = the retry itself failed.
    var retryAttempt: (@Sendable () async -> Bool)?
    /// Retry backoff bounds: first retry after 2 s, doubling to the 15 s
    /// cap; the cadence re-arms after every failed retry while the call
    /// lives (a quarantined candidate must keep getting honest chances to
    /// re-qualify — silence is never a permanent sentence).
    var quicRetryInitialBackoff: TimeInterval = 2
    var quicRetryMaxBackoff: TimeInterval = 15

    // MARK: State

    /// The coordinated attempt bundle assembled by the coordinator BEFORE
    /// `start` runs (candidate + bind token + STUN local endpoint).
    var attempt: QUICAttemptResult?

    private(set) var quicCallMedia: QUICCallMedia?
    /// The standby instance token from the WSS `ready transport=quic`
    /// greeting (pins this exact standby in the fenced promote command).
    private(set) var standbyToken: String?
    /// Newest WSS greeting received before connect() completed (races
    /// the bind proof); applied the moment the media exists.
    private var bufferedGreeting: WSMediaControl?
    /// Policy configuration (tests shrink the sustained-evidence windows so
    /// the deterministic streak rules remain exercised without real-time
    /// waits; production uses the defaults).
    var policyConfig = RouteSelectionPolicy.Config()
    private lazy var advisor = MultiPathAdvisor(config: policyConfig)
    private var evaluateTimer: Timer?
    /// True once the WSS greeting's codec/epoch have been applied.
    private var transportReady = false
    /// True once a genuine inbound frame proved fresh media (protocol §3
    /// client-side readiness rule: greeting AND real downlink audio).
    private var mediaProven = false
    /// True once the advisor CONFIRMED quicDirect as the audio owner.
    private var confirmedOwner = false
    private var closed = false
    /// Serializes promotion execution (one commit in flight at a time).
    private var promoting = false
    private var networkEpoch = ""
    /// Build-62: the last resolved user mode; a CHANGE cancels any pending
    /// commit proposal (request-generation invalidation) so a stale commit
    /// can never confirm after the user picked something else.
    private var lastPreferredMode: RouteModeMapping?
    /// DEBUG test seam state (see `debugSuspendEvaluationTimerForTest`);
    /// always false in production.
    private var evaluationSuspendedForTest = false
    /// Distinct-measurement ids: a new id is minted only when the freshest
    /// probe/ping CHANGES, so re-evaluating one cached measurement never
    /// grows a sustained-gain streak (policy defect 6).
    private var lastProbeAt: Date?
    private var probeSampleID: UInt64 = 0
    private var lastPingAt: Date?
    private var relaySampleID: UInt64 = 0
    /// Build-63 bounded retry state (quarantine after a failure).
    private var quicRetryTask: Task<Void, Never>?
    private var quicRetryBackoff: TimeInterval?
    /// True while a dial (initial attempt or bounded retry) is in flight: the
    /// retry cadence must never start a SECOND concurrent dial.
    private var dialInFlight = false
    /// Build-66: the next dial must mint a FRESH network epoch (a path
    /// generation change happened; old-generation samples must not be able to
    /// mark the new path decision-ready).
    private var mintFreshEpochOnNextDial = false
    #if DEBUG
    /// Test seam: how many retry attempts have been scheduled.
    private(set) var debugRetryCount = 0
    #endif

    /// The authority's adoption truth: QUIC is the CONFIRMED audio owner.
    /// Build-66 production media-health seam (the stall watchdog must never
    /// treat a demoted/standby QUIC socket as the call's carrier).
    var ownsAudio: Bool { confirmedOwner }

    /// Fires when the CONFIRMED QUIC primary dies: the coordinator falls
    /// back through the existing bounded recovery. A standby/staged death
    /// never fires this (the incumbent relay is healthy — silence is not
    /// failure).
    var onPrimaryDeath: (() -> Void)?

    init(wsMedia: @escaping () -> WebSocketCallMedia?,
         preferredTransport: @escaping () -> RouteModeMapping,
         promote: @escaping (UInt64) async -> Bool,
         adoptRelay: (@Sendable () async -> Bool)? = nil,
         adoptDirect: (@Sendable () async -> Bool)? = nil,
         rtcSample: (() -> RouteCandidateSample?)? = nil,
         relayStandby: (() -> RouteCandidateSample?)? = nil,
         publish: @escaping (RouteSelectionDecision, [RouteCandidateKind: RouteCandidateSample], Date) -> Void,
         mediaFactory: @escaping @MainActor () -> QUICCallMedia = { QUICCallMedia() }) {
        self.wsMedia = wsMedia
        self.preferredTransport = preferredTransport
        self.promote = promote
        if let adoptRelay { self.adoptRelay = adoptRelay }
        if let adoptDirect { self.adoptDirect = adoptDirect }
        if let rtcSample { self.webRTCSample = rtcSample }
        if let relayStandby { self.relayStandbySample = relayStandby }
        self.publish = publish
        self.mediaFactory = mediaFactory
    }

    /// Seeds the ALREADY-adopted audio owner (call-start wiring): the
    /// policy anchors min-hold/cooldown on this confirmed adoption, so the
    /// provisional-carrier exemption can never be confused with it.
    func seedInitialOwner(_ kind: RouteCandidateKind) {
        guard advisor.incumbentKind == nil else { return }
        advisor.seedIncumbent(kind, at: now())
    }

    /// The client network epoch of the current attempt (the bounded retry
    /// reuses it; tests inject samples with the same value). Available in
    /// every configuration — the production retry path reads it too.
    var currentNetworkEpoch: String { networkEpoch }

    // MARK: Attempt (protocol §2/§5/§8)

    /// Drives `quicCallMedia.connect(...)` under a bounded overall deadline.
    /// ANY failure returns nil (truthful): the incumbent is untouched and
    /// the advisor records the hard failure so this candidate is demoted.
    /// Success leaves a hot-STANDBY attach: staged, decoding for freshness,
    /// bounded silence uplink pulse, probe sampling running.
    @discardableResult
    func start(
        callId: String,
        deviceId: String,
        api: GatewayAPI,
        pin: QUICPin,
        gatewayCandidate: HostPort,
        bindToken: String,
        networkEpoch: String
    ) async -> QUICCallMedia? {
        startBootstrapCadence()
        guard !closed else { return nil }
        dialInFlight = true
        self.networkEpoch = networkEpoch
        let media = mediaFactory()
        // Staged from birth: the standby attach must never open a second
        // capture graph while the incumbent relay still carries the call.
        media.markAudioStaged(true)
        // A stray activation replay during the standby phase is reported,
        // never fatal (the exclusive promotion owns the failure decision).
        media.graphStartFailureNonfatal = true

        // Bounded overall deadline (the `withSocketTimeout` pattern): the
        // connect's own cancellation handler closes the socket on cancel,
        // so the parked handshake/bind continuations always finish.
        let localEndpoint = attempt?.localEndpoint
        let connectTask = Task { () throws -> Void in
            try await media.connect(
                pin: pin,
                gatewayCandidate: gatewayCandidate,
                localEndpoint: localEndpoint,
                bindToken: bindToken)
        }
        let timeoutTask = Task {
            try? await Task.sleep(nanoseconds: UInt64(attemptTimeout * 1_000_000_000))
            connectTask.cancel()
        }
        do {
            try await connectTask.value
        } catch {
            dialInFlight = false
            timeoutTask.cancel()
            guard !closed else { return nil }
            media.closeWithoutAudio()
            advisor.noteHardFailure(.quicDirect, at: Date())
            // Build-64: the INITIAL trusted dial retries with the bounded
            // backoff too — previously only post-connect state failures
            // re-armed the dial, so a transient handshake/timeout failure
            // at call start quarantined the candidate with no recovery.
            // Invalid trust/capability never reaches this catch (the
            // pre-dial gates abort before start); the retry seam
            // re-verifies capability/trust on every attempt, and the
            // cadence stays bounded (one task, 2 s doubling to the 15 s
            // cap, cancelled by close()).
            scheduleQUICRetry()
            DiagnosticsStore.shared.log(
                "route", "quic attempt failed call=\(AppLog.tag(callId))"
                    + " device=\(AppLog.tag(deviceId)): \(error.localizedDescription)")
            return nil
        }
        timeoutTask.cancel()
        dialInFlight = false
        guard !closed else { media.closeWithoutAudio(); return nil }

        noteQUICRetrySucceeded()
        quicRetryTask?.cancel()
        quicRetryTask = nil
        self.quicCallMedia = media
        if let greeting = bufferedGreeting {
            bufferedGreeting = nil
            handleTransportReady(greeting)
        }
        // READY but not the owner: persistent standby role (no audio
        // ownership, freshness decoding, bounded silence pulse).
        media.markHotStandby()
        media.startProbeSampling()
        media.onInboundRealFrame = { [weak self] in
            Task { @MainActor in self?.markMediaProven() }
        }
        media.onState = { [weak self] state in self?.quicStateChanged(state) }
        AppLog.media.debug("quic standby attached call=\(AppLog.tag(callId))")
        return media
    }

    /// Until the main evaluation tick exists, check readiness at 2 Hz so
    /// relay-pong-first startups evaluate without waiting for QUIC proof.
    private var bootstrapTimer: Timer?
    private func startBootstrapCadence() {
        guard bootstrapTimer == nil else { return }
        let timer = Timer(timeInterval: 0.5, repeats: true) { [weak self] _ in
            Task { @MainActor in
                guard let self else { return }
                if self.evaluateTimer != nil {
                    self.bootstrapTimer?.invalidate()
                    self.bootstrapTimer = nil
                    return
                }
                self.startEvaluationIfReady()
            }
        }
        RunLoop.main.add(timer, forMode: .common)
        bootstrapTimer = timer
    }

    /// WSS pong evidence arrived: wake the evaluation immediately.
    func noteRelayEvidence() {
        startEvaluationIfReady()
    }

    // MARK: WSS control-plane correlation (protocol §3)

    /// Applies the WSS `ready transport=quic` greeting: adopts the
    /// negotiated codec/fmt, binds the epoch (the socket's epoch gate
    /// opens), stores the standby token. A local codec failure kills the
    /// candidate truthfully and keeps the incumbent. Never treated as the
    /// WSS socket's own ready (that routing decision lives in
    /// ``WebSocketCallMedia``).
    func handleTransportReady(_ control: WSMediaControl) {
        guard !closed, control.epoch != nil else { return }
        standbyToken = control.token
        transportReady = true
        // The WSS greeting can race the connect() completion (bind proof
        // arrives over QUIC while the greeting travels WSS): buffer the
        // newest greeting and apply it the moment the media exists.
        guard let media = quicCallMedia else {
            bufferedGreeting = control
            startEvaluationIfReady()
            return
        }
        do {
            try media.applyTransportReady(
                epoch: control.epoch!, codec: control.codec ?? "pcmu", fmt: control.fmt)
        } catch {
            media.closeWithoutAudio()
            advisor.noteHardFailure(.quicDirect, at: Date())
            DiagnosticsStore.shared.log(
                "route", "quic codec adoption failed: \(error.localizedDescription)")
            return
        }
        startEvaluationIfReady()
    }

    /// Binds the epoch from the `promoted` reply (protocol §4: the client
    /// stamps/validates against the ready epoch, updated on promoted).
    func applyPromotedEpoch(_ epoch: UInt64) {
        quicCallMedia?.applyEpoch(epoch)
    }

    /// Build-62: install the FRESH standby identity after this path was
    /// demoted server-side (the promoted ack or a media-state reconcile
    /// carries the new token; the swap consumed the previous one). Without
    /// this a later Q re-promotion would present a stale token and roll
    /// back truthfully — the Q→WSS→Q chain would be one-way.
    func applyStandbyToken(_ token: String, epoch: UInt64) {
        standbyToken = token
        quicCallMedia?.applyEpoch(epoch)
    }

    private func markMediaProven() {
        guard !closed, !mediaProven else { return }
        mediaProven = true
        startEvaluationIfReady()
    }

    private func quicStateChanged(_ state: MediaState) {
        guard !closed else { return }
        switch state {
        case .disconnected, .failed:
            advisor.noteHardFailure(.quicDirect, at: Date())
            // Build-63 ownership-independent failure lifecycle: a
            // standby/staged QUIC death must NEVER freeze selection across
            // the healthy RTC/WSS paths (the old flow invalidated the
            // evaluation tick here and, with the bootstrap cadence already
            // spent, permanently froze the whole authority). The tick
            // keeps running — the quarantined candidate simply stops being
            // proposed until post-failure recovery evidence — and a bounded
            // backoff retry re-arms the dial.
            let wasOwner = confirmedOwner
            confirmedOwner = false
            let deadMedia = quicCallMedia
            quicCallMedia = nil
            transportReady = false
            mediaProven = false
            bufferedGreeting = nil
            standbyToken = nil
            deadMedia?.closeWithoutAudio()
            if wasOwner {
                // The confirmed QUIC primary died: the existing bounded
                // recovery settles the call (never a standby/staged death —
                // the healthy incumbent is not failure).
                onPrimaryDeath?()
            }
            scheduleQUICRetry()
        default:
            break
        }
    }

    // MARK: Bounded quarantine retry (build 63)

    /// Arms the next retry after a failure. The cadence is bounded
    /// (2 s doubling to 15 s) and stops the moment the candidate
    /// reconnects, the call ends, or the seam is absent; at most ONE retry
    /// task exists at any instant (no leaked competing selectors).
    private func scheduleQUICRetry() {
        guard !closed, retryAttempt != nil, quicRetryTask == nil,
              quicCallMedia == nil, !dialInFlight else { return }
        let floor = max(0.2, quicRetryInitialBackoff)
        let cap = max(floor, quicRetryMaxBackoff)
        let current = quicRetryBackoff ?? floor
        let delay = min(current, cap)
        quicRetryBackoff = min(current * 2, cap)
        #if DEBUG
        debugRetryCount += 1
        #endif
        let task = Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
            guard let self else { return }
            self.quicRetryTask = nil
            guard !self.closed, self.quicCallMedia == nil else { return }
            let ok = await self.retryAttempt?() ?? false
            guard !self.closed else { return }
            if !ok {
                // Still quarantined: re-arm the bounded backoff.
                self.scheduleQUICRetry()
            }
        }
        quicRetryTask = task
    }

    /// A retry (or the initial attempt) produced a live socket: reset the
    /// backoff so a LATER failure starts the cadence fresh.
    private func noteQUICRetrySucceeded() {
        quicRetryBackoff = nil
        #if DEBUG
        debugRetryCount = 0
        #endif
    }

    // MARK: Network path changes (build 66)

    /// The client's network path identity changed (Wi-Fi ↔ cellular, an
    /// interface appeared/disappeared, reachability returned). This
    /// generation's QUIC state is now suspect:
    ///  - a live socket that is NO LONGER carrying media is retired through
    ///    the exact socket-death lifecycle (the dial pinned the STUN local
    ///    endpoint to the old interface, so it cannot migrate);
    ///  - a still-fresh socket (frames keep arriving) is left untouched;
    ///  - a quarantined candidate re-dials at the FLOOR of the bounded
    ///    cadence instead of waiting out the old path's 15 s cap;
    ///  - the next dial mints a FRESH network epoch so samples measured on
    ///    the old path can never mark the new one decision-ready.
    /// Never starts a second concurrent dial (an in-flight attempt
    /// self-bounds and owns the retry scheduling).
    func noteNetworkPathChanged(staleGrace: TimeInterval) {
        guard !closed else { return }
        if let media = quicCallMedia, !media.isInboundRealFrameFresh(within: staleGrace) {
            mintFreshEpochOnNextDial = true
            quicStateChanged(.disconnected)
            quicRetryBackoff = nil
            quicRetryTask?.cancel()
            quicRetryTask = nil
            scheduleQUICRetry()
            return
        }
        guard quicCallMedia == nil else {
            // Still carrying real media across the change: keep the socket.
            return
        }
        mintFreshEpochOnNextDial = true
        quicRetryBackoff = nil
        quicRetryTask?.cancel()
        quicRetryTask = nil
        scheduleQUICRetry()
    }

    /// The network epoch the NEXT dial must advertise. A path change mints a
    /// fresh opaque token so the route policy's epoch fence retires every
    /// old-context sample (freshness/streaks reset; a hard-failure fence is
    /// deliberately NOT lifted).
    func nextDialNetworkEpoch() -> String {
        if mintFreshEpochOnNextDial {
            mintFreshEpochOnNextDial = false
            networkEpoch = "quic-\(UUID().uuidString)"
            lastProbeAt = nil
            probeSampleID = 0
        }
        return networkEpoch
    }

    // MARK: Multi-path evaluation (protocol §8)

    /// Starts the 1 s evaluation tick once the WSS greeting has been
    /// applied AND genuine media is proven. The QUIC sample itself only
    /// exists after the first probe echo (makeQUICSample returns nil
    /// before then; evaluateOnce skips while no candidate has any
    /// evidence), so the tick must not wait for the probe — nothing
    /// would ever re-trigger it.
    private func startEvaluationIfReady() {
        // The tick starts when the QUIC path is decision-ready OR as soon
        // as ANY real evidence exists (a relay pong during the provisional
        // bootstrap): startup carrier publishes must not wait for QUIC.
        let quicReady = transportReady && mediaProven
        let anyEvidence = quicCallMedia?.lastProbeSample != nil
            || wsMedia()?.lastPingSample != nil
            || webRTCSample() != nil
        guard !closed, !evaluationSuspendedForTest,
              evaluateTimer == nil, quicReady || anyEvidence else { return }
        evaluateOnce()
        let timer = Timer(timeInterval: evaluationTickInterval, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.evaluateOnce() }
        }
        RunLoop.main.add(timer, forMode: .common)
        evaluateTimer = timer
    }

    private func evaluateOnce(with injected: [RouteCandidateKind: RouteCandidateSample]? = nil) {
        guard !closed else { return }
        // Build-62 forced-mode unification: the closure maps the user's
        // ACTUAL MediaRouteMode + direct sub-preference —
        //   relay  ⇒ forced .wssRelay (strict; never QUIC/RTC)
        //   direct ⇒ forced sub-preference (.quic / .webRTC), or the whole
        //            direct family (.auto sub-preference ⇒ permitted set
        //            {QUIC, RTC}; relay is never selected)
        //   auto   ⇒ no forcing; full scoring
        // A mode CHANGE cancels a pending commit proposal — a user
        // selection is a new request generation — EXCEPT while a commit is
        // actually in flight (build-64): the commit's real outcome must be
        // reflected first (the gateway may already have swapped roles and
        // audio ownership really moved), then the next evaluation
        // reconciles to the user's latest choice through a normal fenced
        // commit. Cancelling mid-flight would quarantine a healthy path
        // that actually adopted and leave the advisor owner stale.
        let mode = preferredTransport()
        if mode != lastPreferredMode {
            if lastPreferredMode != nil, !promoting {
                advisor.cancelPendingSwitch(reason: "user mode changed")
            }
            lastPreferredMode = mode
        }
        guard !promoting else { return }
        let now = self.now()
        var samples: [RouteCandidateKind: RouteCandidateSample] = injected ?? [:]
        if injected == nil {
            if let quicSample = makeQUICSample(now: now) {
                samples[.quicDirect] = quicSample
            }
            if let relaySample = makeRelaySample(now: now) {
                samples[.wssRelay] = relaySample
            } else if let standbyRelay = relayStandbySample() {
                // Build-62: the relay candidate is also measurable from the
                // hot-standby relay socket while RTC/QUIC is primary.
                samples[.wssRelay] = standbyRelay
            }
            if let rtcSample = webRTCSample() {
                samples[.webRTCDirect] = rtcSample
            }
        }
        // Build-62 evidence integrity: each sample KEEPS its real
        // measurement timestamp (`probe.at` / `ping.at` / the RTC stats
        // poll stamp). Re-stamping cached telemetry with the evaluation
        // clock made old readings permanently fresh and defeated the
        // out-of-order/epoch intake fences — freshness must reflect WHEN
        // THE TRANSPORT MEASURED, not when the policy ran. Tests inject
        // coherent clocks (sample measuredAt == evaluation now).
        let decision = advisor.evaluate(now: now, samples: samples, mode: mode)
        #if DEBUG
        debugLastDecisionDetail = decision.detail
        debugLastModeForced = mode.forcedKind
        let src = injected.map { "injected:\($0.keys.map(\.rawValue).sorted().joined(separator: "+"))" } ?? "real"
        debugLastSampleSource = src
        debugEvalHistory.append((source: src, detail: decision.detail))
        if debugEvalHistory.count > 8 { debugEvalHistory.removeFirst() }
        #endif
        if let proposed = decision.proposed, proposed == .wssRelay,
           advisor.incumbentKind == nil {
            // STARTUP-ONLY provisional bootstrap (protocol §8): the relay
            // proposal before ANY confirmed adoption is a carrier, never a
            // commit: adopting it would anchor min-hold on the bootstrap
            // and block the first proven better route. Publish the carrier
            // state; the proposal is cancelled (a later evaluation may
            // initial-select a better path immediately).
            advisor.cancelPendingSwitch(reason: "startup provisional relay carrier")
            confirmedOwner = false
            publish(decision, samples, now)
        } else if let proposed = decision.proposed, proposed != advisor.incumbentKind {
            // A real switch proposal (any target): commit through the
            // TARGET-SPECIFIC coordinator seam BEFORE adopting (two-phase;
            // the policy never adopts on its own).
            promoting = true
            let proposalID = decision.proposalID
            Task { @MainActor [weak self] in
                guard let self else { return }
                let ok: Bool
                switch proposed {
                case .quicDirect:
                    ok = await self.promote(proposalID)
                case .wssRelay:
                    ok = await self.adoptRelay()
                case .webRTCDirect:
                    ok = await self.adoptDirect()
                }
                self.promoting = false
                guard !self.closed else { return }
                if ok, self.advisor.confirmSwitch(
                    to: proposed, proposalID: proposalID, at: self.now()) {
                    self.confirmedOwner = proposed == .quicDirect
                    self.publish(decision, samples, self.now())
                } else if ok {
                    // Build-64 reconciliation: the fenced commit SUCCEEDED
                    // (the gateway swapped roles and audio ownership really
                    // moved) but the proposal was cancelled or expired while
                    // in flight (a user mode change is the request-generation
                    // case). The adopted path is REAL, not a failure:
                    // record the actual owner so the advisor, the exclusive
                    // graphs and the server role agree, then let the next
                    // evaluation reconcile to the user's latest mode through
                    // a normal fenced commit. Quarantining a healthy adopted
                    // path here would freeze the authority on a lie.
                    self.advisor.seedIncumbent(proposed, at: self.now())
                    self.confirmedOwner = proposed == .quicDirect
                    self.publish(decision, samples, self.now())
                    DiagnosticsStore.shared.log(
                        "route", "commit adopted for \(proposed.rawValue)"
                            + " after a request-generation change; reconciling to the latest mode")
                } else {
                    // Rolled back: abandon the proposal and demote the
                    // candidate (recovery evidence re-qualifies it later).
                    self.advisor.cancelPendingSwitch(
                        reason: "commit failed or rolled back for \(proposed.rawValue)")
                    self.advisor.noteHardFailure(proposed, at: self.now())
                }
            }
        } else {
            if decision.reason == .switchPending {
                // The coordinator's commit is in flight: the SAME proposal
                // stays confirmable. Publish the carrier-hold state but
                // NEVER confirm here (only the commit result confirms).
            } else if advisor.incumbentKind != nil || decision.proposed == nil {
                // A keep against a confirmed adoption (or a forced keep):
                // confirming a keep is a no-op success.
                _ = advisor.confirmSwitch(
                    to: decision.chosen, proposalID: decision.proposalID, at: now)
            } else if advisor.incumbentKind == nil {
                // STARTUP-ONLY provisional bootstrap (protocol §8): no
                // confirmed adoption exists yet (relay carries the call as
                // a bootstrap). The relay proposal is NOT adopted — that
                // would anchor the min-hold budget on the provisional
                // carrier and block the first proven better route.
                advisor.cancelPendingSwitch(
                    reason: "startup provisional carrier (no confirmed adoption)")
            }
            // Build-64 ownership truth: a keep decision NEVER re-confirms
            // ownership. confirmedOwner is set only by a successful fenced
            // commit (and cleared by quicStateChanged when the socket
            // dies). Recomputing it from the advisor here resurrected a
            // DEAD incumbent the policy could not yet replace (no
            // media-proven alternative) as the "owner" — a silent fake
            // owner that blocked the recovery flow. Only an explicit move
            // away from QUIC clears the flag.
            if advisor.incumbentKind != .quicDirect {
                confirmedOwner = false
            }
            publish(decision, samples, now)
        }
    }

    /// One QUIC-direct sample from the socket's own telemetry. nil before
    /// BOTH the transport greeting and the first probe sample exist.
    /// `mediaReady` additionally requires genuine inbound audio (protocol
    /// §3): a connected, probed, epoch-gated path is not yet proven media.
    private func makeQUICSample(now: Date) -> RouteCandidateSample? {
        guard transportReady, let media = quicCallMedia,
              let probe = media.lastProbeSample else { return nil }
        let fresh = media.freshProbeSamples(within: 10, now: now)
        if probe.at != lastProbeAt {
            lastProbeAt = probe.at
            probeSampleID &+= 1
        }
        // Build-62: DOWNLINK loss/reorder are REAL receiver-side
        // measurements from the seq16 envelope (the missing transport ACK
        // only hides OUTBOUND delivery — inbound gaps and late arrivals are
        // observable). Unknown before the first framed arrival, never a
        // fabricated zero; silence never fabricates loss (counters only
        // advance on actual arrivals).
        let downlink = media.downlinkQuality
        return RouteCandidateSample(
            rttMs: probe.rtt * 1000,
            jitterMs: (MediaRouteAdvisor.jitter(of: fresh) ?? 0) * 1000,
            lossFraction: downlink.lossFraction,
            reorderFraction: downlink.reorderFraction,
            // LOCAL playback-buffer depth — an ESTIMATE input, presenters
            // label it "（估计）".
            queueCostMs: media.playbackBufferSeconds * 1000,
            available: media.state == .connected,
            confidence: min(1, 0.4 + 0.2 * Double(fresh.count)),
            mediaReady: mediaProven,
            networkEpoch: networkEpoch,
            measuredAt: probe.at,
            sampleID: probeSampleID,
            hasLossMeasurement: downlink.measured,
            hasJitterMeasurement: true,
            hasQueueEstimate: true)
    }

    /// One WSS-relay sample from the incumbent's ping/gateway-buffer
    /// telemetry. nil while no pong has arrived (never fabricated).
    private func makeRelaySample(now: Date) -> RouteCandidateSample? {
        guard let ws = wsMedia(), let ping = ws.lastPingSample else { return nil }
        let fresh = ws.freshPingSamples(within: 10, now: now)
        if ping.at != lastPingAt {
            lastPingAt = ping.at
            relaySampleID &+= 1
        }
        return RouteCandidateSample(
            rttMs: ping.rtt * 1000,
            jitterMs: (MediaRouteAdvisor.jitter(of: fresh) ?? 0) * 1000,
            lossFraction: 0,
            reorderFraction: 0,
            // Gateway-side host buffer — an ESTIMATE input ("（估计）"); nil
            // means the gateway reported no backlog visibility (unknown).
            queueCostMs: (ws.gatewayBufferSeconds ?? 0) * 1000,
            available: ws.state == .connected,
            confidence: min(1, 0.4 + 0.2 * Double(fresh.count)),
            // Build-62 media proof: attach/ping proves CONTROL only — the
            // relay candidate is media-ready only after genuinely decoded
            // inbound frames (epoch-fenced, real-media freshness). Silence
            // ages the stamp but is never a failure signal.
            mediaReady: ws.isInboundRealFrameFresh(within: 5),
            networkEpoch: networkEpoch,
            measuredAt: ping.at,
            sampleID: relaySampleID,
            // The app-level ping/pong does not count unanswered pings as
            // loss today: loss/reorder are UNKNOWN for the relay sample —
            // never a fabricated zero (build-62).
            hasLossMeasurement: false,
            hasJitterMeasurement: true,
            hasQueueEstimate: ws.gatewayBufferSeconds != nil)
    }

    // MARK: Teardown

    /// Full teardown (call end, park, replace). A standby close is always
    /// safe; a confirmed owner gets the full close (releases self-managed
    /// audio like every primary transport).
    func close() {
        closed = true
        evaluateTimer?.invalidate()
        evaluateTimer = nil
        bootstrapTimer?.invalidate()
        bootstrapTimer = nil
        quicRetryTask?.cancel()
        quicRetryTask = nil
        guard let media = quicCallMedia else { return }
        quicCallMedia = nil
        if confirmedOwner {
            media.close()
        } else {
            media.closeWithoutAudio()
        }
    }

    #if DEBUG
    var debugTransportReady: Bool { transportReady }
    var debugMediaProven: Bool { mediaProven }
    var debugConfirmedOwner: Bool { confirmedOwner }
    var debugIncumbent: RouteCandidateKind? { advisor.incumbentKind }
    var debugEvaluationRunning: Bool { evaluateTimer != nil }
    /// Test seam: the fenced standby-promote identity currently held.
    var debugStandbyToken: String? { standbyToken }
    /// Test seam: the last evaluated decision's detail (diagnostics only).
    private(set) var debugLastDecisionDetail: String?
    /// Test seam: the last evaluated forced mode (nil = auto).
    private(set) var debugLastModeForced: RouteCandidateKind??
    /// Test seam: what fed the last evaluation (diagnostics only).
    private(set) var debugLastSampleSource: String?
    /// Test seam: the count recorded by the LAST `debugInjectSamplesForTest`.
    private(set) var debugLastInjectedCount: Int?
    /// Test seam: rolling history of the last evaluations (diagnostics).
    private(set) var debugEvalHistory: [(source: String, detail: String)] = []
    /// Test seam: run one evaluation on demand (bypasses the readiness gate).
    private var injectedSamples: [RouteCandidateKind: RouteCandidateSample]?
    /// Test seam: feeds these samples into the NEXT evaluation (cleared
    /// after use), letting tests exercise the production streak/hold rules
    /// deterministically via the `now` clock.
    func debugInjectSamplesForTest(quic: RouteCandidateSample? = nil,
                                   rtc: RouteCandidateSample? = nil,
                                   relay: RouteCandidateSample? = nil) {
        var map: [RouteCandidateKind: RouteCandidateSample] = [:]
        if let quic { map[.quicDirect] = quic }
        if let rtc { map[.webRTCDirect] = rtc }
        if let relay { map[.wssRelay] = relay }
        injectedSamples = map
        debugLastInjectedCount = map.count
    }

    func debugEvaluateForTest() {
        if let injectedSamples {
            self.injectedSamples = nil
            evaluateOnce(with: injectedSamples)
        } else {
            evaluateOnce()
        }
    }

    /// Test seam: swap the resolved user mode mid-flight (a user's mode
    /// change) without rebuilding the controller.
    func debugSetPreferredModeForTest(_ mode: RouteModeMapping) {
        preferredTransport = { mode }
    }

    /// Test seam: the client network epoch (injected samples must use the
    /// REAL epoch or the intake fence resets their streaks).
    var debugNetworkEpoch: String { networkEpoch }

    /// Test seam: stop the periodic evaluation (main tick AND the bootstrap
    /// cadence) so fully injected evaluations run without real-tick
    /// interference. Manual `debugEvaluateForTest()` remains the driver.
    func debugSuspendEvaluationTimerForTest() {
        evaluationSuspendedForTest = true
        evaluateTimer?.invalidate()
        evaluateTimer = nil
        bootstrapTimer?.invalidate()
        bootstrapTimer = nil
    }

    /// Test seam: a bounded retry is currently armed.
    var debugRetryScheduled: Bool { quicRetryTask != nil }
    /// Test seam: run any pending retry wait now (deterministic tests).
    func debugCancelRetryForTest() {
        quicRetryTask?.cancel()
        quicRetryTask = nil
    }

    #endif
}
