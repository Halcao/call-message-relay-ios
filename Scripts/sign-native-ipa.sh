#!/usr/bin/env bash
# sign-native-ipa.sh — sign an archived CallRelay app for direct device install
# and prove the package actually carries the profile-derived signing identity.
#
# Why: Codesign strict verification passes even when the signature omits
# `application-identifier`, `com.apple.developer.team-identifier` and
# `get-task-allow` (this shipped broken in 0.3.25 build 32).  iOS rejects such
# a package at install time.  This script:
#   1. selects a provisioning profile that matches the bundle id AND is
#      authorised by the actual keychain signing certificate (never just the
#      first profile with a matching app id),
#   2. merges the profile's identity entitlements with the repo capability
#      entitlements (repo narrower values win; every repo key must be granted
#      by the profile),
#   3. signs nested code deepest-first, then the app,
#   4. audits signature, entitlements, profile, icon, architecture and (for
#      the IPA) ZIP layout via Scripts/verify-signed-ipa.py.
#
# The audit deliberately does NOT claim the app installs — physical install on
# the iPhone is the user gate.
#
# Usage:
#   sign-native-ipa.sh <app-or-xcarchive> <out.ipa> [identity] \
#       [--identity NAME] [--profile <path-or-uuid>] [--expect-version X] \
#       [--expect-build N] [--report-dir DIR]
#
# The signing identity is REQUIRED and never hardcoded or guessed.  Provide
# it as the third positional argument, with --identity, or via the
# CALLRELAY_SIGNING_IDENTITY environment variable, e.g.
#   export CALLRELAY_SIGNING_IDENTITY="Apple Development: Your Name (TEAMID1234)"
#   Scripts/sign-native-ipa.sh CallRelay.xcarchive out/CallRelay.ipa
# Without an identity the script prints this usage and exits before any
# keychain query, staging or signing.  Other defaults: bundle id
# com.jiangnangenius.callrelay.
set -euo pipefail

source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/build-env.sh"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LIB="$SCRIPT_DIR/signing_lib.py"
VERIFY="$SCRIPT_DIR/verify-signed-ipa.py"
PYTHON="${PYTHON:-/usr/bin/python3}"

APP_ID="com.jiangnangenius.callrelay"

usage() {
  awk 'NR == 1 { next }
       /^set -euo/ { exit }
       { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"
  exit 2
}

[ $# -ge 2 ] || usage
SOURCE="$1"; shift
OUTPUT="$1"; shift

# Identity precedence: --identity flag > third positional > environment.
# Nothing is hardcoded; an unconfigured identity fails here, before the
# keychain, staging directory or codesign are touched.
IDENTITY="${CALLRELAY_SIGNING_IDENTITY:-}"
PROFILE_ARG=""
EXPECT_VERSION=""
EXPECT_BUILD=""
REPORT_DIR=""

if [ $# -gt 0 ] && [[ "$1" != --* ]]; then
  IDENTITY="$1"; shift
fi
while [ $# -gt 0 ]; do
  case "$1" in
    --profile) PROFILE_ARG="${2:?--profile needs a value}"; shift 2 ;;
    --expect-version) EXPECT_VERSION="${2:?}"; shift 2 ;;
    --expect-build) EXPECT_BUILD="${2:?}"; shift 2 ;;
    --report-dir) REPORT_DIR="${2:?}"; shift 2 ;;
    --identity) IDENTITY="${2:?--identity needs a non-empty value}"; shift 2 ;;
    -h|--help) usage ;;
    *) echo "ERROR: unknown argument: $1" >&2; usage ;;
  esac
done

if [ -z "$IDENTITY" ]; then
  echo "ERROR: no signing identity configured." >&2
  echo "       Pass it as the third argument, use --identity NAME, or set" >&2
  echo "       CALLRELAY_SIGNING_IDENTITY (see the usage below)." >&2
  usage
fi

if [[ "$SOURCE" == *.xcarchive ]]; then
  APP_PATH="$SOURCE/Products/Applications/CallRelay.app"
else
  APP_PATH="$SOURCE"
fi
test -f "$APP_PATH/Info.plist" || { echo "ERROR: no Info.plist in $APP_PATH" >&2; exit 2; }
test -f "$APP_PATH/CallRelay" || { echo "ERROR: no CallRelay executable in $APP_PATH" >&2; exit 2; }

