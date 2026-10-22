package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/store"

	"github.com/gorilla/websocket"
)

func putHarassSettings(t *testing.T, stack *testStack, token string, callBlocking, smsBlocking bool, version *int64) int {
	t.Helper()
	body := map[string]any{"callBlocking": callBlocking, "smsBlocking": smsBlocking}
	if version != nil {
		body["version"] = *version
	}
	return stack.jsonRequest(http.MethodPut, "/api/v2/harassment/settings", token, "", body, nil)
}

func addHarassEntry(t *testing.T, stack *testStack, token, number, matchKind, mode, action string) string {
	t.Helper()
	var response struct {
		Entry struct {
			ID string `json:"id"`
		} `json:"entry"`
	}
	status := stack.jsonRequest(http.MethodPost, "/api/v2/harassment/entries", token, "", map[string]any{
		"number": number, "matchKind": matchKind, "mode": mode, "action": action,
	}, &response)
	if status != http.StatusOK || response.Entry.ID == "" {
		t.Fatalf("add entry status = %d", status)
	}
	return response.Entry.ID
}

// TestHarassmentKeyScopeIsolation proves the accepted isolation contract:
// two keys on the same physical line have completely separate settings,
// lists and history, a foreign id is 404, and devices of ONE key share it.
func TestHarassmentKeyScopeIsolation(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(false) // explicit line1 grant
	tokenA, _ := stack.enroll(keyA, secretA, "A-phone")
	tokenA2, _ := stack.enroll(keyA, secretA, "A-tablet")
	tokenB, _ := stack.enroll(keyB, secretB, "B-phone")

	if status := putHarassSettings(t, stack, tokenA, true, true, nil); status != http.StatusOK {
		t.Fatalf("key A settings status = %d", status)
	}
	entryA := addHarassEntry(t, stack, tokenA, "+8613800000001", "exact", "block", "reject")
	var importResponse struct {
		List struct {
			ID string `json:"id"`
		} `json:"list"`
	}
	status := stack.jsonRequest(http.MethodPost, "/api/v2/harassment/lists/import", tokenA, "", map[string]any{
		"name": "历史名单", "numbers": []string{"+8613800000002", "+8613800000003"},
	}, &importResponse)
	if status != http.StatusOK || importResponse.List.ID == "" {
		t.Fatalf("import status = %d", status)
	}

	// Same key, second device: shares the exact policy.
	var settings struct {
		Settings struct {
			CallBlocking bool  `json:"callBlocking"`
			SMSBlocking  bool  `json:"smsBlocking"`
			Version      int64 `json:"version"`
		} `json:"settings"`
	}
	status = stack.jsonRequest(http.MethodGet, "/api/v2/harassment/settings", tokenA2, "", nil, &settings)
	if status != http.StatusOK || !settings.Settings.CallBlocking || !settings.Settings.SMSBlocking {
		t.Fatalf("same-key device did not share settings: %d %+v", status, settings)
	}

	// Different key: default-allow, no entries/lists/history.
	var settingsB struct {
		Settings struct {
			CallBlocking bool `json:"callBlocking"`
			SMSBlocking  bool `json:"smsBlocking"`
		} `json:"settings"`
		UnknownAction string `json:"unknownAction"`
	}
	status = stack.jsonRequest(http.MethodGet, "/api/v2/harassment/settings", tokenB, "", nil, &settingsB)
	if status != http.StatusOK || settingsB.Settings.CallBlocking || settingsB.Settings.SMSBlocking || settingsB.UnknownAction != "allow" {
		t.Fatalf("key B settings = %d %+v", status, settingsB)
	}
	var entries struct {
		Entries []map[string]any `json:"entries"`
	}
	status = stack.jsonRequest(http.MethodGet, "/api/v2/harassment/entries", tokenB, "", nil, &entries)
	if status != http.StatusOK || len(entries.Entries) != 0 {
		t.Fatalf("key B saw key A entries: %d %+v", status, entries.Entries)
	}

	// Foreign ids are 404 for both entries and lists.
	if status, _ := stack.request(http.MethodDelete, "/api/v2/harassment/entries/"+entryA, tokenB, "", nil); status != http.StatusNotFound {
		t.Fatalf("cross-key entry delete status = %d, want 404", status)
	}
	if status, _ := stack.request(http.MethodDelete, "/api/v2/harassment/lists/"+importResponse.List.ID, tokenB, "", nil); status != http.StatusNotFound {
		t.Fatalf("cross-key list delete status = %d, want 404", status)
	}
	// The owner can delete their own entry.
	if status, _ := stack.request(http.MethodDelete, "/api/v2/harassment/entries/"+entryA, tokenA, "", nil); status != http.StatusOK {
		t.Fatalf("owner entry delete status = %d", status)
	}
}

