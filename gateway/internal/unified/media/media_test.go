package media

import (
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/voice"

	pion "github.com/pion/webrtc/v4"
)

func pcmFrame(value int16) []byte {
	samples := make([]int16, voice.FrameSamples)
	for i := range samples {
		samples[i] = value
	}
	frame, err := voice.EncodePCMU(samples)
	if err != nil {
		panic(err)
	}
	return frame
}

func TestHostJitterConsumesOnceAndUnderflowsToSilence(t *testing.T) {
	jitter := newHostJitter(160)
	jitter.pushPCM(1000, []int16{100})
	jitter.pushPCM(1160, []int16{200})
	if frame := jitter.pop(time.Now()); frame == nil || frame[0] != 100 {
		t.Fatalf("first pop = %v, want 100", frame)
	}
	if frame := jitter.pop(time.Now()); frame == nil || frame[0] != 200 {
		t.Fatalf("second pop = %v, want 200", frame)
	}
	if frame := jitter.pop(time.Now()); frame != nil {
		t.Fatalf("underflow pop = %v, want nil (silence, never a repeated frame)", frame)
	}
	// A duplicate timestamp must not resurrect an already consumed frame.
	jitter.pushPCM(1160, []int16{200})
	if frame := jitter.pop(time.Now()); frame != nil {
		t.Fatalf("replayed duplicate pop = %v, want nil", frame)
	}
}

func TestHostJitterHandlesReorderingAndBoundsCapacity(t *testing.T) {
	jitter := newHostJitter(160)
	jitter.pushPCM(1320, []int16{3})
	jitter.pushPCM(1000, []int16{1})
	jitter.pushPCM(1160, []int16{2})
	want := []int16{1, 2, 3}
	for _, expected := range want {
		frame := jitter.pop(time.Now())
		if frame == nil || frame[0] != expected {
			t.Fatalf("pop = %v, want %d", frame, expected)
		}
	}
	for i := 0; i < hostJitterCapacity+50; i++ {
		jitter.pushPCM(uint32(10_000+160*i), []int16{int16(i)})
	}
	jitter.mu.Lock()
	size := len(jitter.frames)
	jitter.mu.Unlock()
	if size > hostJitterCapacity {
		t.Fatalf("buffer size %d exceeds capacity %d", size, hostJitterCapacity)
	}
}

func TestHostJitterPushTrimsBurstToLatencyTarget(t *testing.T) {
	jitter := newHostJitter(160)
	// Burst larger than the latency bound so the trim path is exercised
	// regardless of the target value.
	const burst = hostJitterTarget + 14
	for i := 0; i < burst; i++ {
		jitter.pushPCM(uint32(1000+160*i), []int16{int16(i)})
	}
	depth, trimmed, _, _ := jitter.stats()
	if depth > hostJitterTarget {
		t.Fatalf("burst left %d frames buffered, want <= %d", depth, hostJitterTarget)
	}
	if trimmed != uint64(burst-hostJitterTarget) {
		t.Fatalf("trimmed %d stale frames, want %d", trimmed, burst-hostJitterTarget)
	}
	// Only the newest target-sized block must survive, in order.
	first := jitter.pop(time.Now())
	if first == nil || first[0] != int16(burst-hostJitterTarget) {
		t.Fatalf("first pop = %v, want %d (oldest stale frame dropped)", first, burst-hostJitterTarget)
	}
	last := first
	for {
		frame := jitter.pop(time.Now())
		if frame == nil {
			break
		}
		last = frame
	}
	if last == nil || last[0] != int16(burst-1) {
		t.Fatalf("last pop = %v, want %d (newest frame preserved)", last, burst-1)
	}
}

