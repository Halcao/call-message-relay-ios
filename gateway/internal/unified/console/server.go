// Package console is the unified operator console: session login migrated
// from the existing Caddy bcrypt hash, line registry, enrollment keys,
// devices, calls/conferences, voicemail and settings. It is served behind
// Caddy TLS on the same origin as /api/v2.
package console

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/runtime"
	"callrelay/gateway/internal/unified/store"

	"github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "callrelay_console"

type Server struct {
	Cfg      config.Config
	Store    *store.Store
	Manager  *runtime.Manager
	Auth     *auth.Service
	Identity *identity.Identity

	mu       sync.Mutex
	attempts map[string][]time.Time
	tmpl     *template.Template
	secure   bool
}

func NewServer(cfg config.Config, st *store.Store, manager *runtime.Manager, authService *auth.Service, gatewayIdentity *identity.Identity) *Server {
	return &Server{
		Cfg: cfg, Store: st, Manager: manager, Auth: authService, Identity: gatewayIdentity,
		attempts: make(map[string][]time.Time),
		tmpl: template.Must(template.New("console").Funcs(template.FuncMap{
			"ts": func(milliseconds int64) string {
				if milliseconds <= 0 {
					return "—"
				}
				return time.UnixMilli(milliseconds).Local().Format("2006-01-02 15:04:05")
			},
			"msduration": func(milliseconds int64) string {
				seconds := milliseconds / 1000
				return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
			},
			"bytes": byteSizeLabel,
			"lineName": func(names map[string]string, id string) string {
				if strings.TrimSpace(id) == "" {
					return "—"
				}
				if name := strings.TrimSpace(names[id]); name != "" {
					return name
				}
				return id
			},
			"directionLabel":       directionLabel,
			"stateLabel":           stateLabel,
			"eventLabel":           eventLabel,
			"conferenceStateLabel": conferenceStateLabel,
			"endReasonLabel":       endReasonLabel,
		}).Parse(consoleTemplates)),
		secure: !strings.HasPrefix(strings.ToLower(cfg.Gateway.PublicBaseURL), "http://"),
	}
}

// byteSizeLabel renders storage sizes for humans without exponent clutter.
func byteSizeLabel(value int64) string {
	switch {
	case value >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(value)/(1<<30))
	case value >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(value)/(1<<20))
	case value >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(value)/(1<<10))
	default:
		return fmt.Sprintf("%d B", value)
	}
}

// directionLabel translates the stored call direction tokens.
func directionLabel(direction string) string {
	switch direction {
	case "inbound":
		return "呼入"
	case "outbound":
		return "呼出"
	default:
		if strings.TrimSpace(direction) == "" {
			return "—"
		}
		return direction
	}
}

// eventLabel translates the overview event stream tokens into short Chinese
// labels; unknown/empty values fall back to a dash or the raw token.
func eventLabel(kind string) string {
	if strings.TrimSpace(kind) == "" {
		return "—"
	}
	prefix, _, _ := strings.Cut(kind, ".")
	switch prefix {
	case "call":
		return "通话"
	case "message":
		return "短信"
	case "line":
		return "线路"
	case "recording":
		return "录音"
	case "voicemail":
		return "留言"
	case "harassment":
		return "拦截"
	case "contacts":
		return "通讯录"
	case "worker":
		return "模块"
	case "push":
		return "推送"
	}
	return kind
}

// stateLabel translates call and voicemail state tokens.
func stateLabel(state string) string {
	switch state {
	case "incoming_ringing":
		return "来电振铃"
	case "outgoing_dialing":
		return "正在外呼"
	case "answering":
		return "接听中"
	case "connecting":
		return "接通中"
	case "active":
		return "通话中"
	case "held":
		return "保持中"
	case "ending":
		return "结束中"
	case "ended":
		return "已结束"
	case "voicemail_answering":
		return "留言接听"
	case "voicemail_recording":
		return "留言录制"
	case "ready":
		return "可播放"
	case "recording":
		return "录制中"
	case "unavailable":
		return "不可用"
	default:
		if strings.TrimSpace(state) == "" {
			return "—"
		}
		return state
	}
}

// conferenceStateLabel translates conference state tokens.
func conferenceStateLabel(state string) string {
	switch state {
	case "active":
		return "进行中"
	case "host_disconnected":
		return "主持人掉线"
	case "ended":
		return "已结束"
	default:
		if strings.TrimSpace(state) == "" {
			return "—"
		}
		return state
	}
}

