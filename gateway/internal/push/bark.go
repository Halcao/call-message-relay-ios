package push

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Bark is an OPTIONAL, self-hosted notification path. It never replaces the
// native APNs/PushKit provider: a deployment with no Bark configuration still
// rings through the normal push + CallKit path, and a device only receives
// Bark notifications after deliberately enabling them and storing its own
// server URL + device key on the gateway.
//
// The device key is a secret. It is sent in the JSON body (never in a URL or
// query string), the gateway never logs it, and the settings API only ever
// returns a redacted hint.
const (
	BarkDefaultTimeout = 10 * time.Second
	barkMaxURLLength   = 512
	barkMaxKeyLength   = 256
	barkResponseLimit  = 8 << 10

	// BarkDeepLinkURL is a static app deeplink carried by the notification.
	// It intentionally contains no token, no gateway credential and no call
	// id: the app re-authenticates with its own paired credential and asks
	// the gateway for actually-ringing calls, so a leaked or replayed
	// notification cannot authorize anything.
	BarkDeepLinkURL = "callrelay://incoming"
)

var (
	ErrBarkURLInvalid    = errors.New("bark server url is invalid")
	ErrBarkURLDisallowed = errors.New("bark server url is not allowed")
	ErrBarkKeyInvalid    = errors.New("bark device key is invalid")
)

// BarkTarget is one device's opt-in Bark destination.
type BarkTarget struct {
	DeviceID  string
	ServerURL string
	DeviceKey string
	Locale    string
}

// BarkNotification is the minimal privacy-preserving alert body. It carries a
// caller handle (needed to know who is ringing) but never message content,
// call tokens or gateway credentials.
type BarkNotification struct {
	Title string
	Body  string
	URL   string
	Group string
	Level string
}

// BarkClient posts notifications to a Bark-compatible server. Transport
// policy:
//
//   - https is required for any public destination (device key + caller id
//     must not travel in clear text across a network);
//   - http is only ever attempted when LAN mode is enabled AND the resolved
//     destination is actually private (loopback/link-local/RFC1918/ULA),
//     checked at dial time, not merely because allow_private is set;
//   - with the policy off, every resolved address must be globally routable,
//     so a mistyped or attacker-controlled host cannot turn the gateway into
//     an SSRF probe against loopback/LAN services.
type BarkClient struct {
	AllowPrivate bool
	HTTPClient   *http.Client
	// lanClient is used for http:// requests only; its dialer additionally
	// requires a private resolved address.
	lanClient *http.Client
}

func NewBarkClient(allowPrivate bool, timeout time.Duration) *BarkClient {
	if timeout <= 0 {
		timeout = BarkDefaultTimeout
	}
	dialer := &net.Dialer{Timeout: timeout}
	return &BarkClient{
		AllowPrivate: allowPrivate,
		HTTPClient:   barkHTTPClient(dialer, allowPrivate, false, timeout),
		lanClient:    barkHTTPClient(dialer, allowPrivate, true, timeout),
	}
}

