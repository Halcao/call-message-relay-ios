#!/usr/bin/env python3
"""Tests for the stable line registry (no hardware required)."""
import importlib.util
import sys
import unittest
from pathlib import Path

MODULE_PATH = Path(__file__).with_name("line_registry.py")
spec = importlib.util.spec_from_file_location("line_registry", MODULE_PATH)
registry = importlib.util.module_from_spec(spec)
sys.modules["line_registry"] = registry
spec.loader.exec_module(registry)

IDENTITY_PATH = Path(__file__).with_name("at_identity.py")
identity_spec = importlib.util.spec_from_file_location("at_identity", IDENTITY_PATH)
at_identity = importlib.util.module_from_spec(identity_spec)
sys.modules["at_identity"] = at_identity
identity_spec.loader.exec_module(at_identity)


class PortAllocationTests(unittest.TestCase):
    def test_twenty_synthetic_lines_get_unique_ports_in_range(self):
        state = registry.empty_registry()
        for index in range(20):
            line_id, entry = registry.upsert_module(state, f"sha256:{index:016x}", f"1-1.{index + 1}")
            self.assertEqual(line_id, f"line{index + 1}")
            self.assertIn(registry.WORKER_PORT_RANGE[0], range(registry.WORKER_PORT_RANGE[0], entry["workerPort"] + 1))
        self.assertGreaterEqual(entry["workerPort"], registry.WORKER_PORT_RANGE[0])
        self.assertLessEqual(entry["workerPort"], registry.WORKER_PORT_RANGE[1])
        self.assertGreaterEqual(entry["internalPort"], registry.INTERNAL_PORT_RANGE[0])
        self.assertLessEqual(entry["internalPort"], registry.INTERNAL_PORT_RANGE[1])
        self.assertEqual(len(registry.used_ports(state, "workerPort")), 20)
        self.assertEqual(len(registry.used_ports(state, "internalPort")), 20)
        self.assertEqual(registry.validate_registry(state), [])

    def test_fifty_lines_still_unique(self):
        state = registry.empty_registry()
        for index in range(50):
            registry.upsert_module(state, f"sha256:{index:016x}", f"2-2.{index + 1}")
        self.assertEqual(len(registry.used_ports(state, "workerPort")), 50)
        self.assertEqual(registry.validate_registry(state), [])


class ReplugTests(unittest.TestCase):
    def test_replug_keeps_line_id_history_and_ports(self):
        state = registry.empty_registry()
        line_id, first = registry.upsert_module(state, "sha256:" + "a" * 16, "1-1.1")
        worker_port, internal_port, data_dir = first["workerPort"], first["internalPort"], first["dataDir"]
        # Same module shows up on a completely different hub path.
        again_id, moved = registry.upsert_module(state, "sha256:" + "a" * 16, "9-9.7")
        self.assertEqual(again_id, line_id)
        self.assertEqual(moved["usbPath"], "9-9.7")
        self.assertEqual(moved["workerPort"], worker_port)
        self.assertEqual(moved["internalPort"], internal_port)
        self.assertEqual(moved["dataDir"], data_dir)
        self.assertEqual(len(state["lines"]), 1)

    def test_shuffled_paths_map_to_stable_lines(self):
        state = registry.empty_registry()
        modules = [f"sha256:{index:016x}" for index in range(4)]
        for index, module in enumerate(modules):
            registry.upsert_module(state, module, f"1-1.{index + 1}")
        shuffled = list(reversed([f"1-1.{index + 1}" for index in range(4)]))
        mapping = {}
        for module, path in zip(reversed(modules), shuffled):
            line_id, _ = registry.upsert_module(state, module, path)
            mapping[module] = line_id
        self.assertEqual(mapping[modules[0]], "line1")
        self.assertEqual(mapping[modules[3]], "line4")
        self.assertEqual(registry.validate_registry(state), [])

    def test_unknown_module_registers_pending_without_preparation(self):
        state = registry.empty_registry()
        registry.register_pending(state, "sha256:" + "b" * 16, "3-4.5", "identity not in private backups")
        self.assertEqual(state["lines"], {})
        self.assertIn("sha256:" + "b" * 16, state["pending"])


