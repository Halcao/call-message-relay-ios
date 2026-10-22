package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"callrelay/gateway/internal/id"
	"callrelay/gateway/internal/push"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/webpush"
)

// Availability values persisted for the designated backup network.
const (
	AvailabilityUnknown     = "unknown"
	AvailabilityAvailable   = "available"
	AvailabilityUnavailable = "unavailable"
)

// ProbeResult is the outcome of one isolated backup-egress probe.
type ProbeResult string

const (
	ProbeOK      ProbeResult = "ok"
	ProbeFail    ProbeResult = "fail"
	ProbeUnknown ProbeResult = "unknown" // busy / setup failure / identity fail-closed / exec error
)

// Config controls the notifier. Durations are already parsed from YAML.
type Config struct {
	Enabled                 bool
	GatewayID               string
	WanLineID               string
	WanStateFile            string
	WanProbeCommand         string
	WanProbeInterval        time.Duration
	WanFailureProbeInterval time.Duration
	WanProbeTimeout         time.Duration
	SustainedFailures       int
	RecoverySuccesses       int
	CarrierSenders          []string
	MaxAlertAttempts        int
}

// Incident is one open per-line fault, persisted so dedupe survives restarts.
// Incidents are keyed by line: an arrears on one line can never be closed by
// another line's recovery, and the WAN probe only speaks for the designated
// line.
type Incident struct {
	LineID           string    `json:"lineId"`
	ID               string    `json:"id"`
	Cause            string    `json:"cause"` // network | arrears
	OpenedAt         time.Time `json:"openedAt"`
	FaultNotified    bool      `json:"faultNotified"`
	ArrearsNotified  bool      `json:"arrearsNotified"`
	RecoveryNotified bool      `json:"recoveryNotified"`
}

type monitorState struct {
	Availability         string               `json:"availability"`
	AvailabilitySince    time.Time            `json:"availabilitySince,omitempty"`
	ConsecutiveFailures  int                  `json:"failures"`
	ConsecutiveSuccesses int                  `json:"successes"`
	LastProbeAt          time.Time            `json:"lastProbeAt,omitempty"`
	LastProbeResult      string               `json:"lastProbeResult,omitempty"`
	LastAgentState       string               `json:"lastAgentState,omitempty"`
	Incidents            map[string]*Incident `json:"incidents,omitempty"`
}

const monitorStateKey = "notify.monitor"

// Monitor observes the designated backup network and inbound carrier SMS and
// fans confirmed events out as ordinary alerts under the line's ACL. Every
// decision that leads to a notification first reserves a durable alert row
// keyed by (line, incident, phase), so one fault produces one alert per
// incident even across restarts and repeated carrier messages.
//
// Locking: the state mutex only guards in-memory state and local SQLite
// reservations. Network calls (probe, APNs/WebPush sends) always run outside
// it, so a slow carrier probe or an unreachable push service can never block
// Status or inbound-SMS handling.
type Monitor struct {
	Store         *store.Store
	Push          push.Sender
	WebPush       *webpush.Manager
	WebPushSender *webpush.Sender
	Cfg           Config
	ReadFile      func(string) ([]byte, error)
	RunProbe      func(context.Context) (ProbeResult, string)
	Now           func() time.Time
	Log           *slog.Logger

	mu      sync.Mutex
	loaded  bool
	state   monitorState
	started bool
	ticking atomic.Bool
}

