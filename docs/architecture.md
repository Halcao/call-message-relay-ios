# CallRelay architecture

CallRelay is a self-hosted bridge between cellular voice/SMS lines and iOS
clients (native app + installable PWA). The repository contains both sides:

- `CallRelay/` — the native iOS/iPadOS client (SwiftUI, CallKit/PushKit).
  ALL calling, SMS and events go through YOUR gateway; the client additionally
  offers an OPTIONAL sync of its history and rule settings through the
  owner's OWN private iCloud (CloudKit) container with iCloud-keychain key
  recovery. The gateway itself never depends on CloudKit, on any author-run
  central service, or on any third-party push relay.
- `gateway/` — the Go gateway (module `callrelay/gateway`).
- `tools/` — portable build/preparation scripts and example configs.

## Process model (gateway)

```
 public HTTPS (your domain, TLS by Caddy/nginx)
        │
   ┌────▼────┐   loopback-only unified daemon (cmd/callrelayd)
   │ reverse │   • v2 REST API + WebSocket events   (internal/unified/api)
   │  proxy  │   • web console + enrollment admin   (internal/unified/console)
   └─────────┘   • call/conference arbitration, media mixer, recording,
        │          voicemail, denoise               (internal/unified/runtime,
        │                                          media, recording, notify)
   127.0.0.1:8790• QUIC datagram listener (optional)(internal/unified/quic)
        │
        │ per-line workers: one process per cellular module
   ┌────▼───────────────────────────────────────────────┐
   │ callrelay-worker (built from cmd/cellbridge-gateway)│
   │  • exclusive owner of ONE modem AT port + ONE PCM   │
   │    device pair                                      │
   │  • CellBridge-derived lineage (MIT, see             │
   │    gateway/LICENSE-CellBridge)                      │
   └─────────────────────────────────────────────────────┘
        │                         │
   USB serial (ttyUSB*/ttyACM*)   ALSA arecord/aplay, raw PCM FIFOs,
   ATD dial / CMGS SMS / URCs    or the vendor qdc507 module-side runtime
   (internal/modem, sms, voice)   (closed vendor firmware, NOT distributed)
```

- The daemon owns ALL clients, lines, state and media mixing; workers are
  dumb, single-line modem/audio agents on loopback behind a shared random
  internal token (`tools/prepare-line.example.sh`).
- State is a single modernc.org/sqlite database (pure Go) under
  `gateway.data_dir`; migrations are plain SQL (`gateway/migrations`).
- Line identity is registry-based: a cellular module is keyed by a hash of
  its verified backup identity (`tools/line_registry.py`), not by USB slot,
  so replugging never re-assigns history; unknown modules are recorded as
  `pending` and never auto-flashed.

## Audio path

8 kHz mono is the domain. The modem leg is fixed-rate G.711 (PCMU) at the
worker; the network hop to clients uses **libopus** (narrowband, inband
FEC/DTX, 20 ms frames — CGO wrapper `gateway/internal/opus` against the
pinned vendored static archives) or PCMU for pre-Opus clients. Per-call
directional denoise runs the **SpeexDSP** preprocessor
(`gateway/internal/speexpp`, vendored archive; denoise only, −6 dB floor).
Routing between peers auto-selects direct WebRTC (ICE), the optional
QUIC DATAGRAM path (where advertised), and the always-available authenticated
WSS relay media socket; the contracts
live in `gateway/docs/media-routing-contract.md`,
`gateway/docs/quic-datagram-media-protocol.md` and
`gateway/docs/recovery-denoise-contract.md`. The daemon does not dial: the
client reconnects a dropped call inside a bounded absolute grace window.

## Auth & pairing

Enrollment keys (`id.secret`, created by `cmd/callrelay-admin`) authorize
new devices against the gateway's Ed25519 identity (`GET
/api/v2/identity`): each phone generates its own ed25519 keypair and proves
possession during enrollment, then receives access/refresh tokens and all
further calls/SMS/events are key-scoped. A key stays valid until the owner
revokes it, so several phones of one household may share a single key (per-
line grants and the manageNumber capability further bound what each device
may do). The console (password → bcrypt file + rate limiting) is for
operators; devices never use it. Machine-readable API references:
`gateway/openapi-v1.yaml` (the v1 worker lineage) and the v2 handlers under
`gateway/internal/unified/api`; the client-facing wire contract summary is
`PROTOCOL.md` in the repository root.

## Notifications

Three independent paths, none required for calling itself:
1. **Native iOS**: optional direct APNs sender (your own .p8 VoIP key) or a
   push broker; `push.*` config.
2. **PWA Web Push**: standard Web Push sent BY the gateway itself (its own
   VAPID key generated on the gateway, subscriptions only after a short-code
   bind in the browser). The author runs no push relay; delivery still rides
   the browser/OS push transport (e.g. Apple's push service wakes Safari),
   and a tap opens the PWA — not a native CallKit wake.
3. **Bark** (optional, off by default): per-device self-hosted bridge with
   strict SSRF policy; see `gateway/configs/bark.example.yaml`.

## Storage policy

Heavy disposable audio (voicemail, recordings) writes only to a SEPARATE
verified mount (SD/eMMC), checked device+fstype per write, with NO fallback;
the eMMC/disk keeps the SQLite index, keys and config. This keeps flash wear
and full-disk failures away from the durable state.

## Provenance

The gateway is derived from the upstream CellBridge project (MIT; pinned
commit in `gateway/THIRD_PARTY_NOTICES.md`) for the worker/modem/SMS/voice
lineage, and re-architected into the unified daemon described above. Opus
and SpeexDSP vendored builds, static-archive locations and per-platform CGO
linking rules are in `gateway/THIRD_PARTY_NOTICES.md`; the original vendor
module-side voice runtime remains closed-source hardware-vendor firmware and
is not distributed here.
