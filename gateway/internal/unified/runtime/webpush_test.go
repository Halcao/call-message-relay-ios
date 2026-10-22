package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/push"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/webpush"
)

type recordedPush struct {
	endpoint string
	payload  []byte
}

type recordingWebPushSender struct {
	mu    sync.Mutex
	sends []recordedPush
}

func (r *recordingWebPushSender) post(ctx context.Context, endpoint string, message []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sends = append(r.sends, recordedPush{endpoint: endpoint, payload: message})
	return http.StatusCreated, nil
}

func (r *recordingWebPushSender) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sends)
}

func newWebPushManagerHarness(t *testing.T, mode string) (*Manager, *store.Store, *recordingWebPushSender, context.Context) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Gateway.ID = "gw-webpush"
	cfg.Gateway.DataDir = dir
	cfg.Gateway.PublicBaseURL = "https://gw.example"
	cfg.WebPush.Enabled = true
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
	if err := st.CreateEnrollmentKey(ctx, "key_1", "test", []byte{1, 2, 3}, "secret", true); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateDevice(ctx, store.Device{ID: "dev_1", KeyID: "key_1", Name: "iPhone", PublicKey: []byte{4, 5, 6}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWebPushSubscription(ctx, store.WebPushSubscription{
		DeviceID: "dev_1", Endpoint: "https://8.8.8.8/push", Auth: "a", P256dh: "d",
	}); err != nil {
		t.Fatal(err)
	}
	if mode != "" {
		if err := st.SetDeviceNotifyMode(ctx, "dev_1", mode); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewManager(cfg, st, nil, nil, nil, nil)
	manager.WebPush = webpush.NewManager(st)
	manager.WebPushSender = &webpush.Sender{
		Subject: "mailto:t@example.com", PublicKey: "pk", PrivateKey: "sk",
	}
	recorder := &recordingWebPushSender{}
	manager.WebPushSender.Post = recorder.post
	return manager, st, recorder, ctx
}

func waitForSends(t *testing.T, recorder *recordingWebPushSender, want int) []recordedPush {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if recorder.count() >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]recordedPush(nil), recorder.sends...)
}

func ringingCall() store.Call {
	return store.Call{
		ID: "line1:wc-1", LineID: "line1", WorkerCallID: "wc-1",
		Direction: "inbound", Peer: "10086", State: StateRinging, CreatedAt: time.Now().UTC(),
	}
}