// New builds a monitor; call Start once the daemon context exists.
func New(st *store.Store, sender push.Sender, cfg Config) *Monitor {
	if cfg.WanLineID == "" {
		cfg.WanLineID = "line2"
	}
	if cfg.WanProbeInterval <= 0 {
		cfg.WanProbeInterval = 15 * time.Minute
	}
	if cfg.WanFailureProbeInterval <= 0 {
		cfg.WanFailureProbeInterval = 5 * time.Minute
	}
	if cfg.WanProbeTimeout <= 0 {
		cfg.WanProbeTimeout = 60 * time.Second
	}
	if cfg.SustainedFailures < 1 {
		cfg.SustainedFailures = 2
	}
	if cfg.RecoverySuccesses < 1 {
		cfg.RecoverySuccesses = 2
	}
	if cfg.MaxAlertAttempts < 1 {
		cfg.MaxAlertAttempts = 5
	}
	if len(cfg.CarrierSenders) == 0 {
		cfg.CarrierSenders = []string{"10086", "10010", "10000"}
	}
	monitor := &Monitor{Store: st, Push: sender, Cfg: cfg}
	monitor.ReadFile = os.ReadFile
	monitor.Now = func() time.Time { return time.Now().UTC() }
	monitor.Log = slog.Default()
	return monitor
}

func (m *Monitor) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

// Start runs the periodic observation loop until ctx ends.
func (m *Monitor) Start(ctx context.Context) {
	if m == nil || !m.Cfg.Enabled || m.started {
		return
	}
	m.started = true
	go func() {
		// Let the workers and the WAN agent settle before the first probe.
		timer := time.NewTimer(45 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if err := m.RunOnce(ctx); err != nil {
				m.Log.Warn("notify monitor tick failed", "error", err)
			}
			timer.Reset(m.nextInterval())
		}
	}()
}

func (m *Monitor) nextInterval() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Availability == AvailabilityUnavailable || m.state.LastProbeResult == string(ProbeFail) {
		return m.Cfg.WanFailureProbeInterval
	}
	return m.Cfg.WanProbeInterval
}

// Status is the read-only view used by the app.
type Status struct {
	Enabled           bool       `json:"enabled"`
	Availability      string     `json:"availability"`
	AvailabilitySince int64      `json:"availabilitySince,omitempty"`
	LastProbeAt       int64      `json:"lastProbeAt,omitempty"`
	LastProbeResult   string     `json:"lastProbeResult,omitempty"`
	AgentState        string     `json:"agentState,omitempty"`
	LineID            string     `json:"lineId"`
	LineName          string     `json:"lineName,omitempty"`
	LineTail          string     `json:"lineTail,omitempty"`
	Incidents         []Incident `json:"incidents,omitempty"`
}

// Status returns the current notifier view. A nil monitor is a valid
// disabled view, never a panic.
func (m *Monitor) Status(ctx context.Context) Status {
	if m == nil {
		return Status{Enabled: false, Availability: AvailabilityUnknown}
	}
	status := Status{Enabled: m.Cfg.Enabled, LineID: m.Cfg.WanLineID, Availability: AvailabilityUnknown}
	m.mu.Lock()
	if err := m.load(ctx); err != nil {
		m.Log.Warn("notify state unavailable", "error", err)
	}
	status.Availability = m.state.Availability
	if status.Availability == "" {
		status.Availability = AvailabilityUnknown
	}
	if !m.state.AvailabilitySince.IsZero() {
		status.AvailabilitySince = m.state.AvailabilitySince.UnixMilli()
	}
	if !m.state.LastProbeAt.IsZero() {
		status.LastProbeAt = m.state.LastProbeAt.UnixMilli()
	}
	status.LastProbeResult = m.state.LastProbeResult
	status.AgentState = m.state.LastAgentState
	for _, incident := range m.state.Incidents {
		if incident != nil {
			status.Incidents = append(status.Incidents, *incident)
		}
	}
	m.mu.Unlock()
	name, tail := m.lineLabel(ctx, m.Cfg.WanLineID)
	status.LineName = name
	status.LineTail = tail
	return status
}

func (m *Monitor) load(ctx context.Context) error {
	if m.loaded {
		return nil
	}
	m.loaded = true
	value, ok, err := m.Store.AlertValue(ctx, monitorStateKey)
	if err != nil || !ok {
		return err
	}
	if err := json.Unmarshal([]byte(value), &m.state); err != nil {
		m.Log.Warn("notify state is corrupt; starting fresh", "error", err)
		m.state = monitorState{}
	}
	if m.state.Incidents == nil {
		m.state.Incidents = map[string]*Incident{}
	}
	return nil
}

