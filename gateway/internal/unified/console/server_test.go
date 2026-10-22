package console

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/runtime"
	"callrelay/gateway/internal/unified/store"
)

const (
	testSessionToken = "console-test-session"
	testCSRF         = "console-test-csrf"
)

type consoleTestEnv struct {
	t       *testing.T
	server  *Server
	store   *store.Store
	manager *runtime.Manager
	auth    *auth.Service
}

// newConsoleTestEnv builds an isolated store/session fixture. No production
// credential file, login form or live service is touched: the session is
// inserted directly and every request carries that cookie plus its CSRF token.
func newConsoleTestEnv(t *testing.T) *consoleTestEnv {
	t.Helper()
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "console.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	// Recoverable-secret storage is enabled in the fixture so key creation and
	// reveal exercise the production sealing path; legacy tests insert NULL
	// rows directly.
	box, err := store.NewRandomSecretBox()
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	database.Secrets = box
	cfg := config.Default()
	cfg.Gateway.PublicBaseURL = "http://127.0.0.1:8789"
	cfg.Gateway.ConsoleBcryptFile = ""
	gatewayIdentity, err := identity.LoadOrCreate(filepath.Join(t.TempDir(), "gateway.key"), cfg.Gateway.ID, cfg.Gateway.Name)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	authService := auth.New(database, gatewayIdentity, time.Hour, time.Hour)
	manager := runtime.NewManager(cfg, database, nil, authService, nil, nil)
	server := NewServer(cfg, database, manager, authService, gatewayIdentity)
	now := time.Now().UTC()
	if err := database.CreateConsoleSession(ctx, store.ConsoleSession{
		TokenHash: hashValue(testSessionToken), CSRF: testCSRF, Username: "test-operator",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), RemoteAddr: "127.0.0.1",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &consoleTestEnv{t: t, server: server, store: database, manager: manager, auth: authService}
}

func (e *consoleTestEnv) request(method, path string, form url.Values, authenticated bool) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if authenticated {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: testSessionToken})
	}
	rec := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *consoleTestEnv) get(path string) *httptest.ResponseRecorder {
	return e.request(http.MethodGet, path, nil, true)
}

func (e *consoleTestEnv) post(path string, form url.Values) *httptest.ResponseRecorder {
	return e.request(http.MethodPost, path, form, true)
}

