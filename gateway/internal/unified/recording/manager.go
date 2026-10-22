// Package recording owns manual call recording for the unified gateway.
//
// Scope and guarantees:
//   - Manual only: one authenticated start/stop per live 1:1 call. There is
//     no automatic mode and no capture/announcement tone injected into the
//     call: the mixer taps are read-only observers, so starting, stopping or
//     failing a recording never changes call audio or routing.
//   - Canonical audio only: the runtime installs taps on the mixer's
//     post-selection uplink frame and the call's downlink leg frame, so a
//     hot-standby promotion or an ICE<->WS handover keeps recording the SAME
//     call without duplicate standby copies.
//   - Bounded and failure-safe: disk writes happen on per-track goroutines
//     behind a fixed-depth queue; the audio path only ever enqueues with a
//     non-blocking send and drops are treated as a truthful stop, never as
//     backpressure. A write error or queue overflow marks the recording
//     failed, removes its partial tracks and leaves the call untouched.
//   - Storage discipline mirrors voicemail: blobs live under a verified SD
//     mount (device + filesystem checked from /proc/self/mounts) and there is
//     no fallback to the eMMC. Paths are token-validated and symlinks refused.
//   - Crash safety: finalized audio is built in the work directory and
//     renamed into place; startup recovery finalizes segments whose call
//     already ended and fails ones interrupted mid-call.
package recording

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"callrelay/gateway/internal/unified/store"
)

const (
	// SampleRate is the canonical gateway mixer rate (internal/voice 8 kHz,
	// 20 ms frames). Recording at the native rate keeps the capture lossless
	// and makes finalization a pure bounded mixdown.
	SampleRate      = 8000
	Channels        = 1
	BitsPerSample   = 16
	BytesPerSample  = 2
	FrameSamples    = 160 // 20 ms
	WaveformSamples = 320
	// DefaultMinFree mirrors the voicemail discipline: never start a
	// recording when the SD card is nearly full.
	DefaultMinFree    = int64(500 * 1024 * 1024)
	defaultQueueDepth = 75 // 1.5 s per track before the writer is considered stalled
	// maxDroppedFrames = 1: a single dropped frame means the fixed-depth
	// queue overflowed, which would compress that track's timeline relative
	// to the other direction. The recording is failed immediately (and its
	// partial tracks discarded) instead of producing misaligned audio.
	maxDroppedFrames = 1
	// terminalCallFence bounds how many ended calls keep a "no recording can
	// start" fence; call ids are never reused for a new call.
	terminalCallFence = 2048
	// storageCheckFrames bounds a long call's unmonitored growth: every 500
	// written frames (~10 s) the writer re-checks the free-space reserve.
	storageCheckFrames = 500
)

var (
	ErrNotActive          = errors.New("recording is not active")
	ErrRecordingDisabled  = errors.New("recording is disabled")
	ErrLowStorage         = errors.New("recording storage is below the minimum free-space threshold")
	ErrCapacityReached    = errors.New("recording storage capacity is reached")
	ErrStorageUnavailable = errors.New("recording storage is unavailable")
	ErrFinalizing         = errors.New("call recording is still finalizing")
	ErrActiveRecording    = errors.New("recording is still in progress")
	ErrStarting           = errors.New("a recording start for this call is already in progress")
	ErrCallEnded          = errors.New("the call has ended; recording is no longer possible")
	ErrInConference       = errors.New("the call belongs to a conference; recording is controlled by the conference")
)

// Options is the storage/policy configuration owned by the runtime config
// (recording.* in unified.yaml).
type Options struct {
	Root             string
	MountPoint       string
	ExpectedDevice   string
	ExpectedFSType   string
	RetentionDays    int
	MinimumFreeBytes int64
	MaxBytes         int64
}

// CallMeta is the call context captured at start time. Kind selects the
// segment shape: "call" (default) is a manual 1:1 segment keyed by the call
// id; "conference" is a manual conference segment keyed by the conference id
// (ID is the conference id in that case).
type CallMeta struct {
	ID        string
	Kind      string
	LineID    string
	Direction string
	Peer      string
	StartedAt time.Time
	// RetentionDays overrides the gateway default for this segment when > 0
	// (per-line retention). TriggerMode is "manual" unless the segment was
	// started automatically by the device/gateway auto preference.
	RetentionDays int
	TriggerMode   string
}

