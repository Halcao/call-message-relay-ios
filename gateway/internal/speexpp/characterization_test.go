package speexpp

import (
	"math"
	"testing"
)

// TestProcessingCharacterization is ENGINEERING EVIDENCE, NOT human-speech
// quality acceptance (build-49 review): the requested authorized human-voice
// fixture with established license could not be obtained, so the shipping
// default flipped to OFF (both switches fully usable; see runtime/denoise.go
// and the iOS/PWA stores). What this test establishes with deterministic
// SYNTHETIC signals (no fixture files, no redistribution questions):
//
// FIRM gates (processor safety — must never regress):
//   - enabled processing keeps altering frames across the whole call (no
//     bypass latch, no VAD-return misuse);
//   - a disabled processor is bit-exact pass-through and wrong-size frames
//     bypass raw (never block/drop);
//   - hot toggling never blocks or clicks and preserves the noise estimate;
//   - frame count is preserved (no buffering/trimming).
//
// MEASURED FACTS (reported, gated only against the approved bound):
//   - added delay per direction, measured by cross-correlating a
//     non-periodic chirp against the processed output over a signed
//     +/-30 ms window. Measured on SpeexDSP 1.2.1 @ 8 kHz/20 ms frames:
//     one full frame (+160 samples, 20.00 ms) — AT the approved <= 20 ms
//     per-direction bound with zero headroom — plus the first warmup frame
//     is strongly suppressed (the noise estimate starts from nothing).
//     This is a per-frame processor filter delay, not buffering.
//   - voiced-material correlation computed AFTER compensating the measured
//     alignment: 0.988 on the synthetic voiced signal (speech-like material
//     preserved once aligned); adapted pause-noise attenuation ~2.3 dB
//     (light processing).
//
// Human listening quality remains the physical user gate; nothing here
// claims otherwise, and the near-zero realtime headroom plus unestablished
// human-quality evidence is exactly why the shipping default is OFF.
func TestProcessingCharacterization(t *testing.T) {

	t.Run("safety_no_bypass_no_block", func(t *testing.T) {
		clean := syntheticVoiced(8000, 4)
		noisy := mixAtSNR(clean, noiseSignal("hiss", len(clean), 99), 10)
		pp, err := New(true)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		out := make([]int16, len(noisy))
		for i := 0; i+FrameSamples <= len(noisy); i += FrameSamples {
			copy(out[i:i+FrameSamples], pp.Process(noisy[i:i+FrameSamples]))
		}
		pp.Close()
		if len(out) != len(noisy) {
			t.Fatalf("frame count changed: in %d out %d (buffering/trimming!)", len(noisy), len(out))
		}
		changed := 0
		for i := 0; i+FrameSamples <= len(noisy); i += FrameSamples {
			if framePower(out[i:i+FrameSamples]) != framePower(noisy[i:i+FrameSamples]) {
				changed++
			}
		}
		if changed < (len(noisy)/FrameSamples)/2 {
			t.Errorf("only %d/%d frames altered (bypass latched?)", changed, len(noisy)/FrameSamples)
		}
		if peak := peakAbs(out); peak > 32767 {
			t.Errorf("clipping: output peak %d exceeds int16 full scale", peak)
		}
	})

	t.Run("safety_disabled_and_wrong_size_bypass", func(t *testing.T) {
		pp, err := New(false)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer pp.Close()
		raw := noiseSignal("hiss", FrameSamples, 3)
		if out := pp.Process(raw); &out[0] != &raw[0] {
			t.Fatal("disabled processor must return the exact input slice")
		}
		pp.SetEnabled(true)
		odd := make([]int16, 100)
		if out := pp.Process(odd); &out[0] != &odd[0] {
			t.Fatal("wrong-size frame must bypass raw")
		}
	})

	t.Run("safety_hot_toggle_no_click_state_preserved", func(t *testing.T) {
		clean := syntheticVoiced(8000, 4)
		noisy := mixAtSNR(clean, noiseSignal("hiss", len(clean), 7), 10)
		pp, err := New(true)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer pp.Close()
		type rec struct{ in, out []int16 }
		var before, across, after []rec
		for i := 0; i+FrameSamples <= len(noisy); i += FrameSamples {
			frame := noisy[i : i+FrameSamples]
			switch {
			case i < 40*FrameSamples:
				pp.SetEnabled(true)
				before = append(before, rec{frame, pp.Process(frame)})
			case i < 45*FrameSamples:
				pp.SetEnabled(false)
				across = append(across, rec{frame, pp.Process(frame)})
			default:
				pp.SetEnabled(true)
				after = append(after, rec{frame, pp.Process(frame)})
			}
		}
		for j, r := range across {
			for k := range r.in {
				if r.out[k] != r.in[k] {
					t.Fatalf("disabled frame %d sample %d not raw pass-through", j, k)
				}
			}
		}
		maxDelta := 0
		for _, r := range before {
			for k := 1; k < len(r.out); k++ {
				if d := absDiff(r.out[k], r.out[k-1]); d > maxDelta {
					maxDelta = d
				}
			}
		}
		boundary := [][]int16{before[len(before)-1].out, across[0].out, across[len(across)-1].out, after[0].out}
		for bi := 0; bi+1 < len(boundary); bi++ {
			prev, next := boundary[bi], boundary[bi+1]
			if d := absDiff(next[0], prev[len(prev)-1]); d > maxDelta*2+256 {
				t.Errorf("toggle boundary %d delta %d exceeds 2x running max %d (click)", bi, d, maxDelta)
			}
		}
		flat := func(rs []rec) []int16 {
			var out []int16
			for _, r := range rs {
				out = append(out, r.out...)
			}
			return out
		}
		gIn, gOut, _ := segmentStats(clean[45*FrameSamples:], noisy[45*FrameSamples:], flat(after))
		if len(gIn) == 0 {
			t.Skip("no pauses in the after region")
		}
		if db(mean(gIn))-db(mean(gOut)) < 1.5 {
			t.Error("noise estimate lost across hot toggle (cold restart)")
		}
	})

	t.Run("measured_added_delay_within_20ms", func(t *testing.T) {
		// Non-periodic chirp: the instantaneous frequency sweeps the whole
		// speech band, so the noise tracker cannot lock onto it and the
		// autocorrelation has a single unambiguous peak at lag 0.
		sig := syntheticChirp(8000, 3)
		pp, err := New(true)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer pp.Close()
		out := make([]int16, len(sig))
		for i := 0; i+FrameSamples <= len(sig); i += FrameSamples {
			copy(out[i:i+FrameSamples], pp.Process(sig[i:i+FrameSamples]))
		}
		// Signed +/-30 ms window (240 samples @ 8 kHz).
		lag, corr := bestLag(sig, out, 240)
		ms := float64(lag) / 8.0
		t.Logf("measured added delay: lag %+d samples (%+.2f ms), correlation %.3f", lag, ms, corr)
		if abs(lag) > 160 { // 20 ms @ 8 kHz — the APPROVED added-delay bound.
			t.Errorf("added delay %+d samples (%+.2f ms) exceeds the 20 ms per-direction bound", lag, ms)
		}
	})

	t.Run("measured_voiced_correlation_after_alignment", func(t *testing.T) {
		clean := syntheticVoiced(8000, 4)
		noisy := mixAtSNR(clean, noiseSignal("hiss", len(clean), 99), 10)
		pp, err := New(true)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer pp.Close()
		out := make([]int16, len(noisy))
		for i := 0; i+FrameSamples <= len(noisy); i += FrameSamples {
			copy(out[i:i+FrameSamples], pp.Process(noisy[i:i+FrameSamples]))
		}
		// Measure the processor's alignment on the chirp, then evaluate the
		// voiced correlation at the SAME lag (never zero-lag correlation).
		chirp := syntheticChirp(8000, 2)
		pp2, _ := New(true)
		chirpOut := make([]int16, len(chirp))
		for i := 0; i+FrameSamples <= len(chirp); i += FrameSamples {
			copy(chirpOut[i:i+FrameSamples], pp2.Process(chirp[i:i+FrameSamples]))
		}
		pp2.Close()
		lag, _ := bestLag(chirp, chirpOut, 240)
		_, _, voiced := segmentStats(clean, noisy, out)
		corr := correlationAtLag(voiced.in, voiced.out, lag)
		t.Logf("voiced correlation at measured lag %+d: %.3f (pause-noise attenuation reported by other suites)", lag, corr)
		if corr < 0.6 {
			t.Errorf("voiced correlation %.3f at measured lag %+d below 0.6 floor (processing destroys voiced material)", corr, lag)
		}
	})
}

