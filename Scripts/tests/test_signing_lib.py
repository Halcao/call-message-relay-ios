#!/usr/bin/env python3
"""Regression tests for Scripts/signing_lib.py.

Covers the 0.3.25 (32) failure modes explicitly:
  * generated signing entitlements must include the profile-derived
    application-identifier / team-identifier / get-task-allow;
  * profiles that do not cover the bundle id (profile mismatch) or do not
    authorise the keychain certificate (certificate mismatch) must be rejected;
  * an app signature missing the identity entitlements must fail the audit
    helpers even when it has valid capability entitlements.

Run: Scripts/tests/run-signing-tests.sh  (or python3 -m unittest discover)
"""

import datetime as dt
import hashlib
import io
import json
import os
import plistlib
import re
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
SCRIPTS = os.path.dirname(HERE)
sys.path.insert(0, SCRIPTS)
import signing_lib as sl  # noqa: E402

REPO_ENTITLEMENTS_PATH = os.path.join(SCRIPTS, "..", "CallRelay", "CallRelay.entitlements")
TEAM = "TEAMID1234"  # fake test team id; nothing here is a real developer identity
BUNDLE = "com.jiangnangenius.callrelay"
CERT_DER = b"callrelay-test-certificate-der"
CERT_FP = hashlib.sha1(CERT_DER).hexdigest()
OTHER_CERT_FP = hashlib.sha1(b"another-certificate").hexdigest()


def repo_entitlements():
    with open(REPO_ENTITLEMENTS_PATH, "rb") as fh:
        return plistlib.load(fh)


def profile_entitlements(application_identifier=None):
    return {
        "application-identifier": application_identifier or "%s.%s" % (TEAM, BUNDLE),
        "aps-environment": "development",
        "com.apple.developer.icloud-container-development-container-identifiers":
            ["iCloud.com.jiangnangenius.callrelay"],
        "com.apple.developer.icloud-container-environment": ["Production", "Development"],
        "com.apple.developer.icloud-container-identifiers":
            ["iCloud.com.jiangnangenius.callrelay"],
        "com.apple.developer.icloud-services": "*",
        "com.apple.developer.team-identifier": TEAM,
        "com.apple.developer.ubiquity-container-identifiers":
            ["iCloud.com.jiangnangenius.callrelay"],
        "com.apple.developer.ubiquity-kvstore-identifier": "%s.*" % TEAM,
        "get-task-allow": True,
        "keychain-access-groups": ["%s.*" % TEAM, "com.apple.token"],
    }


def profile_dict(name="Test Profile", uuid="11111111-1111-1111-1111-111111111111",
                 entitlements=None, certs=(CERT_DER,), devices=("device-a",),
                 creation=None, expiration=None, team=TEAM):
    return {
        "Name": name,
        "UUID": uuid,
        "TeamIdentifier": [team],
        "CreationDate": creation or dt.datetime(2026, 10, 3, 12, 0, 0),
        "ExpirationDate": expiration or dt.datetime(2027, 10, 3, 12, 0, 0),
        "DeveloperCertificates": [bytes(c) for c in certs],
        "ProvisionedDevices": list(devices),
        "Platform": ["iOS"],
        "Entitlements": entitlements or profile_entitlements(),
    }


def write_profile(directory, filename):
    path = os.path.join(directory, filename)
    with open(path, "wb"):
        pass
    return path


class MergeEntitlementsTests(unittest.TestCase):
    def test_merge_adds_profile_identity_entitlements(self):
        merged = sl.merge_signing_entitlements(BUNDLE, repo_entitlements(),
                                               profile_dict())
        self.assertEqual(merged["application-identifier"], "%s.%s" % (TEAM, BUNDLE))
        self.assertEqual(merged["com.apple.developer.team-identifier"], TEAM)
        self.assertIs(merged["get-task-allow"], True)
        # repo capability values stay narrow, not the profile wildcard
        self.assertEqual(merged["com.apple.developer.icloud-services"], ["CloudKit"])
        self.assertEqual(merged["aps-environment"], "development")
        for key in ("application-identifier", "com.apple.developer.team-identifier",
                    "get-task-allow"):
            self.assertIn(key, merged)

    def test_merge_fails_when_profile_has_no_application_identifier(self):
        ents = profile_entitlements()
        del ents["application-identifier"]
        with self.assertRaises(sl.SigningError):
            sl.merge_signing_entitlements(BUNDLE, repo_entitlements(),
                                          profile_dict(entitlements=ents))

    def test_merge_fails_when_identifier_covers_other_bundle(self):
        with self.assertRaises(sl.SigningError):
            sl.merge_signing_entitlements(
                BUNDLE, repo_entitlements(),
                profile_dict(entitlements=profile_entitlements(
                    application_identifier="%s.com.example.other" % TEAM)))

    def test_merge_fails_when_profile_lacks_repo_capability(self):
        ents = profile_entitlements()
        del ents["com.apple.developer.icloud-services"]
        with self.assertRaises(sl.SigningError):
            sl.merge_signing_entitlements(BUNDLE, repo_entitlements(),
                                          profile_dict(entitlements=ents))

    def test_merge_fails_when_profile_is_distribution(self):
        ents = profile_entitlements()
        ents["get-task-allow"] = False
        with self.assertRaises(sl.SigningError):
            sl.merge_signing_entitlements(BUNDLE, repo_entitlements(),
                                          profile_dict(entitlements=ents))


