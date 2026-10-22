package media

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// This file implements hostSocket over a QUIC DATAGRAM connection (build
// 61). Audio frames travel in unreliable RFC 9221 DATAGRAM frames wrapped
// in the QUIC envelope below; control lines travel on the connection's
// single bidirectional stream, newline-delimited, exactly mirroring the
// WSS text/binary protocol. The envelope is the epoch fence unordered
// datagrams need: a stale-epoch, pre-auth or superseded packet is dropped
// HERE and can never reach the shared codec/mixer.

// QUICStream is the narrow stream surface the socket needs (quic-go's
// *quic.Stream satisfies it; tests use fakes).
type QUICStream interface {
	io.Reader
	io.Writer
	io.Closer
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
}

// QUICConnection is the narrow connection surface the socket needs. The
// quic package wraps the real quic-go connection to satisfy it.
type QUICConnection interface {
	// AcceptStream returns the client-initiated bidirectional control
	// stream. Called once at socket start.
	AcceptStream(ctx context.Context) (QUICStream, error)
	SendDatagram(payload []byte) error
	ReceiveDatagram(ctx context.Context) ([]byte, error)
	CloseWithError(code uint64, message string) error
	RemoteAddr() net.Addr
}

// QUIC envelope wire format (version 1):
//
//	[version: u8 = 1][epoch: u64 big-endian][seq16 WSS frame bytes]
//
// The trailing bytes are EXACTLY the WSS binary frame ([seq u16][payload]
// for fmt=seq16, or the bare 160-byte PCMU frame), so after envelope
// validation the receive path is byte-identical to the WebSocket path.
const (
	QUICEnvelopeVersion = 0x01
	// QUICBindVersion marks a BIND datagram: [0x03][token bytes]. Sent by
	// the client in short bursts until downlink audio flows; the token is
	// the single-use punch token from the authenticated candidate POST,
	// so the bind cannot authorize twice and a stray datagram at the
	// punched port cannot attach without the authenticated exchange.
	QUICBindVersion = 0x03
	// QUICProbeVersion marks a loss-tolerant RTT probe: [0x04][u64 BE t];
	// the peer echoes it verbatim. Used only for measurement (receive-
	// context arrival stamping), never for ownership decisions.
	QUICProbeVersion   = 0x04
	quicEnvelopeHeader = 9 // version(1) + epoch(8); seq16 stays inside the frame
	// quicMaxAudioDatagram bounds a single outbound audio datagram
	// (envelope + largest seq16 Opus frame) below the default path MTU.
	quicMaxAudioDatagram = 1200
	// quicInboundQueue bounds the socket's internal frame queue; like the
	// WebSocket kernel buffer it absorbs jitter bursts but can never grow
	// without bound. New audio datagrams are dropped on overflow (newest is
	// what matters); control lines always wait briefly instead.
	quicInboundQueue = 384
	// quicControlWaitTimeout bounds how long a control line may wait behind
	// a full queue before the socket converges as failed.
	quicControlWaitTimeout = 10 * time.Second
)

// StampQUICEnvelope returns src prefixed with the version+epoch header.
// Called on the send path only (mixer goroutine or control writer).
func StampQUICEnvelope(src []byte, epoch uint64) []byte {
	out := make([]byte, 0, quicEnvelopeHeader+len(src))
	out = append(out, QUICEnvelopeVersion)
	out = binary.BigEndian.AppendUint64(out, epoch)
	out = append(out, src...)
	return out
}

// ParseQUICEnvelope validates a datagram against the expected epoch and
// returns the inner WSS frame. ok=false means DROP (wrong version, short
// packet, or epoch mismatch — stale attach/promote/network generation).
func ParseQUICEnvelope(pkt []byte, expectEpoch uint64) (frame []byte, ok bool) {
	if len(pkt) < quicEnvelopeHeader {
		return nil, false
	}
	if pkt[0] != QUICEnvelopeVersion {
		return nil, false
	}
	if binary.BigEndian.Uint64(pkt[1:9]) != expectEpoch {
		return nil, false
	}
	return pkt[quicEnvelopeHeader:], true
}

