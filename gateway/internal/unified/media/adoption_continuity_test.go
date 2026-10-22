package media

import (
	"context"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
)

// attachOpusClient connects the shared Opus test client to a session and
// waits until the session sees the peer connected.
func attachOpusClient(t *testing.T, session *Session, pc *pion.PeerConnection) {
	t.Helper()
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("local: %v", err)
	}
	<-pion.GatheringCompletePromise(pc)
	answer, err := session.AttachClient(context.Background(), pc.LocalDescription().SDP, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("remote: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !session.HostConnected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !session.HostConnected() {
		t.Fatalf("session %s never saw the peer connect", session.Key)
	}
}

// TestAdoptClientPeerPreservesRTPSequenceContinuity is the build-36
// regression for the field "direct route has RTP but no audible audio"
// defect.
//
// A preflight probe writes silence to the client track continuously
// (Opus DTX keeps the 20 ms cadence), so by the time it is adopted the
// client receiver has already consumed a stream of packets on the peer's
// SSRC. The live session used to install the adopted track with FRESH
// sequence/timestamp counters (0/0): the receiver then classified every
// post-adoption packet as a stale replay (RFC 3550 old-packet / NetEq
// duplicate logic) and discarded the modem-to-phone audio until the
// counters caught back up to the pre-adoption value — 18 s of perceived
// silence in the build-35 field call, longer than the call itself.
//
// The adopted session MUST continue the probe's RTP sequence number and
// 48 kHz timestamp so the phone sees one continuous stream across the
// transport handover.
func TestAdoptClientPeerPreservesRTPSequenceContinuity(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	probe := NewSession(engine, "preflight:dev_continuity", "probe")
	defer probe.Close()
	probe.SetProbeEcho(true)

	pc, _, received := opusTestClient(t)
	defer pc.Close()
	attachOpusClient(t, probe, pc)

	// Let the probe emit a real packet stream, then record the newest
	// packet the client has consumed.
	var lastSeq uint16
	var lastTS uint32
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case pkt := <-received:
			lastSeq, lastTS = pkt.SequenceNumber, pkt.Timestamp
		case <-time.After(20 * time.Millisecond):
		}
	}
	if lastSeq == 0 && lastTS == 0 {
		t.Fatalf("probe produced no RTP packets before adoption")
	}

	live := NewSession(engine, "line1:continuity", "call")
	defer live.Close()
	if err := live.AdoptClientPeer(probe); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	// The first post-adoption packet the receiver accepts must be the
	// natural continuation of the probe stream (a small forward step for
	// the probe ticks written between our snapshot and the adoption), and
	// it must arrive promptly. With fresh counters the sender has to
	// re-send as many packets as the probe stream already held before the
	// receiver stops treating them as stale (the probe ran 1.2 s = ~60
	// packets, so the gap is ~1.2 s of discarded audio); the 400 ms bound
	// fails that reset deterministically while the fixed stream continues
	// on the next 20 ms tick. Wrap-safe uint16 arithmetic per RFC 3550.
	postDeadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(postDeadline) {
		select {
		case pkt := <-received:
			seqAdvance := int16(pkt.SequenceNumber - lastSeq)
			if seqAdvance <= 0 {
				// A stale in-flight probe packet is allowed; keep looking.
				continue
			}
			if seqAdvance > 60 {
				t.Fatalf("adopted stream jumped %d packets (seq %d after %d): a restarted counter makes the receiver drop audio as stale",
					seqAdvance, pkt.SequenceNumber, lastSeq)
			}
			tsAdvance := int32(pkt.Timestamp - lastTS)
			want := int32(seqAdvance) * opusFrameTick
			if tsAdvance != want {
				t.Fatalf("timestamp advance = %d, want %d for %d packets (48 kHz cadence must continue across adoption)",
					tsAdvance, want, seqAdvance)
			}
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("no continuous post-adoption packet within 400 ms (last pre-adoption seq=%d ts=%d): the adopted session restarted the RTP counters so the receiver discards the audio as stale",
		lastSeq, lastTS)
}