// TestHarassmentAllDeniedRejectsAndNoVoicemail: every normally authorized
// receiving key denies the caller -> direct upstream reject, no voicemail.
func TestHarassmentAllDeniedRejectsAndNoVoicemail(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "A")
	tokenB, _ := stack.enroll(keyB, secretB, "B")

	const caller = "+8613900000001"
	if status := putHarassSettings(t, stack, tokenA, true, true, nil); status != http.StatusOK {
		t.Fatalf("A settings")
	}
	addHarassEntry(t, stack, tokenA, caller, "exact", "block", "reject")
	if status := putHarassSettings(t, stack, tokenB, true, true, nil); status != http.StatusOK {
		t.Fatalf("B settings")
	}
	addHarassEntry(t, stack, tokenB, caller, "exact", "block", "reject")

	stack.fakes[0].injectIncoming("call-all-denied", caller)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		call, err := stack.store.GetCall(context.Background(), "line1:call-all-denied")
		if err == nil && call.EndedAt != nil {
			if call.EndReason != "harassment_reject" {
				t.Fatalf("end reason = %q, want harassment_reject", call.EndReason)
			}
			if stack.fakes[0].rejects() != 1 {
				t.Fatalf("worker rejects = %d, want 1", stack.fakes[0].rejects())
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("all-denied call was never directly rejected")
}

// TestHarassmentPartialDenyRingsAllowedKeyOnly: key A blocks, key B allows.
// B sees the ring and the worker is NOT rejected; A sees nothing at all.
func TestHarassmentPartialDenyRingsAllowedKeyOnly(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "A")
	tokenB, _ := stack.enroll(keyB, secretB, "B")

	const caller = "+8613900000002"
	if status := putHarassSettings(t, stack, tokenA, true, true, nil); status != http.StatusOK {
		t.Fatalf("A settings")
	}
	addHarassEntry(t, stack, tokenA, caller, "exact", "block", "reject")

	stack.fakes[0].injectIncoming("call-partial", caller)
	// B sees the ringing call.
	call := stack.waitForCall(tokenB, "line1:call-partial")
	if state, _ := call["state"].(string); state != "incoming_ringing" {
		t.Fatalf("B state = %q, want incoming_ringing", state)
	}
	// A never sees it, even by direct id.
	status, payload := stack.request(http.MethodGet, "/api/v2/calls/line1:call-partial", tokenA, "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("A call view status = %d body=%s, want 404", status, payload)
	}
	status, payload = stack.request(http.MethodGet, "/api/v2/calls?active=true", tokenA, "", nil)
	if status != http.StatusOK || containsSub(payload, []byte("call-partial")) {
		t.Fatalf("A active calls leaked: %d %s", status, payload)
	}
	if stack.fakes[0].rejects() != 0 {
		t.Fatalf("worker was rejected despite an allowing key: %d", stack.fakes[0].rejects())
	}
}

