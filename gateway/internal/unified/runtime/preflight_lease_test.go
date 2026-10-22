package runtime

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/media"
)

// Lease-fence regressions for the device-scoped preflight registry.
//
// Bug class being fenced: RenewPreflight used time.Timer.Reset under m.mu.
// Reset cannot cancel an expiry callback that already fired and is blocked on
// m.mu, so the old callback could delete/close a lease that a successful
// renewal had just extended. These tests are deterministic: they invoke the
// exact callback body (expirePreflight) with the generation captured at arm
// time, instead of sleeping on a real timer race.

func isFailureStatus(err error, status int) bool {
	var failure *Failure
	return errors.As(err, &failure) && failure.Status == status
}

// newLeaseManager returns a Manager with the registry state the lease fence
// operates on; no store/engine is needed for pure lease arbitration.
func newLeaseManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(config.Default(), nil, nil, nil, nil, nil)
	t.Cleanup(func() {
		m.mu.Lock()
		entries := make([]*preflightEntry, 0, len(m.preflights))
		for _, entry := range m.preflights {
			entries = append(entries, entry)
		}
		m.preflights = make(map[string]*preflightEntry)
		m.mu.Unlock()
		for _, entry := range entries {
			entry.probe.timer.Stop()
			entry.probe.session.Close()
		}
	})
	return m
}

// seedLease publishes a lease through the same helper AttachPreflight uses, so
// the test exercises the production arming/fencing code (including the
// generation handed to the real expiry callback).
func seedLease(t *testing.T, m *Manager, deviceID string) (*preflightEntry, *media.Session) {
	t.Helper()
	probe := media.NewSession(nil, "preflight:"+deviceID, "probe")
	probe.SetProbeEcho(true)
	m.mu.Lock()
	entry := m.registerPreflightLocked(deviceID, probe)
	m.mu.Unlock()
	t.Cleanup(probe.Close)
	return entry, probe
}

// TestPreflightOldExpiryAfterRenewIsFenced is the exact regression for the
// delivered bug: the pre-renewal timer callback runs AFTER RenewPreflight
// returned 200. The generation fence must make it a no-op so the renewed
// lease stays registered, adoptable and renewable.
func TestPreflightOldExpiryAfterRenewIsFenced(t *testing.T) {
	m := newLeaseManager(t)
	entry, probe := seedLease(t, m, "dev-old-expiry")
	oldGen := entry.gen

	ttl, err := m.RenewPreflight("dev-old-expiry", entry.id)
	if err != nil || ttl != ProbeTTL.Milliseconds() {
		t.Fatalf("renew ttl=%d err=%v", ttl, err)
	}
	if !m.HasPreflight("dev-old-expiry") {
		t.Fatal("renewal dropped the lease")
	}

	// The original timer fired at the ORIGINAL deadline; its callback is only
	// now acquiring m.mu. Reset could not have cancelled it.
	m.expirePreflight("dev-old-expiry", entry.id, probe, oldGen)
	if !m.HasPreflight("dev-old-expiry") {
		t.Fatal("pre-renewal expiry callback reclaimed the freshly renewed lease")
	}
	// Commit ownership is preserved: the renewed candidate is still the one
	// exactly-once claim hands to the call.
	claimed := m.claimPreflight("dev-old-expiry", entry.id)
	if claimed == nil || claimed.session != probe {
		t.Fatal("renewed lease is no longer the registered adoptable candidate")
	}
	// The pre-renewal callback running again after the commit claim must not
	// recreate or close anything claimable.
	m.expirePreflight("dev-old-expiry", entry.id, probe, oldGen)
	if m.claimPreflight("dev-old-expiry", entry.id) != nil {
		t.Fatal("stale expiry callback resurrected a claimed entry")
	}

	// A lease that was renewed and never claimed still gets reaped by its
	// CURRENT generation's callback (the fence does not leak).
	entry2, probe2 := seedLease(t, m, "dev-current-expiry")
	if _, err := m.RenewPreflight("dev-current-expiry", entry2.id); err != nil {
		t.Fatalf("renew: %v", err)
	}
	m.expirePreflight("dev-current-expiry", entry2.id, probe2, entry2.gen)
	if m.HasPreflight("dev-current-expiry") {
		t.Fatal("current-generation expiry callback failed to reclaim the lease")
	}
}

