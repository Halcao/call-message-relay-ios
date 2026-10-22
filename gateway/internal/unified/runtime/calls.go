package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"callrelay/gateway/internal/unified/harass"
	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/unified/worker"

	"github.com/gorilla/websocket"
	pion "github.com/pion/webrtc/v4"
)

// ---------- dial ----------

func (m *Manager) Dial(ctx context.Context, deviceID, lineID, to, clientCallID, idempotencyKey string) (CallView, error) {
	if strings.TrimSpace(to) == "" || strings.TrimSpace(clientCallID) == "" {
		return CallView{}, fail(400, "CB-V2-400", "to 和 clientCallId 必填")
	}
	_, _, line, err := m.requireLine(ctx, deviceID, lineID, func(permission store.KeyLine) bool { return permission.Dial })
	if err != nil {
		return CallView{}, err
	}
	stored, err := m.Store.GetLine(ctx, lineID)
	if err != nil {
		return CallView{}, fail(410, CodeLineUnavailable, "线路不存在")
	}
	if !stored.AllowOutbound {
		return CallView{}, fail(451, CodePolicyBlocked, "该线路已禁止外呼")
	}
	if matchesBlocked(stored.BlockedPrefixes, to) {
		return CallView{}, fail(451, CodePolicyBlocked, "目标号码被线路策略拦截")
	}
	status, reachable, _ := line.Status()
	if !reachable || status.SIM != "ready" || status.Registration != "registered" {
		return CallView{}, fail(410, CodeLineUnavailable, "线路尚未就绪")
	}
	live, err := m.Store.CountLiveCalls(ctx)
	if err != nil {
		return CallView{}, err
	}
	if live >= m.Cfg.Settings.MaxConcurrentCalls {
		return CallView{}, ErrCapacity
	}
	onLine, err := m.Store.CountLiveCallsOnLine(ctx, lineID)
	if err != nil {
		return CallView{}, err
	}
	if onLine > 0 {
		return CallView{}, ErrLineBusy
	}
	globalID := lineID + ":" + strings.TrimSpace(clientCallID)
	if existing, err := m.Store.GetCall(ctx, globalID); err == nil {
		return m.view(existing), nil
	}
	record, err := line.Client.Dial(ctx, to, clientCallID, idempotencyKey)
	if err != nil {
		return CallView{}, mapWorkerFailure(err)
	}
	call := store.Call{
		ID: globalID, LineID: lineID, WorkerCallID: record.ID, DeviceID: deviceID,
		Direction: "outbound", Peer: to, State: mapWorkerState(record.State), CreatedAt: time.Now().UTC(),
	}
	if call.State == StateEnded {
		call.State = StateDialing
	}
	if err := m.Store.UpsertCall(ctx, call); err != nil {
		return CallView{}, err
	}
	view := m.view(call)
	m.publish(lineID, deviceID, "call.updated", view)
	return view, nil
}

// ---------- answer / decline / reject ----------

func (m *Manager) Answer(ctx context.Context, deviceID, callID, idempotencyKey string) (CallView, error) {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return CallView{}, ErrCallGone
	}
	if _, _, _, err := m.requireLine(ctx, deviceID, call.LineID, func(permission store.KeyLine) bool { return permission.ReceiveCalls }); err != nil {
		return CallView{}, err
	}
	if call.EndedAt != nil {
		return CallView{}, ErrCallGone
	}
	if call.State != StateRinging {
		if call.DeviceID == deviceID {
			return m.view(call), nil
		}
		if strings.HasPrefix(call.State, "voicemail") {
			return CallView{}, fail(409, "CB-V2-409F", "来电已转入语音留言")
		}
		return CallView{}, fail(409, CodeAlreadyAnswered, "通话已被其他设备接听")
	}
	claimed, err := m.Store.ClaimCall(ctx, callID, deviceID)
	if err != nil {
		return CallView{}, err
	}
	if !claimed {
		current, fetchErr := m.Store.GetCall(ctx, callID)
		if fetchErr == nil && current.DeviceID == deviceID {
			return m.view(current), nil
		}
		if fetchErr == nil && strings.HasPrefix(current.State, "voicemail") {
			return CallView{}, fail(409, "CB-V2-409F", "来电已转入语音留言")
		}
		return CallView{}, fail(409, CodeAlreadyAnswered, "通话已被其他设备接听")
	}
	line := m.Line(call.LineID)
	if line == nil {
		return CallView{}, fail(410, CodeLineUnavailable, "线路未启用")
	}
	if err := line.Client.Answer(ctx, call.WorkerCallID, idempotencyKey); err != nil {
		// Return the atomic claim so other devices/voicemail can answer;
		// only the still-answering row owned by this device is released, so
		// a racing terminal or answer claim survives.
		_ = m.Store.ReleaseCallClaim(ctx, callID, deviceID)
		return CallView{}, mapWorkerFailure(err)
	}
	now := time.Now().UTC()
	call.DeviceID = deviceID
	call.State = StateConnecting
	// answered_at is monotonic: a racing worker snapshot may already have
	// recorded connectedAt, and the terminal claim may have landed while the
	// upstream Answer RPC was in flight. UpsertLiveCall keeps the earliest
	// answer time and never writes a terminal row.
	if call.AnsweredAt == nil {
		call.AnsweredAt = &now
	}
	if err := m.Store.UpsertLiveCall(ctx, call); err != nil {
		if errors.Is(err, store.ErrCallAlreadyEnded) {
			return CallView{}, ErrCallGone
		}
		return CallView{}, err
	}
	m.cancelVoicemailTimer(callID)
	view := m.view(call)
	m.publish(call.LineID, "", "call.answered", view)
	m.publish(call.LineID, deviceID, "call.updated", view)
	// Auto recording (if enabled for this device) may start as soon as the
	// owning device's media session exists; the helper re-checks everything.
	go m.maybeAutoRecord(context.Background(), call)
	return view, nil
}

// Decline is device-local: it never rejects the upstream call unless every
// device authorized to ring that line has declined.
func (m *Manager) Decline(ctx context.Context, deviceID, callID string) error {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return ErrCallGone
	}
	if _, _, _, err := m.requireLine(ctx, deviceID, call.LineID, func(permission store.KeyLine) bool { return permission.ReceiveCalls }); err != nil {
		return err
	}
	if call.State != StateRinging || call.EndedAt != nil {
		return nil
	}
	declined, err := m.Store.RecordDecline(ctx, callID, deviceID)
	if err != nil {
		return err
	}
	m.publish(call.LineID, "", "call.declined", map[string]string{"callId": callID, "deviceId": deviceID})
	remaining, err := m.otherRingingDevices(ctx, call.LineID, declined, call.Peer)
	if err != nil {
		return err
	}
	if !remaining {
		return m.RejectAsSystem(ctx, callID, "all_devices_declined")
	}
	return nil
}

func (m *Manager) otherRingingDevices(ctx context.Context, lineID, declined, peer string) (bool, error) {
	declinedSet := map[string]bool{}
	for _, value := range strings.Split(declined, ",") {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			declinedSet[trimmed] = true
		}
	}
	devices, err := m.Store.ListDevices(ctx)
	if err != nil {
		return false, err
	}
	for _, device := range devices {
		if declinedSet[device.ID] {
			continue
		}
		access, accessErr := m.Store.DeviceAccess(ctx, device.ID)
		if accessErr != nil {
			continue
		}
		if entry, ok := linePermission(access, lineID); ok && entry.Permissions.ReceiveCalls {
			// A key whose harassment policy denies this caller is not
			// "still ringing": it never received the event/ring/push, so it
			// must not keep the upstream call alive either.
			if m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindCall, peer, "") {
				continue
			}
			return true, nil
		}
	}
	return false, nil
}

