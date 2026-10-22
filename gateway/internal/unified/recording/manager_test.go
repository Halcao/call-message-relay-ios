package recording

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/store"
)

// ---------- harness ----------

type eventRecorder struct {
	mu     sync.Mutex
	kinds  []string
	byKind map[string]store.Recording
}

func (r *eventRecorder) handler(kind string, rec store.Recording) {
	r.mu.Lock()
	r.kinds = append(r.kinds, kind)
	if r.byKind == nil {
		r.byKind = map[string]store.Recording{}
	}
	r.byKind[kind] = rec
	r.mu.Unlock()
}

func (r *eventRecorder) saw(kind string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, candidate := range r.kinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

func (r *eventRecorder) last(kind string) (store.Recording, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byKind[kind]
	return rec, ok
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newTestManager(t *testing.T, st *store.Store) (*Manager, string, *eventRecorder) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "recordings")
	manager, err := NewManager(st, Options{
		Root: root, MountPoint: base, ExpectedDevice: "/dev/mmcblk0", ExpectedFSType: "vfat",
		MinimumFreeBytes: 1024,
	}, nil)
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	manager.mountSourceLookup = func(string) (string, string, string, bool) {
		return base, "/dev/mmcblk0p1", "vfat", true
	}
	manager.freeBytes = func(string) (int64, error) { return 1 << 40, nil }
	recorder := &eventRecorder{}
	manager.SetEventHandler(recorder.handler)
	return manager, root, recorder
}

func insertCall(t *testing.T, st *store.Store, id, lineID, direction string, ended bool) store.Call {
	t.Helper()
	call := store.Call{
		ID: id, LineID: lineID, WorkerCallID: "w_" + id, DeviceID: "dev1",
		Direction: direction, Peer: "10086", State: "active", CreatedAt: time.Now().UTC(),
	}
	if ended {
		now := time.Now().UTC()
		call.State = "ended"
		call.EndedAt = &now
	}
	if err := st.UpsertCall(context.Background(), call); err != nil {
		t.Fatalf("insert call: %v", err)
	}
	return call
}

func testCallMeta(id, lineID, direction string) CallMeta {
	return CallMeta{ID: id, LineID: lineID, Direction: direction, Peer: "10086", StartedAt: time.Now().UTC()}
}

func testFrame(value int16) []int16 {
	frame := make([]int16, FrameSamples)
	for index := range frame {
		frame[index] = value
	}
	return frame
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func readWAV(t *testing.T, path string) []int16 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read wav: %v", err)
	}
	if len(data) < wavHeaderSize || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("not a RIFF/WAVE file: %q", data[:12])
	}
	if got := binary.LittleEndian.Uint32(data[24:28]); got != SampleRate {
		t.Fatalf("wav sample rate = %d, want %d", got, SampleRate)
	}
	payload := data[wavHeaderSize:]
	samples := make([]int16, len(payload)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(payload[index*2 : index*2+2]))
	}
	return samples
}

// ---------- both directions, known samples ----------

