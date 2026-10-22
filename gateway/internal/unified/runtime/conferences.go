package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/store"

	"github.com/gorilla/websocket"
)

const maxConferenceLegs = 3 // host + <=3 external callers = 4 people total

type ConferenceView struct {
	ID            string     `json:"id"`
	HostDeviceID  string     `json:"hostDeviceId"`
	State         string     `json:"state"`
	CreatedAt     int64      `json:"createdAt"`
	GraceDeadline *int64     `json:"graceDeadline,omitempty"`
	Legs          []CallView `json:"legs"`
	// Manual conference recording state. Only the host device can fetch a
	// conference view at all, and the underlying recording row keeps its own
	// key/member-grant scope for list/get/audio independence from this view.
	RecordingID         string `json:"recordingId,omitempty"`
	RecordingState      string `json:"recordingState,omitempty"`
	RecordingDurationMs *int64 `json:"recordingDurationMs,omitempty"`
}

// conferenceView renders a conference WITHOUT a device scope: recording
// pointers are redacted (used by the operator console and by any broadcast
// that must not carry them).
func (m *Manager) conferenceView(conference store.Conference) ConferenceView {
	return m.conferenceViewFor(context.Background(), conference, nil)
}

// conferenceViewFor renders the conference for one device scope. Recording
// pointers are only included when that device's current access still satisfies
// the same key + member-line grant matrix as list/get/audio, so revoking a
// member line also redacts the live in-call state delivered through GET and
// conference.updated events. access nil means "no scope" -> redacted.
func (m *Manager) conferenceViewFor(ctx context.Context, conference store.Conference, access *store.Access) ConferenceView {
	view := ConferenceView{
		ID: conference.ID, HostDeviceID: conference.HostDeviceID, State: conference.State,
		CreatedAt: conference.CreatedAt.UnixMilli(),
	}
	if conference.GraceDeadline != nil {
		value := conference.GraceDeadline.UnixMilli()
		view.GraceDeadline = &value
	}
	if m.Recordings != nil && access != nil {
		if rec, ok := m.Recordings.CallRecording(ctx, conference.ID); ok && rec.State != "deleted" &&
			m.recordingVisibleTo(ctx, *access, rec) {
			view.RecordingID = rec.ID
			view.RecordingState = rec.State
			if rec.DurationMs != nil {
				duration := *rec.DurationMs
				view.RecordingDurationMs = &duration
			}
		}
	}
	legs, err := m.Store.ListConferenceLegs(ctx, conference.ID)
	if err != nil {
		return view
	}
	for _, leg := range legs {
		if leg.RemovedAt != nil {
			continue
		}
		call, callErr := m.Store.GetCall(ctx, leg.CallID)
		if callErr != nil {
			continue
		}
		view.Legs = append(view.Legs, m.view(call))
	}
	return view
}

// conferenceViewForDevice resolves the device scope and renders the view; a
// missing access (revoked session) yields the redacted form.
func (m *Manager) conferenceViewForDevice(ctx context.Context, conferenceID, deviceID string) ConferenceView {
	conference, err := m.Store.GetConference(ctx, conferenceID)
	if err != nil {
		return ConferenceView{ID: conferenceID}
	}
	access, accessErr := m.Store.DeviceAccess(ctx, deviceID)
	if accessErr != nil {
		return m.conferenceViewFor(ctx, conference, nil)
	}
	return m.conferenceViewFor(ctx, conference, &access)
}

// AdminConferenceViews lists conferences for the operator console.
func (m *Manager) AdminConferenceViews(ctx context.Context, activeOnly bool) ([]ConferenceView, error) {
	conferences, err := m.Store.ListConferences(ctx, activeOnly, 50)
	if err != nil {
		return nil, err
	}
	views := make([]ConferenceView, 0, len(conferences))
	for _, conference := range conferences {
		views = append(views, m.conferenceView(conference))
	}
	return views, nil
}

// AdminConferenceView fetches one conference without host-device scoping.
func (m *Manager) AdminConferenceView(ctx context.Context, conferenceID string) (ConferenceView, error) {
	conference, err := m.Store.GetConference(ctx, conferenceID)
	if err != nil {
		return ConferenceView{}, fail(404, "CB-V2-404", "会议不存在")
	}
	return m.conferenceView(conference), nil
}

