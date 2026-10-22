package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/quic"
)

// ServeQUICMedia adopts a BOUND, already-single-use-token-authorized QUIC
// audio session as the call's QUIC standby transport (build 61). ALL
// critical control continues on the incumbent's authenticated WSS plane:
// the ready greeting (with the standby token + epoch + transport marker)
// is delivered over the session's primary WSS socket, and promotion uses
// the existing fenced standby-promote command with transport "quic".
// The audio itself is epoch-fenced DATAGRAM envelopes (see
// media.QUICSocket audio-only mode).
//
// ref binds the socket's datagram envelope epoch source to this session
// (epoch bumped on attach/promote; old-generation datagrams drop).
func (m *Manager) ServeQUICMedia(ctx context.Context, deviceID, callID, codec string, qs *media.QUICSocket, standby bool, ref *quic.SessionRef) error {
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
		// Conference legs are attached through the conference media route;
		// a QUIC per-call attach would double-own the leg.
		return fail(409, "CB-V2-409D", "通话正在会议中，请使用会议媒体通道")
	}
	session := m.session(callID)
	if session == nil {
		// QUIC media never creates the session: the incumbent WSS/ICE path
		// owns session lifecycle and its reliable control plane.
		return fail(409, "CB-V2-409C", "媒体会话尚未建立")
	}
	ref.Set(session)

	// Ready greeting over the incumbent WSS primary: the client learns the
	// QUIC standby was accepted (token/epoch fence every later promote).
	attach := func(negotiated string, info media.AttachInfo) error {
		if current, getErr := m.Store.GetCall(context.Background(), callID); getErr == nil && current.EndedAt != nil {
			return ErrCallGone
		}
		ready := map[string]any{
			"type": "ready", "callId": callID,
			"codec": "opus", "fmt": media.WSFrameFmtSeq16,
			"transport": "quic",
		}
		if info.Standby {
			ready["standby"] = true
			ready["epoch"] = info.Epoch
			ready["token"] = info.Token
		} else {
			ready["epoch"] = info.Epoch
		}
		payload, marshalErr := json.Marshal(ready)
		if marshalErr != nil {
			return marshalErr
		}
		if !session.EnqueueTextOnPrimary(payload) {
			return fail(409, "CB-V2-409E", "控制通道不可用")
		}
		return nil
	}
	// Build 63: the QUIC adapter takes the DEDICATED QUIC standby slot —
	// first-class role identity, never anonymously displacing a real WSS
	// standby from the ws-family slot.
	generation, standbyAccepted, err := session.AttachQUICStandby(qs, "opus", attach)
	if err != nil {
		return err
	}
	slog.Info("quic media attached", "call_id", callID, "device", deviceID, "standby", standbyAccepted, "generation", generation)
	session.PumpWSHost(qs, generation)
	return nil
}

// quicCapability is the additive build-61 capability document. nil
// disables QUIC advertisement entirely (older behaviour).
type quicCapability struct {
	enabled bool
	info    map[string]any
}

// SetQUICCapabilityProvider installs the provider the media API uses to
// advertise QUIC (called once at startup by main when the listener
// exists; a nil provider keeps ice+ws only).
func (m *Manager) SetQUICCapabilityProvider(provider func() (map[string]any, bool)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quicCapabilityProvider = provider
}
