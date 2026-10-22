package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/unified/store"

	"github.com/gorilla/websocket"
)

// putNumber performs an authenticated PUT of the line own number and returns
// the decoded JSON view (or error body).
func (s *testStack) putNumber(t *testing.T, token, lineID, number string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"phoneNumber": number})
	status, payload := s.request(http.MethodPut, "/api/v2/lines/"+lineID+"/number", token, "", body)
	var view map[string]any
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &view); err != nil {
			t.Fatalf("decode number response: %v (%s)", err, payload)
		}
	}
	return status, view
}

// grantManageNumber flips the explicit, off-by-default capability on an
// enrollment key without changing any line grants.
func (s *testStack) grantManageNumber(t *testing.T, keyID string, allowed bool) {
	t.Helper()
	if err := s.auth.SetKeyManageNumber(context.Background(), keyID, allowed); err != nil {
		t.Fatalf("set manage number: %v", err)
	}
}

// scopedKey creates an all-lines=false key and grants it exactly the given
// lines (all four call/SMS flags). It lets tests build keys that must not see
// other lines.
func (s *testStack) scopedKey(t *testing.T, lineIDs ...string) (string, string) {
	t.Helper()
	keyID, secret, err := s.auth.CreateKey(context.Background(), "scoped", false)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	for _, lineID := range lineIDs {
		if err := s.store.SetKeyLine(context.Background(), store.KeyLine{
			KeyID: keyID, LineID: lineID,
			ReceiveSMS: true, ReceiveCalls: true, SendSMS: true, Dial: true,
		}); err != nil {
			t.Fatalf("grant %s: %v", lineID, err)
		}
	}
	return keyID, secret
}

// TestDeviceSetOwnNumberAuthorized proves the explicitly granted capability
// path: set (normalized), persistence/source, cross-device visibility, reset
// to auto, audit hygiene and the canManageNumber flag. All numbers synthetic.
func TestDeviceSetOwnNumberAuthorized(t *testing.T) {
	stack := newTestStack(t)
	keyA, secretA := stack.scopedKey(t, "line1")
	tokenA, _ := stack.enroll(keyA, secretA, "owner phone")
	keyB, secretB := stack.scopedKey(t, "line1")
	tokenB, _ := stack.enroll(keyB, secretB, "family phone")
	keyC, secretC := stack.scopedKey(t, "line2")
	tokenC, _ := stack.enroll(keyC, secretC, "line2 phone")

	// Without the capability even the line-granted owner gets 403.
	status, _ := stack.putNumber(t, tokenA, "line1", "15550001111")
	if status != http.StatusForbidden {
		t.Fatalf("set before capability status = %d, want 403", status)
	}
	// Ordinary line views report canManageNumber=false.
	if line := findLine(stack.getLines(t, tokenA), "line1"); line["canManageNumber"] != false {
		t.Fatalf("default canManageNumber = %v, want false", line["canManageNumber"])
	}

	stack.grantManageNumber(t, keyA, true)

	// Human separators are normalized into one canonical stored shape.
	status, view := stack.putNumber(t, tokenA, "line1", " +1 (555) 000-1111 ")
	if status != http.StatusOK {
		t.Fatalf("authorized set status = %d, want 200", status)
	}
	if got := view["phoneNumber"]; got != "+15550001111" {
		t.Fatalf("normalized number = %v, want +15550001111", got)
	}
	identity, _ := view["identity"].(map[string]any)
	if identity["numberSource"] != "manual" {
		t.Fatalf("source = %v, want manual", identity["numberSource"])
	}
	if identity["phoneMasked"] != "155****1111" {
		t.Fatalf("masked = %v, want 155****1111", identity["phoneMasked"])
	}
	if view["canManageNumber"] != true {
		t.Fatalf("response canManageNumber = %v, want true", view["canManageNumber"])
	}

	row, err := stack.store.GetLineIdentity(context.Background(), "line1")
	if err != nil {
		t.Fatalf("get identity: %v", err)
	}
	if row.ManualNumber != "+15550001111" || row.NumberSource != "manual" {
		t.Fatalf("persisted row = %+v", row)
	}

	// Another authorized phone re-fetching lines sees the shared update, and
	// its key still cannot edit (canManageNumber false).
	lineB := findLine(stack.getLines(t, tokenB), "line1")
	if lineB == nil || lineB["phoneNumber"] != "+15550001111" {
		t.Fatalf("other authorized phone must see updated number: %#v", lineB)
	}
	if lineB["canManageNumber"] != false {
		t.Fatalf("other phone must not gain the capability: %#v", lineB)
	}
	// A phone authorized only on line2 sees neither line1 nor its number.
	if line := findLine(stack.getLines(t, tokenC), "line1"); line != nil {
		t.Fatalf("line2-only phone must not see line1: %#v", line)
	}

	// Audit rows carry target/result only, never number material.
	entries, err := stack.store.ListAudit(context.Background(), 30)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, entry := range entries {
		if entry.Action == "line.number.manual" {
			if entry.Result != "set" && entry.Result != "cleared" {
				t.Fatalf("unexpected audit result %q", entry.Result)
			}
			if strings.Contains(entry.Target, "555") {
				t.Fatalf("audit target leaked number: %q", entry.Target)
			}
		}
	}

	// Reset to auto clears the override. No SIM number provisioned in the
	// fake, so source returns to none and the number disappears for everyone.
	status, view = stack.putNumber(t, tokenA, "line1", "")
	if status != http.StatusOK {
		t.Fatalf("reset status = %d", status)
	}
	identity, _ = view["identity"].(map[string]any)
	if identity["numberSource"] != "none" {
		t.Fatalf("source after reset = %v, want none", identity["numberSource"])
	}
	if got, _ := view["phoneNumber"].(string); got != "" {
		t.Fatalf("phoneNumber after reset = %v, want empty", got)
	}
	row, _ = stack.store.GetLineIdentity(context.Background(), "line1")
	if row.ManualNumber != "" {
		t.Fatalf("manual override persisted after reset: %q", row.ManualNumber)
	}
}

