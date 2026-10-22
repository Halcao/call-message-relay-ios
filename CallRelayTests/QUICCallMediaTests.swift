import XCTest
import AVFoundation
@testable import CallRelay

// MARK: - Deterministic fake

/// Lock-owned fake of the QUIC media socket: drivable from any queue (the
/// production callbacks run on the socket's receive context), records every
/// audio frame / bind / probe sent, and lets tests emit socket events
/// inline. Mirrors the FINAL build-61 seam: the fake speaks
/// envelope-stripped frames (the production socket owns the
/// classification/epoch gate — see QUICSocketFrameTests).
final class FakeQUICMediaSocket: QUICMediaSocket, @unchecked Sendable {
    private let lock = NSLock()
    private(set) var startedCount = 0
    private(set) var closed = false
    private(set) var sentAudioFrames: [Data] = []
    private(set) var sentBinds: [String] = []
    private(set) var sentProbes: [UInt64] = []
    /// When armed, audio-datagram sends throw (send-path failure tests).
    var sendAudioError: Error?
    var expectedEpoch: UInt64 = 0
    var localEndpointMismatch = false
    var lastTLSFailure: String?
    var onAudioFrame: (@Sendable (Data) -> Void)?
    var onBindProof: (@Sendable () -> Void)?
    private var bindProofFired = false
    var onProbeResponse: (@Sendable (UInt64, Date) -> Void)?
    var onStateChange: (@Sendable (QUICSocketState) -> Void)?

    func start() {
        lock.lock(); startedCount += 1; lock.unlock()
    }

    func sendAudioDatagram(_ frame: Data) throws {
        lock.lock()
        guard !closed else { lock.unlock(); throw QUICSocketError.closed }
        let error = sendAudioError
        if error == nil { sentAudioFrames.append(frame) }
        lock.unlock()
        if let error { throw error }
    }

    func sendBind(token: String) throws {
        lock.lock()
        guard !closed else { lock.unlock(); throw QUICSocketError.closed }
        sentBinds.append(token)
        lock.unlock()
    }

    func sendProbe(t: UInt64) throws {
        lock.lock()
        guard !closed else { lock.unlock(); throw QUICSocketError.closed }
        sentProbes.append(t)
        lock.unlock()
    }

    func close() {
        lock.lock()
        guard !closed else { lock.unlock(); return }
        closed = true
        lock.unlock()
        // The production socket always announces the final closed state.
        onStateChange?(.closed)
    }

    // MARK: Test drivers (callable from any queue)

    func emitState(_ state: QUICSocketState) {
        onStateChange?(state)
    }

    /// Delivers one envelope-stripped frame, exactly as the production
    /// socket's `onAudioFrame` contract promises (envelope + epoch
    /// validated inside the real socket).
    func emitAudioFrame(_ frame: Data) {
        // Raw epoch-valid delivery (the helper applied the greeting):
        // also the bind proof when it is the first frame.
        if !bindProofFired {
            bindProofFired = true
            onBindProof?()
        }
        onAudioFrame?(frame)
    }

    /// Delivers a full `[0x01][epoch][frame]` envelope through the same
    /// epoch gate the production socket applies (expectedEpoch must be
    /// bound and equal).
    func emitAudioEnvelope(epoch: UInt64, frame: Data) {
        // Well-formed envelope even with an unbound/mismatched epoch is
        // bind proof (mirrors the production socket); the epoch gate then
        // decides delivery to the codec.
        if !bindProofFired {
            bindProofFired = true
            onBindProof?()
        }
        guard expectedEpoch != 0, epoch == expectedEpoch else { return }
        emitAudioFrame(frame)
    }

    /// Probe echo, stamped by the test on behalf of the socket's receive
    /// context (the production socket stamps `Date()` there).
    func emitProbeResponse(_ t: UInt64, arrivedAt: Date) {
        onProbeResponse?(t, arrivedAt)
    }

    /// Test-only: reset the one-shot bind proof and the closed state so a
    /// FRESH media instance can drive a complete new attach through this
    /// shared fake (multi-call coordinator scenarios).
    func resetForReuse() {
        lock.lock()
        bindProofFired = false
        closed = false
        lock.unlock()
    }
}

// MARK: - Wire framing (pure functions of the production socket)

final class QUICSocketFrameTests: XCTestCase {
    private func envelope(epoch: UInt64, frame: Data) -> Data {
        var data = Data([0x01])
        data.append(contentsOf: [UInt8](repeating: 0, count: 8))
        NWQUICMediaSocket.writeU64BE(epoch, into: &data, at: 1)
        data.append(frame)
        return data
    }

