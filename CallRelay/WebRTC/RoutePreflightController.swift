import Foundation
import AVFoundation

/// Foreground, call-INDEPENDENT direct-path preflight.
///
/// While the app is foreground and no call is active, this controller keeps
/// a detached probe peer connection attached to the gateway's device-scoped
/// preflight endpoint so dial/answer route selection starts from FRESH
/// measurements instead of a cold probe after the relay connects.
///
/// Hard guarantees (2026-10-05 routing policy):
/// * It NEVER captures the microphone, NEVER claims `AVAudioSession` and
///   NEVER starts an audio graph — the probe track stays disabled and
///   `RTCAudioSession.useManualAudio` stays installed, so an idle
///   measurement cannot disturb a running engine or activate the mic.
/// * It NEVER creates a call, sends an SMS or generates any modem traffic.
/// * A data-channel echo measures reachability and RTT only; it is NOT
///   proof the audio pipeline is ready, and no UI copy may claim that.
/// * Everything is generation-fenced: backgrounding, a call starting, or a
///   newer cycle promptly cancels the previous probe (server TTL bounds any
///   orphan server-side).
///
/// Lifecycle: `appDidEnterForeground` starts a bounded measurement cycle.
/// Once connected, the SAME candidate is kept continuously adoptable by
/// renewing its server TTL in place (no renegotiation, same id/peer
/// connection) up to a bounded number of renewals; a renewal failure falls
/// back to a fresh attach. `appDidEnterBackground` stops everything.
/// At call start the coordinator transfers ownership through
/// `handoffForCall()`; the route controller then commit-adopts or discards it.
@MainActor
final class RoutePreflightController {
    /// A fresh, connected, device-scoped candidate ready for call adoption.
    struct Handoff {
        let probe: DirectProbeControlling
        let preflightId: String
        let attachedAt: Date
        /// Server-side expiry as reported by the attach/renew response minus
        /// a small client safety margin; nil when the gateway did not send a
        /// TTL (older gateway) — the client freshness window still applies.
        let expiresAt: Date?
        /// Fresh app-level echo RTT samples (seconds) at handoff time.
        let samples: [TimeInterval]
        /// Build 68: the network path identity this candidate was measured
        /// on. A different path means the measurements are stale by
        /// definition and the caller must not adopt them as fresh.
        let pathGeneration: UInt64
    }

    struct Cadence {
        /// Upper bound on one echo-sampling/warm stretch before a renewal
        /// checkpoint. The actual hold is also bounded by the server TTL.
        var warmSeconds: TimeInterval = 35
        /// Idle time between bounded warm chains.
        var cooldownSeconds: TimeInterval = 45
        /// Bound on the connect wait after the answer is applied.
        var connectTimeout: TimeInterval = 8
        /// Minimum echo samples before a cycle reports measurable.
        var minimumSamples = 2
        /// Echo-collection window per cycle.
        var measureSeconds: TimeInterval = 4
        /// A handoff is fresh for this long after attach (or after its last
        /// fresh echo, whichever is newer, so a renewed candidate stays
        /// usable without renegotiation).
        var handoffFreshSeconds: TimeInterval = 40
        /// Client safety margin under the reported server TTL: the handoff is
        /// never handed out with less than this much server validity left.
        var ttlSafetySeconds: TimeInterval = 4
        /// Build 68: a HEALTHY candidate is renewed continuously instead of
        /// being torn down and renegotiated at a renewal-chain cap (the old
        /// cap forced a full gather+handshake every ~3 minutes on a perfectly
        /// usable path). The resource bound is the ONE candidate itself; a
        /// rebuild now happens only when the evidence is stale/unhealthy or
        /// the server refuses the renewal.
        var maximumRenewalsPerChain = Int.max
        /// Renewals are attempted while the candidate still has genuine
        /// evidence: a fresh echo sample (or a fresh attach) within this
        /// window. Beyond it the candidate is unhealthy and gets rebuilt.
        var healthyEchoSeconds: TimeInterval = 20
        /// Pause between a renewal-failed rebuild and the next attach attempt.
        var renewPauseSeconds: TimeInterval = 0.25
        /// Max consecutive immediate rebuilds before a full cooldown.
        var maximumConsecutiveRebuilds = 3
        /// Build 68: capped exponential backoff after failed rebuilds, so a
        /// gateway that is down is not hammered.
        var backoffSeconds: TimeInterval = 1
        var maximumBackoffSeconds: TimeInterval = 30
        /// Upper bound on how long the GLOBAL preflight ICE configuration
        /// stays usable. The effective lifetime is
        /// `min(this, server expiresAt - iceCacheSafetySeconds)`: the
        /// GATEWAY's own expiry always wins, and a payload with a missing,
        /// malformed or already-passed expiry is NEVER cached (it may carry
        /// credentials).
        var iceCacheSeconds: TimeInterval = 300
        /// Safety margin subtracted from the server-provided expiry.
        var iceCacheSafetySeconds: TimeInterval = 30
    }

