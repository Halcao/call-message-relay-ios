package runtime

import (
	"context"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/store"
)

// answeredCall inserts a call the way the answer/worker path leaves it:
// owned by deviceID, answered, media session attached.
func answeredCall(t *testing.T, st *store.Store, manager *Manager, id, deviceID, direction string) store.Call {
	t.Helper()
	now := time.Now().UTC()
	call := insertActiveCall(t, st, id, deviceID, direction, func(call *store.Call) {
		call.AnsweredAt = &now
	})
	manager.setSession(call.ID, media.NewSession(nil, call.ID, "call"))
	return call
}

func autoRecordingActive(manager *Manager, callID string) bool {
	return manager.Recordings.Active(callID)
}

// waitAutoRecording polls until the auto segment is live. setSession also
// schedules the same attempt asynchronously, so tests must converge on the
// outcome instead of assuming which caller won the reservation.
func waitAutoRecording(t *testing.T, manager *Manager, callID string) store.Recording {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rec, ok := manager.Recordings.CallRecording(context.Background(), callID); ok && rec.State == "recording" {
			return rec
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("auto recording for %s never started", callID)
	return store.Recording{}
}

// assertNoAutoRecording gives the asynchronous setSession attempt a moment to
// prove it will not start.
func assertNoAutoRecording(t *testing.T, manager *Manager, callID string) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if autoRecordingActive(manager, callID) {
		t.Fatalf("auto recording unexpectedly started for %s", callID)
	}
}

// TestAutoRecordRequiresBothSwitchesAndDevicePreference: the gateway mode and
// the DEVICE preference are independent, both default off, and auto starts
// only for the owning device after the call is actually answered.
func TestAutoRecordRequiresBothSwitchesAndDevicePreference(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	defer manager.Recordings.CloseAll("test_end")
	call := answeredCall(t, st, manager, "line1:auto1", "dev_1", "outbound")

	// Default GATEWAY mode permits (unset == all); the DEVICE preference is
	// still OFF, so nothing starts. No hidden administrator prerequisite.
	if mode := st.RecordingAutoMode(ctx); mode != "all" {
		t.Fatalf("default auto mode = %q, want all", mode)
	}
	manager.maybeAutoRecord(ctx, call)
	assertNoAutoRecording(t, manager, call.ID)
	// Device opt-in alone is sufficient -> starts, owned by this device/key,
	// trigger mode auto.
	enabled := true
	if err := st.SetDeviceRecordingPrefs(ctx, "dev_1", &enabled); err != nil {
		t.Fatalf("set device pref: %v", err)
	}
	manager.maybeAutoRecord(ctx, call)
	rec := waitAutoRecording(t, manager, call.ID)
	if rec.TriggerMode != "auto" || rec.DeviceID != "dev_1" || rec.KeyID != "key_1" {
		t.Fatalf("auto recording row = %+v", rec)
	}
	// Idempotent: a second evaluation never creates a second segment.
	first := rec.ID
	manager.maybeAutoRecord(ctx, call)
	current, _ := manager.Recordings.CallRecording(ctx, call.ID)
	if current.ID != first {
		t.Fatalf("auto recording duplicated: %s -> %s", first, current.ID)
	}
}

// TestAutoRecordWaitsForAnsweredOwnership: a still-ringing/dialing call never
// auto-records, and direction filters (incoming/outgoing) are honored.
func TestAutoRecordWaitsForAnsweredOwnership(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	defer manager.Recordings.CloseAll("test_end")
	if err := st.SetRecordingAutoMode(ctx, "incoming"); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	enabled := true
	if err := st.SetDeviceRecordingPrefs(ctx, "dev_1", &enabled); err != nil {
		t.Fatalf("pref: %v", err)
	}
	// Outbound call: an incoming-only mode must not start it.
	outbound := answeredCall(t, st, manager, "line1:auto2", "dev_1", "outbound")
	manager.maybeAutoRecord(ctx, outbound)
	assertNoAutoRecording(t, manager, outbound.ID)
	// Inbound call that has not been answered yet: never starts.
	unanswered := insertActiveCall(t, st, "line1:auto3", "dev_1", "inbound", func(call *store.Call) {
		call.State = StateRinging
	})
	manager.setSession(unanswered.ID, media.NewSession(nil, unanswered.ID, "call"))
	manager.maybeAutoRecord(ctx, unanswered)
	assertNoAutoRecording(t, manager, unanswered.ID)
	// Answered inbound call starts.
	inbound := answeredCall(t, st, manager, "line1:auto4", "dev_1", "inbound")
	manager.maybeAutoRecord(ctx, inbound)
	waitAutoRecording(t, manager, inbound.ID)
}