func (m *Manager) GetConference(ctx context.Context, deviceID, conferenceID string) (ConferenceView, error) {
	if _, err := m.requireConferenceHost(ctx, deviceID, conferenceID); err != nil {
		return ConferenceView{}, err
	}
	if _, err := m.Store.GetConference(ctx, conferenceID); err != nil {
		return ConferenceView{}, fail(404, "CB-V2-404", "会议不存在")
	}
	// Recording pointers follow the same current member-grant scope as the
	// recording API; a revoked member line redacts them here too.
	return m.conferenceViewForDevice(ctx, conferenceID, deviceID), nil
}

func (m *Manager) requireConferenceHost(ctx context.Context, deviceID, conferenceID string) (store.Conference, error) {
	conference, err := m.Store.GetConference(ctx, conferenceID)
	if err != nil {
		return store.Conference{}, fail(404, "CB-V2-404", "会议不存在")
	}
	if conference.HostDeviceID != deviceID {
		return store.Conference{}, ErrForbidden
	}
	if _, err := m.Store.DeviceAccess(ctx, deviceID); err != nil {
		return store.Conference{}, fail(401, "CB-V2-401", "登录状态已失效")
	}
	return conference, nil
}

// Merge joins two or three calls already answered on this device into one
// conference. Host + at most three external callers is the four-person cap.
func (m *Manager) Merge(ctx context.Context, deviceID string, callIDs []string) (ConferenceView, error) {
	if len(callIDs) < 2 || len(callIDs) > maxConferenceLegs {
		return ConferenceView{}, fail(422, CodeConferenceSize, "合并需要 2-3 路外部通话（连同主持人最多 4 人）")
	}
	seen := map[string]bool{}
	calls := make([]store.Call, 0, len(callIDs))
	for _, callID := range callIDs {
		if seen[callID] {
			return ConferenceView{}, fail(422, CodeConferenceSize, "通话重复")
		}
		seen[callID] = true
		call, err := m.Store.GetCall(ctx, callID)
		if err != nil {
			return ConferenceView{}, ErrCallGone
		}
		if call.EndedAt != nil {
			return ConferenceView{}, ErrCallGone
		}
		if call.ConferenceID != "" {
			return ConferenceView{}, fail(409, "CB-V2-409D", "通话已在会议中")
		}
		if call.DeviceID != deviceID {
			return ConferenceView{}, ErrForbidden
		}
		if call.State != StateActive && call.State != StateHeld {
			return ConferenceView{}, fail(409, "CB-V2-409E", "只有已接通的通话可以合并")
		}
		if _, _, _, err := m.requireLine(ctx, deviceID, call.LineID, nil); err != nil {
			return ConferenceView{}, err
		}
		calls = append(calls, call)
	}

	conference := store.Conference{ID: randomID("conf_"), HostDeviceID: deviceID, State: "active", CreatedAt: time.Now().UTC()}
	if err := m.Store.CreateConference(ctx, conference); err != nil {
		return ConferenceView{}, err
	}
	conferenceSession := media.NewSession(m.Engine, conference.ID, "conference")
	conferenceSession.SetCallbacks(m.onHostLost, m.onHostRestored, m.onLegClosed)

	for _, call := range calls {
		// Conference audio is hosted by a multi-leg session. A 1:1 recording
		// segment is finalized as COMPLETED here (the user's captured audio
		// stays playable) and the call is fenced from 1:1 recording while it
		// is a member; the conference itself is recordable manually under
		// the conference id once its host media session exists.
		m.stopRecordingForConference(call.ID)
		leg := m.takeParkedLeg(call.ID)
		if oldSession := m.session(call.ID); oldSession != nil {
			if leg == nil {
				leg = oldSession.RemoveLeg(call.ID)
			}
			m.dropSession(call.ID, false)
			oldSession.Close()
		}
		if leg == nil {
			if mediaConn, attachErr := m.Line(call.LineID).Client.AttachMedia(ctx, call.WorkerCallID); attachErr == nil {
				leg = media.NewLeg(call.ID, &workerMediaTransport{mediaConn}, m.onLegClosed)
			} else {
				// The leg is still registered; media is attached on the
				// first conference offer once the worker is ready.
				slog.Warn("conference leg media deferred", "call_id", call.ID, "error", attachErr)
			}
		}
		if leg != nil {
			leg.SetHeld(call.Held)
			conferenceSession.AddLeg(leg)
		}
		if err := m.Store.AddConferenceLeg(ctx, store.ConferenceLeg{ConferenceID: conference.ID, CallID: call.ID, Held: call.Held, JoinedAt: time.Now().UTC()}); err != nil {
			slog.Warn("conference leg persist failed", "call_id", call.ID, "error", err)
		}
		call.ConferenceID = conference.ID
		call.State = chooseState(call)
		// Conditional write: a terminal claim racing the merge must not be
		// resurrected into the conference; the winning finisher already
		// published call.ended, so skip this stale update.
		if err := m.Store.UpsertLiveCall(ctx, call); err != nil {
			if errors.Is(err, store.ErrCallAlreadyEnded) {
				// The leg ended mid-merge (after the pre-checks): undo its
				// conference-side registration instead of mixing a terminal
				// leg into the new session.
				if leg := conferenceSession.RemoveLeg(call.ID); leg != nil {
					leg.Close()
				}
				_ = m.Store.RemoveConferenceLeg(ctx, conference.ID, call.ID)
				continue
			}
			slog.Warn("conference merge call persist failed", "call_id", call.ID, "error", err)
		}
		m.publish(call.LineID, deviceID, "call.updated", m.view(call))
	}
	m.setSession(conference.ID, conferenceSession)
	// A manually stopped auto-recording on any member continues to suppress
	// auto recording for the SAME ongoing conversation after it becomes a
	// hosted conference.
	for _, call := range calls {
		if m.autoRecordFence.isStopped(call.ID) {
			m.autoRecordFence.mark(conference.ID)
			break
		}
	}
	view := m.conferenceViewForDevice(ctx, conference.ID, deviceID)
	m.publish("", deviceID, "conference.created", view)
	// Auto recording for the HOST device only (default off twice over).
	go m.maybeAutoRecordConference(context.Background(), conference)
	return view, nil
}

