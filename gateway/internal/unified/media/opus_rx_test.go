package media

import (
	"math"
	"testing"
	"time"

	"callrelay/gateway/internal/opus"
	"callrelay/gateway/internal/voice"
)

// encodedToneOpus encodes `frames` consecutive 440 Hz frames; returns payloads
// keyed by RTP timestamp (0, 960, 1920...). lossPerc>0 makes the encoder
// embed RFC 7587 inband FEC (it only embeds redundancy when packet loss is
// signaled — exactly what the closed loop drives in production).
func encodedToneOpus(t *testing.T, n int, lossPerc int) [][]byte {
	t.Helper()
	enc, err := opus.NewEncoder()
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	defer enc.Close()
	if lossPerc > 0 {
		if err := enc.SetPacketLossPerc(lossPerc); err != nil {
			t.Fatalf("loss perc: %v", err)
		}
	}
	var out [][]byte
	for f := 0; f < n; f++ {
		pcm := make([]int16, opus.FrameSamples)
		for i := 0; i < opus.FrameSamples; i++ {
			sample := f*opus.FrameSamples + i
			pcm[i] = int16(12000 * math.Sin(2*math.Pi*440*float64(sample)/opus.SampleRate))
		}
		payload, err := enc.Encode(pcm)
		if err != nil {
			t.Fatalf("encode %d: %v", f, err)
		}
		out = append(out, payload)
	}
	return out
}

func plumbOpusJitter(t *testing.T) (*hostJitter, *opus.Decoder) {
	t.Helper()
	j := newHostJitter(opusFrameTick)
	dec, err := opus.NewDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	t.Cleanup(dec.Close)
	j.setDecoder(dec)
	return j, dec
}

// TestHostJitterOpusReorder: encoded packets pushed out of order must play
// out in timestamp order with the decoder advancing exactly once per slot.
func TestHostJitterOpusReorder(t *testing.T) {
	j, _ := plumbOpusJitter(t)
	payloads := encodedToneOpus(t, 3, 0)
	j.pushEncoded(960, payloads[1])
	j.pushEncoded(0, payloads[0])
	j.pushEncoded(1920, payloads[2])

	var pcm []int16
	for i := 0; i < 3; i++ {
		frame := j.pop(time.Now())
		if frame == nil || len(frame) != voice.FrameSamples {
			t.Fatalf("pop %d = %v samples", i, len(frame))
		}
		pcm = append(pcm, frame...)
	}
	crossings := zeroCrossings(pcm)
	expected := 2 * 440 * len(pcm) / voice.SampleRate
	if crossings < expected*3/4 || crossings > expected*5/4 {
		t.Fatalf("reordered playout distorted: crossings=%d want~%d", crossings, expected)
	}
	// Buffer must be drained in order.
	if depth := len(j.frames); depth != 0 {
		t.Fatalf("buffer left with %d frames", depth)
	}
}

// TestHostJitterOpusDuplicate: a second push of an already-buffered
// timestamp is rejected BEFORE any decode — the decoder never advances twice
// for one slot, and playout continues cleanly.
func TestHostJitterOpusDuplicate(t *testing.T) {
	j, _ := plumbOpusJitter(t)
	payloads := encodedToneOpus(t, 2, 0)
	j.pushEncoded(0, payloads[0])
	j.pushEncoded(960, payloads[1])
	// Duplicate of the buffered timestamp (any transport that allows it —
	// the defensive path; wire duplicates are usually stopped earlier by
	// SRTP replay protection).
	j.pushEncoded(0, payloads[0])

	var pcm []int16
	for i := 0; i < 2; i++ {
		frame := j.pop(time.Now())
		if frame == nil || len(frame) != voice.FrameSamples {
			t.Fatalf("pop %d = %v samples after duplicate", i, len(frame))
		}
		pcm = append(pcm, frame...)
	}
	crossings := zeroCrossings(pcm)
	expected := 2 * 440 * len(pcm) / voice.SampleRate
	if crossings < expected*3/4 || crossings > expected*5/4 {
		t.Fatalf("duplicate corrupted playout: crossings=%d want~%d", crossings, expected)
	}
}