// Reject is the explicit force-reject used by the console and policies.
// A device actor on a ringing call is subject to the cross-key isolation
// rule: when another enrollment key still legitimately sees the call, the
// request degrades to that device's local decline instead of killing the
// physical call for everyone. Console/system actors keep force-reject.
func (m *Manager) Reject(ctx context.Context, actor, callID, reason string) error {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return ErrCallGone
	}
	if call.EndedAt != nil {
		return nil
	}
	if actor != "console" && actor != "system" {
		if _, _, _, err := m.requireLine(ctx, actor, call.LineID, func(permission store.KeyLine) bool { return permission.ReceiveCalls }); err != nil {
			return err
		}
		if call.State == StateRinging && m.crossKeyRingingRemains(ctx, call.LineID, call.Peer, actor) {
			return m.Decline(ctx, actor, callID)
		}
	}
	return m.rejectCall(ctx, call, reason)
}

func (m *Manager) RejectAsSystem(ctx context.Context, callID, reason string) error {
	return m.Reject(ctx, "system", callID, reason)
}

func (m *Manager) rejectCall(ctx context.Context, call store.Call, reason string) error {
	line := m.Line(call.LineID)
	if line != nil {
		if err := line.Client.Reject(ctx, call.WorkerCallID, randomKey()); err != nil && !isWorkerGone(err) {
			slog.Warn("worker reject failed", "call_id", call.ID, "error", err)
		}
	}
	m.finishCall(call, reason)
	return nil
}

// ---------- hangup / hold / resume / dtmf ----------

func (m *Manager) Hangup(ctx context.Context, deviceID, callID string) error {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return ErrCallGone
	}
	if call.EndedAt != nil {
		return nil
	}
	_, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return err
	}
	if !callAllowed(entry, call, deviceID) {
		return ErrForbidden
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return ErrForbidden
	}
	// Cross-key isolation: a device hangup of a still-ringing call must not
	// kill a call another key is still being offered. It degrades to a
	// device-local decline; the upstream reject happens only when no
	// legitimate receiver remains. A key suppressed by its own harassment
	// policy can never hang up (or even observe) that caller's call.
	if call.State == StateRinging {
		if m.crossKeyRingingRemains(ctx, call.LineID, call.Peer, deviceID) {
			return m.Decline(ctx, deviceID, callID)
		}
	} else if strings.HasPrefix(call.State, "voicemail") {
		access, accessErr := m.Store.DeviceAccess(ctx, deviceID)
		if accessErr == nil && m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindCall, call.Peer, "") {
			return ErrForbidden
		}
	}
	return m.hangupCall(ctx, call, "hungup")
}

func (m *Manager) HangupAsSystem(ctx context.Context, callID, reason string) error {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return ErrCallGone
	}
	if call.EndedAt != nil {
		return nil
	}
	return m.hangupCall(ctx, call, reason)
}

func (m *Manager) hangupCall(ctx context.Context, call store.Call, reason string) error {
	line := m.Line(call.LineID)
	if line != nil {
		if err := line.Client.Hangup(ctx, call.WorkerCallID, randomKey()); err != nil && !isWorkerGone(err) {
			slog.Warn("worker hangup failed", "call_id", call.ID, "error", err)
		}
	}
	m.finishCall(call, reason)
	return nil
}

func (m *Manager) finishCall(call store.Call, reason string) {
	m.cancelVoicemailTimer(call.ID)
	// Explicit/remote/system end is terminal: retire any bounded recovery
	// window immediately. A stale deadline callback cannot end a recovered
	// call, and a deadline must never fire for a deliberately hung-up call.
	m.cancelRecovery(call.ID)
	// Terminal calls cancel any in-flight probe: promotion decisions die
	// with the call.
	m.DiscardProbe(call.ID)
	// Re-read first: while this request was in flight the worker event
	// stream may already have recorded the true connectedAt (observed live:
	// a stale hangup snapshot overwrote answered_at back to NULL and emitted
	// a second, inconsistent call.ended).
	if current, err := m.Store.GetCall(context.Background(), call.ID); err == nil {
		call = current
	}
	endReason := call.EndReason
	if endReason == "" {
		endReason = reason
	}
	endedAt := time.Now().UTC()
	// Atomic terminal claim: exactly one finisher (HTTP path, worker event
	// path, policy reject) publishes call.ended; losers only run the
	// idempotent media/convergence cleanup. A claim failure must never fall
	// back to a full-row upsert: the carried snapshot can predate another
	// finisher's terminal claim (or even the answer) and would resurrect /
	// rewrite terminal truth. Re-read instead so the rest of the cleanup and
	// any published view reflect durable state.
	won, err := m.Store.FinishCallIfOpen(context.Background(), call.ID, endedAt, endReason, call.AnsweredAt)
	if err != nil {
		slog.Error("terminal claim failed", "call_id", call.ID, "error", err)
		if current, getErr := m.Store.GetCall(context.Background(), call.ID); getErr == nil {
			call = current
		}
		m.endCallMedia(call.ID)
		m.afterLegEnded(context.Background(), call)
		return
	}
	if won {
		// Re-read for the terminal view: connectedAt can have landed after
		// this function's snapshot while the claim was in flight.
		if current, getErr := m.Store.GetCall(context.Background(), call.ID); getErr == nil {
			call = current
		}
		call.State = StateEnded
		call.EndedAt = &endedAt
		call.EndReason = endReason
		m.publish(call.LineID, "", "call.ended", m.view(call))
	}
	m.endCallMedia(call.ID)
	m.afterLegEnded(context.Background(), call)
}

func (m *Manager) SetHeld(ctx context.Context, deviceID, callID string, held bool) (CallView, error) {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return CallView{}, ErrCallGone
	}
	_, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return CallView{}, err
	}
	if !callAllowed(entry, call, deviceID) {
		return CallView{}, ErrForbidden
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return CallView{}, ErrForbidden
	}
	if call.EndedAt != nil || call.State == StateRinging || strings.HasPrefix(call.State, "voicemail") {
		return CallView{}, ErrCallGone
	}
	call.Held = held
	if held {
		call.State = StateHeld
	} else {
		call.State = StateActive
	}
	// Preserve a conference merge that landed after the pre-flight read:
	// a per-call hold snapshot must never strip conference_id from a merged
	// leg (its media lives on the conference session).
	if fresh, err := m.Store.GetCall(ctx, callID); err == nil {
		call.ConferenceID = fresh.ConferenceID
	}
	// Conditional write: never resurrect a call that ended after the
	// pre-flight GetCall, and never move answered_at/owner backwards.
	if err := m.Store.UpsertLiveCall(ctx, call); err != nil {
		if errors.Is(err, store.ErrCallAlreadyEnded) {
			return CallView{}, ErrCallGone
		}
		return CallView{}, err
	}
	if session := m.session(callID); session != nil {
		if leg := session.Leg(callID); leg != nil {
			leg.SetHeld(held)
		}
	}
	view := m.view(call)
	m.publish(call.LineID, deviceID, "call.updated", view)
	return view, nil
}

