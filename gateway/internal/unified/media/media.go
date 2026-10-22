// Package media terminates the client-facing WebRTC leg for the unified
// service and mixes line audio. One Session represents either a single
// 1:1 call or a conference; line legs are fed by the worker's internal PCMU
// socket. The mixer is mix-minus per leg with 20 ms pacing, isolation for
// held legs and host-disconnect gating so held/disconnected audio can never
// leak between external legs.
package media

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"callrelay/gateway/internal/opus"
	"callrelay/gateway/internal/speexpp"
	"callrelay/gateway/internal/voice"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/interceptor/pkg/report"
	"github.com/pion/interceptor/pkg/twcc"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"
)

// Codec identifiers for the network hop. Opus (RFC 7587, reference libopus
// at the gateway) is preferred; PCMU stays registered so old clients and the
// WSS-era fallback keep negotiating unchanged. The modem leg remains
// fixed-rate G.711 regardless — transcode happens at the gateway.
const (
	codecOpus = "opus"
	codecPCMU = "pcmu"

	opusMime      = "audio/opus"
	opusClockRate = 48000 // RFC 7587 fixed RTP clock; 960 ticks per 20 ms frame
	opusFrameTick = 960
	// defaultOpusPT is the payload type the answer uses unless the client's
	// offer remaps it; captured from the negotiated SDP either way.
	defaultOpusPT = 111
	// draft-holmer-rmcat-transport-wide-cc-extensions: transport-wide
	// sequence numbers for the GCC feedback loop.
	transportCCURI = "http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01"
)

// Exported codec identifiers for packages wiring sessions (runtime).
const (
	CodecOpus = codecOpus
	CodecPCMU = codecPCMU
)

// countLocalCandidates tallies the gathered local candidates in a local SDP
// by transport (UDP/TCP) and type (host/srflx/relay). It is bounded, one-pass
// diagnostic evidence for direct-path failures: the counts show which
// candidates actually existed (host can work on a shared subnet; srflx can
// hole-punch across NAT) and correlate with the peer-state transition log on
// the next field run. They do not alone prove the direct path impossible.
func countLocalCandidates(sdpText string) (udp, tcp, host, srflx, relay int) {
	for _, line := range strings.Split(sdpText, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 8 || !strings.HasPrefix(fields[0], "a=candidate:") {
			continue
		}
		switch strings.ToUpper(fields[2]) {
		case "UDP":
			udp++
		case "TCP":
			tcp++
		}
		for index := 6; index+1 < len(fields); index++ {
			if fields[index] != "typ" {
				continue
			}
			switch fields[index+1] {
			case "host":
				host++
			case "srflx":
				srflx++
			case "relay":
				relay++
			}
			break
		}
	}
	return
}

const frameBytes = 160 // 20 ms PCMU @ 8 kHz

// ProbeEchoLabel is the data channel the client opens on its offer peer
// connection for app-level RTT measurement. The gateway answers it with a
// bounded exact echo (see installProbeEcho); the channel survives probe
// adoption so an active direct call keeps its measurement path.
const ProbeEchoLabel = "callrelay-probe"

const (
	probeEchoMaxBytes  = 1024
	probeEchoRatePerS  = 50
	probeEchoBurstSize = 50
)

// installProbeEcho answers every message on the channel with the exact same
// payload. It is intentionally transport-agnostic about content so the app
// can use {"type":"ping","tag":n} round trips; the bounds stop a runaway or
// hostile client from turning the echo into an amplifier.
func installProbeEcho(channel *pion.DataChannel) {
	var mu sync.Mutex
	tokens, last := probeEchoBurstSize, time.Now()
	channel.OnMessage(func(message pion.DataChannelMessage) {
		if len(message.Data) > probeEchoMaxBytes {
			return
		}
		mu.Lock()
		now := time.Now()
		tokens += int(now.Sub(last).Seconds() * probeEchoRatePerS)
		if tokens > probeEchoBurstSize {
			tokens = probeEchoBurstSize
		}
		last = now
		if tokens <= 0 {
			mu.Unlock()
			return
		}
		tokens--
		mu.Unlock()
		if message.IsString {
			_ = channel.SendText(string(message.Data))
			return
		}
		_ = channel.Send(message.Data)
	})
}

type Engine struct {
	mediaEngine *pion.MediaEngine
}

func NewEngine() (*Engine, error) {
	mediaEngine := &pion.MediaEngine{}
	// Preferred: Opus (RFC 7587) with inband FEC + DTX advertised. The
	// reference libopus (BSD-3-Clause, vendored under third_party/opus)
	// transcodes between the narrowband PCM domain and the network hop; the
	// closed-loop controller (controller.go) drives its bitrate/FEC CTLs
	// from GCC + Receiver Reports. NACK stays negotiated for both codecs.
	if err := mediaEngine.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{
			MimeType: opusMime, ClockRate: opusClockRate, Channels: 2,
			SDPFmtpLine: "minptime=10;useinbandfec=1;usedtx=1",
			RTCPFeedback: []pion.RTCPFeedback{
				{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "transport-cc"},
			},
		},
		PayloadType: defaultOpusPT,
	}, pion.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	// PCMU fallback: old clients (build ≤25 force PCMU-only offers) and any
	// future path that cannot take Opus still negotiate unchanged. NACK
	// retransmission remains the loss concealer here — PCMU is fixed-rate,
	// so no bitrate controller exists for this codec (nothing is claimed).
	if err := mediaEngine.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: pion.RTPCodecCapability{
			MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1,
			RTCPFeedback: []pion.RTCPFeedback{
				{Type: "nack"}, {Type: "nack", Parameter: "pli"}, {Type: "transport-cc"},
			},
		},
		PayloadType: 0,
	}, pion.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	// draft-holmer-rmcat-transport-wide-cc-extensions: the phone's SDK emits
	// TWCC for our downlink (consumed by the per-session GCC estimator) and
	// the twcc sender interceptor gives the phone feedback for its uplink.
	if err := mediaEngine.RegisterHeaderExtension(
		pion.RTPHeaderExtensionCapability{URI: transportCCURI}, pion.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	return &Engine{mediaEngine: mediaEngine}, nil
}

// newAPI builds a peer-connection API for ONE session attach. Every session
// owns its interceptor instances: the GCC estimator, the NACK-request
// budget and the RR observer are per-peer-connection state (windows keyed
// by SSRC would collide across concurrent calls on a shared observer).
//
// Registration order matters (interceptor.Chain composes last-registered as
// outermost): the send-BWE interceptor sits at index 0 so its pacer/feedback
// hook is closest to the wire for outgoing RTP; the NACK budget wraps the
// generator's RTCP emission path so requests spend tokens before the wire.
func (e *Engine) newAPI(controller *bitrateController, budget *nackBudget, observer *rrObserver) (*pion.API, error) {
	registry := &interceptor.Registry{}
	if controller != nil {
		registry.Add(singletonFactory{i: &sendBWEInterceptor{estimator: controller.estimator, controller: controller}})
		// Stamp TWCC sequence numbers into outbound packets BEFORE the
		// estimator records them (the adapter reads the extension from the
		// header; without this writer the packet write fails).
		registry.Add(singletonFactory{i: &twccExtWriter{}})
	}
	generatorFactory, err := nack.NewGeneratorInterceptor()
	if err != nil {
		return nil, fmt.Errorf("nack generator: %w", err)
	}
	generator, err := generatorFactory.NewInterceptor("callrelay-nack-generator")
	if err != nil {
		return nil, fmt.Errorf("nack generator: %w", err)
	}
	if budget == nil {
		// PCMU sessions have no session-owned budget (the controller that
		// would report it is nil); the generator still needs a real bucket
		// or its first NACK would nil-panic.
		budget = newNackBudget()
	}
	registry.Add(singletonFactory{i: &nackBudgetWrap{Interceptor: generator, budget: budget}})
	responder, err := nack.NewResponderInterceptor()
	if err != nil {
		return nil, fmt.Errorf("nack responder: %w", err)
	}
	registry.Add(responder)
	receiver, err := report.NewReceiverInterceptor()
	if err != nil {
		return nil, fmt.Errorf("rtcp receiver: %w", err)
	}
	registry.Add(receiver)
	sender, err := report.NewSenderInterceptor()
	if err != nil {
		return nil, fmt.Errorf("rtcp sender: %w", err)
	}
	registry.Add(sender)
	registry.Add(observerFactory{observer: observer})
	twccSender, err := twcc.NewSenderInterceptor()
	if err != nil {
		return nil, fmt.Errorf("twcc sender: %w", err)
	}
	registry.Add(twccSender)
	return pion.NewAPI(pion.WithMediaEngine(e.mediaEngine), pion.WithInterceptorRegistry(registry)), nil
}

// rrStats accumulates one window of Receiver-Report evidence per SSRC.
type rrStats struct {
	reports     int
	fractionSum int
	jitterMax   uint32
}

// rrObserver consumes inbound RTCP Receiver Reports (the client SDK emits
// them automatically once SR/RR negotiate) and logs bounded congestion
// windows: fraction-lost and inter-arrival jitter AS MEASURED BY THE PHONE.
// onWindow additionally hands each flushed window to the session's
// bitrateController (Opus sessions only) so the measured loss drives
// packet-loss-perc and FEC — feedback consumed as control, not just logged.
type rrObserver struct {
	mu       sync.Mutex
	window   map[uint32]*rrStats
	rawCount int
	rrCount  int
	engine   *Engine // back-reference for test observation; nil in tests
	onWindow func(fractionLost float64)
}

func newRRObserver() *rrObserver {
	return &rrObserver{window: make(map[uint32]*rrStats)}
}

// observerFactory adapts the shared observer to the interceptor.Factory
// shape; every peer connection binds the SAME observer (the window map is
// keyed by SSRC, which is unique per stream).
type observerFactory struct{ observer *rrObserver }

func (f observerFactory) NewInterceptor(_ string) (interceptor.Interceptor, error) {
	return f.observer, nil
}

func (o *rrObserver) ID() string { return "callrelay-rr-observer" }

// BindRTCPReader wraps the inbound RTCP reader and observes every packet.
func (o *rrObserver) BindRTCPReader(reader interceptor.RTCPReader) interceptor.RTCPReader {
	return interceptor.RTCPReaderFunc(func(buf []byte, attributes interceptor.Attributes) (int, interceptor.Attributes, error) {
		n, attrs, err := reader.Read(buf, attributes)
		if err == nil && n > 0 {
			o.observe(buf[:n])
		}
		return n, attrs, err
	})
}

func (o *rrObserver) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return writer
}
func (o *rrObserver) BindLocalStream(_ *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	return writer
}
func (o *rrObserver) UnbindLocalStream(_ *interceptor.StreamInfo) {}
func (o *rrObserver) BindRemoteStream(_ *interceptor.StreamInfo, reader interceptor.RTPReader) interceptor.RTPReader {
	return reader
}
func (o *rrObserver) UnbindRemoteStream(_ *interceptor.StreamInfo) {}
func (o *rrObserver) Close() error                                 { return nil }

func (o *rrObserver) observe(raw []byte) {
	packets, err := rtcp.Unmarshal(raw)
	if err != nil {
		return
	}
	var logLines []string
	o.mu.Lock()
	o.rawCount += len(packets)
	for _, packet := range packets {
		report, ok := packet.(*rtcp.ReceiverReport)
		if !ok {
			continue
		}
		o.rrCount++
		stats := o.window[report.SSRC]
		if stats == nil {
			stats = &rrStats{}
			o.window[report.SSRC] = stats
		}
		for _, reception := range report.Reports {
			stats.reports++
			stats.fractionSum += int(reception.FractionLost)
			if reception.Jitter > stats.jitterMax {
				stats.jitterMax = reception.Jitter
			}
		}
		// Bounded window: emit one line per ~5 s of reports (RR cadence is
		// ~1/s per stream) and reset.
		if stats.reports >= 5 {
			fraction := float64(stats.fractionSum) / float64(stats.reports*255)
			// Jitter is expressed in the RTP clock of the REPORTED stream
			// (1/8000 s for PCMU, 1/48000 s for Opus) — the unit follows
			// the negotiated codec, so it is logged as stream-clock ticks.
			logLines = append(logLines, fmt.Sprintf(
				"ssrc=%d fractionLost=%.3f jitter=%d(stream-clock) reports=%d",
				report.SSRC, fraction, stats.jitterMax, stats.reports))
			if o.onWindow != nil {
				o.onWindow(fraction)
			}
			delete(o.window, report.SSRC)
		}
	}
	o.mu.Unlock()
	for _, line := range logLines {
		slog.Info("media congestion feedback " + line)
	}
}

// observedWindows is the test seam: parsed-but-unflushed evidence.
func (o *rrObserver) observedWindows() map[uint32]rrStats {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make(map[uint32]rrStats, len(o.window))
	for ssrc, stats := range o.window {
		out[ssrc] = *stats
	}
	return out
}

func (o *rrObserver) debugRawCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.rawCount
}

func (o *rrObserver) debugRRCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.rrCount
}

// Transport is the worker-facing side of one call leg.
type Transport interface {
	Frames() <-chan []byte
	SendPCMU([]byte) error
	Close()
}

// hostFrame is one buffered host frame in its WIRE-encoded form. PCMU and
// WSS-Opus payloads arrive already decoded to PCM (their transports are
// ordered or synchronous); ICE-Opus payloads stay ENCODED until playout so
// the stateful Opus decoder only ever advances in timestamp order (never on
// reordered, duplicate or already-late packets) and can apply FEC/PLC at
// the playout deadline.
type hostFrame struct {
	pcm []int16 // PCMU/WSS-decoded audio (playout-ready)
	enc []byte  // Opus payload (decoded at playout)
}

// hostJitter is a bounded, RTP-timestamp-aware jitter buffer for the client
// direction. Frames are consumed exactly once; underflow yields silence for
// PCM and mature libopus PLC for Opus (never a repeated stale frame).
// Trimmed/dropped stale audio REBASES the play cursor to the oldest
// surviving frame: after a trim the next pop plays the newest available
// audio promptly. Without the rebase, a balanced producer/consumer pair
// locks into permanent silence — the cursor keeps pointing at the
// just-trimmed timestamp, every pop returns nil, and the trim counter grows
// ~50/s (observed live as host_trimmed≈250 per 5 s with host_depth pinned at
// the target).
//
// Opus playout: pop() decodes at the play cursor in timestamp order. A miss
// first tries RFC 7587 inband FEC (the next buffered packet carries the
// previous frame's redundancy: decode_fec recovers the missed frame without
// consuming the carrier) and falls back to libopus PLC. This is the
// depacketizer discipline the stateful decoder requires; duplicates are
// rejected by timestamp idempotency BEFORE any decode, and late
// retransmissions are rejected by the playout deadline BEFORE any decode.
type hostJitter struct {
	mu     sync.Mutex
	frames map[uint32]hostFrame
	play   uint32
	// primed gates playback buffering (false during initial prime and a
	// post-underrun rebuffer). started says the play cursor was ever
	// established: once true, behindPlay keeps rejecting already-played
	// timestamps even while de-primed for a rebuffer. started resets only on
	// a genuine timeline/transport reset (clear), never on an underrun.
	started  bool
	primed   bool
	lastPush time.Time
	trimmed  uint64
	// Adaptive playout DELAY (build 48), in 20 ms frames. level is the
	// prime/rebuffer threshold, NEVER a trim threshold: the hard latency
	// trim stays fixed at hostJitterTarget so normal 2-6 frame TCP batches
	// are always retained intact (trimming at the relaxed level would drop
	// good speech from a burst before any underrun grew the buffer). Start
	// 40 ms; ten seconds of continuous LIVE playback lowers the next prime to
	// 20 ms; one ACTIVE starvation episode (an empty play tick whose previous
	// tick still delivered a frame, with recent arrivals) raises it one frame
	// and forces a single re-prime, up to 120 ms. Genuine idle/quiet with no
	// arrivals never grows it. Repeated empty ticks in one starvation episode
	// bump it at most once.
	// level is the adaptive prime/rebuffer threshold in 20 ms frames:
	// 40 ms at start, as low as 20 ms after ten clean seconds, +20 ms
	// immediately after an active underrun (capped at 120 ms). It governs
	// initial prime and re-prime after a genuine timeline reset (clear), and
	// the steady-drift trim in push(); a coalesced batch (fast arrival) is
	// never trimmed to it. liveFrames counts delivered frames; starvedEpisode
	// makes growth happen once per starvation episode.
	level          int
	liveFrames     uint64
	underruns      uint64
	starvedEpisode bool
	// rebuffering pauses consumption after an ACTIVE starvation episode
	// while the queue refills to the adaptive level (bounded: level x 20 ms
	// at 50 fps arrivals). The play cursor stays put so late frames within
	// the refill window are retained; resume restarts at the oldest buffered
	// frame. Build-49.
	rebuffering bool
	// dec is the session's Opus decoder for playout-time decoding. Nil for
	// PCM-only sessions. Set once at codec setup; pop() is the only caller
	// (single mixer goroutine) and serializes against encoder CTLs with the
	// session codecMu.
	dec *opus.Decoder
	// tick is the RTP timestamp step per 20 ms frame: 160 for the PCMU 8 kHz
	// clock, 960 for the RFC 7587 Opus 48 kHz clock. pop() advances the play
	// cursor by tick on misses so loss concealment slots stay aligned with
	// the negotiated codec.
	tick uint32
	// Bounded delivery-spacing evidence: fastArrivals counts pushes whose
	// predecessor arrived <10 ms earlier (transport coalescing/batching),
	// maxGapNanos the longest observed inter-arrival gap. They measure the
	// real burstiness the latency bound must absorb so the bound can be
	// tuned with data instead of inference.
	fastArrivals uint64
	maxGapNanos  int64
}

