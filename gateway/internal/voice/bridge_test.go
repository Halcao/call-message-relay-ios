package voice

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"callrelay/gateway/internal/modem"
)

type fakeAudio struct {
	mu      sync.Mutex
	started bool
	frames  int
}

func (a *fakeAudio) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "fake", SampleRate: 8000, Channels: 1}, nil
}
func (a *fakeAudio) Start(context.Context, modem.CallID) error {
	a.mu.Lock()
	a.started = true
	a.mu.Unlock()
	return nil
}
func (a *fakeAudio) ReadPCM(pcm []int16) (int, error) {
	for index := range pcm {
		pcm[index] = int16(index * 10)
	}
	a.mu.Lock()
	a.frames++
	count := a.frames
	a.mu.Unlock()
	if count > 1 {
		return len(pcm), context.Canceled
	}
	return len(pcm), nil
}
func (a *fakeAudio) WritePCM(pcm []int16) (int, error) { return len(pcm), nil }
func (a *fakeAudio) Stop(context.Context) error        { return nil }
func (a *fakeAudio) Close() error                      { return nil }

type fakeMedia struct {
	mu     sync.Mutex
	writer func([]byte)
	frames [][]byte
}

func (m *fakeMedia) WritePCMU(frame []byte) error {
	m.mu.Lock()
	m.frames = append(m.frames, frame)
	m.mu.Unlock()
	return nil
}
func (m *fakeMedia) OnPCMUFrame(writer func([]byte)) { m.mu.Lock(); m.writer = writer; m.mu.Unlock() }

// gatedOwnedAudio models a single-owner device whose Start parks on a gate
// (like the seconds-long QDC507 route bootstrap) with owner-conditional
// release, so the Start/Stop startup race is exercisable deterministically.
type gatedOwnedAudio struct {
	mu      sync.Mutex
	owner   modem.CallID
	gate    chan struct{}
	started int
	stopped int
	entered int32
}

func (a *gatedOwnedAudio) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "gated-owned", SampleRate: 8000, Channels: 1}, nil
}
func (a *gatedOwnedAudio) Start(ctx context.Context, callID modem.CallID) error {
	a.mu.Lock()
	gate := a.gate
	a.mu.Unlock()
	if gate != nil {
		atomic.AddInt32(&a.entered, 1)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate:
		}
	}
	a.mu.Lock()
	a.owner = callID
	a.started++
	a.mu.Unlock()
	return nil
}
func (a *gatedOwnedAudio) ReadPCM([]int16) (int, error)    { return 0, context.Canceled }
func (a *gatedOwnedAudio) WritePCM(p []int16) (int, error) { return len(p), nil }
func (a *gatedOwnedAudio) Stop(context.Context) error      { return nil }
func (a *gatedOwnedAudio) Close() error                    { return nil }
func (a *gatedOwnedAudio) CurrentOwner() modem.CallID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.owner
}
func (a *gatedOwnedAudio) StopIfOwner(_ context.Context, callID modem.CallID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.owner != callID || a.owner == "" {
		return false
	}
	a.owner = ""
	a.stopped++
	return true
}

// Regression for the startup race: Stop wins while audio.Start is still
// blocked. The stop token is consumed with no recorded call id, so the
// late-finishing Start must detect the terminal state, release the device it
// just took, and refuse to arm loops — otherwise the bridge keeps running
// with no working stop left (the re-leak called out in review).
func TestBridgeStopDuringDeviceStartLeavesNothingRunning(t *testing.T) {
	audio := &gatedOwnedAudio{gate: make(chan struct{})}
	bridge := NewBridge(audio, &fakeMedia{})

	startErr := make(chan error, 1)
	go func() { startErr <- bridge.Start(context.Background(), "call-a") }()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&audio.entered) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("bridge start never reached the gated device start")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Stop consumes the stop token before the start recorded anything.
	if err := bridge.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(audio.gate)
	if err := <-startErr; err == nil {
		t.Fatal("Start must fail when Stop won the startup race")
	}

	audio.mu.Lock()
	owner, started, stopped := audio.owner, audio.started, audio.stopped
	audio.mu.Unlock()
	if owner != "" || started != 1 || stopped != 1 {
		t.Fatalf("device leaked by the startup race: owner=%q started=%d stopped=%d", owner, started, stopped)
	}
	if err := bridge.EnqueueClientPCMU(make([]byte, FrameSamples)); err == nil {
		t.Fatal("stopped bridge accepted a client frame (loops still live)")
	}
}

