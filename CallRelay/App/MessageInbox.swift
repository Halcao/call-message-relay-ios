import Foundation
import Combine
import UIKit

/// UI-facing thread list load phase.
enum ThreadListPhase: Equatable {
    case loading
    case loaded
    case failed(String)
}

enum ThreadOpenPhase: Equatable {
    case idle
    case loading
    case loaded
    case failed(String)
}

/// A message in an open conversation: either an authoritative gateway record
/// or a local submission that has not yet been reflected back by a refresh.
enum MessageRow: Identifiable, Equatable {
    case record(MessageRecord)
    case pending(MessageOutboxEntry)

    var id: String {
        switch self {
        case .record(let message): return message.id
        case .pending(let entry): return entry.id
        }
    }

    var isOutbound: Bool {
        switch self {
        case .record(let message): return message.isOutbound
        case .pending: return true
        }
    }

    var date: Date {
        switch self {
        case .record(let message): return message.createdDate
        case .pending(let entry): return entry.createdAt
        }
    }

    var status: MessageStatus {
        switch self {
        case .record(let message): return message.status
        case .pending(let entry): return entry.status
        }
    }

    var body: String {
        switch self {
        case .record(let message): return message.body
        case .pending(let entry): return entry.body
        }
    }

    /// Only local submissions can fail/retry inline.
    var outboxEntry: MessageOutboxEntry? {
        if case .pending(let entry) = self { return entry }
        return nil
    }
}

/// A local SMS submission. `idempotencyKey` is generated once for the logical
/// message and reused on every retry, so a transport failure can never create
/// a duplicate SMS on the gateway (which dedupes on Idempotency-Key).
///
/// `lineID` is captured ONCE at creation from the conversation's line
/// preference and reused by every retry: a retry must never silently switch
/// to another line/number — only an explicit user change (new submission)
/// may use a different line.
///
/// `needsConfirmation` is set for entries recovered from disk after a
/// process restart: their gateway outcome is ambiguous, so a gateway
/// restart/migration must never auto-send them; only an explicit user
/// retry (which reuses the stable idempotency key) may submit them.
struct MessageOutboxEntry: Identifiable, Equatable, Codable {
    let id: String
    /// Canonical `lineID:peer` form; `var` solely so persisted legacy bare
    /// keys can be migrated at recovery (the idempotency key is untouched).
    var threadKey: String
    let to: String
    let body: String
    let idempotencyKey: String
    let sourceMessageID: String?
    /// The line this submission belongs to; immutable across retries.
    let lineID: String?
    var status: MessageStatus
    var isSending: Bool
    var errorText: String?
    var serverMessageID: String?
    var needsConfirmation: Bool
    let createdAt: Date

    init(threadKey: String, to: String, body: String, now: Date = Date(), sourceMessageID: String? = nil,
         lineID: String? = nil) {
        let key = UUID().uuidString
        self.id = "local-\(key)"
        self.threadKey = threadKey
        self.to = to
        self.body = body
        self.idempotencyKey = key
        self.sourceMessageID = sourceMessageID
        self.lineID = lineID
        self.status = .queued
        self.isSending = true
        self.needsConfirmation = false
        self.createdAt = now
    }

    enum CodingKeys: String, CodingKey {
        case id, threadKey, to, body, idempotencyKey, sourceMessageID, lineID
        case status, isSending, errorText, serverMessageID, needsConfirmation, createdAt
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        threadKey = try c.decode(String.self, forKey: .threadKey)
        to = try c.decode(String.self, forKey: .to)
        body = try c.decode(String.self, forKey: .body)
        idempotencyKey = try c.decode(String.self, forKey: .idempotencyKey)
        sourceMessageID = try c.decodeIfPresent(String.self, forKey: .sourceMessageID)
        lineID = try c.decodeIfPresent(String.self, forKey: .lineID)
        status = try c.decodeIfPresent(MessageStatus.self, forKey: .status) ?? .queued
        isSending = try c.decodeIfPresent(Bool.self, forKey: .isSending) ?? false
        errorText = try c.decodeIfPresent(String.self, forKey: .errorText)
        serverMessageID = try c.decodeIfPresent(String.self, forKey: .serverMessageID)
        needsConfirmation = try c.decodeIfPresent(Bool.self, forKey: .needsConfirmation) ?? true
        createdAt = try c.decodeIfPresent(Date.self, forKey: .createdAt) ?? Date()
    }
}

enum MessageSendError: Error, Equatable {
    case invalidRecipient
    case bodyEmpty
    case bodyTooLong
    case lineNotReady(String)

    var friendlyMessage: String {
        switch self {
        case .invalidRecipient: return "请输入收件人号码（不超过 32 个字符）。"
        case .bodyEmpty: return "请输入短信内容。"
        case .bodyTooLong: return "短信内容不能超过 10000 个字符。"
        case .lineNotReady(let message): return message
        }
    }
}

/// How a conversation is grouped, mirroring the system Messages filters.
enum SenderFolder: Equatable {
    case known
    case unknown
    case junk(reason: String)
}

/// Owns SMS threads, open conversations and the outbox. One instance per
/// paired/demo session; `invalidate()` cancels polling and generations all
/// in-flight requests so a late response after unpair/mode switch is dropped.
@MainActor
final class MessageInbox: ObservableObject {
    @Published private(set) var threads: [MessageThread] = []

    // MARK: Pending user intent (build-67 responsiveness)
    //
    // The gateway persists deletes/reads a round-trip (or several) after the
    // user's action, and on cellular that round-trip is what made these
    // management operations feel nothing like native. These sets carry the
    // user's CLEAR intent in the meantime: a pending delete hides its row
    // immediately, a pending read clears the unread badge immediately.
    // Neither is durable until the gateway accepts: a rejection removes the
    // key (the row/badge returns from server truth) and is reported.
    @Published private(set) var pendingDeleteKeys: Set<String> = []
    @Published private(set) var pendingReadKeys: Set<String> = []
    /// One read-mark action's fence (build-67 correctness). The unread badge
    /// is never derived from a COUNT alone: the thread's newest-message
    /// WATERMARK captured at the action separates the old unread the user
    /// marked from genuinely new messages, and the mutation revision fences
    /// list GETs that were begun before the gateway acknowledged the mark.
    private struct ReadAction {
        /// Newest message (createdAt + id) at the moment the user acted. A
        /// refresh whose newest message still matches it can only be
        /// reporting the OLD unread — not a new message.
        var baselineCreatedAt: Int64
        var baselineID: String
        /// How many old messages were marked (the badge contribution that
        /// may be hidden).
        var markedCount: Int
        /// Mutation revision at the ACK (0 while the mark is still pending).
        /// A list GET that STARTED before this revision carries a pre-ack
        /// server copy and must never overwrite the settled state.
        var ackRevision: UInt64
    }
    private var readActions: [String: ReadAction] = [:]
    /// Monotonic revision bumped on every read-mark ACK.
    private var mutationRevision: UInt64 = 0
    /// Mutating operations in flight per key (overlap guard): a key already
    /// being deleted/read is not submitted twice.
    private var deleteOperationsInFlight: Set<String> = []
    @Published private(set) var listPhase: ThreadListPhase = .loading
    @Published private(set) var threadCache: [String: [MessageRecord]] = [:]
    @Published private(set) var openPhases: [String: ThreadOpenPhase] = [:]
    @Published private(set) var hasMoreThreads: Set<String> = []
    @Published private(set) var outbox: [MessageOutboxEntry] = []
    @Published var openThreadKey: String?
    /// Per-thread junk match reason once a rule has fired (re-evaluated live).
    @Published private(set) var junkReasons: [String: String] = [:]
    /// Gateway-projected per-key junk (build 55). Non-empty only while the
    /// paired gateway serves `GET /threads?folder=junk`; old gateways keep
    /// the local classification fallback.
    @Published private(set) var gatewayJunkThreads: [MessageThread] = []
    /// Set by AppModel: true when the paired gateway serves per-key folders.
    var gatewayQuarantineAvailable: () -> Bool = { false }
    private var gatewayQuarantineActive = false
    /// Set by AppModel: true when the bound wire generation is the unified
    /// gateway (v2). v2 has no global `after` message cursor — the event
    /// stream is the catch-up channel — so `reconcile()` must not issue the
    /// no-op list call.
    var transportIsV2 = false
    /// Set by AppModel: true while the resumable event stream is OPEN, i.e.
    /// new messages/threads arrive as events. Read marks from other devices
    /// are NOT broadcast as events (gateway ReadMessage is silent), so this
    /// flag NEVER licenses permanently skipping revalidation — it only
    /// suppresses redundant FULL fetches on paths where a bounded conditional
    /// revalidation still runs (poll loop / gap / explicit refresh).
    var isEventStreamHealthy: () -> Bool = { false }

    @Published private(set) var actionSyncError: String?
    private var actionStore: SMSActionStore?
    private var actions: [SMSConversationAction] = []
    private var actionWorker: Task<Void, Never>?
    private var actionsEnabled = false
    private var actionLifecycleObserver: AnyCancellable?
    private var actionRetrySleep: Task<Void, Error>?
    private var actionWorkerToken = UUID()
    private var actionStoreLoadFailed = false
    private var smsActionsCapability: Bool?
    private var actionBackgroundTask: UIBackgroundTaskIdentifier = .invalid
    var conversationIsForeground: () -> Bool = { true }
    private let api: GatewayAPI
    private let filter: SpamFilterStore?
    private var outboxStore: OutboxStore?
    /// Durable scoped history cache (nil = ephemeral/demo sessions). Owns the
    /// thread list, loaded pages and paging flags on disk; hydrate at init is
    /// read-only (never marks reads, never rings, never hits the network).
    private let historyStore: GatewayHistoryStore?
    /// True once this session hydrated a non-empty snapshot: the UI renders
    /// from disk before (or without) any network answer.
    private(set) var hydratedFromDisk = false
    /// Loaded message pages whose freshness is not proven for THIS session
    /// (hydrated from disk, or invalidated by a stream gap). Opening or
    /// polling a stale page triggers ONE bounded conditional revalidation
    /// (If-None-Match; a 304 costs zero body bytes); a fresh page renders
    /// from cache with no request at all.
    private var staleCachedPages: Set<String> = []
    /// Durable cache of the gateway's newest-message watermark this device
    /// has actually seen (v1 `after` catch-up bound).
    private var persistedWatermark: Int64 = 0
    /// Durable conversation-delete horizons, scoped to the paired gateway.
    /// The gateway tombstone is authoritative for live data, but CloudKit-
    /// restored history has no knowledge of it: without a durable local copy
    /// a deleted conversation reappears after a restart/cloud refresh.
    private let tombstoneStore: ThreadTombstoneStore
    /// Owner-controlled contact whitelist hook (contacts never enter the
    /// spam engine otherwise).
    var isTrustedContact: ((String) -> Bool)?
    /// Updated by AppModel with real SMS-line readiness.
    var lineReady: () -> Bool = { true }
    /// Per-entry readiness: whether the line CAPTURED in an outbox entry
    /// can send right now. A healthy second line must not be blocked just
    /// because the app default is offline, and vice versa.
    var lineReadyForEntry: ((String?) -> Bool)?
    /// Unified gateway: default line used when a conversation has no
    /// explicit per-thread preference.
    var lineIdProvider: (() -> String?)?
    /// Every line id the bound session is authorized for, in display order.
    /// Used ONLY to resolve legacy unqualified keys against an explicit
    /// authorized line (default first) — never to guess across servers.
    var authorizedLineIDsProvider: (() -> [String])?
    /// Per-conversation outgoing line preference (threadKey -> lineId).
    /// New submissions capture it into the entry; retries reuse the
    /// captured line and never silently fall back to another number.
    var lineIdForThread: ((String) -> String?)?
    private var lineFilter: String?

