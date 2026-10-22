package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"callrelay/gateway/internal/voice"

	"github.com/gorilla/websocket"
)

// mediaTestClient wraps a client media socket with a continuous background
// reader: gorilla latches read errors once a read deadline fires, so deadline
// polling is fatal; production readers block like this too.
type mediaTestClient struct {
	conn   *websocket.Conn
	frames chan []byte
	// firstNonSilence marks the byte pattern used for cellular audio.
}

func dialMedia(t *testing.T, stack *testStack, token, callID string) *mediaTestClient {
	t.Helper()
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/calls/" + callID + "/media"
	conn, response, err := websocket.DefaultDialer.DialContext(context.Background(), url, header)
	if err != nil {
		t.Fatalf("media upgrade: %v (status %v)", err, response)
	}
	client := &mediaTestClient{conn: conn, frames: make(chan []byte, 256)}
	var ready map[string]any
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
		t.Fatalf("ready control = %v err=%v", ready, err)
	}
	go func() {
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				close(client.frames)
				return
			}
			if messageType != websocket.BinaryMessage {
				continue
			}
			client.frames <- payload
		}
	}()
	return client
}

func (c *mediaTestClient) close() { _ = c.conn.Close() }

// energy returns the maximum absolute sample value of a PCM frame.
func energy(frame []int16) int {
	max := 0
	for _, sample := range frame {
		if value := int(sample); value < 0 {
			value = -value
		} else if value > max {
			max = value
		}
	}
	return max
}

// nextFrame returns the next binary frame within the timeout.
func (c *mediaTestClient) nextFrame(t *testing.T, timeout time.Duration) []byte {
	t.Helper()
	select {
	case frame, ok := <-c.frames:
		if !ok {
			t.Fatalf("media socket closed while waiting for a frame")
		}
		return frame
	case <-time.After(timeout):
		t.Fatalf("no media frame within %v", timeout)
		return nil
	}
}

// End-to-end WSS media: the authenticated socket becomes the call's client
// audio transport. Both directions of the mixer must flow — cellular frames
// reach the device only while the device sends host audio (the same host-live
// semantic as the WebRTC path), and device audio reaches the worker leg.
func TestWSMediaCarriesAudioBothDirections(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	waitForLineReady(t, stack, token)

	view := dialOutbound(t, stack, token, "ws-audio")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ws-audio", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ws-audio", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	client := dialMedia(t, stack, token, callID)
	defer client.close()

	// Host audio first: without a host frame the mixer must not leak leg
	// audio to the client (host-live gating, same as WebRTC OnTrack), so the
	// first frames are line-side silence.
	silencePCMU, err := voice.EncodePCMU(make([]int16, voice.FrameSamples))
	if err != nil {
		t.Fatalf("encode silence: %v", err)
	}
	first := client.nextFrame(t, 3*time.Second)
	if len(first) != voice.FrameSamples {
		t.Fatalf("frame size = %d, want %d", len(first), voice.FrameSamples)
	}
	mark := byte(0xD4)
	if first[0] == mark {
		t.Fatalf("leg audio leaked to the client before any host frame")
	}
	_ = silencePCMU

	// A real microphone streams continuously; mimic it so the shared jitter
	// buffer primes (the same host-live gate as the WebRTC RTP path, where
	// audio frames arrive in a steady flow from the first moment).
	hostSilence := make([]byte, voice.FrameSamples)
	for i := range hostSilence {
		hostSilence[i] = 0xFF // PCMU silence
	}
	stopStream := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopStream:
				return
			case <-ticker.C:
				if err := client.conn.WriteMessage(websocket.BinaryMessage, hostSilence); err != nil {
					return
				}
			}
		}
	}()
	defer close(stopStream)

	// Cellular -> device: the remote speaks in a continuous stream, so push
	// marked frames steadily; at least one must arrive mixed on the socket.
	// (Audio pushed before the host is live is deliberately drained — the
	// same anti-stale-audio rule as the WebRTC path.)
	full := make([]byte, voice.FrameSamples)
	for i := range full {
		full[i] = mark
	}
	stopCellular := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopCellular:
				return
			case <-ticker.C:
				if err := stack.fakes[0].pushCellular("ws-audio", full); err != nil {
					return
				}
			}
		}
	}()
	defer close(stopCellular)
	deadline := time.Now().Add(3 * time.Second)
	for {
		frame := client.nextFrame(t, time.Until(deadline))
		// The mixer applies its limiter gain, so assert on decoded energy
		// rather than an exact byte pattern.
		if pcm, err := voice.DecodePCMU(frame); err == nil && energy(pcm) > 400 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cellular audio never reached the ws client")
		}
	}

	// Device -> cellular: the worker socket must receive host audio (the
	// continuous mic stream above).
	deadline = time.Now().Add(3 * time.Second)
	for {
		frames := stack.fakes[0].hostFrames("ws-audio")
		for _, frame := range frames {
			if frame[0] == hostSilence[0] {
				goto hostAudioSeen
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("host audio never reached the worker leg")
		}
		time.Sleep(20 * time.Millisecond)
	}
hostAudioSeen:
}