class SubsetValidationTests(unittest.TestCase):
    def test_wildcard_and_list_subset_allowed(self):
        app = repo_entitlements()
        self.assertEqual(sl.validate_entitlements_subset(app, profile_entitlements()), [])

    def test_missing_capability_fails(self):
        failures = sl.validate_entitlements_subset(
            {"aps-environment": "development",
             "com.apple.developer.healthkit": True},
            {"aps-environment": "development"})
        self.assertTrue(any("healthkit" in f for f in failures))

    def test_scalar_must_be_listed_in_profile_array(self):
        failures = sl.validate_entitlements_subset(
            {"com.apple.developer.icloud-container-environment": "Production"},
            {"com.apple.developer.icloud-container-environment": ["Development"]})
        self.assertEqual(len(failures), 1)

    def test_type_mismatch_fails(self):
        failures = sl.validate_entitlements_subset(
            {"aps-environment": ["development"]},
            {"aps-environment": "development"})
        self.assertEqual(len(failures), 1)


class SigningIdentityFailureTests(unittest.TestCase):
    def test_complete_entitlements_pass(self):
        app = {
            "application-identifier": "%s.%s" % (TEAM, BUNDLE),
            "com.apple.developer.team-identifier": TEAM,
            "get-task-allow": True,
            "aps-environment": "development",
        }
        self.assertEqual(sl.signing_identity_failures(app, TEAM, BUNDLE), [])

    def test_build_32_defect_is_rejected(self):
        # Exact capability-only entitlement set shipped by 0.3.25 (32).
        build_32 = {
            "aps-environment": "development",
            "com.apple.developer.icloud-container-environment": "Production",
            "com.apple.developer.icloud-container-identifiers":
                ["iCloud.com.jiangnangenius.callrelay"],
            "com.apple.developer.icloud-services": ["CloudKit"],
            "com.apple.developer.ubiquity-container-identifiers":
                ["iCloud.com.jiangnangenius.callrelay"],
        }
        failures = sl.signing_identity_failures(build_32, TEAM, BUNDLE)
        self.assertIn("application-identifier is missing", failures)
        self.assertIn("com.apple.developer.team-identifier is missing", failures)
        self.assertIn("get-task-allow is not true", failures)

    def test_wildcard_application_identifier_is_not_exact(self):
        app = {
            "application-identifier": "%s.*" % TEAM,
            "com.apple.developer.team-identifier": TEAM,
            "get-task-allow": True,
            "aps-environment": "development",
        }
        failures = sl.signing_identity_failures(app, TEAM, BUNDLE)
        self.assertTrue(any("expected" in f for f in failures))


