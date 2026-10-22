import Foundation
import AVFoundation
import CallKit

/// Pure, testable projection of gateway + media signals into a single call
/// phase. It never infers "connected" from a REST 201: `active` requires a
/// gateway `active` state (connectedAt present) and a connected media path.
enum CallPhaseResolver {
    static func resolve(gateway: CallRecord?, media: MediaState) -> ActiveCallPhase {
        if media == .failed {
            return .failed(message: String(localized: "音频通道建立失败，请检查网络后重试。"))
        }
        guard let gateway else { return .outgoingDialing }
        if gateway.endedAt != nil || gateway.state == .idle {
            return .ended(reason: gateway.endReason)
        }
        switch gateway.state {
        case .incomingRinging: return .incomingRinging
        case .outgoingDialing:
            return media == .connected ? .connecting : .outgoingDialing
        case .connecting: return .connecting
        case .recovering: return .reconnecting
        case .ending: return .ending
        case .active:
            // Only here is the call genuinely usable, and only with media.
            switch media {
            case .connected: return .active(startedAt: gateway.connectedDate)
            case .disconnected: return .reconnecting
            default: return .connecting
            }
        case .idle: return .ended(reason: gateway.endReason)
        }
    }
}

/// Creates media sessions (swappable in tests/demo).
protocol MediaSessionProviding: Sendable {
    func makeSession() -> CallMediaSession
}

/// Minimal seam over the AppModel-owned foreground direct preflight, so the
/// coordinator can consume a fresh handoff at call start without depending
/// on the concrete controller in tests.
protocol CallRoutePreflightProviding: AnyObject {
    func handoffForCall() -> RoutePreflightController.Handoff?
}

/// Minimal seam over the AppModel-owned foreground relay probe: the
/// coordinator stops it at call start and reads its fresh samples for the
/// warm direct-first comparison.
protocol CallRelayIdleProbeProviding: AnyObject {
    func stop()
    func appDidEnterForeground()
    func freshTimestampedSamples(within window: TimeInterval, now: Date)
        -> [(rtt: TimeInterval, at: Date)]
}

extension RoutePreflightController: CallRoutePreflightProviding {}
extension RelayIdleProbeController: CallRelayIdleProbeProviding {}

struct WebRTCMediaProvider: MediaSessionProviding {
    func makeSession() -> CallMediaSession { WebRTCCallMedia() }
}

/// Observes call truth from the gateway.
@MainActor
protocol CallCoordinatorDelegate: AnyObject {
    func call(_ gatewayId: String, phaseChanged phase: ActiveCallPhase)
    func callDidEnd(gatewayId: String, reason: EndedCallReason)
    /// The set of gateway-owned calls / conference changed.
    func callGroupChanged()
    /// A 1:1 recording was running when its calls merged: the gateway saved
    /// the prior segment as completed and the conference starts unrecorded.
    func conferenceRecordingInterruptedByMerge()
}

extension CallCoordinatorDelegate {
    func callGroupChanged() {}
    func conferenceRecordingInterruptedByMerge() {}
}

/// Orchestrates calls across REST, events, WebRTC and CallKit.
///
/// The coordinator owns every gateway call this device is part of: one active
/// (unheld) call, zero or more held calls, and at most one hosted conference.
/// The active call keeps the single live media session; held calls have only
/// gateway state. A conference replaces per-call media with one host leg.
///
/// Concurrency model: every async sequence captures a `generation`. A user
/// hangup, provider reset, hold, resume, merge or mode switch increments it;
/// late dial/offer/answer results from a previous generation are discarded
/// (and a dial that succeeded server-side is compensated with a hangup) so they
/// can never recreate media or corrupt a newer call.
@MainActor
final class CallCoordinator: NSObject {
    private struct TrackedCall {
        var record: CallRecord?
        var held: Bool
        var muted: Bool
    }

    private let api: GatewayAPI
    private let callKit: CallKitControlling
    private let mediaProvider: MediaSessionProviding
    /// WSS relay session construction, injectable in tests (fake socket/graph).
    private let wsMediaFactory: () -> WebSocketCallMedia
    /// Build-61 QUIC media construction, injectable in tests (fake socket/graph).
    private let quicMediaFactory: () -> QUICCallMedia
    /// Direct probe construction for the route controller, injectable in
    /// tests to drive route transitions without a WebRTC stack.
    private let routeProbeFactory: @MainActor () -> DirectProbeControlling
    private let registry: CallIdentityRegistry
    private let transport: String
    private let mediaRecoveryWindow: TimeInterval

    weak var delegate: CallCoordinatorDelegate?
    var onQuality: ((MediaQuality) -> Void)?

    private var media: CallMediaSession?
    private var wsMedia: WebSocketCallMedia?
    /// Latest measured quality from the 1:1 WebRTC media session (direct
    /// route telemetry: packet loss; RTT/jitter come from the echo samples).
    private var latestWebRTCQuality: MediaQuality?
    private var wsConferenceMedia: WebSocketCallMedia?
    /// Previous relay kept alive (with audio ownership) while a replacement
    /// attach is staged; retired at exclusive promotion.
    private var stagedPreviousRelay: WebSocketCallMedia?
    /// Auto/Direct/Relay routing for the active non-conference call.
    private var route: CallRouteController?

    // MARK: Hot standby (build 50)
    /// The gateway advertised `capabilities.mediaStandby`; both transports
    /// can be kept warm with fenced make-before-break promotion.
    private var standbySupported = false
    /// The hot-standby orchestration state machine (nil until the gateway
    /// capability is confirmed for this call).
    private var hotStandby: HotStandbyController?
    /// Relay transport held in the standby role (receives the mirrored
    /// downlink; never renders; bounded silence uplink pulse).
    private var standbyRelay: WebSocketCallMedia?
    /// Direct probe held in the standby role (track disabled; stats polled).
    private var standbyDirect: (any DirectProbeControlling)?
    /// Freshness of the last genuine standby frame per route kind.
    private var standbyFreshAt: [HotStandbyController.RouteKind: Date] = [:]
    /// Standby fencing (instance token + role epoch) per route kind.
    private var standbyFences: [HotStandbyController.RouteKind: HotStandbyController.StandbyFence] = [:]
    /// UI surface for the settings audio-routes section.
    private(set) var hotStandbyState: HotStandbyController.HotStandbyState?
    /// Set when a media failure should FIRST try a fenced warm-standby
    /// takeover inside the serialized recovery episode (auto mode, fresh
    /// standby only). Cleared by the intercepting attempt.
    private var pendingStandbyTakeover: HotStandbyController.RouteKind?
    /// Foreground, call-independent direct-path preflight (AppModel-owned).
    /// The route controller consumes its fresh candidate at call start.
    var routePreflight: CallRoutePreflightProviding?
    /// Foreground, call-independent relay-path probe (AppModel-owned). Stopped
    /// when a call starts and restarted when the call's media is torn down.
    var idleRelayProbe: CallRelayIdleProbeProviding?
    /// Ownership of the fresh preflight candidate taken at media-establish
    /// time (BEFORE the WSS handshake), handed to the route controller once it
    /// exists. Kept here so a call that fails before routing is built cannot
    /// leak the detached peer, and so the probe is never cancelled by the
    /// cycle stop ahead of consumption (the build-34 "preflight connected but
    /// never selected" defect).
    private var pendingPreflightHandoff: RoutePreflightController.Handoff?
    /// Build-50: a warm preflight that SEEDS the direct standby attach in a
    /// hot-standby call (committed with standby=true) instead of being
    /// discarded by the suppressed legacy initial selection. Consumed by
    /// `attachDirectStandby`; released in teardown if never used.
    private var standbySeed: RoutePreflightController.Handoff?
    /// Build-68 incremental exchange for the hot-standby probe attach.
    private var standbyTrickleExchange: TrickleCandidateExchange?
    private let routeModeDefault: MediaRouteMode
    private let routeGatewayID: String?
    /// Injectable route cadence for deterministic tests; production uses the
    /// controller's built-in defaults.
    private let routeCadence: CallRouteController.Cadence?
    /// ICE/direct advertised by the gateway for the active call.
    private var directAdvertised = false
    /// Fresh relay-path RTT samples captured from the foreground idle relay
    /// probe at call start, used ONLY by the warm direct-first fastpath to
    /// answer "is a ready direct handoff genuinely better than the relay
    /// RIGHT NOW" without attaching a call relay first. Timestamps travel
    /// with the values so freshness is re-validated at decision time (a slow
    /// `/ice` or commit must not act on an aged RTT).
    private var idleRelaySamples: [(rtt: TimeInterval, at: Date)] = []
    /// Build-50 hot-standby snapshot for the settings audio-routes UI.
    var onHotStandbyState: ((HotStandbyController.HotStandbyState?) -> Void)?
    var onRouteState: ((CallRouteState) -> Void)?
    var onRouteNotice: ((String, Bool) -> Void)?
    /// Honest, minimal product copy while audio ownership is unavailable
    /// (another app holds the mic, an interruption is pending, the audio
    /// server is resetting). nil clears it. Never used for route decisions.
    var onAudioStatus: ((String?) -> Void)?

    // MARK: QUIC direct (build 61, protocol docs/quic61)
    /// The per-call QUIC direct-path orchestration (nil until the gateway
    /// advertises `mediaQuic` and the WSS control plane is attached).
    private var quicDirect: QUICDirectController?
    /// Build-65: the QUIC authority's OWN live view of the user's requested
    /// routing mode. The legacy route object only exists while direct
    /// WebRTC is advertised (`routeRelayDidConnect` gates on it), so a
    /// WSS+QUIC call (no ICE transport) has `route == nil` — a mid-call
    /// `selectRouteMode` wrote the preference store but the controller's
    /// mapping closure read the immutable `routeModeDefault`, silently
    /// ignoring forced modes. `selectRouteMode` writes this while the
    /// authority is active; the closure prefers it over the legacy route
    /// state; `closeQUICDirect` resets it with the per-call authority.
    private var quicAuthorityRequestedMode: MediaRouteMode?
    /// Build-65: the SINGLE live source of the user's requested routing
    /// mode. Order: the QUIC authority's per-call override (set by
    /// `selectRouteMode` while the authority is active), the legacy route
    /// object's live state, then the CURRENTLY PERSISTED preference —
    /// never the init-time `routeModeDefault` snapshot alone: the
    /// coordinator outlives individual calls, so a mode saved during call
    /// N must shape call N+1 (and the recovery attempt order) exactly as
    /// the persisted preference says.
    private var liveRequestedMode: MediaRouteMode {
        quicAuthorityRequestedMode
            ?? route?.routeState.mode
            ?? MediaRoutePreferenceStore.shared.mode(for: routeGatewayID)
    }
    /// Test seam: overrides the QUIC attempt budget (production keeps the
    /// ~8 s protocol §8 bound so the answer is never blocked).
    var quicAttemptTimeoutForTest: TimeInterval?
    /// The in-flight QUIC attempt (info + STUN + punch POST + dial+bind),
    /// fired detached so it never blocks the answer (protocol §8).
    private var quicAttemptTask: Task<Void, Never>?
    /// The nonce-fenced waiter for the WSS `promoted` reply during a QUIC
    /// standby promotion. Main-actor state: both the reply delivery and the
    /// timeout hop here, so extraction is race-free.
    private var quicPromoteWaiter: (nonce: String, continuation: CheckedContinuation<WSMediaControl?, Never>)?
    /// Build-61 route-selection publish seam (`AppModel.publishRouteSelection`).
    var onRouteSelection: ((RouteSelectionDecision, [RouteCandidateKind: RouteCandidateSample], Date) -> Void)?
    /// Build-61 QUIC advertisement seam (`AppModel.updateDirectTransportAvailability`).
    var onDirectTransportAvailability: ((ICEConfiguration) -> Void)?
    /// Build-62: injectable STUN resolver so tests script the srflx answer
    /// deterministically; production uses `STUNBindingClient` (best-effort).
    /// Returns BOTH the observed local endpoint and the mapped srflx —
    /// QUIC binds `local`, the gateway punches `mapped`.
    var quicSTUNResolver: (@MainActor (String, UInt16) async -> STUNBindingResult?)?
    /// Build-62: the pairing-trusted gateway identity, AppModel-injected from
    /// the enrollment binding. The QUIC pin document's Ed25519 signature is
    /// verified against THIS anchor (never against the pin's co-returned
    /// identity mirrors). nil ⇒ QUIC stays closed and RTC/WSS are preserved.
    var quicTrustedGatewayIdentity: (gatewayId: String, fingerprint: String)?
    /// Build-61: QUIC advisor evaluation cadence (production 1 s; tests
    /// shorten it so the deterministic ticks run quickly).
    var quicEvaluationTickInterval: TimeInterval = 1
    /// Build-62 test seam: shrink the sustained-evidence windows so the
    /// production streak/hold rules run deterministically without
    /// real-time waits. Production always uses the defaults.
    var quicPolicyConfigForTest: RouteSelectionPolicy.Config?

    // MARK: Network-path recovery (build 66)
    /// Path observer construction, injectable in tests (a fake observer
    /// drives deterministic Wi-Fi ↔ cellular / down-up edges). Production
    /// wraps NWPathMonitor; the observer delivers on the main actor.
    private let pathObserverFactory: @MainActor () -> CallPathObserving
    private var pathObserver: CallPathObserving?
    /// Monotonic client network generation (bumped only on MEANINGFUL path
    /// transitions). Fences duplicate/stale path deliveries.
    private(set) var networkGeneration: UInt64 = 0
    /// True while the OS reports a usable path (the reachable edge
    /// immediately revalidates the carrier).
    private(set) var networkPathUsable = true
    /// Backstop media-stall grace for the ACTIVE carrier (no genuine inbound
    /// frame AND stale transport RTT evidence). The gateway emits a downlink
    /// frame every 20 ms, so this is ~250 missing frames; healthy content
    /// silence still arrives as frames and never triggers. Injectable so
    /// tests run deterministically.
    var mediaStallGrace: TimeInterval = 5
    /// Shorter grace used when a path transition was just observed: the old
    /// interface is gone, so waiting the full stall grace would keep the user
    /// silent for no reason.
    var pathChangeStallGrace: TimeInterval = 1.5
    /// Stall-check cadence (production 1 s).
    var mediaStallTickInterval: TimeInterval = 1
    private var stallWatchdogTask: Task<Void, Never>?
    /// Build-66: one-shot carrier recheck scheduled right after a path
    /// transition (the transition may precede the old interface dying).
    private var pathRecheckTask: Task<Void, Never>?
    /// One arm per stall episode: the watchdog must never hot-loop while the
    /// recovery is already running (or before fresh media returns).
    private var stallArmed = false
    /// Build-66 recovery nudge cooldown for path edges (mirrors the
    /// controller's own rate limit; a burst of path events cannot become an
    /// attempt storm).
    var pathRecoveryNudgeCooldown: TimeInterval = 0.5
    private var lastPathNudgeUptime: TimeInterval?
    /// True between an interruption began and the next usable activation.
    /// While set, watchdog/self-activation recovery is suppressed.
    private var audioInterrupted = false
    private var audioStatusMessage: String?
    private var activeGatewayId: String?
    private var latestGateway: CallRecord?
    private var latestMedia: MediaState = .idle
    /// Direction of the active call (true = outgoing). Tracked locally so
    /// the progress tone knows a connecting phase is the remote ringing.
    private var activeCallIsOutgoing = false
    private var muted = false
    private var speaker = false
    private var monitorTask: Task<Void, Never>?
    private var mediaTask: Task<Void, Never>?
    /// Bounded build-48 media recovery: 45 s absolute window, immediate +
    /// 1/2/4/8 s re-attempts of the SAME call, current path first (auto then
    /// alternate), cleared only by fresh-media proof. `mediaRecoveryTask`
    /// remains the cancellation handle wired into teardown.
    private var mediaRecovery: MediaRecoveryController?
    private var mediaRecoveryTask: Task<Void, Never>?
    /// Build-49: identity of the recovery attempt currently in flight. Route-
    /// controller-initiated transport changes that happen WHILE the episode
    /// is open (e.g. the post-adoption audio gate falling back to the relay)
    /// stage their socket with THIS captured identity, so their fresh media
    /// can still prove — and only prove — the live attempt. Cleared with the
    /// episode (cancelMediaRecovery).
    private var pendingRecoveryProof: MediaRecoveryController.ProofIdentity?
    /// Build-66: the transport a recovery attempt is currently negotiating
    /// (before it installs). Tracked so a fresh-path nudge can interrupt an
    /// attach still bound to the dead network path.
    private var recoveryRelayCandidate: WebSocketCallMedia?
    private var recoveryDirectCandidate: CallMediaSession?
    private var ended = false
    /// Local call-progress tones (ringback while the remote rings; bounded
    /// busy burst on busy-class ends). Fills silence only — never layered
    /// over real early media, and dies with the call/media lifecycle.
    private let progressTone = CallProgressToneController()
    /// Media session kept alive briefly after a busy-class end so the
    /// bounded busy burst remains audible through the shared engine. The
    /// session is fully detached from routing; it is closed (idempotently)
    /// after the hold and never touches a newer call's media.
    private var busyToneSession: WebSocketCallMedia?

    /// Every gateway call this device owns, keyed by gateway call id.
    private var tracked: [String: TrackedCall] = [:]
    /// Non-nil while this device hosts a merged conference.
    private var conference: ConferenceRecord?
    /// Conference legs currently held (locally tracked; the server snapshot
    /// does not carry a hold flag).
    private var heldConferenceLegs: Set<String> = []
    private var defaultLineId: String?
    /// uuid -> gateway id cache so CallKit actions resolve without awaiting
    /// the registry actor on the critical path.
    private var knownUUIDs: [UUID: String] = [:]

    /// Bumped on every teardown/reset; stale continuations observe a stale
    /// generation and must not mutate the new (or empty) call.
    private var generation: UInt64 = 0

    init(
        api: GatewayAPI,
        callKit: CallKitControlling,
        mediaProvider: MediaSessionProviding,
        registry: CallIdentityRegistry,
        transport: String,
        mediaRecoveryWindow: TimeInterval = 45,
        gatewayID: String? = nil,
        wsMediaFactory: @escaping @MainActor () -> WebSocketCallMedia = { WebSocketCallMedia() },
        routeProbeFactory: @escaping @MainActor () -> DirectProbeControlling = { MediaProbeController() },
        routeCadence: CallRouteController.Cadence? = nil,
        quicMediaFactory: @escaping @MainActor () -> QUICCallMedia = { QUICCallMedia() },
        pathObserverFactory: @escaping @MainActor () -> CallPathObserving = { NetworkPathObserver() }
    ) {
        self.api = api
        self.callKit = callKit
        self.mediaProvider = mediaProvider
        self.wsMediaFactory = wsMediaFactory
        self.routeProbeFactory = routeProbeFactory
        self.quicMediaFactory = quicMediaFactory
        self.pathObserverFactory = pathObserverFactory
        self.registry = registry
        self.transport = transport
        self.mediaRecoveryWindow = mediaRecoveryWindow
        self.routeModeDefault = MediaRoutePreferenceStore.shared.mode(for: gatewayID)
        self.routeGatewayID = gatewayID
        self.routeCadence = routeCadence
        super.init()
        callKit.director = self
        // Call-progress tone seams read the CURRENT media session every
        // invocation (the session object changes across handovers), plus a
        // busy-class hold session after teardown.
        progressTone.engineRunning = { [weak self] in
            guard let self else { return false }
            if let ws = self.wsMedia, ws.isGraphRunning { return true }
            if let conf = self.wsConferenceMedia, conf.isGraphRunning { return true }
            if let held = self.busyToneSession, held.isGraphRunning { return true }
            return false
        }
        progressTone.playbackIdleMs = { [weak self] in
            guard let self else { return .max }
            let idle = self.wsMedia?.playbackIdleMilliseconds
                ?? self.wsConferenceMedia?.playbackIdleMilliseconds
                ?? self.busyToneSession?.playbackIdleMilliseconds
                ?? .max
            return idle
        }
        progressTone.emitFrame = { [weak self] frame in
            guard let self else { return }
            if let ws = self.wsMedia {
                ws.pushSyntheticTone(frame)
            } else if let conf = self.wsConferenceMedia {
                conf.pushSyntheticTone(frame)
            } else if let held = self.busyToneSession {
                held.pushSyntheticTone(frame)
            }
        }
        // Forward activation to EVERY live media session. The WSS relay
        // (wsMedia / wsConferenceMedia) is a different object from the ICE
        // session (media); a CallKit answer usually activates the audio
        // session only AFTER the attach ran (didActivate follows the
        // fulfilled answer action), so the attach-time replay sees no active
        // session. Forgetting the WSS session here leaves the relay
        // connected but permanently silent in both directions.
        //
        // Ordering: callbacks are appended to `deliverAudioEvent`'s FIFO and
        // applied on the main actor in delivery order. The OS callbacks
        // (CXProvider delegate, LCK, NotificationCenter) never execute engine
        // or session work inline; every event carries the bridge ownership
        // epoch and is dropped when a newer event already superseded it, so a
        // late callback can never restart audio for an ended call or pause a
        // newer call's audio.
        AudioSessionBridge.shared.onActivate = { [weak self] session in
            self?.deliverAudioEvent { coordinator, epoch in
                coordinator.applyAudioActivation(session, epoch: epoch)
            }
        }
        AudioSessionBridge.shared.onDeactivate = { [weak self] session in
            self?.deliverAudioEvent { coordinator, epoch in
                coordinator.applyAudioDeactivation(session, epoch: epoch)
            }
        }
        // Audio lifecycle (interruption / route / media-services reset). The
        // bridge serializes the session state; the coordinator reacts by
        // stopping or resuming ONLY the live media surfaces — transports are
        // never torn down by an audio event.
        AudioSessionBridge.shared.onInterruptionBegan = { [weak self] in
            self?.deliverAudioEvent { coordinator, epoch in
                coordinator.applyInterruptionBegan(epoch: epoch)
            }
        }
        AudioSessionBridge.shared.onInterruptionEnded = { [weak self] shouldResume in
            self?.deliverAudioEvent { coordinator, epoch in
                coordinator.applyInterruptionEnded(shouldResume: shouldResume, epoch: epoch)
            }
        }
        AudioSessionBridge.shared.onMediaServicesReset = { [weak self] in
            self?.deliverAudioEvent { coordinator, epoch in
                coordinator.applyMediaServicesReset(epoch: epoch)
            }
        }
        AudioSessionBridge.shared.onAvailabilityChanged = { [weak self] available, message in
            self?.deliverAudioEvent { coordinator, epoch in
                coordinator.applyAudioAvailability(available: available, message: message, epoch: epoch)
            }
        }
        AudioSessionBridge.shared.onRouteChanged = { [weak self] _, _ in
            // Route changes do not change ownership; revalidate the live
            // graphs unconditionally (they are nil when no call exists).
            self?.deliverAudioEvent { coordinator, _ in
                coordinator.wsMedia?.revalidateAudioRoute()
                coordinator.wsConferenceMedia?.revalidateAudioRoute()
                coordinator.quicDirect?.quicCallMedia?.revalidateAudioRoute()
            }
        }
    }

    /// Serialized audio-event delivery. Producers append to a lock-protected
    /// FIFO; a single main-actor drain then applies the queued events in
    /// append (delivery) order. Ordering is therefore owned by this queue and
    /// never depends on how the runtime happens to schedule main-actor tasks.
    /// Every consumer re-checks the bridge epoch at execution time, so an
    /// event superseded by a newer state change is dropped instead of racing
    /// it. Delivery is ALWAYS deferred off the producer: the producers are OS
    /// callbacks (CXProvider/LCK delegates, AVAudioSession notifications) and
    /// running engine/session work inline in them can stall the system's own
    /// call presentation.
    private final class AudioEventQueue: @unchecked Sendable {
        typealias Payload = @MainActor (CallCoordinator, UInt64) -> Void

        private let lock = NSLock()
        private var pending: [(epoch: UInt64, payload: Payload)] = []
        private var draining = false

        /// Appends an event. Returns true when the caller must start the drain.
        func append(epoch: UInt64, payload: @escaping Payload) -> Bool {
            lock.lock()
            defer { lock.unlock() }
            pending.append((epoch, payload))
            guard !draining else { return false }
            draining = true
            return true
        }

        /// Next event in append order, or nil when the queue is empty.
        func next() -> (epoch: UInt64, payload: Payload)? {
            lock.lock()
            defer { lock.unlock() }
            guard !pending.isEmpty else {
                draining = false
                return nil
            }
            return pending.removeFirst()
        }
    }

    private let audioEvents = AudioEventQueue()

    private nonisolated func deliverAudioEvent(
        _ block: @escaping @MainActor (CallCoordinator, UInt64) -> Void
    ) {
        let epoch = AudioSessionBridge.shared.eventEpoch
        if audioEvents.append(epoch: epoch, payload: block) {
            Task { @MainActor [weak self] in
                self?.drainAudioEvents()
            }
        }
    }

    @MainActor
    private func drainAudioEvents() {
        while let event = audioEvents.next() {
            event.payload(self, event.epoch)
        }
    }

    // MARK: Audio ownership event application (epoch-fenced)

    private func applyAudioActivation(_ session: AVAudioSession, epoch: UInt64) {
        guard epoch == AudioSessionBridge.shared.eventEpoch else {
            DiagnosticsCensus.shared.increment("audio.staleActivationDropped")
            return
        }
        // Never start media for an ended call; the session must still be the
        // one the bridge considers usable.
        guard activeGatewayId != nil || conference != nil,
              AudioSessionBridge.shared.activeSession === session else { return }
        audioInterrupted = false
        publishAudioStatus(nil)
        media?.audioActivated(with: session)
        wsMedia?.audioActivated(with: session)
        wsConferenceMedia?.audioActivated(with: session)
        // Build-61 QUIC path: the staged gate suppresses a standby start; a
        // confirmed owner starts its graph exactly like every other session.
        quicDirect?.quicCallMedia?.audioActivated(with: session)
        // During a make-before-break handover the OLD relay is the one still
        // carrying audio (wsMedia points at the staged replacement, whose
        // own gate suppresses a second mic); a recovery activation must
        // resume the carrier, not the staged socket.
        stagedPreviousRelay?.audioActivated(with: session)
        // Warm direct-first (build 38): the call can be carried by an adopted
        // direct probe with no WSS relay session.
        route?.directAudioSessionActivated(session)
    }

    private func applyAudioDeactivation(_ session: AVAudioSession, epoch: UInt64) {
        // A deactivation superseded by a newer activation must not pause the
        // newer call's audio.
        guard epoch == AudioSessionBridge.shared.eventEpoch else {
            DiagnosticsCensus.shared.increment("audio.staleDeactivationDropped")
            return
        }
        media?.audioDeactivated(with: session)
        wsMedia?.audioDeactivated(with: session)
        wsConferenceMedia?.audioDeactivated(with: session)
        quicDirect?.quicCallMedia?.audioDeactivated(with: session)
        // A relay being replaced during a handover still runs its graph; stop
        // it too (it is never restarted unless promoted).
        stagedPreviousRelay?.audioDeactivated(with: session)
        // Warm direct-first: the adopted RTC peer must drop its ADM when the
        // system deactivates, or it keeps the mic warm.
        route?.directAudioSessionDeactivated(session)
    }

    private func applyInterruptionBegan(epoch: UInt64) {
        guard epoch == AudioSessionBridge.shared.eventEpoch else { return }
        audioInterrupted = true
        // The bridge has already published onDeactivate (graphs stopped) and
        // its honest availability message; nothing else to tear down. The
        // transport and CallKit call stay fully alive so recovery is a graph
        // re-bind, never a reconnect.
        DiagnosticsStore.shared.log("call", "audio interruption began (transports kept)")
    }

    private func applyInterruptionEnded(shouldResume: Bool, epoch: UInt64) {
        guard epoch == AudioSessionBridge.shared.eventEpoch else { return }
        audioInterrupted = false
        // Self-managed recovery is driven by the bridge (it publishes
        // onActivate on success); a system-managed call intentionally waits
        // for the real system didActivate. Only a system activation clears the
        // honest "waiting" status, so nothing is cleared here.
        DiagnosticsStore.shared.log("call", "audio interruption ended shouldResume=\(shouldResume)")
    }

    private func applyMediaServicesReset(epoch: UInt64) {
        guard epoch == AudioSessionBridge.shared.eventEpoch else { return }
        audioInterrupted = false
        // The bridge invalidates the session and (self-managed only) runs one
        // bounded reactivation; success arrives through onActivate with the
        // rebuilt graph. Transports are untouched.
        DiagnosticsStore.shared.log("call", "audio media services reset (bounded recovery armed)")
    }

    private func applyAudioAvailability(available: Bool, message: String, epoch: UInt64) {
        guard epoch == AudioSessionBridge.shared.eventEpoch else { return }
        publishAudioStatus(available ? nil : message)
    }

    #if DEBUG
    // Lifecycle regression seams: deliver an ownership event with an explicit
    // epoch (production delivery is synchronous and reads the live epoch).
    func applyAudioActivationForTest(_ session: AVAudioSession, epoch: UInt64) {
        applyAudioActivation(session, epoch: epoch)
    }
    func applyAudioDeactivationForTest(_ session: AVAudioSession, epoch: UInt64) {
        applyAudioDeactivation(session, epoch: epoch)
    }
    var audioInterruptedForTest: Bool { audioInterrupted }
    #endif

    // MARK: Multi-call inspection

    /// Current active (unheld) call, when any.
    /// True while ANY call is still live on this device (ringing, dialing or
    /// connected): the foreground preflight stays idle then.
    var hasLiveCall: Bool {
        tracked.values.contains { $0.record?.isFinished == false }
    }

    var activeCallRecord: CallRecord? {
        if let activeGatewayId { return tracked[activeGatewayId]?.record ?? latestGateway }
        return latestGateway
    }

    /// Calls answered on this device and currently held.
    var heldCallRecords: [CallRecord] {
        tracked.compactMap { key, entry -> CallRecord? in
            guard entry.held,
                  !(conference?.legs.contains(where: { $0.id == key }) ?? false),
                  let record = entry.record,
                  !record.isFinished else { return nil }
            return record
        }.sorted { $0.startedAt < $1.startedAt }
    }

    /// Non-nil while this device hosts a merged conference.
    var conferenceRecord: ConferenceRecord? { conference }

    /// Conference legs the host has put on hold.
    var conferenceHeldLegIDs: Set<String> { heldConferenceLegs }

    func isTracking(callId: String) -> Bool { tracked[callId] != nil }

    /// True while this exact gateway call is still tracked as ringing,
    /// independent of UI focus. Push-registered calls carry no record until
    /// reconciliation, so a tracked, unheld entry with no outcome counts as
    /// still ringing.
    func isRinging(callId: String) -> Bool {
        guard let entry = tracked[callId] else { return false }
        guard let record = entry.record else { return !entry.held }
        return record.state == .incomingRinging && !record.isFinished
    }

