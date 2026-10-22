// Package identity owns the unified service's Ed25519 gateway identity. The
// fingerprint is what the iOS client pins before enrollment and on every
// credentialed connection, replacing the upstream v1 placeholder string.
package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

type Identity struct {
	ID      string
	Name    string
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

func LoadOrCreate(path, id, name string) (*Identity, error) {
	if payload, err := os.ReadFile(path); err == nil {
		if len(payload) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("gateway key file %s has %d bytes, expected %d", path, len(payload), ed25519.PrivateKeySize)
		}
		private := ed25519.PrivateKey(append([]byte(nil), payload...))
		return &Identity{ID: id, Name: name, Private: private, Public: private.Public().(ed25519.PublicKey)}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, private, 0o600); err != nil {
		return nil, err
	}
	return &Identity{ID: id, Name: name, Private: private, Public: private.Public().(ed25519.PublicKey)}, nil
}

func (i *Identity) Fingerprint() string {
	sum := sha256.Sum256(i.Public)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (i *Identity) PublicBase64() string {
	return base64.StdEncoding.EncodeToString(i.Public)
}

func (i *Identity) Sign(message []byte) []byte {
	return ed25519.Sign(i.Private, message)
}