func (m *Monitor) saveLocked(ctx context.Context) error {
	payload, err := json.Marshal(m.state)
	if err != nil {
		return err
	}
	return m.Store.SetAlertValue(ctx, monitorStateKey, string(payload))
}

// RunOnce performs one observation pass. Network I/O (the isolated probe and
// all alert sends) happens outside the state lock; only the derived state
// update and durable reservations are serialized.
func (m *Monitor) RunOnce(ctx context.Context) error {
	if m == nil || !m.Cfg.Enabled {
		return nil
	}
	if !m.ticking.CompareAndSwap(false, true) {
		return nil
	}
	defer m.ticking.Store(false)

	// 1. Read the agent state file (small local file, no lock needed).
	agentState, hasAgent := m.readAgentState(ctx)

	// 2. Probe outside the lock. Skip while the agent itself is already
	// probing (degraded/blocked) to avoid contending for its lock.
	var probeResult ProbeResult
	probeRan := false
	if m.probeAvailable() && agentState != "degraded" && agentState != "blocked" {
		probeResult, _ = m.runProbe(ctx)
		probeRan = true
	}

	// 3. Apply the observation under the lock and collect reservations.
	m.mu.Lock()
	if err := m.load(ctx); err != nil {
		m.mu.Unlock()
		return err
	}
	now := m.now()
	var events []*store.AlertEvent
	if hasAgent {
		m.state.LastAgentState = agentState
	}
	if probeRan {
		m.state.LastProbeAt = now
		m.state.LastProbeResult = string(probeResult)
		switch probeResult {
		case ProbeOK:
			m.state.ConsecutiveSuccesses++
			m.state.ConsecutiveFailures = 0
		case ProbeFail:
			m.state.ConsecutiveFailures++
			m.state.ConsecutiveSuccesses = 0
		case ProbeUnknown:
			// Busy / fail-closed / exec error says nothing about the carrier.
		}
	}
	m.applyWanLocked(agentState, now, &events)
	m.retireStaleIncidentsLocked(now, &events)
	if err := m.saveLocked(ctx); err != nil {
		m.Log.Warn("notify state save failed", "error", err)
	}
	m.mu.Unlock()

	// 4. Deliver outside the lock, then retry anything still pending from
	// earlier passes. Events just attempted are skipped so a failing target
	// is not hammered twice within one tick.
	justAttempted := map[string]bool{}
	for _, event := range events {
		if event != nil {
			justAttempted[event.ID] = true
		}
	}
	m.deliverEvents(ctx, events)
	if err := m.retryPending(ctx, justAttempted); err != nil {
		m.Log.Warn("alert retry failed", "error", err)
	}
	return nil
}

// applyWanLocked translates the agent state and probe counters into the
// designated line's network incident. It never touches other lines.
func (m *Monitor) applyWanLocked(agentState string, now time.Time, events *[]*store.AlertEvent) {
	target := ""
	switch agentState {
	case "fallback", "fallback-simulated":
		target = AvailabilityAvailable
	case "blocked", "degraded":
		target = AvailabilityUnavailable
	}
	if target == "" {
		switch {
		case m.state.ConsecutiveFailures >= m.Cfg.SustainedFailures:
			target = AvailabilityUnavailable
		case m.state.LastProbeResult == string(ProbeOK) && m.state.ConsecutiveSuccesses >= m.Cfg.RecoverySuccesses:
			target = AvailabilityAvailable
		}
	}
	if target == "" || target == m.state.Availability {
		return
	}
	previous := m.state.Availability
	m.state.Availability = target
	m.state.AvailabilitySince = now
	lineID := m.Cfg.WanLineID
	switch {
	case target == AvailabilityUnavailable:
		incident := m.incidentLocked(lineID)
		if incident == nil {
			incident = m.openIncidentLocked(lineID, "network", now)
		}
		// One generic alert per incident. An explicit carrier SMS that already
		// told the owner is not duplicated with a generic message.
		if !incident.FaultNotified && !incident.ArrearsNotified && incident.Cause != "arrears" {
			incident.FaultNotified = true
			m.reserveLocked(events, lineID, incident, "network_unavailable", "network_unavailable", now)
		}
	case target == AvailabilityAvailable && previous == AvailabilityUnavailable:
		m.closeIncidentLocked(lineID, now, events, "")
	}
}

