import Foundation

/// Build-68 incremental (trickle) ICE exchange for ONE attach.
///
/// It keeps posting the candidates gathered locally after the offer was sent
/// and keeps polling the gateway's own incrementally gathered candidates,
/// both on the existing authenticated JSON plane, until gathering is
/// finished on BOTH sides (or the exchange is cancelled / outlives its bound).
///
/// Design constraints it enforces:
/// * ONE bounded loop per attach (never one task per candidate).
/// * Batches of at most 8 candidates; local candidates are never posted twice
///   and the remote cursor never regresses.
/// * A stale attach (409) ends the exchange immediately: the peer connection
///   it belonged to was replaced, so the remaining candidates are worthless.
/// * A throttled poll (429) just skips a cycle; transient failures back off
///   with a capped delay and never fail the call — the relay or the already
///   exchanged candidates keep working.
@MainActor
final class TrickleCandidateExchange {
    struct Cadence {
        /// Idle time between exchange cycles.
        var intervalSeconds: TimeInterval = 0.15
        /// Hard bound on one exchange (well beyond any real gathering).
        var maximumSeconds: TimeInterval = 90
        /// Consecutive transport failures tolerated before giving up.
        var maximumConsecutiveFailures = 5
        /// First backoff after a transport failure (doubles, capped).
        var initialBackoffSeconds: TimeInterval = 0.5
        /// Cap for the failure backoff.
        var maximumBackoffSeconds: TimeInterval = 4
        /// Max candidates posted per request (mirrors the gateway bound).
        var maximumBatch = 8
    }

    private let cadence: Cadence
    private let log: (String) -> Void
    private var task: Task<Void, Never>?
    private var postedLocal = 0
    private var remoteCursor = 0
    private(set) var finished = false
    /// Real stage evidence: when the exchange started and why it ended.
    private(set) var startedAt: Date?
    private(set) var finishedAt: Date?
    private(set) var postedCount = 0
    private(set) var appliedRemoteCount = 0

    init(cadence: Cadence = Cadence(), log: @escaping (String) -> Void = { _ in }) {
        self.cadence = cadence
        self.log = log
    }

    /// Starts the exchange. `localCandidates` returns every candidate gathered
    /// so far (the exchange is responsible for not repeating them),
    /// `localGatheringComplete` reports whether the local side finished, and
    /// `apply` feeds one gateway candidate to the peer connection.
    func start(attachId: String,
               initialRemoteCursor: Int,
               localCandidates: @escaping () -> [String],
               localGatheringComplete: @escaping () -> Bool,
               post: @escaping (String, [String]) async throws -> Void,
               poll: @escaping (String, Int) async throws -> V2CandidateBatch,
               apply: @escaping (String) -> Void) {
        guard task == nil, !finished else { return }
        postedLocal = 0
        remoteCursor = max(0, initialRemoteCursor)
        startedAt = Date()
        let attachId = attachId
        task = Task { [weak self] in
            guard let self else { return }
            var consecutiveFailures = 0
            var backoff = self.cadence.initialBackoffSeconds
            let deadline = Date().addingTimeInterval(self.cadence.maximumSeconds)
            while !Task.isCancelled, !self.finished, Date() < deadline {
                // 1. Post the local candidates gathered since the last cycle.
                let gathered = localCandidates()
                if gathered.count > self.postedLocal {
                    let fresh = Array(gathered[self.postedLocal...])
                    let batch = Array(fresh.prefix(self.cadence.maximumBatch))
                    do {
                        try await post(attachId, batch)
                        self.postedLocal += batch.count
                        self.postedCount += batch.count
                    } catch let error as APIError {
                        if case .http(let status, _, _) = error, status == 409 {
                            // Stale attach: the negotiation it belonged to is
                            // gone. Stop quietly — nothing to repair here.
                            self.log("trickle exchange stopped: attach superseded")
                            self.finish()
                            return
                        }
                        consecutiveFailures += 1
                    } catch {
                        consecutiveFailures += 1
                    }
                }
                // 2. Drain the gateway's incrementally gathered candidates.
                do {
                    let result = try await poll(attachId, self.remoteCursor)
                    for candidate in result.candidates {
                        apply(candidate)
                        self.appliedRemoteCount += 1
                    }
                    if result.cursor > self.remoteCursor {
                        self.remoteCursor = result.cursor
                    }
                    consecutiveFailures = 0
                    backoff = self.cadence.initialBackoffSeconds
                    let localDone = localGatheringComplete()
                    let allLocalPosted = localCandidates().count <= self.postedLocal
                    if result.complete, localDone, allLocalPosted {
                        self.log("trickle exchange complete posted=\(self.postedCount)"
                                 + " remote=\(self.appliedRemoteCount)")
                        self.finish()
                        return
                    }
                } catch let error as APIError {
                    if case .http(let status, _, _) = error, status == 409 {
                        self.log("trickle exchange stopped: attach superseded")
                        self.finish()
                        return
                    }
                    if case .http(let status, _, _) = error, status == 429 {
                        // Throttled: wait a full cycle, never a failure.
                    } else {
                        consecutiveFailures += 1
                    }
                } catch {
                    consecutiveFailures += 1
                }
                if consecutiveFailures >= self.cadence.maximumConsecutiveFailures {
                    self.log("trickle exchange gave up after \(consecutiveFailures) failures"
                             + " posted=\(self.postedCount) remote=\(self.appliedRemoteCount)")
                    self.finish()
                    return
                }
                let delay = consecutiveFailures == 0
                    ? self.cadence.intervalSeconds : min(backoff, self.cadence.maximumBackoffSeconds)
                if consecutiveFailures > 0 { backoff = min(backoff * 2, self.cadence.maximumBackoffSeconds) }
                try? await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
            }
            if !self.finished {
                self.log("trickle exchange ended at its bound posted=\(self.postedCount)"
                         + " remote=\(self.appliedRemoteCount)")
                self.finish()
            }
        }
    }

    func cancel() {
        task?.cancel()
        task = nil
        finished = true
        finishedAt = finishedAt ?? Date()
    }

    private func finish() {
        finished = true
        finishedAt = Date()
        task = nil
    }
}
