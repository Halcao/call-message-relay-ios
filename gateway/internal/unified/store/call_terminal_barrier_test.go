package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestUpsertLiveCallBarrierProtectsTerminalTruth deterministically reproduces
// the production terminal-overwrite race at the SQL boundary:
//
//  1. a worker convergence snapshot is read while the call is still live;
//  2. another finisher (HTTP hangup / worker ended event / policy) wins the
//     atomic terminal claim (ended_at + end_reason set);
//  3. the stale snapshot then attempts its full-row convergence write.
//
// The write must be a no-op: the row may neither be resurrected (state back
// to active, ended_at cleared) nor lose end_reason / answered_at. Before the
// barrier this exact interleaving resurrected calls and dropped endReason.
func TestUpsertLiveCallBarrierProtectsTerminalTruth(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	created := time.Now().Add(-time.Minute).UTC()
	answered := created.Add(10 * time.Second)
	live := Call{
		ID: "line1:call-term", LineID: "line1", WorkerCallID: "call-term",
		DeviceID: "dev-owner", Direction: "outbound", Peer: "10086",
		State: "active", CreatedAt: created, AnsweredAt: &answered,
	}
	if err := st.UpsertCall(ctx, live); err != nil {
		t.Fatalf("insert live call: %v", err)
	}

	// Step 1: a worker snapshot taken BEFORE the terminal claim. It is a
	// full-row value object (active, no ended values), exactly like the
	// handleCallEvent convergence write that overwrote truth in production.
	stale := live
	stale.Peer = "10086"
	stale.State = "active"
	stale.EndedAt = nil
	stale.EndReason = ""

	// Step 2: the terminal claim lands first (HTTP hangup path).
	endedAt := time.Now().UTC()
	won, err := st.FinishCallIfOpen(ctx, live.ID, endedAt, "hungup", live.AnsweredAt)
	if err != nil || !won {
		t.Fatalf("terminal claim = %v/%v", won, err)
	}

	// Step 3: the stale snapshot write lands after the claim.
	if err := st.UpsertLiveCall(ctx, stale); !errors.Is(err, ErrCallAlreadyEnded) {
		t.Fatalf("stale post-terminal write: err = %v, want ErrCallAlreadyEnded", err)
	}

	row, err := st.GetCall(ctx, live.ID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if row.State != "ended" {
		t.Fatalf("stale snapshot resurrected state = %q", row.State)
	}
	if row.EndedAt == nil || row.EndedAt.UnixMilli() != endedAt.UnixMilli() {
		t.Fatalf("ended_at lost/mutated: %v", row.EndedAt)
	}
	if row.EndReason != "hungup" {
		t.Fatalf("end_reason lost: %q", row.EndReason)
	}
	if row.AnsweredAt == nil || row.AnsweredAt.UnixMilli() != answered.UnixMilli() {
		t.Fatalf("answered_at lost: %v", row.AnsweredAt)
	}
	if row.DeviceID != "dev-owner" {
		t.Fatalf("device owner lost: %q", row.DeviceID)
	}

	// Only the first claim wins: a second finisher must not overwrite truth.
	later := endedAt.Add(time.Second)
	won2, err := st.FinishCallIfOpen(ctx, live.ID, later, "media_timeout", nil)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if won2 {
		t.Fatal("a second terminal claim won an already-ended call")
	}
	row, _ = st.GetCall(ctx, live.ID)
	if row.EndedAt.UnixMilli() != endedAt.UnixMilli() || row.EndReason != "hungup" {
		t.Fatalf("losing finisher overwrote terminal truth: %+v", row)
	}
}

// TestUpsertLiveCallMonotonicAnswerAndOwner verifies live convergence writes
// never move answered_at backwards or clear the device that owns an answering
// claim, while legitimate field changes (state, held, peer, conference_id)
// still converge.
func TestUpsertLiveCallMonotonicAnswerAndOwner(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	created := time.Now().Add(-time.Minute).UTC()
	first := created.Add(5 * time.Second)
	later := first.Add(20 * time.Second)

	ringing := Call{
		ID: "line1:call-mono", LineID: "line1", WorkerCallID: "call-mono",
		Direction: "inbound", Peer: "10086", State: "incoming_ringing", CreatedAt: created,
	}
	if err := st.UpsertCall(ctx, ringing); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if won, err := st.ClaimCall(ctx, ringing.ID, "dev-a"); err != nil || !won {
		t.Fatalf("claim = %v/%v", won, err)
	}

	// A ringing worker snapshot (empty device_id) converges state/peer but
	// must not clear the answering owner.
	snapshot := ringing
	snapshot.State = "incoming_ringing"
	snapshot.DeviceID = ""
	if err := st.UpsertLiveCall(ctx, snapshot); err != nil {
		t.Fatalf("ringing convergence: %v", err)
	}
	if row, _ := st.GetCall(ctx, ringing.ID); row.DeviceID != "dev-a" {
		t.Fatalf("answering owner cleared by ringing snapshot: %q", row.DeviceID)
	}

	// connectedAt lands; then a later snapshot carrying a DIFFERENT (clock
	// skew) answer time and a nil-answer snapshot must both keep the first.
	answered := ringing
	answered.State = "active"
	answered.AnsweredAt = &first
	if err := st.UpsertLiveCall(ctx, answered); err != nil {
		t.Fatalf("answered write: %v", err)
	}
	skewed := answered
	skewed.AnsweredAt = &later
	if err := st.UpsertLiveCall(ctx, skewed); err != nil {
		t.Fatalf("skewed write: %v", err)
	}
	nilAnswer := answered
	nilAnswer.AnsweredAt = nil
	nilAnswer.Held = true
	nilAnswer.State = "held"
	if err := st.UpsertLiveCall(ctx, nilAnswer); err != nil {
		t.Fatalf("nil-answer convergence: %v", err)
	}
	row, _ := st.GetCall(ctx, ringing.ID)
	if row.AnsweredAt == nil || row.AnsweredAt.UnixMilli() != first.UnixMilli() {
		t.Fatalf("answered_at not monotonic (want %v): %v", first, row.AnsweredAt)
	}
	if !row.Held || row.State != "held" {
		t.Fatalf("legitimate convergence fields dropped: %+v", row)
	}
	if row.DeviceID != "dev-a" {
		t.Fatalf("owner changed: %q", row.DeviceID)
	}
}

// TestFinishCallIfOpenRecordsAnsweredAtInTerminalSnapshot covers the observed
// ordering where connectedAt first appears in the SAME terminal worker
// snapshot (no preceding live update): the terminal claim itself must persist
// answered_at, monotonically.
func TestFinishCallIfOpenRecordsAnsweredAtInTerminalSnapshot(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	created := time.Now().Add(-time.Minute).UTC()
	call := Call{
		ID: "line1:call-ans", LineID: "line1", WorkerCallID: "call-ans",
		Direction: "outbound", Peer: "10086", State: "connecting", CreatedAt: created,
	}
	if err := st.UpsertCall(ctx, call); err != nil {
		t.Fatalf("insert: %v", err)
	}
	answered := created.Add(30 * time.Second)
	endedAt := answered.Add(time.Second)
	if won, err := st.FinishCallIfOpen(ctx, call.ID, endedAt, "remote_ended", &answered); err != nil || !won {
		t.Fatalf("terminal claim = %v/%v", won, err)
	}
	row, _ := st.GetCall(ctx, call.ID)
	if row.AnsweredAt == nil || row.AnsweredAt.UnixMilli() != answered.UnixMilli() {
		t.Fatalf("answered_at missing on terminal claim: %v", row.AnsweredAt)
	}
	if row.EndReason != "remote_ended" || row.EndedAt == nil {
		t.Fatalf("terminal columns wrong: %+v", row)
	}
}

// TestFinishConferenceLegKeepsMetadataAndClaimsOnce verifies conference leg
// termination preserves answered_at/owner, clears the conference link like
// the old full-row write, fills end_reason only when empty, and is a
// single-winner claim.
func TestFinishConferenceLegKeepsMetadataAndClaimsOnce(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	created := time.Now().Add(-time.Minute).UTC()
	answered := created.Add(10 * time.Second)
	leg := Call{
		ID: "line1:call-leg", LineID: "line1", WorkerCallID: "call-leg",
		DeviceID: "dev-host", Direction: "outbound", Peer: "10086",
		State: "active", ConferenceID: "conf_1", CreatedAt: created, AnsweredAt: &answered,
	}
	if err := st.UpsertCall(ctx, leg); err != nil {
		t.Fatalf("insert: %v", err)
	}
	endedAt := time.Now().UTC()
	won, err := st.FinishConferenceLeg(ctx, leg.ID, endedAt, "removed_from_conference")
	if err != nil || !won {
		t.Fatalf("leg claim = %v/%v", won, err)
	}
	row, _ := st.GetCall(ctx, leg.ID)
	if row.State != "ended" || row.EndedAt == nil || row.ConferenceID != "" {
		t.Fatalf("terminal leg columns wrong: %+v", row)
	}
	if row.EndReason != "removed_from_conference" {
		t.Fatalf("end reason = %q", row.EndReason)
	}
	if row.AnsweredAt == nil || row.AnsweredAt.UnixMilli() != answered.UnixMilli() || row.DeviceID != "dev-host" {
		t.Fatalf("leg metadata lost: %+v", row)
	}
	// A racing worker-ended finisher must not win or rewrite the reason.
	won2, err := st.FinishConferenceLeg(ctx, leg.ID, endedAt.Add(time.Second), "remote_ended")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if won2 {
		t.Fatal("second conference-leg finisher won")
	}
	row, _ = st.GetCall(ctx, leg.ID)
	if row.EndReason != "removed_from_conference" {
		t.Fatalf("losing finisher rewrote end reason: %q", row.EndReason)
	}
}

// TestReleaseCallClaimOnlyForOwningAnswer verifies the answer-rollback path
// can only undo this device's still-answering claim.
func TestReleaseCallClaimOnlyForOwningAnswer(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	created := time.Now().UTC()
	call := Call{
		ID: "line1:call-rel", LineID: "line1", WorkerCallID: "call-rel",
		Direction: "inbound", Peer: "10086", State: "incoming_ringing", CreatedAt: created,
	}
	if err := st.UpsertCall(ctx, call); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if won, _ := st.ClaimCall(ctx, call.ID, "dev-a"); !won {
		t.Fatal("claim failed")
	}
	// A different device cannot release dev-a's claim.
	if err := st.ReleaseCallClaim(ctx, call.ID, "dev-b"); err != nil {
		t.Fatalf("release: %v", err)
	}
	row, _ := st.GetCall(ctx, call.ID)
	if row.State != "answering" || row.DeviceID != "dev-a" {
		t.Fatalf("foreign release mutated claim: %+v", row)
	}
	// The owning device rolls back after a failed upstream Answer.
	if err := st.ReleaseCallClaim(ctx, call.ID, "dev-a"); err != nil {
		t.Fatalf("owner release: %v", err)
	}
	row, _ = st.GetCall(ctx, call.ID)
	if row.State != "incoming_ringing" || row.DeviceID != "" {
		t.Fatalf("rollback wrong: %+v", row)
	}
	// Releasing a terminal row is a no-op.
	endedAt := time.Now().UTC()
	if won, _ := st.ClaimCall(ctx, call.ID, "dev-c"); !won {
		t.Fatal("reclaim failed")
	}
	if _, err := st.FinishCallIfOpen(ctx, call.ID, endedAt, "hungup", nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := st.ReleaseCallClaim(ctx, call.ID, "dev-c"); err != nil {
		t.Fatalf("terminal release: %v", err)
	}
	row, _ = st.GetCall(ctx, call.ID)
	if row.State != "ended" || row.EndedAt == nil {
		t.Fatalf("release resurrected terminal row: %+v", row)
	}
}

// TestFinishCallIfOpenNeverPersistsAnEmptyReason pins the terminal invariant
// for the OTHER finisher: a worker terminal snapshot whose reason is empty
// (the real worker's two-step hangup sequence can win against the unified HTTP
// hangup) must still leave an ended call with a reason. Deterministic: the
// claim itself is driven directly, so no timing luck is involved.
func TestFinishCallIfOpenNeverPersistsAnEmptyReason(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	created := time.Now().Add(-time.Minute).UTC()
	answered := created.Add(10 * time.Second)
	live := Call{
		ID: "line1:call-blank", LineID: "line1", WorkerCallID: "call-blank",
		DeviceID: "dev-owner", Direction: "outbound", Peer: "10086",
		State: "active", CreatedAt: created, AnsweredAt: &answered,
	}
	if err := st.UpsertCall(ctx, live); err != nil {
		t.Fatalf("seed live call: %v", err)
	}

	won, err := st.FinishCallIfOpen(ctx, live.ID, time.Now().UTC(), "", live.AnsweredAt)
	if err != nil || !won {
		t.Fatalf("terminal claim: won=%v err=%v", won, err)
	}
	stored, err := st.GetCall(ctx, live.ID)
	if err != nil {
		t.Fatalf("get call: %v", err)
	}
	if stored.State != "ended" || stored.EndedAt == nil {
		t.Fatalf("call not terminal: %+v", stored)
	}
	if stored.EndReason == "" {
		t.Fatalf("terminal claim persisted an EMPTY end reason: %+v", stored)
	}
	if stored.AnsweredAt == nil {
		t.Fatalf("terminal claim lost answered_at: %+v", stored)
	}

	// A LATER claim can never rewrite the terminal row (single claim), so the
	// recorded reason stays stable.
	won, err = st.FinishCallIfOpen(ctx, live.ID, time.Now().UTC(), "device_hangup", live.AnsweredAt)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if won {
		t.Fatalf("a second terminal claim won on an already-ended call")
	}
	again, _ := st.GetCall(ctx, live.ID)
	if again.EndReason != stored.EndReason {
		t.Fatalf("terminal reason changed: %q -> %q", stored.EndReason, again.EndReason)
	}

	// A REAL reason is never replaced by the neutral fallback.
	live2 := Call{
		ID: "line1:call-real", LineID: "line1", WorkerCallID: "call-real",
		DeviceID: "dev-owner", Direction: "outbound", Peer: "10086",
		State: "active", CreatedAt: created, AnsweredAt: &answered,
	}
	if err := st.UpsertCall(ctx, live2); err != nil {
		t.Fatalf("seed live call 2: %v", err)
	}
	if _, err := st.FinishCallIfOpen(ctx, live2.ID, time.Now().UTC(), "remote_hangup", live2.AnsweredAt); err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	second, _ := st.GetCall(ctx, live2.ID)
	if second.EndReason != "remote_hangup" {
		t.Fatalf("real reason lost: %q", second.EndReason)
	}
}
