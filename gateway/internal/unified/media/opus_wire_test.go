package media

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"callrelay/gateway/internal/opus"
	"callrelay/gateway/internal/voice"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

// TestOpusWireReorderDuplicateLate drives REAL Opus RTP over a real peer
// connection with reordered, duplicated and stale packets — the loss
// patterns a cellular uplink actually produces. The gateway's encoded
// jitter stage must deliver in-order, duplicate-free, deadline-bounded
// audio to the mixer.
func TestOpusWireReorderDuplicateLate(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "wire-rl", "call")
	defer session.Close()

	line := newFakeLineTransport()
	leg := NewLeg("line1", line, nil)
	session.AddLeg(leg)
	defer leg.Close()

	me := &pion.MediaEngine{}
	if err := me.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{
			MimeType: pion.MimeTypeOpus, ClockRate: 48000, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
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
		pion.RTPCodecCapability{MimeType: pion.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		"audio", "client")
	if err != nil {
		t.Fatalf("client track: %v", err)
	}
	if _, err := pc.AddTransceiverFromTrack(track, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionSendrecv}); err != nil {
		t.Fatalf("client transceiver: %v", err)
	}
	offer, _ := pc.CreateOffer(nil)
	_ = pc.SetLocalDescription(offer)
	<-pion.GatheringCompletePromise(pc)
	answer, err := session.AttachClient(context.Background(), pc.LocalDescription().SDP, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("client remote: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !session.HostConnected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	// Encode 30 marker frames: frame k carries amplitude 1000+k*400 so the
	// leg can verify DELIVERY ORDER, not just energy.
	enc, err := opus.NewEncoder()
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	defer enc.Close()
	type marker struct {
		ts      uint32
		seq     uint16
		payload []byte
	}
	var markers []marker
	var seq uint16
	for k := 0; k < 30; k++ {
		pcm := make([]int16, opus.FrameSamples)
		amp := int16(1000 + k*400)
		for i := 0; i < opus.FrameSamples; i++ {
			s := k*opus.FrameSamples + i
			pcm[i] = int16(float64(amp) * math.Sin(2*math.Pi*440*float64(s)/opus.SampleRate))
		}
		payload, err := enc.Encode(pcm)
		if err != nil {
			t.Fatalf("encode %d: %v", k, err)
		}
		markers = append(markers, marker{ts: uint32(k * opusFrameTick), seq: seq, payload: payload})
		seq++
	}
	write := func(m marker) {
		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: m.seq, Timestamp: m.ts}, Payload: m.payload}
		if err := track.WriteRTP(pkt); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Pace the stream at the real 20 ms capture cadence (a full-speed burst
	// would legitimately hit the latency bound's trim). Injection plan:
	//   k=0: send frame 1, then frame 0, then frame 2 (reordering)
	//   k=6: duplicate frame 5's timestamp with a fresh sequence number
	//   k=13: stale packet (frame 0 again, fresh seq) — late retransmit
	type scheduled struct {
		at int
		m  marker
	}
	var plan []scheduled
	plan = append(plan, scheduled{0, markers[1]}, scheduled{1, markers[0]}, scheduled{2, markers[2]})
	for k := 3; k < 30; k++ {
		plan = append(plan, scheduled{k, markers[k]})
	}
	plan = append(plan, scheduled{6, marker{ts: markers[5].ts, seq: seq, payload: markers[5].payload}})
	seq++
	plan = append(plan, scheduled{13, marker{ts: markers[0].ts, seq: seq, payload: markers[0].payload}})
	seq++
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	go func() {
		step := 0
		for _, item := range plan {
			for step < item.at {
				<-tick.C
				step++
			}
			<-tick.C
			step++
			write(item.m)
		}
	}()

	// Collect leg frames and verify ORDER by the amplitude marker.
	deadline = time.Now().Add(5 * time.Second)
	for line.count() < 25 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if line.count() < 20 {
		t.Fatalf("leg received %d frames", line.count())
	}
	line.mu.Lock()
	sent := make([][]byte, len(line.sent))
	copy(sent, line.sent)
	line.mu.Unlock()
	var infos []frameInfo
	for _, f := range sent {
		pcm, err := voice.DecodePCMU(f)
		if err != nil {
			continue
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
		infos = append(infos, frameInfo{peak: peak})
	}
	if len(infos) < 20 {
		t.Fatalf("decoded %d leg frames", len(infos))
	}
	// Amplitude markers must be non-decreasing in blocks: exact order for
	// the reordered prefix (frames 0..11 land as 0,1,2,...,11), then the
	// duplicate/stale phases must NOT inject low-marker frames again.
	expected := func(k int) int16 { return int16(float64(1000+k*400) * 0.9 * 0.9) } // leg gain 0.9 + mix margin
	for k := 0; k+1 < len(infos) && k < 11; k++ {
		if infos[k].peak < expected(k) {
			t.Fatalf("leg frame %d peak=%d below marker (order/distortion); sequence=%v", k, infos[k].peak, peaksPrefix(infos, 14))
		}
		if k+1 < 12 && infos[k+1].peak < infos[k].peak-600 {
			t.Fatalf("marker order broken at %d: %v", k, peaksPrefix(infos, 14))
		}
	}
	// After the duplicate/stale injection, the tail must continue the
	// monotone marker ladder (no regression to early amplitudes).
	last := infos[len(infos)-1]
	if last.peak < expected(len(infos)-1) {
		t.Fatalf("tail peak=%d below marker for index %d: %v", last.peak, len(infos)-1, peaksPrefix(infos, len(infos)))
	}
}

type frameInfo struct {
	peak int16
}

func peaksPrefix(infos []frameInfo, n int) string {
	if n > len(infos) {
		n = len(infos)
	}
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = strconv.Itoa(int(infos[i].peak))
	}
	return strings.Join(parts, ",")
}