const (
	hostJitterCapacity = 100 // 2 s @ 20 ms hard memory bound; latency bound is hostJitterTarget
	hostJitterPrime    = 2   // ~40 ms prebuffer before playback starts
	// Adaptive playout levels (build 48), in 20 ms frames. Start at 40 ms,
	// floor 20 ms after hostAdaptiveStableWindow of clean playback; an active
	// underrun raises the level by one frame immediately up to the 120 ms
	// latency bound. Underruns only count while frames recently arrived
	// (hostActiveWindow), so a genuinely quiet/stopped mic never inflates the
	// buffer.
	hostJitterStartLevel   = 2
	hostJitterMinLevel     = 1
	hostJitterStableFrames = 500 // 10 s of actually-delivered 20 ms frames
	hostJitterActiveWindow = 250 * time.Millisecond
	// hostSteadyTrimGap distinguishes a coalesced batch (several frames
	// arrive within the same <10 ms tick — keep them even above the steady
	// level) from clock-drift overfill (one-frame-at-a-time ~20 ms arrivals
	// slowly raise depth — trim to the steady level). Mirrors fastArrivals.
	hostSteadyTrimGap    = 10 * time.Millisecond
	legQueueStartLevel   = 4 // 80 ms modem-leg playout at start
	legQueueMinLevel     = 2 // 40 ms floor after clean playback
	legQueueStableFrames = 500
	legQueueActiveWindow = 100 * time.Millisecond
	// legSteadyGap: arrivals spaced >= this far are non-batch; slow drift
	// overfill is trimmed to the adaptive level in the pump.
	legSteadyGap = 15 * time.Millisecond
	// hostLiveWindow must EXCEED the latency bound (hostJitterTarget × 20 ms):
	// while a bursty transport refills the buffer in batches, no single frame
	// arrives during the inter-batch gap and a too-small window would declare
	// the host dead and DISCARD the audio the buffer is holding.
	hostLiveWindow = 250 * time.Millisecond
	// hostJitterTarget bounds the client->modem jitter buffer: a burst that
	// arrives faster than the 20 ms mixer tick is trimmed to the newest
	// target-sized block (with the cursor rebase above, playback skips the
	// stale gap and resumes promptly — never permanent silence). 6 frames =
	// 120 ms worst-case added latency while steady traffic stays at the
	// ~40 ms prime. Whether real ingress delivery batches beyond 120 ms is
	// UNMEASURED: the 2026-10-04 host_trimmed=1367 evidence proves sustained
	// trims under the OLD code but is equally explained by the cursor-lockstep
	// bug alone (one overflow + smooth traffic trims forever), so it says
	// nothing about batch size. The spacing metrics above are logged per call;
	// raise this bound ONLY with that measured data.
	hostJitterTarget = 6
	// Compile-time guard: the live window must stay larger than a full
	// latency bound of buffered audio.
	_ = hostLiveWindow / (hostJitterTarget * 20 * time.Millisecond)
)

// legQueueTargetFrames is the historic fixed modem->client playout budget;
// it equals the build-48 adaptive START level (80 ms) and remains the hard
// upper trim cap. The live level relaxes to 40 ms on clean playback and is
// restored immediately after an active underrun (see Leg.pop).
const legQueueTargetFrames = legQueueStartLevel

func newHostJitter(tick uint32) *hostJitter {
	if tick == 0 {
		tick = 160
	}
	return &hostJitter{
		frames: make(map[uint32]hostFrame), tick: tick,
		level: hostJitterStartLevel,
	}
}

// adaptiveStats reports the current prime/rebuffer level (20 ms frames) and
// cumulative active-starvation episode count for per-call evidence.
func (j *hostJitter) adaptiveStats() (level int, underruns uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.level, j.underruns
}

// noteLiveFrameLocked accounts one frame or concealment actually delivered at
// pop and closes any open starvation episode: after hostJitterStableFrames
// consecutive live frames (10 s at 50 fps) the prime level relaxes one 20 ms
// step toward the floor. countsTowardStability is false for CONCEALED frames
// (build-49): PLC/FEC synthesis masks a hole, so it may close the episode but
// must never count as clean received stability — otherwise sustained loss
// with concealment looks like clean playback and drains the buffer. Caller
// must hold j.mu.
func (j *hostJitter) noteLiveFrameLocked(countsTowardStability bool) {
	j.starvedEpisode = false
	if !countsTowardStability {
		return
	}
	if j.level > hostJitterMinLevel {
		j.liveFrames++
		if j.liveFrames >= hostJitterStableFrames {
			j.liveFrames = 0
			j.level--
		}
	}
}

// noteStarvationLocked handles a missing play slot with NO concealment.
// While frames were still arriving within the activity window this is an
// ACTIVE underrun: the 10 s uninterrupted-live stability count resets on
// EVERY such tick (so intermittent isolated losses can never relax the
// level), and the level grows +20 ms immediately at most once per starvation
// episode (capped at hostJitterTarget). Build-49: the raised level is REAL
// headroom — pop() pauses consumption and refills to the level via the
// bounded adaptive rebuffer. Cursor validity (started) stays intact so
// already-played timestamps keep being rejected. Genuine quiet (no recent
// arrival) does nothing. Caller must hold j.mu.
func (j *hostJitter) noteStarvationLocked(now time.Time) (grew bool) {
	if !j.starvationActiveLocked(now) {
		return false
	}
	j.liveFrames = 0
	if j.starvedEpisode {
		return false
	}
	j.starvedEpisode = true
	j.underruns++
	if j.level < hostJitterTarget {
		j.level++
		grew = true
	}
	return grew
}

// setDecoder installs the playout-time Opus decoder. Called once per codec
// setup; the decoder is used only from pop().
func (j *hostJitter) setDecoder(dec *opus.Decoder) {
	j.mu.Lock()
	j.dec = dec
	j.mu.Unlock()
}

// pushPCM buffers one already-decoded PCM frame (PCMU or WSS paths).
func (j *hostJitter) pushPCM(timestamp uint32, pcm []int16) {
	frame := make([]int16, len(pcm))
	copy(frame, pcm)
	j.push(timestamp, hostFrame{pcm: frame})
}

// pushEncoded buffers one wire-encoded Opus frame for playout-time decode.
func (j *hostJitter) pushEncoded(timestamp uint32, enc []byte) {
	payload := make([]byte, len(enc))
	copy(payload, enc)
	j.push(timestamp, hostFrame{enc: payload})
}

// oldestLocked returns the lowest timestamp currently buffered. Callers must
// hold j.mu. Wrap comparison matches the existing uint32 RTP timestamp order.
func (j *hostJitter) oldestLocked() uint32 {
	oldest := uint32(0)
	first := true
	for candidate := range j.frames {
		if first || int32(candidate-oldest) < 0 {
			oldest, first = candidate, false
		}
	}
	return oldest
}

func (j *hostJitter) push(timestamp uint32, frame hostFrame) {
	j.mu.Lock()
	defer j.mu.Unlock()
	// Bounded delivery-spacing evidence (cheap; consumed by stats).
	now := time.Now()
	if !j.lastPush.IsZero() {
		if gap := now.Sub(j.lastPush); gap < 10*time.Millisecond {
			j.fastArrivals++
		} else if gap.Nanoseconds() > j.maxGapNanos {
			j.maxGapNanos = gap.Nanoseconds()
		}
	}
	// Duplicate rejection BEFORE any Opus decode: a retransmitted or
	// reordered repeat of an already-buffered timestamp must never reach the
	// stateful decoder twice.
	if _, exists := j.frames[timestamp]; exists {
		j.lastPush = now
		return
	}
	trimmedAny := false
	// Fixed hard latency bound (hostJitterTarget = 6 frames = 120 ms) always
	// applies. Below it, the adaptive level trims ONLY steady overfill from
	// clock drift: a coalesced TCP/WSS batch refills several frames in <10 ms
	// (fast arrival) and must be retained intact even when the steady level
	// relaxed to 20 ms, while one-frame-at-a-time arrivals that let the buffer
	// creep above the level are real drift and shrink to it.
	slowArrival := j.lastPush.IsZero() || now.Sub(j.lastPush) >= hostSteadyTrimGap
	for len(j.frames) >= hostJitterTarget {
		delete(j.frames, j.oldestLocked())
		j.trimmed++
		trimmedAny = true
	}
	if slowArrival {
		for len(j.frames) > j.level {
			delete(j.frames, j.oldestLocked())
			j.trimmed++
			trimmedAny = true
		}
	}
	if len(j.frames) >= hostJitterCapacity {
		delete(j.frames, j.oldestLocked())
		j.trimmed++
		trimmedAny = true
	}
	j.frames[timestamp] = frame
	j.lastPush = now
	// Cursor rebase: dropping stale audio must never leave the play cursor
	// pointing at a deleted frame — with one push and one pop per tick the
	// cursor and the oldest frame would otherwise advance in lockstep and
	// pop would return nil forever (~50 trims/s of permanent silence). Skip
	// FORWARD to the oldest surviving frame (wrap-safe); frames behind the
	// cursor are left for the session-level DropStaleHostAudio clear.
	if trimmedAny && j.primed {
		if _, ok := j.frames[j.play]; !ok {
			if oldest, found := j.oldestFrameLocked(); found && int32(oldest-j.play) > 0 {
				j.play = oldest
			}
		}
	}
}

// oldestFrameLocked returns the lowest buffered timestamp and whether any
// frame is buffered. Callers must hold j.mu.
func (j *hostJitter) oldestFrameLocked() (uint32, bool) {
	if len(j.frames) == 0 {
		return 0, false
	}
	return j.oldestLocked(), true
}

// stats reports the current buffer depth, the cumulative number of stale
// frames dropped by the latency bound, and the delivery-spacing evidence.
// The spacing counters are consumed (reset) by each call so the periodic
// evidence log reports per-window values.
func (j *hostJitter) stats() (int, uint64, uint64, time.Duration) {
	j.mu.Lock()
	defer j.mu.Unlock()
	fast := j.fastArrivals
	maxGap := time.Duration(j.maxGapNanos)
	j.fastArrivals, j.maxGapNanos = 0, 0
	return len(j.frames), j.trimmed, fast, maxGap
}

// pop consumes at most one 20 ms frame, decoded at the play cursor when the
// session is Opus. A nil result means underflow and the caller must send
// silence instead of repeating the previous frame. now drives the adaptive
// playout-level decisions (stable relaxation and active-underrun growth).
//
// Build-49 bounded adaptive rebuffer: an ACTIVE starvation episode pauses
// consumption (rebuffering) and refills the buffer to the ADAPTIVE level —
// the raised level is real playable headroom, not just a trim threshold.
// Consumption pauses only while the queue is below the target; frames
// accumulate at the live arrival rate (50 fps), so the refill is bounded
// (level x 20 ms) and can never permanently stall steady one-frame-per-tick
// playback. On resume the cursor restarts at the oldest buffered frame, so
// only frames that never arrived are skipped (bounded to the stall window).
// Genuine quiet (no recent arrivals) keeps the historical behavior: the
// cursor advances and drops history, ready for a fresh start on the next
// burst. hostLive/liveness is untouched (primed stays set; liveness is
// arrival-driven), so the mixer's present/live semantics do not churn.
func (j *hostJitter) pop(now time.Time) []int16 {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.rebuffering {
		if len(j.frames) < j.rebufferTargetLocked() {
			return nil // bounded refill in progress; consumption paused
		}
		j.rebuffering = false
		j.play = j.oldestLocked()
	}
	if !j.primed {
		// Initial prime (fresh attach / timeline reset): fixed 40 ms.
		// Mid-stream refills use the adaptive rebuffer above instead, so the
		// grown level governs real headroom there. Waiting moves no cursor,
		// and an established cursor (started) keeps rejecting old timestamps
		// via behindPlay even while unprimed.
		if len(j.frames) < hostJitterPrime {
			return nil
		}
		j.play = j.oldestLocked()
		j.primed = true
		j.started = true
	}
	if frame, ok := j.frames[j.play]; ok {
		delete(j.frames, j.play)
		j.play += j.tick
		j.noteLiveFrameLocked(true)
		return j.renderLocked(frame)
	}
	// Missing expected timestamp. Try Opus inband FEC / PLC first: a
	// concealed single loss keeps playback gap-free and is NOT an audible
	// underrun, so it neither grows the level nor de-primes the buffer.
	var concealed []int16
	if j.dec != nil {
		if carrier, ok := j.frames[j.play+j.tick]; ok && len(carrier.enc) > 0 {
			if pcm, err := j.dec.DecodeFEC(carrier.enc); err == nil {
				concealed = pcm
			}
		}
		if concealed == nil {
			if pcm, err := j.dec.Decode(nil); err == nil {
				concealed = pcm
			}
		}
	}
	if concealed != nil {
		j.play += j.tick
		// Build-49: concealment masks a hole — the stream IS delivering audio,
		// so the starvation episode closes, but a SYNTHESIZED frame must never
		// count toward the clean-live stability that relaxes the level
		// (500 concealed ticks would otherwise look like 10 s of clean
		// playback and drain the buffer while loss continues).
		j.noteLiveFrameLocked(false)
		return concealed
	}
	// True audible starvation (no frame, no concealment). ACTIVE (recent
	// arrivals): open/continue the starvation episode (once-per-episode
	// growth + stability reset) and pause consumption for the bounded
	// adaptive rebuffer — the play cursor stays put so late frames within the
	// refill window are retained. Genuine quiet: advance the cursor and drop
	// history as before (no failure signal for silence).
	if j.starvationActiveLocked(now) {
		j.noteStarvationLocked(now)
		if !j.rebuffering {
			j.rebuffering = true
		}
	} else {
		j.play += j.tick
	}
	return nil
}

// starvationActiveLocked reports whether an empty play tick counts as an
// active underrun (frames were still arriving within the activity window).
func (j *hostJitter) starvationActiveLocked(now time.Time) bool {
	return !j.lastPush.IsZero() && now.Sub(j.lastPush) <= hostJitterActiveWindow
}

// rebufferTargetLocked is the queue depth the bounded adaptive rebuffer
// refills to: the adaptive level itself (>= the 20 ms floor).
func (j *hostJitter) rebufferTargetLocked() int {
	if j.level < hostJitterMinLevel {
		return hostJitterMinLevel
	}
	return j.level
}

// renderLocked renders one buffered frame at playout: pass-through PCM, or
// playout-time Opus decode. A corrupt packet conceals via PLC rather than
// producing a hole. Caller must hold j.mu.
func (j *hostJitter) renderLocked(frame hostFrame) []int16 {
	if frame.enc == nil {
		return frame.pcm
	}
	if j.dec == nil {
		return nil
	}
	pcm, err := j.dec.Decode(frame.enc)
	if err != nil {
		pcm, err = j.dec.Decode(nil)
		if err != nil {
			return nil
		}
	}
	return pcm
}

func (j *hostJitter) live(now time.Time) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.primed && !j.lastPush.IsZero() && now.Sub(j.lastPush) < hostLiveWindow
}

// depth reports the buffered host frame count without consuming the
// delivery-spacing counters (stats() resets those for the periodic log).
func (j *hostJitter) depth() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.frames)
}

func (j *hostJitter) clear() {
	j.mu.Lock()
	j.frames = make(map[uint32]hostFrame)
	j.primed = false
	j.started = false
	j.rebuffering = false
	// A genuine timeline/transport reset starts again at the 40 ms buffer;
	// the adaptive level is an in-stream playout-depth decision, not carried
	// across a send-stall resync or transport replacement.
	j.level = hostJitterStartLevel
	j.liveFrames = 0
	j.starvedEpisode = false
	j.lastPush = time.Time{}
	j.mu.Unlock()
}

// behindPlay reports whether the given timestamp already fell behind the
// play cursor. Transports whose timestamps pause while the mixer keeps
// advancing the cursor (synthetic timestamps over a stalled socket) would
// otherwise be dropped forever.
func (j *hostJitter) behindPlay(timestamp uint32) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	// Cursor validity is independent of the current prime state: a
	// post-underrun rebuffer de-primes playback but must not resurrect
	// already-played timestamps. started resets only on a genuine timeline
	// reset (clear on transport replacement), never on an underrun.
	if !j.started {
		return false
	}
	return int32(timestamp-j.play) < 0
}

