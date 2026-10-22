package voice

import (
	"context"
	"strings"
	"testing"
)

func TestOpenALSARequiresBothDevices(t *testing.T) {
	if _, err := OpenALSA("", "hw:0,0"); err == nil {
		t.Fatal("empty capture device was accepted")
	}
	if _, err := OpenALSA("hw:0,0", ""); err == nil {
		t.Fatal("empty playback device was accepted")
	}
}

func TestALSAProbeRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	audio, err := OpenALSA("hw:0,0", "hw:0,0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audio.Probe(ctx); err == nil {
		t.Fatal("canceled probe succeeded")
	}
}

// TestArecordPeriodMatchesVoiceFrame locks the capture period to the 20 ms
// voice frame. A larger period makes arecord deliver several frames per pipe
// read; the bridge forwards them as one burst and the daemon's per-tick leg
// queue trims real modem audio (build 14 choppy/silent downlink, 2026-10-03).
func TestArecordPeriodMatchesVoiceFrame(t *testing.T) {
	args := arecordArgs("hw:CARD=Baiwang,DEV=0")
	period := ""
	buffer := ""
	for index, arg := range args {
		if arg == "--period-size=160" {
			period = arg
		}
		if arg == "--buffer-size=8000" {
			buffer = arg
		}
		_ = index
	}
	if period == "" {
		t.Fatalf("capture period must be %d samples (20 ms frame), args: %v", FrameSamples, args)
	}
	if buffer == "" {
		t.Fatalf("capture buffer must hold ~1 s of audio, args: %v", args)
	}
	if FrameSamples != 160 {
		t.Fatalf("frame size drifted to %d; update the capture period with it", FrameSamples)
	}
}

func TestAplayArgsKeepRingBufferDepth(t *testing.T) {
	args := aplayArgs("hw:CARD=Baiwang,DEV=0")
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--buffer-size=8192") {
		t.Fatalf("playback ring buffer too small for UAC clock jitter: %v", args)
	}
	// The playback period must match the 20 ms voice frame; a 1024-sample
	// period made every blocking write wait a full 128 ms hardware period and
	// was the scheduling cliff behind the choppy-uplink report (2026-10-05).
	if !strings.Contains(joined, "--period-size=160") {
		t.Fatalf("playback period must be %d samples (20 ms frame), args: %v", FrameSamples, args)
	}
}