// TestHostJitterTrimRebasesPlayCursor is the lockstep regression test: once
// the buffer is primed, an overflowing burst must NOT leave the play cursor
// pointing at a deleted frame — with one push and one pop per tick that
// lockstep makes every pop return nil forever while trims grow ~50/s (the
// observed 2026-10-04 failure: host_trimmed≈250 per 5 s, host_depth pinned
// at the target, uplink silent). After the burst, steady one-frame-per-20ms
// delivery must promptly resume non-nil, in-order playback WITHOUT a manual
// clear, at bounded latency.
func TestHostJitterTrimRebasesPlayCursor(t *testing.T) {
	jitter := newHostJitter(160)
	// Prime with two frames and start playback.
	jitter.pushPCM(0, []int16{0})
	jitter.pushPCM(160, []int16{1})
	if frame := jitter.pop(time.Now()); frame == nil || frame[0] != 0 {
		t.Fatalf("prime pop = %v, want frame 0", frame)
	}
	// Burst far beyond the latency bound: play is at 160; the burst pushes
	// enough frames to trim everything from 160 upward.
	const burst = hostJitterTarget + 20
	for i := 0; i < burst; i++ {
		jitter.pushPCM(uint32(160*2+160*i), []int16{int16(100 + i)})
	}
	depth, trimmed, _, _ := jitter.stats()
	if trimmed == 0 {
		t.Fatal("burst did not trim any frame; test cannot prove the rebase")
	}
	if depth == 0 || depth > hostJitterTarget {
		t.Fatalf("post-burst depth = %d, want 1..%d", depth, hostJitterTarget)
	}
	// The very next pops must be non-nil and play the surviving frames IN
	// ORDER (newest target-sized block), with no silence-walk through the
	// trimmed range.
	first := jitter.pop(time.Now())
	if first == nil {
		t.Fatal("pop after burst trim returned nil; play cursor was not rebased (lockstep bug)")
	}
	if want := int16(100 + burst - depth); first[0] != want {
		t.Fatalf("first post-burst pop = %d, want oldest surviving frame %d", first[0], want)
	}
	for i := 1; i < depth; i++ {
		frame := jitter.pop(time.Now())
		if frame == nil || frame[0] != first[0]+int16(i) {
			t.Fatalf("post-burst pop %d = %v, want %d (in-order, no nils)", i, frame, int(first[0])+i)
		}
	}
	// Steady one-frame-per-tick delivery afterwards: every pop non-nil.
	for i := 0; i < 10; i++ {
		jitter.pushPCM(uint32(160*(2+burst+i)), []int16{int16(200 + i)})
		frame := jitter.pop(time.Now())
		if frame == nil {
			t.Fatalf("steady pop %d returned nil after rebase; lockstep resumed", i)
		}
	}
}

// TestHostJitterBurstWithinTargetIsLossless: a transport that delivers in
// ≤target-sized batches (the FRP/TCP coalescing hypothesis) must play every
// frame once with no trims and no silence when the bound absorbs the batch.
func TestHostJitterBurstWithinTargetIsLossless(t *testing.T) {
	const batch = hostJitterTarget // exactly the bound: worst absorbable burst
	const windows = 8
	jitter := newHostJitter(160)
	var delivered, played, silences uint64
	push := func() {
		ts := uint32(delivered * voice.FrameSamples)
		// Runtime contract (PumpWSHost): a frame behind the play cursor
		// clears the buffer before it enters.
		if jitter.behindPlay(ts) {
			jitter.clear()
		}
		jitter.pushPCM(ts, []int16{int16(delivered % 30000)})
		delivered++
	}
	for b := 0; b < batch; b++ {
		push()
	}
	for w := 0; w < windows; w++ {
		for tick := 0; tick < batch; tick++ {
			if frame := jitter.pop(time.Now()); frame != nil {
				played++
			} else {
				silences++
			}
			if w+1 < windows && tick == batch-1 {
				for f := 0; f < batch; f++ {
					push()
				}
			}
		}
	}
	_, trimmed, fast, _ := jitter.stats()
	if trimmed != 0 {
		t.Fatalf("bound-sized bursts trimmed %d frames, want 0 loss", trimmed)
	}
	if silences != 0 {
		t.Fatalf("bound-sized bursts produced %d silence ticks, want 0", silences)
	}
	if played != delivered {
		t.Fatalf("played %d of %d delivered", played, delivered)
	}
	if fast == 0 {
		t.Fatal("spacing metric recorded no fast arrivals for a batched stream")
	}
}