func TestPushWebIncomingRespectsNotifyMode(t *testing.T) {
	// Default (native) mode: no web push, ever.
	manager, _, recorder, ctx := newWebPushManagerHarness(t, "")
	manager.pushWebIncoming(ctx, ringingCall())
	if got := waitForSends(t, recorder, 0); len(got) != 0 {
		t.Fatalf("native mode must not receive web push: %d", len(got))
	}

	// Web mode delivers the declarative payload with the fragment handoff.
	manager, st, recorder, ctx := newWebPushManagerHarness(t, store.NotifyModeWeb)
	if err := st.UpsertCall(ctx, ringingCall()); err != nil {
		t.Fatal(err)
	}
	manager.pushWebIncoming(ctx, ringingCall())
	got := waitForSends(t, recorder, 1)
	if len(got) != 1 {
		t.Fatalf("web mode must deliver: %d", len(got))
	}
	if got[0].endpoint != "https://8.8.8.8/push" {
		t.Fatalf("endpoint = %q", got[0].endpoint)
	}
	var payload struct {
		WebPush      int `json:"web_push"`
		Notification struct {
			Title    string `json:"title"`
			Navigate string `json:"navigate"`
			Tag      string `json:"tag"`
		} `json:"notification"`
	}
	if err := json.Unmarshal(got[0].payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.WebPush != 8030 || payload.Notification.Title == "" {
		t.Fatalf("payload mismatch: %s", got[0].payload)
	}
	if !strings.HasPrefix(payload.Notification.Navigate, "https://gw.example/pwa/call#h=") {
		t.Fatalf("navigate must carry the fragment handoff: %q", payload.Notification.Navigate)
	}

	// Both mode delivers too.
	manager, st, recorder, ctx = newWebPushManagerHarness(t, store.NotifyModeBoth)
	if err := st.UpsertCall(ctx, ringingCall()); err != nil {
		t.Fatal(err)
	}
	manager.pushWebIncoming(ctx, ringingCall())
	if got := waitForSends(t, recorder, 1); len(got) != 1 {
		t.Fatalf("both mode must deliver: %d", len(got))
	}
}

func TestPushWebIncomingSuppressesStaleCalls(t *testing.T) {
	manager, st, recorder, ctx := newWebPushManagerHarness(t, store.NotifyModeWeb)
	stale := ringingCall()
	stale.CreatedAt = time.Now().UTC().Add(-10 * time.Minute)
	if err := st.UpsertCall(ctx, stale); err != nil {
		t.Fatal(err)
	}
	manager.pushWebIncoming(ctx, stale)
	if got := waitForSends(t, recorder, 0); len(got) != 0 {
		t.Fatal("stale ringing call must not notify")
	}

	ended := ringingCall()
	now := time.Now().UTC()
	ended.EndedAt = &now
	ended.State = StateEnded
	if err := st.UpsertCall(ctx, ended); err != nil {
		t.Fatal(err)
	}
	manager.pushWebIncoming(ctx, ended)
	if got := waitForSends(t, recorder, 0); len(got) != 0 {
		t.Fatal("ended call must not notify")
	}
}

func TestPushWebIncomingDeduplicates(t *testing.T) {
	manager, st, recorder, ctx := newWebPushManagerHarness(t, store.NotifyModeWeb)
	call := ringingCall()
	if err := st.UpsertCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	manager.pushWebIncoming(ctx, call)
	manager.pushWebIncoming(ctx, call)
	got := waitForSends(t, recorder, 2)
	// Dedup keeps the count at exactly one per (subscription, call) even
	// with two overlapping rings.
	if len(got) != 1 {
		t.Fatalf("dedup must cap deliveries at 1, got %d", len(got))
	}
}

func TestPushWebIncomingNoopWhenDisabled(t *testing.T) {
	manager := &Manager{}
	manager.pushWebIncoming(context.Background(), ringingCall())
}

type recordingNativeSender struct {
	voip int
	mu   sync.Mutex
}

func (r *recordingNativeSender) SendVoIP(ctx context.Context, target push.Target, payload push.VoIPPayload) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.voip++
	return nil
}

func (r *recordingNativeSender) SendMessage(ctx context.Context, target push.Target, payload push.MessagePayload) error {
	return nil
}

func (r *recordingNativeSender) SendAlert(ctx context.Context, target push.Target, payload push.AlertPayload) error {
	return nil
}

func (r *recordingNativeSender) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.voip
}

func TestPushIncomingCallSkipsWebOnlyDevices(t *testing.T) {
	// A web-only device must not appear in the native dispatch; a native-
	// mode device must.
	native := &recordingNativeSender{}
	manager, st, _, ctx := newWebPushManagerHarness(t, store.NotifyModeWeb)
	_ = st
	manager.Push = native
	// Give the device a stored voip token so ListPushTargets sees it.
	if err := st.SetDevicePush(ctx, "dev_1", store.PushRegistration{
		APNSToken: "apns", VoIPToken: "voip", Environment: "sandbox", Locale: "zh-Hans",
	}); err != nil {
		t.Fatal(err)
	}
	call := ringingCall()
	if err := st.UpsertCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	manager.pushIncomingCall(ctx, call)
	deadline := time.Now().Add(2 * time.Second)
	for native.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if native.count() != 0 {
		t.Fatal("web-only device must not receive a native ring")
	}

	manager, st, _, ctx = newWebPushManagerHarness(t, store.NotifyModeNative)
	native = &recordingNativeSender{}
	manager.Push = native
	if err := st.SetDevicePush(ctx, "dev_1", store.PushRegistration{
		APNSToken: "apns", VoIPToken: "voip", Environment: "sandbox", Locale: "zh-Hans",
	}); err != nil {
		t.Fatal(err)
	}
	manager.pushIncomingCall(ctx, ringingCall())
	deadline = time.Now().Add(2 * time.Second)
	for native.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if native.count() == 0 {
		t.Fatal("native-mode device must receive a native ring")
	}
}
