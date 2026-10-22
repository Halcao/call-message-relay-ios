import Foundation

/// On-device durable history cache: call recents, SMS thread list (normal +
/// gateway junk projection) and already-loaded message pages, plus the raw
/// HTTP validators (ETags) for exactly those list/page GETs. This is what
/// makes relaunch and offline cold start show existing data without
/// re-downloading: hydrate is pure local restore; conditional refresh keeps
/// it converged afterwards at 304 cost.
///
/// Scope/keying: the file name is a non-reversible SHA-256 hash of
/// "<canonical API base>|<gatewayId>|<deviceId>" — the real gateway API
/// identity plus the authorized pairing/device context, never a host-only
/// collision, never a credential. Data protection mirrors the outbox
/// (`.completeFileProtectionUntilFirstUserAuthentication`), writes are
/// atomic, and the store never touches iCloud/CloudKit: it lives under
/// Application Support on the device.
///
/// Coherence rules (root review):
/// * a persisted validator is written ONLY inside the same lock section (and
///   the same file) as the exact body it guards — a flush racing a fetch can
///   never pair an old body with a future response's validator;
/// * every serialized write re-checks the purge flag INSIDE the queue before
///   touching the filesystem, so a `flush()` that raced a `purge()` can
///   never resurrect the revoked scope;
/// * a deleted thread's page validators are dropped by exact parsed
///   `threadKey` query equality (plus legacy bare-peer equivalence), never
///   by substring guessing.
///
/// What may NEVER enter this store: tokens, keys, admin/auth/ICE surfaces,
/// diagnostics, or truncated push previews masquerading as message bodies.
/// The message bodies it stores are the same SMS texts the app already
/// displays on this device, at rest behind the same protection class as the
/// outbox.

enum HistoryCache {
    /// Schema 2: `truncatedKeys` sticky markers. A v1 snapshot's hasMore flags
    /// could have lost evicted-older history (the bounded-paging hole root
    /// flagged), so v1 files are deliberately discarded, never upgraded.
    static let schemaVersion = 2
    static let maxThreads = 400
    static let maxJunkThreads = 200
    static let maxCachedMessageThreads = 200
    static let maxMessagesPerThread = 250
    static let maxTotalMessages = 3000
    static let maxCalls = 200
    static let maxValidators = 600
}

/// One completed conditional GET: the durable validator the caller must
/// commit TOGETHER with the body it just applied, so a snapshot's metadata
/// always describes its persisted body and never a future response.
struct HistoryValidator: Equatable, Sendable {
    /// Canonical request URL the validator guards.
    let key: String
    /// The gateway's ETag for the body the caller is about to persist.
    /// nil = the gateway advertised no ETag: any prior validator for the key
    /// must be dropped (an old gateway / a response that cannot revalidate).
    let etag: String?
}

/// The complete snapshot persisted in ONE atomic file: bodies and their
/// validators are committed as a single unit.
struct HistorySnapshot: Codable {
    var schemaVersion: Int = HistoryCache.schemaVersion
    var savedAtMs: Int64 = 0
    /// Normal-list threads as displayed (server truth with local read/delete
    /// masking already applied), newest first.
    var threads: [MessageThread] = []
    /// Gateway-projected junk rows (empty on local-classification gateways).
    var junk: [MessageThread] = []
    /// Loaded message pages keyed by display thread key, chronological
    /// (oldest first) per thread — the same ordering `threadCache` keeps.
    var messages: [String: [MessageRecord]] = [:]
    /// Call history used to render the Recents tab.
    var calls: [CallRecord] = []
    /// Thread keys whose older-history paging flag was true.
    var hasMoreKeys: [String] = []
    /// Sticky set of conversation keys whose CACHED rows were ever trimmed by
    /// the durable caps (older rows still exist on the server beyond what is
    /// on disk). Once marked, a key keeps `hasMore=true` through every later
    /// flush — even one whose in-memory display says fully loaded — so a
    /// restart + unchanged 304 can never strand the evicted older history
    /// unreachable (bounded paging invariant, root review). Cleared when the
    /// thread leaves the cache entirely (delete/purge).
    var truncatedKeys: [String] = []
    /// Which query variant the persisted `threads`/`junk` body belongs to:
    /// `"folders"` (gateway normal+junk projection), `"line:<id>"` or
    /// `"line:all"`. Validators are only coherent with a body while their
    /// request variant matches this marker; switching variants MUST drop the
    /// list validators first (root review: an A→B→A filter flip, or a relaunch
    /// under a different saved filter, could otherwise 304 the stale variant's
    /// ETag and keep the wrong line's rows forever). Optional so pre-existing
    /// v1 files still decode (nil = variant unknown = no list revalidation).
    var listVariant: String? = nil
    /// canonical request URL -> gateway ETag (opaque validator string).
    var validators: [String: String] = [:]

