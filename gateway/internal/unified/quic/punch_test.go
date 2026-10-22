package quic

import (
	"testing"
	"time"
)

func TestValidatePunchTargetRejects(t *testing.T) {
	cases := []struct {
		name     string
		cand     string
		lanPunch bool
	}{
		{"hostname", "example.com:5000", false},
		{"garbage", "not-an-addr", false},
		{"unspecified", "0.0.0.0:5000", false},
		{"loopback v4", "127.0.0.1:5000", false},
		{"loopback v6", "[::1]:5000", false},
		{"multicast", "224.0.0.1:5000", false},
		{"broadcast", "255.255.255.255:5000", false},
		{"link-local", "169.254.1.1:5000", false},
		{"link-local v6", "[fe80::1]:5000", false},
		{"private rfc1918", "192.168.1.10:5000", false},
		{"private rfc1918 10", "10.0.0.5:5000", false},
		{"private rfc1918 172", "172.16.0.5:5000", false},
		{"private v6 ula", "[fd00::1]:5000", false},
		{"cgnat", "100.64.0.1:5000", false},
		{"reserved 192.0.0", "192.0.0.9:5000", false},
		{"doc 192.0.2", "192.0.2.1:5000", false},
		{"doc 198.51.100", "198.51.100.9:5000", false},
		{"doc 203.0.113", "203.0.113.9:5000", false},
		{"benchmarking", "198.18.0.1:5000", false},
		{"benchmarking 198.19", "198.19.255.254:5000", false},
		{"deprecated 6to4", "192.88.99.1:5000", false},
		{"reserved 240", "240.0.0.1:5000", false},
		{"doc v6", "[2001:db8::1]:5000", false},
		{"benchmark v6", "[2001:2::1]:5000", false},
		{"orchid v6", "[2001:10::1]:5000", false},
		{"low port", "8.8.8.8:80", false},
		{"privileged", "8.8.8.8:22", false},
		{"zone id", "[fe80::1%eth0]:5000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validatePunchTargetImpl(tc.cand, tc.lanPunch); err == nil {
				t.Fatalf("%q (lanPunch=%v) was accepted", tc.cand, tc.lanPunch)
			}
		})
	}
}

func TestValidatePunchTargetAccepts(t *testing.T) {
	cases := []string{
		"8.8.8.8:5000",
		"1.1.1.1:65535",
		"203.0.113.20:1024 -placeholder",
		"[2606:4700:4700::1111]:5000",
		"203.0.114.1:5000",
	}
	_ = cases // documentation ranges are rejected; real globals below.
	for _, cand := range []string{"8.8.8.8:5000", "1.1.1.1:65535", "[2606:4700:4700::1111]:5000", "203.0.114.1:5000"} {
		ap, err := validatePunchTargetImpl(cand, false)
		if err != nil {
			t.Fatalf("%q rejected: %v", cand, err)
		}
		if ap.Port() < 1024 {
			t.Fatalf("%q accepted with privileged port", cand)
		}
	}
	// LAN-scoped private targets pass only with the explicit flag.
	for _, cand := range []string{"192.168.1.10:5000", "[fd00::1]:5000"} {
		if _, err := validatePunchTargetImpl(cand, true); err != nil {
			t.Fatalf("LAN-scoped %q rejected: %v", cand, err)
		}
		if _, err := validatePunchTargetImpl(cand, false); err == nil {
			t.Fatalf("%q accepted without LAN scope", cand)
		}
	}
	// Mapped IPv4 is unmapped then validated.
	if _, err := validatePunchTargetImpl("[::ffff:10.1.2.3]:5000", false); err == nil {
		t.Fatal("mapped private address accepted")
	}
	if _, err := validatePunchTargetImpl("[::ffff:8.8.8.8]:5000", false); err != nil {
		t.Fatalf("mapped global address rejected: %v", err)
	}
}

func TestPunchRegistryLifecycle(t *testing.T) {
	r := newPunchRegistry(false)
	r.window = 60 * time.Millisecond

	// Duplicate active key rejected (no silent replacement / no leak).
	if _, err := r.begin("dev1", "call1", "8.8.8.8:5000"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := r.begin("dev1", "call1", "8.8.4.4:5000"); err == nil {
		t.Fatal("duplicate active punch accepted")
	}
	if r.active != 1 {
		t.Fatalf("active = %d, want 1", r.active)
	}

	// Consume releases the slot; replay rejected.
	sess, _ := r.begin("dev1", "call2", "8.8.8.8:5001")
	if !r.consume("call2", "dev1", sess.token) {
		t.Fatal("consume failed")
	}
	if r.consume("call2", "dev1", sess.token) {
		t.Fatal("replay consumed")
	}
	if r.active != 1 { // call1 session still live
		t.Fatalf("active after consume = %d, want 1", r.active)
	}

	// Expiry releases via gc without going negative.
	time.Sleep(80 * time.Millisecond)
	r.gc()
	if r.active != 0 {
		t.Fatalf("active after expiry = %d, want 0", r.active)
	}
	r.gc()
	if r.active != 0 {
		t.Fatalf("active after double gc = %d, want 0", r.active)
	}

	// Per-device rolling bound: 8 per minute, 9th rejected.
	r.window = time.Hour
	for i := 0; i < 8; i++ {
		callID := "callDev" + string(rune('A'+i))
		if _, err := r.begin("dev2", callID, "8.8.8.8:5000"); err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		// Consume each so the per-call guards do not trip.
		r.consume(callID, "dev2", r.sessions[punchKey{callID, "dev2"}].token)
	}
	if _, err := r.begin("dev2", "callDevZ", "8.8.8.8:5000"); err != ErrPunchRateLimited {
		t.Fatalf("9th per-device punch err = %v", err)
	}

	// Per-call bound (sized for the 45 s recovery contract: late recovery
	// after three failures must remain possible, with headroom for
	// network-epoch flaps).
	r2 := newPunchRegistry(false)
	r2.window = time.Hour
	for i := 0; i < 10; i++ {
		dev := "devCall" + string(rune('A'+i))
		if _, err := r2.begin(dev, "callC", "8.8.8.8:5000"); err != nil {
			t.Fatalf("call begin %d: %v", i, err)
		}
		r2.consume("callC", dev, r2.sessions[punchKey{"callC", dev}].token)
	}
	if _, err := r2.begin("devCallZ", "callC", "8.8.8.8:5000"); err == nil {
		t.Fatal("11th per-call punch accepted (budget exceeded)")
	}

	// Concurrency bound.
	r3 := newPunchRegistry(false)
	r3.window = time.Hour
	devs := []string{"d1", "d2", "d3", "d4"}
	for _, d := range devs {
		if _, err := r3.begin(d, "call-"+d, "8.8.8.8:5000"); err != nil {
			t.Fatalf("conc begin %s: %v", d, err)
		}
	}
	if _, err := r3.begin("d5", "call-d5", "8.8.8.8:5000"); err != ErrPunchRateLimited {
		t.Fatalf("5th concurrent punch err = %v", err)
	}
	if r3.active != 4 {
		t.Fatalf("active = %d, want 4", r3.active)
	}
	// Expire them all and confirm gc drains to exactly zero.
	r3.window = 10 * time.Millisecond
	for _, sess := range r3.sessions {
		sess.expires = time.Now().Add(-time.Millisecond)
	}
	r3.gc()
	if r3.active != 0 {
		t.Fatalf("active after full expiry = %d, want 0", r3.active)
	}
}
