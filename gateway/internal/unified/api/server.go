// Package api exposes the unified /api/v2 surface: anonymous identity and
// enrollment, device-scoped lines, merged SMS history, call arbitration,
// conferences and the durable event stream.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"callrelay/gateway/internal/push"
	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/contacts"
	"callrelay/gateway/internal/unified/events"
	"callrelay/gateway/internal/unified/harass"
	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/notify"
	"callrelay/gateway/internal/unified/quic"
	"callrelay/gateway/internal/unified/runtime"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/webpush"

	"github.com/gorilla/websocket"
)

type Server struct {
	Cfg      config.Config
	Auth     *auth.Service
	Store    *store.Store
	Bus      *events.Bus
	Manager  *runtime.Manager
	Identity *identity.Identity
	// QUIC is the build-61 QUIC direct media listener (nil disables the
	// routes — they answer enabled:false without it).
	QUIC *quic.Server
	// Bark is the optional outbound notifier. Nil builds a client lazily
	// from Cfg.Bark (kept injectable so tests can observe sends).
	Bark *push.BarkClient

	// WebPush is the self-hosted PWA notification path: token manager,
	// VAPID sender and the public key handed to browsers. All nil/disabled
	// together: every handler checks webPushEnabled.
	WebPush        *webpush.Manager
	WebPushSender  *webpush.Sender
	VAPIDPublicKey string
	WebPushPolicy  webpush.Policy

	// Contacts is the principal-scoped address book; Notify is the
	// service-alert monitor. Both are optional so tests can build a server
	// without them.
	Contacts *contacts.Service
	Notify   *notify.Monitor

	barkTestMu  sync.Mutex
	barkTestAt  map[string]time.Time
	bindLimiter bindRateLimiter
}

