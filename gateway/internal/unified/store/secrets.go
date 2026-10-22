package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// secretKeySize is the AES-256 master key length.
const secretKeySize = 32

var (
	// ErrSecretStorageUnavailable means no master key was loaded, so no
	// recoverable secret can be sealed or opened. Authentication (hash) is
	// unaffected; only the operator-facing reveal feature degrades.
	ErrSecretStorageUnavailable = errors.New("recoverable secret storage is not configured")
	// ErrSecretNotStored means this key predates recoverable secret storage
	// (or was created while it was unavailable). Hash-only keys cannot be
	// reconstructed; callers must offer a fresh replacement key instead.
	ErrSecretNotStored = errors.New("no recoverable secret stored for this key")
)

// SecretBox seals small operator-recoverable secrets (one-time enrollment key
// secrets) at rest with AES-256-GCM. The 32-byte master key lives in a
// root-only file separate from the database: an attacker who copies only
// unified.sqlite cannot read any secret, while losing the master key makes
// stored secrets unrecoverable without affecting hash-based authentication.
type SecretBox struct {
	aead cipher.AEAD
}

// NewSecretBox validates a raw 32-byte master key and returns a ready box.
func NewSecretBox(key []byte) (*SecretBox, error) {
	if len(key) != secretKeySize {
		return nil, fmt.Errorf("secret master key must be %d bytes, got %d", secretKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init secret cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("init secret aead: %w", err)
	}
	return &SecretBox{aead: aead}, nil
}

// NewRandomSecretBox mints a fresh random master key in memory. It exists for
// tests and first-run provisioning; production uses LoadOrCreateSecretBox so
// the key survives restarts.
func NewRandomSecretBox() (*SecretBox, error) {
	key := make([]byte, secretKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return NewSecretBox(key)
}

// LoadOrCreateSecretBox reads a base64 master key from path, creating it on
// first use with mode 0600 (parent directories 0750). Creation is atomic and
// no-clobber: a concurrent first run can never overwrite the key another
// process just established, and an existing key is never replaced. Existing
// files with group/other permission bits are tightened to 0600.
func LoadOrCreateSecretBox(path string) (*SecretBox, error) {
	if box, err := readSecretBox(path); err == nil {
		return box, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	key := make([]byte, secretKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := createSecretKeyNoClobber(path, key); err != nil {
		if errors.Is(err, os.ErrExist) {
			// Lost the first-run race: use the winner's key, never ours.
			return readSecretBox(path)
		}
		return nil, err
	}
	return NewSecretBox(key)
}

// readSecretBox loads and validates an existing key file. A missing file
// reports os.ErrNotExist so callers can create one.
func readSecretBox(path string) (*SecretBox, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("secret master key %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("secret master key %s is not a regular file", path)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read secret master key %s: %w", path, err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(payload)))
	if err != nil {
		return nil, fmt.Errorf("decode secret master key %s: %w", path, err)
	}
	// An existing key that is group/other readable is a leak: restrict it
	// before using it, and fail closed if that is not possible.
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("restrict secret master key %s: %w", path, err)
		}
	}
	return NewSecretBox(key)
}

// createSecretKeyNoClobber installs the key without ever replacing an existing
// file. It writes a private temp file and hard-links it into place; link(2)
// fails with EEXIST if another process won the race, which is exactly the
// guarantee needed. Filesystems without hard links fall back to O_EXCL.
func createSecretKeyNoClobber(path string, key []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create secret key directory: %w", err)
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(key) + "\n")
	temp, err := os.CreateTemp(dir, ".console-secrets-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary secret key: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("restrict temporary secret key: %w", err)
	}
	if _, err := temp.Write(encoded); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary secret key: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync temporary secret key: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary secret key: %w", err)
	}
	if err := os.Link(tempPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		// Hard links may be unsupported (some mounted filesystems); retry
		// with an exclusive create so an existing key is still never
		// overwritten.
		file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if openErr != nil {
			if errors.Is(openErr, os.ErrExist) {
				return os.ErrExist
			}
			return fmt.Errorf("install secret master key: %w", err)
		}
		if _, writeErr := file.Write(encoded); writeErr != nil {
			file.Close()
			_ = os.Remove(path)
			return fmt.Errorf("write secret master key: %w", writeErr)
		}
		if syncErr := file.Sync(); syncErr != nil {
			file.Close()
			_ = os.Remove(path)
			return fmt.Errorf("sync secret master key: %w", syncErr)
		}
		return file.Close()
	}
	return nil
}

// Seal encrypts plaintext with a fresh random nonce prepended to the
// ciphertext. A nil box is a programming error; callers must check
// Enabled first.
func (b *SecretBox) Seal(plaintext []byte) ([]byte, error) {
	if b == nil {
		return nil, ErrSecretStorageUnavailable
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts a value produced by Seal. A wrong master key or tampered
// ciphertext fails closed with an error; no partial plaintext escapes.
func (b *SecretBox) Open(sealed []byte) ([]byte, error) {
	if b == nil {
		return nil, ErrSecretStorageUnavailable
	}
	nonceSize := b.aead.NonceSize()
	if len(sealed) < nonceSize {
		return nil, errors.New("sealed secret is truncated")
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt stored secret: %w", err)
	}
	return plaintext, nil
}

// SecretsEnabled reports whether this store can seal and open secrets.
func (s *Store) SecretsEnabled() bool { return s != nil && s.Secrets != nil }
