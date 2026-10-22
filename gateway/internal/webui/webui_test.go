package webui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSWNotificationClickHarness drives the service worker's
// notificationclick routing with mocked window clients (single-redemption,
// second-notification, no-bogus-hash assertions). It requires node on the
// host and skips cleanly where node is unavailable (e.g. CI variants).
func TestSWNotificationClickHarness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available; run gateway/internal/webui/pwa/harness/sw-notificationclick-test.js manually")
	}
	harness := filepath.Join("pwa", "harness", "sw-notificationclick-test.js")
	output, err := exec.Command("node", harness).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("SW notificationclick harness failed: %v", err)
	}
}

// TestPCMUCodecHarness proves the PWA's pcmu.js is byte-for-byte compatible
// with the Go/iOS PCMUCodec (segment table, bias, clipping, sign handling)
// so browser audio interops with every existing endpoint. Skips cleanly
// without node.
func TestPCMUCodecHarness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available; run gateway/internal/webui/pwa/harness/pcmu-test.js manually")
	}
	harness := filepath.Join("pwa", "harness", "pcmu-test.js")
	output, err := exec.Command("node", harness).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("PCMU codec harness failed: %v", err)
	}
}

// TestCallFlowHarness drives the browser call-flow controller with injected
// fakes: answer-before-media ordering (the gateway rejects pre-answer media
// with 409), permission-denied never answers, hangup during async audio init
// leaks nothing, the mute gate sends only zero PCM, send backlog is bounded,
// and the media ready wait is bounded with one retry.
func TestCallFlowHarness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available; run gateway/internal/webui/pwa/harness/callflow-test.js manually")
	}
	harness := filepath.Join("pwa", "harness", "callflow-test.js")
	output, err := exec.Command("node", harness).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("callflow harness failed: %v", err)
	}
}

// TestCallEventsHarness locks gateway call-event envelope parsing: an
// actual runtime envelope surfaces an inbound ringing call while idle and
// never steals the active call.
func TestCallEventsHarness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available; run gateway/internal/webui/pwa/harness/callevents-test.js manually")
	}
	harness := filepath.Join("pwa", "harness", "callevents-test.js")
	output, err := exec.Command("node", harness).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("call-events harness failed: %v", err)
	}
}

// TestDenoiseHarness locks the build-49 PWA denoise preference contract:
// default OFF with strict opt-in parsing, per-enrollment scoped storage
// (hashed seed, never the raw token), logout clearing, private-mode memory
// fallback, change notification, and the attach/hot-control payloads.
func TestDenoiseHarness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available; run gateway/internal/webui/pwa/harness/denoise-test.js manually")
	}
	harness := filepath.Join("pwa", "harness", "denoise-test.js")
	output, err := exec.Command("node", harness).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("denoise harness failed: %v", err)
	}
}

// TestContactsLogicHarness drives the pure web address-book matching rules:
// number fragments, Chinese names, phone-synced pinyin/T9 keys, ranking and
// the import summary. Skips cleanly without node.
func TestContactsLogicHarness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available; run gateway/internal/webui/pwa/harness/contactslogic-test.js manually")
	}
	harness := filepath.Join("pwa", "harness", "contactslogic-test.js")
	output, err := exec.Command("node", harness).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("contacts-logic harness failed: %v", err)
	}
}

// TestThreadLogicHarness locks the cross-line SMS selection rules: a line
// switched in the conversation header is the send line; the gateway's
// canonical thread is reopened with that line persisted under its own key
// (two consecutive sends stay on the picked line, shown once each); retry
// reuses the failed message's original line. Skips cleanly without node.
func TestThreadLogicHarness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available; run gateway/internal/webui/pwa/harness/threadlogic-test.js manually")
	}
	harness := filepath.Join("pwa", "harness", "threadlogic-test.js")
	output, err := exec.Command("node", harness).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("thread-logic harness failed: %v", err)
	}
}

