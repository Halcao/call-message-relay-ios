import XCTest
import AVFoundation
@testable import CallRelay

/// Build-49 conference recovery coverage (the second review required these as
/// dedicated tests, not optional extras): the conference path is serialized
/// through the same MediaRecoveryController (independent 45 s deadline,
/// awaited attempts, captured episode+attempt identity, single audio owner
/// via staged attach + exclusive promotion).
@MainActor
final class ConferenceRecoveryTests: XCTestCase {
    private let gatewayID = "gw-conference-recovery"
    private let mediaURL = URLRequest(url: URL(string: "wss://example.test/media")!)

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

    /// Mutable factory state (escaping closures cannot capture inout).
    @MainActor
    private final class Feed {
        var scripts: [WSScript]
        var created: [WebSocketCallMedia] = []
        var sockets: [FakeMediaSocket] = []
        init(scripts: [WSScript]) { self.scripts = scripts }
        /// Queue a script AFTER startup (the number of startup creations is
        /// not part of the test contract; the recovery candidate must be
        /// queued exactly when the recovery attempt is expected).
        func append(_ script: WSScript) { scripts.append(script) }
        /// Discard un-consumed startup scripts so the NEXT creation is
        /// exactly the one the test just queued.
        func clearScripts() { scripts.removeAll() }
        func makeMedia() -> WebSocketCallMedia {
            let script = scripts.isEmpty ? WSScript() : scripts.removeFirst()
            let media = WebSocketCallMedia(
                socketFactory: { _, _ in script.socket }, audioGraph: script.graph)
            created.append(media)
            sockets.append(script.socket)
            return media
        }
    }

