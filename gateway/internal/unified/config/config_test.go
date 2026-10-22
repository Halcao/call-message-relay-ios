package config

import (
	"strings"
	"testing"
	"time"
)

func TestBarkDefaultsToDisabledWithSafePolicy(t *testing.T) {
	cfg := Default()
	if cfg.Bark.Enabled {
		t.Fatal("bark must be opt-in and disabled by default")
	}
	if cfg.Bark.AllowPrivate {
		t.Fatal("LAN Bark servers must require an explicit operator opt-in")
	}
	timeout, err := cfg.BarkTimeout()
	if err != nil || timeout != 10*time.Second {
		t.Fatalf("default bark timeout: %v %v", timeout, err)
	}
	maxAge, err := cfg.BarkMaxCallAge()
	if err != nil || maxAge != 60*time.Second {
		t.Fatalf("default bark max call age: %v %v", maxAge, err)
	}
}

func TestBarkDurationValidation(t *testing.T) {
	cfg := Default()
	cfg.Bark.Timeout = "soon"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("invalid bark.timeout must fail validation, got %v", err)
	}
	cfg = Default()
	cfg.Bark.MaxCallAge = "-5s"
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-positive bark.max_call_age must fail validation")
	}
	cfg = Default()
	cfg.Bark.Enabled = true
	cfg.Bark.AllowPrivate = true
	cfg.Bark.Timeout = "3s"
	cfg.Bark.MaxCallAge = "30s"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid bark config must pass: %v", err)
	}
	timeout, _ := cfg.BarkTimeout()
	if timeout != 3*time.Second {
		t.Fatalf("configured timeout applies: %v", timeout)
	}
}

func TestSTUNEndpointsValidation(t *testing.T) {
	cfg := Default()
	if valid, _ := cfg.STUNEndpoints(); len(valid) != 0 {
		t.Fatal("no STUN servers by default")
	}
	cfg.ICE.STUNServers = []string{
		"stun.cloudflare.com:3478",
		" stun.l.google.com:19302 ",
		"stun.cloudflare.com:3478", // duplicate
		"",
		"70000",        // invalid port
		"host:no-port", // non-numeric port
		":3478",        // empty host
		"good.example:3478",
	}
	valid, invalid := cfg.STUNEndpoints()
	if len(valid) != 3 || len(invalid) != 3 {
		t.Fatalf("expected 3 valid / 3 invalid, got %v / %v", valid, invalid)
	}
	if valid[0] != "stun.cloudflare.com:3478" ||
		valid[1] != "stun.l.google.com:19302" ||
		valid[2] != "good.example:3478" {
		t.Fatalf("valid order/content wrong: %v", valid)
	}
}
