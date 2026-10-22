import XCTest
@testable import CallRelay

/// Build-55 key isolation hardening: a delayed response from an OLD pairing
/// (or a stale session) must never populate the new pairing's policy,
/// entries, history or recording preference. Each store carries an internal
/// generation that `reset()` bumps; late results are dropped without touching
/// shared state, notices or busy.
@MainActor
final class HarassmentStoreFencingTests: XCTestCase {
    private func waitUntil(_ condition: @autoclosure () -> Bool, timeout: TimeInterval = 3) async {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if condition() { return }
            await Task.yield()
            try? await Task.sleep(nanoseconds: 5_000_000)
        }
    }

    func testStaleRefreshFromOldPairingIsDiscarded() async {
        let old = FakeGatewayAPI()
        old.harassmentSupported = true
        old.harassmentCall = true
        old.harassmentEntryRecords = [HarassmentEntryRecord(
            id: "old_trust", number: "+8613800000199", matchKind: "exact",
            mode: "trust", action: "allow", source: "manual", listId: nil,
            enabled: true, createdAt: nil)]
        old.suspendNextHarassmentSettings()

        let store = HarassmentStore()
        let staleRefresh = Task { await store.refresh(api: old) }
        await waitUntil(old.harassmentSettingsSuspendedConfirmed)

        // The pairing changes while the old response is still in flight.
        store.reset()
        old.resumeHarassmentSettings()
        await staleRefresh.value

        XCTAssertEqual(store.support, .unknown, "old session must not mark the new pairing supported")
        XCTAssertTrue(store.entries.isEmpty, "old key's entries leaked into the new session")
        XCTAssertFalse(store.callBlocking)
        XCTAssertNil(store.lastError)

        // The new pairing loads its own (empty) policy normally.
        let new = FakeGatewayAPI()
        new.harassmentSupported = true
        new.harassmentSMS = true
        await store.refresh(api: new)
        XCTAssertEqual(store.support, .supported)
        XCTAssertTrue(store.smsBlocking)
        XCTAssertFalse(store.callBlocking)
        XCTAssertTrue(store.entries.isEmpty)
    }

    func testStaleMutationFromOldPairingIsDiscarded() async {
        let old = FakeGatewayAPI()
        old.harassmentSupported = true
        let store = HarassmentStore()
        await store.refresh(api: old)
        XCTAssertEqual(store.support, .supported)

        old.suspendNextAddHarassmentEntry()
        let staleAdd = Task {
            await store.addEntry(api: old, number: "+8613800000200", matchKind: "exact",
                                 mode: "block", action: "reject", replaceOpposite: true)
        }
        await waitUntil(old.addEntrySuspendedConfirmed)

        store.reset()
        old.resumeAddHarassmentEntry()
        let result = await staleAdd.value

        XCTAssertNil(result, "stale write must not report success")
        XCTAssertTrue(store.entries.isEmpty, "stale write leaked an entry")
        XCTAssertNil(store.lastError, "stale write must not set a notice")
        XCTAssertFalse(store.busy, "stale write must not leave the new session busy")
    }

    func testRecordingConfigStaleRefreshIsDiscarded() async {
        let old = FakeGatewayAPI()
        old.recordingAutoModeValue = "outgoing"
        old.recordingDeviceAutoRecord = true
        old.suspendNextRecordingSettings()

        let config = RecordingConfigStore()
        let staleRefresh = Task { await config.refresh(api: old) }
        await waitUntil(old.recordingSettingsSuspendedConfirmed)

        config.reset()
        old.resumeRecordingSettings()
        await staleRefresh.value

        XCTAssertNil(config.supported, "old session must not populate the new config")
        XCTAssertFalse(config.deviceAutoRecord)
        XCTAssertEqual(config.autoMode, "off")

        // A genuinely new session shows its own preference.
        let new = FakeGatewayAPI()
        new.recordingAutoModeValue = "all"
        new.recordingDeviceAutoRecord = false
        await config.refresh(api: new)
        XCTAssertEqual(config.supported, true)
        XCTAssertFalse(config.deviceAutoRecord)
    }
}
