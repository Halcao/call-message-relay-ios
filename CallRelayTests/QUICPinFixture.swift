import Foundation
@testable import CallRelay

// MARK: - Build-62 QUIC pin fixtures (REAL Go serialization)
//
// These documents are the byte-exact output of the gateway's pinfixture
// generator (`gateway/cmd/pinfixture`), built through the PRODUCTION pin
// path: synthetic Ed25519 identity (fixed seed, NO production secret) +
// synthetic ECDSA P-256 certificate, signed by `quic.BuildSignedPin` ->
// `identity.Sign` over `pinCanonical`. Regenerate with:
//
//     go run ./cmd/pinfixture -out <dir>
//
// The iOS suite decodes these bytes through the production `QUICMediaInfo`
// decoder and verifies them through the production `QUICPinTrust` verifier,
// so a green test proves the actual Go→Swift wire/signature/date contract.
enum QUICPinFixture {
    /// The synthetic gateway identity (matches `trusted-identity.txt`).
    static let gatewayId = "gateway-quicdirect-fixture"
    static let publicKey = "/N/s0w9ofyMcW0u5pRGh1CcioKW2JNHNIiIPUpLi5+4="
    static let fingerprint = "sha256:9a4fcd1b78fa4237119e6778dc6b4c480f02169d2722c34c93b5ec77002612f7"

    /// A `/identity` response for the synthetic gateway (publicKey is the
    /// RAW key base64 — the verifier derives the fingerprint from it).
    static let identityResponse = IdentityResponse(
        gatewayId: gatewayId,
        gatewayName: "pinfixture",
        mode: "unified",
        transport: nil,
        apiVersion: "v2",
        publicKey: publicKey,
        fingerprint: nil)

    /// Valid window: 2025-01-01 .. 2030-01-01 (unix seconds 1735689600..
    /// 1893456000) — seconds, NOT milliseconds.
    static let validDocument =
        #"{"enabled":true,"alpn":"callrelay-media","pin":{"version":1,"gatewayId":"gateway-quicdirect-fixture","algorithm":"ecdsa-p256-spki-sha256","spkiHash":"4d79DGcwBi5HZh2+GEnT37GV+N4jg7+I1Jur3xcCFkI=","notBefore":1735689600,"notAfter":1893456000,"signature":"T5P7yUd/qtfBUnj6ty/QxaxLJim9/pMatb0ncUNt9HG8QANFwfFSnTbb22rj+/QRsdpElBANyzhoaYIEQWePAg==","identityKey":"/N/s0w9ofyMcW0u5pRGh1CcioKW2JNHNIiIPUpLi5+4=","identityFingerprint":"sha256:9a4fcd1b78fa4237119e6778dc6b4c480f02169d2722c34c93b5ec77002612f7"},"candidates":[{"type":"srflx","proto":"quic","addr":"192.0.2.10:8792","expiresAtMs":1893456000000}],"stun":["192.0.2.53:3478"]}"#

    /// Expired window: 2020-01-01 .. 2021-01-01 (real Go-signed expired doc).
    static let expiredDocument =
        #"{"enabled":true,"alpn":"callrelay-media","pin":{"version":1,"gatewayId":"gateway-quicdirect-fixture","algorithm":"ecdsa-p256-spki-sha256","spkiHash":"4d79DGcwBi5HZh2+GEnT37GV+N4jg7+I1Jur3xcCFkI=","notBefore":1577836800,"notAfter":1609459200,"signature":"v8c+3CXshm0xLxcKozh6Pta30urvtAugEf72vzyrqUMrLTWBiycgSIv/MZWV6m/+RrAC9YfomL1M1m7+1WtLAg==","identityKey":"/N/s0w9ofyMcW0u5pRGh1CcioKW2JNHNIiIPUpLi5+4=","identityFingerprint":"sha256:9a4fcd1b78fa4237119e6778dc6b4c480f02169d2722c34c93b5ec77002612f7"},"candidates":[{"type":"srflx","proto":"quic","addr":"192.0.2.10:8792","expiresAtMs":1893456000000}],"stun":["192.0.2.53:3478"]}"#

    /// Not-yet-valid window: 2099-01-01 .. 2100-01-01 (real Go-signed).
    static let notYetValidDocument =
        #"{"enabled":true,"alpn":"callrelay-media","pin":{"version":1,"gatewayId":"gateway-quicdirect-fixture","algorithm":"ecdsa-p256-spki-sha256","spkiHash":"4d79DGcwBi5HZh2+GEnT37GV+N4jg7+I1Jur3xcCFkI=","notBefore":4070908800,"notAfter":4102444800,"signature":"5UtXolW8W2Xq/W6Zje7WkYHK+C/USLzTZ45aJp6iRmnPOi5oge/YyT4GFMfNh0QjMia71FLFH4Nr6laL9OvHCw==","identityKey":"/N/s0w9ofyMcW0u5pRGh1CcioKW2JNHNIiIPUpLi5+4=","identityFingerprint":"sha256:9a4fcd1b78fa4237119e6778dc6b4c480f02169d2722c34c93b5ec77002612f7"},"candidates":[{"type":"srflx","proto":"quic","addr":"192.0.2.10:8792","expiresAtMs":1893456000000}],"stun":["192.0.2.53:3478"]}"#

    static func decode(_ document: String) throws -> QUICMediaInfo {
        try JSONDecoder().decode(QUICMediaInfo.self, from: Data(document.utf8))
    }
}