// TestHostJitterLateRetransmissionIsDeadlineBounded: a NACK retransmission
// whose timestamp already fell behind the play cursor must never reach the
// mixer: the Session.hostPush gate drops it (same rule the WSS socket path
// applies), duplicates are idempotent, and the buffer depth stays bounded —
// stale recovery packets cannot displace fresh audio or grow latency.
func TestHostJitterLateRetransmissionIsDeadlineBounded(t *testing.T) {
	jitter := newHostJitter(160)
	push := func(ts uint32) { jitter.pushPCM(ts, []int16{int16(1000 + ts)}) }
	// Prime with 2 frames and play 4 ticks (cursor advances to 640).
	push(0)
	push(160)
	_ = jitter.pop(time.Now())
	_ = jitter.pop(time.Now())
	push(320)
	_ = jitter.pop(time.Now())
	push(480)
	_ = jitter.pop(time.Now())
	// Genuine quiet: the empty tick is INACTIVE, so the cursor advances
	// (quiet drops history; it does not arm the adaptive rebuffer).
	jitter.mu.Lock()
	jitter.lastPush = time.Now().Add(-time.Second)
	jitter.mu.Unlock()
	if frame := jitter.pop(time.Now()); frame != nil {
		t.Fatalf("expected underflow silence, got a frame")
	}
	// Late retransmission of an ALREADY-PLAYED frame (ts 160 < play 640):
	// Session.hostPush refuses it via this exact predicate (same rule as the
	// WSS socket path), so the frame never enters the buffer.
	if !jitter.behindPlay(160) {
		t.Fatalf("ts 160 should be behind the play cursor")
	}
	depth, trimmed, _, _ := jitter.stats()
	if depth != 0 {
		t.Fatalf("late retransmission disturbed the buffer: depth %d, want 0", depth)
	}
	// A duplicate of a still-buffered frame is idempotent. The cursor now
	// points at 800 (four pops + one underflow advance).
	push(800)
	push(800)
	if frame := jitter.pop(time.Now()); frame == nil || frame[0] != 1800 {
		t.Fatalf("buffered frame lost or altered by duplicates: %v", frame)
	}
	if frame := jitter.pop(time.Now()); frame != nil {
		t.Fatalf("duplicate retransmission played twice")
	}
	// Fill beyond the latency target with a stale frame in the mix: the
	// latency bound holds regardless, and stale frames are never played
	// (pop only reads the cursor position).
	for i := 0; i < hostJitterTarget+4; i++ {
		push(uint32(960 + i*160))
	}
	push(160) // stale
	depth, _, _, _ = jitter.stats()
	if depth > hostJitterTarget {
		t.Fatalf("buffer depth %d exceeded the latency bound %d", depth, hostJitterTarget)
	}
	if !jitter.behindPlay(160) {
		t.Fatalf("stale frame timestamp should remain behind the cursor")
	}
	_ = trimmed
}

// TestHostJitterBurstBeyondTargetSkipsAndResumes: an OVER-bound burst is
// skipped (stale audio dropped, cursor rebased) and playback resumes
// promptly at the newest audio — one bounded gap, never permanent silence.
func TestHostJitterBurstBeyondTargetSkipsAndResumes(t *testing.T) {
	jitter := newHostJitter(160)
	jitter.pushPCM(0, []int16{0})
	jitter.pushPCM(160, []int16{1})
	_ = jitter.pop(time.Now())
	// 3×-bound burst.
	const burst = 3 * hostJitterTarget
	for i := 0; i < burst; i++ {
		jitter.pushPCM(uint32(320+160*i), []int16{int16(i)})
	}
	played := 0
	for i := 0; i < hostJitterTarget; i++ {
		if frame := jitter.pop(time.Now()); frame != nil {
			played++
		}
	}
	if played == 0 {
		t.Fatal("no frame played after over-bound burst; cursor never recovered")
	}
	// Prompt steady resume: after the surviving block is consumed, a steady
	// stream is non-nil on every pop.
	for i := 0; i < 6; i++ {
		jitter.pushPCM(uint32(320+160*(burst+i)), []int16{int16(50 + i)})
		if frame := jitter.pop(time.Now()); frame == nil {
			t.Fatalf("steady pop %d after over-bound burst returned nil", i)
		}
	}
	_, trimmed, _, _ := jitter.stats()
	if trimmed == 0 {
		t.Fatal("over-bound burst trimmed nothing; expected stale frames to drop")
	}
}

// TestSessionMixOnceLiveGateHoldsBufferedAudio: with the live window derived
// from the latency bound, a delivery gap SHORTER than the bound must NOT
// discard buffered host audio (the old fixed 250 ms window discarded a 600 ms
// buffer after 250 ms of inter-batch silence). Exercises the real
// Session.mixOnce path.
// pumpPush mirrors the runtime contract PumpWSHost implements: every frame
// passes DropStaleHostAudio (clear + re-prime when a resumed stream lands
// behind the play cursor) before it enters the jitter buffer.
func pumpPush(session *Session, gen uint64, ts uint32, value int16) {
	session.DropStaleHostAudio(gen, ts)
	session.hostPush(gen, ts, []int16{value})
}

