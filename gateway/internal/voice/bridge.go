package voice

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"callrelay/gateway/internal/modem"
)

type PCMUTransport interface {
	WritePCMU([]byte) error
	OnPCMUFrame(func([]byte))
}

// Bridge is the only place where the modem's signed PCM representation is
// converted to the V1 WebRTC PCMU wire format. It enforces 20 ms / 160-sample
// frames in both directions and keeps media work off HTTP handlers.
type Bridge struct {
	audio             modem.VoiceAudio
	media             PCMUTransport
	cellularFrame     func([]int16)
	clientFrame       func([]int16)
	stop              sync.Once
	started           bool
	stopped           bool
	callID            modem.CallID
	clientFrameSeen   bool
	cellularFrameSeen bool
	cellularFrames    uint64
	clientFrames      uint64
	cellularPeakMax   int
	clientPeakMax     int
	cellularNonZero   uint64
	clientNonZero     uint64
	clientQueue       [][]int16
	clientDrops       uint64
	cellularSum       uint64
	clientSum         uint64
	cellularWindowAt  time.Time
	clientWindowAt    time.Time
	// lastPlaybackWarn rate-limits the per-tick playback failure log: one
	// line per 2 s plus the suppressed count, instead of hundreds of
	// identical broken-pipe warnings that hide the real failure window.
	lastPlaybackWarn time.Time
	playbackSuppressed int
	mu                sync.Mutex
}

func NewBridge(audio modem.VoiceAudio, media PCMUTransport) *Bridge {
	return &Bridge{audio: audio, media: media}
}

func (b *Bridge) SetFrameHooks(cellularFrame, clientFrame func([]int16)) {
	b.mu.Lock()
	b.cellularFrame, b.clientFrame = cellularFrame, clientFrame
	b.mu.Unlock()
}

func (b *Bridge) Start(ctx context.Context, callID modem.CallID) error {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return fmt.Errorf("voice bridge already stopped")
	}
	b.mu.Unlock()
	if b.audio == nil || b.media == nil {
		return fmt.Errorf("voice audio and WebRTC media are required")
	}
	if err := b.audio.Start(ctx, callID); err != nil {
		return err
	}
	b.mu.Lock()
	if b.stopped {
		// Stop ran while the device was starting: it consumed the stop token
		// before this bridge recorded its call id, so it could not release
		// the device. Release what we just took (owner-conditional on
		// single-owner backends) and stay stopped — otherwise the loops
		// below would start with no working stop left (re-leak).
		b.mu.Unlock()
		if owned, ok := b.audio.(modem.OwnedVoiceAudio); ok {
			_ = owned.StopIfOwner(ctx, callID)
		} else {
			_ = b.audio.Stop(ctx)
		}
		return fmt.Errorf("voice bridge already stopped")
	}
	b.started = true
	b.callID = callID
	b.clientFrameSeen = false
	b.cellularFrameSeen = false
	b.clientQueue = nil
	// Anchor the stats windows so the first log reports a real elapsed time
	// instead of a zero-time subtraction.
	b.clientWindowAt = time.Now()
	b.cellularWindowAt = time.Now()
	b.mu.Unlock()
	b.media.OnPCMUFrame(func(frame []byte) {
		if len(frame) != FrameSamples {
			slog.Warn("voice client frame dropped", "call_id", callID, "samples", len(frame))
			return
		}
		pcm, err := DecodePCMU(frame)
		if err != nil {
			slog.Warn("voice client frame decode failed", "call_id", callID, "error", err)
			return
		}
		b.mu.Lock()
		if !b.started {
			b.mu.Unlock()
			return
		}
		if !b.clientFrameSeen {
			b.clientFrameSeen = true
			slog.Info("voice client audio frame received", "call_id", callID, "pcm_peak", pcmPeak(pcm))
		}
		cpeak := pcmPeak(pcm)
		b.clientFrames++
		if cpeak > b.clientPeakMax { b.clientPeakMax = cpeak }
		if cpeak > 0 { b.clientNonZero++ }
		b.clientSum += uint64(cpeak)
		if b.clientFrames%50 == 0 {
			now := time.Now()
			elapsed := now.Sub(b.clientWindowAt)
			b.clientWindowAt = now
			slog.Info("voice client stats", "call_id", callID, "frames", b.clientFrames, "peak_max", b.clientPeakMax, "nonzero", b.clientNonZero, "mean", b.clientSum/uint64(b.clientFrames), "win_ms", elapsed.Milliseconds(), "drops", b.clientDrops)
			b.clientPeakMax = 0
		}
		clientHook := b.clientFrame
		b.mu.Unlock()
		if clientHook != nil {
			clientHook(pcm)
		}
		// Jitter buffer (2026-09-06): enqueue the decoded frame; a fixed
		// 20ms ticker drains it into aplay so playback never stalls.
		// Without this, iPhone RTP bursts/stalls propagate through the
		// UAC ADAPTIVE playback clock and choke the ASYNC capture side
		// (observed: 50-frame windows taking 3-7s, then burst refills).
		b.enqueueClient(pcm)
	})
	go b.readLoop(ctx, callID)
	go b.clientPlaybackLoop(ctx, callID)
	return nil
}