// Conferences host audio over the same WSS transport: merging must never
// force the host back onto the (cellular-unreachable) ICE path.
func TestWSConferenceMediaCarriesAudio(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	stack.fakes[0].injectIncoming("call-ws1", "+8613800000001")
	stack.fakes[1].injectIncoming("call-ws2", "+8613800000002")
	stack.waitForCall(token, "line1:call-ws1")
	stack.waitForCall(token, "line2:call-ws2")
	for _, callID := range []string{"line1:call-ws1", "line2:call-ws2"} {
		if status := stack.jsonRequest(http.MethodPost, "/api/v2/calls/"+callID+"/answer", token, "ws-ans-"+callID, map[string]string{}, nil); status != http.StatusCreated {
			t.Fatalf("answer %s status = %d", callID, status)
		}
		waitForCallState(t, stack, token, callID, "active")
	}
	var conference map[string]any
	status := stack.jsonRequest(http.MethodPost, "/api/v2/conferences", token, "ws-conf-1",
		map[string][]string{"callIds": {"line1:call-ws1", "line2:call-ws2"}}, &conference)
	if status != http.StatusCreated {
		t.Fatalf("merge status = %d, want 201", status)
	}
	conferenceID, _ := conference["id"].(string)

	header := http.Header{"Authorization": []string{"Bearer " + token}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/conferences/" + conferenceID + "/media"
	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), url, header)
	if err != nil {
		t.Fatalf("conference media upgrade: %v", err)
	}
	defer conn.Close()
	var ready map[string]any
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
		t.Fatalf("ready control = %v err=%v", ready, err)
	}
	frames := make(chan []byte, 256)
	go func() {
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				close(frames)
				return
			}
			if messageType == websocket.BinaryMessage {
				frames <- payload
			}
		}
	}()
	next := func(timeout time.Duration) []byte {
		t.Helper()
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatalf("conference media socket closed")
			}
			return frame
		case <-time.After(timeout):
			t.Fatalf("no conference media frame within %v", timeout)
			return nil
		}
	}

	// Host mic stream (primes the shared jitter buffer).
	hostSilence := make([]byte, voice.FrameSamples)
	for i := range hostSilence {
		hostSilence[i] = 0xFF
	}
	stopStream := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopStream:
				return
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.BinaryMessage, hostSilence); err != nil {
					return
				}
			}
		}
	}()
	defer close(stopStream)

	// Mix-minus: each leg hears the host plus the OTHER leg. Push marked
	// audio on line1; it must reach the conference host socket.
	mark := byte(0xD4)
	full := make([]byte, voice.FrameSamples)
	for i := range full {
		full[i] = mark
	}
	stopCellular := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopCellular:
				return
			case <-ticker.C:
				if err := stack.fakes[0].pushCellular("call-ws1", full); err != nil {
					return
				}
			}
		}
	}()
	defer close(stopCellular)
	deadline := time.Now().Add(3 * time.Second)
	for {
		frame := next(time.Until(deadline))
		if pcm, err := voice.DecodePCMU(frame); err == nil && energy(pcm) > 400 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("conference leg audio never reached the ws host")
		}
	}
}

