package media

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"
)

// newTrickleSession attaches a real local offer with the trickle flag and
// returns the session, the attach token and the offer peer.
func newTrickleSession(t *testing.T, trickle bool) (*Session, string, *pion.PeerConnection) {
	t.Helper()
	mediaEngine := &pion.MediaEngine{}
	if err := mediaEngine.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{
			MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1,
		},
		PayloadType: 0,
	}, pion.RTPCodecTypeAudio); err != nil {
		t.Fatalf("codec: %v", err)
	}
	api := pion.NewAPI(pion.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("pc: %v", err)
	}
	track, err := pion.NewTrackLocalStaticRTP(
		pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		"audio", "trickle")
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if _, err := pc.AddTransceiverFromTrack(track, pion.RTPTransceiverInit{
		Direction: pion.RTPTransceiverDirectionSendrecv,
	}); err != nil {
		t.Fatalf("transceiver: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("local: %v", err)
	}
	<-pion.GatheringCompletePromise(pc)
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	session := NewSession(engine, "trickle-test", "call")
	attach, err := session.AttachClientTrickle(context.Background(), pc.LocalDescription().SDP, nil, trickle)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	token := attach.AttachToken
	return session, token, pc
}

const testCandidate = "candidate:1 1 udp 2113937151 192.0.2.10 51000 typ host generation 0"

// A full-SDP (legacy) attach never accepts candidate batches, even with a
// guessed token, and a trickle attach rejects a wrong token: stale callbacks
// are ignored rather than applied to the wrong peer connection.
func TestAddClientCandidatesRejectsStaleAttach(t *testing.T) {
	session, _, offerPC := newTrickleSession(t, false)
	t.Cleanup(func() { session.Close(); _ = offerPC.Close() })
	if _, err := session.AddClientCandidates("anything", []string{testCandidate}); err != ErrAttachStale {
		t.Fatalf("legacy attach accepted candidates: %v", err)
	}

	trickleSession, token, offerPC2 := newTrickleSession(t, true)
	t.Cleanup(func() { trickleSession.Close(); _ = offerPC2.Close() })
	if _, err := trickleSession.AddClientCandidates("wrong-token", []string{testCandidate}); err != ErrAttachStale {
		t.Fatalf("wrong token accepted: %v", err)
	}
	if _, err := trickleSession.AddClientCandidates("", []string{testCandidate}); err != ErrAttachStale {
		t.Fatalf("empty token accepted: %v", err)
	}
	added, err := trickleSession.AddClientCandidates(token, []string{testCandidate})
	if err != nil || added != 1 {
		t.Fatalf("valid batch rejected: added=%d err=%v", added, err)
	}
	if trickleSession.AttachToken() != token {
		t.Fatalf("attach token changed")
	}
}

// The per-batch and per-attach caps bound the work one authenticated client
// can create; oversized/malformed lines are skipped, never fatal.
func TestAddClientCandidatesBoundsBatches(t *testing.T) {
	session, token, offerPC := newTrickleSession(t, true)
	t.Cleanup(func() { session.Close(); _ = offerPC.Close() })

	batch := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		batch = append(batch, testCandidate)
	}
	added, err := session.AddClientCandidates(token, batch)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if added > trickleMaxBatch {
		t.Fatalf("batch cap not enforced: %d", added)
	}
	total := added
	for i := 0; i < 10; i++ {
		more, err := session.AddClientCandidates(token, batch)
		if err != nil {
			t.Fatalf("batch %d: %v", i, err)
		}
		total += more
	}
	if total > trickleMaxTotal {
		t.Fatalf("total cap not enforced: %d", total)
	}

	// Malformed lines are skipped silently and do not consume the budget.
	fresh, freshToken, freshPC := newTrickleSession(t, true)
	t.Cleanup(func() { fresh.Close(); _ = freshPC.Close() })
	added, err = fresh.AddClientCandidates(freshToken, []string{
		"not-a-candidate", "", "candidate:1 1 udp bogus", string(make([]byte, 600)),
	})
	if err != nil || added != 0 {
		t.Fatalf("malformed lines: added=%d err=%v", added, err)
	}
}

