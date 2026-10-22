package runtime

import (
	"context"
	"log/slog"

	"callrelay/gateway/internal/unified/media"
)

// DenoiseChoice carries OPTIONAL per-attach denoise parameters (the
// backward-compatible media-attach extension). A nil pointer on the choice
// means "not supplied on this attach": the device's stored preference (or the
// build-49 default-OFF policy) applies. Each field is independent so a
// one-direction parameter never changes the other direction.
type DenoiseChoice struct {
	Uplink   *bool
	Downlink *bool
}

// denoiseDefaultOff is the build-49 policy used only when a device has never
// stored a preference (per principal; there is no cross-device global). The
// build-48 default was ON gated on a human-quality/realtime acceptance test;
// the second review established that authorized human-speech quality evidence
// could NOT be obtained (synthetic fixtures are not acceptance), so the
// honest default is OFF with both switches fully usable. An explicit "0"/"1"
// per-device value always wins.
const denoiseDefaultOff = false

// effectiveDenoise resolves the policy for one device + one attach: stored
// per-device preference (OFF when absent), overridden independently by any
// explicit attach parameter.
func (m *Manager) effectiveDenoise(ctx context.Context, deviceID string, choice DenoiseChoice) (uplink, downlink bool) {
	uplink, downlink = denoiseDefaultOff, denoiseDefaultOff
	prefs := m.Store.DeviceDenoisePrefs(ctx, deviceID)
	if prefs.UplinkSet {
		uplink = prefs.Uplink
	}
	if prefs.DownlinkSet {
		downlink = prefs.Downlink
	}
	if choice.Uplink != nil {
		uplink = *choice.Uplink
	}
	if choice.Downlink != nil {
		downlink = *choice.Downlink
	}
	return uplink, downlink
}

// applyDenoise pushes the effective device policy to one owned media session.
func (m *Manager) applyDenoise(ctx context.Context, deviceID string, session *media.Session, choice DenoiseChoice) {
	if session == nil {
		return
	}
	uplink, downlink := m.effectiveDenoise(ctx, deviceID, choice)
	session.SetDenoise(uplink, downlink)
}

// SetDeviceDenoise persists the device's directional switches and applies them
// live to every session currently owned by that device, so a hot toggle takes
// effect at the next 20 ms frame boundary without rebuilding the audio graph.
// nil directions are left unchanged. Other devices' calls are never touched.
func (m *Manager) SetDeviceDenoise(ctx context.Context, deviceID string, uplink, downlink *bool) error {
	if err := m.Store.SetDeviceDenoisePrefs(ctx, deviceID, uplink, downlink); err != nil {
		return err
	}
	// Resolve the full effective policy once.
	up, down := m.effectiveDenoise(ctx, deviceID, DenoiseChoice{})
	m.mu.Lock()
	sessions := make([]sessionOwner, 0, len(m.sessions))
	for key, session := range m.sessions {
		sessions = append(sessions, sessionOwner{key: key, session: session})
	}
	m.mu.Unlock()
	applied := 0
	for _, item := range sessions {
		if m.sessionOwnedByDevice(ctx, item.key, deviceID) {
			item.session.SetDenoise(up, down)
			applied++
		}
	}
	slog.Info("device denoise preference updated", "device", deviceID,
		"uplink", up, "downlink", down, "live_sessions", applied)
	return nil
}

type sessionOwner struct {
	key     string
	session *media.Session
}

// sessionOwnedByDevice reports whether a call/conference media session belongs
// to the given device. A call is owned by its assigned DeviceID; a conference
// is owned by its host device.
func (m *Manager) sessionOwnedByDevice(ctx context.Context, key, deviceID string) bool {
	if call, err := m.Store.GetCall(ctx, key); err == nil && call.EndedAt == nil {
		return call.DeviceID == deviceID
	}
	if conference, err := m.Store.GetConference(ctx, key); err == nil && conference.EndedAt == nil {
		return conference.HostDeviceID == deviceID
	}
	return false
}
