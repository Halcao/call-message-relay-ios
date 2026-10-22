import Foundation
import AVFoundation

/// WSS audio transport: 20 ms binary frames over the authorized WebSocket —
/// the media path reachable on cellular networks where the gateway's ICE
/// candidates are LAN-only. Build 27 negotiates Opus (RFC 7587, inband FEC)
/// with PCMU as the wire-compatible fallback (the attach URL carries
/// `codec=opus`; the gateway's `ready` control announces the negotiated
/// codec; old gateways answer without the field and the socket stays PCMU).
/// It honors the same AVAudioSession/CallKit activation contract as
/// ``WebRTCCallMedia``.
///
/// Wire protocol (mirrors the worker's internal media socket):
///   server -> client: {"type":"ready","codec":"opus"?} text, then binary
///   client -> server: binary frames, plus {"type":"ping","buf":n,"gap":ms}
///   server -> client: {"type":"pong","t":n,"buf":n,"ugap":ms}
/// Each binary message is one 20 ms frame: exactly 160 bytes for PCMU, a
/// variable-length Opus payload otherwise. The ping carries the app's
/// playback-buffer depth and worst downlink inter-arrival gap (the freshest
/// delay evidence the TCP path produces); the pong answers with the
/// gateway's uplink evidence and both sides' Opus encoders adapt from it
/// (bounded, hold-on-stale). Outbound frames go through ONE serial writer
/// with a bounded, drop-stale queue so a stalled tunnel can never
/// accumulate latency; every awaited socket operation is cancelable and
/// fenced by a generation counter.
@MainActor
final class WebSocketCallMedia: NSObject {
    var onState: ((MediaState) -> Void)?
    var onQuality: ((MediaQuality) -> Void)?
    /// Fires on EVERY genuinely decoded inbound network frame (never for
    /// Opus PLC/FEC concealment slots). Build-48 bounded recovery clears
    /// only on this fresh-media proof, not on a socket `.connected` state.
    /// Assigned on the main actor; the callback itself is dispatched back to
    /// the main actor because frames arrive on a detached receive context.
    var onInboundRealFrame: (() -> Void)? {
        get {
            inboundFrameLock.lock()
            defer { inboundFrameLock.unlock() }
            return inboundFrameCallback
        }
        set {
            inboundFrameLock.lock()
            inboundFrameCallback = newValue
            inboundFrameLock.unlock()
        }
    }
    private nonisolated(unsafe) var inboundFrameCallback: (() -> Void)?
    private let inboundFrameLock = NSLock()

    nonisolated private func emitInboundRealFrame() {
        inboundFrameLock.lock()
        let cb = inboundFrameCallback
        inboundFrameLock.unlock()
        guard let cb else { return }
        Task { @MainActor in
            // Build-62: the fresh-media proof timestamp (control/attach or
            // `.connected` alone never counts — only a genuinely decoded
            // inbound frame).
            self.lastInboundRealFrameAt = Date()
            cb()
        }
    }

    /// Build-67 media-starvation evidence: an Opus payload that failed to
    /// decode. Such a frame is NOT media liveness (no real-frame proof) and
    /// previously vanished without a trace, so a sustained decode-failure
    /// run was indistinguishable from "no frames on the wire". Bounded:
    /// counter plus ONE log line per socket.
    private let decodeFailureLock = NSLock()
    private nonisolated(unsafe) var loggedDecodeFailure = false
    nonisolated private func noteOpusDecodeFailure() {
        DiagnosticsCensus.shared.increment("audio.wsOpusDecodeFailed")
        decodeFailureLock.lock()
        let first = !loggedDecodeFailure
        loggedDecodeFailure = true
        decodeFailureLock.unlock()
        guard first else { return }
        Task { @MainActor in
            DiagnosticsStore.shared.log("audio", "wss opus decode failed (media frame dropped)")
        }
    }
    /// When the LAST genuinely decoded inbound frame arrived (nil before
    /// the first). Build-62 route-policy evidence.
    private(set) var lastInboundRealFrameAt: Date?
    /// True when a genuine inbound frame arrived within `window` — the
    /// bidirectional media proof for route selection (silence simply leaves
    /// the last stamp aging; it is never a failure signal).
    func isInboundRealFrameFresh(within window: TimeInterval = 5, now: Date = Date()) -> Bool {
        guard let at = lastInboundRealFrameAt else { return false }
        return now.timeIntervalSince(at) <= window
    }

    /// Audio graph seam, so a start failure is injectable deterministically
    /// (a simulator host may or may not start AVAudioEngine — tests must not
    /// accept either result).
    protocol WSAudioGraphing: AnyObject {
        var onMicFrame: (([Int16]) -> Void)? { get set }
        var isRunning: Bool { get }
        @discardableResult
        func startIfNeeded() -> Bool
        func stop()
        func setMicMuted(_ muted: Bool)
        func pushPlayback(_ frame: [Int16])
        /// Locally generated call-progress tone frame. The default routes to
        /// `pushPlayback` so lightweight fakes keep working; production
        /// overrides it to keep the level census honest.
        func pushSyntheticPlayback(_ frame: [Int16])
        /// Real-downlink silence age (ms); `.max` when never played.
        var playbackIdleMilliseconds: Int { get }
        /// Wire-compatible playback-buffer depth in 20 ms frames (0 when
        /// unknown): the QUEUED frame count, the value the deployed gateway
        /// controller consumes from the ping `buf` field. Semantics are
        /// frozen for remote compatibility.
        var playbackBufferedFrames: Int { get }
        /// TOTAL local playout depth in 20 ms frames: queued plus buffers
        /// already handed to the player. Used for UI telemetry and the
        /// additive ping `depth` field; never replaces `buf`.
        var playbackTotalBufferedFrames: Int { get }
        /// OSStatus-style code of the most recent `startIfNeeded` failure
        /// (privacy-safe numeric; nil when the last start succeeded or no
        /// start ran). Lightweight fakes default to nil.
        var lastStartFailureCode: Int? { get }
        /// Live audio-input readiness for the exclusive promotion
        /// handover (build-51): false while the retiring direct ADM's
        /// asynchronous teardown still holds the input (a start now would
        /// fail with WSAudioGraph code=1). Fakes default to always-ready.
        func isInputReadyForStart() -> Bool
        /// Route-change revalidation: re-check the live input format and
        /// rebuild the capture pipeline when a route change altered it. The
        /// production graph implements this; lightweight fakes default to a
        /// no-op via the protocol extension.
        func revalidateRoute()
    }

    /// Test seam over URLSessionWebSocketTask.
    protocol MediaSocket: AnyObject {
        func resume()
        func cancel()
        func send(_ message: URLSessionWebSocketTask.Message, completionHandler: @escaping (Error?) -> Void)
        func receive(completionHandler: @escaping (Result<URLSessionWebSocketTask.Message, Error>) -> Void)
    }

    /// The factory also receives the owning URLSession so tests can share
    /// the production wiring while keeping a handle for invalidation.
    typealias SocketFactory = (URLRequest, URLSession) -> MediaSocket

    final class URLSessionMediaSocket: MediaSocket {
        let task: URLSessionWebSocketTask
        init(_ task: URLSessionWebSocketTask) { self.task = task }
        func resume() { task.resume() }
        func cancel() { task.cancel(with: .goingAway, reason: nil) }
        func send(_ message: URLSessionWebSocketTask.Message, completionHandler: @escaping (Error?) -> Void) {
            task.send(message, completionHandler: completionHandler)
        }
        func receive(completionHandler: @escaping (Result<URLSessionWebSocketTask.Message, Error>) -> Void) {
            task.receive(completionHandler: completionHandler)
        }
    }

    private let makeSocket: SocketFactory
    private var session: URLSession?
    private var socket: MediaSocket?
    private var receiveGeneration: UInt64 = 0
    /// Nonisolated mirror of `connected`/`receiveGeneration` for the
    /// detached network loops: unstructured `Task` blocks created on the
    /// main actor would otherwise inherit the main executor and re-attach
    /// every loop iteration to the main queue (defeating the off-main
    /// cadence entirely).
    private let connection = ConnectionFence()
    private var currentState: MediaState = .idle {
        didSet {
            onState?(currentState)
            quality.phase = currentState
            onQuality?(quality)
        }
    }
    private var quality = MediaQuality()
    private var selfManagedAudioActive = false
    /// False while this socket is a STAGED handover attach: it may complete
    /// its handshake but must not open a second capture graph until
    /// `activateAudio()` promotes it (exactly one mic owner at all times).
    private var audioAllowed = true
    /// While true a graph-start failure is REPORTED (log/counter) but never
    /// fails the socket: the exclusive staged-promotion window owns the
    /// failure decision (truthful rollback vs real call end). The
    /// coordinator sets it for the promotion and clears it when the
    /// promotion settles; the PRIMARY transport keeps the fatal semantics
    /// (a primary graph that cannot start must fail the media truthfully).
    var graphStartFailureNonfatal = false
    private var connected = false