// retireStaleIncidentsLocked reconciles a restart during recovery: a
// sustained healthy probe that contradicts an open network-caused incident on
// the designated line retires it once. Carrier-arrears incidents are not
// closed here: the probe proves connectivity, not that the carrier cleared
// the balance, so they wait for a restore message or a real availability
// transition.
func (m *Monitor) retireStaleIncidentsLocked(now time.Time, events *[]*store.AlertEvent) {
	if m.state.Availability != AvailabilityAvailable || m.state.LastProbeResult != string(ProbeOK) {
		return
	}
	incident := m.incidentLocked(m.Cfg.WanLineID)
	if incident != nil && incident.Cause == "network" {
		m.closeIncidentLocked(m.Cfg.WanLineID, now, events, "")
	}
}

func (m *Monitor) incidentLocked(lineID string) *Incident {
	if m.state.Incidents == nil {
		m.state.Incidents = map[string]*Incident{}
	}
	return m.state.Incidents[lineID]
}

func (m *Monitor) openIncidentLocked(lineID, cause string, now time.Time) *Incident {
	incidentID, err := id.New("inc_", 8)
	if err != nil {
		incidentID = fmt.Sprintf("inc_%d", now.UnixNano())
	}
	incident := &Incident{LineID: lineID, ID: incidentID, Cause: cause, OpenedAt: now}
	if m.state.Incidents == nil {
		m.state.Incidents = map[string]*Incident{}
	}
	m.state.Incidents[lineID] = incident
	return incident
}

// closeIncidentLocked removes one line's incident and reserves exactly one
// recovery alert if anything had been notified for it. explicitAlert lets a
// carrier restore message choose the "restored" wording; empty derives the
// kind from the incident cause.
func (m *Monitor) closeIncidentLocked(lineID string, now time.Time, events *[]*store.AlertEvent, explicitAlert string) {
	incident := m.incidentLocked(lineID)
	if incident == nil {
		return
	}
	if (incident.FaultNotified || incident.ArrearsNotified) && !incident.RecoveryNotified {
		alert := explicitAlert
		if alert == "" {
			if incident.Cause == "arrears" {
				alert = "restored"
			} else {
				alert = "network_recovered"
			}
		}
		incident.RecoveryNotified = true
		m.reserveLocked(events, lineID, incident, "recovered", alert, now)
	}
	delete(m.state.Incidents, lineID)
}

// reserveLocked inserts the durable dedupe row keyed by (line, incident,
// phase). Repeated carrier messages for the same open incident are therefore
// absorbed without a second notification.
func (m *Monitor) reserveLocked(events *[]*store.AlertEvent, lineID string, incident *Incident, phase, alert string, now time.Time) {
	detail, err := json.Marshal(map[string]string{"alert": alert})
	if err != nil {
		return
	}
	event := store.AlertEvent{
		ID:         "line:" + lineID + ":inc:" + incident.ID + ":" + phase,
		Kind:       "line_alert",
		LineID:     lineID,
		IncidentID: incident.ID,
		Phase:      phase,
		Detail:     string(detail),
		CreatedAt:  now,
	}
	reserved, err := m.Store.ReserveAlertEvent(context.Background(), event)
	if err != nil {
		m.Log.Warn("reserve alert failed", "phase", phase, "line", lineID, "error", err)
		return
	}
	if reserved {
		*events = append(*events, &event)
	}
}

