import XCTest
import CryptoKit
@testable import CallRelay

/// RFC 8489 wire-format tests: the pure parse/build half of the STUN
/// binding client the QUIC hole-punch path uses to discover the phone's own
/// server-reflexive candidate.
final class QUICSTUNTests: XCTestCase {
    /// Fixed transaction id shared by the crafted messages below.
    private let transactionID: [UInt8] = [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12]

    /// Header(20) + XOR-MAPPED-ADDRESS(8) for 192.0.2.1:5000.
    /// Port 5000 (0x1388) XOR 0x2112 = 0x329A; address 192.0.2.1 (0xC0000201)
    /// XOR 0x2112A442 = 0xE112A643.
    private func ipv4ResponseBytes() -> [UInt8] {
        var bytes: [UInt8] = [
            0x01, 0x01,       // Binding Success Response
            0x00, 0x0C,       // length: 12 attribute bytes
            0x21, 0x12, 0xA4, 0x42, // magic cookie
        ]
        bytes.append(contentsOf: transactionID)
        bytes.append(contentsOf: [
            0x00, 0x20,       // XOR-MAPPED-ADDRESS
            0x00, 0x08,       // value length: 8
            0x00,             // reserved
            0x01,             // family: IPv4
            0x32, 0x9A,       // X-Port = 5000 ^ (cookie >> 16)
            0xE1, 0x12, 0xA6, 0x43, // X-Address = 192.0.2.1 ^ cookie
        ])
        return bytes
    }

    /// Header(20) + XOR-MAPPED-ADDRESS(20) for 2001:db8::1:5000.
    /// IPv6 mask = cookie || transaction id; the address differs from the
    /// mask only in the first four bytes (0x20010DB8 ^ cookie = 0x0113A9FA).
    private func ipv6ResponseBytes() -> [UInt8] {
        var bytes: [UInt8] = [
            0x01, 0x01,
            0x00, 0x18,       // length: 24 attribute bytes
            0x21, 0x12, 0xA4, 0x42,
        ]
        bytes.append(contentsOf: transactionID)
        bytes.append(contentsOf: [
            0x00, 0x20,
            0x00, 0x14,       // value length: 20
            0x00,
            0x02,             // family: IPv6
            0x32, 0x9A,       // X-Port = 5000 ^ (cookie >> 16)
            // X-Address = 2001:0DB8:0000...0001 ^ (cookie || tid)
            0x01, 0x13, 0xA9, 0xFA,
            0x01, 0x02, 0x03, 0x04,
            0x05, 0x06, 0x07, 0x08,
            0x09, 0x0A, 0x0B, 0x0D,
        ])
        return bytes
    }

    func testBindingRequestBytesShape() {
        let id = STUNMessage.makeTransactionID()
        XCTAssertEqual(id.count, 12)
        let request = STUNMessage.bindingRequestBytes(transactionID: id)
        XCTAssertEqual(request.count, 20)
        XCTAssertEqual(UInt16(request[0]) << 8 | UInt16(request[1]), 0x0001)
        XCTAssertEqual(UInt16(request[2]) << 8 | UInt16(request[3]), 0, "no attributes")
        XCTAssertEqual(UInt32(request[4]) << 24 | UInt32(request[5]) << 16
                       | UInt32(request[6]) << 8 | UInt32(request[7]),
                       0x2112A442, "magic cookie")
        XCTAssertEqual(Array(request[8..<20]), id, "transaction id echoed verbatim")
    }

    func testParseIPv4XorMappedAddress() throws {
        let parsed = try STUNMessage.parse(ipv4ResponseBytes())
        XCTAssertEqual(parsed.type, 0x0101)
        XCTAssertEqual(parsed.transactionID, transactionID)
        XCTAssertEqual(parsed.xorMappedAddress, HostPort(host: "192.0.2.1", port: 5000))
    }

    func testParseIPv6XorMappedAddress() throws {
        let parsed = try STUNMessage.parse(ipv6ResponseBytes())
        XCTAssertEqual(parsed.type, 0x0101)
        XCTAssertEqual(parsed.xorMappedAddress?.port, 5000)
        XCTAssertEqual(parsed.xorMappedAddress?.host, "2001:db8::1")
    }

