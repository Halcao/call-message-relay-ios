import Foundation
import Network
import Security

/// QUIC direct audio needs the phone's own server-reflexive UDP candidate
/// for NAT hole punching (protocol §5). This is a minimal RFC 8489 STUN
/// binding client: the wire build/parse is PURE and fully unit-testable, the
/// socket is a thin `NWConnection` UDP wrapper around it.
///
/// The same STUN services the gateway already publishes in the ICE
/// configuration are reused here; no new discovery surface is introduced.

/// One host:port endpoint (the gateway's srflx candidate, the phone's own
/// mapped candidate, or a STUN server address).
struct HostPort: Hashable, Sendable {
    var host: String
    var port: UInt16

    /// Explicit because declaring `init?(wireString:)` in the body
    /// suppresses Swift's synthesized memberwise initializer.
    init(host: String, port: UInt16) {
        self.host = host
        self.port = port
    }

    /// "ip:port" rendering for the candidate exchange REST body. IPv6
    /// addresses render bracketed so the gateway's split on the LAST colon
    /// recovers the port unambiguously.
    var wireString: String {
        host.contains(":") ? "[\(host)]:\(port)" : "\(host):\(port)"
    }

    /// Parse an "ip:port" / "[v6]:port" candidate string; nil when the port
    /// is missing or not a number.
    init?(wireString: String) {
        let trimmed = wireString.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return nil }
        if trimmed.hasPrefix("["), let close = trimmed.firstIndex(of: "]") {
            let host = String(trimmed[trimmed.index(after: trimmed.startIndex)..<close])
            let rest = trimmed[trimmed.index(after: close)...]
            guard rest.hasPrefix(":"), let port = UInt16(rest.dropFirst()) else { return nil }
            self.init(host: host, port: port)
        } else {
            guard let split = trimmed.range(of: ":", options: .backwards),
                  let port = UInt16(trimmed[split.upperBound...]) else { return nil }
            let host = String(trimmed[..<split.lowerBound])
            guard !host.isEmpty else { return nil }
            self.init(host: host, port: port)
        }
    }
}

// MARK: - RFC 8489 wire format (pure)

enum STUNError: Error, Equatable {
    case truncated
    case badMagicCookie
    case malformedAttribute
    case transactionMismatch
    case unsupportedAddressFamily
    /// The response decoded but carried no XOR-MAPPED-ADDRESS.
    case noMappedAddress
    case timeout
}

enum STUNMessage {
    /// Binding Request (RFC 8489 §5).
    static let bindingRequestType: UInt16 = 0x0001
    /// Binding Success Response (RFC 8489 §5).
    static let bindingSuccessResponseType: UInt16 = 0x0101
    /// The fixed magic cookie (RFC 8489 §5); also the IPv4 XOR mask source.
    static let magicCookie: UInt32 = 0x2112A442
    /// XOR-MAPPED-ADDRESS attribute type (RFC 8489 §14.2).
    static let xorMappedAddressType: UInt16 = 0x0020

    /// One decoded STUN message. Only what the binding client needs: the
    /// message type, the echoed transaction id, and (when present) the
    /// decoded XOR-MAPPED-ADDRESS.
    struct Parsed: Equatable {
        let type: UInt16
        /// The 12-byte transaction id.
        let transactionID: [UInt8]
        let xorMappedAddress: HostPort?
    }

    // 大端读写助手（STUN 所有多字节字段均为网络字节序）。
    private static func u16(_ bytes: [UInt8], _ offset: Int) -> UInt16 {
        UInt16(bytes[offset]) << 8 | UInt16(bytes[offset + 1])
    }

    private static func u32(_ bytes: [UInt8], _ offset: Int) -> UInt32 {
        UInt32(bytes[offset]) << 24 | UInt32(bytes[offset + 1]) << 16
            | UInt32(bytes[offset + 2]) << 8 | UInt32(bytes[offset + 3])
    }