    /// Enforce durable caps, oldest-first trim.
    func trimmed() -> HistorySnapshot {
        var copy = self
        copy.threads = Array(copy.threads
            .sorted { $0.lastMessage.createdAt > $1.lastMessage.createdAt }
            .prefix(HistoryCache.maxThreads))
        copy.junk = Array(copy.junk
            .sorted { $0.lastMessage.createdAt > $1.lastMessage.createdAt }
            .prefix(HistoryCache.maxJunkThreads))
        copy.calls = Array(copy.calls
            .sorted { $0.startedAt > $1.startedAt }
            .prefix(HistoryCache.maxCalls))

        let originalMessages = copy.messages
        var perKey: [String: [MessageRecord]] = [:]
        var total = 0
        let orderedKeys = copy.messages.keys.sorted { a, b in
            (copy.messages[a]?.last?.createdAt ?? 0) > (copy.messages[b]?.last?.createdAt ?? 0)
        }
        for key in orderedKeys.prefix(HistoryCache.maxCachedMessageThreads) {
            guard total < HistoryCache.maxTotalMessages else { break }
            let records = (copy.messages[key] ?? []).sorted { $0.createdAt < $1.createdAt }
            let keepCount = min(records.count, HistoryCache.maxMessagesPerThread,
                                HistoryCache.maxTotalMessages - total)
            guard keepCount > 0 else { break }
            // Keep the NEWEST records of the page (most useful for display).
            let kept = Array(records.suffix(keepCount))
            if !kept.isEmpty {
                perKey[key] = kept
                total += kept.count
            }
        }
        copy.messages = perKey
        // Bounded paging invariant (root review): rows evicted from a
        // RETAINED conversation still exist on the server, and threads
        // previously marked truncated stay marked while their rows live —
        // even a flush whose in-memory display says fully loaded must keep
        // `hasMore=true` persisted, so a restart + unchanged 304 can never
        // strand the older history un-reachable. A thread dropped from the
        // cache entirely loses its marker (its next open is a fresh full
        // fetch anyway).
        var sticky = Set(copy.truncatedKeys).intersection(perKey.keys)
        for (key, records) in originalMessages {
            if let kept = perKey[key], kept.count < records.count {
                sticky.insert(key)
            }
        }
        copy.truncatedKeys = sticky.sorted()
        var effectiveHasMore = Set(copy.hasMoreKeys).intersection(perKey.keys)
        effectiveHasMore.formUnion(sticky)
        copy.hasMoreKeys = effectiveHasMore.sorted()
        copy.validators = copy.prunedValidators(liveMessageKeys: Set(perKey.keys))
        return copy
    }

    /// A page validator may only survive while the page body it guards is
    /// still in the snapshot; list surfaces (`threads`/`calls`) are single
    /// whole-body endpoints and stay with their recorded lists.
    private func prunedValidators(liveMessageKeys: Set<String>) -> [String: String] {
        var result: [String: String] = [:]
        for (key, etag) in validators {
            guard let pageKey = Self.messageThreadKey(ofValidator: key) else {
                // Not a parseable history GET URL: never persist it.
                continue
            }
            switch pageKey {
            case .list:
                result[key] = etag
            case .thread(let threadKey):
                if liveMessageKeys.contains(threadKey) {
                    result[key] = etag
                }
            }
        }
        if result.count > HistoryCache.maxValidators {
            // Over the absolute cap keep only the whole-list surfaces; pages
            // re-fetch once and re-register validators with their bodies.
            result = result.filter { Self.messageThreadKey(ofValidator: $0.key) == .list }
        }
        return result
    }

