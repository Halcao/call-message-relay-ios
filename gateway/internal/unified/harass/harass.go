// Package harass evaluates one enrollment key's inbound harassment policy.
// It is deliberately pure and small: the runtime loads the key's policy from
// the store (two small queries) and calls Evaluate on the ring/SMS path,
// never on the realtime media path.
//
// Precedence (same contract as the client-side classifier):
//  1. Blocking disabled for a channel => every caller/SMS is allowed.
//  2. Explicit TRUST entries always win.
//  3. Explicit owner SENDER blocks (exact number / digit prefix) — these are
//     never bypassed by contact trust.
//  4. Contact trust (optional; the owner's gateway-synced contacts) — after
//     explicit owner blocks, before text rules.
//  5. Keyword/regex rules, but STRUCTURED OTP/parcel notifications are
//     protected from them (a message that legitimately carries a code).
//  6. Unknown peer => allowed (default allow; no external classification).
//
// Blocked entries with action "reject" suppress the call/SMS for this key;
// entries with action "label" only mark it (imported historical lists default
// to label, never reject).
package harass

import (
	"regexp"
	"strings"

	"callrelay/gateway/internal/unified/contacts"
)

// Kind is the channel an inbound item arrives on.
type Kind string

const (
	KindCall Kind = "call"
	KindSMS  Kind = "sms"
)

// Entry is one enabled rule for a key.
type Entry struct {
	ID      string
	Kind    string // exact | prefix | keyword | regex
	Pattern string
	Mode    string // block | trust
	Action  string // reject | label | allow (trust)
	ListID  string
	Source  string
}

// Policy is the complete evaluation input for one key.
type Policy struct {
	CallBlocking bool
	SMSBlocking  bool
	// ContactWhitelist enables phase 4; ContactTrusted resolves a peer against
	// this key's synced contacts. Both must be set for contact trust to apply.
	ContactWhitelist bool
	ContactTrusted   func(peer string) bool
	Entries          []Entry
}

// Decision is the evaluation result. Allow is the delivery decision; Action
// distinguishes an explicitly allowed item from a denied or labeled one so
// the runtime can record truthful block history.
type Decision struct {
	Allow     bool
	Action    string // allow | reject | label
	Reason    string // trusted | blocklist | prefix | keyword | regex | label | contact | protected | default_allow | disabled
	EntryID   string
	Pattern   string
	MatchedBy string
	// Suppressed is true only when this key's devices must not receive the
	// normal event/ring/push for the item. Quarantined SMS stays retrievable
	// through the key's junk listing.
	Suppressed bool
}

func (p Policy) blockingEnabled(kind Kind) bool {
	if kind == KindCall {
		return p.CallBlocking
	}
	return p.SMSBlocking
}

func isSenderKind(kind string) bool { return kind == "exact" || kind == "prefix" }

func isTextKind(kind string) bool { return kind == "keyword" || kind == "regex" }

// Evaluate applies the key's policy to one inbound peer (phone number) and,
// for SMS, the message body.
func (p Policy) Evaluate(kind Kind, peer, body string) Decision {
	if !p.blockingEnabled(kind) {
		return Decision{Allow: true, Action: "allow", Reason: "disabled"}
	}
	// Phase 1: explicit trust always wins, for both calls and SMS.
	for _, entry := range p.Entries {
		if entry.Mode != "trust" || !entryMatches(entry, kind, peer, body) {
			continue
		}
		return Decision{Allow: true, Action: "allow", Reason: "trusted", EntryID: entry.ID, Pattern: entry.Pattern, MatchedBy: entry.Kind}
	}
	// Phase 2: explicit owner sender blocks (never overridden by contacts).
	for _, entry := range p.Entries {
		if entry.Mode == "trust" || !isSenderKind(entry.Kind) || !entryMatches(entry, kind, peer, body) {
			continue
		}
		return blockDecision(entry)
	}
	// Phase 3: contact trust (optional, after explicit owner blocks).
	if p.ContactWhitelist && p.ContactTrusted != nil && p.ContactTrusted(peer) {
		return Decision{Allow: true, Action: "allow", Reason: "contact"}
	}
	// Phase 4: keyword/regex text rules, with structured OTP/parcel
	// protection matching the client-side classifier.
	if kind == KindSMS && isProtectedTransaction(body) {
		return Decision{Allow: true, Action: "allow", Reason: "protected"}
	}
	for _, entry := range p.Entries {
		if entry.Mode == "trust" || !isTextKind(entry.Kind) || !entryMatches(entry, kind, peer, body) {
			continue
		}
		return blockDecision(entry)
	}
	return Decision{Allow: true, Action: "allow", Reason: "default_allow"}
}