    /// The CURRENT transport state (idle/connected/disconnected/failed/
    /// closed). Unlike the one-shot `onState` callback, this value can be
    /// read after a handover so a caller that just installed this session
    /// can re-synchronize its own media phase instead of inheriting a stale
    /// `.disconnected` left by a superseded socket (build-38 false
    /// "音频中断" report while relay audio was actually healthy).
    var state: MediaState { currentState }

    /// Re-emits the current quality snapshot to `onQuality`. A staged relay
    /// reaches `.connected` during its handshake BEFORE it becomes the
    /// installed session, so that first emission is dropped by the
    /// coordinator's identity fence; the exclusive promotion calls this so
    /// the UI converges on the healthy relay instead of holding a stale
    /// interruption banner from the retired transport.
    func republishCurrentQuality() {
        onQuality?(quality)
    }

    #if DEBUG
    func stateForTest() -> MediaState { currentState }
    /// Test seam: emit the fresh-media proof as if a real inbound frame had
    /// been decoded on this socket.
    func fireInboundRealFrameForTest() { emitInboundRealFrame() }
    #endif

    // MARK: Send queue (single writer, bounded, drop-stale)
    //
    // The gate is lock-owned: mic frames are produced on the audio feed
    // queue (never the main queue), so the capture path crosses into the
    // single-writer socket here without a main-actor hop. The drain task
    // parks on the gate until a frame or a detach arrives.
    private let outbound = OutboundFrameGate()
    private var sendDrainTask: Task<Void, Never>?
    private let sendTimeout: TimeInterval = 10
    // MARK: Ping sampling

    /// Freshness-aware samples: RTT plus when the pong arrived. The route
    /// advisor compares only samples inside its own freshness window.
    struct PingSample: Equatable { let rtt: TimeInterval; let at: Date }
    private(set) var pingSampleLog: [PingSample] = []
    /// RTT values (seconds) of the 120 most recent pongs.
    var pingSamples: [TimeInterval] { pingSampleLog.map(\.rtt) }
    /// RTT samples fresher than `window`, oldest first.
    func freshPingSamples(within window: TimeInterval, now: Date = Date()) -> [TimeInterval] {
        pingSampleLog.filter { now.timeIntervalSince($0.at) <= window }.map(\.rtt)
    }
    /// Most recent ping round-trip with its arrival date (nil before the
    /// first pong). Lets callers apply an explicit freshness deadline and
    /// clear a stale displayed value instead of indefinitely holding it.
    var lastPingSample: PingSample? { pingSampleLog.last }
    /// LOCAL playback-buffer delay in seconds from the audio graph's own
    /// depth (20 ms frames): queued plus scheduled-ahead. Zero-cost read of
    /// an existing counter. This is a local-buffering number, NOT a
    /// mouth-to-ear or network measurement, and its value is UI telemetry —
    /// the wire `buf` contract stays queue-only.
    var playbackBufferSeconds: Double { Double(audioIO.playbackTotalBufferedFrames) * 0.02 }
    /// Last gateway pong host-buffer evidence with arrival date (relay
    /// telemetry only; nil until a pong arrives).
    private var lastGatewayBuffer: (frames: Int, at: Date)?
    var gatewayBufferSeconds: Double? {
        guard let last = lastGatewayBuffer, Date().timeIntervalSince(last.at) <= 8 else { return nil }
        return Double(last.frames) * 0.02
    }
    private var pingTimer: Timer?
    private var pingSequence: UInt64 = 0
    private var pendingPings: [UInt64: Date] = [:]

    private let audioIO: WSAudioGraphing

    private let handshakeTimeout: TimeInterval

    // MARK: WSS codec (build 27: Opus with PCMU fallback)
    //
    // The negotiated codec is fixed by the gateway's ready control before
    // the audio graph starts (connect() returns first; activateAudio() is
    // the handover), so the mic callback never races a codec swap. The
    // state is lock-owned: the receive and mic-feed paths are nonisolated
    // (they must never hop to the main executor per packet).
    private let wssCodec = WSSCodecState()

    init(socketFactory: SocketFactory? = nil,
         audioGraph: WSAudioGraphing? = nil,
         handshakeTimeout: TimeInterval = 15) {
        self.handshakeTimeout = handshakeTimeout
        self.makeSocket = socketFactory ?? { request, session in
            URLSessionMediaSocket(session.webSocketTask(with: request))
        }
        self.audioIO = audioGraph ?? WSAudioGraph()
        super.init()
        audioIO.onMicFrame = { [weak self] frame in
            guard let self, let encoded = self.encodeOutbound(frame) else { return }
            self.outbound.enqueue(encoded)
        }
    }

    /// Encode one outbound frame in the negotiated codec. A transient Opus
    /// failure drops the frame (returns nil): the gateway decodes per the
    /// negotiated codec, so no substitute format is possible mid-stream, and
    /// a dropped frame is honestly concealed by the gateway's PLC.
    private nonisolated func encodeOutbound(_ frame: [Int16]) -> Data? {
        wssCodec.lock.lock()
        let encoder = wssCodec.encoder
        let opus = wssCodec.usesOpus
        let framed = wssCodec.framed
        let seq = wssCodec.seqOut
        wssCodec.seqOut &+= 1
        wssCodec.lock.unlock()
        if opus, let encoder {
            do {
                let payload = try encoder.encode(frame)
                return framed ? WSSFrameCodec.frame(seq: seq, payload: payload) : Data(payload)
            } catch {
                DiagnosticsCensus.shared.increment("audio.wsOpusEncodeFailed")
                // The seq was consumed above: the receiver sees the hole and
                // conceals this slot — dropping silently without a seq gap
                // would misalign its decoder timeline.
                return nil
            }
        }
        return PCMUCodec.encode(frame)
    }

    // MARK: Test seams
    #if DEBUG
    convenience init(shortTimeoutForTest: TimeInterval, socketFactory: @escaping SocketFactory) {
        self.init(socketFactory: socketFactory, handshakeTimeout: shortTimeoutForTest)
    }

    func handleForTest(_ message: URLSessionWebSocketTask.Message) {
        // Tests assert synchronously after calling this seam, so control
        // frames must be handled inline (the production receive path hops
        // to the main actor asynchronously).
        if case .string(let text) = message {
            handleControl(text, arrivedAt: Date())
        } else {
            handle(message)
        }
    }

    func handleForTest(_ message: URLSessionWebSocketTask.Message, arrivedAt: Date) {
        // Arrival-stamp seam: lets tests pin the receive-context stamp and
        // prove the pong RTT/freshness anchor to it rather than to the
        // main-actor handling time.
        if case .string(let text) = message {
            handleControl(text, arrivedAt: arrivedAt)
        } else {
            handle(message)
        }
    }

    var usesOpusForTest: Bool {
        wssCodec.lock.lock(); defer { wssCodec.lock.unlock() }
        return wssCodec.usesOpus
    }
    var framedForTest: Bool {
        wssCodec.lock.lock(); defer { wssCodec.lock.unlock() }
        return wssCodec.framed
    }
    var uplinkBitrateForTest: Int {
        wssCodec.lock.lock(); defer { wssCodec.lock.unlock() }
        return wssCodec.controller.bitrate
    }
    var uplinkFECForTest: Bool {
        wssCodec.lock.lock(); defer { wssCodec.lock.unlock() }
        return wssCodec.controller.fecEnabled
    }

    func audioActivatedForTest(_ session: AVAudioSession) {
        audioActivated(with: session)
    }

    func sendPingForTest() { sendPing() }

    func connectedForTest() -> Bool { connected }
    #endif

    /// Closes a socket whose handshake failed BEFORE it took audio
    /// ownership: retires the network task without touching the audio graph
    /// or deactivating another transport's session.
    func closeWithoutAudio() {
        receiveGeneration &+= 1
        connected = false
        connection.set(connected: false, generation: receiveGeneration)
        pingTimer?.invalidate()
        pingTimer = nil
        outbound.detach()
        sendDrainTask?.cancel()
        sendDrainTask = nil
        socket?.cancel()
        socket = nil
        session?.invalidateAndCancel()
        session = nil
        currentState = .closed
    }

