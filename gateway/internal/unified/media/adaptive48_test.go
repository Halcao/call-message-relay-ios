package media

import (
	"math"
	"testing"
	"time"

	"callrelay/gateway/internal/opus"
)

// encodedTone returns one reference-libopus 20 ms frame of a 440 Hz tone.
func encodedTone(t *testing.T, enc *opus.Encoder) []byte {
	t.Helper()
	tone := make([]int16, opus.FrameSamples)
	for i := range tone {
		tone[i] = int16(12000 * math.Sin(2*math.Pi*440*float64(i)/opus.SampleRate))
	}
	payload, err := enc.Encode(tone)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return payload
}

// TestAdaptiveHostStartsAt40msAndStaysForBursts: a 4-6 frame coalesced batch
// is retained (hard trim at hostJitterTarget), i.e. the adaptive relaxation
// must never chop good batch speech.
func TestAdaptiveHostStartsAt40msAndStaysForBursts(t *testing.T) {
	j := newHostJitter(160)
	if j.level != hostJitterStartLevel {
		t.Fatalf("start level %d, want %d", j.level, hostJitterStartLevel)
	}
	// Coalesced 6-frame batch (<10 ms apart, as a TCP/WSS refill delivers).
	for i := 0; i < hostJitterTarget; i++ {
		j.pushPCM(uint32(i*160), []int16{int16(i + 1)})
	}
	if depth := j.depth(); depth != hostJitterTarget {
		t.Fatalf("batch depth %d, want %d (bursts must be retained)", depth, hostJitterTarget)
	}
}

// TestAdaptiveHostRelaxesTo20msAfterCleanLivePlayback: 500 delivered frames
// with no underrun lower the level to the 20 ms floor.
func TestAdaptiveHostRelaxesTo20msAfterCleanPlayback(t *testing.T) {
	j := newHostJitter(160)
	j.pushPCM(0, []int16{1})
	j.pushPCM(160, []int16{2})
	if f := j.pop(time.Now()); f == nil {
		t.Fatal("prime failed")
	}
	ts := uint32(160)
	for n := 0; n < hostJitterStableFrames+5; n++ {
		ts += 160
		j.pushPCM(ts, []int16{int16(n + 3)})
		if f := j.pop(time.Now()); f == nil {
			t.Fatalf("frame %d underflowed", n)
		}
	}
	if j.level != hostJitterMinLevel {
		t.Fatalf("level after clean playback %d, want %d", j.level, hostJitterMinLevel)
	}
}

// TestAdaptiveHostUnderrunGrowsOncePerEpisodeAndKeepsStaleRejection: a
// sustained starvation with recent arrivals raises the level immediately once,
// resets the clean-live count, and behindPlay keeps rejecting old timestamps
// even while waiting to re-prime.
func TestAdaptiveHostUnderrunGrowsOncePerEpisodeAndKeepsStaleRejection(t *testing.T) {
	j := newHostJitter(160)
	j.pushPCM(0, []int16{1})
	j.pushPCM(160, []int16{2})
	j.pop(time.Now()) // frame 0, started
	j.pop(time.Now()) // frame 160, cursor at 320
	// Arrivals are "recent" for every empty tick.
	for n := 0; n < 10; n++ {
		j.mu.Lock()
		j.lastPush = time.Now()
		j.mu.Unlock()
		if f := j.pop(time.Now()); f != nil {
			t.Fatalf("empty tick %d produced a frame", n)
		}
	}
	if j.level != hostJitterStartLevel+1 {
		t.Fatalf("level after underrun %d, want %d", j.level, hostJitterStartLevel+1)
	}
	if j.underruns != 1 {
		t.Fatalf("underruns %d, want exactly 1 (once per episode)", j.underruns)
	}
	// Cursor-established rejection survives the post-underrun unprimed state.
	if !j.behindPlay(0) {
		t.Fatal("already-played timestamp must stay behind the cursor while unprimed")
	}
	// Transport replacement / send-stall resync clears the buffer (timeline
	// reset), which closes the starvation episode; the stream then re-primes.
	j.clear()
	j.pushPCM(0, []int16{9})
	j.pushPCM(160, []int16{10})
	if f := j.pop(time.Now()); f == nil {
		t.Fatal("re-primed stream did not play")
	}
	if f := j.pop(time.Now()); f == nil || f[0] != 10 {
		t.Fatalf("second re-primed frame = %v", f)
	}
	j.mu.Lock()
	j.lastPush = time.Now()
	j.mu.Unlock()
	j.pop(time.Now())
	if j.underruns != 2 {
		t.Fatalf("underruns after a new episode %d, want 2", j.underruns)
	}
}

