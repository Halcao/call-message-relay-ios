package quic

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/media"

	"github.com/quic-go/quic-go"
)

// SessionRef binds a QUICSocket's envelope epoch source to the media
// session that adopts it (set by the runtime attach path). Until set, the
// epoch is 0 and every inbound datagram is dropped.
type SessionRef struct {
	ptr           atomic.Pointer[media.Session]
	static        atomic.Uint64
	epochOverride atomic.Bool
}

// Set binds the session once the runtime has looked it up.
func (r *SessionRef) Set(s *media.Session) { r.ptr.Store(s) }

// Epoch reports the current primary-role epoch (0 before adoption).
func (r *SessionRef) Epoch() uint64 {
	if r.epochOverride.Load() {
		return r.static.Load()
	}
	s := r.ptr.Load()
	if s == nil {
		return 0
	}
	return s.Epoch()
}

// SetStaticEpoch pins a constant epoch (test hook for the accept path
// before a session exists; production uses Set with the live session).
func (r *SessionRef) SetStaticEpoch(v uint64) {
	r.static.Store(v)
	r.epochOverride.Store(true)
}

// AttachFunc is the runtime hook that adopts an authenticated QUIC media
// connection exactly like an authenticated WSS upgrade (same call guards,
// session ownership, fencing). It blocks until the media pump ends.
type AttachFunc func(ctx context.Context, deviceID, callID, codec string, qs *media.QUICSocket, standby bool, ref *SessionRef) error

// Server is the QUIC direct media listener. Zero value is not usable; use
// New + Start. All exported state accessors are safe after Close.
type Server struct {
	cfg    Config
	id     *identity.Identity
	auth   *auth.Service
	attach AttachFunc

	udp       *net.UDPConn
	stun      *stunClient
	demux     *demuxConn
	transport *quic.Transport
	listener  *quic.Listener
	material  *certMaterial
	punches   *punchRegistry

	sem chan struct{} // unauthenticated-connection bound

	// candidate is the freshest server-reflexive address (plus its
	// source and expiry). Advertised ONLY via the authenticated API.
	candMu     sync.RWMutex
	candidate  string
	candSource string
	candExpire time.Time
	// hostAddrs are the listener's LAN addresses (advertised alongside
	// the srflx candidate for client-initiated LAN direct).
	hostMu    sync.RWMutex
	hostAddrs []string

	warmMu      sync.Mutex
	warmGrants  map[string]warmGrant // token -> grant
	warmLast    map[string]time.Time
	authAttempts sync.Map            // ip -> *attemptWindow

	stopCtx    context.Context
	stop       context.CancelFunc
	done       chan struct{}
	closeOnce  sync.Once
	startedAt  time.Time
	liveCount  atomic.Int64
}

// ErrWarmRateLimited is returned when a device asks to warm too often; it
// is a budgeting signal, never a revocation.
var ErrWarmRateLimited = errors.New("quic warm: requested too frequently")

type warmGrant struct {
	device  string
	target  netip.AddrPort
	expires time.Time
}

type attemptWindow struct {
	mu    sync.Mutex
	times []time.Time
}

// New builds a Server. attach is required; auth is required (token
// validation). Returns an error only on certificate problems.
func New(cfg Config, id *identity.Identity, authSvc *auth.Service, attach AttachFunc) (*Server, error) {
	cfg.defaults()
	if attach == nil {
		return nil, fmt.Errorf("quic: attach hook is required")
	}
	if authSvc == nil {
		return nil, fmt.Errorf("quic: auth service is required")
	}
	material, err := loadOrCreateCert(cfg.DataDir, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		cfg:       cfg,
		id:        id,
		auth:      authSvc,
		attach:    attach,
		material:  material,
		punches:   newPunchRegistry(cfg.LANPunch),
		sem:       make(chan struct{}, cfg.MaxPendingConns),
		warmGrants: make(map[string]warmGrant),
		warmLast:   make(map[string]time.Time),
		stopCtx:   ctx,
		stop:      cancel,
		done:      make(chan struct{}),
	}, nil
}