// Store is the durable surface the manager needs. *store.Store implements it;
// tests may substitute a lighter implementation.
type Store interface {
	SaveRecording(ctx context.Context, rec store.Recording) error
	GetRecording(ctx context.Context, id string) (store.Recording, error)
	GetLatestRecordingByCall(ctx context.Context, callID string) (store.Recording, error)
	ListRecordings(ctx context.Context, lineID string, limit int) ([]store.Recording, error)
	ListRecordingsByState(ctx context.Context, state string, limit int) ([]store.Recording, error)
	ListExpiredRecordings(ctx context.Context, now time.Time, limit int) ([]store.Recording, error)
	RecordingUsageBytes(ctx context.Context) (int64, error)
	HardDeleteRecordingRow(ctx context.Context, id string) error
	UpdateCallRecording(ctx context.Context, callID, recordingID, state string, durationMs *int64) error
	GetCall(ctx context.Context, id string) (store.Call, error)
	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

// Encoder turns the two raw work tracks into the user-facing container.
type Encoder interface {
	Encode(ctx context.Context, uplink, downlink, output string) ([]float32, error)
}

type Manager struct {
	store   Store
	opts    Options
	encoder Encoder
	now     func() time.Time
	// queueDepth/maxDropped are overridable by tests.
	queueDepth int
	maxDropped uint64

	// mountSourceLookup and freeBytes are injectable so tests can exercise
	// the mount/free-space failure paths without a real SD card. Production
	// parses /proc/self/mounts and statfs.
	mountSourceLookup func(path string) (mountPoint, source, fstype string, ok bool)
	freeBytes         func(path string) (int64, error)
	// testWriterWrite is a test-only injection point substituting the disk
	// write of a track writer (nil uses the real file). It exists so the
	// bounded-queue, write-failure and drained-duration paths can be driven
	// deterministically.
	testWriterWrite func(file *os.File, buffer []byte) error

	mu       sync.Mutex
	sessions map[string]*session
	latest   map[string]store.Recording
	onEvent  func(kind string, rec store.Recording)
	closed   bool
	// reserving guards a call id while Start performs its IO outside the
	// lock, so two concurrent starts (or an end racing a start) can never
	// create two sessions or orphan a session for an ended call.
	reserving map[string]bool
	// terminal holds call ids whose call ended (or merged into a conference):
	// no new segment may ever be created for them. Bounded ring.
	terminal      map[string]bool
	terminalOrder []string
	// blocked holds call ids that currently belong to a conference: a 1:1
	// segment must not attach to them (recording is conference-scoped until
	// the call is split/converged back). UnblockCall clears the entry.
	blocked      map[string]uint64
	blockedOrder []blockEntry
	blockedSeq   uint64

	wg sync.WaitGroup
}

// blockEntry pins one BlockCall occurrence so trimming the bounded ring can
// never drop a NEWER block for the same call id (re-block after unblock).
type blockEntry struct {
	id  string
	seq uint64
}

func NewManager(st Store, opts Options, encoder Encoder) (*Manager, error) {
	if st == nil {
		return nil, fmt.Errorf("recording store is required")
	}
	if strings.TrimSpace(opts.Root) == "" {
		return nil, fmt.Errorf("recording root is required")
	}
	if opts.RetentionDays < 0 || opts.MinimumFreeBytes < 0 || opts.MaxBytes < 0 {
		return nil, fmt.Errorf("recording options cannot be negative")
	}
	if encoder == nil {
		encoder = WAVEncoder{}
	}
	return &Manager{
		store: st, opts: opts, encoder: encoder, now: time.Now,
		queueDepth: defaultQueueDepth, maxDropped: maxDroppedFrames,
		mountSourceLookup: defaultMountSourceLookup, freeBytes: diskFreeBytes,
		sessions: make(map[string]*session), latest: make(map[string]store.Recording),
		reserving: make(map[string]bool), terminal: make(map[string]bool),
		blocked: make(map[string]uint64),
	}, nil
}

func (m *Manager) SetEventHandler(handler func(kind string, rec store.Recording)) {
	m.mu.Lock()
	m.onEvent = handler
	m.mu.Unlock()
}

// SetStorageProbes overrides the mount/free-space probes. It exists for
// tests and for deployments that must exercise the storage logic without the
// real SD mount; nil restores the production probes.
func (m *Manager) SetStorageProbes(
	lookup func(path string) (mountPoint, source, fstype string, ok bool),
	free func(path string) (int64, error),
) {
	m.mu.Lock()
	m.mountSourceLookup = lookup
	m.freeBytes = free
	m.mu.Unlock()
}

// Enabled reports the global manual-recording switch. Unset means enabled;
// starting is always an explicit user action, never automatic.
func (m *Manager) Enabled(ctx context.Context) (bool, error) {
	value, err := m.store.Setting(ctx, "recording_enabled")
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(value) != "0", nil
}

func (m *Manager) SetEnabled(ctx context.Context, enabled bool) error {
	value := "1"
	if !enabled {
		value = "0"
	}
	return m.store.SetSetting(ctx, "recording_enabled", value)
}

// Active reports whether a recording session is writing for this call.
func (m *Manager) Active(callID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[callID] != nil
}

// CallRecording returns the current (or latest known) recording for a call.
func (m *Manager) CallRecording(ctx context.Context, callID string) (store.Recording, bool) {
	m.mu.Lock()
	if sess := m.sessions[callID]; sess != nil {
		rec := sess.snapshot()
		m.mu.Unlock()
		return rec, true
	}
	if rec, ok := m.latest[callID]; ok {
		m.mu.Unlock()
		return rec, true
	}
	m.mu.Unlock()
	rec, err := m.store.GetLatestRecordingByCall(ctx, callID)
	if err != nil {
		return store.Recording{}, false
	}
	m.mu.Lock()
	m.latest[callID] = rec
	m.mu.Unlock()
	return rec, true
}

func (m *Manager) remember(rec store.Recording) {
	m.mu.Lock()
	if rec.State == "deleted" {
		delete(m.latest, rec.CallID)
	} else {
		m.latest[rec.CallID] = rec
	}
	m.mu.Unlock()
}

func (m *Manager) publish(kind string, rec store.Recording) {
	m.mu.Lock()
	handler := m.onEvent
	m.mu.Unlock()
	if handler != nil {
		handler(kind, rec)
	}
}

// Start opens a manual recording segment for a live call. Repeated starts for
// the same call return the existing segment (idempotent). deviceID records
// the acting device; keyID is the enrollment-key scope that owns the
// recording (a different key sharing the line must never read or stream it).
func (m *Manager) Start(ctx context.Context, call CallMeta, deviceID, keyID string) (store.Recording, error) {
	if strings.TrimSpace(call.ID) == "" {
		return store.Recording{}, fmt.Errorf("call id is required")
	}
	if err := ctx.Err(); err != nil {
		return store.Recording{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return store.Recording{}, ErrRecordingDisabled
	}
	if sess := m.sessions[call.ID]; sess != nil {
		rec := sess.snapshot()
		m.mu.Unlock()
		return rec, nil
	}
	if m.terminal[call.ID] {
		m.mu.Unlock()
		return store.Recording{}, ErrCallEnded
	}
	if _, blocked := m.blocked[call.ID]; blocked {
		m.mu.Unlock()
		return store.Recording{}, ErrInConference
	}
	if m.reserving[call.ID] {
		m.mu.Unlock()
		return store.Recording{}, ErrStarting
	}
	// Reserve the call id for the duration of the IO below. Without this
	// reservation two concurrent starts would create two sessions (one
	// orphaned with open files), and an end racing a start could leave a
	// session capturing an already-ended call.
	m.reserving[call.ID] = true
	m.mu.Unlock()
	committed := false
	defer func() {
		if !committed {
			m.mu.Lock()
			delete(m.reserving, call.ID)
			m.mu.Unlock()
		}
	}()
	if existing, err := m.store.GetLatestRecordingByCall(ctx, call.ID); err == nil {
		if existing.State == "recording" || existing.State == "starting" {
			committed = true
			m.mu.Lock()
			delete(m.reserving, call.ID)
			m.mu.Unlock()
			return existing, nil
		}
		if existing.State == "finalizing" {
			return store.Recording{}, ErrFinalizing
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Recording{}, err
	}
	enabled, err := m.Enabled(ctx)
	if err != nil {
		return store.Recording{}, err
	}
	if !enabled {
		return store.Recording{}, ErrRecordingDisabled
	}
	root, err := m.audioRoot()
	if err != nil {
		return store.Recording{}, fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}
	budgetCheck := func() error {
		free, freeErr := m.freeBytes(root)
		if freeErr != nil {
			return freeErr
		}
		if minimum := m.opts.MinimumFreeBytes; minimum > 0 && free < minimum {
			return fmt.Errorf("recording storage fell below the minimum reserve (%d bytes free)", free)
		}
		return nil
	}
	if err := budgetCheck(); err != nil {
		if strings.Contains(err.Error(), "minimum reserve") {
			return store.Recording{}, ErrLowStorage
		}
		return store.Recording{}, fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}
	if m.opts.MaxBytes > 0 {
		if used, usedErr := m.store.RecordingUsageBytes(ctx); usedErr == nil && used >= m.opts.MaxBytes {
			return store.Recording{}, ErrCapacityReached
		}
	}
	if !pathToken.MatchString(call.LineID) {
		return store.Recording{}, fmt.Errorf("unsafe recording line component")
	}
	id := randomRecordingID()
	lineDir := filepath.Join(root, call.LineID)
	workRoot := filepath.Join(root, "work")
	finalPath := filepath.Join(lineDir, id+".wav")
	workDir := filepath.Join(workRoot, id)
	if err := ensureSafeDir(root, lineDir); err != nil {
		return store.Recording{}, fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}
	if err := ensureSafeDir(root, workRoot); err != nil {
		return store.Recording{}, fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}
	if err := ensureSafeDir(workRoot, workDir); err != nil {
		return store.Recording{}, fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}
	// The failure callback runs on EITHER the writer goroutine (disk error)
	// or the mixer goroutine (queue overflow). It latches the failure
	// synchronously — cheap, bounded — and performs the stop asynchronously,
	// so a realtime audio thread can never block on disk cleanup.
	sess := &session{workDir: workDir}
	failWriter := func(code string, writerErr error) {
		sess.markFailure(code, writerErr)
		go m.abort(call.ID, code, writerErr)
	}
	uplink, err := newTrackWriter(filepath.Join(workDir, "uplink.pcm.part"), m.queueDepth, m.maxDropped, failWriter, m.testWriterWrite, budgetCheck)
	if err != nil {
		_ = os.RemoveAll(workDir)
		return store.Recording{}, fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}
	downlink, err := newTrackWriter(filepath.Join(workDir, "downlink.pcm.part"), m.queueDepth, m.maxDropped, failWriter, m.testWriterWrite, budgetCheck)
	if err != nil {
		uplink.close()
		_ = os.RemoveAll(workDir)
		return store.Recording{}, fmt.Errorf("%w: %v", ErrStorageUnavailable, err)
	}
	sess.uplink, sess.downlink = uplink, downlink
	created := m.now()
	started := call.StartedAt
	if started.IsZero() {
		started = created
	}
	kind := strings.TrimSpace(call.Kind)
	if kind == "" {
		kind = "call"
	}
	conferenceID := ""
	if kind == "conference" {
		conferenceID = call.ID
	}
	triggerMode := strings.TrimSpace(call.TriggerMode)
	if triggerMode == "" {
		triggerMode = "manual"
	}
	rec := store.Recording{
		ID: id, LineID: call.LineID, CallID: call.ID, Kind: kind, ConferenceID: conferenceID,
		DeviceID: deviceID, KeyID: keyID,
		Direction: call.Direction, Peer: call.Peer, State: "recording", TriggerMode: triggerMode,
		StartedAt: &started, FilePath: finalPath, Container: "wav", Codec: "pcm_s16le",
		SampleRate: SampleRate, Channels: Channels, Bitrate: SampleRate * BitsPerSample,
		CreatedByDevice: deviceID, CreatedAt: created, UpdatedAt: created,
	}
	retentionDays := m.opts.RetentionDays
	if call.RetentionDays > 0 {
		retentionDays = call.RetentionDays
	}
	if expiry := retentionDeadline(created, retentionDays); expiry != nil {
		rec.RetentionExpires = expiry
	}
	if err := m.store.SaveRecording(ctx, rec); err != nil {
		uplink.close()
		downlink.close()
		_ = os.RemoveAll(workDir)
		return store.Recording{}, err
	}
	if err := m.store.UpdateCallRecording(ctx, call.ID, id, rec.State, nil); err != nil {
		slog.Warn("recording call pointer update failed", "call_id", call.ID, "error", err)
	}
	sess.rec = rec
	m.mu.Lock()
	delete(m.reserving, call.ID)
	if m.closed || m.terminal[call.ID] || m.blocked[call.ID] != 0 {
		reason := "gateway_shutdown"
		failErr := ErrCallEnded
		detail := "call ended before the recording attached"
		if m.terminal[call.ID] {
			reason = "call_ended"
		}
		if _, blocked := m.blocked[call.ID]; blocked {
			// The call entered a conference while this start was doing its
			// IO: the segment must never attach to a conference member.
			reason = "conference_media"
			detail = "call joined a conference before the recording attached"
			failErr = ErrInConference
		}
		m.mu.Unlock()
		uplink.close()
		downlink.close()
		// The call ended, entered a conference, or the manager shut down
		// while this start was still doing IO: never register a capture for
		// it. Persist a truthful failure so the row cannot masquerade as an
		// active one.
		m.fail(rec, reason, errors.New(detail), workDir)
		return store.Recording{}, failErr
	}
	m.sessions[call.ID] = sess
	m.latest[call.ID] = rec
	committed = true
	m.mu.Unlock()
	m.publish("started", rec)
	return rec, nil
}

// WriteUplink offers one canonical uplink (user -> line) frame. It never
// blocks; a nil frame is recorded as 20 ms of silence so the two tracks stay
// time-aligned across microphone stalls.
func (m *Manager) WriteUplink(callID string, pcm []int16) {
	m.write(callID, pcm, true)
}

// WriteDownlink offers one canonical downlink (line -> user) frame.
func (m *Manager) WriteDownlink(callID string, pcm []int16) {
	m.write(callID, pcm, false)
}

func (m *Manager) write(callID string, pcm []int16, uplink bool) {
	m.mu.Lock()
	sess := m.sessions[callID]
	m.mu.Unlock()
	if sess == nil {
		return
	}
	if pcm == nil {
		pcm = silenceFrame
	}
	if len(pcm) == 0 {
		return
	}
	writer := sess.downlink
	if uplink {
		writer = sess.uplink
	}
	if writer != nil {
		writer.enqueue(pcm)
	}
}

// Stop ends the active segment for a call and finalizes it in the background.
// It is idempotent from the caller's perspective: a call without an active
// segment reports ErrNotActive. It never waits on the SD card: writer flush
// happens inside the finalize goroutine.
func (m *Manager) Stop(ctx context.Context, callID, reason string) error {
	sess := m.takeSession(callID)
	if sess == nil {
		return ErrNotActive
	}
	m.stopSession(sess, reason)
	return nil
}

// EndCall fences a call as terminal and finalizes any active segment. After
// EndCall no new segment can ever be created for the call id, which closes
// the start-vs-hangup race at the recording layer.
func (m *Manager) EndCall(callID, reason string) {
	m.fenceTerminal(callID)
	sess := m.takeSession(callID)
	if sess == nil {
		return
	}
	m.stopSession(sess, reason)
}

// AbortForCall stops and FAILS any active segment. Used when a call merges
// into a conference (conference recording is unsupported): truthful failure,
// never a silently incomplete "ready".
func (m *Manager) AbortForCall(callID, code, reason string) {
	m.fenceTerminal(callID)
	sess := m.takeSession(callID)
	if sess == nil {
		return
	}
	sess.markFailure(code, errors.New(reason))
	m.stopSession(sess, reason)
}

// StopForConference finalizes an active 1:1 segment as COMPLETED because the
// call is entering a conference, and blocks the call id from starting a new
// 1:1 segment until UnblockCall. The captured audio stays playable: a merge
// must never silently fail or delete a segment the user explicitly started.
// The conference itself can then be recorded manually under the conference id.
func (m *Manager) StopForConference(callID, reason string) {
	if m == nil {
		return
	}
	m.BlockCall(callID)
	sess := m.takeSession(callID)
	if sess == nil {
		return
	}
	m.stopSession(sess, reason)
}

// BlockCall fences a call id as conference-owned. CallBlocked reports whether
// the fence is currently set; UnblockCall clears it when the call becomes a
// standalone 1:1 call again (split, last-leg convergence).
func (m *Manager) BlockCall(callID string) {
	if callID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if _, exists := m.blocked[callID]; exists {
		return
	}
	m.blockedSeq++
	m.blocked[callID] = m.blockedSeq
	m.blockedOrder = append(m.blockedOrder, blockEntry{id: callID, seq: m.blockedSeq})
	for len(m.blockedOrder) > terminalCallFence {
		oldest := m.blockedOrder[0]
		m.blockedOrder = m.blockedOrder[1:]
		if seq, ok := m.blocked[oldest.id]; ok && seq == oldest.seq {
			delete(m.blocked, oldest.id)
		}
	}
}

// UnblockCall clears the conference fence for a call id (idempotent).
func (m *Manager) UnblockCall(callID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blocked, callID)
}

// CallBlocked reports whether a call id is currently conference-fenced.
func (m *Manager) CallBlocked(callID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, blocked := m.blocked[callID]
	return blocked
}

func (m *Manager) fenceTerminal(callID string) {
	m.mu.Lock()
	if !m.terminal[callID] {
		m.terminal[callID] = true
		m.terminalOrder = append(m.terminalOrder, callID)
		for len(m.terminalOrder) > terminalCallFence {
			oldest := m.terminalOrder[0]
			m.terminalOrder = m.terminalOrder[1:]
			delete(m.terminal, oldest)
		}
	}
	m.mu.Unlock()
}

func (m *Manager) takeSession(callID string) *session {
	m.mu.Lock()
	sess := m.sessions[callID]
	delete(m.sessions, callID)
	m.mu.Unlock()
	return sess
}

func (m *Manager) abort(callID, code string, writerErr error) {
	sess := m.takeSession(callID)
	if sess == nil {
		return
	}
	sess.markFailure(code, writerErr)
	m.stopSession(sess, code)
}

// stopSession performs only in-memory bookkeeping on the caller's thread;
// closing/flushing writers and encoding the final container happen in a
// background goroutine so a blocked SD card can never stall call teardown or
// an API request. If the writer already latched a failure the segment fails
// after the writers are drained and the partial tracks are discarded.
func (m *Manager) stopSession(sess *session, reason string) {
	now := m.now()
	rec, failureCode, failureDetail := sess.prepareStop(now)
	if failureCode != "" {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			sess.shutdown()
			m.fail(rec, failureCode, errors.New(failureDetail), sess.workDir)
		}()
		return
	}
	rec.State = "finalizing"
	rec.UpdatedAt = now
	ctx := context.Background()
	if err := m.store.SaveRecording(ctx, rec); err != nil {
		slog.Error("recording finalize persist failed", "recording_id", rec.ID, "error", err)
	}
	_ = m.store.UpdateCallRecording(ctx, rec.CallID, rec.ID, rec.State, rec.DurationMs)
	m.remember(rec)
	m.publish("finalizing", rec)
	m.wg.Add(1)
	go m.finalize(rec, sess, reason)
}

// finalize flushes the work tracks, re-checks for a failure latched during
// the drain, mixes the canonical audio into the final WAV, records
// size/sha256 and publishes the terminal state. sess is nil for startup
// recovery.
func (m *Manager) finalize(rec store.Recording, sess *session, reason string) {
	defer m.wg.Done()
	workDir := filepath.Join(m.rootPath(), "work", rec.ID)
	if sess != nil {
		sess.shutdown()
		workDir = sess.workDir
		// Duration truth comes from the DRAINED writers: prepareStop ran
		// while queued frames were still unpersisted.
		duration := sess.durationLocked()
		rec.DurationMs = &duration
		if failure, detail := sess.failure(); failure != "" {
			m.fail(rec, failure, errors.New(detail), sess.workDir)
			return
		}
	}
	// Revalidate the storage at finalize time: the card may have been
	// unmounted after the recording started. Without this check the mkdir /
	// mixdown / rename below would silently create files on the eMMC
	// fallback path (or through a swapped filesystem layout). The path is
	// re-resolved through the same safePath used for serving.
	if _, err := m.audioRoot(); err != nil {
		m.fail(rec, "storage_unavailable", err, workDir)
		return
	}
	finalPath, err := m.safePath(rec.LineID, rec.ID, rec.FilePath)
	if err != nil {
		m.fail(rec, "unsafe_path", err, workDir)
		return
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o750); err != nil {
		m.fail(rec, "storage_error", err, workDir)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	uplinkPath := filepath.Join(workDir, "uplink.pcm.part")
	downlinkPath := filepath.Join(workDir, "downlink.pcm.part")
	temporary := finalPath + ".part.wav"
	_ = os.Remove(temporary)
	peaks, err := m.encoder.Encode(ctx, uplinkPath, downlinkPath, temporary)
	if err != nil {
		_ = os.Remove(temporary)
		m.fail(rec, "encoder_failed", err, workDir)
		return
	}
	waveformPath := finalPath + ".waveform.json"
	if err := writeWaveformDocument(waveformPath, peaks); err != nil {
		_ = os.Remove(temporary)
		m.fail(rec, "waveform_failed", err, workDir)
		return
	}
	if err := os.Rename(temporary, finalPath); err != nil {
		_ = os.Remove(waveformPath)
		m.fail(rec, "storage_error", err, workDir)
		return
	}
	size, err := fileSize(finalPath)
	if err != nil {
		m.fail(rec, "storage_error", err, workDir)
		return
	}
	hash, err := fileSHA256(finalPath)
	if err != nil {
		m.fail(rec, "storage_error", err, workDir)
		return
	}
	// A delete that raced finalization wins: never resurrect a tombstoned
	// recording with a late "ready" write.
	if current, getErr := m.store.GetRecording(context.Background(), rec.ID); getErr == nil && current.State == "deleted" {
		_ = os.Remove(finalPath)
		_ = os.Remove(waveformPath)
		_ = os.RemoveAll(workDir)
		return
	}
	now := m.now()
	duration := int64((size - wavHeaderSize) / BytesPerSample * 1000 / SampleRate)
	if size <= wavHeaderSize {
		duration = 0
	}
	rec.State = "ready"
	rec.FilePath = finalPath
	rec.SizeBytes = &size
	rec.SHA256 = hash
	rec.WaveformPath = waveformPath
	rec.DurationMs = &duration
	if rec.StoppedAt == nil {
		rec.StoppedAt = &now
	}
	rec.UpdatedAt = now
	if err := m.store.SaveRecording(context.Background(), rec); err != nil {
		m.fail(rec, "database_error", err, workDir)
		return
	}
	_ = m.store.UpdateCallRecording(context.Background(), rec.CallID, rec.ID, rec.State, rec.DurationMs)
	m.remember(rec)
	cleanupWorkDir(workDir)
	_ = reason // retained for future audit detail without changing the stable API
	m.publish("ready", rec)
}

// fail persists a truthful terminal failure and reclaims the unusable work
// data. The call itself is never touched.
func (m *Manager) fail(rec store.Recording, code string, failureErr error, workDir string) {
	now := m.now()
	rec.State = "failed"
	rec.FailureCode = code
	if failureErr != nil {
		rec.FailureDetail = failureErr.Error()
	}
	if rec.StoppedAt == nil {
		rec.StoppedAt = &now
	}
	if rec.UpdatedAt.Before(now) {
		rec.UpdatedAt = now
	}
	slog.Error("call recording failed", "recording_id", rec.ID, "call_id", rec.CallID, "failure_code", code, "error", failureErr)
	ctx := context.Background()
	if err := m.store.SaveRecording(ctx, rec); err != nil {
		slog.Error("recording failure persist failed", "recording_id", rec.ID, "error", err)
	}
	_ = m.store.UpdateCallRecording(ctx, rec.CallID, rec.ID, rec.State, rec.DurationMs)
	m.remember(rec)
	cleanupWorkDir(workDir)
	m.publish("failed", rec)
}

func (m *Manager) Get(ctx context.Context, id string) (store.Recording, error) {
	return m.store.GetRecording(ctx, id)
}

func (m *Manager) List(ctx context.Context, lineID string, limit int) ([]store.Recording, error) {
	return m.store.ListRecordings(ctx, lineID, limit)
}

// AudioPath resolves a ready recording blob for serving. The path must stay
// inside the configured root, use safe tokens and not be a symlink.
func (m *Manager) AudioPath(ctx context.Context, id string) (string, error) {
	rec, err := m.store.GetRecording(ctx, id)
	if err != nil {
		return "", err
	}
	if rec.State != "ready" || rec.FilePath == "" {
		return "", os.ErrNotExist
	}
	return m.safePath(rec.LineID, rec.ID, rec.FilePath)
}

// WaveformPath resolves the waveform JSON for a ready recording.
func (m *Manager) WaveformPath(ctx context.Context, id string) (string, error) {
	rec, err := m.store.GetRecording(ctx, id)
	if err != nil {
		return "", err
	}
	if rec.State != "ready" || rec.WaveformPath == "" {
		return "", os.ErrNotExist
	}
	return m.safePath(rec.LineID, rec.ID, rec.WaveformPath)
}

// safePath validates that value is inside the recording root, that its
// relative components use safe tokens and that NO component on the way
// (including the line directory and the work directory) is a symlink. A
// missing tail is allowed: the caller decides how to report a gone blob.
func (m *Manager) safePath(lineID, id, value string) (string, error) {
	if !pathToken.MatchString(lineID) || !pathToken.MatchString(id) {
		return "", fmt.Errorf("unsafe recording path component")
	}
	root := filepath.Clean(m.rootPath())
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("recording path is outside recording root")
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			break
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("recording path component %s must not be a symlink", filepath.Base(current))
		}
	}
	return absolute, nil
}

