import Foundation
import Network
import CryptoKit
import Security

/// QUIC direct audio transport (build 61 FINAL, protocol
/// `gateway/docs/quic61/protocol.md`): ONE `NWConnection` in QUIC datagram
/// mode (`NWProtocolQUIC.Options.isDatagram = true`) is exactly ONE QUIC
/// session with ONE unreliable DATAGRAM flow (RFC 9000 + RFC 9221), decided
/// by native interop evidence (protocol §0). All critical control (bind
/// authorization, ready, promote, denoise, bye, ping/pong) travels on the
/// EXISTING authenticated WSS plane; this socket carries audio envelopes,
/// bind bursts and loss-tolerant RTT probes only, multiplexed by the first
/// payload byte (protocol §2):
///
/// * `0x01` audio envelope `[0x01][epoch: u64 BE][seq16 WSS frame]`;
/// * `0x03` bind `[0x03][token]` (repeated in short bursts until downlink
///   audio flows; post-bind bursts are ignored, never counted);
/// * `0x04` RTT probe `[0x04][t: u64 BE]`, echoed verbatim by the peer.
///
/// The gateway serves a self-signed ECDSA P-256 certificate, so the TLS
/// handshake runs a `sec_protocol_options_set_verify_block` that checks BOTH
/// full certificate validity (against the pin document's notBefore/notAfter
/// window) AND the SPKI-SHA256 pin — there is deliberately NO trust-all
/// path anywhere, and the verify block never completes `true`
/// unconditionally (protocol §6).

/// The signature-verified pin facts the TLS verify block enforces (protocol
/// §6). The coordinator builds this from the authenticated
/// `GET /api/v2/media/quic` response AFTER verifying its Ed25519 signature
/// against the pairing identity key — never from anything arriving on the
/// QUIC connection itself.
struct QUICPin: Equatable, Sendable {
    /// base64-decoded SHA-256 of the PKIX SPKI DER (P-256 only).
    let spkiSHA256: Data
    /// Certificate-validity window from the signed pin document. Nil when
    /// the document omits a bound; the certificate's own dates are then the
    /// only window (checked via the self-anchored SecTrust evaluation).
    var notBefore: Date?
    var notAfter: Date?
}

enum QUICSocketState: Equatable, Sendable {
    case connecting
    /// Handshake complete: TLS 1.3 + ALPN negotiated, the verify block
    /// accepted the pinned certificate, the datagram flow is usable.
    case ready
    case failed(String)
    case closed
}

enum QUICSocketError: Error, Equatable {
    case notReady
    case closed
}

/// The socket seam `QUICCallMedia` talks to. `NWQUICMediaSocket` is the
/// production implementation; tests drive a fake (same precedent as
/// `WebSocketCallMedia.MediaSocket`).
protocol QUICMediaSocket: AnyObject, Sendable {
    /// Install callbacks BEFORE `start()`; all callbacks deliver on the
    /// socket's receive context or state-update context, NEVER on the main
    /// actor.
    ///
    /// One epoch-valid, envelope-stripped audio frame: `[seq16 WSS frame]`
    /// (the socket consumed `[0x01][epoch]`). Frames carrying any other
    /// epoch, a zero/unset expected epoch, or a malformed envelope are
    /// dropped and counted inside the socket — they never reach this
    /// callback (protocol §4).
    var onAudioFrame: (@Sendable (Data) -> Void)? { get set }
    /// Fired once on the FIRST well-formed audio envelope (version 0x01,
    /// valid length) even when the epoch gate drops the frame: it is the
    /// BIND PROOF that the gateway consumed the token and downlink flows.
    /// Distinct from `onAudioFrame`, which only fires for epoch-valid
    /// frames (the fresh-media proof). Transport proof ≠ media proof.
    var onBindProof: (@Sendable () -> Void)? { get set }
    /// Probe echo delivery: the echoed tag plus an arrival stamp taken by
    /// the socket ON THE RECEIVE CONTEXT, passed through verbatim
    /// (build-60 lesson: probe RTT and its freshness anchor must never
    /// measure the main-actor handling delay).
    var onProbeResponse: (@Sendable (UInt64, Date) -> Void)? { get set }
    var onStateChange: (@Sendable (QUICSocketState) -> Void)? { get set }
    /// Honest NAT-punch report (protocol §5): true when the platform
    /// declined `requiredLocalEndpoint` and dialed from a different local
    /// UDP port — the candidate attempt must then be bounded by the caller.
    var localEndpointMismatch: Bool { get }
    /// The WSS-plane ready/promoted epoch this socket stamps outbound
    /// envelopes with and validates inbound envelopes against (protocol §4).
    /// Defaults to 0: every inbound audio datagram is dropped until the
    /// coordinator binds the greeting's epoch.
    var expectedEpoch: UInt64 { get set }
    /// The last TLS verification failure reason (pin mismatch, validity
    /// window, non-P-256 key, missing chain), nil while the handshake is
    /// clean. Diagnostics only — a failure always completes the verify
    /// block with `false`; this never gates sends.
    var lastTLSFailure: String? { get }

