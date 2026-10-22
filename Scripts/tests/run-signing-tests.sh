#!/usr/bin/env bash
# run-signing-tests.sh — regression suite for the CallRelay signing toolchain.
#
# Runs the hermetic unit tests for Scripts/signing_lib.py.  When the frozen
# 0.3.25 (32) IPA is available (build/rollback/...), the suite also asserts the
# validator rejects it for missing signing-identity entitlements.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PYTHON="${PYTHON:-/usr/bin/python3}"

"$PYTHON" -m py_compile "$DIR/../signing_lib.py" "$DIR/../verify-signed-ipa.py"
cd "$DIR"
"$PYTHON" -m unittest discover -s "$DIR" -p 'test_*.py' -v
