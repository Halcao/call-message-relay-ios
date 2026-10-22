package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/runtime"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/webpush"
)

// Self-hosted PWA Web Push endpoints. Two credential surfaces share these
// routes:
//
//   - Device routes use the native app's paired credential (authenticated).
//     They mint bind tokens and report status; the app never sees and never
//     needs subscription secrets.
//   - Browser routes use the derived, revocable, scope-limited web session
//     (Authorization: Bearer <session>). They manage subscriptions, the
//     notification mode and the minimal per-call status the PWA call screen
//     polls. A web session is accepted ONLY by these handlers — it is not a
//     device credential and is rejected by every other /api/v2 route.

func (s *Server) webPushEnabled(w http.ResponseWriter) bool {
	if !s.Cfg.WebPush.Enabled || s.WebPush == nil || s.WebPushSender == nil {
		writeError(w, http.StatusConflict, "CB-WP-DISABLED",
			"网关未启用网页推送，请联系网关管理员。")
		return false
	}
	return true
}

// publicBase returns the normalized public origin used in bind and handoff
// URLs. It is operator-configured; the notification navigate URL must never
// be attacker-influenced.
func (s *Server) publicBase() string {
	base := strings.TrimRight(strings.TrimSpace(s.Cfg.Gateway.PublicBaseURL), "/")
	if base == "" {
		base = "https://" + strings.TrimSpace(s.Cfg.Gateway.ID)
	}
	return base
}

// ---------- device (native app) surface ----------

func (s *Server) webPushVAPID(w http.ResponseWriter, r *http.Request, access store.Access) {
	if !s.webPushEnabled(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"publicKey": s.VAPIDPublicKey,
	})
}

// bindRateLimiter throttles /webpush/bind per source IP so a 45-bit bind
// code cannot be brute forced inside its 10-minute life.
type bindRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func (l *bindRateLimiter) allow(remoteAddr string, now time.Time) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.attempts == nil {
		l.attempts = map[string][]time.Time{}
	}
	windowStart := now.Add(-time.Minute)
	kept := l.attempts[host][:0]
	for _, at := range l.attempts[host] {
		if at.After(windowStart) {
			kept = append(kept, at)
		}
	}
	if len(kept) >= 12 {
		l.attempts[host] = kept
		return false
	}
	l.attempts[host] = append(kept, now)
	return true
}

// webPushBindToken mints the short bind code shown by the app. The same code
// serves three purposes: typed into the installed PWA (iOS storage isolation
// makes this the primary path), a #fragment link/QR for desktop browsers,
// and it stays valid until ONE successful redemption.
func (s *Server) webPushBindToken(w http.ResponseWriter, r *http.Request, access store.Access) {
	if !s.webPushEnabled(w) {
		return
	}
	if r.PathValue("id") != access.Device.ID {
		writeFailure(w, runtime.ErrForbidden)
		return
	}
	// Scope is chosen deliberately by the owner at mint time: "push" (the
	// historical default) only manages web-push subscriptions; "client"
	// additionally authorizes the standalone browser client. Unknown values
	// fail closed rather than silently widening.
	scope := webpush.ScopePush
	if r.Body != nil && r.ContentLength != 0 {
		var request struct {
			Scope string `json:"scope"`
		}
		if !decodeBody(w, r, &request) {
			return
		}
		if request.Scope != "" {
			if !webpush.ValidScope(request.Scope) {
				writeError(w, http.StatusBadRequest, "CB-WP-SCOPE-400", "未知的绑定范围")
				return
			}
			scope = request.Scope
		}
	}
	code, err := s.WebPush.MintBindCode(r.Context(), access.Device.ID, scope)
	if err != nil {
		writeFailure(w, err)
		return
	}
	base := s.publicBase()
	// The token travels in the FRAGMENT: fragments never reach the gateway
	// or any proxy access log, so opening/copying the link leaks nothing.
	bindURL := base + "/pwa/#bind=" + url.QueryEscape(code)
	_ = s.Store.AppendAudit(r.Context(), access.Device.ID, "device.webpush.bind_token", access.Device.ID, "minted:"+scope)
	writeJSON(w, http.StatusOK, map[string]string{"code": code, "bindUrl": bindURL, "scope": scope})
}

