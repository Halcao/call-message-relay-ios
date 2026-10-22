import Foundation

/// Build-68 incremental (trickle) media capability.
///
/// It is a SEPARATE protocol on purpose: `GatewayAPI` keeps its original
/// pre-68 surface byte for byte, so every existing conformer — an older
/// gateway client, a test fake, a demo client — still satisfies the base
/// protocol and keeps using the full-SDP handshake. Only a client that
/// adopts this capability (the live `HTTPGatewayAPI`) can use incremental
/// ICE, and callers must additionally check the gateway's advertised
/// `capabilities.iceTrickle`.
protocol TrickleMediaAPI: AnyObject {
    /// Incremental call-scoped probe attach: returns the answer as soon as the
    /// local description exists, plus the attach token/cursor for the
    /// candidate exchange.
    func attachMediaProbe(callId: String, sdp: String, trickle: Bool) async throws -> WebRTCAnswer
    /// Incremental device-scoped preflight attach.
    func attachMediaPreflight(sdp: String, trickle: Bool) async throws -> V2PreflightAnswer
    /// Posts the client's remaining candidates for the call's attachment.
    func postCallCandidates(callId: String, attachId: String, candidates: [String]) async throws
    func postProbeCandidates(callId: String, attachId: String, candidates: [String]) async throws
    func postPreflightCandidates(preflightId: String, attachId: String, candidates: [String]) async throws
    /// Polls the gateway's incrementally gathered candidates (`after` is the
    /// caller's cursor).
    func pollCallCandidates(callId: String, attachId: String, after: Int) async throws -> V2CandidateBatch
    func pollProbeCandidates(callId: String, attachId: String, after: Int) async throws -> V2CandidateBatch
    func pollPreflightCandidates(preflightId: String, attachId: String, after: Int) async throws -> V2CandidateBatch
}