func NewServer(cfg config.Config, authService *auth.Service, st *store.Store, bus *events.Bus, manager *runtime.Manager, gatewayIdentity *identity.Identity) *Server {
	return &Server{Cfg: cfg, Auth: authService, Store: st, Bus: bus, Manager: manager, Identity: gatewayIdentity}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v2/health", s.health)
	mux.HandleFunc("GET /api/v2/identity", s.identity)
	mux.HandleFunc("POST /api/v2/enroll", s.enroll)
	mux.HandleFunc("POST /api/v2/auth/refresh", s.refresh)
	mux.HandleFunc("GET /api/v2/device", s.authenticated(s.device))
	mux.HandleFunc("GET /api/v2/lines", s.authenticated(s.lines))
	mux.HandleFunc("PUT /api/v2/lines/{id}/number", s.authenticated(s.updateLineNumber))
	mux.HandleFunc("POST /api/v2/threads/actions", s.authenticated(s.threadActions))
	mux.HandleFunc("GET /api/v2/threads", s.authenticated(s.threads))
	mux.HandleFunc("GET /api/v2/messages", s.authenticated(s.messages))
	mux.HandleFunc("POST /api/v2/messages", s.authenticated(s.sendMessage))
	mux.HandleFunc("POST /api/v2/messages/{id}/read", s.authenticated(s.readMessage))
	mux.HandleFunc("GET /api/v2/calls", s.authenticated(s.listCalls))
	mux.HandleFunc("POST /api/v2/calls", s.authenticated(s.dial))
	mux.HandleFunc("GET /api/v2/calls/{id}", s.authenticated(s.getCall))
	mux.HandleFunc("POST /api/v2/calls/{id}/{action}", s.authenticated(s.callAction))
	mux.HandleFunc("GET /api/v2/calls/{id}/ice", s.authenticated(s.callICE))
	mux.HandleFunc("POST /api/v2/calls/{id}/webrtc/offer", s.authenticated(s.callOffer))
	// Incremental (trickle) ICE: additive candidate-intake routes on the
	// existing authenticated JSON plane. Clients only use them after the
	// gateway advertised capabilities.iceTrickle AND the attach answered an
	// attachId; the full-SDP path is untouched.
	mux.HandleFunc("POST /api/v2/calls/{id}/webrtc/candidates", s.authenticated(s.callCandidates))
	mux.HandleFunc("GET /api/v2/calls/{id}/webrtc/candidates", s.authenticated(s.callCandidatesPoll))
	mux.HandleFunc("GET /api/v2/calls/{id}/media", s.authenticated(s.callMedia))
	mux.HandleFunc("GET /api/v2/calls/{id}/media/measure", s.authenticated(s.callMediaMeasure))
	mux.HandleFunc("POST /api/v2/calls/{id}/media/probe", s.authenticated(s.callMediaProbe))
	mux.HandleFunc("POST /api/v2/calls/{id}/media/probe/commit", s.authenticated(s.callMediaProbeCommit))
	mux.HandleFunc("POST /api/v2/calls/{id}/media/standby/promote", s.authenticated(s.callMediaStandbyPromote))
	mux.HandleFunc("GET /api/v2/calls/{id}/media/state", s.authenticated(s.callMediaState))
	mux.HandleFunc("POST /api/v2/calls/{id}/media/probe/candidates", s.authenticated(s.callMediaProbeCandidates))
	mux.HandleFunc("GET /api/v2/calls/{id}/media/probe/candidates", s.authenticated(s.callMediaProbeCandidatesPoll))
	mux.HandleFunc("DELETE /api/v2/calls/{id}/media/probe", s.authenticated(s.callMediaProbeDiscard))
	mux.HandleFunc("GET /api/v2/ice", s.authenticated(s.globalICE))
	mux.HandleFunc("POST /api/v2/media/probe", s.authenticated(s.mediaPreflight))
	mux.HandleFunc("POST /api/v2/media/probe/renew", s.authenticated(s.mediaPreflightRenew))
	mux.HandleFunc("POST /api/v2/media/probe/candidates", s.authenticated(s.mediaPreflightCandidates))
	mux.HandleFunc("GET /api/v2/media/probe/candidates", s.authenticated(s.mediaPreflightCandidatesPoll))
	mux.HandleFunc("DELETE /api/v2/media/probe", s.authenticated(s.mediaPreflightDiscard))
	mux.HandleFunc("GET /api/v2/media/relay-probe", s.authenticated(s.mediaRelayProbe))
	// QUIC DATAGRAM direct media (build 61): authenticated capability,
	// pre-call warm probe grant, and the per-call candidate/punch exchange.
	// The listener itself is plain UDP on a private port; nothing here
	// exposes it publicly.
	mux.HandleFunc("GET /api/v2/media/quic", s.authenticated(s.mediaQUICInfo))
	mux.HandleFunc("POST /api/v2/media/quic/warm", s.authenticated(s.mediaQUICWarm))
	mux.HandleFunc("POST /api/v2/calls/{id}/quic/candidate", s.authenticated(s.callQUICCandidate))
	mux.HandleFunc("DELETE /api/v2/threads/{key}", s.authenticated(s.deleteThread))
	// Per-key recoverable junk: restoring pins the conversation normal for
	// this key only (same-key devices share it; other keys are untouched).
	mux.HandleFunc("POST /api/v2/threads/{key}/restore", s.authenticated(s.restoreThread))
	mux.HandleFunc("POST /api/v2/conferences", s.authenticated(s.mergeConference))
	mux.HandleFunc("GET /api/v2/conferences/{id}", s.authenticated(s.getConference))
	mux.HandleFunc("POST /api/v2/conferences/{id}/close", s.authenticated(s.closeConference))
	mux.HandleFunc("POST /api/v2/conferences/{id}/webrtc/offer", s.authenticated(s.conferenceOffer))
	mux.HandleFunc("GET /api/v2/conferences/{id}/media", s.authenticated(s.conferenceMedia))
	mux.HandleFunc("POST /api/v2/conferences/{id}/split", s.authenticated(s.splitConference))
	mux.HandleFunc("POST /api/v2/conferences/{id}/legs/{callId}/{action}", s.authenticated(s.legAction))
	// Manual conference recording: the conference HOST device alone controls
	// it; the segment is key-scoped like a 1:1 recording (a different
	// enrollment key sharing a leg line sees neither rows nor events).
	mux.HandleFunc("POST /api/v2/conferences/{id}/recording/start", s.authenticated(s.conferenceRecordingStart))
	mux.HandleFunc("POST /api/v2/conferences/{id}/recording/stop", s.authenticated(s.conferenceRecordingStop))
	mux.HandleFunc("GET /api/v2/voicemails", s.authenticated(s.listVoicemails))
	mux.HandleFunc("GET /api/v2/voicemails/{id}", s.authenticated(s.getVoicemail))
	mux.HandleFunc("DELETE /api/v2/voicemails/{id}", s.authenticated(s.deleteVoicemail))
	mux.HandleFunc("GET /api/v2/voicemails/{id}/audio", s.authenticated(s.voicemailAudio))
	// Manual call recording. Start/stop act on one live 1:1 call the device
	// owns; the list/get/delete/audio routes are key-scoped (a different
	// enrollment key sharing the line sees nothing).
	mux.HandleFunc("POST /api/v2/calls/{id}/recording/start", s.authenticated(s.recordingStart))
	mux.HandleFunc("POST /api/v2/calls/{id}/recording/stop", s.authenticated(s.recordingStop))
	mux.HandleFunc("GET /api/v2/recordings", s.authenticated(s.listRecordings))
	mux.HandleFunc("GET /api/v2/recordings/settings", s.authenticated(s.recordingSettings))
	mux.HandleFunc("GET /api/v2/recordings/{id}", s.authenticated(s.getRecording))
	mux.HandleFunc("DELETE /api/v2/recordings/{id}", s.authenticated(s.deleteRecording))
	mux.HandleFunc("GET /api/v2/recordings/{id}/audio", s.authenticated(s.recordingAudio))
	mux.HandleFunc("GET /api/v2/recordings/{id}/waveform", s.authenticated(s.recordingWaveform))
	mux.HandleFunc("PUT /api/v2/devices/{id}/push", s.authenticated(s.registerPush))
	mux.HandleFunc("PUT /api/v2/devices/{id}/preferences", s.authenticated(s.updatePreferences))
	mux.HandleFunc("GET /api/v2/devices/{id}/bark", s.authenticated(s.getBarkSettings))
	mux.HandleFunc("PUT /api/v2/devices/{id}/bark", s.authenticated(s.updateBarkSettings))
	mux.HandleFunc("POST /api/v2/devices/{id}/bark/test", s.authenticated(s.testBarkNotification))
	// Self-hosted PWA Web Push.
	mux.HandleFunc("GET /api/v2/webpush/vapid", s.authenticated(s.webPushVAPID))
	mux.HandleFunc("POST /api/v2/devices/{id}/webpush/bind-token", s.authenticated(s.webPushBindToken))
	mux.HandleFunc("GET /api/v2/devices/{id}/webpush/status", s.authenticated(s.webPushStatus))
	mux.HandleFunc("PUT /api/v2/devices/{id}/webpush/notify-mode", s.authenticated(s.webPushDeviceNotifyMode))
	mux.HandleFunc("POST /api/v2/webpush/bind", s.webPushBind)
	mux.HandleFunc("GET /api/v2/webpush/session", s.webSession(s.webPushSessionStatus))
	mux.HandleFunc("PUT /api/v2/webpush/subscription", s.webSession(s.webPushSubscribe))
	mux.HandleFunc("DELETE /api/v2/webpush/subscription/{id}", s.webSession(s.webPushUnsubscribe))
	mux.HandleFunc("PUT /api/v2/webpush/notify-mode", s.webSession(s.webPushNotifyMode))
	mux.HandleFunc("POST /api/v2/webpush/test", s.webSession(s.webPushTest))
	mux.HandleFunc("POST /api/v2/webpush/handoff", s.webSession(s.webPushHandoff))
	mux.HandleFunc("POST /api/v2/webpush/session/revoke", s.webPushRevokeSession)
	mux.HandleFunc("GET /api/v2/webpush/calls/{callId}", s.webSession(s.webPushCallStatus))
	// Per-enrollment-key harassment policy (骚扰拦截). Scope always comes
	// from the authenticated key, never from the client; same-key devices
	// share one policy and foreign ids are 404.
	mux.HandleFunc("GET /api/v2/harassment/settings", s.authenticated(s.harassSettings))
	mux.HandleFunc("PUT /api/v2/harassment/settings", s.authenticated(s.updateHarassSettings))
	mux.HandleFunc("GET /api/v2/harassment/entries", s.authenticated(s.harassEntries))
	mux.HandleFunc("POST /api/v2/harassment/entries", s.authenticated(s.addHarassEntry))
	mux.HandleFunc("DELETE /api/v2/harassment/entries/{id}", s.authenticated(s.deleteHarassEntry))
	mux.HandleFunc("GET /api/v2/harassment/lists", s.authenticated(s.harassLists))
	mux.HandleFunc("POST /api/v2/harassment/lists/import", s.authenticated(s.importHarassList))
	mux.HandleFunc("DELETE /api/v2/harassment/lists/{id}", s.authenticated(s.deleteHarassList))
	mux.HandleFunc("GET /api/v2/harassment/history", s.authenticated(s.harassHistory))
	mux.HandleFunc("DELETE /api/v2/harassment/history", s.authenticated(s.clearHarassHistory))
	mux.HandleFunc("POST /api/v2/harassment/test", s.authenticated(s.harassTest))
	// Principal-scoped contacts (native or client-scope web session).
	mux.HandleFunc("GET /api/v2/contacts", s.authenticated(s.listContacts))
	mux.HandleFunc("GET /api/v2/contacts/sync", s.authenticated(s.pullContacts))
	mux.HandleFunc("POST /api/v2/contacts/import", s.authenticated(s.importContacts))
	mux.HandleFunc("POST /api/v2/contacts/sync", s.authenticated(s.syncContacts))
	mux.HandleFunc("GET /api/v2/contacts/export", s.authenticated(s.exportContacts))
	mux.HandleFunc("PUT /api/v2/contacts/{id}", s.authenticated(s.updateContact))
	mux.HandleFunc("DELETE /api/v2/contacts/{id}", s.authenticated(s.deleteContact))
	// Service-alert availability view for the app settings screen.
	mux.HandleFunc("GET /api/v2/notifications/status", s.authenticated(s.notificationsStatus))
	mux.HandleFunc("GET /api/v2/events", s.eventsStream)
	return mux
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// writeJSONRevalidatable answers a GET list with a strong content ETag and
// honors If-None-Match with 304 (no body). It is what lets a paired app keep
// its scoped disk copy of the thread/call history instead of re-downloading
// the whole list on every reopen/reconnect. The tag is a hash of the exact
// response bytes, so it can never vouch for a different body; views are
// device/key-scoped upstream, so different devices naturally get different
// tags. `Cache-Control: no-store` stays: the revalidation is app-level (the
// client stores the body itself), never an intermediate cache's to serve.
func writeJSONRevalidatable(w http.ResponseWriter, r *http.Request, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		// Same silent-degradation contract as writeJSON for unencodable
		// values; still answer with the validator headers we can.
		writeJSON(w, http.StatusOK, value)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"sha256-` + hex.EncodeToString(sum[:])[:32] + `"`
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", etag)
	if noneMatch(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// noneMatch compares a client If-None-Match header with the current strong
// tag: an exact match, a "*" token, or the tag appearing in a comma list all
// mean "not modified" per RFC 9110 conditional semantics.
func noneMatch(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" || header == "*" {
		return header == "*"
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		// Tolerate weak tags a client may store from elsewhere.
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"code": code, "message": message})
}

func writeFailure(w http.ResponseWriter, err error) {
	var failure *runtime.Failure
	if errors.As(err, &failure) {
		writeError(w, failure.Status, failure.Code, failure.Message)
		return
	}
	switch {
	case errors.Is(err, auth.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "CB-V2-401", "登录状态已失效")
	case errors.Is(err, auth.ErrInvalidKey):
		writeError(w, http.StatusForbidden, "CB-ENROLL-403", "配对信息无效或已撤销")
	case errors.Is(err, auth.ErrRevokedInstallation):
		writeError(w, http.StatusForbidden, "CB-ENROLL-REVOKED", "该安装身份已被撤销，需要明确重新授权")
	case errors.Is(err, auth.ErrInvalidProof):
		writeError(w, http.StatusBadRequest, "CB-ENROLL-400", "配对签名校验失败")
	default:
		slog.Warn("api request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "CB-V2-500", "服务内部错误")
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "请求体不是有效 JSON")
		return false
	}
	return true
}

type authHandler func(http.ResponseWriter, *http.Request, store.Access)

func (s *Server) authenticated(next authHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		access, _, err := s.Auth.Authenticate(r.Context(), r.Header.Get("Authorization"))
		if err != nil {
			// Standalone browser clients authenticate through their
			// cookie-bound web session (explicit "client" scope only,
			// origin-checked, exactly the device's own line permissions).
			// Notification-only sessions and mere /pwa access never qualify.
			if s.WebPush != nil {
				if browserAccess, browserErr := s.browserClientAccess(r); browserErr == nil {
					next(w, r, browserAccess)
					return
				}
			}
			writeFailure(w, auth.ErrUnauthorized)
			return
		}
		next(w, r, access)
	}
}

func idempotencyKey(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("Idempotency-Key"))
}

func (s *Server) idempotent(ctx context.Context, w http.ResponseWriter, deviceID, operation, key string, produce func() (int, any, error)) {
	if key == "" || len(key) > 200 {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少或非法 Idempotency-Key")
		return
	}
	claimed, record, err := s.Store.ClaimIdempotency(ctx, deviceID, operation, key)
	if err != nil {
		writeFailure(w, err)
		return
	}
	if !claimed {
		if record.Complete && record.Response != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Idempotency-Replayed", "true")
			w.WriteHeader(record.StatusCode)
			_, _ = w.Write(record.Response)
			return
		}
		writeError(w, http.StatusConflict, "CB-V2-409", "请求正在处理")
		return
	}
	status, value, produceErr := produce()
	if produceErr != nil {
		var failure *runtime.Failure
		status = http.StatusInternalServerError
		code, message := "CB-V2-500", "服务内部错误"
		if errors.As(produceErr, &failure) {
			status, code, message = failure.Status, failure.Code, failure.Message
		}
		body, _ := json.Marshal(map[string]string{"code": code, "message": message})
		_ = s.Store.CompleteIdempotency(ctx, deviceID, operation, key, status, body)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	body, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		writeFailure(w, marshalErr)
		return
	}
	if err := s.Store.CompleteIdempotency(ctx, deviceID, operation, key, status, body); err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// ---------- anonymous ----------

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	// Push readiness is intentionally part of the anonymous health surface:
	// clients distinguish "registered a token" from "the gateway can actually
	// send" (a nil/disabled broker accepts registrations but never delivers),
	// and environment is compared with the app's own signed environment
	// before a lock-screen claim is made.
	push := map[string]any{"configured": s.Manager.PushConfigured()}
	if env := strings.ToLower(strings.TrimSpace(s.Cfg.Push.APNsEnvironment)); env != "" {
		push["environment"] = env
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "version": s.Cfg.Gateway.Version, "apiVersion": "v2",
		"lines": len(s.Manager.Lines()), "time": time.Now().UTC().UnixMilli(),
		"push": push,
	})
}

func (s *Server) identity(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"gatewayId":   s.Identity.ID,
		"gatewayName": s.Identity.Name,
		"apiVersion":  "v2",
		"mode":        "unified",
		"publicKey":   s.Identity.PublicBase64(),
		"fingerprint": s.Identity.Fingerprint(),
	})
}

type enrollRequest struct {
	EnrollmentKey   string `json:"enrollmentKey"`
	DeviceName      string `json:"deviceName"`
	DevicePublicKey string `json:"devicePublicKey"`
	Proof           string `json:"proof"`
}

func (s *Server) enroll(w http.ResponseWriter, r *http.Request) {
	var request enrollRequest
	if !decodeBody(w, r, &request) {
		return
	}
	publicKey, err := decodeBase64(request.DevicePublicKey)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CB-ENROLL-400", "设备公钥不是有效 base64")
		return
	}
	proof, err := decodeBase64(request.Proof)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CB-ENROLL-400", "配对签名不是有效 base64")
		return
	}
	pair, err := s.Auth.Enroll(r.Context(), auth.EnrollmentRequest{
		EnrollmentKey: request.EnrollmentKey, DeviceName: request.DeviceName,
		DevicePublicKey: publicKey, Proof: proof,
	})
	if err != nil {
		writeFailure(w, err)
		return
	}
	_ = s.Store.AppendAudit(r.Context(), pair.DeviceID, "device.enroll", pair.DeviceID, "accepted")
	writeJSON(w, http.StatusOK, map[string]any{
		"deviceId": pair.DeviceID, "accessToken": pair.AccessToken, "refreshToken": pair.RefreshToken,
		"gatewayId": s.Identity.ID, "gatewayName": s.Identity.Name,
	})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	var request struct {
		RefreshToken string `json:"refreshToken"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	pair, err := s.Auth.Refresh(r.Context(), request.RefreshToken)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accessToken": pair.AccessToken, "refreshToken": pair.RefreshToken, "deviceId": pair.DeviceID,
	})
}

