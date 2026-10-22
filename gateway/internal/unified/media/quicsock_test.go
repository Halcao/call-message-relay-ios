package media

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeQUICStream is an in-memory duplex stream (io.Pipe pair).
type fakeQUICStream struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	mu     sync.Mutex
	closed bool
}

func newFakeQUICStreamPair() (a, b *fakeQUICStream) {
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	return &fakeQUICStream{r: r1, w: w2}, &fakeQUICStream{r: r2, w: w1}
}

func (f *fakeQUICStream) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f *fakeQUICStream) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f *fakeQUICStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	_ = f.r.Close()
	_ = f.w.Close()
	return nil
}
func (f *fakeQUICStream) SetReadDeadline(time.Time) error  { return nil }
func (f *fakeQUICStream) SetWriteDeadline(time.Time) error { return nil }

// fakeQUICConn models the gateway-side connection: one control stream +
// datagram channels, with injectable datagram delivery for tests.
type fakeQUICConn struct {
	stream      *fakeQUICStream
	datagrams   chan []byte // inbound datagrams TO the socket (from "client")
	sentDgrams  [][]byte
	recvCount   int
	mu          sync.Mutex
	closed      bool
	closeErrMsg string
}

func newFakeQUICConn() *fakeQUICConn {
	return &fakeQUICConn{
		stream:    newFakeQUICStreamPairClient(),
		datagrams: make(chan []byte, 64),
	}
}

func (c *fakeQUICConn) AcceptStream(ctx context.Context) (QUICStream, error) {
	return c.stream, nil
}
func (c *fakeQUICConn) SendDatagram(p []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("closed")
	}
	c.sentDgrams = append(c.sentDgrams, append([]byte(nil), p...))
	return nil
}
func (c *fakeQUICConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case d := <-c.datagrams:
		c.mu.Lock()
		c.recvCount++
		c.mu.Unlock()
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *fakeQUICConn) CloseWithError(code uint64, msg string) error {
	c.mu.Lock()
	c.closed = true
	c.closeErrMsg = msg
	c.mu.Unlock()
	return c.stream.Close()
}
func (c *fakeQUICConn) RemoteAddr() net.Addr { return fakeAddr("quic.test:1") }

type fakeAddr string

func (f fakeAddr) Network() string { return "quic" }
func (f fakeAddr) String() string  { return string(f) }

func TestQUICEnvelopeRoundTrip(t *testing.T) {
	frame := []byte{0x12, 0x34, 'o', 'p', 'u', 's'}
	pkt := StampQUICEnvelope(frame, 42)
	got, ok := ParseQUICEnvelope(pkt, 42)
	if !ok {
		t.Fatal("valid envelope rejected")
	}
	if string(got) != string(frame) {
		t.Fatalf("payload = %v, want %v", got, frame)
	}
	// Wrong epoch (stale generation) must drop.
	if _, ok := ParseQUICEnvelope(pkt, 43); ok {
		t.Fatal("stale-epoch envelope accepted")
	}
	// Wrong version must drop.
	pkt[0] = 0x02
	if _, ok := ParseQUICEnvelope(pkt, 42); ok {
		t.Fatal("wrong-version envelope accepted")
	}
	// Truncated must drop.
	if _, ok := ParseQUICEnvelope(pkt[:4], 42); ok {
		t.Fatal("truncated envelope accepted")
	}
}

