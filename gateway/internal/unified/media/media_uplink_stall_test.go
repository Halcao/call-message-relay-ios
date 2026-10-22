package media

import (
	"bytes"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/opus"
	"callrelay/gateway/internal/voice"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

// newStalledTestSession builds a Session WITHOUT the background mixer loop so
// every mixOnce() call in these tests is deterministic. Production always uses
// NewSession (which starts the loop); the fields below are exactly what
// NewSession initializes.
func newStalledTestSession(key string) *Session {
	return &Session{
		Key:         key,
		Kind:        "call",
		legs:        make(map[string]*Leg),
		stop:        make(chan struct{}),
		host:        newHostJitter(160),
		clientCodec: codecPCMU,
	}
}

// stallHostFromLive puts the session into the "host present but mic stalled"
// state: a primed jitter buffer whose last push is older than the live
// window. Returns the owned buffer for later resume pushes.
func stallHostFromLive(session *Session) *hostJitter {
	session.mu.Lock()
	session.hostPresent = true
	host := session.host
	session.mu.Unlock()
	host.pushPCM(0, []int16{1})
	host.pushPCM(160, []int16{2})
	host.mu.Lock()
	host.lastPush = time.Now().Add(-hostLiveWindow - 50*time.Millisecond)
	host.mu.Unlock()
	return host
}

func queueLegFrame(t *testing.T, leg *Leg, transport *fakeTransport, value int16) {
	t.Helper()
	transport.frames <- pcmFrame(value)
	waitUntil(t, time.Second, func() bool {
		leg.mu.Lock()
		defer leg.mu.Unlock()
		return len(leg.queue) > 0
	})
}

// toneFrame encodes one 20 ms 400 Hz tone at the given amplitude. Opus does
// not preserve DC, so codec round-trip tests must use a real tone.
func toneFrame(amplitude int16, freqHz float64) []byte {
	samples := make([]int16, voice.FrameSamples)
	for i := range samples {
		samples[i] = int16(float64(amplitude) * math.Sin(2*math.Pi*freqHz*float64(i)/8000.0))
	}
	frame, err := voice.EncodePCMU(samples)
	if err != nil {
		panic(err)
	}
	return frame
}

func queueLegTone(t *testing.T, leg *Leg, transport *fakeTransport, amplitude int16, freqHz float64) {
	t.Helper()
	transport.frames <- toneFrame(amplitude, freqHz)
	waitUntil(t, time.Second, func() bool {
		leg.mu.Lock()
		defer leg.mu.Unlock()
		return len(leg.queue) > 0
	})
}

func decodePCMUFrame(t *testing.T, payload []byte) []int16 {
	t.Helper()
	pcm, err := voice.DecodePCMU(payload)
	if err != nil {
		t.Fatalf("decode PCMU: %v", err)
	}
	return pcm
}

func meanAbs(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		if s < 0 {
			sum -= float64(s)
		} else {
			sum += float64(s)
		}
	}
	return sum / float64(len(pcm))
}

// stallTrackWriter is a fake pion TrackLocalWriter bound to a real
// TrackLocalStaticRTP: it captures exactly the RTP packets the mixer writes
// through the direct ICE client path.
type stallTrackWriter struct {
	mu      sync.Mutex
	packets []*rtp.Packet
}

func (w *stallTrackWriter) WriteRTP(header *rtp.Header, payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	packet := &rtp.Packet{Header: *header, Payload: append([]byte(nil), payload...)}
	w.packets = append(w.packets, packet)
	return len(payload), nil
}

func (w *stallTrackWriter) Write(b []byte) (int, error) { return len(b), nil }

func (w *stallTrackWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.packets)
}

func (w *stallTrackWriter) packet(i int) *rtp.Packet {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.packets[i]
}

type stallTrackContext struct {
	codecs []pion.RTPCodecParameters
	writer pion.TrackLocalWriter
	id     string
}

