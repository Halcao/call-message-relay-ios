import XCTest
@testable import CallRelay

/// Durable scoped history cache + conditional refresh — focused test package
/// for job-ce49552bcfed4276. All data is fictional (dummy contacts 张三 /
/// 李四 on reserved 138000000xx numbers); no real messages, calls or pushes.
///
/// Families (per the frozen handoff + root addendum + coordinator reviews):
/// disk roundtrip/cold launch offline, request-count + transferred-body
/// measurements on unchanged reopen/reconnect, paging/older-page retention,
/// read/delete coherence incl. the pending-mutation kill/lock-in case,
/// scope isolation + revocation + purge-vs-flush races, hydrate-no-alert /
/// read-only restore, old-gateway (no-ETag) fallback + v1 watermark shape,
/// unicode preview bounds, CloudKit independence, and the line-filter
/// variant/body-validator consistency case (A→B→A incl. restart).
@MainActor
final class GatewayHistoryCacheTests: XCTestCase {

    // MARK: Fixtures (fictional only)

    private static let zhang = "+8613800000001"
    private static let li = "+8613800000002"
    private static let keyZ = "line1:+8613800000001"
    private static let keyL = "line1:+8613800000002"

    private func message(id: String, thread: String, peer: String,
                         direction: MessageDirection = .inbound,
                         body: String = "今晚七点网关旁咖啡店见",
                         status: MessageStatus = .sent,
                         createdAt: Int64 = 5_000) -> MessageRecord {
        MessageRecord(id: id, gatewayID: "gw-hist", lineID: "line1", threadKey: thread,
                      direction: direction, peer: peer, body: body, encoding: "ucs2",
                      status: status, createdAt: createdAt)
    }
    private func threadRow(_ key: String, _ peer: String, unread: Int = 0,
                           at: Int64 = 5_000) -> MessageThread {
        MessageThread(key: key, peer: peer, unreadCount: unread,
                      lastMessage: message(id: "last-\(key)", thread: key, peer: peer, createdAt: at))
    }
    private func callRecord(id: String, peer: String, finished: Bool,
                            state: CallState = .active) -> CallRecord {
        CallRecord(id: id, gatewayID: "gw-hist", lineID: "line1", direction: .inbound,
                   peer: peer, state: finished ? .idle : state,
                   startedAt: 4_000, connectedAt: finished ? 4_200 : nil,
                   endedAt: finished ? 4_900 : nil, endReason: finished ? "normal" : nil,
                   recordingId: nil, recordingState: nil, recordingDurationMs: nil)
    }
    private func tempURL(_ tag: String) -> URL {
        FileManager.default.temporaryDirectory
            .appendingPathComponent("callrelay-history-\(tag)-\(UUID().uuidString).json")
    }
    private func makeStore(_ tag: String = "t", throttle: TimeInterval = 0) -> GatewayHistoryStore {
        GatewayHistoryStore(scopeIdentifier: nil, explicitURL: tempURL(tag),
                            writeThrottle: throttle)!
    }
    private func freshStore(_ store: GatewayHistoryStore) -> GatewayHistoryStore {
        // A brand-new instance over the same file = "new process".
        GatewayHistoryStore(scopeIdentifier: nil, explicitURL: store.fileURL)!
    }
    private func makeInbox(api: FakeGatewayAPI, store: GatewayHistoryStore?,
                           tombstoneDefaults: UserDefaults,
                           tombstoneScope: String? = "gw-hist") -> MessageInbox {
        api.validatorReader = { [weak store] key in store?.validator(for: key) }
        return MessageInbox(api: api, filter: nil, tombstoneScope: tombstoneScope,
                            tombstoneDefaults: tombstoneDefaults, historyStore: store)
    }
    private func suiteDefaults() -> UserDefaults {
        UserDefaults(suiteName: "callrelay.hist-tests.\(UUID().uuidString)")!
    }

    // MARK: A. Disk roundtrip + cold launch offline

    func testSnapshotDiskRoundtripAcrossInstances() {
        let store = makeStore("roundtrip")
        let threads = [threadRow(Self.keyZ, Self.zhang, unread: 2, at: 9_000),
                       threadRow(Self.keyL, Self.li, at: 8_000)]
        let junk = [threadRow("line1:+8613800000003", "+8613800000003", at: 7_000)]
        // Pages first: the hasMore flag is only meaningful while the thread
        // has cached rows (bounded-paging invariant).
        store.recordMessages([message(id: "m1", thread: Self.keyZ, peer: Self.zhang),
                              message(id: "m2", thread: Self.keyZ, peer: Self.zhang, createdAt: 6_000)],
                             for: Self.keyZ,
                             validatorUpdates: ["https://fake/messages?threadKey=\(Self.keyZ)": "\"P1\""],
                             savedAt: Date())
        store.recordThreads(threads: threads, junk: junk, hasMoreKeys: [Self.keyZ],
                            validatorUpdates: ["https://fake/threads?folder=normal": "\"E1\""],
                            listSurfaceTag: "folders", savedAt: Date())
        store.recordCalls([callRecord(id: "c1", peer: Self.zhang, finished: true)],
                          validatorUpdates: [:], savedAt: Date())
        store.flush()

        let reloaded = freshStore(store)
        XCTAssertEqual(reloaded.loadStatus, .loaded)
        let snapshot = reloaded.snapshotForHydrate()
        XCTAssertEqual(snapshot?.threads, threads)
        XCTAssertEqual(snapshot?.junk, junk)
        XCTAssertEqual(snapshot?.messages[Self.keyZ]?.map(\.id), ["m1", "m2"])
        XCTAssertEqual(snapshot?.calls.map(\.id), ["c1"])
        XCTAssertEqual(snapshot?.hasMoreKeys, [Self.keyZ])
        XCTAssertEqual(snapshot?.validators["https://fake/threads?folder=normal"], "\"E1\"")
        XCTAssertEqual(reloaded.listSurfaceTag, "folders")
        XCTAssertEqual(reloaded.validator(for: "https://fake/messages?threadKey=\(Self.keyZ)"), "\"P1\"")
    }