    func start()
    /// Unreliable send of one WSS seq16 media frame; the socket stamps the
    /// `[0x01][expectedEpoch]` envelope. Throws only when the socket is not
    /// in a state that can accept datagrams. An oversized frame is dropped
    /// and counted (a truncated Opus frame would corrupt the receiver's
    /// decoder timeline; an honest hole is concealed by PLC).
    func sendAudioDatagram(_ frame: Data) throws
    /// Sends one `[0x03][token]` bind burst unit.
    func sendBind(token: String) throws
    /// Sends one `[0x04][t]` RTT probe; the peer echoes `t` verbatim.
    func sendProbe(t: UInt64) throws
    func close()
}

final class NWQUICMediaSocket: QUICMediaSocket, @unchecked Sendable {
    /// Protocol §4: envelope + payload bound.
    static let maxDatagramBytes = 1200
    /// `[0x01][epoch: u64 BE]` — the seq16 header stays INSIDE the frame.
    static let audioEnvelopeHeaderBytes = 9

    /// The SPKI DER prefix for an ECDSA P-256 (secp256r1) public key: the
    /// fixed `SEQUENCE { SEQUENCE { OID ecPublicKey, OID prime256v1 },
    /// BITSTRING }` header. The gateway's QUIC certificate is P-256 by
    /// protocol §6, so wrapping the raw key bytes reproduces exactly the
    /// DER that Go's `x509.MarshalPKIXPublicKey` produces — the pinned
    /// bytes on both sides hash the same SPKI.
    private static let p256SPKIDERPrefix: [UInt8] = [
        0x30, 0x59, 0x30, 0x13,
        0x06, 0x07, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x02, 0x01,
        0x06, 0x08, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x03, 0x01, 0x07,
        0x03, 0x42, 0x00,
    ]

    /// SHA-256 of the SPKI DER for a raw (uncompressed, 65-byte) P-256
    /// public key. Any other key shape fails closed (nil) — a regenerated
    /// or differently-curved cert can never match the published pin.
    static func spkiSHA256(rawPublicKey: Data) -> Data? {
        guard rawPublicKey.count == 65, rawPublicKey[0] == 0x04 else { return nil }
        var spki = Data(Self.p256SPKIDERPrefix)
        spki.append(rawPublicKey)
        return Data(SHA256.hash(data: spki))
    }

    /// Constant-time equality for the pin comparison (length is not secret;
    /// the bytes are).
    static func constantTimeEqual(_ a: Data, _ b: Data) -> Bool {
        guard a.count == b.count else { return false }
        var difference: UInt8 = 0
        for index in 0..<a.count { difference |= a[index] ^ b[index] }
        return difference == 0
    }

    // MARK: Wire framing (pure, unit-testable without a live session)

    enum DropReason: String, Equatable {
        case empty
        case oversize
        case unknownType
        case badEnvelope
        /// Epoch mismatch OR the expected epoch is still unset (0): the
        /// pre-ready gate from protocol §4 ("pre-bind/pre-attach audio").
        case staleEpoch
    }