// TestHarassmentTrustBeatsBlockAndTestEndpoint covers precedence and the
// advanced testing endpoint (call + SMS keyword/regex).
func TestHarassmentTrustBeatsBlockAndTestEndpoint(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "A")
	if status := putHarassSettings(t, stack, token, true, true, nil); status != http.StatusOK {
		t.Fatalf("settings")
	}
	// Block a whole prefix, then explicitly trust one number inside it.
	addHarassEntry(t, stack, token, "139", "prefix", "block", "reject")
	addHarassEntry(t, stack, token, "+8613912345678", "exact", "trust", "")
	// Keyword + regex SMS rules.
	addHarassEntry(t, stack, token, "发票", "keyword", "block", "reject")
	addHarassEntry(t, stack, token, `(?i)投资.*高回报`, "regex", "block", "reject")

	eval := func(kind, number, body string) map[string]any {
		var out map[string]any
		status := stack.jsonRequest(http.MethodPost, "/api/v2/harassment/test", token, "", map[string]any{
			"kind": kind, "number": number, "body": body,
		}, &out)
		if status != http.StatusOK {
			t.Fatalf("test endpoint status = %d", status)
		}
		return out
	}
	if decision := eval("call", "+8613912345678", ""); decision["allow"] != true || decision["reason"] != "trusted" {
		t.Fatalf("trusted number decision = %+v", decision)
	}
	if decision := eval("call", "+8613999999999", ""); decision["allow"] != false || decision["action"] != "reject" {
		t.Fatalf("prefix block decision = %+v", decision)
	}
	if decision := eval("sms", "+8613711111111", "给您开一张发票"); decision["allow"] != false || decision["reason"] != "keyword" {
		t.Fatalf("keyword decision = %+v", decision)
	}
	if decision := eval("sms", "+8613711111111", "投资稳赚高回报"); decision["allow"] != false || decision["reason"] != "regex" {
		t.Fatalf("regex decision = %+v", decision)
	}
	if decision := eval("call", "+8613711111111", ""); decision["allow"] != true || decision["reason"] != "default_allow" {
		t.Fatalf("unknown caller decision = %+v", decision)
	}
	// Keyword/regex rules never apply to calls.
	if decision := eval("call", "+8613711111111", "发票"); decision["allow"] != true {
		t.Fatalf("keyword leaked into call decision: %+v", decision)
	}
}

