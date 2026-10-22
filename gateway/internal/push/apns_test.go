package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func testKeyFile(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "AuthKey_TEST.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path, key
}

func testSender(t *testing.T, capture func(*http.Request), environment string) *APNsSender {
	t.Helper()
	keyPath, key := testKeyFile(t)
	sender, err := NewAPNsSender(APNsConfig{
		KeyFile: keyPath, KeyID: "KEY123", TeamID: "TEAM123",
		BundleID: "com.example.app", Environment: environment,
	})
	if err != nil {
		t.Fatalf("NewAPNsSender: %v", err)
	}
	fixed := time.Date(2026, 10, 3, 5, 0, 0, 0, time.UTC)
	sender.now = func() time.Time { return fixed }
	sender.key = key
	if capture != nil {
		sender.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			capture(request)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
			}, nil
		})}
	}
	return sender
}

func TestNewAPNsSenderValidatesConfig(t *testing.T) {
	keyPath, _ := testKeyFile(t)
	cases := []struct {
		name   string
		config APNsConfig
	}{
		{"missing key", APNsConfig{KeyID: "k", TeamID: "t", BundleID: "b"}},
		{"missing key id", APNsConfig{KeyFile: keyPath, TeamID: "t", BundleID: "b"}},
		{"missing team", APNsConfig{KeyFile: keyPath, KeyID: "k", BundleID: "b"}},
		{"missing bundle", APNsConfig{KeyFile: keyPath, KeyID: "k", TeamID: "t"}},
		{"bad environment", APNsConfig{KeyFile: keyPath, KeyID: "k", TeamID: "t", BundleID: "b", Environment: "staging"}},
	}
	for _, testCase := range cases {
		if _, err := NewAPNsSender(testCase.config); err == nil {
			t.Fatalf("%s: expected an error", testCase.name)
		}
	}
}

func TestAPNsHostsAndTopic(t *testing.T) {
	sandbox := testSender(t, nil, "sandbox")
	if sandbox.baseURL != apnsSandboxHost {
		t.Fatalf("sandbox host = %s", sandbox.baseURL)
	}
	production := testSender(t, nil, "production")
	if production.baseURL != apnsProductionHost {
		t.Fatalf("production host = %s", production.baseURL)
	}
	if topic := production.config.BundleID + ".voip"; topic != "com.example.app.voip" {
		t.Fatalf("unexpected topic %s", topic)
	}
}