// Addr reports the bound UDP address (nil before Start).
func (s *Server) Addr() net.Addr {
	if s.udp == nil {
		return nil
	}
	return s.udp.LocalAddr()
}

// Pin returns the signed pin document (safe to serve over the
// authenticated API; it contains no secrets).
func (s *Server) Pin() PinDocument { return s.material.pin }

// Live reports whether the listener is running (capability gate: the
// gateway advertises QUIC only while this is true).
func (s *Server) Live() bool {
	select {
	case <-s.done:
		return false
	default:
		return s.listener != nil
	}
}

// Start binds the socket and serves until Close. On ANY setup failure the
// server logs, leaves Live() false and returns nil — QUIC support is
// optional by design and a failure must never affect ice+ws service.
func (s *Server) Start() error {
	udpAddr, err := net.ResolveUDPAddr("udp", s.cfg.Listen)
	if err != nil {
		slog.Warn("quic disabled: bad listen address", "address", s.cfg.Listen, "error", err)
		return nil
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		slog.Warn("quic disabled: udp bind failed", "address", s.cfg.Listen, "error", err)
		return nil
	}
	s.udp = udp
	s.stun = newSTUNClient(udp)
	s.demux = newDemuxConn(udp, s.stun)
	go s.demux.loop()

	s.transport = &quic.Transport{Conn: s.demux}
	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{s.material.cert.Raw},
			PrivateKey:  s.material.key,
		}},
		NextProtos: []string{ALPN},
	}
	listener, err := s.transport.Listen(tlsConf, &quic.Config{
		EnableDatagrams: true,
		MaxIdleTimeout:  DefaultHandshakeIdle,
		// No 0-RTT: call actions must never be replayable (auth happens
		// on the post-handshake control stream anyway, but keep the
		// transport honest too).
		Allow0RTT: false,
	})
	if err != nil {
		slog.Warn("quic disabled: listener failed", "error", err)
		_ = udp.Close()
		return nil
	}
	s.listener = listener
	s.startedAt = time.Now()
	s.refreshHostAddrs()
	go s.candidateLoop()
	go s.gcLoop()
	go s.acceptLoop()
	slog.Info("quic direct media listening",
		"address", udp.LocalAddr().String(),
		"spki_pin", s.material.pin.SPKIHash[:16]+"…",
		"cert_not_after", time.Unix(s.material.pin.NotAfter, 0).Format(time.RFC3339))
	return nil
}

// Close stops the listener and everything it owns.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.stop()
		if s.listener != nil {
			err = s.listener.Close()
		}
		if s.transport != nil {
			_ = s.transport.Close()
		}
		if s.udp != nil {
			_ = s.udp.Close()
		}
		close(s.done)
	})
	return err
}

// refreshHostAddrs enumerates the listener's non-loopback LAN addresses.
func (s *Server) refreshHostAddrs() {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aerr := iface.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				ip = ip4
			}
			out = append(out, net.JoinHostPort(ip.String(), fmt.Sprintf("%d", s.udp.LocalAddr().(*net.UDPAddr).Port)))
		}
	}
	s.hostMu.Lock()
	s.hostAddrs = out
	s.hostMu.Unlock()
}

// candidateLoop keeps the srflx mapping fresh. A NAT binding can expire
// within seconds of silence; the periodic STUN refresh (plus live media)
// keeps it valid and the advertised TTL is always conservative.
func (s *Server) candidateLoop() {
	tick := time.NewTicker(DefaultSTUNRefresh)
	defer tick.Stop()
	s.refreshCandidate()
	for {
		select {
		case <-s.stopCtx.Done():
			return
		case <-tick.C:
			s.refreshCandidate()
		}
	}
}

