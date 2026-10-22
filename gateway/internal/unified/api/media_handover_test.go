package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/runtime"
	"callrelay/gateway/internal/voice"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

// These tests exercise the real P2P handover: a genuine pion peer connection
// on loopback, the detached echo probe, ready-first commit onto a live WSS
// call, both directions of audio across the adopted peer, WSS fallback with
// grace restored, and the rejection paths that must never destroy the
// healthy host.

// markedPCMU returns a full 20 ms PCMU frame whose decoded energy clearly
// exceeds silence (same marker pattern as the ws media tests).
func markedPCMU(mark byte) []byte {
	frame := make([]byte, voice.FrameSamples)
	for i := range frame {
		frame[i] = mark
	}
	return frame
}

// streamPCMUToWorker pushes the same marked frame toward the worker leg at
// 20 ms until stop closes.
func streamPCMUToWorker(stop <-chan struct{}, fake *fakeWorker, callID string, frame []byte) {
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := fake.pushCellular(callID, frame); err != nil {
					return
				}
			}
		}
	}()
}

// streamWSHost writes one PCMU frame per 20 ms on an attached WSS media
// socket until stop closes (mirrors a continuously streaming mic).
func streamWSHost(stop <-chan struct{}, conn *websocket.Conn, frame []byte) {
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
					return
				}
			}
		}
	}()
}

// expectEnergyFrame fails unless a frame with decoded energy arrives within
// the deadline. read pulls payloads from a channel fed by a track/socket
// reader.
func expectEnergyFrame(t *testing.T, read func(time.Duration) ([]byte, bool), timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		frame, ok := read(time.Until(deadline))
		if !ok {
			t.Fatalf("media stream closed while waiting for audio")
		}
		if len(frame) == 0 {
			continue
		}
		if pcm, err := voice.DecodePCMU(frame); err == nil && energy(pcm) > 400 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no audio frame within %v", timeout)
		}
	}
}

// waitPCConnected blocks until the peer reports Connected (or fails).
func waitPCConnected(t *testing.T, pc *pion.PeerConnection) {
	t.Helper()
	if pc.ConnectionState() == pion.PeerConnectionStateConnected {
		return
	}
	changes := make(chan pion.PeerConnectionState, 16)
	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) { changes <- state })
	for {
		select {
		case state := <-changes:
			if state == pion.PeerConnectionStateConnected {
				return
			}
			if state == pion.PeerConnectionStateFailed || state == pion.PeerConnectionStateClosed {
				t.Fatalf("probe peer entered %s", state)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("probe peer never connected (state=%s)", pc.ConnectionState())
		}
	}
}

// handoverProbe attaches a real probe peer and completes ICE on loopback so
// it is a genuinely connected, measurable candidate. It returns the peer,
// the local send track, a channel of audio payloads received from the
// gateway, the probe data channel and a channel of its echo payloads. The
// client creates the callrelay-probe data channel in its offer, exactly like
// the iOS app.
func handoverProbe(t *testing.T, stack *testStack, token, callID string) (*pion.PeerConnection, *pion.TrackLocalStaticRTP, chan []byte, *pion.DataChannel, chan []byte) {
	t.Helper()
	mediaEngine := &pion.MediaEngine{}
	if err := mediaEngine.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{
			MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1,
		},
		PayloadType: 0,
	}, pion.RTPCodecTypeAudio); err != nil {
		t.Fatalf("codec: %v", err)
	}
	api := pion.NewAPI(pion.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("pc: %v", err)
	}
	track, err := pion.NewTrackLocalStaticRTP(
		pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		"audio", "probe")
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if _, err := pc.AddTransceiverFromTrack(track, pion.RTPTransceiverInit{
		Direction: pion.RTPTransceiverDirectionSendrecv,
	}); err != nil {
		t.Fatalf("transceiver: %v", err)
	}
	received := make(chan []byte, 512)
	pc.OnTrack(func(remote *pion.TrackRemote, _ *pion.RTPReceiver) {
		go func() {
			for {
				packet, _, readErr := remote.ReadRTP()
				if readErr != nil {
					return
				}
				payload := make([]byte, len(packet.Payload))
				copy(payload, packet.Payload)
				select {
				case received <- payload:
				default:
				}
			}
		}()
	})
	// The measurement data channel: the gateway echoes payloads exactly.
	probeEcho := make(chan []byte, 128)
	channel, err := pc.CreateDataChannel(media.ProbeEchoLabel, nil)
	if err != nil {
		t.Fatalf("data channel: %v", err)
	}
	channel.OnMessage(func(message pion.DataChannelMessage) {
		payload := make([]byte, len(message.Data))
		copy(payload, message.Data)
		select {
		case probeEcho <- payload:
		default:
		}
	})
	opened := make(chan struct{})
	channel.OnOpen(func() { close(opened) })
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("local: %v", err)
	}
	<-pion.GatheringCompletePromise(pc)
	local := pc.LocalDescription()
	if local == nil {
		t.Fatalf("missing local description")
	}
	body, _ := json.Marshal(map[string]string{"sdp": local.SDP})
	status, payload := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe", token, "", body)
	if status != http.StatusCreated {
		t.Fatalf("probe attach status = %d body=%s", status, payload)
	}
	var answer struct {
		SDP string `json:"sdp"`
	}
	if err := json.Unmarshal(payload, &answer); err != nil || answer.SDP == "" {
		t.Fatalf("probe answer: %v %s", err, payload)
	}
	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatalf("remote: %v", err)
	}
	waitPCConnected(t, pc)
	// SCTP establishment follows ICE/DTLS; pings before the channel opens
	// would be dropped.
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatalf("probe echo channel never opened (state=%s)", channel.ReadyState())
	}
	return pc, track, received, channel, probeEcho
}

