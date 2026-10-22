package runtime

import (
	"context"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/store"
)

func TestBarkCallStillFresh(t *testing.T) {
	now := time.Now().UTC()
	maxAge := time.Minute
	ringing := store.Call{ID: "line1:c1", State: StateRinging, CreatedAt: now.Add(-5 * time.Second)}
	if !barkCallStillFresh(ringing, now, maxAge) {
		t.Fatal("a recent ringing call is fresh")
	}
	stale := ringing
	stale.CreatedAt = now.Add(-2 * time.Minute)
	if barkCallStillFresh(stale, now, maxAge) {
		t.Fatal("a call ringing past maxAge must be suppressed as stale")
	}
	ended := ringing
	ended.EndedAt = &now
	if barkCallStillFresh(ended, now, maxAge) {
		t.Fatal("an ended call must never notify")
	}
	active := ringing
	active.State = StateActive
	if barkCallStillFresh(active, now, maxAge) {
		t.Fatal("only ringing calls may notify")
	}
	// A zero/unknown maxAge disables only the age bound, not the state guard.
	oldButRinging := ringing
	oldButRinging.CreatedAt = now.Add(-24 * time.Hour)
	if !barkCallStillFresh(oldButRinging, now, 0) {
		t.Fatal("zero maxAge must not suppress a ringing call on age alone")
	}
	if barkCallStillFresh(ended, now, 0) {
		t.Fatal("ended must stay suppressed with zero maxAge")
	}
	// Timestamps may be missing on unusual worker snapshots; state still rules.
	noTimestamp := store.Call{ID: "line1:c2", State: StateRinging}
	if !barkCallStillFresh(noTimestamp, now, maxAge) {
		t.Fatal("a ringing call without a timestamp must not be dropped")
	}
}

func TestPushBarkIncomingIsOptionalAndNoopWhenDisabled(t *testing.T) {
	// Nil client / disabled config: the native path is untouched and no store
	// access (zero-value Manager has no store) may panic.
	manager := &Manager{}
	manager.pushBarkIncoming(context.Background(), store.Call{ID: "line1:c1", State: StateRinging})
}
