package voice

import (
	"testing"
	"time"
)

// TestPlaybackPacerTracksWallClockThroughStalls is the regression guard for
// the choppy-uplink defect: a blocking device write used to swallow every
// time.Ticker tick that elapsed inside the block, so the playback loop could
// not consume frames at real time. The pacer must account for every elapsed
// 20 ms slot (a blocking write is not lost time) while still reporting a
// bounded burst for a single discontinuity.
func TestPlaybackPacerTracksWallClockThroughStalls(t *testing.T) {
	const interval = 20 * time.Millisecond
	start := time.Unix(0, 0)
	pacer := newPlaybackPacer(interval, start, 25)

	now := start
	total := 0
	// 100 iterations normally advance one slot; every 5th write stalls for
	// 7 slots (a 140 ms device block). All time must still be accounted for.
	const iterations = 100
	for index := 0; index < iterations; index++ {
		total += pacer.due(now)
		if index%5 == 4 {
			now = now.Add(7 * interval)
		} else {
			now = now.Add(interval)
		}
	}
	// The schedule may lag the final "now" only by the last produced count.
	last := pacer.due(now)
	total += last
	elapsedSlots := int(now.Sub(start)/interval) + 1
	// Allow a rounding slack of one slot for the exact final boundary.
	if total < elapsedSlots-1 || total > elapsedSlots+1 {
		t.Fatalf("pacer accounted %d slots over %d elapsed, want ~%d", total, elapsedSlots, elapsedSlots)
	}

	// A time.Ticker in the same timeline would deliver one wakeup per
	// iteration (100) plus one after each stall instead of all elapsed slots;
	// assert the pacer is strictly better so this test fails if anyone
	// re-introduces lost-slot pacing.
	oldTickerWrites := iterations
	if total <= oldTickerWrites+10 {
		t.Fatalf("pacer produced %d frames, old ticker would produce %d: no catch-up", total, oldTickerWrites)
	}
}

func TestPlaybackPacerResyncsAfterLongStall(t *testing.T) {
	const interval = 20 * time.Millisecond
	start := time.Unix(0, 0)
	pacer := newPlaybackPacer(interval, start, 25)
	if due := pacer.due(start); due != 1 {
		t.Fatalf("first due = %d, want 1", due)
	}
	// Five seconds of discontinuity must not replay 250 frames of stale
	// audio; it is capped and the schedule resyncs to now.
	huge := start.Add(5 * time.Second)
	if due := pacer.due(huge); due != 25 {
		t.Fatalf("long-stall due = %d, want capped 25", due)
	}
	if due := pacer.due(huge.Add(interval)); due != 1 {
		t.Fatalf("post-resync due = %d, want 1", due)
	}
	if due := pacer.due(huge.Add(interval)); due != 0 {
		t.Fatalf("early due = %d, want 0", due)
	}
}