// envelopeEpochGate exercises the critical review race: datagrams from an
// old epoch (pre-promote / pre-attach / superseded connection) must be
// dropped by the adapter, never reaching ReadMessage.
func TestQUICSocketEpochGate(t *testing.T) {
	conn := newFakeQUICConn()
	var epochHolder fakeEpochSource
	qs := NewQUICSocket(context.Background(), conn, epochHolder.Epoch)
	defer qs.Close()
	// Client end of the control stream (so AcceptStream's peer exists).
	client := pairClientOf(conn.stream)
	_ = client

	waitStarted(t, qs)

	// Pre-attach: epoch 0 → every datagram dropped.
	conn.datagrams <- StampQUICEnvelope([]byte("frame-a"), 7)
	waitDropped(t, qs, 1) // processed while epoch still 0

	epochHolder.set(9)
	conn.datagrams <- StampQUICEnvelope([]byte("frame-ok"), 9)
	mt, payload, err := qs.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.BinaryMessage || string(payload) != "frame-ok" {
		t.Fatalf("first frame = %d %q, want binary frame-ok", mt, payload)
	}

	// Stale generation arriving AFTER the epoch moved on (promoted).
	conn.datagrams <- StampQUICEnvelope([]byte("frame-stale"), 7)
	waitDropped(t, qs, 2)

	epochHolder.set(10)
	conn.datagrams <- StampQUICEnvelope([]byte("frame-new"), 10)
	mt, payload, err = qs.ReadMessage()
	if err != nil {
		t.Fatalf("read2: %v", err)
	}
	if string(payload) != "frame-new" {
		t.Fatalf("second frame = %q, want frame-new", payload)
	}
	stale, _, _ := qs.DroppedDatagrams()
	if stale != 2 {
		t.Fatalf("dropped stale = %d, want 2 (pre-attach + old-epoch)", stale)
	}
}

func TestQUICSocketControlLinesAndClose(t *testing.T) {
	conn := newFakeQUICConn()
	var epochHolder fakeEpochSource
	epochHolder.set(3)
	qs := NewQUICSocket(context.Background(), conn, epochHolder.Epoch)
	waitStarted(t, qs)
	client := pairClientOf(conn.stream)

	// Client writes two JSON lines; socket must deliver both in order.
	go func() {
		_, _ = client.Write([]byte(`{"type":"ping","t":1}` + "\n"))
		_, _ = client.Write([]byte(`{"type":"bye"}` + "\n"))
	}()
	mt, payload, err := qs.ReadMessage()
	if err != nil || mt != websocket.TextMessage || string(payload) != `{"type":"ping","t":1}` {
		t.Fatalf("control1 = %d %q %v", mt, payload, err)
	}
	_, payload, err = qs.ReadMessage()
	if err != nil || string(payload) != `{"type":"bye"}` {
		t.Fatalf("control2 = %q %v", payload, err)
	}

	// Downlink audio is envelope-stamped with the CURRENT epoch.
	if err := qs.WriteMessage(websocket.BinaryMessage, []byte{0, 1, 2}); err != nil {
		t.Fatalf("write: %v", err)
	}
	conn.mu.Lock()
	if len(conn.sentDgrams) != 1 {
		t.Fatalf("sent datagrams = %d", len(conn.sentDgrams))
	}
	frame, ok := ParseQUICEnvelope(conn.sentDgrams[0], 3)
	conn.mu.Unlock()
	if !ok || len(frame) != 3 {
		t.Fatalf("downlink not stamped with current epoch: ok=%v", ok)
	}

	// Close: reads fail, writes fail, idempotent.
	_ = qs.Close()
	_ = qs.Close()
	if _, _, err := qs.ReadMessage(); !errors.Is(err, ErrQUICSocketClosed) {
		t.Fatalf("read after close = %v", err)
	}
	if err := qs.WriteMessage(websocket.TextMessage, []byte("x")); !errors.Is(err, ErrQUICSocketClosed) {
		t.Fatalf("write after close = %v", err)
	}
}

func waitDropped(t *testing.T, qs *QUICSocket, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stale, _, _ := qs.DroppedDatagrams(); stale >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	stale, _, _ := qs.DroppedDatagrams()
	t.Fatalf("dropped stale = %d, want >= %d", stale, want)
}

func waitStarted(t *testing.T, qs *QUICSocket) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case <-qs.started:
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("socket did not start")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// fakeEpochSource is a settable epoch source for tests.
type fakeEpochSource struct {
	mu sync.Mutex
	v  uint64
}

func (f *fakeEpochSource) set(v uint64) {
	f.mu.Lock()
	f.v = v
	f.mu.Unlock()
}
func (f *fakeEpochSource) Epoch() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v
}

// pairClientOf returns the peer end of a fakeQUICStream built by
// newFakeQUICStreamPairClient. The pair helper wires both ends; the server
// side is what AcceptStream handed out, so the client end is captured at
// construction via the package-level registry below (tests are
// single-threaded per pair).
var fakeStreamRegistry sync.Map // *fakeQUICStream (server) -> *fakeQUICStream (client)