func blockDecision(entry Entry) Decision {
	if entry.Action == "label" {
		return Decision{Allow: true, Action: "label", Reason: "label", EntryID: entry.ID, Pattern: entry.Pattern, MatchedBy: entry.Kind}
	}
	reason := "blocklist"
	switch entry.Kind {
	case "prefix":
		reason = "prefix"
	case "keyword":
		reason = "keyword"
	case "regex":
		reason = "regex"
	}
	return Decision{Allow: false, Action: "reject", Reason: reason, EntryID: entry.ID, Pattern: entry.Pattern, MatchedBy: entry.Kind, Suppressed: true}
}

func entryMatches(entry Entry, kind Kind, peer, body string) bool {
	switch entry.Kind {
	case "keyword":
		if kind != KindSMS {
			return false
		}
		pattern := strings.TrimSpace(entry.Pattern)
		return pattern != "" && strings.Contains(strings.ToLower(body), strings.ToLower(pattern))
	case "regex":
		if kind != KindSMS {
			return false
		}
		pattern := strings.TrimSpace(entry.Pattern)
		if pattern == "" || len(pattern) > 200 {
			return false
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return false
		}
		return re.MatchString(body)
	case "prefix":
		prefix := contacts.Digits(entry.Pattern)
		if prefix == "" {
			return false
		}
		// Prefix matching uses every conservative spelling so a stored
		// national prefix ("139") also matches +86/0086 callers.
		for _, key := range contacts.CanonicalKeys(peer) {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		}
		return false
	default:
		return contacts.SamePhone(peer, entry.Pattern)
	}
}

// isProtectedTransaction mirrors the client-side SpamPolicy protection: a
// message is protected only when it carries BOTH a strong legitimate marker
// (verification/parcel keyword) AND a standalone 4-8 digit code structure.
// The words alone are NOT enough (scam messages embed them).
func isProtectedTransaction(body string) bool {
	if strings.TrimSpace(body) == "" {
		return false
	}
	hasCode := hasStandaloneCode(body)
	if !hasCode {
		return false
	}
	lower := strings.ToLower(body)
	otpMarker := strings.Contains(body, "验证码") || strings.Contains(body, "校验码") || strings.Contains(body, "动态码") ||
		strings.Contains(lower, "verification code") || strings.Contains(lower, "one-time code") || strings.Contains(lower, "otp")
	parcelMarker := strings.Contains(body, "快递") || strings.Contains(body, "取件码") || strings.Contains(body, "包裹") ||
		strings.Contains(lower, "delivery")
	return otpMarker || parcelMarker
}

// hasStandaloneCode reports whether the body contains a digit run of 4..8
// digits bounded by non-digits (exactly the client regex semantics, without
// lookbehind which RE2 does not support).
func hasStandaloneCode(body string) bool {
	run := 0
	for i := 0; i <= len(body); i++ {
		if i < len(body) && body[i] >= '0' && body[i] <= '9' {
			run++
			continue
		}
		if run >= 4 && run <= 8 {
			return true
		}
		run = 0
	}
	return false
}
