import XCTest
@testable import CallRelay

/// Wire-level and app-level proofs for the durable history package:
/// persisted validators across client re-creation ("process restart"),
/// transferred-body-byte measurements, the pre-identity-verification gate,
/// offline cold start rendering, recents stale/empty-304 cases and the
/// SMS-alert tap routing. Fictional 张三/李四 fixtures only.
@MainActor
final class HistoryTransportAndStartTests: XCTestCase {

    private var server: TestHTTPServer!
    override func setUp() async throws {
        try await super.setUp()
        server = TestHTTPServer()
        try server.start()
    }
    override func tearDown() async throws {
        await server.stop()
        server = nil
        try await super.tearDown()
    }

    private static let threadJSON = """
    [{"key":"line1:+8613800000001","peer":"+8613800000001","unreadCount":2,
      "lastMessage":{"id":"t1","threadKey":"line1:+8613800000001","direction":"inbound",
      "peer":"+8613800000001","body":"张三你好","status":"sent","createdAt":5000}}]
    """
    private static var threadsURLString: String { "http://127.0.0.1:8080/api/v2/threads" }

    private func makeOrigin() -> GatewayOrigin {
        guard case .success(let origin) = GatewayOrigin.validate(
            "http://127.0.0.1:\(server.port)", allowLoopbackHTTP: true, apiVersion: "v2")
        else { fatalError("loopback origin") }
        return origin
    }
    private func makeStore(_ tag: String) -> GatewayHistoryStore {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("hist-\(tag)-\(UUID().uuidString).json")
        return GatewayHistoryStore(scopeIdentifier: nil, explicitURL: url)!
    }
    private func makeClient(store: GatewayHistoryStore?) throws -> HTTPGatewayAPI {
        let tokens = TokenStore(keychain: DictionaryKeychain())
        try tokens.save(TokenSet(accessToken: "access", refreshToken: "refresh", deviceId: "dev-1"))
        return HTTPGatewayAPI(origin: makeOrigin(), tokens: tokens,
                              configuration: server.configuration, historyStore: store)
    }

    // MARK: Persisted validator across restart + zero-byte 304

    func testPersistedValidatorSurvivesClientRecreationAnd304TransfersZeroBytes() async throws {
        let store = makeStore("restart")
        let client1 = try makeClient(store: store)
        server.respond(with: 200, body: Self.threadJSON, headers: ["ETag": "\"W1\""])
        let first = try await client1.listThreadsConditional(folder: nil, lineId: nil)
        guard case .changed(let threads, let validator) = first, let validator else {
            return XCTFail("first fetch must be a full body with a validator")
        }
        XCTAssertEqual(threads.count, 1)
        XCTAssertEqual(validator.etag, "\"W1\"")
        // Coherent commit: body + the validator that guards it, together.
        store.recordThreads(threads: threads, junk: nil, hasMoreKeys: nil,
                            validatorUpdates: [validator.key: validator.etag],
                            listSurfaceTag: "line:all", savedAt: Date())
        store.flush()

        // "Process restart": brand-new transport over the same durable scope.
        let client2 = try makeClient(store: GatewayHistoryStore(
            scopeIdentifier: nil,
            explicitURL: store.fileURL)! )
        let bytesBefore = server.totalTransferredBodyBytes
        server.respond(with: 304, body: "")
        let second = try await client2.listThreadsConditional(folder: nil, lineId: nil)
        guard case .notModified = second else {
            return XCTFail("the persisted validator must survive recreation and revalidate")
        }
        XCTAssertEqual(server.lastIfNoneMatch, "\"W1\"",
                       "the NEW client instance re-sent the PERSISTED ETag")
        XCTAssertEqual(server.lastCachePolicy, .reloadIgnoringLocalCacheData,
                       "the transport never lets URLSession replay its own cached 200")
        XCTAssertEqual(server.totalTransferredBodyBytes, bytesBefore,
                       "a 304 revalidation transfers zero body bytes")
    }