class ProfileEvaluationTests(unittest.TestCase):
    NOW = dt.datetime(2026, 10, 5, 12, 0, 0)

    def test_valid_profile_passes(self):
        ok, reasons = sl.evaluate_profile(profile_dict(), BUNDLE, TEAM, CERT_FP,
                                          now=self.NOW)
        self.assertTrue(ok, reasons)

    def test_profile_mismatch_bundle_rejected(self):
        ok, reasons = sl.evaluate_profile(
            profile_dict(entitlements=profile_entitlements(
                application_identifier="%s.com.example.other" % TEAM)),
            BUNDLE, TEAM, CERT_FP, now=self.NOW)
        self.assertFalse(ok)
        self.assertTrue(any("does not cover" in r for r in reasons), reasons)

    def test_certificate_mismatch_rejected(self):
        ok, reasons = sl.evaluate_profile(profile_dict(certs=(b"other",)), BUNDLE,
                                          TEAM, CERT_FP, now=self.NOW)
        self.assertFalse(ok)
        self.assertTrue(any("does not authorise" in r for r in reasons), reasons)

    def test_expired_profile_rejected(self):
        ok, reasons = sl.evaluate_profile(
            profile_dict(expiration=dt.datetime(2025, 1, 1)),
            BUNDLE, TEAM, CERT_FP, now=self.NOW)
        self.assertFalse(ok)
        self.assertTrue(any("expired" in r for r in reasons), reasons)

    def test_distribution_profile_rejected(self):
        ents = profile_entitlements()
        ents["get-task-allow"] = False
        ok, reasons = sl.evaluate_profile(profile_dict(entitlements=ents), BUNDLE,
                                          TEAM, CERT_FP, now=self.NOW)
        self.assertFalse(ok)
        self.assertTrue(any("development profile" in r for r in reasons), reasons)

    def test_wildcard_profile_covers_bundle(self):
        ok, reasons = sl.evaluate_profile(
            profile_dict(entitlements=profile_entitlements(
                application_identifier="%s.*" % TEAM)),
            BUNDLE, TEAM, CERT_FP, now=self.NOW)
        self.assertTrue(ok, reasons)


class SelectProfileTests(unittest.TestCase):
    def test_best_certificate_matching_profile_is_selected(self):
        with tempfile.TemporaryDirectory() as tmp:
            good = profile_dict(name="Good", uuid="aaaa1111-0000-0000-0000-000000000001",
                                certs=(CERT_DER,),
                                creation=dt.datetime(2026, 10, 3, 9, 0, 0))
            wrong_bundle = profile_dict(
                name="WrongBundle", uuid="bbbb2222-0000-0000-0000-000000000002",
                entitlements=profile_entitlements(
                    application_identifier="%s.com.example.other" % TEAM),
                creation=dt.datetime(2026, 10, 3, 20, 0, 0))
            wrong_cert = profile_dict(name="WrongCert",
                                      uuid="cccc3333-0000-0000-0000-000000000003",
                                      certs=(b"another-certificate",),
                                      creation=dt.datetime(2026, 10, 3, 21, 0, 0))
            good_path = write_profile(tmp, "good.mobileprovision")
            write_profile(tmp, "wrong-bundle.mobileprovision")
            write_profile(tmp, "wrong-cert.mobileprovision")
            profiles = {good_path: good,
                        os.path.join(tmp, "wrong-bundle.mobileprovision"): wrong_bundle,
                        os.path.join(tmp, "wrong-cert.mobileprovision"): wrong_cert}
            with mock.patch.object(sl, "decode_profile",
                                   side_effect=lambda p: profiles[p]), \
                 mock.patch.object(sl, "identity_certificate_der",
                                   return_value=(CERT_FP, CERT_DER)):
                path, profile, report = sl.select_profile(
                    BUNDLE, "Test Identity", profile_dirs=[tmp])
                self.assertEqual(path, good_path)
                self.assertEqual(profile["Name"], "Good")
                statuses = {c["summary"]["name"]: c["ok"] for c in report["candidates"]}
                self.assertTrue(statuses["Good"])
                self.assertFalse(statuses["WrongBundle"])
                self.assertFalse(statuses["WrongCert"])

    def test_explicit_profile_with_wrong_cert_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = write_profile(tmp, "wrong-cert.mobileprovision")
            profile = profile_dict(certs=(b"another-certificate",))
            with mock.patch.object(sl, "decode_profile", return_value=profile), \
                 mock.patch.object(sl, "identity_certificate_der",
                                   return_value=(CERT_FP, CERT_DER)):
                with self.assertRaises(sl.SigningError):
                    sl.select_profile(BUNDLE, "Test Identity", explicit=path)

    def test_explicit_profile_wrong_bundle_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = write_profile(tmp, "wrong-bundle.mobileprovision")
            profile = profile_dict(entitlements=profile_entitlements(
                application_identifier="%s.com.example.other" % TEAM))
            with mock.patch.object(sl, "decode_profile", return_value=profile), \
                 mock.patch.object(sl, "identity_certificate_der",
                                   return_value=(CERT_FP, CERT_DER)):
                with self.assertRaises(sl.SigningError):
                    sl.select_profile(BUNDLE, "Test Identity", explicit=path)