    func testAudioAcceptedOnlyWhenEpochBoundAndMatching() {
        let frame = Data([0x00, 0x05, 1, 2, 3])
        // Unset expected epoch (0): EVERY audio datagram is dropped until
        // the coordinator binds the WSS greeting's epoch (protocol §4).
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(envelope(epoch: 7, frame: frame), expectedEpoch: 0),
                       .staleEpochAudio)
        // Stale epoch (across re-attach/promote/network change).
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(envelope(epoch: 6, frame: frame), expectedEpoch: 7),
                       .staleEpochAudio)
        // Matching epoch: accepted, and the strip is EXACTLY the 9-byte
        // version+epoch header — the seq16 header stays inside the frame.
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(envelope(epoch: 7, frame: frame), expectedEpoch: 7),
                       .audio(frame))
        // Envelope with no inner bytes at all.
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(Data([0x01, 0, 0, 0, 0, 0, 0, 0, 7]), expectedEpoch: 7),
                       .dropped(.badEnvelope))
    }

    func testProbeEchoClassification() {
        var echo = Data([0x04])
        echo.append(contentsOf: [UInt8](repeating: 0, count: 8))
        NWQUICMediaSocket.writeU64BE(0x0102030405060708, into: &echo, at: 1)
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(echo, expectedEpoch: 0), .probe(0x0102030405060708))
        // Probes are not epoch-gated; a malformed length drops.
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(Data([0x04, 1, 2, 3]), expectedEpoch: 0),
                       .dropped(.badEnvelope))
    }

    func testBindBurstsAndUnknownTypesDroppedHonestly() {
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(Data([0x03]) + Data("pt-1".utf8), expectedEpoch: 0),
                       .ignoredBind, "post-bind bursts are ignored, never counted")
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(Data([0x7F, 1, 2]), expectedEpoch: 0),
                       .dropped(.unknownType))
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(Data(), expectedEpoch: 0), .dropped(.empty))
    }

    func testOversizeDatagramsDropped() {
        let frame = Data([0x00, 0x01, 0xAA])
        // 1201 bytes: past the protocol's 1200-byte bound.
        var big = envelope(epoch: 7, frame: frame)
        big.append(contentsOf: [UInt8](repeating: 0, count: 1201 - big.count))
        XCTAssertEqual(big.count, 1201)
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(big, expectedEpoch: 7), .dropped(.oversize))
        // The QUIC path's usable frame size can be lower than 1200.
        XCTAssertEqual(NWQUICMediaSocket.classifyInbound(big, expectedEpoch: 7, maxBytes: 600),
                       .dropped(.oversize))
    }

    func testStampAudioEnvelopeShapeAndOversizeDrop() {
        let stamped = try? XCTUnwrap(NWQUICMediaSocket.stampAudioEnvelope(
            epoch: 0x0102030405060708, frame: Data([0xAA, 0xBB])))
        XCTAssertEqual(stamped?[0], 0x01)
        XCTAssertEqual(Array(stamped?[1...8] ?? Data()), [1, 2, 3, 4, 5, 6, 7, 8])
        XCTAssertEqual(Array(stamped?.dropFirst(9) ?? Data()), [0xAA, 0xBB])
        // Envelope + payload must fit 1200 B: 1191 fits, 1192 does not.
        XCTAssertNotNil(NWQUICMediaSocket.stampAudioEnvelope(epoch: 1, frame: Data(count: 1191)))
        XCTAssertNil(NWQUICMediaSocket.stampAudioEnvelope(epoch: 1, frame: Data(count: 1192)))
    }

    func testBindAndProbeDatagramShapes() {
        XCTAssertEqual(Array(NWQUICMediaSocket.bindDatagram(token: "pt-1")),
                       [0x03] + Array("pt-1".utf8))
        XCTAssertEqual(Array(NWQUICMediaSocket.probeDatagram(t: 0x102)),
                       [0x04, 0, 0, 0, 0, 0, 0, 0x01, 0x02])
    }
}

// MARK: - Lifecycle / bind handshake / probe sampling / fencing

@MainActor
final class QUICCallMediaLifecycleTests: XCTestCase {
    private let candidate = HostPort(host: "203.0.113.9", port: 8792)
    private let pin = QUICPin(spkiSHA256: Data([0xAA, 0xBB]))

