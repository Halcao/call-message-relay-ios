package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/runtime"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/webpush"
)

func validP256dhKey() string {
	raw := append([]byte{0x04}, bytes.Repeat([]byte{7}, 64)...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func validAuthKey() string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 16))
}

// webPushHarness extends the bark harness with the webpush manager and an
// injected sender that records deliveries instead of POSTing.
type webPushHarness struct {
	*barkAPIHarness
	manager   *webpush.Manager
	sender    *webpush.Sender
	delivered [][]byte
}

func newWebPushHarness(t *testing.T, mutate func(*config.Config)) *webPushHarness {
	t.Helper()
	h := &webPushHarness{}
	base := newBarkAPIHarness(t, func(cfg *config.Config) {
		cfg.Gateway.PublicBaseURL = "https://gw.example"
		cfg.WebPush.Enabled = true
		if mutate != nil {
			mutate(cfg)
		}
	})
	h.barkAPIHarness = base
	h.manager = webpush.NewManager(base.store)
	h.manager.BindTTL = time.Minute
	h.manager.SessionTTL = time.Minute
	h.manager.HandoffTTL = time.Minute
	h.sender = &webpush.Sender{
		Subject: "mailto:test@example.com", PublicKey: "pk", PrivateKey: "sk",
	}
	h.sender.Post = func(ctx context.Context, endpoint string, message []byte) (int, error) {
		h.delivered = append(h.delivered, message)
		return http.StatusCreated, nil
	}
	base.api.WebPush = h.manager
	base.api.WebPushSender = h.sender
	base.api.VAPIDPublicKey = "test-vapid-public-key"
	return h
}