func TestTrimClientQueueBoundsLatency(t *testing.T) {
	queue := make([][]int16, 12)
	for i := range queue {
		queue[i] = []int16{int16(i)}
	}
	trimmed, dropped := trimClientQueue(queue, clientQueueTarget)
	if dropped != 12-clientQueueTarget {
		t.Fatalf("dropped %d frames, want %d", dropped, 12-clientQueueTarget)
	}
	if len(trimmed) != clientQueueTarget {
		t.Fatalf("queue length %d, want %d", len(trimmed), clientQueueTarget)
	}
	if trimmed[0][0] != int16(12-clientQueueTarget) {
		t.Fatalf("oldest kept frame = %d, want %d (stale frames dropped)", trimmed[0][0], 12-clientQueueTarget)
	}
	// Below the target nothing may be dropped.
	small := [][]int16{{1}, {2}}
	if out, dropped := trimClientQueue(small, clientQueueTarget); dropped != 0 || len(out) != 2 {
		t.Fatalf("small queue trimmed: dropped=%d len=%d", dropped, len(out))
	}
}

func TestBridgePlaybackTrimsBurstToLatencyTarget(t *testing.T) {
	audio := &fakeAudio{}
	bridge := NewBridge(audio, &fakeMedia{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(ctx, "call-trim"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < clientQueueSize; i++ {
		if err := bridge.EnqueueClientPCMU(make([]byte, FrameSamples)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		bridge.mu.Lock()
		drops := bridge.clientDrops
		depth := len(bridge.clientQueue)
		bridge.mu.Unlock()
		if drops > 0 && depth <= clientQueueTarget {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("playback loop never bounded the burst (drops=%d depth=%d)", drops, depth)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := bridge.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if started := func() bool { bridge.mu.Lock(); defer bridge.mu.Unlock(); return bridge.started }(); started {
		t.Fatal("bridge still started after Stop")
	}
}

func TestBridgeConvertsBothDirections(t *testing.T) {
	audio := &fakeAudio{}
	media := &fakeMedia{}
	bridge := NewBridge(audio, media)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(ctx, "call-1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	media.mu.Lock()
	writer := media.writer
	frames := len(media.frames)
	media.mu.Unlock()
	if writer == nil || frames == 0 {
		t.Fatalf("media writer=%v frames=%d", writer != nil, frames)
	}
	writer(make([]byte, FrameSamples))
	if err := bridge.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// cadenceAudio models a hardware playback ring: it accepts one 160-sample
// frame per playback interval of device time, blocks while the ring is full,
// and can stall for a bounded time (ALSA period wait) to exercise catch-up.
// It also records delivered sequence markers so tests can measure content
// retention (order, integrity), not just write counts.
type cadenceAudio struct {
	mu         sync.Mutex
	interval   time.Duration
	capacity   int
	level      int
	lastDrain  time.Time
	firstWrite time.Time
	lastWrite  time.Time
	maxGap     time.Duration
	writes     int
	sequences  int
	lastSeq    int
	outOfOrder int
	corrupt    int
	stallEvery int
	stallFor   time.Duration
}

func newCadenceAudio(interval time.Duration, capacity int) *cadenceAudio {
	return &cadenceAudio{interval: interval, capacity: capacity, lastDrain: time.Now()}
}

func (a *cadenceAudio) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "cadence", SampleRate: 8000, Channels: 1}, nil
}
func (a *cadenceAudio) Start(context.Context, modem.CallID) error { return nil }
func (a *cadenceAudio) ReadPCM([]int16) (int, error)              { return 0, context.Canceled }
func (a *cadenceAudio) Stop(context.Context) error                { return nil }
func (a *cadenceAudio) Close() error                              { return nil }

func (a *cadenceAudio) WritePCM(pcm []int16) (int, error) {
	a.mu.Lock()
	// Drain one frame per interval of wall-clock time.
	now := time.Now()
	drained := int(now.Sub(a.lastDrain) / a.interval)
	if drained > 0 {
		a.level -= drained
		if a.level < 0 {
			a.level = 0
		}
		a.lastDrain = a.lastDrain.Add(time.Duration(drained) * a.interval)
	}
	// Block until the ring has room for the frame.
	for a.level >= a.capacity {
		wait := a.interval
		a.mu.Unlock()
		time.Sleep(wait)
		a.mu.Lock()
		now = time.Now()
		drained = int(now.Sub(a.lastDrain) / a.interval)
		if drained > 0 {
			a.level -= drained
			if a.level < 0 {
				a.level = 0
			}
			a.lastDrain = a.lastDrain.Add(time.Duration(drained) * a.interval)
		}
	}
	a.level++
	a.writes++
	if sequence := int(pcm[0]); sequence != 0 {
		a.sequences++
		// The synthetic tone stores the sequence in the first two samples;
		// any mutation or reordering is a content-integrity failure.
		if pcm[1] != pcm[0] {
			a.corrupt++
		}
		if sequence <= a.lastSeq {
			a.outOfOrder++
		}
		a.lastSeq = sequence
	}
	if a.firstWrite.IsZero() {
		a.firstWrite = now
	} else if gap := now.Sub(a.lastWrite); gap > a.maxGap {
		a.maxGap = gap
	}
	a.lastWrite = now
	shouldStall := a.stallEvery > 0 && a.writes%a.stallEvery == 0
	stallFor := a.stallFor
	a.mu.Unlock()
	if shouldStall {
		time.Sleep(stallFor)
	}
	return len(pcm), nil
}

func (a *cadenceAudio) stats() (writes, sequences, outOfOrder, corrupt int, elapsed, maxGap time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.firstWrite.IsZero() {
		elapsed = a.lastWrite.Sub(a.firstWrite)
	}
	return a.writes, a.sequences, a.outOfOrder, a.corrupt, elapsed, a.maxGap
}

// oldTickerPlaybackLoop is a faithful copy of the pre-fix pacing (time.Ticker
// at the frame interval, trim-to-target BEFORE popping, blocking write). It
// exists only so the regression test can measure the same device model with
// the old algorithm; production always runs the pacer loop.
func (b *Bridge) oldTickerPlaybackLoop(ctx context.Context, interval time.Duration, callID modem.CallID) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	silence := make([]int16, FrameSamples)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.mu.Lock()
			if !b.started {
				b.mu.Unlock()
				return
			}
			if excess := len(b.clientQueue) - clientQueueTarget; excess > 0 {
				b.clientQueue = b.clientQueue[excess:]
				b.clientDrops += uint64(excess)
			}
			var frame []int16
			if len(b.clientQueue) > 0 {
				frame = b.clientQueue[0]
				b.clientQueue = b.clientQueue[1:]
			}
			b.mu.Unlock()
			if frame == nil {
				frame = silence
			}
			if err := b.writePCM(frame); err != nil {
				b.warnPlayback(callID, err)
			}
		}
	}
}

type cadenceResult struct {
	fed, delivered, queueDrops, maxDepth, outOfOrder, corrupt int
	writes                                                    int
	elapsed, maxGap                                           time.Duration
}

// runCadenceScenario feeds one synthetic sequence frame per playback interval
// (constant real-time mean, scaled time) through the real enqueue path while
// the modeled device performs bounded stalls, then reports content retention
// and drop counts for either the production pacer loop or the old ticker loop.
func runCadenceScenario(t *testing.T, old bool, interval time.Duration, duration time.Duration) cadenceResult {
	t.Helper()
	audio := newCadenceAudio(interval, 8)
	audio.stallEvery = 30
	audio.stallFor = 6 * interval
	bridge := NewBridge(audio, &fakeMedia{})
	ctx, cancel := context.WithCancel(context.Background())

	if old {
		bridge.mu.Lock()
		bridge.started = true
		bridge.callID = "call-old"
		bridge.mu.Unlock()
		go bridge.oldTickerPlaybackLoop(ctx, interval, "call-old")
	} else {
		if err := bridge.Start(ctx, "call-new"); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	deadline := start.Add(duration)
	seq := 0
	maxDepth := 0
	for time.Now().Before(deadline) {
		now := time.Now()
		// Constant real-time mean: one frame per elapsed interval, with
		// deadline catch-up so scheduler overshoot does not trickle.
		want := int(now.Sub(start)/interval) + 1
		for seq < want {
			seq++
			frame := make([]int16, FrameSamples)
			frame[0] = int16(seq)
			frame[1] = int16(seq)
			bridge.enqueueClient(frame)
		}
		bridge.mu.Lock()
		if depth := len(bridge.clientQueue); depth > maxDepth {
			maxDepth = depth
		}
		bridge.mu.Unlock()
		time.Sleep(interval / 4)
	}
	// Give the model the same bounded drain window in both scenarios.
	time.Sleep(10 * interval)
	bridge.mu.Lock()
	queueDrops := int(bridge.clientDrops)
	bridge.mu.Unlock()
	cancel()
	_ = bridge.Stop(context.Background())
	writes, delivered, outOfOrder, corrupt, elapsed, maxGap := audio.stats()
	return cadenceResult{
		fed: seq, delivered: delivered, writes: writes, queueDrops: queueDrops,
		maxDepth: maxDepth, outOfOrder: outOfOrder, corrupt: corrupt,
		elapsed: elapsed, maxGap: maxGap,
	}
}

// TestBridgePlaybackCadenceKeepsConstantFeed is the synthetic uplink
// regression with OLD-vs-NEW evidence: a constant 50 fps (scaled) sequence
// feed crosses a device that blocks for bounded stalls. The old ticker loop
// loses every tick inside a blocking write, so frames pile up in the jitter
// queue and are trimmed; the pacer loop replays the elapsed-slot debt and
// keeps both the cadence and the sequence content. Drop reduction and content
// retention are asserted, not just writes per second.
func TestBridgePlaybackCadenceKeepsConstantFeed(t *testing.T) {
	originalInterval := playbackFrameInterval
	playbackFrameInterval = 2 * time.Millisecond
	defer func() { playbackFrameInterval = originalInterval }()

	const run = 1200 * time.Millisecond
	newStats := runCadenceScenario(t, false, playbackFrameInterval, run)
	oldStats := runCadenceScenario(t, true, playbackFrameInterval, run)
	t.Logf("new: fed=%d delivered=%d writes=%d queueDrops=%d maxDepth=%d outOfOrder=%d corrupt=%d elapsed=%v maxGap=%v",
		newStats.fed, newStats.delivered, newStats.writes, newStats.queueDrops, newStats.maxDepth,
		newStats.outOfOrder, newStats.corrupt, newStats.elapsed, newStats.maxGap)
	t.Logf("old: fed=%d delivered=%d writes=%d queueDrops=%d maxDepth=%d outOfOrder=%d corrupt=%d elapsed=%v maxGap=%v",
		oldStats.fed, oldStats.delivered, oldStats.writes, oldStats.queueDrops, oldStats.maxDepth,
		oldStats.outOfOrder, oldStats.corrupt, oldStats.elapsed, oldStats.maxGap)

	if newStats.fed < 500 {
		t.Fatalf("feed produced only %d frames; scenario did not run", newStats.fed)
	}
	// Content: constant real-time feed must survive with sequence order and
	// tone integrity intact and only bounded trims.
	if newStats.outOfOrder != 0 || newStats.corrupt != 0 {
		t.Fatalf("content integrity lost: outOfOrder=%d corrupt=%d", newStats.outOfOrder, newStats.corrupt)
	}
	if float64(newStats.delivered) < float64(newStats.fed)*0.95 {
		t.Fatalf("new pacing retained only %d/%d frames", newStats.delivered, newStats.fed)
	}
	if newStats.queueDrops > newStats.fed/25 {
		t.Fatalf("new pacing trimmed %d/%d frames (want <= 4%%)", newStats.queueDrops, newStats.fed)
	}
	if newStats.maxDepth > clientQueueTarget+playbackMaxCatchUp {
		t.Fatalf("new queue depth %d exceeded catch-up bound", newStats.maxDepth)
	}
	if newStats.maxGap > audioStallBound(playbackFrameInterval) {
		t.Fatalf("new max write gap %v exceeded bounded stall recovery", newStats.maxGap)
	}
	// Cadence: consumed frames track elapsed device time.
	if newStats.elapsed > 0 {
		expected := float64(newStats.elapsed) / float64(playbackFrameInterval)
		if float64(newStats.writes) < expected*0.85 {
			t.Fatalf("new device consumed %d frames over %v, want >= 85%% of %v",
				newStats.writes, newStats.elapsed, expected)
		}
	}

	// OLD-vs-NEW: the old loop must demonstrably lose frames to the stalls.
	if oldStats.delivered >= newStats.delivered {
		t.Fatalf("old pacing delivered %d frames, new %d: no regression separation",
			oldStats.delivered, newStats.delivered)
	}
	if oldStats.queueDrops <= newStats.queueDrops {
		t.Fatalf("old pacing trimmed %d frames, new %d: drop reduction not demonstrated",
			oldStats.queueDrops, newStats.queueDrops)
	}
	if float64(oldStats.delivered) > float64(newStats.delivered)*0.92 {
		t.Fatalf("old pacing retained %d vs new %d: separation too small to prove the fix",
			oldStats.delivered, newStats.delivered)
	}
}

func audioStallBound(interval time.Duration) time.Duration {
	return 6*interval + 8*interval
}

// blockingAudio parks the first device write so the test can prove the
// enqueue path is not serialized behind a slow ALSA write (the bridge used
// to hold its mutex across WritePCM).
type blockingAudio struct {
	fakeAudio
	entered chan struct{}
	once    sync.Once
}

func (a *blockingAudio) WritePCM(pcm []int16) (int, error) {
	a.once.Do(func() {
		if a.entered != nil {
			a.entered <- struct{}{}
		}
		time.Sleep(250 * time.Millisecond)
	})
	return len(pcm), nil
}

func TestBridgeEnqueueNotBlockedByDeviceWrite(t *testing.T) {
	audio := &blockingAudio{entered: make(chan struct{}, 1)}
	bridge := NewBridge(audio, &fakeMedia{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(ctx, "call-slow-write"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-audio.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("playback loop never entered the device write")
	}
	start := time.Now()
	if err := bridge.EnqueueClientPCMU(make([]byte, FrameSamples)); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("enqueue blocked %v behind a device write", took)
	}
	if err := bridge.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
