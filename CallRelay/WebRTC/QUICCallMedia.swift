import Foundation
import AVFoundation
import Network

/// QUIC DATAGRAM audio transport (build 61 FINAL): the direct media path to
/// the gateway negotiated in `gateway/docs/quic61/protocol.md`. It reuses
/// the WSS path's exact media machinery — OpusCodec (RFC 7587 inband FEC),
/// WSSFrameCodec seq16 slot framing, WSPlaybackScheduler (via WSAudioGraph),
/// the arrival-clock RTT sampling discipline and the build-50 hot-standby
/// role model — so a QUIC socket is just another `hostSocket` candidate for
/// the route coordinator.
///
/// Session model (protocol §0, decided by native interop evidence): ONE
/// QUIC datagram session carries audio ONLY. ALL critical control (bind
/// authorization, ready, promote, denoise, bye, ping/pong) travels on the
/// EXISTING authenticated WSS plane: the coordinator correlates the two by
/// call + device + epoch — the WSS `ready transport=quic` greeting supplies
/// the codec/fmt/epoch that `applyTransportReady` binds here, and promotion
/// goes through the WSS standby-promote command verbatim.
///
/// The audio-ownership contract intentionally mirrors
/// ``WebSocketCallMedia`` one-for-one (staging/promotion, CallKit
/// activation, hot standby, retirement fences) so the coordinator can treat
/// both transports uniformly.
///
/// Lifecycle fences (identical discipline to the WSS socket):
/// * after `close()` every socket callback is ignored (generation fence);
/// * a probe echo answering an unknown tag is ignored;
/// * an audio datagram arriving before the WSS greeting's epoch is bound is
///   dropped by the socket's epoch gate (protocol §4);
/// * probe arrival is stamped on the RECEIVE context by the socket and the
///   RTT/freshness anchor use that stamp verbatim (build-60 lesson).
@MainActor
final class QUICCallMedia: NSObject {
    var onState: ((MediaState) -> Void)?
    var onQuality: ((MediaQuality) -> Void)?
    /// Fires on EVERY genuinely decoded inbound network frame (never for
    /// Opus PLC/FEC concealment slots). Build-48 bounded recovery clears
    /// only on this fresh-media proof, not on a socket `.ready` state.
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
    private nonisolated(unsafe) var inboundFrameProven = false
    /// Build-66 media-health anchor: WHEN the last genuine decoded frame
    /// arrived. Recorded under the same lock as the proof flag so the
    /// detached receive path and the main-actor stall check never race.
    private nonisolated(unsafe) var inboundFrameAt: Date?
    private let inboundFrameLock = NSLock()

    /// Test seam: whether a genuine epoch-valid frame has been decoded
    /// (the fresh-media proof the route policy requires).
    func mediaProvenForTest() -> Bool {
        hasProvenInboundMedia
    }

    /// True once ANY genuine decoded frame has arrived (production media
    /// health; a connected-but-never-proven socket is not stalled — it is
    /// simply not proven yet).
    var hasProvenInboundMedia: Bool {
        inboundFrameLock.lock()
        defer { inboundFrameLock.unlock() }
        return inboundFrameProven
    }

    /// Arrival date of the most recent genuine decoded frame (nil before the
    /// first). Production media-health evidence for the build-66 stall
    /// watchdog.
    var lastInboundRealFrameAt: Date? {
        inboundFrameLock.lock()
        defer { inboundFrameLock.unlock() }
        return inboundFrameAt
    }

    /// Whether genuine inbound media arrived within `window` (mirror of the
    /// WSS transport's contract). Healthy content silence still counts:
    /// the gateway emits a downlink frame every 20 ms, silence included.
    func isInboundRealFrameFresh(within window: TimeInterval, now: Date = Date()) -> Bool {
        guard let at = lastInboundRealFrameAt else { return false }
        return now.timeIntervalSince(at) <= window
    }

    nonisolated private func emitInboundRealFrame() {
        inboundFrameLock.lock()
        inboundFrameProven = true
        inboundFrameAt = Date()
        let callback = inboundFrameCallback
        inboundFrameLock.unlock()
        guard let callback else { return }
        Task { @MainActor in callback() }
    }

    /// Connection fencing values for one connect attempt.
    struct QUICSocketEndpoint: Equatable, Sendable {
        let pin: QUICPin
        let gateway: HostPort
        let localEndpoint: NWEndpoint?
    }

    typealias SocketFactory = (QUICSocketEndpoint) -> QUICMediaSocket

    /// The negotiated wire codec, fixed by the WSS ready greeting before
    /// the audio graph can start (the QUIC path reuses the WSS machinery).
    private enum CodecChoice: String {
        case pcmu, opus
    }

