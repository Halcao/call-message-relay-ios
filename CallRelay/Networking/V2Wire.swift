import Foundation

extension GatewayAPI {
    /// The concrete live HTTP transport, when this API is not demo/v1.
    ///
    /// The v2 line-scoped surface in `APITypes.swift` is declared only in a
    /// protocol extension, so Swift dispatches it statically: calling
    /// `authorizedLines()`/`dial(to:lineId:)`/conference helpers through
    /// `any GatewayAPI` hits the throwing defaults, not `HTTPGatewayAPI`.
    /// Callers holding an existential must reach the implementation through
    /// this cast, e.g. `api.unifiedHTTP?.authorizedLines()`.
    var unifiedHTTP: HTTPGatewayAPI? { self as? HTTPGatewayAPI }

    /// The concrete v2 transport, or a ready error for demo/v1 callers.
    func unifiedRequire() throws -> HTTPGatewayAPI {
        guard let http = unifiedHTTP else {
            throw APIError.notReady("当前配对不是统一网关。")
        }
        return http
    }
}

// MARK: - Unified gateway (apiVersion v2) request bodies
//
// Only what is not already in APITypes.swift. v2 bodies are line-scoped and
// never carry the v1 `transport` field.

struct V2DialRequest: Encodable, Equatable {
    let lineId: String
    let to: String
    let clientCallId: String
}

struct V2SendMessageRequest: Encodable, Equatable {
    let lineId: String
    let to: String
    let body: String
}

struct V2WebRTCOfferRequest: Encodable, Equatable {
    let sdp: String
    let type: String
    /// Build 68 incremental ICE. Sent only when the gateway advertised
    /// `capabilities.iceTrickle` AND the local offer really was produced
    /// incrementally; absent/false keeps the exact old full-SDP behavior
    /// (an old gateway ignores the field, an old client never sends it).
    var trickle: Bool? = nil
}

/// Build 68: one batch of ICE candidates travelling in either direction on an
/// attach-fenced incremental attachment. `attachId` pins the exact
/// negotiation; a batch for a replaced attachment is refused as stale.
struct V2CandidateBatchRequest: Encodable, Equatable {
    let attachId: String
    let candidates: [String]
    var preflightId: String? = nil
}

/// `POST .../candidates` answer: how many candidates were actually applied.
struct V2CandidateAccepted: Decodable, Equatable {
    let added: Int
}

/// `GET .../candidates` answer: the newly gathered gateway candidates, the
/// caller's next cursor and whether gathering is finished. `truncated` is set
/// only if the bounded gateway queue overflowed (never expected with real
/// candidates).
struct V2CandidateBatch: Decodable, Equatable {
    let candidates: [String]
    let cursor: Int
    let complete: Bool
    var truncated: Bool? = nil
}

/// Device-scoped (call-independent) preflight probe answer: a normal SDP
/// answer plus the id a later call commit uses to adopt this exact peer
/// connection, and the server TTL bounding the probe's life.
struct V2PreflightAnswer: Decodable, Equatable {
    let sdp: String
    let type: String
    let iceMode: String
    let preflightId: String
    let ttlMs: Int64
    /// Build 68 incremental attach: present only when this attach was
    /// negotiated as trickle. `candidateCursor` is how many gateway
    /// candidates already travelled inside `sdp`; `gatheringComplete` reports
    /// whether the gateway had finished gathering when the answer was
    /// written (false is normal and expected for trickle).
    var attachId: String? = nil
    var candidateCursor: Int? = nil
    var gatheringComplete: Bool? = nil
}

/// Optional commit body: adopt a device-scoped preflight candidate instead
/// of the call's own probe. `standby` (build 50, only when the gateway
/// advertised `capabilities.mediaStandby`) installs the candidate into the
/// hot-standby slot instead of replacing the primary.
struct V2CommitProbeRequest: Encodable, Equatable {
    let preflightId: String?
    var standby: Bool? = nil
}

