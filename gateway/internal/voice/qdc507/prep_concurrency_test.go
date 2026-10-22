package qdc507

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"callrelay/gateway/internal/modem"
)

func TestPrepFlightSingleLeaderUnderConcurrency(t *testing.T) {
	var f prepFlight
	const n = 16
	gate := make(chan struct{})
	var leaders int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			leader, err := f.begin(context.Background())
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			if leader {
				atomic.AddInt32(&leaders, 1)
				time.Sleep(50 * time.Millisecond)
				f.finish()
			}
		}()
	}
	close(gate)
	wg.Wait()
	if got := atomic.LoadInt32(&leaders); got != 1 {
		t.Fatalf("leaders = %d, want exactly 1", got)
	}
}

func TestPrepFlightCancelWhileWaiting(t *testing.T) {
	var f prepFlight
	leader, err := f.begin(context.Background())
	if err != nil || !leader {
		t.Fatalf("first begin = %v, %v", leader, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, beginErr := f.begin(ctx)
		result <- beginErr
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case got := <-result:
		if got != context.Canceled {
			t.Fatalf("waiting begin error = %v, want context.Canceled", got)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter never returned")
	}
	// The flight stays owned by the first leader until finished.
	f.finish()
}

// TestPreheatSkippedWhenCallOwnerMarked pins the active-call fence: a
// preheat arriving once a call owner exists returns without rotating,
// so a rotation can never hit a starting or active host pair.
func TestPreheatSkippedWhenCallOwnerMarked(t *testing.T) {
	a := &Audio{cfg: Config{Bootstrap: true}}
	a.mu.Lock()
	a.call = "call-owner-1"
	a.mu.Unlock()
	if err := a.PrepareRoute(context.Background()); err != nil {
		t.Fatalf("preheat with an owner marked: %v", err)
	}
}

// stubHostAudio is a minimal modem.VoiceAudio for Start after the route
// is already fresh.
type stubHostAudio struct{ started int32 }

func (s *stubHostAudio) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "stub"}, nil
}
func (s *stubHostAudio) Start(context.Context, modem.CallID) error {
	atomic.StoreInt32(&s.started, 1)
	return nil
}
func (s *stubHostAudio) ReadPCM([]int16) (int, error)  { return 0, nil }
func (s *stubHostAudio) WritePCM([]int16) (int, error)  { return 0, nil }
func (s *stubHostAudio) Stop(context.Context) error     { return nil }
func (s *stubHostAudio) Close() error                   { return nil }

// TestConcurrentPreheatAndStartRotateExactlyOnce exercises the exact
// race flagged in review: PrepareRoute (RING preheat) and Start (fast
// system answer) run concurrently with a stale route. Whichever ordering
// occurs, the route may rotate AT MOST once, never during the marked
// call, and Start must end with its host started against a fresh route.
func TestConcurrentPreheatAndStartRotateExactlyOnce(t *testing.T) {
	for iteration := 0; iteration < 25; iteration++ {
		host := &stubHostAudio{}
		a := &Audio{cfg: Config{Bootstrap: true}, host: host}
		var rotations int32
		release := make(chan struct{})
		a.rotateRoute = func(ctx context.Context) error {
			atomic.AddInt32(&rotations, 1)
			// Hold the flight while the other caller arrives.
			time.Sleep(30 * time.Millisecond)
			select {
			case <-release:
			default:
				close(release)
			}
			a.mu.Lock()
			a.lastRotate = time.Now()
			a.mu.Unlock()
			return nil
		}
		a.mu.Lock()
		a.prepared = true
		a.lastRotate = time.Now().Add(-time.Minute)
		a.mu.Unlock()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = a.PrepareRoute(context.Background())
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(iteration) * time.Millisecond)
			if err := a.Start(context.Background(), "call-1"); err != nil {
				t.Errorf("iteration %d Start: %v", iteration, err)
			}
		}()
		wg.Wait()

		if got := atomic.LoadInt32(&rotations); got > 1 {
			t.Fatalf("iteration %d rotations = %d, want at most 1", iteration, got)
		}
		if atomic.LoadInt32(&host.started) != 1 {
			t.Fatalf("iteration %d host not started after serialized preparation", iteration)
		}
	}
}
