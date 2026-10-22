package qdc507

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"callrelay/gateway/internal/modem"
)

func TestSelectTransportIDPinsTheExactUSBPath(t *testing.T) {
	devices := "List of devices attached\n(serialless)\tdevice usb:2-1 transport_id:2\n(other)\tdevice usb:3-2 transport_id:3\n"
	if got, err := SelectTransportID(devices, "2-1"); err != nil || got != "2" {
		t.Fatalf("got transport=%q err=%v", got, err)
	}
	if _, err := SelectTransportID(devices, "1-1"); err == nil {
		t.Fatal("unrelated USB path was accepted")
	}
	if _, err := SelectTransportID(devices+"duplicate\tdevice usb:2-1 transport_id:4\n", "2-1"); err == nil {
		t.Fatal("ambiguous USB path was accepted")
	}
}

// ---------- single-owner handover invariants ----------

type fakeRunner struct{}

func (fakeRunner) Run(context.Context, ...string) (string, error) { return "", nil }

// handoverHost models the host UAC pair with gate-able Start/Stop so tests can
// park a device transition mid-flight and interleave a competing call exactly.
// Start sets running=true, Stop sets running=false: a "new session killed by a
// stale stop" is observable as running==false at the end.
type handoverHost struct {
	mu           sync.Mutex
	running      bool
	started      int
	stopped      int
	startGate    chan struct{}
	stopGate     chan struct{}
	startEntered int32
	stopEntered  int32
}

func (h *handoverHost) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "handover", SampleRate: 8000, Channels: 1}, nil
}

func (h *handoverHost) Start(ctx context.Context, _ modem.CallID) error {
	h.mu.Lock()
	gate := h.startGate
	h.mu.Unlock()
	if gate != nil {
		atomic.AddInt32(&h.startEntered, 1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
		}
	}
	h.mu.Lock()
	h.running = true
	h.started++
	h.mu.Unlock()
	return nil
}

func (h *handoverHost) Stop(ctx context.Context) error {
	h.mu.Lock()
	gate := h.stopGate
	h.mu.Unlock()
	if gate != nil {
		atomic.AddInt32(&h.stopEntered, 1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
		}
	}
	h.mu.Lock()
	h.running = false
	h.stopped++
	h.mu.Unlock()
	return nil
}

func (h *handoverHost) ReadPCM([]int16) (int, error)    { return 0, context.Canceled }
func (h *handoverHost) WritePCM(p []int16) (int, error) { return len(p), nil }
func (h *handoverHost) Close() error                    { return nil }

// failStartHost fails Start while fail is set, then behaves normally.
type failStartHost struct {
	mu      sync.Mutex
	fail    bool
	started int
}

func (h *failStartHost) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "fail-start", SampleRate: 8000, Channels: 1}, nil
}
func (h *failStartHost) Start(context.Context, modem.CallID) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail {
		return context.DeadlineExceeded
	}
	h.started++
	return nil
}
func (h *failStartHost) ReadPCM([]int16) (int, error)    { return 0, context.Canceled }
func (h *failStartHost) WritePCM(p []int16) (int, error) { return len(p), nil }
func (h *failStartHost) Stop(context.Context) error      { return nil }
func (h *failStartHost) Close() error                    { return nil }

