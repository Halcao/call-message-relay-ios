// Command callrelay-probe performs a real enrollment against a running
// callrelayd through its public HTTPS entry point and reports the sanitized
// contract that the iOS client depends on:
//
//   - /api/v2/lines response shape and per-line dialability,
//   - /api/v2/calls?active=true reconciliation endpoint,
//   - PUT /api/v2/devices/{id}/preferences default-line behavior,
//   - durable /api/v2/events replay on a fresh connect versus
//     ?after=<seq> resume (the phantom-ringing transport contract).
//
// It never prints the enrollment key, bearer tokens, full phone numbers or
// device ids. The caller passes the enrollment key through the
// CALLRELAY_PROBE_KEY environment variable so it never appears in argv or in
// shell history; the key must be revoked immediately after the probe.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	base := flag.String("base", "https://gateway.example.com", "public gateway base URL")
	deviceName := flag.String("device-name", "contract-probe", "device name registered with the gateway")
	observe := flag.Int("observe", 6, "seconds to watch the fresh event replay")
	resumeObserve := flag.Int("resume-observe", 3, "seconds to watch the resumed event stream")
	dumpLines := flag.Bool("dump-lines", false, "print the raw /api/v2/lines body between markers (caller must mask numbers before storing)")
	flag.Parse()

	key := strings.TrimSpace(os.Getenv("CALLRELAY_PROBE_KEY"))
	if key == "" {
		fatal("CALLRELAY_PROBE_KEY is required (never pass the key as argv)")
	}
	baseURL := strings.TrimRight(*base, "/")
	client := &http.Client{Timeout: 20 * time.Second}

	identity := getJSON(client, baseURL+"/api/v2/identity", "")
	gatewayID, _ := identity["gatewayId"].(string)
	if gatewayID == "" {
		fatal("identity response has no gatewayId")
	}
	fmt.Printf("identity=ok gateway_id=%s api_version=%v\n", gatewayID, identity["apiVersion"])

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatal(err.Error())
	}
	keyID, secret, ok := strings.Cut(key, ".")
	if !ok {
		fatal("enrollment key must be id.secret")
	}
	name := *deviceName
	proof := ed25519.Sign(privateKey, []byte(keyID+"\n"+secret+"\n"+gatewayID+"\n"+name))
	enrollBody, _ := json.Marshal(map[string]string{
		"enrollmentKey": key, "deviceName": name,
		"devicePublicKey": base64.StdEncoding.EncodeToString(publicKey),
		"proof":           base64.StdEncoding.EncodeToString(proof),
	})
	enrolled := postJSON(client, baseURL+"/api/v2/enroll", "", enrollBody)
	accessToken, _ := enrolled["accessToken"].(string)
	deviceID, _ := enrolled["deviceId"].(string)
	if accessToken == "" || deviceID == "" {
		fatal("enroll did not return device credentials")
	}
	fmt.Printf("enrolled=ok device=%s\n", maskID(deviceID))

	linesStatus, linesPayload := do(client, http.MethodGet, baseURL+"/api/v2/lines", accessToken, nil)
	if linesStatus != http.StatusOK {
		fatal(fmt.Sprintf("GET /api/v2/lines status %d", linesStatus))
	}
	var lines []any
	if json.Unmarshal(linesPayload, &lines) != nil {
		fatal("GET /api/v2/lines returned invalid json array")
	}
	if *dumpLines {
		fmt.Printf("LINES_JSON_BEGIN\n%s\nLINES_JSON_END\n", linesPayload)
	}
	fmt.Printf("lines=%d\n", len(lines))
	dialable := ""
	for _, raw := range lines {
		line, _ := raw.(map[string]any)
		permissions, _ := line["permissions"].(map[string]any)
		identityView, _ := line["identity"].(map[string]any)
		phone, _ := line["phoneNumber"].(string)
		phoneTail := ""
		if len(phone) >= 4 {
			phoneTail = phone[len(phone)-4:]
		}
		registration, _ := line["registration"].(string)
		voice, _ := line["voice"].(string)
		online, _ := line["online"].(bool)
		enabled, _ := line["enabled"].(bool)
		dial, _ := permissions["dial"].(bool)
		canDial := enabled && online && dial && registration == "registered" &&
			(voice == "ready" || voice == "control_only" || voice == "full_voice" || voice == "control-only")
		fmt.Printf("line id=%v enabled=%v online=%v sim=%v registration=%v voice=%v sms=%v sms_live=%v active_call=%v dial=%v number_len=%d number_tail=%s source=%v can_manage_number=%v keys=%s dialable_now=%v\n",
			line["id"], enabled, online, line["sim"], registration, voice, line["sms"], line["smsLive"],
			line["activeCallId"], dial, len(phone), phoneTail, identityView["numberSource"],
			line["canManageNumber"], sortedKeys(line), canDial)
		if canDial && dialable == "" {
			if id, _ := line["id"].(string); id != "" {
				dialable = id
			}
		}
	}

	active := getJSONArray(client, baseURL+"/api/v2/calls?active=true&limit=50", accessToken)
	states := map[string]int{}
	for _, raw := range active {
		call, _ := raw.(map[string]any)
		state, _ := call["state"].(string)
		states[state]++
	}
	fmt.Printf("active_calls=%d states=%v\n", len(active), states)

	if dialable != "" {
		preferenceBody, _ := json.Marshal(map[string]string{"defaultLineId": dialable})
		status, _ := putJSON(client, baseURL+"/api/v2/devices/"+url.PathEscape(deviceID)+"/preferences", accessToken, preferenceBody)
		device := getJSON(client, baseURL+"/api/v2/device", accessToken)
		deviceView, _ := device["device"].(map[string]any)
		fmt.Printf("default_line_put_status=%d persisted=%v requested=%s\n", status, deviceView["defaultLineId"], dialable)
	}

	replay := watchEvents(baseURL, accessToken, 0, *observe)
	fmt.Printf("fresh_stream frames=%d by_type=%v call_incoming=%d first_seq=%d last_seq=%d\n",
		replay.frames, replay.byType, replay.callIncoming, replay.firstSeq, replay.lastSeq)
	if replay.lastSeq > 0 {
		resumed := watchEvents(baseURL, accessToken, replay.lastSeq, *resumeObserve)
		fmt.Printf("resumed_stream after=%d frames=%d by_type=%v call_incoming=%d first_seq=%d last_seq=%d\n",
			replay.lastSeq, resumed.frames, resumed.byType, resumed.callIncoming, resumed.firstSeq, resumed.lastSeq)
	}
	fmt.Println("probe=done")
}