func TestManualRecordingCapturesBothDirectionsWithKnownSamples(t *testing.T) {
	st := newTestStore(t)
	manager, _, events := newTestManager(t, st)
	ctx := context.Background()
	insertCall(t, st, "line1:call1", "line1", "outbound", false)
	meta := testCallMeta("line1:call1", "line1", "outbound")

	rec, err := manager.Start(ctx, meta, "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if rec.State != "recording" || rec.KeyID != "key1" || rec.DeviceID != "dev1" {
		t.Fatalf("unexpected recording: %+v", rec)
	}
	const frames = 12
	for index := 0; index < frames; index++ {
		manager.WriteUplink(rec.CallID, testFrame(int16(1000+index)))
		manager.WriteDownlink(rec.CallID, testFrame(int16(-1000-index)))
	}
	if err := manager.Stop(ctx, rec.CallID, "manual"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return events.saw("ready") })

	final, err := manager.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.State != "ready" {
		t.Fatalf("state = %q, want ready (detail %s)", final.State, final.FailureDetail)
	}
	if final.DurationMs == nil || *final.DurationMs != frames*FrameSamples*1000/SampleRate {
		t.Fatalf("duration = %v, want %d", final.DurationMs, frames*FrameSamples*1000/SampleRate)
	}
	if final.SizeBytes == nil || *final.SizeBytes != int64(wavHeaderSize+frames*FrameSamples*BytesPerSample) {
		t.Fatalf("size = %v, want %d", final.SizeBytes, wavHeaderSize+frames*FrameSamples*BytesPerSample)
	}
	if len(final.SHA256) != 64 {
		t.Fatalf("sha256 = %q", final.SHA256)
	}
	samples := readWAV(t, final.FilePath)
	if len(samples) != frames*FrameSamples {
		t.Fatalf("samples = %d, want %d", len(samples), frames*FrameSamples)
	}
	for index := 0; index < frames; index++ {
		uplink := int32(1000 + index)
		downlink := int32(-1000 - index)
		want := int16((uplink + downlink) / 2) // -0..: both sides present
		if got := samples[index*FrameSamples]; got != want {
			t.Fatalf("frame %d mix = %d, want %d (uplink %d downlink %d)", index, got, want, uplink, downlink)
		}
	}
	// The wave form document must be written and parse as JSON.
	waveform, err := os.ReadFile(final.WaveformPath)
	if err != nil || len(waveform) == 0 || waveform[0] != '{' {
		t.Fatalf("waveform file invalid: %v %q", err, waveform[:min(len(waveform), 20)])
	}
	if !events.saw("started") || !events.saw("finalizing") || !events.saw("ready") {
		t.Fatalf("lifecycle events missing: %v", events.kinds)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestRecordingSingleSegmentSurvivesConcurrentWritersAndHostSwap exercises the
// "one logical uplink across transport switches" contract at the recorder
// boundary: two writer sources (simulating a primary and a re-attached
// transport) feed the SAME call id, and the manager must produce exactly one
// segment whose sample count covers all frames. The media-layer tap continuity
// itself is covered in the media package.
func TestRecordingSingleSegmentSurvivesConcurrentWritersAndHostSwap(t *testing.T) {
	st := newTestStore(t)
	manager, _, events := newTestManager(t, st)
	manager.queueDepth = 512
	ctx := context.Background()
	insertCall(t, st, "line1:call2", "line1", "inbound", false)

	rec, err := manager.Start(ctx, testCallMeta("line1:call2", "line1", "inbound"), "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var group sync.WaitGroup
	for writer := 0; writer < 2; writer++ {
		group.Add(1)
		go func(value int16) {
			defer group.Done()
			for index := 0; index < 40; index++ {
				manager.WriteDownlink(rec.CallID, testFrame(value))
				manager.WriteUplink(rec.CallID, testFrame(value+1))
			}
		}(int16(100 * (writer + 1)))
	}
	group.Wait()
	if err := manager.Stop(ctx, rec.CallID, "manual"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return events.saw("ready") })
	final, err := manager.Get(ctx, rec.ID)
	if err != nil || final.State != "ready" {
		t.Fatalf("final = %+v err %v", final, err)
	}
	samples := readWAV(t, final.FilePath)
	if len(samples) != 80*FrameSamples {
		t.Fatalf("samples = %d, want %d", len(samples), 80*FrameSamples)
	}
	var rows int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM recordings WHERE call_id = ?`, rec.CallID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("recordings for call = %d, want 1 (one logical uplink)", rows)
	}
}

// ---------- storage failure paths ----------

func TestStartStorageFailures(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	insertCall(t, st, "line1:call3", "line1", "outbound", false)
	meta := testCallMeta("line1:call3", "line1", "outbound")

	t.Run("missing SD mount", func(t *testing.T) {
		manager, _, _ := newTestManager(t, st)
		manager.mountSourceLookup = func(string) (string, string, string, bool) { return "", "", "", false }
		if _, err := manager.Start(ctx, meta, "dev1", "key1"); !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("err = %v, want ErrStorageUnavailable", err)
		}
	})
	t.Run("wrong mount device", func(t *testing.T) {
		manager, _, _ := newTestManager(t, st)
		manager.mountSourceLookup = func(base string) (string, string, string, bool) {
			return base, "/dev/sda1", "vfat", true
		}
		if _, err := manager.Start(ctx, meta, "dev1", "key1"); !errors.Is(err, ErrStorageUnavailable) {
			t.Fatalf("err = %v, want ErrStorageUnavailable", err)
		}
	})
	t.Run("low free space", func(t *testing.T) {
		manager, _, _ := newTestManager(t, st)
		manager.freeBytes = func(string) (int64, error) { return 10, nil }
		if _, err := manager.Start(ctx, meta, "dev1", "key1"); !errors.Is(err, ErrLowStorage) {
			t.Fatalf("err = %v, want ErrLowStorage", err)
		}
	})
	t.Run("global switch off", func(t *testing.T) {
		manager, _, _ := newTestManager(t, st)
		if err := manager.SetEnabled(ctx, false); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if _, err := manager.Start(ctx, meta, "dev1", "key1"); !errors.Is(err, ErrRecordingDisabled) {
			t.Fatalf("err = %v, want ErrRecordingDisabled", err)
		}
		if err := manager.SetEnabled(ctx, true); err != nil {
			t.Fatalf("enable: %v", err)
		}
	})
	t.Run("capacity cap", func(t *testing.T) {
		manager, _, _ := newTestManager(t, st)
		manager.opts.MaxBytes = 1000
		size := int64(1000)
		other := store.Recording{
			ID: "rec_existing", LineID: "line1", CallID: "line1:other", State: "ready",
			SizeBytes: &size, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if err := st.SaveRecording(ctx, other); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := manager.Start(ctx, meta, "dev1", "key1"); !errors.Is(err, ErrCapacityReached) {
			t.Fatalf("err = %v, want ErrCapacityReached", err)
		}
	})
}

// ---------- writer failure truthfulness ----------

func TestWriterWriteErrorFailsTruthfully(t *testing.T) {
	st := newTestStore(t)
	manager, _, events := newTestManager(t, st)
	ctx := context.Background()
	insertCall(t, st, "line1:call4", "line1", "outbound", false)

	var writes int
	var mu sync.Mutex
	manager.testWriterWrite = func(_ *os.File, _ []byte) error {
		mu.Lock()
		writes++
		count := writes
		mu.Unlock()
		if count >= 2 {
			return fmt.Errorf("simulated ENOSPC")
		}
		return nil
	}
	rec, err := manager.Start(ctx, testCallMeta("line1:call4", "line1", "outbound"), "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for index := 0; index < 10; index++ {
		manager.WriteUplink(rec.CallID, testFrame(int16(10+index)))
		manager.WriteDownlink(rec.CallID, testFrame(int16(20+index)))
	}
	waitFor(t, 3*time.Second, func() bool { return events.saw("failed") })

	final, err := manager.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.State != "failed" || final.FailureCode != "storage_error" {
		t.Fatalf("final = state %q code %q detail %q", final.State, final.FailureCode, final.FailureDetail)
	}
	if _, statErr := os.Stat(rec.FilePath); !os.IsNotExist(statErr) {
		t.Fatalf("failed recording left a final blob: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(manager.rootPath(), "work", rec.ID)); !os.IsNotExist(statErr) {
		t.Fatalf("failed recording left work files: %v", statErr)
	}
	call, err := st.GetCall(ctx, rec.CallID)
	if err != nil || call.RecordingState != "failed" {
		t.Fatalf("call recording state = %+v err %v", call, err)
	}
}

func TestWriterOverflowFailsTruthfully(t *testing.T) {
	st := newTestStore(t)
	manager, _, events := newTestManager(t, st)
	manager.queueDepth = 2
	manager.maxDropped = 2
	ctx := context.Background()
	insertCall(t, st, "line1:call5", "line1", "outbound", false)

	release := make(chan struct{})
	var once sync.Once
	manager.testWriterWrite = func(_ *os.File, _ []byte) error {
		<-release // block the writer goroutine so the queue cannot drain
		once.Do(func() {})
		return nil
	}
	rec, err := manager.Start(ctx, testCallMeta("line1:call5", "line1", "outbound"), "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for index := 0; index < 12; index++ {
		manager.WriteUplink(rec.CallID, testFrame(int16(index)))
	}
	// The queue is already full and the writer blocked, so the overflow
	// failure must be latched before the write is allowed to proceed.
	close(release)
	waitFor(t, 3*time.Second, func() bool { return events.saw("failed") })
	final, err := manager.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.State != "failed" || final.FailureCode != "writer_overflow" {
		t.Fatalf("final = state %q code %q", final.State, final.FailureCode)
	}
	// The call's teardown path must not treat the failure as an error.
	waitFor(t, 3*time.Second, func() bool { return !manager.Active(rec.CallID) })
}

func TestFinalizeEncoderFailureIsMarkedFailed(t *testing.T) {
	st := newTestStore(t)
	manager, _, events := newTestManager(t, st)
	manager.encoder = failingEncoder{}
	ctx := context.Background()
	insertCall(t, st, "line1:call6", "line1", "outbound", false)

	rec, err := manager.Start(ctx, testCallMeta("line1:call6", "line1", "outbound"), "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	manager.WriteUplink(rec.CallID, testFrame(5))
	manager.WriteDownlink(rec.CallID, testFrame(6))
	if err := manager.Stop(ctx, rec.CallID, "manual"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return events.saw("failed") })
	final, err := manager.Get(ctx, rec.ID)
	if err != nil || final.State != "failed" || final.FailureCode != "encoder_failed" {
		t.Fatalf("final = %+v err %v", final, err)
	}
}

type failingEncoder struct{}

func (failingEncoder) Encode(context.Context, string, string, string) ([]float32, error) {
	return nil, errors.New("simulated encoder failure")
}

// ---------- delete / retention / path safety ----------

func TestDeleteLifecycleAndActiveGuard(t *testing.T) {
	st := newTestStore(t)
	manager, _, events := newTestManager(t, st)
	ctx := context.Background()
	insertCall(t, st, "line1:call7", "line1", "outbound", false)

	rec, err := manager.Start(ctx, testCallMeta("line1:call7", "line1", "outbound"), "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := manager.Delete(ctx, rec.ID); !errors.Is(err, ErrActiveRecording) {
		t.Fatalf("delete active err = %v, want ErrActiveRecording", err)
	}
	manager.WriteUplink(rec.CallID, testFrame(1))
	manager.WriteDownlink(rec.CallID, testFrame(2))
	if err := manager.Stop(ctx, rec.CallID, "manual"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return events.saw("ready") })
	ready, _ := manager.Get(ctx, rec.ID)
	deleted, err := manager.Delete(ctx, rec.ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted.State != "deleted" || !events.saw("deleted") {
		t.Fatalf("deleted = %+v events %v", deleted, events.kinds)
	}
	if _, err := os.Stat(ready.FilePath); !os.IsNotExist(err) {
		t.Fatalf("blob still present: %v", err)
	}
	if _, err := manager.Get(ctx, rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("row still present: %v", err)
	}
}

func TestAudioPathRejectsSymlinkAndEscape(t *testing.T) {
	st := newTestStore(t)
	manager, root, _ := newTestManager(t, st)
	ctx := context.Background()

	outside := filepath.Join(t.TempDir(), "outside.wav")
	if err := os.WriteFile(outside, []byte("not audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "line1", "rec_link.wav")
	if err := os.MkdirAll(filepath.Dir(symlink), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRecording(ctx, store.Recording{
		ID: "rec_link", LineID: "line1", CallID: "line1:x", State: "ready", FilePath: symlink,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AudioPath(ctx, "rec_link"); err == nil {
		t.Fatal("symlinked recording path must be rejected")
	}
	if err := st.SaveRecording(ctx, store.Recording{
		ID: "rec_escape", LineID: "line1", CallID: "line1:y", State: "ready", FilePath: outside,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AudioPath(ctx, "rec_escape"); err == nil {
		t.Fatal("path outside the recording root must be rejected")
	}
}

func TestRetentionPrunesExpired(t *testing.T) {
	st := newTestStore(t)
	manager, root, _ := newTestManager(t, st)
	ctx := context.Background()
	path := filepath.Join(root, "line1", "rec_old.wav")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	size := int64(3)
	if err := st.SaveRecording(ctx, store.Recording{
		ID: "rec_old", LineID: "line1", CallID: "line1:z", State: "ready", FilePath: path,
		SizeBytes: &size, RetentionExpires: &past, CreatedAt: past, UpdatedAt: past,
	}); err != nil {
		t.Fatal(err)
	}
	manager.prune(ctx)
	if _, err := manager.Get(ctx, "rec_old"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired row survived: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired blob survived: %v", err)
	}
}

// ---------- lifecycle fences / races ----------

func TestConcurrentStartsCreateExactlyOneSession(t *testing.T) {
	st := newTestStore(t)
	manager, _, events := newTestManager(t, st)
	manager.queueDepth = 512
	ctx := context.Background()
	insertCall(t, st, "line1:call_c", "line1", "outbound", false)
	meta := testCallMeta("line1:call_c", "line1", "outbound")

	const attempts = 8
	var group sync.WaitGroup
	results := make([]store.Recording, attempts)
	errs := make([]error, attempts)
	for index := 0; index < attempts; index++ {
		group.Add(1)
		go func(slot int) {
			defer group.Done()
			results[slot], errs[slot] = manager.Start(ctx, meta, "dev1", "key1")
		}(index)
	}
	group.Wait()
	var firstID string
	for slot := 0; slot < attempts; slot++ {
		if errs[slot] != nil {
			if !errors.Is(errs[slot], ErrStarting) {
				t.Fatalf("attempt %d err = %v, want nil or ErrStarting", slot, errs[slot])
			}
			continue
		}
		if firstID == "" {
			firstID = results[slot].ID
		} else if results[slot].ID != firstID {
			t.Fatalf("attempt %d returned %s, want the single session %s", slot, results[slot].ID, firstID)
		}
	}
	if firstID == "" {
		t.Fatal("no concurrent start succeeded")
	}
	manager.WriteUplink(meta.ID, testFrame(11))
	manager.WriteDownlink(meta.ID, testFrame(22))
	if err := manager.Stop(ctx, meta.ID, "manual"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return events.saw("ready") })
	var rows int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM recordings WHERE call_id = ?`, meta.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows for call = %d, want 1", rows)
	}
}

