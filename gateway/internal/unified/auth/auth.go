// Package auth implements unified enrollment keys and per-device tokens.
// One enrollment key can attach many devices and grant many lines; each
// device gets its own Ed25519 identity and independently revocable tokens.
package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/store"
)

var (
	ErrInvalidKey          = errors.New("enrollment key is invalid or revoked")
	ErrInvalidProof        = errors.New("enrollment proof is invalid")
	ErrUnauthorized        = errors.New("unauthorized")
	ErrRevokedInstallation = errors.New("this installation identity was revoked by the owner")
)

type Service struct {
	Store      *store.Store
	Identity   *identity.Identity
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	version    atomic.Int64
}

func New(store *store.Store, gatewayIdentity *identity.Identity, accessTTL, refreshTTL time.Duration) *Service {
	return &Service{Store: store, Identity: gatewayIdentity, AccessTTL: accessTTL, RefreshTTL: refreshTTL}
}

// Version increments on any revocation or permission change so live event
// streams can re-check and drop narrowed access promptly.
func (s *Service) Version() int64 { return s.version.Load() }

func (s *Service) BumpVersion() { s.version.Add(1) }

type EnrollmentRequest struct {
	EnrollmentKey   string
	DeviceName      string
	DevicePublicKey []byte
	Proof           []byte
}

type TokenPair struct {
	DeviceID     string
	AccessToken  string
	RefreshToken string
}

func newToken(prefix string) (string, error) {
	payload := make([]byte, 32)
	if _, err := rand.Read(payload); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func (s *Service) Enroll(ctx context.Context, request EnrollmentRequest) (TokenPair, error) {
	keyID, secret, ok := strings.Cut(strings.TrimSpace(request.EnrollmentKey), ".")
	if !ok || strings.TrimSpace(keyID) == "" || strings.TrimSpace(secret) == "" {
		return TokenPair{}, ErrInvalidKey
	}
	if len(request.DevicePublicKey) != ed25519.PublicKeySize {
		return TokenPair{}, ErrInvalidProof
	}
	key, secretHash, err := s.Store.GetEnrollmentKey(ctx, keyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return TokenPair{}, ErrInvalidKey
		}
		return TokenPair{}, err
	}
	presentedHash := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(presentedHash[:], secretHash) != 1 {
		return TokenPair{}, ErrInvalidKey
	}
	deviceName := strings.TrimSpace(request.DeviceName)
	if deviceName == "" || len(deviceName) > 64 {
		return TokenPair{}, fmt.Errorf("deviceName must be 1-64 characters")
	}
	message := keyID + "\n" + secret + "\n" + s.Identity.ID + "\n" + deviceName
	if !ed25519.Verify(ed25519.PublicKey(request.DevicePublicKey), []byte(message), request.Proof) {
		return TokenPair{}, ErrInvalidProof
	}
	newDeviceID, err := randomID("dev_")
	if err != nil {
		return TokenPair{}, err
	}
	accessToken, refreshToken, hashes, err := s.mintTokens()
	if err != nil {
		return TokenPair{}, err
	}
	// The store transaction re-checks key and installation revocation and
	// is idempotent per (key, installation public key): a network retry
	// resumes the same device instead of creating duplicates, and a
	// concurrent revoke cannot be raced.
	deviceID, _, err := s.Store.CreateDeviceWithTokens(ctx, key.ID, deviceName, request.DevicePublicKey, newDeviceID, hashes)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrKeyRevoked):
			return TokenPair{}, ErrInvalidKey
		case errors.Is(err, store.ErrInstallationRevoked):
			// A synced recovery grant must not silently re-enroll a revoked
			// installation. The app persists this state and requires an
			// explicit owner action (fresh identity or console unbind).
			return TokenPair{}, ErrRevokedInstallation
		default:
			return TokenPair{}, err
		}
	}
	s.BumpVersion()
	return TokenPair{DeviceID: deviceID, AccessToken: accessToken, RefreshToken: refreshToken}, nil
}

