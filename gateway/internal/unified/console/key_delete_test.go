package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"callrelay/gateway/internal/unified/store"
)

// Deleting is a separate, CSRF-protected action that only removes keys that
// are already revoked. Active keys stay untouched; the page renders the
// button only on revoked rows.
func TestKeyDeleteOnlyRemovesRevokedKeys(t *testing.T) {
	env := newConsoleTestEnv(t)
	ctx := context.Background()

	activeID, _, err := env.auth.CreateKey(ctx, "仍在使用", true)
	if err != nil {
		t.Fatalf("create active key: %v", err)
	}
	revokedID, _, err := env.auth.CreateKey(ctx, "历史密钥", false)
	if err != nil {
		t.Fatalf("create revoked key: %v", err)
	}
	if err := env.auth.RevokeKey(ctx, revokedID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Active key: deletion is refused and the key keeps authenticating.
	recorder := env.post("/console/keys/"+activeID+"/delete", url.Values{"csrf": {testCSRF}})
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("active delete status = %d, want 303", recorder.Code)
	}
	if _, _, err := env.store.GetEnrollmentKey(ctx, activeID); err != nil {
		t.Fatalf("active key must survive delete attempt: %v", err)
	}

	// Revoked key: deletion succeeds and the secret/grants are gone.
	recorder = env.post("/console/keys/"+revokedID+"/delete", url.Values{"csrf": {testCSRF}})
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("revoked delete status = %d, want 303", recorder.Code)
	}
	if _, _, err := env.store.GetEnrollmentKey(ctx, revokedID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted key lookup = %v, want ErrNotFound", err)
	}
	audits, err := env.store.ListAudit(ctx, 50)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	found := false
	for _, entry := range audits {
		if entry.Action == "key.delete" && entry.Target == revokedID {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing key.delete audit row")
	}

	// Missing CSRF must never delete anything.
	second, _, err := env.auth.CreateKey(ctx, "另一个历史密钥", false)
	if err != nil {
		t.Fatalf("create second revoked key: %v", err)
	}
	if err := env.auth.RevokeKey(ctx, second); err != nil {
		t.Fatalf("revoke second: %v", err)
	}
	recorder = env.post("/console/keys/"+second+"/delete", nil)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("csrf-less delete status = %d, want 403", recorder.Code)
	}
	if _, _, err := env.store.GetEnrollmentKey(ctx, second); err != nil {
		t.Fatalf("csrf-less delete removed the key: %v", err)
	}

	// The rendered page exposes the action on revoked rows only.
	page := env.get("/console/keys")
	if page.Code != http.StatusOK {
		t.Fatalf("keys page status = %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, `action="/console/keys/`+second+`/delete"`) {
		t.Fatalf("revoked key row is missing its delete action")
	}
	if strings.Contains(body, `action="/console/keys/`+activeID+`/delete"`) {
		t.Fatalf("active key row must not render a delete action")
	}
	if !strings.Contains(body, "删除密钥") {
		t.Fatalf("expected the delete affordance label on the keys page")
	}
}
