package media

import (
	"sync"
	"testing"
	"time"

	"callrelay/gateway/internal/voice"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
)

// fakeEnc records CTLs so tests can assert exactly what the controller
// applied. Implements the subset of *opus.Encoder the controller uses.
type fakeEnc struct {
	mu       sync.Mutex
	bitrate  int
	lossPerc int
	fec      bool
}

func (f *fakeEnc) SetBitrate(b int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bitrate = b
	return nil
}

func (f *fakeEnc) SetPacketLossPerc(p int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lossPerc = p
	return nil
}

func (f *fakeEnc) SetInbandFEC(on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fec = on
	return nil
}

func (f *fakeEnc) get() (int, int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bitrate, f.lossPerc, f.fec
}

func newTestController(t *testing.T) (*bitrateController, *fakeEnc) {
	t.Helper()
	enc := &fakeEnc{}
	c, err := newBitrateController(enc, &sync.Mutex{}, "test")
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	t.Cleanup(c.Close)
	return c, enc
}

func TestBitrateClampedToOpusRange(t *testing.T) {
	c, enc := newTestController(t)
	c.lastBitrateChange = time.Time{}
	c.applyEstimate(1000) // far below min
	if b, _, _ := enc.get(); b != opusBitrateMin {
		t.Fatalf("bitrate = %d, want min %d", b, opusBitrateMin)
	}
	c.lastBitrateChange = time.Time{}
	c.applyEstimate(10_000_000) // far above max
	if b, _, _ := enc.get(); b != opusBitrateMax {
		t.Fatalf("bitrate = %d, want max %d", b, opusBitrateMax)
	}
}

func TestBitrateHysteresisRequiresBothThresholds(t *testing.T) {
	c, enc := newTestController(t)
	// First meaningful apply sets the baseline (32k -> 40k passes both).
	c.lastBitrateChange = time.Time{}
	c.applyEstimate(40000)
	if b, _, _ := enc.get(); b != 40000 {
		t.Fatalf("baseline = %d", b)
	}
	// +1 kbps: fails BOTH the 4 kbps absolute and 15% relative thresholds.
	c.lastBitrateChange = time.Time{}
	c.applyEstimate(41000)
	if b, _, _ := enc.get(); b != 40000 {
		t.Fatalf("sub-threshold jitter applied: %d", b)
	}
	// +3 kbps: passes absolute? No (3k < 4k). Ignored.
	c.lastBitrateChange = time.Time{}
	c.applyEstimate(43000)
	if b, _, _ := enc.get(); b != 40000 {
		t.Fatalf("single-threshold change applied: %d", b)
	}
	// +5 kbps: absolute yes, relative (>= 6k) no — both required. Ignored.
	c.lastBitrateChange = time.Time{}
	c.applyEstimate(45000)
	if b, _, _ := enc.get(); b != 40000 {
		t.Fatalf("partial-threshold change applied: %d", b)
	}
	// +7 kbps: passes both. Applied.
	c.lastBitrateChange = time.Time{}
	c.applyEstimate(47000)
	if b, _, _ := enc.get(); b != 47000 {
		t.Fatalf("legitimate change not applied: %d", b)
	}
}

func TestBitrateCooldownGatesRapidChanges(t *testing.T) {
	c, enc := newTestController(t)
	c.lastBitrateChange = time.Time{}
	c.applyEstimate(48000)
	if b, _, _ := enc.get(); b != 48000 {
		t.Fatalf("baseline = %d", b)
	}
	// Immediately after a real apply, a legitimate-size change is gated.
	c.applyEstimate(32000)
	if b, _, _ := enc.get(); b != 48000 {
		t.Fatalf("cooldown-violating apply went through: %d", b)
	}
}

func TestLossPercHysteresisAndFECToggle(t *testing.T) {
	c, enc := newTestController(t)
	c.onLossWindow(0.10)
	if _, p, fec := enc.get(); p != 10 || !fec {
		t.Fatalf("loss window not applied: perc=%d fec=%v", p, fec)
	}
	c.onLossWindow(0.11) // within ±2 hysteresis
	if _, p, _ := enc.get(); p != 10 {
		t.Fatalf("loss perc jittered: %d", p)
	}
	c.onLossWindow(0.01) // below fecDisableAt but inside the 5 s cooldown
	if _, _, fec := enc.get(); !fec {
		t.Fatalf("fec flipped inside cooldown")
	}
}

func TestFECHysteresisDoesNotFlap(t *testing.T) {
	c, enc := newTestController(t)
	c.lastFecChange = time.Time{}
	c.onLossWindow(0.05)
	if _, _, fec := enc.get(); !fec {
		t.Fatalf("fec not enabled at 5%% loss")
	}
	c.onLossWindow(0.02) // deadband: hold, never flap
	if _, _, fec := enc.get(); !fec {
		t.Fatalf("fec flapped in the deadband")
	}
}

func TestStaleFeedbackHoldsState(t *testing.T) {
	c, enc := newTestController(t)
	c.lastBitrateChange = time.Time{}
	c.applyEstimate(20000) // congested
	if b, _, _ := enc.get(); b != 20000 {
		t.Fatalf("baseline = %d", b)
	}
	now := time.Now()
	c.mu.Lock()
	c.lastTWCC = now.Add(-2 * feedbackExpiry)
	c.lastRR = now.Add(-2 * feedbackExpiry)
	c.reportStaleLocked(now)
	twccLogged := c.twccStaleLogged
	rrLogged := c.rrStaleLogged
	held := c.bitrate
	c.mu.Unlock()
	if !twccLogged || !rrLogged {
		t.Fatalf("stale episodes not logged: twcc=%v rr=%v", twccLogged, rrLogged)
	}
	if b, _, _ := enc.get(); b != held || b != 20000 {
		t.Fatalf("stale report changed encoder state: %d", b)
	}
	// TWCC freshness is tracked separately from RR: refreshing only the
	// RR must not clear the TWCC stale episode.
	c.mu.Lock()
	c.lastRR = now
	c.reportStaleLocked(now)
	twccStillLogged := c.twccStaleLogged
	c.mu.Unlock()
	if !twccStillLogged {
		t.Fatalf("rr freshness cleared the twcc stale flag")
	}
}

