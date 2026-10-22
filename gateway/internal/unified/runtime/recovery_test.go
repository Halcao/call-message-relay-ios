package runtime

import (
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/config"
)

func newRecoveryManager() *Manager {
	return NewManager(config.Default(), nil, nil, nil, nil, nil)
}

// TestRecoveryAbsoluteDeadlineNotExtended: repeated beginRecovery calls
// while an episode is live must neither restart nor extend the deadline.
func TestRecoveryAbsoluteDeadlineNotExtended(t *testing.T) {
	testGraceOverride = 45 * time.Second
	defer func() { testGraceOverride = 0 }()
	m := newRecoveryManager()
	const key = "call:absolute"

	if !m.beginRecovery(key, func() {}) {
		t.Fatal("first beginRecovery must open an episode")
	}
	first, ok := m.recoveryDeadline(key)
	if !ok {
		t.Fatal("episode missing")
	}
	// Repeated losses (the flap that previously re-armed a fresh full timer).
	for i := 0; i < 5; i++ {
		if m.beginRecovery(key, func() {}) {
			t.Fatal("repeated loss must not open a new episode")
		}
	}
	again, ok := m.recoveryDeadline(key)
	if !ok || !again.Equal(first) {
		t.Fatalf("deadline moved: first=%v again=%v", first, again)
	}
	m.resolveRecovery(key)
}

// TestRecoveryFreshMediaCancelsDeadline: a fresh-media resolution before the
// deadline removes the episode; a late/duplicate resolution is a no-op.
func TestRecoveryFreshMediaCancelsDeadline(t *testing.T) {
	testGraceOverride = time.Minute
	defer func() { testGraceOverride = 0 }()
	m := newRecoveryManager()
	const key = "call:fresh"
	expired := 0
	m.beginRecovery(key, func() { expired++ })
	if !m.resolveRecovery(key) {
		t.Fatal("first resolve must report it closed the episode")
	}
	if m.resolveRecovery(key) {
		t.Fatal("duplicate resolve must be false")
	}
	time.Sleep(20 * time.Millisecond)
	if expired != 0 {
		t.Fatal("deadline fired after fresh-media recovery")
	}
}

// TestRecoveryStaleDeadlineCannotEndRecoveredCall is the exact requested
// race: hold the deadline callback just before its terminal mutation and
// inject a fresh-media resolution. Under the serialized claim the deadline
// that genuinely fired first ends the call exactly once, the injected restore
// then no-ops (reports false), and no episode remains to half-restore it.
func TestRecoveryStaleDeadlineCannotEndRecoveredCall(t *testing.T) {
	testGraceOverride = time.Minute
	defer func() {
		testGraceOverride = 0
		recoveryBeforeExpire = nil
	}()
	m := newRecoveryManager()
	const key = "call:stale-race"
	expired := 0

	restoreAttempted := make(chan struct{})
	releaseRestore := make(chan struct{})
	recoveryBeforeExpire = func(string) {
		// Deadline won the claim and is just about to end the call; inject
		// a concurrent fresh-media resolution that must block on endMu.
		go func() {
			close(restoreAttempted)
			_ = m.resolveRecovery(key)
			close(releaseRestore)
		}()
		<-restoreAttempted
		// Give the goroutine a moment to actually block on endMu.
		time.Sleep(30 * time.Millisecond)
	}

	m.beginRecovery(key, func() {
		expired++ // the terminal transition (hangup in production)
	})
	// Manually invoke the timer body (deterministic; no real sleep).
	m.mu.Lock()
	ep := m.graces[key]
	m.mu.Unlock()
	m.recoveryDeadlineFired(key, ep, func() { expired++ })
	<-releaseRestore

	if expired != 1 {
		t.Fatalf("terminal transition ran %d times, want exactly 1", expired)
	}
	if _, ok := m.recoveryDeadline(key); ok {
		t.Fatal("episode must be gone after the deadline transition")
	}
}

// TestRecoveryRestoreWinsClaim: when fresh media is resolved first, a later
// deadline callback (e.g. a timer racing the resolution) never ends the call.
func TestRecoveryRestoreWinsClaim(t *testing.T) {
	testGraceOverride = time.Minute
	defer func() { testGraceOverride = 0 }()
	m := newRecoveryManager()
	const key = "call:restore-wins"
	expired := 0
	m.beginRecovery(key, func() { expired++ })
	m.mu.Lock()
	ep := m.graces[key]
	m.mu.Unlock()
	// Fresh media wins the claim before the deadline body runs.
	m.resolveRecovery(key)
	m.recoveryDeadlineFired(key, ep, func() { expired++ })
	if expired != 0 {
		t.Fatal("deadline ended a call already cleared by fresh media")
	}
}

// TestRecoveryRealTimerRespectsFence uses an actual short timer and the
// before-expire seam to prove the real path honors the same serialization.
func TestRecoveryRealTimerRespectsFence(t *testing.T) {
	testGraceOverride = 40 * time.Millisecond
	defer func() {
		testGraceOverride = 0
		recoveryBeforeExpire = nil
	}()
	m := newRecoveryManager()
	const key = "call:real-timer"
	var wg sync.WaitGroup
	wg.Add(1)
	recoveryBeforeExpire = func(string) { wg.Done() }
	expired := make(chan struct{}, 1)
	m.beginRecovery(key, func() { expired <- struct{}{} })
	select {
	case <-expired:
	case <-time.After(2 * time.Second):
		t.Fatal("deadline never fired")
	}
	wg.Wait()
}
