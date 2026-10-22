import AVFoundation
import XCTest
@testable import CallRelay

@MainActor
final class RecordingTests: XCTestCase {
    private final class TestDriver: CallDriver {
        var onUpdate: ((ActiveCallViewState?) -> Void)?
        var onQuality: ((MediaQuality) -> Void)?
        var onEnded: ((String) -> Void)?
        private(set) var resetCount = 0

        func dial(peer: String, lineId: String?) {}
        func reportIncomingPush(gatewayId: String, uuid: UUID, handle: String, record: CallRecord?) async {}
        func reportIncomingFromEvent(_ call: CallRecord) async {}
        func answerCurrent() {}
        func endCall(gatewayId: String) async {}
        func hangup() {}
        func playDTMF(_ digit: String) {}
        func setMuted(_ muted: Bool) {}
        func setSpeaker(_ enabled: Bool) {}
        func reset() { resetCount += 1 }
    }

    private func makeModel(api: FakeGatewayAPI) -> (AppModel, TestDriver) {
        let model = AppModel()
        let driver = TestDriver()
        model.configureForTesting(api: api, driver: driver, lines: [], defaultLineId: nil)
        return (model, driver)
    }

    private func makePlayingController() throws -> (RecordingPlaybackController, AVAudioPlayer, AVAudioPlayer) {
        // 0.2 s of silence in a valid WAV container; AVAudioPlayer only needs
        // a decodable payload, no audio hardware is touched by construction.
        var data = Data()
        let payload = Data(repeating: 0, count: 3200)
        let dataSize = UInt32(payload.count)
        func le32(_ value: UInt32) -> Data { withUnsafeBytes(of: value.littleEndian) { Data($0) } }
        func le16(_ value: UInt16) -> Data { withUnsafeBytes(of: value.littleEndian) { Data($0) } }
        data.append("RIFF".data(using: .ascii)!)
        data.append(le32(36 + dataSize))
        data.append("WAVE".data(using: .ascii)!)
        data.append("fmt ".data(using: .ascii)!)
        data.append(le32(16))
        data.append(le16(1))
        data.append(le16(1))
        data.append(le32(8000))
        data.append(le32(16000))
        data.append(le16(2))
        data.append(le16(16))
        data.append("data".data(using: .ascii)!)
        data.append(le32(dataSize))
        data.append(payload)
        return (RecordingPlaybackController(), try AVAudioPlayer(data: data), try AVAudioPlayer(data: data))
    }

    func testPlaybackRefusedWhileCallLive() async {
        let controller = RecordingPlaybackController()
        await controller.toggle(
            id: "rec_1",
            load: { Data("not audio".utf8) },
            isCallActive: { true }
        )
        XCTAssertNil(controller.playingId)
        XCTAssertEqual(controller.errorMessage, "通话中暂不可播放录音。")
    }

    func testStopForCallStartKeepsSessionOwnership() throws {
        let (controller, current, stale) = try makePlayingController()
        controller.testingSetPlaying(id: "rec_1", player: current)
        XCTAssertTrue(controller.testingIsPlaying)
        controller.stopForCallStart()
        XCTAssertFalse(controller.testingIsPlaying)
        XCTAssertNil(controller.playingId)
        // A stale finish callback after the call took ownership must be a
        // no-op: nothing is playing and no session call is made.
        controller.audioPlayerDidFinishPlaying(stale, successfully: true)
    }

    func testStalePlayerFinishDoesNotStopNewPlayback() async throws {
        let (controller, current, stale) = try makePlayingController()
        controller.testingSetPlaying(id: "rec_new", player: current)
        controller.audioPlayerDidFinishPlaying(stale, successfully: true) // old delegate
        try? await Task.sleep(nanoseconds: 50_000_000)
        XCTAssertTrue(controller.testingIsPlaying)
        XCTAssertEqual(controller.playingId, "rec_new")
        // The current player finishing stops the clip normally.
        controller.audioPlayerDidFinishPlaying(current, successfully: true)
        await waitUntil { !controller.testingIsPlaying }
        XCTAssertNil(controller.playingId)
    }