    /// Retires THIS socket after the gateway atomically replaced it (a route
    /// commit or a WSS re-attach): stops ping/drain/tasks without deactivating
    /// the system-owned audio session and without publishing a failure state.
    func retireAfterHandover() {
        receiveGeneration &+= 1
        connected = false
        connection.set(connected: false, generation: receiveGeneration)
        pingTimer?.invalidate()
        pingTimer = nil
        pendingPings.removeAll()
        outbound.detach()
        sendDrainTask?.cancel()
        sendDrainTask = nil
        socket?.cancel()
        socket = nil
        session?.invalidateAndCancel()
        session = nil
        audioIO.stop()
    }

    /// Build-50 hot-standby demotion (relay side): stops the capture/playback
    /// graph — the single audio owner moves to the promoted transport —
    /// while KEEPING the authenticated socket alive for standby freshness,
    /// ping RTT and the bounded silence pulse. Never closes the connection;
    /// call `markHotStandby()` to arm the standby role first/after.
    func suspendAudioForStandbyHandover() {
        audioIO.stop()
    }

    // MARK: Connection

    /// Connects and blocks until the gateway's `ready` control arrives.
    /// Throws on any upgrade/auth failure or timeout (the socket is cancelled
    /// so the parked receive continuation always finishes) — the caller
    /// fails the call truthfully instead of falling back to an unreachable
    /// ICE path. Generation-fenced: a close/replace during the handshake
    /// cannot resurrect this socket.
    func connect(request: URLRequest) async throws {
        receiveGeneration &+= 1
        let generation = receiveGeneration
        let config = URLSessionConfiguration.ephemeral
        config.httpCookieStorage = nil
        config.timeoutIntervalForRequest = 30
        let urlSession = URLSession(configuration: config)
        session = urlSession
        // Offer Opus on the attach URL; a build-26 gateway ignores the
        // parameter and answers PCMU, so this is safe against old servers.
        var request = request
        if let url = request.url, var components = URLComponents(url: url, resolvingAgainstBaseURL: false) {
            var items = components.queryItems ?? []
            if !items.contains(where: { $0.name == "codec" }) {
                items.append(URLQueryItem(name: "codec", value: "opus"))
            }
            components.queryItems = items
            request.url = components.url
        }
        let socket = makeSocket(request, urlSession)
        self.socket = socket
        socket.resume()

        let ready: Bool
        do {
            ready = try await withSocketTimeout(seconds: handshakeTimeout, socket: socket) {
                try await self.awaitReady(using: socket)
            }
        } catch {
            if generation == receiveGeneration {
                self.socket = nil
                session = nil
                urlSession.invalidateAndCancel()
            }
            throw error
        }
        guard generation == receiveGeneration else {
            socket.cancel()
            urlSession.invalidateAndCancel()
            throw CancellationError()
        }
        guard ready else {
            socket.cancel()
            urlSession.invalidateAndCancel()
            self.socket = nil
            session = nil
            throw MediaError.neverConnected
        }
        // The socket is READY but does not own the audio graph yet. The
        // caller enables it via activateAudio() at the exact handover moment,
        // so a staging attach never double-captures while another transport
        // still carries the call, and a failed attach leaves the previous
        // transport untouched. The receive loop is transport-level and
        // starts now (audio ownership is independent).
        connected = true
        currentState = .connected
        connection.set(connected: true, generation: receiveGeneration)
        outbound.attach(socket: socket, epoch: receiveGeneration)
        startSendDrain()
        startReceiveLoop()
    }

    /// Binds the audio graph (mic capture + playback) AFTER a ready
    /// handshake. Used by the route handover so the system audio session is
    /// owned by exactly one transport at a time.
    func activateAudio() {
        audioAllowed = true
        startAudio()
    }

    /// Marks a staged handover attach: the socket is READY but must not open
    /// a second capture graph while the previous transport still carries the
    /// call. Cleared at the exclusive promotion (`promoteAudioOwnership()`).
    func markAudioStaged(_ staged: Bool = true) {
        audioAllowed = !staged
        isStagedForHandover = staged
    }

    /// True while this session is a staged, unpromoted handover attach: its
    /// socket lifecycle is NOT the call's carrier and must never drive
    /// call-level media failure, route transitions or a healthy-relay
    /// publish (the exclusive promotion owns those).
    private(set) var isStagedForHandover = false

    /// Exclusive promotion: this session may now open the capture graph.
    func promoteAudioOwnership() {
        audioAllowed = true
        isStagedForHandover = false
        setHotStandby(false)
        stopStandbySilencePulse()
    }

    // MARK: Hot standby (build 50)

    /// Fencing captured from the ready greeting: the session's primary role
    /// epoch at attach time, and this socket's standby instance token when
    /// the gateway accepted the attach into the standby slot.
    private(set) var lastReadyEpoch: UInt64?
    private(set) var lastReadyStandbyToken: String?

    /// True while this socket is a persistent hot STANDBY transport: fully
    /// attached and authenticated, receiving the mirrored downlink mix
    /// (decoded for freshness, never rendered), sending at most a bounded
    /// synthetic-silence uplink pulse. Exactly one of {primary, standby}
    /// renders/owns audio at any instant — a standby NEVER opens a capture
    /// graph and its socket lifecycle must not drive call-level media
    /// failure (the `isStagedForHandover` fencing covers that).
    /// Lock-backed: the nonisolated receive path reads it per frame.
    var isHotStandby: Bool {
        standbyLock.lock()
        defer { standbyLock.unlock() }
        return standbyModeFlag
    }
    private nonisolated(unsafe) var standbyModeFlag = false
    private let standbyLock = NSLock()

    /// Fires on every GENUINE inbound frame while in standby mode (the
    /// hot-standby readiness proof — valid silent PCM counts; the mirrored
    /// downlink mix is real media). PLC/FEC-concealed slots never fire it.
    var onStandbyFreshMedia: (() -> Void)? {
        get {
            standbyLock.lock()
            defer { standbyLock.unlock() }
            return standbyFreshCallback
        }
        set {
            standbyLock.lock()
            defer { standbyLock.unlock() }
            standbyFreshCallback = newValue
        }
    }
    private nonisolated(unsafe) var standbyFreshCallback: (() -> Void)?

    nonisolated private func emitStandbyFresh() {
        standbyLock.lock()
        let cb = standbyFreshCallback
        standbyLock.unlock()
        guard let cb else { return }
        Task { @MainActor in cb() }
    }

    private func setHotStandby(_ value: Bool) {
        standbyLock.lock()
        standbyModeFlag = value
        standbyLock.unlock()
    }

    /// Arms the persistent standby role: no audio ownership, non-fatal
    /// lifecycle, freshness counting, and the bounded silence pulse that
    /// proves the uplink path without a second mic injection.
    func markHotStandby() {
        audioAllowed = false
        isStagedForHandover = true
        setHotStandby(true)
        startStandbySilencePulse()
    }

    /// Clears the standby role WITHOUT promoting audio ownership (the
    /// socket is being dropped or replaced). Promotion uses
    /// `promoteAudioOwnership()` instead.
    func clearHotStandby() {
        setHotStandby(false)
        stopStandbySilencePulse()
    }

    private var standbySilenceTimer: Timer?

    /// Bounded uplink readiness probe: one SILENT frame per second through
    /// this socket's own encoder — never from the mic, never audible, and
    /// cheap. The gateway decodes it for standby freshness and discards it
    /// (single-uplink invariant preserved server-side).
    private func startStandbySilencePulse() {
        stopStandbySilencePulse()
        standbySilenceTimer = Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { [weak self] _ in
            guard let self, self.isHotStandby, self.connected else { return }
            self.enqueueStandbySilenceFrame()
        }
        // Fire one immediately so a freshly-armed standby proves its uplink
        // within the first second.
        enqueueStandbySilenceFrame()
    }

    private func stopStandbySilencePulse() {
        standbySilenceTimer?.invalidate()
        standbySilenceTimer = nil
    }

    private func enqueueStandbySilenceFrame() {
        // Reuse the live outbound encode path: identical codec, seq16
        // framing and sequence space as real uplink (a silence slot is an
        // ordinary media slot; the gateway decodes it for standby freshness
        // and discards it — the single-uplink invariant holds server-side).
        guard let frame = encodeOutbound([Int16](repeating: 0, count: 160)) else { return }
        outbound.enqueue(frame)
    }