// syntheticVoiced builds a deterministic speech-LIKE signal: two harmonics
// with 4 Hz amplitude modulation (speech formant dynamics) inside 0.6 s
// voiced bursts separated by 0.3 s pauses, raised-cosine edges. Clearly
// labeled synthetic — NOT a human-voice fixture.
func syntheticVoiced(sampleRate int, seconds int) []int16 {
	n := sampleRate * seconds
	out := make([]int16, n)
	const voicedLen = 4800 // 0.6 s @ 8 kHz
	const pauseLen = 2400  // 0.3 s
	for i := 0; i < n; i++ {
		cycle := i % (voicedLen + pauseLen)
		if cycle >= voicedLen {
			continue
		}
		env := 1.0
		if cycle < 160 {
			env = float64(cycle) / 160.0
		} else if cycle > voicedLen-160 {
			env = float64(voicedLen-cycle) / 160.0
		}
		phase := float64(i) / float64(sampleRate)
		am := 0.7 + 0.3*math.Sin(2*math.Pi*4*phase)
		v := am * (0.55*math.Sin(2*math.Pi*293*phase) + 0.25*math.Sin(2*math.Pi*911*phase))
		out[i] = int16(v * env * 20000)
	}
	return out
}

// syntheticChirp sweeps 100 -> 3400 -> 100 Hz twice over the duration: a
// deterministic non-periodic probe with a unique autocorrelation peak. The
// phase is accumulated incrementally from the instantaneous frequency.
func syntheticChirp(sampleRate int, seconds int) []int16 {
	n := sampleRate * seconds
	out := make([]int16, n)
	phase := 0.0
	half := float64(seconds) / 2
	step := 1.0 / float64(sampleRate)
	for i := 0; i < n; i++ {
		t := float64(i) / float64(sampleRate)
		p := math.Mod(t/half, 1.0)
		f := 100.0 + (3400.0-100.0)*p
		if int(t/half)%2 == 1 {
			f = 3400.0 - (3400.0-100.0)*p
		}
		phase += 2 * math.Pi * f * step
		out[i] = int16(0.5 * math.Sin(phase) * 20000)
	}
	return out
}

