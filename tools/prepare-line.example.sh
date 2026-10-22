#!/usr/bin/env bash
# Portable per-line worker PREPARATION example.
#
# WHAT THIS IS: a self-contained example that prepares exactly ONE line
# before the systemd unit tools/systemd/callrelay-worker@.service.example
# starts the worker. For each line it resolves an EXPLICIT AT serial path,
# ensures the shared internal token exists, and renders that line's worker
# YAML (tools/config/line-worker.example.yaml) with per-line ports, data
# directory and the explicit tty.
#
# WHY THE TTY MUST BE EXPLICIT: with `modem.tty: auto` several workers on
# one host would scan the same /dev candidates and could claim the SAME
# module. Multi-line deployments therefore pin one unique serial path per
# line. Resolution order:
#   1. registry entry field   "tty" for this line (tools/line_registry.py
#      registry JSON; keep it stable with a udev by-id symlink such as
#      /dev/serial/by-id/usb-<vendor>-<model>-if02-port0 rather than a
#      bare ttyUSB number that renumbers on replug);
#   2. per-line env file      ${WORKER_DIR}/${LINE}.env containing AT_TTY=...
#      (optional override for lab setups);
#   3. otherwise FAIL with a clear message — this script never guesses a
#      module from a USB-path hint.
# The resolved path must exist; otherwise the script fails with attach/udev
# permission hints (dialout group / udev rule for the module VID:PID).
#
# WHAT IT DELIBERATELY DOES NOT DO: this is the SERIAL + CONFIG preparation
# only. It does NOT flash or provision modules. Cellular modules such as the
# IG830/QDC507 family additionally require their ADB/PTY path and the
# CLOSED-SOURCE module-side voice runtime (qdc507_aprv3.ko, qdc507_voice.ko,
# mavo-pcm-bridge.armv7, alsaucm) — one-time vendor-tool operator work
# (EDL flashing/unlocking) performed BEFORE any service starts, NOT part of
# this repository, NOT completed by this script. Without that provisioning
# the voice backend reports unavailable truthfully; raw-pcm/alsa backends on
# plain USB-audio modules do not need the qdc507 runtime.
#
# REAL WRITE SCOPE (do not run it blind on a shared host):
#   - ${WORKER_DIR}/${LINE}.yaml                    (0600, default /run/callrelay)
#   - ${TOKEN_FILE} if missing (random 32 bytes)    (0600, default /etc/callrelay/internal.token)
#   - mkdir -p of the line's registry dataDir       (e.g. /var/lib/cellbridge/line1)
#   - nothing else; reads the registry/template read-only; never talks to
#     the network.
#
# USAGE: prepare-line.example.sh <lineId>    e.g. prepare-line.example.sh line1
# Environment overrides:
#   REGISTRY=/etc/callrelay/lines.json  WORKER_DIR=/run/callrelay
#   TOKEN_FILE=/etc/callrelay/internal.token
#   TEMPLATE=<repo>/tools/config/line-worker.example.yaml
set -euo pipefail

LINE="${1:?usage: $0 <lineId> (e.g. line1)}"
REGISTRY="${REGISTRY:-/etc/callrelay/lines.json}"
WORKER_DIR="${WORKER_DIR:-/run/callrelay}"
TOKEN_FILE="${TOKEN_FILE:-/etc/callrelay/internal.token}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Repo layout keeps the template next to the script (tools/config); when this
# script is installed standalone (e.g. /usr/local/lib/callrelay/) the
# template must be copied to /opt/callrelay/share/line-worker.example.yaml
# or TEMPLATE must be set explicitly (see docs/self-hosting.md step 3).
TEMPLATE="${TEMPLATE:-${SCRIPT_DIR}/config/line-worker.example.yaml}"
[[ -f "$TEMPLATE" ]] || TEMPLATE="/opt/callrelay/share/line-worker.example.yaml"

[[ -f "$REGISTRY" ]] || { echo "registry $REGISTRY missing; seed it first (python3 tools/line_registry.py seed --help)" >&2; exit 1; }
[[ -f "$TEMPLATE" ]] || { echo "worker template not found ($TEMPLATE); copy tools/config/line-worker.example.yaml to the host first" >&2; exit 1; }