    /// Gateway ids of incoming calls still ringing locally, with the moment
    /// their ring started. Reconnect reconciliation releases the ones the
    /// gateway no longer lists, without touching calls that just arrived.
    var ringingIncomingCalls: [(id: String, startedAt: Date)] {
        tracked.compactMap { key, entry in
            guard let record = entry.record,
                  record.state == .incomingRinging,
                  !record.isFinished else { return nil }
            return (key, record.startedDate)
        }
    }

    /// Default line used for outgoing calls.
    func setDefaultLineId(_ lineId: String?) { defaultLineId = lineId }

    // MARK: Event ingestion

    func ingest(event: GatewayEvent) {
        switch event.rawType {
        case "conference.ended":
            Task { [weak self] in
                await self?.refreshConference(noteLegEnded: nil, assumeDissolvedOnFailure: true)
            }
            return
        case "conference.updated":
            Task { [weak self] in
                await self?.refreshConference(noteLegEnded: nil, assumeDissolvedOnFailure: false)
            }
            return
        default:
            break
        }
        guard let call = event.call() else { return }
        if call.id == activeGatewayId {
            latestGateway = call
            track(call.id, record: call)
            if call.isFinished {
                handleRemoteEnd(reason: call.endReason)
            } else {
                publishPhase()
                delegate?.callGroupChanged()
            }
            return
        }
        let isConferenceLeg = conference?.legs.contains(where: { $0.id == call.id }) ?? false
        guard tracked[call.id] != nil || isConferenceLeg else { return }
        if isConferenceLeg {
            if call.isFinished {
                Task { [weak self] in
                    await self?.refreshConference(noteLegEnded: call.id, assumeDissolvedOnFailure: true)
                }
            } else {
                replaceConferenceLeg(call)
                delegate?.callGroupChanged()
            }
            return
        }
        track(call.id, record: call)
        if call.isFinished {
            Task { [weak self] in await self?.removeTrackedCall(call.id, reason: .remoteEnded) }
        } else {
            delegate?.callGroupChanged()
        }
    }

    // MARK: Outbound

    /// Explicit outgoing entry point that carries the line for a unified
    /// gateway (CallKit's director path uses ``setDefaultLineId(_:)``).
    func startOutgoing(peer: String, lineId: String?, uuid: UUID) {
        startOutgoingCall(peer: peer, lineId: lineId ?? defaultLineId, uuid: uuid)
    }

    private func startOutgoingCall(peer: String, lineId: String?, uuid: UUID) {
        guard activeGatewayId == nil else {
            AppLog.call.notice("ignoring duplicate dial; one active call only")
            return
        }
        // Configure the shared session for two-way voice BEFORE the system
        // activates it, so the first engine start finds an already-settled
        // category/mode instead of racing a just-requested reconfiguration
        // (build-33 cold-start dead tap).
        AudioSessionBridge.shared.prepareForVoiceCall()
        beginCall()
        activeCallIsOutgoing = true
        let clientCallId = uuid.uuidString.lowercased()
        activeGatewayId = clientCallId
        latestGateway = nil
        latestMedia = .idle
        tracked[clientCallId] = TrackedCall(record: nil, held: false, muted: false)
        knownUUIDs[uuid] = clientCallId
        delegate?.callGroupChanged()
        publishPhase()
        let gen = generation
        let idem = uuid.uuidString

        lifecycleRun { [weak self] in
            guard let self else { return }
            do {
                let call = try await self.api.dial(
                    to: peer, lineId: lineId, clientCallId: clientCallId, idempotencyKey: idem
                )
                guard gen == self.generation else {
                    // The call was cancelled/reset while dial was in flight. The
                    // carrier call may already exist: converge it with one
                    // hangup using the same idempotency family.
                    await self.compensateServerCall(call.id, gen: gen)
                    return
                }
                await self.registry.associate(gatewayId: call.id, uuid: uuid)
                self.knownUUIDs[uuid] = call.id
                guard gen == self.generation else {
                    await self.compensateServerCall(call.id, gen: gen)
                    return
                }
                self.tracked.removeValue(forKey: clientCallId)
                self.activeGatewayId = call.id
                self.latestGateway = call
                self.tracked[call.id] = TrackedCall(record: call, held: false, muted: false)
                self.callKit.reportOutgoingConnecting(uuid: uuid)
                self.delegate?.callGroupChanged()
                self.publishPhase()
                do {
                    try await self.establishMedia(
                        callId: call.id, uuid: uuid, generation: gen, selfManagedAudio: false)
                } catch is CancellationError {
                    return
                } catch {
                    // Media failed on a call the gateway still owns: converge
                    // the remote leg rather than leave it ringing/connected.
                    guard gen == self.generation else { return }
                    await self.failActiveCall(message: self.friendly(error))
                }
            } catch is CancellationError {
                return
            } catch {
                guard gen == self.generation else { return }
                await self.failActiveCall(message: self.friendly(error))
            }
        }
    }

    /// Best-effort hangup for a gateway call that exists server-side after the
    /// local call was already torn down. Uses a fresh idempotency key.
    private func compensateServerCall(_ callId: String, gen: UInt64) async {
        guard gen <= generation else { return }
        AppLog.call.notice("compensating server-side call after local cancel")
        tracked.removeValue(forKey: callId)
        try? await api.hangup(callId: callId, idempotencyKey: UUID().uuidString)
        await registry.remove(gatewayId: callId)
    }

    // MARK: Incoming answer

    /// Answers a ringing call on this device. When another call is still
    /// active it is put on hold first so the gateway can hand the device over;
    /// on a v1 gateway (hold unsupported) the answer still proceeds.
    ///
    /// Gateway answer only (awaited by the provider so fulfill/fail reflects
    /// the real answer). Media is started afterwards and never blocks
    /// fulfillment.
    /// CallKit-driven answer: the system call already exists, so CallKit owns
    /// audio activation and the media session must not self-activate.
    func answerIncoming(uuid: UUID) async throws {
        try await performAnswer(uuid: uuid, directAudio: false)
    }

    /// Direct in-app answer with no system call: this session explicitly owns
    /// its voice-chat activation (no `didActivate` will ever arrive).
    func answerIncomingDirect(uuid: UUID) async throws {
        try await performAnswer(uuid: uuid, directAudio: true)
    }

    private func performAnswer(uuid: UUID, directAudio: Bool) async throws {
        guard let gatewayId = await gatewayId(for: uuid) else {
            AppLog.call.error("answer with no known gateway call")
            throw APIError.notReady("未知的来电，无法接听。")
        }
        guard conference == nil else {
            throw APIError.notReady("会议进行中无法接听其他来电。")
        }
        if let current = activeGatewayId, current != gatewayId {
            // v1 gateways reject hold with notReady: treat it as unsupported
            // and continue; real failures abort the answer so the first call
            // stays untouched.
            _ = try await requestHold(current)
            guard activeGatewayId == current else { throw CancellationError() }
            _ = bumpGeneration()
            parkActiveCall(current)
            try await answerTarget(gatewayId, uuid: uuid, gen: generation, directAudio: directAudio)
            return
        }
        let gen = bumpGeneration()
        try await answerTarget(gatewayId, uuid: uuid, gen: gen, directAudio: directAudio)
    }

    private func answerTarget(
        _ gatewayId: String, uuid: UUID, gen: UInt64, directAudio: Bool
    ) async throws {
        try await api.answer(callId: gatewayId, idempotencyKey: UUID().uuidString)
        guard gen == generation else { throw CancellationError() }
        activeCallIsOutgoing = false
        knownUUIDs[uuid] = gatewayId
        var entry = tracked[gatewayId] ?? TrackedCall(record: nil, held: false, muted: false)
        entry.held = false
        tracked[gatewayId] = entry
        activate(callId: gatewayId, uuid: uuid, gen: gen, selfManagedAudio: directAudio)
    }

    /// Registers an incoming call reported via push/event before media exists.
    /// A second incoming call is tracked without disturbing the active one.
    func registerIncoming(gatewayId: String, uuid: UUID, record: CallRecord?) {
        knownUUIDs[uuid] = gatewayId
        if activeGatewayId != nil || conference != nil {
            var entry = tracked[gatewayId] ?? TrackedCall(record: nil, held: false, muted: false)
            if let record { entry.record = record }
            tracked[gatewayId] = entry
            Task { await registry.associate(gatewayId: gatewayId, uuid: uuid) }
            delegate?.callGroupChanged()
            return
        }
        beginCall()
        activeCallIsOutgoing = false
        activeGatewayId = gatewayId
        latestGateway = record
        latestMedia = .idle
        tracked[gatewayId] = TrackedCall(record: record, held: false, muted: false)
        Task { await registry.associate(gatewayId: gatewayId, uuid: uuid) }
        delegate?.callGroupChanged()
        publishPhase()
    }

    /// Bounded recovery for a lost system-side audio activation. Polls while
    /// media is still attaching (the one-shot timer in build 21 could expire
    /// before the socket existed), fires once a transport is live, and is a
    /// no-op when the system owns the session. Self-activation failure is
    /// logged so a silent call can never hide behind an unchecked Bool.
    ///
    /// A real interruption suppresses this repair entirely: a competing
    /// session owns audio then, and forcing `setActive(true)` would fight it
    /// (the exact cross-app failure this build repairs). The interruption
    /// lifecycle owns the next activation instead.
    private func armActivationFallback(callId: String, gen: UInt64) {
        Task { @MainActor [weak self] in
            guard let self else { return }
            // Poll up to ~9 s: media attach is normally sub-second, but a
            // slow tunnel must not permanently disable the recovery.
            for _ in 0..<6 {
                try? await Task.sleep(nanoseconds: 1_500_000_000)
                guard gen == self.generation,
                      self.activeGatewayId == callId, !self.ended else { return }
                if AudioSessionBridge.shared.isInterrupted { return }
                if AudioSessionBridge.shared.activeSession != nil { return }
                if self.wsMedia?.isGraphRunning == true { return }
                let activated: Bool
                if let ws = self.wsMedia {
                    activated = ws.activateAudioWithoutCallKit()
                } else if let media = self.media {
                    activated = media.activateAudioWithoutCallKit()
                } else if let route, route.activeTransportIsDirect {
                    // Warm direct-first (build 38): no WSS graph exists; the
                    // adopted RTC peer needs the self-activated session
                    // forwarded exactly like a lost CallKit activation.
                    activated = route.activateDirectWithoutCallKit()
                } else {
                    continue // media still attaching; retry on the next tick
                }
                DiagnosticsStore.shared.log("audio",
                    activated
                        ? "answer activation fallback: self-activation started"
                        : "answer activation fallback FAILED: self-activation returned false")
                if activated { return }
            }
            // Bounded repair exhausted without a usable session: reflect the
            // temporary unavailability honestly instead of leaving a
            // connected-but-silent call unexplained.
            guard gen == self.generation, self.activeGatewayId == callId, !self.ended else { return }
            if AudioSessionBridge.shared.activeSession == nil {
                self.publishAudioStatus(
                    String(localized: "暂时无法启用通话音频，请稍后重试。"))
            }
        }
    }

    // MARK: Audio interruption / route / media-services lifecycle

    private func publishAudioStatus(_ message: String?) {
        // Never surface an audio status without a live call, and never let a
        // late event reintroduce one after the call ended.
        let effective = activeGatewayId != nil ? message : nil
        guard effective != audioStatusMessage else { return }
        audioStatusMessage = effective
        if let effective {
            DiagnosticsStore.shared.log("audio", "status: \(effective)")
        }
        onAudioStatus?(effective)
    }

    /// Makes the given tracked call active, replacing any old media session.
    /// `selfManagedAudio` is true only for a direct in-app answer with no
    /// system call; every other path leaves activation to CallKit.
    private func activate(
        callId: String, uuid: UUID, gen: UInt64, selfManagedAudio: Bool = false
    ) {
        // The call is about to own audio: settle the voice-chat session before
        // any media object starts an engine (the push-registration path in
        // `beginCall` deliberately does not touch the session).
        AudioSessionBridge.shared.prepareForVoiceCall()
        // Every path that makes a call active arms audio demand for it (an
        // already-running second call answered after the first ended must
        // not be treated as an idle app).
        AudioSessionBridge.shared.callStarted()
        activeGatewayId = callId
        latestGateway = tracked[callId]?.record
        latestMedia = .idle
        ended = false
        monitorStarted = false
        monitorTask?.cancel()
        monitorTask = nil
        mediaTask?.cancel()
        mediaTask = nil
        self.cancelMediaRecovery()
        // Build-66: the fresh media task (activate → establishMedia) arms
        // the path observation and the stall backstop.
        media?.close()
        media = nil
        wsMedia?.close()
        wsMedia = nil
        stagedPreviousRelay?.closeWithoutAudio()
        stagedPreviousRelay = nil
        closeQUICDirect()
        route?.teardown()
        route = nil
        discardPendingPreflight()
        if var entry = tracked[callId] {
            entry.held = false
            tracked[callId] = entry
        }
        knownUUIDs[uuid] = callId
        delegate?.callGroupChanged()
        publishPhase()
        // System-owned activation can be lost (build 20: the gateway got the
        // answer but neither didActivate nor any audio session ever arrived,
        // leaving a connected call permanently silent). Arm a bounded
        // fallback: if the call is still active and no session owns audio,
        // self-activate so an answered call can never be silent.
        armActivationFallback(callId: callId, gen: gen)
        mediaTask = Task { [weak self] in
            guard let self else { return }
            do {
                try await self.establishMedia(
                    callId: callId, uuid: uuid, generation: gen,
                    selfManagedAudio: selfManagedAudio)
            } catch is CancellationError {
                return
            } catch {
                guard gen == self.generation else { return }
                let message = (error as? MediaError)?.errorDescription ?? "音频连接失败。"
                await self.failActiveCall(message: message)
            }
        }
    }

    // MARK: Hold / resume

    func holdActive() {
        Task { [weak self] in
            guard let self, let callId = self.activeGatewayId else { return }
            await self.holdCall(callId)
        }
    }

    func resume(callId: String) {
        Task { [weak self] in
            try? await self?.resumeCall(callId)
        }
    }

    /// Holds a call. `notReady` (v1 gateway) is a graceful no-op.
    private func holdCall(_ callId: String) async {
        guard let accepted = try? await requestHold(callId), accepted else { return }
        if activeGatewayId == callId {
            _ = bumpGeneration()
            parkActiveCall(callId)
        } else if var entry = tracked[callId] {
            entry.held = true
            tracked[callId] = entry
            delegate?.callGroupChanged()
        }
        await reportHeldLocally(callId, held: true)
    }

    /// CallKit/CallDriver `setHeld`; throws on real gateway failures so the
    /// provider can fail the CXSetHeldCallAction.
    func setHeld(callId: String, held: Bool) async throws {
        if held {
            guard tracked[callId] != nil else { throw APIError.notReady("未知通话。") }
            let accepted = try await requestHold(callId)
            guard accepted else { return }
            if activeGatewayId == callId {
                _ = bumpGeneration()
                parkActiveCall(callId)
            } else if var entry = tracked[callId] {
                entry.held = true
                tracked[callId] = entry
                delegate?.callGroupChanged()
            }
            await reportHeldLocally(callId, held: true)
        } else {
            _ = try await resumeCall(callId)
        }
    }

    @discardableResult
    private func resumeCall(_ callId: String) async throws -> Bool {
        guard tracked[callId]?.held == true else { return false }
        if let active = activeGatewayId, active != callId {
            _ = try await requestHold(active)
            guard activeGatewayId == active else { return false }
            _ = bumpGeneration()
            parkActiveCall(active)
            try await resumeTarget(callId, gen: generation)
            return true
        }
        let gen = bumpGeneration()
        try await resumeTarget(callId, gen: gen)
        return true
    }

    private func resumeTarget(_ callId: String, gen: UInt64) async throws {
        try await api.resume(callId: callId, idempotencyKey: UUID().uuidString)
        guard gen == generation else { throw CancellationError() }
        let uuid = await registry.uuid(for: callId)
            ?? knownUUIDs.first(where: { $0.value == callId })?.key
            ?? CallIdentifier.callKitUUID(for: callId)
        activate(callId: callId, uuid: uuid, gen: gen)
    }

    @discardableResult
    private func requestHold(_ callId: String) async throws -> Bool {
        do {
            try await api.hold(callId: callId, idempotencyKey: UUID().uuidString)
            return true
        } catch APIError.notReady {
            return false
        }
    }

    /// Moves the active call into the held set locally: cancels its monitors
    /// and releases its media without touching the gateway.
    private func parkActiveCall(_ callId: String) {
        monitorTask?.cancel()
        monitorTask = nil
        mediaTask?.cancel()
        mediaTask = nil
        self.cancelMediaRecovery()
        monitorStarted = false
        media?.close()
        media = nil
        wsMedia?.close()
        wsMedia = nil
        stagedPreviousRelay?.closeWithoutAudio()
        stagedPreviousRelay = nil
        route?.teardown()
        route = nil
        latestMedia = .idle
        if var entry = tracked[callId] {
            entry.held = true
            if entry.record == nil { entry.record = latestGateway }
            tracked[callId] = entry
        } else if let record = latestGateway {
            tracked[callId] = TrackedCall(record: record, held: true, muted: muted)
        }
        activeGatewayId = nil
        latestGateway = nil
        delegate?.callGroupChanged()
        publishAudioStatus(nil)
    }

    private func reportHeldLocally(_ callId: String, held: Bool) async {
        guard let uuid = await registry.uuid(for: callId)
            ?? knownUUIDs.first(where: { $0.value == callId })?.key else { return }
        callKit.reportHeld(uuid: uuid, held: held)
    }

    // MARK: Conference

    func mergeHeldCalls() {
        Task { [weak self] in
            guard let self else { return }
            try? await self.mergeHeldCallsAsync()
        }
    }

    /// Merges the active call plus held calls (2...3 legs) into a conference
    /// this device hosts, then brings up the single host media leg. On any API
    /// error the existing calls are left untouched. Throws so a CallKit
    /// grouping action can fail instead of pretending the merge happened.
    func mergeHeldCallsAsync() async throws {
        // Already grouped: the requested end state holds, so this is a no-op.
        guard conference == nil else { return }
        var ids: [String] = []
        if let active = activeGatewayId,
           let record = tracked[active]?.record ?? latestGateway,
           !record.isFinished,
           tracked[active]?.held != true {
            ids.append(active)
        }
        for record in heldCallRecords where !ids.contains(record.id) {
            ids.append(record.id)
        }
        guard (2...3).contains(ids.count) else {
            throw APIError.notReady("需要两路或三路通话才能合并。")
        }

        let requestGen = generation
        // A recording running on any merged call is finalized by the gateway
        // as a completed segment; the conference starts unrecorded, so the
        // caller must say so instead of silently dropping the red indicator.
        let wasRecording = ids.contains { id in
            if id == activeGatewayId {
                return (tracked[id]?.record ?? latestGateway)?.recordingInProgress == true
            }
            return tracked[id]?.record?.recordingInProgress == true
        }
        let record = try await api.merge(calls: ids, idempotencyKey: UUID().uuidString)
        guard requestGen == generation, conference == nil else { throw CancellationError() }

        let gen = bumpGeneration()
        adoptConference(record)
        do {
            try await establishConferenceMedia(conference: record, generation: gen)
        } catch {
            if error is CancellationError { throw error }
            // The conference exists server-side but has no host audio: close it
            // and leave the individual calls as they were.
            conference = nil
            heldConferenceLegs.removeAll()
            delegate?.callGroupChanged()
            try? await api.closeConference(id: record.id, idempotencyKey: UUID().uuidString)
            throw error
        }
        if wasRecording {
            delegate?.conferenceRecordingInterruptedByMerge()
        }
    }

    // MARK: Conference recording (host device only)

    /// Starts or stops the gateway-side manual conference segment. The
    /// gateway refuses non-host devices, so only the conference owner can
    /// ever reach this path.
    func setConferenceRecording(_ active: Bool) async throws {
        guard let record = conference else {
            throw APIError.notReady("没有进行中的会议。")
        }
        let rec: RecordingRecord
        if active {
            rec = try await api.startConferenceRecording(conferenceId: record.id)
        } else {
            rec = try await api.stopConferenceRecording(conferenceId: record.id)
        }
        guard conference?.id == record.id else { throw CancellationError() }
        applyConferenceRecordingState(
            id: rec.id, state: rec.state, durationMs: rec.durationMs, conferenceId: record.id
        )
        delegate?.callGroupChanged()
    }

    /// Applies one key-scoped `recording.*` event for the current conference
    /// so the in-call indicator converges without waiting for the poll.
    func applyConferenceRecording(_ rec: RecordingRecord) {
        guard let conference,
              rec.conferenceId == conference.id || rec.callId == conference.id else { return }
        applyConferenceRecordingState(
            id: rec.id, state: rec.state, durationMs: rec.durationMs, conferenceId: conference.id
        )
        delegate?.callGroupChanged()
    }

    private func applyConferenceRecordingState(id: String, state: String, durationMs: Int64, conferenceId: String) {
        guard var current = conference, current.id == conferenceId else { return }
        current.recordingId = id
        current.recordingState = state
        current.recordingDurationMs = durationMs
        conference = current
    }

    private func adoptConference(_ record: ConferenceRecord) {        conference = record
        for leg in record.legs {
            var entry = tracked[leg.id] ?? TrackedCall(record: leg, held: false, muted: false)
            entry.record = leg
            entry.held = false
            tracked[leg.id] = entry
        }
        // Stale per-call media work must not assign or close sessions after
        // the conference host leg takes over the audio path.
        mediaTask?.cancel()
        mediaTask = nil
        self.cancelMediaRecovery()
        monitorTask?.cancel()
        monitorTask = nil
        monitorStarted = false
        media?.close()
        media = nil
        wsMedia?.close()
        wsMedia = nil
        route?.setConferenceLocked(true)
        onRouteState?(CallRouteState(mode: routeModeDefault, active: .none,
                                      switching: false, probing: false,
                                      directDegraded: false, conferenceLocked: true,
                                      rttSeconds: nil, notice: nil, offersAutoFallback: false))
        route?.teardown()
        route = nil
        delegate?.callGroupChanged()
    }

    private func establishConferenceMedia(conference record: ConferenceRecord, generation gen: UInt64,
                                          recoveryProof: MediaRecoveryController.ProofIdentity? = nil) async throws {
        guard let firstLeg = record.legs.first?.id else {
            throw APIError.notReady("会议没有可用的通话。")
        }
        let ice = try await api.iceConfiguration(callId: firstLeg)
        guard gen == generation else { throw CancellationError() }

        // Conferences prefer the same WSS transport so a merge on a cellular
        // client never forces media back onto the unreachable ICE path.
        if ice.mediaTransports?.contains("ws") == true {
            try await establishWSConferenceMedia(conference: record, generation: gen,
                                                 recoveryProof: recoveryProof)
            return
        }

        // Build-49: the candidate is staged (built + negotiated) WITHOUT
        // touching the installed session or audio, then adopted only after it
        // actually reports CONNECTED — exactly the 1:1 direct-recovery
        // ordering, so a conference recovery never creates parallel audio
        // owners and a failed candidate never disturbs the incumbent.
        let session = mediaProvider.makeSession()
        let previous = media
        // Fresh-media proof is CAPTURED here at creation (build 49): the
        // closure never looks the identity up at delivery time, so a frame
        // queued before the attempt or from a superseded transport cannot
        // prove this attempt; only the INSTALLED session may deliver it.
        session.setRecoveryFreshMedia { [weak self, weak session] in
            Task { @MainActor in
                guard let self, let session, self.media === session,
                      let proof = recoveryProof, proof.callId == record.id else { return }
                self.markRecoveryFreshMedia(proof: proof, gen: gen)
            }
        }
        session.onState = { [weak self, weak session] state in
            Task { @MainActor in
                guard let self, let session, gen == self.generation,
                      self.conference?.id == record.id,
                      self.media === session || state == .connected else { return }
                self.latestMedia = state
                switch state {
                case .connected:
                    // Recovery clears on fresh media, not on this handshake.
                    // The conference leg carries host audio now; the candidate
                    // adoption below closes per-call media no longer needed.
                    self.publishPhase()
                    self.delegate?.callGroupChanged()
                case .disconnected, .failed:
                    // Build 48/49: a failed conference transport is
                    // recoverable within the same bounded 45 s window; the
                    // pre-adoption candidate failure arms the SAME episode.
                    self.scheduleConferenceMediaFailure(conferenceId: record.id, gen: gen)
                default:
                    self.publishPhase()
                }
            }
        }
        session.onQuality = { [weak self, weak session] quality in
            Task { @MainActor in
                guard let self, let session, gen == self.generation,
                      self.media === session else { return }
                self.onQuality?(quality)
            }
        }

        let offer = try await session.makeConferenceOffer(ice: ice, relayOnly: transport == "tailnet")
        guard gen == generation else {
            session.close()
            throw CancellationError()
        }
        let answer = try await api.conferenceOffer(
            conferenceId: record.id, sdp: offer, idempotencyKey: UUID().uuidString
        )
        guard gen == generation else {
            session.close()
            throw CancellationError()
        }
        try await session.applyAnswer(answer.sdp)
        guard gen == generation else {
            session.close()
            throw CancellationError()
        }
        // Staged readiness: adopt ONLY once the candidate actually reports
        // CONNECTED (bounded). Until then the incumbent stays installed and no
        // second audio graph is started; a candidate that never connects is
        // closed and the next backoff attempt runs.
        let connectDeadline = Date().addingTimeInterval(3)
        while session.currentMediaState != .connected, Date() < connectDeadline {
            try? await Task.sleep(nanoseconds: 100_000_000)
            guard gen == generation, !ended, conference?.id == record.id else {
                session.close()
                return
            }
        }
        guard session.currentMediaState == .connected else {
            session.close()
            return
        }
        // Exclusive adoption: install the candidate, THEN retire the incumbent
        // (ordered single-owner swap; identical to the 1:1 direct recovery).
        media = session
        previous?.close()
        if let activated = AudioSessionBridge.shared.activeSession {
            session.audioActivated(with: activated)
        }
        session.setMicMuted(muted)
        if speaker { try? session.setSpeakerphone(true) }
        latestMedia = .connected
        publishPhase()
        self.delegate?.callGroupChanged()
        startConferenceMonitor(conferenceId: record.id, gen: gen)
    }

    /// WSS host audio for a hosted conference: one socket carries the mixed
    /// host leg; per-leg hold/merge semantics stay server-side.
    ///
    /// Build-49 ordering (matches the proven 1:1 staged handover): the new
    /// socket handshakes WITHOUT audio ownership and WITHOUT replacing the
    /// installed `wsConferenceMedia`; only the exclusive promotion installs
    /// it, starts its graph, and then retires the previous transport — so a
    /// conference recovery (and the initial establish) never runs two audio
    /// owners, a failed candidate closes alone, and every callback is fenced
    /// by the exact session instance plus the captured recovery identity.
    private func establishWSConferenceMedia(conference record: ConferenceRecord, generation gen: UInt64,
                                            recoveryProof: MediaRecoveryController.ProofIdentity? = nil) async throws {
        let denoise = DenoisePreferenceStore.shared.choice(for: routeGatewayID)
        let request = try await api.conferenceMediaWebSocketRequest(conferenceId: record.id, denoise: denoise)
        guard gen == generation else { throw CancellationError() }

        let session = wsMediaFactory()
        let previous = wsConferenceMedia
        // Fresh-media proof is CAPTURED at attach creation (build 49), never
        // looked up at delivery time: a frame queued before the attempt, or
        // from a superseded socket, cannot prove this attempt; only the
        // INSTALLED session may deliver it.
        session.onInboundRealFrame = { [weak self, weak session] in
            Task { @MainActor in
                guard let self, let session, self.wsConferenceMedia === session,
                      let proof = recoveryProof, proof.callId == record.id else { return }
                self.markRecoveryFreshMedia(proof: proof, gen: gen)
            }
        }
        session.onState = { [weak self, weak session] state in
            Task { @MainActor in
                guard let self, let session, gen == self.generation,
                      self.conference?.id == record.id,
                      self.wsConferenceMedia === session else { return }
                self.latestMedia = state
                switch state {
                case .connected:
                    // Fresh media clears recovery, not the handshake alone.
                    self.publishPhase()
                    self.delegate?.callGroupChanged()
                case .disconnected, .failed:
                    // Build 48/49 bounded recovery for the WSS conference
                    // host. Repeated loss while an episode is live is
                    // ignored by the controller (absolute deadline).
                    self.scheduleConferenceMediaFailure(conferenceId: record.id, gen: gen)
                default:
                    self.publishPhase()
                }
            }
        }
        session.onQuality = { [weak self, weak session] quality in
            Task { @MainActor in
                guard let self, let session, gen == self.generation,
                      self.wsConferenceMedia === session else { return }
                self.onQuality?(quality)
            }
        }
        // Stage: handshake first, no audio ownership, no replacement.
        session.markAudioStaged()
        do {
            try await session.connect(request: request)
        } catch {
            session.closeWithoutAudio()
            throw error
        }
        guard gen == generation, conference?.id == record.id else {
            session.closeWithoutAudio()
            throw CancellationError()
        }
        // Exclusive promotion: install the session, then start its graph, then
        // retire the previous transport — exactly one audio owner at a time.
        wsConferenceMedia = session
        session.promoteAudioOwnership()
        session.graphStartFailureNonfatal = true
        defer { session.graphStartFailureNonfatal = false }
        var started = false
        if let activated = AudioSessionBridge.shared.activeSession {
            started = session.startOwnedAudioGraph(with: activated)
        }
        // A delivered activation replay may have started the graph while the
        // start above fenced: that is success ONLY against this exact call,
        // generation and installed session.
        if !started, session.isGraphRunning,
           gen == self.generation, !ended, conference?.id == record.id,
           wsConferenceMedia === session {
            started = true
        }
        guard started else {
            // Roll back: the candidate never took audio; restore the previous
            // conference transport untouched and let the episode continue.
            if wsConferenceMedia === session { wsConferenceMedia = previous }
            session.closeWithoutAudio()
            DiagnosticsCensus.shared.increment("call.conferencePromotionRolledBack")
            publishPhase()
            return
        }
        session.setMicMuted(muted)
        if speaker { try? session.setSpeakerphone(true) }
        previous?.retireAfterHandover()
        latestMedia = .connected
        publishPhase()
        self.delegate?.callGroupChanged()
        startConferenceMonitor(conferenceId: record.id, gen: gen)
    }