// endReasonLabel translates the system end reasons; unknown worker reasons
// stay readable as-is.
func endReasonLabel(reason string) string {
	switch reason {
	case "":
		return "—"
	case "console_hangup":
		return "控制台挂断"
	case "console_reject":
		return "控制台拒接"
	case "removed_from_conference":
		return "移出会议"
	case "remote_hangup":
		return "对方挂断"
	case "device_hangup":
		return "手机挂断"
	case "device_reject":
		return "手机拒接"
	case "no_answer":
		return "无人接听"
	case "hung_up":
		return "已挂断"
	default:
		return reason
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /console/login", s.loginPage)
	mux.HandleFunc("POST /console/login", s.loginSubmit)
	mux.HandleFunc("POST /console/logout", s.withSession(s.logout))
	mux.HandleFunc("GET /console/", s.withSession(s.overview))
	mux.HandleFunc("GET /console/lines", s.withSession(s.linesPage))
	mux.HandleFunc("POST /console/lines/{id}", s.withSession(s.lineUpdate))
	mux.HandleFunc("POST /console/lines/{id}/number", s.withSession(s.lineNumber))
	mux.HandleFunc("POST /console/lines/{id}/refresh-identity", s.withSession(s.lineRefreshIdentity))
	mux.HandleFunc("GET /console/keys", s.withSession(s.keysPage))
	mux.HandleFunc("POST /console/keys", s.withSession(s.keyCreate))
	mux.HandleFunc("POST /console/keys/{id}/reveal", s.withSession(s.keyReveal))
	mux.HandleFunc("POST /console/keys/{id}/revoke", s.withSession(s.keyRevoke))
	mux.HandleFunc("POST /console/keys/{id}/delete", s.withSession(s.keyDelete))
	mux.HandleFunc("POST /console/keys/{id}/lines", s.withSession(s.keyGrant))
	mux.HandleFunc("POST /console/keys/{id}/lines/{line}/delete", s.withSession(s.keyGrantDelete))
	mux.HandleFunc("POST /console/keys/{id}/manage-number", s.withSession(s.keyManageNumber))
	mux.HandleFunc("GET /console/devices", s.withSession(s.devicesPage))
	mux.HandleFunc("POST /console/devices/{id}/revoke", s.withSession(s.deviceRevoke))
	mux.HandleFunc("POST /console/devices/{id}/allow-reenroll", s.withSession(s.deviceAllowReenroll))
	mux.HandleFunc("GET /console/calls", s.withSession(s.callsPage))
	mux.HandleFunc("POST /console/calls/{id}/hangup", s.withSession(s.callHangup))
	mux.HandleFunc("POST /console/calls/{id}/reject", s.withSession(s.callReject))
	mux.HandleFunc("GET /console/conferences", s.withSession(s.conferencesPage))
	mux.HandleFunc("POST /console/conferences/{id}/close", s.withSession(s.conferenceClose))
	mux.HandleFunc("POST /console/conferences/{id}/legs/{callId}/hangup", s.withSession(s.conferenceLegHangup))
	mux.HandleFunc("GET /console/voicemails", s.withSession(s.voicemailsPage))
	mux.HandleFunc("POST /console/voicemails/{id}/delete", s.withSession(s.voicemailDelete))
	mux.HandleFunc("GET /console/voicemails/{id}/audio", s.withSession(s.voicemailAudio))
	mux.HandleFunc("GET /console/settings", s.withSession(s.settingsPage))
	mux.HandleFunc("POST /console/settings", s.withSession(s.settingsUpdate))
	mux.HandleFunc("GET /console/diagnostics", s.withSession(s.diagnosticsPage))
	mux.HandleFunc("POST /console/diagnostics/recover/{line}", s.withSession(s.diagnosticsRecover))
	return mux
}

// ---------- credentials ----------

var bcryptPattern = regexp.MustCompile(`\$2[aby]\$\d{2}\$[./A-Za-z0-9]{53}`)

// parseCredentialFile extracts the username and bcrypt hash from the existing
// Caddy basic_auth snippet. The raw hash is never logged or rendered.
func parseCredentialFile(content string) (string, string, bool) {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		hash := bcryptPattern.FindString(trimmed)
		if hash == "" {
			continue
		}
		username := ""
		for _, field := range strings.Fields(trimmed) {
			cleaned := strings.Trim(field, `"'{}`)
			if cleaned == "" || cleaned == hash || strings.HasPrefix(cleaned, "$") {
				continue
			}
			switch cleaned {
			case "basic_auth", "handle", "route":
				continue
			}
			username = cleaned
			break
		}
		return username, hash, true
	}
	return "", "", false
}

func (s *Server) loadCredential() (string, string, error) {
	payload, err := os.ReadFile(s.Cfg.Gateway.ConsoleBcryptFile)
	if err != nil {
		return "", "", err
	}
	username, hash, ok := parseCredentialFile(string(payload))
	if !ok || username == "" {
		return "", "", errors.New("console credential file has no basic_auth entry")
	}
	return username, hash, nil
}

// ---------- sessions ----------

func hashValue(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func randomValue() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *Server) withSession(next func(http.ResponseWriter, *http.Request, store.ConsoleSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil || cookie.Value == "" {
			http.Redirect(w, r, "/console/login", http.StatusSeeOther)
			return
		}
		session, err := s.Store.LookupConsoleSession(r.Context(), hashValue(cookie.Value))
		if err != nil {
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/console", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
			http.Redirect(w, r, "/console/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "表单解析失败", http.StatusBadRequest)
				return
			}
			presented := r.PostFormValue("csrf")
			if subtle.ConstantTimeCompare([]byte(presented), []byte(session.CSRF)) != 1 {
				http.Error(w, "CSRF 校验失败", http.StatusForbidden)
				return
			}
		}
		next(w, r, session)
	}
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, "login", s.baseData(r, store.ConsoleSession{}, ""))
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}
	remote := clientIP(r)
	if !s.allowLogin(remote) {
		s.render(w, "login", s.baseData(r, store.ConsoleSession{}, "尝试过于频繁，请稍后再试"))
		return
	}
	expectedUser, hash, err := s.loadCredential()
	if err != nil {
		s.render(w, "login", s.baseData(r, store.ConsoleSession{}, "控制台凭据未配置"))
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	if username != expectedUser || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.render(w, "login", s.baseData(r, store.ConsoleSession{}, "用户名或密码错误"))
		return
	}
	token, err := randomValue()
	if err != nil {
		http.Error(w, "会话创建失败", http.StatusInternalServerError)
		return
	}
	csrf, err := randomValue()
	if err != nil {
		http.Error(w, "会话创建失败", http.StatusInternalServerError)
		return
	}
	duration, err := s.Cfg.ConsoleSessionDuration()
	if err != nil {
		duration = 12 * time.Hour
	}
	now := time.Now().UTC()
	if err := s.Store.CreateConsoleSession(r.Context(), store.ConsoleSession{
		TokenHash: hashValue(token), CSRF: csrf, Username: username,
		CreatedAt: now, ExpiresAt: now.Add(duration), RemoteAddr: remote,
	}); err != nil {
		http.Error(w, "会话创建失败", http.StatusInternalServerError)
		return
	}
	_ = s.Store.AppendAudit(r.Context(), username, "console.login", remote, "accepted")
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/console", HttpOnly: true, Secure: s.secure,
		SameSite: http.SameSiteLaxMode, Expires: now.Add(duration),
	})
	http.Redirect(w, r, "/console/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	_ = s.Store.DeleteConsoleSession(r.Context(), session.TokenHash)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/console", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	_ = s.Store.AppendAudit(r.Context(), session.Username, "console.logout", clientIP(r), "accepted")
	http.Redirect(w, r, "/console/login", http.StatusSeeOther)
}