// ObserveInboundSMS classifies one live inbound message. Classification and
// the state update run without any network call; delivery happens after the
// lock is released, so a slow push service cannot stall the runtime.
func (m *Monitor) ObserveInboundSMS(ctx context.Context, lineID, messageID, sender, body string) {
	if m == nil || !m.Cfg.Enabled || messageID == "" {
		return
	}
	classification := ClassifyCarrierSMS(sender, body, m.Cfg.CarrierSenders)
	if classification.Verdict != VerdictArrears && classification.Verdict != VerdictRestored {
		m.Log.Debug("carrier sms ignored", "line", lineID, "verdict", classification.Verdict, "reason", classification.Reason)
		return
	}
	m.mu.Lock()
	if err := m.load(ctx); err != nil {
		m.mu.Unlock()
		return
	}
	now := m.now()
	var events []*store.AlertEvent
	if classification.Verdict == VerdictArrears {
		m.handleArrearsLocked(lineID, now, &events)
	} else {
		m.handleRestoredLocked(lineID, now, &events)
	}
	if err := m.saveLocked(ctx); err != nil {
		m.Log.Warn("notify state save failed", "error", err)
	}
	m.mu.Unlock()
	m.deliverEvents(ctx, events)
}

func (m *Monitor) handleArrearsLocked(lineID string, now time.Time, events *[]*store.AlertEvent) {
	incident := m.incidentLocked(lineID)
	if incident == nil {
		incident = m.openIncidentLocked(lineID, "arrears", now)
	}
	incident.Cause = "arrears"
	if incident.FaultNotified || incident.ArrearsNotified {
		// Incident-level dedupe: a second arrears message for the same open
		// incident only updates state, never sends another alert.
		incident.ArrearsNotified = true
		return
	}
	incident.ArrearsNotified = true
	m.reserveLocked(events, lineID, incident, "arrears", "arrears", now)
}

func (m *Monitor) handleRestoredLocked(lineID string, now time.Time, events *[]*store.AlertEvent) {
	incident := m.incidentLocked(lineID)
	if incident == nil {
		// No open fault: a recharge notice alone is not an alarm.
		return
	}
	m.closeIncidentLocked(lineID, now, events, "restored")
}

func (m *Monitor) readAgentState(ctx context.Context) (string, bool) {
	if strings.TrimSpace(m.Cfg.WanStateFile) == "" {
		return "", false
	}
	read := m.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	payload, err := read(m.Cfg.WanStateFile)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "state=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "state=")), true
		}
	}
	return "", false
}

func (m *Monitor) probeAvailable() bool {
	if m.RunProbe != nil {
		return true
	}
	if strings.TrimSpace(m.Cfg.WanProbeCommand) == "" {
		return false
	}
	info, err := os.Stat(m.Cfg.WanProbeCommand)
	return err == nil && !info.IsDir()
}

func (m *Monitor) runProbe(ctx context.Context) (ProbeResult, string) {
	if m.RunProbe != nil {
		return m.RunProbe(ctx)
	}
	timeout, cancel := context.WithTimeout(ctx, m.Cfg.WanProbeTimeout)
	defer cancel()
	command := exec.CommandContext(timeout, m.Cfg.WanProbeCommand, "--probe-only")
	output, _ := command.CombinedOutput()
	text := string(output)
	switch {
	case strings.Contains(text, "probe=ok"):
		return ProbeOK, text
	case strings.Contains(text, "probe=fail"):
		return ProbeFail, text
	default:
		return ProbeUnknown, text
	}
}

func (m *Monitor) deliverEvents(ctx context.Context, events []*store.AlertEvent) {
	for _, event := range events {
		if event != nil {
			m.deliver(ctx, *event)
		}
	}
}

