package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Health exposes push readiness so a client can distinguish "token
// registered" from "the gateway can actually send": an absent broker still
// accepts registrations but never delivers, and a broker environment that
// disagrees with the app's signed one rejects every push. The section is
// anonymous (readiness must be readable while credentials are being
// repaired) and contains no secret material — a boolean and the configured
// environment name only.
func TestHealthExposesPushReadiness(t *testing.T) {
	stack := newTestStack(t)

	decode := func() map[string]any {
		t.Helper()
		status, payload := stack.request(http.MethodGet, "/api/v2/health", "", "", nil)
		if status != http.StatusOK {
			t.Fatalf("health status = %d, body=%s", status, payload)
		}
		var body map[string]any
		if err := json.Unmarshal(payload, &body); err != nil {
			t.Fatalf("decode health: %v (%s)", err, payload)
		}
		return body
	}

	body := decode()
	push, ok := body["push"].(map[string]any)
	if !ok {
		t.Fatalf("health has no push section: %s", body)
	}
	if configured, _ := push["configured"].(bool); configured {
		t.Fatalf("test manager has no push sender, configured must be false: %v", push)
	}
	if _, present := push["environment"]; present {
		t.Fatalf("environment must be omitted when unconfigured: %v", push)
	}

	// A configured environment is normalized and reported.
	stack.api.Cfg.Push.APNsEnvironment = " Sandbox "
	push, _ = decode()["push"].(map[string]any)
	if got, _ := push["environment"].(string); got != "sandbox" {
		t.Fatalf("environment = %q, want sandbox (normalized)", got)
	}
}
