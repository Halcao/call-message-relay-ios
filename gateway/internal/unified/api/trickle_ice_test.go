package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/runtime"
)

// candidateLine is a syntactically valid host candidate. Reachability is
// irrelevant here: the assertions are about the fencing/bounds of the
// incremental intake, not about connectivity (which build-64 field evidence
// covers on real hardware).
const candidateLine = "candidate:1 1 udp 2113937151 192.0.2.10 51000 typ host generation 0"

func trickleOfferBody(t *testing.T, offer, attachID string, candidates ...string) []byte {
	t.Helper()
	body := map[string]any{"sdp": offer, "type": "offer", "trickle": true}
	if attachID != "" {
		body["attachId"] = attachID
	}
	if len(candidates) > 0 {
		body["candidates"] = candidates
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

func trickleCandidatesBody(attachID string, candidates ...string) []byte {
	encoded, _ := json.Marshal(map[string]any{"attachId": attachID, "candidates": candidates})
	return encoded
}

// TestTrickleIceCapabilityAdvertised pins the capability-negotiation
// contract: the gateway advertises increment via the existing ICE payload so
// old clients (which ignore the flag) keep the full-SDP path and new clients
// never attempt trickle against an old gateway.
func TestTrickleIceCapabilityAdvertised(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	status, payload := stack.request(http.MethodGet, "/api/v2/ice", token, "", nil)
	if status != http.StatusOK {
		t.Fatalf("ice status = %d body=%s", status, payload)
	}
	var view struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := json.Unmarshal(payload, &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view.Capabilities["iceTrickle"] != true {
		t.Fatalf("iceTrickle capability missing: %s", payload)
	}
}

// TestTrickleOfferAttachTokenFencing drives the whole call-offer path:
// trickle offers mint an attach token, the token accepts bounded batches,
// a superseded attach invalidates the old token, and a full-SDP (legacy)
// attach never mints one.
func TestTrickleOfferAttachTokenFencing(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	otherKey, otherSecret := stack.createKey(true)
	otherToken, _ := stack.enroll(otherKey, otherSecret, "Device B")

	view := dialOutbound(t, stack, token, "trickle-call")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("trickle-call", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("trickle-call", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	pc, offer := probeClient(t)
	defer pc.Close()
	status, payload := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/offer", token, "",
		trickleOfferBody(t, offer, ""))
	if status != http.StatusOK {
		t.Fatalf("trickle offer status = %d body=%s", status, payload)
	}
	var answer struct {
		SDP      string `json:"sdp"`
		Trickle  bool   `json:"trickle"`
		AttachID string `json:"attachId"`
	}
	if err := json.Unmarshal(payload, &answer); err != nil || answer.SDP == "" {
		t.Fatalf("trickle answer: %v %s", err, payload)
	}
	if !answer.Trickle || strings.TrimSpace(answer.AttachID) == "" {
		t.Fatalf("trickle offer minted no attach token: %s", payload)
	}

	// A valid batch is applied (the count is the honest applied number).
	status, payload = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/candidates", token, "",
		trickleCandidatesBody(answer.AttachID, candidateLine))
	if status != http.StatusOK {
		t.Fatalf("candidate batch status = %d body=%s", status, payload)
	}
	var applied struct {
		Added int `json:"added"`
	}
	if err := json.Unmarshal(payload, &applied); err != nil || applied.Added != 1 {
		t.Fatalf("candidate batch not applied: %s", payload)
	}

	// Garbage is skipped, never fatal, and never counted.
	status, payload = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/candidates", token, "",
		trickleCandidatesBody(answer.AttachID, "not-a-candidate", "", "candidate:1 1 udp bogus"))
	if status != http.StatusOK {
		t.Fatalf("garbage batch status = %d body=%s", status, payload)
	}
	applied.Added = -1
	_ = json.Unmarshal(payload, &applied)
	if applied.Added != 0 {
		t.Fatalf("garbage counted as applied: %s", payload)
	}

	// The batch size is bounded: 20 lines yield at most trickleMaxBatch.
	lines := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		lines = append(lines, candidateLine)
	}
	status, payload = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/candidates", token, "",
		trickleCandidatesBody(answer.AttachID, lines...))
	if status != http.StatusOK {
		t.Fatalf("bulk batch status = %d", status)
	}
	applied.Added = -1
	_ = json.Unmarshal(payload, &applied)
	if applied.Added > 8 {
		t.Fatalf("batch cap not enforced: %d", applied.Added)
	}

	// A different device cannot post candidates to this call.
	otherStatus, _ := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/candidates", otherToken, "",
		trickleCandidatesBody(answer.AttachID, candidateLine))
	if otherStatus == http.StatusOK {
		t.Fatalf("non-owner device applied candidates")
	}

	// A bogus token is stale, not an error storm.
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/candidates", token, "",
		trickleCandidatesBody("bogus-token", candidateLine))
	if status != http.StatusConflict {
		t.Fatalf("bogus token status = %d, want 409", status)
	}
	// A missing attach id is a client error.
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/candidates", token, "",
		[]byte(`{"candidates":["`+candidateLine+`"]}`))
	if status != http.StatusBadRequest {
		t.Fatalf("missing attachId status = %d, want 400", status)
	}

	// A full-SDP re-offer replaces the attachment: no token, and the previous
	// token is dead — a late batch can never corrupt the new negotiation.
	status, payload = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/offer", token, "",
		[]byte(`{"sdp":`+jsonString(offer)+`,"type":"offer"}`))
	if status != http.StatusOK {
		t.Fatalf("legacy offer status = %d body=%s", status, payload)
	}
	var legacy struct {
		Trickle  bool   `json:"trickle"`
		AttachID string `json:"attachId"`
	}
	_ = json.Unmarshal(payload, &legacy)
	if legacy.Trickle || legacy.AttachID != "" {
		t.Fatalf("full-SDP offer advertised trickle: %s", payload)
	}
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/candidates", token, "",
		trickleCandidatesBody(answer.AttachID, candidateLine))
	if status != http.StatusConflict {
		t.Fatalf("stale token after re-offer = %d, want 409", status)
	}
}

