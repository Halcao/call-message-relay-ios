package runtime

import (
	"context"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"callrelay/gateway/internal/unified/harass"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/voice"
)

//go:embed assets/voicemail-greeting.wav
var greetingFS embed.FS

const (
	StateVoicemailAnswering = "voicemail_answering"
	StateVoicemailRecording = "voicemail_recording"

	silenceThreshold  = 180  // PCM amplitude below this counts as silence
	trailingSilenceMs = 8000 // end the message after 8 s of trailing silence
	minMessageMs      = 1000 // shorter recordings are discarded as empty
)

type VoicemailSettings struct {
	Enabled        bool
	RingSeconds    int
	OfflineSeconds int
	MaxSeconds     int
	RetentionDays  int
	MaxBytes       int64
	MinFreeBytes   int64
}

func (m *Manager) VoicemailSettings(ctx context.Context) VoicemailSettings {
	settings := VoicemailSettings{
		Enabled: true, RingSeconds: 30, OfflineSeconds: 8, MaxSeconds: 120,
		RetentionDays: 30, MaxBytes: 500 << 20, MinFreeBytes: 256 << 20,
	}
	readInt := func(key string, target *int) {
		if value, err := m.Store.Setting(ctx, key); err == nil {
			if parsed, parseErr := strconv.Atoi(strings.TrimSpace(value)); parseErr == nil {
				*target = parsed
			}
		}
	}
	readInt64 := func(key string, target *int64) {
		if value, err := m.Store.Setting(ctx, key); err == nil {
			if parsed, parseErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64); parseErr == nil {
				*target = parsed
			}
		}
	}
	readInt("voicemail_ring_seconds", &settings.RingSeconds)
	readInt("voicemail_offline_seconds", &settings.OfflineSeconds)
	readInt("voicemail_max_seconds", &settings.MaxSeconds)
	readInt("voicemail_retention_days", &settings.RetentionDays)
	readInt64("voicemail_max_bytes", &settings.MaxBytes)
	readInt64("voicemail_min_free_bytes", &settings.MinFreeBytes)
	if value, err := m.Store.Setting(ctx, "voicemail_enabled"); err == nil {
		settings.Enabled = !strings.EqualFold(strings.TrimSpace(value), "false")
	}
	if settings.RingSeconds < 5 {
		settings.RingSeconds = 5
	}
	if settings.OfflineSeconds < 2 {
		settings.OfflineSeconds = 2
	}
	if settings.MaxSeconds < 10 || settings.MaxSeconds > 600 {
		settings.MaxSeconds = 120
	}
	if settings.RetentionDays < 1 {
		settings.RetentionDays = 30
	}
	return settings
}

// ---------- presence ----------

func (m *Manager) DeviceConnected(deviceID string) {
	m.mu.Lock()
	m.presence[deviceID]++
	m.mu.Unlock()
}

func (m *Manager) DeviceDisconnected(deviceID string) {
	m.mu.Lock()
	if m.presence[deviceID] > 0 {
		m.presence[deviceID]--
	}
	if m.presence[deviceID] == 0 {
		delete(m.presence, deviceID)
	}
	m.mu.Unlock()
}

func (m *Manager) onlineDeviceForLine(ctx context.Context, lineID string) bool {
	m.mu.Lock()
	presence := make(map[string]int, len(m.presence))
	for deviceID, count := range m.presence {
		presence[deviceID] = count
	}
	m.mu.Unlock()
	if len(presence) == 0 {
		return false
	}
	devices, err := m.Store.ListDevices(ctx)
	if err != nil {
		return false
	}
	for _, device := range devices {
		if presence[device.ID] == 0 {
			continue
		}
		access, accessErr := m.Store.DeviceAccess(ctx, device.ID)
		if accessErr != nil {
			continue
		}
		if entry, ok := linePermission(access, lineID); ok && entry.Permissions.ReceiveCalls {
			return true
		}
	}
	return false
}

