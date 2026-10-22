# Call quality management — current implementation and honest boundaries

This document describes what the current source tree actually implements for call audio,
and where the honest limits are. It is a public reference: no field logs, no private
deployment details, no per-call timestamps. The sources of truth are the app's media stack
under `CallRelay/WebRTC/` + `CallRelay/Calling/` and the gateway's
`gateway/internal/unified/media/` + `gateway/internal/opus/`. Tests quoted here are
synthetic; audibility on a physical phone is a deployment-side acceptance gate, not a claim
of this document.

Related: [`PROTOCOL.md`](../PROTOCOL.md) (wire contracts), [`architecture.md`](architecture.md)
(repository layout), [`../gateway/docs/media-routing-contract.md`](../gateway/docs/media-routing-contract.md),
[`../gateway/docs/quic-datagram-media-protocol.md`](../gateway/docs/quic-datagram-media-protocol.md),
[`../gateway/docs/recovery-denoise-contract.md`](../gateway/docs/recovery-denoise-contract.md).

## 1. The audio chain, and where quality is bounded

Audio has two hops with different physics:

| Hop | What it is | Rate |
|---|---|---|
| Phone ↔ gateway (network hop) | Opus with PCMU fallback over three possible transports (below) | 8 kHz mono, 16–64 kbps adaptive |
| Gateway ↔ PSTN (modem hop) | QDC507-family modem / UAC audio, G.711 to the carrier | fixed 8 kHz G.711 |

Because the modem leg is fixed narrowband, the **end-to-end audible bandwidth is narrowband**
even though the network hop uses Opus. The network-hop work buys loss resilience and
congestion behaviour, not wideband sound. A 16 kHz / AMR-WB wideband path would require the
modem, gateway and app to be upgraded and verified together; it is **not** claimed here.

The three network transports:

1. **WSS relay — the guaranteed path.** An authenticated WebSocket on the gateway's HTTPS
   route. Each binary message is one 20 ms frame: with `codec=opus` negotiated in the attach
   URL the gateway's `ready` control announces the accepted codec and `fmt=seq16`; frames are
   then `[seq u16 BE][Opus payload]` (`CallRelay/WebRTC/WSSFrameCodec.swift`). The sequence
   counts 20 ms **media slots**, not sent frames: when the bounded send queue drops a stale
   frame (or an encode fails), the hole stays visible on the wire, so the receiver runs
   libopus PLC for exactly the missing slots and the stateful decoder's timeline stays
   aligned. A one-slot gap recovers from the following packet's RFC 7587 inband FEC. A
   pre-Opus gateway answers PCMU and the legacy bare 160-byte frame protocol is unchanged;
   old gateways ignore `codec=opus` safely.
2. **Direct WebRTC (ICE offer/answer).** The WebRTC SDK carries Opus (payload type 111,
   RFC 7587) with PCMU as the wire-compatible fallback; the app-side SDP filter preserves the
   NACK/transport-cc feedback lines for the fallback payload. NACK retransmission runs in
   both directions, with a bounded playout deadline: frames whose RTP timestamp already fell
   behind the play cursor are dropped before the jitter buffer, duplicates are idempotent.
3. **QUIC DATAGRAM direct (capability `mediaQuic`).** An audio-only datagram session to the
   gateway's QUIC listener; **all** critical control (bind authorization, ready, promote,
   denoise, bye, ping/pong) stays on the existing authenticated WSS plane, correlated by
   call + device + epoch (`../gateway/docs/quic-datagram-media-protocol.md`). The transport
   reuses the same codec, seq16 framing, playback scheduler and RTT discipline as the relay
   (`CallRelay/WebRTC/QUICCallMedia.swift`). The phone discovers its own server-reflexive
   UDP candidate with a minimal RFC 8489 STUN binding client
   (`CallRelay/WebRTC/QUICSTUN.swift`) against the STUN services the gateway already
   publishes. TLS trust is a SPKI-SHA256 pin signed by the pairing identity; a pin without a
   matching signature is a hard failure **before any QUIC dial** — never trust-all
   (`CallRelay/Identity/QUICPinTrust.swift`, document shape from
   `GET /api/v2/media/quic`).

Codec provenance: reference libopus **1.5.2** (BSD-3-Clause) — the app links the vendored
`Vendor/opus` xcframework, and the gateway builds the same reference library from
`../gateway/third_party/opus` via `../tools/build-libopus.sh`. Licensing copies:
`Vendor/opus/COPYING` and `../gateway/third_party/opus/COPYING`.

## 2. Congestion response — what actually closes the loop

TCP hides loss as delay, so the relay and the direct path use different, transport-appropriate
signals. All controllers are bounded, cooldown-gated and **hold, never raise, on stale or
absent feedback**.

