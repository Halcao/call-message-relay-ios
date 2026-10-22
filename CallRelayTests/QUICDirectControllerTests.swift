import XCTest
import AVFoundation
@testable import CallRelay

// MARK: - Build-61 QUIC direct path: coordinator wiring + controller policy
//
// Covers the call-flow integration from `gateway/docs/quic61/protocol.md`:
// the bounded standby attempt (mediaReady only after the WSS greeting AND
// probe AND genuine audio), the `ready transport=quic` greeting routing on
// the WSS control plane, the fenced promote seam ordering (staged → command
// → nonce-fenced promoted reply → ownership moves, ws DEMOTED to the
// control-plane standby role — build-62), the timeout rollback (incumbent
// untouched, no audio-gap flag), forced-mode strictness (build-62: the
// user's ACTUAL mode maps relay⇒WSS-only / direct⇒direct-family / auto⇒
// full scoring), attempt-failure truthfulness, the provisional-bootstrap
// min-hold exemption, and the Q→WSS→Q round trip through the real
// coordinator seams (server swap + fresh standby tokens + authority kept).

@MainActor
final class QUICDirectControllerTests: XCTestCase {
    private let gatewayID = "gateway-quicdirect"
    private let readyURL = URLRequest(url: URL(string: "wss://example.test/media")!)

    private final class Observation {
        var published: [(decision: RouteSelectionDecision,
                         samples: [RouteCandidateKind: RouteCandidateSample])] = []
        var qualityPhases: [MediaState] = []
    }

    private struct Harness {
        let coordinator: CallCoordinator
        let api: FakeGatewayAPI
        let observation: Observation
        let wsSocket: FakeMediaSocket
        let wsGraph: Graph
        let quicSocket: FakeQUICMediaSocket
        let quicGraph: Graph
        /// The endpoint bundle the production socket factory was asked to
        /// dial (build-62: proves QUIC binds the STUN LOCAL port, not the
        /// mapped srflx address).
        let quicEndpointBox: EndpointBox
    }

    /// Thread-safe box for the socket factory's endpoint argument.
    private final class EndpointBox: @unchecked Sendable {
        private let lock = NSLock()
        private var value: QUICCallMedia.QUICSocketEndpoint?
        func set(_ endpoint: QUICCallMedia.QUICSocketEndpoint) {
            lock.lock(); value = endpoint; lock.unlock()
        }
        var endpoint: QUICCallMedia.QUICSocketEndpoint? {
            lock.lock(); defer { lock.unlock() }
            return value
        }
    }