// bestLag returns the signed lag (samples) of the cross-correlation peak
// within +/-maxLag and the peak value.
func bestLag(in, out []int16, maxLag int) (int, float64) {
	bestLag, best := 0, math.Inf(-1)
	for lag := -maxLag; lag <= maxLag; lag++ {
		if c := corrAtLag(in, out, lag); c > best {
			best, bestLag = c, lag
		}
	}
	return bestLag, best
}

func corrAtLag(a, b []int16, lag int) float64 {
	start := 0
	if lag < 0 {
		start = -lag
	}
	end := len(a)
	if lag > 0 {
		end = len(a) - lag
	}
	if end <= start {
		return 0
	}
	var num, da, db float64
	for i := start; i < end; i++ {
		x, y := float64(a[i]), float64(b[i+lag])
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}

// --- signal helpers ---

func framePower(p []int16) float64 {
	var s float64
	for _, v := range p {
		f := float64(v)
		s += f * f
	}
	return s / float64(len(p))
}

func meanSquare(p []int16) float64 {
	var s float64
	for _, v := range p {
		f := float64(v)
		s += f * f
	}
	return s / float64(len(p))
}

func mixAtSNR(clean, noise []int16, snrDB float64) []int16 {
	sigPow, noisePow := meanSquare(clean), meanSquare(noise)
	if noisePow == 0 {
		return append([]int16(nil), clean...)
	}
	gain := math.Sqrt(sigPow / (noisePow * math.Pow(10, snrDB/10)))
	out := make([]int16, len(clean))
	for i := range out {
		v := float64(clean[i]) + gain*float64(noise[i])
		if v > 32767 {
			v = 32767
		} else if v < -32768 {
			v = -32768
		}
		out[i] = int16(v)
	}
	return out
}

// noiseSignal: "hiss" = white noise; "fan" = integrated low-frequency rumble.
func noiseSignal(kind string, n int, seed int64) []int16 {
	rng := newRand(seed)
	out := make([]int16, n)
	switch kind {
	case "hiss":
		for i := range out {
			out[i] = int16(rng.next() * 32767)
		}
	case "fan":
		var acc float64
		for i := range out {
			acc = acc*0.985 + rng.next()*0.15
			out[i] = int16(acc * 32767)
		}
	default:
		panic(kind)
	}
	return out
}

type voicedSeg struct{ in, out []float64 }

// segmentStats buckets frames into pauses (clean power below 2% of clean
// peak) and voiced segments.
func segmentStats(clean, in, out []int16) (gapIn, gapOut []float64, voiced voicedSeg) {
	peak := float64(peakAbs(clean))
	gapThresh := (peak * 0.02) * (peak * 0.02) * FrameSamples
	for i := 0; i+FrameSamples <= len(clean); i += FrameSamples {
		c := clean[i : i+FrameSamples]
		var p float64
		for _, s := range c {
			f := float64(s)
			p += f * f
		}
		if p < gapThresh {
			gapIn = append(gapIn, framePower(in[i:i+FrameSamples]))
			gapOut = append(gapOut, framePower(out[i:i+FrameSamples]))
		} else {
			for _, s := range in[i : i+FrameSamples] {
				voiced.in = append(voiced.in, float64(s))
			}
			for _, s := range out[i : i+FrameSamples] {
				voiced.out = append(voiced.out, float64(s))
			}
		}
	}
	return gapIn, gapOut, voiced
}

func correlationAtLag(a, b []float64, lag int) float64 {
	start := 0
	if lag < 0 {
		start = -lag
	}
	end := len(a)
	if lag > 0 {
		end = len(a) - lag
	}
	if end <= start {
		return 0
	}
	var num, da, db float64
	for i := start; i < end; i++ {
		num += a[i] * b[i+lag]
		da += a[i] * a[i]
		db += b[i+lag] * b[i+lag]
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}

func mean(p []float64) float64 {
	if len(p) == 0 {
		return 1
	}
	var s float64
	for _, v := range p {
		s += v
	}
	return s / float64(len(p))
}

func db(p float64) float64 {
	if p <= 0 {
		return -120
	}
	return 10 * math.Log10(p)
}

func peakAbs(p []int16) int {
	m := 0
	for _, v := range p {
		if a := absVal(v); a > m {
			m = a
		}
	}
	return m
}

func absVal(v int16) int {
	if v < 0 {
		return -int(v)
	}
	return int(v)
}

func absDiff(a, b int16) int {
	d := int(a) - int(b)
	if d < 0 {
		return -d
	}
	return d
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

type lcg struct{ s int64 }

func newRand(seed int64) *lcg { return &lcg{s: seed*2654435761 + 1} }

func (r *lcg) next() float64 {
	r.s = r.s*6364136223846793005 + 1442695040888963407
	return (float64((r.s>>11)%2000000) / 1000000.0) - 1.0
}
