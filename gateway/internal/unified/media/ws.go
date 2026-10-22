package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"callrelay/gateway/internal/opus"
	"callrelay/gateway/internal/voice"

	"github.com/gorilla/websocket"
)

// wsHostQueue bounds client-bound audio: at 20 ms per frame six frames are
// ~120 ms. A slow HTTPS tunnel must drop the oldest (stale) frame, never
// queue latency indefinitely. wsWriteDeadline bounds a single blocked
// kernel/TCP write so a stalled tunnel cannot wedge the writer forever.
const (
	wsHostQueue     = 6
	wsWriteDeadline = 10 * time.Second

	// WSS closed-loop bounds (build 27): the WSS path is TCP, so loss hides
	// as delay — the honest congestion signals are the socket's own queue
	// pressure and the app's reported playback depth / inter-arrival gaps
	// (ping extension). The controller steps Opus bitrate inside the same
	// 16-64 kbps range with the same hold/hysteresis discipline as the ICE
	// controller.
	wsBitrateMin     = 16000
	wsBitrateMax     = 64000
	wsBitrateInitial = 32000
	wsBitrateStep    = 8000
	// A single blocked write or queue overflow this long/often counts as
	// congestion evidence; sustained clean windows allow one step up.
	wsBlockEvidence    = 250 * time.Millisecond
	wsCleanWindow      = 3 * time.Second
	wsCooldown         = 800 * time.Millisecond
	wsAppFeedbackStale = 10 * time.Second
	wsAppBufHigh       = 240 // ms of app-reported playback buffer
	wsAppBufLow        = 120
	wsAppGapHighMs     = 200 // reported inter-arrival gap
	wsAppGapFECAtMs    = 60  // enable FEC at/above this sustained gap
	wsAppGapFECBelowMs = 30
)

// wsHost is the WebSocket client transport for a Session's host side. It
// replaces the pion peer connection when the client negotiates the
// authenticated WSS media path (cellular clients that cannot reach ICE host
// candidates). Binary messages carry one 20 ms audio frame each — PCMU
// (160 bytes) by default, or Opus when the client negotiated codec=opus on
// the attach URL (the ready control then announces "codec":"opus"); the
// protocol mirrors the worker's internal media socket ({"type":"ready"}
// control first, then framed audio).
type wsHost struct {
	conn hostSocket

	// Codec machinery for the negotiated WSS codec. Opus sessions carry an
	// encoder (mixed PCM -> socket) and decoder (socket -> host PCM), plus
	// the bounded queue/app-stats controller described above. Nil for PCMU.
	//
	// Opus framing (build 27, negotiated as fmt=seq16 in the ready control):
	// each binary message is [seq: 2 bytes big-endian][Opus payload]. The
	// sequence counts 20 ms MEDIA SLOTS, not sent frames: when the bounded
	// outbound queue drops a stale frame (or the encoder fails), the sequence
	// space keeps the hole, so the receiver can tell a genuine gap from
	// silence, run mature PLC for each missing slot, and align the stateful
	// decoder's timeline. Legacy PCMU stays the bare 160-byte frame.
	codecMu       sync.Mutex
	codec         string // codecOpus | codecPCMU
	framed        bool
	seqOut        uint16
	opusEnc       *opus.Encoder
	opusDec       *opus.Decoder
	bitrate       int
	fecEnabled    bool
	lastChange    time.Time
	lastFecChange time.Time
	lastAppStat   time.Time
	// Congestion evidence windows (consumed by the periodic controller
	// tick driven from the session mixer).
	blockedTotal time.Duration
	overflows    int
	cleanSince   time.Time
	appBufMs     int
	appGapMs     int
	upBufFrames  int
	upGapMs      int

	mu      sync.Mutex
	queue   [][]byte
	signal  chan struct{}
	control chan []byte
	closed  bool
	once    sync.Once
}

func newWSHost(conn hostSocket, codec string) *wsHost {
	h := &wsHost{
		conn: conn, signal: make(chan struct{}, 1), control: make(chan []byte, 8),
		codec: codec, bitrate: wsBitrateInitial,
	}
	if codec == codecOpus {
		if enc, err := opus.NewEncoder(); err == nil {
			if dec, decErr := opus.NewDecoder(); decErr == nil {
				h.opusEnc = enc
				h.opusDec = dec
			} else {
				enc.Close()
				h.codec = codecPCMU
			}
		} else {
			h.codec = codecPCMU
		}
		if h.codec == codecOpus {
			h.framed = true
			h.fecEnabled = true
			h.lastChange = time.Now()
			h.cleanSince = time.Now()
			slog.Info("wss media codec negotiated", "codec", "opus", "fmt", WSFrameFmtSeq16)
		}
	}
	return h
}

// Codec reports the negotiated WSS codec (never empty; falls back to PCMU).
func (h *wsHost) Codec() string {
	h.codecMu.Lock()
	defer h.codecMu.Unlock()
	if h.codec == "" {
		return codecPCMU
	}
	return h.codec
}

// WSFrameFmtSeq16 is the negotiated Opus wire framing: [seq u16 BE][payload].
const WSFrameFmtSeq16 = "seq16"

