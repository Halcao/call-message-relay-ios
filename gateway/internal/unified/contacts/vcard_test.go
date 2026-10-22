package contacts

import (
	"strings"
	"testing"
)

func TestParseVCardsChineseMultiTEL(t *testing.T) {
	card := "BEGIN:VCARD\r\n" +
		"VERSION:3.0\r\n" +
		"FN:张三\r\n" +
		"N:张;三;;;\r\n" +
		"ORG:北京示例科技有限公司;研发部\r\n" +
		"TEL;TYPE=CELL:+86 138 0013 8000\r\n" +
		"TEL;TYPE=WORK:010-82828888\r\n" +
		"EMAIL;TYPE=WORK:zhangsan@example.com\r\n" +
		"NOTE:备注\\,含逗号\\;和分号\r\n" +
		"END:VCARD\r\n"
	parsed, warnings, err := ParseVCards([]byte(card))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(parsed) != 1 {
		t.Fatalf("want 1 contact, got %d", len(parsed))
	}
	got := parsed[0]
	if got.DisplayName != "张三" || got.FamilyName != "张" || got.GivenName != "三" {
		t.Fatalf("name parsed wrong: %+v", got)
	}
	if got.Organization != "北京示例科技有限公司" {
		t.Fatalf("org parsed wrong: %q", got.Organization)
	}
	if len(got.Phones) != 2 {
		t.Fatalf("want 2 phones, got %d", len(got.Phones))
	}
	if got.Phones[0].Label != "CELL" || got.Phones[1].Label != "WORK" {
		t.Fatalf("labels wrong: %+v", got.Phones)
	}
	if got.Note != "备注,含逗号;和分号" {
		t.Fatalf("escapes not resolved: %q", got.Note)
	}
}

func TestParseVCardsFoldedAndEscaped(t *testing.T) {
	card := "BEGIN:VCARD\n" +
		"VERSION:3.0\n" +
		"FN:李\n" +
		" 四\n" + // folded FN: 李四
		"N:李;四;;;\n" +
		"ORG:折叠\n" +
		" 公司\n" + // folded ORG
		"TEL;TYPE=\"WORK,VOICE\":+8613900139000\n" +
		"END:VCARD\n"
	parsed, _, err := ParseVCards([]byte(card))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("want 1 contact, got %d", len(parsed))
	}
	if parsed[0].DisplayName != "李四" {
		t.Fatalf("folded FN wrong: %q", parsed[0].DisplayName)
	}
	if parsed[0].Organization != "折叠公司" {
		t.Fatalf("folded ORG wrong: %q", parsed[0].Organization)
	}
	if parsed[0].Phones[0].Label != "WORK" {
		t.Fatalf("quoted TYPE wrong: %+v", parsed[0].Phones[0])
	}
}

func TestParseVCardsQuotedPrintable(t *testing.T) {
	card := "BEGIN:VCARD\r\n" +
		"VERSION:2.1\r\n" +
		"FN;CHARSET=UTF-8;ENCODING=QUOTED-PRINTABLE:=E5=BC=A0=E4=B8=89\r\n" +
		"N;CHARSET=UTF-8;ENCODING=QUOTED-PRINTABLE:=E5=BC=A0;=E4=B8=89;;;\r\n" +
		"TEL;CELL:13900139000\r\n" +
		"END:VCARD\r\n"
	parsed, _, err := ParseVCards([]byte(card))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("want 1 contact, got %d", len(parsed))
	}
	if parsed[0].DisplayName != "张三" {
		t.Fatalf("QP FN wrong: %q", parsed[0].DisplayName)
	}
	if parsed[0].FamilyName != "张" || parsed[0].GivenName != "三" {
		t.Fatalf("QP N wrong: %+v", parsed[0])
	}
	if parsed[0].Version != "2.1" || parsed[0].Phones[0].Label != "CELL" {
		t.Fatalf("v2.1 shape wrong: %+v", parsed[0])
	}
}

func TestParseVCardsV4URIAndMultipleCards(t *testing.T) {
	stream := strings.Join([]string{
		"BEGIN:VCARD",
		"VERSION:4.0",
		"FN:Alice Wang",
		"N:Wang;Alice;;;",
		"TEL;TYPE=cell;VALUE=uri:tel:+1-873-555-0100",
		"EMAIL:alice@example.com",
		"END:VCARD",
		"BEGIN:VCARD",
		"VERSION:3.0",
		"FN:Bob 王",
		"N:王;Bob;;;",
		"TEL;TYPE=HOME:5551234",
		"END:VCARD",
	}, "\n")
	parsed, _, err := ParseVCards([]byte(stream))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 2 {
		t.Fatalf("want 2 contacts, got %d", len(parsed))
	}
	if parsed[0].Phones[0].Value != "+1-873-555-0100" {
		t.Fatalf("tel URI not unwrapped: %q", parsed[0].Phones[0].Value)
	}
	if parsed[1].DisplayName != "Bob 王" {
		t.Fatalf("second card wrong: %+v", parsed[1])
	}
}

func TestParseVCardsSkipsEmptyAndWarns(t *testing.T) {
	card := "BEGIN:VCARD\nVERSION:3.0\nNOTE:only a note\nEND:VCARD\n"
	parsed, warnings, err := ParseVCards([]byte(card))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 0 {
		t.Fatalf("card without name or number must be skipped, got %+v", parsed)
	}
	if len(warnings) == 0 {
		t.Fatal("expected a warning for the skipped card")
	}
}

func TestParseVCardsNumberOnlyCardIsKept(t *testing.T) {
	card := "BEGIN:VCARD\nVERSION:3.0\nTEL:12345\nEND:VCARD\n"
	parsed, _, err := ParseVCards([]byte(card))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed) != 1 || len(parsed[0].Phones) != 1 {
		t.Fatalf("number-only card should import with its number: %+v", parsed)
	}
}

func TestParseVCardsRejectsOversize(t *testing.T) {
	big := make([]byte, MaxImportBytes+1)
	if _, _, err := ParseVCards(big); err == nil {
		t.Fatal("expected oversize error")
	}
}

func TestParseVCardsCardLimit(t *testing.T) {
	var builder strings.Builder
	for i := 0; i < MaxCards+1; i++ {
		builder.WriteString("BEGIN:VCARD\nVERSION:3.0\nFN:N\nEND:VCARD\n")
	}
	if _, _, err := ParseVCards([]byte(builder.String())); err == nil {
		t.Fatal("expected card limit error")
	}
}

func TestCanonicalPhoneSpellings(t *testing.T) {
	for _, pair := range [][2]string{
		{"+86 138 0013 8000", "13800138000"},
		{"008613800138000", "13800138000"},
		{"8613800138000", "13800138000"},
		{"+1 (873) 555-0100", "+1-873-555-0100"},
	} {
		if !SamePhone(pair[0], pair[1]) {
			t.Errorf("expected %q and %q to match", pair[0], pair[1])
		}
	}
	if SamePhone("13800138000", "13800138001") {
		t.Fatal("different numbers must not match")
	}
	if SamePhone("", "13800138000") {
		t.Fatal("empty number must not match")
	}
	// An explicit +1 number must never gain +86 aliases just because its digit
	// shape is 11 digits long.
	for _, key := range CanonicalKeys("+1 873 555 0100") {
		if key != "18735550100" {
			t.Fatalf("unexpected alias for +1 number: %q", key)
		}
	}
}