func TestNoStaleApplyAfterClose(t *testing.T) {
	enc := &fakeEnc{}
	c, err := newBitrateController(enc, &sync.Mutex{}, "test")
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	c.Close()
	c.lastBitrateChange = time.Time{}
	c.applyEstimate(60000)
	c.onLossWindow(0.5)
	if b, p, _ := enc.get(); b != 0 || p != 0 {
		t.Fatalf("stale apply after close: bitrate=%d loss=%d", b, p)
	}
}

func TestNackBudgetCapsBurst(t *testing.T) {
	budget := newNackBudget()
	allowed := 0
	for i := 0; i < 40; i++ {
		if budget.take() {
			allowed++
		}
	}
	if allowed != int(nackBudgetBurst) {
		t.Fatalf("allowed %d nacks in a burst, want %d", allowed, int(nackBudgetBurst))
	}
	if budget.dropped() == 0 {
		t.Fatalf("no drops recorded")
	}
	budget.mu.Lock()
	budget.last = budget.last.Add(-2 * time.Second)
	budget.mu.Unlock()
	if !budget.take() {
		t.Fatalf("budget did not refill after 2s")
	}
}

func TestNackBudgetWrapFiltersOnlyNacks(t *testing.T) {
	budget := newNackBudget()
	for budget.take() {
	}
	wrapped := &nackBudgetWrap{Interceptor: &interceptor.NoOp{}, budget: budget}
	var wrote []rtcp.Packet
	w := wrapped.BindRTCPWriter(interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, _ interceptor.Attributes) (int, error) {
		wrote = append(wrote, pkts...)
		return len(pkts), nil
	}))
	nack := &rtcp.TransportLayerNack{}
	rr := &rtcp.ReceiverReport{}
	if _, err := w.Write([]rtcp.Packet{nack, rr, nack}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(wrote) != 1 || wrote[0] != rr {
		t.Fatalf("budget-filtered batch = %v, want only the RR", wrote)
	}
}

func TestSendBWEInterceptorPassesRTCPThrough(t *testing.T) {
	estimator, err := gcc.NewSendSideBWE()
	if err != nil {
		t.Fatalf("estimator: %v", err)
	}
	defer estimator.Close()
	i := &sendBWEInterceptor{estimator: estimator}
	reader := i.BindRTCPReader(interceptor.RTCPReaderFunc(func(buf []byte, _ interceptor.Attributes) (int, interceptor.Attributes, error) {
		pkt := &rtcp.TransportLayerCC{MediaSSRC: 1, SenderSSRC: 2}
		raw, err := pkt.Marshal()
		if err != nil {
			return 0, nil, err
		}
		copy(buf, raw)
		return len(raw), nil, nil
	}))
	buf := make([]byte, 1500)
	n, _, err := reader.Read(buf, nil)
	if err != nil || n == 0 {
		t.Fatalf("chain read = %d, %v", n, err)
	}
}

func TestSetupClientCodecPCMUHasNoMachinery(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	s := NewSession(engine, "t", "call")
	defer s.Close()
	if err := s.setupClientCodec(codecPCMU); err != nil {
		t.Fatalf("setup pcmu: %v", err)
	}
	s.codecMu.Lock()
	defer s.codecMu.Unlock()
	if s.opusEnc != nil || s.opusDec != nil || s.controller != nil {
		t.Fatalf("PCMU session carries codec machinery")
	}
	if s.payloadType != 0 {
		t.Fatalf("pcmu payload type = %d", s.payloadType)
	}
}

func TestSetupClientCodecOpusCreatesMachinery(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	s := NewSession(engine, "t", "call")
	defer s.Close()
	if err := s.setupClientCodec(codecOpus); err != nil {
		t.Fatalf("setup opus: %v", err)
	}
	s.codecMu.Lock()
	defer s.codecMu.Unlock()
	if s.opusEnc == nil || s.opusDec == nil || s.controller == nil {
		t.Fatalf("opus session missing machinery")
	}
	if s.payloadType != defaultOpusPT {
		t.Fatalf("opus payload type = %d", s.payloadType)
	}
}

func TestEncodeClientFrameMatchesCodec(t *testing.T) {
	engine, err := NewEngine()
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	s := NewSession(engine, "t", "call")
	defer s.Close()
	if err := s.setupClientCodec(codecOpus); err != nil {
		t.Fatalf("setup: %v", err)
	}
	pcm := make([]int16, voice.FrameSamples)
	for i := range pcm {
		pcm[i] = 500
	}
	opusFrame, err := s.encodeClientFrame(pcm)
	if err != nil {
		t.Fatalf("encode opus: %v", err)
	}
	// A voiced 20 ms Opus frame at 32 kbps is tens of bytes — never the
	// 160-byte PCMU expansion.
	if len(opusFrame) >= 160 {
		t.Fatalf("opus frame = %d bytes, want < 160", len(opusFrame))
	}
	if _, err := s.encodeClientFrame(nil); err == nil {
		t.Fatalf("nil frame silently encoded")
	}
}