// seed creates two of each record so rendering is exercised on populated
// multi-row data rather than empty tables.
func (e *consoleTestEnv) seed() (key1, key2 string) {
	e.t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	if err := e.store.EnsureLine(ctx, store.Line{ID: "line1", Name: "一号线路", WorkerURL: "http://127.0.0.1:9781", Enabled: true}); err != nil {
		e.t.Fatalf("seed line1: %v", err)
	}
	if err := e.store.EnsureLine(ctx, store.Line{ID: "line2", Name: "备用卡", WorkerURL: "http://127.0.0.1:9782", Enabled: false}); err != nil {
		e.t.Fatalf("seed line2: %v", err)
	}
	dryRun := true
	name := "一号线路"
	enabled := true
	allowIn, allowOut := true, true
	reject := "none"
	blocked := ""
	retention := 30
	if err := e.store.UpdateLine(ctx, "line1", store.LineUpdate{
		Name: &name, Enabled: &enabled, SMSDryRun: &dryRun, RejectPolicy: &reject,
		BlockedPrefixes: &blocked, AllowInbound: &allowIn, AllowOutbound: &allowOut, RecordingRetentionDays: &retention,
	}); err != nil {
		e.t.Fatalf("seed line1 settings: %v", err)
	}
	name2 := "备用卡"
	enabled2 := false
	allowIn2, allowOut2 := false, true
	reject2 := "reject_blocked"
	blocked2 := "400,95"
	retention2 := 7
	if err := e.store.UpdateLine(ctx, "line2", store.LineUpdate{
		Name: &name2, Enabled: &enabled2, SMSDryRun: &dryRun, RejectPolicy: &reject2,
		BlockedPrefixes: &blocked2, AllowInbound: &allowIn2, AllowOutbound: &allowOut2, RecordingRetentionDays: &retention2,
	}); err != nil {
		e.t.Fatalf("seed line2 settings: %v", err)
	}

	var err error
	key1, _, err = e.auth.CreateKey(ctx, "我的 iPhone", false)
	if err != nil {
		e.t.Fatalf("seed key1: %v", err)
	}
	key2, _, err = e.auth.CreateKey(ctx, "家庭共享", true)
	if err != nil {
		e.t.Fatalf("seed key2: %v", err)
	}
	if err := e.store.SetKeyLine(ctx, store.KeyLine{
		KeyID: key1, LineID: "line1", ReceiveSMS: true, ReceiveCalls: true, SendSMS: false, Dial: true,
	}); err != nil {
		e.t.Fatalf("seed grant: %v", err)
	}
	if err := e.store.CreateDevice(ctx, store.Device{
		ID: "dev_primary", KeyID: key1, Name: "江南的手机", PublicKey: []byte("test-public-key-primary"),
	}); err != nil {
		e.t.Fatalf("seed device1: %v", err)
	}
	if err := e.store.CreateDevice(ctx, store.Device{
		ID: "dev_backup", KeyID: key2, Name: "备用手机", PublicKey: []byte("test-public-key-backup"),
	}); err != nil {
		e.t.Fatalf("seed device2: %v", err)
	}

	if err := e.store.UpsertCall(ctx, store.Call{
		ID: "call_ringing", LineID: "line1", WorkerCallID: "w-call-ringing", Direction: "inbound", Peer: "+8613800000000", State: "incoming_ringing", CreatedAt: now,
	}); err != nil {
		e.t.Fatalf("seed ringing call: %v", err)
	}
	ended := now.Add(-2 * time.Minute)
	answered := now.Add(-150 * time.Second)
	if err := e.store.UpsertCall(ctx, store.Call{
		ID: "call_ended", LineID: "line1", WorkerCallID: "w-call-ended", Direction: "outbound", Peer: "+8613900000000", State: "ended",
		CreatedAt: now.Add(-3 * time.Minute), AnsweredAt: &answered, EndedAt: &ended, EndReason: "console_hangup",
	}); err != nil {
		e.t.Fatalf("seed ended call: %v", err)
	}
	graceDeadline := now.Add(2 * time.Minute)
	if err := e.store.CreateConference(ctx, store.Conference{
		ID: "conf_active", HostDeviceID: "dev_primary", State: "active", CreatedAt: now, GraceDeadline: &graceDeadline,
	}); err != nil {
		e.t.Fatalf("seed active conference: %v", err)
	}
	if err := e.store.AddConferenceLeg(ctx, store.ConferenceLeg{
		ConferenceID: "conf_active", CallID: "call_ringing", JoinedAt: now,
	}); err != nil {
		e.t.Fatalf("seed conference leg: %v", err)
	}
	conferenceEnded := now.Add(-time.Minute)
	if err := e.store.CreateConference(ctx, store.Conference{
		ID: "conf_ended", HostDeviceID: "dev_backup", State: "ended", CreatedAt: now.Add(-10 * time.Minute), EndedAt: &conferenceEnded,
	}); err != nil {
		e.t.Fatalf("seed ended conference: %v", err)
	}
	if err := e.store.InsertVoicemail(ctx, store.Voicemail{
		ID: "vm_ready", LineID: "line1", Peer: "+8613700000000", State: "ready",
		DurationMs: 12000, SizeBytes: 123456, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.AddDate(0, 0, 30),
	}); err != nil {
		e.t.Fatalf("seed voicemail: %v", err)
	}
	if err := e.store.InsertVoicemail(ctx, store.Voicemail{
		ID: "vm_recording", LineID: "line2", Peer: "+8613600000000", State: "recording",
		DurationMs: 3000, SizeBytes: 4096, CreatedAt: now, ExpiresAt: now.AddDate(0, 0, 30),
	}); err != nil {
		e.t.Fatalf("seed recording voicemail: %v", err)
	}
	return key1, key2
}

