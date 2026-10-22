package console

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/store"

	"golang.org/x/crypto/bcrypt"
)

// ---------- helpers ----------

// adminRequest issues a JSON request against the admin API. `bearer` is the
// native session token (empty for anonymous), `csrf` the session CSRF token
// for mutating calls.
func (e *consoleTestEnv) adminRequest(method, path, bearer, csrf string, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if csrf != "" {
		req.Header.Set(adminCSRFHeader, csrf)
	}
	rec := httptest.NewRecorder()
	e.server.AdminAPIHandler().ServeHTTP(rec, req)
	return rec
}

func decodeAdminJSON(t *testing.T, rec *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

// adminLoginFile writes a throwaway bcrypt credential file and points the
// server at it, so the REAL login path (bcrypt + rate limiter + session
// creation) is exercised instead of a hand-inserted session row.
func (e *consoleTestEnv) adminLoginFile(t *testing.T) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret-pass"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	path := filepath.Join(t.TempDir(), "console-bcrypt.txt")
	if err := os.WriteFile(path, []byte("basic_auth {\n\toperator "+string(hash)+"\n}\n"), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	e.server.Cfg.Gateway.ConsoleBcryptFile = path
}

type adminLoginResult struct {
	Username     string `json:"username"`
	SessionToken string `json:"sessionToken"`
	CSRF         string `json:"csrf"`
	ExpiresAt    int64  `json:"expiresAt"`
}

// adminLogin performs a real native login and returns the session material.
func (e *consoleTestEnv) adminLogin(t *testing.T) adminLoginResult {
	t.Helper()
	rec := e.adminRequest(http.MethodPost, "/api/v2/admin/login", "", "",
		`{"username":"operator","password":"s3cret-pass"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d body=%s", rec.Code, rec.Body.String())
	}
	var result adminLoginResult
	decodeAdminJSON(t, rec, &result)
	if result.SessionToken == "" || result.CSRF == "" {
		t.Fatalf("login answered no session material: %s", rec.Body.String())
	}
	return result
}

// ---------- authentication ----------

func TestAdminLoginAndSessionLifecycle(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.adminLoginFile(t)

	// Wrong password is refused and never mints a session.
	rec := env.adminRequest(http.MethodPost, "/api/v2/admin/login", "", "",
		`{"username":"operator","password":"wrong"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", rec.Code)
	}
	// Wrong username too.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/login", "", "",
		`{"username":"someone","password":"s3cret-pass"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong user status = %d, want 401", rec.Code)
	}

	session := env.adminLogin(t)
	if session.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("session already expired: %d", session.ExpiresAt)
	}
	if session.Username != "operator" {
		t.Fatalf("session username = %q", session.Username)
	}

	// A dropped bearer token is NOT a session: the admin API never accepts a
	// pairing/device token (a device bearer would simply not be a console
	// session row).
	rec = env.adminRequest(http.MethodGet, "/api/v2/admin/overview", "device-bearer-token", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged bearer status = %d, want 401", rec.Code)
	}
	// Anonymous is unauthorized.
	rec = env.adminRequest(http.MethodGet, "/api/v2/admin/overview", "", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}

	// The bearer session works.
	rec = env.adminRequest(http.MethodGet, "/api/v2/admin/session", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("session status = %d body=%s", rec.Code, rec.Body.String())
	}
	var info struct {
		Username string `json:"username"`
		CSRF     string `json:"csrf"`
	}
	decodeAdminJSON(t, rec, &info)
	if info.Username != "operator" || info.CSRF != session.CSRF {
		t.Fatalf("session info mismatch: %+v", info)
	}

	// Mutations require CSRF; a wrong or missing token changes nothing.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/settings", session.SessionToken, "",
		`{"max_concurrent_calls":4}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing csrf status = %d, want 403", rec.Code)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/settings", session.SessionToken, "wrong",
		`{"max_concurrent_calls":4}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong csrf status = %d, want 403", rec.Code)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/settings", session.SessionToken, session.CSRF,
		`{"max_concurrent_calls":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid csrf status = %d body=%s", rec.Code, rec.Body.String())
	}
	// GETs never need CSRF.
	rec = env.adminRequest(http.MethodGet, "/api/v2/admin/settings", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("settings GET status = %d", rec.Code)
	}

	// Logout revokes the session for BOTH the native bearer and any browser.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/logout", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = env.adminRequest(http.MethodGet, "/api/v2/admin/overview", session.SessionToken, "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout status = %d, want 401", rec.Code)
	}
}

func TestAdminWebSessionCookieStillWorks(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	// The existing browser session (cookie + CSRF) reaches the same JSON API:
	// one session store, two presentations.
	req := httptest.NewRequest(http.MethodGet, "/api/v2/admin/lines", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: testSessionToken})
	rec := httptest.NewRecorder()
	env.server.AdminAPIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie session status = %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Lines []adminLineView `json:"lines"`
	}
	decodeAdminJSON(t, rec, &payload)
	if len(payload.Lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(payload.Lines))
	}
}

