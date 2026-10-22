package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"callrelay/gateway/internal/modem"
)

// uncertainDialModem reports the adapter's fail-closed quarantine: the
// previous dial's outcome is unknown, so the line refuses new dials.
type uncertainDialModem struct {
	fakeCallModem
}

func (m *uncertainDialModem) Dial(context.Context, string) error {
	m.mu.Lock()
	m.dials++
	m.mu.Unlock()
	return modem.ErrDialUncertain
}

// TestDialUncertainMapsToConflictWithoutHangup: an uncertain modem state is
// surfaced as a line-busy conflict, and the handler must not issue any
// automatic modem hangup for it (the quarantine owns that decision).
func TestDialUncertainMapsToConflictWithoutHangup(t *testing.T) {
	voice := &fakeVoiceRoute{}
	modemClient := &uncertainDialModem{}
	_, handler, token := setupCallRouteServer(t, voice, modemClient)

	body := `{"to":"+8613800138000","clientCallId":"client-call-1"}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/calls", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Idempotency-Key", "dial-uncertain-1")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "CB-CALL-013") {
		t.Fatalf("body missing uncertain code: %s", recorder.Body.String())
	}
	modemClient.mu.Lock()
	dials, hangups := modemClient.dials, modemClient.hangups
	modemClient.mu.Unlock()
	if dials != 1 {
		t.Fatalf("dials = %d, want 1", dials)
	}
	if hangups != 0 {
		t.Fatalf("automatic hangup issued for an uncertain dial: %d", hangups)
	}
}
