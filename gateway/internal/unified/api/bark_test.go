package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/push"
	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/store"
)

type barkAPIHarness struct {
	server   *httptest.Server
	api      *Server
	store    *store.Store
	token    string
	deviceID string
}

func newBarkAPIHarness(t *testing.T, mutate func(*config.Config)) *barkAPIHarness {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Gateway.ID = "gw-bark"
	cfg.Gateway.Name = "Bark Test"
	cfg.Gateway.DataDir = dir
	cfg.Gateway.Listen = "127.0.0.1:0"
	cfg.Voicemail.AudioRoot = filepath.Join(dir, "sd", "voicemail")
	cfg.Voicemail.MountPoint = filepath.Join(dir, "sd")
	cfg.Voicemail.ExpectedDevice = "/dev/mmcblk0"
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	database, err := store.Open(filepath.Join(dir, "unified.sqlite"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	gatewayIdentity, err := identity.LoadOrCreate(filepath.Join(dir, "gateway.key"), cfg.Gateway.ID, cfg.Gateway.Name)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	authService := auth.New(database, gatewayIdentity, time.Minute, time.Hour)
	keyID, secret, err := authService.CreateKey(context.Background(), "bark-test", true)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	message := keyID + "\n" + secret + "\n" + gatewayIdentity.ID + "\n" + "Test iPhone"
	proof := ed25519.Sign(privateKey, []byte(message))
	pair, err := authService.Enroll(context.Background(), auth.EnrollmentRequest{
		EnrollmentKey: keyID + "." + secret, DeviceName: "Test iPhone",
		DevicePublicKey: publicKey, Proof: proof,
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	apiServer := NewServer(cfg, authService, database, nil, nil, gatewayIdentity)
	server := httptest.NewServer(apiServer.Handler())
	t.Cleanup(func() {
		server.Close()
		_ = database.Close()
	})
	return &barkAPIHarness{server: server, api: apiServer, store: database, token: pair.AccessToken, deviceID: pair.DeviceID}
}

func (h *barkAPIHarness) request(t *testing.T, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	return response.StatusCode, payload
}

func (h *barkAPIHarness) path(suffix string) string {
	return "/api/v2/devices/" + h.deviceID + suffix
}

func TestBarkSettingsRoundTripRedactsSecret(t *testing.T) {
	h := newBarkAPIHarness(t, func(cfg *config.Config) { cfg.Bark.Enabled = true })
	status, body := h.request(t, http.MethodPut, h.path("/bark"), map[string]any{
		"enabled": true, "serverUrl": "https://api.day.app/", "deviceKey": "sekret-key-9876",
	})
	if status != http.StatusOK {
		t.Fatalf("put status %d: %s", status, body)
	}
	if strings.Contains(string(body), "sekret-key-9876") {
		t.Fatal("PUT response must never echo the device key")
	}
	var view struct {
		Enabled       bool   `json:"enabled"`
		ServerURL     string `json:"serverUrl"`
		KeyConfigured bool   `json:"keyConfigured"`
		KeyHint       string `json:"keyHint"`
		AllowPrivate  bool   `json:"allowPrivate"`
	}
	if err := json.Unmarshal(body, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !view.Enabled || view.ServerURL != "https://api.day.app" || !view.KeyConfigured {
		t.Fatalf("unexpected view: %+v", view)
	}
	if view.KeyHint != "••••9876" {
		t.Fatalf("hint must reveal only the last four: %q", view.KeyHint)
	}

	status, body = h.request(t, http.MethodGet, h.path("/bark"), nil)
	if status != http.StatusOK || strings.Contains(string(body), "sekret-key-9876") {
		t.Fatalf("GET status %d body %s", status, body)
	}
	// Redaction is presentation-only: the notifier path still holds the key.
	stored, err := h.store.GetDeviceBark(context.Background(), h.deviceID)
	if err != nil || stored.DeviceKey != "sekret-key-9876" {
		t.Fatalf("stored key missing: %+v %v", stored, err)
	}
	targets, err := h.store.ListBarkTargets(context.Background())
	if err != nil || len(targets) != 1 {
		t.Fatalf("notifier target: %+v %v", targets, err)
	}
}

func TestBarkSettingsLanPolicyIsExplicitOnBothSides(t *testing.T) {
	// Operator has not allowed LAN servers: device opt-in is refused.
	h := newBarkAPIHarness(t, func(cfg *config.Config) { cfg.Bark.Enabled = true })
	status, body := h.request(t, http.MethodPut, h.path("/bark"), map[string]any{
		"enabled": true, "serverUrl": "http://192.168.1.10:8080", "deviceKey": "key-12345", "allowPrivate": true,
	})
	if status != http.StatusForbidden || !strings.Contains(string(body), "CB-BARK-LAN-DISABLED") {
		t.Fatalf("expected operator LAN policy refusal, got %d %s", status, body)
	}

	// Operator allows LAN but the device has not opted in: refused.
	h2 := newBarkAPIHarness(t, func(cfg *config.Config) {
		cfg.Bark.Enabled = true
		cfg.Bark.AllowPrivate = true
	})
	status, body = h2.request(t, http.MethodPut, h2.path("/bark"), map[string]any{
		"enabled": true, "serverUrl": "http://192.168.1.10:8080", "deviceKey": "key-12345", "allowPrivate": false,
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "CB-BARK-LAN") {
		t.Fatalf("expected per-device LAN refusal, got %d %s", status, body)
	}

	// Both sides opt in: accepted, but only over http for the LAN address.
	status, body = h2.request(t, http.MethodPut, h2.path("/bark"), map[string]any{
		"enabled": true, "serverUrl": "http://192.168.1.10:8080", "deviceKey": "key-12345", "allowPrivate": true,
	})
	if status != http.StatusOK || !strings.Contains(string(body), "192.168.1.10") {
		t.Fatalf("expected accepted LAN settings, got %d %s", status, body)
	}
}

func TestBarkSettingsDisabledGatewayStillReportsState(t *testing.T) {
	h := newBarkAPIHarness(t, nil)
	status, body := h.request(t, http.MethodPut, h.path("/bark"), map[string]any{
		"enabled": true, "serverUrl": "https://api.day.app", "deviceKey": "key-12345",
	})
	if status != http.StatusConflict || !strings.Contains(string(body), "CB-BARK-DISABLED") {
		t.Fatalf("disabled gateway must refuse configuration, got %d %s", status, body)
	}
	status, body = h.request(t, http.MethodGet, h.path("/bark"), nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"gatewayEnabled":false`) {
		t.Fatalf("GET must expose the operator switch, got %d %s", status, body)
	}
}

func TestBarkSettingsRequiresCompleteMaterialWhenEnabled(t *testing.T) {
	h := newBarkAPIHarness(t, func(cfg *config.Config) { cfg.Bark.Enabled = true })
	status, body := h.request(t, http.MethodPut, h.path("/bark"), map[string]any{
		"enabled": true, "serverUrl": "https://api.day.app",
	})
	if status != http.StatusBadRequest || !strings.Contains(string(body), "CB-BARK-EMPTY") {
		t.Fatalf("missing key must be refused, got %d %s", status, body)
	}
	// Keeping the existing key on a later save is allowed when omitted.
	if status, body = h.request(t, http.MethodPut, h.path("/bark"), map[string]any{
		"enabled": true, "serverUrl": "https://api.day.app", "deviceKey": "key-12345",
	}); status != http.StatusOK {
		t.Fatalf("initial save failed: %d %s", status, body)
	}
	if status, body = h.request(t, http.MethodPut, h.path("/bark"), map[string]any{
		"enabled": false, "serverUrl": "https://api.day.app",
	}); status != http.StatusOK {
		t.Fatalf("disable without key failed: %d %s", status, body)
	}
	if status, body = h.request(t, http.MethodPut, h.path("/bark"), map[string]any{
		"enabled": true, "serverUrl": "https://api.day.app",
	}); status != http.StatusOK {
		t.Fatalf("re-enable with stored key failed: %d %s", status, body)
	}
}

func TestBarkSettingsOnlyForOwnDevice(t *testing.T) {
	h := newBarkAPIHarness(t, func(cfg *config.Config) { cfg.Bark.Enabled = true })
	status, body := h.request(t, http.MethodPut, "/api/v2/devices/dev_someone_else/bark", map[string]any{
		"enabled": true, "serverUrl": "https://api.day.app", "deviceKey": "key-12345",
	})
	if status != http.StatusForbidden {
		t.Fatalf("another device's settings must be forbidden, got %d %s", status, body)
	}
}

func TestBarkTestEndpointSendsThroughConfiguredServer(t *testing.T) {
	var received struct {
		DeviceKey string `json:"device_key"`
		URL       string `json:"url"`
		Title     string `json:"title"`
	}
	var hits int
	barkServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		payload, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(payload, &received)
		_, _ = w.Write([]byte(`{"code":200}`))
	}))
	defer barkServer.Close()

	h := newBarkAPIHarness(t, func(cfg *config.Config) {
		cfg.Bark.Enabled = true
		cfg.Bark.AllowPrivate = true
	})
	h.api.Bark = push.NewBarkClient(true, 2*time.Second)
	status, body := h.request(t, http.MethodPut, h.path("/bark"), map[string]any{
		"enabled": true, "serverUrl": barkServer.URL, "deviceKey": "key-abcdef",
		"allowPrivate": true,
	})
	if status != http.StatusOK {
		t.Fatalf("configure: %d %s", status, body)
	}
	status, body = h.request(t, http.MethodPost, h.path("/bark/test"), nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"sent":true`) {
		t.Fatalf("test send failed: %d %s", status, body)
	}
	if hits != 1 || received.DeviceKey != "key-abcdef" {
		t.Fatalf("bark server did not receive the notification: %+v hits=%d", received, hits)
	}
	if received.URL != push.BarkDeepLinkURL || strings.Contains(received.URL, "key-abcdef") {
		t.Fatalf("deeplink must be static and token-free: %q", received.URL)
	}
	// Per-device cooldown prevents using the gateway as a notification relay.
	status, body = h.request(t, http.MethodPost, h.path("/bark/test"), nil)
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "CB-BARK-429") {
		t.Fatalf("test must be rate limited, got %d %s", status, body)
	}
}

func TestBarkSettingsValidationNeverEchoesInput(t *testing.T) {
	h := newBarkAPIHarness(t, func(cfg *config.Config) {
		cfg.Bark.Enabled = true
		cfg.Bark.AllowPrivate = true
	})
	const secret = "sekret-key-4455"
	cases := []map[string]any{
		{"enabled": true, "serverUrl": "https://user:pass@api.day.app", "deviceKey": secret, "allowPrivate": false},
		{"enabled": true, "serverUrl": "http://127.0.0.1:9", "deviceKey": secret, "allowPrivate": false},
		{"enabled": true, "serverUrl": "file:///etc/passwd", "deviceKey": secret},
		{"enabled": true, "serverUrl": "https://api.day.app", "deviceKey": "bad key"},
	}
	for _, payload := range cases {
		status, body := h.request(t, http.MethodPut, h.path("/bark"), payload)
		if status == http.StatusOK {
			t.Fatalf("hostile payload accepted: %v", payload)
		}
		if strings.Contains(string(body), secret) {
			t.Fatalf("error response leaked the key: %s", body)
		}
	}
}