// formChunk isolates one rendered form so checkbox/select state can be
// asserted without matching the neighboring record.
func formChunk(t *testing.T, body, action string) string {
	t.Helper()
	marker := `action="` + action + `"`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("form action %q not found", action)
	}
	end := strings.Index(body[start:], "</form>")
	if end < 0 {
		t.Fatalf("form %q is not closed", action)
	}
	return body[start : start+end]
}

// rowChunk isolates the table row containing marker so joined display values
// and the row's form can be asserted together.
func rowChunk(t *testing.T, body, marker string) string {
	t.Helper()
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatalf("row marker %q not found", marker)
	}
	rowStart := strings.LastIndex(body[:start], "<tr>")
	if rowStart < 0 {
		t.Fatalf("row start for %q not found", marker)
	}
	rowEnd := strings.Index(body[start:], "</tr>")
	if rowEnd < 0 {
		t.Fatalf("row for %q is not closed", marker)
	}
	return body[rowStart : start+rowEnd]
}

func checkboxMatches(t *testing.T, chunk, name string) string {
	t.Helper()
	expression := regexp.MustCompile(`<input type="checkbox" name="` + regexp.QuoteMeta(name) + `"[^>]*>`)
	match := expression.FindString(chunk)
	if match == "" {
		t.Fatalf("checkbox %q not found in form chunk", name)
	}
	return match
}

func TestAuthenticatedPagesRenderPopulatedRecords(t *testing.T) {
	env := newConsoleTestEnv(t)
	key1, key2 := env.seed()
	_ = key2

	pages := []struct {
		path string
		want []string
	}{
		{"/console/", []string{"一号线路", "呼入", "来电振铃"}},
		{"/console/lines", []string{"一号线路", "备用卡", `action="/console/lines/line1"`}},
		{"/console/keys", []string{"我的 iPhone", "家庭共享", "一号线路", `action="/console/keys/` + key1 + `/lines/line1/delete"`, `action="/console/keys/` + key2 + `/lines"`}},
		{"/console/devices", []string{"江南的手机", "备用手机", "我的 iPhone"}},
		{"/console/calls", []string{"一号线路", "呼入", "呼出", "来电振铃", "已结束", "控制台挂断"}},
		{"/console/conferences", []string{"江南的手机", "备用手机", "重连截止", `action="/console/conferences/conf_active/legs/call_ringing/hangup"`, `action="/console/conferences/conf_active/close"`}},
		{"/console/voicemails", []string{"一号线路", "120.6 KB", "可播放", "录制中"}},
		{"/console/settings", []string{"voicemail_enabled"}},
		{"/console/diagnostics", []string{"一号线路", "备用卡", "line1"}},
	}
	for _, page := range pages {
		recorder := env.get(page.path)
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200; body=%q", page.path, recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		if !strings.Contains(body, "</html>") {
			t.Fatalf("GET %s did not render a complete document", page.path)
		}
		if strings.Contains(body, "模板渲染失败") {
			t.Fatalf("GET %s rendered the template error page", page.path)
		}
		for _, needle := range page.want {
			if !strings.Contains(body, needle) {
				t.Fatalf("GET %s body missing %q", page.path, needle)
			}
		}
	}

	// The ended conference must not offer close/remove actions, and no form
	// action may fall back to the empty root-scoped id.
	conferenceBody := env.get("/console/conferences").Body.String()
	endedIndex := strings.Index(conferenceBody, "conf_ended")
	if endedIndex < 0 {
		t.Fatal("ended conference was not rendered")
	}
	endedWindow := conferenceBody[endedIndex:]
	if len(endedWindow) > 1200 {
		endedWindow = endedWindow[:1200]
	}
	if !strings.Contains(endedWindow, "已结束") {
		t.Fatal("ended conference is not marked as ended")
	}
	if strings.Contains(endedWindow, `action="/console/conferences/conf_ended/close"`) {
		t.Fatal("ended conference still renders a close form")
	}
	if strings.Contains(conferenceBody, `action="/console/conferences//"`) {
		t.Fatal("conference action lost its id and rendered an empty root-scoped URL")
	}
}