// webPushDeviceNotifyMode lets the paired app change the notification mode
// for its own device (same store the web session writes).
func (s *Server) webPushDeviceNotifyMode(w http.ResponseWriter, r *http.Request, access store.Access) {
	if !s.webPushEnabled(w) {
		return
	}
	if r.PathValue("id") != access.Device.ID {
		writeFailure(w, runtime.ErrForbidden)
		return
	}
	var request struct {
		Mode string `json:"mode"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if !s.setNotifyMode(w, r, access.Device.ID, strings.TrimSpace(request.Mode)) {
		return
	}
	s.webPushStatus(w, r, access)
}

// setNotifyMode applies the mode change with the safety guard: 仅网页推送
// requires at least one live browser subscription, so a denied or failed
// subscribe can never silently drop the native ring path.
func (s *Server) setNotifyMode(w http.ResponseWriter, r *http.Request, deviceID, mode string) bool {
	if mode == store.NotifyModeWeb {
		count, err := s.Store.CountWebPushSubscriptions(r.Context(), deviceID)
		if err != nil || count == 0 {
			writeError(w, http.StatusConflict, "CB-WP-MODE-NOSUB",
				"仅网页推送需要先绑定至少一个浏览器并允许通知")
			return false
		}
	}
	if err := s.Store.SetDeviceNotifyMode(r.Context(), deviceID, mode); err != nil {
		writeError(w, http.StatusBadRequest, "CB-WP-MODE", "通知方式必须是 native、both 或 web")
		return false
	}
	_ = s.Store.AppendAudit(r.Context(), deviceID, "device.webpush.mode", deviceID, mode)
	return true
}

func (s *Server) webPushStatus(w http.ResponseWriter, r *http.Request, access store.Access) {
	if !s.webPushEnabled(w) {
		return
	}
	if r.PathValue("id") != access.Device.ID {
		writeFailure(w, runtime.ErrForbidden)
		return
	}
	count, err := s.Store.CountWebPushSubscriptions(r.Context(), access.Device.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	mode, err := s.Store.DeviceNotifyMode(r.Context(), access.Device.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptionCount": count, "notifyMode": mode})
}

// ---------- browser (web session) surface ----------

type webSessionHandler func(http.ResponseWriter, *http.Request, string)

// webSession authenticates the derived browser session. It never falls back
// to the device credential: the two credential types are strictly separate.
// The session arrives either as a Bearer token (existing push-management
// clients) or as the HttpOnly cookie issued at bind time.
func (s *Server) webSession(next webSessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Cfg.WebPush.Enabled || s.WebPush == nil {
			writeError(w, http.StatusConflict, "CB-WP-DISABLED",
				"网关未启用网页推送，请联系网关管理员。")
			return
		}
		var deviceID string
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		token, found := strings.CutPrefix(header, "Bearer ")
		switch {
		case found && token != "":
			resolved, err := s.WebPush.Authenticate(r.Context(), token)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "CB-WP-401", "网页会话已失效，请重新绑定")
				return
			}
			deviceID = resolved
		default:
			// Cookie-authenticated management writes are subject to the
			// same-origin gate just like the client APIs (SameSite=Strict
			// keeps the cookie off cross-SITE requests; this covers
			// same-site sibling origins and upgrades too).
			if !cookieCSRFAllowed(r) {
				writeError(w, http.StatusUnauthorized, "CB-WP-401", "网页会话来源校验失败")
				return
			}
			resolved, _, err := s.browserSessionFromCookie(r)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "CB-WP-401", "缺少网页会话")
				return
			}
			deviceID = resolved
		}
		next(w, r, deviceID)
	}
}

// webPushBind exchanges a bind code (typed in the PWA or delivered through a
// #fragment link) for the browser's session. The code is consumed only by a
// successful redemption; the native pairing credential is never exposed to
// the page.
func (s *Server) webPushBind(w http.ResponseWriter, r *http.Request) {
	if !s.webPushEnabled(w) {
		return
	}
	// Session minting/replacement is a CSRF-sensitive mutation: the PWA
	// always sends Origin; anything else fails closed so a cross-site form
	// cannot swap the browser's session (SameSite=Strict alone would not
	// stop a same-site sibling origin).
	if !bindCSRFAllowed(r) {
		writeError(w, http.StatusForbidden, "CB-WP-ORIGIN-403", "请求来源校验失败")
		return
	}
	if !s.bindLimiter.allow(r.RemoteAddr, time.Now()) {
		writeError(w, http.StatusTooManyRequests, "CB-WP-BIND-429", "尝试过于频繁，请稍后再试")
		return
	}
	var request struct {
		Code string `json:"code"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	sessionToken, deviceID, scope, err := s.WebPush.RedeemBindCode(r.Context(), strings.TrimSpace(request.Code))
	if err != nil {
		writeError(w, http.StatusGone, "CB-WP-BIND-410", "绑定码无效或已过期，请在 App 中重新生成")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), deviceID, "device.webpush.bind", deviceID, "bound:"+scope)
	// The browser session ALSO lands in an HttpOnly Secure SameSite=Strict
	// cookie scoped to /api/v2: fetch() and WebSocket (media/events) then
	// authenticate without readable-page JavaScript access to the token, and
	// SameSite=Strict keeps the credential off every cross-site request by
	// construction. The bearer copy stays for the existing push-management
	// surface; both resolve to the same revocable, sliding-TTL session.
	http.SetCookie(w, &http.Cookie{
		Name:     webSessionCookieName,
		Value:    sessionToken,
		Path:     "/api/v2",
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteStrictMode,
	})
	// Legacy push management reads the bearer from the response (historical
	// behavior, kept isolated to the push scope). A standalone CLIENT
	// session is cookie-only: the page JS never sees a usable token, so XSS
	// in the origin cannot exfiltrate a working credential and nothing
	// durable needs localStorage persistence.
	responseToken := sessionToken
	if scope == webpush.ScopeClient {
		responseToken = ""
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"sessionToken":   responseToken,
		"vapidPublicKey": s.VAPIDPublicKey,
		"scope":          scope,
	})
}

