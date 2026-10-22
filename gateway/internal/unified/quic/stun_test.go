package quic

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

// Real-UDP tests for the serialized STUN binding client. These run the
// production demux path (demuxConn.loop -> stunClient.maybeHandle) against a
// fixture RFC 8489 server on loopback, proving:
//   - a genuine response is consumed while the query waits (no mu deadlock);
//   - wrong-source and wrong-transaction replies are ignored, never consumed;
//   - cancellation and timeout retire the pending state cleanly and a late
//     response then falls through to the QUIC queue (demux never stalls);
//   - concurrent callers are serialized and race-free (run with -race).

// runSTUNServer starts a fixture STUN server. handle decodes each request
// and returns the response bytes to send (nil = stay silent); fromOverride
// sends the response from a different source address (wrong-source tests).
// The handler also receives the server's own socket for multi-reply scripts.
func runSTUNServer(t *testing.T, handle func(req *stun.Message, from *net.UDPAddr, self *net.UDPConn) (resp []byte, fromOverride *net.UDPConn)) (addr string, requests *atomic.Int64, stop func()) {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	var count atomic.Int64
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, from, err := udp.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if err := req.Decode(); err != nil {
				continue
			}
			count.Add(1)
			resp, override := handle(req, from, udp)
			if resp == nil {
				continue
			}
			out := udp
			if override != nil {
				out = override
			}
			_, _ = out.WriteToUDP(resp, from)
		}
	}()
	return udp.LocalAddr().String(), &count, func() { udp.Close() }
}

func buildBindingResponse(t *testing.T, req *stun.Message, mappedIP net.IP, mappedPort int) []byte {
	t.Helper()
	resp, err := stun.Build(
		stun.NewType(stun.MethodBinding, stun.ClassSuccessResponse),
		stun.NewTransactionIDSetter(req.TransactionID),
		&stun.XORMappedAddress{IP: mappedIP, Port: mappedPort},
	)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Raw
}

// newDemuxSTUNPair wires a stunClient through the production demux loop, the
// exact path the server uses.
func newDemuxSTUNPair(t *testing.T) (*stunClient, *demuxConn) {
	t.Helper()
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	client := newSTUNClient(udp)
	demux := newDemuxConn(udp, client)
	go demux.loop()
	t.Cleanup(func() { udp.Close() })
	return client, demux
}

func mustMapped(t *testing.T, got string, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.0.2.1:54321" {
		t.Fatalf("mapped = %q, want 192.0.2.1:54321", got)
	}
}

