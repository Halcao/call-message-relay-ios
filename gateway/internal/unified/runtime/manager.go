// Package runtime is the unified control plane. It arbitrates calls and
// conferences across all lines, owns the client-facing media sessions,
// applies line policies and fans events out to authorized devices.
package runtime

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"callrelay/gateway/internal/push"
	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/events"
	"callrelay/gateway/internal/unified/harass"
	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/notify"
	"callrelay/gateway/internal/unified/recording"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/unified/worker"
	"callrelay/gateway/internal/webpush"

	pion "github.com/pion/webrtc/v4"
)

const (
	StateRinging    = "incoming_ringing"
	StateDialing    = "outgoing_dialing"
	StateAnswering  = "answering"
	StateConnecting = "connecting"
	StateActive     = "active"
	StateHeld       = "held"
	StateEnding     = "ending"
	StateEnded      = "ended"
)

const (
	CodeForbidden       = "CB-V2-403"
	CodeLineUnavailable = "CB-V2-410"
	CodeCapacity        = "CB-V2-429"
	CodeLineBusy        = "CB-V2-409"
	CodeCallGone        = "CB-V2-404"
	CodeAlreadyAnswered = "CB-V2-409A"
	CodeConferenceSize  = "CB-V2-422"
	CodePolicyBlocked   = "CB-V2-451"
	CodeInternal        = "CB-V2-500"
)

type Failure struct {
	Status  int
	Code    string
	Message string
}

func (f *Failure) Error() string { return f.Message }

func fail(status int, code, message string) *Failure {
	return &Failure{Status: status, Code: code, Message: message}
}

var (
	ErrForbidden = fail(403, CodeForbidden, "当前设备没有该权限")
	ErrCallGone  = fail(404, CodeCallGone, "通话不存在或已结束")
	ErrCapacity  = fail(429, CodeCapacity, "并发通话已达配置上限")
	ErrLineBusy  = fail(409, CodeLineBusy, "该线路已有通话进行中")
)

// LineIdentity is the masked, console-safe identity shown for a line. The
// preparation script contributes the module fields; the unified daemon adds
// the resolved number/source from its private identity store.
type LineIdentity struct {
	LineID         string `json:"lineId"`
	ModuleKey      string `json:"moduleKey"`
	ModuleUSB      string `json:"usbPath"`
	ModuleFirmware string `json:"firmware"`
	SIMMasked      string `json:"simMasked"`
	PhoneMasked    string `json:"phoneMasked"`
	// NumberSource is safe for clients: sim | manual | empty | unsupported |
	// sim_changed | none. It never carries a raw number.
	NumberSource string `json:"numberSource"`
	// OperatorAlpha is the network-reported operator name. Some firmware
	// returns a garbled token ("????"); OperatorNumeric (MCC+MNC) is exposed
	// as the truthful fallback instead of inventing a name.
	OperatorAlpha   string `json:"operatorAlpha,omitempty"`
	OperatorNumeric string `json:"operatorNumeric,omitempty"`
	Registration    string `json:"registration,omitempty"`
	AccessTech      string `json:"accessTech,omitempty"`
	SIMChanged      bool   `json:"simChanged"`
	UpdatedAt       int64  `json:"updatedAt"`
}

type Line struct {
	Cfg    config.Line
	Client *worker.Client

	cancel context.CancelFunc

	mu              sync.RWMutex
	status          worker.LineStatus
	statusAt        time.Time
	reachable       bool
	dryRun          bool
	dryRunKnown     bool
	dryRunSupported bool
	lastError       string
	identity        store.LineIdentity
	identityAt      time.Time
	simPending      bool
}

func (l *Line) Status() (worker.LineStatus, bool, time.Time) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.status, l.reachable, l.statusAt
}

func (l *Line) SMSDryRun() (bool, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.dryRun, l.dryRunSupported
}

type Manager struct {
	Cfg       config.Config
	Store     *store.Store
	Bus       *events.Bus
	Auth      *auth.Service
	Push      push.Sender
	Engine    *media.Engine
	GatewayID string
	// Bark is the OPTIONAL self-hosted notification client. Nil keeps the
	// bridge off; native push/ringing is never affected.
	Bark *push.BarkClient
	// WebPush is the self-hosted PWA notification path. Nil or a disabled
	// config keeps it off. WebPushSender performs the actual endpoint POSTs
	// (injectable in tests); WebPush issues handoff tokens.
	WebPush       *webpush.Manager
	WebPushSender *webpush.Sender
	// Notify is the optional service-alert monitor (confirmed carrier arrears
	// and designated-backup network availability). Nil keeps it off; alerts
	// are always ordinary pushes, never VoIP.
	Notify *notify.Monitor

	// Recordings is the manual call-recording storage engine. Nil keeps the
	// API surface reporting unavailable; production wires it in main.go
	// before Start.
	Recordings *recording.Manager

	mu    sync.Mutex
	lines map[string]*Line
	// quicCapabilityProvider advertises the build-61 QUIC direct media
	// capability (installed by main when the listener starts; nil keeps
	// the ice+ws advertisement unchanged).
	quicCapabilityProvider func() (map[string]any, bool)
	sessions               map[string]*media.Session
	parked                 map[string]*media.Leg
	graces                 map[string]*recoveryEpisode
	voicemailTimers        map[string]*time.Timer
	probes                 map[string]*probeEntry
	preflights             map[string]*preflightEntry
	presence               map[string]int
	closed                 bool
	barkDedup              *push.BarkDeduper
	webpushDedup           *push.BarkDeduper
	// smsPushDedup guards the native SMS alert fan-out: a worker reconnect or
	// event replay may surface the same message.created twice in-process; one
	// device must still receive exactly one notification per message id.
	smsPushDedup *push.BarkDeduper
	// autoRecordFence remembers manual recording stops per call/conference
	// id so auto recording never restarts on that same call.
	autoRecordFence autoRecordFence
}

// probeEntry binds a detached probe session to its expiry timer so a commit
// can stop the timer before adoption: otherwise the TTL could fire mid-adopt
// and close the peer connection the live session just took over.
type probeEntry struct {
	session *media.Session
	timer   *time.Timer
}

// preflightEntry binds a device-scoped (call-independent) detached probe to
// the unguessable id the client uses when adopting it for a call commit.
//
// Lease fence (gen/deadline, both mutated only under Manager.mu): every
// expiry callback captures the generation it was armed for, and may reclaim
// the entry only while that generation is still current. A renewal stops the
// current timer and arms a NEW one for the next generation, because
// time.Timer.Reset would re-run the old closure with the stale generation
// (and cannot cancel a callback that already fired and is blocked on m.mu);
// without the fence a successful renewal could be deleted immediately by the
// previous generation's callback. deadline is the current lease expiry: a
// renewal refuses once it has passed, so an expired lease is never
// resurrected. Delivery generations are monotonic within one entry; a
// superseding attach creates a new entry (new id and probe) whose callbacks
// match only that entry.
type preflightEntry struct {
	id       string
	probe    *probeEntry
	gen      uint64
	deadline time.Time
}

func NewManager(cfg config.Config, st *store.Store, bus *events.Bus, authService *auth.Service, engine *media.Engine, pushSender push.Sender) *Manager {
	return &Manager{
		Cfg: cfg, Store: st, Bus: bus, Auth: authService, Engine: engine, Push: pushSender,
		GatewayID: cfg.Gateway.ID,
		lines:     make(map[string]*Line), sessions: make(map[string]*media.Session),
		parked: make(map[string]*media.Leg), graces: make(map[string]*recoveryEpisode),
		voicemailTimers: make(map[string]*time.Timer), probes: make(map[string]*probeEntry),
		preflights:   make(map[string]*preflightEntry),
		presence:     make(map[string]int),
		barkDedup:    push.NewBarkDeduper(10 * time.Minute),
		webpushDedup: push.NewBarkDeduper(10 * time.Minute),
		smsPushDedup: push.NewBarkDeduper(10 * time.Minute),
	}
}