    /// Classification of a validator key by EXACT URL parse (root review:
    /// substring matching is not acceptable for page identity).
    enum ValidatorSurface: Equatable {
        case list
        case thread(String)
    }

    static func messageThreadKey(ofValidator key: String) -> ValidatorSurface? {
        guard let url = URL(string: key),
              let components = URLComponents(url: url, resolvingAgainstBaseURL: false),
              let lastSegment = components.path.split(separator: "/").last else { return nil }
        switch String(lastSegment) {
        case "threads", "calls":
            return .list
        case "messages":
            // `threadKey` is the paging identity; decode exactly.
            guard let value = components.queryItems?.first(where: { $0.name == "threadKey" })?.value,
                  !value.isEmpty else { return nil }
            return .thread(value)
        default:
            return nil
        }
    }
}

/// Thread-safe on-disk history cache for exactly one scope. Shared between
/// the main-actor display objects (MessageInbox/AppModel) and the URLSession
/// API instance.
///
/// Writes are coalesced (~0.75 s) for bursty event merges, with mandatory
/// `flush()` at settle points (list refresh success, delete/read ack, page
/// fetch, teardown before release). Every filesystem touch happens on the
/// serial write queue and re-checks the purge flag under the lock AT WRITE
/// TIME: a flush enqueued before a purge and run after it (or vice versa)
/// can never leave a purged scope's file behind. All mutations funnel
/// through the lock; the caller (MessageInbox/AppModel) generation-fences
/// WHO may mutate; the store's `purgeFlag` fences writes that race a purge
/// so a late completion can never resurrect a revoked scope.
final class GatewayHistoryStore: @unchecked Sendable {
    enum LoadStatus {
        /// No file existed (first pairing for this scope, or purged).
        case absent
        /// A matching-version snapshot decoded cleanly.
        case loaded
        /// Garbage/short read: treat as no cache and converge via a full
        /// fetch; the next successful write replaces the corrupt file.
        case corrupt
        /// Different schemaVersion: the old layout is discarded, never
        /// partially trusted.
        case versionMismatch
    }

    let fileURL: URL
    private(set) var loadStatus: LoadStatus
    private let lock = NSLock()
    private var current: HistorySnapshot?
    private var loaded = false
    private var purgeFlag = false
    private var writePending = false
    private let writeQueue = DispatchQueue(label: "callrelay.history.write")
    private let writeThrottle: TimeInterval

    /// `scopeIdentifier` is the canonical API identity triple; `explicitURL`
    /// lets tests (and ephemeral fixtures) point the store anywhere.
    init?(scopeIdentifier: String?, explicitURL: URL? = nil, writeThrottle: TimeInterval = 0.75) {
        if let explicitURL {
            self.fileURL = explicitURL
        } else {
            guard let scopeIdentifier, !scopeIdentifier.isEmpty else { return nil }
            let hash = SHA256Lite.hex(Data(scopeIdentifier.utf8)).prefix(20)
            let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            let dir = base.appendingPathComponent("CallRelay/history", isDirectory: true)
            self.fileURL = dir.appendingPathComponent("history-\(hash).json")
        }
        self.writeThrottle = max(0, writeThrottle)
        // Decide the load status eagerly (on the calling thread) so AppModel
        // can treat a corrupt/version-mismatched snapshot as no-cache BEFORE
        // any conditional GET could answer 304 against a missing body.
        if let data = try? Data(contentsOf: fileURL) {
            if let decoded = try? JSONDecoder.iso.decode(HistorySnapshot.self, from: data),
               decoded.schemaVersion == HistoryCache.schemaVersion {
                self.loadStatus = .loaded
                self.current = decoded.trimmed()
            } else if let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
                      (object["schemaVersion"] as? Int) != nil {
                self.loadStatus = .versionMismatch
            } else {
                self.loadStatus = .corrupt
            }
            self.loaded = true
        } else {
            self.loadStatus = .absent
            self.loaded = true
        }
    }