// TestHarassmentImportedListDefaultsLabel: a bulk historical import must
// never start rejecting by itself.
func TestHarassmentImportedListDefaultsLabel(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "A")
	if status := putHarassSettings(t, stack, token, true, true, nil); status != http.StatusOK {
		t.Fatalf("settings")
	}
	const sender = "+8613600000001"
	status := stack.jsonRequest(http.MethodPost, "/api/v2/harassment/lists/import", token, "", map[string]any{
		"name": "旧手机导入", "numbers": []string{sender},
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("import status = %d", status)
	}
	// The imported entry is label, so the SMS still delivers.
	var decision map[string]any
	stack.jsonRequest(http.MethodPost, "/api/v2/harassment/test", token, "", map[string]any{
		"kind": "sms", "number": sender, "body": "您好",
	}, &decision)
	if decision["allow"] != true || decision["action"] != "label" {
		t.Fatalf("imported list decision = %+v, want label+allow", decision)
	}
	// History records the label decision.
	stack.fakes[0].injectIncomingMessage("sms-import-label", sender, "您好")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var history struct {
			Events []map[string]any `json:"events"`
		}
		status = stack.jsonRequest(http.MethodGet, "/api/v2/harassment/history", token, "", nil, &history)
		if status == http.StatusOK && len(history.Events) > 0 {
			if history.Events[0]["action"] != "label" {
				t.Fatalf("history action = %v, want label", history.Events[0]["action"])
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("label decision never reached the history")
}

// TestHarassmentSMSRecipientsFiltered: key A blocks the sender, key B does
// not. The live event stream (the pre-APNs recipient filter) delivers the
// message only to B.
func TestHarassmentSMSRecipientsFiltered(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "A")
	tokenB, _ := stack.enroll(keyB, secretB, "B")
	if status := putHarassSettings(t, stack, tokenA, true, true, nil); status != http.StatusOK {
		t.Fatalf("settings")
	}
	addHarassEntry(t, stack, tokenA, "+8613600000002", "exact", "block", "reject")

	dial := func(token string) *websocket.Conn {
		header := http.Header{"Authorization": []string{"Bearer " + token}}
		url := "ws" + stack.server.URL[len("http"):] + "/api/v2/events"
		conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), url, header)
		if err != nil {
			t.Fatalf("events dial: %v", err)
		}
		return conn
	}
	connA, connB := dial(tokenA), dial(tokenB)
	defer connA.Close()
	defer connB.Close()

	stack.fakes[0].seedThread("line1-sms-policy-1", "+8613600000002", "垃圾短信", 1, time.Now().UnixMilli())
	// B receives message.created for the line.
	_ = connB.SetReadDeadline(time.Now().Add(3 * time.Second))
	delivered := false
	for !delivered {
		_, payload, err := connB.ReadMessage()
		if err != nil {
			t.Fatalf("B never received the message event: %v", err)
		}
		var event map[string]any
		if json.Unmarshal(payload, &event) == nil && event["type"] == "message.created" {
			delivered = true
		}
	}
	// A must not receive the message event; other events may arrive.
	_ = connA.SetReadDeadline(time.Now().Add(1 * time.Second))
	for {
		_, payload, err := connA.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err) {
				t.Fatalf("A socket closed: %v", err)
			}
			break // timeout without a message event: isolation holds
		}
		if containsSub(payload, []byte("message.created")) && containsSub(payload, []byte("sms-policy-1")) {
			t.Fatalf("suppressed SMS leaked to key A: %s", payload)
		}
	}
	// No normal notification for A, but the SAME authorized key must be able
	// to read its own quarantined text through the junk folder; B (unrelated
	// key on the same line) sees it as a normal conversation.
	status, payload := stack.request(http.MethodGet, "/api/v2/threads", tokenA, "", nil)
	if status != http.StatusOK || containsSub(payload, []byte("line1-sms-policy-1")) {
		t.Fatalf("quarantined thread leaked into A's normal folder: %d %s", status, payload)
	}
	status, payload = stack.request(http.MethodGet, "/api/v2/threads?folder=junk", tokenA, "", nil)
	if status != http.StatusOK || !containsSub(payload, []byte("line1-sms-policy-1")) || !containsSub(payload, []byte(`"folder":"junk"`)) {
		t.Fatalf("A cannot list its own junk: %d %s", status, payload)
	}
	status, payload = stack.request(http.MethodGet, "/api/v2/messages?threadKey=line1:line1-sms-policy-1&limit=50", tokenA, "", nil)
	if status != http.StatusOK || !containsSub(payload, []byte("垃圾短信")) {
		t.Fatalf("A cannot read its own quarantined message: %d %s", status, payload)
	}
	status, payload = stack.request(http.MethodGet, "/api/v2/threads", tokenB, "", nil)
	if status != http.StatusOK || !containsSub(payload, []byte("line1-sms-policy-1")) || !containsSub(payload, []byte(`"folder":"normal"`)) {
		t.Fatalf("B must see the message normally: %d %s", status, payload)
	}
	status, payload = stack.request(http.MethodGet, "/api/v2/threads?folder=junk", tokenB, "", nil)
	if status != http.StatusOK || containsSub(payload, []byte("line1-sms-policy-1")) {
		t.Fatalf("A's junk leaked to B: %d %s", status, payload)
	}
}

// TestHarassmentSettingsVersionConflict: two devices of ONE key edit
// concurrently; the stale write conflicts instead of silently overwriting.
func TestHarassmentSettingsVersionConflict(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token1, _ := stack.enroll(keyID, secret, "phone")
	token2, _ := stack.enroll(keyID, secret, "tablet")
	if status := putHarassSettings(t, stack, token1, true, false, nil); status != http.StatusOK {
		t.Fatalf("device1 settings status")
	}
	var view struct {
		Settings struct {
			Version int64 `json:"version"`
		} `json:"settings"`
	}
	stack.jsonRequest(http.MethodGet, "/api/v2/harassment/settings", token2, "", nil, &view)
	if view.Settings.Version != 1 {
		t.Fatalf("version = %d, want 1", view.Settings.Version)
	}
	stale := int64(0)
	if status := putHarassSettings(t, stack, token2, false, true, &stale); status != http.StatusConflict {
		t.Fatalf("stale write status = %d, want 409", status)
	}
	fresh := view.Settings.Version
	if status := putHarassSettings(t, stack, token2, false, true, &fresh); status != http.StatusOK {
		t.Fatalf("fresh write status = %d", status)
	}
}

