package store

import (
	"context"
	"strings"
)

// Recording preferences (build 55).
//
// Two independent switches must BOTH be on before anything records
// automatically: the gateway-level auto mode (operator setting, default off)
// and the per-device auto preference (default OFF). Manual recording is
// unaffected. Auto starts only after the owning device actually has an
// answered/outgoing call, and a manual stop suppresses auto-restart for that
// same call id.
const (
	recordingAutoKeySuffix = ":recording_auto"
	// RecordingAutoModeSetting is the gateway-level operator setting.
	// Values: off | all | incoming | outgoing.
	RecordingAutoModeSetting = "recording.auto_mode"
)

// DeviceRecordingPrefs is one device's auto-record choice. Set=false means
// the device never stored a value, and the policy default is OFF.
type DeviceRecordingPrefs struct {
	AutoRecord    bool
	AutoRecordSet bool
}

func recordingAutoKey(deviceID string) string {
	return "device:" + deviceID + recordingAutoKeySuffix
}

func (s *Store) DeviceRecordingPrefs(ctx context.Context, deviceID string) DeviceRecordingPrefs {
	var prefs DeviceRecordingPrefs
	if raw, err := s.Setting(ctx, recordingAutoKey(deviceID)); err == nil {
		prefs.AutoRecord, prefs.AutoRecordSet = parseDenoiseFlag(raw, true)
	}
	return prefs
}

// SetDeviceRecordingPrefs persists the device auto preference (nil leaves it
// unchanged).
func (s *Store) SetDeviceRecordingPrefs(ctx context.Context, deviceID string, auto *bool) error {
	if auto == nil {
		return nil
	}
	value := "0"
	if *auto {
		value = "1"
	}
	return s.SetSetting(ctx, recordingAutoKey(deviceID), value)
}

// RecordingAutoMode returns the gateway-level mode. UNSET means permit
// ("all"): the accepted plan makes the DEVICE opt-in the only switch the user
// must find — there is no hidden administrator prerequisite. An explicit
// operator value is honored, including an explicit "off" (then the device
// switch cannot enable auto recording, and the UI says why). Unreadable or
// unrecognized values fail open to "all" and still never record anything
// without the per-device opt-in.
func (s *Store) RecordingAutoMode(ctx context.Context) string {
	raw, err := s.Setting(ctx, RecordingAutoModeSetting)
	if err != nil {
		return "all"
	}
	switch strings.TrimSpace(raw) {
	case "off", "all", "incoming", "outgoing":
		return strings.TrimSpace(raw)
	default:
		return "all"
	}
}

// SetRecordingAutoMode validates and persists the gateway-level mode.
func (s *Store) SetRecordingAutoMode(ctx context.Context, mode string) error {
	switch strings.TrimSpace(mode) {
	case "off", "all", "incoming", "outgoing":
	default:
		return ErrNotFound
	}
	return s.SetSetting(ctx, RecordingAutoModeSetting, strings.TrimSpace(mode))
}
