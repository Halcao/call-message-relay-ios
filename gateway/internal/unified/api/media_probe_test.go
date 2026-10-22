package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/runtime"

	pion "github.com/pion/webrtc/v4"
)

// probeClient builds a real PCMU offer SDP. Connectivity is not required
// for the lifecycle assertions; the returned pc is closed by the caller.
func probeClient(t *testing.T) (*pion.PeerConnection, string) {
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
	return pc, local.SDP
}

// The detached probe is safe to attach while a healthy call runs over WSS:
// authorization is enforced, invalid SDP fails cleanly without leaking a
// session, a replacement probe supersedes the old one, the TTL reaps stale
// probes, and ending the call cancels the probe.

// probeFinishHandshake applies the gateway's answer to the probe client.
func probeFinishHandshake(t *testing.T, pc *pion.PeerConnection, answerSDP string) chan *pion.TrackRemote {
	t.Helper()
	remote := make(chan *pion.TrackRemote, 1)
	pc.OnTrack(func(rt *pion.TrackRemote, _ *pion.RTPReceiver) {
		select {
		case remote <- rt:
		default:
		}
	})
	if err := pc.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeAnswer, SDP: answerSDP,
	}); err != nil {
		t.Fatalf("remote: %v", err)
	}
	return remote
}

func TestMediaProbeLifecycle(t *testing.T) {
	runtime.ProbeTTL = 200 * time.Millisecond
	t.Cleanup(func() { runtime.ProbeTTL = 45 * time.Second })
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	view := dialOutbound(t, stack, token, "probe-life")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("probe-life", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("probe-life", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	// Non-owner devices cannot probe.
	keyB, secretB := stack.createKey(true)
	tokenB, _ := stack.enroll(keyB, secretB, "Device B")
	status, _ := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe", tokenB, "",
		[]byte(`{"sdp":"v=0\r\n"}`))
	if status != http.StatusForbidden {
		t.Fatalf("non-owner probe status = %d, want 403", status)
	}

	// Invalid SDP fails cleanly and leaks nothing.
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe", token, "",
		[]byte(`{"sdp":"not-an-offer"}`))
	if status != http.StatusBadGateway {
		t.Fatalf("bad-sdp probe status = %d, want 502", status)
	}
	if stack.manager.HasProbe(callID) {
		t.Fatalf("failed probe leaked a session")
	}

	// A real offer gets an answer; the probe session exists but the call
	// state and the worker's media socket are untouched. (Connectivity is
	// not needed for the lifecycle assertions; live echo measurement is
	// verified on real hardware.)
	pc, offer := probeClient(t)
	defer pc.Close()
	body, _ := json.Marshal(map[string]string{"sdp": offer})
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
	if !stack.manager.HasProbe(callID) {
		t.Fatalf("probe session missing after attach")
	}
	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "active" {
		t.Fatalf("probe disturbed call state: %v", stored["state"])
	}
	if len(stack.fakes[0].hostFrames("probe-life")) != 0 {
		t.Fatalf("probe opened the worker media socket")
	}

	// Explicit discard is a safe no-op retry-wise.
	status, _ = stack.request(http.MethodDelete, "/api/v2/calls/"+callID+"/media/probe", token, "", nil)
	if status != http.StatusNoContent {
		t.Fatalf("discard status = %d, want 204", status)
	}
	if stack.manager.HasProbe(callID) {
		t.Fatalf("probe survived explicit discard")
	}

	// Stale probe reaping: attach again and let the TTL fire.
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe", token, "", body)
	if status != http.StatusCreated {
		t.Fatalf("second probe attach status = %d", status)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !stack.manager.HasProbe(callID) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stack.manager.HasProbe(callID) {
		t.Fatalf("stale probe outlived its TTL")
	}

	// Terminal cancel: an in-flight probe dies with the call.
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe", token, "", body)
	if status != http.StatusCreated {
		t.Fatalf("third probe attach status = %d", status)
	}
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/hangup", token, "probe-life-hang", nil)
	if status != http.StatusNoContent {
		t.Fatalf("hangup status = %d", status)
	}
	waitForCallState(t, stack, token, callID, "ended")
	if stack.manager.HasProbe(callID) {
		t.Fatalf("probe outlived the call")
	}
}

// Commit guards: without a probe, on an ended call, or from the wrong
// device, promotion is refused and the healthy media path is untouched.
func TestMediaProbeCommitGuards(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	tokenA, _ := stack.enroll(keyID, secret, "Device A")
	keyB, secretB := stack.createKey(true)
	tokenB, _ := stack.enroll(keyB, secretB, "Device B")

	view := dialOutbound(t, stack, tokenA, "probe-commit")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("probe-commit", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("probe-commit", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, tokenA, callID, "active")

	commit := "/api/v2/calls/" + callID + "/media/probe/commit"
	status, _ := stack.request(http.MethodPost, commit, tokenA, "commit-0", nil)
	if status != http.StatusNotFound {
		t.Fatalf("commit without probe status = %d, want 404", status)
	}
	status, _ = stack.request(http.MethodPost, commit, tokenB, "commit-1", nil)
	if status != http.StatusForbidden {
		t.Fatalf("non-owner commit status = %d, want 403", status)
	}

	pc, offer := probeClient(t)
	defer pc.Close()
	body, _ := json.Marshal(map[string]string{"sdp": offer})
	status, payload := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe", tokenA, "", body)
	if status != http.StatusCreated {
		t.Fatalf("probe attach status = %d body=%s", status, payload)
	}
	var answer struct {
		SDP string `json:"sdp"`
	}
	_ = json.Unmarshal(payload, &answer)
	_ = probeFinishHandshake(t, pc, answer.SDP)
	// Commit is ready-first: only a connected peer is adoptable.
	waitPCConnected(t, pc)

	status, _ = stack.request(http.MethodPost, commit, tokenA, "commit-2", nil)
	if status != http.StatusOK {
		t.Fatalf("commit status = %d, want 200", status)
	}
	if stack.manager.HasProbe(callID) {
		t.Fatalf("probe session outlived its commit")
	}
	stored := fetchCall(t, stack, tokenA, callID)
	if stored["state"] != "active" {
		t.Fatalf("commit disturbed call state: %v", stored["state"])
	}

	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/hangup", tokenA, "commit-hang", nil)
	if status != http.StatusNoContent {
		t.Fatalf("hangup status = %d", status)
	}
	waitForCallState(t, stack, tokenA, callID, "ended")
	status, _ = stack.request(http.MethodPost, commit, tokenA, "commit-3", nil)
	if status != http.StatusNotFound {
		t.Fatalf("commit after end status = %d, want 404", status)
	}
}