type Leg struct {
	CallID    string
	transport Transport

	mu        sync.Mutex
	queue     [][]int16
	dropped   uint64
	held      bool
	closed    bool
	closeOnce sync.Once
	onClosed  func(callID string, err error)
	// Adaptive modem->client playout DELAY (build 48): level is the rebuffer
	// threshold, not a trim threshold (the hard trim stays fixed at
	// legQueueStartLevel so an 80 ms burst is never chopped at the relaxed
	// 40 ms level). Start 80 ms; 500 consecutive delivered frames relax one
	// step to 40 ms; one ACTIVE starvation episode restores 80 ms once and
	// forces a rebuffer. Genuine idle is never an underrun.
	lastArrival time.Time
	level       int
	liveFrames  uint64
	underruns   uint64
	lastPopLive bool
	// burstCredit is the number of frames of the LATEST coalesced UAC/network
	// batch still queued (arrivals <legSteadyGap apart). While credit covers
	// the over-level depth, the batch is retained even at the relaxed 40 ms
	// level; slow one-frame-at-a-time drift (no credit) is trimmed to level.
	burstCredit int

	// Per-leg downstream (modem->client direction) SpeexDSP preprocessor,
	// applied to this leg's popped frames before mix. Owned by the leg so it
	// follows the leg across host handovers and conference split/park; lazy
	// on first enable and destroyed at Close. Toggling off is gate-only so
	// the noise estimate survives. Guarded by ppMu independently of l.mu so
	// SetDownlinkDenoise never blocks waiting on a frame pop.
	ppMu      sync.Mutex
	pp        *speexpp.Preprocessor
	ppEnabled bool
	procNanos int64
	// Build-49 evidence window: per-frame processing statistics (reset by
	// the periodic evidence line).
	windowNanos  int64
	windowFrames int64
}

const maxQueuedFrames = 50

func NewLeg(callID string, transport Transport, onClosed func(string, error)) *Leg {
	leg := &Leg{
		CallID: callID, transport: transport, onClosed: onClosed,
		level: legQueueStartLevel,
	}
	go leg.pump()
	return leg
}

func (l *Leg) pump() {
	frames := l.transport.Frames()
	for frame := range frames {
		pcm, err := voice.DecodePCMU(frame)
		if err != nil {
			continue
		}
		decoded := make([]int16, len(pcm))
		copy(decoded, pcm)
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return
		}
		if len(l.queue) >= maxQueuedFrames {
			copy(l.queue, l.queue[1:])
			l.queue = l.queue[:len(l.queue)-1]
			if l.burstCredit > 0 {
				l.burstCredit--
			}
		}
		// Batch accounting: burstCredit counts the trailing run of frames
		// that arrived in fast succession (<legSteadyGap apart) — one
		// coalesced UAC/network batch which the adaptive trim must retain
		// even when it exceeds the relaxed 40 ms level. The first frame of a
		// batch only becomes known when the second arrives, so that jump
		// counts both. A paced (~20 ms) arrival earns no new credit.
		now := time.Now()
		fast := !l.lastArrival.IsZero() && now.Sub(l.lastArrival) < legSteadyGap
		l.queue = append(l.queue, decoded)
		if fast {
			if l.burstCredit == 0 {
				l.burstCredit = 2
			} else {
				l.burstCredit++
			}
			if l.burstCredit > len(l.queue) {
				l.burstCredit = len(l.queue)
			}
		}
		l.lastArrival = now
		l.mu.Unlock()
	}
	l.finish(fmt.Errorf("line media closed"))
}

func (l *Leg) finish(err error) {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		l.queue = nil
		l.mu.Unlock()
		l.closePreprocessor()
		if l.onClosed != nil {
			l.onClosed(l.CallID, err)
		}
	})
}

// pop consumes one frame. The adaptive level is the steady trim depth
// (80 ms start, 40 ms after ten clean seconds); a coalesced batch is
// protected by burstCredit so its fresh frames are retained even above the
// relaxed level. now drives live-frame stability and active-underrun
// detection.
func (l *Leg) pop(now time.Time) []int16 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.queue) == 0 {
		active := !l.lastArrival.IsZero() && now.Sub(l.lastArrival) <= legQueueActiveWindow
		if active {
			// Every active empty tick is an underrun: reset the 10 s
			// uninterrupted-live stability count so intermittent isolated
			// losses can never relax the level. Restore 80 ms once per
			// starvation episode (first empty tick after a delivered
			// frame). Genuine idle (no recent arrival) does nothing.
			l.liveFrames = 0
			if l.lastPopLive {
				l.underruns++
				if l.level < legQueueStartLevel {
					l.level = legQueueStartLevel
				}
			}
		}
		l.lastPopLive = false
		return nil
	}
	// Adaptive steady trim: keep at least `level` frames, but never drop
	// frames of the latest coalesced batch (the newest burstCredit frames),
	// which are fresh. The hard cap (maxQueuedFrames in the pump) still
	// bounds memory. A zero-value Leg defaults to the start level.
	if l.level == 0 {
		l.level = legQueueStartLevel
	}
	depth := len(l.queue)
	keep := l.level
	if protected := l.burstCredit; protected > depth {
		protected = depth
	} else if protected > keep {
		keep = protected
	}
	if drop := depth - keep; drop > 0 {
		l.queue = l.queue[drop:]
		l.dropped += uint64(drop)
	}
	frame := l.queue[0]
	l.queue = l.queue[1:]
	if l.burstCredit > 0 {
		l.burstCredit--
	}
	l.lastPopLive = true
	// 500 consecutive delivered frames (10 s of live playback) relax one
	// 20 ms step toward the 40 ms floor.
	if l.level > legQueueMinLevel {
		l.liveFrames++
		if l.liveFrames >= legQueueStableFrames {
			l.liveFrames = 0
			l.level--
		}
	}
	return frame
}

// stats reports the current queue depth and the cumulative number of stale
// frames dropped by the latency bound.
func (l *Leg) stats() (int, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue), l.dropped
}

// adaptiveStats reports the current playout level (20 ms frames) and the
// cumulative active-underrun count for per-call evidence.
func (l *Leg) adaptiveStats() (level int, underruns uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level, l.underruns
}

// Drain empties buffered line audio. It is called on hold, host disconnect
// and unhold so no stale audio leaks after the state transition.
func (l *Leg) Drain() {
	l.mu.Lock()
	l.queue = nil
	l.mu.Unlock()
}

func (l *Leg) SetHeld(held bool) {
	l.mu.Lock()
	l.held = held
	l.queue = nil
	l.mu.Unlock()
}

func (l *Leg) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// SetDownlinkDenoise hot-toggles this leg's downstream preprocessor. State
// (the Speex noise estimate) is preserved across toggles and across host
// handovers; a disabled leg passes raw audio. Allocation is lazy on the first
// enable so an error leaves the established path untouched. Never blocks a
// frame: the gate and state are separate from l.mu.
func (l *Leg) SetDownlinkDenoise(enabled bool) {
	l.ppMu.Lock()
	defer l.ppMu.Unlock()
	l.ppEnabled = enabled
	if enabled && l.pp == nil {
		if pp, err := speexpp.New(true); err == nil {
			l.pp = pp
		} else {
			slog.Warn("denoise leg preprocessor unavailable; raw pass-through",
				"call_id", l.CallID, "error", err)
		}
	}
	if l.pp != nil {
		l.pp.SetEnabled(enabled)
	}
}

// processDownlink applies the leg preprocessor to one frame, returning the
// frame untouched when disabled or on any error (never blocks/drops).
func (l *Leg) processDownlink(frame []int16) []int16 {
	l.ppMu.Lock()
	pp, enabled, started := l.pp, l.ppEnabled, false
	if pp != nil && enabled {
		started = true
	}
	l.ppMu.Unlock()
	if !started || pp == nil || len(frame) != speexpp.FrameSamples {
		return frame
	}
	start := time.Now()
	out := pp.Process(frame)
	l.ppMu.Lock()
	elapsed := time.Since(start).Nanoseconds()
	l.procNanos += elapsed
	l.windowNanos += elapsed
	l.windowFrames++
	l.ppMu.Unlock()
	return out
}

func (l *Leg) closePreprocessor() {
	l.ppMu.Lock()
	if l.pp != nil {
		l.pp.Close()
		l.pp = nil
	}
	l.ppMu.Unlock()
}

// denoiseStats reports the gate state and cumulative processing time.
func (l *Leg) denoiseStats() (enabled bool, nanos int64) {
	l.ppMu.Lock()
	defer l.ppMu.Unlock()
	return l.ppEnabled && l.pp != nil, l.procNanos
}

// denoiseStatsWindow consumes (resets) the per-frame window counters and
// returns them with the frame count, so the periodic evidence line can
// report per-frame cost instead of only the cumulative total.
func (l *Leg) denoiseStatsWindow() (enabled bool, nanos, frames int64) {
	l.ppMu.Lock()
	defer l.ppMu.Unlock()
	nanos, frames = l.windowNanos, l.windowFrames
	l.windowNanos, l.windowFrames = 0, 0
	return l.ppEnabled && l.pp != nil, nanos, frames
}

func (l *Leg) Send(frame []byte) error {
	if l.Held() {
		return nil
	}
	return l.transport.SendPCMU(frame)
}

func (l *Leg) Close() {
	l.finish(nil)
	l.transport.Close()
}

type Session struct {
	Engine *Engine
	Key    string
	Kind   string // call | conference

	mu          sync.Mutex
	pc          *pion.PeerConnection
	track       *pion.TrackLocalStaticRTP
	ws          *wsHost
	legs        map[string]*Leg
	hostPresent bool
	host        *hostJitter
	generation  uint64
	closed      bool
	sequence    uint16
	timestamp   uint32
	// Hot-standby slots (build 50): a second, fully attached authenticated
	// transport held warm alongside the primary. Exactly one of pc/ws is the
	// primary and feeds/mixes host audio; the standby slot of the other
	// family receives the SAME downlink mix (seamless promotion) while its
	// inbound frames are decoded for freshness and discarded, never mixed —
	// the single-uplink invariant holds regardless of client behavior.
	pcStandby        *pion.PeerConnection
	trackStandby     *pion.TrackLocalStaticRTP
	wsStandby        *wsHost
	wsStandbyFreshAt time.Time
	pcStandbyFreshAt time.Time
	// Dedicated QUIC role slots (build 63): the QUIC DATAGRAM adapter is a
	// first-class host transport with its own primary/standby slots instead
	// of anonymously sharing the ws-family slots. Exactly one of
	// pc/ws/quic is the primary; each family's standby slot holds its own
	// demoted transport, so RTC/QUIC/WSS can stay independently warm across
	// repeated handovers (no one-slot eviction: Q→RTC demotes Q into
	// quicStandby while the real WSS keeps wsStandby — and vice versa).
	quic               *wsHost
	quicStandby        *wsHost
	quicStandbyFreshAt time.Time
	quicStandbyToken   string
	// epoch is the PRIMARY role epoch (coordinator rev 2 fencing): bumped
	// whenever the primary identity changes (primary attach/adoption/role
	// promotion). Standby attach/adoption never bumps it. Promotion commands
	// carry expectEpoch; a mismatch means a racing swap won and the client
	// must refresh instead of swapping on stale state.
	epoch uint64
	// wsStandbyToken/pcStandbyToken identify the CURRENT standby instance of
	// each family; a promotion command must pin the exact token, so a
	// replaced/renewed standby can never be promoted by a late command.
	wsStandbyToken string
	pcStandbyToken string
	// promoteNonces gives idempotent retries: nonce -> outcome with expiry.
	// Bounded (promoteNonceLimit entries, promoteNonceTTL); a replayed nonce
	// returns the recorded outcome instead of re-executing the swap.
	promoteNonces   map[string]promoteNonceEntry
	promoteNonceSeq uint64
	// Trickle ICE (build 68): an attach negotiated as incremental records an
	// unguessable token; every later candidate batch must pin that exact
	// token, so candidates trickling in after the peer connection was
	// replaced are ignored instead of corrupting the new attachment. The
	// batch/total counters are the bounded rate/size protection.
	attachToken      string
	attachTrickle    bool
	attachCandidates int
	// Server-side incremental candidate delivery (build 68): with a trickle
	// attach the answer is returned BEFORE the gateway's ICE gathering
	// finishes, so the client collects the remaining gateway candidates from
	// a bounded, ordered queue through the same authenticated HTTP plane.
	//
	// The queue is an ATTACHMENT-OWNED object, not session state: the ICE
	// callbacks of a replaced peer connection hold their own queue, so an old
	// callback can never append into (or complete) the NEW attachment's
	// stream, and a candidate poll reads the queue it captured with the
	// validated token instead of whatever the session points at later.
	gatherQueue *gatherStream
	// probeEcho marks a detached probe session (never registered in the
	// Manager, no line legs): every host frame is written back so the client
	// can measure a true end-to-end round trip without touching call state.
	probeEcho bool
	// route retargets this pc's incoming host frames after adoption: pion
	// fires OnTrack only when a track ARRIVES, so a handler re-registered at
	// adoption time would never run for the already-negotiated track. The
	// read loop installed by AttachClient consults route on every frame
	// instead; AdoptClientPeer swaps it to the live session.
	route atomic.Pointer[routeTarget]

	stop     chan struct{}
	stopOnce sync.Once

	// Network-hop codec state (Opus sessions only; nil for PCMU). The
	// mutex serializes every libopus call on this session: the mixer
	// goroutine encodes, the track read goroutine decodes (via the route
	// handoff the OWNING session keeps), the controller applies CTLs and
	// probe adoption transfers the encoder — libopus instances are not
	// thread-safe.
	codecMu     sync.Mutex
	clientCodec string // codecOpus | codecPCMU
	payloadType uint8
	opusEnc     *opus.Encoder
	opusDec     *opus.Decoder
	controller  *bitrateController
	// nackBudget caps this session's uplink retransmission requests
	// (uplink-direction, constant-rate — see controller.go).
	nackBudget *nackBudget
	// codecOwned is true while this session must release the codec
	// machinery on close; probe adoption clears it on the probe and sets it
	// on the live session (ownership of the estimator follows the pc).
	codecOwned bool

	// statsTick drives the periodic bounded-latency evidence log; the
	// reported counters track the last logged totals. Guarded by mu.
	statsTick          uint64
	reportedHostTrim   uint64
	reportedLegTrimmed uint64

	// uplinkStateKnown/uplinkStalled track the host-uplink liveness edge so
	// the one-shot transition log fires once per stall/resume, never per
	// mixer tick. Guarded by mu; known resets when the host transport goes
	// away so a later re-attach logs its own edges.
	uplinkStateKnown bool
	uplinkStalled    bool

	// Build-48 directional denoise. uplinkPP processes the host (client mic)
	// frame once before it is mixed to the legs; legPP holds one isolated
	// preprocessor per external leg applied to that leg's frames before mix.
	// State is session-scoped: it survives an ICE<->WS host handover (the
	// legs and the call persist) and is destroyed at Close. Gate-only hot
	// toggles preserve the Speex noise estimate. Any library error latches
	// raw pass-through inside the wrapper, so audio is never blocked.
	// Directional denoise policy and the session uplink preprocessor are all
	// guarded by s.mu with the rest of the transport state (SetDenoise is an
	// infrequent control path; the mixer reads the pointer under the same
	// snapshot it already takes). Processing time is a privacy-safe atomic
	// scalar for per-call evidence.
	uplinkPP         *speexpp.Preprocessor
	denoiseProcNanos atomic.Int64
	// Build-49 evidence window: per-frame processing statistics for the
	// periodic scalar line (distinguishing CUMULATIVE compute from the
	// per-frame cost). Consumed (reset) by the 5 s evidence block.
	denoiseWindowNanos  atomic.Int64
	denoiseWindowFrames atomic.Int64
	uplinkDenoise       bool
	downlinkDenoise     bool

	// freshPending/freshGen: after a host attaches/reconnects, recovery is
	// not cleared until the first genuine host frame for that generation
	// arrives. Proves recovery with media, not the control-plane attach or an
	// old connection's RTT.
	freshPending bool
	freshGen     uint64

	onHostLost     func(key string, cause HostLossCause)
	onHostRestored func(key string)
	onLegClosed    func(callID string, err error)
	// recUplink/recDownlink are optional read-only recording taps installed
	// by the runtime for live 1:1 call sessions (nil for conferences,
	// probes and preflights). They run on the 20 ms mixer goroutine and MUST
	// NOT block; the recording package only enqueues into a bounded queue.
	// The uplink tap receives the canonical post-selection uplink frame
	// (after uplink denoise, exactly the frame the mix-minus leg send uses);
	// the downlink tap receives this call's own leg frame (after downlink
	// denoise, exactly the frame the client mix uses). Both are called once
	// per mixer tick with nil when no audio is available, so the two
	// recorded tracks stay time-aligned; standby transports are never tapped
	// twice, which keeps one logical uplink across ICE<->WS handovers and
	// hot-standby promotions.
	recUplink   func([]int16)
	recDownlink func([]int16)
}

