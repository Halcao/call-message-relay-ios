package speexpp

import (
	"math/rand"
	"testing"
)

// BenchmarkThreeCallConcurrency measures the build-48 gateway denoise CPU
// cost for THREE concurrent calls, each with the real per-call topology:
// one session uplink preprocessor plus one downlink preprocessor per leg
// (a 1:1 call has exactly one external leg). Frames are synthesized
// broadband noise at 8 kHz / 20 ms / 160 samples (50 fps) — the mixer's
// cadence — so the benchmark number is directly comparable to per-call CPU
// on a gateway host. Run on-device with:
//
//	GOOS=linux GOARCH=arm64 CGO_ENABLED=1 CC=<musl-cc> go test -c -o speexpp-bench .
//	./speexpp-bench -test.run '^$' -test.bench ThreeCall -test.benchtime 10s
func BenchmarkThreeCallConcurrency(b *testing.B) {
	const calls = 3
	type channel struct{ pp *Preprocessor }
	channels := make([]channel, 0, calls*2)
	for i := 0; i < calls*2; i++ {
		pp, err := New(true)
		if err != nil {
			b.Fatalf("preprocessor %d: %v", i, err)
		}
		defer pp.Close()
		channels = append(channels, channel{pp: pp})
	}
	rng := rand.New(rand.NewSource(48))
	frame := make([]int16, FrameSamples)
	for i := range frame {
		frame[i] = int16(rng.Intn(12000) - 6000)
	}
	// Warm every state's noise estimate once before timing.
	for _, c := range channels {
		for i := 0; i < 100; i++ {
			_ = c.pp.Process(frame)
		}
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		// One 20 ms tick across all six channels (3 uplinks + 3 legs).
		for _, c := range channels {
			_ = c.pp.Process(frame)
		}
	}
}