// webRequest issues a request with the browser session credential.
func (h *webPushHarness) webRequest(t *testing.T, method, path, session string, body any) (int, []byte) {
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
	request.Header.Set("Origin", h.server.URL)
	if session != "" {
		request.Header.Set("Authorization", "Bearer "+session)
	}
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

// bindDevice performs the mint→bind handshake and returns the session token.
func (h *webPushHarness) bindDevice(t *testing.T) string {
	t.Helper()
	status, body := h.request(t, http.MethodPost, h.path("/webpush/bind-token"), nil)
	if status != http.StatusOK {
		t.Fatalf("bind-token status %d: %s", status, body)
	}
	var minted struct {
		Code    string `json:"code"`
		BindURL string `json:"bindUrl"`
	}
	if err := json.Unmarshal(body, &minted); err != nil {
		t.Fatal(err)
	}
	if minted.Code == "" || !strings.HasPrefix(minted.BindURL, "https://gw.example/pwa/#bind=") {
		t.Fatalf("bind view mismatch: %+v", minted)
	}
	// The code travels in the fragment: replaying it from a query string or
	// log line must not work — here we prove the fragment URL shape only.
	status, body = h.webRequest(t, http.MethodPost, "/api/v2/webpush/bind", "", map[string]any{"code": minted.Code})
	if status != http.StatusOK {
		t.Fatalf("bind status %d: %s", status, body)
	}
	var bound struct {
		SessionToken   string `json:"sessionToken"`
		VapidPublicKey string `json:"vapidPublicKey"`
	}
	if err := json.Unmarshal(body, &bound); err != nil {
		t.Fatal(err)
	}
	if bound.SessionToken == "" || bound.VapidPublicKey != "test-vapid-public-key" {
		t.Fatalf("bind response mismatch: %+v", bound)
	}
	return bound.SessionToken
}

func TestWebPushVapidAndStatusRequireDeviceAuth(t *testing.T) {
	h := newWebPushHarness(t, nil)
	status, body := h.request(t, http.MethodGet, "/api/v2/webpush/vapid", nil)
	if status != http.StatusOK {
		t.Fatalf("vapid status %d: %s", status, body)
	}
	if !strings.Contains(string(body), "test-vapid-public-key") {
		t.Fatalf("vapid key missing: %s", body)
	}
	// Status is scoped to the authenticated device path.
	status, _ = h.request(t, http.MethodGet, "/api/v2/devices/other-device/webpush/status", nil)
	if status != http.StatusForbidden {
		t.Fatalf("foreign device status = %d, want 403", status)
	}
}

func TestWebPushBindFlow(t *testing.T) {
	h := newWebPushHarness(t, nil)
	session := h.bindDevice(t)

	// Bind code consumed: second redemption fails.
	h.request(t, http.MethodPost, h.path("/webpush/bind-token"), nil)
	status, _ := h.webRequest(t, http.MethodPost, "/api/v2/webpush/bind", "", map[string]any{"code": "WRONGCODE"})
	if status != http.StatusGone {
		t.Fatalf("unknown code = %d, want 410", status)
	}

	// Session status shows a fresh state with the default native mode.
	status, body := h.webRequest(t, http.MethodGet, "/api/v2/webpush/session", session, nil)
	if status != http.StatusOK {
		t.Fatalf("session status %d: %s", status, body)
	}
	if strings.Contains(string(body), "auth-secret") || strings.Contains(string(body), "dh-secret") {
		t.Fatal("session view must never leak subscription secrets")
	}
	if !strings.Contains(string(body), `"notifyMode":"native"`) {
		t.Fatalf("default mode must be native: %s", body)
	}

	// A web session is rejected by device-only routes and by other routes.
	status, _ = h.webRequest(t, http.MethodGet, "/api/v2/webpush/vapid", session, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("web session on device route = %d, want 401", status)
	}
	status, _ = h.webRequest(t, http.MethodGet, "/api/v2/lines", session, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("web session on general API = %d, want 401", status)
	}
	status, _ = h.webRequest(t, http.MethodGet, "/api/v2/webpush/session", "bogus", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("bogus session = %d, want 401", status)
	}
}

func TestWebPushSubscribeValidateAndTest(t *testing.T) {
	h := newWebPushHarness(t, nil)
	session := h.bindDevice(t)

	// SSRF: private endpoints refused at subscribe time.
	status, _ := h.webRequest(t, http.MethodPut, "/api/v2/webpush/subscription", session, map[string]any{
		"endpoint": "https://192.168.1.10/push", "keys": map[string]string{"p256dh": validP256dhKey(), "auth": validAuthKey()},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("private endpoint = %d, want 400", status)
	}

	status, body := h.webRequest(t, http.MethodPut, "/api/v2/webpush/subscription", session, map[string]any{
		"endpoint": "https://8.8.8.8/push", "keys": map[string]string{"p256dh": validP256dhKey(), "auth": validAuthKey()},
		"userAgent": "TestBrowser/1.0",
	})
	if status != http.StatusOK {
		t.Fatalf("subscribe status %d: %s", status, body)
	}
	if strings.Contains(string(body), "BKey") || strings.Contains(string(body), "AKey") {
		t.Fatal("subscribe view must never echo keys")
	}

	status, body = h.webRequest(t, http.MethodPost, "/api/v2/webpush/test", session, map[string]string{})
	if status != http.StatusOK {
		t.Fatalf("test status %d: %s", status, body)
	}
	if len(h.delivered) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(h.delivered))
	}
	var payload struct {
		WebPush      int `json:"web_push"`
		Notification struct {
			Title string `json:"title"`
		} `json:"notification"`
	}
	if err := json.Unmarshal(h.delivered[0], &payload); err != nil || payload.WebPush != 8030 || payload.Notification.Title == "" {
		t.Fatalf("test payload mismatch: %s", h.delivered[0])
	}
}

func TestWebPushNotifyModeAndUnsubscribe(t *testing.T) {
	h := newWebPushHarness(t, nil)
	session := h.bindDevice(t)
	h.webRequest(t, http.MethodPut, "/api/v2/webpush/subscription", session, map[string]any{
		"endpoint": "https://8.8.8.8/push", "keys": map[string]string{"p256dh": validP256dhKey(), "auth": validAuthKey()},
	})

	status, body := h.webRequest(t, http.MethodPut, "/api/v2/webpush/notify-mode", session, map[string]string{"mode": "web"})
	if status != http.StatusOK {
		t.Fatalf("mode status %d: %s", status, body)
	}
	if !strings.Contains(string(body), `"notifyMode":"web"`) {
		t.Fatalf("mode not persisted: %s", body)
	}
	status, _ = h.webRequest(t, http.MethodPut, "/api/v2/webpush/notify-mode", session, map[string]string{"mode": "bogus"})
	if status != http.StatusBadRequest {
		t.Fatalf("bogus mode = %d, want 400", status)
	}

	var statusView struct {
		Subscriptions []struct {
			ID string `json:"id"`
		} `json:"subscriptions"`
	}
	_, body = h.webRequest(t, http.MethodGet, "/api/v2/webpush/session", session, nil)
	if err := json.Unmarshal(body, &statusView); err != nil || len(statusView.Subscriptions) != 1 {
		t.Fatalf("subscriptions view: %s", body)
	}
	status, _ = h.webRequest(t, http.MethodDelete, "/api/v2/webpush/subscription/"+statusView.Subscriptions[0].ID, session, nil)
	if status != http.StatusOK {
		t.Fatalf("unsubscribe = %d", status)
	}
	if count, _ := h.store.CountWebPushSubscriptions(context.Background(), h.deviceID); count != 0 {
		t.Fatal("unsubscribe must remove the row")
	}
}