const (
	promoteNonceLimit = 8
	promoteNonceTTL   = 30 * time.Second
)

type promoteNonceEntry struct {
	outcome     PromoteOutcome
	kind        string
	token       string
	expectEpoch uint64
	at          time.Time
	order       uint64
}

// PromoteOutcome is the acked result of a fenced standby promotion. Epoch is
// the epoch the swap produced; CurrentEpoch is the session's epoch at the
// moment of this response — on a FRESH execution they are equal; on a nonce
// REPLAY they can differ (another swap intervened), which tells the client
// to reconcile against /media/state before trusting the recorded roles.
type PromoteOutcome struct {
	Epoch        uint64 `json:"epoch"`
	CurrentEpoch uint64 `json:"currentEpoch"`
	Primary      string `json:"primary"`
	Standby      string `json:"standby,omitempty"`
	// StandbyToken is the identity token of the transport DEMOTED into the
	// standby slot by this swap — the client needs it to promote that
	// transport back later.
	StandbyToken string `json:"standbyToken,omitempty"`
}

// AttachInfo is handed to the ready callback so the greeting can carry the
// session epoch and (for a standby attach) the standby token.
type AttachInfo struct {
	Epoch   uint64
	Standby bool
	Token   string
}

// routeTarget is one atomic hop for adopted host frames.
type routeTarget struct {
	session    *Session
	generation uint64
}

// offerPrefersOpus reports whether the client's offer lists Opus in its
// audio m-line. That is the only signal needed: the answer picks the codec
// from what the offer advertises, so an Opus-capable offer becomes an Opus
// call and a PCMU-only offer (older app builds) keeps negotiating PCMU with
// zero client changes.
func offerPrefersOpus(offerSDP string) bool {
	for _, line := range strings.Split(offerSDP, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "a=rtpmap:") && strings.Contains(l, "opus/") {
			return true
		}
	}
	return false
}

// negotiatedAudioPayloadType extracts the payload type bound to the given
// codec mime in a negotiated SDP. The answer's m=audio formats reflect the
// codec the gateway's track bound to (for dynamic types pion preserves the
// remote's PT), so parsing the answer keeps writeTrack correct even if a
// client remaps Opus away from 111.
func negotiatedAudioPayloadType(sdpText, mime string) uint8 {
	mime = strings.ToLower(mime)
	for _, line := range strings.Split(sdpText, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "a=rtpmap:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(l, "a=rtpmap:"))
		if len(fields) != 2 || strings.ToLower(fields[1]) != mime {
			continue
		}
		if pt, err := parsePT(fields[0]); err == nil {
			return pt
		}
	}
	return 0
}

func parsePT(token string) (uint8, error) {
	var pt int
	if _, err := fmt.Sscanf(token, "%d", &pt); err != nil {
		return 0, err
	}
	if pt < 0 || pt > 127 {
		return 0, fmt.Errorf("payload type %d out of range", pt)
	}
	return uint8(pt), nil
}

// setupClientCodec creates the per-session codec machinery for a negotiated
// network hop. Opus gets an encoder (mixer → track), a decoder (track →
// host), the closed-loop controller and the NACK-request budget; PCMU needs
// none of it. The caller owns arrangement of the per-pc API with the same
// controller/budget/observer.
func (s *Session) setupClientCodec(codec string) error {
	s.codecMu.Lock()
	defer s.codecMu.Unlock()
	s.clientCodec = codec
	if codec == codecPCMU {
		s.payloadType = 0
		return nil
	}
	enc, err := opus.NewEncoder()
	if err != nil {
		return fmt.Errorf("opus encoder: %w", err)
	}
	dec, err := opus.NewDecoder()
	if err != nil {
		enc.Close()
		return fmt.Errorf("opus decoder: %w", err)
	}
	budget := newNackBudget()
	controller, err := newBitrateController(enc, &s.codecMu, s.Key)
	if err != nil {
		dec.Close()
		enc.Close()
		return fmt.Errorf("bitrate controller: %w", err)
	}
	s.opusEnc = enc
	s.opusDec = dec
	s.controller = controller
	s.nackBudget = budget
	s.payloadType = defaultOpusPT
	s.codecOwned = true
	// Playout-time decode: the session's jitter buffer decodes Opus at the
	// play cursor (single mixer goroutine) so the decoder only advances in
	// timestamp order. Read the buffer under the session lock: a concurrent
	// WS/ICE handover swaps s.host under s.mu. codecMu -> s.mu matches the
	// lock order writeTrack already uses, so no cycle is introduced.
	s.mu.Lock()
	host := s.host
	s.mu.Unlock()
	host.setDecoder(dec)
	return nil
}

// releaseCodecLocked releases Opus machinery. Caller must hold codecMu;
// nil-safe and idempotent. Ownership (codecOwned) prevents a probe that was
// adopted into a live call from destroying the machinery the live session
// now uses.
func (s *Session) releaseCodecLocked() {
	if s.opusEnc != nil {
		s.opusEnc.Close()
		s.opusEnc = nil
	}
	if s.opusDec != nil {
		s.opusDec.Close()
		s.opusDec = nil
	}
	if s.controller != nil {
		s.controller.Close()
		s.controller = nil
	}
	s.nackBudget = nil
	s.codecOwned = false
	s.clientCodec = ""
}

// encodeClientFrame renders one mixed 8 kHz PCM frame into the wire format
// of the negotiated codec.
func (s *Session) encodeClientFrame(pcm []int16) ([]byte, error) {
	s.codecMu.Lock()
	defer s.codecMu.Unlock()
	if s.clientCodec == codecOpus && s.opusEnc != nil {
		return s.opusEnc.Encode(pcm)
	}
	return voice.EncodePCMU(pcm)
}

// decodeHostFrame renders one inbound network frame into 8 kHz PCM for the
// host jitter buffer.
func (s *Session) decodeHostFrame(codec string, payload []byte) ([]int16, error) {
	if codec == codecOpus {
		s.codecMu.Lock()
		defer s.codecMu.Unlock()
		if s.opusDec == nil {
			return nil, fmt.Errorf("opus frame arrived without a decoder")
		}
		return s.opusDec.Decode(payload)
	}
	return voice.DecodePCMU(payload)
}

func NewSession(engine *Engine, key, kind string) *Session {
	session := &Session{
		Engine: engine, Key: key, Kind: kind,
		legs: make(map[string]*Leg), stop: make(chan struct{}), host: newHostJitter(160),
	}
	go session.loop()
	return session
}

// HostLossCause classifies why a host transport went away. Recoverable
// losses (ICE failure/flap, socket network drop) enter the bounded recovery
// window; AuthRevoked is terminal and bypasses recovery entirely.
type HostLossCause int

const (
	HostLossRecoverable HostLossCause = iota
	HostLossAuthRevoked
)

func (s *Session) SetCallbacks(
	onHostLost func(key string, cause HostLossCause),
	onHostRestored func(key string),
	onLegClosed func(callID string, err error),
) {
	s.mu.Lock()
	s.onHostLost = onHostLost
	s.onHostRestored = onHostRestored
	s.onLegClosed = onLegClosed
	s.mu.Unlock()
}

// SetRecordingTaps installs read-only mixer taps for manual call recording.
// See the field documentation on Session for the exact tap points and
// non-blocking contract. Passing nil removes the taps.
func (s *Session) SetRecordingTaps(uplink, downlink func([]int16)) {
	s.mu.Lock()
	s.recUplink = uplink
	s.recDownlink = downlink
	s.mu.Unlock()
}

// armFreshMediaProof arms the requirement that a re/attached host must prove
// itself with real inbound audio before recovery is considered cleared. The
// next genuine frame (not Opus PLC, not an old connection's RTT) fires the
// restored callback exactly once.
func (s *Session) armFreshMediaProof(generation uint64) {
	s.mu.Lock()
	s.freshPending = true
	s.freshGen = generation
	s.mu.Unlock()
}

// markFreshHostFrame proves the CURRENT transport generation is carrying
// real inbound audio (called from the socket/RTP pump for an actual decoded
// frame, never for Opus PLC slots and never from an old connection's RTT).
// The first real frame for the armed generation fires the restored callback,
// so only fresh media — never attach/CONNECTED alone — clears recovery.
func (s *Session) markFreshHostFrame(generation uint64) {
	s.mu.Lock()
	var cb func(string)
	if s.freshPending && generation == s.freshGen {
		s.freshPending = false
		cb = s.onHostRestored
	}
	key := s.Key
	s.mu.Unlock()
	if cb != nil {
		cb(key)
	}
}

// Standby transport kinds accepted by PromoteStandby. Build 63 added the
// dedicated QUIC slots: "quic" is now a first-class role kind (its own
// primary/standby slots) instead of anonymously occupying the ws-family
// slots, so a MediaState snapshot can never be misread as "standbyWs means
// QUIC" (coordinator build-63 review).
const (
	StandbyKindWS   = "ws"
	StandbyKindICE  = "ice"
	StandbyKindQUIC = "quic"
)

func (s *Session) currentGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// armStandbyFreshMediaProof marks the standby WSS path unproven until its
// first genuine inbound frame. The PRIMARY recovery fencing is untouched: a
// standby attach is not a host replacement.
func (s *Session) armStandbyFreshMediaProof() {
	s.mu.Lock()
	s.wsStandbyFreshAt = time.Time{}
	s.mu.Unlock()
}

// noteStandbyWSFresh records that a genuine media frame arrived on the
// standby WSS transport (readiness proof for the standby path only).
func (s *Session) noteStandbyWSFresh() {
	s.mu.Lock()
	s.wsStandbyFreshAt = time.Now()
	s.mu.Unlock()
}

// StandbyWSFreshAt reports when the standby WSS transport last carried a
// genuine inbound frame (zero = unproven since attach).
func (s *Session) StandbyWSFreshAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wsStandbyFreshAt
}

// StandbyPCFreshAt reports when the standby ICE transport last carried a
// genuine inbound frame (zero = unproven since adoption).
func (s *Session) StandbyPCFreshAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pcStandbyFreshAt
}

// noteStandbyQUICFresh records that a genuine media frame arrived on the
// standby QUIC transport (build 63: the QUIC slot is first-class; its
// freshness clock is separate from the WSS standby's).
func (s *Session) noteStandbyQUICFresh() {
	s.mu.Lock()
	s.quicStandbyFreshAt = time.Now()
	s.mu.Unlock()
}

// StandbyQUICFreshAt reports when the standby QUIC transport last carried a
// genuine inbound frame (zero = unproven since attach).
func (s *Session) StandbyQUICFreshAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quicStandbyFreshAt
}

// newStandbyToken mints an opaque random token binding a standby instance to
// this session (and thereby to its call and authorized owner device).
func newStandbyToken() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is not expected; fall back to a time-unique
		// value so fencing still works within the process lifetime.
		return fmt.Sprintf("t%d-%d", time.Now().UnixNano(), time.Now().Unix())
	}
	return hex.EncodeToString(b[:])
}

// Epoch reports the current PRIMARY role epoch (see the epoch field).
func (s *Session) Epoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// StandbyToken returns the current standby token for the given family ("" if
// none).
func (s *Session) StandbyToken(kind string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case StandbyKindWS:
		return s.wsStandbyToken
	case StandbyKindQUIC:
		return s.quicStandbyToken
	default:
		return s.pcStandbyToken
	}
}

// MediaState is the reconciler view for post-ack-loss client refresh. The
// CURRENT standby identity tokens travel with it so an ack-loss reconcile
// can recover the demoted token exactly — never mixing an old token with a
// new epoch (coordinator review). Build 63: the dedicated QUIC slots make
// every field unambiguous — `primary` is the exact transport kind
// ("ws"|"ice"|"quic") and each standby flag names its family only, so a
// snapshot can never be silently misread as "standbyWs means QUIC".
type MediaState struct {
	Primary          string    `json:"primary"`
	Epoch            uint64    `json:"epoch"`
	StandbyWS        bool      `json:"standbyWs"`
	StandbyICE       bool      `json:"standbyIce"`
	StandbyQUIC      bool      `json:"standbyQuic"`
	StandbyWSToken   string    `json:"standbyWsToken,omitempty"`
	StandbyICEToken  string    `json:"standbyIceToken,omitempty"`
	StandbyQUICToken string    `json:"standbyQuicToken,omitempty"`
	WSFreshAt        time.Time `json:"wsFreshAt,omitempty"`
	PCFreshAt        time.Time `json:"pcFreshAt,omitempty"`
	QUICFreshAt      time.Time `json:"quicFreshAt,omitempty"`
}

// State snapshots the transport roles for the state endpoint.
func (s *Session) State() MediaState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := MediaState{Epoch: s.epoch, WSFreshAt: s.wsStandbyFreshAt, PCFreshAt: s.pcStandbyFreshAt, QUICFreshAt: s.quicStandbyFreshAt}
	switch {
	case s.quic != nil:
		st.Primary = StandbyKindQUIC
	case s.ws != nil:
		st.Primary = StandbyKindWS
	case s.pc != nil:
		st.Primary = StandbyKindICE
	}
	st.StandbyWS = s.wsStandby != nil
	st.StandbyICE = s.pcStandby != nil
	st.StandbyQUIC = s.quicStandby != nil
	st.StandbyWSToken = s.wsStandbyToken
	st.StandbyICEToken = s.pcStandbyToken
	st.StandbyQUICToken = s.quicStandbyToken
	return st
}

// checkPromoteNonce implements idempotent retries. A replayed nonce returns
// the recorded outcome with replay=true ONLY when the request's binding
// (kind/token/expectEpoch) matches what was recorded — a nonce is scoped to
// the exact command it executed, never a wildcard for "some promote". The
// returned outcome's CurrentEpoch is refreshed to NOW so the caller can spot
// an intervening swap. A new nonce returns ok=false and reserves nothing
// (the caller records after executing). Expired entries are reaped lazily
// and the map is capped at promoteNonceLimit (oldest evicted).
func (s *Session) checkPromoteNonce(nonce, kind, token string, expectEpoch uint64, now time.Time) (PromoteOutcome, bool, bool) {
	if s.promoteNonces == nil {
		s.promoteNonces = map[string]promoteNonceEntry{}
	}
	for k, v := range s.promoteNonces {
		if now.Sub(v.at) > promoteNonceTTL {
			delete(s.promoteNonces, k)
		}
	}
	entry, ok := s.promoteNonces[nonce]
	if !ok {
		return PromoteOutcome{}, false, false
	}
	if entry.kind != kind || entry.token != token || entry.expectEpoch != expectEpoch {
		// Same nonce, DIFFERENT command: never execute it (a nonce is scoped
		// to the exact command it carried; reuse signals a client or replay
		// fault). The original entry stays intact for a genuine retry.
		return PromoteOutcome{}, false, true
	}
	entry.outcome.CurrentEpoch = s.epoch
	return entry.outcome, true, false
}

