import Foundation

/// MediaRecoveryPolicy is the pure decision layer for the build-48 bounded
/// media recovery. It owns NO networking and is fully deterministic, so the
/// 45 s window, backoff sequence and current-path-first/forced-only rules are
/// unit-testable without a peer connection or socket.
///
/// Contract (user-approved plan):
/// - One ABSOLUTE deadline from the FIRST loss (`window`, default 45 s);
///   repeated loss events never extend it.
/// - Attempt sequence: immediate (delay 0), then 1, 2, 4, 8, 8, … seconds
///   until the deadline.
/// - Auto mode: attempt the CURRENT path first, then the alternate; forced
///   direct/relay mode never crosses routes.
/// - Recovery succeeds ONLY on fresh-media proof supplied by the owner (an
///   inbound media frame after the attempt), never on `.connected`/RTT alone.
/// - Revoked authorization, explicit local/remote end: terminal immediately.
/// - Same call id only; the coordinator never dials a new call.
struct MediaRecoveryPolicy: Sendable {
    enum Route: Equatable { case relay, direct }
    enum Kind: Equatable { case call, conference }

    let window: TimeInterval
    let backoff: [TimeInterval]
    let immediate: TimeInterval

    init(window: TimeInterval = 45,
         backoff: [TimeInterval] = [1, 2, 4, 8],
         immediate: TimeInterval = 0) {
        self.window = window
        self.backoff = backoff
        self.immediate = immediate
    }

    /// delayBefore returns the wait BEFORE attempt `index` (0-based).
    /// Attempt 0 is immediate; later attempts follow the capped backoff.
    func delayBefore(attempt index: Int) -> TimeInterval {
        if index <= 0 { return immediate }
        let step = index - 1
        if step >= backoff.count { return backoff.last ?? 8 }
        return backoff[step]
    }

    /// routesFor returns the attempt order. Auto: current first then alternate
    /// (alternate is nil when the gateway only offers one path). Forced: the
    /// chosen route only.
    func routesFor(mode: MediaRouteMode, current: Route, alternateAvailable: Bool) -> [Route] {
        switch mode {
        case .direct:
            return [.direct]
        case .relay:
            return [.relay]
        case .auto:
            var order: [Route] = [current]
            if alternateAvailable {
                let alt: Route = current == .relay ? .direct : .relay
                order.append(alt)
            }
            return order
        }
    }

    /// isWithinDeadline reports whether an attempt at `now` may still run.
    func isWithinDeadline(startedAt: Date, now: Date = Date()) -> Bool {
        now.timeIntervalSince(startedAt) <= window
    }
}

/// MediaRecoveryController drives one bounded recovery episode for the
/// coordinator. It is @MainActor, generation-fenced by the call `id`, and its
/// tasks are replaced only on `begin` (repeated loss while an episode is
/// active is ignored, so the absolute deadline never moves).
///
/// Build-49 review fixes:
/// - The absolute deadline is enforced by an INDEPENDENT task that fires at
///   firstLoss + window regardless of what the attempt loop is doing (a
///   hung/slow offer or stalled WSS `ready` can no longer park the window),
///   and at expiry the in-flight attempt loop is cancelled IMMEDIATELY, so a
///   late candidate can never install or prove anything for the dead episode.
/// - All deadline math uses a MONOTONIC uptime clock
///   (`ProcessInfo.processInfo.systemUptime`), never wall `Date()`: a
///   wall-clock change (user travel/NTP step) cannot extend or shrink the
///   45 s window. `Date` remains only on the display-metadata accessor.
/// - Expiry is fenced by (callId, episodeID): a stale timer can never end a
///   recovered or restarted episode.
/// - `succeed` rejects proof that arrives after the absolute deadline.
/// - Attempts are strictly serialized with the 1/2/4/8 s backoff (bounded to
///   the remaining window) and NO extra settle sleep is silently added.
@MainActor
final class MediaRecoveryController {
    struct Attempt: Equatable, Sendable {
        let index: Int
        let route: MediaRecoveryPolicy.Route
        /// Full proof identity (call + episode UUID + attempt id). Fresh-media
        /// proof must carry the SAME identity, so neither a superseded attempt
        /// nor a previous episode can clear a newer recovery.
        let identity: ProofIdentity
        var attemptID: Int { identity.attemptID }
    }

