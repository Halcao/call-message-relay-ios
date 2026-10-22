# CallRelay gateway

The Go module (`callrelay/gateway`) behind the CallRelay iOS app and PWA:
one unified daemon (`cmd/callrelayd`: v2 API, console, media mixer, voicemail,
recording, events) plus one single-line cellular worker per module
(`cmd/cellbridge-gateway`, installed as `callrelay-worker`).

- Build, configure and run: **docs/self-hosting.md** (repository root) —
  including the true-CGO audio requirements and the hardware prerequisites.
- Design overview: **docs/architecture.md** (repository root).
- Wire contracts: `docs/media-routing-contract.md`,
  `docs/quic-datagram-media-protocol.md`, `docs/recovery-denoise-contract.md`,
  `openapi-v1.yaml` (legacy worker lineage) and the v2 handlers in
  `internal/unified/api`.
- Licenses & provenance: `THIRD_PARTY_NOTICES.md`, `LICENSE-CellBridge`
  (upstream MIT; parts of the worker lineage derive from
  github.com/mccding/CellBridge). Vendored Opus/SpeexDSP sources, licenses
  and reproducible build scripts: `third_party/` + `tools/build-*.sh` in the
  repository root.

Quick local build (macOS arm64 / linux arm64 vendored archives, or linux
amd64 with `apt install libopus-dev libspeexdsp-dev`):

    cd gateway
    ../tools/go.sh build ./...

CI compiles and runs the focused suites; see
`.github/workflows/gateway.yml`.
