package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// threadList fetches one folder for a token and decodes the JSON list.
func threadList(t *testing.T, stack *testStack, token, query string) []map[string]any {
	t.Helper()
	var views []map[string]any
	status := stack.jsonRequest(http.MethodGet, "/api/v2/threads"+query, token, "", nil, &views)
	if status != http.StatusOK {
		t.Fatalf("threads%s status = %d", query, status)
	}
	return views
}

func threadByKey(views []map[string]any, key string) map[string]any {
	for _, view := range views {
		if view["key"] == key {
			return view
		}
	}
	return nil
}

// TestSMSQuarantineColdClientRestoreAndIsolation is the accepted recoverable
// junk contract: a cold same-key client sees the quarantined conversation and
// its text, restore pins it normal (never re-quarantined on refresh), and an
// unrelated key on the same line sees only its own normal conversation.
func TestSMSQuarantineColdClientRestoreAndIsolation(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "A-phone")
	tokenB, _ := stack.enroll(keyB, secretB, "B-phone")

	const sender = "+8613600000010"
	const threadKey = "line1-quarantine-1"
	const globalKey = "line1:" + threadKey
	if status := putHarassSettings(t, stack, tokenA, false, true, nil); status != http.StatusOK {
		t.Fatalf("A settings")
	}
	addHarassEntry(t, stack, tokenA, sender, "exact", "block", "reject")
	messageID := stack.fakes[0].seedThread(threadKey, sender, "退订回T 贷款推广", 1, time.Now().UnixMilli())

	// A cold client: normal folder hides it, junk folder lists it, and the
	// authorized key can read the quarantined text.
	if view := threadByKey(threadList(t, stack, tokenA, ""), globalKey); view != nil {
		t.Fatalf("quarantined thread in A normal folder: %+v", view)
	}
	junk := threadByKey(threadList(t, stack, tokenA, "?folder=junk"), globalKey)
	if junk == nil || junk["folder"] != "junk" {
		t.Fatalf("A junk folder missing the conversation: %+v", junk)
	}
	status, payload := stack.request(http.MethodGet,
		"/api/v2/messages?threadKey="+globalKey+"&limit=50", tokenA, "", nil)
	if status != http.StatusOK || !containsSub(payload, []byte("贷款推广")) {
		t.Fatalf("A cannot read its quarantined text: %d %s", status, payload)
	}

	// Restore: normal, pinned, and still normal after a refresh.
	status, payload = stack.request(http.MethodPost, "/api/v2/threads/"+globalKey+"/restore", tokenA, "", nil)
	if status != http.StatusOK {
		t.Fatalf("restore status = %d body=%s", status, payload)
	}
	for attempt := 0; attempt < 2; attempt++ {
		view := threadByKey(threadList(t, stack, tokenA, ""), globalKey)
		if view == nil || view["folder"] != "normal" {
			t.Fatalf("restored thread not normal on refresh %d: %+v", attempt, view)
		}
		if threadByKey(threadList(t, stack, tokenA, "?folder=junk"), globalKey) != nil {
			t.Fatalf("restored thread re-quarantined on refresh %d", attempt)
		}
	}
	// The restored message is still readable.
	status, payload = stack.request(http.MethodGet,
		"/api/v2/messages?threadKey="+globalKey+"&limit=50", tokenA, "", nil)
	if status != http.StatusOK || !containsSub(payload, []byte("贷款推广")) {
		t.Fatalf("restored text unreadable: %d %s", status, payload)
	}

	// Unrelated key: normal folder, no junk leak.
	view := threadByKey(threadList(t, stack, tokenB, ""), globalKey)
	if view == nil || view["folder"] != "normal" {
		t.Fatalf("B normal view wrong: %+v", view)
	}
	if threadByKey(threadList(t, stack, tokenB, "?folder=junk"), globalKey) != nil {
		t.Fatalf("A's junk leaked to B")
	}
	_ = messageID
}

