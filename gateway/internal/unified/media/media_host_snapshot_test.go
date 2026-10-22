package media

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestMixOnceClearsSnapshottedHostNotReplacement pins the handover ownership
// rule for the exact build-35 API race stack: AttachWSHost installs a fresh
// jitter buffer under Session.mu while mixOnce's host-absent path called
// s.host.clear() outside the lock, wiping the new transport's buffer.
//
// The interleaving is deterministic: the test holds the only leg's mutex so
// mixOnce parks in its !hostLive Drain loop AFTER the under-lock snapshot; it
// then publishes a replacement buffer exactly as AttachWSHost does (s.host,
// hostPresent, generation under s.mu) and only then releases the leg. With
// the owned snapshot the clear hits the captured old buffer; without it the
// replacement loses the frames.
func TestMixOnceClearsSnapshottedHostNotReplacement(t *testing.T) {
	oldHost := newHostJitter(160)
	// Seed so the snapshot's pop() exercises the buffer and the clear has
	// observable state to wipe (pop consumes ts=0, leaving one frame).
	oldHost.pushPCM(0, []int16{1})
	oldHost.pushPCM(160, []int16{2})

	blocker := &Leg{CallID: "line1:snapshot"}
	session := &Session{
		Key:  "call:snapshot",
		Kind: "call",
		legs: map[string]*Leg{"line1:snapshot": blocker},
		host: oldHost,
	}

	blocker.mu.Lock() // parks mixOnce in the !hostLive Drain loop
	done := make(chan struct{})
	go func() {
		session.mixOnce()
		close(done)
	}()

	// The snapshot publishes statsTick under Session.mu; once observed,
	// mixOnce can no longer reach the clear before we release blocker.mu,
	// because Drain precedes it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		session.mu.Lock()
		tick := session.statsTick
		session.mu.Unlock()
		if tick != 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mixOnce never reached its snapshot")
		}
		time.Sleep(time.Millisecond)
	}

	// Transport handover exactly as AttachWSHost publishes it.
	replacement := newHostJitter(160)
	session.mu.Lock()
	session.generation++
	session.hostPresent = true
	session.host = replacement
	session.mu.Unlock()
	replacement.pushPCM(1000, []int16{7})
	replacement.pushPCM(1160, []int16{8})

	blocker.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("mixOnce did not finish after the handover")
	}

	replacement.mu.Lock()
	kept := len(replacement.frames)
	replacement.mu.Unlock()
	if kept != 2 {
		t.Fatalf("mixOnce clear wiped the replacement host buffer: %d/2 frames kept", kept)
	}
	oldHost.mu.Lock()
	oldKept := len(oldHost.frames)
	oldHost.mu.Unlock()
	if oldKept != 0 {
		t.Fatalf("snapshotted old buffer was not cleared: %d frames left", oldKept)
	}
}

// TestWSHostReattachRaceFreeMixer hammers the real AttachWSHost/PumpWSHost
// handover against the live session loop and socket pumps under -race: every
// s.host pointer access on those paths must be synchronized with the
// replacement write (ws.go AttachWSHost) even while the mixer ticks.
func TestWSHostReattachRaceFreeMixer(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverConns := make(chan *websocket.Conn, 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Hand the hijacked socket to the test and return: the pump owns the
		// server-side conn exclusively (an extra handler ReadMessage here
		// would race the pump's reader in gorilla/websocket).
		serverConns <- conn
	}))
	defer server.Close()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	dial := func() *websocket.Conn {
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
	session := NewSession(engine, "call:reattach-race", "call")
	defer session.Close()

	for i := 0; i < 48; i++ {
		client := dial()
		serverConn := <-serverConns
		generation, err := session.AttachWSHost(serverConn, "", nil)
		if err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		go session.PumpWSHost(serverConn, generation)
		session.hostPush(generation, uint32(160*i), []int16{int16(i)})
		// Exercise the pump's jitter-depth ping path concurrently with the
		// next replacement.
		_ = client.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping","t":1,"buf":2,"gap":3}`))
		time.Sleep(3 * time.Millisecond)
		if i%3 == 2 {
			session.DetachWSHost(generation, nil)
		}
		_ = client.Close()
	}
}