type eventStats struct {
	frames       int
	byType       map[string]int
	callIncoming int
	firstSeq     int64
	lastSeq      int64
}

// watchEvents opens the authorized event websocket, optionally with a resume
// cursor, and counts sanitized event metadata only.
func watchEvents(baseURL, token string, after int64, seconds int) eventStats {
	stats := eventStats{byType: map[string]int{}}
	wsURL := strings.Replace(strings.Replace(baseURL, "https://", "wss://", 1), "http://", "ws://", 1) + "/api/v2/events"
	if after > 0 {
		wsURL += fmt.Sprintf("?after=%d", after)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	conn, response, err := dialer.Dial(wsURL, header)
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		fatal(fmt.Sprintf("events dial failed status=%d: %v", status, err))
	}
	defer conn.Close()
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	_ = conn.SetReadDeadline(deadline)
	for time.Now().Before(deadline) {
		_, payload, readErr := conn.ReadMessage()
		if readErr != nil {
			break
		}
		var envelope struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &envelope) != nil {
			continue
		}
		stats.frames++
		stats.byType[envelope.Type]++
		if envelope.Type == "call.incoming" {
			stats.callIncoming++
		}
		if stats.firstSeq == 0 || envelope.Seq < stats.firstSeq {
			stats.firstSeq = envelope.Seq
		}
		if envelope.Seq > stats.lastSeq {
			stats.lastSeq = envelope.Seq
		}
	}
	return stats
}

func getJSON(client *http.Client, endpoint, token string) map[string]any {
	status, payload := do(client, http.MethodGet, endpoint, token, nil)
	if status != http.StatusOK {
		fatal(fmt.Sprintf("GET %s status %d", endpoint, status))
	}
	var body map[string]any
	if json.Unmarshal(payload, &body) != nil {
		fatal("GET " + endpoint + " returned invalid json")
	}
	return body
}

func getJSONArray(client *http.Client, endpoint, token string) []any {
	status, payload := do(client, http.MethodGet, endpoint, token, nil)
	if status != http.StatusOK {
		fatal(fmt.Sprintf("GET %s status %d", endpoint, status))
	}
	var body []any
	if json.Unmarshal(payload, &body) != nil {
		fatal("GET " + endpoint + " returned invalid json array")
	}
	return body
}

func postJSON(client *http.Client, endpoint, token string, body []byte) map[string]any {
	status, payload := do(client, http.MethodPost, endpoint, token, body)
	if status != http.StatusOK && status != http.StatusCreated {
		fatal(fmt.Sprintf("POST %s status %d", endpoint, status))
	}
	var parsed map[string]any
	if json.Unmarshal(payload, &parsed) != nil {
		fatal("POST " + endpoint + " returned invalid json")
	}
	return parsed
}

func putJSON(client *http.Client, endpoint, token string, body []byte) (int, []byte) {
	return do(client, http.MethodPut, endpoint, token, body)
}

func do(client *http.Client, method, endpoint, token string, body []byte) (int, []byte) {
	request, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
	if err != nil {
		fatal(err.Error())
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		fatal(err.Error())
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, payload
}

func maskID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8] + "…"
}

func sortedKeys(line map[string]any) string {
	keys := make([]string, 0, len(line))
	for key := range line {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "probe error:", message)
	os.Exit(1)
}