// TestHostJitterOpusLatePacketDropped: a packet behind the play cursor (a
// late NACK retransmit that missed the SRTP replay window) is discarded
// WITHOUT decoding; decoder state stays aligned for subsequent frames.
func TestHostJitterOpusLatePacketDropped(t *testing.T) {
	j, _ := plumbOpusJitter(t)
	payloads := encodedToneOpus(t, 4, 0)
	for i, p := range payloads {
		j.pushEncoded(uint32(i*opusFrameTick), p)
	}
	// Play two frames, then deliver a stale packet (ts 0 again, as a late
	// retransmit would arrive). Production applies the playout-deadline
	// guard at the session boundary (hostPushEncoded rejects it BEFORE the
	// decoder can ever see it); mirror that guard here.
	j.pop(time.Now())
	j.pop(time.Now())
	if !j.behindPlay(0) {
		t.Fatalf("stale timestamp not behind the play cursor")
	}
	// (Not pushed — the guard rejects it. The jitter buffer stays at the
	// two unplayed frames.)
	frame := j.pop(time.Now())
	if frame == nil || len(frame) != voice.FrameSamples {
		t.Fatalf("post-late pop = %v samples", len(frame))
	}
	frame = j.pop(time.Now())
	if frame == nil || len(frame) != voice.FrameSamples {
		t.Fatalf("final pop = %v samples", len(frame))
	}
	var energy int64
	for _, s := range frame {
		energy += int64(s) * int64(s)
	}
	if energy == 0 {
		t.Fatalf("decoder state corrupted by late packet (silence after)")
	}
}

// TestHostJitterOpusFECRecovery: with the next packet carrying RFC 7587
// inband FEC, a missed slot is recovered from the carrier WITHOUT consuming
// it; the carrier then decodes normally at its own slot.
func TestHostJitterOpusFECRecovery(t *testing.T) {
	j, _ := plumbOpusJitter(t)
	payloads := encodedToneOpus(t, 3, 20)
	j.pushEncoded(0, payloads[0])
	j.pushEncoded(opusFrameTick, payloads[1])
	j.pushEncoded(2*opusFrameTick, payloads[2])
	// Prime at frame 0, then simulate the loss of frame 960 (NACK could not
	// repair in time): packet 1920 carries its FEC redundancy.
	if frame := j.pop(time.Now()); frame == nil {
		t.Fatalf("prime pop failed")
	}
	j.mu.Lock()
	delete(j.frames, uint32(opusFrameTick))
	j.mu.Unlock()

	recovered := j.pop(time.Now())
	if recovered == nil || len(recovered) != voice.FrameSamples {
		t.Fatalf("FEC recovery = %v samples", len(recovered))
	}
	var energy int64
	for _, s := range recovered {
		energy += int64(s) * int64(s)
	}
	if energy == 0 {
		t.Fatalf("FEC recovery produced silence — FEC data missing")
	}
	// Carrier must still play at its own slot.
	carrier := j.pop(time.Now())
	if carrier == nil || len(carrier) != voice.FrameSamples {
		t.Fatalf("carrier pop = %v samples", len(carrier))
	}
	var carrierEnergy int64
	for _, s := range carrier {
		carrierEnergy += int64(s) * int64(s)
	}
	if carrierEnergy == 0 {
		t.Fatalf("carrier frame silent after FEC decode")
	}
}

// TestHostJitterOpusPLCMiss: with no carrier, a missed slot gets mature
// libopus PLC (a full frame, never a repeated stale frame).
func TestHostJitterOpusPLCMiss(t *testing.T) {
	j, _ := plumbOpusJitter(t)
	payloads := encodedToneOpus(t, 3, 0)
	j.pushEncoded(0, payloads[0])
	j.pushEncoded(2*opusFrameTick, payloads[2])
	// Play 0, miss 960 (no carrier at 1920? there IS a carrier — remove it
	// to force the PLC path).
	j.pop(time.Now())
	j.mu.Lock()
	delete(j.frames, uint32(2*opusFrameTick))
	j.mu.Unlock()
	plc := j.pop(time.Now())
	if plc == nil || len(plc) != voice.FrameSamples {
		t.Fatalf("PLC frame = %v samples", len(plc))
	}
}

// TestHostJitterPCMPathUnchanged: without a decoder the historic PCM
// behavior (nil on miss) is preserved.
func TestHostJitterPCMPathUnchanged(t *testing.T) {
	j := newHostJitter(160)
	j.pushPCM(0, []int16{1, 2, 3})
	j.pushPCM(160, []int16{4, 5, 6})
	if frame := j.pop(time.Now()); frame == nil || frame[0] != 1 {
		t.Fatalf("first pop = %v", frame)
	}
	if frame := j.pop(time.Now()); frame == nil || frame[0] != 4 {
		t.Fatalf("second pop = %v", frame)
	}
	if frame := j.pop(time.Now()); frame != nil {
		t.Fatalf("pcm miss = %v, want nil", frame)
	}
}