    /// Route-change revalidation: the real graph re-checks the live input
    /// format and rebuilds the capture pipeline when a route change altered
    /// it; no-op for graph fakes.
    func revalidateAudioRoute() {
        audioIO.revalidateRoute()
    }

    /// A receive/send continuation cannot be cancelled by task-group
    /// semantics alone: BOTH the deadline AND parent cancellation actively
    /// cancel the owned socket, so the parked continuation always resumes.
    /// Nonisolated: invoked from the send-drain task; touches only the
    /// (thread-safe) socket and value state.
    private nonisolated func withSocketTimeout<T>(
        seconds: TimeInterval, socket: MediaSocket,
        operation: @escaping () async throws -> T
    ) async throws -> T {
        try await withTaskCancellationHandler {
            try await withThrowingTaskGroup(of: T.self) { group in
                group.addTask { try await operation() }
                group.addTask {
                    try await Task.sleep(nanoseconds: UInt64(seconds * 1_000_000_000))
                    socket.cancel()
                    throw MediaError.neverConnected
                }
                let result = try await group.next()!
                group.cancelAll()
                return result
            }
        } onCancel: {
            socket.cancel()
        }
    }

    private func awaitReady(using socket: MediaSocket) async throws -> Bool {
        let generation = receiveGeneration
        while generation == receiveGeneration {
            let message = try await socket.receiveValue()
            switch message {
            case .string(let text):
                if let data = text.data(using: .utf8),
                   let control = try? JSONDecoder().decode(WSMediaControl.self, from: data),
                   control.type == "ready" {
                    // Build-61 (protocol §3): the QUIC standby greeting rides
                    // this authenticated control plane; it is never THIS
                    // socket's own media ready — surface it and keep waiting.
                    if control.transport == "quic" {
                        onTransportControl?(control)
                        continue
                    }
                    try adoptCodec(control.codec == "opus" ? .opus : .pcmu,
                                   framed: control.fmt == "seq16")
                    // Build-50 fencing: capture the attach-time role epoch
                    // and, for a standby accept, the instance token. A
                    // greeting WITHOUT `standby` on a standby-requested
                    // attach means an old server replaced the primary — the
                    // coordinator detects that via its primary's loss.
                    lastReadyEpoch = control.epoch
                    lastReadyStandbyToken = control.standby == true ? control.token : nil
                    return true
                }
            case .data:
                continue
            @unknown default:
                continue
            }
        }
        return false
    }

    private enum WSSCodec: String {
        case pcmu, opus
    }

    /// Create the codec machinery for the negotiated wire codec. The gateway
    /// decided from the attach URL, so when it announced Opus the socket IS
    /// Opus — a local codec-creation failure must fail the connection
    /// honestly (throw) rather than limp with a PCMU decoder on an Opus
    /// stream. `framed` (fmt=seq16) enables the gap-aware wire protocol.
    private func adoptCodec(_ codec: WSSCodec, framed: Bool) throws {
        wssCodec.lock.lock()
        defer { wssCodec.lock.unlock() }
        wssCodec.usesOpus = codec == .opus
        wssCodec.framed = framed && wssCodec.usesOpus
        wssCodec.seqOut = 0
        wssCodec.depacketizer = WSSFrameCodec.Depacketizer()
        guard wssCodec.usesOpus else { return }
        do {
            wssCodec.encoder = try OpusCodec.Encoder()
            wssCodec.decoder = try OpusCodec.Decoder()
            let negotiatedFmt = wssCodec.framed ? "seq16" : "bare"
            AppLog.media.debug("wss codec negotiated: opus fmt=\(negotiatedFmt)")
            DiagnosticsStore.shared.log("audio", "wss codec: opus fmt=\(negotiatedFmt)")
        } catch {
            wssCodec.usesOpus = false
            wssCodec.framed = false
            wssCodec.encoder = nil
            wssCodec.decoder = nil
            DiagnosticsStore.shared.log("audio", "wss opus unavailable: \(error.localizedDescription)")
            throw MediaError.audioActivationFailed
        }
    }

    private func startReceiveLoop() {
        let generation = receiveGeneration
        guard let socket else { return }
        let fence = connection
        // `Task.detached` is required: an unstructured Task created on the
        // main actor inherits its executor and would re-attach every
        // iteration to the main queue.
        Task.detached { [weak self] in
            while let self {
                let snapshot = fence.snapshot()
                guard snapshot.connected, snapshot.generation == generation else { return }
                do {
                    let message = try await socket.receiveValue()
                    // Post-await fence: a close/replacement while the receive
                    // was parked must not handle or fail the new session.
                    let post = fence.snapshot()
                    guard post.connected, post.generation == generation else { return }
                    self.handle(message)
                } catch {
                    guard fence.snapshot().generation == generation else { return }
                    Task { @MainActor in self.socketDidFail() }
                    return
                }
            }
        }
    }

    /// Nonisolated data path: binary frames are decoded and queued straight
    /// from the receive context (the scheduler is thread-safe and owns its
    /// serial queue), so a busy main thread can no longer backlog the
    /// downlink. Control frames hop back to the main actor.
    /// Standby receive gate: a hot-standby socket decodes genuine frames
    /// (keeping the codec warm and proving readiness) but NEVER feeds the
    /// player — exactly one playback owner. Returns true when the caller
    /// must skip the playback push.
    private nonisolated func standbyConsumes(_ pcm: [Int16]) -> Bool {
        standbyLock.lock()
        let standby = standbyModeFlag
        standbyLock.unlock()
        guard standby else { return false }
        emitStandbyFresh()
        return true
    }

    private nonisolated func handle(_ message: URLSessionWebSocketTask.Message) {
        switch message {
        case .data(let frame):
            wssCodec.recordDownlinkGap()
            wssCodec.lock.lock()
            let decoder = wssCodec.decoder
            let opus = wssCodec.usesOpus
            let framed = wssCodec.framed
            wssCodec.lock.unlock()
            if opus, let decoder {
                if framed {
                    guard let parsed = WSSFrameCodec.parse(frame) else { return }
                    let slots: [WSSFrameCodec.Slot]
                    wssCodec.lock.lock()
                    slots = wssCodec.depacketizer.arrivals(seq: parsed.seq, payload: parsed.payload)
                    wssCodec.lock.unlock()
                    for slot in slots {
                        switch slot {
                        case .decode(let payload):
                            do {
                                let pcm = try decoder.decode(payload)
                                self.emitInboundRealFrame()
                                if !self.standbyConsumes(pcm) {
                                    audioIO.pushPlayback(pcm)
                                }
                            } catch {
                                self.noteOpusDecodeFailure()
                            }
                        case .plc:
                            if let pcm = try? decoder.decode(nil) {
                                audioIO.pushPlayback(pcm)
                            }
                        case .fecRecover(let carrier):
                            // One-slot gap: recover the predecessor from the
                            // carrier's RFC 7587 inband FEC; the carrier is
                            // decoded exactly once by its own .decode slot.
                            // libopus degrades gracefully when no FEC data is
                            // embedded.
                            if let recovered = try? decoder.decodeFEC(carrier) {
                                audioIO.pushPlayback(recovered)
                            } else if let pcm = try? decoder.decode(nil) {
                                audioIO.pushPlayback(pcm)
                            }
                        }
                    }
                    return
                }
                // Bare Opus (build-26 gateway): no framing, no gap detection —
                // decode what arrives; sender drops shift the timeline
                // undetectably, which is exactly what the framed negotiation
                // fixes when both sides are build 27.
                guard !frame.isEmpty, frame.count <= OpusCodec.maxFrameBytes else { return }
                do {
                    let pcm = try decoder.decode(Array(frame))
                    self.emitInboundRealFrame()
                    if !standbyConsumes(pcm) {
                        audioIO.pushPlayback(pcm)
                    }
                } catch {
                    self.noteOpusDecodeFailure()
                }
            } else {
                guard frame.count == 160, let pcm = PCMUCodec.decode(frame) else { return }
                self.emitInboundRealFrame()
                if !standbyConsumes(pcm) {
                    audioIO.pushPlayback(pcm)
                }
            }
        case .string(let text):
            // Stamp the pong's arrival on the RECEIVE context, not inside the
            // main-actor control handler: the RTT must measure the network
            // path plus the gateway's turnaround, never the main-actor queue
            // delay behind a busy UI. These samples feed the call-start route
            // comparison (relay vs direct), the hot-standby promotion gate
            // and the in-call telemetry row, and a scheduling delay is pure
            // noise in every one of them.
            let arrived = Date()
            Task { @MainActor in self.handleControl(text, arrivedAt: arrived) }
        @unknown default:
            break
        }
    }