class IdentityConfigurationTests(unittest.TestCase):
    """The tooling hardcodes no personal identity and never guesses one:
    --identity wins over CALLRELAY_SIGNING_IDENTITY, and a missing identity
    is a friendly usage error raised before any keychain access."""

    def setUp(self):
        patcher = mock.patch.dict(os.environ)
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop("CALLRELAY_SIGNING_IDENTITY", None)

    def test_no_personal_identity_hardcoded_in_scripts(self):
        # Regression guard for de-identification: no Apple identity literal
        # other than the generic "Your Name" example may reappear, and the
        # test team token must stay the documented fake.
        self.assertFalse(hasattr(sl, "DEFAULT_IDENTITY"))
        identity_literal = re.compile(r"Apple Development: (?!Your Name)")
        for name in ("signing_lib.py", "sign-native-ipa.sh", "verify-signed-ipa.py"):
            with open(os.path.join(SCRIPTS, name), encoding="utf-8") as fh:
                self.assertIsNone(identity_literal.search(fh.read()),
                                  "%s hardcodes a personal signing identity" % name)
        self.assertEqual(TEAM, "TEAMID1234")

    def test_resolve_identity_explicit_wins_over_environment(self):
        os.environ["CALLRELAY_SIGNING_IDENTITY"] = "Env Identity"
        self.assertEqual(sl.resolve_identity("Explicit Identity"),
                         "Explicit Identity")

    def test_resolve_identity_falls_back_to_environment(self):
        os.environ["CALLRELAY_SIGNING_IDENTITY"] = "  Env Identity  "
        self.assertEqual(sl.resolve_identity(None), "Env Identity")

    def test_resolve_identity_without_configuration_is_usage_error(self):
        with self.assertRaises(sl.UsageError) as ctx:
            sl.resolve_identity(None)
        message = str(ctx.exception)
        self.assertIn("CALLRELAY_SIGNING_IDENTITY", message)
        self.assertNotIn("None", message)

    def test_select_profile_rejects_missing_identity_before_keychain(self):
        with mock.patch.object(sl, "identity_certificate_der",
                               side_effect=AssertionError("keychain touched")
                               ) as keychain:
            with self.assertRaises(sl.UsageError):
                sl.select_profile(BUNDLE, None)
            keychain.assert_not_called()

    def test_select_profile_explicit_identity_still_checks_certificate(self):
        # A profile that does not authorise the identity certificate must not
        # be selected even with a fresh empty default-free configuration.
        with tempfile.TemporaryDirectory() as tmp:
            path = write_profile(tmp, "wrong-cert.mobileprovision")
            profile = profile_dict(certs=(b"another-certificate",))
            with mock.patch.object(sl, "decode_profile", return_value=profile), \
                 mock.patch.object(sl, "identity_certificate_der",
                                   return_value=(CERT_FP, CERT_DER)):
                with self.assertRaises(sl.SigningError):
                    sl.select_profile(BUNDLE, "Test Identity", explicit=path)

    def test_cli_select_profile_without_identity_exits_usage_error(self):
        stderr = io.StringIO()
        with mock.patch.object(sys, "stderr", stderr), \
             mock.patch.object(sl, "identity_certificate_der",
                               side_effect=AssertionError("keychain touched")):
            code = sl.main(["select-profile", "--bundle-id", BUNDLE])
        self.assertEqual(code, 2)
        self.assertIn("CALLRELAY_SIGNING_IDENTITY", stderr.getvalue())
        self.assertNotIn("None", stderr.getvalue())

    def test_cli_select_profile_uses_environment_identity(self):
        os.environ["CALLRELAY_SIGNING_IDENTITY"] = "Env Identity"
        with tempfile.TemporaryDirectory() as tmp, \
             mock.patch.object(sl, "identity_certificate_der",
                               return_value=(CERT_FP, CERT_DER)) as der, \
             mock.patch.object(sys, "stderr", io.StringIO()):
            code = sl.main(["select-profile", "--bundle-id", BUNDLE,
                            "--profile-dir", tmp])
        der.assert_called_once_with("Env Identity")
        self.assertEqual(code, 3)  # empty profile dir: no usable profile found

    def test_cli_select_profile_explicit_identity_overrides_environment(self):
        os.environ["CALLRELAY_SIGNING_IDENTITY"] = "Env Identity"
        with tempfile.TemporaryDirectory() as tmp, \
             mock.patch.object(sl, "identity_certificate_der",
                               return_value=(CERT_FP, CERT_DER)) as der, \
             mock.patch.object(sys, "stderr", io.StringIO()):
            code = sl.main(["select-profile", "--bundle-id", BUNDLE,
                            "--identity", "Explicit Identity",
                            "--profile-dir", tmp])
        der.assert_called_once_with("Explicit Identity")
        self.assertEqual(code, 3)


