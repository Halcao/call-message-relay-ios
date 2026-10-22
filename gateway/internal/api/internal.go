package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"callrelay/gateway/internal/modem"
	"callrelay/gateway/internal/voice"

	"github.com/gorilla/websocket"
)

// internalUpgrader accepts the loopback-only media socket. The listener is
// bound to 127.0.0.1 and every request must carry the shared internal token,
// so origin checks add no security here.
var internalUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// InternalHandler is the trusted interface consumed by the unified CallRelay
// service on the same host: it exposes the line status, runtime SMS mode,
// voice recovery and the raw PCMU media socket that lets the unified service
// terminate client-facing WebRTC without opening a second ALSA/UAC pair.
func (s *Server) InternalHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/v1/health", s.internalAuth(token, method(http.MethodGet, s.internalHealth)))
	mux.HandleFunc("/internal/v1/line", s.internalAuth(token, method(http.MethodGet, s.internalLine)))
	mux.HandleFunc("/internal/v1/identity/sim", s.internalAuth(token, method(http.MethodGet, s.internalSIMIdentity)))
	mux.HandleFunc("/internal/v1/diagnostics/at", s.internalAuth(token, method(http.MethodGet, s.internalATDiagnostics)))
	mux.HandleFunc("/internal/v1/media/", s.internalAuth(token, s.internalMedia))
	mux.HandleFunc("/internal/v1/settings/sms-dry-run", s.internalAuth(token, s.internalSMSDryRun))
	mux.HandleFunc("/internal/v1/recover", s.internalAuth(token, method(http.MethodPost, s.internalRecover)))
	return mux
}

func (s *Server) internalAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	expected := []byte(strings.TrimSpace(token))
	return func(w http.ResponseWriter, r *http.Request) {
		presented := strings.TrimSpace(r.Header.Get("X-Internal-Token"))
		if presented == "" {
			if authorization := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(authorization, "Bearer ") {
				presented = strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
			}
		}
		if len(expected) == 0 || subtle.ConstantTimeCompare([]byte(presented), expected) != 1 {
			writeError(w, http.StatusUnauthorized, "CB-INT-001", "internal token required")
			return
		}
		next(w, r)
	}
}

func (s *Server) internalHealth(w http.ResponseWriter, _ *http.Request) {
	s.lineMu.RLock()
	line := s.Line
	s.lineMu.RUnlock()
	status := "ok"
	if line.SMS == "unavailable" && line.Voice == "unavailable" {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "version": s.Version, "line": line})
}

func (s *Server) internalLine(w http.ResponseWriter, _ *http.Request) {
	s.lineMu.RLock()
	line := s.Line
	s.lineMu.RUnlock()
	writeJSON(w, http.StatusOK, line)
}

// internalSIMIdentity returns the SIM own number + ICCID read live through
// the modem's serialized AT queue. It is reachable only from the trusted
// loopback network with the shared internal token; the raw number is stored
// by the unified daemon and never rendered to public clients or the console.
// Queries are read-only (AT+CNUM / AT+CCID) and bounded so a wedged port can
// not pin the queue.
func (s *Server) internalSIMIdentity(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.CallModem.(modem.IdentityReader)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, modem.SIMIdentity{NumberSource: "unsupported"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	identity, err := reader.OwnIdentity(ctx)
	if err != nil {
		slog.Warn("sim identity read failed", "error", err)
		writeJSON(w, http.StatusBadGateway, modem.SIMIdentity{NumberSource: "error"})
		return
	}
	writeJSON(w, http.StatusOK, identity)
}

// internalATDiagnostics runs the fixed read-only AT allowlist through the
// worker's serialized modem queue. It exists to attribute a failed dial from
// the modem side (release cause via AT+CEER, IMS registration) without
// opening a competing serial reader. The command list is fixed in the modem
// adapter; the request carries no parameters.
func (s *Server) internalATDiagnostics(w http.ResponseWriter, r *http.Request) {
	diagnoser, ok := s.CallModem.(modem.ATDiagnoser)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"supported": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	results, err := diagnoser.Diagnostics(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "CB-INT-004", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"supported": true, "at": results})
}

