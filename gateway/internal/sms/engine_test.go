package sms

import (
	"context"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/db"
	"callrelay/gateway/internal/modem"
)

func assertNoMessages(t *testing.T, database *db.DB) {
	t.Helper()
	messages, err := database.ListMessages(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 {
		t.Fatalf("messages recorded = %d, want 0", len(messages))
	}
}

type fakeSMSModem struct {
	payloads []modem.SMSPayload
}

func (f *fakeSMSModem) SendSMS(_ context.Context, _ string, payload modem.SMSPayload) (modem.SMSID, error) {
	f.payloads = append(f.payloads, payload)
	return "sms-1", nil
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "cellbridge.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// encodeTestDeliverPDU builds a minimal SMS-DELIVER PDU (no SMSC address) so
// ingest tests exercise the real inbound path instead of relying on a stored
// SMS-SUBMIT copy, which the engine must now skip.
func encodeTestDeliverPDU(t *testing.T, sender, body string) []byte {
	t.Helper()
	address, err := encodeAddress(sender)
	if err != nil {
		t.Fatal(err)
	}
	septets := encodeGSM7(body)
	userData := packSeptets(septets)
	pdu := []byte{0x00, 0x00} // no SMSC; TP-MTI = SMS-DELIVER
	pdu = append(pdu, address...)
	pdu = append(pdu, 0x00, 0x00)                 // TP-PID, TP-DCS (GSM7)
	pdu = append(pdu, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00) // TP-SCTS
	pdu = append(pdu, byte(len(septets)))
	pdu = append(pdu, userData...)
	return pdu
}

func TestEngineIngestsAndDeduplicatesPDU(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(database, nil)
	engine.Now = func() time.Time { return time.Unix(100, 0) }
	raw := encodeTestDeliverPDU(t, "10086", "hello")
	message, err := engine.Ingest(context.Background(), modem.RawSMS{RawPDU: raw, ModemIndex: 1})
	if err != nil || message == nil || message.Body != "hello" {
		t.Fatalf("ingest = %#v, %v", message, err)
	}
	if message.Direction != "inbound" || message.Peer != "10086" {
		t.Fatalf("ingest direction/peer = %s/%s", message.Direction, message.Peer)
	}
	duplicate, err := engine.Ingest(context.Background(), modem.RawSMS{RawPDU: raw, ModemIndex: 1})
	if err != nil || duplicate != nil {
		t.Fatalf("duplicate = %#v, %v", duplicate, err)
	}
}

func TestEngineSkipsStoredSubmitEcho(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(database, nil)
	engine.Now = func() time.Time { return time.Unix(100, 0) }
	// A dry-run (AT+CMGW) submission lands in modem storage; the inbox poll
	// must never re-ingest it as an incoming message.
	segments, err := EncodeSubmitSegments("10086", "hello", 1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(segments[0].PDU)
	if err != nil {
		t.Fatal(err)
	}
	mti, ok := tpduMessageType(raw)
	if !ok || mti != tpMTISubmit {
		t.Fatalf("fixture TP-MTI = %d ok=%v, want SMS-SUBMIT", mti, ok)
	}
	message, err := engine.Ingest(context.Background(), modem.RawSMS{RawPDU: raw, ModemIndex: 1})
	if err != nil {
		t.Fatalf("ingest error = %v", err)
	}
	if message != nil {
		t.Fatalf("stored SMS-SUBMIT must never become a message: %#v", message)
	}
	assertNoMessages(t, database)
}

func TestEngineSkipsStatusReportPDU(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(database, nil)
	deliver := encodeTestDeliverPDU(t, "10086", "hello")
	report := append([]byte(nil), deliver...)
	report[1] = tpMTIStatusReport // TP-MTI bits only; content irrelevant for the skip
	message, err := engine.Ingest(context.Background(), modem.RawSMS{RawPDU: report, ModemIndex: 1})
	if err != nil {
		t.Fatalf("ingest error = %v", err)
	}
	if message != nil {
		t.Fatalf("status report must never become a chat message: %#v", message)
	}
	assertNoMessages(t, database)
}

// dryRunSMSModem reports store-only (AT+CMGW) submission semantics.
type dryRunSMSModem struct{ *fakeSMSModem }

func (dryRunSMSModem) DryRun() bool { return true }

func TestEngineDryRunNeverClaimsSent(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(database, dryRunSMSModem{&fakeSMSModem{}})
	message, err := engine.Send(context.Background(), "10086", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if message == nil || message.Status != "submitted" {
		t.Fatalf("dry-run send = %#v, want status submitted (never sent)", message)
	}
	var status string
	if err := database.QueryRowContext(context.Background(), "SELECT status FROM messages WHERE id = ?", message.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "submitted" {
		t.Fatalf("stored dry-run status = %q, want submitted", status)
	}
}

func TestEngineSendsLongChineseSMSAsPDU(t *testing.T) {
	database := openTestDB(t)
	modemClient := &fakeSMSModem{}
	engine := NewEngine(database, modemClient)
	message, err := engine.Send(context.Background(), "+8613800138000", strings.Repeat("测", 80))
	if err != nil || message == nil || message.Status != "sent" {
		t.Fatalf("send = %#v, %v", message, err)
	}
	if len(modemClient.payloads) != 2 || modemClient.payloads[0].PDU == "" || modemClient.payloads[0].Total != 2 {
		t.Fatalf("payloads = %#v", modemClient.payloads)
	}
}

func TestEnginePersistsSubmittedSMSWhenFinalModemResultIsUnknown(t *testing.T) {
	database := openTestDB(t)
	modemClient := &uncertainSMSModem{}
	engine := NewEngine(database, modemClient)
	message, err := engine.Send(context.Background(), "10086", "测试")
	if err != nil || message == nil || message.Status != "submitted" {
		t.Fatalf("send = %#v, %v", message, err)
	}
	var status string
	if err := database.QueryRowContext(context.Background(), "SELECT status FROM messages WHERE id = ?", message.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "submitted" {
		t.Fatalf("stored status = %q", status)
	}
}

type uncertainSMSModem struct{}

func (uncertainSMSModem) SendSMS(context.Context, string, modem.SMSPayload) (modem.SMSID, error) {
	return "", errors.Join(modem.ErrSMSSubmissionUnknown, errors.New("serial response timed out after Ctrl-Z"))
}
