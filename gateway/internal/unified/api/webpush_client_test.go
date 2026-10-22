package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/webpush"

	"github.com/gorilla/websocket"
)

// Standalone browser client authorization acceptance (real Manager, real
// lines, real fake workers):
//   - Only sessions minted with the explicit "client" scope reach the
//     call/SMS/lines APIs; notification-only ("push") sessions keep their
//     historical surface and are rejected everywhere (never silently
//     upgraded by the deployment).
//   - The cookie credential is origin-checked on plain routes AND WebSocket
//     upgrades (events + media): cross-site requests and foreign-Origin
//     upgrades are rejected.
//   - The browser session carries exactly the device's own line permissions:
//     a sendSms-denied line rejects the browser send.
//   - Revoking the session, the device key, or the device's line grant
//     immediately denies every surface.

// attachWebPush wires a webpush manager into the integration stack.
func (s *testStack) attachWebPush(t *testing.T) *webpush.Manager {
	t.Helper()
	manager := webpush.NewManager(s.store)
	manager.BindTTL = time.Minute
	manager.SessionTTL = time.Minute
	manager.HandoffTTL = time.Minute
	// The Server captured its config by value at construction; flip the
	// webpush gate on the live instance the same way the deployment config
	// would at boot.
	s.api.Cfg.WebPush.Enabled = true
	s.api.WebPush = manager
	s.api.WebPushSender = &webpush.Sender{Subject: "mailto:test@example.com", PublicKey: "pk", PrivateKey: "sk"}
	s.api.VAPIDPublicKey = "test-vapid-public-key"
	return manager
}

// mintBindCode mints a bind code for the stack's enrolled device using its
// device bearer (the standalone pairing step, owner-initiated in the app or
// console-derived enrollment flow).
func (s *testStack) mintBindCode(t *testing.T, token, scope string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"scope": scope})
	status, response := s.request(http.MethodPost, "/api/v2/devices/"+s.deviceID+"/webpush/bind-token", token, "", body)
	if status != http.StatusOK {
		t.Fatalf("bind-token(%s) %d: %s", scope, status, response)
	}
	var minted struct {
		Code  string `json:"code"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(response, &minted); err != nil {
		t.Fatal(err)
	}
	if minted.Code == "" || minted.Scope != scope {
		t.Fatalf("mint mismatch: %+v", minted)
	}
	return minted.Code
}

// redeemBind exchanges a code through the PUBLIC bind route and returns the
// session token + cookie value + scope exactly as a browser would see them.
// scope controls the expected response shape: push keeps the legacy bearer
// (historical push-management clients); client is cookie-only — the page JS
// never receives a usable token.
func (s *testStack) redeemBind(t *testing.T, code string) (sessionToken, cookie, scope string, response *http.Response) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"code": code})
	request, err := http.NewRequest(http.MethodPost, s.server.URL+"/api/v2/webpush/bind", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", s.server.URL)
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bind %d: %s", resp.StatusCode, raw)
	}
	var bound struct {
		SessionToken string `json:"sessionToken"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &bound); err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Cookies() {
		if c.Name == webSessionCookieName {
			cookie = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/api/v2" {
				t.Fatalf("cookie flags wrong: %+v", c)
			}
		}
	}
	if cookie == "" {
		t.Fatalf("bind response incomplete: %s", raw)
	}
	switch bound.Scope {
	case "push":
		if bound.SessionToken == "" {
			t.Fatalf("push scope must keep the legacy bearer: %s", raw)
		}
	case "client":
		if bound.SessionToken != "" {
			t.Fatalf("client scope must be cookie-only (no readable token): %s", raw)
		}
	}
	return bound.SessionToken, cookie, bound.Scope, resp
}

// cookieRequest issues a request carrying ONLY the session cookie. An
// idempotencyKey non-empty value goes in the Idempotency-Key HEADER, exactly
// like the native client sends it.
func (s *testStack) cookieRequest(t *testing.T, method, path, cookie, origin string, body any) (int, []byte) {
	t.Helper()
	return s.cookieRequestIdem(t, method, path, cookie, origin, "", body)
}

