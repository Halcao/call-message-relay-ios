#!/usr/bin/env bash
# Gateway build/test entry point.
#
# Sources Scripts/build-env.sh (when present) so rebuildable caches stay
# outside the source tree on the project volume, then execs `go`. Works from
# any subdirectory of the gateway module; CI may also call `go` directly —
# without build-env.sh the standard user-level Go caches apply.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -f "$ROOT/Scripts/build-env.sh" ]]; then
	# Repository-root layout (this repo): Scripts/ sits next to gateway/.
	source "$ROOT/Scripts/build-env.sh"
elif [[ -f "$ROOT/iPhoneApp/Scripts/build-env.sh" ]]; then
	# Legacy sibling layout used during development (gateway next to iPhoneApp).
	source "$ROOT/iPhoneApp/Scripts/build-env.sh"
fi
exec go "$@"