func barkHTTPClient(dialer *net.Dialer, allowPrivate, requirePrivate bool, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DialContext:           barkDialContext(dialer, allowPrivate, requirePrivate),
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		// Never route the device key through an environment proxy.
		Proxy: nil,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		// The device key travels in the request body. Following a redirect
		// would resend that body (307/308) to a host we did not validate, so
		// redirects are never followed; the 3xx is surfaced as an error.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// clientFor picks the transport that enforces the scheme policy for this
// request: http is pinned to private resolved addresses, https uses the
// standard host policy.
func (c *BarkClient) clientFor(base string) *http.Client {
	if strings.HasPrefix(base, "http://") && c.lanClient != nil {
		return c.lanClient
	}
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return c.lanClient
}

// ValidateBarkServerURL normalizes and validates a user-supplied server base
// URL. Only http(s) is accepted, credentials/fragments/queries are refused.
// https may target a public host; http is only accepted with explicit LAN
// opt-in, and then only for a private/loopback/link-local destination (a
// public literal is rejected here, a hostname at dial time). Hostnames are
// additionally re-checked against their resolved addresses at dial time so a
// DNS record pointing into the LAN is rejected even when it looks public at
// configuration time.
func ValidateBarkServerURL(raw string, allowPrivate bool) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || len(trimmed) > barkMaxURLLength {
		return "", ErrBarkURLInvalid
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", ErrBarkURLInvalid
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", ErrBarkURLInvalid
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrBarkURLInvalid
	}
	if parsed.Scheme == "http" && !allowPrivate {
		// Public clear-text delivery of the device key is never acceptable;
		// http exists only for a deliberate LAN self-host.
		return "", ErrBarkURLDisallowed
	}
	if parsed.Path != "" {
		cleaned := parsed.Path
		for _, segment := range strings.Split(cleaned, "/") {
			if segment == ".." {
				return "", ErrBarkURLInvalid
			}
		}
	}
	host := parsed.Hostname()
	if host == "" {
		return "", ErrBarkURLInvalid
	}
	if port := parsed.Port(); port != "" {
		value, convErr := net.LookupPort("tcp", port)
		if convErr != nil || value < 1 || value > 65535 {
			return "", ErrBarkURLInvalid
		}
	}
	if addr, addrErr := netip.ParseAddr(host); addrErr == nil {
		if parsed.Scheme == "http" {
			// Clear-text is pinned to genuinely private destinations.
			if !barkAddressPrivate(addr) {
				return "", ErrBarkURLDisallowed
			}
		} else if !barkAddressAllowed(addr, allowPrivate) {
			return "", ErrBarkURLDisallowed
		}
	} else if strings.EqualFold(host, "localhost") && !allowPrivate {
		return "", ErrBarkURLDisallowed
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

// ValidateBarkDeviceKey applies a narrow character policy: Bark keys are
// opaque visible ASCII tokens. Control characters/whitespace and oversized
// values are refused so a key can never corrupt a request or a log line.
func ValidateBarkDeviceKey(key string) (string, error) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" || len(trimmed) > barkMaxKeyLength {
		return "", ErrBarkKeyInvalid
	}
	for _, r := range trimmed {
		if r < 0x21 || r > 0x7e {
			return "", ErrBarkKeyInvalid
		}
	}
	return trimmed, nil
}

// BarkKeyHint returns a redacted display value. Four or fewer characters are
// hidden entirely; longer keys reveal only the last four.
func BarkKeyHint(key string) string {
	if len(key) <= 4 {
		return "••••"
	}
	return "••••" + key[len(key)-4:]
}

// BarkCallText returns the concise localized title/body for an incoming call.
// The handle is the only call data included; no message body or credential.
func BarkCallText(locale, peer string) (string, string) {
	display := strings.TrimSpace(peer)
	language := strings.ToLower(strings.TrimSpace(locale))
	chinese := strings.HasPrefix(language, "zh")
	if display == "" {
		if chinese {
			display = "未知号码"
		} else {
			display = "Unknown number"
		}
	}
	if chinese {
		return "CallRelay 来电", display + " 正在呼叫"
	}
	return "CallRelay incoming call", "Calling from " + display
}

// BarkTestText returns the concise localized title/body for the settings
// screen's test notification.
func BarkTestText(locale string) (string, string) {
	chinese := strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "zh")
	if chinese {
		return "CallRelay 测试通知", "Bark 通知桥配置成功（测试）"
	}
	return "CallRelay test notification", "Bark bridge configured successfully (test)"
}

func barkAddressAllowed(addr netip.Addr, allowPrivate bool) bool {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if allowPrivate {
		return addr.IsValid() && !addr.IsUnspecified() && !addr.IsMulticast()
	}
	return addr.IsGlobalUnicast() && !addr.IsPrivate() && !addr.IsLoopback() &&
		!addr.IsLinkLocalUnicast() && !addr.IsLinkLocalMulticast()
}

// barkAddressPrivate reports whether an address is a LAN/loopback destination
// suitable for the explicit clear-text self-host mode.
func barkAddressPrivate(addr netip.Addr) bool {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if !addr.IsValid() {
		return false
	}
	return addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast()
}