// TestAdaptiveHostIsolatedLossDoesNotRelaxStability: intermittent empty ticks
// reset the clean-live count, so the level cannot relax even though 500
// frames are spread across them.
func TestAdaptiveHostIsolatedLossDoesNotRelaxStability(t *testing.T) {
	j := newHostJitter(160)
	j.pushPCM(0, []int16{1})
	j.pushPCM(160, []int16{2})
	j.pop(time.Now())
	ts := uint32(160)
	for n := 0; n < hostJitterStableFrames+10; n++ {
		ts += 160
		j.pushPCM(ts, []int16{3})
		j.pop(time.Now())
		if n%7 == 0 { // intermittent active loss
			j.mu.Lock()
			j.lastPush = time.Now()
			j.mu.Unlock()
			j.pop(time.Now())
		}
	}
	if j.level <= hostJitterMinLevel {
		t.Fatalf("level relaxed to %d despite intermittent losses", j.level)
	}
}

// TestAdaptiveLegStableRelaxAndBatchProtection exercises the modem leg:
// clean playback lowers the trim level, a fast UAC burst is protected by
// burstCredit while slow drift is trimmed.
//
// Modem-level timing note (build-49 review): the leg relaxes ONE level per
// legQueueStableFrames (500) consecutive delivered frames, and the 80 -> 40
// path is TWO levels (4 -> 3 -> 2), i.e. ~1000 delivered frames ~= 20 s of
// clean playback — the approved plan documents this as "stable acceptable".
func TestAdaptiveLegStableRelaxAndBatchProtection(t *testing.T) {
	tr := &fakeTransport{frames: make(chan []byte, 64)}
	leg := NewLeg("c1", tr, nil)
	defer leg.Close()
	// Drive 500 clean paced pops (queue kept filled by direct append in the
	// pump-equivalent; here enqueue manually with ~20 ms spacing).
	now := time.Now()
	leg.mu.Lock()
	leg.lastArrival = now
	leg.mu.Unlock()
	// Two relaxation steps (80->60->40 ms), each needs 500 clean frames.
	for n := 0; n < 2*legQueueStableFrames+5; n++ {
		leg.mu.Lock()
		leg.lastArrival = time.Now().Add(-20 * time.Millisecond)
		leg.queue = append(leg.queue, []int16{int16(n)})
		leg.mu.Unlock()
		if f := leg.pop(time.Now()); f == nil {
			t.Fatalf("frame %d nil", n)
		}
	}
	if lvl, _ := leg.adaptiveStats(); lvl != legQueueMinLevel {
		t.Fatalf("leg level %d, want %d", lvl, legQueueMinLevel)
	}
	// Coalesced 6-frame burst: pump enqueues fast; all six are fresh and
	// must survive even though the relaxed level is 2.
	leg.mu.Lock()
	for i := 0; i < 6; i++ {
		n := time.Now()
		fast := !leg.lastArrival.IsZero() && n.Sub(leg.lastArrival) < legSteadyGap
		leg.queue = append(leg.queue, []int16{int16(100 + i)})
		if fast {
			if leg.burstCredit == 0 {
				leg.burstCredit = 2
			} else {
				leg.burstCredit++
			}
		}
		leg.lastArrival = n
	}
	leg.mu.Unlock()
	played := 0
	for i := 0; i < 6; i++ {
		if leg.pop(time.Now()) != nil {
			played++
		}
	}
	if played != 6 {
		t.Fatalf("coalesced burst played %d/6 (must retain fresh batch)", played)
	}
}