type smsDryRunRequest struct {
	Enabled *bool `json:"enabled"`
}

func (s *Server) internalSMSDryRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		writeError(w, http.StatusMethodNotAllowed, "CB-API-001", "method not allowed")
		return
	}
	setter, canSet := s.CallModem.(interface{ SetDryRun(bool) })
	getter, canGet := s.CallModem.(interface{ DryRun() (bool, bool) })
	if r.Method == http.MethodPut && !canSet {
		writeError(w, http.StatusNotImplemented, "CB-INT-002", "runtime SMS mode is not supported by this backend")
		return
	}
	if r.Method == http.MethodPut {
		var request smsDryRunRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		if err := decoder.Decode(&request); err != nil || request.Enabled == nil {
			writeError(w, http.StatusBadRequest, "CB-INT-003", "body must be {\"enabled\": true|false}")
			return
		}
		setter.SetDryRun(*request.Enabled)
		slog.Info("internal SMS mode changed", "dry_run", *request.Enabled)
	}
	enabled := true
	known := false
	if canGet {
		enabled, known = getter.DryRun()
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": enabled, "supported": known || canSet})
}

// internalRecover re-probes the voice backend (re-bootstrapping the QDC507
// route when needed) and reports the refreshed state. The unified service
// escalates to a process restart if this still fails.
func (s *Server) internalRecover(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	response := map[string]any{"voice": "unavailable", "restarted": false}
	if s.VoiceAudio == nil {
		response["voice"] = "unavailable"
		response["errorCode"] = "VOICE_BACKEND_ABSENT"
		writeJSON(w, http.StatusServiceUnavailable, response)
		return
	}
	capabilities, err := s.VoiceAudio.Probe(ctx)
	if err != nil {
		slog.Warn("internal voice recovery failed", "error", err)
		response["voice"] = "failed"
		response["errorCode"] = "VOICE_PROBE_FAILED"
		writeJSON(w, http.StatusServiceUnavailable, response)
		return
	}
	s.lineMu.Lock()
	s.Line.Voice = "ready"
	s.lineMu.Unlock()
	response["voice"] = "ready"
	response["audio"] = capabilities
	writeJSON(w, http.StatusOK, response)
}