// ensureSafeDir creates target (which must stay under root) component by
// component, rejecting a symlinked or non-directory component on the way: a
// hostile filesystem layout must never redirect recording IO outside the
// verified mount.
func ensureSafeDir(root, target string) error {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("recording directory escapes root")
	}
	parts := []string{}
	if relative != "." {
		parts = strings.Split(relative, string(filepath.Separator))
	}
	current := root
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			if mkErr := os.Mkdir(current, 0o750); mkErr != nil && !os.IsExist(mkErr) {
				return mkErr
			}
			if info, statErr = os.Lstat(current); statErr != nil {
				return statErr
			}
		} else if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("recording path component %s must not be a symlink", filepath.Base(current))
		}
		if index == len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("recording path %s is not a directory", filepath.Base(current))
		}
	}
	return nil
}

// Delete tombstones a finished recording and reclaims its blobs. An active
// segment must be stopped first: deleting live audio would silently truncate
// it, so the caller receives ErrActiveRecording instead. The row is only
// removed once the verified SD mount is present and every blob was purged
// through safePath, so a stored path can never delete something outside the
// recording root and an absent card never forgets (and later orphans) blobs.
func (m *Manager) Delete(ctx context.Context, id string) (store.Recording, error) {
	rec, err := m.store.GetRecording(ctx, id)
	if err != nil {
		return store.Recording{}, err
	}
	if rec.State == "recording" || rec.State == "starting" || rec.State == "finalizing" {
		return rec, ErrActiveRecording
	}
	if rec.State == "deleted" {
		return rec, nil
	}
	now := m.now()
	rec.State = "deleted"
	rec.UpdatedAt = now
	if err := m.store.SaveRecording(ctx, rec); err != nil {
		return store.Recording{}, err
	}
	// Clear the call's current-recording pointer: a deleted recording must
	// not keep showing up on the call.
	_ = m.store.UpdateCallRecording(ctx, rec.CallID, "", "", nil)
	m.remember(rec)
	m.publish("deleted", rec)
	if purgeErr := m.purgeRecording(rec); purgeErr == nil {
		_ = m.store.HardDeleteRecordingRow(ctx, id)
	}
	return rec, nil
}

