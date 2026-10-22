package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"callrelay/gateway/internal/unified/contacts"
	"callrelay/gateway/internal/unified/notify"
)

func attachContacts(stack *testStack) {
	stack.api.Contacts = contacts.NewService(stack.store)
}

// apiJSON issues a token-authenticated JSON request and returns the status
// plus the raw body (decoded into out when provided).
func (s *testStack) apiJSON(method, path, token string, value any, out any) (int, []byte) {
	s.t.Helper()
	var body []byte
	if value != nil {
		body, _ = json.Marshal(value)
	}
	status, payload := s.request(method, path, token, "", body)
	if out != nil && len(payload) > 0 {
		_ = json.Unmarshal(payload, out)
	}
	return status, payload
}

func TestContactsAPIPrincipalIsolation(t *testing.T) {
	stack := newTestStack(t)
	attachContacts(stack)
	keyA, secretA := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "device-a")
	deviceA := stack.deviceID
	keyB, secretB := stack.createKey(true)
	tokenB, _ := stack.enroll(keyB, secretB, "device-b")
	deviceB := stack.deviceID
	if deviceA == deviceB {
		t.Fatal("test devices must differ")
	}

	cardA := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:甲方联系人\r\nTEL;TYPE=CELL:+86 138 0000 0001\r\nEND:VCARD\r\n"

	// Preview must not persist.
	var preview struct {
		Preview bool `json:"preview"`
		Created int  `json:"created"`
	}
	status, _ := stack.apiJSON(http.MethodPost, "/api/v2/contacts/import", tokenA,
		map[string]any{"vcard": cardA, "preview": true}, &preview)
	if status != http.StatusOK || !preview.Preview || preview.Created != 1 {
		t.Fatalf("preview: %d %+v", status, preview)
	}
	var list struct {
		Contacts []map[string]any `json:"contacts"`
		Revision int64            `json:"revision"`
		HasMore  bool             `json:"hasMore"`
	}
	status, _ = stack.apiJSON(http.MethodGet, "/api/v2/contacts", tokenA, nil, &list)
	if status != http.StatusOK || len(list.Contacts) != 0 {
		t.Fatalf("preview must not persist: %d %+v", status, list)
	}

	// Commit.
	var committed struct {
		Created int `json:"created"`
		Entries []struct {
			ContactID string `json:"contactId"`
			Action    string `json:"action"`
		} `json:"entries"`
	}
	status, _ = stack.apiJSON(http.MethodPost, "/api/v2/contacts/import", tokenA,
		map[string]any{"vcard": cardA}, &committed)
	if status != http.StatusOK || committed.Created != 1 || len(committed.Entries) != 1 || committed.Entries[0].ContactID == "" {
		t.Fatalf("commit: %d %+v", status, committed)
	}
	contactA := committed.Entries[0].ContactID

	// Owner reads it; another principal sees nothing by list or search.
	status, _ = stack.apiJSON(http.MethodGet, "/api/v2/contacts", tokenA, nil, &list)
	if status != http.StatusOK || len(list.Contacts) != 1 {
		t.Fatalf("owner list: %d %+v", status, list)
	}
	status, _ = stack.apiJSON(http.MethodGet, "/api/v2/contacts", tokenB, nil, &list)
	if status != http.StatusOK || len(list.Contacts) != 0 {
		t.Fatalf("cross-principal list leaked: %d %+v", status, list)
	}
	for _, query := range []string{"%E7%94%B2%E6%96%B9", "13800000001"} {
		status, _ = stack.apiJSON(http.MethodGet, "/api/v2/contacts?query="+query, tokenB, nil, &list)
		if status != http.StatusOK || len(list.Contacts) != 0 {
			t.Fatalf("cross-principal search %q leaked: %d %+v", query, status, list)
		}
	}

	// Cross-principal edit/delete must 404 and must not damage A's contact.
	status, _ = stack.apiJSON(http.MethodPut, "/api/v2/contacts/"+contactA, tokenB,
		map[string]any{"displayName": "被篡改"}, nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-principal edit status: %d", status)
	}
	status, _ = stack.apiJSON(http.MethodDelete, "/api/v2/contacts/"+contactA, tokenB, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-principal delete status: %d", status)
	}
	status, _ = stack.apiJSON(http.MethodGet, "/api/v2/contacts", tokenA, nil, &list)
	if status != http.StatusOK || len(list.Contacts) != 1 || list.Contacts[0]["displayName"] != "甲方联系人" {
		t.Fatalf("owner contact damaged: %d %+v", status, list)
	}

	// Cross-principal export must not contain A's data.
	status, exportBody := stack.request(http.MethodGet, "/api/v2/contacts/export", tokenB, "", nil)
	if status != http.StatusOK || strings.Contains(string(exportBody), "13800000001") || strings.Contains(string(exportBody), "甲方联系人") {
		t.Fatalf("cross-principal export leaked: %d %s", status, exportBody)
	}

	// Owner edit works.
	status, _ = stack.apiJSON(http.MethodPut, "/api/v2/contacts/"+contactA, tokenA,
		map[string]any{"displayName": "甲方联系人改名"}, nil)
	if status != http.StatusOK {
		t.Fatalf("owner edit: %d", status)
	}
	status, exportBody = stack.request(http.MethodGet, "/api/v2/contacts/export", tokenA, "", nil)
	if status != http.StatusOK || !strings.Contains(string(exportBody), "甲方联系人改名") ||
		!strings.Contains(string(exportBody), "138 0000 0001") {
		t.Fatalf("owner export: %d %s", status, exportBody)
	}
	status, _ = stack.apiJSON(http.MethodDelete, "/api/v2/contacts/"+contactA, tokenA, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("owner delete status: %d", status)
	}
	status, _ = stack.apiJSON(http.MethodGet, "/api/v2/contacts", tokenA, nil, &list)
	if status != http.StatusOK || len(list.Contacts) != 0 {
		t.Fatalf("delete did not take effect: %d %+v", status, list)
	}
}

