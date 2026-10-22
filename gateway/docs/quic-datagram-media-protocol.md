# CallRelay QUIC DATAGRAM Direct Media — Build 61 Protocol & Design

Status: build 61 FINAL contract (2026-10-14, post native-interop review).
This document is the single source of truth for the QUIC direct audio path
between the CallRelay iOS app and the gateway. It is additive:
ICE/WebRTC direct and the authenticated WSS relay are unchanged and remain
the fallback matrix.

## 0. Session model (decided by native interop evidence)

The QUIC connection is an **audio-only datagram session**. ALL critical
control (bind authorization, ready, promote, denoise, bye, ping/pong RTT
reporting) travels on the **existing authenticated HTTPS/WSS control
plane**, bound to the QUIC socket by call + device + generation/epoch.

Why (primary sources, no repeated probing):
- WWDC22 session 10078 documents exactly ONE datagram flow per QUIC
  connection (`NWProtocolQUIC.Options.isDatagram` / `maxDatagramFrameSize`).
- WWDC21 session 10094 documents `NWMultiplexGroup`; a current
  library-author repro (github.com/DitchOoM/socket issue 173, June 2026)
  reports that the EXTRACTED datagram flow misroutes inbound
  peer-initiated stream bytes and suppresses newConnectionHandler. Our own
  macOS↔quic-go probe (2026-10-14) additionally observed two `NWConnection`
  objects to the same endpoint arriving as TWO QUIC sessions (two source
  ports, `ACCEPTED 1`/`ACCEPTED 2`) and the tested NWConnectionGroup
  extraction setup failing — a failed setup is evidence about that setup,
  not a universal platform claim.
- Conclusion: one datagram flow per session is the documented, tested
  reality; stream+datagram multiplexing on one client session is NOT
  relied upon. Ownership-critical control therefore stays on the proven
  reliable WSS plane — no invented unreliable critical controls.

## 1. Goals and non-goals

Goals:

- A **direct** QUIC DATAGRAM (RFC 9000 + RFC 9221) audio transport
  between iOS and the gateway, tried automatically when advertised, with
  WebRTC-direct and WSS-relay as automatic fallbacks.
- **Public P2P without VPN**: STUN-based UDP hole punching to the
  gateway's server-reflexive candidate using the already-configured STUN
  services. The user does NOT connect a VPN daily; a private-LAN-only
  endpoint is not the product outcome.
- Reuse of the existing Opus 20 ms seq16 wire framing, per-call/per-device
  authorization, epoch/generation fencing, hot standby, and the 45 s
  host-lost recovery grace — no parallel media state machine.

Non-goals:

- No QUIC relay through FRP, no new public listening ports on the FRP
  server, no server-side changes, no FRP config changes.
- No new codec; no reliable-stream audio; no control-over-QUIC-stream.
- No claims of NAT traversal universality. Symmetric NAT on either side
  can defeat UDP punching; failure converges to the existing
  direct-WebRTC/WSS-relay decision tree with no user-visible hangup.

## 2. Transport

- UDP, QUIC client→gateway, ALPN `callrelay-media`, TLS 1.3 with
  identity-bound SPKI pin (see §6).
- The client creates ONE `NWConnection` with `NWProtocolQUIC.Options`
  `isDatagram = true` (one datagram flow — the documented mode).
- UDP payload multiplexing (first byte):
  - `0x01` audio envelope: `[0x01][epoch: u64 BE][seq16 WSS frame]`.
  - `0x03` bind: `[0x03][token]` — repeated in short bursts until
    downlink audio flows (token is single-use; post-bind bursts are
    ignored server-side, so lossy retransmits cannot authorize twice).
  - `0x04` RTT probe: `[0x04][t: u64 BE]` — echoed verbatim by the peer.
    Loss-tolerant measurement only; ownership decisions never ride on it.
- No other frame types exist. Anything else is dropped and counted.

## 3. Control plane (existing authenticated WSS media socket)

The WSS relay socket (already attached for the call) is the control
channel for the QUIC media path:

- Client → gateway (text frames on the WSS socket), all existing message
  types unchanged: `ping` (RTT sampling), `denoise`, `bye`.
- Gateway → client: `pong`, plus the QUIC-ready greeting below.
- QUIC attach acceptance is announced on the WSS socket:
  `{"type":"ready","callId":…,"codec":"opus","fmt":"seq16","transport":"quic","standby":true,"epoch":N,"token":"…"}`.
- Promotion of the QUIC standby to primary is the EXISTING fenced
  standby-promote command, sent on the WSS **primary** socket with the
  transport marker: `{"type":"promote","transport":"quic","token":"…","expectEpoch":N,"nonce":"…"}`
  → `{"type":"promoted","epoch":N',"nonce":"…","primary":"…","standby":"…"}`.
  Token pins the standby instance, expectEpoch fences racing swaps, and
  the session promote-nonce map makes retries idempotent (exactly-once
  ownership is the session's existing machinery; the QUIC path reuses it
  verbatim).
