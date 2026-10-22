# Build 48 — 45 s recovery, adaptive queues, directional denoise

Status: implemented; client 0.3.41 (48). All wire changes are optional and
backward compatible (old clients and old servers ignore what they do not
know). No QUIC/TURN/FRP/codec/ports change.

## 1. Recovery window (45 s)

- `settings.disconnect_grace_seconds` default 30 → **45**; DB override
  remains authoritative (console + `seedSetting`), clamp 5..600.
- The window is one ABSOLUTE deadline from the FIRST loss event; repeated
  loss callbacks neither restart nor extend it. Timer callbacks are
  generation/identity fenced (`runtime/recovery.go`): a recovered call can
  never be ended by a stale deadline; the deadline-vs-restore race is
  serialized per episode (`recoveryDeadlineFired` / `resolveRecovery`).
- Recovery clears only on **fresh-media proof** — the first genuinely
  received (non-PLC) inbound frame on the new/reattached transport — not on
  attach/SDP/ICE CONNECTED and not on an old connection's RTT.
- Revoked authorization is terminal (WSS close policy-violation "revoked"
  maps to `HostLossAuthRevoked`) and bypasses the window.
- Explicit local/remote/system ends cancel the episode
  (`finishCall`, `CloseConferenceAsSystem`).
- The daemon does not dial: the CLIENT reconnects the SAME call with
  immediate + 1/2/4/8 s attempts (cap 8) inside the window, current path
  first then the alternate in auto mode, chosen route only in forced mode.

## 2. Adaptive playout (privacy-safe scalar evidence every 5 s)

- Host (client→gateway) jitter: fixed hard trim 120 ms (preserves 2-6 frame
  coalesced TCP batches); steady one-frame-at-a-time clock drift trims to the
  adaptive level; level starts 40 ms, relaxes to 20 ms after ten seconds of
  uninterrupted LIVE playback, and grows +20 ms (cap 120 ms) once per active
  starvation episode while arrivals continue (genuine quiet never grows it);
  intermittent isolated losses reset the clean-live count.
- Modem leg (worker→gateway): hard 80 ms queue cap; level 80 ms start, 40 ms
  after ten clean seconds, restored immediately on an active underrun. The
  newest coalesced UAC batch is protected (`burstCredit`) so relaxed steady
  trimming never chops fresh burst speech.
- Evidence line `media adaptive queue`: `host_level_ms`, `host_starvations`,
  `leg_level_ms_max`, `leg_starvations`, `uplink_denoise`,
  `downlink_denoise`, `uplink_denoise_us`, `downlink_denoise_us`.
  Queue depth is queueing delay only and is never equated to network RTT or
  physical mouth-to-ear delay.

## 3. Directional denoise (SpeexDSP 1.2.1, BSD, statically linked)

- 8 kHz / 20 ms / 160 samples. `SET_DENOISE=1`, `SET_NOISE_SUPPRESS=-6`;
  AGC, VAD gating and dereverb OFF (the phone keeps its own AEC/voice
  processing). `speex_preprocess_run`'s return is the VAD decision, not an
  error. Any non-ready state passes the raw frame through; processing never
  blocks or drops audio.
- Uplink: one session-scoped preprocessor on the host frame, once before
  mix. Downlink: one preprocessor per Leg applied to that leg's frame before
  mix. State survives ICE↔WS handover and conference split/park; destroyed at
  session/leg Close. Hot toggle is gate-only at the next 20 ms boundary.

### Per-device settings (isolated, no cross-principal default)

Settings table keys `device:<id>:audio_denoise_uplink|downlink` ("1"/"0";
absent = light ON). `PUT /api/v2/devices/{id}/preferences` now takes OPTIONAL
fields with PATCH-presence semantics:

| field | type | absent | present |
|---|---|---|---|
| `defaultLineId` | string | unchanged | set/clear (legacy bodies work) |
| `uplinkDenoise` | bool | unchanged | stored + live-applied |
| `downlinkDenoise` | bool | unchanged | stored + live-applied |

Own-device-only writes (403 otherwise). Response echoes effective values
(unset reported as the light-on default).

### Per-attach optional parameters (backward compatible)

- `GET /api/v2/calls/{id}/media?uplinkDenoise=0|1&downlinkDenoise=0|1`
  (same for `/conferences/{id}/media`).
- Offer POST bodies (`/calls/{id}/ice` offer and conference offer) accept
  optional `uplinkDenoise`/`downlinkDenoise` booleans.
- `GET /api/v2/calls/{id}/ice` advertises
  `capabilities: {"audioDenoise": ["uplink","downlink"]}` for feature
  detection; a new client with an old server simply never sends the params.
- Hot change over an attached WSS socket (text control):
  `{"type":"denoise","uplink":<bool>,"downlink":<bool>}` — omitted direction
  keeps its current gate; older servers ignore the unknown message type.

## Tests

`internal/speexpp` (real C library: continuous processing, bypass, hot
toggle), `internal/unified/media` (adaptive level/stale-rejection/batch
protection, real-noise mixer wiring, `-race -count=6` adoption TWCC
`TestAdoptClientPeerPreservesRTPSequenceContinuity`), `internal/unified/runtime`
(absolute deadline, fresh-media cancel, serialized expiry/restore race),
`internal/unified/api` (PATCH presence semantics, same-key different-device
isolation, fresh-media handover survival).