func TestKeysPageScopesGrantFormsAndUsesLineDropdown(t *testing.T) {
	env := newConsoleTestEnv(t)
	key1, _ := env.seed()

	recorder := env.get("/console/keys")
	if recorder.Code != http.StatusOK {
		t.Fatalf("keys page status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()

	deleteAction := "/console/keys/" + key1 + "/lines/line1/delete"
	chunk := rowChunk(t, body, deleteAction)
	if !strings.Contains(chunk, `value="`+testCSRF+`"`) {
		t.Fatalf("delete row for %s is missing the session CSRF value", deleteAction)
	}
	if !strings.Contains(chunk, "一号线路") {
		t.Fatalf("delete row for %s does not show the human line name", deleteAction)
	}

	if !strings.Contains(body, `<select name="line_id">`) {
		t.Fatal("grant form must use a line dropdown, not free-text line id input")
	}
	if strings.Contains(body, `name="line_id" placeholder=`) || strings.Contains(body, `type="text" name="line_id"`) {
		t.Fatal("grant form still contains the free-text line id input")
	}
	for _, lineID := range []string{`value="line1"`, `value="line2"`} {
		if !strings.Contains(body, lineID) {
			t.Fatalf("grant dropdown missing option %s", lineID)
		}
	}
	// An all-lines key with no explicit rows must not claim it has no grants.
	if !strings.Contains(body, "此密钥自动覆盖全部线路") {
		t.Fatal("all-lines key without explicit grants must explain its automatic coverage")
	}
}

func TestLineFormsEchoSavedValues(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()

	recorder := env.get("/console/lines")
	if recorder.Code != http.StatusOK {
		t.Fatalf("lines page status = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()

	line1 := formChunk(t, body, "/console/lines/line1")
	if !strings.Contains(checkboxMatches(t, line1, "enabled"), "checked") {
		t.Fatal("line1 enabled checkbox must reflect the saved enabled=true")
	}
	if !strings.Contains(checkboxMatches(t, line1, "allow_inbound"), "checked") {
		t.Fatal("line1 allow_inbound checkbox must reflect the saved allow_inbound=true")
	}
	if !strings.Contains(checkboxMatches(t, line1, "allow_outbound"), "checked") {
		t.Fatal("line1 allow_outbound checkbox must reflect the saved allow_outbound=true")
	}

	line2 := formChunk(t, body, "/console/lines/line2")
	if strings.Contains(checkboxMatches(t, line2, "enabled"), "checked") {
		t.Fatal("line2 enabled checkbox must not be unconditionally checked")
	}
	if strings.Contains(checkboxMatches(t, line2, "allow_inbound"), "checked") {
		t.Fatal("line2 allow_inbound checkbox must not be unconditionally checked")
	}
	if !strings.Contains(checkboxMatches(t, line2, "allow_outbound"), "checked") {
		t.Fatal("line2 allow_outbound checkbox must reflect the saved allow_outbound=true")
	}
	if !strings.Contains(line2, `<option value="reject_blocked" selected>`) {
		t.Fatal("line2 reject_policy select must echo the saved reject_blocked policy")
	}
	if !strings.Contains(line2, `name="blocked_prefixes" value="400,95"`) {
		t.Fatal("line2 blocked prefixes input must echo the saved value")
	}
	if !strings.Contains(line2, `name="recording_retention_days" min="0" max="3650" value="7"`) {
		t.Fatal("line2 recording retention input must echo the saved value")
	}
}

func TestLineUpdatePersistsAndRenders(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	ctx := context.Background()

	form := url.Values{
		"csrf": {testCSRF}, "name": {"主线路"}, "enabled": {"on"}, "allow_outbound": {"on"},
		"reject_policy": {"reject_all"}, "blocked_prefixes": {"95"}, "recording_retention_days": {"14"},
	}
	recorder := env.post("/console/lines/line1", form)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("line update status = %d, want 303; body=%q", recorder.Code, recorder.Body.String())
	}
	line, err := env.store.GetLine(ctx, "line1")
	if err != nil {
		t.Fatalf("get updated line: %v", err)
	}
	if line.Name != "主线路" || !line.Enabled || line.AllowInbound || !line.AllowOutbound {
		t.Fatalf("line flags not persisted: %+v", line)
	}
	if line.RejectPolicy != "reject_all" || line.BlockedPrefixes != "95" || line.RecordingRetentionDays != 14 {
		t.Fatalf("line policy not persisted: %+v", line)
	}

	body := env.get("/console/lines").Body.String()
	chunk := formChunk(t, body, "/console/lines/line1")
	if !strings.Contains(checkboxMatches(t, chunk, "enabled"), "checked") {
		t.Fatal("updated enabled=true was not reflected")
	}
	if strings.Contains(checkboxMatches(t, chunk, "allow_inbound"), "checked") {
		t.Fatal("updated allow_inbound=false was not reflected")
	}
	if !strings.Contains(checkboxMatches(t, chunk, "allow_outbound"), "checked") {
		t.Fatal("updated allow_outbound=true was not reflected")
	}
	if !strings.Contains(chunk, `<option value="reject_all" selected>`) {
		t.Fatal("updated reject policy was not reflected")
	}
	if !strings.Contains(chunk, `name="blocked_prefixes" value="95"`) {
		t.Fatal("updated blocked prefixes were not reflected")
	}
	if !strings.Contains(chunk, `name="recording_retention_days" min="0" max="3650" value="14"`) {
		t.Fatal("updated retention days were not reflected")
	}

	// A missing CSRF token must be rejected before any store write.
	badForm := url.Values{"name": {"被篡改"}}
	bad := env.post("/console/lines/line1", badForm)
	if bad.Code != http.StatusForbidden {
		t.Fatalf("line update without CSRF status = %d, want 403", bad.Code)
	}
	reloaded, _ := env.store.GetLine(ctx, "line1")
	if reloaded.Name != "主线路" {
		t.Fatal("rejected CSRF request changed the line")
	}
}

func TestKeyGrantPersistsAndDeleteURLIsScoped(t *testing.T) {
	env := newConsoleTestEnv(t)
	key1, key2 := env.seed()
	ctx := context.Background()

	form := url.Values{"csrf": {testCSRF}, "line_id": {"line2"}, "receive_sms": {"on"}, "dial": {"on"}}
	recorder := env.post("/console/keys/"+key2+"/lines", form)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("key grant status = %d, want 303; body=%q", recorder.Code, recorder.Body.String())
	}
	grants, err := env.store.KeyLines(ctx, key2)
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(grants) != 1 || grants[0].LineID != "line2" || !grants[0].ReceiveSMS || grants[0].ReceiveCalls || grants[0].SendSMS || !grants[0].Dial {
		t.Fatalf("grant not persisted as submitted: %+v", grants)
	}

	body := env.get("/console/keys").Body.String()
	key2Chunk := body[strings.Index(body, key2):]
	if !strings.Contains(key2Chunk, "/console/keys/"+key2+"/lines/line2/delete") {
		t.Fatal("new grant delete action does not target the key that owns it")
	}
	if !strings.Contains(key2Chunk, "备用卡") {
		t.Fatal("new grant row does not show the friendly line name")
	}
	key1Delete := formChunk(t, body, "/console/keys/"+key1+"/lines/line1/delete")
	if strings.Contains(key1Delete, key2) {
		t.Fatal("delete form for key1 references the wrong key")
	}

	bad := env.post("/console/keys/"+key2+"/lines", url.Values{"line_id": {"line1"}})
	if bad.Code != http.StatusForbidden {
		t.Fatalf("grant without CSRF status = %d, want 403", bad.Code)
	}
}

func TestSettingsUpdatePersistsAndReadsBack(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	ctx := context.Background()

	form := url.Values{
		"csrf": {testCSRF}, "max_concurrent_calls": {"8"}, "disconnect_grace_seconds": {"45"},
		"voicemail_ring_seconds": {"25"}, "voicemail_offline_seconds": {"60"}, "voicemail_max_seconds": {"180"},
		"voicemail_retention_days": {"14"}, "voicemail_max_bytes_mb": {"800"}, "voicemail_min_free_bytes_mb": {"128"},
	}
	recorder := env.post("/console/settings", form)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("settings update status = %d, want 303; body=%q", recorder.Code, recorder.Body.String())
	}
	if value, err := env.store.Setting(ctx, "voicemail_enabled"); err != nil || value != "0" {
		t.Fatalf("voicemail_enabled = %q (%v), want 0 after unchecked submit", value, err)
	}
	if value, _ := env.store.Setting(ctx, "voicemail_offline_seconds"); value != "60" {
		t.Fatalf("voicemail_offline_seconds = %q, want 60", value)
	}

	body := env.get("/console/settings").Body.String()
	chunk := formChunk(t, body, "/console/settings")
	if strings.Contains(checkboxMatches(t, chunk, "voicemail_enabled"), "checked") {
		t.Fatal("voicemail_enabled checkbox stayed checked after saving disabled")
	}
	if !strings.Contains(chunk, `name="voicemail_offline_seconds" min="2" max="300" value="60"`) {
		t.Fatal("saved offline seconds were not echoed back")
	}
	if !strings.Contains(chunk, `name="voicemail_max_bytes_mb" min="10" max="102400" value="800"`) {
		t.Fatal("saved capacity in MB was not echoed back")
	}

	form.Set("voicemail_enabled", "on")
	if recorder := env.post("/console/settings", form); recorder.Code != http.StatusSeeOther {
		t.Fatalf("settings re-enable status = %d, want 303", recorder.Code)
	}
	chunk = formChunk(t, env.get("/console/settings").Body.String(), "/console/settings")
	if !strings.Contains(checkboxMatches(t, chunk, "voicemail_enabled"), "checked") {
		t.Fatal("voicemail_enabled checkbox must be checked after saving enabled")
	}
}

func TestAnonymousRequestsAreRejected(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()

	recorder := env.request(http.MethodGet, "/console/", nil, false)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/console/login" {
		t.Fatalf("anonymous GET /console/ = %d %q, want 303 /console/login", recorder.Code, recorder.Header().Get("Location"))
	}
	form := url.Values{"csrf": {testCSRF}, "name": {"匿名修改"}}
	recorder = env.request(http.MethodPost, "/console/lines/line1", form, false)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("anonymous POST /console/lines/line1 = %d, want redirect to login", recorder.Code)
	}
	line, _ := env.store.GetLine(context.Background(), "line1")
	if line.Name == "匿名修改" {
		t.Fatal("anonymous POST mutated the line")
	}
}

