package runtime

import (
	"strings"
	"testing"

	"callrelay/gateway/internal/unified/config"
)

// The ICE configuration must expose public STUN servers for P2P address
// discovery independently of TURN, keep LAN host candidates and the WSS
// fallback, and label the policy honestly. A malformed STUN entry must never
// become fatal.
func TestICEConfigurationSTUNIndependentOfTURN(t *testing.T) {
	cfg := config.Default()
	cfg.ICE.STUNServers = []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302"}
	m := &Manager{Cfg: cfg}

	servers, policy := m.ICEConfiguration()
	if policy != "stun" {
		t.Fatalf("policy = %q, want stun (STUN configured, TURN off)", policy)
	}
	if len(servers) != 2 {
		t.Fatalf("expected 2 STUN servers, got %d", len(servers))
	}
	for _, s := range servers {
		if len(s.URLs) != 1 || !strings.HasPrefix(s.URLs[0], "stun:") {
			t.Fatalf("server URLs must be bare stun: entries, got %v", s.URLs)
		}
		if s.Username != "" || s.Credential != nil {
			t.Fatalf("STUN servers carry no credentials: %+v", s)
		}
	}

	// Malformed entries are skipped, not fatal.
	cfg.ICE.STUNServers = append(cfg.ICE.STUNServers, "bad-entry", ":3478")
	servers, policy = m.ICEConfiguration()
	if policy != "stun" || len(servers) != 2 {
		t.Fatalf("malformed STUN entries must be skipped: policy=%q servers=%d", policy, len(servers))
	}
}

func TestICEConfigurationNoSTUNKeepsHostPolicy(t *testing.T) {
	cfg := config.Default()
	m := &Manager{Cfg: cfg}
	servers, policy := m.ICEConfiguration()
	if policy != "host" || len(servers) != 0 {
		t.Fatalf("default must stay host-only: policy=%q servers=%d", policy, len(servers))
	}
}