    func testColdOfflineHydrateRendersWithZeroNetwork() async {
        let store = makeStore("offline")
        let defaults = suiteDefaults()
        // Seed one settled session.
        let seedApi = FakeGatewayAPI()
        seedApi.threads = [threadRow(Self.keyZ, Self.zhang, unread: 2)]
        seedApi.threadPages[Self.keyZ] = [message(id: "s1", thread: Self.keyZ, peer: Self.zhang)]
        let seed = makeInbox(api: seedApi, store: store, tombstoneDefaults: defaults)
        _ = await seed.refreshThreads()
        seed.openThread(Self.keyZ)
        await waitUntil { seed.rows(for: Self.keyZ).contains { $0.id == "s1" } }
        seed.invalidate()

        // New process, gateway unreachable: everything throws; NOTHING may be
        // downloaded just to render.
        let offline = UnreachableAPI()
        let inbox = makeInbox(api: offline, store: freshStore(store), tombstoneDefaults: suiteDefaults())
        XCTAssertEqual(inbox.listPhase, .loaded, "hydrated list renders without any network")
        XCTAssertEqual(inbox.displayThreads.map(\.key), [Self.keyZ])
        inbox.openThread(Self.keyZ)
        XCTAssertEqual(inbox.openPhase(for: Self.keyZ), .loaded, "loaded bodies render instantly")
        XCTAssertEqual(inbox.rows(for: Self.keyZ).map(\.id), ["s1"],
                       "message content survives restart offline")
        XCTAssertEqual(offline.attemptedRequests, 0, "hydrate fired no requests before user action")
    }

    func testOfflineOpenThreadKeepsCacheWhenRevalidationFails() async {
        let store = makeStore("offlineopen")
        let defaults = suiteDefaults()
        let seedApi = FakeGatewayAPI()
        seedApi.threads = [threadRow(Self.keyZ, Self.zhang)]
        seedApi.threadPages[Self.keyZ] = [message(id: "keep1", thread: Self.keyZ, peer: Self.zhang)]
        let seed = makeInbox(api: seedApi, store: store, tombstoneDefaults: defaults)
        _ = await seed.refreshThreads()
        seed.openThread(Self.keyZ)
        await waitUntil { seed.rows(for: Self.keyZ).count == 1 }
        seed.invalidate()

        let offline = UnreachableAPI()
        let inbox = makeInbox(api: offline, store: freshStore(store), tombstoneDefaults: suiteDefaults())
        inbox.openThread(Self.keyZ)
        await waitUntil { offline.attemptedRequests >= 1 }
        // Transient failure RETAINS the data (never fails a loaded view).
        XCTAssertEqual(inbox.openPhase(for: Self.keyZ), .loaded)
        XCTAssertEqual(inbox.rows(for: Self.keyZ).map(\.id), ["keep1"])
    }

    func testCorruptAndVersionMismatchedFilesDegradeToFullFetch() async {
        let url = tempURL("corrupt")
        try! "not json at all {".data(using: .utf8)!.write(to: url)
        let store = GatewayHistoryStore(scopeIdentifier: nil, explicitURL: url)!
        XCTAssertEqual(store.loadStatus, .corrupt)
        XCTAssertNil(store.snapshotForHydrate())

        let api = FakeGatewayAPI()
        api.threads = [threadRow(Self.keyL, Self.li)]
        api.validatorReader = { [weak store] key in store?.validator(for: key) }
        let inbox = MessageInbox(api: api, filter: nil, historyStore: store)
        XCTAssertNotEqual(inbox.listPhase, .loaded, "no partial trust in a corrupt file")
        _ = await inbox.refreshThreads()
        XCTAssertEqual(api.listThreadsFullBodyCount, 1, "converges with a full fetch")
        XCTAssertEqual(inbox.displayThreads.count, 1)

        // Version mismatch is discarded, never partially read.
        let mismatchURL = tempURL("mismatch")
        try! #"{"schemaVersion":99,"threads":[]}"#.data(using: .utf8)!.write(to: mismatchURL)
        let mismatched = GatewayHistoryStore(scopeIdentifier: nil, explicitURL: mismatchURL)!
        XCTAssertEqual(mismatched.loadStatus, .versionMismatch)
        XCTAssertNil(mismatched.snapshotForHydrate())
    }

    // MARK: B. Request counts + transferred bytes on unchanged repeats

    func testUnchangedReopenAfterRestartUses304WithZeroFullBodies() async {
        let store = makeStore("304")
        let defaults = suiteDefaults()
        let api1 = FakeGatewayAPI()
        api1.threads = [threadRow(Self.keyZ, Self.zhang)]
        api1.threadPages[Self.keyZ] = [message(id: "z1", thread: Self.keyZ, peer: Self.zhang, status: .read)]
        let inbox1 = makeInbox(api: api1, store: store, tombstoneDefaults: defaults)
        _ = await inbox1.refreshThreads()
        inbox1.openThread(Self.keyZ)
        await waitUntil { inbox1.rows(for: Self.keyZ).count == 1 }
        XCTAssertEqual(api1.listThreadsFullBodyCount, 1)
        XCTAssertEqual(api1.pageFetchFullBodyCount, 1)
        inbox1.invalidate()

        // Same scope, new session, server content UNCHANGED: every repeat
        // open/refresh is a 304 — no whole-history response bodies at all.
        let api2 = FakeGatewayAPI()
        api2.threads = api1.threads
        api2.threadPages = api1.threadPages
        let inbox2 = makeInbox(api: api2, store: freshStore(store), tombstoneDefaults: defaults)
        XCTAssertEqual(inbox2.listPhase, .loaded)
        _ = await inbox2.refreshThreads()
        inbox2.openThread(Self.keyZ)
        await waitUntil { inbox2.openPhase(for: Self.keyZ) == .loaded && inbox2.rows(for: Self.keyZ).count == 1 }
        _ = await inbox2.refreshThreads()
        XCTAssertEqual(api2.listThreadsFullBodyCount, 0, "unchanged list: zero full bodies")
        XCTAssertEqual(api2.listThreads304Count, 2)
        XCTAssertEqual(api2.pageFetchFullBodyCount, 0, "unchanged page: zero full bodies")
        XCTAssertGreaterThanOrEqual(api2.pageFetch304Count, 1)
        XCTAssertEqual(inbox2.displayThreads.map(\.key), [Self.keyZ], "304 keeps the local copy")
    }