    private enum CycleOutcome {
        /// The candidate was held and renewed to the configured chain cap.
        case renewCapReached
        /// The server no longer owns the candidate: rebuild now.
        case needsRebuild
        /// Attach/connect failed (a short backoff already ran).
        case unavailable
        /// Ownership moved to a call: stop the cycle.
        case consumed
        /// Generation/cancellation superseded this run.
        case stopped
    }

    private let api: GatewayAPI
    private let probeFactory: @MainActor () -> DirectProbeControlling
    private let cadence: Cadence
    private let eligible: () -> Bool
    private let log: (String) -> Void
    /// UI observation of the idle direct-path measurement. Emitted only on
    /// real phase/sample changes (no polling), so the connection screen can
    /// show the measured candidate without opening a call.
    var onUpdate: ((RoutePreflightSnapshot) -> Void)?

    private var generation: UInt64 = 0
    private var cycleTask: Task<Void, Never>?
    private var probe: DirectProbeControlling?
    private var preflightId: String?
    private var attachedAt: Date?
    /// Build 68 incremental exchange for the CURRENT candidate (nil when the
    /// gateway does not advertise trickle or the attach was full-SDP).
    private var trickleExchange: TrickleCandidateExchange?
    /// Network path identity captured at attach time.
    private var attachedPathGeneration: UInt64?
    /// Build-68 attach token of the CURRENT candidate (nil for a full-SDP
    /// attach).
    private var attachId: String?
    private var currentPathGeneration: () -> UInt64 = { 0 }
    /// Bounded cache of the GLOBAL preflight ICE configuration
    /// (`GET /ice`, no call id, no minted per-call credentials). Every entry
    /// is keyed by the network-path generation it was fetched on and expires
    /// after `iceCacheSeconds`; a network change or expiry refetches. The
    /// per-call ICE payload (with its per-call IDs/credentials) is NEVER
    /// cached here.
    private var cachedICE: (config: ICEConfiguration, pathGeneration: UInt64, fetchedAt: Date)?
    private var iceCacheHits = 0
    private var iceCacheMisses = 0
    /// Server TTL deadline (client-margined) for the current candidate.
    private var probeExpiry: Date?
    /// True once the current probe's ownership moved to a call handoff.
    private var ownershipTransferred = false

    init(api: GatewayAPI,
         cadence: Cadence = Cadence(),
         pathGeneration: @escaping () -> UInt64 = { 0 },
         eligible: @escaping () -> Bool = { true },
         probeFactory: @escaping @MainActor () -> DirectProbeControlling = { MediaProbeController() },
         log: @escaping (String) -> Void = { message in
             Task { @MainActor in DiagnosticsStore.shared.log("route", message) }
         }) {
        self.api = api
        self.cadence = cadence
        self.currentPathGeneration = pathGeneration
        self.eligible = eligible
        self.probeFactory = probeFactory
        self.log = log
    }

    var isAttached: Bool { probe != nil }

