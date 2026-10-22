package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// conditionalResponse is a raw GET result including headers, which the
// shared stack.request helper deliberately drops.
type conditionalResponse struct {
	status int
	body   []byte
	header http.Header
}

func (s *testStack) rawGet(path, token, ifNoneMatch string) conditionalResponse {
	s.t.Helper()
	request, err := http.NewRequest(http.MethodGet, s.server.URL+path, nil)
	if err != nil {
		s.t.Fatalf("request: %v", err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if ifNoneMatch != "" {
		request.Header.Set("If-None-Match", ifNoneMatch)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		s.t.Fatalf("do: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return conditionalResponse{status: response.StatusCode, body: body, header: response.Header.Clone()}
}

// seedHistoryThread appends `count` inbound messages to one conversation and
// a matching thread-list entry WITHOUT live events: pure history fixtures
// for the conditional GET tests (no push fan-out side effects).
func (f *fakeWorker) seedHistoryThread(threadKey, peer string, count int, newestUnixMilli int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, count)
	messages := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		id := fmt.Sprintf("%s_m%d", threadKey, index+1)
		createdAt := newestUnixMilli - int64(index)*1000
		message := map[string]any{
			"id": id, "gatewayID": "gw-" + f.LineID, "lineID": f.LineID + ":line",
			"threadKey": threadKey, "direction": "inbound", "peer": peer,
			"body": fmt.Sprintf("存档消息 %d/%d", index+1, count), "encoding": "gsm7",
			"status": "received", "createdAt": createdAt,
		}
		f.messages = append(f.messages, message)
		messages = append(messages, message)
		ids = append(ids, id)
	}
	last := messages[0] // newest first: ids[0] carries the highest createdAt
	replaced := false
	for index, existing := range f.threads {
		if existing["key"] == threadKey {
			f.threads[index] = map[string]any{"key": threadKey, "peer": peer, "unreadCount": count, "lastMessage": last}
			replaced = true
			break
		}
	}
	if !replaced {
		f.threads = append(f.threads, map[string]any{"key": threadKey, "peer": peer, "unreadCount": count, "lastMessage": last})
	}
	return ids
}

// emitMessageEvent replays a live message.created event for an already
// seeded message so the gateway indexes (message -> thread) for per-key read
// state, exactly like a reconnecting worker would.
func (f *fakeWorker) emitMessageEvent(threadKey, messageID, peer, body string, createdAt int64) {
	message := map[string]any{
		"id": messageID, "gatewayID": "gw-" + f.LineID, "lineID": f.LineID + ":line",
		"threadKey": threadKey, "direction": "inbound", "peer": peer,
		"body": body, "encoding": "gsm7", "status": "received", "createdAt": createdAt,
	}
	payload, _ := json.Marshal(map[string]any{
		"id": "evt_" + messageID, "seq": time.Now().UnixNano(), "type": "message.created",
		"createdAt": time.Now().Unix(), "data": message,
	})
	f.events <- payload
}

func strict(fakes ...*fakeWorker) {
	for _, fake := range fakes {
		fake.mu.Lock()
		fake.strictThreadMessages = true
		fake.mu.Unlock()
	}
}

func pairedStack(t *testing.T) (*testStack, string) {
	t.Helper()
	stack := newTestStack(t)
	strict(stack.fakes...)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "etag-phone")
	return stack, token
}

func decodeList(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var values []map[string]any
	if err := json.Unmarshal(body, &values); err != nil {
		t.Fatalf("body is not a JSON list (%d bytes): %v", len(body), err)
	}
	return values
}

// The core iOS revalidation flow: first authorized GET answers the full body
// with a strong tag; every unchanged repeat costs a zero-byte 304.
func TestThreadsConditionalGetSavesBodyBytes(t *testing.T) {
	stack, token := pairedStack(t)
	fake := stack.fakes[0]
	fake.seedHistoryThread("etag-thread", "+8613900001111", 3, time.Now().UnixMilli()-60_000)

	first := stack.rawGet("/api/v2/threads", token, "")
	if first.status != http.StatusOK {
		t.Fatalf("first GET = %d: %s", first.status, first.body)
	}
	etag := first.header.Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, `"sha256-`) || !strings.HasSuffix(etag, `"`) {
		t.Fatalf("strong content ETag missing: %q", etag)
	}
	if got := first.header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("conditional revalidation must not enable shared caching: %q", got)
	}
	if len(first.body) < 100 {
		t.Fatalf("fixture too small to prove byte savings: %d", len(first.body))
	}

	replay := stack.rawGet("/api/v2/threads", token, etag)
	if replay.status != http.StatusNotModified {
		t.Fatalf("unchanged repeat GET = %d, want 304", replay.status)
	}
	if len(replay.body) != 0 {
		t.Fatalf("304 must have an empty body, got %d bytes", len(replay.body))
	}
	if replay.header.Get("ETag") != etag {
		t.Fatalf("304 must echo the validator: %q vs %q", replay.header.Get("ETag"), etag)
	}
	t.Logf("bytes saved per unchanged reload: %d (200 body) -> 0 (304 body), ETag %s", len(first.body), etag)

	// Representation is byte-stable while nothing changed: independent 200s
	// without the header must match exactly and re-vouch the same tag.
	plain := stack.rawGet("/api/v2/threads", token, "")
	if plain.status != http.StatusOK || string(plain.body) != string(first.body) || plain.header.Get("ETag") != etag {
		t.Fatalf("stable representation violated")
	}

	// Old clients that never send If-None-Match keep the plain 200 contract.
	if got := plain.header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content type changed: %q", got)
	}

	// A new message changes the representation: the stale tag must NOT buy a
	// 304; the client gets the new body and a rotated tag.
	fake.seedHistoryThread("etag-thread-2", "+8613900002222", 1, time.Now().UnixMilli()-30_000)
	after := stack.rawGet("/api/v2/threads", token, etag)
	if after.status != http.StatusOK {
		t.Fatalf("stale tag after mutation must answer 200, got %d", after.status)
	}
	rotated := after.header.Get("ETag")
	if rotated == "" || rotated == etag {
		t.Fatalf("tag must rotate with the representation: %q", rotated)
	}
	if again := stack.rawGet("/api/v2/threads", token, rotated); again.status != http.StatusNotModified {
		t.Fatalf("fresh tag must 304, got %d", again.status)
	}
}