    private func scheduleConferenceMediaFailure(conferenceId: String, gen: UInt64) {
        if mediaRecovery == nil {
            mediaRecovery = MediaRecoveryController(policy: MediaRecoveryPolicy(window: mediaRecoveryWindow))
        }
        guard let mediaRecovery, !mediaRecovery.isActive else { return }
        // Conference host reconnects the same conference transport; direct
        // probing is not used, so only the relay route is attempted.
        publishPhase()
        mediaRecovery.begin(
            callId: conferenceId, mode: .relay, current: .relay, alternateAvailable: false,
            onAttempt: { [weak self] attempt in
                // Awaited by the controller (build 49): conference attempts
                // are strictly serialized; the attempt ends before the next
                // backoff, never an unawaited overlapping re-establish. The
                // attempt's full identity is captured for fresh-media proof.
                guard let self, gen == self.generation,
                      self.conference?.id == conferenceId else { return }
                await self.performConferenceRecoveryAttempt(
                    conferenceId: conferenceId, gen: gen, proof: attempt.identity)
            },
            onExpire: { [weak self] in
                // Inline on the main actor (no queued Task): a queued
                // fresh-media callback cannot lose a race to expiry.
                guard let self, gen == self.generation,
                      self.conference?.id == conferenceId else { return }
                await self.failConference(message: String(localized: "会议音频长时间未恢复，已结束。"))
            })
    }

    /// One serialized conference recovery attempt: re-establish the SAME
    /// conference transport (WSS when advertised, else direct) with the
    /// attempt identity captured for fresh-media proof. All throw paths
    /// close only the failed candidate; the incumbent keeps the call.
    private func performConferenceRecoveryAttempt(
        conferenceId: String, gen: UInt64,
        proof: MediaRecoveryController.ProofIdentity
    ) async {
        guard gen == generation, conference != nil else { return }
        if let record = self.conference {
            do {
                try await self.establishConferenceMedia(
                    conference: record, generation: gen, recoveryProof: proof)
            } catch is CancellationError {
                return
            } catch {
                AppLog.call.notice("conference recovery attempt failed: \(error)")
            }
        }
    }

    /// Media-failure deadline for a hosted conference: claim terminal and
    /// tear down LOCALLY first (mirrors failActiveCall), then let
    /// `endAllCalls` fire the SINGLE best-effort server close off the
    /// critical path. A stalled close RPC must never keep the conference,
    /// its UI or its media alive past the deadline, no second close may be
    /// recorded, and no new recovery episode may begin while expiry is
    /// awaiting the network.
    private func failConference(message: String) async {
        publishFailed(message)
        await endAllCalls()
    }

