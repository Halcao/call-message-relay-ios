package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/events"

	"github.com/gorilla/websocket"
)

// blockingInject enqueues one worker event and fails if the bounded queue is
// full. Unlike injectWorkerEvent (non-blocking send, may drop), delivery is
// guaranteed, so FIFO ordering with a subsequent event of the same line's
// single consumer is deterministic.
func blockingInject(t *testing.T, fake *fakeWorker, eventType string, record workerCall) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"id":  "evt_block_" + record.ID + "_" + eventType + "_" + time.Now().Format("150405.000000000"),
		"seq": time.Now().UnixNano(), "type": eventType, "createdAt": time.Now().Unix(), "data": record,
	})
	select {
	case fake.events <- payload:
	case <-time.After(3 * time.Second):
		t.Fatalf("worker event queue full")
	}
}

// durableCallEndedCount counts the persisted call.ended events for one call.
func durableCallEndedCount(t *testing.T, stack *testStack, callID string) int {
	t.Helper()
	all, err := stack.bus.Replay(context.Background(), 0, 5000, func(event events.Event) bool {
		return event.Type == "call.ended"
	})
	if err != nil {
		t.Fatalf("replay events: %v", err)
	}
	count := 0
	for _, event := range all {
		var data map[string]any
		if json.Unmarshal(event.Data, &data) == nil && data["id"] == callID {
			count++
		}
	}
	return count
}

// The production terminal-overwrite race: a worker convergence snapshot whose
// read predates the terminal claim must never resurrect the call or drop
// endReason/connectedAt afterwards, and the call must publish exactly one
// call.ended. The stale snapshot is delivered after the claim is durable, and
// a second call's connecting event acts as an ordering barrier proving the
// line's single worker-event consumer has processed the stale snapshot.
func TestStaleWorkerSnapshotAfterTerminalClaimCannotOverwrite(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	view := dialOutbound(t, stack, token, "term-stale")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	blockingInject(t, stack.fakes[0], "cellular_ready", workerRecord("term-stale", "connecting", nil, nil, ""))
	blockingInject(t, stack.fakes[0], "media_ready", workerRecord("term-stale", "active", &connectedAt, nil, ""))
	active := waitForCallState(t, stack, token, callID, "active")
	if _, ok := active["connectedAt"].(float64); !ok {
		t.Fatalf("call not active with connectedAt: %v", active)
	}

	status, _ := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/hangup", token, "term-stale-hang", nil)
	if status != http.StatusNoContent {
		t.Fatalf("hangup status = %d, want 204", status)
	}
	ended := waitForCallState(t, stack, token, callID, "ended")
	endReason, _ := ended["endReason"].(string)
	if endReason == "" {
		t.Fatalf("terminal call lost endReason: %v", ended)
	}

	// Stale convergence snapshot: the row value a worker event handler would
	// have read before the claim (active, connectedAt set, no terminal
	// columns). The old full-row upsert resurrected the call and cleared
	// endReason; the conditional barrier must reject it.
	stale := workerRecord("term-stale", "active", &connectedAt, nil, "")
	stale.EndedAt = nil
	blockingInject(t, stack.fakes[0], "media_ready", stale)

	// Ordering barrier: a new call's connecting transition is processed after
	// the stale snapshot by the same per-line consumer.
	dialOutbound(t, stack, token, "term-flush")
	blockingInject(t, stack.fakes[0], "cellular_ready", workerRecord("term-flush", "connecting", nil, nil, ""))
	waitForCallState(t, stack, token, "line1:term-flush", "connecting")

	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "ended" {
		t.Fatalf("stale snapshot resurrected call: %v", stored)
	}
	if reason, _ := stored["endReason"].(string); reason == "" {
		t.Fatalf("stale snapshot dropped endReason: %v", stored)
	}
	if value, ok := stored["connectedAt"].(float64); !ok || int64(value) != connectedAt {
		t.Fatalf("stale snapshot dropped/mutated connectedAt: %v", stored)
	}
	if count := durableCallEndedCount(t, stack, callID); count != 1 {
		t.Fatalf("call.ended published %d times, want exactly 1", count)
	}
}

// The parent fix's answering guard must not drop valid worker updates: an
// active snapshot (with connectedAt) has to converge the answering claim, while
// a stale ringing snapshot must not drag it back to ringing.
func TestAnsweringGuardKeepsValidUpdatesAndBlocksRinging(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	stack.fakes[0].injectIncoming("guard-1", "+8613800000001")
	stack.waitForCall(token, "line1:guard-1")

	devices, err := stack.store.ListDevices(context.Background())
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	deviceID := ""
	for _, device := range devices {
		if device.Name == "Device A" {
			deviceID = device.ID
		}
	}
	if deviceID == "" {
		t.Fatal("enrolled device not found")
	}
	// The device-local answering claim (what POST /answer performs before the
	// upstream Answer RPC returns).
	if claimed, err := stack.store.ClaimCall(context.Background(), "line1:guard-1", deviceID); err != nil || !claimed {
		t.Fatalf("claim = %v/%v", claimed, err)
	}

	header := http.Header{"Authorization": []string{"Bearer " + token}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/events"
	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), url, header)
	if err != nil {
		t.Fatalf("events dial: %v", err)
	}
	defer conn.Close()

	// Stale ringing snapshot while answering: guard must keep answering.
	blockingInject(t, stack.fakes[0], "call.updated", workerRecord("guard-1", "incoming_ringing", nil, nil, ""))
	ringingView := readCallEvent(t, conn, "line1:guard-1", "call.updated", 3*time.Second)
	if ringingView["state"] != "answering" {
		t.Fatalf("ringing snapshot clobbered answering claim: %v", ringingView)
	}

	// Valid active snapshot while answering: must converge to active with
	// connectedAt (the pre-parent-fix guard dropped this update entirely).
	connectedAt := time.Now().UnixMilli()
	blockingInject(t, stack.fakes[0], "media_ready", workerRecord("guard-1", "active", &connectedAt, nil, ""))
	activeView := readCallEvent(t, conn, "line1:guard-1", "call.updated", 3*time.Second)
	if activeView["state"] != "active" {
		t.Fatalf("valid active update dropped: %v", activeView)
	}
	if value, ok := activeView["connectedAt"].(float64); !ok || int64(value) != connectedAt {
		t.Fatalf("active update lost connectedAt: %v", activeView)
	}
	stored := fetchCall(t, stack, token, "line1:guard-1")
	if stored["state"] != "active" {
		t.Fatalf("stored state = %v, want active", stored["state"])
	}
	if value, ok := stored["connectedAt"].(float64); !ok || int64(value) != connectedAt {
		t.Fatalf("stored connectedAt = %v, want %d", stored["connectedAt"], connectedAt)
	}
}

