import XCTest
@testable import CallRelay

// MARK: - Build-62 QUIC pin trust (protocol §6, closes the TRUST-VERIFICATION GAP)
//
// Every fixture is REAL Go serialization from `gateway/cmd/pinfixture`
// (synthetic identity, production `quic.BuildSignedPin` signing path), so a
// green suite proves the actual Go→Swift contract: JSON field names, the
// canonical signed bytes (`cert.go: pinCanonical`), UNIX-SECOND validity
// dates, and Ed25519 signature verification against the pairing-trusted
// identity — never against the pin's co-returned identity mirrors.

@MainActor
final class QUICPinTrustTests: XCTestCase {
    private let referenceNow = Date(timeIntervalSince1970: 1_800_000_000) // 2027-01

    // MARK: Helpers

    private func pin(from document: String = QUICPinFixture.validDocument)
        throws -> QUICMediaInfo.PinDocument
    {
        let info = try QUICPinFixture.decode(document)
        return try XCTUnwrap(info.pin)
    }

    private func verify(
        _ pin: QUICMediaInfo.PinDocument,
        live: IdentityResponse? = QUICPinFixture.identityResponse,
        gatewayId: String = QUICPinFixture.gatewayId,
        fingerprint: String = QUICPinFixture.fingerprint,
        now: Date? = nil
    ) -> Result<QUICPin, QUICPinTrustError> {
        QUICPinTrust.verifiedPin(
            pin,
            liveIdentity: live,
            trustedGatewayId: gatewayId,
            trustedFingerprint: fingerprint,
            now: now ?? referenceNow)
    }

    /// A copy of `pin` with selected fields replaced (documents are
    /// immutable values; tampering must never mutate the fixture).
    private func mutated(
        _ pin: QUICMediaInfo.PinDocument,
        version: Int? = nil,
        gatewayId: String? = nil,
        algorithm: String? = nil,
        spkiHash: String? = nil,
        notBefore: Int64? = nil,
        notAfter: Int64? = nil,
        signature: String? = nil
    ) -> QUICMediaInfo.PinDocument {
        QUICMediaInfo.PinDocument(
            version: version ?? pin.version,
            gatewayId: gatewayId ?? pin.gatewayId,
            algorithm: algorithm ?? pin.algorithm,
            spkiHash: spkiHash ?? pin.spkiHash,
            notBefore: notBefore ?? pin.notBefore,
            notAfter: notAfter ?? pin.notAfter,
            signature: signature ?? pin.signature,
            identityKey: pin.identityKey,
            identityFingerprint: pin.identityFingerprint)
    }

    // MARK: The happy path: real Go signature, production decode + verify

    func testGoSignedPinVerifiesThroughProductionDecoder() throws {
        // Decode through the production API pipeline (HTTP client shape ==
        // JSONDecoder of the same bytes).
        let info = try QUICPinFixture.decode(QUICPinFixture.validDocument)
        XCTAssertTrue(info.enabled)
        XCTAssertEqual(info.pinHash.count, 32)
        let result = verify(try XCTUnwrap(info.pin))
        guard case .success(let pin) = result else {
            XCTFail("a genuine Go-signed pin must verify: \(result)")
            return
        }
        XCTAssertEqual(pin.spkiSHA256, Data(base64Encoded: "4d79DGcwBi5HZh2+GEnT37GV+N4jg7+I1Jur3xcCFkI="))
        // UNIX SECONDS → Date (a millisecond reading would land in 1970).
        XCTAssertEqual(pin.notBefore, Date(timeIntervalSince1970: 1_735_689_600))
        XCTAssertEqual(pin.notAfter, Date(timeIntervalSince1970: 1_893_456_000))
    }