// The public entry point redirects to the console login; that page must make
// the standalone web client discoverable with an explicit link. Console auth
// and client key/bind-code pairing remain separate. The link is asserted on
// the anonymous login page only, so no extra access is required.
func TestLoginPageLinksStandaloneClient(t *testing.T) {
	env := newConsoleTestEnv(t)
	recorder := env.request(http.MethodGet, "/console/login", nil, false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /console/login = %d, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `href="/pwa/client.html"`) || !strings.Contains(body, "打开网页版客户端") {
		t.Fatal("login page must expose an explicit standalone web client link")
	}
}

func TestRenderFailureReturns500WithoutPartialHTML(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()

	env.server.tmpl = template.Must(template.New("broken").Parse(
		`{{define "layout"}}<html><body>partial page {{.NoSuchField}}</body></html>{{end}}`,
	))
	recorder := env.get("/console/")
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("render failure status = %d, want 500", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "partial page") {
		t.Fatalf("failed render leaked partial HTML: %q", body)
	}
	if !strings.Contains(body, "模板渲染失败") {
		t.Fatalf("render failure body = %q, want template failure message", body)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("render failure Content-Type = %q, want text/plain (headers were not flushed as HTML)", contentType)
	}
}

func TestByteSizeLabel(t *testing.T) {
	cases := map[int64]string{
		0:       "0 B",
		999:     "999 B",
		4096:    "4.0 KB",
		123456:  "120.6 KB",
		5 << 20: "5.0 MB",
		3 << 30: "3.00 GB",
	}
	for value, want := range cases {
		if got := byteSizeLabel(value); got != want {
			t.Fatalf("byteSizeLabel(%d) = %q, want %q", value, got, want)
		}
	}
}