    /// Drives the full connect: start -> .ready -> bind bursts -> WSS
    /// greeting applied -> first inbound audio proves the bind.
    private func connect(
        _ media: QUICCallMedia,
        via socket: FakeQUICMediaSocket,
        epoch: UInt64 = 17,
        bindToken: String = "pt-1"
    ) async throws {
        let task = Task { try await media.connect(
            pin: pin, gatewayCandidate: candidate,
            localEndpoint: nil, bindToken: bindToken) }
        await waitUntil { socket.startedCount == 1 }
        socket.emitState(.ready)
        await waitUntil { !socket.sentBinds.isEmpty }
        try media.applyTransportReady(epoch: epoch, codec: "opus", fmt: "seq16")
        let enc = try OpusCodec.Encoder()
        let tone = [Int16](repeating: 1_200, count: OpusCodec.frameSamples)
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 0, payload: try enc.encode(tone)))
        try await task.value
    }

    func testConnectSendsBindBurstsAndCompletesOnFirstInboundAudio() async throws {
        let socket = FakeQUICMediaSocket()
        var states: [MediaState] = []
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: FakeAudioGraph())
        media.onState = { states.append($0) }

        let task = Task { try await media.connect(
            pin: pin, gatewayCandidate: candidate,
            localEndpoint: nil, bindToken: "pt-9") }
        await waitUntil { socket.startedCount == 1 }
        socket.emitState(.ready)
        await waitUntil { !socket.sentBinds.isEmpty }
        XCTAssertEqual(socket.sentBinds.first, "pt-9", "bind bursts carry the one-time punch token")
        XCTAssertEqual(socket.startedCount, 1)
        XCTAssertFalse(media.connectedForTest(), "no media state before the bind proof")

        try media.applyTransportReady(epoch: 17, codec: "opus", fmt: "seq16")
        let enc = try OpusCodec.Encoder()
        let tone = [Int16](repeating: 1_200, count: OpusCodec.frameSamples)
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 0, payload: try enc.encode(tone)))
        try await task.value

        XCTAssertEqual(states.last, .connected)
        XCTAssertTrue(media.connectedForTest())
        XCTAssertTrue(media.usesOpusForTest, "the WSS greeting's codec is adopted")
        XCTAssertTrue(media.framedForTest, "the WSS greeting's fmt seq16 is adopted")
        XCTAssertEqual(media.lastReadyEpoch, 17, "the greeting's epoch is bound for fencing")
        XCTAssertEqual(socket.expectedEpoch, 17, "the socket stamps/validates against the bound epoch")
        media.close()
    }

    func testBindBurstsStopAfterFirstInboundAudio() async throws {
        let socket = FakeQUICMediaSocket()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: FakeAudioGraph())

        let task = Task { try await media.connect(
            pin: pin, gatewayCandidate: candidate,
            localEndpoint: nil, bindToken: "pt-1") }
        await waitUntil { socket.startedCount == 1 }
        socket.emitState(.ready)
        await waitUntil { !socket.sentBinds.isEmpty }
        try media.applyTransportReady(epoch: 7, codec: "opus", fmt: "seq16")
        let enc = try OpusCodec.Encoder()
        let tone = [Int16](repeating: 1_200, count: OpusCodec.frameSamples)
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 0, payload: try enc.encode(tone)))
        try await task.value

        let burstCount = socket.sentBinds.count
        XCTAssertGreaterThanOrEqual(burstCount, 1)
        try await Task.sleep(nanoseconds: 600_000_000)
        XCTAssertEqual(socket.sentBinds.count, burstCount,
                       "bind bursts stop once downlink audio proves the bind")
        media.close()
    }

    /// Build-60 discipline, mirrored from WebSocketCallMediaTests: the echo
    /// arrived 0.2 s after the send but the handler only runs 0.5 s after
    /// the send (stalled main thread). A handler reading its own clock
    /// would record ~0.5 s; the pinned receive-context stamp must record
    /// ~0.2 s and anchor freshness.
    func testProbeArrivalStampPinnedToReceiveContext() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        try await connect(media, via: socket)

        let t0 = Date()
        media.sendProbeForTest()
        await waitUntil { socket.sentProbes.count == 1 }
        let tag = socket.sentProbes[0]
        XCTAssertGreaterThanOrEqual(tag, UInt64(t0.unixMilliseconds))
        let pinnedArrival = t0.addingTimeInterval(0.2)
        try await Task.sleep(nanoseconds: 500_000_000)
        socket.emitProbeResponse(tag, arrivedAt: pinnedArrival)

        await waitUntil { media.probeSamples.count == 1 }
        XCTAssertEqual(media.probeSamples.count, 1, "a matched echo records one RTT sample")
        XCTAssertGreaterThanOrEqual(media.probeSamples[0], 0.15,
                                    "RTT must reflect the pinned network arrival")
        XCTAssertLessThanOrEqual(media.probeSamples[0], 0.3,
                                 "a 0.5 s main-actor handling delay must not inflate the RTT")
        XCTAssertEqual(media.lastProbeSample?.at, pinnedArrival,
                       "the freshness anchor is the arrival stamp, not the handling time")
        media.close()
    }

    func testProbeEchoWithUnknownTagIgnored() async throws {
        let socket = FakeQUICMediaSocket()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: FakeAudioGraph())
        try await connect(media, via: socket)
        socket.emitProbeResponse(999_999, arrivedAt: Date())
        try await Task.sleep(nanoseconds: 100_000_000)
        XCTAssertTrue(media.probeSamples.isEmpty, "an echo answering no outstanding probe is dropped")
        media.close()
    }

    func testConnectCompletesOnlyAfterBindProofAndProofFrameIsFenceDropped() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        let task = Task { try await media.connect(
            pin: pin, gatewayCandidate: candidate,
            localEndpoint: nil, bindToken: "pt-1") }
        await waitUntil { socket.startedCount == 1 }
        socket.emitState(.ready)
        await waitUntil { !socket.sentBinds.isEmpty }
        try media.applyTransportReady(epoch: 5, codec: "opus", fmt: "seq16")

        // The first epoch-valid frame IS the bind proof (protocol §3): it
        // completes the connect while the connection fence still says
        // not-connected, so the frame itself is dropped — media became
        // connected BECAUSE of it.
        let enc = try OpusCodec.Encoder()
        let tone = [Int16](repeating: 1_200, count: OpusCodec.frameSamples)
        socket.emitAudioEnvelope(epoch: 5, frame: WSSFrameCodec.frame(seq: 1, payload: try enc.encode(tone)))
        try await task.value
        XCTAssertTrue(media.connectedForTest())
        XCTAssertEqual(graph.pushedFrames, 0, "the bind-proof frame is fence-dropped")

        // The next frame decodes normally.
        socket.emitAudioEnvelope(epoch: 5, frame: WSSFrameCodec.frame(seq: 2, payload: try enc.encode(tone)))
        await waitUntil { graph.pushedFrames == 1 }
        media.close()
    }

    func testEpochUpdateMidCallKeepsTheSeq16Space() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        try await connect(media, via: socket)

        let tone = [Int16](repeating: 500, count: 160)
        graph.onMicFrame?(tone)
        graph.onMicFrame?(tone)
        await waitUntil { socket.sentAudioFrames.count == 2 }

        // A `promoted` control re-binds the epoch: fencing only — the live
        // codec state (encoder, seq16 space, depacketizer) must survive.
        media.applyEpoch(18)
        XCTAssertEqual(media.lastReadyEpoch, 18)
        XCTAssertEqual(socket.expectedEpoch, 18)

        graph.onMicFrame?(tone)
        await waitUntil { socket.sentAudioFrames.count == 3 }
        XCTAssertEqual(WSSFrameCodec.parse(socket.sentAudioFrames[2])?.seq, 2,
                       "the outbound sequence space is not reset by an epoch update")
        media.close()
    }

    func testAudioGatedUntilCoordinatorAppliesReadyGreeting() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        let task = Task { try await media.connect(
            pin: pin, gatewayCandidate: candidate,
            localEndpoint: nil, bindToken: "pt-1") }
        await waitUntil { socket.startedCount == 1 }
        socket.emitState(.ready)
        await waitUntil { !socket.sentBinds.isEmpty }
        // No applyTransportReady yet: the epoch is unbound, so the codec
        // gate drops every audio datagram (the fake mirrors that gate in
        // emitAudioEnvelope). A well-formed pre-greeting envelope still
        // proves the BIND (the gateway stamps downlink with ITS current
        // epoch before the WSS greeting reaches us): transport connects,
        // the codec stays silent, and no media-ready proof fires.
        let enc = try OpusCodec.Encoder()
        let tone = [Int16](repeating: 1_200, count: OpusCodec.frameSamples)
        socket.emitAudioEnvelope(epoch: 3, frame: WSSFrameCodec.frame(seq: 0, payload: try enc.encode(tone)))
        try await Task.sleep(nanoseconds: 200_000_000)
        XCTAssertTrue(media.connectedForTest(), "a well-formed envelope proves the bind even before the greeting")
        XCTAssertEqual(graph.pushedFrames, 0, "the codec gate drops pre-greeting audio")
        XCTAssertFalse(media.mediaProvenForTest(), "no fresh-media proof before an epoch-valid decoded frame")

        // The coordinator applies the WSS greeting (codec + epoch); the
        // first epoch-valid frame then proves the bind and media is live.
        try media.applyTransportReady(epoch: 3, codec: "opus", fmt: "seq16")
        socket.emitAudioEnvelope(epoch: 3, frame: WSSFrameCodec.frame(seq: 1, payload: try enc.encode(tone)))
        try await task.value
        XCTAssertTrue(media.connectedForTest())
        socket.emitAudioEnvelope(epoch: 3, frame: WSSFrameCodec.frame(seq: 2, payload: try enc.encode(tone)))
        await waitUntil { graph.pushedFrames == 1 }
        media.close()
    }

    func testAfterCloseAllSocketCallbacksIgnored() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        var standbyFresh = 0
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        media.onStandbyFreshMedia = { standbyFresh += 1 }
        try await connect(media, via: socket)

        var realFrames = 0
        media.onInboundRealFrame = { realFrames += 1 }
        media.close()

        socket.emitProbeResponse(123, arrivedAt: Date())
        socket.emitAudioFrame(Data(PCMUCodec.encode([Int16](repeating: 100, count: 160))))
        socket.emitState(.failed("late"))
        XCTAssertEqual(media.probeSamples.count, 0, "late probe echoes are ignored after close")
        XCTAssertEqual(graph.pushedFrames, 0, "late frames are ignored after close")
        XCTAssertEqual(realFrames, 0, "no fresh-media proof from a closed socket")
        XCTAssertEqual(standbyFresh, 0)
        XCTAssertEqual(media.stateForTest(), .closed)
    }

    func testHandshakeTimeoutThrowsAndClosesSocket() async {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(shortTimeoutForTest: 0.2, socketFactory: { _ in socket })
        do {
            try await media.connect(pin: pin, gatewayCandidate: candidate,
                                    localEndpoint: nil, bindToken: "pt-1")
            XCTFail("a stalled QUIC handshake must throw")
        } catch {
            XCTAssertTrue(socket.closed, "timeout must close the socket")
        }
        XCTAssertFalse(media.connectedForTest(), "a timed-out handshake never connects")
        XCTAssertEqual(graph.startCount, 0, "no audio graph starts on a failed handshake")
        media.close()
    }

    func testBindTimeoutThrowsAndClosesSocket() async {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(shortTimeoutForTest: 2, bindTimeoutForTest: 0.3,
                                  socketFactory: { _ in socket })
        let task = Task { try await media.connect(
            pin: pin, gatewayCandidate: candidate,
            localEndpoint: nil, bindToken: "pt-1") }
        await waitUntil { socket.startedCount == 1 }
        // Handshake completes, but the gateway never sends downlink audio:
        // the bind bursts expire truthfully.
        socket.emitState(.ready)
        do {
            try await task.value
            XCTFail("a bind with no downlink audio must throw")
        } catch let error as QUICMediaError {
            guard case .bindTimeout = error else {
                return XCTFail("expected bindTimeout, got \(error)")
            }
        } catch {
            XCTFail("expected bindTimeout, got \(error)")
        }
        XCTAssertTrue(socket.closed, "bind timeout must close the socket")
        XCTAssertFalse(media.connectedForTest())
        XCTAssertEqual(graph.startCount, 0, "a failed bind never starts audio")
        media.close()
    }

    /// Bad-pin path (the NW-level verify block itself is not unit-testable):
    /// a socket that dies with `.failed` and a TLS failure detail surfaces
    /// as a thrown `tlsVerificationFailed` and never starts audio.
    func testTLSFailureFailsHandshakeAndNeverStartsAudio() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        let task = Task { try await media.connect(
            pin: pin, gatewayCandidate: candidate,
            localEndpoint: nil, bindToken: "pt-1") }
        await waitUntil { socket.startedCount == 1 }
        socket.lastTLSFailure = "spki pin mismatch"
        socket.emitState(.failed("spki pin mismatch"))
        do {
            try await task.value
            XCTFail("a failed socket must fail the connect")
        } catch let error as QUICMediaError {
            guard case .tlsVerificationFailed(let detail) = error else {
                return XCTFail("expected tlsVerificationFailed, got \(error)")
            }
            XCTAssertTrue(detail.contains("pin"), "the pin failure detail is surfaced: \(detail)")
        }
        XCTAssertFalse(media.connectedForTest())
        XCTAssertEqual(graph.startCount, 0, "a failed QUIC attach never starts audio")
        XCTAssertEqual(media.stateForTest(), .idle, "no partial media state is published")
        media.close()
    }

    func testSocketFailureWithoutTLSDetailIsSocketFailed() async throws {
        let socket = FakeQUICMediaSocket()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: FakeAudioGraph())
        let task = Task { try await media.connect(
            pin: pin, gatewayCandidate: candidate,
            localEndpoint: nil, bindToken: "pt-1") }
        await waitUntil { socket.startedCount == 1 }
        socket.emitState(.failed("network down"))
        do {
            try await task.value
            XCTFail("a failed socket must fail the connect")
        } catch let error as QUICMediaError {
            guard case .socketFailed(let reason) = error else {
                return XCTFail("expected socketFailed, got \(error)")
            }
            XCTAssertTrue(reason.contains("network down"))
        }
        media.close()
    }
}