    func testOldGatewayWithoutEtagHeaderNeverRevalidates() async throws {
        let store = makeStore("oldgw")
        let client = try makeClient(store: store)
        server.respond(with: 200, body: Self.threadJSON)
        let first = try await client.listThreadsConditional(folder: nil, lineId: nil)
        guard case .changed(_, let validator) = first else { return XCTFail("200 expected") }
        XCTAssertEqual(validator?.etag, nil, "no ETag advertised => no usable validator")
        server.respond(with: 200, body: Self.threadJSON)
        _ = try await client.listThreadsConditional(folder: nil, lineId: nil)
        XCTAssertNil(server.lastIfNoneMatch, "a client without a validator never sends If-None-Match")
        XCTAssertTrue(store.validatorsForTest.isEmpty)
    }

    func testAdminSurfaceNeverCarriesValidators() async throws {
        // Defense-in-depth: the token/ICE/admin paths are not the conditional
        // surface; they must never send If-None-Match even with a populated
        // validator store.
        let store = makeStore("allowlist")
        store.recordThreads(threads: [], junk: nil, hasMoreKeys: nil,
                            validatorUpdates: [Self.threadsURLString: "\"W9\""],
                            savedAt: Date())
        store.flush()
        let client = try makeClient(store: store)
        server.respond(with: 200, body: #"{"status":"ok","version":"2","push":{"configured":true}}"#)
        _ = try await client.gatewayHealth()
        XCTAssertNil(server.lastIfNoneMatch, "health/admin GETs are never conditioned")
    }

    // MARK: Pre-verification gate (hydrate-before-network safety)

    func testIdentityGateBlocksEveryCredentialedPathUntilVerified() async throws {
        let store = makeStore("gate")
        let client = try makeClient(store: store)
        client.requireIdentityVerification()
        server.respond(with: 200, body: Self.threadJSON, headers: ["ETag": "\"G1\""])
        do {
            _ = try await client.listThreadsConditional(folder: nil, lineId: nil)
            return XCTFail("gated session must not fetch")
        } catch APIError.notReady {
            // expected, before the socket was ever used
        }
        XCTAssertEqual(server.requestCount, 0,
                       "the gated client put NOTHING on the wire — no bearer leak pre-verify")
        server.respond(with: 200, body: Self.threadJSON, headers: ["ETag": "\"G1\""])
        client.markIdentityVerified()
        _ = try await client.listThreadsConditional(folder: nil, lineId: nil)
        XCTAssertEqual(server.requestCount, 1, "verification opens the live path")
    }

    // MARK: Offline cold start (addendum #1) — real session, dead gateway

    func testOfflineColdStartRendersHydratedHistoryBeforeAndWithoutNetwork() async throws {
        // Previously-verified stored binding + durable snapshot, no network.
        let bindingURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("bindings-\(UUID().uuidString).json")
        let bindings = BindingStore(storeURL: bindingURL)
        try bindings.save(GatewayBinding(
            gatewayId: "hist-gw", gatewayName: nil, endpoint: "https://127.0.0.1:9",
            fingerprint: "sha256:fake", transport: "unified", pairedAt: Date(),
            allowLoopbackHTTP: false, apiVersion: "v2", defaultLineId: nil))
        let tokens = TokenStore(keychain: DictionaryKeychain())
        try tokens.save(TokenSet(accessToken: "a", refreshToken: "r", deviceId: "hist-dev"))
        guard case .success(let origin) = GatewayOrigin.validate(
            "https://127.0.0.1:9", apiVersion: "v2") else { return XCTFail("origin") }
        let scope = HistoryScope.identifier(origin: origin, gatewayID: "hist-gw", deviceID: "hist-dev")!
        let store = GatewayHistoryStore(scopeIdentifier: scope)!
        defer { store.purge() }

        let record = MessageRecord(id: "hh1", gatewayID: "hist-gw", lineID: "line1",
                                   threadKey: "line1:+8613800000001", direction: .inbound,
                                   peer: "+8613800000001", body: "张三：明早八点取货",
                                   encoding: nil, status: .sent, createdAt: 8_000)
        let row = MessageThread(key: "line1:+8613800000001", peer: "+8613800000001",
                                unreadCount: 1, lastMessage: record)
        let call = CallRecord(id: "hc1", gatewayID: "hist-gw", lineID: "line1",
                              direction: .outbound, peer: "+8613800000002", state: .idle,
                              startedAt: 7_000, connectedAt: 7_100, endedAt: 7_900,
                              endReason: nil, recordingId: nil, recordingState: nil,
                              recordingDurationMs: nil)
        store.recordThreads(threads: [row], junk: nil, hasMoreKeys: nil,
                            validatorUpdates: [:], savedAt: Date())
        store.recordMessages([record], for: row.key, validatorUpdates: [:], savedAt: Date())
        store.recordCalls([call], validatorUpdates: [:], savedAt: Date())
        store.flush()
        let settledSnapshot = GatewayHistoryStore(scopeIdentifier: nil, explicitURL: store.fileURL)?
            .snapshotForHydrate()

        let model = AppModel(identities: IdentityStore(keychain: DictionaryKeychain()),
                             tokenStore: tokens, bindingStore: bindings,
                             defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.bootstrap()
        // HYDRATE IS SYNCHRONOUS and precedes any network completion: the
        // display state exists while the identity probe is still in flight.
        XCTAssertNotNil(model.inbox, "the hydrated session exists before the network verdict")
        XCTAssertEqual(model.inbox?.displayThreads.map(\.key), [row.key])
        inboxOpenCheck(model, row, record)
        XCTAssertEqual(model.recents.map(\.id), ["hc1"],
                       "Recents render from disk on an offline cold start")

        // The probe against the dead host settles offline; display RETAINS,
        // the durable file is untouched (hydrate is read-only), and a normal
        // reconnect attempt (retryConnection = teardown+restart) preserves
        // the snapshot: teardown is not unpair.
        await waitUntil(timeout: 10) {
            if case .offline = model.linePhase { return true }
            return false
        }
        if case .offline = model.linePhase {} else { return XCTFail("offline verdict expected") }
        XCTAssertEqual(model.inbox?.displayThreads.map(\.key), [row.key],
                       "offline state keeps the hydrated list")
        model.retryConnection()
        XCTAssertNotNil(model.inbox)
        XCTAssertEqual(model.inbox?.displayThreads.map(\.key), [row.key],
                       "ordinary teardown/reconnect preserves the private snapshot")
        // Hydrate/teardown may re-settle the SAME snapshot; what must not
        // change is its logical content (never a mask, purge or loss).
        let afterRetry = GatewayHistoryStore(scopeIdentifier: nil, explicitURL: store.fileURL)?
            .snapshotForHydrate()
        XCTAssertEqual(afterRetry?.threads, settledSnapshot?.threads,
                       "the settled durable body is unchanged across offline restart")
        XCTAssertEqual(afterRetry?.calls, settledSnapshot?.calls)

        // Unpair purges the scope; the next cold start has nothing.
        model.unpair()
        XCTAssertFalse(FileManager.default.fileExists(atPath: store.fileURL.path),
                       "unpair purges the scoped durable history")
        XCTAssertTrue(model.recents.isEmpty)
        XCTAssertEqual(model.inbox?.displayThreads.count, nil) // nil before verify re-hydrates
        model.teardownForTest()
    }

    private func inboxOpenCheck(_ model: AppModel, _ row: MessageThread,
                                _ record: MessageRecord) {
        guard let inbox = model.inbox else { return XCTFail("hydrated inbox") }
        inbox.openThread(row.key)
        XCTAssertEqual(inbox.openPhase(for: row.key), .loaded, "loaded bodies render instantly")
        XCTAssertEqual(inbox.rows(for: row.key).map(\.id), [record.id])
    }

    // MARK: Recents stale/empty cases (root review)

    func testStaleRecentsResponseAfterRebindNeverMutatesNewSession() async {
        let api = FakeGatewayAPI()
        let model = configuredModel(api)
        api.armConditionalCallsWait()
        let task = Task { await model.refreshRecentsPublic() }
        await waitUntil { api.conditionalCallsParked }
        // Rebind (unpair/switch gateway): session generation bumps.
        model.testingBumpSessionGeneration()
        let newer = CallRecord(id: "c-new", gatewayID: "gw", lineID: nil, direction: .inbound,
                               peer: "+8613800000002", state: .idle, startedAt: 9_000,
                               connectedAt: nil, endedAt: 9_500, endReason: nil,
                               recordingId: nil, recordingState: nil, recordingDurationMs: nil)
        api.resumeConditionalCalls(with: .success(.changed([newer], validator: nil)))
        let settled = await task.value
        XCTAssertFalse(settled, "the superseded response is fenced out")
        XCTAssertTrue(model.recents.isEmpty,
                      "an old pairing's in-flight list may not mutate the new session UI")
    }

    func testLegitimateEmptyCallHistory304DoesNotForceFullRefetch() async {
        let api = FakeGatewayAPI()
        api.callsStub = [] // the gateway has genuinely zero calls
        let store = GatewayHistoryStore(
            scopeIdentifier: nil,
            explicitURL: FileManager.default.temporaryDirectory
                .appendingPathComponent("empty304-\(UUID().uuidString).json"))!
        defer { store.purge() }
        let model = configuredModel(api)
        model.setHistoryStoreForTesting(store)
        api.validatorReader = { [weak store] key in store?.validator(for: key) }

        _ = await model.refreshRecentsPublic()
        XCTAssertTrue(model.testingCallsSnapshotSettled)
        XCTAssertEqual(api.listCallsFullBodyCount, 1, "one settled fetch of the empty truth")
        _ = await model.refreshRecentsPublic()
        XCTAssertEqual(api.listCallsFullBodyCount, 1,
                       "a valid empty snapshot + matching ETag stays at 304 — no refetch storm")
        XCTAssertEqual(api.listCalls304Count, 1)
        XCTAssertTrue(model.recents.isEmpty)
    }

    // MARK: Gap revalidation wiring (coordinator)

    func testGapRevalidationRevalidatesOpenPageAndStaleMarksCachedPages() async {
        let api = FakeGatewayAPI()
        api.threads = [historyThread]
        api.threadPages[historyThread.key] = [historyMessage]
        let store = GatewayHistoryStore(
            scopeIdentifier: nil,
            explicitURL: FileManager.default.temporaryDirectory
                .appendingPathComponent("gap-\(UUID().uuidString).json"))!
        defer { store.purge() }
        api.validatorReader = { [weak store] key in store?.validator(for: key) }
        let inbox = MessageInbox(api: api, filter: nil,
                                 tombstoneDefaults: UserDefaults(suiteName: UUID().uuidString)!,
                                 historyStore: store)
        _ = await inbox.refreshThreads()
        inbox.openThread(historyThread.key)
        await waitUntil { inbox.rows(for: historyThread.key).count == 1 }
        let fullAfterOpen = api.pageFetchFullBodyCount

        let model = configuredModel(api)
        model.injectInboxForTesting(inbox)
        // Simulate stream (re)open after a gap: bounded, immediate revalidate
        // of the OPEN conversation — not waiting for the 20 s poll.
        api.threadPages[historyThread.key] = [
            historyMessage,
            MessageRecord(id: "gap-new", gatewayID: "gw", lineID: "line1",
                          threadKey: historyThread.key, direction: .inbound,
                          peer: historyThread.peer, body: "李四：路上", encoding: nil,
                          status: .sent, createdAt: 9_999),
        ]
        await model.testingReconcileAfterGap()
        XCTAssertGreaterThan(api.pageFetchFullBodyCount, fullAfterOpen,
                             "gap path revalidated the open page immediately")
        XCTAssertTrue(inbox.rows(for: historyThread.key).contains { $0.id == "gap-new" })
        XCTAssertTrue(inbox.loadedPageKeysForTest.isEmpty,
                      "validated pages leave the stale set")
        model.teardownForTest()
    }

    // MARK: SMS alert routing (AppModel)

    private func smsUserInfo(gatewayId: String, threadKey: String = "line1:+8613800000001",
                             preview: String = "张三：明早…") -> [AnyHashable: Any] {
        [
            "aps": ["alert": ["title": "张三", "body": preview],
                    "sound": "default", "category": "CALLRELAY_SMS",
                    "thread-id": "sms-0123456789abcdef"],
            "messageId": "line1:1390",
            "gatewayId": gatewayId,
            "lineId": "line1",
            "threadKey": threadKey,
            "peer": "张三",
            "preview": preview,
            "issuedAt": 1_790_936_681,
        ]
    }
    private func makeBoundModel(gatewayId: String) -> (AppModel, BindingStore) {
        let bindingURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("b-\(UUID().uuidString).json")
        let bindings = BindingStore(storeURL: bindingURL)
        try? bindings.save(GatewayBinding(
            gatewayId: gatewayId, gatewayName: nil, endpoint: "https://tap.invalid",
            fingerprint: "sha256:x", transport: "unified", pairedAt: Date(),
            allowLoopbackHTTP: false, apiVersion: "v2", defaultLineId: nil))
        let model = AppModel(identities: IdentityStore(keychain: DictionaryKeychain()),
                             tokenStore: TokenStore(keychain: DictionaryKeychain()),
                             bindingStore: bindings,
                             defaults: UserDefaults(suiteName: UUID().uuidString)!)
        return (model, bindings)
    }

    func testSMSTapRoutesToExactConversationOnlyForPairedGateway() {
        let (model, _) = makeBoundModel(gatewayId: "gw-tap")
        model.selectedTab = .keypad
        model.handleServiceNotification(userInfo: smsUserInfo(gatewayId: "gw-tap"))
        XCTAssertEqual(model.selectedTab, .messages)
        XCTAssertEqual(model.pendingSMSConversationKey, "line1:+8613800000001")
        XCTAssertEqual(model.consumePendingSMSConversation(), "line1:+8613800000001")
        XCTAssertNil(model.consumePendingSMSConversation(), "one-shot consumption")

        // Foreign gateway identity: no jump, no tab steal.
        model.selectedTab = .keypad
        model.handleServiceNotification(userInfo: smsUserInfo(gatewayId: "gw-EVIL"))
        XCTAssertEqual(model.selectedTab, .keypad)
        XCTAssertNil(model.pendingSMSConversationKey)

        // Service alert keeps its exclusive Settings routing.
        model.handleServiceNotification(userInfo: [
            "kind": "line_alert", "alert": "arrears", "lineId": "line1",
        ])
        XCTAssertEqual(model.selectedTab, .settings)
        XCTAssertNotNil(model.pendingServiceAlert)
        XCTAssertNil(model.pendingSMSConversationKey)
        model.teardownForTest()
    }

    func testTruncatedPreviewIsPresentationOnlyAndNeverStoredAsAMessage() async {
        let (model, _) = makeBoundModel(gatewayId: "gw-prev")
        let fullBody = "张三：明天上午九点整，社区活动中心三楼彩排，请勿迟到，带齐全部乐谱和琴弓。"
        let preview = String(fullBody.prefix(12)) + "…"
        let store = GatewayHistoryStore(
            scopeIdentifier: nil,
            explicitURL: FileManager.default.temporaryDirectory
                .appendingPathComponent("prev-\(UUID().uuidString).json"))!
        defer { store.purge() }
        let record = MessageRecord(id: "line1:1390", gatewayID: "gw-prev", lineID: "line1",
                                   threadKey: "line1:+8613800000001", direction: .inbound,
                                   peer: "+8613800000001", body: fullBody, encoding: nil,
                                   status: .sent, createdAt: 1_790_936_000)
        store.recordMessages([record], for: record.threadKey, validatorUpdates: [:], savedAt: Date())
        store.flush()

        model.handleServiceNotification(userInfo: smsUserInfo(gatewayId: "gw-prev", preview: preview))
        XCTAssertEqual(model.pendingSMSConversationKey, record.threadKey,
                       "tap routing uses the COMPLETE threadKey, not the preview")

        // The durable store still holds the full body; the truncated preview
        // never entered it (the route type does not even carry body fields).
        let data = try! Data(contentsOf: store.fileURL)
        let text = String(data: data, encoding: .utf8) ?? ""
        XCTAssertTrue(text.contains(fullBody), "full SMS body survives")
        XCTAssertFalse(text.contains(preview), "the truncated push preview is never persisted")

        // An offline inbox hydrates the FULL body for display.
        let inbox = MessageInbox(api: UnreachableAPI2(), filter: nil,
                                 tombstoneDefaults: UserDefaults(suiteName: UUID().uuidString)!,
                                 historyStore: store)
        inbox.openThread(record.threadKey)
        XCTAssertEqual(inbox.rows(for: record.threadKey).first?.body, fullBody)
        model.teardownForTest()
    }

    // MARK: Helpers

    private var historyThread: MessageThread {
        MessageThread(key: "line1:+8613800000001", peer: "+8613800000001", unreadCount: 0,
                      lastMessage: historyMessage)
    }
    private var historyMessage: MessageRecord {
        MessageRecord(id: "g1", gatewayID: "gw", lineID: "line1",
                      threadKey: "line1:+8613800000001", direction: .inbound,
                      peer: "+8613800000001", body: "张三：到了", encoding: nil,
                      status: .read, createdAt: 5_000)
    }

    private func configuredModel(_ api: FakeGatewayAPI) -> AppModel {
        let bindingURL = FileManager.default.temporaryDirectory
            .appendingPathComponent("cfg-\(UUID().uuidString).json")
        let model = AppModel(
            identities: IdentityStore(keychain: DictionaryKeychain()),
            tokenStore: TokenStore(keychain: DictionaryKeychain()),
            bindingStore: BindingStore(storeURL: bindingURL),
            defaults: UserDefaults(suiteName: UUID().uuidString)!)
        model.configureForTesting(api: api, driver: SilentDriver(), lines: [], defaultLineId: nil)
        return model
    }
}

/// The dead-host offline test leaves the gated session dangling until the
/// probe settles; this seam tears it down deterministically.
extension AppModel {
    func teardownForTest() {
        unpair()
    }
}

@MainActor
final class UnreachableAPI2: FakeGatewayAPI {
    override func listThreadMessages(threadKey: String, beforeCreatedAt: Int64?,
                                     beforeID: String?, limit: Int) async throws -> ThreadMessagePage {
        try Self.reject()
    }
    override func listThreadMessagesConditional(threadKey: String, beforeCreatedAt: Int64?,
                                                beforeID: String?, limit: Int) async throws
        -> ConditionalOutcome<ThreadMessagePage> {
        try Self.reject()
    }
    private static func reject() throws -> Never {
        throw APIError.network(URLError(.cannotConnectToHost))
    }
}

/// Minimal CallDriver stub for configureForTesting sessions.
final class SilentDriver: CallDriver {
    var onUpdate: ((ActiveCallViewState?) -> Void)?
    var onQuality: ((MediaQuality) -> Void)?
    var onEnded: ((String) -> Void)?
    func dial(peer: String, lineId: String?) {}
    func dial(peer: String) {}
    func reportIncomingPush(gatewayId: String, uuid: UUID, handle: String, record: CallRecord?) async {}
    func reportIncomingFromEvent(_ call: CallRecord) async {}
    func answerCurrent() {}
    func endCall(gatewayId: String) async {}
    func hangup() {}
    func playDTMF(_ digit: String) {}
    func setMuted(_ muted: Bool) {}
    func setSpeaker(_ enabled: Bool) {}
    func reset() {}
}