func (m *Manager) hasPushTargetForLine(ctx context.Context, lineID string) bool {
	targets, err := m.Store.ListPushTargets(ctx)
	if err != nil {
		return false
	}
	for _, target := range targets {
		access, accessErr := m.Store.DeviceAccess(ctx, target.DeviceID)
		if accessErr != nil {
			continue
		}
		if entry, ok := linePermission(access, lineID); ok && entry.Permissions.ReceiveCalls {
			return true
		}
	}
	return false
}

// ---------- schedule / ownership ----------

// voicemailDelay is pure so the scheduling policy is testable: the short
// offline grace applies only when the phone neither has a live event stream
// nor a registered push target. A suspended iOS app legitimately has no
// WebSocket and must still get the full ring grace so PushKit can wake it.
func voicemailDelay(settings VoicemailSettings, hasOnlineDevice, hasPushTarget bool) time.Duration {
	delay := time.Duration(settings.RingSeconds) * time.Second
	if !hasOnlineDevice && !hasPushTarget {
		offline := time.Duration(settings.OfflineSeconds) * time.Second
		if offline > 0 && offline < delay {
			delay = offline
		}
	}
	return delay
}

func (m *Manager) scheduleVoicemail(ctx context.Context, call store.Call) {
	settings := m.VoicemailSettings(ctx)
	if !settings.Enabled || greetingSamples() == nil {
		return
	}
	hasOnline := m.onlineDeviceForLine(ctx, call.LineID)
	hasPush := m.hasPushTargetForLine(ctx, call.LineID)
	delay := voicemailDelay(settings, hasOnline, hasPush)
	callID := call.ID
	m.mu.Lock()
	if existing := m.voicemailTimers[callID]; existing != nil {
		existing.Stop()
	}
	m.voicemailTimers[callID] = time.AfterFunc(delay, func() { m.startVoicemail(callID) })
	m.mu.Unlock()
}

func (m *Manager) cancelVoicemailTimer(callID string) {
	m.mu.Lock()
	if timer := m.voicemailTimers[callID]; timer != nil {
		timer.Stop()
		delete(m.voicemailTimers, callID)
	}
	m.mu.Unlock()
}

func (m *Manager) startVoicemail(callID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	settings := m.VoicemailSettings(ctx)
	if !settings.Enabled || greetingSamples() == nil {
		return
	}
	call, err := m.Store.GetCall(ctx, callID)
	if err != nil || call.EndedAt != nil || call.State != StateRinging {
		return
	}
	claimed, err := m.Store.ClaimCallForVoicemail(ctx, callID)
	if err != nil || !claimed {
		// A device answered first; its media owns the call now.
		return
	}
	audioRoot, rootErr := m.voicemailAudioRoot()
	usage, usageErr := m.Store.VoicemailUsageBytes(ctx)
	var free int64
	var freeErr error
	if rootErr == nil {
		free, freeErr = diskFreeBytes(audioRoot)
	}
	if rootErr != nil || usageErr != nil || freeErr != nil || !storageAvailable(free, usage, settings) {
		slog.Warn("voicemail unavailable: SD storage", "call_id", callID, "free_bytes", free, "used_bytes", usage,
			"error_root", rootErr, "error_free", freeErr, "error_usage", usageErr)
		_, _ = m.Store.RevertVoicemailClaim(ctx, callID)
		reason := "storage"
		if rootErr != nil {
			reason = "sd_unavailable"
		}
		m.publish(call.LineID, "", "voicemail.unavailable", map[string]string{"callId": callID, "reason": reason})
		return
	}
	line := m.Line(call.LineID)
	if line == nil {
		_, _ = m.Store.RevertVoicemailClaim(ctx, callID)
		return
	}
	if err := line.Client.Answer(ctx, call.WorkerCallID, randomKey()); err != nil {
		slog.Warn("voicemail answer failed", "call_id", callID, "error", err)
		_, _ = m.Store.RevertVoicemailClaim(ctx, callID)
		return
	}
	now := time.Now().UTC()
	call.State = StateVoicemailRecording
	call.DeviceID = ""
	call.AnsweredAt = &now
	// Terminal-safe, monotonic write: a finisher that landed during the
	// upstream Answer RPC must never be resurrected by this snapshot.
	if err := m.Store.UpsertLiveCall(ctx, call); err != nil {
		return
	}
	m.publish(call.LineID, "", "call.updated", redactedCallView(m.view(call)))
	m.publish(call.LineID, "", "voicemail.started", map[string]string{"callId": callID, "lineId": call.LineID})
	go m.recordVoicemail(call, settings)
}

