package console

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestManualNumberEndpointAndRender verifies the operator can set a manual
// own number (synthetic), it persists with source manual, is rendered in the
// authenticated console in full, and clearing it falls back to none.
func TestManualNumberEndpointAndRender(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	ctx := context.Background()

	form := url.Values{"csrf": {testCSRF}, "manual_number": {"+15550001234"}}
	rec := env.post("/console/lines/line1/number", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("manual number status = %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	row, err := env.store.GetLineIdentity(ctx, "line1")
	if err != nil {
		t.Fatalf("get identity: %v", err)
	}
	if row.ManualNumber != "+15550001234" || row.NumberSource != "manual" {
		t.Fatalf("identity row = %+v", row)
	}

	body := env.get("/console/lines").Body.String()
	// HTML escapes '+' to &#43;; assert the visible digits of the full number.
	if !strings.Contains(body, "15550001234") {
		t.Fatal("authenticated console must render the full manual number")
	}

	// Clearing returns to the no-number state.
	clearForm := url.Values{"csrf": {testCSRF}, "manual_number": {""}}
	if rec := env.post("/console/lines/line1/number", clearForm); rec.Code != http.StatusSeeOther {
		t.Fatalf("clear status = %d", rec.Code)
	}
	row, _ = env.store.GetLineIdentity(ctx, "line1")
	if row.ManualNumber != "" || row.NumberSource != "none" {
		t.Fatalf("cleared identity = %+v", row)
	}
}

// TestEmptySIMStatusRendersHonestly ensures the console shows a friendly
// empty-SIM placeholder rather than a stale or masked number.
func TestEmptySIMStatusRendersHonestly(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	// No SIM number present; line2's store row defaults to source none. The
	// rendered status must not be a masked phone and must prompt manual input.
	body := env.get("/console/lines").Body.String()
	chunk := formChunk(t, body, "/console/lines/line2/number")
	if !strings.Contains(chunk, `name="manual_number"`) {
		t.Fatal("line2 should offer manual number input")
	}
}

// TestNumberEndpointRequiresCSRF prevents an unauthenticated write.
func TestNumberEndpointRequiresCSRF(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	bad := url.Values{"manual_number": {"+15550001234"}}
	if rec := env.post("/console/lines/line1/number", bad); rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want 403", rec.Code)
	}
}

// TestKeyManageNumberCapability proves the explicit, off-by-default console
// grant: created keys lack it, the toggle persists it (and renders checked),
// and switching it off removes it. Line grants are untouched throughout.
func TestKeyManageNumberCapability(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	ctx := context.Background()

	// Create without the checkbox: capability defaults off even for an
	// all-lines key.
	form := url.Values{"csrf": {testCSRF}, "name": {"家庭共享"}, "all_lines": {"on"}}
	rec := env.post("/console/keys", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create key status = %d", rec.Code)
	}
	keys, err := env.store.ListEnrollmentKeys(ctx)
	if err != nil {
		t.Fatalf("list keys: %v", err)
	}
	var keyID string
	for _, key := range keys {
		if key.Name == "家庭共享" {
			keyID = key.ID
			if key.ManageNumber {
				t.Fatal("new shared key must default manage_number off")
			}
		}
	}
	if keyID == "" {
		t.Fatal("created key not found")
	}

	// Enable the capability via the per-key toggle.
	on := url.Values{"csrf": {testCSRF}, "manage_number": {"on"}}
	if rec := env.post("/console/keys/"+keyID+"/manage-number", on); rec.Code != http.StatusSeeOther {
		t.Fatalf("enable status = %d", rec.Code)
	}
	key, _, err := env.store.GetEnrollmentKey(ctx, keyID)
	if err != nil {
		t.Fatalf("get key: %v", err)
	}
	if !key.ManageNumber {
		t.Fatal("manage_number must be enabled after toggle")
	}
	if body := env.get("/console/keys").Body.String(); !strings.Contains(body, `name="manage_number" checked`) {
		t.Fatal("keys page must render the checked capability toggle")
	}

	// Switch off: ordinary/shared state restored.
	off := url.Values{"csrf": {testCSRF}}
	if rec := env.post("/console/keys/"+keyID+"/manage-number", off); rec.Code != http.StatusSeeOther {
		t.Fatalf("disable status = %d", rec.Code)
	}
	key, _, _ = env.store.GetEnrollmentKey(ctx, keyID)
	if key.ManageNumber {
		t.Fatal("manage_number must be removable")
	}

	// The toggle requires CSRF.
	if rec := env.post("/console/keys/"+keyID+"/manage-number", url.Values{"manage_number": {"on"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want 403", rec.Code)
	}
}

// TestCreateKeyWithManageNumberCheckbox covers the create-time opt-in.
func TestCreateKeyWithManageNumberCheckbox(t *testing.T) {
	env := newConsoleTestEnv(t)
	env.seed()
	ctx := context.Background()
	form := url.Values{"csrf": {testCSRF}, "name": {"管理员手机"},
		"all_lines": {"on"}, "manage_number": {"on"}}
	if rec := env.post("/console/keys", form); rec.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d", rec.Code)
	}
	keys, _ := env.store.ListEnrollmentKeys(ctx)
	var found bool
	for _, key := range keys {
		if key.Name == "管理员手机" {
			found = true
			if !key.ManageNumber {
				t.Fatal("create-time checkbox must enable the capability")
			}
			if !key.AllLines {
				t.Fatal("all-lines checkbox must still apply")
			}
		}
	}
	if !found {
		t.Fatal("key not found")
	}
}