func TestEndCallFencesAgainstLateStart(t *testing.T) {
	st := newTestStore(t)
	manager, _, _ := newTestManager(t, st)
	ctx := context.Background()
	insertCall(t, st, "line1:call_f", "line1", "outbound", false)
	meta := testCallMeta("line1:call_f", "line1", "outbound")

	rec, err := manager.Start(ctx, meta, "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// A hangup while the segment is live finalizes it and fences the id.
	manager.EndCall(meta.ID, "call_ended")
	if _, err := manager.Start(ctx, meta, "dev1", "key1"); !errors.Is(err, ErrCallEnded) {
		t.Fatalf("post-hangup start err = %v, want ErrCallEnded", err)
	}
	if manager.Active(meta.ID) {
		t.Fatal("session survived EndCall")
	}
	// Let the background finalizer finish before the temp dir is reclaimed.
	waitFor(t, 3*time.Second, func() bool {
		current, getErr := manager.Get(ctx, rec.ID)
		return getErr == nil && current.State != "recording" && current.State != "finalizing"
	})
}

// TestTrackWriterCloseDuringEnqueueIsRaceFree exercises the enqueue/close
// interleaving under the race detector: the channel must never be closed
// under a concurrent send.
func TestTrackWriterCloseDuringEnqueueIsRaceFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "track.pcm.part")
	writer, err := newTrackWriter(path, 8, 1, func(string, error) {}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := 0; index < 300; index++ {
				writer.enqueue(testFrame(int16(index)))
			}
		}()
	}
	time.Sleep(2 * time.Millisecond)
	writer.close()
	group.Wait()
}