// MARK: - seq16 framing / codec reuse (same assertions as WSSCodecTests)

@MainActor
final class QUICCallMediaFramingTests: XCTestCase {
    private let candidate = HostPort(host: "203.0.113.9", port: 8792)
    private let pin = QUICPin(spkiSHA256: Data([0x01]))

    private func connect(_ media: QUICCallMedia, via socket: FakeQUICMediaSocket) async throws {
        let task = Task { try await media.connect(
            pin: pin, gatewayCandidate: candidate,
            localEndpoint: nil, bindToken: "pt-1") }
        await waitUntil { socket.startedCount == 1 }
        socket.emitState(.ready)
        await waitUntil { !socket.sentBinds.isEmpty }
        try media.applyTransportReady(epoch: 5, codec: "opus", fmt: "seq16")
        let enc = try OpusCodec.Encoder()
        let tone = [Int16](repeating: 1_200, count: OpusCodec.frameSamples)
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 0, payload: try enc.encode(tone)))
        try await task.value
    }

    /// Inbound gap (frames 1, 2, 4): slot 3 is recovered from packet 4's
    /// inband FEC and every slot reaches playback exactly once.
    func testInboundGapRunsFECAndDecodesEverySlot() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        try await connect(media, via: socket)

        let enc = try OpusCodec.Encoder()
        let tone = (0..<OpusCodec.frameSamples).map {
            Int16(9_000 * sin(2 * .pi * 440 * Double($0) / Double(OpusCodec.sampleRate)))
        }
        let p1 = try enc.encode(tone)
        let p2 = try enc.encode(tone)
        let p4 = try enc.encode(tone)

        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 1, payload: p1))
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 2, payload: p2))
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 4, payload: p4))
        XCTAssertEqual(graph.pushedFrames, 4,
                       "decode(1), decode(2), FEC(3), decode(4) — every slot plays once")
        media.close()
    }

    /// Multi-slot gaps emit mature PLC per missing slot (bounded by
    /// WSSFrameCodec.maxGapSlots), keeping the decoder timeline aligned.
    func testMultiSlotGapConcealedWithPLC() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        try await connect(media, via: socket)

        let enc = try OpusCodec.Encoder()
        let tone = [Int16](repeating: 1_000, count: OpusCodec.frameSamples)
        let p1 = try enc.encode(tone)
        let p5 = try enc.encode(tone)
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 1, payload: p1))
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 5, payload: p5))
        XCTAssertEqual(graph.pushedFrames, 5, "decode(1) + three PLC slots + decode(5) — every slot plays once")
        media.close()
    }

    func testOutboundFramesCarrySeq16() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        try await connect(media, via: socket)

        let frame = [Int16](repeating: 500, count: 160)
        graph.onMicFrame?(frame)
        graph.onMicFrame?(frame)
        await waitUntil { socket.sentAudioFrames.count >= 2 }
        XCTAssertEqual(socket.sentAudioFrames.count, 2)
        let first = WSSFrameCodec.parse(socket.sentAudioFrames[0])
        let second = WSSFrameCodec.parse(socket.sentAudioFrames[1])
        XCTAssertEqual(first?.seq, 0)
        XCTAssertEqual(second?.seq, 1)
        XCTAssertGreaterThan(first?.payload.count ?? 0, 0, "real Opus payload, seq16 framed")
        media.close()
    }

    func testFreshMediaProofFiresOnlyForGenuineFrames() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        var realFrames = 0
        media.onInboundRealFrame = { realFrames += 1 }
        try await connect(media, via: socket)

        let enc = try OpusCodec.Encoder()
        let tone = [Int16](repeating: 2_000, count: OpusCodec.frameSamples)
        let p1 = try enc.encode(tone)
        let p3 = try enc.encode(tone)
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 1, payload: p1))
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 3, payload: p3))
        // decode(1) is genuine; FEC(2) and decode(3) follow.
        await waitUntil { realFrames == 2 }
        XCTAssertEqual(realFrames, 2, "PLC/FEC concealment slots never fire the proof")
        media.close()
    }
}

