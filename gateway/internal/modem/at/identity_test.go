package at

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// identityPort answers the bounded identity command set from a script.
type identityPort struct {
	mu       sync.Mutex
	responses map[string]string
	writes   []string
	pending  bytes.Buffer
}

func (p *identityPort) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	command := strings.TrimSpace(string(data))
	p.writes = append(p.writes, command)
	for prefix, response := range p.responses {
		if strings.HasPrefix(command, prefix) {
			p.pending.WriteString(response)
			break
		}
	}
	if _, ok := p.responses[command]; !ok {
		// Only respond to explicitly scripted commands.
	}
	return len(data), nil
}

func (p *identityPort) Read(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending.Len() == 0 {
		return 0, io.EOF
	}
	return p.pending.Read(data)
}

func (p *identityPort) Close() error { return nil }

func TestAdapterOwnIdentity(t *testing.T) {
	port := &identityPort{responses: map[string]string{
		"AT+CNUM":  "\r\n+CNUM: ,\"+15550001111\",145\r\nOK\r\n",
		"AT+CCID":  "\r\n+CCID: 0123456789ABCDEF0123\r\nOK\r\n",
		"AT+CIMI":  "\r\n001012000011111\r\nOK\r\n",
		"AT+COPS?": "\r\n+COPS: 0,0,\"Test Operator\",7\r\nOK\r\n",
		"AT+CEREG?": "\r\n+CEREG: 0,1,\"04D2\",\"000486A9\",7\r\nOK\r\n",
		`AT+QENG="servingcell"`: "\r\n+QENG: \"servingcell\",\"NOCONN\",\"LTE\",\"FDD\",001,01,296649,123,1650,3,1,1,1234\r\nOK\r\n",
	}}
	adapter := NewAdapter(NewClient(port))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	identity, err := adapter.OwnIdentity(ctx)
	if err != nil {
		t.Fatalf("OwnIdentity: %v", err)
	}
	if identity.Number != "+15550001111" || identity.NumberSource != "sim" {
		t.Fatalf("number = %q source=%q", identity.Number, identity.NumberSource)
	}
	if identity.ICCID != "0123456789ABCDEF0123" || identity.IMSI != "001012000011111" {
		t.Fatalf("ids = %q/%q", identity.ICCID, identity.IMSI)
	}
	if identity.OperatorAlpha != "Test Operator" || identity.Registration != "registered_home" {
		t.Fatalf("operator=%q reg=%q", identity.OperatorAlpha, identity.Registration)
	}
	if identity.AccessTech != "lte" || identity.TAC != "1234" {
		t.Fatalf("cell = %#v", identity)
	}
	if identity.Commands["AT+CNUM"] != "ok" {
		t.Fatalf("commands = %#v", identity.Commands)
	}
}

func TestAdapterOwnIdentityEmptySIM(t *testing.T) {
	port := &identityPort{responses: map[string]string{
		"AT+CNUM": "\r\nOK\r\n", // empty record, like line2
		"AT+CCID": "\r\n+CCID: 0123456789ABCDEF0124\r\nOK\r\n",
		"AT+CIMI": "\r\n001012000022222\r\nOK\r\n",
	}}
	adapter := NewAdapter(NewClient(port))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	identity, err := adapter.OwnIdentity(ctx)
	if err != nil {
		t.Fatalf("OwnIdentity: %v", err)
	}
	if identity.Number != "" || identity.NumberSource != "empty" {
		t.Fatalf("empty SIM = %q/%q", identity.Number, identity.NumberSource)
	}
	if identity.Commands["AT+CNUM"] != "empty" {
		t.Fatalf("cnum status = %q", identity.Commands["AT+CNUM"])
	}
}

func TestParseCNUMObservedShape(t *testing.T) {
	// Synthetic stand-in for the observed empty-unquoted-alpha shape.
	got := ParseCNUM("\r\n+CNUM: ,\"+15550001111\",145\r\n\r\nOK\r\n")
	if !got.OK || !got.Found() {
		t.Fatalf("expected ok+found, got %#v", got)
	}
	if got.Number() != "+15550001111" {
		t.Fatalf("number = %q", got.Number())
	}
	if !got.Entries[0].International || got.Entries[0].TypeOfAddress != 145 {
		t.Fatalf("entry = %#v", got.Entries[0])
	}
}

func TestParseCNUMNationalKeptVerbatim(t *testing.T) {
	got := ParseCNUM("+CNUM: \"Line\",\"13800138000\",145\r\nOK\r\n")
	if !got.Found() || got.Number() != "13800138000" {
		t.Fatalf("national number = %#v", got)
	}
	if got.Entries[0].International {
		t.Fatalf("national number must not be marked international")
	}
}