// TestDeviceSetOwnNumberValidationAndScope covers invalid input, missing
// grants, the shared-key rule, anonymous access and device revocation.
func TestDeviceSetOwnNumberValidationAndScope(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true) // all-lines key
	token, _ := stack.enroll(keyID, secret, "manager")
	stack.grantManageNumber(t, keyID, true)

	cases := []struct {
		name   string
		number string
	}{
		{"too short", "12"},
		{"supplementary service", "*#31#"},
		{"star in number", "1555000*111"},
		{"hash suffix", "15550001111#"},
		{"letters", "1555000abcd"},
		{"too long", "123456789012345678901"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, payload := stack.putNumber(t, token, "line1", tc.number)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			if code, _ := payload["code"].(string); code != "CB-V2-400" {
				t.Fatalf("code = %v, want CB-V2-400", payload["code"])
			}
		})
	}
	row, _ := stack.store.GetLineIdentity(context.Background(), "line1")
	if row.ManualNumber != "" {
		t.Fatalf("invalid input changed stored number: %q", row.ManualNumber)
	}

	// Scoped key with the capability may only edit a line it is granted.
	offKey, offSecret := stack.scopedKey(t, "line1")
	offToken, _ := stack.enroll(offKey, offSecret, "scoped")
	stack.grantManageNumber(t, offKey, true)
	if status, _ := stack.putNumber(t, offToken, "line2", "15550002222"); status != http.StatusForbidden {
		t.Fatalf("off-line edit status = %d, want 403", status)
	}
	if row, _ := stack.store.GetLineIdentity(context.Background(), "line2"); row.ManualNumber != "" {
		t.Fatalf("unauthorized line changed: %q", row.ManualNumber)
	}

	// Full-access shared key WITHOUT the capability cannot edit.
	plainKey, plainSecret := stack.createKey(true)
	plainToken, _ := stack.enroll(plainKey, plainSecret, "shared")
	if status, _ := stack.putNumber(t, plainToken, "line1", "15550001111"); status != http.StatusForbidden {
		t.Fatalf("shared key edit status = %d, want 403", status)
	}

	// Receive-only key (no dial/send) without capability: 403. The number
	// management capability is independent of the four call/SMS flags, so
	// after an explicit grant it is allowed.
	recvKey, recvSecret, _ := stack.auth.CreateKey(context.Background(), "recv", false)
	if err := stack.store.SetKeyLine(context.Background(), store.KeyLine{
		KeyID: recvKey, LineID: "line1",
		ReceiveSMS: true, ReceiveCalls: true, SendSMS: false, Dial: false,
	}); err != nil {
		t.Fatalf("set receive-only grant: %v", err)
	}
	recvToken, _ := stack.enroll(recvKey, recvSecret, "receive only")
	if status, _ := stack.putNumber(t, recvToken, "line1", "15550001111"); status != http.StatusForbidden {
		t.Fatalf("receive-only without capability status = %d, want 403", status)
	}
	stack.grantManageNumber(t, recvKey, true)
	if status, _ := stack.putNumber(t, recvToken, "line1", "15550001111"); status != http.StatusOK {
		t.Fatalf("receive-only with explicit capability status = %d, want 200", status)
	}

	// Anonymous request rejected.
	req, _ := http.NewRequest(http.MethodPut, stack.server.URL+"/api/v2/lines/line1/number",
		bytes.NewReader([]byte(`{"phoneNumber":"15550001111"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("anonymous do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", resp.StatusCode)
	}

	// Revoking the device closes the path immediately (DeviceAccess ErrNotFound).
	devices, err := stack.store.ListDevices(context.Background())
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	var managerDevice string
	for _, device := range devices {
		if device.Name == "manager" {
			managerDevice = device.ID
		}
	}
	if managerDevice == "" {
		t.Fatal("manager device not found")
	}
	if err := stack.auth.RevokeDevice(context.Background(), managerDevice); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if status, _ := stack.putNumber(t, token, "line1", "15550003333"); status != http.StatusUnauthorized {
		t.Fatalf("revoked device status = %d, want 401", status)
	}
}

// TestLineNumberChangeFansOut proves a line.updated event reaches another
// authorized device and never contains the number.
func TestLineNumberChangeFansOut(t *testing.T) {
	stack := newTestStack(t)
	cursor, err := stack.store.LatestEventSeq(context.Background())
	if err != nil {
		t.Fatalf("latest seq: %v", err)
	}
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "editor")
	stack.grantManageNumber(t, keyID, true)

	header := http.Header{"Authorization": []string{"Bearer " + token}}
	wsURL := "ws" + stack.server.URL[len("http"):] + "/api/v2/events?after=" + fmt.Sprint(cursor)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		t.Fatalf("events dial: %v", err)
	}
	defer conn.Close()

	if _, view := stack.putNumber(t, token, "line1", "15550001111"); view == nil {
		t.Fatal("expected response view")
	}

	deadline := time.Now().Add(3 * time.Second)
	sawUpdate := false
	for time.Now().Before(deadline) && !sawUpdate {
		_ = conn.SetReadDeadline(time.Now().Add(time.Until(deadline)))
		_, payload, readErr := conn.ReadMessage()
		if readErr != nil {
			break
		}
		if bytes.Contains(payload, []byte("555")) {
			t.Fatalf("event must not contain number material: %s", payload)
		}
		var event map[string]any
		if json.Unmarshal(payload, &event) != nil {
			continue
		}
		if event["type"] == "line.updated" {
			data, _ := event["data"].(map[string]any)
			if data["id"] == "line1" {
				sawUpdate = true
			}
		}
	}
	if !sawUpdate {
		t.Fatal("did not receive a number-free line.updated event")
	}
}