func chooseState(call store.Call) string {
	if call.Held {
		return StateHeld
	}
	return StateActive
}

func (m *Manager) ensureConferenceSession(ctx context.Context, conference store.Conference) (*media.Session, error) {
	session := m.session(conference.ID)
	if session == nil {
		session = media.NewSession(m.Engine, conference.ID, "conference")
		session.SetCallbacks(m.onHostLost, m.onHostRestored, m.onLegClosed)
		m.setSession(conference.ID, session)
	}
	legs, err := m.Store.ListConferenceLegs(ctx, conference.ID)
	if err != nil {
		return nil, err
	}
	for _, leg := range legs {
		if leg.RemovedAt != nil {
			continue
		}
		call, callErr := m.Store.GetCall(ctx, leg.CallID)
		if callErr != nil || call.EndedAt != nil {
			continue
		}
		if session.Leg(call.ID) != nil {
			continue
		}
		mediaConn, attachErr := m.Line(call.LineID).Client.AttachMedia(ctx, call.WorkerCallID)
		if attachErr != nil {
			slog.Warn("conference session leg attach failed", "call_id", call.ID, "error", attachErr)
			continue
		}
		lineLeg := media.NewLeg(call.ID, &workerMediaTransport{mediaConn}, m.onLegClosed)
		lineLeg.SetHeld(call.Held)
		session.AddLeg(lineLeg)
	}
	return session, nil
}

// CheckWSConferenceMediaAccess enforces the conference media guards before an
// HTTP upgrade so rejections stay plain HTTP errors.
func (m *Manager) CheckWSConferenceMediaAccess(ctx context.Context, deviceID, conferenceID string) error {
	conference, err := m.requireConferenceHost(ctx, deviceID, conferenceID)
	if err != nil {
		return err
	}
	if conference.EndedAt != nil {
		return fail(404, "CB-V2-404", "会议已结束")
	}
	return nil
}