// ---------- device / lines ----------

func (s *Server) device(w http.ResponseWriter, r *http.Request, access store.Access) {
	writeJSON(w, http.StatusOK, map[string]any{
		"device": map[string]any{
			"id": access.Device.ID, "name": access.Device.Name, "keyId": access.Key.ID,
			"defaultLineId": access.Device.DefaultLineID,
		},
		// Capability declaration: clients feature-detect these names instead
		// of probing for 404s, and an old client that ignores the map keeps
		// working unchanged.
		"capabilities": map[string]any{
			"harassment": true, "harassmentImport": true, "recordingAuto": true,
			"smsQuarantine": true, "smsKeyOverlay": true, "smsThreadActions": true,
		},
		"key":   map[string]any{"id": access.Key.ID, "name": access.Key.Name, "allLines": access.Key.AllLines},
		"lines": s.Manager.LineViews(r.Context(), access, ""),
	})
}

func (s *Server) lines(w http.ResponseWriter, r *http.Request, access store.Access) {
	writeJSON(w, http.StatusOK, s.Manager.LineViews(r.Context(), access, strings.TrimSpace(r.URL.Query().Get("line"))))
}

// updateLineNumber lets an explicitly authorized paired device replace or
// reset (empty value) the line's shared own number. Authorization is twofold:
// a live grant to the exact line and the key-level manage-number capability,
// so an ordinary or shared enrollment key can never edit global line
// metadata. The audit row carries no number material; other authorized
// phones learn the change through line.updated + re-fetch.
func (s *Server) updateLineNumber(w http.ResponseWriter, r *http.Request, access store.Access) {
	lineID := r.PathValue("id")
	var request struct {
		PhoneNumber string `json:"phoneNumber"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	view, err := s.Manager.SetDeviceManualNumber(r.Context(), access.Device.ID, lineID, request.PhoneNumber)
	if err != nil {
		writeFailure(w, err)
		return
	}
	result := "set"
	if strings.TrimSpace(request.PhoneNumber) == "" {
		result = "cleared"
	}
	_ = s.Store.AppendAudit(r.Context(), access.Device.ID, "line.number.manual", lineID, result)
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) updatePreferences(w http.ResponseWriter, r *http.Request, access store.Access) {
	if r.PathValue("id") != access.Device.ID {
		writeFailure(w, runtime.ErrForbidden)
		return
	}
	// PATCH presence semantics (build 48): every field is optional. A present
	// field is changed; an absent field is left untouched. An audio-only
	// patch therefore never clears defaultLineId, and a legacy
	// defaultLineId-only body never changes the denoise switches.
	var request struct {
		DefaultLineID   *string `json:"defaultLineId"`
		UplinkDenoise   *bool   `json:"uplinkDenoise"`
		DownlinkDenoise *bool   `json:"downlinkDenoise"`
		AutoRecord      *bool   `json:"autoRecord"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if request.DefaultLineID != nil {
		lineID := strings.TrimSpace(*request.DefaultLineID)
		granted := false
		for _, entry := range access.Lines {
			if entry.Line.ID == lineID {
				granted = true
			}
		}
		if lineID != "" && !granted {
			writeFailure(w, runtime.ErrForbidden)
			return
		}
		if err := s.Store.SetDeviceDefaultLine(r.Context(), access.Device.ID, lineID); err != nil {
			writeFailure(w, err)
			return
		}
	}
	// Persist + live-apply to only this device's owned sessions (frame
	// boundary safe); other devices/keys are never affected.
	if err := s.Manager.SetDeviceDenoise(r.Context(), access.Device.ID,
		request.UplinkDenoise, request.DownlinkDenoise); err != nil {
		writeFailure(w, err)
		return
	}
	if err := s.Store.SetDeviceRecordingPrefs(r.Context(), access.Device.ID, request.AutoRecord); err != nil {
		writeFailure(w, err)
		return
	}
	prefs := s.Store.DeviceDenoisePrefs(r.Context(), access.Device.ID)
	up, down := prefs.Uplink, prefs.Downlink
	// Build-49 honest default: OFF until the device owner enables a switch
	// (authorized human-speech quality acceptance was not established).
	if !prefs.UplinkSet {
		up = false
	}
	if !prefs.DownlinkSet {
		down = false
	}
	defaultLine := access.Device.DefaultLineID
	if request.DefaultLineID != nil {
		defaultLine = strings.TrimSpace(*request.DefaultLineID)
	}
	recordingPrefs := s.Store.DeviceRecordingPrefs(r.Context(), access.Device.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"defaultLineId":   defaultLine,
		"uplinkDenoise":   up,
		"downlinkDenoise": down,
		// Auto recording is a DEVICE preference, default OFF; the gateway
		// auto mode must also be on before anything records automatically.
		"autoRecord": recordingPrefs.AutoRecord,
	})
}