// frameOutbound stamps one encoded Opus payload with the media-slot sequence
// number. Called from the session mixer goroutine only.
func (h *wsHost) frameOutbound(payload []byte) []byte {
	h.codecMu.Lock()
	framed := h.framed
	seq := h.seqOut
	h.seqOut++
	h.codecMu.Unlock()
	if !framed {
		return payload
	}
	out := make([]byte, 2+len(payload))
	out[0] = byte(seq >> 8)
	out[1] = byte(seq)
	copy(out[2:], payload)
	return out
}

// maxWSGapSlots bounds the PLC work a single gap can trigger (a stalled
// socket must not produce minutes of concealment in one burst).
const maxWSGapSlots = 50

// wsDepacketize advances the expected-sequence state for the framed uplink:
// it returns the payload to decode for THIS message plus how many slots were
// missed before it (each missed slot is one PLC frame at the receiver). A
// duplicate (seq at/behind the cursor) reports consume=false: the caller
// must not decode it nor advance the media timeline. A first frame seeds
// the cursor.
func wsDepacketize(state *uint16, haveState *bool, seq uint16, payload []byte) (decode []byte, missed int, consume bool) {
	if !*haveState {
		*state = seq
		*haveState = true
		return payload, 0, true
	}
	// Wrap-aware ordering: a sequence within half the space behind the
	// cursor is obsolete, equal is a duplicate — neither may reach the
	// stateful decoder.
	d := int16(seq - *state)
	if d <= 0 {
		return nil, 0, false
	}
	missed = int(d) - 1
	if missed > maxWSGapSlots {
		missed = maxWSGapSlots
	}
	*state = seq
	return payload, missed, true
}

// Close releases codec machinery with the socket.
func (h *wsHost) closeCodec() {
	h.codecMu.Lock()
	defer h.codecMu.Unlock()
	if h.opusEnc != nil {
		h.opusEnc.Close()
		h.opusEnc = nil
	}
	if h.opusDec != nil {
		h.opusDec.Close()
		h.opusDec = nil
	}
}

// encodePCM renders one mixed 8 kHz PCM frame into the negotiated wire
// format. Called from the session mixer goroutine only.
func (h *wsHost) encodePCM(pcm []int16) ([]byte, error) {
	h.codecMu.Lock()
	defer h.codecMu.Unlock()
	if h.codec == codecOpus && h.opusEnc != nil {
		return h.opusEnc.Encode(pcm)
	}
	return voice.EncodePCMU(pcm)
}

// decodeFrame renders one inbound binary frame into 8 kHz PCM for the host
// jitter buffer. Called from the socket pump goroutine only.
func (h *wsHost) decodeFrame(payload []byte) ([]int16, error) {
	h.codecMu.Lock()
	defer h.codecMu.Unlock()
	if h.codec == codecOpus && h.opusDec != nil {
		return h.opusDec.Decode(payload)
	}
	return voice.DecodePCMU(payload)
}

// decodePLC renders one missed 20 ms slot with mature libopus concealment
// (the framed uplink calls it once per sequence hole).
func (h *wsHost) decodePLC() ([]int16, error) {
	h.codecMu.Lock()
	defer h.codecMu.Unlock()
	if h.opusDec == nil {
		return nil, fmt.Errorf("wss decode plc: no decoder")
	}
	return h.opusDec.Decode(nil)
}

// decodeFEC renders the missed PREVIOUS slot from the given packet's RFC 7587
// inband FEC redundancy without consuming the packet (it decodes normally
// afterwards). Only meaningful for a one-slot gap on a framed Opus uplink.
func (h *wsHost) decodeFEC(payload []byte) ([]int16, error) {
	h.codecMu.Lock()
	defer h.codecMu.Unlock()
	if h.opusDec == nil {
		return nil, fmt.Errorf("wss decode fec: no decoder")
	}
	return h.opusDec.DecodeFEC(payload)
}

// noteWriteBlock records a writer blockage duration as congestion evidence.
func (h *wsHost) noteWriteBlock(d time.Duration) {
	if d < wsBlockEvidence {
		return
	}
	h.codecMu.Lock()
	h.blockedTotal += d
	h.cleanSince = time.Time{}
	h.codecMu.Unlock()
}

// noteQueueOverflow records a dropped-oldest enqueue as congestion evidence.
func (h *wsHost) noteQueueOverflow() {
	h.codecMu.Lock()
	h.overflows++
	h.cleanSince = time.Time{}
	h.codecMu.Unlock()
}

// noteAppStats consumes the ping extension: the app's playback buffer depth
// (frames→ms) and its worst inter-arrival gap over the last second. This is
// the freshest end-to-end delay evidence the TCP path can produce.
func (h *wsHost) noteAppStats(bufFrames, gapMs int) {
	h.codecMu.Lock()
	defer h.codecMu.Unlock()
	h.lastAppStat = time.Now()
	h.appBufMs = bufFrames * 20
	h.appGapMs = gapMs
	if h.appBufMs > wsAppBufHigh || gapMs > wsAppGapHighMs {
		h.cleanSince = time.Time{}
	}
}

