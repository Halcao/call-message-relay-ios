import XCTest
@testable import CallRelay

/// AppModel-level key isolation: every async harassment operation captures
/// the pairing session generation AND the API client at entry and aborts the
/// moment the pairing changes, so no old payload can reach the new key and no
/// old result can mirror into the new session or surface a stale notice.
@MainActor
final class AppModelHarassmentFencingTests: XCTestCase {
    private func makeModel() -> AppModel {
        AppModel(
            defaults: UserDefaults(suiteName: "fence-\(UUID().uuidString)")!,
            spamFilter: SpamFilterStore(storeURL: FileManager.default.temporaryDirectory
                .appendingPathComponent("fence-\(UUID().uuidString).json")))
    }

    private func waitUntil(_ condition: @autoclosure () -> Bool, timeout: TimeInterval = 3) async {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if condition() { return }
            await Task.yield()
            try? await Task.sleep(nanoseconds: 5_000_000)
        }
    }

    func testDelayedImportIsAbortedOnRepairAndWritesNothingToNewKey() async {
        let old = FakeGatewayAPI()
        old.harassmentSupported = true
        let new = FakeGatewayAPI()
        new.harassmentSupported = true

        let model = makeModel()
        model.api = old
        await model.refreshHarassmentPolicy()
        XCTAssertEqual(model.harassment.support, .supported)
        model.spamFilter.addRule(kind: .senderExact, value: "+8613800000301", label: "拦截")
        // A SECOND importable rule is required to exercise the real bug:
        // remaining loop iterations must not reach the NEW key after a repair.
        model.spamFilter.addRule(kind: .keyword, value: "发票", label: "拦截")
        XCTAssertGreaterThanOrEqual(model.spamFilter.gatewayImportPayload().count, 2)

        old.suspendNextAddHarassmentEntry()
        let importTask = Task { await model.importLocalRulesToGateway() }
        await waitUntil(old.addEntrySuspendedConfirmed)

        // Re-pair while the first write is still in flight.
        model.harassment.reset()
        model.api = new
        model.testingBumpSessionGeneration()
        old.resumeAddHarassmentEntry()

        let outcome = await importTask.value
        XCTAssertTrue(outcome.aborted, "the import loop must abort on a pairing change")
        XCTAssertEqual(new.addEntryCalls, 0,
                       "no further payload may be attempted against the NEW key")
        XCTAssertTrue(new.harassmentEntryRecords.isEmpty,
                      "an OLD pairing payload reached the NEW key")
        // The suspended FIRST write may still land on the OLD key (harmless);
        // the loop must stop there instead of sending the remaining payloads.
        XCTAssertEqual(old.addEntryCalls, 1,
                       "the import loop must abort after the fenced iteration")
    }

    func testStaleToggleDoesNotMirrorAfterReset() async {
        let old = FakeGatewayAPI()
        old.harassmentSupported = true
        old.harassmentCall = false
        old.harassmentSMS = false

        let model = makeModel()
        model.api = old
        await model.refreshHarassmentPolicy()
        XCTAssertFalse(model.spamFilter.callBlockingEnabled)

        old.suspendNextHarassmentSettingsUpdate()
        let toggleTask = Task { await model.setHarassmentToggles(callBlocking: true, smsBlocking: false) }
        await waitUntil(old.updateSettingsSuspendedConfirmed)

        model.harassment.reset()
        model.testingBumpSessionGeneration()
        old.resumeHarassmentSettingsUpdate()
        await toggleTask.value

        XCTAssertFalse(model.spamFilter.callBlockingEnabled,
                       "a stale toggle result must not mirror into the new session")
        XCTAssertFalse(model.harassment.callBlocking)
    }

    func testStaleContextualBlockSetsNoNoticeOrMirror() async {
        let old = FakeGatewayAPI()
        old.harassmentSupported = true

        let model = makeModel()
        model.api = old
        await model.refreshHarassmentPolicy()

        // Suspend the FIRST await (channel toggle) and change the pairing
        // while it is in flight: the whole contextual action must abort with
        // no local mirror and no notice.
        old.suspendNextHarassmentSettingsUpdate()
        let blockTask = Task { await model.blockHarassmentNumber("+8613800000302", channel: "call") }
        await waitUntil(old.updateSettingsSuspendedConfirmed)

        model.harassment.reset()
        model.testingBumpSessionGeneration()
        old.resumeHarassmentSettingsUpdate()

        let blocked = await blockTask.value
        XCTAssertFalse(blocked)
        XCTAssertNil(model.harassment.actionNotice, "a stale failure must not show a notice")
        XCTAssertFalse(model.spamFilter.callBlockingEnabled,
                       "a stale contextual block must not enable the new session's channel")
        XCTAssertTrue(old.harassmentEntryRecords.isEmpty,
                      "the fenced action must not write an entry for the old session either")
    }
}

extension AppModelHarassmentFencingTests {
    /// The retained OLD session's onTogglesChanged callback (queued before a
    /// re-pair and executed after the NEW session refreshed) must be inert:
    /// it must not write the old preferences to the new key's gateway API.
    func testRetainedOldToggleCallbackNeverWritesToNewKey() async {
        let old = FakeGatewayAPI()
        old.harassmentSupported = true
        let new = FakeGatewayAPI()
        new.harassmentSupported = true

        let model = makeModel()
        model.api = old
        await model.refreshHarassmentPolicy()
        let retainedOldCallback = model.spamFilter.onTogglesChanged

        // Re-pair first, then fail the new refresh so support is fully
        // installed for the NEW session before the stale callback runs.
        model.harassment.reset()
        model.api = new
        model.testingBumpSessionGeneration()
        await model.refreshHarassmentPolicy()
        XCTAssertEqual(model.harassment.support, .supported)

        retainedOldCallback?(true, true)
        try? await Task.sleep(nanoseconds: 60_000_000)

        XCTAssertTrue(new.harassmentSettingsWrites.isEmpty,
                      "a retained old-session toggle callback wrote to the NEW key")
        XCTAssertFalse(new.harassmentCall)
        XCTAssertFalse(new.harassmentSMS)
    }
}