    /// Fresh connected candidate for an imminent call, if one exists.
    var freshHandoff: Handoff? {
        guard let probe, let preflightId, let attachedAt,
              probe.connected, !ownershipTransferred,
              (Date().timeIntervalSince(attachedAt) <= cadence.handoffFreshSeconds
                || (probe.latestQualitySample.map {
                    Date().timeIntervalSince($0.at) <= cadence.handoffFreshSeconds
                } ?? false))
        else { return nil }
        // A candidate measured on a DIFFERENT network path is stale evidence:
        // never hand it out as fresh (a Wi-Fi→cellular switch must be
        // re-measured, not adopted).
        if let attachedPathGeneration, attachedPathGeneration != currentPathGeneration() {
            return nil
        }
        if let probeExpiry, Date() >= probeExpiry {
            // The server TTL lapsed (or the renewal never landed): this
            // candidate can no longer be committed; never hand it out.
            return nil
        }
        return Handoff(probe: probe, preflightId: preflightId,
                       attachedAt: attachedAt, expiresAt: probeExpiry,
                       samples: probe.freshQualitySamples(within: 10, now: Date()),
                       pathGeneration: attachedPathGeneration ?? currentPathGeneration())
    }

    /// Consumes the fresh candidate: ownership moves to the caller (the
    /// call's route controller), which will commit-adopt or discard it.
    func consumeHandoff() -> Handoff? {
        guard let handoff = freshHandoff else { return nil }
        ownershipTransferred = true
        // The call takes over the peer connection; the idle exchange stops
        // here so no candidate is posted for a commit in flight.
        trickleExchange?.cancel()
        trickleExchange = nil
        probe = nil
        self.preflightId = nil
        attachedAt = nil
        probeExpiry = nil
        attachedPathGeneration = nil
        // The cycle loop stops at the next generation/idle checkpoint; the
        // call-start generation bump happens in `callWillStart`.
        log("preflight handoff consumed id=\(handoff.preflightId)")
        return handoff
    }

    /// Call starting: stop the idle cycle and transfer ownership of a fresh
    /// candidate in ONE step. The handoff (if any) is preserved until the
    /// caller adopts or discards it; a candidate that is not handed over is
    /// cancelled immediately, so no probe outlives its usefulness.
    func handoffForCall() -> Handoff? {
        let handoff = consumeHandoff()
        generation &+= 1
        cycleTask?.cancel()
        cycleTask = nil
        if handoff == nil, !ownershipTransferred {
            cancelProbe(serverDiscard: true)
        }
        publish(RoutePreflightSnapshot(phase: .stopped))
        return handoff
    }

    /// Call starting without taking a handoff: cancel the warm cycle promptly
    /// (ownership either already transferred via `consumeHandoff`, or the
    /// probe is dead weight).
    func callWillStart() {
        generation &+= 1
        cycleTask?.cancel()
        cycleTask = nil
        if !ownershipTransferred { cancelProbe(serverDiscard: true) }
        publish(RoutePreflightSnapshot(phase: .stopped))
    }

    func appDidEnterBackground() {
        generation &+= 1
        cycleTask?.cancel()
        cycleTask = nil
        // Background releases the cached configuration too: a long
        // background stay may cross network changes we never observed.
        invalidateICECache()
        cancelProbe(serverDiscard: true)
        publish(RoutePreflightSnapshot(phase: .stopped))
        log("preflight stopped (background)")
    }

    /// Foreground entry point: starts the bounded measurement cycle when
    /// eligible. Safe to call repeatedly (foreground notifications,
    /// incoming-answer revalidation kicks).
    func appDidEnterForeground() {
        guard eligible(), cycleTask == nil, probe == nil else { return }
        let gen = generation
        cycleTask = Task { [weak self] in await self?.cycle(gen: gen) }
    }

    /// Incoming wake/answer revalidation: identical to foreground; it never
    /// delays the Apple incoming report (this runs alongside it).
    func revalidate() {
        appDidEnterForeground()
    }

    /// Explicit user "重新检测": drop the current candidate and start a fresh
    /// bounded measurement cycle now. Foreground idle only — `eligible`
    /// still gates (never during a call, never against a pinned relay), and
    /// no microphone/audio engine is touched.
    func recheck() {
        generation &+= 1
        cycleTask?.cancel()
        cycleTask = nil
        invalidateICECache()
        cancelProbe(serverDiscard: true)
        publish(RoutePreflightSnapshot(phase: .probing))
        appDidEnterForeground()
    }

    // MARK: - Cycle