// adaptController applies the bounded WSS downlink controller. Called from
// the session mixer tick; steps bitrate/FEC on the freshest of socket
// pressure and app stats, with cooldown and stale-hold. It never raises on
// stale or absent feedback.
func (h *wsHost) adaptController(now time.Time) {
	h.codecMu.Lock()
	defer h.codecMu.Unlock()
	if h.codec != codecOpus || h.opusEnc == nil {
		return
	}
	congested := h.blockedTotal > 0 || h.overflows > 0 ||
		h.appBufMs > wsAppBufHigh || h.appGapMs > wsAppGapHighMs
	blocked := h.blockedTotal
	overflows := h.overflows
	h.blockedTotal = 0
	h.overflows = 0

	wantFEC := h.fecEnabled
	if h.appGapMs >= wsAppGapFECAtMs {
		wantFEC = true
	} else if h.appGapMs <= wsAppGapFECBelowMs {
		wantFEC = false
	}
	if wantFEC != h.fecEnabled && now.Sub(h.lastFecChange) >= wsCooldown {
		if err := h.opusEnc.SetInbandFEC(wantFEC); err == nil {
			h.fecEnabled = wantFEC
			h.lastFecChange = now
			slog.Info("wss opus fec adapted", "enabled", wantFEC, "appGapMs", h.appGapMs)
		}
	}

	target := h.bitrate
	switch {
	case congested:
		target -= wsBitrateStep
	case blocked > 0 || overflows > 0:
		target -= wsBitrateStep
	default:
		// Raise only on a sustained clean window AND a fresh, low app
		// buffer. Stale/absent stats hold the current value.
		statsFresh := now.Sub(h.lastAppStat) < wsAppFeedbackStale
		if h.cleanSince.IsZero() {
			h.cleanSince = now
		}
		if now.Sub(h.cleanSince) >= wsCleanWindow && statsFresh && h.appBufMs <= wsAppBufLow && h.appGapMs <= wsAppGapFECBelowMs {
			target += wsBitrateStep
		}
	}
	target = clampInt(target, wsBitrateMin, wsBitrateMax)
	if target == h.bitrate {
		return
	}
	if now.Sub(h.lastChange) < wsCooldown {
		return
	}
	if err := h.opusEnc.SetBitrate(target); err != nil {
		return
	}
	h.bitrate = target
	h.lastChange = now
	slog.Info("wss opus bitrate adapted", "bitrate", target,
		"blockedMs", blocked.Milliseconds(), "overflows", overflows,
		"appBufMs", h.appBufMs, "appGapMs", h.appGapMs)
}

// uplinkEvidence reports what the app needs for its own uplink adaptation:
// the host buffer depth this socket feeds (frames) and the worst app->gateway
// inter-arrival gap observed by this pump in the last second (ms).
func (h *wsHost) uplinkEvidence() (bufFrames int, gapMs int) {
	h.codecMu.Lock()
	defer h.codecMu.Unlock()
	return h.upBufFrames, h.upGapMs
}

// noteUplinkGap records pump-side inter-arrival evidence for the pong.
func (h *wsHost) noteUplinkGap(gapMs int) {
	h.codecMu.Lock()
	h.upGapMs = gapMs
	h.codecMu.Unlock()
}

// noteUplinkBuf records the host buffer depth the pump observes.
func (h *wsHost) noteUplinkBuf(frames int) {
	h.codecMu.Lock()
	h.upBufFrames = frames
	h.codecMu.Unlock()
}

// enqueueText schedules a text control frame (e.g. an RTT pong). It is
// written by the same goroutine as the audio frames so the socket never
// sees concurrent writes; a backed-up control queue drops the oldest pong.
func (h *wsHost) enqueueText(payload []byte) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	select {
	case h.control <- payload:
	default:
	}
}

// enqueue appends one encoded frame; when the queue is full the oldest frame
// is discarded so a backed-up tunnel never replays stale audio (and the
// overflow is counted as congestion evidence for the codec controller).
func (h *wsHost) enqueue(frame []byte) {
	overflow := false
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	if len(h.queue) >= wsHostQueue {
		h.queue = h.queue[1:]
		overflow = true
	}
	h.queue = append(h.queue, frame)
	h.mu.Unlock()
	if overflow {
		h.noteQueueOverflow()
	}
	select {
	case h.signal <- struct{}{}:
	default:
	}
}

func (h *wsHost) writer() {
	for {
		// Text control (RTT pongs) goes first, written by this same
		// goroutine, so audio and control frames never interleave.
		select {
		case text := <-h.control:
			if !h.write(websocket.TextMessage, text) {
				return
			}
			continue
		default:
		}

		h.mu.Lock()
		for len(h.queue) == 0 && !h.closed {
			h.mu.Unlock()
			select {
			case <-h.signal:
			case text := <-h.control:
				if !h.write(websocket.TextMessage, text) {
					return
				}
			}
			h.mu.Lock()
		}
		if h.closed {
			h.mu.Unlock()
			return
		}
		frame := h.queue[0]
		h.queue = h.queue[1:]
		h.mu.Unlock()
		if !h.write(websocket.BinaryMessage, frame) {
			return
		}
	}
}