// TestTricklePreflightCandidates covers the call-independent preflight path:
// the attach mints a token, batches are applied on the device-scoped
// preflight id, and a superseded preflight id is stale.
func TestTricklePreflightCandidates(t *testing.T) {
	stack := newTestStack(t)
	runtime.ProbeTTL = 3 * time.Second
	t.Cleanup(func() { runtime.ProbeTTL = 45 * time.Second })
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	pc, offer := probeClient(t)
	defer pc.Close()
	status, payload := stack.request(http.MethodPost, "/api/v2/media/probe", token, "",
		trickleOfferBody(t, offer, ""))
	if status != http.StatusCreated {
		t.Fatalf("trickle preflight status = %d body=%s", status, payload)
	}
	var first struct {
		PreflightID string `json:"preflightId"`
		AttachID    string `json:"attachId"`
		Trickle     bool   `json:"trickle"`
	}
	if err := json.Unmarshal(payload, &first); err != nil || first.PreflightID == "" || first.AttachID == "" || !first.Trickle {
		t.Fatalf("preflight answer incomplete: %v %s", err, payload)
	}

	preflightBody, _ := json.Marshal(map[string]any{
		"preflightId": first.PreflightID, "attachId": first.AttachID, "candidates": []string{candidateLine},
	})
	status, payload = stack.request(http.MethodPost, "/api/v2/media/probe/candidates", token, "", preflightBody)
	if status != http.StatusOK {
		t.Fatalf("preflight candidate status = %d body=%s", status, payload)
	}
	if !strings.Contains(string(payload), `"added":1`) {
		t.Fatalf("preflight batch not applied: %s", payload)
	}

	// Unknown preflight id is an honest 404.
	unknownBody, _ := json.Marshal(map[string]any{
		"preflightId": "prb_missing", "attachId": first.AttachID, "candidates": []string{candidateLine},
	})
	status, _ = stack.request(http.MethodPost, "/api/v2/media/probe/candidates", token, "", unknownBody)
	if status != http.StatusNotFound {
		t.Fatalf("unknown preflight status = %d, want 404", status)
	}
	// Wrong attach token for the live preflight is stale (409) and changes
	// nothing: the preflight id and peer connection are untouched.
	staleBody, _ := json.Marshal(map[string]any{
		"preflightId": first.PreflightID, "attachId": "wrong", "candidates": []string{candidateLine},
	})
	status, _ = stack.request(http.MethodPost, "/api/v2/media/probe/candidates", token, "", staleBody)
	if status != http.StatusConflict {
		t.Fatalf("stale preflight token status = %d, want 409", status)
	}
	// The live preflight is still usable after the rejected batch.
	status, payload = stack.request(http.MethodPost, "/api/v2/media/probe/renew", token, "",
		[]byte(`{"preflightId":"`+first.PreflightID+`"}`))
	if status != http.StatusOK {
		t.Fatalf("preflight unusable after stale batch: %d %s", status, payload)
	}

	// A superseding preflight attach invalidates the old attach token.
	status, payload = stack.request(http.MethodPost, "/api/v2/media/probe", token, "",
		trickleOfferBody(t, offer, ""))
	if status != http.StatusCreated {
		t.Fatalf("second preflight status = %d body=%s", status, payload)
	}
	var second struct {
		PreflightID string `json:"preflightId"`
		AttachID    string `json:"attachId"`
	}
	_ = json.Unmarshal(payload, &second)
	if second.PreflightID == first.PreflightID {
		t.Fatalf("preflight not superseded")
	}
	tokenBody, _ := json.Marshal(map[string]any{
		"preflightId": first.PreflightID, "attachId": first.AttachID, "candidates": []string{candidateLine},
	})
	status, _ = stack.request(http.MethodPost, "/api/v2/media/probe/candidates", token, "", tokenBody)
	if status != http.StatusNotFound {
		t.Fatalf("stale preflight id status = %d, want 404", status)
	}
}