// RunRetention prunes expired READY recordings and completes blob reclamation
// for tombstones whose SD card was unavailable at delete time.
func (m *Manager) RunRetention(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		m.prune(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) prune(ctx context.Context) {
	// Without the verified SD mount every stored path would resolve as
	// ENOENT; hard-deleting rows then would orphan the blobs forever once
	// the card is remounted. Retention is skipped until the mount returns.
	if _, err := m.audioRoot(); err != nil {
		return
	}
	expired, err := m.store.ListExpiredRecordings(ctx, m.now(), 200)
	if err == nil {
		for _, rec := range expired {
			if purgeErr := m.purgeRecording(rec); purgeErr == nil {
				_ = m.store.HardDeleteRecordingRow(ctx, rec.ID)
			}
		}
	}
	deleted, err := m.store.ListRecordingsByState(ctx, "deleted", 200)
	if err == nil {
		for _, rec := range deleted {
			if purgeErr := m.purgeRecording(rec); purgeErr == nil {
				_ = m.store.HardDeleteRecordingRow(ctx, rec.ID)
			}
		}
	}
}

// purgeRecording removes a recording's blobs ONLY through the verified mount
// and the same token/ancestor validation used for serving. A stored path that
// escapes the root or crosses a symlink is refused, keeping the tombstone for
// manual inspection instead of deleting an arbitrary file.
func (m *Manager) purgeRecording(rec store.Recording) error {
	if _, err := m.audioRoot(); err != nil {
		return err
	}
	for _, candidate := range []string{rec.FilePath, rec.WaveformPath} {
		if candidate == "" {
			continue
		}
		path, err := m.safePath(rec.LineID, rec.ID, candidate)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Recover reconciles rows/work directories left by a restart. A segment whose
// call already ended is finalized from its durable tracks; a segment that was
// still live is marked failed because its media session is gone.
func (m *Manager) Recover(ctx context.Context) error {
	root := m.rootPath()
	if _, err := m.audioRoot(); err != nil {
		slog.Warn("recording recovery skipped: SD unavailable", "error", err)
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(root, "work"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if _, getErr := m.store.GetRecording(ctx, id); getErr == nil {
			continue
		}
		info, statErr := entry.Info()
		if statErr == nil && m.now().Sub(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(root, "work", id))
		}
	}
	// Process each interrupted row exactly once: a row moved to
	// 'finalizing' by this loop must not be listed again by a later state
	// pass and finalized twice (two concurrent finalizers race on the same
	// work directory and one clobbers the other's terminal state).
	seen := map[string]bool{}
	for _, state := range []string{"recording", "starting", "finalizing"} {
		rows, listErr := m.store.ListRecordingsByState(ctx, state, 500)
		if listErr != nil {
			return listErr
		}
		for _, rec := range rows {
			if seen[rec.ID] {
				continue
			}
			seen[rec.ID] = true
			if !pathToken.MatchString(rec.ID) || !pathToken.MatchString(rec.LineID) {
				m.fail(rec, "unsafe_id", errors.New("recording row has an unsafe path component"), "")
				continue
			}
			call, callErr := m.store.GetCall(ctx, rec.CallID)
			ended := callErr == nil && call.EndedAt != nil
			workDir := filepath.Join(root, "work", rec.ID)
			if _, dirErr := os.Stat(workDir); os.IsNotExist(dirErr) {
				if ended {
					m.fail(rec, "missing_data", errors.New("recording work files are missing"), workDir)
				} else {
					m.fail(rec, "gateway_restarted", errors.New("recording media session was interrupted by Gateway restart"), workDir)
				}
				continue
			}
			if !ended {
				m.fail(rec, "gateway_restarted", errors.New("recording media session was interrupted by Gateway restart"), workDir)
				continue
			}
			if rec.StoppedAt == nil && call.EndedAt != nil {
				value := *call.EndedAt
				rec.StoppedAt = &value
			}
			rec.State = "finalizing"
			rec.UpdatedAt = m.now()
			_ = m.store.SaveRecording(ctx, rec)
			m.wg.Add(1)
			go m.finalize(rec, nil, "recovery")
		}
	}
	return nil
}

// CloseAll flushes every active segment and waits (bounded) for finalization.
// A process that exits mid-finalize leaves its work directory for the next
// startup Recover.
func (m *Manager) CloseAll(reason string) {
	m.mu.Lock()
	m.closed = true
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		if err := m.Stop(context.Background(), id, reason); err != nil && !errors.Is(err, ErrNotActive) {
			slog.Warn("recording shutdown stop failed", "call_id", id, "error", err)
		}
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		slog.Warn("recording finalization did not finish before shutdown")
	}
}

// StorageStatus reports the verified SD state for diagnostics/API.
type StorageStatus struct {
	Mounted   bool   `json:"mounted"`
	Device    string `json:"device,omitempty"`
	FSType    string `json:"fstype,omitempty"`
	Root      string `json:"root,omitempty"`
	FreeBytes int64  `json:"freeBytes,omitempty"`
	UsedBytes int64  `json:"usedBytes,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (m *Manager) StorageStatus(ctx context.Context) StorageStatus {
	status := StorageStatus{Root: m.rootPath()}
	root, err := m.audioRoot()
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Mounted = true
	_, source, fstype, _ := m.mountSourceLookup(root)
	status.Device, status.FSType = source, fstype
	if free, freeErr := m.freeBytes(root); freeErr == nil {
		status.FreeBytes = free
	}
	if used, usedErr := m.store.RecordingUsageBytes(ctx); usedErr == nil {
		status.UsedBytes = used
	}
	return status
}

func (m *Manager) rootPath() string {
	return filepath.Clean(strings.TrimSpace(m.opts.Root))
}

// audioRoot verifies the configured SD mount and device before any recording.
// It never falls back to the eMMC or the underlying root directory: when the
// card is absent or mismatched, recording is unavailable while ordinary
// telephony stays healthy.
func (m *Manager) audioRoot() (string, error) {
	root := m.rootPath()
	mountPoint := filepath.Clean(strings.TrimSpace(m.opts.MountPoint))
	if root == "." || mountPoint == "." {
		return "", errors.New("recording audio root is not configured")
	}
	if !strings.HasPrefix(root, mountPoint+string(filepath.Separator)) {
		return "", errors.New("recording audio root escapes mount point")
	}
	foundMount, source, fstype, ok := m.mountSourceLookup(root)
	if !ok || filepath.Clean(foundMount) != mountPoint {
		return "", fmt.Errorf("recording SD mount %s is not present", mountPoint)
	}
	if expected := strings.TrimSpace(m.opts.ExpectedDevice); expected != "" && !strings.HasPrefix(source, expected) {
		return "", fmt.Errorf("recording mount device %s does not match expected %s", source, expected)
	}
	if expected := strings.TrimSpace(m.opts.ExpectedFSType); expected != "" && !strings.EqualFold(fstype, expected) {
		return "", fmt.Errorf("recording mount filesystem %s does not match expected %s", fstype, expected)
	}
	info, err := os.Lstat(root)
	switch {
	case os.IsNotExist(err):
		if mkErr := os.MkdirAll(root, 0o700); mkErr != nil {
			return "", mkErr
		}
		info, err = os.Lstat(root)
		if err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("recording audio root must not be a symlink")
	}
	if !info.IsDir() {
		return "", errors.New("recording audio root is not a directory")
	}
	return root, nil
}

type session struct {
	mu       sync.Mutex
	rec      store.Recording
	workDir  string
	uplink   *trackWriter
	downlink *trackWriter
	stopping bool

	failureCode   string
	failureDetail string
}

func (s *session) markFailure(code string, err error) {
	if code == "" {
		return
	}
	s.mu.Lock()
	if s.failureCode == "" {
		s.failureCode = code
		if err != nil {
			s.failureDetail = err.Error()
		}
	}
	s.mu.Unlock()
}

func (s *session) snapshot() store.Recording {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.rec
	if !s.stopping {
		duration := s.durationLocked()
		rec.DurationMs = &duration
	}
	return rec
}

// prepareStop produces the terminal snapshot in memory only (no disk IO on
// the caller's thread). It returns the failure latched before the stop, if
// any; a failure latched during the later drain is caught by failure() after
// shutdown().
func (s *session) prepareStop(now time.Time) (store.Recording, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopping = true
	rec := s.rec
	duration := s.durationLocked()
	rec.DurationMs = &duration
	rec.StoppedAt = &now
	rec.UpdatedAt = now
	return rec, s.failureCode, s.failureDetail
}

func (s *session) failure() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failureCode, s.failureDetail
}

// shutdown closes both writer queues and waits for them to drain. It may
// block on the SD card; callers must be background goroutines.
func (s *session) shutdown() {
	if s.uplink != nil {
		s.uplink.close()
	}
	if s.downlink != nil {
		s.downlink.close()
	}
}

// durationLocked derives elapsed samples from the writer counters. Caller
// holds s.mu.
func (s *session) durationLocked() int64 {
	var samples int64
	if s.uplink != nil {
		if written := s.uplink.writtenSamples(); written > samples {
			samples = written
		}
	}
	if s.downlink != nil {
		if written := s.downlink.writtenSamples(); written > samples {
			samples = written
		}
	}
	return samples * 1000 / SampleRate
}

// trackWriter is the bounded async disk writer. enqueue never blocks: the
// queue has a fixed depth and an overflow or write error latches a failure
// (once) that stops the recording truthfully.
type trackWriter struct {
	file *os.File
	// queueDepth/maxDropped are copied from the manager so tests can shrink
	// or enlarge the window.
	queueDepth int
	maxDropped uint64
	onFailure  func(code string, err error)
	// writeOverride is nil in production; tests substitute the disk write.
	writeOverride func(file *os.File, buffer []byte) error
	// budgetCheck bounds a long call's storage growth: it runs on the writer
	// goroutine (never the audio path) every storageCheckFrames and fails
	// the segment once free space approaches the configured reserve.
	budgetCheck      func() error
	framesSinceCheck int

	queue     chan []int16
	done      chan struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once

	mu         sync.Mutex
	err        error
	closed     bool
	dropped    uint64
	written    int64
	failOnce   sync.Once
	syncTick   int
	frameBytes []byte
}

func newTrackWriter(path string, queueDepth int, maxDropped uint64, onFailure func(string, error), writeOverride func(*os.File, []byte) error, budgetCheck func() error) (*trackWriter, error) {
	if queueDepth < 1 {
		queueDepth = 1
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, err
	}
	writer := &trackWriter{
		file: file, queueDepth: queueDepth, maxDropped: maxDropped, onFailure: onFailure,
		queue: make(chan []int16, queueDepth), done: make(chan struct{}),
		writeOverride: writeOverride, budgetCheck: budgetCheck,
	}
	writer.wg.Add(1)
	go writer.run()
	return writer, nil
}

func (w *trackWriter) enqueue(pcm []int16) {
	buffer := make([]int16, len(pcm))
	copy(buffer, pcm)
	// The non-blocking send happens UNDER w.mu and close() closes the
	// channel under the same lock, so a send-on-closed-channel panic is
	// impossible; the lock is held only for the duration of the select.
	w.mu.Lock()
	if w.closed || w.err != nil {
		w.mu.Unlock()
		return
	}
	select {
	case w.queue <- buffer:
		w.mu.Unlock()
	default:
		w.dropped++
		dropped := w.dropped
		w.mu.Unlock()
		if dropped >= w.maxDropped {
			w.notifyFailure("writer_overflow", fmt.Errorf("recording writer stalled: %d frames dropped", dropped))
		}
	}
}

func (w *trackWriter) notifyFailure(code string, err error) {
	w.failOnce.Do(func() {
		w.mu.Lock()
		w.err = err
		w.mu.Unlock()
		if w.onFailure != nil {
			w.onFailure(code, err)
		}
	})
}

func (w *trackWriter) run() {
	defer w.wg.Done()
	draining := false
	for frame := range w.queue {
		if draining {
			continue
		}
		code, err := w.writeFrame(frame)
		if err != nil {
			draining = true
			if code == "" {
				code = "storage_error"
			}
			w.notifyFailure(code, err)
		}
	}
	close(w.done)
}

func (w *trackWriter) writeFrame(pcm []int16) (string, error) {
	need := len(pcm) * BytesPerSample
	if cap(w.frameBytes) < need {
		w.frameBytes = make([]byte, need)
	}
	buffer := w.frameBytes[:need]
	for index, sample := range pcm {
		buffer[index*2] = byte(uint16(sample))
		buffer[index*2+1] = byte(uint16(sample) >> 8)
	}
	if w.writeOverride != nil {
		if err := w.writeOverride(w.file, buffer); err != nil {
			return "storage_error", err
		}
	} else if _, err := w.file.Write(buffer); err != nil {
		return "storage_error", err
	}
	w.mu.Lock()
	w.written += int64(len(pcm))
	w.syncTick++
	sync := w.syncTick >= 500 // bound unfsynced data to ~10 s
	if sync {
		w.syncTick = 0
	}
	w.framesSinceCheck++
	checkBudget := w.budgetCheck != nil && w.framesSinceCheck >= storageCheckFrames
	if checkBudget {
		w.framesSinceCheck = 0
	}
	w.mu.Unlock()
	if sync {
		if err := w.file.Sync(); err != nil {
			return "storage_error", err
		}
	}
	if checkBudget {
		if err := w.budgetCheck(); err != nil {
			return "low_storage", err
		}
	}
	return "", nil
}

func (w *trackWriter) close() {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		close(w.queue)
		w.mu.Unlock()
		w.wg.Wait()
		_ = w.file.Sync()
		_ = w.file.Close()
	})
}

func (w *trackWriter) writtenSamples() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

var (
	pathToken      = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	silenceFrame   = make([]int16, FrameSamples)
	mountsFilePath = "/proc/self/mounts"
)

func defaultMountSourceLookup(path string) (mountPoint, source, fstype string, ok bool) {
	payload, err := os.ReadFile(mountsFilePath)
	if err != nil {
		return "", "", "", false
	}
	target := filepath.Clean(path)
	best := ""
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		entry := filepath.Clean(strings.ReplaceAll(fields[1], "\\040", " "))
		if target == entry || strings.HasPrefix(target, entry+string(filepath.Separator)) {
			if len(entry) > len(best) {
				best = entry
				mountPoint, source, fstype = entry, fields[0], fields[2]
			}
		}
	}
	return mountPoint, source, fstype, mountPoint != ""
}

func diskFreeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func cleanupWorkDir(workDir string) {
	if workDir == "" {
		return
	}
	_ = os.RemoveAll(workDir)
}

func purgeBlobs(rec store.Recording) error {
	if rec.FilePath != "" {
		if err := os.Remove(rec.FilePath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if rec.WaveformPath != "" {
		if err := os.Remove(rec.WaveformPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func retentionDeadline(created time.Time, days int) *time.Time {
	if days <= 0 {
		return nil
	}
	value := created.Add(time.Duration(days) * 24 * time.Hour)
	return &value
}

func randomRecordingID() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	return "rec_" + hex.EncodeToString(raw)
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