    func testParseSkipsUnknownAttributesWithPadding() throws {
        // SOFTWARE (0x8022) with a 5-byte value padded to 8, then the mapped
        // address: exercises 4-byte attribute alignment walking.
        var bytes: [UInt8] = [0x01, 0x01, 0x00, 0x18, 0x21, 0x12, 0xA4, 0x42]
        bytes.append(contentsOf: transactionID)
        bytes.append(contentsOf: [
            0x80, 0x22, 0x00, 0x05,
            0x74, 0x65, 0x73, 0x74, 0x31,       // "test1" (5 B value)
            0x00, 0x00, 0x00,                   // padded to a 4-byte boundary
        ])
        bytes.append(contentsOf: [
            0x00, 0x20, 0x00, 0x08, 0x00, 0x01, 0x32, 0x9A,
            0xE1, 0x12, 0xA6, 0x43,
        ])
        let parsed = try STUNMessage.parse(bytes)
        XCTAssertEqual(parsed.xorMappedAddress, HostPort(host: "192.0.2.1", port: 5000))
    }

    func testParseRejectsGarbage() {
        XCTAssertThrowsError(try STUNMessage.parse([0x01])) { error in
            XCTAssertEqual(error as? STUNError, .truncated)
        }
        var badCookie = ipv4ResponseBytes()
        badCookie[4] = 0x22
        XCTAssertThrowsError(try STUNMessage.parse(badCookie)) { error in
            XCTAssertEqual(error as? STUNError, .badMagicCookie)
        }
        var truncatedAttr = ipv4ResponseBytes()
        truncatedAttr[3] = 0x18 // claim more attribute bytes than present
        XCTAssertThrowsError(try STUNMessage.parse(truncatedAttr)) { error in
            XCTAssertEqual(error as? STUNError, .truncated)
        }
        // A binding REQUEST is structurally valid but carries no mapped
        // address: parse succeeds, address stays nil.
        let request = STUNMessage.bindingRequestBytes(transactionID: transactionID)
        let parsed = try? STUNMessage.parse(request)
        XCTAssertEqual(parsed?.type, 0x0001)
        XCTAssertNil(parsed?.xorMappedAddress)
    }

    func testHostPortWireString() {
        XCTAssertEqual(HostPort(host: "203.0.113.7", port: 8792).wireString, "203.0.113.7:8792")
        XCTAssertEqual(HostPort(host: "2001:db8::42", port: 8792).wireString, "[2001:db8::42]:8792")
        XCTAssertEqual(HostPort(wireString: "203.0.113.7:8792"),
                       HostPort(host: "203.0.113.7", port: 8792))
        XCTAssertEqual(HostPort(wireString: "[2001:db8::42]:8792"),
                       HostPort(host: "2001:db8::42", port: 8792))
        XCTAssertNil(HostPort(wireString: "no-port"))
        XCTAssertNil(HostPort(wireString: "host:notaport"))
        XCTAssertNil(HostPort(wireString: ""))
    }
}

// MARK: - SPKI pin math (the exact bytes the TLS verify block compares)

final class QUICPinTests: XCTestCase {
    func testSpkiSHA256MatchesHandComputedSPKI() {
        // A raw uncompressed P-256 public key: 0x04 || X || Y.
        var raw = Data([0x04])
        raw.append(contentsOf: [UInt8](repeating: 0x11, count: 32))
        raw.append(contentsOf: [UInt8](repeating: 0x22, count: 32))
        var expectedSPKI = Data([
            0x30, 0x59, 0x30, 0x13,
            0x06, 0x07, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x02, 0x01,
            0x06, 0x08, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x03, 0x01, 0x07,
            0x03, 0x42, 0x00,
        ])
        expectedSPKI.append(raw)
        let expected = Data(SHA256.hash(data: expectedSPKI))
        XCTAssertEqual(NWQUICMediaSocket.spkiSHA256(rawPublicKey: raw), expected)
    }

    func testSpkiSHA256FailsClosedOnOtherKeyShapes() {
        XCTAssertNil(NWQUICMediaSocket.spkiSHA256(rawPublicKey: Data([0x04, 0x01])))
        // Compressed key (0x02 prefix) must not be silently accepted.
        var compressed = Data([0x02])
        compressed.append(contentsOf: [UInt8](repeating: 0x11, count: 32))
        XCTAssertNil(NWQUICMediaSocket.spkiSHA256(rawPublicKey: compressed))
        XCTAssertNil(NWQUICMediaSocket.spkiSHA256(rawPublicKey: Data()))
    }

    func testConstantTimeEqual() {
        let a = Data([1, 2, 3, 4])
        XCTAssertTrue(NWQUICMediaSocket.constantTimeEqual(a, Data([1, 2, 3, 4])))
        XCTAssertFalse(NWQUICMediaSocket.constantTimeEqual(a, Data([1, 2, 3, 5])))
        XCTAssertFalse(NWQUICMediaSocket.constantTimeEqual(a, Data([1, 2, 3])))
        XCTAssertFalse(NWQUICMediaSocket.constantTimeEqual(Data(), Data([0])))
        XCTAssertTrue(NWQUICMediaSocket.constantTimeEqual(Data(), Data()))
    }
}