// Replacement sockets: when a new WS host replaces an old one, the old
// pump's exit (its socket closing) must never detach or kill the new host.
func TestWSMediaReplacementSurvivesOldPumpExit(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	view := dialOutbound(t, stack, token, "ws-replace")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ws-replace", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ws-replace", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	first := dialMedia(t, stack, token, callID)
	defer first.close()
	replacement := dialMedia(t, stack, token, callID)
	defer replacement.close()

	// Drop the superseded socket; its pump's defer must not touch the
	// replacement (the observed bug closed the CURRENT host).
	first.conn.Close()
	time.Sleep(100 * time.Millisecond)

	// The replacement must still carry mixed audio.
	hostSilence := make([]byte, voice.FrameSamples)
	for i := range hostSilence {
		hostSilence[i] = 0xFF
	}
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if err := replacement.conn.WriteMessage(websocket.BinaryMessage, hostSilence); err != nil {
				return
			}
		}
	}()
	mark := byte(0xD4)
	full := make([]byte, voice.FrameSamples)
	for i := range full {
		full[i] = mark
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := stack.fakes[0].pushCellular("ws-replace", full); err != nil {
					return
				}
			}
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		frame := replacement.nextFrame(t, time.Until(deadline))
		if pcm, err := voice.DecodePCMU(frame); err == nil && energy(pcm) > 400 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replacement socket lost audio after old socket closed")
		}
	}
}

// Host-lost grace: dropping the socket starts the disconnect grace; a
// re-attach before the deadline must cancel the pending hangup (the
// restored callback) so the call survives the original grace expiry.
func TestWSMediaReattachBeforeGraceSurvives(t *testing.T) {
	stack := newTestStack(t)
	if err := stack.store.SetSetting(context.Background(), "disconnect_grace_seconds", "5"); err != nil {
		t.Fatalf("set grace: %v", err)
	}
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	view := dialOutbound(t, stack, token, "ws-grace")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ws-grace", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ws-grace", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	first := dialMedia(t, stack, token, callID)
	first.conn.Close() // host lost: grace starts (5s)
	time.Sleep(200 * time.Millisecond)

	// Re-attach well inside the grace window.
	replacement := dialMedia(t, stack, token, callID)
	defer replacement.close()

	// Build-48 recovery clears only on FRESH-MEDIA proof, not the attach
	// alone: stream real host PCMU frames on the replacement socket before
	// the deadline. (A handful of 20 ms frames is enough to fire the
	// fresh-host callback.)
	hostFrame := make([]byte, voice.FrameSamples)
	for i := range hostFrame {
		hostFrame[i] = 0xFF
	}
	stopFresh := make(chan struct{})
	streamWSHost(stopFresh, replacement.conn, hostFrame)
	defer close(stopFresh)
	time.Sleep(300 * time.Millisecond)

	// Past the original grace deadline the call must still be active.
	time.Sleep(5 * time.Second)
	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "active" {
		t.Fatalf("call ended despite timely re-attach with fresh media (state=%v)", stored["state"])
	}
}

// Stall/resume: when the client's send stream pauses, the mixer's play
// cursor advances past the synthetic timestamps; on resume the session must
// re-sync and carry audio again instead of dropping every frame forever.
func TestWSMediaStallThenResume(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	view := dialOutbound(t, stack, token, "ws-stall")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ws-stall", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ws-stall", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	client := dialMedia(t, stack, token, callID)
	defer client.close()

	hostSilence := make([]byte, voice.FrameSamples)
	for i := range hostSilence {
		hostSilence[i] = 0xFF
	}
	var streaming atomic.Bool
	streaming.Store(true)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if !streaming.Load() {
				continue
			}
			if err := client.conn.WriteMessage(websocket.BinaryMessage, hostSilence); err != nil {
				return
			}
		}
	}()

	mark := byte(0xD4)
	full := make([]byte, voice.FrameSamples)
	for i := range full {
		full[i] = mark
	}
	pushMarked := func() {
		stop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					if err := stack.fakes[0].pushCellular("ws-stall", full); err != nil {
						return
					}
				}
			}
		}()
		deadline := time.Now().Add(2 * time.Second)
		for {
			frame := client.nextFrame(t, time.Until(deadline))
			if pcm, err := voice.DecodePCMU(frame); err == nil && energy(pcm) > 400 {
				close(stop)
				return
			}
			if time.Now().After(deadline) {
				close(stop)
				t.Fatalf("marked cellular audio never arrived")
			}
		}
	}

	// Baseline: audio flows while both streams run.
	pushMarked()

	// Stall the client send stream past several play-cursor windows.
	streaming.Store(false)
	time.Sleep(900 * time.Millisecond)
	streaming.Store(true)

	// After resume, audio must flow again (re-prime/drop-stale resync).
	pushMarked()
}

