package harass

import "testing"

func policy(entries ...Entry) Policy {
	return Policy{CallBlocking: true, SMSBlocking: true, Entries: entries}
}

// TestEvaluateDefaultsToAllow: an unconfigured key is inert and unknown
// callers always default to allow.
func TestEvaluateDefaultsToAllow(t *testing.T) {
	disabled := Policy{}
	if decision := disabled.Evaluate(KindCall, "+8613800000001", ""); !decision.Allow || decision.Reason != "disabled" {
		t.Fatalf("disabled call = %+v", decision)
	}
	enabled := policy()
	if decision := enabled.Evaluate(KindCall, "+8613800000001", ""); !decision.Allow || decision.Reason != "default_allow" {
		t.Fatalf("unknown caller = %+v", decision)
	}
	if decision := enabled.Evaluate(KindSMS, "+8613800000001", "hello"); !decision.Allow || decision.Reason != "default_allow" {
		t.Fatalf("unknown SMS = %+v", decision)
	}
}

// TestTrustBeatsBlock: the trust list always wins, including against a
// broader block prefix, and matches +86/0086 spellings.
func TestTrustBeatsBlock(t *testing.T) {
	entries := []Entry{
		{ID: "b1", Kind: "prefix", Pattern: "139", Mode: "block", Action: "reject"},
		{ID: "t1", Kind: "exact", Pattern: "+8613912345678", Mode: "trust"},
	}
	p := policy(entries...)
	for _, peer := range []string{"+8613912345678", "8613912345678", "008613912345678", "13912345678"} {
		if decision := p.Evaluate(KindCall, peer, ""); !decision.Allow || decision.Reason != "trusted" {
			t.Fatalf("trusted %s = %+v", peer, decision)
		}
	}
	if decision := p.Evaluate(KindCall, "+8613912345679", ""); decision.Allow || decision.Reason != "prefix" {
		t.Fatalf("untrusted sibling = %+v", decision)
	}
}

// TestLabelNeverSuppresses: imported historical lists default to mark-only.
func TestLabelNeverSuppresses(t *testing.T) {
	p := policy(Entry{ID: "l1", Kind: "exact", Pattern: "13800000001", Mode: "block", Action: "label", ListID: "list_1"})
	decision := p.Evaluate(KindSMS, "13800000001", "hi")
	if !decision.Allow || decision.Action != "label" || decision.Suppressed {
		t.Fatalf("label decision = %+v", decision)
	}
}

// TestKeywordAndRegexApplyToSMSOnly.
func TestKeywordAndRegexApplyToSMSOnly(t *testing.T) {
	p := policy(
		Entry{ID: "k1", Kind: "keyword", Pattern: "发票", Mode: "block", Action: "reject"},
		Entry{ID: "r1", Kind: "regex", Pattern: `(?i)投资.*高回报`, Mode: "block", Action: "reject"},
	)
	if decision := p.Evaluate(KindSMS, "+8613711111111", "可以开发票"); decision.Allow || decision.Reason != "keyword" {
		t.Fatalf("keyword = %+v", decision)
	}
	if decision := p.Evaluate(KindSMS, "+8613711111111", "投资稳赚高回报"); decision.Allow || decision.Reason != "regex" {
		t.Fatalf("regex = %+v", decision)
	}
	if decision := p.Evaluate(KindSMS, "+8613711111111", "普通消息"); !decision.Allow {
		t.Fatalf("clean SMS = %+v", decision)
	}
	if decision := p.Evaluate(KindCall, "+8613711111111", "发票"); !decision.Allow {
		t.Fatalf("keyword leaked to calls = %+v", decision)
	}
	// An invalid regex never matches (and never panics).
	bad := policy(Entry{ID: "r2", Kind: "regex", Pattern: "([", Mode: "block", Action: "reject"})
	if decision := bad.Evaluate(KindSMS, "+8613711111111", "anything"); !decision.Allow {
		t.Fatalf("invalid regex matched = %+v", decision)
	}
}

// TestChannelTogglesAreIndependent.
func TestChannelTogglesAreIndependent(t *testing.T) {
	p := Policy{CallBlocking: true, SMSBlocking: false, Entries: []Entry{
		{ID: "e1", Kind: "exact", Pattern: "+8613800000002", Mode: "block", Action: "reject"},
	}}
	if decision := p.Evaluate(KindCall, "+8613800000002", ""); decision.Allow {
		t.Fatalf("call block ignored = %+v", decision)
	}
	if decision := p.Evaluate(KindSMS, "+8613800000002", ""); !decision.Allow {
		t.Fatalf("SMS toggle off still blocked = %+v", decision)
	}
}

