package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateBarkServerURL(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		allowPrivate bool
		want         string
		wantErr      bool
		disallowed   bool
	}{
		{name: "public https", raw: "https://api.day.app", want: "https://api.day.app"},
		{name: "public https with path", raw: "https://bark.example.com/bark/", want: "https://bark.example.com/bark"},
		{name: "http public rejected without opt-in", raw: "http://bark.example.com", disallowed: true},
		{name: "http public hostname allowed with LAN opt-in (dial pins private)", raw: "http://bark.example.com", allowPrivate: true, want: "http://bark.example.com"},
		{name: "http public ip rejected even with opt-in", raw: "http://8.8.8.8", allowPrivate: true, disallowed: true},
		{name: "http public ip rejected without opt-in", raw: "http://8.8.8.8", disallowed: true},
		{name: "https private ip needs opt-in", raw: "https://192.168.1.10:8443", disallowed: true},
		{name: "https private ip with opt-in", raw: "https://192.168.1.10:8443", allowPrivate: true, want: "https://192.168.1.10:8443"},
		{name: "lan ip needs opt-in", raw: "http://192.168.1.10:8080", disallowed: true},
		{name: "lan ip with opt-in", raw: "http://192.168.1.10:8080", allowPrivate: true, want: "http://192.168.1.10:8080"},
		{name: "loopback needs opt-in", raw: "http://127.0.0.1:8080", disallowed: true},
		{name: "loopback with opt-in", raw: "http://127.0.0.1:8080", allowPrivate: true, want: "http://127.0.0.1:8080"},
		{name: "localhost needs opt-in", raw: "http://localhost:8080", disallowed: true},
		{name: "localhost with opt-in", raw: "http://localhost:8080", allowPrivate: true, want: "http://localhost:8080"},
		{name: "link-local needs opt-in", raw: "http://169.254.10.5", disallowed: true},
		{name: "credentials rejected", raw: "https://user:pass@api.day.app", wantErr: true},
		{name: "query rejected", raw: "https://api.day.app?key=abc", wantErr: true},
		{name: "fragment rejected", raw: "https://api.day.app#x", wantErr: true},
		{name: "traversal rejected", raw: "https://api.day.app/../admin", wantErr: true},
		{name: "non-http scheme rejected", raw: "file:///etc/passwd", wantErr: true},
		{name: "empty rejected", raw: "   ", wantErr: true},
		{name: "bad port rejected", raw: "https://api.day.app:notaport", wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := ValidateBarkServerURL(testCase.raw, testCase.allowPrivate)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", testCase.raw, got)
				}
				return
			}
			if testCase.disallowed {
				if err == nil {
					t.Fatalf("expected disallowed error for %q", testCase.raw)
				}
				if !strings.Contains(err.Error(), "not allowed") {
					t.Fatalf("expected disallowed error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("normalized %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestBarkDeviceKeyValidationAndHint(t *testing.T) {
	if _, err := ValidateBarkDeviceKey(""); err == nil {
		t.Fatal("empty key must be rejected")
	}
	if _, err := ValidateBarkDeviceKey("bad key"); err == nil {
		t.Fatal("whitespace key must be rejected")
	}
	if _, err := ValidateBarkDeviceKey("bad\nkey"); err == nil {
		t.Fatal("control character key must be rejected")
	}
	long := strings.Repeat("a", barkMaxKeyLength+1)
	if _, err := ValidateBarkDeviceKey(long); err == nil {
		t.Fatal("oversized key must be rejected")
	}
	key, err := ValidateBarkDeviceKey("  AbCd1234  ")
	if err != nil || key != "AbCd1234" {
		t.Fatalf("valid key trimmed: %q %v", key, err)
	}
	if hint := BarkKeyHint("AbCd1234"); hint != "••••1234" {
		t.Fatalf("hint reveals only last four: %q", hint)
	}
	if hint := BarkKeyHint("abc"); hint != "••••" {
		t.Fatalf("short key fully hidden: %q", hint)
	}
}

func TestBarkCallTextLocalization(t *testing.T) {
	title, body := BarkCallText("zh-Hans", "13800001111")
	if !strings.Contains(title, "来电") || !strings.Contains(body, "13800001111") {
		t.Fatalf("unexpected zh text: %q / %q", title, body)
	}
	title, body = BarkCallText("en-US", "13800001111")
	if !strings.Contains(strings.ToLower(title), "incoming") || !strings.Contains(body, "13800001111") {
		t.Fatalf("unexpected en text: %q / %q", title, body)
	}
	title, body = BarkCallText("zh-Hant", "")
	if !strings.Contains(body, "未知号码") {
		t.Fatalf("blank peer must not leak an empty body: %q / %q", title, body)
	}
	if t1, _ := BarkTestText("en"); !strings.Contains(strings.ToLower(t1), "test") {
		t.Fatalf("unexpected test title: %q", t1)
	}
}

func TestBarkClientPostsKeyInBodyNeverURL(t *testing.T) {
	var gotPath, gotMethod, gotContentType, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		payload, _ := io.ReadAll(r.Body)
		gotBody = string(payload)
		if strings.Contains(r.URL.String(), "sekret") {
			t.Errorf("device key leaked into the request URL: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":200,"message":"success"}`))
	}))
	defer server.Close()

	client := NewBarkClient(true, 2*time.Second)
	err := client.Send(context.Background(), BarkTarget{
		DeviceID: "dev_1", ServerURL: server.URL, DeviceKey: "sekret-key", Locale: "zh-Hans",
	}, BarkNotification{
		Title: "CallRelay 来电", Body: "138 正在呼叫", URL: BarkDeepLinkURL,
		Group: "CallRelay", Level: "timeSensitive",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/push" {
		t.Fatalf("expected POST /push, got %s %s", gotMethod, gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("unexpected content type %q", gotContentType)
	}
	var body struct {
		DeviceKey string `json:"device_key"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		URL       string `json:"url"`
		Level     string `json:"level"`
	}
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.DeviceKey != "sekret-key" {
		t.Fatalf("device key must travel in the body, got %q", body.DeviceKey)
	}
	if body.URL != BarkDeepLinkURL || strings.Contains(body.URL, "sekret") {
		t.Fatalf("deeplink must be static and token-free: %q", body.URL)
	}
	if body.Level != "timeSensitive" {
		t.Fatalf("unexpected level %q", body.Level)
	}
}

func TestBarkClientRefusesPrivateTargetWithoutOptIn(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"code":200}`))
	}))
	defer server.Close()

	client := NewBarkClient(false, time.Second)
	err := client.Send(context.Background(), BarkTarget{
		ServerURL: server.URL, DeviceKey: "key-123456",
	}, BarkNotification{Title: "x", Body: "y"})
	if err == nil {
		t.Fatal("loopback target must be refused without explicit LAN opt-in")
	}
	if requests.Load() != 0 {
		t.Fatal("no request may leave the process for a disallowed target")
	}
}

func TestBarkClientNeverFollowsRedirects(t *testing.T) {
	var redirected atomic.Int64
	redirectedTo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		payload, _ := io.ReadAll(r.Body)
		if strings.Contains(string(payload), "sekret-key") {
			t.Errorf("key forwarded to redirect target")
		}
		_, _ = w.Write([]byte(`{"code":200}`))
	}))
	defer redirectedTo.Close()

	var firstBody string
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		firstBody = string(payload)
		http.Redirect(w, r, redirectedTo.URL+"/push", http.StatusTemporaryRedirect)
	}))
	defer first.Close()

	client := NewBarkClient(true, time.Second)
	err := client.Send(context.Background(), BarkTarget{
		ServerURL: first.URL, DeviceKey: "sekret-key",
	}, BarkNotification{Title: "x", Body: "y"})
	if err == nil {
		t.Fatal("a 3xx response must surface as an error, not be followed")
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect target must never receive the notification")
	}
	if !strings.Contains(firstBody, "sekret-key") {
		t.Fatalf("sanity: first hop carries the key in its body, got %q", firstBody)
	}
}

func TestBarkClientSanitizesProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A hostile/broken server echoes the secret; the error must not.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"message":"rejected sekret-key at http://internal"}`))
	}))
	defer server.Close()

	client := NewBarkClient(true, time.Second)
	err := client.Send(context.Background(), BarkTarget{
		ServerURL: server.URL, DeviceKey: "sekret-key",
	}, BarkNotification{Title: "x", Body: "y"})
	if err == nil {
		t.Fatal("non-2xx must be an error")
	}
	if strings.Contains(err.Error(), "sekret-key") || strings.Contains(err.Error(), server.URL) {
		t.Fatalf("error leaked secret/endpoint: %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should carry the HTTP status only: %v", err)
	}
}