func (h *wsHost) write(messageType int, payload []byte) bool {
	start := time.Now()
	_ = h.conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
	if err := h.conn.WriteMessage(messageType, payload); err != nil {
		// Converge this exact socket: the reader unblocks with an error,
		// the host detaches and the Manager's host-lost grace applies.
		h.Close()
		return false
	}
	h.noteWriteBlock(time.Since(start))
	return true
}

func (h *wsHost) Close() {
	h.once.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.queue = nil
		h.mu.Unlock()
		select {
		case h.signal <- struct{}{}:
		default:
		}
		h.closeCodec()
		_ = h.conn.Close()
	})
}

// EnqueueTextOnPrimary delivers one text control frame on the session's
// current primary WSS host (the wsHost writer serializes it). Used by the
// QUIC standby attach (build 61): the QUIC audio session has no control
// channel, so its ready greeting travels the incumbent's reliable WSS
// plane. Build 63: when no WSS PRIMARY is attached the greeting falls back
// to the demoted WSS standby socket — with a dedicated QUIC primary the
// control plane lives on the ws-family standby and must keep working.
// False only when no WSS socket is attached at all.
func (s *Session) EnqueueTextOnPrimary(payload []byte) bool {
	s.mu.Lock()
	host := s.ws
	if host == nil {
		host = s.wsStandby
	}
	s.mu.Unlock()
	if host == nil {
		return false
	}
	host.enqueueText(payload)
	return true
}

// AttachWSHost replaces whatever host transport the session currently has
// (pion peer connection or an older socket) with an authenticated WSS
// transport. codec is the negotiated wire codec for this socket ("opus" when
// the client requested codec=opus on the attach URL and libopus is
// available, otherwise "pcmu"); the ready greeting (if any) is written
// BEFORE the frame writer starts, so greeting and audio can never interleave
// on the socket.
//
// Re-attaching arms fresh-media proof (generation-valid): onHostRestored
// fires only when the first GENUINE inbound frame arrives on this socket, so
// a host-lost recovery episode is never cancelled by a handshake alone — but
// it IS cancelled as soon as real audio flows. It returns the generation the
// socket's pump must fence on.
func (s *Session) AttachWSHost(conn hostSocket, codec string, ready func(negotiatedCodec string, info AttachInfo) error) (uint64, error) {
	host := newWSHost(conn, codec)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		host.Close()
		return 0, fmt.Errorf("session is closed")
	}
	s.generation++
	generation := s.generation
	// The primary identity changes: bump the role epoch so clients holding
	// a stale expectEpoch are rejected by any in-flight promotion command.
	s.epoch++
	previous := s.pc
	s.pc = nil
	s.track = nil
	previousWS := s.ws
	previousQUIC := s.quic
	// Build 63: a displaced QUIC primary (CROSS-family) demotes into its own
	// family's standby slot when free, so a WSS (re)attach never silently
	// retires the QUIC path. A replaced WSS primary is the SAME family as
	// the new socket — it is always CLOSED (demoting it would leave two
	// live WSS sockets and would resurrect superseded pumps).
	if previousQUIC != nil && s.quicStandby == nil {
		s.quicStandby = previousQUIC
		s.quicStandbyToken = newStandbyToken()
		s.quicStandbyFreshAt = time.Time{}
		previousQUIC = nil
	}
	s.ws = host
	s.quic = nil
	// The socket is the host transport: presence is explicit and immediate,
	// and host audio arriving within the live window keeps the mix running.
	s.hostPresent = true
	// Fresh PCM-domain jitter for the socket path: the socket feeds decoded
	// PCM with synthetic 8 kHz timestamps (160 per frame), so any Opus ICE
	// decoder/tick from a previous transport must not leak into it (an
	// ICE-Opus -> WSS fallback would otherwise step the play cursor by 960
	// against 160-stepped pushes and conceal every frame).
	s.host = newHostJitter(160)
	for _, leg := range s.legs {
		leg.Drain()
	}
	s.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	if previousWS != nil && previousWS != host {
		previousWS.Close()
	}
	if previousQUIC != nil {
		previousQUIC.Close()
	}
	if ready != nil {
		// Announce the codec ACTUALLY created locally (newWSHost falls
		// back to PCM when libopus init fails); announcing the requested
		// codec while speaking PCMU silently wedges the call forever.
		s.mu.Lock()
		info := AttachInfo{Epoch: s.epoch}
		s.mu.Unlock()
		if err := ready(host.Codec(), info); err != nil {
			s.DetachWSHost(generation, host)
			return generation, err
		}
	}
	// Recovery clears only on FRESH-MEDIA proof (the first genuine inbound
	// frame on this new socket), never on the attach/handshake alone, so arm
	// that proof before the writer starts. A replacement that landed while
	// the ready greeting was in flight must never start its writer.
	if s.isCurrentWSHost(generation, host) {
		go host.writer()
		s.armFreshMediaProof(generation)
	}
	return generation, nil
}

// isCurrentWSHost reports whether host is currently the session's primary
// host socket (WSS or QUIC — build 63 dedicated QUIC primary slot).
func (s *Session) isCurrentWSHost(generation uint64, host *wsHost) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && s.generation == generation && (s.ws == host || s.quic == host)
}

