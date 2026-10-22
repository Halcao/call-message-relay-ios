import XCTest
import AVFoundation
@testable import CallRelay

/// Build-66: network handover / weak-link recovery at the REAL coordinator
/// seams. Covers Wi-Fi ↔ cellular identity changes, down/up edges, stale old
/// sockets, the media-health stall backstop and the QUIC pinned-carrier
/// retirement with a fresh network epoch — every trigger arms the SAME
/// bounded 45 s recovery (immediate + 1/2/4/8 s attempts) and only genuine
/// inbound media clears it.
@MainActor
final class NetworkPathRecoveryTests: XCTestCase {
    private let gatewayID = "gw-netpath-recovery"
    private let readyURL = URLRequest(url: URL(string: "wss://example.test/media")!)

    private let wifiSignature = CallPathSignature(satisfied: true, usedFamilies: [.wifi])
    private let cellularSignature = CallPathSignature(
        satisfied: true, usedFamilies: [.cellular], isExpensive: true)

    // MARK: Fake observer

    final class FakeCallPathObserver: CallPathObserving {
        var onEvent: ((CallPathEvent) -> Void)?
        private(set) var started = false
        private(set) var stopped = false
        private(set) var currentGeneration: UInt64 = 0
        private(set) var isReachable = true

        func start() { started = true }

        func stop() {
            stopped = true
            onEvent = nil
        }

        func emit(_ event: CallPathEvent) {
            currentGeneration = max(currentGeneration, event.generation)
            switch event {
            case .becameUnreachable: isReachable = false
            default: isReachable = true
            }
            onEvent?(event)
        }
    }

    // MARK: WSS harness