    /// Build a Binding Request: 20-byte header (type, length=0, magic
    /// cookie, transaction id), no attributes — the phone only needs its own
    /// mapped address back. `transactionID` MUST be 12 random bytes.
    static func bindingRequestBytes(transactionID: [UInt8]) -> [UInt8] {
        precondition(transactionID.count == 12)
        var out = [UInt8](repeating: 0, count: 20)
        out[0] = UInt8(bindingRequestType >> 8)
        out[1] = UInt8(bindingRequestType & 0xff)
        // length = 0 (bytes 2...3 already zero)
        out[4] = UInt8(magicCookie >> 24)
        out[5] = UInt8((magicCookie >> 16) & 0xff)
        out[6] = UInt8((magicCookie >> 8) & 0xff)
        out[7] = UInt8(magicCookie & 0xff)
        for i in 0..<12 { out[8 + i] = transactionID[i] }
        return out
    }

    /// Parse one (possibly reassembled) STUN message. Returns nil for
    /// anything shorter than a header; throws on a structurally corrupt
    /// message. Unknown attributes are skipped (padded to 4-byte alignment);
    /// only XOR-MAPPED-ADDRESS (IPv4 + IPv6) is decoded.
    static func parse(_ bytes: [UInt8]) throws -> Parsed {
        guard bytes.count >= 20 else { throw STUNError.truncated }
        let type = u16(bytes, 0)
        let length = Int(u16(bytes, 2))
        guard u32(bytes, 4) == magicCookie else { throw STUNError.badMagicCookie }
        // length covers only attributes; the whole message is 20 + length.
        guard 20 + length <= bytes.count else { throw STUNError.truncated }
        let transactionID = Array(bytes[8..<20])
        var xorMapped: HostPort? = nil
        var cursor = 20
        let end = 20 + length
        while cursor + 4 <= end {
            let attrType = u16(bytes, cursor)
            let attrLength = Int(u16(bytes, cursor + 2))
            let valueStart = cursor + 4
            guard valueStart + attrLength <= end else { throw STUNError.malformedAttribute }
            if attrType == xorMappedAddressType {
                xorMapped = try decodeXorMappedAddress(bytes, valueStart, attrLength, transactionID)
            }
            // Attributes are padded to 4-byte boundaries (RFC 8489 §5).
            cursor = valueStart + ((attrLength + 3) & ~3)
        }
        return Parsed(type: type, transactionID: transactionID, xorMappedAddress: xorMapped)
    }

    /// RFC 8489 §14.2: port XOR cookie>>16; IPv4 address XOR cookie (4 B);
    /// IPv6 address XOR cookie || transaction id (16 B).
    private static func decodeXorMappedAddress(
        _ bytes: [UInt8], _ start: Int, _ length: Int, _ transactionID: [UInt8]
    ) throws -> HostPort {
        guard length >= 4, start + length <= bytes.count else { throw STUNError.malformedAttribute }
        let family = bytes[start + 1]
        let xport = u16(bytes, start + 2)
        let port = UInt16(xport ^ UInt16(magicCookie >> 16))
        switch family {
        case 0x01:
            guard length >= 8 else { throw STUNError.malformedAttribute }
            let xaddr = u32(bytes, start + 4)
            let addr = xaddr ^ magicCookie
            let host = "\(addr >> 24 & 0xff).\(addr >> 16 & 0xff).\(addr >> 8 & 0xff).\(addr & 0xff)"
            return HostPort(host: host, port: port)
        case 0x02:
            guard length >= 20 else { throw STUNError.malformedAttribute }
            // XOR mask = magic cookie (4 B) || transaction id (12 B).
            var mask = [UInt8](repeating: 0, count: 16)
            mask[0] = UInt8(magicCookie >> 24)
            mask[1] = UInt8((magicCookie >> 16) & 0xff)
            mask[2] = UInt8((magicCookie >> 8) & 0xff)
            mask[3] = UInt8(magicCookie & 0xff)
            for i in 0..<12 { mask[4 + i] = transactionID[i] }
            var raw = [UInt8](repeating: 0, count: 16)
            for i in 0..<16 { raw[i] = bytes[start + 4 + i] ^ mask[i] }
            let host = IPv6AddressText(raw)
            return HostPort(host: host, port: port)
        default:
            throw STUNError.unsupportedAddressFamily
        }
    }