    @MainActor private func handleControl(_ text: String, arrivedAt: Date) {
        guard let data = text.data(using: .utf8),
              let control = try? JSONDecoder().decode(WSMediaControl.self, from: data) else { return }
        if control.type == "error" {
            socketDidFail()
        } else if control.type == "pong", let tag = control.t,
                  let sent = pendingPings.removeValue(forKey: tag) {
            // The RTT is computed against the RECEIVE-context arrival stamp
            // (passed in), never `Date()` here: main-actor scheduling delay
            // must not inflate the sample.
            let rtt = arrivedAt.timeIntervalSince(sent)
            // Measurement hygiene: only accept finite, non-negative pongs
            // that answer a ping from the last 30 s — a clock anomaly or a
            // stale replay must never enter the samples the UI/advisor use.
            if rtt.isFinite, rtt >= 0, rtt <= 30 {
                pingSampleLog.append(PingSample(rtt: rtt, at: arrivedAt))
                if pingSampleLog.count > 120 { pingSampleLog.removeFirst(pingSampleLog.count - 120) }
            }
            // Gateway-side host buffer depth (seconds) for the live telemetry
            // row; stale evidence is never rendered as current.
            if let hostBuf = control.buf {
                lastGatewayBuffer = (frames: max(0, hostBuf), at: arrivedAt)
            }
            // Uplink closed loop: the gateway answers with the depth of the
            // host buffer this socket feeds and the worst uplink gap it saw.
            // Fresh evidence drives bounded encoder adaptation; stale or
            // absent evidence holds (never raises into the unknown).
            wssCodec.lock.lock()
            let encoder = wssCodec.encoder
            let opus = wssCodec.usesOpus
            wssCodec.lock.unlock()
            if opus, let encoder {
                let now = Date()
                wssCodec.lock.lock()
                let settings = wssCodec.controller.adapt(
                    now: now,
                    hostBufFrames: control.buf ?? 0,
                    uplinkGapMs: control.ugap ?? 0
                )
                wssCodec.lock.unlock()
                if let settings {
                    try? encoder.setBitrate(settings.bitrate)
                    try? encoder.setInbandFEC(settings.fec)
                    // Inband FEC is only embedded when the encoder expects
                    // loss; drive the expectation from the same hysteresis.
                    try? encoder.setPacketLossPerc(settings.fec ? 10 : 0)
                }
            }
        } else if control.type == "promoted"
                    || (control.type == "ready" && control.transport == "quic") {
            // Build-61 QUIC control plane (protocol §3): the `promoted`
            // reply and the QUIC ready greeting are surfaced to the
            // coordinator, never consumed as this socket's own state.
            onTransportControl?(control)
        }
    }

    private var lastLoggedSocketState: MediaState?

    private func socketDidFail(toFailed: Bool = false) {
        guard connected else { return }
        connected = false
        connection.set(connected: false, generation: receiveGeneration)
        currentState = toFailed ? .failed : .disconnected
        if currentState != lastLoggedSocketState {
            lastLoggedSocketState = currentState
            DiagnosticsStore.shared.log("audio", "ws socket \(currentState == .failed ? "failed" : "disconnected")")
        }
    }

    // MARK: Send path

    private func startSendDrain() {
        sendDrainTask?.cancel()
        // See startReceiveLoop: detached so the drain never re-attaches to
        // the main executor between sends.
        sendDrainTask = Task.detached { [weak self] in
            guard let self else { return }
            while !Task.isCancelled {
                // Parks (cancellable) until a frame arrives or the gate
                // detaches (close/replace/hangup, including a detach that
                // ran before the park). A delivery carries its dequeue
                // epoch; a re-attach invalidates it before any write.
                guard let delivery = await self.outbound.awaitFrame() else { return }
                guard !Task.isCancelled else { return }
                let snapshot = self.outbound.snapshot()
                guard snapshot.accepting, snapshot.epoch == delivery.epoch,
                      let socket = snapshot.socket else { return }
                let sendStarted = ProcessInfo.processInfo.systemUptime
                do {
                    try await self.withSocketTimeout(seconds: self.sendTimeout, socket: socket) {
                        try await socket.sendValue(.data(delivery.frame))
                    }
                    // Send-path health: a stalled tunnel shows up here (and in
                    // uplinkGateDropped) long before the call feels it.
                    DiagnosticsCensus.shared.maximize(
                        "audio.uplinkSendMsMax",
                        Int((ProcessInfo.processInfo.systemUptime - sendStarted) * 1000))
                } catch {
                    // Only the CURRENT socket's failure may fail the session.
                    guard self.outbound.snapshot().epoch == snapshot.epoch else { return }
                    Task { @MainActor in self.socketDidFail() }
                    return
                }
            }
        }
    }

    // MARK: Denoise control (build 48)

    /// Build-61 QUIC control plane (protocol §3): control frames on this
    /// socket that are NOT this socket's own media handshake — the
    /// `ready transport=quic` standby greeting and the `promoted` reply to a
    /// standby-promote command. Surfaced verbatim to the coordinator; never
    /// treated as this socket's own ready, pong or failure.
    var onTransportControl: ((WSMediaControl) -> Void)?

    /// Build-50 fenced promotion request ON this standby socket: asks the
    /// gateway to promote THIS transport to the primary role (the previous
    /// primary is demoted into the standby slot; both stay attached). The
    /// token pins this exact standby instance, expectEpoch the client's view
    /// of the primary role, nonce gives idempotent retries. The gateway
    /// replies `{"type":"promoted",...}` on this same socket; an old gateway
    /// ignores the unknown control (the caller times out / reconciles).
    ///
    /// Build-61: an optional `transport` marker (e.g. `"quic"`) selects the
    /// standby transport to promote; nil keeps the exact legacy JSON.
    func sendPromoteCommand(token: String, expectEpoch: UInt64, nonce: String,
                            transport: String? = nil) {
        guard let socket, connected else { return }
        var json = "{\"type\":\"promote\""
        if let transport { json += ",\"transport\":\"\(transport)\"" }
        json += ",\"token\":\"\(token)\",\"expectEpoch\":\(expectEpoch),\"nonce\":\"\(nonce)\"}"
        socket.send(.string(json)) { _ in }
    }

