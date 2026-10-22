package media

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A replacement host that lands between AttachWSHost(old) and
// PumpWSHost(old) must not be captured or killed by the old pump: the entry
// identity fence closes only the superseded connection.
func TestWSHostOldPumpEntryAfterReplacementKeepsNewHost(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverConns := make(chan *websocket.Conn, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConns <- conn
		// Keep the handler alive for the life of the test; closes propagate
		// to the client on server shutdown.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	dial := func() *websocket.Conn {
		url := "ws" + strings.TrimPrefix(server.URL, "http")
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		return conn
	}

	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "fence", "call")
	defer session.Close()

	clientA := dial()
	serverA := <-serverConns
	genA, err := session.AttachWSHost(serverA, "", nil)
	if err != nil {
		t.Fatalf("attach A: %v", err)
	}
	clientB := dial()
	serverB := <-serverConns
	if _, err := session.AttachWSHost(serverB, "", nil); err != nil {
		t.Fatalf("attach B: %v", err)
	}

	// The old pump enters AFTER the replacement: it must close only the
	// server A connection and leave the current host (B) attached.
	session.PumpWSHost(serverA, genA)

	session.mu.Lock()
	current := session.ws
	session.mu.Unlock()
	if current == nil || current.conn != serverB {
		t.Fatalf("replacement host was detached by the old pump")
	}

	// B still carries mixed frames to its client.
	clientB.SetReadDeadline(time.Now().Add(2 * time.Second))
	messageType, payload, err := clientB.ReadMessage()
	if err != nil {
		t.Fatalf("replacement host stopped serving: %v", err)
	}
	if messageType != websocket.BinaryMessage || len(payload) != frameBytes {
		t.Fatalf("unexpected frame from replacement host")
	}
	_ = clientA
}