func (m *Manager) DTMF(ctx context.Context, deviceID, callID, digit, idempotencyKey string) error {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return ErrCallGone
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return ErrForbidden
	}
	_, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return err
	}
	if !callAllowed(entry, call, deviceID) {
		return ErrForbidden
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

// ---------- client media ----------

// Offer negotiates the call's client media. trickle=true marks an
// incremental (build-68) offer: the answer is identical, but the session
// mints an attach token the client uses to post its remaining ICE
// candidates (AdoptCallCandidates). Full-SDP clients keep trickle=false and
// the response carries no token.
func (m *Manager) Offer(ctx context.Context, deviceID, callID, offerSDP string, denoise DenoiseChoice, trickle bool) (map[string]any, error) {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return nil, ErrCallGone
	}
	_, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return nil, err
	}
	if !callAllowed(entry, call, deviceID) {
		return nil, ErrForbidden
	}
	if call.EndedAt != nil {
		return nil, ErrCallGone
	}
	if call.State == StateRinging {
		return nil, fail(409, "CB-V2-409B", "请先接听")
	}
	if strings.HasPrefix(call.State, "voicemail") {
		return nil, fail(409, "CB-V2-409F", "留言进行中，不能接入媒体")
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return nil, ErrForbidden
	}
	if call.ConferenceID != "" {
		// Same guard and ordering as the WSS media route: a merged leg's
		// audio belongs to the conference session, and a per-call WebRTC
		// offer would double-attach / steal the worker's single-owner media
		// socket.
		return nil, fail(409, "CB-V2-409D", "通话正在会议中，请使用会议媒体通道")
	}
	if strings.TrimSpace(offerSDP) == "" {
		return nil, fail(400, "CB-V2-400", "缺少 SDP offer")
	}
	session := m.session(callID)
	if session == nil {
		session = media.NewSession(m.Engine, callID, "call")
		session.SetCallbacks(m.onHostLost, m.onHostRestored, m.onLegClosed)
		m.setSession(callID, session)
	}
	if session.LegCount() == 0 {
		if leg := m.takeParkedLeg(callID); leg != nil {
			leg.SetHeld(call.Held)
			session.AddLeg(leg)
		} else if err := m.attachLineLeg(ctx, session, call); err != nil {
			return nil, err
		}
	}
	// Apply (or refresh on re-offer) the device's directional denoise
	// policy once the session/leg exist, before the answer returns, so the
	// first mixed frames use it.
	m.applyDenoise(ctx, deviceID, session, denoise)
	iceServers, policy := m.ICEConfiguration()
	attach, err := session.AttachClientTrickle(ctx, offerSDP, iceServers, trickle)
	if err != nil {
		return nil, fail(502, "CB-V2-502", "媒体协商失败")
	}
	response := map[string]any{"sdp": attach.SDP, "type": "answer", "iceMode": policy, "callId": callID}
	if trickle {
		// The answer was written BEFORE the gateway's gathering finished: the
		// client must posts its own remaining candidates to
		// /calls/{id}/webrtc/candidates and poll the gateway's from
		// GET /calls/{id}/webrtc/candidates until complete.
		response["trickle"] = true
		response["attachId"] = attach.AttachToken
		response["candidateCursor"] = attach.CandidateCursor
		response["gatheringComplete"] = attach.GatheringComplete
	}
	return response, nil
}

// AdoptCallCandidates applies one incremental candidate batch to the call's
// CURRENT client attachment. The attach token pins the negotiation: a batch
// whose token no longer matches (a newer offer replaced the peer connection)
// is reported stale and changes nothing. Every guard the media attach itself
// enforces is re-checked first, so a token can never be used to reach a call
// the device is not allowed to touch.
func (m *Manager) AdoptCallCandidates(ctx context.Context, deviceID, callID, attachToken string, candidates []string) (int, error) {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return 0, ErrCallGone
	}
	_, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return 0, err
	}
	if !callAllowed(entry, call, deviceID) {
		return 0, ErrForbidden
	}
	if call.EndedAt != nil {
		return 0, ErrCallGone
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return 0, ErrForbidden
	}
	if call.ConferenceID != "" {
		return 0, fail(409, "CB-V2-409D", "通话正在会议中，请使用会议媒体通道")
	}
	session := m.session(callID)
	if session == nil {
		return 0, fail(404, "CB-V2-404", "媒体尚未建立")
	}
	added, err := session.AddClientCandidates(attachToken, candidates)
	if err != nil {
		return 0, fail(409, "CB-V2-409G", "候选已过期，请重新协商媒体")
	}
	return added, nil
}

// AdoptProbeCandidates applies an incremental candidate batch to the call's
// detached measurement probe (same fencing as the probe attach itself).
func (m *Manager) AdoptProbeCandidates(ctx context.Context, deviceID, callID, attachToken string, candidates []string) (int, error) {
	if err := m.CheckWSMediaAccess(ctx, deviceID, callID); err != nil {
		return 0, err
	}
	m.mu.Lock()
	entry := m.probes[callID]
	m.mu.Unlock()
	if entry == nil {
		return 0, fail(404, "CB-V2-404", "探测已结束")
	}
	added, err := entry.session.AddClientCandidates(attachToken, candidates)
	if err != nil {
		return 0, fail(409, "CB-V2-409G", "候选已过期，请重新探测")
	}
	return added, nil
}

// AdoptPreflightCandidates applies an incremental candidate batch to the
// device's call-independent preflight probe. The device-scoped preflight id
// AND the attach token must both match, so a stale client can never inject
// candidates into a newer preflight of the same device.
func (m *Manager) AdoptPreflightCandidates(ctx context.Context, deviceID, preflightID, attachToken string, candidates []string) (int, error) {
	_ = ctx
	if strings.TrimSpace(preflightID) == "" {
		return 0, fail(400, "CB-V2-400", "缺少 preflightId")
	}
	m.mu.Lock()
	entry := m.preflights[deviceID]
	var session *media.Session
	if entry != nil && entry.id == preflightID && entry.probe != nil {
		session = entry.probe.session
	}
	m.mu.Unlock()
	if session == nil {
		return 0, fail(404, "CB-V2-404", "预检候选已过期")
	}
	added, err := session.AddClientCandidates(attachToken, candidates)
	if err != nil {
		return 0, fail(409, "CB-V2-409G", "候选已过期，请重新探测")
	}
	return added, nil
}

func (m *Manager) Ice(ctx context.Context, deviceID, callID string) (map[string]any, error) {
	if callID != "" {
		call, err := m.Store.GetCall(ctx, callID)
		if err != nil {
			return nil, ErrCallGone
		}
		if _, _, _, err := m.requireLine(ctx, deviceID, call.LineID, nil); err != nil {
			return nil, err
		}
	}
	servers, policy := m.ICEConfiguration()
	response := make([]map[string]any, 0, len(servers))
	for _, server := range servers {
		response = append(response, map[string]any{"urls": server.URLs, "username": server.Username, "credential": server.Credential})
	}
	// mediaTransports advertises the client-facing audio paths this gateway
	// serves: "ice" (SDP offer/answer WebRTC) and "ws" (authenticated WSS
	// PCMU on this same HTTPS route — the only path reachable for cellular
	// clients when no public ICE candidate exists). Older clients ignore the
	// field and keep using ICE; new clients prefer "ws" when present.
	// Set once before Manager.Start and never mutated; a benign race here
	// is cheaper than nesting the manager lock inside requireLine paths.
	provider := m.quicCapabilityProvider
	transports := []string{"ice", "ws"}
	capabilities := map[string]any{"audioDenoise": []string{"uplink", "downlink"}, "mediaStandby": true,
		// iceTrickle (build 68): the gateway accepts an incremental client
		// offer (offer posted as soon as the first candidates exist) plus
		// bounded, attach-token-fenced candidate batches on
		// /calls/{id}/webrtc/candidates, /calls/{id}/media/probe/candidates
		// and /media/probe/candidates. Clients MUST fall back to full SDP
		// when this flag is absent.
		"iceTrickle": true}
	if provider != nil {
		if info, ok := provider(); ok {
			transports = append(transports, "quic")
			capabilities["mediaQuic"] = true
			capabilities["quic"] = info
		}
	}
	return map[string]any{
		"policy":          policy,
		"iceServers":      response,
		"mediaTransports": transports,
		// capabilities (build 48): new clients detect SpeexDSP directional
		// denoise support and attach the optional parameters; old clients
		// ignore this field entirely. mediaStandby (build 50) advertises the
		// hot-standby attach/promote protocol; clients MUST NOT use any
		// standby feature unless it is present.
		"capabilities": capabilities,
	}, nil
}