// clientQueueSize and clientQueueTarget bound the jitter queue. Capacity is
// deliberate: keep at least 3 frames so a 60ms network stall never starves
// aplay, and drop the oldest frame when full so end-to-end latency cannot
// grow unbounded.
const (
	clientQueueSize   = 50 // hard memory bound: 1s @ 20ms
	clientQueueTarget = 5  // latency bound: ~100ms of jitter absorption
)

// playbackFrameInterval is the uplink frame cadence. It is a variable only so
// tests can compress time deterministically; production always runs 20 ms.
var playbackFrameInterval = 20 * time.Millisecond

// playbackMaxCatchUp bounds how many frames one pacer step may replay after a
// device/scheduler stall. 25 frames = 500 ms: large enough to cover the
// longest observed ALSA/UAC write stall (128 ms with the old 1024-sample
// playback period) plus scheduler jitter, yet a hard cap on how much audio
// can be written ahead of real time at once. The queue is trimmed to
// clientQueueTarget AFTER each pop, so a catch-up cannot leave more than the
// target queued; only the device ring may briefly hold up to this bound.
const playbackMaxCatchUp = 25

// EnqueueClientPCMU accepts one 160-byte PCMU frame from an internal media
// peer (the unified service owns client-facing WebRTC) and pushes it into
// the same jitter queue used by the public WebRTC path. The single ALSA/UAC
// pair therefore stays owned by this process.
func (b *Bridge) EnqueueClientPCMU(frame []byte) error {
	if len(frame) != FrameSamples {
		return fmt.Errorf("expected %d-byte PCMU frame, got %d", FrameSamples, len(frame))
	}
	pcm, err := DecodePCMU(frame)
	if err != nil {
		return err
	}
	b.mu.Lock()
	if !b.started {
		b.mu.Unlock()
		return fmt.Errorf("voice bridge is not started")
	}
	if !b.clientFrameSeen {
		b.clientFrameSeen = true
		slog.Info("voice internal client audio frame received", "call_id", b.callID, "pcm_peak", pcmPeak(pcm))
	}
	cpeak := pcmPeak(pcm)
	b.clientFrames++
	if cpeak > b.clientPeakMax {
		b.clientPeakMax = cpeak
	}
	if cpeak > 0 {
		b.clientNonZero++
	}
	b.clientSum += uint64(cpeak)
	if b.clientFrames%50 == 0 {
		now := time.Now()
		b.clientWindowAt = now
		slog.Info("voice internal client stats", "call_id", b.callID, "frames", b.clientFrames, "peak_max", b.clientPeakMax, "nonzero", b.clientNonZero, "mean", b.clientSum/uint64(b.clientFrames), "drops", b.clientDrops)
		b.clientPeakMax = 0
	}
	b.mu.Unlock()
	b.enqueueClient(pcm)
	return nil
}