func newFakeQUICStreamPairClient() *fakeQUICStream {
	srv, client := newFakeQUICStreamPair()
	fakeStreamRegistry.Store(srv, client)
	return srv
}

func pairClientOf(srv *fakeQUICStream) *fakeQUICStream {
	v, ok := fakeStreamRegistry.Load(srv)
	if !ok {
		return nil
	}
	return v.(*fakeQUICStream)
}

// TestQUICAudioSocketModes: the production audio-only adapter ignores
// post-bind binds, echoes RTT probes, survives empty datagrams, refuses
// text writes, and re-fences queued audio at dequeue.
func TestQUICAudioSocketModes(t *testing.T) {
	conn := newFakeQUICConn()
	var epochHolder fakeEpochSource
	epochHolder.set(5)
	qs := NewQUICAudioSocket(context.Background(), conn, epochHolder.Epoch)
	defer qs.Close()
	_ = pairClientOf(conn.stream)

	// Text writes fail by design (control lives on the WSS plane).
	if err := qs.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping"}`)); !errors.Is(err, ErrQUICNoControlStream) {
		t.Fatalf("text write in audio-only mode = %v", err)
	}

	// Bind frames post-handshake are ignored, not queued.
	conn.datagrams <- append([]byte{QUICBindVersion}, []byte("token")...)
	// Empty datagram: counted, no panic.
	conn.datagrams <- []byte{}
	// RTT probe: echoed verbatim.
	probe := make([]byte, 9)
	probe[0] = QUICProbeVersion
	probe[8] = 0x7A
	conn.datagrams <- probe

	// Wait for the echo to prove processing reached the probe.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn.mu.Lock()
		n := len(conn.sentDgrams)
		conn.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	conn.mu.Lock()
	if len(conn.sentDgrams) != 1 || conn.sentDgrams[0][0] != QUICProbeVersion || conn.sentDgrams[0][8] != 0x7A {
		t.Fatalf("probe echo wrong: %x", conn.sentDgrams)
	}
	conn.mu.Unlock()
	if qs.EmptyDrops() != 1 {
		t.Fatalf("empty drops = %d, want 1", qs.EmptyDrops())
	}

	// Dequeue fence: enqueue audio at epoch 5, advance epoch before the
	// pump reads it — the frame must drop at dequeue, never be returned.
	conn.datagrams <- StampQUICEnvelope([]byte("old-frame"), 5)
	time.Sleep(50 * time.Millisecond) // let it queue
	epochHolder.set(6)
	readDone := make(chan error, 1)
	go func() {
		_, _, err := qs.ReadMessage()
		readDone <- err
	}()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if qs.DequeueDrops() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if qs.DequeueDrops() < 1 {
		t.Fatalf("dequeue drops = %d, want >= 1", qs.DequeueDrops())
	}
	_ = qs.Close()
	if err := <-readDone; !errors.Is(err, ErrQUICSocketClosed) {
		t.Fatalf("read after dequeue-drop = %v", err)
	}
}

// TestQUICAudioSocketAudioFlow: current-epoch audio flows; text control
// never appears.
func TestQUICAudioSocketAudioFlow(t *testing.T) {
	conn := newFakeQUICConn()
	var epochHolder fakeEpochSource
	epochHolder.set(4)
	qs := NewQUICAudioSocket(context.Background(), conn, epochHolder.Epoch)
	defer qs.Close()

	conn.datagrams <- StampQUICEnvelope([]byte("frame-1"), 4)
	mt, payload, err := qs.ReadMessage()
	if err != nil || mt != websocket.BinaryMessage || string(payload) != "frame-1" {
		t.Fatalf("audio = %d %q %v", mt, payload, err)
	}
	// Close unblocks readers with ErrQUICSocketClosed.
	_ = qs.Close()
	if _, _, err := qs.ReadMessage(); !errors.Is(err, ErrQUICSocketClosed) {
		t.Fatalf("read after close = %v", err)
	}
}