    func testRemoteReadConvergesViaBoundedRevalidationWhileStreamOpen() async {
        // The coordinator's case: the gateway does NOT broadcast read marks,
        // so even with a healthy SSE stream the bounded conditional refresh
        // must converge another device's read — and unchanged data must stay
        // at 304/zero body bytes.
        let store = makeStore("remoteread")
        let api = FakeGatewayAPI()
        api.threads = [threadRow(Self.keyZ, Self.zhang, unread: 2)]
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: suiteDefaults())
        inbox.isEventStreamHealthy = { true }
        _ = await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 2)

        api.simulateRemoteReadOfThread(Self.keyZ)
        _ = await inbox.refreshThreads()
        XCTAssertEqual(inbox.displayThreads.first?.unreadCount, 0,
                       "bounded conditional revalidation surfaces the non-broadcast read")
        XCTAssertEqual(api.listThreadsFullBodyCount, 2, "exactly one full body per real change")
        _ = await inbox.refreshThreads()
        XCTAssertEqual(api.listThreads304Count, 1, "unchanged revalidation costs no body")
    }

    // MARK: C. Event-driven cache updates

    func testEventMergeLandsInDurableStoreWithoutFullListGET() async {
        let store = makeStore("events")
        let defaults = suiteDefaults()
        let api = FakeGatewayAPI()
        api.threads = [threadRow(Self.keyZ, Self.zhang)]
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: defaults)
        _ = await inbox.refreshThreads()
        XCTAssertEqual(api.listThreadsFullBodyCount, 1)

        inbox.isEventStreamHealthy = { true }
        let event = message(id: "li-new", thread: Self.keyL, peer: Self.li,
                            body: "张三您好，明天顺路接您", createdAt: 9_000)
        inbox.apply(eventMessage: event)
        await waitUntil { inbox.displayThreads.contains { $0.key == Self.keyL } }
        XCTAssertEqual(api.listThreadsFullBodyCount, 1, "healthy-stream events never re-GET the list")
        XCTAssertTrue(inbox.displayThreads.contains { $0.key == Self.keyL })

        // The event row is durable…
        let reloaded = freshStore(store)
        let snapshot = reloaded.snapshotForHydrate()
        XCTAssertEqual(snapshot?.threads.map(\.key).sorted(), [Self.keyL, Self.keyZ].sorted())
        XCTAssertEqual(snapshot?.messages[Self.keyL]?.map(\.id), ["li-new"])
        // …and the list validator was dropped for coherence (locally
        // transformed body), so the next session re-fetches at most once.
        XCTAssertEqual(reloaded.listValidatorCountForTest, 0)
        // …and a fresh offline inbox renders it.
        let offline = UnreachableAPI()
        let inbox2 = MessageInbox(api: offline, filter: nil, tombstoneScope: "gw-hist",
                                  tombstoneDefaults: suiteDefaults(), historyStore: reloaded)
        XCTAssertTrue(inbox2.displayThreads.contains { $0.key == Self.keyL })
    }

    // MARK: D. Read/delete coherence + durability races

    func testDeleteTombstoneDurableAndLateStaleRefreshCannotReAdd() async {
        let store = makeStore("delete")
        let defaults = suiteDefaults()
        let api = FakeGatewayAPI()
        api.threads = [threadRow(Self.keyZ, Self.zhang), threadRow(Self.keyL, Self.li)]
        api.threadPages[Self.keyZ] = [message(id: "d1", thread: Self.keyZ, peer: Self.zhang)]
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: defaults)
        _ = await inbox.refreshThreads()
        inbox.openThread(Self.keyZ)
        await waitUntil { inbox.rows(for: Self.keyZ).count == 1 }

        let deleted = await inbox.deleteThread(Self.keyZ)
        XCTAssertTrue(deleted)
        XCTAssertFalse(inbox.displayThreads.contains { $0.key == Self.keyZ })

        // Recreate from disk with the same tombstone scope: deleted thread
        // stays hidden although the FAKE server still lists it.
        let api2 = FakeGatewayAPI()
        api2.threads = api.threads
        let inbox2 = makeInbox(api: api2, store: freshStore(store), tombstoneDefaults: defaults)
        XCTAssertFalse(inbox2.threads.contains { $0.key == Self.keyZ })
        _ = await inbox2.refreshThreads()
        XCTAssertFalse(inbox2.displayThreads.contains { $0.key == Self.keyZ },
                       "late/full refresh cannot re-add a tombstoned thread")
        XCTAssertNil(freshStore(store).snapshotForHydrate()?.messages[Self.keyZ],
                     "deleted thread's cached page is durably gone with its validators")
    }

    func testPendingReadKeepsRawServerBodyUnderItsETagNeverOptimisticCopy() async {
        let store = makeStore("readack")
        let defaults = suiteDefaults()
        let api = FakeGatewayAPI()
        api.threads = [threadRow(Self.keyZ, Self.zhang, unread: 2)]
        api.threadPages[Self.keyZ] = [
            message(id: "u2", thread: Self.keyZ, peer: Self.zhang, createdAt: 5_200),
            message(id: "u1", thread: Self.keyZ, peer: Self.zhang, createdAt: 5_100),
        ]
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: defaults)
        _ = await inbox.refreshThreads()
        XCTAssertNotNil(store.validator(for: FakeGatewayAPI.threadsAllKey))

        // REJECTED read: the optimistic badge must NOT reach the durable
        // snapshot under the raw server ETag. The disk keeps the raw unread
        // copy plus its matching validator; a new process shows server truth.
        api.readErrors["u1"] = APIError.network(URLError(.timedOut))
        api.readErrors["u2"] = APIError.network(URLError(.timedOut))
        _ = await inbox.markThreadsRead([Self.keyZ])
        await waitUntil { !inbox.isPendingRead(Self.keyZ) }
        let afterFailure = freshStore(store).snapshotForHydrate()
        XCTAssertEqual(afterFailure?.threads.first?.unreadCount, 2,
                       "failed read never persists a masked badge under the raw ETag")
        XCTAssertNotNil(freshStore(store).validator(for: FakeGatewayAPI.threadsAllKey),
                        "raw body+ETag pair stays coherent through a failed mutation")

        // SUCCESSFUL read followed by a crash BEFORE the next settled refresh:
        // the read-ack settle writes the display copy but DROPS the list
        // validators, so no future process can 304 the optimistic body into
        // permanence. (markThreadsRead then re-fetches and re-coheres with the
        // raw pair; when the server itself converges, truth follows.)
        api.readErrors.removeAll()
        _ = await inbox.markThreadsRead([Self.keyZ])
        await waitUntil { inbox.displayThreads.first?.unreadCount == 0 }
        let afterSuccess = freshStore(store).snapshotForHydrate()
        // Whatever badge disk holds, the invariant is: body matches server
        // content for its validator. Server content here is still unread=2 in
        // the fake, so the coherent disk copy is unread=2 (NOT the optimistic
        // 0) — a relaunch shows server truth, never a phantom read.
        XCTAssertEqual(afterSuccess?.threads.first?.unreadCount, 2,
                       "optimistic display state is never the durable body under a live ETag")
        // Once the server converges (remote read settles), bounded
        // revalidation follows truth — and stays at 304 afterwards.
        api.simulateRemoteReadOfThread(Self.keyZ)
        _ = await inbox.refreshThreads()
        XCTAssertEqual(freshStore(store).snapshotForHydrate()?.threads.first?.unreadCount, 0,
                       "the converged server truth persists, then 304 locks nothing wrong")
        _ = await inbox.refreshThreads()
        XCTAssertGreaterThanOrEqual(api.listThreads304Count, 1)
    }

    func testPendingDeleteIsNeverPersistedAsHiddenForever() async {
        let store = makeStore("pendingkill")
        let defaults = suiteDefaults()
        let api = FakeGatewayAPI()
        api.threads = [threadRow(Self.keyZ, Self.zhang), threadRow(Self.keyL, Self.li)]
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: defaults)
        _ = await inbox.refreshThreads()
        // User intent only (gateway round trip not complete): hide is
        // IN-MEMORY only; a 304-style refresh commits nothing.
        inbox.beginPendingDeletion(Self.keyZ)
        _ = await inbox.refreshThreads()
        inbox.invalidate()

        let recreated = freshStore(store).snapshotForHydrate()
        XCTAssertEqual(recreated?.threads.count, 2,
                       "unresolved delete never hides a row durably")
        XCTAssertNotNil(freshStore(store).validator(for: FakeGatewayAPI.threadsAllKey),
                        "the raw body+ETag pair is untouched by pending intent")
    }

    func testLateRefreshAfterInvalidateNeverWritesOrResurrects() async {
        let store = makeStore("late")
        let defaults = suiteDefaults()
        let api = FakeGatewayAPI()
        api.threads = [threadRow(Self.keyZ, Self.zhang)]
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: defaults)
        _ = await inbox.refreshThreads()
        inbox.invalidate()
        let settledData = try! Data(contentsOf: store.fileURL)

        let api2 = FakeGatewayAPI()
        api2.threads = [threadRow(Self.keyL, Self.li)]
        api2.validatorReader = { [weak store] key in store?.validator(for: key) }
        let stale = api2
        _ = await inbox.refreshThreads()
        let other = makeInbox(api: stale, store: nil, tombstoneDefaults: defaults)
        _ = await other.refreshThreads()
        XCTAssertEqual(try! Data(contentsOf: store.fileURL), settledData,
                       "an invalidated inbox persists nothing")
        // A purged scope never re-materializes from late writes.
        store.purge()
        store.recordThreads(threads: [threadRow(Self.keyZ, Self.zhang)], junk: nil,
                            hasMoreKeys: nil, validatorUpdates: [:], savedAt: Date())
        store.flush()
        XCTAssertFalse(FileManager.default.fileExists(atPath: store.fileURL.path),
                       "writes racing a purge cannot resurrect the scope")
    }

    func testPurgeBeatsConcurrentFlushAcrossRepetitions() async {
        // Root review: flush captured a snapshot then raced purge and used to
        // rewrite the purged file. All filesystem touches now re-check the
        // purge flag INSIDE the serialized queue, so this must hold every
        // time, not "usually".
        for _ in 0..<25 {
            let store = makeStore("race", throttle: 0.002)
            let rowZ = threadRow(Self.keyZ, Self.zhang)
            let rowL = threadRow(Self.keyL, Self.li)
            store.recordThreads(threads: [rowZ], junk: nil,
                                hasMoreKeys: nil, validatorUpdates: [:], savedAt: Date())
            await withCheckedContinuation { (cont: CheckedContinuation<Void, Never>) in
                let group = DispatchGroup()
                group.enter()
                DispatchQueue.global().async {
                    store.recordThreads(threads: [rowL], junk: nil,
                                        hasMoreKeys: nil, validatorUpdates: [:], savedAt: Date())
                    store.flush()
                    group.leave()
                }
                group.enter()
                DispatchQueue.global().async {
                    store.purge()
                    group.leave()
                }
                group.notify(queue: .global()) { cont.resume() }
            }
            XCTAssertNil(GatewayHistoryStore(scopeIdentifier: nil, explicitURL: store.fileURL)?
                .snapshotForHydrate(), "a purged scope never comes back after a racing flush")
        }
    }

    // MARK: E. Scope isolation / revocation

    func testScopeFollowsCanonicalAPIIdentityAndDevicePrincipal() throws {
        guard case .success(let https) = GatewayOrigin.validate("https://gw.example.test", apiVersion: "v2"),
              case .success(let otherPort) = GatewayOrigin.validate("https://gw.example.test:8443", apiVersion: "v2"),
              case .success(let v1) = GatewayOrigin.validate("https://gw.example.test", apiVersion: "v1")
        else { return XCTFail("valid origins") }
        let a = HistoryScope.identifier(origin: https, gatewayID: "gw-1", deviceID: "dev-1")
        let b = HistoryScope.identifier(origin: otherPort, gatewayID: "gw-1", deviceID: "dev-1")
        let c = HistoryScope.identifier(origin: https, gatewayID: "gw-1", deviceID: "dev-2")
        let d = HistoryScope.identifier(origin: v1, gatewayID: "gw-1", deviceID: "dev-1")
        XCTAssertEqual(Set([a, b, c, d].compactMap { $0 }).count, 4,
                       "origin, port, device AND api version all isolate the scope")
        XCTAssertNil(HistoryScope.identifier(origin: https, gatewayID: "gw-1", deviceID: nil))
        XCTAssertNil(HistoryScope.identifier(origin: https, gatewayID: "gw-1", deviceID: "   "))
        // No principal => the store declines to exist => ephemeral session.
        XCTAssertNil(GatewayHistoryStore(scopeIdentifier: nil))
    }

    func testDifferentScopesNeverShareFiles() {
        func storeFor(_ device: String) -> GatewayHistoryStore {
            guard case .success(let origin) = GatewayOrigin.validate("https://scope.test", apiVersion: "v2")
            else { fatalError() }
            let scope = HistoryScope.identifier(origin: origin, gatewayID: "gw-x", deviceID: device)!
            return GatewayHistoryStore(scopeIdentifier: scope)!
        }
        let dev1 = storeFor("dev-1")
        dev1.recordThreads(threads: [threadRow(Self.keyZ, Self.zhang)], junk: nil,
                           hasMoreKeys: nil, validatorUpdates: [:], savedAt: Date())
        dev1.flush()
        defer { dev1.purge() }
        let dev2 = storeFor("dev-2")
        XCTAssertNotEqual(dev1.fileURL, dev2.fileURL)
        XCTAssertNil(dev2.snapshotForHydrate(), "another device's pairing sees an empty hydrate")
        dev2.purge()
    }

    // MARK: F. Body/validator surface consistency (root's final case)

    func testMixed304ChangedFolderRefreshNeverOverwritesUnchangedBodyWithMaskedDisplay() async {
        // Root review: with a PENDING read/delete over the normal folder, a
        // refresh whose normal answers 304 and junk answers 200 must write
        // ONLY the new junk body — the unchanged raw normal copy and its
        // coherent ETag stay untouched on disk (never the masked display).
        let store = makeStore("mixed")
        let defaults = suiteDefaults()
        let normalZ = threadRow(Self.keyZ, Self.zhang, unread: 2)
        let junkL = threadRow(Self.keyL, Self.li, at: 4_000)
        let api = FakeGatewayAPI()
        api.quarantineSupported = true
        api.quarantineFolders = ["normal": [normalZ], "junk": [junkL]]
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: defaults)
        inbox.gatewayQuarantineAvailable = { true }
        _ = await inbox.refreshThreads()
        let normalValidatorBefore = store.validator(for: FakeGatewayAPI.threadsFolderKey("normal"))
        let normalBodyBefore = freshStore(store).snapshotForHydrate()?.threads
        XCTAssertNotNil(normalValidatorBefore)

        inbox.beginPendingDeletion(Self.keyZ)
        api.quarantineFolders["junk"] = [
            junkL,
            threadRow("line1:+8613800000003", "+8613800000003", at: 3_000),
        ]
        _ = await inbox.refreshThreads()
        XCTAssertEqual(api.listThreads304Count, 1, "normal folder answered 304")
        XCTAssertEqual(api.listThreadsFullBodyCount, 3, "only junk re-downloaded (2 changes + 1 junk this pass)")

        let disk = freshStore(store).snapshotForHydrate()
        XCTAssertEqual(disk?.threads, normalBodyBefore, "unchanged folder keeps its RAW body")
        XCTAssertEqual(freshStore(store).validator(for: FakeGatewayAPI.threadsFolderKey("normal")),
                       normalValidatorBefore, "and its coherent validator")
        XCTAssertEqual(disk?.junk.count, 2, "the changed folder DID converge")
        XCTAssertFalse(inbox.displayThreads.contains { $0.key == Self.keyZ },
                       "UI optimistic intent stays instant (responsiveness preserved)")
    }

    func testFlushThenReplacementPurgeSurvivesStaleDelayedWrite() async throws {
        // Root review: an old store instance retained by a hung task flushes
        // before its throttled coalesced write fires; a REPLACEMENT store
        // then purges the same file (unpair/reconnect swap). The stale
        // delayed callback must be a no-op — the purged scope stays gone.
        let storeA = makeStore("swap", throttle: 0.06)
        let rowZ = threadRow(Self.keyZ, Self.zhang)
        storeA.recordThreads(threads: [rowZ], junk: nil, hasMoreKeys: nil,
                             validatorUpdates: [:], savedAt: Date())
        storeA.flush()
        let storeB = GatewayHistoryStore(scopeIdentifier: nil, explicitURL: storeA.fileURL)!
        storeB.purge()
        XCTAssertFalse(FileManager.default.fileExists(atPath: storeA.fileURL.path))
        try await Task.sleep(nanoseconds: 300_000_000)
        XCTAssertFalse(FileManager.default.fileExists(atPath: storeA.fileURL.path),
                       "a stale instance's delayed coalesced write cannot resurrect a purged scope")
        // The replacement's own in-memory purge state is final.
        XCTAssertNil(storeB.snapshotForHydrate())
    }

    func testLineFilterVariantFlipAtoBtoAIncludingRestartNever304sWrongRows() async {
        let store = makeStore("variant")
        let defaults = suiteDefaults()
        let threadA = threadRow("lineA:+8613800000001", Self.zhang)
        let threadB = threadRow("lineB:+8613800000002", Self.li)

        func makeApi() -> FakeGatewayAPI {
            let api = FakeGatewayAPI()
            api.threadsByLine = ["lineA": [threadA], "lineB": [threadB]]
            return api
        }

        // Session 1 on line A.
        let api1 = makeApi()
        let inbox1 = makeInbox(api: api1, store: store, tombstoneDefaults: defaults)
        inbox1.setLineFilter("lineA")
        await waitUntil { inbox1.displayThreads.map(\.key) == [threadA.key] }
        XCTAssertNotNil(freshStore(store).validator(for: FakeGatewayAPI.threadsLineKey("lineA")),
                        "A's coherent body+ETag is committed")
        inbox1.invalidate()

        // Switch to line B (same scope, new session): variant differs → A's
        // stale ETags are dropped and B's body+ETag commit together.
        let api2 = makeApi()
        let inbox2 = makeInbox(api: api2, store: freshStore(store), tombstoneDefaults: defaults)
        inbox2.setLineFilter("lineB")
        await waitUntil { inbox2.displayThreads.map(\.key) == [threadB.key] }
        XCTAssertNil(freshStore(store).validator(for: FakeGatewayAPI.threadsLineKey("lineA")),
                     "A's validator must not survive B's commit")

        // Back to line A after a RESTART: A has no stored validator anymore,
        // so it must full-fetch instead of 304-ing B's rows.
        let api3 = makeApi()
        let inbox3 = makeInbox(api: api3, store: freshStore(store), tombstoneDefaults: defaults)
        inbox3.setLineFilter("lineA")
        await waitUntil { inbox3.displayThreads.map(\.key) == [threadA.key] }
        XCTAssertEqual(inbox3.displayThreads.map(\.key), [threadA.key],
                       "A→B→A renders A's rows — never B's under A's stale ETag")
        XCTAssertGreaterThanOrEqual(api3.listThreadsFullBodyCount, 1,
                                    "the variant switch forced one honest full fetch")
        XCTAssertNotNil(freshStore(store).validator(for: FakeGatewayAPI.threadsLineKey("lineA")),
                        "A's pair is coherent again")
    }

    func testFoldersAndLineSurfacesSwitchDropsCrossSurfaceStaleValidators() async {
        let store = makeStore("folders")
        let defaults = suiteDefaults()
        let api = FakeGatewayAPI()
        api.quarantineSupported = true
        api.quarantineFolders = ["normal": [threadRow(Self.keyZ, Self.zhang)],
                                 "junk": [threadRow(Self.keyL, Self.li)]]
        api.threads = api.quarantineFolders["normal"]!
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: defaults)
        inbox.gatewayQuarantineAvailable = { true }
        _ = await inbox.refreshThreads()
        XCTAssertEqual(freshStore(store).listSurfaceTag, "folders")
        // Unchanged folders revalidation stays efficient (304 per dataset).
        _ = await inbox.refreshThreads()
        XCTAssertEqual(api.listThreadsFullBodyCount, 2, "normal+junk each fetched once")
        XCTAssertEqual(api.listThreads304Count, 2, "both datasets 304 when unchanged")

        // Capability flips to line-mode: folders validators must go.
        inbox.gatewayQuarantineAvailable = { false }
        _ = await inbox.refreshThreads()
        XCTAssertNil(freshStore(store).validator(for: FakeGatewayAPI.threadsFolderKey("normal")),
                     "surface switch dropped the stale folder validator")
        XCTAssertEqual(freshStore(store).listSurfaceTag, "line:all")
    }

    // MARK: G. Hydrate is read-only; alerts untouched

    func testHydrateAndRestoreNeverMarkReadsOrReRing() async {
        let store = makeStore("noalert")
        let defaults = suiteDefaults()
        let seedApi = FakeGatewayAPI()
        seedApi.threads = [threadRow(Self.keyZ, Self.zhang, unread: 3)]
        seedApi.threadPages[Self.keyZ] = [
            message(id: "u9", thread: Self.keyZ, peer: Self.zhang, status: .sent),
        ]
        seedApi.callsStub = [callRecord(id: "ring-old", peer: Self.zhang, finished: false,
                                        state: .incomingRinging)]
        let seed = makeInbox(api: seedApi, store: store, tombstoneDefaults: defaults)
        _ = await seed.refreshThreads()
        seed.openThread(Self.keyZ)
        await waitUntil { seed.rows(for: Self.keyZ).contains { $0.id == "u9" } }
        store.recordCalls(seedApi.callsStub, validatorUpdates: [:], savedAt: Date())
        store.flush()
        seed.invalidate()

        // Restore in a new process: unread rows render WITHOUT any read POST,
        // and stored call rows are display-only (no driver is even reachable
        // from MessageInbox; handleIncoming's guards never see a hydrate).
        let api2 = FakeGatewayAPI()
        api2.threads = seedApi.threads
        api2.threadPages = seedApi.threadPages
        let inbox2 = makeInbox(api: api2, store: freshStore(store), tombstoneDefaults: defaults)
        inbox2.start(pollInterval: 3600)
        await pumpMainActor(8)
        XCTAssertTrue(api2.readMarked.isEmpty, "hydration must never mark messages read")
        XCTAssertTrue(inbox2.displayThreads.contains { $0.key == Self.keyZ })
        XCTAssertEqual(inbox2.listPhase, .loaded)
        inbox2.invalidate()
    }

    // MARK: H. Old gateway + v1 shape

    func testOldGatewayWithoutEtagKeepsFullFetchBehaviorAndNothingBreaks() async {
        let store = makeStore("old")
        let defaults = suiteDefaults()
        let api = FakeGatewayAPI()
        api.conditionalNeverRevalidates = true
        api.threads = [threadRow(Self.keyZ, Self.zhang)]
        api.threadPages[Self.keyZ] = [message(id: "o1", thread: Self.keyZ, peer: Self.zhang)]
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: defaults)
        _ = await inbox.refreshThreads()
        _ = await inbox.refreshThreads()
        XCTAssertEqual(api.listThreadsFullBodyCount, 2, "old gateways: unchanged behavior (always 200)")
        XCTAssertEqual(api.listThreads304Count, 0)
        XCTAssertTrue(freshStore(store).validatorsForTest.isEmpty,
                      "no validators are stored for a gateway that never advertises ETags")
        inbox.openThread(Self.keyZ)
        await waitUntil { inbox.rows(for: Self.keyZ).count == 1 }
        XCTAssertEqual(inbox.openPhase(for: Self.keyZ), .loaded)
    }

    func testV1ReconcilePassesCachedWatermarkNotZeroAndV2Skips() async {
        let store = makeStore("watermark")
        let defaults = suiteDefaults()
        let seedApi = FakeGatewayAPI()
        seedApi.threads = [threadRow(Self.keyZ, Self.zhang)]
        seedApi.threadPages[Self.keyZ] = [
            message(id: "w1", thread: Self.keyZ, peer: Self.zhang, createdAt: 6_400),
            message(id: "w2", thread: Self.keyZ, peer: Self.zhang, createdAt: 7_777),
        ]
        let seed = makeInbox(api: seedApi, store: store, tombstoneDefaults: defaults)
        _ = await seed.refreshThreads()
        seed.openThread(Self.keyZ)
        await waitUntil { seed.rows(for: Self.keyZ).count == 2 }
        seed.invalidate()

        let api = FakeGatewayAPI()
        api.extraMessages = []
        let inbox = makeInbox(api: api, store: freshStore(store), tombstoneDefaults: defaults)
        inbox.transportIsV2 = false
        _ = await inbox.reconcile()
        XCTAssertEqual(api.listMessagesAfterCursors, [7_777],
                       "v1 catch-up is bounded by the cached watermark, never after:0")

        let api2 = FakeGatewayAPI()
        let inbox2 = makeInbox(api: api2, store: freshStore(store), tombstoneDefaults: defaults)
        inbox2.transportIsV2 = true
        _ = await inbox2.reconcile()
        XCTAssertTrue(api2.listMessagesAfterCursors.isEmpty,
                      "v2 skips the dead global-after call entirely")
    }

    // MARK: I. Junk retention (root review) + unicode

    func testTransientRefreshFailureKeepsHydratedJunkEverywhere() async {
        let store = makeStore("junkretain")
        let defaults = suiteDefaults()
        let seedApi = FakeGatewayAPI()
        seedApi.quarantineSupported = true
        seedApi.quarantineFolders = ["normal": [threadRow(Self.keyZ, Self.zhang)],
                                     "junk": [threadRow(Self.keyL, Self.li)]]
        let seed = makeInbox(api: seedApi, store: store, tombstoneDefaults: defaults)
        seed.gatewayQuarantineAvailable = { true }
        _ = await seed.refreshThreads()
        XCTAssertEqual(seed.junkThreads.count, 1)
        seed.invalidate()

        // Cold hydrate shows junk; a TRANSIENT refresh failure keeps it in
        // memory AND on disk (nothing is committed on the failure path).
        let api = FakeGatewayAPI()
        api.quarantineSupported = true
        api.conditionalListError = APIError.network(URLError(.timedOut))
        let inbox = makeInbox(api: api, store: freshStore(store), tombstoneDefaults: defaults)
        inbox.gatewayQuarantineAvailable = { true }
        XCTAssertEqual(inbox.junkThreads.count, 1, "hydrated junk renders offline")
        let before = try! Data(contentsOf: store.fileURL)
        _ = await inbox.refreshThreads()
        XCTAssertEqual(inbox.junkThreads.count, 1, "transient failure never erases junk")
        XCTAssertEqual(try! Data(contentsOf: store.fileURL), before,
                       "failed refresh commits nothing")

        // A genuine 404/notReady old gateway still converges (junk retires).
        api.conditionalListError = nil
        api.quarantineSupported = false
        api.threads = [threadRow(Self.keyZ, Self.zhang)]
        _ = await inbox.refreshThreads()
        XCTAssertTrue(inbox.junkThreads.isEmpty, "old gateway retires junk on SUCCESS only")
    }

    func testUnicodeAndLongBodiesRoundtripUnsplit() {
        let store = makeStore("unicode")
        let long = "张三李四" + String(repeating: "测试短信🎉", count: 60) + "尾行😀"
        let rec = message(id: "uni1", thread: Self.keyZ, peer: Self.zhang, body: long)
        store.recordMessages([rec], for: Self.keyZ, validatorUpdates: [:], savedAt: Date())
        store.recordThreads(threads: [MessageThread(key: Self.keyZ, peer: Self.zhang,
                                                    unreadCount: 0, lastMessage: rec)],
                            junk: nil, hasMoreKeys: nil, validatorUpdates: [:], savedAt: Date())
        store.flush()
        let back = freshStore(store).snapshotForHydrate()
        XCTAssertEqual(back?.messages[Self.keyZ]?.first?.body, long)
        XCTAssertEqual(back?.threads.first?.lastMessage.body, long)
        XCTAssertFalse(long.hasSuffix("…"), "the app-side body is the FULL text, not a clipped preview")
    }

    // MARK: J. CloudKit independence

    func testStoreIsApplicationSupportAndExcludesCloudRows() {
        guard case .success(let origin) = GatewayOrigin.validate("https://ck.test", apiVersion: "v2") else {
            return XCTFail("origin")
        }
        let scope = HistoryScope.identifier(origin: origin, gatewayID: "g", deviceID: "d")!
        let store = GatewayHistoryStore(scopeIdentifier: scope)!
        defer { store.purge() }
        XCTAssertTrue(store.fileURL.path.contains("/Library/Application Support/CallRelay/history/"))
        XCTAssertFalse(store.fileURL.path.contains("CloudKit"),
                       "local persistence is never tied to iCloud containers")

        let defaults = suiteDefaults()
        let api = FakeGatewayAPI()
        api.threads = [threadRow(Self.keyZ, Self.zhang)]
        let inbox = MessageInbox(api: api, filter: nil, tombstoneDefaults: defaults, historyStore: store)
        api.threadPages[Self.keyZ] = [message(id: "live1", thread: Self.keyZ, peer: Self.zhang)]
        inbox.openThread(Self.keyZ)
        let expectation = self.expectation(description: "page")
        Task { @MainActor in
            await waitUntil { inbox.rows(for: Self.keyZ).contains { $0.id == "live1" } }
            inbox.applyCloudMessages([SyncedMessage(id: "scope.cloud1", gatewayScope: "scope",
                                                    threadKey: Self.keyZ, peer: Self.zhang,
                                                    body: "云端恢复的历史内容", direction: "inbound",
                                                    status: "read", createdAt: 4_000,
                                                    updatedAt: Date(timeIntervalSince1970: 4))],
                                     scope: "scope")
            await waitUntil { inbox.rows(for: Self.keyZ).contains { $0.id.hasPrefix("cloud:") } }
            inbox.invalidate()
            let snapshot = freshStore(store).snapshotForHydrate()
            let storedIDs = Set(snapshot?.messages[Self.keyZ]?.map(\.id) ?? [])
            XCTAssertFalse(storedIDs.contains { MessageInbox.isCloudRecordID($0) },
                           "CloudKit-restored rows keep their own durable path; disk cache is independent")
            XCTAssertTrue(storedIDs.contains("live1"))
            expectation.fulfill()
        }
        waitForExpectations(timeout: 5)
    }

    // MARK: K. Page gap retention

    func testGapRevalidationRefreshesNewestPageAndPreservesOlderLoadedRows() async {
        let store = makeStore("pagegap")
        let defaults = suiteDefaults()
        let api = FakeGatewayAPI()
        api.threads = [threadRow(Self.keyZ, Self.zhang)]
        api.threadPages[Self.keyZ] = [
            message(id: "n2", thread: Self.keyZ, peer: Self.zhang, createdAt: 5_200),
            message(id: "n1", thread: Self.keyZ, peer: Self.zhang, createdAt: 5_100),
        ]
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: defaults)
        _ = await inbox.refreshThreads()
        inbox.openThread(Self.keyZ)
        await waitUntil { inbox.rows(for: Self.keyZ).count == 2 }

        // Known gap: cached pages need bounded revalidation, and the newer
        // newest-page body MERGES over retained older pages (never replaces).
        inbox.markLoadedPagesStale()
        api.threadPages[Self.keyZ] = [
            message(id: "n4", thread: Self.keyZ, peer: Self.zhang, createdAt: 5_900),
            message(id: "n3", thread: Self.keyZ, peer: Self.zhang, createdAt: 5_800),
            message(id: "n2", thread: Self.keyZ, peer: Self.zhang, createdAt: 5_200),
            message(id: "n1", thread: Self.keyZ, peer: Self.zhang, createdAt: 5_100),
        ]
        _ = await inbox.refreshOpenThread(Self.keyZ)
        XCTAssertEqual(inbox.rows(for: Self.keyZ).count, 4, "older loaded pages are preserved")
        XCTAssertEqual(api.pageFetchFullBodyCount, 2, "one bounded newest-page fetch per change")

        // Unchanged afterwards: 304, zero body.
        _ = await inbox.refreshOpenThread(Self.keyZ)
        XCTAssertEqual(api.pageFetch304Count, 1)
        XCTAssertEqual(api.pageFetchFullBodyCount, 2)

        // The merged page is durable across restart, offline.
        inbox.invalidate()
        let offline = UnreachableAPI()
        let inbox2 = makeInbox(api: offline, store: freshStore(store), tombstoneDefaults: defaults)
        inbox2.openThread(Self.keyZ)
        XCTAssertEqual(inbox2.rows(for: Self.keyZ).count, 4)
    }

    // MARK: L. Bounded paging across the durable cap (root invariant)

    func testFullyLoadedConversationTrimmedToCapKeepsOlderHistoryReachableAcrossRestart() async {
        let store = makeStore("cap")
        let defaults = suiteDefaults()
        // 260 fictional 张三 records, FULLY loaded (server says no more).
        let all = (1...260).map { i in
            message(id: String(format: "m%03d", i), thread: Self.keyZ, peer: Self.zhang,
                    body: "张三记录\(i)", createdAt: Int64(1_000 + i))
        }
        let api = FakeGatewayAPI()
        api.threads = [threadRow(Self.keyZ, Self.zhang)]
        api.threadPages[Self.keyZ] = all.reversed() // newest-first, like the gateway
        api.threadHasMore = false
        let inbox = makeInbox(api: api, store: store, tombstoneDefaults: defaults)
        _ = await inbox.refreshThreads()
        inbox.openThread(Self.keyZ)
        await waitUntil { inbox.rows(for: Self.keyZ).count == 260 }
        XCTAssertFalse(inbox.hasMoreThreads.contains(Self.keyZ))

        // A later locally-driven flush carries in-memory hasMore=false; the
        // durable snapshot must STILL keep the truncated range marked: rows
        // beyond the 250 cap exist on the server and must stay reachable.
        inbox.apply(eventMessage: message(id: "m261", thread: Self.keyZ,
                                          peer: Self.zhang, createdAt: 2_000))
        await waitUntil { inbox.rows(for: Self.keyZ).count == 261 }
        let snapshot = freshStore(store).snapshotForHydrate()
        XCTAssertEqual(snapshot?.messages[Self.keyZ]?.count, HistoryCache.maxMessagesPerThread,
                       "durable pages are capped at the newest 250")
        XCTAssertEqual(snapshot?.truncatedKeys, [Self.keyZ])
        XCTAssertEqual(snapshot?.hasMoreKeys, [Self.keyZ],
                       "sticky truncated marker survives a hasMore=false flush")

        // Restart + UNCHANGED server: the reopen 304s (zero body) yet keeps
        // "load older" available from the cached oldest cursor.
        let api2 = FakeGatewayAPI()
        api2.threads = api.threads
        api2.threadPages = [Self.keyZ: all.reversed()]
        api2.threadHasMore = false
        let inbox2 = makeInbox(api: api2, store: freshStore(store), tombstoneDefaults: defaults)
        inbox2.openThread(Self.keyZ)
        await waitUntil { inbox2.openPhase(for: Self.keyZ) == .loaded }
        XCTAssertEqual(inbox2.rows(for: Self.keyZ).count, HistoryCache.maxMessagesPerThread)
        XCTAssertTrue(inbox2.hasMoreThreads.contains(Self.keyZ),
                      "hasMore survives restart + unchanged 304 for a capped thread")
        XCTAssertEqual(api2.pageFetch304Count, 1, "reopen costs a 304, not a full body")
        inbox2.loadOlder()
        await waitUntil { inbox2.rows(for: Self.keyZ).count == 261 }
        XCTAssertEqual(inbox2.rows(for: Self.keyZ).first?.id, "m001",
                       "the evicted older history is re-loadable from the server")
    }

    // MARK: Statics: transport shape + recents reconciliation

    func testReconcileCallListKeepsTerminalRecordsAgainstStaleResponse() {
        let live = callRecord(id: "c-old", peer: Self.zhang, finished: false, state: .incomingRinging)
        let ended = callRecord(id: "c-old", peer: Self.zhang, finished: true)
        let out = AppModel.reconcileCallList(server: [live], local: [ended], terminalIds: ["c-old"])
        XCTAssertEqual(out.first?.endedAt, ended.endedAt,
                       "a pre-event ringing copy may not resurrect an ended call")
        let fresh = AppModel.reconcileCallList(server: [ended], local: [live], terminalIds: ["c-old"])
        XCTAssertEqual(fresh.first?.endedAt, ended.endedAt, "server truth still wins when current")
        let untouched = AppModel.reconcileCallList(server: [live], local: [live], terminalIds: [])
        XCTAssertEqual(untouched, [live])
    }
}

