// Admin JSON API (build 68): the additive, authenticated machine interface
// the native iOS management screens use. It is deliberately the SAME
// package, session store and business logic as the HTML console (no HTML
// scraping, no second policy): every route below calls the exact store /
// auth / manager methods the console handlers call, so the web console and
// the native screens can never drift apart.
//
// Auth model (unchanged expectations):
//   - A session is a console session row (token hash + CSRF), created by the
//     same bcrypt credential check and rate limiter as /console/login.
//   - The web console keeps using the HttpOnly SameSite=Lax cookie; the
//     native client presents the SAME token as `Authorization: Bearer` and
//     keeps it in a per-device Keychain item (ThisDeviceOnly, never
//     synchronizable). A native session can be revoked on the gateway like
//     any console session, and the password itself is never stored.
//   - Every state-changing request requires the session CSRF token in the
//     `X-CallRelay-CSRF` header (or a `csrf` query/form value for simple
//     clients). GET/HEAD never require it and never mutate.
//
// The API is mounted at /api/v2/admin/ — an additive route on the existing
// HTTPS listener and existing reverse proxy; no new port or service.
package console

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"callrelay/gateway/internal/unified/store"

	"golang.org/x/crypto/bcrypt"
)

// adminCSRFHeader carries the per-session CSRF token for native clients.
const adminCSRFHeader = "X-CallRelay-CSRF"

// adminErrorBody is the single JSON error shape of this API. The human
// message is the same concise Chinese the console shows; `code` is stable
// for client-side branching.
func adminErrorBody(code, message string) map[string]string {
	return map[string]string{"error": code, "message": message}
}

func adminError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, adminErrorBody(code, message))
}

// AdminAPIHandler builds the JSON administrator API. Mount it on the outer
// mux at "/api/v2/admin/" (more specific than the device API's "/api/v2/"
// prefix, so it wins without disturbing any existing route).
func (s *Server) AdminAPIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/admin/login", s.adminLogin)
	mux.HandleFunc("GET /api/v2/admin/session", s.adminWithSession(s.adminSessionInfo))
	mux.HandleFunc("POST /api/v2/admin/logout", s.adminWithSession(s.adminLogout))

	mux.HandleFunc("GET /api/v2/admin/overview", s.adminWithSession(s.adminOverview))

	mux.HandleFunc("GET /api/v2/admin/lines", s.adminWithSession(s.adminLines))
	mux.HandleFunc("POST /api/v2/admin/lines/{id}", s.adminWithSession(s.adminLineUpdate))
	mux.HandleFunc("POST /api/v2/admin/lines/{id}/number", s.adminWithSession(s.adminLineNumber))
	mux.HandleFunc("POST /api/v2/admin/lines/{id}/refresh-identity", s.adminWithSession(s.adminLineRefreshIdentity))

	mux.HandleFunc("GET /api/v2/admin/keys", s.adminWithSession(s.adminKeys))
	mux.HandleFunc("POST /api/v2/admin/keys", s.adminWithSession(s.adminKeyCreate))
	mux.HandleFunc("POST /api/v2/admin/keys/{id}/reveal", s.adminWithSession(s.adminKeyReveal))
	mux.HandleFunc("POST /api/v2/admin/keys/{id}/revoke", s.adminWithSession(s.adminKeyRevoke))
	mux.HandleFunc("POST /api/v2/admin/keys/{id}/delete", s.adminWithSession(s.adminKeyDelete))
	mux.HandleFunc("POST /api/v2/admin/keys/{id}/grant", s.adminWithSession(s.adminKeyGrant))
	mux.HandleFunc("POST /api/v2/admin/keys/{id}/grant/{line}/delete", s.adminWithSession(s.adminKeyGrantDelete))
	mux.HandleFunc("POST /api/v2/admin/keys/{id}/manage-number", s.adminWithSession(s.adminKeyManageNumber))

	mux.HandleFunc("GET /api/v2/admin/devices", s.adminWithSession(s.adminDevices))
	mux.HandleFunc("POST /api/v2/admin/devices/{id}/revoke", s.adminWithSession(s.adminDeviceRevoke))
	mux.HandleFunc("POST /api/v2/admin/devices/{id}/allow-reenroll", s.adminWithSession(s.adminDeviceAllowReenroll))

	mux.HandleFunc("GET /api/v2/admin/calls", s.adminWithSession(s.adminCalls))
	mux.HandleFunc("POST /api/v2/admin/calls/{id}/hangup", s.adminWithSession(s.adminCallHangup))
	mux.HandleFunc("POST /api/v2/admin/calls/{id}/reject", s.adminWithSession(s.adminCallReject))

	mux.HandleFunc("GET /api/v2/admin/conferences", s.adminWithSession(s.adminConferences))
	mux.HandleFunc("POST /api/v2/admin/conferences/{id}/close", s.adminWithSession(s.adminConferenceClose))
	mux.HandleFunc("POST /api/v2/admin/conferences/{id}/legs/{callId}/hangup", s.adminWithSession(s.adminConferenceLegHangup))

	mux.HandleFunc("GET /api/v2/admin/voicemails", s.adminWithSession(s.adminVoicemails))
	mux.HandleFunc("DELETE /api/v2/admin/voicemails/{id}", s.adminWithSession(s.adminVoicemailDelete))
	mux.HandleFunc("GET /api/v2/admin/voicemails/{id}/audio", s.adminWithSession(s.adminVoicemailAudio))

	mux.HandleFunc("GET /api/v2/admin/recording", s.adminWithSession(s.adminRecording))
	mux.HandleFunc("POST /api/v2/admin/recording", s.adminWithSession(s.adminRecordingUpdate))

	mux.HandleFunc("GET /api/v2/admin/settings", s.adminWithSession(s.adminSettings))
	mux.HandleFunc("POST /api/v2/admin/settings", s.adminWithSession(s.adminSettingsUpdate))

	mux.HandleFunc("GET /api/v2/admin/diagnostics", s.adminWithSession(s.adminDiagnostics))
	mux.HandleFunc("POST /api/v2/admin/diagnostics/recover/{line}", s.adminWithSession(s.adminDiagnosticsRecover))
	return mux
}

