package media

import (
	"math"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/voice"
)

func decodedPCMU(t *testing.T, value int16) int16 {
	t.Helper()
	frame := make([]int16, voice.FrameSamples)
	for index := range frame {
		frame[index] = value
	}
	encoded, err := voice.EncodePCMU(frame)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := voice.DecodePCMU(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return decoded[0]
}

// TestRecordingTapsCanonicalFramesOncePerTick proves the recording tap
// contract on the real mixer:
//   - exactly one uplink and one downlink tap call per 20 ms tick (no
//     duplicate standby or conference copies),
//   - the uplink tap sees the canonical post-selection host frame,
//   - the downlink tap sees this call's own leg frame,
//   - the taps survive a host-transport replacement (the same Session object
//     survives ICE<->WS handover and hot-standby promotion),
//   - a lost host records silence (nil) instead of stale audio.
func TestRecordingTapsCanonicalFramesOncePerTick(t *testing.T) {
	session := NewSession(nil, "call:tape", "call")
	defer session.Close()

	var mu sync.Mutex
	var uplink, downlink [][]int16
	session.SetRecordingTaps(
		func(pcm []int16) {
			mu.Lock()
			uplink = append(uplink, append([]int16(nil), pcm...))
			mu.Unlock()
		},
		func(pcm []int16) {
			mu.Lock()
			downlink = append(downlink, append([]int16(nil), pcm...))
			mu.Unlock()
		},
	)

	transport := newFakeTransport(16)
	leg := NewLeg("call:tape", transport, nil)
	session.AddLeg(leg)
	session.mu.Lock()
	session.hostPresent = true
	gen := session.generation
	session.mu.Unlock()

	prime := func(base int16) {
		for index := 0; index < hostJitterTarget+4; index++ {
			pumpPush(session, gen, uint32(160*index), base+int16(index))
		}
	}
	prime(2000)
	wantDown := decodedPCMU(t, 4321)
	transport.frames <- pcmFrame(4321)
	waitUntil(t, time.Second, func() bool {
		depth, _ := leg.stats()
		return depth > 0
	})
	session.mixOnce()

	mu.Lock()
	if len(uplink) != 1 || len(downlink) != 1 {
		t.Fatalf("tap calls after one tick = uplink %d downlink %d, want 1/1", len(uplink), len(downlink))
	}
	upFrame, downFrame := uplink[0], downlink[0]
	mu.Unlock()
	if upFrame == nil {
		t.Fatal("uplink tap saw nil while host was live and primed")
	}
	if value := upFrame[0]; value < 2000 || value > 2000+int16(hostJitterTarget+4) {
		t.Fatalf("uplink tap sample = %d, want a primed host frame in [2000..]", value)
	}
	if downFrame == nil || downFrame[0] != wantDown {
		t.Fatalf("downlink tap = %v, want leg frame %d (PCMU-decoded 4321)", downFrame, wantDown)
	}

	// Host-transport replacement (handover): the session object persists, a
	// FRESH jitter buffer is installed, and the taps must keep receiving
	// exactly one frame per tick without duplicating.
	session.mu.Lock()
	session.host = newHostJitter(160)
	gen = session.generation
	session.mu.Unlock()
	prime(3000)
	wantDown2 := decodedPCMU(t, 7777)
	transport.frames <- pcmFrame(7777)
	waitUntil(t, time.Second, func() bool {
		depth, _ := leg.stats()
		return depth > 0
	})
	session.mixOnce()
	mu.Lock()
	if len(uplink) != 2 || len(downlink) != 2 {
		t.Fatalf("tap calls after handover = uplink %d downlink %d, want 2/2", len(uplink), len(downlink))
	}
	upFrame, downFrame = uplink[1], downlink[1]
	mu.Unlock()
	if upFrame == nil || upFrame[0] < 3000 {
		t.Fatalf("post-handover uplink tap = %v, want fresh 3000+ frame", upFrame)
	}
	if downFrame == nil || downFrame[0] != wantDown2 {
		t.Fatalf("post-handover downlink tap = %v, want %d (PCMU-decoded 7777)", downFrame, wantDown2)
	}

	// Host genuinely gone: the tap still fires once per tick with nil so the
	// recording timeline keeps running, but no stale audio is captured.
	session.mu.Lock()
	session.hostPresent = false
	session.mu.Unlock()
	session.mixOnce()
	mu.Lock()
	defer mu.Unlock()
	if len(uplink) != 3 || len(downlink) != 3 {
		t.Fatalf("tap calls after host loss = uplink %d downlink %d, want 3/3", len(uplink), len(downlink))
	}
	if uplink[2] != nil || downlink[2] != nil {
		t.Fatalf("host-loss taps = %v / %v, want nil/nil", uplink[2], downlink[2])
	}
}

// TestConferenceRecordingTapsMixAllLegsExactlyOnce proves the conference tap
// contract on the real mixer:
//   - exactly one uplink and one downlink tap call per tick,
//   - the uplink tap sees the canonical host frame,
//   - the downlink tap sees the CLIENT MIX of every unheld leg (summed once,
//     in sorted order, then limited), never one leg twice and never a stale
//     copy,
//   - a held leg is drained and excluded from the recording, matching what
//     the host actually hears,
//   - a lost host records silence for both directions.
func TestConferenceRecordingTapsMixAllLegsExactlyOnce(t *testing.T) {
	session := NewSession(nil, "conf:tape", "conference")
	defer session.Close()

	var mu sync.Mutex
	var uplink, downlink [][]int16
	session.SetRecordingTaps(
		func(pcm []int16) {
			mu.Lock()
			uplink = append(uplink, append([]int16(nil), pcm...))
			mu.Unlock()
		},
		func(pcm []int16) {
			mu.Lock()
			downlink = append(downlink, append([]int16(nil), pcm...))
			mu.Unlock()
		},
	)

	transportA := newFakeTransport(16)
	transportB := newFakeTransport(16)
	legA := NewLeg("call:a", transportA, nil)
	legB := NewLeg("call:b", transportB, nil)
	session.AddLeg(legA)
	session.AddLeg(legB)
	session.mu.Lock()
	session.hostPresent = true
	gen := session.generation
	session.mu.Unlock()

	prime := func(base int16) {
		for index := 0; index < hostJitterTarget+4; index++ {
			pumpPush(session, gen, uint32(160*index), base+int16(index))
		}
	}
	prime(2000)
	aDecoded := decodedPCMU(t, 3000)
	bDecoded := decodedPCMU(t, 1000)
	transportA.frames <- pcmFrame(3000)
	transportB.frames <- pcmFrame(1000)
	waitUntil(t, time.Second, func() bool {
		ad, _ := legA.stats()
		bd, _ := legB.stats()
		return ad > 0 && bd > 0
	})
	session.mixOnce()

	mu.Lock()
	if len(uplink) != 1 || len(downlink) != 1 {
		t.Fatalf("tap calls after one tick = uplink %d downlink %d, want 1/1", len(uplink), len(downlink))
	}
	upFrame, downFrame := uplink[0], downlink[0]
	mu.Unlock()
	if upFrame == nil || upFrame[0] < 2000 {
		t.Fatalf("uplink tap = %v, want a primed host frame", upFrame)
	}
	wantMix := int16(math.Round(0.9*float64(aDecoded) + 0.9*float64(bDecoded)))
	if downFrame == nil {
		t.Fatal("conference downlink tap saw nil while both legs were live")
	}
	if got := downFrame[0]; got != wantMix {
		t.Fatalf("conference downlink tap = %d, want both legs mixed once (%d)", got, wantMix)
	}
	// A duplicated leg would land near 0.9*(a+a+b) or the limiter's ceiling;
	// the exact mix equality above plus this bound pins "once".
	if doubled := 0.9*float64(aDecoded)*2 + 0.9*float64(bDecoded); float64(downFrame[0]) >= doubled {
		t.Fatalf("downlink tap %d looks like a duplicated leg mix (bound %.0f)", downFrame[0], doubled)
	}

	// Holding a leg removes it from the host mix and therefore from the
	// recording; the other leg keeps being captured.
	legB.SetHeld(true)
	transportB.frames <- pcmFrame(2000)
	transportA.frames <- pcmFrame(3000)
	waitUntil(t, time.Second, func() bool {
		ad, _ := legA.stats()
		return ad > 0
	})
	session.mixOnce()
	mu.Lock()
	downFrame = downlink[len(downlink)-1]
	mu.Unlock()
	if wantOnlyA := int16(math.Round(0.9 * float64(aDecoded))); downFrame == nil || downFrame[0] != wantOnlyA {
		t.Fatalf("held-leg downlink tap = %v, want only the unheld leg mix (%d)", downFrame, wantOnlyA)
	}

	// Host-transport replacement (ICE<->WS / hot-standby promotion): the
	// session object persists, a FRESH jitter buffer is installed, and the
	// conference taps must keep mixing both legs exactly once per tick.
	session.mu.Lock()
	session.host = newHostJitter(160)
	gen = session.generation
	session.mu.Unlock()
	legB.SetHeld(false)
	prime(3000)
	transportA.frames <- pcmFrame(3000)
	transportB.frames <- pcmFrame(1000)
	waitUntil(t, time.Second, func() bool {
		ad, _ := legA.stats()
		bd, _ := legB.stats()
		return ad > 0 && bd > 0
	})
	session.mixOnce()
	mu.Lock()
	upFrame, downFrame = uplink[len(uplink)-1], downlink[len(downlink)-1]
	mu.Unlock()
	if upFrame == nil || upFrame[0] < 3000 {
		t.Fatalf("post-handover conference uplink tap = %v, want a fresh 3000+ host frame", upFrame)
	}
	if downFrame == nil || downFrame[0] != wantMix {
		t.Fatalf("post-handover conference downlink tap = %v, want both legs mixed once (%d)", downFrame, wantMix)
	}

	// Host genuinely gone: the taps still fire once per tick with nil so the
	// recording timeline keeps running, but no stale audio is captured.
	session.mu.Lock()
	session.hostPresent = false
	session.mu.Unlock()
	session.mixOnce()
	mu.Lock()
	defer mu.Unlock()
	if len(uplink) != len(downlink) {
		t.Fatalf("tap call counts diverged: uplink %d downlink %d", len(uplink), len(downlink))
	}
	if last := len(uplink) - 1; uplink[last] != nil || downlink[last] != nil {
		t.Fatalf("host-loss taps = %v / %v, want nil/nil", uplink[last], downlink[last])
	}
}
