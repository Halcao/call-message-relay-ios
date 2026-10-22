package media

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/voice"

	"github.com/gorilla/websocket"
)

// wsTestPair spins a real loopback websocket pair. The HTTP handler hands
// the server-side conn to the test and then blocks WITHOUT reading — the
// test owns the pump; a reading handler would steal frames.
func wsTestPair(t *testing.T) (client *websocket.Conn, server *websocket.Conn, cleanup func()) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverConns := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConns <- conn
		<-done
	}))
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	server = <-serverConns
	return client, server, func() {
		close(done)
		_ = client.Close()
		_ = server.Close()
		srv.Close()
	}
}

func TestWSHostCodecDefaultsAndFallback(t *testing.T) {
	_, server, cleanup := wsTestPair(t)
	defer cleanup()

	// Default (no negotiation) is PCMU.
	h := newWSHost(server, "")
	if h.Codec() != codecPCMU {
		t.Fatalf("default codec = %q, want pcmu", h.Codec())
	}
	pcm := make([]int16, voice.FrameSamples)
	for i := range pcm {
		pcm[i] = 300
	}
	frame, err := h.encodePCM(pcm)
	if err != nil {
		t.Fatalf("pcmu encode: %v", err)
	}
	if len(frame) != frameBytes {
		t.Fatalf("pcmu frame = %d bytes", len(frame))
	}
	if _, err := h.decodeFrame(frame); err != nil {
		t.Fatalf("pcmu decode: %v", err)
	}

	// Opus negotiation creates Opus machinery and a sub-160-byte frame.
	h2 := newWSHost(server, codecOpus)
	defer h2.Close()
	if h2.Codec() != codecOpus {
		t.Fatalf("opus codec fell back: %q", h2.Codec())
	}
	opusFrame, err := h2.encodePCM(pcm)
	if err != nil {
		t.Fatalf("opus encode: %v", err)
	}
	if len(opusFrame) >= frameBytes {
		t.Fatalf("opus frame = %d bytes, want < %d", len(opusFrame), frameBytes)
	}
	decoded, err := h2.decodeFrame(opusFrame)
	if err != nil {
		t.Fatalf("opus decode: %v", err)
	}
	if len(decoded) != voice.FrameSamples {
		t.Fatalf("decoded %d samples", len(decoded))
	}
	var energy int64
	for _, s := range decoded {
		energy += int64(s) * int64(s)
	}
	if energy == 0 {
		t.Fatalf("round-trip energy is zero")
	}
}

func TestWSControllerStepsAndHolds(t *testing.T) {
	client, server, cleanup := wsTestPair(t)
	defer cleanup()
	_ = client
	h := newWSHost(server, codecOpus)
	defer h.Close()
	now := time.Now()

	// Congestion evidence steps DOWN one 8 kbps step per adapt (cooldown
	// respected); blocked evidence is consumed per call.
	h.lastChange = now.Add(-time.Second)
	h.noteWriteBlock(300 * time.Millisecond)
	h.adaptController(now)
	if h.bitrate != wsBitrateInitial-wsBitrateStep {
		t.Fatalf("bitrate = %d after congestion", h.bitrate)
	}

	// Stale app stats (never reported) must hold even with a clean window.
	h.lastChange = now.Add(-time.Second)
	h.cleanSince = now.Add(-wsCleanWindow - time.Second)
	h.adaptController(now)
	if h.bitrate != wsBitrateInitial-wsBitrateStep {
		t.Fatalf("bitrate moved without fresh app stats: %d", h.bitrate)
	}

	// Fresh low stats + sustained clean window step UP.
	h.lastChange = now.Add(-time.Second)
	h.noteAppStats(2, 10) // 40 ms buffer, 10 ms gap
	h.cleanSince = now.Add(-wsCleanWindow - time.Second)
	h.adaptController(now)
	if h.bitrate != wsBitrateInitial {
		t.Fatalf("bitrate = %d after clean recovery", h.bitrate)
	}
}

func TestWSControllerClampsAndCooldown(t *testing.T) {
	client, server, cleanup := wsTestPair(t)
	defer cleanup()
	_ = client
	h := newWSHost(server, codecOpus)
	defer h.Close()
	now := time.Now()
	h.lastChange = now.Add(-time.Second)
	// Pin app stats fresh-and-low so only congestion drives the clamp.
	h.noteAppStats(1, 5)
	h.cleanSince = now.Add(-wsCleanWindow - time.Second)
	for i := 0; i < 12; i++ {
		h.lastChange = now.Add(-time.Second)
		h.noteAppStats(1, 5)
		h.noteWriteBlock(400 * time.Millisecond)
		h.adaptController(now)
	}
	if h.bitrate != wsBitrateMin {
		t.Fatalf("bitrate = %d, want clamp at min %d", h.bitrate, wsBitrateMin)
	}
	// Immediate re-adapt is cooldown-gated.
	h.noteAppStats(1, 5)
	h.adaptController(now)
	if h.bitrate != wsBitrateMin {
		t.Fatalf("cooldown violated: %d", h.bitrate)
	}
}