// expectEcho fails unless an exact data-channel echo with the wanted tag
// arrives within the timeout.
func expectEcho(t *testing.T, echoes chan []byte, tag int64, timeout time.Duration) {
	t.Helper()
	want, _ := json.Marshal(map[string]any{"type": "ping", "tag": tag})
	deadline := time.Now().Add(timeout)
	for {
		select {
		case payload := <-echoes:
			if string(payload) == string(want) {
				return
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("no echo for tag %d within %v", tag, timeout)
		}
	}
}

// sendEchoPing fires one measurement ping on the probe data channel.
func sendEchoPing(t *testing.T, channel *pion.DataChannel, tag int64) {
	t.Helper()
	if channel.ReadyState() != pion.DataChannelStateOpen {
		t.Fatalf("probe echo channel is not open (state=%s)", channel.ReadyState())
	}
	payload, _ := json.Marshal(map[string]any{"type": "ping", "tag": tag})
	if err := channel.SendText(string(payload)); err != nil {
		t.Fatalf("echo ping: %v", err)
	}
}

// mediaClientReader adapts a mediaTestClient's frame channel to the
// expectEnergyFrame API (the dialMedia reader goroutine closes the channel
// when the socket dies).
func mediaClientReader(client *mediaTestClient) func(time.Duration) ([]byte, bool) {
	return func(timeout time.Duration) ([]byte, bool) {
		select {
		case frame, ok := <-client.frames:
			return frame, ok
		case <-time.After(timeout):
			return nil, false
		}
	}
}

// trackReader adapts a probe payload channel to the expectEnergyFrame API.
func trackReader(ch chan []byte) func(time.Duration) ([]byte, bool) {
	return func(timeout time.Duration) ([]byte, bool) {
		select {
		case frame, ok := <-ch:
			return frame, ok
		case <-time.After(timeout):
			return nil, false
		}
	}
}

// streamTrackRTP writes one PCMU frame per 20 ms on the client's send track
// until stop closes, mimicking a real microphone.
func streamTrackRTP(stop <-chan struct{}, track *pion.TrackLocalStaticRTP, frame []byte) {
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		var sequence uint16
		var timestamp uint32
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_ = track.WriteRTP(&rtp.Packet{
					Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: sequence, Timestamp: timestamp},
					Payload: frame,
				})
				sequence++
				timestamp += voice.FrameSamples
			}
		}
	}()
}

// mediaTransport returns the call view's mediaTransport field ("" absent).
func mediaTransport(t *testing.T, stack *testStack, token, callID string) string {
	t.Helper()
	stored := fetchCall(t, stack, token, callID)
	value, _ := stored["mediaTransport"].(string)
	return value
}

