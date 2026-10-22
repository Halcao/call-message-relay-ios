package push

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncatePreviewBoundsCJK(t *testing.T) {
	// 300 CJK runes (900 bytes) into the 120-rune body bound.
	body := strings.Repeat("短", 300)
	got := TruncatePreview(body, PreviewBodyRunes)
	if n := utf8.RuneCountInString(got); n != PreviewBodyRunes {
		t.Fatalf("CJK clip must fill the bound exactly, got %d runes: %q", n, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("clipped CJK must end with the ellipsis: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("clipped CJK split a code point: %q", got)
	}
	if want := strings.Repeat("短", PreviewBodyRunes-1) + "…"; got != want {
		t.Fatalf("clip must keep the first %d runes; got %q", PreviewBodyRunes-1, got)
	}
}

func TestTruncatePreviewKeepsEmojiWhole(t *testing.T) {
	// Family-combining sequences and ZWJ emoji are multiple code points;
	// the bound is a CODE POINT budget and never a byte slice, so output is
	// always valid UTF-8 (no replacement characters).
	body := strings.Repeat("🎉👨‍👩‍👧‍👦", 80)
	got := TruncatePreview(body, PreviewBodyRunes)
	if !utf8.ValidString(got) {
		t.Fatalf("emoji preview is invalid UTF-8: %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("emoji preview contains a replacement rune: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > PreviewBodyRunes {
		t.Fatalf("emoji preview exceeds the bound: %d runes", n)
	}
}

func TestTruncatePreviewCollapsesWhitespace(t *testing.T) {
	if got := TruncatePreview("  Alice\n\n 经多行  \t 短信  \n", 120); got != "Alice 经多行 短信" {
		t.Fatalf("whitespace collapse produced %q", got)
	}
	// A clip that would land on a space must not leave "␠…".
	body := strings.Repeat("a", 119) + " 空格尾随"
	got := TruncatePreview(body, PreviewBodyRunes)
	if strings.HasSuffix(got, " …") {
		t.Fatalf("ellipsis must not follow a dangling space: %q", got)
	}
	if got != strings.Repeat("a", 119)+"…" {
		t.Fatalf("unexpected clip %q", got)
	}
}

func TestTruncatePreviewIdempotent(t *testing.T) {
	// The runtime truncates once at the fan-out point and the sender
	// truncates again before embedding; the second pass must be a no-op.
	inputs := []string{
		strings.Repeat("中", 500),
		strings.Repeat("🎉", 300),
		"  Alice\t多行\n正文  " + strings.Repeat("x", 400),
		strings.Repeat("a", 119) + "尾",
		"exact", "", "   ", "x",
	}
	for _, in := range inputs {
		once := TruncatePreview(in, PreviewBodyRunes)
		if twice := TruncatePreview(once, PreviewBodyRunes); twice != once {
			t.Fatalf("double truncation changed the preview: %q -> %q", once, twice)
		}
	}
}

func TestTruncatePreviewDegenerateLimits(t *testing.T) {
	if got := TruncatePreview("anything", 0); got != "" {
		t.Fatalf("maxRunes<1 must yield empty, got %q", got)
	}
	if got := TruncatePreview("anything", -5); got != "" {
		t.Fatalf("negative maxRunes must yield empty, got %q", got)
	}
	if got := TruncatePreview("恰好", PreviewPeerRunes); got != "恰好" {
		t.Fatalf("under-bound text must pass through untouched, got %q", got)
	}
}
