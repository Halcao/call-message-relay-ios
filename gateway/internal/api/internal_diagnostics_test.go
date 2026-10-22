package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"callrelay/gateway/internal/modem"
)

type diagnosticsModem struct {
	*fakeCallModem
	results []modem.ATDiagnostic
	err     error
	calls   int
}

func (m *diagnosticsModem) Diagnostics(context.Context) ([]modem.ATDiagnostic, error) {
	m.calls++
	return m.results, m.err
}

func TestInternalATDiagnosticsEndpoint(t *testing.T) {
	server := NewServer("gw_test", "Test", "dev")
	modemStub := &diagnosticsModem{
		fakeCallModem: &fakeCallModem{},
		results: []modem.ATDiagnostic{
			{Command: "AT+CEER", Lines: []string{`+CEER: 16,"Normal call clearing"`, "OK"}},
			{Command: "AT+CIREG?", Error: "AT command failed: ERROR"},
		},
	}
	server.CallModem = modemStub
	handler := server.InternalHandler("secret-token")

	// Missing/wrong token is rejected and never reaches the modem queue.
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/diagnostics/at", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", response.Code)
	}
	if modemStub.calls != 0 {
		t.Fatalf("unauthorized request reached the modem queue %d times", modemStub.calls)
	}

	request = httptest.NewRequest(http.MethodGet, "/internal/v1/diagnostics/at", nil)
	request.Header.Set("X-Internal-Token", "secret-token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authed status = %d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Supported bool                 `json:"supported"`
		AT        []modem.ATDiagnostic `json:"at"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Supported || len(body.AT) != 2 {
		t.Fatalf("payload = %#v", body)
	}
	if body.AT[0].Command != "AT+CEER" || len(body.AT[0].Lines) != 2 {
		t.Fatalf("CEER result = %#v", body.AT[0])
	}
	if body.AT[1].Error == "" {
		t.Fatalf("per-command failure lost: %#v", body.AT[1])
	}
}

func TestInternalATDiagnosticsUnsupportedBackend(t *testing.T) {
	server := NewServer("gw_test", "Test", "dev")
	server.CallModem = &fakeCallModem{} // no ATDiagnoser
	handler := server.InternalHandler("secret-token")
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/diagnostics/at", nil)
	request.Header.Set("X-Internal-Token", "secret-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", response.Code)
	}
}

func TestInternalATDiagnosticsReportsQueueError(t *testing.T) {
	server := NewServer("gw_test", "Test", "dev")
	server.CallModem = &diagnosticsModem{fakeCallModem: &fakeCallModem{}, err: errors.New("AT client is unavailable")}
	handler := server.InternalHandler("secret-token")
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/diagnostics/at", nil)
	request.Header.Set("X-Internal-Token", "secret-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
}