// recordPromoteNonce stores an executed outcome — bound to the exact command
// (kind/token/expectEpoch) — for idempotent replay.
func (s *Session) recordPromoteNonce(nonce, kind, token string, expectEpoch uint64, outcome PromoteOutcome, now time.Time) {
	if s.promoteNonces == nil {
		s.promoteNonces = map[string]promoteNonceEntry{}
	}
	for len(s.promoteNonces) >= promoteNonceLimit {
		oldestKey := ""
		var oldestOrder uint64
		first := true
		for k, v := range s.promoteNonces {
			if first || v.order < oldestOrder {
				oldestKey, oldestOrder, first = k, v.order, false
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.promoteNonces, oldestKey)
	}
	s.promoteNonceSeq++
	s.promoteNonces[nonce] = promoteNonceEntry{
		outcome: outcome, kind: kind, token: token, expectEpoch: expectEpoch,
		at: now, order: s.promoteNonceSeq,
	}
}

// pushPCFrame routes one inbound peer-connection frame by LIVE slot
// identity: the current primary's frames enter the host mix (and feed the
// fresh-media proof); a standby pc's frames are timestamped as standby
// freshness and discarded; a replaced/closed pc's frames are dropped. This
// complements captured-generation fencing for the read loop: a hot-standby
// promotion changes a pc's ROLE without invalidating the transport, so the
// role is read live per frame.
func (s *Session) pushPCFrame(pc *pion.PeerConnection, timestamp uint32, pcm []int16, payload []byte, encoded bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.pcStandby == pc {
		s.pcStandbyFreshAt = time.Now()
		s.mu.Unlock()
		return
	}
	if s.pc != pc {
		s.mu.Unlock()
		return
	}
	host := s.host
	generation := s.generation
	s.mu.Unlock()
	if host.behindPlay(timestamp) {
		return
	}
	if encoded {
		host.pushEncoded(timestamp, payload)
	} else {
		host.pushPCM(timestamp, pcm)
	}
	s.hostFrameArrived(generation)
}

// SetDenoise applies the device's directional denoise policy to this owned
// session. Uplink state is session-scoped (survives ICE<->WS handover,
// destroyed at Close); the downlink gate is pushed to every leg (state lives
// on the leg and survives handover/conference split). Toggles are gate-only
// so the Speex noise estimate is preserved; allocation failures pass raw.
func (s *Session) SetDenoise(uplink, downlink bool) {
	// Allocation happens outside s.mu; the pointer is published under s.mu so
	// the mixer snapshot and AddLeg see a consistent policy.
	s.mu.Lock()
	needAlloc := uplink && s.uplinkPP == nil
	legs := make([]*Leg, 0, len(s.legs))
	for _, leg := range s.legs {
		legs = append(legs, leg)
	}
	s.mu.Unlock()
	var pp *speexpp.Preprocessor
	if needAlloc {
		created, err := speexpp.New(true)
		if err != nil {
			slog.Warn("denoise uplink preprocessor unavailable; raw pass-through",
				"session", s.Key, "error", err)
			uplink = false
		} else {
			pp = created
		}
	}
	s.mu.Lock()
	if pp != nil {
		if s.uplinkPP == nil {
			s.uplinkPP = pp
		} else {
			pp.Close()
		}
	}
	s.uplinkDenoise = uplink
	s.downlinkDenoise = downlink
	upPP := s.uplinkPP
	s.mu.Unlock()
	if upPP != nil {
		upPP.SetEnabled(uplink)
	}
	for _, leg := range legs {
		leg.SetDownlinkDenoise(downlink)
	}
}

// currentDenoise reports the live directional gates (s.mu snapshot).
func (s *Session) currentDenoise() (uplink, downlink bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uplinkDenoise, s.downlinkDenoise
}

// processUplink applies the session's host-direction preprocessor once to
// the mic frame before mix. Raw pass-through on any non-enabled/error state.
func (s *Session) processUplink(frame []int16) []int16 {
	s.mu.Lock()
	enabled := s.uplinkDenoise
	var pp *speexpp.Preprocessor
	// A just-disabled gate keeps the PP allocated (state preserved) but must
	// return the raw slice untouched; a never-enabled gate has no PP at all.
	if enabled {
		pp = s.uplinkPP
	}
	s.mu.Unlock()
	if !enabled || pp == nil || len(frame) != speexpp.FrameSamples {
		return frame
	}
	start := time.Now()
	out := pp.Process(frame)
	elapsed := time.Since(start).Nanoseconds()
	s.denoiseProcNanos.Add(elapsed)
	s.denoiseWindowNanos.Add(elapsed)
	s.denoiseWindowFrames.Add(1)
	return out
}

func (s *Session) closeUplinkDenoise() {
	s.mu.Lock()
	pp := s.uplinkPP
	s.uplinkPP = nil
	s.mu.Unlock()
	if pp != nil {
		pp.Close()
	}
}

// DenoiseStats returns uplink enablement, whether any leg downlink is
// enabled and cumulative processing nanoseconds (privacy-safe scalars).
func (s *Session) DenoiseStats() (up bool, down bool, upNanos int64, downNanos int64) {
	s.mu.Lock()
	up = s.uplinkDenoise && s.uplinkPP != nil
	legs := make([]*Leg, 0, len(s.legs))
	for _, leg := range s.legs {
		legs = append(legs, leg)
	}
	s.mu.Unlock()
	upNanos = s.denoiseProcNanos.Load()
	for _, leg := range legs {
		en, nanos := leg.denoiseStats()
		if en {
			down = true
		}
		downNanos += nanos
	}
	return up, down, upNanos, downNanos
}

// DenoiseStatsWindow consumes (resets) the per-frame window counters for the
// periodic evidence line: returns per-frame mean microseconds for the window
// alongside the cumulative totals from DenoiseStats. Queue depth/levels come
// from the adaptive stats; RTT and mouth-to-ear latency are SEPARATE
// measurements (route telemetry and physical timing respectively) and are
// never implied by these compute numbers.
func (s *Session) DenoiseStatsWindow() (upPerFrameUs, downPerFrameUs float64) {
	upNanos := s.denoiseWindowNanos.Swap(0)
	upFrames := s.denoiseWindowFrames.Swap(0)
	s.mu.Lock()
	legs := make([]*Leg, 0, len(s.legs))
	for _, leg := range s.legs {
		legs = append(legs, leg)
	}
	s.mu.Unlock()
	var downNanos, downFrames int64
	for _, leg := range legs {
		_, nanos, frames := leg.denoiseStatsWindow()
		downNanos += nanos
		downFrames += frames
	}
	if upFrames > 0 {
		upPerFrameUs = float64(upNanos) / float64(upFrames) / 1000.0
	}
	if downFrames > 0 {
		downPerFrameUs = float64(downNanos) / float64(downFrames) / 1000.0
	}
	return upPerFrameUs, downPerFrameUs
}

// applyHostState drives the host lifecycle from a peer connection's state
// transitions. The fence is the pc's LIVE PRIMARY identity: a standby pc
// (build-50 hot standby) or a replaced pc must never set hostPresent, open a
// recovery episode, or clear one — its state changes are observed by the
// client, not by the mix.
func (s *Session) applyHostState(pc *pion.PeerConnection, state pion.PeerConnectionState) {
	s.mu.Lock()
	if s.closed || s.pc != pc {
		s.mu.Unlock()
		return
	}
	var lost func(string, HostLossCause)
	switch state {
	case pion.PeerConnectionStateConnected:
		s.hostPresent = true
		// Recovery is not cleared on CONNECTED alone: arm fresh-media proof
		// so the first real inbound frame on this generation restores.
		s.freshPending = true
		s.freshGen = s.generation
	case pion.PeerConnectionStateDisconnected, pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed:
		// ICE failure/flap/closure is recoverable within the bounded window
		// (build 48: a .failed state no longer ends the call immediately).
		s.hostPresent = false
		s.freshPending = false
		s.host.clear()
		for _, leg := range s.legs {
			leg.Drain()
		}
		lost = s.onHostLost
	}
	s.mu.Unlock()
	if lost != nil {
		lost(s.Key, HostLossRecoverable)
	}
}

// hostFrameArrived is called from the inbound pumps for every GENUINE
// decoded frame (not an Opus PLC slot, not a duplicate). It feeds the
// fresh-media recovery proof for the current transport generation.
func (s *Session) hostFrameArrived(generation uint64) {
	s.markFreshHostFrame(generation)
}

func (s *Session) hostPush(generation uint64, timestamp uint32, pcm []int16) {
	s.mu.Lock()
	valid := !s.closed && generation == s.generation
	host := s.host
	s.mu.Unlock()
	// Playout-deadline bound (same rule the WSS path applies at the socket):
	// a frame whose RTP timestamp already fell behind the play cursor — a
	// late NACK retransmission — can never be played; admitting it would
	// only displace fresh audio from the bounded jitter buffer. Duplicates
	// of timestamps still buffered are rejected idempotently inside push.
	if valid && !host.behindPlay(timestamp) {
		host.pushPCM(timestamp, pcm)
	}
}

// hostPushEncoded buffers one wire-encoded Opus frame for playout-time
// decode. The encoded stage preserves the depacketizer discipline: the
// playout deadline and duplicate rejection run BEFORE the stateful decoder
// ever sees the payload, so reordered, duplicate or late NACK retransmission
// packets cannot corrupt decoder state.
func (s *Session) hostPushEncoded(generation uint64, timestamp uint32, payload []byte) {
	s.mu.Lock()
	valid := !s.closed && generation == s.generation
	host := s.host
	s.mu.Unlock()
	if valid && !host.behindPlay(timestamp) {
		host.pushEncoded(timestamp, payload)
	}
}

// ClientAttach is the result of a client media negotiation.
type ClientAttach struct {
	// SDP is the answer (full candidate set for a full-SDP attach; the
	// candidates gathered so far for a trickle attach).
	SDP string
	// AttachToken pins this exact attachment for later incremental candidate
	// traffic in BOTH directions ("" for a full-SDP attach).
	AttachToken string
	// CandidateCursor is how many queued gateway candidates are already
	// present in SDP: the client starts polling after this cursor so no
	// candidate is delivered twice and none is lost.
	CandidateCursor int
	// GatheringComplete reports whether the gateway finished gathering
	// before the answer was written. Always true for a full-SDP attach.
	GatheringComplete bool
}

// AttachClient accepts the full (non-trickle) SDP offer from the iOS client
// and returns the answer after ICE gathering completes. Any previous peer is
// closed outside the session lock so its callbacks can never deadlock.
func (s *Session) AttachClient(ctx context.Context, offerSDP string, iceServers []pion.ICEServer) (string, error) {
	attach, err := s.AttachClientTrickle(ctx, offerSDP, iceServers, false)
	return attach.SDP, err
}

// AttachClientTrickle performs the same negotiation for a client that
// advertised the incremental (trickle) capability. With trickle=true the
// answer is returned as soon as the local description exists — WITHOUT
// waiting for the gateway's own ICE gathering — and the gateway's candidates
// gathered afterwards are delivered through the bounded, attach-fenced queue
// (LocalCandidates) on the same authenticated JSON plane. With
// trickle=false the old full-SDP behavior is byte-for-byte preserved: the
// caller waits for gathering and receives a complete answer with no token.
func (s *Session) AttachClientTrickle(ctx context.Context, offerSDP string, iceServers []pion.ICEServer, trickle bool) (ClientAttach, error) {
	return s.attachClient(ctx, offerSDP, iceServers, trickle)
}

func (s *Session) attachClient(ctx context.Context, offerSDP string, iceServers []pion.ICEServer, trickle bool) (ClientAttach, error) {
	// Negotiate the network-hop codec from the offer itself: Opus when the
	// client offers it (new app), PCMU otherwise (build ≤25 forces
	// PCMU-only). The modem leg and WSS host path are untouched either way.
	codec := codecPCMU
	if offerPrefersOpus(offerSDP) {
		codec = codecOpus
	}
	hostTick := uint32(160)
	if codec == codecOpus {
		hostTick = opusFrameTick
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ClientAttach{}, fmt.Errorf("session is closed")
	}
	s.generation++
	generation := s.generation
	previous := s.pc
	s.pc = nil
	s.track = nil
	// A fresh attachment owns a fresh (empty) candidate stream; the previous
	// one keeps serving any poll that still names ITS token.
	stream := &gatherStream{}
	s.gatherQueue = stream
	// A new attach invalidates the previous attach's candidate token
	// immediately: a late candidate batch from the replaced negotiation can
	// never be applied to the new peer connection. The gateway-side candidate
	// queue belongs to the attach too and starts empty.
	s.attachToken = ""
	s.attachTrickle = false
	s.attachCandidates = 0
	previousWS := s.ws
	previousQUIC := s.quic
	// Build 63: a displaced WSS/QUIC primary demotes into its own family's
	// standby slot when free (same contract as AdoptClientPeer), so an ICE
	// attach never silently retires the relay or the QUIC path.
	if previousWS != nil && s.wsStandby == nil {
		s.wsStandby = previousWS
		s.wsStandbyToken = newStandbyToken()
		s.wsStandbyFreshAt = time.Time{}
		previousWS = nil
	}
	if previousQUIC != nil && s.quicStandby == nil {
		s.quicStandby = previousQUIC
		s.quicStandbyToken = newStandbyToken()
		s.quicStandbyFreshAt = time.Time{}
		previousQUIC = nil
	}
	s.ws = nil
	s.quic = nil
	s.hostPresent = false
	s.host = newHostJitter(hostTick)
	for _, leg := range s.legs {
		leg.Drain()
	}
	s.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	if previousWS != nil {
		previousWS.Close()
	}
	if previousQUIC != nil {
		previousQUIC.Close()
	}
	s.codecMu.Lock()
	if s.codecOwned {
		s.releaseCodecLocked()
	}
	s.codecMu.Unlock()

	if err := s.setupClientCodec(codec); err != nil {
		return ClientAttach{}, err
	}
	failCodec := func(err error) (ClientAttach, error) {
		s.codecMu.Lock()
		s.releaseCodecLocked()
		s.codecMu.Unlock()
		return ClientAttach{}, err
	}
	observer := newRRObserver()
	s.codecMu.Lock()
	controller, budget := s.controller, s.nackBudget
	s.codecMu.Unlock()
	observer.onWindow = func(fractionLost float64) {
		if controller != nil {
			controller.onLossWindow(fractionLost)
		}
	}
	api, err := s.Engine.newAPI(controller, budget, observer)
	if err != nil {
		return failCodec(err)
	}

	// pcStop is closed when this attachment's peer connection is closed, so
	// the gathering-completion watcher above can never outlive its pc.
	pcStop := make(chan struct{})
	pc, err := api.NewPeerConnection(pion.Configuration{ICEServers: iceServers})
	if err != nil {
		return failCodec(fmt.Errorf("create peer connection: %w", err))
	}
	trackCodec := pion.RTPCodecCapability{MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1}
	if codec == codecOpus {
		trackCodec = pion.RTPCodecCapability{MimeType: opusMime, ClockRate: opusClockRate, Channels: 2}
	}
	track, err := pion.NewTrackLocalStaticRTP(trackCodec, "audio", "callrelay")
	if err != nil {
		_ = pc.Close()
		return failCodec(err)
	}
	if _, err := pc.AddTransceiverFromTrack(track, pion.RTPTransceiverInit{Direction: pion.RTPTransceiverDirectionSendrecv}); err != nil {
		_ = pc.Close()
		return failCodec(err)
	}
	// The client opens the callrelay-probe channel in its offer; answer it
	// with a bounded echo so app-level RTT is measurable without streaming
	// microphone audio. The handler lives on the pc, so it keeps working
	// when a probe peer is adopted onto the live session.
	pc.OnDataChannel(func(channel *pion.DataChannel) {
		if channel.Label() == ProbeEchoLabel {
			installProbeEcho(channel)
		}
	})
	pc.OnTrack(func(remote *pion.TrackRemote, receiver *pion.RTPReceiver) {
		// pion MIME types are canonical (audio/PCMU, audio/opus): compare
		// directly — lowercasing here silently rejected every PCMU track.
		mime := remote.Codec().MimeType
		if mime != pion.MimeTypePCMU && mime != opusMime {
			return
		}
		remoteCodec := codecPCMU
		if mime == opusMime {
			remoteCodec = codecOpus
		}
		// Drive the RTCP interceptor chain (pion only pumps it when the
		// app reads): NACK generation, SR/RR, the congestion-feedback
		// observer and the GCC estimator all live behind receiver.ReadRTCP.
		go func() {
			for {
				if _, _, err := receiver.ReadRTCP(); err != nil {
					return
				}
			}
		}()
		for {
			packet, _, readErr := remote.ReadRTP()
			if readErr != nil {
				return
			}
			// Depacketizer discipline (build 27): PCMU payloads are decoded
			// inline (the PCMU decoder is stateless), but Opus payloads are
			// buffered ENCODED and decoded only at the playout cursor inside
			// the jitter buffer — reordering, duplicates and late NACK
			// retransmissions are rejected by timestamp/deadline BEFORE the
			// stateful Opus decoder can advance out of order, and missing
			// slots get FEC/PLC at the playout deadline.
			if remoteCodec == codecPCMU {
				if len(packet.Payload) != frameBytes {
					continue
				}
				pcm, decodeErr := voice.DecodePCMU(packet.Payload)
				if decodeErr != nil {
					continue
				}
				// Adoption retargets frames through s.route without
				// re-registering this handler (OnTrack only fires at track
				// arrival, so a handler swapped in later would never run).
				// Frames route by LIVE primary/standby identity on the owning
				// session: a hot-standby promotion changes the pc's role, not
				// its validity, so captured-generation fencing cannot decide it.
				owner := s
				if target := s.route.Load(); target != nil {
					owner = target.session
				}
				owner.pushPCFrame(pc, packet.Timestamp, pcm, nil, false)
				continue
			}
			owner := s
			if target := s.route.Load(); target != nil {
				owner = target.session
			}
			owner.pushPCFrame(pc, packet.Timestamp, nil, packet.Payload, true)
		}
	})
	var stopOnce sync.Once
	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		// Bounded lifecycle evidence per peer (fires only on meaningful
		// transitions): correlates a direct probe's real reachability with
		// the gathered candidate census in the daemon journal.
		switch state {
		case pion.PeerConnectionStateConnected,
			pion.PeerConnectionStateDisconnected,
			pion.PeerConnectionStateFailed,
			pion.PeerConnectionStateClosed:
			slog.Info("media peer state", "session", s.Key, "state", state.String())
		}
		if state == pion.PeerConnectionStateClosed {
			// The attachment's life ended: release the gathering watcher (and
			// let a poll see completion instead of waiting forever).
			stopOnce.Do(func() { close(pcStop) })
		}
		s.applyHostState(pc, state)
	})

	// Incremental delivery: register the candidate collector BEFORE the local
	// description exists so no gathered candidate can be missed. A full-SDP
	// attach does not need the queue (its SDP carries everything).
	if trickle {
		// The callbacks capture THIS attachment's stream (and its peer
		// connection), so a callback that fires after the peer was replaced
		// can only ever touch its own stream.
		pc.OnICECandidate(func(candidate *pion.ICECandidate) {
			// A replaced peer connection must not keep feeding the stream
			// either: the stream is closed to further appends once the owning
			// pc is no longer the session's current one.
			s.mu.Lock()
			owner := s.pc == pc
			s.mu.Unlock()
			if !owner {
				return
			}
			if candidate == nil {
				stream.markComplete()
				return
			}
			line := candidate.ToJSON().Candidate
			if strings.TrimSpace(line) == "" {
				return
			}
			stream.append(line)
		})
	}
	if err := pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: offerSDP}); err != nil {
		_ = pc.Close()
		return ClientAttach{}, fmt.Errorf("set remote description: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = pc.Close()
		return ClientAttach{}, fmt.Errorf("create answer: %w", err)
	}
	gatherComplete := pion.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		_ = pc.Close()
		return ClientAttach{}, fmt.Errorf("set local description: %w", err)
	}
	if !trickle {
		select {
		case <-gatherComplete:
		case <-ctx.Done():
			_ = pc.Close()
			return ClientAttach{}, ctx.Err()
		}
	} else {
		// The client polls the remaining gateway candidates from the bounded
		// queue; this goroutine only records REAL completion. It is bounded by
		// the peer connection's own life: a replaced/closed peer resolves the
		// promise.
		go func() {
			// Bounded by the peer connection's own life: gathering completes
			// when the pc closes too, and an explicit close stops this
			// goroutine from outliving its attachment.
			select {
			case <-gatherComplete:
				stream.markComplete()
			case <-pcStop:
			}
		}()
	}
	local := pc.LocalDescription()
	if local == nil {
		_ = pc.Close()
		return ClientAttach{}, fmt.Errorf("missing local description")
	}
	udp, tcp, host, srflx, relay := countLocalCandidates(local.SDP)
	slog.Info("media candidates gathered", "session", s.Key, "trickle", trickle,
		"udp", udp, "tcp", tcp, "host", host, "srflx", srflx, "relay", relay)

	token := ""
	if trickle {
		token = newStandbyToken()
	}
	s.mu.Lock()
	if s.closed || s.generation != generation {
		s.mu.Unlock()
		_ = pc.Close()
		s.codecMu.Lock()
		if s.codecOwned {
			s.releaseCodecLocked()
		}
		s.codecMu.Unlock()
		return ClientAttach{}, fmt.Errorf("session was replaced")
	}
	s.pc = pc
	s.track = track
	s.attachToken = token
	s.attachTrickle = trickle
	s.attachCandidates = 0
	s.mu.Unlock()
	attach := ClientAttach{SDP: local.SDP, AttachToken: token, GatheringComplete: !trickle}
	if trickle {
		// Candidates already present in the answer SDP must not be delivered
		// twice: the client resumes the queue exactly after them.
		attach.CandidateCursor = stream.cursorForSDP(local.SDP)
	}
	// Capture the negotiated payload type from the answer (dynamic PTs can
	// be remapped by the client; pion preserves the remote's mapping).
	if codec == codecOpus {
		if pt := negotiatedAudioPayloadType(local.SDP, opusMime); pt != 0 {
			s.codecMu.Lock()
			s.payloadType = pt
			s.codecMu.Unlock()
		}
	}
	return attach, nil
}