class SimIdentityTests(unittest.TestCase):
    def test_sim_swap_invalidates_old_association(self):
        state = registry.empty_registry()
        line_id, entry = registry.upsert_module(state, "sha256:" + "c" * 16, "1-1.1")
        entry = registry.apply_sim_identity(entry, "89860000000000000001", "+8613800000001")
        state["lines"][line_id] = entry
        self.assertEqual(entry["state"], "active")
        self.assertEqual(entry["numberSource"], "sim")
        before = entry["phoneMasked"]
        swapped = registry.apply_sim_identity(entry, "89860000000000000002", "+8613900000002")
        self.assertEqual(swapped["state"], "pending_sim_confirmation")
        self.assertNotEqual(swapped["phoneMasked"], before)
        # Explicit confirmation restores the line.
        confirmed = registry.confirm_sim(state, line_id, swapped["phoneMasked"])
        self.assertEqual(confirmed["state"], "active")

    def test_manual_override_wins_over_sim(self):
        state = registry.empty_registry()
        line_id, entry = registry.upsert_module(state, "sha256:" + "d" * 16, "1-1.2")
        entry = registry.apply_sim_identity(entry, "89860000000000000003", "+8613800000003")
        state["lines"][line_id] = entry
        manual = registry.set_number(state, line_id, "01012345678")
        self.assertEqual(manual["numberSource"], "manual")
        after = registry.apply_sim_identity(manual, "89860000000000000003", "+8613800000003")
        self.assertEqual(after["numberSource"], "manual")
        self.assertEqual(after["phoneMasked"], "010****5678")

    def test_absent_sim_does_not_clear_existing_identity(self):
        entry = registry.apply_sim_identity({}, "89860000000000000004", "+8613800000004")
        unchanged = registry.apply_sim_identity(entry, "", "")
        self.assertEqual(unchanged["phoneMasked"], entry["phoneMasked"])
        self.assertEqual(unchanged["numberSource"], "sim")

    def test_sim_swap_clears_stale_auto_number_until_confirmed(self):
        entry = registry.apply_sim_identity({}, "89860000000000000005", "13800138005")
        self.assertEqual(entry["phoneMasked"], "138****8005")
        swapped = registry.apply_sim_identity(entry, "89860000000000000006", "13900139006")
        self.assertEqual(swapped["state"], "pending_sim_confirmation")
        # The new SIM must not silently inherit the previous auto number.
        self.assertEqual(swapped["phoneMasked"], "")
        self.assertEqual(swapped["numberSource"], "none")
        state = registry.empty_registry()
        state["lines"]["line1"] = swapped
        confirmed = registry.confirm_sim(state, "line1", "139****9006")
        self.assertEqual(confirmed["state"], "active")
        self.assertEqual(confirmed["phoneMasked"], "139****9006")
        self.assertEqual(confirmed["numberSource"], "sim")

    def test_manual_number_survives_sim_swap(self):
        entry = registry.apply_sim_identity({}, "89860000000000000007", "+8613800000007")
        entry["phoneMasked"] = "010****5678"
        entry["numberSource"] = "manual"
        swapped = registry.apply_sim_identity(entry, "89860000000000000008", "+8613900000008")
        self.assertEqual(swapped["state"], "pending_sim_confirmation")
        self.assertEqual(swapped["phoneMasked"], "010****5678")
        self.assertEqual(swapped["numberSource"], "manual")
        state = registry.empty_registry()
        state["lines"]["line1"] = swapped
        confirmed = registry.confirm_sim(state, "line1")
        self.assertEqual(confirmed["numberSource"], "manual")
        self.assertEqual(confirmed["phoneMasked"], "010****5678")

    def test_empty_sim_records_number_source_none(self):
        entry = registry.apply_sim_identity({}, "89860000000000000009", "")
        self.assertEqual(entry["numberSource"], "none")
        self.assertEqual(entry["phoneMasked"], "")


