package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// dialRelayProbe upgrades an authenticated device-scoped relay measurement
// socket (no call required).
func dialRelayProbe(t *testing.T, stack *testStack, token string) *websocket.Conn {
	t.Helper()
	header := http.Header{"Authorization": []string{"Bearer " + token}}
	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/media/relay-probe"
	conn, _, err := websocket.DefaultDialer.DialContext(t.Context(), url, header)
	if err != nil {
		t.Fatalf("relay probe upgrade: %v", err)
	}
	return conn
}

// TestMediaRelayProbeEchoAndAuth proves the idle relay measurement socket:
// device auth is enforced before the upgrade, a ready control is sent, and
// app-level pings are echoed with the same tag over the relay transport
// without any call, media session or audio.
func TestMediaRelayProbeEchoAndAuth(t *testing.T) {
	stack := newTestStack(t)
	keyID, secret := stack.createKey(true)
	token, _ := stack.enroll(keyID, secret, "Device RP")

	url := "ws" + stack.server.URL[len("http"):] + "/api/v2/media/relay-probe"
	if conn, response, err := websocket.DefaultDialer.Dial(url, nil); err == nil {
		conn.Close()
		t.Fatalf("unauthenticated relay probe upgrade succeeded: %v", response)
	} else if response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated relay probe status = %v, want 401", response)
	}

	conn := dialRelayProbe(t, stack, token)
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ready struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&ready); err != nil || ready.Type != "ready" {
		t.Fatalf("ready control = %+v err=%v, want type=ready", ready, err)
	}

	for _, tag := range []int64{1, 7, 9000} {
		payload, _ := json.Marshal(map[string]any{"type": "ping", "t": tag})
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			t.Fatalf("relay probe ping %d: %v", tag, err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var pong struct {
			Type string `json:"type"`
			T    int64  `json:"t"`
		}
		if err := conn.ReadJSON(&pong); err != nil || pong.Type != "pong" || pong.T != tag {
			t.Fatalf("relay probe pong = %+v err=%v, want pong t=%d", pong, err, tag)
		}
	}

	// A non-ping text frame is ignored (no pong, no error): send one and then
	// a real ping; the next frame must still be the matching pong.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"noop"}`)); err != nil {
		t.Fatalf("noop send: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{"type": "ping", "t": 42})
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatalf("ping after noop: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var pong struct {
		Type string `json:"type"`
		T    int64  `json:"t"`
	}
	if err := conn.ReadJSON(&pong); err != nil || pong.Type != "pong" || pong.T != 42 {
		t.Fatalf("pong after noop = %+v err=%v, want pong t=42", pong, err)
	}
}
