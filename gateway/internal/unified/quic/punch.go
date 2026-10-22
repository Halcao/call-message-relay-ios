package quic

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// validatePunchTarget enforces the anti-scanner / anti-amplifier rules for
// a client-posted punch candidate: it must be a literal IP:port that is
// globally routable (loopback, multicast, broadcast, reserved,
// benchmarking, documentation, CGNAT, unspecified and link-local ranges
// are all rejected), on a non-privileged port, without a zone identifier.
// Private targets are accepted ONLY when the operator explicitly enabled
// LANPunch; the default product path is client-initiated LAN direct (no
// server punch to private addresses). Indirection (hostnames) is never
// accepted: the candidate is punched exactly as validated.
var validatePunchTarget = validatePunchTargetImpl

func validatePunchTargetImpl(candidate string, lanPunch bool) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(candidate)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("candidate must be a literal ip:port")
	}
	addr := ap.Addr()
	if addr.Zone() != "" {
		return netip.AddrPort{}, fmt.Errorf("candidates with zone identifiers are not punched")
	}
	addr = addr.Unmap()
	if !isRoutablePunchAddr(addr, lanPunch) {
		return netip.AddrPort{}, fmt.Errorf("candidate is not a routable punch target")
	}
	if ap.Port() < 1024 {
		return netip.AddrPort{}, fmt.Errorf("privileged ports are not punched")
	}
	return netip.AddrPortFrom(addr, ap.Port()), nil
}

// isRoutablePunchAddr is the explicit IPv4/IPv6 routability policy.
func isRoutablePunchAddr(addr netip.Addr, lanPunch bool) bool {
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() {
		return false
	}
	if addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() {
		return false
	}
	if addr.IsPrivate() {
		// RFC 1918 / ULA: only under the explicit LANPunch scope; the
		// default product path is client-initiated LAN direct.
		return lanPunch
	}
	if addr.Is4() || addr.Is4In6() {
		b := addr.As4()
		switch {
		case b[0] == 0: // 0.0.0.0/8 "this network"
			return false
		case b[0] == 100 && b[1] >= 64 && b[1] <= 127: // 100.64.0.0/10 CGNAT
			return false
		case b[0] == 192 && b[1] == 0 && b[2] == 0: // 192.0.0.0/24 IETF reserved
			return false
		case b[0] == 192 && b[1] == 0 && b[2] == 2: // 192.0.2.0/24 TEST-NET-1
			return false
		case b[0] == 192 && b[1] == 88 && b[2] == 99: // 192.88.99.0/24 deprecated 6to4
			return false
		case b[0] == 198 && (b[1] == 51 && b[2] == 100): // 198.51.100.0/24 TEST-NET-2
			return false
		case b[0] == 198 && (b[1] == 18 || b[1] == 19): // 198.18.0.0/15 benchmarking
			return false
		case b[0] == 203 && b[1] == 0 && b[2] == 113: // 203.0.113.0/24 TEST-NET-3
			return false
		case b[0] >= 224: // 224.0.0.0/4 multicast + 240.0.0.0/4 reserved (incl. 255.255.255.255 broadcast)
			return false
		}
		return true
	}
	// IPv6 beyond the universal checks above.
	if addr.Is6() {
		a := addr.As16()
		switch {
		case a[0] == 0x20 && a[1] == 0x01 && a[2] == 0x0d && a[3] == 0xb8: // 2001:db8::/32 documentation
			return false
		case a[0] == 0x20 && a[1] == 0x01 && a[2] == 0x00 && a[3] == 0x02: // 2001:2::/48 benchmarking
			return false
		case a[0] == 0x20 && a[1] == 0x01 && a[2] == 0x00 && (a[3]&0xf0) == 0x10: // 2001:10::/28 ORCHID (deprecated)
			return false
		}
	}
	return true
}

// punchSession is a single bounded punch authorization. The token is
// single-use, expires quickly, and is consumed by the QUIC auth handshake
// (replays are rejected). The session dies with its window.
type punchSession struct {
	token   string
	device  string
	call    string
	target  netip.AddrPort
	created time.Time
	expires time.Time
	used    bool
}

type punchKey struct{ call, device string }

// punchCallWindow bounds the per-call punch history that feeds
// callPunchCount (rolling, so a long-lived call cannot pin the count
// forever and a finished call's count decays on its own).
const punchCallWindow = 10 * time.Minute

// punchRegistry owns live punch sessions plus the global/device/call rate
// bounds. Raw punch datagrams are fixed-size random payloads: they open
// the gateway NAT mapping for the phone's observed endpoint and expect NO
// response, so the gateway can never act as a UDP reflector/amplifier.
type punchRegistry struct {
	mu       sync.Mutex
	sessions map[punchKey]*punchSession
	byToken  map[string]*punchSession
	// devicePunchTimes is a rolling log of recent punch starts per device.
	devicePunchTimes map[string][]time.Time
	// callPunchTimes is a rolling log of punch starts per call (bounded
	// lifecycle: entries expire with punchCallWindow, so finished calls
	// release their budget without any explicit call-end hook).
	callPunchTimes map[string][]time.Time
	active         int

	lanPunch bool
	window   time.Duration
	maxDev   int // per rolling minute
	maxCall  int // per rolling call window
	maxConc  int
}

