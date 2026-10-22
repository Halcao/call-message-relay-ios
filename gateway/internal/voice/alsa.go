package voice

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"callrelay/gateway/internal/modem"
)

// ALSAAudio connects a USB Audio Class modem through the standard ALSA
// command-line tools. Keeping the process boundary here means the rest of
// Gateway only sees signed PCM and does not need cgo or vendor libraries.
// The configured devices must support mono S16_LE at 8000 Hz.
//
// Locking rules (learned the hard way — a Process.Wait under a.mu wedged
// the whole gateway): a.mu only guards the pointer fields. Killing and
// waiting on child processes happens OUTSIDE the lock, so a stuck child
// can never hold up the next call's Start.
type ALSAAudio struct {
	captureDevice  string
	playbackDevice string

	mu            sync.Mutex
	call          modem.CallID
	capture       *exec.Cmd
	playback      *exec.Cmd
	captureWatch  *procWatch
	playbackWatch *procWatch
	rx            io.ReadCloser
	tx            io.WriteCloser
	txMu          sync.Mutex
	once          sync.Once
}

func OpenALSA(captureDevice, playbackDevice string) (*ALSAAudio, error) {
	captureDevice = strings.TrimSpace(captureDevice)
	playbackDevice = strings.TrimSpace(playbackDevice)
	if captureDevice == "" || playbackDevice == "" {
		return nil, fmt.Errorf("alsa voice backend requires capture and playback devices")
	}
	return &ALSAAudio{captureDevice: captureDevice, playbackDevice: playbackDevice}, nil
}

func (a *ALSAAudio) Probe(ctx context.Context) (modem.AudioCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return modem.AudioCapabilities{}, err
	}
	if _, err := exec.LookPath("arecord"); err != nil {
		return modem.AudioCapabilities{}, fmt.Errorf("arecord is unavailable: %w", err)
	}
	if _, err := exec.LookPath("aplay"); err != nil {
		return modem.AudioCapabilities{}, fmt.Errorf("aplay is unavailable: %w", err)
	}
	return modem.AudioCapabilities{Backend: "alsa-uac", SampleRate: SampleRate, Channels: Channels}, nil
}

// snapshot returns the current pair for IO.
func (a *ALSAAudio) snapshot() (rx io.ReadCloser, tx io.WriteCloser, callID modem.CallID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rx, a.tx, a.call
}

// arecordArgs builds the capture command. The capture period MUST equal the
// 20 ms voice frame: the bridge readLoop consumes exactly FrameSamples per
// read, so a larger ALSA period delivers N frames in one burst and the
// daemon's per-tick leg queue trims real audio (observed 2026-10-03:
// leg_trimmed ~20 frames/s with a 1024-sample period). A 160-sample period
// yields a steady 50 frames/s; the buffer holds ~1 s (50 periods) so UAC
// clock jitter is still absorbed.
func arecordArgs(captureDevice string) []string {
	return []string{
		"-q", "-D", captureDevice, "-t", "raw",
		"-f", "S16_LE", "-r", fmt.Sprint(SampleRate), "-c", fmt.Sprint(Channels),
		"--buffer-size=8000", "--period-size=160",
	}
}

// aplayArgs builds the playback command. The period must match the 20 ms
// voice frame: the bridge's pacer writes one 160-sample frame per slot, and
// a 1024-sample period made every blocking write wait for a full 128 ms
// hardware period, which is the scheduling cliff that let the old ticker
// drop frames (choppy uplink, 2026-10-05). A 160-sample period lets the UAC
// ring accept each frame on its own 20 ms boundary. The buffer still holds
// ~1 s (51 periods) so USB clock jitter is absorbed.
func aplayArgs(playbackDevice string) []string {
	return []string{
		"-q", "-D", playbackDevice, "-t", "raw",
		"-f", "S16_LE", "-r", fmt.Sprint(SampleRate), "-c", fmt.Sprint(Channels),
		"--buffer-size=8192", "--period-size=160",
	}
}

// reap kills and waits for a child OUTSIDE a.mu. It relies on the
// process watch when one exists (only one Wait may run per Cmd); without
// a watch it falls back to its own bounded Wait. A stuck child can never
// hold up the next call's Start.
func reap(cmd *exec.Cmd, watch *procWatch) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	if watch != nil {
		watch.wait(waitWatchTimeout())
		return
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-waitTimeout():
	}
}

// waitDeadline is a package-level var so tests can shorten it.
var waitTimeout = func() <-chan time.Time {
	return time.After(3 * time.Second)
}

// waitWatchTimeout bounds how long Stop waits for a watcher to collect
// the exit reason after Kill (also test-shortened via waitReapTimeout).
var waitWatchTimeout = func() time.Duration { return 3 * time.Second }