    /// The global preflight ICE configuration, from the bounded cache when it
    /// is still valid for the CURRENT network path; otherwise fetched (and
    /// stored) fresh. Only this call-independent payload is cached.
    private func cachedICEConfiguration() async throws -> ICEConfiguration {
        let pathGeneration = currentPathGeneration()
        let lifetimeGeneration = generation
        if let cached = cachedICE,
           cached.pathGeneration == pathGeneration,
           Date().timeIntervalSince(cached.fetchedAt) < effectiveCacheLifetime(cached.config) {
            iceCacheHits += 1
            return cached.config
        }
        iceCacheMisses += 1
        let config = try await api.iceConfiguration()
        // Guard the store: a fetch that straddled an invalidation, a
        // background transition or a cycle stop must NOT repopulate the cache
        // for a state that no longer exists.
        guard lifetimeGeneration == generation else { return config }
        if isCacheable(config) {
            cachedICE = (config, pathGeneration, Date())
        } else {
            // No usable server expiry (or an already-expired one): never keep
            // it — the payload may carry credentials.
            cachedICE = nil
        }
        return config
    }

    /// Effective lifetime of one cached payload: the local upper bound or the
    /// GATEWAY's own expiry minus the safety margin, whichever is SHORTER.
    /// It is only ever called for cacheable payloads.
    private func effectiveCacheLifetime(_ config: ICEConfiguration) -> TimeInterval {
        guard let expiry = config.expiryDate else { return 0 }
        let serverBound = expiry.timeIntervalSinceNow - cadence.iceCacheSafetySeconds
        return min(cadence.iceCacheSeconds, max(0, serverBound))
    }

    /// A payload is only cacheable when the gateway supplied a parseable,
    /// still-future expiry with real margin left.
    private func isCacheable(_ config: ICEConfiguration) -> Bool {
        effectiveCacheLifetime(config) > 0
    }

    /// Real cache evidence for the stage log (hits/misses since launch).
    var iceCacheStats: (hits: Int, misses: Int) { (iceCacheHits, iceCacheMisses) }

    /// Invalidates the cached configuration (network change, logout or an
    /// explicit recheck): the next handshake refetches. The bump of
    /// `generation` also fences any in-flight fetch, so an answer that lands
    /// after the invalidation can never repopulate the cache.
    func invalidateICECache() {
        cachedICE = nil
        generation &+= 1
    }

    private func publish(_ snapshot: RoutePreflightSnapshot) {
        onUpdate?(snapshot)
    }

    private func cycle(gen: UInt64) async {
        var consecutiveRebuilds = 0
        var backoff = cadence.backoffSeconds
        while !Task.isCancelled, gen == generation {
            guard eligible() else { return }
            let outcome = await runOnce(gen: gen)
            guard !Task.isCancelled, gen == generation else { return }
            switch outcome {
            case .consumed, .stopped:
                return
            case .needsRebuild:
                // The server lost the candidate (or refused a renewal): a
                // rebuild is the honest response, but repeated failures must
                // not hammer the gateway — capped exponential backoff, and a
                // long cooldown only after the consecutive-rebuild cap.
                consecutiveRebuilds += 1
                if consecutiveRebuilds >= cadence.maximumConsecutiveRebuilds {
                    consecutiveRebuilds = 0
                    backoff = cadence.backoffSeconds
                    log("preflight rebuild cap reached; backing off \(Int(cadence.cooldownSeconds))s")
                    try? await Task.sleep(nanoseconds: UInt64(cadence.cooldownSeconds * 1_000_000_000))
                } else {
                    try? await Task.sleep(nanoseconds: UInt64(backoff * 1_000_000_000))
                    backoff = min(backoff * 2, cadence.maximumBackoffSeconds)
                }
            case .unavailable:
                // The path/gateway is not usable right now: keep the measured
                // candidate (if any) and re-measure with the same capped
                // backoff instead of a tight retry loop.
                consecutiveRebuilds = 0
                backoff = cadence.backoffSeconds
                try? await Task.sleep(nanoseconds: UInt64(cadence.cooldownSeconds * 1_000_000_000))
            case .renewCapReached:
                consecutiveRebuilds = 0
                backoff = cadence.backoffSeconds
                try? await Task.sleep(nanoseconds: UInt64(cadence.cooldownSeconds * 1_000_000_000))
            }
        }
    }