func TestSessionMixOnceLiveGateHoldsBufferedAudio(t *testing.T) {
	session := NewSession(nil, "call:livegate", "call")
	defer session.Close()
	transport := newFakeTransport(8)
	leg := NewLeg("line1:call", transport, nil)
	session.AddLeg(leg)
	session.mu.Lock()
	session.hostPresent = true
	gen := session.generation
	session.mu.Unlock()

	// Primed buffer holding a full bound of audio, then an idle gap shorter
	// than hostLiveWindow: playback must still flow to the leg.
	for i := 0; i < hostJitterTarget; i++ {
		pumpPush(session, gen, uint32(160*i), int16(1000+i))
	}
	session.host.mu.Lock()
	session.host.lastPush = time.Now().Add(-hostJitterTarget * 10 * time.Millisecond) // gap < window
	session.host.mu.Unlock()
	session.mixOnce()
	waitUntil(t, time.Second, func() bool { return transport.sentCount() >= 1 })
	legFrame := transport.sent[len(transport.sent)-1]
	decoded, err := voice.DecodePCMU(legFrame)
	if err != nil || len(decoded) != voice.FrameSamples {
		t.Fatalf("leg frame malformed: %v", err)
	}
	if decoded[0] < 800 || decoded[0] > 1100 {
		t.Fatalf("leg received %d, want host audio ~900+ (buffered audio must not be discarded)", decoded[0])
	}

	// A gap BEYOND the live window must gate all external-leg writes: the
	// next mixOnce treats the host as stalled and sends NOTHING to the leg.
	// The queued leg audio is consumed for the (absent here) client-side
	// downlink only, never routed leg-to-leg or replayed stale.
	before := transport.sentCount()
	session.host.mu.Lock()
	session.host.lastPush = time.Now().Add(-hostLiveWindow - 50*time.Millisecond)
	session.host.mu.Unlock()
	transport.frames <- pcmFrame(777) // leg audio arriving while host is dead
	session.mixOnce()
	time.Sleep(100 * time.Millisecond) // allow a violating send to land
	if transport.sentCount() != before {
		t.Fatalf("mixOnce sent %d frames to the leg while host was beyond the live window",
			transport.sentCount()-before)
	}
}

// TestSessionPumpStallResumeClearsBehindPlay exercises the REAL runtime
// recovery path: PumpWSHost calls DropStaleHostAudio before every hostPush;
// after a send stall lets the play cursor run ahead, the next frame lands
// behind the cursor and the buffer clears and re-primes — no manual clear.
func TestSessionPumpStallResumeClearsBehindPlay(t *testing.T) {
	session := NewSession(nil, "call:pumpstall", "call")
	defer session.Close()
	session.SetCallbacks(func(string, HostLossCause) {}, nil, nil)
	session.mu.Lock()
	session.hostPresent = true
	gen := session.generation
	session.mu.Unlock()

	// Primed, then a stall: pops keep advancing the cursor with no pushes.
	pumpPush(session, gen, 0, 1)
	pumpPush(session, gen, 160, 2)
	if frame := session.host.pop(time.Now()); frame == nil {
		t.Fatal("first pop nil; prime failed")
	}
	// Simulate the stall: no arrivals for longer than the activity window
	// (genuine quiet — NOT an active underrun), so the cursor advances past
	// every buffered frame instead of pausing for the adaptive rebuffer.
	session.host.mu.Lock()
	session.host.lastPush = time.Now().Add(-time.Second)
	session.host.mu.Unlock()
	for i := 0; i < 5; i++ {
		_ = session.host.pop(time.Now()) // nil walks, cursor advances
	}
	// Runtime pre-push check: the resumed stream continues at ts=320, which
	// is now BEHIND the cursor — DropStaleHostAudio must clear.
	pumpPush(session, gen, 320, 30)
	pumpPush(session, gen, 480, 31)
	if frame := session.host.pop(time.Now()); frame == nil || frame[0] != 30 {
		t.Fatalf("post-stall pop = %v, want 30 (cleared + re-primed)", frame)
	}
	if frame := session.host.pop(time.Now()); frame == nil || frame[0] != 31 {
		t.Fatalf("second post-stall pop = %v, want 31", frame)
	}
}

