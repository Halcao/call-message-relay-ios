# CallRelay self-hosting guide

This guide takes you from a fresh clone to a working private gateway:
build → configure → workers + daemon → HTTPS reverse proxy → console
bootstrap → device pairing → notifications. Everything is self-hosted on
machines and accounts YOU control; the author runs no relay, broker or push
service, and distributes no credentials.

## 0. What you need first (hardware & accounts)

- **A Linux or macOS host.** Verified build/runtime targets: Linux arm64
  (the production class, static musl CGO daemon — this is the reference
  deployment), macOS arm64 (development host). Linux x86-64 works as a
  build/development target linking the SYSTEM libopus/libspeexdsp
  (`apt install libopus-dev libspeexdsp-dev`); its CI job is included but
  was not exercised at the time this guide was written — treat it as
  CI-pending, not hardware-qualified. Windows is not supported.
  **The code is not specific to any one board** (the upstream development
  host was a small ARM box; anything with USB host power + serial + optional
  ALSA will do). Do NOT expect every modem module to work: support follows
  the AT/voice capabilities described below.
- **One USB cellular module per line** with:
  - an exposed **AT serial port** (shows up under `/dev/serial/by-id/...`,
    `/dev/ttyUSB*` or `/dev/ttyACM*`);
  - a **voice path** — either a USB audio (ALSA) interface, raw PCM FIFOs,
    or for the IG830/QDC507 family the module-side **closed-source vendor
    runtime** (`qdc507_aprv3.ko`, `qdc507_voice.ko`, `mavo-pcm-bridge.armv7`,
    `alsaucm`). That vendor firmware is NOT distributed in this repository:
    obtain it from your module vendor and provision the modules ONCE (EDL
    flashing/unlock is operator work with vendor tools BEFORE installation).
    The example services deliberately never flash modules; unknown modules
    are recorded as `pending` by the registry instead of being touched.
  - a SIM with caller-ID/SMS; if you plan outbound calls/SMS, a plan you
    pay for (SMS are REAL `AT+CMGS` sends by default — keep
    `CELLBRIDGE_SMS_DRY_RUN=false` in production so "sent" means sent).
- **A domain name** you can point DNS at the host, **or** a CGNAT tunnel
  setup (optional `tools/frp/` example). TLS comes from your reverse proxy.
- **For native iOS push**: your own Apple Developer membership, an app you
  build/sign with YOUR bundle id + team, and YOUR APNs .p8 key. A
  re-signed/restored client can NOT borrow the author's push identity;
  without your own APNs setup, native background ringing is unavailable and
  you should use the PWA path instead.

## 1. Build the gateway binaries (true CGO)

Prereqs: Go 1.26+, and for the daemon a C toolchain with system libopus /
SpeexDSP 1.2.1 — on the two vendored host targets the pinned static archives
under `gateway/third_party/` are used automatically:

```bash
cd gateway
# macOS arm64 (development host): vendored darwin-arm64 archives
../tools/go.sh build ./...

# production class: static linux/arm64-musl (matches the vendored archives)
# CC wrapper e.g. from `brew install messense/macos-cross-toolchains/aarch64-unknown-linux-musl`
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-unknown-linux-musl-cc \
CGO_LDFLAGS=-static \
  ../tools/go.sh build -tags netgo,osusergo -trimpath -ldflags='-s -w' \
  -o ../build/callrelayd ./cmd/callrelayd
# per-line worker binary: from the CellBridge-derived gateway
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-unknown-linux-musl-cc \
CGO_LDFLAGS=-static \
  ../tools/go.sh build -tags netgo,osusergo -trimpath -ldflags='-s -w' \
  -o ../build/callrelay-worker ./cmd/cellbridge-gateway
# admin CLI MUST be cross-built with the SAME GOOS/GOARCH/CGO/CC as the
# daemon (a macOS-built admin binary does not run on the Linux host)
CGO_ENABLED=1 GOOS=linux GOARCH=arm64 CC=aarch64-unknown-linux-musl-cc \
CGO_LDFLAGS=-static \
  ../tools/go.sh build -tags netgo,osusergo -trimpath -ldflags='-s -w' \
  -o ../build/callrelay-admin ./cmd/callrelay-admin

# ...and return to the repository root, which all later `tools/` steps use
cd ..
```

`CGO_ENABLED=1` matters for the daemon: that is what links the REAL Opus and
SpeexDSP audio code. A `CGO_ENABLED=0` daemon would be an audio-broken
daemon; the build above is what the production hosts run. Verify with
`go version -m build/callrelayd` (you should see `CGO_ENABLED=1`) and
`file` (static-pie/aarch64).

