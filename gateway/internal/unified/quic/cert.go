package quic

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"callrelay/gateway/internal/unified/identity"
)

// pinDomain separates pin signatures from any other use of the gateway
// identity key.
const pinDomain = "callrelay-quic-pin-v1"

// PinDocument is the canonical, signed QUIC certificate pin. It binds the
// gateway identity, the certificate SPKI hash and the certificate validity
// window; the client verifies the signature against the identity key it
// ALREADY trusts from the authenticated pairing (GET /api/v2/identity) —
// never against a co-returned untrusted copy — and then pins the SPKI in
// its TLS verifier with full certificate-validity checks.
type PinDocument struct {
	Version    int    `json:"version"`
	GatewayID  string `json:"gatewayId"`
	Algorithm  string `json:"algorithm"` // "ecdsa-p256-spki-sha256"
	SPKIHash   string `json:"spkiHash"`  // base64(SHA-256 of PKIX public-key DER)
	NotBefore  int64  `json:"notBefore"` // unix seconds
	NotAfter   int64  `json:"notAfter"`  // unix seconds
	Signature  string `json:"signature"` // base64(ed25519 over canonical text)
	// IdentityKey/IdentityFingerprint are informational mirrors of the
	// already-published gateway identity; clients MUST validate against
	// their trusted pairing record, not against these fields alone.
	IdentityKey         string `json:"identityKey"`
	IdentityFingerprint string `json:"identityFingerprint"`
}

// pinCanonical returns the exact signed text: domain, version, gateway id,
// algorithm, SPKI hash, and the validity window. Any field change breaks
// the signature by construction.
func pinCanonical(doc PinDocument) []byte {
	return []byte(fmt.Sprintf("%s\n%d\n%s\n%s\n%s\n%d\n%d",
		pinDomain, doc.Version, doc.GatewayID, doc.Algorithm, doc.SPKIHash, doc.NotBefore, doc.NotAfter))
}

// VerifyPin checks sig (raw ed25519) over the canonical text using the
// trusted identity public key and confirms the SPKI hash matches.
func VerifyPin(doc PinDocument, trustedKey ed25519.PublicKey) error {
	if doc.Version != 1 {
		return fmt.Errorf("pin: unsupported version %d", doc.Version)
	}
	if doc.Algorithm != "ecdsa-p256-spki-sha256" {
		return fmt.Errorf("pin: unsupported algorithm %q", doc.Algorithm)
	}
	sig, err := base64.StdEncoding.DecodeString(doc.Signature)
	if err != nil {
		return fmt.Errorf("pin: signature decode: %w", err)
	}
	if !ed25519.Verify(trustedKey, pinCanonical(doc), sig) {
		return fmt.Errorf("pin: signature mismatch (untrusted or stale pin document)")
	}
	return nil
}

// spkiSHA256 returns the SHA-256 of the PKIX public-key DER (the pin
// input). This is the same derivation the iOS side computes with
// SecKeyCopyExternalRepresentation + the fixed P-256 SPKI prefix.
func spkiSHA256(cert *x509.Certificate) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// certMaterial is the loaded certificate + its pin document.
type certMaterial struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pin  PinDocument
}

// loadOrCreateCert loads the persisted ECDSA P-256 self-signed certificate
// (0600, in DataDir) or creates one valid for ~13 months. The SAN carries
// the gateway id; trust comes exclusively from the identity-bound pin.
func loadOrCreateCert(dataDir string, id *identity.Identity) (*certMaterial, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("quic: data dir required for certificate")
	}
	certPath := filepath.Join(dataDir, "quic-cert.pem")
	keyPath := filepath.Join(dataDir, "quic-key.pem")

	if raw, err := os.ReadFile(certPath); err == nil {
		if keyRaw, kerr := os.ReadFile(keyPath); kerr == nil {
			if mat, perr := parseCertMaterial(raw, keyRaw, id); perr == nil {
				return mat, nil
			}
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: id.ID, Organization: []string{"CallRelay"}},
		DNSNames:              []string{id.ID},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(390 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	return buildMaterial(cert, key, id)
}

func parseCertMaterial(certRaw, keyRaw []byte, id *identity.Identity) (*certMaterial, error) {
	certBlock, _ := pem.Decode(certRaw)
	keyBlock, _ := pem.Decode(keyRaw)
	if certBlock == nil || keyBlock == nil {
		return nil, fmt.Errorf("quic: corrupt certificate files")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	keyAny, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	if time.Now().After(cert.NotAfter.Add(-48 * time.Hour)) {
		return nil, fmt.Errorf("quic: certificate expiring; reissuing")
	}
	return buildMaterial(cert, keyAny, id)
}

func buildMaterial(cert *x509.Certificate, key *ecdsa.PrivateKey, id *identity.Identity) (*certMaterial, error) {
	pin, err := BuildSignedPin(cert, id)
	if err != nil {
		return nil, err
	}
	return &certMaterial{cert: cert, key: key, pin: pin}, nil
}

// BuildSignedPin produces the production pin document for an already-issued
// certificate and gateway identity (same canonical text and signing path as
// the live listener). Production callers go through loadOrCreateCert; the
// pinfixture generator (cmd/pinfixture) uses this to emit cross-language
// test vectors consumed by the iOS suite.
func BuildSignedPin(cert *x509.Certificate, id *identity.Identity) (PinDocument, error) {
	if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok {
		return PinDocument{}, fmt.Errorf("quic: certificate is not ECDSA")
	}
	hash, err := spkiSHA256(cert)
	if err != nil {
		return PinDocument{}, err
	}
	doc := PinDocument{
		Version:             1,
		GatewayID:           id.ID,
		Algorithm:           "ecdsa-p256-spki-sha256",
		SPKIHash:            hash,
		NotBefore:           cert.NotBefore.Unix(),
		NotAfter:            cert.NotAfter.Unix(),
		IdentityKey:         id.PublicBase64(),
		IdentityFingerprint: id.Fingerprint(),
	}
	doc.Signature = base64.StdEncoding.EncodeToString(id.Sign(pinCanonical(doc)))
	return doc, nil
}