func TestParseCNUMEmptyErrorTruncatedControl(t *testing.T) {
	if got := ParseCNUM("\r\nOK\r\n"); !got.OK || got.Found() {
		t.Fatalf("empty SIM record should be ok/found=false, got %#v", got)
	}
	if got := ParseCNUM("ERROR\r\n"); !got.Error || got.Found() {
		t.Fatalf("error response, got %#v", got)
	}
	if got := ParseCNUM("+CNUM: ,\"+15550001111\",145\r\nERROR\r\n"); got.Found() || !got.Error {
		t.Fatalf("error must not expose partial entries, got %#v", got)
	}
	if got := ParseCNUM("AT+CNUM\r\n"); !got.Incomplete || got.OK {
		t.Fatalf("echo-only must be incomplete, got %#v", got)
	}
	if got := ParseCNUM("+CNUM: ,\"+15550001111\",145\r\n"); !got.Incomplete || got.Found() {
		t.Fatalf("truncated must be incomplete with no entries, got %#v", got)
	}
	for _, ctrl := range []string{"*#31#", "#31#", "*31*1#"} {
		if got := ParseCNUM("+CNUM: ,\"" + ctrl + "\",145\r\nOK\r\n"); got.Found() {
			t.Fatalf("control string %q parsed as number: %#v", ctrl, got)
		}
	}
}

func TestParseCNUMMultipleEntries(t *testing.T) {
	got := ParseCNUM("+CNUM: ,\"+15550001111\",145\r\n+CNUM: \"x\",\"13900139000\",128\r\nOK\r\n")
	if len(got.Entries) != 2 || got.Number() != "+15550001111" {
		t.Fatalf("entries = %#v", got.Entries)
	}
}

func TestParseCCIDShapes(t *testing.T) {
	cases := map[string]string{
		"+CCID: 0123456789ABCDEF0123\r\nOK\r\n":       "0123456789ABCDEF0123",
		"+CCID: \"1234567890123456789\"\r\nOK\r\n":   "1234567890123456789",
		"1234567890123456789\r\nOK\r\n":              "1234567890123456789",
		"+CCID: bogus\r\nOK\r\n":                     "",
		"+CCID: ABCDEF\r\nOK\r\n":                    "",
		"ERROR\r\n":                                  "",
	}
	for transcript, want := range cases {
		if got := ParseCCID(transcript); got != want {
			t.Fatalf("ParseCCID(%q) = %q, want %q", transcript, got, want)
		}
	}
}

func TestParseCOPSAndRegistration(t *testing.T) {
	numeric, alpha := ParseCOPS([]string{"+COPS: 0,0,\"Test Operator\",7"})
	if numeric != "" || alpha != "Test Operator" {
		t.Fatalf("cops = %q/%q", numeric, alpha)
	}
	numeric, _ = ParseCOPS([]string{"+COPS: 0,2,\"00101\",7"})
	if numeric != "00101" {
		t.Fatalf("numeric operator = %q", numeric)
	}
	reg, lac, _, cell := ParseRegistration([]string{"+CREG: 0,1,\"1234\",\"5678\""}, false)
	if reg != "registered_home" || strings.ToUpper(lac) != "1234" || strings.ToUpper(cell) != "5678" {
		t.Fatalf("cereg = %q %q %q", reg, lac, cell)
	}
	reg, _, tac, cell := ParseRegistration([]string{"+CEREG: 0,5,\"04D2\",\"000486A9\""}, true)
	if reg != "registered_roaming" || strings.ToUpper(tac) != "04D2" || strings.ToUpper(cell) != "000486A9" {
		t.Fatalf("lte reg = %q tac=%q cell=%q", reg, tac, cell)
	}
}

func TestParseServingCell(t *testing.T) {
	meta := ParseServingCell([]string{
		`+QENG: "servingcell","NOCONN","LTE","FDD",001,01,296649,123,1650,3,1,1,1234`,
	})
	if meta.AccessTech != "lte" || meta.CellID != "296649" || meta.PCI != 123 || meta.EARFCN != 1650 || meta.TAC != "1234" || meta.OperatorNumeric != "00101" {
		t.Fatalf("lte cell = %#v", meta)
	}
	meta = ParseServingCell([]string{
		`+QENG: "servingcell","NOCONN","GSM",001,01,1234,5678`,
	})
	if meta.AccessTech != "gsm" || meta.LAC != "1234" || meta.CellID != "5678" {
		t.Fatalf("gsm cell = %#v", meta)
	}
	if got := ParseServingCell([]string{"ERROR"}); got.CellID != "" {
		t.Fatalf("error response parsed: %#v", got)
	}
}

func TestParseCLIPShapes(t *testing.T) {
	cases := []struct {
		line   string
		peer   string
		isClip bool
	}{
		{`+CLIP: "18712345678",161`, "18712345678", true},
		{`+CLIP: "18712345678",161,,,,"Zhang"`, "18712345678", true},
		{`+CLIP: "+8618712345678",145`, "+8618712345678", true},
		{`+CLIP: 18712345678,129`, "18712345678", true},
		{`+clip: "18712345678",129`, "18712345678", true},
		{`+CLIP: "",129`, "", true},
		{`+CLIP: ,129`, "", true},
		{`+CLIP: "123",129`, "123", true},
		{`+CLIP: "abc",129`, "", true},
		{`+CLIP: "*31#18712345678",129`, "", true},
		{`+CLIP: "12345678901234567890123",129`, "", true},
		{`RING`, "", false},
		{`+CLCC: 1,1,4,0,0,"18712345678",129`, "", false},
	}
	for _, test := range cases {
		peer, isClip := ParseCLIP(test.line)
		if peer != test.peer || isClip != test.isClip {
			t.Fatalf("ParseCLIP(%q) = %q, %v; want %q, %v", test.line, peer, isClip, test.peer, test.isClip)
		}
	}
}