// ---------- deletion / purge safety ----------

func TestDeleteKeepsTombstoneWhileMountAbsentThenPrunes(t *testing.T) {
	st := newTestStore(t)
	manager, root, events := newTestManager(t, st)
	ctx := context.Background()
	insertCall(t, st, "line1:call_d", "line1", "outbound", false)
	rec, err := manager.Start(ctx, testCallMeta("line1:call_d", "line1", "outbound"), "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	manager.WriteUplink(rec.CallID, testFrame(1))
	manager.WriteDownlink(rec.CallID, testFrame(2))
	if err := manager.Stop(ctx, rec.CallID, "manual"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return events.saw("ready") })
	ready, err := manager.Get(ctx, rec.ID)
	if err != nil || ready.State != "ready" {
		t.Fatalf("ready = %+v err %v", ready, err)
	}

	// Card pulled: the delete must tombstone without touching the row/blob,
	// otherwise the remounted card would keep orphaned audio forever.
	manager.mountSourceLookup = func(string) (string, string, string, bool) { return "", "", "", false }
	if _, err := manager.Delete(ctx, rec.ID); err != nil {
		t.Fatalf("delete while absent: %v", err)
	}
	if _, err := os.Stat(ready.FilePath); err != nil {
		t.Fatalf("blob disappeared while the SD was absent: %v", err)
	}
	row, err := manager.Get(ctx, rec.ID)
	if err != nil || row.State != "deleted" {
		t.Fatalf("tombstone = %+v err %v", row, err)
	}
	// Card returns: retention reclaims the blob and removes the tombstone.
	mountPoint := filepath.Dir(root)
	manager.mountSourceLookup = func(string) (string, string, string, bool) {
		return mountPoint, "/dev/mmcblk0p1", "vfat", true
	}
	manager.prune(ctx)
	if _, err := manager.Get(ctx, rec.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("tombstone survived prune: %v", err)
	}
	if _, err := os.Stat(ready.FilePath); !os.IsNotExist(err) {
		t.Fatalf("blob survived prune: %v", err)
	}
}

