// Package quic implements the build-61 QUIC DATAGRAM direct media listener
// on the gateway: one UDP socket shared between STUN (RFC 8489 candidate
// discovery) and the QUIC listener (RFC 9000 + RFC 9221), authenticated
// per-call media attach with epoch-fenced datagrams, authenticated
// candidate exchange with bounded NAT punching, and an identity-bound
// certificate pin document. It is additive: when disabled or when the
// listener fails to start, the gateway serves ice+ws exactly as before
// and advertises nothing.
package quic

import (
	"errors"
	"fmt"
	"time"
)

// ALPN is the negotiated application protocol for media connections.
const ALPN = "callrelay-media"

// Default configuration values (overridable via Config).
const (
	DefaultListen             = ":8792"
	DefaultCandidateTTL       = 60 * time.Second
	DefaultSTUNRefresh        = 20 * time.Second
	DefaultSTUNTimeout        = 3 * time.Second
	DefaultMaxPendingConns    = 32
	DefaultAuthTimeout        = 5 * time.Second
	DefaultPunchWindow        = 8 * time.Second
	DefaultMaxPunchConcurrent = 4
	DefaultMaxPunchPerDevice  = 8          // per rolling minute
	// Per-call punch budget sized for the promised 45 s recovery: four
	// 8 s attempts inside the window (0/9/19/31 s) plus flapping/network-
	// epoch headroom. A rate-limit answer is a budgeting signal, NEVER
	// credential revocation: the client keeps its incumbent audio and
	// retries inside the recovery grace.
	DefaultMaxPunchPerCall    = 10         // per rolling 10 minute window
	DefaultWarmGrantTTL       = 2 * time.Minute
	DefaultWarmMinInterval    = 30 * time.Second // per device between warm probes
	DefaultHandshakeIdle      = 90 * time.Second
	PunchPacketCount          = 4
	PunchPacketBytes          = 64
)

// Config controls the QUIC direct media listener.
type Config struct {
	// Listen is the UDP listen address (private LAN scope; no public port
	// mapping is required or created). Default ":8792".
	Listen string
	// STUNServers are host:port candidates used for server-reflexive
	// address discovery (RFC 8489). Typically the gateway's existing
	// ice.stun_servers. May be empty (candidate = host addresses only).
	STUNServers []string
	// CandidateTTL bounds how long an advertised srflx candidate stays
	// valid; refreshed continuously while running.
	CandidateTTL time.Duration
	// LANPunch allows server-side punching of PRIVATE client candidates.
	// Default false: LAN direct is client-initiated (the client dials the
	// gateway's LAN address directly; no server punch to private targets),
	// and only global-unicast non-reserved public targets are punched.
	LANPunch bool
	// MaxPendingConns bounds unauthenticated connections.
	MaxPendingConns int
	// AuthTimeout bounds the first (auth) control message.
	AuthTimeout time.Duration
	// DataDir holds the persisted self-signed certificate (0600).
	DataDir string
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.CandidateTTL <= 0 {
		c.CandidateTTL = DefaultCandidateTTL
	}
	if c.MaxPendingConns <= 0 {
		c.MaxPendingConns = DefaultMaxPendingConns
	}
	if c.AuthTimeout <= 0 {
		c.AuthTimeout = DefaultAuthTimeout
	}
}

// ErrClosed is returned by operations on a stopped Server.
var ErrClosed = errors.New("quic: server closed")

// errAuthFailed closes a connection with an auth-specific application error.
type authError struct{ msg string }

func (e *authError) Error() string { return fmt.Sprintf("quic auth: %s", e.msg) }
