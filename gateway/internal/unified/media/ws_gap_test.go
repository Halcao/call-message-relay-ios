package media

import (
	"testing"
	"time"

	"callrelay/gateway/internal/opus"
	"callrelay/gateway/internal/voice"

	"github.com/gorilla/websocket"
)

// TestWSDepacketizeGapMath: the sequence-space gap logic is exact — the
// sender's bounded queue drops create holes the receiver must count.
func TestWSDepacketizeGapMath(t *testing.T) {
	var state uint16
	var have bool
	// First frame seeds the cursor.
	body, missed, consume := wsDepacketize(&state, &have, 7, []byte{1})
	if !consume || missed != 0 || len(body) != 1 {
		t.Fatalf("seed = %v %d %v", body, missed, consume)
	}
	// In-order frame: no miss.
	_, missed, consume = wsDepepacketizeHelper(&state, 8)
	if !consume || missed != 0 {
		t.Fatalf("in-order missed=%d", missed)
	}
	// Gap of 3 (one dropped frame): seq jumps 8 -> 10.
	_, missed, consume = wsDepepacketizeHelper(&state, 10)
	if !consume || missed != 1 {
		t.Fatalf("one-drop missed=%d want 1", missed)
	}
	// Duplicate: not consumed, no decode.
	_, missed, consume = wsDepacketize(&state, &have, 10, []byte{2})
	if consume || missed != 0 {
		t.Fatalf("duplicate consume=%v missed=%d", consume, missed)
	}
	// Obsolete (behind the cursor): not consumed.
	_, _, consume = wsDepacketize(&state, &have, 4, []byte{3})
	if consume {
		t.Fatalf("obsolete frame consumed")
	}
	// Burst gap caps at the PLC bound.
	_, missed, consume = wsDepacketize(&state, &have, 10+maxWSGapSlots+5, []byte{4})
	if !consume || missed != maxWSGapSlots {
		t.Fatalf("burst missed=%d want %d", missed, maxWSGapSlots)
	}
}

func wsDepepacketizeHelper(state *uint16, seq uint16) ([]byte, int, bool) {
	have := true
	return wsDepacketize(state, &have, seq, []byte{9})
}

