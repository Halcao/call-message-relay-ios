package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newBarkStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "unified.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func seedBarkDevice(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateEnrollmentKey(ctx, "key_1", "test", []byte{1, 2, 3}, "secret", true); err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := st.CreateDevice(ctx, Device{
		ID: "dev_1", KeyID: "key_1", Name: "Test iPhone", PublicKey: []byte{4, 5, 6},
	}); err != nil {
		t.Fatalf("create device: %v", err)
	}
}

func TestDeviceBarkRoundTripAndNotifierTargets(t *testing.T) {
	st := newBarkStore(t)
	seedBarkDevice(t, st)
	ctx := context.Background()

	if _, err := st.GetDeviceBark(ctx, "dev_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unconfigured device must be ErrNotFound, got %v", err)
	}

	settings := DeviceBark{
		DeviceID: "dev_1", Enabled: true, ServerURL: "https://api.day.app",
		DeviceKey: "sekret-key-1", KeyHint: "••••key-1", AllowPrivate: false,
	}
	if err := st.SetDeviceBark(ctx, settings); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := st.GetDeviceBark(ctx, "dev_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Enabled || got.ServerURL != settings.ServerURL || got.DeviceKey != settings.DeviceKey ||
		got.KeyHint != settings.KeyHint || got.AllowPrivate {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("updated_at must be set")
	}

	// The notifier view carries the secret exactly once and only for a live,
	// enabled device.
	targets, err := st.ListBarkTargets(ctx)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets: %v %v", targets, err)
	}
	if targets[0].DeviceKey != "sekret-key-1" || targets[0].ServerURL != settings.ServerURL {
		t.Fatalf("notifier target mismatch: %+v", targets[0])
	}

	// Disabled rows never reach the notifier.
	if err := st.SetDeviceBark(ctx, DeviceBark{
		DeviceID: "dev_1", Enabled: false, ServerURL: settings.ServerURL,
		DeviceKey: settings.DeviceKey, KeyHint: settings.KeyHint,
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if targets, _ = st.ListBarkTargets(ctx); len(targets) != 0 {
		t.Fatalf("disabled settings must not notify: %+v", targets)
	}

	// Re-enabled but key-cleared rows never notify either; the key/hint update
	// is an upsert, not a duplicate row.
	if err := st.SetDeviceBark(ctx, DeviceBark{
		DeviceID: "dev_1", Enabled: true, ServerURL: settings.ServerURL, AllowPrivate: true,
	}); err != nil {
		t.Fatalf("re-enable without key: %v", err)
	}
	if targets, _ = st.ListBarkTargets(ctx); len(targets) != 0 {
		t.Fatalf("keyless settings must not notify: %+v", targets)
	}
	got, _ = st.GetDeviceBark(ctx, "dev_1")
	if got.DeviceKey != "" || !got.AllowPrivate {
		t.Fatalf("cleared key/LAN flag not persisted: %+v", got)
	}
}

func TestListBarkTargetsExcludesRevokedDeviceOrKey(t *testing.T) {
	st := newBarkStore(t)
	seedBarkDevice(t, st)
	ctx := context.Background()
	if err := st.SetDeviceBark(ctx, DeviceBark{
		DeviceID: "dev_1", Enabled: true, ServerURL: "https://api.day.app",
		DeviceKey: "sekret-key-1", KeyHint: "••••key-1",
	}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if targets, _ := st.ListBarkTargets(ctx); len(targets) != 1 {
		t.Fatalf("expected one target: %+v", targets)
	}
	if err := st.SetDeviceRevoked(ctx, "dev_1", true); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if targets, _ := st.ListBarkTargets(ctx); len(targets) != 0 {
		t.Fatalf("revoked device must not receive Bark notifications: %+v", targets)
	}
}

func TestDeleteDeviceBarkIdempotent(t *testing.T) {
	st := newBarkStore(t)
	seedBarkDevice(t, st)
	ctx := context.Background()
	if err := st.SetDeviceBark(ctx, DeviceBark{DeviceID: "dev_1", Enabled: true}); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.DeleteDeviceBark(ctx, "dev_1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteDeviceBark(ctx, "dev_1"); err != nil {
		t.Fatalf("delete again: %v", err)
	}
	if _, err := st.GetDeviceBark(ctx, "dev_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted settings must be gone, got %v", err)
	}
}
