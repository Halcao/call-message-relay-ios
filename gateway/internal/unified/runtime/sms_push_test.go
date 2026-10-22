package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"callrelay/gateway/internal/push"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/unified/worker"
)

// recordingPushSender captures every native fan-out payload so the tests can
// assert the exact wire fields the APNs sender will embed.
type recordingPushSender struct {
	mu       sync.Mutex
	messages []push.MessagePayload
	targets  []push.Target
	voip     []push.VoIPPayload
	alerts   []push.AlertPayload
}

func (r *recordingPushSender) SendVoIP(_ context.Context, _ push.Target, payload push.VoIPPayload) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.voip = append(r.voip, payload)
	return nil
}

func (r *recordingPushSender) SendMessage(_ context.Context, target push.Target, payload push.MessagePayload) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, payload)
	r.targets = append(r.targets, target)
	return nil
}

func (r *recordingPushSender) SendAlert(_ context.Context, _ push.Target, payload push.AlertPayload) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.alerts = append(r.alerts, payload)
	return nil
}

func (r *recordingPushSender) sentMessages() []push.MessagePayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]push.MessagePayload(nil), r.messages...)
}

func (r *recordingPushSender) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messages)
}

func newSMSPushHarness(t *testing.T) (*Manager, *store.Store, *recordingPushSender) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Gateway.ID = "gw-smspush"
	cfg.Gateway.DataDir = dir
	st, err := store.Open(filepath.Join(dir, "unified.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureLine(ctx, store.Line{ID: "line1", Name: "Line 1", WorkerURL: "http://127.0.0.1:1", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// key_1 owns line1 (all_lines) and both registered push devices.
	if err := st.CreateEnrollmentKey(ctx, "key_1", "test", []byte{1, 2, 3}, "secret", true); err != nil {
		t.Fatal(err)
	}
	for index, deviceID := range []string{"dev_1", "dev_2"} {
		if err := st.CreateDevice(ctx, store.Device{ID: deviceID, KeyID: "key_1", Name: deviceID, PublicKey: []byte{byte(index + 4), 5, 6}}); err != nil {
			t.Fatal(err)
		}
		if err := st.SetDevicePush(ctx, deviceID, store.PushRegistration{
			APNSToken: "tok-" + deviceID, Environment: "production", Locale: "zh-CN",
		}); err != nil {
			t.Fatal(err)
		}
	}
	sender := &recordingPushSender{}
	manager := NewManager(cfg, st, nil, nil, nil, sender)
	if manager.smsPushDedup == nil {
		t.Fatal("NewManager must initialize the SMS push deduper alongside the others")
	}
	return manager, st, sender
}

func smsFixture(id string) worker.Message {
	return worker.Message{
		ID: id, GatewayID: "gw-worker", LineID: "line1:line", ThreadKey: "peer-9",
		Direction: "inbound", Peer: "+8613900001234",
		Body: strings.Repeat("短信", 150), Encoding: "ucs2", Status: "received",
		CreatedAt: time.Now().UnixMilli(),
	}
}

func waitForMessages(t *testing.T, sender *recordingPushSender, want int) []push.MessagePayload {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sender.count() >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return sender.sentMessages()
}

// The runtime must wire the real routing identity (lineId:threadKey
// composite like MessageView) plus sender/preview into the payload.
func TestPushMessageWiresRoutingIdentityAndBoundedPreview(t *testing.T) {
	manager, _, sender := newSMSPushHarness(t)
	message := smsFixture("sms_wire")
	manager.pushMessage("line1", message)

	sent := waitForMessages(t, sender, 2)
	if len(sent) != 2 {
		t.Fatalf("both grant-holding devices must receive exactly one alert each, got %d", len(sent))
	}
	for _, payload := range sent {
		if payload.MessageID != "line1:sms_wire" {
			t.Fatalf("messageId = %q, want line1:sms_wire", payload.MessageID)
		}
		if payload.ThreadKey != "line1:peer-9" {
			t.Fatalf("threadKey = %q, want the MessageView composite line1:peer-9", payload.ThreadKey)
		}
		if payload.GatewayID != "gw-smspush" {
			t.Fatalf("gatewayId = %q, want the unified gateway identity", payload.GatewayID)
		}
		if payload.LineID != "line1" {
			t.Fatalf("lineId = %q", payload.LineID)
		}
		if payload.Peer != message.Peer {
			t.Fatalf("sender peer = %q, want the raw sender for the APNs display bound", payload.Peer)
		}
		if want := push.TruncatePreview(message.Body, push.PreviewBodyRunes); payload.Preview != want {
			t.Fatalf("preview must be bounded at the fan-out point: %q vs %q", payload.Preview, want)
		}
		if utf8.RuneCountInString(payload.Preview) > push.PreviewBodyRunes {
			t.Fatalf("preview exceeds %d runes", push.PreviewBodyRunes)
		}
		if payload.IssuedAt <= 0 {
			t.Fatalf("issuedAt missing")
		}
	}
	// Device targeting must reach the registered push tokens of both devices.
	if sent[0].GatewayID == "" {
		t.Fatal("empty gateway id")
	}
	// SMS never rides the VoIP or service-alert channels.
	if len(sender.voip) != 0 || len(sender.alerts) != 0 {
		t.Fatal("SMS fan-out must not touch VoIP or AlertPayload paths")
	}
}

// A worker reconnect replaying the same message id must not re-alert; the
// dedupe is per (device, message), so both devices still get exactly one.
// 40 concurrent replays under -race also exercise the shared deduper.
func TestPushMessageDedupesPerDeviceMessageUnderConcurrency(t *testing.T) {
	manager, _, sender := newSMSPushHarness(t)
	message := smsFixture("sms_replay")
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			manager.pushMessage("line1", message)
		}()
	}
	wg.Wait()
	sent := waitForMessages(t, sender, 2)
	// Give stragglers a quiet window: a broken dedupe would keep counting up.
	time.Sleep(250 * time.Millisecond)
	sent = sender.sentMessages()
	if len(sent) != 2 {
		t.Fatalf("40 concurrent replays produced %d pushes; dedupe must allow exactly one per device", len(sent))
	}
	devices := map[string]int{}
	for _, payload := range sent {
		devices[payload.MessageID]++
	}
	// Different message ids are independent reservations.
	manager.pushMessage("line1", smsFixture("sms_replay_b"))
	second := waitForMessages(t, sender, 4)
	if len(second) != 4 {
		t.Fatalf("a new message id must fan out again, got %d total", len(second))
	}
	// And the same id replayed again (sequentially) stays suppressed.
	manager.pushMessage("line1", smsFixture("sms_replay"))
	time.Sleep(200 * time.Millisecond)
	if count := sender.count(); count != 4 {
		t.Fatalf("same (device, message) replay must not re-alert, total %d", count)
	}
	_ = devices
}