func (s *Server) allowLogin(remote string) bool {
	limit := s.Cfg.Settings.ConsoleLoginRatePerMinute
	if limit <= 0 {
		limit = 8
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	window := s.attempts[remote]
	kept := window[:0]
	for _, attempt := range window {
		if now.Sub(attempt) < time.Minute {
			kept = append(kept, attempt)
		}
	}
	if len(kept) >= limit {
		s.attempts[remote] = kept
		return false
	}
	s.attempts[remote] = append(kept, now)
	return true
}

func clientIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		if first, _, found := strings.Cut(forwarded, ","); found {
			return strings.TrimSpace(first)
		}
		return forwarded
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- view data ----------

// grantView is one key-to-line permission row with the display name joined
// so the keys page never shows a raw line id on its own.
type grantView struct {
	store.KeyLine
	LineName string
}

type keyView struct {
	Key    store.EnrollmentKey
	Grants []grantView
	// SecretRecoverable is true when an encrypted operator-recoverable copy
	// exists. Legacy hash-only keys cannot be reconstructed and must show the
	// honest replacement workflow instead of a broken reveal button.
	SecretRecoverable bool
}

// lineFormView carries the persisted line settings the edit form must echo
// back. runtime.LineView holds live status; the store row holds the saved
// reject/allow/retention values that are not part of the runtime status view.
type lineFormView struct {
	runtime.LineView
	RejectPolicy           string
	BlockedPrefixes        string
	RecordingRetentionDays int
	AllowInbound           bool
	AllowOutbound          bool
}

// conferenceConsoleView adds the joined host display name and an Ended
// helper so the template can scope actions to the current conference.
type conferenceConsoleView struct {
	runtime.ConferenceView
	HostName string
}

func (v conferenceConsoleView) Ended() bool { return v.State == "ended" }

type deviceView struct {
	Device         store.Device
	KeyName        string
	LineCount      int
	Online         bool
	PushRegistered bool
}

type newKeyView struct {
	ID           string
	Secret       string
	Payload      string
	QRDataURI    string
	NewlyCreated bool
}

type viewData struct {
	Page             string
	Title            string
	User             string
	CSRF             string
	Flash            string
	Error            string
	Lines            []lineFormView
	LineNames        map[string]string
	Keys             []keyView
	Devices          []deviceView
	Calls            []runtime.CallView
	Conferences      []conferenceConsoleView
	Voicemails       []runtime.VoicemailView
	Events           []map[string]any
	Audit            []store.AuditEntry
	Settings         map[string]string
	NewKey           *newKeyView
	GatewayID        string
	Fingerprint      string
	PublicKey        string
	PushReady        bool
	TURNPolicy       string
	OnlineCount      int
	ActiveCalls      int
	VoicemailBytes   int64
	VoicemailStorage runtime.VoicemailStorageStatus
	Now              time.Time
}

func (s *Server) baseData(r *http.Request, session store.ConsoleSession, flash string) viewData {
	return viewData{
		Page: "login", Title: "登录", User: session.Username, CSRF: session.CSRF,
		Flash: flash, Now: time.Now(),
		GatewayID: s.Identity.ID, Fingerprint: s.Identity.Fingerprint(), PublicKey: s.Identity.PublicBase64(),
	}
}

func (s *Server) newData(r *http.Request, session store.ConsoleSession, page, title string) viewData {
	data := viewData{
		Page: page, Title: title, User: session.Username, CSRF: session.CSRF,
		Flash: strings.TrimSpace(r.URL.Query().Get("flash")),
		Error: strings.TrimSpace(r.URL.Query().Get("error")),
		Now:   time.Now(), GatewayID: s.Identity.ID, Fingerprint: s.Identity.Fingerprint(), PublicKey: s.Identity.PublicBase64(),
	}
	data.LineNames = s.lineNames(r.Context())
	return data
}

func (s *Server) lineNames(ctx context.Context) map[string]string {
	names := map[string]string{}
	if lines, err := s.Store.ListLines(ctx); err == nil {
		for _, line := range lines {
			names[line.ID] = line.Name
		}
	}
	return names
}

// render builds the whole page in memory first: a template error must never
// look like a successful 200 with half a page, so nothing reaches the
// ResponseWriter until execution has completed.
func (s *Server) render(w http.ResponseWriter, page string, data viewData) {
	data.Page = page
	var pageBuffer bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&pageBuffer, "layout", data); err != nil {
		slog.Error("console template render failed", "page", page, "error", err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "模板渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(pageBuffer.Bytes())
}

func redirect(w http.ResponseWriter, r *http.Request, path, flash string) {
	if flash != "" {
		path += "?flash=" + url.QueryEscape(flash)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// ---------- pages ----------

func (s *Server) overview(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	ctx := r.Context()
	data := s.newData(r, session, "overview", "总览")
	data.Lines = s.lineFormViews(ctx)
	for _, line := range data.Lines {
		if line.Online {
			data.OnlineCount++
		}
	}
	active := true
	if calls, err := s.Store.ListCalls(ctx, "", "", active, 100); err == nil {
		for _, call := range calls {
			data.Calls = append(data.Calls, s.Manager.AdminCallView(call))
		}
	}
	data.ActiveCalls = len(data.Calls)
	if devices, err := s.Store.ListDevices(ctx); err == nil {
		data.Devices = s.deviceViews(ctx, devices)
	}
	if keys, err := s.Store.ListEnrollmentKeys(ctx); err == nil {
		data.Keys = s.keyViews(ctx, keys)
	}
	data.Conferences = s.conferenceViews(ctx, true)
	if events, err := s.Manager.RecentEventsForConsole(ctx, 15); err == nil {
		data.Events = events
	}
	if audit, err := s.Store.ListAudit(ctx, 10); err == nil {
		data.Audit = audit
	}
	if voicemails, err := s.Store.ListVoicemails(ctx, "", 1000); err == nil {
		for _, voicemail := range voicemails {
			data.VoicemailBytes += voicemail.SizeBytes
		}
	}
	s.render(w, "overview", data)
}

func (s *Server) linesPage(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	data := s.newData(r, session, "lines", "线路")
	data.Lines = s.lineFormViews(r.Context())
	s.render(w, "lines", data)
}

func (s *Server) lineUpdate(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	lineID := r.PathValue("id")
	line, err := s.Store.GetLine(r.Context(), lineID)
	if err != nil {
		redirect(w, r, "/console/lines", "线路不存在")
		return
	}
	update := store.LineUpdate{}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name != "" {
		update.Name = &name
	}
	enabled := r.PostFormValue("enabled") == "on"
	update.Enabled = &enabled
	allowInbound := r.PostFormValue("allow_inbound") == "on"
	update.AllowInbound = &allowInbound
	allowOutbound := r.PostFormValue("allow_outbound") == "on"
	update.AllowOutbound = &allowOutbound
	policy := r.PostFormValue("reject_policy")
	if policy == "" {
		policy = "none"
	}
	switch policy {
	case "none", "reject_all", "reject_blocked":
	default:
		policy = "none"
	}
	update.RejectPolicy = &policy
	blocked := strings.TrimSpace(r.PostFormValue("blocked_prefixes"))
	update.BlockedPrefixes = &blocked
	if retention, parseErr := strconv.Atoi(r.PostFormValue("recording_retention_days")); parseErr == nil {
		update.RecordingRetentionDays = &retention
	}
	if err := s.Store.UpdateLine(r.Context(), lineID, update); err != nil {
		redirect(w, r, "/console/lines", "保存失败")
		return
	}
	// SMS live is an explicit, real worker toggle; failure is reported, not
	// silently ignored.
	if r.PostFormValue("sms_live") == "on" {
		if err := s.Manager.SetLineSMSDryRun(r.Context(), lineID, false); err != nil {
			redirect(w, r, "/console/lines", "线路已保存，但短信实发切换失败：请检查 worker 状态")
			_ = line
			return
		}
	} else if line.SMSDryRun == false {
		if err := s.Manager.SetLineSMSDryRun(r.Context(), lineID, true); err != nil {
			redirect(w, r, "/console/lines", "线路已保存，但短信 dry-run 切换失败：请检查 worker 状态")
			return
		}
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "line.update", lineID, "accepted")
	redirect(w, r, "/console/lines", "线路设置已保存")
}

// lineNumber saves (or, when blank, clears) the operator manual number
// override. The raw number is persisted only in the daemon's private
// database; the audit target and flash carry no number material.
func (s *Server) lineNumber(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	lineID := r.PathValue("id")
	if _, err := s.Store.GetLine(r.Context(), lineID); err != nil {
		redirect(w, r, "/console/lines", "线路不存在")
		return
	}
	number := strings.TrimSpace(r.PostFormValue("manual_number"))
	if err := s.Manager.SetManualNumber(r.Context(), lineID, number); err != nil {
		redirect(w, r, "/console/lines", "号码保存失败")
		return
	}
	result := "set"
	if number == "" {
		result = "cleared"
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "line.number.manual", lineID, result)
	redirect(w, r, "/console/lines", "号码设置已保存")
}

// lineRefreshIdentity triggers an immediate bounded read-only own-number /
// SIM refresh through the worker queue.
func (s *Server) lineRefreshIdentity(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	lineID := r.PathValue("id")
	if _, err := s.Manager.RefreshLineIdentity(r.Context(), lineID); err != nil {
		redirect(w, r, "/console/lines", "重新读取 SIM 号码失败：请检查 worker 状态")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "line.identity.refresh", lineID, "accepted")
	redirect(w, r, "/console/lines", "已重新读取 SIM 号码")
}

// NumberLabel renders the friendly own-number text for the authenticated
// console: the operator is allowed to see the line's FULL number, so it is
// shown verbatim (manual override first, then SIM number). Empty/unsupported
// states get an honest, non-technical placeholder; ICCID/IMSI never appear.
func (v lineFormView) NumberLabel() string {
	if number := strings.TrimSpace(v.PhoneNumber); number != "" {
		return number
	}
	switch v.Identity.NumberSource {
	case "empty":
		return "SIM 未存储号码，可在下方手动设置"
	case "unsupported":
		return "模块暂不支持读取，可手动设置"
	case "sim_changed":
		return "SIM 已更换，待确认"
	case "error":
		return "暂时读取失败"
	}
	return "暂无号码"
}

// NumberKind returns a short friendly provenance label.
func (v lineFormView) NumberKind() string {
	switch v.Identity.NumberSource {
	case "manual":
		return "手动"
	case "sim":
		return "SIM"
	default:
		return ""
	}
}

// lineFormViews merges live runtime status with the persisted line row so the
// operator form can show the actual saved values instead of hardcoded
// defaults.
func (s *Server) lineFormViews(ctx context.Context) []lineFormView {
	statuses := s.Manager.AdminLineViews(ctx)
	byID := make(map[string]lineFormView, len(statuses))
	order := make([]string, 0, len(statuses))
	for _, status := range statuses {
		byID[status.ID] = lineFormView{LineView: status}
		order = append(order, status.ID)
	}
	if lines, err := s.Store.ListLines(ctx); err == nil {
		for _, line := range lines {
			form, exists := byID[line.ID]
			if !exists {
				form = lineFormView{LineView: runtime.LineView{ID: line.ID, Name: line.Name, Enabled: line.Enabled}}
				order = append(order, line.ID)
			}
			form.RejectPolicy = line.RejectPolicy
			form.BlockedPrefixes = line.BlockedPrefixes
			form.RecordingRetentionDays = line.RecordingRetentionDays
			form.AllowInbound = line.AllowInbound
			form.AllowOutbound = line.AllowOutbound
			byID[line.ID] = form
		}
	}
	views := make([]lineFormView, 0, len(order))
	for _, id := range order {
		views = append(views, byID[id])
	}
	return views
}

// conferenceViews joins the host device display name and keeps the runtime
// view fields for the console template.
func (s *Server) conferenceViews(ctx context.Context, activeOnly bool) []conferenceConsoleView {
	conferences, err := s.Manager.AdminConferenceViews(ctx, activeOnly)
	if err != nil {
		return nil
	}
	hostNames := map[string]string{}
	if devices, err := s.Store.ListDevices(ctx); err == nil {
		for _, device := range devices {
			hostNames[device.ID] = device.Name
		}
	}
	views := make([]conferenceConsoleView, 0, len(conferences))
	for _, conference := range conferences {
		views = append(views, conferenceConsoleView{ConferenceView: conference, HostName: hostNames[conference.HostDeviceID]})
	}
	return views
}

func (s *Server) keyViews(ctx context.Context, keys []store.EnrollmentKey) []keyView {
	lineNames := s.lineNames(ctx)
	views := make([]keyView, 0, len(keys))
	for _, key := range keys {
		grants, _ := s.Store.KeyLines(ctx, key.ID)
		rows := make([]grantView, 0, len(grants))
		for _, grant := range grants {
			rows = append(rows, grantView{KeyLine: grant, LineName: strings.TrimSpace(lineNames[grant.LineID])})
		}
		recoverable, _ := s.Store.KeySecretStored(ctx, key.ID)
		views = append(views, keyView{Key: key, Grants: rows, SecretRecoverable: recoverable})
	}
	return views
}

func (s *Server) keysPage(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	data := s.newData(r, session, "keys", "配对密钥")
	data.Lines = s.lineFormViews(r.Context())
	if keys, err := s.Store.ListEnrollmentKeys(r.Context()); err == nil {
		data.Keys = s.keyViews(r.Context(), keys)
	}
	// A freshly created key is shown in full exactly on this authenticated
	// response. The URL carries only the key id: the secret and pairing
	// payload are decrypted from the server-side sealed copy, never from a
	// query string, referrer or browser history.
	if created := strings.TrimSpace(r.URL.Query().Get("created")); created != "" {
		key, _, err := s.Store.GetEnrollmentKey(r.Context(), created)
		if err == nil && key.RevokedAt == nil {
			secret, secretErr := s.Store.EnrollmentKeySecret(r.Context(), created)
			if secretErr == nil {
				if view := s.newKeyView(created, secret); view != nil {
					view.NewlyCreated = true
					data.NewKey = view
				}
			}
		}
	}
	s.render(w, "keys", data)
}

// newKeyView assembles the one-time display material for a recoverable key:
// the full enrollment key (`keyId.secret`), the paste/scan JSON payload and
// its QR image. It never reads or writes any URL material.
func (s *Server) newKeyView(keyID, secret string) *newKeyView {
	enrollmentKey := keyID + "." + secret
	payload, err := json.Marshal(map[string]string{
		"gatewayId": s.Identity.ID, "gatewayName": s.Identity.Name,
		"baseURL": s.Cfg.Gateway.PublicBaseURL, "enrollmentKey": enrollmentKey,
		"fingerprint": s.Identity.Fingerprint(), "apiVersion": "v2",
	})
	if err != nil {
		return nil
	}
	view := &newKeyView{ID: keyID, Secret: secret, Payload: string(payload)}
	if png, err := qrcode.Encode(string(payload), qrcode.Medium, 320); err == nil {
		view.QRDataURI = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	}
	return view
}

// keyReveal is the admin-only reveal endpoint: POST + session + CSRF, and the
// answer is JSON so the secret never appears in a URL, cache or access log.
// Revoked keys are not revealed; legacy hash-only keys answer 409 with an
// explicit unrecoverable status so the UI can offer the replacement workflow.
func (s *Server) keyReveal(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID := r.PathValue("id")
	key, _, err := s.Store.GetEnrollmentKey(r.Context(), keyID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "密钥不存在"})
		return
	}
	if key.RevokedAt != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "revoked", "message": "密钥已撤销，不再提供显示或复制"})
		return
	}
	secret, err := s.Store.EnrollmentKeySecret(r.Context(), keyID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrSecretNotStored):
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":   "unrecoverable",
				"message": "该密钥创建时只保存了校验值，无法恢复显示或复制；既有设备与密钥仍可正常使用，不会被自动撤销。",
			})
		case errors.Is(err, store.ErrSecretStorageUnavailable):
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "storage_unavailable", "message": "可恢复密钥存储未配置，无法显示密钥",
			})
		default:
			slog.Error("console key reveal failed", "key_id", keyID, "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": "显示密钥失败"})
		}
		return
	}
	view := s.newKeyView(keyID, secret)
	if view == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": "生成配对内容失败"})
		return
	}
	// Audit records the fact of the reveal only; the secret is never logged.
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.reveal", keyID, "accepted")
	writeJSON(w, http.StatusOK, map[string]string{
		"keyId":          view.ID,
		"enrollmentKey":  keyID + "." + secret,
		"pairingPayload": view.Payload,
		"qrDataUri":      view.QRDataURI,
	})
}