// CheckConferenceMediaGuard rejects per-call media attach for a call merged
// into a conference: the leg's media belongs to the conference session, and
// a per-call attach would double-attach the worker's single-owner socket.
// Call it before the HTTP upgrade so the rejection is a plain 409.
func (m *Manager) CheckConferenceMediaGuard(ctx context.Context, callID string) error {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil || call.EndedAt != nil {
		return ErrCallGone
	}
	if call.ConferenceID != "" {
		return fail(409, "CB-V2-409D", "通话正在会议中，请使用会议媒体通道")
	}
	return nil
}

// CheckWSMediaAccess enforces the WS media guards before an HTTP upgrade so
// rejections stay plain HTTP errors. ServeWSMedia re-verifies them.
func (m *Manager) CheckWSMediaAccess(ctx context.Context, deviceID, callID string) error {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return ErrCallGone
	}
	_, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return err
	}
	if !callAllowed(entry, call, deviceID) {
		return ErrForbidden
	}
	if call.EndedAt != nil {
		return ErrCallGone
	}
	if call.State == StateRinging {
		return fail(409, "CB-V2-409B", "请先接听")
	}
	if strings.HasPrefix(call.State, "voicemail") {
		return fail(409, "CB-V2-409F", "留言进行中，不能接入媒体")
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return ErrForbidden
	}
	return nil
}

// CallLocalCandidates hands out the gateway's incrementally gathered
// candidates for the call's current client attachment (same guards and same
// attach fencing as the intake path).
func (m *Manager) CallLocalCandidates(ctx context.Context, deviceID, callID, attachToken string, after int) (media.CandidateBatch, error) {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return media.CandidateBatch{}, ErrCallGone
	}
	_, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return media.CandidateBatch{}, err
	}
	if !callAllowed(entry, call, deviceID) {
		return media.CandidateBatch{}, ErrForbidden
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return media.CandidateBatch{}, ErrForbidden
	}
	if call.ConferenceID != "" {
		return media.CandidateBatch{}, fail(409, "CB-V2-409D", "通话正在会议中，请使用会议媒体通道")
	}
	session := m.session(callID)
	if session == nil {
		return media.CandidateBatch{}, fail(404, "CB-V2-404", "媒体尚未建立")
	}
	batch, err := session.LocalCandidates(attachToken, after)
	if err != nil {
		return media.CandidateBatch{}, candidateBatchFailure(err, "候选已过期，请重新协商媒体")
	}
	return batch, nil
}

// ProbeLocalCandidates is the same poll for the call's detached probe.
func (m *Manager) ProbeLocalCandidates(ctx context.Context, deviceID, callID, attachToken string, after int) (media.CandidateBatch, error) {
	if err := m.CheckWSMediaAccess(ctx, deviceID, callID); err != nil {
		return media.CandidateBatch{}, err
	}
	m.mu.Lock()
	entry := m.probes[callID]
	m.mu.Unlock()
	if entry == nil {
		return media.CandidateBatch{}, fail(404, "CB-V2-404", "探测已结束")
	}
	batch, err := entry.session.LocalCandidates(attachToken, after)
	if err != nil {
		return media.CandidateBatch{}, candidateBatchFailure(err, "候选已过期，请重新探测")
	}
	return batch, nil
}

// PreflightLocalCandidates is the same poll for the device-scoped preflight.
func (m *Manager) PreflightLocalCandidates(ctx context.Context, deviceID, preflightID, attachToken string, after int) (media.CandidateBatch, error) {
	_ = ctx
	if strings.TrimSpace(preflightID) == "" {
		return media.CandidateBatch{}, fail(400, "CB-V2-400", "缺少 preflightId")
	}
	m.mu.Lock()
	entry := m.preflights[deviceID]
	var session *media.Session
	if entry != nil && entry.id == preflightID && entry.probe != nil {
		session = entry.probe.session
	}
	m.mu.Unlock()
	if session == nil {
		return media.CandidateBatch{}, fail(404, "CB-V2-404", "预检候选已过期")
	}
	batch, err := session.LocalCandidates(attachToken, after)
	if err != nil {
		return media.CandidateBatch{}, candidateBatchFailure(err, "候选已过期，请重新探测")
	}
	return batch, nil
}

// candidateBatchFailure maps media-level candidate errors onto the wire error
// shape: a stale attach is a 409 (the client renegotiates), a throttled poll
// is a retryable 429.
func candidateBatchFailure(err error, staleMessage string) error {
	if errors.Is(err, media.ErrAttachPollThrottled) {
		return fail(429, "CB-V2-429", "请求过于频繁，请稍后再试")
	}
	return fail(409, "CB-V2-409G", staleMessage)
}

// ---------- detached media probe (safe P2P candidate measurement) ----------

// ProbeTTL bounds a detached probe's life; a var so tests can compress it.
var ProbeTTL = 45 * time.Second

// HasProbe reports whether a detached probe currently exists for a call.
func (m *Manager) HasProbe(callID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.probes[callID] != nil
}

// AttachProbe creates a DETACHED echo session for a call: it is never
// registered in the Manager's session table, never grows line legs, never
// touches call state and never opens the worker's media socket — so probing
// a direct candidate cannot disturb the healthy media path (WSS) the call
// currently uses. The client reflects frames and measures a genuine
// end-to-end round trip to compare against the WSS path's ping RTT.
func (m *Manager) AttachProbe(ctx context.Context, deviceID, callID, offerSDP string, trickle bool) (map[string]any, error) {
	if err := m.CheckWSMediaAccess(ctx, deviceID, callID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(offerSDP) == "" {
		return nil, fail(400, "CB-V2-400", "缺少 SDP offer")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fail(410, CodeLineUnavailable, "网关正在关闭")
	}
	if existing := m.probes[callID]; existing != nil {
		existing.timer.Stop()
		existing.session.Close()
	}
	probe := media.NewSession(m.Engine, "probe:"+callID, "probe")
	probe.SetProbeEcho(true)
	entry := &probeEntry{session: probe}
	m.probes[callID] = entry
	m.mu.Unlock()
	// Bounded probe life: a stale probe (client vanished mid-measurement)
	// must never outlive its usefulness or leak the peer connection.
	entry.timer = time.AfterFunc(ProbeTTL, func() { m.discardProbe(callID, probe) })
	iceServers, policy := m.ICEConfiguration()
	attach, err := probe.AttachClientTrickle(ctx, offerSDP, iceServers, trickle)
	if err != nil {
		entry.timer.Stop()
		m.discardProbe(callID, probe)
		return nil, fail(502, "CB-V2-502", "媒体协商失败")
	}
	response := map[string]any{"sdp": attach.SDP, "type": "answer", "iceMode": policy, "callId": callID}
	if trickle {
		response["trickle"] = true
		response["attachId"] = attach.AttachToken
		response["candidateCursor"] = attach.CandidateCursor
		response["gatheringComplete"] = attach.GatheringComplete
	}
	return response, nil
}

// ---------- call-independent direct-path preflight ----------

// randomPreflightID mints an unguessable probe identifier for the
// device-scoped preflight registry.
func randomPreflightID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("prb_%d", time.Now().UnixNano())
	}
	return "prb_" + hex.EncodeToString(buf[:])
}