func (m *Manager) Start(ctx context.Context) error {
	for _, configuredLine := range m.Cfg.Lines {
		if err := m.Store.EnsureLine(ctx, store.Line{
			ID: configuredLine.ID, Name: configuredLine.Name, WorkerURL: configuredLine.WorkerURL,
			TokenFile: configuredLine.TokenFile, IdentityFile: configuredLine.IdentityFile, Enabled: configuredLine.Enabled,
		}); err != nil {
			return err
		}
	}
	configured, err := m.Store.ListLines(ctx)
	if err != nil {
		return err
	}
	_ = configured
	for _, configuredLine := range m.Cfg.Lines {
		if !configuredLine.Enabled {
			continue
		}
		m.startLine(ctx, configuredLine)
	}
	go m.RunVoicemailRetention(ctx)
	go m.WatchLines(ctx)
	if stunEndpoints, malformed := m.Cfg.STUNEndpoints(); len(stunEndpoints) > 0 || len(malformed) > 0 {
		slog.Info("STUN address discovery configured",
			"servers", stunEndpoints, "ignored", malformed,
			"policy_note", "STUN discovers public addresses only; no relay is hosted and WSS fallback is unaffected")
	}
	slog.Info("unified runtime started", "lines", len(m.lines), "voicemail_greeting_ready", greetingSamples() != nil)
	return nil
}

func (m *Manager) startLine(parent context.Context, configuredLine config.Line) *Line {
	m.mu.Lock()
	if existing := m.lines[configuredLine.ID]; existing != nil {
		m.mu.Unlock()
		return existing
	}
	lineCtx, cancel := context.WithCancel(parent)
	runtimeLine := &Line{
		Cfg: configuredLine, cancel: cancel,
		Client: worker.New(configuredLine.ID, configuredLine.WorkerURL, configuredLine.InternalURL, configuredLine.TokenFile, configuredLine.IdentityFile, m.Store, m.Auth.Identity),
	}
	m.lines[configuredLine.ID] = runtimeLine
	m.mu.Unlock()
	go m.consumeEvents(lineCtx, runtimeLine)
	go m.pollLine(lineCtx, runtimeLine)
	go m.reconcileLine(lineCtx, runtimeLine)
	go m.pollIdentity(lineCtx, runtimeLine)
	slog.Info("line runtime started", "line", configuredLine.ID, "worker", configuredLine.WorkerURL)
	return runtimeLine
}

func (m *Manager) stopLine(lineID string) {
	m.mu.Lock()
	runtimeLine := m.lines[lineID]
	delete(m.lines, lineID)
	m.mu.Unlock()
	if runtimeLine == nil {
		return
	}
	if runtimeLine.cancel != nil {
		runtimeLine.cancel()
	}
	slog.Info("line runtime stopped", "line", lineID)
}

// WatchLines re-reads the generated lines file so newly discovered modules
// are added and removed modules are stopped without a service restart.
func (m *Manager) WatchLines(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reloadLines(ctx)
		}
	}
}

func (m *Manager) reloadLines(ctx context.Context) {
	path := strings.TrimSpace(m.Cfg.Gateway.LinesFile)
	if path == "" {
		return
	}
	lines, err := config.LoadLinesFile(path)
	if err != nil {
		slog.Warn("reload lines file failed", "path", path, "error", err)
		return
	}
	desired := map[string]config.Line{}
	for _, line := range lines {
		if line.ID == "" {
			continue
		}
		desired[line.ID] = line
		line := line
		if err := m.Store.EnsureLine(ctx, store.Line{
			ID: line.ID, Name: line.Name, WorkerURL: line.WorkerURL,
			TokenFile: line.TokenFile, IdentityFile: line.IdentityFile, Enabled: line.Enabled,
		}); err != nil {
			slog.Warn("ensure line failed", "line", line.ID, "error", err)
			continue
		}
		running := m.Line(line.ID)
		if running == nil {
			if line.Enabled {
				m.startLine(ctx, line)
			}
			continue
		}
		if !line.Enabled || running.Cfg.WorkerURL != line.WorkerURL {
			m.stopLine(line.ID)
			if line.Enabled {
				m.startLine(ctx, line)
			}
		}
	}
	for _, running := range m.Lines() {
		if _, ok := desired[running.Cfg.ID]; !ok {
			m.stopLine(running.Cfg.ID)
		}
	}
	m.mu.Lock()
	m.Cfg.Lines = lines
	m.mu.Unlock()
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	sessions := make([]*media.Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessions = map[string]*media.Session{}
	episodes := make([]*recoveryEpisode, 0, len(m.graces))
	for _, ep := range m.graces {
		episodes = append(episodes, ep)
	}
	m.graces = map[string]*recoveryEpisode{}
	m.mu.Unlock()
	// Flush and finalize active recordings before their sessions close (a
	// bounded wait; work directories left by an interrupted finalize are
	// reconciled by Recover on the next start).
	if m.Recordings != nil {
		m.Recordings.CloseAll("gateway_shutdown")
	}
	for _, session := range sessions {
		session.Close()
	}
	for _, ep := range episodes {
		ep.timer.Stop()
	}
	m.mu.Lock()
	for _, timer := range m.voicemailTimers {
		timer.Stop()
	}
	m.voicemailTimers = map[string]*time.Timer{}
	m.mu.Unlock()
}

func (m *Manager) Line(lineID string) *Line {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lines[lineID]
}

func (m *Manager) Lines() []*Line {
	m.mu.Lock()
	defer m.mu.Unlock()
	lines := make([]*Line, 0, len(m.lines))
	for _, line := range m.lines {
		lines = append(lines, line)
	}
	return lines
}

func (m *Manager) pollLine(ctx context.Context, line *Line) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		m.refreshLine(ctx, line)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) refreshLine(ctx context.Context, line *Line) {
	status, err := line.Client.Line(ctx)
	changed := false
	simTransitioned := false
	line.mu.Lock()
	if err == nil {
		previous := line.status
		simTransitioned = previous.SIM != "" && previous.SIM != status.SIM
		line.status = status
		line.statusAt = time.Now().UTC()
		if !line.reachable || previous.SIM != status.SIM || previous.Registration != status.Registration ||
			previous.Voice != status.Voice || previous.SMS != status.SMS || previous.ActiveCallID != status.ActiveCallID {
			changed = true
		}
		line.reachable = true
		line.lastError = ""
	} else if line.reachable || line.lastError == "" {
		line.reachable = false
		line.lastError = err.Error()
		changed = true
	}
	line.mu.Unlock()
	if err == nil {
		if value, known, dryErr := line.Client.SMSDryRun(ctx); dryErr == nil {
			line.mu.Lock()
			line.dryRun = value
			line.dryRunSupported = known
			line.mu.Unlock()
			if current, lineErr := m.Store.GetLine(ctx, line.Cfg.ID); lineErr == nil && current.SMSDryRun != value {
				_ = m.Store.UpdateLine(ctx, line.Cfg.ID, store.LineUpdate{SMSDryRun: &value})
			}
		}
		// A SIM status transition (insert/swap/ready) is the natural moment
		// to re-read own number + subscriber identity, bounded via the worker
		// queue. Runs independently so a slow AT read never delays status.
		if simTransitioned {
			go func() {
				probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
				defer cancel()
				if m.refreshSIMIdentity(probeCtx, line) {
					m.refreshLine(context.WithoutCancel(ctx), line)
				}
			}()
		}
	}
	if changed {
		identity := m.LineIdentity(line)
		line.mu.RLock()
		payload := map[string]any{
			"id": line.Cfg.ID, "name": line.Cfg.Name, "enabled": line.Cfg.Enabled,
			"online": line.reachable, "sim": line.status.SIM,
			"operator": resolvedOperator(
				line.status.Operator, identity.OperatorAlpha, identity.OperatorNumeric),
			"registration": line.status.Registration, "voice": line.status.Voice, "sms": line.status.SMS,
			"signal": line.status.Signal, "activeCallId": line.status.ActiveCallID,
			"lastError": line.lastError,
			"identity": map[string]any{
				"phoneMasked": identity.PhoneMasked, "numberSource": identity.NumberSource,
				"simChanged": identity.SIMChanged,
			},
		}
		line.mu.RUnlock()
		m.publish(line.Cfg.ID, "", "line.updated", payload)
	}
}