// ---------- sessions ----------

// adminSession resolves the session token from the native bearer header or
// the browser cookie. Both are the same console session rows, so revocation
// and expiry behave identically everywhere.
func (s *Server) adminSessionToken(r *http.Request) string {
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		if token, ok := strings.CutPrefix(header, "Bearer "); ok {
			return strings.TrimSpace(token)
		}
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		return cookie.Value
	}
	return ""
}

func (s *Server) adminWithSession(next func(http.ResponseWriter, *http.Request, store.ConsoleSession)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := s.adminSessionToken(r)
		if token == "" {
			adminError(w, http.StatusUnauthorized, "unauthorized", "请先登录管理账号")
			return
		}
		session, err := s.Store.LookupConsoleSession(r.Context(), hashValue(token))
		if err != nil {
			adminError(w, http.StatusUnauthorized, "unauthorized", "管理会话已过期，请重新登录")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			// The CSRF token travels ONLY in a header: a query-string token
			// would be written into reverse-proxy/access logs. The HTML
			// console keeps its own form field validation in
			// Server.withSession, untouched.
			presented := strings.TrimSpace(r.Header.Get(adminCSRFHeader))
			if subtle.ConstantTimeCompare([]byte(presented), []byte(session.CSRF)) != 1 {
				adminError(w, http.StatusForbidden, "csrf", "安全校验失败，请重新登录")
				return
			}
		}
		next(w, r, session)
	}
}

func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeAdminBody(w, r, &request) {
		return
	}
	remote := clientIP(r)
	if !s.allowLogin(remote) {
		adminError(w, http.StatusTooManyRequests, "rate_limited", "尝试过于频繁，请稍后再试")
		return
	}
	expectedUser, hash, err := s.loadCredential()
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "unconfigured", "控制台凭据未配置")
		return
	}
	username := strings.TrimSpace(request.Username)
	if username != expectedUser || bcrypt.CompareHashAndPassword([]byte(hash), []byte(request.Password)) != nil {
		_ = s.Store.AppendAudit(r.Context(), username, "admin.login", remote, "rejected")
		adminError(w, http.StatusUnauthorized, "bad_credentials", "用户名或密码错误")
		return
	}
	token, err := randomValue()
	if err != nil {
		adminError(w, http.StatusInternalServerError, "internal", "会话创建失败")
		return
	}
	csrf, err := randomValue()
	if err != nil {
		adminError(w, http.StatusInternalServerError, "internal", "会话创建失败")
		return
	}
	duration, err := s.Cfg.ConsoleSessionDuration()
	if err != nil {
		duration = 12 * time.Hour
	}
	now := time.Now().UTC()
	expiresAt := now.Add(duration)
	if err := s.Store.CreateConsoleSession(r.Context(), store.ConsoleSession{
		TokenHash: hashValue(token), CSRF: csrf, Username: username,
		CreatedAt: now, ExpiresAt: expiresAt, RemoteAddr: remote,
	}); err != nil {
		adminError(w, http.StatusInternalServerError, "internal", "会话创建失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), username, "admin.login", remote, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{
		"username": username, "sessionToken": token, "csrf": csrf,
		"expiresAt": expiresAt.UnixMilli(), "gatewayId": s.Identity.ID,
		"apiVersion": 2,
	})
}

func (s *Server) adminSessionInfo(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	writeJSON(w, http.StatusOK, map[string]any{
		"username": session.Username, "csrf": session.CSRF,
		"expiresAt": session.ExpiresAt.UnixMilli(), "gatewayId": s.Identity.ID,
	})
}

func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	if err := s.Store.DeleteConsoleSession(r.Context(), session.TokenHash); err != nil {
		slog.Error("admin logout failed", "error", err)
		adminError(w, http.StatusInternalServerError, "internal", "退出失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "admin.logout", clientIP(r), "accepted")
	w.WriteHeader(http.StatusNoContent)
}

// decodeAdminBody enforces a bounded JSON body (defensive: admin bodies are
// tiny) and answers with the shared error shape.
func decodeAdminBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		adminError(w, http.StatusBadRequest, "bad_request", "请求体不是有效 JSON")
		return false
	}
	return true
}

