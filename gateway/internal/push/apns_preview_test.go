package push

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// sendMessageEnvelope sends one dummy SMS alert through the captured sender
// and returns the FINAL JSON bytes APNs would have received, plus the send
// error (if any). All inputs are synthetic names/numbers/text.
func sendMessageEnvelope(t *testing.T, target Target, payload MessagePayload) ([]byte, map[string]any, error) {
	t.Helper()
	var body []byte
	sender := testSender(t, func(request *http.Request) {
		body, _ = io.ReadAll(request.Body)
	}, "production")
	err := sender.SendMessage(context.Background(), target, payload)
	if err != nil {
		return body, nil, err
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("final envelope is not valid JSON: %v (%s)", err, body)
	}
	return body, envelope, nil
}

func dummyTarget() Target {
	return Target{DeviceID: "dev_dummy", APNSToken: "alert-token-dummy", Environment: "production"}
}

// Root review finding (1): the VISIBLE aps fields were bounded but the raw
// payload embedded next to aps was unbounded. The embedded peer/preview must
// themselves be the truncated values in the final JSON.
func TestSendMessageTruncatesEmbeddedPayloadFields(t *testing.T) {
	peer := strings.Repeat("王", 40)          // over the 32-rune peer bound
	body := strings.Repeat("很长的中文短信内容🎉", 60) // far over the 120-rune bound
	raw, envelope, err := sendMessageEnvelope(t, dummyTarget(), MessagePayload{
		MessageID: "line1:sms_bounded", GatewayID: "gw-test", LineID: "line1",
		ThreadKey: "line1:peer-1", Peer: peer, Preview: body, IssuedAt: 1,
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got, ok := envelope["peer"].(string); !ok || utf8.RuneCountInString(got) > PreviewPeerRunes {
		t.Fatalf("embedded peer must be bounded in the final JSON, got %q (%s)", envelope["peer"], raw)
	} else if want := TruncatePreview(peer, PreviewPeerRunes); got != want {
		t.Fatalf("embedded peer %q must equal the truncated value %q", got, want)
	}
	if got, ok := envelope["preview"].(string); !ok || utf8.RuneCountInString(got) > PreviewBodyRunes {
		t.Fatalf("embedded preview must be bounded in the final JSON, got %q", envelope["preview"])
	} else if want := TruncatePreview(body, PreviewBodyRunes); got != want {
		t.Fatalf("embedded preview %q must equal the truncated value %q", got, want)
	}
	aps, _ := envelope["aps"].(map[string]any)
	alert, ok := aps["alert"].(map[string]any)
	if !ok {
		t.Fatalf("aps.alert missing: %s", raw)
	}
	if alert["title"] != TruncatePreview(peer, PreviewPeerRunes) {
		t.Fatalf("visible title must be the bounded sender, got %v", alert["title"])
	}
	if alert["body"] != TruncatePreview(body, PreviewBodyRunes) {
		t.Fatalf("visible body must be the bounded preview, got %v", alert["body"])
	}
	if utf8.RuneCountInString(alert["title"].(string)) > PreviewPeerRunes ||
		utf8.RuneCountInString(alert["body"].(string)) > PreviewBodyRunes {
		t.Fatalf("visible alert fields exceed their bounds: %s", raw)
	}
	if !utf8.Valid(raw) {
		t.Fatalf("final envelope is invalid UTF-8")
	}
	if len(raw) > apnsMaxPayloadBytes {
		t.Fatalf("bounded display content must fit the APNs budget, got %d bytes", len(raw))
	}
	if envelope["threadKey"] != "line1:peer-1" || envelope["messageId"] != "line1:sms_bounded" {
		t.Fatalf("routing identity lost: %s", raw)
	}
	if envelope["category"] != nil {
		t.Fatalf("category is an aps field, not top level")
	}
	if aps["category"] != "CALLRELAY_SMS" {
		t.Fatalf("aps.category must route SMS notifications: %v", aps["category"])
	}
}

// Root review findings (2)+(3): the grouping tag is hashed from the FULL
// GatewayID + FULL ThreadKey (never a truncated key), and ThreadKey routing
// identifiers are embedded intact.
func TestSendMessageThreadIDUsesFullKeyAndGatewayScope(t *testing.T) {
	longKey := "line1:" + strings.Repeat("共前缀", 100) // 300+ runes, shared far beyond 128
	otherKey := longKey + "|不同后缀"
	gwA := MessagePayload{MessageID: "m1", GatewayID: "gw-A", LineID: "line1", ThreadKey: longKey, IssuedAt: 1}
	gwB := gwA
	gwB.GatewayID = "gw-B"
	sameA := gwA

	_, env1, err := sendMessageEnvelope(t, dummyTarget(), gwA)
	if err != nil {
		t.Fatalf("send 1: %v", err)
	}
	_, env2, err := sendMessageEnvelope(t, dummyTarget(), sameA)
	if err != nil {
		t.Fatalf("send 2: %v", err)
	}
	_, envB, err := sendMessageEnvelope(t, dummyTarget(), gwB)
	if err != nil {
		t.Fatalf("send 3: %v", err)
	}
	shared := gwA
	shared.ThreadKey = otherKey
	_, envShared, err := sendMessageEnvelope(t, dummyTarget(), shared)
	if err != nil {
		t.Fatalf("send 4: %v", err)
	}

	aps := func(env map[string]any) map[string]any {
		a, _ := env["aps"].(map[string]any)
		return a
	}
	id1, _ := aps(env1)["thread-id"].(string)
	id2, _ := aps(env2)["thread-id"].(string)
	idB, _ := aps(envB)["thread-id"].(string)
	idS, _ := aps(envShared)["thread-id"].(string)
	if id1 == "" || !strings.HasPrefix(id1, "sms-") || len(id1) != len("sms-")+16 {
		t.Fatalf("thread-id shape wrong: %q", id1)
	}
	if id1 != id2 {
		t.Fatalf("identical (gateway, full key) must group together: %q vs %q", id1, id2)
	}
	// Cross-gateway separation for the SAME thread key.
	if id1 == idB {
		t.Fatalf("same key on different gateways must not share a thread-id: %q", id1)
	}
	// Keys sharing a >128-rune prefix must separate — proves the digest used
	// the FULL key and not a 128-bound slice.
	if id1 == idS {
		t.Fatalf("keys sharing a long prefix must not collide: %q", id1)
	}
	// Recompute the digest independently: it is exactly sha256(gateway+NUL+key).
	sum := sha256.Sum256([]byte(gwA.GatewayID + "\x00" + gwA.ThreadKey))
	if want := "sms-" + hex.EncodeToString(sum[:])[:16]; id1 != want {
		t.Fatalf("thread-id = %q, want %q", id1, want)
	}
	// The embedded custom key is the COMPLETE 300+ rune key, not a slice.
	if env1["threadKey"] != longKey {
		t.Fatalf("threadKey must be embedded complete, got %d runes", utf8.RuneCountInString(env1["threadKey"].(string)))
	}
}

func TestSendMessagePreviewFallsBackToGenericLine(t *testing.T) {
	cases := []struct {
		name       string
		locale     string
		payload    MessagePayload
		wantTitle  string
		wantBody   string
		wantThread bool
	}{
		{"zh generic", "zh-CN", MessagePayload{MessageID: "m", GatewayID: "g"}, "CallRelay", "收到一条新短信", false},
		{"en generic", "en-US", MessagePayload{MessageID: "m", GatewayID: "g"}, "CallRelay", "New message", false},
		{"empty locale falls back to zh", "", MessagePayload{MessageID: "m", GatewayID: "g"}, "CallRelay", "收到一条新短信", false},
		{"peer only", "zh-CN", MessagePayload{MessageID: "m", GatewayID: "g", Peer: "10086"}, "10086", "10086", false},
		{"preview no peer", "zh-CN", MessagePayload{MessageID: "m", GatewayID: "g", Preview: "验证码 1234"}, "CallRelay", "验证码 1234", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			target := dummyTarget()
			target.Locale = testCase.locale
			_, envelope, err := sendMessageEnvelope(t, target, testCase.payload)
			if err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			aps, _ := envelope["aps"].(map[string]any)
			alert, _ := aps["alert"].(map[string]any)
			if alert["title"] != testCase.wantTitle || alert["body"] != testCase.wantBody {
				t.Fatalf("alert = %v, want %q/%q", alert, testCase.wantTitle, testCase.wantBody)
			}
			_, hasThread := aps["thread-id"]
			if hasThread != testCase.wantThread {
				t.Fatalf("thread-id presence = %v, want %v", hasThread, testCase.wantThread)
			}
		})
	}
}

func TestSendMessageFinalJSONUnder4096WithLongUnicode(t *testing.T) {
	// Worst realistic case: 5000-rune CJK+emoji body, 200-rune peer, normal
	// routing ids. The display bounds must keep the FINAL envelope inside
	// the 4 KiB APNs budget.
	raw, _, err := sendMessageEnvelope(t, dummyTarget(), MessagePayload{
		MessageID: "line1:sms_big", GatewayID: "gw-0123456789", LineID: "line1",
		ThreadKey: "line1:" + strings.Repeat("p", 100),
		Peer:      strings.Repeat("联系", 100),
		Preview:   strings.Repeat("内容🎉", 2500),
		IssuedAt:  1729500000,
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if len(raw) > apnsMaxPayloadBytes {
		t.Fatalf("final envelope is %d bytes, over %d", len(raw), apnsMaxPayloadBytes)
	}
}

func TestSendMessageOversizedRoutingMetadataIsRejectedNotTruncated(t *testing.T) {
	// Routing ids are NEVER clipped to fit: a pathologically oversized
	// threadKey/messageId must trip the final-size guard instead.
	_, _, err := sendMessageEnvelope(t, dummyTarget(), MessagePayload{
		MessageID: strings.Repeat("m", 5000), GatewayID: "g", ThreadKey: strings.Repeat("t", 5000),
	})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized routing metadata must be rejected by the size guard, got %v", err)
	}
}

func TestSendAlertKeepsServiceAlertContractUnchanged(t *testing.T) {
	// The SMS preview contract must not leak into (or regress) service
	// alerts: same title/body/thread-id/category contract, no peer/preview.
	var body []byte
	sender := testSender(t, func(request *http.Request) { body, _ = io.ReadAll(request.Body) }, "production")
	err := sender.SendAlert(context.Background(), dummyTarget(), AlertPayload{
		Kind: "line_alert", Alert: "arrears", LineID: "line1", LineName: "主卡",
		Title: "欠费提醒", Body: "余额不足", IncidentID: "inc-1", GatewayID: "gw", IssuedAt: 5,
	})
	if err != nil {
		t.Fatalf("SendAlert: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("alert json: %v", err)
	}
	for _, key := range []string{"peer", "preview", "threadKey"} {
		if _, present := envelope[key]; present {
			t.Fatalf("service alerts must not carry SMS preview fields: %s", key)
		}
	}
	aps, _ := envelope["aps"].(map[string]any)
	if aps["category"] != "CALLRELAY_LINE_ALERT" {
		t.Fatalf("alert category changed: %v", aps["category"])
	}
	if aps["thread-id"] != "line-alert-line1" {
		t.Fatalf("alert thread-id changed: %v", aps["thread-id"])
	}
	if envelope["kind"] != "line_alert" || envelope["incidentId"] != "inc-1" {
		t.Fatalf("service alert routing fields lost: %s", body)
	}
}

// The optional broker sender must forward the extended MessagePayload fields
// untouched (the broker renders its own aps). This deployment uses direct APNs; this
// keeps the broker path compatible for any broker-configured tenant.
func TestBrokerMessageCarriesExtendedFields(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := newBrokerCaptureServer(t, requests)
	defer server.Close()
	broker := NewBroker(server.URL, "Bearer broker-test")
	err := broker.SendMessage(context.Background(), Target{
		DeviceID: "dev_1", APNSToken: "alert-token", Environment: "production",
	}, MessagePayload{
		MessageID: "line1:sms_1", GatewayID: "gw", LineID: "line1",
		ThreadKey: "line1:peer-9", Peer: "客服", Preview: "您的验证码是 1234", IssuedAt: 9,
	})
	if err != nil {
		t.Fatalf("broker SendMessage: %v", err)
	}
	body := <-requests
	payload, ok := body["payload"].(map[string]any)
	if !ok {
		t.Fatalf("broker payload missing: %#v", body)
	}
	if payload["threadKey"] != "line1:peer-9" || payload["peer"] != "客服" || payload["preview"] != "您的验证码是 1234" {
		t.Fatalf("broker must forward the extended fields untouched: %#v", payload)
	}
	if payload["messageId"] != "line1:sms_1" || payload["gatewayId"] != "gw" {
		t.Fatalf("routing identity lost through broker: %#v", payload)
	}
}

func newBrokerCaptureServer(t *testing.T, sink chan map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/push/message" {
			t.Errorf("path = %s", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		sink <- body
		writer.WriteHeader(http.StatusAccepted)
	}))
	return server
}
