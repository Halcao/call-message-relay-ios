import XCTest
@testable import CallRelay

/// Verifies the HTTP client hits the exact pinned gateway SMS paths and
/// sends/parses the handler shapes, using a loopback HTTPS-disabled test
/// server (no real network, no credentials leak).
@MainActor
final class SMSHTTPClientTests: XCTestCase {
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

    private func makeClient() throws -> HTTPGatewayAPI {
        guard case .success(let origin) = GatewayOrigin.validate(
            "http://127.0.0.1:\(server.port)", allowLoopbackHTTP: true
        ) else { throw NSError(domain: "test", code: 1) }
        let store = TokenStore(keychain: DictionaryKeychain())
        try store.save(TokenSet(accessToken: "a", refreshToken: "r", deviceId: "dev"))
        return HTTPGatewayAPI(origin: origin, tokens: store, configuration: server.configuration)
    }

    private func makeV2Client() throws -> HTTPGatewayAPI {
        guard case .success(let origin) = GatewayOrigin.validate(
            "http://127.0.0.1:\(server.port)", allowLoopbackHTTP: true, apiVersion: "v2"
        ) else { throw NSError(domain: "test", code: 1) }
        let store = TokenStore(keychain: DictionaryKeychain())
        try store.save(TokenSet(accessToken: "a", refreshToken: "r", deviceId: "dev"))
        return HTTPGatewayAPI(origin: origin, tokens: store, configuration: server.configuration)
    }