func newPreparedTestAudio(t *testing.T, host modem.VoiceAudio) *Audio {
	t.Helper()
	audio, err := OpenWithRunner(host, Config{TTY: "/dev/ttyUSB0", RuntimeDir: "/tmp/qdc-handover", Bootstrap: true}, fakeRunner{})
	if err != nil {
		t.Fatal(err)
	}
	// ensureRuntime short-circuits on a recently-rotated prepared route, so
	// these tests exercise the owner/host-transition logic only.
	audio.mu.Lock()
	audio.prepared = true
	audio.lastRotate = time.Now()
	audio.mu.Unlock()
	return audio
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for: " + message)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Deterministic version of the detached-stop race: StopIfOwner clears the
// owner and parks in host.Stop while a new call claims the device. The stale
// stop must never leave the new session dead: the host transition is
// serialized (hostMu) and the new host.Start runs strictly after the old
// host.Stop, so `running` must be true at the end.
func TestStopIfOwnerNeverKillsNewerSessionDuringHandover(t *testing.T) {
	host := &handoverHost{}
	audio := newPreparedTestAudio(t, host)

	if err := audio.Start(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.stopGate = make(chan struct{})
	host.startGate = make(chan struct{})
	host.mu.Unlock()

	stopResult := make(chan bool, 1)
	go func() { stopResult <- audio.StopIfOwner(context.Background(), "old") }()
	waitFor(t, func() bool { return atomic.LoadInt32(&host.stopEntered) == 1 }, "stale stop parked in host.Stop")

	// The new call claims the device while the stale stop is parked.
	startErr := make(chan error, 1)
	go func() { startErr <- audio.Start(context.Background(), "new") }()
	waitFor(t, func() bool { return audio.CurrentOwner() == "new" }, "new call claimed the device")

	// Release the transitions: the new host.Start is queued behind the
	// serialized host stop, so it always runs last and revives the device.
	// The settle delay makes the ordering explicit for unpatched backends
	// (their detached host.Stop would deterministically land after the new
	// host.Start and kill it); the patched backend is order-independent.
	close(host.startGate)
	time.Sleep(50 * time.Millisecond)
	close(host.stopGate)
	<-stopResult
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}

	host.mu.Lock()
	running, started, stopped := host.running, host.started, host.stopped
	host.mu.Unlock()
	if !running || started != 2 || stopped != 1 {
		t.Fatalf("new session killed by stale stop: running=%v started=%d stopped=%d", running, started, stopped)
	}
	if owner := audio.CurrentOwner(); owner != "new" {
		t.Fatalf("owner = %q, want new", owner)
	}
}

// Same invariant for the unconditional Stop used by full teardown paths.
func TestStopNeverKillsNewerSessionDuringHandover(t *testing.T) {
	host := &handoverHost{}
	audio := newPreparedTestAudio(t, host)

	if err := audio.Start(context.Background(), "old"); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.stopGate = make(chan struct{})
	host.startGate = make(chan struct{})
	host.mu.Unlock()

	stopResult := make(chan error, 1)
	go func() { stopResult <- audio.Stop(context.Background()) }()
	waitFor(t, func() bool { return atomic.LoadInt32(&host.stopEntered) == 1 }, "stop parked in host.Stop")

	startErr := make(chan error, 1)
	go func() { startErr <- audio.Start(context.Background(), "new") }()
	waitFor(t, func() bool { return audio.CurrentOwner() == "new" }, "new call claimed the device")

	// Release the transitions: the new host.Start is queued behind the
	// serialized host stop, so it always runs last and revives the device.
	// The settle delay makes the ordering explicit for unpatched backends
	// (their detached host.Stop would deterministically land after the new
	// host.Start and kill it); the patched backend is order-independent.
	close(host.startGate)
	time.Sleep(50 * time.Millisecond)
	close(host.stopGate)
	if err := <-stopResult; err != nil {
		t.Fatal(err)
	}
	if err := <-startErr; err != nil {
		t.Fatal(err)
	}

	host.mu.Lock()
	running, started, stopped := host.running, host.started, host.stopped
	host.mu.Unlock()
	if !running || started != 2 || stopped != 1 {
		t.Fatalf("new session killed by stop: running=%v started=%d stopped=%d", running, started, stopped)
	}
	if owner := audio.CurrentOwner(); owner != "new" {
		t.Fatalf("owner = %q, want new", owner)
	}
}

// A failed host.Start must not leave a phantom owner behind: the next call
// would otherwise be refused as "audio is already active" forever.
func TestFailedStartLeavesNoPhantomOwner(t *testing.T) {
	host := &failStartHost{fail: true}
	audio := newPreparedTestAudio(t, host)

	if err := audio.Start(context.Background(), "a"); err == nil {
		t.Fatal("expected the failing start to surface its error")
	}
	if owner := audio.CurrentOwner(); owner != "" {
		t.Fatalf("phantom owner after failed start: %q", owner)
	}

	host.mu.Lock()
	host.fail = false
	host.mu.Unlock()
	if err := audio.Start(context.Background(), "b"); err != nil {
		t.Fatalf("next call refused after a failed start: %v", err)
	}
	if owner := audio.CurrentOwner(); owner != "b" {
		t.Fatalf("owner = %q, want b", owner)
	}
}

// TestHostPairStopInvalidatesRouteFreshness pins the sequential-call repair:
// after the host UAC pair has run and stopped, the next call's preparation
// must rotate the module voice route instead of trusting the 30 s freshness
// window. The QDC507 capture stream dies once the host pair has touched the
// gadget, so a quick second call that reused the fresh route read silence
// (build-33 call-2 field evidence class). Both Stop and StopIfOwner route
// through the same invalidation; this test covers Stop and the shared helper
// covers the StopIfOwner path.
func TestHostPairStopInvalidatesRouteFreshness(t *testing.T) {
	host := &stubHostAudio{}
	audio := &Audio{cfg: Config{Bootstrap: true}, host: host}
	var rotations int32
	audio.rotateRoute = func(context.Context) error {
		atomic.AddInt32(&rotations, 1)
		audio.mu.Lock()
		audio.prepared = true
		audio.lastRotate = time.Now()
		audio.mu.Unlock()
		return nil
	}
	audio.mu.Lock()
	audio.prepared = true
	audio.lastRotate = time.Now()
	audio.mu.Unlock()

	if err := audio.Start(context.Background(), "call-1"); err != nil {
		t.Fatalf("start call-1: %v", err)
	}
	if err := audio.Stop(context.Background()); err != nil {
		t.Fatalf("stop call-1: %v", err)
	}
	if err := audio.PrepareRoute(context.Background()); err != nil {
		t.Fatalf("prepare call-2: %v", err)
	}
	if got := atomic.LoadInt32(&rotations); got != 1 {
		t.Fatalf("rotations after host-pair stop = %d, want exactly 1", got)
	}
}

// TestFreshRouteStaysTrustedWithinOneCallPreparation pins the other side of
// the policy: a route rotated for the pre-dial preparation must NOT rotate
// again when the same call's Start runs moments later.
func TestFreshRouteStaysTrustedWithinOneCallPreparation(t *testing.T) {
	host := &stubHostAudio{}
	audio := &Audio{cfg: Config{Bootstrap: true}, host: host}
	var rotations int32
	audio.rotateRoute = func(context.Context) error {
		atomic.AddInt32(&rotations, 1)
		audio.mu.Lock()
		audio.prepared = true
		audio.lastRotate = time.Now()
		audio.mu.Unlock()
		return nil
	}
	audio.mu.Lock()
	audio.prepared = true
	audio.lastRotate = time.Now().Add(-time.Minute)
	audio.mu.Unlock()

	if err := audio.PrepareRoute(context.Background()); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := audio.Start(context.Background(), "call-1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := atomic.LoadInt32(&rotations); got != 1 {
		t.Fatalf("rotations = %d, want exactly 1 for one call", got)
	}
}