    private let makeSocket: SocketFactory
    private let socketBox = SocketBox()
    private var receiveGeneration: UInt64 = 0
    /// Nonisolated mirror of `connected`/`receiveGeneration` for the
    /// detached datagram receive context (same discipline as
    /// `WebSocketCallMedia.ConnectionFence`).
    private let connection = QUICConnectionFence()
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
    /// `promoteAudioOwnership()` promotes it (exactly one mic owner).
    private var audioAllowed = true
    /// While true a graph-start failure is REPORTED but never fails the
    /// socket (the exclusive staged-promotion window owns the decision).
    var graphStartFailureNonfatal = false
    private var connected = false
    /// Continuation of the in-flight phase-1 handshake wait. Main-actor
    /// private: socket state events funnel through `socketStateChanged` on
    /// the main actor and the timeout hops to it, so resumption is
    /// race-free. One-shot: `resumeReady` extracts before resuming.
    private var readyContinuation: CheckedContinuation<Void, Error>?
    /// True between the start of phase 1 and its settlement: a socket
    /// failure in this window that finds no parked continuation is stashed
    /// here so the next connect attempt fails fast instead of hanging.
    private var awaitingReady = false
    private var readyEarlyError: Error?
    /// Phase-2 bind-proof awaiter: resumed from the socket's RECEIVE context
    /// (the first epoch-valid audio frame) as well as from timeout/state
    /// paths, so it lives in a lock box, not main-actor state.
    private let bindAwaiter = BindAwaiter()

    /// The CURRENT transport state (idle/connected/disconnected/failed/
    /// closed); same re-synchronization contract as WebSocketCallMedia.
    var state: MediaState { currentState }

    /// Re-emits the current quality snapshot (exclusive-promotion parity
    /// with the WSS transport).
    func republishCurrentQuality() {
        onQuality?(quality)
    }

    #if DEBUG
    func stateForTest() -> MediaState { currentState }
    /// Test seam: probe-pipeline diagnostics (build-65).
    var debugProbeDiagnosticsForTest: (sent: Int, pending: Int, log: Int, timerArmed: Bool) {
        (DiagnosticsCensus.shared.snapshot()["quic.probeSent"] ?? 0,
         pendingProbes.count,
         probeSampleLog.count,
         probeTimer != nil)
    }
    /// Test seam: emit the fresh-media proof as if a real inbound frame had
    /// been decoded on this socket.
    func fireInboundRealFrameForTest() { emitInboundRealFrame() }
    #endif

    // MARK: Probe sampling

    /// Freshness-aware samples: RTT plus when the echo arrived.
    struct ProbeSample: Equatable { let rtt: TimeInterval; let at: Date }
    private(set) var probeSampleLog: [ProbeSample] = []
    /// RTT values (seconds) of the 120 most recent probe echoes.
    var probeSamples: [TimeInterval] { probeSampleLog.map(\.rtt) }
    /// RTT samples fresher than `window`, oldest first.
    func freshProbeSamples(within window: TimeInterval, now: Date = Date()) -> [TimeInterval] {
        probeSampleLog.filter { now.timeIntervalSince($0.at) <= window }.map(\.rtt)
    }
    /// Most recent probe round-trip with its arrival date (nil before the
    /// first echo).
    var lastProbeSample: ProbeSample? { probeSampleLog.last }
    /// LOCAL playback-buffer delay in seconds (UI telemetry only).
    var playbackBufferSeconds: Double { Double(audioIO.playbackTotalBufferedFrames) * 0.02 }

    /// Build-62: REAL receiver-side downlink quality measured from the
    /// seq16 envelope (gaps = lost frames, no-work arrivals = late/
    /// duplicate). Unknown only before the first framed arrival or for
    /// unframed codecs — never a fabricated zero.
    var downlinkQuality: WSSCodecState.DownlinkQualitySnapshot {
        wssCodec.lock.lock()
        defer { wssCodec.lock.unlock() }
        return wssCodec.downlinkQualitySnapshot()
    }
    private var probeTimer: Timer?
    private var pendingProbes: [UInt64: Date] = [:]

    private let audioIO: WebSocketCallMedia.WSAudioGraphing
    var handshakeTimeout: TimeInterval { handshakeTimeoutValue }
    /// Policy seam: the owning controller may widen the handshake deadline
    /// up to its overall attempt budget (never narrows it).
    func setHandshakeTimeoutForPolicy(_ value: TimeInterval) {
        handshakeTimeoutValue = value
    }
    private var handshakeTimeoutValue: TimeInterval
    private let bindTimeout: TimeInterval

    // MARK: Codec state (reuse of the WSS machinery)

    // Lock-owned like the WSS sibling: the datagram receive path and the
    // mic feed are nonisolated and must never hop to the main executor per
    // packet. The negotiated codec is fixed by the WSS ready greeting
    // before the audio graph starts.
    private let wssCodec = WSSCodecState()

    init(socketFactory: SocketFactory? = nil,
         audioGraph: WebSocketCallMedia.WSAudioGraphing? = nil,
         handshakeTimeout: TimeInterval = 15,
         bindTimeout: TimeInterval = 8) {
        self.handshakeTimeoutValue = handshakeTimeout
        self.bindTimeout = bindTimeout
        self.makeSocket = socketFactory ?? { endpoint in
            NWQUICMediaSocket(
                pin: endpoint.pin,
                host: endpoint.gateway.host,
                port: endpoint.gateway.port,
                localEndpoint: endpoint.localEndpoint
            )
        }
        self.audioIO = audioGraph ?? WSAudioGraph()
        super.init()
        audioIO.onMicFrame = { [weak self] frame in
            guard let self, let encoded = self.encodeOutbound(frame) else { return }
            self.sendDatagramNow(encoded)
        }
    }

