package console

import (
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConsoleCopyHandlerFallback runs the real rendered inline copy/reveal
// handler in a JS engine with a DOM + clipboard double. It proves the handler
// reports success only when the clipboard API resolves (or the legacy
// execCommand actually reports success), reports failure with a selectable
// fallback when the clipboard is rejected, animates reveal (POST) -> copy ->
// hide, and never leaves the secret in the DOM after hiding.
func TestConsoleCopyHandlerFallback(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not available; console copy-handler harness skipped")
	}
	env := newConsoleTestEnv(t)
	env.seed()

	recorder := env.post("/console/keys", url.Values{
		"csrf": {testCSRF}, "name": {"复制回退测试"}, "all_lines": {"on"},
	})
	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("key create status = %d, want 303", recorder.Code)
	}
	page := env.get(recorder.Header().Get("Location"))
	if page.Code != http.StatusOK {
		t.Fatalf("created keys page status = %d, want 200", page.Code)
	}
	htmlPath := filepath.Join(t.TempDir(), "keys.html")
	if err := os.WriteFile(htmlPath, page.Body.Bytes(), 0o600); err != nil {
		t.Fatalf("write rendered page: %v", err)
	}

	harness := filepath.Join("testdata", "copy_handler_harness.js")
	output, err := exec.Command(node, harness, htmlPath).CombinedOutput()
	if err != nil {
		t.Fatalf("copy-handler harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "ALL COPY HANDLER SCENARIOS PASSED") {
		t.Fatalf("copy-handler harness did not report success:\n%s", output)
	}
}