// AttachPreflight creates a DETACHED echo probe scoped to the DEVICE, with
// no call: a foreground client measures the direct path before any call
// exists so dial/answer route selection can start from fresh measurements.
// It never touches call state, never opens line legs and never captures
// audio — the bounded `callrelay-probe` data channel alone answers "is a
// direct path reachable, and how fresh is its RTT". A data-channel echo
// does NOT prove the audio pipeline is ready; the live call still stages
// the chosen transport exactly as before. Bounded by ProbeTTL like call
// probes, superseding any stale preflight for the same device.
func (m *Manager) AttachPreflight(ctx context.Context, deviceID, offerSDP string, trickle bool) (map[string]any, error) {
	if strings.TrimSpace(offerSDP) == "" {
		return nil, fail(400, "CB-V2-400", "缺少 SDP offer")
	}
	if _, err := m.Store.DeviceAccess(ctx, deviceID); err != nil {
		return nil, fail(401, "CB-V2-401", "登录状态已失效")
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fail(410, CodeLineUnavailable, "网关正在关闭")
	}
	if existing := m.preflights[deviceID]; existing != nil {
		existing.probe.timer.Stop()
		existing.probe.session.Close()
		delete(m.preflights, deviceID)
	}
	probe := media.NewSession(m.Engine, "preflight:"+deviceID, "probe")
	probe.SetProbeEcho(true)
	// Publish the entry and arm its generation-fenced expiry callback under
	// the registry lock: the lease (id, probe, gen, deadline) and the
	// callback that reclaims exactly that generation must become visible
	// atomically. Arming outside the lock would leave a window where a
	// renewal bumps gen while probe.timer is still nil, so no timer would
	// ever reclaim the extended lease.
	entry := m.registerPreflightLocked(deviceID, probe)
	preflightID := entry.id
	m.mu.Unlock()
	iceServers, policy := m.ICEConfiguration()
	attach, err := probe.AttachClientTrickle(ctx, offerSDP, iceServers, trickle)
	if err != nil {
		entry.probe.timer.Stop()
		m.discardPreflight(deviceID, preflightID, probe)
		return nil, fail(502, "CB-V2-502", "媒体协商失败")
	}
	slog.Info("media preflight probe attached", "device", deviceID, "policy", policy, "trickle", trickle)
	response := map[string]any{"sdp": attach.SDP, "type": "answer", "iceMode": policy, "preflightId": preflightID,
		"ttlMs": ProbeTTL.Milliseconds()}
	if trickle {
		response["trickle"] = true
		response["attachId"] = attach.AttachToken
		response["candidateCursor"] = attach.CandidateCursor
		response["gatheringComplete"] = attach.GatheringComplete
	}
	return response, nil
}

// registerPreflightLocked publishes a fresh device-scoped preflight lease and
// arms its expiry callback. Caller must hold m.mu. The callback captures the
// generation current at arm time; expirePreflight only reclaims while that
// generation is still registered, so a renewal that bumps the generation can
// never be clobbered by the old callback (time.Timer.Reset cannot cancel a
// callback that already fired or is blocked on m.mu).
func (m *Manager) registerPreflightLocked(deviceID string, probe *media.Session) *preflightEntry {
	preflightID := randomPreflightID()
	pe := &probeEntry{session: probe}
	generation := uint64(1)
	pe.timer = time.AfterFunc(ProbeTTL, func() {
		m.expirePreflight(deviceID, preflightID, probe, generation)
	})
	entry := &preflightEntry{
		id: preflightID, probe: pe,
		gen: generation, deadline: time.Now().Add(ProbeTTL),
	}
	m.preflights[deviceID] = entry
	return entry
}

// DiscardPreflight closes a device-scoped preflight probe (idempotent).
// Explicit discard is intentionally UNCONDITIONAL with respect to the lease
// generation: when the id still names the registered candidate the client's
// cancel must win, regardless of how many renewals have extended the lease.
func (m *Manager) DiscardPreflight(deviceID, preflightID string) {
	m.mu.Lock()
	entry := m.preflights[deviceID]
	owned := entry != nil && entry.id == preflightID
	if owned {
		delete(m.preflights, deviceID)
		entry.probe.timer.Stop()
	}
	m.mu.Unlock()
	if owned {
		entry.probe.session.Close()
	}
}

// discardPreflight is the explicit (non-TTL) reclaim used by attach failure
// and supersession paths: it closes the probe only while it is still the
// exact registered candidate. Like DiscardPreflight it is unconditional with
// respect to the lease generation — identity (id + session) is the authority.
func (m *Manager) discardPreflight(deviceID, preflightID string, probe *media.Session) {
	m.mu.Lock()
	entry := m.preflights[deviceID]
	owned := entry != nil && entry.id == preflightID && entry.probe.session == probe
	if owned {
		delete(m.preflights, deviceID)
		entry.probe.timer.Stop()
	}
	m.mu.Unlock()
	if owned {
		probe.Close()
	}
}

// expirePreflight is the TTL reclaim for ONE lease generation. The generation
// is captured when the callback is armed; the entry is reclaimed only while
// that generation is still registered. A renewal bumps the generation under
// m.mu, so an expiry callback that already fired (or is blocked on m.mu) from
// before the renewal can never delete or close the extended lease — the
// exact race time.Timer.Reset cannot fix on its own. Entries removed by
// discard or commit no longer match, so expiry stays a no-op after those
// explicit paths own the teardown.
func (m *Manager) expirePreflight(deviceID, preflightID string, probe *media.Session, generation uint64) {
	m.mu.Lock()
	entry := m.preflights[deviceID]
	owned := entry != nil && entry.id == preflightID && entry.probe.session == probe &&
		entry.gen == generation
	if owned {
		delete(m.preflights, deviceID)
		entry.probe.timer.Stop()
	}
	m.mu.Unlock()
	if owned {
		probe.Close()
	}
}

// RenewPreflight extends the server TTL of a device-scoped preflight probe
// while the SAME attached candidate is still registered for the device. A
// foreground client keeps one measured, connected candidate continuously
// adoptable instead of renegotiating a new peer connection every TTL window;
// the echo/data-channel state, measured RTT history and preflight id are all
// preserved. The id is unchanged, so an in-flight commit still claims the
// same entry. Unknown/superseded ids return an honest 404 and the client
// falls back to a fresh attach.
//
// Lease fence: a renewal is accepted only while the CURRENT deadline has not
// passed. Once a lease deadline has passed its expiry callback has fired (or
// will, regardless of timer churn) and owns the teardown; renewing then would
// let the 200 response be followed immediately by the old callback deleting
// the candidate. Each accepted renewal bumps gen and arms a brand-new timer
// whose callback captures that generation, so a callback from any earlier
// generation (including one already fired and blocked on m.mu) becomes a
// no-op.
func (m *Manager) RenewPreflight(deviceID, preflightID string) (int64, error) {
	if strings.TrimSpace(preflightID) == "" {
		return 0, fail(400, "CB-V2-400", "缺少 preflightId")
	}
	now := time.Now()
	m.mu.Lock()
	entry := m.preflights[deviceID]
	if entry == nil || entry.id != preflightID {
		m.mu.Unlock()
		return 0, fail(404, "CB-V2-404", "预检候选已过期")
	}
	if !now.Before(entry.deadline) {
		// The lease already lapsed: never resurrect it. The callback armed
		// for the current generation reclaims the entry (renewed or not, a
		// lapsed lease cannot be extended), and the client re-attaches.
		m.mu.Unlock()
		return 0, fail(404, "CB-V2-404", "预检候选已过期")
	}
	entry.gen++
	entry.deadline = now.Add(ProbeTTL)
	// Re-arm a NEW timer whose callback captures the new generation: a
	// time.Timer's function is fixed at creation, so Reset would merely fire
	// the OLD closure (with the stale generation) at the new deadline and the
	// extended lease would never be reaped. Stop the old handle (best effort;
	// a callback already running is fenced by the generation bump) and
	// replace it.
	if entry.probe.timer != nil {
		entry.probe.timer.Stop()
	}
	leaseID := entry.id
	leaseProbe := entry.probe.session
	generation := entry.gen
	entry.probe.timer = time.AfterFunc(ProbeTTL, func() {
		m.expirePreflight(deviceID, leaseID, leaseProbe, generation)
	})
	ttlMs := ProbeTTL.Milliseconds()
	m.mu.Unlock()
	return ttlMs, nil
}