func TestHostJitterPacedStreamIsNotTrimmed(t *testing.T) {
	jitter := newHostJitter(160)
	for i := 0; i < 4; i++ {
		jitter.pushPCM(uint32(1000+160*i), []int16{int16(i)})
	}
	if _, trimmed, _, _ := jitter.stats(); trimmed != 0 {
		t.Fatalf("paced stream trimmed %d frames, want 0", trimmed)
	}
	for i := 0; i < 4; i++ {
		frame := jitter.pop(time.Now())
		if frame == nil || frame[0] != int16(i) {
			t.Fatalf("pop %d = %v, want %d (order preserved, no drop)", i, frame, i)
		}
	}
}

func TestLegPopTrimsStaleAudioToLatencyBound(t *testing.T) {
	transport := newFakeTransport(1)
	leg := NewLeg("line1:call_trim", transport, nil)
	defer leg.Close()

	const burst = 10
	leg.mu.Lock()
	for i := 1; i <= burst; i++ {
		leg.queue = append(leg.queue, []int16{int16(i)})
	}
	leg.mu.Unlock()

	frame := leg.pop(time.Now())
	want := int16(burst - legQueueTargetFrames + 1)
	if frame == nil || frame[0] != want {
		t.Fatalf("first pop = %v, want %d (stale burst trimmed to target)", frame, want)
	}
	depth, dropped := leg.stats()
	if depth != legQueueTargetFrames-1 {
		t.Fatalf("queue depth after pop = %d, want %d", depth, legQueueTargetFrames-1)
	}
	if dropped != uint64(burst-legQueueTargetFrames) {
		t.Fatalf("dropped %d stale frames, want %d", dropped, burst-legQueueTargetFrames)
	}
	// The remaining frames keep playing in order until the queue drains.
	for i := 0; i < legQueueTargetFrames-1; i++ {
		if next := leg.pop(time.Now()); next == nil || next[0] != want+int16(i)+1 {
			t.Fatalf("drain %d = %v, want %d", i, next, want+int16(i)+1)
		}
	}
	if frame := leg.pop(time.Now()); frame != nil {
		t.Fatalf("queue not drained, got %v", frame)
	}
	_, droppedAfter := leg.stats()
	if droppedAfter != uint64(burst-legQueueTargetFrames) {
		t.Fatalf("drain double-counted drops: %d", droppedAfter)
	}
}

type fakeTransport struct {
	frames chan []byte
	mu     sync.Mutex
	sent   [][]byte
	closed bool
}

func newFakeTransport(buffer int) *fakeTransport {
	return &fakeTransport{frames: make(chan []byte, buffer)}
}

func (f *fakeTransport) Frames() <-chan []byte { return f.frames }

func (f *fakeTransport) SendPCMU(frame []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, append([]byte(nil), frame...))
	return nil
}

func (f *fakeTransport) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeTransport) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.frames)
	}
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}

func TestLegHoldAndUnholdDropBufferedAudio(t *testing.T) {
	transport := newFakeTransport(16)
	leg := NewLeg("line1:call_1", transport, nil)
	defer leg.Close()

	transport.frames <- pcmFrame(1000)
	transport.frames <- pcmFrame(1000)
	waitUntil(t, time.Second, func() bool {
		leg.mu.Lock()
		defer leg.mu.Unlock()
		return len(leg.queue) == 2
	})
	leg.SetHeld(true)
	leg.mu.Lock()
	heldQueue := len(leg.queue)
	leg.mu.Unlock()
	if heldQueue != 0 {
		t.Fatalf("held leg retained %d buffered frames", heldQueue)
	}
	if err := leg.Send(pcmFrame(1000)); err != nil {
		t.Fatalf("held send: %v", err)
	}
	if transport.sentCount() != 0 {
		t.Fatalf("held leg sent audio to the line")
	}
	leg.SetHeld(false)
	leg.mu.Lock()
	unheldQueue := len(leg.queue)
	leg.mu.Unlock()
	if unheldQueue != 0 {
		t.Fatalf("unhold leaked %d stale frames", unheldQueue)
	}

	transport.frames <- pcmFrame(500)
	waitUntil(t, time.Second, func() bool {
		leg.mu.Lock()
		defer leg.mu.Unlock()
		return len(leg.queue) == 1
	})
	if leg.pop(time.Now()) == nil {
		t.Fatal("fresh audio missing after unhold")
	}
}

