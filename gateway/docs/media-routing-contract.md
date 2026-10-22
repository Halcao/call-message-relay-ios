# CallRelay Media Routing Contract — Auto/Direct/Relay (2026-10-03)

Status legend: **[existing]** already deployed on the running gateway;
**[final]** merged in this change and deployed by this job;
**[pending]** decision still open — do not build against it until resolved.

Scope: gateway (`callrelayd`) support for automatic P2P/WSS routing and
manual Auto/Direct/Relay selection. The **app owns the selection policy**
(when to probe, when to commit, hysteresis, forced-mode UX). The gateway
owns safe primitives: a detached probe that can never harm the healthy
path, an atomic ready-first commit, a non-intrusive measurement socket,
and graceful fallback. Backend build under test here: `callrelayd`
(unified v2 API).

Transport names used below:

- `ice` — WebRTC peer connection (SDP offer/answer over the existing HTTPS
  API; media flows over LAN UDP once connected — pion allocates ephemeral
  UDP ports for RTP/RTCP on the gateway's LAN addresses). Reachable only
  where the client can reach those ICE host candidates (LAN today: no
  STUN/TURN is configured or added by this change).
- `ws` — authenticated WSS binary PCMU socket (`GET /api/v2/calls/{id}/media`,
  20 ms frames, `{"type":"ready"}` first), served on the existing HTTPS
  listener. Always available; the only media path on cellular.
- `measure` — authenticated WSS **measurement-only** socket (§5); reuses
  the HTTPS listener, carries no audio and never affects the media path.

No new public ports, services, STUN/TURN entries or FRP changes: the only
listener serving this contract is the existing HTTPS one; direct media
uses LAN UDP discovered via ICE host candidates.

## Surface

### 1. Capability advertisement — **[existing]**

`GET /api/v2/calls/{id}/ice` →

```json
{
  "policy": "all" | "relay",
  "iceServers": [{"urls": [], "username": "", "credential": ""}],
  "mediaTransports": ["ice", "ws"]
}
```

### 2. Detached probe (measure a direct candidate safely) — **[final]**

`POST /api/v2/calls/{id}/media/probe`

Request: `{"sdp": "<full SDP offer, non-trickle, PCMU>"}` (PCMU 8 kHz mono,
same codec rules as the existing WebRTC offer). The client SHOULD create
its `callrelay-probe` DataChannel (§4) before generating the offer so the
candidate's app-level RTT is measurable without streaming microphone audio.

Response `201`:

```json
{"sdp": "<answer>", "type": "answer", "iceMode": "<policy>", "callId": "<id>"}
```

Semantics:

- The probe is a **detached echo session**: it is never registered in the
  call's session table, never opens the worker's media socket, never
  changes call state, and never touches the live media path. Probing
  cannot disturb a healthy WSS (or ICE) call.
- Measurement is the app-level DataChannel echo (§4): the client sends
  `{"type":"ping","tag":<n>}` and timestamps the exact echoed message —
  no microphone injection and no audible echo required. (The gateway also
  reflects streamed audio frames back for clients that do stream audio;
  the DataChannel echo is the canonical, non-audible mechanism.)
- The app compares the candidate's DataChannel echo RTT against the WSS
  path's app-level ping RTT (§5, same wire shape). Both numbers must be
  fresh samples of comparable paths; the app applies its own hysteresis.
  The gateway does **not** decide "direct is always faster" and never
  auto-switches.
- Attaching a second probe replaces the first (the old one is closed).
- Errors: 401 unauthenticated, 403 non-owner/forbidden, 404 unknown call,
  409 call still ringing or in voicemail, 410 line unavailable, 400 empty
  SDP, 502 negotiation failure (no session leaks).
- Lifetime: a probe expires after **45 s** if not committed or discarded.
  The app must commit or discard within that window; a `404` on commit
  means the probe expired (or was replaced) — just re-probe. Ending the
  call cancels any in-flight probe.

### 3. Commit (ready-first promotion, atomic) — **[final]**

`POST /api/v2/calls/{id}/media/probe/commit`

Request: empty body. Response `200`: `{"committed": "true"}` (string value
kept for wire compat with the already-released shape).

Guarantees:

- **Ready-first**: commit adopts the probe's peer connection only if it is
  still `connected` at commit time. A stale, disconnected, never-connected
  or superseded probe is rejected with `502 CB-V2-502`; **the previous
  live host (WSS or ICE) is left completely untouched**, so a failed
  candidate can never hang up or silence a call.
- **Atomic vs expiry**: the probe leaves the registry and its TTL timer
  stops before adoption; two concurrent commits can never adopt the same
  peer.
- **Generation-fenced**: adoption bumps the session generation; the old
  host transport is closed only after the new peer is installed, and a
  superseded socket's pump can never detach the new host.
- **Grace-cancelling**: a successful adoption fires host-restored, so a
  pending host-lost grace timer cannot hang up a call that just promoted.
- The `callrelay-probe` DataChannel keeps echoing on the adopted peer, so
  post-commit direct-path RTT monitoring stays available (§4).
- Errors: 404 no probe / call ended / probe expired; 403 non-owner;
  409 ringing/voicemail; **409 `CB-V2-409D` when the call is merged into a
  conference** (conference media hosts all legs on the conference session;
  per-call commit would double-attach the worker's single-owner media
  socket); 502 candidate not connected / adoption failure. Any commit
  failure leaves the previously active path live and returns an explicit
  failure code — no silent fallback, no implicit switch.
- Hold: commit is allowed on held calls; the leg's held state is
  preserved across adoption.

### 4. App-level RTT echo (DataChannel) — **[final]**

Wire shape (both directions): JSON text, `{"type":"ping","tag":<n>}` → the
gateway echoes the **exact same message** back.

- The **client** creates the data channel with label `callrelay-probe` on
  its offer peer connection before generating the SDP offer (both for
  probe offers and normal call offers). SDP offer/answer rules require the
  m=application section to exist in the offer; the gateway cannot add it
  from the answer side.
- The gateway answers the channel with a bounded echo: messages over
  1024 bytes are dropped, echoes are rate-limited (token bucket, ~50/s,
  burst 50) so a runaway client cannot amplify traffic.
- Because the echo handler lives on the negotiated peer connection, it
  keeps working unchanged after a probe is committed onto the live
  session — continuous direct-path RTT sampling on an active direct call.

### 5. Measurement-only socket (WSS baseline alongside ICE) — **[final]**

`GET /api/v2/calls/{id}/media/measure` (WebSocket upgrade, same auth as
media).

- Purpose: sample the WSS path's app-level RTT **while the call's media
  runs over ICE** (and vice versa) without touching the media path.
  Re-opening the normal media socket would replace the ICE host — this
  socket never does.
- Protocol: text JSON only. Client → `{"type":"ping","t":<ms>}`; server →
  `{"type":"pong","t":<ms>}` (same as the media socket's control channel).
  Binary audio frames are ignored; nothing is ever written toward the
  mixer. Multiple measurement sockets may coexist; they hold no session
  state.
- Lifecycle/auth: same pre-upgrade guards as media (401/403/404/409);
  the socket is closed by the gateway when the call ends (polled, ≤2 s).
  Reads are capped (4096 bytes) and pings rate-limited like the echo
  channel.

### 6. Discard — **[existing]**

`DELETE /api/v2/calls/{id}/media/probe` → `204`. Safe no-op when no probe
exists (retries cannot fail spuriously).

## Fallback / rollback (WSS restores grace) — **[final]**

Bidirectional handover is repeatable and app-driven:

- **WSS → ICE**: probe (§2) → commit (§3).
- **ICE → WSS** (fallback): open `GET /api/v2/calls/{id}/media` again. The
  attach replaces the current host transport (the adopted peer connection
  is closed), re-fires host-restored and cancels any host-lost grace
  timer, then serves `{"type":"ready"}` and resumes the identical PCMU
  frame protocol. A failed direct candidate can roll back mid-call without
  a hangup.
- **Grace honesty**: when the active host transport drops without a
  replacement (ICE disconnect/fail or socket death), the gateway starts
  the normal disconnect-grace timer and publishes `call.updated`; the
  client re-attaching WSS (or the peer reconnecting) inside the window
  cancels the grace.
- Auto mode: the app may fall back freely at any point. Forced modes: the
  app enforces "forced direct = no silent fallback" in its own UI; the
  gateway only guarantees every failure is explicit and never destroys the
  still-working path.

## Truthful transport reporting — **[final]**

`CallView` gains an optional `mediaTransport` field:

- `"ice"` — the session hosting this call's audio currently runs a
  WebRTC peer connection.
- `"ws"` — it currently runs the authenticated WSS socket.
- omitted — no client media transport attached yet.

For a call merged into a conference, the field reports the **conference
session's** transport (the path actually carrying that leg's audio), not
the absent per-call session. The app should reconcile its own routing
state with this value (e.g. a forced-direct session must not display
"direct" while the view reports `"ws"`). Informational only.

## Quality comparison rules (app-side, backend-supported) — **[final]**

- Direct-path RTT: DataChannel echo (§4), sampled continuously on the
  active peer.
- WSS-path RTT: ping/pong on the media socket while WSS is active, or on
  the measurement socket (§5) while ICE is active — both directions are
  measurable without disrupting media.
- Compare only fresh samples (app discards samples older than its own
  window), require several samples before deciding, and apply hysteresis
  before switching. The gateway never embeds a "direct is always faster"
  assumption and never auto-promotes.

## Out of scope / unchanged

- No new public ports/services, no STUN/TURN additions, no FRP changes.
  With the current (empty) ICE server list, direct media works on LAN
  paths only; cellular clients keep using WSS.
- Conference media stays on the conference session; per-call routing
  switches are rejected while merged (409 `CB-V2-409D` on both commit and
  the per-call media reattach — use the conference media route) rather
  than silently disabling conference controls. Probes and measurement
  sockets remain allowed on merged calls (they never touch the leg).
- Every commit attempt consumes the probe uniformly: success, rejection
  and failure all drop it (a 409/502 means re-probe before retrying).
- Call control (answer/hold/hangup/DTMF/conference) is unchanged and
  remains available during any routing phase.

## Pending choices (do not build against until resolved)

- **[pending] Forced-mode endpoint**: manual Auto/Direct/Relay is fully
  expressible with the primitives above (Auto = app policy over
  probe/commit/fallback; Direct = commit + treat rollback as explicit
  failure; Relay = never probe / re-attach WSS). A dedicated
  `PUT /calls/{id}/route` endpoint is **not** planned for this release —
  the app drives switching. If the iOS side needs server-enforced mode
  pinning (e.g. to survive app restarts mid-call), coordinator to confirm.
- **[pending] Server-computed stats**: no aggregated RTT/jitter/loss push;
  all sampling is client-side (echo + ping/pong). A `getStats`-style
  endpoint is a possible follow-up, not this release.

## Gateway build under test

`callrelayd` from this repository (cmd/callrelayd). Deployment evidence
(deployed binary hash + health checks) is kept privately by each operator
alongside their own rollout record; it is not distributed in this repo.