    private func startConferenceMonitor(conferenceId: String, gen: UInt64) {
        monitorTask?.cancel()
        monitorTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 1_500_000_000)
                guard let self, gen == self.generation, self.conference?.id == conferenceId else { return }
                do {
                    let updated = try await self.api.conference(id: conferenceId)
                    guard gen == self.generation, self.conference?.id == conferenceId else { return }
                    await self.applyConferenceSnapshot(updated)
                    if self.conference == nil { return }
                } catch APIError.http(let status, _, _) where status == 404 || status == 405 {
                    await self.dissolveConference(remaining: [])
                    return
                } catch APIError.notReady {
                    return
                } catch {
                    // Transient failure: keep polling the same conference.
                }
            }
        }
    }

    private func applyConferenceSnapshot(_ updated: ConferenceRecord) async {
        guard conference?.id == updated.id else { return }
        if updated.legs.count <= 1 {
            await dissolveConference(remaining: updated.legs)
            return
        }
        conference = updated
        for leg in updated.legs {
            var entry = tracked[leg.id] ?? TrackedCall(record: leg, held: false, muted: false)
            entry.record = leg
            tracked[leg.id] = entry
        }
        delegate?.callGroupChanged()
    }

    /// Server dissolved the conference (or only one leg remains): close the
    /// host leg and let a surviving call continue as an ordinary call.
    private func dissolveConference(remaining: [CallRecord]) async {
        let previousLegIDs = Set((conference?.legs ?? remaining).map(\.id))
        for leg in remaining {
            var entry = tracked[leg.id] ?? TrackedCall(record: leg, held: false, muted: false)
            entry.record = leg
            entry.held = false
            tracked[leg.id] = entry
        }
        conference = nil
        heldConferenceLegs.removeAll()
        monitorTask?.cancel()
        monitorTask = nil
        self.cancelMediaRecovery()
        monitorStarted = false
        media?.close()
        media = nil
        wsMedia?.close()
        wsMedia = nil
        stagedPreviousRelay?.closeWithoutAudio()
        stagedPreviousRelay = nil
        closeQUICDirect()
        route?.teardown()
        route = nil
        latestMedia = .idle

        let live = remaining.filter { !$0.isFinished }
        let survivor = live.count == 1 ? live[0] : nil
        for id in previousLegIDs where id != survivor?.id {
            tracked.removeValue(forKey: id)
            await markCallEndedLocally(id, reason: .remoteEnded)
        }

        if let survivor {
            let uuid = await registry.uuid(for: survivor.id)
                ?? knownUUIDs.first(where: { $0.value == survivor.id })?.key
                ?? CallIdentifier.callKitUUID(for: survivor.id)
            let gen = bumpGeneration()
            activate(callId: survivor.id, uuid: uuid, gen: gen)
        } else {
            activeGatewayId = nil
            latestGateway = nil
            // No active call remains: release audio demand so a late
            // interruption/media-reset completion cannot revive audio.
            releaseAudioOwnershipIfIdle()
            delegate?.callGroupChanged()
        }
    }

    func endConferenceLeg(callId: String) {
        Task { [weak self] in await self?.performEndConferenceLeg(callId) }
    }

    private func performEndConferenceLeg(_ callId: String) async {
        guard let record = conference else {
            endSpecificCall(callId, reason: .userHungUp)
            return
        }
        do {
            try await api.removeConferenceLeg(
                conferenceId: record.id, callId: callId, idempotencyKey: UUID().uuidString
            )
        } catch {
            return
        }
        guard conference?.id == record.id else { return }
        let remaining = record.legs.filter { $0.id != callId }
        heldConferenceLegs.remove(callId)
        if remaining.count <= 1 {
            await markCallEndedLocally(callId, reason: .remoteEnded)
            await dissolveConference(remaining: remaining)
        } else {
            tracked.removeValue(forKey: callId)
            await markCallEndedLocally(callId, reason: .remoteEnded)
            conference = updatedConference(record, legs: remaining)
            delegate?.callGroupChanged()
        }
    }

    func holdConferenceLeg(callId: String, held: Bool) {
        Task { [weak self] in
            guard let self, let record = self.conference,
                  record.legs.contains(where: { $0.id == callId }) else { return }
            do {
                try await self.api.setConferenceLegHeld(
                    conferenceId: record.id, callId: callId, held: held,
                    idempotencyKey: UUID().uuidString
                )
            } catch {
                return
            }
            if held { self.heldConferenceLegs.insert(callId) }
            else { self.heldConferenceLegs.remove(callId) }
            self.delegate?.callGroupChanged()
        }
    }

    /// Routes DTMF to one conference leg; a nil id prefers the first leg that
    /// is not on hold.
    func playConferenceDTMF(_ digit: String, callId: String?) {
        guard let record = conference else { return }
        let requested = callId.flatMap { id in
            record.legs.contains(where: { $0.id == id }) ? id : nil
        }
        guard let target = requested
            ?? record.legs.first(where: { !heldConferenceLegs.contains($0.id) })?.id
            ?? record.legs.first?.id else { return }
        Task { [weak self] in
            try? await self?.api.conferenceLegDTMF(
                conferenceId: record.id, callId: target, digit: digit, idempotencyKey: UUID().uuidString
            )
        }
    }

    /// Pulls one leg out of the conference; it resumes as the ordinary active
    /// call, the remaining legs stay held.
    func splitConference(callId: String) {
        Task { [weak self] in
            guard let self else { return }
            try? await self.splitConferenceAsync(callId: callId)
        }
    }

    /// Throwing core of ``splitConference(callId:)`` so a system ungroup action
    /// can fail when the gateway rejects the split.
    func splitConferenceAsync(callId: String) async throws {
        guard let record = conference else { throw APIError.notReady("没有进行中的会议。") }
        try await api.splitConference(
            id: record.id, callId: callId, idempotencyKey: UUID().uuidString
        )
        guard conference?.id == record.id else { throw CancellationError() }
        let selected = record.legs.first(where: { $0.id == callId })
        let remaining = record.legs.filter { $0.id != callId }
        conference = nil
        heldConferenceLegs.removeAll()
        monitorTask?.cancel()
        monitorTask = nil
        self.cancelMediaRecovery()
        monitorStarted = false
        media?.close()
        media = nil
        wsMedia?.close()
        wsMedia = nil
        stagedPreviousRelay?.closeWithoutAudio()
        stagedPreviousRelay = nil
        closeQUICDirect()
        route?.teardown()
        route = nil
        latestMedia = .idle
        for leg in remaining {
            var entry = tracked[leg.id] ?? TrackedCall(record: leg, held: false, muted: false)
            entry.record = leg
            entry.held = true
            tracked[leg.id] = entry
        }
        guard let selected else {
            activeGatewayId = nil
            latestGateway = nil
            delegate?.callGroupChanged()
            return
        }
        var entry = tracked[selected.id] ?? TrackedCall(record: selected, held: false, muted: false)
        entry.record = selected
        entry.held = false
        tracked[selected.id] = entry
        let uuid = await registry.uuid(for: selected.id)
            ?? knownUUIDs.first(where: { $0.value == selected.id })?.key
            ?? CallIdentifier.callKitUUID(for: selected.id)
        let gen = bumpGeneration()
        activate(callId: selected.id, uuid: uuid, gen: gen)
        await reportHeldLocally(selected.id, held: false)
    }

    /// Reconciles a system "leave group" request: a conference leg is split
    /// back out to the ordinary active call, while calls that are not grouped
    /// (or unknown to this device) are already independent and a no-op.
    func ungroup(uuid: UUID) async throws {
        guard let gatewayId = await gatewayId(for: uuid),
              let record = conference,
              record.legs.contains(where: { $0.id == gatewayId }) else { return }
        try await splitConferenceAsync(callId: gatewayId)
    }

    /// Host hangup: closes the conference and ends every CallKit call.
    ///
    /// Build-49 ordering: the terminal state is claimed and EVERYTHING is
    /// torn down LOCALLY first (mirrors failActiveCall); the server close is
    /// best-effort afterwards. A stalled close RPC must never keep the
    /// conference, its UI or its media alive past the media-failure
    /// deadline, and no new recovery episode may begin while expiry is
    /// awaiting the network.
    func endAllCalls() async {
        let record = conference
        conference = nil
        heldConferenceLegs.removeAll()
        let ids = Array(tracked.keys)
        _ = invalidateGeneration()
        tracked.removeAll()
        activeGatewayId = nil
        latestGateway = nil
        latestMedia = .idle
        monitorStarted = false
        ended = true
        AudioSessionBridge.shared.callEnded()
        publishAudioStatus(nil)
        for id in ids {
            await markCallEndedLocally(id, reason: .remoteEnded)
        }
        knownUUIDs.removeAll()
        delegate?.callGroupChanged()
        if let record {
            Task { [api] in
                try? await api.closeConference(id: record.id, idempotencyKey: UUID().uuidString)
            }
        }
    }

    // MARK: Media

    private func establishMedia(
        callId: String, uuid: UUID, generation gen: UInt64, selfManagedAudio: Bool
    ) async throws {
        // Build-66: observe the OS path for THIS call and arm the bounded
        // media-stall backstop. Both the outgoing flow and the answer flow
        // reach media through here, so the wiring is shared; both helpers
        // self-fence on the call generation.
        startPathObservationIfNeeded()
        stopMediaStallWatchdog()
        startMediaStallWatchdogIfNeeded(callId: callId, gen: gen)
        // A call is starting: stop BOTH idle measurement cycles and take
        // ownership of a fresh direct candidate in ONE step, BEFORE any call
        // media handshake. The relay-idle samples are captured first (they
        // stop at this moment) so the warm direct-first decision compares
        // the ready candidate against FRESH relay evidence, not an old RTT.
        // The route controller receives the handoff once routing is built
        // (`routeRelayDidConnect`); until then the coordinator owns it and
        // discards it if the call fails.
        idleRelaySamples = idleRelayProbe?.freshTimestampedSamples(within: 30, now: Date()) ?? []
        let preflight = routePreflight?.handoffForCall()
        pendingPreflightHandoff = preflight
        // The relay probe always loses to a live call: it must not keep a
        // measurement socket while the call's own relay transport carries
        // audio. It is restarted on teardown by the AppModel (foreground and
        // no-live-call gated).
        idleRelayProbe?.stop()
        let ice = try await api.iceConfiguration(callId: callId)
        guard gen == self.generation else { throw CancellationError() }
        // Set the direct-advertised flag up front: the warm direct-first
        // fastpath runs before establishWSMedia (which used to set it).
        directAdvertised = ice.mediaTransports?.contains("ice") == true
        // Build-61: surface the QUIC advertisement for the 直连方式 chooser
        // and the 当前路径 stats (hidden again on non-QUIC gateways).
        onDirectTransportAvailability?(ice)

        // Capability negotiation: when the gateway advertises the
        // authenticated WSS audio transport, use it — it rides the same
        // reachable HTTPS route and is the only media path on cellular
        // networks where the gateway's ICE candidates are LAN-only. There is
        // deliberately no silent ICE fallback: if the socket fails, the call
        // fails truthfully.
        if ice.mediaTransports?.contains("ws") == true {
            // Warm direct-first fastpath (build 38): a fresh, connected,
            // measured preflight handoff is committed BEFORE the call relay
            // attaches when the chosen mode allows it AND fresh measurements
            // show direct is genuinely better (auto) or it was explicitly
            // chosen (direct). Answer/audio are never blocked on a new
            // handshake — the handoff was warmed while idle, and any failure
            // (commit rejection, missing leg, bad audio) falls back to the
            // normal audible relay attach. When the evidence is missing,
            // stale or marginal the relay goes first and the existing
            // bounded promotion/fallback semantics are preserved.
            if let preflight,
               await takeWarmDirectFastpath(
                    preflight: preflight, ice: ice, callId: callId,
                    uuid: uuid, gen: gen, selfManagedAudio: selfManagedAudio) {
                return
            }
            // Re-guard after every await: a hangup/generation bump or a
            // fastpath that ended the call (self-activation failure) must
            // never attach a relay afterwards.
            guard gen == self.generation, activeGatewayId == callId, !Task.isCancelled,
                  !ended else {
                // The replaced/ended call can never consume the candidate.
                self.discardPendingPreflight()
                throw CancellationError()
            }
            // The fastpath did not adopt: its preflight (if declined) must be
            // discarded exactly once. pendingPreflightHandoff still owns it
            // for the relay-first route controller when retained; releases
            // happen in routeRelayDidConnect / discardPendingPreflight.
            try await establishWSMedia(
                callId: callId, uuid: uuid, generation: gen, ice: ice,
                selfManagedAudio: selfManagedAudio)
            // Build-61: the QUIC direct standby attempt rides the just-
            // established WSS control plane (protocol §8: bounded, never
            // blocking the answer; ANY failure keeps the incumbent relay).
            attemptQUICDirect(callId: callId, gen: gen, ice: ice)
            return
        }

        let session = mediaProvider.makeSession()
        media = session
        // CallKit may activate audio before the ICE request returns. Replay the
        // current activation so a newly created media session cannot stay mute.
        // ONLY the explicit direct-answer mode self-activates; a CallKit-owned
        // call must wait for its (possibly delayed) didActivate.
        if let activated = AudioSessionBridge.shared.activeSession {
            session.audioActivated(with: activated)
        } else if selfManagedAudio {
            guard session.activateAudioWithoutCallKit() else {
                session.close()
                throw MediaError.audioActivationFailed
            }
        }
        let relayOnly = transport == "tailnet"
        session.onState = { [weak self] state in
            Task { @MainActor in
                guard let self, gen == self.generation else { return }
                self.latestMedia = state
                switch state {
                case .connected:
                    // Build 48: `.connected` alone no longer cancels recovery;
                    // the first real inbound frame (markRecoveryFreshMedia)
                    // does. An initial call has no episode anyway.
                    self.publishPhase()
                    self.startMonitorIfNeeded(callId: callId, uuid: uuid, gen: gen)
                case .disconnected, .failed:
                    // ICE can flap on a Wi-Fi/cellular handover and a
                    // `.failed` state is recoverable (build 48): enter the
                    // bounded 45 s recovery (immediate + 1/2/4/8 s re-attempts
                    // of the SAME call, current path first) instead of ending
                    // immediately. Explicit/remote ends and revoked auth use
                    // the fail/finish paths, never this one.
                    self.scheduleMediaFailure(
                        callId: callId, gen: gen,
                        message: String(localized: "音频长时间未恢复，已结束通话。"))
                default:
                    self.publishPhase()
                }
            }
        }
        session.onQuality = { [weak self] quality in
            Task { @MainActor in
                guard let self, gen == self.generation else { return }
                self.latestWebRTCQuality = quality
                self.onQuality?(quality)
            }
        }
        // The initial direct establish carries no recovery proof: only a
        // session created FOR a recovery attempt may clear an episode
        // (build 49 — captured identity, never a delivery-time lookup).
        session.setRecoveryFreshMedia(nil)

        let offer = try await session.makeOffer(ice: ice, relayOnly: relayOnly)
        guard gen == self.generation else {
            session.close()
            throw CancellationError()
        }
        let answer = try await api.webRTCOffer(
            callId: callId, sdp: offer, transport: transport, idempotencyKey: UUID().uuidString
        )
        guard gen == self.generation else {
            session.close()
            throw CancellationError()
        }
        try await session.applyAnswer(answer.sdp)
        guard gen == self.generation else {
            session.close()
            throw CancellationError()
        }
        session.setMicMuted(muted)
        if speaker { try? session.setSpeakerphone(true) }
        startMonitorIfNeeded(callId: callId, uuid: uuid, gen: gen)
    }

    private var monitorStarted = false

    /// WSS PCMU media path, selected when the gateway advertises it in `/ice`.
    /// Same lifecycle contract as the WebRTC path: CallKit may have activated
    /// audio before the socket opens (replay it); a direct in-app answer
    /// self-activates; failures end the call truthfully.
    private func establishWSMedia(
        callId: String, uuid: UUID, generation gen: UInt64,
        ice: ICEConfiguration, selfManagedAudio: Bool
    ) async throws {
        directAdvertised = ice.mediaTransports?.contains("ice") == true
        // Forced direct requires a direct-capable gateway; surface the
        // impossibility BEFORE the call settles, while WSS safely carries it.
        if routeModeDefault == .direct, !directAdvertised, route == nil {
            onRouteNotice?(
                String(localized: "当前网关不支持直连，无法按你的选择使用直连；可改用自动模式。"), true)
        }
        try await attachWSSession(
            callId: callId, uuid: uuid, gen: gen, ice: ice, selfManagedAudio: selfManagedAudio)
    }

    /// Builds/connects one WSS session and wires its callbacks. Used for the
    /// initial attach AND for ICE -> WSS rollback after a direct session.
    ///
    /// `ownAudioImmediately` is false during a staged route rollback: the
    /// socket completes its handshake and becomes the READY replacement
    /// WITHOUT starting the capture/playback graph; the caller enables audio
    /// only once the previous transport is retired, so exactly one transport
    /// ever owns the mic. A failed staged attach is closed here and never
    /// replaces `wsMedia`, so the still-working path is untouched.
    @discardableResult
    private func attachWSSession(
        callId: String, uuid: UUID, gen: UInt64, ice: ICEConfiguration,
        selfManagedAudio: Bool, ownAudioImmediately: Bool = true,
        recoveryProof: MediaRecoveryController.ProofIdentity? = nil,
        onCandidate: ((WebSocketCallMedia) -> Void)? = nil
    ) async throws -> WebSocketCallMedia {
        let denoise = DenoisePreferenceStore.shared.choice(for: routeGatewayID)
        let request = try await api.mediaWebSocketRequest(callId: callId, denoise: denoise)
        guard gen == self.generation else { throw CancellationError() }

        let session = wsMediaFactory()
        // Build-66: hand the freshly created candidate to the caller BEFORE
        // its handshake can be awaited, so a fresh-path recovery nudge can
        // cancel it while it is still negotiating (staged recovery only).
        onCandidate?(session)
        // Build-61 QUIC control plane (protocol §3): the `ready
        // transport=quic` greeting and the `promoted` promote reply arrive
        // ON this authenticated WSS socket; they are routed to the QUIC
        // controller, never consumed as this socket's own media handshake.
        session.onTransportControl = { [weak self, weak session] control in
            Task { @MainActor in
                guard let self, let session, gen == self.generation else { return }
                self.handleWSTransportControl(control, session: session)
            }
        }
        // Fresh-media proof (build 48/49): a genuinely received inbound frame
        // clears recovery ONLY when (a) this session is still the INSTALLED
        // wsMedia and (b) the proof identity CAPTURED at attach creation for
        // this attempt still matches. Build 49 removed the delivery-time
        // lookup entirely: a frame queued before creation, or from a
        // superseded socket/previous episode, therefore can never prove a
        // newer attempt. Non-recovery attaches carry no proof and never mark.
        session.onInboundRealFrame = { [weak self, weak session] in
            Task { @MainActor in
                guard let self, let session, let proof = recoveryProof else { return }
                guard self.wsMedia === session, proof.callId == callId else { return }
                self.markRecoveryFreshMedia(proof: proof, gen: gen)
            }
        }
        // Keep the previous transport + its audio ownership until the new
        // socket is confirmed ready. Assign wsMedia only AFTER connect.
        session.onState = { [weak self, weak session] state in
            Task { @MainActor in
                guard let self, gen == self.generation, self.wsMedia === session else { return }
                // An expected socket EOF caused by a successful route commit
                // or a WSS re-attach must never end the call.
                if let route = self.route, route.consumeRelayState(state) {
                    if state == .closed || state == .failed {
                        // Server closed the superseded host: retire it.
                        self.wsMedia = nil
                        session?.retireAfterHandover()
                    }
                    return
                }
                // A STAGED, unpromoted handover attach is not the carrier:
                // its `.connected` must never flip the route to relay
                // (build-44: it published a false healthy relay on a pinned
                // direct route after the rollback), and its mid-stage
                // disconnect must never arm call-level failure while the
                // previous transport still carries audio. The explicit
                // promotion/rollback owns every one of those decisions.
                if session?.isStagedForHandover == true, state != .closed {
                    return
                }
                self.latestMedia = state
                switch state {
                case .connected:
                    // Build 48: recovery clears on the first real inbound
                    // frame (session.onInboundRealFrame), not on this
                    // handshake. Initial calls have no active episode.
                    self.publishPhase()
                    self.startMonitorIfNeeded(callId: callId, uuid: uuid, gen: gen)
                    session?.startPingSampling()
                    self.routeRelayDidConnect(ice: ice, callId: callId)
                case .disconnected, .failed:
                    // Socket drop or failed state: bounded 45 s recovery with
                    // immediate + 1/2/4/8 s same-call re-attempts (build 48),
                    // instead of ending immediately.
                    self.scheduleMediaFailure(callId: callId, gen: gen,
                                              message: String(localized: "音频长时间未恢复，已结束通话。"))
                default:
                    self.publishPhase()
                }
            }
        }
        session.onQuality = { [weak self, weak session] quality in
            Task { @MainActor in
                guard let self, gen == self.generation, self.wsMedia === session else { return }
                // Build-38 false "音频已中断": the relay a route transaction
                // just retired can still emit a terminal quality update
                // (.disconnected/.closed/.failed) when the gateway closes it.
                // That is the EXPECTED handover EOF consumed above for
                // `onState`; the quality channel must swallow it with the same
                // fence, or the retired transport's interruption banner
                // survives on a healthy relay/direct path (the quality object
                // has no recovery transition once the socket is gone).
                if quality.phase == .disconnected || quality.phase == .closed
                        || quality.phase == .failed,
                   let route = self.route,
                   route.consumeRelayState(quality.phase) {
                    return
                }
                self.onQuality?(quality)
            }
        }
        let previous = wsMedia
        if ownAudioImmediately {
            // Initial attach: the session owns this call's only transport, so
            // it is installed (and audio-bound) BEFORE connecting; the
            // `.connected` callback below must see `wsMedia === session`.
            wsMedia = session
            if let activated = AudioSessionBridge.shared.activeSession {
                session.audioActivated(with: activated)
            } else if selfManagedAudio {
                guard session.activateAudioWithoutCallKit() else {
                    wsMedia = previous
                    session.closeWithoutAudio()
                    throw MediaError.audioActivationFailed
                }
            }
            do {
                try await session.connect(request: request)
            } catch {
                if wsMedia === session { wsMedia = previous }
                session.closeWithoutAudio()
                throw error
            }
            guard gen == self.generation else {
                if wsMedia === session { wsMedia = previous }
                session.closeWithoutAudio()
                throw CancellationError()
            }
            previous?.retireAfterHandover()
        } else {
            // Staged rollback attach: handshake first, NO audio and NO
            // `wsMedia` replacement until the exclusive promotion. A failure
            // never disturbs the transport still carrying audio.
            session.markAudioStaged()
            do {
                try await session.connect(request: request)
            } catch {
                session.closeWithoutAudio()
                throw error
            }
            guard gen == self.generation else {
                session.closeWithoutAudio()
                throw CancellationError()
            }
            wsMedia = session
            stagedPreviousRelay = previous
        }
        session.setMicMuted(muted)
        if speaker { try? session.setSpeakerphone(true) }
        return session
    }

    // MARK: Auto / Direct / Relay routing

    /// Creates the per-call route controller once direct is advertised and
    /// the relay is healthy.
    /// Build-38 warm direct-first fastpath.
    ///
    /// A foreground preflight keeps a measured, connected direct peer warm
    /// while the app is idle. Before build 38 every call attached the WSS
    /// relay first and only considered that candidate later (or never in the
    /// same call), so a healthy, already-connected direct path was always
    /// followed by a relay-first interruption banner. When fresh evidence
    /// shows the warm candidate is genuinely better (auto) or the user
    /// explicitly chose direct, this commits it at call start instead.
    ///
    /// Safety:
    /// * The decision needs FRESH measurements on both paths (idle relay
    ///   probe vs the preflight); stale/missing/marginal evidence returns
    ///   false and the normal relay-first flow runs, answer never blocked.
    /// * A commit rejection, missing leg or post-adoption audio-gate failure
    ///   is reconciled by the route controller's existing staged-relay
    ///   fallback, which attaches a fresh WSS host — the call still has
    ///   audio. The candidate is discarded on failure.
    /// * The call relay is NEVER attached on this path (there is no relay to
    ///   retire); the route controller starts directly on the direct peer.
    @discardableResult
    private func takeWarmDirectFastpath(
        preflight handoff: RoutePreflightController.Handoff,
        ice: ICEConfiguration, callId: String, uuid: UUID, gen: UInt64,
        selfManagedAudio: Bool
    ) async -> Bool {
        guard directAdvertised, route == nil, conference == nil else { return false }
        let probe = handoff.probe
        guard probe.connected, probe.mediaReady else { return false }
        // The decision is evaluated HERE (after the /ice await), not from a
        // snapshot taken before it: both paths' samples carry timestamps and
        // are re-validated for freshness at this moment, so a slow request
        // can never act on an aged RTT or a merely-stale `connected` flag.
        let now = Date()
        let directSamples = probe.freshTimestampedQualitySamples(within: 10, now: now)
        let decision = CallRouteController.evaluateWarmDirect(
            mode: routeModeDefault,
            candidateConnected: probe.connected,
            candidateMediaReady: probe.mediaReady,
            directSamples: directSamples,
            relaySamples: idleRelaySamples,
            echoStalls: probe.echoStallCount,
            now: now)
        guard decision.take else {
            let directFresh = directSamples.filter {
                now.timeIntervalSince($0.at) <= 10
            }.count
            DiagnosticsStore.shared.log("route",
                "warm direct-first declined id=\(handoff.preflightId)"
                + " mode=\(routeModeDefault.rawValue) reason=\(decision.reason)"
                + " directSamples=\(directFresh) relaySamples=\(idleRelaySamples.count)"
                + " stalls=\(probe.echoStallCount)")
            return false
        }
        // Re-check liveness immediately before the mutating commit: a hangup
        // or generation bump during evaluation must not start a direct adopt.
        guard gen == generation, activeGatewayId == callId, !Task.isCancelled else {
            return false
        }
        DiagnosticsStore.shared.log("route",
            "warm direct-first commit id=\(handoff.preflightId) reason=\(decision.reason)"
            + " directSamples=\(directSamples.count) relaySamples=\(idleRelaySamples.count)")
        // Commit the device-scoped preflight to THIS call. Unlike the
        // relay-first flow there is no previous WSS host for the gateway to
        // close: the call leg is parked/attached to the live session inside
        // CommitProbe itself (takeParkedLeg/attachLineLeg).
        do {
            try await api.commitMediaProbe(callId: callId, preflightId: handoff.preflightId)
        } catch {
            guard gen == generation else { return false }
            DiagnosticsStore.shared.log("route",
                "warm direct-first commit failed: \(error.localizedDescription); relay-first")
            // The candidate may or may not still be registered server-side;
            // release it locally and let the relay-first route controller do
            // its own bounded probe/promotion for this call.
            probe.cancel()
            pendingPreflightHandoff = nil
            Task { [api] in try? await api.discardMediaPreflight(preflightId: handoff.preflightId) }
            return false
        }
        guard gen == generation, !Task.isCancelled else {
            // The call was torn down while the commit was parked: every
            // generation-bump path (invalidate/activate/park) already discards
            // the pending preflight and its peer server-side; never touch the
            // route or attach a relay afterwards.
            return false
        }
        pendingPreflightHandoff = nil
        // Exclusive local adoption: hand the (already active, for a CallKit
        // answer) audio session to the RTC peer, enable the mic track. A
        // direct in-app answer self-activates; a failed self-activation is a
        // real failure to surface, never a silent call.
        if selfManagedAudio {
            guard probe.activateAudioWithoutCallKit() else {
                probe.cancel()
                await failActiveCall(message: String(localized: "无法启用通话音频，请重试。"))
                return false
            }
        } else if let activated = AudioSessionBridge.shared.activeSession {
            probe.audioSessionActivated(activated)
        }
        probe.adopt(activatedSession: AudioSessionBridge.shared.activeSession)
        probe.setMuted(muted)
        if speaker { try? probe.setSpeakerphone(true) }
        latestMedia = .connected
        self.cancelMediaRecovery()
        // Build the route controller in direct-first mode. The coordinator
        // hands it the committed peer (it is NOT a pending preflight anymore)
        // and the controller owns the bounded audio gate + failure fallback.
        let controller = makeRouteController(callId: callId, ice: ice, initialDirect: probe,
                                             preflight: nil)
        route = controller
        controller.beginWithAdoptedDirect(probe)
        publishPhase()
        startMonitorIfNeeded(callId: callId, uuid: uuid, gen: gen)
        configureHotStandbyIfSupported(ice: ice, callId: callId)
        if standbySupported {
            // Hot-standby direct-first: coordinator-owned bounded gate (the
            // legacy in-controller gate is suppressed for this call).
            startOwnedDirectFirstGate(probe: probe, callId: callId, gen: gen)
        }
        return true
    }

    /// Builds the per-call route controller with all coordinator callbacks.
    /// Shared by the relay-first entry (`initialDirect == nil`) and the warm
    /// direct-first fastpath (the already-committed peer is passed in).
    private func makeRouteController(
        callId: String, ice: ICEConfiguration,
        initialDirect: DirectProbeControlling?,
        preflight: RoutePreflightController.Handoff?
    ) -> CallRouteController {
        return CallRouteController(
            callId: callId,
            initialMode: routeModeDefault,
            api: api,
            ice: ice,
            callbacks: .init(
                activatedAudioSession: { AudioSessionBridge.shared.activeSession },
                isMuted: { [weak self] in self?.muted ?? false },
                isConference: { [weak self] in self?.conference != nil },
                retireRelay: { [weak self] in self?.retireRelayAfterAdoption() },
                stageRelay: { [weak self] proof in
                    // A recovery attempt threads its captured identity
                    // explicitly; a route-internal handover during an open
                    // episode falls back to the pending attempt identity so
                    // its fresh media can prove exactly that attempt.
                    await self?.stageWSMedia(
                        callId: callId,
                        proof: proof ?? self?.pendingRecoveryProof) ?? false
                },
                promoteStagedRelay: { [weak self] peer in
                    guard let self else { return false }
                    // Build-50: an explicitly requested relay fallback /
                    // recovery inside a hot-standby call DEMOTES the direct
                    // peer instead of closing it.
                    let retain = self.hotStandby != nil
                    let ok = await self.promoteStagedWSMedia(
                        retiringDirect: peer, retainRetiringPeer: retain)
                    guard ok, retain, let peer else { return ok }
                    // Track the demoted peer as the direct standby handle so
                    // its freshness is counted (never an unreferenced
                    // orphan); reconcile its fence from the server's actual
                    // roles. When the server has no ice standby the watchdog
                    // rebuilds one on its next round.
                    self.standbyDirect = peer
                    self.armDirectStandbyFreshness(probe: peer)
                    self.standbyFreshAt[.direct] = nil
                    if let callId = self.activeGatewayId,
                       let state = try? await self.api.mediaState(callId: callId),
                       state.standbyIce == true, let token = state.standbyIceToken {
                        self.installStandbyFence(kind: .direct, token: token, epoch: state.epoch)
                    }
                    return ok
                },
                discardPreflight: { [weak self] handoff in
                    handoff.probe.cancel()
                    Task { try? await self?.api.discardMediaPreflight(preflightId: handoff.preflightId) }
                },
                fetchTransport: { [weak self] in
                    guard let self else { return nil }
                    return (try? await self.api.fetchCall(id: callId))?.mediaTransport
                },
                relaySamples: { [weak self] in
                    self?.wsMedia?.freshPingSamples(within: 30) ?? []
                },
                relayLatestSample: { [weak self] in
                    self?.wsMedia?.lastPingSample
                },
                relayTelemetry: { [weak self] in
                    guard let self, let ws = self.wsMedia else { return .unknown }
                    let samples = ws.freshPingSamples(within: 8)
                    return RouteTransportTelemetry(
                        rttSeconds: samples.last,
                        jitterSeconds: MediaRouteAdvisor.jitter(of: samples),
                        lossFraction: nil,
                        localBufferSeconds: ws.playbackBufferSeconds,
                        gatewayBufferSeconds: ws.gatewayBufferSeconds)
                },
                directLossFraction: { [weak self] in
                    self?.latestWebRTCQuality?.packetLoss
                },
                onState: { [weak self] state in
                    Task { @MainActor in
                        self?.onRouteState?(state)
                        self?.delegate?.callGroupChanged()
                    }
                },
                onNotice: { [weak self] message, offersAuto in
                    Task { @MainActor in self?.onRouteNotice?(message, offersAuto) }
                }
            ),
            preflight: preflight,
            initialDirect: initialDirect,
            probeFactory: routeProbeFactory,
            cadence: routeCadence ?? CallRouteController.Cadence(),
            // Build-50 single owner: ownership starts at route creation —
            // same gate as configureHotStandbyIfSupported — so the initial
            // selection task can never race the standby's probe attach.
            hotStandbyOwnsRouting: ice.mediaStandby && conference == nil
        )
    }

    /// Creates the per-call route controller once direct is advertised and
    /// the relay is healthy (relay-first flow).
    private func routeRelayDidConnect(ice: ICEConfiguration, callId: String) {
        let preflight = pendingPreflightHandoff
        pendingPreflightHandoff = nil
        guard directAdvertised, route == nil, conference == nil else {
            // This call cannot adopt a direct candidate: release the probe
            // explicitly instead of leaving it to the server TTL.
            if let preflight { discardPreflightHandoff(preflight) }
            route?.relayDidConnect(wsMedia: wsMedia)
            return
        }
        let hotOwned = ice.mediaStandby && conference == nil
        if hotOwned {
            // Seed the standby attach; the legacy initial selection gets no
            // preflight (it only pins the carrier in hot-owned mode).
            standbySeed = preflight
        }
        let controller = makeRouteController(callId: callId, ice: ice, initialDirect: nil,
                                             preflight: hotOwned ? nil : preflight)
        route = controller
        controller.relayDidConnect(wsMedia: wsMedia)
        configureHotStandbyIfSupported(ice: ice, callId: callId)
    }

    /// Releases a preflight candidate the coordinator owns but this call
    /// cannot adopt (no direct path advertised, routing already built) or the
    /// call ended before routing consumed it: close the peer and tell the
    /// gateway to drop the device-scoped entry. The bounded server TTL is a
    /// backstop, never the primary release.
    private func discardPreflightHandoff(_ handoff: RoutePreflightController.Handoff) {
        handoff.probe.cancel()
        Task { [api] in
            try? await api.discardMediaPreflight(preflightId: handoff.preflightId)
        }
    }

    private func discardPendingPreflight() {
        guard let handoff = pendingPreflightHandoff else { return }
        pendingPreflightHandoff = nil
        discardPreflightHandoff(handoff)
    }

    /// Stops the WSS transport after the gateway atomically adopted the
    /// direct peer. Resets the coordinator's media state and cancels any
    /// grace timer so the EXPECTED old-socket EOF cannot end the promoted call.
    private func retireRelayAfterAdoption() {
        // Build 63: under the three-path authority the WSS socket is the
        // QUIC control plane and the only relay-standby telemetry feed; the
        // ICE commit demoted it server-side (dedicated slots keep it
        // attached). Retiring it here would strand the QUIC authority
        // without control and make the WSS candidate unmeasurable for the
        // rest of the call — keep it attached in the standby role.
        if quicDirect != nil {
            wsMedia?.markHotStandby()
            wsMedia?.startPingSampling()
            latestMedia = .connected
            onQuality?(MediaQuality(phase: .connected))
            publishPhase()
            return
        }
        let old = wsMedia
        wsMedia = nil
        old?.retireAfterHandover()
        // Build-48: adoption does not clear a recovery episode; only fresh
        // media on the adopted transport does (its own proof closure). Keep
        // the episode until then so a silent promoted path still expires.
        mediaRecoveryTask?.cancel()
        mediaRecoveryTask = nil
        latestMedia = .connected
        // Clear any interruption banner: the relay just retired is the source
        // of the last MediaQuality; the adopted direct peer carries the call
        // now and its health surfaces through route telemetry (build 38:
        // never show "音频中断" while the active path is healthy).
        onQuality?(MediaQuality(phase: .connected))
        publishPhase()
    }

    // MARK: Hot standby lifecycle (build 50)

    /// Creates the hot-standby controller once the gateway capability is
    /// known for this call, arms the standby for the non-primary direction
    /// and starts the watchdog (freshness tracking, rebuild-on-loss, and
    /// the composite auto-choice scorer over LIVE metrics).
    private func configureHotStandbyIfSupported(ice: ICEConfiguration, callId: String) {
        guard ice.mediaStandby, conference == nil, hotStandby == nil else { return }
        standbySupported = true
        let controller = HotStandbyController(callbacks: makeHotStandbyCallbacks(callId: callId))
        hotStandby = controller
        let gen = generation
        let primaryIsDirect = route?.activeTransportIsDirect == true || media != nil
        Task { @MainActor [weak self] in
            guard let self, gen == self.generation, !self.ended,
                  self.activeGatewayId == callId, self.hotStandby === controller else { return }
            if primaryIsDirect {
                await self.attachRelayStandby(callId: callId)
            } else {
                await self.attachDirectStandby(callId: callId)
            }
            guard gen == self.generation, !self.ended,
                  self.activeGatewayId == callId, self.hotStandby === controller else { return }
            self.startStandbyWatchdog(callId: callId, gen: gen)
            // Honor an initial FORCED selection the carrier does not match
            // (e.g. persisted .direct on a relay-first call): once the
            // selected standby proves fresh it is promoted exactly once.
            self.honorInitialForcedSelection(
                callId: callId, gen: gen, controller: controller)
        }
    }

    private func makeHotStandbyCallbacks(callId: String) -> HotStandbyController.Callbacks {
        // The OWNER generation is captured at CONSTRUCTION: a callback that
        // begins late (old call object reused across a generation bump)
        // still fences against the generation it was built for, and every
        // async boundary re-checks it plus the active-call identity.
        let ownerGeneration = generation
        let alive: () -> Bool = { [weak self] in
            guard let self else { return false }
            return ownerGeneration == self.generation && !self.ended
                && self.activeGatewayId == callId
        }
        return .init(
            currentPrimary: { [weak self] in
                guard let self, alive() else { return .relay }
                return (self.route?.activeTransportIsDirect == true || self.media != nil) ? .direct : .relay
            },
            standbyFresh: { [weak self] kind in
                guard let self, alive(), let at = self.standbyFreshAt[kind] else { return false }
                return Date().timeIntervalSince(at) <= 5
            },
            promoteCommand: { [weak self] fence, nonce in
                guard let self, alive() else { return nil }
                let callbackGen = ownerGeneration
                do {
                    let response = try await self.api.promoteStandby(
                        callId: callId, transport: fence.kind == .relay ? "ws" : "ice",
                        token: fence.token, expectEpoch: fence.epoch, nonce: nonce)
                    // Identity fence BEFORE any mutation: a stale ack from a
                    // dead call must not install fences for a new one.
                    guard callbackGen == self.generation, alive() else { return nil }
                    if let current = response.currentEpoch, current != response.epoch {
                        // A replay after an intervening swap (or a raced
                        // command): the recorded outcome is stale. Reconcile
                        // against the SERVER'S ACTUAL ROLES + the server's
                        // CURRENT standby identity tokens, and only proceed
                        // when the requested swap is the one in effect. An
                        // old-epoch token is NEVER paired with a new epoch.
                        guard let state = try? await self.api.mediaState(callId: callId) else { return nil }
                        guard callbackGen == self.generation, alive() else { return nil }
                        let requestedPrimary = fence.kind == .relay ? "ws" : "ice"
                        guard state.primary == requestedPrimary else { return nil }
                        if state.standbyIce == true, let token = state.standbyIceToken {
                            self.installStandbyFence(kind: .direct, token: token, epoch: state.epoch)
                        }
                        if state.standbyWs == true, let token = state.standbyWsToken {
                            self.installStandbyFence(kind: .relay, token: token, epoch: state.epoch)
                        }
                        return state.epoch
                    }
                    if let demoted = response.standby, let token = response.standbyToken {
                        let kind: HotStandbyController.RouteKind = demoted == "ice" ? .direct : .relay
                        self.installStandbyFence(kind: kind, token: token, epoch: response.epoch)
                    }
                    return response.epoch
                } catch {
                    AppLog.media.notice("standby promote command failed: \(error)")
                    return nil
                }
            },
            refreshServerRoles: { [weak self] in
                guard let self, alive() else { return nil }
                let callbackGen = ownerGeneration
                guard let state = try? await self.api.mediaState(callId: callId) else { return nil }
                // Identity fence BEFORE installing the server's tokens.
                guard callbackGen == self.generation, alive() else { return nil }
                // Install the server's CURRENT standby identity tokens too
                // (coordinator review): an ack-loss reconcile must recover
                // the demoted token exactly, in BOTH reconcile paths.
                if state.standbyIce == true, let token = state.standbyIceToken {
                    self.installStandbyFence(kind: .direct, token: token, epoch: state.epoch)
                }
                if state.standbyWs == true, let token = state.standbyWsToken {
                    self.installStandbyFence(kind: .relay, token: token, epoch: state.epoch)
                }
                return (state.primary == "ice" ? .direct : .relay, state.epoch)
            },
            transferAudioOwnership: { [weak self] kind in
                guard let self, alive() else { return false }
                if kind == .relay {
                    // Relay promotion: the standby relay takes audio ownership
                    // through the proven staged machinery; the direct peer is
                    // DEMOTED to standby (never closed).
                    return await self.promoteRelayStandbyToPrimary()
                }
                return await self.promoteDirectStandbyToPrimary()
            },
            demoteCurrentToStandby: { [weak self] in
                // The promote command already demoted the old primary
                // server-side and the ack's token was installed by
                // promoteCommand; nothing further to do locally.
                nil
            },
            canMaintainStandby: { [weak self] in
                guard let self else { return false }
                return alive() && self.activeGatewayId != nil
            },
            onState: { [weak self] state in
                Task { @MainActor in
                    // A queued publish from an old call must not overwrite
                    // the new call's published standby state.
                    guard let self, alive() else { return }
                    self.hotStandbyState = state
                    self.onHotStandbyState?(state)
                    self.delegate?.callGroupChanged()
                }
            }
        )
    }

    private func installStandbyFence(kind: HotStandbyController.RouteKind, token: String, epoch: UInt64) {
        let fence = HotStandbyController.StandbyFence(kind: kind, token: token, epoch: epoch)
        standbyFences[kind] = fence
        hotStandby?.installStandby(fence)
    }

    /// Attaches the relay in the hot-standby role: a second authenticated
    /// WSS socket (standby=1) that receives the mirrored downlink mix, never
    /// renders, and sends at most a bounded silence pulse. The gateway's
    /// ready greeting MUST carry `standby` + the fencing token; anything
    /// else means an old server (which would have evicted the primary —
    /// detected by the primary's loss) and this socket is dropped.
    private func attachRelayStandby(callId: String) async {
        let gen = generation
        do {
            let request = try await api.mediaWebSocketStandbyRequest(callId: callId)
            guard gen == generation, !ended else { return }
            let session = wsMediaFactory()
            try await session.connect(request: request)
            guard gen == generation, !ended else { session.closeWithoutAudio(); return }
            guard session.lastReadyStandbyToken != nil, let epoch = session.lastReadyEpoch else {
                // Old gateway ignored the parameter and replaced the primary.
                session.closeWithoutAudio()
                return
            }
            session.markHotStandby()
            session.startPingSampling()
            standbyRelay = session
            armRelayStandbyFreshness(session: session)
            installStandbyFence(kind: .relay, token: session.lastReadyStandbyToken!, epoch: epoch)
            AppLog.media.debug("hot standby: relay attached (epoch \(epoch))")
        } catch {
            AppLog.media.notice("relay standby attach failed: \(error)")
            DiagnosticsStore.shared.log("route", "relay standby attach failed: \(error)")
        }
    }

    private func armRelayStandbyFreshness(session: WebSocketCallMedia) {
        session.onStandbyFreshMedia = { [weak self, weak session] in
            Task { @MainActor in
                guard let self, let session, self.standbyRelay === session else { return }
                self.standbyFreshAt[.relay] = Date()
                self.hotStandby?.refreshState()
            }
        }
    }

    /// Recurring direct-standby readiness proof: the one-shot fresh-media
    /// hook RE-ARMS after every fire, so each observed inbound advance
    /// refreshes the timestamp (a stale transport simply stops refreshing
    /// and goes unproven). Pre-commit probe echo NEVER counts: the baseline
    /// is the commit ack and only post-adoption packets flow through the
    /// live-session read path.
    private func armDirectStandbyFreshness(probe: any DirectProbeControlling) {
        probe.setRecoveryFreshMedia { [weak self, weak probe] in
            Task { @MainActor in
                guard let self, let probe, self.standbyDirect === probe else { return }
                self.standbyFreshAt[.direct] = Date()
                self.hotStandby?.refreshState()
                // Re-arm for the next advance (one-shot by design).
                self.armDirectStandbyFreshness(probe: probe)
            }
        }
    }

    /// Installs the direct path in the hot-standby role: a measured call
    /// probe committed into the gateway's standby slot (the primary is
    /// untouched). The freshness clock starts UNPROVEN at the commit ack —
    /// only recurring post-adoption packet advances prove it.
    private func attachDirectStandby(callId: String) async {
        let gen = generation
        let seed = standbySeed
        standbySeed = nil
        do {
            if let seed, seed.probe.connected, seed.probe.mediaReady {
                // Warm preflight fast path: commit the warmed probe straight
                // into the standby slot (no second probe offer/attach can
                // race it).
                let fence = try await api.commitMediaProbe(
                    callId: callId, preflightId: seed.preflightId, asStandby: true)
                guard gen == generation, !ended else { seed.probe.cancel(); return }
                installDirectStandby(probe: seed.probe, fence: fence, warm: true)
                return
            }
            if let seed { discardPreflightHandoff(seed) }
            let ice = try await api.iceConfiguration(callId: callId)
            guard gen == generation, !ended, ice.mediaStandby else { return }
            let probe = routeProbeFactory()
            // Build 68: incremental offer + exchange when the gateway
            // advertises trickle; full SDP otherwise.
            var trickleProbe: TrickleProbeCapable?
            if ice.mediaTrickle, api is TrickleMediaAPI, let capable = probe as? TrickleProbeCapable {
                trickleProbe = capable
            }
            let offer: String
            if let trickleProbe {
                guard let built = try? await trickleProbe.makeOfferTrickle(ice: ice) else {
                    probe.cancel()
                    return
                }
                offer = built.sdp
            } else {
                guard let made = try? await probe.makeOffer(ice: ice) else { probe.cancel(); return }
                offer = made
            }
            let answer: WebRTCAnswer
            if trickleProbe != nil, let trickleAPI = api as? TrickleMediaAPI {
                answer = try await trickleAPI.attachMediaProbe(callId: callId, sdp: offer, trickle: true)
            } else {
                answer = try await api.attachMediaProbe(callId: callId, sdp: offer)
            }
            try await probe.applyAnswer(answer.sdp)
            if let trickleProbe, let attachId = answer.attachId {
                let exchange = TrickleCandidateExchange { message in
                    DiagnosticsStore.shared.log("route", message)
                }
                standbyTrickleExchange?.cancel()
                standbyTrickleExchange = exchange
                exchange.start(
                    attachId: attachId,
                    initialRemoteCursor: answer.candidateCursor ?? 0,
                    localCandidates: { [weak trickleProbe] in
                        trickleProbe?.locallyGatheredCandidates ?? []
                    },
                    localGatheringComplete: { [weak trickleProbe] in
                        trickleProbe?.gatheringFinished ?? true
                    },
                    post: { [weak self] attachId, candidates in
                        guard let trickleAPI = self?.api as? TrickleMediaAPI else {
                            throw APIError.notReady("当前配对不支持增量直连协商。")
                        }
                        try await trickleAPI.postProbeCandidates(callId: callId, attachId: attachId,
                                                                 candidates: candidates)
                    },
                    poll: { [weak self] attachId, after in
                        guard let trickleAPI = self?.api as? TrickleMediaAPI else {
                            throw APIError.notReady("当前配对不支持增量直连协商。")
                        }
                        return try await trickleAPI.pollProbeCandidates(callId: callId,
                                                                        attachId: attachId, after: after)
                    },
                    apply: { [weak trickleProbe] candidate in
                        trickleProbe?.addRemoteCandidate(candidate)
                    })
            }
            guard gen == generation, !ended else { probe.cancel(); return }
            // The gateway requires a CONNECTED peer for a standby commit;
            // the SDP answer alone only sets remote description. Await a
            // bounded connection proof, then commit, then baseline the
            // freshness proof at the commit ack (pre-commit echo/stats
            // never count — the arm below re-baselines at the CURRENT
            // counter, so only post-commit advances prove the path).
            guard await awaitProbeConnected(probe: probe, gen: gen, timeout: probeStandbyCommitTimeout) else {
                probe.cancel()
                return
            }
            let fence = try await api.commitMediaProbe(callId: callId, preflightId: nil, asStandby: true)
            guard gen == generation, !ended else { probe.cancel(); return }
            installDirectStandby(probe: probe, fence: fence, warm: false)
        } catch {
            AppLog.media.notice("direct standby install failed: \(error)")
            DiagnosticsStore.shared.log("route", "direct standby install failed: \(error)")
        }
    }

    /// Shared tail: installs the direct probe into the standby role —
    /// freshness starts UNPROVEN regardless of warm/cold origin (only
    /// post-commit packet advances count).
    private func installDirectStandby(probe: DirectProbeControlling, fence: V2StandbyFence, warm: Bool) {
        standbyDirect = probe
        standbyFreshAt[.direct] = nil
        if let token = fence.token {
            installStandbyFence(kind: .direct, token: token, epoch: fence.epoch ?? 0)
        }
        armDirectStandbyFreshness(probe: probe)
        AppLog.media.debug("hot standby: direct standby installed (\(warm ? "warm seed" : "cold"))")
    }

    /// Relay-standby promotion (direct -> relay): the fenced promote
    /// command (issued by the controller BEFORE this call) swapped the roles
    /// server-side; audio ownership moves through the proven staged
    /// machinery (suspend ADM -> start relay graph), and the direct peer is
    /// DEMOTED to the standby role — never closed — with a recurring
    /// readiness proof.
    private func promoteRelayStandbyToPrimary() async -> Bool {
        guard let session = standbyRelay else { return false }
        let gen = generation
        let peer = route?.currentDirectProbe
        let previous = wsMedia
        // The promote command ALREADY swapped the server roles before this
        // transfer ran; its ack installed the direct standby's fence
        // (token+epoch) — the exact handle a rollback needs to restore the
        // server's direct primary (build-51).
        let directStandbyFence = standbyFences[.direct]
        wsMedia = session
        stagedPreviousRelay = previous
        let promoted = await promoteStagedWSMedia(retiringDirect: peer, retainRetiringPeer: true)
        if promoted {
            // Post-await identity fences: a hangup/interruption/replace
            // during the staged promotion must never mutate audio or state.
            guard gen == generation, !ended, wsMedia === session else { return false }
            standbyRelay = nil
            standbyFences[.relay] = nil
            hotStandby?.removeStandby(.relay)
            route?.noteExternalPrimary(false, adopted: nil)
            // The demoted direct peer stays warm as the new standby handle:
            // promoteStagedWSMedia ALREADY suspended its audio device
            // (ordered handover) and, with retainRetiringPeer, kept the
            // transport. The fence was installed from the promote ack.
            if let peer {
                standbyDirect = peer
                armDirectStandbyFreshness(probe: peer)
                standbyFreshAt[.direct] = nil // unproven until real advances
            }
            return true
        }
        // Local rollback (the staged machinery already restored the direct
        // peer's audio device and closed the staged relay). With a LIVE
        // call identity, reconcile the coordinator state — the promoted
        // session is closed and must never be re-promoted (the build-50
        // field log shows the 300 ms retry loop re-promoting it 7×/5 s) —
        // and swap the SERVER's primary back so both sides agree (its
        // uplink is otherwise discarded one-way). A superseded/stale
        // promotion (gen/ended) touches NOTHING here.
        guard gen == generation, !ended, let callId = activeGatewayId else { return false }
        if standbyRelay === session { standbyRelay = nil }
        standbyFences[.relay] = nil
        hotStandby?.removeStandby(.relay)
        standbyFreshAt[.relay] = nil
        await restoreDirectPrimaryAfterFailedRelayPromotion(
            gen: gen, callId: callId, fence: directStandbyFence)
        return false
    }

    /// Direct-standby promotion (relay -> direct): the fenced promote
    /// command already swapped the roles server-side; audio ownership moves
    /// to the probe (relay graph stops first, then the SDK ADM adopts), and
    /// the relay DEMOTES to the standby socket role — kept attached, ping
    /// sampling and freshness tracking only. A bounded audio gate verifies
    /// the adopted path really produces media before reporting success.
    private func promoteDirectStandbyToPrimary() async -> Bool {
        guard let probe = standbyDirect else { return false }
        let gen = generation
        let callId = activeCallIdForStandby ?? ""
        // Ordered transfer: stop the relay graph BEFORE the SDK ADM adopts
        // (two concurrent owners break the engine).
        wsMedia?.suspendAudioForStandbyHandover()
        if probe.isAdoptedPeer {
            // A DEMOTED peer keeps its transport with the ADM suspended;
            // re-promotion RESUMES it (generation-fenced inside the probe,
            // with the fresh bidirectional proof enforced by the gate
            // below). Re-entering the guarded `adopt` would no-op and
            // leave the call muted.
            probe.resumeAudioDeviceAfterFailedHandover()
        } else {
            probe.adopt(activatedSession: AudioSessionBridge.shared.activeSession)
        }
        let proved = await awaitStandbyDirectFreshMedia(probe: probe, gen: gen)
        // Post-await identity fences before ANY audio/state mutation.
        guard gen == generation, !ended, standbyDirect === probe else {
            probe.cancel()
            return false
        }
        guard proved else {
            return await rollbackDirectStandbyPromotion(probe: probe, callId: callId, gen: gen)
        }
        let relay = wsMedia
        wsMedia = nil
        standbyRelay = relay
        relay?.markHotStandby()
        relay?.startPingSampling()
        if let relay { armRelayStandbyFreshness(session: relay) }
        standbyFences[.direct] = nil
        standbyDirect = nil
        route?.noteExternalPrimary(true, adopted: probe)
        onQuality?(MediaQuality(phase: .connected))
        publishPhase()
        return true
    }

    /// Bounded BIDIRECTIONAL fresh-media gate on a freshly adopted standby
    /// direct path. CONNECTED/RTT never count; both the inbound AND the
    /// outbound packet counters must advance past per-promotion baselines
    /// (captured right after the ADM adopt/resume — valid silent PCM counts,
    /// no speech-energy requirement). A path whose uplink never starts rolls
    /// back bounded by `directStandbyAudioGateTimeout`.
    private func awaitStandbyDirectFreshMedia(probe: any DirectProbeControlling, gen: UInt64) async -> Bool {
        let inboundBaseline = probe.inboundAudioPackets
        let outboundBaseline = probe.outboundAudioPackets
        let deadline = Date().addingTimeInterval(directStandbyAudioGateTimeout)
        while Date() < deadline {
            guard gen == generation, !ended else { return false }
            if probe.inboundAudioPackets > inboundBaseline,
               probe.outboundAudioPackets > outboundBaseline {
                return true
            }
            try? await Task.sleep(nanoseconds: 250_000_000)
        }
        return false
    }

    /// Rollback of a failed direct-standby promotion: restore the relay as
    /// the audible owner and reconcile the server roles (the relay fence
    /// came from the original promote ack). Bounded: one re-promote attempt
    /// plus a state reconcile; the recovery episode remains the final
    /// fallback if rollback itself cannot be confirmed.
    private func rollbackDirectStandbyPromotion(probe: any DirectProbeControlling, callId: String, gen: UInt64) async -> Bool {
        // Owned teardown FIRST: adopt/resume acquired RTCAudioDemand; a bare
        // cancel() would leak that owner while the relay graph restarts
        // (closeTransport releases owned audio). Fenced so a stale rollback
        // cannot touch a newer call's state.
        if gen == generation, !ended {
            probe.closeTransport()
        }
        if let relayFence = standbyFences[.relay] {
            let acked = try? await api.promoteStandby(
                callId: callId, transport: "ws",
                token: relayFence.token, expectEpoch: relayFence.epoch, nonce: UUID().uuidString)
            if acked == nil {
                // Ack lost: reconcile actual roles from the server.
                if let state = try? await api.mediaState(callId: callId), state.primary != "ws" {
                    // The rollback swap did not land; one bounded retry.
                    _ = try? await api.promoteStandby(
                        callId: callId, transport: "ws",
                        token: relayFence.token, expectEpoch: state.epoch, nonce: UUID().uuidString)
                }
            }
        }
        guard gen == generation, !ended else { return false }
        // Restore the audible owner: the relay was only graph-suspended.
        standbyFences[.direct] = nil
        standbyDirect = nil
        if let relay = wsMedia {
            relay.promoteAudioOwnership()
            if let activated = AudioSessionBridge.shared.activeSession {
                relay.startOwnedAudioGraph(with: activated)
            }
        }
        return false
    }

    /// Standby watchdog: (a) rebuilds a standby that is MISSING (initial
    /// attach failed, failed promotion cleared it) or whose freshness went
    /// stale, (b) runs the composite auto-choice scorer over LIVE per-path
    /// metrics, promoting on a sustained material advantage (cooldown-
    /// protected; forced modes never auto-promote). Rebuilds are
    /// SINGLE-FLIGHT and suppressed during a promotion or an open recovery
    /// episode. Exits on generation change or call end — no late tasks.
    private var standbyWatchdogTask: Task<Void, Never>?
    private var standbyRebuildInFlight = false

    private func startStandbyWatchdog(callId: String, gen: UInt64) {
        standbyWatchdogTask?.cancel()
        standbyRebuildTask?.cancel()
        standbyRebuildTask = nil
        standbyRebuildInFlight = false
        standbyWatchdogTask = Task { @MainActor [weak self] in
            guard let self else { return }
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 5_000_000_000)
                guard !Task.isCancelled, gen == self.generation, !self.ended,
                      self.activeGatewayId != nil else { return }
                let primaryIsDirect = self.route?.activeTransportIsDirect == true
                let primaryKind: HotStandbyController.RouteKind = primaryIsDirect ? .direct : .relay
                let opposite: HotStandbyController.RouteKind = primaryIsDirect ? .relay : .direct
                // (a) Missing or stale opposite-transport rebuild, single
                // flight, never during a promotion or recovery episode.
                let missing = self.standbyFences[opposite] == nil
                let stale = self.standbyFences[opposite] != nil
                    && (self.standbyFreshAt[opposite].map { Date().timeIntervalSince($0) > 10 } ?? true)
                if (missing || stale), !self.standbyRebuildInFlight,
                   self.hotStandby?.promotionInFlight != true,
                   self.mediaRecovery?.isActive != true {
                    self.standbyRebuildInFlight = true
                    if opposite == .relay {
                        self.standbyRelay?.clearHotStandby(); self.standbyRelay?.closeWithoutAudio(); self.standbyRelay = nil
                    } else {
                        self.standbyDirect?.cancel(); self.standbyDirect = nil
                    }
                    self.standbyFences[opposite] = nil
                    if opposite == .relay {
                        await self.attachRelayStandby(callId: callId)
                    } else {
                        await self.attachDirectStandby(callId: callId)
                    }
                    self.standbyRebuildInFlight = false
                    continue
                }
                // (b) Composite auto choice over live metrics (auto only).
                // Build-62 single authority: while the QUIC three-path
                // authority is active IT decides across RTC/QUIC/WSS; the
                // hot-standby scorer must not adopt audio independently.
                guard self.quicDirect == nil,
                      self.route?.routeState.mode == .auto,
                      self.hotStandby?.promotionInFlight != true,
                      // Never promote alongside the serialized recovery
                      // episode (same guard the rebuild branch enforces).
                      self.mediaRecovery?.isActive != true,
                      let controller = self.hotStandby,
                      let standbyKind = self.standbyFences.keys.first(where: { $0 != primaryKind }) else { continue }
                let standby = self.liveStandbyQuality(kind: standbyKind)
                let primary = self.livePrimaryQuality(kind: primaryKind)
                let failed = self.latestMedia == .disconnected || self.latestMedia == .failed
                    || self.latestWebRTCQuality?.phase == .failed
                    || self.latestWebRTCQuality?.phase == .disconnected
                if controller.shouldPromote(standby: standby, primary: primary, primaryFailed: failed) {
                    _ = await controller.promoteStandby(standbyKind)
                }
            }
        }
    }

    private var standbyRebuildTask: Task<Void, Never>?

    /// Live per-path quality for the scorer. ONLY real measurements feed
    /// the scorer: RTT/jitter from actual samples, backlog from real
    /// gateway-buffer telemetry, freshness from real frame-advance
    /// timestamps. A path with no real measurement reports UNSTABLE (score
    /// .infinity) — never a fabricated value (coordinator review).
    private func liveStandbyQuality(kind: HotStandbyController.RouteKind) -> HotStandbyController.PathQuality {
        if kind == .relay, let s = standbyRelay {
            let samples = s.freshPingSamples(within: 8)
            guard let rtt = samples.last, let freshAt = standbyFreshAt[.relay] else {
                return .unknown
            }
            return .init(rttMs: rtt * 1000,
                         jitterMs: (MediaRouteAdvisor.jitter(of: samples) ?? 0) * 1000,
                         backlogMs: (s.gatewayBufferSeconds ?? 0) * 1000,
                         freshnessAgeMs: Date().timeIntervalSince(freshAt) * 1000,
                         stable: Date().timeIntervalSince(freshAt) <= 5)
        }
        // Direct standby: echo RTT from the probe's stats.
        let samples = standbyDirect?.freshTimestampedQualitySamples(within: 8, now: Date()) ?? []
        guard let rtt = samples.last?.rtt, let freshAt = standbyFreshAt[.direct] else {
            return .unknown
        }
        return .init(rttMs: rtt * 1000,
                     jitterMs: (MediaRouteAdvisor.jitter(of: samples.map { $0.rtt }) ?? 0) * 1000,
                     backlogMs: 0, // no direct-path backlog telemetry: unknown -> 0 weight, not fabricated
                     freshnessAgeMs: Date().timeIntervalSince(freshAt) * 1000,
                     stable: Date().timeIntervalSince(freshAt) <= 5)
    }

    private func livePrimaryQuality(kind: HotStandbyController.RouteKind) -> HotStandbyController.PathQuality {
        if kind == .relay, let ws = wsMedia {
            let samples = ws.freshPingSamples(within: 8)
            guard let rtt = samples.last else { return .unknown }
            return .init(rttMs: rtt * 1000,
                         jitterMs: (MediaRouteAdvisor.jitter(of: samples) ?? 0) * 1000,
                         backlogMs: (ws.gatewayBufferSeconds ?? 0) * 1000,
                         freshnessAgeMs: 0, // the carrier's freshness is its liveness, below
                         stable: latestMedia == .connected)
        }
        let samples = route?.currentDirectProbe?.freshTimestampedQualitySamples(within: 8, now: Date()) ?? []
        guard let rtt = samples.last?.rtt else { return .unknown }
        return .init(rttMs: rtt * 1000,
                     jitterMs: (MediaRouteAdvisor.jitter(of: samples.map { $0.rtt }) ?? 0) * 1000,
                     backlogMs: 0,
                     freshnessAgeMs: 0,
                     stable: latestWebRTCQuality?.phase != .failed
                         && latestWebRTCQuality?.phase != .disconnected
                         && latestMedia == .connected)
    }

    /// Bounded startup window for honoring a forced selection the initial
    /// carrier does not match (legacy performRequestDirect bounded ~10 s).
    /// Injectable for deterministic tests.
    var initialSelectionHonorWindow: TimeInterval = 12
    /// Bounded bidirectional media proof window for a direct-first startup
    /// (equivalent to the legacy post-adoption audio gate).
    var ownedDirectFirstGateWindow: TimeInterval = 2.0
    /// Settle/re-check window after the ONE bounded ADM restart.
    var ownedDirectFirstRestartSettle: TimeInterval = 1.5

    /// Bounded wait/promote for a wanted standby through the SINGLE owner:
    /// waits for real freshness (rebuilding a MISSING standby, single-flight,
    /// same flag the watchdog uses) then runs the fenced promotion. Never
    /// touches legacy startup/advisor/commit paths.
    @discardableResult
    private func ownedStandbyPromoteAttempt(
        wanted: HotStandbyController.RouteKind,
        controller: HotStandbyController,
        window: TimeInterval
    ) async -> Bool {
        let deadline = Date().addingTimeInterval(window)
        // A FORCED route is current only while the user's persisted mode is
        // exactly that route; a rapid mode change AWAY must cancel the stale
        // request immediately, including after every await below.
        let forcedMode: MediaRouteMode = wanted == .direct ? .direct : .relay
        func modeStillWantsThis() -> Bool { route?.routeState.mode == forcedMode }
        while Date() < deadline {
            guard !ended, activeGatewayId != nil, hotStandby === controller,
                  modeStillWantsThis() else { return false }
            if standbyFences[wanted] == nil, !standbyRebuildInFlight,
               controller.promotionInFlight != true, mediaRecovery?.isActive != true {
                // Missing standby (failed/absent initial attach): rebuild
                // inline so the bounded attempt does not wait for a 5 s
                // watchdog tick.
                standbyRebuildInFlight = true
                if wanted == .relay {
                    standbyRelay?.clearHotStandby()
                    standbyRelay?.closeWithoutAudio()
                    standbyRelay = nil
                } else {
                    standbyDirect?.cancel()
                    standbyDirect = nil
                }
                standbyFences[wanted] = nil
                if let callId = activeGatewayId {
                    if wanted == .relay {
                        await attachRelayStandby(callId: callId)
                    } else {
                        await attachDirectStandby(callId: callId)
                    }
                }
                standbyRebuildInFlight = false
                // The user's latest choice may have changed during rebuild.
                guard !ended, activeGatewayId != nil, hotStandby === controller,
                      modeStillWantsThis() else { return false }
            }
            if standbyFences[wanted] != nil,
               standbyFreshAt[wanted].map({ Date().timeIntervalSince($0) <= 5 }) == true {
                // Final mode fence BEFORE the promotion hits the wire: a stale
                // request must never override the latest user choice.
                guard modeStillWantsThis() else { return false }
                let outcome = await controller.promoteStandby(wanted)
                if case .promoted = outcome { return true }
                // Build-51: a promotion that RAN and ROLLED BACK must never
                // tight-retry inside this window — each attempt suspends the
                // live carrier's ADM (the field log shows 7 suspend/resume
                // audio cutouts in 5 s after one failed direct→relay
                // transfer). The rollback already reconciled client AND
                // server state. A fresh EXPLICIT user selection starts a new
                // attempt; the 45 s current-route failure recovery is
                // untouched. Only a not-yet-ready standby (.unavailable)
                // keeps waiting inside the bounded window.
                if case .rolledBack = outcome { return false }
            }
            try? await Task.sleep(nanoseconds: 300_000_000)
        }
        return false
    }

    /// Promotes the selected standby once when the call starts with a
    /// FORCED mode the initial carrier does not match (persisted `.direct`
    /// on a relay-first call). Bounded by `initialSelectionHonorWindow`;
    /// if the selected path never proves fresh, the audible carrier keeps
    /// the call and the failure is reported truthfully. AUTO never enters
    /// here (the composite scorer owns its decision). No competing timer.
    private func honorInitialForcedSelection(callId: String, gen: UInt64,
                                              controller: HotStandbyController) {
        // Build-62 single authority: while the QUIC three-path authority is
        // active it owns forced-mode routing (preferredTransport mapping);
        // the hot-standby machinery must not promote in parallel.
        guard quicDirect == nil else { return }
        let mode = route?.routeState.mode ?? routeModeDefault
        let carrierDirect = route?.activeTransportIsDirect == true || media != nil
        let wanted: HotStandbyController.RouteKind
        switch mode {
        case .direct where !carrierDirect: wanted = .direct
        case .relay where carrierDirect: wanted = .relay
        default: return
        }
        Task { @MainActor [weak self] in
            guard let self else { return }
            let promoted = await self.ownedStandbyPromoteAttempt(
                wanted: wanted, controller: controller,
                window: self.initialSelectionHonorWindow)
            guard !promoted, gen == self.generation, !self.ended,
                  self.activeGatewayId == callId else { return }
            // The user moved away from the forced route: no failure notice —
            // the stale request was cancelled, not a failed promotion.
            let forcedMode: MediaRouteMode = wanted == .direct ? .direct : .relay
            guard self.route?.routeState.mode == forcedMode else { return }
            let message = wanted == .direct
                ? String(localized: "直连不可用，继续使用中继。")
                : String(localized: "中继不可用，继续使用直连。")
            self.onRouteNotice?(message, true)
        }
    }

    /// Build-50 direct-first bounded gate: proves bidirectional media after
    /// the warm preflight was adopted (same inputs as the legacy gate:
    /// NetEq output pull + packet advance, echo never counts). On failure
    /// ONE bounded ADM restart, then — still dead — enters the single
    /// coordinator 45 s recovery episode (mode semantics decide order; no
    /// competing legacy timer).
    /// True while the direct-first gate still owns the SAME probe as the
    /// route's primary. A user switching to relay (or any swap) demotes the
    /// probe: the gate must become inert — no restart of the demoted ADM and
    /// no recovery for a no-longer-primary.
    private func directFirstGateStillOwns(_ probe: DirectProbeControlling) -> Bool {
        guard route?.activeTransportIsDirect == true else { return false }
        return route?.currentDirectProbe === probe
    }

    private func startOwnedDirectFirstGate(probe: DirectProbeControlling,
                                            callId: String, gen: UInt64) {
        Task { @MainActor [weak self] in
            guard let self else { return }
            if await self.awaitDirectFirstProof(
                probe: probe, callId: callId, gen: gen, window: self.ownedDirectFirstGateWindow) {
                return
            }
            // Re-check ownership BEFORE the restart: a demoted ADM must not
            // be restarted by the stale gate.
            guard gen == self.generation, !self.ended,
                  self.activeGatewayId == callId,
                  self.directFirstGateStillOwns(probe) else { return }
            // ONE bounded ADM restart when the probe supports it; a probe
            // that cannot restart falls straight through to recovery.
            if probe.restartAudioDevice() {
                if await self.awaitDirectFirstProof(
                    probe: probe, callId: callId, gen: gen,
                    window: self.ownedDirectFirstRestartSettle) {
                    return
                }
            }
            // Re-check ownership BEFORE recovery dispatch: recovering the
            // wrong (demoted) primary would fight the user's latest action.
            guard gen == self.generation, !self.ended,
                  self.activeGatewayId == callId,
                  self.directFirstGateStillOwns(probe) else { return }
            DiagnosticsStore.shared.log("route",
                "direct-first gate failed; entering 45s recovery")
            self.scheduleMediaFailure(
                callId: callId, gen: gen,
                message: String(localized: "音频长时间未恢复，已结束通话。"))
        }
    }

    private func awaitDirectFirstProof(probe: DirectProbeControlling, callId: String,
                                        gen: UInt64, window: TimeInterval) async -> Bool {
        let deadline = Date().addingTimeInterval(window)
        while Date() < deadline {
            // A call that ended/changed, OR a probe demoted/swapped out of
            // the primary, is "settled" — never restart or recover it.
            guard gen == generation, !ended, activeGatewayId == callId,
                  directFirstGateStillOwns(probe) else { return true }
            if probe.audioFlowing { return true }
            try? await Task.sleep(nanoseconds: 100_000_000)
        }
        guard directFirstGateStillOwns(probe) else { return true }
        return probe.audioFlowing
    }

    /// Tears down both standby transports and the watchdog (call end, park,
    /// replace). A standby close is ALWAYS safe: server-side it is silent
    /// by design.
    private func teardownHotStandby() {
        if let seed = standbySeed {
            standbySeed = nil
            discardPreflightHandoff(seed)
        }
        standbyTrickleExchange?.cancel()
        standbyTrickleExchange = nil
        standbyWatchdogTask?.cancel()
        standbyWatchdogTask = nil
        standbyRebuildTask?.cancel()
        standbyRebuildTask = nil
        standbyRebuildInFlight = false
        hotStandby?.invalidate()
        standbyRelay?.clearHotStandby()
        standbyRelay?.closeWithoutAudio()
        standbyRelay = nil
        standbyDirect?.cancel()
        standbyDirect = nil
        standbyFences = [:]
        standbyFreshAt = [:]
        hotStandby = nil
        hotStandbyState = nil
    }

    private var activeCallIdForStandby: String? { activeGatewayId }
    /// Bounded wait for fresh media on a freshly adopted standby direct path.
    var directStandbyAudioGateTimeout: TimeInterval = 2.0
    /// Bounded wait for the standby probe's peer connection before the
    /// standby commit (the gateway rejects an unconnected peer).
    var probeStandbyCommitTimeout: TimeInterval = 8.0

    /// Poll the probe's connection state (bounded); the commit requires a
    /// CONNECTED peer. Exits early on generation change or call end.
    private func awaitProbeConnected(probe: any DirectProbeControlling, gen: UInt64, timeout: TimeInterval) async -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if probe.connected { return true }
            guard gen == generation, !ended else { return false }
            try? await Task.sleep(nanoseconds: 100_000_000)
        }
        return probe.connected
    }

    // MARK: QUIC direct path (build 61, protocol docs/quic61)

    /// Routes build-61 QUIC control frames that arrived ON the WSS socket
    /// (protocol §3): the `ready transport=quic` standby greeting goes to
    /// the QUIC controller; the nonce-fenced `promoted` reply settles the
    /// in-flight promotion await. Never treated as the WSS socket's own
    /// ready/failure state.
    private func handleWSTransportControl(_ control: WSMediaControl,
                                          session: WebSocketCallMedia) {
        if control.type == "ready", control.transport == "quic" {
            quicDirect?.handleTransportReady(control)
        } else if control.type == "promoted" {
            guard let waiter = quicPromoteWaiter, waiter.nonce == control.nonce else { return }
            quicPromoteWaiter = nil
            waiter.continuation.resume(returning: control)
        }
    }

    /// Build-61 QUIC direct standby attempt (protocol §8): fetches the pin
    /// document, resolves the phone's srflx candidate (best-effort STUN),
    /// posts the candidate for the coordinated punch window, then drives the
    /// bounded (~8 s) QUIC dial+bind through the controller. Fired detached
    /// AFTER the WSS path selection so it never blocks the answer; every
    /// resume is generation-fenced like the hot-standby machinery.
    private func attemptQUICDirect(callId: String, gen: UInt64, ice: ICEConfiguration) {
        // QUIC control travels on the authenticated WSS plane (protocol
        // §0/§3): no WSS media socket, no QUIC attempt.
        guard ice.mediaQuic, ice.mediaTransports?.contains("ws") == true,
              quicDirect == nil, quicAttemptTask == nil else { return }
        let controller = QUICDirectController(
            wsMedia: { [weak self] in self?.wsMedia },
            // Build-62 forced-mode unification: the user's ACTUAL routing
            // mode drives the policy. 中继 ⇒ WSS only; 直连 ⇒ the direct
            // family (sub-preference pins QUIC/WebRTC, 自动 sub-preference
            // permits {QUIC, RTC} — relay is NEVER selected); 自动 ⇒ full
            // scoring. The old closure read only the direct sub-preference,
            // so 中继+直连偏好 still allowed Q/RTC selection.
            preferredTransport: { [weak self] in
                guard let self else { return .auto }
                // Build-65: the live requested mode — the QUIC authority's
                // override first (set through `selectRouteMode` even when
                // the legacy route object does not exist on a WSS+QUIC
                // call), then the legacy route state, then the CURRENTLY
                // PERSISTED preference (never the init-time snapshot).
                switch self.liveRequestedMode {
                case .relay:
                    return .relay
                case .direct:
                    switch DirectTransportPreferenceStore.shared
                        .choice(for: self.routeGatewayID) {
                    case .quic: return .quic
                    case .webRTC: return .webRTC
                    case .auto: return .directFamily
                    }
                case .auto:
                    return .auto
                }
            },
            promote: { [weak self] proposalID in
                await self?.promoteQUICDirect(proposalID: proposalID) ?? false
            },
            // Build-62 production wiring: the three-path authority decides
            // with REAL evidence and commits through the REAL coordinator
            // seams (the pre-62 controller saw only QUIC-vs-relay and
            // could never adopt WSS/RTC).
            adoptRelay: { [weak self] in
                await self?.adoptRelayForPolicy() ?? false
            },
            adoptDirect: { [weak self] in
                await self?.adoptDirectForPolicy() ?? false
            },
            rtcSample: { [weak self] in
                self?.makeRTCSample()
            },
            relayStandby: { [weak self] in
                self?.makeRelayStandbySample()
            },
            publish: { [weak self] decision, samples, at in
                self?.onRouteSelection?(decision, samples, at)
            },
            mediaFactory: quicMediaFactory)
        controller.evaluationTickInterval = quicEvaluationTickInterval
        if let policyConfig = quicPolicyConfigForTest {
            controller.policyConfig = policyConfig
        }
        controller.onPrimaryDeath = { [weak self] in
            guard let self, let callId = self.activeGatewayId else { return }
            // The confirmed QUIC primary died: the existing bounded 45 s
            // recovery settles the call (fresh relay re-attach first).
            self.scheduleMediaFailure(
                callId: callId, gen: self.generation,
                message: String(localized: "音频长时间未恢复，已结束通话。"))
        }
        quicDirect = controller
        if let budget = quicAttemptTimeoutForTest {
            controller.attemptTimeout = budget
        }
        // Build-63 bounded quarantine retry: a failed QUIC candidate
        // (standby or primary) re-runs this coordinated chain with the
        // SAME network epoch instead of freezing the authority.
        controller.retryAttempt = { [weak self] in
            await self?.retryQUICDirect(callId: callId, generation: gen) ?? false
        }
        // ONE decision authority across RTC/QUIC/WSS: while the QUIC
        // controller is active, the legacy route controller's automatic
        // promotion stands down (manual forced choices still work).
        route?.automaticStandDown = true
        // Seed ONLY a REAL adoption: the warm RTC fastpath hands over an
        // already-adopted direct session. The relay bootstrap is NOT an
        // adoption — it is the provisional carrier (protocol §8): seeding
        // it would anchor min-hold on the bootstrap and block the first
        // proven better route.
        if media != nil {
            controller.seedInitialOwner(.webRTCDirect)
        }
        quicAttemptTask = Task { @MainActor [weak self] in
            guard let self else { return }
            defer { self.quicAttemptTask = nil }
            // Build-62 single-authority safety: the legacy selector stands
            // down only while the QUIC authority ACTUALLY runs. ANY early
            // abort (capability off, untrusted pin, STUN unreachable, punch
            // rejected — the common public-NAT failure) returns the legacy
            // authority immediately: auto-routing across RTC/WSS must never
            // freeze because the QUIC attempt never reached `start`.
            let abortAttemptRestoringLegacyAuthority: () -> Void = {
                self.route?.automaticStandDown = false
                self.quicDirect?.close()
                self.quicDirect = nil
            }
            do {
                // (a) The capability + TLS pin document (authenticated GET).
                let info = try await api.quicMediaInfo()
                guard gen == self.generation, !self.ended,
                      self.activeGatewayId == callId, !Task.isCancelled else { return }
                guard info.enabled, !info.pinHash.isEmpty,
                      let pinDocument = info.pin,
                      let candidate = info.candidates
                          .first(where: { $0.type == "srflx" }) ?? info.candidates.first,
                      let gatewayCandidate = candidate.hostPort else {
                    abortAttemptRestoringLegacyAuthority()
                    return
                }
                // A stale advertised candidate is never dialed.
                if let expiresAtMs = candidate.expiresAtMs,
                   expiresAtMs <= Date().unixMilliseconds {
                    abortAttemptRestoringLegacyAuthority()
                    return
                }
                // (b) TRUST (protocol §6, build-62): verify the pin
                // document's Ed25519 signature against the pairing-trusted
                // gateway identity BEFORE any dial. The trusted anchor is the
                // AppModel-injected binding; the live key is accepted only
                // when its DERIVED fingerprint equals the binding (the served
                // fingerprint string alone is never trusted). ANY failure
                // keeps QUIC closed — the incumbent RTC/WSS path is
                // untouched and the reason lands in diagnostics.
                guard let trusted = self.quicTrustedGatewayIdentity else {
                    DiagnosticsStore.shared.log(
                        "route", "quic attempt aborted: no trusted gateway identity")
                    abortAttemptRestoringLegacyAuthority()
                    return
                }
                let liveIdentity = try? await api.identity()
                guard gen == self.generation, !self.ended,
                      self.activeGatewayId == callId, !Task.isCancelled else { return }
                let pin: QUICPin
                switch QUICPinTrust.verifiedPin(
                    pinDocument,
                    liveIdentity: liveIdentity,
                    trustedGatewayId: trusted.gatewayId,
                    trustedFingerprint: trusted.fingerprint,
                    now: Date()
                ) {
                case .success(let verified):
                    pin = verified
                case .failure(let trustError):
                    DiagnosticsStore.shared.log(
                        "route", "quic attempt aborted: pin trust failed: \(trustError)")
                    abortAttemptRestoringLegacyAuthority()
                    return
                }
                // (c)+(d) STUN + punch + bounded dial, shared with the
                // build-63 bounded retry path.
                await self.performQUICDial(
                    callId: callId, gen: gen, controller: controller,
                    pin: pin, info: info, networkEpoch: "quic-\(UUID().uuidString)",
                    onPreDialAbort: abortAttemptRestoringLegacyAuthority)
            } catch {
                guard gen == self.generation, !self.ended else { return }
                DiagnosticsStore.shared.log(
                    "route", "quic attempt aborted: \(error.localizedDescription)")
                abortAttemptRestoringLegacyAuthority()
            }
        }
    }

    /// Coordinated dial: STUN → punch post → bounded socket start.
    /// Shared by the initial attempt and every build-63 bounded retry; the
    /// retry path keeps the SAME network epoch (the quarantined path's
    /// context is unchanged — only the punch token rotates). Every await
    /// re-fences generation/call identity; `onPreDialAbort` runs ONLY on a
    /// pre-dial capability/trust/STUN/punch failure (a dial failure never
    /// aborts the authority — it is the quarantine/retry case).
    private func performQUICDial(
        callId: String, gen: UInt64, controller: QUICDirectController,
        pin: QUICPin, info: QUICMediaInfo, networkEpoch: String,
        onPreDialAbort: @escaping () -> Void
    ) async {
        // (c) Best-effort STUN: the phone's OWN srflx candidate for
        // the coordinated punch, plus the observed LOCAL endpoint so
        // the QUIC dial reuses the exact NAT binding. Without it the
        // punch window cannot be coordinated, so the attempt
        // truthfully stands down.
        let resolveSTUN = quicSTUNResolver ?? { host, port in
            try? await STUNBindingClient.querySTUNCandidate(host: host, port: port)
        }
        guard let stun = info.stun?.lazy
            .compactMap({ HostPort(wireString: $0) }).first,
            let binding = await resolveSTUN(stun.host, stun.port)
        else {
            DiagnosticsStore.shared.log(
                "route", "quic attempt skipped: no srflx candidate")
            onPreDialAbort()
            return
        }
        // The srflx candidate (what the gateway punches) is the MAPPED
        // address; the local source endpoint (what QUIC binds via
        // requiredLocalEndpoint) is the OBSERVED local address:port.
        // These differ under NAT — binding QUIC to the mapped public
        // address was the build-61 defect.
        let localCandidate = binding.mapped
        let localEndpoint: NWEndpoint?
        if let local = binding.local {
            localEndpoint = NWEndpoint.hostPort(
                host: NWEndpoint.Host(local.host),
                port: NWEndpoint.Port(rawValue: local.port)!)
        } else {
            // Source-port reuse unsupported: dial without a pinned
            // local endpoint; the socket reports an honest
            // localEndpointMismatch and the attempt stays bounded.
            localEndpoint = nil
        }
        guard gen == self.generation, !self.ended,
              self.activeGatewayId == callId, !Task.isCancelled else { return }
        // Post the candidate → one-time punch token + window.
        guard let posted = try? await api.postQUICCandidate(
            callId: callId, candidate: localCandidate.wireString,
            networkEpoch: networkEpoch) else {
            onPreDialAbort()
            return
        }
        guard gen == self.generation, !self.ended,
              self.activeGatewayId == callId, !Task.isCancelled else { return }
        guard let punchCandidate = posted.gatewayHostPort else {
            onPreDialAbort()
            return
        }
        // (d) Bounded dial + bind bursts (truthful nil on failure).
        controller.attempt = QUICDirectController.QUICAttemptResult(
            candidate: punchCandidate,
            bindToken: posted.punchToken,
            localEndpoint: localEndpoint)
        _ = await controller.start(
            callId: callId,
            // The authenticated session already identifies this
            // device to the gateway; reserved for diagnostics.
            deviceId: "",
            api: api,
            pin: pin,
            gatewayCandidate: punchCandidate,
            bindToken: posted.punchToken,
            networkEpoch: networkEpoch)
    }

    /// Build-63 bounded retry: re-runs the capability/trust/STUN/punch/
    /// dial chain with the controller's CURRENT network epoch after a
    /// QUIC failure. Every step re-fences the ORIGINAL captured
    /// generation/call identity — a superseded lifecycle (hangup, new
    /// call) abandons the retry without touching anything.
    private func retryQUICDirect(callId: String, generation gen: UInt64) async -> Bool {
        guard let controller = quicDirect, !ended,
              gen == self.generation, self.activeGatewayId == callId,
              !Task.isCancelled else { return false }
        do {
            let info = try await api.quicMediaInfo()
            guard gen == self.generation, !ended, self.activeGatewayId == callId else { return false }
            guard info.enabled, !info.pinHash.isEmpty,
                  let pinDocument = info.pin,
                  let candidate = info.candidates
                      .first(where: { $0.type == "srflx" }) ?? info.candidates.first,
                  let gatewayCandidate = candidate.hostPort else { return false }
            if let expiresAtMs = candidate.expiresAtMs,
               expiresAtMs <= Date().unixMilliseconds { return false }
            guard let trusted = self.quicTrustedGatewayIdentity else { return false }
            let liveIdentity = try? await api.identity()
            guard gen == self.generation, !ended, self.activeGatewayId == callId else { return false }
            let pin: QUICPin
            switch QUICPinTrust.verifiedPin(
                pinDocument, liveIdentity: liveIdentity,
                trustedGatewayId: trusted.gatewayId,
                trustedFingerprint: trusted.fingerprint, now: Date()) {
            case .success(let verified): pin = verified
            case .failure: return false
            }
            // The candidate gate is re-validated but intentionally NOT
            // re-dialed here: performQUICDial re-resolves STUN + punches
            // fresh (the advertised gateway candidate is re-checked inside
            // the controller's start via the attempt bundle).
            _ = gatewayCandidate
            await performQUICDial(
                callId: callId, gen: gen, controller: controller,
                pin: pin, info: info,
                // Build-66: a path generation change mints a FRESH network
                // epoch for this dial so samples measured on the old path
                // can never mark the new one decision-ready.
                networkEpoch: controller.nextDialNetworkEpoch(),
                onPreDialAbort: {})
            return controller.quicCallMedia != nil
        } catch {
            return false
        }
    }

    /// Bounded await of the nonce-fenced `promoted` reply on the WSS
    /// socket. The reply delivery and this timeout both hop to the main
    /// actor, so the one-shot extraction is race-free; nil on timeout.
    private func awaitQUICPromotedReply(
        nonce: String, timeout: TimeInterval
    ) async -> WSMediaControl? {
        await withCheckedContinuation { continuation in
            quicPromoteWaiter = (nonce, continuation)
            Task { @MainActor [weak self] in
                try? await Task.sleep(nanoseconds: UInt64(timeout * 1_000_000_000))
                guard let self,
                      let waiter = self.quicPromoteWaiter,
                      waiter.nonce == nonce else { return }
                self.quicPromoteWaiter = nil
                waiter.continuation.resume(returning: nil)
            }
        }
    }

    /// Bounded window for the `promoted` reply after the WSS promote
    /// command. Injectable for deterministic tests.
    var quicPromoteReplyTimeout: TimeInterval = 5

    /// QUIC standby → primary promotion (protocol §3): the fenced
    /// standby-promote command goes out on the WSS PRIMARY socket with
    /// `transport:"quic"`; the nonce-fenced `promoted` reply acknowledges
    /// the server-side swap; ONLY then does audio ownership move, mirroring
    /// `promoteStagedWSMedia`'s proven ordering (the retiring ws graph
    /// suspends before the QUIC graph starts, bounded input-readiness wait
    /// plus one graph-start retry). ANY failure rolls back truthfully: the
    /// QUIC candidate closes without audio and the ws primary keeps the
    /// call — no audio-gap flag, no false failure publish.
    @discardableResult
    private func promoteQUICDirect(proposalID: UInt64) async -> Bool {
        guard let quicDirect,
              let quic = quicDirect.quicCallMedia,
              let token = quicDirect.standbyToken,
              let session = wsMedia, session.state == .connected,
              let callId = activeGatewayId else { return false }
        let gen = generation
        let bridgeEpoch = AudioSessionBridge.shared.eventEpoch
        guard gen == self.generation, !ended else { return false }

        // (a) Staged promotion window: this function owns the failure
        // decision (a graph-start failure is reported, never socket-fatal).
        quic.markAudioStaged(true)
        quic.graphStartFailureNonfatal = true
        defer { quic.graphStartFailureNonfatal = false }
        // Replay the live activation per the contract; the staged gate
        // suppresses the start (the owned graph start below is the real
        // one) and keeps the self-managed ownership bookkeeping current.
        if let activated = AudioSessionBridge.shared.activeSession {
            quic.audioActivated(with: activated)
        }

        // (b) The fenced promote command on the WSS primary socket.
        let expectEpoch = quic.lastReadyEpoch ?? session.lastReadyEpoch ?? 0
        let nonce = UUID().uuidString
        session.sendPromoteCommand(
            token: token, expectEpoch: expectEpoch, nonce: nonce, transport: "quic")

        // (c) Bounded, nonce-fenced await of the `promoted` reply.
        let reply = await awaitQUICPromotedReply(nonce: nonce, timeout: quicPromoteReplyTimeout)
        guard gen == self.generation, !ended,
              wsMedia === session, quicDirect.quicCallMedia === quic,
              !AudioSessionBridge.shared.isInterrupted else {
            // A superseded promotion touches nothing (the newer lifecycle
            // owns audio from here).
            return false
        }
        guard let reply, reply.nonce == nonce else {
            // Rollback: the QUIC candidate never took audio; the ws
            // primary keeps the call untouched.
            quic.closeWithoutAudio()
            DiagnosticsStore.shared.log(
                "route", "quic promote timed out; rolled back proposal=\(proposalID)")
            return false
        }

        // (d) Exclusive ownership transfer (make-before-break, single
        // owner at every instant): the retiring ws graph suspends BEFORE
        // the QUIC graph starts.
        quic.promoteAudioOwnership()
        session.suspendAudioForStandbyHandover()
        await waitForQUICPromotionInputReadiness(
            media: quic, gen: gen, bridgeEpoch: bridgeEpoch)
        var started = false
        for attempt in 0...max(0, promotionGraphStartRetries) {
            if attempt > 0 {
                DiagnosticsCensus.shared.increment("audio.quicPromotedGraphRetry")
                try? await Task.sleep(nanoseconds: promotionGraphRetrySettleNanoseconds)
            }
            // Post-await fence: the call, the audio ownership epoch and the
            // live ws primary must all still be this promotion's.
            guard gen == self.generation, !ended,
                  activeGatewayId == callId || conference != nil,
                  AudioSessionBridge.shared.eventEpoch == bridgeEpoch,
                  !AudioSessionBridge.shared.isInterrupted,
                  wsMedia === session else { break }
            guard let activated = AudioSessionBridge.shared.activeSession else {
                DiagnosticsCensus.shared.increment("audio.quicPromotionNoActiveSession")
                break
            }
            started = quic.startOwnedAudioGraph(with: activated)
            if started { break }
        }
        // A delivered activation may have started the graph while the retry
        // loop fenced: success only against THIS call's identity.
        if !started, quic.isGraphRunning,
           gen == self.generation, wsMedia === session {
            started = true
        }
        guard started else {
            // Build-62 server-reconciled rollback: the fenced ack ALREADY
            // swapped the server roles (QUIC primary, WSS demoted — its
            // uplink is discarded server-side). Resuming the local WSS
            // graph alone would be silently one-way. Restore the WSS
            // primary SERVER-side with its fresh standby identity and only
            // a CONFIRMED restore may resume audio; a superseded lifecycle
            // (hangup/new call/interruption during the readiness wait)
            // touches NOTHING here. An unconfirmed restore enters the
            // bounded 45 s recovery — never a healthy publish.
            quic.closeWithoutAudio()
            DiagnosticsCensus.shared.increment("call.quicPromotionRolledBack")
            DiagnosticsStore.shared.log(
                "call", "quic promotion rolled back (graph start failed)")
            guard gen == self.generation, !ended, wsMedia === session,
                  AudioSessionBridge.shared.eventEpoch == bridgeEpoch else {
                return false
            }
            if await restoreWSPrimaryAfterFailedQUICPromotion(
                gen: gen, callId: callId, wsSession: session,
                familyHoldsQUIC: true) {
                session.promoteAudioOwnership()
                if let activated = AudioSessionBridge.shared.activeSession {
                    _ = session.startOwnedAudioGraph(with: activated)
                }
                publishPhase()
            } else {
                publishPhase()
                scheduleMediaFailure(
                    callId: callId, gen: gen,
                    message: String(localized: "音频长时间未恢复，已结束通话。"))
            }
            return false
        }

        // Promoted: audio ownership moved atomically. Build-62 control-plane
        // separation (protocol §7, coordinator review): the ws socket is
        // DEMOTED to the server-side standby role, NOT retired — the QUIC
        // design relies on the WSS plane for reliable critical control and
        // the demoted standby is the ONLY relay telemetry feed; retiring it
        // made any later Q→WSS comparison/switch impossible. Single audio
        // owner still holds: the ws graph stays stopped.
        if let epoch = reply.epoch {
            quicDirect.applyPromotedEpoch(epoch)
        }
        session.markHotStandby()
        session.startPingSampling()
        // Reconcile the demoted standby's FRESH identity (the swap minted a
        // new token server-side): the fence a later Q→WSS swap needs. Best
        // effort: a failed reconcile leaves the fence nil and the later
        // swap stages a fresh standby instead.
        if let state = try? await api.mediaState(callId: callId),
           gen == generation, !ended,
           state.standbyWs == true, let token = state.standbyWsToken {
            wsStandbyFence = (token: token, epoch: state.epoch)
        }
        latestMedia = .connected
        quic.republishCurrentQuality()
        onQuality?(MediaQuality(phase: .connected))
        publishPhase()
        return true
    }

    /// Polls the QUIC media's live input readiness until usable or the
    /// bounded timeout expires (the retiring ws graph's stop releases the
    /// input asynchronously; a start now would fail with code=1 — the
    /// build-51 lesson, same contract as `waitForPromotionInputReadiness`).
    private func waitForQUICPromotionInputReadiness(
        media: QUICCallMedia, gen: UInt64, bridgeEpoch: UInt64
    ) async {
        let deadline = Date().addingTimeInterval(promotionInputReadinessTimeout)
        while Date() < deadline {
            guard gen == self.generation, !ended,
                  AudioSessionBridge.shared.eventEpoch == bridgeEpoch,
                  !AudioSessionBridge.shared.isInterrupted,
                  wsMedia != nil else { return }
            if media.isAudioInputReady() { return }
            try? await Task.sleep(nanoseconds: promotionInputReadinessPollNanoseconds)
        }
    }

    /// Cancels the in-flight QUIC attempt task and closes the QUIC media.
    /// Called on every teardown path that closes the WSS media (a standby
    /// close is always safe server-side).
    // MARK: Build-61 policy seams (three-path single authority)

    /// REAL WebRTC-direct evidence (never fabricated): present only when a
    /// direct media session exists with live stats. Absent = "unknown".
    /// Build-62: every quality term is the transport's REAL measurement —
    /// loss only when the RTCP reports exposed it (nil = unknown, flagged
    /// via `hasLossMeasurement`), jitter only when the report carried it,
    /// the freshness anchor is the stats-poll timestamp (never re-stamped),
    /// and the measurement ID is the poll counter (a changed RTT VALUE is
    /// not a new measurement).
    private func makeRTCSample() -> RouteCandidateSample? {
        // ACTIVE RTC owner: full real stats from the live session.
        if let direct = media as? WebRTCCallMedia,
           let rtt = direct.policyRttSeconds, rtt > 0,
           let measuredAt = direct.policyStatsMeasuredAt {
            return RouteCandidateSample(
                rttMs: rtt * 1000,
                jitterMs: (direct.policyJitterSeconds ?? 0) * 1000,
                lossFraction: direct.policyPacketLoss ?? 0,
                reorderFraction: 0,
                queueCostMs: 0,
                available: true,
                confidence: direct.policyFreshMediaProven ? 0.8 : 0.5,
                mediaReady: direct.policyFreshMediaProven,
                networkEpoch: "",
                measuredAt: measuredAt,
                sampleID: direct.policyStatsMeasurementID,
                hasLossMeasurement: direct.policyPacketLoss != nil,
                hasJitterMeasurement: direct.policyJitterSeconds != nil,
                // No backlog visibility on the RTC path.
                hasQueueEstimate: false)
        }
        // STANDBY RTC candidate (WSS/QUIC primary): REAL probe telemetry
        // from the warm standby handle — the policy can select RTC from any
        // primary, not only while RTC owns audio. mediaReady is the standby
        // freshness proof (real frame advances), never CONNECTED alone.
        guard let probe = standbyDirect,
              let freshAt = standbyFreshAt[.direct] else { return nil }
        let samples = probe.freshTimestampedQualitySamples(within: 8, now: Date())
        guard let newest = samples.last else { return nil }
        // Stable per-connection measurement id: a NEW id only when the
        // freshest sample changes (a cached reading re-evaluated never
        // grows a sustained-gain streak).
        if newest.at != lastRTCStandbySampleAt {
            lastRTCStandbySampleAt = newest.at
            rtcStandbySampleID &+= 1
        }
        let fresh = Date().timeIntervalSince(freshAt) <= 5
        return RouteCandidateSample(
            rttMs: newest.rtt * 1000,
            jitterMs: (MediaRouteAdvisor.jitter(of: samples.map { $0.rtt }) ?? 0) * 1000,
            lossFraction: 0,
            reorderFraction: 0,
            queueCostMs: 0,
            available: true,
            confidence: fresh ? 0.7 : 0.4,
            mediaReady: fresh,
            networkEpoch: "",
            measuredAt: newest.at,
            sampleID: rtcStandbySampleID,
            hasLossMeasurement: false,
            hasJitterMeasurement: true,
            hasQueueEstimate: false)
    }
    private var lastRTCStandbySampleAt: Date?
    private var rtcStandbySampleID: UInt64 = 0

    /// Build-62: REAL relay evidence from the hot-standby relay socket
    /// (ping RTT + genuine mirrored-downlink freshness) while RTC/QUIC is
    /// the primary and `wsMedia` is not the carrier.
    private func makeRelayStandbySample() -> RouteCandidateSample? {
        guard let relay = standbyRelay,
              let freshAt = standbyFreshAt[.relay] else { return nil }
        guard let ping = relay.lastPingSample else { return nil }
        let freshPings = relay.freshPingSamples(within: 8)
        if ping.at != lastRelayStandbyPingAt {
            lastRelayStandbyPingAt = ping.at
            relayStandbySampleID &+= 1
        }
        let fresh = Date().timeIntervalSince(freshAt) <= 5
        return RouteCandidateSample(
            rttMs: ping.rtt * 1000,
            jitterMs: (MediaRouteAdvisor.jitter(of: freshPings) ?? 0) * 1000,
            lossFraction: 0,
            reorderFraction: 0,
            queueCostMs: (relay.gatewayBufferSeconds ?? 0) * 1000,
            available: relay.state == .connected,
            confidence: fresh ? 0.7 : 0.4,
            mediaReady: fresh,
            networkEpoch: "",
            measuredAt: ping.at,
            sampleID: relayStandbySampleID + 1_000_000,
            hasLossMeasurement: false,
            hasJitterMeasurement: true,
            hasQueueEstimate: relay.gatewayBufferSeconds != nil)
    }
    private var lastRelayStandbyPingAt: Date?
    private var relayStandbySampleID: UInt64 = 0

    /// The policy-driven staged relay (Q→WSS / RTC→WSS), when the swap is
    /// in flight. Promoted into `wsMedia` only after the server acks.
    private var policyStagedRelay: WebSocketCallMedia?
    /// Build-62: the FRESH standby identity (token + epoch) of the real WSS
    /// socket after it was DEMOTED server-side by a QUIC promotion. The
    /// demoted socket's own greeting predates the swap, so the fresh token
    /// only exists here (reconciled from the promoted ack / mediaState).
    private var wsStandbyFence: (token: String, epoch: UInt64)?

    /// Policy-driven relay adoption (Q→WSS / RTC→WSS), build-62 production
    /// seam. Server-correct make-before-break: (1) stage a fresh WSS in the
    /// SERVER standby role — the incumbent QUIC/RTC audio is NEVER muted
    /// before the staged target exists; (2) the fenced REST promote swaps
    /// server roles and returns the demoted QUIC standby's FRESH token;
    /// (3) only then does the retiring QUIC owner step aside locally; (4)
    /// the QUIC path returns to hot standby and the QUIC controller REMAINS
    /// the single decision authority (the legacy selector and the hot-
    /// standby watchdog stay stood down — no second authority resumes).
    private func adoptRelayForPolicy() async -> Bool {
        guard let callId = activeGatewayId else { return false }
        let gen = generation
        // Build-64: capture the QUIC media object ONCE — every post-await
        // step must reconcile against THIS call's captured objects, never
        // re-read the shared `quicDirect?.quicCallMedia` (a newer call may
        // own the controller by then).
        let quicMedia = quicDirect?.quicCallMedia
        // (1) The relay target: reuse the DEMOTED ws standby left behind by
        // a QUIC promotion (its fresh fence lives in `wsStandbyFence`); when
        // none exists (RTC-primary case), stage a fresh standby WITHOUT
        // touching the incumbent audio (build-62 fix: the old flow muted the
        // QUIC owner before staging, so an early stage failure left the
        // call muted on every exit).
        var session = wsMedia
        var fence = wsStandbyFence
        var stagedFresh = false
        if fence == nil || session == nil {
            // Build-64: the helper RETURNS the session it created — the
            // outer flow only ever holds ITS captured instance and never
            // re-reads the shared `policyStagedRelay` (a newer lifecycle
            // may already own it by the time this flow resumes).
            guard let staged = await stagePolicyRelayStandby(callId: callId, gen: gen) else { return false }
            session = staged
            stagedFresh = true
            if let token = staged.lastReadyStandbyToken,
               let epoch = staged.lastReadyEpoch {
                fence = (token: token, epoch: epoch)
            }
        }
        guard gen == generation, !ended,
              let session,
              let fence else {
            // Build-64 stale-guard repair: tear down ONLY the session THIS
            // flow staged, and only while it is still the staged relay —
            // a newer call's staging (written while this flow awaited the
            // fenced promote) is never closed by the stale guard.
            if stagedFresh, let staged = session, policyStagedRelay === staged {
                policyStagedRelay = nil
                staged.closeWithoutAudio()
            }
            return false
        }
        let token = fence.token
        let epoch = fence.epoch
        // (2) Server-side fenced swap.
        let nonce = UUID().uuidString
        do {
            let response = try await api.promoteStandby(
                callId: callId, transport: "ws", token: token,
                expectEpoch: epoch, nonce: nonce)
            // Build-63: a mode change/hangup/new call during the await owns
            // state now — this stale commit touches nothing further.
            guard gen == generation, !ended else { return false }
            if let current = response.currentEpoch, current != response.epoch {
                // Stale/replayed ack: reconcile against the server's actual
                // roles before trusting any client-side change.
                guard let state = try? await api.mediaState(callId: callId),
                      gen == generation, !ended, state.primary == "ws" else { return false }
                applyDemotedQUICIdentity(from: state)
            } else {
                applyDemotedQUICIdentity(from: response)
            }
        } catch {
            return false
        }
        // (3) Local ownership transfer: single audio owner at every instant.
        // The retiring QUIC owner steps aside only NOW, after the swap ack.
        let previous = wsMedia
        wsMedia = session
        policyStagedRelay = nil
        if previous === session {
            // A demoted-standby promotion reuses the SAME session: there is
            // no superseded relay to retire.
            stagedPreviousRelay = nil
        } else if quicDirect != nil {
            // Build-65: the previous WSS primary is the QUIC authority's
            // CONTROL PLANE (build-62 demote design — it carries the QUIC
            // promote commands and relay telemetry). Retiring it here (the
            // old legacy relay-replacement semantics) would strand QUIC
            // control and tear the authority down mid-call. Demote it to
            // the hot-standby role exactly like the fence-reuse path.
            previous?.markHotStandby()
            stagedPreviousRelay = nil
        } else {
            stagedPreviousRelay = previous
        }
        // Audio exclusivity: the QUIC graph SUSPENDS before the staged ws
        // graph may start (single capture owner at every instant).
        quicMedia?.markAudioStaged(true)
        quicMedia?.suspendAudioForStandbyHandover()
        let promoted = await promoteStagedWSMedia(retiringDirect: nil)
        // Build-64 final post-await fence: a hangup/new call during the
        // promote owns state now — this stale path must NOT publish against
        // the newer lifecycle or schedule a media failure for it (the 45 s
        // recovery is only ever armed for the LIVE call it diagnoses).
        guard gen == generation, !ended, activeGatewayId == callId else {
            return false
        }
        guard promoted else {
            // Build-62 server truth: the fenced swap ALREADY acked
            // server-side (WSS primary, QUIC demoted — its uplink is
            // discarded). Neither direction is a proven carrier now:
            // resuming the QUIC graph locally would be one-way. The bounded
            // 45 s recovery settles the call (fresh relay re-attach first) —
            // never a silent healthy publish.
            publishPhase()
            scheduleMediaFailure(
                callId: callId, gen: gen,
                message: String(localized: "音频长时间未恢复，已结束通话。"))
            return false
        }
        // The swap consumed the ws standby identity.
        wsStandbyFence = nil
        // (4) QUIC returns to hot standby; the authority is retained.
        quicMedia?.markHotStandby()
        return true
    }

    /// Build-63 typed reconcile: learns the FRESH identity of a QUIC
    /// transport the server just demoted into a standby slot, so a later
    /// Q re-promotion stays possible. Prefers the dedicated `standbyQuic`
    /// slot (new gateways — unambiguous); falls back to the ws-family
    /// interpretation ONLY for pre-63 gateways, where the QUIC adapter
    /// occupied the ws standby slot and that snapshot IS the QUIC socket.
    /// A real-WSS demote (or an ICE demote) never reaches the controller.
    private func applyDemotedQUICIdentity(from state: V2MediaStateResponse) {
        if state.standbyQuic == true, let freshToken = state.standbyQuicToken {
            quicDirect?.applyStandbyToken(freshToken, epoch: state.epoch)
        } else if state.standbyQuic == nil,
                  state.standbyWs == true, let freshToken = state.standbyWsToken {
            // Pre-63 gateway: the ws-family standby slot holds the QUIC
            // adapter (documented legacy ambiguity, resolved by the
            // dedicated slots on new gateways).
            quicDirect?.applyStandbyToken(freshToken, epoch: state.epoch)
        }
    }

    /// Promote-ACK variant of the demoted-QUIC reconcile: the ack's
    /// `standby` names the DEMOTED transport's family exactly on new
    /// gateways ("quic"); a pre-63 gateway reports the QUIC adapter as
    /// "ws" (its ws-family slot). An "ice" demote (RTC incumbent) is never
    /// applied to the QUIC controller.
    private func applyDemotedQUICIdentity(from response: V2PromoteStandbyResponse) {
        guard let freshToken = response.standbyToken else { return }
        switch response.standby {
        case "quic":
            quicDirect?.applyStandbyToken(freshToken, epoch: response.epoch)
        case "ws":
            // Pre-63 gateway legacy interpretation (see the mediaState
            // variant). On new gateways this branch is unreachable in the
            // QUIC-authority flows — and if it ever fired, a mismatched
            // token fails the promote fence truthfully at re-promotion.
            quicDirect?.applyStandbyToken(freshToken, epoch: response.epoch)
        default:
            break
        }
    }

    /// Stages a fresh WSS relay in the SERVER standby role (never evicts
    /// the incumbent primary), returning the session once its greeting
    /// carried the standby token + epoch (nil on any failure — the
    /// incumbent is untouched). Build-64: the caller receives the created
    /// instance so a stale caller can only ever reconcile against (and
    /// tear down) its OWN session, never the shared staged slot.
    @discardableResult
    private func stagePolicyRelayStandby(callId: String, gen: UInt64) async -> WebSocketCallMedia? {
        do {
            let request = try await api.mediaWebSocketStandbyRequest(callId: callId)
            guard gen == generation, !ended else { return nil }
            let session = wsMediaFactory()
            try await session.connect(request: request)
            guard gen == generation, !ended else { session.closeWithoutAudio(); return nil }
            guard session.lastReadyStandbyToken != nil, session.lastReadyEpoch != nil else {
                session.closeWithoutAudio()
                return nil
            }
            session.markHotStandby()
            session.startPingSampling()
            policyStagedRelay = session
            return session
        } catch {
            AppLog.call.notice("policy staged relay attach failed: \(error)")
            return nil
        }
    }

    /// Policy-driven direct adoption (Q→RTC / W→RTC): only when the
    /// existing direct machinery holds a PROVEN-ready candidate; otherwise
    /// the proposal rolls back truthfully (nothing adopts). Build-62: on
    /// success the QUIC path DEMOTES to hot standby and the QUIC
    /// controller remains the single decision authority (no close, no
    /// legacy-selector resume). The server-side ICE promote demoted the
    /// QUIC primary into the ws-standby slot with a fresh token: learn it
    /// from the server snapshot so a later Q re-promotion stays possible.
    /// Test seam: overrides the RTC adoption inside `adoptDirectForPolicy`
    /// (the legacy proven-candidate machinery is scripted at a different
    /// level; the ordering contract under test lives HERE).
    var debugAdoptDirectPolicyOverride: (() async -> Bool)?

    private func adoptDirectForPolicy() async -> Bool {
        let adopt: () async -> Bool
        if let debugAdoptDirectPolicyOverride {
            adopt = debugAdoptDirectPolicyOverride
        } else {
            guard let route, route.hasProvenReadyDirectCandidateForPolicy() else { return false }
            adopt = { await route.adoptProvenDirectForPolicy() }
        }
        // Audio exclusivity at the handover boundary: the ACTUAL incumbent
        // (the live QUIC graph when Q owns audio — NOT merely wsMedia)
        // SUSPENDS before the RTC adoption can open its capture graph; a
        // failed or cancelled handoff restores it (single capture owner at
        // every instant).
        // Build-63 ORIGINAL-call fencing: capture the committing lifecycle
        // identity BEFORE any await — the post-await restore may only touch
        // THIS call/generation (a hangup/new call during the adopt must
        // never be resumed/muted/closed from this stale callback).
        // Build-64: the QUIC media object is captured with the same
        // identity fence and used for every post-await restore/demote —
        // never re-read from the shared controller state mid-callback.
        let originalGen = generation
        let originalCallId = activeGatewayId
        let quicOwnerAtAdopt = quicDirect
        let quicMedia = quicDirect?.quicCallMedia
        quicMedia?.markAudioStaged(true)
        quicMedia?.suspendAudioForStandbyHandover()
        let ok = await adopt()
        guard ok else {
            // A superseded lifecycle (hangup/new call/interruption during
            // the adopt) owns state now: this stale callback touches
            // NOTHING — no resume, no mute, no close against the newer
            // call.
            guard originalGen == generation, !ended,
                  originalCallId == activeGatewayId else { return false }
            // Build-62 server truth: if the direct adopt never swapped the
            // server roles, QUIC is still primary and its audio restores
            // honestly; if an ICE swap landed underneath, restore the WSS
            // (QUIC) primary SERVER-side first — a merely local resume
            // would be one-way. An unconfirmed restore enters the bounded
            // recovery, never a healthy publish.
            if let callId = originalCallId, let session = wsMedia,
               await restoreWSPrimaryAfterFailedQUICPromotion(
                   gen: originalGen, callId: callId, wsSession: session,
                   familyHoldsQUIC: false) {
                // Build-64 restore re-fence: the restore AWAIT may have
                // spanned a hangup/new call — the captured QUIC graph must
                // never restart after a newer lifecycle took the mic
                // (capture the controller identity too: a same-generation
                // controller replacement means the captured media is not
                // the live authority's anymore).
                guard originalGen == generation, !ended,
                      originalCallId == activeGatewayId,
                      quicDirect === quicOwnerAtAdopt else { return false }
                quicMedia?.markAudioStaged(false)
                quicMedia?.promoteAudioOwnership()
                if let activated = AudioSessionBridge.shared.activeSession {
                    _ = quicMedia?.startOwnedAudioGraph(with: activated)
                }
            } else if let callId = originalCallId {
                // Build-64: same re-fence before ANY publish/recovery: a
                // recovery episode is only ever armed for the LIVE call the
                // failure was diagnosed on.
                guard originalGen == generation, !ended,
                      originalCallId == activeGatewayId else { return false }
                publishPhase()
                scheduleMediaFailure(
                    callId: callId, gen: originalGen,
                    message: String(localized: "音频长时间未恢复，已结束通话。"))
            }
            return false
        }
        // Success: only THIS lifecycle may mark/learn against the live
        // sockets (a newer call's media must not be demoted here).
        guard originalGen == generation, !ended,
              originalCallId == activeGatewayId else { return true }
        quicMedia?.markHotStandby()
        if let callId = originalCallId,
           let state = try? await api.mediaState(callId: callId),
           originalGen == generation, !ended {
            // Build-63 typed reconcile: the demoted QUIC appears in the
            // dedicated standbyQuic slot on new gateways; a pre-63 gateway
            // reports it in the (ambiguous) ws-family slot. Prefer the
            // typed field; fall back to the legacy interpretation ONLY
            // when the typed field is absent — never trust an ambiguous
            // ws-family snapshot as confirmed QUIC ownership.
            if state.standbyQuic == true, let freshToken = state.standbyQuicToken {
                quicDirect?.applyStandbyToken(freshToken, epoch: state.epoch)
            } else if state.standbyQuic == nil,
                      state.standbyWs == true, let freshToken = state.standbyWsToken {
                quicDirect?.applyStandbyToken(freshToken, epoch: state.epoch)
            }
        }
        return true
    }

    private func closeQUICDirect() {
        quicAttemptTask?.cancel()
        quicAttemptTask = nil
        if let waiter = quicPromoteWaiter {
            quicPromoteWaiter = nil
            waiter.continuation.resume(returning: nil)
        }
        quicDirect?.close()
        quicDirect = nil
        quicAuthorityRequestedMode = nil
        // An in-flight policy staging holds no audio; close it.
        policyStagedRelay?.closeWithoutAudio()
        policyStagedRelay = nil
        wsStandbyFence = nil
        // The QUIC authority is gone: the legacy selector resumes its
        // automatic routing for the remaining transports.
        route?.automaticStandDown = false
    }

    /// Staged rollback used by the route controller: attaches a fresh WSS
    /// host WITHOUT taking audio ownership. Returns true once `ready`.
    /// `proof`, when supplied (recovery attempt), binds this session's
    /// fresh-media callback to the exact episode+attempt identity.
    @discardableResult
    private func stageWSMedia(callId: String, proof: MediaRecoveryController.ProofIdentity? = nil,
                              onCandidate: ((WebSocketCallMedia) -> Void)? = nil) async -> Bool {
        let gen = generation
        do {
            let ice = try await api.iceConfiguration(callId: callId)
            guard gen == generation else { return false }
            _ = try await attachWSSession(
                callId: callId, uuid: UUID(), gen: gen, ice: ice,
                selfManagedAudio: false, ownAudioImmediately: false,
                recoveryProof: proof, onCandidate: onCandidate)
            guard gen == generation else { return false }
            return true
        } catch {
            AppLog.call.notice("staged relay attach failed: \(error)")
            return false
        }
    }

    /// Exclusive promotion after the staged relay is the server-side host:
    /// starts the staged relay graph and retires the peer when one exists.
    ///
    /// Build-44 ordering (field regression): the retiring direct peer's
    /// audio device is released BEFORE the staged graph starts — a
    /// concurrent engine start against the live RTC ADM fails ("graph
    /// start error"), and the pre-fix flow then failed the staged socket
    /// AND published a healthy relay AND let the call die. Now a failed
    /// start ROLLS BACK: the staged socket closes without audio, the
    /// previous transport (direct peer or old relay — never closed) is
    /// restored and its audio device resumed. Because the staged attach
    /// already made the SERVER commit the relay, a preserved direct peer is
    /// media-UNPROVEN afterwards (ICE connectivity is not server-routing
    /// proof): the route always publishes the honest degraded/recovery
    /// state, and the bounded failure machinery (monitor re-handover,
    /// server reconciliation, media-failure grace) settles the call. Only a
    /// PROVEN running staged graph reports true, so the route may publish
    /// relay.
    ///
    /// Fencing: the retry loop and every post-await step re-check the call
    /// generation (hangup/new call), the bridge ownership epoch
    /// (interruption/deactivation/new activation/media reset) and the
    /// authoritative system audio session. A stale promotion performs NO
    /// cleanup against the newer lifecycle (no staged-state clearing, no
    /// shared-audio resume, no publish). A staged promotion NEVER
    /// self-activates a session and never fails the socket from a
    /// graph-start error.
    @discardableResult
    /// - Parameter retainRetiringPeer: build-50 hot standby: the retiring
    ///   direct peer is DEMOTED (ADM suspended, transport kept) instead of
    ///   closed, so ICE->WS->ICE preserves the SAME peer connection with
    ///   zero closes until hangup.
    private func promoteStagedWSMedia(retiringDirect peer: DirectProbeControlling?, retainRetiringPeer: Bool = false) async -> Bool {
        // Build-48: promotion does NOT clear a bounded recovery episode —
        // only fresh media (the first real inbound frame on the promoted
        // transport) does. A stale/passive timer handle is still stopped.
        mediaRecoveryTask?.cancel()
        mediaRecoveryTask = nil
        let gen = generation
        let bridgeEpoch = AudioSessionBridge.shared.eventEpoch
        let superseded = stagedPreviousRelay
        guard let session = wsMedia else { return false }
        // Exclusive promotion: this session may now open the capture graph
        // (it was gated off while the previous transport carried audio).
        session.promoteAudioOwnership()
        // A graph-start failure inside this window is reported, never fatal
        // to the socket: this function owns the failure decision.
        session.graphStartFailureNonfatal = true
        defer { session.graphStartFailureNonfatal = false }
        // Ordered audio handover: release the retiring peer's ADM FIRST
        // (the transport stays alive for rollback). Mirrors the proven
        // relay→direct adoption sequence (two concurrent audio owners
        // break the engine start).
        peer?.suspendAudioDeviceForHandover()
        // Build-51 field repair (direct→relay ownership failure): the
        // retiring ADM's audio-unit teardown is ASYNCHRONOUS — the engine
        // start fails with WSAudioGraph code=1 ("no usable input format")
        // while the input still reports a zero format (18 code=1 failures /
        // 9 rolled-back promotions in one field call under the old blind
        // 150 ms settle). Wait BOUNDED for the input to become usable
        // before the first start instead of racing it; a timeout proceeds
        // and the start then reports truthfully (single rollback).
        await waitForPromotionInputReadiness(session: session, gen: gen,
                                             bridgeEpoch: bridgeEpoch)
        var started = false
        for attempt in 0...max(0, promotionGraphStartRetries) {
            if attempt > 0 {
                DiagnosticsCensus.shared.increment("audio.wsPromotedGraphRetry")
                try? await Task.sleep(nanoseconds: promotionGraphRetrySettleNanoseconds)
            }
            // Post-await fence: the call, the audio ownership and the live
            // session must all still be the ones this promotion was armed
            // with — a hangup, an interruption or a newer call supersedes.
            guard gen == self.generation, !ended,
                  activeGatewayId != nil || conference != nil,
                  AudioSessionBridge.shared.eventEpoch == bridgeEpoch,
                  !AudioSessionBridge.shared.isInterrupted,
                  wsMedia === session else { break }
            // A staged promotion requires the authoritative active session
            // (system or self-managed by THIS call). It must never
            // self-activate: with no session the promotion cannot run —
            // roll back instead of claiming audio against nothing.
            guard let activated = AudioSessionBridge.shared.activeSession else {
                DiagnosticsCensus.shared.increment("audio.wsPromotionNoActiveSession")
                break
            }
            started = session.startOwnedAudioGraph(with: activated)
            if started { break }
        }
        // A delivered activation may have started the staged graph while the
        // retry loop fenced (the bridge replays activations to `wsMedia`).
        // That is success ONLY against THIS call's identity: a newer call
        // (generation), an ended call, or a replaced `wsMedia` must never
        // let a stale promotion retire the peer or publish relay. A newer
        // bridge epoch with the SAME call identity may legitimately start
        // the graph (the activation replay above), so it is not a blocker.
        if !started, session.isGraphRunning,
           gen == self.generation, !ended, wsMedia === session {
            started = true
        }
        guard started else {
            // A newer call (generation) or audio lifecycle event (bridge
            // epoch: interruption/deactivation/new activation/media reset)
            // owns the state now. This stale promotion must NOT clear
            // staged state, resume shared audio, or publish against it —
            // the delivered lifecycle events drive audio from here.
            guard gen == self.generation,
                  AudioSessionBridge.shared.eventEpoch == bridgeEpoch else {
                // A newer lifecycle owns audio now: keep its state (no
                // staged-state clearing, no ADM resume, no publish), but
                // re-gate the staged session so a later activation replay
                // cannot start its graph underneath the restored direct
                // peer — two concurrent audio owners must never exist.
                session.markAudioStaged()
                DiagnosticsCensus.shared.increment("call.relayPromotionSuperseded")
                return false
            }
            // Roll back: the staged socket never took audio; close it and
            // restore the previous transport untouched. A nil `superseded`
            // means the previous transport is the direct peer, which the
            // route controller still owns — resume its audio device.
            stagedPreviousRelay = nil
            if wsMedia === session { wsMedia = superseded }
            session.closeWithoutAudio()
            peer?.resumeAudioDeviceAfterFailedHandover()
            // Evidence only: the staged attach already made the SERVER
            // commit the relay, and ICE "connected" alone is NOT proof the
            // server still routes call media to the preserved peer — the
            // route therefore always publishes the degraded/recovery state
            // after a failed promotion (never a kept-line claim), and the
            // bounded failure machinery (monitor re-handover, media-failure
            // grace) settles the call.
            let directUsable = peer?.connected ?? false
            DiagnosticsCensus.shared.increment("call.relayPromotionRolledBack")
            DiagnosticsStore.shared.log("call",
                "relay promotion failed; rolled back (directUsable=\(directUsable))")
            publishPhase()
            return false
        }
        stagedPreviousRelay = nil
        latestMedia = .connected
        session.startPingSampling()
        // The staged socket reached `.connected` during the handshake BEFORE
        // it became `wsMedia`, so that quality emission was dropped by the
        // identity fence; re-publish now so the interruption banner from the
        // failing direct transport clears on the healthy relay (build 38).
        session.republishCurrentQuality()
        superseded?.retireAfterHandover()
        if retainRetiringPeer {
            // Hot standby: keep the peer's transport alive as the new
            // direct standby (its ADM is already suspended above).
        } else {
            peer?.closeTransport()
        }
        publishPhase()
        return true
    }

    /// Bounded graph-start retries for a staged promotion (the first
    /// attempt can race the retiring ADM's asynchronous teardown; ONE
    /// settle-and-retry covers it without unbounded churn). Injectable for
    /// deterministic tests.
    var promotionGraphStartRetries = 1
    /// Settle between a failed staged graph start and its single retry.
    var promotionGraphRetrySettleNanoseconds: UInt64 = 150_000_000
    /// Bounded wait for the retiring ADM's teardown to release the audio
    /// input before the staged graph starts (build-51). The start's hard
    /// guard is a usable input format; without this wait the first attempt
    /// fails with WSAudioGraph code=1 while the SDK unit is still stopping.
    var promotionInputReadinessTimeout: TimeInterval = 2.0
    /// Poll cadence for the input-readiness wait (production 40 ms).
    var promotionInputReadinessPollNanoseconds: UInt64 = 40_000_000

    /// Polls the staged session's live input readiness until it reports
    /// usable or the bounded timeout expires. DEADLINE-BOUNDED: it can
    /// never prevent the initial prepare/configuration indefinitely — on
    /// timeout the caller proceeds and the start reports truthfully
    /// (single rollback). Every poll re-checks the call/bridge/session
    /// identity so a superseded promotion stops waiting (and never starts
    /// a graph against a newer lifecycle).
    private func waitForPromotionInputReadiness(session: WebSocketCallMedia,
                                                gen: UInt64,
                                                bridgeEpoch: UInt64) async {
        let deadline = Date().addingTimeInterval(promotionInputReadinessTimeout)
        while Date() < deadline {
            guard gen == self.generation, !ended,
                  AudioSessionBridge.shared.eventEpoch == bridgeEpoch,
                  !AudioSessionBridge.shared.isInterrupted,
                  wsMedia === session else { return }
            if session.isAudioInputReady() { return }
            try? await Task.sleep(nanoseconds: promotionInputReadinessPollNanoseconds)
        }
    }

    /// Build-51: after a relay promotion whose fenced command acked but
    /// whose LOCAL audio transfer failed, the gateway still holds the relay
    /// as primary with the direct peer demoted (its uplink is discarded
    /// server-side — the far end goes one-way). Restore the direct primary
    /// SERVER-side using the acked direct-standby fence, and PROVE the
    /// restore before reporting success:
    ///  - a validated ack (primary == ice, no epoch drift), or
    ///  - a reconcile against the server's ACTUAL roles after a rejected /
    ///    lost ack (the existing mediaState path).
    /// Only a CONFIRMED restore refreshes the standby tokens (the demoted
    /// relay's new fence) and drops the now-primary direct fence — a stale
    /// standby fence for the carrier must never survive. When the restore
    /// cannot be confirmed, the call enters the EXISTING bounded 45 s
    /// recovery (never a false "restored", never a redial). Every await is
    /// generation/ended-fenced: a superseded promotion touches nothing and
    /// lets the newer lifecycle decide.
    private func restoreDirectPrimaryAfterFailedRelayPromotion(
        gen: UInt64, callId: String,
        fence: HotStandbyController.StandbyFence?) async {
        guard gen == self.generation, !ended else { return }
        guard let fence else {
            DiagnosticsStore.shared.log("route",
                "relay rollback: no direct standby fence; cannot restore server roles")
            scheduleMediaFailure(callId: callId, gen: gen,
                                 message: String(localized: "音频长时间未恢复，已结束通话。"))
            return
        }
        /// A fence may only be carried by a LOCALLY SURVIVING standby
        /// transport. The staged relay session was closed by the rollback
        /// (standbyRelay == nil): reinstalling its fence from the ack would
        /// leave a dead fence that blocks the inline rebuild — a fresh
        /// manual selection would wait for the watchdog instead of
        /// attaching a new standby immediately.
        func localStandbyAlive(_ kind: HotStandbyController.RouteKind) -> Bool {
            switch kind {
            case .relay: return standbyRelay != nil
            case .direct: return standbyDirect != nil
            }
        }
        /// Shared reconcile tail: returns true only when the server's
        /// ACTUAL roles confirm the direct path is primary again, with the
        /// current standby tokens installed — ONLY for locally surviving
        /// transports (never an old token paired with a new epoch, never a
        /// fence for the just-closed relay session).
        func reconcileAndConfirm() async -> Bool {
            guard gen == self.generation, !ended,
                  let state = try? await api.mediaState(callId: callId) else { return false }
            guard gen == self.generation, !ended else { return false }
            guard state.primary == "ice" else { return false }
            standbyFences[.direct] = nil
            hotStandby?.removeStandby(.direct)
            if state.standbyIce == true, let token = state.standbyIceToken,
               localStandbyAlive(.direct) {
                installStandbyFence(kind: .direct, token: token, epoch: state.epoch)
            }
            if state.standbyWs == true, let token = state.standbyWsToken,
               localStandbyAlive(.relay) {
                installStandbyFence(kind: .relay, token: token, epoch: state.epoch)
            }
            return true
        }
        func confirmFromAck(_ response: V2PromoteStandbyResponse) {
            standbyFences[.direct] = nil
            hotStandby?.removeStandby(.direct)
            if let demoted = response.standby, let token = response.standbyToken {
                let kind: HotStandbyController.RouteKind = demoted == "ice" ? .direct : .relay
                if localStandbyAlive(kind) {
                    installStandbyFence(kind: kind, token: token, epoch: response.epoch)
                }
            }
        }
        do {
            let response = try await api.promoteStandby(
                callId: callId, transport: "ice",
                token: fence.token, expectEpoch: fence.epoch,
                nonce: "rb-\(UUID().uuidString)")
            guard gen == self.generation, !ended else { return }
            // Validate the ack: the direct path must be primary and the
            // epoch must be self-consistent (a drifted currentEpoch means
            // another swap raced us — reconcile instead of trusting it).
            let epochSelfConsistent = (response.currentEpoch ?? response.epoch) == response.epoch
            if response.primary == "ice", epochSelfConsistent {
                confirmFromAck(response)
                DiagnosticsStore.shared.log("route",
                    "relay rollback restored direct primary server-side (epoch=\(response.epoch))")
                return
            }
            if await reconcileAndConfirm() {
                DiagnosticsStore.shared.log("route",
                    "relay rollback restored direct primary via reconcile (epoch \(fence.epoch))")
                return
            }
            // A superseded restore (reconcile returned false because the
            // call died mid-await) schedules NOTHING against the new
            // lifecycle — only a LIVE, genuinely-unconfirmed restore enters
            // the bounded recovery.
            guard gen == self.generation, !ended else { return }
            AppLog.media.notice("relay rollback restore unconfirmed: server roles mismatch")
            DiagnosticsStore.shared.log("route",
                "relay rollback restore unconfirmed: entering bounded recovery")
            scheduleMediaFailure(callId: callId, gen: gen,
                                 message: String(localized: "音频长时间未恢复，已结束通话。"))
        } catch {
            // Rejected or ack lost: reconcile BEFORE deciding — the swap
            // may have landed while the ack died.
            if await reconcileAndConfirm() {
                DiagnosticsStore.shared.log("route",
                    "relay rollback restored direct primary via ack-loss reconcile")
                return
            }
            guard gen == self.generation, !ended else { return }
            AppLog.media.notice("relay rollback server restore failed: \(error)")
            DiagnosticsStore.shared.log("route",
                "relay rollback server restore failed: entering bounded recovery")
            scheduleMediaFailure(callId: callId, gen: gen,
                                 message: String(localized: "音频长时间未恢复，已结束通话。"))
        }
    }

    private func reattachWSMedia(callId: String) async -> Bool {
        await stageWSMedia(callId: callId)
    }

    /// Build-62: after a QUIC promotion whose fenced command acked but whose
    /// LOCAL graph start failed, the gateway holds QUIC as primary with the
    /// WSS demoted to the standby slot (its uplink discarded server-side).
    /// Restore the WSS primary SERVER-side and prove it before any local
    /// audio resumes:
    ///  - the demoted WSS standby identity comes from the server snapshot
    ///    (never a co-returned or guessed token);
    ///  - a validated ack (primary == ws, self-consistent epoch), or
    ///  - a reconcile against the server's ACTUAL roles after a lost or
    ///    rejected ack.
    /// A confirmed restore ALSO installs the demoted QUIC standby fence (the
    /// swap-minted fresh token) so the three-path authority keeps working.
    /// Every await is generation/session-fenced: a superseded rollback
    /// touches nothing. Returns true ONLY on a confirmed server restore.
    ///
    /// - Parameter familyHoldsQUIC: true when the promoted ack just made
    ///   QUIC the ws-family primary (the REAL WSS sits in the standby
    ///   slot): a fenced swap-back is REQUIRED even though the family reads
    ///   "ws". False for the direct-adopt rollback, where "ws" primary
    ///   means QUIC itself never lost the slot. mediaState cannot
    ///   distinguish the two (the family string is shared), so with
    ///   `familyHoldsQUIC` an ack-less rollback is UNCONFIRMED by
    ///   construction and never claims success.
    private func restoreWSPrimaryAfterFailedQUICPromotion(
        gen: UInt64, callId: String, wsSession: WebSocketCallMedia,
        familyHoldsQUIC: Bool) async -> Bool {
        func reconcileAndConfirm() async -> Bool {
            // The ws-family string cannot tell "QUIC still primary" from
            // "real WSS swapped back": only a validated ack confirms the
            // QUIC case; never claim success from the ambiguous snapshot.
            guard !familyHoldsQUIC else { return false }
            guard gen == self.generation, !ended, wsMedia === wsSession,
                  let state = try? await api.mediaState(callId: callId) else { return false }
            guard gen == self.generation, !ended, wsMedia === wsSession else { return false }
            guard state.primary == "ws" else { return false }
            applyDemotedQUICIdentity(from: state)
            return true
        }
        do {
            // Learn the demoted WSS standby identity from the server
            // snapshot (the WSS-message promoted ack does not carry it).
            guard let state = try? await api.mediaState(callId: callId),
                  state.standbyWs == true, let token = state.standbyWsToken else {
                return await reconcileAndConfirm()
            }
            guard gen == self.generation, !ended, wsMedia === wsSession else { return false }
            // Nothing to swap back when the WSS family is ALREADY primary
            // and QUIC never held the slot (the failed adoption never
            // changed the server roles).
            if state.primary == "ws", !familyHoldsQUIC {
                return true
            }
            let response = try await api.promoteStandby(
                callId: callId, transport: "ws", token: token,
                expectEpoch: state.epoch, nonce: "rb-\(UUID().uuidString)")
            guard gen == self.generation, !ended, wsMedia === wsSession else { return false }
            let epochSelfConsistent = (response.currentEpoch ?? response.epoch) == response.epoch
            if response.primary == "ws", epochSelfConsistent {
                applyDemotedQUICIdentity(from: response)
                DiagnosticsStore.shared.log("route",
                    "quic rollback restored WSS primary server-side (epoch=\(response.epoch))")
                return true
            }
            // A raced swap: trust only the server's actual roles.
            return await reconcileAndConfirm()
        } catch {
            // Rejected or ack lost: reconcile BEFORE deciding.
            return await reconcileAndConfirm()
        }
    }

    /// Bounded window for an explicit manual switch whose standby is not
    /// ready yet: the owner rebuilds/awaits the selected standby inside it.
    var manualOwnedSwitchWindow: TimeInterval = 10

    /// User changes the route mode mid-call (in-call compact menu).
    /// Build-50: capability-enabled calls route EVERY explicit selection
    /// through the single owner — a ready warm standby rides the fenced
    /// make-before-break promotion; an unready standby is rebuilt/awaited
    /// bounded; a never-ready selected path leaves the audible carrier with
    /// a truthful notice. Legacy startup/advisor/primary-commit paths are
    /// never reached in a hot-standby call.
    func selectRouteMode(_ mode: MediaRouteMode) async {
        MediaRoutePreferenceStore.shared.setMode(mode, for: routeGatewayID)
        // Build-62 single authority: while the QUIC three-path authority is
        // active IT owns every explicit selection (forced-mode mapping);
        // the hot-standby promote path must not compete with it.
        // Build-65: write the authority's OWN live requested mode — the
        // legacy route object may not exist at all on a WSS+QUIC call (no
        // direct WebRTC advertised), and without this the forced choice
        // was read back as the immutable startup default.
        if quicDirect != nil {
            quicAuthorityRequestedMode = mode
            route?.applyModeWithoutTeardown(mode)
            return
        }
        if let controller = hotStandby, let route {
            let current: HotStandbyController.RouteKind =
                route.activeTransportIsDirect ? .direct : .relay
            // Record the choice immediately WITHOUT teardown: the watchdog
            // honors a forced selection (never auto-switches back) and
            // recovery respects it.
            route.applyModeWithoutTeardown(mode)
            guard mode != .auto else { return } // scorer owns auto decisions
            let wanted: HotStandbyController.RouteKind =
                mode == .direct ? .direct : .relay
            guard wanted != current else { return }
            let ok = await ownedStandbyPromoteAttempt(
                wanted: wanted, controller: controller,
                window: manualOwnedSwitchWindow)
            if !ok {
                // No notice when a newer user choice cancelled the request.
                guard route.routeState.mode == mode else { return }
                let message = wanted == .direct
                    ? String(localized: "直连不可用，继续使用中继。")
                    : String(localized: "中继不可用，继续使用直连。")
                self.onRouteNotice?(message, true)
            }
            return
        }
        guard let route else { return }
        await route.setMode(mode)
    }

    /// Hot directional denoise toggle for any active WSS transports
    /// (1:1 and conference). nil leaves that direction unchanged. The
    /// gateway applies it at the next 20 ms frame boundary; direct WebRTC
    /// uses the gateway-side mix state set on the next attach.
    func setDenoiseLive(uplink: Bool?, downlink: Bool?) {
        wsMedia?.sendDenoiseControl(uplink: uplink, downlink: downlink)
        wsConferenceMedia?.sendDenoiseControl(uplink: uplink, downlink: downlink)
    }

    var currentRouteState: CallRouteState? { route?.routeState }

    /// Build-48 bounded recovery. On an ICE/WSS `disconnected` OR `failed`
    /// state the same call gets an absolute 45 s window: the controller
    /// attempts immediate + 1/2/4/8 s reconnects (current path first in auto,
    /// chosen route only when forced) of the SAME call id, never a new dial.
    /// Recovery is cleared ONLY by fresh inbound media (see
    /// `markRecoveryFreshMedia`), not by `.connected`/RTT alone. Revoked
    /// authorization and explicit/remote ends never enter here (they call
    /// fail/finish paths which cancel the controller in teardown).
    private func scheduleMediaFailure(callId: String, gen: UInt64, message: String) {
        if mediaRecovery == nil {
            mediaRecovery = MediaRecoveryController(policy: MediaRecoveryPolicy(window: mediaRecoveryWindow))
        }
        guard let mediaRecovery, !mediaRecovery.isActive else { return }
        // Current route decides attempt order; falls back to relay when no
        // route controller exists yet.
        let isDirect = route?.activeTransportIsDirect == true
            || (media != nil && wsMedia == nil)
        let current: MediaRecoveryPolicy.Route = isDirect ? .direct : .relay
        // Build-65: the SAME live requested mode the QUIC authority maps
        // from — the recovery attempt order and alternate availability
        // must reflect the user's CURRENT choice (a forced mode saved
        // mid-call persists and shapes the recovery of the next call),
        // not the coordinator's init-time snapshot.
        let mode = liveRequestedMode
        // Alternate availability: in AUTO mode the cross-route attempt runs
        // through the route controller's proven single-owner handover
        // (performRelayHandover / performRequestDirect); forced modes never
        // cross routes (enforced here AND by the policy).
        let alternate = mode == .auto && directAdvertised && route != nil
        publishPhase()
        // Build-50 hot standby: in AUTO mode a FRESH warm standby takes over
        // on primary failure (failure bypasses the scorer's cooldown/dwell).
        // The takeover runs as the FIRST attempt INSIDE the serialized
        // recovery episode (never concurrently with it); forced-route
        // semantics are unchanged: a forced selection only recovers its
        // SELECTED route.
        pendingStandbyTakeover = nil
        if mode == .auto, hotStandby != nil, mediaRecovery.isActive != true {
            let currentKind: HotStandbyController.RouteKind = isDirect ? .direct : .relay
            if let standbyKind = standbyFences.keys.first(where: { $0 != currentKind }),
               let at = standbyFreshAt[standbyKind], Date().timeIntervalSince(at) <= 5 {
                pendingStandbyTakeover = standbyKind
            }
        }
        mediaRecovery.begin(
            callId: callId, mode: mode, current: current, alternateAvailable: alternate,
            onAttempt: { [weak self] attempt in
                // Awaited by the controller: attempts are strictly serialized,
                // never two concurrent replacement transports.
                await self?.performRecoveryAttempt(callId: callId, gen: gen, attempt: attempt)
            },
            onExpire: { [weak self] in
                // Runs inline on the main actor from the controller's
                // independent deadline task: no queued Task, so a fresh-media
                // callback already delivered can never lose a race to expiry.
                guard let self, gen == self.generation else { return }
                await self.failActiveCall(message: message)
            })
    }

    // MARK: Network-path / media-stall recovery (build 66)

    /// Starts the path observation for an active call (idempotent). The
    /// observer reports MEANINGFUL transitions only (deduplicated and
    /// debounced by `CallPathTracker`), delivered on the main actor.
    private func startPathObservationIfNeeded() {
        guard pathObserver == nil else { return }
        let observer = pathObserverFactory()
        observer.onEvent = { [weak self] event in
            self?.handleCallPathEvent(event)
        }
        pathObserver = observer
        observer.start()
        // Seed the current generation so only SUBSEQUENT transitions act.
        networkGeneration = observer.currentGeneration
        networkPathUsable = observer.isReachable
    }

    private func stopPathObservation() {
        pathObserver?.onEvent = nil
        pathObserver?.stop()
        pathObserver = nil
        lastPathNudgeUptime = nil
    }

    /// One path transition. Duplicate/stale deliveries are fenced by the
    /// monotonic generation: a replayed event can never re-arm anything.
    private func handleCallPathEvent(_ event: CallPathEvent) {
        switch event {
        case .baseline(let generation):
            // First snapshot of this observer: identity only. Never a
            // cutover — no revalidation, no nudge, no attempt.
            guard generation > networkGeneration else { return }
            networkGeneration = generation
            networkPathUsable = pathObserver?.isReachable ?? true
        case .becameUnreachable(let generation):
            guard generation > networkGeneration else { return }
            networkGeneration = generation
            networkPathUsable = false
            // No attempts while the OS reports no usable path: they would
            // burn the single bounded window on a dead network. The
            // reachable edge revalidates the carrier immediately below.
            DiagnosticsStore.shared.log("call",
                "network path lost (generation \(generation))")
        case .becameReachable(let generation), .pathChanged(let generation, _):
            guard generation > networkGeneration else { return }
            networkGeneration = generation
            networkPathUsable = true
            guard !ended, let callId = activeGatewayId else { return }
            revalidateCarrierOnFreshPath(callId: callId)
        }
    }

    /// A fresh usable path appeared (Wi-Fi ↔ cellular or reachability
    /// returned): the old generation's transports and candidates are
    /// revalidated NOW instead of waiting out a stale transport timeout.
    private func revalidateCarrierOnFreshPath(callId: String) {
        // A pinned QUIC socket cannot migrate to a new interface: retire it
        // when it is no longer carrying genuine media (a socket that is
        // still fresh keeps the call). This may itself arm the recovery
        // through the owner-death path.
        quicDirect?.noteNetworkPathChanged(staleGrace: pathChangeStallGrace)
        if mediaRecovery?.isActive == true {
            // The episode is already running: shorten the inter-attempt wait
            // AND interrupt an in-flight attempt that is still negotiating
            // over the dead path, so the fresh path is attempted promptly
            // (rate limited; no second concurrent attempt, no deadline
            // extension).
            nudgeRecoveryForFreshPath()
            return
        }
        guard let reason = stalledCarrierEvidence(grace: pathChangeStallGrace) else {
            // The carrier was still producing media at the event instant —
            // but NWPathMonitor often reports the transition a moment BEFORE
            // the old interface actually dies. Recheck once after the short
            // grace so the recovery is armed within ~1.5 s of the real
            // cutover instead of waiting out the full stall backstop.
            schedulePathCarrierRecheck(callId: callId)
            return
        }
        DiagnosticsStore.shared.log("call",
            "carrier \(reason) stalled across a network path change; arming bounded recovery")
        scheduleMediaFailure(callId: callId, gen: generation,
                             message: String(localized: "音频长时间未恢复，已结束通话。"))
    }

    /// One-shot recheck scheduled after a path transition when the carrier
    /// was still fresh at the event instant (see `revalidateCarrierOnFreshPath`).
    /// Bounded: replaced by any newer event, fenced on call generation, and it
    /// only arms the existing bounded recovery — never ends the call.
    private func schedulePathCarrierRecheck(callId: String) {
        pathRecheckTask?.cancel()
        let grace = max(0.2, pathChangeStallGrace)
        let gen = generation
        pathRecheckTask = Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(grace * 1_000_000_000))
            guard let self, !Task.isCancelled, gen == self.generation, !self.ended,
                  self.activeGatewayId == callId else { return }
            guard self.mediaRecovery?.isActive != true else { return }
            guard let reason = self.stalledCarrierEvidence(grace: grace) else { return }
            // A stale pinned QUIC carrier is retired through the same
            // socket-death lifecycle; its owner-death arms the recovery.
            self.quicDirect?.noteNetworkPathChanged(staleGrace: grace)
            DiagnosticsStore.shared.log("call",
                "carrier \(reason) stalled after the path transition; arming bounded recovery")
            self.scheduleMediaFailure(callId: callId, gen: gen,
                message: String(localized: "音频长时间未恢复，已结束通话。"))
        }
    }

    /// Rate-limited prompt attempt for a live recovery episode on a fresh
    /// path (duplicate edges coalesce; the controller enforces its own
    /// cooldown as well). An attempt stuck in an old-path attach is
    /// INTERRUPTED — bounded, fenced by call generation + attempt identity.
    private func nudgeRecoveryForFreshPath() {
        let now = ProcessInfo.processInfo.systemUptime
        if let last = lastPathNudgeUptime, now - last < pathRecoveryNudgeCooldown { return }
        lastPathNudgeUptime = now
        mediaRecovery?.retryNow()
    }

    /// The ACTIVE carrier's genuine media-health evidence: nil when media is
    /// fresh (or no carrier is proven yet), a short reason when the carrier
    /// is genuinely stalled. Connection state alone NEVER qualifies — a
    /// connected socket whose real frames stopped AND whose own RTT probes
    /// stopped is dead; healthy content silence keeps frames flowing.
    private func stalledCarrierEvidence(grace: TimeInterval) -> String? {
        let now = Date()
        if let controller = quicDirect, controller.ownsAudio,
           let media = controller.quicCallMedia {
            guard media.hasProvenInboundMedia,
                  !media.isInboundRealFrameFresh(within: grace, now: now) else { return nil }
            // Build-67 (same contract as the relay branch above): the QUIC
            // gateway also emits a downlink frame every 20 ms — silence
            // included (QUICCallMedia.isInboundRealFrameFresh documents the
            // identical expected-media contract), while the 1 s probe echoes
            // are only CONTROL-plane liveness. Requiring the probes to stop
            // too would leave the exact ping-alive/no-audio hole this build
            // repairs: a stalled QUIC media plane with live echoes would keep
            // the call silent and "healthy". Bounded and consistent: the
            // caller retires the stalled pinned carrier and arms the same
            // recovery; content silence (frames still arriving) never arms,
            // and a never-proven carrier is still excluded above.
            return "quic"
        }
        if let ws = wsMedia, ws.state == .connected, !ws.isHotStandby {
            guard ws.lastInboundRealFrameAt != nil,
                  !ws.isInboundRealFrameFresh(within: grace, now: now) else { return nil }
            // Build-67 stuck-silence repair (user-confirmed: total loss of
            // actual audio mid-call while the call UI still showed an
            // ongoing call; the user hung up manually). The relay downlink
            // streams one frame per 20 ms media slot for the whole call —
            // silence is DELIVERED as silent frames (0.3.55(65) 5G trace:
            // 1532 inbound pushes in 28.5 s, 770 of them silent PCM). A
            // still-answering control plane (fresh pongs) therefore does NOT
            // make missing REAL frames "content silence": it is a dead media
            // path with a healthy socket — exactly the state the previous
            // "frames AND pongs stopped" condition could never see, so the
            // call stayed silent with no recovery. The arm is the same
            // bounded recovery (staged replacement, never a hangup by the
            // watchdog itself), and content silence — frames still arriving
            // — still never arms.
            return "relay"
        }
        return nil
    }

    /// Backstop watchdog for a silently-dead carrier on an unchanged path
    /// (unstable link that never trips NWPathMonitor or a transport-level
    /// error). Bounded: one arm per stall episode, never while a recovery
    /// episode is live, and it only ARMS the existing bounded recovery —
    /// it never ends the call by itself.
    private func startMediaStallWatchdogIfNeeded(callId: String, gen: UInt64) {
        guard stallWatchdogTask == nil else { return }
        let tick = max(0.1, mediaStallTickInterval)
        stallWatchdogTask = Task { @MainActor [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: UInt64(tick * 1_000_000_000))
                guard let self, gen == self.generation, !self.ended,
                      self.activeGatewayId == callId else { return }
                guard self.mediaRecovery?.isActive != true else { continue }
                guard let reason = self.stalledCarrierEvidence(grace: self.mediaStallGrace) else {
                    // Fresh media (or no proven carrier yet): re-arm for a
                    // future stall episode.
                    self.stallArmed = false
                    continue
                }
                guard !self.stallArmed else { continue }
                self.stallArmed = true
                if reason == "quic" {
                    // A stalled pinned QUIC carrier can never migrate: retire
                    // it through the socket-death lifecycle so no zombie
                    // generation lingers behind the recovered call. When it
                    // was the owner this arms the same bounded recovery.
                    self.quicDirect?.noteNetworkPathChanged(staleGrace: self.mediaStallGrace)
                }
                DiagnosticsStore.shared.log("call",
                    "carrier \(reason) media stalled; arming bounded recovery")
                self.scheduleMediaFailure(callId: callId, gen: gen,
                    message: String(localized: "音频长时间未恢复，已结束通话。"))
            }
        }
    }

    private func stopMediaStallWatchdog() {
        stallWatchdogTask?.cancel()
        stallWatchdogTask = nil
        stallArmed = false
        pathRecheckTask?.cancel()
        pathRecheckTask = nil
    }

    /// One bounded recovery reconnect preserving the SINGLE-AUDIO-OWNER
    /// invariant: a replacement relay is staged (handshake, NO graph) and
    /// exclusively promoted (which retires the incumbent) only once ready;
    /// a replacement direct peer is only swapped in after it negotiates and
    /// the incumbent is closed first in the ordered handover. The alternate
    /// route (auto mode only) is delegated to the route controller's
    /// established handover machinery. Forced modes only attempt their own
    /// route. The controller awaits this, so the next backoff never starts
    /// while a candidate is in flight.
    private func performRecoveryAttempt(callId: String, gen: UInt64,
                                         attempt: MediaRecoveryController.Attempt) async {
        guard gen == generation, !ended else { return }
        pendingRecoveryProof = attempt.identity
        // Build-66: register the owner-side interrupt for THIS attempt. A
        // fresh-path nudge can then cancel the candidate transport still
        // negotiating over the dead path; the handler is identity-fenced so
        // a superseded attempt's candidate is never touched.
        let attemptIdentity = attempt.identity
        mediaRecovery?.setAttemptInterrupt { [weak self] in
            self?.interruptRecoveryAttempt(attemptIdentity)
        }
        defer {
            mediaRecovery?.setAttemptInterrupt(nil)
            recoveryRelayCandidate = nil
            recoveryDirectCandidate = nil
        }
        let mode = self.route?.routeState.mode ?? routeModeDefault
        if mode == .direct, attempt.route != .direct { return }
        if mode == .relay, attempt.route != .relay { return }
        // Build-50 serialized warm-standby takeover (auto mode only): a fresh
        // standby promotes FIRST, before the failed route is rebuilt — an
        // instant warm takeover is the point of the standby; queueing it
        // behind a multi-second re-attach would defeat it. Runs as the first
        // recovery attempt; on failure the normal attempt machinery proceeds.
        if let takeover = pendingStandbyTakeover, let controller = hotStandby {
            pendingStandbyTakeover = nil
            let outcome = await controller.promoteStandby(takeover)
            if case .promoted = outcome {
                mediaRecovery?.succeed(attempt.identity)
                return
            }
        }
        // Is this the CURRENT installed transport or the alternate? The
        // alternate in auto mode is delegated to the route controller's
        // proven single-owner handover machinery (it owns staged relay /
        // probe+commit transactions).
        let currentIsDirect = route?.routeState.active == .direct || (media != nil && wsMedia == nil)
        let currentRoute: MediaRecoveryPolicy.Route = currentIsDirect ? .direct : .relay
        if attempt.route != currentRoute {
            // Auto ALTERNATE (build 49): proven single-owner route machinery
            // with the attempt identity CAPTURED for fresh-media proof — the
            // adopted transport clears THIS episode only on its first real
            // inbound audio (never CONNECTED/RTT, never a delivery-time
            // lookup, so a stale transport or queued frame cannot claim it).
            let adopted = await route?.recoverAlternate(
                proof: attempt.identity,
                onFreshMedia: { [weak self] proof in
                    Task { @MainActor in
                        self?.markRecoveryFreshMedia(proof: proof, gen: gen)
                    }
                }) ?? false
            if !adopted {
                DiagnosticsStore.shared.log("call",
                    "recovery alternate attempt \(attempt.attemptID) did not become the carrier")
            }
            return
        }
        do {
            let proof = attempt.identity
            switch attempt.route {
            case .relay:
                try await recoverRelay(callId: callId, gen: gen, proof: proof)
            case .direct:
                try await recoverDirect(callId: callId, gen: gen, proof: proof)
            }
        } catch is CancellationError {
            return
        } catch {
            AppLog.call.notice("recovery attempt \(attempt.attemptID) failed: \(error)")
        }
    }

    /// Staged relay recovery: handshake first without taking audio, then the
    /// proven promote/retire handover so exactly one transport ever owns the
    /// mic. A failed stage closes only the candidate and leaves the incumbent
    /// (or silence) untouched.
    private func recoverRelay(callId: String, gen: UInt64,
                              proof: MediaRecoveryController.ProofIdentity) async throws {
        // Relay proof is wired inside attachWSSession with the attempt's
        // identity AND the exact session; a frame from a superseded socket is
        // rejected there. The candidate is tracked while its handshake is in
        // flight so a fresh-path nudge can cancel it (build-66).
        guard await stageWSMedia(callId: callId, proof: proof, onCandidate: { [weak self] session in
            self?.recoveryRelayCandidate = session
        }) else { return }
        guard gen == generation, !ended else { return }
        _ = await promoteStagedWSMedia(retiringDirect: nil)
    }

    /// Build-66: the identity-fenced interrupt for an in-flight recovery
    /// attempt. Closes ONLY the candidate transport this attempt created
    /// (never an installed carrier) and only while the episode still owns
    /// exactly that attempt — a superseded attempt or a cleared episode
    /// touches nothing.
    private func interruptRecoveryAttempt(_ identity: MediaRecoveryController.ProofIdentity) {
        guard let recovery = mediaRecovery, recovery.isActive,
              recovery.currentProofIdentity == identity else { return }
        if let staged = recoveryRelayCandidate {
            DiagnosticsStore.shared.log("call",
                "interrupting a stale-path recovery attach (attempt \(identity.attemptID))")
            staged.closeWithoutAudio()
        }
        recoveryDirectCandidate?.close()
    }

    /// Direct recovery: build a fresh candidate peer WITHOUT touching audio,
    /// wire its ongoing state/quality callbacks, wait for the candidate to
    /// actually CONNECT, then close the incumbent (releasing the ADM) before
    /// the candidate takes over audio. Never two live audio owners.
    private func recoverDirect(callId: String, gen: UInt64,
                               proof: MediaRecoveryController.ProofIdentity) async throws {
        let candidate = mediaProvider.makeSession()
        // Build-66: tracked while negotiating so a fresh-path nudge can
        // cancel it (the identity fence lives in interruptRecoveryAttempt).
        recoveryDirectCandidate = candidate
        candidate.setRecoveryFreshMedia { [weak self, weak candidate] in
            Task { @MainActor in
                guard let self, let candidate else { return }
                // Only the INSTALLED transport may prove fresh media.
                guard self.media === candidate else { return }
                self.markRecoveryFreshMedia(proof: proof, gen: gen)
            }
        }
        // Ongoing callbacks: later failures re-enter recovery, quality keeps
        // flowing to the UI (a replacement must not go dark).
        candidate.onState = { [weak self, weak candidate] state in
            Task { @MainActor in
                guard let self, let candidate, gen == self.generation,
                      self.media === candidate || state == .connected else { return }
                self.latestMedia = state
                switch state {
                case .disconnected, .failed:
                    self.scheduleMediaFailure(callId: callId, gen: gen,
                        message: String(localized: "音频长时间未恢复，已结束通话。"))
                default:
                    self.publishPhase()
                }
            }
        }
        candidate.onQuality = { [weak self] quality in
            Task { @MainActor in
                guard let self, gen == self.generation else { return }
                self.latestWebRTCQuality = quality
                self.onQuality?(quality)
            }
        }
        let ice = try await api.iceConfiguration(callId: callId)
        guard gen == generation, !ended else { candidate.close(); throw CancellationError() }
        let offer = try await candidate.makeOffer(ice: ice, relayOnly: transport == "tailnet")
        guard gen == generation else { candidate.close(); throw CancellationError() }
        let answer = try await api.webRTCOffer(
            callId: callId, sdp: offer, transport: transport,
            idempotencyKey: UUID().uuidString)
        guard gen == generation else { candidate.close(); throw CancellationError() }
        try await candidate.applyAnswer(answer.sdp)
        guard gen == generation, !ended, mediaRecovery?.currentProofIdentity == proof else {
            candidate.close()
            return
        }
        // Staged readiness: adopt ONLY once the candidate actually reports
        // connected (bounded). Until then the incumbent stays installed and
        // no second audio graph is started; a candidate that never connects
        // is closed and the next backoff attempt runs.
        let deadline = Date().addingTimeInterval(3)
        while candidate.currentMediaState != .connected, Date() < deadline {
            try? await Task.sleep(nanoseconds: 100_000_000)
            guard gen == generation, !ended, mediaRecovery?.currentProofIdentity == proof else {
                candidate.close()
                return
            }
        }
        guard candidate.currentMediaState == .connected else {
            candidate.close()
            return
        }
        // Ordered single-owner swap: the incumbent's audio device is released
        // BEFORE the candidate takes over; media points at the candidate from
        // here so the fresh-media proof and later failures resolve against it.
        let previous = media
        media = candidate
        previous?.close()
        candidate.setMicMuted(muted)
        if let activated = AudioSessionBridge.shared.activeSession {
            candidate.audioActivated(with: activated)
        }
    }

    /// Fresh-media proof for recovery, fenced by BOTH the call generation and
    /// the full episode identity: an old transport's queued inbound callback
    /// can never clear a newer attempt or a previous episode, and the WSS
    /// path additionally requires the emitting session to still be current.
    func markRecoveryFreshMedia(proof: MediaRecoveryController.ProofIdentity, gen: UInt64) {
        guard gen == generation, !ended else { return }
        guard mediaRecovery?.succeed(proof) == true else { return }
        cancelMediaRecovery()
        mediaRecovery = nil
        latestMedia = .connected
        publishPhase()
    }

    #if DEBUG
    /// Test seam: the most recent direct-recovery candidate session.
    var debugLatestDirectRecoverySession: CallMediaSession? { media }
    /// Test seam: whether a bounded recovery episode is currently active.
    var debugRecoveryActive: Bool { mediaRecovery?.isActive == true }
    // Build-50 hot-standby test seams (DEBUG-equivalent internal accessors).
    var debugHotStandbyStandbyKind: HotStandbyController.RouteKind? { hotStandbyState?.standby }
    var debugHotStandbyStandbyFresh: Bool { hotStandbyState?.standbyFresh ?? false }
    var debugHotStandbyFences: [HotStandbyController.RouteKind: HotStandbyController.StandbyFence] { standbyFences }
    var debugPendingStandbyTakeover: HotStandbyController.RouteKind? { pendingStandbyTakeover }
    var debugRouteActiveIsDirect: Bool { route?.activeTransportIsDirect == true }
    var debugRouteMode: MediaRouteMode? { route?.routeState.mode }
    var debugHotStandbyController: HotStandbyController? { hotStandby }
    var debugStandbyRelayInstalled: Bool { standbyRelay != nil }
    var debugStandbySupported: Bool { standbySupported }
    var debugHotStandbyExists: Bool { hotStandby != nil }
    var debugStandbyDirectInstalled: Bool { standbyDirect != nil }
    /// Test seam: whether a given transport instance is the installed one.
    func debugWSMediaIsInstalled(_ session: WebSocketCallMedia) -> Bool { wsMedia === session }
    /// Build-64 test seam: the installed WSS media connection state.
    var debugWSMediaConnectedForTest: Bool { wsMedia?.state == .connected }
    /// Test seam: whether a given socket is the installed conference host.
    func debugWSConferenceMediaIsInstalled(_ session: WebSocketCallMedia) -> Bool {
        wsConferenceMedia === session
    }
    /// Test seam: the installed conference host socket (nil when absent).
    func debugConferenceWSMedia() -> WebSocketCallMedia? { wsConferenceMedia }
    // Build-61 QUIC direct test seams (DEBUG-equivalent internal accessors).
    var debugQUICDirect: QUICDirectController? { quicDirect }
    var debugQUICAttemptRunning: Bool { quicAttemptTask != nil }
    /// Build-64 test seam: the policy-staged relay (nil when absent).
    var debugPolicyStagedRelay: WebSocketCallMedia? { policyStagedRelay }
    /// Build-64 test seam: the demoted-ws standby fence (nil when absent).
    var debugWSStandbyFence: (token: String, epoch: UInt64)? { wsStandbyFence }
    /// Build-65 test seam: the installed WSS media (nil when absent).
    var debugWSMediaForTest: WebSocketCallMedia? { wsMedia }
    // Build-66 network-path / media-stall test seams.
    /// The current client network generation (0 = no transition yet).
    var debugNetworkGeneration: UInt64 { networkGeneration }
    /// Whether the stall watchdog task is currently armed.
    var debugStallWatchdogActive: Bool { stallWatchdogTask != nil }
    /// Whether a stall episode already armed the recovery (one per episode).
    var debugStallArmed: Bool { stallArmed }
    /// Drive the pass the watchdog performs on each tick (deterministic).
    func debugRunStallCheckForTest(grace: TimeInterval? = nil) {
        guard !ended, let callId = activeGatewayId else { return }
        guard mediaRecovery?.isActive != true else { return }
        guard let reason = stalledCarrierEvidence(grace: grace ?? mediaStallGrace) else {
            stallArmed = false
            return
        }
        guard !stallArmed else { return }
        stallArmed = true
        DiagnosticsStore.shared.log("call",
            "carrier \(reason) media stalled (test tick); arming bounded recovery")
        scheduleMediaFailure(callId: callId, gen: generation,
            message: String(localized: "音频长时间未恢复，已结束通话。"))
    }
    /// Deliver one path event exactly as the observer would.
    func debugHandlePathEventForTest(_ event: CallPathEvent) {
        handleCallPathEvent(event)
    }
    /// Build-62: whether the QUIC authority has stood the legacy selector
    /// down (false after any attempt abort — legacy RTC/WSS routing resumes).
    var debugRouteAutomaticStandDown: Bool { route?.automaticStandDown == true }
    /// Test seam: drive the QUIC standby → primary promotion directly.
    func debugPromoteQUICDirect(proposalID: UInt64) async -> Bool {
        await promoteQUICDirect(proposalID: proposalID)
    }
    #endif

    /// Cancels any in-flight recovery (generation bump, teardown, explicit
    /// or remote end). Recovery never survives a new call lifecycle.
    private func cancelMediaRecovery() {
        mediaRecovery?.cancel()
        mediaRecovery = nil
        pendingRecoveryProof = nil
        mediaRecoveryTask?.cancel()
        mediaRecoveryTask = nil
    }

    private func startMonitorIfNeeded(callId: String, uuid: UUID, gen: UInt64) {
        guard monitorStarted == false else { return }
        monitorStarted = true
        monitorTask?.cancel()
        monitorTask = Task { [weak self] in
            var reportedConnected = false
            let deadline = Date().addingTimeInterval(45)
            while !Task.isCancelled, Date() < deadline {
                try? await Task.sleep(nanoseconds: 1_200_000_000)
                guard let self, gen == self.generation else { return }
                if let call = try? await self.api.fetchCall(id: callId) {
                    guard gen == self.generation else { return }
                    self.latestGateway = call
                    self.track(callId, record: call)
                    if call.endedAt != nil { self.handleRemoteEnd(reason: call.endReason); return }
                    if call.state == .active, self.latestMedia == .connected, !reportedConnected {
                        reportedConnected = true
                        self.callKit.reportConnected(uuid: uuid, startedAt: call.connectedDate)
                    }
                    self.publishPhase()
                }
            }
            guard let self else { return }
            if !reportedConnected, gen == self.generation {
                // Never claim success without a real media+gateway connection.
                await self.failActiveCall(message: String(localized: "未能在限定时间内接通音频。"))
            }
        }
    }

    // MARK: End / reset

    /// Ends exactly one gateway call (active, held or ringing extra).
    private func endSpecificCall(_ gatewayId: String, reason: EndedCallReason) {
        if conference?.legs.contains(where: { $0.id == gatewayId }) == true {
            Task { [weak self] in await self?.performEndConferenceLeg(gatewayId) }
            return
        }
        let record = tracked[gatewayId]?.record ?? (gatewayId == activeGatewayId ? latestGateway : nil)
        let ringing = record?.state == .incomingRinging
        if gatewayId == activeGatewayId {
            finishLocalCall(gatewayId: gatewayId, reason: reason)
        } else {
            tracked.removeValue(forKey: gatewayId)
            delegate?.callGroupChanged()
        }
        knownUUIDs = knownUUIDs.filter { $0.value != gatewayId }
        Task { [weak self] in
            guard let self else { return }
            await self.registry.remove(gatewayId: gatewayId)
            do {
                if ringing {
                    try await self.api.reject(callId: gatewayId, idempotencyKey: UUID().uuidString)
                } else {
                    try await self.api.hangup(callId: gatewayId, idempotencyKey: UUID().uuidString)
                }
            } catch { AppLog.call.notice("end command failed at gateway") }
        }
    }

    private func handleRemoteEnd(reason: String?) {
        guard let gatewayId = activeGatewayId else { return }
        // A busy-class end earns a bounded busy burst; keep the media
        // session alive briefly so it stays audible through the shared
        // engine (CallKit may still cut it on deactivate — graceful).
        let holdForBusyTone = CallProgressToneController.isBusyClassEndReason(reason)
            && (wsMedia?.isGraphRunning == true || wsConferenceMedia?.isGraphRunning == true)
        progressTone.callEnded(reason: reason)
        finishLocalCall(gatewayId: gatewayId, reason: .remoteEnded,
                        deferMediaClose: holdForBusyTone)
        Task {
            if let uuid = await registry.uuid(for: gatewayId) {
                await callKit.reportEnded(uuid: uuid, reason: .remoteEnded)
            }
            await registry.remove(gatewayId: gatewayId)
        }
    }

    func externalEnd(gatewayId: String) async {
        if gatewayId == activeGatewayId {
            handleRemoteEnd(reason: nil)
            return
        }
        if conference?.legs.contains(where: { $0.id == gatewayId }) == true {
            await refreshConference(noteLegEnded: gatewayId, assumeDissolvedOnFailure: true)
            return
        }
        guard tracked[gatewayId] != nil else { return }
        await removeTrackedCall(gatewayId, reason: .remoteEnded)
    }

    private func removeTrackedCall(_ gatewayId: String, reason: EndedCallReason) async {
        tracked.removeValue(forKey: gatewayId)
        knownUUIDs = knownUUIDs.filter { $0.value != gatewayId }
        delegate?.callGroupChanged()
        if let uuid = await registry.uuid(for: gatewayId) {
            switch reason {
            case .failed:
                await callKit.reportEnded(uuid: uuid, reason: .failed)
            default:
                await callKit.reportEnded(uuid: uuid, reason: .remoteEnded)
            }
        }
        await registry.remove(gatewayId: gatewayId)
    }

    private func markCallEndedLocally(_ callId: String, reason: CXCallEndedReason) async {
        if let uuid = await registry.uuid(for: callId) {
            await callKit.reportEnded(uuid: uuid, reason: reason)
        }
        await registry.remove(gatewayId: callId)
        knownUUIDs = knownUUIDs.filter { $0.value != callId }
    }

    private func refreshConference(noteLegEnded: String?, assumeDissolvedOnFailure: Bool) async {
        guard let snapshot = conference else { return }
        if let noteLegEnded {
            tracked.removeValue(forKey: noteLegEnded)
            await markCallEndedLocally(noteLegEnded, reason: .remoteEnded)
        }
        do {
            let updated = try await api.conference(id: snapshot.id)
            await applyConferenceSnapshot(updated)
        } catch {
            if assumeDissolvedOnFailure {
                await dissolveConference(remaining: [])
            }
        }
    }

    private func replaceConferenceLeg(_ call: CallRecord) {
        guard let current = conference,
              let index = current.legs.firstIndex(where: { $0.id == call.id }) else { return }
        var legs = current.legs
        legs[index] = call
        conference = updatedConference(current, legs: legs)
    }

    private func updatedConference(_ record: ConferenceRecord, legs: [CallRecord]) -> ConferenceRecord {
        var updated = ConferenceRecord(
            id: record.id, hostDeviceId: record.hostDeviceId, state: record.state,
            createdAt: record.createdAt, graceDeadline: record.graceDeadline, legs: legs
        )
        // A leg-set change must never drop the server-confirmed recording
        // state; only a fresh conference fetch may replace it.
        updated.recordingId = record.recordingId
        updated.recordingState = record.recordingState
        updated.recordingDurationMs = record.recordingDurationMs
        return updated
    }

    private func failActiveCall(message: String) async {
        guard let gatewayId = activeGatewayId else { return }
        publishFailed(message)
        finishLocalCall(gatewayId: gatewayId, reason: .failed)
        Task {
            if let uuid = await registry.uuid(for: gatewayId) {
                await callKit.reportEnded(uuid: uuid, reason: .failed)
            }
            await registry.remove(gatewayId: gatewayId)
            try? await api.hangup(callId: gatewayId, idempotencyKey: UUID().uuidString)
        }
    }

    func handleProviderReset() {
        let ids = Set(tracked.keys).union(activeGatewayId.map { Set([$0]) } ?? [])
        stopMediaStallWatchdog()
        stopPathObservation()
        conference = nil
        heldConferenceLegs.removeAll()
        if let gatewayId = activeGatewayId {
            finishLocalCall(gatewayId: gatewayId, reason: .failed)
        } else {
            _ = invalidateGeneration()
        }
        tracked.removeAll()
        activeGatewayId = nil
        latestGateway = nil
        latestMedia = .idle
        monitorStarted = false
        knownUUIDs.removeAll()
        AudioSessionBridge.shared.callEnded()
        publishAudioStatus(nil)
        delegate?.callGroupChanged()
        guard !ids.isEmpty else { return }
        Task { [weak self] in
            guard let self else { return }
            for id in ids {
                await self.registry.remove(gatewayId: id)
                try? await self.api.hangup(callId: id, idempotencyKey: UUID().uuidString)
            }
        }
    }

    /// Clear local ownership before any network or actor suspension. Delayed
    /// responses can never restart audio after a hangup or clear a newer call.
    /// `deferMediaClose` keeps the retired WSS session alive for the bounded
    /// busy-tone hold (it is detached from routing and closed afterwards).
    private func finishLocalCall(gatewayId: String, reason: EndedCallReason,
                                 deferMediaClose: Bool = false) {
        _ = invalidateGeneration(deferMediaClose: deferMediaClose)
        activeGatewayId = nil
        latestGateway = nil
        latestMedia = .idle
        ended = true
        tracked.removeValue(forKey: gatewayId)
        knownUUIDs = knownUUIDs.filter { $0.value != gatewayId }
        // No live call left: fence any pending audio recovery and clear the
        // honest status so a late interruption/media-reset completion can
        // never resurrect audio (or a notice) for the ended call.
        releaseAudioOwnershipIfIdle()
        delegate?.callDidEnd(gatewayId: gatewayId, reason: reason)
        delegate?.callGroupChanged()
    }

    /// The coordinator owns no live call any more: fence pending audio
    /// recovery. Called on every end/reset path that leaves no live call.
    private func releaseAudioOwnershipIfIdle() {
        guard !hasLiveCall, conference == nil else { return }
        AudioSessionBridge.shared.callEnded()
        publishAudioStatus(nil)
    }

    // MARK: Helpers

    private func track(_ id: String, record: CallRecord?, held: Bool? = nil) {
        var entry = tracked[id] ?? TrackedCall(record: record, held: held ?? false, muted: false)
        if let record { entry.record = record }
        if let held { entry.held = held }
        tracked[id] = entry
    }

    private func beginCall() {
        generation += 1
        ended = false
        monitorStarted = false
        monitorTask?.cancel()
        mediaTask?.cancel()
        self.cancelMediaRecovery()
        // A new call kills any lingering progress tone (ringback or the
        // bounded busy burst from a previous call).
        progressTone.stopAll()
        // Explicit live-call audio demand: bounded recovery is armed for THIS
        // call and released when the last call ends.
        AudioSessionBridge.shared.callStarted()
        // Fresh call, fresh status: the bridge's interrupted flag reflects the
        // real current session state (another app may still hold audio).
        audioInterrupted = AudioSessionBridge.shared.isInterrupted
        publishAudioStatus(nil)
        // NOTE: the voice-chat session is NOT reconfigured here. `beginCall`
        // runs on the incoming-push path BEFORE the system report; a
        // synchronous `setCategory` there delays/blocks the report and runs
        // audio work on the system callback's main thread. The session is
        // configured where audio actually starts (outgoing dial start and
        // `activate` for answers), and `AudioSessionBridge.didActivate`
        // normalizes once more before any engine starts.
    }

    @discardableResult
    private func bumpGeneration() -> UInt64 {
        generation += 1
        return generation
    }

    private func invalidateGeneration(deferMediaClose: Bool = false) -> UInt64 {
        generation += 1
        monitorTask?.cancel()
        mediaTask?.cancel()
        self.cancelMediaRecovery()
        stopMediaStallWatchdog()
        stopPathObservation()
        media?.close()
        media = nil
        let closing = wsMedia
        wsMedia = nil
        if deferMediaClose, let closing {
            holdForBusyToneBurst(closing)
        } else {
            closing?.close()
        }
        stagedPreviousRelay?.closeWithoutAudio()
        stagedPreviousRelay = nil
        closeQUICDirect()
        teardownHotStandby()
        route?.teardown()
        route = nil
        discardPendingPreflight()
        // The call is gone: idle relay measurement may resume (the probe's
        // eligible gate re-checks no-live-call/foreground state, so this is a
        // no-op when another call is still tracked or the app is background).
        idleRelayProbe?.appDidEnterForeground()
        monitorStarted = false
        return generation
    }

    /// Keeps the just-retired WSS session alive for the bounded busy-tone
    /// hold so the burst stays audible through the shared engine, then
    /// closes it. The session is routing-detached; `close()` is idempotent,
    /// and a newer call's media is a different object entirely.
    private func holdForBusyToneBurst(_ session: WebSocketCallMedia) {
        busyToneSession = session
        Task { @MainActor in
            try? await Task.sleep(nanoseconds: UInt64(1.2 * 1_000_000_000))
            session.close()
            if self.busyToneSession === session { self.busyToneSession = nil }
        }
    }

    private var lifecycleTask: Task<Void, Never>?
    private func lifecycleRun(_ operation: @escaping @MainActor () async -> Void) {
        lifecycleTask = Task { @MainActor in
            await operation()
        }
    }

    private var lastLoggedPhase: ActiveCallPhase?

    private func publishPhase() {
        guard let gatewayId = activeGatewayId else { return }
        let phase = CallPhaseResolver.resolve(gateway: latestGateway, media: latestMedia)
        progressTone.update(
            phase: phase,
            isOutgoing: activeCallIsOutgoing,
            routeSwitching: route?.routeState.switching ?? false)
        if phase != lastLoggedPhase {
            lastLoggedPhase = phase
            DiagnosticsStore.shared.log(
                "call", "phase \(Self.phaseName(phase)) call=\(AppLog.tag(gatewayId))")
        }
        delegate?.call(gatewayId, phaseChanged: phase)
    }

    private static func phaseName(_ phase: ActiveCallPhase) -> String {
        switch phase {
        case .none: return "none"
        case .incomingRinging: return "incomingRinging"
        case .outgoingDialing: return "outgoingDialing"
        case .connecting: return "connecting"
        case .active: return "active"
        case .held: return "held"
        case .reconnecting: return "reconnecting"
        case .ending: return "ending"
        case .ended: return "ended"
        case .failed: return "failed"
        }
    }

    private func publishFailed(_ message: String) {
        guard let gatewayId = activeGatewayId else { return }
        delegate?.call(gatewayId, phaseChanged: .failed(message: message))
    }

    private func friendly(_ error: Error) -> String {
        (error as? APIError)?.friendlyMessage ?? "通话失败。"
    }

    private func gatewayId(for uuid: UUID) async -> String? {
        if let known = knownUUIDs[uuid] { return known }
        if let mapped = await registry.gatewayId(for: uuid) {
            knownUUIDs[uuid] = mapped
            return mapped
        }
        if let active = activeGatewayId, active == uuid.uuidString.lowercased() { return active }
        return nil
    }

    func setSpeakerphone(_ enabled: Bool) {
        speaker = enabled
        if let media {
            do { try media.setSpeakerphone(enabled) }
            catch { AppLog.call.notice("speaker route change failed") }
        }
        if let wsMedia {
            do { try wsMedia.setSpeakerphone(enabled) }
            catch { AppLog.call.notice("speaker route change failed") }
        }
        // Warm direct-first (build 38): the active transport is an adopted
        // direct probe with no WSS/ICE session object on the coordinator.
        route?.setDirectSpeakerphone(enabled)
    }
}