func (c *stallTrackContext) CodecParameters() []pion.RTPCodecParameters {
	return c.codecs
}
func (c *stallTrackContext) HeaderExtensions() []pion.RTPHeaderExtensionParameter { return nil }
func (c *stallTrackContext) SSRC() pion.SSRC                                      { return 4001 }
func (c *stallTrackContext) SSRCRetransmission() pion.SSRC                        { return 0 }
func (c *stallTrackContext) SSRCForwardErrorCorrection() pion.SSRC                { return 0 }
func (c *stallTrackContext) WriteStream() pion.TrackLocalWriter                   { return c.writer }
func (c *stallTrackContext) ID() string                                           { return c.id }
func (c *stallTrackContext) RTCPReader() interceptor.RTCPReader                   { return nil }

func bindStallTrack(t *testing.T, capability pion.RTPCodecCapability, pt pion.PayloadType, writer *stallTrackWriter) *pion.TrackLocalStaticRTP {
	t.Helper()
	track, err := pion.NewTrackLocalStaticRTP(capability, "audio", "callrelay")
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	ctx := &stallTrackContext{
		codecs: []pion.RTPCodecParameters{{RTPCodecCapability: capability, PayloadType: pt}},
		writer: writer,
		id:     "stall-test",
	}
	if _, err := track.Bind(ctx); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return track
}

// TestMixOnceUplinkStallKeepsDirectClientDownlink is the core field-bug
// reproduction on the direct (ICE) fan-out: a connected host goes from live
// to no host frames for longer than hostLiveWindow while fresh remote PCM
// keeps arriving. Before the fix the client heard only silence; now the
// client keeps receiving fresh unheld leg audio, no external leg ever
// receives a frame during starvation, the host buffer is not wiped, and host
// resume restores normal forwarding.
func TestMixOnceUplinkStallKeepsDirectClientDownlink(t *testing.T) {
	session := newStalledTestSession("call:stall-direct")
	defer session.Close()

	transport := newFakeTransport(8)
	leg := NewLeg("line1:stall", transport, nil)
	session.AddLeg(leg)

	writer := &stallTrackWriter{}
	capability := pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1}
	track := bindStallTrack(t, capability, 0, writer)
	session.mu.Lock()
	session.track = track
	session.mu.Unlock()

	host := stallHostFromLive(session)

	// First stalled tick: fresh remote PCM must reach the client, the host
	// buffer keeps its remaining priming frame, and the leg gets NOTHING.
	queueLegFrame(t, leg, transport, 600)
	session.mixOnce()
	waitUntil(t, time.Second, func() bool { return writer.count() == 1 })
	if got := transport.sentCount(); got != 0 {
		t.Fatalf("external leg received %d frames during uplink starvation", got)
	}
	pcm := decodePCMUFrame(t, writer.packet(0).Payload)
	if pcm[0] < 500 || pcm[0] > 600 {
		t.Fatalf("client downlink = %d, want fresh leg audio ~540", pcm[0])
	}
	if depth := host.depth(); depth == 0 {
		t.Fatal("present-but-stalled host buffer was wiped instead of keeping priming frames")
	}

	// Sustained starvation: every tick mixes the fresh leg frame to the
	// client while external-leg writes stay at ZERO.
	for i := 0; i < 3; i++ {
		queueLegFrame(t, leg, transport, int16(600+i))
		session.mixOnce()
		waitUntil(t, time.Second, func() bool { return writer.count() >= i+2 })
		if got := transport.sentCount(); got != 0 {
			t.Fatalf("tick %d wrote %d frames to an external leg during starvation", i, got)
		}
		pcm := decodePCMUFrame(t, writer.packet(i+1).Payload)
		if pcm[0] < 500 || pcm[0] > 600 {
			t.Fatalf("tick %d client downlink = %d, want fresh leg audio", i, pcm[0])
		}
	}

	// Host resumes (runtime contract: DropStaleHostAudio then hostPush):
	// external forwarding returns with live host audio.
	gen := session.generation
	pumpPush(session, gen, 320, 1000)
	pumpPush(session, gen, 480, 1000)
	session.mixOnce()
	waitUntil(t, time.Second, func() bool { return transport.sentCount() >= 1 })
	decoded := decodePCMUFrame(t, transport.sent[0])
	if decoded[0] < 850 || decoded[0] > 950 {
		t.Fatalf("resume leg audio = %d, want host ~900 (forwarding restored)", decoded[0])
	}
}

