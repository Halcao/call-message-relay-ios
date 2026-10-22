#!/usr/bin/env python3
"""Signing helpers and validators for CallRelay native IPA packaging.

This module exists because `codesign --verify --strict` alone does not prove an
iOS package can install on a device.  A development build needs the signing
entitlements to carry the profile-derived identity keys
(`application-identifier`, `com.apple.developer.team-identifier`,
`get-task-allow`) in addition to the app's capability entitlements, and the
selected provisioning profile must actually authorise the signing certificate
found in the keychain.

The CLI is intentionally dependency-free (stdlib + macOS tools) so the release
runbook can call it from `sign-native-ipa.sh` and from regression tests.

Signing identity is configured, never hardcoded and never guessed: the
signing path (`select-profile`) requires an identity supplied with
`--identity` or the `CALLRELAY_SIGNING_IDENTITY` environment variable and
exits with a usage error before any keychain query when neither is set.
Audit commands treat `--identity` as an optional expectation: without it the
audit derives the embedded profile and certificate chain from the artifact
alone, with no keychain lookup; with it the expectation is strictly checked.

Subcommands:
  inspect-profile    decode a .mobileprovision into a redacted JSON summary
  select-profile     choose a usable profile for a bundle id + identity
  make-entitlements  merge profile identity keys with repo capability values
  audit              full signature/package audit of a .app or .ipa
  compare            compare two signed apps'/IPAs' signing material

Exit codes: 0 pass, 2 usage/IO error, 3 validation failure.

Security: raw device UDIDs are never emitted; they are only reported as counts
and SHA-1 prefixes.
"""

from __future__ import annotations

import argparse
import datetime as _dt
import fnmatch
import hashlib
import json
import os
import plistlib
import re
import shutil
import subprocess
import sys
import tempfile

ENV_IDENTITY = "CALLRELAY_SIGNING_IDENTITY"
NO_IDENTITY_HELP = (
    "no signing identity configured: pass --identity NAME or set the "
    "%s environment variable to your own codesigning identity "
    '(e.g. "Apple Development: Your Name (TEAMID1234)"); no identity is '
    "hardcoded and none is guessed from the keychain" % ENV_IDENTITY
)
DEFAULT_PROFILE_DIRS = (
    os.path.expanduser("~/Library/Developer/Xcode/UserData/Provisioning Profiles"),
    os.path.expanduser("~/Library/MobileDevice/Provisioning Profiles"),
)
IDENTITY_KEYS = (
    "application-identifier",
    "com.apple.developer.team-identifier",
    "get-task-allow",
)
SECURITY = "/usr/bin/security"
CODESIGN = "/usr/bin/codesign"
LIPO = "/usr/bin/lipo"
DITTO = "/usr/bin/ditto"
OPENSSL = "/usr/bin/openssl"


class SigningError(Exception):
    """Raised for expected validation/configuration failures (exit code 3)."""


class UsageError(SigningError):
    """Raised for bad inputs (exit code 2)."""


def configured_identity():
    """Signing identity from the CALLRELAY_SIGNING_IDENTITY environment, or None."""
    value = (os.environ.get(ENV_IDENTITY) or "").strip()
    return value or None


def resolve_identity(explicit=None):
    """Explicit --identity wins over the environment; a missing identity is a
    usage error, never a silent keychain guess and never a stringified None."""
    if explicit:
        return explicit
    identity = configured_identity()
    if not identity:
        raise UsageError(NO_IDENTITY_HELP)
    return identity


# --------------------------------------------------------------------------
# small process helpers
# --------------------------------------------------------------------------