    /// One bounded warm chain: attach, connect, measure, then hold the SAME
    /// candidate by renewing its server TTL in place until the chain cap or a
    /// renewal failure. Returns why the chain ended; `defer` releases the
    /// candidate unless ownership already moved to a call.
    private func runOnce(gen: UInt64) async -> CycleOutcome {
        let probe = probeFactory()
        self.probe = probe
        ownershipTransferred = false
        publish(RoutePreflightSnapshot(phase: .probing))
        defer {
            if self.probe === probe {
                cancelProbe(serverDiscard: true)
                publish(RoutePreflightSnapshot(phase: .stopped))
            }
        }
        let cycleStarted = Date()
        // A cycle that still owns a connected, previously measured candidate is
        // a WARM revalidation; a cycle that starts from scratch is COLD.
        let hadMeasuredCandidate = self.probe != nil
        var configMs = 0
        do {
            let configStart = Date()
            let ice = try await cachedICEConfiguration()
            configMs = Int(Date().timeIntervalSince(configStart) * 1000)
            guard ice.mediaTransports?.contains("ice") == true else {
                log("preflight skipped: gateway advertises no direct path")
                publish(RoutePreflightSnapshot(phase: .unavailable(String(localized: "网关未提供直连路径"))))
                return .unavailable
            }
            guard !Task.isCancelled, gen == generation else { return .stopped }
            // Build 68 incremental handshake: when the gateway advertises
            // trickle (and the probe supports it) the offer goes out as soon
            // as the local description exists — no gather wait — and the
            // remaining candidates travel in BOTH directions while the
            // connection checks already run. Anything else keeps the exact
            // full-SDP behavior.
            var trickleProbe: TrickleProbeCapable?
            if ice.mediaTrickle, api is TrickleMediaAPI, let capable = probe as? TrickleProbeCapable {
                trickleProbe = capable
            }
            let offerStart = Date()
            var offer: String
            var attachId: String?
            var remoteCursor = 0
            var attachTrickle = false
            if let trickleProbe {
                let built = try await trickleProbe.makeOfferTrickle(ice: ice)
                offer = built.sdp
                attachTrickle = true
            } else {
                offer = try await probe.makeOffer(ice: ice)
            }
            let gatherMs = Int(Date().timeIntervalSince(offerStart) * 1000)
            guard !Task.isCancelled, gen == generation else { return .stopped }
            let attachStart = Date()
            let trickleAPI = api as? TrickleMediaAPI
            let answer: V2PreflightAnswer
            if attachTrickle, let trickleAPI {
                answer = try await trickleAPI.attachMediaPreflight(sdp: offer, trickle: true)
            } else {
                answer = try await api.attachMediaPreflight(sdp: offer)
            }
            let attachMs = Int(Date().timeIntervalSince(attachStart) * 1000)
            guard !Task.isCancelled, gen == generation else {
                // Background raced the attach: tell the server to drop it.
                try? await api.discardMediaPreflight(preflightId: answer.preflightId)
                return .stopped
            }
            preflightId = answer.preflightId
            probeExpiry = Self.expiry(ttlMs: answer.ttlMs, margin: cadence.ttlSafetySeconds,
                                      now: Date())
            attachedPathGeneration = currentPathGeneration()
            try await probe.applyAnswer(answer.sdp)
            // The incremental exchange runs CONCURRENTLY with the connection
            // establishment: the gateway's candidates arrive while the local
            // ones are still being posted.
            if let trickleProbe, let attachId = answer.attachId {
                remoteCursor = answer.candidateCursor ?? 0
                self.attachId = attachId
                let exchange = TrickleCandidateExchange(log: log)
                trickleExchange = exchange
                let preflightID = answer.preflightId
                exchange.start(
                    attachId: attachId,
                    initialRemoteCursor: remoteCursor,
                    localCandidates: { [weak trickleProbe] in
                        trickleProbe?.locallyGatheredCandidates ?? []
                    },
                    localGatheringComplete: { [weak trickleProbe] in
                        trickleProbe?.gatheringFinished ?? true
                    },
                    post: { [trickleAPI] attachId, candidates in
                        guard let trickleAPI else { throw APIError.notReady("当前配对不支持增量直连协商。") }
                        try await trickleAPI.postPreflightCandidates(
                            preflightId: preflightID, attachId: attachId, candidates: candidates)
                    },
                    poll: { [trickleAPI] attachId, after in
                        guard let trickleAPI else { throw APIError.notReady("当前配对不支持增量直连协商。") }
                        return try await trickleAPI.pollPreflightCandidates(
                            preflightId: preflightID, attachId: attachId, after: after)
                    },
                    apply: { [weak trickleProbe] candidate in
                        trickleProbe?.addRemoteCandidate(candidate)
                    })
            }
            let connected = await waitConnected(probe, gen: gen)
            guard connected else {
                log("preflight did not connect in time gatherMs=\(gatherMs) attachMs=\(attachMs)"
                    + " trickle=\(attachTrickle)")
                publish(RoutePreflightSnapshot(phase: .unavailable(String(localized: "直连探测未连通"))))
                return .unavailable
            }
            attachedAt = Date()
            let connectMs = Int(Date().timeIntervalSince(cycleStarted) * 1000)
            // Cold/warm is a REAL distinction: a warm attach comes from an
            // already-measured candidate revalidation, a cold one starts from
            // nothing. The cache hit/miss counters are measured, never
            // fabricated.
            let warm = hadMeasuredCandidate
            log("preflight attached cold=\(!warm ? 1 : 0) warm=\(warm ? 1 : 0)"
                + " id=\(answer.preflightId) configMs=\(configMs)"
                + " gatherMs=\(gatherMs) attachMs=\(attachMs) connectMs=\(connectMs)"
                + " firstFrameReadyMs=\(connectMs) ttlMs=\(answer.ttlMs)"
                + " trickle=\(attachTrickle ? 1 : 0)"
                + " iceCacheHits=\(iceCacheHits) iceCacheMisses=\(iceCacheMisses)")
            publish(connectedSnapshot(probe))
            // Collect echo samples: the probe pings once per second once the
            // echo channel opens; wait for the bounded measure window.
            let measureEnd = Date().addingTimeInterval(cadence.measureSeconds)
            while Date() < measureEnd, !Task.isCancelled, gen == generation {
                if probe.freshQualitySamples(within: 60, now: Date()).count >= cadence.minimumSamples { break }
                try? await Task.sleep(nanoseconds: 200_000_000)
            }
            let samples = probe.freshQualitySamples(within: 60, now: Date())
            log("preflight measured samples=\(samples.count)"
                + (samples.last.map { " rtt=\(Int($0 * 1000))ms" } ?? ""))
            guard !Task.isCancelled, gen == generation, !ownershipTransferred else { return .stopped }
            publish(connectedSnapshot(probe))

            // Hold the SAME measured candidate adoptable: renew the server
            // TTL in place instead of renegotiating a new peer connection
            // every TTL window. The renewal endpoint is optional (older
            // gateways 404): any failure rebuilds a fresh candidate.
            var renewals = 0
            while !Task.isCancelled, gen == generation, !ownershipTransferred {
                let now = Date()
                let holdUntil = min(now.addingTimeInterval(cadence.warmSeconds),
                                    probeExpiry ?? now.addingTimeInterval(cadence.warmSeconds))
                // Live idle publication: the echo samples arrive once per
                // second; republish the connected snapshot at the same cadence
                // so the measured RTT on the route screen keeps advancing
                // instead of aging from the last cycle checkpoint (build-36
                // field: a connected probe with 未测得/aged values).
                var lastPublishedLive = Date()
                while Date() < holdUntil {
                    if Task.isCancelled || gen != generation || ownershipTransferred { break }
                    if Date().timeIntervalSince(lastPublishedLive) >= 1.0 {
                        lastPublishedLive = Date()
                        publish(connectedSnapshot(probe))
                    }
                    try? await Task.sleep(nanoseconds: 250_000_000)
                }
                if Task.isCancelled || gen != generation { return .stopped }
                if ownershipTransferred { return .consumed }
                guard let id = preflightId, let expiry = probeExpiry else { return .needsRebuild }
                if Date() >= expiry {
                    log("preflight TTL lapsed before renewal id=\(id)")
                    return .needsRebuild
                }
                // Build 68: a HEALTHY candidate is renewed indefinitely (one
                // bounded resource) instead of being rebuilt at a chain cap.
                // "Healthy" means the SAME peer connection still delivers
                // fresh evidence (echo sample or fresh attach) AND the network
                // path did not change; anything else is rebuilt honestly.
                if let attachedPathGeneration,
                   attachedPathGeneration != currentPathGeneration() {
                    log("preflight network path changed id=\(id); rebuilding")
                    return .needsRebuild
                }
                let echoAge = probe.latestQualitySample.map {
                    Date().timeIntervalSince($0.at)
                } ?? Date().timeIntervalSince(attachedAt ?? Date())
                if echoAge > cadence.healthyEchoSeconds {
                    log("preflight evidence stale id=\(id) echoAgeMs=\(Int(echoAge * 1000))")
                    return .needsRebuild
                }
                if renewals >= cadence.maximumRenewalsPerChain { return .renewCapReached }
                do {
                    let renewed = try await api.renewMediaPreflight(preflightId: id)
                    guard gen == generation, !ownershipTransferred else { return .stopped }
                    probeExpiry = Self.expiry(ttlMs: renewed.ttlMs, margin: cadence.ttlSafetySeconds,
                                              now: Date())
                    renewals += 1
                    let echoAge = probe.latestQualitySample.map {
                        Int(Date().timeIntervalSince($0.at) * 1000)
                    } ?? -1
                    log("preflight renewed id=\(id) renewals=\(renewals) ttlMs=\(renewed.ttlMs)"
                        + " echoAgeMs=\(echoAge)")
                    publish(connectedSnapshot(probe))
                } catch {
                    guard !Task.isCancelled, gen == generation else { return .stopped }
                    log("preflight renew failed id=\(id) error=\(error.localizedDescription); rebuilding")
                    return .needsRebuild
                }
            }
            return ownershipTransferred ? .consumed : .stopped
        } catch {
            guard !Task.isCancelled, gen == generation else { return .stopped }
            log("preflight failed: \(error.localizedDescription)")
            publish(RoutePreflightSnapshot(phase: .unavailable(String(localized: "直连探测失败"))))
            try? await Task.sleep(nanoseconds: 2_000_000_000)
            return .unavailable
        }
    }