    func testSessionTeardownClearsRecordingState() async {
        let api = FakeGatewayAPI()
        api.recordingsStub = [record(state: "ready")]
        let (model, _) = makeModel(api: api)
        await model.refreshRecordings()
        XCTAssertEqual(model.recordings.count, 1)
        model.unpair()
        XCTAssertTrue(model.recordings.isEmpty)
        XCTAssertNil(model.recordingNotice)
        XCTAssertNil(model.lastDeletedRecordingId)
        XCTAssertFalse(model.recordingRequestPending)
    }

    func testDeleteFailureNoticeSurfacesForList() async {
        let api = FakeGatewayAPI()
        api.recordingsStub = [record(state: "ready")]
        api.recordingDeleteResult = .failure(APIError.network(URLError(.timedOut)))
        let (model, _) = makeModel(api: api)
        await model.refreshRecordings()
        _ = await model.deleteRecording("rec_1")
        XCTAssertEqual(model.recordingNotice, "删除录音失败，请稍后重试。")
    }

    private func activeState(callId: String = "line1:call1") -> ActiveCallViewState {
        ActiveCallViewState(
            gatewayCallId: callId, peer: "10086", isOutgoing: true,
            phase: .active(startedAt: Date()), isMuted: false,
            startedAt: Date(), connectedAt: Date()
        )
    }

    private func record(
        id: String = "rec_1",
        callId: String = "line1:call1",
        state: String,
        durationMs: Int64 = 0
    ) -> RecordingRecord {
        RecordingRecord(
            id: id, callId: callId, lineId: "line1", peer: "10086",
            state: state, durationMs: durationMs, createdAt: Date().unixMilliseconds,
            keyId: "key_1"
        )
    }

    func testRecordingRecordDecodesGatewayPayload() throws {
        let json = """
        {
          "id": "rec_abc", "callId": "line1:c1", "lineId": "line1", "lineName": "线路一",
          "direction": "outbound", "peer": "10086", "state": "ready", "triggerMode": "manual",
          "startedAt": 1700000000000, "stoppedAt": 1700000060000, "durationMs": 60000,
          "sizeBytes": 960000, "sha256": "aa", "container": "wav", "codec": "pcm_s16le",
          "sampleRate": 8000, "channels": 1, "createdByDeviceId": "dev_1", "keyId": "key_1",
          "createdAt": 1700000000000, "expiresAt": 1702592000000
        }
        """
        let decoded = try JSONDecoder().decode(RecordingRecord.self, from: Data(json.utf8))
        XCTAssertEqual(decoded.id, "rec_abc")
        XCTAssertEqual(decoded.state, "ready")
        XCTAssertTrue(decoded.isReady)
        XCTAssertFalse(decoded.isFailed)
        XCTAssertEqual(decoded.durationLabel, "1:00")
        XCTAssertEqual(decoded.keyId, "key_1")
        XCTAssertEqual(decoded.direction, "outbound")
    }

    func testCallRecordDecodesServerRecordingFields() throws {
        let json = """
        {
          "id": "line1:c1", "direction": "outbound", "peer": "10086", "state": "active",
          "startedAt": 1700000000000, "recordingId": "rec_1", "recordingState": "recording",
          "recordingDurationMs": 4000
        }
        """
        let call = try JSONDecoder().decode(CallRecord.self, from: Data(json.utf8))
        XCTAssertEqual(call.recordingId, "rec_1")
        XCTAssertEqual(call.recordingState, "recording")
        XCTAssertEqual(call.recordingDurationMs, 4000)
    }

    func testStartRecordingAppliesConfirmedServerState() async {
        let api = FakeGatewayAPI()
        api.recordingStartResult = .success(record(state: "recording"))
        let (model, driver) = makeModel(api: api)
        model.activeCall = activeState()

        await model.startCallRecording()

        XCTAssertEqual(api.recordingStarts, ["line1:call1"])
        XCTAssertEqual(model.activeCall?.recordingId, "rec_1")
        XCTAssertEqual(model.activeCall?.recordingState, "recording")
        XCTAssertFalse(model.recordingRequestPending)
        XCTAssertNil(model.recordingNotice)
    }