// barkDialContext enforces the transport policy at connection time. When
// requirePrivate is set (http LAN mode) every resolved address must be
// private; otherwise, with LAN mode off, every address must be globally
// routable. In both guarded cases the connection targets a validated IP
// directly, closing the resolve-then-switch (DNS rebinding) gap.
func barkDialContext(base *net.Dialer, allowPrivate, requirePrivate bool) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if allowPrivate && !requirePrivate {
			// https with explicit LAN opt-in: both public and private
			// destinations are allowed. No policy bypass for clear text,
			// which always goes through the requirePrivate branch below.
			return base.DialContext(ctx, network, address)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("bark dial: %w", err)
		}
		var addresses []netip.Addr
		if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
			addresses = []netip.Addr{addr}
		} else {
			resolved, lookupErr := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if lookupErr != nil || len(resolved) == 0 {
				return nil, fmt.Errorf("bark dial: resolve %s: %w", host, lookupErr)
			}
			addresses = resolved
		}
		for _, addr := range addresses {
			if requirePrivate {
				if !barkAddressPrivate(addr) {
					return nil, ErrBarkURLDisallowed
				}
			} else if !barkAddressAllowed(addr, false) {
				return nil, ErrBarkURLDisallowed
			}
		}
		return base.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
	}
}

// Send posts one notification. The server URL and key are re-validated here
// as defense in depth, because callers may hold persisted settings from an
// older, laxer policy.
func (c *BarkClient) Send(ctx context.Context, target BarkTarget, notification BarkNotification) error {
	if c == nil {
		return errors.New("bark client is not configured")
	}
	base, err := ValidateBarkServerURL(target.ServerURL, c.AllowPrivate)
	if err != nil {
		return err
	}
	key, err := ValidateBarkDeviceKey(target.DeviceKey)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		DeviceKey string `json:"device_key"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		URL       string `json:"url,omitempty"`
		Group     string `json:"group,omitempty"`
		Level     string `json:"level,omitempty"`
	}{
		DeviceKey: key,
		Title:     strings.TrimSpace(notification.Title),
		Body:      strings.TrimSpace(notification.Body),
		URL:       strings.TrimSpace(notification.URL),
		Group:     strings.TrimSpace(notification.Group),
		Level:     strings.TrimSpace(notification.Level),
	})
	if err != nil {
		return errors.New("encode bark notification")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/push", bytes.NewReader(payload))
	if err != nil {
		return errors.New("create bark request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.clientFor(base).Do(request)
	if err != nil {
		// Never wrap the transport error verbatim: a URL may embed a path.
		if errors.Is(err, ErrBarkURLDisallowed) {
			return ErrBarkURLDisallowed
		}
		return errors.New("bark request failed")
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, barkResponseLimit))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("bark server returned HTTP %d", response.StatusCode)
	}
	// Bark answers {"code":200,...}. Delivery is only accepted on an explicit
	// 200 code: a 2xx with a malformed, empty or missing code response is
	// treated as not delivered, because silently claiming success would hide
	// a misconfigured proxy or an incompatible server.
	var envelope struct {
		Code *int `json:"code"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return errors.New("bark server returned an unrecognized response")
	}
	if envelope.Code == nil {
		return errors.New("bark server response is missing code")
	}
	if *envelope.Code != 200 {
		return fmt.Errorf("bark server rejected notification (code %d)", *envelope.Code)
	}
	return nil
}

// BarkDeduper reserves one notification per (device, call) so overlapping
// worker snapshots or a reconnect cannot ring the same call twice. Entries
// expire after TTL; a missing entry only costs one extra notification.
type BarkDeduper struct {
	mu   sync.Mutex
	ttl  time.Duration
	seen map[string]time.Time
}

func NewBarkDeduper(ttl time.Duration) *BarkDeduper {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &BarkDeduper{ttl: ttl, seen: map[string]time.Time{}}
}

func BarkDedupKey(deviceID, callID string) string { return deviceID + "|" + callID }

// Reserve reports whether this (device, call) pair may notify now. The first
// caller wins; later callers within TTL are suppressed.
func (d *BarkDeduper) Reserve(deviceID, callID string) bool {
	if d == nil {
		return true
	}
	return d.reserveAt(deviceID, callID, time.Now())
}

func (d *BarkDeduper) reserveAt(deviceID, callID string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	cutoff := now.Add(-d.ttl)
	for key, at := range d.seen {
		if at.Before(cutoff) {
			delete(d.seen, key)
		}
	}
	key := BarkDedupKey(deviceID, callID)
	if at, ok := d.seen[key]; ok && at.After(cutoff) {
		return false
	}
	d.seen[key] = now
	return true
}

// Forget releases a reservation (used when a send is skipped before any
// provider call, so a genuine retry can still notify).
func (d *BarkDeduper) Forget(deviceID, callID string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.seen, BarkDedupKey(deviceID, callID))
}
