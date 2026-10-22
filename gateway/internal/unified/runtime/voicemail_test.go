package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/voice"
)

type sendRecorder struct {
	frames [][]byte
	err    error
}

func (r *sendRecorder) Send(frame []byte) error {
	if r.err != nil {
		return r.err
	}
	r.frames = append(r.frames, append([]byte(nil), frame...))
	return nil
}

func TestGreetingAssetIsRealAndParseable(t *testing.T) {
	samples := greetingSamples()
	if samples == nil {
		t.Fatal("voicemail greeting asset missing from embedded build")
	}
	if len(samples) < 8000 {
		t.Fatalf("greeting is only %d samples, want >= 1s of audible audio", len(samples))
	}
	peak := pcmPeak(samples)
	if peak < 3000 {
		t.Fatalf("greeting peak = %d, looks like silence", peak)
	}
}

func TestBuildBeepHasFadeAndTone(t *testing.T) {
	beep := buildBeep(1000, 200)
	if len(beep) != 1600 {
		t.Fatalf("beep length = %d, want 1600", len(beep))
	}
	if beep[0] != 0 || beep[len(beep)-1] != 0 {
		t.Fatalf("beep must fade in/out, endpoints = %d/%d", beep[0], beep[len(beep)-1])
	}
	peak := pcmPeak(beep)
	if peak < 5000 || peak > 10000 {
		t.Fatalf("beep peak = %d, want ~9000", peak)
	}
}

func TestPlaySamplesPacesPCMUAt20ms(t *testing.T) {
	recorder := &sendRecorder{}
	samples := make([]int16, 320) // 2 frames
	started := time.Now()
	if err := playSamples(context.Background(), recorder, samples); err != nil {
		t.Fatalf("playSamples: %v", err)
	}
	if len(recorder.frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(recorder.frames))
	}
	for _, frame := range recorder.frames {
		if len(frame) != voice.FrameSamples {
			t.Fatalf("frame length = %d, want %d", len(frame), voice.FrameSamples)
		}
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("playback took %v, pacing looks wrong", elapsed)
	}
}

func TestStorageAvailableHonorsCaps(t *testing.T) {
	settings := VoicemailSettings{MaxBytes: 1000, MinFreeBytes: 500}
	if !storageAvailable(600, 999, settings) {
		t.Fatal("available storage should permit recording")
	}
	if storageAvailable(400, 10, settings) {
		t.Fatal("low free space must block recording")
	}
	if storageAvailable(10_000, 1000, settings) {
		t.Fatal("capacity cap must block recording")
	}
}

func TestAnswerVersusVoicemailAtomicOwnership(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()

	deviceCall := store.Call{ID: "line1:call-a", LineID: "line1", WorkerCallID: "call-a", Direction: "inbound", Peer: "+8613800000000", State: StateRinging, CreatedAt: now}
	voicemailCall := store.Call{ID: "line1:call-b", LineID: "line1", WorkerCallID: "call-b", Direction: "inbound", Peer: "+8613800000000", State: StateRinging, CreatedAt: now}
	if err := st.UpsertCall(ctx, deviceCall); err != nil {
		t.Fatalf("insert call a: %v", err)
	}
	if err := st.UpsertCall(ctx, voicemailCall); err != nil {
		t.Fatalf("insert call b: %v", err)
	}

	// Device answer wins: voicemail must not be able to claim.
	claimed, err := st.ClaimCall(ctx, deviceCall.ID, "dev_1")
	if err != nil || !claimed {
		t.Fatalf("device claim = %v/%v", claimed, err)
	}
	voicemailWon, err := st.ClaimCallForVoicemail(ctx, deviceCall.ID)
	if err != nil || voicemailWon {
		t.Fatalf("voicemail claimed an answered call: %v/%v", voicemailWon, err)
	}

	// Voicemail wins: device answer must not be able to claim.
	voicemailWon, err = st.ClaimCallForVoicemail(ctx, voicemailCall.ID)
	if err != nil || !voicemailWon {
		t.Fatalf("voicemail claim = %v/%v", voicemailWon, err)
	}
	claimed, err = st.ClaimCall(ctx, voicemailCall.ID, "dev_2")
	if err != nil || claimed {
		t.Fatalf("device claimed a voicemail call: %v/%v", claimed, err)
	}
	current, err := st.GetCall(ctx, voicemailCall.ID)
	if err != nil || current.State != StateVoicemailAnswering || current.DeviceID != "" {
		t.Fatalf("voicemail ownership state = %+v (err %v)", current, err)
	}
}