func TestBarkClientRejectsNon200BarkCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":400,"message":"failed to send, check key"}`))
	}))
	defer server.Close()

	client := NewBarkClient(true, time.Second)
	err := client.Send(context.Background(), BarkTarget{
		ServerURL: server.URL, DeviceKey: "key-12345",
	}, BarkNotification{Title: "x", Body: "y"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("bark-level rejection must surface the numeric code: %v", err)
	}
}

func TestBarkClientRequiresExplicitCode200(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "malformed json", body: "not-json"},
		{name: "missing code", body: `{"message":"success"}`},
		{name: "null code", body: `{"code":null}`},
		{name: "html error page", body: "<html>proxy error</html>"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()
			client := NewBarkClient(true, time.Second)
			err := client.Send(context.Background(), BarkTarget{
				ServerURL: server.URL, DeviceKey: "key-12345",
			}, BarkNotification{Title: "x", Body: "y"})
			if err == nil {
				t.Fatalf("2xx without a documented code=200 must not be accepted (body %q)", testCase.body)
			}
		})
	}
}

func TestBarkClientHTTPLANModeStillSendsOnPrivateTarget(t *testing.T) {
	var got atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Add(1)
		_, _ = w.Write([]byte(`{"code":200}`))
	}))
	defer server.Close()

	// Explicit LAN opt-in plus an actual private destination: clear-text
	// delivery is deliberately supported.
	client := NewBarkClient(true, 2*time.Second)
	if err := client.Send(context.Background(), BarkTarget{
		ServerURL: server.URL, DeviceKey: "key-12345",
	}, BarkNotification{Title: "lan", Body: "ok"}); err != nil {
		t.Fatalf("explicit LAN self-host must work: %v", err)
	}
	if got.Load() != 1 {
		t.Fatalf("expected one delivered notification, got %d", got.Load())
	}
}