    @MainActor
    private final class WSScript {
        let socket = FakeMediaSocket()
        let graph = Graph()
        init(ready: Bool = true) {
            socket.scripted = ready
                ? [.message(.success(.string(#"{"type":"ready"}"#))), .park]
                : [.park]
        }
    }

    private final class WSSTracked {
        var media: [WebSocketCallMedia] = []
        var sockets: [FakeMediaSocket] = []
    }

    private struct WSSHarness {
        let coordinator: CallCoordinator
        let api: FakeGatewayAPI
        let callKit: FakeCallKit
        let observer: FakeCallPathObserver
        let scripts: [WSScript]
        let tracked: WSSTracked
        let uuid: UUID
    }

    private func makeWSSCall(
        scripts: [WSScript],
        window: TimeInterval = 45,
        stallGrace: TimeInterval = 30,
        stallTick: TimeInterval = 0.05,
        pathChangeGrace: TimeInterval = 0.2
    ) -> WSSHarness {
        let api = FakeGatewayAPI()
        api.iceConfigOverride = ICEConfiguration(
            policy: "all", iceServers: [],
            expiresAt: "2026-10-01T00:00:00Z",
            mediaTransports: ["ws"])
        api.mediaWSRequestOverride = readyURL
        let observer = FakeCallPathObserver()
        let tracked = WSSTracked()
        var remaining = scripts
        let callKit = FakeCallKit()
        let coordinator = CallCoordinator(
            api: api, callKit: callKit,
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            mediaRecoveryWindow: window, gatewayID: gatewayID,
            wsMediaFactory: {
                let script = remaining.isEmpty ? WSScript() : remaining.removeFirst()
                let media = WebSocketCallMedia(
                    socketFactory: { _, _ in script.socket }, audioGraph: script.graph)
                tracked.media.append(media)
                tracked.sockets.append(script.socket)
                return media
            },
            routeCadence: nil,
            pathObserverFactory: { observer })
        coordinator.mediaStallGrace = stallGrace
        coordinator.mediaStallTickInterval = stallTick
        coordinator.pathChangeStallGrace = pathChangeGrace
        coordinator.pathRecoveryNudgeCooldown = 0.05
        return WSSHarness(coordinator: coordinator, api: api, callKit: callKit,
                          observer: observer, scripts: scripts, tracked: tracked,
                          uuid: UUID())
    }

    private func startWSSCall(_ h: WSSHarness) async {
        let record = makeCallRecord(id: h.uuid.uuidString.lowercased(), state: .outgoingDialing)
        h.api.dialResult = .success(record)
        h.api.activeRecordForPoll = makeCallRecord(id: record.id, state: .active)
        h.coordinator.startOutgoing(peer: "555-0123", uuid: h.uuid)
        await waitUntil(timeout: 3) { h.scripts[0].socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(30)
    }

    override func setUp() {
        super.setUp()
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
    }

    override func tearDown() {
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        DirectTransportPreferenceStore.shared.setChoice(.auto, for: gatewayID)
        AudioSessionBridge.shared.didDeactivate(AVAudioSession.sharedInstance())
        super.tearDown()
    }

    // MARK: 1. Path change with a stale carrier arms recovery NOW

    func testPathChangeWithStaleCarrierArmsRecoveryPromptlyAndFreshMediaSettles() async throws {
        let h = makeWSSCall(scripts: [WSScript(), WSScript()])
        await startWSSCall(h)
        let initial = h.tracked.media[0]
        XCTAssertTrue(h.coordinator.debugWSMediaIsInstalled(initial))
        // Genuine media on the carrier, then the interface changes and the
        // frames stop (the field Wi-Fi → cellular report).
        initial.fireInboundRealFrameForTest()
        await pumpMainActor(3)
        try? await Task.sleep(nanoseconds: 300_000_000)
        h.observer.emit(.pathChanged(generation: 1, previous: wifiSignature))

        // The stale carrier is caught IMMEDIATELY — no waiting for a stale
        // transport timeout.
        await waitUntil(timeout: 2) { h.coordinator.debugRecoveryActive }
        XCTAssertTrue(h.coordinator.debugRecoveryActive)
        // The immediate first attempt stages a replacement relay.
        await waitUntil(timeout: 3) { h.tracked.media.count >= 2 }

        // A frame from the SUPERSEDED socket must not clear the episode.
        initial.fireInboundRealFrameForTest()
        await pumpMainActor(10)
        XCTAssertTrue(h.coordinator.debugRecoveryActive,
                      "an old-generation frame cleared a newer recovery")

        // Fresh media on the replacement clears it; the call survives.
        let replacement = h.tracked.media[1]
        await waitUntil(timeout: 3) { h.coordinator.debugWSMediaIsInstalled(replacement) }
        replacement.fireInboundRealFrameForTest()
        await waitUntil(timeout: 2) { !h.coordinator.debugRecoveryActive }
        XCTAssertFalse(h.coordinator.debugRecoveryActive)
        XCTAssertTrue(h.callKit.ended.isEmpty, "a recovered call must never end")
        XCTAssertTrue(h.api.hangups.isEmpty, "a recovered call must never be hung up")
        h.coordinator.handleProviderReset()
    }

    // MARK: 2. Down / up edges

    func testUnreachableEdgeBurnsNoAttemptsAndReachableEdgeRecovers() async throws {
        let h = makeWSSCall(scripts: [WSScript(), WSScript()])
        await startWSSCall(h)
        let initial = h.tracked.media[0]
        initial.fireInboundRealFrameForTest()
        await pumpMainActor(3)
        try? await Task.sleep(nanoseconds: 300_000_000)

        // The path goes unusable: no attempt may be spent on a dead network.
        h.observer.emit(.becameUnreachable(generation: 1))
        await pumpMainActor(30)
        XCTAssertFalse(h.coordinator.debugRecoveryActive,
                       "attempts must not run while the OS reports no usable path")
        XCTAssertEqual(h.tracked.media.count, 1)

        // The path returns: the stale carrier is revalidated at the edge and
        // the bounded recovery starts immediately.
        h.observer.emit(.becameReachable(generation: 2))
        await waitUntil(timeout: 2) { h.coordinator.debugRecoveryActive }
        XCTAssertTrue(h.coordinator.debugRecoveryActive)
        await waitUntil(timeout: 3) { h.tracked.media.count >= 2 }
        h.coordinator.handleProviderReset()
    }

    // MARK: 3. Duplicate / stale path events are fenced (no hot loop)

    func testDuplicateAndStalePathEventsCannotStormOrArm() async throws {
        // A wide path-change grace isolates the EVENT fencing under test:
        // neither the immediate check nor the one-shot recheck may act.
        let h = makeWSSCall(scripts: [WSScript(), WSScript()], pathChangeGrace: 30)
        await startWSSCall(h)
        let initial = h.tracked.media[0]
        // A real change while the carrier is genuinely fresh: no recovery —
        // a working socket is never torn down on a signature change alone.
        initial.fireInboundRealFrameForTest()
        await pumpMainActor(3)
        h.observer.emit(.pathChanged(generation: 1, previous: cellularSignature))
        await pumpMainActor(20)
        XCTAssertFalse(h.coordinator.debugRecoveryActive)
        XCTAssertEqual(h.tracked.media.count, 1)

        // Replays of the SAME generation and a stale generation are ignored.
        for _ in 0..<10 {
            h.observer.emit(.pathChanged(generation: 1, previous: cellularSignature))
        }
        h.observer.emit(.pathChanged(generation: 0, previous: cellularSignature))
        await pumpMainActor(30)
        XCTAssertEqual(h.coordinator.debugNetworkGeneration, 1)
        XCTAssertFalse(h.coordinator.debugRecoveryActive)
        XCTAssertEqual(h.tracked.media.count, 1)

        // Even when the carrier later goes stale, a REPLAYED generation must
        // not re-arm anything.
        try? await Task.sleep(nanoseconds: 300_000_000)
        for _ in 0..<10 {
            h.observer.emit(.pathChanged(generation: 1, previous: cellularSignature))
        }
        await pumpMainActor(30)
        XCTAssertFalse(h.coordinator.debugRecoveryActive)
        XCTAssertEqual(h.tracked.media.count, 1)
        h.coordinator.handleProviderReset()
    }

    /// A fresh carrier at the event instant is NOT cut over — but if the real
    /// cutover lands moments later, the one-shot recheck must arm the
    /// recovery promptly (well before the stall backstop) so the user's audio
    /// gap stays short.
    func testPathTransitionRechecksAFreshCarrierAndRecoversPromptly() async throws {
        let h = makeWSSCall(scripts: [WSScript(), WSScript()], stallGrace: 30)
        await startWSSCall(h)
        let initial = h.tracked.media[0]
        // A frame THIS instant: the carrier is fresh when NWPathMonitor
        // reports the transition (it often fires before the interface dies).
        initial.fireInboundRealFrameForTest()
        await pumpMainActor(3)
        h.observer.emit(.pathChanged(generation: 1, previous: wifiSignature))
        await pumpMainActor(10)
        XCTAssertFalse(h.coordinator.debugRecoveryActive,
                       "a genuinely fresh carrier must not be cut over at the event instant")
        // Frames stop with the real cutover: the recheck arms within the
        // short grace — the 30 s stall backstop cannot be responsible.
        await waitUntil(timeout: 2) { h.coordinator.debugRecoveryActive }
        XCTAssertTrue(h.coordinator.debugRecoveryActive)
        await waitUntil(timeout: 3) { h.tracked.media.count >= 2 }
        let replacement = h.tracked.media[1]
        await waitUntil(timeout: 3) { h.coordinator.debugWSMediaIsInstalled(replacement) }
        replacement.fireInboundRealFrameForTest()
        await waitUntil(timeout: 2) { !h.coordinator.debugRecoveryActive }
        XCTAssertFalse(h.coordinator.debugRecoveryActive)
        XCTAssertTrue(h.callKit.ended.isEmpty)
        h.coordinator.handleProviderReset()
    }

    // MARK: 4. Media-stall backstop (unstable link, no path event)

    /// The FIRST observer snapshot is identity only: it must never look like
    /// a cutover, even when the carrier is already stale at that instant.
    func testBaselineSnapshotIsNeverACutover() async throws {
        let h = makeWSSCall(scripts: [WSScript(), WSScript()])
        await startWSSCall(h)
        let initial = h.tracked.media[0]
        initial.fireInboundRealFrameForTest()
        await pumpMainActor(3)
        try? await Task.sleep(nanoseconds: 300_000_000) // carrier now stale
        h.observer.emit(.baseline(generation: 1))
        await pumpMainActor(30)
        XCTAssertEqual(h.coordinator.debugNetworkGeneration, 1)
        XCTAssertFalse(h.coordinator.debugRecoveryActive,
                       "the first snapshot must never revalidate or arm recovery")
        XCTAssertEqual(h.tracked.media.count, 1)
        h.coordinator.handleProviderReset()
    }

    /// A fresh path interrupts a recovery attempt still negotiating over the
    /// dead path: the hung candidate is cancelled and the next attempt runs
    /// immediately, long before the 15 s handshake timeout.
    func testFreshPathInterruptsAStaleInFlightRecoveryAttempt() async throws {
        let hung = WSScript(ready: false)
        let h = makeWSSCall(scripts: [WSScript(), hung, WSScript()])
        await startWSSCall(h)
        let initial = h.tracked.media[0]
        initial.fireInboundRealFrameForTest()
        await pumpMainActor(3)

        // The old path breaks (transport failure starts the bounded
        // recovery); attempt 0 hangs in the handshake on the dead path.
        h.tracked.sockets[0].deliver(.failure(URLError(.networkConnectionLost)))
        await waitUntil(timeout: 2) { h.coordinator.debugRecoveryActive }
        await waitUntil(timeout: 2) { h.tracked.media.count >= 2 }
        XCTAssertTrue(h.tracked.sockets[1].resumed)
        XCTAssertFalse(h.tracked.sockets[1].cancelled,
                       "the hung attempt must still be negotiating")

        // The fresh path appears: the stale in-flight attach is interrupted
        // and the next attempt stages a replacement PROMPTLY.
        h.observer.emit(.pathChanged(generation: 1, previous: wifiSignature))
        await waitUntil(timeout: 2) { h.tracked.sockets[1].cancelled }
        XCTAssertTrue(h.tracked.sockets[1].cancelled,
                      "the stale in-flight attach must be interrupted")
        await waitUntil(timeout: 2) { h.tracked.media.count >= 3 }
        XCTAssertGreaterThanOrEqual(h.tracked.media.count, 3,
                                    "the next attempt must run immediately, not after the 15 s handshake timeout")

        let replacement = h.tracked.media[2]
        await waitUntil(timeout: 3) { h.coordinator.debugWSMediaIsInstalled(replacement) }
        replacement.fireInboundRealFrameForTest()
        await waitUntil(timeout: 2) { !h.coordinator.debugRecoveryActive }
        XCTAssertFalse(h.coordinator.debugRecoveryActive)
        XCTAssertTrue(h.callKit.ended.isEmpty)
        h.coordinator.handleProviderReset()
    }

    func testStallWatchdogArmsOnceThenFreshMediaClearsAndTheCallSurvives() async throws {
        let h = makeWSSCall(scripts: [WSScript(), WSScript()],
                            stallGrace: 0.3, stallTick: 0.05)
        await startWSSCall(h)
        let initial = h.tracked.media[0]
        // A connected-but-never-proven carrier is not a stall.
        try? await Task.sleep(nanoseconds: 500_000_000)
        XCTAssertFalse(h.coordinator.debugRecoveryActive)

        // Genuine media, then the link goes quiet: connection state stays
        // "connected" (the brittle case), but frames and pongs stopped.
        initial.fireInboundRealFrameForTest()
        await waitUntil(timeout: 3) { h.coordinator.debugRecoveryActive }
        XCTAssertTrue(h.coordinator.debugStallArmed)
        await waitUntil(timeout: 3) { h.tracked.media.count >= 2 }
        XCTAssertEqual(h.tracked.media.count, 2, "one arm per stall episode")

        // Fresh media on the replacement clears the episode; ongoing frames
        // keep the call healthy (the watchdog re-arms only on a NEW stall).
        let replacement = h.tracked.media[1]
        await waitUntil(timeout: 3) { h.coordinator.debugWSMediaIsInstalled(replacement) }
        replacement.fireInboundRealFrameForTest()
        await waitUntil(timeout: 2) { !h.coordinator.debugRecoveryActive }
        for _ in 0..<4 {
            replacement.fireInboundRealFrameForTest()
            try? await Task.sleep(nanoseconds: 100_000_000)
        }
        XCTAssertTrue(h.callKit.ended.isEmpty, "a recovered call must survive")
        XCTAssertFalse(h.coordinator.debugRecoveryActive)
        h.coordinator.handleProviderReset()
    }

    /// Build-67 (user-confirmed stuck silence: total loss of actual audio
    /// while the call UI still showed an ongoing call; the user hung up
    /// manually). The 0.3.55(65) trace shows the socket answering pongs
    /// (control plane alive) while real media can stall: the relay streams
    /// one frame per 20 ms slot for the whole call — silence is DELIVERED as
    /// silent frames — so missing REAL frames are a dead media path, not
    /// content silence. The bounded recovery must arm, and the watchdog must
    /// never hang the call up by itself.
    func testRelayMediaStarvationWithALiveControlPlaneStillArmsRecovery() async throws {
        let h = makeWSSCall(scripts: [WSScript(), WSScript()],
                            stallGrace: 0.2, stallTick: 0.05)
        await startWSSCall(h)
        let initial = h.tracked.media[0]
        initial.fireInboundRealFrameForTest()
        await pumpMainActor(3)

        // The control plane stays ALIVE: a real ping is answered by a real
        // pong, so `freshPingSamples` is non-empty at the stall check.
        initial.sendPingForTest()
        h.tracked.sockets[0].deliver(.success(.string(#"{"type":"pong","t":1}"#)))
        await pumpMainActor(5)
        XCTAssertFalse(initial.freshPingSamples(within: 5).isEmpty,
                       "this scenario requires a proven-live control plane")

        // Real media frames stop; the socket stays connected and pong-alive.
        try? await Task.sleep(nanoseconds: 300_000_000)
        h.coordinator.debugRunStallCheckForTest(grace: 0.2)
        XCTAssertTrue(h.coordinator.debugStallArmed,
                      "media starvation behind a live control plane must still arm the bounded recovery")
        await waitUntil(timeout: 3) { h.coordinator.debugRecoveryActive }
        XCTAssertTrue(h.callKit.ended.isEmpty,
                      "the stall watchdog arms recovery; it never hangs the call up by itself")
        h.coordinator.handleProviderReset()
    }

    /// Content silence is NOT failure: frames keep ARRIVING (silent PCM is
    /// still a delivered frame — the relay streams one per 20 ms slot), so a
    /// quiet far end must never arm the stall recovery.
    func testContentSilenceWithFramesFlowingNeverArmsTheStallBackstop() async throws {
        let h = makeWSSCall(scripts: [WSScript(), WSScript()],
                            stallGrace: 0.2, stallTick: 0.05)
        await startWSSCall(h)
        let initial = h.tracked.media[0]
        for _ in 0..<4 {
            initial.fireInboundRealFrameForTest()
            try? await Task.sleep(nanoseconds: 80_000_000)
        }
        h.coordinator.debugRunStallCheckForTest(grace: 0.2)
        XCTAssertFalse(h.coordinator.debugStallArmed,
                       "frames still arriving must never arm the stall recovery")
        XCTAssertFalse(h.coordinator.debugRecoveryActive)
        h.coordinator.handleProviderReset()
    }
    // MARK: 5. Remote hangup during an open episode ends immediately

    func testRemoteHangupDuringPathRecoveryEndsImmediately() async throws {
        let h = makeWSSCall(scripts: [WSScript(), WSScript()])
        await startWSSCall(h)
        let initial = h.tracked.media[0]
        initial.fireInboundRealFrameForTest()
        await pumpMainActor(3)
        try? await Task.sleep(nanoseconds: 300_000_000)
        h.observer.emit(.pathChanged(generation: 1, previous: wifiSignature))
        await waitUntil(timeout: 2) { h.coordinator.debugRecoveryActive }

        let now = Date().unixMilliseconds
        h.api.activeRecordForPoll = CallRecord(
            id: h.uuid.uuidString.lowercased(), gatewayID: "gw", lineID: "gw:line",
            direction: .outbound, peer: "555-0123", state: .active,
            startedAt: now - 2_000, connectedAt: now - 1_900, endedAt: now,
            endReason: "hangup", recordingId: nil, recordingState: nil,
            recordingDurationMs: nil, mediaTransport: "ws")
        await waitUntil(timeout: 6) { h.callKit.ended.contains { $0.uuid == h.uuid } }
        XCTAssertTrue(h.callKit.ended.contains { $0.uuid == h.uuid })
        XCTAssertFalse(h.coordinator.debugRecoveryActive,
                       "the episode must not survive a remote hangup")
        h.coordinator.handleProviderReset()
    }

    // MARK: QUIC pinned-carrier harness

    private struct QUICHarness {
        let coordinator: CallCoordinator
        let api: FakeGatewayAPI
        let observer: FakeCallPathObserver
        let wsSocket: FakeMediaSocket
        let quicSocket: FakeQUICMediaSocket
        let uuid: UUID
    }

    private func makeQUICCall() -> QUICHarness {
        let api = FakeGatewayAPI()
        api.iceConfigOverride = ICEConfiguration(
            policy: "all", iceServers: [],
            expiresAt: "2026-10-01T00:00:00Z",
            mediaTransports: ["ws", "quic"], mediaQuic: true)
        api.mediaWSRequestOverride = readyURL
        api.quicInfoResult = .success(try! QUICPinFixture.decode(QUICPinFixture.validDocument))
        api.identityResponse = QUICPinFixture.identityResponse
        api.quicCandidateResult = .success(QUICPunchInfo(
            punchToken: "ptok", gatewayCandidate: "203.0.113.7:4433",
            punchAtMs: 0, deadlineMs: 8000))
        let observer = FakeCallPathObserver()
        let wsSocket = FakeMediaSocket()
        wsSocket.scripted = [
            .message(.success(.string(#"{"type":"ready","epoch":2}"#))),
            .park,
        ]
        let quicSocket = FakeQUICMediaSocket()
        let coordinator = CallCoordinator(
            api: api, callKit: FakeCallKit(),
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            mediaRecoveryWindow: 45, gatewayID: gatewayID,
            wsMediaFactory: {
                WebSocketCallMedia(socketFactory: { _, _ in wsSocket }, audioGraph: Graph())
            },
            routeProbeFactory: { FakeDirectProbe() },
            quicMediaFactory: {
                QUICCallMedia(
                    socketFactory: { _ in quicSocket },
                    audioGraph: Graph(),
                    handshakeTimeout: 0.5, bindTimeout: 0.5)
            },
            pathObserverFactory: { observer })
        coordinator.quicTrustedGatewayIdentity = (
            gatewayId: QUICPinFixture.gatewayId,
            fingerprint: QUICPinFixture.fingerprint)
        coordinator.quicSTUNResolver = { _, _ in
            STUNBindingResult(
                local: HostPort(host: "192.0.2.5", port: 51000),
                mapped: HostPort(host: "198.51.100.9", port: 54321))
        }
        coordinator.quicPromoteReplyTimeout = 0.4
        coordinator.quicEvaluationTickInterval = 0.1
        coordinator.quicAttemptTimeoutForTest = 30
        // The stall backstop is exercised in its own WSS test; keep it out of
        // the QUIC timing here.
        coordinator.mediaStallGrace = 30
        coordinator.pathChangeStallGrace = 0.2
        return QUICHarness(coordinator: coordinator, api: api, observer: observer,
                           wsSocket: wsSocket, quicSocket: quicSocket, uuid: UUID())
    }

    private func startQUICOutgoing(_ h: QUICHarness) async {
        let uuid = h.uuid
        let record = makeCallRecord(id: uuid.uuidString.lowercased(), state: .outgoingDialing)
        h.api.dialResult = .success(record)
        h.api.activeRecordForPoll = makeCallRecord(id: record.id, state: .active)
        h.coordinator.startOutgoing(peer: "555-0142", uuid: uuid)
        await waitUntil(timeout: 3) {
            h.quicSocket.startedCount == 1 && h.coordinator.debugQUICDirect != nil
        }
    }

    private func lastPingJSON(_ h: QUICHarness) -> String? {
        h.wsSocket.sends.reversed().compactMap {
            if case .string(let text) = $0.message, text.contains("\"ping\"") { return text }
            return nil
        }.first
    }

    private func pingTag(of pingJSON: String) -> Int? {
        guard let range = pingJSON.range(of: "\"t\":") else { return nil }
        let tail = pingJSON[range.upperBound...]
        return Int(tail.prefix { $0.isNumber })
    }

    private func answerRelayPongs(_ h: QUICHarness, count: Int) async {
        var lastAnswered: Int?
        for _ in 0..<count {
            let before = lastAnswered
            h.coordinator.debugWSMediaForTest?.sendPingForTest()
            await waitUntil(timeout: 3) {
                guard let ping = self.lastPingJSON(h) else { return false }
                return self.pingTag(of: ping) != before
            }
            guard let ping = lastPingJSON(h), let t = pingTag(of: ping) else { return }
            lastAnswered = t
            h.wsSocket.deliver(.success(.string(#"{"type":"pong","t":\#(t),"buf":2}"#)))
            await pumpMainActor(10)
        }
    }

    private func echoQUICProbe(_ h: QUICHarness, rttNanoseconds: UInt64) async {
        let before = h.quicSocket.sentProbes.count
        h.coordinator.debugQUICDirect?.quicCallMedia?.sendProbeForTest()
        await waitUntil(timeout: 3) { h.quicSocket.sentProbes.count > before }
        guard h.quicSocket.sentProbes.count > before,
              let tag = h.quicSocket.sentProbes.last else { return }
        try? await Task.sleep(nanoseconds: rttNanoseconds)
        h.quicSocket.emitProbeResponse(tag, arrivedAt: Date())
        await pumpMainActor(10)
    }

    private func proveQUICGreetingAndMedia(_ h: QUICHarness, epoch: UInt64 = 7) async {
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

    private func promoteCommands(_ h: QUICHarness) -> [String] {
        h.wsSocket.sends.compactMap {
            if case .string(let text) = $0.message, text.contains("\"promote\"") {
                return text
            }
            return nil
        }
    }

    private func confirmQUICOwner(_ h: QUICHarness) async {
        await startQUICOutgoing(h)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(10)
        await proveQUICGreetingAndMedia(h)
        await answerRelayPongs(h, count: 1)
        await echoQUICProbe(h, rttNanoseconds: 15_000_000)
        await waitUntil(timeout: 3) { !self.promoteCommands(h).isEmpty }
        guard let command = promoteCommands(h).first,
              let range = command.range(of: "\"nonce\":\"") else {
            XCTFail("no QUIC promote command")
            return
        }
        let rest = command[range.upperBound...]
        let nonce = String(rest[..<rest.firstIndex(of: "\"")!])
        h.wsSocket.deliver(.success(.string(
            #"{"type":"promoted","epoch":8,"nonce":"\#(nonce)","primary":"quic","standby":"ws"}"#)))
        await waitUntil(timeout: 3) {
            h.coordinator.debugQUICDirect?.debugIncumbent == .quicDirect
        }
    }

    private func fastPolicyConfig() -> RouteSelectionPolicy.Config {
        RouteSelectionPolicy.Config(
            minimumHoldInterval: 0.3,
            switchCooldown: 0.3,
            sustainedWindow: 2,
            sustainedWindowSpan: 6,
            sustainedMinDuration: 0.2)
    }

    // MARK: 6. QUIC pinned carrier: fresh survives, stale is retired + re-dialed

    func testPathChangeKeepsFreshQUICCarrierAndRetiresStalePinnedOneWithFreshEpoch() async throws {
        let h = makeQUICCall()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        h.coordinator.debugQUICDirect?.quicRetryInitialBackoff = 0.05
        await confirmQUICOwner(h)
        let controller = h.coordinator.debugQUICDirect!
        XCTAssertTrue(controller.ownsAudio)
        XCTAssertFalse(h.quicSocket.closed)
        let firstEpoch = controller.debugNetworkEpoch
        let postsBefore = h.api.quicCandidatePosts.count

        // (a) A path signal while the carrier is genuinely fresh: the
        // working socket is NOT torn down, no recovery is armed.
        h.quicSocket.emitAudioEnvelope(epoch: 8, frame: Data(count: 160))
        await pumpMainActor(5)
        h.observer.emit(.pathChanged(generation: 1, previous: wifiSignature))
        await pumpMainActor(10)
        XCTAssertFalse(h.quicSocket.closed,
                       "a still-fresh pinned socket must survive a path signal")
        XCTAssertFalse(h.coordinator.debugRecoveryActive)

        // (b) The interface is gone: frames and probe echoes stop. The next
        // path edge revalidates immediately — the pinned socket cannot
        // migrate, so it is retired through the socket-death lifecycle and
        // the bounded recovery arms.
        try? await Task.sleep(nanoseconds: 300_000_000)
        h.observer.emit(.pathChanged(generation: 2, previous: cellularSignature))
        await waitUntil(timeout: 2) { h.quicSocket.closed }
        XCTAssertTrue(h.quicSocket.closed,
                      "a stale pinned carrier must be retired on a path change")
        await waitUntil(timeout: 2) { h.coordinator.debugRecoveryActive }
        XCTAssertTrue(h.coordinator.debugRecoveryActive)

        // (c) The quarantined candidate re-dials promptly and the next dial
        // mints a FRESH network epoch (old-path samples can never mark the
        // new path decision-ready).
        await waitUntil(timeout: 3) { h.api.quicCandidatePosts.count > postsBefore }
        XCTAssertGreaterThan(h.api.quicCandidatePosts.count, postsBefore)
        XCTAssertNotEqual(h.api.quicCandidatePosts.last?.networkEpoch, firstEpoch)
        XCTAssertNotEqual(controller.debugNetworkEpoch, firstEpoch)
        h.coordinator.handleProviderReset()
    }

    /// A QUIC carrier that was still fresh at the instant of a path signal is
    /// kept — but if it then stalls with NO further path event, the media
    /// backstop must retire the pinned zombie instead of preserving it
    /// forever behind the recovered call.
    func testStallWatchdogRetiresAZombieQUICCarrierWithoutAPathEvent() async throws {
        let h = makeQUICCall()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        await confirmQUICOwner(h)
        XCTAssertTrue(h.coordinator.debugQUICDirect!.ownsAudio)

        // From here a short stall grace applies: a fresh frame, then the
        // path signal arrives while the socket is genuinely fresh (kept).
        h.coordinator.mediaStallGrace = 0.25
        h.quicSocket.emitAudioEnvelope(epoch: 8, frame: Data(count: 160))
        await pumpMainActor(5)
        h.observer.emit(.pathChanged(generation: 1, previous: wifiSignature))
        await pumpMainActor(10)
        XCTAssertFalse(h.quicSocket.closed,
                       "a still-fresh pinned socket must survive a path signal")

        // Frames stop for good and NO further path event arrives: the
        // watchdog must retire the zombie and arm the bounded recovery.
        await waitUntil(timeout: 3) { h.quicSocket.closed }
        XCTAssertTrue(h.quicSocket.closed,
                      "a stalled pinned QUIC carrier must be retired, never preserved as a zombie")
        XCTAssertTrue(h.coordinator.debugRecoveryActive)
        h.coordinator.handleProviderReset()
    }

    /// Build-67 consistency (same contract as the relay branch): the QUIC
    /// gateway also streams a downlink frame every 20 ms — silence included —
    /// so a fresh probe ECHO is control-plane liveness only. A stalled QUIC
    /// media plane with live echoes must still be retired and recovered.
    func testQUICMediaStarvationWithLiveProbeEchoesStillRetiresAndRecovers() async throws {
        let h = makeQUICCall()
        h.coordinator.quicPolicyConfigForTest = fastPolicyConfig()
        await confirmQUICOwner(h)
        XCTAssertTrue(h.coordinator.debugQUICDirect!.ownsAudio)

        h.coordinator.mediaStallGrace = 0.25
        h.quicSocket.emitAudioEnvelope(epoch: 8, frame: Data(count: 160))
        await pumpMainActor(5)

        // Real frames stop, but the probe plane keeps echoing: a live echo
        // must NOT mask the media starvation.
        try? await Task.sleep(nanoseconds: 300_000_000)
        await echoQUICProbe(h, rttNanoseconds: 0)
        let probes = h.coordinator.debugQUICDirect?.quicCallMedia?
            .freshProbeSamples(within: 5) ?? []
        XCTAssertFalse(probes.isEmpty,
                       "this scenario requires a proven-live probe plane")

        await waitUntil(timeout: 3) { h.quicSocket.closed }
        XCTAssertTrue(h.quicSocket.closed,
                      "QUIC media starvation with live probes must retire the carrier")
        XCTAssertTrue(h.coordinator.debugRecoveryActive)
        h.coordinator.handleProviderReset()
    }
}
