package quic

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/identity"
)

func TestPinSignVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id, err := identity.LoadOrCreate(filepath.Join(dir, "gw.key"), "gw-pin", "Pin Test")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	mat, err := loadOrCreateCert(filepath.Join(dir, "quic"), id)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	doc := mat.pin

	// The trusted key comes from the ALREADY-TRUSTED pairing record
	// (/api/v2/identity), never from the co-returned pin document.
	trusted, err := decodeBase64Key(id.PublicBase64())
	if err != nil {
		t.Fatalf("decode trusted key: %v", err)
	}
	if err := VerifyPin(doc, trusted); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Tamper with each security-relevant field: signature must fail.
	tampered := doc
	tampered.SPKIHash = doc.SPKIHash[:len(doc.SPKIHash)-2] + "AA"
	if err := VerifyPin(tampered, trusted); err == nil {
		t.Fatal("tampered SPKI verified")
	}
	tampered = doc
	tampered.GatewayID = "gw-evil"
	if err := VerifyPin(tampered, trusted); err == nil {
		t.Fatal("tampered gateway id verified")
	}
	tampered = doc
	tampered.NotAfter = doc.NotAfter + 1
	if err := VerifyPin(tampered, trusted); err == nil {
		t.Fatal("tampered expiry verified")
	}
	tampered = doc
	tampered.Version = 2
	if err := VerifyPin(tampered, trusted); err == nil {
		t.Fatal("tampered version verified")
	}

	// A different (equally trusted-looking) key must NOT verify.
	other, _, err := ed25519.GenerateKey(&fixedReader{})
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if err := VerifyPin(doc, other); err == nil {
		t.Fatal("verified against the wrong identity key")
	}
}

// fixedReader deterministically feeds ed25519.GenerateKey (which requires
// rand.Reader-shaped input but tolerates a deterministic source).
type fixedReader struct{ n int }

func (f *fixedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(f.n + i)
	}
	f.n++
	return len(p), nil
}

func decodeBase64Key(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return ed25519.PublicKey(raw), nil
}

func TestCertPersistenceAndReissue(t *testing.T) {
	dir := t.TempDir()
	id, err := identity.LoadOrCreate(filepath.Join(dir, "gw.key"), "gw-persist", "Persist")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	dataDir := filepath.Join(dir, "quic")
	mat1, err := loadOrCreateCert(dataDir, id)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	mat2, err := loadOrCreateCert(dataDir, id)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if mat1.pin.SPKIHash != mat2.pin.SPKIHash {
		t.Fatal("reload produced a different pin (cert not persisted)")
	}
	// Permissions on the private key are root-only.
	info, err := os.Stat(filepath.Join(dataDir, "quic-key.pem"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %#o, want 0600", info.Mode().Perm())
	}
	// Validity window is sane and the pin carries it.
	if mat1.pin.NotAfter <= mat1.pin.NotBefore {
		t.Fatal("invalid cert window")
	}
	if time.Until(time.Unix(mat1.pin.NotAfter, 0)) < 300*24*time.Hour {
		t.Fatal("cert validity too short")
	}
}