// The tag covers the exact query/page representation, never a global list.
func TestConditionalGetQueryAndPageAffectETag(t *testing.T) {
	stack, token := pairedStack(t)
	fake := stack.fakes[0]
	now := time.Now().UnixMilli() - 60_000
	ids := fake.seedHistoryThread("paged-thread", "+8613900003333", 3, now)

	all := stack.rawGet("/api/v2/threads", token, "")
	lineScoped := stack.rawGet("/api/v2/threads?line=line1", token, "")
	junk := stack.rawGet("/api/v2/threads?folder=junk", token, "")
	if lineScoped.header.Get("ETag") == "" || junk.header.Get("ETag") == "" {
		t.Fatal("filtered lists need their own validators")
	}
	// line1 is the seeded line; the unfiltered list also carries line2's
	// threads, so the representations (and tags) differ. junk filters the
	// normal folder out entirely.
	if lineScoped.header.Get("ETag") == all.header.Get("ETag") {
		t.Fatal("query affects the representation but not the tag")
	}
	if junk.header.Get("ETag") == all.header.Get("ETag") || junk.header.Get("ETag") == lineScoped.header.Get("ETag") {
		t.Fatal("folder filter must produce its own tag")
	}
	// Each variant revalidates against its own tag only.
	if got := stack.rawGet("/api/v2/threads?line=line1", token, lineScoped.header.Get("ETag")); got.status != http.StatusNotModified {
		t.Fatalf("line-scoped revalidation = %d, want 304", got.status)
	}
	if got := stack.rawGet("/api/v2/threads?line=line1", token, junk.header.Get("ETag")); got.status != http.StatusOK {
		t.Fatalf("cross-variant tag must not 304, got %d", got.status)
	}

	// Message pages: page 1 and page 2 are different representations with
	// different tags; each page 304s against its own tag.
	page1 := stack.rawGet("/api/v2/messages?threadKey=line1:paged-thread&limit=2", token, "")
	if page1.status != http.StatusOK {
		t.Fatalf("page1 = %d: %s", page1.status, page1.body)
	}
	messages := decodeList(t, page1.body)
	if len(messages) != 2 {
		t.Fatalf("limit=2 must page 2 messages, got %d", len(messages))
	}
	if got := page1.header.Get("X-CallRelay-Has-More"); got != "true" {
		t.Fatalf("page1 has-more = %q, want true", got)
	}
	etag1 := page1.header.Get("ETag")

	// A 304 keeps the content metadata a client needs to stay consistent.
	replay1 := stack.rawGet("/api/v2/messages?threadKey=line1:paged-thread&limit=2", token, etag1)
	if replay1.status != http.StatusNotModified || len(replay1.body) != 0 {
		t.Fatalf("page1 revalidation = %d (%d bytes)", replay1.status, len(replay1.body))
	}
	if got := replay1.header.Get("X-CallRelay-Has-More"); got != "true" {
		t.Fatalf("304 must preserve X-CallRelay-Has-More, got %q", got)
	}

	page2 := stack.rawGet("/api/v2/messages?threadKey=line1:paged-thread&limit=2&beforeId=line1:"+ids[1], token, "")
	if page2.status != http.StatusOK {
		t.Fatalf("page2 = %d", page2.status)
	}
	if page2.header.Get("ETag") == etag1 {
		t.Fatal("different page must not share the tag")
	}
	if got := page2.header.Get("X-CallRelay-Has-More"); got != "false" {
		t.Fatalf("page2 has-more = %q, want false", got)
	}
	if got := decodeList(t, page2.body); len(got) != 1 {
		t.Fatalf("page2 must hold the remaining message, got %d", len(got))
	}
	if stale := stack.rawGet("/api/v2/messages?threadKey=line1:paged-thread&limit=2", token, page2.header.Get("ETag")); stale.status != http.StatusOK {
		t.Fatal("page2 tag must not 304 the page1 representation")
	}
}