// trimClientQueue drops the oldest frames when the queue exceeds the
// latency target and reports how many were dropped. It is pure so the
// bounded-latency policy is directly testable.
func trimClientQueue(queue [][]int16, target int) ([][]int16, int) {
	excess := len(queue) - target
	if excess <= 0 {
		return queue, 0
	}
	return queue[excess:], excess
}

func (b *Bridge) enqueueClient(pcm []int16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.clientQueue) >= clientQueueSize {
		// drop oldest to bound latency
		copy(b.clientQueue, b.clientQueue[1:])
		b.clientQueue = b.clientQueue[:len(b.clientQueue)-1]
	}
	frame := make([]int16, len(pcm))
	copy(frame, pcm)
	b.clientQueue = append(b.clientQueue, frame)
}

// clientPlaybackLoop drains the jitter queue at a fixed 20ms cadence,
// writing silence when the queue is empty (keeps UAC playback RUNNING
// and the full-duplex clock moving — no XRUN, no capture stall).
//
// The loop is driven by playbackPacer rather than time.Ticker: a blocking
// device write makes every ticker tick inside the block disappear, which
// slows consumption below real time and forced the queue to be trimmed into
// choppy audio. The pacer reports the elapsed-slot debt instead, so writes
// catch up exactly like a hardware playback clock.
func (b *Bridge) clientPlaybackLoop(ctx context.Context, callID modem.CallID) {
	pacer := newPlaybackPacer(playbackFrameInterval, time.Now(), playbackMaxCatchUp)
	silence := make([]int16, FrameSamples)
	for {
		now := time.Now()
		due := pacer.due(now)
		if due == 0 {
			wait := pacer.waitFor(now)
			if wait <= 0 {
				continue
			}
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		for index := 0; index < due; index++ {
			frame, ok := b.nextPlaybackFrame()
			if !ok {
				return
			}
			if frame == nil {
				frame = silence
			}
			if err := b.writePCM(frame); err != nil {
				b.warnPlayback(callID, err)
			}
		}
	}
}

// nextPlaybackFrame pops the oldest queued frame and then trims whatever is
// left to the latency target. Popping BEFORE trimming is deliberate: a
// device stall (a 1024-sample ALSA period could block a write for ~128 ms)
// leaves the pacer owing several frames, and those frames are real-time
// audio that arrived while the device was busy. Trimming first would discard
// exactly the legitimate frames the catch-up burst is supposed to play;
// trimming only the remainder keeps the latency bound without losing the
// catch-up. ok=false means the bridge stopped and the playback loop must
// exit.
func (b *Bridge) nextPlaybackFrame() (frame []int16, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.started {
		return nil, false
	}
	if len(b.clientQueue) > 0 {
		frame = b.clientQueue[0]
		b.clientQueue = b.clientQueue[1:]
	}
	if excess := len(b.clientQueue) - clientQueueTarget; excess > 0 {
		// Drop the oldest leftovers so end-to-end latency stays bounded.
		b.clientQueue = b.clientQueue[excess:]
		b.clientDrops += uint64(excess)
	}
	return frame, true
}

// playbackWarnInterval bounds how often one failing playback tick logs.
const playbackWarnInterval = 2 * time.Second

// warnPlayback logs a modem playback failure at most once per
// playbackWarnInterval; failures inside the window are counted and the
// next line reports how many were suppressed. The error already carries
// the subprocess exit/stderr cause from ALSAAudio.WritePCM.
func (b *Bridge) warnPlayback(callID modem.CallID, err error) {
	b.mu.Lock()
	now := time.Now()
	due := now.Sub(b.lastPlaybackWarn) >= playbackWarnInterval
	suppressed := 0
	if due {
		b.lastPlaybackWarn = now
		suppressed = b.playbackSuppressed
		b.playbackSuppressed = 0
	} else {
		b.playbackSuppressed++
	}
	b.mu.Unlock()
	if !due {
		return
	}
	args := []any{"call_id", callID, "error", err}
	if suppressed > 0 {
		args = append(args, "suppressed", suppressed)
	}
	slog.Warn("voice modem playback failed", args...)
}

func (b *Bridge) readLoop(ctx context.Context, callID modem.CallID) {
	pcm := make([]int16, FrameSamples)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		count, err := b.audio.ReadPCM(pcm)
		if err != nil {
			if b.isStarted() {
				slog.Warn("voice modem capture failed", "call_id", callID, "error", err)
			}
			return
		}
		if !b.isStarted() {
			return
		}
		if count != FrameSamples {
			continue
		}
		b.mu.Lock()
		cellularHook := b.cellularFrame
		b.mu.Unlock()
		if cellularHook != nil {
			cellularHook(pcm)
		}
		b.mu.Lock()
		if !b.cellularFrameSeen {
			b.cellularFrameSeen = true
			slog.Info("voice cellular audio frame received", "call_id", callID, "pcm_peak", pcmPeak(pcm))
		}
		peak := pcmPeak(pcm)
		b.cellularFrames++
		if peak > b.cellularPeakMax { b.cellularPeakMax = peak }
		if peak > 0 { b.cellularNonZero++ }
		b.cellularSum += uint64(peak)
		if b.cellularFrames%50 == 0 {
			now := time.Now()
			elapsed := now.Sub(b.cellularWindowAt)
			b.cellularWindowAt = now
			slog.Info("voice cellular stats", "call_id", callID, "frames", b.cellularFrames, "peak_max", b.cellularPeakMax, "nonzero", b.cellularNonZero, "mean", b.cellularSum/uint64(b.cellularFrames), "win_ms", elapsed.Milliseconds())
			b.cellularPeakMax = 0
		}
		b.mu.Unlock()
		frame, err := EncodePCMU(pcm)
		if err != nil {
			slog.Warn("voice cellular frame encode failed", "call_id", callID, "error", err)
			return
		}
		if err := b.media.WritePCMU(frame); err != nil {
			if b.isStarted() {
				slog.Warn("voice client playback failed", "call_id", callID, "error", err)
			}
			return
		}
	}
}