func (s *testStack) cookieRequestIdem(t *testing.T, method, path, cookie, origin, idem string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, s.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		request.Header.Set("Cookie", webSessionCookieName+"="+cookie)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if idem != "" {
		request.Header.Set("Idempotency-Key", idem)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (s *testStack) wsDial(t *testing.T, path, cookie, origin string) (*websocket.Conn, error) {
	t.Helper()
	header := http.Header{}
	if cookie != "" {
		header.Set("Cookie", webSessionCookieName+"="+cookie)
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial("ws"+s.server.URL[len("http"):]+path, header)
	return conn, err
}

func TestStandaloneClientScopeEnforcedOnCallSMSAPIs(t *testing.T) {
	stack := newTestStack(t)
	stack.attachWebPush(t)
	keyID, secret := stack.createKey(false)
	deviceToken, _ := stack.enroll(keyID, secret, "Browser Phone")

	code := stack.mintBindCode(t, deviceToken, "push")
	_, pushCookie, scope, _ := stack.redeemBind(t, code)
	if scope != "push" {
		t.Fatalf("scope = %q", scope)
	}

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/v2/lines", nil},
		{http.MethodGet, "/api/v2/device", nil},
		{http.MethodGet, "/api/v2/threads", nil},
		{http.MethodGet, "/api/v2/messages", nil},
		{http.MethodPost, "/api/v2/messages", map[string]any{"to": "10086", "body": "hi", "idempotencyKey": "k1"}},
		{http.MethodGet, "/api/v2/calls", nil},
		{http.MethodPost, "/api/v2/calls", map[string]any{"to": "10086", "clientCallId": "cc1", "idempotencyKey": "d1"}},
	} {
		status, body := stack.cookieRequest(t, tc.method, tc.path, pushCookie, stack.server.URL, tc.body)
		if status != http.StatusUnauthorized {
			t.Fatalf("push scope %s %s = %d, want 401: %s", tc.method, tc.path, status, body)
		}
	}
	// Events WebSocket pre-upgrade: same denial.
	if conn, err := stack.wsDial(t, "/api/v2/events", pushCookie, stack.server.URL); err == nil {
		_ = conn.Close()
		t.Fatal("push-scope session must not open the events stream")
	}
}

func TestStandaloneClientScopeGrantsExactlyDevicePermissions(t *testing.T) {
	stack := newTestStack(t)
	stack.attachWebPush(t)
	keyID, secret := stack.createKey(false)
	deviceToken, _ := stack.enroll(keyID, secret, "Browser Phone")

	code := stack.mintBindCode(t, deviceToken, "client")
	_, clientCookie, scope, _ := stack.redeemBind(t, code)
	if scope != "client" {
		t.Fatalf("scope = %q", scope)
	}

	status, _ := stack.cookieRequest(t, http.MethodGet, "/api/v2/lines", clientCookie, stack.server.URL, nil)
	if status != http.StatusOK {
		t.Fatalf("client lines = %d, want 200", status)
	}
	status, _ = stack.cookieRequest(t, http.MethodGet, "/api/v2/threads", clientCookie, stack.server.URL, nil)
	if status != http.StatusOK {
		t.Fatalf("client threads = %d, want 200", status)
	}

	// A real SMS send on the granted line succeeds end-to-end (fake worker).
	status, body := stack.cookieRequestIdem(t, http.MethodPost, "/api/v2/messages", clientCookie,
		stack.server.URL, "sms-1", map[string]any{"to": "10086", "body": "hi"})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("client send = %d: %s", status, body)
	}
	// A real dial reaches the gateway and connects the fake worker leg.
	status, body = stack.cookieRequestIdem(t, http.MethodPost, "/api/v2/calls", clientCookie,
		stack.server.URL, "d1", map[string]any{"to": "10086", "clientCallId": "cc1"})
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("client dial = %d: %s", status, body)
	}
	// Events stream upgrades for the client scope.
	conn, err := stack.wsDial(t, "/api/v2/events", clientCookie, stack.server.URL)
	if err != nil {
		t.Fatalf("client events stream must upgrade: %v", err)
	}
	_ = conn.Close()
	// Media attach for an unknown call passes authZ and fails at the call
	// guard (not 401) — the authZ boundary is what we prove here.
	status, _ = stack.cookieRequest(t, http.MethodGet, "/api/v2/calls/call_nope/media",
		clientCookie, stack.server.URL, nil)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		t.Fatalf("client media pre-flight must pass authZ: %d", status)
	}
}

func TestStandaloneCookieOriginEnforced(t *testing.T) {
	stack := newTestStack(t)
	stack.attachWebPush(t)
	keyID, secret := stack.createKey(false)
	deviceToken, _ := stack.enroll(keyID, secret, "Browser Phone")
	code := stack.mintBindCode(t, deviceToken, "client")
	_, clientCookie, _, _ := stack.redeemBind(t, code)

	// Foreign Origin on a plain route.
	status, _ := stack.cookieRequest(t, http.MethodGet, "/api/v2/lines", clientCookie,
		"https://evil.example", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("foreign Origin lines = %d, want 401", status)
	}
	// Foreign Origin on the events WebSocket upgrade.
	if conn, err := stack.wsDial(t, "/api/v2/events", clientCookie, "https://evil.example"); err == nil {
		_ = conn.Close()
		t.Fatal("foreign-Origin WebSocket must not upgrade")
	}
}