// ServeWSConferenceMedia attaches the host device's authenticated WebSocket
// as the conference's client audio transport, preserving the same mixer,
// hold/leg and host-lost grace semantics as the WebRTC conference path.
func (m *Manager) ServeWSConferenceMedia(ctx context.Context, deviceID, conferenceID, codec string, conn *websocket.Conn, denoise DenoiseChoice) error {
	if codec != "opus" {
		codec = ""
	}
	conference, err := m.requireConferenceHost(ctx, deviceID, conferenceID)
	if err != nil {
		return err
	}
	if conference.EndedAt != nil {
		return fail(404, "CB-V2-404", "会议已结束")
	}
	session, err := m.ensureConferenceSession(ctx, conference)
	if err != nil {
		return err
	}
	// Host device directional denoise (applies to its uplink and every
	// leg's downlink into the host mix).
	m.applyDenoise(ctx, deviceID, session, denoise)
	generation, err := session.AttachWSHost(conn, codec, func(negotiated string, _ media.AttachInfo) error {
		// Terminal re-check: the conference may have ended while its legs
		// were attaching; never resurrect it with a fresh host socket.
		if current, getErr := m.Store.GetConference(context.Background(), conferenceID); getErr == nil && current.EndedAt != nil {
			return fail(404, "CB-V2-404", "会议已结束")
		}
		ready := map[string]string{"type": "ready", "conferenceId": conferenceID}
		// Echo only the codec actually created on this host (local Opus
		// init may have fallen back to PCMU).
		if negotiated == media.CodecOpus {
			ready["codec"] = "opus"
			ready["fmt"] = media.WSFrameFmtSeq16
		}
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		defer conn.SetWriteDeadline(time.Time{})
		return conn.WriteJSON(ready)
	})
	if err != nil {
		return err
	}
	stopPing := media.PingWSHost(conn, 25*time.Second)
	defer stopPing()
	slog.Info("ws conference media attached", "conference_id", conferenceID, "device", deviceID)
	session.PumpWSHost(conn, generation)
	return nil
}

func (m *Manager) ConferenceOffer(ctx context.Context, deviceID, conferenceID, offerSDP string, denoise DenoiseChoice) (map[string]any, error) {
	conference, err := m.requireConferenceHost(ctx, deviceID, conferenceID)
	if err != nil {
		return nil, err
	}
	if conference.EndedAt != nil {
		return nil, fail(404, "CB-V2-404", "会议已结束")
	}
	session, err := m.ensureConferenceSession(ctx, conference)
	if err != nil {
		return nil, err
	}
	m.applyDenoise(ctx, deviceID, session, denoise)
	iceServers, policy := m.ICEConfiguration()
	answer, err := session.AttachClient(ctx, offerSDP, iceServers)
	if err != nil {
		return nil, fail(502, "CB-V2-502", "媒体协商失败")
	}
	return map[string]any{"sdp": answer, "type": "answer", "iceMode": policy, "conferenceId": conferenceID}, nil
}

func (m *Manager) CloseConference(ctx context.Context, deviceID, conferenceID, reason string) error {
	if _, err := m.requireConferenceHost(ctx, deviceID, conferenceID); err != nil {
		return err
	}
	return m.CloseConferenceAsSystem(ctx, conferenceID, reason)
}

