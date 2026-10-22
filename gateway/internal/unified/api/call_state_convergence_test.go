package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These tests reproduce the 2026-10-03 live failure where real outbound calls
// stayed "connecting" on the phone even though the remote had answered, then
// silently ended ~9-10s later:
//
//   - the worker's cellular_ready/media_ready events were forwarded raw
//     (broadcast) but never normalized into the unified call record, so the
//     record never reached "active" and answered_at stayed NULL;
//   - the HTTP hangup path then upserted a stale pre-answer snapshot,
//     overwriting answered_at back to NULL and emitting a second,
//     inconsistent call.ended (first carrying connectedAt, second null).

func dialOutbound(t *testing.T, stack *testStack, token, clientCallID string) map[string]any {
	t.Helper()
	var view map[string]any
	status := stack.jsonRequest(http.MethodPost, "/api/v2/calls", token, "dial-"+clientCallID,
		map[string]string{"lineId": "line1", "to": "10086", "clientCallId": clientCallID}, &view)
	if status != http.StatusCreated {
		t.Fatalf("dial status = %d, want 201", status)
	}
	return view
}

func fetchCall(t *testing.T, stack *testStack, token, callID string) map[string]any {
	t.Helper()
	var view map[string]any
	status := stack.jsonRequest(http.MethodGet, "/api/v2/calls/"+callID, token, "", nil, &view)
	if status != http.StatusOK {
		t.Fatalf("fetch %s status = %d, want 200", callID, status)
	}
	return view
}

func waitForCallState(t *testing.T, stack *testStack, token, callID, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		view := fetchCall(t, stack, token, callID)
		if view["state"] == want {
			return view
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("call %s did not reach state %q", callID, want)
	return nil
}

func readCallEvent(t *testing.T, conn *websocket.Conn, callID, eventType string, timeout time.Duration) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read event %s/%s: %v", callID, eventType, err)
		}
		var event struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		if event.Type != eventType {
			continue
		}
		var data map[string]any
		if json.Unmarshal(event.Data, &data) != nil || data["id"] != callID {
			continue
		}
		return data
	}
}

func workerRecord(callID, state string, connectedAt, endedAt *int64, endReason string) workerCall {
	return workerCall{
		ID: callID, GatewayID: "gw-line1", LineID: "line1:line", Direction: "outbound",
		Peer: "10086", State: state, StartedAt: time.Now().UnixMilli() - 5000,
		ConnectedAt: connectedAt, EndedAt: endedAt, EndReason: endReason,
	}
}

// The unified record must converge dialing -> connecting -> active from the
// worker's readiness events, and the owner device must receive device-scoped
// call.updated events carrying state/connectedAt (the phone's "connected"
// signal). Before the fix the record stayed "outgoing_dialing" forever.
func TestWorkerReadinessEventsActivateOutboundCall(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	header := http.Header{"Authorization": []string{"Bearer " + token}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/events"
	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), url, header)
	if err != nil {
		t.Fatalf("events dial: %v", err)
	}
	defer conn.Close()

	view := dialOutbound(t, stack, token, "conv-ready")
	callID, _ := view["id"].(string)
	if state, _ := view["state"].(string); state != "outgoing_dialing" {
		t.Fatalf("initial state = %q, want outgoing_dialing", state)
	}
	// Consume the dial's own call.updated echo so the readiness events below
	// are matched unambiguously.
	if echo := readCallEvent(t, conn, callID, "call.updated", 3*time.Second); echo["state"] != "outgoing_dialing" {
		t.Fatalf("dial echo state = %v, want outgoing_dialing", echo["state"])
	}

	// Worker accepted the ATD: cellular_ready moves the call to connecting.
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("conv-ready", "connecting", nil, nil, ""))
	connecting := readCallEvent(t, conn, callID, "call.updated", 3*time.Second)
	if state, _ := connecting["state"].(string); state != "connecting" {
		t.Fatalf("cellular_ready call.updated state = %q, want connecting", state)
	}
	waitForCallState(t, stack, token, callID, "connecting")

	// Worker audio bridge up after the remote answered: media_ready is the
	// answer signal and must carry connectedAt to the owner device.
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("conv-ready", "active", &connectedAt, nil, ""))
	active := readCallEvent(t, conn, callID, "call.updated", 3*time.Second)
	if state, _ := active["state"].(string); state != "active" {
		t.Fatalf("media_ready call.updated state = %q, want active", state)
	}
	if _, ok := active["connectedAt"].(float64); !ok {
		t.Fatalf("media_ready call.updated missing connectedAt: %v", active)
	}
	stored := waitForCallState(t, stack, token, callID, "active")
	if _, ok := stored["connectedAt"].(float64); !ok {
		t.Fatalf("stored call missing connectedAt after media_ready: %v", stored)
	}
}