    enum InboundFrame: Equatable {
        case audio(Data)
        /// Well-formed audio envelope whose epoch the client does not
        /// (yet) accept: bind proof, but never reaches the codec.
        case staleEpochAudio
        case probe(UInt64)
        /// `0x03` post-bind bursts: ignored by contract, not counted.
        case ignoredBind
        case dropped(DropReason)
    }

    static func writeU64BE(_ value: UInt64, into data: inout Data, at offset: Int) {
        for shift in stride(from: 56, through: 0, by: -8) {
            data[offset + (56 - shift) / 8] = UInt8((value >> UInt64(shift)) & 0xff)
        }
    }

    static func readU64BE(_ data: Data, at offset: Int) -> UInt64 {
        var value: UInt64 = 0
        for index in 0..<8 {
            value = (value << 8) | UInt64(data[offset + index])
        }
        return value
    }

    /// First-byte multiplexing of one inbound UDP payload (protocol §2).
    /// Everything that fails validation is classified here (never reaches
    /// a callback) so the receive loop can count it honestly.
    static func classifyInbound(
        _ datagram: Data,
        expectedEpoch: UInt64,
        maxBytes: Int = maxDatagramBytes
    ) -> InboundFrame {
        guard !datagram.isEmpty else { return .dropped(.empty) }
        guard datagram.count <= maxBytes else { return .dropped(.oversize) }
        switch datagram[0] {
        case 0x01:
            // [0x01][epoch: u64 BE][seq16 WSS frame]: the header strip is
            // exactly version+epoch = 9 bytes; the seq16 header stays
            // inside the delivered frame. An epoch mismatch still proves
            // the bind (gateway stamped with ITS current epoch; the client
            // learns the epoch from the WSS greeting slightly later), so
            // the envelope surfaces as bindProof while remaining
            // epoch-DROPPED for the codec.
            guard datagram.count > audioEnvelopeHeaderBytes else { return .dropped(.badEnvelope) }
            let epoch = readU64BE(datagram, at: 1)
            guard expectedEpoch != 0, epoch == expectedEpoch else { return .staleEpochAudio }
            return .audio(datagram.subdata(in: audioEnvelopeHeaderBytes..<datagram.count))
        case 0x03:
            return .ignoredBind
        case 0x04:
            // [0x04][t: u64 BE], echoed verbatim; anything else is garbage.
            guard datagram.count == 1 + 8 else { return .dropped(.badEnvelope) }
            return .probe(readU64BE(datagram, at: 1))
        default:
            return .dropped(.unknownType)
        }
    }

    /// Builds `[0x01][epoch][frame]`; nil when the envelope would exceed
    /// the protocol's 1200-byte bound (caller drops and counts).
    static func stampAudioEnvelope(epoch: UInt64, frame: Data) -> Data? {
        guard frame.count + audioEnvelopeHeaderBytes <= maxDatagramBytes else { return nil }
        var envelope = Data(count: audioEnvelopeHeaderBytes)
        envelope[0] = 0x01
        writeU64BE(epoch, into: &envelope, at: 1)
        envelope.append(frame)
        return envelope
    }

    static func bindDatagram(token: String) -> Data {
        var datagram = Data([0x03])
        datagram.append(Data(token.utf8))
        return datagram
    }

    static func probeDatagram(t: UInt64) -> Data {
        var datagram = Data(count: 1 + 8)
        datagram[0] = 0x04
        writeU64BE(t, into: &datagram, at: 1)
        return datagram
    }

    // MARK: TLS verification