// ErrAttachStale reports that a trickle candidate batch belongs to a peer
// connection that has already been replaced (or to a non-trickle attach).
var ErrAttachStale = errors.New("attach superseded")

// Trickle candidate bounds: a client only needs a handful of candidates per
// attach, and every batch is size- and rate-bounded so a misbehaving or
// hostile authenticated peer cannot grow the ICE agent without limit.
const (
	trickleMaxBatch   = 8
	trickleMaxTotal   = 32
	trickleMaxLineLen = 512
)

// AddClientCandidates applies one incremental candidate batch from a client
// that negotiated AttachClientTrickle. The token pins the exact attachment:
// a batch that arrives after a newer attach (or on a full-SDP attach) is
// rejected as stale and changes nothing. Individual malformed/oversized
// lines are skipped, never fatal, and the batch/total caps bound the work.
func (s *Session) AddClientCandidates(token string, candidates []string) (int, error) {
	if strings.TrimSpace(token) == "" {
		return 0, ErrAttachStale
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrAttachStale
	}
	if !s.attachTrickle || s.attachToken == "" ||
		subtle.ConstantTimeCompare([]byte(token), []byte(s.attachToken)) != 1 {
		s.mu.Unlock()
		return 0, ErrAttachStale
	}
	pc := s.pc
	if pc == nil {
		s.mu.Unlock()
		return 0, nil
	}
	// Per-batch cap first (one request can never flood the ICE agent).
	if len(candidates) > trickleMaxBatch {
		candidates = candidates[:trickleMaxBatch]
	}
	// RESERVE the whole batch against the per-attach budget while still under
	// the lock: two concurrent batches can no longer both read the same
	// "remaining" and together exceed the promised total. The reservation
	// counts attempted lines (a malformed line consumes budget: it is still
	// bounded work), and it is dropped entirely when the batch is empty.
	if len(candidates) == 0 {
		s.mu.Unlock()
		return 0, nil
	}
	if remaining := trickleMaxTotal - s.attachCandidates; remaining <= 0 {
		s.mu.Unlock()
		return 0, nil
	} else if len(candidates) > remaining {
		candidates = candidates[:remaining]
	}
	s.attachCandidates += len(candidates)
	s.mu.Unlock()
	// AddICECandidate is called OUTSIDE the session lock: pion may invoke
	// connection-state callbacks synchronously on some paths, and those take
	// the session lock (applyHostState). The reservation above is what keeps
	// the bound honest without holding the lock across the pion call.
	added := 0
	for _, raw := range candidates {
		line := strings.TrimSpace(raw)
		if line == "" || len(line) > trickleMaxLineLen {
			continue
		}
		if !strings.HasPrefix(line, "candidate:") {
			continue
		}
		if err := pc.AddICECandidate(pion.ICECandidateInit{Candidate: line}); err != nil {
			// A malformed candidate is skipped; its reservation still counts
			// toward the bound, so a client cannot spin on it.
			continue
		}
		added++
	}
	return added, nil
}

// gatherStream is ONE attachment's incremental candidate queue. It is
// created per attach and captured by that attach's ICE callbacks, so its
// lifetime is exactly the attachment's.
type gatherStream struct {
	mu        sync.Mutex
	items     []string
	complete  bool
	truncated bool
	lastPoll  time.Time
}

// CandidateBatch is one poll answer for the gateway-side incremental
// candidate stream of one attachment.
type CandidateBatch struct {
	// Candidates are the newly gathered gateway candidates after the caller's
	// cursor, in gathering order.
	Candidates []string `json:"candidates"`
	// Cursor is the caller's next cursor (feed it back).
	Cursor int `json:"cursor"`
	// Complete is true once gathering finished AND every queued candidate has
	// been handed out.
	Complete bool `json:"complete"`
	// Truncated is true when the bounded queue overflowed: the client must
	// keep using the candidates it already has (never an unbounded stream).
	Truncated bool `json:"truncated,omitempty"`
}

// gatherQueueLimit bounds the gateway-side incremental candidate stream: a
// real peer connection gathers a handful of candidates; 64 is far above any
// legitimate set and keeps one attachment's memory/queue work bounded.
const gatherQueueLimit = 64

// gatherPollMinInterval throttles polling on one attachment: a client that
// hammers the endpoint gets a single retryable answer, not a request loop.
const gatherPollMinInterval = 100 * time.Millisecond

func (g *gatherStream) append(line string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.items) >= gatherQueueLimit {
		g.truncated = true
		return
	}
	g.items = append(g.items, line)
}

func (g *gatherStream) markComplete() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.complete = true
	g.mu.Unlock()
}

// cursorForSDP reports how many queued candidates are already present in the
// given SDP, so the client's poll resumes exactly after them.
func (g *gatherStream) cursorForSDP(sdp string) int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cursor := 0
	for _, candidate := range g.items {
		if !strings.Contains(sdp, candidate) {
			break
		}
		cursor++
	}
	return cursor
}

// drain hands out every candidate after the caller's cursor and reports real
// completion. Polling is rate-limited per attachment.
func (g *gatherStream) drain(after int) (CandidateBatch, error) {
	if g == nil {
		return CandidateBatch{}, ErrAttachStale
	}
	if after < 0 {
		after = 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if !g.lastPoll.IsZero() && now.Sub(g.lastPoll) < gatherPollMinInterval {
		return CandidateBatch{}, ErrAttachPollThrottled
	}
	g.lastPoll = now
	if after > len(g.items) {
		after = len(g.items)
	}
	return CandidateBatch{
		Candidates: append([]string(nil), g.items[after:]...),
		Cursor:     len(g.items),
		Complete:   g.complete,
		Truncated:  g.truncated,
	}, nil
}

// LocalCandidates hands out the gateway's gathered candidates for the
// attachment the token pins. The token AND the queue are validated together
// under the session lock, so a reattach between the check and the read is
// impossible: the poll always drains the queue of the attach it named.
func (s *Session) LocalCandidates(token string, after int) (CandidateBatch, error) {
	if strings.TrimSpace(token) == "" {
		return CandidateBatch{}, ErrAttachStale
	}
	s.mu.Lock()
	if s.closed || !s.attachTrickle || s.attachToken == "" ||
		subtle.ConstantTimeCompare([]byte(token), []byte(s.attachToken)) != 1 {
		s.mu.Unlock()
		return CandidateBatch{}, ErrAttachStale
	}
	stream := s.gatherQueue
	s.mu.Unlock()
	return stream.drain(after)
}

// ErrAttachPollThrottled reports that the same attachment was polled faster
// than the bounded interval allows.
var ErrAttachPollThrottled = errors.New("candidate poll throttled")

// AttachToken reports the current trickle attach token ("" for a full-SDP
// attach or no attachment), for evidence/diagnostics only.
func (s *Session) AttachToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attachToken
}

func (s *Session) HostConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hostPresent
}

// HostKind reports which client transport the session currently hosts:
// "ice" (peer connection), "ws" (authenticated socket), "quic" (QUIC
// DATAGRAM direct) or "" (none yet). It is the ground truth behind the
// CallView mediaTransport field so the app never claims direct while the
// gateway is still relaying.
func (s *Session) HostKind() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quic != nil {
		return "quic"
	}
	if s.ws != nil {
		return "ws"
	}
	if s.pc != nil {
		return "ice"
	}
	return ""
}

// PromoteStandby atomically promotes the standby transport of the given
// kind to the primary role (build-50 hot standby), DEMOTING the current
// primary into the standby slot. Both transports stay attached — nothing is
// opened or closed on this path, which is what makes the handover
// make-before-break: the promoted transport has already been carrying the
// downlink mix and its freshness is proven, and the demoted transport keeps
// receiving the same mix as the new standby.
//
// Fenced per coordinator rev 2: the command MUST pin the exact standby
// instance (token — a replaced/renewed standby no longer matches), the
// caller's view of the primary role (expectEpoch — a racing swap changes the
// epoch and is rejected so late HTTP/WS acks and rapid manual swaps can
// never promote a wrong/replacement standby or swap server/client
// inconsistently), and a nonce giving idempotent retries (a replayed nonce
// returns the recorded outcome instead of re-executing).
//
// Effects under the session lock: epoch + generation bumps (fresh-media
// proof armed for the promoted transport; the first genuine inbound frame
// clears any open recovery episode), a FRESH host jitter buffer for the new
// primary (the stale client-uplink queue is discarded — the client also
// discards its standby queue at promotion), hostPresent=true. The modem legs
// are NOT drained (coordinator review): they carry the caller's downlink,
// which must stay continuous across the swap — the mixer keeps writing live
// leg frames to both transports every tick. The demoted transport's inbound
// frames stop feeding the mix from the next frame (the single-uplink
// invariant holds before, during and after the swap); its read loop keeps
// running and its frames are freshness-counted as standby.
func (s *Session) PromoteStandby(kind, token string, expectEpoch uint64, nonce string) (PromoteOutcome, error) {
	now := time.Now()
	if nonce == "" {
		// The protocol makes nonce mandatory: without it a retried command
		// after an ack loss could not be distinguished from a new one.
		return PromoteOutcome{}, fmt.Errorf("missing nonce")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return PromoteOutcome{}, fmt.Errorf("session is closed")
	}
	if outcome, replay, nonceConflict := s.checkPromoteNonce(nonce, kind, token, expectEpoch, now); replay || nonceConflict {
		s.mu.Unlock()
		if nonceConflict {
			return PromoteOutcome{}, fmt.Errorf("nonce reuse with a different command")
		}
		return outcome, nil
	}
	var wantToken string
	switch kind {
	case StandbyKindWS:
		wantToken = s.wsStandbyToken
	case StandbyKindICE:
		wantToken = s.pcStandbyToken
	case StandbyKindQUIC:
		wantToken = s.quicStandbyToken
	default:
		s.mu.Unlock()
		return PromoteOutcome{}, fmt.Errorf("unknown standby kind %q", kind)
	}
	if wantToken == "" || token != wantToken {
		s.mu.Unlock()
		return PromoteOutcome{}, fmt.Errorf("standby token mismatch")
	}
	if s.epoch != expectEpoch {
		s.mu.Unlock()
		return PromoteOutcome{}, fmt.Errorf("role epoch changed: have %d, expect %d", s.epoch, expectEpoch)
	}
	// Validate the entire promotion shape BEFORE any state mutation. A
	// failed promotion must preserve the source host untouched: an epoch or
	// generation bump on a rejected command would fence the live primary's
	// fresh-media callbacks and could strand a healthy call.
	if kind == StandbyKindWS && s.wsStandby == nil {
		s.mu.Unlock()
		return PromoteOutcome{}, fmt.Errorf("no websocket standby to promote")
	}
	if kind == StandbyKindQUIC {
		if s.quicStandby == nil {
			s.mu.Unlock()
			return PromoteOutcome{}, fmt.Errorf("no quic standby to promote")
		}
		if s.quic != nil {
			// Exactly one QUIC primary per session, mirroring the ICE
			// shape: promoting a QUIC standby while QUIC is already primary
			// is a faulty-client swap, not a silent demote.
			s.mu.Unlock()
			return PromoteOutcome{}, fmt.Errorf("primary is already quic; unsupported standby swap")
		}
	}
	if kind == StandbyKindICE {
		if s.pcStandby == nil {
			s.mu.Unlock()
			return PromoteOutcome{}, fmt.Errorf("no ice standby to promote")
		}
		if s.pc != nil {
			// The contract carries exactly ONE direct transport per session
			// (the client's single direct handle). Promoting an ICE standby
			// while an ICE peer is primary is an unsupported swap shape, not
			// a silent demote: reject it loudly so a faulty client cannot
			// strand two live peers.
			s.mu.Unlock()
			return PromoteOutcome{}, fmt.Errorf("primary is already ice; unsupported standby swap")
		}
	}
	s.generation++
	generation := s.generation
	s.epoch++
	outcome := PromoteOutcome{Epoch: s.epoch, CurrentEpoch: s.epoch, Primary: kind}
	wireDecoder := false
	// Demote the CURRENT primary into ITS OWN family's standby slot (build
	// 63: dedicated QUIC slots make every role typed). One standby slot per
	// family: when that slot still holds a transport of the same family (a
	// same-family primary+standby pair), the displaced standby is collected
	// for closure after the swap — its token dies with it and the freshly
	// demoted primary takes the slot. Closures happen outside s.mu.
	var displaced []*wsHost
	var displacedPC *pion.PeerConnection
	demotePrimaryLocked := func() {
		switch {
		case s.pc != nil:
			if s.pcStandby != nil {
				displacedPC = s.pcStandby
			}
			s.pcStandby = s.pc
			s.trackStandby = s.track
			s.pcStandbyToken = newStandbyToken()
			s.pcStandbyFreshAt = time.Time{}
			s.pc = nil
			s.track = nil
			outcome.Standby = StandbyKindICE
			outcome.StandbyToken = s.pcStandbyToken
		case s.quic != nil:
			if s.quicStandby != nil {
				displaced = append(displaced, s.quicStandby)
			}
			s.quicStandby = s.quic
			s.quicStandbyToken = newStandbyToken()
			s.quicStandbyFreshAt = time.Time{}
			s.quic = nil
			outcome.Standby = StandbyKindQUIC
			outcome.StandbyToken = s.quicStandbyToken
		case s.ws != nil:
			if s.wsStandby != nil {
				displaced = append(displaced, s.wsStandby)
			}
			s.wsStandby = s.ws
			s.wsStandbyToken = newStandbyToken()
			s.wsStandbyFreshAt = time.Time{}
			s.ws = nil
			outcome.Standby = StandbyKindWS
			outcome.StandbyToken = s.wsStandbyToken
		}
	}
	switch kind {
	case StandbyKindWS:
		promoted := s.wsStandby
		s.wsStandby = nil
		s.wsStandbyToken = ""
		demotePrimaryLocked()
		s.ws = promoted
		// The promoted socket decodes into a fresh PCM-domain jitter (160
		// ticks per 20 ms frame, matching the existing WSS host path).
		s.host = newHostJitter(160)
	case StandbyKindQUIC:
		promoted := s.quicStandby
		s.quicStandby = nil
		s.quicStandbyToken = ""
		demotePrimaryLocked()
		s.quic = promoted
		// QUIC decodes into the same PCM-domain jitter as the WSS socket.
		s.host = newHostJitter(160)
	case StandbyKindICE:
		promoted := s.pcStandby
		promotedTrack := s.trackStandby
		s.pcStandby = nil
		s.trackStandby = nil
		s.pcStandbyToken = ""
		demotePrimaryLocked()
		s.pc = promoted
		s.track = promotedTrack
		hostTick := uint32(160)
		if s.clientCodec == codecOpus {
			hostTick = opusFrameTick
		}
		s.host = newHostJitter(hostTick)
		wireDecoder = true
	}
	s.hostPresent = true
	s.freshPending = true
	s.freshGen = generation
	promotedHost := s.host
	s.recordPromoteNonce(nonce, kind, token, expectEpoch, outcome, now)
	s.mu.Unlock()
	// A same-family standby displaced by the demote closes only after the
	// swap; its callbacks carry stale identities and can never detach the
	// newly demoted primary-of-record.
	for _, h := range displaced {
		h.Close()
	}
	if displacedPC != nil {
		_ = displacedPC.Close()
	}
	if wireDecoder {
		// Outside s.mu, matching setupClientCodec/AdoptClientPeer. The
		// decoder pointer is read under codecMu: a concurrent adoption's
		// codec transfer also takes codecMu, so the pointer can never tear;
		// codecMu alone (no s.mu held) keeps the established lock order.
		s.codecMu.Lock()
		dec := s.opusDec
		s.codecMu.Unlock()
		promotedHost.setDecoder(dec)
	}
	return outcome, nil
}