func TestContactsAPISyncIdempotentAndTombstones(t *testing.T) {
	stack := newTestStack(t)
	attachContacts(stack)
	keyA, secretA := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "device-a")
	keyB, secretB := stack.createKey(true)
	tokenB, _ := stack.enroll(keyB, secretB, "device-b")

	batch := map[string]any{"contacts": []map[string]any{{
		"clientRef":   "ref-api-1",
		"displayName": "张三",
		"searchKey":   "zhangsan 9426",
		"phones":      []map[string]string{{"label": "CELL", "value": "+86 138 0000 0002"}},
	}}}
	var syncResult struct {
		Created  int   `json:"created"`
		Revision int64 `json:"revision"`
	}
	status, _ := stack.apiJSON(http.MethodPost, "/api/v2/contacts/sync", tokenA, batch, &syncResult)
	if status != http.StatusOK || syncResult.Created != 1 || syncResult.Revision == 0 {
		t.Fatalf("sync: %d %+v", status, syncResult)
	}
	status, body := stack.apiJSON(http.MethodPost, "/api/v2/contacts/sync", tokenA, batch, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"unchanged":1`) {
		t.Fatalf("repeated sync must merge: %d %s", status, body)
	}
	// Same phone under another principal is a separate contact.
	status, _ = stack.apiJSON(http.MethodPost, "/api/v2/contacts/sync", tokenB, batch, &syncResult)
	if status != http.StatusOK || syncResult.Created != 1 {
		t.Fatalf("second principal sync: %d %+v", status, syncResult)
	}

	// Incremental pull returns the contact, then the tombstone after delete.
	var page struct {
		Contacts  []map[string]any `json:"contacts"`
		Revision  int64            `json:"revision"`
		NextSince int64            `json:"nextSince"`
		HasMore   bool             `json:"hasMore"`
	}
	status, _ = stack.apiJSON(http.MethodGet, "/api/v2/contacts/sync?since=0", tokenA, nil, &page)
	if status != http.StatusOK || len(page.Contacts) != 1 || page.HasMore {
		t.Fatalf("pull: %d %+v", status, page)
	}
	contactID, _ := page.Contacts[0]["id"].(string)
	status, _ = stack.apiJSON(http.MethodDelete, "/api/v2/contacts/"+contactID, tokenA, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("delete: %d", status)
	}
	status, _ = stack.apiJSON(http.MethodGet, "/api/v2/contacts/sync?since=1", tokenA, nil, &page)
	if status != http.StatusOK || len(page.Contacts) != 1 {
		t.Fatalf("tombstone pull: %d %+v", status, page)
	}
	if deleted, _ := page.Contacts[0]["deleted"].(bool); !deleted {
		t.Fatalf("expected a deletion tombstone: %+v", page.Contacts[0])
	}
	// The stale device re-syncs the same client ref; it must not come back.
	status, body = stack.apiJSON(http.MethodPost, "/api/v2/contacts/sync", tokenA, batch, nil)
	if status != http.StatusOK || !strings.Contains(string(body), `"skipped":1`) {
		t.Fatalf("resurrection guard: %d %s", status, body)
	}
}

func TestContactsAPIRequiresAuthentication(t *testing.T) {
	stack := newTestStack(t)
	attachContacts(stack)
	for _, path := range []string{"/api/v2/contacts", "/api/v2/contacts/export", "/api/v2/contacts/sync"} {
		status, _ := stack.request(http.MethodGet, path, "", "", nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("%s without token: %d", path, status)
		}
	}
	status, _ := stack.request(http.MethodPost, "/api/v2/contacts/import", "", "", []byte(`{"vcard":"x"}`))
	if status != http.StatusUnauthorized {
		t.Fatalf("import without token: %d", status)
	}
}

func TestContactsAPIBrowserClientSession(t *testing.T) {
	stack := newTestStack(t)
	attachContacts(stack)
	key, secret := stack.createKey(true)
	token, _ := stack.enroll(key, secret, "device-web")
	stack.attachWebPush(t)
	code := stack.mintBindCode(t, token, "client")
	_, cookie, scope, _ := stack.redeemBind(t, code)
	if scope != "client" {
		t.Fatalf("scope: %s", scope)
	}
	status, body := stack.cookieRequest(t, http.MethodGet, "/api/v2/contacts", cookie, stack.server.URL, nil)
	if status != http.StatusOK {
		t.Fatalf("client cookie list: %d %s", status, body)
	}
	card := "BEGIN:VCARD\nVERSION:3.0\nFN:网页导入\nTEL:13700000001\nEND:VCARD\n"
	status, body = stack.cookieRequest(t, http.MethodPost, "/api/v2/contacts/import", cookie, stack.server.URL,
		map[string]any{"vcard": card})
	if status != http.StatusOK || !strings.Contains(string(body), `"created":1`) {
		t.Fatalf("client cookie import: %d %s", status, body)
	}
}

func TestNotificationsStatusLineACL(t *testing.T) {
	stack := newTestStack(t)
	monitor := notify.New(stack.store, nil, notify.Config{Enabled: true, WanLineID: "line2"})
	monitor.RunProbe = func(context.Context) (notify.ProbeResult, string) {
		return notify.ProbeUnknown, ""
	}
	_ = monitor.RunOnce(context.Background())
	stack.api.Notify = monitor

	keyAll, secretAll := stack.createKey(true)
	tokenAll, _ := stack.enroll(keyAll, secretAll, "device-all")
	keyScoped, secretScoped := stack.scopedKey(t, "line1")
	tokenScoped, _ := stack.enroll(keyScoped, secretScoped, "device-scoped")

	var full notify.Status
	status, _ := stack.apiJSON(http.MethodGet, "/api/v2/notifications/status", tokenAll, nil, &full)
	if status != http.StatusOK || full.LineName != "line2" {
		t.Fatalf("authorized status: %d %+v", status, full)
	}
	var scoped notify.Status
	status, _ = stack.apiJSON(http.MethodGet, "/api/v2/notifications/status", tokenScoped, nil, &scoped)
	if status != http.StatusOK {
		t.Fatalf("scoped status: %d", status)
	}
	if scoped.LineName != "" || scoped.LineTail != "" {
		t.Fatalf("unauthorized device must not see line details: %+v", scoped)
	}
}