func (a *ALSAAudio) Start(ctx context.Context, callID modem.CallID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	if a.call != "" && a.call != callID {
		current := a.call
		a.mu.Unlock()
		return fmt.Errorf("ALSA audio is already active for call %s", current)
	}
	if a.call == callID {
		a.mu.Unlock()
		return nil
	}
	// Take ownership, then release the lock before touching children.
	a.call = callID
	capture, playback, rx, tx := a.capture, a.playback, a.rx, a.tx
	captureWatch, playbackWatch := a.captureWatch, a.playbackWatch
	a.capture, a.playback, a.rx, a.tx = nil, nil, nil, nil
	a.captureWatch, a.playbackWatch = nil, nil
	a.mu.Unlock()

	// Kill any stale pair outside the lock.
	reap(capture, captureWatch)
	reap(playback, playbackWatch)
	if rx != nil {
		_ = rx.Close()
	}
	if tx != nil {
		_ = tx.Close()
	}

	captureLog, playbackLog := newBoundedLog(), newBoundedLog()
	newCapture := exec.Command("arecord", arecordArgs(a.captureDevice)...)
	newPlayback := exec.Command("aplay", aplayArgs(a.playbackDevice)...)
	newCapture.Stderr = captureLog
	newPlayback.Stderr = playbackLog
	newRx, err := newCapture.StdoutPipe()
	if err != nil {
		a.releaseCall(callID)
		return fmt.Errorf("create ALSA capture pipe: %w", err)
	}
	newTx, err := newPlayback.StdinPipe()
	if err != nil {
		_ = newRx.Close()
		a.releaseCall(callID)
		return fmt.Errorf("create ALSA playback pipe: %w", err)
	}
	// Critical ordering on the QDC507 UAC gadget: the capture stream must
	// start FIRST. If aplay starts first, the full-duplex ASYNC clock
	// locks onto the playback side and the capture reads silence forever
	// (verified 2026-09-06: manual arecord-only = full-scale VoLTE voice;
	// aplay-first pair = zeros).
	if err := newCapture.Start(); err != nil {
		_ = newTx.Close()
		_ = newRx.Close()
		reap(newPlayback, nil)
		a.releaseCall(callID)
		return fmt.Errorf("start ALSA capture: %w", err)
	}
	if err := newPlayback.Start(); err != nil {
		_ = newRx.Close()
		_ = newTx.Close()
		reap(newCapture, nil)
		a.releaseCall(callID)
		return fmt.Errorf("start ALSA playback: %w", err)
	}
	newCaptureWatch := startProcWatch("arecord", newCapture, captureLog)
	newPlaybackWatch := startProcWatch("aplay", newPlayback, playbackLog)
	a.mu.Lock()
	a.capture, a.playback, a.rx, a.tx = newCapture, newPlayback, newRx, newTx
	a.captureWatch, a.playbackWatch = newCaptureWatch, newPlaybackWatch
	a.mu.Unlock()
	// The bridge playback loop owns the 20 ms silence cadence once the call
	// is up: it writes a frame every slot even when no client RTP arrived.
	// A second silence pacer here (the old primeLoop) double-fed the UAC
	// ring at up to 2x real time, filled the pipe, blocked the bridge's
	// writes and forced queue trims (choppy uplink, 2026-10-05).
	slog.Info("alsa pair started", "call_id", callID, "capture", a.captureDevice, "playback", a.playbackDevice)
	return nil
}

func (a *ALSAAudio) releaseCall(callID modem.CallID) {
	a.mu.Lock()
	if a.call == callID {
		a.call = ""
	}
	a.mu.Unlock()
}

func (a *ALSAAudio) ReadPCM(samples []int16) (int, error) {
	rx, _, callID := a.snapshot()
	if callID == "" || rx == nil {
		return 0, fmt.Errorf("ALSA audio is not active")
	}
	a.mu.Lock()
	watch := a.captureWatch
	a.mu.Unlock()
	buffer := make([]byte, len(samples)*2)
	count, err := io.ReadFull(rx, buffer)
	for index := 0; index+1 < count; index += 2 {
		samples[index/2] = int16(binary.LittleEndian.Uint16(buffer[index : index+2]))
	}
	if err != nil {
		if summary := watch.summary(); summary != "" {
			err = fmt.Errorf("%w: %s", err, summary)
		}
	}
	return count / 2, err
}

func (a *ALSAAudio) WritePCM(samples []int16) (int, error) {
	_, tx, callID := a.snapshot()
	if callID == "" || tx == nil {
		return 0, fmt.Errorf("ALSA audio is not active")
	}
	a.mu.Lock()
	watch := a.playbackWatch
	a.mu.Unlock()
	buffer := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(buffer[index*2:index*2+2], uint16(sample))
	}
	a.txMu.Lock()
	count, err := tx.Write(buffer)
	a.txMu.Unlock()
	if err != nil {
		if summary := watch.summary(); summary != "" {
			err = fmt.Errorf("%w: %s", err, summary)
		}
	}
	return count / 2, err
}

