package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newDeleteKeyStore(t *testing.T) *Store {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "delete-key.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	box, err := NewRandomSecretBox()
	if err != nil {
		t.Fatalf("secret box: %v", err)
	}
	database.Secrets = box
	return database
}

// Only revoked keys can be deleted; active keys are refused so deletion can
// never bypass the explicit revoke step.
func TestDeleteKeyRefusesActiveAndRemovesRevoked(t *testing.T) {
	ctx := context.Background()
	st := newDeleteKeyStore(t)
	now := time.Now().UTC()

	if err := st.CreateEnrollmentKey(ctx, "key_active", "active", []byte{9, 9, 9}, "active-secret", true); err != nil {
		t.Fatalf("create active key: %v", err)
	}
	if err := st.CreateEnrollmentKey(ctx, "key_revoked", "revoked", []byte{7, 7, 7}, "revoked-secret", false); err != nil {
		t.Fatalf("create revoked key: %v", err)
	}
	if err := st.SetKeyLine(ctx, KeyLine{KeyID: "key_revoked", LineID: "line1", ReceiveSMS: true}); err != nil {
		t.Fatalf("grant revoked key: %v", err)
	}
	if err := st.SetKeyLine(ctx, KeyLine{KeyID: "key_active", LineID: "line2", Dial: true}); err != nil {
		t.Fatalf("grant active key: %v", err)
	}
	// A device enrolled under the key, with a live token pair, exists before
	// the key is revoked (the normal history shape).
	if _, _, err := st.CreateDeviceWithTokens(ctx, "key_revoked", "probe-iPhone", []byte{1, 2, 3, 4},
		"dev_under_revoked", TokenHashes{
			AccessHash:       []byte{0xaa},
			RefreshHash:      []byte{0xbb},
			AccessExpiresAt:  now.Add(time.Hour),
			RefreshExpiresAt: now.Add(24 * time.Hour),
		}); err != nil {
		t.Fatalf("create device: %v", err)
	}
	if err := st.SetEnrollmentKeyRevoked(ctx, "key_revoked", true); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if err := st.DeleteRevokedKey(ctx, "key_active"); !errors.Is(err, ErrKeyActive) {
		t.Fatalf("delete active key error = %v, want ErrKeyActive", err)
	}
	if _, _, err := st.GetEnrollmentKey(ctx, "key_active"); err != nil {
		t.Fatalf("active key must survive a refused delete: %v", err)
	}

	if err := st.DeleteRevokedKey(ctx, "key_revoked"); err != nil {
		t.Fatalf("delete revoked key: %v", err)
	}
	if _, _, err := st.GetEnrollmentKey(ctx, "key_revoked"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key lookup = %v, want ErrNotFound", err)
	}
	if grants, err := st.KeyLines(ctx, "key_revoked"); err != nil || len(grants) != 0 {
		t.Fatalf("grants after delete = %v (%v), want none", grants, err)
	}
	// Other keys' grants and secrets stay untouched.
	if grants, err := st.KeyLines(ctx, "key_active"); err != nil || len(grants) != 1 {
		t.Fatalf("active grants after delete = %v (%v), want 1", grants, err)
	}
	if secret, err := st.EnrollmentKeySecret(ctx, "key_active"); err != nil || secret != "active-secret" {
		t.Fatalf("active secret after delete = %q (%v)", secret, err)
	}
	// Device history rows survive and cannot authenticate without the key.
	device, err := st.GetDevice(ctx, "dev_under_revoked")
	if err != nil {
		t.Fatalf("device row must survive key deletion: %v", err)
	}
	if device.RevokedAt != nil {
		t.Fatalf("key deletion must not silently rewrite device revocation state")
	}
	if _, err := st.DeviceAccess(ctx, "dev_under_revoked"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("device access after key deletion = %v, want ErrNotFound", err)
	}
	var activeTokens int
	if err := st.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM device_tokens WHERE device_id = ? AND revoked_at IS NULL`,
		"dev_under_revoked").Scan(&activeTokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if activeTokens != 0 {
		t.Fatalf("live tokens after key deletion = %d, want 0", activeTokens)
	}
	// A second delete of the same id is a plain not-found, not a success.
	if err := st.DeleteRevokedKey(ctx, "key_revoked"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete error = %v, want ErrNotFound", err)
	}
}
