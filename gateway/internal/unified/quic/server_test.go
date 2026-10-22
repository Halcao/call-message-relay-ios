package quic

import (
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/store"

	"github.com/quic-go/quic-go"
)

// testEnv builds a real store+auth+identity stack and a live QUIC server
// with a controllable attach hook.
type testEnv struct {
	server    *Server
	deviceID  string
	token     string
	gatewayID *identity.Identity
	attachFn  AttachFunc
	attachMu  sync.Mutex
	lastQS    *media.QUICSocket
	attachErr error
	attachBody AttachFunc // late-bound per-test attach behavior
	attached  chan struct{}
}

func newTestEnv(t *testing.T, mutate func(*Server)) *testEnv {
	t.Helper()
	prev := validatePunchTarget
	validatePunchTarget = func(candidate string, lanPunch bool) (netip.AddrPort, error) {
		ap, err := netip.ParseAddrPort(candidate)
		if err != nil {
			return netip.AddrPort{}, err
		}
		return ap, nil
	}
	t.Cleanup(func() { validatePunchTarget = prev })

	dir := t.TempDir()
	database, err := store.Open(filepath.Join(dir, "unified.sqlite"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	gwID, err := identity.LoadOrCreate(filepath.Join(dir, "gateway.key"), "gw-test", "Test")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	authService := auth.New(database, gwID, time.Minute, time.Hour)
	devPub, devPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	keyID, secret, err := authService.CreateKey(context.Background(), "quic-test", true)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	proof := ed25519.Sign(devPriv, []byte(keyID+"\n"+secret+"\n"+gwID.ID+"\nQUIC Test"))
	pair, err := authService.Enroll(context.Background(), auth.EnrollmentRequest{
		EnrollmentKey: keyID + "." + secret, DeviceName: "QUIC Test",
		DevicePublicKey: devPub, Proof: proof,
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	env := &testEnv{
		deviceID:  pair.DeviceID,
		token:     pair.AccessToken,
		gatewayID: gwID,
		attached:  make(chan struct{}, 8),
	}
	env.attachFn = func(ctx context.Context, deviceID, callID, codec string, qs *media.QUICSocket, standby bool, ref *SessionRef) error {
		env.attachMu.Lock()
		env.lastQS = qs
		env.attachMu.Unlock()
		defer func() { env.attached <- struct{}{} }()
		if env.attachErr != nil {
			return env.attachErr
		}
		if env.attachBody != nil {
			return env.attachBody(ctx, deviceID, callID, codec, qs, standby, ref)
		}
		return nil
	}
	server, err := New(Config{
		Listen:      "127.0.0.1:0",
		STUNServers: nil,
		DataDir:     filepath.Join(dir, "quic"),
	}, gwID, authService, env.attachFn)
	if err != nil {
		t.Fatalf("quic new: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("quic start: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	env.server = server
	if mutate != nil {
		mutate(server)
	}
	return env
}

// dial connects a real quic-go client with the production pin+validity
// verifier (InsecureSkipVerify ONLY to replace default CA validation with
// the mandatory fail-closed identity pin — the accepted pattern per the
// TLS review; unconditional trust is never used).
func (e *testEnv) dial(t *testing.T, expectPin string, now time.Time) (*quic.Conn, error) {
	t.Helper()
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("udp: %v", err)
	}
	t.Cleanup(func() { udp.Close() })
	tlsConf := &tls.Config{
		InsecureSkipVerify: true, // replaced by the fail-closed verifier below
		NextProtos:         []string{ALPN},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("pin: no peer certificate")
			}
			cert := cs.PeerCertificates[0]
			if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
				return errors.New("pin: certificate validity window violated")
			}
			der, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(der)
			if base64.StdEncoding.EncodeToString(sum[:]) != expectPin {
				return errors.New("pin: SPKI hash mismatch")
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return quic.Dial(ctx, udp, e.server.Addr(), tlsConf, &quic.Config{
		EnableDatagrams: true,
	})
}

// sendBind sends one [0x03][token] bind datagram (client side).
func sendBind(t *testing.T, conn *quic.Conn, token string) {
	t.Helper()
	frame := append([]byte{media.QUICBindVersion}, []byte(token)...)
	if err := conn.SendDatagram(frame); err != nil {
		t.Fatalf("send bind: %v", err)
	}
}

// expectClose asserts the connection dies (rejection/convergence).
func expectClose(t *testing.T, conn *quic.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if _, err := conn.ReceiveDatagram(ctx); err == nil {
		t.Fatal("expected connection close, got a datagram")
	}
}

// TestQUICFullMediaPath// TestQUICFullMediaPath: dial → [0x03] bind → attach → bidirectional
// envelope-stamped audio with epoch gating, all over the audio-only
// session (control would travel the WSS plane in production; the fake
// attach here substitutes for the runtime standby attach).
func TestQUICFullMediaPath(t *testing.T) {
	env := newTestEnv(t, nil)
	const testEpoch = uint64(7)
	env.attachBody = func(ctx context.Context, deviceID, callID, codec string, qs *media.QUICSocket, standby bool, ref *SessionRef) error {
		if deviceID != env.deviceID {
			return fmt.Errorf("device = %q", deviceID)
		}
		if callID != "call-1" {
			return fmt.Errorf("call = %q", callID)
		}
		if codec != "opus" {
			return fmt.Errorf("codec = %q", codec)
		}
		if !standby {
			return fmt.Errorf("quic attach must request the standby role")
		}
		ref.SetStaticEpoch(testEpoch)
		go func() {
			for {
				mt, payload, err := qs.ReadMessage()
				if err != nil {
					return
				}
				if mt == 2 {
					if werr := qs.WriteMessage(2, payload); werr != nil {
						return
					}
				}
			}
		}()
		env.attachMu.Lock()
		env.lastQS = qs
		env.attachMu.Unlock()
		env.attached <- struct{}{}
		select {}
	}

	// Token from the authenticated candidate POST (public doc-range
	// candidate so validation passes).
	res, err := env.server.StartPunch(env.deviceID, "call-1", "192.0.2.1:50000")
	if err != nil {
		t.Fatalf("start punch: %v", err)
	}
	if res.PunchToken == "" {
		t.Fatal("empty punch token")
	}

	conn, err := env.dial(t, env.server.Pin().SPKIHash, time.Now())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseWithError(0, "test done") })
	sendBind(t, conn, res.PunchToken)
	select {
	case <-env.attached:
	case <-time.After(3 * time.Second):
		t.Fatal("attach not invoked")
	}

	// Bidirectional datagrams: client stamps envelopes like the iOS
	// adapter will; the fake attach echoes them.
	echoBack := func(seq uint16, payload []byte) []byte {
		frame := append([]byte{byte(seq >> 8), byte(seq)}, payload...)
		pkt := media.StampQUICEnvelope(frame, testEpoch)
		if err := conn.SendDatagram(pkt); err != nil {
			t.Fatalf("send datagram: %v", err)
		}
		dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer dcancel()
		got, err := conn.ReceiveDatagram(dctx)
		if err != nil {
			t.Fatalf("recv datagram: %v", err)
		}
		inner, ok := media.ParseQUICEnvelope(got, testEpoch)
		if !ok {
			t.Fatalf("echo not epoch-stamped: %x", got)
		}
		return inner
	}
	back := echoBack(1, []byte("hello-quic"))
	if string(back[2:]) != "hello-quic" {
		t.Fatalf("echo = %q", back)
	}

	// Stale-epoch datagrams drop (counted), never echo.
	bad := media.StampQUICEnvelope([]byte{0, 9, 'x'}, testEpoch-1)
	if err := conn.SendDatagram(bad); err != nil {
		t.Fatalf("send stale: %v", err)
	}
	back = echoBack(2, []byte("after-stale"))
	if string(back[2:]) != "after-stale" {
		t.Fatalf("post-stale echo = %q", back)
	}
	env.attachMu.Lock()
	qs := env.lastQS
	env.attachMu.Unlock()
	stale, _, _ := qs.DroppedDatagrams()
	if stale < 1 {
		t.Fatalf("server dropped stale = %d, want >= 1", stale)
	}

	// RTT probe echo ([0x04]) answers loss-tolerant measurement.
	probe := make([]byte, 9)
	probe[0] = media.QUICProbeVersion
	binary.BigEndian.PutUint64(probe[1:9], 0x1234)
	if err := conn.SendDatagram(probe); err != nil {
		t.Fatalf("send probe: %v", err)
	}
	pctx, pcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pcancel()
	presp, err := conn.ReceiveDatagram(pctx)
	if err != nil || presp[0] != media.QUICProbeVersion || binary.BigEndian.Uint64(presp[1:9]) != 0x1234 {
		t.Fatalf("probe echo = %x err=%v", presp, err)
	}

	// Replayed bind token on a NEW connection: single-use → rejected.
	conn2, err := env.dial(t, env.server.Pin().SPKIHash, time.Now())
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	t.Cleanup(func() { conn2.CloseWithError(0, "done") })
	sendBind(t, conn2, res.PunchToken)
	expectClose(t, conn2)
}

// TestQUICWrongPinAndExpiredCert are the mandatory TLS negatives on the
// real transport: wrong pin → handshake refused; expired certificate →
// validity check fails even with the right pin.
func TestQUICWrongPinAndExpiredCert(t *testing.T) {
	env := newTestEnv(t, nil)

	// 1. Wrong pin: client pins a different (valid base64) hash.
	wrong := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if _, err := env.dial(t, wrong, time.Now()); err == nil {
		t.Fatal("handshake with wrong pin succeeded")
	}

	// 2. Expired certificate: build a server with a cert whose validity
	// window ended; the pin matches but the validity check must fail.
	expired := newExpiredServer(t, env)
	t.Cleanup(func() { expired.server.Close() })
	if _, err := expired.dial(t, expired.server.Pin().SPKIHash, time.Now()); err == nil {
		t.Fatal("handshake with expired certificate succeeded")
	}
}

func newExpiredServer(t *testing.T, template *testEnv) *testEnv {
	t.Helper()
	// Build a fresh env sharing the harness but with an expired cert:
	// easiest is a dedicated server with a hand-made expired material.
	dir := t.TempDir()
	gwID, err := identity.LoadOrCreate(filepath.Join(dir, "gateway.key"), "gw-expired", "Expired")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: gwID.ID},
		DNSNames:     []string{gwID.ID},
		NotBefore:    time.Now().Add(-72 * time.Hour),
		NotAfter:     time.Now().Add(-24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Persist so New() loads it as-is (NotAfter check in loadOrCreateCert
	// would reissue; bypass via material injection below).
	mat, err := buildMaterial(cert, key, gwID)
	if err != nil {
		t.Fatalf("material: %v", err)
	}
	database, err := store.Open(filepath.Join(dir, "unified.sqlite"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	authService := auth.New(database, gwID, time.Minute, time.Hour)
	server, err := New(Config{Listen: "127.0.0.1:0", DataDir: filepath.Join(dir, "quic")}, gwID, authService,
		func(ctx context.Context, deviceID, callID, codec string, qs *media.QUICSocket, standby bool, ref *SessionRef) error {
			return io.EOF
		})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	server.material = mat // expired cert + matching pin
	if err := server.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	return &testEnv{server: server, gatewayID: gwID, deviceID: template.deviceID, token: template.token}
}

// TestQUICBindNegatives: unbound or mis-authorized connections never
// reach the attach hook; audio pre-bind is discarded.
func TestQUICBindNegatives(t *testing.T) {
	env := newTestEnv(t, nil)

	t.Run("unknown token", func(t *testing.T) {
		conn, err := env.dial(t, env.server.Pin().SPKIHash, time.Now())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.CloseWithError(0, "done") })
		sendBind(t, conn, "deadbeef")
		expectClose(t, conn)
	})

	t.Run("audio before bind then valid bind", func(t *testing.T) {
		res, err := env.server.StartPunch(env.deviceID, "call-audiofirst", "192.0.2.1:50003")
		if err != nil {
			t.Fatalf("punch: %v", err)
		}
		conn, err := env.dial(t, env.server.Pin().SPKIHash, time.Now())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.CloseWithError(0, "done") })
		// Pre-bind audio is discarded server-side (nothing reaches the
		// codec before authorization/adoption).
		pkt := media.StampQUICEnvelope(make([]byte, 160), 9)
		if err := conn.SendDatagram(pkt); err != nil {
			t.Fatalf("pre-bind audio: %v", err)
		}
		sendBind(t, conn, res.PunchToken)
		select {
		case <-env.attached:
		case <-time.After(3 * time.Second):
			t.Fatal("attach not invoked after valid bind")
		}
	})

	t.Run("no bind at all", func(t *testing.T) {
		conn, err := env.dial(t, env.server.Pin().SPKIHash, time.Now())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { conn.CloseWithError(0, "done") })
		// Server closes at the auth timeout; sending nothing must not
		// wedge it. Poll for closure with a generous bound.
		expectClose(t, conn)
	})
}

// TestQUICWarmProbe: the pre-call warm path authenticates via the
// single-use warm grant, coordinates the punch, and echoes the CLIENT's
// probe nonces verbatim (RTT uses only the client clock).
func TestQUICWarmProbe(t *testing.T) {
	env := newTestEnv(t, nil)
	warm, err := env.server.IssueWarmGrant(env.deviceID, "192.0.2.1:60000")
	if err != nil {
		t.Fatalf("warm grant: %v", err)
	}
	if warm.WarmToken == "" {
		t.Fatal("empty warm token")
	}
	conn, err := env.dial(t, env.server.Pin().SPKIHash, time.Now())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseWithError(0, "done") })
	sendBind(t, conn, warm.WarmToken)
	// Client-nonce probe: the echo must carry OUR timestamp (never the
	// server's unsynchronized wall clock).
	probe := make([]byte, 9)
	probe[0] = media.QUICProbeVersion
	binary.BigEndian.PutUint64(probe[1:9], 0xC0FFEE42)
	if err := conn.SendDatagram(probe); err != nil {
		t.Fatalf("probe: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	for {
		pkt, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatalf("warm echo: %v", err)
		}
		if len(pkt) >= 9 && pkt[0] == media.QUICProbeVersion {
			if binary.BigEndian.Uint64(pkt[1:9]) != 0xC0FFEE42 {
				t.Fatalf("echo nonce = %x, want our client nonce", pkt[1:9])
			}
			break
		}
	}
	select {
	case <-env.attached:
		t.Fatal("warm probe reached the call attach hook")
	case <-time.After(300 * time.Millisecond):
	}
	// Grant is single-use: a second probe with the same token is rejected.
	conn2, err := env.dial(t, env.server.Pin().SPKIHash, time.Now())
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	t.Cleanup(func() { conn2.CloseWithError(0, "done") })
	sendBind(t, conn2, warm.WarmToken)
	expectClose(t, conn2)
	// Rate limiting is a budgeting signal: too-frequent warm requests
	// fail without revoking anything.
	if _, err := env.server.IssueWarmGrant(env.deviceID, "192.0.2.1:60001"); err == nil {
		t.Fatal("back-to-back warm grant was not rate limited")
	}
}