// Guardrails: unauthenticated upgrades, non-owner devices and ended calls are
// rejected with plain HTTP errors before the socket opens.
func TestWSMediaAuthorizationGuards(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	tokenA, _ := stack.enroll(keyID, secret, "Device A")
	keyB, secretB := stack.createKey(true)
	tokenB, _ := stack.enroll(keyB, secretB, "Device B")

	view := dialOutbound(t, stack, tokenA, "ws-guard")
	callID, _ := view["id"].(string)

	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/calls/" + callID + "/media"
	// No credentials: the upgrade must never happen.
	if _, response, err := websocket.DefaultDialer.Dial(url, nil); err == nil {
		t.Fatalf("unauthenticated media upgrade succeeded: %v", response)
	} else if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %v, want 401", response)
	}
	// Wrong device (line-authorized but not the call owner).
	if _, response, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": []string{"Bearer " + tokenB}}); err == nil {
		t.Fatalf("non-owner media upgrade succeeded")
	} else if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner status = %v, want 403", response)
	}
	// Ended call: hang up, then the media route reports the call gone.
	status, _ := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/hangup", tokenA, "ws-guard-hang", nil)
	if status != http.StatusNoContent {
		t.Fatalf("hangup status = %d", status)
	}
	waitForCallState(t, stack, tokenA, callID, "ended")
	if _, response, err := websocket.DefaultDialer.Dial(url, http.Header{"Authorization": []string{"Bearer " + tokenA}}); err == nil {
		t.Fatalf("ended-call media upgrade succeeded")
	} else if response == nil || response.StatusCode != http.StatusNotFound {
		t.Fatalf("ended-call status = %v, want 404", response)
	}
}

// The ICE payload must advertise both transports so new clients can prefer
// WSS while older clients (which ignore the field) keep using WebRTC.
func TestICEAdvertisesMediaTransports(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	view := dialOutbound(t, stack, token, "ws-cap")
	callID, _ := view["id"].(string)

	status, payload := stack.request(http.MethodGet, "/api/v2/calls/"+callID+"/ice", token, "", nil)
	if status != http.StatusOK {
		t.Fatalf("ice status = %d", status)
	}
	var body struct {
		Policy          string   `json:"policy"`
		MediaTransports []string `json:"mediaTransports"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("ice decode: %v", err)
	}
	joined := strings.Join(body.MediaTransports, ",")
	if !strings.Contains(joined, "ws") || !strings.Contains(joined, "ice") {
		t.Fatalf("mediaTransports = %v, want ice+ws", body.MediaTransports)
	}
}

// A hangup while the media socket is attached must close the socket and end
// the call exactly once (the terminal-overwrite regression stays fixed when
// media flows over WSS).
func TestWSMediaTerminalCleanup(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	view := dialOutbound(t, stack, token, "ws-term")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ws-term", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ws-term", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	client := dialMedia(t, stack, token, callID)
	defer client.close()

	status, _ := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/hangup", token, "ws-term-hang", nil)
	if status != http.StatusNoContent {
		t.Fatalf("hangup status = %d", status)
	}
	// The socket must close as the session tears down.
	select {
	case _, ok := <-client.frames:
		if ok {
			t.Fatalf("unexpected frame after hangup")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("media socket stayed open after hangup")
	}
	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "ended" {
		t.Fatalf("stored state = %v, want ended", stored["state"])
	}
	if _, ok := stored["connectedAt"].(float64); !ok {
		t.Fatalf("stored call lost connectedAt: %v", stored)
	}
}

// waitForLineReady blocks until the unified runtime has completed its first
// worker status refresh, so a dial can never race the initial
// "line unreachable" window.
func waitForLineReady(t *testing.T, stack *testStack, token string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, payload := stack.request(http.MethodGet, "/api/v2/lines", token, "", nil)
		if status == http.StatusOK {
			var lines []map[string]any
			if json.Unmarshal(payload, &lines) == nil && len(lines) > 0 {
				allOnline := true
				for _, line := range lines {
					online, _ := line["online"].(bool)
					allOnline = allOnline && online
				}
				if allOnline {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("lines never came online")
}
