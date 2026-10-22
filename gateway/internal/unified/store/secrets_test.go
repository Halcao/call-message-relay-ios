package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSecretBoxSealOpenRoundTrip(t *testing.T) {
	box, err := NewRandomSecretBox()
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	plaintext := []byte("s3cret-value-not-for-logs")
	sealed, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Contains(sealed, plaintext) {
		t.Fatal("ciphertext contains the plaintext bytes")
	}
	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("open = %q, want %q", opened, plaintext)
	}
	// A different master key must fail closed and return no plaintext.
	other, err := NewRandomSecretBox()
	if err != nil {
		t.Fatalf("new other box: %v", err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("opening with the wrong master key must fail")
	}
	// Tampering with the ciphertext must fail the GCM tag check.
	sealed[len(sealed)-1] ^= 0xff
	if _, err := box.Open(sealed); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
}

func TestLoadOrCreateSecretBoxPersistsRestrictiveKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "console-secrets.key")
	first, err := LoadOrCreateSecretBox(path)
	if err != nil {
		t.Fatalf("create box: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("master key mode = %o, want 600", mode)
	}
	sealed, err := first.Seal([]byte("recoverable"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	reloaded, err := LoadOrCreateSecretBox(path)
	if err != nil {
		t.Fatalf("reload box: %v", err)
	}
	opened, err := reloaded.Open(sealed)
	if err != nil {
		t.Fatalf("reload open: %v", err)
	}
	if string(opened) != "recoverable" {
		t.Fatalf("reloaded plaintext = %q", opened)
	}
}

func TestStoreSealsCreatedKeySecretAndLeavesLegacyUnstored(t *testing.T) {
	ctx := context.Background()
	database, err := Open(filepath.Join(t.TempDir(), "secrets.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	box, err := NewRandomSecretBox()
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	database.Secrets = box

	hash := []byte{1, 2, 3}
	if err := database.CreateEnrollmentKey(ctx, "key_new", "new", hash, "top-secret", true); err != nil {
		t.Fatalf("create sealed key: %v", err)
	}
	secret, err := database.EnrollmentKeySecret(ctx, "key_new")
	if err != nil || secret != "top-secret" {
		t.Fatalf("secret = %q (%v), want top-secret", secret, err)
	}
	stored, err := database.KeySecretStored(ctx, "key_new")
	if err != nil || !stored {
		t.Fatalf("KeySecretStored = %v (%v), want true", stored, err)
	}
	var raw []byte
	if err := database.DB.QueryRowContext(ctx,
		`SELECT secret_encrypted FROM enrollment_keys WHERE id = ?`, "key_new").Scan(&raw); err != nil {
		t.Fatalf("read sealed: %v", err)
	}
	if bytes.Contains(raw, []byte("top-secret")) {
		t.Fatal("database column contains the plaintext secret")
	}

	// A key created without a master key (legacy path) stores NULL and reports
	// the honest unrecoverable status; hash authentication is unaffected.
	plain, err := Open(filepath.Join(t.TempDir(), "plain.sqlite"))
	if err != nil {
		t.Fatalf("open plain: %v", err)
	}
	defer plain.Close()
	if err := plain.Migrate(ctx); err != nil {
		t.Fatalf("migrate plain: %v", err)
	}
	if err := plain.CreateEnrollmentKey(ctx, "key_legacy", "legacy", hash, "old-secret", true); err != nil {
		t.Fatalf("create legacy key: %v", err)
	}
	stored, err = plain.KeySecretStored(ctx, "key_legacy")
	if err != nil || stored {
		t.Fatalf("legacy KeySecretStored = %v (%v), want false", stored, err)
	}
	if _, err := plain.EnrollmentKeySecret(ctx, "key_legacy"); err != ErrSecretStorageUnavailable {
		t.Fatalf("legacy reveal error = %v, want ErrSecretStorageUnavailable", err)
	}
	plain.Secrets = box
	if _, err := plain.EnrollmentKeySecret(ctx, "key_legacy"); err != ErrSecretNotStored {
		t.Fatalf("legacy with box error = %v, want ErrSecretNotStored", err)
	}
}

func TestLoadOrCreateTightensExistingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console-secrets.key")
	original, err := NewRandomSecretBox()
	if err != nil {
		t.Fatalf("box: %v", err)
	}
	sealed, err := original.Seal([]byte("keep-me"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	// Simulate a key file that was created by an older, laxer tool.
	key := make([]byte, secretKeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	box, err := NewSecretBox(key)
	if err != nil {
		t.Fatalf("box2: %v", err)
	}
	sealed, err = box.Seal([]byte("keep-me"))
	if err != nil {
		t.Fatalf("seal2: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(key) + "\n"
	if err := os.WriteFile(path, []byte(encoded), 0o644); err != nil {
		t.Fatalf("write lax key: %v", err)
	}
	loaded, err := LoadOrCreateSecretBox(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("existing key mode = %o, want 600 after load", mode)
	}
	opened, err := loaded.Open(sealed)
	if err != nil || string(opened) != "keep-me" {
		t.Fatalf("existing key material was replaced: %q (%v)", opened, err)
	}
}

func TestLoadOrCreateNeverOverwritesEstablishedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "console-secrets.key")
	first, err := LoadOrCreateSecretBox(path)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	sealed, err := first.Seal([]byte("established"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	second, err := LoadOrCreateSecretBox(path)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	opened, err := second.Open(sealed)
	if err != nil || string(opened) != "established" {
		t.Fatalf("second load replaced the established key: %q (%v)", opened, err)
	}

	// A concurrent first run must converge on exactly one key: every goroutine
	// either creates it or loads the winner's copy, and all can open the same
	// sealed value.
	racePath := filepath.Join(t.TempDir(), "race", "console-secrets.key")
	const workers = 8
	boxes := make(chan *SecretBox, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			box, err := LoadOrCreateSecretBox(racePath)
			if err != nil {
				errs <- err
				return
			}
			boxes <- box
		}()
	}
	wg.Wait()
	close(boxes)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent load: %v", err)
	}
	var winner *SecretBox
	count := 0
	for box := range boxes {
		if winner == nil {
			winner = box
		}
		count++
	}
	if count != workers {
		t.Fatalf("got %d boxes, want %d", count, workers)
	}
	marker, err := winner.Seal([]byte("race-marker"))
	if err != nil {
		t.Fatalf("seal marker: %v", err)
	}
	final, err := LoadOrCreateSecretBox(racePath)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	opened, err = final.Open(marker)
	if err != nil || string(opened) != "race-marker" {
		t.Fatalf("concurrent creation produced different keys: %q (%v)", opened, err)
	}
}