    /// Compact RFC 5952-ish IPv6 text (lowercase hex, one "::" collapse of
    /// the first all-zero run). Enough for candidate strings; the gateway
    /// parses with net.ParseIP which accepts this form.
    private static func IPv6AddressText(_ raw: [UInt8]) -> String {
        func group(_ i: Int) -> String {
            String(format: "%x", UInt16(raw[i * 2]) << 8 | UInt16(raw[i * 2 + 1]))
        }
        let groups = (0..<8).map(group)
        // Find the longest run of zero groups and collapse it.
        var bestStart = -1, bestLen = 0
        var i = 0
        while i < 8 {
            if groups[i] == "0" {
                var j = i
                while j < 8, groups[j] == "0" { j += 1 }
                if j - i > bestLen { bestStart = i; bestLen = j - i }
                i = j
            } else {
                i += 1
            }
        }
        if bestLen > 1 {
            let head = bestStart == 0 ? "" : groups[0..<bestStart].joined(separator: ":")
            let tailStart = bestStart + bestLen
            let tail = tailStart >= 8 ? "" : groups[tailStart...].joined(separator: ":")
            switch (head.isEmpty, tail.isEmpty) {
            case (true, true): return "::"
            case (true, false): return "::\(tail)"
            case (false, true): return "\(head)::"
            default: return "\(head)::\(tail)"
            }
        }
        // Re-emit with leading zeros dropped (the %x format already did).
        return groups.joined(separator: ":")
    }

    /// 12 crypto-quality random bytes for a transaction id.
    static func makeTransactionID() -> [UInt8] {
        var bytes = [UInt8](repeating: 0, count: 12)
        let status = SecRandomCopyBytes(kSecRandomDefault, 12, &bytes)
        precondition(status == errSecSuccess, "SecRandomCopyBytes failed: \(status)")
        return bytes
    }
}

// MARK: - Thin socket (Network.framework UDP)

/// The outcome of one STUN binding exchange. `local` is the socket's ACTUAL
/// local UDP endpoint (the interface address and source port the OS
/// assigned) — what the QUIC socket must bind via `requiredLocalEndpoint`.
/// `mapped` is the XOR-MAPPED-ADDRESS (the server-reflexive candidate the
/// gateway punches). They are DIFFERENT endpoints under NAT (and the port
/// numbers routinely differ); conflating them binds QUIC to an unowned
/// public address.
struct STUNBindingResult: Equatable, Sendable {
    let local: HostPort?
    let mapped: HostPort
}

enum STUNBindingClient {
    /// One binding exchange against `server`, optionally bound to
    /// `localEndpoint` (the NAT-punch discipline: the QUIC socket reuses the
    /// STUN socket's actual local port, so both flows share one NAT
    /// binding). Returns BOTH the observed local endpoint and the mapped
    /// srflx candidate — the caller binds QUIC to `local` and advertises
    /// `mapped` to the gateway.
    static func querySTUNCandidate(
        host: String,
        port: UInt16,
        timeout: TimeInterval = 3,
        localEndpoint: NWEndpoint? = nil
    ) async throws -> STUNBindingResult {
        let endpoint = NWEndpoint.hostPort(host: NWEndpoint.Host(host),
                                           port: NWEndpoint.Port(rawValue: port)!)
        // The required local endpoint lives on the PARAMETERS (set before
        // the connection is created), not on the connection.
        let parameters = NWParameters.udp
        if let localEndpoint {
            parameters.requiredLocalEndpoint = localEndpoint
        }
        let connection = NWConnection(to: endpoint, using: parameters)
        let queue = DispatchQueue(label: "callrelay.stun.query")
        connection.start(queue: queue)
        defer { connection.cancel() }

        // Wait until the UDP socket is actually bound so the OBSERVED local
        // endpoint (not a guess, not the mapped address) is known. This is
        // the port the QUIC socket must reuse.
        guard await waitUntilBound(connection, timeout: timeout) else {
            throw STUNError.timeout
        }
        let local = connection.currentPath?.localEndpoint.flatMap(hostPort(from:))

        let transactionID = STUNMessage.makeTransactionID()
        let request = STUNMessage.bindingRequestBytes(transactionID: transactionID)

        return try await withThrowingTaskGroup(of: STUNBindingResult.self) { group in
            group.addTask {
                try await withCheckedThrowingContinuation { continuation in
                    connection.send(content: Data(request), completion: .contentProcessed { _ in })
                    receiveResponse(connection: connection, transactionID: transactionID,
                                    local: local, attemptsLeft: 3, continuation: continuation)
                }
            }
            group.addTask {
                try await Task.sleep(nanoseconds: UInt64(timeout * 1_000_000_000))
                connection.cancel()
                throw STUNError.timeout
            }
            let result = try await group.next()!
            group.cancelAll()
            return result
        }
    }

