import XCTest
import AVFoundation
@testable import CallRelay

/// Coordinator-level (not policy-only) tests for build-48 recovery:
/// session-identity + episode fencing of fresh-media proof, and the auto
/// current-failure → alternate attempt actually running through the route
/// controller.
@MainActor
final class RecoveryCoordinatorFencingTests: XCTestCase {
    private let gatewayID = "gw-recovery-fencing"
    private let readyURL = URLRequest(url: URL(string: "wss://example.test/media")!)

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

    private func makeAPI(transports: [String]) -> FakeGatewayAPI {
        let api = FakeGatewayAPI()
        api.iceConfigOverride = ICEConfiguration(
            policy: "all", iceServers: [],
            expiresAt: "2026-10-01T00:00:00Z",
            mediaTransports: transports)
        api.mediaWSRequestOverride = readyURL
        return api
    }

    override func setUp() {
        super.setUp()
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
    }

    override func tearDown() {
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        AudioSessionBridge.shared.didDeactivate(AVAudioSession.sharedInstance())
        super.tearDown()
    }

    private func startOutgoing(_ coordinator: CallCoordinator, _ api: FakeGatewayAPI) -> UUID {
        let uuid = UUID()
        let record = makeCallRecord(id: uuid.uuidString.lowercased(), state: .outgoingDialing)
        api.dialResult = .success(record)
        api.activeRecordForPoll = makeCallRecord(id: record.id, state: .active)
        coordinator.startOutgoing(peer: "555-0123", uuid: uuid)
        return uuid
    }

