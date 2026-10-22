package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/events"
	"callrelay/gateway/internal/unified/recording"
	"callrelay/gateway/internal/unified/store"
)

// attachRecordingManager wires a real recording manager with injected
// storage probes so the API surface can be exercised without an SD card.
func attachRecordingManager(t *testing.T, stack *testStack) (*recording.Manager, string) {
	t.Helper()
	base := t.TempDir()
	manager, err := recording.NewManager(stack.store, recording.Options{
		Root: filepath.Join(base, "recordings"), MountPoint: base,
		ExpectedDevice: "/dev/mmcblk0", ExpectedFSType: "vfat", MinimumFreeBytes: 1,
	}, nil)
	if err != nil {
		t.Fatalf("recording manager: %v", err)
	}
	manager.SetStorageProbes(
		func(string) (string, string, string, bool) { return base, "/dev/mmcblk0p1", "vfat", true },
		func(string) (int64, error) { return 1 << 40, nil },
	)
	stack.manager.SetRecordings(manager)
	return manager, base
}

func writeReadyWAV(t *testing.T, path string, sample int16) int64 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 44+160*2)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], 8000)
	binary.LittleEndian.PutUint32(data[28:32], 16000)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], 320)
	for index := 0; index < 160; index++ {
		binary.LittleEndian.PutUint16(data[44+index*2:], uint16(sample))
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	return int64(len(data))
}