// Full handover lifecycle over real peer connections: healthy WSS call,
// echo-measured probe, ready-first commit, both audio directions over the
// adopted peer, ICE drop → WSS fallback with grace restored, and a second
// commit proving repeatable bidirectional switching.
func TestProbeCommitHandoverCarriesAudio(t *testing.T) {
	stack := newTestStack(t)
	if err := stack.store.SetSetting(t.Context(), "disconnect_grace_seconds", "5"); err != nil {
		t.Fatalf("set grace: %v", err)
	}
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	waitForLineReady(t, stack, token)
	// Routing/handover assertion uses constant-value marked PCMU frames
	// (pure DC, not speech); pin THIS device's SpeexDSP denoise off (the
	// build-49 default is already OFF; the explicit rows keep the test
	// independent of any future default change) so level assertions measure
	// routing, not suppression.
	for _, suffix := range []string{":audio_denoise_uplink", ":audio_denoise_downlink"} {
		if err := stack.store.SetSetting(t.Context(), "device:"+stack.deviceID+suffix, "0"); err != nil {
			t.Fatal(err)
		}
	}

	view := dialOutbound(t, stack, token, "handover")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("handover", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("handover", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	// Phase 1: healthy WSS media in both directions.
	wss := dialMedia(t, stack, token, callID)
	defer wss.close()
	hostSilence := markedPCMU(0xFF)
	stopWSSHost := make(chan struct{})
	streamWSHost(stopWSSHost, wss.conn, hostSilence)
	stopCell := make(chan struct{})
	streamPCMUToWorker(stopCell, stack.fakes[0], "handover", markedPCMU(0xD4))
	expectEnergyFrame(t, mediaClientReader(wss), 3*time.Second)
	if transport := mediaTransport(t, stack, token, callID); transport != "ws" {
		t.Fatalf("mediaTransport = %q, want ws", transport)
	}

	// Phase 2: detached probe with a real connected peer. The measurement
	// data channel must echo pings exactly (the candidate measurement path
	// an app uses without streaming microphone audio)...
	pc, track, echo, probeDC, probeEcho := handoverProbe(t, stack, token, callID)
	defer pc.Close()
	sendEchoPing(t, probeDC, 1)
	expectEcho(t, probeEcho, 1, 3*time.Second)
	// ...and the audio echo path still reflects streamed frames for clients
	// that do stream (mixer echo rule, kept for non-mobile clients).
	stopProbeHost := make(chan struct{})
	streamTrackRTP(stopProbeHost, track, markedPCMU(0xD4))
	expectEnergyFrame(t, trackReader(echo), 3*time.Second)

	// The healthy WSS path must be untouched by probing and echoing.
	expectEnergyFrame(t, mediaClientReader(wss), 3*time.Second)
	if transport := mediaTransport(t, stack, token, callID); transport != "ws" {
		t.Fatalf("probe disturbed the live transport: %q", transport)
	}

	// Phase 3: commit promotes the verified peer; the WSS socket is DEMOTED
	// into its family standby slot (build 63: the displaced relay stays
	// attached and warm — it is never silently retired) and audio flows
	// both ways over the adopted peer.
	commit := "/api/v2/calls/" + callID + "/media/probe/commit"
	status, payload := stack.request(http.MethodPost, commit, token, "ho-commit-1", nil)
	if status != http.StatusOK {
		t.Fatalf("commit status = %d body=%s", status, payload)
	}
	if stack.manager.HasProbe(callID) {
		t.Fatalf("probe session outlived its commit")
	}
	close(stopWSSHost)
	if transport := mediaTransport(t, stack, token, callID); transport != "ice" {
		t.Fatalf("mediaTransport = %q, want ice after commit", transport)
	}
	// The demoted WSS standby stays attached and keeps receiving the
	// mirrored downlink mix (the standby-mirror contract) — it must NOT be
	// closed by the commit.
	select {
	case _, ok := <-wss.frames:
		if !ok {
			t.Fatalf("demoted WSS standby was closed by the commit")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("demoted WSS standby received no mirrored mix after commit")
	}
	// Production convergence: the client retires its replaced relay socket;
	// the server pump then detaches the standby and the socket closes.
	wss.close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case _, ok := <-wss.frames:
			if !ok {
				goto wssClosed
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("server did not converge the retired WSS standby socket")
		}
	}
wssClosed:
	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "active" {
		t.Fatalf("commit disturbed call state: %v", stored["state"])
	}
	// The measurement channel keeps echoing on the adopted peer, so the
	// direct path stays measurable for the whole call.
	sendEchoPing(t, probeDC, 2)
	expectEcho(t, probeEcho, 2, 3*time.Second)
	// Cellular -> client over the adopted peer.
	expectEnergyFrame(t, trackReader(echo), 3*time.Second)
	// Client -> worker over the adopted peer (host audio reaches the leg).
	workerBaseline := len(stack.fakes[0].hostFrames("handover"))
	deadline = time.Now().Add(3 * time.Second)
	for {
		frames := stack.fakes[0].hostFrames("handover")
		for _, frame := range frames[workerBaseline:] {
			if pcm, err := voice.DecodePCMU(frame); err == nil && energy(pcm) > 400 {
				goto hostAudioSeen
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("host audio never reached the worker leg over the adopted peer")
		}
		time.Sleep(20 * time.Millisecond)
	}
hostAudioSeen:

	// Phase 4: the direct candidate drops; the call must enter host-lost
	// grace (no hangup) and WSS fallback restores audio, cancelling grace.
	close(stopProbeHost)
	close(stopCell)
	pc.Close()
	time.Sleep(300 * time.Millisecond)
	fallback := dialMedia(t, stack, token, callID)
	defer fallback.close()
	stopFallbackHost := make(chan struct{})
	streamWSHost(stopFallbackHost, fallback.conn, hostSilence)
	defer close(stopFallbackHost)
	stopCell2 := make(chan struct{})
	streamPCMUToWorker(stopCell2, stack.fakes[0], "handover", markedPCMU(0xD4))
	defer close(stopCell2)
	expectEnergyFrame(t, mediaClientReader(fallback), 3*time.Second)
	if transport := mediaTransport(t, stack, token, callID); transport != "ws" {
		t.Fatalf("mediaTransport = %q, want ws after fallback", transport)
	}
	// Past the original 5 s grace deadline the call must still be active.
	time.Sleep(5 * time.Second)
	stored = fetchCall(t, stack, token, callID)
	if stored["state"] != "active" {
		t.Fatalf("call ended despite timely WSS fallback (state=%v)", stored["state"])
	}

	// Phase 5: handover is repeatable — probe and commit again.
	pc2, track2, echo2, dc2, echoDC2 := handoverProbe(t, stack, token, callID)
	defer pc2.Close()
	sendEchoPing(t, dc2, 3)
	expectEcho(t, echoDC2, 3, 3*time.Second)
	stopProbe2 := make(chan struct{})
	streamTrackRTP(stopProbe2, track2, markedPCMU(0xD4))
	defer close(stopProbe2)
	status, payload = stack.request(http.MethodPost, commit, token, "ho-commit-2", nil)
	if status != http.StatusOK {
		t.Fatalf("second commit status = %d body=%s", status, payload)
	}
	if transport := mediaTransport(t, stack, token, callID); transport != "ice" {
		t.Fatalf("mediaTransport = %q, want ice after second commit", transport)
	}
	expectEnergyFrame(t, trackReader(echo2), 3*time.Second)
	stored = fetchCall(t, stack, token, callID)
	if stored["state"] != "active" {
		t.Fatalf("second commit disturbed call state: %v", stored["state"])
	}
}

// A candidate that never connected must be refused without touching the
// healthy WSS host: explicit 502, probe dropped, previous path alive.
func TestProbeCommitRejectsUnconnectedCandidate(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	waitForLineReady(t, stack, token)

	view := dialOutbound(t, stack, token, "ho-reject")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ho-reject", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ho-reject", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	wss := dialMedia(t, stack, token, callID)
	defer wss.close()
	stopWSSHost := make(chan struct{})
	streamWSHost(stopWSSHost, wss.conn, markedPCMU(0xFF))
	defer close(stopWSSHost)
	stopCell := make(chan struct{})
	streamPCMUToWorker(stopCell, stack.fakes[0], "ho-reject", markedPCMU(0xD4))
	defer close(stopCell)
	expectEnergyFrame(t, mediaClientReader(wss), 3*time.Second)

	// Attach a probe but never apply the answer: the gateway peer can never
	// connect. The offer itself is well-formed, so negotiation succeeds.
	pc, offer := probeClient(t)
	defer pc.Close()
	body, _ := json.Marshal(map[string]string{"sdp": offer})
	status, payload := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe", token, "", body)
	if status != http.StatusCreated {
		t.Fatalf("probe attach status = %d body=%s", status, payload)
	}
	if !stack.manager.HasProbe(callID) {
		t.Fatalf("probe missing after attach")
	}

	commit := "/api/v2/calls/" + callID + "/media/probe/commit"
	status, payload = stack.request(http.MethodPost, commit, token, "ho-reject-commit", nil)
	if status != http.StatusBadGateway {
		t.Fatalf("commit of unconnected probe status = %d body=%s, want 502", status, payload)
	}
	if stack.manager.HasProbe(callID) {
		t.Fatalf("rejected probe was not dropped")
	}
	// The previous WSS path keeps carrying audio untouched.
	expectEnergyFrame(t, mediaClientReader(wss), 3*time.Second)
	if transport := mediaTransport(t, stack, token, callID); transport != "ws" {
		t.Fatalf("failed commit disturbed the live transport: %q", transport)
	}
	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "active" {
		t.Fatalf("failed commit disturbed call state: %v", stored["state"])
	}
}

// A merged conference leg must refuse per-call promotion loudly (409) — a
// commit would double-attach the worker's single-owner media socket — while
// conference audio keeps flowing.
func TestProbeCommitRejectedInConference(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	stack.fakes[0].injectIncoming("ho-conf1", "+8613800000001")
	stack.fakes[1].injectIncoming("ho-conf2", "+8613800000002")
	stack.waitForCall(token, "line1:ho-conf1")
	stack.waitForCall(token, "line2:ho-conf2")
	// The fake worker emits the post-answer call.updated on a non-blocking
	// send, which can drop under load; production workers re-sync via
	// polling, so mirror that by re-emitting the active record while waiting.
	for _, callID := range []string{"line1:ho-conf1", "line2:ho-conf2"} {
		if status := stack.jsonRequest(http.MethodPost, "/api/v2/calls/"+callID+"/answer", token, "ho-conf-ans-"+callID, map[string]string{}, nil); status != http.StatusCreated {
			t.Fatalf("answer %s status = %d", callID, status)
		}
	}
	waitActive := func(callID, workerCallID string, fake *fakeWorker) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			status, payload := stack.request(http.MethodGet, "/api/v2/calls/"+callID, token, "", nil)
			if status == http.StatusOK {
				var view map[string]any
				if json.Unmarshal(payload, &view) == nil && view["state"] == "active" {
					return
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("call %s did not reach state active", callID)
			}
			fake.mu.Lock()
			record := fake.calls[workerCallID]
			fake.mu.Unlock()
			record.State = "active"
			if record.ConnectedAt == nil {
				now := time.Now().UnixMilli()
				record.ConnectedAt = &now
			}
			fake.injectWorkerEvent("call.updated", record)
			time.Sleep(300 * time.Millisecond)
		}
	}
	waitActive("line1:ho-conf1", "ho-conf1", stack.fakes[0])
	waitActive("line2:ho-conf2", "ho-conf2", stack.fakes[1])
	var conference map[string]any
	status := stack.jsonRequest(http.MethodPost, "/api/v2/conferences", token, "ho-conf-merge",
		map[string][]string{"callIds": {"line1:ho-conf1", "line2:ho-conf2"}}, &conference)
	if status != http.StatusCreated {
		t.Fatalf("merge status = %d, want 201", status)
	}
	conferenceID, _ := conference["id"].(string)

	// Conference media over WSS, flowing.
	header := map[string][]string{"Authorization": {"Bearer " + token}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/conferences/" + conferenceID + "/media"
	conn, _, err := websocket.DefaultDialer.DialContext(t.Context(), url, header)
	if err != nil {
		t.Fatalf("conference media upgrade: %v", err)
	}
	defer conn.Close()
	var ready map[string]any
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&ready); err != nil || ready["type"] != "ready" {
		t.Fatalf("ready control = %v err=%v", ready, err)
	}
	confFrames := make(chan []byte, 256)
	go func() {
		for {
			messageType, payload, readErr := conn.ReadMessage()
			if readErr != nil {
				close(confFrames)
				return
			}
			if messageType == websocket.BinaryMessage {
				confFrames <- payload
			}
		}
	}()
	confRead := func(timeout time.Duration) ([]byte, bool) {
		select {
		case frame, ok := <-confFrames:
			return frame, ok
		case <-time.After(timeout):
			return nil, false
		}
	}
	stopConfHost := make(chan struct{})
	streamWSHost(stopConfHost, conn, markedPCMU(0xFF))
	defer close(stopConfHost)
	stopConfCell := make(chan struct{})
	streamPCMUToWorker(stopConfCell, stack.fakes[0], "ho-conf1", markedPCMU(0xD4))
	defer close(stopConfCell)
	expectEnergyFrame(t, confRead, 3*time.Second)

	// Transport truth for a merged leg: the leg's audio is hosted by the
	// CONFERENCE session, so its view must report that session's "ws".
	if transport := mediaTransport(t, stack, token, "line1:ho-conf1"); transport != "ws" {
		t.Fatalf("conferenced leg mediaTransport = %q, want ws (conference session)", transport)
	}

	// Detached probe attach stays harmless on a conferenced call, but commit
	// must be refused with an explicit 409 and the probe dropped.
	pc, track, _, _, _ := handoverProbe(t, stack, token, "line1:ho-conf1")
	defer pc.Close()
	stopProbe := make(chan struct{})
	streamTrackRTP(stopProbe, track, markedPCMU(0xD4))
	defer close(stopProbe)

	commit := "/api/v2/calls/line1:ho-conf1/media/probe/commit"
	status, payload := stack.request(http.MethodPost, commit, token, "ho-conf-commit", nil)
	if status != http.StatusConflict {
		t.Fatalf("conference commit status = %d body=%s, want 409", status, payload)
	}
	if stack.manager.HasProbe("line1:ho-conf1") {
		t.Fatalf("conference-rejected probe was not dropped")
	}
	// The per-call media reattach the commit guard mirrors must also be
	// refused (it would steal the conferenced leg's worker socket).
	reattachURL := "ws" + stack.server.URL[len("http"):] + "/api/v2/calls/line1:ho-conf1/media"
	if _, response, err := websocket.DefaultDialer.Dial(reattachURL, http.Header{"Authorization": []string{"Bearer " + token}}); err == nil {
		t.Fatalf("conferenced per-call media reattach succeeded")
	} else if response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("conferenced reattach status = %v, want 409", response)
	}
	expectEnergyFrame(t, confRead, 3*time.Second)
}

// A hangup racing the commit must win cleanly: exactly one terminal
// outcome, no resurrected media session, no surviving probe.
func TestProbeCommitRacingHangupNoResurrection(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	waitForLineReady(t, stack, token)

	view := dialOutbound(t, stack, token, "ho-termrace")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ho-termrace", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ho-termrace", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	pc, track, _, _, _ := handoverProbe(t, stack, token, callID)
	defer pc.Close()
	stopProbe := make(chan struct{})
	streamTrackRTP(stopProbe, track, markedPCMU(0xD4))
	defer close(stopProbe)

	commit := "/api/v2/calls/" + callID + "/media/probe/commit"
	type result struct {
		status int
		body   string
	}
	commitResult := make(chan result, 1)
	go func() {
		status, payload := stack.request(http.MethodPost, commit, token, "ho-termrace-commit", nil)
		commitResult <- result{status, string(payload)}
	}()
	if status, _ := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/hangup", token, "ho-termrace-hang", nil); status != http.StatusNoContent {
		t.Fatalf("hangup status = %d", status)
	}
	waitForCallState(t, stack, token, callID, "ended")
	commitDone := <-commitResult
	if commitDone.status != http.StatusOK && commitDone.status != http.StatusNotFound {
		t.Fatalf("racing commit status = %d body=%s, want 200 or 404", commitDone.status, commitDone.body)
	}
	// The call ended exactly once and no media session survived to
	// resurrect it.
	if stack.manager.HasProbe(callID) {
		t.Fatalf("probe survived the terminal race")
	}
	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "ended" {
		t.Fatalf("call state = %v, want ended", stored["state"])
	}
	if _, ok := stored["mediaTransport"]; ok {
		t.Fatalf("media session resurrected after terminal race: %v", stored)
	}
}