func TestStandaloneRevocationDeniesEverySurface(t *testing.T) {
	stack := newTestStack(t)
	manager := stack.attachWebPush(t)
	keyID, secret := stack.createKey(false)
	deviceToken, _ := stack.enroll(keyID, secret, "Browser Phone")
	code := stack.mintBindCode(t, deviceToken, "client")
	_, clientCookie, _, _ := stack.redeemBind(t, code)

	if status, _ := stack.cookieRequest(t, http.MethodGet, "/api/v2/lines", clientCookie, stack.server.URL, nil); status != http.StatusOK {
		t.Fatalf("pre-revocation lines = %d", status)
	}
	if err := manager.RevokeSessions(context.Background(), stack.deviceID); err != nil {
		t.Fatal(err)
	}
	if status, _ := stack.cookieRequest(t, http.MethodGet, "/api/v2/lines", clientCookie, stack.server.URL, nil); status != http.StatusUnauthorized {
		t.Fatalf("revoked session lines = %d, want 401", status)
	}
}

func TestStandaloneDeviceKeyRevocationDeniesClientSession(t *testing.T) {
	stack := newTestStack(t)
	stack.attachWebPush(t)
	keyID, secret := stack.createKey(false)
	deviceToken, _ := stack.enroll(keyID, secret, "Browser Phone")
	code := stack.mintBindCode(t, deviceToken, "client")
	_, clientCookie, _, _ := stack.redeemBind(t, code)

	if err := stack.store.SetEnrollmentKeyRevoked(context.Background(), keyID, true); err != nil {
		t.Fatal(err)
	}
	if status, _ := stack.cookieRequest(t, http.MethodGet, "/api/v2/lines", clientCookie, stack.server.URL, nil); status != http.StatusUnauthorized {
		t.Fatalf("key-revoked client lines = %d, want 401", status)
	}
}

func TestStandaloneCookieOriginPolicy(t *testing.T) {
	stack := newTestStack(t)
	stack.attachWebPush(t)
	keyID, secret := stack.createKey(false)
	deviceToken, _ := stack.enroll(keyID, secret, "Browser Phone")
	code := stack.mintBindCode(t, deviceToken, "client")
	_, clientCookie, _, _ := stack.redeemBind(t, code)

	// Same-site SIBLING origin (host without the port) must fail.
	status, _ := stack.cookieRequest(t, http.MethodGet, "/api/v2/lines", clientCookie,
		"http://127.0.0.1", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("sibling-origin lines = %d, want 401", status)
	}
	// Wrong scheme against the effective (plain http) origin must fail.
	status, _ = stack.cookieRequest(t, http.MethodGet, "/api/v2/lines", clientCookie,
		"https://"+strings.TrimPrefix(stack.server.URL, "http://"), nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("wrong-scheme lines = %d, want 401", status)
	}
	// Reloads/navigations send no Origin: safe GETs still work.
	status, _ = stack.cookieRequest(t, http.MethodGet, "/api/v2/lines", clientCookie, "", nil)
	if status != http.StatusOK {
		t.Fatalf("Origin-less GET lines = %d, want 200", status)
	}
	// Mutations without Origin fail closed.
	status, _ = stack.cookieRequest(t, http.MethodPost, "/api/v2/messages", clientCookie, "",
		map[string]any{"to": "10086", "body": "hi"})
	if status != http.StatusUnauthorized {
		t.Fatalf("Origin-less POST messages = %d, want 401", status)
	}
	// Trusted-proxy scheme: behind X-Forwarded-Proto https the effective
	// origin becomes https://, matching browser expectations and rejecting
	// plain-http forgeries.
	proxyOrigin := "https://" + strings.TrimPrefix(stack.server.URL, "http://")
	status, _ = stack.cookieRequestForwarded(t, http.MethodGet, "/api/v2/lines", clientCookie,
		proxyOrigin, "https")
	if status != http.StatusOK {
		t.Fatalf("proxied https lines = %d, want 200", status)
	}
	status, _ = stack.cookieRequestForwarded(t, http.MethodGet, "/api/v2/lines", clientCookie,
		stack.server.URL, "https")
	if status != http.StatusUnauthorized {
		t.Fatalf("scheme-downgrade behind proxy = %d, want 401", status)
	}
}