func (s *Server) registerPush(w http.ResponseWriter, r *http.Request, access store.Access) {
	if r.PathValue("id") != access.Device.ID {
		writeFailure(w, runtime.ErrForbidden)
		return
	}
	var request struct {
		APNSToken   string `json:"apnsToken"`
		VoIPToken   string `json:"voipToken"`
		Environment string `json:"environment"`
		Locale      string `json:"locale"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	// A stored environment is required for the target to be sendable at all
	// (both senders reject an empty one); accepting '' here would register a
	// device that then fails EVERY push attempt with a logged-but-permanent
	// error. This mirrors the strict v1 contract.
	if request.Environment != "sandbox" && request.Environment != "production" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "environment 必须是 sandbox 或 production")
		return
	}
	// Never let a malformed/duplicate registration blank tokens the device
	// already has: a request that clears both tokens while a live
	// registration exists is rejected instead of silently un-targeting the
	// device (the v2 handler previously overwrote unconditionally).
	existing, existingErr := s.Store.DevicePush(r.Context(), access.Device.ID)
	if existingErr == nil && (existing.APNSToken != "" || existing.VoIPToken != "") &&
		request.APNSToken == "" && request.VoIPToken == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "注册不能清空已有的推送令牌")
		return
	}
	if err := s.Store.SetDevicePush(r.Context(), access.Device.ID, store.PushRegistration{
		APNSToken: request.APNSToken, VoIPToken: request.VoIPToken,
		Environment: request.Environment, Locale: request.Locale,
	}); err != nil {
		writeFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- optional Bark bridge ----------

// barkSettingsView is the redacted wire view. device_key is NEVER returned;
// the app only learns whether a key is stored and a masked hint.
type barkSettingsView struct {
	Enabled              bool   `json:"enabled"`
	ServerURL            string `json:"serverUrl"`
	KeyConfigured        bool   `json:"keyConfigured"`
	KeyHint              string `json:"keyHint,omitempty"`
	AllowPrivate         bool   `json:"allowPrivate"`
	UpdatedAt            int64  `json:"updatedAt,omitempty"`
	GatewayEnabled       bool   `json:"gatewayEnabled"`
	GatewayAllowsPrivate bool   `json:"gatewayAllowsPrivate"`
}

func (s *Server) barkView(settings store.DeviceBark) barkSettingsView {
	return barkSettingsView{
		Enabled: settings.Enabled, ServerURL: settings.ServerURL,
		KeyConfigured: settings.DeviceKey != "", KeyHint: settings.KeyHint,
		AllowPrivate:   settings.AllowPrivate,
		UpdatedAt:      settings.UpdatedAt.UnixMilli(),
		GatewayEnabled: s.Cfg.Bark.Enabled, GatewayAllowsPrivate: s.Cfg.Bark.AllowPrivate,
	}
}

func (s *Server) barkClient() *push.BarkClient {
	if s.Bark != nil {
		return s.Bark
	}
	timeout, err := s.Cfg.BarkTimeout()
	if err != nil {
		timeout = push.BarkDefaultTimeout
	}
	return push.NewBarkClient(s.Cfg.Bark.AllowPrivate, timeout)
}

func (s *Server) getBarkSettings(w http.ResponseWriter, r *http.Request, access store.Access) {
	if r.PathValue("id") != access.Device.ID {
		writeFailure(w, runtime.ErrForbidden)
		return
	}
	settings, err := s.Store.GetDeviceBark(r.Context(), access.Device.ID)
	if errors.Is(err, store.ErrNotFound) {
		// Never configured is a normal empty state, not an error.
		writeJSON(w, http.StatusOK, s.barkView(store.DeviceBark{}))
		return
	}
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.barkView(settings))
}

func writeBarkValidationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, push.ErrBarkURLDisallowed):
		writeError(w, http.StatusBadRequest, "CB-BARK-LAN",
			"该地址指向局域网或本机；需要网关与这台手机都明确允许局域网 Bark 服务器")
	case errors.Is(err, push.ErrBarkKeyInvalid):
		writeError(w, http.StatusBadRequest, "CB-BARK-KEY", "Bark 设备密钥无效")
	default:
		writeError(w, http.StatusBadRequest, "CB-BARK-URL",
			"Bark 服务器地址无效（仅支持 http/https，不能包含账号、查询或片段）")
	}
}

func (s *Server) updateBarkSettings(w http.ResponseWriter, r *http.Request, access store.Access) {
	if r.PathValue("id") != access.Device.ID {
		writeFailure(w, runtime.ErrForbidden)
		return
	}
	if !s.Cfg.Bark.Enabled {
		writeError(w, http.StatusConflict, "CB-BARK-DISABLED",
			"Bark 通知未启用，请联系网关管理员。")
		return
	}
	var request struct {
		Enabled      bool    `json:"enabled"`
		ServerURL    string  `json:"serverUrl"`
		DeviceKey    *string `json:"deviceKey"`
		ClearKey     bool    `json:"clearKey"`
		AllowPrivate bool    `json:"allowPrivate"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if request.AllowPrivate && !s.Cfg.Bark.AllowPrivate {
		writeError(w, http.StatusForbidden, "CB-BARK-LAN-DISABLED",
			"网关不允许局域网 Bark 服务器。")
		return
	}
	existing, err := s.Store.GetDeviceBark(r.Context(), access.Device.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeFailure(w, err)
		return
	}
	settings := existing
	settings.DeviceID = access.Device.ID
	settings.AllowPrivate = request.AllowPrivate

	switch {
	case request.ClearKey:
		settings.DeviceKey = ""
		settings.KeyHint = ""
	case request.DeviceKey != nil && strings.TrimSpace(*request.DeviceKey) != "":
		key, keyErr := push.ValidateBarkDeviceKey(*request.DeviceKey)
		if keyErr != nil {
			writeBarkValidationError(w, keyErr)
			return
		}
		settings.DeviceKey = key
		settings.KeyHint = push.BarkKeyHint(key)
	}

	serverURL := strings.TrimSpace(request.ServerURL)
	if serverURL != "" {
		normalized, urlErr := push.ValidateBarkServerURL(serverURL, settings.AllowPrivate)
		if urlErr != nil {
			writeBarkValidationError(w, urlErr)
			return
		}
		settings.ServerURL = normalized
	} else if request.Enabled {
		writeError(w, http.StatusBadRequest, "CB-BARK-URL", "启用 Bark 通知需要服务器地址")
		return
	}
	settings.Enabled = request.Enabled
	if settings.Enabled && (settings.ServerURL == "" || settings.DeviceKey == "") {
		writeError(w, http.StatusBadRequest, "CB-BARK-EMPTY", "启用 Bark 通知需要服务器地址与设备密钥")
		return
	}
	if err := s.Store.SetDeviceBark(r.Context(), settings); err != nil {
		writeFailure(w, err)
		return
	}
	result := "disabled"
	if settings.Enabled {
		result = "enabled"
	}
	_ = s.Store.AppendAudit(r.Context(), access.Device.ID, "device.bark.update", access.Device.ID, result)
	stored, err := s.Store.GetDeviceBark(r.Context(), access.Device.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.barkView(stored))
}

// testBarkNotification sends one synthetic notification so the owner can
// verify server URL/key before relying on it. It is rate-limited per device
// and never echoes the secret, the server URL or the provider error body.
func (s *Server) testBarkNotification(w http.ResponseWriter, r *http.Request, access store.Access) {
	if r.PathValue("id") != access.Device.ID {
		writeFailure(w, runtime.ErrForbidden)
		return
	}
	if !s.Cfg.Bark.Enabled {
		writeError(w, http.StatusConflict, "CB-BARK-DISABLED",
			"Bark 通知未启用，请联系网关管理员。")
		return
	}
	if !s.barkTestAllowed(access.Device.ID, time.Now()) {
		writeError(w, http.StatusTooManyRequests, "CB-BARK-429", "测试通知过于频繁，请稍后再试")
		return
	}
	settings, err := s.Store.GetDeviceBark(r.Context(), access.Device.ID)
	if err != nil || !settings.Enabled || settings.ServerURL == "" || settings.DeviceKey == "" {
		writeError(w, http.StatusConflict, "CB-BARK-EMPTY", "请先保存并启用 Bark 服务器地址与设备密钥")
		return
	}
	title, body := push.BarkTestText(access.Device.PushLocale)
	timeout, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	err = s.barkClient().Send(timeout, push.BarkTarget{
		DeviceID: settings.DeviceID, ServerURL: settings.ServerURL,
		DeviceKey: settings.DeviceKey, Locale: access.Device.PushLocale,
	}, push.BarkNotification{
		Title: title, Body: body,
		URL: push.BarkDeepLinkURL, Group: "CallRelay", Level: "timeSensitive",
	})
	if err != nil {
		slog.Warn("bark test notification failed", "device_id", access.Device.ID, "error", err)
		writeError(w, http.StatusBadGateway, "CB-BARK-502", "Bark 服务器未能接受测试通知，请检查地址与密钥")
		return
	}
	_ = s.Store.AppendAudit(r.Context(), access.Device.ID, "device.bark.test", access.Device.ID, "sent")
	writeJSON(w, http.StatusOK, map[string]bool{"sent": true})
}

// barkTestAllowed enforces a 30-second per-device cooldown on test sends.
func (s *Server) barkTestAllowed(deviceID string, now time.Time) bool {
	s.barkTestMu.Lock()
	defer s.barkTestMu.Unlock()
	if s.barkTestAt == nil {
		s.barkTestAt = map[string]time.Time{}
	}
	if last, ok := s.barkTestAt[deviceID]; ok && now.Sub(last) < 30*time.Second {
		return false
	}
	s.barkTestAt[deviceID] = now
	return true
}

// ---------- messages ----------

func (s *Server) threads(w http.ResponseWriter, r *http.Request, access store.Access) {
	query := r.URL.Query()
	// folder: normal (default) | junk | all. Junk is the per-key quarantine;
	// the same authorized key can read its contents through threadKey fetch.
	views, err := s.Manager.Threads(r.Context(), access.Device.ID,
		strings.TrimSpace(query.Get("line")), strings.TrimSpace(query.Get("folder")))
	if err != nil {
		writeFailure(w, err)
		return
	}
	// Revalidatable: the paired app caches this list on disk per scope and
	// re-opens it offline; unchanged repeat openings answer 304 instead of a
	// full-history download.
	writeJSONRevalidatable(w, r, views)
}

func (s *Server) messages(w http.ResponseWriter, r *http.Request, access store.Access) {
	query := r.URL.Query()
	threadKey := strings.TrimSpace(query.Get("threadKey"))
	if threadKey == "" {
		// Merged recent view: latest message per thread across lines is
		// served by /threads; this endpoint requires a thread scope.
		writeError(w, http.StatusBadRequest, "CB-V2-400", "threadKey 必填")
		return
	}
	var before int64
	if value := strings.TrimSpace(query.Get("before")); value != "" {
		if _, err := json.Number(value).Int64(); err != nil {
			writeError(w, http.StatusBadRequest, "CB-V2-400", "before 必须是毫秒时间戳")
			return
		}
		before, _ = json.Number(value).Int64()
	}
	limit := 50
	if value := strings.TrimSpace(query.Get("limit")); value != "" {
		if parsed, err := json.Number(value).Int64(); err == nil && parsed > 0 && parsed <= 200 {
			limit = int(parsed)
		}
	}
	views, hasMore, err := s.Manager.ThreadMessages(r.Context(), access.Device.ID, threadKey, before, query.Get("beforeId"), limit)
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("X-CallRelay-Has-More", map[bool]string{true: "true", false: "false"}[hasMore])
	// Revalidatable like the other history lists: a cached conversation can
	// be re-checked after an event gap or a stale reopen without re-downloading
	// the page body. The header above is set BEFORE the conditional write so a
	// 304 keeps carrying it (RFC 9110: a 304 must include the validators and
	// content metadata that would accompany the 200).
	writeJSONRevalidatable(w, r, views)
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		LineID string `json:"lineId"`
		To     string `json:"to"`
		Body   string `json:"body"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	lineID := strings.TrimSpace(request.LineID)
	if lineID == "" {
		if len(access.Lines) == 1 {
			lineID = access.Lines[0].Line.ID
		} else {
			writeError(w, http.StatusBadRequest, "CB-V2-LINE-REQUIRED", "请先选择发送线路")
			return
		}
	}
	key := idempotencyKey(r)
	s.idempotent(r.Context(), w, access.Device.ID, "message.send:"+lineID, key, func() (int, any, error) {
		view, err := s.Manager.SendMessage(r.Context(), access.Device.ID, lineID, request.To, request.Body, key)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusCreated, view, nil
	})
}

func (s *Server) readMessage(w http.ResponseWriter, r *http.Request, access store.Access) {
	key := idempotencyKey(r)
	if key == "" {
		key = "read-" + r.PathValue("id")
	}
	if err := s.Manager.ReadMessage(r.Context(), access.Device.ID, r.PathValue("id"), key); err != nil {
		writeFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- calls ----------

func (s *Server) listCalls(w http.ResponseWriter, r *http.Request, access store.Access) {
	activeOnly := strings.EqualFold(r.URL.Query().Get("active"), "true")
	limit := 100
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		if parsed, err := json.Number(value).Int64(); err == nil && parsed > 0 && parsed <= 500 {
			limit = int(parsed)
		}
	}
	views, err := s.Manager.ListCalls(r.Context(), access.Device.ID, strings.TrimSpace(r.URL.Query().Get("line")), activeOnly, limit)
	if err != nil {
		writeFailure(w, err)
		return
	}
	// Only the ended/whole-history list is revalidatable. `active=true` is the
	// authoritative live-call filter: a 304 there could mask a call that just
	// started or ended, so it always returns the fresh body.
	if activeOnly {
		writeJSON(w, http.StatusOK, views)
		return
	}
	writeJSONRevalidatable(w, r, views)
}

func (s *Server) dial(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		LineID       string `json:"lineId"`
		To           string `json:"to"`
		ClientCallID string `json:"clientCallId"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	lineID := strings.TrimSpace(request.LineID)
	if lineID == "" {
		if len(access.Lines) == 1 {
			lineID = access.Lines[0].Line.ID
		} else {
			writeError(w, http.StatusBadRequest, "CB-V2-LINE-REQUIRED", "请先选择外呼线路")
			return
		}
	}
	key := idempotencyKey(r)
	s.idempotent(r.Context(), w, access.Device.ID, "call.dial", key, func() (int, any, error) {
		view, err := s.Manager.Dial(r.Context(), access.Device.ID, lineID, request.To, request.ClientCallID, key)
		if err != nil {
			return 0, nil, err
		}
		return http.StatusCreated, view, nil
	})
}

func (s *Server) getCall(w http.ResponseWriter, r *http.Request, access store.Access) {
	view, err := s.Manager.GetCall(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) callAction(w http.ResponseWriter, r *http.Request, access store.Access) {
	callID := r.PathValue("id")
	action := r.PathValue("action")
	switch action {
	case "answer", "reject", "decline", "hangup", "hold", "resume", "dtmf":
	default:
		writeError(w, http.StatusNotFound, "CB-V2-404", "不支持的通话操作")
		return
	}
	if action == "dtmf" {
		var request struct {
			Digit string `json:"digit"`
		}
		if !decodeBody(w, r, &request) {
			return
		}
		if len([]rune(strings.TrimSpace(request.Digit))) != 1 {
			writeError(w, http.StatusBadRequest, "CB-V2-400", "digit 必须是单个字符")
			return
		}
		if err := s.Manager.DTMF(r.Context(), access.Device.ID, callID, request.Digit, idempotencyKey(r)); err != nil {
			writeFailure(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	key := idempotencyKey(r)
	if key == "" {
		key = action + "-" + callID
	}
	s.idempotent(r.Context(), w, access.Device.ID, "call."+action, key, func() (int, any, error) {
		switch action {
		case "answer":
			view, err := s.Manager.Answer(r.Context(), access.Device.ID, callID, key)
			if err != nil {
				return 0, nil, err
			}
			return http.StatusCreated, view, nil
		case "reject":
			if err := s.Manager.Reject(r.Context(), access.Device.ID, callID, "rejected_by_device"); err != nil {
				return 0, nil, err
			}
			return http.StatusNoContent, map[string]string{"status": "rejected"}, nil
		case "decline":
			if err := s.Manager.Decline(r.Context(), access.Device.ID, callID); err != nil {
				return 0, nil, err
			}
			return http.StatusNoContent, map[string]string{"status": "declined"}, nil
		case "hangup":
			if err := s.Manager.Hangup(r.Context(), access.Device.ID, callID); err != nil {
				return 0, nil, err
			}
			return http.StatusNoContent, map[string]string{"status": "hungup"}, nil
		case "hold", "resume":
			view, err := s.Manager.SetHeld(r.Context(), access.Device.ID, callID, action == "hold")
			if err != nil {
				return 0, nil, err
			}
			return http.StatusOK, view, nil
		}
		return http.StatusNotFound, nil, nil
	})
}

func (s *Server) callOffer(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		SDP             string `json:"sdp"`
		Type            string `json:"type"`
		UplinkDenoise   *bool  `json:"uplinkDenoise,omitempty"`
		DownlinkDenoise *bool  `json:"downlinkDenoise,omitempty"`
		// Trickle (build 68): an optional incremental-ICE flag. A client that
		// saw capabilities.iceTrickle may send an offer as soon as it has its
		// first candidates and post the rest to
		// POST /calls/{id}/webrtc/candidates with the returned attachId.
		// Absent/false reproduces the full-SDP behavior byte for byte.
		Trickle bool `json:"trickle,omitempty"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if request.Type != "offer" || strings.TrimSpace(request.SDP) == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "需要 type=offer 与 sdp")
		return
	}
	answer, err := s.Manager.Offer(r.Context(), access.Device.ID, r.PathValue("id"), request.SDP, runtime.DenoiseChoice{
		Uplink: request.UplinkDenoise, Downlink: request.DownlinkDenoise,
	}, request.Trickle)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

// parseDenoiseQuery extracts the OPTIONAL per-attach denoise parameters from
// a media WS URL (?uplinkDenoise=0|1&downlinkDenoise=0|1). Absent or invalid
// values leave the direction unset (stored preference/default applies), so
// old clients attaching without the params behave exactly as before.
func parseDenoiseQuery(q url.Values) runtime.DenoiseChoice {
	parse := func(name string) *bool {
		raw, ok := q[name]
		if !ok || len(raw) == 0 {
			return nil
		}
		switch raw[0] {
		case "1":
			v := true
			return &v
		case "0":
			v := false
			return &v
		default:
			return nil
		}
	}
	return runtime.DenoiseChoice{Uplink: parse("uplinkDenoise"), Downlink: parse("downlinkDenoise")}
}

func (s *Server) callICE(w http.ResponseWriter, r *http.Request, access store.Access) {
	servers, err := s.Manager.Ice(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, servers)
}

// callMedia upgrades the authorized /calls/{id}/media GET to a WebSocket and
// hands the connection to the runtime as the call's client audio transport
// (20 ms PCMU binary frames, {"type":"ready"} first). Authorization happens
// before the upgrade so unauthenticated/Forbidden attempts never become a
// hanging socket; per-message limits and the runtime's bounded queue keep a
// slow tunnel from accumulating stale audio.
// conferenceMedia upgrades the host device's authorized conference media GET
// to the WebSocket client audio transport (see callMedia).
func (s *Server) conferenceMedia(w http.ResponseWriter, r *http.Request, access store.Access) {
	conferenceID := r.PathValue("id")
	// Build-50 hot standby is NOT offered for conference media in this
	// delivery: reject the parameter loudly so an over-eager client gets a
	// clean 409 instead of silently evicting the conference host. The
	// capability advertisement is per-call; clients must not attempt
	// standby on conference routes at all.
	if r.URL.Query().Get("standby") != "" {
		writeError(w, http.StatusConflict, "CB-V2-409D", "会议媒体暂不支持备用通道")
		return
	}
	if err := s.Manager.CheckWSConferenceMediaAccess(r.Context(), access.Device.ID, conferenceID); err != nil {
		writeFailure(w, err)
		return
	}
	conn, err := eventUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	q := r.URL.Query()
	codec := q.Get("codec")
	if err := s.Manager.ServeWSConferenceMedia(r.Context(), access.Device.ID, conferenceID, codec, conn, parseDenoiseQuery(q)); err != nil {
		slog.Warn("ws conference media attach failed", "conference_id", conferenceID, "error", err)
	}
	_ = conn.Close()
}

// callMediaProbe attaches a DETACHED echo session for candidate-quality
// measurement. The probe never touches the call's real media path, state or
// legs; it exists only so a client can compare a direct candidate's true
// round trip against the healthy transport before deciding to promote.
func (s *Server) callMediaProbe(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		SDP string `json:"sdp"`
		// Optional incremental-ICE flag (see callOffer).
		Trickle bool `json:"trickle,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || strings.TrimSpace(request.SDP) == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少 SDP offer")
		return
	}
	answer, err := s.Manager.AttachProbe(r.Context(), access.Device.ID, r.PathValue("id"), request.SDP, request.Trickle)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, answer)
}

// callMediaProbeCommit atomically promotes the measured probe onto the live
// media path (the negotiated peer connection replaces the WSS host inside
// the call's session; the WSS socket is detached server-side too). An
// optional {preflightId} adopts the caller's device-scoped preflight
// candidate (fresh foreground measurement) instead of a call-scoped probe.
// An optional {standby:true} (build 50, capability-gated) instead installs
// the probe into the hot-standby slot, leaving the current primary untouched.
func (s *Server) callMediaProbeCommit(w http.ResponseWriter, r *http.Request, access store.Access) {
	preflightID := ""
	asStandby := false
	var request struct {
		PreflightID string `json:"preflightId"`
		Standby     bool   `json:"standby"`
	}
	// The body is optional; a decode failure on an empty body must not fail
	// the plain commit path.
	_ = json.NewDecoder(r.Body).Decode(&request)
	preflightID = request.PreflightID
	asStandby = request.Standby
	callID := r.PathValue("id")
	if err := s.Manager.CommitProbe(r.Context(), access.Device.ID, callID, preflightID, asStandby); err != nil {
		writeFailure(w, err)
		return
	}
	if asStandby {
		// The ack carries the session epoch and the new standby instance's
		// identity token: the client's freshness clock starts HERE
		// (pre-adoption probe echo/stats never count) and every later
		// promotion command is fenced with these values.
		writeJSON(w, http.StatusOK, map[string]any{
			"committed": "true",
			"epoch":     s.Manager.MediaEpoch(callID),
			"token":     s.Manager.StandbyToken(callID, media.StandbyKindICE),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"committed": "true", "epoch": s.Manager.MediaEpoch(callID)})
}

// iceCandidateBatch is the shared incremental-candidate request body. The
// shape is deliberately tiny and strictly bounded server-side (max 8 lines
// per batch, 32 per attach, 512 bytes per line).
type iceCandidateBatch struct {
	AttachID    string   `json:"attachId"`
	PreflightID string   `json:"preflightId,omitempty"`
	Candidates  []string `json:"candidates"`
}

// callCandidates applies one incremental candidate batch to the call's
// current client attachment (the attachId comes from the offer response).
func (s *Server) callCandidates(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request iceCandidateBatch
	if !decodeBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.AttachID) == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少 attachId")
		return
	}
	added, err := s.Manager.AdoptCallCandidates(r.Context(), access.Device.ID, r.PathValue("id"),
		request.AttachID, request.Candidates)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added})
}

