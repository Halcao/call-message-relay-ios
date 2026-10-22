import Foundation
import CryptoKit

/// Build-62 closes the documented TRUST-VERIFICATION GAP (protocol §6): the
/// QUIC pin document's Ed25519 signature is now verified on-device against
/// the gateway identity the app ALREADY trusts from pairing — never against
/// the `identityKey`/`identityFingerprint` mirrors co-returned inside the pin
/// document itself.
///
/// Trust chain, fail-closed at every step:
/// 1. The caller injects the pairing binding `(gatewayId, fingerprint)`
///    (captured at enrollment, persisted in `binding.json`).
/// 2. The live gateway identity is fetched from the ANONYMOUS `/identity`
///    handshake. The channel itself is not the anchor: the public key is
///    ACCEPTED only when its fingerprint — derived here with the gateway's
///    canonical algorithm (`identity.go: "sha256:"+hex(SHA-256(raw key))`) —
///    equals the binding fingerprint. A served fingerprint STRING is never
///    trusted on its own.
/// 3. The pin document's signature is verified over the exact canonical
///    newline-joined rendering (`cert.go: pinCanonical`), with version,
///    algorithm, gateway id, SPKI hash shape and the certificate validity
///    window all checked. The result is a `QUICPin` whose notBefore/notAfter
///    are UNIX SECONDS rendered as dates (the gateway emits
///    `cert.NotBefore/NotAfter.Unix()` — seconds, never milliseconds).
enum QUICPinTrustError: Error, Equatable, CustomStringConvertible {
    /// No pairing-trusted identity was injected into the call plane.
    case untrustedIdentity
    /// The `/identity` handshake failed or the live gateway id differs.
    case identityMismatched
    /// The live public key's derived fingerprint differs from the binding.
    case derivedFingerprintMismatched
    case wrongVersion(Int?)
    case wrongAlgorithm(String?)
    case wrongGateway(String?)
    case malformedSPKIHash
    case malformedSignature
    case invalidSignature
    case notYetValid(notBefore: Date, now: Date)
    case expired(notAfter: Date, now: Date)

    var description: String {
        switch self {
        case .untrustedIdentity:
            return "no pairing-trusted gateway identity available"
        case .identityMismatched:
            return "live identity unreachable or gateway id differs from binding"
        case .derivedFingerprintMismatched:
            return "live public key fingerprint does not match pairing binding"
        case .wrongVersion(let v):
            return "pin version \(v.map(String.init) ?? "nil") != 1"
        case .wrongAlgorithm(let a):
            return "pin algorithm \(a ?? "nil") unsupported"
        case .wrongGateway(let g):
            return "pin gateway \(g ?? "nil") differs from binding"
        case .malformedSPKIHash:
            return "pin spkiHash is not base64(SHA-256)"
        case .malformedSignature:
            return "pin signature is not base64(ed25519)"
        case .invalidSignature:
            return "pin signature invalid for trusted identity"
        case .notYetValid(let notBefore, let now):
            return "certificate window starts \(notBefore) (now \(now))"
        case .expired(let notAfter, let now):
            return "certificate window ended \(notAfter) (now \(now))"
        }
    }
}

enum QUICPinTrust {
    /// `cert.go: pinDomain` — signed-text domain separation.
    static let domain = "callrelay-quic-pin-v1"
    /// `cert.go` algorithm gate.
    static let algorithm = "ecdsa-p256-spki-sha256"

    /// The EXACT `pinCanonical` rendering: domain, version, gatewayId,
    /// algorithm, spkiHash, notBefore, notAfter joined by "\n" with no
    /// trailing newline. Byte-identical to the Go side by construction
    /// (a Go-signed fixture verifies only when this matches).
    static func canonicalDocument(_ pin: QUICMediaInfo.PinDocument) -> Data {
        let text = [
            domain,
            "\(pin.version ?? 0)",
            pin.gatewayId ?? "",
            pin.algorithm ?? "",
            pin.spkiHash ?? "",
            "\(pin.notBefore ?? 0)",
            "\(pin.notAfter ?? 0)",
        ].joined(separator: "\n")
        return Data(text.utf8)
    }

    /// The gateway's canonical identity fingerprint
    /// (`identity.go: Fingerprint`): "sha256:" + lowercase hex of the
    /// SHA-256 over the raw 32-byte Ed25519 public key.
    static func fingerprint(publicKeyBase64: String) -> String? {
        guard let raw = Data(base64Encoded: publicKeyBase64), raw.count == 32 else { return nil }
        return "sha256:" + SHA256.hash(data: raw).map { String(format: "%02x", $0) }.joined()
    }

    /// Fail-closed verification of a gateway pin document against the
    /// pairing-trusted identity. Returns the `QUICPin` for the TLS verify
    /// block on success; every failure is a distinct, loggable reason.
    static func verifiedPin(
        _ pin: QUICMediaInfo.PinDocument,
        liveIdentity: IdentityResponse?,
        trustedGatewayId: String,
        trustedFingerprint: String,
        now: Date
    ) -> Result<QUICPin, QUICPinTrustError> {
        guard !trustedGatewayId.isEmpty, !trustedFingerprint.isEmpty else {
            return .failure(.untrustedIdentity)
        }
        // (2) anchor the live key to the pairing fingerprint by DERIVATION.
        guard let live = liveIdentity, let liveId = live.gatewayId,
              liveId == trustedGatewayId else {
            return .failure(.identityMismatched)
        }
        guard let publicKey = live.publicKey,
              let derived = fingerprint(publicKeyBase64: publicKey),
              derived == trustedFingerprint else {
            return .failure(.derivedFingerprintMismatched)
        }
        // (3) the signed document itself.
        guard pin.version == 1 else { return .failure(.wrongVersion(pin.version)) }
        guard pin.algorithm == algorithm else { return .failure(.wrongAlgorithm(pin.algorithm)) }
        guard pin.gatewayId == trustedGatewayId else { return .failure(.wrongGateway(pin.gatewayId)) }
        guard let spkiHash = pin.spkiHash,
              let spki = Data(base64Encoded: spkiHash), spki.count == 32 else {
            return .failure(.malformedSPKIHash)
        }
        guard let signatureText = pin.signature,
              let signature = Data(base64Encoded: signatureText), signature.count == 64 else {
            return .failure(.malformedSignature)
        }
        guard let publicKeyData = Data(base64Encoded: publicKey), publicKeyData.count == 32,
              let trustedKey = try? Curve25519.Signing.PublicKey(rawRepresentation: publicKeyData) else {
            return .failure(.derivedFingerprintMismatched)
        }
        guard trustedKey.isValidSignature(signature, for: canonicalDocument(pin)) else {
            return .failure(.invalidSignature)
        }
        // UNIX seconds → Date (gateway emits cert.Unix() seconds exactly).
        let notBefore = pin.notBefore.map { Date(timeIntervalSince1970: TimeInterval($0)) }
        let notAfter = pin.notAfter.map { Date(timeIntervalSince1970: TimeInterval($0)) }
        if let notBefore, now < notBefore {
            return .failure(.notYetValid(notBefore: notBefore, now: now))
        }
        if let notAfter, now > notAfter {
            return .failure(.expired(notAfter: notAfter, now: now))
        }
        return .success(QUICPin(
            spkiSHA256: spki,
            notBefore: notBefore,
            notAfter: notAfter))
    }
}