func TestProviderTokenIsVerifiableES256(t *testing.T) {
	sender := testSender(t, nil, "production")
	token, err := sender.providerToken(sender.now())
	if err != nil {
		t.Fatalf("providerToken: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token must have three parts, got %d", len(parts))
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	var header map[string]string
	if json.Unmarshal(headerBytes, &header) != nil || header["alg"] != "ES256" || header["kid"] != "KEY123" {
		t.Fatalf("unexpected header %s", headerBytes)
	}
	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		t.Fatalf("decode claims json: %v", err)
	}
	if claims["iss"] != "TEAM123" {
		t.Fatalf("iss = %v", claims["iss"])
	}
	if _, ok := claims["iat"].(float64); !ok {
		t.Fatalf("iat missing")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		t.Fatalf("signature length %d err %v", len(signature), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(&sender.key.PublicKey, digest[:], r, s) {
		t.Fatalf("token signature did not verify")
	}
}

func TestSendVoIPRequestShape(t *testing.T) {
	var captured *http.Request
	var body []byte
	sender := testSender(t, func(request *http.Request) {
		captured = request
		body, _ = io.ReadAll(request.Body)
	}, "sandbox")
	err := sender.SendVoIP(context.Background(), Target{
		DeviceID: "dev1", VoIPToken: "voip-token-1", Environment: "sandbox",
	}, VoIPPayload{
		CallUUID: "uuid-1", CallID: "line1:abc", Handle: "13800001111",
		GatewayID: "gw", LineID: "line1", IssuedAt: 123,
	})
	if err != nil {
		t.Fatalf("SendVoIP: %v", err)
	}
	if captured == nil {
		t.Fatal("no request captured")
	}
	if captured.URL.Path != "/3/device/voip-token-1" {
		t.Fatalf("path = %s", captured.URL.Path)
	}
	wantHeaders := map[string]string{
		"apns-topic":      "com.example.app.voip",
		"apns-push-type":  "voip",
		"apns-priority":   "10",
		"apns-expiration": "0", // immediate-or-drop: no stale ring
	}
	for header, want := range wantHeaders {
		if got := captured.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if auth := captured.Header.Get("authorization"); !strings.HasPrefix(auth, "bearer ") {
		t.Fatalf("authorization header missing bearer prefix")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if payload["callUUID"] != "uuid-1" || payload["callId"] != "line1:abc" || payload["gatewayId"] != "gw" {
		t.Fatalf("unexpected payload %s", body)
	}
	aps, ok := payload["aps"].(map[string]any)
	if !ok || aps["content-available"] != float64(1) {
		t.Fatalf("VoIP envelope must carry aps.content-available=1: %s", body)
	}
}

func TestSendMessageUsesAlertPush(t *testing.T) {
	var captured *http.Request
	sender := testSender(t, func(request *http.Request) { captured = request }, "production")
	err := sender.SendMessage(context.Background(), Target{
		DeviceID: "dev1", APNSToken: "alert-token-1", Environment: "production",
	}, MessagePayload{MessageID: "m1", GatewayID: "gw", LineID: "line1", IssuedAt: 1})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if captured.URL.Path != "/3/device/alert-token-1" {
		t.Fatalf("path = %s", captured.URL.Path)
	}
	if captured.Header.Get("apns-topic") != "com.example.app" {
		t.Fatalf("topic = %s", captured.Header.Get("apns-topic"))
	}
	if captured.Header.Get("apns-push-type") != "alert" || captured.Header.Get("apns-priority") != "5" {
		t.Fatalf("alert push headers wrong: %v", captured.Header)
	}
	var body map[string]any
	payloadBytes, _ := io.ReadAll(captured.Body)
	if err := json.Unmarshal(payloadBytes, &body); err != nil {
		t.Fatalf("alert payload json: %v", err)
	}
	aps, ok := body["aps"].(map[string]any)
	if !ok {
		t.Fatalf("alert envelope missing aps: %s", payloadBytes)
	}
	if _, ok := aps["alert"].(map[string]any); !ok || aps["sound"] != "default" {
		t.Fatalf("alert envelope must carry aps.alert and aps.sound: %s", payloadBytes)
	}
	if body["messageId"] != "m1" || body["gatewayId"] != "gw" {
		t.Fatalf("custom fields lost: %s", payloadBytes)
	}
}

func TestAPNsEnvironmentRoutingAndMismatch(t *testing.T) {
	sender := testSender(t, nil, "production")
	if host, err := sender.apnsHost("production"); err != nil || host != apnsProductionHost {
		t.Fatalf("production host = %s err=%v", host, err)
	}
	if _, err := sender.apnsHost("sandbox"); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("sandbox target against production config must be rejected, got %v", err)
	}
	if _, err := sender.apnsHost("staging"); err == nil {
		t.Fatalf("invalid target environment must be rejected")
	}

	var captured *http.Request
	sandbox := testSender(t, func(request *http.Request) { captured = request }, "sandbox")
	if err := sandbox.SendVoIP(context.Background(), Target{
		DeviceID: "dev", VoIPToken: "tok", Environment: "sandbox",
	}, VoIPPayload{CallUUID: "u", CallID: "c", GatewayID: "g"}); err != nil {
		t.Fatalf("sandbox send: %v", err)
	}
	if captured.URL.Host != "api.sandbox.push.apple.com" {
		t.Fatalf("sandbox target went to %s", captured.URL.Host)
	}
}

func TestAPNsKeyMustBeP256(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "AuthKey_P384.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if _, err := NewAPNsSender(APNsConfig{
		KeyFile: path, KeyID: "k", TeamID: "t", BundleID: "b", Environment: "sandbox",
	}); err == nil || !strings.Contains(err.Error(), "P-256") {
		t.Fatalf("non-P-256 key must be rejected, got %v", err)
	}
}

func TestAPNsPayloadLimit(t *testing.T) {
	sender := testSender(t, func(*http.Request) {}, "production")
	err := sender.SendMessage(context.Background(), Target{
		DeviceID: "dev", APNSToken: "tok", Environment: "production",
	}, MessagePayload{MessageID: strings.Repeat("x", 5000), GatewayID: "g"})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized payload must be rejected, got %v", err)
	}
}

func TestAPNsErrorIsPrivacySafe(t *testing.T) {
	sender := testSender(t, nil, "production")
	sender.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusGone,
			Body:       io.NopCloser(strings.NewReader(`{"reason":"BadDeviceToken"}`)),
			Header:     make(http.Header),
		}, nil
	})}
	err := sender.SendVoIP(context.Background(), Target{
		DeviceID: "dev1", VoIPToken: "super-secret-token", Environment: "production",
	}, VoIPPayload{CallUUID: "u", CallID: "c", GatewayID: "g"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "BadDeviceToken") || !strings.Contains(err.Error(), "410") {
		t.Fatalf("unexpected error %q", err)
	}
	if strings.Contains(err.Error(), "super-secret-token") {
		t.Fatalf("error leaked the device token: %q", err)
	}
}

func TestProviderTokenIsCached(t *testing.T) {
	authorizations := make([]string, 0, 2)
	sender := testSender(t, func(request *http.Request) {
		authorizations = append(authorizations, request.Header.Get("authorization"))
	}, "production")
	for i := 0; i < 2; i++ {
		_ = sender.SendVoIP(context.Background(), Target{
			DeviceID: "dev", VoIPToken: "token", Environment: "production",
		}, VoIPPayload{CallUUID: "u", CallID: "c", GatewayID: "g"})
	}
	if len(authorizations) != 2 || authorizations[0] != authorizations[1] {
		t.Fatalf("provider token must be cached: %v", authorizations)
	}
}