// TestSessionDenoiseRawBypassWhenDisabled confirms the never-block contract
// in the real mixer path and isolation across sessions.
func TestSessionDenoiseRawBypassWhenDisabled(t *testing.T) {
	s := NewSession(nil, "call:dn", "call")
	defer s.Close()
	// Default: no denoise set => raw identity (no PP allocated).
	frame := make([]int16, 160)
	for i := range frame {
		frame[i] = int16(i)
	}
	if out := s.processUplink(frame); &out[0] != &frame[0] {
		t.Fatal("uplink must pass the raw slice when disabled")
	}
	up, down, upNs, downNs := s.DenoiseStats()
	if up || down || upNs != 0 || downNs != 0 {
		t.Fatalf("default denoise stats unexpected: %v %v %d %d", up, down, upNs, downNs)
	}
}

// setLastPush ages/preserves the inter-arrival gap the next push() observes,
// deterministically simulating paced (>=10 ms apart) or coalesced (<10 ms)
// arrivals without sleeping. (The buffer's play cursor and timestamps stay
// contiguous; only the ARRIVAL SPACING is controlled.)
func setLastPush(j *hostJitter, recent bool) {
	j.mu.Lock()
	if recent {
		j.lastPush = time.Now()
	} else {
		j.lastPush = time.Now().Add(-20 * time.Millisecond)
	}
	j.mu.Unlock()
}

// pacedWarm drives clean 1-frame/tick playback with contiguous timestamps.
func pacedWarm(j *hostJitter, ts *uint32, ticks int, now *time.Time) {
	for n := 0; n < ticks; n++ {
		setLastPush(j, false)
		*ts += 160
		j.pushPCM(*ts, []int16{3})
		*now = now.Add(20 * time.Millisecond)
		j.pop(*now)
	}
}

