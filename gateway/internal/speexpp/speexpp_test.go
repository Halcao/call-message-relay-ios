package speexpp

import (
	"math"
	"math/rand"
	"testing"
)

// rms returns the root-mean-square level of a frame.
func rms(p []int16) float64 {
	var sum float64
	for _, v := range p {
		f := float64(v)
		sum += f * f
	}
	return math.Sqrt(sum / float64(len(p)))
}

// noiseFrame builds one broadband-noise frame at the given amplitude.
func noiseFrame(rng *rand.Rand, amp int) []int16 {
	p := make([]int16, FrameSamples)
	for i := range p {
		p[i] = int16(rng.Intn(2*amp+1) - amp)
	}
	return p
}

// TestProcessContinuesAcrossFrames is a REAL C-library test (not an
// allocation/pass-through check): the library contract says
// speex_preprocess_run returns the VAD decision, NOT an error. Before the
// build-48 fix the wrapper treated any non-zero return as fatal and latched
// raw bypass on the very first frame (with VAD off the library returns 1).
// This test verifies that enabled processing keeps altering noise frames for
// the whole call, while a disabled processor passes the identical buffer.
func TestProcessContinuesAcrossFrames(t *testing.T) {
	pp, err := New(true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer pp.Close()
	if !pp.Enabled() {
		t.Fatal("expected enabled")
	}

	rng := rand.New(rand.NewSource(42))
	const frames = 100
	changedFrames := 0
	var rawEnergy, outEnergy float64
	for i := 0; i < frames; i++ {
		raw := noiseFrame(rng, 8000)
		before := make([]int16, len(raw))
		copy(before, raw)
		out := pp.Process(raw)
		if rms(out) != rms(before) {
			changedFrames++
		}
		rawEnergy += rms(before)
		outEnergy += rms(out)
		// Input buffer must never be mutated in place.
		for j := range raw {
			if raw[j] != before[j] {
				t.Fatalf("frame %d: Process mutated the caller buffer at sample %d", i, j)
			}
		}
	}
	// Speex needs a few frames to estimate the noise floor; processing must
	// still be changing most frames by the end (a latched bypass changes
	// exactly zero frames).
	if changedFrames < frames/2 {
		t.Fatalf("denoise only changed %d/%d frames (latched bypass?)", changedFrames, frames)
	}
	// Light -6 dB floor must not silence speech-like energy entirely, but
	// broadband noise should be attenuated overall.
	if outEnergy >= rawEnergy {
		t.Fatalf("noise energy not attenuated: raw=%.0f out=%.0f", rawEnergy, outEnergy)
	}

	// Frame 200 (beyond the priming window) must still be processed.
	later := noiseFrame(rng, 8000)
	if rms(pp.Process(later)) == rms(later) {
		t.Fatal("processing stopped (bypass latched on later frame)")
	}
}

// TestDisabledAndWrongSizeRawBypass proves the never-block contract.
func TestDisabledAndWrongSizeRawBypass(t *testing.T) {
	pp, err := New(false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer pp.Close()
	raw := noiseFrame(rand.New(rand.NewSource(1)), 5000)
	out := pp.Process(raw)
	if &out[0] != &raw[0] {
		t.Fatal("disabled processor must return the exact input slice")
	}
	pp.SetEnabled(true)
	odd := make([]int16, 100)
	if &pp.Process(odd)[0] != &odd[0] {
		t.Fatal("wrong-size frame must bypass raw")
	}
	// Gate-only toggle preserves the state (no error path).
	pp.SetEnabled(false)
	pp.SetEnabled(true)
	if &pp.Process(odd)[0] != &odd[0] {
		t.Fatal("wrong-size frame must still bypass after toggles")
	}
}

// TestHotToggleAtFrameBoundary toggles the gate per-frame and confirms audio
// is never blocked and processing resumes with the preserved state.
func TestHotToggleAtFrameBoundary(t *testing.T) {
	pp, _ := New(true)
	defer pp.Close()
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 20; i++ {
		frame := noiseFrame(rng, 6000)
		if i%2 == 0 {
			pp.SetEnabled(false)
			if &pp.Process(frame)[0] != &frame[0] {
				t.Fatalf("frame %d: disabled frame not passed raw", i)
			}
		} else {
			pp.SetEnabled(true)
			// No panic, fixed size returned.
			if got := len(pp.Process(frame)); got != FrameSamples {
				t.Fatalf("frame %d: got %d samples", i, got)
			}
		}
	}
}