- Client-side readiness rule: a QUIC path is usable only after the WSS
  `ready transport=quic` greeting AND genuine inbound audio datagrams
  (fresh-media proof), both in the current epoch.

## 4. Audio datagrams and epoch fencing

- Envelope `0x01`: `[epoch: u64 BE][seq16 WSS frame bytes]`. The inner
  bytes are EXACTLY the WSS binary frame (Opus `fmt=seq16` or bare PCMU);
  after envelope validation the receive path is byte-identical to WSS.
- Both sides bind the expected epoch to the current host role:
  - Gateway: stamps/validates against the session primary-role epoch
    (captured at attach, updated atomically on promote) — the socket
    adapter validates at intake AND re-validates at dequeue (a queued
    frame can never be relabelled into a new epoch by a promote race).
  - Client: stamps/validates against the epoch from the QUIC `ready`
    greeting (updated on `promoted`).
- Rejects (drop + count, never reach the codec): wrong version, epoch
  older than current (across re-attach/promote/network change),
  pre-bind/pre-attach audio, superseded-connection frames.
- Gateway drops any datagram before the bind token is consumed.
- Max datagram: envelope + payload ≤ 1200 B and ≤ the QUIC path's usable
  datagram frame size.

## 5. Public P2P: candidates and hole punching

1. Gateway keeps ONE UDP socket for STUN + QUIC (demuxed), so the srflx
   candidate names the exact port the QUIC listener answers on. STUN
   refresh ~20 s; candidate TTL 60 s.
2. Signalling (existing authenticated /api/v2 over FRP HTTPS):
   - `GET /api/v2/media/quic` → `{enabled, alpn, pin:{…}, candidates:[{type:"srflx"|"host", proto:"quic", addr, expiresAtMs}], stun:[…]}`.
   - `POST /api/v2/media/quic/warm` → `{warmToken}` (single-use, 2 min;
     min 30 s between issues per device).
   - `POST /api/v2/calls/{id}/quic/candidate` body
     `{candidate:"ip:port", networkEpoch:"…"}` →
     `{punchToken, gatewayCandidate, punchAtMs, deadlineMs}`. Candidate
     validation: literal IP:port only; loopback/multicast/reserved/
     documentation/benchmarking/CGNAT/low ports/zone-ids rejected;
     private targets only under the explicit `lan_punch` scope (default
     off — LAN direct is client-initiated, no server punch to private
     addresses). Bounds: per device 4/min, per call 3/10 min, global
     concurrency 4; token single-use, TTL = punch window (8 s), tied to
     the live call grant (attach re-verifies call liveness + device
     authorization).
3. Punching: within the coordinated window both sides send real QUIC
   Initials from the shared socket to the peer's candidate (client: its
   handshake; gateway: short-lived raw-UDP bursts — fixed 64 B payloads,
   no response expected, never a reflector/amplifier).
4. IPv6 preferred when the path is v6 and a v6 candidate exists;
   family mismatch is recorded, not fatal.

## 6. TLS and pinning

- Self-signed ECDSA P-256 certificate (persisted 0600 under the data dir,
  ~13-month validity, SAN = gateway id).
- `GET /api/v2/media/quic` (authenticated) returns the canonical signed
  pin document: version, gateway id, algorithm (`ecdsa-p256-spki-sha256`),
  base64 SHA-256 of the PKIX SPKI DER, NotBefore/NotAfter, and an Ed25519
  signature (gateway identity key) over the canonical text
  `callrelay-quic-pin-v1 || version || gatewayId || algorithm || spkiHash || notBefore || notAfter`.
- The client verifies the signature against the identity key it ALREADY
  trusts from the pairing (`GET /api/v2/identity`) — never against the
  co-returned untrusted copy — then pins the SPKI in its TLS verifier
  WITH full certificate-validity checks (NotBefore/NotAfter). Apple:
  `sec_protocol_options_set_verify_block`. No trust-all paths anywhere;
  wrong-pin and expired-certificate are hard handshake failures (tested
  on the real quic-go transport).

## 7. Gateway bounds

- Unauthenticated connections: ≤ 32 pending; bind timeout 5 s; per-IP
  auth attempts 4/10 s.
- QUIC audio session attaches ONLY in the standby role alongside an
  existing primary (WSS or ICE). Control ownership is separated from
  audio role: the WSS socket stays attached and bound as the reliable
  control plane even when its AUDIO role is demoted to standby (or QUIC/
  ICE owns audio); an audio promotion never severs control. If no primary
  exists, the attach is rejected.
