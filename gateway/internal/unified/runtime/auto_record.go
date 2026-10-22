package runtime

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"callrelay/gateway/internal/unified/recording"
	"callrelay/gateway/internal/unified/store"
)

// Auto recording (build 55) is opt-in twice over: the gateway operator mode
// (default off) AND the owning device's preference (default OFF). It only
// starts AFTER the device actually owns an answered/outgoing call with live
// media, never on a merely-dialing/ringing leg. A manual stop permanently
// suppresses auto-restart for that same call id (the fence is dropped when
// the call ends, so the next call is unaffected).

type autoRecordFence struct {
	mu      sync.Mutex
	stopped map[string]bool
}

func (f *autoRecordFence) mark(callID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped == nil {
		f.stopped = map[string]bool{}
	}
	f.stopped[callID] = true
}

func (f *autoRecordFence) isStopped(callID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped[callID]
}

func (f *autoRecordFence) clear(callID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.stopped, callID)
}

// lineRetentionDays resolves the per-line retention override (stored on the
// line row) for a recording segment.
func (m *Manager) lineRetentionDays(ctx context.Context, lineID string) int {
	if line, err := m.Store.GetLine(ctx, lineID); err == nil {
		return line.RecordingRetentionDays
	}
	return 0
}

// maybeAutoRecord starts an automatic 1:1 segment when every condition is
// met. Idempotent: an active/starting segment is left alone.
func (m *Manager) maybeAutoRecord(ctx context.Context, call store.Call) {
	if m.Recordings == nil || call.EndedAt != nil || call.DeviceID == "" {
		return
	}
	if strings.HasPrefix(call.State, "voicemail") || call.ConferenceID != "" {
		return
	}
	// The owning device must actually have an answered/outgoing call: a
	// media session alone is not ownership, and a ringing leg never starts.
	if call.AnsweredAt == nil {
		return
	}
	mode := m.Store.RecordingAutoMode(ctx)
	if mode == "off" {
		return
	}
	if mode == "incoming" && call.Direction != "inbound" {
		return
	}
	if mode == "outgoing" && call.Direction != "outbound" {
		return
	}
	if !m.Store.DeviceRecordingPrefs(ctx, call.DeviceID).AutoRecord {
		return
	}
	if m.autoRecordFence.isStopped(call.ID) {
		return
	}
	if m.Recordings.Active(call.ID) {
		return
	}
	session := m.session(call.ID)
	if session == nil || session.Kind != "call" {
		return
	}
	access, err := m.Store.DeviceAccess(ctx, call.DeviceID)
	if err != nil {
		return
	}
	rec, err := m.Recordings.Start(ctx, recording.CallMeta{
		ID: call.ID, Kind: "call", LineID: call.LineID, Direction: call.Direction,
		Peer: call.Peer, StartedAt: call.CreatedAt,
		RetentionDays: m.lineRetentionDays(ctx, call.LineID), TriggerMode: "auto",
	}, call.DeviceID, access.Key.ID)
	if err != nil {
		slog.Warn("auto recording start failed", "call_id", call.ID, "device_id", call.DeviceID, "error", err)
		return
	}
	_ = m.Store.AppendAudit(ctx, call.DeviceID, "recording.auto.start", rec.ID, "accepted")
}

// maybeAutoRecordConference starts an automatic conference segment for the
// HOST device only (members can never auto-record a conference they do not
// host). Called once the conference session exists.
func (m *Manager) maybeAutoRecordConference(ctx context.Context, conference store.Conference) {
	if m.Recordings == nil || conference.EndedAt != nil || conference.HostDeviceID == "" {
		return
	}
	mode := m.Store.RecordingAutoMode(ctx)
	if mode == "off" {
		return
	}
	if !m.Store.DeviceRecordingPrefs(ctx, conference.HostDeviceID).AutoRecord {
		return
	}
	if m.autoRecordFence.isStopped(conference.ID) {
		return
	}
	if m.Recordings.Active(conference.ID) {
		return
	}
	session := m.session(conference.ID)
	if session == nil || session.Kind != "conference" {
		return
	}
	if _, err := m.startConferenceRecording(ctx, conference.HostDeviceID, conference.ID, "auto"); err != nil {
		slog.Warn("auto conference recording start failed", "conference_id", conference.ID, "device_id", conference.HostDeviceID, "error", err)
		return
	}
	_ = m.Store.AppendAudit(ctx, conference.HostDeviceID, "recording.auto.start", conference.ID, "accepted")
}
