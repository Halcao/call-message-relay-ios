package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
)

// enrollDevice mirrors testStack.enroll but returns the device id too, so a
// test can own two distinct principals.
func (s *testStack) enrollDevice(t *testing.T, keyID, secret, name string) (token, deviceID string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	message := keyID + "\n" + secret + "\n" + s.identity.ID + "\n" + name
	proof := ed25519.Sign(privateKey, []byte(message))
	body, _ := json.Marshal(map[string]string{
		"enrollmentKey": keyID + "." + secret, "deviceName": name,
		"devicePublicKey": base64.StdEncoding.EncodeToString(publicKey),
		"proof":           base64.StdEncoding.EncodeToString(proof),
	})
	status, response := s.request(http.MethodPost, "/api/v2/enroll", "", "", body)
	if status != http.StatusOK {
		t.Fatalf("enroll status %d: %s", status, response)
	}
	var pair struct {
		DeviceID    string `json:"deviceId"`
		AccessToken string `json:"accessToken"`
	}
	if err := json.Unmarshal(response, &pair); err != nil {
		t.Fatal(err)
	}
	return pair.AccessToken, pair.DeviceID
}

func putPrefs(t *testing.T, s *testStack, token, deviceID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	status, payload := s.request(http.MethodPut, "/api/v2/devices/"+deviceID+"/preferences", token, "", raw)
	out := map[string]any{}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &out)
	}
	return status, out
}

// TestPreferencesAudioOnlyPatchNeverClearsDefaultLine: PATCH presence
// semantics — an audio-only update preserves defaultLineId and a legacy
// defaultLineId-only body preserves the audio switches.
func TestPreferencesAudioOnlyPatchNeverClearsDefaultLine(t *testing.T) {
	s := newTestStack(t)
	keyID, secret := s.createKey(true)
	token, deviceID := s.enrollDevice(t, keyID, secret, "audio-owner")

	// Establish a default line.
	if status, out := putPrefs(t, s, token, deviceID, map[string]any{"defaultLineId": "line1"}); status != http.StatusOK || out["defaultLineId"] != "line1" {
		t.Fatalf("set line: status=%d out=%v", status, out)
	}
	// Audio-only patch: defaultLineId must survive and the switches change.
	status, out := putPrefs(t, s, token, deviceID, map[string]any{"uplinkDenoise": false})
	if status != http.StatusOK || out["defaultLineId"] != "line1" {
		t.Fatalf("audio patch disturbed defaultLineId: status=%d out=%v", status, out)
	}
	if up, _ := out["uplinkDenoise"].(bool); up {
		t.Fatalf("uplinkDenoise should be false, got %v", out["uplinkDenoise"])
	}
	// Build-49 default (never-set downlink) is OFF.
	if down, _ := out["downlinkDenoise"].(bool); down {
		t.Fatal("unset downlink must report the default-off policy")
	}
	// Legacy line-only patch: audio switches must be preserved.
	status, out = putPrefs(t, s, token, deviceID, map[string]any{"defaultLineId": ""})
	if status != http.StatusOK {
		t.Fatalf("line-only patch status %d", status)
	}
	if up, _ := out["uplinkDenoise"].(bool); up {
		t.Fatal("line-only patch must preserve uplinkDenoise=false")
	}
	if down, _ := out["downlinkDenoise"].(bool); down {
		t.Fatal("line-only patch must preserve downlink default-off")
	}
}

// TestPreferencesDenoiseIsolationAcrossDevices: one device's switch never
// changes another principal's resolved preference, and only the owning
// device id can write its own row.
func TestPreferencesDenoiseIsolationAcrossDevices(t *testing.T) {
	s := newTestStack(t)
	// Same enrollment key, two different devices: proves per-DEVICE isolation
	// (not merely per-key) for the audio preference.
	keyID, secret := s.createKey(true)
	tokenA, devA := s.enrollDevice(t, keyID, secret, "device-A")
	tokenB, devB := s.enrollDevice(t, keyID, secret, "device-B")

	// A turns its uplink off.
	if status, _ := putPrefs(t, s, tokenA, devA, map[string]any{"uplinkDenoise": false}); status != http.StatusOK {
		t.Fatalf("A set status %d", status)
	}
	// B never set anything: reads the build-49 default-off policy (and must
	// never inherit A's stored value).
	if status, outB := putPrefs(t, s, tokenB, devB, map[string]any{}); status != http.StatusOK {
		t.Fatalf("B status %d", status)
	} else {
		if up, _ := outB["uplinkDenoise"].(bool); up {
			t.Fatal("device B inherited device A's uplink choice (cross-principal leak)")
		}
	}
	// B cannot write A's preferences.
	if status, _ := putPrefs(t, s, tokenB, devA, map[string]any{"uplinkDenoise": false}); status != http.StatusForbidden {
		t.Fatalf("cross-device write status=%d, want 403", status)
	}
	// A's stored value is unchanged after the forbidden attempt.
	if status, outA := putPrefs(t, s, tokenA, devA, map[string]any{}); status != http.StatusOK {
		t.Fatalf("A re-read status %d", status)
	} else if up, _ := outA["uplinkDenoise"].(bool); up {
		t.Fatal("forbidden cross-device write mutated A's preference")
	}
	// Round-trip reconnect with the same token still sees the saved value.
	if status, outA := putPrefs(t, s, tokenA, devA, map[string]any{}); status != http.StatusOK {
		t.Fatalf("reconnect read %d", status)
	} else if down, _ := outA["downlinkDenoise"].(bool); down {
		t.Fatal("reconnect changed downlink default")
	}
}
