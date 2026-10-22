package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/runtime"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/voice"
	"strings"

	pion "github.com/pion/webrtc/v4"
)

// Global ICE + device-scoped preflight probe: the client can measure the
// direct path while foreground WITHOUT any call existing. No call state, no
// line legs and no worker media socket are touched; the candidate can then
// be adopted by a real call's commit through the preflightId.

func TestGlobalICE(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	status, payload := stack.request(http.MethodGet, "/api/v2/ice", token, "", nil)
	if status != http.StatusOK {
		t.Fatalf("global ice status = %d body=%s", status, payload)
	}
	var view struct {
		Policy          string           `json:"policy"`
		IceServers      []map[string]any `json:"iceServers"`
		MediaTransports []string         `json:"mediaTransports"`
	}
	if err := json.Unmarshal(payload, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Policy == "" || view.MediaTransports == nil {
		t.Fatalf("global ice incomplete: %s", payload)
	}

	// Unauthenticated callers get nothing.
	status, _ = stack.request(http.MethodGet, "/api/v2/ice", "", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous ice status = %d, want 401", status)
	}
}

func TestMediaPreflightLifecycle(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	// Invalid SDP fails cleanly.
	status, _ := stack.request(http.MethodPost, "/api/v2/media/probe", token, "",
		[]byte(`{"sdp":"not-an-offer"}`))
	if status != http.StatusBadGateway {
		t.Fatalf("bad-sdp preflight status = %d, want 502", status)
	}

	// A real offer gets an answer carrying an adoptable id and TTL.
	pc, offer := probeClient(t)
	defer pc.Close()
	body, _ := json.Marshal(map[string]string{"sdp": offer})
	status, payload := stack.request(http.MethodPost, "/api/v2/media/probe", token, "", body)
	if status != http.StatusCreated {
		t.Fatalf("preflight attach status = %d body=%s", status, payload)
	}
	var answer struct {
		SDP         string `json:"sdp"`
		PreflightID string `json:"preflightId"`
		TTLMs       int64  `json:"ttlMs"`
	}
	if err := json.Unmarshal(payload, &answer); err != nil || answer.SDP == "" ||
		answer.PreflightID == "" || answer.TTLMs <= 0 {
		t.Fatalf("preflight answer incomplete: %v %s", err, payload)
	}

	// A second attach supersedes the first; the stale id is then unusable.
	status, payload = stack.request(http.MethodPost, "/api/v2/media/probe", token, "", body)
	if status != http.StatusCreated {
		t.Fatalf("second preflight attach status = %d", status)
	}
	var second struct {
		PreflightID string `json:"preflightId"`
	}
	if err := json.Unmarshal(payload, &second); err != nil || second.PreflightID == answer.PreflightID {
		t.Fatalf("second preflight did not supersede: %s", payload)
	}

	// Explicit discard is idempotent for retries.
	status, _ = stack.request(http.MethodDelete, "/api/v2/media/probe?preflightId="+second.PreflightID, token, "", nil)
	if status != http.StatusNoContent {
		t.Fatalf("preflight discard status = %d, want 204", status)
	}
	status, _ = stack.request(http.MethodDelete, "/api/v2/media/probe?preflightId="+second.PreflightID, token, "", nil)
	if status != http.StatusNoContent {
		t.Fatalf("preflight re-discard status = %d, want 204", status)
	}

	// A superseded candidate can never be committed: honest 404.
	view := dialOutbound(t, stack, token, "preflight-commit")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("preflight-commit", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("preflight-commit", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	stale, _ := json.Marshal(map[string]string{"preflightId": answer.PreflightID})
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe/commit", token, "", stale)
	if status != http.StatusNotFound {
		t.Fatalf("commit with superseded preflight status = %d, want 404", status)
	}
}

// TestMediaPreflightRenewal verifies the server-side TTL extension: a
// still-registered device-scoped candidate can be renewed in place (same id,
// no renegotiation), an unknown/superseded id is an honest 404, and a renewed
// entry survives past the ORIGINAL TTL while still being reaped after the
// renewed one.
func TestMediaPreflightRenewal(t *testing.T) {
	runtime.ProbeTTL = 400 * time.Millisecond
	t.Cleanup(func() { runtime.ProbeTTL = 45 * time.Second })
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	pc, offer := probeClient(t)
	defer pc.Close()
	body, _ := json.Marshal(map[string]string{"sdp": offer})
	status, payload := stack.request(http.MethodPost, "/api/v2/media/probe", token, "", body)
	if status != http.StatusCreated {
		t.Fatalf("preflight attach status = %d body=%s", status, payload)
	}
	var answer struct {
		PreflightID string `json:"preflightId"`
	}
	if err := json.Unmarshal(payload, &answer); err != nil || answer.PreflightID == "" {
		t.Fatalf("preflight answer: %v %s", err, payload)
	}

	// A missing body and an unknown id are clean client errors.
	status, _ = stack.request(http.MethodPost, "/api/v2/media/probe/renew", token, "", []byte(`{}`))
	if status != http.StatusBadRequest {
		t.Fatalf("empty renew status = %d, want 400", status)
	}
	status, _ = stack.request(http.MethodPost, "/api/v2/media/probe/renew", token, "",
		[]byte(`{"preflightId":"prb_unknown"}`))
	if status != http.StatusNotFound {
		t.Fatalf("unknown renew status = %d, want 404", status)
	}

	// Renewing the live entry returns the refreshed TTL, same id.
	renewBody, _ := json.Marshal(map[string]string{"preflightId": answer.PreflightID})
	status, payload = stack.request(http.MethodPost, "/api/v2/media/probe/renew", token, "", renewBody)
	if status != http.StatusOK {
		t.Fatalf("renew status = %d body=%s", status, payload)
	}
	var renewed struct {
		PreflightID string `json:"preflightId"`
		TTLMs       int64  `json:"ttlMs"`
	}
	if err := json.Unmarshal(payload, &renewed); err != nil || renewed.PreflightID != answer.PreflightID ||
		renewed.TTLMs != runtime.ProbeTTL.Milliseconds() {
		t.Fatalf("renew answer incomplete: %v %s", err, payload)
	}

	// Past the ORIGINAL deadline the renewed entry is still registered ...
	time.Sleep(250 * time.Millisecond)
	if !stack.manager.HasPreflight(stack.deviceID) {
		t.Fatalf("renewed preflight was reaped at the original TTL")
	}
	// ... and after the renewed deadline it is gone.
	time.Sleep(300 * time.Millisecond)
	if stack.manager.HasPreflight(stack.deviceID) {
		t.Fatalf("renewed preflight outlived its renewed TTL")
	}
	status, _ = stack.request(http.MethodPost, "/api/v2/media/probe/renew", token, "", renewBody)
	if status != http.StatusNotFound {
		t.Fatalf("post-expiry renew status = %d, want 404", status)
	}
}

// preflightProbeClient builds a handover-style client (send track + echo
// data channel) attached through the DEVICE-SCOPED preflight endpoint — no
// call exists at attach time. The measurement phase streams NO microphone
// audio: only data-channel pings, proving preflight needs no audio path.
func preflightProbeClient(t *testing.T, stack *testStack, token string) (pc *pion.PeerConnection, track *pion.TrackLocalStaticRTP, received chan []byte, channel *pion.DataChannel, echoes chan []byte, preflightID string) {
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
	connection, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("pc: %v", err)
	}
	sendTrack, err := pion.NewTrackLocalStaticRTP(
		pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		"audio", "probe")
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if _, err := connection.AddTransceiverFromTrack(sendTrack, pion.RTPTransceiverInit{
		Direction: pion.RTPTransceiverDirectionSendrecv,
	}); err != nil {
		t.Fatalf("transceiver: %v", err)
	}
	received = make(chan []byte, 512)
	connection.OnTrack(func(remote *pion.TrackRemote, _ *pion.RTPReceiver) {
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
	echoes = make(chan []byte, 128)
	channel, err = connection.CreateDataChannel(media.ProbeEchoLabel, nil)
	if err != nil {
		t.Fatalf("data channel: %v", err)
	}
	channel.OnMessage(func(message pion.DataChannelMessage) {
		payload := make([]byte, len(message.Data))
		copy(payload, message.Data)
		select {
		case echoes <- payload:
		default:
		}
	})
	opened := make(chan struct{})
	channel.OnOpen(func() { close(opened) })
	offer, err := connection.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := connection.SetLocalDescription(offer); err != nil {
		t.Fatalf("local: %v", err)
	}
	<-pion.GatheringCompletePromise(connection)
	local := connection.LocalDescription()
	if local == nil {
		t.Fatalf("missing local description")
	}
	body, _ := json.Marshal(map[string]string{"sdp": local.SDP})
	status, payload := stack.request(http.MethodPost, "/api/v2/media/probe", token, "", body)
	if status != http.StatusCreated {
		t.Fatalf("preflight attach status = %d body=%s", status, payload)
	}
	var answer struct {
		SDP         string `json:"sdp"`
		PreflightID string `json:"preflightId"`
	}
	if err := json.Unmarshal(payload, &answer); err != nil || answer.SDP == "" || answer.PreflightID == "" {
		t.Fatalf("preflight answer: %v %s", err, payload)
	}
	if err := connection.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer.SDP}); err != nil {
		t.Fatalf("remote: %v", err)
	}
	waitPCConnected(t, connection)
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatalf("preflight echo channel never opened (state=%s)", channel.ReadyState())
	}
	return connection, sendTrack, received, channel, echoes, answer.PreflightID
}

// Preflight (data-channel-only, NO microphone audio) → live call adoption →
// bidirectional SYNTHETIC PCM over the adopted peer. mediaTransport=="ice"
// alone is never treated as audio proof — energy frames in both directions
// are asserted explicitly (the field failure class this build targets).
func TestMediaPreflightCommitAdoption(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	waitForLineReady(t, stack, token)

	pc, track, received, channel, echoChan, preflightID := preflightProbeClient(t, stack, token)
	defer pc.Close()

	// Phase 1: measurement WITHOUT any audio streaming — the data channel
	// alone must answer the preflight question. The detached probe's mixer
	// still ticks silence frames downstream (normal pipeline fill), so the
	// honest no-audio assertion is ENERGY, not absence: any preflight media
	// frame must decode to silence.
	sendEchoPing(t, channel, 1)
	expectEcho(t, echoChan, 1, 3*time.Second)
	preflightDeadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(preflightDeadline) {
		select {
		case frame := <-received:
			if pcm, err := voice.DecodePCMU(frame); err != nil || energy(pcm) > 0 {
				t.Fatalf("preflight carried non-silence audio without any capture: %d bytes", len(frame))
			}
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Phase 2: a call appears; the preflight candidate is adopted by commit.
	view := dialOutbound(t, stack, token, "preflight-adopt")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("preflight-adopt", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("preflight-adopt", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	commit, _ := json.Marshal(map[string]string{"preflightId": preflightID})
	status, payload := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe/commit", token, "", commit)
	if status != http.StatusOK {
		t.Fatalf("commit with fresh preflight status = %d body=%s", status, payload)
	}
	if transport := mediaTransport(t, stack, token, callID); transport != "ice" {
		t.Fatalf("mediaTransport = %q, want ice after preflight commit", transport)
	}

	// Phase 3: bidirectional SYNTHETIC PCM must actually flow over the
	// adopted peer (client -> worker leg, worker -> client track).
	stopClient := make(chan struct{})
	defer close(stopClient)
	streamTrackRTP(stopClient, track, markedPCMU(0xD4))
	workerBaseline := len(stack.fakes[0].hostFrames("preflight-adopt"))
	deadline := time.Now().Add(3 * time.Second)
	for {
		frames := stack.fakes[0].hostFrames("preflight-adopt")
		for _, frame := range frames[workerBaseline:] {
			if pcm, err := voice.DecodePCMU(frame); err == nil && energy(pcm) > 400 {
				goto clientToWorkerOK
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("client audio never reached the worker leg over the adopted preflight peer")
		}
		time.Sleep(20 * time.Millisecond)
	}
clientToWorkerOK:
	stopWorker := make(chan struct{})
	defer close(stopWorker)
	streamPCMUToWorker(stopWorker, stack.fakes[0], "preflight-adopt", markedPCMU(0xD4))
	expectEnergyFrame(t, trackReader(received), 3*time.Second)

	// The adopted candidate is consumed exactly once.
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe/commit", token, "", commit)
	if status != http.StatusNotFound {
		t.Fatalf("double commit status = %d, want 404", status)
	}

	// Unknown preflight id: honest 404 (stale/background race fallback).
	unknown, _ := json.Marshal(map[string]string{"preflightId": "prb_missing"})
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe/commit", token, "", unknown)
	if status != http.StatusNotFound {
		t.Fatalf("commit with unknown preflight status = %d, want 404", status)
	}
}

// Thread delete: an authorized device tombstones a conversation; the thread
// list and message history hide it for EVERY device (tombstone, not local
// state), while a NEW message after the delete reappears as a fresh
// conversation. Devices without the line grant cannot delete.
func TestThreadDeleteTombstone(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	oldTime := time.Now().Add(-time.Hour).UnixMilli()
	fake := stack.fakes[0]
	fake.mu.Lock()
	fake.threads = []map[string]any{{
		"key": "line1-peer", "peer": "+8613800000001", "unreadCount": 1,
		"lastMessage": map[string]any{
			"id": "msg_old", "gatewayID": "gw-line1", "lineID": "line1:line",
			"threadKey": "line1-peer", "direction": "inbound", "peer": "+8613800000001",
			"body": "before delete", "encoding": "gsm7", "status": "sent", "createdAt": oldTime,
		},
	}}
	fake.messages = []map[string]any{{
		"id": "msg_old", "gatewayID": "gw-line1", "lineID": "line1:line",
		"threadKey": "line1-peer", "direction": "inbound", "peer": "+8613800000001",
		"body": "before delete", "encoding": "gsm7", "status": "sent", "createdAt": oldTime,
	}}
	fake.mu.Unlock()

	// The thread is visible before the delete.
	status, payload := stack.request(http.MethodGet, "/api/v2/threads", token, "", nil)
	if status != http.StatusOK || !containsSub(payload, []byte("line1:line1-peer")) {
		t.Fatalf("threads before delete: status=%d body=%s", status, payload)
	}

	// A device WITHOUT the line1 grant cannot delete: enroll a key granted
	// only line2 and attempt the line1 thread.
	keyB, secretB := stack.createKey(false)
	if err := stack.store.DeleteKeyLine(context.Background(), keyB, "line1"); err != nil {
		t.Fatalf("revoke line1 grant: %v", err)
	}
	if err := stack.store.SetKeyLine(context.Background(), store.KeyLine{
		KeyID: keyB, LineID: "line2", ReceiveSMS: true, ReceiveCalls: true, SendSMS: true, Dial: true,
	}); err != nil {
		t.Fatalf("grant line2: %v", err)
	}
	tokenB, _ := stack.enroll(keyB, secretB, "Device B")
	status, _ = stack.request(http.MethodDelete, "/api/v2/threads/line1:line1-peer", tokenB, "", nil)
	if status != http.StatusForbidden && status != http.StatusNotFound {
		t.Fatalf("unauthorized thread delete status = %d, want 403/404", status)
	}

	// Authorized delete succeeds and the thread disappears for BOTH devices.
	status, _ = stack.request(http.MethodDelete, "/api/v2/threads/line1:line1-peer", token, "", nil)
	if status != http.StatusOK {
		t.Fatalf("thread delete status = %d, want 200", status)
	}
	status, payload = stack.request(http.MethodGet, "/api/v2/threads", token, "", nil)
	if status != http.StatusOK || containsSub(payload, []byte("line1-peer")) {
		t.Fatalf("threads after delete: status=%d body=%s", status, payload)
	}
	status, payload = stack.request(http.MethodGet, "/api/v2/threads", tokenB, "", nil)
	if status != http.StatusOK || containsSub(payload, []byte("line1-peer")) {
		t.Fatalf("threads after delete (other device): status=%d body=%s", status, payload)
	}

	// History is hidden, not destroyed: the tombstone filters the listing.
	status, payload = stack.request(http.MethodGet,
		"/api/v2/messages?threadKey=line1:line1-peer&limit=50", token, "", nil)
	if status != http.StatusOK {
		t.Fatalf("messages after delete status = %d", status)
	}
	var history []map[string]any
	if err := json.Unmarshal(payload, &history); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("tombstoned history visible: %s", payload)
	}

	// A NEW message after the delete reappears as a fresh conversation.
	fake.mu.Lock()
	fake.threads = []map[string]any{{
		"key": "line1-peer", "peer": "+8613800000001", "unreadCount": 1,
		"lastMessage": map[string]any{
			"id": "msg_new", "gatewayID": "gw-line1", "lineID": "line1:line",
			"threadKey": "line1-peer", "direction": "inbound", "peer": "+8613800000001",
			"body": "after delete", "encoding": "gsm7", "status": "sent", "createdAt": time.Now().UnixMilli(),
		},
	}}
	fake.messages = append(fake.messages, map[string]any{
		"id": "msg_new", "gatewayID": "gw-line1", "lineID": "line1:line",
		"threadKey": "line1-peer", "direction": "inbound", "peer": "+8613800000001",
		"body": "after delete", "encoding": "gsm7", "status": "sent", "createdAt": time.Now().UnixMilli(),
	})
	fake.mu.Unlock()
	status, payload = stack.request(http.MethodGet, "/api/v2/threads", token, "", nil)
	if status != http.StatusOK || !containsSub(payload, []byte("line1-peer")) {
		t.Fatalf("thread with new message should reappear: status=%d body=%s", status, payload)
	}
	status, payload = stack.request(http.MethodGet,
		"/api/v2/messages?threadKey=line1:line1-peer&limit=50", token, "", nil)
	if err := json.Unmarshal(payload, &history); err != nil || len(history) != 1 {
		t.Fatalf("post-delete history should show only the new message: %s", payload)
	}

	// Malformed keys fail honestly.
	status, _ = stack.request(http.MethodDelete, "/api/v2/threads/nolineprefix", token, "", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("malformed thread delete status = %d, want 400", status)
	}
}

// Push registration hardening: environment is required (an empty one makes
// the target permanently unsendable), and a registration can never blank
// tokens the device already has.
func TestRegisterPushValidation(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	deviceID := stack.deviceID

	put := func(value string) int {
		status, _ := stack.request(http.MethodPut, "/api/v2/devices/"+deviceID+"/push", token, "",
			[]byte(value))
		return status
	}

	if status := put(`{"apnsToken":"aa","voipToken":"vv","environment":""}`); status != http.StatusBadRequest {
		t.Fatalf("empty environment status = %d, want 400", status)
	}
	if status := put(`{"apnsToken":"aa","voipToken":"vv","environment":"staging"}`); status != http.StatusBadRequest {
		t.Fatalf("invalid environment status = %d, want 400", status)
	}
	if status := put(`{"apnsToken":"aa","voipToken":"vv","environment":"sandbox"}`); status != http.StatusNoContent {
		t.Fatalf("valid registration status = %d, want 204", status)
	}
	if status := put(`{"apnsToken":"","voipToken":"","environment":"sandbox"}`); status != http.StatusBadRequest {
		t.Fatalf("blanking registration status = %d, want 400", status)
	}
	// A partial update (one token path) stays allowed.
	if status := put(`{"apnsToken":"bb","voipToken":"vv","environment":"sandbox"}`); status != http.StatusNoContent {
		t.Fatalf("partial update status = %d, want 204", status)
	}
}

func containsSub(haystack, needle []byte) bool {
	return strings.Contains(string(haystack), string(needle))
}
