package media

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/opus"
	"callrelay/gateway/internal/voice"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

// fakeLineTransport records PCMU frames the mixer sends to one line leg.
type fakeLineTransport struct {
	mu     sync.Mutex
	sent   [][]byte
	frames chan []byte
	once   sync.Once
}

func newFakeLineTransport() *fakeLineTransport {
	return &fakeLineTransport{frames: make(chan []byte, 16)}
}

func (f *fakeLineTransport) Frames() <-chan []byte { return f.frames }
func (f *fakeLineTransport) SendPCMU(b []byte) error {
	cp := make([]byte, len(b))
	copy(cp, b)
	f.mu.Lock()
	f.sent = append(f.sent, cp)
	f.mu.Unlock()
	return nil
}
func (f *fakeLineTransport) Close() {
	f.once.Do(func() { close(f.frames) })
}

func (f *fakeLineTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func zeroCrossings(pcm []int16) int {
	crossings := 0
	for i := 1; i < len(pcm); i++ {
		if (pcm[i-1] >= 0) != (pcm[i] >= 0) {
			crossings++
		}
	}
	return crossings
}

// opusTestClient builds a real Opus-capable pion client like the new iOS
// app: Opus preferred, PCMU fallback, TWCC extension negotiated.
func opusTestClient(t *testing.T) (*pion.PeerConnection, *pion.TrackLocalStaticRTP, chan *rtp.Packet) {
	t.Helper()
	me := &pion.MediaEngine{}
	if err := me.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{
			MimeType: pion.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, pion.RTPCodecTypeAudio); err != nil {
		t.Fatalf("client opus codec: %v", err)
	}
	if err := me.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		PayloadType:        0,
	}, pion.RTPCodecTypeAudio); err != nil {
		t.Fatalf("client pcmu codec: %v", err)
	}
	if err := me.RegisterHeaderExtension(
		pion.RTPHeaderExtensionCapability{URI: transportCCURI}, pion.RTPCodecTypeAudio); err != nil {
		t.Fatalf("client twcc ext: %v", err)
	}
	api := pion.NewAPI(pion.WithMediaEngine(me))
	pc, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("client pc: %v", err)
	}
	track, err := pion.NewTrackLocalStaticRTP(
		pion.RTPCodecCapability{MimeType: pion.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "client")
	if err != nil {
		t.Fatalf("client track: %v", err)
	}
	if _, err := pc.AddTransceiverFromTrack(track, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionSendrecv}); err != nil {
		t.Fatalf("client transceiver: %v", err)
	}
	received := make(chan *rtp.Packet, 512)
	pc.OnTrack(func(remote *pion.TrackRemote, _ *pion.RTPReceiver) {
		go func() {
			for {
				pkt, _, err := remote.ReadRTP()
				if err != nil {
					return
				}
				cp := *pkt
				cp.Payload = make([]byte, len(pkt.Payload))
				copy(cp.Payload, pkt.Payload)
				received <- &cp
			}
		}()
	})
	return pc, track, received
}

// TestOpusNegotiationAndBidirectionalMedia is the real-path proof: an
// Opus-preferring client negotiates Opus against the gateway, streams a
// synthetic voiced tone UP (client libopus → gateway decode → leg PCM) and
// receives it BACK (gateway encode → client decode), with the RFC 7587 48 kHz
// timestamp cadence verified on the wire.
func TestOpusNegotiationAndBidirectionalMedia(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "opus-loop", "call")
	defer session.Close()
	session.SetProbeEcho(true)

	line := newFakeLineTransport()
	leg := NewLeg("line1", line, nil)
	session.AddLeg(leg)
	defer leg.Close()

	pc, track, received := opusTestClient(t)
	defer pc.Close()

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if !offerPrefersOpus(offer.SDP) {
		t.Fatalf("test client offer does not prefer opus")
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("client local: %v", err)
	}
	<-pion.GatheringCompletePromise(pc)
	answer, err := session.AttachClient(context.Background(), pc.LocalDescription().SDP, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	session.codecMu.Lock()
	codec, pt := session.clientCodec, session.payloadType
	session.codecMu.Unlock()
	if codec != codecOpus {
		t.Fatalf("negotiated codec = %q, want opus", codec)
	}
	if pt != defaultOpusPT {
		t.Fatalf("opus payload type = %d, want %d", pt, defaultOpusPT)
	}
	if !strings.Contains(answer, "opus/48000") {
		t.Fatalf("answer lacks the opus rtpmap:\n%s", answer)
	}
	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("client remote: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for !session.HostConnected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !session.HostConnected() {
		t.Fatalf("gateway never saw the host connect")
	}

	// Client encodes a 440 Hz tone with the reference libopus and streams
	// it at the 20 ms RFC 7587 cadence.
	enc, err := opus.NewEncoder()
	if err != nil {
		t.Fatalf("client encoder: %v", err)
	}
	defer enc.Close()
	tone := make([]int16, opus.FrameSamples)
	for i := 0; i < opus.FrameSamples; i++ {
		tone[i] = int16(12000 * math.Sin(2*math.Pi*440*float64(i)/opus.SampleRate))
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		var ts uint32
		var written int
		defer func() { t.Logf("client wrote %d packets", written) }()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			payload, err := enc.Encode(tone)
			if err != nil {
				return
			}
			werr := track.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: uint16(written), Timestamp: ts},
				Payload: payload,
			})
			written++
			if werr != nil && written < 10 {
				t.Logf("client write #%d err: %v", written, werr)
			}
			ts += opusFrameTick
		}
	}()

	// Downlink: collect ~1.5 s of echoed packets and verify tone, energy
	// and the 960-tick timestamp cadence.
	var payloads [][]byte
	var timestamps []uint32
	timeout := time.After(2500 * time.Millisecond)
	for len(payloads) < 50 {
		select {
		case pkt := <-received:
			payloads = append(payloads, pkt.Payload)
			timestamps = append(timestamps, pkt.Timestamp)
		case <-timeout:
			goto collected
		}
	}