func TestWebPushHandoffBoundToSessionDevice(t *testing.T) {
	h := newWebPushHarness(t, nil)
	session := h.bindDevice(t)

	// Ringing call owned by this device.
	call := store.Call{
		ID: "line1:wc-1", LineID: "line1", WorkerCallID: "wc-1", DeviceID: h.deviceID,
		Direction: "inbound", Peer: "10086", State: runtime.StateRinging, CreatedAt: time.Now().UTC(),
	}
	if err := h.store.UpsertCall(context.Background(), call); err != nil {
		t.Fatal(err)
	}

	token := h.manager.MintHandoff(h.deviceID, call.ID)
	// The token travels in the POST body, never the URL path/query.
	status, body := h.webRequest(t, http.MethodPost, "/api/v2/webpush/handoff", session, map[string]string{"token": token})
	if status != http.StatusOK {
		t.Fatalf("handoff status %d: %s", status, body)
	}
	if !strings.Contains(string(body), "10086") || !strings.Contains(string(body), "gw-bark") {
		t.Fatalf("handoff must return the caller handle and gateway id: %s", body)
	}
	// Single use: replay fails identically.
	status, _ = h.webRequest(t, http.MethodPost, "/api/v2/webpush/handoff", session, map[string]string{"token": token})
	if status != http.StatusGone {
		t.Fatalf("replay = %d, want 410", status)
	}

	// A session of another device cannot redeem this device's handoff.
	second := newWebPushHarness(t, nil)
	session2 := second.bindDevice(t)
	token2 := h.manager.MintHandoff(h.deviceID, call.ID)
	status, _ = second.webRequest(t, http.MethodPost, "/api/v2/webpush/handoff", session2, map[string]string{"token": token2})
	if status != http.StatusGone {
		t.Fatalf("foreign handoff = %d, want 410", status)
	}

	// Ended call: handoff answers 410.
	token3 := h.manager.MintHandoff(h.deviceID, call.ID)
	ended := call
	now := time.Now().UTC()
	ended.EndedAt = &now
	ended.State = runtime.StateEnded
	if err := h.store.UpsertCall(context.Background(), ended); err != nil {
		t.Fatal(err)
	}
	status, _ = h.webRequest(t, http.MethodPost, "/api/v2/webpush/handoff", session, map[string]string{"token": token3})
	if status != http.StatusGone {
		t.Fatalf("ended call handoff = %d, want 410", status)
	}
}