// Reads and per-key deletes change THIS key's representation and rotate the
// tag; a stale tag never masks the change.
func TestConditionalGetReadAndDeleteRotateETag(t *testing.T) {
	stack, token := pairedStack(t)
	fake := stack.fakes[0]
	now := time.Now().UnixMilli() - 60_000
	ids := fake.seedHistoryThread("watermark-thread", "+8613900004444", 2, now)
	fake.emitMessageEvent("watermark-thread", ids[0], "+8613900004444", "已读测试消息一", now)
	fake.emitMessageEvent("watermark-thread", ids[1], "+8613900004444", "已读测试消息二", now-1000)
	// Give the event pump a moment to index the messages (read state is
	// keyed off the live index).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, indexed := stack.store.MessageThread(context.Background(), "line1", ids[0]); indexed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	before := stack.rawGet("/api/v2/threads", token, "")
	beforeTag := before.header.Get("ETag")
	var beforeViews []map[string]any
	_ = json.Unmarshal(before.body, &beforeViews)
	unreadBefore := -1
	for _, view := range beforeViews {
		if view["key"] == "line1:watermark-thread" {
			unreadBefore = int(view["unreadCount"].(float64))
		}
	}
	if unreadBefore != 2 {
		t.Fatalf("fixture unread = %d, want 2", unreadBefore)
	}

	if status := stack.jsonRequest(http.MethodPost, "/api/v2/messages/line1:"+url.PathEscape(ids[0])+"/read", token, "read-etag-1", nil, nil); status != http.StatusNoContent {
		// ids[0] is the newest; the endpoint takes the global message id.
		t.Fatalf("read = %d", status)
	}
	// Poll until the watermark is visible (event indexing is async).
	var after conditionalResponse
	watermarked := false
	for attempt := 0; attempt < 60 && !watermarked; attempt++ {
		after = stack.rawGet("/api/v2/threads", token, "")
		var views []map[string]any
		_ = json.Unmarshal(after.body, &views)
		for _, view := range views {
			if view["key"] == "line1:watermark-thread" && int(view["unreadCount"].(float64)) != 2 {
				watermarked = true
			}
		}
		if !watermarked {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !watermarked {
		t.Fatal("read watermark never changed the threads representation")
	}
	if after.header.Get("ETag") == beforeTag {
		t.Fatal("a read must rotate the threads ETag (representation changed)")
	}
	if stale := stack.rawGet("/api/v2/threads", token, beforeTag); stale.status != http.StatusOK {
		t.Fatal("stale pre-read tag must not 304 the post-read representation")
	}
	if fresh := stack.rawGet("/api/v2/threads", token, after.header.Get("ETag")); fresh.status != http.StatusNotModified {
		t.Fatalf("post-read tag must revalidate, got %d", fresh.status)
	}

	// Per-key delete: the thread disappears for this key — new body, new tag.
	delStatus := stack.jsonRequest(http.MethodDelete, "/api/v2/threads/"+url.PathEscape("line1:watermark-thread"), token, "", nil, nil)
	if delStatus != http.StatusNoContent && delStatus != http.StatusOK {
		t.Fatalf("delete thread = %d", delStatus)
	}
	deleted := stack.rawGet("/api/v2/threads", token, after.header.Get("ETag"))
	if deleted.status != http.StatusOK {
		t.Fatalf("stale pre-delete tag must not 304 after a deletion, got %d", deleted.status)
	}
	for _, view := range decodeList(t, deleted.body) {
		if view["key"] == "line1:watermark-thread" {
			t.Fatal("deleted thread still listed")
		}
	}
	if again := stack.rawGet("/api/v2/threads", token, deleted.header.Get("ETag")); again.status != http.StatusNotModified {
		t.Fatalf("post-delete tag must revalidate, got %d", again.status)
	}
}

// active=true is the authoritative live-call filter: it must never be
// answered conditionally, so a just-changed call state cannot be masked.
func TestActiveCallsAreNeverConditional(t *testing.T) {
	stack, token := pairedStack(t)
	list := stack.rawGet("/api/v2/calls?limit=10", token, "")
	if list.status != http.StatusOK {
		t.Fatalf("calls list = %d", list.status)
	}
	tag := list.header.Get("ETag")
	if tag == "" {
		t.Fatal("non-active calls list must be revalidatable")
	}
	if again := stack.rawGet("/api/v2/calls?limit=10", token, tag); again.status != http.StatusNotModified {
		t.Fatalf("unchanged calls list = %d, want 304", again.status)
	}

	// A new inbound call rotates the non-active tag too (whole-history list).
	stack.fakes[0].injectIncoming("live-etag-1", "+8613900005555")
	call := stack.waitForCall(token, "line1:live-etag-1")
	if call == nil {
		t.Fatal("call never appeared")
	}
	changed := stack.rawGet("/api/v2/calls?limit=10", token, tag)
	if changed.status != http.StatusOK || changed.header.Get("ETag") == tag {
		t.Fatalf("new call must rotate the tag, got %d/%q", changed.status, changed.header.Get("ETag"))
	}

	// active=true: no ETag header at all, and even the exact current-list
	// tag (borrowed by the non-active call) must not buy a 304.
	active := stack.rawGet("/api/v2/calls?active=true", token, "")
	if active.status != http.StatusOK {
		t.Fatalf("active list = %d", active.status)
	}
	if active.header.Get("ETag") != "" {
		t.Fatal("active=true must never advertise a validator")
	}
	// Feed the active body's own bytes back as a candidate tag: the handler
	// ignores If-None-Match there, so the answer stays 200.
	borrowed := strings.TrimSuffix(strings.TrimPrefix(changed.header.Get("ETag"), `"`), `"`)
	if forced := stack.rawGet("/api/v2/calls?active=true", token, `"`+borrowed+`"`); forced.status != http.StatusOK {
		t.Fatal("active=true must never answer 304")
	}
	if forced := stack.rawGet("/api/v2/calls?active=true", token, "*"); forced.status != http.StatusOK {
		t.Fatal("active=true must ignore wildcard conditional")
	}
}

func TestConditionalGetUnauthenticatedNeverBypasses(t *testing.T) {
	stack, token := pairedStack(t)
	granted := stack.rawGet("/api/v2/threads", token, "")
	tag := granted.header.Get("ETag")
	if tag == "" {
		t.Fatal("fixture missing ETag")
	}
	anonymous := stack.rawGet("/api/v2/threads", "", tag)
	if anonymous.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated If-None-Match = %d, want 401 (auth runs BEFORE conditional handling)", anonymous.status)
	}
	if anonymous.status == http.StatusNotModified {
		t.Fatal("304 bypass")
	}
	if anonymous.header.Get("ETag") != "" {
		t.Fatal("error response must not carry a validator")
	}
	if wildcard := stack.rawGet("/api/v2/messages?threadKey=line1:whatever", "", "*"); wildcard.status != http.StatusUnauthorized {
		t.Fatalf("wildcard anonymous probe = %d, want 401", wildcard.status)
	}
	// A revoked/unknown token behaves identically.
	if bogus := stack.rawGet("/api/v2/threads", "Bearer not-a-real-token", tag); bogus.status != http.StatusUnauthorized {
		t.Fatalf("bogus token = %d, want 401", bogus.status)
	}
}

func TestConditionalGetCrossKeyCannotBorrowTag(t *testing.T) {
	stack := newTestStack(t)
	strict(stack.fakes...)
	// Key A: all lines. Key B: line1 only (createKey(false) grants line1
	// explicitly; line2 stays ungranted).
	keyA, secretA := stack.createKey(true)
	tokenA, _ := stack.enroll(keyA, secretA, "etag-key-a")
	keyB, secretB := stack.createKey(false)
	tokenB, _ := stack.enroll(keyB, secretB, "etag-key-b")

	stack.fakes[1].seedHistoryThread("line2-secret-thread", "+8613900006666", 2, time.Now().UnixMilli()-60_000)
	a200 := stack.rawGet("/api/v2/messages?threadKey=line2:line2-secret-thread&limit=50", tokenA, "")
	if a200.status != http.StatusOK || len(decodeList(t, a200.body)) != 2 {
		t.Fatalf("authorized page1 = %d", a200.status)
	}
	tagA := a200.header.Get("ETag")

	// Device B has no line2 grant: it must get its own authorization error,
	// never a 304 borrowed from device A's identical-looking request.
	b := stack.rawGet("/api/v2/messages?threadKey=line2:line2-secret-thread&limit=50", tokenB, tagA)
	if b.status == http.StatusNotModified {
		t.Fatal("unauthorized device must never be short-circuited by another device's ETag")
	}
	if b.status == http.StatusOK {
		// line2 content must not leak: an empty-for-B representation is fine,
		// the tag must differ and the body must not contain the messages.
		if b.header.Get("ETag") == tagA {
			t.Fatal("B's representation must not share A's tag")
		}
		if strings.Contains(string(b.body), "line2-secret-thread") || strings.Contains(string(b.body), "存档消息") {
			t.Fatal("history leaked across keys")
		}
	}
	// threads for line1: both keys see the same seeded data but the tags
	// must still round-trip per representation.
	l1a := stack.rawGet("/api/v2/threads?line=line1", tokenA, "")
	l1b := stack.rawGet("/api/v2/threads?line=line1", tokenB, l1a.header.Get("ETag"))
	if l1a.status != http.StatusOK {
		t.Fatalf("A threads = %d", l1a.status)
	}
	// B shares line1's grant scope, so a 304 here is the CORRECT answer;
	// proving tag semantics are per-representation, not per-device secret.
	if l1b.status != http.StatusNotModified && l1b.status != http.StatusOK {
		t.Fatalf("B line1 threads = %d", l1b.status)
	}
}

func TestConditionalGetValidatorSyntaxTolerance(t *testing.T) {
	stack, token := pairedStack(t)
	first := stack.rawGet("/api/v2/threads", token, "")
	tag := first.header.Get("ETag")

	if got := stack.rawGet("/api/v2/threads", token, "*"); got.status != http.StatusNotModified {
		t.Fatalf("wildcard IF-None-Match with a validator present = %d, want 304", got.status)
	}
	if got := stack.rawGet("/api/v2/threads", token, "W/"+tag); got.status != http.StatusNotModified {
		t.Fatalf("weak-tag echo = %d, want 304 (tolerated)", got.status)
	}
	if got := stack.rawGet("/api/v2/threads", token, `"other-tag", `+tag); got.status != http.StatusNotModified {
		t.Fatalf("comma-list = %d, want 304", got.status)
	}
	if got := stack.rawGet("/api/v2/threads", token, "sha256-unquoted"); got.status != http.StatusOK {
		t.Fatalf("non-matching tag must answer 200, got %d", got.status)
	}
}

func TestNonHistoryEndpointsAreNeverConditional(t *testing.T) {
	stack, token := pairedStack(t)
	// Admin/auth/key/ICE-adjacent endpoints must not gain ETags or 304
	// handling: a stale validator must not hide rotated credentials or ICE.
	for _, path := range []string{
		"/api/v2/notifications/status",
		"/api/v2/contacts",
		"/api/v2/lines",
		"/api/v2/harassment/lists",
	} {
		response := stack.rawGet(path, token, "")
		if response.status == http.StatusNotModified {
			t.Fatalf("%s answered 304 without any validator", path)
		}
		if response.header.Get("ETag") != "" {
			t.Fatalf("%s must not advertise an ETag: %q", path, response.header.Get("ETag"))
		}
		starred := stack.rawGet(path, token, "*")
		if starred.status == http.StatusNotModified || starred.header.Get("ETag") != "" {
			t.Fatalf("%s gained conditional semantics (status=%d etag=%q)", path, starred.status, starred.header.Get("ETag"))
		}
	}
}
