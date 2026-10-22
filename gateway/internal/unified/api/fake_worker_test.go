package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"callrelay/gateway/internal/voice"

	"github.com/gorilla/websocket"
)

// fakeWorker is a controllable stand-in for a forked CellBridge worker. It
// implements the v1 control/history API plus the internal SMS-mode and media
// endpoints the unified service depends on.
type fakeWorker struct {
	LineID string

	mu          sync.Mutex
	calls       map[string]workerCall
	answerCalls int
	dialCalls   int
	rejectCalls int
	readCalls   int
	messages    []map[string]any
	threads     []map[string]any
	dryRun      bool
	failNextSMS bool
	// strictThreadMessages opts into worker-faithful GET /api/v1/messages
	// semantics (threadKey filter + before/beforeId cursor + limit +
	// X-CellBridge-Has-More). Disabled by default so every existing test
	// keeps the historical "return all messages" behaviour.
	strictThreadMessages bool
	simIdentity          map[string]any
	events               chan []byte
	server               *httptest.Server
	messageSeq           int

	// mediaConns mimics the worker's single-owner PCMU socket: one websocket
	// per call, "ready" on open, binary frames in both directions.
	mediaConns map[string]*fakeMediaConn

	// Harness-only modem simulation (PWA browser loopback). Disabled unless
	// a test enables it, so every other test is unaffected.
	modemToneHz    float64
	autoAnswer     bool
	audioStats     map[string]*modemAudioStats
	downlinkFrames int64
	mediasOpened   int
}

// fakeMediaConn records host-bound frames and lets a test push cellular
// frames toward the daemon.
type fakeMediaConn struct {
	mu       sync.Mutex
	writeMu  sync.Mutex
	conn     *websocket.Conn
	received [][]byte
	done     chan struct{}
	stopOnce sync.Once
}

func (c *fakeMediaConn) stop() {
	c.stopOnce.Do(func() { close(c.done) })
}

func (c *fakeMediaConn) push(frame []byte) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("media socket not open")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (c *fakeMediaConn) frames() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.received))
	copy(out, c.received)
	return out
}

func (f *fakeWorker) mediaSocket(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Internal-Token") != "test-internal-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	callID := r.PathValue("id")
	entry := &fakeMediaConn{conn: conn, done: make(chan struct{})}
	f.mu.Lock()
	f.mediaConns[callID] = entry
	toneHz := f.modemToneHz
	f.mediasOpened++
	f.mu.Unlock()
	defer func() {
		entry.stop()
		f.mu.Lock()
		if f.mediaConns[callID] == entry {
			delete(f.mediaConns, callID)
		}
		f.mu.Unlock()
		_ = conn.Close()
	}()
	entry.writeMu.Lock()
	err = conn.WriteJSON(map[string]string{"type": "ready", "callId": callID})
	entry.writeMu.Unlock()
	if err != nil {
		return
	}
	if toneHz > 0 {
		go f.pumpDownlinkTone(entry, toneHz)
	}
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		entry.mu.Lock()
		entry.received = append(entry.received, append([]byte(nil), payload...))
		entry.mu.Unlock()
		f.recordUplinkAudio(callID, payload)
	}
}

// pumpDownlinkTone synthesizes a continuous 20 ms PCMU tone toward the
// gateway (the fake cellular modem's downlink audio). It stops when the
// media socket closes.
func (f *fakeWorker) pumpDownlinkTone(entry *fakeMediaConn, hz float64) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var phase float64
	step := 2 * math.Pi * hz / 8000.0
	for {
		select {
		case <-entry.done:
			return
		case <-ticker.C:
			frame := make([]int16, 160)
			for i := range frame {
				frame[i] = int16(0.30 * 32767 * math.Sin(phase))
				phase += step
				if phase > 2*math.Pi {
					phase -= 2 * math.Pi
				}
			}
			encoded, err := voice.EncodePCMU(frame)
			if err != nil {
				return
			}
			if err := entry.push(encoded); err != nil {
				return
			}
			f.mu.Lock()
			f.downlinkFrames++
			f.mu.Unlock()
		}
	}
}

// pushCellular delivers one 20 ms PCMU frame as if the cellular side produced it.
func (f *fakeWorker) pushCellular(callID string, frame []byte) error {
	f.mu.Lock()
	entry := f.mediaConns[callID]
	f.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("no media socket for call %s", callID)
	}
	return entry.push(frame)
}

