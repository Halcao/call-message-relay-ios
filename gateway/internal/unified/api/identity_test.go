package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

// getLines fetches authenticated line views for an enrolled all-lines device.
func (s *testStack) getLines(t *testing.T, token string) []map[string]any {
	t.Helper()
	status, payload := s.request(http.MethodGet, "/api/v2/lines", token, "", nil)
	if status != http.StatusOK {
		t.Fatalf("lines status %d: %s", status, payload)
	}
	var lines []map[string]any
	if err := json.Unmarshal(payload, &lines); err != nil {
		t.Fatalf("decode lines: %v", err)
	}
	return lines
}

func findLine(lines []map[string]any, id string) map[string]any {
	for _, line := range lines {
		if line["id"] == id {
			return line
		}
	}
	return nil
}

// waitForLine polls the authenticated line views until cond passes, bounded
// to timeout (identity converges asynchronously after the worker AT probe;
// the eventual value asserted below is UNCHANGED — only the read is given
// its bounded window). Failures print the last observed view.
func (s *testStack) waitForLine(t *testing.T, token, lineID string, cond func(map[string]any) bool, why string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var line map[string]any
	for {
		for _, candidate := range s.getLines(t, token) {
			if candidate["id"] == lineID {
				line = candidate
			}
		}
		if line != nil && cond(line) {
			return line
		}
		if time.Now().After(deadline) {
			t.Fatalf("line %s did not converge within %s: %s (last view %#v)", lineID, timeout, why, line)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestSIMOwnNumberEndToEnd verifies the full lifecycle using only synthetic
// identifiers: SIM-derived number reaches the authenticated API with source
// "sim"; line2's empty record is honest; a SIM change invalidates the stale
// number; a manual override wins; and a stale preparation-file mask never
// leaks after the SIM goes empty.
func TestSIMOwnNumberEndToEnd(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "phone")

	// line1 reports a synthetic SIM own number (same shape as the real
	// module: empty unquoted alpha, explicit '+', TOA 145 handled by worker).
	stack.fakes[0].setSIMIdentity(map[string]any{
		"number":          "+15550001111",
		"numberSource":    "sim",
		"typeOfAddress":   145,
		"iccid":           "0123456789ABCDEF0123",
		"imsi":            "001012000011111",
		"operatorAlpha":   "Test Operator",
		"operatorNumeric": "00101",
		"registration":    "registered_home",
		"accessTech":      "lte",
		"cellId":          "000486A9",
		"tac":             "04D2",
		"pci":             123,
		"earfcn":          1650,
		"commands":        map[string]string{"AT+CNUM": "ok", "AT+CCID": "ok", "AT+CIMI": "ok"},
	})
	// line2 honestly reports an empty SIM record.
	stack.fakes[1].setSIMIdentity(map[string]any{
		"number": "", "numberSource": "empty", "iccid": "0123456789ABCDEF0124",
		"commands": map[string]string{"AT+CNUM": "empty"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := stack.manager.RefreshLineIdentity(ctx, "line1"); err != nil {
		t.Fatalf("refresh line1: %v", err)
	}
	if _, err := stack.manager.RefreshLineIdentity(ctx, "line2"); err != nil {
		t.Fatalf("refresh line2: %v", err)
	}

	line1 := stack.waitForLine(t, token, "line1", func(l map[string]any) bool {
		id, _ := l["identity"].(map[string]any)
		return l["phoneNumber"] == "+15550001111" && id["numberSource"] == "sim"
	}, "SIM own number must reach the authorized line view", 5*time.Second)
	line2 := stack.waitForLine(t, token, "line2", func(l map[string]any) bool {
		id, _ := l["identity"].(map[string]any)
		_, hasID := l["identity"]
		return hasID && id["numberSource"] == "empty"
	}, "empty SIM record must converge", 5*time.Second)

	// Authenticated, line-authorized device sees the FULL own number ...
	if got := line1["phoneNumber"]; got != "+15550001111" {
		t.Fatalf("line1 full number = %v, want +15550001111", got)
	}
	identity1, _ := line1["identity"].(map[string]any)
	if identity1["numberSource"] != "sim" {
		t.Fatalf("line1 source = %v, want sim", identity1["numberSource"])
	}
	// ... plus the compatibility masked field ...
	if identity1["phoneMasked"] != "155****1111" {
		t.Fatalf("line1 masked = %v, want 155****1111", identity1["phoneMasked"])
	}
	// ... while raw subscriber/cell identifiers never reach the app view.
	for _, leaked := range []string{"iccid", "imsi", "cellId", "tac", "lac"} {
		if _, present := identity1[leaked]; present {
			t.Fatalf("identity must not expose %s", leaked)
		}
	}
	if _, present := line1["iccid"]; present {
		t.Fatalf("line view must not expose raw iccid")
	}

	// line2 empty SIM record: no number, honest source, no stale value.
	if got, _ := line2["phoneNumber"].(string); got != "" {
		t.Fatalf("line2 number = %v, want empty", got)
	}
	identity2, _ := line2["identity"].(map[string]any)
	if identity2["numberSource"] != "empty" {
		t.Fatalf("line2 source = %v, want empty", identity2["numberSource"])
	}
	if identity2["phoneMasked"] != "" {
		t.Fatalf("line2 masked = %v, want empty", identity2["phoneMasked"])
	}

	// SIM swap: a changed ICCID invalidates the stale auto number.
	stack.fakes[0].setSIMIdentity(map[string]any{
		"number": "+15550009999", "numberSource": "sim", "iccid": "0123456789ABCDEF0999",
	})
	if _, err := stack.manager.RefreshLineIdentity(ctx, "line1"); err != nil {
		t.Fatalf("refresh after swap: %v", err)
	}
	line1 = stack.waitForLine(t, token, "line1", func(l map[string]any) bool {
		if got, _ := l["phoneNumber"].(string); got != "" {
			return false
		}
		id, _ := l["identity"].(map[string]any)
		return id["numberSource"] == "sim_changed"
	}, "SIM swap must invalidate the stale auto number", 5*time.Second)
	if got, _ := line1["phoneNumber"].(string); got != "" {
		t.Fatalf("after SIM swap number must be invalidated, got %v", got)
	}
	identity1, _ = line1["identity"].(map[string]any)
	if identity1["numberSource"] != "sim_changed" {
		t.Fatalf("after swap source = %v, want sim_changed", identity1["numberSource"])
	}

	// Manual override wins over the pending SIM number.
	if err := stack.manager.SetManualNumber(ctx, "line1", "13800138000"); err != nil {
		t.Fatalf("set manual: %v", err)
	}
	line1 = stack.waitForLine(t, token, "line1", func(l map[string]any) bool {
		return l["phoneNumber"] == "13800138000"
	}, "manual override must converge", 5*time.Second)
	if got := line1["phoneNumber"]; got != "13800138000" {
		t.Fatalf("manual number = %v, want 13800138000", got)
	}
	identity1, _ = line1["identity"].(map[string]any)
	if identity1["numberSource"] != "manual" {
		t.Fatalf("manual source = %v, want manual", identity1["numberSource"])
	}
}

// TestStaleIdentityFileMaskCleared ensures a phoneMasked left in the
// preparation identity file for a line whose SIM is now empty is replaced,
// not inherited.
func TestStaleIdentityFileMaskCleared(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "phone2")

	// Write the stale preparation identity content to the line's EXISTING
	// identity path via atomic rename (readers see the old or new file, never
	// a partial write). Repointing Line.Cfg from a test would race the
	// production poller, which reads it lock-free by design.
	line1 := stack.manager.Line("line1")
	if line1 == nil {
		t.Fatalf("line1 not running")
	}
	stalePath := line1.Cfg.IdentityFile
	stale := `{"lineId":"line1","phoneMasked":"199****9999","numberSource":"sim"}`
	tmp := stalePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(stale), 0o600); err != nil {
		t.Fatalf("write stale identity: %v", err)
	}
	if err := os.Rename(tmp, stalePath); err != nil {
		t.Fatalf("rename stale identity: %v", err)
	}
	stack.fakes[0].setSIMIdentity(map[string]any{
		"number": "", "numberSource": "empty", "iccid": "0123456789ABCDEF0123",
		"commands": map[string]string{"AT+CNUM": "empty"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := stack.manager.RefreshLineIdentity(ctx, "line1"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	line := findLine(stack.getLines(t, token), "line1")
	identity, _ := line["identity"].(map[string]any)
	if identity["phoneMasked"] != "" {
		t.Fatalf("stale file mask leaked: %v", identity["phoneMasked"])
	}
	if identity["numberSource"] != "empty" {
		t.Fatalf("source = %v, want empty", identity["numberSource"])
	}
	if got, _ := line["phoneNumber"].(string); got != "" {
		t.Fatalf("full number must be empty, got %v", line["phoneNumber"])
	}
}
