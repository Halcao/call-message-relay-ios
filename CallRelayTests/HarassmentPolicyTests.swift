import XCTest
@testable import CallRelay

/// Build-55 local fallback semantics: the master switches gate interception,
/// imported/converted payloads default to mark-only, and history is local.
@MainActor
final class HarassmentPolicyTests: XCTestCase {
    /// Bounded async poll for the pending-window assertions.
    private func waitUntil(_ condition: @autoclosure () -> Bool, timeout: TimeInterval = 3) async {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if condition() { return }
            await Task.yield()
            try? await Task.sleep(nanoseconds: 5_000_000)
        }
    }

    private func makeStore() -> SpamFilterStore {
        SpamFilterStore(storeURL: FileManager.default.temporaryDirectory
            .appendingPathComponent("harassment-test-\(UUID().uuidString).json"))
    }

    func testMasterSwitchesGateLocalScreening() {
        let store = makeStore()
        store.addRule(kind: .senderExact, value: "13800000001", label: "测试")
        // Default OFF: a block rule exists but nothing is rejected.
        if case .reject = store.policy().screenCall(peer: "13800000001", listHits: []) {
            XCTFail("call rejected while the call switch is off")
        }
        store.setCallBlocking(true, notifyGateway: false)
        guard case .reject = store.policy().screenCall(peer: "13800000001", listHits: []) else {
            return XCTFail("call not rejected after enabling the call switch")
        }
        // Trusted number still wins while blocking is on.
        store.addRule(kind: .whitelistSender, value: "13800000001", label: "信任")
        if case .reject = store.policy().screenCall(peer: "13800000001", listHits: []) {
            XCTFail("trusted number was rejected")
        }

        // SMS switch is independent (use a number without a trust rule).
        store.addRule(kind: .senderExact, value: "13800000010", label: "短信拦截")
        guard case .unknown = store.policy().classifySMS(
            peer: "13800000010", body: "hi", isKnownSender: false) else {
            return XCTFail("SMS classified while the SMS switch is off")
        }
        store.setSMSBlocking(true, notifyGateway: false)
        guard case .junk = store.policy().classifySMS(
            peer: "13800000010", body: "hi", isKnownSender: false) else {
            return XCTFail("SMS not quarantined after enabling the SMS switch")
        }
    }

    func testTogglesAndHistoryPersist() {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("harassment-persist-\(UUID().uuidString).json")
        let store = SpamFilterStore(storeURL: url)
        store.setCallBlocking(true, notifyGateway: false)
        store.setSMSBlocking(true, notifyGateway: false)
        store.recordScreening(peer: "13800000002", kind: "call", action: "reject", reason: "blocklist")
        store.flush()

        let reopened = SpamFilterStore(storeURL: url)
        XCTAssertTrue(reopened.callBlockingEnabled)
        XCTAssertTrue(reopened.smsBlockingEnabled)
        XCTAssertEqual(reopened.screeningHistory.count, 1)
        XCTAssertEqual(reopened.screeningHistory.first?.peer, "13800000002")
        reopened.clearScreeningHistory()
        XCTAssertTrue(reopened.screeningHistory.isEmpty)
    }

    func testGatewayImportPayloadDefaultsImportedListsToLabel() {
        let store = makeStore()
        store.addRule(kind: .senderExact, value: "13800000003", label: "拦截")
        store.addRule(kind: .whitelistSender, value: "13800000004", label: "信任")
        store.addRule(kind: .regex, value: "投资.*回报", label: "正则")
        _ = store.importPasted(name: "旧名单", text: "13800000005\n13800000006")
        // Local list defaults to label: the payload must stay mark-only.
        let payloads = store.gatewayImportPayload()
        let imported = payloads.filter { $0.number == "13800000005" || $0.number == "13800000006" }
        XCTAssertEqual(imported.count, 2)
        XCTAssertTrue(imported.allSatisfy { $0.mode == "block" && $0.action == "label" },
                      "imported list numbers must default to label: \(imported)")

        let block = payloads.first { $0.number == "13800000003" }
        XCTAssertEqual(block?.mode, "block")
        XCTAssertEqual(block?.action, "reject")
        let trust = payloads.first { $0.number == "13800000004" }
        XCTAssertEqual(trust?.mode, "trust")
        let regex = payloads.first { $0.number == "投资.*回报" }
        XCTAssertEqual(regex?.matchKind, "regex")
    }

    func testImportedListRejectModeIsExplicit() {
        let store = makeStore()
        _ = store.importPasted(name: "明确拦截", text: "13800000007")
        guard let list = store.lists.first(where: { $0.name == "明确拦截" }) else {
            return XCTFail("list missing")
        }
        store.list(mode: .reject, for: list.id)
        let payload = store.gatewayImportPayload().first { $0.number == "13800000007" }
        XCTAssertEqual(payload?.action, "reject", "an explicitly rejected list must import as reject")
    }

    func testUnsupportedGatewayDetection() {
        XCTAssertTrue(HarassmentStore.isUnsupported(APIError.notReady("旧网关")))
        XCTAssertTrue(HarassmentStore.isUnsupported(APIError.http(status: 404, code: "CB-V2-404", message: nil)))
        XCTAssertFalse(HarassmentStore.isUnsupported(APIError.http(status: 500, code: nil, message: nil)))
        XCTAssertFalse(HarassmentStore.isUnsupported(APIError.cancelled))
    }

    func testRecordingAutoModeLabels() {
        let config = RecordingConfigStore()
        config.reset()
        XCTAssertEqual(config.autoModeLabel, "关闭")
    }

    // MARK: Build-67 recording switch: intent, fences, honest failure

    /// The switch persists through the gateway and a later refresh keeps the
    /// confirmed value.
    func testRecordingSwitchPersistsAndSurvivesARefresh() async {
        let api = FakeGatewayAPI()
        api.recordingDeviceAutoRecord = false
        let config = RecordingConfigStore()
        await config.refresh(api: api)
        XCTAssertEqual(config.deviceAutoRecord, false)
        XCTAssertNil(config.lastError)

        let ok = await config.setDeviceAutoRecord(api: api, true)
        XCTAssertTrue(ok)
        XCTAssertTrue(config.deviceAutoRecord, "the chosen state must stay on after the ack")
        XCTAssertTrue(api.recordingDeviceAutoRecord, "the gateway really persisted the choice")

        await config.refresh(api: api)
        XCTAssertTrue(config.deviceAutoRecord, "a later refresh keeps the persisted value")
        XCTAssertNil(config.lastError)
        XCTAssertFalse(config.pendingWrite)
    }

    /// A refresh racing a pending write must never flip the chosen switch
    /// back (the old server value predates the write).
    func testStaleRefreshDuringPendingWriteCannotFlipTheSwitch() async {
        let api = FakeGatewayAPI()
        api.recordingDeviceAutoRecord = false
        let config = RecordingConfigStore()
        await config.refresh(api: api)

        api.armAutoRecordHold()
        let task = Task { await config.setDeviceAutoRecord(api: api, true) }
        await waitUntil(config.pendingWrite)
        XCTAssertTrue(config.deviceAutoRecord, "the switch shows the user's intent while pending")

        // A refresh lands while the write is parked: the server still says
        // false, but it must NOT overwrite the chosen value.
        await config.refresh(api: api)
        XCTAssertTrue(config.deviceAutoRecord,
                      "a pre-write server copy must never flip the switch back")

        api.resumeAutoRecord(.success(()))
        let ok = await task.value
        XCTAssertTrue(ok)
        XCTAssertTrue(config.deviceAutoRecord)
        XCTAssertFalse(config.pendingWrite)
    }

    /// A rejected write restores the last CONFIRMED value and explains why.
    func testRejectedRecordingSwitchRestoresTheConfirmedValueAndExplains() async {
        let api = FakeGatewayAPI()
        api.recordingDeviceAutoRecord = false
        let config = RecordingConfigStore()
        await config.refresh(api: api)

        api.autoRecordUpdateError = APIError.http(status: 403, code: "CB-V2-403", message: "forbidden")
        let ok = await config.setDeviceAutoRecord(api: api, true)
        XCTAssertFalse(ok)
        XCTAssertFalse(config.deviceAutoRecord,
                       "a rejected write must restore the confirmed state, never a fake ON")
        XCTAssertNotNil(config.lastError, "the bounce must be explained")
        XCTAssertFalse(config.pendingWrite)

        // The next successful write clears the error.
        api.autoRecordUpdateError = nil
        let ok2 = await config.setDeviceAutoRecord(api: api, true)
        XCTAssertTrue(ok2)
        XCTAssertTrue(config.deviceAutoRecord)
        XCTAssertNil(config.lastError)
    }

    /// A pairing reset clears pending state and errors: an old write's
    /// late return must not touch the new session.
    func testResetDuringPendingWriteClearsPendingStateAndLateReturnIsInert() async {
        let api = FakeGatewayAPI()
        api.recordingDeviceAutoRecord = false
        let config = RecordingConfigStore()
        await config.refresh(api: api)

        api.armAutoRecordHold()
        let task = Task { await config.setDeviceAutoRecord(api: api, true) }
        await waitUntil(config.pendingWrite)
        config.reset()
        XCTAssertFalse(config.pendingWrite)
        XCTAssertNil(config.lastError)

        api.resumeAutoRecord(.success(()))
        let ok = await task.value
        XCTAssertFalse(ok, "a superseded write never claims the new session's state")
        XCTAssertFalse(config.deviceAutoRecord)
        XCTAssertFalse(config.pendingWrite)
    }

    // MARK: Gateway echo verification (the "silently ignored field" class)

    /// The recording switch write must be CONFIRMED by the gateway's echoed
    /// preference: a 200 that ignores the field (or stores the opposite)
    /// must surface as a failure instead of a fake ON state.
    func testAutoRecordWriteOutcomeVerifiesTheGatewayEcho() {
        func body(_ json: String) -> Data { Data(json.utf8) }
        XCTAssertNil(HTTPGatewayAPI.autoRecordWriteOutcome(
            requested: true, responseBody: body(#"{"autoRecord":true,"defaultLineId":"line1"}"#)),
            "a matching echo confirms the write")
        XCTAssertNotNil(HTTPGatewayAPI.autoRecordWriteOutcome(
            requested: true, responseBody: body(#"{"autoRecord":false}"#)),
            "the opposite stored value must be reported as not saved")
        XCTAssertNotNil(HTTPGatewayAPI.autoRecordWriteOutcome(
            requested: true, responseBody: body(#"{"defaultLineId":"line1","uplinkDenoise":false}"#)),
            "a 200 without the echo (older gateway ignoring the field) must not look saved")
        // The V2 endpoint always returns the stored view: an empty or
        // malformed 2xx is UNCONFIRMED, never a confirmed write.
        XCTAssertNotNil(HTTPGatewayAPI.autoRecordWriteOutcome(
            requested: true, responseBody: Data()),
            "an empty 2xx body cannot confirm the stored preference")
        XCTAssertNotNil(HTTPGatewayAPI.autoRecordWriteOutcome(
            requested: true, responseBody: body("not json")),
            "a non-JSON 2xx body cannot confirm the stored preference")
        XCTAssertNotNil(HTTPGatewayAPI.autoRecordWriteOutcome(
            requested: true, responseBody: body("")),
            "an empty object cannot confirm the stored preference")
    }
}