// engineOf reuses an existing session's engine for a replacement session.
func engineOf(t *testing.T, session *Session) *Engine {
	t.Helper()
	if session == nil || session.Engine == nil {
		t.Fatalf("session has no engine")
	}
	return session.Engine
}

// A replace attach invalidates the previous attach's token even before the
// new peer connection is stored.
func TestAddClientCandidatesInvalidatedByNewAttach(t *testing.T) {
	session, token, offerPC := newTrickleSession(t, true)
	t.Cleanup(func() { session.Close(); _ = offerPC.Close() })
	if _, err := session.AddClientCandidates(token, []string{testCandidate}); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	// Re-attach with the same (still valid) SDP: a new peer connection and a
	// new token replace the old attachment.
	replacement := NewSession(engineOf(t, session), "trickle-test-2", "call")
	t.Cleanup(func() { replacement.Close() })
	if _, err := session.AddClientCandidates(token, nil); err != nil {
		t.Fatalf("no-op batch: %v", err)
	}
	session.Close()
	if _, err := session.AddClientCandidates(token, []string{testCandidate}); err != ErrAttachStale {
		t.Fatalf("closed session accepted candidates: %v", err)
	}
}

// blackholeICE is a STUN URL that never answers: host candidates are gathered
// immediately while srflx gathering stays pending, which is exactly the state
// the early-answer path must handle.
func blackholeICE() []pion.ICEServer {
	return []pion.ICEServer{{URLs: []string{"stun:127.0.0.1:1"}}}
}

// TestTrickleAttachAnswersBeforeGatheringCompletes proves the build-68
// contract: a trickle attach returns the answer WITHOUT waiting for the
// gateway's ICE gathering, and the candidates gathered afterwards are
// delivered incrementally through the attach-fenced queue (nothing is lost,
// nothing is delivered twice).
func TestTrickleAttachAnswersBeforeGatheringCompletes(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	remote, offerSDP := clientOffer(t)
	defer remote.Close()
	session := NewSession(engine, "early-answer", "call")
	t.Cleanup(func() { session.Close() })

	started := time.Now()
	attach, err := session.AttachClientTrickle(context.Background(), offerSDP, blackholeICE(), true)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed > 2*time.Second {
		t.Fatalf("trickle attach waited for gathering: %s", elapsed)
	}
	if attach.GatheringComplete {
		t.Fatalf("gathering reported complete despite a black-hole STUN server")
	}
	if attach.AttachToken == "" {
		t.Fatalf("no attach token minted")
	}
	if attach.SDP == "" || !strings.Contains(attach.SDP, "v=0") {
		t.Fatalf("answer SDP missing: %q", attach.SDP)
	}

	// The poll hands out the candidates gathered so far (host candidates
	// arrive after the answer) and honestly reports that gathering is still
	// running.
	cursor := attach.CandidateCursor
	var seen []string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(gatherPollMinInterval)
		batch, err := session.LocalCandidates(attach.AttachToken, cursor)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if batch.Cursor < cursor {
			t.Fatalf("cursor went backwards: %d < %d", batch.Cursor, cursor)
		}
		cursor = batch.Cursor
		seen = append(seen, batch.Candidates...)
		if len(seen) > 0 && !batch.Complete {
			break
		}
	}
	if len(seen) == 0 {
		t.Fatalf("no incremental candidates were delivered")
	}
	for _, candidate := range seen {
		if !strings.HasPrefix(candidate, "candidate:") {
			t.Fatalf("delivered a malformed candidate: %q", candidate)
		}
	}
	// Every candidate already in the answer SDP is accounted for by the
	// cursor: polling after it must not repeat them.
	time.Sleep(gatherPollMinInterval)
	repeat, err := session.LocalCandidates(attach.AttachToken, 0)
	if err != nil {
		t.Fatalf("full poll: %v", err)
	}
	if len(repeat.Candidates) > len(seen)+attach.CandidateCursor {
		t.Fatalf("queue grew unexpectedly: %d", len(repeat.Candidates))
	}
	for i, candidate := range seen {
		var found bool
		for _, queued := range repeat.Candidates {
			if queued == candidate {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("candidate %d delivered then missing from the queue", i)
		}
	}

	// A wrong/absent token is stale; the queue never serves another attach.
	if _, err := session.LocalCandidates("wrong", 0); err != ErrAttachStale {
		t.Fatalf("wrong token accepted: %v", err)
	}
	// Polling faster than the bounded interval is refused with a retryable
	// error, not served.
	if _, err := session.LocalCandidates(attach.AttachToken, cursor); !errors.Is(err, ErrAttachPollThrottled) {
		t.Fatalf("immediate second poll not throttled: %v", err)
	}
}