    /// Encode one outbound frame in the negotiated codec. A transient Opus
    /// failure drops the frame after consuming its sequence slot, so the
    /// receiver sees the hole and conceals it (identical contract to WSS).
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
                DiagnosticsCensus.shared.increment("audio.quicOpusEncodeFailed")
                return nil
            }
        }
        return PCMUCodec.encode(frame)
    }

    // MARK: Test seams
    #if DEBUG
    convenience init(shortTimeoutForTest: TimeInterval,
                     bindTimeoutForTest: TimeInterval = 1,
                     socketFactory: @escaping SocketFactory) {
        self.init(socketFactory: socketFactory, handshakeTimeout: shortTimeoutForTest,
                  bindTimeout: bindTimeoutForTest)
    }

    /// Inline audio-frame handling (same nonisolated decode path the socket
    /// callback drives).
    func handleAudioForTest(_ data: Data) {
        handleAudioFrame(data)
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

    func sendProbeForTest() { sendProbe() }

    func connectedForTest() -> Bool { connected }
    #endif

    // MARK: Retirement / teardown contract (mirror of WebSocketCallMedia)

    /// Closes a socket whose handshake failed BEFORE it took audio
    /// ownership: retires the network path without touching the audio
    /// graph or deactivating another transport's session.
    func closeWithoutAudio() {
        receiveGeneration &+= 1
        connected = false
        connection.set(connected: false, generation: receiveGeneration)
        probeTimer?.invalidate()
        probeTimer = nil
        pendingProbes.removeAll()
        awaitingReady = false
        readyContinuation = nil
        readyEarlyError = nil
        bindAwaiter.resume(with: .failure(CancellationError()))
        if let socket = socketBox.take() {
            socket.close()
        }
        currentState = .closed
    }

    /// Retires THIS socket after the gateway atomically replaced it: stops
    /// probe/tasks without deactivating the system-owned audio session and
    /// without publishing a failure state.
    func retireAfterHandover() {
        receiveGeneration &+= 1
        connected = false
        connection.set(connected: false, generation: receiveGeneration)
        probeTimer?.invalidate()
        probeTimer = nil
        pendingProbes.removeAll()
        awaitingReady = false
        readyContinuation = nil
        readyEarlyError = nil
        bindAwaiter.resume(with: .failure(CancellationError()))
        if let socket = socketBox.take() {
            socket.close()
        }
        audioIO.stop()
    }

    /// Build-50 hot-standby demotion: the single audio owner moves to the
    /// promoted transport; this socket stays attached for standby freshness.
    func suspendAudioForStandbyHandover() {
        audioIO.stop()
    }

    // MARK: Connection

    /// Connects to the gateway's QUIC candidate and blocks until the
    /// gateway's downlink audio proves the bind (protocol §2/§3):
    /// 1. the QUIC handshake (TLS 1.3 + ALPN; the verify block enforces the
    ///    SPKI pin AND the certificate-validity window — a pin/validity
    ///    failure throws `tlsVerificationFailed`);
    /// 2. `[0x03][bindToken]` bursts every 250 ms (~30 max) until the first
    ///    epoch-valid inbound audio datagram arrives, or ~8 s
    ///    (`bindTimeout`) — on timeout the candidate fails truthfully and
    ///    the incumbent transport is never blocked.
    ///
    /// The codec and epoch are NOT negotiated here: they arrive on the WSS
    /// plane (`ready transport=quic`) and are bound via
    /// `applyTransportReady` while this connect is in flight; inbound audio
    /// stays dropped by the socket's epoch gate until then.
    func connect(
        pin: QUICPin,
        gatewayCandidate: HostPort,
        localEndpoint: NWEndpoint?,
        bindToken: String
    ) async throws {
        receiveGeneration &+= 1
        let generation = receiveGeneration
        let socket = makeSocket(.init(pin: pin, gateway: gatewayCandidate, localEndpoint: localEndpoint))
        socketBox.set(socket)
        installCallbacks(on: socket, generation: generation)
        socket.start()

        // Phase 1: the QUIC handshake. The continuation is parked
        // synchronously on this (main-actor) context before anything
        // suspends, and every resumer is main-actor (state events hop to
        // it), so `.ready`/`.failed` can never outrun the parking.
        awaitingReady = true
        let handshakeTimeoutTask = Task { [weak self] in
            guard let self else { return }
            try? await Task.sleep(nanoseconds: UInt64(self.handshakeTimeoutValue * 1_000_000_000))
            // One-shot: a no-op when the handshake already settled and this
            // task simply lost the cancel race.
            self.resumeReady(with: .failure(MediaError.neverConnected))
        }
        do {
            try await withTaskCancellationHandler {
                if let early = self.readyEarlyError {
                    self.readyEarlyError = nil
                    throw early
                }
                try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
                    self.readyContinuation = continuation
                }
            } onCancel: {
                socket.close()
            }
            handshakeTimeoutTask.cancel()
        } catch {
            handshakeTimeoutTask.cancel()
            // Idempotent no-op when the socket state already consumed it.
            resumeReady(with: .failure(CancellationError()))
            if generation == receiveGeneration {
                awaitingReady = false
                socketBox.take()?.close()
            }
            throw error
        }
        awaitingReady = false
        guard generation == receiveGeneration else {
            socket.close()
            throw CancellationError()
        }

        // Phase 2: bind bursts until the first epoch-valid audio frame
        // proves the gateway consumed the bind token. The awaiter is
        // parked synchronously like phase 1; the timeout and the socket
        // state paths resume it deterministically.
        let awaiter = bindAwaiter
        awaiter.reset()
        let bindTimeoutTask = Task {
            try? await Task.sleep(nanoseconds: UInt64(self.bindTimeout * 1_000_000_000))
            awaiter.resume(with: .failure(QUICMediaError.bindTimeout))
        }
        let burstTask = Task {
            // [0x03][token] every 250 ms, ~30 max (protocol §2). Post-bind
            // bursts are ignored server-side, so lossy retransmits cannot
            // authorize twice; a send failure just ends this loop — the
            // proof or the timeout decides.
            var bursts = 0
            while bursts < Self.maxBindBursts, !Task.isCancelled {
                do { try socket.sendBind(token: bindToken) } catch { return }
                bursts += 1
                do { try await Task.sleep(nanoseconds: 250_000_000) } catch { return }
            }
        }
        do {
            try await withTaskCancellationHandler {
                try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<Void, Error>) in
                    awaiter.park(continuation)
                }
            } onCancel: {
                socket.close()
            }
            bindTimeoutTask.cancel()
            burstTask.cancel()
        } catch {
            bindTimeoutTask.cancel()
            burstTask.cancel()
            // Idempotent no-op when the proof/timeout already consumed it.
            awaiter.resume(with: .failure(CancellationError()))
            if generation == receiveGeneration {
                socketBox.take()?.close()
            }
            throw error
        }
        guard generation == receiveGeneration else {
            socket.close()
            throw CancellationError()
        }

        // READY but not yet the audio owner: the caller enables the graph at
        // the exact handover moment via activateAudio()/promotion, so a
        // staging attach never double-captures while another transport
        // still carries the call (identical contract to WSS connect()).
        connected = true
        currentState = .connected
        connection.set(connected: true, generation: receiveGeneration)
    }

    /// One-shot extraction resume for the phase-1 handshake continuation.
    private func resumeReady(with result: Result<Void, Error>) {
        let continuation = readyContinuation
        readyContinuation = nil
        continuation?.resume(with: result)
    }

    private func installCallbacks(on socket: QUICMediaSocket, generation: UInt64) {
        socket.onBindProof = { [weak self] in
            // The first well-formed audio envelope (any epoch) settles
            // the connect() bind wait: the gateway consumed the token and
            // downlink flows. Media readiness additionally requires the
            // WSS-plane epoch greeting plus a genuinely DECODED frame.
            self?.bindAwaiter.succeed()
        }
        socket.onAudioFrame = { [weak self] data in
            guard let self else { return }
            // Frames decode straight on the receive context (the scheduler
            // is thread-safe); the generation check fences a REPLACED
            // socket: a stale socket's late frames must never inject into
            // the new attach.
            let snapshot = self.connection.snapshot()
            guard snapshot.connected, snapshot.generation == generation else { return }
            self.handleAudioFrame(data)
        }
        socket.onProbeResponse = { [weak self] t, arrivedAt in
            guard let self else { return }
            // The stamp arrives verbatim from the socket's receive context;
            // only the BOOKKEEPING hops to the main actor.
            Task { @MainActor in
                self.handleProbeResponse(t: t, arrivedAt: arrivedAt, generation: generation)
            }
        }
        socket.onStateChange = { [weak self] state in
            guard let self else { return }
            Task { @MainActor in
                self.socketStateChanged(state, generation: generation)
            }
        }
    }

    // MARK: Socket event handling

    @MainActor private func socketStateChanged(_ state: QUICSocketState, generation: UInt64) {
        guard generation == receiveGeneration else { return }
        switch state {
        case .ready:
            // Handshake + TLS pin/validity accepted: the datagram flow is
            // dialable. Media readiness additionally requires the bind
            // proof and the WSS-plane epoch greeting (protocol §3).
            resumeReady(with: .success(()))
        case .failed(let reason):
            let tlsDetail = socketBox.snapshot()?.lastTLSFailure
            let error: Error = tlsDetail.map { QUICMediaError.tlsVerificationFailed($0) }
                ?? QUICMediaError.socketFailed(reason)
            if readyContinuation != nil {
                resumeReady(with: .failure(error))
            } else if awaitingReady {
                // Failure in the synchronous gap before phase 1 parks:
                // fail the next attempt fast instead of hanging.
                readyEarlyError = error
            } else {
                socketDidFail()
            }
            bindAwaiter.resume(with: .failure(error))
        case .closed:
            if readyContinuation != nil {
                // Teardown/cancel/timeout during the handshake.
                resumeReady(with: .failure(CancellationError()))
            } else if connected {
                // The gateway hung up underneath a live attach (bye raced
                // the close): report the loss truthfully.
                socketDidFail()
            }
            bindAwaiter.resume(with: .failure(CancellationError()))
        case .connecting:
            break
        }
    }

    /// Nonisolated audio path: decode and queue straight from the receive
    /// context (the scheduler is thread-safe). Standby decodes for
    /// freshness but never renders — exactly one playback owner.
    private nonisolated func handleAudioFrame(_ data: Data) {
        let snapshot = connection.snapshot()
        guard snapshot.connected else { return }
        wssCodec.recordDownlinkGap()
        wssCodec.lock.lock()
        let decoder = wssCodec.decoder
        let opus = wssCodec.usesOpus
        let framed = wssCodec.framed
        wssCodec.lock.unlock()
        if opus, let decoder {
            if framed {
                guard let parsed = WSSFrameCodec.parse(data) else { return }
                let slots: [WSSFrameCodec.Slot]
                wssCodec.lock.lock()
                slots = wssCodec.depacketizer.arrivals(seq: parsed.seq, payload: parsed.payload)
                if slots.isEmpty {
                    // Seeded stream, no work: a duplicate/late (reordered)
                    // frame — measured, never a fabricated term.
                    wssCodec.recordDownlinkLateOrDuplicate()
                } else {
                    wssCodec.recordDownlinkSlots(slots)
                }
                wssCodec.lock.unlock()
                for slot in slots {
                    switch slot {
                    case .decode(let payload):
                        if let pcm = try? decoder.decode(payload) {
                            self.emitInboundRealFrame()
                            if !self.standbyConsumes(pcm) {
                                audioIO.pushPlayback(pcm)
                            }
                        }
                    case .plc:
                        if let pcm = try? decoder.decode(nil) {
                            audioIO.pushPlayback(pcm)
                        }
                    case .fecRecover(let carrier):
                        if let recovered = try? decoder.decodeFEC(carrier) {
                            audioIO.pushPlayback(recovered)
                        } else if let pcm = try? decoder.decode(nil) {
                            audioIO.pushPlayback(pcm)
                        }
                    }
                }
                return
            }
            // Bare Opus parity with the WSS path (unframed fallback).
            guard !data.isEmpty, data.count <= OpusCodec.maxFrameBytes else { return }
            guard let pcm = try? decoder.decode(Array(data)) else { return }
            self.emitInboundRealFrame()
            if !standbyConsumes(pcm) {
                audioIO.pushPlayback(pcm)
            }
        } else {
            guard data.count == 160, let pcm = PCMUCodec.decode(data) else { return }
            self.emitInboundRealFrame()
            if !standbyConsumes(pcm) {
                audioIO.pushPlayback(pcm)
            }
        }
    }

    private nonisolated func standbyConsumes(_ pcm: [Int16]) -> Bool {
        standbyLock.lock()
        let standby = standbyModeFlag
        standbyLock.unlock()
        guard standby else { return false }
        emitStandbyFresh()
        return true
    }

    private nonisolated func sendDatagramNow(_ frame: Data) {
        guard let socket = socketBox.snapshot() else { return }
        do {
            try socket.sendAudioDatagram(frame)
        } catch {
            Task { @MainActor in self.datagramSendFailed() }
        }
    }

    @MainActor private func datagramSendFailed() {
        guard connected else { return }
        socketDidFail()
    }

    @MainActor private func handleProbeResponse(t: UInt64, arrivedAt: Date, generation: UInt64) {
        guard generation == receiveGeneration, connected else {
            DiagnosticsCensus.shared.increment(
                generation == receiveGeneration ? "quic.probeDropNotConnected" : "quic.probeDropGeneration")
            return
        }
        guard let sent = pendingProbes.removeValue(forKey: t) else {
            DiagnosticsCensus.shared.increment("quic.probeDropNoPending")
            return
        }
        // The RTT is computed against the RECEIVE-context arrival stamp
        // (passed in), never `Date()` here: main-actor scheduling delay
        // must not inflate the sample (build-60 discipline).
        let rtt = arrivedAt.timeIntervalSince(sent)
        // Measurement hygiene: only accept finite, non-negative echoes
        // answering a probe from the last 30 s (identical discipline to the
        // WSS ping path).
        guard rtt.isFinite, rtt >= 0, rtt <= 30 else {
            DiagnosticsCensus.shared.increment("quic.probeDropHygiene")
            return
        }
        probeSampleLog.append(ProbeSample(rtt: rtt, at: arrivedAt))
        if probeSampleLog.count > 120 { probeSampleLog.removeFirst(probeSampleLog.count - 120) }
    }

    private var lastLoggedSocketState: MediaState?

    @MainActor private func socketDidFail(toFailed: Bool = false) {
        guard connected else { return }
        connected = false
        connection.set(connected: false, generation: receiveGeneration)
        currentState = toFailed ? .failed : .disconnected
        if currentState != lastLoggedSocketState {
            lastLoggedSocketState = currentState
            DiagnosticsStore.shared.log("audio", "quic socket \(currentState == .failed ? "failed" : "disconnected")")
        }
    }

    /// Create the codec machinery for the negotiated wire codec. A local
    /// Opus failure after the gateway announced Opus fails the attach
    /// honestly (throw) rather than limping with a PCMU decoder.
    private func adoptCodec(_ codec: CodecChoice, framed: Bool) throws {
        wssCodec.lock.lock()
        defer { wssCodec.lock.unlock() }
        wssCodec.usesOpus = codec == .opus
        wssCodec.framed = framed && wssCodec.usesOpus
        wssCodec.seqOut = 0
        wssCodec.depacketizer = WSSFrameCodec.Depacketizer()
        wssCodec.resetDownlinkQuality()
        guard wssCodec.usesOpus else { return }
        do {
            wssCodec.encoder = try OpusCodec.Encoder()
            wssCodec.decoder = try OpusCodec.Decoder()
            let negotiatedFmt = wssCodec.framed ? "seq16" : "bare"
            AppLog.media.debug("quic codec negotiated: opus fmt=\(negotiatedFmt)")
            DiagnosticsStore.shared.log("audio", "quic codec: opus fmt=\(negotiatedFmt)")
        } catch {
            wssCodec.usesOpus = false
            wssCodec.framed = false
            wssCodec.encoder = nil
            wssCodec.decoder = nil
            DiagnosticsStore.shared.log("audio", "quic opus unavailable: \(error.localizedDescription)")
            throw MediaError.audioActivationFailed
        }
    }

    /// Adopts the negotiated codec the FIRST time only: a later epoch
    /// update (promote/re-attach greeting) must NOT reset the live codec
    /// state — the seq16 sequence space and the depacketizer have to
    /// survive an epoch change mid-call.
    private func adoptCodecIfNeeded(_ codec: CodecChoice, framed: Bool) throws {
        wssCodec.lock.lock()
        let matches = wssCodec.usesOpus == (codec == .opus)
            && wssCodec.framed == (framed && codec == .opus)
        wssCodec.lock.unlock()
        if !matches { try adoptCodec(codec, framed: framed) }
    }

    // MARK: WSS control-plane correlation (protocol §3)

    /// The WSS `ready transport=quic` greeting, delivered by the
    /// coordinator on the WSS control plane: adopts the negotiated
    /// codec/fmt and THEN binds the epoch. Audio stays dropped by the
    /// socket's epoch gate until the epoch is bound, and adoption precedes
    /// the binding, so a frame passing the gate can never meet an
    /// unadopted codec. Throws on local Opus unavailability (the caller
    /// fails this candidate and keeps the incumbent transport).
    func applyTransportReady(epoch: UInt64, codec: String, fmt: String?) throws {
        try adoptCodecIfNeeded(codec == "opus" ? .opus : .pcmu, framed: fmt == "seq16")
        applyEpoch(epoch)
    }

    /// Binds the epoch from a WSS greeting or `promoted` control
    /// (protocol §4): the socket stamps outbound envelopes with it and
    /// drops every inbound datagram carrying any other epoch. The
    /// coordinator must set the epoch BEFORE counting this path's
    /// fresh-media proof as mediaReady (the proof is meaningless in a
    /// stale epoch).
    func applyEpoch(_ epoch: UInt64) {
        lastReadyEpoch = epoch
        socketBox.snapshot()?.expectedEpoch = epoch
    }

    /// The epoch last bound via `applyTransportReady`/`applyEpoch`
    /// (protocol §4 fencing, mirrors the WSS path's `lastReadyEpoch`).
    private(set) var lastReadyEpoch: UInt64?

    // MARK: Audio ownership (mirror of the WebSocketCallMedia contract)

    /// Binds the audio graph (mic capture + playback) AFTER a ready
    /// handshake; the route handover calls it at the exact handover moment.
    func activateAudio() {
        audioAllowed = true
        startAudio()
    }

    /// Marks a staged handover attach: READY but must not open a second
    /// capture graph while the previous transport still carries the call.
    func markAudioStaged(_ staged: Bool = true) {
        audioAllowed = !staged
        isStagedForHandover = staged
    }

    /// True while this session is a staged, unpromoted handover attach: its
    /// socket lifecycle is NOT the call's carrier and must never drive
    /// call-level media failure or a healthy-route publish.
    private(set) var isStagedForHandover = false

    /// Exclusive promotion: this session may now open the capture graph.
    func promoteAudioOwnership() {
        audioAllowed = true
        isStagedForHandover = false
        setHotStandby(false)
        stopStandbySilencePulse()
    }

    // MARK: Hot standby (build 50, same role model as WSS)

    /// True while this socket is a persistent hot STANDBY transport:
    /// attached and authenticated, decoding the mirrored downlink mix for
    /// freshness, never rendering, sending at most the bounded synthetic
    /// silence uplink pulse. Lock-backed: the nonisolated receive path
    /// reads it per frame.
    var isHotStandby: Bool {
        standbyLock.lock()
        defer { standbyLock.unlock() }
        return standbyModeFlag
    }
    private nonisolated(unsafe) var standbyModeFlag = false
    private let standbyLock = NSLock()

    /// Fires on every GENUINE inbound frame while in standby mode (the
    /// hot-standby readiness proof — valid silent PCM counts; PLC/FEC
    /// concealment slots never fire it).
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
        let callback = standbyFreshCallback
        standbyLock.unlock()
        guard let callback else { return }
        Task { @MainActor in callback() }
    }

    private func setHotStandby(_ value: Bool) {
        standbyLock.lock()
        standbyModeFlag = value
        standbyLock.unlock()
    }

    /// Arms the persistent standby role: no audio ownership, non-fatal
    /// lifecycle, freshness counting, and the bounded silence pulse that
    /// proves the uplink without a second mic injection.
    func markHotStandby() {
        audioAllowed = false
        isStagedForHandover = true
        setHotStandby(true)
        startStandbySilencePulse()
    }

    /// Clears the standby role WITHOUT promoting audio ownership.
    func clearHotStandby() {
        setHotStandby(false)
        stopStandbySilencePulse()
    }

    private var standbySilenceTimer: Timer?

    /// Bounded uplink readiness probe: one SILENT frame per second through
    /// this socket's own encoder — never from the mic, never audible.
    private func startStandbySilencePulse() {
        stopStandbySilencePulse()
        standbySilenceTimer = Timer.scheduledTimer(withTimeInterval: 1.0, repeats: true) { [weak self] _ in
            guard let self, self.isHotStandby, self.connected else { return }
            self.enqueueStandbySilenceFrame()
        }
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
        sendDatagramNow(frame)
    }

    /// Route-change revalidation: the real graph re-checks the live input
    /// format; graph fakes do nothing.
    func revalidateAudioRoute() {
        audioIO.revalidateRoute()
    }

    // MARK: Probe sampling

    func startProbeSampling() {
        guard probeTimer == nil else { return }
        // Sample immediately: the call-start route decision compares
        // MEASURED fresh RTTs between candidate paths.
        sendProbe()
        let timer = Timer(timeInterval: 1.0, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.sendProbe() }
        }
        RunLoop.main.add(timer, forMode: .common)
        probeTimer = timer
    }

    private func sendProbe() {
        guard connected, let socket = socketBox.snapshot() else {
            DiagnosticsCensus.shared.increment("quic.probeSendBlocked")
            return
        }
        // The peer echoes `t` verbatim; epoch-millisecond tags are unique
        // at the 1 s cadence and carry honest send time.
        let tag = UInt64(Date().unixMilliseconds)
        pendingProbes[tag] = Date()
        if pendingProbes.count > 8 { pendingProbes.removeAll() }
        DiagnosticsCensus.shared.increment("quic.probeSent")
        let generation = receiveGeneration
        do {
            try socket.sendProbe(t: tag)
        } catch {
            Task { @MainActor in
                guard self.receiveGeneration == generation, self.connected else { return }
                self.socketDidFail()
            }
        }
    }

    // MARK: Audio lifecycle (CallKit contract, same as WebSocketCallMedia)

    func audioActivated(with session: AVAudioSession) {
        if AudioSessionBridge.shared.isInterrupted {
            DiagnosticsCensus.shared.increment("audio.quicActivationWhileInterrupted")
            return
        }
        selfManagedAudioActive = AudioSessionBridge.shared.currentOwnership == .selfManaged
        guard audioAllowed else {
            DiagnosticsCensus.shared.increment("audio.quicStagedActivationSuppressed")
            return
        }
        let normalized = AudioSessionBridge.normalizeForVoiceChat(session)
        if normalized {
            DiagnosticsStore.shared.log("audio", "quic session normalized at engine start (changed=yes)")
        }
        guard audioIO.startIfNeeded() else {
            // Never claim audio that cannot run. Inside the staged-promotion
            // window the caller owns the failure decision; the PRIMARY
            // transport keeps the fatal semantics.
            DiagnosticsStore.shared.log("audio",
                "quic system activation failed: graph start error"
                + Self.failureCodeSuffix(audioIO.lastStartFailureCode))
            if graphStartFailureNonfatal {
                DiagnosticsCensus.shared.increment("audio.quicActivationFailureNonfatal")
                return
            }
            socketDidFail(toFailed: true)
            return
        }
        DiagnosticsStore.shared.log("audio", "quic audio activated (system)")
        AppLog.media.debug("quic audio activated")
    }

    /// Owned graph start that REPORTS failure to the caller instead of
    /// failing the socket (exclusive promotion / self-managed activation).
    @discardableResult
    func startOwnedAudioGraph(with session: AVAudioSession) -> Bool {
        if AudioSessionBridge.shared.isInterrupted {
            DiagnosticsCensus.shared.increment("audio.quicActivationWhileInterrupted")
            return false
        }
        selfManagedAudioActive = AudioSessionBridge.shared.currentOwnership == .selfManaged
        guard audioAllowed else {
            DiagnosticsCensus.shared.increment("audio.quicStagedActivationSuppressed")
            return false
        }
        let normalized = AudioSessionBridge.normalizeForVoiceChat(session)
        if normalized {
            DiagnosticsStore.shared.log("audio", "quic session normalized at engine start (changed=yes)")
        }
        guard audioIO.startIfNeeded() else {
            DiagnosticsCensus.shared.increment("audio.quicOwnedGraphStartFail")
            DiagnosticsStore.shared.log("audio",
                "quic owned graph start failed"
                + Self.failureCodeSuffix(audioIO.lastStartFailureCode))
            return false
        }
        DiagnosticsStore.shared.log("audio", "quic audio activated (owned)")
        AppLog.media.debug("quic audio activated (owned)")
        return true
    }

    /// Live audio-input readiness for the exclusive promotion handover
    /// (build-51). Poll-safe.
    func isAudioInputReady() -> Bool { audioIO.isInputReadyForStart() }

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
            return startOwnedAudioGraph(with: active)
        }
        guard let session = AudioSessionBridge.shared.activateSelfManaged() else {
            DiagnosticsStore.shared.log("audio", "quic direct-answer activation error")
            return false
        }
        selfManagedAudioActive = true
        if audioAllowed, !audioIO.startIfNeeded() {
            AudioSessionBridge.shared.registerSelfManagedDeactivation()
            selfManagedAudioActive = false
            DiagnosticsStore.shared.log("audio",
                "quic direct-answer activation failed: graph start error"
                + Self.failureCodeSuffix(audioIO.lastStartFailureCode))
            return false
        }
        _ = session
        DiagnosticsStore.shared.log("audio", "quic audio activated (direct answer)")
        return true
    }

    func deactivateAudioWithoutCallKit() {
        guard selfManagedAudioActive else { return }
        selfManagedAudioActive = false
        audioIO.stop()
        AudioSessionBridge.shared.deactivateSelfManaged()
    }

    private func startAudio() {
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

    // MARK: Call progress tone / telemetry (mirror)

    var isGraphRunning: Bool { audioIO.isRunning }
    var playbackIdleMilliseconds: Int { audioIO.playbackIdleMilliseconds }

    func pushSyntheticTone(_ frame: [Int16]) {
        audioIO.pushSyntheticPlayback(frame)
    }

    func setSpeakerphone(_ enabled: Bool) throws {
        let session = AVAudioSession.sharedInstance()
        try session.overrideOutputAudioPort(enabled ? .speaker : .none)
    }

    /// Full teardown: fences late callbacks, cancels timers, closes the
    /// socket, tears the codec down and releases self-managed audio.
    func close() {
        receiveGeneration &+= 1
        connected = false
        connection.set(connected: false, generation: receiveGeneration)
        probeTimer?.invalidate()
        probeTimer = nil
        pendingProbes.removeAll()
        awaitingReady = false
        readyContinuation = nil
        readyEarlyError = nil
        bindAwaiter.resume(with: .failure(CancellationError()))
        if let socket = socketBox.take() {
            socket.close()
        }
        wssCodec.lock.lock()
        wssCodec.encoder?.close()
        wssCodec.decoder?.close()
        wssCodec.encoder = nil
        wssCodec.decoder = nil
        wssCodec.usesOpus = false
        wssCodec.framed = false
        wssCodec.lock.unlock()
        stopStandbySilencePulse()
        audioIO.stop()
        deactivateAudioWithoutCallKit()
        currentState = .closed
    }

    /// [0x03] bind burst cadence: ~30 bursts x 250 ms brackets the ~8 s
    /// punch/bind window (protocol §2/§5).
    static let maxBindBursts = 30
}

