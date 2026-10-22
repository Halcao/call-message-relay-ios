package voice

import "time"

// playbackPacer converts wall-clock time into the number of fixed 20 ms
// frames that are due to be written to the cellular playback device.
//
// Why not time.Ticker: Go's ticker drops ticks while the receiver is busy.
// Writing 160 PCM samples into the ALSA/UAC ring can block for a full
// hardware period (observed: 100+ ms with the previous aplay period), so a
// ticker-based loop silently lost the ticks that elapsed during the block:
// it kept consuming fewer frames per second than the network delivered, the
// jitter queue overflowed and was trimmed, and the modem heard choppy,
// time-compressed uplink audio (live evidence 2026-10-05:
// "voice internal client stats ... drops" rising by ~45 frames per 100).
//
// The pacer instead advances a monotonic schedule by exactly one interval
// per frame; a call that arrives after a stall reports the accumulated debt
// and the loop writes the backlog back-to-back, so the long-run cadence
// tracks the wall clock instead of the scheduler. A stall longer than
// maxBurst intervals is treated as a discontinuity (suspend, device reset):
// the schedule resyncs to now and only maxBurst frames are replayed so
// end-to-end latency cannot grow without bound.
type playbackPacer struct {
	interval time.Duration
	next     time.Time
	maxBurst int
}

func newPlaybackPacer(interval time.Duration, now time.Time, maxBurst int) *playbackPacer {
	if interval <= 0 {
		interval = 20 * time.Millisecond
	}
	if maxBurst < 1 {
		maxBurst = 1
	}
	return &playbackPacer{interval: interval, next: now, maxBurst: maxBurst}
}

// due returns how many frames should be written at now and advances the
// schedule. It never returns a negative count and never discards elapsed
// slots unless they exceed maxBurst.
func (p *playbackPacer) due(now time.Time) int {
	if now.Before(p.next) {
		return 0
	}
	count := int(now.Sub(p.next)/p.interval) + 1
	if count > p.maxBurst {
		// Discontinuity: replay a bounded burst and restart the schedule at
		// the next slot so the stale backlog is discarded instead of being
		// played back minutes later.
		p.next = now.Add(p.interval)
		return p.maxBurst
	}
	p.next = p.next.Add(time.Duration(count) * p.interval)
	return count
}

// waitFor returns the duration until the next frame is due, capped at the
// frame interval so a resync after a long stall takes effect promptly.
func (p *playbackPacer) waitFor(now time.Time) time.Duration {
	if !now.Before(p.next) {
		return 0
	}
	wait := p.next.Sub(now)
	if wait > p.interval {
		wait = p.interval
	}
	return wait
}