// A full-SDP attach never serves the incremental queue.
func TestFullSDPAttachHasNoIncrementalQueue(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	remote, offerSDP := clientOffer(t)
	defer remote.Close()
	session := NewSession(engine, "full-sdp", "call")
	t.Cleanup(func() { session.Close() })
	attach, err := session.AttachClientTrickle(context.Background(), offerSDP, nil, false)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !attach.GatheringComplete {
		t.Fatalf("full-SDP attach reported incomplete gathering")
	}
	if attach.AttachToken != "" {
		t.Fatalf("full-SDP attach minted a token")
	}
	if _, err := session.LocalCandidates("anything", 0); err != ErrAttachStale {
		t.Fatalf("full-SDP attach served the incremental queue: %v", err)
	}
}

// clientOffer builds a real PCMU offer (with local candidates) and returns
// the offering peer.
func clientOffer(t *testing.T) (*pion.PeerConnection, string) {
	t.Helper()
	mediaEngine := &pion.MediaEngine{}
	if err := mediaEngine.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{
			MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1,
		},
		PayloadType: 0,
	}, pion.RTPCodecTypeAudio); err != nil {
		t.Fatalf("codec: %v", err)
	}
	api := pion.NewAPI(pion.WithMediaEngine(mediaEngine))
	pc, err := api.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatalf("pc: %v", err)
	}
	track, err := pion.NewTrackLocalStaticRTP(
		pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		"audio", "offer")
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if _, err := pc.AddTransceiverFromTrack(track, pion.RTPTransceiverInit{
		Direction: pion.RTPTransceiverDirectionSendrecv,
	}); err != nil {
		t.Fatalf("transceiver: %v", err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("local: %v", err)
	}
	<-pion.GatheringCompletePromise(pc)
	return pc, pc.LocalDescription().SDP
}