func hmacSHA1Base64(secret, message string) string {
	mac := hmac.New(sha1.New, []byte(secret))
	_, _ = mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// reconcileLine hangs up worker calls left active by a previous unified
// process. Media sessions cannot survive a restart, so an orphan would keep
// the SIM busy forever.
func (m *Manager) reconcileLine(ctx context.Context, line *Line) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}
	calls, err := line.Client.ListCalls(ctx, 50)
	if err != nil {
		slog.Warn("worker call reconciliation skipped", "line", line.Cfg.ID, "error", err)
		return
	}
	for _, record := range calls {
		if record.EndedAt != nil {
			continue
		}
		globalID := line.Cfg.ID + ":" + record.ID
		existing, getErr := m.Store.GetCall(ctx, globalID)
		if getErr == nil && existing.EndedAt == nil {
			continue
		}
		slog.Warn("hanging up orphaned worker call after restart", "line", line.Cfg.ID, "call_id", record.ID)
		if hangupErr := line.Client.Hangup(ctx, record.ID, randomKey()); hangupErr != nil {
			slog.Warn("orphan hangup failed", "line", line.Cfg.ID, "call_id", record.ID, "error", hangupErr)
		}
	}
}

func (m *Manager) consumeEvents(ctx context.Context, line *Line) {
	stream := line.Client.StreamEvents(ctx)
	for event := range stream {
		m.handleWorkerEvent(ctx, line, event)
	}
}

func (m *Manager) publish(lineID, deviceID, eventType string, data any) {
	if m.Bus == nil {
		return
	}
	if _, err := m.Bus.Publish(context.Background(), lineID, deviceID, eventType, data); err != nil {
		slog.Warn("publish event failed", "type", eventType, "error", err)
	}
}

// ---------- permissions ----------

func linePermission(access store.Access, lineID string) (store.LineAccess, bool) {
	for _, entry := range access.Lines {
		if entry.Line.ID == lineID {
			return entry, true
		}
	}
	return store.LineAccess{}, false
}

func (m *Manager) requireLine(ctx context.Context, deviceID, lineID string, check func(store.KeyLine) bool) (store.Access, store.LineAccess, *Line, error) {
	access, err := m.Store.DeviceAccess(ctx, deviceID)
	if err != nil {
		return store.Access{}, store.LineAccess{}, nil, fail(401, "CB-V2-401", "登录状态已失效")
	}
	entry, ok := linePermission(access, lineID)
	if !ok {
		return store.Access{}, store.LineAccess{}, nil, ErrForbidden
	}
	if check != nil && !check(entry.Permissions) {
		return store.Access{}, store.LineAccess{}, nil, ErrForbidden
	}
	line := m.Line(lineID)
	if line == nil || !line.Cfg.Enabled {
		return store.Access{}, store.LineAccess{}, nil, fail(410, CodeLineUnavailable, "线路未启用")
	}
	return access, entry, line, nil
}

// ---------- calls: queries ----------

type CallView struct {
	ID            string `json:"id"`
	LineID        string `json:"lineId"`
	LineName      string `json:"lineName"`
	Direction     string `json:"direction"`
	Peer          string `json:"peer"`
	State         string `json:"state"`
	Held          bool   `json:"held"`
	ConferenceID  string `json:"conferenceId,omitempty"`
	OwnerDeviceID string `json:"ownerDeviceId,omitempty"`
	StartedAt     int64  `json:"startedAt"`
	ConnectedAt   *int64 `json:"connectedAt,omitempty"`
	EndedAt       *int64 `json:"endedAt,omitempty"`
	EndReason     string `json:"endReason,omitempty"`
	// MediaTransport reports the session's current client audio transport
	// ("ice" or "ws"); omitted when nothing is attached.
	MediaTransport string `json:"mediaTransport,omitempty"`
	// Manual recording state for this call (latest segment). Present while a
	// segment is starting/recording/finalizing and after one finished.
	RecordingID         string `json:"recordingId,omitempty"`
	RecordingState      string `json:"recordingState,omitempty"`
	RecordingDurationMs *int64 `json:"recordingDurationMs,omitempty"`
}

// view renders a call for API consumers. It takes m.mu internally (via
// session) and must not be called with m.mu already held.
func (m *Manager) view(call store.Call) CallView {
	view := CallView{
		ID: call.ID, LineID: call.LineID, Direction: call.Direction, Peer: call.Peer,
		State: call.State, Held: call.Held, ConferenceID: call.ConferenceID,
		OwnerDeviceID: call.DeviceID, StartedAt: call.CreatedAt.UnixMilli(), EndReason: call.EndReason,
	}
	// Media truth: a merged call's audio is hosted by the conference
	// session, so report that session's transport instead of the absent
	// per-call one.
	mediaKey := call.ID
	if call.ConferenceID != "" {
		mediaKey = call.ConferenceID
	}
	if session := m.session(mediaKey); session != nil {
		view.MediaTransport = session.HostKind()
	}
	// Recording pointers come from the call row (no per-row query on list
	// paths); a live segment additionally reports its current duration from
	// the in-memory recording session.
	if call.RecordingID != "" {
		view.RecordingID = call.RecordingID
		view.RecordingState = call.RecordingState
		view.RecordingDurationMs = call.RecordingDurationMs
		if m.Recordings != nil && call.RecordingState == "recording" {
			if rec, ok := m.Recordings.CallRecording(context.Background(), call.ID); ok {
				view.RecordingState = rec.State
				if rec.DurationMs != nil {
					value := *rec.DurationMs
					view.RecordingDurationMs = &value
				}
			}
		}
	}
	if line := m.Line(call.LineID); line != nil {
		view.LineName = line.Cfg.Name
	}
	if call.AnsweredAt != nil {
		value := call.AnsweredAt.UnixMilli()
		view.ConnectedAt = &value
	}
	if call.EndedAt != nil {
		value := call.EndedAt.UnixMilli()
		view.EndedAt = &value
	}
	return view
}

func (m *Manager) GetCall(ctx context.Context, deviceID, callID string) (CallView, error) {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return CallView{}, ErrCallGone
	}
	access, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return CallView{}, err
	}
	if !callAllowed(entry, call, deviceID) {
		return CallView{}, ErrForbidden
	}
	// A key whose policy denies this inbound caller sees the call as gone;
	// another key on the same line is unaffected.
	if call.Direction == "inbound" && m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindCall, call.Peer, "") {
		return CallView{}, ErrCallGone
	}
	view := m.view(call)
	return m.redactRecordingForAccess(ctx, access, call, view), nil
}

// redactRecordingForAccess removes the recording pointer/state from a call
// view when the requesting device's enrollment key is not the recording's
// owner key. The call row is shared by line permissions, but recording
// metadata is key-scoped.
func (m *Manager) redactRecordingForAccess(ctx context.Context, access store.Access, call store.Call, view CallView) CallView {
	if view.RecordingID == "" || m.Recordings == nil {
		return view
	}
	rec, ok := m.Recordings.CallRecording(ctx, call.ID)
	if !ok || recordingKeyAllowed(access, rec) {
		return view
	}
	return redactedCallView(view)
}