func TestStandaloneUnbindRevokesOnlyThisBrowser(t *testing.T) {
	stack := newTestStack(t)
	stack.attachWebPush(t)
	keyID, secret := stack.createKey(false)
	deviceToken, _ := stack.enroll(keyID, secret, "Browser Phone")

	// Two browser sessions on the same device (two browsers bound).
	codeA := stack.mintBindCode(t, deviceToken, "client")
	_, cookieA, _, _ := stack.redeemBind(t, codeA)
	codeB := stack.mintBindCode(t, deviceToken, "client")
	_, cookieB, _, _ := stack.redeemBind(t, codeB)

	// Revoke browser A through the new endpoint (same-origin cookie POST).
	revoke := func(cookie string) int {
		request, err := http.NewRequest(http.MethodPost, stack.server.URL+"/api/v2/webpush/session/revoke", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Cookie", webSessionCookieName+"="+cookie)
		request.Header.Set("Origin", stack.server.URL)
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if status := revoke(cookieA); status != http.StatusOK {
		t.Fatalf("revoke A = %d", status)
	}

	// A is logged out — even a safe GET without Origin now fails (cookie dead).
	if status, _ := stack.cookieRequest(t, http.MethodGet, "/api/v2/lines", cookieA, "", nil); status != http.StatusUnauthorized {
		t.Fatalf("revoked browser A lines = %d, want 401", status)
	}
	// B still works: exactly-this-browser revocation.
	if status, _ := stack.cookieRequest(t, http.MethodGet, "/api/v2/lines", cookieB, "", nil); status != http.StatusOK {
		t.Fatalf("browser B lines = %d, want 200", status)
	}
	// The native device credential is untouched (unrelated-device red line).
	status, _ := stack.request(http.MethodGet, "/api/v2/lines", deviceToken, "", nil)
	if status != http.StatusOK {
		t.Fatalf("native device lines = %d, want 200", status)
	}
}

func TestStandaloneBindEndpointCSRFAndSessionReplacement(t *testing.T) {
	stack := newTestStack(t)
	stack.attachWebPush(t)
	keyID, secret := stack.createKey(false)
	deviceToken, _ := stack.enroll(keyID, secret, "Browser Phone")
	code := stack.mintBindCode(t, deviceToken, "client")

	// Missing Origin on the redeem POST: rejected (no cross-site form bind).
	bind := func(origin string) int {
		body, _ := json.Marshal(map[string]string{"code": code})
		request, err := http.NewRequest(http.MethodPost, stack.server.URL+"/api/v2/webpush/bind", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if status := bind(""); status != http.StatusForbidden {
		t.Fatalf("Origin-less bind = %d, want 403", status)
	}
	if status := bind("https://evil.example"); status != http.StatusForbidden {
		t.Fatalf("foreign-Origin bind = %d, want 403", status)
	}
	// The code is NOT consumed by a rejected redeem: the legitimate PWA
	// redeem still succeeds afterwards (session-replacement attack denied).
	if status := bind(stack.server.URL); status != http.StatusOK {
		t.Fatalf("legit bind = %d, want 200", status)
	}
}

// cookieRequestForwarded adds X-Forwarded-Proto to emulate a trusted proxy.
func (s *testStack) cookieRequestForwarded(t *testing.T, method, path, cookie, origin, forwardedProto string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(method, s.server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Cookie", webSessionCookieName+"="+cookie)
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if forwardedProto != "" {
		request.Header.Set("X-Forwarded-Proto", forwardedProto)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestStandalonePerLineSendPermissionStillApplies(t *testing.T) {
	stack := newTestStack(t)
	stack.attachWebPush(t)
	keyID, secret := stack.createKey(false)
	// Same key, but line1 loses sendSms after enrollment.
	if err := stack.store.SetKeyLine(context.Background(), store.KeyLine{KeyID: keyID, LineID: "line1", ReceiveSMS: true, ReceiveCalls: true, SendSMS: false, Dial: true}); err != nil {
		t.Fatal(err)
	}
	deviceToken, _ := stack.enroll(keyID, secret, "Browser Phone")
	code := stack.mintBindCode(t, deviceToken, "client")
	_, clientCookie, _, _ := stack.redeemBind(t, code)

	pre, preBody := stack.cookieRequest(t, http.MethodGet, "/api/v2/device", clientCookie, stack.server.URL, nil)
	if pre != http.StatusOK {
		t.Fatalf("client auth pre-flight = %d: %s", pre, preBody)
	}
	status, body := stack.cookieRequestIdem(t, http.MethodPost, "/api/v2/messages", clientCookie,
		stack.server.URL, "sms-1", map[string]any{"to": "10086", "body": "hi"})
	if status != http.StatusForbidden {
		t.Fatalf("sendSms-denied line = %d, want 403: %s", status, body)
	}
}
