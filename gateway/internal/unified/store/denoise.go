package store

import (
	"context"
	"strings"
)

// Device-scoped directional denoise preferences (build 48).
//
// Isolation: values live ONLY in the generic settings table under
// "device:<id>:..." keys, exactly like the existing notify_mode preference —
// there is no gateway-global audio default key, so one device's switch can
// never affect another principal's calls. A value absent for the device means
// the build-49 policy default (OFF — authorized human-speech quality was not
// established); an explicit "0"/"1" is the device owner's choice and always
// wins. These functions hold no audio content.

const (
	denoiseUplinkKeySuffix   = ":audio_denoise_uplink"
	denoiseDownlinkKeySuffix = ":audio_denoise_downlink"
)

func denoiseKey(deviceID, suffix string) string {
	return "device:" + deviceID + suffix
}

// DeviceDenoise is one device's directional denoise choice. Set=false means
// the device never stored a value (caller applies the policy default).
type DeviceDenoise struct {
	Uplink      bool
	Downlink    bool
	UplinkSet   bool
	DownlinkSet bool
}

// parseDenoiseFlag maps a stored value to a bool + presence.
func parseDenoiseFlag(value string, present bool) (bool, bool) {
	if !present {
		return false, false
	}
	switch strings.TrimSpace(value) {
	case "1":
		return true, true
	case "0":
		return false, true
	default:
		return false, false
	}
}

// DeviceDenoisePrefs resolves one device's directional denoise preferences.
// A missing/garbage row behaves as "unset" (no error); callers apply the
// build-49 default-OFF policy in that case.
func (s *Store) DeviceDenoisePrefs(ctx context.Context, deviceID string) DeviceDenoise {
	var prefs DeviceDenoise
	if up, err := s.Setting(ctx, denoiseKey(deviceID, denoiseUplinkKeySuffix)); err == nil {
		prefs.Uplink, prefs.UplinkSet = parseDenoiseFlag(up, true)
	}
	if down, err := s.Setting(ctx, denoiseKey(deviceID, denoiseDownlinkKeySuffix)); err == nil {
		prefs.Downlink, prefs.DownlinkSet = parseDenoiseFlag(down, true)
	}
	return prefs
}

// SetDeviceDenoisePrefs persists only the provided directions: nil means
// "leave unchanged" (PATCH presence semantics), so an audio-only update never
// touches unrelated device settings.
func (s *Store) SetDeviceDenoisePrefs(ctx context.Context, deviceID string, uplink, downlink *bool) error {
	if uplink != nil {
		value := "0"
		if *uplink {
			value = "1"
		}
		if err := s.SetSetting(ctx, denoiseKey(deviceID, denoiseUplinkKeySuffix), value); err != nil {
			return err
		}
	}
	if downlink != nil {
		value := "0"
		if *downlink {
			value = "1"
		}
		if err := s.SetSetting(ctx, denoiseKey(deviceID, denoiseDownlinkKeySuffix), value); err != nil {
			return err
		}
	}
	return nil
}