// ---------- overview ----------

func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	ctx := r.Context()
	lines := s.lineFormViews(ctx)
	online := 0
	for _, line := range lines {
		if line.Online {
			online++
		}
	}
	calls := []any{}
	if rows, err := s.Store.ListCalls(ctx, "", "", true, 100); err == nil {
		for _, call := range rows {
			calls = append(calls, s.Manager.AdminCallView(call))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"gatewayId": s.Identity.ID, "gatewayName": s.Identity.Name,
		"fingerprint": s.Identity.Fingerprint(), "apiVersion": 2,
		"onlineLines": online, "lineCount": len(lines),
		"activeCallCount": len(calls),
		"pushReady":       s.Manager.PushConfigured(),
		"trickleIce":      true,
	})
}

// ---------- lines ----------

// adminLineView is the flat JSON line row the native editor binds to.
type adminLineView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Enabled         bool   `json:"enabled"`
	Online          bool   `json:"online"`
	Registration    string `json:"registration"`
	Operator        string `json:"operator"`
	SMSLive         bool   `json:"smsLive"`
	PhoneNumber     string `json:"phoneNumber"`
	NumberSource    string `json:"numberSource"`
	NumberKind      string `json:"numberKind"`
	NumberLabel     string `json:"numberLabel"`
	RejectPolicy    string `json:"rejectPolicy"`
	BlockedPrefixes string `json:"blockedPrefixes"`
	RetentionDays   int    `json:"retentionDays"`
	AllowInbound    bool   `json:"allowInbound"`
	AllowOutbound   bool   `json:"allowOutbound"`
	Bars            int    `json:"bars"`
	LastError       string `json:"lastError,omitempty"`
}

func (s *Server) adminLines(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	views := s.lineFormViews(r.Context())
	out := make([]adminLineView, 0, len(views))
	for _, view := range views {
		kind := view.NumberKind()
		out = append(out, adminLineView{
			ID: view.ID, Name: view.Name, Enabled: view.Enabled, Online: view.Online,
			Registration: view.Registration, Operator: view.Operator, SMSLive: view.SMSLive,
			PhoneNumber: view.PhoneNumber, NumberSource: view.Identity.NumberSource,
			NumberKind: kind, NumberLabel: view.NumberLabel(),
			RejectPolicy: view.RejectPolicy, BlockedPrefixes: view.BlockedPrefixes,
			RetentionDays: view.RecordingRetentionDays,
			AllowInbound:  view.AllowInbound, AllowOutbound: view.AllowOutbound,
			Bars: view.Signal.Bars, LastError: view.LastError,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": out})
}

// adminLineUpdate mirrors console lineUpdate exactly: the same LineUpdate
// fields with the same validation (name non-empty, policy whitelist, bounded
// retention days) and the same real SMS live toggle through the worker path.
func (s *Server) adminLineUpdate(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	lineID := r.PathValue("id")
	line, err := s.Store.GetLine(r.Context(), lineID)
	if err != nil {
		adminError(w, http.StatusNotFound, "not_found", "线路不存在")
		return
	}
	var request struct {
		Name            *string `json:"name"`
		Enabled         *bool   `json:"enabled"`
		AllowInbound    *bool   `json:"allowInbound"`
		AllowOutbound   *bool   `json:"allowOutbound"`
		RejectPolicy    *string `json:"rejectPolicy"`
		BlockedPrefixes *string `json:"blockedPrefixes"`
		RetentionDays   *int    `json:"retentionDays"`
		SMSLive         *bool   `json:"smsLive"`
	}
	if !decodeAdminBody(w, r, &request) {
		return
	}
	update := store.LineUpdate{}
	if request.Name != nil {
		name := strings.TrimSpace(*request.Name)
		if name == "" {
			adminError(w, http.StatusBadRequest, "invalid_name", "线路名称不能为空")
			return
		}
		update.Name = &name
	}
	update.Enabled = request.Enabled
	update.AllowInbound = request.AllowInbound
	update.AllowOutbound = request.AllowOutbound
	if request.RejectPolicy != nil {
		policy := strings.TrimSpace(*request.RejectPolicy)
		switch policy {
		case "none", "reject_all", "reject_blocked":
		default:
			adminError(w, http.StatusBadRequest, "invalid_policy", "拦截策略无效")
			return
		}
		update.RejectPolicy = &policy
	}
	if request.BlockedPrefixes != nil {
		blocked := strings.TrimSpace(*request.BlockedPrefixes)
		update.BlockedPrefixes = &blocked
	}
	if request.RetentionDays != nil {
		days := *request.RetentionDays
		if days < 0 || days > 3650 {
			adminError(w, http.StatusBadRequest, "invalid_retention", "录音留存天数无效")
			return
		}
		update.RecordingRetentionDays = &days
	}
	if err := s.Store.UpdateLine(r.Context(), lineID, update); err != nil {
		slog.Error("admin line update failed", "line", lineID, "error", err)
		adminError(w, http.StatusInternalServerError, "save_failed", "保存失败")
		return
	}
	// SMS live is an explicit, real worker toggle — the same one the console
	// uses. A stored line setting with a failed worker switch is reported,
	// never silently swallowed.
	if request.SMSLive != nil {
		if err := s.Manager.SetLineSMSDryRun(r.Context(), lineID, !*request.SMSLive); err != nil {
			adminError(w, http.StatusBadGateway, "sms_toggle_failed",
				"线路已保存，但短信实发切换失败：请检查模块状态")
			return
		}
	} else if !line.SMSDryRun {
		// The persisted row says live; keep the worker in step (same as the
		// console, which re-applies dry-run when the box is unchecked).
		_ = s.Manager.SetLineSMSDryRun(r.Context(), lineID, false)
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "line.update", lineID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"saved": true})
}

func (s *Server) adminLineNumber(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	lineID := r.PathValue("id")
	if _, err := s.Store.GetLine(r.Context(), lineID); err != nil {
		adminError(w, http.StatusNotFound, "not_found", "线路不存在")
		return
	}
	var request struct {
		ManualNumber *string `json:"manualNumber"`
	}
	if !decodeAdminBody(w, r, &request) {
		return
	}
	number := ""
	if request.ManualNumber != nil {
		number = strings.TrimSpace(*request.ManualNumber)
	}
	if err := s.Manager.SetManualNumber(r.Context(), lineID, number); err != nil {
		adminError(w, http.StatusBadRequest, "invalid_number", "号码保存失败")
		return
	}
	result := "set"
	if number == "" {
		result = "cleared"
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "line.number.manual", lineID, result)
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "manualNumber": number})
}