    /// The sec_protocol verify block, as a pure function of the handshake
    /// metadata: (a) full certificate-validity check — the gateway's cert is
    /// self-signed, so evaluating a SecTrust anchored on the leaf itself is
    /// exactly the signature + notBefore/notAfter check
    /// (`kSecOIDX509V1Validity*` are macOS-only and cannot be used on iOS),
    /// plus the pin document's own window when it carries one; (b) the
    /// SPKI-SHA256 pin via a constant-time compare, P-256 keys only.
    /// Returns the failure reason (nil on success) so the socket can expose
    /// it as `lastTLSFailure` — and so the handshake can NEVER pass on a
    /// mismatched or expired certificate.
    static func verifyPeer(metadata: sec_protocol_metadata_t, pin: QUICPin) -> String? {
        var leaf: SecCertificate?
        sec_protocol_metadata_access_peer_certificate_chain(metadata) { certificate in
            // Build-62: bridge the `OS_sec_certificate` wrapper through the
            // documented `sec_certificate_copy_ref` API (never a force-cast:
            // a crashed verify block would fail the handshake path in a way
            // no test could observe).
            if leaf == nil {
                leaf = sec_certificate_copy_ref(certificate).takeRetainedValue()
            }
        }
        guard let leaf else { return "no peer certificate" }
        guard certificateIsCurrentlyValid(leaf) else {
            return "certificate not currently valid"
        }
        let now = Date()
        if let notBefore = pin.notBefore, now < notBefore {
            return "certificate before pin notBefore"
        }
        if let notAfter = pin.notAfter, now > notAfter {
            return "certificate past pin notAfter"
        }
        guard let key = SecCertificateCopyKey(leaf),
              let raw = SecKeyCopyExternalRepresentation(key, nil) as Data?,
              let spkiHash = spkiSHA256(rawPublicKey: raw) else {
            return "peer key is not an uncompressed P-256 key"
        }
        guard constantTimeEqual(spkiHash, pin.spkiSHA256) else {
            return "spki pin mismatch"
        }
        return nil
    }

    static func certificateIsCurrentlyValid(_ certificate: SecCertificate) -> Bool {
        var trust: SecTrust?
        guard SecTrustCreateWithCertificates(
            certificate, SecPolicyCreateBasicX509(), &trust) == errSecSuccess,
              let trust else { return false }
        SecTrustSetAnchorCertificates(trust, [certificate] as CFArray)
        var error: CFError?
        return SecTrustEvaluateWithError(trust, &error)
    }

    private static func installPinVerifyBlock(
        on options: NWProtocolQUIC.Options,
        pin: QUICPin,
        queue: DispatchQueue,
        failureSink: @escaping @Sendable (String) -> Void
    ) {
        // Argument order is (options, verify_block, verify_block_queue).
        sec_protocol_options_set_verify_block(options.securityProtocolOptions, { metadata, _, complete in
            if let failure = Self.verifyPeer(metadata: metadata, pin: pin) {
                failureSink(failure)
                complete(false)
            } else {
                complete(true)
            }
        }, queue)
    }

    // MARK: State

    private let connection: NWConnection
    private let requiredLocalEndpoint: NWEndpoint?
    private let queue = DispatchQueue(label: "callrelay.quic.socket")
    /// Written by the verify block before any handshake failure state;
    /// reads never touch the socket queue.
    private let tlsFailureBox = TLSFailureBox()

    private let lock = NSLock()
    private var started = false
    private var closed = false
    private var closedEmitted = false
    private var failedEmitted = false
    private var ready = false
    private var readyEmitted = false
    private var usableDatagramSize = NWQUICMediaSocket.maxDatagramBytes
    private var localEndpointMismatchFlag = false
    private var expectedEpochValue: UInt64 = 0
    private var bindProofFired = false

    var onBindProof: (@Sendable () -> Void)?

    private func noteBindProof() {
        lock.lock()
        guard !bindProofFired else { lock.unlock(); return }
        bindProofFired = true
        let callback = onBindProof
        lock.unlock()
        callback?()
    }

    var onAudioFrame: (@Sendable (Data) -> Void)?
    var onProbeResponse: (@Sendable (UInt64, Date) -> Void)?
    var onStateChange: (@Sendable (QUICSocketState) -> Void)?

    var localEndpointMismatch: Bool {
        lock.lock(); defer { lock.unlock() }
        return localEndpointMismatchFlag
    }

