import XCTest
@testable import CallRelay
import AVFoundation

/// Build-50 hot-standby COORDINATOR integration tests: direct standby
/// install with real readiness proof (never fresh-on-commit), old-gateway
/// single-path fallback, serialized failure takeover (auto only), and
/// forced-mode preservation.
@MainActor
final class HotStandbyCoordinatorTests: XCTestCase {
    private let gatewayID = "gateway-hotstandby"
    private let readyURL = URLRequest(url: URL(string: "wss://example.test/media")!)

    @MainActor
    private final class WSScript {
        let socket = FakeMediaSocket()
        let graph = Graph()
        init(ready: String?) {
            let script = ready.map { [FakeMediaSocket.Scripted.message(.success(.string($0))), .park] } ?? [FakeMediaSocket.Scripted.park]
            socket.scripted = script
        }
        /// Failing attach script: the first receive is an error (models a
        /// staged relay that cannot connect in the both-down recovery test).
        init(failed: Bool) {
            socket.scripted = [.message(.failure(URLError(.networkConnectionLost)))]
        }
    }

    /// REAL timer-driven cadence (non-nil) with short intervals so many
    /// rounds run quickly. The point vs `routeCadence:nil`: the route's
    /// timer loops actually fire repeatedly through the test.
    private var timerCadence: CallRouteController.Cadence {
        var c = CallRouteController.Cadence()
        c.autoInterval = 0.2
        c.monitorInterval = 0.2
        c.rttPublishInterval = 0.1
        return c
    }

    private func makeAPI(standby: Bool) -> FakeGatewayAPI {
        let api = FakeGatewayAPI()
        api.iceConfigOverride = ICEConfiguration(
            policy: "all", iceServers: [],
            expiresAt: "2026-10-01T00:00:00Z",
            mediaTransports: ["ice", "ws"], mediaStandby: standby)
        api.mediaWSRequestOverride = readyURL
        api.standbyWebSocketResult = readyURL
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

    private final class ScriptBox { var created: [WSScript] = [] }

    private func makeCoordinator(api: FakeGatewayAPI, scripts: [WSScript], probe: FakeDirectProbe, box: ScriptBox,
                                 cadence: CallRouteController.Cadence? = nil) -> CallCoordinator {
        var pool = scripts
        return CallCoordinator(
            api: api, callKit: FakeCallKit(),
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            mediaRecoveryWindow: 45, gatewayID: gatewayID,
            wsMediaFactory: {
                let script = pool.isEmpty ? WSScript(ready: nil) : pool.removeFirst()
                box.created.append(script)
                return WebSocketCallMedia(socketFactory: { _, _ in script.socket }, audioGraph: script.graph)
            },
            routeProbeFactory: { probe },
            routeCadence: cadence)
    }

    private func startOutgoing(_ coordinator: CallCoordinator, _ api: FakeGatewayAPI) -> UUID {
        let uuid = UUID()
        let record = makeCallRecord(id: uuid.uuidString.lowercased(), state: .outgoingDialing)
        api.dialResult = .success(record)
        api.activeRecordForPoll = makeCallRecord(id: record.id, state: .active)
        coordinator.startOutgoing(peer: "555-0199", uuid: uuid)
        return uuid
    }

    /// Relay-primary call on a standby-capable gateway: the DIRECT path is
    /// installed as standby (probe + commit-as-standby), the fence comes from
    /// the commit ack, and the freshness clock starts UNPROVEN — only a real
    /// post-adoption packet advance (the re-armed one-shot fresh-media hook)
    /// proves it. Never fresh-on-commit.
    func testDirectStandbyInstalledWithRealReadinessProof() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(60)

        XCTAssertTrue(coordinator.debugStandbySupported)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled, "direct standby not installed")
        XCTAssertEqual(coordinator.debugHotStandbyFences[.direct]?.token, "fake-token")
        // Unproven at install: commit ack alone never counts.
        XCTAssertFalse(coordinator.debugHotStandbyStandbyFresh, "standby must start unproven")