func (s *Server) refreshCandidate() {
	ctx, cancel := context.WithTimeout(s.stopCtx, 2*DefaultSTUNTimeout)
	defer cancel()
	for _, server := range s.cfg.STUNServers {
		ap, err := s.stun.query(ctx, server)
		if err != nil {
			slog.Debug("stun refresh failed", "server", server, "error", err)
			continue
		}
		addr := ap.Addr()
		if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsMulticast() {
			continue
		}
		s.candMu.Lock()
		s.candidate = ap.String()
		s.candSource = server
		s.candExpire = time.Now().Add(s.cfg.CandidateTTL)
		s.candMu.Unlock()
		s.refreshHostAddrs()
		return
	}
}

func (s *Server) gcLoop() {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-s.stopCtx.Done():
			return
		case <-tick.C:
			s.punches.gc()
			s.gcWarm()
		}
	}
}

func (s *Server) gcWarm() {
	now := time.Now()
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	for tok, grant := range s.warmGrants {
		if now.After(grant.expires) {
			delete(s.warmGrants, tok)
		}
	}
}

// CandidateReport is the authenticated-API view of the listener.
type CandidateReport struct {
	Enabled     bool     `json:"enabled"`
	ALPN        string   `json:"alpn"`
	Pin         PinDocument `json:"pin"`
	Candidates  []Candidate `json:"candidates"`
	STUNServers []string `json:"stun"`
}

// Candidate is one dialable endpoint advertisement.
type Candidate struct {
	Type        string `json:"type"` // "srflx" | "host"
	Proto       string `json:"proto"` // always "quic"
	Addr        string `json:"addr"`
	ExpiresAtMs int64  `json:"expiresAtMs,omitempty"`
}

// Info builds the capability document (served only over authenticated
// HTTPS). When the listener is not live it returns enabled=false and the
// gateway must NOT advertise QUIC in mediaTransports.
func (s *Server) Info() CandidateReport {
	report := CandidateReport{
		Enabled:     s.Live(),
		ALPN:        ALPN,
		Pin:         s.material.pin,
		STUNServers: append([]string(nil), s.cfg.STUNServers...),
	}
	if !report.Enabled {
		return report
	}
	s.candMu.RLock()
	cand, expires := s.candidate, s.candExpire
	s.candMu.RUnlock()
	if cand != "" && time.Now().Before(expires) {
		report.Candidates = append(report.Candidates, Candidate{
			Type: "srflx", Proto: "quic", Addr: cand, ExpiresAtMs: expires.UnixMilli(),
		})
	}
	s.hostMu.RLock()
	hosts := append([]string(nil), s.hostAddrs...)
	s.hostMu.RUnlock()
	for _, h := range hosts {
		report.Candidates = append(report.Candidates, Candidate{Type: "host", Proto: "quic", Addr: h})
	}
	return report
}

// PunchResult is returned by StartPunch (and becomes the API response).
type PunchResult struct {
	PunchToken      string `json:"punchToken"`
	GatewayCandidate string `json:"gatewayCandidate"`
	PunchAtMs       int64  `json:"punchAtMs"`
	DeadlineMs      int64  `json:"deadlineMs"`
}

// ErrNoCandidate means no srflx candidate is currently known.
var ErrNoCandidate = errors.New("quic: no server-reflexive candidate currently known")