// TestMixOnceUplinkStallKeepsDirectOpusDownlink proves the same client
// downlink guarantee on the negotiated Opus codec: the direct track receives
// a real Opus-encoded, non-silent frame of the remote party while the leg
// receives nothing.
func TestMixOnceUplinkStallKeepsDirectOpusDownlink(t *testing.T) {
	session := newStalledTestSession("call:stall-direct-opus")
	defer session.Close()
	if err := session.setupClientCodec(codecOpus); err != nil {
		t.Fatalf("opus codec: %v", err)
	}

	transport := newFakeTransport(8)
	leg := NewLeg("line1:stall-opus", transport, nil)
	session.AddLeg(leg)

	writer := &stallTrackWriter{}
	capability := pion.RTPCodecCapability{MimeType: opusMime, ClockRate: opusClockRate, Channels: 2}
	track := bindStallTrack(t, capability, defaultOpusPT, writer)
	session.mu.Lock()
	session.track = track
	session.mu.Unlock()

	stallHostFromLive(session)

	queueLegTone(t, leg, transport, 6000, 400)
	session.mixOnce()
	waitUntil(t, time.Second, func() bool { return writer.count() == 1 })
	if got := transport.sentCount(); got != 0 {
		t.Fatalf("external leg received %d frames during uplink starvation", got)
	}
	packet := writer.packet(0)
	if packet.PayloadType != defaultOpusPT {
		t.Fatalf("client payload type = %d, want Opus %d", packet.PayloadType, defaultOpusPT)
	}
	dec, err := opus.NewDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer dec.Close()
	pcm, err := dec.Decode(packet.Payload)
	if err != nil {
		t.Fatalf("opus decode: %v", err)
	}
	mean := meanAbs(pcm)
	if mean < 1500 || mean > 6000 {
		t.Fatalf("client Opus downlink mean |sample| = %.1f, want remote 400 Hz tone (~3400)", mean)
	}
}

// TestMixOnceUplinkStallHeldLegNeverFeedsClient pins hold semantics on the
// client-only path: a held leg is drained and never contributes to what the
// caller hears, and no external leg receives anything.
func TestMixOnceUplinkStallHeldLegNeverFeedsClient(t *testing.T) {
	session := newStalledTestSession("call:stall-held")
	defer session.Close()

	activeTransport := newFakeTransport(8)
	activeLeg := NewLeg("line1:active", activeTransport, nil)
	session.AddLeg(activeLeg)
	heldTransport := newFakeTransport(8)
	heldLeg := NewLeg("line2:held", heldTransport, nil)
	session.AddLeg(heldLeg)
	heldLeg.SetHeld(true)

	writer := &stallTrackWriter{}
	capability := pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1}
	session.mu.Lock()
	session.track = bindStallTrack(t, capability, 0, writer)
	session.mu.Unlock()

	stallHostFromLive(session)

	queueLegFrame(t, activeLeg, activeTransport, 1000)
	queueLegFrame(t, heldLeg, heldTransport, 30000)
	session.mixOnce()
	waitUntil(t, time.Second, func() bool { return writer.count() == 1 })

	if got := activeTransport.sentCount(); got != 0 {
		t.Fatalf("active leg received %d frames during starvation", got)
	}
	if got := heldTransport.sentCount(); got != 0 {
		t.Fatalf("held leg received %d frames during starvation", got)
	}
	heldLeg.mu.Lock()
	heldQueue := len(heldLeg.queue)
	heldLeg.mu.Unlock()
	if heldQueue != 0 {
		t.Fatalf("held leg queue not drained: %d frames", heldQueue)
	}
	pcm := decodePCMUFrame(t, writer.packet(0).Payload)
	if pcm[0] < 800 || pcm[0] > 1000 {
		t.Fatalf("client mix = %d, want only the active leg ~900 (held audio leaked?)", pcm[0])
	}
}