Linux x86-64 dev boxes instead install the system libraries
(`apt install build-essential libopus-dev libspeexdsp-dev`) and build with
plain `CGO_ENABLED=1 go build ./...` — see the `#cgo linux,amd64` directives
in `gateway/internal/opus/opus.go` and `gateway/internal/speexpp/speexpp.go`.

Optionally regenerate the vendored archives from pinned upstream sources:
`tools/build-libopus.sh` / `tools/build-speexdsp.sh` (checksum-pinned tarballs,
neutral `/build/callrelay` path prefix maps). See those scripts' headers for
exact toolchain requirements; the `ios` mode is a recipe for the app's
xcframework and is not part of gateway verification.

## 2. Configure the daemon

```bash
install -d -m 0750 /etc/callrelay /var/lib/callrelay
cp tools/config/unified.example.yaml /etc/callrelay/unified.yaml
$EDITOR /etc/callrelay/unified.yaml   # set id, public_base_url, paths
# Console admin credential — generate YOUR OWN password hash; nothing is
# shipped with a default. -c creates the file, -B bcrypt, and the password
# is prompted INTERACTIVELY so it never enters shell history or ps:
htpasswd -Bc /etc/callrelay/console.bcrypt admin
chmod 0600 /etc/callrelay/console.bcrypt
```

Secrets policy: every credential (console bcrypt, gateway Ed25519 key,
APNs .p8, VAPID key, worker internal token, TURN secret via env) lives in a
root-owned file or environment variable — the YAML never holds a secret
value. `tools/config/unified.example.yaml` comments each field.

## 3. Register lines and prepare workers

Install the runtime layout the example units reference (no error is hidden;
every step is visible):

```bash
install -d -m 0755 /opt/callrelay/bin /opt/callrelay/share /usr/local/lib/callrelay
# from the repository root; step 1 wrote the linux/arm64 artifacts here:
install -m 0755 build/callrelayd          /opt/callrelay/bin/callrelayd
install -m 0755 build/callrelay-worker    /opt/callrelay/bin/callrelay-worker
install -m 0755 build/callrelay-admin     /opt/callrelay/bin/callrelay-admin
install -m 0755 tools/prepare-line.example.sh /usr/local/lib/callrelay/prepare-line.example.sh
install -m 0644 tools/config/line-worker.example.yaml /opt/callrelay/share/line-worker.example.yaml
# (prepare-line.example.sh reads TEMPLATE=/opt/callrelay/share/line-worker.example.yaml
#  automatically once installed standalone; from the repo it uses tools/config/ directly)
cp tools/config/lines.json.example /etc/callrelay/lines.json
$EDITOR /etc/callrelay/lines.json   # replace placeholders; see below
```

Registry rules (multi-line hosts):

```bash
python3 tools/line_registry.py --registry /etc/callrelay/lines.json status
python3 tools/line_registry.py render-lines --help
```

- Give each line an EXPLICIT AT serial: the entry's `"tty"` field must be a
  stable path — prefer a udev by-id symlink (`ls -l /dev/serial/by-id/`)
  over a bare `ttyUSB` number that renumbers on replug.
  `tools/prepare-line.example.sh` refuses to run a line without a tty (it
  never binds by scanning /dev, because several workers with `tty: auto`
  would fight over one module); lab override: `AT_TTY=...` in
  `/run/callrelay/lineN.env`.
- Placeholder module keys/port/data dirs in `lines.json.example` MUST be
  replaced with values for YOUR modules; `line_registry.py status` validates
  key shape, state and port uniqueness.
- Generate the per-line block the daemon hot-reloads:
  `python3 tools/line_registry.py --registry /etc/callrelay/lines.json render-lines > /etc/callrelay/lines.yaml`

Firmware reality check (unchanged from step 0): modules must ALREADY be
provisioned. The IG830/QDC507 family additionally needs its ADB/PTY path and
the closed-source vendor voice runtime; EDL flashing is one-time operator
work with vendor tools, outside this repository, and never performed by
these services.

Install the example units:

```bash
cp tools/systemd/callrelay-worker@.service.example /etc/systemd/system/callrelay-worker@.service
cp tools/systemd/callrelayd.service.example        /etc/systemd/system/callrelayd.service
systemctl daemon-reload
systemctl enable --now callrelay-worker@1.service callrelayd.service
# callrelay-adb.service.example only if your modules use the qdc507 runtime
```