// webPushRevokeSession implements "unbind THIS browser": the presented
// session credential (bearer or cookie) authorizes revoking exactly its own
// session row. Other browser sessions of the device and every native device
// credential stay valid; afterwards the cookie/bearer is dead and a cold
// reload lands on the bind screen.
func (s *Server) webPushRevokeSession(w http.ResponseWriter, r *http.Request) {
	if !s.webPushEnabled(w) {
		return
	}
	if !cookieCSRFAllowed(r) {
		writeError(w, http.StatusUnauthorized, "CB-WP-401", "网页会话来源校验失败")
		return
	}
	var token string
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if bearer, found := strings.CutPrefix(header, "Bearer "); found && bearer != "" {
		token = bearer
	} else if cookie, err := r.Cookie(webSessionCookieName); err == nil {
		token = cookie.Value
	}
	if token == "" {
		writeError(w, http.StatusUnauthorized, "CB-WP-401", "缺少网页会话")
		return
	}
	if _, _, err := s.WebPush.AuthenticateScoped(r.Context(), token); err != nil {
		writeError(w, http.StatusUnauthorized, "CB-WP-401", "网页会话已失效")
		return
	}
	if err := s.WebPush.RevokeSession(r.Context(), token); err != nil {
		writeFailure(w, err)
		return
	}
	// Expire the cookie client-side as well.
	http.SetCookie(w, &http.Cookie{
		Name: webSessionCookieName, Value: "", Path: "/api/v2",
		MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	_ = s.Store.AppendAudit(r.Context(), "web-session", "webpush.session.revoked", "self", "revoked")
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// webSessionCookieName is the browser-session cookie issued at bind time.
const webSessionCookieName = "cr_ws_session"

// browserSessionFromCookie resolves the HttpOnly web session cookie to its
// device and scope, sliding the TTL. Absent/invalid cookies fail closed.
func (s *Server) browserSessionFromCookie(r *http.Request) (deviceID, scope string, err error) {
	cookie, err := r.Cookie(webSessionCookieName)
	if err != nil || cookie.Value == "" {
		return "", "", auth.ErrUnauthorized
	}
	return s.WebPush.AuthenticateScoped(r.Context(), cookie.Value)
}

// effectiveScheme honors a trusted proxy's X-Forwarded-Proto before the
// transport's TLS state, so HTTPS deployments behind Caddy/FRP compare
// origins against https:// while plain tests stay http://.
func effectiveScheme(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		if scheme, _, _ := strings.Cut(forwarded, ","); strings.TrimSpace(scheme) != "" {
			return strings.ToLower(strings.TrimSpace(scheme))
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// originMatches compares the request's Origin (then Referer) against the
// FULL effective origin (scheme + host + port). It reports whether an Origin
// header was present at all, so callers can distinguish "cross-site" from
// "no header sent" (navigations, native clients).
func originMatches(r *http.Request) (matched, present bool) {
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		return false, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false, true
	}
	want := effectiveScheme(r) + "://" + r.Host
	return strings.EqualFold(u.Scheme+"://"+u.Host, want), true
}

// cookieCSRFAllowed decides whether a COOKIE-authenticated request may
// proceed. Cookie credentials are only ever honored when the browser proves
// a same-origin context:
//   - Origin/Referer present -> must equal the full effective origin
//     (scheme+host+port), which also rejects same-site sibling origins and
//     scheme-downgrade attempts behind a trusted proxy.
//   - Origin absent -> only safe methods (reload/navigation/prefetch) may
//     read; mutations and WebSocket upgrades (which ALWAYS carry Origin from
//     browsers) fail closed.
//
// Bearer-authenticated native clients never pass through this gate.
func cookieCSRFAllowed(r *http.Request) bool {
	matched, present := originMatches(r)
	if present {
		return matched
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// bindCSRFAllowed guards the public redeem endpoint: a fetch from the PWA
// always carries Origin; a missing or foreign Origin on a POST is rejected
// so a cross-site form cannot mint or replace the browser session.
func bindCSRFAllowed(r *http.Request) bool {
	matched, present := originMatches(r)
	if present {
		return matched
	}
	return false
}

// browserClientAccess authenticates a STANDALONE browser client through its
// cookie-bound web session. Only sessions minted with the explicit "client"
// scope may pass; notification-only ("push") sessions keep their historical
// surface and never reach the call/SMS APIs. The resulting Access is exactly
// the device's own line permissions — no new authority is created.
func (s *Server) browserClientAccess(r *http.Request) (store.Access, error) {
	if !cookieCSRFAllowed(r) {
		return store.Access{}, auth.ErrUnauthorized
	}
	deviceID, scope, err := s.browserSessionFromCookie(r)
	if err != nil {
		return store.Access{}, auth.ErrUnauthorized
	}
	if scope != webpush.ScopeClient {
		return store.Access{}, auth.ErrUnauthorized
	}
	return s.Store.DeviceAccess(r.Context(), deviceID)
}

// browserClientAllowed reports whether the request carries a valid
// client-scoped browser session (cookie path, origin-checked).
func (s *Server) browserClientAllowed(r *http.Request) bool {
	_, err := s.browserClientAccess(r)
	return err == nil
}

// subscriptionView is the redacted wire view of one subscription: the
// endpoint origin (for the user to recognize their browser) and timestamps
// only. auth/p256dh never leave the store.
type subscriptionView struct {
	ID           string `json:"id"`
	EndpointHost string `json:"endpointHost"`
	UserAgent    string `json:"userAgent"`
	CreatedAt    int64  `json:"createdAt"`
	LastSeenAt   int64  `json:"lastSeenAt"`
}

func subscriptionViews(subs []store.WebPushSubscription) []subscriptionView {
	views := make([]subscriptionView, 0, len(subs))
	for _, sub := range subs {
		host := ""
		if parsed, err := url.Parse(sub.Endpoint); err == nil {
			host = parsed.Host
		}
		views = append(views, subscriptionView{
			ID:           sub.ID,
			EndpointHost: host,
			UserAgent:    sub.UserAgent,
			CreatedAt:    sub.CreatedAt.UnixMilli(),
			LastSeenAt:   sub.LastSeenAt.UnixMilli(),
		})
	}
	return views
}

func (s *Server) webPushSessionStatus(w http.ResponseWriter, r *http.Request, deviceID string) {
	subs, err := s.Store.ListWebPushSubscriptions(r.Context(), deviceID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	mode, err := s.Store.DeviceNotifyMode(r.Context(), deviceID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	access, err := s.Store.DeviceAccess(r.Context(), deviceID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deviceId":         deviceID,
		"deviceName":       access.Device.Name,
		"notifyMode":       mode,
		"vapidPublicKey":   s.VAPIDPublicKey,
		"maxSubscriptions": s.Cfg.WebPushMaxSubscriptions(),
		"subscriptions":    subscriptionViews(subs),
	})
}

func (s *Server) webPushSubscribe(w http.ResponseWriter, r *http.Request, deviceID string) {
	var request struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
		UserAgent string `json:"userAgent"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	endpoint, err := s.WebPushPolicy.ValidateEndpoint(request.Endpoint)
	if err != nil {
		writeWebPushValidationError(w, err)
		return
	}
	p256dh, err := webpush.ValidateP256dh(request.Keys.P256dh)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CB-WP-KEY", "浏览器订阅无效，请重新开启通知。")
		return
	}
	authKey, err := webpush.ValidateAuth(request.Keys.Auth)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CB-WP-KEY", "浏览器订阅无效，请重新开启通知。")
		return
	}
	existing, err := s.Store.ListWebPushSubscriptions(r.Context(), deviceID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	known := false
	for _, sub := range existing {
		if sub.Endpoint == endpoint {
			known = true
			break
		}
	}
	if !known && len(existing) >= s.Cfg.WebPushMaxSubscriptions() {
		writeError(w, http.StatusConflict, "CB-WP-LIMIT",
			"该设备的浏览器订阅数量已达上限，请先移除不用的订阅")
		return
	}
	if err := s.Store.UpsertWebPushSubscription(r.Context(), store.WebPushSubscription{
		DeviceID: deviceID, Endpoint: endpoint, Auth: authKey, P256dh: p256dh,
		UserAgent: strings.TrimSpace(request.UserAgent),
	}); err != nil {
		writeFailure(w, err)
		return
	}
	_ = s.Store.AppendAudit(r.Context(), deviceID, "device.webpush.subscribe", deviceID, "saved")
	s.webPushSessionStatus(w, r, deviceID)
}

func (s *Server) webPushUnsubscribe(w http.ResponseWriter, r *http.Request, deviceID string) {
	subID := r.PathValue("id")
	if err := s.Store.DeleteWebPushSubscription(r.Context(), deviceID, subID); err != nil {
		writeFailure(w, err)
		return
	}
	_ = s.Store.AppendAudit(r.Context(), deviceID, "device.webpush.unsubscribe", deviceID, subID)
	// Safe fallback: removing the last subscription while the device is in
	// web-only mode would otherwise leave it with no notification path at
	// all — revert to native (system push) automatically.
	count, err := s.Store.CountWebPushSubscriptions(r.Context(), deviceID)
	if err == nil && count == 0 {
		mode, modeErr := s.Store.DeviceNotifyMode(r.Context(), deviceID)
		if modeErr == nil && mode == store.NotifyModeWeb {
			_ = s.Store.SetDeviceNotifyMode(r.Context(), deviceID, store.NotifyModeNative)
			_ = s.Store.AppendAudit(r.Context(), deviceID, "device.webpush.mode", deviceID, "auto-native")
		}
	}
	s.webPushSessionStatus(w, r, deviceID)
}

func (s *Server) webPushNotifyMode(w http.ResponseWriter, r *http.Request, deviceID string) {
	var request struct {
		Mode string `json:"mode"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if !s.setNotifyMode(w, r, deviceID, strings.TrimSpace(request.Mode)) {
		return
	}
	s.webPushSessionStatus(w, r, deviceID)
}

// webPushTest sends one synthetic push to every subscription of the session's
// device so the owner can verify the browser path end to end. Rate limited
// per device exactly like the Bark test path.
func (s *Server) webPushTest(w http.ResponseWriter, r *http.Request, deviceID string) {
	if !s.barkTestAllowed(deviceID, time.Now()) {
		writeError(w, http.StatusTooManyRequests, "CB-WP-429", "测试通知过于频繁，请稍后再试")
		return
	}
	subs, err := s.Store.ListWebPushSubscriptions(r.Context(), deviceID)
	if err != nil || len(subs) == 0 {
		writeError(w, http.StatusConflict, "CB-WP-EMPTY", "还没有浏览器订阅，请先在页面中允许通知")
		return
	}
	locale := ""
	if access, accessErr := s.Store.DeviceAccess(r.Context(), deviceID); accessErr == nil {
		locale = access.Device.PushLocale
	}
	payload, err := webpush.TestPayload(locale, s.publicBase()+"/pwa/")
	if err != nil {
		writeFailure(w, err)
		return
	}
	delivered := 0
	var lastErr error
	for _, sub := range subs {
		timeout, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		err := s.WebPushSender.Send(timeout, webpush.Subscription{
			ID: sub.ID, Endpoint: sub.Endpoint, Auth: sub.Auth, P256dh: sub.P256dh,
		}, payload)
		cancel()
		if err != nil {
			lastErr = err
			if webpush.SubscriptionGone(err) {
				_ = s.Store.DeleteWebPushSubscription(r.Context(), deviceID, sub.ID)
			}
			continue
		}
		delivered++
	}
	if delivered == 0 {
		slog.Warn("webpush test failed", "device_id", deviceID, "error", lastErr)
		writeError(w, http.StatusBadGateway, "CB-WP-502", "浏览器推送服务未能接受测试通知")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), deviceID, "device.webpush.test", deviceID, "sent")
	writeJSON(w, http.StatusOK, map[string]any{"sent": true, "delivered": delivered})
}

// webPushHandoff consumes one handoff token (carried in the POST body, never
// in the URL path or query — those would land in server/proxy access logs)
// and returns the minimal metadata of that one still-ringing call. Ended,
// stale or foreign calls and every replay/stale/unknown token answer 410 so
// nothing leaks. The token is never written to logs or audit entries.
func (s *Server) webPushHandoff(w http.ResponseWriter, r *http.Request, deviceID string) {
	var request struct {
		Token string `json:"token"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	entry, err := s.WebPush.ConsumeHandoff(strings.TrimSpace(request.Token))
	if err != nil {
		writeError(w, http.StatusGone, "CB-WP-HANDOFF-410", "来电链接已过期")
		return
	}
	if entry.DeviceID != deviceID {
		writeError(w, http.StatusGone, "CB-WP-HANDOFF-410", "来电链接已过期")
		return
	}
	call, err := s.Store.GetCall(r.Context(), entry.CallID)
	if err != nil || !callStillRinging(call) {
		writeError(w, http.StatusGone, "CB-WP-HANDOFF-410", "该来电已结束")
		return
	}
	access, err := s.Store.DeviceAccess(r.Context(), deviceID)
	lineName := ""
	if err == nil {
		for _, lineAccess := range access.Lines {
			if lineAccess.Line.ID == call.LineID {
				lineName = lineAccess.Line.Name
			}
		}
	}
	_ = s.Store.AppendAudit(r.Context(), deviceID, "device.webpush.handoff", deviceID, "consumed")
	writeJSON(w, http.StatusOK, map[string]any{
		"callId":    call.ID,
		"peer":      call.Peer,
		"line":      lineName,
		"gatewayId": s.Cfg.Gateway.ID,
	})
}

// webPushCallStatus is the bounded poll the PWA call screen uses while the
// page is open. The session may only ask about calls of its own device; the
// reply carries ringing state and the caller handle — nothing else.
func (s *Server) webPushCallStatus(w http.ResponseWriter, r *http.Request, deviceID string) {
	call, err := s.Store.GetCall(r.Context(), r.PathValue("callId"))
	if err != nil || call.DeviceID != deviceID {
		writeError(w, http.StatusNotFound, "CB-WP-404", "通话不存在")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"callId":  call.ID,
		"peer":    call.Peer,
		"state":   call.State,
		"ringing": callStillRinging(call),
	})
}

// callStillRinging re-checks the stored call immediately before any
// notification or handoff decision, exactly like the Bark freshness check.
func callStillRinging(call store.Call) bool {
	return call.State == runtime.StateRinging && call.EndedAt == nil
}

func writeWebPushValidationError(w http.ResponseWriter, err error) {
	if errors.Is(err, webpush.ErrEndpointDisallowed) {
		writeError(w, http.StatusBadRequest, "CB-WP-ENDPOINT-DENIED",
			"订阅端点必须是公网 HTTPS 地址")
		return
	}
	writeError(w, http.StatusBadRequest, "CB-WP-ENDPOINT", "订阅端点无效")
}