collected:
	if len(payloads) < 40 {
		t.Fatalf("received %d echoed packets, want >= 40", len(payloads))
	}
	for i := 1; i < len(timestamps); i++ {
		if got := timestamps[i] - timestamps[i-1]; got != opusFrameTick {
			t.Fatalf("timestamp step = %d, want %d (RFC 7587 clock)", got, opusFrameTick)
		}
	}
	dec, err := opus.NewDecoder()
	if err != nil {
		t.Fatalf("client decoder: %v", err)
	}
	defer dec.Close()
	var pcm []int16
	for _, payload := range payloads {
		frame, err := dec.Decode(payload)
		if err != nil {
			t.Fatalf("client decode: %v", err)
		}
		pcm = append(pcm, frame...)
	}
	var energy int64
	for _, s := range pcm {
		energy += int64(s) * int64(s)
	}
	if energy == 0 {
		t.Fatalf("downlink energy is zero — gateway encode never ran")
	}
	crossings := zeroCrossings(pcm)
	expected := 2 * 440 * len(pcm) / opus.SampleRate
	if crossings < expected*3/4 || crossings > expected*5/4 {
		t.Fatalf("zero crossings = %d, want ~%d (tone distorted)", crossings, expected)
	}

	// Uplink: the line leg must carry the decoded client tone as PCM.
	deadline = time.Now().Add(2 * time.Second)
	for line.count() < 30 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if line.count() < 20 {
		t.Fatalf("leg received %d frames, want >= 20", line.count())
	}
	line.mu.Lock()
	sent := make([][]byte, len(line.sent))
	copy(sent, line.sent)
	line.mu.Unlock()
	var legPCM []int16
	for _, frame := range sent {
		decoded, err := voice.DecodePCMU(frame)
		if err != nil {
			continue
		}
		legPCM = append(legPCM, decoded...)
	}
	var legEnergy int64
	for _, s := range legPCM {
		legEnergy += int64(s) * int64(s)
	}
	if legEnergy == 0 {
		t.Fatalf("uplink leg energy is zero — gateway decode never ran")
	}
	legCrossings := zeroCrossings(legPCM)
	legExpected := 2 * 440 * len(legPCM) / voice.SampleRate
	if legCrossings < legExpected*3/4 || legCrossings > legExpected*5/4 {
		t.Fatalf("leg zero crossings = %d, want ~%d", legCrossings, legExpected)
	}
}

// TestPCMUOfferStillNegotiatesPCMU locks the fallback: a build ≤25-style
// PCMU-only offer gets a PCMU answer and plain G.711 media in both
// directions — no Opus machinery is created.
func TestPCMUOfferStillNegotiatesPCMU(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "pcmu-fallback", "call")
	defer session.Close()
	session.SetProbeEcho(true)

	me := &pion.MediaEngine{}
	if err := me.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		PayloadType:        0,
	}, pion.RTPCodecTypeAudio); err != nil {
		t.Fatalf("client codec: %v", err)
	}
	api := pion.NewAPI(pion.WithMediaEngine(me))
	pc, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("client pc: %v", err)
	}
	defer pc.Close()
	track, err := pion.NewTrackLocalStaticRTP(
		pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "client")
	if err != nil {
		t.Fatalf("client track: %v", err)
	}
	if _, err := pc.AddTransceiverFromTrack(track, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionSendrecv}); err != nil {
		t.Fatalf("client transceiver: %v", err)
	}
	received := make(chan *rtp.Packet, 256)
	pc.OnTrack(func(remote *pion.TrackRemote, _ *pion.RTPReceiver) {
		go func() {
			for {
				pkt, _, err := remote.ReadRTP()
				if err != nil {
					return
				}
				cp := *pkt
				cp.Payload = make([]byte, len(pkt.Payload))
				copy(cp.Payload, pkt.Payload)
				received <- &cp
			}
		}()
	})
	offer, _ := pc.CreateOffer(nil)
	_ = pc.SetLocalDescription(offer)
	<-pion.GatheringCompletePromise(pc)
	answer, err := session.AttachClient(context.Background(), pc.LocalDescription().SDP, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	session.codecMu.Lock()
	codec := session.clientCodec
	session.codecMu.Unlock()
	if codec != codecPCMU {
		t.Fatalf("negotiated codec = %q, want pcmu fallback", codec)
	}
	if strings.Contains(answer, "opus") {
		t.Fatalf("PCMU-only offer got an opus answer:\n%s", answer)
	}
	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("client remote: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !session.HostConnected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// Stream PCMU; the probe echo must return G.711 frames (160 bytes).
	samples := make([]int16, voice.FrameSamples)
	for i := range samples {
		samples[i] = 1000
	}
	frame, _ := voice.EncodePCMU(samples)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		var ts uint32
		var written int
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			if err := track.WriteRTP(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: uint16(written), Timestamp: ts},
				Payload: frame,
			}); err != nil {
				return
			}
			written++
			ts += 160
		}
	}()
	select {
	case pkt := <-received:
		if len(pkt.Payload) != frameBytes {
			t.Fatalf("fallback path payload = %d bytes, want %d PCMU", len(pkt.Payload), frameBytes)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no frame echoed on the PCMU fallback path")
	}
}