// TestWebRTCHarness drives the browser WebRTC media path with injected
// fakes: the existing gateway direct endpoints are used in order
// (GET ice -> POST offer), mic tracks are attached to the peer connection,
// mute flips real track.enabled, stop releases the peer connection, a failed
// negotiation rejects, and CallFlow prefers WebRTC while falling back to WSS
// on a single shared mic stream.
func TestWebRTCHarness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available; run gateway/internal/webui/pwa/harness/webrtc-test.js manually")
	}
	harness := filepath.Join("pwa", "harness", "webrtc-test.js")
	output, err := exec.Command("node", harness).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("webrtc harness failed: %v", err)
	}
}

// TestSettingsBackLink guards defect 3: the notification/settings page must
// always expose a visible link back to the standalone client (an installed
// PWA has no browser chrome), and the client page must load the thread
// logic used by the SMS fix.
func TestSettingsBackLink(t *testing.T) {
	index, err := pwaAssets.ReadFile("pwa/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !bytes.Contains(index, []byte(`id="client-link"`)) ||
		!bytes.Contains(index, []byte(`href="/pwa/client.html"`)) {
		t.Fatal("settings page must contain a visible back link to /pwa/client.html")
	}
	if !bytes.Contains(index, []byte("client.html?fixture=1")) {
		t.Fatal("settings back link must preserve the fixture marker in fixture mode")
	}
	client, err := pwaAssets.ReadFile("pwa/client.html")
	if err != nil {
		t.Fatalf("read client.html: %v", err)
	}
	if !bytes.Contains(client, []byte("/pwa/threadlogic.js")) {
		t.Fatal("client.html must load threadlogic.js")
	}
	if !bytes.Contains(client, []byte("/pwa/webrtc.js")) {
		t.Fatal("client.html must load the browser WebRTC media module")
	}
	// Contacts tab: the page must load the pure suggestion logic and expose
	// the import/preview surface (vCard import is a shipped product action).
	if !bytes.Contains(client, []byte("/pwa/contactslogic.js")) {
		t.Fatal("client.html must load contactslogic.js")
	}
	for _, required := range []string{`data-tab="contacts"`, `id="contacts-import"`, `id="contacts-preview-commit"`, `id="contacts-file"`} {
		if !bytes.Contains(client, []byte(required)) {
			t.Fatalf("client.html must expose the contacts UI (%s)", required)
		}
	}
	// Standalone-first pairing: the gateway connection key is the PRIMARY
	// flow (no native App required); the App-generated bind code stays in a
	// collapsed secondary disclosure. The primary copy is product language,
	// not raw credential internals.
	enrollIdx := bytes.Index(client, []byte("enroll-key"))
	detailsIdx := bytes.Index(client, []byte("<details"))
	bindIdx := bytes.Index(client, []byte("bind-code"))
	if enrollIdx < 0 || detailsIdx < 0 || bindIdx < 0 ||
		!(enrollIdx < detailsIdx && bindIdx > detailsIdx) {
		t.Fatal("gateway-key pairing must be primary; the App bind code stays inside the secondary disclosure")
	}
	if !bytes.Contains(client, []byte("粘贴网关连接密钥即可拨打电话和收发短信。")) {
		t.Fatal("client pairing must present the standalone connection-key flow")
	}
	if bytes.Contains(client, []byte("keyId.secret")) {
		t.Fatal("pairing UI must not expose credential internals")
	}
	// And the link must actually be SERVED at /pwa/ (no-store HTML).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/pwa/", nil)
	PWAHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("client-link")) {
		t.Fatalf("/pwa/ must serve the settings page with the back link, code=%d", rec.Code)
	}
}

// TestResamplerHarness locks the capture decimator to exactly 8000 samples
// per second of input at 44.1/48 kHz (Math.round would yield 7350 Hz at
// 44.1 kHz), with phase-coherent reconstruction across chunk boundaries.
func TestResamplerHarness(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available; run gateway/internal/webui/pwa/harness/resampler-test.js manually")
	}
	harness := filepath.Join("pwa", "harness", "resampler-test.js")
	output, err := exec.Command("node", harness).CombinedOutput()
	t.Log(string(output))
	if err != nil {
		t.Fatalf("resampler harness failed: %v", err)
	}
}