    /// Server TTL minus the client safety margin. A non-positive/absent TTL
    /// leaves the deadline nil (client freshness window only).
    static func expiry(ttlMs: Int64, margin: TimeInterval, now: Date) -> Date? {
        guard ttlMs > 0 else { return nil }
        let seconds = TimeInterval(ttlMs) / 1000 - margin
        guard seconds > 0 else { return nil }
        return now.addingTimeInterval(seconds)
    }

    private func connectedSnapshot(_ probe: DirectProbeControlling) -> RoutePreflightSnapshot {
        RoutePreflightSnapshot(
            phase: .connected,
            lastRTT: probe.latestQualitySample?.rtt ?? probe.samples.last,
            lastSampleAt: probe.latestQualitySample?.at
        )
    }

    private func waitConnected(_ probe: DirectProbeControlling, gen: UInt64) async -> Bool {
        let deadline = Date().addingTimeInterval(cadence.connectTimeout)
        while Date() < deadline {
            if Task.isCancelled || gen != generation { return false }
            if probe.connected, probe.mediaReady { return true }
            try? await Task.sleep(nanoseconds: 200_000_000)
        }
        return probe.connected && probe.mediaReady
    }

    private func cancelProbe(serverDiscard: Bool) {
        let probe = self.probe
        let preflightId = self.preflightId
        trickleExchange?.cancel()
        trickleExchange = nil
        self.probe = nil
        self.preflightId = nil
        attachedAt = nil
        probeExpiry = nil
        attachedPathGeneration = nil
        attachId = nil
        probe?.cancel()
        if serverDiscard, let preflightId {
            Task { try? await api.discardMediaPreflight(preflightId: preflightId) }
        }
    }
}