// TestMixOnceHostAbsentKeepsIsolationAndClearsHost pins the abandoned-host
// security invariant: with hostPresent=false, every external leg is drained
// and receives nothing, the host buffer is wiped, and the client receives
// exactly silence — no fresh leg audio, no stale replay.
func TestMixOnceHostAbsentKeepsIsolationAndClearsHost(t *testing.T) {
	session := newStalledTestSession("call:absent")
	defer session.Close()

	transport1 := newFakeTransport(8)
	leg1 := NewLeg("line1:absent", transport1, nil)
	session.AddLeg(leg1)
	transport2 := newFakeTransport(8)
	leg2 := NewLeg("line2:absent", transport2, nil)
	session.AddLeg(leg2)

	writer := &stallTrackWriter{}
	capability := pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1}
	session.mu.Lock()
	session.track = bindStallTrack(t, capability, 0, writer)
	session.mu.Unlock()

	// Stale host audio buffered, then the host goes away (hostPresent stays
	// false; applyHostState/DetachWSHost are the production paths that clear).
	session.host.pushPCM(0, []int16{3000})
	session.host.pushPCM(160, []int16{3000})
	queueLegFrame(t, leg1, transport1, 1000)
	queueLegFrame(t, leg2, transport2, 2000)

	session.mixOnce()
	waitUntil(t, time.Second, func() bool { return writer.count() == 1 })

	if got := transport1.sentCount(); got != 0 {
		t.Fatalf("leg1 received %d frames while host absent", got)
	}
	if got := transport2.sentCount(); got != 0 {
		t.Fatalf("leg2 received %d frames while host absent", got)
	}
	for i, leg := range []*Leg{leg1, leg2} {
		leg.mu.Lock()
		remaining := len(leg.queue)
		leg.mu.Unlock()
		if remaining != 0 {
			t.Fatalf("leg%d queue not drained while host absent: %d frames", i+1, remaining)
		}
	}
	if depth := session.host.depth(); depth != 0 {
		t.Fatalf("host buffer not cleared while host absent: %d frames", depth)
	}
	pcm := decodePCMUFrame(t, writer.packet(0).Payload)
	for i, sample := range pcm {
		if sample != 0 {
			t.Fatalf("client sample[%d] = %d while host absent, want silence", i, sample)
		}
	}
}