// StartPunch validates the client's candidate, enforces the punch bounds
// and arms the punch window. The returned token is single-use and must be
// presented by the QUIC auth handshake within its TTL (tied to the live
// call grant — ServeQUICMedia re-verifies call liveness).
func (s *Server) StartPunch(deviceID, callID, candidate string) (PunchResult, error) {
	sess, err := s.punches.begin(deviceID, callID, candidate)
	if err != nil {
		return PunchResult{}, err
	}
	s.candMu.RLock()
	gwCand := s.candidate
	candValid := gwCand != "" && time.Now().Before(s.candExpire)
	s.candMu.RUnlock()
	if !candValid {
		// LAN-only gateways (or STUN outage) advertise their host
		// candidates instead; the client dials them directly.
		s.hostMu.RLock()
		if len(s.hostAddrs) > 0 {
			gwCand = s.hostAddrs[0]
			candValid = true
		}
		s.hostMu.RUnlock()
	}
	if !candValid {
		s.punches.consume(callID, deviceID, sess.token) // roll back
		return PunchResult{}, ErrNoCandidate
	}
	punchAt := time.Now().Add(150 * time.Millisecond)
	res := PunchResult{
		PunchToken:       sess.token,
		GatewayCandidate: gwCand,
		PunchAtMs:        punchAt.UnixMilli(),
		DeadlineMs:       sess.expires.UnixMilli(),
	}
	go s.punches.punchLoop(s.udp, sess)
	return res, nil
}

// IssueWarmGrant registers a device-grant for a pre-call QUIC warm
// attempt (bounded: one outstanding grant per device, min interval
// between attempts, 2 warm punches per minute per device). The client
// posts its STUN-learned candidate so the gateway can coordinate a
// bounded device-grant punch — a client-only Initial cannot establish
// fresh pre-call reachability evidence under endpoint-filtering NAT.
// The grant authorizes ONE probe connection; no call, no mixer, no
// microphone.
// WarmResult is the authenticated-API response for a warm grant.
type WarmResult struct {
	Enabled     bool   `json:"enabled"`
	WarmToken   string `json:"warmToken"`
	GatewayCandidate string `json:"gatewayCandidate,omitempty"`
	PunchAtMs   int64  `json:"punchAtMs,omitempty"`
}

// IssueWarmGrant issues (or reuses) a device-grant and starts the bounded
// coordinated punch. Rate-limit answers are budgeting signals, never
// revocations.
func (s *Server) IssueWarmGrant(deviceID, candidate string) (WarmResult, error) {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	now := time.Now()
	res := WarmResult{Enabled: true}
	if last, ok := s.warmLast[deviceID]; ok && now.Sub(last) < DefaultWarmMinInterval {
		return res, ErrWarmRateLimited
	}
	// The warm candidate obeys the same anti-scanner validation as call
	// punches; the grant adds no new public surface.
	target, err := validatePunchTarget(candidate, s.cfg.LANPunch)
	if err != nil {
		return res, err
	}
	// Reuse the device's outstanding grant (idempotent warm-up).
	for tok, grant := range s.warmGrants {
		if grant.device == deviceID && now.Before(grant.expires) {
			res.WarmToken = tok
			res.GatewayCandidate = s.currentGatewayCandidateLocked()
			res.PunchAtMs = now.Add(150 * time.Millisecond).UnixMilli()
			return res, nil
		}
	}
	var tokBytes [16]byte
	if _, err := rand.Read(tokBytes[:]); err != nil {
		return res, err
	}
	token := fmt.Sprintf("%x", tokBytes[:])
	s.warmGrants[token] = warmGrant{device: deviceID, target: target, expires: now.Add(DefaultWarmGrantTTL)}
	s.warmLast[deviceID] = now
	if s.udp != nil {
		// Coordinated bounded punch so the phone's Initial can arrive
		// under endpoint-filtering NAT (in-call candidates get the same
		// treatment via StartPunch).
		go func() {
			deadline := time.NewTimer(DefaultPunchWindow)
			defer deadline.Stop()
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			buf := make([]byte, PunchPacketBytes)
			_, _ = rand.Read(buf)
			sent := 0
			send := func() {
				if sent >= PunchPacketCount {
					return
				}
				_, _ = s.udp.WriteTo(buf, net.UDPAddrFromAddrPort(target))
				sent++
			}
			send()
			for {
				select {
				case <-tick.C:
					send()
				case <-deadline.C:
					return
				}
			}
		}()
	}
	res.WarmToken = token
	res.GatewayCandidate = s.currentGatewayCandidate()
	res.PunchAtMs = now.Add(150 * time.Millisecond).UnixMilli()
	return res, nil
}