enum QUICMediaError: Error, LocalizedError {
    /// Transport failure before/instead of media readiness (handshake
    /// failure, socket death underneath a live attach). Carries a
    /// privacy-safe reason.
    case socketFailed(String)
    /// The TLS verify block rejected the peer: SPKI pin mismatch, the
    /// certificate outside its validity window, or a non-P-256 key. Hard
    /// handshake failure — there is deliberately no trust-all fallback
    /// (protocol §6).
    case tlsVerificationFailed(String)
    /// The gateway never delivered downlink audio within the bind window:
    /// the candidate fails truthfully and the caller keeps (or falls back
    /// to) the incumbent transport.
    case bindTimeout

    var errorDescription: String? {
        switch self {
        case .socketFailed(let reason):
            return String(localized: "QUIC 直连音频通道失败：\(reason)")
        case .tlsVerificationFailed(let detail):
            return String(localized: "QUIC 直连 TLS 校验失败：\(detail)")
        case .bindTimeout:
            return String(localized: "QUIC 直连绑定超时")
        }
    }
}

// MARK: - Private support types

/// Lock-owned socket box: the nonisolated mic-feed and datagram paths read
/// the current socket without a main-actor hop.
private final class SocketBox: @unchecked Sendable {
    private let lock = NSLock()
    private var socket: QUICMediaSocket?