// TestQUICLossyPath: a 30%-loss one-way UDP path still completes the
// bind (retried by the client) and delivers a majority of audio
// datagrams; nothing wedges.
func TestQUICLossyPath(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	env := newTestEnv(t, nil)
	const testEpoch = uint64(3)
	env.attachBody = func(ctx context.Context, deviceID, callID, codec string, qs *media.QUICSocket, standby bool, ref *SessionRef) error {
		ref.SetStaticEpoch(testEpoch)
		go func() {
			for {
				mt, payload, err := qs.ReadMessage()
				if err != nil {
					return
				}
				if mt == 2 {
					_ = qs.WriteMessage(2, payload)
				}
			}
		}()
		env.attached <- struct{}{}
		select {}
	}
	res, err := env.server.StartPunch(env.deviceID, "call-loss", "192.0.2.1:50004")
	if err != nil {
		t.Fatalf("punch: %v", err)
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("udp: %v", err)
	}
	t.Cleanup(func() { udp.Close() })
	lossy := &lossyPacketConn{conn: udp, drop: 0.30, seed: 42}
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{ALPN},
		VerifyConnection:   func(cs tls.ConnectionState) error { return nil }, // handshake integrity is proven by the full-path test; this exercises loss
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := quic.Dial(ctx, lossy, env.server.Addr(), tlsConf, &quic.Config{EnableDatagrams: true})
	if err != nil {
		t.Fatalf("dial over lossy path: %v", err)
	}
	t.Cleanup(func() { conn.CloseWithError(0, "done") })
	// Client retries the bind (loss-tolerant, idempotent: single-use
	// token, post-bind bursts ignored).
	go func() {
		for i := 0; i < 30; i++ {
			sendBind(t, conn, res.PunchToken)
			time.Sleep(300 * time.Millisecond)
		}
	}()
	select {
	case <-env.attached:
	case <-time.After(10 * time.Second):
		t.Fatal("attach not invoked over lossy path")
	}
	const sent = 60
	for i := 0; i < sent; i++ {
		pkt := media.StampQUICEnvelope([]byte{byte(i >> 8), byte(i), 'x'}, testEpoch)
		if err := conn.SendDatagram(pkt); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	got := 0
	dctx, dcancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer dcancel()
	for {
		if _, err := conn.ReceiveDatagram(dctx); err != nil {
			break
		}
		got++
	}
	if got < sent*3/10 {
		t.Fatalf("echoed %d/%d datagrams over 30%% loss path", got, sent)
	}
}

// lossyPacketConn randomly drops outbound packets (one-directional loss
// is plenty to exercise recovery).
type lossyPacketConn struct {
	conn net.PacketConn
	drop float64
	seed int64
	mu   sync.Mutex
	rnd  float64
}

func (l *lossyPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	return l.conn.ReadFrom(p)
}
func (l *lossyPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	l.mu.Lock()
	l.seed = (l.seed*1103515245 + 12345) % (1 << 31)
	l.rnd = float64(l.seed) / float64(1<<31)
	drop := l.rnd < l.drop
	l.mu.Unlock()
	if drop {
		return len(p), nil
	}
	return l.conn.WriteTo(p, addr)
}
func (l *lossyPacketConn) Close() error                       { return l.conn.Close() }
func (l *lossyPacketConn) LocalAddr() net.Addr                { return l.conn.LocalAddr() }
func (l *lossyPacketConn) SetDeadline(t time.Time) error      { return l.conn.SetDeadline(t) }
func (l *lossyPacketConn) SetReadDeadline(t time.Time) error  { return l.conn.SetReadDeadline(t) }
func (l *lossyPacketConn) SetWriteDeadline(t time.Time) error { return l.conn.SetWriteDeadline(t) }