    /// Relay-only call; failing the socket starts recovery. Attempt 1 stages
    /// and promotes a REPLACEMENT socket. A fresh-media proof fired by the
    /// OLD (superseded) socket must be rejected; the installed socket's own
    /// proof clears recovery.
    func testOldSocketProofAfterReplacementIsRejected() async throws {
        let api = makeAPI(transports: ["ws"])
        let script1 = WSScript()
        let script2 = WSScript()
        var scripts = [script1, script2]
        var created: [WebSocketCallMedia] = []
        let callKit = FakeCallKit()
        let coordinator = CallCoordinator(
            api: api, callKit: callKit,
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            gatewayID: gatewayID,
            wsMediaFactory: {
                let script = scripts.isEmpty ? WSScript() : scripts.removeFirst()
                let media = WebSocketCallMedia(
                    socketFactory: { _, _ in script.socket }, audioGraph: script.graph)
                created.append(media)
                return media
            },
            routeCadence: nil)
        _ = startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { script1.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(30)
        XCTAssertGreaterThanOrEqual(created.count, 1)

        // Socket dies: recovery opens and attempt 1 (immediate) stages a
        // fresh relay, promoted once ready.
        script1.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await waitUntil(timeout: 5) { created.count >= 2 && script2.socket.resumed }
        await pumpMainActor(40)
        let superseded = created[0]
        let installed = created[1]

        // Old socket's proof must NOT clear the active recovery.
        superseded.fireInboundRealFrameForTest()
        await pumpMainActor(10)
        XCTAssertTrue(coordinator.debugRecoveryActive,
                      "a superseded socket's frame cleared recovery (fence breach)")

        // The installed socket's proof clears it.
        installed.fireInboundRealFrameForTest()
        await waitUntil(timeout: 3) { !coordinator.debugRecoveryActive }
        XCTAssertFalse(coordinator.debugRecoveryActive)
        coordinator.handleProviderReset()
    }

    /// AUTO mode: after the relay dies and its own recovery keeps failing,
    /// later attempts must cross to the ALTERNATE route through the route
    /// controller (probe attach + commit), not be silently dropped.
    func testAutoRelayFailureReachesAlternateDirectAttempt() async throws {
        let api = makeAPI(transports: ["ws", "ice"])
        let script1 = WSScript()
        var scripts = [script1]
        let probe = FakeDirectProbe()
        probe.connected = true
        probe.mediaReady = true
        var probes = [probe]
        let callKit = FakeCallKit()
        let coordinator = CallCoordinator(
            api: api, callKit: callKit,
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            gatewayID: gatewayID,
            wsMediaFactory: {
                let script = scripts.isEmpty ? WSScript() : scripts.removeFirst()
                return WebSocketCallMedia(
                    socketFactory: { _, _ in script.socket }, audioGraph: script.graph)
            },
            routeProbeFactory: { probes.isEmpty ? FakeDirectProbe() : probes.removeFirst() },
            routeCadence: nil)
        _ = startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { script1.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(40)

        // The relay dies; recovery begins on the current (relay) route.
        script1.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await pumpMainActor(20)
        XCTAssertTrue(coordinator.debugRecoveryActive)

        // Within the 45 s window the alternate direct attempt MUST run via
        // the route controller: a probe is attached and committed.
        await waitUntil(timeout: 8) {
            !api.attachProbeCalls.isEmpty || !api.commitCalls.isEmpty
        }
        let crossed = !api.attachProbeCalls.isEmpty || !api.commitCalls.isEmpty
        XCTAssertTrue(crossed,
                      "auto recovery never attempted the alternate direct route")
        coordinator.handleProviderReset()
    }

    // MARK: - Build-49 expiry semantics (coordinator level)

    /// Expiry claims terminal locally FIRST: with the conference close RPC
    /// hung, the media-failure deadline still tears the conference and every
    /// call down locally; the parked close resolves afterwards and changes
    /// nothing.
    func testConferenceExpiryTearsDownLocallyBeforeHungCloseCompletes() async throws {
        let api = makeAPI(transports: ["ws"])
        api.conferenceMediaWSRequestOverride = readyURL
        var scripts: [WSScript] = [WSScript(), WSScript(), WSScript()]
        var created: [WebSocketCallMedia] = []
        var sockets: [FakeMediaSocket] = []
        let coordinator = CallCoordinator(
            api: api, callKit: FakeCallKit(),
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            mediaRecoveryWindow: 1.0, gatewayID: gatewayID,
            wsMediaFactory: {
                let script = scripts.isEmpty ? WSScript() : scripts.removeFirst()
                let media = WebSocketCallMedia(
                    socketFactory: { _, _ in script.socket }, audioGraph: script.graph)
                created.append(media)
                sockets.append(script.socket)
                return media
            },
            routeCadence: nil)

        // Two answered calls merged into a WSS conference (no audio
        // activation: every recovery promotion rolls back, no fresh frame
        // ever arrives, so the deadline ends it).
        func answer(_ id: String, _ peer: String) async {
            let uuid = UUID()
            coordinator.registerIncoming(
                gatewayId: id, uuid: uuid,
                record: makeCallRecord(id: id, state: .incomingRinging, direction: .inbound, peer: peer))
            try? await coordinator.answerIncoming(uuid: uuid)
        }
        await answer("call-A", "1001")
        await answer("call-B", "1002")
        await waitUntil(timeout: 5) { api.answers.count == 2 }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(30)
        let legB = makeCallRecord(id: "call-B", state: .active, direction: .inbound, peer: "1002")
        let legA = makeCallRecord(id: "call-A", state: .active, direction: .inbound, peer: "1001")
        api.mergeResult = makeConferenceRecord(id: "conf-1", legs: [legB, legA])
        api.conferenceSnapshot = makeConferenceRecord(id: "conf-1", legs: [legB, legA])
        coordinator.mergeHeldCalls()
        await waitUntil(timeout: 5) { coordinator.conferenceRecord != nil }
        await waitUntil(timeout: 5) { coordinator.debugConferenceWSMedia() != nil }
        await pumpMainActor(30)
        // created[] and sockets[] grow in lockstep: pair the installed host
        // media with its socket by index.
        let hostIndex = created.firstIndex {
            coordinator.debugWSConferenceMediaIsInstalled($0)
        }!

        // Arm the HUNG server close, then kill the host socket.
        api.armCloseConferenceWait()
        sockets[hostIndex].deliver(.failure(URLError(.networkConnectionLost)))
        // The conference must be gone locally well before the parked close
        // resolves (the 1 s deadline), and no recovery episode outlives it.
        await waitUntil(timeout: 6) { coordinator.conferenceRecord == nil }
        XCTAssertFalse(coordinator.debugRecoveryActive)
        // The best-effort close is fired from a task off the local teardown:
        // wait for it to actually reach the parked RPC.
        await waitUntil(timeout: 3) { api.closeConferenceParked }
        XCTAssertTrue(api.closeConferenceParked, "the close RPC should still be parked")
        // Resuming the hung close must be a no-op against the ended state.
        api.resumeCloseConference(with: .success(()))
        await pumpMainActor(20)
        XCTAssertNil(coordinator.conferenceRecord)
        XCTAssertFalse(coordinator.debugRecoveryActive)
        coordinator.handleProviderReset()
    }

    /// After 1:1 expiry ends the call, a further loss on the dead transport
    /// must not reopen a recovery episode (generation fence + ended state).
    func testExpiryThenFurtherLossDoesNotReopenEpisode() async throws {
        let api = makeAPI(transports: ["ws"])
        let script1 = WSScript()
        var scripts = [script1]
        let callKit = FakeCallKit()
        let coordinator = CallCoordinator(
            api: api, callKit: callKit,
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            mediaRecoveryWindow: 1.0, gatewayID: gatewayID,
            wsMediaFactory: {
                let script = scripts.isEmpty ? WSScript() : scripts.removeFirst()
                return WebSocketCallMedia(
                    socketFactory: { _, _ in script.socket }, audioGraph: script.graph)
            },
            routeCadence: nil)
        let uuid = startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { script1.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(30)

        script1.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await waitUntil(timeout: 3) { coordinator.debugRecoveryActive }
        // Deadline ends the call truthfully (no fresh media, 1 s window).
        await waitUntil(timeout: 6) { callKit.ended.contains { $0.uuid == uuid } }
        XCTAssertFalse(coordinator.debugRecoveryActive)
        // Build-63 determinism: the expiry teardown issues its hangup
        // asynchronously; on a loaded runner that first teardown can land
        // AFTER this fence was sampled (the build-62 flake measured 0 then
        // 1). Wait for the first teardown to quiesce, THEN fence the count
        // — the no-second-teardown invariant is unchanged.
        await waitUntil(timeout: 6) { api.hangups.count >= 1 }
        let hangupsAtExpiry = api.hangups.count

        // A further loss on the dead transport must be inert. (Teardown
        // cancels the socket; deliver only when a receive is still parked —
        // either way no second teardown may occur and no episode reopens.)
        if script1.socket.isParked {
            script1.socket.deliver(.failure(URLError(.networkConnectionLost)))
        }
        await pumpMainActor(30)
        XCTAssertFalse(coordinator.debugRecoveryActive)
        XCTAssertEqual(api.hangups.count, hangupsAtExpiry, "no second teardown")
        coordinator.handleProviderReset()
    }

    /// Build-49 REQUIRED integration proof: relay→direct ALTERNATE recovery
    /// clears the episode ONLY on the adopted peer's real fresh media —
    /// connected/RTT alone and a stale attempt-1 relay frame do not, and the
    /// cleared call survives past the old backoff/deadline without ending.
    func testAlternateRelayToDirectFreshMediaClearsEpisode() async throws {
        let api = makeAPI(transports: ["ws", "ice"])
        let script1 = WSScript()
        let script2 = WSScript()
        var scripts = [script1, script2]
        // The auto route controller spends ONE startup opportunity measuring a
        // detached candidate; give it a DISPOSABLE probe so the scripted
        // recovery probe is consumed by the recovery attempt itself.
        let disposable = FakeDirectProbe()
        disposable.connected = true
        disposable.mediaReady = true
        let probe = FakeDirectProbe()
        probe.connected = true
        probe.mediaReady = true
        var probes = [disposable, probe]
        var created: [WebSocketCallMedia] = []
        let callKit = FakeCallKit()
        let fastCadence = CallRouteController.Cadence(
            autoInterval: 0.05, monitorInterval: 0.05, rttPublishInterval: 0.05,
            candidateTimeout: 0.3, connectPoll: 0.02,
            unknownReconcileTries: 3, unknownReconcileInterval: 0.02,
            attachTimeout: 0.3, comparisonTimeout: 0.8, comparisonPoll: 0.02,
            startupOpportunityTimeout: 0.3, startupMeasurementSeconds: 0.2,
            startupMeasurementPoll: 0.02, directAudioReadyTimeout: 0.2,
            directSignalCheckSeconds: 0.2, directSignalGatePoll: 0.05)
        let coordinator = CallCoordinator(
            api: api, callKit: callKit,
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            gatewayID: gatewayID,
            wsMediaFactory: {
                let script = scripts.isEmpty ? WSScript() : scripts.removeFirst()
                let media = WebSocketCallMedia(
                    socketFactory: { _, _ in script.socket }, audioGraph: script.graph)
                created.append(media)
                return media
            },
            routeProbeFactory: { probes.isEmpty ? FakeDirectProbe() : probes.removeFirst() },
            routeCadence: fastCadence)
        _ = startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { script1.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(40)

        // Relay dies -> episode opens; attempt 1 (relay) stages + promotes a
        // replacement relay but nobody sends fresh media.
        script1.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await waitUntil(timeout: 5) { created.count >= 2 }
        await pumpMainActor(40)
        XCTAssertTrue(coordinator.debugRecoveryActive)

        // Attempt 2 (alternate, direct) commits and adopts the SCRIPTED probe
        // (the startup opportunity consumed the disposable one).
        await waitUntil(timeout: 10) {
            !api.commitCalls.isEmpty && probe.adoptCount >= 1 && disposable.adoptCount == 0
        }
        XCTAssertTrue(probe.recoveryFreshMediaWired,
                      "the adopted peer must carry the captured attempt proof hook")
        // CONNECTED/adoption alone must not clear the episode.
        await pumpMainActor(40)
        XCTAssertTrue(coordinator.debugRecoveryActive,
                      "CONNECTED/adoption alone cleared recovery")
        // A stale attempt-1 relay frame must not clear it either.
        created[1].fireInboundRealFrameForTest()
        await pumpMainActor(20)
        XCTAssertTrue(coordinator.debugRecoveryActive,
                      "a stale relay frame cleared the episode")

        // The adopted peer's FIRST REAL inbound audio clears the episode.
        probe.fireRecoveryFreshMediaForTest()
        await waitUntil(timeout: 3) { !coordinator.debugRecoveryActive }
        XCTAssertFalse(coordinator.debugRecoveryActive)
        // The cleared call survives past the old backoff/deadline boundary.
        try? await Task.sleep(nanoseconds: 1_600_000_000)
        await pumpMainActor(10)
        XCTAssertTrue(callKit.ended.isEmpty, "a recovered call must not end")
        XCTAssertEqual(api.hangups, [], "a recovered call must not be hung up")
        coordinator.handleProviderReset()
    }

    /// Build-49 REQUIRED integration proof: the OTHER direction — after an
    /// alternate→direct adoption with the episode still open, a route-level
    /// fallback back to the relay clears the SAME episode on the staged
    /// relay's fresh media (pending attempt identity), not on CONNECTED.
    func testAlternateDirectThenFallbackRelayFreshMediaClearsEpisode() async throws {
        let api = makeAPI(transports: ["ws", "ice"])
        let script1 = WSScript()
        let script2 = WSScript()
        var scripts = [script1, script2]
        // Disposable probe for the startup opportunity; the scripted probe is
        // for the recovery adoption.
        let disposable = FakeDirectProbe()
        disposable.connected = true
        disposable.mediaReady = true
        let probe = FakeDirectProbe()
        probe.connected = true
        probe.mediaReady = true
        var probes = [disposable, probe]
        var created: [WebSocketCallMedia] = []
        let callKit = FakeCallKit()
        let fastCadence = CallRouteController.Cadence(
            autoInterval: 0.05, monitorInterval: 0.05, rttPublishInterval: 0.05,
            candidateTimeout: 0.3, connectPoll: 0.02,
            unknownReconcileTries: 3, unknownReconcileInterval: 0.02,
            attachTimeout: 0.3, comparisonTimeout: 0.8, comparisonPoll: 0.02,
            startupOpportunityTimeout: 0.3, startupMeasurementSeconds: 0.2,
            startupMeasurementPoll: 0.02, directAudioReadyTimeout: 0.2,
            directSignalCheckSeconds: 0.2, directSignalGatePoll: 0.05)
        let coordinator = CallCoordinator(
            api: api, callKit: callKit,
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            gatewayID: gatewayID,
            wsMediaFactory: {
                let script = scripts.isEmpty ? WSScript() : scripts.removeFirst()
                let media = WebSocketCallMedia(
                    socketFactory: { _, _ in script.socket }, audioGraph: script.graph)
                created.append(media)
                return media
            },
            routeProbeFactory: { probes.isEmpty ? FakeDirectProbe() : probes.removeFirst() },
            routeCadence: fastCadence)
        _ = startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { script1.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(40)

        // Relay dies -> episode; attempt 1 (relay) restages the relay without
        // fresh media; attempt 2 adopts the SCRIPTED direct probe (episode
        // STILL open).
        script1.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await waitUntil(timeout: 5) { created.count >= 2 }
        await pumpMainActor(40)
        await waitUntil(timeout: 10) {
            !api.commitCalls.isEmpty && probe.adoptCount >= 1 && disposable.adoptCount == 0
        }
        XCTAssertTrue(coordinator.debugRecoveryActive)

        // The adopted direct peer now dies: the route monitor falls back to a
        // freshly staged relay DURING the open episode. The staged relay
        // carries the pending attempt identity; its promotion (CONNECTED)
        // alone must not clear, its fresh frame must.
        probe.connected = false
        await waitUntil(timeout: 10) { created.count >= 3 }
        await pumpMainActor(60)
        XCTAssertTrue(coordinator.debugRecoveryActive,
                      "relay fallback promotion (CONNECTED) cleared the episode")
        created[2].fireInboundRealFrameForTest()
        await waitUntil(timeout: 3) { !coordinator.debugRecoveryActive }
        XCTAssertFalse(coordinator.debugRecoveryActive)
        try? await Task.sleep(nanoseconds: 1_600_000_000)
        await pumpMainActor(10)
        XCTAssertTrue(callKit.ended.isEmpty, "a recovered call must not end")
        coordinator.handleProviderReset()
    }

    /// A candidate whose attach completes AFTER expiry must not install
    /// media, prove fresh media, or otherwise resurrect the ended call.
    func testLateCandidateAfterExpiryCannotInstallOrProve() async throws {
        let api = makeAPI(transports: ["ws"])
        let script1 = WSScript()
        // The recovery candidate parks forever (no ready frame)...
        let hung = WSScript(ready: false)
        var scripts = [script1, hung]
        var created: [WebSocketCallMedia] = []
        let callKit = FakeCallKit()
        let coordinator = CallCoordinator(
            api: api, callKit: callKit,
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            mediaRecoveryWindow: 1.0, gatewayID: gatewayID,
            wsMediaFactory: {
                let script = scripts.isEmpty ? WSScript() : scripts.removeFirst()
                let media = WebSocketCallMedia(
                    socketFactory: { _, _ in script.socket }, audioGraph: script.graph)
                created.append(media)
                return media
            },
            routeCadence: nil)
        let uuid = startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { script1.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(30)

        script1.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await waitUntil(timeout: 3) { coordinator.debugRecoveryActive }
        // Let the hung attempt begin (candidate created, connect parked).
        await waitUntil(timeout: 3) { created.count >= 2 }
        // Deadline: expiry cancels the in-flight attempt and ends the call.
        await waitUntil(timeout: 6) { callKit.ended.contains { $0.uuid == uuid } }
        XCTAssertFalse(coordinator.debugRecoveryActive)

        // The late candidate's attach was already cancelled at expiry (the
        // socket's parked receive resumed with an error); if anything is
        // still parked, completing it must not install media or prove
        // anything for the dead episode.
        if hung.socket.isParked {
            hung.socket.deliver(.success(.string(#"{"type":"ready"}"#)))
            await pumpMainActor(40)
        }
        XCTAssertFalse(coordinator.debugWSMediaIsInstalled(created[1]))
        created[1].fireInboundRealFrameForTest()
        await pumpMainActor(20)
        XCTAssertFalse(coordinator.debugRecoveryActive)
        coordinator.handleProviderReset()
    }
}