    /// Extract a plain host:port from a path endpoint; nil for anything that
    /// is not an IP host:port (a name endpoint cannot be a required local
    /// endpoint for the QUIC dial).
    static func hostPort(from endpoint: NWEndpoint) -> HostPort? {
        guard case .hostPort(let host, let port) = endpoint else { return nil }
        return HostPort(host: "\(host)", port: port.rawValue)
    }

    /// One-shot wait for the UDP connection to become ready (i.e. the socket
    /// is bound and the actual local port is assigned). Times out rather
    /// than waiting forever so the caller's budget still bounds the attempt.
    private static func waitUntilBound(
        _ connection: NWConnection, timeout: TimeInterval
    ) async -> Bool {
        await withCheckedContinuation { continuation in
            // The state handler fires with the CURRENT state when assigned;
            // guard anyway so an already-ready connection cannot strand the
            // continuation if that ever changes.
            if connection.state == .ready {
                continuation.resume(returning: true)
                return
            }
            let waiter = BoundWaiter(continuation: continuation)
            connection.stateUpdateHandler = { state in
                switch state {
                case .ready:
                    waiter.resume(returning: true)
                case .failed, .cancelled:
                    waiter.resume(returning: false)
                default:
                    break
                }
            }
            waiter.arm(timeout: timeout) { connection.cancel() }
        }
    }

    /// Thread-safe one-shot resume for `waitUntilBound`, plus its own
    /// timeout so a silently-stuck socket still releases the continuation.
    private final class BoundWaiter: @unchecked Sendable {
        private let lock = NSLock()
        private var continuation: CheckedContinuation<Bool, Never>?

        init(continuation: CheckedContinuation<Bool, Never>) {
            self.continuation = continuation
        }

        func resume(returning value: Bool) {
            lock.lock()
            let target = continuation
            continuation = nil
            lock.unlock()
            target?.resume(returning: value)
        }

        func arm(timeout: TimeInterval, onTimeout: @escaping @Sendable () -> Void) {
            Task {
                try? await Task.sleep(nanoseconds: UInt64(timeout * 1_000_000_000))
                self.lock.lock()
                let pending = self.continuation != nil
                self.continuation = nil
                self.lock.unlock()
                if pending {
                    onTimeout()
                }
            }
        }
    }

    /// UDP may drop the first binding response on a fresh mapping; retry the
    /// request up to `attemptsLeft` times (each send re-opens the mapping).
    private static func receiveResponse(
        connection: NWConnection,
        transactionID: [UInt8],
        local: HostPort?,
        attemptsLeft: Int,
        continuation: CheckedContinuation<STUNBindingResult, Error>
    ) {
        connection.receiveMessage { content, _, _, error in
            if let error {
                continuation.resume(throwing: error)
                return
            }
            guard let content, !content.isEmpty else {
                continuation.resume(throwing: STUNError.truncated)
                return
            }
            do {
                let parsed = try STUNMessage.parse(Array(content))
                guard parsed.transactionID == transactionID else {
                    throw STUNError.transactionMismatch
                }
                guard parsed.type == STUNMessage.bindingSuccessResponseType else {
                    throw STUNError.noMappedAddress
                }
                guard let mapped = parsed.xorMappedAddress else {
                    throw STUNError.noMappedAddress
                }
                continuation.resume(returning: STUNBindingResult(local: local, mapped: mapped))
            } catch {
                if attemptsLeft > 0 {
                    let request = STUNMessage.bindingRequestBytes(transactionID: transactionID)
                    connection.send(content: Data(request), completion: .contentProcessed { _ in })
                    receiveResponse(connection: connection, transactionID: transactionID,
                                    local: local, attemptsLeft: attemptsLeft - 1,
                                    continuation: continuation)
                } else {
                    continuation.resume(throwing: error)
                }
            }
        }
    }
}