// TestAdaptiveHostGrownLevelAbsorbsJitter (build-49 review): cause an
// underrun (level grows 40 -> 60 ms), then resume PLAIN 50 fps arrivals with
// no manual prefill: the bounded adaptive rebuffer must automatically refill
// to the level (real playable headroom) within level ticks, absorb a 60 ms
// stall the floor buffer could not, and then run a mild-jitter schedule
// (one covered 1-tick arrival gap + coalesced catch-up every 5th tick,
// average exactly 50 fps) with zero further starvation episodes. Per-frame
// ID accounting proves every pushed frame is delivered exactly once in order.
func TestAdaptiveHostGrownLevelAbsorbsJitter(t *testing.T) {
	// Contrast: the floor-level (20 ms) buffer starves on the same stall.
	j1 := newHostJitter(160)
	j1.pushPCM(0, []int16{1})
	j1.pushPCM(160, []int16{2})
	j1.pop(time.Now())
	ts1 := uint32(160)
	now := time.Now()
	pacedWarm(j1, &ts1, hostJitterStableFrames+5, &now)
	if j1.level != hostJitterMinLevel {
		t.Fatalf("setup level %d, want floor %d", j1.level, hostJitterMinLevel)
	}
	if d := j1.depth(); d != hostJitterMinLevel {
		t.Fatalf("setup depth %d, want %d", d, hostJitterMinLevel)
	}
	stallStarved := 0
	for k := 0; k < 3; k++ {
		now = now.Add(20 * time.Millisecond)
		if j1.pop(now) == nil {
			stallStarved++
		}
		setLastPush(j1, true)
	}
	if stallStarved == 0 {
		t.Fatal("floor buffer did not starve on a 60 ms stall (contrast broken)")
	}

	// Under test: one starvation episode, then PLAIN 50 fps resume.
	j := newHostJitter(160)
	j.pushPCM(0, []int16{1})
	j.pushPCM(160, []int16{2})
	j.pop(time.Now())
	j.pop(time.Now())
	setLastPush(j, true)
	if j.pop(time.Now()) != nil {
		t.Fatal("setup underrun did not fire")
	}
	if j.level != hostJitterStartLevel+1 || j.underruns != 1 {
		t.Fatalf("growth setup level=%d underruns=%d", j.level, j.underruns)
	}
	// Automatic bounded rebuffer: resume PLAIN 50 fps arrivals with NO manual
	// prefill. Consumption stays paused while the queue accumulates; after
	// `level` paced arrivals the headroom equals the level by construction.
	ts := uint32(160)
	var want, got []int16
	for n := 0; n < j.level; n++ {
		setLastPush(j, false)
		ts += 160
		j.pushPCM(ts, []int16{int16(10 + n)})
		want = append(want, int16(10+n))
		now = now.Add(20 * time.Millisecond)
		setLastPush(j, true)
	}
	if d := j.depth(); d != j.level {
		t.Fatalf("automatic rebuffer depth %d, want %d (headroom not created)", d, j.level)
	}
	// The headroom plays oldest-first (resume restarts at the oldest frame).
	now = now.Add(20 * time.Millisecond)
	if f := j.pop(now); f == nil || f[0] != 10 {
		t.Fatalf("resume pop = %v, want id 10", f)
	}
	got = append(got, 10)
	setLastPush(j, true)
	// Two paced arrivals restore the steady level-depth before the jitter.
	for n := 0; n < j.level-1; n++ {
		setLastPush(j, false)
		ts += 160
		j.pushPCM(ts, []int16{int16(20 + n)})
		want = append(want, int16(20+n))
		now = now.Add(20 * time.Millisecond)
		f := j.pop(now)
		if f == nil {
			t.Fatal("paced arrival starved right after the rebuffer")
		}
		got = append(got, f[0])
		setLastPush(j, true)
	}
	if d := j.depth(); d != j.level-1 {
		t.Fatalf("steady depth %d, want %d", d, j.level-1)
	}
	// Mild jitter: 25 cycles of [paced, paced, paced, GAP, coalesced pair]
	// = 5 arrivals per 5 ticks (exactly 50 fps) with a covered gap.
	next := 0
	for cycle := 0; cycle < 25; cycle++ {
		for tick := 0; tick < 5; tick++ {
			now = now.Add(20 * time.Millisecond)
			switch tick {
			case 0, 1, 2:
				setLastPush(j, false)
				next++
				ts += 160
				j.pushPCM(ts, []int16{int16(next)})
				want = append(want, int16(next))
			case 3:
				// Arrival gap: the headroom must cover this pop.
			case 4:
				setLastPush(j, true)
				for k := 0; k < 2; k++ {
					next++
					ts += 160
					j.pushPCM(ts, []int16{int16(next)})
					want = append(want, int16(next))
				}
			}
			if f := j.pop(now); f != nil {
				got = append(got, f[0])
			}
			setLastPush(j, true) // stay "active" across the gap
		}
	}
	if j.underruns != 1 {
		t.Fatalf("underruns grew to %d under stall+jitter (repeated gaps)", j.underruns)
	}
	// Conservation: delivered + still-queued == pushed (no drops yet).
	if len(got)+j.depth() != len(want) {
		t.Fatalf("delivered %d + queued %d != pushed %d (frames lost)", len(got), j.depth(), len(want))
	}

	// Drain the tail (inactive arrivals so the drain itself cannot starve):
	// every pushed frame must eventually deliver exactly once, in order.
	setLastPush(j, false)
	for {
		now = now.Add(20 * time.Millisecond)
		f := j.pop(now)
		if f == nil {
			break
		}
		got = append(got, f[0])
	}
	if len(got) != len(want) {
		t.Fatalf("after drain delivered %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivery order broken at %d: got %d want %d", i, got[i], want[i])
		}
	}
	_, drainedTrims, _, _ := j.stats()
	if drainedTrims != 0 {
		t.Fatalf("drain trimmed %d frames", drainedTrims)
	}
}

