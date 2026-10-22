#!/usr/bin/env bash
# Source this file to keep this checkout's rebuildable data on its source volume.
_CALLRELAY_APP_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
_CALLRELAY_CHECKOUT="$(printf '%s' "$_CALLRELAY_APP_ROOT" | shasum -a 256 | cut -c1-12)"
case "$_CALLRELAY_APP_ROOT" in
  /Volumes/*/*)
    _CALLRELAY_VOLUME="${_CALLRELAY_APP_ROOT#/Volumes/}"
    _CALLRELAY_VOLUME="/Volumes/${_CALLRELAY_VOLUME%%/*}"
    _CALLRELAY_DEFAULT_CACHE="$_CALLRELAY_VOLUME/.codex-build/call-message-relay/$_CALLRELAY_CHECKOUT"
    ;;
  *) _CALLRELAY_DEFAULT_CACHE="$_CALLRELAY_APP_ROOT/.build-cache" ;;
esac
export CALLRELAY_CACHE_ROOT="${CALLRELAY_CACHE_ROOT:-$_CALLRELAY_DEFAULT_CACHE}"
export DERIVED_DATA_ROOT="${DERIVED_DATA_ROOT:-$CALLRELAY_CACHE_ROOT/xcode/release-variants}"
# TMPDIR from macOS normally points to the internal disk. Use a project override.
export TMPDIR="${CALLRELAY_TMPDIR:-$CALLRELAY_CACHE_ROOT/tmp}"
export GOCACHE="${GOCACHE:-$CALLRELAY_CACHE_ROOT/go/build}"
export GOMODCACHE="${GOMODCACHE:-$CALLRELAY_CACHE_ROOT/go/modules}"
export GOTMPDIR="${GOTMPDIR:-$CALLRELAY_CACHE_ROOT/go/tmp}"
mkdir -p "$CALLRELAY_CACHE_ROOT" "$TMPDIR" "$GOCACHE" "$GOMODCACHE" "$GOTMPDIR"
chmod 700 "$TMPDIR"
unset _CALLRELAY_APP_ROOT _CALLRELAY_CHECKOUT _CALLRELAY_VOLUME _CALLRELAY_DEFAULT_CACHE