    /// Sends the hot directional-denoise control to the gateway. An older
    /// gateway ignores the unknown message type; the next attach params and
    /// the preferences PATCH converge the policy regardless.
    func sendDenoiseControl(uplink: Bool?, downlink: Bool?) {
        guard let socket, connected else { return }
        var parts: [String] = [#""type":"denoise""#]
        if let uplink { parts.append(#""uplink":\#(uplink ? "true" : "false")"#) }
        if let downlink { parts.append(#""downlink":\#(downlink ? "true" : "false")"#) }
        let json = "{" + parts.joined(separator: ",") + "}"
        socket.send(.string(json)) { _ in }
    }

    // MARK: Ping sampling

    func startPingSampling() {
        guard pingTimer == nil else { return }
        // Sample immediately: the call-start route decision compares the
        // relay's MEASURED fresh RTT against the preflight candidate, and it
        // must not wait a full timer tick for the relay's first sample. The
        // socket is already READY at every call site; a ping is a small JSON
        // control frame and never touches audio.
        sendPing()
        let timer = Timer(timeInterval: 1.0, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.sendPing() }
        }
        RunLoop.main.add(timer, forMode: .common)
        pingTimer = timer
    }

    private func sendPing() {
        guard connected, let socket else { return }
        pingSequence &+= 1
        pendingPings[pingSequence] = Date()
        if pendingPings.count > 8 { pendingPings.removeAll() }
        let tag = pingSequence
        // Downlink evidence for the gateway's controller. CONTRACT: `buf`
        // stays QUEUED frames only — the deployed gateway's WSS bitrate/FEC
        // controller consumes it against fixed thresholds (wsAppBufHigh/Low),
        // so its semantics must not change silently. `depth` is an additive
        // field carrying the TOTAL local playout depth (queued + scheduled
        // ahead) for future gateway use; the current parser ignores unknown
        // fields, and `gap` remains the worst inter-arrival gap. Rotating
        // the gap window here (main actor) keeps the nonisolated receive
        // path lock-free.
        let bufFrames = audioIO.playbackBufferedFrames
        let totalFrames = audioIO.playbackTotalBufferedFrames
        let gap = wssCodec.consumeDownlinkGapForPing()
        let generation = receiveGeneration
        socket.send(.string("{\"type\":\"ping\",\"t\":\(tag),\"buf\":\(bufFrames),\"depth\":\(totalFrames),\"gap\":\(gap)}")) { [weak self] error in
            guard error != nil else { return }
            Task { @MainActor in
                guard let self, self.receiveGeneration == generation else { return }
                self.socketDidFail()
            }
        }
    }

    // MARK: Audio lifecycle (CallKit contract, same as WebRTCCallMedia)

    func audioActivated(with session: AVAudioSession) {
        // A real interruption owns the session right now: starting an engine
        // would race the competing app for the mic. The bridge does not
        // publish activation while interrupted; this guard is the last line
        // of defense for a replay racing the notification.
        if AudioSessionBridge.shared.isInterrupted {
            DiagnosticsCensus.shared.increment("audio.wsActivationWhileInterrupted")
            return
        }
        // Ownership is explicit: only a self-managed activation keeps this
        // session responsible for deactivating on close; a system activation
        // permanently transfers that responsibility to CallKit/LCK.
        selfManagedAudioActive = AudioSessionBridge.shared.currentOwnership == .selfManaged
        // A staged relay (handover in progress) must never open a second mic
        // while the previous transport still carries the call.
        guard audioAllowed else {
            DiagnosticsCensus.shared.increment("audio.wsStagedActivationSuppressed")
            return
        }
        // CallKit activates OUR app's session with the configuration the
        // system picked (build-16 field evidence: mode=Default/Speaker on
        // outgoing, VoiceChat/Receiver on incoming), and some combinations
        // leave the engine's render cycle dead — the graph starts, yet the
        // mic tap delivers zeros and player buffers never complete, so the
        // call is silent in BOTH directions. Normalizing category/options
        // to exactly what the proven self-managed path uses fixes the
        // mismatch before the engine starts. Ownership stays with CallKit:
        // only setCategory runs here, never setActive/deactivate.
        let normalized = AudioSessionBridge.normalizeForVoiceChat(session)
        if normalized {
            // Only log the rare path where a change was actually applied:
            // startIfNeeded() then follows a category change, which is not
            // the normal (pre-configured) call path anymore.
            DiagnosticsStore.shared.log("audio", "ws session normalized at engine start (changed=yes)")
        }
        guard audioIO.startIfNeeded() else {
            // Never claim audio that cannot run. Inside the staged-promotion
            // window the caller owns the failure decision (rollback vs call
            // end) — the socket must not be failed underneath it.
            DiagnosticsStore.shared.log("audio",
                "ws system activation failed: graph start error"
                + Self.failureCodeSuffix(audioIO.lastStartFailureCode))
            if graphStartFailureNonfatal {
                DiagnosticsCensus.shared.increment("audio.wsActivationFailureNonfatal")
                return
            }
            socketDidFail(toFailed: true)
            return
        }
        DiagnosticsStore.shared.log("audio", "ws audio activated (system)")
        AppLog.media.debug("ws audio activated")
    }

    /// Owned graph start that REPORTS failure to the caller instead of
    /// failing the socket: used by the exclusive handover promotion and by
    /// the self-managed activation, where the caller decides between a
    /// truthful rollback to the still-live previous transport and a real
    /// call failure. A staged graph that cannot start must never kill a
    /// healthy carrier nor be published as a healthy relay (build-43
    /// regression: "ws system activation failed: graph start error" →
    /// socket failed → call ended while the direct transport was alive).
    @discardableResult
    func startOwnedAudioGraph(with session: AVAudioSession) -> Bool {
        if AudioSessionBridge.shared.isInterrupted {
            DiagnosticsCensus.shared.increment("audio.wsActivationWhileInterrupted")
            return false
        }
        selfManagedAudioActive = AudioSessionBridge.shared.currentOwnership == .selfManaged
        guard audioAllowed else {
            DiagnosticsCensus.shared.increment("audio.wsStagedActivationSuppressed")
            return false
        }
        let normalized = AudioSessionBridge.normalizeForVoiceChat(session)
        if normalized {
            DiagnosticsStore.shared.log("audio", "ws session normalized at engine start (changed=yes)")
        }
        guard audioIO.startIfNeeded() else {
            DiagnosticsCensus.shared.increment("audio.wsOwnedGraphStartFail")
            DiagnosticsStore.shared.log("audio",
                "ws owned graph start failed"
                + Self.failureCodeSuffix(audioIO.lastStartFailureCode))
            return false
        }
        DiagnosticsStore.shared.log("audio", "ws audio activated (owned)")
        AppLog.media.debug("ws audio activated (owned)")
        return true
    }

    /// Live audio-input readiness for the exclusive promotion handover
    /// (build-51): the staged relay graph may only start once the retiring
    /// direct ADM's asynchronous teardown has released the input — the
    /// start's hard guard is a usable input format (WSAudioGraph code=1
    /// otherwise). Poll-safe: reads only the session-derived input format.
    func isAudioInputReady() -> Bool { audioIO.isInputReadyForStart() }

    /// Privacy-safe failure-code suffix for diagnostics (numeric only).
    private static func failureCodeSuffix(_ code: Int?) -> String {
        " code=\(code.map(String.init) ?? "unknown")"
    }

    func audioDeactivated(with session: AVAudioSession) {
        audioIO.stop()
    }

    @discardableResult
    func activateAudioWithoutCallKit() -> Bool {
        if selfManagedAudioActive { return true }
        if let active = AudioSessionBridge.shared.activeSession {
            // Failure-reporting start (never socket-fails here): the caller
            // owns the failure decision and gets the truthful Bool.
            return startOwnedAudioGraph(with: active)
        }
        guard let session = AudioSessionBridge.shared.activateSelfManaged() else {
            DiagnosticsStore.shared.log("audio", "ws direct-answer activation error")
            return false
        }
        selfManagedAudioActive = true
        if audioAllowed, !audioIO.startIfNeeded() {
            // The session was activated but the graph cannot run: release the
            // ownership we just claimed instead of pretending audio works.
            AudioSessionBridge.shared.registerSelfManagedDeactivation()
            selfManagedAudioActive = false
            DiagnosticsStore.shared.log("audio",
                "ws direct-answer activation failed: graph start error"
                + Self.failureCodeSuffix(audioIO.lastStartFailureCode))
            return false
        }
        _ = session
        DiagnosticsStore.shared.log("audio", "ws audio activated (direct answer)")
        return true
    }

    func deactivateAudioWithoutCallKit() {
        guard selfManagedAudioActive else { return }
        selfManagedAudioActive = false
        audioIO.stop()
        AudioSessionBridge.shared.deactivateSelfManaged()
    }

    private func startAudio() {
        // A system call may have activated the session before the socket was
        // ready; otherwise a direct answer self-activates before connecting.
        // Audio activation failure fails the media session truthfully.
        if AudioSessionBridge.shared.isInterrupted { return }
        if AudioSessionBridge.shared.activeSession != nil || selfManagedAudioActive {
            guard audioAllowed else { return }
            guard audioIO.startIfNeeded() else {
                socketDidFail(toFailed: true)
                return
            }
        }
    }

    func setMicMuted(_ muted: Bool) {
        audioIO.setMicMuted(muted)
    }

    // MARK: Call progress tone

    /// Whether the capture/playback graph is running (the progress tone can
    /// only fill silence through the shared engine).
    var isGraphRunning: Bool { audioIO.isRunning }

    /// Real-downlink silence age in milliseconds.
    var playbackIdleMilliseconds: Int { audioIO.playbackIdleMilliseconds }

    /// Queues one locally generated 160-sample progress-tone frame into the
    /// SAME playback scheduler as network audio (shared route/output, no
    /// second session), suppressible by any real downlink frame.
    func pushSyntheticTone(_ frame: [Int16]) {
        audioIO.pushSyntheticPlayback(frame)
    }

    func setSpeakerphone(_ enabled: Bool) throws {
        let session = AVAudioSession.sharedInstance()
        try session.overrideOutputAudioPort(enabled ? .speaker : .none)
    }

    /// Full teardown: fences late callbacks, cancels timers and tasks,
    /// drains the send queue, cancels the socket and invalidates the owning
    /// URLSession so no delegate or continuation can outlive the session.
    func close() {
        receiveGeneration &+= 1
        connected = false
        connection.set(connected: false, generation: receiveGeneration)
        pingTimer?.invalidate()
        pingTimer = nil
        pendingPings.removeAll()
        let gateDrops = outbound.droppedFrames
        outbound.detach()
        sendDrainTask?.cancel()
        sendDrainTask = nil
        socket?.cancel()
        socket = nil
        session?.invalidateAndCancel()
        session = nil
        wssCodec.lock.lock()
        wssCodec.encoder?.close()
        wssCodec.decoder?.close()
        wssCodec.encoder = nil
        wssCodec.decoder = nil
        wssCodec.usesOpus = false
        wssCodec.framed = false
        wssCodec.lock.unlock()
        audioIO.stop()
        deactivateAudioWithoutCallKit()
        currentState = .closed
        if gateDrops > 0 {
            DiagnosticsStore.shared.log("audio",
                "ws uplink gate dropped=\(gateDrops) (send path fell behind realtime)")
        }
    }
}

/// Lock-owned outbound frame gate: mic frames are produced on the audio
/// feed queue (never the main queue), so the capture path crosses into the
/// single-writer socket here without a main-actor hop. The drain task parks
/// on the gate until a frame arrives or `detach` closes it. Bounded and
/// drop-stale so a stalled tunnel can never accumulate uplink latency.
///
/// Delivery is exact-once: a parked consumer receives the frame directly
/// (the frame is NOT also queued), and a dequeued frame carries the gate
/// epoch at dequeue so the consumer can verify the socket is still current
/// before writing. A detach always unparks the consumer with nil, including
/// a detach that ran BEFORE the consumer parked.
/// Nonisolated connection fence mirroring the main-actor `connected` flag
/// and receive generation for the detached network loops. Updated at the
/// exact points where the main-actor state changes.
private final class ConnectionFence: @unchecked Sendable {
    private let lock = NSLock()
    private var connected = false
    private var generation: UInt64 = 0

    struct Snapshot: Equatable {
        let connected: Bool
        let generation: UInt64
    }

    func set(connected: Bool, generation: UInt64) {
        lock.lock()
        self.connected = connected
        self.generation = generation
        lock.unlock()
    }

    func snapshot() -> Snapshot {
        lock.lock()
        defer { lock.unlock() }
        return Snapshot(connected: connected, generation: generation)
    }
}

private final class OutboundFrameGate: @unchecked Sendable {
    /// One dequeued frame plus the gate epoch at dequeue; the consumer only
    /// writes it to a socket whose epoch still matches.
    struct Delivery {
        let frame: Data
        let epoch: UInt64
    }

    private let lock = NSLock()
    private var queue: [Data] = []
    private let maxQueue = 8
    private var waiter: CheckedContinuation<Delivery?, Never>?
    private var socket: WebSocketCallMedia.MediaSocket?
    private var epoch: UInt64 = 0
    private var accepting = false
    /// STEADY-STATE UPLINK DISCRIMINATOR: frames dropped because the socket
    /// send path fell behind realtime (>8 frames queued). If the per-run
    /// level census is healthy (mic frames flowing, few silent) yet the far
    /// end hears gaps together with a rising drop count, the loss sits in the
    /// transport, not the capture. Bounded counter only.
    private(set) var droppedFrames = 0

    struct Snapshot {
        let socket: WebSocketCallMedia.MediaSocket?
        let epoch: UInt64
        let accepting: Bool
    }

    func attach(socket: WebSocketCallMedia.MediaSocket, epoch: UInt64) {
        lock.lock()
        self.socket = socket
        self.epoch = epoch
        accepting = true
        lock.unlock()
    }

    /// Idempotent close: unblocks the parked drain with nil, drops queued
    /// frames and invalidates the socket reference. Later enqueues are
    /// ignored, so a late mic frame after hangup can never resurrect sends.
    func detach() {
        lock.lock()
        accepting = false
        epoch &+= 1
        queue.removeAll()
        socket = nil
        let pending = waiter
        waiter = nil
        lock.unlock()
        pending?.resume(returning: nil)
    }

    /// Exact-once handoff: when a consumer is parked it receives THIS frame
    /// directly (nothing is appended to the queue); otherwise the frame is
    /// queued for a future consumer.
    func enqueue(_ frame: Data) {
        lock.lock()
        guard accepting else { lock.unlock(); return }
        if let pending = waiter {
            waiter = nil
            let delivery = Delivery(frame: frame, epoch: epoch)
            lock.unlock()
            pending.resume(returning: delivery)
            return
        }
        if queue.count >= maxQueue {
            queue.removeFirst() // drop-stale: oldest realtime audio first
            droppedFrames += 1
            DiagnosticsCensus.shared.increment("audio.uplinkGateDropped")
        }
        queue.append(frame)
        lock.unlock()
    }

    /// Parks until a frame is available (returned with the dequeue epoch)
    /// or the gate detaches (returns nil — including a detach that already
    /// ran before this call). The parked continuation is transferred under
    /// the lock so a concurrent enqueue/detach can never lose or
    /// double-resume it.
    func awaitFrame() async -> Delivery? {
        lock.lock()
        if !accepting {
            lock.unlock()
            return nil
        }
        if let frame = queue.first {
            queue.removeFirst()
            let epoch = epoch
            lock.unlock()
            return Delivery(frame: frame, epoch: epoch)
        }
        return await withCheckedContinuation { continuation in
            waiter = continuation
            // An enqueue/detach may have raced us between the empty check
            // and parking: deliver/exit immediately instead of parking.
            if !accepting {
                waiter = nil
                lock.unlock()
                continuation.resume(returning: nil)
                return
            }
            if let queued = queue.first {
                queue.removeFirst()
                let epoch = epoch
                waiter = nil
                lock.unlock()
                continuation.resume(returning: Delivery(frame: queued, epoch: epoch))
                return
            }
            lock.unlock()
        }
    }

    func snapshot() -> Snapshot {
        lock.lock()
        defer { lock.unlock() }
        return Snapshot(socket: socket, epoch: epoch, accepting: accepting)
    }
}

/// Lock-owned WSS codec state: the receive loop and the mic feed are
/// nonisolated (they must never hop to the main executor per packet), and
/// the negotiated codec is chosen once at ready time before the graph
/// starts, so a plain NSLock box is sufficient and cheaper than an actor.
final class WSSCodecState: @unchecked Sendable {
    let lock = NSLock()
    var usesOpus = false
    /// Framed Opus (fmt=seq16 announced in the ready control). Build-26
    /// gateways speak bare Opus: the app still decodes (no gap detection —
    /// honestly bounded), so mixed-version pairs keep working.
    var framed = false
    /// Outbound media-slot sequence: incremented per 20 ms capture tick even
    /// when the frame is dropped (encode failure or bounded-queue stale
    /// drop), so the receiver sees the hole and conceals it.
    var seqOut: UInt16 = 0
    var depacketizer = WSSFrameCodec.Depacketizer()
    var encoder: OpusCodec.Encoder?
    var decoder: OpusCodec.Decoder?
    var controller = WSSBitrateController()
    var lastFrameArrival: Date?
    var worstDownlinkGapMs = 0

    // MARK: Receiver-side downlink quality (build-62)
    //
    // The seq16 envelope makes DOWNLINK loss/reorder MEASURABLE at the
    // receiver — the lack of transport ACKs only hides OUTBOUND delivery.
    // Counters advance on actual arrivals only: silence never fabricates
    // loss. Reset together with the depacketizer on codec/epoch changes.
    private(set) var downlinkSeeded = false
    private(set) var downlinkReceived: UInt64 = 0
    private(set) var downlinkLost: UInt64 = 0
    private(set) var downlinkFECAssisted: UInt64 = 0
    private(set) var downlinkLateOrDuplicate: UInt64 = 0

    func resetDownlinkQuality() {
        downlinkSeeded = false
        downlinkReceived = 0
        downlinkLost = 0
        downlinkFECAssisted = 0
        downlinkLateOrDuplicate = 0
    }

    /// Record one depacketizer work list. Callers hold `lock`.
    func recordDownlinkSlots(_ slots: [WSSFrameCodec.Slot]) {
        if !slots.isEmpty { downlinkSeeded = true }
        for slot in slots {
            switch slot {
            case .decode:
                downlinkReceived &+= 1
            case .plc:
                downlinkLost &+= 1
            case .fecRecover:
                downlinkReceived &+= 1
                downlinkFECAssisted &+= 1
            }
        }
    }

    /// A seeded stream that produced NO work list for an arrival carried a
    /// duplicate/late (reordered-behind) frame. Callers hold `lock`.
    func recordDownlinkLateOrDuplicate() {
        guard downlinkSeeded else { return }
        downlinkLateOrDuplicate &+= 1
    }

    struct DownlinkQualitySnapshot: Equatable, Sendable {
        /// True once the framed seq16 stream produced measurable arrivals.
        let measured: Bool
        let receivedFrames: UInt64
        let lostFrames: UInt64
        let fecAssistedFrames: UInt64
        let lateOrDuplicateFrames: UInt64
        var lossFraction: Double {
            let total = receivedFrames + lostFrames
            return total == 0 ? 0 : Double(lostFrames) / Double(total)
        }
        var reorderFraction: Double {
            let arrivals = receivedFrames + lateOrDuplicateFrames
            return arrivals == 0 ? 0 : Double(lateOrDuplicateFrames) / Double(arrivals)
        }
    }

    /// Callers hold `lock`.
    func downlinkQualitySnapshot() -> DownlinkQualitySnapshot {
        DownlinkQualitySnapshot(
            measured: downlinkSeeded && (downlinkReceived + downlinkLost) > 0,
            receivedFrames: downlinkReceived,
            lostFrames: downlinkLost,
            fecAssistedFrames: downlinkFECAssisted,
            lateOrDuplicateFrames: downlinkLateOrDuplicate)
    }

    /// Downlink inter-arrival evidence for the ping: worst gap observed
    /// since the last read (sendPing rotates the window on the main actor).
    func recordDownlinkGap() {
        lock.lock()
        defer { lock.unlock() }
        let now = Date()
        if let last = lastFrameArrival {
            let gap = Int(now.timeIntervalSince(last) * 1000)
            if gap > worstDownlinkGapMs { worstDownlinkGapMs = gap }
        }
        lastFrameArrival = now
    }

    func consumeDownlinkGapForPing() -> Int {
        lock.lock()
        defer { lock.unlock() }
        let gap = worstDownlinkGapMs
        worstDownlinkGapMs = 0
        return gap
    }
}

struct WSMediaControl: Decodable {
    let type: String
    /// Echoed ping tag (pong only).
    let t: UInt64?
    /// Negotiated codec announced in the ready control ("opus" when the
    /// gateway accepted codec=opus; absent = PCMU fallback).
    let codec: String?
    /// Wire framing for Opus ("seq16" = sequence-stamped media slots,
    /// enabling receiver-side gap detection/PLC; absent = bare Opus as
    /// shipped in build 26).
    let fmt: String?
    /// Gateway evidence in the pong: depth of the host buffer this socket
    /// feeds (frames) and the worst uplink inter-arrival gap (ms).
    let buf: Int?
    let ugap: Int?
    /// Build-50 hot standby: true when the gateway accepted this attach into
    /// the standby slot (the primary transport was NOT evicted). Absent on
    /// primary attaches and older gateways.
    let standby: Bool?
    /// Build-50 fencing: the session's primary role epoch at attach time.
    let epoch: UInt64?
    /// Build-50 fencing: this standby instance's identity token (promotion
    /// commands must pin it exactly).
    let token: String?
    /// Build-61 QUIC control plane (protocol `docs/quic61/protocol.md` §3):
    /// present on the `ready` greeting when it announces a NON-ws transport
    /// (`"quic"`) — never THIS socket's own media ready.
    let transport: String?
    /// Build-61 fencing: echoed on the `promoted` reply so the promoter can
    /// fence duplicates/replays of the standby-promote command.
    let nonce: String?

    // The `promoted` reply names the post-swap roles as TRANSPORT STRINGS
    // (`"primary":"quic","standby":"ws"`), while the `ready` greeting uses
    // `standby` as a BOOL. One key, two shapes: decode tolerantly.
    let primaryKind: String?
    let standbyKind: String?

    enum CodingKeys: String, CodingKey {
        case type, t, codec, fmt, buf, ugap, epoch, token, transport, nonce
        case standby, primary
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        type = try c.decode(String.self, forKey: .type)
        t = try c.decodeIfPresent(UInt64.self, forKey: .t)
        codec = try c.decodeIfPresent(String.self, forKey: .codec)
        fmt = try c.decodeIfPresent(String.self, forKey: .fmt)
        buf = try c.decodeIfPresent(Int.self, forKey: .buf)
        ugap = try c.decodeIfPresent(Int.self, forKey: .ugap)
        epoch = try c.decodeIfPresent(UInt64.self, forKey: .epoch)
        token = try c.decodeIfPresent(String.self, forKey: .token)
        transport = try c.decodeIfPresent(String.self, forKey: .transport)
        nonce = try c.decodeIfPresent(String.self, forKey: .nonce)
        if let flag = try? c.decodeIfPresent(Bool.self, forKey: .standby) {
            standby = flag
            standbyKind = nil
        } else {
            standby = nil
            standbyKind = try? c.decodeIfPresent(String.self, forKey: .standby) ?? nil
        }
        primaryKind = try? c.decodeIfPresent(String.self, forKey: .primary) ?? nil
    }
}

// Default implementations keep lightweight test fakes conforming without
// churn; production overrides them (the real graph keeps the level census
// honest for synthetic frames and reports true playback silence age).
extension WebSocketCallMedia.WSAudioGraphing {
    func pushSyntheticPlayback(_ frame: [Int16]) { pushPlayback(frame) }
    var playbackIdleMilliseconds: Int { .max }
    var playbackBufferedFrames: Int { 0 }
    /// Lightweight fakes have no separate scheduled-ahead depth.
    var playbackTotalBufferedFrames: Int { playbackBufferedFrames }
    /// Route-change revalidation hook: the real graph re-checks the live
    /// input format and rebuilds the capture pipeline when the route changed
    /// it; lightweight fakes do nothing.
    func revalidateRoute() {}
    /// Lightweight fakes report no failure detail.
    var lastStartFailureCode: Int? { nil }
    /// Lightweight fakes default to READY; tests override to script the
    /// ADM-teardown window (build-51 promotion readiness gate).
    func isInputReadyForStart() -> Bool { true }
}

extension WebSocketCallMedia.MediaSocket {
    func receiveValue() async throws -> URLSessionWebSocketTask.Message {
        try await withCheckedThrowingContinuation { continuation in
            receive { result in
                continuation.resume(with: result)
            }
        }
    }

    fileprivate func sendValue(_ message: URLSessionWebSocketTask.Message) async throws {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
            send(message) { error in
                if let error { continuation.resume(throwing: error) } else { continuation.resume() }
            }
        }
    }
}

// MARK: - μ-law codec (pure, testable)

enum PCMUCodec {
    private static let segmentEndpoints: [Int] = [0, 256, 512, 1024, 2048, 4096, 8192, 16384]

    /// 160 int16 samples -> 160 μ-law bytes. Clips to G.711 range; the
    /// gateway uses the identical algorithm so both directions agree.
    static func encode(_ frame: [Int16]) -> Data {
        var out = Data(count: frame.count)
        for (index, sample) in frame.enumerated() {
            var sign = UInt8(0)
            var value = Int(sample)
            if value < 0 { sign = 0x80; value = -value }
            if value > 32635 { value = 32635 }
            value += 132
            var segment = 0
            while segment < segmentEndpoints.count - 1 && value >= segmentEndpoints[segment + 1] {
                segment += 1
            }
            let mantissa = (value >> (segment + 3)) & 0x0F
            out[index] = ~(sign | UInt8(segment << 4) | UInt8(mantissa))
        }
        return out
    }

    /// 160 μ-law bytes -> 160 int16 samples. Any byte pattern decodes
    /// (G.711 is total), so a corrupted frame yields noise, never a crash.
    static func decode(_ data: Data) -> [Int16]? {
        guard data.count == 160 else { return nil }
        var out = [Int16](repeating: 0, count: data.count)
        for (index, byte) in data.enumerated() {
            let value = ~byte
            let sign = value & 0x80
            let segment = Int((value >> 4) & 0x07)
            let mantissa = Int(value & 0x0F)
            let sample = ((mantissa << 3) + 132) << segment
            out[index] = sign != 0 ? Int16(132 - sample) : Int16(sample - 132)
        }
        return out
    }
}
