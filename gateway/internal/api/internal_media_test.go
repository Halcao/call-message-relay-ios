package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"callrelay/gateway/internal/call"
	"callrelay/gateway/internal/db"
	"callrelay/gateway/internal/modem"
	"callrelay/gateway/internal/voice"

	"github.com/gorilla/websocket"
)

// lockAudio models the single-owner voice device (QDC507/ALSA/raw PCM share one
// capture/playback pair per line). Start for another call id fails exactly like
// production; release is owner-conditional, like StopIfOwner. When startGate
// is non-nil, Start blocks on it (or ctx) to simulate the seconds-long route
// bootstrap, with blockedStarts counting the in-flight starts.
type lockAudio struct {
	mu          sync.Mutex
	owner       modem.CallID
	starts      int
	stops       int
	startGate   chan struct{}
	blockedStarts int32
}

func (a *lockAudio) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "lock-test", SampleRate: 8000, Channels: 1}, nil
}

func (a *lockAudio) Start(ctx context.Context, callID modem.CallID) error {
	a.mu.Lock()
	gate := a.startGate
	a.mu.Unlock()
	if gate != nil {
		atomic.AddInt32(&a.blockedStarts, 1)
		select {
		case <-ctx.Done():
			atomic.AddInt32(&a.blockedStarts, -1)
			return ctx.Err()
		case <-gate:
			atomic.AddInt32(&a.blockedStarts, -1)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.owner != "" && a.owner != callID {
		return &audioHeldError{current: a.owner}
	}
	a.owner = callID
	a.starts++
	return nil
}

func (a *lockAudio) ReadPCM(pcm []int16) (int, error) {
	time.Sleep(20 * time.Millisecond)
	return 0, context.Canceled
}

func (a *lockAudio) WritePCM(pcm []int16) (int, error) { return len(pcm), nil }

func (a *lockAudio) Stop(context.Context) error {
	a.mu.Lock()
	a.owner = ""
	a.stops++
	a.mu.Unlock()
	return nil
}

func (a *lockAudio) Close() error { return nil }

func (a *lockAudio) CurrentOwner() modem.CallID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.owner
}

func (a *lockAudio) StopIfOwner(_ context.Context, callID modem.CallID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.owner != callID || a.owner == "" {
		return false
	}
	a.owner = ""
	a.stops++
	return true
}

// gateModem blocks WaitAnyActive until released, simulating the seconds-long
// cellular wait during which the call may end and another begin.
type gateModem struct {
	*fakeCallModem
	proceed chan struct{}
	once    sync.Once
}

func newGateModem(hold bool) *gateModem {
	gm := &gateModem{fakeCallModem: &fakeCallModem{}, proceed: make(chan struct{})}
	if !hold {
		gm.release()
	}
	return gm
}

func (m *gateModem) WaitAnyActive(ctx context.Context) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-m.proceed:
		return true, nil
	}
}
func (m *gateModem) release() { m.once.Do(func() { close(m.proceed) }) }

type audioHeldError struct{ current modem.CallID }

func (e *audioHeldError) Error() string {
	return "QDC507 audio is already active for call " + string(e.current)
}

func dialInternalMedia(t *testing.T, server *httptest.Server, token, callID string) *websocket.Conn {
	t.Helper()
	url := strings.Replace(server.URL, "http://", "ws://", 1) + "/internal/v1/media/" + callID
	header := map[string][]string{"X-Internal-Token": {token}}
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("dial media socket: %v", err)
	}
	return conn
}

func mediaControl(t *testing.T, conn *websocket.Conn, deadline time.Duration) map[string]string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(deadline))
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read control frame: %v", err)
	}
	var frame map[string]string
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("control frame %q is not JSON: %v", payload, err)
	}
	return frame
}