// SetProbeEcho marks (or clears) detached-probe echo mode. Only ever set on
// sessions the Manager never registers; a probe only reflects host frames so
// a client can measure the candidate's true round trip.
func (s *Session) SetProbeEcho(echo bool) {
	s.mu.Lock()
	s.probeEcho = echo
	s.mu.Unlock()
}

// AdoptClientPeer moves an ALREADY-NEGOTIATED, CONNECTED peer connection
// from a probe session into this session. The new peer is installed in the
// SAME critical section that detaches the previous host, so the session is
// never hostless: a concurrent replacement that wins the generation fence
// closes OUR pc as its "previous" transport and keeps its own host — an
// error here can never destroy a newer, healthy host. The previous host
// closes only after the install, and its callbacks carry stale generations.
//
// Ready-first: the peer must be in PeerConnectionStateConnected at adoption
// time; every validation happens under the probe/live locks before anything
// is torn down. Host lifecycle bookkeeping mirrors AttachWSHost: only a
// still-current CONNECTED adoption fires restored (cancelling a pending
// host-lost grace); a peer that dropped during adoption fires the lost
// lifecycle and must never have that grace cancelled by this adoption.
func (s *Session) AdoptClientPeer(probe *Session, asStandby ...bool) error {
	standby := len(asStandby) > 0 && asStandby[0]
	probe.mu.Lock()
	pc := probe.pc
	track := probe.track
	if pc == nil || track == nil {
		probe.mu.Unlock()
		if pc != nil {
			_ = pc.Close()
		}
		return fmt.Errorf("probe has no connected peer")
	}
	// Validate under the probe lock: ownership may have changed between any
	// earlier observation and this claim (TTL claim, replacement probe).
	if probe.pc != pc || probe.track != track {
		probe.mu.Unlock()
		_ = pc.Close()
		return fmt.Errorf("probe was superseded")
	}
	if state := pc.ConnectionState(); state != pion.PeerConnectionStateConnected {
		probe.mu.Unlock()
		_ = pc.Close()
		return fmt.Errorf("probe peer is not connected (state=%s)", state)
	}
	probe.pc = nil
	probe.track = nil
	probe.generation++
	probe.hostPresent = false
	// Own the probe's jitter buffer for the decoder detach below: a socket
	// re-attach on the probe would swap probe.host under probe.mu.
	probeHost := probe.host
	// RTP continuity fence (build-36 field root cause): the probe session has
	// been writing a 50 pps stream to this same track/SSRC since attach
	// (Opus DTX or PCMU silence), so the client receiver has already consumed
	// probe.sequence / probe.timestamp. Starting the live session at 0/0 made
	// every post-adoption packet look like a stale replay to the receiver
	// (RFC 3550 old-packet / NetEq duplicate logic); the phone then discarded
	// the modem audio until the counters caught up to the pre-adoption value
	// (~1 packet per 20 ms, i.e. as long as the probe had been running — 18 s
	// in the build-35 field call, longer than the call). The live stream MUST
	// continue the probe's counters on the shared SSRC.
	probeSequence := probe.sequence
	probeTimestamp := probe.timestamp
	probe.mu.Unlock()
	probe.codecMu.Lock()
	probeCodec := probe.clientCodec
	probe.codecMu.Unlock()
	hostTick := uint32(160)
	if probeCodec == codecOpus {
		hostTick = opusFrameTick
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = pc.Close()
		return fmt.Errorf("session is closed")
	}
	generation := s.generation
	var previousPC *pion.PeerConnection
	var previousWS *wsHost
	var previousQUIC *wsHost
	var adoptedHost *hostJitter
	if standby {
		// Hot-standby adoption (build 50): the pc joins the standby slot and
		// the PRIMARY is left completely untouched — no generation/epoch
		// bump (a pending fresh-media proof on the live primary must stay
		// valid), no jitter rebuild, no leg drain, no host-present change.
		// The previous standby pc (if any) closes after the install; the new
		// instance gets its own identity token and a zero freshness clock
		// (pre-adoption probe echo/stats never count — only frames flowing
		// through THIS session's read path after the install prove it).
		previousPC = s.pcStandby
		s.pcStandby = pc
		s.trackStandby = track
		s.pcStandbyToken = newStandbyToken()
		s.pcStandbyFreshAt = time.Time{}
		if s.track == nil {
			// No primary ICE track shares the counter space: continue the
			// probe's RTP sequence/timestamp so the standby stream stays
			// continuous for a later seamless promotion.
			s.sequence = probeSequence
			s.timestamp = probeTimestamp
		}
	} else {
		s.generation++
		generation = s.generation
		s.epoch++
		previousPC = s.pc
		previousWS = s.ws
		previousQUIC = s.quic
		// Build 63: a displaced WSS/QUIC primary is DEMOTED into its own
		// family's standby slot when that slot is free (the three-path
		// architecture keeps every eligible path warm across Q→RTC etc.),
		// and closed only when the slot is occupied. The ICE commit
		// therefore never silently retires the QUIC or WSS path.
		if previousWS != nil && s.wsStandby == nil {
			s.wsStandby = previousWS
			s.wsStandbyToken = newStandbyToken()
			s.wsStandbyFreshAt = time.Time{}
			previousWS = nil
		}
		if previousQUIC != nil && s.quicStandby == nil {
			s.quicStandby = previousQUIC
			s.quicStandbyToken = newStandbyToken()
			s.quicStandbyFreshAt = time.Time{}
			previousQUIC = nil
		}
		// Atomic install: from this point the session hosts the adopted peer,
		// and any concurrent replacer sees it as the transport to close.
		s.pc = pc
		s.track = track
		s.ws = nil
		s.quic = nil
		s.probeEcho = false
		s.hostPresent = true
		// Continue the probe's RTP sequence number and timestamp: the adopted
		// peer keeps the same SSRC and track object, so a reset is indistinguishable
		// from a replayed/old stream at the receiver and mutes the call (see the
		// continuity fence above).
		s.sequence = probeSequence
		s.timestamp = probeTimestamp
		// The adopted peer negotiated the probe's codec: the host buffer must
		// advance its play cursor in that codec's RTP clock (960 ticks per 20 ms
		// for Opus, 160 for PCMU).
		s.host = newHostJitter(hostTick)
		// Own the installed buffer for the codec transfer below: a concurrent
		// transport handover may swap s.host after this critical section.
		adoptedHost = s.host
		for _, leg := range s.legs {
			leg.Drain()
		}
	}
	s.mu.Unlock()

	// Old transports close only after the install; their callbacks carry
	// stale generations and can never detach the adopted host. A demoted
	// WSS/QUIC primary (nil here) stays attached in its family standby slot.
	if previousPC != nil {
		_ = previousPC.Close()
	}
	if previousWS != nil {
		previousWS.Close()
	}
	if previousQUIC != nil {
		previousQUIC.Close()
	}

	// Transfer the Opus codec machinery with the peer: the encoder feeds
	// this session's mixer writes, the closed-loop controller drives it and
	// the decoder plays out of this session's jitter buffer — so all three
	// belong to the live session now. The probe's jitter buffer (whose mixer
	// loop keeps ticking until the probe session is reaped) must release the
	// decoder reference so the same C object is never decoded concurrently
	// from two buffers. Lock order probe→live matches AdoptClientPeer's
	// session lock order; setDecoder takes the buffer mutex and renders
	// lock-free, so no cycle is introduced.
	//
	// Standby adoption on a session that ALREADY owns codec machinery keeps
	// the live encoder (it feeds both ICE tracks with the same downlink mix)
	// and never disturbs the primary's negotiated payload type; the probe's
	// duplicate machinery is released with the probe.
	probe.codecMu.Lock()
	s.codecMu.Lock()
	if standby && s.clientCodec != "" {
		if probe.codecOwned {
			if probe.opusEnc != nil {
				probe.opusEnc.Close()
			}
			if probe.opusDec != nil {
				probe.opusDec.Close()
			}
		}
		probe.clientCodec = ""
		probe.opusEnc = nil
		probe.opusDec = nil
		probe.codecOwned = false
	} else {
		s.clientCodec = probe.clientCodec
		s.payloadType = probe.payloadType
		s.opusEnc = probe.opusEnc
		s.opusDec = probe.opusDec
		s.controller = probe.controller
		s.nackBudget = probe.nackBudget
		s.codecOwned = probe.codecOwned
		probe.clientCodec = ""
		probe.opusEnc = nil
		probe.opusDec = nil
		probe.controller = nil
		probe.nackBudget = nil
		probe.codecOwned = false
	}
	s.codecMu.Unlock()
	probe.codecMu.Unlock()
	probeHost.setDecoder(nil)
	if adoptedHost != nil {
		adoptedHost.setDecoder(s.opusDec)
	}

	// Retarget the pc's existing read loop (installed by the probe's
	// AttachClient) at THIS session. pion fires OnTrack only when a track
	// arrives, so re-registering a handler here would never run for the
	// already-negotiated track; the route swap achieves the re-bind. The
	// probe's own generation was bumped above, so its direct pushes are
	// inert even if route were cleared. The pc's role (primary vs standby)
	// is read LIVE from the slot map on every frame, so a later hot-standby
	// promotion/demotion needs no read-loop change.
	probe.route.Store(&routeTarget{session: s, generation: generation})
	pc.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		s.applyHostState(pc, state)
	})

	// Final fence: a replacement that landed after the install already owns
	// this pc (it closed it as the previous transport); just report the
	// loss without touching the newer host.
	s.mu.Lock()
	var replaced bool
	if standby {
		replaced = s.closed || s.pcStandby != pc
	} else {
		replaced = s.closed || s.generation != generation || s.pc != pc
	}
	state := pion.PeerConnectionState(0)
	if !replaced && !standby {
		// Fresh state read under the lock: transitions that fired between
		// the install and the handler re-bind above are caught here instead
		// of being lost.
		state = pc.ConnectionState()
		if state == pion.PeerConnectionStateConnected {
			// Adopted and connected: require fresh-media proof before
			// cancelling recovery; the adopted peer's read loop already
			// routes frames here through probe.route with this generation.
			s.freshPending = true
			s.freshGen = generation
		}
	}
	s.mu.Unlock()
	if replaced {
		probe.route.Store(nil)
		_ = pc.Close()
		return fmt.Errorf("session was replaced")
	}
	if standby {
		return nil
	}
	switch state {
	case pion.PeerConnectionStateConnected:
		// Recovery clears on the first genuine frame (hostFrameArrived).
	case pion.PeerConnectionStateDisconnected, pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed:
		// Dropped during adoption: start the host-lost recovery lifecycle.
		// Crucially there is NO restore here — a peer that is gone must not
		// cancel the recovery it just started.
		s.applyHostState(pc, state)
	}
	return nil
}

func (s *Session) AddLeg(leg *Leg) {
	s.mu.Lock()
	if s.onLegClosed != nil {
		leg.mu.Lock()
		leg.onClosed = s.onLegClosed
		leg.mu.Unlock()
	}
	downlink := s.downlinkDenoise
	s.legs[leg.CallID] = leg
	s.mu.Unlock()
	// Propagate the current downlink policy outside s.mu (it allocates a
	// preprocessor lazily and must not hold the session lock).
	leg.SetDownlinkDenoise(downlink)
}

// RemoveLeg detaches a leg without closing it (conference transfers, split).
func (s *Session) RemoveLeg(callID string) *Leg {
	s.mu.Lock()
	leg := s.legs[callID]
	delete(s.legs, callID)
	s.mu.Unlock()
	return leg
}

func (s *Session) Leg(callID string) *Leg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.legs[callID]
}

func (s *Session) LegCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.legs)
}

// CloseClientOnly tears down the client-facing transports but keeps line legs
// alive so they can be moved into another session.
func (s *Session) CloseClientOnly() {
	s.mu.Lock()
	pc := s.pc
	s.pc = nil
	s.track = nil
	host := s.ws
	s.ws = nil
	pcStandby := s.pcStandby
	s.pcStandby = nil
	s.trackStandby = nil
	wsStandby := s.wsStandby
	s.wsStandby = nil
	s.wsStandbyToken = ""
	s.pcStandbyToken = ""
	quic := s.quic
	s.quic = nil
	quicStandby := s.quicStandby
	s.quicStandby = nil
	s.quicStandbyToken = ""
	s.hostPresent = false
	s.host.clear()
	s.generation++
	s.mu.Unlock()
	s.codecMu.Lock()
	if s.codecOwned {
		s.releaseCodecLocked()
	}
	s.codecMu.Unlock()
	if pc != nil {
		_ = pc.Close()
	}
	if host != nil {
		host.Close()
	}
	if pcStandby != nil {
		_ = pcStandby.Close()
	}
	if wsStandby != nil {
		wsStandby.Close()
	}
	if quic != nil {
		quic.Close()
	}
	if quicStandby != nil {
		quicStandby.Close()
	}
}

func (s *Session) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.mu.Lock()
	s.closed = true
	pc := s.pc
	s.pc = nil
	pcStandby := s.pcStandby
	s.pcStandby = nil
	host := s.ws
	s.ws = nil
	wsStandby := s.wsStandby
	s.wsStandby = nil
	s.wsStandbyToken = ""
	s.pcStandbyToken = ""
	quicHost := s.quic
	s.quic = nil
	quicStandby := s.quicStandby
	s.quicStandby = nil
	s.quicStandbyToken = ""
	s.recUplink = nil
	s.recDownlink = nil
	// Own the jitter buffer under the same lock: the trim statistics below
	// must read the buffer this Close retired, not a replacement.
	jitter := s.host
	s.generation++
	legs := make([]*Leg, 0, len(s.legs))
	for _, leg := range s.legs {
		legs = append(legs, leg)
	}
	s.legs = map[string]*Leg{}
	s.mu.Unlock()
	s.codecMu.Lock()
	if s.codecOwned {
		s.releaseCodecLocked()
	}
	s.codecMu.Unlock()
	if pc != nil {
		_ = pc.Close()
	}
	if pcStandby != nil {
		_ = pcStandby.Close()
	}
	if host != nil {
		host.Close()
	}
	if wsStandby != nil {
		wsStandby.Close()
	}
	if quicHost != nil {
		quicHost.Close()
	}
	if quicStandby != nil {
		quicStandby.Close()
	}
	for _, leg := range legs {
		leg.Close()
	}
	// Destroy the session-scoped uplink preprocessor at end. Legs moved out
	// (conference split/park) keep their own downstream preprocessors with
	// them; legs closed above already released theirs.
	s.closeUplinkDenoise()
	// Per-call evidence: how much stale audio the latency bound dropped.
	// Zero on a healthy paced call; non-zero proves a burst was trimmed.
	_, hostTrimmed, _, _ := jitter.stats()
	var legTrimmed uint64
	for _, leg := range legs {
		_, trimmed := leg.stats()
		legTrimmed += trimmed
	}
	if hostTrimmed > 0 || legTrimmed > 0 {
		slog.Info("media session latency trims", "session", s.Key, "host_trimmed", hostTrimmed, "leg_trimmed", legTrimmed)
	}
}

var silence = make([]int16, voice.FrameSamples)

func (s *Session) loop() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.mixOnce()
		}
	}
}