func (m *Monitor) retryPending(ctx context.Context, skip map[string]bool) error {
	events, err := m.Store.PendingAlertEvents(ctx, m.Cfg.MaxAlertAttempts, 20)
	if err != nil {
		return err
	}
	now := m.now()
	for _, event := range events {
		if skip[event.ID] {
			continue
		}
		if now.Sub(event.CreatedAt) > 24*time.Hour {
			// Too stale to be useful; retire it without another push.
			_ = m.Store.MarkAlertDelivered(ctx, event.ID)
			continue
		}
		m.deliver(ctx, event)
	}
	return nil
}

type alertDetail struct {
	Alert string `json:"alert"`
}

// alertTarget is one device/subscription the alert should reach.
type alertTarget struct {
	key       string
	deviceID  string
	locale    string
	native    *push.Target
	webpushID string
	webpush   *webpush.Subscription
}

// deliveryCandidates resolves the current ACL- and preference-filtered target
// set for a line. It is recomputed on every attempt, so a device that
// registers a token while an alert is still retrying is included.
func (m *Monitor) deliveryCandidates(ctx context.Context, lineID string) []alertTarget {
	var candidates []alertTarget
	if m.Push != nil {
		targets, err := m.Store.ListPushTargets(ctx)
		if err != nil {
			m.Log.Warn("list push targets failed", "error", err)
		}
		for _, target := range targets {
			if !m.allowed(ctx, target.DeviceID, lineID) || target.APNSToken == "" {
				continue
			}
			if target.Environment != "sandbox" && target.Environment != "production" {
				continue
			}
			mode, err := m.Store.DeviceNotifyMode(ctx, target.DeviceID)
			if err == nil && mode == store.NotifyModeWeb {
				continue
			}
			candidate := alertTarget{
				key: "apns:" + target.DeviceID, deviceID: target.DeviceID, locale: target.Locale,
				native: &push.Target{
					DeviceID: target.DeviceID, APNSToken: target.APNSToken, VoIPToken: target.VoIPToken,
					Environment: target.Environment, Locale: target.Locale,
				},
			}
			candidates = append(candidates, candidate)
		}
	}
	if m.WebPushSender != nil {
		targets, err := m.Store.ListWebPushTargets(ctx)
		if err != nil {
			m.Log.Warn("list webpush targets failed", "error", err)
		}
		for _, target := range targets {
			if !m.allowed(ctx, target.DeviceID, lineID) {
				continue
			}
			mode, err := m.Store.DeviceNotifyMode(ctx, target.DeviceID)
			if err != nil || (mode != store.NotifyModeWeb && mode != store.NotifyModeBoth) {
				continue
			}
			subscription := webpush.Subscription{
				ID: target.SubID, Endpoint: target.Endpoint, Auth: target.Auth, P256dh: target.P256dh,
			}
			candidates = append(candidates, alertTarget{
				key: "webpush:" + target.DeviceID + ":" + target.SubID, deviceID: target.DeviceID,
				locale: target.Locale, webpushID: target.SubID, webpush: &subscription,
			})
		}
	}
	return candidates
}

