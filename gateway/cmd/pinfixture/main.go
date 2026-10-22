// Command pinfixture generates deterministic cross-language QUIC pin
// fixtures for the iOS test suite. It builds a synthetic gateway identity
// (fixed seed — NO production secret) and a synthetic ECDSA P-256
// certificate, then emits the exact GET /api/v2/media/quic JSON the gateway
// would serve, signed through the production pin path
// (quic.BuildSignedPin -> identity.Sign -> pinCanonical).
//
// Usage:
//
//	go run ./cmd/pinfixture [-out dir]
//
// Writes three files into dir (default stdout for the valid one):
//
//	quic-pin-valid.json      (window 2025-01-01..2030-01-01)
//	quic-pin-expired.json    (window 2020-01-01..2021-01-01)
//	quic-pin-notyet.json     (window 2099-01-01..2100-01-01)
//	trusted-identity.txt     (synthetic publicKey + fingerprint to embed)
//
// The Swift suite embeds these bytes verbatim (see
// iPhoneApp/CallRelayTests/QUICPinFixture.swift) so the production decoder
// and trust verifier are tested against REAL Go serialization.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"time"

	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/quic"
)

const gatewayID = "gateway-quicdirect-fixture"

// deterministicReader seeds the P-256 key from a fixed stream so the whole
// fixture is byte-reproducible.
type deterministicReader struct{ buf []byte }

func (r *deterministicReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		next := sha256.Sum256([]byte("callrelay-pinfixture-ecdsa-stream"))
		r.buf = next[:]
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func syntheticIdentity() *identity.Identity {
	seed := sha256.Sum256([]byte("callrelay-quic-pin-fixture-v1"))
	private := ed25519.NewKeyFromSeed(seed[:])
	return &identity.Identity{
		ID:      gatewayID,
		Name:    "pinfixture",
		Private: private,
		Public:  private.Public().(ed25519.PublicKey),
	}
}

func fixtureCert(notBefore, notAfter time.Time) (*x509.Certificate, error) {
	// Deterministic P-256 key: ecdsa.GenerateKey is NOT usable here because
	// randutil.MaybeReadByte draws from the per-process global math/rand.
	// A fixed valid scalar (RFC 6979-style test vector) keeps the fixture
	// byte-reproducible while remaining a perfectly ordinary key.
	scalar := sha256.Sum256([]byte("callrelay-pinfixture-p256-scalar-v1"))
	scalar[0] &= 0xFF // documented fixed scalar; well below the group order
	curve := elliptic.P256()
	x, y := curve.ScalarBaseMult(scalar[:])
	key := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y},
		D:         new(big.Int).SetBytes(scalar[:]),
	}
	// Fixed serial (derived from the fixture seed) so the whole fixture is
	// byte-reproducible across runs and machines.
	serialSum := sha256.Sum256([]byte("callrelay-pinfixture-serial-v1"))
	serial := new(big.Int).SetBytes(serialSum[:])
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: gatewayID, Organization: []string{"CallRelay"}},
		DNSNames:              []string{gatewayID},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(&deterministicReader{}, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func report(id *identity.Identity, cert *x509.Certificate) ([]byte, error) {
	pin, err := quic.BuildSignedPin(cert, id)
	if err != nil {
		return nil, err
	}
	// Mirror the production response shape exactly (quic.CandidateReport).
	doc := struct {
		Enabled     bool            `json:"enabled"`
		ALPN        string          `json:"alpn"`
		Pin         quic.PinDocument `json:"pin"`
		Candidates  []quic.Candidate `json:"candidates"`
		STUNServers []string        `json:"stun"`
	}{
		Enabled: true,
		ALPN:    "callrelay-media",
		Pin:     pin,
		Candidates: []quic.Candidate{
			{Type: "srflx", Proto: "quic", Addr: "192.0.2.10:8792", ExpiresAtMs: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()},
		},
		STUNServers: []string{"192.0.2.53:3478"},
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func main() {
	outDir := ""
	if len(os.Args) == 3 && os.Args[1] == "-out" {
		outDir = os.Args[2]
	}
	id := syntheticIdentity()
	windows := []struct {
		name              string
		notBefore, notAfter time.Time
	}{
		{"quic-pin-valid.json", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"quic-pin-expired.json", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"quic-pin-notyet.json", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, w := range windows {
		cert, err := fixtureCert(w.notBefore, w.notAfter)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cert:", err)
			os.Exit(1)
		}
		payload, err := report(id, cert)
		if err != nil {
			fmt.Fprintln(os.Stderr, "pin:", err)
			os.Exit(1)
		}
		if outDir == "" {
			os.Stdout.Write(payload)
			continue
		}
		if err := os.WriteFile(outDir+"/"+w.name, payload, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "wrote", outDir+"/"+w.name)
	}
	trusted := fmt.Sprintf("publicKey=%s\nfingerprint=%s\ngatewayId=%s\n",
		id.PublicBase64(), id.Fingerprint(), id.ID)
	if outDir == "" {
		fmt.Fprint(os.Stderr, trusted)
	} else {
		if err := os.WriteFile(outDir+"/trusted-identity.txt", []byte(trusted), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "wrote", outDir+"/trusted-identity.txt")
	}
}

var _ io.Reader = (*deterministicReader)(nil)