// MARK: - Hot standby (build 50 role model, mirrored from the WSS tests)

@MainActor
final class QUICCallMediaStandbyTests: XCTestCase {
    private let candidate = HostPort(host: "203.0.113.9", port: 8792)
    private let pin = QUICPin(spkiSHA256: Data([0x02]))

    private func connect(
        _ media: QUICCallMedia,
        via socket: FakeQUICMediaSocket,
        epoch: UInt64 = 21
    ) async throws {
        let task = Task { try await media.connect(
            pin: pin, gatewayCandidate: candidate,
            localEndpoint: nil, bindToken: "pt-1") }
        await waitUntil { socket.startedCount == 1 }
        socket.emitState(.ready)
        await waitUntil { !socket.sentBinds.isEmpty }
        try media.applyTransportReady(epoch: epoch, codec: "opus", fmt: "seq16")
        let enc = try OpusCodec.Encoder()
        let tone = [Int16](repeating: 1_200, count: OpusCodec.frameSamples)
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 0, payload: try enc.encode(tone)))
        try await task.value
    }

    func testStandbyDecodesForFreshnessButNeverRenders() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        var standbyFresh = 0
        media.onStandbyFreshMedia = { standbyFresh += 1 }
        try await connect(media, via: socket)

        media.markHotStandby()
        XCTAssertTrue(media.isHotStandby)

        let enc = try OpusCodec.Encoder()
        let tone = [Int16](repeating: 1_500, count: OpusCodec.frameSamples)
        let p1 = try enc.encode(tone)
        socket.emitAudioFrame(WSSFrameCodec.frame(seq: 1, payload: p1))
        await waitUntil { standbyFresh == 1 }
        XCTAssertEqual(standbyFresh, 1, "genuine standby frames prove freshness")
        XCTAssertEqual(graph.pushedFrames, 0, "a standby socket NEVER renders")
        XCTAssertEqual(graph.startCount, 0, "a standby socket never opens the capture graph")
        media.close()
    }

    func testStandbySilencePulseUsesTheLiveEncoder() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        try await connect(media, via: socket)
        let before = socket.sentAudioFrames.count
        media.markHotStandby()
        await waitUntil { socket.sentAudioFrames.count > before }
        let pulse = try XCTUnwrap(socket.sentAudioFrames.last)
        XCTAssertNotNil(WSSFrameCodec.parse(pulse),
                        "the silence pulse is a real seq16 frame through this socket's encoder")
        media.close()
    }

    /// Build-44/50 parity: a standby send failure reports an honest
    /// disconnection; the call-level non-fatality is the coordinator's
    /// standby identity fence (the media never self-promotes a failure).
    func testStandbySendFailureReportsDisconnected() async throws {
        let socket = FakeQUICMediaSocket()
        let graph = FakeAudioGraph()
        var states: [MediaState] = []
        let media = QUICCallMedia(socketFactory: { _ in socket }, audioGraph: graph)
        media.onState = { states.append($0) }
        try await connect(media, via: socket)
        socket.sendAudioError = URLError(.networkConnectionLost)
        media.markHotStandby()
        await waitUntil(timeout: 2) { !media.connectedForTest() }
        XCTAssertEqual(states.last, .disconnected,
                       "a dead standby socket reports disconnection, never a call failure")
        media.close()
    }
}

