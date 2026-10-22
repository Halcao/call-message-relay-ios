package media

import (
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/gcc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

// Closed-loop congestion control for the Opus network hop (2026-10-05,
// build 26). The pion pkg/gcc SendSideBWE estimator consumes transport-cc
// feedback the phone emits for the gateway's downlink and produces a target
// bitrate; the Receiver Reports the rrObserver collects report downlink loss
// as measured by the phone. Both feed bounded libopus encoder CTLs
// (bitrate, inband FEC, packet-loss-perc) with clamping, hysteresis and
// feedback expiry. This is control, not telemetry: every knob is applied to
// the live encoder of this session.
//
// Direction honesty: TWCC and RR describe the DOWNLINK (gateway→phone). The
// NACK-request budget covers the UPLINK (phone→gateway) direction; it is a
// fixed token bucket and is NOT modulated from downlink estimates — the two
// directions are not causally linked and an asymmetric uplink must not be
// asserted from downlink bandwidth.
const (
	// Opus bitrate bounds for the narrowband network hop. PCMU is 64 kbps
	// fixed; the adapted Opus stream stays inside 16-64 kbps per the plan.
	opusBitrateMin     = 16000
	opusBitrateMax     = 64000
	opusBitrateInitial = 32000

	// Bitrate hysteresis: a new estimate is applied only when it differs
	// from the current value by at least bitrateMinDelta bps AND
	// current*bitrateMinRatio (both thresholds). Small estimator jitter
	// never reaches the encoder; the same gate bounds post-expiry snaps.
	bitrateMinDelta       = 4000
	bitrateMinRatio       = 0.15
	bitrateChangeCooldown = 800 * time.Millisecond

	// Packet-loss-perc bounds and hysteresis.
	lossPercMax        = 30
	lossPercHysteresis = 2

	// FEC hysteresis: enable at/above fecEnableAt fraction lost, disable at
	// or below fecDisableAt, at most once per fecCooldown. FEC costs bitrate
	// (~the loss percentage again in redundancy), so it must not flap.
	fecEnableAt  = 0.03
	fecDisableAt = 0.01
	fecCooldown  = 5 * time.Second

	// Feedback expiry, tracked separately per feedback type so one healthy
	// stream cannot keep the other's state fresh:
	//   - TWCC (transport-cc from the phone, consumed by the GCC estimator)
	//     drives the bitrate. When it goes silent the congestion state is
	//     UNKNOWN — the correct response is to HOLD the current bitrate,
	//     never to raise it (silence can mean severe congestion where the
	//     feedback itself is lost) and not to lower it either (no evidence).
	//   - RR (Receiver Reports) drives packet-loss-perc/FEC. Silence holds
	//     the last values; FEC stays as configured rather than flapping.
	feedbackExpiry = 10 * time.Second

	// Uplink NACK-request budget (token bucket). Requests are tiny RTCP
	// packets; the bucket caps their burst independently of any downlink
	// signal (see package note above).
	nackBudgetPerSec = 12.0
	nackBudgetBurst  = 12.0
)

// opusEncoder is the subset of *opus.Encoder the controller drives; tests
// substitute a recording fake.
type opusEncoder interface {
	SetBitrate(int) error
	SetPacketLossPerc(int) error
	SetInbandFEC(bool) error
}

// bitrateController adapts one session's Opus encoder from live congestion
// feedback. Nil for PCMU sessions (the fixed-rate codec has nothing to
// adapt — no bitrate controller is claimed there).
//
// Lock order: encMu (the session codecMu) before mu. Session teardown takes
// codecMu then calls Close (which takes mu); controller callbacks never
// take mu before encMu, so the two paths cannot deadlock.
type bitrateController struct {
	estimator *gcc.SendSideBWE
	enc       opusEncoder
	encMu     *sync.Mutex // session codecMu; every libopus call is serialized by this
	label     string

	mu                sync.Mutex
	bitrate           int
	lossPerc          int
	fecEnabled        bool
	lastTWCC          time.Time
	lastRR            time.Time
	lastBitrateChange time.Time
	lastFecChange     time.Time
	twccStaleLogged   bool
	rrStaleLogged     bool
	closed            bool

	stop     chan struct{}
	stopOnce sync.Once
}

func newBitrateController(enc opusEncoder, encMu *sync.Mutex, label string) (*bitrateController, error) {
	// The pacer is a deliberate NoOp: gcc's leaky-bucket pacer budgets
	// per-tick bytes at the target bitrate, which for 16-64 kbps audio
	// releases one ~120-byte packet only every ~30 ms (budget/tick < packet
	// size) — the queue would grow without bound and add seconds of latency.
	// Audio is self-paced at the 20 ms mixer tick; retransmission requests
	// are bounded by the uplink NACK token bucket instead.
	estimator, err := gcc.NewSendSideBWE(
		gcc.SendSideBWEInitialBitrate(opusBitrateInitial),
		gcc.SendSideBWEMinBitrate(opusBitrateMin),
		gcc.SendSideBWEMaxBitrate(opusBitrateMax),
		gcc.SendSideBWEPacer(gcc.NewNoOpPacer()),
	)
	if err != nil {
		return nil, err
	}
	c := &bitrateController{
		estimator: estimator,
		enc:       enc,
		encMu:     encMu,
		label:     label,
		bitrate:   opusBitrateInitial,
		stop:      make(chan struct{}),
	}
	estimator.OnTargetBitrateChange(c.applyEstimate)
	go c.expiryLoop()
	return c, nil
}

// touchTWCC records actual transport-cc arrival. Called from the RTCP read
// path (the estimator callback alone is not an arrival timestamp: the
// callback fires when the estimator recomputes, which is downstream of the
// write). Estimator-driven applies still carry their own hysteresis gate.
func (c *bitrateController) touchTWCC() {
	c.mu.Lock()
	c.lastTWCC = time.Now()
	c.twccStaleLogged = false
	c.mu.Unlock()
}

// applyEstimate is the GCC target-bitrate callback. Clamped to the Opus
// range, gated by the both-threshold hysteresis and the cooldown, then
// applied to the live encoder atomically with the state update. Holding
// encMu+mu through the CTL call keeps "encoder value == reported value":
// a failed CTL never becomes reported state, and no other callback can
// interleave a competing CTL.
func (c *bitrateController) applyEstimate(bitrate int) {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	clamped := clampInt(bitrate, opusBitrateMin, opusBitrateMax)
	delta := math.Abs(float64(clamped - c.bitrate))
	if delta < bitrateMinDelta || delta < float64(c.bitrate)*bitrateMinRatio {
		return
	}
	if time.Since(c.lastBitrateChange) < bitrateChangeCooldown {
		return
	}
	if err := c.enc.SetBitrate(clamped); err != nil {
		slog.Warn("opus bitrate apply failed", "session", c.label, "bitrate", clamped, "error", err)
		return
	}
	c.bitrate = clamped
	c.lastBitrateChange = time.Now()
	slog.Info("opus bitrate adapted", "session", c.label, "bitrate", clamped)
}

// onLossWindow consumes one bounded RR window from the rrObserver:
// downlink loss as measured by the phone drives packet-loss-perc and the
// FEC hysteresis. Same atomicity contract as applyEstimate.
func (c *bitrateController) onLossWindow(fractionLost float64) {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.lastRR = time.Now()
	c.rrStaleLogged = false
	lossPerc := clampInt(int(math.Round(fractionLost*100)), 0, lossPercMax)
	if absInt(lossPerc-c.lossPerc) < lossPercHysteresis {
		lossPerc = c.lossPerc
	}
	lossChanged := lossPerc != c.lossPerc

	wantFEC := c.fecEnabled
	if fractionLost >= fecEnableAt {
		wantFEC = true
	} else if fractionLost <= fecDisableAt {
		wantFEC = false
	}
	fecChanged := wantFEC != c.fecEnabled
	if fecChanged && time.Since(c.lastFecChange) < fecCooldown {
		fecChanged = false
		wantFEC = c.fecEnabled
	}
	if lossChanged {
		if err := c.enc.SetPacketLossPerc(lossPerc); err != nil {
			slog.Warn("opus loss perc apply failed", "session", c.label, "perc", lossPerc, "error", err)
			lossChanged = false
			lossPerc = c.lossPerc
		}
	}
	if fecChanged {
		if err := c.enc.SetInbandFEC(wantFEC); err != nil {
			slog.Warn("opus fec apply failed", "session", c.label, "enabled", wantFEC, "error", err)
			fecChanged = false
			wantFEC = c.fecEnabled
		}
	}
	c.lossPerc = lossPerc
	c.fecEnabled = wantFEC
	if fecChanged {
		c.lastFecChange = time.Now()
		slog.Info("opus fec adapted", "session", c.label, "enabled", wantFEC, "fractionLost", fractionLost)
	}
}

// expiryLoop detects silent feedback streams. It NEVER changes encoder
// state: silence means unknown, and the conservative hold (neither raise
// nor lower) is the safe response. Each stale episode logs once.
func (c *bitrateController) expiryLoop() {
	ticker := time.NewTicker(feedbackExpiry / 2)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			continue
		}
		c.reportStaleLocked(time.Now())
		c.mu.Unlock()
	}
}