// TestTrickleProbeCandidates covers the call-scoped measurement probe path
// and the media-safety property: probing never perturbs call state.
func TestTrickleProbeCandidates(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	view := dialOutbound(t, stack, token, "trickle-probe")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("trickle-probe", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("trickle-probe", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	pc, offer := probeClient(t)
	defer pc.Close()
	status, payload := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe", token, "",
		trickleOfferBody(t, offer, ""))
	if status != http.StatusCreated {
		t.Fatalf("trickle probe status = %d body=%s", status, payload)
	}
	var answer struct {
		AttachID string `json:"attachId"`
	}
	_ = json.Unmarshal(payload, &answer)
	if answer.AttachID == "" {
		t.Fatalf("probe minted no attach token: %s", payload)
	}
	status, payload = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe/candidates", token, "",
		trickleCandidatesBody(answer.AttachID, candidateLine))
	if status != http.StatusOK || !strings.Contains(string(payload), `"added":1`) {
		t.Fatalf("probe candidate status = %d body=%s", status, payload)
	}
	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "active" {
		t.Fatalf("candidate intake disturbed call state: %v", stored["state"])
	}
	if len(stack.fakes[0].hostFrames("trickle-probe")) != 0 {
		t.Fatalf("candidate intake opened the worker media socket")
	}
	// After the probe ends, submissions are refused.
	status, _ = stack.request(http.MethodDelete, "/api/v2/calls/"+callID+"/media/probe", token, "", nil)
	if status != http.StatusNoContent {
		t.Fatalf("probe discard status = %d", status)
	}
	status, _ = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/media/probe/candidates", token, "",
		trickleCandidatesBody(answer.AttachID, candidateLine))
	if status != http.StatusNotFound {
		t.Fatalf("candidates after probe discard = %d, want 404", status)
	}
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// TestTrickleEarlyAnswerAndServerCandidatePoll proves the build-68 handshake
// contract end to end: with a black-hole STUN server the gateway answers the
// trickle offer WITHOUT waiting for its own gathering, and the candidates it
// gathers afterwards are delivered through the attach-fenced poll on the same
// authenticated JSON plane.
func TestTrickleEarlyAnswerAndServerCandidatePoll(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	view := dialOutbound(t, stack, token, "trickle-early")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("trickle-early", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("trickle-early", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	// Host candidates still gather instantly; srflx gathering can never
	// finish, which is the state the early answer must survive.
	stack.manager.Cfg.ICE.STUNServers = []string{"127.0.0.1:1"}

	pc, offer := probeClient(t)
	defer pc.Close()
	started := time.Now()
	status, payload := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/offer", token, "",
		trickleOfferBody(t, offer, ""))
	elapsed := time.Since(started)
	if status != http.StatusOK {
		t.Fatalf("trickle offer status = %d body=%s", status, payload)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("offer waited for gathering: %s", elapsed)
	}
	var answer struct {
		AttachID          string `json:"attachId"`
		CandidateCursor   int    `json:"candidateCursor"`
		GatheringComplete bool   `json:"gatheringComplete"`
	}
	if err := json.Unmarshal(payload, &answer); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if answer.AttachID == "" {
		t.Fatalf("no attach token: %s", payload)
	}
	if answer.GatheringComplete {
		t.Fatalf("gateway claimed complete gathering despite a black-hole STUN server: %s", payload)
	}

	// The poll delivers the gateway's candidates incrementally.
	pollPath := "/api/v2/calls/" + callID + "/webrtc/candidates?attachId=" + answer.AttachID
	cursor := answer.CandidateCursor
	seen := 0
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && seen == 0 {
		time.Sleep(150 * time.Millisecond)
		status, payload = stack.request(http.MethodGet, pollPath+"&after="+itoa(cursor), token, "", nil)
		if status != http.StatusOK {
			t.Fatalf("poll status = %d body=%s", status, payload)
		}
		var batch struct {
			Candidates []string `json:"candidates"`
			Cursor     int      `json:"cursor"`
			Complete   bool     `json:"complete"`
		}
		if err := json.Unmarshal(payload, &batch); err != nil {
			t.Fatalf("decode batch: %v", err)
		}
		if batch.Cursor < cursor {
			t.Fatalf("cursor regressed: %d < %d", batch.Cursor, cursor)
		}
		for _, candidate := range batch.Candidates {
			if !strings.HasPrefix(candidate, "candidate:") {
				t.Fatalf("malformed incremental candidate %q", candidate)
			}
			seen++
		}
		cursor = batch.Cursor
		if batch.Complete {
			t.Fatalf("poll reported complete gathering despite a black-hole STUN server")
		}
	}
	if seen == 0 {
		t.Fatalf("no gateway candidates were delivered incrementally")
	}

	// Stale/garbage tokens are refused; the immediate re-poll is throttled.
	status, _ = stack.request(http.MethodGet,
		"/api/v2/calls/"+callID+"/webrtc/candidates?attachId=stale-token", token, "", nil)
	if status != http.StatusConflict {
		t.Fatalf("stale token poll status = %d, want 409", status)
	}
	status, _ = stack.request(http.MethodGet, pollPath+"&after="+itoa(cursor), token, "", nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("unthrottled poll status = %d, want 429", status)
	}
	status, _ = stack.request(http.MethodGet, "/api/v2/calls/"+callID+"/webrtc/candidates", token, "", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("missing attachId status = %d, want 400", status)
	}

	// Client candidate intake still works on the same attachment.
	status, payload = stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/candidates", token, "",
		trickleCandidatesBody(answer.AttachID, candidateLine))
	if status != http.StatusOK || !strings.Contains(string(payload), `"added":1`) {
		t.Fatalf("candidate intake after poll = %d %s", status, payload)
	}
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