        // Real post-admission advance proves it (recurring re-arm).
        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        XCTAssertTrue(coordinator.debugHotStandbyStandbyFresh, "real advance did not prove the standby")
        coordinator.handleProviderReset()
    }

    /// Old gateway (no mediaStandby capability): no standby is configured;
    /// the call keeps the proven single path.
    func testOldGatewayKeepsSinglePath() async throws {
        let api = makeAPI(standby: false)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready"}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(60)

        XCTAssertFalse(coordinator.debugStandbySupported)
        XCTAssertFalse(coordinator.debugStandbyDirectInstalled)
        XCTAssertFalse(coordinator.debugStandbyRelayInstalled)
        coordinator.handleProviderReset()
    }

    /// Media failure with a FRESH standby in AUTO mode: the serialized
    /// takeover flag is armed; the recovery episode's first attempt issues
    /// the fenced promote command (transport/token/epoch pinned); a real
    /// fresh-media advance on the adopted standby completes the takeover;
    /// the call never hangs up.
    func testMediaFailureTakesOverFreshStandbySerialized() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        let uuid = startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)
        let current = box.created.last!

        // Prove the standby with a real advance, then kill the CURRENT
        // primary socket (the call may re-establish after didActivate).
        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        XCTAssertTrue(coordinator.debugHotStandbyStandbyFresh)
        current.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await pumpMainActor(60)

        // The serialized takeover ran as the FIRST recovery attempt: the
        // fenced promote command was issued with the exact fencing (the
        // arming flag is consumed by that attempt before this observation).
        XCTAssertEqual(api.standbyPromoteCalls.count, 1)
        XCTAssertEqual(api.standbyPromoteCalls.first?.transport, "ice")
        XCTAssertEqual(api.standbyPromoteCalls.first?.token, "fake-token")

        // The adopted standby must prove BIDIRECTIONAL media progress before
        // the takeover completes; advance both counters, then the call
        // continues on direct.
        probe.advanceMediaForTest(inbound: 10, outbound: 4)
        await pumpMainActor(60)
        XCTAssertFalse(api.hangups.contains(uuid.uuidString.lowercased()), "call must not hang up after takeover")
        coordinator.handleProviderReset()
    }

    /// Advance BOTH media counters while `block` runs: the promotion's
    /// bidirectional audio gate re-baselines per promotion, so pre-switch
    /// advances cannot satisfy it — new inbound AND outbound progress must
    /// land DURING the promotion (exactly what a live probe produces).
    /// Deterministic bidirectional media feeder for a promotion.
    ///
    /// The production promotion gate captures inbound/outbound packet
    /// BASELINES right after the ADM adopt/resume and then requires a LATER
    /// advance on BOTH counters. The old fixture fired ONE advance after a
    /// fixed 12-pump delay, so under full-suite scheduling that advance could
    /// land BEFORE the baselines were captured — the legitimate gate then
    /// timed out and the production recovery (correctly) rolled back and
    /// retried, breaking the one-attempt assertions without any product
    /// defect.
    ///
    /// This feeder advances BOTH counters continuously for the whole block:
    /// the counters only ever grow, so whatever baseline the production gate
    /// captures — before, during or after any ownership step — always has a
    /// strictly later advance to observe. No timing guess, no fixed pump, no
    /// ownership snapshot that could miss an already-occurred step, and the
    /// loop honours cancellation on every iteration. The feeder is cancelled
    /// and awaited AFTER the block (i.e. after the proof), so nothing
    /// outlives its test.
    private func advanceDuringPromotion(_ probe: FakeDirectProbe, inbound: UInt64, outbound: UInt64,
                                        _ block: () async -> Void) async {
        let feeder = Task { @MainActor in
            var tick: UInt64 = 0
            while !Task.isCancelled {
                tick += 1
                probe.advanceMediaForTest(inbound: inbound + tick, outbound: outbound + tick)
                try? await Task.sleep(nanoseconds: 5_000_000)
            }
        }
        // One tick before the block: an ownership step that happens
        // immediately is already covered by the growing counters.
        try? await Task.sleep(nanoseconds: 5_000_000)
        await block()
        feeder.cancel()
        await feeder.value
    }

    /// Reverse/double switch (ICE->WS->ICE->WS->ICE): every manual switch
    /// rides the warm standby; the direct peer is DEMOTED (ADM suspended,
    /// transport kept) and later RESUMED — never re-adopted, never closed —
    /// so zero closeTransport calls happen until hangup.
    func testDoubleSwitchPreservesTransportsWithZeroCloses() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)

        // Switch 1: relay -> DIRECT via the warm standby (target ready first).
        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        XCTAssertTrue(coordinator.debugHotStandbyStandbyFresh)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 3, currentEpoch: 3, primary: "ice", standby: "ws", standbyToken: "ws-tok-2")
        await advanceDuringPromotion(probe, inbound: 10, outbound: 4) {
            await coordinator.selectRouteMode(.direct)
        }
        await pumpMainActor(60)
        XCTAssertEqual(api.standbyPromoteCalls.count, 1)
        XCTAssertEqual(api.standbyPromoteCalls.first?.transport, "ice")
        XCTAssertEqual(probe.adoptCount, 1, "first promotion adopts the standby probe")
        XCTAssertEqual(probe.closeCount, 0, "direct peer must survive demotion")
        XCTAssertFalse(primary.socket.cancelled, "demoted relay socket must stay attached")

        // Switch 2: direct -> RELAY via the demoted relay standby.
        let pcm = PCMUCodec.encode([Int16](repeating: 5, count: 160))
        primary.socket.deliver(.success(.data(pcm)))
        await pumpMainActor(30)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")
        await coordinator.selectRouteMode(.relay)
        await pumpMainActor(60)
        XCTAssertEqual(api.standbyPromoteCalls.count, 2)
        XCTAssertEqual(api.standbyPromoteCalls.last?.transport, "ws")
        XCTAssertEqual(probe.suspendAudioCount, 1, "peer suspended once for demotion")
        XCTAssertEqual(probe.closeCount, 0, "same direct peer across both swaps")

        // Switch 3: relay -> DIRECT again: the DEMOTED peer is RESUMED, not
        // re-adopted (the guarded adopt must not no-op a re-promotion).
        probe.fireRecoveryFreshMediaForTest()
        let pcm2 = PCMUCodec.encode([Int16](repeating: 6, count: 160))
        primary.socket.deliver(.success(.data(pcm2)))
        await pumpMainActor(30)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 5, currentEpoch: 5, primary: "ice", standby: "ws", standbyToken: "ws-tok-3")
        await advanceDuringPromotion(probe, inbound: 22, outbound: 9) {
            await coordinator.selectRouteMode(.direct)
        }
        await pumpMainActor(60)
        XCTAssertEqual(api.standbyPromoteCalls.count, 3)
        XCTAssertEqual(probe.resumeAudioCount, 1, "demoted peer resumed on re-promotion")
        XCTAssertEqual(probe.adoptCount, 1, "the guarded adopt ran exactly once across all swaps")
        XCTAssertEqual(probe.closeCount, 0, "zero closes through ICE->WS->ICE")
        coordinator.handleProviderReset()
    }

    /// Manual forced mode persists through the hot promotion (no auto
    /// switch-back), and a failure of the NON-selected path never crosses
    /// routes: forced direct only recovers direct.
    func testManualForcedModePersistsWithHotPromotion() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)

        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 3, currentEpoch: 3, primary: "ice", standby: "ws", standbyToken: "ws-tok-2")
        await advanceDuringPromotion(probe, inbound: 10, outbound: 4) {
            await coordinator.selectRouteMode(.direct)
        }
        await pumpMainActor(60)
        print("HSLOG after direct: adopt=\(probe.adoptCount) suspend=\(probe.suspendAudioCount) resume=\(probe.resumeAudioCount) promotes=\(api.standbyPromoteCalls.count)")
        XCTAssertEqual(api.standbyPromoteCalls.count, 1)
        XCTAssertEqual(probe.adoptCount, 1, "no legacy fallback: the standby path carried the switch")
        // The forced mode applied WITHOUT teardown: the route reports it.
        XCTAssertEqual(coordinator.debugRouteMode, .direct)
        // The promoted kind's fence is gone; the demoted relay fence is in.
        XCTAssertNil(coordinator.debugHotStandbyFences[.direct])
        XCTAssertNotNil(coordinator.debugHotStandbyFences[.relay])

        // Kill the demoted relay standby socket: silent by design, no
        // recovery episode, no route change (forced direct stays).
        primary.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await pumpMainActor(60)
        XCTAssertEqual(coordinator.debugRouteMode, .direct)
        XCTAssertFalse(coordinator.debugRecoveryActive)
        XCTAssertEqual(api.standbyPromoteCalls.count, 1, "no auto cross-route switch")
        coordinator.handleProviderReset()
    }

    /// Rollback demand balance: when the bidirectional gate fails, the
    /// probe's owned audio teardown runs (no RTCAudioDemand leak) and the
    /// suspended relay graph restarts as the audible owner.
    func testRollbackReleasesOwnedAudioAndRestoresRelay() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)
        coordinator.directStandbyAudioGateTimeout = 0.3

        // Ready the standby, attempt the manual switch, but let the gate
        // time out: NO media progress lands during the promotion.
        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 3, currentEpoch: 3, primary: "ice", standby: "ws", standbyToken: "ws-tok-2")
        await coordinator.selectRouteMode(.direct)
        await pumpMainActor(60)

        // The adopt acquired the audio owner; rollback must release it.
        XCTAssertEqual(probe.adoptCount, 1)
        XCTAssertFalse(probe.audioDemandHeld, "rollback must release the owned audio (no ADM leak)")
        // The relay graph restarted as the audible owner.
        XCTAssertGreaterThan(primary.graph.startCount, 0, "relay graph must restart after rollback")
        coordinator.handleProviderReset()
    }

    /// The bidirectional gate requires NEW INBOUND AND OUTBOUND progress:
    /// inbound-only advancement (uplink never starts) rolls back bounded.
    func testBidirectionalGateRejectsInboundOnly() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)
        coordinator.directStandbyAudioGateTimeout = 0.3

        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 3, currentEpoch: 3, primary: "ice", standby: "ws", standbyToken: "ws-tok-2")
        // Only INBOUND advances during the promotion; outbound stays flat.
        let task = Task { @MainActor in
            await pumpMainActor(10)
            probe.advanceMediaForTest(inbound: 9, outbound: 0)
        }
        await coordinator.selectRouteMode(.direct)
        await task.value
        await pumpMainActor(60)

        // Inbound-only cannot satisfy the gate: rollback, demand released.
        XCTAssertFalse(probe.audioDemandHeld, "inbound-only promotion must roll back and release audio")
        XCTAssertEqual(probe.resumeAudioCount, 0)
        coordinator.handleProviderReset()
    }

    /// Ack loss on the promote command: the reconcile installs the SERVER'S
    /// CURRENT standby tokens (not a stale-epoch pairing) and completes the
    /// takeover from the reconciled roles.
    func testAckLossReconcileInstallsServerTokens() async throws {
        let api = makeAPI(standby: true)
        api.standbyPromoteError = APIError.notReady("ack lost")
        api.standbyStateResult = V2MediaStateResponse(
            primary: "ice", epoch: 5,
            standbyWs: true, standbyIce: false,
            standbyWsToken: "srv-ws-tok", standbyIceToken: nil)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)

        // Prove the standby fresh, then fail the primary: the takeover's
        // promote command loses its ack; reconcile recovers roles + tokens.
        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        let current = box.created.last!
        current.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await pumpMainActor(60)

        // The demoted relay's fence comes from the SERVER state (epoch 5),
        // never an old token paired with a new epoch.
        XCTAssertEqual(coordinator.debugHotStandbyFences[.relay]?.token, "srv-ws-tok")
        XCTAssertEqual(coordinator.debugHotStandbyFences[.relay]?.epoch, 5)
        // The takeover completed: the adopted standby proved bidirectional
        // media progress.
        probe.advanceMediaForTest(inbound: 10, outbound: 4)
        await pumpMainActor(60)
        coordinator.handleProviderReset()
    }

    /// Lifecycle fencing: a promote parked on an OLD call's controller,
    /// delivered AFTER the call's generation moved on (new call live), must
    /// leave the NEW call's controller, fences and watchdog untouched — and
    /// the old controller, once invalidated, permanently refuses promotes.
    func testStalePromoteAckCannotTouchNewCall() async throws {
        let api = makeAPI(standby: true)
        let probeA = FakeDirectProbe()
        let box = ScriptBox()
        let first = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [first], probe: probeA, box: box)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { first.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(30)
        guard let oldController = coordinator.debugHotStandbyController else {
            XCTFail("no hot standby controller")
            return
        }
        // Prove the direct standby so the old promote passes the fresh gate.
        probeA.fireRecoveryFreshMediaForTest()
        await pumpMainActor(20)

        // Park the old promote's HTTP ack mid-flight.
        api.standbyPromoteArmed = true
        let oldPromote = Task { await oldController.promoteStandby(.direct) }
        await pumpMainActor(20)
        XCTAssertEqual(api.standbyPromoteCalls.count, 1, "the old command did reach the wire")

        // The old call's lifecycle ends (generation bumps; new call begins).
        coordinator.handleProviderReset()
        let second = WSScript(ready: #"{"type":"ready","epoch":7}"#)
        box.created.removeAll()
        let relay2 = second
        // Fresh scripts for the new call: reuse the same coordinator with a
        // new primary script via a second pool entry is not available, so
        // start on a NEW coordinator sharing the API (same gateway).
        let probeB = FakeDirectProbe()
        let box2 = ScriptBox()
        let coordinator2 = makeCoordinator(api: api, scripts: [relay2], probe: probeB, box: box2)
        startOutgoing(coordinator2, api)
        await waitUntil(timeout: 3) { relay2.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(60)
        guard let newController = coordinator2.debugHotStandbyController else {
            XCTFail("no new hot standby controller")
            return
        }
        XCTAssertTrue(newController !== oldController)
        let newFences = coordinator2.debugHotStandbyFences

        // The stale ack lands NOW: the old controller must not transfer,
        // and the new call's state is untouched.
        api.resumeStandbyPromote()
        await oldPromote.value
        await pumpMainActor(40)

        XCTAssertEqual(coordinator2.debugHotStandbyFences, newFences, "stale ack mutated the new call's fences")
        XCTAssertFalse(coordinator2.debugRecoveryActive, "stale ack opened recovery on the new call")
        // Old controller: permanently invalidated, fences cleared, refuses.
        oldController.invalidate()
        XCTAssertTrue(oldController.invalidated)
        let refused = await oldController.promoteStandby(.relay)
        XCTAssertEqual(refused, .unavailable("invalidated"))
        XCTAssertEqual(oldController.fences.count, 0)
        coordinator2.handleProviderReset()
    }

    /// Forced (non-auto) mode: a media failure NEVER arms the standby
    /// takeover — the forced selection only recovers its selected route.
    func testForcedModeDoesNotArmStandbyTakeover() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        MediaRoutePreferenceStore.shared.setMode(.relay, for: gatewayID)
        await coordinator.selectRouteMode(.relay)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(60)

        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        primary.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await pumpMainActor(60)
        XCTAssertNil(coordinator.debugPendingStandbyTakeover, "forced mode must not take over the standby")
        XCTAssertEqual(api.standbyPromoteCalls.count, 0)
        coordinator.handleProviderReset()
    }

    // MARK: Real-cadence single-owner tests (build 50)

    /// Repeatedly invokes `fire` for `seconds` with yields between so the
    /// queued main-actor callback from each fire actually runs. Used to keep
    /// a standby fresh through many real timer rounds.
    private func keepFresh(seconds: TimeInterval, _ fire: () -> Void) async {
        let deadline = Date().addingTimeInterval(seconds)
        while Date() < deadline {
            fire()
            try? await Task.sleep(nanoseconds: 200_000_000)
        }
        await pumpMainActor(4)
    }

    /// REAL cadence, AUTO: after the direct standby attaches, BOTH warm
    /// transports survive many cadence rounds AND a watchdog tick with ZERO
    /// legacy automation — no startup primary commit, no failure-monitor
    /// handover, no server-reconcile handover, no measure socket, no rebuild
    /// and no promotion. Negative assertions are exact call counts.
    func testWarmTransportsSurviveCadenceRoundsWithoutLegacyAutomation() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box,
                                          cadence: timerCadence)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)

        // Keep the direct standby fresh across ~10 monitor rounds (0.2 s)
        // and one watchdog tick (5 s).
        await keepFresh(seconds: 5.4) { probe.fireRecoveryFreshMediaForTest() }

        XCTAssertFalse(primary.socket.cancelled, "primary relay socket must survive")
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)
        XCTAssertTrue(coordinator.debugHotStandbyStandbyFresh)
        XCTAssertEqual(coordinator.currentRouteState?.active, .relay)
        XCTAssertEqual(coordinator.currentRouteState?.pinned, true)
        XCTAssertEqual(api.primaryCommitCalls.count, 0, "no legacy startup primary commit")
        XCTAssertEqual(api.standbyCommitCalls.count, 1, "exactly one standby commit (no rebuilds)")
        XCTAssertEqual(api.attachProbeCalls.count, 1, "no legacy candidate probes or rebuilds")
        XCTAssertEqual(api.mediaWSRequestCallCount, 1, "no legacy staged-relay handover")
        XCTAssertEqual(api.measureRequestCallCount, 0, "no legacy measure socket")
        XCTAssertEqual(api.standbyPromoteCalls.count, 0, "no promotions")
        coordinator.handleProviderReset()
    }

    /// REAL cadence: relay→direct→relay→direct warm switches while timer
    /// loops keep firing throughout. The SAME direct peer is suspended and
    /// resumed (zero closeTransport), the demoted relay socket is never
    /// cancelled, and all routing goes through fenced standby promotes — no
    /// legacy primary commits, staging attaches or measure sockets.
    func testCadenceRepeatedSwitchesWithZeroCloses() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box,
                                          cadence: timerCadence)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)

        // Switch 1: relay -> DIRECT via the warm standby.
        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 3, currentEpoch: 3, primary: "ice", standby: "ws", standbyToken: "ws-tok-2")
        await advanceDuringPromotion(probe, inbound: 10, outbound: 4) {
            await coordinator.selectRouteMode(.direct)
        }
        await pumpMainActor(60)
        XCTAssertEqual(api.standbyPromoteCalls.count, 1)
        XCTAssertEqual(probe.closeCount, 0)
        XCTAssertFalse(primary.socket.cancelled, "demoted relay socket stays attached")
        XCTAssertEqual(coordinator.currentRouteState?.active, .direct)

        // Several cadence rounds on direct: keep the demoted relay standby
        // fresh with REAL downlink frames decoded by the standby session.
        await keepFresh(seconds: 1.2) {
            let pcm = PCMUCodec.encode([Int16](repeating: 5, count: 160))
            primary.socket.deliver(.success(.data(pcm)))
        }
        XCTAssertTrue(coordinator.debugHotStandbyStandbyFresh, "relay standby fresh via real frames")

        // Switch 2: direct -> RELAY.
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")
        await coordinator.selectRouteMode(.relay)
        await pumpMainActor(60)
        XCTAssertEqual(api.standbyPromoteCalls.count, 2)
        XCTAssertEqual(probe.suspendAudioCount, 1)
        XCTAssertEqual(probe.closeCount, 0)
        XCTAssertEqual(coordinator.currentRouteState?.active, .relay)

        // Switch 3: relay -> DIRECT: the demoted peer is RESUMED, never
        // re-adopted.
        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 5, currentEpoch: 5, primary: "ice", standby: "ws", standbyToken: "ws-tok-3")
        await advanceDuringPromotion(probe, inbound: 22, outbound: 9) {
            await coordinator.selectRouteMode(.direct)
        }
        await pumpMainActor(60)
        XCTAssertEqual(api.standbyPromoteCalls.count, 3)
        XCTAssertEqual(probe.resumeAudioCount, 1)
        XCTAssertEqual(probe.adoptCount, 1)
        XCTAssertEqual(probe.closeCount, 0, "zero closes across all swaps")
        XCTAssertEqual(api.primaryCommitCalls.count, 0)
        XCTAssertEqual(api.standbyCommitCalls.count, 1)
        XCTAssertEqual(api.attachProbeCalls.count, 1)
        XCTAssertEqual(api.mediaWSRequestCallCount, 1)
        coordinator.handleProviderReset()
    }

    /// REAL cadence after a warm switch to FORCED DIRECT: the relay standby
    /// is kept FRESH (it would qualify for a quality-based switch), yet the
    /// forced mode cannot be silently changed through cadence rounds and a
    /// watchdog tick.
    func testForcedModeSurvivesCadenceWithoutSilentSwitch() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box,
                                          cadence: timerCadence)
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)

        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 3, currentEpoch: 3, primary: "ice", standby: "ws", standbyToken: "ws-tok-2")
        await advanceDuringPromotion(probe, inbound: 10, outbound: 4) {
            await coordinator.selectRouteMode(.direct)
        }
        await pumpMainActor(60)
        XCTAssertEqual(api.standbyPromoteCalls.count, 1)
        XCTAssertEqual(coordinator.currentRouteState?.mode, .direct)

        // Keep the relay standby FRESH across cadence rounds and a watchdog.
        await keepFresh(seconds: 5.4) {
            let pcm = PCMUCodec.encode([Int16](repeating: 7, count: 160))
            primary.socket.deliver(.success(.data(pcm)))
        }

        XCTAssertEqual(coordinator.currentRouteState?.mode, .direct, "forced mode was silently changed")
        XCTAssertEqual(coordinator.currentRouteState?.active, .direct)
        XCTAssertEqual(api.standbyPromoteCalls.count, 1, "no automatic cross-route promote")
        XCTAssertEqual(probe.closeCount, 0)
        XCTAssertFalse(coordinator.debugRecoveryActive)
        coordinator.handleProviderReset()
    }

    /// REAL cadence: a primary failure with a FRESH warm standby completes
    /// the fenced takeover INSIDE the 45 s episode (measured elapsed),
    /// through active timer rounds, with no hangup.
    func testPrimaryFailureRecoversWithinEpisodeWithRealCadence() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box,
                                          cadence: timerCadence)
        // Armed BEFORE the call starts: with the real cadence a promotion can
        // legitimately fire before any later setup line runs, so the park must
        // not depend on timing.
        api.standbyPromoteParkOnCall = 1
        let uuid = startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)

        probe.fireRecoveryFreshMediaForTest()
        await pumpMainActor(30)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 3, currentEpoch: 3, primary: "ice", standby: "ws", standbyToken: "ws-tok-2")
        let startedAt = Date()
        await advanceDuringPromotion(probe, inbound: 10, outbound: 4) {
            // 1. The primary failure lands FIRST, while the warm standby's
            //    fresh-media evidence is still inside its window (it was fired
            //    immediately before this block).
            primary.socket.deliver(.failure(URLError(.networkConnectionLost)))
            // 2. The takeover's promote command is PARKED (armed before the
            //    call started), so wait until it is observably issued and then
            //    release the ack.
            await waitUntil(timeout: 5) { api.standbyPromoteCalls.count == 1 }
            api.resumeStandbyPromote()
            // 3. This block only returns once the takeover has FULLY completed
            //    (direct primary, no recovery episode, nothing pending), so the
            //    feeder keeps proving fresh bidirectional media through the
            //    proof and the post-ack work.
            await waitUntil(timeout: 8) {
                coordinator.currentRouteState?.active == .direct
                    && !coordinator.debugRecoveryActive
                    && coordinator.debugPendingStandbyTakeover == nil
            }
        }
        let elapsed = Date().timeIntervalSince(startedAt)
        XCTAssertLessThan(elapsed, 10, "takeover must finish well within the 45 s episode")
        XCTAssertEqual(api.standbyPromoteCalls.count, 1)
        XCTAssertFalse(api.hangups.contains(uuid.uuidString.lowercased()), "no hangup after takeover")
        XCTAssertFalse(coordinator.debugRecoveryActive)
        coordinator.handleProviderReset()
    }

    /// REAL cadence BOTH-DOWN path: at primary failure the standby is NOT
    /// fresh → no warm takeover; the current-route relay rebuild fails fast;
    /// the alternate direct recovery (after the 1 s backoff) commits the
    /// recovering direct peer and its first fresh-media proof closes the
    /// episode — measured within the 45 s window.
    func testBothDownRecoversWithin45Seconds() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let failedStage = WSScript(failed: true)
        let coordinator = makeCoordinator(api: api, scripts: [primary, failedStage], probe: probe,
                                          box: box, cadence: timerCadence)
        let uuid = startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)
        // Standby deliberately left UNPROVEN at failure.

        let startedAt = Date()
        primary.socket.deliver(.failure(URLError(.networkConnectionLost)))
        await pumpMainActor(40)
        XCTAssertTrue(coordinator.debugRecoveryActive, "both-down opens the recovery episode")
        // No warm takeover was armed.
        XCTAssertNil(coordinator.debugPendingStandbyTakeover)

        // After the 1 s backoff the alternate direct attempt commits the
        // shared probe; its first real fresh-media proof closes the episode.
        try await Task.sleep(nanoseconds: 1_400_000_000)
        await pumpMainActor(20)
        probe.fireRecoveryFreshMediaForTest()
        await waitUntil(timeout: 3) { !coordinator.debugRecoveryActive }
        let elapsed = Date().timeIntervalSince(startedAt)
        XCTAssertLessThan(elapsed, 45, "recovery must finish inside the 45 s episode")
        XCTAssertEqual(api.primaryCommitCalls.count, 1, "one primary direct recovery commit")
        XCTAssertFalse(api.hangups.contains(uuid.uuidString.lowercased()))
        coordinator.handleProviderReset()
    }

    // MARK: Initial forced selection + direct-first gate (owner handoff)

    private final class FakePreflight: CallRoutePreflightProviding {
        private let handoffValue: RoutePreflightController.Handoff?
        init(_ handoff: RoutePreflightController.Handoff?) { handoffValue = handoff }
        func handoffForCall() -> RoutePreflightController.Handoff? { handoffValue }
    }

    /// Models the foreground idle relay probe so an AUTO direct-first
    /// fastpath has FRESH relay evidence (direct 50 ms materially beats
    /// relay 100 ms).
    private final class FakeIdleRelay: CallRelayIdleProbeProviding {
        private(set) var stopped = false
        func stop() { stopped = true }
        func appDidEnterForeground() {}
        func freshTimestampedSamples(within window: TimeInterval, now: Date)
            -> [(rtt: TimeInterval, at: Date)] {
            Array(repeating: (0.10, now), count: 6)
        }
    }

    private func makeHandoff(_ probe: FakeDirectProbe) -> RoutePreflightController.Handoff {
        .init(probe: probe, preflightId: "prb_warm", attachedAt: Date(),
              expiresAt: Date().addingTimeInterval(30),
              samples: Array(repeating: 0.05, count: 4), pathGeneration: 0)
    }

    /// Persisted FORCED DIRECT on a relay-first call: the direct standby is
    /// brought up by the single owner and PROMOTED at startup once fresh
    /// (the suppressed legacy selection must not pin relay forever); the
    /// demoted relay stays warm. Bounded measured.
    func testInitialPersistedDirectPromotesDirectStandby() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 3, currentEpoch: 3, primary: "ice", standby: "ws", standbyToken: "ws-tok-2")
        let startedAt = Date()
        let uuid = startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled)

        probe.fireRecoveryFreshMediaForTest()
        // The feeder must outlive the WHOLE promotion, so the completion is
        // observed INSIDE the block (before the feeder is stopped): a
        // confirmed direct primary, the demoted relay kept warm and no
        // takeover left pending.
        await advanceDuringPromotion(probe, inbound: 10, outbound: 4) {
            await waitUntil(timeout: 8) {
                coordinator.currentRouteState?.active == .direct
                    && coordinator.debugStandbyRelayInstalled
                    && coordinator.debugPendingStandbyTakeover == nil
            }
        }
        let elapsed = Date().timeIntervalSince(startedAt)
        XCTAssertLessThan(elapsed, 15)
        XCTAssertEqual(coordinator.currentRouteState?.mode, .direct)
        XCTAssertEqual(api.standbyPromoteCalls.count, 1)
        XCTAssertEqual(api.standbyPromoteCalls.first?.transport, "ice")
        XCTAssertTrue(coordinator.debugStandbyRelayInstalled, "demoted relay must stay warm")
        XCTAssertEqual(probe.closeCount, 0)
        XCTAssertEqual(api.primaryCommitCalls.count, 0, "no legacy primary commit")
        XCTAssertEqual(api.standbyCommitCalls.count, 1)
        XCTAssertEqual(api.attachProbeCalls.count, 1)
        XCTAssertFalse(api.hangups.contains(uuid.uuidString.lowercased()))
        coordinator.handleProviderReset()
    }

    /// Persisted FORCED DIRECT whose standby never proves fresh within the
    /// bounded startup window: the audible relay keeps the call, zero
    /// promote calls, and the failure is reported truthfully (notice).
    func testInitialPersistedDirectUnreadyKeepsRelayTruthfully() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        coordinator.initialSelectionHonorWindow = 0.6
        var notices = 0
        coordinator.onRouteNotice = { _, _ in notices += 1 }
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)
        // Never fire freshness.
        try await Task.sleep(nanoseconds: 1_000_000_000)
        await pumpMainActor(20)

        XCTAssertEqual(coordinator.currentRouteState?.active, .relay)
        XCTAssertEqual(coordinator.currentRouteState?.mode, .direct, "mode persisted, not flipped")
        XCTAssertEqual(api.standbyPromoteCalls.count, 0)
        XCTAssertEqual(notices, 1, "truthful notice delivered")
        XCTAssertFalse(coordinator.debugRecoveryActive)
        coordinator.handleProviderReset()
    }

    /// DIRECT-FIRST healthy startup: the warm preflight is adopted, the
    /// bounded owner gate passes on bidirectional media, no recovery
    /// episode, relay standby warm. Negative assertions on legacy paths.
    func testDirectFirstHealthyGateNoRecovery() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(90)

        XCTAssertEqual(coordinator.currentRouteState?.active, .direct)
        XCTAssertTrue(coordinator.debugStandbyRelayInstalled)
        XCTAssertFalse(coordinator.debugRecoveryActive)
        XCTAssertEqual(api.standbyPromoteCalls.count, 0)
        XCTAssertEqual(api.primaryCommitCalls.count, 1, "warm primary preflight commit")
        XCTAssertEqual(api.standbyCommitCalls.count, 0)
        XCTAssertEqual(api.attachProbeCalls.count, 0, "seed reused, no second probe attach")
        XCTAssertEqual(api.measureRequestCallCount, 0)
        coordinator.handleProviderReset()
    }

    /// DIRECT-FIRST with the ONE bounded ADM restart healing media: no
    /// recovery episode opens (restart budget proven, no false recovery).
    func testDirectFirstRestartHealsWithoutRecovery() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        probe.audioFlowingValue = false
        probe.restartCapable = true
        probe.restartHealsAudio = true
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        coordinator.ownedDirectFirstGateWindow = 0.5
        startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(120)

        XCTAssertEqual(probe.restartAudioCount, 1, "exactly one bounded restart")
        XCTAssertEqual(coordinator.currentRouteState?.active, .direct)
        XCTAssertFalse(coordinator.debugRecoveryActive)
        XCTAssertEqual(api.standbyPromoteCalls.count, 0)
        coordinator.handleProviderReset()
    }

    /// AUTO direct-first, warm ICE-connected but SILENT capture: the owner
    /// gate fails (even after the restart budget) and enters the SINGLE 45s
    /// episode; with the relay standby FRESH, the fenced relay promotion
    /// completes the call within the episode, direct demoted retained, no
    /// hangup.
    func testDirectFirstSilentCaptureRecoversViaRelay() async throws {
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        probe.audioFlowingValue = false // silent capture
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        coordinator.idleRelayProbe = FakeIdleRelay()
        // Gate outlasts the relay standby install so the takeover is armed.
        coordinator.ownedDirectFirstGateWindow = 3.0
        let uuid = startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyRelayInstalled }

        // Prove the relay standby FRESH well before the gate expires.
        let pcm = PCMUCodec.encode([Int16](repeating: 9, count: 160))
        relayStandby.socket.deliver(.success(.data(pcm)))
        await pumpMainActor(20)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")
        let startedAt = Date()
        await waitUntil(timeout: 6) { coordinator.currentRouteState?.active == .relay }
        let elapsed = Date().timeIntervalSince(startedAt)
        XCTAssertLessThan(elapsed, 15, "recovery well within the 45s episode")
        XCTAssertEqual(api.standbyPromoteCalls.count, 1)
        XCTAssertEqual(api.standbyPromoteCalls.first?.transport, "ws")
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled, "direct demoted retained, not closed")
        XCTAssertEqual(probe.closeCount, 0)
        XCTAssertFalse(api.hangups.contains(uuid.uuidString.lowercased()))
        coordinator.handleProviderReset()
    }

    /// FORCED DIRECT direct-first silent capture: mode semantics — the 45s
    /// episode rebuilds the selected DIRECT only; the relay standby is never
    /// automatically promoted (explicit fallback stays a user action).
    func testDirectFirstSilentForcedDirectDoesNotPromoteRelay() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        probe.audioFlowingValue = false
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        coordinator.ownedDirectFirstGateWindow = 1.0
        startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyRelayInstalled }
        // Relay fresh, but mode forced direct: still no relay promotion.
        let pcm = PCMUCodec.encode([Int16](repeating: 8, count: 160))
        relayStandby.socket.deliver(.success(.data(pcm)))
        // Wait for the 1s gate to expire and open the episode.
        await waitUntil(timeout: 3) { coordinator.debugRecoveryActive }

        XCTAssertTrue(coordinator.debugRecoveryActive, "episode rebuilds the selected direct")
        XCTAssertEqual(api.standbyPromoteCalls.count, 0, "no automatic relay fallback under forced direct")
        coordinator.handleProviderReset()
    }

    // MARK: Fence regression (owner review final pass)

    /// Rapid mode change DURING the pending initial forced honor: the stale
    /// honor cancels immediately — zero promote calls, zero failure notices,
    /// no recovery; the audible relay simply stays.
    func testRapidModeChangeCancelsPendingInitialHonor() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        let box = ScriptBox()
        let primary = WSScript(ready: #"{"type":"ready","epoch":2}"#)
        let coordinator = makeCoordinator(api: api, scripts: [primary], probe: probe, box: box)
        coordinator.initialSelectionHonorWindow = 2.0
        var notices = 0
        coordinator.onRouteNotice = { _, _ in notices += 1 }
        startOutgoing(coordinator, api)
        await waitUntil(timeout: 3) { primary.socket.resumed }
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyDirectInstalled }
        // Never prove direct fresh; move away while the honor is waiting.
        try await Task.sleep(nanoseconds: 300_000_000)
        await coordinator.selectRouteMode(.relay)
        // Wait past the original honor window.
        try await Task.sleep(nanoseconds: 2_200_000_000)
        await pumpMainActor(20)

        XCTAssertEqual(coordinator.currentRouteState?.active, .relay)
        XCTAssertEqual(api.standbyPromoteCalls.count, 0, "stale honor must not promote")
        XCTAssertEqual(notices, 0, "no failure notice after a deliberate mode change")
        XCTAssertFalse(coordinator.debugRecoveryActive)
        coordinator.handleProviderReset()
    }

    /// User switches to relay DURING the direct-first startup gate: once the
    /// probe is demoted the stale gate becomes inert — it never restarts the
    /// demoted ADM nor dispatches recovery for the wrong primary; the warm
    /// relay promotion settles the call.
    func testDirectFirstGateInertAfterDemotion() async throws {
        MediaRoutePreferenceStore.shared.setMode(.auto, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        probe.audioFlowingValue = false // gate would otherwise pass
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        coordinator.idleRelayProbe = FakeIdleRelay()
        // Gate outlasts the relay standby install + manual switch.
        coordinator.ownedDirectFirstGateWindow = 4.0
        startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyRelayInstalled }

        // Ready the relay, then demote the direct while the gate is pending.
        let pcm = PCMUCodec.encode([Int16](repeating: 6, count: 160))
        relayStandby.socket.deliver(.success(.data(pcm)))
        await pumpMainActor(20)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")
        await coordinator.selectRouteMode(.relay)
        await waitUntil(timeout: 3) { coordinator.currentRouteState?.active == .relay }
        // Let the original gate window fully expire; if fenced it stays inert.
        try await Task.sleep(nanoseconds: 3_200_000_000)
        await pumpMainActor(20)

        XCTAssertEqual(api.standbyPromoteCalls.count, 1, "only the user's warm relay promotion")
        XCTAssertEqual(probe.restartAudioCount, 0, "stale gate must not restart the demoted ADM")
        XCTAssertFalse(coordinator.debugRecoveryActive, "no recovery for a no-longer-primary")
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled, "demoted direct retained warm")
        coordinator.handleProviderReset()
    }

    // MARK: Build-51 direct→relay ownership repair

    /// Direct-primary call, user switches to relay (build-51 field failure):
    /// the retiring direct ADM's teardown keeps the input unusable for a
    /// window. The promotion must WAIT for input readiness and start the
    /// relay graph EXACTLY ONCE after it — one promote command, one ADM
    /// suspend, no code=1 start attempt at all.
    func testRelayPromotionWaitsForInputReadinessBeforeStartingGraph() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        relayStandby.graph.inputNotReadyProbes = 5
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        let uuid = startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyRelayInstalled }

        // Prove the relay standby fresh, then switch.
        let pcm = PCMUCodec.encode([Int16](repeating: 7, count: 160))
        relayStandby.socket.deliver(.success(.data(pcm)))
        await pumpMainActor(20)
        api.standbyPromoteResult = V2PromoteStandbyResponse(
            epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")
        await coordinator.selectRouteMode(.relay)

        XCTAssertEqual(coordinator.currentRouteState?.active, .relay)
        XCTAssertEqual(api.standbyPromoteCalls.count, 1, "exactly one promote command")
        XCTAssertEqual(api.standbyPromoteCalls.first?.transport, "ws")
        // The graph started only AFTER the readiness probes turned positive.
        XCTAssertGreaterThanOrEqual(relayStandby.graph.inputReadyProbeCount, 6,
                                    "the start must wait out the ADM-teardown window")
        XCTAssertEqual(relayStandby.graph.startCount, 1, "a single graph start, no code=1 retry")
        // Ordered single-owner handover: suspend once, never resumed.
        XCTAssertEqual(probe.suspendAudioCount, 1)
        XCTAssertEqual(probe.resumeAudioCount, 0)
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled, "demoted direct retained warm")
        XCTAssertEqual(probe.closeCount, 0)
        XCTAssertFalse(api.hangups.contains(uuid.uuidString.lowercased()))
        coordinator.handleProviderReset()
    }

    /// Direct→relay promotion whose relay graph start keeps failing
    /// (build-51 field scenario): ONE bounded attempt — the live ADM is
    /// suspended and resumed exactly once, the rolled-back attempt never
    /// tight-retries across the old 300 ms loop horizon, the CLOSED relay
    /// session is never re-promoted, and the SERVER'S primary is restored
    /// to the direct path (swap-back with the acked standby fence, VALIDATED
    /// against the ack) so the call stays consistent both directions.
    func testFailedRelayPromotionRollsBackOnceRestoresServerPrimaryWithoutThrash() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        relayStandby.graph.startResult = false // relay graph can never start
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        coordinator.promotionInputReadinessTimeout = 0.2
        let uuid = startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyRelayInstalled }

        let pcm = PCMUCodec.encode([Int16](repeating: 5, count: 160))
        relayStandby.socket.deliver(.success(.data(pcm)))
        await pumpMainActor(20)
        // Per-call scripting: the promotion ack (relay becomes primary),
        // then the swap-back ack VALIDATED as direct-primary with the
        // demoted relay's fresh standby token.
        api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")),
            .success(V2PromoteStandbyResponse(
                epoch: 5, currentEpoch: 5, primary: "ice", standby: "ws", standbyToken: "ws-tok-rb")),
        ]
        await coordinator.selectRouteMode(.relay)

        // One promotion attempt: the fenced command, then ONE rollback with
        // the server-side primary restored (the swap-back promote).
        XCTAssertEqual(api.standbyPromoteCalls.count, 2, "promotion + one server swap-back")
        XCTAssertEqual(api.standbyPromoteCalls.first?.transport, "ws")
        XCTAssertEqual(api.standbyPromoteCalls.first?.token, "relay-tok")
        XCTAssertEqual(api.standbyPromoteCalls.last?.transport, "ice",
                       "the rollback restores the direct primary server-side")
        XCTAssertEqual(api.standbyPromoteCalls.last?.token, "ice-tok-2")
        XCTAssertEqual(api.standbyPromoteCalls.last?.expectEpoch, 4)
        // The restore is PROVEN, not assumed: the now-primary direct fence
        // is gone. The demoted relay session was CLOSED by the rollback, so
        // its fence stays NIL — a fresh manual selection rebuilds inline
        // instead of waiting for the watchdog.
        XCTAssertNil(coordinator.debugHotStandbyFences[.direct],
                     "a standby fence for the carrier must never survive")
        XCTAssertNil(coordinator.debugHotStandbyFences[.relay],
                     "the closed relay session must not carry a dead fence")
        XCTAssertFalse(coordinator.debugRecoveryActive, "confirmed restore: no recovery")
        // Single-owner handover: one suspend, one resume. The relay session
        // was closed and must never be re-promoted.
        XCTAssertEqual(probe.suspendAudioCount, 1, "no retry thrash: the ADM suspended once")
        XCTAssertEqual(probe.resumeAudioCount, 1)
        XCTAssertFalse(coordinator.debugStandbyRelayInstalled, "closed relay session dropped")
        // Truthful end state: direct still carries, mode stays relay, call alive.
        XCTAssertEqual(coordinator.currentRouteState?.active, .direct)
        XCTAssertEqual(coordinator.debugRouteMode, .relay)
        XCTAssertFalse(api.hangups.contains(uuid.uuidString.lowercased()))

        // The OLD code re-attempted every 300 ms: observe well past that
        // horizon — nothing new may fire.
        try await Task.sleep(nanoseconds: 1_700_000_000)
        await pumpMainActor(30)
        XCTAssertEqual(api.standbyPromoteCalls.count, 2, "no automatic tight retry after rollback")
        XCTAssertEqual(probe.suspendAudioCount, 1, "no further ADM suspend/resume churn")
        XCTAssertEqual(probe.resumeAudioCount, 1)
        XCTAssertFalse(coordinator.debugRecoveryActive)
        coordinator.handleProviderReset()
    }

    /// The swap-back is REJECTED but the reconcile against the server's
    /// ACTUAL roles confirms the direct path is primary (the swap landed
    /// while the ack died): the restore is accepted, the server's CURRENT
    /// standby tokens are installed, NO recovery opens.
    func testFailedRelayPromotionSwapBackAckLostReconcileConfirmsRestore() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        relayStandby.graph.startResult = false
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        coordinator.promotionInputReadinessTimeout = 0.2
        let uuid = startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyRelayInstalled }

        let pcm = PCMUCodec.encode([Int16](repeating: 5, count: 160))
        relayStandby.socket.deliver(.success(.data(pcm)))
        await pumpMainActor(20)
        api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")),
            .failure(APIError.notReady("swap-back ack lost")),
        ]
        // The reconcile sees the server's ACTUAL roles: direct primary
        // again, the relay as the standby with a fresh token.
        api.standbyStateResult = V2MediaStateResponse(
            primary: "ice", epoch: 6,
            standbyWs: true, standbyIce: false,
            standbyWsToken: "ws-tok-reconciled", standbyIceToken: nil)
        await coordinator.selectRouteMode(.relay)

        XCTAssertEqual(api.standbyPromoteCalls.count, 2)
        XCTAssertGreaterThanOrEqual(api.mediaStateCalls, 1, "lost ack must reconcile")
        XCTAssertFalse(coordinator.debugRecoveryActive, "reconciled restore: no recovery")
        XCTAssertNil(coordinator.debugHotStandbyFences[.direct])
        // The reconciled ws standby token belongs to the just-CLOSED relay
        // session: no local transport carries it, so no fence is installed.
        XCTAssertNil(coordinator.debugHotStandbyFences[.relay],
                     "a fence requires a locally surviving transport")
        XCTAssertEqual(probe.suspendAudioCount, 1)
        XCTAssertEqual(probe.resumeAudioCount, 1)
        XCTAssertEqual(coordinator.currentRouteState?.active, .direct)
        XCTAssertFalse(api.hangups.contains(uuid.uuidString.lowercased()))
        coordinator.handleProviderReset()
    }

    /// Confirmed rollback, then a FRESH explicit user selection: the
    /// closed-WS fence stayed nil, so the owner REBUILDS the relay standby
    /// INLINE (no watchdog wait) and the second promotion succeeds — the
    /// demoted direct peer is suspended a second time, never closed.
    func testFailedRelayPromotionManualRetryAfterConfirmedRollbackSucceedsInline() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        relayStandby.graph.startResult = false
        // Pool carries the REBUILD standby for the second attempt.
        let rebuiltStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok-2","epoch":5}"#)
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby, rebuiltStandby],
                                          probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        coordinator.promotionInputReadinessTimeout = 0.2
        let uuid = startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyRelayInstalled }

        let pcm = PCMUCodec.encode([Int16](repeating: 5, count: 160))
        relayStandby.socket.deliver(.success(.data(pcm)))
        await pumpMainActor(20)
        api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")),
            .success(V2PromoteStandbyResponse(
                epoch: 5, currentEpoch: 5, primary: "ice", standby: "ws", standbyToken: "ws-tok-rb")),
            .success(V2PromoteStandbyResponse(
                epoch: 6, currentEpoch: 6, primary: "ws", standby: "ice", standbyToken: "ice-tok-3")),
        ]
        // First attempt fails and rolls back (server restored).
        await coordinator.selectRouteMode(.relay)
        XCTAssertEqual(coordinator.currentRouteState?.active, .direct)
        XCTAssertEqual(api.standbyPromoteCalls.count, 2)
        XCTAssertNil(coordinator.debugHotStandbyFences[.relay])

        // Fresh explicit intent: the owner rebuilds the relay standby
        // inline (fence nil → no watchdog wait) and promotes it. Wait for
        // the REBUILT fence: it is installed only after the attach and the
        // freshness hook are live (the receive loop is parked by then).
        let secondSwitch = Task { @MainActor in
            await coordinator.selectRouteMode(.relay)
        }
        await waitUntil(timeout: 5) {
            coordinator.debugHotStandbyFences[.relay]?.token == "relay-tok-2"
        }
        rebuiltStandby.socket.deliver(.success(.data(pcm)))
        try await Task.sleep(nanoseconds: 400_000_000)
        rebuiltStandby.socket.deliver(.success(.data(pcm)))
        await waitUntil(timeout: 8) { coordinator.currentRouteState?.active == .relay }
        await secondSwitch.value

        XCTAssertEqual(api.standbyPromoteCalls.count, 3, "two promotions + one swap-back")
        XCTAssertEqual(api.standbyPromoteCalls.last?.transport, "ws")
        XCTAssertEqual(api.standbyPromoteCalls.last?.token, "relay-tok-2")
        XCTAssertEqual(coordinator.currentRouteState?.active, .relay)
        XCTAssertEqual(coordinator.debugRouteMode, .relay)
        XCTAssertEqual(probe.suspendAudioCount, 2, "second handover suspends the demoted ADM")
        XCTAssertEqual(probe.resumeAudioCount, 1, "the first rollback resumed exactly once")
        XCTAssertTrue(coordinator.debugStandbyDirectInstalled, "direct demoted, never closed")
        XCTAssertEqual(probe.closeCount, 0)
        XCTAssertEqual(coordinator.debugHotStandbyFences[.direct]?.token, "ice-tok-3")
        XCTAssertFalse(coordinator.debugRecoveryActive)
        XCTAssertFalse(api.hangups.contains(uuid.uuidString.lowercased()))
        coordinator.handleProviderReset()
    }

    /// The swap-back is rejected AND the reconcile shows the server still
    /// holds the relay as primary: the restore is NOT confirmed — the call
    /// must enter the EXISTING bounded 45 s recovery (no false "restored",
    /// no redial) instead of silently splitting ownership.
    func testFailedRelayPromotionUnconfirmedRestoreEntersBoundedRecovery() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        relayStandby.graph.startResult = false
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        coordinator.promotionInputReadinessTimeout = 0.2
        let uuid = startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyRelayInstalled }

        let pcm = PCMUCodec.encode([Int16](repeating: 5, count: 160))
        relayStandby.socket.deliver(.success(.data(pcm)))
        await pumpMainActor(20)
        // Observable fence: the STARTUP direct promotion must have settled
        // before the manual relay switch. Without it the manual command races
        // an in-flight take-over and the intended promote/rollback/reconcile
        // sequence is never reached (fixture ordering, not a product defect).
        await waitUntil(timeout: 10) {
            coordinator.currentRouteState?.active == .direct
                && coordinator.debugPendingStandbyTakeover == nil
        }
        api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")),
            .failure(APIError.notReady("swap-back rejected")),
        ]
        // Server truth: the relay is STILL the primary (restore unconfirmed).
        api.standbyStateResult = V2MediaStateResponse(
            primary: "ws", epoch: 4,
            standbyWs: false, standbyIce: true,
            standbyWsToken: nil, standbyIceToken: "ice-tok-2")
        await coordinator.selectRouteMode(.relay)

        XCTAssertEqual(api.standbyPromoteCalls.count, 2)
        XCTAssertGreaterThanOrEqual(api.mediaStateCalls, 1, "rejected restore must reconcile")
        XCTAssertTrue(coordinator.debugRecoveryActive,
                      "unconfirmed restore must enter the bounded 45s recovery")
        XCTAssertEqual(coordinator.currentRouteState?.active, .direct)
        XCTAssertFalse(api.hangups.contains(uuid.uuidString.lowercased()),
                       "recovery is bounded; no immediate hangup, no redial")
        coordinator.handleProviderReset()
    }

    /// A stale restore (the call ended while the swap-back was in flight)
    /// must touch NOTHING: no reconcile, no recovery, no further promotes.
    func testFailedRelayPromotionSupersededRestoreIsInert() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        relayStandby.graph.startResult = false
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        coordinator.promotionInputReadinessTimeout = 0.2
        startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyRelayInstalled }

        let pcm = PCMUCodec.encode([Int16](repeating: 5, count: 160))
        relayStandby.socket.deliver(.success(.data(pcm)))
        await pumpMainActor(20)
        api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")),
        ]
        // The swap-back (promote call #2) parks in flight.
        api.standbyPromoteParkOnCall = 2
        let switchTask = Task { @MainActor in
            await coordinator.selectRouteMode(.relay)
        }
        await waitUntil(timeout: 5) { api.standbyPromoteCalls.count == 2 }
        // End the call while the swap-back is parked: the generation the
        // restore was armed with dies with it.
        coordinator.handleProviderReset()
        api.resumeStandbyPromote()
        await switchTask.value
        await pumpMainActor(30)

        XCTAssertEqual(api.standbyPromoteCalls.count, 2)
        XCTAssertEqual(api.mediaStateCalls, 0, "a stale restore must not reconcile")
        XCTAssertFalse(coordinator.debugRecoveryActive, "no recovery from a dead lifecycle")
        coordinator.handleProviderReset()
    }

    /// The call ends while the restore's RECONCILE is in flight: the
    /// superseded restore schedules NOTHING — no second reconcile, no
    /// recovery, no logs-driven action against the dead lifecycle.
    func testFailedRelayPromotionSupersededDuringReconcileIsInert() async throws {
        MediaRoutePreferenceStore.shared.setMode(.direct, for: gatewayID)
        let api = makeAPI(standby: true)
        let probe = FakeDirectProbe()
        probe.samplesToReturn = Array(repeating: 0.05, count: 4)
        let box = ScriptBox()
        let relayStandby = WSScript(ready:
            #"{"type":"ready","standby":true,"token":"relay-tok","epoch":3}"#)
        relayStandby.graph.startResult = false
        let coordinator = makeCoordinator(api: api, scripts: [relayStandby], probe: probe, box: box)
        coordinator.routePreflight = FakePreflight(makeHandoff(probe))
        coordinator.promotionInputReadinessTimeout = 0.2
        startOutgoing(coordinator, api)
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await waitUntil(timeout: 3) { coordinator.debugStandbyRelayInstalled }

        let pcm = PCMUCodec.encode([Int16](repeating: 5, count: 160))
        relayStandby.socket.deliver(.success(.data(pcm)))
        await pumpMainActor(20)
        // Same observable quiescence fence: the startup promotion must be
        // settled before the manual relay switch so the reconcile actually
        // starts (and can then park on `standbyStateArmed`).
        await waitUntil(timeout: 10) {
            coordinator.currentRouteState?.active == .direct
                && coordinator.debugPendingStandbyTakeover == nil
        }
        api.standbyPromoteScript = [
            .success(V2PromoteStandbyResponse(
                epoch: 4, currentEpoch: 4, primary: "ws", standby: "ice", standbyToken: "ice-tok-2")),
            .failure(APIError.notReady("swap-back rejected")),
        ]
        api.standbyStateArmed = true // the reconcile parks in flight
        let switchTask = Task { @MainActor in
            await coordinator.selectRouteMode(.relay)
        }
        await waitUntil(timeout: 5) { api.mediaStateCalls == 1 }
        // End the call mid-reconcile: the generation the restore was armed
        // with dies with it.
        coordinator.handleProviderReset()
        api.resumeStandbyState()
        await switchTask.value
        await pumpMainActor(30)

        XCTAssertEqual(api.mediaStateCalls, 1, "a superseded restore must not reconcile again")
        XCTAssertEqual(api.standbyPromoteCalls.count, 2)
        XCTAssertFalse(coordinator.debugRecoveryActive,
                       "no recovery may be scheduled for a dead lifecycle")
        coordinator.handleProviderReset()
    }
}