// Commit racing a WSS re-attach must leave exactly one live PRIMARY host:
// the session is never hostless, the loser transport is demoted into its
// family standby slot (build 63 — kept warm, converged closed when the
// client retires it) and the call keeps flowing audio on the winner.
func TestProbeCommitConcurrentReattachInvariant(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	waitForLineReady(t, stack, token)

	view := dialOutbound(t, stack, token, "ho-inv")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ho-inv", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ho-inv", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	original := dialMedia(t, stack, token, callID)
	defer original.close()

	pc, track, echo, _, _ := handoverProbe(t, stack, token, callID)
	defer pc.Close()
	stopProbe := make(chan struct{})
	streamTrackRTP(stopProbe, track, markedPCMU(0xD4))
	defer close(stopProbe)

	// Start the commit, then immediately attach a replacement WSS host.
	commit := "/api/v2/calls/" + callID + "/media/probe/commit"
	type result struct {
		status int
		body   string
	}
	commitResult := make(chan result, 1)
	go func() {
		status, payload := stack.request(http.MethodPost, commit, token, "ho-inv-commit", nil)
		commitResult <- result{status, string(payload)}
	}()
	time.Sleep(50 * time.Millisecond)
	replacement := dialMedia(t, stack, token, callID)
	defer replacement.close()
	commitDone := <-commitResult
	if commitDone.status != http.StatusOK && commitDone.status != http.StatusBadGateway {
		t.Fatalf("racing commit status = %d body=%s", commitDone.status, commitDone.body)
	}

	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "active" {
		t.Fatalf("call state = %v, want active", stored["state"])
	}
	transport := mediaTransport(t, stack, token, callID)
	// Build 63 contract: exactly one PRIMARY host; the loser's socket is
	// DEMOTED into its family standby slot (kept warm, never the host mix),
	// not closed. The client retires its replaced socket and the server
	// must converge it closed in whatever role it now holds.
	original.close()
	originalClosed := false
	deadline := time.Now().Add(3 * time.Second)
	for !originalClosed && time.Now().Before(deadline) {
		select {
		case _, ok := <-original.frames:
			if !ok {
				originalClosed = true
			}
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !originalClosed {
		t.Fatalf("client-retired original socket was not converged by the server")
	}
	switch transport {
	case "ice":
		// The adoption won; the replacement WSS socket was DEMOTED to the
		// standby role by the adoption's own replace — it must stay
		// attached and keep receiving the mirrored downlink (warm standby),
		// while the peer carries audio.
		select {
		case _, ok := <-replacement.frames:
			if !ok {
				t.Fatalf("demoted replacement standby was closed after ICE won")
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("demoted replacement standby received no mirror after ICE won")
		}
		stopCell := make(chan struct{})
		streamPCMUToWorker(stopCell, stack.fakes[0], "ho-inv", markedPCMU(0xD4))
		defer close(stopCell)
		expectEnergyFrame(t, trackReader(echo), 3*time.Second)
	case "ws":
		// The replacement won; the gateway closed the adopted peer (the
		// client observes the drop asynchronously) and the new socket
		// carries audio.
		dropDeadline := time.Now().Add(10 * time.Second)
		for pc.ConnectionState() == pion.PeerConnectionStateConnected && time.Now().Before(dropDeadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if pc.ConnectionState() == pion.PeerConnectionStateConnected {
			t.Fatalf("losing peer was not closed after WSS won")
		}
		stopHost := make(chan struct{})
		streamWSHost(stopHost, replacement.conn, markedPCMU(0xFF))
		defer close(stopHost)
		stopCell := make(chan struct{})
		streamPCMUToWorker(stopCell, stack.fakes[0], "ho-inv", markedPCMU(0xD4))
		defer close(stopCell)
		expectEnergyFrame(t, mediaClientReader(replacement), 3*time.Second)
	default:
		t.Fatalf("mediaTransport = %q, want ice or ws (one live host)", transport)
	}
}

// The atomic claim: many concurrent commits on one connected probe produce
// exactly one winner; every loser gets an explicit 404 and the winner's peer
// carries audio — no simultaneous adoption, no torn-down host.
func TestProbeCommitConcurrentOnlyOneWins(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	waitForLineReady(t, stack, token)

	view := dialOutbound(t, stack, token, "ho-race")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ho-race", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ho-race", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	pc, track, echo, _, _ := handoverProbe(t, stack, token, callID)
	defer pc.Close()
	stopProbe := make(chan struct{})
	streamTrackRTP(stopProbe, track, markedPCMU(0xD4))
	defer close(stopProbe)

	commit := "/api/v2/calls/" + callID + "/media/probe/commit"
	results := make(chan int, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, _ := stack.request(http.MethodPost, commit, token, "", nil)
			results <- status
		}()
	}
	wg.Wait()
	winners, losers := 0, 0
	for i := 0; i < 8; i++ {
		switch status := <-results; status {
		case http.StatusOK:
			winners++
		case http.StatusNotFound:
			losers++
		default:
			t.Fatalf("unexpected concurrent commit status = %d", status)
		}
	}
	if winners != 1 || losers != 7 {
		t.Fatalf("concurrent commits: winners=%d losers=%d, want 1/7", winners, losers)
	}
	if stack.manager.HasProbe(callID) {
		t.Fatalf("probe session outlived the winning commit")
	}
	// The winner's adopted peer carries the call (and the host stream keeps
	// it live): cellular audio must reach the client.
	stopCell := make(chan struct{})
	streamPCMUToWorker(stopCell, stack.fakes[0], "ho-race", markedPCMU(0xD4))
	defer close(stopCell)
	expectEnergyFrame(t, trackReader(echo), 3*time.Second)
	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "active" {
		t.Fatalf("concurrent commits disturbed call state: %v", stored["state"])
	}
}

// A probe that expired before its commit is an explicit 404, never a
// partial adoption (TTL reaping races the commit registry, not the peer).
func TestProbeCommitAfterTTLExpiry(t *testing.T) {
	runtime.ProbeTTL = 250 * time.Millisecond
	t.Cleanup(func() { runtime.ProbeTTL = 45 * time.Second })
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	waitForLineReady(t, stack, token)

	view := dialOutbound(t, stack, token, "ho-ttl")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ho-ttl", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ho-ttl", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	pc, offer := probeClient(t)
	defer pc.Close()
	body, _ := json.Marshal(map[string]string{"sdp": offer})
	status, payload := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe", token, "", body)
	if status != http.StatusCreated {
		t.Fatalf("probe attach status = %d body=%s", status, payload)
	}
	deadline := time.Now().Add(3 * time.Second)
	for stack.manager.HasProbe(callID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if stack.manager.HasProbe(callID) {
		t.Fatalf("probe outlived its TTL")
	}
	status, payload = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe/commit", token, "ho-ttl-commit", nil)
	if status != http.StatusNotFound {
		t.Fatalf("commit after TTL status = %d body=%s, want 404", status, payload)
	}
	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "active" {
		t.Fatalf("expired-commit attempt disturbed call state: %v", stored["state"])
	}
}

// dialMeasure upgrades an authenticated measurement socket for a call.
func dialMeasure(t *testing.T, stack *testStack, token, callID string) *websocket.Conn {
	t.Helper()
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/calls/" + callID + "/media/measure"
	conn, _, err := websocket.DefaultDialer.DialContext(t.Context(), url, header)
	if err != nil {
		t.Fatalf("measure upgrade: %v", err)
	}
	return conn
}

// measurePing sends one JSON ping and waits for the pong with the same t.
func measurePing(t *testing.T, conn *websocket.Conn, value int64) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"type": "ping", "t": value})
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatalf("measure ping: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var pong struct {
		Type string `json:"type"`
		T    int64  `json:"t"`
	}
	if err := conn.ReadJSON(&pong); err != nil || pong.Type != "pong" || pong.T != value {
		t.Fatalf("measure pong = %+v err=%v, want pong t=%d", pong, err, value)
	}
	_ = conn.SetReadDeadline(time.Time{})
}

// The measurement socket samples the WSS path's RTT while the call's media
// runs over the adopted ICE peer: it never replaces or feeds the session's
// host transport, and the gateway closes it when the call ends.
func TestWSMeasureCoexistsWithICE(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	tokenA, _ := stack.enroll(keyID, secret, "Device A")
	keyB, secretB := stack.createKey(true)
	tokenB, _ := stack.enroll(keyB, secretB, "Device B")
	waitForLineReady(t, stack, tokenA)

	view := dialOutbound(t, stack, tokenA, "ho-measure")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("ho-measure", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("ho-measure", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, tokenA, callID, "active")

	// Auth guards before the upgrade: unauthenticated 401, non-owner 403.
	measureURL := "ws" + stack.server.URL[len("http"):] + "/api/v2/calls/" + callID + "/media/measure"
	if _, response, err := websocket.DefaultDialer.Dial(measureURL, nil); err == nil {
		t.Fatalf("unauthenticated measure upgrade succeeded: %v", response)
	} else if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated measure status = %v, want 401", response)
	}
	if _, response, err := websocket.DefaultDialer.Dial(measureURL, http.Header{"Authorization": []string{"Bearer " + tokenB}}); err == nil {
		t.Fatalf("non-owner measure upgrade succeeded")
	} else if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("non-owner measure status = %v, want 403", response)
	}

	// Promote the call onto an ICE peer (probe -> commit).
	pc, track, echo, probeDC, probeEcho := handoverProbe(t, stack, tokenA, callID)
	defer pc.Close()
	stopProbe := make(chan struct{})
	streamTrackRTP(stopProbe, track, markedPCMU(0xD4))
	defer close(stopProbe)
	sendEchoPing(t, probeDC, 10)
	expectEcho(t, probeEcho, 10, 3*time.Second)
	commit := "/api/v2/calls/" + callID + "/media/probe/commit"
	if status, payload := stack.request(http.MethodPost, commit, tokenA, "ho-measure-commit", nil); status != http.StatusOK {
		t.Fatalf("commit status = %d body=%s", status, payload)
	}
	if transport := mediaTransport(t, stack, tokenA, callID); transport != "ice" {
		t.Fatalf("mediaTransport = %q, want ice", transport)
	}

	// The measurement socket now provides the WSS baseline WITHOUT touching
	// the ICE host transport.
	measure := dialMeasure(t, stack, tokenA, callID)
	defer measure.Close()
	measurePing(t, measure, 111)
	measurePing(t, measure, 222)

	// ICE media must be unaffected: both echo and mixed audio keep flowing.
	sendEchoPing(t, probeDC, 11)
	expectEcho(t, probeEcho, 11, 3*time.Second)
	stopCell := make(chan struct{})
	streamPCMUToWorker(stopCell, stack.fakes[0], "ho-measure", markedPCMU(0xD4))
	defer close(stopCell)
	expectEnergyFrame(t, trackReader(echo), 3*time.Second)
	if transport := mediaTransport(t, stack, tokenA, callID); transport != "ice" {
		t.Fatalf("measurement socket disturbed the ICE host: %q", transport)
	}

	// Terminal: hangup closes the measurement socket server-side.
	if status, _ := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/hangup", tokenA, "ho-measure-hang", nil); status != http.StatusNoContent {
		t.Fatalf("hangup status = %d", status)
	}
	waitForCallState(t, stack, tokenA, callID, "ended")
	deadline := time.Now().Add(4 * time.Second)
	for {
		_ = measure.SetReadDeadline(time.Now().Add(time.Until(deadline)))
		if _, _, err := measure.ReadMessage(); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("measurement socket stayed open after hangup")
		}
	}
}