func (m *Manager) ListCalls(ctx context.Context, deviceID, lineFilter string, activeOnly bool, limit int) ([]CallView, error) {
	access, err := m.Store.DeviceAccess(ctx, deviceID)
	if err != nil {
		return nil, fail(401, "CB-V2-401", "登录状态已失效")
	}
	allowed := map[string]bool{}
	for _, entry := range access.Lines {
		allowed[entry.Line.ID] = true
	}
	if lineFilter != "" && !allowed[lineFilter] {
		return nil, ErrForbidden
	}
	calls, err := m.Store.ListCalls(ctx, lineFilter, "", activeOnly, limit)
	if err != nil {
		return nil, err
	}
	views := make([]CallView, 0, len(calls))
	for _, call := range calls {
		if !allowed[call.LineID] {
			continue
		}
		entry, ok := linePermission(access, call.LineID)
		if !ok || !callAllowed(entry, call, deviceID) {
			continue
		}
		if call.Direction == "inbound" && m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindCall, call.Peer, "") {
			continue
		}
		views = append(views, m.redactRecordingForAccess(ctx, access, call, m.view(call)))
	}
	return views, nil
}

// ---------- worker events ----------

func (m *Manager) handleWorkerEvent(ctx context.Context, line *Line, event worker.Event) {
	switch {
	case event.Type == "line.updated":
		m.refreshLine(ctx, line)
	case strings.HasPrefix(event.Type, "message."):
		m.handleMessageEvent(ctx, line, event)
	case strings.HasPrefix(event.Type, "call."):
		m.handleCallEvent(ctx, line, event)
	case event.Type == "cellular_ready" || event.Type == "media_ready":
		// Readiness transitions carry the authoritative state snapshot:
		// cellular_ready (outbound ATD accepted) and media_ready (worker
		// audio bridge up, connectedAt set) are the only signals that move
		// an outbound call dialing -> connecting -> active. They must update
		// the unified record, not only ride the raw broadcast, otherwise the
		// call never shows answered and clients stay on "connecting".
		m.handleCallEvent(ctx, line, event)
	default:
		// recording.* and gateway.* stay line-scoped informational events.
		m.publish(line.Cfg.ID, "", event.Type, json.RawMessage(event.Data))
	}
}

func (m *Manager) handleMessageEvent(ctx context.Context, line *Line, event worker.Event) {
	var message worker.Message
	if json.Unmarshal(event.Data, &message) != nil || message.ID == "" {
		return
	}
	// Index the conversation for per-key read state (small upsert per live
	// event; never a history scan).
	_ = m.Store.RecordMessageThread(ctx, line.Cfg.ID, message.ID, message.ThreadKey, time.UnixMilli(message.CreatedAt).UTC())
	payload := m.messageView(line.Cfg.ID, message)
	m.publish(line.Cfg.ID, "", event.Type, payload)
	if event.Type == "message.created" && message.Direction == "inbound" {
		m.recordInboundHarassSMS(ctx, line.Cfg.ID, message)
		go m.pushMessage(line.Cfg.ID, message)
		go m.pushWebMessage(line.Cfg.ID, message)
		// Live carrier messages are classified for confirmed arrears/recharge
		// alerts. Only newly created events reach here, and the notifier
		// dedupes by message id, so no historical SMS is ever replayed.
		if m.Notify != nil {
			go func(lineID string, message worker.Message) {
				timeout, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				m.Notify.ObserveInboundSMS(timeout, lineID, message.ID, message.Peer, message.Body)
			}(line.Cfg.ID, message)
		}
	}
}

// recordInboundHarassSMS writes one block-history row per key whose policy
// denies or labels the sender. Delivery itself is suppressed at the event
// stream and push layers (per receiving key), so this only records history.
func (m *Manager) recordInboundHarassSMS(ctx context.Context, lineID string, message worker.Message) {
	keys, err := m.Store.LineReceiverKeys(ctx, lineID, false)
	if err != nil {
		return
	}
	for _, keyID := range keys {
		decision := m.HarassmentDecision(ctx, keyID, harass.KindSMS, message.Peer, message.Body)
		m.recordHarassEvent(ctx, keyID, lineID, harass.KindSMS, message.Peer, decision, lineID+":"+message.ID)
	}
}