// HasPreflight reports whether a device-scoped preflight entry is currently
// registered. Registry bookkeeping only (no peer state change); used by
// lifecycle tests and diagnostics.
func (m *Manager) HasPreflight(deviceID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.preflights[deviceID] != nil
}

// claimPreflight removes a device-scoped preflight from the registry when
// the id matches; on success the caller owns the probe entry (its TTL is
// stopped by the commit path exactly like a call probe's).
func (m *Manager) claimPreflight(deviceID, preflightID string) *probeEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.preflights[deviceID]
	if entry == nil || entry.id != preflightID {
		return nil
	}
	delete(m.preflights, deviceID)
	return entry.probe
}

// CommitProbe promotes the call's measured probe into the live media path:
// the already-connected peer connection moves into the call's session
// atomically and the previous host (WSS) is detached. Safe by construction —
// the candidate is re-verified connected at commit time before the healthy
// transport is touched; on any failure the previous path keeps carrying
// audio and the call reports an explicit failure.
// CommitProbe promotes the call's measured probe into the live media path.
// When preflightID is non-empty the candidate is claimed from the device's
// call-independent preflight registry instead (fresh foreground measurement
// reused for this call); either way the probe leaves its registry before any
// validation below, so expiry can never race the commit, two concurrent
// commits can never adopt the same peer connection, and every commit attempt
// consumes the probe uniformly (a rejection leaves the previous active path
// live and returns an explicit failure; the client re-probes for a later
// attempt).
func (m *Manager) CommitProbe(ctx context.Context, deviceID, callID, preflightID string, asStandby bool) error {
	if err := m.CheckWSMediaAccess(ctx, deviceID, callID); err != nil {
		return err
	}
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil || call.EndedAt != nil {
		return ErrCallGone
	}
	var entry *probeEntry
	if preflightID != "" {
		// A stale or unknown preflight id is a normal race (TTL expiry,
		// backgrounding): the client falls back to a fresh call-scoped
		// probe, so 404 is honest and sufficient.
		if entry = m.claimPreflight(deviceID, preflightID); entry == nil {
			return fail(404, "CB-V2-404", "预检候选已过期")
		}
	} else {
		m.mu.Lock()
		entry = m.probes[callID]
		if entry != nil {
			delete(m.probes, callID)
		}
		m.mu.Unlock()
	}
	if entry == nil {
		return fail(404, "CB-V2-404", "没有可提交的直接媒体候选")
	}
	entry.timer.Stop()
	probe := entry.session
	if call.ConferenceID != "" {
		// A conferenced call's leg lives on the conference session; adopting
		// a per-call peer here would double-attach the worker's single-owner
		// media socket. Reject loudly instead of breaking conference audio.
		probe.Close()
		return fail(409, "CB-V2-409D", "通话正在会议中，请先拆分会议再切换媒体路径")
	}
	live := m.session(callID)
	if live == nil {
		live = media.NewSession(m.Engine, callID, "call")
		live.SetCallbacks(m.onHostLost, m.onHostRestored, m.onLegClosed)
		m.setSession(callID, live)
	}
	if live.LegCount() == 0 {
		if leg := m.takeParkedLeg(callID); leg != nil {
			leg.SetHeld(call.Held)
			live.AddLeg(leg)
		} else if err := m.attachLineLeg(ctx, live, call); err != nil {
			probe.Close()
			return err
		}
	}
	// Terminal re-validation immediately before adoption: the call may have
	// ended or been merged into a conference while the probe was claimed.
	// The closed-session fence inside AdoptClientPeer is the last line of
	// defense — an ended call's media is torn down and can never be
	// resurrected by a late adoption.
	current, err := m.Store.GetCall(ctx, callID)
	if err != nil || current.EndedAt != nil {
		probe.Close()
		return ErrCallGone
	}
	if current.ConferenceID != "" {
		probe.Close()
		return fail(409, "CB-V2-409D", "通话正在会议中，请先拆分会议再切换媒体路径")
	}
	if err := live.AdoptClientPeer(probe, asStandby); err != nil {
		// The candidate was unusable: drop it and leave the previous host
		// (WSS or ICE) carrying audio untouched.
		probe.Close()
		return fail(502, "CB-V2-502", "候选媒体接管失败: "+err.Error())
	}
	slog.Info("probe committed to live media path", "call_id", callID, "device", deviceID, "standby", asStandby)
	return nil
}

// DiscardProbe closes the call's probe, if it is still the registered one.
func (m *Manager) DiscardProbe(callID string) {
	m.mu.Lock()
	entry := m.probes[callID]
	if entry != nil {
		delete(m.probes, callID)
		entry.timer.Stop()
	}
	m.mu.Unlock()
	if entry != nil {
		entry.session.Close()
	}
}

// discardProbe is the TTL/attach-replacement reclaim: it closes the probe
// only when the session is still the registered one, so a commit that
// already claimed the entry never has its adopted peer torn down here.
func (m *Manager) discardProbe(callID string, probe *media.Session) {
	m.mu.Lock()
	entry := m.probes[callID]
	owned := entry != nil && entry.session == probe
	if owned {
		delete(m.probes, callID)
		entry.timer.Stop()
	}
	m.mu.Unlock()
	if owned {
		probe.Close()
	}
}