- Punch/warm budgets (bounded; sized for the promised 45 s recovery):
  per device 8/min; per call 10 per rolling 10 min (four 8 s attempts in
  the window plus network-epoch headroom); global concurrency 4. A
  rate-limit answer is a budgeting signal, NEVER credential revocation:
  the client keeps its incumbent audio and retries inside the recovery
  grace. Warm probes echo the client's own [0x04] nonces verbatim (RTT
  uses only the client clock) and close — a closed probe is not a
  maintained warm media connection.
- Listener failure degrades to capability-absent (advertises nothing).

## 8. Client routing integration (iOS)

- Capability: `mediaTransports` gains `"quic"` and
  `capabilities.mediaQuic = true` only while the listener is live;
  `capabilities.quic` carries the pin/candidate document.
- Call start chooses the best already-warm usable path; there is no
  forced relay-audio detour. When a fresh media-proven warmed direct path
  (RTC or QUIC) exists, it is adopted directly. Where relay audio is
  used, it is a PROVISIONAL bootstrap: the provisional incumbent does not
  consume the policy's minimum-hold/cooldown budget, and the first proven
  better route may be adopted immediately (the hysteresis/min-hold
  discipline applies only to switching away from a confirmed adoption).
- Forced modes remain strict: using the WSS socket as the control plane
  is always fine; silently carrying FORCED direct/QUIC audio over the
  relay is never allowed.
- Call flow: the reliable WSS control channel establishes first (audio
  role per routing); QUIC info + candidate POST run in parallel; QUIC
  dial + bind bursts bounded (~8 s, never blocking the answer); the
  gateway attaches QUIC as STANDBY; the QUIC `ready` greeting arrives
  over the WSS socket with `transport:"quic"`; samples accumulate; the
  deterministic multi-candidate selection policy (RTT, jitter/reorder/
  loss, queue, availability, confidence; hysteresis/min-hold/cooldown/
  epoch freshness; two-phase propose→confirm) decides; promotion uses
  the existing WSS standby-promote command with `transport:"quic"`;
  demotion/failure falls back through the existing recovery machinery
  (45 s grace, 1/2/4/8 retries).
- Pre-call warm: `POST /media/quic/warm {candidate}` coordinates a
  bounded device-grant punch (a client-only Initial cannot establish
  fresh pre-call evidence under endpoint-filtering NAT); the probe
  echoes client-nonce RTT samples and closes. Warm measurements inform
  call-start ATTEMPT PRIORITY only — they are never call-time
  `mediaReady` proof, and a closed probe is not a persistent connection.
  The live call always re-binds a fresh owner (per-call punch token).
- Diagnostics show chosen path + numeric stats; queue-derived numbers
  labelled （估计）; selection reasons recorded redacted.

## 9. Testing status (factual; raw logs preserved)

Gateway — EXECUTED (this build):
- `go test -race ./internal/unified/... -count=1`: PASS, 14/14 packages,
  0 data-race reports (final log: /tmp/final-race-unified.log, exit 0).
- QUIC/media suites cover: full path (bind → standby attach →
  bidirectional envelope-stamped audio), wrong-pin + expired-cert
  negatives, bind negatives (unknown token, audio-before-bind, no-bind
  timeout), warm probe single-use + client-nonce echo, 30% one-way-loss
  path, punch validation matrix, registry lifecycle (no leaked/negative
  active counts), pin sign/verify/tamper, mixer integration (uplink→
  mixer→leg, downlink envelope stamping, standby decode-discard, fenced
  promote dropping old-epoch frames, superseded-socket fencing), adapter
  epoch gate + dequeue fence + empty-datagram + probe echo.
- One historical flake note: `TestHangupAfterAnswer…` failed once in an
  earlier transition run; baseline `go test -race -count=10` on the final
  source passes 10/10 (log /tmp/hangup-baseline.log). No ownership or
  convergence assertion was weakened.

iOS — EXECUTED (this build):
- Selection-policy suites + transport unit suites verified via standalone
  harness/typecheck (no xcodebuild yet); full XCTest execution happens in
  the build phase below.

iOS — PLANNED (build phase): xcodegen, device/simulator build, full
CallRelayTests run with raw xcresult retention.

Real phone (physical install + human listening acceptance): PENDING —
separate user gate; no claim of real-device audio is made here.

## 10. Honest limitations

- Hole punching through symmetric carrier NAT may fail; the shipped
  behaviour is a truthful "unavailable" with automatic fallback.
- Local/synthetic tests prove protocol correctness and media integrity;
  they do not prove real-world AU→CN audio improvement. No RTT-improvement
  claim is made without field measurement.
- The Mac↔gateway ICMP numbers (333 ms private, 166+28 ms relay estimate)
  are host-route estimates, not measured media-path RTT.