The worker keeps exclusive ownership of its modem + audio pair; `callrelayd`
is the single process that owns clients, lines, state and the mixer. Check
with `journalctl -u callrelayd -u 'callrelay-worker@*' -b`.

## 4. HTTPS reverse proxy

The daemon binds loopback only; terminate TLS with your proxy and expose
`/api/v2/*`, `/console*`, `/pwa*`, `/healthz` — see
`tools/caddy/Caddyfile.example` (or nginx equivalents). If your host is
behind CGNAT, `tools/frp/` shows the optional tunnel shape (self-hosted
frps; bring your own token file). QUIC media (when enabled) is UDP to the
host directly; it is an enhancement, never a requirement, and cannot punch
through every NAT (failures fall back to WSS/WebRTC automatically).

## 5. Bootstrap the console and pair devices

1. Open `https://your-gateway-domain/` → console login (the admin user and
   password from step 2).
2. Create an enrollment key — either in the console, or on the host:
   `callrelay-admin -config /etc/callrelay/unified.yaml key create -name first`
   → prints `key_id` + `key=<id>.<secret>`.
3. In the iOS app (built with YOUR Apple account, see `BUILD.md`/`README.md`)
   or in the PWA, enter the gateway URL and the `id.secret` pair. The device
   generates its own ed25519 keypair; the gateway verifies possession during
   enrollment and issues access/refresh tokens. One key may enroll several
   devices (household phones share it) and stays valid until you revoke it
   (`key revoke -id …`) — revoke when pairing is finished if you want to
   close further enrollment.
4. Verify end-to-end without touching the phone network:
   `callrelay-admin … selftest -base https://your-gateway-domain -key <id>.<secret>`
   (enrolls a throwaway device, exercises the public API, then revoke it).
   `callrelay-probe` does the deeper contract probe the same way.

## 6. Notifications

- **PWA (recommended default):** open `https://your-gateway-domain/pwa/`,
  install it, and bind it with the short code shown. The gateway generates
  its own VAPID key under `data_dir/webpush/` and sends standard Web Push
  to bound browsers itself — the author runs no push relay — though final
  delivery still rides on the browser/OS push transport (Safari wakes via
  Apple's push infrastructure). Tapping a notification opens the PWA page;
  it is NOT equivalent to a native CallKit background call.
- **Native iOS background ringing:** only with YOUR OWN APNs VoIP key whose
  team matches the app signature. Set `push.apns_*` in unified.yaml to your
  key file/id/team/bundle id and restart. Without it, the native app still
  works in foreground/on-network-wake, but you should not expect lock-screen
  VoIP wakes. No author-operated broker is involved; there is an optional
  `push.broker_url` for tenants that run their own.
- **Bark bridge (optional):** self-hosted or official, per-device opt-in,
  off by default; see `gateway/configs/bark.example.yaml` for the enforced
  SSRF/HTTPS policy.

## 7. Operations notes

- Storage: keep `data_dir` on durable storage (it holds the SQLite state,
  keys and identity). Voicemail/recording audio deliberately writes ONLY to
  a separate verified mount (device+fstype pinned in config; no fallback) so
  a full card can never wedge the system disk.
- Upgrades: the daemon restarts workers via systemd on line changes; to
  upgrade binaries yourself, keep a previous copy under
  `/opt/callrelay/bin/backup-<stamp>/` and `systemctl restart callrelayd
  callrelay-worker@…`. Schema migrations are additive; keep the old binary
  at hand for rollback.
- Backups: `sqlite` file + `key_file` + `console.bcrypt` + registry JSON are
  the minimal restore set. Losing the enrollment secret box means re-pairing
  devices.
- Logs are redacted by design: numbers/ICCID stay masked, tokens are never
  printed; `callrelay-sms-query` and the probes follow the same rules.

## 8. Verification status (what is proven vs pending)

| Item | Status |
| --- | --- |
| Linux arm64 static CGO daemon/worker/admin cross-build (`go version -m` shows CGO_ENABLED=1, tags, linked libopus 1.5.2 + SpeexDSP) | verified locally |
| macOS arm64 true-CGO builds + focused Go suites (config/store/opus/speexpp/sms + SMS-action + api/runtime) | verified locally |
| Example systemd/config/scripts static checks + gitleaks scans of the published tree | verified locally |
| Linux x86-64 system-library build | wired to CI (`.github/workflows/gateway.yml`); not executed locally, CI result pending first push |
| Docker image | intentionally not provided (see §0 hardware access; an untested image would mislead) |
| Physical modem lines / carriers / EDL provisioning / field audio quality | operator hardware work, out of scope of this repository |
