package qdc507

import (
	"context"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/modem"
)

// guardedHost records whether its Start was ever invoked and blocks it
// behind a channel.
type guardedHost struct {
	started chan struct{}
}

func (h *guardedHost) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "guarded"}, nil
}
func (h *guardedHost) Start(context.Context, modem.CallID) error {
	close(h.started)
	return nil
}
func (h *guardedHost) ReadPCM([]int16) (int, error) { return 0, nil }
func (h *guardedHost) WritePCM([]int16) (int, error) { return 0, nil }
func (h *guardedHost) Stop(context.Context) error    { return nil }
func (h *guardedHost) Close() error                  { return nil }

// TestStopDuringPreparationDoesNotStartHost exercises the owner-generation
// fence: Stop runs while Start is blocked inside route preparation; the
// pair must never start for an ended call.
func TestStopDuringPreparationDoesNotStartHost(t *testing.T) {
	host := &guardedHost{started: make(chan struct{})}
	a := &Audio{cfg: Config{Bootstrap: true}, host: host}
	rotationEntered := make(chan struct{})
	releaseRotation := make(chan struct{})
	var once sync.Once
	a.rotateRoute = func(context.Context) error {
		once.Do(func() { close(rotationEntered) })
		<-releaseRotation
		a.mu.Lock()
		a.lastRotate = time.Now()
		a.mu.Unlock()
		return nil
	}
	a.mu.Lock()
	a.prepared = true
	a.lastRotate = time.Now().Add(-time.Minute)
	a.mu.Unlock()

	result := make(chan error, 1)
	go func() {
		result <- a.Start(context.Background(), "call-stop-test")
	}()
	<-rotationEntered
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop during preparation: %v", err)
	}
	close(releaseRotation)

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Start returned nil after Stop during preparation; want error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start never returned after Stop during preparation")
	}
	select {
	case <-host.started:
		t.Fatal("host pair started for an ended call")
	default:
	}
	// Owner left cleared.
	if got := a.CurrentOwner(); got != "" {
		t.Fatalf("owner = %q, want empty", got)
	}
}