    /// Byte-level canary: the Swift canonical rendering must equal the
    /// exact bytes Go signs (domain + 6 newline-joined fields, no trailing
    /// newline). If the Go layout ever changes, this fails first.
    func testCanonicalBytesMatchGoPinCanonicalExactly() throws {
        let pin = try pin()
        let expected = """
            callrelay-quic-pin-v1
            1
            gateway-quicdirect-fixture
            ecdsa-p256-spki-sha256
            4d79DGcwBi5HZh2+GEnT37GV+N4jg7+I1Jur3xcCFkI=
            1735689600
            1893456000
            """
        XCTAssertEqual(String(data: QUICPinTrust.canonicalDocument(pin), encoding: .utf8), expected)
    }

    func testDerivedFingerprintMatchesGatewayAlgorithm() {
        // identity.go: "sha256:"+hex(SHA256(raw 32-byte key)).
        XCTAssertEqual(
            QUICPinTrust.fingerprint(publicKeyBase64: QUICPinFixture.publicKey),
            QUICPinFixture.fingerprint)
        // Wrong key material fails closed.
        XCTAssertNil(QUICPinTrust.fingerprint(publicKeyBase64: "AAAA"))
        XCTAssertNil(QUICPinTrust.fingerprint(publicKeyBase64: Data(count: 33).base64EncodedString()))
    }

    // MARK: Identity anchor negatives

    func testWrongGatewayRejected() throws {
        let result = verify(try pin(), gatewayId: "gateway-EVIL")
        XCTAssertEqual(result, .failure(.identityMismatched))
    }

    func testWrongLiveKeyRejectedByDerivedFingerprint() throws {
        // A DIFFERENT (attacker) key that is structurally valid Ed25519:
        // derivation makes the fingerprint mismatch visible instead of
        // trusting any served string.
        let other = IdentityResponse(
            gatewayId: QUICPinFixture.gatewayId,
            gatewayName: nil, mode: nil, transport: nil, apiVersion: nil,
            publicKey: Data(repeating: 0xAB, count: 32).base64EncodedString(),
            fingerprint: QUICPinFixture.fingerprint) // lying server string
        let result = verify(try pin(), live: other)
        XCTAssertEqual(result, .failure(.derivedFingerprintMismatched))
    }

    func testServedFingerprintStringAloneIsNeverTrusted() throws {
        // Server returns the CORRECT fingerprint string but a WRONG key and
        // no publicKey: there is nothing to derive from → mismatch, even
        // though the strings would have matched the old comparison.
        let lying = IdentityResponse(
            gatewayId: QUICPinFixture.gatewayId,
            gatewayName: nil, mode: nil, transport: nil, apiVersion: nil,
            publicKey: nil,
            fingerprint: QUICPinFixture.fingerprint)
        let result = verify(try pin(), live: lying)
        XCTAssertEqual(result, .failure(.derivedFingerprintMismatched))
    }

    func testWrongTrustedFingerprintRejected() throws {
        let result = verify(try pin(), fingerprint: "sha256:0000")
        XCTAssertEqual(result, .failure(.derivedFingerprintMismatched))
    }

    func testUnreachableIdentityRejected() throws {
        XCTAssertEqual(verify(try pin(), live: nil), .failure(.identityMismatched))
    }

    func testMissingTrustedIdentityFailsClosed() throws {
        XCTAssertEqual(verify(try pin(), gatewayId: ""), .failure(.untrustedIdentity))
        XCTAssertEqual(verify(try pin(), fingerprint: ""), .failure(.untrustedIdentity))
    }

    // MARK: Document negatives

    func testWrongVersionRejected() throws {
        let result = verify(mutated(try pin(), version: 2))
        XCTAssertEqual(result, .failure(.wrongVersion(2)))
    }

    func testWrongAlgorithmRejected() throws {
        let result = verify(mutated(try pin(), algorithm: "ed25519-spki-sha256"))
        XCTAssertEqual(result, .failure(.wrongAlgorithm("ed25519-spki-sha256")))
    }