    private let policy: MediaRecoveryPolicy
    /// ProofIdentity is captured WHEN a transport is created and carried by
    /// its fresh-media closure. It binds the emission to one episode, one
    /// attempt AND (at the coordinator) one transport instance, so neither a
    /// stale episode nor a superseded socket/peer can clear a newer attempt.
    struct ProofIdentity: Equatable, Sendable {
        let callId: String
        let episodeID: UUID
        let attemptID: Int
    }

    private var task: Task<Void, Never>?
    /// Independent absolute-deadline task (build 49). Fires at
    /// startedAt + window no matter what the attempt loop is awaiting.
    private var expireTask: Task<Void, Never>?
    /// Build-66: the interruptible wait preceding the NEXT attempt. A path
    /// change nudges it (`retryNow`) so the fresh path is attempted promptly
    /// instead of waiting out the current backoff step. `nil` while an
    /// attempt is actually in flight — attempts stay strictly serialized.
    private var waitTask: Task<Void, Never>?
    /// True while `onAttempt` is being awaited (the attempt owns the single
    /// audio-owner path).
    private(set) var attemptInFlight = false
    /// Owner-installed interrupt for the in-flight attempt: cancels the
    /// candidate transport being negotiated over the dead path. Fenced by
    /// the owner against the live (call, episode, attempt) identity.
    private var attemptInterrupt: (@MainActor () -> Void)?
    /// Set when a nudge arrives while an attempt is in flight: the NEXT
    /// attempt runs immediately (the current one was interrupted or already
    /// finishing), still inside the absolute deadline.
    private var runNextAttemptImmediately = false
    /// Rate limit for nudges: a burst of path edges can never become an
    /// attempt storm.
    var nudgeCooldown: TimeInterval = 0.5
    private var lastNudgeUptime: TimeInterval?
    private(set) var activeCallId: String?
    /// Monotonic episode start (system uptime seconds); nil when idle.
    private(set) var startedUptime: TimeInterval?
    private var attemptCount = 0
    private(set) var episodeID = UUID()
    /// Identity of the most recently STARTED attempt; fresh-media proof for
    /// an older attempt is rejected so a queued callback can never clear
    /// recovery for a newer in-flight attempt.
    private(set) var currentAttemptID = 0
    private var cancelled = false
    /// Monotonic uptime clock, injectable for deterministic tests. Wall
    /// `Date()` is NEVER used for deadline math (NTP/travel steps must not
    /// move the 45 s window).
    var uptime: () -> TimeInterval = { ProcessInfo.processInfo.systemUptime }

    init(policy: MediaRecoveryPolicy = MediaRecoveryPolicy()) {
        self.policy = policy
    }

    var isActive: Bool { activeCallId != nil }

    /// Identity of the currently in-flight attempt (nil when idle). Fresh
    /// media proof must carry exactly this identity — and the coordinator
    /// additionally requires the emitting transport to be the installed one,
    /// so a callback queued by an OLD socket/peer cannot claim it.
    var currentProofIdentity: ProofIdentity? {
        guard let callId = activeCallId else { return nil }
        return ProofIdentity(callId: callId, episodeID: episodeID, attemptID: currentAttemptID)
    }