    var isPurged: Bool {
        lock.lock(); defer { lock.unlock() }
        return purgeFlag
    }

    /// Current persisted validators (test/diagnostics read-only copy).
    var validatorsForTest: [String: String] {
        lock.lock(); defer { lock.unlock() }
        return current?.validators ?? [:]
    }

    /// Count of whole-list surface validators still held (normal/junk/line
    /// variants). Used to prove a variant flip dropped the stale variant's
    /// ETag instead of leaving it to 304 the wrong body.
    var listValidatorCountForTest: Int {
        lock.lock(); defer { lock.unlock() }
        guard let validators = current?.validators else { return 0 }
        return validators.keys.filter {
            HistorySnapshot.messageThreadKey(ofValidator: $0) == .list
        }.count
    }

    // MARK: Read (hydrate)

    /// Read-only restore of the persisted snapshot (nil = no usable cache).
    /// Hydrate is pure data restore: it never triggers network, read marks,
    /// CallKit or notification side effects.
    func snapshotForHydrate() -> HistorySnapshot? {
        lock.lock(); defer { lock.unlock() }
        guard !purgeFlag else { return nil }
        return current
    }

    // MARK: Validators

    /// The persisted validator for a conditional GET — ONLY meaningful as a
    /// cold-session seed (fresh instances after relaunch). Live sessions hold
    /// their in-flight validators in the transport and commit them to disk
    /// exclusively via the `record*` calls together with the exact body.
    func validator(for key: String) -> String? {
        lock.lock(); defer { lock.unlock() }
        guard !purgeFlag else { return nil }
        return current?.validators[key]
    }

    // MARK: Record (display state + its validators -> durable snapshot)

    /// Commit the list surfaces atomically with the validators guarding the
    /// server bodies they were derived from. `validatorUpdates` maps a
    /// request key to its new ETag (nil = remove the validator).
    ///
    /// `clearListValidators` (root durability review): set when the body being
    /// written is a LOCALLY-TRANSFORMED display state (pending read mask or a
    /// pending/optimistic delete) rather than a raw settled server response.
    /// Such a body does NOT match the raw ETag the last full fetch returned,
    /// so keeping that ETag could make a future process answer `304` and lock
    /// in the wrong badge / keep a row hidden forever. When true, every
    /// whole-list validator is dropped: the next conditional GET is forced to
    /// a full fetch that re-establishes a coherent body+ETag pair. Row
    /// deletion stays durable through the independent tombstone store, and
    /// genuinely-new event rows are a server-true superset that only ever
    /// re-fetches — none of them can hide current server content.
    /// The query variant whose body is currently persisted in the flat
    /// `threads`/`junk` fields, or nil if the list surfaces hold no coherent
    /// committed body. MessageInbox compares this before fetching so a filter
    /// flip (A→B) or a folders↔line change never reuses a stale variant's
    /// ETag against the wrong body (root body/validator review).
    var listSurfaceTag: String? {
        lock.lock(); defer { lock.unlock() }
        return current?.listVariant
    }

    /// Drop ALL whole-list validators (normal/junk/line variants) without
    /// touching per-thread page or call validators. Called before a fetch
    /// whose query variant differs from the persisted body's variant, so the
    /// wrong variant can never be revalidated to a `304` that keeps stale
    /// rows; the subsequent full fetch re-establishes a coherent pair.
    func invalidateListValidators() {
        mutate(savedAt: Date()) { snapshot in
            var next = snapshot
            next.validators = next.validators.filter {
                HistorySnapshot.messageThreadKey(ofValidator: $0.key) != .list
            }
            next.listVariant = nil
            return next
        }
    }

