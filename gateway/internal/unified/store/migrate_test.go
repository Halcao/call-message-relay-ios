package store

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigrateAddsManageNumberColumn proves an older database (created without
// the manage_number column on enrollment_keys) migrates additively and that
// existing keys default the capability to false.
func TestMigrateAddsManageNumberColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "unified.sqlite")

	legacy, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := legacy.DB.ExecContext(ctx, `CREATE TABLE enrollment_keys (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, secret_hash BLOB NOT NULL,
		all_lines INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, revoked_at INTEGER)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := legacy.DB.ExecContext(ctx, `INSERT INTO enrollment_keys
		(id, name, secret_hash, all_lines, created_at) VALUES ('key_old','old', x'00', 1, 0)`); err != nil {
		t.Fatalf("insert legacy key: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer migrated.Close()
	if err := migrated.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	key, _, err := migrated.GetEnrollmentKey(ctx, "key_old")
	if err != nil {
		t.Fatalf("get legacy key: %v", err)
	}
	if key.ManageNumber {
		t.Fatal("existing key must default manage_number to false")
	}
	if !key.AllLines {
		t.Fatal("existing all_lines value must survive migration")
	}
	if err := migrated.SetKeyManageNumber(ctx, "key_old", true); err != nil {
		t.Fatalf("enable capability: %v", err)
	}
	key, _, _ = migrated.GetEnrollmentKey(ctx, "key_old")
	if !key.ManageNumber {
		t.Fatal("manage_number toggle did not persist")
	}
}

// TestMigrateAddsSecretEncryptedColumn proves older databases gain the
// recoverable-secret column additively while existing keys stay hash-only
// (NULL), which the console reports as honestly unrecoverable.
func TestMigrateAddsSecretEncryptedColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "unified.sqlite")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer database.Close()
	if _, err := database.DB.ExecContext(ctx, `CREATE TABLE enrollment_keys (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, secret_hash BLOB NOT NULL,
		all_lines INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, revoked_at INTEGER)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := database.DB.ExecContext(ctx, `INSERT INTO enrollment_keys
		(id, name, secret_hash, all_lines, created_at) VALUES ('key_old','old', x'00', 1, 0)`); err != nil {
		t.Fatalf("insert legacy key: %v", err)
	}
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := database.DB.ExecContext(ctx,
		`UPDATE enrollment_keys SET secret_encrypted = x'01' WHERE id = 'key_old'`); err != nil {
		t.Fatalf("secret_encrypted column missing after migrate: %v", err)
	}
	stored, err := database.KeySecretStored(ctx, "key_old")
	if err != nil || !stored {
		t.Fatalf("KeySecretStored = %v (%v), want true after writing the column", stored, err)
	}
}