    private func makeHarness() -> Harness {
        let api = FakeGatewayAPI()
        api.iceConfigOverride = ICEConfiguration(
            policy: "all", iceServers: [],
            expiresAt: "2026-10-01T00:00:00Z",
            mediaTransports: ["ws", "quic"], mediaQuic: true)
        api.mediaWSRequestOverride = readyURL
        // Build-62: REAL Go-signed pin fixture (see QUICPinFixture); the
        // pre-62 fictional `alg/hash` JSON is gone.
        api.quicInfoResult = .success(try! QUICPinFixture.decode(
            QUICPinFixture.validDocument))
        api.identityResponse = QUICPinFixture.identityResponse
        api.quicCandidateResult = .success(QUICPunchInfo(
            punchToken: "ptok", gatewayCandidate: "203.0.113.7:4433",
            punchAtMs: 0, deadlineMs: 8000))

        let wsSocket = FakeMediaSocket()
        wsSocket.scripted = [
            .message(.success(.string(#"{"type":"ready","epoch":2}"#))),
            .park,
        ]
        let wsGraph = Graph()
        let quicSocket = FakeQUICMediaSocket()
        let quicGraph = Graph()
        let endpointBox = EndpointBox()
        let observation = Observation()
        let coordinator = CallCoordinator(
            api: api, callKit: FakeCallKit(),
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            mediaRecoveryWindow: 45, gatewayID: gatewayID,
            wsMediaFactory: {
                WebSocketCallMedia(
                    socketFactory: { _, _ in wsSocket }, audioGraph: wsGraph)
            },
            routeProbeFactory: { FakeDirectProbe() },
            quicMediaFactory: {
                QUICCallMedia(
                    socketFactory: { endpoint in
                        endpointBox.set(endpoint)
                        return quicSocket
                    },
                    audioGraph: quicGraph,
                    handshakeTimeout: 0.5, bindTimeout: 0.5)
            })
        // Build-62 trust anchor: the pairing binding's gateway id +
        // fingerprint, AppModel-injected in production.
        coordinator.quicTrustedGatewayIdentity = (
            gatewayId: QUICPinFixture.gatewayId,
            fingerprint: QUICPinFixture.fingerprint)
        // Build-62 local/srflx separation fixture: the OBSERVED local
        // endpoint and the MAPPED srflx differ (NAT-translated port), as on
        // a real carrier NAT. QUIC must bind 51000; the gateway must be
        // told 54321.
        coordinator.quicSTUNResolver = { _, _ in
            STUNBindingResult(
                local: HostPort(host: "192.0.2.5", port: 51000),
                mapped: HostPort(host: "198.51.100.9", port: 54321))
        }
        coordinator.quicPromoteReplyTimeout = 0.4
        coordinator.quicEvaluationTickInterval = 0.1
        coordinator.quicAttemptTimeoutForTest = 30
        coordinator.onRouteSelection = { decision, samples, _ in
            observation.published.append((decision, samples))
        }
        coordinator.onQuality = { quality in
            observation.qualityPhases.append(quality.phase)
        }
        return Harness(
            coordinator: coordinator, api: api, observation: observation,
            wsSocket: wsSocket, wsGraph: wsGraph,
            quicSocket: quicSocket, quicGraph: quicGraph,
            quicEndpointBox: endpointBox)
    }

    override func setUp() {
        super.setUp()
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
    }

    override func tearDown() {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        AudioSessionBridge.shared.didDeactivate(AVAudioSession.sharedInstance())
        super.tearDown()
    }

    // MARK: Drivers

    private func startOutgoing(_ h: Harness) async {
        let uuid = UUID()
        let record = makeCallRecord(id: uuid.uuidString.lowercased(), state: .outgoingDialing)
        h.api.dialResult = .success(record)
        h.api.activeRecordForPoll = makeCallRecord(id: record.id, state: .active)
        h.coordinator.startOutgoing(peer: "555-0142", uuid: uuid)
        // WSS relay attaches (scripted ready), then the QUIC attempt dials.
        await waitUntil(timeout: 3) {
            h.quicSocket.startedCount == 1 && h.coordinator.debugQUICDirect != nil
        }
        // Build-62 wire contract, asserted on the live attempt:
        // (a) the punch POST carries the MAPPED srflx candidate, never the
        //     local endpoint;
        // (b) the QUIC socket is asked to bind the OBSERVED LOCAL port
        //     (NAT-translated mapping ports are not local interfaces);
        // (c) the pin reaching the socket is the signature-verified one —
        //     UNIX SECONDS (a ms reading would land in 1970) with both
        //     bounds retained.
        XCTAssertEqual(h.api.quicCandidatePosts.first?.candidate, "198.51.100.9:54321")
        guard let endpoint = h.quicEndpointBox.endpoint else {
            XCTFail("the QUIC dial must reach the socket factory")
            return
        }
        guard case .hostPort(let localHost, let localPort) = endpoint.localEndpoint else {
            XCTFail("QUIC must bind the STUN local endpoint")
            return
        }
        XCTAssertEqual("\(localHost)", "192.0.2.5")
        XCTAssertEqual(localPort.rawValue, 51000)
        XCTAssertEqual(endpoint.pin.notAfter, Date(timeIntervalSince1970: 1_893_456_000))
        XCTAssertEqual(endpoint.pin.notBefore, Date(timeIntervalSince1970: 1_735_689_600))
    }

    /// Answers one relay pong so the WSS sample exists (fresh ping RTT).
    private func answerOneRelayPong(_ h: Harness, holdNanoseconds: UInt64 = 15_000_000) async {
        await answerRelayPongs(h, count: 1, holdNanoseconds: holdNanoseconds)
    }

    /// Answers `count` relay pongs, each matching a FRESH ping driven
    /// through the REAL production `sendPing` (the hosted environment
    /// under-fires 1 s run-loop timers; stale re-answers would target an
    /// already-consumed t and be dropped). Each answered pong produces a
    /// distinct PingSample so the policy's distinct-sample streak can grow.
    private func answerRelayPongs(_ h: Harness, count: Int, holdNanoseconds: UInt64 = 15_000_000) async {
        var lastAnsweredTag: Int?
        for _ in 0..<count {
            let before = lastAnsweredTag
            h.coordinator.debugWSMediaForTest?.sendPingForTest()
            await waitUntil(timeout: 3) {
                guard let ping = self.lastPingJSON(h) else { return false }
                return self.pingTag(of: ping) != before
            }
            guard let ping = lastPingJSON(h) else { return }
            let t = self.pingTag(of: ping) ?? 1
            lastAnsweredTag = t
            try? await Task.sleep(nanoseconds: holdNanoseconds)
            h.wsSocket.deliver(.success(.string(
                #"{"type":"pong","t":\#(t),"buf":2}"#)))
            await pumpMainActor(10)
        }
    }

    private func lastPingJSON(_ h: Harness) -> String? {
        h.wsSocket.sends.reversed().compactMap {
            if case .string(let text) = $0.message, text.contains("\"ping\"") { return text }
            return nil
        }.first
    }

    private func pingTag(of pingJSON: String) -> Int? {
        guard let range = pingJSON.range(of: "\"t\":") else { return nil }
        let tail = pingJSON[range.upperBound...]
        let digits = tail.prefix { $0.isNumber }
        return Int(digits)
    }

    /// Delivers the WSS `ready transport=quic` greeting (PCMU so the test
    /// frames decode), then TWO epoch-valid audio frames: the first settles
    /// the connect's bind proof (consumed by the awaiter while the
    /// connection fence still says not-connected), the second is the first
    /// DECODED frame — the fresh-media proof.
    private func proveQUICGreetingAndMedia(_ h: Harness, epoch: UInt64 = 7) async {
        h.quicSocket.emitState(.ready)
        await pumpMainActor(10)
        h.wsSocket.deliver(.success(.string(
            #"{"type":"ready","transport":"quic","epoch":\#(epoch),"token":"tok","standby":true}"#)))
        await pumpMainActor(10)
        h.quicSocket.emitAudioEnvelope(epoch: epoch, frame: Data(count: 160))
        await pumpMainActor(10)
        h.quicSocket.emitAudioEnvelope(epoch: epoch, frame: Data(count: 160))
        await pumpMainActor(10)
    }

    /// Echoes a FRESH probe with a scripted RTT: drives the REAL
    /// production `sendProbe` once (the hosted environment under-fires 1 s
    /// run-loop timers, so waiting for the timer would replay an
    /// already-consumed tag and be dropped by the pending-table fence),
    /// captures the fresh tag and answers it after the scripted delay.
    /// Skips the round when no fresh tag is produced (fixture limitation,
    /// reported by the callers' bounded loops) — never replays an old tag.
    private func echoQUICProbe(_ h: Harness, rttNanoseconds: UInt64) async {
        let before = h.quicSocket.sentProbes.count
        h.coordinator.debugQUICDirect?.quicCallMedia?.sendProbeForTest()
        await waitUntil(timeout: 3) { h.quicSocket.sentProbes.count > before }
        guard h.quicSocket.sentProbes.count > before,
              let tag = h.quicSocket.sentProbes.last else { return }
        try? await Task.sleep(nanoseconds: rttNanoseconds)
        h.quicSocket.emitProbeResponse(tag, arrivedAt: Date())
        await pumpMainActor(10)
    }

    private func promoteCommands(_ h: Harness) -> [String] {
        h.wsSocket.sends.compactMap {
            if case .string(let text) = $0.message, text.contains("\"promote\"") {
                return text
            }
            return nil
        }
    }

    private func nonce(ofPromoteJSON json: String) -> String {
        let marker = "\"nonce\":\""
        guard let start = json.range(of: marker) else { return "" }
        let rest = json[start.upperBound...]
        guard let end = rest.range(of: "\"") else { return "" }
        return String(rest[..<end.lowerBound])
    }

    // MARK: 1. Attempt success path: mediaReady only after transport ready
    // + first probe (+ genuine inbound audio as the freshness proof).

    func testAttemptSuccessMediaReadyOnlyAfterGreetingProbeAndRealMedia() async throws {
        // Forced relay keeps the QUIC path a permanent standby: this test
        // observes the readiness discipline without the auto-promotion
        // (which would roll back and close the socket mid-test).
        MediaRoutePreferenceStore.shared.setMode(.relay, for: gatewayID)
        let h = makeHarness()
        await startOutgoing(h)
        // Give the relay a sample so mixed evaluations are observable.
        await answerOneRelayPong(h)
        let controller = h.coordinator.debugQUICDirect!

        // Dial in flight: no greeting, no sample, no evaluation tick.
        XCTAssertFalse(controller.debugTransportReady)
        XCTAssertFalse(controller.debugEvaluationRunning)
        XCTAssertFalse(h.observation.published.contains { $0.samples[.quicDirect] != nil })

        // Handshake done; greeting not yet applied.
        h.quicSocket.emitState(.ready)
        await pumpMainActor(10)
        XCTAssertFalse(controller.debugTransportReady)
        XCTAssertFalse(h.observation.published.contains { $0.samples[.quicDirect] != nil })

        // Greeting applied: epoch bound, but the connect still waits for the
        // bind proof and no probe exists yet — still no QUIC sample.
        h.wsSocket.deliver(.success(.string(
            #"{"type":"ready","transport":"quic","epoch":7,"token":"tok","standby":true}"#)))
        await pumpMainActor(10)
        XCTAssertTrue(controller.debugTransportReady)
        XCTAssertFalse(controller.debugMediaProven)
        // The tick may already run (relay evidence starts it); the QUIC
        // SAMPLE still requires the first probe echo.
        XCTAssertFalse(h.observation.published.contains { $0.samples[.quicDirect] != nil })

        // First epoch-valid audio: the bind proof — the connect settles,
        // but this frame is consumed by the awaiter, not decoded.
        h.quicSocket.emitAudioEnvelope(epoch: 7, frame: Data(count: 160))
        await pumpMainActor(10)
        XCTAssertEqual(controller.quicCallMedia?.state, .connected)
        XCTAssertFalse(controller.debugMediaProven)
        XCTAssertFalse(h.observation.published.contains { $0.samples[.quicDirect] != nil })

        // Second epoch-valid audio: the first DECODED frame = fresh-media
        // proof. The evaluation tick may now run, but the QUIC sample still
        // requires a probe echo — no QUIC sample is published yet.
        h.quicSocket.emitAudioEnvelope(epoch: 7, frame: Data(count: 160))
        await pumpMainActor(10)
        if !controller.debugMediaProven {
            XCTFail("no media proof: epoch=\(h.quicSocket.expectedEpoch) transportReady=\(controller.debugTransportReady) mediaState=\(String(describing: controller.quicCallMedia?.state))")
        }
        XCTAssertFalse(h.observation.published.contains { $0.samples[.quicDirect] != nil })

        // First probe echo: NOW the path is decision-ready and the sample
        // carries mediaReady == true (greeting + probe + genuine audio).
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)
        await waitUntil(timeout: 3) {
            h.observation.published.contains { $0.samples[.quicDirect] != nil }
        }
        let sample = h.observation.published.last { $0.samples[.quicDirect] != nil }!
            .samples[.quicDirect]!
        if !sample.mediaReady {
            XCTFail("QUIC sample not media-ready: proven=\(controller.debugMediaProven) ready=\(controller.debugTransportReady) rtt=\(sample.rttMs) id=\(sample.sampleID)")
        }
        XCTAssertTrue(sample.available)
        if sample.confidence <= 0.4 {
            XCTFail("low confidence: \(sample.confidence) rtt=\(sample.rttMs) id=\(sample.sampleID) measuredAt=\(sample.measuredAt)")
        }
        // Build-62 evidence honesty: the QUIC sample carries the REAL
        // receiver-side measurement flags (framed seq16 downlink is
        // measurable; nothing fabricated).
        XCTAssertTrue(sample.hasJitterMeasurement)
        XCTAssertTrue(sample.hasQueueEstimate)
        XCTAssertTrue(controller.debugEvaluationRunning)
        h.coordinator.handleProviderReset()
    }

    // MARK: 2. The WSS `ready transport=quic` greeting is routed to the QUIC
    // controller, never consumed as the WSS socket's own ready.

    func testQUICGreetingIsNotTheWSSOwnReady() async throws {
        // Socket level: a quic greeting arriving BEFORE the ws ready must not
        // complete the handshake; the real ready afterwards does.
        let socket = FakeMediaSocket()
        socket.scripted = [
            .message(.success(.string(
                #"{"type":"ready","transport":"quic","codec":"opus","epoch":7,"token":"t"}"#))),
            .message(.success(.string(#"{"type":"ready","epoch":2}"#))),
            .park,
        ]
        let media = WebSocketCallMedia(
            socketFactory: { _, _ in socket }, audioGraph: Graph())
        var foreign: [WSMediaControl] = []
        media.onTransportControl = { foreign.append($0) }
        try await media.connect(request: URLRequest(url: URL(string: "wss://x.test/media")!))
        XCTAssertEqual(foreign.count, 1)
        XCTAssertEqual(foreign.first?.transport, "quic")
        // The greeting's codec was NOT adopted by the ws socket.
        XCTAssertFalse(media.usesOpusForTest)
        XCTAssertEqual(media.state, .connected)

        // Legacy promote JSON is unchanged without the transport marker.
        media.sendPromoteCommand(token: "tk", expectEpoch: 3, nonce: "n1")
        if case .string(let text) = socket.sends.last!.message {
            XCTAssertTrue(text.contains("\"type\":\"promote\""))
            XCTAssertFalse(text.contains("transport"))
        } else {
            XCTFail("promote command must be a text frame")
        }
        media.close()
    }

    func testCoordinatorRoutesQUICGreetingToController() async throws {
        let h = makeHarness()
        await startOutgoing(h)
        await proveQUICGreetingAndMedia(h)
        // The greeting reached the QUIC controller...
        let controller = h.coordinator.debugQUICDirect!
        XCTAssertTrue(controller.debugTransportReady)
        XCTAssertEqual(controller.standbyToken, "tok")
        XCTAssertTrue(controller.debugMediaProven)
        // ...and the ws control plane is untouched: pings keep flowing, the
        // socket is not cancelled, no failure state surfaced.
        XCTAssertFalse(h.wsSocket.cancelled)
        XCTAssertFalse(h.observation.qualityPhases.contains(.failed))
        XCTAssertFalse(h.observation.qualityPhases.contains(.disconnected))
        h.coordinator.handleProviderReset()
    }

    // MARK: 3. Promote seam ordering: staged → command (transport quic,
    // fenced token/epoch) → nonce-fenced promoted reply → ownership moves,
    // ws DEMOTED to the control-plane standby role (build-62: not retired —
    // the QUIC design relies on the WSS plane for reliable critical control
    // and the demoted standby is the only relay telemetry feed).

    func testPromoteQUICDirectOrderingAndOwnershipMove() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.quic, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let h = makeHarness()
        // The promote reconcile reads the demoted ws standby identity from
        // the server snapshot.
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)

        // The forced-QUIC evaluation proposes and the coordinator seam sends
        // the fenced promote command on the WSS primary socket.
        await waitUntil(timeout: 3) { !self.promoteCommands(h).isEmpty }
        let command = promoteCommands(h).first!
        XCTAssertTrue(command.contains("\"transport\":\"quic\""))
        XCTAssertTrue(command.contains("\"token\":\"tok\""))
        XCTAssertTrue(command.contains("\"expectEpoch\":7"))

        // Staged discipline BEFORE the reply: the QUIC graph has not started
        // and the ws primary still owns/runs audio.
        XCTAssertEqual(h.quicGraph.startCount, 0)
        XCTAssertTrue(h.wsGraph.isRunning)
        XCTAssertTrue(h.coordinator.debugQUICDirect!.quicCallMedia!.isStagedForHandover)

        // The gateway acks with the fenced promoted reply (new epoch).
        let nonce = nonce(ofPromoteJSON: command)
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":8,"nonce":"\#(nonce)","primary":"quic","standby":"ws"}"#)))
        await waitUntil(timeout: 3) {
            h.quicGraph.startCount == 1
        }
        await pumpMainActor(10)

        // Ownership moved: QUIC owns audio (graph started, staged cleared,
        // epoch re-bound from the promoted reply). The ws socket is DEMOTED,
        // NOT retired: connection alive, graph stopped, pings keep running —
        // it is the control plane + the relay sample source.
        let controller = h.coordinator.debugQUICDirect!
        XCTAssertFalse(controller.quicCallMedia!.isStagedForHandover)
        XCTAssertEqual(h.quicSocket.expectedEpoch, 8)
        XCTAssertGreaterThanOrEqual(h.wsGraph.stopCount, 1)
        XCTAssertFalse(h.wsGraph.isRunning)
        XCTAssertFalse(h.wsSocket.cancelled, "the demoted ws control plane must stay connected")
        XCTAssertTrue(controller.debugConfirmedOwner)
        XCTAssertEqual(controller.debugIncumbent, .quicDirect)
        // The decision published only AFTER the confirmed switch.
        let decision = h.observation.published.last!.decision
        XCTAssertEqual(decision.chosen, .quicDirect)
        XCTAssertEqual(decision.reason, .forcedMode)
        h.coordinator.handleProviderReset()
    }

    // MARK: 4. Promote timeout → rollback: ws stays primary, the pending
    // switch is cancelled, no audio-gap flag, recovery NOT armed.

    func testPromoteTimeoutRollsBackWithoutAudioGap() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.quic, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let h = makeHarness()
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)

        await waitUntil(timeout: 3) { !self.promoteCommands(h).isEmpty }
        // Never answer the promoted reply: the bounded timeout rolls back.
        await waitUntil(timeout: 3) { h.quicSocket.closed }

        // The QUIC candidate closed without audio; the ws primary keeps the
        // call untouched (graph still running, socket not retired).
        XCTAssertTrue(h.wsGraph.isRunning)
        XCTAssertFalse(h.wsSocket.cancelled)
        // No failure/gap state leaked to the UI quality channel.
        XCTAssertFalse(h.observation.qualityPhases.contains(.disconnected))
        XCTAssertFalse(h.observation.qualityPhases.contains(.failed))
        // Recovery is NOT triggered while the relay owns audio, and the
        // failed promotion surfaces as a truthful forcedUnavailable keep.
        XCTAssertFalse(h.coordinator.debugRecoveryActive)
        await waitUntil(timeout: 3) {
            h.observation.published.contains { $0.decision.reason == .forcedUnavailable }
        }
        let keep = h.observation.published.last { $0.decision.reason == .forcedUnavailable }!
        XCTAssertEqual(keep.decision.chosen, .wssRelay)
        // The pending switch was CANCELLED (not left outstanding): a stuck
        // proposal would surface as .switchPending keeps, which never come.
        await pumpMainActor(20)
        XCTAssertFalse(h.observation.published.contains { $0.decision.reason == .switchPending })
        h.coordinator.handleProviderReset()
    }

    // MARK: 5. Forced .quic with no proven QUIC media keeps the incumbent
    // relay strictly (forcedUnavailable) — never a silent forced move.

    func testForcedQUICUnavailableKeepsIncumbent() async throws {
        let observation = Observation()
        let controller = QUICDirectController(
            wsMedia: { nil },
            preferredTransport: { .quic },
            promote: { _ in false },
            publish: { decision, samples, _ in
                observation.published.append((decision, samples))
            })
        // No QUIC media, no samples at all: the forced candidate is unusable
        // and the relay safe-default is proposed WITHOUT being adopted as a
        // confirmed switch (provisional strictness).
        controller.debugEvaluateForTest()
        XCTAssertEqual(observation.published.count, 1)
        let decision = observation.published.first!.decision
        XCTAssertEqual(decision.chosen, .wssRelay)
        XCTAssertEqual(decision.reason, .forcedUnavailable)
        XCTAssertNil(controller.debugIncumbent)
    }

    // MARK: 5b. Build-62 trust gate: when the pin cannot be verified against
    // the pairing-trusted identity, QUIC stays CLOSED — no socket, no dial,
    // incumbent relay untouched. Both failure modes (missing anchor, live
    // key mismatch) are covered.

    func testAttemptAbortsWhenTrustedIdentityMissing() async throws {
        let h = makeHarness()
        h.coordinator.quicTrustedGatewayIdentity = nil
        await startOutgoingAllowNoDial(h)
        XCTAssertEqual(h.quicSocket.startedCount, 0)
        XCTAssertNil(h.quicEndpointBox.endpoint)
        XCTAssertTrue(h.api.quicCandidatePosts.isEmpty)
        // Build-62 single-authority safety: the abort returns the legacy
        // RTC/WSS routing authority (never frozen by a dead QUIC attempt).
        XCTAssertFalse(h.coordinator.debugRouteAutomaticStandDown)
        XCTAssertNil(h.coordinator.debugQUICDirect)
        // The call itself is healthy on the relay.
        XCTAssertFalse(h.observation.qualityPhases.contains(.failed))
        h.coordinator.handleProviderReset()
    }

    func testAttemptAbortsWhenLiveKeyDoesNotMatchBinding() async throws {
        let h = makeHarness()
        // The gateway serves a DIFFERENT (attacker) key while the binding
        // expects the fixture fingerprint: derivation must fail closed.
        h.api.identityResponse = IdentityResponse(
            gatewayId: QUICPinFixture.gatewayId,
            gatewayName: nil, mode: nil, transport: nil, apiVersion: nil,
            publicKey: Data(repeating: 0xCD, count: 32).base64EncodedString(),
            fingerprint: QUICPinFixture.fingerprint)
        await startOutgoingAllowNoDial(h)
        XCTAssertEqual(h.quicSocket.startedCount, 0)
        XCTAssertNil(h.quicEndpointBox.endpoint)
        XCTAssertTrue(h.api.quicCandidatePosts.isEmpty)
        XCTAssertFalse(h.coordinator.debugRouteAutomaticStandDown)
        XCTAssertNil(h.coordinator.debugQUICDirect)
        h.coordinator.handleProviderReset()
    }

    /// Early-abort matrix (build-62): a QUIC capability that is off, and a
    /// STUN/punch failure on the common public NAT, both return the legacy
    /// RTC/WSS routing authority — auto-routing never freezes.
    func testAttemptAbortMatrixRestoresLegacyAuthority() async throws {
        // (a) capability disabled.
        let disabled = makeHarness()
        disabled.api.quicInfoResult = .success(try! QUICPinFixture.decode(
            #"{"enabled":false,"candidates":[]}"#))
        await startOutgoingAllowNoDial(disabled)
        XCTAssertEqual(disabled.quicSocket.startedCount, 0)
        XCTAssertFalse(disabled.coordinator.debugRouteAutomaticStandDown)
        XCTAssertNil(disabled.coordinator.debugQUICDirect)
        disabled.coordinator.handleProviderReset()

        // (b) STUN unreachable (the common carrier-NAT failure): info is
        // fine, trust passes, the punch candidate is never posted.
        let noStun = makeHarness()
        noStun.coordinator.quicSTUNResolver = { _, _ in nil }
        await startOutgoingAllowNoDial(noStun)
        XCTAssertEqual(noStun.quicSocket.startedCount, 0)
        XCTAssertTrue(noStun.api.quicCandidatePosts.isEmpty)
        XCTAssertFalse(noStun.coordinator.debugRouteAutomaticStandDown)
        XCTAssertNil(noStun.coordinator.debugQUICDirect)
        noStun.coordinator.handleProviderReset()

        // (c) punch POST rejected (token window exhausted): no dial, legacy
        // authority restored.
        let punchRejected = makeHarness()
        punchRejected.api.quicCandidateResult = .failure(APIError.network(URLError(.cannotConnectToHost)))
        await startOutgoingAllowNoDial(punchRejected)
        XCTAssertEqual(punchRejected.quicSocket.startedCount, 0)
        XCTAssertFalse(punchRejected.coordinator.debugRouteAutomaticStandDown)
        XCTAssertNil(punchRejected.coordinator.debugQUICDirect)
        punchRejected.coordinator.handleProviderReset()
    }

    /// Drives an outgoing call but tolerates the QUIC attempt never dialing
    /// (trust gate): waits for the attempt task to settle instead of the
    /// socket starting.
    private func startOutgoingAllowNoDial(_ h: Harness) async {
        let uuid = UUID()
        let record = makeCallRecord(id: uuid.uuidString.lowercased(), state: .outgoingDialing)
        h.api.dialResult = .success(record)
        h.api.activeRecordForPoll = makeCallRecord(id: record.id, state: .active)
        h.coordinator.startOutgoing(peer: "555-0142", uuid: uuid)
        // The attempt task runs (info + trust + STUN + punch) then either
        // dials or aborts; both settle the task. Aborts also nil the
        // controller, so wait on the task alone.
        await waitUntil(timeout: 3) {
            !h.coordinator.debugQUICAttemptRunning
        }
        await pumpMainActor(10)
    }

    // MARK: 6. Attempt failure is truthful: the advisor is told, the
    // incumbent relay is untouched, recovery is NOT armed.

    func testAttemptFailureKeepsIncumbentAndArmsNoRecovery() async throws {
        let h = makeHarness()
        await startOutgoing(h)
        // The dial dies mid-handshake (TLS/punch/unreachable class).
        h.quicSocket.emitState(.failed("dial unreachable"))
        await waitUntil(timeout: 3) {
            h.coordinator.debugQUICDirect!.quicCallMedia == nil
                && !h.coordinator.debugQUICAttemptRunning
        }
        XCTAssertNil(h.coordinator.debugQUICDirect!.quicCallMedia)
        // The relay path is untouched: still attached, call still active,
        // no recovery episode, no failure publish.
        XCTAssertFalse(h.coordinator.debugRecoveryActive)
        XCTAssertFalse(h.observation.qualityPhases.contains(.failed))
        XCTAssertFalse(h.observation.qualityPhases.contains(.disconnected))
        XCTAssertFalse(h.observation.published.contains { $0.samples[.quicDirect] != nil })
        h.coordinator.handleProviderReset()
    }

    // MARK: 7. Provisional bootstrap: a relay incumbent carrying the call
    // provisionally does NOT consume the min-hold budget — the first proven
    // better QUIC route is proposed immediately (the advisor starts fresh).

    func testProvisionalBootstrapProposesQUICWithoutMinHold() async throws {
        let observation = Observation()
        var promoteCalls = 0
        let controller = QUICDirectController(
            wsMedia: { nil },
            preferredTransport: { .auto },
            promote: { _ in promoteCalls += 1; return true },
            publish: { decision, samples, _ in
                observation.published.append((decision, samples))
            })
        var clock = Date()
        controller.now = { clock }
        func sample(_ rtt: Double, _ id: UInt64, _ epoch: String) -> RouteCandidateSample {
            RouteCandidateSample(
                rttMs: rtt, jitterMs: 1, lossFraction: 0, reorderFraction: 0,
                queueCostMs: 0, available: true, confidence: 0.9,
                mediaReady: true, networkEpoch: epoch, measuredAt: clock, sampleID: id)
        }
        // Relay proves FIRST: the startup carrier publishes with NO
        // confirmed adoption (provisional bootstrap).
        clock.addTimeInterval(1)
        controller.debugInjectSamplesForTest(relay: sample(60, 1, "r"))
        controller.debugEvaluateForTest()
        let provisional = observation.published.last!.decision
        XCTAssertEqual(provisional.chosen, .wssRelay)
        XCTAssertNil(controller.debugIncumbent,
                     "the provisional relay carrier must not be a confirmed adoption")
        // QUIC proves clearly better: the very next evaluation
        // initial-selects QUIC — no minimum-hold penalty exists because
        // the relay bootstrap was never adopted (protocol §8, production
        // defaults).
        clock.addTimeInterval(1)
        controller.debugInjectSamplesForTest(
            quic: sample(40, 2, "q"), relay: sample(60, 3, "r"))
        controller.debugEvaluateForTest()
        await pumpMainActor(10)
        XCTAssertEqual(promoteCalls, 1, "a real QUIC promote commit was proposed")
        XCTAssertEqual(controller.debugIncumbent, .quicDirect)
        XCTAssertEqual(observation.published.last?.decision.chosen, .quicDirect)
    }

    // MARK: 8. Three-path authority: an ACTIVE RTC path that is faster than
    // both QUIC and relay is kept (stays RTC; no commit fires).

    func testActiveRTCFasterThanQUICAndRelayStaysRTC() async throws {
        let observation = Observation()
        var promoteCalls = 0
        var adoptRelayCalls = 0
        var adoptDirectCalls = 0
        let controller = QUICDirectController(
            wsMedia: { nil },
            preferredTransport: { .auto },
            promote: { _ in promoteCalls += 1; return false },
            adoptRelay: { adoptRelayCalls += 1; return false },
            adoptDirect: { adoptDirectCalls += 1; return false },
            rtcSample: {
                RouteCandidateSample(
                    rttMs: 30, jitterMs: 2, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: "rtc", measuredAt: Date(),
                    sampleID: 1)
            },
            publish: { decision, samples, _ in
                observation.published.append((decision, samples))
            })
        controller.seedInitialOwner(.webRTCDirect)
        // QUIC and relay prove slower — over several evaluations with
        // advancing measurements the RTC owner must be KEPT (no hysteresis
        // violation can demote a clean faster incumbent).
        var clock = Date()
        controller.now = { clock }
        for i in 0..<5 {
            controller.debugInjectSamplesForTest(quic: RouteCandidateSample(
                rttMs: 90, jitterMs: 6, lossFraction: 0, reorderFraction: 0,
                queueCostMs: 0, available: true, confidence: 0.9,
                mediaReady: true, networkEpoch: "q", measuredAt: clock,
                sampleID: UInt64(10 + i)), relay: RouteCandidateSample(
                rttMs: 70, jitterMs: 5, lossFraction: 0, reorderFraction: 0,
                queueCostMs: 0, available: true, confidence: 0.9,
                mediaReady: true, networkEpoch: "r", measuredAt: clock,
                sampleID: UInt64(20 + i)))
            clock.addTimeInterval(1)
            controller.debugEvaluateForTest()
        }
        XCTAssertEqual(promoteCalls, 0, "no QUIC commit for a losing QUIC path")
        XCTAssertEqual(adoptRelayCalls, 0, "no relay commit for a losing relay")
        XCTAssertEqual(adoptDirectCalls, 0, "RTC is already the owner")
        let last = observation.published.last?.decision
        XCTAssertEqual(last?.chosen, .webRTCDirect)
        XCTAssertEqual(controller.debugIncumbent, .webRTCDirect)
    }

    // MARK: 9. Q→WSS: after QUIC is CONFIRMED, relay becoming best goes
    // through the real target-specific commit (adoptRelay), confirm and
    // publish — the startup-only provisional exemption no longer applies.

    func testRelayBecomesBestAfterQUICConfirmedUsesRelayCommit() async throws {
        let observation = Observation()
        var adoptRelayCalls = 0
        let controller = QUICDirectController(
            wsMedia: { nil },
            preferredTransport: { .auto },
            promote: { _ in true },
            adoptRelay: { adoptRelayCalls += 1; return true },
            publish: { decision, samples, _ in
                observation.published.append((decision, samples))
            })
        // QUIC is the confirmed owner.
        controller.seedInitialOwner(.quicDirect)
        var clock = Date()
        controller.now = { clock }
        // QUIC degraded: 200 ms + loss; relay clean at 60 ms. The sustained
        // gain rule (production defaults) needs distinct samples spanning
        // the minimum duration: advance the clock BEFORE each evaluation so
        // the injected measurement timestamps stay coherent with the
        // evaluation clock (build-62: real measuredAt is never re-stamped;
        // a sample dated before a 21 s jump would be stale).
        for (i, step) in [21.0, 4, 4].enumerated() {
            clock.addTimeInterval(step)
            controller.debugInjectSamplesForTest(quic: RouteCandidateSample(
                rttMs: 200, jitterMs: 20, lossFraction: 0.08, reorderFraction: 0,
                queueCostMs: 0, available: true, confidence: 0.9,
                mediaReady: true, networkEpoch: "q", measuredAt: clock,
                sampleID: UInt64(30 + i)), relay: RouteCandidateSample(
                rttMs: 60, jitterMs: 4, lossFraction: 0, reorderFraction: 0,
                queueCostMs: 0, available: true, confidence: 0.9,
                mediaReady: true, networkEpoch: "r", measuredAt: clock,
                sampleID: UInt64(40 + i)))
            controller.debugEvaluateForTest()
            await pumpMainActor(20)
        }
        XCTAssertGreaterThanOrEqual(adoptRelayCalls, 1,
                                    "relay adoption must use the target-specific commit")
        XCTAssertEqual(controller.debugIncumbent, .wssRelay)
        let adopted = observation.published.last { $0.decision.chosen == .wssRelay }
        XCTAssertNotNil(adopted, "diagnostics must show the adopted relay transport")
    }

    // MARK: 10. Q→RTC→WSS→Q with the production hold/cooldown discipline:
    // every move goes through its target-specific commit and the clock
    // advances satisfy min-hold/cooldown honestly.

    func testQRTCWSQChainUsesTargetCommitsWithHoldDiscipline() async throws {
        let observation = Observation()
        var commits: [String] = []
        let controller = QUICDirectController(
            wsMedia: { nil },
            preferredTransport: { .auto },
            promote: { _ in commits.append("quic"); return true },
            adoptRelay: { commits.append("relay"); return true },
            adoptDirect: { commits.append("direct"); return true },
            publish: { decision, samples, _ in
                observation.published.append((decision, samples))
            })
        controller.seedInitialOwner(.quicDirect)
        var clock = Date()
        controller.now = { clock }

        func eval(_ quicMs: Double, _ rtcMs: Double, _ relayMs: Double, _ step: TimeInterval, _ ids: UInt64) {
            clock.addTimeInterval(step)
            controller.debugInjectSamplesForTest(
                quic: RouteCandidateSample(
                    rttMs: quicMs, jitterMs: 1, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: "q", measuredAt: clock, sampleID: ids),
                rtc: RouteCandidateSample(
                    rttMs: rtcMs, jitterMs: 1, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: "r", measuredAt: clock, sampleID: ids + 100),
                relay: RouteCandidateSample(
                    rttMs: relayMs, jitterMs: 1, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: "w", measuredAt: clock, sampleID: ids + 200))
            controller.debugEvaluateForTest()
        }

        // Q→RTC: 21 s first (min-hold from the QUIC seed), then 4 s steps
        // so the three gains sit inside the 12 s sustained window while
        // spanning ≥ the 4 s minimum duration.
        for (i, step) in [21.0, 4, 4].enumerated() {
            eval(150, 30, 80, step, UInt64(50 + i))
        }
        for _ in 0..<6 { await pumpMainActor(10) }
        XCTAssertEqual(controller.debugIncumbent, .webRTCDirect)
        // RTC→WSS: min-hold (20 s) + cooldown (15 s) anchored at the RTC
        // adoption, then the same sustained cadence.
        for (i, step) in [21.0, 4, 4].enumerated() {
            eval(150, 90, 40, step, UInt64(60 + i))
        }
        for _ in 0..<6 { await pumpMainActor(10) }
        XCTAssertEqual(controller.debugIncumbent, .wssRelay)
        // WSS→Q: QUIC proves best again.
        for (i, step) in [21.0, 4, 4].enumerated() {
            eval(25, 90, 60, step, UInt64(70 + i))
        }
        for _ in 0..<6 { await pumpMainActor(10) }
        XCTAssertEqual(controller.debugIncumbent, .quicDirect)
        XCTAssertEqual(commits, ["direct", "relay", "quic"],
                       "every move used its target-specific commit exactly once")
        XCTAssertEqual(observation.published.last?.decision.chosen, .quicDirect,
                       "UI/diagnostics match the real adopted transport")
    }
}

// MARK: - Build-62 production round trips (real coordinator seams)

extension QUICDirectControllerTests {

    /// Production-shaped policy windows: the REAL streak/min-hold/cooldown
    /// rules run, just fast enough for a deterministic test.
    private func fastPolicyConfig() -> RouteSelectionPolicy.Config {
        RouteSelectionPolicy.Config(
            minimumHoldInterval: 0.3,
            switchCooldown: 0.3,
            sustainedWindow: 2,
            sustainedWindowSpan: 6,
            sustainedMinDuration: 0.2)
    }

    /// Q→WSS→Q through the ACTUAL coordinator seams: QUIC confirmed owner;
    /// relay proves better (real relay pings vs degraded QUIC probes); the
    /// adoptRelay seam performs the server swap (fenced REST promote) and
    /// the local make-before-break transfer; the QUIC authority is RETAINED
    /// (no legacy-selector resume); QUIC proves better again and is
    /// RE-PROMOTED with the fresh standby token learned from the swap ack.
    func testCoordinatorQToWSSToQThroughRealSeams() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.coordinator.quicPromoteReplyTimeout = 3
        // Promote reconcile: the demoted ws standby's fresh identity.
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)

        // QUIC proves ready and becomes the confirmed owner (auto mode).
        await proveQUICGreetingAndMedia(h)
        await answerOneRelayPong(h)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)
        await waitUntil(timeout: 3) { !self.promoteCommands(h).isEmpty }
        let firstPromote = self.promoteCommands(h).first!
        XCTAssertTrue(firstPromote.contains("\"transport\":\"quic\""))
        XCTAssertTrue(firstPromote.contains("\"expectEpoch\":7"))
        let firstNonce = nonce(ofPromoteJSON: firstPromote)
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":8,"nonce":"\#(firstNonce)","primary":"quic","standby":"ws"}"#)))
        await waitUntil(timeout: 3) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .quicDirect
        }
        let controller = h.coordinator.debugQUICDirect!
        XCTAssertTrue(controller.debugConfirmedOwner)
        XCTAssertFalse(h.wsSocket.cancelled, "ws demoted to control-plane standby, not retired")

        // Relay proves better: fast relay pongs (distinct samples), degraded
        // QUIC probes.
        h.api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 9, currentEpoch: nil, primary: "ws", standby: "ws",
                standbyToken: "quic-tok-2"))
        ]
        // Phase 1 — control RTT alone must NOT promote the relay: with only
        // pongs (no genuine inbound media frames) the relay candidate is
        // unproven and the QUIC incumbent holds (build-62 media proof).
        h.api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 9, currentEpoch: nil, primary: "ws", standby: "ws",
                standbyToken: "quic-tok-2"))
        ]
        var fedPingsOnly = 0
        for _ in 0..<5 {
            await answerRelayPongs(h, count: 1, holdNanoseconds: 1_000_000)
            await echoQUICProbe(h, rttNanoseconds: 250_000_000)
            await pumpMainActor(15)
            fedPingsOnly += 1
        }
        XCTAssertEqual(h.api.standbyPromoteCalls.count, 0,
                       "control RTT alone must never win a commit: \(fedPingsOnly) ping rounds without media")
        XCTAssertEqual(h.coordinator.debugQUICDirect?.debugIncumbent, .quicDirect)

        // Phase 2 — feed GENUINE inbound media frames too: now the relay is
        // media-proven and the sustained-gain rule fires the REAL adopt
        // seam. Evidence stays fresh within the policy TTL throughout.
        var committed = false
        for _ in 0..<14 where !committed {
            await answerRelayPongs(h, count: 1, holdNanoseconds: 1_000_000)
            h.wsSocket.deliver(.success(.data(PCMUCodec.encode([Int16](repeating: 7, count: 160)))))
            await echoQUICProbe(h, rttNanoseconds: 250_000_000)
            await pumpMainActor(15)
            committed = h.api.standbyPromoteCalls.count == 1
                && h.coordinator.debugQUICDirect?.debugIncumbent == .wssRelay
        }
        XCTAssertTrue(committed, "the relay sustained-gain commit never fired")
        XCTAssertEqual(h.api.standbyPromoteCalls.first?.transport, "ws")
        XCTAssertEqual(h.api.standbyPromoteCalls.first?.token, "ws-tok-fresh",
                       "the swap fences with the demoted ws standby identity")
        // QUIC audio stepped aside; the ws graph owns audio again; the QUIC
        // socket stays alive as hot standby.
        XCTAssertTrue(h.wsGraph.isRunning)
        XCTAssertFalse(h.quicSocket.closed)
        XCTAssertFalse(h.coordinator.debugQUICDirect!.debugConfirmedOwner)
        let relayAdopted = h.observation.published.last {
            $0.decision.chosen == .wssRelay && $0.decision.reason == .sustainedGain
        }
        XCTAssertNotNil(relayAdopted)

        // QUIC proves better again (fast probes vs slow relay pongs). Wait
        // for a SECOND fenced promote command (the first promoted QUIC
        // earlier in the test).
        let promoteCountBefore = self.promoteCommands(h).count
        var rePromoted = false
        for _ in 0..<12 where !rePromoted {
            // Echo every newest probe (its pending entry is only valid until
            // the 8-deep hygiene ring rolls), then answer a slow pong.
            await echoQUICProbe(h, rttNanoseconds: 12_000_000)
            await answerRelayPongs(h, count: 1, holdNanoseconds: 250_000_000)
            await pumpMainActor(15)
            rePromoted = self.promoteCommands(h).count > promoteCountBefore
        }
        if !rePromoted {
            let dump = h.observation.published.suffix(10).map {
                "\($0.decision.reason.rawValue)/\($0.decision.chosen.rawValue)->\($0.decision.proposed?.rawValue ?? "nil"): \($0.decision.detail)"
                    + " q=\($0.samples[.quicDirect].map { "\(Int($0.rttMs))ms id\($0.sampleID) m\($0.mediaReady)" } ?? "nil")"
                    + " r=\($0.samples[.wssRelay].map { "\(Int($0.rttMs))ms id\($0.sampleID) m\($0.mediaReady)" } ?? "nil")"
            }.joined(separator: " | ")
            let probeDiag = h.coordinator.debugQUICDirect?.quicCallMedia?.debugProbeDiagnosticsForTest
            XCTFail("no Q re-promotion command; probeDiag=\(String(describing: probeDiag)) census=\(DiagnosticsCensus.shared.snapshot().filter { $0.key.contains("quic") || $0.key.contains("Promotion") }.map { "\($0.key)=\($0.value)" }.sorted().joined(separator: ",")); decisions: \(dump)")
            h.coordinator.handleProviderReset()
            return
        }
        guard let rePromote = self.promoteCommands(h).last else {
            XCTFail("no Q re-promotion command")
            h.coordinator.handleProviderReset()
            return
        }
        XCTAssertTrue(rePromote.contains("\"transport\":\"quic\""))
        XCTAssertTrue(rePromote.contains("\"token\":\"quic-tok-2\""),
                      "the Q re-promotion must use the fresh standby token from the swap ack: \(rePromote)")
        XCTAssertTrue(rePromote.contains("\"expectEpoch\":9"), "\(rePromote)")
        let nonce = nonce(ofPromoteJSON: rePromote)
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":10,"nonce":"\#(nonce)","primary":"quic","standby":"ws"}"#)))
        // The SECOND reply re-binds epoch 10 — wait on THAT (the first
        // promotion already left incumbent==quicDirect behind), then for
        // the controller's confirmSwitch (async commit Task).
        await waitUntil(timeout: 5) {
            h.quicSocket.expectedEpoch == 10 && h.quicGraph.isRunning
        }
        await waitUntil(timeout: 3) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .quicDirect
                && h.coordinator.debugQUICDirect?.debugConfirmedOwner == true
        }
        if !h.quicGraph.isRunning || h.wsGraph.isRunning {
            let dump = h.observation.published.suffix(8).map {
                "\($0.decision.reason.rawValue)/\($0.decision.chosen.rawValue)"
            }.joined(separator: ",")
            let census = DiagnosticsCensus.shared.snapshot()
            let quicCensus = census.filter { $0.key.contains("quic") || $0.key.contains("Promotion") }
                .map { "\($0.key)=\($0.value)" }.joined(separator: ",")
            XCTFail("graphs: quic=\(h.quicGraph.isRunning) ws=\(h.wsGraph.isRunning) qStarts=\(h.quicGraph.startCount) wStarts=\(h.wsGraph.startCount) staged=\(h.coordinator.debugQUICDirect?.quicCallMedia?.isStagedForHandover ?? true) decisions=\(dump) census=\(quicCensus)")
        }
        h.coordinator.handleProviderReset()
    }

    // MARK: Forced-mode matrix (build-62 single authority)

    /// 中继 forced at startup: the QUIC attempt still attaches as standby
    /// (control plane), but the policy must NEVER propose QUIC/RTC — the
    /// published decisions stay relay even when QUIC proves fast.
    func testForcedRelayNeverSelectsQUIC() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.relay, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await echoQUICProbe(h, rttNanoseconds: 10_000_000)
        // Let several evaluation ticks run with QUIC provably excellent.
        await pumpMainActor(30)
        XCTAssertTrue(self.promoteCommands(h).isEmpty,
                      "forced relay must never commit QUIC")
        XCTAssertFalse(h.api.standbyPromoteCalls.contains { $0.transport == "ws" },
                       "forced relay must never adopt relay audio as a policy switch either")
        let decisions = h.observation.published.map(\.decision)
        XCTAssertFalse(decisions.contains { $0.chosen == .quicDirect },
                       "forced relay: QUIC is never the chosen path")
        h.coordinator.handleProviderReset()
    }

    // MARK: Build-64 commit/request-generation reconciliation (real seams)

    /// The reported defect, through the REAL coordinator seams: a QUIC
    /// commit in flight when the user forces RELAY must still capture the
    /// REAL adopted owner (the ack swapped the server role and audio moved
    /// — advisor, exclusive graphs and server role agree, nothing healthy
    /// quarantined), then reconcile to the user's latest choice through
    /// the NORMAL fenced relay commit.
    func testModeChangeMidQUICCommitReconcilesToRelay() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.coordinator.quicPromoteReplyTimeout = 3
        // The demoted ws standby identity: the relay commit under test reuses
        // it through the fence (no fresh staging on the shared socket).
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await answerOneRelayPong(h)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)
        // The QUIC sustained gain fires the REAL fenced promote command and
        // parks on the reply.
        await waitUntil(timeout: 3) { !self.promoteCommands(h).isEmpty }
        let command = self.promoteCommands(h).first!
        XCTAssertTrue(command.contains("\"transport\":\"quic\""))
        let nonce = nonce(ofPromoteJSON: command)

        // The user forces RELAY while the commit is in flight (through the
        // production seam — the controller reads route.routeState.mode).
        await h.coordinator.selectRouteMode(.relay)

        // The gateway acks the swap: the commit SUCCEEDS for real.
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":8,"nonce":"\#(nonce)","primary":"quic","standby":"ws"}"#)))
        let controller = h.coordinator.debugQUICDirect!
        await waitUntil(timeout: 3) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .quicDirect
        }
        // The REAL adopted owner is captured: advisor, exclusive graphs and
        // server role agree; the mid-flight mode change quarantined NOTHING.
        XCTAssertTrue(controller.debugConfirmedOwner)
        XCTAssertTrue(h.quicGraph.isRunning, "the QUIC graph owns audio after the confirmed commit")
        XCTAssertFalse(h.wsGraph.isRunning, "the ws graph suspended at the handover")
        XCTAssertFalse(h.quicSocket.closed)

        // Reconcile to the user's latest choice: forced relay is now usable
        // (fresh media-proven sample) → the NORMAL fenced relay commit.
        h.api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 9, currentEpoch: nil, primary: "ws", standby: "ws",
                standbyToken: "quic-tok-2"))
        ]
        controller.debugSuspendEvaluationTimerForTest()
        let epoch = controller.debugNetworkEpoch
        let now = Date()
        controller.debugInjectSamplesForTest(
            quic: RouteCandidateSample(
                rttMs: 200, jitterMs: 20, lossFraction: 0, reorderFraction: 0,
                queueCostMs: 0, available: true, confidence: 0.9,
                mediaReady: true, networkEpoch: epoch, measuredAt: now, sampleID: 501),
            relay: RouteCandidateSample(
                rttMs: 40, jitterMs: 3, lossFraction: 0, reorderFraction: 0,
                queueCostMs: 0, available: true, confidence: 0.9,
                mediaReady: true, networkEpoch: epoch, measuredAt: now, sampleID: 601))
        controller.debugEvaluateForTest()
        await waitUntil(timeout: 5) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .wssRelay
        }
        if controller.debugIncumbent != .wssRelay {
            let dump = h.observation.published.suffix(8).map {
                "\($0.decision.reason.rawValue)/\($0.decision.chosen.rawValue)->\($0.decision.proposed?.rawValue ?? "nil"): \($0.decision.detail)"
            }.joined(separator: " | ")
            XCTFail("no relay reconcile: mode=\(String(describing: h.coordinator.debugRouteMode)) incumbent=\(String(describing: controller.debugIncumbent)) fence=\(String(describing: h.coordinator.debugWSStandbyFence)) staged=\(String(describing: h.coordinator.debugPolicyStagedRelay != nil)) standbyCalls=\(h.api.standbyPromoteCalls) standbyWSCalls=\(h.api.standbyWebSocketCalls) decisions=\(dump)")
            h.coordinator.handleProviderReset()
            return
        }
        XCTAssertFalse(controller.debugConfirmedOwner)
        XCTAssertTrue(h.wsGraph.isRunning)
        XCTAssertFalse(h.quicGraph.isRunning)
        XCTAssertFalse(h.quicSocket.closed, "the demoted QUIC socket stays attached as the standby")
        XCTAssertTrue(h.api.standbyPromoteCalls.contains { $0.transport == "ws" },
                      "the relay commit ran through the real fenced server promote")
        XCTAssertEqual(h.api.standbyPromoteCalls.last?.token, "ws-tok-fresh",
                       "the relay commit fenced with the demoted ws standby identity")
        h.coordinator.handleProviderReset()
    }

    /// Failure rollback of the same shape: a QUIC commit in flight across a
    /// mode change that then ROLLS BACK (graph start failure) keeps the
    /// incumbent honestly — no false owner, no healthy-path quarantine, the
    /// server-reconciled restore settles the call (no recovery), and the
    /// evaluation authority stays alive.
    func testModeChangeMidQUICCommitRollbackKeepsIncumbentHealthy() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.coordinator.quicPromoteReplyTimeout = 3
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await answerOneRelayPong(h)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)
        await waitUntil(timeout: 3) { !self.promoteCommands(h).isEmpty }
        let command = self.promoteCommands(h).first!
        let nonce = nonce(ofPromoteJSON: command)

        // Mode change mid-flight (production seam), then the graph start
        // FAILS after the ack (the server already swapped roles — the
        // rollback must reconcile server-side through the real restore).
        await h.coordinator.selectRouteMode(.relay)
        h.api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 9, currentEpoch: nil, primary: "ws", standby: "ws",
                standbyToken: "quic-tok-2"))
        ]
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":8,"nonce":"\#(nonce)","primary":"quic","standby":"ws"}"#)))
        h.quicGraph.startResult = false
        let controller = h.coordinator.debugQUICDirect!
        await waitUntil(timeout: 5) {
            h.api.standbyPromoteCalls.contains { $0.transport == "ws" }
        }
        // The server restore fenced with the demoted ws standby identity.
        XCTAssertEqual(h.api.standbyPromoteCalls.first?.token, "ws-tok-fresh")
        await waitUntil(timeout: 3) { h.wsGraph.isRunning }
        // No false owner, no quarantine of a healthy path, no recovery
        // episode: the incumbent relay kept the call throughout.
        XCTAssertNotEqual(controller.debugIncumbent, .quicDirect)
        XCTAssertFalse(controller.debugConfirmedOwner)
        XCTAssertFalse(h.coordinator.debugRecoveryActive,
                       "a confirmed server restore must not enter recovery")
        XCTAssertFalse(h.observation.published.contains {
            $0.decision.chosen == .quicDirect && $0.decision.reason != .forcedUnavailable
        }, "a rolled-back commit must never publish QUIC as the adopted path")
        XCTAssertTrue(controller.debugEvaluationRunning,
                      "the authority stays alive across the rollback")
        h.coordinator.handleProviderReset()
    }

    /// RTC direction: an RTC commit in flight across a mode change captures
    /// the REAL adopted RTC owner, then reconciles to the user's relay.
    func testModeChangeMidRTCCommitReconcilesToRelay() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await confirmQUICOwner(h)
        let controller = h.coordinator.debugQUICDirect!
        XCTAssertTrue(controller.debugConfirmedOwner)

        // Park the REAL RTC adoption commit mid-await.
        var adoptContinuations: [CheckedContinuation<Bool, Never>] = []
        h.coordinator.debugAdoptDirectPolicyOverride = {
            await withCheckedContinuation { cont in
                adoptContinuations.append(cont)
            }
        }
        controller.debugSuspendEvaluationTimerForTest()
        let epoch = controller.debugNetworkEpoch
        var id: UInt64 = 40
        func evalRTCBetter() async {
            let now = Date()
            controller.debugInjectSamplesForTest(
                quic: RouteCandidateSample(
                    rttMs: 80, jitterMs: 5, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch, measuredAt: now, sampleID: 100 + id),
                rtc: RouteCandidateSample(
                    rttMs: 15, jitterMs: 1, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch, measuredAt: now, sampleID: id),
                relay: RouteCandidateSample(
                    rttMs: 120, jitterMs: 8, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch, measuredAt: now, sampleID: 200 + id))
            id += 1
            controller.debugEvaluateForTest()
            await pumpMainActor(15)
        }
        await evalRTCBetter()
        try? await Task.sleep(nanoseconds: 350_000_000)
        await evalRTCBetter()
        await waitUntil(timeout: 3) { adoptContinuations.count == 1 }

        // Mode change mid-await (production seam); the RTC adoption then
        // SUCCEEDS for real.
        await h.coordinator.selectRouteMode(.relay)
        adoptContinuations.forEach { $0.resume(returning: true) }
        await waitUntil(timeout: 3) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .webRTCDirect
        }
        XCTAssertFalse(controller.debugConfirmedOwner)
        XCTAssertFalse(h.quicGraph.isRunning, "QUIC suspended for the RTC owner")

        // Reconcile to relay through the normal fenced relay commit.
        h.api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 9, currentEpoch: nil, primary: "ws", standby: "quic",
                standbyToken: "quic-tok-2"))
        ]
        let now2 = Date()
        controller.debugInjectSamplesForTest(
            rtc: RouteCandidateSample(
                rttMs: 15, jitterMs: 1, lossFraction: 0, reorderFraction: 0,
                queueCostMs: 0, available: true, confidence: 0.9,
                mediaReady: true, networkEpoch: epoch, measuredAt: now2, sampleID: 60),
            relay: RouteCandidateSample(
                rttMs: 120, jitterMs: 8, lossFraction: 0, reorderFraction: 0,
                queueCostMs: 0, available: true, confidence: 0.9,
                mediaReady: true, networkEpoch: epoch, measuredAt: now2, sampleID: 70))
        controller.debugEvaluateForTest()
        await waitUntil(timeout: 5) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .wssRelay
        }
        if controller.debugIncumbent != .wssRelay {
            let dump = h.observation.published.suffix(8).map {
                "\($0.decision.reason.rawValue)/\($0.decision.chosen.rawValue)->\($0.decision.proposed?.rawValue ?? "nil"): \($0.decision.detail)"
            }.joined(separator: " | ")
            XCTFail("no relay reconcile after RTC capture: incumbent=\(String(describing: controller.debugIncumbent)) standbyCalls=\(h.api.standbyPromoteCalls) decisions=\(dump)")
        }
        h.coordinator.debugAdoptDirectPolicyOverride = nil
        h.coordinator.handleProviderReset()
    }

    /// Relay direction: a relay commit in flight across a forced-QUIC mode
    /// change captures the REAL adopted relay owner, then reconciles to
    /// QUIC with the fresh standby token learned from the swap ack.
    func testModeChangeMidRelayCommitReconcilesToQUIC() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await confirmQUICOwner(h)
        let controller = h.coordinator.debugQUICDirect!

        // Park the REAL relay commit at the fenced server promote.
        h.api.standbyPromoteArmed = true
        await adoptRelayOverQUICParked(h)
        let parked = h.api.standbyPromoteCalls.count == 1
        XCTAssertTrue(parked, "the relay commit parks at the fenced promote")
        // The mode change lands while the commit is in flight (production
        // seam; the direct sub-preference store is read live by the closure).
        DirectTransportPreferenceStore.shared.setChoice(.quic, for: gatewayID)
        await h.coordinator.selectRouteMode(.direct)
        // The relay commit SUCCEEDS for real (resumeStandbyPromote returns
        // the canned result — the demoted QUIC's fresh standby identity is
        // learned from it).
        h.api.standbyPromoteArmed = false
        h.api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 9, currentEpoch: nil, primary: "ws", standby: "quic",
            standbyToken: "quic-tok-2")
        h.api.resumeStandbyPromote()
        await waitUntil(timeout: 5) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .wssRelay
        }
        XCTAssertEqual(controller.debugIncumbent, .wssRelay,
                       "the REAL adopted relay owner is captured, not quarantined")
        XCTAssertFalse(controller.debugConfirmedOwner)
        XCTAssertTrue(h.wsGraph.isRunning)

        // Reconcile to the forced QUIC path: fresh standby identity learned
        // from the swap ack makes the re-promotion fence valid. Suspend the
        // tick FIRST (timer tasks already enqueued survive invalidation —
        // drain them), then let the authority evaluate the forced mode once.
        controller.debugSuspendEvaluationTimerForTest()
        await pumpMainActor(30)
        let promoteCountBefore = self.promoteCommands(h).count
        if promoteCountBefore == 1 {
            // No stale commit fired: drive the reconciliation through the
            // production mapping with a fully injected evaluation.
            let epoch = controller.debugNetworkEpoch
            let now = Date()
            controller.debugInjectSamplesForTest(
                quic: RouteCandidateSample(
                    rttMs: 20, jitterMs: 2, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch, measuredAt: now, sampleID: 81),
                relay: RouteCandidateSample(
                    rttMs: 120, jitterMs: 8, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch, measuredAt: now, sampleID: 91))
            controller.debugEvaluateForTest()
            await pumpMainActor(20)
        }
        await waitUntil(timeout: 3) { self.promoteCommands(h).count > 1 }
        guard self.promoteCommands(h).count > 1 else {
            let dump = h.observation.published.suffix(8).map {
                "\($0.decision.reason.rawValue)/\($0.decision.chosen.rawValue)->\($0.decision.proposed?.rawValue ?? "nil"): \($0.decision.detail)"
            }.joined(separator: " | ")
            XCTFail("no QUIC re-promotion: incumbent=\(String(describing: controller.debugIncumbent)) standbyToken=\(String(describing: controller.debugStandbyToken)) history=\(controller.debugEvalHistory.map { "\($0.source)|\($0.detail.prefix(40))" }.joined(separator: " ;; ")) decisions=\(dump)")
            h.coordinator.handleProviderReset()
            return
        }
        let rePromote = self.promoteCommands(h).last!
        XCTAssertTrue(rePromote.contains("\"transport\":\"quic\""))
        XCTAssertTrue(rePromote.contains("\"token\":\"quic-tok-2\""),
                      "the re-promotion fences with the fresh standby token from the swap ack: \(rePromote)")
        XCTAssertTrue(rePromote.contains("\"expectEpoch\":9"), "\(rePromote)")
        let nonce = nonce(ofPromoteJSON: rePromote)
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":10,"nonce":"\#(nonce)","primary":"quic","standby":"ws"}"#)))
        await waitUntil(timeout: 5) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .quicDirect
        }
        XCTAssertTrue(controller.debugConfirmedOwner)
        XCTAssertTrue(h.quicGraph.isRunning)
        h.coordinator.handleProviderReset()
    }

    /// Build-65: a forced mode saved during call N must shape call N+1. The
    /// coordinator outlives calls and WSS+QUIC calls have no legacy route
    /// object — the authority's mapping (and recovery) read the CURRENTLY
    /// PERSISTED preference, never the coordinator's init-time snapshot.
    func testPersistedForcedModeSurvivesIntoNextQUICCall() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        await startOutgoing(h)
        // Mid-call forced relay through the production seam (persists too).
        await h.coordinator.selectRouteMode(.relay)
        h.coordinator.handleProviderReset()
        await pumpMainActor(10)

        // Call 2 on the SAME long-lived coordinator: the persisted forced
        // relay must keep the new QUIC authority from ever selecting QUIC,
        // even with excellent QUIC evidence.
        h.quicSocket.resetForReuse()
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await answerOneRelayPong(h)
        for _ in 0..<4 {
            await echoQUICProbe(h, rttNanoseconds: 10_000_000)
            await answerRelayPongs(h, count: 1, holdNanoseconds: 250_000_000)
            h.wsSocket.deliver(.success(.data(PCMUCodec.encode([Int16](repeating: 7, count: 160)))))
            await pumpMainActor(20)
        }
        XCTAssertTrue(self.promoteCommands(h).isEmpty,
                      "persisted forced relay must keep QUIC unselected in the next call")
        XCTAssertFalse(h.observation.published.contains {
            $0.decision.chosen == .quicDirect && $0.decision.reason != .forcedUnavailable
        })
        h.coordinator.handleProviderReset()
    }

    /// A stale adoptRelayForPolicy (superseded by a hangup while its fenced
    /// promote was parked) must never close the NEWER call's staged relay.
    /// The staged sessions live on their own sockets so both lifecycles
    /// are observable independently.
    func testStaleRelayCommitNeverClosesNewerStagedRelay() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let api = FakeGatewayAPI()
        api.iceConfigOverride = ICEConfiguration(
            policy: "all", iceServers: [],
            expiresAt: "2026-10-01T00:00:00Z",
            mediaTransports: ["ws", "quic"], mediaQuic: true)
        api.mediaWSRequestOverride = readyURL
        api.quicInfoResult = .success(try! QUICPinFixture.decode(
            QUICPinFixture.validDocument))
        api.identityResponse = QUICPinFixture.identityResponse
        api.quicCandidateResult = .success(QUICPunchInfo(
            punchToken: "ptok", gatewayCandidate: "203.0.113.7:4433",
            punchAtMs: 0, deadlineMs: 8000))
        // The fresh-STAGING path answers the standby WebSocket request.
        api.standbyWebSocketResult = readyURL
        // No ws standby fence from the QUIC-promote reconcile: both calls'
        // relay commits take the FRESH-STAGING path.
        api.standbyStateResult = V2MediaStateResponse(
            primary: "quic", epoch: 8, standbyWs: false, standbyIce: false,
            standbyWsToken: nil, standbyIceToken: nil)

        let primarySocket = FakeMediaSocket()
        primarySocket.scripted = [
            .message(.success(.string(#"{"type":"ready","epoch":2}"#))),
            .park,
        ]
        let primaryGraph = Graph()
        let stagedSocketA = FakeMediaSocket()
        stagedSocketA.scripted = [.park]
        let stagedSocketB = FakeMediaSocket()
        stagedSocketB.scripted = [.park]
        let stagedProvider = StagedSocketProvider(
            primary: primarySocket, staged: [stagedSocketA, stagedSocketB])
        let quicSocket = FakeQUICMediaSocket()
        let quicGraph = Graph()
        let endpointBox = EndpointBox()
        let observation = Observation()
        let coordinator = CallCoordinator(
            api: api, callKit: FakeCallKit(),
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            mediaRecoveryWindow: 45, gatewayID: gatewayID,
            wsMediaFactory: {
                let socket = stagedProvider.next()
                return WebSocketCallMedia(
                    socketFactory: { _, _ in socket }, audioGraph: Graph())
            },
            routeProbeFactory: { FakeDirectProbe() },
            quicMediaFactory: {
                QUICCallMedia(
                    socketFactory: { endpoint in
                        endpointBox.set(endpoint)
                        return quicSocket
                    },
                    audioGraph: quicGraph,
                    handshakeTimeout: 0.5, bindTimeout: 0.5)
            })
        coordinator.quicTrustedGatewayIdentity = (
            gatewayId: QUICPinFixture.gatewayId,
            fingerprint: QUICPinFixture.fingerprint)
        coordinator.quicSTUNResolver = { _, _ in
            STUNBindingResult(
                local: HostPort(host: "192.0.2.5", port: 51000),
                mapped: HostPort(host: "198.51.100.9", port: 54321))
        }
        coordinator.quicPromoteReplyTimeout = 3
        coordinator.quicEvaluationTickInterval = 0.1
        coordinator.quicAttemptTimeoutForTest = 30
        coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        coordinator.onRouteSelection = { decision, samples, _ in
            observation.published.append((decision, samples))
        }
        let h = Harness(
            coordinator: coordinator, api: api, observation: observation,
            wsSocket: primarySocket, wsGraph: primaryGraph,
            quicSocket: quicSocket, quicGraph: quicGraph,
            quicEndpointBox: endpointBox)

        // ---- Call 1: QUIC owner, then a relay commit that PARKS at the
        // fenced promote, with the staged standby already installed.
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await answerOneRelayPong(h)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)
        await waitUntil(timeout: 3) { !self.promoteCommands(h).isEmpty }
        let nonce1 = nonce(ofPromoteJSON: self.promoteCommands(h).first!)
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":8,"nonce":"\#(nonce1)","primary":"quic","standby":"ws"}"#)))
        await waitUntil(timeout: 3) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .quicDirect
        }
        // Relay becomes best → the commit stages a fresh standby (socket A),
        // then parks at the armed fenced promote.
        api.standbyPromoteArmed = true
        await feedRelayAdoptionEvidence(h, stagedSockets: stagedProvider) {
            $0.api.standbyPromoteCalls.count == 1
        }
        if h.coordinator.debugPolicyStagedRelay == nil {
            let dump = observation.published.suffix(10).map {
                "\($0.decision.reason.rawValue)/\($0.decision.chosen.rawValue)->\($0.decision.proposed?.rawValue ?? "nil"): \($0.decision.detail)"
                    + " r=\($0.samples[.wssRelay].map { "\(Int($0.rttMs))ms id\($0.sampleID) m\($0.mediaReady)" } ?? "nil")"
            }.joined(separator: " | ")
            XCTFail("call 1 staged nothing: standbyCalls=\(api.standbyPromoteCalls) standbyWS=\(api.standbyWebSocketCalls) stagedParked=\(stagedSocketA.isParked) created=\(stagedProvider.createdCount) quicProbes=\(quicSocket.sentProbes.count) decisions=\(dump)")
            h.coordinator.handleProviderReset()
            return
        }

        // ---- Hangup mid-promote: teardown retires the staged session.
        h.coordinator.handleProviderReset()
        await pumpMainActor(20)
        XCTAssertEqual(stagedSocketA.cancelCount, 1,
                       "teardown retires call 1's staged standby")

        // ---- Call 2 (same coordinator): QUIC owner again, relay commit
        // stages a NEW standby (socket B) and completes.
        quicSocket.resetForReuse()
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        // Call 2's primary ws attach parks on the (exhausted) script:
        // wait for the parked receive, then deliver its ready greeting.
        await waitUntil(timeout: 3) { primarySocket.isParked }
        primarySocket.deliver(.success(.string(#"{"type":"ready","epoch":2}"#)))
        await waitUntil(timeout: 3) {
            h.quicSocket.startedCount >= 2 && h.coordinator.debugQUICDirect != nil
        }
        await proveQUICGreetingAndMedia(h, epoch: 7)
        await answerOneRelayPong(h)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)
        await waitUntil(timeout: 3) { self.promoteCommands(h).count >= 2 }
        let nonce2 = nonce(ofPromoteJSON: self.promoteCommands(h).last!)
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":8,"nonce":"\#(nonce2)","primary":"quic","standby":"ws"}"#)))
        await waitUntil(timeout: 3) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .quicDirect
        }
        api.standbyPromoteArmed = false
        api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 9, currentEpoch: nil, primary: "ws", standby: "ws",
                standbyToken: "quic-tok-2"))
        ]
        await feedRelayAdoptionEvidence(h, stagedSockets: stagedProvider) {
            $0.coordinator.debugQUICDirect?.debugIncumbent == .wssRelay
        }
        XCTAssertNil(h.coordinator.debugPolicyStagedRelay,
                     "call 2's staged relay was promoted (slot consumed)")
        XCTAssertFalse(stagedSocketA === stagedSocketB)

        // ---- The stale call-1 promote finally resumes: its guard must NOT
        // touch call 2's live state (no close of the promoted relay, no
        // failure episode for the new call).
        api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 9, currentEpoch: nil, primary: "ws", standby: "ws",
                standbyToken: "quic-tok-2"))
        ]
        api.resumeStandbyPromote()
        await pumpMainActor(30)
        if stagedSocketB.cancelCount != 0 || h.coordinator.debugRecoveryActive
            || h.coordinator.debugQUICDirect?.debugIncumbent != .wssRelay {
            let dump = observation.published.suffix(8).map {
                "\($0.decision.reason.rawValue)/\($0.decision.chosen.rawValue)->\($0.decision.proposed?.rawValue ?? "nil")"
            }.joined(separator: " | ")
            XCTFail("stale damage: bCancels=\(stagedSocketB.cancelCount) recovery=\(h.coordinator.debugRecoveryActive) incumbent=\(String(describing: h.coordinator.debugQUICDirect?.debugIncumbent)) wsConnected=\(h.coordinator.debugWSMediaConnectedForTest) standbyCalls=\(api.standbyPromoteCalls) decisions=\(dump)")
        }
        h.coordinator.handleProviderReset()
    }

    /// A stale adoptDirectForPolicy restore (hangup during the
    /// server-restore await) must not restart the captured QUIC graph after
    /// a newer call took the mic, and must not arm a recovery episode for
    /// the new call.
    func testStaleDirectAdoptRestoreAfterHangupRestartsNothing() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        // The failed RTC adoption left the server roles swapped (ICE
        // primary): the restore must reach the fenced ws promote (armed →
        // parked), not the already-primary shortcut.
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ice", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await confirmQUICOwner(h)
        let controller = h.coordinator.debugQUICDirect!
        XCTAssertTrue(h.quicGraph.isRunning)

        // Q→RTC commit whose adoption FAILS, entering the
        // restoreWSPrimaryAfterFailedQUICPromotion await — park THAT at the
        // armed fenced promote.
        h.coordinator.debugAdoptDirectPolicyOverride = { false }
        h.api.standbyPromoteArmed = true
        controller.debugSuspendEvaluationTimerForTest()
        let epoch = controller.debugNetworkEpoch
        var id: UInt64 = 70
        func evalRTCBetter() {
            let now = Date()
            controller.debugInjectSamplesForTest(
                quic: RouteCandidateSample(
                    rttMs: 80, jitterMs: 5, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch, measuredAt: now, sampleID: 100 + id),
                rtc: RouteCandidateSample(
                    rttMs: 15, jitterMs: 1, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch, measuredAt: now, sampleID: id),
                relay: RouteCandidateSample(
                    rttMs: 120, jitterMs: 8, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch, measuredAt: now, sampleID: 200 + id))
            id += 1
            controller.debugEvaluateForTest()
        }
        evalRTCBetter()
        try? await Task.sleep(nanoseconds: 350_000_000)
        evalRTCBetter()
        await waitUntil(timeout: 3) {
            h.api.standbyPromoteCalls.contains { $0.transport == "ws" }
        }
        // The restore is parked; the QUIC owner stays suspended awaiting the
        // restore outcome.
        XCTAssertFalse(h.quicGraph.isRunning, "QUIC suspended during the failed handover")

        // Hangup mid-restore, then the stale restore resumes: it must NOT
        // restart the captured QUIC graph nor arm a recovery for the new
        // lifecycle.
        h.coordinator.handleProviderReset()
        await pumpMainActor(10)
        let quicStartCount = h.quicGraph.startCount
        h.api.standbyPromoteArmed = false
        h.api.resumeStandbyPromote()
        await pumpMainActor(30)
        XCTAssertEqual(h.quicGraph.startCount, quicStartCount,
                       "the stale restore must never restart the captured QUIC graph")
        XCTAssertFalse(h.coordinator.debugRecoveryActive,
                       "the stale restore must not schedule media failure for a dead call")
        h.coordinator.debugAdoptDirectPolicyOverride = nil
        h.coordinator.handleProviderReset()
    }

    /// Initial trusted-dial failure must arm the BOUNDED quarantine retry
    /// (previously only post-connect state failures did), with exactly one
    /// retry task and an honest re-run of the coordinated chain.
    func testInitialDialFailureArmsBoundedRetry() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        await startOutgoing(h)
        // The dial dies mid-handshake: the initial-start catch path.
        h.quicSocket.emitState(.failed("dial unreachable"))
        let controller = h.coordinator.debugQUICDirect!
        await waitUntil(timeout: 3) { controller.debugRetryScheduled }
        XCTAssertTrue(controller.debugRetryScheduled,
                      "the initial trusted-dial failure arms the bounded retry")
        XCTAssertTrue(controller.debugRetryCount >= 1)
        XCTAssertNil(controller.quicCallMedia)
        // The retry honestly re-runs the coordinated chain: capability +
        // trust re-verified, STUN re-resolved, dial re-attempted
        // (startedCount 1 → 2) with the SAME network epoch.
        await waitUntil(timeout: 10) { h.quicSocket.startedCount >= 2 }
        XCTAssertGreaterThanOrEqual(h.quicSocket.startedCount, 2)
        h.coordinator.handleProviderReset()
    }

    /// 直连 + 自动 sub-preference permits ONLY the direct family: the relay
    /// incumbent is not kept, and the best direct transport wins on
    /// evidence (never a relay selection).
    func testDirectFamilyPermittedSetNeverSelectsRelay() async throws {
        let observation = Observation()
        let controller = QUICDirectController(
            wsMedia: { nil },
            preferredTransport: { .directFamily },
            promote: { _ in true },
            adoptDirect: { true },
            publish: { decision, samples, _ in
                observation.published.append((decision, samples))
            })
        controller.policyConfig = fastPolicyConfig()
        var clock = Date()
        controller.now = { clock }
        // Relay is the provisional carrier; QUIC + RTC prove ready, RTC
        // faster. The permitted set {QUIC, RTC} must exclude relay.
        for (i, step) in [0.4, 0.4].enumerated() {
            controller.debugInjectSamplesForTest(
                quic: RouteCandidateSample(
                    rttMs: 60, jitterMs: 3, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: "q", measuredAt: clock,
                    sampleID: UInt64(50 + i)),
                rtc: RouteCandidateSample(
                    rttMs: 35, jitterMs: 2, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: "r", measuredAt: clock,
                    sampleID: UInt64(60 + i)),
                relay: RouteCandidateSample(
                    rttMs: 20, jitterMs: 1, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: "w", measuredAt: clock,
                    sampleID: UInt64(70 + i)))
            clock.addTimeInterval(step)
            controller.debugEvaluateForTest()
            await pumpMainActor(20)
        }
        XCTAssertEqual(controller.debugIncumbent, .webRTCDirect,
                       "direct-family mode picks the best DIRECT transport")
        XCTAssertFalse(observation.published.contains { $0.decision.chosen == .wssRelay },
                       "relay is never selected in direct-family mode")
    }
}