/// `/api/v2/calls/{id}/media/standby/promote` body (build 50): the fenced
/// hot-standby role swap. The token pins the exact standby instance, the
/// expected epoch the client's view of the primary role, the nonce gives
/// idempotent retries; the ack carries the new epoch plus `currentEpoch` so
/// a replay after an intervening swap is visible and reconcilable.
struct V2PromoteStandbyRequest: Encodable, Equatable {
    let transport: String
    let token: String
    let expectEpoch: UInt64
    let nonce: String
}

struct V2PromoteStandbyResponse: Decodable, Equatable {
    let epoch: UInt64
    let currentEpoch: UInt64?
    let primary: String
    let standby: String?
    /// Token issued to the transport DEMOTED into the standby slot by this
    /// swap (needed to promote it back later).
    let standbyToken: String?
}

/// `/api/v2/calls/{id}/media/state` response (build 50; build 63 typed
/// QUIC roles): transport roles for post-ack-loss reconciliation.
struct V2MediaStateResponse: Decodable, Equatable {
    /// Exact primary transport kind: "ws" (real WSS), "ice" (WebRTC
    /// direct) or "quic" (QUIC DATAGRAM direct, build 63 dedicated slot).
    let primary: String
    let epoch: UInt64
    /// Real-WSS standby presence (build 63: never QUIC — the ambiguity the
    /// coordinator review flagged is gone on new gateways).
    let standbyWs: Bool?
    let standbyIce: Bool?
    /// Dedicated QUIC standby (build 63). Absent on pre-63 gateways ⇒ the
    /// client falls back to the documented legacy ws-family interpretation
    /// (an old gateway may hold the QUIC adapter in the ws standby slot).
    let standbyQuic: Bool?
    /// CURRENT standby identity tokens (coordinator review): an ack-loss
    /// reconcile recovers the demoted token from HERE, never pairing an
    /// old-epoch token with a new epoch.
    let standbyWsToken: String?
    let standbyIceToken: String?
    /// Build 63: the QUIC standby's own identity token.
    let standbyQuicToken: String?

    init(primary: String, epoch: UInt64,
         standbyWs: Bool? = nil, standbyIce: Bool? = nil,
         standbyWsToken: String? = nil, standbyIceToken: String? = nil,
         standbyQuic: Bool? = nil, standbyQuicToken: String? = nil) {
        self.primary = primary
        self.epoch = epoch
        self.standbyWs = standbyWs
        self.standbyIce = standbyIce
        self.standbyWsToken = standbyWsToken
        self.standbyIceToken = standbyIceToken
        self.standbyQuic = standbyQuic
        self.standbyQuicToken = standbyQuicToken
    }
}

/// Fencing values issued when a standby transport is installed (probe-commit
/// ack or the standby ready greeting's epoch/token pair).
struct V2StandbyFence: Equatable {
    let epoch: UInt64?
    let token: String?
}

struct V2StandbyCommitResponse: Decodable, Equatable {
    let committed: String?
    let epoch: UInt64?
    let token: String?
}

/// `/api/v2/media/probe/renew` body: extend the SAME registered candidate's
/// server TTL in place (no SDP, no renegotiation).
struct V2PreflightRenewRequest: Encodable, Equatable {
    let preflightId: String
}

/// `/api/v2/media/probe/renew` response: the refreshed server TTL for the
/// unchanged preflight id. `ttlMs` counts from the renewal instant.
struct V2PreflightRenewAnswer: Decodable, Equatable {
    let preflightId: String
    let ttlMs: Int64
}

struct V2DevicePreferencesRequest: Encodable, Equatable {
    let defaultLineId: String
}

/// Build-48 PATCH-presence preferences body: every field is optional; an
/// absent key is omitted so an audio-only patch never clears defaultLineId
/// and a line-only legacy patch never changes the denoise switches.
struct V2DenoisePreferencesRequest: Encodable, Equatable {
    var uplinkDenoise: Bool?
    var downlinkDenoise: Bool?
}