// currentGatewayCandidate prefers the live srflx candidate and falls back
// to the first LAN host address.
func (s *Server) currentGatewayCandidate() string {
	s.candMu.RLock()
	cand, expires := s.candidate, s.candExpire
	s.candMu.RUnlock()
	if cand != "" && time.Now().Before(expires) {
		return cand
	}
	s.hostMu.RLock()
	defer s.hostMu.RUnlock()
	if len(s.hostAddrs) > 0 {
		return s.hostAddrs[0]
	}
	return ""
}

func (s *Server) currentGatewayCandidateLocked() string { return s.currentGatewayCandidate() }

// consumeWarmGrantByToken validates + consumes a warm grant by its token
// (single use; possession of the token — minted by the authenticated
// warm endpoint — is the proof on the UDP channel).
func (s *Server) consumeWarmGrantByToken(token string) bool {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	grant, ok := s.warmGrants[token]
	if !ok || time.Now().After(grant.expires) {
		return false
	}
	delete(s.warmGrants, token)
	return true
}

// acceptLoop serves authenticated media connections until Close.
func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept(s.stopCtx)
		if err != nil {
			return
		}
		s.liveCount.Add(1)
		go func() {
			defer s.liveCount.Add(-1)
			s.serveConn(conn)
		}()
	}
}

// serveConn performs the bounded auth handshake, then hands the socket to
// the runtime attach hook. ANY failure closes the connection with a
// descriptive application error; nothing reaches the media layer
// unauthenticated.
func (s *Server) serveConn(conn *quic.Conn) {
	remote := conn.RemoteAddr().String()
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	if !s.allowAuthAttempt(host) {
		slog.Debug("quic auth rate limited", "remote", host)
		_ = conn.CloseWithError(0x2, "rate limited")
		return
	}
	select {
	case s.sem <- struct{}{}:
		// The semaphore bounds PENDING AUTHENTICATION only; it is
		// released right after the auth handshake succeeds so live calls
		// are never capped by the unauthenticated-connection budget.
	case <-time.After(s.cfg.AuthTimeout):
		_ = conn.CloseWithError(0x2, "server busy")
		return
	case <-s.stopCtx.Done():
		return
	}

	// Bind handshake (audio-only session): the first datagram must be a
	// [0x03][token] bind whose token was minted by the authenticated
	// candidate POST for a live call+device (or, for probes, by the
	// authenticated warm endpoint). Audio/probe datagrams that arrive
	// first are discarded; a stray datagram at the punched port cannot
	// attach without the authenticated HTTPS exchange. Post-bind bursts
	// are ignored by the adapter, so lossy retransmits cannot authorize
	// twice.
	ctx, cancel := context.WithTimeout(s.stopCtx, s.cfg.AuthTimeout)
	defer cancel()
	token, err := readBindDatagram(ctx, conn)
	if err != nil {
		<-s.sem
		_ = conn.CloseWithError(0x2, "bind timeout")
		return
	}
	callID, deviceID, ok := s.punches.consumeByToken(token)
	if !ok {
		// Not a media token: try the warm-grant table (pre-call probe).
		if s.consumeWarmGrantByToken(token) {
			<-s.sem
			s.serveProbe(ctx, conn)
			return
		}
		<-s.sem
		_ = conn.CloseWithError(0x2, "invalid or expired bind token")
		return
	}
	<-s.sem // bound: the pending-auth slot is free again

	ref := &SessionRef{}
	wrapped := wrapConn{conn}
	// Audio-only adapter: envelopes + RTT probes; all critical control
	// (ready/promote/denoise/bye) uses the existing authenticated WSS
	// plane bound by call/device/epoch (single datagram flow per WWDC22;
	// NWMultiplexGroup datagram extraction has a reported platform
	// limitation, so no stream multiplexing is attempted).
	qs := media.NewQUICAudioSocket(s.stopCtx, wrapped, ref.Epoch)
	slog.Info("quic media attaching", "call_id", callID, "device", deviceID, "remote", remote)
	if err := s.attach(s.stopCtx, deviceID, callID, "opus", qs, true, ref); err != nil {
		slog.Info("quic media attach rejected", "call_id", callID, "device", deviceID, "error", err)
		_ = conn.CloseWithError(0x2, "attach rejected")
		return
	}
	// Attach ended (pump returned): converge the connection.
	_ = conn.CloseWithError(0x0, "done")
}

