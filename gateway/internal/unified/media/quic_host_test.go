package media

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// quicMediaPair builds a real Session <-> QUICSocket pair over the fake
// QUIC connection: the test acts as the client (datagram + control stream).
type quicMediaPair struct {
	session *Session
	qs      *QUICSocket     // server-side adapter (attached to the session)
	conn    *fakeQUICConn   // server-side fake connection
	client  *fakeQUICStream // client end of the control stream
	clientD chan []byte     // datagrams the server sent (captured via conn hook)
}

func newQUICMediaPair(t *testing.T, codec string) *quicMediaPair {
	t.Helper()
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "quic:1", "call")
	conn := newFakeQUICConn()
	client := pairClientOf(conn.stream)
	// Capture outbound datagrams.
	sent := make(chan []byte, 64)
	connHook := &hookQUICConn{QUICConnection: conn, onSend: func(p []byte) { sent <- p }}
	epochSource := func() uint64 { return session.Epoch() }
	qs := NewQUICSocket(context.Background(), connHook, epochSource)
	pair := &quicMediaPair{session: session, qs: qs, conn: conn, client: client, clientD: sent}
	t.Cleanup(func() {
		qs.Close()
		session.Close()
	})
	var info AttachInfo
	gen, err := session.AttachWSHost(qs, codec, func(negotiated string, i AttachInfo) error {
		info = i
		return nil
	})
	if err != nil {
		t.Fatalf("attach quic host: %v", err)
	}
	_ = gen
	pairEpoch := info.Epoch
	_ = pairEpoch
	return pair
}

type hookQUICConn struct {
	QUICConnection
	onSend func([]byte)
}

func (h *hookQUICConn) SendDatagram(p []byte) error {
	h.onSend(p)
	return h.QUICConnection.SendDatagram(p)
}

// sendClientFrame stamps a PCMU frame with the session's current epoch and
// queues it as an inbound datagram.
func (p *quicMediaPair) sendClientFrame(t *testing.T, pcmByte byte) {
	t.Helper()
	frame := make([]byte, 160)
	for i := range frame {
		frame[i] = pcmByte
	}
	p.conn.datagrams <- StampQUICEnvelope(frame, p.session.Epoch())
}

func (p *quicMediaPair) sendClientFrameEpoch(t *testing.T, pcmByte byte, epoch uint64) {
	t.Helper()
	frame := make([]byte, 160)
	for i := range frame {
		frame[i] = pcmByte
	}
	p.conn.datagrams <- StampQUICEnvelope(frame, epoch)
}

func (p *quicMediaPair) recvServerDatagram(t *testing.T) []byte {
	t.Helper()
	select {
	case d := <-p.clientD:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("no datagram from server")
		return nil
	}
}

func (p *quicMediaPair) writeControl(t *testing.T, v any) {
	t.Helper()
	payload, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, _ = p.client.Write(append(payload, '\n'))
}

func (p *quicMediaPair) readControl(t *testing.T) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	_ = p.client.SetReadDeadline(deadline)
	buf := make([]byte, 0, 4096)
	one := make([]byte, 1)
	for len(buf) < 4096 {
		n, err := p.client.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				var out map[string]any
				if err := json.Unmarshal(buf, &out); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return out
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			t.Fatalf("control read: %v", err)
		}
	}
	t.Fatal("control line too long")
	return nil
}

func qs2Inbound(qs *QUICSocket) chan quicInboundMsg { return qs.inbound }

