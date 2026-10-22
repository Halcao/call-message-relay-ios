package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/auth"
	"callrelay/gateway/internal/unified/config"
	"callrelay/gateway/internal/unified/events"
	"callrelay/gateway/internal/unified/identity"
	"callrelay/gateway/internal/unified/media"
	"callrelay/gateway/internal/unified/runtime"
	"callrelay/gateway/internal/unified/store"

	"github.com/gorilla/websocket"
)

type testStack struct {
	t        *testing.T
	cfg      config.Config
	store    *store.Store
	auth     *auth.Service
	bus      *events.Bus
	manager  *runtime.Manager
	identity *identity.Identity
	server   *httptest.Server
	api      *Server
	fakes    []*fakeWorker
	cancel   context.CancelFunc
	// deviceID is the most recently enrolled device (tests enroll one).
	deviceID string
}

func newTestStack(t *testing.T) *testStack {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Gateway.ID = "gw-test"
	cfg.Gateway.Name = "Test"
	cfg.Gateway.DataDir = dir
	cfg.Gateway.Listen = "127.0.0.1:0"
	cfg.Voicemail.AudioRoot = filepath.Join(dir, "sd-missing", "voicemail")
	cfg.Voicemail.MountPoint = filepath.Join(dir, "sd-missing")
	cfg.Voicemail.ExpectedDevice = "/dev/mmcblk0"

	fakes := []*fakeWorker{newFakeWorker("line1"), newFakeWorker("line2")}
	for index, fake := range fakes {
		tokenFile := filepath.Join(dir, fmt.Sprintf("internal%d.token", index+1))
		if err := os.WriteFile(tokenFile, []byte("test-internal-token"), 0o600); err != nil {
			t.Fatalf("write token: %v", err)
		}
		cfg.Lines = append(cfg.Lines, config.Line{
			ID: fake.LineID, Name: fake.LineID, WorkerURL: fake.server.URL,
			TokenFile: tokenFile, IdentityFile: filepath.Join(dir, "missing-identity.json"), Enabled: true,
		})
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	database, err := store.Open(filepath.Join(dir, "unified.sqlite"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	gatewayIdentity, err := identity.LoadOrCreate(filepath.Join(dir, "gateway.key"), cfg.Gateway.ID, cfg.Gateway.Name)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	authService := auth.New(database, gatewayIdentity, time.Minute, time.Hour)
	bus := events.NewBus(database)
	engine, err := media.NewEngine()
	if err != nil {
		t.Fatalf("media engine: %v", err)
	}
	manager := runtime.NewManager(cfg, database, bus, authService, engine, nil)
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Start(ctx); err != nil {
		t.Fatalf("manager start: %v", err)
	}
	apiServer := NewServer(cfg, authService, database, bus, manager, gatewayIdentity)
	server := httptest.NewServer(apiServer.Handler())
	stack := &testStack{
		t: t, cfg: cfg, store: database, auth: authService, bus: bus, manager: manager,
		identity: gatewayIdentity, server: server, api: apiServer, fakes: fakes, cancel: cancel,
	}
	t.Cleanup(func() {
		server.Close()
		cancel()
		manager.Close()
		for _, fake := range fakes {
			fake.close()
		}
		_ = database.Close()
	})
	return stack
}

// createKey creates an enrollment key; when allLines is false a line1 grant
// is added explicitly.
func (s *testStack) createKey(allLines bool) (string, string) {
	keyID, secret, err := s.auth.CreateKey(context.Background(), "test-key", allLines)
	if err != nil {
		s.t.Fatalf("create key: %v", err)
	}
	if !allLines {
		if err := s.store.SetKeyLine(context.Background(), store.KeyLine{
			KeyID: keyID, LineID: "line1", ReceiveSMS: true, ReceiveCalls: true, SendSMS: true, Dial: true,
		}); err != nil {
			s.t.Fatalf("grant line1: %v", err)
		}
	}
	return keyID, secret
}

func (s *testStack) enroll(keyID, secret, name string) (string, ed25519.PrivateKey) {
	s.t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		s.t.Fatalf("generate key: %v", err)
	}
	message := keyID + "\n" + secret + "\n" + s.identity.ID + "\n" + name
	proof := ed25519.Sign(privateKey, []byte(message))
	body, _ := json.Marshal(map[string]string{
		"enrollmentKey": keyID + "." + secret, "deviceName": name,
		"devicePublicKey": base64.StdEncoding.EncodeToString(publicKey),
		"proof":           base64.StdEncoding.EncodeToString(proof),
	})
	status, response := s.request(http.MethodPost, "/api/v2/enroll", "", "", body)
	if status != http.StatusOK {
		s.t.Fatalf("enroll status %d: %s", status, response)
	}
	var pair struct {
		DeviceID     string `json:"deviceId"`
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(response, &pair); err != nil {
		s.t.Fatalf("enroll decode: %v", err)
	}
	s.deviceID = pair.DeviceID
	return pair.AccessToken, privateKey
}

func (s *testStack) request(method, path, token, idempotencyKey string, body []byte) (int, []byte) {
	s.t.Helper()
	request, err := http.NewRequest(method, s.server.URL+path, bytes.NewReader(body))
	if err != nil {
		s.t.Fatalf("request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		s.t.Fatalf("do: %v", err)
	}
	defer response.Body.Close()
	payload := make([]byte, 0)
	buffer := make([]byte, 4096)
	for {
		count, readErr := response.Body.Read(buffer)
		payload = append(payload, buffer[:count]...)
		if readErr != nil {
			break
		}
	}
	return response.StatusCode, payload
}

func (s *testStack) jsonRequest(method, path, token, idempotencyKey string, value any, out any) int {
	s.t.Helper()
	body, _ := json.Marshal(value)
	status, payload := s.request(method, path, token, idempotencyKey, body)
	if out != nil && len(payload) > 0 {
		_ = json.Unmarshal(payload, out)
	}
	return status
}

func (s *testStack) waitForCall(token, callID string) map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, payload := s.request(http.MethodGet, "/api/v2/calls?active=true", token, "", nil)
		if status == http.StatusOK {
			var calls []map[string]any
			if json.Unmarshal(payload, &calls) == nil {
				for _, call := range calls {
					if callID == "" || call["id"] == callID {
						return call
					}
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.t.Fatalf("call %q did not appear", callID)
	return nil
}

func TestLateCallerIDUpdatesUnifiedCall(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	// Ring first with an unknown peer, then deliver the +CLIP update for the
	// same call. The unified record must gain the number without a second
	// call record or a state reset.
	stack.fakes[0].injectIncoming("call-clip-1", "")
	call := stack.waitForCall(token, "line1:call-clip-1")
	if peer, _ := call["peer"].(string); peer != "" {
		t.Fatalf("ringing peer = %q, want empty", peer)
	}
	stack.fakes[0].injectCallUpdate("call-clip-1", "+8618712345678")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, payload := stack.request(http.MethodGet, "/api/v2/calls?active=true", token, "", nil)
		if status == http.StatusOK {
			var calls []map[string]any
			if json.Unmarshal(payload, &calls) == nil {
				matches := 0
				for _, entry := range calls {
					if entry["id"] == "line1:call-clip-1" {
						matches++
						if peer, _ := entry["peer"].(string); peer == "+8618712345678" {
							return
						}
					}
				}
				if matches > 1 {
					t.Fatalf("late caller id duplicated the call: %s", payload)
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("unified call never received the late caller id")
}

func TestIdentityAndAuthBoundary(t *testing.T) {
	stack := newTestStack(t)
	status, payload := stack.request(http.MethodGet, "/api/v2/identity", "", "", nil)
	if status != http.StatusOK || !bytes.Contains(payload, []byte(stack.identity.Fingerprint())) {
		t.Fatalf("identity status %d: %s", status, payload)
	}
	if status, _ := stack.request(http.MethodGet, "/api/v2/lines", "", "", nil); status != http.StatusUnauthorized {
		t.Fatalf("anonymous lines status = %d, want 401", status)
	}
	status, _ = stack.request(http.MethodGet, "/api/v2/lines", "bogus-token", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("bogus token status = %d, want 401", status)
	}
}

func TestCrossKeyLineIsolation(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(false)
	tokenA, _ := stack.enroll(keyA, secretA, "Device A")
	tokenB, _ := stack.enroll(keyB, secretB, "Device B")

	var linesA, linesB []map[string]any
	if status := stack.jsonRequest(http.MethodGet, "/api/v2/lines", tokenA, "", nil, &linesA); status != http.StatusOK {
		t.Fatalf("lines A status %d", status)
	}
	if status := stack.jsonRequest(http.MethodGet, "/api/v2/lines", tokenB, "", nil, &linesB); status != http.StatusOK {
		t.Fatalf("lines B status %d", status)
	}
	if len(linesA) != 2 {
		t.Fatalf("all-lines key sees %d lines, want 2", len(linesA))
	}
	if len(linesB) != 1 || linesB[0]["id"] != "line1" {
		t.Fatalf("line1-only key sees %v, want only line1", linesB)
	}

	// Threads must be filtered to the granted line.
	var threadsB []map[string]any
	if status := stack.jsonRequest(http.MethodGet, "/api/v2/threads", tokenB, "", nil, &threadsB); status != http.StatusOK {
		t.Fatalf("threads B status %d", status)
	}
	for _, thread := range threadsB {
		if thread["lineId"] != "line1" {
			t.Fatalf("line2 thread leaked to line1-only key: %v", thread)
		}
	}
	// Sending on line2 must be forbidden.
	status := stack.jsonRequest(http.MethodPost, "/api/v2/messages", tokenB, "iso-1",
		map[string]string{"lineId": "line2", "to": "10086", "body": "hello"}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("line2 send status = %d, want 403", status)
	}
	// Calls list filtered by a forbidden line is rejected too.
	if status, _ := stack.request(http.MethodGet, "/api/v2/calls?line=line2", tokenB, "", nil); status != http.StatusForbidden {
		t.Fatalf("line2 calls status = %d, want 403", status)
	}
	// Voicemail index isolation.
	now := time.Now().UTC()
	for _, voicemail := range []store.Voicemail{
		{ID: "vm_l1", LineID: "line1", State: "ready", CreatedAt: now, ExpiresAt: now.AddDate(0, 0, 30)},
		{ID: "vm_l2", LineID: "line2", State: "ready", CreatedAt: now, ExpiresAt: now.AddDate(0, 0, 30)},
	} {
		if err := stack.store.InsertVoicemail(context.Background(), voicemail); err != nil {
			t.Fatalf("insert voicemail: %v", err)
		}
	}
	var voicemailsB []map[string]any
	if status := stack.jsonRequest(http.MethodGet, "/api/v2/voicemails", tokenB, "", nil, &voicemailsB); status != http.StatusOK {
		t.Fatalf("voicemails B status %d", status)
	}
	if len(voicemailsB) != 1 || voicemailsB[0]["lineId"] != "line1" {
		t.Fatalf("voicemail isolation failed: %v", voicemailsB)
	}
	if status, _ := stack.request(http.MethodGet, "/api/v2/voicemails/vm_l2/audio", tokenB, "", nil); status == http.StatusOK {
		t.Fatal("line2 voicemail audio readable by line1-only key")
	}

	// Authorized DELETE: hidden from the list immediately, a repeat delete
	// converges to 404, and a key without the line cannot delete it.
	var deleted map[string]any
	if status := stack.jsonRequest(http.MethodDelete, "/api/v2/voicemails/vm_l1", tokenB, "", nil, &deleted); status != http.StatusOK {
		t.Fatalf("delete own voicemail status = %d", status)
	}
	if deleted["id"] != "vm_l1" || deleted["deleted"] != true {
		t.Fatalf("delete response = %#v", deleted)
	}
	if status, _ := stack.request(http.MethodDelete, "/api/v2/voicemails/vm_l1", tokenB, "", nil); status != http.StatusNotFound {
		t.Fatalf("repeat delete status = %d, want 404", status)
	}
	voicemailsB = nil
	if status := stack.jsonRequest(http.MethodGet, "/api/v2/voicemails", tokenB, "", nil, &voicemailsB); status != http.StatusOK {
		t.Fatalf("voicemails after delete status %d", status)
	}
	if len(voicemailsB) != 0 {
		t.Fatalf("deleted voicemail still listed: %v", voicemailsB)
	}
	if status, _ := stack.request(http.MethodDelete, "/api/v2/voicemails/vm_l2", tokenB, "", nil); status != http.StatusForbidden {
		t.Fatalf("cross-line delete status = %d, want 403", status)
	}
}

func TestRevocationInvalidatesDeviceAndKey(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	if status, _ := stack.request(http.MethodGet, "/api/v2/device", token, "", nil); status != http.StatusOK {
		t.Fatalf("device status before revoke")
	}
	deviceID := ""
	var device struct {
		Device struct {
			ID string `json:"id"`
		} `json:"device"`
	}
	if status := stack.jsonRequest(http.MethodGet, "/api/v2/device", token, "", nil, &device); status != http.StatusOK {
		t.Fatalf("device decode failed")
	}
	deviceID = device.Device.ID
	if err := stack.auth.RevokeDevice(context.Background(), deviceID); err != nil {
		t.Fatalf("revoke device: %v", err)
	}
	if status, _ := stack.request(http.MethodGet, "/api/v2/device", token, "", nil); status != http.StatusUnauthorized {
		t.Fatalf("revoked device token still accepted")
	}
	// Key revocation invalidates an otherwise live device immediately.
	key2, secret2 := stack.createKey(true)
	token2, _ := stack.enroll(key2, secret2, "Device C")
	if err := stack.auth.RevokeKey(context.Background(), key2); err != nil {
		t.Fatalf("revoke key: %v", err)
	}
	if status, _ := stack.request(http.MethodGet, "/api/v2/device", token2, "", nil); status != http.StatusUnauthorized {
		t.Fatalf("revoked key token still accepted")
	}
}

func TestConcurrentAnswerSingleWinner(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(false)
	tokenA, _ := stack.enroll(keyA, secretA, "Device A")
	tokenB, _ := stack.enroll(keyB, secretB, "Device B")

	stack.fakes[0].injectIncoming("call-in-1", "+8613800000001")
	call := stack.waitForCall(tokenA, "line1:call-in-1")

	var wait sync.WaitGroup
	statuses := make(chan int, 2)
	for index, token := range []string{tokenA, tokenB} {
		wait.Add(1)
		go func(index int, token string) {
			defer wait.Done()
			status := stack.jsonRequest(http.MethodPost, "/api/v2/calls/line1:call-in-1/answer", token,
				fmt.Sprintf("answer-%d", index), map[string]string{}, nil)
			statuses <- status
		}(index, token)
	}
	wait.Wait()
	close(statuses)
	wins, conflicts := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusCreated:
			wins++
		case http.StatusConflict:
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("answer race: wins=%d conflicts=%d, want 1/1", wins, conflicts)
	}
	if _, answers := stack.fakes[0].counts(); answers != 1 {
		t.Fatalf("worker answer calls = %d, want exactly 1", answers)
	}
	if call["state"] == nil {
		t.Fatal("call snapshot missing")
	}
}

func TestDialIdempotencyAndLineBusy(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	status := stack.jsonRequest(http.MethodPost, "/api/v2/calls", token, "dial-1",
		map[string]string{"lineId": "line1", "to": "10086", "clientCallId": "client-1"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("dial status = %d, want 201", status)
	}
	status = stack.jsonRequest(http.MethodPost, "/api/v2/calls", token, "dial-1",
		map[string]string{"lineId": "line1", "to": "10086", "clientCallId": "client-1"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("replayed dial status = %d, want 201", status)
	}
	if dials, _ := stack.fakes[0].counts(); dials != 1 {
		t.Fatalf("worker dial calls = %d, want 1 after idempotent replay", dials)
	}
	status = stack.jsonRequest(http.MethodPost, "/api/v2/calls", token, "dial-2",
		map[string]string{"lineId": "line1", "to": "10010", "clientCallId": "client-2"}, nil)
	if status != http.StatusConflict {
		t.Fatalf("second dial on busy line = %d, want 409", status)
	}
}

func TestSMSDryRunToggleAndFailedMessage(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")

	if err := stack.manager.SetLineSMSDryRun(context.Background(), "line1", false); err != nil {
		t.Fatalf("enable live SMS: %v", err)
	}
	stack.fakes[0].mu.Lock()
	live := stack.fakes[0].dryRun
	stack.fakes[0].mu.Unlock()
	if live {
		t.Fatal("worker still in dry-run after live enable")
	}
	if err := stack.manager.SetLineSMSDryRun(context.Background(), "line1", true); err != nil {
		t.Fatalf("restore dry-run: %v", err)
	}

	stack.fakes[0].setFailNextSMS()
	var message map[string]any
	status := stack.jsonRequest(http.MethodPost, "/api/v2/messages", token, "sms-1",
		map[string]string{"lineId": "line1", "to": "10086", "body": "hi"}, &message)
	if status != http.StatusCreated {
		t.Fatalf("failed message status = %d, want persisted 201", status)
	}
	if message["status"] != "failed" {
		t.Fatalf("message status = %v, want failed", message["status"])
	}
	var sent map[string]any
	status = stack.jsonRequest(http.MethodPost, "/api/v2/messages", token, "sms-2",
		map[string]string{"lineId": "line1", "to": "10086", "body": "again"}, &sent)
	if status != http.StatusCreated || sent["status"] != "sent" {
		t.Fatalf("successful message status = %d/%v", status, sent["status"])
	}
}

func TestConferenceSizeCapAndMerge(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device A")
	waitForLineReady(t, stack, token)

	stack.fakes[0].injectIncoming("call-c1", "+8613800000001")
	stack.fakes[1].injectIncoming("call-c2", "+8613800000002")
	stack.waitForCall(token, "line1:call-c1")
	stack.waitForCall(token, "line2:call-c2")
	for _, callID := range []string{"line1:call-c1", "line2:call-c2"} {
		if status := stack.jsonRequest(http.MethodPost, "/api/v2/calls/"+callID+"/answer", token, "ans-"+callID, map[string]string{}, nil); status != http.StatusCreated {
			t.Fatalf("answer %s status = %d", callID, status)
		}
		waitForCallState(t, stack, token, callID, "active")
	}
	// Four external legs exceed the four-person cap.
	status := stack.jsonRequest(http.MethodPost, "/api/v2/conferences", token, "conf-too-big",
		map[string][]string{"callIds": {"a", "b", "c", "d"}}, nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("oversized conference status = %d, want 422", status)
	}
	var conference map[string]any
	status = stack.jsonRequest(http.MethodPost, "/api/v2/conferences", token, "conf-1",
		map[string][]string{"callIds": {"line1:call-c1", "line2:call-c2"}}, &conference)
	if status != http.StatusCreated {
		t.Fatalf("merge status = %d, want 201", status)
	}
	legs, _ := conference["legs"].([]any)
	if len(legs) != 2 {
		t.Fatalf("conference legs = %d, want 2", len(legs))
	}
	// A second conference for an already-merged call is rejected.
	status = stack.jsonRequest(http.MethodPost, "/api/v2/conferences", token, "conf-2",
		map[string][]string{"callIds": {"line1:call-c1", "line2:call-c2"}}, nil)
	if status == http.StatusCreated {
		t.Fatal("call merged into two conferences")
	}
}

func TestEventsStreamPermissionFilter(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.createKey(true)
	keyB, secretB := stack.createKey(false)
	tokenA, _ := stack.enroll(keyA, secretA, "Device A")
	tokenB, _ := stack.enroll(keyB, secretB, "Device B")
	_ = tokenA

	header := http.Header{"Authorization": []string{"Bearer " + tokenB}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/events"
	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), url, header)
	if err != nil {
		t.Fatalf("events dial: %v", err)
	}
	defer conn.Close()

	// Publish a forbidden line2 event and an allowed line1 event.
	if _, err := stack.bus.Publish(context.Background(), "line2", "", "call.updated", map[string]string{"id": "line2:secret"}); err != nil {
		t.Fatalf("publish line2: %v", err)
	}
	if _, err := stack.bus.Publish(context.Background(), "line1", "", "call.updated", map[string]string{"id": "line1:visible"}); err != nil {
		t.Fatalf("publish line1: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read events: %v", err)
		}
		var event map[string]any
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		if event["lineId"] == "line2" {
			t.Fatalf("forbidden line2 event leaked: %s", payload)
		}
		if event["lineId"] == "line1" {
			return // allowed event observed; isolation holds
		}
	}
}

// grantLine1 bends one key's per-line permissions for matrix tests.
func (s *testStack) grantLine1(keyID string, permissions store.KeyLine) {
	s.t.Helper()
	permissions.KeyID = keyID
	permissions.LineID = "line1"
	if err := s.store.SetKeyLine(context.Background(), permissions); err != nil {
		s.t.Fatalf("grant: %v", err)
	}
}

func readEventWithLine(t *testing.T, conn *websocket.Conn, lineID string, timeout time.Duration) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read event waiting for %s: %v", lineID, err)
		}
		var event map[string]any
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		if event["lineId"] == lineID {
			return event
		}
	}
}

func TestPermissionMatrixDirectionalIsolation(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(false)
	stack.grantLine1(keyID, store.KeyLine{ReceiveSMS: false, ReceiveCalls: false, SendSMS: true, Dial: true})
	if err := stack.store.SetKeyLine(context.Background(), store.KeyLine{
		KeyID: keyID, LineID: "line2", ReceiveSMS: false, ReceiveCalls: false, SendSMS: true, Dial: true,
	}); err != nil {
		t.Fatalf("grant line2: %v", err)
	}
	token, _ := stack.enroll(keyID, secret, "Send+Dial device")

	// Inbound call metadata must be invisible without receive_calls.
	stack.fakes[0].injectIncoming("call-matrix-in", "+8613800000009")
	waitForAnyCall(t, stack, token)
	status, _ := stack.request(http.MethodGet, "/api/v2/calls/line1:call-matrix-in", token, "", nil)
	if status != http.StatusForbidden {
		t.Fatalf("inbound call fetch status = %d, want 403", status)
	}
	var calls []map[string]any
	stack.jsonRequest(http.MethodGet, "/api/v2/calls", token, "", nil, &calls)
	for _, call := range calls {
		if call["direction"] == "inbound" {
			t.Fatalf("inbound call leaked to dial-only device: %v", call)
		}
	}

	// Outbound call created by this device is visible and dialable. Use
	// line2 because line1 is occupied by the ringing inbound call.
	status = stack.jsonRequest(http.MethodPost, "/api/v2/calls", token, "matrix-dial",
		map[string]string{"lineId": "line2", "to": "10086", "clientCallId": "matrix-out"}, nil)
	if status != http.StatusCreated {
		t.Fatalf("outbound dial status = %d", status)
	}
	stack.jsonRequest(http.MethodGet, "/api/v2/calls", token, "", nil, &calls)
	seenOutbound := false
	for _, call := range calls {
		if call["id"] == "line2:matrix-out" {
			seenOutbound = true
		}
	}
	if !seenOutbound {
		t.Fatal("own outbound call missing from dial-only history")
	}

	// Inbound SMS body/peer must not leak; own outbound receipt may.
	var threads []map[string]any
	stack.jsonRequest(http.MethodGet, "/api/v2/threads", token, "", nil, &threads)
	if len(threads) != 0 {
		t.Fatalf("inbound-only thread leaked to send-only device: %v", threads)
	}
	var page []map[string]any
	stack.jsonRequest(http.MethodGet, "/api/v2/messages?threadKey=line1:line1-peer", token, "", nil, &page)
	for _, message := range page {
		if message["direction"] != "outbound" {
			t.Fatalf("inbound message leaked to send-only device: %v", message)
		}
	}

	// Voicemail requires receive_calls.
	now := time.Now().UTC()
	if err := stack.store.InsertVoicemail(context.Background(), store.Voicemail{
		ID: "vm_matrix", LineID: "line1", State: "ready", CreatedAt: now, ExpiresAt: now.AddDate(0, 0, 30),
	}); err != nil {
		t.Fatalf("insert voicemail: %v", err)
	}
	var voicemails []map[string]any
	stack.jsonRequest(http.MethodGet, "/api/v2/voicemails", token, "", nil, &voicemails)
	if len(voicemails) != 0 {
		t.Fatalf("voicemail leaked without receive_calls: %v", voicemails)
	}
	if status, _ := stack.request(http.MethodGet, "/api/v2/voicemails/vm_matrix/audio", token, "", nil); status == http.StatusOK {
		t.Fatal("voicemail audio leaked without receive_calls")
	}
}

func TestEventFilterDirectionAndLineLessEvents(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(false)
	stack.grantLine1(keyID, store.KeyLine{ReceiveSMS: false, ReceiveCalls: false, SendSMS: true, Dial: true})
	token, _ := stack.enroll(keyID, secret, "Event device")

	cursor, err := stack.store.LatestEventSeq(context.Background())
	if err != nil {
		t.Fatalf("latest seq: %v", err)
	}
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/events?after=" + fmt.Sprintf("%d", cursor)
	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), url, header)
	if err != nil {
		t.Fatalf("events dial: %v", err)
	}
	defer conn.Close()

	inbound, _ := stack.bus.Publish(context.Background(), "line1", "", "call.updated", map[string]any{"id": "line1:a", "direction": "inbound"})
	outbound, _ := stack.bus.Publish(context.Background(), "line1", "", "call.updated", map[string]any{"id": "line1:b", "direction": "outbound"})
	lineLess, _ := stack.bus.Publish(context.Background(), "", "", "gateway.warning", map[string]any{"code": "TEST"})
	_ = inbound
	_ = lineLess

	// Wait for the outbound call.updated, ignoring unrelated line.updated
	// traffic that may originate from the manager's status polling.
	deadline := time.Now().Add(3 * time.Second)
	sawOutbound := false
	for time.Now().Before(deadline) && !sawOutbound {
		_ = conn.SetReadDeadline(time.Now().Add(time.Until(deadline)))
		_, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var event map[string]any
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		data, _ := event["data"].(map[string]any)
		if event["type"] == "call.updated" && data["id"] == "line1:b" {
			sawOutbound = true
		}
	}
	if !sawOutbound {
		t.Fatal("own outbound call event was not delivered")
	}
	_ = outbound
	// Neither the inbound call event nor the line-less event may arrive.
	_ = conn.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			break
		}
		var event map[string]any
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		if event["lineId"] == "" {
			t.Fatalf("line-less event delivered: %s", payload)
		}
		if event["type"] == "call.updated" {
			data, _ := event["data"].(map[string]any)
			if data["id"] == "line1:a" {
				t.Fatalf("inbound call event delivered without receive_calls: %s", payload)
			}
		}
	}
}

func TestEventsOriginCheckRejectsSuffixMatch(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Origin device")
	wsURL := "ws" + stack.server.URL[len("http"):] + "/api/v2/events"
	header := http.Header{"Authorization": []string{"Bearer " + token}}

	badOrigin := http.Header{}
	for key, values := range header {
		badOrigin[key] = values
	}
	badOrigin.Set("Origin", "http://"+strings.TrimPrefix(stack.server.URL, "http://")+".evil.example")
	if _, response, err := websocket.DefaultDialer.DialContext(context.Background(), wsURL, badOrigin); err == nil {
		t.Fatal("suffix origin was accepted")
	} else if response != nil && response.StatusCode != http.StatusForbidden {
		t.Fatalf("suffix origin status = %d, want 403", response.StatusCode)
	}

	goodOrigin := http.Header{}
	for key, values := range header {
		goodOrigin[key] = values
	}
	goodOrigin.Set("Origin", "http://"+strings.TrimPrefix(stack.server.URL, "http://"))
	conn, _, err := websocket.DefaultDialer.DialContext(context.Background(), wsURL, goodOrigin)
	if err != nil {
		t.Fatalf("matching origin rejected: %v", err)
	}
	_ = conn.Close()
}

func waitForAnyCall(t *testing.T, stack *testStack, token string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, payload := stack.request(http.MethodGet, "/api/v2/calls?active=true", token, "", nil)
		if status == http.StatusOK && len(payload) > 2 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("inbound call did not reach the store")
}