func TestPlanMixGatesAllExternalOutputWhenHostNotLive(t *testing.T) {
	frames := map[string][]int16{
		"line1:call": {1000, 1000},
		"line2:call": {2000, 2000},
	}
	// Host genuinely absent: no client output, no external writes.
	client, sends := planMix(false, false, []int16{3000, 3000}, frames, []string{"line1:call", "line2:call"}, map[string]bool{})
	if client != nil {
		t.Fatalf("client output while host absent = %v, want nil", client)
	}
	if len(sends) != 0 {
		t.Fatalf("external-to-external audio leaked while host absent: %v", sends)
	}

	// Host present but uplink stalled: fresh leg audio still reaches the
	// client, and NO external leg receives anything.
	client, sends = planMix(true, false, []int16{3000, 3000}, frames, []string{"line1:call", "line2:call"}, map[string]bool{})
	if client == nil {
		t.Fatal("client output nil while host present and uplink stalled")
	}
	// line1 1000*0.9=900 + line2 2000*0.9=1800 = 2700.
	if client[0] < 2600 || client[0] > 2800 {
		t.Fatalf("stalled-host client mix = %d, want ~2700", client[0])
	}
	if len(sends) != 0 {
		t.Fatalf("external-to-external audio leaked while host uplink stalled: %v", sends)
	}

	// Defensive invariant: presence is the stronger gate, so an inconsistent
	// (hostPresent=false, hostLive=true) call still yields no output at all.
	client, sends = planMix(false, true, []int16{3000, 3000}, frames, []string{"line1:call", "line2:call"}, map[string]bool{})
	if client != nil {
		t.Fatalf("client output with host absent and hostLive true = %v, want nil", client)
	}
	if len(sends) != 0 {
		t.Fatalf("external output with host absent and hostLive true: %v", sends)
	}
}

func TestPlanMixExcludesHeldLegAndSelf(t *testing.T) {
	frames := map[string][]int16{
		"line1:call": {1000, 1000},
		"line2:call": {2000, 2000},
	}
	_, sends := planMix(true, true, []int16{500, 500}, frames, []string{"line1:call", "line2:call"}, map[string]bool{"line2:call": true})
	if _, ok := sends["line2:call"]; ok {
		t.Fatal("held leg received output")
	}
	line1 := sends["line1:call"]
	if line1 == nil {
		t.Fatal("active leg missing output")
	}
	// line1 must hear host (500*0.9=450) plus line2 (2000*0.9=1800) = 2250,
	// never its own 1000.
	if line1[0] < 2200 || line1[0] > 2300 {
		t.Fatalf("line1 mix-minus = %d, want ~2250", line1[0])
	}
}

func TestMixerDeterministicAndScaledNotClipped(t *testing.T) {
	frames := map[string][]int16{
		"a": {30000, -30000},
		"b": {30000, -30000},
		"c": {30000, -30000},
	}
	first := mixClientOutput(frames)
	for i := 0; i < 20; i++ {
		next := mixClientOutput(frames)
		for index := range first {
			if first[index] != next[index] {
				t.Fatalf("mixer output changed between runs at %d: %d vs %d", index, first[index], next[index])
			}
		}
	}
	for _, sample := range first {
		if sample > 32767 || sample < -32768 {
			t.Fatalf("mixer clipped out of range: %d", sample)
		}
	}
	// Three 0.9 sources sum to 81000; the limiter must scale down and keep
	// the sign/relative shape instead of clipping at 32767.
	if first[0] != 32767 {
		t.Fatalf("expected full-scale after scaling, got %d", first[0])
	}
	if first[1] != -32767 {
		t.Fatalf("expected negative full-scale after scaling, got %d", first[1])
	}
}

func TestLimitFramePreservesSmallSignals(t *testing.T) {
	values := []float64{100, -200, 0}
	out := limitFrame(values)
	want := []int16{100, -200, 0}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("limitFrame[%d] = %d, want %d", i, out[i], want[i])
		}
	}
}