class CNUMParseTests(unittest.TestCase):
    def test_observed_empty_alpha_shape(self):
        transcript = 'AT+CNUM\r\r\n+CNUM: ,"+8613800138000",145\r\r\nOK\r\n'
        result = at_identity.parse_cnum(transcript)
        self.assertTrue(result.ok)
        self.assertFalse(result.error)
        self.assertTrue(result.found)
        self.assertEqual(result.number, "+8613800138000")
        self.assertEqual(result.entries[0].type_of_address, 145)
        self.assertTrue(result.entries[0].international)

    def test_national_number_with_international_toa_kept_verbatim(self):
        # Firmware stores TOA 145 alongside an 11-digit national number with
        # no leading '+'. Do not invent a country code or blindly prefix '+'.
        transcript = '+CNUM: "My Line","13800138000",145\n\nOK\n'
        result = at_identity.parse_cnum(transcript)
        self.assertTrue(result.found)
        self.assertEqual(result.number, "13800138000")
        self.assertFalse(result.entries[0].international)

    def test_quoted_alpha_and_whitespace(self):
        transcript = '  +CNUM: "Home" , "+8613800138001" , 145  \n\nOK\n'
        result = at_identity.parse_cnum(transcript)
        self.assertEqual(result.number, "+8613800138001")
        self.assertEqual(result.entries[0].alpha, "Home")

    def test_multiple_entries_first_wins(self):
        transcript = ('+CNUM: ,"+8613800138002",145\r\n'
                      '+CNUM: "fax","13900139000",128\r\n\r\nOK\r\n')
        result = at_identity.parse_cnum(transcript)
        self.assertEqual(len(result.entries), 2)
        self.assertEqual(result.number, "+8613800138002")
        self.assertEqual(result.entries[1].number, "13900139000")

    def test_empty_sim_record_is_ok_without_entries(self):
        result = at_identity.parse_cnum("AT+CNUM\r\r\n\r\nOK\r\n")
        self.assertTrue(result.ok)
        self.assertFalse(result.found)
        self.assertFalse(result.unsupported)
        self.assertEqual(result.number, "")

    def test_error_distinguished_from_empty(self):
        result = at_identity.parse_cnum("AT+CNUM\r\r\nERROR\r\n")
        self.assertTrue(result.error)
        self.assertFalse(result.ok)
        self.assertFalse(result.found)
        cme = at_identity.parse_cnum("AT+CNUM\r\r\n+CME ERROR: 4\r\n")
        self.assertTrue(cme.error)
        self.assertTrue(cme.unsupported)

    def test_error_with_partial_entry_exposes_nothing(self):
        result = at_identity.parse_cnum('+CNUM: ,"+8613800138009",145\nERROR\n')
        self.assertTrue(result.error)
        self.assertFalse(result.ok)
        self.assertFalse(result.found)
        self.assertEqual(result.number, "")
        self.assertEqual(result.entries, ())

    def test_echo_only_is_incomplete_not_success(self):
        result = at_identity.parse_cnum("AT+CNUM\r\n")
        self.assertFalse(result.ok)
        self.assertFalse(result.error)
        self.assertTrue(result.incomplete)
        self.assertFalse(result.found)

    def test_truncated_after_entry_is_incomplete(self):
        result = at_identity.parse_cnum('+CNUM: ,"+8613800138010",145\n')
        self.assertTrue(result.incomplete)
        self.assertFalse(result.ok)
        self.assertEqual(result.number, "")

    def test_ignores_supplementary_service_strings(self):
        for value in ("*#31#", "#31#", "*31*123#"):
            result = at_identity.parse_cnum(f'+CNUM: ,"{value}",145\n\nOK\n')
            self.assertFalse(result.found, value)
        # Fewer than three digits is not a usable own number.
        short = at_identity.parse_cnum('+CNUM: ,"12",145\n\nOK\n')
        self.assertFalse(short.found)


class CCIDParseTests(unittest.TestCase):
    def test_prefixed_hex_with_letter_nibbles(self):
        transcript = "AT+CCID\r\r\n+CCID: 0123456789ABCDEF0123\r\r\nOK\r\n"
        self.assertEqual(at_identity.parse_ccid(transcript), "0123456789ABCDEF0123")

    def test_prefixed_digits(self):
        transcript = '+CCID: "8986000012345678901"\r\nOK\r\n'
        self.assertEqual(at_identity.parse_ccid(transcript), "8986000012345678901")

    def test_bare_token_variant(self):
        transcript = "AT+CCID\r\r\n8986000012345678901\r\r\nOK\r\n"
        self.assertEqual(at_identity.parse_ccid(transcript), "8986000012345678901")

    def test_noise_text_not_stripped_into_identity(self):
        transcript = "AT+CCID\r\r\n+CCID: not-an-iccid\r\r\nOK\r\n"
        self.assertEqual(at_identity.parse_ccid(transcript), "")
        transcript2 = "AT+CCID\r\r\nSIM READY\r\r\nOK\r\n"
        self.assertEqual(at_identity.parse_ccid(transcript2), "")

    def test_short_hex_rejected(self):
        self.assertEqual(at_identity.parse_ccid("+CCID: ABCDEF\nOK\n"), "")

    def test_absent(self):
        self.assertEqual(at_identity.parse_ccid("AT+CCID\r\r\nERROR\r\n"), "")

    def test_sim_hash_keeps_hex_nibbles(self):
        self.assertNotEqual(registry.sim_hash("898600100924F0043660"),
                            registry.sim_hash("89860010092400043660"))
        self.assertEqual(registry.sim_hash("abcd"), registry.sim_hash("ABCD"))


class RenderTests(unittest.TestCase):
    def test_lines_yaml_renders_active_lines_only_with_unique_ports(self):
        state = registry.empty_registry()
        registry.seed_line(state, "line1", "sha256:" + "e" * 16, "1-1.1", 9781, 9791, firmware="fw1")
        registry.seed_line(state, "line2", "sha256:" + "f" * 16, "1-1.2", 9782, 9792, firmware="fw2")
        disabled, _ = registry.upsert_module(state, "sha256:" + "1" * 16, "1-1.3")
        state["lines"][disabled]["state"] = "pending_sim_confirmation"
        rendered = registry.render_lines_yaml(state)
        self.assertIn("id: line1", rendered)
        self.assertIn("worker_url: http://127.0.0.1:9781", rendered)
        self.assertIn("id: line2", rendered)
        self.assertNotIn(f"id: {disabled}", rendered)
        self.assertEqual(registry.validate_registry(state), [])


if __name__ == "__main__":
    unittest.main()