    func recordThreads(threads: [MessageThread]?, junk: [MessageThread]?,
                       hasMoreKeys: Set<String>?,
                       validatorUpdates: [String: String?],
                       clearListValidators: Bool = false,
                       listSurfaceTag: String? = nil,
                       savedAt: Date) {
        mutate(savedAt: savedAt) { snapshot in
            var next = snapshot
            next.schemaVersion = HistoryCache.schemaVersion
            if let threads { next.threads = threads }
            if let junk { next.junk = junk }
            if let hasMoreKeys { next.hasMoreKeys = Array(hasMoreKeys) }
            // A different query variant committed a new list body: the prior
            // variant's ETags no longer describe THIS body, so drop every
            // whole-list validator before recording the new one (root review:
            // an A→B→A flip or relaunch under a different filter could 304 and
            // keep the wrong line's rows otherwise).
            if let listSurfaceTag, next.listVariant != listSurfaceTag {
                next.validators = next.validators.filter {
                    HistorySnapshot.messageThreadKey(ofValidator: $0.key) != .list
                }
            }
            Self.apply(&next.validators, validatorUpdates)
            if clearListValidators {
                next.validators = next.validators.filter {
                    HistorySnapshot.messageThreadKey(ofValidator: $0.key) != .list
                }
            }
            if let listSurfaceTag { next.listVariant = listSurfaceTag }
            return next
        }
    }

    func recordMessages(_ records: [MessageRecord], for key: String,
                        validatorUpdates: [String: String?],
                        savedAt: Date) {
        mutate(savedAt: savedAt) { snapshot in
            var next = snapshot
            next.schemaVersion = HistoryCache.schemaVersion
            if records.isEmpty {
                next.messages.removeValue(forKey: key)
                next.hasMoreKeys.removeAll { $0 == key }
            } else {
                next.messages[key] = records
            }
            Self.apply(&next.validators, validatorUpdates)
            return next
        }
    }

    /// Drop a deleted conversation's cached page AND every validator that
    /// guards any page of that thread key (exact parsed query equality, plus
    /// the legacy bare-peer form, since unqualified keys were fetched under
    /// their qualified candidates).
    func removeMessages(forKey key: String, savedAt: Date) {
        mutate(savedAt: savedAt) { snapshot in
            var next = snapshot
            next.messages.removeValue(forKey: key)
            next.hasMoreKeys.removeAll { $0 == key }
            let peers = [key, ThreadKey.peer(of: key)]
            next.validators = next.validators.filter { entry in
                guard case .thread(let threadKey) = HistorySnapshot.messageThreadKey(ofValidator: entry.key)
                else { return true }
                return !peers.contains(threadKey)
            }
            return next
        }
    }

    func recordCalls(_ calls: [CallRecord],
                     validatorUpdates: [String: String?],
                     savedAt: Date) {
        mutate(savedAt: savedAt) { snapshot in
            var next = snapshot
            next.schemaVersion = HistoryCache.schemaVersion
            next.calls = calls
            Self.apply(&next.validators, validatorUpdates)
            return next
        }
    }

    /// Merge one call event record into the durable history (newer state for
    /// an existing id, or a prepend for a new one), keeping display order.
    func mergeCall(_ call: CallRecord, savedAt: Date) {
        mutate(savedAt: savedAt) { snapshot in
            var next = snapshot
            var calls = snapshot.calls
            if let index = calls.firstIndex(where: { $0.id == call.id }) {
                calls[index] = call
            } else {
                calls.insert(call, at: 0)
            }
            next.calls = calls
            return next
        }
    }

    private func mutate(savedAt: Date, _ transform: (HistorySnapshot) -> HistorySnapshot) {
        lock.lock()
        guard !purgeFlag else { lock.unlock(); return }
        ensureMutableLoadedLocked()
        var snapshot = current ?? HistorySnapshot(savedAtMs: savedAt.unixMilliseconds)
        snapshot.savedAtMs = savedAt.unixMilliseconds
        current = transform(snapshot).trimmed()
        scheduleWriteLocked()
        lock.unlock()
    }

    private static func apply(_ storage: inout [String: String], _ updates: [String: String?]) {
        for (key, etag) in updates {
            if let etag, !etag.isEmpty {
                storage[key] = etag
            } else {
                storage.removeValue(forKey: key)
            }
        }
    }