// TestKeyCreateStaysRevealableWithoutSecretsInURL is the core console repair:
// a freshly generated key is created sealed-server-side, the redirect carries
// only the key id, the full document reclaims the secret for the authenticated
// admin, and a later POST+CSRF reveal returns key and pairing payload with no
// secret ever reaching a URL, cache or log.
func TestKeyCreateStaysRevealableWithoutSecretsInURL(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	ctx := context.Background()

	form := url.Values{"csrf": {testCSRF}, "name": {"可恢复密钥"}, "all_lines": {"on"}}
	recorder := env.post("/console/keys", form)
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("key create status = %d, want 303; body=%q", recorder.Code, recorder.Body.String())
	}
	location := recorder.Header().Get("Location")
	for _, leak := range []string{"new=", "payload=", "secret=", "enrollmentKey="} {
		if strings.Contains(location, leak) {
			t.Fatalf("create redirect leaked %q material: %q", leak, location)
		}
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse location %q: %v", location, err)
	}
	created := parsed.Query().Get("created")
	if !strings.HasPrefix(created, "key_") {
		t.Fatalf("created key id = %q, want key_ prefix", created)
	}
	secret, err := env.store.EnrollmentKeySecret(ctx, created)
	if err != nil || secret == "" {
		t.Fatalf("sealed secret not recoverable: %q (%v)", secret, err)
	}

	// At rest the ciphertext must not contain the secret; only the hash and
	// AEAD blob exist.
	var sealed []byte
	if err := env.store.DB.QueryRowContext(ctx,
		`SELECT secret_encrypted FROM enrollment_keys WHERE id = ?`, created).Scan(&sealed); err != nil {
		t.Fatalf("read sealed secret: %v", err)
	}
	if len(sealed) == 0 || strings.Contains(string(sealed), secret) {
		t.Fatal("stored value is not sealed ciphertext")
	}

	// The immediate authenticated response shows the full material once.
	createdPage := env.get(location)
	if createdPage.Code != http.StatusOK {
		t.Fatalf("created page status = %d, want 200", createdPage.Code)
	}
	createdBody := createdPage.Body.String()
	if !strings.Contains(createdBody, created+"."+secret) {
		t.Fatal("created page does not show the full enrollment key")
	}
	if !strings.Contains(createdBody, `id="keys-csrf"`) {
		t.Fatal("created page is missing the CSRF carrier for reveal")
	}

	// Revisiting /console/keys without the created marker must not render the
	// secret, but must offer the masked reveal control and recoverable status.
	listBody := env.get("/console/keys").Body.String()
	if strings.Contains(listBody, secret) || strings.Contains(listBody, created+"."+secret) {
		t.Fatal("secret rendered on the plain key list")
	}
	if !strings.Contains(listBody, "可查看") {
		t.Fatal("recoverable key is not marked as revealable")
	}
	if !strings.Contains(listBody, `data-reveal-url="/console/keys/`+created+`/reveal"`) {
		t.Fatal("recoverable key has no reveal endpoint hook")
	}

	// Reveal is POST-only and CSRF-gated: a GET is handled by the console's
	// generic GET subtree and must never return secret material.
	if got := env.get("/console/keys/" + created + "/reveal"); got.Code != http.StatusOK ||
		strings.Contains(got.Body.String(), secret) ||
		strings.Contains(got.Body.String(), created+"."+secret) {
		t.Fatalf("GET reveal = %d, must not return secret material", got.Code)
	}
	if bad := env.post("/console/keys/"+created+"/reveal", url.Values{}); bad.Code != http.StatusForbidden {
		t.Fatalf("reveal without CSRF status = %d, want 403", bad.Code)
	}
	if anon := env.request(http.MethodPost, "/console/keys/"+created+"/reveal",
		url.Values{"csrf": {testCSRF}}, false); anon.Code != http.StatusSeeOther {
		t.Fatalf("anonymous reveal status = %d, want redirect to login", anon.Code)
	}

	reveal := env.post("/console/keys/"+created+"/reveal", url.Values{"csrf": {testCSRF}})
	if reveal.Code != http.StatusOK {
		t.Fatalf("reveal status = %d, want 200; body=%q", reveal.Code, reveal.Body.String())
	}
	if cache := reveal.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("reveal Cache-Control = %q, want no-store", cache)
	}
	var payload struct {
		KeyID          string `json:"keyId"`
		EnrollmentKey  string `json:"enrollmentKey"`
		PairingPayload string `json:"pairingPayload"`
		QRDataURI      string `json:"qrDataUri"`
	}
	if err := json.Unmarshal(reveal.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode reveal: %v", err)
	}
	if payload.KeyID != created || payload.EnrollmentKey != created+"."+secret {
		t.Fatalf("reveal key material mismatch: %+v", payload)
	}
	if !strings.Contains(payload.PairingPayload, created+"."+secret) {
		t.Fatal("pairing payload does not carry the enrollment key")
	}
	if !strings.HasPrefix(payload.QRDataURI, "data:image/png;base64,") {
		t.Fatal("reveal did not include a QR image")
	}

	// The reveal is audited by id only; the secret is never an audit value.
	audit, err := env.store.ListAudit(ctx, 20)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	revealed := false
	for _, entry := range audit {
		if strings.Contains(entry.Target, secret) || strings.Contains(entry.Result, secret) {
			t.Fatal("audit leaked the key secret")
		}
		if entry.Action == "key.reveal" && entry.Target == created {
			revealed = true
		}
	}
	if !revealed {
		t.Fatal("reveal was not audited")
	}
}