// TestSMSQuarantineDeleteAndReadAreKeyScoped proves per-key delete/unread:
// neither touches the other key's view nor the underlying worker state, and a
// later message reopens the conversation for the deleting key only.
func TestSMSQuarantineDeleteAndReadAreKeyScoped(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "A")
	tokenB, _ := stack.enroll(keyB, secretB, "B")

	const peer = "+8613600000011"
	const threadKey = "line1-scope-1"
	const globalKey = "line1:" + threadKey
	messageID := stack.fakes[0].seedThread(threadKey, peer, "普通消息", 1, time.Now().UnixMilli())

	// B reads nothing yet; A marks the message read. Only A's unread drops,
	// and the worker's global read endpoint is NEVER called.
	status, payload := stack.request(http.MethodPost, "/api/v2/messages/line1:"+messageID+"/read", tokenA, "", nil)
	if status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("A read status = %d %s", status, payload)
	}
	time.Sleep(100 * time.Millisecond)
	if view := threadByKey(threadList(t, stack, tokenA, ""), globalKey); view == nil || view["unreadCount"].(float64) != 0 {
		t.Fatalf("A unread not cleared: %+v", view)
	}
	if view := threadByKey(threadList(t, stack, tokenB, ""), globalKey); view == nil || view["unreadCount"].(float64) != 1 {
		t.Fatalf("B unread was affected by A's read: %+v", view)
	}
	if calls := stack.fakes[0].readCallsLocked(); calls != 0 {
		t.Fatalf("per-key read touched the shared worker read state: %d", calls)
	}

	// A deletes the conversation: A hides it, B keeps it, the worker keeps
	// the raw message and thread.
	status, payload = stack.request(http.MethodDelete, "/api/v2/threads/"+globalKey, tokenA, "", nil)
	if status != http.StatusOK {
		t.Fatalf("A delete status = %d %s", status, payload)
	}
	if threadByKey(threadList(t, stack, tokenA, ""), globalKey) != nil ||
		threadByKey(threadList(t, stack, tokenA, "?folder=junk"), globalKey) != nil {
		t.Fatalf("deleted thread still visible for A")
	}
	if view := threadByKey(threadList(t, stack, tokenB, ""), globalKey); view == nil {
		t.Fatalf("B lost the conversation because A deleted it")
	}
	stack.fakes[0].mu.Lock()
	rawThreads, rawMessages := len(stack.fakes[0].threads), len(stack.fakes[0].messages)
	stack.fakes[0].mu.Unlock()
	if rawThreads == 0 || rawMessages == 0 {
		t.Fatalf("A's delete erased shared worker data: threads=%d messages=%d", rawThreads, rawMessages)
	}

	// A NEW message after the delete reopens the conversation for A.
	stack.fakes[0].seedThread(threadKey, peer, "新消息", 1, time.Now().UnixMilli()+50)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if view := threadByKey(threadList(t, stack, tokenA, ""), globalKey); view != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("a new message did not reopen the thread for A")
}

// TestSMSQuarantineAccessControl: revoked keys are denied and a line a key
// was never granted is inaccessible.
func TestSMSQuarantineAccessControl(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(false) // explicit line1 grant only
	tokenA, _ := stack.enroll(keyA, secretA, "A")
	tokenB, _ := stack.enroll(keyB, secretB, "B")
	stack.fakes[0].seedThread("line1-access", "+8613600000012", "x", 1, time.Now().UnixMilli())

	// B has an explicit line1 grant; a line2 thread must still be refused.
	status, payload := stack.request(http.MethodGet, "/api/v2/messages?threadKey=line2:whatever&limit=10", tokenB, "", nil)
	if status != http.StatusForbidden {
		t.Fatalf("unassigned-line messages status = %d body=%s, want 403", status, payload)
	}
	// Revocation cuts the junk listing immediately.
	if err := stack.auth.RevokeKey(context.Background(), keyA); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	status, _ = stack.request(http.MethodGet, "/api/v2/threads?folder=junk", tokenA, "", nil)
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("revoked key junk listing status = %d, want 401/403", status)
	}
}

// TestSMSQuarantineCapabilityDeclaration advertises the overlay so clients can
// switch between gateway-managed and local-only junk handling.
func TestSMSQuarantineCapabilityDeclaration(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "A")
	var device struct {
		Capabilities map[string]bool `json:"capabilities"`
	}
	status := stack.jsonRequest(http.MethodGet, "/api/v2/device", token, "", nil, &device)
	if status != http.StatusOK || !device.Capabilities["smsQuarantine"] || !device.Capabilities["smsKeyOverlay"] {
		t.Fatalf("capabilities = %d %+v", status, device.Capabilities)
	}
}

// TestSMSQuarantineThreadFolderJSON verifies the wire shape stays stable.
func TestSMSQuarantineThreadFolderJSON(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "A")
	stack.fakes[0].seedThread("line1-shape", "+8613600000013", "hi", 0, time.Now().UnixMilli())
	status, payload := stack.request(http.MethodGet, "/api/v2/threads", token, "", nil)
	if status != http.StatusOK {
		t.Fatalf("threads status = %d", status)
	}
	var decoded []struct {
		Key    string `json:"key"`
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode threads: %v", err)
	}
	found := false
	for _, view := range decoded {
		if view.Key == "line1:line1-shape" {
			found = true
			if view.Folder != "normal" {
				t.Fatalf("folder wire shape = %+v", view)
			}
		}
	}
	if !found {
		t.Fatalf("seeded thread missing from decode: %+v", decoded)
	}
}