/// Build-55 PATCH-presence preferences body for the per-DEVICE auto-recording
/// preference (default OFF). Only `autoRecord` is sent, so this update never
/// touches defaultLineId or the denoise switches.
struct V2AutoRecordPreferencesRequest: Encodable, Equatable {
    var autoRecord: Bool?
}

struct V2LineNumberRequest: Encodable, Equatable {
    /// Non-empty sets a manual own number; empty resets the line to the
    /// SIM-read number.
    let phoneNumber: String
}

struct V2ConferenceCreateRequest: Encodable, Equatable {
    let callIds: [String]
}

struct V2ConferenceSplitRequest: Encodable, Equatable {
    let callId: String
}

/// `/api/v2/auth/refresh` response; unlike v1 it echoes the bound deviceId.
struct V2RefreshResponse: Decodable, Equatable {
    let accessToken: String
    let refreshToken: String
    let deviceId: String?
}

/// `/api/v2/calls/{id}/ice`. TURN entries may omit the short-lived
/// username/credential on host-only candidates, so those stay optional here
/// and are normalized when mapped onto the shared `ICEConfiguration`.
struct V2ICEServer: Decodable, Equatable {
    let urls: [String]
    let username: String?
    let credential: String?
}

struct V2ICEConfiguration: Decodable, Equatable {
    let policy: String
    let iceServers: [V2ICEServer]
    /// Advertised client audio transports ("ice", "ws"); absent on older
    /// gateways, in which case clients keep using WebRTC/ICE only.
    let mediaTransports: [String]?
    /// Build-50 capability map; `mediaStandby == true` gates the hot-standby
    /// attach/promote protocol. Absent on older gateways.
    let capabilities: V2MediaCapabilities?

    struct V2MediaCapabilities: Decodable, Equatable {
        let audioDenoise: [String]?
        let mediaStandby: Bool?
        /// Build-61: the gateway serves the QUIC DATAGRAM direct audio
        /// endpoint (protocol `docs/quic61/protocol.md`). Absent/false on
        /// older gateways and while the QUIC listener is down (the gateway
        /// degrades to "capability absent" instead of advertising a dead
        /// listener).
        let mediaQuic: Bool?
        /// Build 68: the gateway accepts an incremental (trickle) client
        /// offer and serves its own gathered candidates incrementally on the
        /// same authenticated JSON plane.
        let iceTrickle: Bool?
    }
}

// MARK: - QUIC direct media (build 61)

/// `GET /api/v2/media/quic`: the QUIC capability + TLS pin document. The pin
/// (SPKI-SHA256 plus the gateway identity key's Ed25519 signature over the
/// canonical newline-joined document, gateway `internal/unified/quic/cert.go:
/// pinCanonical`) is what the app verifies against the pairing identity it
/// already trusts, then pins into the QUIC TLS verify block. Never
/// trust-all: a pin without a matching signature is a hard failure BEFORE
/// any QUIC dial.
struct QUICMediaInfo: Decodable, Equatable, Sendable {
    let enabled: Bool
    let alpn: String?
    /// The full pin document as served by the gateway. Build-62 wire fix:
    /// the real shape is version/gatewayId/algorithm/spkiHash/notBefore/
    /// notAfter/signature/identityKey/identityFingerprint (the pre-62
    /// decoder read a fictional `alg/hash` shape, so `pinHash` was always
    /// empty against a real gateway and the attempt silently stood down).
    let pin: PinDocument?
    /// Convenience accessors derived from `pin` (fail-closed empty/nil).
    var pinHash: Data {
        Data(base64Encoded: pin?.spkiHash ?? "") ?? Data()
    }
    var pinSignature: Data {
        Data(base64Encoded: pin?.signature ?? "") ?? Data()
    }
    var identityFingerprint: String? { pin?.identityFingerprint }
    /// Certificate window, UNIX SECONDS exactly as the gateway's
    /// `cert.NotBefore/NotAfter.Unix()` emits them (build-62 fix: seconds,
    /// never milliseconds).
    var notBefore: Int64? { pin?.notBefore }
    var notAfter: Int64? { pin?.notAfter }
    let candidates: [QUICCandidate]
    /// STUN servers usable for the phone's own srflx discovery (same
    /// services the ICE configuration lists; the gateway repeats them here
    /// so the QUIC attempt is self-contained).
    let stun: [String]?

