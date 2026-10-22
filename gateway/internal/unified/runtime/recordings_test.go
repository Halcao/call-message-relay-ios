package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/recording"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/unified/worker"
)

// newRecordingHarness builds a runtime Manager with a real store, one line
// and TWO devices under DIFFERENT enrollment keys that both hold the same
// line permissions. That is exactly the isolation case: line scope alone
// would let key_2 see key_1's recordings.
func newRecordingHarness(t *testing.T) (*Manager, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := st.EnsureLine(ctx, store.Line{ID: "line1", Name: "线路一", Enabled: true}); err != nil {
		t.Fatalf("ensure line: %v", err)
	}
	for _, key := range []struct{ id, device string }{{"key_1", "dev_1"}, {"key_2", "dev_2"}} {
		if err := st.CreateEnrollmentKey(ctx, key.id, key.id, []byte(key.id), "secret", false); err != nil {
			t.Fatalf("create key %s: %v", key.id, err)
		}
		if err := st.CreateDevice(ctx, store.Device{ID: key.device, KeyID: key.id, Name: key.device, PublicKey: []byte(key.device)}); err != nil {
			t.Fatalf("create device %s: %v", key.device, err)
		}
		if err := st.SetKeyLine(ctx, store.KeyLine{
			KeyID: key.id, LineID: "line1", ReceiveSMS: true, ReceiveCalls: true, SendSMS: true, Dial: true,
		}); err != nil {
			t.Fatalf("grant line to %s: %v", key.id, err)
		}
	}
	cfg := config.Default()
	manager := NewManager(cfg, st, nil, nil, nil, nil)
	// Register the runtime line the way startLine would; requireLine checks
	// the live runtime registry, not only the store row.
	manager.mu.Lock()
	manager.lines["line1"] = &Line{Cfg: config.Line{ID: "line1", Name: "线路一", Enabled: true}}
	manager.mu.Unlock()
	base := t.TempDir()
	recordings, err := recording.NewManager(st, recording.Options{
		Root: filepath.Join(base, "recordings"), MountPoint: base,
		ExpectedDevice: "/dev/mmcblk0", ExpectedFSType: "vfat", MinimumFreeBytes: 1024,
	}, nil)
	if err != nil {
		t.Fatalf("new recording manager: %v", err)
	}
	recordings.SetStorageProbes(
		func(string) (string, string, string, bool) { return base, "/dev/mmcblk0p1", "vfat", true },
		func(string) (int64, error) { return 1 << 40, nil },
	)
	manager.SetRecordings(recordings)
	return manager, st, base
}

func insertActiveCall(t *testing.T, st *store.Store, id, deviceID, direction string, mutate func(*store.Call)) store.Call {
	t.Helper()
	call := store.Call{
		ID: id, LineID: "line1", WorkerCallID: "w_" + id, DeviceID: deviceID,
		Direction: direction, Peer: "13800000000", State: StateActive, CreatedAt: time.Now().UTC(),
	}
	if mutate != nil {
		mutate(&call)
	}
	if err := st.UpsertCall(context.Background(), call); err != nil {
		t.Fatalf("insert call: %v", err)
	}
	return call
}

func waitRecordingState(t *testing.T, manager *Manager, deviceID, recordingID, want string) RecordingView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := manager.RecordingDetailForDevice(context.Background(), deviceID, recordingID)
		if err == nil && view.State == want {
			return view
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, err := manager.RecordingDetailForDevice(context.Background(), deviceID, recordingID)
	t.Fatalf("recording %s never reached %s: %+v err %v", recordingID, want, view, err)
	return RecordingView{}
}