// TestGatherStreamIsAttachmentOwned pins the root-review fence: the candidate
// stream belongs to ONE attachment, so a stale callback (or a stale poll)
// from a replaced attachment can never touch the new one.
func TestGatherStreamIsAttachmentOwned(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	remote, offerSDP := clientOffer(t)
	defer remote.Close()
	session := NewSession(engine, "attach-owned", "call")
	t.Cleanup(func() { session.Close() })

	first, err := session.AttachClientTrickle(context.Background(), offerSDP, blackholeICE(), true)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	firstQueue := session.gatherQueue
	if firstQueue == nil {
		t.Fatalf("no stream created")
	}
	time.Sleep(gatherPollMinInterval)
	if _, err := session.LocalCandidates(first.AttachToken, 0); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	// A second attach replaces the attachment (and its stream).
	remote2, offerSDP2 := clientOffer(t)
	defer remote2.Close()
	second, err := session.AttachClientTrickle(context.Background(), offerSDP2, blackholeICE(), true)
	if err != nil {
		t.Fatalf("second attach: %v", err)
	}
	if session.gatherQueue == firstQueue {
		t.Fatalf("reattach reused the previous attachment's queue")
	}
	// The OLD stream keeps only its own candidates: writing through the old
	// handle cannot reach the new stream (this is exactly what a stale ICE
	// callback does).
	firstQueue.append("candidate:1 1 udp 1 192.0.2.99 9 typ host")
	var secondItems int
	session.gatherQueue.mu.Lock()
	secondItems = len(session.gatherQueue.items)
	session.gatherQueue.mu.Unlock()
	if secondItems != 0 {
		t.Fatalf("stale callback leaked into the new attachment: %d items", secondItems)
	}
	// The old token is stale; the new token drains the new (empty) stream.
	if _, err := session.LocalCandidates(first.AttachToken, 0); err != ErrAttachStale {
		t.Fatalf("old token still served: %v", err)
	}
	time.Sleep(gatherPollMinInterval)
	batch, err := session.LocalCandidates(second.AttachToken, 0)
	if err != nil {
		t.Fatalf("new token poll: %v", err)
	}
	for _, candidate := range batch.Candidates {
		if strings.Contains(candidate, "192.0.2.99") {
			t.Fatalf("stale candidate delivered on the new attachment")
		}
	}
	// The old stream keeps its own completion state; marking it complete must
	// not mark the new attachment complete.
	firstQueue.markComplete()
	session.gatherQueue.mu.Lock()
	newComplete := session.gatherQueue.complete
	session.gatherQueue.mu.Unlock()
	if newComplete {
		t.Fatalf("stale completion leaked into the new attachment")
	}
}

// TestLocalCandidatesRaceWithReattach drives a poll concurrently with a
// reattach. Run under -race: the token check and the queue read are validated
// together, so every answer is either the old attachment's (before the swap)
// or an honest stale error — never a mix.
func TestLocalCandidatesRaceWithReattach(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	remote, offerSDP := clientOffer(t)
	defer remote.Close()
	session := NewSession(engine, "attach-race", "call")
	t.Cleanup(func() { session.Close() })

	first, err := session.AttachClientTrickle(context.Background(), offerSDP, blackholeICE(), true)
	if err != nil {
		t.Fatalf("first attach: %v", err)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			batch, err := session.LocalCandidates(first.AttachToken, 0)
			if err != nil {
				continue
			}
			for _, candidate := range batch.Candidates {
				if !strings.HasPrefix(candidate, "candidate:") {
					t.Errorf("malformed candidate from a racing poll: %q", candidate)
					return
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for i := 0; i < 4; i++ {
		remoteN, offerN := clientOffer(t)
		if _, err := session.AttachClientTrickle(context.Background(), offerN, blackholeICE(), true); err != nil {
			t.Fatalf("reattach %d: %v", i, err)
		}
		remoteN.Close()
	}
	close(done)
	wg.Wait()
}

// TestAddClientCandidatesConcurrentBurstStaysBounded drives many batches at
// once (run under -race): the per-attach budget is RESERVED under the session
// lock before any pion call, so concurrent batches can never overshoot the
// promised total even though the ICE calls happen outside the lock.
func TestAddClientCandidatesConcurrentBurstStaysBounded(t *testing.T) {
	session, token, offerPC := newTrickleSession(t, true)
	t.Cleanup(func() { session.Close(); _ = offerPC.Close() })

	const workers = 8
	const perBatch = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := make([]string, perBatch)
			for j := range batch {
				batch[j] = testCandidate
			}
			added, err := session.AddClientCandidates(token, batch)
			if err != nil {
				t.Errorf("burst batch failed: %v", err)
				return
			}
			mu.Lock()
			total += added
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total > trickleMaxTotal {
		t.Fatalf("concurrent batches exceeded the bound: %d > %d", total, trickleMaxTotal)
	}
	session.mu.Lock()
	reserved := session.attachCandidates
	session.mu.Unlock()
	if reserved > trickleMaxTotal {
		t.Fatalf("reserved budget exceeded the bound: %d", reserved)
	}
}