    func testRecordingIndicatorOnlyFollowsServerState() async {
        let api = FakeGatewayAPI()
        // A failed start must NOT show a recording indicator.
        api.recordingStartResult = .failure(APIError.http(status: 409, code: "CB-V2-409C", message: "会议通话暂不支持录音"))
        let (model, driver) = makeModel(api: api)
        model.activeCall = activeState()

        await model.startCallRecording()

        XCTAssertEqual(model.activeCall?.recordingState, "")
        XCTAssertFalse(model.activeCall?.isRecording ?? true)
        XCTAssertEqual(model.recordingNotice, "会议通话暂不支持录音")
    }

    func testStopRecordingAppliesFinalizingState() async {
        let api = FakeGatewayAPI()
        api.recordingStartResult = .success(record(state: "recording"))
        api.recordingStopResult = .success(record(state: "finalizing", durationMs: 12500))
        let (model, driver) = makeModel(api: api)
        model.activeCall = activeState()

        await model.startCallRecording()
        await model.stopCallRecording()

        XCTAssertEqual(api.recordingStops, ["line1:call1"])
        XCTAssertEqual(model.activeCall?.recordingState, "finalizing")
        XCTAssertEqual(model.activeCall?.recordingDurationMs, 12500)
    }

    func testRemoteRecordingEventUpdatesCallAndList() async throws {
        let api = FakeGatewayAPI()
        api.recordingsStub = [record(state: "recording")]
        let (model, driver) = makeModel(api: api)
        model.activeCall = activeState()

        let eventJSON = """
        {"id":"evt_1","seq":1,"type":"recording.started","createdAt":1700000000000,
         "data":{"id":"rec_remote","callId":"line1:call1","lineId":"line1","state":"recording",
                 "durationMs":250,"createdAt":1700000000000,"keyId":"key_1"}}
        """
        let event = try JSONDecoder().decode(GatewayEvent.self, from: Data(eventJSON.utf8))
        model.testingHandleEvent(event)

        XCTAssertEqual(model.activeCall?.recordingId, "rec_remote")
        XCTAssertEqual(model.activeCall?.recordingState, "recording")
        await waitUntil(timeout: 2) { model.recordings.count == 1 }
        XCTAssertEqual(model.recordings.first?.id, "rec_1")
    }

    func testRecordingFailedEventSurfacesNotice() async throws {
        let api = FakeGatewayAPI()
        let (model, driver) = makeModel(api: api)
        model.activeCall = activeState()

        let eventJSON = """
        {"id":"evt_2","seq":2,"type":"recording.failed","createdAt":1700000000000,
         "data":{"id":"rec_1","callId":"line1:call1","lineId":"line1","state":"failed",
                 "failureCode":"writer_overflow","durationMs":0,"createdAt":1700000000000,
                 "keyId":"key_1"}}
        """
        let event = try JSONDecoder().decode(GatewayEvent.self, from: Data(eventJSON.utf8))
        model.testingHandleEvent(event)

        XCTAssertEqual(model.activeCall?.recordingState, "failed")
        XCTAssertEqual(model.recordingNotice, "录音未完成，已停止。")
    }

    func testDeleteRecordingRemovesLocallyAndReportsFailure() async {
        let api = FakeGatewayAPI()
        api.recordingsStub = [record(state: "ready")]
        let (model, _) = makeModel(api: api)
        await model.refreshRecordings()
        XCTAssertEqual(model.recordings.count, 1)

        api.recordingsStub = []
        let deleted = await model.deleteRecording("rec_1")
        XCTAssertTrue(deleted)
        XCTAssertTrue(model.recordings.isEmpty)
        XCTAssertEqual(model.lastDeletedRecordingId, "rec_1")

        api.recordingDeleteResult = .failure(APIError.network(URLError(.timedOut)))
        api.recordingsStub = [record(state: "ready")]
        await model.refreshRecordings()
        let failed = await model.deleteRecording("rec_1")
        XCTAssertFalse(failed)
        XCTAssertEqual(model.recordingNotice, "删除录音失败，请稍后重试。")
        XCTAssertEqual(model.recordings.count, 1)
    }