// MARK: - CallKit director

extension CallCoordinator: CallDirecting {
    func startOutgoing(peer: String, uuid: UUID) {
        startOutgoing(peer: peer, lineId: defaultLineId, uuid: uuid)
    }

    func endCall(uuid: UUID, reason: EndedCallReason) {
        if let direct = knownUUIDs[uuid]
            ?? (activeGatewayId == uuid.uuidString.lowercased() ? activeGatewayId : nil) {
            endSpecificCall(direct, reason: reason)
            return
        }
        Task { [weak self] in
            guard let self, let gatewayId = await self.registry.gatewayId(for: uuid) else { return }
            self.endSpecificCall(gatewayId, reason: reason)
        }
    }

    func setMuted(uuid: UUID, muted: Bool) {
        Task { [weak self] in
            guard let self else { return }
            var gatewayId = self.knownUUIDs[uuid]
                ?? (self.activeGatewayId == uuid.uuidString.lowercased() ? self.activeGatewayId : nil)
            if gatewayId == nil { gatewayId = await self.registry.gatewayId(for: uuid) }
            guard let gatewayId else { return }
            if var entry = self.tracked[gatewayId] {
                entry.muted = muted
                self.tracked[gatewayId] = entry
            }
            if gatewayId == self.activeGatewayId {
                self.muted = muted
                self.media?.setMicMuted(muted)
                self.wsMedia?.setMicMuted(muted)
                self.quicDirect?.quicCallMedia?.setMicMuted(muted)
                self.route?.setMuted(muted)
            }
        }
    }