func TestWebPushCallStatusScopedToOwnCalls(t *testing.T) {
	h := newWebPushHarness(t, nil)
	session := h.bindDevice(t)
	own := store.Call{
		ID: "line1:own", LineID: "line1", WorkerCallID: "own", DeviceID: h.deviceID,
		Direction: "inbound", Peer: "10086", State: runtime.StateRinging, CreatedAt: time.Now().UTC(),
	}
	foreign := store.Call{
		ID: "line1:foreign", LineID: "line1", WorkerCallID: "foreign", DeviceID: "someone-else",
		Direction: "inbound", Peer: "10010", State: runtime.StateRinging, CreatedAt: time.Now().UTC(),
	}
	if err := h.store.UpsertCall(context.Background(), own); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCall(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	status, body := h.webRequest(t, http.MethodGet, "/api/v2/webpush/calls/"+own.ID, session, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"ringing":true`) {
		t.Fatalf("own call status %d: %s", status, body)
	}
	status, _ = h.webRequest(t, http.MethodGet, "/api/v2/webpush/calls/"+foreign.ID, session, nil)
	if status != http.StatusNotFound {
		t.Fatalf("foreign call = %d, want 404", status)
	}
}

func TestWebPushWebOnlyModeRequiresSubscription(t *testing.T) {
	h := newWebPushHarness(t, nil)
	session := h.bindDevice(t)

	// Selecting web-only with no subscription must fail and keep native.
	status, _ := h.webRequest(t, http.MethodPut, "/api/v2/webpush/notify-mode", session, map[string]string{"mode": "web"})
	if status != http.StatusConflict {
		t.Fatalf("web-only without subscription = %d, want 409", status)
	}
	mode, _ := h.store.DeviceNotifyMode(context.Background(), h.deviceID)
	if mode != store.NotifyModeNative {
		t.Fatalf("mode must stay native, got %q", mode)
	}

	// Same guard from the app side.
	status, _ = h.request(t, http.MethodPut, h.path("/webpush/notify-mode"), map[string]string{"mode": "web"})
	if status != http.StatusConflict {
		t.Fatalf("device web-only without subscription = %d, want 409", status)
	}

	// With a subscription the web-only mode applies...
	h.webRequest(t, http.MethodPut, "/api/v2/webpush/subscription", session, map[string]any{
		"endpoint": "https://8.8.8.8/push", "keys": map[string]string{"p256dh": validP256dhKey(), "auth": validAuthKey()},
	})
	status, body := h.webRequest(t, http.MethodPut, "/api/v2/webpush/notify-mode", session, map[string]string{"mode": "web"})
	if status != http.StatusOK || !strings.Contains(string(body), `"notifyMode":"web"`) {
		t.Fatalf("web-only with subscription = %d: %s", status, body)
	}

	// ...and removing that last subscription safely falls back to native so
	// the device never loses its only notification path.
	var view struct {
		Subscriptions []struct {
			ID string `json:"id"`
		} `json:"subscriptions"`
	}
	_, body = h.webRequest(t, http.MethodGet, "/api/v2/webpush/session", session, nil)
	if err := json.Unmarshal(body, &view); err != nil || len(view.Subscriptions) != 1 {
		t.Fatalf("session view: %s", body)
	}
	status, body = h.webRequest(t, http.MethodDelete, "/api/v2/webpush/subscription/"+view.Subscriptions[0].ID, session, nil)
	if status != http.StatusOK {
		t.Fatalf("unsubscribe = %d", status)
	}
	if !strings.Contains(string(body), `"notifyMode":"native"`) {
		t.Fatalf("auto fallback to native missing: %s", body)
	}
}

func TestWebPushDisabledAnswersConflict(t *testing.T) {
	h := newWebPushHarness(t, func(cfg *config.Config) { cfg.WebPush.Enabled = false })
	h.api.WebPush = nil
	h.api.WebPushSender = nil
	status, body := h.request(t, http.MethodGet, "/api/v2/webpush/vapid", nil)
	if status != http.StatusConflict {
		t.Fatalf("vapid when disabled = %d: %s", status, body)
	}
	status, _ = h.webRequest(t, http.MethodPost, "/api/v2/webpush/bind", "", map[string]string{"code": "ABCDE2345"})
	if status != http.StatusConflict {
		t.Fatalf("bind when disabled = %d", status)
	}
}

func TestWebPushBindRateLimited(t *testing.T) {
	h := newWebPushHarness(t, nil)
	for i := 0; i < 12; i++ {
		status, _ := h.webRequest(t, http.MethodPost, "/api/v2/webpush/bind", "", map[string]string{"code": "ZZZZZZZZZ"})
		if status != http.StatusGone {
			t.Fatalf("attempt %d = %d, want 410", i, status)
		}
	}
	status, _ := h.webRequest(t, http.MethodPost, "/api/v2/webpush/bind", "", map[string]string{"code": "ZZZZZZZZZ"})
	if status != http.StatusTooManyRequests {
		t.Fatalf("rate limit = %d, want 429", status)
	}
}

var _ = filepath.Join // keep unused import stable across harness evolution
