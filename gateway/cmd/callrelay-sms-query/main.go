// Command callrelay-sms-query performs ONE operator-authorized, read-only
// carrier query cycle through the existing unified-gateway API:
//
//	enroll (temporary key, revoked by the operator afterwards) ->
//	POST /api/v2/messages {lineId, to, body} for each query ->
//	poll /api/v2/threads + /api/v2/messages and print the carrier replies.
//
// It is meant for balance/plan/data-allowance queries to an OFFICIAL carrier
// service number only. It never prints credential material (the key is read
// from a file and never echoed), never retries a paid/ordering service, and
// masks digit runs of 8+ characters (phone numbers, ICCIDs) in output. The
// temporary device id is written to -device-out for revocation.
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
	"os"
	"regexp"
	"strings"
	"time"
)

func main() {
	base := flag.String("base", "https://gateway.example.com", "gateway base URL")
	line := flag.String("line", "line2", "line id to send from")
	to := flag.String("to", "10086", "carrier service number")
	bodies := flag.String("bodies", "YE,CXLL", "comma-separated read-only query bodies")
	keyFile := flag.String("key-file", "", "file containing the 'key=id.secret' line")
	wait := flag.Int("wait", 90, "seconds to wait for carrier replies")
	deviceOut := flag.String("device-out", "", "write the enrolled device id here for revocation")
	listOnly := flag.Bool("list", false, "list recent threads and their messages instead of sending")
	flag.Parse()

	key, err := readKey(*keyFile)
	if err != nil {
		fatal(err.Error())
	}
	client := &http.Client{Timeout: 25 * time.Second}
	identity := getJSON(client, *base+"/api/v2/identity", "")
	gatewayID, _ := identity["gatewayId"].(string)
	if gatewayID == "" {
		fatal("identity response has no gatewayId")
	}

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fatal(err.Error())
	}
	keyID, secret, ok := strings.Cut(key, ".")
	if !ok {
		fatal("enrollment key must be id.secret")
	}
	const name = "sms-query-probe"
	proof := ed25519.Sign(privateKey, []byte(keyID+"\n"+secret+"\n"+gatewayID+"\n"+name))
	enrollBody, _ := json.Marshal(map[string]string{
		"enrollmentKey": key, "deviceName": name,
		"devicePublicKey": base64.StdEncoding.EncodeToString(publicKey),
		"proof":           base64.StdEncoding.EncodeToString(proof),
	})
	enrolled := postJSON(client, *base+"/api/v2/enroll", "", enrollBody)
	accessToken, _ := enrolled["accessToken"].(string)
	deviceID, _ := enrolled["deviceId"].(string)
	if accessToken == "" || deviceID == "" {
		fatal("enroll did not return device credentials")
	}
	if *deviceOut != "" {
		_ = os.WriteFile(*deviceOut, []byte(deviceID+"\n"), 0o600)
	}
	fmt.Printf("enrolled=ok device=%s\n", maskID(deviceID))

	if *listOnly {
		status, payload := do(client, http.MethodGet, *base+"/api/v2/threads", accessToken, nil, false)
		if status != http.StatusOK {
			fatal(fmt.Sprintf("GET /api/v2/threads status %d", status))
		}
		var threads []map[string]any
		if json.Unmarshal(payload, &threads) != nil {
			fatal("threads invalid json")
		}
		for _, thread := range threads {
			key, _ := thread["key"].(string)
			peer, _ := thread["peer"].(string)
			last, _ := thread["lastMessage"].(map[string]any)
			body, _ := last["body"].(string)
			direction, _ := last["direction"].(string)
			created, _ := last["createdAt"].(float64)
			fmt.Printf("thread key=%s peer=%s dir=%s at=%s body=%q\n",
				sanitize(key), sanitize(peer), direction,
				time.UnixMilli(int64(created)).UTC().Format(time.RFC3339), sanitize(body))
			if strings.Contains(key, *to) || peer == *to {
				status, payload := do(client, http.MethodGet,
					*base+"/api/v2/messages?threadKey="+key+"&limit=20", accessToken, nil, false)
				if status == http.StatusOK {
					var views []map[string]any
					if json.Unmarshal(payload, &views) == nil {
						for _, view := range views {
							vdir, _ := view["direction"].(string)
							vbody, _ := view["body"].(string)
							vcreated, _ := view["createdAt"].(float64)
							fmt.Printf("  msg dir=%s at=%s body=%q\n", vdir,
								time.UnixMilli(int64(vcreated)).UTC().Format(time.RFC3339), sanitize(vbody))
						}
					}
				}
			}
		}
		fmt.Println("list=done")
		return
	}

	for _, body := range strings.Split(*bodies, ",") {
		body = strings.TrimSpace(body)
		if body == "" {
			continue
		}
		payload, _ := json.Marshal(map[string]string{"lineId": *line, "to": *to, "body": body})
		status, response := do(client, http.MethodPost, *base+"/api/v2/messages", accessToken, payload, true)
		fmt.Printf("send body_len=%d status=%d\n", len(body), status)
		if status != http.StatusCreated && status != http.StatusOK {
			fmt.Printf("send_error=%s\n", sanitize(string(response)))
		}
		time.Sleep(2 * time.Second)
	}

	deadline := time.Now().Add(time.Duration(*wait) * time.Second)
	seen := map[string]bool{}
	for time.Now().Before(deadline) {
		threadKey := findThread(client, *base, accessToken, *line, *to)
		if threadKey != "" {
			status, payload := do(client, http.MethodGet,
				*base+"/api/v2/messages?threadKey="+threadKey+"&limit=20", accessToken, nil, false)
			if status == http.StatusOK {
				var views []map[string]any
				if json.Unmarshal(payload, &views) == nil {
					for _, view := range views {
						direction, _ := view["direction"].(string)
						if direction != "incoming" {
							continue
						}
						id, _ := view["id"].(string)
						if id == "" || seen[id] {
							continue
						}
						seen[id] = true
						body, _ := view["body"].(string)
						created, _ := view["createdAt"].(float64)
						fmt.Printf("reply at=%s body=%q\n",
							time.UnixMilli(int64(created)).UTC().Format(time.RFC3339), sanitize(body))
					}
				}
			}
		}
		if len(seen) > 0 {
			// Give the carrier a few extra seconds for a second part.
			time.Sleep(8 * time.Second)
			break
		}
		time.Sleep(3 * time.Second)
	}
	fmt.Printf("replies=%d\n", len(seen))
	fmt.Println("sms_query=done")
}