// reportStaleLocked logs one line per stale episode. It NEVER changes
// encoder state: silent feedback means unknown congestion, and the
// conservative hold (neither raise nor lower) is the safe response.
// Caller must hold mu.
func (c *bitrateController) reportStaleLocked(now time.Time) {
	twccStale := now.Sub(c.lastTWCC) >= feedbackExpiry
	rrStale := now.Sub(c.lastRR) >= feedbackExpiry
	if twccStale && !c.twccStaleLogged {
		c.twccStaleLogged = true
		slog.Info("opus twcc feedback stale; holding bitrate", "session", c.label, "bitrate", c.bitrate)
	}
	if rrStale && !c.rrStaleLogged {
		c.rrStaleLogged = true
		slog.Info("opus rr feedback stale; holding loss state", "session", c.label)
	}
}

// Close shuts the controller and its estimator down (stops the estimator
// goroutines). Called with the session codecMu already held by
// releaseCodecLocked, so it must NOT take encMu. After Close returns no
// callback can apply a CTL: every applier holds mu and checks closed.
func (c *bitrateController) Close() {
	c.stopOnce.Do(func() { close(c.stop) })
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	_ = c.estimator.Close()
}

// singletonFactory adapts a single interceptor instance to the
// interceptor.Factory shape; the per-session registry builds exactly one
// chain, so the instance is used once.
type singletonFactory struct{ i interceptor.Interceptor }