// callMediaProbeCandidates applies one incremental candidate batch to the
// call's detached measurement probe.
func (s *Server) callMediaProbeCandidates(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request iceCandidateBatch
	if !decodeBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.AttachID) == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少 attachId")
		return
	}
	added, err := s.Manager.AdoptProbeCandidates(r.Context(), access.Device.ID, r.PathValue("id"),
		request.AttachID, request.Candidates)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added})
}

// mediaPreflightCandidates applies one incremental candidate batch to the
// device-scoped preflight probe; both the preflight id and the attach token
// must be current.
func (s *Server) mediaPreflightCandidates(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request iceCandidateBatch
	if !decodeBody(w, r, &request) {
		return
	}
	added, err := s.Manager.AdoptPreflightCandidates(r.Context(), access.Device.ID,
		strings.TrimSpace(request.PreflightID), request.AttachID, request.Candidates)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added})
}

// candidatePollQuery parses the incremental-candidate poll parameters.
func candidatePollQuery(r *http.Request) (after int) {
	raw := strings.TrimSpace(r.URL.Query().Get("after"))
	if raw == "" {
		return 0
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

// callCandidatesPoll serves the gateway's incrementally gathered candidates
// for the call's current attachment (see callCandidates).
func (s *Server) callCandidatesPoll(w http.ResponseWriter, r *http.Request, access store.Access) {
	attachID := strings.TrimSpace(r.URL.Query().Get("attachId"))
	if attachID == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少 attachId")
		return
	}
	batch, err := s.Manager.CallLocalCandidates(r.Context(), access.Device.ID, r.PathValue("id"),
		attachID, candidatePollQuery(r))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (s *Server) callMediaProbeCandidatesPoll(w http.ResponseWriter, r *http.Request, access store.Access) {
	attachID := strings.TrimSpace(r.URL.Query().Get("attachId"))
	if attachID == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少 attachId")
		return
	}
	batch, err := s.Manager.ProbeLocalCandidates(r.Context(), access.Device.ID, r.PathValue("id"),
		attachID, candidatePollQuery(r))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (s *Server) mediaPreflightCandidatesPoll(w http.ResponseWriter, r *http.Request, access store.Access) {
	attachID := strings.TrimSpace(r.URL.Query().Get("attachId"))
	preflightID := strings.TrimSpace(r.URL.Query().Get("preflightId"))
	if attachID == "" || preflightID == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少 preflightId 或 attachId")
		return
	}
	batch, err := s.Manager.PreflightLocalCandidates(r.Context(), access.Device.ID, preflightID,
		attachID, candidatePollQuery(r))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

// callMediaStandbyPromote executes the build-50 hot-standby role swap:
// {"transport":"ws"|"ice","token","expectEpoch","nonce"} promotes the call's
// standby transport of that kind to the primary role; the previous primary
// is demoted into the standby slot and both transports stay attached. The
// token pins the exact standby instance, expectEpoch the client's view of
// the primary role, nonce gives idempotent retries; a fencing failure
// returns 409 and changes nothing.
func (s *Server) callMediaStandbyPromote(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		Transport   string `json:"transport"`
		Token       string `json:"token"`
		ExpectEpoch uint64 `json:"expectEpoch"`
		Nonce       string `json:"nonce"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil ||
		(request.Transport != media.StandbyKindWS &&
			request.Transport != media.StandbyKindICE &&
			request.Transport != media.StandbyKindQUIC) {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少有效的 transport (ws|ice|quic)")
		return
	}
	outcome, err := s.Manager.PromoteStandby(r.Context(), access.Device.ID, r.PathValue("id"),
		request.Transport, request.Token, request.ExpectEpoch, request.Nonce)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, outcome)
}

// callMediaState exposes the call session's transport roles + freshness for
// post-ack-loss reconciliation (coordinator rev 2 fencing).
func (s *Server) callMediaState(w http.ResponseWriter, r *http.Request, access store.Access) {
	state, err := s.Manager.MediaState(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// globalICE serves the client-facing ICE configuration without requiring a
// call, so a foreground client can warm a direct-path preflight before
// dialing. The daemon-level Manager.Ice already supports an empty call id.
func (s *Server) globalICE(w http.ResponseWriter, r *http.Request, access store.Access) {
	response, err := s.Manager.Ice(r.Context(), access.Device.ID, "")
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// mediaPreflight attaches a device-scoped, call-independent detached probe
// for foreground direct-path measurement. It creates no call, sends no SMS
// and captures no audio; the bounded echo data channel answers reachability
// and RTT freshness only.
func (s *Server) mediaPreflight(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		SDP string `json:"sdp"`
		// Optional incremental-ICE flag (see callOffer).
		Trickle bool `json:"trickle,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || strings.TrimSpace(request.SDP) == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少 SDP offer")
		return
	}
	answer, err := s.Manager.AttachPreflight(r.Context(), access.Device.ID, request.SDP, request.Trickle)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, answer)
}