extension QUICDirectControllerTests {

    // MARK: Q→RTC audio-exclusivity ordering (build-62)

    private func confirmQUICOwner(_ h: Harness) async {
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await answerOneRelayPong(h)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)
        await waitUntil(timeout: 3) { !self.promoteCommands(h).isEmpty }
        let command = self.promoteCommands(h).first!
        let nonce = nonce(ofPromoteJSON: command)
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":8,"nonce":"\#(nonce)","primary":"quic","standby":"ws"}"#)))
        await waitUntil(timeout: 3) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .quicDirect
        }
    }

    /// The RTC target may open capture only AFTER the live QUIC incumbent
    /// graph has suspended — a fake target that "rejects overlapping
    /// capture" proves the ordering through the REAL evaluateOnce +
    /// REAL adopt closure.
    func testCoordinatorQToRTCOwnershipOrdering() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await confirmQUICOwner(h)
        let controller = h.coordinator.debugQUICDirect!
        XCTAssertTrue(controller.debugConfirmedOwner)
        XCTAssertTrue(h.quicGraph.isRunning, "QUIC owns the capture graph")
        // Fully injected evaluations: stop the real tick (it would reset
        // the injected streaks) and use the REAL network epoch (a foreign
        // epoch fences the intake).
        controller.debugSuspendEvaluationTimerForTest()
        let epoch = controller.debugNetworkEpoch

        let quicGraph = h.quicGraph
        var targetOpenedWhileQUICOwned = true
        h.coordinator.debugAdoptDirectPolicyOverride = {
            // The target's capture graph opens HERE: overlapping capture
            // must already be impossible.
            targetOpenedWhileQUICOwned = quicGraph.isRunning
            return true
        }
        let now = Date()
        for id: UInt64 in [1, 2] {
            controller.debugInjectSamplesForTest(
                quic: RouteCandidateSample(
                    rttMs: 85, jitterMs: 6, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: 1000 + id),
                rtc: RouteCandidateSample(
                    rttMs: 25, jitterMs: 2, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: id),
                relay: RouteCandidateSample(
                    rttMs: 95, jitterMs: 6, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: 100 + id))
            try? await Task.sleep(nanoseconds: 400_000_000)
            controller.debugEvaluateForTest()
            await pumpMainActor(20)
        }
        XCTAssertFalse(targetOpenedWhileQUICOwned,
                        "the RTC target opened capture while the QUIC graph still owned it")
        if controller.debugIncumbent != .webRTCDirect {
            let dump = h.observation.published.map {
                "\($0.decision.reason.rawValue)/\($0.decision.chosen.rawValue)->\($0.decision.proposed?.rawValue ?? "nil"): \($0.decision.detail)"
            }.joined(separator: " | ")
            XCTFail("incumbent \(controller.debugIncumbent?.rawValue ?? "nil"); decisions: \(dump)")
        }
        // QUIC demoted to standby (staged, socket alive) — authority kept.
        XCTAssertFalse(h.quicSocket.closed)
        h.coordinator.debugAdoptDirectPolicyOverride = nil
        h.coordinator.handleProviderReset()
    }

    /// A failed/cancelled Q→RTC handoff restores the QUIC incumbent audio.
    func testCoordinatorQToRTCFailedHandoffRestoresQUIC() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await confirmQUICOwner(h)
        let controller = h.coordinator.debugQUICDirect!
        controller.debugSuspendEvaluationTimerForTest()
        let epoch = controller.debugNetworkEpoch
        h.coordinator.debugAdoptDirectPolicyOverride = { false }
        let now = Date()
        for id: UInt64 in [1, 2] {
            controller.debugInjectSamplesForTest(
                rtc: RouteCandidateSample(
                    rttMs: 25, jitterMs: 2, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: id),
                relay: RouteCandidateSample(
                    rttMs: 95, jitterMs: 6, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: 100 + id))
            try? await Task.sleep(nanoseconds: 400_000_000)
            controller.debugEvaluateForTest()
            await pumpMainActor(20)
        }
        // The handoff failed: QUIC stays the confirmed owner with audio.
        XCTAssertEqual(controller.debugIncumbent, .quicDirect)
        XCTAssertTrue(controller.debugConfirmedOwner)
        XCTAssertFalse(controller.quicCallMedia!.isStagedForHandover,
                       "a failed handoff must unstage the incumbent audio")
        h.coordinator.debugAdoptDirectPolicyOverride = nil
        h.coordinator.handleProviderReset()
    }
}

