package quic

import (
	"log/slog"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// demuxConn multiplexes STUN and QUIC traffic on one UDP socket so the
// server-reflexive candidate names the exact port the QUIC listener
// answers on. A single reader goroutine classifies each inbound packet:
// STUN messages (top two bits of byte 0 are 00 and the message looks like
// RFC 8489) are routed to the serialized STUN client; everything else is
// queued for quic-go's Transport. Unknown sources can never inject into
// the STUN transaction state: responses are matched by transaction ID AND
// source address, and requests from non-STUN sources are ignored.
type demuxConn struct {
	udp  *net.UDPConn
	stun *stunClient

	quicQueue chan demuxPacket

	closed    chan struct{}
	closeOnce sync.Once

	// readDeadline lets quic-go's Transport.Close unblock the listen loop
	// (it sets a past read deadline on the provided PacketConn and waits
	// for the loop to exit). Deadline changes signal the waiter.
	readDeadlineMu sync.Mutex
	readDeadline   time.Time
	readDeadlineCh chan struct{}

	droppedNonQUIC atomic.Int64
	droppedQueue   atomic.Int64
}

type demuxPacket struct {
	b    []byte
	addr net.Addr
}

func newDemuxConn(udp *net.UDPConn, stun *stunClient) *demuxConn {
	return &demuxConn{
		udp:            udp,
		stun:           stun,
		quicQueue:      make(chan demuxPacket, 512),
		closed:         make(chan struct{}),
		readDeadlineCh: make(chan struct{}),
	}
}

// looksLikeSTUN reports whether pkt is plausibly an RFC 8489 STUN message:
// the first two bits must be 00 and the message length must be consistent
// with the datagram size.
func looksLikeSTUN(pkt []byte) bool {
	if len(pkt) < 20 {
		return false
	}
	if pkt[0]&0xC0 != 0 {
		return false
	}
	declared := int(pkt[2])<<8 | int(pkt[3])
	return declared+20 <= len(pkt) && declared+20 >= 20
}

func (d *demuxConn) loop() {
	buf := make([]byte, 64*1024)
	for {
		n, addr, err := d.udp.ReadFrom(buf)
		if err != nil {
			d.closeOnce.Do(func() { close(d.closed) })
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		if looksLikeSTUN(pkt) && d.stun.maybeHandle(pkt, addr) {
			continue
		}
		select {
		case d.quicQueue <- demuxPacket{b: pkt, addr: addr}:
		default:
			// A flooded garbage source must never wedge the QUIC path:
			// drop the packet (quic-go's own loss handling covers it).
			d.droppedQueue.Add(1)
		}
	}
}

func (d *demuxConn) ReadFrom(p []byte) (int, net.Addr, error) {
	d.readDeadlineMu.Lock()
	deadline := d.readDeadline
	deadlineCh := d.readDeadlineCh
	d.readDeadlineMu.Unlock()
	if !deadline.IsZero() {
		remain := time.Until(deadline)
		if remain <= 0 {
			return 0, nil, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(remain)
		defer timer.Stop()
		select {
		case pkt := <-d.quicQueue:
			n := copy(p, pkt.b)
			return n, pkt.addr, nil
		case <-deadlineCh:
			return 0, nil, os.ErrDeadlineExceeded
		case <-timer.C:
			return 0, nil, os.ErrDeadlineExceeded
		case <-d.closed:
			return 0, nil, net.ErrClosed
		}
	}
	select {
	case pkt := <-d.quicQueue:
		n := copy(p, pkt.b)
		return n, pkt.addr, nil
	case <-deadlineCh:
		return 0, nil, os.ErrDeadlineExceeded
	case <-d.closed:
		return 0, nil, net.ErrClosed
	}
}

func (d *demuxConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return d.udp.WriteTo(p, addr)
}

func (d *demuxConn) Close() error {
	d.closeOnce.Do(func() { close(d.closed) })
	return d.udp.Close()
}

func (d *demuxConn) LocalAddr() net.Addr                { return d.udp.LocalAddr() }
func (d *demuxConn) SetDeadline(t time.Time) error { return d.SetReadDeadline(t) }

func (d *demuxConn) SetReadDeadline(t time.Time) error {
	d.readDeadlineMu.Lock()
	d.readDeadline = t
	close(d.readDeadlineCh)
	d.readDeadlineCh = make(chan struct{})
	d.readDeadlineMu.Unlock()
	return nil
}

func (d *demuxConn) SetWriteDeadline(t time.Time) error { return nil }

func (d *demuxConn) logDrops() {
	slog.Debug("quic demux state", "dropped_nonquic", d.droppedNonQUIC.Load(), "dropped_queue", d.droppedQueue.Load())
}