    /// The gateway-signed pin document (`cert.go: PinDocument`). Field names
    /// match the Go JSON tags exactly; the Ed25519 signature covers the
    /// canonical newline-joined rendering of these fields.
    struct PinDocument: Decodable, Equatable, Sendable {
        let version: Int?
        let gatewayId: String?
        let algorithm: String?
        let spkiHash: String?
        /// UNIX seconds (`cert.Unix()`), both bounds.
        let notBefore: Int64?
        let notAfter: Int64?
        let signature: String?
        /// Informational mirrors of the published identity. NEVER a trust
        /// anchor: the trusted key/fingerprint comes from the pairing
        /// binding + the derived `/identity` check.
        let identityKey: String?
        let identityFingerprint: String?
    }

    enum CodingKeys: String, CodingKey {
        case enabled, alpn, pin, candidates, stun
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        enabled = (try? c.decodeIfPresent(Bool.self, forKey: .enabled)) ?? false
        alpn = try? c.decodeIfPresent(String.self, forKey: .alpn)
        // Tolerant decode: a malformed pin document decodes as nil (the
        // caller then refuses the capability) instead of failing the whole
        // document and hiding the `enabled=false` truth.
        pin = try? c.decodeIfPresent(PinDocument.self, forKey: .pin)
        candidates = (try? c.decodeIfPresent([QUICCandidate].self, forKey: .candidates)) ?? []
        stun = try? c.decodeIfPresent([String].self, forKey: .stun)
    }
}

/// One advertised QUIC candidate (currently only "srflx", the gateway's
/// STUN-refresh server-reflexive address shared with the QUIC listener).
struct QUICCandidate: Decodable, Equatable, Sendable {
    let type: String
    /// "ip:port" (IPv6 bracketed).
    let addr: String
    /// Server-side TTL; a stale candidate is never dialed.
    let expiresAtMs: Int64?

    var hostPort: HostPort? { HostPort(wireString: addr) }
}

/// `POST /api/v2/calls/{id}/quic/candidate` response: the coordinated punch
/// window. `punchAtMs` is the shared start time; `deadlineMs` bounds the
/// attempt (~8 s); the one-time `punchToken` authenticates the QUIC attach.
struct QUICPunchInfo: Decodable, Equatable, Sendable {
    let punchToken: String
    let gatewayCandidate: String
    let punchAtMs: Int64
    let deadlineMs: Int64

    var gatewayHostPort: HostPort? { HostPort(wireString: gatewayCandidate) }
}

/// `POST /api/v2/calls/{id}/quic/candidate` body: the phone's own srflx
/// candidate plus the client network-epoch string for punch coordination.
struct V2QUICCandidateRequest: Encodable, Equatable, Sendable {
    let candidate: String
    let networkEpoch: String
}

/// `/api/v2` call view: the unified gateway writes `lineId` (lowercase d)
/// while the shared `CallRecord` was pinned to v1's `lineID`. Decode the v2
/// shape here, inside a file this change owns, and map it onto `CallRecord`
/// without altering the shared wire model used by events and v1.
struct V2CallView: Decodable {
    let id: String
    let gatewayId: String?
    let lineId: String?
    let lineName: String?
    let direction: String?
    let peer: String?
    let state: String?
    let startedAt: Int64?
    let connectedAt: Int64?
    let endedAt: Int64?
    let endReason: String?
    let recordingId: String?
    let recordingState: String?
    let recordingDurationMs: Int64?
    /// Truthful transport reporting: "ice" (direct) or "ws" (relay); absent
    /// before any client media attaches, or on older gateways.
    let mediaTransport: String?