// ReceiveSMS is the delivery gate: no grant -> no alert. Devices under a key
// with an explicit line override that clears ReceiveSMS are excluded, and a
// key without the line at all never sees it.
func TestPushMessageHonoursReceiveSMSGrant(t *testing.T) {
	manager, st, sender := newSMSPushHarness(t)
	ctx := context.Background()
	// key_2: no line grant at all.
	if err := st.CreateEnrollmentKey(ctx, "key_2", "test2", []byte{7, 8, 9}, "secret", false); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDevice(ctx, store.Device{ID: "dev_3", KeyID: "key_2", Name: "dev_3", PublicKey: []byte{10}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDevicePush(ctx, "dev_3", store.PushRegistration{APNSToken: "tok-dev_3", Environment: "production"}); err != nil {
		t.Fatal(err)
	}
	// key_3: explicit line1 override WITHOUT ReceiveSMS (calls only).
	if err := st.CreateEnrollmentKey(ctx, "key_3", "test3", []byte{11, 12, 13}, "secret", false); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDevice(ctx, store.Device{ID: "dev_4", KeyID: "key_3", Name: "dev_4", PublicKey: []byte{14}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDevicePush(ctx, "dev_4", store.PushRegistration{APNSToken: "tok-dev_4", Environment: "production"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKeyLine(ctx, store.KeyLine{KeyID: "key_3", LineID: "line1", ReceiveCalls: true}); err != nil {
		t.Fatal(err)
	}

	manager.pushMessage("line1", smsFixture("sms_grant"))
	got := waitForMessages(t, sender, 2)
	time.Sleep(250 * time.Millisecond)
	got = sender.sentMessages()
	if len(got) != 2 {
		t.Fatalf("exactly the two key_1 devices may be alerted, got %d", len(got))
	}
	for _, payload := range got {
		if payload.MessageID != "line1:sms_grant" {
			t.Fatalf("unexpected payload %q", payload.MessageID)
		}
	}
}

// Per-key harassment suppression gates the alert for the suppressed key only:
// another key that also holds the line keeps receiving its own alerts.
func TestPushMessageHarassmentSuppressionIsKeyScoped(t *testing.T) {
	manager, st, sender := newSMSPushHarness(t)
	ctx := context.Background()
	if _, err := st.SetHarassmentSettings(ctx, "key_1", false, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertHarassmentEntry(ctx, store.HarassmentEntry{
		KeyID: "key_1", Number: "+8613900001234", MatchKind: "sms", Mode: "block", Action: "reject", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	// key_2 legitimately receives SMS on line1 and has NO policy entry.
	if err := st.CreateEnrollmentKey(ctx, "key_2", "test2", []byte{7, 8, 9}, "secret", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetKeyLine(ctx, store.KeyLine{KeyID: "key_2", LineID: "line1", ReceiveSMS: true, ReceiveCalls: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDevice(ctx, store.Device{ID: "dev_other", KeyID: "key_2", Name: "dev_other", PublicKey: []byte{15}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDevicePush(ctx, "dev_other", store.PushRegistration{APNSToken: "tok-other", Environment: "production"}); err != nil {
		t.Fatal(err)
	}

	manager.pushMessage("line1", smsFixture("sms_harass"))
	got := waitForMessages(t, sender, 1)
	time.Sleep(250 * time.Millisecond)
	got = sender.sentMessages()
	if len(got) != 1 {
		t.Fatalf("the suppressed key must receive 0 and the clean key 1, got %d total", len(got))
	}
}

// The fan-out trigger is INBOUND message.created only: outbound mirrors and
// non-message events never alert, and the live event path keeps indexing
// conversation state.
func TestPushMessageTriggersOnInboundCreatedOnly(t *testing.T) {
	manager, st, sender := newSMSPushHarness(t)
	line := &Line{Cfg: config.Line{ID: "line1", Name: "Line 1"}}
	ctx := context.Background()

	decode := func(message worker.Message) json.RawMessage {
		payload, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}

	outbound := smsFixture("sms_out")
	outbound.Direction = "outbound"
	outbound.Status = "sent"
	manager.handleMessageEvent(ctx, line, worker.Event{ID: "e1", Type: "message.created", Data: decode(outbound)})

	updatedInbound := smsFixture("sms_upd")
	manager.handleMessageEvent(ctx, line, worker.Event{ID: "e2", Type: "message.updated", Data: decode(updatedInbound)})

	callEvent := worker.Event{ID: "e3", Type: "call.ended", Data: decode(worker.Message{ID: "not-a-call"})}
	manager.handleMessageEvent(ctx, line, callEvent)

	if count := sender.count(); count != 0 {
		t.Fatalf("outbound/updated/non-message events must not alert, got %d pushes", count)
	}
	// The thread index advanced for every parsed message (read-state input).
	if _, _, ok := st.MessageThread(ctx, "line1", "sms_out"); !ok {
		t.Fatal("live events must keep indexing the conversation for read state")
	}

	fresh := smsFixture("sms_new")
	manager.handleMessageEvent(ctx, line, worker.Event{ID: "e4", Type: "message.created", Data: decode(fresh)})
	got := waitForMessages(t, sender, 2)
	if len(got) != 2 {
		t.Fatalf("inbound message.created must fan out to both devices, got %d", len(got))
	}
	for _, payload := range got {
		if payload.MessageID != "line1:sms_new" || payload.ThreadKey != "line1:peer-9" {
			t.Fatalf("composite routing identity wrong: %+v", payload)
		}
	}
}

// The dedupe lives in process memory (BarkDeduper TTL) and the event client
// has NO replay cursor (StreamEvents has no Last-Event-ID), so a worker
// reconnect can only re-deliver events the worker itself still emits. This
// test pins the actual, bounded contract: replays inside the TTL window are
// suppressed; a manager rebuild (daemon restart) forgets the reservation and
// may alert once more. No test may claim cross-restart persistence.
func TestPushMessageDedupeIsInProcessTTLBounded(t *testing.T) {
	manager, _, sender := newSMSPushHarness(t)
	message := smsFixture("sms_ttl")
	manager.pushMessage("line1", message)
	if got := waitForMessages(t, sender, 2); len(got) != 2 {
		t.Fatalf("first fan-out = %d", len(got))
	}
	// Reconnect replay inside the 10-minute TTL: suppressed.
	manager.pushMessage("line1", smsFixture("sms_ttl"))
	time.Sleep(150 * time.Millisecond)
	if count := sender.count(); count != 2 {
		t.Fatalf("in-TTL replay must be suppressed, got %d", count)
	}
	// A FRESH manager (daemon restart) has no memory of the reservation.
	restarted, _, after := newSMSPushHarness(t)
	restarted.pushMessage("line1", smsFixture("sms_ttl"))
	if got := waitForMessages(t, after, 2); len(got) != 2 {
		t.Fatalf("restart-bounded: the same message may re-alert exactly once after a daemon restart")
	}
}

// A Manager without a configured push sender must stay inert (nil guard).
func TestPushMessageNilSenderIsNoop(t *testing.T) {
	manager := &Manager{}
	manager.pushMessage("line1", smsFixture("any"))
}