    /// begin opens the episode unless one is already live for this same id.
    /// onAttempt performs ONE reconnect and is awaited to completion before
    /// the next attempt is scheduled (serialized attempts — never two
    /// concurrent media graphs). The owner independently calls `succeed`
    /// (carrying the attempt identity) when fresh media proves the path.
    /// onExpire runs exactly once at the absolute deadline unless the episode
    /// recovered/cancelled first — the deadline task is independent of the
    /// attempt loop, so a hung attach can never delay expiry.
    func begin(callId: String,
               mode: MediaRouteMode,
               current: MediaRecoveryPolicy.Route,
               alternateAvailable: Bool,
               onAttempt: @escaping @MainActor (Attempt) async -> Void,
               onExpire: @escaping @MainActor () async -> Void) {
        if activeCallId == callId {
            return // repeated loss must not extend/restart
        }
        cancel()
        activeCallId = callId
        let start = uptime()
        startedUptime = start
        attemptCount = 0
        currentAttemptID = 0
        episodeID = UUID() // new episode identity: attempt ids reset safely
        cancelled = false
        let routes = policy.routesFor(mode: mode, current: current,
                                      alternateAvailable: alternateAvailable)
        let deadline = start + policy.window
        let episode = episodeID
        lastNudgeUptime = nil
        waitTask = nil
        attemptInFlight = false
        runNextAttemptImmediately = false
        attemptInterrupt = nil

        // Independent absolute-deadline task. Fenced by the episode identity:
        // recovery (succeed -> cancel), a newer begin, or teardown all cancel
        // this task, so a stale timer can never end a recovered/restarted
        // episode. When it does fire, the episode ends THEN onExpire runs —
        // any proof delivered afterwards is rejected by succeed().
        expireTask = Task { [weak self] in
            let remaining = deadline - (self?.uptime() ?? 0)
            if remaining > 0 {
                try? await Task.sleep(nanoseconds: UInt64(remaining * 1_000_000_000))
            }
            guard let self, !Task.isCancelled, !self.cancelled,
                  self.activeCallId == callId, self.episodeID == episode else { return }
            // Episode ends at the absolute deadline even with an attempt
            // still in flight (a hung attach must not extend the window), and
            // the attempt loop is cancelled NOW so a late candidate can never
            // install media or deliver proof for the dead episode.
            self.activeCallId = nil
            self.task?.cancel()
            self.task = nil
            await onExpire()
        }

        task = Task { [weak self] in
            guard let self else { return }
            var index = 0
            var skipNextWait = false
            while !Task.isCancelled {
                guard !self.cancelled, self.activeCallId == callId else { return }
                // Wait before this attempt (0 for the first), but never past
                // the absolute deadline. A consumed fresh-path nudge skips
                // the wait exactly once (still deadline-bounded above).
                let delay = skipNextWait ? 0 : self.policy.delayBefore(attempt: index)
                skipNextWait = false
                if delay > 0 {
                    let remaining = deadline - self.uptime()
                    let wait = min(delay, max(0, remaining))
                    if wait <= 0 { break }
                    // Build-66: the inter-attempt wait is a separate,
                    // cancellable task so a path-change nudge
                    // (retryNow) shortens ONLY the wait — never an
                    // in-flight attempt, never the absolute deadline.
                    let waiter = Task {
                        _ = try? await Task.sleep(nanoseconds: UInt64(wait * 1_000_000_000))
                    }
                    self.waitTask = waiter
                    await waiter.value
                    self.waitTask = nil
                    guard !Task.isCancelled, !self.cancelled,
                          self.activeCallId == callId else { return }
                }
                guard !Task.isCancelled, !self.cancelled, self.activeCallId == callId else { return }
                if self.uptime() >= deadline { break }
                // One attempt per route in order; after the last route the
                // next backoff step begins again from the current route.
                let route = routes[index % max(1, routes.count)]
                self.attemptCount += 1
                let attemptID = self.attemptCount
                self.currentAttemptID = attemptID
                let identity = ProofIdentity(callId: callId, episodeID: self.episodeID,
                                              attemptID: attemptID)
                // Serialized: await the full attach before the next attempt.
                // Fresh-media proof may arrive DURING this await and close the
                // episode via succeed() (which cancels this task); the guards
                // after the await exit the loop in that case. Expiry cancels
                // this task mid-await too. There is no extra settle sleep: the
                // backoff intervals are exactly the approved 1/2/4/8 s, each
                // bounded to the remaining window.
                self.attemptInFlight = true
                await onAttempt(Attempt(index: index, route: route, identity: identity))
                self.attemptInFlight = false
                if self.runNextAttemptImmediately {
                    // A fresh-path nudge interrupted this attempt: run the
                    // NEXT attempt now instead of waiting the backoff. The
                    // flag is consumed exactly once (no storm) and the
                    // absolute deadline still bounds the attempt below.
                    self.runNextAttemptImmediately = false
                    skipNextWait = true
                }
                index += 1
                guard !Task.isCancelled, !self.cancelled, self.activeCallId == callId else { return }
            }
        }
    }