    func testTamperedSPKIHashBreaksSignature() throws {
        // Flip one base64 char: still decodable to 32 bytes, but the signed
        // canonical text no longer matches.
        var hash = "4d79DGcwBi5HZh2+GEnT37GV+N4jg7+I1Jur3xcCFkI="
        hash = "5" + hash.dropFirst()
        let result = verify(mutated(try pin(), spkiHash: hash))
        XCTAssertEqual(result, .failure(.invalidSignature))
    }

    func testMalformedSPKIHashRejected() throws {
        let result = verify(mutated(try pin(), spkiHash: "not-base64!!"))
        XCTAssertEqual(result, .failure(.malformedSPKIHash))
    }

    func testMalformedSignatureRejected() throws {
        let result = verify(mutated(try pin(), signature: "not-base64!!"))
        XCTAssertEqual(result, .failure(.malformedSignature))
    }

    func testTruncatedSignatureRejected() throws {
        let result = verify(mutated(try pin(), signature: Data(count: 32).base64EncodedString()))
        XCTAssertEqual(result, .failure(.malformedSignature))
    }

    func testWrongPinDocumentRejected() throws {
        // A signature from ANOTHER genuine Go document (the expired one)
        // must not verify over the valid document's canonical text.
        let other = try pin(from: QUICPinFixture.expiredDocument)
        let result = verify(mutated(try pin(), signature: other.signature))
        XCTAssertEqual(result, .failure(.invalidSignature))
    }

    // MARK: Cross-language certificate validity (real Go values)

    func testExpiredCertificateRejected() throws {
        // The expired document is a REAL Go-signed expired cert; at any
        // "now" after its window the date gate rejects it.
        let result = verify(try pin(from: QUICPinFixture.expiredDocument))
        guard case .failure(.expired(let notAfter, _)) = result else {
            XCTFail("expected .expired, got \(result)")
            return
        }
        XCTAssertEqual(notAfter, Date(timeIntervalSince1970: 1_609_459_200))
    }

    func testExpiredDocumentSignatureIsGenuine() throws {
        // Sanity: the expired fixture is properly signed — inside its own
        // window it verifies. This separates "signature wrong" from "date
        // wrong" and proves the negative above is the date gate only.
        let result = verify(
            try pin(from: QUICPinFixture.expiredDocument),
            now: Date(timeIntervalSince1970: 1_600_000_000)) // 2020-09
        guard case .success = result else {
            XCTFail("the expired fixture must be a genuine Go signature: \(result)")
            return
        }
    }

    func testNotYetValidCertificateRejected() throws {
        let result = verify(try pin(from: QUICPinFixture.notYetValidDocument))
        guard case .failure(.notYetValid(let notBefore, _)) = result else {
            XCTFail("expected .notYetValid, got \(result)")
            return
        }
        XCTAssertEqual(notBefore, Date(timeIntervalSince1970: 4_070_908_800))
    }

    func testBoundaryDatesAreInclusive() throws {
        let pin = try pin()
        // Exactly at notBefore / notAfter the certificate is usable.
        func assertUsable(_ result: Result<QUICPin, QUICPinTrustError>, _ message: String) {
            guard case .success = result else {
                XCTFail("\(message): \(result)")
                return
            }
        }
        assertUsable(verify(pin, now: Date(timeIntervalSince1970: 1_735_689_600)), "at notBefore")
        assertUsable(verify(pin, now: Date(timeIntervalSince1970: 1_893_456_000)), "at notAfter")
        // One second outside fails.
        XCTAssertEqual(
            verify(pin, now: Date(timeIntervalSince1970: 1_735_689_599)),
            .failure(.notYetValid(notBefore: Date(timeIntervalSince1970: 1_735_689_600),
                                  now: Date(timeIntervalSince1970: 1_735_689_599))))
        XCTAssertEqual(
            verify(pin, now: Date(timeIntervalSince1970: 1_893_456_001)),
            .failure(.expired(notAfter: Date(timeIntervalSince1970: 1_893_456_000),
                              now: Date(timeIntervalSince1970: 1_893_456_001))))
    }
}