// serveProbe answers a pre-call warm probe: the client dials and sends
// [0x03][warmToken] bursts (warmToken from the authenticated POST
// /media/quic/warm); the server validates the single-use grant and echoes
// [0x04][t] probe acks three times. No session, no mixer, no microphone —
// NAT warm-up + reachability proof only. Everything is idempotent and
// loss-tolerant by design (no ownership decisions ride on it).
// serveProbe echoes the client's own [0x04][nonce u64] probes verbatim
// for a bounded window: the RTT sample uses ONLY the client clock (two
// unsynchronized wall clocks must never be subtracted) and the sample
// reflects the post-bind steady path (handshake/bind duration is separate
// evidence). The probe then closes — a closed probe is NOT a maintained
// warm media connection; the app re-warms on its cadence.
func (s *Server) serveProbe(ctx context.Context, conn *quic.Conn) {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			_ = conn.CloseWithError(0x0, "warm complete")
			return
		default:
		}
		dctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		pkt, err := conn.ReceiveDatagram(dctx)
		cancel()
		if err != nil {
			continue
		}
		if len(pkt) >= 9 && pkt[0] == media.QUICProbeVersion {
			if err := conn.SendDatagram(pkt); err != nil {
				return
			}
		}
	}
}

// readBindDatagram waits for the first [0x03][token] bind datagram
// (discarding anything else, all counted server-side by the session
// bound) and returns the token bytes.
func readBindDatagram(ctx context.Context, conn *quic.Conn) (string, error) {
	for {
		pkt, err := conn.ReceiveDatagram(ctx)
		if err != nil {
			return "", err
		}
		if len(pkt) < 2 || pkt[0] != media.QUICBindVersion {
			continue // audio/probe pre-bind or garbage: never reaches the codec
		}
		return string(pkt[1:]), nil
	}
}

// allowAuthAttempt enforces 4 auth attempts per 10 s per source IP.
func (s *Server) allowAuthAttempt(ip string) bool {
	windowAny, _ := s.authAttempts.LoadOrStore(ip, &attemptWindow{})
	w := windowAny.(*attemptWindow)
	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-10 * time.Second)
	kept := w.times[:0]
	for _, t := range w.times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.times = kept
	if len(kept) >= 4 {
		return false
	}
	w.times = append(w.times, now)
	return true
}

// readControlLine reads one newline-delimited JSON control line with a
// size bound.
func readControlLine(stream *quic.Stream, max int) ([]byte, error) {
	buf := make([]byte, 0, 256)
	one := make([]byte, 1)
	for len(buf) < max {
		n, err := stream.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return buf, nil
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("control line too long")
}

// wrapConn adapts *quic.Conn to media.QUICConnection without the media
// package importing quic-go.
type wrapConn struct{ c *quic.Conn }

func (w wrapConn) AcceptStream(ctx context.Context) (media.QUICStream, error) {
	return w.c.AcceptStream(ctx)
}
func (w wrapConn) SendDatagram(p []byte) error            { return w.c.SendDatagram(p) }
func (w wrapConn) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	return w.c.ReceiveDatagram(ctx)
}
func (w wrapConn) CloseWithError(code uint64, msg string) error {
	return w.c.CloseWithError(quic.ApplicationErrorCode(code), msg)
}
func (w wrapConn) RemoteAddr() net.Addr { return w.c.RemoteAddr() }