def run(cmd, input_bytes=None):
    return subprocess.run(
        cmd,
        input=input_bytes,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def _read_plist(path):
    with open(path, "rb") as fh:
        return plistlib.load(fh)


def _write_plist(obj, path):
    os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
    with open(path, "wb") as fh:
        plistlib.dump(obj, fh, fmt=plistlib.FMT_XML, sort_keys=False)


def _sha1_hex(data):
    return hashlib.sha1(data).hexdigest()


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


# --------------------------------------------------------------------------
# provisioning profiles
# --------------------------------------------------------------------------


def decode_profile(path):
    """Decode a CMS/PKCS#7 .mobileprovision into a plist dict."""
    if not os.path.isfile(path):
        raise UsageError("provisioning profile not found: %s" % path)
    proc = run([SECURITY, "cms", "-D", "-i", path])
    if proc.returncode != 0:
        raise SigningError(
            "cannot decode provisioning profile %s: %s"
            % (path, proc.stderr.decode("utf-8", "replace").strip())
        )
    try:
        profile = plistlib.loads(proc.stdout)
    except Exception as exc:  # pylint: disable=broad-except
        raise SigningError("invalid provisioning profile plist %s: %s" % (path, exc))
    if not isinstance(profile, dict):
        raise SigningError("unexpected provisioning profile shape: %s" % path)
    return profile


def profile_uuid(profile):
    return profile.get("UUID")


def profile_name(profile):
    return profile.get("Name")


def profile_team(profile):
    team = profile.get("TeamIdentifier")
    if isinstance(team, list) and team:
        return team[0]
    if isinstance(team, str):
        return team
    return None


def profile_app_id(profile):
    return (profile.get("Entitlements") or {}).get("application-identifier")


def profile_entitlements(profile):
    return dict(profile.get("Entitlements") or {})


def profile_device_count(profile):
    devices = profile.get("ProvisionedDevices")
    return len(devices) if isinstance(devices, list) else 0


def profile_cert_fingerprints(profile):
    certs = profile.get("DeveloperCertificates") or []
    return [_sha1_hex(bytes(c)) for c in certs]


def profile_expiration(profile):
    exp = profile.get("ExpirationDate")
    if isinstance(exp, _dt.datetime):
        return exp
    return None


def profile_summary(profile):
    """Redacted summary: no raw UDIDs or certificate bytes."""
    return {
        "name": profile_name(profile),
        "uuid": profile_uuid(profile),
        "team": profile_team(profile),
        "application_identifier": profile_app_id(profile),
        "get_task_allow": (profile.get("Entitlements") or {}).get("get-task-allow"),
        "expiration": str(profile_expiration(profile)),
        "creation": str(profile.get("CreationDate")),
        "device_count": profile_device_count(profile),
        "certificate_count": len(profile.get("DeveloperCertificates") or []),
        "certificate_fingerprints": profile_cert_fingerprints(profile),
        "platform": profile.get("Platform"),
    }


def app_id_covers(pattern, team, bundle_id):
    """Return True when `pattern` (possibly containing `*`) covers TEAM.bundle."""
    if not pattern:
        return False
    exact = "%s.%s" % (team, bundle_id)
    if pattern == exact:
        return True
    if pattern in ("*", "%s.*" % team):
        return True
    if pattern.endswith("*") and exact.startswith(pattern[:-1]):
        return True
    return False


# --------------------------------------------------------------------------
# keychain identity
# --------------------------------------------------------------------------


def find_identity_fingerprints(identity):
    """SHA-1 fingerprints of valid codesigning identities with this name."""
    proc = run([SECURITY, "find-identity", "-v", "-p", "codesigning"])
    if proc.returncode != 0:
        raise SigningError("security find-identity failed: %s" % proc.stderr)
    fingerprints = []
    for line in proc.stdout.decode("utf-8", "replace").splitlines():
        match = re.match(r'\s*\d+\)\s+([0-9A-Fa-f]{40})\s+"(.*)"', line)
        if match and match.group(2) == identity:
            fingerprints.append(match.group(1).lower())
    return fingerprints


def identity_certificate_der(identity):
    """Return the DER certificate for the keychain identity.

    Multiple certificates may share a common name; only certificates that
    `security find-identity` reports as valid codesigning identities are
    accepted.  A name that resolves to more than one valid certificate is an
    error (ambiguous signing identity).
    """
    valid = set(find_identity_fingerprints(identity))
    if not valid:
        raise SigningError("no valid codesigning identity named %r" % identity)
    proc = run([SECURITY, "find-certificate", "-a", "-c", identity, "-p"])
    if proc.returncode != 0 or not proc.stdout:
        raise SigningError("cannot read keychain certificate for %r" % identity)
    matches = []
    for pem in proc.stdout.split(b"-----BEGIN CERTIFICATE-----"):
        if b"-----END CERTIFICATE-----" not in pem:
            continue
        der_proc = run(
            [OPENSSL, "x509", "-inform", "PEM", "-outform", "DER"],
            input_bytes=b"-----BEGIN CERTIFICATE-----" + pem.split(b"-----END CERTIFICATE-----")[0] + b"-----END CERTIFICATE-----\n",
        )
        if der_proc.returncode != 0:
            continue
        fingerprint = _sha1_hex(der_proc.stdout)
        if fingerprint in valid:
            matches.append((fingerprint, der_proc.stdout))
    fingerprints = sorted({fp for fp, _ in matches})
    if not fingerprints:
        raise SigningError(
            "certificate(s) named %r do not match any valid codesigning identity" % identity
        )
    if len(fingerprints) > 1:
        raise SigningError(
            "ambiguous signing identity %r: matches %s; pass a unique identity"
            % (identity, ", ".join(fingerprints))
        )
    fingerprint = fingerprints[0]
    der = next(der for fp, der in matches if fp == fingerprint)
    return fingerprint, der


def profile_authorizes_certificate(profile, fingerprint):
    return fingerprint.lower() in profile_cert_fingerprints(profile)


# --------------------------------------------------------------------------
# profile candidate selection
# --------------------------------------------------------------------------


def _candidate_paths(profile_dirs):
    seen = set()
    for directory in profile_dirs:
        if not directory or not os.path.isdir(directory):
            continue
        for entry in sorted(os.listdir(directory)):
            if not entry.endswith(".mobileprovision"):
                continue
            path = os.path.join(directory, entry)
            if path not in seen:
                seen.add(path)
                yield path


def _resolve_explicit_profile(value, profile_dirs):
    if os.path.isfile(value):
        return value
    token = value.strip().lower()
    for path in _candidate_paths(profile_dirs):
        stem = os.path.splitext(os.path.basename(path))[0].lower()
        if stem == token or stem.startswith(token):
            return path
    raise UsageError("provisioning profile not found by path/uuid: %s" % value)


def evaluate_profile(profile, bundle_id, team, fingerprint, now=None, require_dev=True):
    """Return (ok, reasons, hard_checks)."""
    now = now or _dt.datetime.utcnow()
    reasons = []
    ent = profile_entitlements(profile)
    prof_team = profile_team(profile)
    prof_app_id = ent.get("application-identifier")
    if not prof_team or not prof_app_id:
        reasons.append("profile is missing TeamIdentifier/application-identifier")
    else:
        if team and prof_team != team:
            reasons.append("team %s != expected %s" % (prof_team, team))
        if not app_id_covers(prof_app_id, prof_team, bundle_id):
            reasons.append(
                "application-identifier %s does not cover %s.%s"
                % (prof_app_id, prof_team, bundle_id)
            )
    exp = profile_expiration(profile)
    if exp is None:
        reasons.append("profile has no ExpirationDate")
    elif exp <= now:
        reasons.append("profile expired at %s" % exp)
    if require_dev and ent.get("get-task-allow") is not True:
        reasons.append("profile is not a development profile (get-task-allow != true)")
    if fingerprint and not profile_authorizes_certificate(profile, fingerprint):
        reasons.append("profile does not authorise signing certificate %s" % fingerprint)
    if not profile.get("ProvisionsAllDevices") and profile_device_count(profile) == 0:
        reasons.append("profile lists no provisioned devices")
    if reasons:
        return False, reasons
    return True, []


def select_profile(bundle_id, identity, explicit=None, team=None, profile_dirs=None,
                   require_dev=True):
    """Select a usable development profile; returns (path, profile, report).

    A signing identity is mandatory here: choosing a profile without proving
    it authorises the real keychain certificate could silently package an
    IPA that iOS refuses to install, so an empty identity is rejected with a
    usage error before the keychain or any profile directory is touched.
    """
    if not identity:
        raise UsageError(NO_IDENTITY_HELP)
    profile_dirs = list(profile_dirs or DEFAULT_PROFILE_DIRS)
    fingerprint, _ = identity_certificate_der(identity)
    report = {"bundle_id": bundle_id, "identity": identity,
              "identity_fingerprint": fingerprint, "candidates": []}
    if explicit:
        path = _resolve_explicit_profile(explicit, profile_dirs)
        profile = decode_profile(path)
        ok, reasons = evaluate_profile(profile, bundle_id, team, fingerprint,
                                       require_dev=require_dev)
        report["candidates"].append({
            "path": path, "summary": profile_summary(profile),
            "ok": ok, "reasons": reasons})
        if not ok:
            raise SigningError(
                "explicit profile %s is not usable: %s" % (path, "; ".join(reasons))
            )
        report["selected"] = path
        report["selection"] = "explicit"
        return path, profile, report

    usable = []
    for path in _candidate_paths(profile_dirs):
        try:
            profile = decode_profile(path)
        except SigningError as exc:
            report["candidates"].append({"path": path, "ok": False,
                                         "reasons": [str(exc)]})
            continue
        ok, reasons = evaluate_profile(profile, bundle_id, team, fingerprint,
                                       require_dev=require_dev)
        summary = profile_summary(profile)
        report["candidates"].append({"path": path, "summary": summary,
                                     "ok": ok, "reasons": reasons})
        if ok:
            creation = profile.get("CreationDate")
            if not isinstance(creation, _dt.datetime):
                creation = _dt.datetime.min
            exact = 0 if profile_app_id(profile) == "%s.%s" % (profile_team(profile), bundle_id) else 1
            creation_rank = (creation.date().toordinal(),
                             creation.hour * 3600 + creation.minute * 60 + creation.second)
            usable.append((exact, -creation_rank[0], -creation_rank[1],
                           str(summary["uuid"]), path, profile))
    if not usable:
        raise SigningError(
            "no usable development profile for %s (certificate %s); candidates: %s"
            % (bundle_id, fingerprint, report["candidates"])
        )
    usable.sort(key=lambda item: (item[0], item[1], item[2], item[3]))
    exact, _, _, _, path, profile = usable[0]
    report["selected"] = path
    report["selection"] = "best-match exact-app-id" if exact == 0 else "best-match wildcard"
    return path, profile, report


# --------------------------------------------------------------------------
# entitlement merge + subset validation
# --------------------------------------------------------------------------


def _allowed_value(app_value, profile_value, key):
    """Return None when allowed, else a human-readable reason."""
    if profile_value == "*":
        return None
    if isinstance(profile_value, bool) or isinstance(app_value, bool):
        if profile_value != app_value:
            return "value %r != profile %r" % (app_value, profile_value)
        return None
    if isinstance(profile_value, str):
        if isinstance(app_value, str):
            if app_value == profile_value or fnmatch.fnmatchcase(app_value, profile_value):
                return None
            return "%r not allowed by profile pattern %r" % (app_value, profile_value)
        return "profile declares string but app value is %s" % type(app_value).__name__
    if isinstance(profile_value, list):
        items = app_value if isinstance(app_value, list) else [app_value]
        for item in items:
            if item == "*":
                continue
            if not any(
                (pv == "*")
                or (pv == item)
                or (isinstance(pv, str) and isinstance(item, str)
                    and fnmatch.fnmatchcase(item, pv))
                for pv in profile_value
            ):
                return "%r is not listed in profile %r" % (item, profile_value)
        return None
    if isinstance(profile_value, dict):
        if not isinstance(app_value, dict):
            return "profile declares dict but app value is %s" % type(app_value).__name__
        for sub_key, sub_value in app_value.items():
            if sub_key not in profile_value:
                return "nested key %r not in profile" % sub_key
            reason = _allowed_value(sub_value, profile_value[sub_key],
                                    "%s.%s" % (key, sub_key))
            if reason:
                return reason
        return None
    if isinstance(profile_value, int) and isinstance(app_value, int):
        if profile_value != app_value:
            return "value %r != profile %r" % (app_value, profile_value)
        return None
    if app_value != profile_value:
        return "value %r != profile %r" % (app_value, profile_value)
    return None


def validate_entitlements_subset(app_entitlements, profile_entitlements_map):
    """Validate every app entitlement is allowed by the profile.

    Returns a list of failure strings (empty when valid).
    """
    failures = []
    for key, value in sorted(app_entitlements.items()):
        if key not in profile_entitlements_map:
            failures.append("entitlement %r is not granted by the profile" % key)
            continue
        reason = _allowed_value(value, profile_entitlements_map[key], key)
        if reason:
            failures.append("entitlement %r: %s" % (key, reason))
    return failures


def signing_identity_failures(entitlements, expected_team, bundle_id,
                              expected_aps="development"):
    """Mandatory signing-identity entitlements for a device-installable build.

    This is the regression guard for the 0.3.25 (32) defect: codesign strict
    verification passed while `application-identifier`,
    `com.apple.developer.team-identifier` and `get-task-allow` were absent.
    """
    failures = []
    exact_app_id = None
    if expected_team and bundle_id:
        exact_app_id = "%s.%s" % (expected_team, bundle_id)
    app_id = entitlements.get("application-identifier")
    if not app_id:
        failures.append("application-identifier is missing")
    elif exact_app_id and app_id != exact_app_id:
        failures.append("application-identifier=%r expected %r"
                        % (app_id, exact_app_id))
    team_id = entitlements.get("com.apple.developer.team-identifier")
    if not team_id:
        failures.append("com.apple.developer.team-identifier is missing")
    elif expected_team and team_id != expected_team:
        failures.append("com.apple.developer.team-identifier=%r expected %r"
                        % (team_id, expected_team))
    if entitlements.get("get-task-allow") is not True:
        failures.append("get-task-allow is not true")
    if expected_aps is not None and entitlements.get("aps-environment") != expected_aps:
        failures.append("aps-environment=%r expected %r"
                        % (entitlements.get("aps-environment"), expected_aps))
    return failures


def merge_signing_entitlements(bundle_id, repo_entitlements, profile):
    """Merge repo capability entitlements with profile identity entitlements."""
    if repo_entitlements is None:
        raise UsageError("repository entitlements are required")
    prof_ent = profile_entitlements(profile)
    team = profile_team(profile)
    prof_app_id = prof_ent.get("application-identifier")
    if not team:
        raise SigningError("profile has no TeamIdentifier")
    if not prof_app_id or not app_id_covers(prof_app_id, team, bundle_id):
        raise SigningError(
            "profile application-identifier %r does not cover %s.%s"
            % (prof_app_id, team, bundle_id)
        )
    if prof_ent.get("get-task-allow") is not True:
        raise SigningError("profile is not a development profile (get-task-allow != true)")
    failures = validate_entitlements_subset(repo_entitlements, prof_ent)
    if failures:
        raise SigningError(
            "repository entitlements are not fully granted by profile %s: %s"
            % (profile_name(profile), "; ".join(failures))
        )
    merged = dict(repo_entitlements)
    exact_app_id = "%s.%s" % (team, bundle_id)
    merged["application-identifier"] = exact_app_id
    merged["com.apple.developer.team-identifier"] = team
    merged["get-task-allow"] = True
    return merged


# --------------------------------------------------------------------------
# app/IPA helpers
# --------------------------------------------------------------------------


def entitlements_of_app(app_path):
    proc = run([CODESIGN, "-d", "--entitlements", ":-", app_path])
    if proc.returncode != 0 or not proc.stdout:
        raise SigningError(
            "cannot read entitlements from %s (unsigned or invalid): %s"
            % (app_path, proc.stderr.decode("utf-8", "replace").strip())
        )
    try:
        ents = plistlib.loads(proc.stdout)
    except Exception as exc:  # pylint: disable=broad-except
        raise SigningError("cannot parse entitlements of %s: %s" % (app_path, exc))
    if not isinstance(ents, dict):
        raise SigningError("unexpected entitlements shape for %s" % app_path)
    return ents


def codesign_details(path):
    proc = run([CODESIGN, "-dv", "--verbose=4", path])
    text = proc.stderr.decode("utf-8", "replace") + proc.stdout.decode("utf-8", "replace")
    details = {}
    for line in text.splitlines():
        if "=" in line:
            key, _, value = line.partition("=")
            details[key.strip()] = value.strip()
    return details


def _extract_ipa(ipa_path, work_dir):
    proc = run([DITTO, "-x", "-k", "--norsrc", "--noextattr", "--noqtn",
                ipa_path, work_dir])
    if proc.returncode != 0:
        raise SigningError("cannot extract IPA %s: %s"
                           % (ipa_path, proc.stderr.decode("utf-8", "replace")))
    app_dir = os.path.join(work_dir, "Payload", "CallRelay.app")
    if not os.path.isdir(app_dir):
        raise SigningError("IPA Payload/CallRelay.app missing after extraction")
    return app_dir


def _zip_layout_failures(ipa_path):
    import zipfile

    failures = []
    try:
        with zipfile.ZipFile(ipa_path) as archive:
            bad = archive.testzip()
            if bad is not None:
                failures.append("ZIP CRC failure at %s" % bad)
            names = archive.namelist()
    except Exception as exc:  # pylint: disable=broad-except
        return ["cannot open IPA as ZIP: %s" % exc]
    if not names:
        failures.append("IPA contains no entries")
        return failures
    top = set()
    for name in names:
        if name.startswith("/") or "\\" in name:
            failures.append("unsafe ZIP entry %r" % name)
        parts = name.split("/")
        if ".." in parts:
            failures.append("path-traversal ZIP entry %r" % name)
        top.add(parts[0])
    if top != {"Payload"}:
        failures.append("expected exactly one top-level Payload/ directory, saw %s"
                        % sorted(top))
    required = [
        "Payload/CallRelay.app/Info.plist",
        "Payload/CallRelay.app/CallRelay",
        "Payload/CallRelay.app/embedded.mobileprovision",
        "Payload/CallRelay.app/_CodeSignature/CodeResources",
    ]
    for entry in required:
        if entry not in names:
            failures.append("IPA is missing %s" % entry)
    if any(n.startswith("__MACOSX") for n in names):
        failures.append("IPA contains __MACOSX metadata entries")
    return failures


def _nested_code_paths(app_path):
    paths = []
    frameworks = os.path.join(app_path, "Frameworks")
    if os.path.isdir(frameworks):
        for entry in sorted(os.listdir(frameworks)):
            full = os.path.join(frameworks, entry)
            if entry.endswith((".framework", ".appex")):
                paths.append(full)
            elif entry.endswith(".dylib"):
                paths.append(full)
    plugins = os.path.join(app_path, "PlugIns")
    if os.path.isdir(plugins):
        for entry in sorted(os.listdir(plugins)):
            if entry.endswith((".appex", ".framework")):
                paths.append(os.path.join(plugins, entry))
    return paths


def _executable_in_bundle(bundle_path):
    info_path = os.path.join(bundle_path, "Info.plist")
    if os.path.isfile(info_path):
        try:
            exe = _read_plist(info_path).get("CFBundleExecutable")
            if exe and os.path.isfile(os.path.join(bundle_path, exe)):
                return os.path.join(bundle_path, exe)
        except Exception:  # pylint: disable=broad-except
            pass
    fallback = os.path.join(bundle_path, os.path.basename(bundle_path).rsplit(".", 1)[0])
    return fallback if os.path.isfile(fallback) else None


def _archs_of(binary):
    proc = run([LIPO, "-archs", binary])
    if proc.returncode != 0:
        return []
    return proc.stdout.decode("utf-8", "replace").split()


# --------------------------------------------------------------------------
# audit
# --------------------------------------------------------------------------


def _hard_failures(checks):
    return [c for c in checks if c["status"] == "fail"]


def audit_artifact(artifact, profile_path=None, identity=None, expect_bundle_id=None,
                   expect_team=None, expect_version=None, expect_build=None,
                   allow_version_mismatch=False, work_parent=None):
    """Audit a signed .app or .ipa.  Returns (report, failures list).

    The report is JSON-serialisable.  `device_install_proof` is always False:
    only a physical install by the user can prove installability.
    """
    checks = []
    notes = []
    artifact = os.path.abspath(artifact)
    is_ipa = artifact.endswith(".ipa")

    def check(name, ok, detail):
        checks.append({"name": name, "status": "pass" if ok else "fail",
                       "detail": detail})
        return ok

    if not os.path.exists(artifact):
        raise UsageError("artifact not found: %s" % artifact)

    report = {
        "artifact": artifact,
        "artifact_type": "ipa" if is_ipa else "app",
        "sha256": sha256_file(artifact) if os.path.isfile(artifact) else None,
        "size_bytes": os.path.getsize(artifact) if os.path.isfile(artifact) else None,
        "device_install_proof": False,
        "device_install_note": (
            "codesign/package audit only; physical installation on the target "
            "iPhone remains the user gate"
        ),
    }

    temp_dir = None
    try:
        if is_ipa:
            temp_dir = tempfile.mkdtemp(prefix="callrelay-audit-",
                                        dir=work_parent or None)
            layout_failures = _zip_layout_failures(artifact)
            check("ipa_zip_layout", not layout_failures,
                  "ok" if not layout_failures else "; ".join(layout_failures))
            app_dir = _extract_ipa(artifact, temp_dir)
        else:
            app_dir = artifact
            if os.path.isdir(app_dir):
                check("bundle_directory", True, "signed .app directory")
            else:
                raise UsageError("not a .app directory or .ipa: %s" % artifact)

        info = _read_plist(os.path.join(app_dir, "Info.plist"))
        report["bundle_id"] = info.get("CFBundleIdentifier")
        report["version"] = info.get("CFBundleShortVersionString")
        report["build"] = info.get("CFBundleVersion")
        report["minimum_os"] = info.get("MinimumOSVersion")
        report["platform"] = info.get("DTPlatformName") or info.get("CFBundleSupportedPlatforms")

        # --- bundle identity/version -------------------------------------
        if expect_bundle_id:
            check("bundle_id_exact", report["bundle_id"] == expect_bundle_id,
                  "Info.plist CFBundleIdentifier=%r expected %r"
                  % (report["bundle_id"], expect_bundle_id))
        else:
            check("bundle_id_present", bool(report["bundle_id"]),
                  "CFBundleIdentifier=%r" % report["bundle_id"])
        for label, expected, actual in (
            ("version", expect_version, report["version"]),
            ("build", expect_build, report["build"]),
        ):
            if expected:
                ok = actual == expected
                if not ok and allow_version_mismatch:
                    check("%s_metadata" % label, True,
                          "actual %r != requested %r (allowed mismatch: audit-only)"
                          % (actual, expected))
                else:
                    check("%s_metadata" % label, ok,
                          "actual %r expected %r" % (actual, expected))
        check("executable_present", os.path.isfile(os.path.join(app_dir, "CallRelay")),
              "CallRelay executable in bundle")
        if os.path.isfile(os.path.join(app_dir, "CallRelay")):
            archs = _archs_of(os.path.join(app_dir, "CallRelay"))
            report["executable_architectures"] = archs
            check("executable_arm64", "arm64" in archs, "archs=%s" % archs)
        else:
            check("executable_arm64", False, "missing executable")

        # --- icon assets --------------------------------------------------
        icon_png = os.path.join(app_dir, "AppIcon60x60@2x.png")
        icon_ok = os.path.isfile(icon_png) and os.path.getsize(icon_png) > 0
        if icon_ok:
            with open(icon_png, "rb") as fh:
                icon_ok = fh.read(8) == b"\x89PNG\r\n\x1a\n"
        assets = os.path.join(app_dir, "Assets.car")
        assets_ok = os.path.isfile(assets) and os.path.getsize(assets) > 0
        info_icons = info.get("CFBundleIcons") or info.get("CFBundleIcons~ipad") or {}
        icon_name = None
        for key in ("CFBundleIcons~ipad", "CFBundleIcons"):
            primary = (info.get(key) or {}).get("CFBundlePrimaryIcon") or {}
            icon_name = primary.get("CFBundleIconName") or icon_name
        check("icon_assets", icon_ok and assets_ok and bool(icon_name),
              "AppIcon60x60@2x.png=%s Assets.car=%s CFBundleIconName=%r"
              % (icon_ok, assets_ok, icon_name))

        # --- embedded provisioning profile --------------------------------
        embedded_path = os.path.join(app_dir, "embedded.mobileprovision")
        embedded_ok = os.path.isfile(embedded_path)
        check("embedded_profile_present", embedded_ok, embedded_path)
        profile = None
        if embedded_ok:
            try:
                profile = decode_profile(embedded_path)
            except SigningError as exc:
                check("embedded_profile_decodes", False, str(exc))
        if profile is not None:
            summary = profile_summary(profile)
            report["profile"] = summary
            check("embedded_profile_decodes", True,
                  "uuid=%s name=%r" % (summary["uuid"], summary["name"]))
            now = _dt.datetime.utcnow()
            exp = profile_expiration(profile)
            check("profile_not_expired", exp is not None and exp > now,
                  "expiration=%s" % exp)
            p_team = profile_team(profile)
            p_app_id = profile_app_id(profile)
            check("profile_covers_bundle",
                  bool(p_team and app_id_covers(p_app_id, p_team, report["bundle_id"] or "")),
                  "application-identifier=%r bundle=%r"
                  % (p_app_id, report["bundle_id"]))
            check("profile_is_development",
                  (profile.get("Entitlements") or {}).get("get-task-allow") is True,
                  "get-task-allow=%r"
                  % (profile.get("Entitlements") or {}).get("get-task-allow"))
            check("profile_has_devices",
                  bool(profile.get("ProvisionsAllDevices")) or profile_device_count(profile) > 0,
                  "provisioned_devices=%d" % profile_device_count(profile))
            fp = None
            if identity:
                fp, _ = identity_certificate_der(identity)
            if fp:
                check("profile_authorizes_identity",
                      profile_authorizes_certificate(profile, fp),
                      "identity certificate %s in profile certificates" % fp)
                report["identity_fingerprint"] = fp
            if expect_team:
                check("profile_team", p_team == expect_team,
                      "profile team %r expected %r" % (p_team, expect_team))
            if profile_path:
                try:
                    expected_profile = decode_profile(profile_path)
                    same = (profile_uuid(expected_profile) or "").lower() == \
                           (profile_uuid(profile) or "").lower()
                    check("embedded_profile_matches_selection", same,
                          "embedded uuid=%s selected uuid=%s"
                          % (profile_uuid(profile), profile_uuid(expected_profile)))
                except SigningError as exc:
                    check("embedded_profile_matches_selection", False, str(exc))

        # --- code signature + entitlements --------------------------------
        verify = run([CODESIGN, "--verify", "--strict", "--deep", "--verbose=2", app_dir])
        sig_ok = verify.returncode == 0
        check("codesign_strict_verify", sig_ok,
              "codesign --verify --strict --deep rc=%d %s"
              % (verify.returncode,
                 verify.stderr.decode("utf-8", "replace").strip()[:400]))

        ents = None
        try:
            ents = entitlements_of_app(app_dir)
        except SigningError as exc:
            check("entitlements_readable", False, str(exc))
        if ents is not None:
            report["entitlements"] = ents
            check("entitlements_readable", True, "keys=%d" % len(ents))
            expected_team = expect_team or (profile_team(profile) if profile else None)
            identity_failures = signing_identity_failures(
                ents, expected_team, report.get("bundle_id")
            )
            check("signing_identity_entitlements", not identity_failures,
                  "ok" if not identity_failures else "; ".join(identity_failures))
            app_id = ents.get("application-identifier")
            team_id = ents.get("com.apple.developer.team-identifier")
            exact_app_id = ("%s.%s" % (expected_team, report["bundle_id"])
                            if expected_team and report.get("bundle_id") else None)
            check("entitlement_application_identifier",
                  bool(app_id) and app_id == exact_app_id,
                  "application-identifier=%r expected %r" % (app_id, exact_app_id))
            check("entitlement_team_identifier",
                  bool(team_id) and (not expected_team or team_id == expected_team),
                  "team-identifier=%r" % team_id)
            check("entitlement_get_task_allow", ents.get("get-task-allow") is True,
                  "get-task-allow=%r" % ents.get("get-task-allow"))
            check("entitlement_aps_environment",
                  ents.get("aps-environment") == "development",
                  "aps-environment=%r" % ents.get("aps-environment"))
            if profile is not None:
                failures = validate_entitlements_subset(ents, profile_entitlements(profile))
                check("entitlements_subset_of_profile", not failures,
                      "ok" if not failures else "; ".join(failures))
            required_repo_keys = [
                "aps-environment",
                "com.apple.developer.icloud-container-environment",
                "com.apple.developer.icloud-container-identifiers",
                "com.apple.developer.icloud-services",
                "com.apple.developer.ubiquity-container-identifiers",
            ]
            missing = [k for k in required_repo_keys if k not in ents]
            check("capability_entitlements_preserved", not missing,
                  "missing=%s" % missing if missing else "ok")

        # --- nested code --------------------------------------------------
        nested = _nested_code_paths(app_dir)
        nested_ok = True
        nested_details = []
        for path in nested:
            result = run([CODESIGN, "--verify", "--strict", path])
            item_ok = result.returncode == 0
            if item_ok:
                details = codesign_details(path)
                team_seen = details.get("TeamIdentifier")
                if expected_team and team_seen and team_seen != expected_team:
                    item_ok = False
                    nested_details.append("%s: team %s != %s"
                                          % (os.path.basename(path), team_seen, expected_team))
                exe = _executable_in_bundle(path)
                if exe and "arm64" not in _archs_of(exe):
                    item_ok = False
                    nested_details.append("%s: executable missing arm64"
                                          % os.path.basename(path))
            else:
                nested_details.append("%s: codesign rc=%d"
                                      % (os.path.basename(path), result.returncode))
            nested_ok = nested_ok and item_ok
        check("nested_code_valid", nested_ok and bool(nested),
              "nested=%s %s" % ([os.path.basename(p) for p in nested],
                                "; ".join(nested_details) if nested_details else "ok"))

        # --- aggregate ----------------------------------------------------
        report["checks"] = checks
        report["failures"] = [c["name"] for c in _hard_failures(checks)]
        report["notes"] = notes
        report["verdict"] = "pass" if not report["failures"] else "fail"
        return report, report["failures"]
    finally:
        if temp_dir:
            shutil.rmtree(temp_dir, ignore_errors=True)


def compare_artifacts(reference, candidate, identity=None, work_parent=None):
    """Compare signing material of two .app/.ipa artifacts (redacted)."""
    temp_dirs = []

    def load(path):
        path = os.path.abspath(path)
        if path.endswith(".ipa"):
            tmp = tempfile.mkdtemp(prefix="callrelay-compare-", dir=work_parent or None)
            temp_dirs.append(tmp)
            return _extract_ipa(path, tmp)
        return path

    try:
        ref_app = load(reference)
        cand_app = load(candidate)
        ref_ents = entitlements_of_app(ref_app)
        cand_ents = entitlements_of_app(cand_app)
        ref_profile = None
        cand_profile = None
        for app, target in ((ref_app, "ref"), (cand_app, "cand")):
            embedded = os.path.join(app, "embedded.mobileprovision")
            if os.path.isfile(embedded):
                summary = profile_summary(decode_profile(embedded))
                if target == "ref":
                    ref_profile = summary
                else:
                    cand_profile = summary
        result = {
            "reference": os.path.abspath(reference),
            "candidate": os.path.abspath(candidate),
            "reference_entitlements": ref_ents,
            "candidate_entitlements": cand_ents,
            "keys_only_in_reference": sorted(set(ref_ents) - set(cand_ents)),
            "keys_only_in_candidate": sorted(set(cand_ents) - set(ref_ents)),
            "value_differences": {
                k: {"reference": ref_ents.get(k), "candidate": cand_ents.get(k)}
                for k in sorted(set(ref_ents) & set(cand_ents))
                if ref_ents.get(k) != cand_ents.get(k)
            },
            "identity_keys_equal": all(
                ref_ents.get(k) == cand_ents.get(k) for k in IDENTITY_KEYS
            ),
            "reference_profile": ref_profile,
            "candidate_profile": cand_profile,
        }
        return result
    finally:
        for tmp in temp_dirs:
            shutil.rmtree(tmp, ignore_errors=True)


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def _emit(report, json_path=None):
    text = json.dumps(report, indent=2, sort_keys=False, default=str)
    if json_path:
        os.makedirs(os.path.dirname(os.path.abspath(json_path)), exist_ok=True)
        with open(json_path, "w", encoding="utf-8") as fh:
            fh.write(text + "\n")
    return text


def _cmd_inspect_profile(args):
    profile = decode_profile(args.profile)
    print(_emit(profile_summary(profile), args.json))
    return 0


def _cmd_select_profile(args):
    path, profile, report = select_profile(
        args.bundle_id, resolve_identity(args.identity), explicit=args.profile,
        team=args.team, profile_dirs=args.profile_dir or None,
    )
    _emit(report, args.report)
    print(path)
    return 0


def _cmd_make_entitlements(args):
    profile = decode_profile(args.profile)
    repo = _read_plist(args.repo_entitlements)
    merged = merge_signing_entitlements(args.bundle_id, repo, profile)
    _write_plist(merged, args.out)
    report = {
        "profile": profile_summary(profile),
        "bundle_id": args.bundle_id,
        "repo_entitlements": repo,
        "merged_entitlements": merged,
        "identity_keys": [k for k in IDENTITY_KEYS],
        "output": args.out,
    }
    _emit(report, args.report)
    print("merged %d entitlements -> %s" % (len(merged), args.out))
    return 0


def _cmd_audit(args):
    report, failures = audit_artifact(
        args.artifact, profile_path=args.profile, identity=args.identity,
        expect_bundle_id=args.expect_bundle_id, expect_team=args.team,
        expect_version=args.expect_version, expect_build=args.expect_build,
        allow_version_mismatch=args.allow_version_mismatch,
    )
    text = _emit(report, args.json)
    print(text)
    if failures:
        print("AUDIT FAILED: %s" % ", ".join(failures), file=sys.stderr)
        return 3
    print("AUDIT PASS (device install still requires the physical user gate)",
          file=sys.stderr)
    return 0


def _cmd_compare(args):
    result = compare_artifacts(args.reference, args.candidate, identity=args.identity)
    print(_emit(result, args.json))
    if args.require_identity_equal and not result["identity_keys_equal"]:
        print("COMPARE FAILED: identity keys differ", file=sys.stderr)
        return 3
    return 0


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    p = sub.add_parser("inspect-profile", help="decode a profile to redacted JSON")
    p.add_argument("profile")
    p.add_argument("--json")
    p.set_defaults(func=_cmd_inspect_profile)

    p = sub.add_parser("select-profile", help="pick a usable profile for a bundle id")
    p.add_argument("--bundle-id", required=True)
    p.add_argument("--identity",
                   help="codesigning identity (required; falls back to the "
                        "CALLRELAY_SIGNING_IDENTITY environment variable)")
    p.add_argument("--team")
    p.add_argument("--profile", help="explicit path or UUID prefix")
    p.add_argument("--profile-dir", action="append")
    p.add_argument("--report")
    p.set_defaults(func=_cmd_select_profile)

    p = sub.add_parser("make-entitlements", help="merge repo + profile entitlements")
    p.add_argument("--bundle-id", required=True)
    p.add_argument("--profile", required=True)
    p.add_argument("--repo-entitlements", required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--report")
    p.set_defaults(func=_cmd_make_entitlements)

    p = sub.add_parser("audit", help="audit a signed .app or .ipa")
    p.add_argument("artifact")
    p.add_argument("--profile", help="expected selected profile (path)")
    p.add_argument("--identity",
                   help="expected codesigning identity (optional; when omitted the "
                        "audit derives the embedded profile/certificate chain from "
                        "the artifact alone, with no keychain lookup)")
    p.add_argument("--team")
    p.add_argument("--expect-bundle-id")
    p.add_argument("--expect-version")
    p.add_argument("--expect-build")
    p.add_argument("--allow-version-mismatch", action="store_true",
                   help="report version mismatch without failing (audit of old artifacts)")
    p.add_argument("--json")
    p.set_defaults(func=_cmd_audit)

    p = sub.add_parser("compare", help="compare reference vs candidate signing material")
    p.add_argument("--reference", required=True)
    p.add_argument("--candidate", required=True)
    p.add_argument("--identity",
                   help="optional label only; compare reads the embedded profiles "
                        "and entitlements from the artifacts, not the keychain")
    p.add_argument("--json")
    p.add_argument("--require-identity-equal", action="store_true")
    p.set_defaults(func=_cmd_compare)

    return parser


def main(argv=None):
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except UsageError as exc:
        print("ERROR: %s" % exc, file=sys.stderr)
        return 2
    except SigningError as exc:
        print("FAIL: %s" % exc, file=sys.stderr)
        return 3


if __name__ == "__main__":
    sys.exit(main())