// TestHarassmentCrossKeyRejectDoesNotKillCall: a device reject on a ringing
// call degrades to a local decline while another key still sees the call;
// the upstream reject happens only once no legitimate receiver remains.
func TestHarassmentCrossKeyRejectDoesNotKillCall(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "A")
	tokenB, _ := stack.enroll(keyB, secretB, "B")
	stack.fakes[0].injectIncoming("call-cross-key", "+8613700000001")
	stack.waitForCall(tokenA, "line1:call-cross-key")

	// A rejects: B still rings, no upstream reject.
	if status, payload := stack.request(http.MethodPost, "/api/v2/calls/line1:call-cross-key/reject", tokenA, "", nil); status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("A reject status = %d body=%s", status, payload)
	}
	time.Sleep(200 * time.Millisecond)
	call := stack.waitForCall(tokenB, "line1:call-cross-key")
	if state, _ := call["state"].(string); state != "incoming_ringing" {
		t.Fatalf("B state after A reject = %q", state)
	}
	if stack.fakes[0].rejects() != 0 {
		t.Fatalf("A reject killed the cross-key call: rejects=%d", stack.fakes[0].rejects())
	}
	// B rejects too: now the call is force-rejected for real.
	if status, payload := stack.request(http.MethodPost, "/api/v2/calls/line1:call-cross-key/reject", tokenB, "", nil); status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("B reject status = %d body=%s", status, payload)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		call, err := stack.store.GetCall(context.Background(), "line1:call-cross-key")
		if err == nil && call.EndedAt != nil && stack.fakes[0].rejects() == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("call not rejected after both keys declined: rejects=%d", stack.fakes[0].rejects())
}

// TestHarassmentCapabilityDeclaration keeps old clients working: the new
// capability is additive metadata, not a breaking change.
func TestHarassmentCapabilityDeclaration(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "old-client")
	var device struct {
		Capabilities map[string]bool `json:"capabilities"`
		Device       map[string]any  `json:"device"`
		Lines        []any           `json:"lines"`
	}
	status := stack.jsonRequest(http.MethodGet, "/api/v2/device", token, "", nil, &device)
	if status != http.StatusOK || !device.Capabilities["harassment"] || !device.Capabilities["harassmentImport"] {
		t.Fatalf("capabilities = %d %+v", status, device.Capabilities)
	}
	// The default (unconfigured) policy is inert: unknown callers allowed.
	var decision map[string]any
	stack.jsonRequest(http.MethodPost, "/api/v2/harassment/test", token, "", map[string]any{
		"kind": "call", "number": "+8613500000009",
	}, &decision)
	if decision["allow"] != true || decision["reason"] != "disabled" {
		t.Fatalf("default policy = %+v, want allow/disabled", decision)
	}
}

// TestHarassmentRevokedKeyDenied: revocation cuts access immediately.
func TestHarassmentRevokedKeyDenied(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "A")
	if status := putHarassSettings(t, stack, token, true, true, nil); status != http.StatusOK {
		t.Fatalf("settings")
	}
	if err := stack.auth.RevokeKey(context.Background(), keyID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	status, _ := stack.request(http.MethodGet, "/api/v2/harassment/settings", token, "", nil)
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Fatalf("revoked key settings status = %d, want 401/403", status)
	}
}

// TestHarassmentHistoryClearIsKeyScoped verifies clear only touches the key.
func TestHarassmentHistoryClearIsKeyScoped(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "A")
	tokenB, _ := stack.enroll(keyB, secretB, "B")
	const caller = "+8613600000003"
	for token := range map[string]bool{tokenA: true, tokenB: true} {
		if status := putHarassSettings(t, stack, token, true, true, nil); status != http.StatusOK {
			t.Fatalf("settings")
		}
		addHarassEntry(t, stack, token, caller, "exact", "block", "reject")
	}
	stack.fakes[0].injectIncomingMessage("sms-clear-scope", caller, "x")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var history struct {
			Events []map[string]any `json:"events"`
		}
		stack.jsonRequest(http.MethodGet, "/api/v2/harassment/history", tokenA, "", nil, &history)
		if len(history.Events) == 0 {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		break
	}
	if status, _ := stack.request(http.MethodDelete, "/api/v2/harassment/history", tokenA, "", nil); status != http.StatusOK {
		t.Fatalf("clear history status")
	}
	var stillThere struct {
		Events []map[string]any `json:"events"`
	}
	stack.jsonRequest(http.MethodGet, "/api/v2/harassment/history", tokenB, "", nil, &stillThere)
	if len(stillThere.Events) == 0 {
		t.Fatalf("clearing key A history erased key B history")
	}
}

