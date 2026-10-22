#!/usr/bin/env python3
"""verify-signed-ipa.py — audit a signed CallRelay .app/.ipa deterministically.

Thin CLI wrapper around `signing_lib.audit_artifact` (see that module for the
full check list).  Exit codes: 0 pass, 2 usage/IO, 3 audit failure.

The audit proves the package is internally consistent and carries the
profile-authorised identity entitlements.  It does NOT prove the app installs:
only the physical device gate can prove that.

No personal signing identity is hardcoded.  Pass `--identity` to enforce an
expectation against the keychain; without it the audit derives the embedded
profile and certificate chain from the artifact alone (handy for auditing
packages built elsewhere).  The signing flow reaches the identity through
`--identity`, a positional argument, or the CALLRELAY_SIGNING_IDENTITY
environment variable, as documented in signing_lib.py.
"""

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import signing_lib  # noqa: E402


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("artifact", help="signed CallRelay.app or .ipa")
    parser.add_argument("--profile", help="selected provisioning profile path")
    parser.add_argument("--identity",
                        help="expected codesigning identity (optional; when omitted "
                             "the audit derives the embedded profile/certificate "
                             "chain from the artifact, no keychain lookup)")
    parser.add_argument("--team")
    parser.add_argument("--expect-bundle-id", default="com.jiangnangenius.callrelay")
    parser.add_argument("--expect-version")
    parser.add_argument("--expect-build")
    parser.add_argument("--allow-version-mismatch", action="store_true")
    parser.add_argument("--json")
    args = parser.parse_args(argv)

    report, failures = signing_lib.audit_artifact(
        args.artifact,
        profile_path=args.profile,
        identity=args.identity,
        expect_bundle_id=args.expect_bundle_id,
        expect_team=args.team,
        expect_version=args.expect_version,
        expect_build=args.expect_build,
        allow_version_mismatch=args.allow_version_mismatch,
    )
    signing_lib._emit(report, args.json)
    if failures:
        print("AUDIT FAILED: %s" % ", ".join(failures), file=sys.stderr)
        return 3
    print(
        "AUDIT PASS (%s): bundle=%s version=%s build=%s profile=%s; "
        "device install requires the physical user gate"
        % (
            report.get("artifact_type"),
            report.get("bundle_id"),
            report.get("version"),
            report.get("build"),
            (report.get("profile") or {}).get("name"),
        ),
        file=sys.stderr,
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