REPO_ENTITLEMENTS="$(cd "$SCRIPT_DIR/.." && pwd)/CallRelay/CallRelay.entitlements"
test -f "$REPO_ENTITLEMENTS" || { echo "ERROR: missing $REPO_ENTITLEMENTS" >&2; exit 2; }

if [ -z "$REPORT_DIR" ]; then
  REPORT_DIR="$(dirname "$OUTPUT")/$(basename "$OUTPUT" .ipa)-evidence"
fi
mkdir -p "$REPORT_DIR"

STAGING="$(mktemp -d "${TMPDIR:-/tmp}/callrelay-sign.XXXXXX")"
trap 'rm -rf "$STAGING"' EXIT
APP="$STAGING/Payload/CallRelay.app"
mkdir -p "$STAGING/Payload"
ditto --norsrc --noextattr --noqtn "$APP_PATH" "$APP"

VERSION="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' "$APP_PATH/Info.plist")"
BUILD="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleVersion' "$APP_PATH/Info.plist")"
[ -n "$VERSION" ] && [ -n "$BUILD" ] || { echo "ERROR: app has no version metadata" >&2; exit 2; }
[ -n "$EXPECT_VERSION" ] || EXPECT_VERSION="$VERSION"
[ -n "$EXPECT_BUILD" ] || EXPECT_BUILD="$BUILD"

# 1. Select + validate the provisioning profile against the real keychain cert.
SELECT_ARGS=(select-profile --bundle-id "$APP_ID" --identity "$IDENTITY"
             --report "$REPORT_DIR/profile-selection.json")
if [ -n "$PROFILE_ARG" ]; then
  SELECT_ARGS+=(--profile "$PROFILE_ARG")
fi
PROFILE="$("$PYTHON" "$LIB" "${SELECT_ARGS[@]}" | tail -n 1)"
test -f "$PROFILE" || { echo "ERROR: profile selection produced no path" >&2; exit 3; }
echo "Selected profile: $PROFILE"

# 2. Merge profile identity entitlements with the repo capability values.
SIGNING_ENTITLEMENTS="$STAGING/signing-entitlements.plist"
"$PYTHON" "$LIB" make-entitlements \
  --bundle-id "$APP_ID" \
  --profile "$PROFILE" \
  --repo-entitlements "$REPO_ENTITLEMENTS" \
  --out "$SIGNING_ENTITLEMENTS" \
  --report "$REPORT_DIR/entitlement-merge.json"

# 3. Embed the profile before sealing the app, then sign nested code
#    deepest-first (frameworks/appex/dylibs; static .a archives are untouched).
cp "$PROFILE" "$APP/embedded.mobileprovision"
while IFS= read -r item; do
  [ -e "$item" ] || continue
  echo "Signing nested: ${item#"$APP"/}"
  /usr/bin/codesign --force --sign "$IDENTITY" --timestamp=none "$item"
done < <(find "$APP" -depth \( -name '*.framework' -o -name '*.appex' -o -name '*.dylib' \) -print)

/usr/bin/codesign --force --sign "$IDENTITY" \
  --entitlements "$SIGNING_ENTITLEMENTS" --timestamp=none "$APP"

# 4. Audit the sealed app before packaging.
"$PYTHON" "$VERIFY" "$APP" \
  --profile "$PROFILE" \
  --identity "$IDENTITY" \
  --expect-bundle-id "$APP_ID" \
  --expect-version "$EXPECT_VERSION" \
  --expect-build "$EXPECT_BUILD" \
  --json "$REPORT_DIR/audit-app.json"

# 5. Package and audit the IPA (ZIP layout, CRC, embedded copy).
mkdir -p "$(dirname "$OUTPUT")"
rm -f "$OUTPUT"
ditto -c -k --norsrc --noextattr --noqtn --keepParent "$STAGING/Payload" "$OUTPUT"
"$PYTHON" "$VERIFY" "$OUTPUT" \
  --profile "$PROFILE" \
  --identity "$IDENTITY" \
  --expect-bundle-id "$APP_ID" \
  --expect-version "$EXPECT_VERSION" \
  --expect-build "$EXPECT_BUILD" \
  --json "$REPORT_DIR/audit-ipa.json"

SHA="$(shasum -a 256 "$OUTPUT" | awk '{print $1}')"
echo "Signed CallRelay $VERSION ($BUILD)"
echo "IPA: $OUTPUT"
echo "SHA-256: $SHA"
echo "Evidence: $REPORT_DIR"
echo "NOT PROVEN HERE: physical installation on the iPhone (user gate)."