    private func makeAPI() -> FakeGatewayAPI {
        let api = FakeGatewayAPI()
        api.iceConfigOverride = ICEConfiguration(
            policy: "all", iceServers: [],
            expiresAt: "2026-10-01T00:00:00Z", mediaTransports: ["ws"])
        api.mediaWSRequestOverride = mediaURL
        api.conferenceMediaWSRequestOverride = mediaURL
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

    /// Two answered calls merged into a hosted WSS conference. `created` is
    /// appended by the wsMediaFactory in creation order; the THIRD created
    /// media (index 2) is the conference host socket.
    private func startWSSConference(
        _ coordinator: CallCoordinator, _ api: FakeGatewayAPI, _ feed: Feed
    ) async -> WebSocketCallMedia {
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
        XCTAssertEqual(api.answers.count, 2, "both gateway answers must complete")
        AudioSessionBridge.shared.didActivate(AVAudioSession.sharedInstance())
        await pumpMainActor(30)
        let legB = makeCallRecord(id: "call-B", state: .active, direction: .inbound, peer: "1002")
        let legA = makeCallRecord(id: "call-A", state: .active, direction: .inbound, peer: "1001")
        api.mergeResult = makeConferenceRecord(id: "conf-1", legs: [legB, legA])
        api.conferenceSnapshot = makeConferenceRecord(id: "conf-1", legs: [legB, legA])
        coordinator.mergeHeldCalls()
        await waitUntil(timeout: 5) { coordinator.conferenceRecord != nil }
        // Identify the host socket by LIFECYCLE (installed), not by creation
        // index: held-call media and fenced stale establishes make the count
        // nondeterministic.
        await waitUntil(timeout: 5) { coordinator.debugConferenceWSMedia() != nil }
        await pumpMainActor(30)
        return coordinator.debugConferenceWSMedia()!
    }

    /// The socket whose handshake is still open (resumed but not connected) —
    /// i.e. the hung candidate. Live sockets park their RECEIVE loop too once
    /// the scripted messages are exhausted, so isParked alone cannot
    /// distinguish them.
    private func hungSocket(_ feed: Feed, excluding: FakeMediaSocket? = nil) -> FakeMediaSocket? {
        // The recovery candidate is the MOST RECENT open-handshake socket:
        // earlier ones may be stale per-call sockets fenced mid-attach by a
        // generation bump (resumed, never connected, already closed).
        for (media, socket) in zip(feed.created, feed.sockets).reversed()
        where socket !== excluding {
            if socket.resumed && !media.connectedForTest() { return socket }
        }
        return nil
    }

    /// created[] and sockets[] grow in lockstep; map a media to its socket.
    private func socket(for media: WebSocketCallMedia, in feed: Feed) -> FakeMediaSocket {
        feed.sockets[feed.created.firstIndex { $0 === media }!]
    }

    private func makeCoordinator(
        _ api: FakeGatewayAPI, _ feed: Feed, window: TimeInterval = 45
    ) -> CallCoordinator {
        CallCoordinator(
            api: api, callKit: FakeCallKit(),
            mediaProvider: FakeMediaProvider(session: FakeMediaSession()),
            registry: CallIdentityRegistry(), transport: "unified",
            mediaRecoveryWindow: window, gatewayID: gatewayID,
            wsMediaFactory: { feed.makeMedia() },
            routeCadence: nil)
    }

    /// The REQUIRED happy path: host socket drops -> bounded recovery opens ->
    /// the replacement socket stages, promotes (single owner) and its OWN
    /// fresh frame clears the episode; the conference is never closed.
    func testConferenceDropRecoversOnReplacementSocketFreshFrame() async throws {
        let api = makeAPI()
        // Only the startup creations are pre-queued; the recovery candidate
        // is queued WHEN the recovery attempt is expected, so script
        // consumption never depends on how many sockets startup created.
        let hungCandidate = WSScript(ready: false)
        let feed = Feed(scripts: [WSScript(), WSScript(), WSScript()])
        let coordinator = makeCoordinator(api, feed)
        let host = await startWSSConference(coordinator, api, feed)
        let hostSocket = socket(for: host, in: feed)

        // Queue the hung candidate right before the loss that arms recovery:
        // discard any un-consumed startup scripts so the recovery attempt is
        // guaranteed to consume exactly this one.
        feed.clearScripts()
        feed.append(hungCandidate)
        hostSocket.deliver(.failure(URLError(.networkConnectionLost)))
        await waitUntil(timeout: 3) { coordinator.debugRecoveryActive }
        XCTAssertTrue(coordinator.debugRecoveryActive)
        XCTAssertEqual(api.closeConferences, [], "recovery must not close the conference")
        XCTAssertTrue(coordinator.debugWSConferenceMediaIsInstalled(host))

        // A frame delivered BEFORE the candidate's exclusive promotion (it is
        // not the installed socket) must not clear the episode.
        await waitUntil(timeout: 5) { hungSocket(feed, excluding: hostSocket) != nil }
        let candidateSocket = hungSocket(feed, excluding: hostSocket)!
        let candidateIndex = feed.sockets.firstIndex { $0 === candidateSocket }!
        let candidate = feed.created[candidateIndex]
        XCTAssertFalse(coordinator.debugWSConferenceMediaIsInstalled(candidate))
        candidate.fireInboundRealFrameForTest()
        await pumpMainActor(30)
        XCTAssertTrue(coordinator.debugRecoveryActive,
                      "a pre-promotion frame must not clear the episode")

        // Complete the handshake: the staged attach promotes (audio is
        // active) and the INSTALLED socket's own frame clears the episode.
        hungCandidate.socket.deliver(.success(.string(#"{"type":"ready"}"#)))
        await waitUntil(timeout: 5) { coordinator.debugWSConferenceMediaIsInstalled(candidate) }
        candidate.fireInboundRealFrameForTest()
        await waitUntil(timeout: 3) { !coordinator.debugRecoveryActive }
        XCTAssertFalse(coordinator.debugRecoveryActive)
        XCTAssertEqual(api.closeConferences, [])
        XCTAssertNotNil(coordinator.conferenceRecord)
        coordinator.handleProviderReset()
    }

    /// Expiry: with no fresh media the SAME bounded window ends the
    /// conference truthfully (close + all calls ended) — no silent livelock.
    func testConferenceRecoveryExpiresWithoutFreshMedia() async throws {
        let api = makeAPI()
        let feed = Feed(scripts: [WSScript(), WSScript(), WSScript()])
        let coordinator = makeCoordinator(api, feed, window: 1.0)
        let host = await startWSSConference(coordinator, api, feed)
        let hostSocket = socket(for: host, in: feed)
        // Kill the audio session BEFORE the drop: every recovery promotion
        // rolls back and no fresh frame ever arrives, so the independent
        // 1 s deadline must end the conference.
        AudioSessionBridge.shared.didDeactivate(AVAudioSession.sharedInstance())
        hostSocket.deliver(.failure(URLError(.networkConnectionLost)))
        await waitUntil(timeout: 6) { !api.closeConferences.isEmpty }
        XCTAssertEqual(api.closeConferences, ["conf-1"])
        await waitUntil(timeout: 5) { coordinator.conferenceRecord == nil }
        XCTAssertNil(coordinator.conferenceRecord)
        XCTAssertFalse(coordinator.debugRecoveryActive)
        coordinator.handleProviderReset()
    }

    /// Remote terminal event DURING a pending (hung) recovery attach: the
    /// dissolution cancels the episode and closes the candidate; nothing
    /// resurrects the conference afterwards.
    func testRemoteConferenceEndDuringPendingRecoveryAttach() async throws {
        let api = makeAPI()
        // The recovery candidate hangs forever (no ready frame); it is queued
        // when the recovery attempt is expected, not guessed by position.
        let hungCandidate = WSScript(ready: false)
        let feed = Feed(scripts: [WSScript(), WSScript(), WSScript()])
        let coordinator = makeCoordinator(api, feed)
        let host = await startWSSConference(coordinator, api, feed)
        let hostSocket = socket(for: host, in: feed)
        feed.clearScripts()
        feed.append(hungCandidate)
        hostSocket.deliver(.failure(URLError(.networkConnectionLost)))
        await waitUntil(timeout: 3) { coordinator.debugRecoveryActive }
        await waitUntil(timeout: 3) { hungSocket(feed, excluding: hostSocket) != nil }

        // The candidate's connect is parked; the server now reports one leg.
        let legB = makeCallRecord(id: "call-B", state: .active, direction: .inbound, peer: "1002")
        api.conferenceSnapshot = makeConferenceRecord(id: "conf-1", legs: [legB])
        await waitUntil(timeout: 8) { coordinator.conferenceRecord == nil }
        XCTAssertFalse(coordinator.debugRecoveryActive, "remote end must cancel the episode")

        // Unwind the parked candidate: after the generation bump its guard
        // closes it quietly.
        if hungCandidate.socket.isParked {
            hungCandidate.socket.deliver(.failure(URLError(.networkConnectionLost)))
            await pumpMainActor(30)
        }
        coordinator.handleProviderReset()
    }

    /// A frame from the SUPERSEDED conference socket after recovery must not
    /// corrupt the new state (old-callback fencing, conference path).
    func testSupersededConferenceSocketProofAfterRecoveryIsIgnored() async throws {
        let api = makeAPI()
        let feed = Feed(scripts: [WSScript(), WSScript(), WSScript(), WSScript()])
        let coordinator = makeCoordinator(api, feed)
        let host = await startWSSConference(coordinator, api, feed)
        let hostSocket = socket(for: host, in: feed)
        hostSocket.deliver(.failure(URLError(.networkConnectionLost)))
        await waitUntil(timeout: 3) { coordinator.debugRecoveryActive }
        await waitUntil(timeout: 5) { feed.created.count > feed.sockets.firstIndex(where: { $0 === hostSocket })! }
        let candidate = feed.created.last!
        await waitUntil(timeout: 5) { coordinator.debugWSConferenceMediaIsInstalled(candidate) }
        candidate.fireInboundRealFrameForTest()
        await waitUntil(timeout: 3) { !coordinator.debugRecoveryActive }
        // The OLD host socket's late frame must be rejected: recovery stays
        // cleared (no crash, no re-armed episode, no closed conference).
        host.fireInboundRealFrameForTest()
        await pumpMainActor(30)
        XCTAssertFalse(coordinator.debugRecoveryActive)
        XCTAssertEqual(api.closeConferences, [])
        XCTAssertNotNil(coordinator.conferenceRecord)
        coordinator.handleProviderReset()
    }

    /// The direct conference path (.failed on the host leg) enters the SAME
    /// bounded recovery and restores media with a staged candidate adoption.
    func testDirectConferenceFailureRecoversWithStagedCandidate() async throws {
        let api = FakeGatewayAPI() // default ICE-only configuration
        let provider = FakeMediaProvider(session: FakeMediaSession())
        provider.createsNewSessions = true
        let coordinator = CallCoordinator(
            api: api, callKit: FakeCallKit(), mediaProvider: provider,
            registry: CallIdentityRegistry(), transport: "unified",
            gatewayID: gatewayID)
        func answer(_ id: String, _ peer: String) async {
            let uuid = UUID()
            coordinator.registerIncoming(
                gatewayId: id, uuid: uuid,
                record: makeCallRecord(id: id, state: .incomingRinging, direction: .inbound, peer: peer))
            try? await coordinator.answerIncoming(uuid: uuid)
        }
        await answer("call-A", "1001")
        await answer("call-B", "1002")
        await pumpMainActor(30)
        let legB = makeCallRecord(id: "call-B", state: .active, direction: .inbound, peer: "1002")
        let legA = makeCallRecord(id: "call-A", state: .active, direction: .inbound, peer: "1001")
        api.mergeResult = makeConferenceRecord(id: "conf-1", legs: [legB, legA])
        coordinator.mergeHeldCalls()
        await waitUntil(timeout: 5) { coordinator.conferenceRecord != nil && !api.conferenceOffers.isEmpty }
        // The conference host session is the LAST created media (the merge's
        // generation bump fences the in-flight per-call establishes).
        await pumpMainActor(30)
        let host = provider.created.last!

        // Host leg fails: recovery opens (was an immediate hangup pre-48).
        host.onState?(.failed)
        await waitUntil(timeout: 3) { coordinator.debugRecoveryActive }
        XCTAssertTrue(coordinator.debugRecoveryActive)
        print("DIAG recovery active, created=\(provider.created.count)")
        try? await Task.sleep(nanoseconds: 1_000_000_000)
        print("DIAG after 1s created=\(provider.created.count) conference=\(coordinator.conferenceRecord?.id ?? "nil")")

        // The staged candidate adopts itself (FakeMediaSession defaults to
        // .connected) and its own fresh-media hook clears the episode; the
        // incumbent host session is closed exactly once by the ordered swap.
        let baseCount = provider.created.count
        await waitUntil(timeout: 5) { provider.created.count > baseCount }
        await pumpMainActor(30)
        provider.created.last!.debugFireFreshMedia()
        await waitUntil(timeout: 3) { !coordinator.debugRecoveryActive }
        XCTAssertFalse(coordinator.debugRecoveryActive)
        XCTAssertEqual(api.closeConferences, [])
        XCTAssertGreaterThanOrEqual(host.closeCount, 1, "the failed incumbent must be retired")
        XCTAssertNotNil(coordinator.conferenceRecord)
        coordinator.handleProviderReset()
    }
}