    enum CodingKeys: String, CodingKey {
        case id, gatewayId, lineId, lineName, direction, peer, state
        case startedAt, connectedAt, endedAt, endReason
        case recordingId, recordingState, recordingDurationMs, mediaTransport
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = (try? c.decodeIfPresent(String.self, forKey: .id)) ?? ""
        gatewayId = try? c.decodeIfPresent(String.self, forKey: .gatewayId)
        lineId = try? c.decodeIfPresent(String.self, forKey: .lineId)
        lineName = try? c.decodeIfPresent(String.self, forKey: .lineName)
        direction = try? c.decodeIfPresent(String.self, forKey: .direction)
        peer = try? c.decodeIfPresent(String.self, forKey: .peer)
        state = try? c.decodeIfPresent(String.self, forKey: .state)
        startedAt = try? c.decodeIfPresent(Int64.self, forKey: .startedAt)
        connectedAt = try? c.decodeIfPresent(Int64.self, forKey: .connectedAt)
        endedAt = try? c.decodeIfPresent(Int64.self, forKey: .endedAt)
        endReason = try? c.decodeIfPresent(String.self, forKey: .endReason)
        recordingId = try? c.decodeIfPresent(String.self, forKey: .recordingId)
        recordingState = try? c.decodeIfPresent(String.self, forKey: .recordingState)
        recordingDurationMs = try? c.decodeIfPresent(Int64.self, forKey: .recordingDurationMs)
        mediaTransport = try? c.decodeIfPresent(String.self, forKey: .mediaTransport)
    }

    var callRecord: CallRecord {
        CallRecord(
            id: id,
            gatewayID: gatewayId,
            lineID: lineId,
            direction: direction.flatMap(CallDirection.init(rawValue:)) ?? .outbound,
            peer: peer,
            state: CallState.serverState(state),
            startedAt: startedAt ?? 0,
            connectedAt: connectedAt,
            endedAt: endedAt,
            endReason: endReason,
            recordingId: recordingId,
            recordingState: recordingState,
            recordingDurationMs: recordingDurationMs,
            mediaTransport: mediaTransport
        )
    }
}

// MARK: - Contacts sync (principal-scoped) and service-alert status

/// One labeled value in an uploaded contact.
struct ContactSyncField: Codable, Equatable {
    let label: String
    let value: String
}

/// One mobile→gateway contact upload. `clientRef` is the device's stable
/// system-contacts identifier: the gateway uses it to absorb repeated imports
/// without duplicating and to refuse resurrecting a contact the owner deleted
/// elsewhere.
struct ContactSyncEntry: Codable, Equatable {
    let clientRef: String
    let displayName: String
    let givenName: String
    let familyName: String
    let organization: String
    let nickname: String
    /// Pinyin/T9 search signature computed on-device (the server never
    /// transliterates CJK); empty for non-CJK names.
    let searchKey: String
    let phones: [ContactSyncField]
    let emails: [ContactSyncField]
    let urls: [ContactSyncField]
}

struct ContactSyncRequest: Encodable, Equatable {
    let contacts: [ContactSyncEntry]
}

struct ContactSyncResult: Decodable, Equatable {
    let revision: Int64
    let created: Int
    let updated: Int
    let unchanged: Int
    let skipped: Int
    let total: Int
}

/// One open service-alert incident on a line (arrears or network).
struct GatewayNotificationIncident: Decodable, Equatable {
    let lineId: String
    let cause: String
    let openedAt: String?
    let faultNotified: Bool?
    let arrearsNotified: Bool?
    let recoveryNotified: Bool?
}

/// `/api/v2/notifications/status`: the designated-line availability view the
/// Settings screen renders. Line name/tail are blanked server-side for keys
/// without access to that line.
struct GatewayNotificationStatus: Decodable, Equatable {
    let enabled: Bool
    let availability: String
    let lineId: String?
    let lineName: String?
    let lineTail: String?
    let lastProbeAt: Int64?
    let lastProbeResult: String?
    let agentState: String?
    let incidents: [GatewayNotificationIncident]?
}