// deliver fans one alert out to every eligible target, skipping targets whose
// per-target ledger already records a successful delivery. An alert is only
// retired when every current candidate has accepted it; otherwise the failed
// targets stay pending for bounded retries.
func (m *Monitor) deliver(ctx context.Context, event store.AlertEvent) {
	deliveries, err := m.Store.ListAlertDeliveries(ctx, event.ID)
	if err != nil {
		m.Log.Warn("list alert deliveries failed", "error", err)
		return
	}
	var detail alertDetail
	_ = json.Unmarshal([]byte(event.Detail), &detail)
	if detail.Alert == "" {
		detail.Alert = event.Phase
	}
	lineID := event.LineID
	if lineID == "" {
		lineID = m.Cfg.WanLineID
	}
	lineName, tail := m.lineLabel(ctx, lineID)
	candidates := m.deliveryCandidates(ctx, lineID)
	if len(candidates) == 0 {
		// Nothing to notify right now; count the pass so zero-target alerts
		// eventually stop retrying instead of spinning forever.
		_ = m.Store.MarkAlertAttempt(ctx, event.ID)
		return
	}

	var wg sync.WaitGroup
	for _, candidate := range candidates {
		if state, ok := deliveries[candidate.key]; ok && state.DeliveredAt != nil {
			continue
		}
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			title, body := AlertContent(detail.Alert, candidate.locale, lineName, tail)
			timeout, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			delivered := false
			if candidate.native != nil {
				err := m.Push.SendAlert(timeout, *candidate.native, push.AlertPayload{
					Kind: "line_alert", Alert: detail.Alert, LineID: lineID, LineName: lineName,
					Title: title, Body: body, IncidentID: event.IncidentID,
					GatewayID: m.Cfg.GatewayID, IssuedAt: m.now().Unix(),
				})
				if err != nil {
					m.Log.Warn("alert push failed", "device_id", candidate.deviceID, "line", lineID, "error", err)
				} else {
					delivered = true
				}
			} else if candidate.webpush != nil {
				payload, marshalErr := json.Marshal(webpush.Message{
					WebPush: 8030,
					Notification: webpush.Notification{
						Title: title, Body: body, Lang: LanguageTag(candidate.locale), Dir: "auto",
						Tag: "callrelay-alert-" + lineID, Navigate: "/pwa/", Silent: false,
					},
				})
				if marshalErr == nil {
					err := m.WebPushSender.Send(timeout, *candidate.webpush, payload)
					if err != nil {
						if webpush.SubscriptionGone(err) {
							_ = m.Store.DeleteWebPushSubscription(context.Background(), candidate.deviceID, candidate.webpushID)
						}
						m.Log.Warn("alert webpush failed", "device_id", candidate.deviceID, "error", err)
					} else {
						delivered = true
					}
				}
			}
			if err := m.Store.RecordAlertDelivery(context.Background(), event.ID, candidate.key, delivered); err != nil {
				m.Log.Warn("record alert delivery failed", "error", err)
			}
		}()
	}
	wg.Wait()

	// Retire the event only when no candidate remains undelivered.
	updated, err := m.Store.ListAlertDeliveries(ctx, event.ID)
	if err != nil {
		m.Log.Warn("reload alert deliveries failed", "error", err)
		return
	}
	allDelivered := true
	for _, candidate := range candidates {
		if state, ok := updated[candidate.key]; !ok || state.DeliveredAt == nil {
			allDelivered = false
			break
		}
	}
	if allDelivered {
		if err := m.Store.MarkAlertDelivered(ctx, event.ID); err != nil {
			m.Log.Warn("mark alert delivered failed", "error", err)
		}
		return
	}
	if err := m.Store.MarkAlertAttempt(ctx, event.ID); err != nil {
		m.Log.Warn("mark alert attempt failed", "error", err)
	}
}

// allowed enforces the line ACL server-side: a device only receives alerts
// for a line its pairing key is granted, regardless of token registration.
func (m *Monitor) allowed(ctx context.Context, deviceID, lineID string) bool {
	access, err := m.Store.DeviceAccess(ctx, deviceID)
	if err != nil {
		return false
	}
	for _, entry := range access.Lines {
		if entry.Line.ID == lineID {
			return true
		}
	}
	return false
}

func (m *Monitor) lineLabel(ctx context.Context, lineID string) (string, string) {
	name := ""
	if line, err := m.Store.GetLine(ctx, lineID); err == nil {
		name = line.Name
	}
	if name == "" {
		name = lineID
	}
	tail := ""
	if identity, err := m.Store.GetLineIdentity(ctx, lineID); err == nil {
		tail = Tail(identity.PhoneNumber)
	}
	return name, tail
}