func readKey(path string) (string, error) {
	if path == "" {
		return strings.TrimSpace(os.Getenv("CALLRELAY_PROBE_KEY")), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "key=") {
			return strings.TrimPrefix(line, "key="), nil
		}
	}
	return "", fmt.Errorf("no key= line in %s", path)
}

func findThread(client *http.Client, base, token, line, to string) string {
	status, payload := do(client, http.MethodGet, base+"/api/v2/threads", token, nil, false)
	if status != http.StatusOK {
		return ""
	}
	var threads []map[string]any
	if json.Unmarshal(payload, &threads) != nil {
		return ""
	}
	want := line + ":" + to
	for _, thread := range threads {
		key, _ := thread["key"].(string)
		peer, _ := thread["peer"].(string)
		if key == want || peer == to {
			return key
		}
	}
	return ""
}

var digitRun = regexp.MustCompile(`\d{8,}`)

func sanitize(body string) string {
	return digitRun.ReplaceAllStringFunc(body, func(run string) string {
		if len(run) <= 4 {
			return run
		}
		return run[:2] + strings.Repeat("*", len(run)-4) + run[len(run)-2:]
	})
}

func maskID(value string) string {
	if len(value) <= 8 {
		return value
	}
	return value[:8] + "…"
}

func getJSON(client *http.Client, endpoint, token string) map[string]any {
	status, payload := do(client, http.MethodGet, endpoint, token, nil, false)
	if status != http.StatusOK {
		fatal(fmt.Sprintf("GET %s status %d", endpoint, status))
	}
	var body map[string]any
	if json.Unmarshal(payload, &body) != nil {
		fatal("GET " + endpoint + " returned invalid json")
	}
	return body
}

func postJSON(client *http.Client, endpoint, token string, body []byte) map[string]any {
	status, payload := do(client, http.MethodPost, endpoint, token, body, true)
	if status != http.StatusOK && status != http.StatusCreated {
		fatal(fmt.Sprintf("POST %s status %d", endpoint, status))
	}
	var parsed map[string]any
	if json.Unmarshal(payload, &parsed) != nil {
		fatal("POST " + endpoint + " returned invalid json")
	}
	return parsed
}

func do(client *http.Client, method, endpoint, token string, body []byte, idempotent bool) (int, []byte) {
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
	if idempotent {
		request.Header.Set("Idempotency-Key", fmt.Sprintf("sms-query-%d-%d", time.Now().UnixNano(), len(body)))
	}
	response, err := client.Do(request)
	if err != nil {
		fatal(err.Error())
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, payload
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "sms-query error:", message)
	os.Exit(1)
}