    /// succeed closes the episode on fresh-media proof; it accepts only proof
    /// whose FULL identity (call, episode, attempt) is the current one AND
    /// which arrives within the absolute (monotonic) deadline, so a callback
    /// queued by an old socket/peer, a previous episode, or a late frame
    /// after expiry can never succeed a newer attempt (or resurrect an
    /// expired one). Returns true when it closed the episode.
    @discardableResult
    func succeed(_ identity: ProofIdentity) -> Bool {
        guard let start = startedUptime, let current = currentProofIdentity,
              current == identity, uptime() <= start + policy.window else { return false }
        cancel()
        return true
    }

    /// Marks the current attempt identity (called by the owner right before
    /// starting the attach so the transport closure captures the exact id).
    func setCurrentAttempt(_ id: Int) {
        currentAttemptID = id
    }

    /// Build-66: nudge the episode so the NEXT attempt runs now — used when a
    /// freshly usable network path appears (Wi-Fi ↔ cellular) so recovery
    /// never waits out a stale backoff step OR a stale in-flight attach on
    /// the dead path. Semantics:
    ///  - an inter-attempt WAIT is cancelled immediately;
    ///  - an IN-FLIGHT attempt is interrupted through the owner-installed,
    ///    identity-fenced interrupt (the candidate transport being negotiated
    ///    over the dead path is closed) and the next attempt then runs
    ///    immediately — never a second concurrent attempt, and the absolute
    ///    45 s deadline is unchanged;
    ///  - rate limited (`nudgeCooldown`) so duplicate path edges cannot
    ///    build an attempt storm.
    func retryNow() {
        guard activeCallId != nil else { return }
        let now = uptime()
        if let last = lastNudgeUptime, now - last < nudgeCooldown { return }
        if let waiter = waitTask {
            lastNudgeUptime = now
            waiter.cancel()
            return
        }
        guard attemptInFlight else { return }
        lastNudgeUptime = now
        runNextAttemptImmediately = true
        attemptInterrupt?()
    }

    /// Owner-installed interrupt for the in-flight attempt (build 66).
    /// The owner MUST fence the handler on the live proof identity before
    /// touching any transport, so a superseded attempt's candidate is never
    /// closed by a newer episode's nudge.
    func setAttemptInterrupt(_ handler: (@MainActor () -> Void)?) {
        attemptInterrupt = handler
    }

    func cancel() {
        cancelled = true
        task?.cancel()
        task = nil
        waitTask?.cancel()
        waitTask = nil
        expireTask?.cancel()
        expireTask = nil
        activeCallId = nil
        startedUptime = nil
        attemptCount = 0
        currentAttemptID = 0
        lastNudgeUptime = nil
        attemptInFlight = false
        runNextAttemptImmediately = false
        attemptInterrupt = nil
    }

    /// deadlineEpoch is display metadata ONLY (wall clock). All deadline
    /// decisions use the monotonic `startedUptime` + window.
    var deadlineEpoch: Date? {
        startedUptime.map { _ in Date().addingTimeInterval(policy.window) }
    }

    var attempts: Int { attemptCount }
}