// MARK: - QUIC capability / punch REST surface (build 61)

@MainActor
final class QUICMediaEndpointTests: XCTestCase {
    private var server: TestHTTPServer!

    override func setUp() async throws {
        try await super.setUp()
        server = TestHTTPServer()
        try server.start()
    }

    override func tearDown() async throws {
        await server.stop()
        server = nil
        try await super.tearDown()
    }

    private func makeClient() throws -> HTTPGatewayAPI {
        guard case .success(let origin) = GatewayOrigin.validate(
            "http://127.0.0.1:\(server.port)", allowLoopbackHTTP: true, apiVersion: "v2"
        ) else { throw NSError(domain: "test", code: 1) }
        let store = TokenStore(keychain: DictionaryKeychain())
        try store.save(TokenSet(accessToken: "access", refreshToken: "refresh", deviceId: "device-1"))
        return HTTPGatewayAPI(origin: origin, tokens: store, configuration: server.configuration)
    }

    /// Build-62: the response body is the REAL Go-generated pin document
    /// (gateway/cmd/pinfixture) — production-shaped `version/gatewayId/
    /// algorithm/spkiHash/notBefore/notAfter/signature/identityKey/
    /// identityFingerprint`, with the certificate window in UNIX SECONDS.
    private func quicInfoJSON() -> String {
        QUICPinFixture.validDocument
    }

