import XCTest
@testable import CallRelay

/// Contextual 拦截此号码 must actually block the originating channel even
/// when both master switches are at their default OFF, must preserve the
/// other channel's preference, and must never report a failed write as
/// success.
@MainActor
final class HarassmentContextualBlockTests: XCTestCase {
    private func makeModel(_ fake: FakeGatewayAPI) -> AppModel {
        let model = AppModel(
            defaults: UserDefaults(suiteName: "contextual-\(UUID().uuidString)")!,
            spamFilter: SpamFilterStore(storeURL: FileManager.default.temporaryDirectory
                .appendingPathComponent("contextual-\(UUID().uuidString).json")))
        model.api = fake
        return model
    }

    func testFreshDefaultOffContextualCallBlockEnablesCallChannel() async {
        let fake = FakeGatewayAPI()
        fake.harassmentSupported = true
        let model = makeModel(fake)
        await model.refreshHarassmentPolicy()
        XCTAssertEqual(model.harassment.support, .supported)
        XCTAssertFalse(model.spamFilter.callBlockingEnabled)
        XCTAssertFalse(model.spamFilter.smsBlockingEnabled)

        let blocked = await model.blockHarassmentNumber("+8613800000041", channel: "call")
        XCTAssertTrue(blocked)
        XCTAssertEqual(fake.harassmentReplaceFlags, [true],
                       "contextual block must replace an opposite exact trust atomically")
        XCTAssertTrue(fake.harassmentCall, "contextual call block must enable the call channel")
        XCTAssertFalse(fake.harassmentSMS, "the other channel preference must be preserved")
        XCTAssertTrue(fake.harassmentEntryRecords.contains { $0.number == "+8613800000041" && $0.mode == "block" })
        // The local layer mirrors the shared key policy (the gateway itself
        // enforces the number; the local rule is only needed on the fallback
        // path, so here we assert the switch mirrors the shared state).
        XCTAssertTrue(model.spamFilter.callBlockingEnabled)
    }

    func testContextualSMSBlockEnablesSMSChannelOnly() async {
        let fake = FakeGatewayAPI()
        fake.harassmentSupported = true
        fake.harassmentCall = true // pre-existing other-channel preference
        let model = makeModel(fake)
        await model.refreshHarassmentPolicy()

        let blocked = await model.blockHarassmentNumber("+8613800000042", channel: "sms")
        XCTAssertTrue(blocked)
        XCTAssertTrue(fake.harassmentSMS, "contextual SMS block must enable the SMS channel")
        XCTAssertTrue(fake.harassmentCall, "the call channel preference must be preserved")
        XCTAssertTrue(model.spamFilter.smsBlockingEnabled)
    }

    func testFailedGatewayWriteIsNotReportedAsSuccess() async {
        let fake = FakeGatewayAPI()
        fake.harassmentSupported = true
        fake.harassmentAddError = APIError.http(status: 403, code: "CB-V2-403", message: "已撤销")
        let model = makeModel(fake)
        await model.refreshHarassmentPolicy()

        let blocked = await model.blockHarassmentNumber("+8613800000043", channel: "call")
        XCTAssertFalse(blocked, "a revoked/forbidden write must not be reported as blocked")
        XCTAssertNotNil(model.harassment.actionNotice, "the failure must surface a visible notice")
    }

    func testVersionConflictIsNotReportedAsSuccess() async {
        let fake = FakeGatewayAPI()
        fake.harassmentSupported = true
        let model = makeModel(fake)
        await model.refreshHarassmentPolicy()
        // Another same-key device writes first: our loaded version is stale.
        fake.harassmentVersionValue += 1

        let blocked = await model.blockHarassmentNumber("+8613800000044", channel: "call")
        XCTAssertFalse(blocked)
        XCTAssertNotNil(model.harassment.actionNotice)
    }

    func testLegacyLocalFallbackBlocksOriginatingChannel() async {
        let fake = FakeGatewayAPI() // harassmentSupported stays false
        let model = makeModel(fake)
        await model.refreshHarassmentPolicy()
        XCTAssertEqual(model.harassment.support, .unavailable)

        // A pre-existing local trust for the SAME canonical number must be
        // replaced by the contextual block (legacy fallback parity).
        model.spamFilter.addRule(kind: .whitelistSender, value: "+8613800000045", label: "信任")
        model.spamFilter.markSenderKnown("+8613800000045")

        let blocked = await model.blockHarassmentNumber("+8613800000045", channel: "call")
        XCTAssertTrue(blocked)
        XCTAssertTrue(model.spamFilter.callBlockingEnabled, "legacy fallback must enable the call channel")
        XCTAssertFalse(model.spamFilter.smsBlockingEnabled, "the SMS preference must stay untouched")
        XCTAssertFalse(model.spamFilter.rules.contains { $0.kind == .whitelistSender },
                       "the exact trust rule must be replaced")
        XCTAssertFalse(model.spamFilter.isKnownSender("13800000045"))
        if case .reject = model.spamFilter.policy().screenCall(peer: "13800000045", listHits: []) {
            // expected
        } else {
            XCTFail("legacy contextual block did not take effect")
        }
    }

    func testTrustThenBlockReplacesTrustEntryAndReportsBroaderTrust() async {
        let fake = FakeGatewayAPI()
        fake.harassmentSupported = true
        let model = makeModel(fake)
        await model.refreshHarassmentPolicy()

        // Trust first, then the contextual block must remove the trust entry.
        let trusted = await model.trustHarassmentNumber("+8613800000046")
        XCTAssertTrue(trusted)
        XCTAssertTrue(fake.harassmentEntryRecords.contains { $0.mode == "trust" })
        let blocked = await model.blockHarassmentNumber("+8613800000046", channel: "call")
        XCTAssertTrue(blocked)
        XCTAssertFalse(fake.harassmentEntryRecords.contains { $0.mode == "trust" },
                       "opposite exact trust must be replaced")
        XCTAssertTrue(fake.harassmentEntryRecords.contains { $0.mode == "block" })

        // A broader trust rule (fake reports ineffective): never success.
        fake.harassmentAddEffective = false
        fake.harassmentAddNote = "已有更宽的信任规则（138），该号码仍会放行。"
        let ineffective = await model.blockHarassmentNumber("+8613800000047", channel: "call")
        XCTAssertFalse(ineffective, "an ineffective block must not be reported as success")
        XCTAssertEqual(model.harassment.actionNotice, fake.harassmentAddNote)
    }

    func testBulkImportNeverReplacesOppositeRules() async {
        let fake = FakeGatewayAPI()
        fake.harassmentSupported = true
        let model = makeModel(fake)
        await model.refreshHarassmentPolicy()
        model.spamFilter.addRule(kind: .senderExact, value: "+8613800000048", label: "拦截")
        // Perform an explicit import (local rules -> gateway).
        let outcome = await model.importLocalRulesToGateway()
        XCTAssertGreaterThan(outcome.imported, 0)
        XCTAssertEqual(fake.harassmentReplaceFlags, Array(repeating: false, count: outcome.imported),
                       "bulk imports must preserve the configured precedence")
    }
}
