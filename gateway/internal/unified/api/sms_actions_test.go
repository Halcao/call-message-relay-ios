package api

import (
	"callrelay/gateway/internal/unified/events"
	"callrelay/gateway/internal/unified/runtime"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func actionRequest(t *testing.T, stack *testStack, token string, actions ...runtime.SMSThreadAction) []runtime.SMSActionResult {
	t.Helper()
	var response struct {
		Results []runtime.SMSActionResult `json:"results"`
	}
	status := stack.jsonRequest(http.MethodPost, "/api/v2/threads/actions", token, "", map[string]any{"actions": actions}, &response)
	if status != 200 {
		t.Fatalf("actions HTTP %d", status)
	}
	return response.Results
}

func TestSMSThreadActionsPartialReplayTupleAndKeyIsolation(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "A")
	deviceA := stack.deviceID
	tokenB, _ := stack.enroll(keyB, secretB, "B")
	deviceB := stack.deviceID
	ctx := context.Background()
	at := time.Now().UnixMilli()
	const thread = "actions"
	const global = "line1:actions"
	fake := stack.fakes[0]
	fake.mu.Lock()
	fake.strictThreadMessages = true
	old := map[string]any{"id": "m2", "threadKey": thread, "direction": "inbound", "peer": "+12025550123", "body": "old", "status": "received", "createdAt": float64(at)}
	fake.messages = []map[string]any{old}
	fake.threads = []map[string]any{{"key": thread, "peer": "+12025550123", "unreadCount": 1, "lastMessage": old}}
	fake.mu.Unlock()
	read := runtime.SMSThreadAction{ID: "read1", Kind: "read", ThreadKey: global, ThroughCreatedAt: at, ThroughID: "line1:m2"}
	invalid := read
	invalid.ID = "invalid"
	invalid.ThroughID = "line1:missing"
	beforeRead := stack.rawGet("/api/v2/threads", tokenA, "")
	beforeReadB := stack.rawGet("/api/v2/threads", tokenB, "")
	results := actionRequest(t, stack, tokenA, read, invalid)
	if len(results) != 2 || !results[0].OK || results[1].OK || results[1].Error == nil || results[1].Error.Retryable {
		t.Fatalf("partial %+v", results)
	}
	if got := threadByKey(threadList(t, stack, tokenA, ""), global); got == nil || got["unreadCount"].(float64) != 0 {
		t.Fatalf("A unread %+v", got)
	}
	if got := threadByKey(threadList(t, stack, tokenB, ""), global); got == nil || got["unreadCount"].(float64) != 1 {
		t.Fatalf("B unread %+v", got)
	}
	if got := stack.rawGet("/api/v2/threads", tokenA, beforeRead.header.Get("ETag")); got.status != 200 {
		t.Fatalf("read ETag stale %d", got.status)
	}
	if got := stack.rawGet("/api/v2/threads", tokenB, beforeReadB.header.Get("ETag")); got.status != 304 {
		t.Fatalf("other key ETag changed %d", got.status)
	}
	storedEvents, err := stack.store.EventsAfter(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	accessA, _ := stack.store.DeviceAccess(ctx, deviceA)
	accessB, _ := stack.store.DeviceAccess(ctx, deviceB)
	foundRead := false
	for _, stored := range storedEvents {
		if stored.Type != "thread.read" {
			continue
		}
		foundRead = true
		var payload map[string]any
		if json.Unmarshal(stored.Payload, &payload) != nil || payload["throughID"] != "line1:m2" {
			t.Fatalf("read event %s", stored.Payload)
		}
		event := events.Event{LineID: stored.LineID, Type: stored.Type, Data: stored.Payload}
		if !stack.api.eventAllowed(ctx, deviceA, accessA, event) || stack.api.eventAllowed(ctx, deviceB, accessB, event) {
			t.Fatal("read event key isolation")
		}
	}
	if !foundRead {
		t.Fatal("missing read event")
	}
	// Lost response replay must not expand a deletion over a newer same-time ID.
	del := read
	del.ID = "delete1"
	del.Kind = "delete"
	if !actionRequest(t, stack, tokenA, del)[0].OK {
		t.Fatal("delete")
	}
	fake.mu.Lock()
	newer := map[string]any{"id": "m3", "threadKey": thread, "direction": "inbound", "peer": "+12025550123", "body": "new", "status": "received", "createdAt": float64(at)}
	fake.messages = append(fake.messages, newer)
	fake.threads[0]["lastMessage"] = newer
	fake.mu.Unlock()
	if !actionRequest(t, stack, tokenA, del)[0].OK {
		t.Fatal("replay")
	}
	got := threadByKey(threadList(t, stack, tokenA, ""), global)
	if got == nil || got["unreadCount"].(float64) != 1 {
		t.Fatalf("new tie hidden/read %+v", got)
	}
	var messages []map[string]any
	if status := stack.jsonRequest(http.MethodGet, "/api/v2/messages?threadKey="+global, tokenA, "", nil, &messages); status != 200 || len(messages) != 1 || messages[0]["id"] != "line1:m3" {
		t.Fatalf("filtered messages %d %+v", status, messages)
	}
	conflict := del
	conflict.ThroughID = "line1:m3"
	if result := actionRequest(t, stack, tokenA, conflict)[0]; result.OK || result.Error == nil || result.Error.Code != "CB-V2-409" {
		t.Fatalf("conflict %+v", result)
	}
	if err := stack.store.SetEnrollmentKeyRevoked(ctx, keyA, true); err != nil {
		t.Fatal(err)
	}
	status := stack.jsonRequest(http.MethodPost, "/api/v2/threads/actions", tokenA, "", map[string]any{"actions": []runtime.SMSThreadAction{read}}, nil)
	if status != 401 && status != 403 {
		t.Fatalf("revoked status %d", status)
	}
}

func TestSMSThreadActionsColdBoundaryBeyondFourPages(t *testing.T) {
	stack := newTestStack(t)
	key, secret := stack.createKey(true)
	token, _ := stack.enroll(key, secret, "A")
	fake := stack.fakes[0]
	fake.mu.Lock()
	fake.strictThreadMessages = true
	base := time.Now().UnixMilli()
	for i := 0; i < 1100; i++ {
		fake.messages = append(fake.messages, map[string]any{"id": fmt.Sprintf("m%04d", i), "threadKey": "deep", "direction": "inbound", "peer": "+12025550123", "body": "fixture", "status": "received", "createdAt": float64(base + int64(i))})
	}
	fake.mu.Unlock()
	action := runtime.SMSThreadAction{ID: "deep-read", Kind: "read", ThreadKey: "line1:deep", ThroughCreatedAt: base, ThroughID: "line1:m0000"}
	if result := actionRequest(t, stack, token, action)[0]; !result.OK {
		t.Fatalf("deep boundary %+v", result)
	}
	var device struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	if status := stack.jsonRequest(http.MethodGet, "/api/v2/device", token, "", nil, &device); status != 200 || !device.Capabilities["smsThreadActions"] {
		t.Fatalf("capability %d %+v", status, device)
	}
}