type MessageView struct {
	ID        string `json:"id"`
	LineID    string `json:"lineId"`
	LineName  string `json:"lineName"`
	ThreadKey string `json:"threadKey"`
	Direction string `json:"direction"`
	Peer      string `json:"peer"`
	Body      string `json:"body"`
	Encoding  string `json:"encoding"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
}

func (m *Manager) messageView(lineID string, message worker.Message) MessageView {
	_ = m.Store.RecordMessageThread(context.Background(), lineID, message.ID, message.ThreadKey, time.UnixMilli(message.CreatedAt))
	view := MessageView{
		ID: lineID + ":" + message.ID, LineID: lineID, ThreadKey: lineID + ":" + message.ThreadKey,
		Direction: message.Direction, Peer: message.Peer, Body: message.Body,
		Encoding: message.Encoding, Status: message.Status, CreatedAt: message.CreatedAt,
	}
	if line := m.Line(lineID); line != nil {
		view.LineName = line.Cfg.Name
	}
	return view
}

func (m *Manager) handleCallEvent(ctx context.Context, line *Line, event worker.Event) {
	var record worker.CallRecord
	if json.Unmarshal(event.Data, &record) != nil || record.ID == "" {
		return
	}
	globalID := line.Cfg.ID + ":" + record.ID
	existing, err := m.Store.GetCall(ctx, globalID)
	found := err == nil

	if !found {
		call := store.Call{
			ID: globalID, LineID: line.Cfg.ID, WorkerCallID: record.ID,
			Direction: normalizeDirection(record.Direction), Peer: record.Peer,
			State: mapWorkerState(record.State), CreatedAt: time.Now().UTC(),
		}
		if record.StartedAt > 0 {
			call.CreatedAt = time.UnixMilli(record.StartedAt).UTC()
		}
		if record.ConnectedAt != nil {
			value := time.UnixMilli(*record.ConnectedAt).UTC()
			call.AnsweredAt = &value
		}
		if call.Direction == "inbound" && call.State == StateRinging {
			if m.rejectInboundByPolicy(ctx, line, call) {
				return
			}
			// Per-enrollment-key harassment evaluation. Keys that deny this
			// caller receive no event/ring/push; a call is rejected upstream
			// ONLY when every normally authorized receiving key denied it
			// (merely-offline devices never count as denial, and a different
			// key on the same line may still answer).
			allowed, suppressed := m.harassmentCallReceivers(ctx, call.LineID, call.Peer, call.ID)
			if len(allowed) == 0 && len(suppressed) > 0 {
				// All-denied: direct reject, no voicemail.
				call.State = StateEnded
				now := time.Now().UTC()
				call.EndedAt = &now
				call.EndReason = "harassment_reject"
				_ = m.Store.UpsertCall(ctx, call)
				go func(workerCallID string) {
					if err := line.Client.Reject(context.Background(), workerCallID, randomKey()); err != nil {
						slog.Warn("harassment reject failed", "line", line.Cfg.ID, "call_id", call.ID, "error", err)
					}
				}(call.WorkerCallID)
				return
			}
			_ = m.Store.UpsertCall(ctx, call)
			m.publish(call.LineID, "", "call.incoming", m.view(call))
			go m.pushIncomingCall(ctx, call)
			go m.pushBarkIncoming(ctx, call)
			go m.pushWebIncoming(ctx, call)
			// Voicemail is line-wide; a suppressed key must never see its
			// events/rows (gated in the API/runtime). Arm it only when at
			// least one allowed key exists, or when no key receives the line
			// at all (legacy behavior).
			if len(allowed) > 0 || len(suppressed) == 0 {
				m.scheduleVoicemail(ctx, call)
			}
			return
		}
		if record.EndedAt != nil {
			endedAt := time.UnixMilli(*record.EndedAt).UTC()
			call.EndedAt = &endedAt
			call.State = StateEnded
		}
		_ = m.Store.UpsertCall(ctx, call)
		m.publish(call.LineID, "", "call.updated", redactedCallView(m.view(call)))
		return
	}

	// Do not let stale worker snapshots overwrite terminal unified state.
	if existing.EndedAt != nil {
		return
	}
	existing.Peer = record.Peer
	if record.ConnectedAt != nil && existing.AnsweredAt == nil {
		value := time.UnixMilli(*record.ConnectedAt).UTC()
		existing.AnsweredAt = &value
	}
	mapped := mapWorkerState(record.State)
	if mapped == StateEnded || record.EndedAt != nil {
		endReason := existing.EndReason
		if endReason == "" {
			endReason = record.EndReason
		}
		endedAt := time.Now().UTC()
		if record.EndedAt != nil {
			endedAt = time.UnixMilli(*record.EndedAt).UTC()
		}
		// Atomic terminal claim: the first finisher (worker event path,
		// HTTP hangup/reject, policy reject) owns the single call.ended.
		// connectedAt may first arrive in this same terminal snapshot;
		// the claim records it monotonically so terminal truth keeps
		// answered_at even when no live update preceded it.
		won, claimErr := m.Store.FinishCallIfOpen(ctx, globalID, endedAt, endReason, existing.AnsweredAt)
		m.cancelVoicemailTimer(existing.ID)
		if claimErr != nil {
			slog.Error("terminal claim failed", "call_id", globalID, "error", claimErr)
			m.endCallMedia(existing.ID)
			m.afterLegEnded(ctx, existing)
			return
		}
		if won {
			// Re-read for the terminal view so connectedAt/endReason
			// reflect exactly what the atomic claim persisted.
			if current, getErr := m.Store.GetCall(ctx, globalID); getErr == nil {
				existing = current
			} else {
				existing.State = StateEnded
				existing.EndedAt = &endedAt
				if existing.EndReason == "" {
					existing.EndReason = endReason
				}
			}
			m.publish(existing.LineID, "", "call.ended", m.view(existing))
		}
		m.endCallMedia(existing.ID)
		m.afterLegEnded(ctx, existing)
		return
	}
	// A device-local answering claim must not be overwritten while the
	// worker still reports the call as ringing, and voicemail ownership is
	// never replaced by worker state snapshots. Any OTHER worker snapshot
	// (active/ended/...) must converge while answering: the worker only
	// reports active after accepting the answer, and this handler's
	// full-row UpsertCall would otherwise rewrite the Answer's own
	// connecting transition back to answering and strand the call.
	if strings.HasPrefix(existing.State, "voicemail") {
		// keep voicemail ownership; only the terminal branch above ends it
	} else if existing.State != StateAnswering || mapped != StateRinging {
		existing.State = mapped
	}
	if mapped == StateHeld {
		existing.Held = true
	}
	// Conditional write: between the GetCall above and this write another
	// finisher (HTTP hangup, policy reject, another worker event) may have
	// claimed the terminal transition. A full-row upsert of this stale
	// snapshot would then resurrect the row (state back to active,
	// ended_at/end_reason cleared, answered_at lost). UpsertLiveCall only
	// matches a still-live row; on a lost race re-read for the terminal
	// view and converge media/idempotently without publishing anything.
	if err := m.Store.UpsertLiveCall(ctx, existing); err != nil {
		if !errors.Is(err, store.ErrCallAlreadyEnded) {
			return
		}
		if current, getErr := m.Store.GetCall(ctx, globalID); getErr == nil {
			m.endCallMedia(current.ID)
			m.afterLegEnded(ctx, current)
		}
		return
	}
	m.publish(existing.LineID, "", "call.updated", redactedCallView(m.view(existing)))
	// Converging to answered/active on a live media session is the other
	// auto-recording moment (media may attach before or after this event).
	if existing.State == StateActive || existing.State == StateConnecting {
		go m.maybeAutoRecord(context.Background(), existing)
	}
}

func normalizeDirection(value string) string {
	if strings.EqualFold(value, "inbound") {
		return "inbound"
	}
	return "outbound"
}

func mapWorkerState(state string) string {
	switch state {
	case "incoming_ringing":
		return StateRinging
	case "outgoing_dialing":
		return StateDialing
	case "connecting":
		return StateConnecting
	case "active":
		return StateActive
	case "ending":
		return StateEnding
	case "recovering":
		return StateConnecting
	case "ended", "idle":
		return StateEnded
	default:
		return StateDialing
	}
}

// ---------- inbound policy ----------

func (m *Manager) rejectInboundByPolicy(ctx context.Context, line *Line, call store.Call) bool {
	stored, err := m.Store.GetLine(ctx, line.Cfg.ID)
	if err != nil {
		return false
	}
	reject := false
	reason := ""
	if !stored.AllowInbound {
		reject, reason = true, "line_inbound_disabled"
	}
	if !reject && strings.EqualFold(stored.RejectPolicy, "reject_all") {
		reject, reason = true, "line_reject_all"
	}
	if !reject && strings.EqualFold(stored.RejectPolicy, "reject_blocked") && matchesBlocked(stored.BlockedPrefixes, call.Peer) {
		reject, reason = true, "line_blocked_prefix"
	}
	if !reject {
		return false
	}
	call.State = StateEnded
	now := time.Now().UTC()
	call.EndedAt = &now
	call.EndReason = reason
	_ = m.Store.UpsertCall(ctx, call)
	go func() {
		if err := line.Client.Reject(context.Background(), call.WorkerCallID, randomKey()); err != nil {
			slog.Warn("policy reject failed", "line", line.Cfg.ID, "call_id", call.ID, "error", err)
		}
	}()
	m.publish(call.LineID, "", "call.ended", m.view(call))
	return true
}

func matchesBlocked(prefixes, peer string) bool {
	peer = strings.TrimSpace(peer)
	if peer == "" {
		return false
	}
	for _, prefix := range strings.Split(prefixes, ",") {
		value := strings.TrimSpace(prefix)
		if value != "" && strings.HasPrefix(peer, value) {
			return true
		}
	}
	return false
}

// ---------- push ----------

func (m *Manager) pushIncomingCall(ctx context.Context, call store.Call) {
	if m.Push == nil {
		slog.Warn("push skipped", "kind", "voip", "reason", "no APNs broker configured", "error_code", "BLOCKED_EXTERNAL_APNS", "line", call.LineID)
		return
	}
	targets, err := m.Store.ListPushTargets(ctx)
	if err != nil {
		slog.Warn("push skipped", "kind", "voip", "reason", "list targets failed", "line", call.LineID, "error", err)
		return
	}
	eligible := targets[:0]
	for _, target := range targets {
		// Every skip is logged with its reason: an aggregate "no eligible
		// targets" line alone cannot distinguish a revoked device, a
		// missing line grant and a deliberate web-only mode when
		// correlating a field "never rang" report (2026-10-05 review).
		access, accessErr := m.Store.DeviceAccess(ctx, target.DeviceID)
		if accessErr != nil {
			slog.Info("push target skipped", "kind", "voip", "device_id", target.DeviceID,
				"line", call.LineID, "reason", "device access unavailable")
			continue
		}
		entry, ok := linePermission(access, call.LineID)
		if !ok {
			slog.Info("push target skipped", "kind", "voip", "device_id", target.DeviceID,
				"line", call.LineID, "reason", "no line grant")
			continue
		}
		if !entry.Permissions.ReceiveCalls {
			slog.Info("push target skipped", "kind", "voip", "device_id", target.DeviceID,
				"line", call.LineID, "reason", "receive_calls not granted")
			continue
		}
		if m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindCall, call.Peer, "") {
			slog.Info("push target skipped", "kind", "voip", "device_id", target.DeviceID,
				"line", call.LineID, "reason", "harassment policy")
			continue
		}
		// Devices that deliberately selected the web-only notification
		// mode do not receive a native ring: one selected mode, no
		// duplicate channels.
		mode, modeErr := m.Store.DeviceNotifyMode(ctx, target.DeviceID)
		if modeErr == nil && mode == store.NotifyModeWeb {
			slog.Info("push target skipped", "kind", "voip", "device_id", target.DeviceID,
				"line", call.LineID, "reason", "notify mode web")
			continue
		}
		if target.Environment != "sandbox" && target.Environment != "production" {
			slog.Warn("push target skipped", "kind", "voip", "device_id", target.DeviceID,
				"line", call.LineID, "reason", "push environment invalid", "environment", target.Environment)
			continue
		}
		if target.VoIPToken == "" {
			slog.Warn("push target skipped", "kind", "voip", "device_id", target.DeviceID,
				"line", call.LineID, "reason", "voip token missing")
			continue
		}
		eligible = append(eligible, target)
	}
	// Dispatch evidence: without an accepted/failed record per ring, a
	// background "never rang" report cannot be correlated with APNs
	// delivery (2026-10-03 build 14 feedback). Contents stay out: device
	// id, environment and gateway call id only.
	if len(eligible) == 0 {
		slog.Info("push dispatch empty", "kind", "voip", "call_id", call.ID, "line", call.LineID, "reason", "no eligible push targets")
		return
	}
	slog.Info("push dispatch started", "kind", "voip", "call_id", call.ID, "line", call.LineID, "target_count", len(eligible))
	for _, target := range eligible {
		target := target
		go func() {
			timeout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := m.Push.SendVoIP(timeout, push.Target{
				DeviceID: target.DeviceID, APNSToken: target.APNSToken, VoIPToken: target.VoIPToken,
				Environment: target.Environment, Locale: target.Locale,
			}, push.VoIPPayload{
				CallUUID: call.ID, CallID: call.ID, Handle: call.Peer, GatewayID: m.GatewayID,
				LineID: call.LineID, IssuedAt: time.Now().Unix(),
			})
			if err != nil {
				slog.Warn("voip push failed", "device_id", target.DeviceID, "environment", target.Environment, "line", call.LineID, "call_id", call.ID, "error", err)
			} else {
				slog.Info("voip push accepted", "device_id", target.DeviceID, "environment", target.Environment, "line", call.LineID, "call_id", call.ID)
			}
		}()
	}
}

// barkCallStillFresh reports whether a ringing call is still worth an
// incoming notification. A call that already ended, moved out of ringing, or
// has been ringing longer than maxAge (e.g. a replayed event after a gateway
// reconnect) is suppressed as stale.
func barkCallStillFresh(call store.Call, now time.Time, maxAge time.Duration) bool {
	if call.State != StateRinging || call.EndedAt != nil {
		return false
	}
	if maxAge > 0 && !call.CreatedAt.IsZero() && now.Sub(call.CreatedAt) > maxAge {
		return false
	}
	return true
}

// pushBarkIncoming fans an incoming call out over the OPTIONAL Bark path.
// It is independent of m.Push: with no APNs provider configured, Bark still
// works, and with Bark off the native path is untouched. Each (device, call)
// notifies at most once; the stored call is re-read immediately before send
// so an ended/moved call is never announced late.
func (m *Manager) pushBarkIncoming(ctx context.Context, call store.Call) {
	if m.Bark == nil || !m.Cfg.Bark.Enabled {
		return
	}
	targets, err := m.Store.ListBarkTargets(ctx)
	if err != nil {
		return
	}
	maxAge, err := m.Cfg.BarkMaxCallAge()
	if err != nil {
		maxAge = 60 * time.Second
	}
	for _, target := range targets {
		access, accessErr := m.Store.DeviceAccess(ctx, target.DeviceID)
		if accessErr != nil {
			continue
		}
		entry, ok := linePermission(access, call.LineID)
		if !ok || !entry.Permissions.ReceiveCalls {
			continue
		}
		if m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindCall, call.Peer, "") {
			continue
		}
		target := target
		if !m.barkDedup.Reserve(target.DeviceID, call.ID) {
			continue
		}
		go func() {
			timeout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			fresh, lookupErr := m.Store.GetCall(context.Background(), call.ID)
			if lookupErr != nil || !barkCallStillFresh(fresh, time.Now().UTC(), maxAge) {
				return
			}
			title, body := push.BarkCallText(target.Locale, fresh.Peer)
			if err := m.Bark.Send(timeout, push.BarkTarget{
				DeviceID: target.DeviceID, ServerURL: target.ServerURL,
				DeviceKey: target.DeviceKey, Locale: target.Locale,
			}, push.BarkNotification{
				Title: title, Body: body,
				URL: push.BarkDeepLinkURL, Group: "CallRelay", Level: "timeSensitive",
			}); err != nil {
				// Error text is already sanitized (no URL/key/body) by the
				// client; log only the device and line.
				slog.Warn("bark notification failed", "device_id", target.DeviceID, "line", call.LineID, "error", err)
			}
		}()
	}
}

// pushWebIncoming fans an incoming call out over the self-hosted PWA Web
// Push path. Devices are filtered by notify mode (native/both/web): the
// default native mode never receives a web ring, so an un-configured device
// is never surprised by a duplicate channel. Each (subscription, call)
// notifies at most once; the stored call is re-read immediately before send
// so an ended/moved call is never announced late. The payload carries only
// the caller handle plus a single-use, short-lived handoff token in the
// navigate URL fragment — never call tokens, credentials or message content.
func (m *Manager) pushWebIncoming(ctx context.Context, call store.Call) {
	if m.WebPush == nil || m.WebPushSender == nil || !m.Cfg.WebPush.Enabled {
		return
	}
	targets, err := m.Store.ListWebPushTargets(ctx)
	if err != nil {
		return
	}
	maxAge, err := m.Cfg.BarkMaxCallAge()
	if err != nil {
		maxAge = 60 * time.Second
	}
	base := strings.TrimRight(strings.TrimSpace(m.Cfg.Gateway.PublicBaseURL), "/")
	if base == "" {
		base = "https://" + strings.TrimSpace(m.GatewayID)
	}
	for _, target := range targets {
		access, accessErr := m.Store.DeviceAccess(ctx, target.DeviceID)
		if accessErr != nil {
			continue
		}
		entry, ok := linePermission(access, call.LineID)
		if !ok || !entry.Permissions.ReceiveCalls {
			continue
		}
		if m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindCall, call.Peer, "") {
			continue
		}
		mode, modeErr := m.Store.DeviceNotifyMode(ctx, target.DeviceID)
		if modeErr == nil && mode == store.NotifyModeNative {
			continue
		}
		target := target
		if !m.webpushDedup.Reserve(target.DeviceID+":"+target.SubID, call.ID) {
			continue
		}
		go func() {
			timeout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			fresh, lookupErr := m.Store.GetCall(context.Background(), call.ID)
			if lookupErr != nil || !barkCallStillFresh(fresh, time.Now().UTC(), maxAge) {
				return
			}
			// The handoff token rides in the URL fragment: it never appears
			// in server or proxy access logs, so bots and link previews
			// cannot consume it before the owner taps the notification.
			handoff := m.WebPush.MintHandoff(target.DeviceID, fresh.ID)
			navigate := base + "/pwa/call#h=" + handoff
			payload, payloadErr := webpush.CallPayload(target.Locale, fresh.Peer, navigate, fresh.ID)
			if payloadErr != nil {
				return
			}
			if err := m.WebPushSender.Send(timeout, webpush.Subscription{
				ID: target.SubID, Endpoint: target.Endpoint, Auth: target.Auth, P256dh: target.P256dh,
			}, payload); err != nil {
				if webpush.SubscriptionGone(err) {
					_ = m.Store.DeleteWebPushSubscription(context.Background(), target.DeviceID, target.SubID)
				}
				slog.Warn("webpush notification failed", "device_id", target.DeviceID, "line", call.LineID, "error", err)
			}
		}()
	}
}

// pushWebMessage fans one inbound SMS out over the self-hosted PWA Web Push
// path. Native SMS alerts require APNs (or a push broker); a self-hosted
// deployment has neither, so without this path an SMS only reaches a phone
// while the PWA is in the foreground. The channel choice mirrors calls: a
// device that selected native-only never gets a duplicate web alert, while
// both/web do. Sender and a bounded preview ride along exactly as in the APNs
// payload, gated by the same ReceiveSMS grant and harassment policy.
func (m *Manager) pushWebMessage(lineID string, message worker.Message) {
	if m.WebPush == nil || m.WebPushSender == nil || !m.Cfg.WebPush.Enabled {
		return
	}
	targets, err := m.Store.ListWebPushTargets(context.Background())
	if err != nil {
		return
	}
	base := strings.TrimRight(strings.TrimSpace(m.Cfg.Gateway.PublicBaseURL), "/")
	if base == "" {
		base = "https://" + strings.TrimSpace(m.GatewayID)
	}
	peer := push.TruncatePreview(message.Peer, push.PreviewPeerRunes)
	preview := push.TruncatePreview(message.Body, push.PreviewBodyRunes)
	messageID := lineID + ":" + message.ID
	for _, target := range targets {
		access, accessErr := m.Store.DeviceAccess(context.Background(), target.DeviceID)
		if accessErr != nil {
			continue
		}
		entry, ok := linePermission(access, lineID)
		if !ok || !entry.Permissions.ReceiveSMS {
			continue
		}
		if m.HarassSuppressedForKey(context.Background(), access.Key.ID, harass.KindSMS, message.Peer, message.Body) {
			continue
		}
		mode, modeErr := m.Store.DeviceNotifyMode(context.Background(), target.DeviceID)
		if modeErr == nil && mode == store.NotifyModeNative {
			continue
		}
		target := target
		if !m.webpushDedup.Reserve(target.DeviceID+":"+target.SubID, "sms:"+messageID) {
			continue
		}
		go func() {
			timeout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			payload, payloadErr := webpush.SmsPayload(target.Locale, peer, preview, base+"/pwa/client.html", messageID)
			if payloadErr != nil {
				return
			}
			if err := m.WebPushSender.Send(timeout, webpush.Subscription{
				ID: target.SubID, Endpoint: target.Endpoint, Auth: target.Auth, P256dh: target.P256dh,
			}, payload); err != nil {
				if webpush.SubscriptionGone(err) {
					_ = m.Store.DeleteWebPushSubscription(context.Background(), target.DeviceID, target.SubID)
				}
				slog.Warn("webpush notification failed", "kind", "sms", "device_id", target.DeviceID, "line", lineID, "error", err)
			}
		}()
	}
}

func (m *Manager) pushMessage(lineID string, message worker.Message) {
	if m.Push == nil {
		return
	}
	targets, err := m.Store.ListPushTargets(context.Background())
	if err != nil {
		return
	}
	for _, target := range targets {
		access, accessErr := m.Store.DeviceAccess(context.Background(), target.DeviceID)
		if accessErr != nil {
			continue
		}
		entry, ok := linePermission(access, lineID)
		if !ok || !entry.Permissions.ReceiveSMS {
			continue
		}
		if m.HarassSuppressedForKey(context.Background(), access.Key.ID, harass.KindSMS, message.Peer, message.Body) {
			continue
		}
		// One notification per (device, message id): a replayed worker event
		// for the same message may not re-alert the owner.
		if !m.smsPushDedup.Reserve(target.DeviceID, "sms:"+lineID+":"+message.ID) {
			continue
		}
		target := target
		go func() {
			timeout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// Owner-authorized 2026-10-21: sender + bounded body preview for
			// the devices that may actually read this conversation (grant +
			// harassment gates above). The preview is truncated HERE so no
			// sender implementation can forget the bound; it is never logged.
			_ = m.Push.SendMessage(timeout, push.Target{
				DeviceID: target.DeviceID, APNSToken: target.APNSToken, VoIPToken: target.VoIPToken,
				Environment: target.Environment, Locale: target.Locale,
			}, push.MessagePayload{
				MessageID: lineID + ":" + message.ID, GatewayID: m.GatewayID, LineID: lineID,
				ThreadKey: lineID + ":" + message.ThreadKey,
				Peer:      message.Peer,
				Preview:   push.TruncatePreview(message.Body, push.PreviewBodyRunes),
				IssuedAt:  time.Now().Unix(),
			})
		}()
	}
}

// ---------- helpers ----------

func randomKey() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return "idem_" + base64.RawURLEncoding.EncodeToString(raw)
}

func randomID(prefix string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return prefix + base64.RawURLEncoding.EncodeToString(raw)
}

func (m *Manager) session(key string) *media.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[key]
}

func (m *Manager) setSession(key string, session *media.Session) {
	m.mu.Lock()
	if m.closed && session != nil {
		m.mu.Unlock()
		session.Close()
		return
	}
	if existing := m.sessions[key]; existing != nil && existing != session {
		m.mu.Unlock()
		existing.Close()
		m.mu.Lock()
	}
	m.sessions[key] = session
	recordings := m.Recordings
	m.mu.Unlock()
	// Manual recording taps belong to live media sessions: a 1:1 call session
	// taps its canonical uplink + its own downlink leg, a conference session
	// taps the same canonical uplink + the all-legs client mix (see mixOnce).
	// The callbacks are installed on the session object, so they survive
	// every host handover (ICE<->WS, hot-standby promotion, probe adoption)
	// that keeps the same session: one logical uplink, one downlink owner.
	if session != nil && recordings != nil && (session.Kind == "call" || session.Kind == "conference") {
		callID := session.Key
		session.SetRecordingTaps(
			func(pcm []int16) { recordings.WriteUplink(callID, pcm) },
			func(pcm []int16) { recordings.WriteDownlink(callID, pcm) },
		)
	}
	// A media session appearing is the moment auto recording can safely
	// begin (the owning device's client is actually attached). The helpers
	// re-check ownership, answered state and both opt-in switches.
	if session != nil && recordings != nil {
		switch session.Kind {
		case "call":
			if call, err := m.Store.GetCall(context.Background(), session.Key); err == nil {
				go m.maybeAutoRecord(context.Background(), call)
			}
		case "conference":
			if conference, err := m.Store.GetConference(context.Background(), session.Key); err == nil {
				go m.maybeAutoRecordConference(context.Background(), conference)
			}
		}
	}
}

func (m *Manager) dropSession(key string, closeSession bool) {
	m.mu.Lock()
	session := m.sessions[key]
	delete(m.sessions, key)
	m.mu.Unlock()
	if session != nil && closeSession {
		session.Close()
	}
}

func (m *Manager) endCallMedia(callID string) {
	// The conversation is over: a manual auto-record suppression does not
	// outlive it (a genuinely new dial/answer is never suppressed).
	m.autoRecordFence.clear(callID)
	// Finalize an active manual recording before the session (and its taps)
	// goes away. Normal hangup keeps the captured audio; failures inside the
	// recording layer never propagate into call teardown.
	m.stopRecording(callID, "call_ended")
	if session := m.session(callID); session != nil {
		m.dropSession(callID, true)
	}
}

// DisconnectGrace resolves the host-disconnect grace period, letting the
// console override the YAML default.
func (m *Manager) DisconnectGrace(ctx context.Context) time.Duration {
	if value, err := m.Store.Setting(ctx, "disconnect_grace_seconds"); err == nil {
		if parsed, parseErr := strconv.Atoi(strings.TrimSpace(value)); parseErr == nil && parsed >= 5 && parsed <= 600 {
			return time.Duration(parsed) * time.Second
		}
	}
	return m.Cfg.DisconnectGrace()
}

// SetLineSMSDryRun flips the worker's runtime SMS submission mode and
// persists it so the console reflects the real state.
func (m *Manager) SetLineSMSDryRun(ctx context.Context, lineID string, dryRun bool) error {
	line := m.Line(lineID)
	if line == nil {
		return fail(410, CodeLineUnavailable, "线路未启用")
	}
	if err := line.Client.SetSMSDryRun(ctx, dryRun); err != nil {
		return fail(502, "CB-V2-502", "切换短信模式失败: "+err.Error())
	}
	if err := m.Store.UpdateLine(ctx, lineID, store.LineUpdate{SMSDryRun: &dryRun}); err != nil {
		return err
	}
	line.mu.Lock()
	line.dryRun = dryRun
	line.dryRunSupported = true
	line.mu.Unlock()
	m.publish(lineID, "", "line.updated", map[string]any{"id": lineID, "smsLive": !dryRun})
	return nil
}

// RecoverLine re-probes the worker voice backend; when that still fails and
// the line has no live call, the worker service is restarted. Restarting
// during an active call is refused so media is never cut by an operator
// mistake.
func (m *Manager) RecoverLine(ctx context.Context, lineID string) error {
	line := m.Line(lineID)
	if line == nil {
		return fail(410, CodeLineUnavailable, "线路未启用")
	}
	probeErr := line.Client.Recover(ctx)
	m.refreshLine(ctx, line)
	if probeErr == nil {
		return nil
	}
	live, countErr := m.Store.CountLiveCallsOnLine(ctx, lineID)
	if countErr != nil {
		return countErr
	}
	if live > 0 {
		return fail(409, CodeLineBusy, "线路正在通话，不能重启 worker")
	}
	unit := "callrelay-worker@" + strings.TrimPrefix(lineID, "line")
	restartCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := exec.CommandContext(restartCtx, "systemctl", "restart", unit).CombinedOutput(); err != nil {
		return fail(502, "CB-V2-502", "worker 重启失败")
	}
	return nil
}

// lineIdentity builds the console/client-safe identity for a line: module
// fields come from the preparation script's file; number/source/operator come
// from the daemon's private identity row and are exposed only masked.
func (m *Manager) LineIdentity(line *Line) LineIdentity {
	identity := LineIdentity{LineID: line.Cfg.ID}
	if payload, err := os.ReadFile(line.Cfg.IdentityFile); err == nil {
		_ = json.Unmarshal(payload, &identity)
	}
	identity.LineID = line.Cfg.ID

	row := m.lineIdentitySnapshot(line)
	_, masked, source := resolvedNumber(row)
	// Always replace (never inherit) the preparation file's phoneMasked: an
	// empty/unsupported/changed SIM must not keep a previous card's number.
	identity.PhoneMasked = masked
	identity.NumberSource = source
	identity.SIMChanged = source == numberSourceSIMChanged
	line.mu.RLock()
	identity.SIMChanged = identity.SIMChanged || line.simPending
	line.mu.RUnlock()
	if row.ICCIDMasked != "" {
		identity.SIMMasked = row.ICCIDMasked
	}
	identity.OperatorAlpha = row.OperatorAlpha
	identity.OperatorNumeric = row.OperatorNumeric
	identity.Registration = friendlyRegistration(row.Registration)
	identity.AccessTech = row.AccessTech
	if !row.UpdatedAt.IsZero() {
		identity.UpdatedAt = row.UpdatedAt.UnixMilli()
	}
	return identity
}

// lineFullNumber returns the FULL effective own number (manual override or
// SIM-derived) for authenticated, line-authorized surfaces only. Callers must
// never place it in anonymous endpoints, free/broadcast events, logs or
// reports. ICCID/IMSI/cell identifiers stay masked even here.
func (m *Manager) lineFullNumber(line *Line) string {
	row := m.lineIdentitySnapshot(line)
	full, _, _ := resolvedNumber(row)
	return full
}

// cleanOperatorName rejects empty and non-textual operator tokens (some
// firmware answers "????" for the alpha name), so the API never shows a fake
// operator. Callers fall back to the MCC+MNC numeric identity instead.
func cleanOperatorName(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	for _, r := range trimmed {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return trimmed
		}
	}
	return ""
}

// resolvedOperator prefers the live AT status, then the stored network name,
// then the numeric MCC+MNC. It never invents a name.
func resolvedOperator(statusOperator, alpha, numeric string) string {
	if value := cleanOperatorName(statusOperator); value != "" {
		return value
	}
	if value := cleanOperatorName(alpha); value != "" {
		return value
	}
	return strings.TrimSpace(numeric)
}

// friendlyRegistration translates the AT registration token to a short,
// user-facing status; callers get no cell/LAC/TAC precision here.
func friendlyRegistration(value string) string {
	switch value {
	case "registered_home":
		return "registered"
	case "registered_roaming":
		return "roaming"
	case "searching":
		return "searching"
	case "denied":
		return "denied"
	case "not_registered":
		return "not_registered"
	default:
		return ""
	}
}

// ICEConfiguration returns the configured ICE servers for one client offer.
// STUN entries (P2P address discovery) are always included when configured;
// TURN relay entries are added when enabled. The policy label reports the
// external-candidate capability: "relay" (TURN on), "stun" (STUN only) or
// "host" (none — LAN host candidates alone). No new ports/services are
// opened by this configuration; a STUN miss merely yields no server-reflexive
// candidate while host candidates and the HTTPS/WSS fallback stay available.
func (m *Manager) ICEConfiguration() ([]pion.ICEServer, string) {
	endpoints, malformed := m.Cfg.STUNEndpoints()
	servers := make([]pion.ICEServer, 0, len(endpoints)+1)
	for _, endpoint := range endpoints {
		servers = append(servers, pion.ICEServer{URLs: []string{"stun:" + endpoint}})
	}
	policy := "host"
	if len(endpoints) > 0 {
		policy = "stun"
	}
	for _, entry := range malformed {
		slog.Warn("ignoring invalid STUN endpoint; host candidates and WSS fallback retained",
			"entry", entry)
	}
	if !m.Cfg.TURN.Enabled {
		if len(servers) == 0 {
			return nil, policy
		}
		return servers, policy
	}
	secret := os.Getenv(m.Cfg.TURN.SecretEnv)
	if secret == "" {
		if len(servers) == 0 {
			return nil, policy
		}
		return servers, policy
	}
	ttl, err := m.Cfg.TURNCredentialDuration()
	if err != nil {
		if len(servers) == 0 {
			return nil, policy
		}
		return servers, policy
	}
	expiresAt := time.Now().UTC().Add(ttl)
	username := fmt.Sprintf("%d:%s", expiresAt.Unix(), "callrelay")
	credential := turnCredential(secret, username)
	port := m.Cfg.TURN.Port
	if port == 0 {
		port = 3478
	}
	host := strings.TrimSpace(m.Cfg.TURN.Host)
	servers = append(servers, pion.ICEServer{URLs: []string{
		fmt.Sprintf("turn:%s:%d?transport=udp", host, port),
		fmt.Sprintf("turn:%s:%d?transport=tcp", host, port),
	}, Username: username, Credential: credential})
	return servers, "relay"
}