// mediaPreflightDiscard discards the caller's device-scoped preflight probe.
func (s *Server) mediaPreflightDiscard(w http.ResponseWriter, r *http.Request, access store.Access) {
	preflightID := strings.TrimSpace(r.URL.Query().Get("preflightId"))
	s.Manager.DiscardPreflight(access.Device.ID, preflightID)
	w.WriteHeader(http.StatusNoContent)
}

// mediaPreflightRenew extends the server TTL of the caller's still-registered
// device-scoped preflight probe. The client keeps ONE measured, connected
// candidate continuously adoptable across TTL windows instead of rebuilding
// (and losing) it every 45 s; the peer connection, echo history and id are
// untouched, and no call/SMS/mic activity is involved. A stale, superseded or
// unknown id is an honest 404 so the client re-attaches.
func (s *Server) mediaPreflightRenew(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		PreflightID string `json:"preflightId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || strings.TrimSpace(request.PreflightID) == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少 preflightId")
		return
	}
	ttlMs, err := s.Manager.RenewPreflight(access.Device.ID, request.PreflightID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"preflightId": request.PreflightID, "ttlMs": ttlMs,
	})
}

// mediaRelayProbe upgrades an authenticated device to a call-independent,
// measurement-only relay WebSocket: app-level JSON ping/pong over the same
// transport a WSS call uses, with no call, media session or audio. The
// foreground app uses it to show a MEASURED idle relay RTT; the socket is
// bounded server-side and can never touch a live call.
func (s *Server) mediaRelayProbe(w http.ResponseWriter, r *http.Request, access store.Access) {
	conn, err := eventUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if err := s.Manager.ServeWSRelayProbe(r.Context(), access.Device.ID, conn); err != nil {
		slog.Warn("ws relay probe failed", "device", access.Device.ID, "error", err)
	}
	_ = conn.Close()
}

// deleteThread records an SMS conversation delete (tombstone) for the
// authenticated device. The global thread key is "lineId:workerKey"; phone
// numbers in keys contain no slashes, so a single path value is safe.
func (s *Server) restoreThread(w http.ResponseWriter, r *http.Request, access store.Access) {
	key := r.PathValue("key")
	if err := s.Manager.RestoreThread(r.Context(), access.Device.ID, key); err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"restored": true})
}

func (s *Server) deleteThread(w http.ResponseWriter, r *http.Request, access store.Access) {
	key := strings.TrimSpace(r.PathValue("key"))
	if err := s.Manager.DeleteThread(r.Context(), access.Device.ID, key); err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "deleted": true})
}

func (s *Server) callMediaProbeDiscard(w http.ResponseWriter, r *http.Request, access store.Access) {
	// Ownership is enforced by the probe attach path; discarding an unknown
	// probe is a safe no-op so retries cannot fail spuriously.
	s.Manager.DiscardProbe(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

// callMediaMeasure upgrades an authenticated request to the measurement-only
// WebSocket: JSON ping/pong for WSS-path RTT sampling alongside ANY active
// host transport (ICE included). It never replaces, feeds or detaches the
// session's media path, so measurement cannot affect live audio.
func (s *Server) callMediaMeasure(w http.ResponseWriter, r *http.Request, access store.Access) {
	if err := s.Manager.CheckWSMediaAccess(r.Context(), access.Device.ID, r.PathValue("id")); err != nil {
		writeFailure(w, err)
		return
	}
	conn, err := eventUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if err := s.Manager.ServeWSMeasure(r.Context(), access.Device.ID, r.PathValue("id"), conn); err != nil {
		slog.Warn("ws measure attach failed", "call_id", r.PathValue("id"), "error", err)
	}
	_ = conn.Close()
}

func (s *Server) callMedia(w http.ResponseWriter, r *http.Request, access store.Access) {
	// Browser media attaches authenticate via the client-scoped cookie; the
	// WebSocket upgrade from a browser always carries Origin, so a
	// cross-site upgrade is rejected by the origin check. Native attaches
	// keep using the device bearer (no Origin header sent).
	if _, _, bearerErr := s.Auth.Authenticate(r.Context(), r.Header.Get("Authorization")); bearerErr != nil {
		if s.WebPush != nil {
			if browserAccess, browserErr := s.browserClientAccess(r); browserErr == nil {
				access = browserAccess
			}
		}
	}
	callID := r.PathValue("id")
	// Pre-flight the same guards the runtime enforces so rejections are
	// plain HTTP errors instead of upgrade-time hangs.
	if err := s.Manager.CheckWSMediaAccess(r.Context(), access.Device.ID, callID); err != nil {
		writeFailure(w, err)
		return
	}
	// A merged leg's media is hosted by the conference session; a per-call
	// attach would steal the worker's single-owner socket.
	if err := s.Manager.CheckConferenceMediaGuard(r.Context(), callID); err != nil {
		writeFailure(w, err)
		return
	}
	conn, err := eventUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	q := r.URL.Query()
	codec := q.Get("codec")
	// Hot standby (build 50): clients that saw capabilities.mediaStandby may
	// attach a second socket in the standby role (standby=1). It never
	// evicts the primary; its close is silent. Old clients/servers never
	// send or see the parameter.
	standby := q.Get("standby") == "1" || q.Get("standby") == "true"
	if err := s.Manager.ServeWSMedia(r.Context(), access.Device.ID, callID, codec, conn, parseDenoiseQuery(q), standby); err != nil {
		slog.Warn("ws media attach failed", "call_id", callID, "error", err)
	}
	_ = conn.Close()
}

// ---------- conferences ----------

func (s *Server) mergeConference(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		CallIDs []string `json:"callIds"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	view, err := s.Manager.Merge(r.Context(), access.Device.ID, request.CallIDs)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

func (s *Server) getConference(w http.ResponseWriter, r *http.Request, access store.Access) {
	view, err := s.Manager.GetConference(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) closeConference(w http.ResponseWriter, r *http.Request, access store.Access) {
	if err := s.Manager.CloseConference(r.Context(), access.Device.ID, r.PathValue("id"), "closed_by_host"); err != nil {
		writeFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) conferenceOffer(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		SDP             string `json:"sdp"`
		Type            string `json:"type"`
		UplinkDenoise   *bool  `json:"uplinkDenoise,omitempty"`
		DownlinkDenoise *bool  `json:"downlinkDenoise,omitempty"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if request.Type != "offer" || strings.TrimSpace(request.SDP) == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "需要 type=offer 与 sdp")
		return
	}
	answer, err := s.Manager.ConferenceOffer(r.Context(), access.Device.ID, r.PathValue("id"), request.SDP, runtime.DenoiseChoice{
		Uplink: request.UplinkDenoise, Downlink: request.DownlinkDenoise,
	})
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, answer)
}

func (s *Server) splitConference(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		CallID string `json:"callId"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if err := s.Manager.Split(r.Context(), access.Device.ID, r.PathValue("id"), request.CallID); err != nil {
		writeFailure(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) legAction(w http.ResponseWriter, r *http.Request, access store.Access) {
	conferenceID := r.PathValue("id")
	callID := r.PathValue("callId")
	action := r.PathValue("action")
	switch action {
	case "hangup":
		if err := s.Manager.RemoveLeg(r.Context(), access.Device.ID, conferenceID, callID); err != nil {
			writeFailure(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "hold", "resume":
		view, err := s.Manager.SetLegHeld(r.Context(), access.Device.ID, conferenceID, callID, action == "hold")
		if err != nil {
			writeFailure(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	case "dtmf":
		var request struct {
			Digit string `json:"digit"`
		}
		if !decodeBody(w, r, &request) {
			return
		}
		if len([]rune(strings.TrimSpace(request.Digit))) != 1 {
			writeError(w, http.StatusBadRequest, "CB-V2-400", "digit 必须是单个字符")
			return
		}
		if err := s.Manager.LegDTMF(r.Context(), access.Device.ID, conferenceID, callID, request.Digit, idempotencyKey(r)); err != nil {
			writeFailure(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusNotFound, "CB-V2-404", "不支持的会议操作")
	}
}

// ---------- events ----------

var eventUpgrader = websocket.Upgrader{
	ReadBufferSize: 4096, WriteBufferSize: 4096,
	// Exact same-origin check: scheme, host and port all have to match the
	// request host. A substring/suffix match would let attacker-controlled
	// origins through.
	CheckOrigin: func(r *http.Request) bool {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin == "" {
			return true
		}
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || parsed.User != nil {
			return false
		}
		if parsed.Host != r.Host {
			return false
		}
		// The gateway always listens on plain loopback HTTP and a TLS
		// terminating reverse proxy forwards X-Forwarded-Proto, so r.TLS is
		// nil for every real browser hop. effectiveScheme() honors that
		// trusted-proxy header first and falls back to r.TLS.
		return parsed.Scheme == effectiveScheme(r)
	},
}

// eventAllowed applies the four-permission matrix per event: inbound call
// metadata needs receive_calls, outbound needs dial, inbound SMS needs
// receive_sms while a send-only device only sees its own outbound receipts,
// and voicemail requires receive_calls. Line-less events are never broadcast.
func (s *Server) eventAllowed(ctx context.Context, deviceID string, fresh store.Access, event events.Event) bool {
	if event.DeviceID != "" {
		return event.DeviceID == deviceID
	}
	if event.LineID == "" {
		return false
	}
	var entry store.LineAccess
	found := false
	for _, candidate := range fresh.Lines {
		if candidate.Line.ID == event.LineID {
			entry, found = candidate, true
			break
		}
	}
	if !found {
		return false
	}
	switch {
	case strings.HasPrefix(event.Type, "call."):
		var view struct {
			Direction     string `json:"direction"`
			OwnerDeviceID string `json:"ownerDeviceId"`
			Peer          string `json:"peer"`
		}
		if json.Unmarshal(event.Data, &view) != nil {
			return false
		}
		if view.Direction == "inbound" {
			if !entry.Permissions.ReceiveCalls {
				return false
			}
			// Per-key harassment scope: a key that explicitly denies this
			// caller receives no ring/event/push, while another key on the
			// same line is unaffected.
			return !s.Manager.HarassSuppressedForKey(ctx, fresh.Key.ID, harass.KindCall, view.Peer, "")
		}
		return entry.Permissions.Dial
	case strings.HasPrefix(event.Type, "message."):
		var message struct {
			Direction string `json:"direction"`
			Peer      string `json:"peer"`
			Body      string `json:"body"`
		}
		if json.Unmarshal(event.Data, &message) != nil {
			return false
		}
		if message.Direction == "outbound" {
			if entry.Permissions.ReceiveSMS {
				return true
			}
			return entry.Permissions.SendSMS
		}
		if !entry.Permissions.ReceiveSMS {
			return false
		}
		return !s.Manager.HarassSuppressedForKey(ctx, fresh.Key.ID, harass.KindSMS, message.Peer, message.Body)
	case strings.HasPrefix(event.Type, "thread."):
		// Per-key overlay events (delete/restore) carry the owning key id;
		// another key on the same line must never apply them.
		var view struct {
			KeyID string `json:"keyId"`
		}
		if json.Unmarshal(event.Data, &view) != nil {
			return false
		}
		if view.KeyID == "" || view.KeyID != fresh.Key.ID {
			return false
		}
		return entry.Permissions.ReceiveSMS
	case strings.HasPrefix(event.Type, "voicemail."):
		if !entry.Permissions.ReceiveCalls {
			return false
		}
		var voicemail struct {
			Peer string `json:"peer"`
		}
		if json.Unmarshal(event.Data, &voicemail) != nil {
			return false
		}
		return !s.Manager.HarassSuppressedForKey(ctx, fresh.Key.ID, harass.KindCall, voicemail.Peer, "")
	case strings.HasPrefix(event.Type, "recording."):
		// Key scope, not merely line scope: a recording belongs to the
		// enrollment key that started it, so a device on the same line via a
		// different key must not receive its lifecycle events. A conference
		// recording additionally requires every member line's CURRENT grant
		// (a revoked secondary line silences the event stream too).
		var view struct {
			KeyID        string `json:"keyId"`
			Kind         string `json:"kind"`
			ConferenceID string `json:"conferenceId"`
		}
		if json.Unmarshal(event.Data, &view) != nil {
			return false
		}
		if view.KeyID == "" || view.KeyID != fresh.Key.ID {
			return false
		}
		if view.Kind == "conference" && !s.Manager.ConferenceRecordingScopeAllowed(ctx, fresh, view.ConferenceID) {
			return false
		}
		return entry.Permissions.ReceiveCalls || entry.Permissions.Dial
	default:
		return entry.Permissions.ReceiveCalls || entry.Permissions.Dial ||
			entry.Permissions.ReceiveSMS || entry.Permissions.SendSMS
	}
}

func (s *Server) eventsStream(w http.ResponseWriter, r *http.Request) {
	access, _, err := s.Auth.Authenticate(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		// Browser clients carry the cookie instead (client scope). A
		// cross-site WebSocket upgrade would present a foreign Origin, which
		// browserClientAccess rejects, so the CSRF surface stays closed.
		if s.WebPush != nil {
			if browserAccess, browserErr := s.browserClientAccess(r); browserErr == nil {
				access = browserAccess
				err = nil
			}
		}
		if err != nil {
			writeFailure(w, auth.ErrUnauthorized)
			return
		}
	}
	after, _ := json.Number(strings.TrimSpace(r.URL.Query().Get("after"))).Int64()
	conn, err := eventUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	deviceID := access.Device.ID
	s.Manager.DeviceConnected(deviceID)
	defer s.Manager.DeviceDisconnected(deviceID)

	ctx, cancelConnection := context.WithCancel(r.Context())
	defer cancelConnection()

	// Read pump: detects phone-side close immediately so presence and
	// subscription cleanup happen without waiting for a future write. It also
	// refreshes the read deadline on client pongs.
	go func() {
		conn.SetReadLimit(4096)
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		})
		for {
			if _, _, readErr := conn.ReadMessage(); readErr != nil {
				cancelConnection()
				return
			}
		}
	}()

	filter := func(event events.Event) bool {
		fresh, accessErr := s.Store.DeviceAccess(context.Background(), deviceID)
		if accessErr != nil {
			return false
		}
		return s.eventAllowed(context.Background(), deviceID, fresh, event)
	}

	stream, cancelStream := s.Bus.Subscribe(ctx, after, filter)
	defer cancelStream()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, accessErr := s.Store.DeviceAccess(ctx, deviceID); accessErr != nil {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "revoked"), time.Now().Add(2*time.Second))
				return
			}
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				return
			}
		case event, ok := <-stream:
			if !ok {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "resync"), time.Now().Add(2*time.Second))
				return
			}
			fresh, accessErr := s.Store.DeviceAccess(ctx, deviceID)
			if accessErr != nil {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "revoked"), time.Now().Add(2*time.Second))
				return
			}
			if !s.eventAllowed(ctx, deviceID, fresh, event) {
				continue
			}
			if err := conn.WriteJSON(event); err != nil {
				return
			}
		}
	}
}