    func testQUICMediaInfoEndpoint() async throws {
        server.respond(with: 200, body: quicInfoJSON())
        let client = try makeClient()
        let info = try await client.quicMediaInfo()
        XCTAssertEqual(server.lastPath, "/api/v2/media/quic")
        XCTAssertEqual(server.lastMethod, "GET")
        XCTAssertEqual(server.lastAuthorization, "Bearer access")
        XCTAssertTrue(info.enabled)
        XCTAssertEqual(info.alpn, "callrelay-media")
        // The REAL gateway pin shape (build-62 wire fix): spkiHash/algorithm
        // plus version, gatewayId and BOTH unix-second bounds.
        let pin = try XCTUnwrap(info.pin)
        XCTAssertEqual(pin.version, 1)
        XCTAssertEqual(pin.gatewayId, "gateway-quicdirect-fixture")
        XCTAssertEqual(pin.algorithm, "ecdsa-p256-spki-sha256")
        XCTAssertEqual(info.pinHash, Data(base64Encoded: "4d79DGcwBi5HZh2+GEnT37GV+N4jg7+I1Jur3xcCFkI="))
        XCTAssertEqual(info.pinSignature, Data(base64Encoded:
            "T5P7yUd/qtfBUnj6ty/QxaxLJim9/pMatb0ncUNt9HG8QANFwfFSnTbb22rj+/QRsdpElBANyzhoaYIEQWePAg=="))
        XCTAssertEqual(info.identityFingerprint, QUICPinFixture.fingerprint)
        XCTAssertEqual(info.notBefore, 1_735_689_600)
        XCTAssertEqual(info.notAfter, 1_893_456_000)
        XCTAssertEqual(info.candidates.count, 1)
        XCTAssertEqual(info.candidates.first?.type, "srflx")
        XCTAssertEqual(info.candidates.first?.hostPort, HostPort(host: "192.0.2.10", port: 8792))
        XCTAssertEqual(info.stun, ["192.0.2.53:3478"])
    }

