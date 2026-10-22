#!/usr/bin/env bash
# Project-local xcodebuild entry point; explicit paths/jobs still take precedence.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/build-env.sh"
export DEVELOPER_DIR="${DEVELOPER_DIR:-/Applications/Xcode.app/Contents/Developer}"
if [[ "${1:-}" == --print-cache-path ]]; then
  printf '%s\n' "$CALLRELAY_CACHE_ROOT"
  exit 0
fi
args=()
has_derived=false
has_jobs=false
for arg in "$@"; do
  [[ "$arg" != -derivedDataPath ]] || has_derived=true
  [[ "$arg" != -jobs ]] || has_jobs=true
done
$has_derived || args+=(-derivedDataPath "$CALLRELAY_CACHE_ROOT/xcode/default")
$has_jobs || args+=(-jobs "${JOBS:-6}")
exec "$DEVELOPER_DIR/usr/bin/xcodebuild" ${args[@]+"${args[@]}"} "$@"