    func testExportFileNameIsSanitized() {
        let rec = RecordingRecord(
            id: "rec_1", callId: "line1:c1", lineId: "line1", peer: "+86/138 0000",
            state: "ready", durationMs: 1000, createdAt: 1700000000000
        )
        let name = RecordingsView.exportFileName(rec)
        XCTAssertFalse(name.contains("/"))
        XCTAssertTrue(name.hasPrefix("CallRelay-"))
        XCTAssertTrue(name.hasSuffix(".wav"))
    }
}

/// Build-54 conference recording coverage: host-only start/stop through the
/// conference endpoints, server-confirmed state on the conference record,
/// same-conference event fencing, and the honest merge feedback when a 1:1
/// segment is finalized by merging.
@MainActor
final class ConferenceRecordingTests: XCTestCase {
    private final class NoticeDelegate: CallCoordinatorDelegate {
        var mergeInterrupts = 0
        var groupChanges = 0
        func call(_ gatewayId: String, phaseChanged phase: ActiveCallPhase) {}
        func callDidEnd(gatewayId: String, reason: EndedCallReason) {}
        func callGroupChanged() { groupChanges += 1 }
        func conferenceRecordingInterruptedByMerge() { mergeInterrupts += 1 }
    }

    private func makeCoordinator(api: FakeGatewayAPI, media: FakeMediaSession)
    -> (CallCoordinator, FakeMediaProvider, CallIdentityRegistry, NoticeDelegate) {
        let callKit = FakeCallKit()
        let provider = FakeMediaProvider(session: media)
        let registry = CallIdentityRegistry()
        let coordinator = CallCoordinator(
            api: api, callKit: callKit, mediaProvider: provider,
            registry: registry, transport: "tailnet"
        )
        let delegate = NoticeDelegate()
        coordinator.delegate = delegate
        return (coordinator, provider, registry, delegate)
    }

    @discardableResult
    private func answer(
        _ coordinator: CallCoordinator, api: FakeGatewayAPI,
        registry: CallIdentityRegistry, id: String, peer: String
    ) async -> UUID {
        let uuid = await registry.associate(gatewayId: id)
        coordinator.registerIncoming(
            gatewayId: id, uuid: uuid,
            record: makeCallRecord(id: id, state: .incomingRinging, direction: .inbound, peer: peer)
        )
        try? await coordinator.answerIncoming(uuid: uuid)
        await waitUntil { api.answers.contains(id) }
        return uuid
    }

    /// Answers two calls and merges them into a hosted conference.
    private func startConference(
        api: FakeGatewayAPI, media: FakeMediaSession
    ) async -> (CallCoordinator, NoticeDelegate) {
        let (coordinator, _, registry, delegate) = makeCoordinator(api: api, media: media)
        await answer(coordinator, api: api, registry: registry, id: "call-A", peer: "1001")
        await answer(coordinator, api: api, registry: registry, id: "call-B", peer: "1002")
        await waitUntil { api.answers.count == 2 }
        let legB = makeCallRecord(id: "call-B", state: .active, direction: .inbound, peer: "1002")
        let legA = makeCallRecord(id: "call-A", state: .active, direction: .inbound, peer: "1001")
        api.mergeResult = makeConferenceRecord(id: "conf-1", legs: [legB, legA])
        api.conferenceSnapshot = makeConferenceRecord(id: "conf-1", legs: [legB, legA])
        coordinator.mergeHeldCalls()
        await waitUntil(timeout: 5) { coordinator.conferenceRecord != nil }
        return (coordinator, delegate)
    }

    private func conferenceRecording(
        id: String = "rec_conf", state: String, conferenceId: String = "conf-1", durationMs: Int64 = 0
    ) -> RecordingRecord {
        RecordingRecord(
            id: id, callId: conferenceId, lineId: "gw:line", peer: "1001、1002",
            state: state, durationMs: durationMs, createdAt: Date().unixMilliseconds,
            keyId: "key_1", kind: "conference", conferenceId: conferenceId
        )
    }