func TestRecordingKeyScopeIsolation(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	call := insertActiveCall(t, st, "line1:call1", "dev_1", "outbound", nil)
	manager.setSession(call.ID, media.NewSession(nil, call.ID, "call"))
	defer manager.Recordings.CloseAll("test_end")

	view, err := manager.StartRecording(ctx, "dev_1", call.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if view.KeyID != "key_1" || view.State != "recording" {
		t.Fatalf("view = %+v", view)
	}
	// The acting key's second device (same key) may read the row.
	if _, err := manager.RecordingDetailForDevice(ctx, "dev_1", view.ID); err != nil {
		t.Fatalf("same-key read: %v", err)
	}
	// A DIFFERENT key holding the SAME line must see nothing, not even
	// existence (404), and its list must be empty.
	if _, err := manager.RecordingDetailForDevice(ctx, "dev_2", view.ID); err == nil {
		t.Fatal("different-key device read the recording")
	} else {
		var failure *Failure
		if !errors.As(err, &failure) || failure.Status != 404 {
			t.Fatalf("different-key read err = %v, want 404", err)
		}
	}
	list, err := manager.ListRecordings(ctx, "dev_2", "", 50)
	if err != nil {
		t.Fatalf("different-key list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("different-key list = %+v, want empty", list)
	}
	if _, err := manager.RecordingAudioPath(ctx, "dev_2", view.ID); err == nil {
		t.Fatal("different-key device resolved the audio path")
	}
	list, err = manager.ListRecordings(ctx, "dev_1", "", 50)
	if err != nil || len(list) != 1 {
		t.Fatalf("owner list = %+v err %v", list, err)
	}
}

func TestRecordingStartRequiresOwnershipAndMediaSession(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	call := insertActiveCall(t, st, "line1:call2", "dev_1", "outbound", nil)

	// A different device (even on another key) can never control this call.
	if _, err := manager.StartRecording(ctx, "dev_2", call.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner start err = %v, want ErrForbidden", err)
	}
	// An active call without an attached media session cannot record.
	if _, err := manager.StartRecording(ctx, "dev_1", call.ID); err == nil {
		t.Fatal("start without a media session must fail")
	} else {
		var failure *Failure
		if !errors.As(err, &failure) || failure.Status != 409 {
			t.Fatalf("no-media start err = %v, want 409", err)
		}
	}
	// With a session the same call starts fine, and a repeat is idempotent.
	manager.setSession(call.ID, media.NewSession(nil, call.ID, "call"))
	defer manager.Recordings.CloseAll("test_end")
	first, err := manager.StartRecording(ctx, "dev_1", call.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	again, err := manager.StartRecording(ctx, "dev_1", call.ID)
	if err != nil || again.ID != first.ID {
		t.Fatalf("idempotent start = %+v err %v (first %s)", again, err, first.ID)
	}
}

func TestRecordingConferenceRejectedWithActionableFailure(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	// A conference leg whose conference no longer exists must not silently
	// start a 1:1 segment: the conference path reports the missing conference
	// as a 4xx failure, never a success.
	call := insertActiveCall(t, st, "line1:call3", "dev_1", "outbound", func(call *store.Call) {
		call.ConferenceID = "conf_missing"
	})
	manager.setSession(call.ID, media.NewSession(nil, call.ID, "call"))
	if _, err := manager.StartRecording(ctx, "dev_1", call.ID); err == nil {
		t.Fatal("conference leg with a missing conference must refuse recording")
	} else {
		var failure *Failure
		if !errors.As(err, &failure) || failure.Status != 404 {
			t.Fatalf("conference start err = %v, want 404", err)
		}
	}
}

// attachDummyWorkerClient points the runtime line at an unreachable worker so
// Merge/Split/Close exercise their state machine while every media attach
// fails fast (the recording lifecycle under test owns no audio).
func attachDummyWorkerClient(t *testing.T, manager *Manager, st *store.Store) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "internal.token")
	if err := os.WriteFile(token, []byte("dummy"), 0o600); err != nil {
		t.Fatalf("write worker token: %v", err)
	}
	manager.Line("line1").Client = worker.New(
		"line1", "http://127.0.0.1:1", "ws://127.0.0.1:1", token, "", st, nil)
}

// TestConferenceRecordingLifecycleAcrossMergeSplitAndEnd is the focused
// three-way acceptance: the prior 1:1 segment survives a merge as a playable
// completed recording, the host device/key alone controls the conference
// segment, every participant is captured once, and split/leg-end/close each
// finalize the conference segment as completed while releasing the members
// back to ordinary 1:1 recording.
func TestConferenceRecordingLifecycleAcrossMergeSplitAndEnd(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	attachDummyWorkerClient(t, manager, st)
	defer manager.Recordings.CloseAll("test_end")

	callA := insertActiveCall(t, st, "line1:confa", "dev_1", "outbound", nil)
	callB := insertActiveCall(t, st, "line1:confb", "dev_1", "outbound", nil)
	manager.setSession(callA.ID, media.NewSession(nil, callA.ID, "call"))

	// A manual 1:1 segment is running when the user merges. It must become a
	// COMPLETED recording with its captured audio, never a failure, and the
	// member calls must be fenced from new 1:1 segments.
	prior, err := manager.StartRecording(ctx, "dev_1", callA.ID)
	if err != nil {
		t.Fatalf("start 1:1: %v", err)
	}
	manager.Recordings.WriteUplink(callA.ID, make([]int16, 160))
	manager.Recordings.WriteDownlink(callA.ID, make([]int16, 160))

	conference, err := manager.Merge(ctx, "dev_1", []string{callA.ID, callB.ID})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	priorReady := waitRecordingState(t, manager, "dev_1", prior.ID, "ready")
	// At least the explicit frame must be captured; the live session's mixer
	// taps may also append aligned silence ticks, so this is a lower bound.
	if priorReady.DurationMs < 20 {
		t.Fatalf("prior 1:1 segment duration = %d, want >= 20ms", priorReady.DurationMs)
	}
	if priorReady.Kind != "call" {
		t.Fatalf("prior segment kind = %q, want call", priorReady.Kind)
	}
	if !manager.Recordings.CallBlocked(callA.ID) || !manager.Recordings.CallBlocked(callB.ID) {
		t.Fatal("merged calls must be fenced from 1:1 recording")
	}

	// Only the host device (and therefore only its enrollment key) may start
	// or stop the conference segment.
	if _, err := manager.StartConferenceRecording(ctx, "dev_2", conference.ID); err == nil {
		t.Fatal("non-host device started conference recording")
	}
	confRec, err := manager.StartConferenceRecording(ctx, "dev_1", conference.ID)
	if err != nil {
		t.Fatalf("conference start: %v", err)
	}
	if confRec.Kind != "conference" || confRec.ConferenceID != conference.ID || confRec.KeyID != "key_1" {
		t.Fatalf("conference recording view = %+v", confRec)
	}
	if _, err := manager.StopConferenceRecording(ctx, "dev_2", conference.ID); err == nil {
		t.Fatal("non-host device stopped conference recording")
	}
	// The canonical taps deliver the host uplink plus the all-legs client mix
	// once per tick; two ticks must yield a 40 ms mixed segment.
	for tick := 0; tick < 2; tick++ {
		manager.Recordings.WriteUplink(conference.ID, make([]int16, 160))
		manager.Recordings.WriteDownlink(conference.ID, make([]int16, 160))
	}
	if _, err := manager.StopConferenceRecording(ctx, "dev_1", conference.ID); err != nil {
		t.Fatalf("conference stop: %v", err)
	}
	confReady := waitRecordingState(t, manager, "dev_1", confRec.ID, "ready")
	// Two explicit ticks were offered; the session mixer may add aligned
	// silence ticks, so only the lower bound is deterministic.
	if confReady.DurationMs < 40 {
		t.Fatalf("conference segment duration = %d, want >= 40ms", confReady.DurationMs)
	}

	// Key isolation: the other key sharing the line sees neither the row nor
	// its audio, while the owning key lists it.
	if _, err := manager.RecordingDetailForDevice(ctx, "dev_2", confRec.ID); err == nil {
		t.Fatal("different-key device read the conference recording")
	} else {
		var failure *Failure
		if !errors.As(err, &failure) || failure.Status != 404 {
			t.Fatalf("different-key read err = %v, want 404", err)
		}
	}
	if _, err := manager.RecordingAudioPath(ctx, "dev_2", confRec.ID); err == nil {
		t.Fatal("different-key device resolved the conference audio")
	}
	otherList, err := manager.ListRecordings(ctx, "dev_2", "", 50)
	if err != nil || len(otherList) != 0 {
		t.Fatalf("different-key list = %+v err %v, want empty", otherList, err)
	}
	ownerList, err := manager.ListRecordings(ctx, "dev_1", "", 50)
	if err != nil {
		t.Fatalf("owner list: %v", err)
	}
	found := false
	for _, view := range ownerList {
		if view.ID == confRec.ID && view.Kind == "conference" {
			found = true
		}
	}
	if !found {
		t.Fatalf("owner list misses the conference segment: %+v", ownerList)
	}

	// Split dissolves the conference: the active conference segment is
	// finalized as completed and every leg may record as a 1:1 call again.
	splitRec, err := manager.StartConferenceRecording(ctx, "dev_1", conference.ID)
	if err != nil {
		t.Fatalf("second conference start: %v", err)
	}
	manager.Recordings.WriteUplink(conference.ID, make([]int16, 160))
	manager.Recordings.WriteDownlink(conference.ID, make([]int16, 160))
	if err := manager.Split(ctx, "dev_1", conference.ID, callB.ID); err != nil {
		t.Fatalf("split: %v", err)
	}
	if splitReady := waitRecordingState(t, manager, "dev_1", splitRec.ID, "ready"); splitReady.Kind != "conference" {
		t.Fatalf("split segment kind = %q, want conference", splitReady.Kind)
	}
	if manager.Recordings.CallBlocked(callA.ID) || manager.Recordings.CallBlocked(callB.ID) {
		t.Fatal("split must release the conference recording fence")
	}

	// A second merge (the held leg plus the active leg) then host close: the
	// conference segment completes and every leg ends truthfully.
	conference2, err := manager.Merge(ctx, "dev_1", []string{callA.ID, callB.ID})
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	closeRec, err := manager.StartConferenceRecording(ctx, "dev_1", conference2.ID)
	if err != nil {
		t.Fatalf("third conference start: %v", err)
	}
	manager.Recordings.WriteUplink(conference2.ID, make([]int16, 160))
	manager.Recordings.WriteDownlink(conference2.ID, make([]int16, 160))
	if err := manager.CloseConferenceAsSystem(ctx, conference2.ID, "host_ended"); err != nil {
		t.Fatalf("close conference: %v", err)
	}
	waitRecordingState(t, manager, "dev_1", closeRec.ID, "ready")
	for _, id := range []string{callA.ID, callB.ID} {
		call, err := st.GetCall(ctx, id)
		if err != nil || call.EndedAt == nil {
			t.Fatalf("leg %s must be ended after host close: %+v err %v", id, call, err)
		}
	}
}

// TestConferenceRecordingLegEndKeepsSegmentPlayable proves the last-leg
// convergence path: when a member ends (worker/remote hangup shape) the
// conference segment is completed, not dropped, and the surviving call is
// immediately recordable as 1:1 again.
func TestConferenceRecordingLegEndKeepsSegmentPlayable(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	attachDummyWorkerClient(t, manager, st)
	defer manager.Recordings.CloseAll("test_end")

	callA := insertActiveCall(t, st, "line1:conv1", "dev_1", "outbound", nil)
	callB := insertActiveCall(t, st, "line1:conv2", "dev_1", "outbound", nil)
	conference, err := manager.Merge(ctx, "dev_1", []string{callA.ID, callB.ID})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	rec, err := manager.StartConferenceRecording(ctx, "dev_1", conference.ID)
	if err != nil {
		t.Fatalf("conference start: %v", err)
	}
	manager.Recordings.WriteUplink(conference.ID, make([]int16, 160))
	manager.Recordings.WriteDownlink(conference.ID, make([]int16, 160))
	if err := manager.RemoveLegAsSystem(ctx, conference.ID, callA.ID); err != nil {
		t.Fatalf("remove leg: %v", err)
	}
	waitRecordingState(t, manager, "dev_1", rec.ID, "ready")
	if manager.Recordings.CallBlocked(callB.ID) {
		t.Fatal("surviving call must be released from the conference fence")
	}
	call, err := st.GetCall(ctx, callB.ID)
	if err != nil || call.ConferenceID != "" || call.EndedAt != nil {
		t.Fatalf("survivor = %+v err %v, want standalone active call", call, err)
	}
}

// TestConferenceRecordingStorageFailureLeavesCallsUntouched proves failure
// isolation: an unavailable recording target refuses the start with an
// actionable failure while the conference, its legs and its media session
// stay exactly as they were.
func TestConferenceRecordingStorageFailureLeavesCallsUntouched(t *testing.T) {
	ctx := context.Background()
	manager, st, base := newRecordingHarness(t)
	attachDummyWorkerClient(t, manager, st)
	defer manager.Recordings.CloseAll("test_end")

	callA := insertActiveCall(t, st, "line1:confio1", "dev_1", "outbound", nil)
	callB := insertActiveCall(t, st, "line1:confio2", "dev_1", "outbound", nil)
	conference, err := manager.Merge(ctx, "dev_1", []string{callA.ID, callB.ID})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if manager.session(conference.ID) == nil {
		t.Fatal("conference media session missing before storage failure")
	}
	manager.Recordings.SetStorageProbes(
		func(string) (string, string, string, bool) { return base, "/dev/mmcblk0p1", "vfat", true },
		func(string) (int64, error) { return 0, nil },
	)
	if _, err := manager.StartConferenceRecording(ctx, "dev_1", conference.ID); err == nil {
		t.Fatal("conference start with no free space must fail")
	} else {
		var failure *Failure
		if !errors.As(err, &failure) || failure.Status != 409 {
			t.Fatalf("storage failure err = %v, want 409", err)
		}
	}
	if manager.session(conference.ID) == nil || manager.session(conference.ID).Kind != "conference" {
		t.Fatal("storage failure must not tear down conference media")
	}
	current, err := manager.GetConference(ctx, "dev_1", conference.ID)
	if err != nil || current.State != "active" || len(current.Legs) != 2 {
		t.Fatalf("conference changed on storage failure: %+v err %v", current, err)
	}
}

func TestRecordingLifecycleUpdatesCallViewTruthfully(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	call := insertActiveCall(t, st, "line1:call4", "dev_1", "inbound", nil)
	session := media.NewSession(nil, call.ID, "call")
	manager.setSession(call.ID, session)
	defer manager.endCallMedia(call.ID)

	view, err := manager.StartRecording(ctx, "dev_1", call.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	manager.Recordings.WriteUplink(call.ID, make([]int16, 160))
	manager.Recordings.WriteDownlink(call.ID, make([]int16, 160))
	liveView, err := manager.GetCall(ctx, "dev_1", call.ID)
	if err != nil {
		t.Fatalf("get call: %v", err)
	}
	if liveView.RecordingID != view.ID || liveView.RecordingState != "recording" {
		t.Fatalf("live call view recording = %s/%s", liveView.RecordingID, liveView.RecordingState)
	}
	if _, err := manager.StopRecording(ctx, "dev_1", call.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	ready := waitRecordingState(t, manager, "dev_1", view.ID, "ready")
	if ready.DurationMs != 20 {
		t.Fatalf("duration = %d, want 20ms (one 160-sample frame)", ready.DurationMs)
	}
	finalView, err := manager.GetCall(ctx, "dev_1", call.ID)
	if err != nil || finalView.RecordingState != "ready" {
		t.Fatalf("final call view = %+v err %v", finalView, err)
	}
	// Deleting the recording clears the call-row pointer state.
	if _, err := manager.DeleteRecording(ctx, "dev_1", view.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	deletedView, err := manager.GetCall(ctx, "dev_1", call.ID)
	if err != nil {
		t.Fatalf("get call after delete: %v", err)
	}
	if deletedView.RecordingID != "" || deletedView.RecordingState != "" {
		t.Fatalf("call view still points at deleted recording: %+v", deletedView)
	}
}

func TestRecordingPermissionMatrixUsesDirection(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	// Replace key_1's grant with dial-only; an inbound recording must then be
	// invisible, an outbound one visible.
	if err := st.SetKeyLine(ctx, store.KeyLine{
		KeyID: "key_1", LineID: "line1", ReceiveSMS: true, ReceiveCalls: false, SendSMS: true, Dial: true,
	}); err != nil {
		t.Fatalf("revoke receive_calls: %v", err)
	}
	now := time.Now().UTC()
	inbound := store.Recording{
		ID: "rec_in", LineID: "line1", CallID: "line1:in", KeyID: "key_1", Direction: "inbound",
		State: "ready", CreatedAt: now, UpdatedAt: now,
	}
	outbound := store.Recording{
		ID: "rec_out", LineID: "line1", CallID: "line1:out", KeyID: "key_1", Direction: "outbound",
		State: "ready", CreatedAt: now, UpdatedAt: now,
	}
	if err := st.SaveRecording(ctx, inbound); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRecording(ctx, outbound); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RecordingDetailForDevice(ctx, "dev_1", inbound.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("inbound without receive_calls err = %v, want ErrForbidden", err)
	}
	if _, err := manager.RecordingDetailForDevice(ctx, "dev_1", outbound.ID); err != nil {
		t.Fatalf("outbound with dial: %v", err)
	}
}

// registerLine adds an enabled runtime line and grants it to the given keys
// with the full four-permission set, mirroring startLine's registration.
func registerLine(t *testing.T, manager *Manager, st *store.Store, lineID string, keyIDs ...string) {
	t.Helper()
	ctx := context.Background()
	if err := st.EnsureLine(ctx, store.Line{ID: lineID, Name: lineID, Enabled: true}); err != nil {
		t.Fatalf("ensure %s: %v", lineID, err)
	}
	for _, keyID := range keyIDs {
		if err := st.SetKeyLine(ctx, store.KeyLine{
			KeyID: keyID, LineID: lineID, ReceiveSMS: true, ReceiveCalls: true, SendSMS: true, Dial: true,
		}); err != nil {
			t.Fatalf("grant %s to %s: %v", lineID, keyID, err)
		}
	}
	manager.mu.Lock()
	registered := &Line{Cfg: config.Line{ID: lineID, Name: lineID, Enabled: true}}
	if primary := manager.lines["line1"]; primary != nil {
		// Test seam: reuse the dummy worker client so Merge's media attach
		// fails fast instead of dereferencing a nil client.
		registered.Client = primary.Client
	}
	manager.lines[lineID] = registered
	manager.mu.Unlock()
}

func revokeLineGrant(t *testing.T, st *store.Store, lineID, keyID string) {
	t.Helper()
	if err := st.SetKeyLine(context.Background(), store.KeyLine{
		KeyID: keyID, LineID: lineID, ReceiveSMS: true, ReceiveCalls: false, SendSMS: true, Dial: false,
	}); err != nil {
		t.Fatalf("revoke %s: %v", lineID, err)
	}
}

// TestConferenceRecordingMemberGrantScope proves the coordinator-reviewed
// member scope: a multi-line conference segment requires the CURRENT grant of
// every member line/direction for start/list/get/audio/events, a revoked
// secondary-line grant hides the segment even while the anchor line stays
// granted, and a leg added after the segment started is included in the
// scope immediately.
func TestConferenceRecordingMemberGrantScope(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	attachDummyWorkerClient(t, manager, st)
	defer manager.Recordings.CloseAll("test_end")
	registerLine(t, manager, st, "line2", "key_1")
	registerLine(t, manager, st, "line3", "key_1")

	callA := insertActiveCall(t, st, "c_grant_a", "dev_1", "outbound", nil)
	callB := insertActiveCall(t, st, "c_grant_b", "dev_1", "outbound", func(call *store.Call) {
		call.LineID = "line2"
	})
	conference, err := manager.Merge(ctx, "dev_1", []string{callA.ID, callB.ID})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	// Secondary-line grant revoked BEFORE start: the host's key no longer
	// covers the content it would capture, so start refuses with an
	// actionable 403.
	revokeLineGrant(t, st, "line2", "key_1")
	if _, err := manager.StartConferenceRecording(ctx, "dev_1", conference.ID); err == nil {
		t.Fatal("start with revoked secondary line succeeded")
	} else {
		var failure *Failure
		if !errors.As(err, &failure) || failure.Status != 403 {
			t.Fatalf("start with revoked secondary line err = %v, want 403", err)
		}
	}
	registerLine(t, manager, st, "line2", "key_1")

	rec, err := manager.StartConferenceRecording(ctx, "dev_1", conference.ID)
	if err != nil {
		t.Fatalf("conference start: %v", err)
	}
	manager.Recordings.WriteUplink(conference.ID, make([]int16, 160))
	manager.Recordings.WriteDownlink(conference.ID, make([]int16, 160))

	// Revoked WHILE recording: list/get/audio/events all fail closed for the
	// owning key even though the anchor line1 grant is untouched. The
	// conference view redacts the recording pointer for the same reason.
	revokeLineGrant(t, st, "line2", "key_1")
	if _, err := manager.RecordingDetailForDevice(ctx, "dev_1", rec.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("detail with revoked secondary line err = %v, want ErrForbidden", err)
	}
	if list, err := manager.ListRecordings(ctx, "dev_1", "", 50); err != nil || len(list) != 0 {
		t.Fatalf("list with revoked secondary line = %+v err %v, want empty", list, err)
	}
	if _, err := manager.RecordingAudioPath(ctx, "dev_1", rec.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("audio with revoked secondary line err = %v, want ErrForbidden", err)
	}
	access, err := st.DeviceAccess(ctx, "dev_1")
	if err != nil {
		t.Fatal(err)
	}
	if manager.ConferenceRecordingScopeAllowed(ctx, access, conference.ID) {
		t.Fatal("event scope allowed with a revoked secondary line")
	}
	redacted, err := manager.GetConference(ctx, "dev_1", conference.ID)
	if err != nil {
		t.Fatalf("get conference: %v", err)
	}
	if redacted.RecordingID != "" || redacted.RecordingState != "" {
		t.Fatalf("conference view leaked recording pointers with revoked grant: %+v", redacted)
	}
	// Key scope must still win: a DIFFERENT key is not-found, not forbidden,
	// and the member matrix never leaks.
	if _, err := manager.RecordingDetailForDevice(ctx, "dev_2", rec.ID); err == nil {
		t.Fatal("different-key device read the conference recording")
	}

	// Restore the grant; the live in-call pointer comes back, then a NEW leg
	// added after the segment started is part of the scope immediately:
	// visible while line3 is granted, hidden the moment that new grant is
	// revoked.
	registerLine(t, manager, st, "line2", "key_1")
	restored, err := manager.GetConference(ctx, "dev_1", conference.ID)
	if err != nil || restored.RecordingState != "recording" {
		t.Fatalf("conference view after regrant = %+v err %v, want recording", restored, err)
	}
	if _, err := manager.RecordingDetailForDevice(ctx, "dev_1", rec.ID); err != nil {
		t.Fatalf("detail after regrant: %v", err)
	}
	callC := insertActiveCall(t, st, "c_grant_c", "dev_1", "inbound", func(call *store.Call) {
		call.LineID = "line3"
	})
	if err := st.AddConferenceLeg(ctx, store.ConferenceLeg{
		ConferenceID: conference.ID, CallID: callC.ID, JoinedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("add mid-recording leg: %v", err)
	}
	revokeLineGrant(t, st, "line3", "key_1")
	if _, err := manager.RecordingDetailForDevice(ctx, "dev_1", rec.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("detail with revoked new-leg line err = %v, want ErrForbidden", err)
	}
}

// TestConferenceRecordingConcurrentStartAndSplitNeverLeaksAnActiveSegment
// drives the real runtime Split against a concurrent manual conference start
// (the coordinator-requested start/end race). Whatever the interleaving, the
// dissolved conference must never be left with an active recording session,
// and any row the racing start created must be terminal-truthful.
func TestConferenceRecordingConcurrentStartAndSplitNeverLeaksAnActiveSegment(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	attachDummyWorkerClient(t, manager, st)
	defer manager.Recordings.CloseAll("test_end")

	for round := 0; round < 8; round++ {
		callA := insertActiveCall(t, st, fmt.Sprintf("c_ra_a%d", round), "dev_1", "outbound", nil)
		callB := insertActiveCall(t, st, fmt.Sprintf("c_ra_b%d", round), "dev_1", "outbound", nil)
		conference, err := manager.Merge(ctx, "dev_1", []string{callA.ID, callB.ID})
		if err != nil {
			t.Fatalf("round %d merge: %v", round, err)
		}
		var wg sync.WaitGroup
		var startView RecordingView
		var startErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			startView, startErr = manager.StartConferenceRecording(ctx, "dev_1", conference.ID)
		}()
		go func() {
			defer wg.Done()
			_ = manager.Split(ctx, "dev_1", conference.ID, callB.ID)
		}()
		wg.Wait()
		if manager.Recordings.Active(conference.ID) {
			t.Fatalf("round %d: dissolved conference has an active recording session", round)
		}
		if startErr == nil && startView.ID != "" {
			row, getErr := st.GetRecording(ctx, startView.ID)
			if getErr != nil {
				t.Fatalf("round %d: racing row missing: %v", round, getErr)
			}
			if row.State == "recording" || row.State == "starting" {
				t.Fatalf("round %d: racing row left in %s with no session", round, row.State)
			}
		}
	}
}

// TestConferenceRecordingFailsClosedOnMissingMemberMetadata pins the
// authorization invariant: a member leg whose call row cannot be resolved
// makes the scope check FAIL rather than silently shrinking the member set.
func TestConferenceRecordingFailsClosedOnMissingMemberMetadata(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	defer manager.Recordings.CloseAll("test_end")
	callA := insertActiveCall(t, st, "c_fc_a", "dev_1", "outbound", nil)
	now := time.Now().UTC()
	if err := st.CreateConference(ctx, store.Conference{
		ID: "conf_fc", HostDeviceID: "dev_1", State: "active", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddConferenceLeg(ctx, store.ConferenceLeg{
		ConferenceID: "conf_fc", CallID: callA.ID, JoinedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// A member row with no call: the scope must fail closed.
	if err := st.AddConferenceLeg(ctx, store.ConferenceLeg{
		ConferenceID: "conf_fc", CallID: "missing_member_call", JoinedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	access, err := st.DeviceAccess(ctx, "dev_1")
	if err != nil {
		t.Fatal(err)
	}
	if manager.ConferenceRecordingScopeAllowed(ctx, access, "conf_fc") {
		t.Fatal("scope allowed with a missing member call row")
	}
}
