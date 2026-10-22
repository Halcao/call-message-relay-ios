"""Black-box checks for concurrent per-line preparation (no modem or network)."""
import concurrent.futures
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import unittest

TOOLS = Path(__file__).resolve().parent


class PrepareLineTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="callrelay-prepare-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.registry = json.loads((TOOLS / "config/lines.json.example").read_text())
        for line, entry in self.registry["lines"].items():
            tty = self.root / f"{line}.tty"
            tty.touch()
            entry["tty"] = str(tty)
            entry["dataDir"] = str(self.root / f"{line}.data")
        self.registry_path = self.root / "lines.json"
        self.save_registry()
        self.token = self.root / "etc/internal.token"
        self.env = os.environ | {
            "REGISTRY": str(self.registry_path),
            "WORKER_DIR": str(self.root / "run"),
            "TOKEN_FILE": str(self.token),
            "TEMPLATE": str(TOOLS / "config/line-worker.example.yaml"),
        }

    def save_registry(self):
        self.registry_path.write_text(json.dumps(self.registry))

    def prepare(self, line):
        return subprocess.run(["bash", str(TOOLS / "prepare-line.example.sh"), line],
                              env=self.env, capture_output=True, text=True)

    def test_parallel_workers_share_one_complete_token_and_distinct_ttys(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            results = list(pool.map(self.prepare, ["line1", "line2"] * 4))
        self.assertTrue(all(p.returncode == 0 for p in results),
                        [(p.returncode, p.stderr) for p in results])
        token = self.token.read_bytes()
        self.assertRegex(token.decode(), r"^[a-f0-9]{64}\n$")
        self.assertEqual(stat.S_IMODE(self.token.stat().st_mode), 0o600)
        self.assertEqual(list(self.token.parent.glob(".internal-token-*")), [])
        for line in ["line1", "line2"]:
            content = (self.root / f"run/{line}.yaml").read_text()
            self.assertIn(str(self.root / f"{line}.tty"), content)
        self.assertEqual(self.prepare("line1").returncode, 0)
        self.assertEqual(self.token.read_bytes(), token)

    def test_usb_hint_without_tty_does_not_render_or_create_token(self):
        self.registry["lines"]["line2"].pop("tty")
        self.save_registry()
        result = self.prepare("line2")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no explicit AT serial", result.stderr)
        self.assertFalse((self.root / "run/line2.yaml").exists())
        self.assertFalse(self.token.exists())


if __name__ == "__main__":
    unittest.main()