// TestEmptyPeerNeverMatches.
func TestEmptyPeerNeverMatches(t *testing.T) {
	p := policy(Entry{ID: "e1", Kind: "exact", Pattern: "+8613800000003", Mode: "block", Action: "reject"})
	if decision := p.Evaluate(KindCall, "", ""); !decision.Allow {
		t.Fatalf("empty peer blocked = %+v", decision)
	}
	p2 := policy(Entry{ID: "e2", Kind: "prefix", Pattern: "1", Mode: "block", Action: "reject"})
	if decision := p2.Evaluate(KindCall, "", ""); !decision.Allow {
		t.Fatalf("empty peer prefix blocked = %+v", decision)
	}
}

// TestProtectedTransactionalMessagesMirrorClient: structured OTP/parcel
// notifications bypass keyword/regex rules exactly like the iOS classifier,
// while explicit sender blocks still apply.
func TestProtectedTransactionalMessagesMirrorClient(t *testing.T) {
	p := policy(
		Entry{ID: "k1", Kind: "keyword", Pattern: "验证码", Mode: "block", Action: "reject"},
		Entry{ID: "k2", Kind: "keyword", Pattern: "快递", Mode: "block", Action: "reject"},
		Entry{ID: "r1", Kind: "regex", Pattern: "取件", Mode: "block", Action: "reject"},
		Entry{ID: "b1", Kind: "exact", Pattern: "+8613800000010", Mode: "block", Action: "reject"},
	)
	// Structured OTP: marker + standalone 4-8 digit code -> protected.
	if decision := p.Evaluate(KindSMS, "+8613900000010", "【银行】验证码 482913，5 分钟内有效。"); !decision.Allow || decision.Reason != "protected" {
		t.Fatalf("OTP not protected: %+v", decision)
	}
	// Parcel pickup with a code -> protected.
	if decision := p.Evaluate(KindSMS, "+8613900000010", "您的快递已到，取件码 123456。"); !decision.Allow || decision.Reason != "protected" {
		t.Fatalf("parcel not protected: %+v", decision)
	}
	// Marker words WITHOUT a code are still junk (scam protection intact).
	if decision := p.Evaluate(KindSMS, "+8613900000010", "验证码即将过期请点击链接"); decision.Allow {
		t.Fatalf("marker-only message was protected: %+v", decision)
	}
	// An explicit owner sender block beats the OTP protection.
	if decision := p.Evaluate(KindSMS, "+8613800000010", "【银行】验证码 482913"); decision.Allow {
		t.Fatalf("explicit number block bypassed by protection: %+v", decision)
	}
}

// TestContactTrustComesAfterOwnerBlocksMirrorsClient precedence.
func TestContactTrustComesAfterOwnerBlocksMirrorsClient(t *testing.T) {
	contact := func(string) bool { return true }
	p := Policy{
		CallBlocking: true, SMSBlocking: true, ContactWhitelist: true, ContactTrusted: contact,
		Entries: []Entry{
			{ID: "b1", Kind: "exact", Pattern: "+8613800000011", Mode: "block", Action: "reject"},
			{ID: "k1", Kind: "keyword", Pattern: "发票", Mode: "block", Action: "reject"},
			{ID: "t1", Kind: "exact", Pattern: "+8613800000012", Mode: "trust"},
		},
	}
	// Explicit owner block wins over contact trust.
	if decision := p.Evaluate(KindSMS, "+8613800000011", "您好"); decision.Allow {
		t.Fatalf("owner block overridden by contact trust: %+v", decision)
	}
	// Contact trust wins over keyword rules.
	if decision := p.Evaluate(KindSMS, "+8613800000013", "开发票"); !decision.Allow || decision.Reason != "contact" {
		t.Fatalf("contact trust did not apply: %+v", decision)
	}
	// Explicit trust wins over everything.
	if decision := p.Evaluate(KindSMS, "+8613800000012", "发票"); !decision.Allow || decision.Reason != "trusted" {
		t.Fatalf("trust entry precedence broken: %+v", decision)
	}
	// Without the switch the contact is not trusted.
	p.ContactWhitelist = false
	if decision := p.Evaluate(KindSMS, "+8613800000013", "开发票"); decision.Allow {
		t.Fatalf("contact trust applied while disabled: %+v", decision)
	}
}