// TestAdaptiveHostConcealedFramesDoNotRelaxStability (build-49 review): with
// an Opus session, sustained single-frame losses concealed by PLC/FEC must
// NOT count as clean received stability: every concealed tick opens a new
// starvation episode (real losses), so the level must never relax, and after
// growth the buffer headroom is created by the rebuffer — while genuine
// buffered playback still relaxes normally (other tests).
func TestAdaptiveHostConcealedFramesDoNotRelaxStability(t *testing.T) {
	dec, err := opus.NewDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer dec.Close()
	enc, err := opus.NewEncoder()
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	defer enc.Close()
	j := newHostJitter(960) // Opus clock tick
	j.setDecoder(dec)
	// Exercise from the start level (a relaxed floor would hide growth).
	j.pushEncoded(0, encodedTone(t, enc))
	j.pushEncoded(960, encodedTone(t, enc))
	if j.pop(time.Now()) == nil {
		t.Fatal("prime failed")
	}
	// Drain the priming pair so the buffer enters each cycle empty: push
	// one, pop the real frame, then pop the concealed loss.
	if j.pop(time.Now()) == nil {
		t.Fatal("prime drain failed")
	}
	// Sustained 50% loss: one arrival per cycle, two pops — the second pop
	// hits a missing timestamp and must be concealed. Concealment keeps
	// playback gap-free, so by design it is NOT an audible underrun and does
	// NOT grow the level; the build-49 requirement is that it also must NOT
	// count as clean received stability (which would relax the level and
	// drain the buffer while loss continues).
	now := time.Now()
	for n := 0; n < 40; n++ {
		setLastPush(j, true)
		ts := uint32(1920 * (n + 1))
		j.pushEncoded(ts, encodedTone(t, enc))
		now = now.Add(20 * time.Millisecond)
		if out := j.pop(now); out == nil {
			t.Fatalf("cycle %d: real frame missing", n)
		}
		setLastPush(j, true)
		now = now.Add(20 * time.Millisecond)
		concealed := j.pop(now) // the loss: PLC/FEC must conceal it
		if concealed == nil {
			t.Fatalf("cycle %d: concealment missing", n)
		}
		// Exactly the real frames may count: 2 priming pops + (n+1) cycle
		// pops A. A concealed frame must add NOTHING.
		if want := uint64(n + 3); j.liveFrames != want {
			t.Fatalf("cycle %d: liveFrames %d, want %d (concealed frame counted as clean stability)", n, j.liveFrames, want)
		}
	}
	// 40 cycles x 2 pops = 80 delivered frames; only the real ones may count
	// toward stability, and 40 real frames is far below the 500-frame
	// threshold — but the assertion that matters: the level must not have
	// RELAXED (concealed ticks must not accrue), and no starvation episode
	// was manufactured by concealment.
	if j.level > hostJitterStartLevel {
		t.Fatalf("concealed losses grew the level to %d (they must not)", j.level)
	}
	if want := uint64(42); j.liveFrames != want {
		t.Fatalf("stability count %d under 50%% loss, want %d (concealment masked holes)", j.liveFrames, want)
	}
	if j.underruns != 0 {
		t.Fatalf("concealed losses manufactured %d starvation episodes", j.underruns)
	}
}

