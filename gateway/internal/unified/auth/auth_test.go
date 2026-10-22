package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/store"
)

func newTestService(t *testing.T) (*Service, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "unified.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	gatewayIdentity, err := identity.LoadOrCreate(filepath.Join(dir, "gateway.key"), "gw-test", "Test Gateway")
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	return New(st, gatewayIdentity, 15*time.Minute, 30*24*time.Hour), st
}

func newInstallation(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return publicKey, privateKey
}

func enrollInstallation(t *testing.T, service *Service, keyID, secret, name string, publicKey ed25519.PublicKey, privateKey ed25519.PrivateKey) (TokenPair, error) {
	t.Helper()
	message := keyID + "\n" + secret + "\n" + service.Identity.ID + "\n" + name
	proof := ed25519.Sign(privateKey, []byte(message))
	return service.Enroll(context.Background(), EnrollmentRequest{
		EnrollmentKey:   keyID + "." + secret,
		DeviceName:      name,
		DevicePublicKey: publicKey,
		Proof:           proof,
	})
}

func TestEnrollmentIsIdempotentPerInstallation(t *testing.T) {
	service, st := newTestService(t)
	keyID, secret, err := service.CreateKey(context.Background(), "test-key", true)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	publicKey, privateKey := newInstallation(t)
	first, err := enrollInstallation(t, service, keyID, secret, "iPhone", publicKey, privateKey)
	if err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	second, err := enrollInstallation(t, service, keyID, secret, "iPhone", publicKey, privateKey)
	if err != nil {
		t.Fatalf("retry enroll: %v", err)
	}
	if first.DeviceID != second.DeviceID {
		t.Fatalf("retry created a new device: %s vs %s", first.DeviceID, second.DeviceID)
	}
	count, err := st.CountDevicesForKey(context.Background(), keyID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("device count = %d, want 1", count)
	}
}

func TestConcurrentEnrollRetriesShareOneDevice(t *testing.T) {
	service, st := newTestService(t)
	keyID, secret, err := service.CreateKey(context.Background(), "test-key", true)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	publicKey, privateKey := newInstallation(t)
	const workers = 8
	deviceIDs := make(chan string, workers)
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			pair, err := enrollInstallation(t, service, keyID, secret, "iPhone", publicKey, privateKey)
			if err != nil {
				errorsCh <- err
				return
			}
			deviceIDs <- pair.DeviceID
		}()
	}
	wait.Wait()
	close(deviceIDs)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent enroll failed: %v", err)
	}
	unique := map[string]bool{}
	for deviceID := range deviceIDs {
		unique[deviceID] = true
	}
	if len(unique) != 1 {
		t.Fatalf("concurrent retries produced %d devices, want 1", len(unique))
	}
	count, _ := st.CountDevicesForKey(context.Background(), keyID)
	if count != 1 {
		t.Fatalf("device count = %d, want 1", count)
	}
}

func TestConcurrentRefreshHasSingleWinner(t *testing.T) {
	service, _ := newTestService(t)
	keyID, secret, err := service.CreateKey(context.Background(), "test-key", true)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	publicKey, privateKey := newInstallation(t)
	pair, err := enrollInstallation(t, service, keyID, secret, "iPhone", publicKey, privateKey)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	const workers = 16
	var wait sync.WaitGroup
	successes := make(chan TokenPair, workers)
	failures := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			refreshed, refreshErr := service.Refresh(context.Background(), pair.RefreshToken)
			if refreshErr != nil {
				failures <- refreshErr
				return
			}
			successes <- refreshed
		}()
	}
	close(start)
	wait.Wait()
	close(successes)
	close(failures)
	winners := 0
	for range successes {
		winners++
	}
	if winners != 1 {
		t.Fatalf("refresh winners = %d, want exactly 1", winners)
	}
	for failure := range failures {
		if !errors.Is(failure, ErrUnauthorized) {
			t.Fatalf("loser error = %v, want ErrUnauthorized", failure)
		}
	}
}

func TestRevokedDeviceCannotRefreshOrUseAccess(t *testing.T) {
	service, _ := newTestService(t)
	keyID, secret, _ := service.CreateKey(context.Background(), "test-key", true)
	publicKey, privateKey := newInstallation(t)
	pair, err := enrollInstallation(t, service, keyID, secret, "iPhone", publicKey, privateKey)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := service.RevokeDevice(context.Background(), pair.DeviceID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := service.Refresh(context.Background(), pair.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("refresh after revoke = %v, want ErrUnauthorized", err)
	}
	if _, _, err := service.Authenticate(context.Background(), "Bearer "+pair.AccessToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("access after revoke = %v, want ErrUnauthorized", err)
	}
	// The revoked installation must not silently re-enroll via a synced grant.
	if _, err := enrollInstallation(t, service, keyID, secret, "iPhone", publicKey, privateKey); !errors.Is(err, ErrRevokedInstallation) {
		t.Fatalf("re-enroll after revocation = %v, want ErrRevokedInstallation", err)
	}
}

func TestRevokedKeyBlocksEnrollmentAndRefresh(t *testing.T) {
	service, _ := newTestService(t)
	keyID, secret, _ := service.CreateKey(context.Background(), "test-key", true)
	publicKey, privateKey := newInstallation(t)
	pair, err := enrollInstallation(t, service, keyID, secret, "iPhone", publicKey, privateKey)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := service.RevokeKey(context.Background(), keyID); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	if _, err := service.Refresh(context.Background(), pair.RefreshToken); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("refresh after key revoke = %v, want ErrUnauthorized", err)
	}
	freshPublic, freshPrivate := newInstallation(t)
	if _, err := enrollInstallation(t, service, keyID, secret, "iPad", freshPublic, freshPrivate); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("enroll after key revoke = %v, want ErrInvalidKey", err)
	}
}

func TestEnrollmentRejectsWrongSecretAndBadProof(t *testing.T) {
	service, _ := newTestService(t)
	keyID, secret, _ := service.CreateKey(context.Background(), "test-key", true)
	publicKey, privateKey := newInstallation(t)
	if _, err := enrollInstallation(t, service, keyID, secret+"x", "iPhone", publicKey, privateKey); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("wrong secret = %v, want ErrInvalidKey", err)
	}
	// Proof signed by a different installation key must fail.
	otherPublic, otherPrivate := newInstallation(t)
	message := keyID + "\n" + secret + "\n" + service.Identity.ID + "\n" + "iPhone"
	proof := ed25519.Sign(otherPrivate, []byte(message))
	if _, err := service.Enroll(context.Background(), EnrollmentRequest{
		EnrollmentKey: keyID + "." + secret, DeviceName: "iPhone", DevicePublicKey: publicKey, Proof: proof,
	}); !errors.Is(err, ErrInvalidProof) {
		t.Fatalf("bad proof = %v, want ErrInvalidProof", err)
	}
	_ = otherPublic
}