// ServeWSMedia attaches an authenticated WebSocket as the client/host audio
// transport for one call. Permission and ownership guards mirror Offer; the
// line leg is attached exactly like the WebRTC path so the mixer, hold,
// conference and grace semantics stay identical. codec is the negotiated wire
// codec from the attach URL ("" or "opus"; anything else is PCMU). It blocks
// until the socket dies or the session ends, then applies the standard
// host-lost lifecycle.
//
// standby=true (build 50, only used when the gateway advertised
// capabilities.mediaStandby) attaches the socket in the hot-standby role
// instead: the existing primary is NOT evicted, the socket receives the same
// downlink mix, and its close is silent. The ready greeting carries
// "standby":true so the client can distinguish acceptance from an old server
// that ignored the flag.
func (m *Manager) ServeWSMedia(ctx context.Context, deviceID, callID, codec string, conn *websocket.Conn, denoise DenoiseChoice, standby bool) error {
	// wsReadyWriteDeadline bounds the ready-greeting write so a dead
	// tunnel cannot wedge the session before its pump and host-lost grace
	// exist.
	const wsReadyWriteDeadline = 5 * time.Second
	if codec != "opus" {
		codec = ""
	}
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil {
		return ErrCallGone
	}
	_, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return err
	}
	if !callAllowed(entry, call, deviceID) {
		return ErrForbidden
	}
	if call.EndedAt != nil {
		return ErrCallGone
	}
	if call.State == StateRinging {
		return fail(409, "CB-V2-409B", "请先接听")
	}
	if strings.HasPrefix(call.State, "voicemail") {
		return fail(409, "CB-V2-409F", "留言进行中，不能接入媒体")
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return ErrForbidden
	}
	if call.ConferenceID != "" {
		// A merged leg's media belongs to the conference session; a
		// per-call reattach would double-attach the worker's single-owner
		// media socket and steal the leg's audio. Point the client at the
		// conference media route instead.
		return fail(409, "CB-V2-409D", "通话正在会议中，请使用会议媒体通道")
	}
	session := m.session(callID)
	if session == nil {
		session = media.NewSession(m.Engine, callID, "call")
		session.SetCallbacks(m.onHostLost, m.onHostRestored, m.onLegClosed)
		m.setSession(callID, session)
	}
	if session.LegCount() == 0 {
		if leg := m.takeParkedLeg(callID); leg != nil {
			leg.SetHeld(call.Held)
			session.AddLeg(leg)
		} else if err := m.attachLineLeg(ctx, session, call); err != nil {
			return err
		}
	}
	// Apply the device's denoise policy for this attach before audio flows.
	m.applyDenoise(ctx, deviceID, session, denoise)
	attach := func(negotiated string, info media.AttachInfo) error {
		// Terminal re-check: the call may have ended while the line leg was
		// attaching; a finished call must never be resurrected by a fresh
		// host socket.
		if current, getErr := m.Store.GetCall(context.Background(), callID); getErr == nil && current.EndedAt != nil {
			return ErrCallGone
		}
		ready := map[string]any{"type": "ready", "callId": callID}
		// Echo only the codec the gateway's own host successfully created
		// (newWSHost may have fallen back to PCMU on libopus init failure).
		if negotiated == media.CodecOpus {
			ready["codec"] = "opus"
			ready["fmt"] = media.WSFrameFmtSeq16
		}
		if info.Standby {
			// The client requested standby AND the gateway accepted it into
			// the standby slot: the primary was not evicted. An absent flag
			// means either a primary attach or an old server that ignored
			// the parameter — the client distinguishes by watching its
			// primary transport. The epoch + token fence every later
			// promotion command (coordinator rev 2).
			ready["standby"] = true
			ready["epoch"] = info.Epoch
			ready["token"] = info.Token
		} else {
			ready["epoch"] = info.Epoch
		}
		// A stalled tunnel must not block inside the ready greeting with
		// no pump running and no host-lost grace armed.
		if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsReadyWriteDeadline)); err != nil {
			// Ping is only an opportunistic liveness probe; never fail
			// ready over it.
			slog.Debug("ready pre-write ping skipped", "error", err)
		}
		_ = conn.SetWriteDeadline(time.Now().Add(wsReadyWriteDeadline))
		defer conn.SetWriteDeadline(time.Time{})
		return conn.WriteJSON(ready)
	}
	var generation uint64
	if standby {
		var standbyAccepted bool
		generation, standbyAccepted, err = session.AttachWSStandby(conn, codec, attach)
		if err != nil {
			return err
		}
		slog.Info("ws media attached", "call_id", callID, "device", deviceID, "standby", standbyAccepted)
	} else {
		generation, err = session.AttachWSHost(conn, codec, func(negotiated string, info media.AttachInfo) error {
			return attach(negotiated, info)
		})
		if err != nil {
			return err
		}
		slog.Info("ws media attached", "call_id", callID, "device", deviceID)
	}
	stopPing := media.PingWSHost(conn, 25*time.Second)
	defer stopPing()
	session.PumpWSHost(conn, generation)
	return nil
}

// PromoteStandby promotes the call's standby transport of the given kind to
// the primary role (build-50 hot standby): the atomic role swap happens
// inside the media session; the demoted primary stays attached as the new
// standby. The request must come from the call's owning device, and the call
// must be live — the same guards as any media attach. token/expectEpoch/
// nonce fence the command (see media.Session.PromoteStandby).
func (m *Manager) PromoteStandby(ctx context.Context, deviceID, callID, kind, token string, expectEpoch uint64, nonce string) (media.PromoteOutcome, error) {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil || call.EndedAt != nil {
		return media.PromoteOutcome{}, ErrCallGone
	}
	_, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return media.PromoteOutcome{}, err
	}
	if !callAllowed(entry, call, deviceID) {
		return media.PromoteOutcome{}, ErrForbidden
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return media.PromoteOutcome{}, ErrForbidden
	}
	if call.ConferenceID != "" {
		return media.PromoteOutcome{}, fail(409, "CB-V2-409D", "通话正在会议中，请使用会议媒体通道")
	}
	session := m.session(callID)
	if session == nil {
		return media.PromoteOutcome{}, fail(409, "CB-V2-409", "通话媒体未 attach")
	}
	outcome, err := session.PromoteStandby(kind, token, expectEpoch, nonce)
	if err != nil {
		return media.PromoteOutcome{}, fail(409, "CB-V2-409", "备用通道不可用: "+err.Error())
	}
	slog.Info("standby promoted to primary", "call_id", callID, "device", deviceID,
		"kind", kind, "epoch", outcome.Epoch, "standby", outcome.Standby)
	return outcome, nil
}

// MediaEpoch returns the call session's current primary role epoch (0 when
// no session exists); used to fence promotion commands and to ack commits.
func (m *Manager) MediaEpoch(callID string) uint64 {
	session := m.session(callID)
	if session == nil {
		return 0
	}
	return session.Epoch()
}

// StandbyToken returns the call session's current standby token for the
// given family ("" when none).
func (m *Manager) StandbyToken(callID, kind string) string {
	session := m.session(callID)
	if session == nil {
		return ""
	}
	return session.StandbyToken(kind)
}

// MediaState exposes the call session's transport roles so a client that
// lost a promotion ack (or raced a manual swap) can reconcile against the
// server before issuing another fenced command.
func (m *Manager) MediaState(ctx context.Context, deviceID, callID string) (media.MediaState, error) {
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil || call.EndedAt != nil {
		return media.MediaState{}, ErrCallGone
	}
	_, entry, _, err := m.requireLine(ctx, deviceID, call.LineID, nil)
	if err != nil {
		return media.MediaState{}, err
	}
	// Same authorization guard as PromoteStandby: the reconciler view is as
	// sensitive as the promotion command itself.
	if !callAllowed(entry, call, deviceID) {
		return media.MediaState{}, ErrForbidden
	}
	if call.DeviceID != "" && call.DeviceID != deviceID {
		return media.MediaState{}, ErrForbidden
	}
	session := m.session(callID)
	if session == nil {
		return media.MediaState{}, fail(409, "CB-V2-409", "通话媒体未 attach")
	}
	return session.State(), nil
}