func (s *Server) adminLineRefreshIdentity(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	lineID := r.PathValue("id")
	identity, err := s.Manager.RefreshLineIdentity(r.Context(), lineID)
	if err != nil {
		adminError(w, http.StatusBadGateway, "refresh_failed", "重新读取 SIM 号码失败：请检查模块状态")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "line.identity.refresh", lineID, "accepted")
	// Answer with the refreshed EFFECTIVE number the operator screen shows
	// (manual override first, then the SIM value) — read back from the same
	// merged view the console renders, never a guessed value.
	number := ""
	for _, view := range s.lineFormViews(r.Context()) {
		if view.ID == lineID {
			number = view.PhoneNumber
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"saved": true, "numberSource": identity.NumberSource, "phoneNumber": number,
	})
}

// ---------- enrollment keys ----------

type adminGrantView struct {
	LineID       string `json:"lineId"`
	LineName     string `json:"lineName"`
	ReceiveSMS   bool   `json:"receiveSms"`
	ReceiveCalls bool   `json:"receiveCalls"`
	SendSMS      bool   `json:"sendSms"`
	Dial         bool   `json:"dial"`
}

type adminKeyView struct {
	ID                string           `json:"id"`
	Name              string           `json:"name"`
	AllLines          bool             `json:"allLines"`
	ManageNumber      bool             `json:"manageNumber"`
	Revoked           bool             `json:"revoked"`
	SecretRecoverable bool             `json:"secretRecoverable"`
	CreatedAt         int64            `json:"createdAt"`
	RevokedAt         *int64           `json:"revokedAt,omitempty"`
	Grants            []adminGrantView `json:"grants"`
}