func TestStalePeerGenerationCallbacksAreIgnored(t *testing.T) {
	session := NewSession(nil, "call:1", "call")
	defer session.Close()
	var lost int
	session.SetCallbacks(func(string, HostLossCause) { lost++ }, nil, nil)
	// Identity fencing (build 50): any non-nil pointer stands in for a peer
	// connection; only the pc installed in the PRIMARY slot may drive the
	// host lifecycle.
	currentPC := &pion.PeerConnection{}
	otherPC := &pion.PeerConnection{}
	session.mu.Lock()
	session.pc = currentPC
	session.hostPresent = true
	session.mu.Unlock()

	session.applyHostState(otherPC, pion.PeerConnectionStateFailed)
	if !session.HostConnected() {
		t.Fatal("foreign/standby pc cleared current host state")
	}
	if lost != 0 {
		t.Fatalf("foreign/standby pc fired host-lost callback %d times", lost)
	}

	session.applyHostState(currentPC, pion.PeerConnectionStateFailed)
	if session.HostConnected() {
		t.Fatal("primary pc state did not clear host state")
	}
	if lost != 1 {
		t.Fatalf("host-lost callback count = %d, want 1", lost)
	}

	// hostPush from an old generation must not fill the jitter buffer.
	session.hostPush(6, 1000, []int16{1})
	session.mu.Lock()
	size := len(session.host.frames)
	session.mu.Unlock()
	if size != 0 {
		t.Fatalf("stale hostPush wrote %d frames", size)
	}
	// Drop the stand-in pc: it is a bare identity token, not a real peer,
	// and must not be Closed by session.Close().
	session.mu.Lock()
	session.pc = nil
	session.mu.Unlock()
}

func TestHostDisconnectDrainsLegBuffers(t *testing.T) {
	session := NewSession(nil, "conf:1", "conference")
	defer session.Close()
	transport := newFakeTransport(8)
	leg := NewLeg("line1:call", transport, nil)
	session.AddLeg(leg)

	transport.frames <- pcmFrame(900)
	waitUntil(t, time.Second, func() bool {
		leg.mu.Lock()
		defer leg.mu.Unlock()
		return len(leg.queue) == 1
	})
	// Identity fencing: only the pc installed in the PRIMARY slot drives the
	// host lifecycle; install a stand-in and fail it.
	pc := &pion.PeerConnection{}
	session.mu.Lock()
	session.pc = pc
	session.hostPresent = true
	session.mu.Unlock()
	session.applyHostState(pc, pion.PeerConnectionStateFailed)
	// (the stand-in is cleared below before Close, same as the test above)
	leg.mu.Lock()
	remaining := len(leg.queue)
	leg.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("disconnect left %d buffered leg frames", remaining)
	}
	// Drop the stand-in pc (identity token only, not a real peer).
	session.mu.Lock()
	session.pc = nil
	session.mu.Unlock()
}

func TestMixOnceSendsNothingToLegsWhileHostAbsent(t *testing.T) {
	session := NewSession(nil, "call:2", "call")
	defer session.Close()
	transport := newFakeTransport(8)
	leg := NewLeg("line1:call", transport, nil)
	session.AddLeg(leg)
	transport.frames <- pcmFrame(1000)
	waitUntil(t, time.Second, func() bool {
		leg.mu.Lock()
		defer leg.mu.Unlock()
		return len(leg.queue) == 1
	})
	session.mixOnce()
	if transport.sentCount() != 0 {
		t.Fatalf("leg received %d frames while host absent", transport.sentCount())
	}
	leg.mu.Lock()
	remaining := len(leg.queue)
	leg.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("leg queue not drained while host absent: %d", remaining)
	}
}