    func set(_ socket: QUICMediaSocket?) {
        lock.lock()
        self.socket = socket
        lock.unlock()
    }

    /// Clears and returns the previous socket (so the caller closes exactly
    /// the instance it owned).
    func take() -> QUICMediaSocket? {
        lock.lock()
        let previous = socket
        socket = nil
        lock.unlock()
        return previous
    }

    func snapshot() -> QUICMediaSocket? {
        lock.lock()
        defer { lock.unlock() }
        return socket
    }
}

/// Nonisolated connection fence mirroring the main-actor `connected` flag
/// and receive generation for the detached datagram receive context.
private final class QUICConnectionFence: @unchecked Sendable {
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

/// Lock-owned single-shot bind-proof awaiter: resumed from the socket's
/// RECEIVE context (the first epoch-valid audio frame), the timeout task,
/// and main-actor socket state events, so the continuation cannot live in
/// main-actor state. Every resume is an idempotent no-op once the parked
/// continuation has been consumed.
private final class BindAwaiter: @unchecked Sendable {
    private let lock = NSLock()
    private var continuation: CheckedContinuation<Void, Error>?

    func reset() {
        lock.lock()
        continuation = nil
        lock.unlock()
    }

    func park(_ continuation: CheckedContinuation<Void, Error>) {
        lock.lock()
        if let stale = self.continuation {
            self.continuation = continuation
            lock.unlock()
            stale.resume(throwing: CancellationError())
        } else {
            self.continuation = continuation
            lock.unlock()
        }
    }

    func succeed() {
        resume(with: .success(()))
    }

    func resume(with result: Result<Void, Error>) {
        lock.lock()
        let continuation = self.continuation
        self.continuation = nil
        lock.unlock()
        continuation?.resume(with: result)
    }
}