func (s *Server) adminKeys(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	ctx := r.Context()
	keys, err := s.Store.ListEnrollmentKeys(ctx)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "internal", "读取密钥失败")
		return
	}
	names := s.lineNames(ctx)
	out := make([]adminKeyView, 0, len(keys))
	for _, key := range keys {
		view := adminKeyView{
			ID: key.ID, Name: key.Name, AllLines: key.AllLines, ManageNumber: key.ManageNumber,
			Revoked: key.RevokedAt != nil, CreatedAt: key.CreatedAt.UnixMilli(),
		}
		if key.RevokedAt != nil {
			revokedAt := key.RevokedAt.UnixMilli()
			view.RevokedAt = &revokedAt
		}
		recoverable, _ := s.Store.KeySecretStored(ctx, key.ID)
		view.SecretRecoverable = recoverable
		grants, _ := s.Store.KeyLines(ctx, key.ID)
		view.Grants = make([]adminGrantView, 0, len(grants))
		for _, grant := range grants {
			view.Grants = append(view.Grants, adminGrantView{
				LineID: grant.LineID, LineName: strings.TrimSpace(names[grant.LineID]),
				ReceiveSMS: grant.ReceiveSMS, ReceiveCalls: grant.ReceiveCalls,
				SendSMS: grant.SendSMS, Dial: grant.Dial,
			})
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) adminKeyCreate(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	var request struct {
		Name         string `json:"name"`
		AllLines     bool   `json:"allLines"`
		ManageNumber bool   `json:"manageNumber"`
	}
	if !decodeAdminBody(w, r, &request) {
		return
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = "未命名密钥"
	}
	keyID, _, err := s.Auth.CreateKey(r.Context(), name, request.AllLines)
	if err != nil {
		slog.Error("admin key create failed", "error", err)
		adminError(w, http.StatusInternalServerError, "create_failed", "创建失败")
		return
	}
	if request.ManageNumber {
		if err := s.Auth.SetKeyManageNumber(r.Context(), keyID, true); err != nil {
			adminError(w, http.StatusInternalServerError, "manage_number_failed", "号码管理授权失败")
			return
		}
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.create", keyID, "accepted")
	// The one-time material is answered for THIS authenticated response only
	// (same sealed server-side copy the console decrypts), never cached.
	s.adminKeyMaterial(w, r, keyID, session.Username)
}

// adminKeyReveal is the admin-only reveal endpoint for an existing
// recoverable key; revoked keys are never revealed, and hash-only legacy keys
// answer 409 with the honest unrecoverable status.
func (s *Server) adminKeyReveal(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID := r.PathValue("id")
	key, _, err := s.Store.GetEnrollmentKey(r.Context(), keyID)
	if err != nil {
		adminError(w, http.StatusNotFound, "not_found", "密钥不存在")
		return
	}
	if key.RevokedAt != nil {
		adminError(w, http.StatusConflict, "revoked", "密钥已撤销，不再提供显示或复制")
		return
	}
	s.adminKeyMaterial(w, r, keyID, session.Username)
}

// adminKeyMaterial answers the sealed secret, pairing payload and QR for one
// key. A missing secret storage or a hash-only legacy key is reported as a
// distinct status so the UI can show the replacement workflow.
func (s *Server) adminKeyMaterial(w http.ResponseWriter, r *http.Request, keyID, actor string) {
	secret, err := s.Store.EnrollmentKeySecret(r.Context(), keyID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrSecretNotStored):
			adminError(w, http.StatusConflict, "unrecoverable",
				"该密钥创建时只保存了校验值，无法恢复显示或复制；既有设备与密钥仍可正常使用，不会被自动撤销。")
		case errors.Is(err, store.ErrSecretStorageUnavailable):
			adminError(w, http.StatusServiceUnavailable, "storage_unavailable", "可恢复密钥存储未配置，无法显示密钥")
		default:
			slog.Error("admin key material failed", "key_id", keyID, "error", err)
			adminError(w, http.StatusInternalServerError, "internal", "显示密钥失败")
		}
		return
	}
	view := s.newKeyView(keyID, secret)
	if view == nil {
		adminError(w, http.StatusInternalServerError, "internal", "生成配对内容失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), actor, "key.reveal", keyID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{
		"keyId": view.ID, "enrollmentKey": keyID + "." + secret,
		"pairingPayload": view.Payload, "qrDataUri": view.QRDataURI,
	})
}