type quicInboundMsg struct {
	msgType int // websocket.TextMessage | websocket.BinaryMessage
	payload []byte
	// epoch is the envelope epoch the frame was validated against at
	// intake. ReadMessage revalidates it at DEQUEUE (after any queue
	// wait) so a queued old-generation frame can never be relabelled
	// into the new epoch by a promote race.
	epoch uint64
}

// QUICSocket adapts one authenticated QUIC connection to hostSocket.
//
// Two control transports exist:
//   - stream control (default): the client opens one bidirectional
//     stream; text messages ride it (used by quic-go-to-quic-go peers and
//     tests that can open streams).
//   - control-over-datagram (NewQUICSocketDatagramControl): text messages
//     are framed as [0x02][JSON] datagrams; there is NO AcceptStream. This
//     is the production interop mode with Apple's Network.framework, whose
//     classic API cannot multiplex a stream and a datagram channel onto a
//     single client QUIC session (verified by probe: two NWConnections =
//     two sessions; NWMultiplexGroup extract fails). Auth/ready/revoke
//     reliability is app-level: the client retries auth; the server
//     re-sends ready on duplicate auth (see the quic package handshake).
//
// Lifecycle: NewQUICSocket starts ingress immediately. The media pump
// calls ReadMessage exactly like the WS pump. Close (or Fail on transport
// death) unblocks everything and is idempotent. After Close all reads
// return ErrQUICSocketClosed and all writes fail: a superseded socket can
// never deliver into the session.
type QUICSocket struct {
	conn        QUICConnection
	epochSource func() uint64 // session primary-role epoch (0 = not attached yet)
	// audioOnly is the production Apple-Network.framework mode (build 61):
	// the session carries ONE datagram flow (WWDC22-documented) with audio
	// envelopes ([0x01]) and RTT echo probes ([0x04]) only — ALL critical
	// control (auth/bind, ready, promote, denoise, bye) travels on the
	// existing authenticated HTTPS/WSS plane bound to this socket by
	// call/device/epoch (NWMultiplexGroup datagram-flow extraction has a
	// reported platform limitation — socket repo issue 173 — so no stream
	// multiplexing is attempted). WriteMessage(text) fails in this mode;
	// the runtime delivers ready/promoted over the WSS incumbent.
	audioOnly bool

	inbound chan quicInboundMsg

	startOnce sync.Once
	started   chan struct{} // closed when control accept resolved
	startErr  error         // sticky, guarded by startMu
	startMu   sync.Mutex

	mu            sync.Mutex
	writeDeadline time.Time
	closed        bool
	control       QUICStream
	doneCh        chan struct{}
	closeOnce     sync.Once

	droppedStaleEpoch atomic.Int64
	droppedOverflow   atomic.Int64
	droppedOversize   atomic.Int64
	droppedDequeue    atomic.Int64
	droppedEmpty      atomic.Int64
	rttEchoes         atomic.Int64
}

// NewQUICSocket creates the adapter and starts ingress (accepting the
// control stream itself). epochSource must return the CURRENT primary role
// epoch of the owning session (bumped on attach/promote); while it returns
// 0 every inbound datagram is dropped (nothing is accepted before
// authorization/adoption and the current epoch is known).
func NewQUICSocket(ctx context.Context, conn QUICConnection, epochSource func() uint64) *QUICSocket {
	return NewQUICSocketWithControl(ctx, conn, nil, epochSource)
}

// NewQUICAudioSocket is the production Apple-Network.framework mode
// (build 61): ONE datagram flow carrying audio envelopes and RTT probes
// only; all critical control uses the existing authenticated WSS plane.
// No stream is accepted and text writes fail by design.
func NewQUICAudioSocket(ctx context.Context, conn QUICConnection, epochSource func() uint64) *QUICSocket {
	q := newQUICSocket(conn, epochSource)
	q.audioOnly = true
	close(q.started) // nothing to accept
	go q.datagramReader()
	return q
}