func TestMixOnceDeliversHostToLegWhenLive(t *testing.T) {
	session := NewSession(nil, "call:3", "call")
	defer session.Close()
	transport := newFakeTransport(8)
	leg := NewLeg("line1:call", transport, nil)
	session.AddLeg(leg)

	session.mu.Lock()
	session.hostPresent = true
	session.mu.Unlock()
	session.host.pushPCM(1000, []int16{1000})
	session.host.pushPCM(1160, []int16{1000})

	session.mixOnce()
	waitUntil(t, time.Second, func() bool { return transport.sentCount() == 1 })
	frame := transport.sent[0]
	decoded, err := voice.DecodePCMU(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded[0] < 800 || decoded[0] > 1000 {
		t.Fatalf("leg received %d, want host audio ~900", decoded[0])
	}
}

var _ = fmt.Sprintf

// simulateLegConsumption replays an exact arrival schedule (batch index ->
// frame count, delivered every 128 ms like an arecord period) against the
// REAL leg queue + pop/trim logic, with the daemon mixer consuming one frame
// per 20 ms tick. Deterministic: pure event ordering, no wall clock.
func simulateLegConsumption(batches []int, periodMs, tickMs int) (played, silence int, dropped uint64) {
	leg := &Leg{}
	type event struct {
		at    int
		batch int // -1 = mixer tick
	}
	events := []event{}
	for i := range batches {
		events = append(events, event{at: i * periodMs, batch: i})
	}
	totalMs := len(batches) * periodMs
	for t := 0; t < totalMs; t += tickMs {
		events = append(events, event{at: t, batch: -1})
	}
	sort.Slice(events, func(a, b int) bool {
		if events[a].at != events[b].at {
			return events[a].at < events[b].at
		}
		return events[a].batch >= 0 // arrivals before the tick at the same instant
	})
	seq := 0
	for _, ev := range events {
		if ev.batch >= 0 {
			// Same append path as Leg.pump (including the hard cap).
			leg.mu.Lock()
			for i := 0; i < batches[ev.batch]; i++ {
				if len(leg.queue) >= maxQueuedFrames {
					copy(leg.queue, leg.queue[1:])
					leg.queue = leg.queue[:len(leg.queue)-1]
				}
				leg.queue = append(leg.queue, []int16{int16(seq)})
				seq++
			}
			leg.mu.Unlock()
			continue
		}
		if frame := leg.pop(time.Now()); frame == nil {
			silence++
		} else {
			played++
		}
	}
	_, dropped = leg.stats()
	return played, silence, dropped
}

func TestLegQueue_BurstVsPacedArrival(t *testing.T) {
	// Old behaviour (measured on a production ARM gateway with the Baiwang pipe): arecord
	// --period-size=1024 delivers alternating 7/6 frames per 128 ms period.
	var burst []int
	for cycle := 0; cycle < 12; cycle++ {
		if cycle%2 == 0 {
			burst = append(burst, 7)
		} else {
			burst = append(burst, 6)
		}
	}
	burstPlayed, burstSilence, burstDropped := simulateLegConsumption(burst, 128, 20)

	arrived := 0
	for _, b := range burst {
		arrived += b
	}
	if burstDropped == 0 {
		t.Fatal("burst pattern dropped zero frames: model no longer exercises the trim path")
	}
	if burstSilence == 0 {
		t.Fatal("burst pattern starved zero ticks: expected dry gaps between 128 ms periods")
	}
	if burstPlayed+int(burstDropped) != arrived {
		t.Fatalf("frames not accounted: played=%d dropped=%d arrived=%d", burstPlayed, burstDropped, arrived)
	}
	// Live evidence (build 14): steady leg_trimmed ~20/s and constant
	// underflow gaps; the model must reproduce meaningful loss without being
	// worse than reality.
	if lost := burstDropped + uint64(burstSilence); lost < uint64(arrived)/4 {
		t.Fatalf("burst pattern lost only %d of %d frames; live evidence showed ~30-45%% loss", lost, arrived)
	}

	// New behaviour (measured: median interarrival 20.0 ms): arecord
	// --period-size=160 delivers exactly one frame per 20 ms period.
	paced := make([]int, 96)
	for i := range paced {
		paced[i] = 1
	}
	pacedPlayed, pacedSilence, pacedDropped := simulateLegConsumption(paced, 20, 20)
	if pacedDropped != 0 || pacedSilence != 0 {
		t.Fatalf("paced pattern dropped=%d silence=%d, want 0/0 (capture period now matches the frame)", pacedDropped, pacedSilence)
	}
	if pacedPlayed != 96 {
		t.Fatalf("paced pattern played %d, want 96", pacedPlayed)
	}

	if burstPlayed >= pacedPlayed {
		t.Fatalf("burst played %d >= paced %d: model is not a fair comparison", burstPlayed, pacedPlayed)
	}
	t.Logf("burst(128ms period): played=%d silenceTicks=%d trimmed=%d of %d arrived; paced(20ms period): played=%d silence=0 trimmed=0",
		burstPlayed, burstSilence, burstDropped, arrived, pacedPlayed)
}