    private var generation: UInt64 = 0
    private var pollTask: Task<Void, Never>?
    private let pageLimit = 50
    private let now: () -> Date
    private var readKeys: Set<String> = []
    private var isValid = true
    /// Message ids restored read-only from CloudKit. They can never trigger a
    /// gateway send/dial/read-mark and are purged on account change/logout.
    private var cloudMessageIDs: Set<String> = []

    /// Marker in `gatewayID` for restored records (never a real gateway id).
    static let cloudGatewayMarker = "cloud-restore"

    init(
        api: GatewayAPI,
        filter: SpamFilterStore? = nil,
        outboxStore: OutboxStore? = nil,
        tombstoneScope: String? = nil,
        tombstoneDefaults: UserDefaults = .standard,
        historyStore: GatewayHistoryStore? = nil,
        actionStore: SMSActionStore? = nil,
        now: @escaping () -> Date = Date.init
    ) {
        self.api = api
        self.filter = filter
        self.outboxStore = outboxStore
        self.historyStore = historyStore
        self.actionStore = actionStore
        self.tombstoneStore = ThreadTombstoneStore(scope: tombstoneScope, defaults: tombstoneDefaults)
        self.now = now
        self.tombstonedThreadKeys = tombstoneStore.load()
        recoverPersistedOutbox()
        hydrateFromDisk()
        do { actions = try actionStore?.load() ?? []; restoreActionOverlay() }
        catch { actionStoreLoadFailed = true; actionSyncError = "无法读取待同步操作，请重试。" }
        actionLifecycleObserver = NotificationCenter.default.publisher(for: UIApplication.didBecomeActiveNotification)
            .sink { [weak self] _ in
                Task { @MainActor in
                    guard let self else { return }
                    self.wakeActionSync()
                    if let key = self.openThreadKey { self.markUnreadReadIfNeeded(self.threadCache[key] ?? []) }
                }
            }
    }

    /// Read-only restore of the durable snapshot into display state. Pure
    /// data: NO `markUnreadReadIfNeeded`, NO reconcile/refresh, NO CallKit —
    /// hydrate may run offline and must never side-effect the gateway.
    private func hydrateFromDisk() {
        guard let snapshot = historyStore?.snapshotForHydrate() else { return }
        let liveThreads = snapshot.threads.filter {
            !isTombstoned($0.key, newestMessageAt: $0.lastMessage.createdAt)
        }
        let liveJunk = snapshot.junk.filter {
            !isTombstoned($0.key, newestMessageAt: $0.lastMessage.createdAt)
        }
        var pages: [String: [MessageRecord]] = [:]
        for (key, records) in snapshot.messages {
            let kept = records.filter {
                !MessageInbox.isCloudRecordID($0.id)
                    && !isTombstoned(key, newestMessageAt: records.last?.createdAt ?? 0)
            }
            if !kept.isEmpty {
                pages[key] = kept
                staleCachedPages.insert(key)
            }
            if let newest = kept.last?.createdAt { persistedWatermark = max(persistedWatermark, newest) }
        }
        if snapshot.threads.isEmpty && snapshot.junk.isEmpty && pages.isEmpty { return }
        threads = liveThreads
        gatewayJunkThreads = liveJunk
        gatewayQuarantineActive = !liveJunk.isEmpty
        threadCache = pages
        hasMoreThreads = Set(snapshot.hasMoreKeys).intersection(pages.keys)
        listPhase = .loaded
        hydratedFromDisk = true
        // Local reclassification only (no network).
        reevaluateAll()
    }

    /// Called by AppModel when the event stream (re)opens after a gap or the
    /// app returns to foreground with cached pages: proven-fresh becomes
    /// "needs one bounded conditional revalidation" again.
    func markLoadedPagesStale() {
        staleCachedPages.formUnion(threadCache.keys)
    }

    var loadedPageKeysForTest: Set<String> { staleCachedPages }

    /// Switches the SMS history/filter scope to one authorized line (nil = all).
    func setLineFilter(_ lineId: String?) {
        lineFilter = lineId
        Task { await reconcile() }
    }

    /// Which query variant the displayed list currently represents. The flat
    /// `threads`/`junk` body can only be coherent with ONE variant at a time;
    /// a validator from another variant (a different line filter, or the
    /// folders projection vs a line list) must NEVER revalidate it — an
    /// A→B→A flip or a relaunch under a different filter could otherwise 304
    /// and keep the wrong line's rows forever (root body/validator review).
    private var intendedListSurfaceTag: String {
        gatewayQuarantineAvailable()
            ? "folders"
            : "line:" + (lineFilter ?? "all")
    }
    private static func lineListSurfaceTag(_ filter: String?) -> String {
        "line:" + (filter ?? "all")
    }

    // MARK: Lifecycle

