import XCTest
@testable import CallRelay

/// Deterministic, socket-free stub for the administrator API. Responses are
/// routed by PATH (the console triggers several concurrent section loads), and
/// any route can be "gated": its response is delivered only when the test
/// fires the semaphore, which makes "the answer arrives after the session is
/// gone" races reproducible without sleeps or timing luck.
final class AdminTestProtocol: URLProtocol {
    struct Route {
        var status: Int
        var body: String
        /// When non-nil, the response waits for this semaphore.
        var gate: DispatchSemaphore?
        /// Extra gate that fires AFTER the response gate, before delivery.
        var postGate: DispatchSemaphore?
    }

    private static let lock = NSLock()
    private static var routes: [String: Route] = [:]
    private static var requestedPaths: [String] = []

    static func reset() {
        lock.lock(); defer { lock.unlock() }
        routes = [:]
        requestedPaths = []
        gateTimeouts = 0
    }

    static func set(_ path: String, _ route: Route) {
        lock.lock(); defer { lock.unlock() }
        routes[path] = route
    }

    static func setJSON(_ path: String, _ body: String, status: Int = 200,
                        gate: DispatchSemaphore? = nil) {
        set(path, Route(status: status, body: body, gate: gate, postGate: nil))
    }

    /// How many times `path` has reached the transport. Tests poll this with
    /// an ASYNC wait (never a blocking one: the console runs on the main
    /// actor), so "the request is in flight" is an observed fact rather than a
    /// timing guess.
    static func requestCount(_ path: String) -> Int {
        lock.lock(); defer { lock.unlock() }
        return requestedPaths.filter { $0 == path }.count
    }

    static func requested() -> [String] {
        lock.lock(); defer { lock.unlock() }
        return requestedPaths
    }