func randomID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *Service) mintTokens() (string, string, store.TokenHashes, error) {
	accessToken, err := newToken("cra_")
	if err != nil {
		return "", "", store.TokenHashes{}, err
	}
	refreshToken, err := newToken("crr_")
	if err != nil {
		return "", "", store.TokenHashes{}, err
	}
	now := time.Now().UTC()
	hashes := store.TokenHashes{
		AccessHash:       hashToken(accessToken),
		RefreshHash:      hashToken(refreshToken),
		AccessExpiresAt:  now.Add(s.AccessTTL),
		RefreshExpiresAt: now.Add(s.RefreshTTL),
	}
	return accessToken, refreshToken, hashes, nil
}

// Refresh consumes the presented single-use refresh token and issues a new
// pair in one transaction. Concurrent refreshes with the same token have
// exactly one winner; revoked devices or keys cannot mint new tokens.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	accessToken, newRefresh, hashes, err := s.mintTokens()
	if err != nil {
		return TokenPair{}, err
	}
	deviceID, err := s.Store.RotateRefreshToken(ctx, hashToken(refreshToken), hashes.AccessHash, hashes.RefreshHash, hashes.AccessExpiresAt, hashes.RefreshExpiresAt)
	if err != nil {
		return TokenPair{}, ErrUnauthorized
	}
	return TokenPair{DeviceID: deviceID, AccessToken: accessToken, RefreshToken: newRefresh}, nil
}

// Authenticate resolves a bearer access token to live device access. Key or
// device revocation is checked on every call.
func (s *Service) Authenticate(ctx context.Context, bearer string) (store.Access, []byte, error) {
	token := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(bearer), "Bearer "))
	if token == "" {
		return store.Access{}, nil, ErrUnauthorized
	}
	record, err := s.Store.LookupDeviceToken(ctx, hashToken(token), "access")
	if err != nil {
		return store.Access{}, nil, ErrUnauthorized
	}
	access, err := s.Store.DeviceAccess(ctx, record.DeviceID)
	if err != nil {
		return store.Access{}, nil, ErrUnauthorized
	}
	return access, hashToken(token), nil
}

// CreateKey mints a new enrollment key and returns the one-time secret
// material. Only the hash is stored; the caller must show it exactly once.
func (s *Service) CreateKey(ctx context.Context, name string, allLines bool) (string, string, error) {
	keyID, err := randomID("key_")
	if err != nil {
		return "", "", err
	}
	secret, err := newToken("")
	if err != nil {
		return "", "", err
	}
	secretHash := sha256.Sum256([]byte(secret))
	if err := s.Store.CreateEnrollmentKey(ctx, keyID, strings.TrimSpace(name), secretHash[:], secret, allLines); err != nil {
		return "", "", err
	}
	s.BumpVersion()
	return keyID, secret, nil
}

func (s *Service) RevokeDevice(ctx context.Context, deviceID string) error {
	if err := s.Store.SetDeviceRevoked(ctx, deviceID, true); err != nil {
		return err
	}
	s.BumpVersion()
	return nil
}

func (s *Service) RevokeKey(ctx context.Context, keyID string) error {
	if err := s.Store.SetEnrollmentKeyRevoked(ctx, keyID, true); err != nil {
		return err
	}
	s.BumpVersion()
	return nil
}

// DeleteRevokedKey permanently removes an already-revoked enrollment key and
// its explicit line grants. Active keys are refused (store.ErrKeyActive): the
// operator must revoke first, so deletion can never bypass the two-step
// safety property. Device rows, tokens' history and call/SMS records stay.
func (s *Service) DeleteRevokedKey(ctx context.Context, keyID string) error {
	if err := s.Store.DeleteRevokedKey(ctx, keyID); err != nil {
		return err
	}
	s.BumpVersion()
	return nil
}

// SetKeyManageNumber grants or removes the explicit capability to edit the
// own number of lines the key's devices are authorized to use. It changes no
// line grants; ordinary/shared keys keep the capability off by default.
func (s *Service) SetKeyManageNumber(ctx context.Context, keyID string, allowed bool) error {
	if err := s.Store.SetKeyManageNumber(ctx, keyID, allowed); err != nil {
		return err
	}
	s.BumpVersion()
	return nil
}