// recordVoicemail plays the audible greeting + beep, then records the caller.
// Recording only begins after the beep: there is never a silent capture.
func (m *Manager) recordVoicemail(call store.Call, settings VoicemailSettings) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(settings.MaxSeconds+180)*time.Second)
	defer cancel()
	line := m.Line(call.LineID)
	if line == nil {
		m.finishCall(call, "voicemail_unavailable")
		return
	}
	conn, err := line.Client.AttachMedia(ctx, call.WorkerCallID)
	if err != nil {
		slog.Warn("voicemail media attach failed", "call_id", call.ID, "error", err)
		m.finishCall(call, "voicemail_media_failed")
		return
	}
	defer conn.Close()
	if err := conn.WaitReady(ctx); err != nil {
		slog.Warn("voicemail media not ready", "call_id", call.ID, "error", err)
		m.finishCall(call, "voicemail_media_failed")
		return
	}

	voicemailID := randomID("vm_")
	audioRoot, rootErr := m.voicemailAudioRoot()
	if rootErr != nil {
		slog.Warn("voicemail SD unavailable after answer; hanging up", "call_id", call.ID, "error", rootErr)
		now := time.Now().UTC()
		record := store.Voicemail{
			ID: voicemailID, LineID: call.LineID, CallID: call.ID, Peer: call.Peer,
			State: "unavailable", CreatedAt: now, ExpiresAt: now.AddDate(0, 0, settings.RetentionDays),
		}
		_ = m.Store.InsertVoicemail(context.Background(), record)
		m.publish(call.LineID, "", "voicemail.failed", map[string]string{"callId": call.ID, "lineId": call.LineID, "reason": "sd_unavailable"})
		if current, getErr := m.Store.GetCall(context.Background(), call.ID); getErr == nil && current.EndedAt == nil {
			_ = line.Client.Hangup(context.Background(), call.WorkerCallID, randomKey())
			m.finishCall(current, "voicemail_storage_unavailable")
		}
		return
	}
	if err := os.MkdirAll(filepath.Join(audioRoot, call.LineID), 0o700); err != nil {
		m.finishCall(call, "voicemail_storage_failed")
		return
	}
	partPath, pathErr := safeVoicemailPath(audioRoot, call.LineID, voicemailID)
	if pathErr != nil {
		m.finishCall(call, "voicemail_storage_failed")
		return
	}
	partPath = strings.TrimSuffix(partPath, ".wav") + ".part.wav"
	finalPath := strings.TrimSuffix(partPath, ".part.wav") + ".wav"
	file, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		m.finishCall(call, "voicemail_storage_failed")
		return
	}
	writer := newWAVWriter(file, 8000, 1)
	now := time.Now().UTC()
	record := store.Voicemail{
		ID: voicemailID, LineID: call.LineID, CallID: call.ID, Peer: call.Peer,
		State: "recording", CreatedAt: now, ExpiresAt: now.AddDate(0, 0, settings.RetentionDays),
	}
	if err := m.Store.InsertVoicemail(ctx, record); err != nil {
		_ = file.Close()
		_ = os.Remove(partPath)
		m.finishCall(call, "voicemail_storage_failed")
		return
	}

	if err := playSamples(ctx, conn, greetingSamples()); err != nil {
		_ = file.Close()
		_ = os.Remove(partPath)
		_ = m.Store.DeleteVoicemail(context.Background(), voicemailID)
		m.finishCall(call, "voicemail_greeting_failed")
		return
	}
	if err := playSamples(ctx, conn, buildBeep(1000, 200)); err != nil {
		_ = file.Close()
		_ = os.Remove(partPath)
		_ = m.Store.DeleteVoicemail(context.Background(), voicemailID)
		m.finishCall(call, "voicemail_greeting_failed")
		return
	}
	// Discard anything buffered during greeting/beep: the message starts now.
	drainFrames(conn.Frames)

	recordedSamples := 0
	silenceSamples := 0
	maxSamples := settings.MaxSeconds * 8000
	lastCheck := time.Now()
