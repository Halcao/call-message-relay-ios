package sms

import "testing"

func TestValidateDestination(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"plain", "10086", "10086", true},
		{"field failure spaces without plus", "86 130 0313 2132", "8613003132132", true},
		{"international keeps plus", "+86 130 0313 2132", "+8613003132132", true},
		{"hyphens and parentheses", "(021) 1234-5678", "02112345678", true},
		{"dots", "138.0013.8000", "13800138000", true},
		{"surrounding whitespace", "  10086  ", "10086", true},
		{"extension semicolon rejected", "10086;123", "", false},
		{"extension x rejected", "10086x123", "", false},
		{"dial pause comma rejected", "10086,123", "", false},
		{"letters rejected", "call fred", "", false},
		{"fullwidth digits rejected", "+８６１３８", "", false},
		{"empty", "   ", "", false},
		{"lone plus", "+", "", false},
		{"only separators", "- - ()", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ValidateDestination(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ValidateDestination(%q) = (%q, %v), want (%q, %v)",
					tc.in, got, ok, tc.want, tc.ok)
			}
			// NormalizeDestination mirrors it.
			if normalized := NormalizeDestination(tc.in); normalized != got {
				t.Fatalf("NormalizeDestination(%q) = %q, want %q", tc.in, normalized, got)
			}
		})
	}
}