func newQUICSocket(conn QUICConnection, epochSource func() uint64) *QUICSocket {
	return &QUICSocket{
		conn:        conn,
		epochSource: epochSource,
		inbound:     make(chan quicInboundMsg, quicInboundQueue),
		started:     make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
}

// NewQUICSocketWithControl is the production constructor when the caller
// already performed the authenticated handshake on the control stream: the
// stream is adopted verbatim (auth, ready, ping/pong, promote and bye all
// ride this ONE stream — there is never a second AcceptStream wait), and
// any handshake-era read deadline on it must already have been cleared by
// the caller.
func NewQUICSocketWithControl(ctx context.Context, conn QUICConnection, control QUICStream, epochSource func() uint64) *QUICSocket {
	q := newQUICSocket(conn, epochSource)
	if control != nil {
		q.startMu.Lock()
		q.startErr = nil
		q.startMu.Unlock()
		q.mu.Lock()
		q.control = control
		q.mu.Unlock()
		close(q.started)
		go q.controlReader(control)
		go q.datagramReader()
		return q
	}
	go q.start(ctx)
	return q
}

// DroppedDatagrams reports drop counters for diagnostics/tests.
func (q *QUICSocket) DroppedDatagrams() (staleEpoch, overflow, oversize int64) {
	return q.droppedStaleEpoch.Load(), q.droppedOverflow.Load(), q.droppedOversize.Load()
}

// DequeueDrops reports frames dropped at dequeue (epoch relabel fence).
func (q *QUICSocket) DequeueDrops() int64 { return q.droppedDequeue.Load() }

// EmptyDrops reports zero-length datagrams (never a panic).
func (q *QUICSocket) EmptyDrops() int64 { return q.droppedEmpty.Load() }

// RTTEchoes reports RTT probe echoes answered.
func (q *QUICSocket) RTTEchoes() int64 { return q.rttEchoes.Load() }

func (q *QUICSocket) start(ctx context.Context) {
	defer close(q.started)
	stream, err := q.conn.AcceptStream(ctx)
	q.startMu.Lock()
	q.startErr = err
	q.startMu.Unlock()
	if err != nil {
		return
	}
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		_ = stream.Close()
		return
	}
	q.control = stream
	q.mu.Unlock()
	go q.controlReader(stream)
	go q.datagramReader()
}

func (q *QUICSocket) controlReader(stream QUICStream) {
	rd := bufio.NewReaderSize(stream, 4096)
	for {
		line, err := rd.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := trimLineEnding(line)
			if len(trimmed) > 0 {
				q.deliverControl(quicInboundMsg{msgType: websocket.TextMessage, payload: trimmed})
			}
		}
		if err != nil {
			// A dead control stream converges the socket: the media pump's
			// ReadMessage must observe the failure (like a WS read error).
			q.Fail(err)
			return
		}
	}
}

func (q *QUICSocket) currentEpoch() uint64 {
	if q.epochSource == nil {
		return 0
	}
	return q.epochSource()
}