func TestRecordingHTTPLifecycleAndKeyIsolation(t *testing.T) {
	stack := newTestStack(t)
	_, base := attachRecordingManager(t, stack)
	keyA, secretA := stack.createKey(false)
	tokenA, _ := stack.enroll(keyA, secretA, "Device A")
	deviceA := stack.deviceID
	keyB, secretB := stack.createKey(false)
	tokenB, _ := stack.enroll(keyB, secretB, "Device B")

	blob := filepath.Join(base, "recordings", "line1", "rec_seed.wav")
	size := writeReadyWAV(t, blob, 1200)
	waveformPath := blob + ".waveform.json"
	if err := os.WriteFile(waveformPath, []byte(`{"sampleCount":320,"peaks":[0.1]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := stack.store.SaveRecording(context.Background(), store.Recording{
		ID: "rec_seed", LineID: "line1", CallID: "line1:seed", DeviceID: deviceA, KeyID: keyA,
		Direction: "outbound", Peer: "10086", State: "ready", TriggerMode: "manual",
		FilePath: blob, WaveformPath: waveformPath, SizeBytes: &size, Container: "wav",
		Codec: "pcm_s16le", SampleRate: 8000, Channels: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Owner key: list is key-scoped and includes the row.
	status, payload := stack.request(http.MethodGet, "/api/v2/recordings", tokenA, "", nil)
	if status != http.StatusOK {
		t.Fatalf("list status = %d: %s", status, payload)
	}
	var list []map[string]any
	if err := json.Unmarshal(payload, &list); err != nil || len(list) != 1 || list[0]["id"] != "rec_seed" {
		t.Fatalf("owner list = %s err %v", payload, err)
	}
	if list[0]["keyId"] != keyA {
		t.Fatalf("list keyId = %v, want %s", list[0]["keyId"], keyA)
	}

	// Audio and waveform stream from the authenticated route only.
	status, audio := stack.request(http.MethodGet, "/api/v2/recordings/rec_seed/audio", tokenA, "", nil)
	if status != http.StatusOK || len(audio) != int(size) || !strings.HasPrefix(string(audio), "RIFF") {
		t.Fatalf("audio status=%d len=%d", status, len(audio))
	}
	status, waveform := stack.request(http.MethodGet, "/api/v2/recordings/rec_seed/waveform", tokenA, "", nil)
	if status != http.StatusOK || !strings.Contains(string(waveform), "sampleCount") {
		t.Fatalf("waveform status=%d body=%s", status, waveform)
	}

	// A DIFFERENT key sharing the same line sees nothing: not the list, not
	// the row, not the audio (404, so existence itself stays hidden).
	status, payload = stack.request(http.MethodGet, "/api/v2/recordings", tokenB, "", nil)
	if status != http.StatusOK {
		t.Fatalf("other-key list status = %d", status)
	}
	if err := json.Unmarshal(payload, &list); err != nil || len(list) != 0 {
		t.Fatalf("other-key list = %s err %v, want empty", payload, err)
	}
	if status, _ = stack.request(http.MethodGet, "/api/v2/recordings/rec_seed", tokenB, "", nil); status != http.StatusNotFound {
		t.Fatalf("other-key row status = %d, want 404", status)
	}
	if status, _ = stack.request(http.MethodGet, "/api/v2/recordings/rec_seed/audio", tokenB, "", nil); status != http.StatusNotFound {
		t.Fatalf("other-key audio status = %d, want 404", status)
	}
	if status, _ = stack.request(http.MethodDelete, "/api/v2/recordings/rec_seed", tokenB, "", nil); status != http.StatusNotFound {
		t.Fatalf("other-key delete status = %d, want 404", status)
	}

	// Settings are read-only for devices (no writable global switch).
	status, payload = stack.request(http.MethodGet, "/api/v2/recordings/settings", tokenA, "", nil)
	if status != http.StatusOK {
		t.Fatalf("settings status = %d: %s", status, payload)
	}
	var settings map[string]any
	if err := json.Unmarshal(payload, &settings); err != nil {
		t.Fatalf("settings decode: %v", err)
	}
	// Build 55: an unset gateway mode permits (all); the DEVICE preference
	// stays the actual opt-in (default OFF), so no admin prerequisite.
	if settings["enabled"] != true || settings["autoMode"] != "all" {
		t.Fatalf("settings = %s", payload)
	}
	if status, _ = stack.request(http.MethodPut, "/api/v2/recordings/settings", tokenA, "",
		[]byte(`{"enabled":false}`)); status < 400 {
		t.Fatalf("PUT settings unexpectedly accepted: %d", status)
	}

	// Start on a non-existent call is rejected. A conference leg now routes
	// to the conference recording path (the old 409 blocker is replaced): a
	// leg that names a missing conference fails 404 instead of silently
	// starting a 1:1 segment.
	if status, _ = stack.request(http.MethodPost, "/api/v2/calls/line1:missing/recording/start", tokenA, "", nil); status == http.StatusOK {
		t.Fatal("start on a missing call succeeded")
	}
	now2 := time.Now().UTC()
	if err := stack.store.UpsertCall(context.Background(), store.Call{
		ID: "line1:confleg", LineID: "line1", WorkerCallID: "confleg", DeviceID: deviceA,
		Direction: "outbound", Peer: "10086", State: "active", ConferenceID: "conf_missing", CreatedAt: now2,
	}); err != nil {
		t.Fatal(err)
	}
	status, payload = stack.request(http.MethodPost, "/api/v2/calls/line1:confleg/recording/start", tokenA, "", nil)
	if status != http.StatusNotFound || !strings.Contains(string(payload), "会议不存在") {
		t.Fatalf("conference leg start status=%d body=%s", status, payload)
	}

	// A live conference with no host media session fails with an actionable
	// 409 (not a silent success), and a device from ANOTHER key can neither
	// start nor stop it (host/key scope).
	if err := stack.store.CreateConference(context.Background(), store.Conference{
		ID: "conf_x", HostDeviceID: deviceA, State: "active", CreatedAt: now2,
	}); err != nil {
		t.Fatal(err)
	}
	status, payload = stack.request(http.MethodPost, "/api/v2/conferences/conf_x/recording/start", tokenA, "", nil)
	if status != http.StatusConflict || !strings.Contains(string(payload), "媒体") {
		t.Fatalf("conference start status=%d body=%s", status, payload)
	}
	if status, _ = stack.request(http.MethodPost, "/api/v2/conferences/conf_x/recording/start", tokenB, "", nil); status != http.StatusForbidden {
		t.Fatalf("other-key conference start status=%d, want 403", status)
	}
	if status, _ = stack.request(http.MethodPost, "/api/v2/conferences/conf_x/recording/stop", tokenB, "", nil); status != http.StatusForbidden {
		t.Fatalf("other-key conference stop status=%d, want 403", status)
	}

	// Owner delete tombstones and removes the blob.
	status, payload = stack.request(http.MethodDelete, "/api/v2/recordings/rec_seed", tokenA, "", nil)
	if status != http.StatusOK || !strings.Contains(string(payload), "true") {
		t.Fatalf("delete status=%d body=%s", status, payload)
	}
	if status, _ = stack.request(http.MethodGet, "/api/v2/recordings/rec_seed", tokenA, "", nil); status != http.StatusNotFound {
		t.Fatalf("deleted row status = %d, want 404", status)
	}
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Fatalf("blob survived delete: %v", err)
	}
}

func TestRecordingEventAllowedRequiresSameKey(t *testing.T) {
	stack := newTestStack(t)
	access := store.Access{
		Key: store.EnrollmentKey{ID: "key_1"},
		Lines: []store.LineAccess{{
			Line:        store.Line{ID: "line1"},
			Permissions: store.KeyLine{ReceiveCalls: true, Dial: true},
		}},
	}
	base := events.Event{Type: "recording.started", LineID: "line1"}
	base.Data = json.RawMessage(`{"id":"rec_1","keyId":"key_1","direction":"inbound"}`)
	if !stack.api.eventAllowed(context.Background(), "dev_1", access, base) {
		t.Fatal("same-key recording event rejected")
	}
	other := events.Event{Type: "recording.started", LineID: "line1", Data: json.RawMessage(`{"id":"rec_1","keyId":"key_2","direction":"inbound"}`)}
	if stack.api.eventAllowed(context.Background(), "dev_1", access, other) {
		t.Fatal("different-key recording event delivered")
	}
	empty := events.Event{Type: "recording.ready", LineID: "line1", Data: json.RawMessage(`{"id":"rec_1","direction":"inbound"}`)}
	if stack.api.eventAllowed(context.Background(), "dev_1", access, empty) {
		t.Fatal("key-less recording event delivered (must fail closed)")
	}
	smsOnly := store.Access{
		Key: store.EnrollmentKey{ID: "key_1"},
		Lines: []store.LineAccess{{
			Line:        store.Line{ID: "line1"},
			Permissions: store.KeyLine{ReceiveSMS: true},
		}},
	}
	if stack.api.eventAllowed(context.Background(), "dev_1", smsOnly, base) {
		t.Fatal("recording event delivered to an SMS-only device")
	}
	malformed := events.Event{Type: "recording.failed", LineID: "line1", Data: json.RawMessage(`{bad json`)}
	if stack.api.eventAllowed(context.Background(), "dev_1", access, malformed) {
		t.Fatal("malformed recording event delivered")
	}
}