    func testConferenceRecordingToggleUsesHostEndpointAndConfirmedState() async throws {
        let api = FakeGatewayAPI()
        let (coordinator, _) = await startConference(api: api, media: FakeMediaSession())

        api.conferenceRecordingStartResult = .success(conferenceRecording(state: "recording"))
        try await coordinator.setConferenceRecording(true)
        XCTAssertEqual(api.conferenceRecordingStarts, ["conf-1"])
        XCTAssertEqual(coordinator.conferenceRecord?.recordingId, "rec_conf")
        XCTAssertEqual(coordinator.conferenceRecord?.recordingState, "recording")
        XCTAssertTrue(coordinator.conferenceRecord?.isRecording == true)

        api.conferenceRecordingStopResult = .success(
            conferenceRecording(state: "finalizing", durationMs: 4200))
        try await coordinator.setConferenceRecording(false)
        XCTAssertEqual(api.conferenceRecordingStops, ["conf-1"])
        XCTAssertEqual(coordinator.conferenceRecord?.recordingState, "finalizing")
        XCTAssertEqual(coordinator.conferenceRecord?.recordingDurationMs, 4200)
    }

    func testConferenceRecordingEventsAreFencedToThisConference() async {
        let api = FakeGatewayAPI()
        let (coordinator, _) = await startConference(api: api, media: FakeMediaSession())

        // A foreign conference id must never touch this conference's state.
        coordinator.applyConferenceRecording(
            conferenceRecording(id: "rec_other", state: "recording", conferenceId: "conf-OTHER"))
        XCTAssertNil(coordinator.conferenceRecord?.recordingState)
        coordinator.applyConferenceRecording(conferenceRecording(state: "recording", durationMs: 800))
        XCTAssertEqual(coordinator.conferenceRecord?.recordingState, "recording")
        XCTAssertEqual(coordinator.conferenceRecord?.recordingDurationMs, 800)
    }

    func testMergeWhileRecordingReportsSavedSegmentOnce() async throws {
        let api = FakeGatewayAPI()
        let (coordinator, _, registry, delegate) = makeCoordinator(api: api, media: FakeMediaSession())
        // Held call B first, then active call A carrying the 1:1 recording.
        await answer(coordinator, api: api, registry: registry, id: "call-B", peer: "1002")
        await answer(coordinator, api: api, registry: registry, id: "call-A", peer: "1001")
        await waitUntil { api.answers.count == 2 }
        XCTAssertEqual(coordinator.activeCallRecord?.id, "call-A")

        let eventJSON = """
        {"id":"evt_1","seq":1,"type":"call.updated","createdAt":1,
         "data":{"id":"call-A","direction":"inbound","peer":"1001","state":"active",
                 "startedAt":1,"recordingId":"rec_1","recordingState":"recording",
                 "recordingDurationMs":1000}}
        """
        coordinator.ingest(event: try JSONDecoder().decode(GatewayEvent.self, from: Data(eventJSON.utf8)))
        XCTAssertEqual(coordinator.activeCallRecord?.recordingInProgress, true)

        let legA = makeCallRecord(id: "call-A", state: .active, direction: .inbound, peer: "1001")
        let legB = makeCallRecord(id: "call-B", state: .active, direction: .inbound, peer: "1002")
        api.mergeResult = makeConferenceRecord(id: "conf-1", legs: [legA, legB])
        api.conferenceSnapshot = makeConferenceRecord(id: "conf-1", legs: [legA, legB])
        coordinator.mergeHeldCalls()
        await waitUntil(timeout: 5) { coordinator.conferenceRecord != nil }

        await waitUntil(timeout: 2) { delegate.mergeInterrupts == 1 }
        XCTAssertEqual(delegate.mergeInterrupts, 1, "exactly one saved-segment notice per merge")
        // Truthful stopped state: the conference carries no ongoing segment.
        XCTAssertFalse(coordinator.conferenceRecord?.recordingInProgress == true)
        XCTAssertNil(coordinator.conferenceRecord?.recordingState)
    }
}
