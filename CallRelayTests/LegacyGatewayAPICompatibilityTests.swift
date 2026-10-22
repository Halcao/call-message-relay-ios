import XCTest
import AVFoundation
@testable import CallRelay

/// Build-68 compatibility contract: the incremental (trickle) media API is an
/// OPTIONAL capability, NOT part of `GatewayAPI`. A client that predates it —
/// an older gateway client, a fake, a demo transport — must keep working
/// through the ORIGINAL full-SDP entry points; nothing about the trickle work
/// may turn an old conformer into an error.
///
/// These tests pin both halves:
/// * an OLD conformer (no `TrickleMediaAPI`) still attaches and renews
///   normally even when the gateway advertises `iceTrickle`;
/// * a trickle-capable client is only used when BOTH the gateway advertises
///   the capability and the client supports it.
@MainActor
final class LegacyGatewayAPICompatibilityTests: XCTestCase {

    /// The pre-68 conformer used here is the plain `FakeGatewayAPI`: it
    /// implements `GatewayAPI` ONLY. If the incremental work had leaked into
    /// the base protocol, this type would not compile at all — and if the call
    /// sites required the capability, it would stop working.

    /// A probe that WOULD be trickle-capable, so the only thing standing
    /// between the controller and the incremental path is the client.
    @MainActor
    private final class TrickleCapableProbe: DirectProbeControlling, TrickleProbeCapable {
        let base = FakeDirectProbe()
        var onState: ((MediaState) -> Void)? {
            get { base.onState } set { base.onState = newValue }
        }
        var onMediaReady: (() -> Void)? {
            get { base.onMediaReady } set { base.onMediaReady = newValue }
        }
        var connected: Bool { base.connected }
        var mediaReady: Bool { base.mediaReady }
        var samples: [TimeInterval] { base.samples }
        var audioFlowing: Bool { base.audioFlowing }
        var playoutStatUnavailable: Bool { base.playoutStatUnavailable }
        var inboundAudioPackets: UInt64 { base.inboundAudioPackets }
        var outboundAudioPackets: UInt64 { base.outboundAudioPackets }
        var inboundPlayoutSamples: UInt64 { base.inboundPlayoutSamples }
        var playoutSamplesStatSeen: Bool { base.playoutSamplesStatSeen }
        var outboundAudioEnergySinceAdoption: Double { base.outboundAudioEnergySinceAdoption }
        var outboundAudioEnergyReported: Bool { base.outboundAudioEnergyReported }
        var latestQualitySample: (rtt: TimeInterval, at: Date)? { base.latestQualitySample }
        var echoStallCount: Int { base.echoStallCount }
        func restartAudioDevice() -> Bool { base.restartAudioDevice() }
        func freshQualitySamples(within window: TimeInterval, now: Date) -> [TimeInterval] {
            base.freshQualitySamples(within: window, now: now)
        }
        func freshTimestampedQualitySamples(within window: TimeInterval, now: Date)
            -> [(rtt: TimeInterval, at: Date)] {
            base.freshTimestampedQualitySamples(within: window, now: now)
        }
        func makeOffer(ice: ICEConfiguration) async throws -> String { try await base.makeOffer(ice: ice) }
        func applyAnswer(_ sdp: String) async throws { try await base.applyAnswer(sdp) }
        func adopt(activatedSession session: AVAudioSession?) { base.adopt(activatedSession: session) }
        func audioSessionActivated(_ session: AVAudioSession) { base.audioSessionActivated(session) }
        func audioSessionDeactivated(_ session: AVAudioSession) { base.audioSessionDeactivated(session) }
        func activateAudioWithoutCallKit() -> Bool { base.activateAudioWithoutCallKit() }
        func suspendAudioDeviceForHandover() { base.suspendAudioDeviceForHandover() }
        func resumeAudioDeviceAfterFailedHandover() { base.resumeAudioDeviceAfterFailedHandover() }
        func setSpeakerphone(_ enabled: Bool) throws { try base.setSpeakerphone(enabled) }
        func setMuted(_ muted: Bool) { base.setMuted(muted) }
        func setRecoveryFreshMedia(_ callback: (() -> Void)?) { base.setRecoveryFreshMedia(callback) }
        var isAdoptedPeer: Bool { base.isAdoptedPeer }
        func cancel() { base.cancel() }
        func closeTransport() { base.closeTransport() }