// TestPreflightExpiredLeaseCannotRenew pins the deadline half of the fence: a
// lease whose deadline has passed must not be resurrected by a renewal even
// if its callback has not yet acquired m.mu. The lapsed lease stays for its
// callback to reclaim; a fresh attach starts a new negotiable lease.
func TestPreflightExpiredLeaseCannotRenew(t *testing.T) {
	m := newLeaseManager(t)
	entry, probe := seedLease(t, m, "dev-expired")
	m.mu.Lock()
	entry.deadline = time.Now().Add(-time.Millisecond)
	m.mu.Unlock()

	if _, err := m.RenewPreflight("dev-expired", entry.id); !isFailureStatus(err, 404) {
		t.Fatalf("expired renew err=%v, want 404", err)
	}
	if !m.HasPreflight("dev-expired") {
		t.Fatal("refused renewal must leave the lapsed lease to its expiry callback")
	}
	m.expirePreflight("dev-expired", entry.id, probe, entry.gen)
	if m.HasPreflight("dev-expired") {
		t.Fatal("lapsed lease was not reclaimed by its expiry callback")
	}
	if _, err := m.RenewPreflight("dev-expired", entry.id); !isFailureStatus(err, 404) {
		t.Fatalf("post-expiry renew err=%v, want 404", err)
	}
	entry2, _ := seedLease(t, m, "dev-expired")
	if _, err := m.RenewPreflight("dev-expired", entry2.id); err != nil {
		t.Fatalf("fresh lease must be renewable: %v", err)
	}
}

// TestPreflightDiscardIsUnconditionalAcrossRenewals: explicit discard is not
// generation-gated. After several renewals a cancel must remove the lease,
// close it once and make it unclaimable/unrenewable; stale expiry callbacks
// and wrong-id cancels are no-ops.
func TestPreflightDiscardIsUnconditionalAcrossRenewals(t *testing.T) {
	m := newLeaseManager(t)
	entry, probe := seedLease(t, m, "dev-discard")
	oldGen := entry.gen
	for i := 0; i < 3; i++ {
		if _, err := m.RenewPreflight("dev-discard", entry.id); err != nil {
			t.Fatalf("renew %d: %v", i, err)
		}
	}
	// A pre-renewal expiry callback racing the explicit cancel stays a no-op.
	m.expirePreflight("dev-discard", entry.id, probe, oldGen)
	if !m.HasPreflight("dev-discard") {
		t.Fatal("stale expiry reclaimed a renewed lease before explicit discard")
	}

	m.DiscardPreflight("dev-discard", entry.id)
	if m.HasPreflight("dev-discard") {
		t.Fatal("explicit discard did not remove the renewed lease")
	}
	if claimed := m.claimPreflight("dev-discard", entry.id); claimed != nil {
		t.Fatal("discarded lease still claimable by a commit")
	}
	if _, err := m.RenewPreflight("dev-discard", entry.id); !isFailureStatus(err, 404) {
		t.Fatalf("renew after discard err=%v, want 404", err)
	}
	// Expiry and re-discard after the explicit path remain no-ops (idempotent).
	m.expirePreflight("dev-discard", entry.id, probe, entry.gen)
	m.DiscardPreflight("dev-discard", entry.id)

	entry2, _ := seedLease(t, m, "dev-discard")
	m.DiscardPreflight("dev-discard", "prb_wrong_id")
	if !m.HasPreflight("dev-discard") {
		t.Fatal("wrong-id discard removed the registered lease")
	}
	if _, err := m.RenewPreflight("dev-discard", entry2.id); err != nil {
		t.Fatalf("live lease became unrenewable: %v", err)
	}
}