    private static func route(for request: URLRequest) -> Route {
        lock.lock()
        // The lock is held for the whole lookup and NEVER across a callback:
        // it is released by this defer on every path (the previous version
        // returned early while holding it, which deadlocked teardown).
        defer { lock.unlock() }
        var path = request.url?.path ?? ""
        // The console client may be built for either wire generation; the
        // routes are registered once under the v2 spelling.
        for prefix in ["/api/v1", "/api/v2"] where path.hasPrefix(prefix) {
            path = "/api/v2" + path.dropFirst(prefix.count)
            break
        }
        requestedPaths.append(path)
        if let route = routes[path] { return route }
        // Every unconfigured console route answers an empty, valid payload so
        // a concurrent refresh never fails for an unrelated reason.
        switch path {
        case "/api/v2/admin/overview":
            return Route(status: 200, body: #"{"gatewayId":"gw-a","gatewayName":"测试网关","fingerprint":"fp","apiVersion":2,"onlineLines":0,"lineCount":0,"activeCallCount":0,"pushReady":false,"trickleIce":true}"#, gate: nil, postGate: nil)
        case "/api/v2/admin/lines":
            return Route(status: 200, body: #"{"lines":[]}"#, gate: nil, postGate: nil)
        case "/api/v2/admin/keys":
            return Route(status: 200, body: #"{"keys":[]}"#, gate: nil, postGate: nil)
        case "/api/v2/admin/devices":
            return Route(status: 200, body: #"{"devices":[]}"#, gate: nil, postGate: nil)
        case "/api/v2/admin/calls":
            return Route(status: 200, body: #"{"calls":[]}"#, gate: nil, postGate: nil)
        case "/api/v2/admin/conferences":
            return Route(status: 200, body: #"{"conferences":[]}"#, gate: nil, postGate: nil)
        default:
            return Route(status: 200, body: #"{"settings":{}}"#, gate: nil, postGate: nil)
        }
    }

    static func configuration() -> URLSessionConfiguration {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [AdminTestProtocol.self]
        config.timeoutIntervalForRequest = 3
        config.timeoutIntervalForResource = 5
        return config
    }

    static func loginBody(token: String = "token-1", csrf: String = "csrf-1",
                          gatewayId: String = "gw-a", username: String = "operator") -> String {
        """
        {"username":"\(username)","sessionToken":"\(token)","csrf":"\(csrf)",\
        "expiresAt":\(Int64(Date().addingTimeInterval(3600).timeIntervalSince1970 * 1000)),\
        "gatewayId":"\(gatewayId)"}
        """
    }

    static func overviewBody(onlineLines: Int = 2, gatewayId: String = "gw-a") -> String {
        """
        {"gatewayId":"\(gatewayId)","gatewayName":"测试网关","fingerprint":"fp","apiVersion":2,\
        "onlineLines":\(onlineLines),"lineCount":2,"activeCallCount":0,"pushReady":true,"trickleIce":true}
        """
    }

    // MARK: URLProtocol

    /// Deliveries run here, never on the URLProtocol callback queue: a gated
    /// (deliberately postponed) response must not block other requests, and a
    /// cancelled request must never be delivered.
    private static let deliveryQueue = DispatchQueue(label: "admin-test-protocol.delivery",
                                                     attributes: .concurrent)
    private let stateLock = NSLock()
    private var cancelled = false

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        let route = AdminTestProtocol.route(for: request)
        let request = self.request
        AdminTestProtocol.deliveryQueue.async { [weak self] in
            guard let self else { return }
            // Bounded gate: an unreleased gate delays this ONE response (never
            // the protocol queue) and always finishes, so a broken test can
            // never hang the suite.
            if let gate = route.gate, gate.wait(timeout: .now() + 10) == .timedOut {
                AdminTestProtocol.noteGateTimeout()
            }
            if let postGate = route.postGate {
                _ = postGate.wait(timeout: .now() + 10)
            }
            self.deliver(route, request: request)
        }
    }

    private func deliver(_ route: Route, request: URLRequest) {
        stateLock.lock()
        if cancelled {
            stateLock.unlock()
            return
        }
        stateLock.unlock()
        guard let url = request.url else {
            client?.urlProtocol(self, didFailWithError: URLError(.badURL))
            return
        }
        let response = HTTPURLResponse(url: url, statusCode: route.status,
                                       httpVersion: "HTTP/1.1",
                                       headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(route.body.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {
        stateLock.lock()
        cancelled = true
        stateLock.unlock()
    }

    /// How many gated responses hit their bounded wait (test-fixture health,
    /// asserted by nothing so far but visible in the log).
    private static var gateTimeouts = 0
    private static func noteGateTimeout() {
        lock.lock(); defer { lock.unlock() }
        gateTimeouts += 1
    }

    static func gateTimeoutCount() -> Int {
        lock.lock(); defer { lock.unlock() }
        return gateTimeouts
    }



}

@MainActor
final class AdminCenterFencingTests: XCTestCase {
    private var center: AdminCenter!
    private var origin: GatewayOrigin!

    override func setUp() async throws {
        try await super.setUp()
        AdminTestProtocol.reset()
        AdminAPI.sessionConfigurationOverride = AdminTestProtocol.configuration()
        // A concurrent delegate queue: a deliberately parked response must not
        // block the other requests the same test issues (production keeps the
        // default serial queue).
        let queue = OperationQueue()
        queue.maxConcurrentOperationCount = 8
        AdminAPI.sessionDelegateQueueOverride = queue
        // The administrator session lives in a per-device Keychain item; tests
        // swap in an in-memory equivalent so no real Keychain is touched and
        // the outcome is deterministic.
        AdminSessionStorage.store = AdminSessionStore(keychain: DictionaryKeychain())
        center = AdminCenter()
        guard case .success(let value) = GatewayOrigin.validate("https://gw.example.com", apiVersion: "v2") else {
            return XCTFail("origin")
        }
        origin = value
        center.bind(origin: origin, gatewayId: "gw-a")
    }

    override func tearDown() async throws {
        AdminSessionStorage.store.clear()
        AdminSessionStorage.store = AdminSessionStore.shared
        AdminAPI.sessionConfigurationOverride = nil
        AdminAPI.sessionDelegateQueueOverride = nil
        AdminTestProtocol.reset()
        try await super.tearDown()
    }

    /// Waits (without blocking the main actor) until `path` has been requested
    /// at least `count` times.
    private func waitForRequests(_ path: String, count: Int = 1,
                                 timeout: TimeInterval = 5) async -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if AdminTestProtocol.requestCount(path) >= count { return true }
            try? await Task.sleep(nanoseconds: 20_000_000)
        }
        return AdminTestProtocol.requestCount(path) >= count
    }

    private func signInA() async -> Bool {
        AdminTestProtocol.setJSON("/api/v2/admin/login", AdminTestProtocol.loginBody())
        let result = await center.signIn(origin: origin, gatewayId: "gw-a",
                                         username: "operator", password: "pw")
        if !result {
            XCTFail("sign-in failed: \(center.errorText ?? "no error")"
                    + " requested=\(AdminTestProtocol.requested())")
        }
        return result
    }

    /// A GET that was in flight across a logout must NOT repopulate the
    /// cleared console state when it finally lands.
    func testLateOverviewAfterSignOutIsIgnored() async throws {
        let signedInA = await signInA()
        XCTAssertTrue(signedInA)
        XCTAssertNotNil(center.overview)

        // Park the next overview response, start the refresh, log out while it
        // is still suspended, then release it.
        let gate = DispatchSemaphore(value: 0)
        defer { gate.signal() }
        AdminTestProtocol.setJSON("/api/v2/admin/overview",
                                  AdminTestProtocol.overviewBody(onlineLines: 9), gate: gate)
        let before = AdminTestProtocol.requestCount("/api/v2/admin/overview")
        let refresh = Task { await center.refresh(.overview) }
        let started = await waitForRequests("/api/v2/admin/overview", count: before + 1)
        XCTAssertTrue(started, "the parked overview request never started")

        // A local logout (no logout request) while the GET is still parked:
        // the console loses its session identity immediately, exactly as the
        // real "logout after the request was issued" race does.
        await center.signOut(origin: nil)
        XCTAssertNil(center.session)

        gate.signal()
        await refresh.value
        XCTAssertNil(center.session)
        XCTAssertNil(center.overview, "a late overview from the closed session must be ignored")
    }

    /// A 401 from an OLD session must not clear a NEWER session, and the new
    /// session must be fully USABLE while the old request is still in flight:
    /// it refreshes the very same section and performs a mutation. The parked
    /// 401 is then released and must be ignored.
    func testStaleUnauthorizedDoesNotClearNewLogin() async throws {
        let signedInA = await signInA()
        XCTAssertTrue(signedInA)

        // Park a devices response that will answer 401 for session A.
        let gate = DispatchSemaphore(value: 0)
        AdminTestProtocol.set("/api/v2/admin/devices",
                              .init(status: 401,
                                    body: #"{"error":"unauthorized","message":"管理会话已过期，请重新登录。"}"#,
                                    gate: gate, postGate: nil))
        // Release the gate no matter how the test exits: a failure can never
        // leave a request (and the test) hanging.
        defer { gate.signal() }

        let stale = Task { await center.refresh(.devices) }
        let started = await waitForRequests("/api/v2/admin/devices")
        XCTAssertTrue(started, "the parked devices request never started")

        // Establish session B from the device store (the same path a foreground
        // relaunch uses), with its own routes.
        AdminTestProtocol.setJSON("/api/v2/admin/devices", #"{"devices":[]}"#)
        AdminTestProtocol.setJSON("/api/v2/admin/overview", AdminTestProtocol.overviewBody(onlineLines: 5))
        let sessionB = AdminSession(
            gatewayId: "gw-a", baseURL: origin.baseURL.absoluteString,
            username: "operator", sessionToken: "token-B", csrf: "csrf-B",
            expiresAt: Date().addingTimeInterval(3600))
        XCTAssertTrue(AdminSessionStorage.store.save(sessionB))
        let release = Task {
            try? await Task.sleep(nanoseconds: 300_000_000)
            gate.signal()
        }
        await center.restore(origin: origin, gatewayId: "gw-a")
        await release.value
        XCTAssertEqual(center.session?.sessionToken, "token-B")

        // B is USABLE while A's request is still pending: the SAME section the
        // old request was loading must refresh (this is the regression the
        // per-generation operation tracking fixes), and a mutation must run.
        await center.refresh(.devices)
        XCTAssertNotNil(center.devices, "session B could not load the section session A was loading")
        XCTAssertFalse(center.isLoading(.devices), "a stale loading flag blocked the new session")
        AdminTestProtocol.setJSON("/api/v2/admin/devices/dev1/revoke", #"{"revoked":true}"#)
        let revoked = await center.revokeDevice(id: "dev1")
        XCTAssertTrue(revoked, "session B could not mutate while the old request was pending")

        await stale.value
        XCTAssertEqual(center.session?.sessionToken, "token-B",
                       "a stale 401 must not clear the newer session")
        XCTAssertNil(center.errorText, "a stale 401 must not surface an error for the new session")
    }

    /// Switching gateways clears loaded administrator data immediately, and a
    /// session issued by the other gateway is never presented as this one's.
    func testGatewaySwitchClearsLoadedStateImmediately() async throws {
        let signedInA = await signInA()
        XCTAssertTrue(signedInA)
        XCTAssertNotNil(center.overview, "signed-in console must load its overview")

        guard case .success(let other) = GatewayOrigin.validate("https://other.example.com", apiVersion: "v2") else {
            return XCTFail("origin")
        }
        center.bind(origin: other, gatewayId: "gw-b")
        XCTAssertNil(center.overview)
        XCTAssertTrue(center.lines.isEmpty)
        XCTAssertNil(center.session, "a session issued by another gateway is not usable here")
    }

    /// A login answer that lands AFTER the pairing was removed must be dropped,
    /// not stored.
    func testSignInFinishingAfterUnpairIsIgnored() async throws {
        let gate = DispatchSemaphore(value: 0)
        AdminTestProtocol.setJSON("/api/v2/admin/login", AdminTestProtocol.loginBody(), gate: gate)
        let signIn = Task {
            await center.signIn(origin: origin, gatewayId: "gw-a", username: "operator", password: "pw")
        }
        try await Task.sleep(nanoseconds: 200_000_000)
        // Unpair: the console loses its gateway while the password check runs.
        center.bind(origin: nil, gatewayId: nil)
        gate.signal()
        let result = await signIn.value
        XCTAssertFalse(result)
        XCTAssertNil(center.session)
        XCTAssertNil(AdminSessionStorage.store.load())
    }
}

// MARK: - Capability discovery (build 69)

@MainActor
final class AdminCapabilityDiscoveryTests: XCTestCase {
    private var center: AdminCenter!
    private var origin: GatewayOrigin!
    private var otherOrigin: GatewayOrigin!

    override func setUp() async throws {
        try await super.setUp()
        AdminTestProtocol.reset()
        AdminAPI.sessionConfigurationOverride = AdminTestProtocol.configuration()
        let queue = OperationQueue()
        queue.maxConcurrentOperationCount = 8
        AdminAPI.sessionDelegateQueueOverride = queue
        AdminSessionStorage.store = AdminSessionStore(keychain: DictionaryKeychain())
        center = AdminCenter()
        guard case .success(let value) = GatewayOrigin.validate("https://gw.example.com", apiVersion: "v2"),
              case .success(let other) = GatewayOrigin.validate("https://other.example.com", apiVersion: "v2") else {
            return XCTFail("origins")
        }
        origin = value
        otherOrigin = other
        center.bind(origin: origin, gatewayId: "gw-a")
    }

    override func tearDown() async throws {
        AdminSessionStorage.store.clear()
        AdminSessionStorage.store = AdminSessionStore.shared
        AdminAPI.sessionConfigurationOverride = nil
        AdminAPI.sessionDelegateQueueOverride = nil
        AdminTestProtocol.reset()
        try await super.tearDown()
    }

    /// An older v2 gateway HAS no administrator API: its 404 must be reported
    /// as unsupported (the honest upgrade reason), never as a dead entry.
    func testOldV2GatewayIsUnsupported() async throws {
        AdminTestProtocol.setJSON("/api/v2/admin/session", #"{"error":"not_found"}"#, status: 404)
        await center.refreshAdminAvailability(origin: origin)
        XCTAssertEqual(center.adminAvailability, .unsupported)
    }

    /// The build-68 gateway answers the unauthenticated probe with its own
    /// JSON 401: that is the proof the route exists.
    func testNewGatewayIsAvailable() async throws {
        AdminTestProtocol.setJSON("/api/v2/admin/session",
                                  #"{"error":"unauthorized","message":"请先登录管理账号"}"#, status: 401)
        await center.refreshAdminAvailability(origin: origin)
        XCTAssertEqual(center.adminAvailability, .available)
    }

    /// A transport failure (or a non-JSON protection page) is UNKNOWN: it
    /// must never be presented as "this gateway does not support it".
    func testProbeFailureStaysUnknown() async throws {
        AdminTestProtocol.set("/api/v2/admin/session",
                              .init(status: 503, body: "<html>gateway</html>", gate: nil, postGate: nil))
        await center.refreshAdminAvailability(origin: origin)
        XCTAssertEqual(center.adminAvailability, .unknown)

        // A bare 401 that is NOT the admin API's JSON shape is unknown too.
        AdminTestProtocol.set("/api/v2/admin/session",
                              .init(status: 401, body: "not-json", gate: nil, postGate: nil))
        await center.refreshAdminAvailability(origin: origin)
        XCTAssertEqual(center.adminAvailability, .unknown)

        // The admin API's 401 with a DIFFERENT error code is not its contract.
        AdminTestProtocol.setJSON("/api/v2/admin/session",
                                  #"{"error":"token_expired","message":"expired"}"#, status: 401)
        await center.refreshAdminAvailability(origin: origin)
        XCTAssertEqual(center.adminAvailability, .unknown)
    }

    /// An HTML/deceptive 200 and a generic proxy 403 must NOT be mistaken for
    /// the administrator API: only the real session shape / 401 contract is
    /// evidence.
    func testDeceptiveResponsesAreUnknownNotAvailable() async throws {
        AdminTestProtocol.set("/api/v2/admin/session",
                              .init(status: 200, body: "<html><body>captive portal</body></html>",
                                    gate: nil, postGate: nil))
        await center.refreshAdminAvailability(origin: origin)
        XCTAssertEqual(center.adminAvailability, .unknown, "an HTML 200 is not the API")

        AdminTestProtocol.setJSON("/api/v2/admin/session",
                                  #"{"error":"forbidden","message":"proxy denied"}"#, status: 403)
        await center.refreshAdminAvailability(origin: origin)
        XCTAssertEqual(center.adminAvailability, .unknown, "a generic 403 is not evidence")

        // The real session shape on a 200 IS evidence.
        AdminTestProtocol.setJSON("/api/v2/admin/session",
                                  #"{"username":"operator","csrf":"c","expiresAt":1792199999000,"gatewayId":"gw-a"}"#,
                                  status: 200)
        await center.refreshAdminAvailability(origin: origin)
        XCTAssertEqual(center.adminAvailability, .available)
    }

    /// A LATE probe answer for the previous gateway must not label the new
    /// one (identity fencing), and switching gateways resets to unknown.
    func testLateProbeAnswerForPreviousGatewayIsIgnored() async throws {
        let gate = DispatchSemaphore(value: 0)
        defer { gate.signal() }
        AdminTestProtocol.set("/api/v2/admin/session",
                              .init(status: 404, body: #"{"error":"not_found"}"#, gate: gate, postGate: nil))
        let probe = Task { await center.refreshAdminAvailability(origin: origin) }
        let started = await waitFor("/api/v2/admin/session")
        XCTAssertTrue(started, "the probe request never started")

        // Switch gateways while the old probe is still parked.
        center.bind(origin: otherOrigin, gatewayId: "gw-b")
        XCTAssertEqual(center.adminAvailability, .unknown, "a new gateway starts unknown")

        gate.signal()
        await probe.value
        XCTAssertEqual(center.adminAvailability, .unknown,
                       "a late answer for the previous gateway must not label the new one")
    }

    private func waitFor(_ path: String, timeout: TimeInterval = 5) async -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if AdminTestProtocol.requestCount(path) >= 1 { return true }
            try? await Task.sleep(nanoseconds: 20_000_000)
        }
        return AdminTestProtocol.requestCount(path) >= 1
    }
}

// MARK: - Recording switch opt-out (build 69)

@MainActor
final class RecordingSwitchOptOutTests: XCTestCase {
    /// A user must ALWAYS be able to turn a confirmed ON back OFF, even when
    /// the gateway policy/storage currently blocks ENABLING it. The rule
    /// lives in `RecordingConfigStore.autoRecordToggleDisabled`, which the
    /// Settings switch binds to.
    func testConfirmedOnStaysSwitchableWhilePolicyBlocksEnabling() async throws {
        let api = FakeGatewayAPI()
        api.recordingAutoModeValue = "off"
        api.recordingDeviceAutoRecord = true
        let store = RecordingConfigStore()
        await store.refresh(api: api)
        XCTAssertTrue(store.deviceAutoRecord, "the confirmed ON must be retained")
        XCTAssertTrue(store.autoRecordIneffective, "the gateway policy blocks enabling")
        XCTAssertFalse(store.autoRecordToggleDisabled,
                       "a confirmed ON must remain switchable so the user can opt out")

        // The same policy with a confirmed OFF blocks ENABLING.
        api.recordingDeviceAutoRecord = false
        await store.refresh(api: api)
        XCTAssertFalse(store.deviceAutoRecord)
        XCTAssertTrue(store.autoRecordToggleDisabled,
                      "enabling stays blocked while the policy forbids it")

        // Unknown state (not loaded yet) never blocks the switch.
        store.reset()
        XCTAssertFalse(store.autoRecordToggleDisabled)
    }
}