    func testQUICMediaInfoDecodesEnabledFalseTruthfully() async throws {
        server.respond(with: 200, body: #"{"enabled":false,"candidates":[]}"#)
        let client = try makeClient()
        let info = try await client.quicMediaInfo()
        XCTAssertFalse(info.enabled)
        XCTAssertTrue(info.pinHash.isEmpty)
        XCTAssertTrue(info.candidates.isEmpty)
    }

    func testPostQUICCandidateEndpoint() async throws {
        server.respond(with: 200, body: """
            {"punchToken":"pt-1","gatewayCandidate":"203.0.113.9:8792",
             "punchAtMs":1730444400500,"deadlineMs":1730444408500}
            """)
        let client = try makeClient()
        let punch = try await client.postQUICCandidate(
            callId: "call-1", candidate: "198.51.100.7:51000", networkEpoch: "wifi-7")
        XCTAssertEqual(server.lastPath, "/api/v2/calls/call-1/quic/candidate")
        XCTAssertEqual(server.lastMethod, "POST")
        XCTAssertEqual(server.lastAuthorization, "Bearer access")
        XCTAssertNotNil(server.lastIdempotencyKey)
        let body = try XCTUnwrap(server.lastBody)
        XCTAssertTrue(body.contains("\"candidate\":\"198.51.100.7:51000\""), "body carries the phone's srflx candidate: \(body)")
        XCTAssertTrue(body.contains("\"networkEpoch\":\"wifi-7\""), "body carries the network epoch: \(body)")
        XCTAssertEqual(punch.punchToken, "pt-1")
        XCTAssertEqual(punch.gatewayHostPort, HostPort(host: "203.0.113.9", port: 8792))
        XCTAssertEqual(punch.punchAtMs, 1730444400500)
        XCTAssertEqual(punch.deadlineMs, 1730444408500)
    }

    func testICEConfigurationDecodesMediaQuic() throws {
        let json = """
            {"policy":"all","iceServers":[],"expiresAt":"2027-01-01T00:00:00Z",
             "mediaTransports":["ice","ws","quic"],"mediaStandby":true,"mediaQuic":true}
            """
        let config = try JSONDecoder().decode(ICEConfiguration.self, from: Data(json.utf8))
        XCTAssertTrue(config.mediaQuic)
        XCTAssertTrue(config.mediaStandby)
        XCTAssertEqual(config.mediaTransports, ["ice", "ws", "quic"])

        // Older gateways omit both flags: the decode must not fail and the
        // defaults stay false.
        let legacy = """
            {"policy":"all","iceServers":[],"expiresAt":"2027-01-01T00:00:00Z"}
            """
        let old = try JSONDecoder().decode(ICEConfiguration.self, from: Data(legacy.utf8))
        XCTAssertFalse(old.mediaQuic)
        XCTAssertFalse(old.mediaStandby)
    }

    func testV2CapabilitiesDecodeMediaQuic() throws {
        let json = """
            {"policy":"all","iceServers":[],
             "mediaTransports":["ice","ws","quic"],
             "capabilities":{"audioDenoise":["uplink","downlink"],"mediaStandby":true,"mediaQuic":true}}
            """
        let config = try JSONDecoder().decode(V2ICEConfiguration.self, from: Data(json.utf8))
        XCTAssertEqual(config.capabilities?.mediaQuic, true)
        XCTAssertEqual(config.capabilities?.mediaStandby, true)

        let legacy = """
            {"policy":"all","iceServers":[],
             "capabilities":{"audioDenoise":["uplink"],"mediaStandby":true}}
            """
        let old = try JSONDecoder().decode(V2ICEConfiguration.self, from: Data(legacy.utf8))
        XCTAssertNil(old.capabilities?.mediaQuic)
    }
}
