package opus

import (
	"math"
	"testing"
)

// synthTone builds a 20 ms 440 Hz full-scale-ish sine at 8 kHz.
func synthTone() []int16 {
	pcm := make([]int16, FrameSamples)
	for i := 0; i < FrameSamples; i++ {
		pcm[i] = int16(12000 * math.Sin(2*math.Pi*440*float64(i)/SampleRate))
	}
	return pcm
}

func energy(pcm []int16) int64 {
	var e int64
	for _, s := range pcm {
		e += int64(s) * int64(s)
	}
	return e
}

func TestRoundTripPreservesEnergy(t *testing.T) {
	enc, err := NewEncoder()
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	defer enc.Close()
	dec, err := NewDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer dec.Close()

	pcm := synthTone()
	encoded, err := enc.Encode(pcm)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) == 0 || len(encoded) > MaxFrameBytes {
		t.Fatalf("encoded frame = %d bytes", len(encoded))
	}
	decoded, err := dec.Decode(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != FrameSamples {
		t.Fatalf("decoded %d samples, want %d", len(decoded), FrameSamples)
	}
	src, dst := energy(pcm), energy(decoded)
	// Opus at 32 kbps narrowband keeps voiced energy in a loose band; the
	// invariant needed by the mixer is "clearly non-silence, roughly
	// conserved", not sample equality.
	if dst == 0 || dst < src/8 || dst > src*4 {
		t.Fatalf("energy not conserved: src=%d dst=%d", src, dst)
	}
}

func TestPacketLossConcealmentFillsFrame(t *testing.T) {
	enc, _ := NewEncoder()
	defer enc.Close()
	dec, _ := NewDecoder()
	defer dec.Close()

	// Feed real audio so the decoder has state to conceal from.
	pcm := synthTone()
	for i := 0; i < 5; i++ {
		pkt, err := enc.Encode(pcm)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		if _, err := dec.Decode(pkt); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	// A lost packet (nil payload) must produce a full PLC frame — the
	// mature concealment the gateway relies on for NACK gaps.
	concealed, err := dec.Decode(nil)
	if err != nil {
		t.Fatalf("plc decode: %v", err)
	}
	if len(concealed) != FrameSamples {
		t.Fatalf("plc produced %d samples, want %d", len(concealed), FrameSamples)
	}
}

func TestCTLBoundsAndDefaults(t *testing.T) {
	enc, err := NewEncoder()
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	defer enc.Close()
	if err := enc.SetBitrate(-1); err == nil {
		t.Fatalf("negative bitrate accepted")
	}
	if err := enc.SetBitrate(1 << 20); err == nil {
		t.Fatalf("absurd bitrate accepted")
	}
	if err := enc.SetBitrate(24000); err != nil {
		t.Fatalf("in-range bitrate rejected: %v", err)
	}
	if err := enc.SetPacketLossPerc(101); err == nil {
		t.Fatalf("loss perc >100 accepted")
	}
	if err := enc.SetPacketLossPerc(20); err != nil {
		t.Fatalf("loss perc 20 rejected: %v", err)
	}
	if err := enc.SetInbandFEC(true); err != nil {
		t.Fatalf("fec enable: %v", err)
	}
	if err := enc.SetDTX(false); err != nil {
		t.Fatalf("dtx: %v", err)
	}
	if err := enc.SetComplexity(11); err == nil {
		t.Fatalf("complexity 11 accepted")
	}
}

func TestDTXSilenceIsTiny(t *testing.T) {
	enc, err := NewEncoder()
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	defer enc.Close()
	silence := make([]int16, FrameSamples)
	pkt, err := enc.Encode(silence)
	if err != nil {
		t.Fatalf("encode silence: %v", err)
	}
	if len(pkt) > 8 {
		t.Fatalf("dtx silence packet = %d bytes, want tiny comfort frame", len(pkt))
	}
}

func TestVersionReported(t *testing.T) {
	if Version() == "" {
		t.Fatalf("empty opus version")
	}
	t.Logf("libopus: %s", Version())
}