    func playDTMF(uuid: UUID, digit: String) {
        Task { [weak self] in
            guard let self else { return }
            var gatewayId = self.knownUUIDs[uuid]
                ?? (self.activeGatewayId == uuid.uuidString.lowercased() ? self.activeGatewayId : nil)
            if gatewayId == nil { gatewayId = await self.registry.gatewayId(for: uuid) }
            guard let gatewayId else { return }
            if let conference = self.conference,
               conference.legs.contains(where: { $0.id == gatewayId }) {
                try? await self.api.conferenceLegDTMF(
                    conferenceId: conference.id, callId: gatewayId, digit: digit,
                    idempotencyKey: UUID().uuidString
                )
            } else {
                try? await self.api.dtmf(
                    callId: gatewayId, digit: digit, idempotencyKey: UUID().uuidString
                )
            }
        }
    }

    func setHeld(uuid: UUID, held: Bool) async throws {
        guard let gatewayId = await gatewayId(for: uuid) else {
            throw APIError.notReady("未知通话。")
        }
        try await setHeld(callId: gatewayId, held: held)
    }

    func setGroup(uuid: UUID, groupUUID: UUID?) async throws {
        if groupUUID == nil {
            try await ungroup(uuid: uuid)
        } else {
            try await mergeHeldCallsAsync()
        }
    }
}