// With the pre-fix code (mu held across the network wait) this test deadlocks
// and hits the context deadline: maybeHandle can never publish.
func TestSTUNQuerySuccessThroughDemux(t *testing.T) {
	client, _ := newDemuxSTUNPair(t)
	addr, requests, stop := runSTUNServer(t, func(req *stun.Message, from *net.UDPAddr, self *net.UDPConn) ([]byte, *net.UDPConn) {
		return buildBindingResponse(t, req, net.ParseIP("192.0.2.1"), 54321), nil
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ap, err := client.query(ctx, addr)
	mustMapped(t, ap.String(), err)
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want exactly 1 (first attempt succeeds)", requests.Load())
	}
}

func TestSTUNQueryIgnoresWrongSource(t *testing.T) {
	client, demux := newDemuxSTUNPair(t)
	// The wrong-source reply comes from a second socket.
	wrong, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()

	addr, _, stop := runSTUNServer(t, func(req *stun.Message, from *net.UDPAddr, self *net.UDPConn) ([]byte, *net.UDPConn) {
		go func() {
			resp := buildBindingResponse(t, req, net.ParseIP("192.0.2.1"), 54321)
			time.Sleep(50 * time.Millisecond)
			_, _ = wrong.WriteToUDP(resp, from) // right tx, WRONG source
			time.Sleep(300 * time.Millisecond)
			_, _ = self.WriteToUDP(resp, from)  // right tx, right source
		}()
		return nil, nil
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type result struct {
		ap  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		ap, err := client.query(ctx, addr)
		done <- result{ap: ap.String(), err: err}
	}()

	// The wrong-source response must NOT satisfy the pending query.
	select {
	case r := <-done:
		t.Fatalf("query completed from the wrong source: %v %v", r.ap, r.err)
	case <-time.After(200 * time.Millisecond):
	}

	res := <-done
	mustMapped(t, res.ap, res.err)

	// The ignored wrong-source packet fell through to the QUIC queue (demux
	// honesty: the loop was never stalled behind the lock).
	_ = demux.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 2048)
	n, from, err := demux.ReadFrom(buf)
	if err != nil {
		t.Fatalf("wrong-source packet must fall through to the QUIC queue: %v", err)
	}
	if from.String() == addr {
		t.Fatalf("fall-through packet came from the server itself: %v", from)
	}
	if n < 20 {
		t.Fatalf("fall-through packet too small to be the STUN response: %d", n)
	}
}

func TestSTUNQueryIgnoresWrongTransactionID(t *testing.T) {
	client, demux := newDemuxSTUNPair(t)
	// The server answers the request twice from the CORRECT source: first a
	// response whose transaction ID is corrupted (must be rejected by the
	// txID check), then the genuine one.
	addr, _, stop := runSTUNServer(t, func(req *stun.Message, from *net.UDPAddr, self *net.UDPConn) ([]byte, *net.UDPConn) {
		go func() {
			bad := buildBindingResponse(t, req, net.ParseIP("192.0.2.1"), 54321)
			bad[8] ^= 0xFF // transaction ID no longer matches
			good := buildBindingResponse(t, req, net.ParseIP("192.0.2.1"), 54321)
			time.Sleep(50 * time.Millisecond)
			_, _ = self.WriteToUDP(bad, from)
			time.Sleep(300 * time.Millisecond)
			_, _ = self.WriteToUDP(good, from)
		}()
		return nil, nil
	})
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type result struct {
		ap  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		ap, err := client.query(ctx, addr)
		done <- result{ap: ap.String(), err: err}
	}()
	select {
	case r := <-done:
		t.Fatalf("query completed from a corrupted transaction: %v %v", r.ap, r.err)
	case <-time.After(200 * time.Millisecond):
	}
	res := <-done
	mustMapped(t, res.ap, res.err)

	// The corrupted response fell through to the QUIC queue.
	_ = demux.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := demux.ReadFrom(make([]byte, 2048)); err != nil {
		t.Fatalf("corrupted-tx response must fall through to the QUIC queue: %v", err)
	}
}

func TestSTUNQueryCancellationRetiresState(t *testing.T) {
	client, demux := newDemuxSTUNPair(t)
	addr, _, stop := runSTUNServer(t, func(req *stun.Message, from *net.UDPAddr, self *net.UDPConn) ([]byte, *net.UDPConn) {
		return nil, nil // silent
	})
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := client.query(ctx, addr)
		done <- result{err: err}
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case r := <-done:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled query did not return")
	}

	// A late genuine response must NOT be consumed after cancellation: it
	// falls through to the QUIC queue, proving the pending state retired.
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	// Reconstruct a valid response: any txID works — the pending table is
	// empty so maybeHandle must reject it BEFORE the txID check.
	var tx [12]byte
	tx[0] = 1
	resp, err := stun.Build(
		stun.NewType(stun.MethodBinding, stun.ClassSuccessResponse),
		stun.NewTransactionIDSetter(tx),
		&stun.XORMappedAddress{IP: net.ParseIP("192.0.2.1"), Port: 54321},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.WriteTo(resp.Raw, mustUDPAddr(t, demux.LocalAddr())); err != nil {
		t.Fatal(err)
	}
	_ = demux.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := demux.ReadFrom(make([]byte, 2048)); err != nil {
		t.Fatalf("late post-cancel response must fall through to the QUIC queue: %v", err)
	}
}

func TestSTUNQueryTimeoutRetriesAndExpires(t *testing.T) {
	client, _ := newDemuxSTUNPair(t)
	addr, requests, stop := runSTUNServer(t, func(req *stun.Message, from *net.UDPAddr, self *net.UDPConn) ([]byte, *net.UDPConn) {
		return nil, nil // silent
	})
	defer stop()

	// DefaultSTUNTimeout is a production const (3 s): the query retries
	// once, so the honest expiry lands after ~2x that. Bounded, no fake
	// clocks.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := client.query(ctx, addr)
	if !errors.Is(err, ErrSTUNNoResponse) {
		t.Fatalf("err = %v, want ErrSTUNNoResponse", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d, want 2 (one retry)", requests.Load())
	}
	if elapsed := time.Since(start); elapsed < 2*DefaultSTUNTimeout {
		t.Fatalf("timeout path returned after %v, want both attempts to expire", elapsed)
	}
}

func TestSTUNConcurrentQueriesRaceFree(t *testing.T) {
	client, _ := newDemuxSTUNPair(t)
	addr, requests, stop := runSTUNServer(t, func(req *stun.Message, from *net.UDPAddr, self *net.UDPConn) ([]byte, *net.UDPConn) {
		return buildBindingResponse(t, req, net.ParseIP("192.0.2.1"), 54321), nil
	})
	defer stop()

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	aps := make([]string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ap, err := client.query(ctx, addr)
			errs[i] = err
			aps[i] = ap.String()
		}(i)
	}
	wg.Wait()
	for i := range callers {
		mustMapped(t, aps[i], errs[i])
	}
	if requests.Load() != callers {
		t.Fatalf("requests = %d, want %d (each serialized query transacts once)", requests.Load(), callers)
	}
}

func mustUDPAddr(t *testing.T, addr net.Addr) *net.UDPAddr {
	t.Helper()
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		t.Fatalf("addr %v is not UDP", addr)
	}
	return ua
}