// hostFrames returns the PCMU frames the daemon wrote toward the worker.
func (f *fakeWorker) hostFrames(callID string) [][]byte {
	f.mu.Lock()
	entry := f.mediaConns[callID]
	f.mu.Unlock()
	if entry == nil {
		return nil
	}
	return entry.frames()
}

type workerCall struct {
	ID          string `json:"id"`
	GatewayID   string `json:"gatewayID"`
	LineID      string `json:"lineID"`
	Direction   string `json:"direction"`
	Peer        string `json:"peer"`
	State       string `json:"state"`
	StartedAt   int64  `json:"startedAt"`
	ConnectedAt *int64 `json:"connectedAt"`
	EndedAt     *int64 `json:"endedAt"`
	EndReason   string `json:"endReason,omitempty"`
}

func newFakeWorker(lineID string) *fakeWorker {
	fake := &fakeWorker{
		LineID: lineID, calls: make(map[string]workerCall),
		dryRun: true, events: make(chan []byte, 64), mediaConns: make(map[string]*fakeMediaConn),
		audioStats:  make(map[string]*modemAudioStats),
		simIdentity: map[string]any{"number": "", "numberSource": "empty", "commands": map[string]string{"AT+CNUM": "empty"}},
		threads: []map[string]any{{
			"key": lineID + "-peer", "peer": "+8613800000001", "unreadCount": 1,
			"lastMessage": map[string]any{
				"id": "msg_1", "gatewayID": "gw-" + lineID, "lineID": lineID + ":line",
				"threadKey": lineID + "-peer", "direction": "inbound", "peer": "+8613800000001",
				"body": "hello " + lineID, "encoding": "gsm7", "status": "sent", "createdAt": time.Now().UnixMilli(),
			},
		}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/pairing/start", fake.pairingStart)
	mux.HandleFunc("POST /api/v1/pairing/complete", fake.pairingComplete)
	mux.HandleFunc("POST /api/v1/auth/refresh", fake.refresh)
	mux.HandleFunc("GET /api/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "version": "test"})
	})
	mux.HandleFunc("GET /api/v1/identity", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"gatewayId": "gw-" + lineID, "gatewayName": lineID, "apiVersion": "v1"})
	})
	mux.HandleFunc("GET /api/v1/line", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"sim": "ready", "operator": "Test", "registration": "registered", "voice": "ready", "sms": "ready", "capabilityTier": "full_voice"})
	})
	mux.HandleFunc("GET /api/v1/events", fake.eventsSocket)
	mux.HandleFunc("GET /api/v1/calls", fake.listCalls)
	mux.HandleFunc("POST /api/v1/calls", fake.dial)
	mux.HandleFunc("GET /api/v1/calls/{id}", fake.getCall)
	mux.HandleFunc("POST /api/v1/calls/{id}/{action}", fake.callAction)
	mux.HandleFunc("GET /api/v1/threads", fake.listThreads)
	mux.HandleFunc("GET /api/v1/messages", fake.listMessages)
	mux.HandleFunc("POST /api/v1/messages", fake.sendMessage)
	mux.HandleFunc("POST /api/v1/messages/{id}/read", func(w http.ResponseWriter, _ *http.Request) {
		fake.mu.Lock()
		fake.readCalls++
		fake.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /internal/v1/settings/sms-dry-run", fake.getDryRun)
	mux.HandleFunc("PUT /internal/v1/settings/sms-dry-run", fake.putDryRun)
	mux.HandleFunc("GET /internal/v1/identity/sim", fake.simIdentityHandler)
	mux.HandleFunc("POST /internal/v1/recover", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"voice": "ready"}) })
	mux.HandleFunc("GET /internal/v1/media/{id}", fake.mediaSocket)
	fake.server = httptest.NewServer(mux)
	return fake
}

func (f *fakeWorker) close() { f.server.Close() }

func (f *fakeWorker) pairingStart(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"gatewayId": "gw-" + f.LineID, "pairingId": "pair-" + f.LineID, "oneTimeSecret": "secret", "expiresAt": time.Now().Add(time.Minute).Unix()})
}

func (f *fakeWorker) pairingComplete(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"deviceId": "dev_worker_" + f.LineID, "accessToken": "acc_" + f.LineID, "refreshToken": "ref_" + f.LineID})
}

func (f *fakeWorker) refresh(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"accessToken": "acc_" + f.LineID, "refreshToken": "ref_" + f.LineID})
}

func (f *fakeWorker) eventsSocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		select {
		case <-r.Context().Done():
			return
		case payload, ok := <-f.events:
			if !ok {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
				return
			}
		}
	}
}

