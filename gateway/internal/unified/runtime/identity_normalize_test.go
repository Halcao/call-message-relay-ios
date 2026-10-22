package runtime

import "testing"

// TestNormalizeManualNumber fixes the accepted stored shape: 3-20 digits
// with an optional single leading '+', human separators removed, and an
// explicit reject for supplementary-service/control strings.
func TestNormalizeManualNumber(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"plain digits", "13800138000", "13800138000", true},
		{"international plus", " +15550001111 ", "+15550001111", true},
		{"separators", "+1 (555) 000-1111", "+15550001111", true},
		{"national separators", "138 0013 8000", "13800138000", true},
		{"clear", "   ", "", true},
		{"plus in middle rejected", "1555+0001", "", false},
		{"star control", "*#31#", "", false},
		{"star digits", "1555*0001", "", false},
		{"hash suffix", "15550001111#", "", false},
		{"too short", "12", "", false},
		{"too long", "123456789012345678901", "", false},
		{"letters", "1555000abcd", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := normalizeManualNumber(tc.input)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("normalizeManualNumber(%q) = %q,%v want %q,%v", tc.input, got, ok, tc.want, tc.ok)
			}
		})
	}
}
