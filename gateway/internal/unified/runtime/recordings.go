package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"callrelay/gateway/internal/unified/recording"
	"callrelay/gateway/internal/unified/store"
)

// RecordingView is the API/event shape of one manual call recording. It
// intentionally exposes metadata only: the audio is served exclusively
// through the authenticated /audio route, never as a public URL.
type RecordingView struct {
	ID                string `json:"id"`
	CallID            string `json:"callId"`
	LineID            string `json:"lineId"`
	LineName          string `json:"lineName,omitempty"`
	Direction         string `json:"direction"`
	Peer              string `json:"peer,omitempty"`
	State             string `json:"state"`
	TriggerMode       string `json:"triggerMode"`
	StartedAt         int64  `json:"startedAt,omitempty"`
	StoppedAt         int64  `json:"stoppedAt,omitempty"`
	DurationMs        int64  `json:"durationMs"`
	SizeBytes         int64  `json:"sizeBytes,omitempty"`
	SHA256            string `json:"sha256,omitempty"`
	Container         string `json:"container,omitempty"`
	Codec             string `json:"codec,omitempty"`
	SampleRate        int    `json:"sampleRate,omitempty"`
	Channels          int    `json:"channels,omitempty"`
	CreatedByDeviceID string `json:"createdByDeviceId,omitempty"`
	// Kind is "call" (manual 1:1 segment) or "conference" (manual hosted
	// conference segment); ConferenceID is set only for the latter and is
	// the id used to start/stop the segment.
	Kind         string `json:"kind,omitempty"`
	ConferenceID string `json:"conferenceId,omitempty"`
	// KeyID is the enrollment-key scope that owns the recording. It is only
	// returned to devices already inside that scope and drives the event
	// stream filter (a different key sharing the line sees neither events
	// nor rows).
	KeyID         string `json:"keyId,omitempty"`
	FailureCode   string `json:"failureCode,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	CreatedAt     int64  `json:"createdAt"`
	ExpiresAt     int64  `json:"expiresAt,omitempty"`
}

func (m *Manager) recordingView(rec store.Recording) RecordingView {
	kind := rec.Kind
	if kind == "" {
		kind = "call"
	}
	view := RecordingView{
		ID: rec.ID, CallID: rec.CallID, LineID: rec.LineID, Direction: rec.Direction, Peer: rec.Peer,
		State: rec.State, TriggerMode: rec.TriggerMode, Container: rec.Container, Codec: rec.Codec,
		SampleRate: rec.SampleRate, Channels: rec.Channels, SizeBytes: valueOrZero(rec.SizeBytes),
		SHA256: rec.SHA256, CreatedByDeviceID: rec.CreatedByDevice, KeyID: rec.KeyID, FailureCode: rec.FailureCode,
		FailureReason: rec.FailureDetail, CreatedAt: rec.CreatedAt.UnixMilli(),
		Kind: kind, ConferenceID: rec.ConferenceID,
	}
	if rec.StartedAt != nil {
		view.StartedAt = rec.StartedAt.UnixMilli()
	}
	if rec.StoppedAt != nil {
		view.StoppedAt = rec.StoppedAt.UnixMilli()
	}
	if rec.DurationMs != nil {
		view.DurationMs = *rec.DurationMs
	}
	if rec.RetentionExpires != nil {
		view.ExpiresAt = rec.RetentionExpires.UnixMilli()
	}
	if line := m.Line(rec.LineID); line != nil {
		view.LineName = line.Cfg.Name
	}
	return view
}

func valueOrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// SetRecordings wires the recording manager and forwards its lifecycle
// events onto the bus (recording.* plus a refreshed call.updated so an
// active-call screen sees the live state without polling).
func (m *Manager) SetRecordings(manager *recording.Manager) {
	m.Recordings = manager
	if manager == nil {
		return
	}
	manager.SetEventHandler(func(kind string, rec store.Recording) {
		m.publish(rec.LineID, "", "recording."+kind, m.recordingView(rec))
		if rec.Kind == "conference" {
			// The conference host gets a targeted refresh so its in-call
			// indicator converges from server truth without polling. The
			// conference row is host-scoped, so no other device may learn
			// the recording belongs to it; the view also redacts recording
			// pointers if a member line grant was revoked.
			if rec.DeviceID != "" {
				ctx := context.Background()
				if _, err := m.Store.GetConference(ctx, rec.CallID); err == nil {
					m.publish("", rec.DeviceID, "conference.updated",
						m.conferenceViewForDevice(ctx, rec.CallID, rec.DeviceID))
				}
			}
			return
		}
		if call, err := m.Store.GetCall(context.Background(), rec.CallID); err == nil {
			full := m.view(call)
			// The owner device receives the recording pointers through a
			// targeted event; every other line-authorized device gets the
			// call state with the recording metadata redacted, so a
			// different key can never observe which call is being recorded.
			if rec.DeviceID != "" {
				m.publish(call.LineID, rec.DeviceID, "call.updated", full)
			}
			m.publish(call.LineID, "", "call.updated", redactedCallView(full))
		}
	})
}

// stopRecording finalizes an active segment (normal hangup path). EndCall
// also fences the call id so a concurrent start can never attach after the
// hangup.
func (m *Manager) stopRecording(callID, reason string) {
	if m.Recordings == nil {
		return
	}
	m.Recordings.EndCall(callID, reason)
}

// stopRecordingForConference finalizes an active 1:1 segment as COMPLETED
// because the call is being merged into a conference, and fences the call id
// so no new 1:1 segment can attach while it is a conference member. The
// conference can then be recorded manually under the conference id; the
// captured 1:1 audio stays playable instead of being failed silently.
func (m *Manager) stopRecordingForConference(callID string) {
	if m.Recordings == nil {
		return
	}
	m.Recordings.StopForConference(callID, "merged_into_conference")
}

// releaseConferenceRecordingFence lets a call that left a conference (split
// or last-leg convergence) start ordinary 1:1 segments again.
func (m *Manager) releaseConferenceRecordingFence(callID string) {
	if m.Recordings == nil {
		return
	}
	m.Recordings.UnblockCall(callID)
}

// recordingAllowed enforces the call-direction permission matrix for a
// recording row: an inbound call's recording needs receive_calls, an
// outbound one needs dial. A conference segment spans lines and is anchored
// on the host's first-leg line, so any current call grant on that line keeps
// it visible. Because requireLine already proves the line grant comes from
// this device's key, this is the per-key isolation rule.
func recordingAllowed(entry store.LineAccess, rec store.Recording) bool {
	if rec.Kind == "conference" {
		return entry.Permissions.Dial || entry.Permissions.ReceiveCalls
	}
	if rec.Direction == "inbound" {
		return entry.Permissions.ReceiveCalls
	}
	return entry.Permissions.Dial
}

func recordingFailure(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, recording.ErrStorageUnavailable):
		return fail(409, "CB-V2-409S", "录音存储卡不可用")
	case errors.Is(err, recording.ErrLowStorage):
		return fail(409, "CB-V2-409S", "录音存储空间不足")
	case errors.Is(err, recording.ErrCapacityReached):
		return fail(409, "CB-V2-409S", "录音存储容量已满")
	case errors.Is(err, recording.ErrRecordingDisabled):
		return fail(409, "CB-V2-409", "录音功能已关闭")
	case errors.Is(err, recording.ErrFinalizing):
		return fail(409, "CB-V2-409", "上一段录音正在收尾")
	case errors.Is(err, recording.ErrActiveRecording):
		return fail(409, "CB-V2-409", "录音正在进行中，请先停止")
	case errors.Is(err, recording.ErrStarting):
		return fail(409, "CB-V2-409", "录音正在启动，请稍候")
	case errors.Is(err, recording.ErrInConference):
		return fail(409, "CB-V2-409C", "该通话正在会议中，请使用会议录音")
	case errors.Is(err, recording.ErrCallEnded):
		return fail(409, CodeCallGone, "通话已结束")
	case errors.Is(err, recording.ErrNotActive):
		return fail(409, "CB-V2-409", "当前没有进行中的录音")
	}
	return err
}

// recordingKeyAllowed enforces the enrollment-key scope: a recording belongs
// to the key that started it. Fails CLOSED: a row without a key scope (this
// feature has no pre-existing real recordings) is never exposed to any
// device, and a device holding a DIFFERENT key that also happens to be
// authorized for the same line must never see the row, its events or its
// audio.
func recordingKeyAllowed(access store.Access, rec store.Recording) bool {
	return rec.KeyID != "" && rec.KeyID == access.Key.ID
}

// conferenceMemberCalls returns every call that has been a member of the
// conference. Removed legs stay in scope on purpose: their audio is part of
// the captured segment, so dropping their line from the access check would
// expose content from a line this key no longer holds. A member whose call
// metadata cannot be resolved FAILS the lookup: silently shrinking the scope
// would under-authorize the check.
func (m *Manager) conferenceMemberCalls(ctx context.Context, conferenceID string) ([]store.Call, error) {
	legs, err := m.Store.ListConferenceLegs(ctx, conferenceID)
	if err != nil {
		return nil, err
	}
	calls := make([]store.Call, 0, len(legs))
	for _, leg := range legs {
		call, callErr := m.Store.GetCall(ctx, leg.CallID)
		if callErr != nil {
			return nil, fmt.Errorf("conference %s member %s call metadata unavailable: %w", conferenceID, leg.CallID, callErr)
		}
		calls = append(calls, call)
	}
	return calls, nil
}

// conferenceMemberGrantsAllowed verifies the CURRENT grant of every member
// line with the direction-appropriate permission. A conference segment mixes
// audio from every member line, so start/list/get/audio/events all fail
// closed when ANY member's line/direction grant is missing or was revoked —
// including a secondary line of a multi-line conference and a leg added
// after the segment started (the member set is resolved at check time).
func (m *Manager) conferenceMemberGrantsAllowed(ctx context.Context, access store.Access, conferenceID string) bool {
	if strings.TrimSpace(conferenceID) == "" {
		return false
	}
	calls, err := m.conferenceMemberCalls(ctx, conferenceID)
	if err != nil || len(calls) == 0 {
		return false
	}
	for _, call := range calls {
		entry, ok := linePermission(access, call.LineID)
		if !ok {
			return false
		}
		if call.Direction == "inbound" {
			if !entry.Permissions.ReceiveCalls {
				return false
			}
		} else if !entry.Permissions.Dial {
			return false
		}
	}
	return true
}

// ConferenceRecordingScopeAllowed is the event-filter seam: it applies the
// same member-line grant matrix to a recording.* event identified only by its
// conference id, so a revoked secondary-line grant also silences the event
// stream (without loading the recording row, which may already be purged).
func (m *Manager) ConferenceRecordingScopeAllowed(ctx context.Context, access store.Access, conferenceID string) bool {
	return m.conferenceMemberGrantsAllowed(ctx, access, conferenceID)
}

// recordingScopeAllowed applies the line/member matrix WITHOUT the key check
// (callers report a key mismatch as not-found and a scope failure as
// forbidden, so existence stays hidden across keys).
func (m *Manager) recordingScopeAllowed(ctx context.Context, access store.Access, rec store.Recording) bool {
	if rec.Kind == "conference" {
		if rec.ConferenceID == "" {
			return false
		}
		return m.conferenceMemberGrantsAllowed(ctx, access, rec.ConferenceID)
	}
	entry, ok := linePermission(access, rec.LineID)
	return ok && recordingAllowed(entry, rec)
}

// recordingVisibleTo applies the complete key + scope matrix; used by the
// list and the event filter.
func (m *Manager) recordingVisibleTo(ctx context.Context, access store.Access, rec store.Recording) bool {
	if !recordingKeyAllowed(access, rec) {
		return false
	}
	return m.recordingScopeAllowed(ctx, access, rec)
}

// redactedCallView strips recording pointers/metadata. Line-scoped
// call.updated events use this form so a device from another key on the same
// line never learns which recording belongs to the call; the owner device
// receives the full view through a targeted event instead.
func redactedCallView(view CallView) CallView {
	view.RecordingID = ""
	view.RecordingState = ""
	view.RecordingDurationMs = nil
	return view
}

// requireRecordingCall resolves the call and applies the same ownership and
// permission checks as hangup. A conference leg is accepted here (the caller
// routes it to the conference recording path): a member call can only be
// controlled by its owner device, and the conference path re-checks the host
// device on top.
func (m *Manager) requireRecordingCall(ctx context.Context, deviceID, callID string) (store.Call, store.Access, error) {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return store.Call{}, store.Access{}, ErrCallGone
	}
	if call.EndedAt != nil {
		return store.Call{}, store.Access{}, fail(409, CodeCallGone, "通话已结束")
	}
	access, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return store.Call{}, store.Access{}, err
	}
	if !callAllowed(entry, call, deviceID) {
		return store.Call{}, store.Access{}, ErrForbidden
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return store.Call{}, store.Access{}, ErrForbidden
	}
	return call, access, nil
}

// StartRecording begins a manual segment for a live call. Idempotent: the
// existing segment is returned when one is already running. A conference leg
// is routed to the conference-scoped recording (host device only).
func (m *Manager) StartRecording(ctx context.Context, deviceID, callID string) (RecordingView, error) {
	call, access, err := m.requireRecordingCall(ctx, deviceID, callID)
	if err != nil {
		return RecordingView{}, err
	}
	if call.ConferenceID != "" {
		return m.StartConferenceRecording(ctx, deviceID, call.ConferenceID)
	}
	if m.Recordings == nil {
		return RecordingView{}, fail(503, CodeInternal, "录音功能不可用")
	}
	session := m.session(callID)
	if session == nil || session.Kind != "call" {
		return RecordingView{}, fail(409, "CB-V2-409M", "通话媒体未就绪，无法开始录音")
	}
	rec, startErr := m.Recordings.Start(ctx, recording.CallMeta{
		ID: call.ID, Kind: "call", LineID: call.LineID, Direction: call.Direction, Peer: call.Peer,
		StartedAt: call.CreatedAt, RetentionDays: m.lineRetentionDays(ctx, call.LineID), TriggerMode: "manual",
	}, deviceID, access.Key.ID)
	if startErr != nil {
		return RecordingView{}, recordingFailure(startErr)
	}
	// An explicit manual start is the ONLY user action that clears the
	// manual-stop suppression for this call (a genuinely new dial/answer has
	// a different call id and was never suppressed).
	m.autoRecordFence.clear(callID)
	_ = m.Store.AppendAudit(ctx, deviceID, "recording.start", rec.ID, "accepted")
	return m.recordingView(rec), nil
}

// StartConferenceRecording begins a manual segment for a live conference the
// device hosts. Only the host device (and therefore only its enrollment key)
// can start, stop or later reach the segment; the canonical mixer taps cover
// the host uplink plus the all-legs client mix exactly once.
func (m *Manager) StartConferenceRecording(ctx context.Context, deviceID, conferenceID string) (RecordingView, error) {
	return m.startConferenceRecording(ctx, deviceID, conferenceID, "manual")
}

// startConferenceRecording is the shared manual/auto entry: TriggerMode is
// carried into the durable row so the library can distinguish a user tap
// from the device/gateway auto preference.
func (m *Manager) startConferenceRecording(ctx context.Context, deviceID, conferenceID, trigger string) (RecordingView, error) {
	conference, err := m.requireConferenceHost(ctx, deviceID, conferenceID)
	if err != nil {
		return RecordingView{}, err
	}
	if conference.EndedAt != nil {
		return RecordingView{}, fail(404, "CB-V2-404", "会议已结束")
	}
	if m.Recordings == nil {
		return RecordingView{}, fail(503, CodeInternal, "录音功能不可用")
	}
	session := m.session(conferenceID)
	if session == nil || session.Kind != "conference" {
		return RecordingView{}, fail(409, "CB-V2-409M", "会议媒体未就绪，无法开始录音")
	}
	access, accessErr := m.Store.DeviceAccess(ctx, deviceID)
	if accessErr != nil {
		return RecordingView{}, fail(401, "CB-V2-401", "登录状态已失效")
	}
	// Current-grant gate for every member line/direction: a conference
	// segment mixes all of them, so the host's key must hold each grant at
	// the moment recording starts (a revoked secondary-line grant refuses
	// the start even though the anchor line is still granted).
	if !m.conferenceMemberGrantsAllowed(ctx, access, conferenceID) {
		return RecordingView{}, fail(403, "CB-V2-403L", "线路权限已变更，无法录制该会议")
	}
	meta, metaErr := m.conferenceRecordingMeta(ctx, conference)
	if metaErr != nil {
		return RecordingView{}, metaErr
	}
	meta.TriggerMode = trigger
	meta.RetentionDays = m.lineRetentionDays(ctx, meta.LineID)
	rec, startErr := m.Recordings.Start(ctx, meta, deviceID, access.Key.ID)
	if startErr != nil {
		return RecordingView{}, recordingFailure(startErr)
	}
	if trigger == "manual" {
		m.autoRecordFence.clear(conferenceID)
	}
	_ = m.Store.AppendAudit(ctx, deviceID, "recording.start", rec.ID, "accepted")
	return m.recordingView(rec), nil
}

// conferenceRecordingMeta anchors a conference segment row on the host's
// first live leg: line grant, direction and peer list come from the actual
// members, so the existing list/permission model stays truthful even when a
// conference spans lines.
func (m *Manager) conferenceRecordingMeta(ctx context.Context, conference store.Conference) (recording.CallMeta, error) {
	legs, err := m.Store.ListConferenceLegs(ctx, conference.ID)
	if err != nil {
		return recording.CallMeta{}, err
	}
	meta := recording.CallMeta{ID: conference.ID, Kind: "conference", StartedAt: conference.CreatedAt}
	peers := make([]string, 0, len(legs))
	for _, leg := range legs {
		if leg.RemovedAt != nil {
			continue
		}
		call, callErr := m.Store.GetCall(ctx, leg.CallID)
		if callErr != nil || call.EndedAt != nil {
			continue
		}
		if meta.LineID == "" {
			meta.LineID = call.LineID
			meta.Direction = call.Direction
		}
		if strings.TrimSpace(call.Peer) != "" {
			peers = append(peers, call.Peer)
		}
	}
	if meta.LineID == "" {
		return recording.CallMeta{}, fail(409, CodeCallGone, "会议中没有可用的通话")
	}
	meta.Peer = strings.Join(peers, "、")
	return meta, nil
}

// StopRecording finalizes the active segment. A call without an active
// segment converges to the latest finished segment instead of erroring when
// one exists, so retries after a lost response stay safe. A conference leg
// routes to the conference-scoped segment (host device only).
func (m *Manager) StopRecording(ctx context.Context, deviceID, callID string) (RecordingView, error) {
	call, _, err := m.requireRecordingCall(ctx, deviceID, callID)
	if err != nil {
		return RecordingView{}, err
	}
	if call.ConferenceID != "" {
		return m.StopConferenceRecording(ctx, deviceID, call.ConferenceID)
	}
	if m.Recordings == nil {
		return RecordingView{}, fail(503, CodeInternal, "录音功能不可用")
	}
	if !m.Recordings.Active(callID) {
		if rec, ok := m.Recordings.CallRecording(ctx, callID); ok && (rec.State == "finalizing" || rec.State == "ready") {
			return m.recordingView(rec), nil
		}
		return RecordingView{}, fail(409, "CB-V2-409", "当前没有进行中的录音")
	}
	if stopErr := m.Recordings.Stop(ctx, callID, "manual"); stopErr != nil {
		return RecordingView{}, recordingFailure(stopErr)
	}
	// A manual stop suppresses auto-restart for THIS call id (cleared when
	// the call ends), so an auto preference never fights the user.
	m.autoRecordFence.mark(callID)
	_ = m.Store.AppendAudit(ctx, deviceID, "recording.stop", callID, "accepted")
	rec, ok := m.Recordings.CallRecording(ctx, callID)
	if !ok {
		return RecordingView{}, fail(500, CodeInternal, "录音状态不可用")
	}
	return m.recordingView(rec), nil
}

// StopConferenceRecording finalizes the active conference segment. Only the
// conference host may stop it; a retry after a finished segment converges to
// the newest terminal state instead of erroring.
func (m *Manager) StopConferenceRecording(ctx context.Context, deviceID, conferenceID string) (RecordingView, error) {
	if _, err := m.requireConferenceHost(ctx, deviceID, conferenceID); err != nil {
		return RecordingView{}, err
	}
	if m.Recordings == nil {
		return RecordingView{}, fail(503, CodeInternal, "录音功能不可用")
	}
	if !m.Recordings.Active(conferenceID) {
		if rec, ok := m.Recordings.CallRecording(ctx, conferenceID); ok && (rec.State == "finalizing" || rec.State == "ready") {
			return m.recordingView(rec), nil
		}
		return RecordingView{}, fail(409, "CB-V2-409", "当前没有进行中的录音")
	}
	if stopErr := m.Recordings.Stop(ctx, conferenceID, "manual"); stopErr != nil {
		return RecordingView{}, recordingFailure(stopErr)
	}
	m.autoRecordFence.mark(conferenceID)
	_ = m.Store.AppendAudit(ctx, deviceID, "recording.stop", conferenceID, "accepted")
	rec, ok := m.Recordings.CallRecording(ctx, conferenceID)
	if !ok {
		return RecordingView{}, fail(500, CodeInternal, "录音状态不可用")
	}
	return m.recordingView(rec), nil
}

// ListRecordings returns recordings the device may see: same line filter and
// per-row direction permission as call history, so a key without the line
// grant never sees a row or event about it.
func (m *Manager) ListRecordings(ctx context.Context, deviceID, lineFilter string, limit int) ([]RecordingView, error) {
	if m.Recordings == nil {
		return []RecordingView{}, nil
	}
	access, err := m.Store.DeviceAccess(ctx, deviceID)
	if err != nil {
		return nil, fail(401, "CB-V2-401", "登录状态已失效")
	}
	allowed := map[string]bool{}
	for _, entry := range access.Lines {
		allowed[entry.Line.ID] = true
	}
	lineFilter = strings.TrimSpace(lineFilter)
	if lineFilter != "" && !allowed[lineFilter] {
		return nil, ErrForbidden
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	recordings, err := m.Recordings.List(ctx, lineFilter, limit)
	if err != nil {
		return nil, err
	}
	views := make([]RecordingView, 0, len(recordings))
	for _, rec := range recordings {
		// Conference rows carry audio from every member line, so they are
		// checked against the CURRENT grants of all members (including legs
		// removed after the segment started). Call rows keep the per-line
		// direction matrix.
		if !m.recordingVisibleTo(ctx, access, rec) {
			continue
		}
		views = append(views, m.recordingView(rec))
	}
	return views, nil
}

// RecordingForDevice resolves one recording with the same permission matrix.
func (m *Manager) RecordingForDevice(ctx context.Context, deviceID, recordingID string) (store.Recording, error) {
	if m.Recordings == nil {
		return store.Recording{}, fail(404, CodeCallGone, "录音不存在")
	}
	rec, err := m.Recordings.Get(ctx, recordingID)
	if err != nil {
		return store.Recording{}, fail(404, CodeCallGone, "录音不存在")
	}
	if rec.State == "deleted" {
		return store.Recording{}, fail(404, CodeCallGone, "录音不存在")
	}
	if rec.Kind == "conference" {
		access, accessErr := m.Store.DeviceAccess(ctx, deviceID)
		if accessErr != nil {
			return store.Recording{}, fail(401, "CB-V2-401", "登录状态已失效")
		}
		if !recordingKeyAllowed(access, rec) {
			// A different key must not learn the recording exists.
			return store.Recording{}, fail(404, CodeCallGone, "录音不存在")
		}
		if !m.recordingScopeAllowed(ctx, access, rec) {
			return store.Recording{}, ErrForbidden
		}
		return rec, nil
	}
	access, entry, _, err := m.requireLine(ctx, deviceID, rec.LineID, nil)
	if err != nil {
		return store.Recording{}, err
	}
	if !recordingKeyAllowed(access, rec) {
		// A different key sharing the line must not learn the recording
		// exists: report not-found, not forbidden.
		return store.Recording{}, fail(404, CodeCallGone, "录音不存在")
	}
	if !recordingAllowed(entry, rec) {
		return store.Recording{}, ErrForbidden
	}
	return rec, nil
}

// DeleteRecording tombstones a finished recording for the owning device.
func (m *Manager) DeleteRecording(ctx context.Context, deviceID, recordingID string) (RecordingView, error) {
	rec, err := m.RecordingForDevice(ctx, deviceID, recordingID)
	if err != nil {
		return RecordingView{}, err
	}
	deleted, deleteErr := m.Recordings.Delete(ctx, rec.ID)
	if deleteErr != nil {
		return RecordingView{}, recordingFailure(deleteErr)
	}
	_ = m.Store.AppendAudit(ctx, deviceID, "recording.delete", rec.ID, "accepted")
	return m.recordingView(deleted), nil
}

// RecordingDetailForDevice returns the API view for one recording.
func (m *Manager) RecordingDetailForDevice(ctx context.Context, deviceID, recordingID string) (RecordingView, error) {
	rec, err := m.RecordingForDevice(ctx, deviceID, recordingID)
	if err != nil {
		return RecordingView{}, err
	}
	return m.recordingView(rec), nil
}

// RecordingAudioPath resolves the authenticated audio blob for serving.
func (m *Manager) RecordingAudioPath(ctx context.Context, deviceID, recordingID string) (string, error) {
	if _, err := m.RecordingForDevice(ctx, deviceID, recordingID); err != nil {
		return "", err
	}
	return m.Recordings.AudioPath(ctx, recordingID)
}

// RecordingWaveformPath resolves the waveform JSON for serving.
func (m *Manager) RecordingWaveformPath(ctx context.Context, deviceID, recordingID string) (string, error) {
	if _, err := m.RecordingForDevice(ctx, deviceID, recordingID); err != nil {
		return "", err
	}
	return m.Recordings.WaveformPath(ctx, recordingID)
}

// RecordingSettings reports the manual-only configuration plus the
// verified SD storage state so a client can explain why recording is
// unavailable before the user tries.
type RecordingSettingsView struct {
	Enabled          bool                    `json:"enabled"`
	AutoMode         string                  `json:"autoMode"` // off | all | incoming | outgoing
	DeviceAutoRecord bool                    `json:"deviceAutoRecord"`
	RetentionDays    int                     `json:"retentionDays"`
	MinimumFreeBytes int64                   `json:"minimumFreeBytes"`
	MaxBytes         int64                   `json:"maxBytes"`
	Storage          recording.StorageStatus `json:"storage"`
}

// RecordingSettings reports the gateway auto mode (operator setting, default
// off), the requesting DEVICE's own auto preference (default OFF), and the
// verified SD storage state. Auto recording requires both to be on.
func (m *Manager) RecordingSettings(ctx context.Context, deviceID string) (RecordingSettingsView, error) {
	if m.Recordings == nil {
		return RecordingSettingsView{}, fail(503, CodeInternal, "录音功能不可用")
	}
	enabled, err := m.Recordings.Enabled(ctx)
	if err != nil {
		return RecordingSettingsView{}, err
	}
	mode := m.Store.RecordingAutoMode(ctx)
	prefs := m.Store.DeviceRecordingPrefs(ctx, deviceID)
	return RecordingSettingsView{
		Enabled: enabled, AutoMode: mode, DeviceAutoRecord: prefs.AutoRecord,
		RetentionDays:    m.Cfg.Recording.RetentionDays,
		MinimumFreeBytes: m.Cfg.Recording.MinimumFreeBytes, MaxBytes: m.Cfg.Recording.MaxBytes,
		Storage: m.Recordings.StorageStatus(ctx),
	}, nil
}

// SetRecordingEnabled is intentionally NOT exposed through the device API:
// the global manual-recording switch is an operator concern, not something
// any paired device should flip for every key. The manager method remains
// available for tests/console use.
func (m *Manager) SetRecordingEnabled(ctx context.Context, enabled bool) error {
	if m.Recordings == nil {
		return fail(503, CodeInternal, "录音功能不可用")
	}
	return m.Recordings.SetEnabled(ctx, enabled)
}

// recordingRetentionProbe is a test/diagnostic seam kept close to the API
// surface: it reports how many ready recordings would expire at now.
func (m *Manager) recordingRetentionProbe(ctx context.Context, now time.Time) (int, error) {
	if m.Recordings == nil {
		return 0, nil
	}
	rows, err := m.Store.ListExpiredRecordings(ctx, now, 500)
	return len(rows), err
}