// A device hangup after the call reached active must preserve answered truth:
// exactly one terminal event, and the stored call keeps connectedAt. Before
// the fix the HTTP hangup path re-upserted its stale pre-answer snapshot
// (answered_at -> NULL) and emitted a second call.ended without connectedAt.
func TestHangupAfterAnswerPreservesConnectedAtAndSingleTerminalEvent(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	header := http.Header{"Authorization": []string{"Bearer " + token}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/events"
	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), url, header)
	if err != nil {
		t.Fatalf("events dial: %v", err)
	}
	defer conn.Close()

	view := dialOutbound(t, stack, token, "conv-term")
	callID, _ := view["id"].(string)
	_ = readCallEvent(t, conn, callID, "call.updated", 3*time.Second) // dial echo
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("conv-term", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("conv-term", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	// The fake worker mirrors the real terminal sequence (hangup -> call.ended
	// events) during the Hangup HTTP call, reproducing the observed race.
	status, _ := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/hangup", token, "term-1", nil)
	if status != http.StatusNoContent {
		t.Fatalf("hangup status = %d, want 204", status)
	}

	ended := readCallEvent(t, conn, callID, "call.ended", 3*time.Second)
	if _, ok := ended["connectedAt"].(float64); !ok {
		t.Fatalf("terminal call.ended lost connectedAt: %v", ended)
	}
	// Exactly one terminal event: a second one would overwrite truth with the
	// stale snapshot (the production bug). Drain briefly and count.
	_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
	terminalCount := 1
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var event struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(payload, &event) != nil || event.Type != "call.ended" {
			continue
		}
		var data map[string]any
		if json.Unmarshal(event.Data, &data) == nil && data["id"] == callID {
			terminalCount++
		}
	}
	if terminalCount != 1 {
		t.Fatalf("call.ended published %d times for one call, want 1", terminalCount)
	}

	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "ended" {
		t.Fatalf("stored state = %v, want ended", stored["state"])
	}
	if _, ok := stored["connectedAt"].(float64); !ok {
		t.Fatalf("stored call lost connectedAt after hangup: %v", stored)
	}
	if reason, _ := stored["endReason"].(string); reason == "" {
		t.Fatalf("stored call missing endReason: %v", stored)
	}
}

// TestWorkerTerminalSnapshotWithoutReasonStillConverges deterministically
// reproduces the intermittent convergence failure observed in
// TestHangupAfterAnswerPreservesConnectedAtAndSingleTerminalEvent: the worker's
// terminal snapshot can WIN the atomic claim while carrying NO end reason
// (kind "call.ended" with endedAt but an empty endReason), and the unified row
// then ended with a blank reason. The worker event is injected FIRST here, so
// the ordering is fixed rather than raced.
func TestWorkerTerminalSnapshotWithoutReasonStillConverges(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	header := http.Header{"Authorization": []string{"Bearer " + token}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/events"
	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), url, header)
	if err != nil {
		t.Fatalf("events dial: %v", err)
	}
	defer conn.Close()

	view := dialOutbound(t, stack, token, "conv-blank")
	callID, _ := view["id"].(string)
	_ = readCallEvent(t, conn, callID, "call.updated", 3*time.Second)
	connectedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("cellular_ready", workerRecord("conv-blank", "connecting", nil, nil, ""))
	stack.fakes[0].injectWorkerEvent("media_ready", workerRecord("conv-blank", "active", &connectedAt, nil, ""))
	waitForCallState(t, stack, token, callID, "active")

	// The worker's own terminal snapshot, with NO reason at all.
	endedAt := time.Now().UnixMilli()
	stack.fakes[0].injectWorkerEvent("call.ended", workerRecord("conv-blank", "ended", nil, &endedAt, ""))
	waitForCallState(t, stack, token, callID, "ended")

	stored := fetchCall(t, stack, token, callID)
	if stored["state"] != "ended" {
		t.Fatalf("stored state = %v, want ended", stored["state"])
	}
	if _, ok := stored["connectedAt"].(float64); !ok {
		t.Fatalf("terminal convergence lost connectedAt: %v", stored)
	}
	if reason, _ := stored["endReason"].(string); reason == "" {
		t.Fatalf("terminal convergence persisted an EMPTY end reason: %v", stored)
	}
	// The HTTP hangup after the worker already ended the call must not produce
	// a second terminal transition or change the recorded reason.
	before, _ := stored["endReason"].(string)
	status, _ := stack.request(http.MethodPost, "/api/v2/calls/"+callID+"/hangup", token, "blank-2", nil)
	if status != http.StatusNoContent && status != http.StatusConflict {
		t.Fatalf("hangup after terminal status = %d", status)
	}
	after := fetchCall(t, stack, token, callID)
	if reason, _ := after["endReason"].(string); reason != before {
		t.Fatalf("terminal reason changed after a late hangup: %q -> %q", before, reason)
	}
}