class AuditIdentityOptionalityTests(unittest.TestCase):
    """Audit wrappers keep identity optional (artifact-derived audit) and
    honour an explicit --identity expectation."""

    def _load_verify_module(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location(
            "verify_signed_ipa", os.path.join(SCRIPTS, "verify-signed-ipa.py"))
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return module

    def test_identity_defaults_to_none_for_derived_audit(self):
        verify = self._load_verify_module()
        with mock.patch.object(sl, "audit_artifact",
                               return_value=({"artifact_type": "app"}, [])) as audit, \
             mock.patch.object(sys, "stderr", io.StringIO()):
            code = verify.main(["Any.app"])
        self.assertEqual(code, 0)
        self.assertIsNone(audit.call_args.kwargs["identity"])

    def test_explicit_identity_expectation_is_passed_through(self):
        verify = self._load_verify_module()
        with mock.patch.object(sl, "audit_artifact",
                               return_value=({"artifact_type": "app"}, [])) as audit, \
             mock.patch.object(sys, "stderr", io.StringIO()):
            verify.main(["Any.app", "--identity", "Some Identity"])
        self.assertEqual(audit.call_args.kwargs["identity"], "Some Identity")


class SignScriptCliTests(unittest.TestCase):
    """sign-native-ipa.sh stops with a usage error before keychain/staging
    when no identity is configured, and accepts explicit/env identities."""

    def setUp(self):
        self.script = os.path.join(SCRIPTS, "sign-native-ipa.sh")
        base = dict(os.environ)
        base.pop("CALLRELAY_SIGNING_IDENTITY", None)
        self.env = base

    def test_missing_identity_is_usage_error(self):
        with tempfile.TemporaryDirectory() as tmp:
            proc = subprocess.run([self.script, "missing.app",
                                   os.path.join(tmp, "out.ipa")],
                                  capture_output=True, text=True, env=self.env,
                                  cwd=tmp)
            self.assertEqual(proc.returncode, 2)
            self.assertIn("no signing identity configured", proc.stderr)
            self.assertIn("CALLRELAY_SIGNING_IDENTITY", proc.stderr)
            self.assertNotIn("None", proc.stderr)
            self.assertEqual(os.listdir(tmp), [])  # no staging/evidence created

    def test_environment_identity_reaches_input_validation(self):
        env = dict(self.env)
        env["CALLRELAY_SIGNING_IDENTITY"] = "Test Identity (TEAMID1234)"
        proc = subprocess.run([self.script, "missing.app", "out.ipa"],
                              capture_output=True, text=True, env=env)
        self.assertEqual(proc.returncode, 2)
        self.assertIn("no Info.plist", proc.stderr)
        self.assertNotIn("no signing identity configured", proc.stderr)

    def test_positional_identity_reaches_input_validation(self):
        proc = subprocess.run([self.script, "missing.app", "out.ipa",
                               "Test Identity (TEAMID1234)"],
                              capture_output=True, text=True, env=self.env)
        self.assertEqual(proc.returncode, 2)
        self.assertIn("no Info.plist", proc.stderr)
        self.assertNotIn("no signing identity configured", proc.stderr)


BAD_IPA_DEFAULT = os.environ.get(
    "CALLRELAY_BAD_IPA",
    os.path.join(SCRIPTS, "..", "build", "rollback",
                 "CallRelay-Feather-Native-0.3.25-32-signed.ipa"),
)


class RealBadArtifactIntegrationTests(unittest.TestCase):
    """Optional integration: audit of the frozen 0.3.25 (32) IPA must fail."""

    @unittest.skipUnless(os.path.isfile(BAD_IPA_DEFAULT),
                         "frozen 32 IPA not present (set CALLRELAY_BAD_IPA)")
    def test_frozen_32_ipa_is_rejected(self):
        report, failures = sl.audit_artifact(BAD_IPA_DEFAULT, identity=None,
                                             allow_version_mismatch=True)
        self.assertIn("signing_identity_entitlements", failures)
        self.assertFalse(report["device_install_proof"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