// injectIncoming emits a worker call.incoming event.
func (f *fakeWorker) injectIncoming(callID, peer string) {
	record := workerCall{
		ID: callID, GatewayID: "gw-" + f.LineID, LineID: f.LineID + ":line",
		Direction: "inbound", Peer: peer, State: "incoming_ringing",
		StartedAt: time.Now().UnixMilli(),
	}
	f.mu.Lock()
	f.calls[callID] = record
	f.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{
		"id": "evt_" + callID, "seq": time.Now().UnixNano(), "type": "call.incoming",
		"createdAt": time.Now().Unix(), "data": record,
	})
	f.events <- payload
}

// injectCallUpdate emits a worker call.updated event for an existing call,
// mirroring a late +CLIP caller-ID arriving after the initial ring.
func (f *fakeWorker) injectCallUpdate(callID, peer string) {
	f.mu.Lock()
	record, ok := f.calls[callID]
	if !ok {
		f.mu.Unlock()
		return
	}
	record.Peer = peer
	f.calls[callID] = record
	f.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{
		"id": "evt_" + callID + "_updated", "seq": time.Now().UnixNano(), "type": "call.updated",
		"createdAt": time.Now().Unix(), "data": record,
	})
	f.events <- payload
}

func (f *fakeWorker) listCalls(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	records := make([]workerCall, 0, len(f.calls))
	for _, record := range f.calls {
		records = append(records, record)
	}
	f.mu.Unlock()
	if records == nil {
		records = []workerCall{}
	}
	writeJSON(w, 200, records)
}