// TestMixOnceUplinkStallKeepsWSPCMUDownlink drives the real WSS attach/pump/
// mixer path (PCMU): during starvation the client socket keeps receiving
// 160-byte frames of fresh remote audio while the external leg observes zero
// writes across repeated ticks.
func TestMixOnceUplinkStallKeepsWSPCMUDownlink(t *testing.T) {
	client, server, cleanup := wsTestPair(t)
	defer cleanup()

	session := newStalledTestSession("call:stall-ws-pcmu")
	defer session.Close()
	transport := newFakeTransport(8)
	leg := NewLeg("line1:stall-ws", transport, nil)
	session.AddLeg(leg)

	gen, err := session.AttachWSHost(server, "", func(codec string, _ AttachInfo) error {
		if codec != codecPCMU {
			t.Errorf("negotiated codec = %q, want pcmu", codec)
		}
		return server.WriteJSON(map[string]string{"type": "ready", "callId": "stall-ws"})
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	go session.PumpWSHost(server, gen)

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if mt, _, err := client.ReadMessage(); err != nil || mt != websocket.TextMessage {
		t.Fatalf("ready read: mt=%v err=%v", mt, err)
	}

	stallHostFromLive(session)
	for i := 0; i < 5; i++ {
		queueLegFrame(t, leg, transport, int16(600+i))
		session.mixOnce()
		if got := transport.sentCount(); got != 0 {
			t.Fatalf("tick %d wrote %d frames to an external leg during WSS starvation", i, got)
		}
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		mt, payload, err := client.ReadMessage()
		if err != nil || mt != websocket.BinaryMessage {
			t.Fatalf("tick %d downlink read: mt=%v err=%v", i, mt, err)
		}
		if len(payload) != frameBytes {
			t.Fatalf("tick %d WS PCMU frame = %d bytes, want %d", i, len(payload), frameBytes)
		}
		pcm := decodePCMUFrame(t, payload)
		if pcm[0] < 500 || pcm[0] > 600 {
			t.Fatalf("tick %d client WS downlink = %d, want fresh leg audio", i, pcm[0])
		}
	}
}

// TestMixOnceUplinkStallKeepsWSOpusDownlink is the WSS counterpart on the
// negotiated Opus codec: framed [seq u16 BE][payload] frames keep carrying
// non-silent remote audio to the client while no external leg is written.
func TestMixOnceUplinkStallKeepsWSOpusDownlink(t *testing.T) {
	client, server, cleanup := wsTestPair(t)
	defer cleanup()

	session := newStalledTestSession("call:stall-ws-opus")
	defer session.Close()
	transport := newFakeTransport(8)
	leg := NewLeg("line1:stall-ws-opus", transport, nil)
	session.AddLeg(leg)

	gen, err := session.AttachWSHost(server, codecOpus, func(codec string, _ AttachInfo) error {
		if codec != codecOpus {
			t.Errorf("negotiated codec = %q, want opus", codec)
		}
		return server.WriteJSON(map[string]any{"type": "ready", "callId": "stall-ws-opus", "codec": "opus"})
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	go session.PumpWSHost(server, gen)

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if mt, _, err := client.ReadMessage(); err != nil || mt != websocket.TextMessage {
		t.Fatalf("ready read: mt=%v err=%v", mt, err)
	}

	stallHostFromLive(session)
	// One continuous decoder in stream order mirrors the client receiver:
	// framing must stay gap-free while only the routing is under test.
	dec, err := opus.NewDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer dec.Close()
	for i := 0; i < 3; i++ {
		queueLegTone(t, leg, transport, 6000, 400)
		session.mixOnce()
		if got := transport.sentCount(); got != 0 {
			t.Fatalf("tick %d wrote %d frames to an external leg during Opus starvation", i, got)
		}
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		mt, payload, err := client.ReadMessage()
		if err != nil || mt != websocket.BinaryMessage {
			t.Fatalf("tick %d downlink read: mt=%v err=%v", i, mt, err)
		}
		if len(payload) < 3 || len(payload) >= frameBytes+2 {
			t.Fatalf("tick %d WS Opus frame = %d bytes, want framed sub-%d", i, len(payload), frameBytes+2)
		}
		seq := int(payload[0])<<8 | int(payload[1])
		if seq != i {
			t.Fatalf("tick %d WS Opus seq = %d, want %d", i, seq, i)
		}
		pcm, err := dec.Decode(payload[2:])
		if err != nil {
			t.Fatalf("tick %d opus decode: %v", i, err)
		}
		mean := meanAbs(pcm)
		if mean < 1500 || mean > 6000 {
			t.Fatalf("tick %d client WS Opus mean |sample| = %.1f, want remote 400 Hz tone", i, mean)
		}
	}
}

// TestMixOnceUplinkTransitionLogFiresOncePerEdge proves the bounded
// transition log: one line when a live uplink stalls, one when it resumes,
// never per tick.
func TestMixOnceUplinkTransitionLogFiresOncePerEdge(t *testing.T) {
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(previous)

	session := newStalledTestSession("call:stall-log")
	defer session.Close()
	transport := newFakeTransport(8)
	session.AddLeg(NewLeg("line1:log", transport, nil))

	host := session.host
	session.mu.Lock()
	session.hostPresent = true
	session.mu.Unlock()
	// Live observation first, so the stall below is a real edge.
	host.pushPCM(0, []int16{100})
	host.pushPCM(160, []int16{100})
	host.mu.Lock()
	host.lastPush = time.Now()
	host.mu.Unlock()
	session.mixOnce()

	// Five stalled ticks must produce exactly one stall line.
	host.mu.Lock()
	host.lastPush = time.Now().Add(-hostLiveWindow - 50*time.Millisecond)
	host.mu.Unlock()
	for i := 0; i < 5; i++ {
		session.mixOnce()
	}
	stalls := strings.Count(logBuf.String(), "media host uplink stalled")
	if stalls != 1 {
		t.Fatalf("stall log lines = %d across 5 stalled ticks, want 1", stalls)
	}

	// Resume: repeated live ticks must produce exactly one resume line.
	host.pushPCM(320, []int16{100})
	host.pushPCM(480, []int16{100})
	host.mu.Lock()
	host.lastPush = time.Now()
	host.mu.Unlock()
	for i := 0; i < 3; i++ {
		session.mixOnce()
	}
	resumes := strings.Count(logBuf.String(), "media host uplink resumed")
	if resumes != 1 {
		t.Fatalf("resume log lines = %d across resume ticks, want 1", resumes)
	}
}

// TestObserveUplinkStateResetsWhenHostGone pins the bookkeeping: a host that
// attaches and is already stalled on its first tick emits nothing (no false
// "stalled" edge), and detaching resets the state so a re-attach starts a
// fresh edge history.
func TestObserveUplinkStateResetsWhenHostGone(t *testing.T) {
	session := newStalledTestSession("call:edge")

	if stalled, resumed := session.observeUplinkState(true, false); stalled || resumed {
		t.Fatalf("first observation reported an edge: stalled=%v resumed=%v", stalled, resumed)
	}
	if stalled, resumed := session.observeUplinkState(false, false); stalled || resumed {
		t.Fatalf("host-gone observation reported an edge: stalled=%v resumed=%v", stalled, resumed)
	}
	if stalled, resumed := session.observeUplinkState(true, true); stalled || resumed {
		t.Fatalf("re-attach first observation reported an edge: stalled=%v resumed=%v", stalled, resumed)
	}
	stalled, resumed := session.observeUplinkState(true, false)
	if !stalled || resumed {
		t.Fatalf("stall edge missing: stalled=%v resumed=%v", stalled, resumed)
	}
	if stalled, resumed := session.observeUplinkState(true, false); stalled || resumed {
		t.Fatalf("repeated stall reported another edge: stalled=%v resumed=%v", stalled, resumed)
	}
	stalled, resumed = session.observeUplinkState(true, true)
	if stalled || !resumed {
		t.Fatalf("resume edge missing: stalled=%v resumed=%v", stalled, resumed)
	}
}

// TestMixOnceUplinkStallNoLegKeepsOpusCadence pins the no-leg-frame case on
// the direct Opus path (leg gap / priming / hold): the client must keep
// receiving one Opus packet per tick as silence, never nothing, and no
// external leg may be written.
func TestMixOnceUplinkStallNoLegKeepsOpusCadence(t *testing.T) {
	session := newStalledTestSession("call:stall-noleg-opus")
	defer session.Close()
	if err := session.setupClientCodec(codecOpus); err != nil {
		t.Fatalf("opus codec: %v", err)
	}
	transport := newFakeTransport(8)
	leg := NewLeg("line1:noleg", transport, nil)
	session.AddLeg(leg)

	writer := &stallTrackWriter{}
	capability := pion.RTPCodecCapability{MimeType: opusMime, ClockRate: opusClockRate, Channels: 2}
	session.mu.Lock()
	session.track = bindStallTrack(t, capability, defaultOpusPT, writer)
	session.mu.Unlock()

	stallHostFromLive(session)

	dec, err := opus.NewDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer dec.Close()
	for i := 0; i < 3; i++ {
		session.mixOnce()
		if got := transport.sentCount(); got != 0 {
			t.Fatalf("tick %d wrote %d frames to an external leg with no leg audio", i, got)
		}
		waitUntil(t, time.Second, func() bool { return writer.count() >= i+1 })
		packet := writer.packet(i)
		if packet.PayloadType != defaultOpusPT {
			t.Fatalf("tick %d payload type = %d, want Opus %d", i, packet.PayloadType, defaultOpusPT)
		}
		pcm, err := dec.Decode(packet.Payload)
		if err != nil {
			t.Fatalf("tick %d decode: %v", i, err)
		}
		if len(pcm) != voice.FrameSamples {
			t.Fatalf("tick %d decoded %d samples, want %d", i, len(pcm), voice.FrameSamples)
		}
		if mean := meanAbs(pcm); mean > 200 {
			t.Fatalf("tick %d silence mean |sample| = %.1f, want near zero", i, mean)
		}
	}
}

// TestMixOnceUplinkStallLegGapKeepsPCMUCadence: a mixer tick with no fresh
// leg frame (remote gap) still writes caller-side silence under PCMU, and
// the next fresh frame resumes. No external leg is written at any point.
func TestMixOnceUplinkStallLegGapKeepsPCMUCadence(t *testing.T) {
	session := newStalledTestSession("call:stall-gap-pcmu")
	defer session.Close()
	transport := newFakeTransport(8)
	leg := NewLeg("line1:gap", transport, nil)
	session.AddLeg(leg)

	writer := &stallTrackWriter{}
	capability := pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1}
	session.mu.Lock()
	session.track = bindStallTrack(t, capability, 0, writer)
	session.mu.Unlock()
	stallHostFromLive(session)

	queueLegFrame(t, leg, transport, 600) // tick 0: fresh frame
	session.mixOnce()
	session.mixOnce() // tick 1: gap (no fresh leg frame)
	queueLegFrame(t, leg, transport, 600)
	session.mixOnce() // tick 2: fresh frame again

	waitUntil(t, time.Second, func() bool { return writer.count() == 3 })
	if got := transport.sentCount(); got != 0 {
		t.Fatalf("external leg received %d frames during starvation/gap", got)
	}
	if pcm := decodePCMUFrame(t, writer.packet(0).Payload); pcm[0] < 500 || pcm[0] > 600 {
		t.Fatalf("tick 0 client = %d, want ~540", pcm[0])
	}
	if pcm := decodePCMUFrame(t, writer.packet(1).Payload); pcm[0] != 0 {
		t.Fatalf("gap tick client = %d, want silence 0", pcm[0])
	}
	if pcm := decodePCMUFrame(t, writer.packet(2).Payload); pcm[0] < 500 || pcm[0] > 600 {
		t.Fatalf("tick 2 client = %d, want ~540", pcm[0])
	}
}

// TestMixOnceProbeEchoStallDoesNotReplayStaleFrame: a detached probe whose
// host uplink stalls must write caller-side silence, never echo the stale
// buffered host frame; once the host is live again, probe echo semantics
// return unchanged.
func TestMixOnceProbeEchoStallDoesNotReplayStaleFrame(t *testing.T) {
	session := newStalledTestSession("call:probe-stall")
	defer session.Close()
	session.SetProbeEcho(true)

	writer := &stallTrackWriter{}
	capability := pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1}
	session.mu.Lock()
	session.track = bindStallTrack(t, capability, 0, writer)
	session.mu.Unlock()

	session.mu.Lock()
	session.hostPresent = true
	session.mu.Unlock()
	host := session.host
	staleFrame := make([]int16, voice.FrameSamples)
	for i := range staleFrame {
		staleFrame[i] = 3000
	}
	host.pushPCM(0, staleFrame)
	host.pushPCM(160, staleFrame)
	host.mu.Lock()
	host.lastPush = time.Now().Add(-hostLiveWindow - 50*time.Millisecond)
	host.mu.Unlock()

	session.mixOnce()
	waitUntil(t, time.Second, func() bool { return writer.count() == 1 })
	pcm := decodePCMUFrame(t, writer.packet(0).Payload)
	for i, sample := range pcm {
		if sample != 0 {
			t.Fatalf("stalled probe sample[%d] = %d, want silence (no stale echo)", i, sample)
		}
	}

	// Host live again: the probe echoes its own buffered frame, unchanged.
	host.mu.Lock()
	host.lastPush = time.Now()
	host.mu.Unlock()
	session.mixOnce()
	waitUntil(t, time.Second, func() bool { return writer.count() == 2 })
	pcm = decodePCMUFrame(t, writer.packet(1).Payload)
	if pcm[0] < 2000 {
		t.Fatalf("live probe echo = %d, want the host frame ~3000", pcm[0])
	}
}

// TestMixOnceUplinkStallKeepsRealPeerOpusDownlink is the real-RTP playout
// counterpart of the direct test: a real Opus peer connection (ICE/DTLS/RTP/
// RTCP) keeps a 20 ms packet cadence during host-uplink starvation, receives
// fresh leg audio as soon as a frame arrives, and the external leg is never
// written at any point.
func TestMixOnceUplinkStallKeepsRealPeerOpusDownlink(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "call:stall-realpeer", "call")
	defer session.Close()

	line := newFakeLineTransport()
	leg := NewLeg("line1:realpeer", line, nil)
	session.AddLeg(leg)
	defer leg.Close()

	pc, _, received := opusTestClient(t)
	defer pc.Close()
	attachOpusClient(t, session, pc)

	dec, err := opus.NewDecoder()
	if err != nil {
		t.Fatalf("decoder: %v", err)
	}
	defer dec.Close()

	// Phase 1: host present but never primed (stalled). Real RTP packets must
	// keep arriving at the RFC 7587 cadence as silence, and the leg must see
	// zero writes.
	var timestamps []uint32
	var silentMeans []float64
	deadline := time.Now().Add(3 * time.Second)
	for len(timestamps) < 3 && time.Now().Before(deadline) {
		select {
		case pkt := <-received:
			pcm, err := dec.Decode(pkt.Payload)
			if err != nil {
				t.Fatalf("decode starvation packet: %v", err)
			}
			timestamps = append(timestamps, pkt.Timestamp)
			silentMeans = append(silentMeans, meanAbs(pcm))
			if got := line.count(); got != 0 {
				t.Fatalf("leg written %d times during starvation", got)
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if len(timestamps) < 3 {
		t.Fatalf("received %d packets during starvation, want >= 3", len(timestamps))
	}
	for i := 1; i < len(timestamps); i++ {
		if step := timestamps[i] - timestamps[i-1]; step != opusFrameTick {
			t.Fatalf("timestamp step = %d, want %d (RFC 7587 cadence)", step, opusFrameTick)
		}
	}
	for i, mean := range silentMeans {
		if mean > 200 {
			t.Fatalf("starvation packet %d mean |sample| = %.1f, want silence", i, mean)
		}
	}

	// Phase 2: fresh remote audio must reach the real receiver while the leg
	// still gets nothing.
	tone := make([]int16, voice.FrameSamples)
	for i := range tone {
		tone[i] = int16(6000 * math.Sin(2*math.Pi*400*float64(i)/8000.0))
	}
	frame, err := voice.EncodePCMU(tone)
	if err != nil {
		t.Fatalf("leg encode: %v", err)
	}
	line.frames <- frame

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case pkt := <-received:
			pcm, err := dec.Decode(pkt.Payload)
			if err != nil {
				continue
			}
			if mean := meanAbs(pcm); mean > 1500 {
				if got := line.count(); got != 0 {
					t.Fatalf("leg written %d times during starvation", got)
				}
				return
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("no audible downlink packet after fresh leg audio arrived")
}