// MARK: - Test doubles

/// An API that throws like an unreachable gateway (records attempts).
@MainActor
final class UnreachableAPI: FakeGatewayAPI {
    private(set) var attemptedRequests = 0
    private func rejected() throws -> Never {
        attemptedRequests += 1
        throw APIError.network(URLError(.cannotConnectToHost))
    }
    override func listThreads() async throws -> [MessageThread] { try rejected() }
    override func listThreads(folder: String) async throws -> [MessageThread] { try rejected() }
    override func listThreadsConditional(folder: String?, lineId: String?) async throws
        -> ConditionalOutcome<[MessageThread]> { try rejected() }
    override func listThreadMessages(threadKey: String, beforeCreatedAt: Int64?,
                                     beforeID: String?, limit: Int) async throws -> ThreadMessagePage {
        try rejected()
    }
    override func listThreadMessagesConditional(threadKey: String, beforeCreatedAt: Int64?,
                                                beforeID: String?, limit: Int) async throws
        -> ConditionalOutcome<ThreadMessagePage> { try rejected() }
    override func listCalls(limit: Int) async throws -> [CallRecord] { try rejected() }
    override func listCallsConditional(limit: Int) async throws -> ConditionalOutcome<[CallRecord]> {
        try rejected()
    }
    override func markMessageRead(id: String, idempotencyKey: String) async throws { try rejected() }
    override func deleteThread(threadKey: String) async throws { try rejected() }
}

extension FakeGatewayAPI {
    /// Canonical keys the fake's conditional surfaces use (must stay aligned
    /// with `fakeValidatorKey`).
    static var threadsAllKey: String { "https://fake-gateway.test/api/v2/threads" }
    static func threadsLineKey(_ line: String) -> String {
        "https://fake-gateway.test/api/v2/threads?line=\(line)"
    }
    static func threadsFolderKey(_ folder: String) -> String {
        "https://fake-gateway.test/api/v2/threads?folder=\(folder)"
    }
}
