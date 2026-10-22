#!/usr/bin/env python3
"""Targeted tests for the feather-source bridge scanner (has_bridge_content).

Locks two contracts:
1. The verified Swift-mangling false positive (`barK`, e.g. `ToolbarK0`)
   must NOT reject a pure-native package.
2. Real bridge markers in ANY other case must still reject it — including
   lowercase `bark` endpoints and all-caps `BARK`.
"""
import re
import unittest
from pathlib import Path

_SOURCE = (Path(__file__).resolve().parent.parent / "update-feather-source.py").read_text()
# The script is flat (no main guard), so execute only the self-contained
# scanner block — everything from the forbidden pattern up to the argument
# parser.
_SLICE = _SOURCE[_SOURCE.index("NATIVE_FORBIDDEN_BINARY = re.compile("):
                 _SOURCE.index("parser = argparse.ArgumentParser")]
_NS = {"re": re}
exec(compile(_SLICE, "update-feather-source.py<scanner>", "exec"), _NS)
has_bridge_content = _NS["has_bridge_content"]


class BridgeScanTests(unittest.TestCase):
    def test_toolbar_mangling_artifact_is_not_a_bridge(self):
        # Exact bytes from the real native binary scan (0.3.35 build 42).
        self.assertFalse(has_bridge_content(b"AA07ToolbarK0Rd__lFQOy"))
        self.assertFalse(has_bridge_content(b"AA012TupleToolbarK0VyAA11ToolbarItemV"))
        self.assertFalse(has_bridge_content(b"0D19BarTitleDisplayModeyQrAA17NavigationBarIte"))

    def test_lowercase_bark_endpoint_still_rejected(self):
        self.assertTrue(has_bridge_content(b"https://bark.day.app/abcdef/Title"))
        self.assertTrue(has_bridge_content(b"bark://push"))
        self.assertTrue(has_bridge_content(b"api.day.app bark push"))

    def test_capitals_still_rejected(self):
        self.assertTrue(has_bridge_content(b"BarkBridge"))
        self.assertTrue(has_bridge_content(b"Bark push token"))
        self.assertTrue(has_bridge_content(b"BARK"))

    def test_other_bridge_markers_still_rejected(self):
        for marker in (b"WebPushBridge", b"webpush", b"Web Push",
                       b"IncomingCallChecker", b"CheckIncomingCallIntent",
                       b"callrelay://incoming"):
            self.assertTrue(has_bridge_content(marker), marker)

    def test_mangled_artifact_plus_real_marker_rejected(self):
        blob = b"AA07ToolbarK0Rd__lFQOy" + b" " + b"https://bark.day.app/x"
        self.assertTrue(has_bridge_content(blob))

    def test_clean_native_blob_accepted(self):
        blob = ("9CallRelay0A12NumberIntentV 9CallRelay0B10LineEntityV "
                "用 CallRelay 拨打").encode("utf-8")
        self.assertFalse(has_bridge_content(blob))


if __name__ == "__main__":
    unittest.main()