func (f singletonFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) { return f.i, nil }

// sendBWEInterceptor plugs one peer connection's streams into the session's
// GCC estimator: outgoing RTP is recorded for TWCC accounting (the pacer is
// a NoOp — see newBitrateController), and inbound RTCP (TransportLayerCC
// from the phone, RFC 8888 when offered) feeds the estimator. The first
// sight of an actual TransportLayerCC packet also timestamps feedback
// arrival on the controller.
type sendBWEInterceptor struct {
	interceptor.NoOp
	estimator  *gcc.SendSideBWE
	controller *bitrateController
}

func (i *sendBWEInterceptor) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return i.estimator.AddStream(info, writer)
}

func (i *sendBWEInterceptor) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(buf []byte, attributes interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, attrs, err := reader.Read(buf, attributes)
		if err == nil && n > 0 {
			if pkts, uerr := rtcp.Unmarshal(buf[:n]); uerr == nil {
				for _, pkt := range pkts {
					if _, isTWCC := pkt.(*rtcp.TransportLayerCC); isTWCC && i.controller != nil {
						i.controller.touchTWCC()
					}
				}
				// The estimator consumes TransportLayerCC / CCFeedback and
				// ignores other packets; its closed-error is expected
				// during teardown.
				_ = i.estimator.WriteRTCP(pkts, attrs)
			}
		}
		return n, attrs, err
	})
}

