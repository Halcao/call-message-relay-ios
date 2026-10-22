package webpush

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadOrCreateKeysRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "webpush", "vapid.pem")
	first, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if first.PublicKey == "" || first.PrivateKey == "" {
		t.Fatal("keys must not be empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %o, want 600", info.Mode().Perm())
	}
	second, err := LoadOrCreateKeys(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second.PublicKey != first.PublicKey || second.PrivateKey != first.PrivateKey {
		t.Fatal("reloaded keys differ from generated keys")
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("key dir mode = %o, want 700", dirInfo.Mode().Perm())
	}
}

func TestLoadOrCreateKeysRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vapid.pem")
	if err := os.WriteFile(path, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKeys(path); err == nil {
		t.Fatal("garbage key file must fail")
	}
}

func TestValidateEndpoint(t *testing.T) {
	var policy Policy
	valid := []string{
		"https://push.example.com/send/abc",
		"https://web.push.apple.com/xyz",
		"https://updates.push.services.mozilla.com/wpush/v2/abc",
		"https://fcm.googleapis.com/fcm/send/xyz",
		"https://8.8.8.8/x",
	}
	for _, raw := range valid {
		if _, err := policy.ValidateEndpoint(raw); err != nil {
			t.Fatalf("%s must be accepted: %v", raw, err)
		}
	}
	invalid := []string{
		"",
		"http://push.example.com/x",
		"ftp://push.example.com/x",
		// loopback / RFC1918 / link-local
		"https://127.0.0.1/x",
		"https://127.34.55.1/x",
		"https://10.0.0.5/x",
		"https://192.168.1.1/x",
		"https://172.16.0.1/x",
		"https://169.254.1.1/x",
		"https://[::1]/x",
		"https://[fd00::1]/x",
		"https://[fe80::1]/x",
		// IPv4-mapped forms must unwrap then reject
		"https://[::ffff:192.168.1.1]/x",
		"https://[::ffff:10.1.2.3]/x",
		// special-use ranges still flagged global-unicast by netip
		"https://100.64.0.1/x",   // CGNAT shared space
		"https://100.127.255.254/x",
		"https://198.18.0.1/x",   // benchmarking
		"https://198.19.255.1/x",
		"https://192.0.0.9/x",    // RFC 6890 assignments
		"https://192.0.2.1/x",    // TEST-NET-1
		"https://198.51.100.7/x", // TEST-NET-2
		"https://203.0.113.9/x",  // TEST-NET-3
		"https://240.0.0.1/x",    // reserved
		"https://255.255.255.255/x",
		"https://[2001:db8::1]/x", // v6 documentation
		"https://[2001:2::1]/x",   // v6 benchmarking
		"https://[3fff::1]/x",     // v6 documentation (RFC 9637)
		// structural
		"https://user:pass@push.example.com/x",
		"https://8.8.8.8/x#frag",
		"https://push.example.com:99999/x",
		strings.Repeat("a", 2100),
	}
	for _, raw := range invalid {
		if _, err := policy.ValidateEndpoint(raw); err == nil {
			t.Fatalf("%s must be rejected", raw)
		}
	}
}

func TestEndpointPolicyAllowlistMode(t *testing.T) {
	policy := Policy{Allowlist: []string{"push.services.mozilla.com", "googleapis.com"}}
	if _, err := policy.ValidateEndpoint("https://updates.push.services.mozilla.com/wpush/v2/x"); err != nil {
		t.Fatalf("allowlisted host rejected: %v", err)
	}
	if _, err := policy.ValidateEndpoint("https://fcm.googleapis.com/fcm/send/x"); err != nil {
		t.Fatalf("suffix-allowlisted host rejected: %v", err)
	}
	if _, err := policy.ValidateEndpoint("https://evil-push.services.mozilla.com.attacker.io/x"); err == nil {
		t.Fatal("lookalike host must not match by suffix")
	}
	if _, err := policy.ValidateEndpoint("https://web.push.apple.com/x"); err == nil {
		t.Fatal("non-allowlisted host must fail in allowlist mode")
	}
}

