package sms

import (
	"strings"
	"unicode"
)

// NormalizeDestination reduces a user-entered destination to its dialable
// form. It is a thin wrapper over ValidateDestination for callers that only
// need the normalized string; an empty result means the input was empty or
// invalid.
func NormalizeDestination(raw string) string {
	normalized, ok := ValidateDestination(raw)
	if !ok {
		return ""
	}
	return normalized
}

// ValidateDestination reduces a user-entered destination to its dialable
// form: a single leading '+' is preserved (international TON) and ONLY
// recognized cosmetic separators (whitespace, hyphens, parentheses, dots)
// are removed. Contacts hand over formatted strings such as
// "+86 130 0313 2132"; passed verbatim they fail PDU encoding BEFORE the
// message row is created, which the field log showed as
//
//	invalid SMS PDU: invalid destination "86 130 0313 2132"
//
// ok is false for an empty destination OR when anything other than ASCII
// digits remains after the separators are stripped — extensions (";123",
// "x123"), dial pauses (","), letters and non-ASCII digits are rejected
// rather than silently concatenated into the number, which could misdirect
// the message. Digits are never rewritten (no trunk/country-code guessing):
// the modem keeps exactly the number the user chose.
func ValidateDestination(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	international := strings.HasPrefix(trimmed, "+")
	body := trimmed
	if international {
		body = trimmed[1:]
	}
	var builder strings.Builder
	for _, r := range body {
		switch {
		case unicode.IsSpace(r), r == '-' || r == '(' || r == ')' || r == '.':
			continue // recognized cosmetic separator
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		default:
			return "", false // extension, pause, letter, non-ASCII digit…
		}
	}
	if builder.Len() == 0 {
		return "", false
	}
	if international {
		return "+" + builder.String(), true
	}
	return builder.String(), true
}