// twccExtWriter stamps the negotiated transport-wide sequence number into
// every outgoing RTP packet. The GCC feedback adapter attributes departures
// by reading this extension (its OnSent FAILS the packet write when the
// extension is absent), and the peer's SDK acknowledges the sequence numbers
// back in TransportLayerCC reports — this writer is what makes that loop
// possible. It binds only when the TWCC extension actually negotiated.
//
// Concurrency (build 48 race fix): pion may invoke BindLocalStream while a
// previously bound stream's writer is already emitting — concretely during
// warm adoption, where the adopted probe's interceptor chain keeps serving
// the live session's writeTrack while adoption re-binds streams, so the
// hdrExtID assignment raced the per-packet seq increment (the pre-build-48
// baseline failed -race here). One fresh writer is created per peer
// connection in Engine.newAPI, and both fields are guarded by mu so a bind
// can never race a write; the sequence space itself stays monotonic for the
// life of the peer connection.
type twccExtWriter struct {
	interceptor.NoOp
	mu       sync.Mutex
	hdrExtID uint8
	seq      uint16
}

func (w *twccExtWriter) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	var hdrExtID uint8
	for _, e := range info.RTPHeaderExtensions {
		if e.URI == transportCCURI {
			hdrExtID = uint8(e.ID) //nolint:gosec // G115
			break
		}
	}
	w.mu.Lock()
	w.hdrExtID = hdrExtID
	w.mu.Unlock()
	if hdrExtID == 0 {
		return writer
	}
	return interceptor.RTPWriterFunc(func(header *rtp.Header, payload []byte, attributes interceptor.Attributes) (int, error) {
		w.mu.Lock()
		w.seq++
		seq := w.seq
		w.mu.Unlock()
		var ext rtp.TransportCCExtension
		ext.TransportSequence = seq
		raw, err := ext.Marshal()
		if err != nil {
			return 0, err
		}
		if err := header.SetExtension(hdrExtID, raw); err != nil {
			return 0, err
		}
		return writer.Write(header, payload, attributes)
	})
}

// nackBudget caps the RTCP packets (NACK retransmission requests) an inner
// interceptor may emit, with a refillable token bucket. When the bucket is
// empty, TransportLayerNack packets are dropped; every other RTCP packet
// passes untouched. Dropped requests are bounded-loss recovery deferrals,
// not correctness issues: the playout deadline at the gateway drops late
// retransmissions anyway. The bucket is direction-local (uplink requests)
// and constant-rate: no downlink estimate asserts anything about it.
type nackBudget struct {
	interceptor.NoOp
	mu     sync.Mutex
	tokens float64
	rate   float64
	burst  float64
	last   time.Time
	now    func() time.Time // test seam
	drops  uint64
}

func newNackBudget() *nackBudget {
	return &nackBudget{
		tokens: nackBudgetBurst,
		rate:   nackBudgetPerSec,
		burst:  nackBudgetBurst,
		last:   time.Now(),
		now:    time.Now,
	}
}

func (b *nackBudget) setRate(rate float64) {
	b.mu.Lock()
	b.rate = rate
	b.mu.Unlock()
}

// take consumes one token when available; reports whether the packet may be
// sent.
func (b *nackBudget) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.tokens += elapsed * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	if b.tokens < 1 {
		b.drops++
		return false
	}
	b.tokens--
	return true
}

// BindRTCPWriter wraps the inner interceptor's emission path so every
// TransportLayerNack it produces spends a budget token.
type nackBudgetWrap struct {
	interceptor.Interceptor
	budget *nackBudget
}

func (w *nackBudgetWrap) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	inner := w.Interceptor.BindRTCPWriter(writer)
	return interceptor.RTCPWriterFunc(func(pkts []rtcp.Packet, attributes interceptor.Attributes) (int, error) {
		kept := make([]rtcp.Packet, 0, len(pkts))
		for _, pkt := range pkts {
			if _, isNack := pkt.(*rtcp.TransportLayerNack); isNack && !w.budget.take() {
				continue
			}
			kept = append(kept, pkt)
		}
		if len(kept) == 0 {
			return 0, nil
		}
		return inner.Write(kept, attributes)
	})
}

// dropped reports the cumulative number of budget-dropped NACKs (evidence
// for the periodic controller log).
func (b *nackBudget) dropped() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.drops
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