func newMediaTestServer(t *testing.T, holdWait bool) (*Server, *gateModem, *lockAudio, *httptest.Server) {
	t.Helper()
	database, err := db.Open(t.TempDir() + "/worker.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"call-ended", "call-live", "call-old", "call-next"} {
		if _, err := database.SaveCall(context.Background(), db.Call{
			ID: id, Direction: "inbound", Peer: "100", State: "connecting",
			StartedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer("gw_test", "Test", "dev")
	server.Database = database
	server.CallControl = call.NewController()
	gm := newGateModem(holdWait)
	server.CallModem = gm
	audio := &lockAudio{}
	server.VoiceAudio = audio
	httpServer := httptest.NewServer(server.InternalHandler("secret-token"))
	t.Cleanup(httpServer.Close)
	return server, gm, audio, httpServer
}

// Regression for the live incident (2026-10-03): a late inbound media socket
// started audio AFTER its call ended on NO CARRIER and leaked the single
// device lock; every following outbound call rang the remote phone but failed
// bridge start ("media_lost").
func TestInternalMediaNeverLeaksAudioLock(t *testing.T) {
	_, gm, audio, httpServer := newMediaTestServer(t, true)

	// A media socket is blocked inside WaitAnyActive; the call ends before it
	// returns. It must NOT take audio and must report call_ended.
	conn := dialInternalMedia(t, httpServer, "secret-token", "call-ended")
	time.Sleep(50 * time.Millisecond) // socket connected, blocked on cellular wait
	gm.release()
	frame := mediaControl(t, conn, 3*time.Second)
	if frame["type"] != "error" || frame["code"] != "call_ended" {
		t.Fatalf("late socket frame = %#v, want error/call_ended", frame)
	}
	_ = conn.Close()
	audio.mu.Lock()
	owner, starts, stops := audio.owner, audio.starts, audio.stops
	audio.mu.Unlock()
	if owner != "" || starts != 0 {
		t.Fatalf("late socket took audio: owner=%q starts=%d", owner, starts)
	}
	if stops != 0 {
		t.Fatal("teardown of a never-started socket must not touch the device")
	}
}

// Deterministic ownership race: an OLD call's socket ends (and tries cleanup)
// ONLY after a NEW call legitimately owns audio. The old teardown must not
// release the new call's lock — including the reclaim path where an orphan
// lock existed beforehand.
func TestStaleTeardownCannotStopNewOwner(t *testing.T) {
	server, _, audio, httpServer := newMediaTestServer(t, false)

	// Seed an orphan lock from an earlier call (the observed live leak).
	if err := audio.Start(context.Background(), "call-old"); err != nil {
		t.Fatal(err)
	}
	oldBridge := voice.NewBridge(audio, internalDropTransport{})
	server.voiceBridges["call-old"] = oldBridge

	// New outbound call becomes the controller's active call.
	if _, _, err := server.CallControl.Dial("req-next", "call-next", "100"); err != nil {
		t.Fatal(err)
	}
	conn := dialInternalMedia(t, httpServer, "secret-token", "call-next")
	frame := mediaControl(t, conn, 3*time.Second)
	if frame["type"] != "ready" {
		t.Fatalf("new call frame = %#v, want ready", frame)
	}
	audio.mu.Lock()
	if audio.owner != "call-next" || audio.starts < 1 {
		t.Fatalf("after reclaim owner=%q starts=%d, want call-next", audio.owner, audio.starts)
	}
	audio.mu.Unlock()

	// NOW the stale old bridge finishes its teardown (socket close / terminal
	// event arriving late). It must not release call-next's audio.
	oldBridge.Stop(context.Background())
	server.releaseMedia("call-old", oldBridge)
	audio.mu.Lock()
	owner := audio.owner
	audio.mu.Unlock()
	if owner != "call-next" {
		t.Fatalf("stale teardown stole audio from call-next: owner=%q", owner)
	}
	_ = conn.Close()
	time.Sleep(50 * time.Millisecond)
	audio.mu.Lock()
	owner = audio.owner
	audio.mu.Unlock()
	// The call-next socket's own teardown releases it because it is still the
	// owner; only a FOREIGN teardown must be rejected.
	if owner != "" {
		t.Fatalf("own teardown should release, owner=%q", owner)
	}
}

// A socket for a call that is not the active call must never take or reclaim
// audio while a different call is active and holds the device.
func TestAcquireNeverTakesAudioFromAnotherActiveCall(t *testing.T) {
	server, _, audio, httpServer := newMediaTestServer(t, false)
	controller := server.CallControl

	if _, _, err := controller.Dial("req-old", "call-old", "100"); err != nil {
		t.Fatal(err)
	}
	if err := audio.Start(context.Background(), "call-old"); err != nil {
		t.Fatal(err)
	}
	// A stray/duplicate socket for call-next arrives while call-old is the
	// live, audio-owning call.
	conn := dialInternalMedia(t, httpServer, "secret-token", "call-next")
	frame := mediaControl(t, conn, 3*time.Second)
	if frame["code"] != "call_ended" {
		t.Fatalf("frame = %#v, want call_ended (foreign active call untouched)", frame)
	}
	_ = conn.Close()
	audio.mu.Lock()
	defer audio.mu.Unlock()
	if audio.owner != "call-old" {
		t.Fatalf("active holder lost the lock: owner=%q", audio.owner)
	}
}

func TestIsAudioHeldElsewhere(t *testing.T) {
	if !isAudioHeldElsewhere(&audioHeldError{current: "x"}) {
		t.Fatal("expected backend lock error recognized")
	}
	if isAudioHeldElsewhere(nil) || isAudioHeldElsewhere(errors.New("nope")) {
		t.Fatal("unrelated error must not be treated as a lock conflict")
	}
}

// Regression for the reclaim review: the stale holder's bridge was REALLY
// started (its recorded call id is the holder), so stale.Stop already
// releases the device. acquireMedia must treat that completed release as a
// reclaimed lock and start the new call — the old code mistook the second
// (no-op) StopIfOwner for a failure and returned the original
// "audio is already active" error, so the new call could never start.
func TestAcquireReclaimSucceedsWhenStaleBridgeWasStarted(t *testing.T) {
	server, _, audio, httpServer := newMediaTestServer(t, false)

	// Stale bridge from an earlier call: actually started against the device
	// (recorded call id = call-old) then abandoned with its socket still
	// open — the exact live-leak shape.
	oldBridge := voice.NewBridge(audio, internalDropTransport{})
	oldCtx, oldCancel := context.WithCancel(context.Background())
	defer oldCancel()
	if err := oldBridge.Start(oldCtx, "call-old"); err != nil {
		t.Fatal(err)
	}
	server.webrtcMu.Lock()
	server.voiceBridges["call-old"] = oldBridge
	server.webrtcMu.Unlock()

	// New outbound call becomes the controller's active call.
	if _, _, err := server.CallControl.Dial("req-next", "call-next", "100"); err != nil {
		t.Fatal(err)
	}
	conn := dialInternalMedia(t, httpServer, "secret-token", "call-next")
	frame := mediaControl(t, conn, 5*time.Second)
	if frame["type"] != "ready" {
		t.Fatalf("new call frame = %#v, want ready (reclaim after a started stale bridge)", frame)
	}
	audio.mu.Lock()
	owner, starts := audio.owner, audio.starts
	audio.mu.Unlock()
	if owner != "call-next" {
		t.Fatalf("owner after reclaim = %q, want call-next (starts=%d)", owner, starts)
	}
	_ = conn.Close()
}

// Deterministic version of the Snapshot/Start/terminal-URC race: the device
// start is mid-flight (route bootstrap takes seconds live) when the modem
// reports NO CARRIER. The socket must report call_ended and the device must
// be free afterwards — never held by the ended call.
func TestTerminalURCDuringBridgeStartReleasesAudio(t *testing.T) {
	server, _, audio, httpServer := newMediaTestServer(t, false)
	audio.startGate = make(chan struct{})

	if _, _, err := server.CallControl.Dial("req-live", "call-live", "100"); err != nil {
		t.Fatal(err)
	}
	conn := dialInternalMedia(t, httpServer, "secret-token", "call-live")
	// Wait until the handler is blocked inside the gated device start.
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&audio.blockedStarts) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("media handler never reached the gated device start")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Terminal URC arrives mid-start. closeWebRTC blocks on mediaMu until
	// acquireMedia finishes, then releases the just-acquired lock itself.
	endedDone := make(chan struct{})
	go func() {
		defer close(endedDone)
		server.HandleModemEvent(modem.ModemEvent{Kind: "ended", CallID: "call-live", Raw: "NO CARRIER"})
	}()
	time.Sleep(50 * time.Millisecond) // let closeWebRTC park on mediaMu
	close(audio.startGate)

	frame := mediaControl(t, conn, 5*time.Second)
	if frame["code"] != "call_ended" {
		t.Fatalf("frame = %#v, want call_ended (call ended during device start)", frame)
	}
	<-endedDone
	audio.mu.Lock()
	owner := audio.owner
	audio.mu.Unlock()
	if owner != "" {
		t.Fatalf("ended call kept the audio device: owner=%q", owner)
	}
	_ = conn.Close()
}

// Bridge lifecycle: Stop is terminal. A retry on the same bridge must be
// rejected instead of re-arming loops whose later Stop would be a no-op
// (consumed sync.Once) — the re-leak shape called out in review.
func TestBridgeStartAfterStopIsRejected(t *testing.T) {
	audio := &lockAudio{}
	bridge := voice.NewBridge(audio, internalDropTransport{})
	ctx := context.Background()
	if err := bridge.Start(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Start(ctx, "b"); err == nil {
		t.Fatal("Start after Stop must be rejected (terminal lifecycle)")
	}
	if owner := audio.CurrentOwner(); owner != "" {
		t.Fatalf("device re-owned by the rejected restart: %q", owner)
	}
}

// A bridge that never started never owned the device; stopping it must not
// cut audio a foreign live call legitimately holds.
func TestNeverStartedBridgeStopKeepsForeignOwner(t *testing.T) {
	audio := &lockAudio{}
	if err := audio.Start(context.Background(), "foreign"); err != nil {
		t.Fatal(err)
	}
	bridge := voice.NewBridge(audio, internalDropTransport{})
	_ = bridge.Stop(context.Background())
	if owner := audio.CurrentOwner(); owner != "foreign" {
		t.Fatalf("never-started bridge stopped the device: owner=%q", owner)
	}
	audio.mu.Lock()
	stops := audio.stops
	audio.mu.Unlock()
	if stops != 0 {
		t.Fatalf("never-started bridge touched the device: stops=%d", stops)
	}
}

func TestOwnerConditionalStop(t *testing.T) {
	audio := &lockAudio{}
	_ = audio.Start(context.Background(), "a")
	if audio.StopIfOwner(context.Background(), "b") {
		t.Fatal("foreign owner release must be rejected")
	}
	if audio.CurrentOwner() != "a" {
		t.Fatal("owner changed by foreign release")
	}
	if !audio.StopIfOwner(context.Background(), "a") {
		t.Fatal("own release must succeed")
	}
	if audio.CurrentOwner() != "" {
		t.Fatal("owner not cleared")
	}
}