// TestAutoRecordManualStopSuppressesRestartForSameCall: after a manual stop,
// auto never resumes that call, while the next call is unaffected.
func TestAutoRecordManualStopSuppressesRestartForSameCall(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	defer manager.Recordings.CloseAll("test_end")
	if err := st.SetRecordingAutoMode(ctx, "all"); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	enabled := true
	if err := st.SetDeviceRecordingPrefs(ctx, "dev_1", &enabled); err != nil {
		t.Fatalf("pref: %v", err)
	}
	call := answeredCall(t, st, manager, "line1:auto5", "dev_1", "outbound")
	manager.maybeAutoRecord(ctx, call)
	waitAutoRecording(t, manager, call.ID)
	if _, err := manager.StopRecording(ctx, "dev_1", call.ID); err != nil {
		t.Fatalf("manual stop: %v", err)
	}
	manager.maybeAutoRecord(ctx, call)
	assertNoAutoRecording(t, manager, call.ID)
	// A different call is not fenced.
	next := answeredCall(t, st, manager, "line1:auto6", "dev_1", "outbound")
	manager.maybeAutoRecord(ctx, next)
	waitAutoRecording(t, manager, next.ID)
}

// TestAutoRecordConferenceHostOnly: conference auto recording starts only
// for the host device; a member device can never start it.
func TestAutoRecordConferenceHostOnly(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	defer manager.Recordings.CloseAll("test_end")
	if err := st.SetRecordingAutoMode(ctx, "all"); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	enabled := true
	if err := st.SetDeviceRecordingPrefs(ctx, "dev_1", &enabled); err != nil {
		t.Fatalf("pref: %v", err)
	}
	conference := store.Conference{ID: "conf_auto", HostDeviceID: "dev_1", State: "active", CreatedAt: time.Now().UTC()}
	if err := st.CreateConference(ctx, conference); err != nil {
		t.Fatalf("create conference: %v", err)
	}
	legCall := insertActiveCall(t, st, "line1:confleg", "dev_1", "outbound", func(call *store.Call) {
		call.ConferenceID = conference.ID
	})
	if err := st.AddConferenceLeg(ctx, store.ConferenceLeg{
		ConferenceID: conference.ID, CallID: legCall.ID, JoinedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("add leg: %v", err)
	}
	manager.setSession(conference.ID, media.NewSession(nil, conference.ID, "conference"))
	manager.maybeAutoRecordConference(ctx, conference)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if manager.Recordings.Active(conference.ID) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !manager.Recordings.Active(conference.ID) {
		t.Fatal("host conference auto recording did not start")
	}
	rec, ok := manager.Recordings.CallRecording(ctx, conference.ID)
	if !ok || rec.Kind != "conference" || rec.TriggerMode != "auto" || rec.DeviceID != "dev_1" {
		t.Fatalf("conference recording row = %+v ok=%v", rec, ok)
	}
	// A member device cannot control it.
	if _, err := manager.StopConferenceRecording(ctx, "dev_2", conference.ID); err == nil {
		t.Fatal("member device stopped the host's conference recording")
	}
}

// TestRecordingSettingsReportAutoPreference keeps the client contract
// honest: gateway mode, device preference and storage all visible.
func TestRecordingSettingsReportAutoPreference(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	view, err := manager.RecordingSettings(ctx, "dev_1")
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if view.AutoMode != "all" || view.DeviceAutoRecord {
		t.Fatalf("default settings = %+v, want all/false", view)
	}
	if err := st.SetRecordingAutoMode(ctx, "outgoing"); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	enabled := true
	if err := st.SetDeviceRecordingPrefs(ctx, "dev_1", &enabled); err != nil {
		t.Fatalf("pref: %v", err)
	}
	view, err = manager.RecordingSettings(ctx, "dev_1")
	if err != nil {
		t.Fatalf("settings 2: %v", err)
	}
	if view.AutoMode != "outgoing" || !view.DeviceAutoRecord {
		t.Fatalf("settings after opt-in = %+v", view)
	}
}

// TestAutoRecordOperatorOffBeatsDeviceOptIn: an explicit operator "off" is
// honored (the device switch alone must not override it), and one opted-in
// host records while another device of the SAME key without the preference
// does not.
func TestAutoRecordOperatorOffBeatsDeviceOptIn(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	defer manager.Recordings.CloseAll("test_end")
	enabled := true
	if err := st.SetDeviceRecordingPrefs(ctx, "dev_1", &enabled); err != nil {
		t.Fatalf("pref: %v", err)
	}
	if err := st.SetRecordingAutoMode(ctx, "off"); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	call := answeredCall(t, st, manager, "line1:autoff", "dev_1", "outbound")
	manager.maybeAutoRecord(ctx, call)
	assertNoAutoRecording(t, manager, call.ID)
	// Back to the default permit: the opted-in device starts, a different
	// device of the same key without the preference does not.
	if err := st.SetRecordingAutoMode(ctx, "all"); err != nil {
		t.Fatalf("reset mode: %v", err)
	}
	manager.maybeAutoRecord(ctx, call)
	waitAutoRecording(t, manager, call.ID)
	silentCall := answeredCall(t, st, manager, "line1:autoff2", "dev_2", "outbound")
	manager.maybeAutoRecord(ctx, silentCall)
	assertNoAutoRecording(t, manager, silentCall.ID)
}

// TestAutoRecordManualStopSuppressionSurvivesRecoveryMergeSplit is the
// accepted single->conference->split regression: a manual stop keeps the
// ongoing conversation suppressed through session recovery AND across a
// merge into a hosted conference and a split back; only an explicit manual
// start (or a genuinely new call) records again.
func TestAutoRecordManualStopSuppressionSurvivesRecoveryMergeSplit(t *testing.T) {
	ctx := context.Background()
	manager, st, _ := newRecordingHarness(t)
	attachDummyWorkerClient(t, manager, st)
	defer manager.Recordings.CloseAll("test_end")
	if err := st.SetRecordingAutoMode(ctx, "all"); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	enabled := true
	if err := st.SetDeviceRecordingPrefs(ctx, "dev_1", &enabled); err != nil {
		t.Fatalf("pref: %v", err)
	}
	call := answeredCall(t, st, manager, "line1:cont1", "dev_1", "outbound")
	manager.maybeAutoRecord(ctx, call)
	waitAutoRecording(t, manager, call.ID)

	// Manual stop: the conversation is suppressed from now on.
	if _, err := manager.StopRecording(ctx, "dev_1", call.ID); err != nil {
		t.Fatalf("manual stop: %v", err)
	}

	// Route recovery: the media session is recreated for the SAME call.
	manager.dropSession(call.ID, true)
	manager.setSession(call.ID, media.NewSession(nil, call.ID, "call"))
	manager.maybeAutoRecord(ctx, call)
	assertNoAutoRecording(t, manager, call.ID)

	// Merge into a hosted conference: the inherited suppression blocks the
	// conference segment too (host + device preference still on).
	callB := answeredCall(t, st, manager, "line1:cont2", "dev_1", "outbound")
	manager.setSession(callB.ID, media.NewSession(nil, callB.ID, "call"))
	conferenceView, err := manager.Merge(ctx, "dev_1", []string{call.ID, callB.ID})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	conference, err := st.GetConference(ctx, conferenceView.ID)
	if err != nil {
		t.Fatalf("get conference: %v", err)
	}
	manager.maybeAutoRecordConference(ctx, conference)
	assertNoAutoRecording(t, manager, conference.ID)

	// Split back through the real API: the released leg keeps the
	// suppression for the continuing conversation.
	if err := manager.Split(ctx, "dev_1", conference.ID, call.ID); err != nil {
		t.Fatalf("split: %v", err)
	}
	manager.setSession(call.ID, media.NewSession(nil, call.ID, "call"))
	manager.maybeAutoRecord(ctx, call)
	assertNoAutoRecording(t, manager, call.ID)

	// An explicit manual start clears the suppression (the ONLY user action
	// that does), and a manual stop suppresses again.
	manual, err := manager.StartRecording(ctx, "dev_1", call.ID)
	if err != nil {
		t.Fatalf("explicit manual start: %v", err)
	}
	if manual.TriggerMode != "manual" {
		t.Fatalf("manual trigger mode = %q", manual.TriggerMode)
	}
	manager.maybeAutoRecord(ctx, call)
	// Still the manual segment, no duplicate.
	current, _ := manager.Recordings.CallRecording(ctx, call.ID)
	if current.ID != manual.ID {
		t.Fatalf("auto started a duplicate segment: manual=%s current=%s", manual.ID, current.ID)
	}
}