func (s *Server) adminKeyRevoke(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID := r.PathValue("id")
	if err := s.Auth.RevokeKey(r.Context(), keyID); err != nil {
		adminError(w, http.StatusInternalServerError, "revoke_failed", "撤销失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.revoke", keyID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

func (s *Server) adminKeyDelete(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID := r.PathValue("id")
	switch err := s.Auth.DeleteRevokedKey(r.Context(), keyID); {
	case err == nil:
		_ = s.Store.AppendAudit(r.Context(), session.Username, "key.delete", keyID, "accepted")
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	case errors.Is(err, store.ErrKeyActive):
		adminError(w, http.StatusConflict, "active", "只能删除已撤销的密钥，请先撤销")
	case errors.Is(err, store.ErrNotFound):
		adminError(w, http.StatusNotFound, "not_found", "密钥不存在或已删除")
	default:
		slog.Error("admin key delete failed", "key_id", keyID, "error", err)
		adminError(w, http.StatusInternalServerError, "delete_failed", "删除失败")
	}
}

func (s *Server) adminKeyGrant(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID := r.PathValue("id")
	var request struct {
		LineID       string `json:"lineId"`
		ReceiveSMS   bool   `json:"receiveSms"`
		ReceiveCalls bool   `json:"receiveCalls"`
		SendSMS      bool   `json:"sendSms"`
		Dial         bool   `json:"dial"`
	}
	if !decodeAdminBody(w, r, &request) {
		return
	}
	lineID := strings.TrimSpace(request.LineID)
	if lineID == "" {
		adminError(w, http.StatusBadRequest, "missing_line", "请选择线路")
		return
	}
	if _, err := s.Store.GetLine(r.Context(), lineID); err != nil {
		adminError(w, http.StatusNotFound, "not_found", "线路不存在")
		return
	}
	entry := store.KeyLine{
		KeyID: keyID, LineID: lineID,
		ReceiveSMS: request.ReceiveSMS, ReceiveCalls: request.ReceiveCalls,
		SendSMS: request.SendSMS, Dial: request.Dial,
	}
	if err := s.Store.SetKeyLine(r.Context(), entry); err != nil {
		adminError(w, http.StatusInternalServerError, "grant_failed", "授权失败")
		return
	}
	s.Auth.BumpVersion()
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.grant", keyID+"/"+lineID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"saved": true})
}

func (s *Server) adminKeyGrantDelete(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID, lineID := r.PathValue("id"), r.PathValue("line")
	if err := s.Store.DeleteKeyLine(r.Context(), keyID, lineID); err != nil {
		adminError(w, http.StatusInternalServerError, "grant_delete_failed", "删除授权失败")
		return
	}
	s.Auth.BumpVersion()
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.ungrant", keyID+"/"+lineID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"saved": true})
}

func (s *Server) adminKeyManageNumber(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	keyID := r.PathValue("id")
	var request struct {
		ManageNumber bool `json:"manageNumber"`
	}
	if !decodeAdminBody(w, r, &request) {
		return
	}
	if err := s.Auth.SetKeyManageNumber(r.Context(), keyID, request.ManageNumber); err != nil {
		adminError(w, http.StatusInternalServerError, "update_failed", "更新号码管理授权失败")
		return
	}
	result := "granted"
	if !request.ManageNumber {
		result = "revoked"
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "key.manage_number", keyID, result)
	writeJSON(w, http.StatusOK, map[string]any{"saved": true, "manageNumber": request.ManageNumber})
}

// ---------- devices ----------

type adminDeviceView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	KeyID          string `json:"keyId"`
	KeyName        string `json:"keyName"`
	DefaultLineID  string `json:"defaultLineId"`
	LineCount      int    `json:"lineCount"`
	PushRegistered bool   `json:"pushRegistered"`
	Revoked        bool   `json:"revoked"`
	CreatedAt      int64  `json:"createdAt"`
	RevokedAt      *int64 `json:"revokedAt,omitempty"`
}

func (s *Server) adminDevices(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	ctx := r.Context()
	devices, err := s.Store.ListDevices(ctx)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "internal", "读取设备失败")
		return
	}
	keys, _ := s.Store.ListEnrollmentKeys(ctx)
	keyNames := map[string]string{}
	for _, key := range keys {
		keyNames[key.ID] = key.Name
	}
	out := make([]adminDeviceView, 0, len(devices))
	for _, device := range devices {
		view := adminDeviceView{
			ID: device.ID, Name: device.Name, KeyID: device.KeyID, KeyName: keyNames[device.KeyID],
			DefaultLineID:  device.DefaultLineID,
			PushRegistered: device.APNSToken != "" || device.VoIPToken != "",
			Revoked:        device.RevokedAt != nil, CreatedAt: device.CreatedAt.UnixMilli(),
		}
		if device.RevokedAt != nil {
			revokedAt := device.RevokedAt.UnixMilli()
			view.RevokedAt = &revokedAt
		}
		if access, err := s.Store.DeviceAccess(ctx, device.ID); err == nil {
			view.LineCount = len(access.Lines)
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

func (s *Server) adminDeviceRevoke(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	deviceID := r.PathValue("id")
	if err := s.Auth.RevokeDevice(r.Context(), deviceID); err != nil {
		adminError(w, http.StatusInternalServerError, "revoke_failed", "撤销失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "device.revoke", deviceID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}

func (s *Server) adminDeviceAllowReenroll(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	deviceID := r.PathValue("id")
	device, err := s.Store.GetDevice(r.Context(), deviceID)
	if err != nil {
		adminError(w, http.StatusNotFound, "not_found", "设备不存在")
		return
	}
	if err := s.Store.ClearRevokedPublicKey(r.Context(), device.PublicKey); err != nil {
		adminError(w, http.StatusInternalServerError, "clear_failed", "解除绑定失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "device.allow-reenroll", deviceID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"allowed": true})
}

// ---------- calls & conferences ----------

func (s *Server) adminCalls(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	calls, err := s.Store.ListCalls(r.Context(), "", "", false, 100)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "internal", "读取通话失败")
		return
	}
	out := make([]any, 0, len(calls))
	for _, call := range calls {
		out = append(out, s.Manager.AdminCallView(call))
	}
	writeJSON(w, http.StatusOK, map[string]any{"calls": out})
}

func (s *Server) adminCallHangup(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	callID := r.PathValue("id")
	if err := s.Manager.HangupAsSystem(r.Context(), callID, "console_hangup"); err != nil {
		adminError(w, http.StatusConflict, "hangup_failed", "挂断失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "call.hangup", callID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"hungUp": true})
}

func (s *Server) adminCallReject(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	callID := r.PathValue("id")
	if err := s.Manager.RejectAsSystem(r.Context(), callID, "console_reject"); err != nil {
		adminError(w, http.StatusConflict, "reject_failed", "拒绝失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "call.reject", callID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"rejected": true})
}

func (s *Server) adminConferences(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	views := s.conferenceViews(r.Context(), false)
	out := make([]map[string]any, 0, len(views))
	for _, view := range views {
		out = append(out, map[string]any{
			"id": view.ID, "hostDeviceId": view.HostDeviceID, "hostName": view.HostName,
			"state": view.State, "createdAt": view.CreatedAt, "legs": view.Legs,
			"recordingState": view.RecordingState,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"conferences": out})
}

func (s *Server) adminConferenceClose(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	conferenceID := r.PathValue("id")
	if err := s.Manager.CloseConferenceAsSystem(r.Context(), conferenceID, "console_close"); err != nil {
		adminError(w, http.StatusConflict, "close_failed", "结束失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "conference.close", conferenceID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"closed": true})
}

func (s *Server) adminConferenceLegHangup(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	conferenceID, callID := r.PathValue("id"), r.PathValue("callId")
	if err := s.Manager.RemoveLegAsSystem(r.Context(), conferenceID, callID); err != nil {
		adminError(w, http.StatusConflict, "remove_failed", "移除失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "conference.remove-leg", callID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}

// ---------- voicemail ----------

func (s *Server) adminVoicemails(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	rows, err := s.Store.ListVoicemails(r.Context(), "", 200)
	if err != nil {
		adminError(w, http.StatusInternalServerError, "internal", "读取留言失败")
		return
	}
	out := make([]any, 0, len(rows))
	var bytes int64
	for _, row := range rows {
		out = append(out, s.Manager.AdminVoicemailView(row))
		bytes += row.SizeBytes
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"voicemails": out, "totalBytes": bytes,
		"storage": s.Manager.VoicemailStorageStatus(),
	})
}

func (s *Server) adminVoicemailDelete(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	voicemail, err := s.Store.GetVoicemail(r.Context(), r.PathValue("id"))
	if err != nil {
		adminError(w, http.StatusNotFound, "not_found", "留言不存在")
		return
	}
	if voicemail.Path != "" {
		_ = os.Remove(voicemail.Path)
	}
	if err := s.Store.DeleteVoicemail(r.Context(), voicemail.ID); err != nil {
		adminError(w, http.StatusInternalServerError, "delete_failed", "删除失败")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "voicemail.delete", voicemail.ID, "accepted")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminVoicemailAudio(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	voicemail, err := s.Store.GetVoicemail(r.Context(), r.PathValue("id"))
	if err != nil || voicemail.State != "ready" {
		adminError(w, http.StatusNotFound, "not_found", "留言不可播放")
		return
	}
	file, err := os.Open(voicemail.Path)
	if err != nil {
		adminError(w, http.StatusNotFound, "not_found", "留言文件不存在")
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		adminError(w, http.StatusNotFound, "not_found", "留言文件不存在")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, voicemail.ID+".wav", stat.ModTime(), file)
}

// ---------- recording policy ----------

// adminRecordingView is the GATEWAY-SIDE recording policy (global manual
// switch + auto mode + storage state). It is deliberately separate from a
// device's local auto-record preference: flipping the device toggle can never
// imply a gateway policy change and vice versa.
type adminRecordingView struct {
	Enabled          bool   `json:"enabled"`
	AutoMode         string `json:"autoMode"`
	RetentionDays    int    `json:"retentionDays"`
	MinimumFreeBytes int64  `json:"minimumFreeBytes"`
	MaxBytes         int64  `json:"maxBytes"`
	StorageAvailable bool   `json:"storageAvailable"`
	StorageError     string `json:"storageError,omitempty"`
	FreeBytes        int64  `json:"freeBytes,omitempty"`
	UsedBytes        int64  `json:"usedBytes,omitempty"`
	Configured       bool   `json:"configured"`
}

func (s *Server) adminRecording(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	writeJSON(w, http.StatusOK, s.adminRecordingView(r.Context()))
}

// adminRecordingView merges the real gateway recording engine state with the
// persisted operator policy. An unwired recording engine reports
// configured=false without inventing a switch state.
func (s *Server) adminRecordingView(ctx context.Context) adminRecordingView {
	view := adminRecordingView{
		AutoMode:         s.Store.RecordingAutoMode(ctx),
		RetentionDays:    s.Cfg.Recording.RetentionDays,
		MinimumFreeBytes: s.Cfg.Recording.MinimumFreeBytes,
		MaxBytes:         s.Cfg.Recording.MaxBytes,
	}
	if s.Manager.Recordings == nil {
		return view
	}
	view.Configured = true
	enabled, err := s.Manager.Recordings.Enabled(ctx)
	if err != nil {
		view.StorageError = err.Error()
		return view
	}
	view.Enabled = enabled
	status := s.Manager.Recordings.StorageStatus(ctx)
	view.StorageAvailable = status.Mounted && status.Error == ""
	view.StorageError = status.Error
	view.FreeBytes = status.FreeBytes
	view.UsedBytes = status.UsedBytes
	return view
}

func (s *Server) adminRecordingUpdate(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	var request struct {
		Enabled  *bool   `json:"enabled"`
		AutoMode *string `json:"autoMode"`
	}
	if !decodeAdminBody(w, r, &request) {
		return
	}
	if request.Enabled != nil {
		// The global manual-recording switch is real only when the recording
		// engine is wired; without it the request is refused instead of
		// pretending the switch took effect.
		if s.Manager.Recordings == nil {
			adminError(w, http.StatusServiceUnavailable, "unavailable", "录音功能不可用")
			return
		}
		if err := s.Manager.SetRecordingEnabled(r.Context(), *request.Enabled); err != nil {
			adminError(w, http.StatusInternalServerError, "save_failed", "保存录音开关失败")
			return
		}
	}
	if request.AutoMode != nil {
		mode := strings.TrimSpace(*request.AutoMode)
		switch mode {
		case "off", "all", "incoming", "outgoing":
		default:
			adminError(w, http.StatusBadRequest, "invalid_mode", "自动录音模式无效")
			return
		}
		if err := s.Store.SetRecordingAutoMode(r.Context(), mode); err != nil {
			adminError(w, http.StatusInternalServerError, "save_failed", "保存自动录音模式失败")
			return
		}
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "recording.policy", "unified", "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"saved": true})
}

// ---------- settings ----------

func (s *Server) adminSettings(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	values := s.settingsMap(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"settings": values})
}

func (s *Server) adminSettingsUpdate(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	var request map[string]any
	if !decodeAdminBody(w, r, &request) {
		return
	}
	settingInt := func(key string, min, max int64) bool {
		raw, ok := request[key]
		if !ok {
			return true
		}
		number, ok := adminNumber(raw)
		if !ok || number < min || number > max {
			adminError(w, http.StatusBadRequest, "invalid_"+key, "设置值超出允许范围")
			return false
		}
		if err := s.Store.SetSetting(r.Context(), key, strconv.FormatInt(number, 10)); err != nil {
			adminError(w, http.StatusInternalServerError, "save_failed", "保存设置失败")
			return false
		}
		return true
	}
	if !settingInt("max_concurrent_calls", 1, 32) {
		return
	}
	if !settingInt("disconnect_grace_seconds", 5, 600) {
		return
	}
	if raw, ok := request["voicemailEnabled"]; ok {
		enabled, ok := raw.(bool)
		if !ok {
			adminError(w, http.StatusBadRequest, "invalid_voicemail", "留言开关无效")
			return
		}
		if err := s.Store.SetSetting(r.Context(), "voicemail_enabled", strconv.FormatInt(boolToInt(enabled), 10)); err != nil {
			adminError(w, http.StatusInternalServerError, "save_failed", "保存设置失败")
			return
		}
	}
	if !settingInt("voicemail_ring_seconds", 5, 300) {
		return
	}
	if !settingInt("voicemail_offline_seconds", 2, 300) {
		return
	}
	if !settingInt("voicemail_max_seconds", 10, 600) {
		return
	}
	if !settingInt("voicemail_retention_days", 1, 3650) {
		return
	}
	if raw, ok := request["voicemailMaxBytesMb"]; ok {
		mb, ok := adminNumber(raw)
		if !ok || mb < 10 || mb > 102400 {
			adminError(w, http.StatusBadRequest, "invalid_voicemail_max_bytes", "留言容量上限无效")
			return
		}
		if err := s.Store.SetSetting(r.Context(), "voicemail_max_bytes", strconv.FormatInt(mb<<20, 10)); err != nil {
			adminError(w, http.StatusInternalServerError, "save_failed", "保存设置失败")
			return
		}
	}
	if raw, ok := request["voicemailMinFreeBytesMb"]; ok {
		mb, ok := adminNumber(raw)
		if !ok || mb < 16 || mb > 102400 {
			adminError(w, http.StatusBadRequest, "invalid_voicemail_min_free", "最低可用空间无效")
			return
		}
		if err := s.Store.SetSetting(r.Context(), "voicemail_min_free_bytes", strconv.FormatInt(mb<<20, 10)); err != nil {
			adminError(w, http.StatusInternalServerError, "save_failed", "保存设置失败")
			return
		}
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "settings.update", "unified", "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"saved": true})
}

// adminNumber accepts JSON numbers (all realistic clients) and numeric
// strings, so a form-style client keeps working.
func adminNumber(raw any) (int64, bool) {
	switch value := raw.(type) {
	case float64:
		return int64(value), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// ---------- diagnostics ----------

func (s *Server) adminDiagnostics(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	ctx := r.Context()
	lines := s.lineFormViews(ctx)
	events := []any{}
	if rows, err := s.Manager.RecentEventsForConsole(ctx, 30); err == nil {
		for _, event := range rows {
			events = append(events, event)
		}
	}
	audit := []any{}
	if rows, err := s.Store.ListAudit(ctx, 30); err == nil {
		for _, entry := range rows {
			audit = append(audit, map[string]any{
				"actor": entry.Actor, "action": entry.Action, "target": entry.Target,
				"result": entry.Result, "at": entry.CreatedAt.UnixMilli(),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lines": lines, "events": events, "audit": audit,
		"pushReady": s.Manager.PushConfigured(), "turnPolicy": s.Manager.TURNPolicy(),
		"voicemailStorage": s.Manager.VoicemailStorageStatus(),
		"settings":         s.settingsMap(ctx),
	})
}

func (s *Server) adminDiagnosticsRecover(w http.ResponseWriter, r *http.Request, session store.ConsoleSession) {
	lineID := r.PathValue("line")
	if err := s.Manager.RecoverLine(r.Context(), lineID); err != nil {
		adminError(w, http.StatusBadGateway, "recover_failed", "恢复失败："+err.Error())
		return
	}
	_ = s.Store.AppendAudit(r.Context(), session.Username, "line.recover", lineID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{"recovered": true})
}