func TestBarkClientHTTPPublicTargetRefusedEvenWithLANOptIn(t *testing.T) {
	// A public literal over http is refused before any connection; the
	// allow_private switch never turns into "clear text to anywhere".
	client := NewBarkClient(true, time.Second)
	err := client.Send(context.Background(), BarkTarget{
		ServerURL: "http://198.51.100.20:8080", DeviceKey: "key-12345",
	}, BarkNotification{Title: "x", Body: "y"})
	if !errors.Is(err, ErrBarkURLDisallowed) {
		t.Fatalf("expected disallowed public http target, got %v", err)
	}
}

func TestBarkDeduperOneNotificationPerDeviceAndCall(t *testing.T) {
	deduper := NewBarkDeduper(time.Minute)
	now := time.Now()
	if !deduper.reserveAt("dev_a", "line1:call1", now) {
		t.Fatal("first reserve must win")
	}
	if deduper.reserveAt("dev_a", "line1:call1", now.Add(time.Second)) {
		t.Fatal("second reserve within TTL must be suppressed")
	}
	if !deduper.reserveAt("dev_b", "line1:call1", now.Add(time.Second)) {
		t.Fatal("another device must still be notified")
	}
	if !deduper.reserveAt("dev_a", "line1:call2", now.Add(time.Second)) {
		t.Fatal("another call must still be notified")
	}
	if !deduper.reserveAt("dev_a", "line1:call1", now.Add(2*time.Minute)) {
		t.Fatal("expired reservation must be reusable")
	}
	deduper.Forget("dev_b", "line1:call1")
	if !deduper.reserveAt("dev_b", "line1:call1", now.Add(2*time.Second)) {
		t.Fatal("forgotten reservation must be reusable")
	}
}