// ServeWSMeasure serves a measurement-only socket: app-level JSON ping/pong
// for sampling the WSS path's RTT while the call's media runs over ANY host
// transport (ICE included). Unlike ServeWSMedia it never replaces, feeds or
// detaches the session's host transport — measurement cannot affect live
// audio — and it holds no session state, so sockets may come and go freely.
// The gateway closes it when the call ends.
func (m *Manager) ServeWSMeasure(ctx context.Context, deviceID, callID string, conn *websocket.Conn) error {
	if err := m.CheckWSMediaAccess(ctx, deviceID, callID); err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	// Lifecycle: the socket dies with the call (polled, ≤2 s) even when the
	// client stops sending.
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				call, err := m.Store.GetCall(context.Background(), callID)
				if err != nil || call.EndedAt != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	conn.SetReadLimit(4096)
	var tokens, last = measureBurst, time.Now()
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return nil
		}
		if messageType != websocket.TextMessage {
			continue
		}
		now := time.Now()
		tokens += int(now.Sub(last).Seconds() * measureRatePerSecond)
		if tokens > measureBurst {
			tokens = measureBurst
		}
		last = now
		if tokens <= 0 {
			continue
		}
		var control struct {
			Type string `json:"type"`
			T    int64  `json:"t"`
		}
		if json.Unmarshal(payload, &control) != nil || control.Type != "ping" {
			continue
		}
		tokens--
		pong, marshalErr := json.Marshal(map[string]any{"type": "pong", "t": control.T})
		if marshalErr != nil {
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(wsMeasureWriteDeadline))
		if err := conn.WriteMessage(websocket.TextMessage, pong); err != nil {
			return nil
		}
	}
}

const (
	measureRatePerSecond   = 50
	measureBurst           = 50
	wsMeasureWriteDeadline = 10 * time.Second
)

// ServeWSRelayProbe serves a device-scoped, call-independent measurement
// socket over the SAME authenticated relay WebSocket transport a call uses:
// JSON ping/pong only, no call, no media session, no audio. The foreground
// app keeps one of these while idle so the route picker can show a MEASURED
// relay RTT (build 0.3.30), not an HTTP guess. It holds no session state, is
// bounded by an absolute lifetime and an idle timeout, and can never touch a
// live call's transport. The first frame is a ready control so the client
// knows the relay path accepted the socket.
func (m *Manager) ServeWSRelayProbe(ctx context.Context, deviceID string, conn *websocket.Conn) error {
	const (
		relayProbeMaxLifetime = 8 * time.Minute
		relayProbeIdleTimeout = 30 * time.Second
	)
	ready := []byte(`{"type":"ready"}`)
	_ = conn.SetWriteDeadline(time.Now().Add(wsMeasureWriteDeadline))
	if err := conn.WriteMessage(websocket.TextMessage, ready); err != nil {
		return nil
	}
	_ = conn.SetWriteDeadline(time.Time{})
	deadline := time.Now().Add(relayProbeMaxLifetime)
	var tokens, last = measureBurst, time.Now()
	for {
		if time.Now().After(deadline) {
			return nil
		}
		_ = conn.SetReadDeadline(time.Now().Add(relayProbeIdleTimeout))
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return nil
		}
		if messageType != websocket.TextMessage {
			continue
		}
		now := time.Now()
		tokens += int(now.Sub(last).Seconds() * measureRatePerSecond)
		if tokens > measureBurst {
			tokens = measureBurst
		}
		last = now
		if tokens <= 0 {
			continue
		}
		var control struct {
			Type string `json:"type"`
			T    int64  `json:"t"`
		}
		if json.Unmarshal(payload, &control) != nil || control.Type != "ping" {
			continue
		}
		tokens--
		pong, marshalErr := json.Marshal(map[string]any{"type": "pong", "t": control.T})
		if marshalErr != nil {
			continue
		}
		_ = conn.SetWriteDeadline(time.Now().Add(wsMeasureWriteDeadline))
		if err := conn.WriteMessage(websocket.TextMessage, pong); err != nil {
			return nil
		}
		_ = conn.SetWriteDeadline(time.Time{})
	}
}

func (m *Manager) attachLineLeg(ctx context.Context, session *media.Session, call store.Call) error {
	line := m.Line(call.LineID)
	if line == nil {
		return fail(410, CodeLineUnavailable, "线路未启用")
	}
	mediaConn, err := line.Client.AttachMedia(ctx, call.WorkerCallID)
	if err != nil {
		return fail(502, "CB-V2-502", "线路媒体通道不可用: "+err.Error())
	}
	// Wait for the worker bridge to verify the cellular leg and open its
	// UAC pair BEFORE greeting the phone. Without this gate the app
	// started its graph against a leg that could stay silent for tens of
	// seconds (route rotation happens inside the worker bridge); a failure
	// here fails the call truthfully instead. The deadline stays under the
	// app's own 15 s ready timeout.
	readyCtx, readyCancel := context.WithTimeout(ctx, workerMediaReadyTimeout)
	readyErr := mediaConn.WaitReady(readyCtx)
	readyCancel()
	if readyErr != nil {
		mediaConn.Close()
		return fail(502, "CB-V2-502", "线路媒体未就绪: "+readyErr.Error())
	}
	leg := media.NewLeg(call.ID, &workerMediaTransport{mediaConn}, m.onLegClosed)
	session.AddLeg(leg)
	return nil
}

// workerMediaReadyTimeout bounds how long the daemon waits for the
// worker bridge to come up after an answer; must stay below the app's
// 15 s handshake timeout.
const workerMediaReadyTimeout = 12 * time.Second

type workerMediaTransport struct {
	conn *worker.Media
}

func (t *workerMediaTransport) Frames() <-chan []byte       { return t.conn.Frames }
func (t *workerMediaTransport) SendPCMU(frame []byte) error { return t.conn.Send(frame) }
func (t *workerMediaTransport) Close()                      { t.conn.Close() }

// ---------- media callbacks (onHostLost/onHostRestored live in
// recovery.go: build-48 bounded recovery episode semantics) ----------

func (m *Manager) onLegClosed(callID string, err error) {
	call, getErr := m.Store.GetCall(context.Background(), callID)
	if getErr != nil || call.EndedAt != nil {
		return
	}
	go func() {
		time.Sleep(1500 * time.Millisecond)
		current, err := m.Store.GetCall(context.Background(), callID)
		if err != nil || current.EndedAt != nil {
			return
		}
		slog.Warn("line media closed; converging call", "call_id", callID, "error", err)
		if line := m.Line(current.LineID); line != nil {
			_ = line.Client.Hangup(context.Background(), current.WorkerCallID, randomKey())
		}
		m.finishCall(current, "media_lost")
	}()
}

// takeParkedLeg moves a leg from the post-split parking area into a session.
func (m *Manager) takeParkedLeg(callID string) *media.Leg {
	m.mu.Lock()
	defer m.mu.Unlock()
	leg := m.parked[callID]
	delete(m.parked, callID)
	return leg
}

func (m *Manager) parkLeg(callID string, leg *media.Leg) {
	if leg == nil {
		return
	}
	leg.SetHeld(true)
	m.mu.Lock()
	m.parked[callID] = leg
	m.mu.Unlock()
}

// afterLegEnded converges a conference when one of its legs ends.
func (m *Manager) afterLegEnded(ctx context.Context, call store.Call) {
	if call.ConferenceID == "" {
		return
	}
	_ = m.Store.RemoveConferenceLeg(ctx, call.ConferenceID, call.ID)
	m.handleConferenceLegChange(ctx, call.ConferenceID)
}

// ---------- helpers ----------

func isWorkerGone(err error) bool {
	var apiErr *worker.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusConflict
	}
	return false
}

func mapWorkerFailure(err error) error {
	var apiErr *worker.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.Status {
		case http.StatusConflict:
			return fail(409, "CB-V2-409C", "线路正忙: "+apiErr.Message)
		case http.StatusForbidden:
			return fail(451, CodePolicyBlocked, apiErr.Message)
		case http.StatusNotFound:
			return ErrCallGone
		default:
			return fail(502, "CB-V2-502", "线路操作失败: "+apiErr.Message)
		}
	}
	return fail(502, "CB-V2-502", "线路操作失败")
}

func turnCredential(secret, username string) string {
	return hmacSHA1Base64(secret, username)
}

var _ = pion.ICEServer{}
