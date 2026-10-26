package contacts

import "testing"

func parsedCard(name string, phone string) ParsedContact {
	return ParsedContact{
		CardIndex: 0, DisplayName: name, GivenName: name,
		Phones: []Field{{Value: phone}},
	}
}

func TestBuildPlanRepeatedImportMergesWithoutDuplicates(t *testing.T) {
	existing := []Contact{FromParsedForTest("ct_1", parsedCard("张三", "+86 138 0013 8000"))}
	plan := BuildPlan(existing, []ParsedContact{parsedCard("张三", "13800138000")}, nil)
	if len(plan.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(plan.Entries))
	}
	entry := plan.Entries[0]
	if entry.Action != ActionUnchanged {
		t.Fatalf("repeated import must be unchanged, got %s (%v)", entry.Action, entry.Changes)
	}
	if len(entry.Contact.Phones) != 1 {
		t.Fatalf("number duplicated: %+v", entry.Contact.Phones)
	}
}

func TestBuildPlanUpgradesBetterNameAndKeepsCustomFields(t *testing.T) {
	existing := []Contact{{
		ID: "ct_1", DisplayName: "张三", Organization: "原单位", Note: "老备注", SearchKey: "zhangsan",
		Phones: []Field{{Label: "CELL", Value: "+86 138 0013 8000"}},
	}}
	card := ParsedContact{
		DisplayName: "张三 工程师", GivenName: "三", FamilyName: "张", Nickname: "老张",
		Organization: "新单位",
		Phones:       []Field{{Label: "WORK", Value: "13800138000"}, {Label: "HOME", Value: "010-82828888"}},
	}
	plan := BuildPlan(existing, []ParsedContact{card}, nil)
	entry := plan.Entries[0]
	if entry.Action != ActionUpdate {
		t.Fatalf("want update, got %s", entry.Action)
	}
	if entry.Contact.Note != "老备注" {
		t.Fatalf("custom note lost: %q", entry.Contact.Note)
	}
	if entry.Contact.Organization != "原单位" {
		t.Fatalf("existing organization overwritten: %q", entry.Contact.Organization)
	}
	if entry.Contact.SearchKey != "zhangsan" {
		t.Fatalf("search key lost: %q", entry.Contact.SearchKey)
	}
	if entry.Contact.Nickname != "老张" {
		t.Fatalf("nickname not adopted: %q", entry.Contact.Nickname)
	}
	if len(entry.Contact.Phones) != 2 {
		t.Fatalf("phone union wrong: %+v", entry.Contact.Phones)
	}
	found := false
	for _, c := range entry.Conflicts {
		if c == "单位不一致，保留原值" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected minimal conflict note, got %v", entry.Conflicts)
	}
}

func TestBuildPlanDoesNotMergeSameNameDifferentNumber(t *testing.T) {
	existing := []Contact{FromParsedForTest("ct_1", parsedCard("李四", "13900139000"))}
	plan := BuildPlan(existing, []ParsedContact{parsedCard("李四", "13700137000")}, nil)
	if plan.Entries[0].Action != ActionCreate {
		t.Fatalf("different people with the same name must stay separate, got %s", plan.Entries[0].Action)
	}
}

func TestBuildPlanSameNameNoNumbersMerges(t *testing.T) {
	existing := []Contact{{ID: "ct_1", DisplayName: "王五"}}
	card := ParsedContact{DisplayName: "王五", Organization: "某公司"}
	plan := BuildPlan(existing, []ParsedContact{card}, nil)
	if plan.Entries[0].Action != ActionUpdate {
		t.Fatalf("no-number duplicate import should merge, got %s", plan.Entries[0].Action)
	}
	if plan.Entries[0].Contact.Organization != "某公司" {
		t.Fatalf("organization not filled: %+v", plan.Entries[0].Contact)
	}
}

func TestBuildPlanDuplicateWithinOneFile(t *testing.T) {
	plan := BuildPlan(nil, []ParsedContact{
		parsedCard("赵六", "+8613600136000"),
		parsedCard("赵六", "13600136000"),
	}, nil)
	if len(plan.Entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(plan.Entries))
	}
	if plan.Entries[0].Action != ActionCreate {
		t.Fatalf("first should create, got %s", plan.Entries[0].Action)
	}
	if plan.Entries[1].Action != ActionUnchanged {
		t.Fatalf("second duplicate should collapse, got %s (%v)", plan.Entries[1].Action, plan.Entries[1].Changes)
	}
}

func TestBuildPlanEmailMergeAndSkipInvalid(t *testing.T) {
	existing := []Contact{{ID: "ct_1", DisplayName: "Someone", Emails: []Field{{Value: "A@Example.com"}}}}
	card := ParsedContact{DisplayName: "Someone Else", Emails: []Field{{Value: "a@example.com"}}}
	plan := BuildPlan(existing, []ParsedContact{card, {}}, nil)
	if plan.Entries[0].Action != ActionUnchanged {
		t.Fatalf("email match should merge, got %s", plan.Entries[0].Action)
	}
	if plan.Skipped != 1 {
		t.Fatalf("invalid card should be skipped, got %d", plan.Skipped)
	}
}

// FromParsedForTest is a helper that assigns an id to a parsed card for this
// package's tests only.
func FromParsedForTest(id string, card ParsedContact) Contact {
	contact := contactFromParsed(card)
	contact.ID = id
	return contact
}

// Chinese contact cards exported by a phone set to "given name, family name"
// carry FN: "黛茜 樊" while N holds family=樊 given=黛茜. The list/export
// label must read surname-first with no separator, and it must stay
// byte-identical for western names and for account-style labels whose
// spaces are not a given/family split.
func TestPreferredDisplayNameOrdersHanNamesSurnameFirst(t *testing.T) {
	cases := []struct {
		stored, given, family, want string
	}{
		{"黛茜 樊", "黛茜", "樊", "樊黛茜"},
		{" 昊   曹 ", "昊", "曹", "曹昊"},
		{"樊黛茜", "黛茜", "樊", "樊黛茜"},                     // already surname-first: idempotent
		{"Alice Wang", "Alice", "Wang", "Alice Wang"}, // western order untouched
		{"Noora 崔", "Noora", "崔", "Noora 崔"},          // latin given name stays
		{"世界 莫名奇妙物语", "世界", "莫名奇妙物语", "世界 莫名奇妙物语"},    // not a surname
		{"", "黛茜", "樊", ""},
	}
	for _, c := range cases {
		if got := preferredDisplayName(c.stored, c.given, c.family); got != c.want {
			t.Errorf("preferredDisplayName(%q,%q,%q) = %q, want %q",
				c.stored, c.given, c.family, got, c.want)
		}
	}
	// contactLabel keeps the composed/org/phone fallbacks when no label exists.
	label := contactLabel(Contact{DisplayName: "黛茜 樊", GivenName: "黛茜", FamilyName: "樊"})
	if label != "樊黛茜" {
		t.Fatalf("contactLabel = %q, want 樊黛茜", label)
	}
}