func TestResolveEndpointHostMixedDNSFailsClosed(t *testing.T) {
	var policy Policy
	original := lookupIP
	defer func() { lookupIP = original }()
	// Mixed public + private answers must fail closed (rebinding defense).
	lookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{
			netip.MustParseAddr("8.8.8.8"),
			netip.MustParseAddr("192.168.1.50"),
		}, nil
	}
	if err := policy.ResolveEndpointHost(context.Background(), "https://flaky.example.com/push"); !errors.Is(err, ErrEndpointDisallowed) {
		t.Fatalf("mixed DNS must fail closed: %v", err)
	}
	// All-public answers pass.
	lookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}, nil
	}
	if err := policy.ResolveEndpointHost(context.Background(), "https://ok.example.com/push"); err != nil {
		t.Fatalf("public DNS must pass: %v", err)
	}
	// All-private answers fail.
	lookupIP = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.8")}, nil
	}
	if err := policy.ResolveEndpointHost(context.Background(), "https://lan.example.com/push"); !errors.Is(err, ErrEndpointDisallowed) {
		t.Fatalf("private DNS must fail: %v", err)
	}
}

func TestHardenedClientNeverFollowsRedirects(t *testing.T) {
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			w.Header().Set("Location", "https://8.8.8.8/target")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer redirector.Close()
	client := hardenedClient(Policy{allowLoopback: true}, 5*time.Second)
	request, err := http.NewRequest(http.MethodGet, redirector.URL+"/redirect", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect must not be followed, got %d", response.StatusCode)
	}
}

func TestValidateSubscriptionKeys(t *testing.T) {
	validP256dh := base64.RawURLEncoding.EncodeToString(append([]byte{0x04}, bytes.Repeat([]byte{7}, 64)...))
	if _, err := ValidateP256dh(validP256dh); err != nil {
		t.Fatalf("valid p256dh rejected: %v", err)
	}
	validAuth := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16))
	if _, err := ValidateAuth(validAuth); err != nil {
		t.Fatalf("valid auth rejected: %v", err)
	}
	// Padded base64url is also accepted (browsers may emit either).
	padded := base64.URLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16))
	if _, err := ValidateAuth(padded); err != nil {
		t.Fatalf("padded auth rejected: %v", err)
	}
	// Wrong sizes, wrong point format, garbage and wrong alphabets fail.
	bad := []string{
		base64.RawURLEncoding.EncodeToString(append([]byte{0x02}, bytes.Repeat([]byte{7}, 32)...)), // compressed
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 64)),                          // missing 0x04
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 33)),                          // short
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 96)),                          // long
		"not!base64!url!",
		base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16)), // non-URL alphabet
	}
	for _, raw := range bad {
		if _, err := ValidateP256dh(raw); err == nil {
			t.Fatalf("p256dh %q must be rejected", raw)
		}
	}
	for _, raw := range []string{
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 15)),
		base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 17)),
		"",
		"has space",
	} {
		if _, err := ValidateAuth(raw); err == nil {
			t.Fatalf("auth %q must be rejected", raw)
		}
	}
}