    /// Regression: the conversation key is "lineId:peer" and must reach the
    /// gateway EXACTLY once-encoded. Builds 25-27 pre-encoded it here, the
    /// URL builder escaped the `%` again and the gateway's split found no
    /// colon (CB-V2-400 "threadKey 格式错误") — every delete silently failed.
    func testDeleteThreadUsesRawQualifiedKeyExactlyOnce() async throws {
        server.respond(with: 200, body: #"{"key":"line1:+8613003132132","deleted":true}"#)
        let client = try makeV2Client()
        try await client.deleteThread(threadKey: "line1:+8613003132132")
        XCTAssertEqual(server.lastMethod, "DELETE")
        XCTAssertEqual(server.lastPath, "/api/v2/threads/line1:+8613003132132")
        XCTAssertNotNil(server.lastIdempotencyKey)
    }

    func testDeleteThreadRefusesOnV1Binding() async throws {
        let client = try makeClient()
        do {
            try await client.deleteThread(threadKey: "line1:555")
            XCTFail("v1 delete must not be attempted")
        } catch {
            // Expected: the v1 origin is not a unified gateway.
        }
        XCTAssertNil(server.lastMethod)
    }

    func testListThreadsUsesExactPathAndBareArray() async throws {
        server.respond(
            with: 200,
            body: #"[{"key":"555","peer":"555","unreadCount":1,"lastMessage":{"id":"m","threadKey":"555","direction":"inbound","peer":"555","body":"hi","status":"sent","createdAt":1}}]"#
        )
        let client = try makeClient()
        let threads = try await client.listThreads()
        XCTAssertEqual(threads.first?.key, "555")
        XCTAssertEqual(server.lastPath, "/api/v1/threads")
        XCTAssertEqual(server.lastAuthorization, "Bearer a")
    }

    func testInboxLoadFailureThenSuccessfulRetry() async throws {
        let client = try makeClient()
        let inbox = MessageInbox(api: client)
        server.respond(with: 503, body: "{}")
        let first = await inbox.refreshThreads()
        XCTAssertFalse(first)
        guard case .failed = inbox.listPhase else { return XCTFail("Expected recoverable load failure") }
        server.respond(with: 200, body: "[]")
        let retried = await inbox.refreshThreads()
        XCTAssertTrue(retried)
        XCTAssertEqual(inbox.listPhase, .loaded)
        XCTAssertTrue(inbox.displayThreads.isEmpty)
    }

    func testSendMessagePathHeadersAndCreatedResponse() async throws {
        server.respond(
            with: 201,
            body: #"{"id":"msg_2","threadKey":"555","direction":"outbound","peer":"555","body":"hi","encoding":"gsm7","status":"queued","createdAt":12}"#
        )
        let client = try makeClient()
        let message = try await client.sendMessage(to: "555", body: "hi", idempotencyKey: "key-1")
        XCTAssertEqual(message.id, "msg_2")
        XCTAssertEqual(message.status, .queued)
        XCTAssertEqual(server.lastPath, "/api/v1/messages")
        XCTAssertEqual(server.lastMethod, "POST")
        XCTAssertEqual(server.lastIdempotencyKey, "key-1")
        let parsedBody = try JSONSerialization.jsonObject(
            with: Data((server.lastBody ?? "").utf8)
        ) as? [String: String]
        XCTAssertEqual(parsedBody?["to"], "555")
        XCTAssertEqual(parsedBody?["body"], "hi")
    }

    func test502WithFailedMessageIsReturnedNotThrown() async throws {
        server.respond(
            with: 502,
            body: #"{"id":"msg_3","threadKey":"555","direction":"outbound","peer":"555","body":"hi","status":"failed","createdAt":13}"#
        )
        let client = try makeClient()
        let message = try await client.sendMessage(to: "555", body: "hi", idempotencyKey: "key-2")
        XCTAssertEqual(message.status, .failed)
        XCTAssertEqual(message.id, "msg_3")
    }

    func testThreadMessagesParseHasMoreHeaderAndChronologicalOrder() async throws {
        server.respond(
            with: 200,
            body: #"[{"id":"old","threadKey":"k","direction":"inbound","peer":"p","body":"a","status":"sent","createdAt":1},{"id":"new","threadKey":"k","direction":"inbound","peer":"p","body":"b","status":"sent","createdAt":2}]"#,
            headers: ["X-CellBridge-Has-More": "true"]
        )
        let client = try makeClient()
        let page = try await client.listThreadMessages(
            threadKey: "k", beforeCreatedAt: nil, beforeID: nil, limit: 50
        )
        XCTAssertTrue(page.hasMore)
        XCTAssertEqual(page.messages.map(\.id), ["old", "new"])
        let query = try XCTUnwrap(server.lastQuery)
        XCTAssertTrue(query.contains("threadKey=k"))
        XCTAssertTrue(query.contains("limit=50"))
    }

    func testReadReceiptPostsExactActionPath() async throws {
        server.respond(with: 204, body: "")
        let client = try makeClient()
        try await client.markMessageRead(id: "msg_9", idempotencyKey: "rk")
        XCTAssertEqual(server.lastMethod, "POST")
        XCTAssertEqual(server.lastPath, "/api/v1/messages/msg_9/read")
        XCTAssertEqual(server.lastIdempotencyKey, "rk")
    }

    // MARK: Recording device preference wire path (build-67)
    //
    // The recording switch must not fake success: the exact PUT path/method/
    // body/auth are asserted, and the gateway's echoed preference decides
    // whether the write succeeded. Live evidence (2026-10-16) showed the
    // gateway had never persisted a recording pref while the same device's
    // denoise pref persisted, so a silently-ignored field must surface as a
    // failure instead of an ON state the server never accepted.

    func testDeviceAutoRecordSendsExactPathMethodBodyAndAuth() async throws {
        server.respond(with: 200, body: #"{"defaultLineId":"line1","uplinkDenoise":false,"downlinkDenoise":false,"autoRecord":true}"#)
        let client = try makeV2Client()
        let stored = try await client.updateDeviceAutoRecord(true)
        XCTAssertTrue(stored)
        XCTAssertEqual(server.lastMethod, "PUT")
        XCTAssertEqual(server.lastPath, "/api/v2/devices/dev/preferences")
        XCTAssertEqual(server.lastAuthorization, "Bearer a")
        XCTAssertNotNil(server.lastIdempotencyKey)
        let body = try XCTUnwrap(server.lastBody).replacingOccurrences(of: " ", with: "")
        XCTAssertTrue(body.contains(#""autoRecord":true"#),
                      "the device preference body must carry autoRecord=true (got \(body))")
    }

    func testDeviceAutoRecordFalseSendsFalseAndReturnsEcho() async throws {
        server.respond(with: 200, body: #"{"autoRecord":false}"#)
        let client = try makeV2Client()
        let stored = try await client.updateDeviceAutoRecord(false)
        XCTAssertFalse(stored)
        let body = try XCTUnwrap(server.lastBody).replacingOccurrences(of: " ", with: "")
        XCTAssertTrue(body.contains(#""autoRecord":false"#))
    }

    /// A 2xx that stores the OPPOSITE value must throw, never fake success.
    func testEchoedOppositePreferenceIsReportedAsNotSaved() async throws {
        server.respond(with: 200, body: #"{"autoRecord":false}"#)
        let client = try makeV2Client()
        do {
            _ = try await client.updateDeviceAutoRecord(true)
            XCTFail("an unconfirmed echo must not report success")
        } catch let error as APIError {
            guard case .http(let status, let code, let message) = error else {
                return XCTFail("expected a structured http failure, got \(error)")
            }
            XCTAssertEqual(status, 200)
            XCTAssertEqual(code, "CB-CLIENT-NOT-SAVED")
            XCTAssertTrue((message ?? "").contains("未保存"))
        }
    }

    /// A 2xx WITHOUT the echo (an older gateway ignoring the field) must
    /// throw: this is the exact "switch bounces back later" class.
    func testMissingEchoFieldIsReportedAsNotSaved() async throws {
        server.respond(with: 200, body: #"{"defaultLineId":"line1","uplinkDenoise":false,"downlinkDenoise":false}"#)
        let client = try makeV2Client()
        do {
            _ = try await client.updateDeviceAutoRecord(true)
            XCTFail("a response without the stored preference cannot confirm the write")
        } catch let error as APIError {
            guard case .http(_, let code, _) = error else {
                return XCTFail("expected a structured http failure, got \(error)")
            }
            XCTAssertEqual(code, "CB-CLIENT-NOT-SAVED")
        }
    }

    /// A non-2xx (e.g. a rejected device binding) keeps its own status/code.
    func testNon2xxPreferenceWriteKeepsTheGatewayStatusAndCode() async throws {
        server.respond(with: 403, body: #"{"code":"CB-V2-403","message":"forbidden"}"#)
        let client = try makeV2Client()
        do {
            _ = try await client.updateDeviceAutoRecord(true)
            XCTFail("a rejected write must throw")
        } catch let error as APIError {
            guard case .http(let status, let code, _) = error else {
                return XCTFail("expected a structured http failure, got \(error)")
            }
            XCTAssertEqual(status, 403)
            XCTAssertEqual(code, "CB-V2-403")
        }
    }

    /// The V2 endpoint always returns the stored view, so an empty/204 body
    /// is UNCONFIRMED and must be reported as not saved (never a fake ON).
    func testEmptyPreferenceEchoBodyIsUnconfirmedNotSaved() async throws {
        server.respond(with: 204, body: "")
        let client = try makeV2Client()
        do {
            _ = try await client.updateDeviceAutoRecord(true)
            XCTFail("an empty 2xx body cannot confirm the stored preference")
        } catch let error as APIError {
            guard case .http(_, let code, _) = error else {
                return XCTFail("expected a structured http failure, got \(error)")
            }
            XCTAssertEqual(code, "CB-CLIENT-NOT-SAVED")
        }
    }
}