// isStandbyWSHost reports whether host is currently one of the session's
// standby sockets (hot standby, build 50/63): attached but carrying no host
// mix.
func (s *Session) isStandbyWSHost(host *wsHost) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && (s.wsStandby == host || s.quicStandby == host)
}

// wsHostByConn returns the live host or standby host served by conn, so one
// pump implementation fences on socket identity rather than role at entry (a
// standby socket promoted mid-call stays on the same pump).
func (s *Session) wsHostByConn(conn hostSocket) *wsHost {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ws != nil && s.ws.conn == conn {
		return s.ws
	}
	if s.wsStandby != nil && s.wsStandby.conn == conn {
		return s.wsStandby
	}
	if s.quic != nil && s.quic.conn == conn {
		return s.quic
	}
	if s.quicStandby != nil && s.quicStandby.conn == conn {
		return s.quicStandby
	}
	return nil
}

// DetachWSHost removes the socket when its generation is still current, so a
// replaced or superseded socket can never tear down a newer host. A nil host
// detaches whatever is current for the generation (used by error paths that
// never learned the host pointer). When host is non-nil the fence is the
// socket IDENTITY (s.ws == host), so a pump that learned its socket before a
// hot-standby promotion bump can still detach it correctly.
func (s *Session) DetachWSHost(generation uint64, host *wsHost, causes ...HostLossCause) {
	cause := HostLossRecoverable
	if len(causes) > 0 {
		cause = causes[0]
	}
	s.mu.Lock()
	primary := s.ws
	if s.quic != nil {
		primary = s.quic
	}
	if s.closed || (host != nil && primary != host) || (host == nil && s.generation != generation) {
		s.mu.Unlock()
		if host != nil {
			host.Close()
		}
		return
	}
	current := primary
	if s.ws == host {
		s.ws = nil
	} else {
		s.quic = nil
	}
	s.hostPresent = false
	s.freshPending = false
	s.host.clear()
	for _, leg := range s.legs {
		leg.Drain()
	}
	lost := s.onHostLost
	s.mu.Unlock()
	if current != nil {
		current.Close()
	}
	if lost != nil {
		lost(s.Key, cause)
	}
}

// AttachWSStandby attaches an authenticated WSS socket in the hot-standby
// role (build 50): it coexists with the current primary transport WITHOUT
// evicting it — no generation reset, no jitter rebuild, no leg drain, no
// host-lost lifecycle. The mixer mirrors the client downlink mix onto it so
// the standby path proves itself with real inaudible media; inbound frames
// are decoded for freshness and discarded, never mixed (single-uplink
// invariant). The ready callback reports standby=true so the client can tell
// acceptance apart from an old server that ignored the flag (and would have
// replaced the primary). When the session has NO primary yet, the request
// degrades to a normal primary attach so the client always gets media.
//
// The returned bool reports the actual role: true = standby, false = the
// request degraded to a primary attach. PumpWSHost serves either role.
func (s *Session) AttachWSStandby(conn hostSocket, codec string, ready func(negotiatedCodec string, info AttachInfo) error) (uint64, bool, error) {
	s.mu.Lock()
	hasPrimary := !s.closed && (s.pc != nil || s.ws != nil || s.quic != nil)
	s.mu.Unlock()
	if !hasPrimary {
		generation, err := s.AttachWSHost(conn, codec, func(negotiated string, info AttachInfo) error {
			return ready(negotiated, info)
		})
		return generation, false, err
	}
	host := newWSHost(conn, codec)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		host.Close()
		return 0, false, fmt.Errorf("session is closed")
	}
	previous := s.wsStandby
	s.wsStandby = host
	s.wsStandbyFreshAt = time.Time{}
	// New standby instance: mint its identity token. The primary epoch is
	// NOT bumped (the primary is unchanged).
	token := newStandbyToken()
	s.wsStandbyToken = token
	s.mu.Unlock()
	if previous != nil && previous != host {
		previous.Close()
	}
	if ready != nil {
		s.mu.Lock()
		info := AttachInfo{Epoch: s.epoch, Standby: true, Token: token}
		s.mu.Unlock()
		if err := ready(host.Codec(), info); err != nil {
			s.DetachWSStandby(host)
			return 0, true, err
		}
	}
	if s.isStandbyWSHost(host) {
		go host.writer()
		// A standby attach is fresh-media proof for the standby path only:
		// the primary's recovery fencing is untouched.
		s.armStandbyFreshMediaProof()
	}
	return s.currentGeneration(), true, nil
}

