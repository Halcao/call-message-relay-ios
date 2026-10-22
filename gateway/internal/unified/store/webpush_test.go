package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newWebPushStore(t *testing.T) *Store {
	t.Helper()
	st := newBarkStore(t)
	seedBarkDevice(t, st)
	ctx := context.Background()
	// A second device on a second key, used to prove scoping.
	if err := st.CreateEnrollmentKey(ctx, "key_2", "other", []byte{7, 8, 9}, "secret2", true); err != nil {
		t.Fatalf("create key 2: %v", err)
	}
	if err := st.CreateDevice(ctx, Device{
		ID: "dev_2", KeyID: "key_2", Name: "Other Phone", PublicKey: []byte{10, 11, 12},
	}); err != nil {
		t.Fatalf("create device 2: %v", err)
	}
	return st
}

func upsertSub(t *testing.T, st *Store, deviceID, endpoint string) {
	t.Helper()
	if err := st.UpsertWebPushSubscription(context.Background(), WebPushSubscription{
		DeviceID: deviceID, Endpoint: endpoint, Auth: "auth-secret", P256dh: "dh-secret",
		UserAgent: "TestBrowser/1.0",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

func TestWebPushSubscriptionLifecycle(t *testing.T) {
	st := newWebPushStore(t)
	ctx := context.Background()
	upsertSub(t, st, "dev_1", "https://push.example/a")
	upsertSub(t, st, "dev_1", "https://push.example/a") // refresh same endpoint
	upsertSub(t, st, "dev_1", "https://push.example/b")
	upsertSub(t, st, "dev_2", "https://push.example/c")

	subs, err := st.ListWebPushSubscriptions(ctx, "dev_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 {
		t.Fatalf("dev_1 subs = %d, want 2 (refresh must not duplicate)", len(subs))
	}
	count, err := st.CountWebPushSubscriptions(ctx, "dev_1")
	if err != nil || count != 2 {
		t.Fatalf("count = %d, %v", count, err)
	}
	for _, sub := range subs {
		if sub.Auth != "auth-secret" || sub.P256dh != "dh-secret" {
			t.Fatal("secrets must round-trip for the notifier")
		}
	}

	if err := st.DeleteWebPushSubscription(ctx, "dev_1", subs[0].ID); err != nil {
		t.Fatal(err)
	}
	if count, _ := st.CountWebPushSubscriptions(ctx, "dev_1"); count != 1 {
		t.Fatal("delete must remove exactly one subscription")
	}
	// Another device cannot remove dev_1's remaining subscription.
	remaining, _ := st.ListWebPushSubscriptions(ctx, "dev_1")
	if err := st.DeleteWebPushSubscription(ctx, "dev_2", remaining[0].ID); err != nil {
		t.Fatal(err)
	}
	if count, _ := st.CountWebPushSubscriptions(ctx, "dev_1"); count != 1 {
		t.Fatal("foreign delete must be a no-op")
	}
}

func TestWebPushTargetsOnlyLiveDevices(t *testing.T) {
	st := newWebPushStore(t)
	ctx := context.Background()
	upsertSub(t, st, "dev_1", "https://push.example/a")
	upsertSub(t, st, "dev_2", "https://push.example/c")

	targets, err := st.ListWebPushTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	if err := st.SetDeviceRevoked(ctx, "dev_2", true); err != nil {
		t.Fatal(err)
	}
	targets, err = st.ListWebPushTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].DeviceID != "dev_1" {
		t.Fatalf("revoked device must drop out: %+v", targets)
	}
	// Revocation must also have removed dispatch access at the storage layer.
	if count, _ := st.CountWebPushSubscriptions(ctx, "dev_2"); count != 0 {
		t.Fatal("revoked device subscriptions must be removed")
	}
	if _, _, err := st.TouchWebPushSession(ctx, "deadbeef", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session must fail: %v", err)
	}
}

func TestWebPushKeyDeletionCascades(t *testing.T) {
	st := newWebPushStore(t)
	ctx := context.Background()
	upsertSub(t, st, "dev_2", "https://push.example/c")
	if err := st.SaveWebPushSession(ctx, "dev_2", "hash-s2", "push", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.SetEnrollmentKeyRevoked(ctx, "key_2", true); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRevokedKey(ctx, "key_2"); err != nil {
		t.Fatal(err)
	}
	if count, _ := st.CountWebPushSubscriptions(ctx, "dev_2"); count != 0 {
		t.Fatal("key deletion must remove subscriptions")
	}
	if _, _, err := st.TouchWebPushSession(ctx, "hash-s2", time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatal("key deletion must remove sessions")
	}
}

func TestWebPushBindTokenSemantics(t *testing.T) {
	st := newWebPushStore(t)
	ctx := context.Background()
	if err := st.SaveWebPushBindToken(ctx, "dev_1", "hash-b1", "push", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Reusable until a successful redemption.
	device, scope, err := st.RedeemWebPushBindToken(ctx, "hash-b1")
	if err != nil || device != "dev_1" || scope != "push" {
		t.Fatalf("first presentment: %v %s %s", err, device, scope)
	}
	device, scope, err = st.RedeemWebPushBindToken(ctx, "hash-b1")
	if err != nil || device != "dev_1" || scope != "push" {
		t.Fatalf("reusable until consumed: %v %s %s", err, device, scope)
	}
	if err := st.MarkWebPushBindTokenUsed(ctx, "hash-b1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemWebPushBindToken(ctx, "hash-b1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("consumed code must fail")
	}
	if _, _, err := st.RedeemWebPushBindToken(ctx, "hash-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown code must fail identically")
	}
	if err := st.SaveWebPushBindToken(ctx, "dev_1", "hash-b2", "client", -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RedeemWebPushBindToken(ctx, "hash-b2"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired code must fail identically")
	}
}

func TestWebPushSessionSemantics(t *testing.T) {
	st := newWebPushStore(t)
	ctx := context.Background()
	if err := st.SaveWebPushSession(ctx, "dev_1", "hash-s1", "client", time.Minute); err != nil {
		t.Fatal(err)
	}
	device, scope, err := st.TouchWebPushSession(ctx, "hash-s1", time.Minute)
	if err != nil || device != "dev_1" || scope != "client" {
		t.Fatalf("touch: %v %s", err, device)
	}
	if err := st.RevokeWebPushSessions(ctx, "dev_1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.TouchWebPushSession(ctx, "hash-s1", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked session must fail")
	}
	if err := st.SaveWebPushSession(ctx, "dev_1", "hash-s2", "push", -time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.TouchWebPushSession(ctx, "hash-s2", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired session must fail")
	}
}

func TestDeviceNotifyMode(t *testing.T) {
	st := newWebPushStore(t)
	ctx := context.Background()
	mode, err := st.DeviceNotifyMode(ctx, "dev_1")
	if err != nil || mode != NotifyModeNative {
		t.Fatalf("default mode: %q %v", mode, err)
	}
	for _, want := range []string{NotifyModeNative, NotifyModeBoth, NotifyModeWeb} {
		if err := st.SetDeviceNotifyMode(ctx, "dev_1", want); err != nil {
			t.Fatal(err)
		}
		if got, _ := st.DeviceNotifyMode(ctx, "dev_1"); got != want {
			t.Fatalf("mode = %q, want %q", got, want)
		}
	}
	if err := st.SetDeviceNotifyMode(ctx, "dev_1", "bogus"); err == nil {
		t.Fatal("invalid mode must fail")
	}
	// Scoping: dev_2 untouched.
	if got, _ := st.DeviceNotifyMode(ctx, "dev_2"); got != NotifyModeNative {
		t.Fatalf("dev_2 mode = %q, want native", got)
	}
}