// mixClientOutput sums active external legs for the host. Sources are
// accumulated in float64 in sorted order and limited once, so the result is
// deterministic regardless of map iteration order.
func mixClientOutput(frames map[string][]int16) []int16 {
	ids := make([]string, 0, len(frames))
	for id := range frames {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	mixed := make([]float64, voice.FrameSamples)
	for _, id := range ids {
		addSource(mixed, frames[id], 0.9)
	}
	return limitFrame(mixed)
}

// mixLegOutput produces the mix-minus frame one external leg hears: the host
// plus every other active leg, never itself.
func mixLegOutput(legID string, frames map[string][]int16, hostFrame []int16) []int16 {
	ids := make([]string, 0, len(frames))
	for id := range frames {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	mixed := make([]float64, voice.FrameSamples)
	addSource(mixed, hostFrame, 0.9)
	for _, id := range ids {
		if id == legID {
			continue
		}
		addSource(mixed, frames[id], 0.9)
	}
	return limitFrame(mixed)
}

func addSource(target []float64, frame []int16, gain float64) {
	if frame == nil {
		return
	}
	for i := 0; i < len(target) && i < len(frame); i++ {
		target[i] += float64(frame[i]) * gain
	}
}

// limitFrame applies the single limiter stage: if the accumulated mix
// exceeds full scale it is scaled down instead of clipped, keeping the mix
// free of map-order-dependent distortion.
func limitFrame(values []float64) []int16 {
	maxAbs := 0.0
	for _, value := range values {
		if abs := math.Abs(value); abs > maxAbs {
			maxAbs = abs
		}
	}
	scale := 1.0
	if maxAbs > 32767 {
		scale = 32767 / maxAbs
	}
	out := make([]int16, len(values))
	for i, value := range values {
		out[i] = int16(math.Round(value * scale))
	}
	return out
}

// popLegFrames consumes at most one fresh 20 ms frame per unheld leg and
// drains held legs. It is shared by the live mix and by the uplink-stalled
// client-only mix so both preserve the same hold semantics: a held leg never
// feeds the client and never receives anything, and its queue is emptied on
// the hold transition.
func popLegFrames(legs []*Leg, now time.Time) (map[string][]int16, map[string]bool, []string) {
	frames := make(map[string][]int16, len(legs))
	held := make(map[string]bool, len(legs))
	legIDs := make([]string, 0, len(legs))
	for _, leg := range legs {
		legIDs = append(legIDs, leg.CallID)
		if leg.Held() {
			held[leg.CallID] = true
			leg.Drain()
			continue
		}
		if frame := leg.pop(now); frame != nil {
			frames[leg.CallID] = frame
		}
	}
	return frames, held, legIDs
}

// planMix is the pure decision layer used by tests. Host PRESENCE and mic
// LIVENESS are separate gates:
//   - hostPresent=false means the authorized host is genuinely gone: no
//     external leg may receive any frame at all (not even another external
//     leg's audio) and the client output is nil (the caller-side
//     silence/drain path owns the write).
//   - hostPresent=true, hostLive=false means the uplink mic stream stalled
//     past the live window while the transport is still attached: fresh
//     unheld leg frames are still mixed DOWN to the client (a dead
//     microphone must not mute the caller's received audio), but no
//     external leg receives anything, so a stalled host can never bridge
//     one external leg to another.
//   - hostLive=true is the normal mix-minus routing.
func planMix(hostPresent, hostLive bool, hostFrame []int16, frames map[string][]int16, legIDs []string, held map[string]bool) ([]int16, map[string][]int16) {
	// Presence is the STRONGER gate: a host that is not attached must never
	// produce any output even if a caller passes an inconsistent hostLive
	// boolean. Liveness only decides whether external legs may be written.
	if !hostPresent {
		return nil, map[string][]int16{}
	}
	if !hostLive {
		return mixClientOutput(frames), map[string][]int16{}
	}
	client := mixClientOutput(frames)
	sends := make(map[string][]int16, len(legIDs))
	for _, id := range legIDs {
		if held[id] {
			continue
		}
		sends[id] = mixLegOutput(id, frames, hostFrame)
	}
	return client, sends
}

// observeUplinkState records the host-uplink liveness edge for the one-shot
// transition log. It reports true exactly on the first tick that observes a
// change while a host is present, so the caller emits at most one line per
// stall/resume edge. When the host transport is gone the bookkeeping resets
// (no log; the host-lost lifecycle owns that transition), so a later
// re-attach can report its own edges.
func (s *Session) observeUplinkState(hostPresent, hostLive bool) (stalled, resumed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !hostPresent {
		s.uplinkStateKnown = false
		s.uplinkStalled = false
		return false, false
	}
	known := s.uplinkStateKnown
	prevStalled := s.uplinkStalled
	s.uplinkStateKnown = true
	s.uplinkStalled = !hostLive
	if !known || prevStalled == !hostLive {
		return false, false
	}
	return !hostLive, hostLive
}

func (s *Session) mixOnce() {
	now := time.Now()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	track := s.track
	ws := s.ws
	quic := s.quic
	trackStandby := s.trackStandby
	wsStandby := s.wsStandby
	quicStandby := s.quicStandby
	hostPresent := s.hostPresent
	recUplink, recDownlink := s.recUplink, s.recDownlink
	// Capture the jitter buffer in the SAME critical section as the transport
	// pointers: a concurrent re-attach (AttachWSHost / AttachClient /
	// AdoptClientPeer) swaps s.host under s.mu, and this tick must keep acting
	// on the buffer it snapshotted. Without the owned snapshot the
	// !hostPresent clear below dereferenced s.host again outside the lock and
	// could wipe the freshly installed transport's buffer.
	host := s.host
	hostFrame := host.pop(now)
	hostLive := hostPresent && host.live(now)
	legs := make([]*Leg, 0, len(s.legs))
	for _, leg := range s.legs {
		legs = append(legs, leg)
	}
	s.statsTick++
	tick := s.statsTick
	s.mu.Unlock()
	// Uplink (host mic) denoise once before the frame enters any mix. Runs
	// outside s.mu (processUplink takes it); hostFrame is an owned slice
	// from pop. Gate is frame-boundary safe and state is session-scoped.
	if hostFrame != nil {
		hostFrame = s.processUplink(hostFrame)
	}
	// Recording tap 1/2: canonical post-selection, post-denoise uplink. The
	// same frame feeds the mix-minus leg sends below; nil (stalled mic or no
	// host) records as silence so the timeline survives transient gaps.
	if recUplink != nil {
		if hostPresent && hostLive {
			recUplink(hostFrame)
		} else {
			recUplink(nil)
		}
	}

	// Periodic bounded-latency evidence: only logs when the trim paths
	// actually dropped stale audio, so a live call leaves proof of the bound
	// in the daemon log without a per-tick flood.
	if tick%250 == 0 {
		hostDepth, hostTrimmed, fastArrivals, maxGap := host.stats()
		legDepth := 0
		var legTrimmed uint64
		for _, leg := range legs {
			depth, trimmed := leg.stats()
			legDepth += depth
			legTrimmed += trimmed
		}
		s.mu.Lock()
		freshHost := hostTrimmed - s.reportedHostTrim
		freshLeg := legTrimmed - s.reportedLegTrimmed
		// A probe adoption or host replacement installs a FRESH jitter buffer
		// whose trimmed counter restarts at 0 while the reported watermark
		// persists. Without the reset clamp the unsigned delta wraps to
		// ~1.8e19 and poisons the diagnostic line (observed live
		// host_trimmed=18446744073709551613).
		if hostTrimmed < s.reportedHostTrim {
			freshHost = hostTrimmed
		}
		if legTrimmed < s.reportedLegTrimmed {
			freshLeg = legTrimmed
		}
		if freshHost > 0 || freshLeg > 0 {
			s.reportedHostTrim, s.reportedLegTrimmed = hostTrimmed, legTrimmed
		}
		s.mu.Unlock()
		if freshHost > 0 || freshLeg > 0 {
			slog.Info("media latency trim", "session", s.Key,
				"host_depth", hostDepth, "host_trimmed", freshHost,
				"leg_depth", legDepth, "leg_trimmed", freshLeg,
				"host_fast_arrivals", fastArrivals, "host_max_gap_ms", maxGap.Milliseconds())
		} else if fastArrivals > 0 {
			// Delivery-spacing evidence even when the latency bound absorbed
			// the burst with zero trims: this is the measured batchiness the
			// hostJitterTarget must cover (fast_arrivals ≈ frames delivered
			// coalesced, max_gap the longest inter-batch silence).
			slog.Info("media host burst spacing", "session", s.Key,
				"host_depth", hostDepth,
				"host_fast_arrivals", fastArrivals, "host_max_gap_ms", maxGap.Milliseconds())
		}
		// Build-48 adaptive-queue + denoise evidence, every 5 s: current
		// prime/rebuffer levels in ms, cumulative starvation episodes since
		// attach, and directional denoise processing time. Scalars only —
		// no audio content, no PII.
		hostLevel, hostUnderruns := host.adaptiveStats()
		legLevelMax, legUnderruns := 0, uint64(0)
		for _, leg := range legs {
			lvl, under := leg.adaptiveStats()
			if lvl > legLevelMax {
				legLevelMax = lvl
			}
			legUnderruns += under
		}
		upOn, downOn, upNanos, downNanos := s.DenoiseStats()
		upPerFrameUs, downPerFrameUs := s.DenoiseStatsWindow()
		// Scope: these are QUEUE levels (playout latency budget) plus DENOISE
		// compute only. Network RTT and full mouth-to-ear latency are measured
		// separately (route telemetry / physical timing) and are NOT part of
		// this line. Denoise compute is reported per-frame (window mean) AND
		// cumulative (since attach) so the two are never confused.
		slog.Info("media adaptive queue", "session", s.Key,
			"host_level_ms", hostLevel*20, "host_starvations", hostUnderruns,
			"leg_level_ms_max", legLevelMax*20, "leg_starvations", legUnderruns,
			"uplink_denoise", upOn, "downlink_denoise", downOn,
			"uplink_denoise_us_per_frame", upPerFrameUs,
			"downlink_denoise_us_per_frame", downPerFrameUs,
			"uplink_denoise_us_total", upNanos/1000, "downlink_denoise_us_total", downNanos/1000)
	}

	if stalled, resumed := s.observeUplinkState(hostPresent, hostLive); stalled || resumed {
		// Bounded scalar evidence: one line per liveness edge, never per
		// tick. Session key + host buffer depth only; no audio content or
		// PII.
		if stalled {
			slog.Info("media host uplink stalled", "session", s.Key, "host_depth", host.depth())
		} else {
			slog.Info("media host uplink resumed", "session", s.Key)
		}
	}

	var client []int16
	var sends map[string][]int16
	if hostPresent {
		frames, held, legIDs := popLegFrames(legs, now)
		// Downlink (remote/modem leg -> client mic owner) denoise: apply
		// each leg's own isolated preprocessor to that leg's frame BEFORE
		// mix, exactly once per leg per tick, so the subsequent mix-minus
		// leg sends reuse the same processed frame and never re-denoise.
		for _, leg := range legs {
			if frame, ok := frames[leg.CallID]; ok {
				frames[leg.CallID] = leg.processDownlink(frame)
			}
		}
		client, sends = planMix(true, hostLive, hostFrame, frames, legIDs, held)
		// Recording tap 2/2: a 1:1 call taps this call's own downlink leg
		// frame (post leg denoise, pre client mix). A conference taps the
		// canonical client mix instead (all unheld legs summed exactly
		// once, post denoise, the same frame the host hears), so every
		// participant is captured exactly once and a held/standby stream is
		// never double-recorded.
		if recDownlink != nil {
			if s.Kind == "conference" {
				recDownlink(client)
			} else {
				recDownlink(frames[s.Key])
			}
		}
	} else {
		// Host genuinely gone: full isolation as before. Never route one
		// external leg to another or replay stale line audio while the host
		// cannot receive it; wipe the buffer.
		for _, leg := range legs {
			leg.Drain()
		}
		if recDownlink != nil {
			recDownlink(nil)
		}
		// The host buffer is wiped only when the host is genuinely gone. A
		// present-but-quiet host (its first frames are still priming the
		// jitter buffer, or its mic paused briefly) must keep them: clearing
		// here made a perfectly regular 1-frame-per-tick stream prime forever
		// (frames were wiped before two could accumulate), a latent race the
		// bursty WebRTC RTP delivery only masked.
		host.clear()
	}
	if !hostLive && client == nil {
		// Historic host-stalled/host-absent caller-side output is silence
		// for BOTH codecs: a present host whose mic stalled must still hear
		// the call, and an abandoned host never changes the caller-side
		// cadence. planMix already returns silence PCM for the
		// present-but-stalled case; this guards any future nil-producing
		// path (e.g. an empty leg mix) from skipping the write.
		client = silence
	}
	if hostLive {
		s.mu.Lock()
		probeEcho := s.probeEcho
		s.mu.Unlock()
		if probeEcho && hostFrame != nil {
			// Detached probe: echo the host's own frame back so the client
			// measures a genuine end-to-end round trip over this candidate.
			// Only a LIVE host frame may be echoed: a stalled probe must
			// never replay a stale buffered frame.
			client = hostFrame
		}
	}
	// Opus keeps the 20 ms RTP cadence even with no leg audio: DTX emits a
	// comfort-only packet so the phone's jitter buffer and loss accounting
	// never see a silent hole. PCMU keeps its historic skip (nil client →
	// no packet) to avoid changing the long-proven fallback path. The
	// stalled/host-absent paths above pre-fill silence for both codecs, so
	// this normalization only handles a defensive nil.
	s.codecMu.Lock()
	iceCodec := s.clientCodec
	s.codecMu.Unlock()
	if client == nil && iceCodec == codecOpus {
		client = silence
	}
	s.writeClientPCM(track, trackStandby, ws, wsStandby, quic, quicStandby, client)
	for id, frame := range sends {
		leg := s.Leg(id)
		if leg == nil {
			continue
		}
		if encoded, err := voice.EncodePCMU(frame); err == nil {
			if sendErr := leg.Send(encoded); sendErr != nil {
				slog.Warn("media leg write failed", "session", s.Key, "call_id", id, "error", sendErr)
			}
		}
	}
}

// writeClientPCM fans one mixed 8 kHz PCM frame out to whichever host
// transports are attached, encoding per transport: the pion RTP track uses
// the session's negotiated ICE codec; the WSS socket uses its own negotiated
// codec (PCMU = historic 160-byte frames, or Opus when codec=opus was
// negotiated on the attach URL). A nil frame means "no leg audio": the ICE
// Opus path converts it to silence at the caller (DTX keeps the cadence);
// the WSS Opus socket encodes a DTX comfort frame while the WSS PCMU socket
// keeps its historic skip. The WSS codec controller also ticks here.
//
// Hot standby (build 50): the SAME frame is mirrored to the standby
// transport(s) — one ICE encode feeds the primary and (if present) the
// standby track with identical sequence/timestamp on their distinct SSRCs,
// and each WSS socket (primary and standby) encodes its own copy from its
// own negotiated codec. The standby therefore carries continuous, real,
// inaudible-while-unrendered downlink media, so promotion never needs a new
// connection and never waits for media to resume.
func (s *Session) writeClientPCM(track, trackStandby *pion.TrackLocalStaticRTP, ws, wsStandby, quic, quicStandby *wsHost, pcm []int16) {
	if (track != nil || trackStandby != nil) && pcm != nil {
		if encoded, err := s.encodeClientFrame(pcm); err == nil {
			s.writeTrack(track, trackStandby, encoded)
		}
	}
	writeWS := func(socket *wsHost) {
		if socket == nil {
			return
		}
		frame := pcm
		if frame == nil {
			if socket.Codec() != codecOpus {
				return
			}
			frame = silence
		}
		if encoded, err := socket.encodePCM(frame); err == nil {
			socket.enqueue(socket.frameOutbound(encoded))
		}
		socket.adaptController(time.Now())
	}
	writeWS(ws)
	writeWS(wsStandby)
	writeWS(quic)
	writeWS(quicStandby)
}

func (s *Session) writeTrack(track, trackStandby *pion.TrackLocalStaticRTP, frame []byte) {
	s.codecMu.Lock()
	payloadType := s.payloadType
	step := uint32(160)
	if s.clientCodec == codecOpus {
		step = opusFrameTick
	}
	s.codecMu.Unlock()
	s.mu.Lock()
	sequence := s.sequence
	timestamp := s.timestamp
	s.sequence++
	s.timestamp += step
	s.mu.Unlock()
	header := rtp.Header{Version: 2, PayloadType: payloadType, SequenceNumber: sequence, Timestamp: timestamp}
	if track != nil {
		if err := track.WriteRTP(&rtp.Packet{Header: header, Payload: frame}); err != nil {
			slog.Debug("media client write failed", "session", s.Key, "error", err)
		}
	}
	if trackStandby != nil {
		if err := trackStandby.WriteRTP(&rtp.Packet{Header: header, Payload: frame}); err != nil {
			slog.Debug("media standby client write failed", "session", s.Key, "error", err)
		}
	}
}