        // Incremental capability (never used with the legacy client).
        var onLocalCandidate: ((String) -> Void)?
        var onGatheringComplete: (() -> Void)?
        var locallyGatheredCandidates: [String] = []
        var gatheringFinished: Bool { true }
        var trickleOfferCount = 0
        func makeOfferTrickle(ice: ICEConfiguration) async throws -> TrickleOffer {
            trickleOfferCount += 1
            return TrickleOffer(sdp: try await base.makeOffer(ice: ice), gatheredBefore: 0)
        }
        func addRemoteCandidate(_ sdp: String) {}
    }

    /// An OLD client must keep working end-to-end even when the gateway
    /// advertises `iceTrickle`: the controller falls back to the original
    /// methods, which is exactly the pre-68 behavior.
    func testLegacyClientUsesFullSDPEvenWhenGatewayAdvertisesTrickle() async throws {
        let client = FakeGatewayAPI()
        client.preflightIce = ICEConfiguration(
            policy: "all", iceServers: [],
            expiresAt: RFC3339Date.formatter.string(from: Date().addingTimeInterval(3600)),
            mediaTransports: ["ice", "ws"],
            mediaStandby: true,
            mediaTrickle: true,
            mediaQuic: false)
        var probes: [TrickleCapableProbe] = []
        var logs: [String] = []
        let controller = RoutePreflightController(
            api: client,
            cadence: RoutePreflightController.Cadence(
                warmSeconds: 0.1, cooldownSeconds: 0.5, connectTimeout: 0.3,
                minimumSamples: 1, measureSeconds: 0.05, handoffFreshSeconds: 40,
                ttlSafetySeconds: 0.05, maximumRenewalsPerChain: 1,
                renewPauseSeconds: 0.01, maximumConsecutiveRebuilds: 1),
            eligible: { true },
            probeFactory: {
                let probe = TrickleCapableProbe()
                probes.append(probe)
                return probe
            },
            log: { logs.append($0) })

        controller.appDidEnterForeground()
        let deadline = Date().addingTimeInterval(3)
        while Date() < deadline, controller.freshHandoff == nil {
            try? await Task.sleep(nanoseconds: 20_000_000)
        }
        XCTAssertNotNil(controller.freshHandoff,
                        "the legacy client path must still produce a usable candidate; logs: \(logs)")
        XCTAssertEqual(client.preflightAttachCount, 1,
                       "the ORIGINAL attachMediaPreflight(sdp:) must run")
        XCTAssertEqual(client.preflightTrickleAttaches, 0,
                       "an old client must never be sent a trickle attach")
        XCTAssertEqual(probes.first?.trickleOfferCount, 0,
                       "a legacy client must not be sent down the incremental path")
        XCTAssertFalse(logs.contains { $0.contains("当前配对不支持") },
                       "a legacy client must never surface a capability error")
    }

    /// The mirror contract: a trickle-capable client IS used when the gateway
    /// advertises the capability (the capability protocol is what gates it).
    func testTrickleCapableClientUsesIncrementalPath() async throws {
        let client = FakeTrickleGatewayAPI()
        client.preflightIce = ICEConfiguration(
            policy: "all", iceServers: [],
            expiresAt: RFC3339Date.formatter.string(from: Date().addingTimeInterval(3600)),
            mediaTransports: ["ice", "ws"],
            mediaStandby: true,
            mediaTrickle: true,
            mediaQuic: false)
        var probes: [TrickleCapableProbe] = []
        let controller = RoutePreflightController(
            api: client,
            cadence: RoutePreflightController.Cadence(
                warmSeconds: 0.05, cooldownSeconds: 0.2, connectTimeout: 0.3,
                minimumSamples: 1, measureSeconds: 0.02, handoffFreshSeconds: 40,
                ttlSafetySeconds: 0.05, maximumRenewalsPerChain: 1,
                renewPauseSeconds: 0.01, maximumConsecutiveRebuilds: 1),
            eligible: { true },
            probeFactory: {
                let probe = TrickleCapableProbe()
                probes.append(probe)
                return probe
            },
            log: { _ in })
        controller.appDidEnterForeground()
        let deadline = Date().addingTimeInterval(3)
        while Date() < deadline, controller.freshHandoff == nil {
            try? await Task.sleep(nanoseconds: 20_000_000)
        }
        XCTAssertNotNil(controller.freshHandoff)
        XCTAssertEqual(probes.first?.trickleOfferCount, 1,
                       "the incremental offer must be used when both sides support it")
        XCTAssertEqual(client.preflightTrickleAttaches, 1,
                       "the attach must carry the trickle flag")
        XCTAssertGreaterThanOrEqual(client.preflightIceFetchCount, 1)
    }
}