func (m *Manager) CloseConferenceAsSystem(ctx context.Context, conferenceID, reason string) error {
	m.cancelRecovery(conferenceID)
	conference, err := m.Store.GetConference(ctx, conferenceID)
	if err != nil {
		return err
	}
	if conference.EndedAt != nil {
		return nil
	}
	legs, err := m.Store.ListConferenceLegs(ctx, conferenceID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	conference.State = "ended"
	conference.EndedAt = &now
	conference.GraceDeadline = nil
	_ = m.Store.UpdateConference(ctx, conference)
	for _, leg := range legs {
		if leg.RemovedAt != nil {
			continue
		}
		call, callErr := m.Store.GetCall(ctx, leg.CallID)
		if callErr != nil || call.EndedAt != nil {
			continue
		}
		if line := m.Line(call.LineID); line != nil {
			if err := line.Client.Hangup(ctx, call.WorkerCallID, randomKey()); err != nil && !isWorkerGone(err) {
				slog.Warn("conference leg hangup failed", "call_id", call.ID, "error", err)
			}
		}
		// Conditional terminal claim: a racing worker ended snapshot or
		// HTTP hangup must not be overwritten, and exactly one call.ended is
		// published per leg.
		won, claimErr := m.Store.FinishConferenceLeg(ctx, call.ID, now, reason)
		if claimErr != nil {
			slog.Error("conference leg terminal claim failed", "call_id", call.ID, "error", claimErr)
			continue
		}
		if !won {
			continue
		}
		call.State = StateEnded
		call.EndedAt = &now
		call.ConferenceID = ""
		if call.EndReason == "" {
			call.EndReason = reason
		}
		m.publish(call.LineID, "", "call.ended", m.view(call))
	}
	// Finalize an active conference segment as a completed recording before
	// the multi-leg session (and its canonical taps) goes away.
	m.stopRecording(conferenceID, "conference_ended")
	m.autoRecordFence.clear(conferenceID)
	m.dropSession(conferenceID, true)
	m.publish("", "", "conference.ended", map[string]any{"id": conferenceID, "reason": reason})
	return nil
}

func (m *Manager) RemoveLeg(ctx context.Context, deviceID, conferenceID, callID string) error {
	if _, err := m.requireConferenceHost(ctx, deviceID, conferenceID); err != nil {
		return err
	}
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return ErrCallGone
	}
	if call.ConferenceID != conferenceID {
		return fail(404, "CB-V2-404", "该通话不在会议中")
	}
	if session := m.session(conferenceID); session != nil {
		if leg := session.RemoveLeg(callID); leg != nil {
			leg.Close()
		}
	}
	if line := m.Line(call.LineID); line != nil {
		if err := line.Client.Hangup(ctx, call.WorkerCallID, randomKey()); err != nil && !isWorkerGone(err) {
			slog.Warn("remove leg hangup failed", "call_id", callID, "error", err)
		}
	}
	now := time.Now().UTC()
	// Conditional terminal claim: never overwrite a finisher that raced
	// the removal (worker ended snapshot, HTTP hangup) or publish a second
	// call.ended for one leg.
	won, claimErr := m.Store.FinishConferenceLeg(ctx, call.ID, now, "removed_from_conference")
	if claimErr != nil {
		return claimErr
	}
	call.State = StateEnded
	call.EndedAt = &now
	call.EndReason = "removed_from_conference"
	call.ConferenceID = ""
	_ = m.Store.RemoveConferenceLeg(ctx, conferenceID, callID)
	if won {
		m.publish(call.LineID, "", "call.ended", m.view(call))
	}
	m.handleConferenceLegChange(ctx, conferenceID)
	return nil
}

func (m *Manager) SetLegHeld(ctx context.Context, deviceID, conferenceID, callID string, held bool) (CallView, error) {
	if _, err := m.requireConferenceHost(ctx, deviceID, conferenceID); err != nil {
		return CallView{}, err
	}
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil || call.ConferenceID != conferenceID {
		return CallView{}, ErrCallGone
	}
	call.Held = held
	call.State = chooseState(call)
	if err := m.Store.UpsertLiveCall(ctx, call); err != nil {
		if errors.Is(err, store.ErrCallAlreadyEnded) {
			return CallView{}, ErrCallGone
		}
		return CallView{}, err
	}
	if err := m.Store.SetConferenceLegHeld(ctx, conferenceID, callID, held); err != nil {
		return CallView{}, err
	}
	if session := m.session(conferenceID); session != nil {
		if leg := session.Leg(callID); leg != nil {
			leg.SetHeld(held)
		}
	}
	view := m.view(call)
	m.publish(call.LineID, deviceID, "conference.updated", m.conferenceViewForDevice(ctx, conferenceID, deviceID))
	return view, nil
}

func (m *Manager) LegDTMF(ctx context.Context, deviceID, conferenceID, callID, digit, idempotencyKey string) error {
	if _, err := m.requireConferenceHost(ctx, deviceID, conferenceID); err != nil {
		return err
	}
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil || call.ConferenceID != conferenceID {
		return ErrCallGone
	}
	line := m.Line(call.LineID)
	if line == nil {
		return fail(410, CodeLineUnavailable, "线路未启用")
	}
	if err := line.Client.DTMF(ctx, call.WorkerCallID, digit, idempotencyKey); err != nil {
		return mapWorkerFailure(err)
	}
	return nil
}