func (a *ALSAAudio) Stop(ctx context.Context) error {
	a.mu.Lock()
	capture, playback, rx, tx := a.capture, a.playback, a.rx, a.tx
	captureWatch, playbackWatch := a.captureWatch, a.playbackWatch
	a.capture, a.playback, a.rx, a.tx = nil, nil, nil, nil
	a.captureWatch, a.playbackWatch = nil, nil
	a.call = ""
	a.mu.Unlock()

	reap(capture, captureWatch)
	reap(playback, playbackWatch)
	if rx != nil {
		_ = rx.Close()
	}
	if tx != nil {
		_ = tx.Close()
	}
	return nil
}

// CurrentOwner reports the call id holding the ALSA pair, or "".
func (a *ALSAAudio) CurrentOwner() modem.CallID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.call
}

// StopIfOwner reaps the ALSA pair only when callID still owns it, so a stale
// teardown cannot cut a newer call's audio.
func (a *ALSAAudio) StopIfOwner(ctx context.Context, callID modem.CallID) bool {
	a.mu.Lock()
	if a.call != callID || a.call == "" {
		a.mu.Unlock()
		return false
	}
	capture, playback, rx, tx := a.capture, a.playback, a.rx, a.tx
	captureWatch, playbackWatch := a.captureWatch, a.playbackWatch
	a.capture, a.playback, a.rx, a.tx = nil, nil, nil, nil
	a.captureWatch, a.playbackWatch = nil, nil
	a.call = ""
	a.mu.Unlock()
	reap(capture, captureWatch)
	reap(playback, playbackWatch)
	if rx != nil {
		_ = rx.Close()
	}
	if tx != nil {
		_ = tx.Close()
	}
	return true
}

func (a *ALSAAudio) Close() error {
	var err error
	a.once.Do(func() {
		err = a.Stop(context.Background())
	})
	return err
}

// boundedLog captures the most recent ALSA-tool stderr output. ALSA
// command-line tools emit only status/error lines on stderr (XRUN, device
// busy, unsupported format) — never audio content — so it is safe to
// surface. Output is sanitized to printable ASCII and capped so a chatty
// tool cannot grow memory without limit; when full, the OLDEST bytes are
// dropped, keeping the tail that explains why the process died.
type boundedLog struct {
	mu      sync.Mutex
	buf     []byte
	cap     int
	maxLine int
}

const (
	stderrLogCapacity = 4096
	stderrTailReport  = 500
)

func newBoundedLog() *boundedLog {
	return &boundedLog{cap: stderrLogCapacity, maxLine: stderrTailReport}
}

func (l *boundedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range p {
		switch {
		case b == '\n' || b == '\t':
			l.buf = append(l.buf, b)
		case b >= 0x20 && b <= 0x7E:
			l.buf = append(l.buf, b)
		default:
			l.buf = append(l.buf, '?')
		}
		if len(l.buf) > l.cap {
			// Drop oldest, then advance to the next newline when
			// possible so reports start on a complete line.
			l.buf = l.buf[len(l.buf)-l.cap:]
			if nl := bytesIndexByte(l.buf, '\n'); nl >= 0 && nl < len(l.buf)-1 {
				l.buf = l.buf[nl+1:]
			}
		}
	}
	return len(p), nil
}

// bytesIndexByte is a tiny indirection to keep imports unchanged.
func bytesIndexByte(s []byte, c byte) int {
	for i, b := range s {
		if b == c {
			return i
		}
	}
	return -1
}

func (l *boundedLog) tail(limit int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.buf
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	// Collapse repeated whitespace so the report stays single-line.
	return strings.TrimSpace(string(out))
}

// procWatch owns the single legal Wait for an ALSA child process and
// records WHY it exited: the kernel exit description plus the sanitized
// stderr tail. It is the evidence that turns an opaque "broken pipe"
// write failure into an observable modem-playback failure reason.
type procWatch struct {
	label  string
	stderr *boundedLog

	done chan struct{}
	mu   sync.Mutex
	info string
}

func startProcWatch(label string, cmd *exec.Cmd, stderr *boundedLog) *procWatch {
	w := &procWatch{label: label, stderr: stderr, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		detail := ""
		if cmd.ProcessState != nil {
			detail = cmd.ProcessState.String()
		}
		if err != nil && cmd.ProcessState == nil {
			detail = err.Error()
		}
		stderrTail := ""
		if stderr != nil {
			stderrTail = stderr.tail(stderrTailReport)
		}
		w.mu.Lock()
		w.info = fmt.Sprintf("%s exited: %s", label, detail)
		if stderrTail != "" {
			w.info += "; stderr=" + stderrTail
		}
		w.mu.Unlock()
		close(w.done)
	}()
	return w
}

func (w *procWatch) wait(timeout time.Duration) {
	if w == nil {
		return
	}
	if timeout <= 0 {
		<-w.done
		return
	}
	select {
	case <-w.done:
	case <-time.After(timeout):
	}
}

func (w *procWatch) summary() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.info
}
