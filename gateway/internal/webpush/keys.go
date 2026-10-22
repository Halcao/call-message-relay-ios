// Package webpush implements the self-hosted PWA notification path: each
// gateway owns its VAPID key pair and its browsers' push subscriptions, and
// sends standards-compliant Web Push (RFC 8030 + VAPID + RFC 8291 payload
// encryption) itself. There is no central broker: subscriptions live in the
// gateway store bound to the authenticated device that registered them.
package webpush

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// KeyPair is this gateway's VAPID identity. PrivateKeyPEM never leaves the
// gateway host; PublicKey is the base64url-encoded uncompressed P-256 point
// handed to browsers as applicationServerKey.
type KeyPair struct {
	PublicKey  string
	PrivateKey string
}

// LoadOrCreateKeys loads the VAPID key pair from path, creating it on first
// use. The private key is written with 0600 inside a 0700 directory and is
// generated at runtime — key material is never shipped inside the binary or
// any public artifact.
func LoadOrCreateKeys(path string) (*KeyPair, error) {
	if path == "" {
		return nil, errors.New("webpush: vapid key path is empty")
	}
	if data, err := os.ReadFile(path); err == nil {
		return decodeKeyPair(data)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("webpush: read vapid key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("webpush: create key directory: %w", err)
	}
	privateKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("webpush: generate vapid key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "VAPID PRIVATE KEY",
		Bytes: privateKey.Bytes(),
	})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("webpush: write vapid key: %w", err)
	}
	return decodeKeyPair(pemBytes)
}

func decodeKeyPair(data []byte) (*KeyPair, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("webpush: vapid key file is not PEM")
	}
	privateKey, err := ecdh.P256().NewPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("webpush: parse vapid private key: %w", err)
	}
	// The public key wire form is the uncompressed point 0x04||X||Y,
	// base64url-encoded without padding — exactly what PushManager.subscribe
	// expects for applicationServerKey.
	publicKey := privateKey.PublicKey().Bytes()
	return &KeyPair{
		PublicKey:  base64.RawURLEncoding.EncodeToString(publicKey),
		PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey.Bytes()),
	}, nil
}