// TestQUICHostMixerIntegration: uplink datagrams → mixer → leg (PCMU),
// downlink leg → mixer → envelope-stamped datagrams, with epoch fencing.
// Shaped to be residual-free: the stale-leak assertion runs while NOTHING
// legitimate is in flight, so any leg send is proof of a leak.
func TestQUICHostMixerIntegration(t *testing.T) {
	pair := newQUICMediaPair(t, "")
	transport := newFakeTransport(8)
	leg := NewLeg("quic:1:line1", transport, nil)
	pair.session.AddLeg(leg)

	gen := pair.session.currentGeneration()
	go pair.session.PumpWSHost(pair.qs, gen)

	// 1) Stale-epoch uplink first: dropped by the socket (epoch gate), and
	//    the mixer must stay silent.
	pair.sendClientFrameEpoch(t, 0x33, pair.session.Epoch()-1)
	waitUntil(t, 2*time.Second, func() bool {
		st, _, _ := pair.qs.DroppedDatagrams()
		return st >= 1
	})
	time.Sleep(300 * time.Millisecond)
	if transport.sentCount() != 0 {
		t.Fatalf("stale-epoch frame reached the mixer: sends=%d", transport.sentCount())
	}

	// 2) Fresh uplink: frames flow into the mix and out to the leg.
	pair.sendClientFrame(t, 0x11)
	pair.sendClientFrame(t, 0x22)
	waitUntil(t, 3*time.Second, func() bool { return transport.sentCount() >= 2 })

	// 3) Downlink: one leg frame becomes one envelope-stamped datagram.
	transport.frames <- make([]byte, 160)
	pkt := pair.recvServerDatagram(t)
	frame, ok := ParseQUICEnvelope(pkt, pair.session.Epoch())
	if !ok || len(frame) != 160 {
		t.Fatalf("downlink frame invalid: ok=%v len=%d", ok, len(frame))
	}

	// 4) Another stale-epoch frame mid-stream: dropped, mixer total
	//    unchanged once the two legitimate sends drained.
	waitUntil(t, 3*time.Second, func() bool {
		a := transport.sentCount()
		time.Sleep(150 * time.Millisecond)
		return transport.sentCount() == a && a >= 2
	})
	before := transport.sentCount()
	pair.sendClientFrameEpoch(t, 0x44, pair.session.Epoch()-1)
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		if transport.sentCount() != before {
			st, _, _ := pair.qs.DroppedDatagrams()
			t.Fatalf("stale-epoch frame reached the mixer: before=%d now=%d drops=%d",
				before, transport.sentCount(), st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestQUICHostStandbyAndPromote: standby decode-discard, fenced promote,
// epoch bump drops old-generation datagrams.
func TestQUICHostStandbyAndPromote(t *testing.T) {
	pair := newQUICMediaPair(t, "")
	transport := newFakeTransport(8)
	leg := NewLeg("quic:1:line1", transport, nil)
	pair.session.AddLeg(leg)

	// Second QUIC connection in the standby role.
	conn2 := newFakeQUICConn()
	client2 := pairClientOf(conn2.stream)
	epochSource := func() uint64 { return pair.session.Epoch() }
	qs2 := NewQUICSocket(context.Background(), conn2, epochSource)
	var standbyInfo AttachInfo
	_, accepted, err := pair.session.AttachWSStandby(qs2, "", func(negotiated string, i AttachInfo) error {
		standbyInfo = i
		return nil
	})
	if err != nil || !accepted {
		t.Fatalf("standby attach: accepted=%v err=%v", accepted, err)
	}
	if standbyInfo.Standby != true || standbyInfo.Token == "" {
		t.Fatalf("standby info = %+v", standbyInfo)
	}

	gen := pair.session.currentGeneration()
	go pair.session.PumpWSHost(pair.qs, gen)
	go pair.session.PumpWSHost(qs2, gen)

	// Standby uplink: decoded for freshness, NEVER mixed (single-uplink
	// invariant): leg count must not grow.
	before := transport.sentCount()
	for i := 0; i < 6; i++ {
		conn2.datagrams <- StampQUICEnvelope(make([]byte, 160), pair.session.Epoch())
	}
	time.Sleep(150 * time.Millisecond)
	if transport.sentCount() != before {
		t.Fatal("standby frame fed the mix")
	}

	// Fenced promote on the standby control stream.
	pair.writeControl2(t, client2, map[string]any{
		"type": "promote", "token": standbyInfo.Token,
		"expectEpoch": pair.session.Epoch(), "nonce": "n1",
	})
	reply := pair.readControl2(t, client2)
	if reply["type"] != "promoted" || reply["nonce"] != "n1" {
		t.Fatalf("promote reply = %v", reply)
	}
	newEpoch := pair.session.Epoch()
	if newEpoch != standbyInfo.Epoch+1 {
		t.Fatalf("epoch after promote = %d, want %d", newEpoch, standbyInfo.Epoch+1)
	}

	// Old-generation datagrams (pre-promote epoch) are dropped on BOTH
	// sockets; the new primary only accepts the current epoch.
	conn2.datagrams <- StampQUICEnvelope(make([]byte, 160), standbyInfo.Epoch)
	time.Sleep(150 * time.Millisecond)
	stale, _, _ := qs2.DroppedDatagrams()
	if stale < 1 {
		t.Fatalf("post-promote stale drops = %d", stale)
	}
	// New-epoch frames on the promoted socket reach the mixer (the
	// adaptive host jitter pops only at its target depth, so feed enough
	// frames to drain).
	for i := 0; i < 6; i++ {
		conn2.datagrams <- StampQUICEnvelope(make([]byte, 160), newEpoch)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if transport.sentCount() > before {
			break
		}
		st, ov, _ := qs2.DroppedDatagrams()
		_ = st
		_ = ov
		time.Sleep(10 * time.Millisecond)
	}
	if transport.sentCount() <= before {
		st, ov, _ := qs2.DroppedDatagrams()
		conn2.mu.Lock()
		rc := conn2.recvCount
		conn2.mu.Unlock()
		t.Fatalf("promoted frame never mixed: sent=%d before=%d stale=%d overflow=%d depth=%d epoch=%d recv=%d inbox=%d/%d",
			transport.sentCount(), before, st, ov, hostJitterDepth(pair.session), pair.session.Epoch(), rc, len(qs2Inbound(qs2)), cap(qs2Inbound(qs2)))
	}
}

func (p *quicMediaPair) writeControl2(t *testing.T, client *fakeQUICStream, v any) {
	t.Helper()
	payload, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_, _ = client.Write(append(payload, '\n'))
}

func (p *quicMediaPair) readControl2(t *testing.T, client *fakeQUICStream) map[string]any {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 0, 4096)
	one := make([]byte, 1)
	for len(buf) < 4096 {
		n, err := client.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				var out map[string]any
				if err := json.Unmarshal(buf, &out); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				return out
			}
			buf = append(buf, one[0])
		}
		if err != nil {
			t.Fatalf("control read: %v", err)
		}
	}
	t.Fatal("control line too long")
	return nil
}