    var expectedEpoch: UInt64 {
        get {
            lock.lock(); defer { lock.unlock() }
            return expectedEpochValue
        }
        set {
            lock.lock(); expectedEpochValue = newValue; lock.unlock()
        }
    }

    var lastTLSFailure: String? { tlsFailureBox.snapshot() }

    /// - Parameter pin: the SPKI-SHA256 pin plus validity window from the
    ///   authenticated pin document (already signature-verified by the
    ///   caller).
    init(pin: QUICPin,
         host: String,
         port: UInt16,
         alpn: String = "callrelay-media",
         localEndpoint: NWEndpoint? = nil) {
        self.requiredLocalEndpoint = localEndpoint
        let endpoint = NWEndpoint.hostPort(host: NWEndpoint.Host(host),
                                           port: NWEndpoint.Port(rawValue: port)!)

        // ONE QUIC session, datagram mode only (protocol §0/§2): no second
        // connection, no control stream — control lives on the WSS plane.
        let options = NWProtocolQUIC.Options(alpn: [alpn])
        options.isDatagram = true
        // Advertise the RFC 9221 DATAGRAM flow explicitly (WWDC22 10078):
        // `isDatagram` alone does NOT negotiate `max_datagram_frame_size`
        // against quic-go — the build-62 native smoke measured the server
        // answering "datagram support disabled" without this. The bound
        // matches the protocol's 1200-byte envelope maximum.
        options.maxDatagramFrameSize = Self.maxDatagramBytes
        options.maxUDPPayloadSize = 1350
        // The failure box (not self) is captured so the verify block can be
        // installed before every stored property is initialized.
        let failureBox = tlsFailureBox
        Self.installPinVerifyBlock(on: options, pin: pin, queue: queue) { reason in
            failureBox.record(reason)
        }

        let parameters = NWParameters(quic: options)
        // The punched UDP source port lives on the PARAMETERS and must be
        // bound BEFORE the connection is created. Local-endpoint reuse is
        // allowed: the STUN socket that discovered this port cancels right
        // before the dial (protocol §5 port sharing) and without reuse the
        // sequential bind races the kernel's async port release (measured
        // as NWError 48 in the build-62 native smoke).
        parameters.allowLocalEndpointReuse = true
        if let localEndpoint {
            parameters.requiredLocalEndpoint = localEndpoint
        }
        connection = NWConnection(to: endpoint, using: parameters)
    }

    // MARK: Lifecycle

    func start() {
        lock.lock()
        guard !started, !closed else { lock.unlock(); return }
        started = true
        lock.unlock()

        connection.stateUpdateHandler = { [weak self] state in
            self?.handleState(state)
        }
        receiveLoop()
        connection.start(queue: queue)
    }

    func close() {
        lock.lock()
        guard !closed else { lock.unlock(); return }
        closed = true
        lock.unlock()
        connection.cancel()
        emitClosed()
    }

    // MARK: Send

    func sendAudioDatagram(_ frame: Data) throws {
        lock.lock()
        guard !closed, ready else { lock.unlock(); throw QUICSocketError.notReady }
        let epoch = expectedEpochValue
        lock.unlock()
        guard let envelope = Self.stampAudioEnvelope(epoch: epoch, frame: frame) else {
            DiagnosticsCensus.shared.increment("audio.quicDatagramOversize")
            return
        }
        connection.send(content: envelope, isComplete: true, completion: .contentProcessed {
            [weak self] error in
            if let error {
                self?.failWith("datagram send: \(error.localizedDescription)")
            }
        })
    }

    func sendBind(token: String) throws {
        try sendRaw(Self.bindDatagram(token: token))
    }

    func sendProbe(t: UInt64) throws {
        try sendRaw(Self.probeDatagram(t: t))
    }

    private func sendRaw(_ datagram: Data) throws {
        lock.lock()
        guard !closed, ready else { lock.unlock(); throw QUICSocketError.notReady }
        lock.unlock()
        connection.send(content: datagram, isComplete: true, completion: .contentProcessed {
            [weak self] error in
            if let error {
                self?.failWith("datagram send: \(error.localizedDescription)")
            }
        })
    }