func (s *Server) listVoicemails(w http.ResponseWriter, r *http.Request, access store.Access) {
	views, err := s.Manager.ListVoicemails(r.Context(), access.Device.ID, strings.TrimSpace(r.URL.Query().Get("line")))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getVoicemail(w http.ResponseWriter, r *http.Request, access store.Access) {
	voicemail, err := s.Manager.VoicemailForDevice(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": voicemail.ID, "lineId": voicemail.LineID, "peer": voicemail.Peer,
		"durationMs": voicemail.DurationMs, "sizeBytes": voicemail.SizeBytes,
		"createdAt": voicemail.CreatedAt.UnixMilli(), "expiresAt": voicemail.ExpiresAt.UnixMilli(),
	})
}

func (s *Server) deleteVoicemail(w http.ResponseWriter, r *http.Request, access store.Access) {
	view, err := s.Manager.DeleteVoicemail(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": view.ID, "lineId": view.LineID, "deleted": true})
}

func (s *Server) voicemailAudio(w http.ResponseWriter, r *http.Request, access store.Access) {
	voicemail, err := s.Manager.VoicemailForDevice(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	file, err := os.Open(voicemail.Path)
	if err != nil {
		writeError(w, http.StatusGone, "CB-V2-410", "留言音频已不可用")
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, voicemail.ID+".wav", stat.ModTime(), file)
}

// ---------- manual call recording ----------

func (s *Server) recordingStart(w http.ResponseWriter, r *http.Request, access store.Access) {
	view, err := s.Manager.StartRecording(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) recordingStop(w http.ResponseWriter, r *http.Request, access store.Access) {
	view, err := s.Manager.StopRecording(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) conferenceRecordingStart(w http.ResponseWriter, r *http.Request, access store.Access) {
	view, err := s.Manager.StartConferenceRecording(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) conferenceRecordingStop(w http.ResponseWriter, r *http.Request, access store.Access) {
	view, err := s.Manager.StopConferenceRecording(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) listRecordings(w http.ResponseWriter, r *http.Request, access store.Access) {
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	views, err := s.Manager.ListRecordings(r.Context(), access.Device.ID, strings.TrimSpace(r.URL.Query().Get("line")), limit)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (s *Server) getRecording(w http.ResponseWriter, r *http.Request, access store.Access) {
	view, err := s.Manager.RecordingDetailForDevice(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) deleteRecording(w http.ResponseWriter, r *http.Request, access store.Access) {
	view, err := s.Manager.DeleteRecording(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": view.ID, "deleted": true})
}

func (s *Server) recordingAudio(w http.ResponseWriter, r *http.Request, access store.Access) {
	path, err := s.Manager.RecordingAudioPath(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusGone, "CB-V2-410", "录音文件已不可用")
			return
		}
		writeFailure(w, err)
		return
	}
	s.serveRecordingBlob(w, r, path, "audio/wav", r.PathValue("id")+".wav")
}

func (s *Server) recordingWaveform(w http.ResponseWriter, r *http.Request, access store.Access) {
	path, err := s.Manager.RecordingWaveformPath(r.Context(), access.Device.ID, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusGone, "CB-V2-410", "录音波形已不可用")
			return
		}
		writeFailure(w, err)
		return
	}
	s.serveRecordingBlob(w, r, path, "application/json; charset=utf-8", r.PathValue("id")+".waveform.json")
}

func (s *Server) serveRecordingBlob(w http.ResponseWriter, r *http.Request, path, contentType, name string) {
	file, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusGone, "CB-V2-410", "录音文件已不可用")
		return
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(stat.Size(), 10))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, name, stat.ModTime(), file)
}

func (s *Server) recordingSettings(w http.ResponseWriter, r *http.Request, access store.Access) {
	view, err := s.Manager.RecordingSettings(r.Context(), access.Device.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func decodeBase64(value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, errors.New("empty value")
	}
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(trimmed)
}

func (s *Server) threadActions(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		Actions []runtime.SMSThreadAction `json:"actions"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if len(request.Actions) == 0 || len(request.Actions) > 100 {
		writeError(w, 400, "CB-V2-400", "每批需要 1 到 100 项操作")
		return
	}
	writeJSON(w, 200, map[string]any{"results": s.Manager.ThreadActions(r.Context(), access.Device.ID, request.Actions)})
}