// TestPreflightCommitClaimBeatsRenewAndExpiry: the commit claim removes the
// entry first (exactly-once adoption), so a concurrent renewal gets 404 and a
// pre-claim expiry callback cannot touch the probe the commit now owns.
func TestPreflightCommitClaimBeatsRenewAndExpiry(t *testing.T) {
	m := newLeaseManager(t)
	entry, probe := seedLease(t, m, "dev-claim")
	oldGen := entry.gen

	claimed := m.claimPreflight("dev-claim", entry.id)
	if claimed == nil || claimed.session != probe {
		t.Fatal("claim did not hand over the registered probe")
	}
	if m.HasPreflight("dev-claim") {
		t.Fatal("claimed lease stayed registered (double adoption possible)")
	}
	if _, err := m.RenewPreflight("dev-claim", entry.id); !isFailureStatus(err, 404) {
		t.Fatalf("renew after commit claim err=%v, want 404", err)
	}
	m.expirePreflight("dev-claim", entry.id, probe, oldGen)
	if claimed2 := m.claimPreflight("dev-claim", entry.id); claimed2 != nil {
		t.Fatal("stale expiry callback resurrected a claimed lease")
	}
}

// TestPreflightRenewalOutlivesOriginalDeadline exercises the real armed timer
// end-to-end with a compressed TTL: after a renewal the ORIGINAL generation's
// callback fires and must not reap the extended lease, while the renewed
// deadline still reaps it.
func TestPreflightRenewalOutlivesOriginalDeadline(t *testing.T) {
	originalTTL := ProbeTTL
	ProbeTTL = 500 * time.Millisecond
	t.Cleanup(func() { ProbeTTL = originalTTL })

	m := newLeaseManager(t)
	entry, _ := seedLease(t, m, "dev-timer")
	time.Sleep(100 * time.Millisecond)
	if _, err := m.RenewPreflight("dev-timer", entry.id); err != nil {
		t.Fatalf("renew: %v", err)
	}
	// Past the ORIGINAL deadline (500 ms from attach), still inside the
	// renewed window (500 ms from renewal). Sleep 400 ms after the renewal
	// so the check lands at the *original* deadline with real headroom before
	// the renewed one; sleeping the full TTL landed exactly ON the renewed
	// deadline and made the assertion a scheduler coin flip under load.
	time.Sleep(400 * time.Millisecond)
	if !m.HasPreflight("dev-timer") {
		t.Fatal("original-generation timer reaped the renewed lease")
	}
	// Past the renewed deadline: reaped exactly once, then unrenewable.
	time.Sleep(500 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for m.HasPreflight("dev-timer") {
		if time.Now().After(deadline) {
			t.Fatal("renewed lease was never reaped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := m.RenewPreflight("dev-timer", entry.id); !isFailureStatus(err, 404) {
		t.Fatalf("post-expiry renew err=%v, want 404", err)
	}
}

// TestPreflightLeaseRaceRenewCancelExpiry hammers renew/cancel/expiry
// concurrently across independent devices (run under -race). Invariants: the
// registry never contains a half-built lease, and whenever a lease survives
// the concurrent reclaim attempts it is still the exact registered candidate,
// claimable exactly once by the post-round commit-style claim.
func TestPreflightLeaseRaceRenewCancelExpiry(t *testing.T) {
	m := newLeaseManager(t)
	const rounds = 60
	for round := 0; round < rounds; round++ {
		device := fmt.Sprintf("dev-lease-race-%d", round)
		entry, probe := seedLease(t, m, device)
		oldGen := entry.gen

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(4)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 3; i++ {
				_, _ = m.RenewPreflight(device, entry.id)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			// Stale callback: armed generation from before any renewal.
			m.expirePreflight(device, entry.id, probe, oldGen)
		}()
		go func() {
			defer wg.Done()
			<-start
			m.mu.Lock()
			gen := entry.gen
			m.mu.Unlock()
			m.expirePreflight(device, entry.id, probe, gen)
		}()
		go func() {
			defer wg.Done()
			<-start
			m.DiscardPreflight(device, entry.id)
		}()
		close(start)
		wg.Wait()

		if m.HasPreflight(device) {
			// Only a renewal that beat every reclaim may survive, and it must
			// still be the exact registered candidate for a commit claim.
			claimed := m.claimPreflight(device, entry.id)
			if claimed == nil || claimed.session != probe {
				t.Fatalf("round %d: surviving lease is not the registered candidate", round)
			}
		}
	}
}