    // MARK: Settle / teardown / purge

    /// Mandatory synchronous flush at settle points and before teardown.
    /// The actual filesystem write happens ON the serial queue and re-checks
    /// the purge flag there: enqueuing this can never race a concurrent
    /// purge() into a post-purge resurrection (regression-tested). A flush is
    /// FORCED: it settles (and clears) any pending coalesced write; later
    /// delayed callbacks for the same generation are then no-ops, so a
    /// retained old store instance can never re-write the file after a
    /// replacement store purged the scope (root review: reconnect/unpair
    /// swap path).
    func flush() {
        writeQueue.sync { [weak self] in
            self?.performWrite(forced: true)
        }
    }

    /// Remove the entire scoped cache (unpair, binding removal, known
    /// credential revocation, gateway identity mismatch). After a purge no
    /// late write may resurrect the scope: every filesystem touch re-checks
    /// the flag inside the serialized queue, and the file itself is gone.
    func purge() {
        lock.lock()
        purgeFlag = true
        current = nil
        loaded = false
        writePending = false
        lock.unlock()
        writeQueue.sync {
            try? FileManager.default.removeItem(at: self.fileURL)
        }
    }

    // MARK: Internals

    private func ensureMutableLoadedLocked() {
        guard !loaded else { return }
        loaded = true
        if let data = try? Data(contentsOf: fileURL),
           let decoded = try? JSONDecoder.iso.decode(HistorySnapshot.self, from: data),
           decoded.schemaVersion == HistoryCache.schemaVersion {
            current = decoded.trimmed()
        }
    }

    private func scheduleWriteLocked() {
        guard !writePending else { return }
        writePending = true
        writeQueue.asyncAfter(deadline: .now() + writeThrottle) { [weak self] in
            // Runs serialized with purge()/flush(): the flags are re-checked
            // NOW. A coalesced write only writes data that is still pending —
            // if a forced flush already settled the snapshot, this stale
            // callback must not write again and resurrect a scope a
            // replacement store purged (root review: reconnect/unpair swap).
            self?.performWrite(forced: false)
        }
    }

    private func performWrite(forced: Bool) {
        lock.lock()
        let purged = purgeFlag
        // Non-forced (delayed coalesced) writes are skipped once nothing is
        // pending — the forced flush consumed it. A forced flush always writes
        // the current snapshot (idempotent settle) while not purged.
        let shouldWrite = !purged && (forced || writePending)
        writePending = false
        let snapshot = shouldWrite ? current : nil
        lock.unlock()
        guard shouldWrite, let snapshot else { return }
        guard let data = try? JSONEncoder.iso.encode(snapshot) else { return }
        do {
            let dir = fileURL.deletingLastPathComponent()
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            try data.write(to: fileURL,
                           options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
        } catch {
            AppLog.network.error("history save failed")
        }
    }
}

/// Stable cache scope for one paired gateway + authorized device context.
/// Uses the canonical API base (scheme/host/port/path/version) so a host-only
/// match can never collide across port-based deployments or relay prefixes.
///
/// A concrete, non-empty device/pairing principal is REQUIRED: without it
/// several pairings on the same origin+gateway could share one disk
/// namespace. When the device id is missing/empty the durable scope is
/// declined (nil) and the session runs EPHEMERAL — validators stay in memory
/// for the session, nothing is written to disk. The deployed v2 gateway
/// always resolves a concrete per-key deviceId, so normal user caches are
/// unaffected; this only fences degenerate/unbound credentials.
enum HistoryScope {
    /// Returns the scope identifier, or nil when no stable non-secret
    /// device/pairing principal is available (=> no durable history).
    static func identifier(origin: GatewayOrigin, gatewayID: String, deviceID: String?) -> String? {
        guard let deviceID else { return nil }
        let principal = deviceID.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !principal.isEmpty else { return nil }
        return "\(origin.canonicalScopeID)|\(gatewayID)|\(principal)"
    }
}
