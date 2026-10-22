package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"callrelay/gateway/internal/modem"
)

type identityModem struct {
	*fakeCallModem
	identity modem.SIMIdentity
}

func (m *identityModem) OwnIdentity(context.Context) (modem.SIMIdentity, error) {
	return m.identity, nil
}

func TestInternalSIMIdentityEndpoint(t *testing.T) {
	server := NewServer("gw_test", "Test", "dev")
	server.CallModem = &identityModem{
		fakeCallModem: &fakeCallModem{},
		identity: modem.SIMIdentity{
			Number: "+15550001111", NumberSource: "sim", ICCID: "0123456789ABCDEF0123",
			IMSI: "001012000011111", OperatorAlpha: "Test Operator", Commands: map[string]string{"AT+CNUM": "ok"},
		},
	}
	handler := server.InternalHandler("secret-token")

	// Missing token is rejected.
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/identity/sim", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", response.Code)
	}

	// Correct token returns the full (loopback-private) payload.
	request = httptest.NewRequest(http.MethodGet, "/internal/v1/identity/sim", nil)
	request.Header.Set("X-Internal-Token", "secret-token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authed status = %d body=%s", response.Code, response.Body.String())
	}
	var got modem.SIMIdentity
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Number != "+15550001111" || got.NumberSource != "sim" || got.IMSI != "001012000011111" {
		t.Fatalf("payload = %#v", got)
	}
}

func TestInternalSIMIdentityUnsupportedWithoutReader(t *testing.T) {
	server := NewServer("gw_test", "Test", "dev")
	server.CallModem = &fakeCallModem{} // no IdentityReader
	handler := server.InternalHandler("secret-token")
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/identity/sim", nil)
	request.Header.Set("X-Internal-Token", "secret-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", response.Code)
	}
}