// Split dissolves a conference: the selected leg becomes an ordinary active
// call and every other leg is held (isolated, no client media) until the
// host resumes it explicitly.
func (m *Manager) Split(ctx context.Context, deviceID, conferenceID, callID string) error {
	conference, err := m.requireConferenceHost(ctx, deviceID, conferenceID)
	if err != nil {
		return err
	}
	legs, err := m.Store.ListConferenceLegs(ctx, conferenceID)
	if err != nil {
		return err
	}
	found := false
	for _, leg := range legs {
		if leg.RemovedAt == nil && leg.CallID == callID {
			found = true
		}
	}
	if !found {
		return fail(404, "CB-V2-404", "该通话不在会议中")
	}
	conferenceSession := m.session(conferenceID)
	now := time.Now().UTC()
	conference.State = "ended"
	conference.EndedAt = &now
	conference.GraceDeadline = nil
	_ = m.Store.UpdateConference(ctx, conference)
	// The conference is dissolving: finalize its segment as completed before
	// the session (and its taps) closes. Every leg becomes a standalone call
	// again, so release the 1:1 recording fence for each.
	m.stopRecording(conferenceID, "conference_split")
	conferenceSuppressed := m.autoRecordFence.isStopped(conferenceID)

	var selected *media.Leg
	for _, leg := range legs {
		if leg.RemovedAt != nil {
			continue
		}
		call, callErr := m.Store.GetCall(ctx, leg.CallID)
		if callErr != nil || call.EndedAt != nil {
			m.releaseConferenceRecordingFence(leg.CallID)
			continue
		}
		m.releaseConferenceRecordingFence(call.ID)
		if conferenceSuppressed {
			// The ongoing conversation keeps its manual-stop suppression
			// after splitting back into a 1:1 call.
			m.autoRecordFence.mark(call.ID)
		}
		var transferred *media.Leg
		if conferenceSession != nil {
			transferred = conferenceSession.RemoveLeg(call.ID)
		}
		call.ConferenceID = ""
		if call.ID == callID {
			selected = transferred
			call.Held = false
			call.State = StateActive
		} else {
			call.Held = true
			call.State = StateHeld
			if transferred != nil {
				m.parkLeg(call.ID, transferred)
			}
		}
		if err := m.Store.UpsertLiveCall(ctx, call); err != nil {
			// A finisher racing the split owns the terminal truth; do not
			// resurrect the leg or publish a stale update for it.
			if errors.Is(err, store.ErrCallAlreadyEnded) {
				continue
			}
			slog.Warn("conference split leg persist failed", "call_id", call.ID, "error", err)
		}
		m.publish(call.LineID, deviceID, "call.updated", m.view(call))
	}
	if conferenceSession != nil {
		conferenceSession.Close()
	}
	m.dropSession(conferenceID, false)
	if selected == nil {
		if call, callErr := m.Store.GetCall(ctx, callID); callErr == nil {
			if mediaConn, attachErr := m.Line(call.LineID).Client.AttachMedia(ctx, call.WorkerCallID); attachErr == nil {
				selected = media.NewLeg(call.ID, &workerMediaTransport{mediaConn}, m.onLegClosed)
			}
		}
	}
	if selected != nil {
		session := media.NewSession(m.Engine, callID, "call")
		session.SetCallbacks(m.onHostLost, m.onHostRestored, m.onLegClosed)
		session.AddLeg(selected)
		m.setSession(callID, session)
	}
	m.publish("", deviceID, "conference.ended", map[string]any{"id": conferenceID, "reason": "split"})
	return nil
}

// RemoveLegAsSystem is the console-forced variant of RemoveLeg.
func (m *Manager) RemoveLegAsSystem(ctx context.Context, conferenceID, callID string) error {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return ErrCallGone
	}
	if call.ConferenceID != conferenceID {
		return fail(404, "CB-V2-404", "该通话不在会议中")
	}
	if session := m.session(conferenceID); session != nil {
		if leg := session.RemoveLeg(callID); leg != nil {
			leg.Close()
		}
	}
	if line := m.Line(call.LineID); line != nil {
		if err := line.Client.Hangup(ctx, call.WorkerCallID, randomKey()); err != nil && !isWorkerGone(err) {
			slog.Warn("console remove leg hangup failed", "call_id", callID, "error", err)
		}
	}
	now := time.Now().UTC()
	won, claimErr := m.Store.FinishConferenceLeg(ctx, call.ID, now, "removed_from_conference")
	if claimErr != nil {
		return claimErr
	}
	call.State = StateEnded
	call.EndedAt = &now
	call.EndReason = "removed_from_conference"
	call.ConferenceID = ""
	_ = m.Store.RemoveConferenceLeg(ctx, conferenceID, callID)
	if won {
		m.publish(call.LineID, "", "call.ended", m.view(call))
	}
	m.handleConferenceLegChange(ctx, conferenceID)
	return nil
}