// writeJSON answers with no-store JSON and never caches secret material.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (s *Server) keyCreate(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		name = "未命名密钥"
	}
	allLines := r.PostFormValue("all_lines") == "on"
	manageNumber := r.PostFormValue("manage_number") == "on"
	keyID, _, err := s.Auth.CreateKey(r.Context(), name, allLines)
	if err != nil {
		redirect(w, r, "/console/keys", "创建失败")
		return
	}
	if manageNumber {
		if err := s.Auth.SetKeyManageNumber(r.Context(), keyID, true); err != nil {
			redirect(w, r, "/console/keys", "号码管理授权失败")
			return
		}
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.create", keyID, "accepted")
	// The redirect carries only the key id; the page decrypts the sealed
	// secret server-side for this authenticated response.
	http.Redirect(w, r, "/console/keys?created="+url.QueryEscape(keyID), http.StatusSeeOther)
}

func (s *Server) keyRevoke(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID := r.PathValue("id")
	if err := s.Auth.RevokeKey(r.Context(), keyID); err != nil {
		redirect(w, r, "/console/keys", "撤销失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.revoke", keyID, "accepted")
	redirect(w, r, "/console/keys", "密钥已撤销，关联设备与新注册立即失效")
}

// keyDelete permanently removes an already-revoked enrollment key and its
// explicit grants. It is a separate POST + CSRF action from revoke; active
// keys are rejected so an operator can never delete a key a device still
// uses. History, device rows and the revoked-installation block list are
// preserved.
func (s *Server) keyDelete(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID := r.PathValue("id")
	switch err := s.Auth.DeleteRevokedKey(r.Context(), keyID); {
	case err == nil:
		_ = s.Store.AppendAudit(r.Context(), session.Username, "key.delete", keyID, "accepted")
		redirect(w, r, "/console/keys", "已删除该撤销密钥及其线路授权；设备与历史记录保留")
	case errors.Is(err, store.ErrKeyActive):
		redirect(w, r, "/console/keys", "只能删除已撤销的密钥，请先撤销")
	case errors.Is(err, store.ErrNotFound):
		redirect(w, r, "/console/keys", "密钥不存在或已删除")
	default:
		slog.Error("console key delete failed", "key_id", keyID, "error", err)
		redirect(w, r, "/console/keys", "删除失败")
	}
}

func (s *Server) keyGrant(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID := r.PathValue("id")
	lineID := strings.TrimSpace(r.PostFormValue("line_id"))
	if lineID == "" {
		redirect(w, r, "/console/keys", "请选择线路")
		return
	}
	entry := store.KeyLine{
		KeyID: keyID, LineID: lineID,
		ReceiveSMS:   r.PostFormValue("receive_sms") == "on",
		ReceiveCalls: r.PostFormValue("receive_calls") == "on",
		SendSMS:      r.PostFormValue("send_sms") == "on",
		Dial:         r.PostFormValue("dial") == "on",
	}
	if err := s.Store.SetKeyLine(r.Context(), entry); err != nil {
		redirect(w, r, "/console/keys", "授权失败")
		return
	}
	s.Auth.BumpVersion()
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.grant", keyID+"/"+lineID, "accepted")
	redirect(w, r, "/console/keys", "线路授权已更新")
}

func (s *Server) keyGrantDelete(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID, lineID := r.PathValue("id"), r.PathValue("line")
	if err := s.Store.DeleteKeyLine(r.Context(), keyID, lineID); err != nil {
		redirect(w, r, "/console/keys", "删除授权失败")
		return
	}
	s.Auth.BumpVersion()
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.ungrant", keyID+"/"+lineID, "accepted")
	redirect(w, r, "/console/keys", "授权已移除")
}

// keyManageNumber toggles the explicit capability for a key's devices to
// edit own numbers on lines the key is authorized to use. It changes no line
// grants and is independent of the all-lines default, so shared keys stay
// read-only until deliberately enabled.
func (s *Server) keyManageNumber(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID := r.PathValue("id")
	allowed := r.PostFormValue("manage_number") == "on"
	if err := s.Auth.SetKeyManageNumber(r.Context(), keyID, allowed); err != nil {
		redirect(w, r, "/console/keys", "更新号码管理授权失败")
		return
	}
	result := "granted"
	if !allowed {
		result = "revoked"
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.manage_number", keyID, result)
	redirect(w, r, "/console/keys", "号码管理授权已更新")
}

func (s *Server) deviceViews(ctx context.Context, devices []store.Device) []deviceView {
	keys, _ := s.Store.ListEnrollmentKeys(ctx)
	keyNames := map[string]string{}
	for _, key := range keys {
		keyNames[key.ID] = key.Name
	}
	views := make([]deviceView, 0, len(devices))
	for _, device := range devices {
		view := deviceView{Device: device, KeyName: keyNames[device.KeyID], PushRegistered: device.APNSToken != "" || device.VoIPToken != ""}
		if access, err := s.Store.DeviceAccess(ctx, device.ID); err == nil {
			view.LineCount = len(access.Lines)
		}
		views = append(views, view)
	}
	return views
}

func (s *Server) devicesPage(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	data := s.newData(r, session, "devices", "设备")
	if devices, err := s.Store.ListDevices(r.Context()); err == nil {
		data.Devices = s.deviceViews(r.Context(), devices)
	}
	s.render(w, "devices", data)
}

func (s *Server) deviceRevoke(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	deviceID := r.PathValue("id")
	if err := s.Auth.RevokeDevice(r.Context(), deviceID); err != nil {
		redirect(w, r, "/console/devices", "撤销失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "device.revoke", deviceID, "accepted")
	redirect(w, r, "/console/devices", "设备已撤销，该安装不会自动重新注册")
}

func (s *Server) deviceAllowReenroll(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	deviceID := r.PathValue("id")
	device, err := s.Store.GetDevice(r.Context(), deviceID)
	if err != nil {
		redirect(w, r, "/console/devices", "设备不存在")
		return
	}
	if err := s.Store.ClearRevokedPublicKey(r.Context(), device.PublicKey); err != nil {
		redirect(w, r, "/console/devices", "解除绑定失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "device.allow-reenroll", deviceID, "accepted")
	redirect(w, r, "/console/devices", "已允许该安装重新注册（需要明确操作）")
}

func (s *Server) callsPage(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	data := s.newData(r, session, "calls", "通话")
	calls, err := s.Store.ListCalls(r.Context(), "", "", false, 100)
	if err == nil {
		for _, call := range calls {
			data.Calls = append(data.Calls, s.Manager.AdminCallView(call))
		}
	}
	s.render(w, "calls", data)
}

func (s *Server) callHangup(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	if err := s.Manager.HangupAsSystem(r.Context(), r.PathValue("id"), "console_hangup"); err != nil {
		redirect(w, r, "/console/calls", "挂断失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "call.hangup", r.PathValue("id"), "accepted")
	redirect(w, r, "/console/calls", "通话已挂断")
}

func (s *Server) callReject(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	if err := s.Manager.RejectAsSystem(r.Context(), r.PathValue("id"), "console_reject"); err != nil {
		redirect(w, r, "/console/calls", "拒绝失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "call.reject", r.PathValue("id"), "accepted")
	redirect(w, r, "/console/calls", "来电已拒绝")
}

func (s *Server) conferencesPage(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	data := s.newData(r, session, "conferences", "会议")
	data.Conferences = s.conferenceViews(r.Context(), false)
	s.render(w, "conferences", data)
}

func (s *Server) conferenceClose(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	if err := s.Manager.CloseConferenceAsSystem(r.Context(), r.PathValue("id"), "console_close"); err != nil {
		redirect(w, r, "/console/conferences", "结束失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "conference.close", r.PathValue("id"), "accepted")
	redirect(w, r, "/console/conferences", "会议已结束")
}

func (s *Server) conferenceLegHangup(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	if err := s.Manager.RemoveLegAsSystem(r.Context(), r.PathValue("id"), r.PathValue("callId")); err != nil {
		redirect(w, r, "/console/conferences", "移除失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "conference.remove-leg", r.PathValue("callId"), "accepted")
	redirect(w, r, "/console/conferences", "会议成员已移除")
}

func (s *Server) voicemailsPage(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	data := s.newData(r, session, "voicemails", "语音留言")
	if voicemails, err := s.Store.ListVoicemails(r.Context(), "", 200); err == nil {
		for _, voicemail := range voicemails {
			data.Voicemails = append(data.Voicemails, s.Manager.AdminVoicemailView(voicemail))
			data.VoicemailBytes += voicemail.SizeBytes
		}
	}
	data.VoicemailStorage = s.Manager.VoicemailStorageStatus()
	s.render(w, "voicemails", data)
}

func (s *Server) voicemailDelete(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	voicemail, err := s.Store.GetVoicemail(r.Context(), r.PathValue("id"))
	if err != nil {
		redirect(w, r, "/console/voicemails", "留言不存在")
		return
	}
	if voicemail.Path != "" {
		_ = os.Remove(voicemail.Path)
	}
	_ = s.Store.DeleteVoicemail(r.Context(), voicemail.ID)
	_ = s.Store.AppendAudit(r.Context(), session.Username, "voicemail.delete", voicemail.ID, "accepted")
	redirect(w, r, "/console/voicemails", "留言已删除")
}

func (s *Server) voicemailAudio(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	voicemail, err := s.Store.GetVoicemail(r.Context(), r.PathValue("id"))
	if err != nil || voicemail.State != "ready" {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(voicemail.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, voicemail.ID+".wav", stat.ModTime(), file)
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	data := s.newData(r, session, "settings", "设置")
	data.Settings = s.settingsMap(r.Context())
	s.render(w, "settings", data)
}

func (s *Server) settingsMap(ctx context.Context) map[string]string {
	values := map[string]string{
		"max_concurrent_calls":      strconv.Itoa(s.Cfg.Settings.MaxConcurrentCalls),
		"disconnect_grace_seconds":  strconv.Itoa(s.Cfg.Settings.DisconnectGraceSeconds),
		"voicemail_enabled":         "true",
		"voicemail_ring_seconds":    "30",
		"voicemail_offline_seconds": "8",
		"voicemail_max_seconds":     "120",
		"voicemail_retention_days":  "30",
		"voicemail_max_bytes":       strconv.FormatInt(500<<20, 10),
		"voicemail_min_free_bytes":  strconv.FormatInt(256<<20, 10),
	}
	for key := range values {
		if value, err := s.Store.Setting(ctx, key); err == nil && strings.TrimSpace(value) != "" {
			values[key] = strings.TrimSpace(value)
		}
	}
	// voicemail_enabled is stored as 1/0 by settingsUpdate but defaults to
	// "true" from config; normalize so the checkbox reflects saved state.
	values["voicemail_enabled"] = boolSetting(values["voicemail_enabled"])
	values["voicemail_max_bytes_mb"] = bytesToMB(values["voicemail_max_bytes"])
	values["voicemail_min_free_bytes_mb"] = bytesToMB(values["voicemail_min_free_bytes"])
	return values
}

// bytesToMB converts a stored byte count into the whole-MB number the
// settings form edits.
func bytesToMB(value string) string {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return "0"
	}
	return strconv.FormatInt(parsed>>20, 10)
}

// boolSetting normalizes the several boolean spellings used across config and
// store into the 1/0 the settings form checks for.
func boolSetting(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return "1"
	default:
		return "0"
	}
}

func (s *Server) settingsUpdate(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	set := func(key string, value int64) bool {
		if err := s.Store.SetSetting(r.Context(), key, strconv.FormatInt(value, 10)); err != nil {
			return false
		}
		return true
	}
	if value, err := strconv.Atoi(r.PostFormValue("max_concurrent_calls")); err == nil && value >= 1 && value <= 32 {
		set("max_concurrent_calls", int64(value))
	}
	if value, err := strconv.Atoi(r.PostFormValue("disconnect_grace_seconds")); err == nil && value >= 5 && value <= 600 {
		set("disconnect_grace_seconds", int64(value))
	}
	set("voicemail_enabled", boolToInt(r.PostFormValue("voicemail_enabled") == "on"))
	if value, err := strconv.Atoi(r.PostFormValue("voicemail_ring_seconds")); err == nil && value >= 5 && value <= 300 {
		set("voicemail_ring_seconds", int64(value))
	}
	if value, err := strconv.Atoi(r.PostFormValue("voicemail_offline_seconds")); err == nil && value >= 2 && value <= 300 {
		set("voicemail_offline_seconds", int64(value))
	}
	if value, err := strconv.Atoi(r.PostFormValue("voicemail_max_seconds")); err == nil && value >= 10 && value <= 600 {
		set("voicemail_max_seconds", int64(value))
	}
	if value, err := strconv.Atoi(r.PostFormValue("voicemail_retention_days")); err == nil && value >= 1 && value <= 3650 {
		set("voicemail_retention_days", int64(value))
	}
	if value, err := strconv.ParseInt(r.PostFormValue("voicemail_max_bytes_mb"), 10, 64); err == nil && value >= 10 && value <= 102400 {
		set("voicemail_max_bytes", value<<20)
	}
	if value, err := strconv.ParseInt(r.PostFormValue("voicemail_min_free_bytes_mb"), 10, 64); err == nil && value >= 16 && value <= 102400 {
		set("voicemail_min_free_bytes", value<<20)
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "settings.update", "unified", "accepted")
	redirect(w, r, "/console/settings", "设置已保存")
}

func boolToInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func (s *Server) diagnosticsPage(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	data := s.newData(r, session, "diagnostics", "诊断")
	data.Lines = s.lineFormViews(r.Context())
	data.Settings = s.settingsMap(r.Context())
	data.PushReady = s.Manager.PushConfigured()
	data.TURNPolicy = s.Manager.TURNPolicy()
	data.VoicemailStorage = s.Manager.VoicemailStorageStatus()
	if events, err := s.Manager.RecentEventsForConsole(r.Context(), 30); err == nil {
		data.Events = events
	}
	if audit, err := s.Store.ListAudit(r.Context(), 30); err == nil {
		data.Audit = audit
	}
	s.render(w, "diagnostics", data)
}

func (s *Server) diagnosticsRecover(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	lineID := r.PathValue("line")
	if err := s.Manager.RecoverLine(r.Context(), lineID); err != nil {
		redirect(w, r, "/console/diagnostics", "恢复失败："+err.Error())
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "line.recover", lineID, "accepted")
	redirect(w, r, "/console/diagnostics", "线路恢复探测完成")
}