func TestCallPayloadShape(t *testing.T) {
	payload, err := CallPayload("zh-Hans", "10086", "https://gw.example/pwa/call#h=TOKEN", "call-1")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		WebPush      int `json:"web_push"`
		Notification struct {
			Title    string `json:"title"`
			Body     string `json:"body"`
			Navigate string `json:"navigate"`
			Tag      string `json:"tag"`
			Renotify bool   `json:"renotify"`
			Silent   bool   `json:"silent"`
		} `json:"notification"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.WebPush != 8030 {
		t.Fatalf("web_push = %d, want 8030", decoded.WebPush)
	}
	if decoded.Notification.Navigate != "https://gw.example/pwa/call#h=TOKEN" {
		t.Fatalf("navigate = %q", decoded.Notification.Navigate)
	}
	if decoded.Notification.Tag != "call-1" || !decoded.Notification.Renotify || decoded.Notification.Silent {
		t.Fatal("tag/renotify/silent mismatch")
	}
	if decoded.Notification.Title == "" || !strings.Contains(decoded.Notification.Body, "10086") {
		t.Fatal("call metadata missing")
	}
}

func TestTestPayloadLocaleAndNavigate(t *testing.T) {
	zh, err := TestPayload("zh-Hant", "https://gw.example/pwa/")
	if err != nil {
		t.Fatal(err)
	}
	en, err := TestPayload("en-US", "https://gw.example/pwa/")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(zh), "configured successfully") {
		t.Fatal("zh payload must not be English")
	}
	if !strings.Contains(string(en), "configured successfully") {
		t.Fatal("en payload must be English")
	}
	// The tap target must be a valid same-origin URL, never empty.
	var zhMsg, enMsg Message
	if err := json.Unmarshal(zh, &zhMsg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(en, &enMsg); err != nil {
		t.Fatal(err)
	}
	if zhMsg.Notification.Navigate != "https://gw.example/pwa/" ||
		enMsg.Notification.Navigate != "https://gw.example/pwa/" {
		t.Fatalf("test navigate target mismatch: %q / %q", zhMsg.Notification.Navigate, enMsg.Notification.Navigate)
	}
	if zhMsg.WebPush != 8030 || zhMsg.Notification.Title == "" {
		t.Fatal("test payload schema mismatch")
	}
}

func TestSenderStatusErrorAndGone(t *testing.T) {
	sender := &Sender{
		Subject: "mailto:t@example.com", PublicKey: "pk", PrivateKey: "sk",
		Post: func(ctx context.Context, endpoint string, message []byte) (int, error) {
			return http.StatusGone, nil
		},
	}
	err := sender.Send(context.Background(), Subscription{Endpoint: "https://127.0.0.1/x"}, []byte("{}"))
	if !errors.Is(err, ErrEndpointDisallowed) {
		t.Fatalf("SSRF endpoint must be refused before POST: %v", err)
	}
	sender.Post = func(ctx context.Context, endpoint string, message []byte) (int, error) {
		return http.StatusGone, nil
	}
	err = sender.Send(context.Background(), Subscription{Endpoint: "https://8.8.8.8/x"}, []byte("{}"))
	if !SubscriptionGone(err) {
		t.Fatalf("410 must be reported as gone: %v", err)
	}
	sender.Post = func(ctx context.Context, endpoint string, message []byte) (int, error) {
		return http.StatusCreated, nil
	}
	if err := sender.Send(context.Background(), Subscription{Endpoint: "https://8.8.8.8/x"}, []byte("{}")); err != nil {
		t.Fatalf("2xx must succeed: %v", err)
	}
	sender.Post = func(ctx context.Context, endpoint string, message []byte) (int, error) {
		return 0, errors.New("boom")
	}
	err = sender.Send(context.Background(), Subscription{Endpoint: "https://8.8.8.8/x"}, []byte("{}"))
	if err == nil || strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "8.8.8.8") {
		t.Fatalf("transport errors must be sanitized: %v", err)
	}
}

func TestHandoffLifecycle(t *testing.T) {
	manager := NewManager(nil)
	manager.HandoffTTL = 50 * time.Millisecond
	token := manager.MintHandoff("device-1", "call-1")
	entry, err := manager.ConsumeHandoff(token)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if entry.DeviceID != "device-1" || entry.CallID != "call-1" {
		t.Fatalf("entry mismatch: %+v", entry)
	}
	if _, err := manager.ConsumeHandoff(token); !errors.Is(err, ErrHandoffInvalid) {
		t.Fatal("replay must fail")
	}
	other := manager.MintHandoff("device-1", "call-2")
	manager.handoffs[other] = handoffEntry{DeviceID: "d", CallID: "c", Expires: time.Now().Add(-time.Second)}
	if _, err := manager.ConsumeHandoff(other); !errors.Is(err, ErrHandoffInvalid) {
		t.Fatal("expired handoff must fail")
	}
	if _, err := manager.ConsumeHandoff("nope"); !errors.Is(err, ErrHandoffInvalid) {
		t.Fatal("unknown handoff must fail")
	}
}