// AttachQUICStandby attaches the QUIC DATAGRAM adapter in the dedicated
// QUIC hot-standby slot (build 63): it coexists with the current primary
// WITHOUT evicting it — the same standby contract as AttachWSStandby (no
// generation/epoch bump, mirrored downlink mix, inbound frames decoded for
// freshness and discarded). The returned bool reports the actual role:
// true = standby, false = the request degraded to a primary attach (only
// when the session has no primary yet, so the client always gets media).
// The ready callback's greeting travels the WSS control plane (the QUIC
// session carries no control channel).
func (s *Session) AttachQUICStandby(conn hostSocket, codec string, ready func(negotiatedCodec string, info AttachInfo) error) (uint64, bool, error) {
	s.mu.Lock()
	hasPrimary := !s.closed && (s.pc != nil || s.ws != nil || s.quic != nil)
	s.mu.Unlock()
	if !hasPrimary {
		generation, err := s.AttachWSHost(conn, codec, func(negotiated string, info AttachInfo) error {
			return ready(negotiated, info)
		})
		return generation, false, err
	}
	host := newWSHost(conn, codec)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		host.Close()
		return 0, false, fmt.Errorf("session is closed")
	}
	previous := s.quicStandby
	s.quicStandby = host
	s.quicStandbyFreshAt = time.Time{}
	// New standby instance: mint its identity token. The primary epoch is
	// NOT bumped (the primary is unchanged).
	token := newStandbyToken()
	s.quicStandbyToken = token
	s.mu.Unlock()
	if previous != nil && previous != host {
		previous.Close()
	}
	if ready != nil {
		s.mu.Lock()
		info := AttachInfo{Epoch: s.epoch, Standby: true, Token: token}
		s.mu.Unlock()
		if err := ready(host.Codec(), info); err != nil {
			s.DetachWSStandby(host)
			return 0, true, err
		}
	}
	if s.isStandbyWSHost(host) {
		go host.writer()
		// A standby attach is fresh-media proof for the standby path only:
		// the primary's recovery fencing is untouched.
		s.armStandbyQUICFreshMediaProof()
	}
	return s.currentGeneration(), true, nil
}

// armStandbyQUICFreshMediaProof marks the standby QUIC path unproven until
// its first genuine inbound frame (build 63 first-class slot).
func (s *Session) armStandbyQUICFreshMediaProof() {
	s.mu.Lock()
	s.quicStandbyFreshAt = time.Time{}
	s.mu.Unlock()
}

// DetachWSStandby removes a standby socket silently: the primary transport
// and the call lifecycle are never touched and no recovery episode opens.
// Used by the standby pump exit and the ready-greeting error path. Build 63:
// serves BOTH family standby slots (real WSS and the dedicated QUIC slot).
func (s *Session) DetachWSStandby(host *wsHost) {
	s.mu.Lock()
	if s.closed || (s.wsStandby != host && s.quicStandby != host) {
		s.mu.Unlock()
		if host != nil {
			host.Close()
		}
		return
	}
	if s.wsStandby == host {
		s.wsStandby = nil
		s.wsStandbyToken = ""
	} else {
		s.quicStandby = nil
		s.quicStandbyToken = ""
	}
	s.mu.Unlock()
	host.Close()
}

// detachWSByRole converges a dead socket in whichever role it currently
// holds: a PRIMARY close runs the standard host-lost lifecycle (recovery
// window); a STANDBY close is silent (the call and its primary are never
// touched); a socket already replaced in either slot only closes its own
// connection. The role is read at exit time because a hot-standby promotion
// may have flipped it mid-pump.
func (s *Session) detachWSByRole(host *wsHost, cause HostLossCause) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		host.Close()
		return
	}
	if s.ws == host || s.quic == host {
		generation := s.generation
		s.mu.Unlock()
		s.DetachWSHost(generation, host, cause)
		return
	}
	if s.wsStandby == host || s.quicStandby == host {
		s.mu.Unlock()
		s.DetachWSStandby(host)
		return
	}
	s.mu.Unlock()
	host.Close()
}