// TestRevealReportsLegacyAndRevokedKeysHonestly covers the irreversible old
// hash-only keys (explicit cannot-recover status plus replacement path) and
// revoked keys, without touching their authentication or devices.
func TestRevealReportsLegacyAndRevokedKeysHonestly(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	ctx := context.Background()

	legacySecret := "legacy-irreversible-secret"
	legacyHash := sha256.Sum256([]byte(legacySecret))
	if _, err := env.store.DB.ExecContext(ctx, `INSERT INTO enrollment_keys
		(id, name, secret_hash, all_lines, created_at) VALUES ('key_legacy', '旧密钥', ?, 1, ?)`,
		legacyHash[:], time.Now().UnixMilli()); err != nil {
		t.Fatalf("insert legacy key: %v", err)
	}

	// Hash-only keys must still authenticate: device recovery/pairing keeps
	// working after the migration.
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	deviceName := "legacy-phone"
	message := "key_legacy\n" + legacySecret + "\n" + env.server.Identity.ID + "\n" + deviceName
	proof := ed25519.Sign(private, []byte(message))
	if _, err := env.auth.Enroll(ctx, auth.EnrollmentRequest{
		EnrollmentKey: "key_legacy." + legacySecret, DeviceName: deviceName,
		DevicePublicKey: public, Proof: proof,
	}); err != nil {
		t.Fatalf("legacy key stopped authenticating: %v", err)
	}

	recorder := env.post("/console/keys/key_legacy/reveal", url.Values{"csrf": {testCSRF}})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("legacy reveal status = %d, want 409", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "unrecoverable") {
		t.Fatalf("legacy reveal body = %q, want unrecoverable marker", recorder.Body.String())
	}
	body := env.get("/console/keys").Body.String()
	if !strings.Contains(body, "旧密钥未保存原文") {
		t.Fatal("legacy key is not marked as unrecoverable")
	}
	if !strings.Contains(body, "创建新密钥") || !strings.Contains(body, `href="#create-key"`) {
		t.Fatal("legacy key does not offer the explicit replacement workflow")
	}

	// A revoked key is never revealed, even when its sealed copy exists.
	revoked, _, err := env.auth.CreateKey(ctx, "已撤销", true)
	if err != nil {
		t.Fatalf("create revoked key: %v", err)
	}
	if err := env.auth.RevokeKey(ctx, revoked); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	recorder = env.post("/console/keys/"+revoked+"/reveal", url.Values{"csrf": {testCSRF}})
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "revoked") {
		t.Fatalf("revoked reveal = %d %q, want 409 revoked", recorder.Code, recorder.Body.String())
	}
}