func (f *fakeWorker) dial(w http.ResponseWriter, r *http.Request) {
	var request struct {
		To           string `json:"to"`
		ClientCallID string `json:"clientCallId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, 400, "CB-CALL-001", "invalid JSON")
		return
	}
	f.mu.Lock()
	f.dialCalls++
	record := workerCall{
		ID: request.ClientCallID, GatewayID: "gw-" + f.LineID, LineID: f.LineID + ":line",
		Direction: "outbound", Peer: request.To, State: "outgoing_dialing",
		StartedAt: time.Now().UnixMilli(),
	}
	f.calls[request.ClientCallID] = record
	auto := f.autoAnswer
	f.mu.Unlock()
	writeJSON(w, 201, record)
	if auto {
		go f.autoAnswerCall(record.ID)
	}
}

// autoAnswerCall simulates the remote party picking up: cellular_ready moves
// the unified call dialing -> connecting and media_ready (connectedAt set)
// moves it to active, exactly like the real worker event sequence.
func (f *fakeWorker) autoAnswerCall(workerCallID string) {
	time.Sleep(250 * time.Millisecond)
	f.mu.Lock()
	record, ok := f.calls[workerCallID]
	f.mu.Unlock()
	if !ok || record.State == "ended" {
		return
	}
	now := time.Now().UnixMilli()
	connecting := record
	connecting.State = "connecting"
	active := record
	active.State = "active"
	active.ConnectedAt = &now
	f.injectWorkerEvent("cellular_ready", connecting)
	f.injectWorkerEvent("media_ready", active)
	f.injectWorkerEvent("call.updated", active)
}

func (f *fakeWorker) getCall(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	record, ok := f.calls[r.PathValue("id")]
	f.mu.Unlock()
	if !ok {
		writeError(w, 404, "CB-CALL-006", "call not found")
		return
	}
	writeJSON(w, 200, record)
}

func (f *fakeWorker) callAction(w http.ResponseWriter, r *http.Request) {
	callID, action := r.PathValue("id"), r.PathValue("action")
	f.mu.Lock()
	record, ok := f.calls[callID]
	if !ok {
		f.mu.Unlock()
		writeError(w, 404, "CB-CALL-006", "call not found")
		return
	}
	switch action {
	case "answer":
		f.answerCalls++
		now := time.Now().UnixMilli()
		record.State = "active"
		record.ConnectedAt = &now
	case "reject":
		f.rejectCalls++
		now := time.Now().UnixMilli()
		record.State = "ended"
		record.EndedAt = &now
	case "hangup":
		now := time.Now().UnixMilli()
		record.State = "ended"
		record.EndedAt = &now
	}
	f.calls[callID] = record
	f.mu.Unlock()
	// Worker state changes are event-driven in production; mirror that so
	// the unified call state converges without a real media attach. Hangup
	// and reject emit the real worker's two-step terminal sequence
	// (kind "hangup" with state "ending", then kind "call.ended" carrying
	// endedAt) — that sequence is what raced the unified HTTP hangup path.
	if action == "hangup" || action == "reject" {
		ending := record
		ending.State = "ending"
		ending.EndedAt = nil
		f.injectWorkerEvent("hangup", ending)
		f.injectWorkerEvent("call.ended", record)
	} else {
		payload, _ := json.Marshal(map[string]any{
			"id": "evt_" + callID + "_" + action, "seq": time.Now().UnixNano(), "type": "call.updated",
			"createdAt": time.Now().Unix(), "data": record,
		})
		select {
		case f.events <- payload:
		default:
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// injectWorkerEvent emits an arbitrary worker event type carrying a call
// record. It mirrors the production worker's event kinds beyond call.updated:
// cellular_ready (outbound ATD accepted), media_ready (worker audio bridge up,
// connectedAt set) and the terminal call.ended snapshot.
func (f *fakeWorker) injectWorkerEvent(eventType string, record workerCall) {
	f.mu.Lock()
	f.calls[record.ID] = record
	f.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{
		"id": "evt_" + record.ID + "_" + eventType, "seq": time.Now().UnixNano(), "type": eventType,
		"createdAt": time.Now().Unix(), "data": record,
	})
	select {
	case f.events <- payload:
	default:
	}
}

func (f *fakeWorker) listThreads(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.threads == nil {
		f.threads = []map[string]any{}
	}
	writeJSON(w, 200, f.threads)
}

func (f *fakeWorker) listMessages(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	messages := f.messages
	if messages == nil {
		messages = []map[string]any{}
	}
	if !f.strictThreadMessages {
		writeJSON(w, 200, messages)
		return
	}
	// Worker-faithful thread page: newest first, cursor (beforeId wins over
	// before), hard limit, and the has-more header the unified client reads.
	query := r.URL.Query()
	key := query.Get("threadKey")
	limit, _ := strconv.Atoi(query.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	before, _ := strconv.ParseInt(query.Get("before"), 10, 64)
	beforeID := query.Get("beforeId")
	filtered := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		if key != "" && message["threadKey"] != key {
			continue
		}
		createdAt, _ := message["createdAt"].(float64)
		if before > 0 && (int64(createdAt) > before || int64(createdAt) == before && (beforeID == "" || message["id"].(string) >= beforeID)) {
			continue
		}
		filtered = append(filtered, message)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		a, _ := filtered[i]["createdAt"].(float64)
		b, _ := filtered[j]["createdAt"].(float64)
		return a > b
	})
	if beforeID != "" && before == 0 {
		cut := -1
		for index, message := range filtered {
			if message["id"] == beforeID {
				cut = index + 1
				break
			}
		}
		if cut < 0 {
			filtered = nil
		} else {
			filtered = filtered[cut:]
		}
	}
	hasMore := len(filtered) > limit
	page := filtered
	if hasMore {
		page = filtered[:limit]
	}
	w.Header().Set("X-CellBridge-Has-More", map[bool]string{true: "true", false: "false"}[hasMore])
	if page == nil {
		page = []map[string]any{}
	}
	writeJSON(w, 200, page)
}

func (f *fakeWorker) sendMessage(w http.ResponseWriter, r *http.Request) {
	var request struct {
		To   string `json:"to"`
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, 400, "CB-SMS-001", "invalid JSON")
		return
	}
	f.mu.Lock()
	f.messageSeq++
	message := map[string]any{
		"id": fmt.Sprintf("sms_%d", f.messageSeq), "gatewayID": "gw-" + f.LineID, "lineID": f.LineID + ":line",
		"threadKey": f.LineID + "-peer", "direction": "outbound", "peer": request.To,
		"body": request.Body, "encoding": "gsm7", "status": "sent", "createdAt": time.Now().UnixMilli(),
	}
	fail := f.failNextSMS
	if fail {
		message["status"] = "failed"
	}
	f.failNextSMS = false
	f.mu.Unlock()
	if fail {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(message)
		return
	}
	writeJSON(w, 201, message)
}

func (f *fakeWorker) simIdentityHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Internal-Token") != "test-internal-token" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, 200, f.simIdentity)
}

func (f *fakeWorker) setSIMIdentity(value map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.simIdentity = value
}

func (f *fakeWorker) getDryRun(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON(w, 200, map[string]any{"enabled": f.dryRun, "supported": true})
}

func (f *fakeWorker) putDryRun(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Enabled == nil {
		writeError(w, 400, "CB-INT-003", "invalid body")
		return
	}
	f.mu.Lock()
	f.dryRun = *request.Enabled
	f.mu.Unlock()
	writeJSON(w, 200, map[string]any{"enabled": *request.Enabled, "supported": true})
}

func (f *fakeWorker) setFailNextSMS() {
	f.mu.Lock()
	f.failNextSMS = true
	f.mu.Unlock()
}

func (f *fakeWorker) counts() (dial, answer int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dialCalls, f.answerCalls
}

func (f *fakeWorker) rejects() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rejectCalls
}

func (f *fakeWorker) readCallsLocked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readCalls
}

// seedThread installs a full conversation fixture (thread list entry + message
// history) and emits the live message.created event, so overlay/quarantine
// tests exercise the real listing paths.
func (f *fakeWorker) seedThread(threadKey, peer, body string, unread int, createdAt int64) string {
	if createdAt == 0 {
		createdAt = time.Now().UnixMilli()
	}
	messageID := "msg_" + threadKey
	message := map[string]any{
		"id": messageID, "gatewayID": "gw-" + f.LineID, "lineID": f.LineID + ":line",
		"threadKey": threadKey, "direction": "inbound", "peer": peer,
		"body": body, "encoding": "gsm7", "status": "received", "createdAt": createdAt,
	}
	thread := map[string]any{
		"key": threadKey, "peer": peer, "unreadCount": unread, "lastMessage": message,
	}
	f.mu.Lock()
	replaced := false
	for index, existing := range f.threads {
		if existing["key"] == threadKey {
			f.threads[index] = thread
			replaced = true
			break
		}
	}
	if !replaced {
		f.threads = append(f.threads, thread)
	}
	f.messages = append(f.messages, message)
	f.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{
		"id": "evt_msg_" + messageID, "seq": time.Now().UnixNano(), "type": "message.created",
		"createdAt": time.Now().Unix(), "data": message,
	})
	f.events <- payload
	return messageID
}

// injectIncomingMessage emits a worker message.created event for an inbound
// SMS, exactly like the real worker's live event stream.
func (f *fakeWorker) injectIncomingMessage(messageID, peer, body string) {
	message := map[string]any{
		"id": messageID, "gatewayID": "gw-" + f.LineID, "lineID": f.LineID + ":line",
		"threadKey": "line1-" + messageID, "direction": "inbound", "peer": peer,
		"body": body, "encoding": "gsm7", "status": "received", "createdAt": time.Now().UnixMilli(),
	}
	f.mu.Lock()
	f.messages = append(f.messages, message)
	f.mu.Unlock()
	payload, _ := json.Marshal(map[string]any{
		"id": "evt_msg_" + messageID, "seq": time.Now().UnixNano(), "type": "message.created",
		"createdAt": time.Now().Unix(), "data": message,
	})
	f.events <- payload
}

// ---------- harness-only modem audio simulation (PWA browser loopback) ----------

type modemAudioStats struct {
	mu      sync.Mutex
	frames  int
	samples int
	sumSq   float64
	zero    int
	first   time.Time
	last    time.Time
	maxGap  time.Duration
	pcm     []int16
}

func (s *modemAudioStats) observe(frame []int16, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.first.IsZero() {
		if gap := at.Sub(s.last); gap > s.maxGap {
			s.maxGap = gap
		}
	} else {
		s.first = at
	}
	s.last = at
	s.frames++
	nonzero := false
	for _, value := range frame {
		s.samples++
		s.sumSq += float64(value) * float64(value)
		if value != 0 {
			nonzero = true
		}
	}
	if !nonzero {
		s.zero++
	}
	const capSamples = 8000 * 6
	if len(s.pcm) < capSamples {
		s.pcm = append(s.pcm, frame...)
	}
}

func (f *fakeWorker) recordUplinkAudio(callID string, payload []byte) {
	frame, err := voice.DecodePCMU(payload)
	if err != nil || len(frame) == 0 {
		return
	}
	f.mu.Lock()
	stats := f.audioStats[callID]
	if stats == nil {
		stats = &modemAudioStats{}
		f.audioStats[callID] = stats
	}
	f.mu.Unlock()
	stats.observe(frame, time.Now())
}

// modemAudioReport is the decoded uplink evidence measured at the fake
// cellular modem: real PCM the gateway sent toward the modem.
type modemAudioReport struct {
	Frames     int       `json:"frames"`
	Samples    int       `json:"samples"`
	RMS        float64   `json:"rms"`
	ToneHz     float64   `json:"toneHz"`
	TonePower  float64   `json:"tonePower"`
	ZeroFrames int       `json:"zeroFrames"`
	DurationMS int64     `json:"durationMs"`
	MaxGapMS   int64     `json:"maxGapMs"`
	Spectrum   []toneBin `json:"spectrum,omitempty"`
}

type toneBin struct {
	Hz    float64 `json:"hz"`
	Power float64 `json:"power"`
}

func (f *fakeWorker) uplinkReport(callID string) modemAudioReport {
	f.mu.Lock()
	stats := f.audioStats[callID]
	f.mu.Unlock()
	if stats == nil {
		return modemAudioReport{}
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	report := modemAudioReport{
		Frames: stats.frames, Samples: stats.samples, ZeroFrames: stats.zero,
		MaxGapMS: stats.maxGap.Milliseconds(),
	}
	if stats.samples > 0 {
		report.RMS = math.Sqrt(stats.sumSq / float64(stats.samples))
	}
	if !stats.first.IsZero() {
		report.DurationMS = stats.last.Sub(stats.first).Milliseconds()
	}
	report.ToneHz, report.TonePower = estimateToneHz(stats.pcm)
	report.Spectrum = topToneBins(stats.pcm)
	return report
}

// topToneBins returns the four strongest 25 Hz bins for diagnostics.
func topToneBins(pcm []int16) []toneBin {
	if len(pcm) < 400 {
		return nil
	}
	bins := make([]toneBin, 0, 4)
	for hz := 200.0; hz <= 1800.0; hz += 25.0 {
		bins = append(bins, toneBin{Hz: hz, Power: goertzelPowerAt(pcm, hz)})
	}
	for i := 1; i < len(bins); i++ {
		for j := i; j > 0 && bins[j].Power > bins[j-1].Power; j-- {
			bins[j], bins[j-1] = bins[j-1], bins[j]
		}
	}
	if len(bins) > 4 {
		bins = bins[:4]
	}
	return bins
}

func goertzelPowerAt(pcm []int16, hz float64) float64 {
	coeff := 2 * math.Cos(2*math.Pi*hz/8000.0)
	var s0, s1, s2 float64
	for _, sample := range pcm {
		s0 = float64(sample)/32768.0 + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	return (s1*s1 + s2*s2 - coeff*s1*s2) / float64(len(pcm))
}

// estimateToneHz scans candidate frequencies with a Goertzel filter and
// returns the strongest bin plus its normalized power.
func estimateToneHz(pcm []int16) (float64, float64) {
	if len(pcm) < 400 {
		return 0, 0
	}
	bestHz, bestPower := 0.0, 0.0
	for hz := 250.0; hz <= 1600.0; hz += 25.0 {
		coeff := 2 * math.Cos(2*math.Pi*hz/8000.0)
		var s0, s1, s2 float64
		for _, sample := range pcm {
			s0 = float64(sample)/32768.0 + coeff*s1 - s2
			s2 = s1
			s1 = s0
		}
		power := (s1*s1 + s2*s2 - coeff*s1*s2) / float64(len(pcm))
		if power > bestPower {
			bestPower, bestHz = power, hz
		}
	}
	return bestHz, bestPower
}

func (f *fakeWorker) resetModemAudio() {
	f.mu.Lock()
	f.audioStats = make(map[string]*modemAudioStats)
	f.mu.Unlock()
}

// uplinkPCM copies the captured uplink PCM for one call (debug dumps only).
func (f *fakeWorker) uplinkPCM(callID string) []int16 {
	f.mu.Lock()
	stats := f.audioStats[callID]
	f.mu.Unlock()
	if stats == nil {
		return nil
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	return append([]int16(nil), stats.pcm...)
}

func (f *fakeWorker) busiestAudioCall() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	bestID := ""
	bestFrames := -1
	for id, stats := range f.audioStats {
		stats.mu.Lock()
		frames := stats.frames
		stats.mu.Unlock()
		if frames > bestFrames {
			bestFrames, bestID = frames, id
		}
	}
	return bestID
}

func (f *fakeWorker) enableModemTone(hz float64) {
	f.mu.Lock()
	f.modemToneHz = hz
	f.mu.Unlock()
}

func (f *fakeWorker) setAutoAnswer(on bool) {
	f.mu.Lock()
	f.autoAnswer = on
	f.mu.Unlock()
}

var _ = strings.TrimSpace