// The device API's bearer tokens carry no administrator privilege: a real,
// freshly minted device access token authenticates on the DEVICE plane and is
// still rejected on the admin plane, and a random token is rejected too.
func TestAdminRejectsDeviceTokens(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	ctx := context.Background()
	const deviceToken = "cra_device_token_for_isolation_test"
	if err := env.store.StoreDeviceToken(ctx, hashToken(deviceToken), "dev_primary", "access",
		time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("store device token: %v", err)
	}
	// The token IS a real device credential...
	if _, _, err := env.auth.Authenticate(ctx, deviceToken); err != nil {
		t.Fatalf("device token not accepted on the device plane: %v", err)
	}
	// ...and grants nothing on the admin plane.
	for _, token := range []string{deviceToken, "not-a-console-session"} {
		rec := env.adminRequest(http.MethodGet, "/api/v2/admin/devices", token, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("device token %q on admin API = %d, want 401", token[:6], rec.Code)
		}
		rec = env.adminRequest(http.MethodPost, "/api/v2/admin/settings", token, "", `{"max_concurrent_calls":2}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("device token %q mutated admin settings: %d", token[:6], rec.Code)
		}
	}
}

// hashToken mirrors the auth service's token hashing (sha256).
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// ---------- lines ----------

func TestAdminLinesListAndUpdate(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	session := env.browserSession()

	rec := env.adminRequest(http.MethodGet, "/api/v2/admin/lines", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("lines status = %d", rec.Code)
	}
	var list struct {
		Lines []adminLineView `json:"lines"`
	}
	decodeAdminJSON(t, rec, &list)
	if len(list.Lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(list.Lines))
	}
	if list.Lines[0].RetentionDays != 30 || list.Lines[0].RejectPolicy != "none" {
		t.Fatalf("saved line values not echoed: %+v", list.Lines[0])
	}

	// Real update persists exactly the submitted fields.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/lines/line1", session.SessionToken, session.CSRF,
		`{"name":"主线路","rejectPolicy":"reject_all","blockedPrefixes":"95","retentionDays":14,"allowInbound":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("line update status = %d body=%s", rec.Code, rec.Body.String())
	}
	line, err := env.store.GetLine(context.Background(), "line1")
	if err != nil {
		t.Fatalf("get line: %v", err)
	}
	if line.Name != "主线路" || line.RejectPolicy != "reject_all" || line.BlockedPrefixes != "95" ||
		line.RecordingRetentionDays != 14 || line.AllowInbound {
		t.Fatalf("line not persisted: %+v", line)
	}
	if !line.AllowOutbound {
		t.Fatalf("unsubmitted field was clobbered: %+v", line)
	}

	// Validation rejects garbage without touching the row.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/lines/line1", session.SessionToken, session.CSRF,
		`{"rejectPolicy":"delete_everything"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad policy status = %d, want 400", rec.Code)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/lines/line1", session.SessionToken, session.CSRF,
		`{"name":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name status = %d, want 400", rec.Code)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/lines/line1", session.SessionToken, session.CSRF,
		`{"retentionDays":99999}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad retention status = %d, want 400", rec.Code)
	}
	// Unknown line is an honest 404.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/lines/line9", session.SessionToken, session.CSRF,
		`{"name":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown line status = %d, want 404", rec.Code)
	}

	// Manual number set and clear both persist through the real manager path.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/lines/line1/number", session.SessionToken, session.CSRF,
		`{"manualNumber":"+61400123456"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("number set status = %d body=%s", rec.Code, rec.Body.String())
	}
	identity, err := env.store.GetLineIdentity(context.Background(), "line1")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	if identity.ManualNumber != "+61400123456" {
		t.Fatalf("manual number not stored: %+v", identity)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/lines/line1/number", session.SessionToken, session.CSRF,
		`{"manualNumber":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("number clear status = %d", rec.Code)
	}
	identity, _ = env.store.GetLineIdentity(context.Background(), "line1")
	if identity.ManualNumber != "" {
		t.Fatalf("manual number not cleared: %+v", identity)
	}
}

// ---------- keys ----------

func TestAdminKeyLifecycle(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	session := env.browserSession()

	// Create + reveal: the response carries the SAME material the console
	// shows once (key id + secret) and never a URL.
	rec := env.adminRequest(http.MethodPost, "/api/v2/admin/keys", session.SessionToken, session.CSRF,
		`{"name":"测试密钥","allLines":false,"manageNumber":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("key create status = %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		KeyID          string `json:"keyId"`
		EnrollmentKey  string `json:"enrollmentKey"`
		PairingPayload string `json:"pairingPayload"`
	}
	decodeAdminJSON(t, rec, &created)
	if created.KeyID == "" || !strings.HasPrefix(created.EnrollmentKey, created.KeyID+".") {
		t.Fatalf("key material malformed: %+v", created)
	}
	if !strings.Contains(created.PairingPayload, created.EnrollmentKey) {
		t.Fatalf("pairing payload lacks the key: %s", created.PairingPayload)
	}
	key, _, err := env.store.GetEnrollmentKey(context.Background(), created.KeyID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if !key.ManageNumber {
		t.Fatalf("manage-number capability not persisted")
	}

	// Reveal again returns the same secret (recoverable copy).
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/keys/"+created.KeyID+"/reveal", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reveal status = %d body=%s", rec.Code, rec.Body.String())
	}
	var revealed struct {
		EnrollmentKey string `json:"enrollmentKey"`
	}
	decodeAdminJSON(t, rec, &revealed)
	if revealed.EnrollmentKey != created.EnrollmentKey {
		t.Fatalf("reveal mismatch: %s != %s", revealed.EnrollmentKey, created.EnrollmentKey)
	}

	// Grants: set, read back, then remove.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/keys/"+created.KeyID+"/grant", session.SessionToken, session.CSRF,
		`{"lineId":"line1","receiveSms":true,"receiveCalls":true,"sendSms":true,"dial":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant status = %d body=%s", rec.Code, rec.Body.String())
	}
	grants, err := env.store.KeyLines(context.Background(), created.KeyID)
	if err != nil || len(grants) != 1 {
		t.Fatalf("grants = %+v err=%v", grants, err)
	}
	if !grants[0].SendSMS || !grants[0].Dial || !grants[0].ReceiveSMS || !grants[0].ReceiveCalls {
		t.Fatalf("grant flags not persisted: %+v", grants[0])
	}
	rec = env.adminRequest(http.MethodPost,
		"/api/v2/admin/keys/"+created.KeyID+"/grant/line1/delete", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("grant delete status = %d body=%s", rec.Code, rec.Body.String())
	}
	grants, _ = env.store.KeyLines(context.Background(), created.KeyID)
	if len(grants) != 0 {
		t.Fatalf("grant survived delete: %+v", grants)
	}

	// Number-management permission toggle.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/keys/"+created.KeyID+"/manage-number",
		session.SessionToken, session.CSRF, `{"manageNumber":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("manage-number status = %d", rec.Code)
	}
	key, _, _ = env.store.GetEnrollmentKey(context.Background(), created.KeyID)
	if key.ManageNumber {
		t.Fatalf("manage-number not revoked")
	}

	// Revoke then delete: an ACTIVE key cannot be deleted, a revoked one can.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/keys/"+created.KeyID+"/delete", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete-active status = %d, want 409", rec.Code)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/keys/"+created.KeyID+"/revoke", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d body=%s", rec.Code, rec.Body.String())
	}
	// A revoked key is never revealed.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/keys/"+created.KeyID+"/reveal", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("reveal-revoked status = %d, want 409", rec.Code)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/keys/"+created.KeyID+"/delete", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete-revoked status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, _, err := env.store.GetEnrollmentKey(context.Background(), created.KeyID); err == nil {
		t.Fatalf("key survived delete")
	}
	// Deleting again is an honest 404.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/keys/"+created.KeyID+"/delete", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("double delete status = %d, want 404", rec.Code)
	}

	// Key list exposes revocability/grants for the native screen.
	rec = env.adminRequest(http.MethodGet, "/api/v2/admin/keys", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("keys list status = %d", rec.Code)
	}
	var list struct {
		Keys []adminKeyView `json:"keys"`
	}
	decodeAdminJSON(t, rec, &list)
	if len(list.Keys) != 3 { // two seeded + the created one (now deleted) => 2 + 0
		// seeded key1/key2 remain
		if len(list.Keys) != 2 {
			t.Fatalf("keys = %d, want 2", len(list.Keys))
		}
	}
	for _, view := range list.Keys {
		if view.SecretRecoverable != true {
			t.Fatalf("seeded key should be recoverable in the fixture: %+v", view)
		}
	}
}

// A legacy hash-only key answers the explicit unrecoverable conflict the UI
// turns into the replacement workflow.
func TestAdminKeyRevealReportsUnrecoverable(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	session := env.browserSession()
	legacy := encodeLegacyKey(t, env)
	rec := env.adminRequest(http.MethodPost, "/api/v2/admin/keys/"+legacy+"/reveal", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("legacy reveal status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	decodeAdminJSON(t, rec, &body)
	if body["error"] != "unrecoverable" {
		t.Fatalf("legacy reveal error = %q", body["error"])
	}
}

// ---------- devices ----------

func TestAdminDeviceRevokeAndReenroll(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	session := env.browserSession()

	rec := env.adminRequest(http.MethodGet, "/api/v2/admin/devices", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("devices status = %d", rec.Code)
	}
	var list struct {
		Devices []adminDeviceView `json:"devices"`
	}
	decodeAdminJSON(t, rec, &list)
	if len(list.Devices) != 2 {
		t.Fatalf("devices = %d, want 2", len(list.Devices))
	}
	if list.Devices[0].KeyName == "" {
		t.Fatalf("device key name missing: %+v", list.Devices[0])
	}

	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/devices/dev_primary/revoke", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke status = %d body=%s", rec.Code, rec.Body.String())
	}
	device, err := env.store.GetDevice(context.Background(), "dev_primary")
	if err != nil || device.RevokedAt == nil {
		t.Fatalf("device not revoked: %+v err=%v", device, err)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/devices/dev_primary/allow-reenroll", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("allow-reenroll status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/devices/dev_missing/allow-reenroll", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing device status = %d, want 404", rec.Code)
	}
}

// ---------- recording policy ----------

func TestAdminRecordingPolicyIsRealAndSeparate(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	session := env.browserSession()

	// The fixture manager has no recording engine wired: the policy endpoint
	// still reports the persisted auto mode and says configured=false.
	rec := env.adminRequest(http.MethodGet, "/api/v2/admin/recording", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("recording GET status = %d", rec.Code)
	}
	var policy adminRecordingView
	decodeAdminJSON(t, rec, &policy)
	if policy.Configured {
		t.Fatalf("recording engine unexpectedly configured")
	}
	if policy.AutoMode == "" {
		t.Fatalf("auto mode missing")
	}

	// A mutation with no recording engine is an honest 503, never a silent
	// success.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/recording", session.SessionToken, session.CSRF, `{"enabled":true}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("recording POST status = %d, want 503", rec.Code)
	}
	// The auto mode is a REAL store setting and validates its values.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/recording", session.SessionToken, session.CSRF, `{"autoMode":"incoming"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("auto mode status = %d body=%s", rec.Code, rec.Body.String())
	}
	if mode := env.store.RecordingAutoMode(context.Background()); mode != "incoming" {
		t.Fatalf("auto mode = %q, want incoming", mode)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/recording", session.SessionToken, session.CSRF, `{"autoMode":"always"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad auto mode status = %d, want 400", rec.Code)
	}
	if mode := env.store.RecordingAutoMode(context.Background()); mode != "incoming" {
		t.Fatalf("bad mode changed stored value: %q", mode)
	}
}

// ---------- voicemail ----------

func TestAdminVoicemailListAndDelete(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	session := env.browserSession()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vm1.wav")
	if err := os.WriteFile(path, []byte("RIFF....WAVE"), 0o600); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	now := time.Now().UTC()
	if err := env.store.InsertVoicemail(ctx, store.Voicemail{
		ID: "vm1", LineID: "line1", Peer: "+61400111222", State: "ready", Path: path,
		DurationMs: 4200, SizeBytes: 12, CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("create voicemail: %v", err)
	}

	rec := env.adminRequest(http.MethodGet, "/api/v2/admin/voicemails", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("voicemails status = %d", rec.Code)
	}
	var list struct {
		Voicemails []map[string]any `json:"voicemails"`
		TotalBytes int64            `json:"totalBytes"`
	}
	decodeAdminJSON(t, rec, &list)
	found := false
	for _, row := range list.Voicemails {
		if row["id"] == "vm1" {
			found = true
		}
	}
	if !found || list.TotalBytes <= 0 {
		t.Fatalf("seed voicemail missing from list: %+v", list)
	}

	// Audio is served authenticated; anonymous is rejected.
	req := httptest.NewRequest(http.MethodGet, "/api/v2/admin/voicemails/vm1/audio", nil)
	rec = httptest.NewRecorder()
	env.server.AdminAPIHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous audio status = %d, want 401", rec.Code)
	}
	rec = env.adminRequest(http.MethodGet, "/api/v2/admin/voicemails/vm1/audio", session.SessionToken, "", "")
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("audio status = %d len=%d", rec.Code, rec.Body.Len())
	}

	rec = env.adminRequest(http.MethodDelete, "/api/v2/admin/voicemails/vm1", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("voicemail delete status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := env.store.GetVoicemail(ctx, "vm1"); err == nil {
		t.Fatalf("voicemail survived delete")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("voicemail audio file still on disk")
	}
}

// ---------- settings & diagnostics ----------

func TestAdminSettingsValidationAndPersistence(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	session := env.browserSession()
	ctx := context.Background()

	rec := env.adminRequest(http.MethodPost, "/api/v2/admin/settings", session.SessionToken, session.CSRF,
		`{"max_concurrent_calls":6,"disconnect_grace_seconds":45,"voicemailEnabled":false,"voicemail_ring_seconds":25}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings update status = %d body=%s", rec.Code, rec.Body.String())
	}
	if value, _ := env.store.Setting(ctx, "max_concurrent_calls"); value != "6" {
		t.Fatalf("max_concurrent_calls = %q", value)
	}
	if value, _ := env.store.Setting(ctx, "voicemail_enabled"); value != "0" {
		t.Fatalf("voicemail_enabled = %q", value)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/settings", session.SessionToken, session.CSRF,
		`{"max_concurrent_calls":0}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range status = %d, want 400", rec.Code)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/settings", session.SessionToken, session.CSRF,
		`{"max_concurrent_calls":"not-a-number"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-numeric status = %d, want 400", rec.Code)
	}
	if value, _ := env.store.Setting(ctx, "max_concurrent_calls"); value != "6" {
		t.Fatalf("failed update changed the value: %q", value)
	}
	// Numeric strings stay accepted for form-style clients.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/settings", session.SessionToken, session.CSRF,
		`{"max_concurrent_calls":"8"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("numeric-string status = %d body=%s", rec.Code, rec.Body.String())
	}
	if value, _ := env.store.Setting(ctx, "max_concurrent_calls"); value != "8" {
		t.Fatalf("numeric-string update = %q", value)
	}
}

func TestAdminDiagnosticsAndOverview(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	session := env.browserSession()

	rec := env.adminRequest(http.MethodGet, "/api/v2/admin/overview", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview status = %d", rec.Code)
	}
	var overview struct {
		GatewayID string `json:"gatewayId"`
		LineCount int    `json:"lineCount"`
		Trickle   bool   `json:"trickleIce"`
	}
	decodeAdminJSON(t, rec, &overview)
	if overview.GatewayID == "" || overview.LineCount != 2 || !overview.Trickle {
		t.Fatalf("overview = %+v", overview)
	}

	rec = env.adminRequest(http.MethodGet, "/api/v2/admin/diagnostics", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnostics status = %d", rec.Code)
	}
	var diagnostics struct {
		Lines    []adminLineView   `json:"lines"`
		Settings map[string]string `json:"settings"`
	}
	decodeAdminJSON(t, rec, &diagnostics)
	if len(diagnostics.Lines) != 2 || diagnostics.Settings["voicemail_ring_seconds"] == "" {
		t.Fatalf("diagnostics payload incomplete: %+v", diagnostics)
	}

	// Recovery targets a real line; an unknown line is an honest failure that
	// does not pretend success.
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/diagnostics/recover/line1", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("recover status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/diagnostics/recover/nope", session.SessionToken, session.CSRF, "")
	if rec.Code == http.StatusOK {
		t.Fatalf("unknown line recovery pretended success")
	}
}

// ---------- calls & conferences ----------

func TestAdminCallControls(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	session := env.browserSession()
	ctx := context.Background()
	err := env.store.UpsertCall(ctx, store.Call{
		ID: "call1", LineID: "line1", Direction: "inbound", Peer: "+61400999888", State: "incoming_ringing",
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create call: %v", err)
	}

	rec := env.adminRequest(http.MethodGet, "/api/v2/admin/calls", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("calls status = %d", rec.Code)
	}
	var list struct {
		Calls []map[string]any `json:"calls"`
	}
	decodeAdminJSON(t, rec, &list)
	foundCall := false
	for _, row := range list.Calls {
		if row["id"] == "call1" && row["peer"] == "+61400999888" {
			foundCall = true
		}
	}
	if !foundCall {
		t.Fatalf("seeded call missing from list: %+v", list.Calls)
	}

	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/calls/nope/hangup", session.SessionToken, session.CSRF, "")
	if rec.Code == http.StatusOK {
		t.Fatalf("unknown call hangup pretended success")
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/calls/call1/reject", session.SessionToken, session.CSRF, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("reject status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = env.adminRequest(http.MethodGet, "/api/v2/admin/conferences", session.SessionToken, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("conferences status = %d", rec.Code)
	}
	rec = env.adminRequest(http.MethodPost, "/api/v2/admin/conferences/c1/close", session.SessionToken, session.CSRF, "")
	if rec.Code == http.StatusOK {
		t.Fatalf("unknown conference close pretended success")
	}
}

// browserSession is a pre-existing console session (cookie-created) presented
// as a native bearer token: the admin API accepts the same rows.
func (e *consoleTestEnv) browserSession() adminLoginResult {
	return adminLoginResult{SessionToken: testSessionToken, CSRF: testCSRF}
}

// encodeLegacyKey inserts a hash-only (non-recoverable) key row directly,
// exactly like the legacy keys created before sealed secrets existed.
func encodeLegacyKey(t *testing.T, env *consoleTestEnv) string {
	t.Helper()
	ctx := context.Background()
	if err := env.store.CreateEnrollmentKey(ctx, "key_legacy", "旧密钥", []byte("hash"), "", false); err != nil {
		t.Fatalf("create legacy key: %v", err)
	}
	return "key_legacy"
}