func (b *Bridge) isStarted() bool {
	b.mu.Lock()
	started := b.started
	b.mu.Unlock()
	return started
}

func (b *Bridge) writePCM(pcm []int16) error {
	// Never hold b.mu across the device write: the ALSA/UAC write can block
	// for a full hardware period, and holding the bridge lock would stall the
	// RTP/media enqueue path behind it (additional avoidable drops). The
	// started flag is re-checked after the write so a Stop that raced the
	// write does not surface a misleading playback failure during teardown.
	b.mu.Lock()
	started := b.started
	b.mu.Unlock()
	if !started {
		return nil
	}
	_, err := b.audio.WritePCM(pcm)
	if err != nil {
		b.mu.Lock()
		stillStarted := b.started
		b.mu.Unlock()
		if !stillStarted {
			return nil
		}
	}
	return err
}

func pcmPeak(samples []int16) int {
	peak := 0
	for _, sample := range samples {
		value := int(sample)
		if value < 0 {
			value = -value
		}
		if value > peak {
			peak = value
		}
	}
	return peak
}

func (b *Bridge) Stop(ctx context.Context) error {
	b.stop.Do(func() {
		b.mu.Lock()
		started := b.started
		b.started = false
		b.stopped = true
		callID := b.callID
		b.callID = ""
		b.mu.Unlock()
		if started {
			slog.Info("voice bridge stopping", "call_id", callID)
		}
		if !started {
			return
		}
		// Single-owner backends: release ONLY via the recorded call id, and
		// never plain-Stop — a teardown of a stale/late socket must not cut
		// audio a newer call legitimately took over, and a bridge that never
		// started never owned the device so it must not stop it either.
		// Backends without ownership tracking keep plain Stop.
		if owned, ok := b.audio.(modem.OwnedVoiceAudio); ok {
			if callID != "" {
				_ = owned.StopIfOwner(ctx, callID)
			}
			return
		}
		_ = b.audio.Stop(ctx)
	})
	return nil
}