// TestHarassmentStoreReceiverKeys covers the effective receiver resolution
// including all-lines, explicit overrides and revocation.
func TestHarassmentStoreReceiverKeys(t *testing.T) {
	stack := newTestStack(t)
	ctx := context.Background()
	keyAll, _, err := stack.auth.CreateKey(ctx, "all", true)
	if err != nil {
		t.Fatalf("key all: %v", err)
	}
	keyGranted, _, err := stack.auth.CreateKey(ctx, "granted", false)
	if err != nil {
		t.Fatalf("key granted: %v", err)
	}
	if err := stack.store.SetKeyLine(ctx, store.KeyLine{
		KeyID: keyGranted, LineID: "line1", ReceiveSMS: true, ReceiveCalls: false, SendSMS: true, Dial: true,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	keyDenied, _, err := stack.auth.CreateKey(ctx, "denied", false)
	if err != nil {
		t.Fatalf("key denied: %v", err)
	}
	if err := stack.store.SetKeyLine(ctx, store.KeyLine{
		KeyID: keyDenied, LineID: "line1", ReceiveSMS: true, ReceiveCalls: false, SendSMS: true, Dial: true,
	}); err != nil {
		t.Fatalf("deny grant: %v", err)
	}
	keys, err := stack.store.LineReceiverKeys(ctx, "line1", true)
	if err != nil {
		t.Fatalf("receiver keys: %v", err)
	}
	set := map[string]bool{}
	for _, key := range keys {
		set[key] = true
	}
	if !set[keyAll] || set[keyGranted] || set[keyDenied] {
		t.Fatalf("call receiver keys = %v", keys)
	}
	// receive_sms resolution differs per line permission.
	smsKeys, err := stack.store.LineReceiverKeys(ctx, "line1", false)
	if err != nil {
		t.Fatalf("sms receiver keys: %v", err)
	}
	smsSet := map[string]bool{}
	for _, key := range smsKeys {
		smsSet[key] = true
	}
	if !smsSet[keyGranted] || !smsSet[keyDenied] {
		t.Fatalf("sms receiver keys = %v", smsKeys)
	}
	if err := stack.auth.RevokeKey(ctx, keyAll); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	keys, _ = stack.store.LineReceiverKeys(ctx, "line1", true)
	for _, key := range keys {
		if key == keyAll {
			t.Fatalf("revoked key still a receiver")
		}
	}
}

type entryWriteResult struct {
	Entry struct {
		ID        string `json:"id"`
		Number    string `json:"number"`
		Mode      string `json:"mode"`
		MatchKind string `json:"matchKind"`
	} `json:"entry"`
	Replaced  int    `json:"replaced"`
	Effective bool   `json:"effective"`
	Note      string `json:"note"`
}

func writeHarassEntry(t *testing.T, stack *testStack, token, number, matchKind, mode string, replaceOpposite bool) entryWriteResult {
	t.Helper()
	var result entryWriteResult
	status := stack.jsonRequest(http.MethodPost, "/api/v2/harassment/entries", token, "", map[string]any{
		"number": number, "matchKind": matchKind, "mode": mode, "replaceOpposite": replaceOpposite,
	}, &result)
	if status != http.StatusOK {
		t.Fatalf("entry write status = %d", status)
	}
	return result
}

// TestContextualEntryReplacementAndEffectiveness: Trust -> Block -> Trust and
// canonical spelling variants are real replacements (never a false success),
// broader trust rules are preserved and reported honestly, and other keys
// are untouched.
func TestContextualEntryReplacementAndEffectiveness(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "A")
	tokenB, _ := stack.enroll(keyB, secretB, "B")
	if status := putHarassSettings(t, stack, tokenA, true, true, nil); status != http.StatusOK {
		t.Fatalf("A settings")
	}

	const number = "+8613800000091"
	// Trust first.
	if result := writeHarassEntry(t, stack, tokenA, number, "exact", "trust", true); result.Replaced != 0 {
		t.Fatalf("first trust replaced %d entries", result.Replaced)
	}
	// Contextual block must atomically replace the exact trust.
	blocked := writeHarassEntry(t, stack, tokenA, number, "exact", "block", true)
	if !blocked.Effective || blocked.Replaced != 1 {
		t.Fatalf("block result = %+v, want effective with 1 replacement", blocked)
	}
	var decision map[string]any
	stack.jsonRequest(http.MethodPost, "/api/v2/harassment/test", tokenA, "", map[string]any{
		"kind": "call", "number": number,
	}, &decision)
	if decision["allow"] != false {
		t.Fatalf("blocked number still allowed: %+v", decision)
	}
	// Block -> trust replacement restores delivery.
	trusted := writeHarassEntry(t, stack, tokenA, number, "exact", "trust", true)
	if !trusted.Effective || trusted.Replaced != 1 {
		t.Fatalf("trust result = %+v, want effective with 1 replacement", trusted)
	}
	stack.jsonRequest(http.MethodPost, "/api/v2/harassment/test", tokenA, "", map[string]any{
		"kind": "call", "number": number,
	}, &decision)
	if decision["allow"] != true || decision["reason"] != "trusted" {
		t.Fatalf("restored trust not effective: %+v", decision)
	}

	// Canonical spelling variants replace each other.
	if result := writeHarassEntry(t, stack, tokenA, "+86 138-0000-0092", "exact", "trust", true); result.Replaced != 0 {
		t.Fatalf("canonical trust replaced %d", result.Replaced)
	}
	canonicalBlock := writeHarassEntry(t, stack, tokenA, "13800000092", "exact", "block", true)
	if !canonicalBlock.Effective || canonicalBlock.Replaced != 1 {
		t.Fatalf("canonical block = %+v", canonicalBlock)
	}

	// A broader trust rule stays and the exact block is reported ineffective.
	if result := writeHarassEntry(t, stack, tokenA, "138", "prefix", "trust", false); result.Replaced != 0 {
		t.Fatalf("broad trust replaced %d", result.Replaced)
	}
	ineffective := writeHarassEntry(t, stack, tokenA, "+8613800000093", "exact", "block", true)
	if ineffective.Effective || ineffective.Note == "" {
		t.Fatalf("broader trust must be reported honestly: %+v", ineffective)
	}
	stack.jsonRequest(http.MethodPost, "/api/v2/harassment/test", tokenA, "", map[string]any{
		"kind": "call", "number": "+8613800000093",
	}, &decision)
	if decision["allow"] != true || decision["reason"] != "trusted" {
		t.Fatalf("broad trust precedence broken: %+v", decision)
	}

	// Another key's identical trust entry must survive A's replacement.
	if status := putHarassSettings(t, stack, tokenB, true, true, nil); status != http.StatusOK {
		t.Fatalf("B settings")
	}
	writeHarassEntry(t, stack, tokenB, number, "exact", "trust", true)
	writeHarassEntry(t, stack, tokenA, number, "exact", "block", true)
	stack.jsonRequest(http.MethodPost, "/api/v2/harassment/test", tokenB, "", map[string]any{
		"kind": "call", "number": number,
	}, &decision)
	if decision["allow"] != true || decision["reason"] != "trusted" {
		t.Fatalf("key B lost its trust entry: %+v", decision)
	}
	// A's unrelated rules are preserved too (the prefix trust from above).
	var entriesA struct {
		Entries []map[string]any `json:"entries"`
	}
	stack.jsonRequest(http.MethodGet, "/api/v2/harassment/entries", tokenA, "", nil, &entriesA)
	foundPrefix := false
	for _, entry := range entriesA.Entries {
		if entry["number"] == "138" && entry["mode"] == "trust" {
			foundPrefix = true
		}
	}
	if !foundPrefix {
		t.Fatalf("unrelated broad rule was removed: %+v", entriesA.Entries)
	}
}