extension QUICDirectControllerTests {

    // MARK: Build-62 server-reconciled QUIC promotion rollback

    /// Server ACK then local graph-start failure: the rollback performs the
    /// REAL server swap-back with the demoted WSS standby identity, resumes
    /// the WSS graph only after the confirmed restore, and never arms the
    /// recovery.
    func testCoordinatorQUICPromoteGraphFailureRestoresWSSServerSide() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.coordinator.quicPromoteReplyTimeout = 3
        // The demoted WSS standby identity the rollback must fence with.
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await answerOneRelayPong(h)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)
        await waitUntil(timeout: 3) { !self.promoteCommands(h).isEmpty }
        let command = self.promoteCommands(h).first!
        let nonce = nonce(ofPromoteJSON: command)

        // The gateway acks, but the QUIC graph start now FAILS (build-44
        // scripted failure): the ack already swapped the server roles.
        h.api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 9, currentEpoch: nil, primary: "ws", standby: "ws",
                standbyToken: "quic-tok-2"))
        ]
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":8,"nonce":"\#(nonce)","primary":"quic","standby":"ws"}"#)))
        h.quicGraph.startResult = false
        await waitUntil(timeout: 5) {
            h.api.standbyPromoteCalls.contains { $0.transport == "ws" }
        }
        // The swap-back fenced with the DEMOTED WSS standby identity.
        XCTAssertEqual(h.api.standbyPromoteCalls.first?.token, "ws-tok-fresh")
        // The WSS audio resumed only after the confirmed restore.
        await waitUntil(timeout: 3) { h.wsGraph.isRunning }
        XCTAssertFalse(h.coordinator.debugRecoveryActive,
                       "a confirmed server restore must not enter recovery")
        XCTAssertTrue(h.observation.qualityPhases.contains(.connected))
        h.coordinator.handleProviderReset()
    }

    /// Swap-back ack lost (and the ws-family snapshot cannot confirm the
    /// QUIC case): the rollback enters the bounded 45 s recovery — never a
    /// healthy WSS publish.
    func testCoordinatorQUICPromoteGraphFailureUnconfirmedGoesRecovery() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.coordinator.quicPromoteReplyTimeout = 3
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await startOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await answerOneRelayPong(h)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)
        await waitUntil(timeout: 3) { !self.promoteCommands(h).isEmpty }
        let command = self.promoteCommands(h).first!
        let nonce = nonce(ofPromoteJSON: command)

        // Ack the swap, then REJECT the swap-back: the ambiguous family
        // snapshot cannot confirm the restore (familyHoldsQUIC).
        h.api.standbyPromoteScript = [
            .failure(APIError.http(status: 502, code: "CB-V2-502", message: "swap-back rejected"))
        ]
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":8,"nonce":"\#(nonce)","primary":"quic","standby":"ws"}"#)))
        h.quicGraph.startResult = false
        await waitUntil(timeout: 6) { h.coordinator.debugRecoveryActive }
        XCTAssertTrue(h.coordinator.debugRecoveryActive,
                      "an unconfirmed server restore must enter the bounded recovery")
        h.coordinator.handleProviderReset()
    }
}