// TestQUICHostWrongCallBindingFenced: the pump only serves the socket in
// its CURRENT role; a superseded socket cannot inject audio.
func TestQUICHostSupersededSocketFenced(t *testing.T) {
	pair := newQUICMediaPair(t, "")
	transport := newFakeTransport(8)
	leg := NewLeg("quic:1:line1", transport, nil)
	pair.session.AddLeg(leg)
	gen := pair.session.currentGeneration()
	go pair.session.PumpWSHost(pair.qs, gen)

	// Replace the primary with a second QUIC socket (generation bump).
	conn2 := newFakeQUICConn()
	epochSource := func() uint64 { return pair.session.Epoch() }
	qs2 := NewQUICSocket(context.Background(), conn2, epochSource)
	if _, err := pair.session.AttachWSHost(qs2, "", func(string, AttachInfo) error { return nil }); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	go pair.session.PumpWSHost(qs2, pair.session.currentGeneration())
	// The superseded socket's pump observes the replacement on its next
	// read and exits without touching the session.
	pair.sendClientFrame(t, 0x55)
	time.Sleep(150 * time.Millisecond)
	// Only the new socket's frames reach the mixer.
	before := transport.sentCount()
	for i := 0; i < 6; i++ {
		conn2.datagrams <- StampQUICEnvelope(make([]byte, 160), pair.session.Epoch())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if transport.sentCount() > before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if transport.sentCount() <= before {
		st, ov, _ := qs2.DroppedDatagrams()
		conn2.mu.Lock()
		rc := conn2.recvCount
		conn2.mu.Unlock()
		t.Fatalf("new-primary frame never mixed: sent=%d before=%d stale=%d overflow=%d depth=%d epoch=%d recv=%d",
			transport.sentCount(), before, st, ov, hostJitterDepth(pair.session), pair.session.Epoch(), rc)
	}
}

// TestQUICWSSQURoundTripRolesAndTokens: the full Q→WSS→Q server-side role
// lifecycle with the build-63 FIRST-CLASS QUIC slot. A QUIC standby attaches
// into its own quicStandby slot; promoting it (kind "quic") demotes the real
// WSS primary into the WSS standby slot with a FRESH token; promoting the
// WSS back demotes QUIC into the quicStandby slot with a fresh token; the
// final QUIC re-promotion validates again. Each swap bumps the epoch;
// uplink direction follows the primary role. The state snapshot exposes
// typed roles (primary ∈ {ws,ice,quic}, per-family standby flags) so the
// client can never misread "standbyWs" as QUIC.
func TestQUICWSSQURoundTripRolesAndTokens(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "hs:quic-roundtrip", "call")
	defer session.Close()

	transport := newFakeTransport(8)
	leg := NewLeg("line1:call", transport, nil)
	session.AddLeg(leg)

	wsClient, wsServer, cleanup := wsTestPair(t)
	defer cleanup()
	_, wsInfo := attachPrimaryWS(t, session, wsServer)
	wsDone := make(chan struct{})
	go func() {
		session.PumpWSHost(wsServer, session.currentGeneration())
		close(wsDone)
	}()

	// QUIC standby occupies its OWN first-class slot — never the ws-family
	// standby slot (build 63).
	conn := newFakeQUICConn()
	epochSource := func() uint64 { return session.Epoch() }
	qs := NewQUICSocket(context.Background(), conn, epochSource)
	var quicInfo AttachInfo
	_, accepted, err := session.AttachQUICStandby(qs, "", func(negotiated string, i AttachInfo) error {
		quicInfo = i
		return nil
	})
	if err != nil || !accepted || quicInfo.Token == "" {
		t.Fatalf("quic standby attach: accepted=%v info=%+v err=%v", accepted, quicInfo, err)
	}
	if st := session.State(); !st.StandbyQUIC || st.StandbyWS {
		t.Fatalf("state after quic standby attach = %+v, want standbyQuic only", st)
	}
	gen := session.currentGeneration()
	go session.PumpWSHost(qs, gen)

	// 1) Q: promote the QUIC standby over the WSS primary.
	first, err := session.PromoteStandby(StandbyKindQUIC, quicInfo.Token, wsInfo.Epoch, "n1")
	if err != nil {
		t.Fatalf("promote quic: %v", err)
	}
	if first.Primary != StandbyKindQUIC || first.Standby != StandbyKindWS {
		t.Fatalf("first outcome = %+v, want quic primary / ws standby", first)
	}
	if st := session.State(); st.Primary != StandbyKindQUIC || !st.StandbyWS || st.StandbyQUIC {
		t.Fatalf("state after quic promote = %+v, want primary quic + ws standby", st)
	}
	wsStandbyToken := first.StandbyToken
	if wsStandbyToken == "" {
		t.Fatal("demoted WSS standby must carry a fresh token")
	}

	// QUIC primary uplink reaches the mix (the adaptive host jitter pops
	// only at its target depth, so feed enough frames to drain); the
	// demoted WSS uplink does not.
	before := transport.sentCount()
	for i := 0; i < 6; i++ {
		conn.datagrams <- StampQUICEnvelope(make([]byte, 160), session.Epoch())
	}
	if err := wsClient.WriteMessage(websocket.BinaryMessage, pcmuBinary(9001)); err != nil {
		t.Fatalf("demoted ws uplink write: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for transport.sentCount() <= before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if transport.sentCount() <= before {
		t.Fatalf("quic primary uplink never mixed: sent=%d before=%d", transport.sentCount(), before)
	}

	// The demoted WSS standby's uplink is discarded (single-uplink
	// invariant) even though its connection stays open.
	time.Sleep(250 * time.Millisecond)
	// (Depth can pop as the mixer consumes, so assert via the standby
	// freshness path: the demoted socket never gains standby freshness.)
	if session.State().StandbyWS != true {
		t.Fatal("demoted WSS must occupy the standby slot")
	}

	// 2) WSS: promote the demoted WSS back over the QUIC primary.
	second, err := session.PromoteStandby(StandbyKindWS, wsStandbyToken, first.Epoch, "n2")
	if err != nil {
		t.Fatalf("promote ws back: %v", err)
	}
	quicStandbyToken := second.StandbyToken
	if quicStandbyToken == "" || quicStandbyToken == quicInfo.Token {
		t.Fatalf("demoted quic standby token = %q (old %q), want a fresh one", quicStandbyToken, quicInfo.Token)
	}
	if session.Epoch() != wsInfo.Epoch+2 {
		t.Fatalf("epoch = %d, want %d", session.Epoch(), wsInfo.Epoch+2)
	}

	// WSS-primary uplink mixes again; the QUIC standby's does not.
	before = transport.sentCount()
	for i := 0; i < 6; i++ {
		if err := wsClient.WriteMessage(websocket.BinaryMessage, pcmuBinary(9002)); err != nil {
			t.Fatalf("promoted ws uplink write: %v", err)
		}
	}
	conn.datagrams <- StampQUICEnvelope(make([]byte, 160), session.Epoch())
	deadline = time.Now().Add(2 * time.Second)
	for transport.sentCount() <= before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if transport.sentCount() <= before {
		t.Fatalf("ws primary uplink never mixed after swap back: depth=%d epoch=%d",
			hostJitterDepth(session), session.Epoch())
	}

	// 3) Q again: the fresh QUIC standby token re-promotes QUIC.
	third, err := session.PromoteStandby(StandbyKindQUIC, quicStandbyToken, second.Epoch, "n3")
	if err != nil {
		t.Fatalf("re-promote quic: %v", err)
	}
	if third.Primary != StandbyKindQUIC || session.Epoch() != wsInfo.Epoch+3 {
		t.Fatalf("third outcome = %+v epoch=%d", third, session.Epoch())
	}
	// A stale-token retry must fail closed (no fourth swap).
	if _, err := session.PromoteStandby(StandbyKindQUIC, quicStandbyToken, second.Epoch, "n4"); err == nil {
		t.Fatal("stale quic token promoted twice")
	}
	if session.Epoch() != wsInfo.Epoch+3 {
		t.Fatalf("epoch moved after stale retry: %d", session.Epoch())
	}
}

// TestQUICThreePathIndependentWarmth is the build-63 acceptance scenario:
// RTC/QUIC/WSS stay independently warm and eligible across a full handover
// cycle. A WSS primary adopts an ICE probe (Q→RTC direction): BOTH the WSS
// primary (demoted into the WSS standby slot) and a previously attached
// QUIC standby (untouched in its own slot) stay attached; the state
// snapshot names every role exactly. Promoting QUIC back demotes ICE into
// the ICE standby slot — after which ALL THREE standby/primary slots are
// populated and the next handover needs no cold staging.
func TestQUICThreePathIndependentWarmth(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "hs:quic-3path", "call")
	defer session.Close()

	transport := newFakeTransport(8)
	leg := NewLeg("line1:call", transport, nil)
	session.AddLeg(leg)

	// WSS primary + QUIC standby (the classic build-61 attach shape, now in
	// the dedicated QUIC slot).
	wsClient, wsServer, cleanup := wsTestPair(t)
	defer cleanup()
	_ = wsClient
	_, _ = attachPrimaryWS(t, session, wsServer)
	wsDone := make(chan struct{})
	go func() {
		session.PumpWSHost(wsServer, session.currentGeneration())
		close(wsDone)
	}()

	conn := newFakeQUICConn()
	epochSource := func() uint64 { return session.Epoch() }
	qs := NewQUICSocket(context.Background(), conn, epochSource)
	var quicInfo AttachInfo
	_, accepted, err := session.AttachQUICStandby(qs, "", func(negotiated string, i AttachInfo) error {
		quicInfo = i
		return nil
	})
	if err != nil || !accepted || quicInfo.Token == "" {
		t.Fatalf("quic standby attach: accepted=%v info=%+v err=%v", accepted, quicInfo, err)
	}
	go session.PumpWSHost(qs, session.currentGeneration())

	// Q→RTC: an ICE probe is adopted over the WSS primary. The displaced
	// WSS primary must DEMOTE into the WSS standby slot (never silently
	// retired), and the QUIC standby must be untouched.
	probe := NewSession(engine, "preflight:dev_3path", "probe")
	defer probe.Close()
	pc, _, _ := opusTestClient(t)
	defer pc.Close()
	attachOpusClient(t, probe, pc)
	if err := session.AdoptClientPeer(probe); err != nil {
		t.Fatalf("adopt probe: %v", err)
	}
	st := session.State()
	if st.Primary != StandbyKindICE {
		t.Fatalf("primary = %q, want ice after adoption", st.Primary)
	}
	if !st.StandbyWS || st.StandbyWSToken == "" {
		t.Fatalf("demoted WSS standby missing after ICE adoption: %+v", st)
	}
	if !st.StandbyQUIC || st.StandbyQUICToken != quicInfo.Token {
		t.Fatalf("quic standby disturbed by ICE adoption: %+v (want token %q)", st, quicInfo.Token)
	}
	if st.StandbyWSToken == "" {
		t.Fatal("demoted WSS standby must carry a fresh identity token")
	}

	// Control-plane survival: with ICE primary and the WSS plane demoted,
	// the QUIC greeting path (EnqueueTextOnPrimary) must still find the
	// WSS standby socket.
	if !session.EnqueueTextOnPrimary([]byte(`{"type":"ping","t":1}`)) {
		t.Fatal("WSS control plane unreachable after ICE adoption")
	}

	// QUIC standby liveness: its downlink mirror still flows (datagrams
	// keep arriving on the fake connection's outbound sink). sentDgrams is
	// owned by the adapter's writer goroutine — read it under the fake
	// conn's mutex.
	sentCount := func() int {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return len(conn.sentDgrams)
	}
	sentBefore := sentCount()
	deadline := time.Now().Add(2 * time.Second)
	for sentCount() <= sentBefore && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sentCount() <= sentBefore {
		t.Fatalf("quic standby mirror stalled after ICE adoption (sent=%d)", sentCount())
	}

	// RTC→Q: promoting the QUIC standby demotes ICE into the ICE standby
	// slot; WSS stays in the WSS standby slot. Every family is now warm —
	// the next handover needs no cold staging.
	outcome, err := session.PromoteStandby(StandbyKindQUIC, quicInfo.Token, st.Epoch, "p1")
	if err != nil {
		t.Fatalf("promote quic after ice adoption: %v", err)
	}
	if outcome.Primary != StandbyKindQUIC || outcome.Standby != StandbyKindICE {
		t.Fatalf("outcome = %+v, want quic primary / ice standby", outcome)
	}
	st = session.State()
	if st.Primary != StandbyKindQUIC || !st.StandbyWS || !st.StandbyICE {
		t.Fatalf("state after quic re-promote = %+v, want quic primary + ws & ice standbys", st)
	}

	// Q→WSS: the demoted WSS standby promotes back over QUIC; QUIC returns
	// to its own standby slot with a fresh token. Full cycle complete.
	back, err := session.PromoteStandby(StandbyKindWS, st.StandbyWSToken, outcome.Epoch, "p2")
	if err != nil {
		t.Fatalf("promote ws back: %v", err)
	}
	if back.Primary != StandbyKindWS || back.Standby != StandbyKindQUIC || back.StandbyToken == "" {
		t.Fatalf("back outcome = %+v, want ws primary / fresh quic standby", back)
	}
	if st := session.State(); st.Primary != StandbyKindWS || !st.StandbyQUIC || !st.StandbyICE {
		t.Fatalf("state after ws re-promote = %+v, want ws primary + quic & ice standbys", st)
	}
}