// waitActiveWithReemit drives an inbound call to active, mirroring production
// worker polling: the answer RPC emits a non-blocking call.updated that can be
// dropped, so re-emit the active record until the unified record converges.
func waitActiveWithReemit(t *testing.T, stack *testStack, token, callID, workerCallID string, fake *fakeWorker) {
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

// Legacy WebRTC per-call Offer must reject a conference-merged leg with the
// same 409 CB-V2-409D as the WSS media route (a per-call attach would steal
// the worker's single-owner media socket), while the dedicated conference
// Offer route keeps serving the merged session.
func TestCallOfferRejectsConferencedLegAndConferenceRouteStillWorks(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	stack.fakes[0].injectIncoming("off-conf1", "+8613800000001")
	stack.fakes[1].injectIncoming("off-conf2", "+8613800000002")
	stack.waitForCall(token, "line1:off-conf1")
	stack.waitForCall(token, "line2:off-conf2")
	for _, entry := range []struct {
		callID       string
		workerCallID string
		fake         *fakeWorker
	}{
		{"line1:off-conf1", "off-conf1", stack.fakes[0]},
		{"line2:off-conf2", "off-conf2", stack.fakes[1]},
	} {
		if status := stack.jsonRequest(http.MethodPost, "/api/v2/calls/"+entry.callID+"/answer", token, "off-ans-"+entry.callID, map[string]string{}, nil); status != http.StatusCreated {
			t.Fatalf("answer %s status = %d", entry.callID, status)
		}
		waitActiveWithReemit(t, stack, token, entry.callID, entry.workerCallID, entry.fake)
	}
	var conference map[string]any
	status := stack.jsonRequest(http.MethodPost, "/api/v2/conferences", token, "off-merge",
		map[string][]string{"callIds": {"line1:off-conf1", "line2:off-conf2"}}, &conference)
	if status != http.StatusCreated {
		t.Fatalf("merge status = %d, want 201", status)
	}
	conferenceID, _ := conference["id"].(string)
	if conferenceID == "" {
		t.Fatalf("merge returned no conference id: %v", conference)
	}

	pc, sdp := probeClient(t)
	defer pc.Close()
	body, _ := json.Marshal(map[string]string{"type": "offer", "sdp": sdp})

	// Per-call legacy WebRTC offer on the merged leg: explicit 409 CB-V2-409D.
	status, payload := stack.request(http.MethodPost, "/api/v2/calls/line1:off-conf1/webrtc/offer", token, "off-call", body)
	if status != http.StatusConflict {
		t.Fatalf("conferenced per-call offer status = %d body=%s, want 409", status, payload)
	}
	var failure map[string]any
	if err := json.Unmarshal(payload, &failure); err != nil || failure["code"] != "CB-V2-409D" {
		t.Fatalf("conferenced per-call offer code = %s (%v), want CB-V2-409D", payload, err)
	}

	// The conference Offer route is preserved and answers with SDP.
	status, payload = stack.request(http.MethodPost, "/api/v2/conferences/"+conferenceID+"/webrtc/offer", token, "off-conf", body)
	if status != http.StatusOK {
		t.Fatalf("conference offer status = %d body=%s, want 200", status, payload)
	}
	var answer map[string]any
	if err := json.Unmarshal(payload, &answer); err != nil {
		t.Fatalf("decode conference offer: %v (%s)", err, payload)
	}
	if sdpAnswer, _ := answer["sdp"].(string); sdpAnswer == "" {
		t.Fatalf("conference offer returned no SDP: %s", payload)
	}
}

// A non-conferenced call keeps the legacy per-call Offer path working.
func TestCallOfferNonConferencedStillWorks(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	view := dialOutbound(t, stack, token, "offer-ok")
	callID, _ := view["id"].(string)
	connectedAt := time.Now().UnixMilli()
	blockingInject(t, stack.fakes[0], "cellular_ready", workerRecord("offer-ok", "connecting", nil, nil, ""))
	blockingInject(t, stack.fakes[0], "media_ready", workerRecord("offer-ok", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	pc, sdp := probeClient(t)
	defer pc.Close()
	body, _ := json.Marshal(map[string]string{"type": "offer", "sdp": sdp})
	status, payload := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/webrtc/offer", token, "offer-ok-1", body)
	if status != http.StatusOK {
		t.Fatalf("per-call offer status = %d body=%s, want 200", status, payload)
	}
	var answer map[string]any
	if err := json.Unmarshal(payload, &answer); err != nil {
		t.Fatalf("decode offer: %v (%s)", err, payload)
	}
	if sdpAnswer, _ := answer["sdp"].(string); sdpAnswer == "" {
		t.Fatalf("per-call offer returned no SDP: %s", payload)
	}
}
