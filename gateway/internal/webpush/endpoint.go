package webpush

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Endpoint validation enforces the SSRF boundary for outbound Web Push: a
// subscription endpoint is a browser vendor's public HTTPS push service, so
// the gateway refuses anything that is not https with a globally routable
// resolved address. Validation happens at subscribe time AND at send time
// (defense in depth against stale or tampered rows), with the same dial-time
// re-resolution the Bark client uses, so a DNS name that flips to a non-
// public address between check and send is still rejected.

var (
	ErrEndpointInvalid    = errors.New("webpush: endpoint is invalid")
	ErrEndpointDisallowed = errors.New("webpush: endpoint is not allowed")
	ErrSubscriptionKey    = errors.New("webpush: subscription key is invalid")
)

const (
	maxEndpointLength = 2048
	maxKeyLength      = 256
)

// specialUseBlocks lists IANA special-purpose ranges that Go's
// IsGlobalUnicast still reports as unicast but that must never receive
// gateway traffic (CGNAT shared space, benchmarking, documentation/test
// networks and IETF reserved assignments). Loopback/link-local/multicast/
// unspecified are already excluded by IsGlobalUnicast plus the private-range
// check; IPv4-mapped IPv6 is unwrapped before evaluation.
var specialUseBlocks = func() []netip.Prefix {
	raw := []string{
		// IPv4
		"100.64.0.0/10",    // RFC 6598 CGNAT / shared address space
		"192.0.0.0/24",     // RFC 6890 IETF protocol assignments
		"192.0.2.0/24",     // RFC 5737 TEST-NET-1
		"198.18.0.0/15",    // RFC 2544 benchmarking
		"198.51.100.0/24",  // RFC 5737 TEST-NET-2
		"203.0.113.0/24",   // RFC 5737 TEST-NET-3
		"192.88.99.0/24",   // RFC 7526 deprecated relay anycast
		"240.0.0.0/4",      // RFC 1112 reserved
		"255.255.255.255/32", // broadcast
		// IPv6
		"::/128",           // RFC 4291 unspecified
		"2001:db8::/32",    // RFC 3849 documentation
		"2001:2::/48",      // RFC 5180 benchmarking
		"2001:10::/28",     // RFC 4843 ORCHID
		"3fff::/20",        // RFC 9637 documentation (newer allocations)
	}
	prefixes := make([]netip.Prefix, 0, len(raw))
	for _, cidr := range raw {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			panic(fmt.Sprintf("webpush: bad special-use block %q: %v", cidr, err))
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes
}()

// Policy decides which endpoint hosts/addresses are acceptable. An empty
// Allowlist keeps the default deny-special-use behaviour (any globally
// routable HTTPS endpoint is accepted, which suits every major browser push
// service); a non-empty Allowlist switches the gateway to explicit
// vendor-endpoint compatibility mode where only the listed host suffixes
// pass, for operators who want a narrower outbound surface.
type Policy struct {
	Allowlist []string
	// allowLoopback relaxes the address check for tests that run local
	// servers. Never settable outside this package: production gateways
	// always evaluate the strict policy.
	allowLoopback bool
}

func (p Policy) hostAllowed(host string) bool {
	if len(p.Allowlist) == 0 {
		return true
	}
	host = strings.ToLower(host)
	for _, suffix := range p.Allowlist {
		suffix = strings.ToLower(strings.TrimSpace(suffix))
		if suffix == "" {
			continue
		}
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// lookupIP is rebindable in tests to simulate mixed DNS answers.
var lookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func (p Policy) endpointAddressAllowed(addr netip.Addr) bool {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if p.allowLoopback && (addr.IsLoopback() || addr.IsPrivate()) {
		return true
	}
	if !addr.IsValid() || !addr.IsGlobalUnicast() {
		return false
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, block := range specialUseBlocks {
		if block.Contains(addr) {
			return false
		}
	}
	return true
}

// ValidateEndpoint statically validates a subscription endpoint URL.
func (p Policy) ValidateEndpoint(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > maxEndpointLength {
		return "", ErrEndpointInvalid
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", ErrEndpointInvalid
	}
	if parsed.Scheme != "https" {
		return "", ErrEndpointDisallowed
	}
	if parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", ErrEndpointInvalid
	}
	host := parsed.Hostname()
	if host == "" {
		return "", ErrEndpointInvalid
	}
	if !p.hostAllowed(host) {
		return "", ErrEndpointDisallowed
	}
	if addr, addrErr := netip.ParseAddr(host); addrErr == nil {
		if !p.endpointAddressAllowed(addr) {
			return "", ErrEndpointDisallowed
		}
	}
	if port := parsed.Port(); port != "" {
		value, convErr := net.LookupPort("tcp", port)
		if convErr != nil || value < 1 || value > 65535 {
			return "", ErrEndpointInvalid
		}
	}
	return parsed.String(), nil
}

// ResolveEndpointHost re-resolves the endpoint host and requires every
// resolved address to be globally routable. Called at send time so DNS
// rebinding cannot smuggle a non-public target past the static check; a
// mixed answer (any public + any non-public address) fails closed.
func (p Policy) ResolveEndpointHost(ctx context.Context, endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return ErrEndpointInvalid
	}
	host := parsed.Hostname()
	if host == "" {
		return ErrEndpointInvalid
	}
	if !p.hostAllowed(host) {
		return ErrEndpointDisallowed
	}
	if addr, addrErr := netip.ParseAddr(host); addrErr == nil {
		if !p.endpointAddressAllowed(addr) {
			return ErrEndpointDisallowed
		}
		return nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addresses, err := lookupIP(lookupCtx, host)
	if err != nil || len(addresses) == 0 {
		return fmt.Errorf("webpush: resolve endpoint: %w", err)
	}
	for _, addr := range addresses {
		if !p.endpointAddressAllowed(addr) {
			return ErrEndpointDisallowed
		}
	}
	return nil
}

// ValidateP256dh requires the exact RFC 8291/browser wire form of the user
// agent public key: base64url (padded or not) of a 65-byte uncompressed
// P-256 point (0x04 || X || Y). The sender library unmarshals exactly this
// form, so anything else would fail at delivery time — reject it here,
// before persistence.
func ValidateP256dh(raw string) (string, error) {
	return validateDecodedKey(raw, 65, func(decoded []byte) bool {
		return decoded[0] == 0x04
	})
}

// ValidateAuth requires the RFC 8291 auth secret: base64url of exactly
// 16 bytes.
func ValidateAuth(raw string) (string, error) {
	return validateDecodedKey(raw, 16, nil)
}

func validateDecodedKey(raw string, size int, extra func([]byte) bool) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > maxKeyLength {
		return "", ErrSubscriptionKey
	}
	decoded, err := decodeBase64URL(trimmed)
	if err != nil || len(decoded) != size {
		return "", ErrSubscriptionKey
	}
	if extra != nil && !extra(decoded) {
		return "", ErrSubscriptionKey
	}
	return trimmed, nil
}

func decodeBase64URL(value string) ([]byte, error) {
	if decoded, err := base64.URLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawURLEncoding.DecodeString(value)
}

// hardenedClient returns the HTTP client used for endpoint POSTs: no proxy,
// no redirect following (a 307 would re-post the encrypted body to a host
// we did not validate), bounded handshake, and dial-time validation of every
// resolved address against the public-only policy.
func hardenedClient(policy Policy, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("webpush dial: %w", err)
			}
			var addresses []netip.Addr
			if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
				addresses = []netip.Addr{addr}
			} else {
				resolved, lookupErr := lookupIP(ctx, host)
				if lookupErr != nil || len(resolved) == 0 {
					return nil, fmt.Errorf("webpush dial: resolve %s: %w", host, lookupErr)
				}
				addresses = resolved
			}
			for _, addr := range addresses {
				if !policy.endpointAddressAllowed(addr) {
					return nil, ErrEndpointDisallowed
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
		},
		Proxy:                 nil,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
