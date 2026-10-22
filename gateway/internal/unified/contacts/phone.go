// Package contacts owns principal-scoped address-book import, merge and
// sync. It is deliberately independent from the iOS client rules except for
// the conservative canonical-phone contract: mainland +86/0086/trunk
// spellings collapse, everything unrecognized stays digit-exact so a merge
// can never be widened into a prefix match.
package contacts

import "strings"

// Digits strips everything but ASCII digits. It intentionally does not
// convert full-width or Arabic-Indic digits.
func Digits(raw string) string {
	var builder strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// CanonicalKeys returns every conservative spelling of a phone number for
// equality comparison. The plain digit string is always included; mainland
// mobile/landline shapes additionally get their +86/0086 aliases. An explicit
// "+1..." shape is never given +86 aliases.
func CanonicalKeys(raw string) []string {
	digits := Digits(raw)
	if digits == "" {
		return nil
	}
	keys := []string{digits}
	add := func(value string) {
		for _, existing := range keys {
			if existing == value {
				return
			}
		}
		keys = append(keys, value)
	}
	explicitInternational := strings.HasPrefix(strings.TrimSpace(raw), "+")
	if explicitInternational && strings.HasPrefix(digits, "1") {
		return keys
	}

	local := digits
	switch {
	case strings.HasPrefix(digits, "0086") && len(digits) > 4:
		local = digits[4:]
	case strings.HasPrefix(digits, "86") && len(digits) > 11 && len(digits) <= 13:
		local = digits[2:]
	}

	if isMainlandMobile(local) {
		add(local)
		add("86" + local)
		add("0086" + local)
		if strings.HasPrefix(digits, "86") {
			add(digits)
		}
	} else if landline, ok := mainlandLandline(local); ok {
		add(landline)
		add("86" + landline[1:])
		add("0086" + landline[1:])
	}
	return keys
}

// SamePhone reports whether two numbers share any canonical spelling.
func SamePhone(a, b string) bool {
	left := CanonicalKeys(a)
	if len(left) == 0 {
		return false
	}
	right := CanonicalKeys(b)
	if len(right) == 0 {
		return false
	}
	for _, x := range left {
		for _, y := range right {
			if x == y {
				return true
			}
		}
	}
	return false
}

// CanonicalPhone returns a stable match form for storage: the plain digit
// string when present. Matching itself uses CanonicalKeys, so this value is
// only an index hint.
func CanonicalPhone(raw string) string { return Digits(raw) }

func isMainlandMobile(digits string) bool {
	if len(digits) != 11 || !strings.HasPrefix(digits, "1") {
		return false
	}
	second := digits[1]
	return second >= '3' && second <= '9'
}

func mainlandLandline(digits string) (string, bool) {
	if !strings.HasPrefix(digits, "0") || len(digits) < 10 || len(digits) > 12 {
		return "", false
	}
	areaLength := 4
	if digits[1] == '1' || digits[1] == '2' {
		areaLength = 3
	}
	localCount := len(digits) - areaLength
	if localCount < 7 || localCount > 8 {
		return "", false
	}
	return digits[:areaLength] + digits[len(digits)-localCount:], true
}