func trimLineEnding(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func (q *QUICSocket) datagramReader() {
	ctx := context.Background()
	for {
		pkt, err := q.conn.ReceiveDatagram(ctx)
		if err != nil {
			q.Fail(err)
			return
		}
		if len(pkt) == 0 {
			// An empty authenticated datagram is garbage, not a crash.
			q.droppedEmpty.Add(1)
			continue
		}
		if len(pkt) > quicMaxAudioDatagram {
			q.droppedOversize.Add(1)
			continue
		}
		if q.audioOnly && pkt[0] == QUICBindVersion {
			// Post-bind retransmits are ignored (token already consumed);
			// pre-bind binds never reach the adapter (the server consumes
			// them before construction).
			continue
		}
		if q.audioOnly && pkt[0] == QUICProbeVersion {
			if err := q.conn.SendDatagram(pkt); err != nil {
				q.Fail(err)
				return
			}
			q.rttEchoes.Add(1)
			continue
		}
		epoch := uint64(0)
		if q.epochSource != nil {
			epoch = q.epochSource()
		}
		frame, ok := ParseQUICEnvelope(pkt, epoch)
		if !ok {
			// Old epoch (pre-promote/pre-attach generation), wrong version,
			// or garbage: dropped before it can reach the codec/mixer.
			q.droppedStaleEpoch.Add(1)
			continue
		}
		select {
		case q.inbound <- quicInboundMsg{msgType: websocket.BinaryMessage, payload: frame, epoch: epoch}:
		default:
			// Queue full: the NEW frame drops (fresh audio beats queued
			// audio — same stale-drop discipline as the wsHost queue).
			q.droppedOverflow.Add(1)
		}
	}
}

// deliverControl queues a control line; control is reliable and infrequent
// so it waits (bounded) rather than dropping.
func (q *QUICSocket) deliverControl(msg quicInboundMsg) {
	select {
	case q.inbound <- msg:
		return
	default:
	}
	select {
	case q.inbound <- msg:
	case <-time.After(quicControlWaitTimeout):
		q.Fail(ErrQUICSocketClosed)
	case <-q.doneCh:
	}
}

func (q *QUICSocket) ReadMessage() (int, []byte, error) {
	<-q.started
	q.startMu.Lock()
	err := q.startErr
	q.startMu.Unlock()
	if err != nil {
		return 0, nil, err
	}
	for {
		select {
		case msg := <-q.inbound:
			// Dequeue fence: a frame that waited in the queue across a
			// promotion/attach must not be relabelled into the new epoch.
			if msg.msgType == websocket.BinaryMessage {
				if msg.epoch != q.currentEpoch() {
					q.droppedDequeue.Add(1)
					continue
				}
			}
			return msg.msgType, msg.payload, nil
		case <-q.doneCh:
			return 0, nil, ErrQUICSocketClosed
		}
	}
}

func (q *QUICSocket) WriteMessage(messageType int, payload []byte) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return ErrQUICSocketClosed
	}
	if messageType == websocket.TextMessage {
		if q.audioOnly {
			q.mu.Unlock()
			return ErrQUICNoControlStream // control lives on the WSS plane
		}
		stream := q.control
		if stream == nil {
			q.mu.Unlock()
			return ErrQUICNoControlStream
		}
		deadline := q.writeDeadline
		q.mu.Unlock()
		out := make([]byte, 0, len(payload)+1)
		out = append(out, payload...)
		out = append(out, '\n')
		if !deadline.IsZero() {
			_ = stream.SetWriteDeadline(deadline)
		}
		_, err := stream.Write(out)
		if err != nil {
			q.Fail(err)
		}
		return err
	}
	// Binary: audio datagram with the epoch envelope. The epoch is read at
	// send time so a promotion restamps immediately after the session bump.
	epoch := uint64(0)
	if q.epochSource != nil {
		epoch = q.epochSource()
	}
	q.mu.Unlock()
	if len(payload)+quicEnvelopeHeader > quicMaxAudioDatogramGuard() {
		q.droppedOversize.Add(1)
		return nil // oversize frame dropped, socket stays healthy
	}
	return q.conn.SendDatagram(StampQUICEnvelope(payload, epoch))
}

func quicMaxAudioDatogramGuard() int { return quicMaxAudioDatagram }

func (q *QUICSocket) SetWriteDeadline(t time.Time) error {
	q.mu.Lock()
	q.writeDeadline = t
	q.mu.Unlock()
	return nil
}

// markClosed closes the done channel exactly once; called by both Close
// and Fail.
func (q *QUICSocket) markClosed() {
	q.closeOnce.Do(func() { close(q.doneCh) })
}

// Close converges the socket: pumps unblock, readers get
// ErrQUICSocketClosed and the host detaches exactly like the WS path.
// Idempotent.
func (q *QUICSocket) Close() error {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.markClosed()
	_ = q.conn.CloseWithError(0, "media socket closed")
	return nil
}

// Fail marks the socket dead because the underlying connection died (peer
// gone, transport error). Unblocks readers so the pump detaches with the
// recoverable host-lost lifecycle.
func (q *QUICSocket) Fail(err error) {
	if err == nil {
		err = ErrQUICSocketClosed
	}
	slog.Debug("quic media socket failed", "remote", q.conn.RemoteAddr(), "error", err)
	_ = q.conn.CloseWithError(0, "media socket failed")
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.markClosed()
}

var _ hostSocket = (*QUICSocket)(nil)