    func start(pollInterval: TimeInterval = 20) {
        guard isValid else { return }
        generation += 1
        actionsEnabled = true
        wakeActionSync()
        // A session that already hydrated from disk renders immediately; its
        // catch-up runs through the bounded conditional revalidation below
        // instead of a redundant full bootstrap. A cold (no cache) session
        // still reconciles eagerly.
        if !hydratedFromDisk {
            Task { await reconcile() }
        }
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: UInt64(pollInterval * 1_000_000_000))
                guard !Task.isCancelled else { return }
                // Lightweight periodic revalidation. It is CONDITIONAL
                // (If-None-Match/304) and intentionally NOT skipped while the
                // event stream is healthy: the gateway does not broadcast
                // read marks, so this bounded check is what converges another
                // device's reads. Unchanged data costs zero body bytes.
                await self?.refreshThreads()
                if let key = self?.openThreadKey { await self?.refreshOpenThread(key) }
            }
        }
    }

    func invalidate() {
        // Settle the durable cache BEFORE bumping the generation: everything
        // captured by this still-valid session flushes to disk; anything that
        // races after the bump is fenced out and can never write.
        flushHistory()
isValid = false
        actionWorkerToken = UUID()
        actionWorker?.cancel(); actionWorker = nil; actionRetrySleep?.cancel()
        endActionBackgroundTask()
        generation += 1
        // A pairing change aborts every pending intent: no optimistic badge
        // or hidden row may outlive the session that created it.
        abortPendingReads(Array(pendingReadKeys))
        pendingDeleteKeys.removeAll()
        deleteOperationsInFlight.removeAll()
        readActions.removeAll()
        pollTask?.cancel()
        pollTask = nil
    }

    // MARK: Durable history

    /// Generation-fenced durable list commit with the coherence rule from
    /// root's durability review:
    ///
    /// * SETTLED write (a refresh returned fresh server bodies): the caller
    ///   passes those RAW bodies plus the ETags the gateway issued for them;
    ///   disk always stores body↔validator pairs matching the server. Local
    ///   optimistic masks (pendingRead/pendingDelete) stay in-memory only —
    ///   if the process dies with an unresolved mutation, the durable copy is
    ///   still the raw server truth, never the masked one.
    /// * LOCAL-TRANSFORM write (event row, delete applied, read ACK): the
    ///   current display state is persisted but ALL list-surface validators
    ///   are dropped. A transformed body must never be retained under a raw
    ///   server validator, because after a kill/recreate an unchanged-server
    ///   revalidation could answer 304 and lock the wrong unread badge or a
    ///   hidden row in forever. The next settled GET then runs as a full
    ///   fetch and re-establishes the coherent pair. Deleted rows remain
    ///   hidden offline through the independent durable tombstone horizon, so
    ///   there is no optimistic-resurrection flash, and genuinely-new event
    ///   rows are server-true content the next fetch carries anyway.
    /// * A pure 304 refresh with no local change commits NOTHING: the last
    ///   coherent body+ETag on disk is still exactly right (this is what
    ///   keeps repeated unchanged opens at zero body bytes).
    private func persistHistory(flush: Bool, captured: UInt64,
                                settledNormal: [MessageThread]? = nil,
                                settledJunk: [MessageThread]? = nil,
                                validatorUpdates: [String: String?] = [:],
                                localTransform: Bool = false,
                                listSurfaceTag: String? = nil) {
        guard isValid, captured == generation, let historyStore else { return }
        if localTransform {
            historyStore.recordThreads(
                threads: actionStore == nil ? threads.filter { !isPendingDeletion($0.key) } : threads,
                junk: gatewayJunkThreads,
                hasMoreKeys: hasMoreThreads,
                validatorUpdates: [:],
                clearListValidators: true,
                savedAt: now()
            )
        } else {
            // SETTLED mode: write ONLY the folder/list datasets that this
            // refresh actually re-fetched. A 304'd dataset keeps its on-disk
            // body AND its validator untouched — overwriting it with the
            // masked display copy while retaining its ETag (root review:
            // mixed normal-304 + junk-200 under a pending read/delete) is
            // exactly the lock-in class this store must never create.
            if settledNormal == nil && settledJunk == nil { return }
            historyStore.recordThreads(
                threads: settledNormal,
                junk: settledJunk,
                hasMoreKeys: hasMoreThreads,
                validatorUpdates: validatorUpdates,
                listSurfaceTag: listSurfaceTag,
                savedAt: now()
            )
        }
        if flush { historyStore.flush() }
    }

    /// Commit one thread's loaded page (live records only — CloudKit-restored
    /// rows keep their own durable path) with the validator that guarded it.
    private func persistPage(_ key: String, captured: UInt64,
                             validator: HistoryValidator? = nil,
                             flush: Bool) {
        guard isValid, captured == generation, let historyStore else { return }
        let records = (threadCache[key] ?? [])
            .filter { !MessageInbox.isCloudRecordID($0.id) }
        var updates: [String: String?] = [:]
        if let validator { updates[validator.key] = validator.etag }
        historyStore.recordMessages(records, for: key,
                                    validatorUpdates: updates, savedAt: now())
        if flush { historyStore.flush() }
    }

    private func flushHistory() {
        historyStore?.flush()
    }

    /// Newest live-message timestamp held in the per-thread cache; bounds the
    /// v1 `after:` catch-up so a gap never re-downloads the whole history.
    private var cachedMessageWatermark: Int64 {
        var maxAt = persistedWatermark
        for (_, records) in threadCache {
            for record in records where !MessageInbox.isCloudRecordID(record.id) {
                maxAt = max(maxAt, record.createdAt)
            }
        }
        return maxAt
    }

    // MARK: Threads

    /// Conversation tombstones this device has applied (local deletes and
    /// `thread.deleted` events from other devices). A tombstoned thread stays
    /// hidden until a NEWER gateway message reopens it; its pre-delete
    /// history is not re-shown by cloud restores either.
    private var tombstonedThreadKeys: [String: Int64] = [:]

    /// Applies a conversation delete locally (optimistic, after the gateway
    /// accepted it): drops the thread row, prunes its cached messages and
    /// marks the tombstone so cloud restores cannot resurrect it. The
    /// horizon is persisted per gateway, because restored CloudKit history
    /// outlives the session and has no server-side thread tombstone.
    func applyThreadDeleted(key: String, deletedAt: Int64? = nil, throughID: String? = nil) {
        if let deletedAt, let throughID {
            let action = SMSConversationAction(id: UUID().uuidString, kind: .delete, threadKey: key,
                throughCreatedAt: deletedAt, throughID: throughID, completed: true)
            var next = actions; next.append(action)
            do { try actionStore?.save(next); actions = next }
            catch { actionSyncError = "无法保存删除状态，请重试。"; return }
            applyBoundedDeletion(action); restoreActionOverlay()
            persistHistory(flush: true, captured: generation, localTransform: true)
            return
        }
        pendingDeleteKeys.remove(key)
        threads.removeAll { $0.key == key }
        gatewayJunkThreads.removeAll { $0.key == key }
        threadCache.removeValue(forKey: key)
        hasMoreThreads.remove(key)
        staleCachedPages.remove(key)
        tombstonedThreadKeys[key] = deletedAt ?? Date().unixMilliseconds
        tombstoneStore.save(tombstonedThreadKeys)
        reevaluateAll()
        // Durable: the removed thread AND its cached page disappear together.
        if let historyStore {
            historyStore.removeMessages(forKey: key, savedAt: now())
        }
        persistHistory(flush: true, captured: generation, localTransform: true)
    }

    /// Durable tombstone horizons for tests and diagnostics (read-only copy).
    var persistedTombstoneHorizons: [String: Int64] { tombstonedThreadKeys }

    /// Marks every inbound unread live message in each conversation as read.
    /// Marks a conversation delete as PENDING (user intent, not durable):
    /// the row hides immediately; `cancelPendingDeletion` restores it when
    /// the gateway rejects.
    func beginPendingDeletion(_ key: String) {
        pendingDeleteKeys.insert(key)
    }

    func cancelPendingDeletion(_ key: String) {
        pendingDeleteKeys.remove(key)
    }

    func isPendingDeletion(_ key: String) -> Bool {
        guard pendingDeleteKeys.contains(key) else { return false }
        guard let action = actions.last(where: { $0.threadKey == key && $0.kind == .delete && !$0.failed }),
              let latest = (threads + gatewayJunkThreads).first(where: { $0.key == key })?.lastMessage else { return true }
        return action.covers(latest)
    }

    func isPendingRead(_ key: String) -> Bool {
        pendingReadKeys.contains(key)
    }

    /// Bulk delete: EVERY selected row is marked pending BEFORE the first
    /// network call (waiting sequentially made later rows look unresponsive),
    /// then each key is submitted once and settled honestly (accepted rows
    /// stay hidden with a durable tombstone; rejected rows return).
    func deleteThreads(_ keys: [String]) async -> BulkOperationResult {
        if actionStore != nil { return enqueueActions(keys, kind: .delete) }
        var result = BulkOperationResult()
        var seen = Set<String>()
        let unique = keys.filter { seen.insert($0).inserted }
        for key in unique { beginPendingDeletion(key) }
        for key in unique {
            if await deleteThread(key) {
                result.succeeded.append(key)
            } else {
                result.failed.append(key)
            }
        }
        return result
    }

    /// Deletes one conversation THROUGH the gateway with pending user intent:
    /// the row hides immediately (`pendingDeleteKeys`), the durable tombstone
    /// is applied only after the gateway accepted, and a rejection restores
    /// the row. Returns false without touching the gateway when the session
    /// is invalid/offline or a newer pairing superseded the operation.
    @discardableResult
    func deleteThread(_ key: String) async -> Bool {
        guard isValid else { return false }
        if actionStore != nil { return enqueueActions([key], kind: .delete).failed.isEmpty }
        // Overlap guard: the same key is never submitted twice (the row is
        // already pending from the first request).
        guard !deleteOperationsInFlight.contains(key) else { return true }
        deleteOperationsInFlight.insert(key)
        defer { deleteOperationsInFlight.remove(key) }
        let captured = generation
        beginPendingDeletion(key)
        do {
            try await api.deleteThread(threadKey: key)
            guard captured == generation else { return false }
            applyThreadDeleted(key: key)
            return true
        } catch {
            guard captured == generation else { return false }
            cancelPendingDeletion(key)
            return false
        }
    }

    /// Display badge for one thread, fenced by the read actions (if any).
    /// `fetchRevision` is the mutation revision at the moment the caller's
    /// list GET STARTED (the live UI passes the current revision).
    ///
    /// Rules (build-67 review):
    /// * a GET begun before the ACK must not overwrite the settled state —
    ///   the local post-ack state wins;
    /// * the same newest-message watermark as the action ⇒ every unread the
    ///   copy reports is one the user marked ⇒ nothing new to show;
    /// * a NEWER watermark ⇒ a new message exists ⇒ it stays unread, with
    ///   at least 1 shown even when the total count equals the pre-mark
    ///   count (same count, different newest id/timestamp).
    private func maskedUnread(_ thread: MessageThread, fetchRevision: UInt64) -> Int {
        if let intent = actions.last(where: { $0.threadKey == thread.key && $0.kind == .read && !$0.failed }) {
            return intent.covers(thread.lastMessage) ? 0 : max(1, thread.unreadCount - intent.unreadCount)
        }
        guard let action = readActions[thread.key] else { return thread.unreadCount }
        if action.ackRevision > fetchRevision {
            // Pre-ack copy: the settled local state is authoritative.
            return 0
        }
        if thread.lastMessage.createdAt == action.baselineCreatedAt,
           thread.lastMessage.id == action.baselineID {
            return 0
        }
        return max(1, thread.unreadCount - action.markedCount)
    }

    /// Optimistically hides one thread's STALE unread (intent shown while
    /// the gateway persists); returns the captured previous count.
    @discardableResult
    private func clearUnreadOptimistically(_ key: String, thread: MessageThread) -> Int {
        let previous = thread.unreadCount
        readActions[key] = ReadAction(
            baselineCreatedAt: thread.lastMessage.createdAt,
            baselineID: thread.lastMessage.id,
            markedCount: previous,
            ackRevision: 0)
        if let index = threads.firstIndex(where: { $0.key == key }) {
            threads[index].unreadCount = maskedUnread(threads[index], fetchRevision: mutationRevision)
        }
        pendingReadKeys.insert(key)
        return previous
    }

    /// Restores the badge captured at the pending mark (a rejection must
    /// never leave a fake "read" state behind). Unread that arrived while
    /// the mark was pending is preserved on top of the restored count.
    private func restoreUnreadAfterRejection(_ key: String) {
        let stale = readActions[key]?.markedCount ?? 0
        defer {
            pendingReadKeys.remove(key)
            readActions[key] = nil
        }
        guard stale > 0 else { return }
        if let index = threads.firstIndex(where: { $0.key == key }) {
            threads[index].unreadCount = max(threads[index].unreadCount, stale)
        }
    }

    /// Clears every still-pending read key (abort/invalidation): no
    /// permanent optimistic badge may survive a cancelled operation.
    private func abortPendingReads(_ keys: [String]) {
        for key in keys where pendingReadKeys.contains(key) {
            restoreUnreadAfterRejection(key)
        }
    }

    /// Existing per-message endpoint only: no backend rewrite. NOTE (verified
    /// against the gateway): `ReadMessage` does NOT broadcast any event, so
    /// other devices converge through the bounded CONDITIONAL list/page
    /// revalidation (the poll loop + gap/foreground paths), never through a
    /// read event — which is why revalidation must stay enabled even while
    /// the event stream is healthy. Returns honest per-thread results —
    /// already-read messages are not re-marked, and a thread whose page fetch
    /// or a mark request fails is reported as failed rather than dropped.
    func markThreadsRead(_ keys: [String]) async -> BulkOperationResult {
        if actionStore != nil { return enqueueActions(keys, kind: .read) }
        let captured = generation
        var result = BulkOperationResult()
        // A key already pending is not re-submitted (overlapping taps are
        // one operation), and its badge already reads as cleared.
        // Dedupe + skip keys already pending (overlapping taps are ONE
        // operation, never a re-submission).
        var seen = Set<String>()
        let fresh = keys.filter { seen.insert($0).inserted && !pendingReadKeys.contains($0) }
        var expectedUnread: [String: Int] = [:]
        for key in fresh {
            guard let thread = threads.first(where: { $0.key == key }) else {
                expectedUnread[key] = 1
                pendingReadKeys.insert(key)
                readActions[key] = ReadAction(baselineCreatedAt: 0, baselineID: "",
                                              markedCount: 1, ackRevision: 0)
                continue
            }
            expectedUnread[key] = clearUnreadOptimistically(key, thread: thread)
        }
        for key in fresh {
            do {
                _ = try await markThreadRead(key, expectedUnread: expectedUnread[key] ?? 1)
                pendingReadKeys.remove(key)
                // The ACK bumps the mutation revision: any list GET that
                // STARTED before it must not overwrite this settled state.
                mutationRevision &+= 1
                readActions[key]?.ackRevision = mutationRevision
                result.succeeded.append(key)
                // Settle point: the masked (read) list state is durable the
                // moment the gateway ACKs — a restart keeps the cleared
                // badge until the SERVER says otherwise, not a re-fetch race.
                persistHistory(flush: true, captured: captured, localTransform: true)
            } catch is CancellationError {
                restoreUnreadAfterRejection(key)
                result.failed.append(key)
                // Pairing invalidation/cancel: no remaining key may keep a
                // permanent optimistic badge.
                abortPendingReads(fresh)
                for remaining in fresh
                where pendingReadKeys.contains(remaining) == false
                    && !result.succeeded.contains(remaining)
                    && !result.failed.contains(remaining) {
                    result.failed.append(remaining)
                }
                break
            } catch {
                restoreUnreadAfterRejection(key)
                result.failed.append(key)
            }
        }
        if !result.succeeded.isEmpty { await refreshThreads() }
        return result
    }

    /// Fetches and marks one conversation's unread inbound messages (bounded
    /// to four pages of history). A mid-thread failure leaves already-marked
    /// messages read and throws so the caller reports the thread honestly.
    private func markThreadRead(_ key: String, expectedUnread: Int? = nil) async throws -> Int {
        let captured = generation
        var before: MessageRecord?
        // The count captured BEFORE the optimistic clear drives the bounded
        // paging (the live thread's badge now reads 0 while pending).
        var remaining = max(1, expectedUnread
            ?? threads.first { $0.key == key }?.unreadCount ?? 1)
        var marked = 0
        while true {
            guard captured == generation else { throw CancellationError() }
            let page = try await api.listThreadMessages(
                threadKey: key,
                beforeCreatedAt: before?.createdAt, beforeID: before?.id, limit: pageLimit
            )
            guard captured == generation else { throw CancellationError() }
            let unread = page.messages.filter {
                $0.direction == .inbound && $0.status != .read && !Self.isCloudRecordID($0.id)
            }
            var updated: [MessageRecord] = []
            for message in unread {
                try await api.markMessageRead(id: message.id, idempotencyKey: UUID().uuidString)
                guard captured == generation else { throw CancellationError() }
                marked += 1
                remaining -= 1
                updated.append(message.with(status: .read))
            }
            if !updated.isEmpty { mergeMessages(updated) }
            guard page.hasMore, let oldest = page.messages.first, remaining > 0 else { break }
            before = oldest
        }
        return marked
    }

    /// True when `key` was deleted and no newer gateway message has
    /// reopened the conversation.
    private func isTombstoned(_ key: String, newestMessageAt: Int64) -> Bool {
        guard let horizon = tombstonedThreadKeys[key] else { return false }
        return newestMessageAt <= horizon
    }

    /// True for a gateway that genuinely does not serve per-key folders
    /// (old worker 404/405 or a protocol notReady), as opposed to a
    /// TRANSIENT failure (offline, gated session, 5xx, token race). Only the
    /// former may retire the quarantine projection; transient failures must
    /// keep the prior cached normal+junk exactly as-is (root review): a cold
    /// offline hydrate can hit a failed refresh while `harassment.support`
    /// is still unknown, and clearing then would durably erase hydrated
    /// junk rows that the next flush would persist as loss.
    private static func isFolderSurfaceUnsupported(_ error: Error) -> Bool {
        guard let apiError = error as? APIError else { return false }
        switch apiError {
        case .notReady:
            return true
        case .http(let status, _, _):
            return status == 404 || status == 405
        default:
            return false
        }
    }

    /// Conditional (ETag/304) thread-list refresh. A `.notModified` folder
    /// keeps the local copy — it is NEVER a failure and transfers zero body
    /// bytes — while a changed folder converges server truth, including read
    /// marks that the gateway does not broadcast as events. List/folder state
    /// is STAGED and committed only on a successful outcome, so any transient
    /// failure preserves the hydrated/cached normal + junk rows; success
    /// settles a mandatory durable flush (read/delete masking persisted).
    @discardableResult
    func refreshThreads() async -> Bool {
        guard isValid else { return false }
        let captured = generation
        // Fence anchor: any read-mark ACKed AFTER this fetch started makes
        // this response's badge for that key pre-ack (never authoritative).
        let fetchRevision = mutationRevision
        // Validators guarding the server bodies we are about to apply; they
        // are committed in the SAME snapshot write as those bodies.
        var listValidators: [String: String?] = [:]
        // RAW server bodies (pre-mask, pre-tombstone-filter) exactly as the
        // gateway returned them under the ETags we are committing. Only these
        // are durable; the masked/tombstone-filtered display copy stays in
        // memory. This is the root durability fix: never persist an optimis-
        // tically transformed body under the raw-server validator.
        var settledNormalRaw: [MessageThread]?
        var settledJunkRaw: [MessageThread]?
        // Variant coherence before ANY conditional send: if the persisted
        // list body was committed under a different query variant, its
        // list-surface ETags must not go back out (they would 304 the wrong
        // variant's body into being kept). One drop, then this fetch
        // re-establishes a coherent body+ETag+tag triple.
        let intendedTag = intendedListSurfaceTag
        if let storedTag = historyStore?.listSurfaceTag, storedTag != intendedTag {
            historyStore?.invalidateListValidators()
        }
        var servedTag = intendedTag
        do {
            // Staged list/folder changes — NOTHING below touches display
            // state until every request settled successfully.
            var loaded: [MessageThread]?
            var stagedJunk: [MessageThread]?
            var stagedQuarantineActive: Bool?
            if gatewayQuarantineAvailable() {
                do {
                    // Per-key folders from the gateway: the same authorized
                    // key can read its own quarantined text, restore is
                    // shared by same-key devices and another key is isolated.
                    let normal = try await api.listThreadsConditional(folder: "normal", lineId: nil)
                    let junk = try await api.listThreadsConditional(folder: "junk", lineId: nil)
                    guard captured == generation else { return false }
                    if case .changed(let threads, let validator) = normal {
                        loaded = threads
                        settledNormalRaw = threads
                        if let validator { listValidators[validator.key] = validator.etag }
                    }
                    if case .changed(let threads, let validator) = junk {
                        stagedJunk = threads
                            .filter { !isTombstoned($0.key, newestMessageAt: $0.lastMessage.createdAt) }
                            .sorted { $0.lastMessage.createdAt > $1.lastMessage.createdAt }
                        settledJunkRaw = threads
                        if let validator { listValidators[validator.key] = validator.etag }
                    }
                    stagedQuarantineActive = true
                    servedTag = "folders"
                } catch let error as APIError where Self.isFolderSurfaceUnsupported(error) {
                    // Genuinely folder-unsupported (old gateway): fall back to
                    // the local classifier. A TRANSIENT error does not come
                    // here — it rethrows to the outer catch and keeps prior
                    // cached rows (root review).
                    guard captured == generation else { return false }
                    let outcome = try await api.listThreadsConditional(folder: nil, lineId: lineFilter)
                    guard captured == generation else { return false }
                    if case .changed(let threads, let validator) = outcome {
                        loaded = threads
                        settledNormalRaw = threads
                        if let validator { listValidators[validator.key] = validator.etag }
                    }
                    stagedJunk = []
                    settledJunkRaw = []
                    stagedQuarantineActive = false
                    servedTag = Self.lineListSurfaceTag(lineFilter)
                }
            } else {
                let outcome = try await api.listThreadsConditional(folder: nil, lineId: lineFilter)
                guard captured == generation else { return false }
                if case .changed(let threads, let validator) = outcome {
                    loaded = threads
                    settledNormalRaw = threads
                    if let validator { listValidators[validator.key] = validator.etag }
                    // A fresh line-filtered body REPLACES any folder projection;
                    // on 304 the staged state stays untouched (the persisted
                    // pair already belongs to this exact variant).
                    stagedJunk = []
                    settledJunkRaw = []
                    stagedQuarantineActive = false
                    servedTag = Self.lineListSurfaceTag(lineFilter)
                }
            }
            guard captured == generation else { return false }
            // Commit the staged outcome atomically.
            if let stagedJunk { gatewayJunkThreads = stagedJunk }
            if let active = stagedQuarantineActive { gatewayQuarantineActive = active }
            // A pure-304 refresh (nothing changed) is a success with the
            // local copy kept: listPhase settles, no overwrite, no failure.
            if let loaded {
                // Server-side tombstones hide pre-delete threads (a newer
                // message reopens the conversation; the gateway already
                // filters, this covers the local-delete race before the next
                // fetch). The read-action fence is applied per thread: a copy
                // fetched before a mark's ACK cannot overwrite the settled
                // state, and a newer newest-message watermark always keeps
                // genuinely new unread.
                threads = loaded
                    .filter { !isTombstoned($0.key, newestMessageAt: $0.lastMessage.createdAt) }
                    .map { thread in
                        var copy = thread
                        copy.unreadCount = maskedUnread(thread, fetchRevision: fetchRevision)
                        return copy
                    }
                    .sorted { $0.lastMessage.createdAt > $1.lastMessage.createdAt }
                // Read actions the server has fully caught up with
                // (acknowledged copy reports zero unread) no longer need a
                // fence.
                for thread in loaded where thread.unreadCount == 0 {
                    if let action = readActions[thread.key], action.ackRevision != 0,
                       action.ackRevision <= fetchRevision {
                        readActions[thread.key] = nil
                    }
                }
                rebuildCloudThreads()
                reevaluateAll()
            }
            listPhase = .loaded
            // Settle point: a body refresh commits the RAW server lists with
            // the ETags that guard them (never the masked display copy);
            // a pure-304 with no local change commits nothing (disk already
            // holds the coherent pair), keeping unchanged repeats at zero
            // body bytes even across a process restart.
            persistHistory(flush: true, captured: captured,
                           settledNormal: settledNormalRaw,
                           settledJunk: settledJunkRaw,
                           validatorUpdates: listValidators,
                           listSurfaceTag: servedTag)
            return true
        } catch is CancellationError {
            return false
        } catch {
            guard captured == generation else { return false }
            // Transient failure: keep EVERY prior cached row (normal AND
            // junk); only surface a failure when there is nothing to show.
            if displayThreads.isEmpty, gatewayJunkThreads.isEmpty {
                listPhase = .failed(friendly(error))
            }
            return false
        }
    }

    /// True while the gateway's per-key folders are the source of truth.
    var isGatewayQuarantineActive: Bool { gatewayQuarantineActive }

    /// Catch-up after a WebSocket gap or foreground return. On v2 the
    /// gateway has no global `after` cursor (`listMessages` is a documented
    /// no-op) and the resumable event stream IS the message catch-up
    /// channel, so this skips the dead call and runs the bounded CONDITIONAL
    /// list revalidation instead — an unchanged list costs a 304, never a
    /// full download, and it still converges read marks that are not
    /// broadcast as events. On v1 the catch-up is bounded by the cached
    /// newest-message watermark (not from zero), so already-held history is
    /// never re-downloaded. Older history is fetched per-thread when the
    /// conversation is opened.
    @discardableResult
    func reconcile() async -> Bool {
        guard isValid else { return false }
        if transportIsV2 {
            return await refreshThreads()
        }
        let captured = generation
        do {
            let batch = try await api.listMessages(after: cachedMessageWatermark, limit: 200)
            guard captured == generation else { return false }
            if !batch.isEmpty { mergeMessages(batch) }
            reevaluateAll()
            return await refreshThreads()
        } catch is CancellationError {
            return false
        } catch {
            guard captured == generation else { return false }
            if threads.isEmpty { listPhase = .failed(friendly(error)) }
            return false
        }
    }

    /// Gateway threads plus not-yet-reflected outbox submissions, newest
    /// activity first, so a freshly sent message is visible immediately.
    /// Outbox entries are keyed canonically (`lineID:peer`), so a pending
    /// send now lands ON the gateway thread it belongs to instead of
    /// duplicating it with an unopenable bare-key row.
    var displayThreads: [MessageThread] {
        var result = threads.filter { !isPendingDeletion($0.key) }
            .map { thread in
                var copy = thread
                copy.unreadCount = maskedUnread(thread, fetchRevision: mutationRevision)
                return copy
            }
        for entry in outbox {
            if result.contains(where: { $0.key == entry.threadKey
                    || $0.key == ThreadKey.canonical(lineID: entry.lineID, peer: entry.to) }) { continue }
            let record = MessageRecord(
                id: entry.id, gatewayID: nil, lineID: entry.lineID,
                threadKey: entry.threadKey, direction: .outbound, peer: entry.to,
                body: entry.body, encoding: nil,
                status: entry.isSending ? .queued : entry.status,
                createdAt: entry.createdAt.unixMilliseconds
            )
            result.append(MessageThread(key: entry.threadKey, peer: entry.to, unreadCount: 0, lastMessage: record))
        }
        return result.sorted { $0.lastMessage.createdAt > $1.lastMessage.createdAt }
    }

    // MARK: Folders / spam

    /// CloudKit-restored threads the live gateway does not provide.
    private var visibleCloudThreads: [MessageThread] {
        cloudThreads.filter { !isHidden($0.key) && !isPendingDeletion($0.key) }
    }

    /// All visible threads split into the system-Messages-style groups.
    var knownThreads: [MessageThread] {
        displayThreads.filter { folder(for: $0) == .known && !isHidden($0.key) }
            + visibleCloudThreads.filter { folder(for: $0) == .known }
    }
    var unknownThreads: [MessageThread] {
        displayThreads.filter { if case .unknown = folder(for: $0) { return true }; return false }
            .filter { !isHidden($0.key) }
            + visibleCloudThreads.filter { if case .unknown = folder(for: $0) { return true }; return false }
    }
    var junkThreads: [MessageThread] {
        if gatewayQuarantineActive { return gatewayJunkThreads.filter { !isPendingDeletion($0.key) }.map { var row = $0; row.unreadCount = maskedUnread(row, fetchRevision: mutationRevision); return row } }
        return displayThreads.filter { if case .junk = folder(for: $0) { return true }; return false }
            .filter { !isHidden($0.key) }
            + visibleCloudThreads.filter { if case .junk = folder(for: $0) { return true }; return false }
    }

    func junkReason(for key: String) -> String? { junkReasons[key] }

    func folder(for thread: MessageThread) -> SenderFolder {
        let key = thread.key
        if isExplicitlyKnown(thread) { return .known }
        if let reason = junkReasons[key] { return .junk(reason: reason) }
        return .unknown
    }

    private func isExplicitlyKnown(_ thread: MessageThread) -> Bool {
        // Owner-marked known (explicit trust) wins over everything.
        if filter?.isKnownSender(thread.peer) == true { return true }
        // Contact trust applies AFTER explicit owner sender blocks: a contact
        // the owner deliberately blocked stays quarantined.
        if isTrustedContact?(thread.peer) == true,
           filter?.policy().hasSenderBlock(peer: thread.peer) != true {
            return true
        }
        // An actual conversation (any outbound message) means the owner knows
        // this sender; a single inbound promo does not.
        if let records = threadCache[thread.key], records.contains(where: { $0.isOutbound }) {
            return true
        }
        if thread.lastMessage.isOutbound { return true }
        return false
    }

    private func isHidden(_ key: String) -> Bool { filter?.isThreadHidden(key) == true }

    /// Re-run the policy over every loaded thread/cache so toggling a rule,
    /// restoring a sender or loading a paged mixed thread updates folders
    /// immediately without hiding a legitimate reply.
    func reevaluateAll() {
        // Gateway-managed quarantine: folders come from the server; local
        // reclassification must not fight the key's shared state.
        if gatewayQuarantineActive {
            junkReasons = [:]
            return
        }
        guard let filter else { return }
        let policy = filter.policy()
        var reasons: [String: String] = [:]
        var seen = Set<String>()
        for thread in displayThreads + cloudThreads {
            if seen.contains(thread.key) { continue }
            seen.insert(thread.key)
            if isExplicitlyKnown(thread) { continue }
            // Evaluate the latest inbound content available (cache preferred).
            let candidate = threadCache[thread.key]?
                .last(where: { !$0.isOutbound }) ?? (thread.lastMessage.isOutbound ? nil : thread.lastMessage)
            guard let message = candidate else { continue }
            if case .junk(let reason) = policy.classifySMS(
                peer: message.peer, body: message.body,
                isKnownSender: filter.isKnownSender(message.peer)
            ) {
                reasons[thread.key] = reason
            }
        }
        junkReasons = reasons
    }

    /// "标记为已知发件人": whitelist the sender permanently and re-evaluate.
    func markSenderKnown(peer: String) {
        filter?.markSenderKnown(peer)
        reevaluateAll()
    }

    /// Restore a quarantined thread back to the normal list. On a
    /// quarantine-capable gateway this writes the KEY-scoped restored state
    /// (shared by same-key devices, pinned so it cannot re-quarantine); the
    /// local known-sender mark is kept so a later old-gateway session still
    /// treats the sender as trusted.
    func restoreJunk(threadKey key: String, peer: String) {
        filter?.markSenderKnown(peer)
        if gatewayQuarantineActive {
            gatewayJunkThreads.removeAll { $0.key == key }
            objectWillChange.send()
            Task {
                do {
                    try await api.restoreThread(threadKey: key)
                } catch {
                    // Restore failed: reload so the UI reflects server truth.
                }
                _ = await refreshThreads()
            }
            return
        }
        reevaluateAll()
    }

    /// Dismiss a junk thread. On a quarantine-capable gateway this is a
    /// per-KEY delete (raw worker history untouched, another key unaffected);
    /// on old gateways it stays a local-only hide.
    func dismissJunk(threadKey key: String) {
        if gatewayQuarantineActive {
            gatewayJunkThreads.removeAll { $0.key == key }
            objectWillChange.send()
            Task {
                do {
                    try await api.deleteThread(threadKey: key)
                    applyThreadDeleted(key: key)
                } catch {
                    // Keep the optimistic removal; the next refresh converges.
                }
                _ = await refreshThreads()
            }
            return
        }
        filter?.hideThread(key)
        objectWillChange.send()
    }

    /// Opens a conversation. A cached page renders INSTANTLY — including with
    /// no gateway and no network — and gets exactly one bounded conditional
    /// revalidation (If-None-Match; an unchanged page costs zero body
    /// bytes). A page already proven fresh this session renders with no
    /// request at all. Read marking only ever follows user action; the
    /// hydrate path never marks reads, never rings and never notifies.
    func openThread(_ key: String) {
        openThreadKey = key
        let cached = threadCache[key] ?? []
        markUnreadReadIfNeeded(cached)
        if !cached.isEmpty {
            openPhases[key] = .loaded
            if staleCachedPages.contains(key), ThreadKey.isQualified(key) {
                Task { await revalidateOpenPage(key) }
            }
            return
        }
        Task { await loadThreadPage(key) }
    }

    /// Bounded freshness check of the OPENED page of a conversation: one
    /// conditional GET of the newest page only — older loaded pages and the
    /// paging cursor are preserved, and the whole history is never forced.
    /// A transient error keeps the rendered cache (never fails a loaded view).
    @discardableResult
    private func revalidateOpenPage(_ key: String) async -> Bool {
        guard isValid, ThreadKey.isQualified(key) else { return false }
        let captured = generation
        do {
            let outcome = try await api.listThreadMessagesConditional(
                threadKey: key, beforeCreatedAt: nil, beforeID: nil, limit: pageLimit)
            guard captured == generation else { return false }
            switch outcome {
            case .changed(let page, let validator):
                applyFetchedPage(page, displayKey: key, fetchKey: key,
                                 captured: captured, validator: validator)
                staleCachedPages.remove(key)
                return true
            case .notModified:
                // 304 proves the local copy current — unless it is missing
                // (torn/absent body despite a validator): converge with one
                // unconditional fetch instead of showing nothing.
                if threadCache[key]?.isEmpty ?? true {
                    return await loadThreadPage(key)
                }
                openPhases[key] = .loaded
                staleCachedPages.remove(key)
                markUnreadReadIfNeeded(threadCache[key] ?? [])
                return true
            }
        } catch is CancellationError {
            return false
        } catch {
            guard captured == generation else { return false }
            if threadCache[key] == nil { openPhases[key] = .failed(friendly(error)) }
            return false
        }
    }

    func closeThread() {
        openThreadKey = nil
    }

    @discardableResult
    private func loadThreadPage(_ key: String, olderThan oldest: MessageRecord? = nil) async -> Bool {
        guard isValid else { return false }
        let captured = generation
        if oldest == nil { openPhases[key] = threadCache[key] == nil ? .loading : .loaded }
        // Legacy unqualified keys (restored history from a v1 pairing, or an
        // ancient outbox row): the v2 gateway rejects them with
        // CB-V2-400 "threadKey 格式错误" — a raw developer error that must
        // never reach the UI. Resolve against an EXPLICIT authorized line
        // (default first, then each authorized line) — the server validation
        // stays strict, we simply try the concrete qualified candidates.
        if oldest == nil && !ThreadKey.isQualified(key) {
            return await loadUnqualifiedThread(key, captured: captured)
        }
        return await loadThreadPage(key, qualifiedKey: key, olderThan: oldest, captured: captured)
    }

    private func loadUnqualifiedThread(_ key: String, captured: UInt64) async -> Bool {
        var candidates: [String] = []
        if let line = lineIdProvider?() {
            candidates.append(line)
        }
        for line in authorizedLineIDsProvider?() ?? [] where !candidates.contains(line) {
            candidates.append(line)
        }
        // No lines at all (offline demo): the API accepts the bare key as-is.
        if candidates.isEmpty {
            return await loadThreadPage(key, qualifiedKey: key, olderThan: nil, captured: captured)
        }
        for line in candidates {
            let qualified = ThreadKey.canonical(lineID: line, peer: key)
            // Display key: the bare key the user tapped; fetch key: the
            // concrete qualified candidate.
            if await loadThreadPage(key, qualifiedKey: qualified, olderThan: nil, captured: captured) {
                return true
            }
        }
        guard captured == generation else { return false }
        if threadCache[key] == nil {
            DiagnosticsStore.shared.log("messages",
                "unqualified threadKey unresolved: peer=\(DiagnosticsRedactor.redactPhoneNumbers(ThreadKey.peer(of: key))) lines=\(candidates.count)")
            openPhases[key] = .failed(String(localized:
                "无法确定这条旧对话所属的线路。请在对话列表中向该号码发送一条新短信，或联系网关管理员同步线路。"))
        }
        return false
    }

    @discardableResult
    private func loadThreadPage(_ key: String, qualifiedKey fetchKey: String,
                                olderThan oldest: MessageRecord?,
                                captured: UInt64) async -> Bool {
        do {
            let page: ThreadMessagePage
            var validator: HistoryValidator?
            if oldest == nil {
                // Newest page: ask conditionally so a repeat open of an
                // unchanged conversation transfers zero body bytes and lets
                // us (re)register the durable validator.
                let outcome = try await api.listThreadMessagesConditional(
                    threadKey: fetchKey, beforeCreatedAt: nil, beforeID: nil, limit: pageLimit)
                switch outcome {
                case .changed(let fresh, let v):
                    page = fresh
                    validator = v
                case .notModified:
                    // We have no cached body for this key (that is why we are
                    // loading) yet the gateway answered 304 — a validator with
                    // no matching persisted body. Converge with ONE full fetch.
                    page = try await api.listThreadMessages(
                        threadKey: fetchKey, beforeCreatedAt: nil, beforeID: nil, limit: pageLimit)
                }
            } else {
                // Explicit "load older" pagination: always a genuine new page
                // (unique cursor), full fetch, merged onto the retained pages.
                page = try await api.listThreadMessages(
                    threadKey: fetchKey, beforeCreatedAt: oldest?.createdAt, beforeID: oldest?.id,
                    limit: pageLimit)
            }
            guard captured == generation else { return false }
            // Cache under the tapped key so the open/rows state stays
            // consistent for the conversation the user actually opened. The
            // gateway returns the canonical qualified threadKey; when the
            // user tapped a legacy bare row, rekey the page to that bare key
            // (the line that served it is preserved on each record).
            let messages: [MessageRecord]
            if fetchKey == key {
                messages = page.messages
            } else {
                messages = page.messages.map {
                    MessageRecord(id: $0.id, gatewayID: $0.gatewayID, lineID: $0.lineID,
                                  threadKey: key, direction: $0.direction, peer: $0.peer,
                                  body: $0.body, encoding: $0.encoding, status: $0.status,
                                  createdAt: $0.createdAt)
                }
            }
            merge(messages, for: key)
            if page.hasMore { hasMoreThreads.insert(key) } else { hasMoreThreads.remove(key) }
            openPhases[key] = .loaded
            // Settle point: persist the page bodies AND the validator that
            // guarded them in the same atomic write.
            persistPage(key, captured: captured, validator: validator, flush: true)
            markUnreadReadIfNeeded(messages)
            reevaluateAll()
            return true
        } catch is CancellationError {
            return false
        } catch {
            guard captured == generation else { return false }
            if threadCache[key] == nil { openPhases[key] = .failed(friendly(error)) }
            return false
        }
    }

    /// Applies a fetched newest-page body: merge, paging flag, open phase,
    /// durable page+validator commit, read marking and folder re-evaluation.
    private func applyFetchedPage(_ page: ThreadMessagePage, displayKey key: String,
                                  fetchKey: String, captured: UInt64,
                                  validator: HistoryValidator?) {
        guard captured == generation else { return }
        let messages: [MessageRecord]
        if fetchKey == key {
            messages = page.messages
        } else {
            messages = page.messages.map {
                MessageRecord(id: $0.id, gatewayID: $0.gatewayID, lineID: $0.lineID,
                              threadKey: key, direction: $0.direction, peer: $0.peer,
                              body: $0.body, encoding: $0.encoding, status: $0.status,
                              createdAt: $0.createdAt)
            }
        }
        merge(messages, for: key)
        if page.hasMore { hasMoreThreads.insert(key) } else { hasMoreThreads.remove(key) }
        openPhases[key] = .loaded
        persistPage(key, captured: captured, validator: validator, flush: true)
        markUnreadReadIfNeeded(messages)
        reevaluateAll()
    }

    func loadOlder() {
        guard let key = openThreadKey, let oldest = threadCache[key]?.first, hasMoreThreads.contains(key) else { return }
        Task { await loadThreadPage(key, olderThan: oldest) }
    }

    func retryThread() {
        guard let key = openThreadKey else { return }
        Task { await loadThreadPage(key) }
    }

    private func merge(_ incoming: [MessageRecord], for key: String) {
        mergeMessages(incoming)
    }

    /// Merge into the per-thread cache (also used by reconciliation).
    private func mergeMessages(_ incoming: [MessageRecord]) {
        var perThread: [String: [MessageRecord]] = [:]
        for message in incoming {
            if actions.contains(where: { $0.kind == .delete && !$0.failed && $0.threadKey == message.threadKey && $0.covers(message) }) { continue }
            // A message at/before the conversation's tombstone is history:
            // never re-merge it (a NEWER message reopens the thread).
            if isTombstoned(message.threadKey, newestMessageAt: message.createdAt) { continue }
            perThread[message.threadKey, default: []].append(message)
            persistedWatermark = max(persistedWatermark, message.createdAt)
        }
        for (key, messages) in perThread {
            var byID: [String: MessageRecord] = [:]
            for message in threadCache[key] ?? [] { byID[message.id] = message }
            for message in messages { byID[message.id] = message }
            threadCache[key] = byID.values.sorted {
                $0.createdAt == $1.createdAt ? $0.id < $1.id : $0.createdAt < $1.createdAt
            }
        }
        pruneMirroredOutbox()
        suppressCloudCopiesOfLiveMessages()
        reevaluateAll()
        // Event/ack merges of already-open threads land on disk too
        // (throttled — a burst of events is one write, not N).
        if historyStore != nil {
            let captured = generation
            for key in perThread.keys where openPhases[key] == .loaded || threadCache.keys.contains(key) {
                persistPage(key, captured: captured, validator: nil, flush: false)
            }
        }
    }

    /// Whether an outbox entry belongs to this conversation: exact canonical
    /// key, or a legacy bare entry whose captured line/peer resolves here.
    private func outboxEntry(_ entry: MessageOutboxEntry, belongsTo threadKey: String) -> Bool {
        if entry.threadKey == threadKey { return true }
        guard !ThreadKey.isQualified(entry.threadKey) else { return false }
        return threadKey == ThreadKey.canonical(lineID: entry.lineID, peer: entry.to)
            || ThreadKey.peer(of: threadKey) == entry.to
    }

    func rows(for threadKey: String) -> [MessageRow] {
        let records = (threadCache[threadKey] ?? []).filter { message in
            !actions.contains { $0.kind == .delete && !$0.failed && $0.threadKey == threadKey && $0.covers(message) }
        }
        var rows = records.map(MessageRow.record)
        let knownIDs = Set(records.map(\.id))
        for entry in outbox where outboxEntry(entry, belongsTo: threadKey) {
            if let serverID = entry.serverMessageID, knownIDs.contains(serverID) { continue }
            rows.append(.pending(entry))
        }
        return rows.sorted { lhs, rhs in
            lhs.date == rhs.date ? lhs.id < rhs.id : lhs.date < rhs.date
        }
    }

    func openPhase(for key: String) -> ThreadOpenPhase {
        openPhases[key] ?? .idle
    }

    // MARK: Send

    func canStartNewSend(to rawRecipient: String, body: String, isLineReady: Bool) -> String? {
        let recipient = rawRecipient.trimmingCharacters(in: .whitespacesAndNewlines)
        let trimmedBody = body.trimmingCharacters(in: .whitespacesAndNewlines)
        if recipient.isEmpty || recipient.count > 32 {
            return MessageSendError.invalidRecipient.friendlyMessage
        }
        if trimmedBody.isEmpty { return MessageSendError.bodyEmpty.friendlyMessage }
        if body.count > 10_000 { return MessageSendError.bodyTooLong.friendlyMessage }
        if !isLineReady {
            return MessageSendError.lineNotReady("短信线路不可用：请确认 SIM 就绪、已注册网络且短信能力可用。").friendlyMessage
        }
        return nil
    }

    /// The line a NEW submission to this thread will use: the per-thread
    /// preference when set, otherwise the app default. Retries never
    /// re-resolve — they reuse the line captured in the entry.
    func preferredLine(for threadKey: String) -> String? {
        lineIdForThread?(threadKey) ?? lineIdProvider?()
    }

    /// - Parameter lineId: Explicit line for this submission (compose view's
    ///   "From" choice). When nil the per-thread preference/app default
    ///   applies. Either way the value is CAPTURED into the entry and every
    ///   retry reuses it — never a silent fallback to another number.
    @discardableResult
    func send(to rawRecipient: String, body rawBody: String, isLineReady: Bool,
              lineId: String? = nil) -> MessageOutboxEntry? {
        guard isValid else { return nil }
        // Normalize the dialable destination FIRST: contact numbers carry
        // cosmetic formatting ("+86 130 0313 2132") which the gateway's PDU
        // encoder rejects before creating any message (every send failed
        // instantly in the field). The normalized value also keys the thread,
        // so the conversation can never split on formatting variants.
        guard let recipient = PhoneNormalizer.dialable(rawRecipient) else { return nil }
        let body = rawBody.trimmingCharacters(in: .whitespacesAndNewlines)
        guard canStartNewSend(to: recipient, body: body, isLineReady: isLineReady) == nil else {
            return nil
        }
        if let pending = outbox.first(where: { $0.isSending && $0.to == recipient && $0.body == body }) {
            return pending
        }
        // Capture the line FIRST so the outbox row is keyed by the canonical
        // `lineID:peer` form: a legacy bare-keyed synthetic thread duplicates
        // the gateway conversation and can never open on v2 (CB-V2-400), which
        // is exactly what the "threadKey 格式错误" screen reported.
        let capturedLine = lineId ?? preferredLine(for: recipient)
        let entry = MessageOutboxEntry(
            threadKey: ThreadKey.canonical(lineID: capturedLine, peer: recipient),
            to: recipient, body: body, now: now(), lineID: capturedLine)
        outbox.append(entry)
        persistOutbox()
        performSend(entry)
        return entry
    }

    /// Explicit user retry: clears the restart quarantine and re-submits
    /// with the ORIGINAL line and idempotency key.
    func retry(_ entry: MessageOutboxEntry) {
        guard isValid else { return }
        guard let index = outbox.firstIndex(where: { $0.id == entry.id }),
              !outbox[index].isSending else { return }
        outbox[index].isSending = true
        outbox[index].status = .queued
        outbox[index].errorText = nil
        outbox[index].needsConfirmation = false
        persistOutbox()
        performSend(outbox[index])
    }

    /// Re-sends a message the gateway truthfully persisted as failed. This is
    /// a NEW logical submission (new idempotency key): replaying the old key
    /// would only replay the gateway's stored failure, never resend the SMS.
    ///
    /// The retry MUST originate from the record's original line: re-resolving
    /// the current preference could silently move the resend to another SIM
    /// after the user changed the conversation line. Records that carry no
    /// line (legacy unified history) fail explicitly instead of guessing.
    func resendFailed(_ record: MessageRecord) {
        guard isValid, record.isOutbound, record.status == .failed,
              !outbox.contains(where: { $0.isSending && $0.sourceMessageID == record.id }) else { return }
        guard let originalLine = record.lineID else {
            var entry = MessageOutboxEntry(threadKey: record.threadKey, to: record.peer, body: record.body,
                                           now: now(), sourceMessageID: record.id, lineID: nil)
            entry.isSending = false
            entry.status = .failed
            entry.errorText = String(localized: "无法确定原发送线路，请重新编辑内容后发送。")
            outbox.append(entry)
            persistOutbox()
            return
        }
        let entry = MessageOutboxEntry(threadKey: record.threadKey, to: record.peer, body: record.body,
                                       now: now(), sourceMessageID: record.id,
                                       lineID: originalLine)
        outbox.append(entry)
        persistOutbox()
        performSend(entry)
    }

    /// Retry durable queued entries once the line CAPTURED IN EACH ENTRY is
    /// ready (foreground/reconnect). Stable idempotency keys make this safe
    /// after ambiguity. Entries quarantined after an app restart
    /// (`needsConfirmation`) are NEVER auto-sent: only an explicit user
    /// retry submits them.
    func flushReadyOutbox() {
        for entry in outbox where !entry.isSending && entry.status != .failed && !entry.needsConfirmation {
            guard entryReadiness(entry) else { continue }
            guard let index = outbox.firstIndex(where: { $0.id == entry.id }) else { continue }
            outbox[index].isSending = true
            performSend(outbox[index])
        }
    }

    private func entryReadiness(_ entry: MessageOutboxEntry) -> Bool {
        lineReadyForEntry?(entry.lineID) ?? lineReady()
    }

    private func performSend(_ entry: MessageOutboxEntry) {
        let captured = generation
        Task {
            // Hold the message when its captured line isn't ready yet;
            // AppModel flushes the queue when that line's readiness returns.
            // A healthy second line is never blocked by the default line.
            guard self.entryReadiness(entry) else {
                guard captured == self.generation else { return }
                self.markWaiting(entryID: entry.id)
                return
            }
            guard isValid, captured == generation else { return }
            let message: MessageRecord
            do {
                // The entry's captured line is authoritative: a retry uses
                // the same line the user originally sent from, never a
                // silent fallback to another number.
                message = try await api.sendMessage(
                    to: entry.to, body: entry.body, lineId: entry.lineID,
                    idempotencyKey: entry.idempotencyKey
                )
            } catch {
                guard captured == generation else { return }
                self.markFailure(entryID: entry.id, error: error)
                return
            }
            guard captured == generation else { return }
            self.markAccepted(entryID: entry.id, message: message)
        }
    }

    private func markWaiting(entryID: String) {
        guard let index = outbox.firstIndex(where: { $0.id == entryID }) else { return }
        outbox[index].isSending = false
        outbox[index].status = .queued
        outbox[index].errorText = outbox[index].needsConfirmation
            ? String(localized: "应用重启后待确认，点“重试”发送")
            : String(localized: "等待所选线路恢复后自动发送")
        persistOutbox()
    }

    private func markFailure(entryID: String, error: Error) {
        guard let index = outbox.firstIndex(where: { $0.id == entryID }) else { return }
        outbox[index].isSending = false
        outbox[index].status = .failed
        outbox[index].errorText = friendly(error)
        persistOutbox()
    }

    private func markAccepted(entryID: String, message: MessageRecord) {
        guard let index = outbox.firstIndex(where: { $0.id == entryID }) else { return }
        outbox[index].isSending = false
        outbox[index].serverMessageID = message.id
        outbox[index].status = message.status == .failed ? .failed : message.status
        if message.status == .failed {
            outbox[index].errorText = "网关报告短信发送失败，可重试。"
            persistOutbox()
        } else {
            outbox.remove(at: index)
            persistOutbox()
        }
        mergeMessages([message])
        Task { await refreshThreads() }
    }

    private func pruneMirroredOutbox() {
        var removed = false
        for entry in outbox {
            guard let serverID = entry.serverMessageID else { continue }
            // Exact canonical lookup first; legacy bare entries resolve via
            // their captured line/peer.
            let keys = [entry.threadKey,
                        ThreadKey.canonical(lineID: entry.lineID, peer: entry.to)]
            let mirrored = keys.contains { key in
                threadCache[key]?.contains(where: { $0.id == serverID }) == true
            }
            guard mirrored else { continue }
            outbox.removeAll { $0.id == entry.id }
            removed = true
        }
        if removed { persistOutbox() }
    }

    // MARK: Persistent outbox

    private func recoverPersistedOutbox() {
        guard let persisted = outboxStore?.load(), !persisted.isEmpty else { return }
        // The process died mid-flight: the gateway outcome is ambiguous.
        // Quarantine every recovered entry: it is never auto-sent (a line
        // recovery or gateway restart must not submit old records); the
        // stable idempotency key makes an explicit user retry safe, and
        // gateway idempotency dedupes any message it already accepted.
        outbox = persisted.map { entry in
            var entry = entry
            entry.isSending = false
            entry.needsConfirmation = true
            // Migrate legacy bare keys to the canonical qualified form so the
            // recovered row joins the real conversation (and opens) instead
            // of duplicating it with an unopenable bare-key thread.
            if !ThreadKey.isQualified(entry.threadKey) {
                entry.threadKey = ThreadKey.canonical(lineID: entry.lineID, peer: entry.to)
            }
            if entry.status != .failed {
                entry.status = .queued
                entry.errorText = String(localized: "应用重启后待确认，点“重试”发送")
            }
            return entry
        }
        persistOutbox()
    }

    private func persistOutbox() {
        guard let outboxStore else { return }
        outboxStore.save(outbox)
    }

    // MARK: Test seams (@testable import only)

    /// Inserts an externally-constructed entry (legacy-shape fixtures) without
    /// triggering a send.
    func addOutboxEntryForTest(_ entry: MessageOutboxEntry) {
        outbox.append(entry)
    }

    var outboxForTest: [MessageOutboxEntry] { outbox }

    // MARK: CloudKit-restored history (read-only, never gateway actions)

    /// Threads assembled purely from restored CloudKit messages for the
    /// currently bound gateway scope. They are read-only: no read marks, no
    /// sends are ever triggered automatically.
    @Published private(set) var cloudThreads: [MessageThread] = []

    private static func cloudRecordID(_ syncedID: String) -> String { "cloud:\(syncedID)" }
    static func isCloudRecordID(_ id: String) -> Bool { id.hasPrefix("cloud:") }

    /// Merge downloaded messages (already scoped/filtered by the app layer)
    /// into read-only threads. Cloud logical ids are "<scope>.<raw id>"; the
    /// raw gateway id is used to de-duplicate against a live record so the
    /// same message is never shown twice once the gateway fetch covers it.
    func applyCloudMessages(_ messages: [SyncedMessage], scope: String) {
        guard isValid, !messages.isEmpty else { return }
        var records: [MessageRecord] = []
        for message in messages {
            let raw = message.id.hasPrefix(scope + ".")
                ? String(message.id.dropFirst(scope.count + 1)) : message.id
            // Live gateway copy already present: the cloud restore is hidden.
            if let cached = threadCache[message.threadKey],
               cached.contains(where: { $0.id == raw && !MessageInbox.isCloudRecordID($0.id) }) {
                continue
            }

            let record = MessageRecord(
                id: Self.cloudRecordID(raw),
                gatewayID: Self.cloudGatewayMarker,
                lineID: nil,
                threadKey: message.threadKey,
                direction: MessageDirection(rawValue: message.direction) ?? .inbound,
                peer: message.peer,
                body: message.body,
                encoding: nil,
                status: MessageStatus(rawValue: message.status) ?? .unknown,
                createdAt: message.createdAt
            )
            cloudMessageIDs.insert(record.id)
            records.append(record)
        }
        if !records.isEmpty { mergeMessages(records) }
        rebuildCloudThreads()
    }

    /// Drop restored rows whose raw id is now delivered by the live gateway,
    /// so a message can never be displayed twice.
    func suppressCloudCopiesOfLiveMessages() {
        guard !cloudMessageIDs.isEmpty else { return }
        var removed = Set<String>()
        for (key, records) in threadCache {
            let liveRawIDs = Set(records.filter { !MessageInbox.isCloudRecordID($0.id) }.map(\.id))
            guard !liveRawIDs.isEmpty else { continue }
            for cloudID in cloudMessageIDs {
                let raw = String(cloudID.dropFirst("cloud:".count))
                guard liveRawIDs.contains(raw),
                      let idx = threadCache[key]?.firstIndex(where: { $0.id == cloudID }) else { continue }
                threadCache[key]?.remove(at: idx)
                removed.insert(cloudID)
            }
            if threadCache[key]?.isEmpty == true { threadCache[key] = nil }
        }
        if !removed.isEmpty {
            cloudMessageIDs.subtract(removed)
            rebuildCloudThreads()
        }
    }

    /// Replace the whole restored set for a gateway scope with the engine's
    /// converged snapshot (which already honors tombstones/LWW).
    func setCloudMessages(_ messages: [SyncedMessage], scope: String) {
        purgeCloudRestored()
        applyCloudMessages(messages, scope: scope)
    }

    /// Drop ALL restored history (account change / logout / disable).
    func purgeCloudRestored() {
        let ids = cloudMessageIDs
        guard !ids.isEmpty else {
            cloudThreads = []
            return
        }
        for key in threadCache.keys {
            threadCache[key]?.removeAll { ids.contains($0.id) }
            if threadCache[key]?.isEmpty == true { threadCache[key] = nil }
        }
        cloudMessageIDs = []
        cloudThreads = []
        reevaluateAll()
    }

    private func rebuildCloudThreads() {
        // Restored history only contributes threads the live gateway does not
        // already provide; mixed threads are owned by the gateway fetch.
        // Tombstoned conversations stay hidden regardless of source.
        let liveKeys = Set(threads.map(\.key))
        var built: [MessageThread] = []
        for (key, records) in threadCache where !liveKeys.contains(key) {
            let restored = records.filter { cloudMessageIDs.contains($0.id) }
            guard let last = restored.max(by: { $0.createdAt < $1.createdAt }) else { continue }
            if isTombstoned(key, newestMessageAt: last.createdAt) { continue }
            built.append(MessageThread(key: key, peer: last.peer, unreadCount: 0, lastMessage: last))
        }
        cloudThreads = built.sorted { $0.lastMessage.createdAt > $1.lastMessage.createdAt }
    }

    func isCloudThread(_ key: String) -> Bool {
        guard let records = threadCache[key], !records.isEmpty else { return false }
        return records.allSatisfy { cloudMessageIDs.contains($0.id) }
    }

    // MARK: Events / demo pushes

    func apply(eventMessage message: MessageRecord) {
        guard isValid else { return }
        let captured = generation
        mergeMessages([message])
        if message.threadKey == openThreadKey {
            markUnreadReadIfNeeded([message])
        }
        // The event carries the full record: update the thread ROW locally so
        // the list shows the new message/unread immediately, then persist —
        // NOT an unconditional whole-list re-download.
        upsertThreadRow(for: message)
        persistHistory(flush: true, captured: captured, localTransform: true)
        // Only a bounded CONDITIONAL revalidation runs when the list has
        // never loaded this session, or the event stream is unhealthy (we
        // may have missed other deltas). An unchanged list costs a 304.
        if listPhase != .loaded || !isEventStreamHealthy() {
            Task { await self.refreshThreads() }
        }
    }

    /// Merges a live message into the in-memory thread list (row upsert) so
    /// the event path keeps the list warm without a network round trip.
    private func upsertThreadRow(for message: MessageRecord) {
        if isTombstoned(message.threadKey, newestMessageAt: message.createdAt) { return }
        var thread = threads.first { $0.key == message.threadKey }
        if thread == nil {
            thread = MessageThread(key: message.threadKey, peer: message.peer,
                                   unreadCount: 0, lastMessage: message)
        }
        let current = thread!
        var updated = current
        if message.createdAt >= current.lastMessage.createdAt
            || (message.createdAt == current.lastMessage.createdAt && message.id > current.lastMessage.id) {
            updated = MessageThread(key: current.key, peer: current.peer,
                                     unreadCount: current.unreadCount,
                                     lastMessage: message, folder: current.folder)
            if message.direction == .inbound, message.status != .read,
               (message.threadKey != openThreadKey || !conversationIsForeground()) {
                updated.unreadCount += 1
            }
        }
        if let index = threads.firstIndex(where: { $0.key == message.threadKey }) {
            threads[index] = updated
        } else {
            threads.append(updated)
        }
        threads.sort { $0.lastMessage.createdAt > $1.lastMessage.createdAt }
        if listPhase != .loaded, !threads.isEmpty { listPhase = .loaded }
    }

    /// Poll-loop revalidation of the OPEN conversation. Bounded + conditional:
    /// an unchanged newest page answers 304 (zero body bytes) and the loaded
    /// older pages/cursor are preserved; this is what surfaces read marks and
    /// other server-side state the gateway does NOT broadcast as events, even
    /// while the event stream is healthy.
    @discardableResult
    func refreshOpenThread(_ key: String) async -> Bool {
        if threadCache[key]?.isEmpty == false {
            return await revalidateOpenPage(key)
        }
        return await loadThreadPage(key)
    }

    // MARK: Read receipts

    private func markUnreadReadIfNeeded(_ messages: [MessageRecord]) {
        guard conversationIsForeground(), let key = openThreadKey else { return }
        let messages = messages.filter { $0.threadKey == key }
        if actionStore != nil {
            guard messages.contains(where: { $0.direction == .inbound && $0.status != .read }) else { return }
            _ = enqueueActions([key], kind: .read)
            return
        }
        for message in messages where message.direction == .inbound && message.status != .read {
            let dedupeKey = "read:\(message.id)"
            guard !readKeys.contains(dedupeKey) else { continue }
            readKeys.insert(dedupeKey)
            let key = UUID().uuidString
            let captured = generation
            Task { [weak self] in
                guard let self, isValid, captured == generation else { return }
                do {
                    try await api.markMessageRead(id: message.id, idempotencyKey: key)
                    guard isValid, captured == generation else { return }
                    if let list = threadCache[message.threadKey],
                       let index = list.firstIndex(where: { $0.id == message.id }) {
                        threadCache[message.threadKey]?[index] = list[index].with(status: .read)
                    }
                    await refreshThreads()
                } catch {
                    guard captured == generation else { return }
                    readKeys.remove(dedupeKey)
                }
            }
        }
    }

    // MARK: Durable conversation operations
    private func enqueueActions(_ keys: [String], kind: SMSConversationAction.Kind) -> BulkOperationResult {
        var result = BulkOperationResult()
        guard isValid, !actionStoreLoadFailed else { result.failed = keys; return result }
        var next = actions
        for key in Set(keys) {
            let latest = ((threadCache[key] ?? []) + (threads + gatewayJunkThreads).filter { $0.key == key }.map(\.lastMessage))
                .filter { !Self.isCloudRecordID($0.id) }
                .max { $0.createdAt == $1.createdAt ? $0.id < $1.id : $0.createdAt < $1.createdAt }
            guard let latest else { result.failed.append(key); continue }
            if next.contains(where: { $0.threadKey == key && !$0.failed && $0.covers(latest) && ($0.kind == kind || $0.kind == .delete) }) {
                result.succeeded.append(key); continue
            }
            next.removeAll { $0.threadKey == key && !$0.attempted && (kind == .delete || $0.kind == kind) }
            next.append(SMSConversationAction(id: UUID().uuidString, kind: kind, threadKey: key,
                throughCreatedAt: latest.createdAt, throughID: latest.id,
                unreadCount: (threads + gatewayJunkThreads).first { $0.key == key }?.unreadCount ?? 0))
            result.succeeded.append(key)
        }
        do { try actionStore?.save(next) }
        catch {
            actionSyncError = "无法保存操作，请重试。"
            result.failed += result.succeeded; result.succeeded = []; return result
        }
        actions = next; restoreActionOverlay(); wakeActionSync()
        return result
    }

    private func restoreActionOverlay() {
        pendingDeleteKeys = Set(actions.filter { !$0.failed && $0.kind == .delete }.map(\.threadKey))
        pendingReadKeys = Set(actions.filter { !$0.failed && !$0.completed && $0.kind == .read }.map(\.threadKey))
        objectWillChange.send()
    }

    func retrySMSActions() {
        if actionStoreLoadFailed {
            do { actions = try actionStore?.load() ?? []; actionStoreLoadFailed = false }
            catch { actionSyncError = "无法读取待同步操作，请重试。"; return }
        }
        var next = actions
        for i in next.indices { next[i].failed = false }
        do { try actionStore?.save(next); actions = next; actionSyncError = nil; restoreActionOverlay(); wakeActionSync() }
        catch { actionSyncError = "无法保存操作，请重试。" }
    }

    func wakeActionSync() {
        actionRetrySleep?.cancel()
        guard isValid, actionsEnabled, actionWorker == nil, actions.contains(where: { !$0.failed && !$0.completed }) else { return }
        let token = UUID(); actionWorkerToken = token
        actionWorker = Task { [weak self] in await self?.runActionSync(token: token) }
    }

    func actionSyncEnteredBackground() {
        guard actionWorker != nil, actionBackgroundTask == .invalid else { return }
        let token = actionWorkerToken
        actionBackgroundTask = UIApplication.shared.beginBackgroundTask(withName: "短信同步") { [weak self] in
            Task { @MainActor in self?.expireActionSync(token: token) }
        }
    }
    private func expireActionSync(token: UUID) {
        guard actionWorkerToken == token else { return }
        actionWorkerToken = UUID()
        actionWorker?.cancel(); actionWorker = nil; actionRetrySleep?.cancel()
        endActionBackgroundTask()
    }
    #if DEBUG
    func expireActionSyncForTest() { expireActionSync(token: actionWorkerToken) }
    var actionSyncRunningForTest: Bool { actionWorker != nil }
    #endif
    private func endActionBackgroundTask() {
        if actionBackgroundTask != .invalid {
            UIApplication.shared.endBackgroundTask(actionBackgroundTask); actionBackgroundTask = .invalid
        }
    }
    func purgeSMSActions() {
        actionsEnabled = false; actionWorkerToken = UUID(); actionWorker?.cancel(); actionWorker = nil; actionRetrySleep?.cancel()
        actionStore?.purge(); actions = []; restoreActionOverlay(); endActionBackgroundTask()
    }

    private func runActionSync(token: UUID) async {
        defer { if actionWorkerToken == token { actionWorker = nil; endActionBackgroundTask() } }
        var delay: UInt64 = 1
        let captured = generation
        while isValid && captured == generation && token == actionWorkerToken && !Task.isCancelled {
            let batch = Array(actions.filter { !$0.failed && !$0.completed }.prefix(100))
            guard !batch.isEmpty else { return }
            do {
                // Persist the immutable payload before any request can reach the server.
                var attempted = actions
                for i in attempted.indices where batch.contains(where: { $0.id == attempted[i].id }) { attempted[i].attempted = true }
                try actionStore?.save(attempted); actions = attempted
                let response: SMSActionResponse
                if smsActionsCapability == nil { smsActionsCapability = try await api.supportsSMSActions() }
                if smsActionsCapability == true {
                    response = try await api.performSMSActions(batch)
                } else {
                    var results: [SMSActionResponse.Result] = []
                    for action in batch {
                        if action.kind == .delete {
                            // Legacy delete has no watermark: never erase messages received after this intent.
                            results.append(.init(id: action.id, ok: false, error: .init(code: "upgrade_required", message: "请更新网关后重试删除。", retryable: false)))
                            continue
                        }
                        var before: MessageRecord?
                        while true {
                            try Task.checkCancellation()
                            let page = try await api.listThreadMessages(threadKey: action.threadKey,
                                beforeCreatedAt: before?.createdAt, beforeID: before?.id, limit: pageLimit)
                            for message in page.messages where action.covers(message) && message.direction == .inbound && message.status != .read {
                                try await api.markMessageRead(id: message.id, idempotencyKey: action.id + ":" + message.id)
                            }
                            guard page.hasMore, let oldest = page.messages.first, oldest.id != before?.id else { break }
                            before = oldest
                        }
                        results.append(.init(id: action.id, ok: true, error: nil))
                    }
                    response = SMSActionResponse(results: results)
                }
                guard !Task.isCancelled, captured == generation, token == actionWorkerToken, isValid else { return }
                let expectedIDs = Set(batch.map(\.id))
                guard response.results.count == batch.count,
                    Set(response.results.map(\.id)) == expectedIDs else { throw URLError(.cannotParseResponse) }
                var next = actions
                var settled: [SMSConversationAction] = []
                for result in response.results {
                    guard let action = batch.first(where: { $0.id == result.id }) else { continue }
                    if result.ok {
                        if let i = next.firstIndex(where: { $0.id == result.id }) { next[i].completed = true }
                        settled.append(action)
                    }
                    else if result.error?.retryable == false {
                        if let index = next.firstIndex(where: { $0.id == result.id }) { next[index].failed = true }
                        actionSyncError = "部分操作未完成，请重试。"
                    }
                }
                // Raw cache changes settle before journal removal so a restart cannot resurrect a badge.
                for action in settled {
                    if action.kind == .delete { applyBoundedDeletion(action) }
                    else { applyReadBoundary(key: action.threadKey, at: action.throughCreatedAt, id: action.throughID) }
                }
                historyStore?.invalidateListValidators()
                persistHistory(flush: true, captured: captured, localTransform: true)
                // Keep one durable acknowledged boundary per thread/kind; raw late GETs cannot undo intent on restart.
                next = next.filter { item in
                    !item.completed || !next.contains(where: { other in
                        other.completed && other.id != item.id && other.threadKey == item.threadKey && other.kind == item.kind &&
                        (other.throughCreatedAt > item.throughCreatedAt || (other.throughCreatedAt == item.throughCreatedAt && other.throughID > item.throughID))
                    })
                }
                try actionStore?.save(next); actions = next; restoreActionOverlay()
                if response.results.contains(where: { !$0.ok && $0.error?.retryable != false }) || response.results.count < batch.count {
                    throw URLError(.cannotConnectToHost)
                }
                delay = 1
            } catch is CancellationError { return }
            catch {
                guard !Task.isCancelled, captured == generation else { return }
                if case .http(let status, _, _) = error as? APIError, smsActionsCapability == true, status == 404 || status == 405 {
                    smsActionsCapability = false
                    continue
                }
                if case .http(let status, _, _) = error as? APIError, (400..<500).contains(status), status != 401, status != 408, status != 429 {
                    var next = actions
                    for i in next.indices where batch.contains(where: { $0.id == next[i].id }) { next[i].failed = true }
                    if (try? actionStore?.save(next)) != nil { actions = next; restoreActionOverlay() }
                    actionSyncError = "部分操作未完成，请重试。"
                }
                let sleep = Task { try await Task.sleep(nanoseconds: delay * 1_000_000_000) }
                actionRetrySleep = sleep
                _ = try? await sleep.value
                actionRetrySleep = nil
                if Task.isCancelled { return }
                delay = min(60, delay * 2)
            }
        }
    }

    private func applyBoundedDeletion(_ action: SMSConversationAction) {
        threads.removeAll { $0.key == action.threadKey && action.covers($0.lastMessage) }
        gatewayJunkThreads.removeAll { $0.key == action.threadKey && action.covers($0.lastMessage) }
        threadCache[action.threadKey]?.removeAll { action.covers($0) }
        if let remaining = threadCache[action.threadKey], !remaining.isEmpty {
            persistPage(action.threadKey, captured: generation, validator: nil, flush: true)
        } else { historyStore?.removeMessages(forKey: action.threadKey, savedAt: now()) }
    }

    func applyReadBoundary(key: String, at: Int64, id: String) {
        let boundary = SMSConversationAction(id: "event", kind: .read, threadKey: key, throughCreatedAt: at, throughID: id)
        for i in threads.indices where threads[i].key == key && boundary.covers(threads[i].lastMessage) { threads[i].unreadCount = 0 }
        if let messages = threadCache[key] {
            threadCache[key] = messages.map { boundary.covers($0) && $0.direction == .inbound ? $0.with(status: .read) : $0 }
        }
        mutationRevision &+= 1
        readActions[key] = ReadAction(baselineCreatedAt: at, baselineID: id, markedCount: 0, ackRevision: mutationRevision)
        historyStore?.invalidateListValidators()
        persistHistory(flush: true, captured: generation, localTransform: true)
    }

    // MARK: Test support

    var generationValue: UInt64 { generation }

    /// Injects a durable store after init and runs the recovery path, so
    /// restart-quarantine behavior is testable.
    #if DEBUG
    func setOutboxStoreForTest(_ store: OutboxStore) {
        outboxStore = store
        recoverPersistedOutbox()
    }
    #endif

    private func friendly(_ error: Error) -> String {
        // Never leak the raw gateway developer error into the product UI:
        // "threadKey 格式错误" means an unqualified key slipped through; the
        // user cannot act on that wording. Keep the precise failure in the
        // sanitized diagnostics instead.
        if case .http(_, let code, let message) = error as? APIError,
           code == "CB-V2-400", message?.contains("threadKey") == true {
            DiagnosticsStore.shared.log("messages",
                "gateway rejected threadKey format (raw error preserved here)")
            return String(localized: "对话标识无法被网关识别。这通常来自旧版本的数据：返回列表重进该对话，或向该号码发一条新短信即可恢复。")
        }
        return (error as? APIError)?.friendlyMessage ?? "无法连接网关，请稍后重试。"
    }
}
