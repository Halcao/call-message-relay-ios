package voice

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestBoundedLogSanitizesAndCaps(t *testing.T) {
	log := newBoundedLog()
	// Non-printable bytes are replaced; tabs/newlines survive.
	if _, err := log.Write([]byte{'h', 'i', 0x00, '\t', '\n', 0x80}); err != nil {
		t.Fatal(err)
	}
	got := log.tail(100)
	if got != "hi?\t\n?" {
		t.Fatalf("sanitization failed: %q", got)
	}

	// Capacity: writing far beyond the cap keeps only the most recent
	// bytes, so the tail explains the process death.
	payload := strings.Repeat("x", log.cap+2000)
	marker := "FINAL-LINE"
	if _, err := log.Write([]byte(payload + marker)); err != nil {
		t.Fatal(err)
	}
	tail := log.tail(log.cap + 10)
	if len(tail) > log.cap || !strings.Contains(tail, marker) {
		t.Fatalf("capacity/tail behavior failed: len=%d marker=%v", len(tail), strings.Contains(tail, marker))
	}
}

func TestProcWatchCapturesExitAndStderr(t *testing.T) {
	log := newBoundedLog()
	// ALSA-style tool that prints a status line then exits nonzero.
	cmd := exec.Command("/bin/sh", "-c", "echo device-or-resource-busy 1>&2; exit 7")
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	watch := startProcWatch("aplay", cmd, log)
	watch.wait(2 * time.Second)
	summary := watch.summary()
	for _, want := range []string{"aplay exited", "exit status 7", "device-or-resource-busy"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q missing %q", summary, want)
		}
	}
}

func TestBridgeWarnPlaybackRateLimited(t *testing.T) {
	b := &Bridge{}
	failure := errors.New("write |1: broken pipe")
	b.warnPlayback("call-1", failure)
	b.warnPlayback("call-1", failure)
	b.warnPlayback("call-1", failure)
	// First call logs; the next two land inside the suppression window.
	if b.playbackSuppressed != 2 {
		t.Fatalf("suppressed = %d, want 2", b.playbackSuppressed)
	}
	// Expire the window: the next failure logs again and resets the count.
	b.lastPlaybackWarn = time.Now().Add(-playbackWarnInterval - time.Second)
	b.warnPlayback("call-1", failure)
	if b.playbackSuppressed != 0 {
		t.Fatalf("suppressed after window = %d, want 0", b.playbackSuppressed)
	}
}