// internalMedia upgrades to a binary WebSocket that carries 160-byte PCMU
// frames in both directions. Server->client frames come from the modem UAC
// capture; client->server frames are queued for modem playback. The bridge
// starts only after AT+CLCC reports the cellular leg active, matching the
// firmware-wedge workaround used by the SIP path.
func (s *Server) internalMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "CB-API-001", "method not allowed")
		return
	}
	callID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/internal/v1/media/"), "/")
	if callID == "" || strings.Contains(callID, "/") {
		writeError(w, http.StatusNotFound, "CB-CALL-006", "call not found")
		return
	}
	if s.Database == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-CALL-000", "call database unavailable")
		return
	}
	if _, err := s.Database.GetCall(r.Context(), callID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "CB-CALL-006", "call not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "CB-CALL-004", err.Error())
		return
	}
	if s.VoiceAudio == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-VOICE-001", "voice audio backend is unavailable")
		return
	}
	conn, err := internalUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	peer := &internalMediaPeer{conn: conn}
	bridge := voice.NewBridge(s.VoiceAudio, internalDropTransport{})
	bridge.SetFrameHooks(func(pcm []int16) {
		frame, encodeErr := voice.EncodePCMU(pcm)
		if encodeErr != nil {
			return
		}
		if writeErr := peer.writeBinary(frame); writeErr != nil {
			_ = conn.Close()
		}
	}, nil)

	s.webrtcMu.Lock()
	previous := s.voiceBridges[callID]
	s.voiceBridges[callID] = bridge
	s.webrtcMu.Unlock()
	if previous != nil && previous != bridge {
		// Serialized with start/teardown so the replacement release and the
		// new socket's acquire below cannot interleave with another call's
		// device take.
		s.mediaMu.Lock()
		_ = previous.Stop(context.Background())
		s.mediaMu.Unlock()
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer func() {
		s.releaseMedia(callID, bridge)
		s.webrtcMu.Lock()
		if s.voiceBridges[callID] == bridge {
			delete(s.voiceBridges, callID)
		}
		s.webrtcMu.Unlock()
	}()

	active := true
	if waiter, ok := s.CallModem.(interface {
		WaitAnyActive(context.Context) (bool, error)
	}); ok {
		readyCtx, readyCancel := context.WithTimeout(ctx, 45*time.Second)
		active, err = waiter.WaitAnyActive(readyCtx)
		readyCancel()
		if err != nil {
			active = false
		}
	}
	if !active {
		_ = peer.writeControl(internalMediaError{Type: "error", Code: "cellular_not_active"})
		return
	}
	// acquireMedia serializes the decision "is this call still the one active
	// cellular leg?" with bridge start and with end-of-call teardown. A late
	// socket whose call already ended must never take the shared single-owner
	// audio device: observed live, such a socket leaked the QDC507 lock and
	// every following outbound call failed bridge start while the remote phone
	// rang.
	startErr := s.acquireMedia(ctx, callID, bridge)
	switch {
	case errors.Is(startErr, errMediaCallEnded):
		slog.Warn("internal media socket arrived after call ended; not starting audio",
			"call_id", callID, "active_call", s.activeCallID())
		_ = peer.writeControl(internalMediaError{Type: "error", Code: "call_ended"})
		return
	case startErr != nil:
		slog.Warn("internal media bridge start failed", "call_id", callID, "error", startErr)
		_ = peer.writeControl(internalMediaError{Type: "error", Code: "audio_start_failed"})
		return
	}
	if s.CallControl != nil {
		if current, event, markErr := s.CallControl.MarkMediaReady(callID); markErr == nil {
			_, _ = s.persistCall(context.Background(), current, event)
		}
	}
	if err := peer.writeControl(map[string]string{"type": "ready", "callId": callID}); err != nil {
		return
	}

	for {
		messageType, payload, readErr := conn.ReadMessage()
		if readErr != nil {
			return
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		if len(payload) != voice.FrameSamples {
			continue
		}
		if enqueueErr := bridge.EnqueueClientPCMU(payload); enqueueErr != nil {
			return
		}
	}
}

// errNoCallRow is kept so callers can distinguish a missing call without
// importing database/sql just for one sentinel.
var errNoCallRow = errors.New("call not found")

// errMediaCallEnded marks a media socket whose call is no longer the active
// cellular leg; it must not take the audio device.
var errMediaCallEnded = errors.New("call ended before media start")

// activeCallID reports the call controller's current active call id ("" when
// idle).
func (s *Server) activeCallID() string {
	if s.CallControl == nil {
		return ""
	}
	if current := s.CallControl.Snapshot(); current != nil {
		return current.ID
	}
	return ""
}

// acquireMedia is the single serialized decision point for taking the
// single-owner audio device. While holding mediaMu it checks the call
// controller, starts the bridge, and — when a previous bridge's teardown left
// a foreign lock — reclaims that lock ONLY while the owner is still the stale
// call (StopIfOwner). A socket whose call ended and a new call became active
// can therefore never stop the new call's audio, and a foreign lock cannot be
// released while its real call still owns it.
//
// Lock discipline: mediaMu may be held while taking webrtcMu (the stale-bridge
// lookup below), but webrtcMu is NEVER held while taking mediaMu — the WebRTC
// offer handler releases webrtcMu before starting a bridge. That ordering is
// what makes the serialized start/end/teardown here race-free.
func (s *Server) acquireMedia(ctx context.Context, callID string, bridge *voice.Bridge) error {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	if s.activeCallID() != callID {
		return errMediaCallEnded
	}
	start := func() error {
		if err := bridge.Start(ctx, modem.CallID(callID)); err != nil {
			return err
		}
		// Bridge start can take seconds (remote route bootstrap). The call
		// may have ended on a terminal URC while we were starting: with
		// closeWebRTC's stop serialized on mediaMu, the ended call's
		// teardown either already ran (the pre-check above caught it) or
		// runs after we return — so a dead call must never keep audio here.
		if s.activeCallID() != callID {
			_ = bridge.Stop(ctx)
			return errMediaCallEnded
		}
		return nil
	}
	heldErr := start()
	if heldErr == nil || !isAudioHeldElsewhere(heldErr) {
		return heldErr
	}
	owned, ok := s.VoiceAudio.(modem.OwnedVoiceAudio)
	if !ok {
		return heldErr
	}
	holder := string(owned.CurrentOwner())
	// Reclaim only a foreign holder that is NOT a live call (ended) or
	// the current socket's call. Never release a holder which is still
	// the controller's active call.
	if holder == "" || holder == callID || holder == s.activeCallID() {
		return heldErr
	}
	// Stop tracked bridges belonging to the stale holder before dropping
	// its device lock; owner-conditional release protects a call that
	// took over between the checks.
	s.webrtcMu.Lock()
	stale := s.voiceBridges[holder]
	delete(s.voiceBridges, holder)
	s.webrtcMu.Unlock()
	if stale != nil {
		_ = stale.Stop(ctx)
	}
	// stale.Stop may ALREADY have released the device (its recorded call id
	// is the holder, so its owner-conditional stop succeeded). Only a device
	// that is still held afterwards is a genuine reclaim failure.
	if owned.CurrentOwner() != "" && !owned.StopIfOwner(ctx, modem.CallID(holder)) {
		return heldErr
	}
	slog.Warn("reclaimed stale audio lock", "call_id", callID, "stale_owner", holder)
	return start()
}

// releaseMedia is the serialized counterpart for socket teardown: the bridge
// loops are stopped (idempotent; on single-owner backends the device release
// inside Bridge.Stop is owner-conditional, so a stale socket's teardown can
// never cut a newer call's audio). The extra StopIfOwner covers a bridge that
// never started (and so recorded no call id) while the device is somehow still
// named for callID. A newer call that legitimately took over keeps its audio.
func (s *Server) releaseMedia(callID string, bridge *voice.Bridge) {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	_ = bridge.Stop(context.Background())
	if owned, ok := s.VoiceAudio.(modem.OwnedVoiceAudio); ok {
		owned.StopIfOwner(context.Background(), modem.CallID(callID))
	}
}

// isAudioHeldElsewhere recognizes the single-owner audio lock errors emitted
// by every voice backend ("QDC507/ALSA/PCM audio is already active for call …").
// Those backends share one capture/playback device per line, so a foreign lock
// means an earlier bridge never released it.
func isAudioHeldElsewhere(err error) bool {
	return err != nil && strings.Contains(err.Error(), "audio is already active")
}

type internalMediaError struct {
	Type string `json:"type"`
	Code string `json:"code"`
}

type internalMediaPeer struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

// internalMediaWriteDeadline bounds one capture-frame write toward the
// unified daemon. The capture hook runs on the ALSA read loop: without a
// deadline a stalled daemon socket would block the read loop and stall the
// UAC capture indefinitely. A miss closes the socket and the bridge stops.
const internalMediaWriteDeadline = 1 * time.Second

func (p *internalMediaPeer) writeBinary(frame []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(internalMediaWriteDeadline))
	return p.conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (p *internalMediaPeer) writeControl(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_ = p.conn.SetWriteDeadline(time.Now().Add(internalMediaWriteDeadline))
	return p.conn.WriteMessage(websocket.TextMessage, payload)
}

// internalDropTransport satisfies voice.PCMUTransport for the internal media
// path. Modem playback is fed through Bridge.EnqueueClientPCMU directly and
// no client-facing WebRTC session exists in the worker.
type internalDropTransport struct{}

func (internalDropTransport) WritePCMU([]byte) error   { return nil }
func (internalDropTransport) OnPCMUFrame(func([]byte)) {}