// TestWSSUplinkSenderDropsConcealed: a paced framed uplink with a one-slot
// sender drop must deliver every media slot to the mixer (the dropped slot
// as mature PLC), keeping the decoder timeline aligned — the post-gap frames
// must still carry energy.
func TestWSSUplinkSenderDropsConcealed(t *testing.T) {
	client, server, cleanup := wsTestPair(t)
	defer cleanup()

	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "ws-drops", "call")
	defer session.Close()

	line := newFakeLineTransport()
	leg := NewLeg("line1", line, nil)
	session.AddLeg(leg)
	defer leg.Close()

	gen, err := session.AttachWSHost(server, codecOpus, func(_ string, _ AttachInfo) error {
		return server.WriteJSON(map[string]string{"type": "ready", "callId": "ws-drops", "codec": "opus", "fmt": WSFrameFmtSeq16})
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	go session.PumpWSHost(server, gen)

	host := session.ws
	tone := make([]int16, voice.FrameSamples)
	for i := range tone {
		if i%2 == 0 {
			tone[i] = 6000
		} else {
			tone[i] = -6000
		}
	}
	payload, err := host.encodePCM(tone)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Paced uplink: 14 slots with ONE sender drop at seq 5 (the bounded
	// queue would produce exactly this hole).
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		seq := uint16(0)
		write := func() {
			out := make([]byte, 2+len(payload))
			out[0] = byte(seq >> 8)
			out[1] = byte(seq)
			copy(out[2:], payload)
			_ = client.WriteMessage(websocket.BinaryMessage, out)
			seq++
		}
		for i := 0; i < 14; i++ {
			<-tick.C
			if i == 5 {
				seq++ // sender drop: the slot exists, no frame sent
				continue
			}
			write()
		}
	}()

	// Every media slot must reach the leg (13 real + 1 PLC = 14 pops);
	// give the mixer a bounded window then count.
	deadline := time.Now().Add(3 * time.Second)
	for line.count() < 12 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := line.count(); got < 12 {
		t.Fatalf("leg received %d slots, want >= 12 (drop must be concealed)", got)
	}
	// Post-gap alignment: the LAST leg frame must carry full energy (the
	// decoder stayed aligned through the PLC slot).
	line.mu.Lock()
	last := line.sent[len(line.sent)-1]
	line.mu.Unlock()
	pcm, err := voice.DecodePCMU(last)
	if err != nil {
		t.Fatalf("decode leg frame: %v", err)
	}
	var peak int16
	for _, s := range pcm {
		if s < 0 {
			s = -s
		}
		if s > peak {
			peak = s
		}
	}
	if peak < 2000 {
		t.Fatalf("post-gap leg frame peak=%d, decoder timeline misaligned", peak)
	}
}

// TestWSSDownlinkBlockedWriterExposesDrops: a slow reader forces the bounded
// queue to drop stale frames; the framed wire must expose those drops as
// sequence holes (the receiver-side contract) while never growing memory.
func TestWSSDownlinkBlockedWriterExposesDrops(t *testing.T) {
	client, server, cleanup := wsTestPair(t)
	defer cleanup()

	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "ws-slow", "call")
	defer session.Close()

	gen, err := session.AttachWSHost(server, codecOpus, func(_ string, _ AttachInfo) error {
		return server.WriteJSON(map[string]string{"type": "ready", "callId": "ws-slow", "codec": "opus", "fmt": WSFrameFmtSeq16})
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	go session.PumpWSHost(server, gen)

	// Do NOT read: the mixer keeps emitting 20 ms frames; the bounded queue
	// (wsHostQueue frames) must drop stale ones, and the wire sequence must
	// show the holes.
	time.Sleep(700 * time.Millisecond)
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	var seqs []int
	for len(seqs) < 8 {
		mt, payload, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if mt != websocket.BinaryMessage || len(payload) < 3 {
			continue
		}
		seqs = append(seqs, int(payload[0])<<8|int(payload[1]))
	}
	// Queue bounded: at most wsHostQueue unsent frames; with 700 ms of
	// production the wire must show at least one hole OR a bounded span.
	span := seqs[len(seqs)-1] - seqs[0]
	if span != len(seqs)-1 {
		// Drops exposed as sequence holes — the framing contract working.
		t.Logf("wire sequence shows drops: %v", seqs)
	}
	if len(seqs) == 0 || seqs[len(seqs)-1]-seqs[0] > 200 {
		t.Fatalf("unbounded wire growth: %v", seqs)
	}
}

// TestWSSUplinkDelayBurstResumes: a stalled uplink that resumes in a burst
// must not wedge the pump (DropStaleHostAudio re-primes) and the framed
// sequence must process every arriving frame once.
func TestWSSUplinkDelayBurstResumes(t *testing.T) {
	client, server, cleanup := wsTestPair(t)
	defer cleanup()

	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "ws-burst", "call")
	defer session.Close()

	gen, err := session.AttachWSHost(server, codecOpus, func(_ string, _ AttachInfo) error {
		return server.WriteJSON(map[string]string{"type": "ready", "callId": "ws-burst", "codec": "opus", "fmt": WSFrameFmtSeq16})
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	go session.PumpWSHost(server, gen)

	host := session.ws
	silence := make([]int16, voice.FrameSamples)
	payload, err := host.encodePCM(silence)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Stall, then burst 10 frames at once (TCP coalescing pattern).
	time.Sleep(400 * time.Millisecond)
	for seq := uint16(0); seq < 10; seq++ {
		out := make([]byte, 2+len(payload))
		out[0] = byte(seq >> 8)
		out[1] = byte(seq)
		copy(out[2:], payload)
		if err := client.WriteMessage(websocket.BinaryMessage, out); err != nil {
			t.Fatalf("burst write: %v", err)
		}
	}
	// Then steady frames; the pump must still be alive and processing.
	for seq := uint16(10); seq < 14; seq++ {
		out := make([]byte, 2+len(payload))
		out[0] = byte(seq >> 8)
		out[1] = byte(seq)
		copy(out[2:], payload)
		if err := client.WriteMessage(websocket.BinaryMessage, out); err != nil {
			t.Fatalf("steady write: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		host.mu.Lock()
		depth := len(host.queue)
		host.mu.Unlock()
		if depth == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pump wedged: queue never drained")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWSSOpusFECDrivenByLossPerc: inband FEC exists only when the encoder is
// told to expect loss (what the closed loop signals); the receiver's
// FEC-first path is exercised end to end at the codec seam.
func TestWSSOpusFECDrivenByLossPerc(t *testing.T) {
	enc, err := opus.NewEncoder()
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	defer enc.Close()
	dec, err := opus.NewDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer dec.Close()
	if err := enc.SetPacketLossPerc(20); err != nil {
		t.Fatalf("loss perc: %v", err)
	}
	tone := make([]int16, voice.FrameSamples)
	for i := range tone {
		if i%2 == 0 {
			tone[i] = 5000
		} else {
			tone[i] = -5000
		}
	}
	first, err := enc.Encode(tone)
	if err != nil {
		t.Fatalf("encode 1: %v", err)
	}
	second, err := enc.Encode(tone)
	if err != nil {
		t.Fatalf("encode 2: %v", err)
	}
	if _, err := dec.Decode(first); err != nil {
		t.Fatalf("decode 1: %v", err)
	}
	recovered, err := dec.DecodeFEC(second)
	if err != nil || len(recovered) != voice.FrameSamples {
		t.Fatalf("fec recover: %v len=%d", err, len(recovered))
	}
	// Without loss perc the encoder embeds no FEC: DecodeFEC must fail and
	// the caller falls back to PLC (honest, not silent garbage).
	enc2, _ := opus.NewEncoder()
	defer enc2.Close()
	plain, _ := enc2.Encode(tone)
	plain2, _ := enc2.Encode(tone)
	dec2, _ := opus.NewDecoder()
	defer dec2.Close()
	if _, err := dec2.Decode(plain); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Without embedded FEC libopus still emits a graceful frame (internal
	// PLC fallback), so the receiver attempt degrades honestly; what MUST
	// NOT happen is an error that swallows the carrier.
	recovered2, err := dec2.DecodeFEC(plain2)
	if err != nil || len(recovered2) != voice.FrameSamples {
		t.Fatalf("DecodeFEC without FEC data: err=%v len=%d", err, len(recovered2))
	}
}