extension QUICDirectControllerTests {

    // MARK: Build-63 ownership-independent QUIC failure lifecycle

    /// Drives the REAL coordinator seams: QUIC confirmed owner → relay
    /// proves better → adoptRelay commits (Q→WSS) — leaving QUIC as the
    /// hot standby under a confirmed WSS owner. Shared by the build-63
    /// failure-lifecycle tests.
    private func adoptRelayOverQUIC(_ h: Harness) async {
        h.api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 9, currentEpoch: nil, primary: "ws", standby: "ws",
                standbyToken: "quic-tok-2"))
        ]
        var committed = false
        for _ in 0..<14 where !committed {
            await answerRelayPongs(h, count: 1, holdNanoseconds: 1_000_000)
            h.wsSocket.deliver(.success(.data(PCMUCodec.encode([Int16](repeating: 7, count: 160)))))
            await echoQUICProbe(h, rttNanoseconds: 250_000_000)
            await pumpMainActor(15)
            committed = h.api.standbyPromoteCalls.count == 1
                && h.coordinator.debugQUICDirect?.debugIncumbent == .wssRelay
        }
        XCTAssertTrue(committed, "the relay sustained-gain commit never fired")
    }

    /// Ordered socket provider for multi-call harnesses: the coordinator's
    /// wsMediaFactory pops one pre-scripted socket per session creation in
    /// the deterministic order PRIMARY, STAGED, PRIMARY, STAGED… (one relay
    /// commit per call), so staged standbys get their own socket independent
    /// of each call's primary/control-plane socket.
    private final class StagedSocketProvider: @unchecked Sendable {
        private let lock = NSLock()
        private let primary: FakeMediaSocket
        private var staged: [FakeMediaSocket]
        private var index = 0
        init(primary: FakeMediaSocket, staged: [FakeMediaSocket]) {
            self.primary = primary
            self.staged = staged
        }
        private var lastCreated: FakeMediaSocket?
        func next() -> FakeMediaSocket {
            lock.lock(); defer { lock.unlock() }
            defer { index += 1 }
            let socket: FakeMediaSocket
            if index % 2 == 0 {
                socket = primary
            } else if !staged.isEmpty {
                socket = staged.removeFirst()
            } else {
                socket = FakeMediaSocket()
                socket.scripted = [.park]
            }
            lastCreated = socket
            return socket
        }
        var createdCount: Int {
            lock.lock(); defer { lock.unlock() }
            return index
        }
        /// The most recently created STANDBY socket (nil while only the
        /// primary exists).
        var lastStaged: FakeMediaSocket? {
            lock.lock(); defer { lock.unlock() }
            return index > 1 ? lastCreated : nil
        }
    }

    /// Feeds real relay evidence (distinct pongs + genuine inbound frames vs
    /// degraded QUIC probes) until `completion` reports the commit state.
    /// Delivers the staged standby's ready greeting on its own socket when
    /// the staging path needs it (each staged socket greeted exactly once).
    private func feedRelayAdoptionEvidence(
        _ h: Harness,
        stagedSockets: StagedSocketProvider? = nil,
        completion: @MainActor (Harness) -> Bool
    ) async {
        var greeted = Set<ObjectIdentifier>()
        var ordinal = 0
        for _ in 0..<16 {
            await answerRelayPongs(h, count: 1, holdNanoseconds: 1_000_000)
            // Best-effort media proof: deliver only while the receive is
            // parked (the single-slot fake overwrites a busy handler).
            if h.wsSocket.isParked {
                h.wsSocket.deliver(.success(.data(PCMUCodec.encode([Int16](repeating: 7, count: 160)))))
            }
            await echoQUICProbe(h, rttNanoseconds: 250_000_000)
            await pumpMainActor(15)
            if let provider = stagedSockets,
               let socket = provider.lastStaged, socket.isParked,
               !greeted.contains(ObjectIdentifier(socket)) {
                // A staged standby's connect is parked on its own socket:
                // greet it (the stage helper installs it as soon as the
                // greeting carries the standby token).
                greeted.insert(ObjectIdentifier(socket))
                ordinal += 1
                socket.deliver(.success(.string(
                    #"{"type":"ready","epoch":3,"token":"staged-tok-\#(ordinal)","standby":true}"#)))
                await pumpMainActor(10)
            }
            if completion(h) { return }
        }
        XCTFail("relay adoption evidence loop never reached the expected commit state")
        h.coordinator.handleProviderReset()
    }

    /// Drives the REAL relay sustained-gain commit and returns once its
    /// fenced server promote is PARKED (caller resumes it deliberately).
    private func adoptRelayOverQUICParked(_ h: Harness) async {
        var parked = false
        for _ in 0..<14 where !parked {
            await answerRelayPongs(h, count: 1, holdNanoseconds: 1_000_000)
            h.wsSocket.deliver(.success(.data(PCMUCodec.encode([Int16](repeating: 7, count: 160)))))
            await echoQUICProbe(h, rttNanoseconds: 250_000_000)
            await pumpMainActor(15)
            parked = h.api.standbyPromoteCalls.count == 1
        }
        XCTAssertTrue(parked, "the relay commit never reached the fenced promote")
    }

    /// Blocker 1 (standby death): with WSS the confirmed owner and QUIC the
    /// hot standby, the QUIC socket dying must NOT freeze selection — the
    /// evaluation tick keeps running across the healthy paths, a bounded
    /// retry arms, RTC stays adoptable, and the retry honestly re-dials.
    func testStandbyQUICDeathNeverFreezesSelection() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await confirmQUICOwner(h)
        await adoptRelayOverQUIC(h)
        let controller = h.coordinator.debugQUICDirect!
        XCTAssertEqual(controller.debugIncumbent, .wssRelay)
        XCTAssertTrue(controller.debugConfirmedOwner == false,
                      "after Q→WSS the QUIC path is the standby, not the owner")
        XCTAssertFalse(h.quicSocket.closed, "the demoted QUIC socket stays attached")

        // The standby QUIC socket dies.
        h.quicSocket.emitState(.failed("test"))
        await pumpMainActor(20)

        // The build-62 defect froze HERE: evaluateTimer invalidated, the
        // bootstrap cadence already spent, the whole authority silent.
        XCTAssertTrue(controller.debugEvaluationRunning,
                      "a standby QUIC death must never stop the evaluation tick")
        XCTAssertTrue(controller.debugRetryScheduled,
                      "a standby QUIC death arms the bounded quarantine retry")
        XCTAssertNil(controller.quicCallMedia)
        XCTAssertFalse(h.wsGraph.isRunning == false && h.quicGraph.isRunning,
                       "no leaked competing capture graph after the standby death")

        // Selection is alive: RTC (injected, real network epoch) is better
        // than the relay and the REAL adopt seam fires — through a target
        // that rejects overlapping capture.
        controller.debugSuspendEvaluationTimerForTest()
        let epoch = controller.debugNetworkEpoch
        let quicGraph = h.quicGraph
        var targetOpenedWhileQUICOwned = true
        h.coordinator.debugAdoptDirectPolicyOverride = {
            targetOpenedWhileQUICOwned = quicGraph.isRunning
            return true
        }
        let now = Date()
        for id: UInt64 in [1, 2] {
            controller.debugInjectSamplesForTest(
                rtc: RouteCandidateSample(
                    rttMs: 25, jitterMs: 2, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: id),
                relay: RouteCandidateSample(
                    rttMs: 95, jitterMs: 6, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: 100 + id))
            try? await Task.sleep(nanoseconds: 400_000_000)
            controller.debugEvaluateForTest()
            await pumpMainActor(20)
        }
        XCTAssertFalse(targetOpenedWhileQUICOwned,
                        "the RTC target opened capture while the QUIC graph still owned it")
        XCTAssertEqual(controller.debugIncumbent, .webRTCDirect,
                       "selection must continue across the healthy paths after a standby Q death")
        h.coordinator.debugAdoptDirectPolicyOverride = nil

        // The bounded retry honestly re-dials (startedCount 1 → 2) with the
        // same network epoch.
        await waitUntil(timeout: 8) { h.quicSocket.startedCount >= 2 }
        XCTAssertGreaterThanOrEqual(h.quicSocket.startedCount, 2,
                                    "the quarantine retry must re-run the coordinated dial")
        h.coordinator.handleProviderReset()
    }

    /// Blocker 1 (primary death): the confirmed QUIC primary dying enters
    /// the bounded recovery AND arms the retry — the candidate is
    /// quarantined, not erased, and re-qualifies on its own.
    func testQUICPrimaryDeathEntersRecoveryAndRetries() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await confirmQUICOwner(h)
        let controller = h.coordinator.debugQUICDirect!
        XCTAssertTrue(controller.debugConfirmedOwner)

        h.quicSocket.emitState(.failed("test"))
        await pumpMainActor(20)

        XCTAssertFalse(controller.debugConfirmedOwner)
        XCTAssertTrue(controller.debugEvaluationRunning,
                      "the primary death must not freeze the authority either")
        XCTAssertTrue(controller.debugRetryScheduled,
                      "the primary death arms the bounded quarantine retry")
        // The bounded recovery owns the call now (45 s window, fresh relay
        // re-attach first): the WSS control-plane socket must stay attached
        // for the recovery to ride on.
        XCTAssertFalse(h.wsSocket.cancelled,
                       "the recovery path needs the WSS control plane alive")
        // The retry re-dials with the same epoch and the candidate
        // re-qualifies (a fresh standby attach lands).
        await waitUntil(timeout: 8) { h.quicSocket.startedCount >= 2 }
        XCTAssertGreaterThanOrEqual(h.quicSocket.startedCount, 2)
        h.coordinator.handleProviderReset()
    }

    /// Blocker 2 (client): Q→RTC under the authority must NOT retire the
    /// WSS relay socket — it is the QUIC control plane and the only relay
    /// telemetry feed. The build-62 flow closed it via the legacy
    /// retireRelay callback; the build-63 flow keeps it attached in the
    /// demoted standby role, and the demoted QUIC identity is learned from
    /// the TYPED standbyQuic state field (never the ambiguous ws family).
    func testQToRTCKeepsWSSControlPlaneAndUsesTypedState() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        // Typed build-63 snapshot: the demoted QUIC sits in the dedicated
        // standbyQuic slot with its own token.
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ice", epoch: 9, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil,
            standbyQuic: true, standbyQuicToken: "quic-tok-typed")
        await confirmQUICOwner(h)
        let controller = h.coordinator.debugQUICDirect!
        controller.debugSuspendEvaluationTimerForTest()
        let epoch = controller.debugNetworkEpoch
        var adoptCalls = 0
        h.coordinator.debugAdoptDirectPolicyOverride = {
            adoptCalls += 1
            return true
        }
        let now = Date()
        for id: UInt64 in [1, 2] {
            controller.debugInjectSamplesForTest(
                quic: RouteCandidateSample(
                    rttMs: 85, jitterMs: 6, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: 1000 + id),
                rtc: RouteCandidateSample(
                    rttMs: 25, jitterMs: 2, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: id),
                relay: RouteCandidateSample(
                    rttMs: 95, jitterMs: 6, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: 100 + id))
            try? await Task.sleep(nanoseconds: 400_000_000)
            controller.debugEvaluateForTest()
            await pumpMainActor(20)
        }
        XCTAssertEqual(adoptCalls, 1)
        XCTAssertEqual(controller.debugIncumbent, .webRTCDirect)
        // The WSS control plane SURVIVES the direct adoption.
        XCTAssertFalse(h.wsSocket.cancelled,
                       "Q→RTC must not retire the WSS control plane under the authority")

        // QUIC proves better again: the re-promotion must fence with the
        // TYPED standbyQuic token learned from the typed state field.
        // Sample ids stay MONOTONIC per transport (2000+ > the first
        // loop's 1000+) so the out-of-order intake fence cannot drop them;
        // timestamps anchor to the CURRENT clock (inside the future-skew
        // bound) and strictly follow the first loop's.
        let later = Date()
        var rePromoted = false
        for id: UInt64 in 10..<24 where !rePromoted {
            controller.debugInjectSamplesForTest(
                quic: RouteCandidateSample(
                    rttMs: 15, jitterMs: 1, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: later.addingTimeInterval(Double(id) * 0.4),
                    sampleID: 2000 + id),
                rtc: RouteCandidateSample(
                    rttMs: 85, jitterMs: 6, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: later.addingTimeInterval(Double(id) * 0.4),
                    sampleID: 200 + id),
                relay: RouteCandidateSample(
                    rttMs: 95, jitterMs: 6, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: later.addingTimeInterval(Double(id) * 0.4),
                    sampleID: 300 + id))
            try? await Task.sleep(nanoseconds: 400_000_000)
            controller.debugEvaluateForTest()
            await pumpMainActor(20)
            rePromoted = promoteCommands(h).contains {
                $0.contains("\"token\":\"quic-tok-typed\"")
            }
        }
        XCTAssertTrue(rePromoted,
                      "the Q re-promotion must fence with the typed standbyQuic token")
        h.coordinator.debugAdoptDirectPolicyOverride = nil
        h.coordinator.handleProviderReset()
    }

    /// Blocker 3: a hangup/new call during the Q→RTC adopt await must leave
    /// the stale callback touching NOTHING — no server reconcile, no
    /// recovery schedule, no publish against the newer lifecycle.
    func testAdoptDirectFailureAfterTeardownTouchesNothing() async throws {
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let h = makeHarness()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 8, standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-fresh", standbyIceToken: nil)
        await confirmQUICOwner(h)
        let controller = h.coordinator.debugQUICDirect!
        controller.debugSuspendEvaluationTimerForTest()
        let epoch = controller.debugNetworkEpoch
        let mediaStateCallsBefore = h.api.mediaStateCalls
        let promoteCallsBefore = h.api.standbyPromoteCalls.count
        // The adopt itself tears the call down mid-await (hangup/new call
        // during the server commit) and then reports failure.
        h.coordinator.debugAdoptDirectPolicyOverride = {
            h.coordinator.handleProviderReset()
            return false
        }
        let now = Date()
        for id: UInt64 in [1, 2] {
            controller.debugInjectSamplesForTest(
                rtc: RouteCandidateSample(
                    rttMs: 25, jitterMs: 2, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: id),
                relay: RouteCandidateSample(
                    rttMs: 95, jitterMs: 6, lossFraction: 0, reorderFraction: 0,
                    queueCostMs: 0, available: true, confidence: 0.9,
                    mediaReady: true, networkEpoch: epoch,
                    measuredAt: now.addingTimeInterval(Double(id) * 0.4),
                    sampleID: 100 + id))
            try? await Task.sleep(nanoseconds: 400_000_000)
            controller.debugEvaluateForTest()
            await pumpMainActor(20)
            if h.coordinator.debugQUICDirect == nil { break }
        }
        // The stale failure path ran AFTER the teardown: it must not have
        // touched the server (no reconcile promote / mediaState for the
        // dead call) — the newer lifecycle owns state.
        XCTAssertEqual(h.api.standbyPromoteCalls.count, promoteCallsBefore,
                       "a stale post-teardown adopt must not issue server reconciles")
        XCTAssertEqual(h.api.mediaStateCalls, mediaStateCallsBefore,
                       "a stale post-teardown adopt must not query server state")
        h.coordinator.debugAdoptDirectPolicyOverride = nil
        h.coordinator.handleProviderReset()
    }
}