    // MARK: Receive loop

    /// DATAGRAM flow: one receiveMessage per datagram (RFC 9221 frame).
    /// The arrival stamp for probe echoes is taken HERE, on the receive
    /// context, before any actor hop (build-60 discipline).
    private func receiveLoop() {
        connection.receiveMessage { [weak self] content, _, _, error in
            guard let self else { return }
            if let error {
                self.failWith("datagram receive: \(error.localizedDescription)")
                return
            }
            if let content {
                self.lock.lock()
                let epoch = self.expectedEpochValue
                let maxBytes = self.usableDatagramSize
                self.lock.unlock()
                switch Self.classifyInbound(content, expectedEpoch: epoch, maxBytes: maxBytes) {
                case .audio(let payload):
                    self.noteBindProof()
                    self.lock.lock()
                    let callback = self.onAudioFrame
                    self.lock.unlock()
                    callback?(payload)
                case .staleEpochAudio:
                    // Bind proof even though the epoch gate drops the
                    // frame for the codec (see classifyInbound).
                    self.noteBindProof()
                    DiagnosticsCensus.shared.increment("audio.quicDrop.staleEpoch")
                case .probe(let t):
                    self.lock.lock()
                    let callback = self.onProbeResponse
                    self.lock.unlock()
                    callback?(t, Date())
                case .ignoredBind:
                    break
                case .dropped(let reason):
                    DiagnosticsCensus.shared.increment("audio.quicDrop.\(reason.rawValue)")
                }
            }
            self.receiveLoop()
        }
    }

    // MARK: State

    private func handleState(_ state: NWConnection.State) {
        switch state {
        case .ready:
            if let metadata = connection.metadata(definition: NWProtocolQUIC.definition)
                as? NWProtocolQUIC.Metadata {
                let size = metadata.usableDatagramFrameSize
                lock.lock()
                if size > 0 { usableDatagramSize = min(size, Self.maxDatagramBytes) }
                lock.unlock()
            }
            verifyLocalEndpoint()
            lock.lock()
            ready = true
            let becameReady = !readyEmitted
            readyEmitted = true
            lock.unlock()
            if becameReady { emit(.ready) }
        case .waiting, .preparing:
            // Path discovery / NAT punching in progress; stay `connecting`.
            break
        case .failed(let error):
            failWith(error.localizedDescription)
        case .cancelled:
            emitClosed()
        default:
            break
        }
    }

    /// Protocol §5: when a required local endpoint was requested, verify
    /// the platform actually dialed from it; report honestly, keep going.
    private func verifyLocalEndpoint() {
        guard let required = requiredLocalEndpoint,
              let actual = connection.currentPath?.localEndpoint,
              actual != required else { return }
        lock.lock()
        localEndpointMismatchFlag = true
        lock.unlock()
    }

    private func emit(_ state: QUICSocketState) {
        lock.lock()
        let callback = onStateChange
        lock.unlock()
        callback?(state)
    }

    private func emitClosed() {
        lock.lock()
        guard !closedEmitted else { lock.unlock(); return }
        closedEmitted = true
        let callback = onStateChange
        lock.unlock()
        callback?(.closed)
    }

    private func failWith(_ reason: String) {
        lock.lock()
        guard !closed, !failedEmitted, !closedEmitted else { lock.unlock(); return }
        failedEmitted = true
        let callback = onStateChange
        lock.unlock()
        callback?(.failed(reason))
    }
}

/// Lock-owned slot for the TLS verify block's failure reason: the block runs
/// on the socket queue (possibly before the socket's stored properties are
/// even initialized), so the reason cannot live in main-actor state.
private final class TLSFailureBox: @unchecked Sendable {
    private let lock = NSLock()
    private var message: String?

    func record(_ message: String) {
        lock.lock()
        self.message = message
        lock.unlock()
    }

    func snapshot() -> String? {
        lock.lock()
        defer { lock.unlock() }
        return message
    }
}