// TestAdaptiveHostSteadyDecreaseReducesQueuedLatency (build-49 review): with
// paced arrivals (one frame per tick, the steady operating point), relaxing
// 40 -> 20 ms halves the measured queued depth while every pushed frame is
// delivered exactly once and nothing is cropped. Coalesced speech protection
// at the relaxed level is proven by the immediate retention of a full 4-frame
// batch (fast arrivals are never drift-trimmed).
func TestAdaptiveHostSteadyDecreaseReducesQueuedLatency(t *testing.T) {
	measure := func(relaxToFloor bool) (meanDepth, delivered, pushed, endDepth int, trims uint64) {
		j := newHostJitter(160)
		j.pushPCM(0, []int16{1})
		j.pushPCM(160, []int16{2})
		j.pop(time.Now())
		pushed = 2    // two setup frames
		delivered = 1 // the setup pop delivered one of them
		ts := uint32(160)
		now := time.Now()
		warm := 2
		if relaxToFloor {
			warm = hostJitterStableFrames + 5
		}
		pacedWarm(j, &ts, warm, &now)
		wantLevel := hostJitterStartLevel
		if relaxToFloor {
			wantLevel = hostJitterMinLevel
		}
		if j.level != wantLevel {
			t.Fatalf("setup level %d, want %d", j.level, wantLevel)
		}
		// Prime the buffer to exactly the level so the steady mean is the
		// level itself (queued latency = level * 20 ms by construction).
		for d := j.depth(); d < j.level; d++ {
			setLastPush(j, false)
			ts += 160
			j.pushPCM(ts, []int16{3})
			pushed++
		}
		_, trimmedBefore, _, _ := j.stats()
		depthSum := 0
		for n := 0; n < 300; n++ {
			setLastPush(j, false)
			ts += 160
			j.pushPCM(ts, []int16{int16(n)})
			pushed++
			now = now.Add(20 * time.Millisecond)
			if j.pop(now) != nil {
				delivered++
			}
			depthSum += j.depth()
		}
		_, trimmedAfter, _, _ := j.stats()
		return depthSum / 300, delivered, pushed, j.depth(), trimmedAfter - trimmedBefore
	}
	depth40, del40, push40, end40, trims40 := measure(false)
	depth20, del20, push20, end20, trims20 := measure(true)
	if depth20 >= depth40 {
		t.Fatalf("relaxation did not reduce queued latency: level2 mean %d, level1 mean %d", depth40, depth20)
	}
	if del40+end40 != push40 || del20+end20 != push20 {
		t.Fatalf("frames lost: level2 delivered %d + queued %d != pushed %d; level1 %d + %d != %d",
			del40, end40, push40, del20, end20, push20)
	}
	if trims40 != 0 || trims20 != 0 {
		t.Fatalf("paced steady-state arrivals were cropped: trims %d/%d", trims40, trims20)
	}

	// Coalesced speech at the RELAXED level: a 4-frame batch (<10 ms apart)
	// is retained in full at the moment of arrival (burst protection).
	j := newHostJitter(160)
	j.pushPCM(0, []int16{1})
	j.pushPCM(160, []int16{2})
	j.pop(time.Now())
	ts := uint32(160)
	now := time.Now()
	pacedWarm(j, &ts, hostJitterStableFrames+5, &now)
	if j.level != hostJitterMinLevel {
		t.Fatalf("setup level %d, want floor", j.level)
	}
	_, beforeTrims, _, _ := j.stats()
	beforeDepth := j.depth()
	for k := 0; k < 4; k++ {
		setLastPush(j, true)
		ts += 160
		j.pushPCM(ts, []int16{int16(7 + k)})
	}
	// Every batch frame is present (queued depth grew by exactly the batch;
	// fast arrivals are never drift-trimmed at the relaxed level).
	if d := j.depth(); d != beforeDepth+4 {
		t.Fatalf("coalesced batch depth %d, want %d (batch cropped at relaxed level)", d, beforeDepth+4)
	}
	_, afterTrims, _, _ := j.stats()
	if afterTrims != beforeTrims {
		t.Fatalf("coalesced batch was trimmed at the relaxed level: %d -> %d", beforeTrims, afterTrims)
	}
}
