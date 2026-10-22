# Third-Party Notices — CallRelay gateway

The CallRelay gateway combines original CallRelay code with material derived
from upstream open-source projects. Each component retains its own license;
nothing here is re-licensed.

## Upstream CellBridge (derived gateway lineage)

Parts of this gateway descend from the CellBridge reference implementation
(`cmd/cellbridge-*`, `internal/api`, `internal/sip`, `internal/modem`,
`internal/sms`, `internal/voice` and the worker runtime the unified daemon
delegates per-line modem/UAC ownership to).

- Upstream project: https://github.com/mccding/CellBridge
- Provenance commit pinned for this integration:
  `2ee9d6b05d8a4391f782e6ce8a6d23ef57015fda`
- License: MIT, "Copyright (c) 2026 CellBridge contributors". The full license
  text is preserved verbatim in [`LICENSE-CellBridge`](LICENSE-CellBridge).
- Upstream also ships a NOTICE clarifying its own implementation is
  independent of MacCellular/NasAnySim/dji-4g-vohive-mac. The module-side
  voice runtime (`qdc507_aprv3.ko`, `qdc507_voice.ko`, `mavo-pcm-bridge`,
  `alsaucm`) is **closed-source vendor firmware**: it is NOT distributed in
  this repository and must be obtained from the cellular-module vendor or
  hardware bundle before the per-line audio path can run (see
  `docs/self-hosting.md` in the repository root).

The original CallRelay client and gateway additions remain under the
repository-root PolyForm Noncommercial License; the CellBridge MIT grant
covers only the CellBridge-derived portions, exactly as upstream licensed it.

## Opus reference codec — `third_party/opus`

- Library: libopus 1.5.2 (reference audio codec), used for the network-hop
  Opus codec (8 kHz narrowband, inband FEC/DTX).
- Source: https://downloads.xiph.org/releases/opus/opus-1.5.2.tar.gz
- SHA-256 (pinned in tools/build-libopus.sh):
  65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1
- License: BSD 3-Clause; full text in `third_party/opus/COPYING`.
- Vendored static archives (reproducible via `tools/build-libopus.sh`):
  `third_party/opus/lib-darwin-arm64/libopus.a` and
  `third_party/opus/lib-linux-arm64-musl/libopus.a` (gateway host targets).
  On other Linux architectures (e.g. x86-64 development boxes) the CGO
  wrappers link the system libopus instead; see `internal/opus/opus.go`
  build directives and `docs/self-hosting.md`.

## SpeexDSP preprocessor — `third_party/speexdsp`

- Library: SpeexDSP 1.2.1 preprocessor, used ONLY for the per-call
  directional voice denoise (8 kHz / 20 ms / 160-sample frames; denoise on,
  noise suppress floor -6 dB; AGC/VAD gating/dereverb off). The iOS client
  and PWA control it through optional per-device parameters; no SpeexDSP
  code ships in the iOS app.
- Source: https://downloads.xiph.org/releases/speex/speexdsp-1.2.1.tar.gz
- SHA-256 (pinned in tools/build-speexdsp.sh):
  8c777343e4a6399569c72abc38a95b24db56882c83dbdb6c6424a5f4aeb54d3d
- License: BSD 3-Clause; full text in `third_party/speexdsp/COPYING`.
- Vendored static archives (reproducible via `tools/build-speexdsp.sh`):
  `third_party/speexdsp/lib-darwin-arm64/libspeexdsp.a` and
  `third_party/speexdsp/lib-linux-arm64-musl/libspeexdsp.a`; on other
  Linux architectures the wrapper links the system libspeexdsp.

## Go module dependencies

Pion WebRTC/interceptor/rtp/rtcp, gorilla/websocket, grandcat/zeroconf,
quic-go, SherClockHolmes/webpush-go, skip2/go-qrcode, modernc.org/sqlite,
golang.org/x/crypto, gopkg.in/yaml.v3 and the other Go modules listed in
`go.mod` retain their own licenses in the module sources (MIT, BSD-2/3,
Apache-2.0 or ISC per module).