# 1. Pull the line entry: ports, data dir, explicit tty, usb path (printout
#    only, never used to auto-bind). A registry entry WITHOUT "tty" here
#    must come out empty (never shifted from usbPath).
# Extract each registry field with its own command substitution: positional
# reads (space OR tab) collapse empty fields — a line missing "tty" would
# silently shift usbPath into the TTY slot. Separate substitutions keep an
# absent "tty" verbatim EMPTY.
pyget() {  # pyget <field> <default>
	python3 -c '
import json, sys
registry = json.load(open(sys.argv[1]))
entry = registry.get("lines", {}).get(sys.argv[2])
if entry is None:
    sys.exit(f"line {sys.argv[2]} is not in the registry")
if entry.get("state") != "active":
    sys.exit("line %s state is %r; only active lines may run" % (sys.argv[2], entry.get("state")))
print(entry.get(sys.argv[3], sys.argv[4]), end="")
' "$REGISTRY" "$LINE" "$1" "$2"
}
WORKER_PORT="$(pyget workerPort "")"
INTERNAL_PORT="$(pyget internalPort "")"
[[ -n "$WORKER_PORT" && -n "$INTERNAL_PORT" ]] || { echo "line $LINE has no allocated ports in $REGISTRY" >&2; exit 1; }
DATA_DIR="$(pyget dataDir "/var/lib/cellbridge/$LINE")"
# The two reads below must be able to come back EMPTY (that is meaningful):
REG_TTY="$(pyget tty "")"
USB_PATH="$(pyget usbPath "")"

# 2. Resolve the EXPLICIT AT serial (registry field first, per-line env
#    override second, hard failure third — never auto-scan).
AT_TTY="$REG_TTY"
ENV_FILE="$WORKER_DIR/$LINE.env"
if [[ -f "$ENV_FILE" ]]; then
	# shellcheck disable=SC1090
	AT_TTY="$(grep -E '^AT_TTY=' "$ENV_FILE" | tail -n1 | cut -d= -f2-)"
fi
if [[ -z "$AT_TTY" ]]; then
	cat >&2 <<EOF
line $LINE has no explicit AT serial: set the registry entry "tty" (a stable
udev by-id path, e.g. /dev/serial/by-id/usb-<vendor>-<model>-if02-port0) or
write AT_TTY=... into $ENV_FILE. auto/scan binding is NOT supported for
multi-line hosts (workers would fight over one module).
EOF
	exit 1
fi
if [[ ! -e "$AT_TTY" ]]; then
	echo "AT serial $AT_TTY for line $LINE does not exist: attach the module and check enumeration (ls -l /dev/serial/by-id/); the service user needs rw access (dialout group or a udev rule for the module VID:PID)" >&2
	exit 1
fi

# 3. ALSA hint (informational only; raw-pcm FIFOs and the vendor qdc507
#    module-side runtime are alternative voice paths — see header).
if ! command -v arecord >/dev/null 2>&1; then
	echo "note: arecord (alsa-utils) not found; the alsa voice backend will be unavailable" >&2
fi

# 4. Publish one complete shared token atomically: several worker instances
# can start together, and must never truncate or overwrite each other's key.
python3 - "$TOKEN_FILE" <<'PYTOKEN'
import os, pathlib, secrets, tempfile, sys
path = pathlib.Path(sys.argv[1])
path.parent.mkdir(parents=True, exist_ok=True)
if path.exists():
    if not path.is_file() or path.stat().st_size == 0:
        sys.exit("internal token file is empty or invalid; repair it before starting workers")
    sys.exit(0)
fd, temp = tempfile.mkstemp(prefix=".internal-token-", dir=path.parent)
try:
    with os.fdopen(fd, "w") as stream:
        os.fchmod(stream.fileno(), 0o600)
        stream.write(secrets.token_hex(32) + "\n")
        stream.flush()
        os.fsync(stream.fileno())
    try:
        os.link(temp, path)
    except FileExistsError:
        if not path.is_file() or path.stat().st_size == 0:
            sys.exit("internal token file is empty or invalid; repair it before starting workers")
finally:
    os.unlink(temp)
PYTOKEN

# 5. Render the per-line worker YAML: explicit tty + listen ports + data dir.
mkdir -p -m 0755 "$WORKER_DIR"
OUT="$WORKER_DIR/$LINE.yaml"
sed -e "s|listen: 127.0.0.1:9701|listen: 127.0.0.1:${WORKER_PORT}|" \
	-e "s|listen: 127.0.0.1:9801|listen: 127.0.0.1:${INTERNAL_PORT}|" \
	-e "s|tty: auto|tty: ${AT_TTY}|" \
	-e "s|dir: /var/lib/cellbridge/line1|dir: ${DATA_DIR}|" \
	-e "s|token_file: .*|token_file: ${TOKEN_FILE}|" \
	"$TEMPLATE" > "$OUT"
chmod 0600 "$OUT"
mkdir -p "$DATA_DIR"
echo "prepared ${OUT}: tty=${AT_TTY} worker=${WORKER_PORT} internal=${INTERNAL_PORT} data=${DATA_DIR} (registry usb hint: ${USB_PATH:-n/a})"