// PumpWSHost blocks serving one attached socket in EITHER role (primary host
// or hot standby — build 50): binary frames become host audio when the socket
// is the primary (paced into the shared jitter buffer with synthetic RTP
// timestamps) or are decoded for freshness and discarded when it is the
// standby; any read failure detaches THIS socket in its CURRENT role — the
// primary path applies the Manager's host-lost/grace lifecycle, the standby
// path closes silently and never touches the call.
//
// Identity fence at entry and per message: the pump looks the socket up by
// conn in whichever slot currently holds it, so a hot-standby promotion that
// flips its role mid-pump is picked up on the next message without a new
// pump. A replaced/closed socket is observed as absent and the pump exits
// having touched nothing else.
func (s *Session) PumpWSHost(conn hostSocket, generation uint64) {
	host := s.wsHostByConn(conn)
	s.mu.Lock()
	valid := host != nil && !s.closed
	s.mu.Unlock()
	if !valid {
		_ = conn.Close()
		return
	}
	readErr := error(nil)
	defer func() {
		s.detachWSByRole(host, wsCloseCause(readErr))
	}()
	var timestamp uint32
	// Framed-Opus uplink sequence state (seeded by the first frame). Shared
	// across roles: the socket's uplink sequence space is continuous across a
	// standby→primary promotion, so the depacketizer cursor must survive it.
	var upSeq uint16
	var upHaveSeq bool
	wasStandby := false
	if wc, ok := conn.(interface{ SetReadLimit(int64) }); ok {
		wc.SetReadLimit(4096)
	}
	lastArrival := time.Now()
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			readErr = err
			return
		}
		// Live role + buffer ownership for THIS message: a promotion or a
		// concurrent replacement lands between messages, never mid-message.
		s.mu.Lock()
		if s.closed || (s.ws != host && s.wsStandby != host &&
			s.quic != host && s.quicStandby != host) {
			s.mu.Unlock()
			return
		}
		standby := s.wsStandby == host || s.quicStandby == host
		quicSlot := s.quic == host || s.quicStandby == host
		jitter := s.host
		gen := s.generation
		s.mu.Unlock()
		if standby != wasStandby {
			// Role transition: the (possibly freshly recreated) host jitter
			// owns a synthetic timeline that starts at 0. BOTH directions
			// reset: an ex-standby promoted to primary AND a demoted primary
			// re-promoted later (Q→WSS→Q) must not leak the previous role's
			// timestamps into the new host buffer (they would be dropped as
			// stale and the promoted socket would stay silent).
			timestamp = 0
		}
		wasStandby = standby
		now := time.Now()
		gapMs := int(now.Sub(lastArrival).Milliseconds())
		lastArrival = now
		if gapMs > 0 {
			host.noteUplinkGap(gapMs)
		}
		if messageType == websocket.TextMessage {
			// App-level ping/pong for honest end-to-end RTT sampling on the
			// WSS path (transport pings do not reach the app layer). Pongs
			// are written by the host's writer goroutine, never here. The
			// ping may carry the app's playback-buffer depth and worst
			// inter-arrival gap (build 27) — the freshest delay evidence the
			// TCP path produces — and the pong answers with this socket's
			// uplink evidence so the app can adapt its own encoder.
			var control struct {
				Type string `json:"type"`
				T    int64  `json:"t"`
				Buf  int    `json:"buf,omitempty"`
				Gap  int    `json:"gap,omitempty"`
			}
			if json.Unmarshal(payload, &control) == nil && control.Type == "ping" {
				host.noteAppStats(control.Buf, control.Gap)
				upBuf, upGap := host.uplinkEvidence()
				jitter.mu.Lock()
				hostDepth := len(jitter.frames)
				jitter.mu.Unlock()
				host.noteUplinkBuf(hostDepth)
				if pong, marshalErr := json.Marshal(map[string]any{
					"type": "pong", "t": control.T,
					"buf":  upBuf,
					"ugap": upGap,
				}); marshalErr == nil {
					host.enqueueText(pong)
				}
				continue
			}
			// Build-48 hot denoise toggle: {"type":"denoise",
			// "uplink":bool,"downlink":bool}. Applies at the next frame
			// boundary to this owned session (gate-only; state preserved).
			// Older servers simply ignore the unknown type.
			var dn struct {
				Type     string `json:"type"`
				Uplink   *bool  `json:"uplink"`
				Downlink *bool  `json:"downlink"`
			}
			if json.Unmarshal(payload, &dn) == nil && dn.Type == "denoise" && (dn.Uplink != nil || dn.Downlink != nil) {
				// Omitted direction keeps its current gate. Applies in
				// EITHER role: denoise is session policy, and a standby
				// socket is an attached, authorized transport of this call.
				up, down := s.currentDenoise()
				if dn.Uplink != nil {
					up = *dn.Uplink
				}
				if dn.Downlink != nil {
					down = *dn.Downlink
				}
				s.SetDenoise(up, down)
			}
			// Build-50 hot standby: {"type":"promote","token",
			// "expectEpoch","nonce"} received ON the standby socket requests
			// the fenced atomic role swap (this socket becomes the primary;
			// the previous primary is demoted into the standby slot). On the
			// primary socket it is a harmless no-op. A fencing failure
			// (stale epoch / wrong token) leaves every role untouched.
			var pm struct {
				Type        string `json:"type"`
				Token       string `json:"token"`
				ExpectEpoch uint64 `json:"expectEpoch"`
				Nonce       string `json:"nonce"`
				Transport   string `json:"transport,omitempty"`
			}
			if json.Unmarshal(payload, &pm) == nil && pm.Type == "promote" && standby && pm.Transport == "" {
				if outcome, err := s.PromoteStandby(StandbyKindWS, pm.Token, pm.ExpectEpoch, pm.Nonce); err == nil {
					if reply, marshalErr := json.Marshal(map[string]any{
						"type": "promoted", "epoch": outcome.Epoch,
						"nonce": pm.Nonce, "primary": outcome.Primary,
						"standby": outcome.Standby,
					}); marshalErr == nil {
						host.enqueueText(reply)
					}
				}
			}
			// Build 61/62/63 — QUIC standby promote issued from an
			// authenticated WSS control socket (the QUIC audio session
			// carries no control; the reliable WSS plane drives ownership).
			// Both socket roles may carry it: after a QUIC adoption the WSS
			// plane is DEMOTED to the standby role (protocol §7 control
			// separation), and it remains the same device-authenticated
			// channel. The token pins the exact standby instance and
			// expectEpoch fences racing swaps; the nonce keeps retries
			// idempotent. Exactly-once: the session's promote-nonce map
			// records the outcome, so a duplicated or delayed command cannot
			// swap twice. Build 63: the dedicated QUIC slots make "quic" a
			// first-class promote kind — the command can no longer be
			// confused with a ws-family swap.
			if json.Unmarshal(payload, &pm) == nil && pm.Type == "promote" && pm.Transport == "quic" {
				if outcome, err := s.PromoteStandby(StandbyKindQUIC, pm.Token, pm.ExpectEpoch, pm.Nonce); err == nil {
					if reply, marshalErr := json.Marshal(map[string]any{
						"type": "promoted", "epoch": outcome.Epoch,
						"nonce": pm.Nonce, "primary": outcome.Primary,
						"standby": outcome.Standby,
					}); marshalErr == nil {
						host.enqueueText(reply)
					}
				}
			}
			continue
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		if standby {
			// Standby uplink: decode the genuine frame for freshness and
			// DISCARD it — a standby transport must never feed the host mix
			// (the single-uplink invariant holds even if the client sends
			// real audio on the standby path). Build 63: the QUIC standby
			// freshness clock is its own (first-class slot).
			noteFresh := s.noteStandbyWSFresh
			if quicSlot {
				noteFresh = s.noteStandbyQUICFresh
			}
			if host.Codec() == codecOpus {
				if len(payload) < 3 {
					continue
				}
				seq := uint16(payload[0])<<8 | uint16(payload[1])
				body, _, consume := wsDepacketize(&upSeq, &upHaveSeq, seq, payload[2:])
				if !consume {
					continue
				}
				if _, decodeErr := host.decodeFrame(body); decodeErr == nil {
					noteFresh()
				}
				continue
			}
			if len(payload) != frameBytes {
				continue
			}
			if _, decodeErr := host.decodeFrame(payload); decodeErr == nil {
				noteFresh()
			}
			continue
		}
		if host.Codec() == codecOpus {
			// Framed Opus uplink: [seq u16 BE][payload]. The sequence
			// counts media slots, so sender-side drops (bounded-queue
			// stale drops, encoder failures) appear as holes the receiver
			// conceals with mature PLC — the decoder timeline stays
			// aligned and a one-slot gap can recover via RFC 7587 inband
			// FEC from the following packet.
			if len(payload) < 3 {
				continue
			}
			seq := uint16(payload[0])<<8 | uint16(payload[1])
			body, missed, consume := wsDepacketize(&upSeq, &upHaveSeq, seq, payload[2:])
			if !consume {
				continue
			}
			// Staleness decision for this batch BEFORE any decode: a
			// burst arriving past the playout deadline is discarded
			// without touching the stateful decoder.
			s.DropStaleHostAudio(gen, timestamp)
			if jitter.behindPlay(timestamp) {
				continue
			}
			for i := 0; i < missed; i++ {
				var slot []int16
				// One-slot gap: try RFC 7587 inband FEC from the carrier
				// BEFORE normal decode (the carrier decodes normally right
				// after); fall back to PLC when no FEC data is embedded.
				if missed == 1 {
					if recovered, fecErr := host.decodeFEC(body); fecErr == nil {
						slot = recovered
					}
				}
				if slot == nil {
					recovered, plcErr := host.decodePLC()
					if plcErr != nil {
						recovered = nil
					}
					slot = recovered
				}
				s.hostPush(gen, timestamp, slot)
				timestamp += voice.FrameSamples
			}
			pcm, decodeErr := host.decodeFrame(body)
			if decodeErr != nil {
				continue
			}
			s.hostPush(gen, timestamp, pcm)
			// A genuinely received (not PLC-concealed) frame proves this new
			// socket carries real media — this is what clears recovery.
			s.hostFrameArrived(gen)
			timestamp += voice.FrameSamples
			continue
		}
		if len(payload) != frameBytes {
			continue
		}
		ts := timestamp
		timestamp += voice.FrameSamples
		// A send stall lets the play cursor run ahead of the synthetic
		// timestamps; without the reset every post-stall frame would be
		// dropped as "behind the cursor" and audio would never resume.
		s.DropStaleHostAudio(gen, ts)
		if jitter.behindPlay(ts) {
			continue
		}
		pcm, decodeErr := host.decodeFrame(payload)
		if decodeErr != nil {
			continue
		}
		s.hostPush(gen, ts, pcm)
		s.hostFrameArrived(gen)
	}
}

// wsCloseCause maps a socket read error to a host-loss cause. A policy
// violation carrying "revoked" (the daemon's device-revocation close frame)
// is terminal: it must bypass the recovery window.
func wsCloseCause(err error) HostLossCause {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr.Code == websocket.ClosePolicyViolation {
		return HostLossAuthRevoked
	}
	return HostLossRecoverable
}

// DropStaleHostAudio clears the host jitter buffer when a transport frame
// arrives whose timestamp already fell behind the play cursor (a client-side
// send stall let the cursor advance past it). The next frames re-prime the
// buffer; without this the session would play silence forever.
func (s *Session) DropStaleHostAudio(generation uint64, timestamp uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || generation != s.generation {
		return
	}
	if s.host.behindPlay(timestamp) {
		s.host.clear()
	}
}

// PingWSHost keeps NAT/proxy mappings alive on idle audio. A failed ping
// converges this exact socket (close), so the reader exits, the host
// detaches and the host-lost grace applies. It returns a stop function; the
// caller stops it when the socket detaches.
func PingWSHost(conn *websocket.Conn, interval time.Duration) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	return func() { close(stop) }
}