- **Direct WebRTC downlink (gateway → phone):** pion `pkg/gcc` send-side bandwidth estimation
  consumes the TransportLayerCC feedback the phone's SDK emits (the gateway stamps outbound
  packets with the negotiated TWCC sequence number); the estimate drives `OPUS_SET_BITRATE`
  inside 16–64 kbps with both-threshold hysteresis (≥ 4 kbps **and** ≥ 15 %) and an 800 ms
  cooldown. Receiver Reports (fraction lost as measured by the phone) drive
  `OPUS_SET_PACKET_LOSS_PERC` (0–30, ±2 hysteresis) and inband FEC on/off hysteresis
  (on ≥ 3 %, off ≤ 1 %, 5 s cooldown). TWCC and RR freshness are tracked separately so a
  healthy RR stream cannot keep a dead bandwidth estimate alive.
- **Direct WebRTC uplink (phone → gateway):** the phone SDK runs its own sender-side
  estimator from the gateway's TWCC feedback. Gateway NACK **requests** are token-bucketed
  (direction-local — no downlink estimate asserts anything about the uplink).
- **WSS relay downlink:** the gateway steps its Opus bitrate (16–64 kbps, 8 kbps steps,
  800 ms cooldown, 3 s clean-window requirement) from its own socket queue pressure and the
  app's ping evidence (`buf` = playback-buffer frames, `gap` = worst inter-arrival gap in
  ms) — the freshest end-to-end delay evidence a TCP path produces. FEC enables on sustained
  gaps ≥ 60 ms and disables below 30 ms.
- **WSS relay uplink:** the pong answers with the gateway host-buffer depth (`buf`) and the
  observed uplink gap (`ugap`); the app's
  [`WSSBitrateController`](../CallRelay/WebRTC/WSSBitrateController.swift) applies the same
  bounded discipline (16–64 kbps, initial 32 kbps, 8 kbps steps, 0.8 s cooldown, 3 s clean
  window, 10 s feedback staleness) and additionally raises `OPUS_SET_PACKET_LOSS_PERC` to
  10 % **only while FEC is enabled**, so inband redundancy actually exists to recover from.
- **PCMU sessions have no bitrate controller.** G.711 is fixed-rate; claiming adaptive
  control for it would be branding. They keep NACK/RR loss handling only.
- **Pacing:** audio is emitted at the 20 ms mixer tick (self-paced). The gcc leaky-bucket
  pacer is deliberately replaced by a NoOp pacer: at 16–64 kbps its per-tick budget is
  smaller than one audio packet, so it would queue packets without bound
  (`../gateway/internal/unified/media/controller.go`).

## 3. Playout, jitter and loss concealment

- **App WSS playback** (`CallRelay/WebRTC/WSPlaybackScheduler.swift`): the playback target
  follows the measured inter-arrival spacing (EWMA), bounded to **4–12 frames = 80–240 ms**;
  bursts past the high-water mark are caught up by trimming to the target (bounded
  accumulated delay); a 50-frame hard cap remains as the safety bound.
- **App WSS concealment:** a genuine underrun while recent real audio played repeats the last
  **scheduled** frame (playback timeline, never the newest queued input) with exponential
  decay and sign alternation (anti-buzz), capped at 5 ticks per chain. This is **not NetEq**
  and not G.711 Appendix I — it is an honestly-labeled basic fallback. Silence clears the
  replay seed, so an underrun during true silence can never replay stale speech.
- **Gateway host path:** timestamp-aware host jitter with a 120 ms target; late frames are
  dropped before the buffer (same deadline rule as the WSS path); duplicates are idempotent
  and depth stays ≤ target.
- **Remaining honest item:** a receiver-side NetEq-class buffer is still the open piece of
  work. The gateway already uses mature libopus PLC for gaps; the app's bounded fallback plus
  PLC remains the interim until field evidence says it underperforms.

## 4. Measured RTT is not the media queue

Two different numbers, deliberately kept apart:

- **RTT** is measured by the dedicated measurement socket
  ([`WSMediaMeasure`](../CallRelay/WebRTC/WSMediaMeasure.swift)): text ping/pong on
  `GET /api/v2/calls/{id}/media/measure`. It never attaches to the host session and never
  carries audio, so the relay RTT stays measurable even while a direct transport is live.
  The QUIC path samples RTT on its own arrival clock; the direct WebRTC path is measured by
  the SDK.
- **Queue/backlog** is an estimate produced by the transport layer (playback buffer depth or
  socket queue), consumed by the route policy as one additive score term. Presenters must
  label queue-derived numbers as **（估计）**; they are not a measurement of one-way media
  latency.

Neither number is presented as "audio latency"; the UI shows connection state, measured RTT
and the clearly-labeled queue estimate only.

## 5. Automatic routing: dual path, stable switching, 45 s recovery

`RouteSelectionPolicy` (`CallRelay/Calling/RouteSelectionPolicy.swift`) scores three
candidates: `quicDirect`, `webRTCDirect`, `wssRelay`. The relay is the guaranteed path — it
is selectable even when no public candidate exists — but **direct is not assumed to be
faster**. Selection is evidence-driven:

- A candidate only counts as media-proven with **genuine bidirectional media** evidence;
  echo/data-channel reachability and `.connected`/sole RTT do not count.