recording:
	for {
		select {
		case <-ctx.Done():
			break recording
		case frame, ok := <-conn.Frames:
			if !ok {
				break recording
			}
			pcm, decodeErr := voice.DecodePCMU(frame)
			if decodeErr != nil {
				continue
			}
			if _, writeErr := writer.WriteSamples(pcm); writeErr != nil {
				slog.Warn("voicemail write failed", "voicemail_id", voicemailID, "error", writeErr)
				break recording
			}
			recordedSamples += len(pcm)
			if peak := pcmPeak(pcm); peak < silenceThreshold {
				silenceSamples += len(pcm)
			} else {
				silenceSamples = 0
			}
			if silenceSamples >= trailingSilenceMs*8 {
				break recording
			}
			if recordedSamples >= maxSamples {
				break recording
			}
			if time.Since(lastCheck) > time.Second {
				lastCheck = time.Now()
				if current, getErr := m.Store.GetCall(context.Background(), call.ID); getErr != nil || current.EndedAt != nil {
					break recording
				}
			}
		}
	}

	durationMs := int64(recordedSamples) * 1000 / 8000
	sizeBytes, finalizeErr := writer.Finalize()
	_ = file.Close()
	if finalizeErr != nil {
		slog.Warn("voicemail finalize failed", "voicemail_id", voicemailID, "error", finalizeErr)
	}
	if durationMs < minMessageMs {
		_ = os.Remove(partPath)
		_ = m.Store.DeleteVoicemail(context.Background(), voicemailID)
		m.publish(call.LineID, "", "voicemail.empty", map[string]string{"callId": call.ID, "lineId": call.LineID})
	} else if finalizeErr == nil {
		if err := os.Rename(partPath, finalPath); err != nil {
			slog.Warn("voicemail rename failed", "voicemail_id", voicemailID, "error", err)
			_ = os.Remove(partPath)
			_ = m.Store.FinalizeVoicemail(context.Background(), voicemailID, "corrupt", "", durationMs, sizeBytes, time.Now().UTC().AddDate(0, 0, settings.RetentionDays))
			m.publish(call.LineID, "", "voicemail.failed", map[string]string{"voicemailId": voicemailID, "lineId": call.LineID, "reason": "write_failed"})
		} else {
			expiresAt := time.Now().UTC().AddDate(0, 0, settings.RetentionDays)
			_ = m.Store.FinalizeVoicemail(context.Background(), voicemailID, "ready", finalPath, durationMs, sizeBytes, expiresAt)
			record.State = "ready"
			record.Path = finalPath
			record.DurationMs = durationMs
			record.SizeBytes = sizeBytes
			record.ExpiresAt = expiresAt
			m.publish(call.LineID, "", "voicemail.created", m.voicemailView(record))
			_ = m.Store.AppendAudit(context.Background(), "system", "voicemail.record", voicemailID, "accepted")
			slog.Info("voicemail recorded", "voicemail_id", voicemailID, "line", call.LineID, "duration_ms", durationMs, "size_bytes", sizeBytes)
		}
	} else {
		_ = os.Remove(partPath)
		_ = m.Store.FinalizeVoicemail(context.Background(), voicemailID, "corrupt", "", durationMs, sizeBytes, time.Now().UTC().AddDate(0, 0, settings.RetentionDays))
		m.publish(call.LineID, "", "voicemail.failed", map[string]string{"voicemailId": voicemailID, "lineId": call.LineID, "reason": "wav_finalize_failed"})
	}

	if current, getErr := m.Store.GetCall(context.Background(), call.ID); getErr == nil && current.EndedAt == nil {
		_ = line.Client.Hangup(context.Background(), call.WorkerCallID, randomKey())
		m.finishCall(current, "voicemail")
	}
}