func TestExpiredVoicemailsAreSelectedForRetention(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	expired := store.Voicemail{ID: "vm_expired", LineID: "line1", State: "ready", CreatedAt: now.AddDate(0, 0, -40), ExpiresAt: now.Add(-time.Hour)}
	fresh := store.Voicemail{ID: "vm_fresh", LineID: "line1", State: "ready", CreatedAt: now, ExpiresAt: now.AddDate(0, 0, 30)}
	if err := st.InsertVoicemail(ctx, expired); err != nil {
		t.Fatalf("insert expired: %v", err)
	}
	if err := st.InsertVoicemail(ctx, fresh); err != nil {
		t.Fatalf("insert fresh: %v", err)
	}
	selected, err := st.ListExpiredVoicemails(ctx, now, 10)
	if err != nil {
		t.Fatalf("list expired: %v", err)
	}
	if len(selected) != 1 || selected[0].ID != expired.ID {
		t.Fatalf("expired selection = %+v, want only %s", selected, expired.ID)
	}
}

func TestSoftDeletedBlobReclaimedBelowCapacityWhenMountReturns(t *testing.T) {
	// Regression: the soft-deleted tombstone cleanup used to sit behind the
	// MaxBytes capacity early returns, so an under-capacity system would retry
	// removing the blob forever when the SD mount came back.
	original := mountSourceLookup
	t.Cleanup(func() { mountSourceLookup = original })

	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mountPoint := t.TempDir()
	audioRoot := filepath.Join(mountPoint, "voicemail")
	cfg := config.Default()
	cfg.Gateway.DataDir = t.TempDir()
	cfg.Voicemail.AudioRoot = audioRoot
	cfg.Voicemail.MountPoint = mountPoint
	cfg.Voicemail.ExpectedDevice = "/dev/mmcblk0p1"
	// Default storage cap is far larger than the 3-byte test blob, so this is
	// the under-capacity path where the old code returned before tombstone
	// cleanup.
	manager := NewManager(cfg, st, nil, nil, nil, nil)

	now := time.Now().UTC()
	lineDir := filepath.Join(audioRoot, "line1")
	if err := os.MkdirAll(lineDir, 0o700); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(lineDir, "vm_del.wav")
	if err := os.WriteFile(blob, []byte("wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Insert then soft-delete (as DELETE /voicemails/{id} does).
	if err := st.InsertVoicemail(ctx, store.Voicemail{
		ID: "vm_del", LineID: "line1", State: "ready", Path: blob,
		SizeBytes: 3, CreatedAt: now, ExpiresAt: now.AddDate(0, 0, 30),
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := st.SoftDeleteVoicemail(ctx, "vm_del", now)
	if err != nil || !deleted {
		t.Fatalf("soft delete = %v/%v", deleted, err)
	}

	// Mount unavailable: prune must keep the tombstone and the blob.
	mountSourceLookup = func(string) (string, string, string, bool) { return "", "", "", false }
	manager.pruneVoicemail(ctx)
	if rows, _ := st.ListDeletedVoicemails(ctx, 10); len(rows) != 1 {
		t.Fatalf("tombstone dropped while SD unavailable: %d rows", len(rows))
	}
	if _, err := os.Stat(blob); err != nil {
		t.Fatalf("blob removed while SD unavailable: %v", err)
	}

	// Mount returns (still under capacity / unlimited): blob removed and row
	// hard-deleted on the same pass.
	mountSourceLookup = func(string) (string, string, string, bool) {
		return mountPoint, "/dev/mmcblk0p1", "vfat", true
	}
	manager.pruneVoicemail(ctx)
	if rows, _ := st.ListDeletedVoicemails(ctx, 10); len(rows) != 0 {
		t.Fatalf("tombstone not reclaimed: %d rows", len(rows))
	}
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Fatalf("blob left behind: %v", err)
	}
}

func TestVoicemailAudioRootRequiresVerifiedSD(t *testing.T) {
	original := mountSourceLookup
	t.Cleanup(func() { mountSourceLookup = original })

	mountPoint := t.TempDir()
	audioRoot := filepath.Join(mountPoint, "voicemail")
	cfg := config.Default()
	cfg.Voicemail.AudioRoot = audioRoot
	cfg.Voicemail.MountPoint = mountPoint
	cfg.Voicemail.ExpectedDevice = "/dev/mmcblk0"
	manager := NewManager(cfg, nil, nil, nil, nil, nil)

	// Wrong device (eMMC) must be refused.
	mountSourceLookup = func(string) (string, string, string, bool) { return mountPoint, "/dev/mmcblk1p2", "ext4", true }
	if _, err := manager.voicemailAudioRoot(); err == nil {
		t.Fatal("wrong mount device was accepted")
	}
	// Missing mount must be refused without falling back to the root fs.
	mountSourceLookup = func(string) (string, string, string, bool) { return "", "", "", false }
	if _, err := manager.voicemailAudioRoot(); err == nil {
		t.Fatal("missing SD mount was accepted")
	}
	// Wrong filesystem type is refused even when the device path matches.
	mountSourceLookup = func(string) (string, string, string, bool) { return mountPoint, "/dev/mmcblk0p1", "ext4", true }
	if _, err := manager.voicemailAudioRoot(); err == nil {
		t.Fatal("wrong filesystem type was accepted")
	}
	// Correct SD mount creates the dedicated subdirectory.
	mountSourceLookup = func(string) (string, string, string, bool) { return mountPoint, "/dev/mmcblk0p1", "vfat", true }
	root, err := manager.voicemailAudioRoot()
	if err != nil {
		t.Fatalf("verified SD mount rejected: %v", err)
	}
	if root != audioRoot {
		t.Fatalf("audio root = %q, want %q", root, audioRoot)
	}
	if info, statErr := os.Lstat(audioRoot); statErr != nil || !info.IsDir() {
		t.Fatalf("audio root was not created on the verified mount: %v", statErr)
	}
	status := manager.VoicemailStorageStatus()
	if !status.Mounted || status.Device != "/dev/mmcblk0p1" || status.FSType != "vfat" {
		t.Fatalf("storage status = %+v", status)
	}
}

func TestSafeVoicemailPathRejectsTraversalAndSymlinks(t *testing.T) {
	root := t.TempDir()
	if _, err := safeVoicemailPath(root, "../evil", "vm_1"); err == nil {
		t.Fatal("path traversal in line id was accepted")
	}
	if _, err := safeVoicemailPath(root, "line1", "../vm_1"); err == nil {
		t.Fatal("path traversal in voicemail id was accepted")
	}
	valid, err := safeVoicemailPath(root, "line1", "vm_ok")
	if err != nil {
		t.Fatalf("valid path rejected: %v", err)
	}
	if !strings.HasPrefix(valid, root+string(filepath.Separator)) {
		t.Fatalf("valid path escaped root: %q", valid)
	}
	if err := os.MkdirAll(filepath.Dir(valid), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink("/etc/passwd", valid); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := safeVoicemailPath(root, "line1", "vm_ok"); err == nil {
		t.Fatal("symlinked voicemail path was accepted")
	}
}

func TestVoicemailDelayPolicyNeverShortensForPushCapableLine(t *testing.T) {
	settings := VoicemailSettings{RingSeconds: 30, OfflineSeconds: 8, Enabled: true}
	// Suspended/locked iOS has no WebSocket; a registered push target must
	// still get the full ring grace so PushKit can wake the phone.
	if delay := voicemailDelay(settings, false, true); delay != 30*time.Second {
		t.Fatalf("push-capable no-WS delay = %v, want 30s", delay)
	}
	if delay := voicemailDelay(settings, true, true); delay != 30*time.Second {
		t.Fatalf("online delay = %v, want 30s", delay)
	}
	if delay := voicemailDelay(settings, true, false); delay != 30*time.Second {
		t.Fatalf("online no-push delay = %v, want 30s", delay)
	}
	// Only a line with neither a live stream nor any push target may use the
	// short offline grace.
	if delay := voicemailDelay(settings, false, false); delay != 8*time.Second {
		t.Fatalf("truly unreachable delay = %v, want 8s", delay)
	}
}

func TestVoicemailClaimNeverResurrectsEndedCall(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	call := store.Call{ID: "line1:call-race", LineID: "line1", WorkerCallID: "call-race", Direction: "inbound", Peer: "+8613800000000", State: StateRinging, CreatedAt: now}
	if err := st.UpsertCall(ctx, call); err != nil {
		t.Fatalf("insert: %v", err)
	}
	claimed, err := st.ClaimCallForVoicemail(ctx, call.ID)
	if err != nil || !claimed {
		t.Fatalf("claim = %v/%v", claimed, err)
	}
	// Remote end wins while voicemail owns the call; failure paths must not
	// write a stale ringing snapshot back over the terminal row.
	call.State = StateEnded
	ended := time.Now().UTC()
	call.EndedAt = &ended
	if err := st.UpsertCall(ctx, call); err != nil {
		t.Fatalf("end: %v", err)
	}
	reverted, err := st.RevertVoicemailClaim(ctx, call.ID)
	if err != nil {
		t.Fatalf("revert: %v", err)
	}
	if reverted {
		t.Fatal("revert touched an ended call")
	}
	promoted, err := st.MarkVoicemailRecording(ctx, call.ID, time.Now().UTC())
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted {
		t.Fatal("promote touched an ended call")
	}
	current, err := st.GetCall(ctx, call.ID)
	if err != nil || current.State != StateEnded {
		t.Fatalf("ended row mutated: %+v err=%v", current, err)
	}
}

func TestVoicemailRevertOnlyWhileStillOwned(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	call := store.Call{ID: "line1:call-own", LineID: "line1", WorkerCallID: "call-own", Direction: "inbound", Peer: "+8613800000000", State: StateRinging, CreatedAt: time.Now().UTC()}
	if err := st.UpsertCall(ctx, call); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if claimed, _ := st.ClaimCallForVoicemail(ctx, call.ID); !claimed {
		t.Fatal("claim failed")
	}
	if reverted, err := st.RevertVoicemailClaim(ctx, call.ID); err != nil || !reverted {
		t.Fatalf("revert while owned = %v/%v", reverted, err)
	}
	current, _ := st.GetCall(ctx, call.ID)
	if current.State != StateRinging || current.DeviceID != "" {
		t.Fatalf("revert state = %+v", current)
	}
}