- Low confidence inflates the effective score, so unproven paths lose to proven ones; when a
  path cannot measure loss, the loss term is **excluded** and the candidate's confidence is
  discounted instead of being credited with a perfect record.
- **Stable switching (anti-flap):** a challenger must beat the incumbent by more than the
  hysteresis fraction (15 %), hold at least 3 gains over **distinct** samples within a 12 s
  window, and the streak must span at least 4 s. The incumbent keeps a minimum hold of 20 s
  after a confirmed adoption, and at least 15 s must pass between two confirmed switches. A
  sample older than 10 s is not fresh; an outstanding proposal expires after 8 s; candidates
  below 0.4 confidence are not selectable.
- **User modes are strict:** 自动 = full scoring; 直连+自动 = direct family only (never the
  relay); forced relay/QUIC/WebRTC never crosses routes, and an unavailable forced choice
  keeps the current route rather than silently moving.

**Hot standby (capability `mediaStandby`)** keeps the non-primary transport warm: the gateway
mirrors the downlink mix onto the standby transport, so promotion needs no connection setup.
The client side ([`HotStandbyController`](../CallRelay/Calling/HotStandbyController.swift))
owns freshness proof, the fenced promote command (token + expected epoch + idempotent
nonce), single-owner audio handover and bounded rollback to the still-warm demoted transport.
Honest boundary: connection reuse is **not** a zero-audio-gap guarantee — the mix source
swaps before the client ack and the audio-owner transfer takes real time. The automated
evidence is exactly one render-source switch, stale-queue discard (no replayed frames),
continuous paced frame IDs and a measured switch window in frames; audibility remains a
physical gate. Conference media does not offer standby at all (the gateway rejects the
parameter with 409).

**Bounded recovery** ([`MediaRecoveryPolicy`](../CallRelay/Calling/MediaRecoveryPolicy.swift)):
one **absolute 45 s window** from the first media loss (later loss events never extend it);
attempts run immediately, then 1, 2, 4, 8, 8… seconds. Auto mode tries the current path
first, then the alternate; forced modes never cross routes. Recovery succeeds **only** on
fresh inbound media after an attempt (never on `.connected`/RTT alone). Revoked
authorization and explicit local/remote end are terminal immediately, and the coordinator
never redials a new call — the same call id only.

## 6. Denoise switches — the actual defaults

- The gateway applies **directional** SpeexDSP preprocessing: uplink = local microphone,
  downlink = remote/modem leg; the capability `audioDenoise` advertises both directions
  (`../gateway/internal/unified/runtime/denoise.go`).
- **Default is OFF in both directions.** A build once defaulted these switches ON, but that
  was abandoned: authorized human-speech quality evidence could not be obtained (synthetic
  fixtures are not acceptance), so the honest default is off with both switches fully usable.
- An explicit per-device preference always wins. The app stores it per gateway
  ([`DenoisePreferenceStore`](../CallRelay/Settings/DenoisePreferenceStore.swift)) and syncs
  the same choice to the gateway's per-device `preferences`; the media attach carries the
  optional per-attach override, which wins for that attach only. Keys are gateway-scoped and
  the server persists per device, so two phones (or the PWA) never share a switch.

## 7. What the evidence covers — and what it does not

Covered by synthetic tests over the real code paths:

- Gateway (`../gateway/internal/unified/media/`): `TestWSDepacketizeGapMath`,
  `TestWSSUplinkSenderDropsConcealed`, `TestWSSDownlinkBlockedWriterExposesDrops`,
  `TestWSSUplinkDelayBurstResumes`, `TestWSSOpusFECDrivenByLossPerc`; Opus round-trip /
  PLC / CTL bounds; closed-loop component tests with real `gcc.SendSideBWE` + libopus + a
  real TWCC recorder; controller clamp/hysteresis/cooldown/stale-hold/NACK-budget tests.
- App (`CallRelayTests/`): frame-codec round-trip and gap/PLC/FEC/duplicate/burst caps;
  `RouteSelectionPolicyTests` / `RouteSelectionPolicyReviewTests` decision matrices and
  anti-flap gates; `MediaRecoveryPolicyTests` and recovery fencing; `HotStandbyControllerTests`;
  `QUICDirectControllerTests`, `QUICSTUNTests`, `QUICPinTrustTests`; WSS bitrate controller
  tests; denoise preference tests.

Not proven here (deployment-side field gates):

- Audibility and adaptation under real cellular loss/jitter, direct-vs-relay performance on a
  specific network, the audible gap of a hot-standby switch, Echo/Bluetooth/speaker routing,
  APNs lock-screen wake-up, and Wi-Fi ↔ cellular transitions during a call.
- No claim that direct is always faster, no "zero drop" or "zero gap" promise, and no TURN
  dependency: TURN credentials from the ICE endpoint are optional short-lived assists, the
  authenticated WSS relay is the guaranteed fallback, and QUIC direct availability depends on
  the deployment's QUIC listener (`GET /api/v2/media/quic` answers `enabled:false` when
  absent, in which case it is simply not a candidate).