func newPunchRegistry(lanPunch bool) *punchRegistry {
	return &punchRegistry{
		sessions:         make(map[punchKey]*punchSession),
		byToken:          make(map[string]*punchSession),
		devicePunchTimes: make(map[string][]time.Time),
		callPunchTimes:   make(map[string][]time.Time),
		lanPunch:         lanPunch,
		window:           DefaultPunchWindow,
		maxDev:           DefaultMaxPunchPerDevice,
		maxCall:          DefaultMaxPunchPerCall,
		maxConc:          DefaultMaxPunchConcurrent,
	}
}

// ErrPunchRateLimited is returned when a device/call/global punch bound
// would be exceeded.
var ErrPunchRateLimited = fmt.Errorf("quic punch: rate limited")

// begin validates the candidate, enforces the bounds and registers a new
// punch session. A second begin for the SAME call+device while one is
// active is rejected (no silent replacement — replacement used to leak
// the global concurrency count).
func (r *punchRegistry) begin(device, call, candidate string) (*punchSession, error) {
	target, err := validatePunchTarget(candidate, r.lanPunch)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if _, busy := r.sessions[punchKey{call, device}]; busy {
		return nil, fmt.Errorf("quic punch: a punch is already active for this call")
	}
	// Per-device rolling window (1 minute).
	r.devicePunchTimes[device] = pruneTimes(r.devicePunchTimes[device], now.Add(-time.Minute))
	if len(r.devicePunchTimes[device]) >= r.maxDev {
		return nil, ErrPunchRateLimited
	}
	r.callPunchTimes[call] = pruneTimes(r.callPunchTimes[call], now.Add(-punchCallWindow))
	if len(r.callPunchTimes[call]) >= r.maxCall {
		return nil, ErrPunchRateLimited
	}
	if r.active >= r.maxConc {
		return nil, ErrPunchRateLimited
	}
	var tokenBytes [16]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return nil, err
	}
	sess := &punchSession{
		token:   hex.EncodeToString(tokenBytes[:]),
		device:  device,
		call:    call,
		target:  target,
		created: now,
		expires: now.Add(r.window),
	}
	r.sessions[punchKey{call, device}] = sess
	r.byToken[sess.token] = sess
	r.devicePunchTimes[device] = append(r.devicePunchTimes[device], now)
	r.callPunchTimes[call] = append(r.callPunchTimes[call], now)
	r.active++
	return sess, nil
}

// consume marks a session used (single-use token); returns false for
// unknown/expired/already-used tokens.
func (r *punchRegistry) consume(call, device, token string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.sessions[punchKey{call, device}]
	if !ok || sess.used || sess.token != token || time.Now().After(sess.expires) {
		return false
	}
	sess.used = true
	delete(r.sessions, punchKey{call, device})
	delete(r.byToken, sess.token)
	if r.active > 0 {
		r.active--
	}
	return true
}

// consumeByToken binds a QUIC connection to the call/device its token was
// minted for: single-use, TTL-bounded, and keyed to the CURRENT grant
// (ServeQUICMedia re-verifies call liveness at attach). Client bind bursts
// after consumption find no token and are ignored by the adapter.
func (r *punchRegistry) consumeByToken(token string) (call, device string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, found := r.byToken[token]
	if !found || sess.used || time.Now().After(sess.expires) {
		return "", "", false
	}
	sess.used = true
	delete(r.sessions, punchKey{sess.call, sess.device})
	delete(r.byToken, token)
	if r.active > 0 {
		r.active--
	}
	return sess.call, sess.device, true
}

// gc removes expired sessions and stale rate-window history (bounded
// lifecycle; active never goes negative).
func (r *punchRegistry) gc() {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for key, sess := range r.sessions {
		if now.After(sess.expires) {
			delete(r.sessions, key)
			delete(r.byToken, sess.token)
			if !sess.used && r.active > 0 {
				r.active--
			}
		}
	}
	for dev, times := range r.devicePunchTimes {
		kept := pruneTimes(times, now.Add(-time.Minute))
		if len(kept) == 0 {
			delete(r.devicePunchTimes, dev)
		} else {
			r.devicePunchTimes[dev] = kept
		}
	}
	for call, times := range r.callPunchTimes {
		kept := pruneTimes(times, now.Add(-punchCallWindow))
		if len(kept) == 0 {
			delete(r.callPunchTimes, call)
		} else {
			r.callPunchTimes[call] = kept
		}
	}
}

func pruneTimes(times []time.Time, cutoff time.Time) []time.Time {
	kept := times[:0]
	for _, t := range times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	return kept
}

// punchLoop sends the bounded raw-UDP punch burst for a session.
func (r *punchRegistry) punchLoop(udp *net.UDPConn, sess *punchSession) {
	target := net.UDPAddrFromAddrPort(sess.target)
	// Fixed-size random payloads: no request/response semantics, no
	// amplification factor, identifiable only as NAT-opening noise.
	buf := make([]byte, PunchPacketBytes)
	_, _ = rand.Read(buf)
	deadline := time.NewTimer(r.window)
	defer deadline.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	sent := 0
	send := func() {
		if sent >= PunchPacketCount {
			return
		}
		_, _ = udp.WriteTo(buf, target)
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
}