// playSamples streams signed PCM to the line at the fixed 20 ms cadence.
func playSamples(ctx context.Context, conn interface {
	Send([]byte) error
}, samples []int16) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for offset := 0; offset+voice.FrameSamples <= len(samples); offset += voice.FrameSamples {
		frame, err := voice.EncodePCMU(samples[offset : offset+voice.FrameSamples])
		if err != nil {
			return err
		}
		if err := conn.Send(frame); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

func drainFrames(frames <-chan []byte) {
	for {
		select {
		case <-frames:
		default:
			return
		}
	}
}

func pcmPeak(samples []int16) int {
	peak := 0
	for _, sample := range samples {
		value := int(sample)
		if value < 0 {
			value = -value
		}
		if value > peak {
			peak = value
		}
	}
	return peak
}

// ---------- greeting / beep ----------

var (
	greetingOnce    sync.Once
	greetingDecoded []int16
)

func greetingSamples() []int16 {
	greetingOnce.Do(func() {
		payload, err := greetingFS.ReadFile("assets/voicemail-greeting.wav")
		if err != nil {
			slog.Error("voicemail greeting asset missing", "error", err)
			return
		}
		samples, err := parseWAV(payload)
		if err != nil {
			slog.Error("voicemail greeting asset invalid", "error", err)
			return
		}
		greetingDecoded = samples
	})
	return greetingDecoded
}

// parseWAV accepts the 8 kHz / mono / 16-bit PCM greeting generated by
// tools/generate-voicemail-greeting.sh.
func parseWAV(payload []byte) ([]int16, error) {
	if len(payload) < 44 || string(payload[0:4]) != "RIFF" || string(payload[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	offset := 12
	var data []byte
	for offset+8 <= len(payload) {
		chunkID := string(payload[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(payload[offset+4 : offset+8]))
		body := offset + 8
		if body+chunkSize > len(payload) {
			chunkSize = len(payload) - body
		}
		if chunkID == "fmt " {
			if chunkSize < 16 {
				return nil, errors.New("fmt chunk too small")
			}
			audioFormat := binary.LittleEndian.Uint16(payload[body : body+2])
			channels := binary.LittleEndian.Uint16(payload[body+2 : body+4])
			sampleRate := binary.LittleEndian.Uint32(payload[body+4 : body+8])
			bits := binary.LittleEndian.Uint16(payload[body+14 : body+16])
			if audioFormat != 1 || channels != 1 || sampleRate != 8000 || bits != 16 {
				return nil, fmt.Errorf("unsupported greeting format: fmt=%d channels=%d rate=%d bits=%d", audioFormat, channels, sampleRate, bits)
			}
		}
		if chunkID == "data" {
			data = payload[body : body+chunkSize]
		}
		offset = body + chunkSize
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if len(data) < 2 {
		return nil, errors.New("greeting has no audio data")
	}
	samples := make([]int16, len(data)/2)
	for i := range samples {
		samples[i] = int16(binary.LittleEndian.Uint16(data[i*2 : i*2+2]))
	}
	return samples, nil
}

func buildBeep(frequency, durationMs int) []int16 {
	count := 8000 * durationMs / 1000
	samples := make([]int16, count)
	fade := count / 20
	if fade < 1 {
		fade = 1
	}
	for i := 0; i < count; i++ {
		amplitude := 9000.0
		if i < fade {
			amplitude *= float64(i) / float64(fade)
		}
		if count-i < fade {
			amplitude *= float64(count-i) / float64(fade)
		}
		samples[i] = int16(amplitude * math.Sin(2*math.Pi*float64(frequency)*float64(i)/8000))
	}
	samples[0] = 0
	samples[count-1] = 0
	return samples
}

// ---------- storage ----------

// mountSourceLookup is injectable so tests can exercise mount verification
// without a real SD card. Production parses /proc/self/mounts.
var mountSourceLookup = defaultMountSourceLookup

func defaultMountSourceLookup(path string) (mountPoint, source, fstype string, ok bool) {
	payload, err := os.ReadFile("/proc/self/mounts")
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

// voicemailAudioRoot verifies the configured SD mount and device before any
// recording. It never falls back to the eMMC or the underlying root
// directory: when the card is absent or mismatched, voicemail is unavailable
// while ordinary telephony stays healthy.
func (m *Manager) voicemailAudioRoot() (string, error) {
	cfg := m.Cfg.Voicemail
	root := filepath.Clean(strings.TrimSpace(cfg.AudioRoot))
	mountPoint := filepath.Clean(strings.TrimSpace(cfg.MountPoint))
	if root == "." || mountPoint == "." {
		return "", errors.New("voicemail audio root is not configured")
	}
	if !strings.HasPrefix(root, mountPoint+string(filepath.Separator)) {
		return "", fmt.Errorf("voicemail audio root escapes mount point")
	}
	foundMount, source, fstype, ok := mountSourceLookup(root)
	if !ok || filepath.Clean(foundMount) != mountPoint {
		return "", fmt.Errorf("voicemail SD mount %s is not present", mountPoint)
	}
	if expected := strings.TrimSpace(cfg.ExpectedDevice); expected != "" && !strings.HasPrefix(source, expected) {
		return "", fmt.Errorf("voicemail mount device %s does not match expected %s", source, expected)
	}
	if expected := strings.TrimSpace(cfg.ExpectedFSType); expected != "" && !strings.EqualFold(fstype, expected) {
		return "", fmt.Errorf("voicemail mount filesystem %s does not match expected %s", fstype, expected)
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
		return "", errors.New("voicemail audio root must not be a symlink")
	}
	if !info.IsDir() {
		return "", errors.New("voicemail audio root is not a directory")
	}
	return root, nil
}

type VoicemailStorageStatus struct {
	Mounted   bool
	Device    string
	FSType    string
	Root      string
	FreeBytes int64
	Error     string
}

// VoicemailStorageStatus reports the verified SD state for the console.
func (m *Manager) VoicemailStorageStatus() VoicemailStorageStatus {
	status := VoicemailStorageStatus{Root: m.Cfg.Voicemail.AudioRoot}
	root, err := m.voicemailAudioRoot()
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Mounted = true
	_, source, fstype, _ := mountSourceLookup(root)
	status.Device = source
	status.FSType = fstype
	if free, freeErr := diskFreeBytes(root); freeErr == nil {
		status.FreeBytes = free
	}
	return status
}

var voicemailToken = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// safeVoicemailPath builds and validates the audio path for one recording.
func safeVoicemailPath(root, lineID, voicemailID string) (string, error) {
	if !voicemailToken.MatchString(lineID) || !voicemailToken.MatchString(voicemailID) {
		return "", errors.New("unsafe voicemail path component")
	}
	path := filepath.Join(root, lineID, voicemailID+".wav")
	if !strings.HasPrefix(path, filepath.Clean(root)+string(filepath.Separator)) {
		return "", errors.New("voicemail path escapes audio root")
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("voicemail path must not be a symlink")
	}
	return path, nil
}

func diskFreeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}

func storageAvailable(freeBytes, usedBytes int64, settings VoicemailSettings) bool {
	if settings.MinFreeBytes > 0 && freeBytes < settings.MinFreeBytes {
		return false
	}
	if settings.MaxBytes > 0 && usedBytes >= settings.MaxBytes {
		return false
	}
	return true
}

// ---------- retention ----------

func (m *Manager) RunVoicemailRetention(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		m.pruneVoicemail(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) pruneVoicemail(ctx context.Context) {
	audioRoot, rootErr := m.voicemailAudioRoot()
	if rootErr != nil {
		// Without the verified SD the blobs are unreachable; keep the
		// index untouched rather than deleting anything on the eMMC.
		slog.Warn("voicemail retention skipped: SD unavailable", "error", rootErr)
		return
	}
	expired, err := m.Store.ListExpiredVoicemails(ctx, time.Now().UTC(), 200)
	if err != nil {
		return
	}
	_ = audioRoot
	for _, voicemail := range expired {
		if voicemail.Path != "" {
			_ = os.Remove(voicemail.Path)
		}
		_ = m.Store.DeleteVoicemail(ctx, voicemail.ID)
		slog.Info("voicemail expired and removed", "voicemail_id", voicemail.ID, "line", voicemail.LineID)
	}
	// Hard-delete rows soft-deleted from the app as soon as the blob is
	// reachable, regardless of the capacity quota. This must run before the
	// MaxBytes early returns: under-capacity systems would otherwise retry the
	// blob removal forever. A row whose removal fails (mount mismatch/IO) is
	// kept for a later pass.
	m.removeSoftDeletedBlobs(ctx, audioRoot)
	settings := m.VoicemailSettings(ctx)
	if settings.MaxBytes <= 0 {
		return
	}
	usage, err := m.Store.VoicemailUsageBytes(ctx)
	if err != nil || usage <= settings.MaxBytes {
		return
	}
	oldest, err := m.Store.ListVoicemails(ctx, "", 200)
	if err != nil {
		return
	}
	for i := len(oldest) - 1; i >= 0 && usage > settings.MaxBytes; i-- {
		voicemail := oldest[i]
		if voicemail.Path != "" {
			_ = os.Remove(voicemail.Path)
		}
		_ = m.Store.DeleteVoicemail(ctx, voicemail.ID)
		usage -= voicemail.SizeBytes
		slog.Info("voicemail evicted by capacity cap", "voicemail_id", voicemail.ID, "line", voicemail.LineID)
	}
}

// removeSoftDeletedBlobs reclaims blobs for rows the app soft-deleted, then
// hard-deletes the rows. Only verified paths under the mounted SD root are
// removed; an invalid/unsafe path keeps the row instead of risking another
// file.
func (m *Manager) removeSoftDeletedBlobs(ctx context.Context, audioRoot string) {
	deleted, err := m.Store.ListDeletedVoicemails(ctx, 200)
	if err != nil {
		return
	}
	for _, voicemail := range deleted {
		if voicemail.Path != "" {
			expected, pathErr := safeVoicemailPath(audioRoot, voicemail.LineID, voicemail.ID)
			if pathErr != nil || filepath.Clean(voicemail.Path) != expected {
				continue
			}
			if removeErr := os.Remove(expected); removeErr != nil && !os.IsNotExist(removeErr) {
				continue
			}
		}
		_ = m.Store.DeleteVoicemail(ctx, voicemail.ID)
	}
}

// ---------- views / API ----------

type VoicemailView struct {
	ID         string `json:"id"`
	LineID     string `json:"lineId"`
	LineName   string `json:"lineName"`
	Peer       string `json:"peer"`
	State      string `json:"state"`
	DurationMs int64  `json:"durationMs"`
	SizeBytes  int64  `json:"sizeBytes"`
	CreatedAt  int64  `json:"createdAt"`
	ExpiresAt  int64  `json:"expiresAt"`
}

func (m *Manager) voicemailView(voicemail store.Voicemail) VoicemailView {
	view := VoicemailView{
		ID: voicemail.ID, LineID: voicemail.LineID, Peer: voicemail.Peer, State: voicemail.State,
		DurationMs: voicemail.DurationMs, SizeBytes: voicemail.SizeBytes,
		CreatedAt: voicemail.CreatedAt.UnixMilli(), ExpiresAt: voicemail.ExpiresAt.UnixMilli(),
	}
	if line := m.Line(voicemail.LineID); line != nil {
		view.LineName = line.Cfg.Name
	}
	return view
}

func (m *Manager) ListVoicemails(ctx context.Context, deviceID, lineFilter string) ([]VoicemailView, error) {
	access, err := m.Store.DeviceAccess(ctx, deviceID)
	if err != nil {
		return nil, fail(401, "CB-V2-401", "登录状态已失效")
	}
	if lineFilter != "" {
		entry, ok := linePermission(access, lineFilter)
		if !ok || !entry.Permissions.ReceiveCalls {
			return nil, ErrForbidden
		}
	}
	voicemails, err := m.Store.ListVoicemails(ctx, lineFilter, 200)
	if err != nil {
		return nil, err
	}
	views := make([]VoicemailView, 0, len(voicemails))
	for _, voicemail := range voicemails {
		entry, ok := linePermission(access, voicemail.LineID)
		if !ok || !entry.Permissions.ReceiveCalls {
			continue
		}
		// A key whose harassment policy denies this caller never sees the
		// voicemail either (blocked callers are rejected before voicemail).
		if m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindCall, voicemail.Peer, "") {
			continue
		}
		views = append(views, m.voicemailView(voicemail))
	}
	return views, nil
}

func (m *Manager) VoicemailForDevice(ctx context.Context, deviceID, voicemailID string) (store.Voicemail, error) {
	voicemail, err := m.Store.GetVoicemail(ctx, voicemailID)
	if err != nil || voicemail.DeletedAt != nil {
		return store.Voicemail{}, fail(404, "CB-V2-404", "留言不存在")
	}
	access, _, _, err := m.requireLine(ctx, deviceID, voicemail.LineID, func(permission store.KeyLine) bool { return permission.ReceiveCalls })
	if err != nil {
		return store.Voicemail{}, err
	}
	if m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindCall, voicemail.Peer, "") {
		// Existence-hidden for a suppressing key on the same line.
		return store.Voicemail{}, fail(404, "CB-V2-404", "留言不存在")
	}
	return voicemail, nil
}

// DeleteVoicemail soft-deletes an authorized device's voicemail and removes
// its audio blob from the verified SD root. The row is kept (deleted_at set)
// so concurrent list/audio requests see a stable 404 and retention can later
// hard-delete a blob whose mount was unavailable at delete time. Only a device
// with receive_calls on the voicemail's line may delete it.
func (m *Manager) DeleteVoicemail(ctx context.Context, deviceID, voicemailID string) (VoicemailView, error) {
	voicemail, err := m.VoicemailForDevice(ctx, deviceID, voicemailID)
	if err != nil {
		return VoicemailView{}, err
	}
	deleted, err := m.Store.SoftDeleteVoicemail(ctx, voicemailID, time.Now().UTC())
	if err != nil {
		return VoicemailView{}, err
	}
	if !deleted {
		return VoicemailView{}, fail(404, "CB-V2-404", "留言不存在")
	}
	// Best-effort blob removal behind the same mount + path verification used
	// for recording. A missing/unmounted SD card never blocks the delete: the
	// row is already hidden and retention prunes the file when the mount is
	// back.
	if root, rootErr := m.voicemailAudioRoot(); rootErr == nil {
		if expected, pathErr := safeVoicemailPath(root, voicemail.LineID, voicemail.ID); pathErr == nil &&
			filepath.Clean(voicemail.Path) == expected {
			if removeErr := os.Remove(expected); removeErr != nil && !os.IsNotExist(removeErr) {
				slog.Warn("voicemail blob removal failed", "voicemail_id", voicemailID, "error", removeErr)
			}
		} else if pathErr != nil {
			slog.Warn("voicemail blob path unsafe at delete", "voicemail_id", voicemailID, "error", pathErr)
		}
	}
	view := m.voicemailView(voicemail)
	_ = m.Store.AppendAudit(ctx, deviceID, "voicemail.delete", voicemailID, "accepted")
	m.publish(voicemail.LineID, "", "voicemail.deleted", map[string]any{
		"id": voicemail.ID, "lineId": voicemail.LineID,
	})
	return view, nil
}