// TestWSSOpusEndToEnd drives the real attach/pump/mixer path over a loopback
// socket: ready announces the codec, uplink Opus frames reach the host
// buffer, and the mixer emits Opus downlink frames.
func TestWSSOpusEndToEnd(t *testing.T) {
	client, server, cleanup := wsTestPair(t)
	defer cleanup()

	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "ws-opus", "call")
	defer session.Close()

	readyDone := make(chan struct{})
	gen, err := session.AttachWSHost(server, codecOpus, func(_ string, _ AttachInfo) error {
		close(readyDone)
		return server.WriteJSON(map[string]string{"type": "ready", "callId": "ws-opus", "codec": "opus"})
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	go session.PumpWSHost(server, gen)

	// The ready control must announce the negotiated codec.
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	mt, payload, err := client.ReadMessage()
	if err != nil || mt != websocket.TextMessage {
		t.Fatalf("ready read: %v", err)
	}
	var ready struct {
		Type  string `json:"type"`
		Codec string `json:"codec"`
	}
	if json.Unmarshal(payload, &ready) != nil || ready.Type != "ready" || ready.Codec != "opus" {
		t.Fatalf("ready = %s", payload)
	}

	// Uplink: stream FRAMED Opus (seq u16 BE + payload) and prove the frames
	// land in the host buffer (decoded PCM with energy).
	host := session.ws
	if host == nil || host.Codec() != codecOpus {
		t.Fatalf("ws host codec not opus")
	}
	tone := make([]int16, voice.FrameSamples)
	for i := range tone {
		// simple high-energy alternating pattern (survives NB Opus clearly)
		if i%2 == 0 {
			tone[i] = 6000
		} else {
			tone[i] = -6000
		}
	}
	encFrame, err := host.encodePCM(tone)
	if err != nil {
		t.Fatalf("encode uplink: %v", err)
	}
	var upSeq uint16
	framed := func(payload []byte) []byte {
		out := make([]byte, 2+len(payload))
		out[0] = byte(upSeq >> 8)
		out[1] = byte(upSeq)
		upSeq++
		copy(out[2:], payload)
		return out
	}
	for i := 0; i < 12; i++ {
		if err := client.WriteMessage(websocket.BinaryMessage, framed(encFrame)); err != nil {
			t.Fatalf("write uplink: %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		session.host.mu.Lock()
		depth := len(session.host.frames)
		session.host.mu.Unlock()
		if depth > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no uplink frame reached the host buffer")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Ping extension: stats consumed, pong carries uplink evidence.
	if err := client.WriteJSON(map[string]any{"type": "ping", "t": 7, "buf": 3, "gap": 15}); err != nil {
		t.Fatalf("ping: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		mt, payload, err = client.ReadMessage()
		if err != nil {
			t.Fatalf("pong read: %v", err)
		}
		if mt != websocket.TextMessage {
			continue
		}
		var pong map[string]any
		if json.Unmarshal(payload, &pong) != nil || pong["type"] != "pong" {
			continue
		}
		if pong["t"].(float64) != 7 {
			continue
		}
		if _, ok := pong["buf"]; !ok {
			t.Fatalf("pong missing uplink buffer evidence: %s", payload)
		}
		if _, ok := pong["ugap"]; !ok {
			t.Fatalf("pong missing uplink gap evidence: %s", payload)
		}
		break
	}
	host.codecMu.Lock()
	if host.appBufMs != 60 || host.appGapMs != 15 {
		t.Fatalf("app stats not consumed: buf=%d gap=%d", host.appBufMs, host.appGapMs)
	}
	host.codecMu.Unlock()

	// Downlink: the mixer must emit FRAMED Opus frames (seq u16 BE +
	// payload), with the sequence advancing per media slot.
	session.mu.Lock()
	session.hostPresent = true
	session.mu.Unlock()
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	frames := 0
	var lastSeq int = -1
	for frames < 20 {
		mt, payload, err = client.ReadMessage()
		if err != nil {
			t.Fatalf("downlink read: %v", err)
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		if len(payload) < 3 || len(payload) >= frameBytes+2 {
			t.Fatalf("downlink frame = %d bytes, want framed sub-%d Opus", len(payload), frameBytes+2)
		}
		seq := int(payload[0])<<8 | int(payload[1])
		if lastSeq >= 0 && seq != lastSeq+1 {
			t.Fatalf("downlink seq = %d after %d (framing broken)", seq, lastSeq)
		}
		lastSeq = seq
		frames++
	}
}

// TestWSSPCMUFallbackKeepsWireFormat: without negotiation the socket stays
// byte-identical to the historic 160-byte PCMU protocol.
func TestWSSPCMUFallbackKeepsWireFormat(t *testing.T) {
	client, server, cleanup := wsTestPair(t)
	defer cleanup()

	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "ws-pcmu", "call")
	defer session.Close()

	gen, err := session.AttachWSHost(server, "", func(_ string, _ AttachInfo) error {
		return server.WriteJSON(map[string]string{"type": "ready", "callId": "ws-pcmu"})
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	go session.PumpWSHost(server, gen)

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	mt, payload, err := client.ReadMessage()
	if err != nil || mt != websocket.TextMessage {
		t.Fatalf("ready read: %v", err)
	}
	if strings.Contains(string(payload), "codec") {
		t.Fatalf("pcmu ready must not announce a codec: %s", payload)
	}

	// Uplink PCMU 160-byte frames must be accepted.
	pcm := make([]int16, voice.FrameSamples)
	for i := range pcm {
		pcm[i] = 500
	}
	frame, _ := voice.EncodePCMU(pcm)
	for i := 0; i < 6; i++ {
		if err := client.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Fatalf("uplink: %v", err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		session.host.mu.Lock()
		depth := len(session.host.frames)
		session.host.mu.Unlock()
		if depth > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no uplink frame reached the host buffer")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