func TestSafePathRejectsSymlinkedAncestorAndPurgeRefusesEscape(t *testing.T) {
	st := newTestStore(t)
	manager, root, _ := newTestManager(t, st)
	ctx := context.Background()
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.wav")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	// `<root>/linelink` is a symlink to an outside directory: every path
	// through it must be refused even though it is lexically under root.
	link := filepath.Join(root, "linelink")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	rec := store.Recording{
		ID: "rec_link", LineID: "line1", CallID: "line1:l", KeyID: "key1", State: "ready",
		FilePath: filepath.Join(link, "victim.wav"), CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.SaveRecording(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AudioPath(ctx, rec.ID); err == nil {
		t.Fatal("symlinked ancestor accepted by AudioPath")
	}
	if err := manager.purgeRecording(rec); err == nil {
		t.Fatal("purge accepted a path through a symlinked ancestor")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("outside victim file was removed: %v", err)
	}
	// A hostile stored path outside the root is refused as well.
	escape := store.Recording{
		ID: "rec_escape2", LineID: "line1", CallID: "line1:e", KeyID: "key1", State: "ready",
		FilePath: victim, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.SaveRecording(ctx, escape); err != nil {
		t.Fatal(err)
	}
	if err := manager.purgeRecording(escape); err == nil {
		t.Fatal("purge accepted a path outside the root")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("outside victim file was removed via escape: %v", err)
	}
}

// ---------- crash recovery ----------

func TestRecoverFinalizesEndedCallAndFailsLiveCall(t *testing.T) {
	st := newTestStore(t)
	manager, root, events := newTestManager(t, st)
	ctx := context.Background()

	// Simulate two crashed segments: one whose call ended, one whose call
	// is still marked live (media session impossible after a restart).
	endedCall := insertCall(t, st, "line1:rec_ended", "line1", "outbound", true)
	liveCall := insertCall(t, st, "line1:rec_live", "line1", "outbound", false)

	prepareCrash := func(callID, recID string) store.Recording {
		workDir := filepath.Join(root, "work", recID)
		if err := os.MkdirAll(workDir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeTrack := func(name string, value int16) {
			frame := testFrame(value)
			buffer := make([]byte, len(frame)*2)
			for index, sample := range frame {
				binary.LittleEndian.PutUint16(buffer[index*2:index*2+2], uint16(sample))
			}
			if err := os.WriteFile(filepath.Join(workDir, name), buffer, 0o640); err != nil {
				t.Fatal(err)
			}
		}
		writeTrack("uplink.pcm.part", 1000)
		writeTrack("downlink.pcm.part", -500)
		started := time.Now().Add(-time.Minute)
		rec := store.Recording{
			ID: recID, LineID: "line1", CallID: callID, State: "recording",
			StartedAt: &started, FilePath: filepath.Join(root, "line1", recID+".wav"),
			Container: "wav", Codec: "pcm_s16le", SampleRate: SampleRate, Channels: Channels,
			CreatedAt: started, UpdatedAt: started,
		}
		if err := st.SaveRecording(ctx, rec); err != nil {
			t.Fatal(err)
		}
		return rec
	}
	endedRec := prepareCrash(endedCall.ID, "rec_ended")
	liveRec := prepareCrash(liveCall.ID, "rec_live")

	if err := manager.Recover(ctx); err != nil {
		t.Fatalf("recover: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		final, err := manager.Get(ctx, endedRec.ID)
		return err == nil && final.State == "ready"
	})
	endedFinal, _ := manager.Get(ctx, endedRec.ID)
	if endedFinal.DurationMs == nil || *endedFinal.DurationMs != FrameSamples*1000/SampleRate {
		t.Fatalf("recovered duration = %v", endedFinal.DurationMs)
	}
	samples := readWAV(t, endedFinal.FilePath)
	if got, want := samples[0], int16((1000-500)/2); got != want {
		t.Fatalf("recovered first sample = %d, want %d", got, want)
	}
	waitFor(t, 3*time.Second, func() bool {
		final, err := manager.Get(ctx, liveRec.ID)
		return err == nil && final.State == "failed"
	})
	liveFinal, _ := manager.Get(ctx, liveRec.ID)
	if liveFinal.FailureCode != "gateway_restarted" {
		t.Fatalf("live crash code = %q", liveFinal.FailureCode)
	}
	if !events.saw("ready") || !events.saw("failed") {
		t.Fatalf("recovery events = %v", events.kinds)
	}
}

// ---------- long-call storage bound + drained duration ----------

func TestRecordingStopsWhenFreeSpaceFallsDuringCall(t *testing.T) {
	st := newTestStore(t)
	manager, _, events := newTestManager(t, st)
	manager.queueDepth = 2048
	ctx := context.Background()
	insertCall(t, st, "line1:call_lowdisk", "line1", "outbound", false)

	var checks int
	var mu sync.Mutex
	manager.freeBytes = func(string) (int64, error) {
		mu.Lock()
		checks++
		count := checks
		mu.Unlock()
		if count <= 1 { // Start's free-space check
			return 1 << 40, nil
		}
		return 10, nil // the card fills up during the call
	}
	rec, err := manager.Start(ctx, testCallMeta("line1:call_lowdisk", "line1", "outbound"), "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for index := 0; index < storageCheckFrames+50; index++ {
		manager.WriteUplink(rec.CallID, testFrame(int16(index%1000)))
		manager.WriteDownlink(rec.CallID, testFrame(int16(-index%1000)))
	}
	waitFor(t, 5*time.Second, func() bool { return events.saw("failed") })
	final, err := manager.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.State != "failed" || final.FailureCode != "low_storage" {
		t.Fatalf("final = state %q code %q detail %q", final.State, final.FailureCode, final.FailureDetail)
	}
	if manager.Active(rec.CallID) {
		t.Fatal("segment kept running below the storage reserve")
	}
	if _, statErr := os.Stat(rec.FilePath); !os.IsNotExist(statErr) {
		t.Fatalf("low-storage failure left a final blob: %v", statErr)
	}
}

func TestFinalDurationUsesDrainedSampleCounts(t *testing.T) {
	st := newTestStore(t)
	manager, _, events := newTestManager(t, st)
	manager.queueDepth = 128
	ctx := context.Background()
	insertCall(t, st, "line1:call_drain", "line1", "outbound", false)

	// Slow real writes so several frames are still queued when Stop runs;
	// the final duration must reflect every drained frame, not the counter
	// snapshot taken before the writers were flushed.
	manager.testWriterWrite = func(file *os.File, buffer []byte) error {
		time.Sleep(3 * time.Millisecond)
		_, err := file.Write(buffer)
		return err
	}
	const frames = 6
	rec, err := manager.Start(ctx, testCallMeta("line1:call_drain", "line1", "outbound"), "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	for index := 0; index < frames; index++ {
		manager.WriteUplink(rec.CallID, testFrame(int16(100+index)))
		manager.WriteDownlink(rec.CallID, testFrame(int16(200+index)))
	}
	time.Sleep(5 * time.Millisecond) // let at least one frame persist
	if err := manager.Stop(ctx, rec.CallID, "manual"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return events.saw("ready") })
	final, err := manager.Get(ctx, rec.ID)
	if err != nil || final.State != "ready" {
		t.Fatalf("final = %+v err %v", final, err)
	}
	want := int64(frames * FrameSamples * 1000 / SampleRate)
	if final.DurationMs == nil || *final.DurationMs != want {
		t.Fatalf("duration = %v, want %d (drained frames)", final.DurationMs, want)
	}
}

func TestFinalizeAfterUnmountDoesNotCreateFiles(t *testing.T) {
	st := newTestStore(t)
	manager, root, events := newTestManager(t, st)
	ctx := context.Background()
	insertCall(t, st, "line1:call_unmount", "line1", "outbound", false)
	rec, err := manager.Start(ctx, testCallMeta("line1:call_unmount", "line1", "outbound"), "dev1", "key1")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	manager.WriteUplink(rec.CallID, testFrame(100))
	manager.WriteDownlink(rec.CallID, testFrame(200))
	time.Sleep(20 * time.Millisecond) // ensure the frames reached the work files
	// The card is pulled after start: the probe fails and the mount tree
	// (including the work directory) is gone. The finalizer must fail
	// truthfully and must NOT recreate any directory/file on the fallback
	// filesystem.
	manager.mountSourceLookup = func(string) (string, string, string, bool) { return "", "", "", false }
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(ctx, rec.CallID, "manual"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return events.saw("failed") })
	final, err := manager.Get(ctx, rec.ID)
	if err != nil || final.State != "failed" || final.FailureCode != "storage_unavailable" {
		t.Fatalf("final = %+v err %v", final, err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("finalizer recreated filesystem paths after unmount: %v", statErr)
	}
}

// TestConferenceSegmentKindAndRecordingFence proves the two manager-level
// guarantees the runtime builds on:
//   - a conference segment persists kind=conference + the conference id and
//     finalizes as a normal ready WAV,
//   - a call fenced with BlockCall can never start a segment (the merge
//     race), and UnblockCall restores ordinary 1:1 recording.
func TestConferenceSegmentKindAndRecordingFence(t *testing.T) {
	st := newTestStore(t)
	manager, _, events := newTestManager(t, st)
	ctx := context.Background()
	insertCall(t, st, "conf_1", "line1", "outbound", false)
	insertCall(t, st, "line1:call_fence", "line1", "outbound", false)

	meta := CallMeta{ID: "conf_1", Kind: "conference", LineID: "line1", Direction: "outbound",
		Peer: "张三、李四", StartedAt: time.Now().UTC()}
	rec, err := manager.Start(ctx, meta, "dev1", "key1")
	if err != nil {
		t.Fatalf("conference start: %v", err)
	}
	if rec.Kind != "conference" || rec.ConferenceID != "conf_1" {
		t.Fatalf("conference rec = kind %q conference %q", rec.Kind, rec.ConferenceID)
	}
	row, err := st.GetRecording(ctx, rec.ID)
	if err != nil || row.Kind != "conference" || row.ConferenceID != "conf_1" {
		t.Fatalf("persisted conference row = %+v err %v", row, err)
	}
	manager.WriteUplink(rec.CallID, testFrame(10))
	manager.WriteDownlink(rec.CallID, testFrame(20))
	if err := manager.Stop(ctx, rec.CallID, "conference_ended"); err != nil {
		t.Fatalf("conference stop: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool { return events.saw("ready") })
	final, err := manager.Get(ctx, rec.ID)
	if err != nil || final.State != "ready" || final.Kind != "conference" {
		t.Fatalf("final conference = %+v err %v", final, err)
	}

	manager.BlockCall("line1:call_fence")
	if _, err := manager.Start(ctx, testCallMeta("line1:call_fence", "line1", "outbound"), "dev1", "key1"); !errors.Is(err, ErrInConference) {
		t.Fatalf("fenced start err = %v, want ErrInConference", err)
	}
	manager.UnblockCall("line1:call_fence")
	if manager.CallBlocked("line1:call_fence") {
		t.Fatal("CallBlocked still true after UnblockCall")
	}
	callRec, err := manager.Start(ctx, testCallMeta("line1:call_fence", "line1", "outbound"), "dev1", "key1")
	if err != nil {
		t.Fatalf("start after unblock: %v", err)
	}
	if callRec.Kind != "call" || callRec.ConferenceID != "" {
		t.Fatalf("1:1 rec kind = %q conference %q, want call/empty", callRec.Kind, callRec.ConferenceID)
	}
	manager.WriteUplink(callRec.CallID, testFrame(5))
	manager.WriteDownlink(callRec.CallID, testFrame(6))
	if err := manager.Stop(ctx, callRec.CallID, "manual"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Drain the background finalizer before the store closes with the test.
	waitFor(t, 3*time.Second, func() bool {
		row, err := manager.Get(ctx, callRec.ID)
		return err == nil && row.State == "ready"
	})
}

// TestConcurrentBlockAndStartNeverRegistersFencedSession drives the merge
// race directly: Start and StopForConference+BlockCall run concurrently for
// the same call id. The invariant is the merge-safety guarantee: a
// conference-fenced call id can never end up with a registered 1:1 session,
// and the loser reports a truthful error instead of a phantom success.
func TestConcurrentBlockAndStartNeverRegistersFencedSession(t *testing.T) {
	st := newTestStore(t)
	manager, _, _ := newTestManager(t, st)
	ctx := context.Background()
	for round := 0; round < 40; round++ {
		id := fmt.Sprintf("line1:race_%d", round)
		insertCall(t, st, id, "line1", "outbound", false)

		var wg sync.WaitGroup
		var startErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, startErr = manager.Start(ctx, testCallMeta(id, "line1", "outbound"), "dev1", "key1")
		}()
		go func() {
			defer wg.Done()
			manager.StopForConference(id, "merged_into_conference")
		}()
		wg.Wait()

		if manager.CallBlocked(id) && manager.Active(id) {
			t.Fatalf("round %d: fenced call has an active 1:1 session", round)
		}
		if startErr != nil && !errors.Is(startErr, ErrInConference) {
			t.Fatalf("round %d: start error = %v, want nil or ErrInConference", round, startErr)
		}
		if manager.Active(id) {
			if err := manager.Stop(ctx, id, "cleanup"); err != nil {
				t.Fatalf("round %d cleanup stop: %v", round, err)
			}
		}
		manager.UnblockCall(id)
	}
}