// handleConferenceLegChange converges conference shape after a leg ends:
// one remaining leg becomes an ordinary call, zero ends the conference.
func (m *Manager) handleConferenceLegChange(ctx context.Context, conferenceID string) {
	conference, err := m.Store.GetConference(ctx, conferenceID)
	if err != nil || conference.EndedAt != nil {
		return
	}
	legs, err := m.Store.ListConferenceLegs(ctx, conferenceID)
	if err != nil {
		return
	}
	active := make([]store.ConferenceLeg, 0, len(legs))
	for _, leg := range legs {
		if leg.RemovedAt != nil {
			continue
		}
		call, callErr := m.Store.GetCall(ctx, leg.CallID)
		if callErr != nil || call.EndedAt != nil {
			_ = m.Store.RemoveConferenceLeg(ctx, conferenceID, leg.CallID)
			continue
		}
		active = append(active, leg)
	}
	switch {
	case len(active) == 0:
		now := time.Now().UTC()
		conference.State = "ended"
		conference.EndedAt = &now
		_ = m.Store.UpdateConference(ctx, conference)
		// Finalize an active conference segment as completed before the
		// session goes away; no leg survives, so no fence is released.
		m.stopRecording(conferenceID, "conference_ended")
		m.autoRecordFence.clear(conferenceID)
		m.dropSession(conferenceID, true)
		m.publish("", "", "conference.ended", map[string]any{"id": conferenceID, "reason": "no_legs"})
	case len(active) == 1:
		call, callErr := m.Store.GetCall(ctx, active[0].CallID)
		if callErr != nil {
			return
		}
		var leg *media.Leg
		if session := m.session(conferenceID); session != nil {
			leg = session.RemoveLeg(call.ID)
		}
		if leg == nil {
			if mediaConn, attachErr := m.Line(call.LineID).Client.AttachMedia(ctx, call.WorkerCallID); attachErr == nil {
				leg = media.NewLeg(call.ID, &workerMediaTransport{mediaConn}, m.onLegClosed)
			}
		}
		now := time.Now().UTC()
		conference.State = "ended"
		conference.EndedAt = &now
		_ = m.Store.UpdateConference(ctx, conference)
		// The last leg returns to an ordinary call: finalize the conference
		// segment as completed and allow the call to record as 1:1 again —
		// unless the ongoing conversation was manually auto-stop suppressed,
		// in which case the suppression follows the surviving leg.
		m.stopRecording(conferenceID, "conference_ended")
		if m.autoRecordFence.isStopped(conferenceID) {
			m.autoRecordFence.mark(call.ID)
		}
		m.autoRecordFence.clear(conferenceID)
		m.releaseConferenceRecordingFence(call.ID)
		m.dropSession(conferenceID, true)
		call.ConferenceID = ""
		call.State = chooseState(call)
		if err := m.Store.UpsertLiveCall(ctx, call); err != nil {
			// A racing finisher owns terminal truth; skip the stale
			// update and any session resurrection.
			if errors.Is(err, store.ErrCallAlreadyEnded) {
				return
			}
			slog.Warn("conference convergence persist failed", "call_id", call.ID, "error", err)
		}
		if leg != nil {
			leg.SetHeld(call.Held)
			session := media.NewSession(m.Engine, call.ID, "call")
			session.SetCallbacks(m.onHostLost, m.onHostRestored, m.onLegClosed)
			session.AddLeg(leg)
			m.setSession(call.ID, session)
		}
		m.publish(call.LineID, call.DeviceID, "call.updated", m.view(call))
		m.publish("", call.DeviceID, "conference.ended", map[string]any{"id": conferenceID, "reason": "last_leg_returns_to_call", "callId": call.ID})
	default:
		m.publish("", "", "conference.updated", m.conferenceView(conference))
	}
}
