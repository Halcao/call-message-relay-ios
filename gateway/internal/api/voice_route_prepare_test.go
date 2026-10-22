package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/auth"
	"callrelay/gateway/internal/call"
	"callrelay/gateway/internal/db"
	"callrelay/gateway/internal/modem"
)

// fakeVoiceRoute is a modem.VoiceAudio that additionally records
// PrepareRoute calls (the QDC507 pre-dial / pre-answer hook).
type fakeVoiceRoute struct {
	mu        sync.Mutex
	prepares  int
	failWith  error
	sequence  *[]string
}

func (f *fakeVoiceRoute) PrepareRoute(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares++
	if f.sequence != nil {
		*f.sequence = append(*f.sequence, "prepare")
	}
	if f.failWith != nil {
		return f.failWith
	}
	return nil
}

func (f *fakeVoiceRoute) Probe(context.Context) (modem.AudioCapabilities, error) {
	return modem.AudioCapabilities{Backend: "fake"}, nil
}
func (f *fakeVoiceRoute) Start(context.Context, modem.CallID) error { return nil }
func (f *fakeVoiceRoute) ReadPCM([]int16) (int, error)             { return 0, nil }
func (f *fakeVoiceRoute) WritePCM([]int16) (int, error)            { return 0, nil }
func (f *fakeVoiceRoute) Stop(context.Context) error               { return nil }
func (f *fakeVoiceRoute) Close() error                             { return nil }

// orderedCallModem records where Dial falls in the prepare/dial order.
type orderedCallModem struct {
	fakeCallModem
	sequence *[]string
}

func (m *orderedCallModem) Dial(context.Context, string) error {
	m.mu.Lock()
	m.dials++
	if m.sequence != nil {
		*m.sequence = append(*m.sequence, "dial")
	}
	m.mu.Unlock()
	return nil
}

func setupCallRouteServer(t *testing.T, voice modem.VoiceAudio, modemClient interface {
	CallModemLike
}) (*Server, http.Handler, string) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	application := NewServer("gw_test", "Test Gateway", "dev")
	application.Database = database
	application.Auth = auth.NewService("gw_test", database)
	application.Fingerprint = "sha256/test"
	application.CallControl = call.NewController()
	application.VoiceAudio = voice
	application.CallModem = modemClient
	handler := application.Handler()
	token := pairTestDevice(t, handler)
	return application, handler, token
}

// CallModemLike narrels the modem fake surface used by these tests.
type CallModemLike interface {
	Dial(context.Context, string) error
	Answer(context.Context) error
	Hangup(context.Context) error
	DTMF(context.Context, rune) error
}

func TestDialPreparesVoiceRouteBeforeDialing(t *testing.T) {
	var sequence []string
	voice := &fakeVoiceRoute{sequence: &sequence}
	modemClient := &orderedCallModem{sequence: &sequence}
	_, handler, token := setupCallRouteServer(t, voice, modemClient)

	body := `{"to":"+8613800138000","clientCallId":"client-call-1"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", "dial-1")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("dial status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if voice.prepares != 1 {
		t.Fatalf("route preparations = %d, want 1", voice.prepares)
	}
	if len(sequence) < 2 || sequence[0] != "prepare" || sequence[1] != "dial" {
		t.Fatalf("ordering = %v, want prepare before dial", sequence)
	}
}

func TestDialAbortsWhenVoiceRoutePreparationFails(t *testing.T) {
	voice := &fakeVoiceRoute{failWith: errors.New("route rotation failed")}
	modemClient := &orderedCallModem{}
	_, handler, token := setupCallRouteServer(t, voice, modemClient)

	body := `{"to":"+8613800138000","clientCallId":"client-call-1"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", "dial-fail-1")
	handler.ServeHTTP(recorder, request)
	if recorder.Code < 500 {
		t.Fatalf("dial status = %d, want 5xx: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "route rotation failed") {
		t.Fatalf("dial body missing honest failure reason: %s", recorder.Body.String())
	}
	modemClient.mu.Lock()
	dials := modemClient.dials
	modemClient.mu.Unlock()
	if dials != 0 {
		t.Fatalf("modem dialed %d times after route preparation failed", dials)
	}
}

func TestIncomingEventPreheatsVoiceRoute(t *testing.T) {
	voice := &fakeVoiceRoute{}
	modemClient := &orderedCallModem{}
	application, _, _ := setupCallRouteServer(t, voice, modemClient)

	application.HandleModemEvent(modem.ModemEvent{
		Kind:   "incoming",
		CallID: modem.CallID("incoming-1"),
		Peer:   "+8613800138000",
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		voice.mu.Lock()
		prepares := voice.prepares
		voice.mu.Unlock()
		if prepares == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("incoming RING did not preheat the voice route")
}

func TestIncomingEventWithoutRoutePreparerStillWorks(t *testing.T) {
	// Plain backends (no PrepareRoute) must not panic or fail the event.
	modemClient := &orderedCallModem{}
	application, _, _ := setupCallRouteServer(t, nil, modemClient)
	application.HandleModemEvent(modem.ModemEvent{
		Kind:   "incoming",
		CallID: modem.CallID("incoming-2"),
		Peer:   "+8613800138000",
	})
}
