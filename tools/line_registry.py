#!/usr/bin/env python3
"""Stable line registry for CallRelay.

Physical modules are identified by their module identity hash (derived from
the private verified backup), never by USB hub slot. The registry maps
module identity -> stable line id + allocated worker/internal ports + data
directory, so replugging a module into a different USB path keeps its line
id and history. Unknown modules are recorded as pending approval and are
never unlocked or flashed automatically.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
import time
from pathlib import Path

REGISTRY_VERSION = 1
DEFAULT_REGISTRY = Path("/etc/callrelay/lines.json")
WORKER_PORT_RANGE = (9700, 9799)
INTERNAL_PORT_RANGE = (9800, 9899)
DEFAULT_DATA_ROOT = "/var/lib/cellbridge"
DEFAULT_TOKEN_FILE = "/etc/callrelay/internal.token"
DEFAULT_IDENTITY_DIR = "/run/cmr"
LINE_ID_PATTERN = re.compile(r"^line([0-9]+)$")
MODULE_KEY_PATTERN = re.compile(r"^sha256:[0-9a-f]{16}$")


def empty_registry() -> dict:
    return {"version": REGISTRY_VERSION, "lines": {}, "pending": {}}


def load_registry(path: Path) -> dict:
    if not path.exists():
        return empty_registry()
    registry = json.loads(path.read_text())
    registry.setdefault("version", REGISTRY_VERSION)
    registry.setdefault("lines", {})
    registry.setdefault("pending", {})
    return registry


def save_registry(path: Path, registry: dict) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    temp = path.with_suffix(path.suffix + ".tmp")
    temp.write_text(json.dumps(registry, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
    temp.chmod(0o600)
    temp.replace(path)


def used_ports(registry: dict, field: str, exclude_line: str | None = None) -> set:
    return {
        int(entry[field])
        for line_id, entry in registry["lines"].items()
        if field in entry and line_id != exclude_line
    }


def allocate_port(registry: dict, field: str, port_range=WORKER_PORT_RANGE, preferred: int | None = None,
                  exclude_line: str | None = None) -> int:
    used = used_ports(registry, field, exclude_line)
    if preferred is not None:
        if not (1024 <= preferred <= 65535):
            raise ValueError(f"preferred port {preferred} outside valid range")
        if preferred not in used:
            return preferred
    start, end = port_range
    for port in range(start, end + 1):
        if port not in used:
            return port
    raise RuntimeError(f"no free port in {port_range}")


def next_line_id(registry: dict) -> str:
    highest = 0
    for line_id in registry["lines"]:
        match = LINE_ID_PATTERN.match(line_id)
        if match:
            highest = max(highest, int(match.group(1)))
    return f"line{highest + 1}"


def data_dir_for(line_id: str, data_root: str = DEFAULT_DATA_ROOT) -> str:
    return f"{data_root}/{line_id}"


def identity_file_for(line_id: str, identity_dir: str = DEFAULT_IDENTITY_DIR) -> str:
    return f"{identity_dir}/{line_id}.identity.json"


def upsert_module(
    registry: dict,
    module_key: str,
    usb_path: str,
    firmware: str = "",
    preferred_line: str | None = None,
    worker_port_range=WORKER_PORT_RANGE,
    internal_port_range=INTERNAL_PORT_RANGE,
    data_root: str = DEFAULT_DATA_ROOT,
) -> tuple[str, dict]:
    if not MODULE_KEY_PATTERN.match(module_key):
        raise ValueError(f"invalid module key {module_key!r}")
    line_id = None
    for candidate_id, entry in registry["lines"].items():
        if entry.get("moduleKey") == module_key:
            line_id = candidate_id
            break
    if line_id is None and preferred_line is not None:
        line_id = preferred_line
    if line_id is None:
        line_id = next_line_id(registry)
    entry = registry["lines"].get(line_id, {})
    entry.update(
        {
            "moduleKey": module_key,
            "usbPath": usb_path,
            "firmware": firmware or entry.get("firmware", ""),
            "updatedAt": int(time.time() * 1000),
        }
    )
    entry.setdefault("state", "active")
    entry.setdefault("numberSource", "none")
    entry.setdefault("phoneMasked", "")
    entry.setdefault("simHash", "")
    entry["workerPort"] = allocate_port(registry, "workerPort", worker_port_range, entry.get("workerPort"), exclude_line=line_id)
    entry["internalPort"] = allocate_port(registry, "internalPort", internal_port_range, entry.get("internalPort"), exclude_line=line_id)
    entry.setdefault("dataDir", data_dir_for(line_id, data_root))
    registry["lines"][line_id] = entry
    registry["pending"].pop(module_key, None)
    return line_id, entry


def register_pending(registry: dict, module_key: str, usb_path: str, reason: str = "unknown module") -> None:
    registry["pending"].setdefault(
        module_key,
        {"usbPath": usb_path, "reason": reason, "firstSeenAt": int(time.time() * 1000)},
    )


def seed_line(registry: dict, line_id: str, module_key: str, usb_path: str, worker_port: int, internal_port: int,
              firmware: str = "", data_root: str = DEFAULT_DATA_ROOT) -> dict:
    entry = registry["lines"].get(line_id, {})
    entry.update(
        {
            "moduleKey": module_key,
            "usbPath": usb_path,
            "workerPort": worker_port,
            "internalPort": internal_port,
            "dataDir": entry.get("dataDir", data_dir_for(line_id, data_root)),
            "firmware": firmware or entry.get("firmware", ""),
            "state": entry.get("state", "active"),
            "numberSource": entry.get("numberSource", "none"),
            "phoneMasked": entry.get("phoneMasked", ""),
            "simHash": entry.get("simHash", ""),
            "updatedAt": int(time.time() * 1000),
        }
    )
    registry["lines"][line_id] = entry
    return entry


def sim_hash(iccid: str) -> str:
    # ICCIDs are printed as 18-20 hex chars and routinely contain A-F nibbles
    # (e.g. ...924F004...); normalizing to upper-case hex (rather than
    # deleting letters) keeps distinct cards from collapsing.
    token = re.sub(r"[^0-9A-Fa-f]", "", iccid or "").upper()
    if not token:
        return ""
    return "sha256:" + hashlib.sha256(token.encode()).hexdigest()[:16]


def mask_phone(value: str) -> str:
    digits = re.sub(r"\D", "", value or "")
    if len(digits) >= 7:
        return digits[:3] + "*" * (len(digits) - 7) + digits[-4:]
    return ""


NUMBER_SOURCE_NONE = "none"
NUMBER_SOURCE_SIM = "sim"
NUMBER_SOURCE_MANUAL = "manual"
VALID_NUMBER_SOURCES = {NUMBER_SOURCE_NONE, NUMBER_SOURCE_SIM, NUMBER_SOURCE_MANUAL}


def apply_sim_identity(entry: dict, iccid: str, phone: str) -> dict:
    """Update SIM identity; a changed ICCID invalidates the old association
    and requires explicit confirmation before the line runs again.

    Number precedence:

    * a manual operator override always wins and survives SIM changes (it is
      an explicit choice, not SIM-derived state);
    * a SIM-derived auto number is bound to the ICCID: swapping the SIM
      invalidates the stale auto number immediately instead of letting the new
      card masquerade under the previous number. The new SIM's own number is
      only adopted after explicit confirmation;
    * no ICCID (unreadable) never clears an existing association.
    """
    entry = dict(entry)
    incoming = sim_hash(iccid)
    if incoming:
        previous = entry.get("simHash", "")
        manual = entry.get("numberSource") == NUMBER_SOURCE_MANUAL
        if previous and previous != incoming:
            entry["state"] = "pending_sim_confirmation"
            entry["simHash"] = incoming
            if manual:
                # Keep the explicit operator number; it is not SIM-derived.
                entry["numberSource"] = NUMBER_SOURCE_MANUAL
            else:
                entry["phoneMasked"] = ""
                entry["numberSource"] = NUMBER_SOURCE_NONE
            entry["updatedAt"] = int(time.time() * 1000)
            return entry
        entry["simHash"] = incoming
        if not manual:
            entry["numberSource"] = NUMBER_SOURCE_SIM if phone else NUMBER_SOURCE_NONE
            entry["phoneMasked"] = mask_phone(phone) or entry.get("phoneMasked", "")
    entry["updatedAt"] = int(time.time() * 1000)
    return entry


def confirm_sim(registry: dict, line_id: str, phone_masked: str = "", number_source: str = "") -> dict:
    entry = registry["lines"].get(line_id)
    if entry is None:
        raise KeyError(line_id)
    entry["state"] = "active"
    if phone_masked:
        entry["phoneMasked"] = phone_masked
        entry["numberSource"] = number_source if number_source in VALID_NUMBER_SOURCES else NUMBER_SOURCE_SIM
    elif entry.get("numberSource") == NUMBER_SOURCE_MANUAL:
        entry["numberSource"] = NUMBER_SOURCE_MANUAL
    else:
        # The replacement SIM has no confirmed number of its own yet.
        entry["numberSource"] = NUMBER_SOURCE_NONE
    entry["updatedAt"] = int(time.time() * 1000)
    return entry


def set_number(registry: dict, line_id: str, phone: str) -> dict:
    entry = registry["lines"].get(line_id)
    if entry is None:
        raise KeyError(line_id)
    entry["phoneMasked"] = mask_phone(phone)
    entry["numberSource"] = NUMBER_SOURCE_MANUAL
    entry["updatedAt"] = int(time.time() * 1000)
    return entry


def clear_manual_number(registry: dict, line_id: str) -> dict:
    """Remove a manual override so the SIM-derived number (if any) applies."""
    entry = registry["lines"].get(line_id)
    if entry is None:
        raise KeyError(line_id)
    entry["phoneMasked"] = ""
    entry["numberSource"] = NUMBER_SOURCE_NONE
    entry["updatedAt"] = int(time.time() * 1000)
    return entry


def render_lines_yaml(registry: dict, token_file: str = DEFAULT_TOKEN_FILE, identity_dir: str = DEFAULT_IDENTITY_DIR) -> str:
    lines = ["# generated by tools/line_registry.py render-lines; do not edit by hand", "lines:"]
    for line_id in sorted(registry["lines"], key=lambda value: (len(value), value)):
        entry = registry["lines"][line_id]
        if entry.get("state") != "active":
            continue
        name = entry.get("phoneMasked") or entry.get("firmware") or line_id
        lines.extend(
            [
                f"  - id: {line_id}",
                f"    name: \"{line_id.upper()} {name}\"".replace("\\", ""),
                f"    worker_url: http://127.0.0.1:{entry['workerPort']}",
                f"    internal_url: http://127.0.0.1:{entry['internalPort']}",
                f"    internal_token_file: {token_file}",
                f"    identity_file: {identity_file_for(line_id, identity_dir)}",
                "    enabled: true",
            ]
        )
    return "\n".join(lines) + "\n"


def validate_registry(registry: dict) -> list:
    problems = []
    worker_ports = {}
    internal_ports = {}
    for line_id, entry in registry["lines"].items():
        if not LINE_ID_PATTERN.match(line_id):
            problems.append(f"invalid line id {line_id}")
        module_key = entry.get("moduleKey", "")
        if not MODULE_KEY_PATTERN.match(module_key):
            problems.append(f"{line_id}: invalid module key {module_key!r}")
        for field, ports in (("workerPort", worker_ports), ("internalPort", internal_ports)):
            port = entry.get(field)
            if not isinstance(port, int) or not (1024 <= port <= 65535):
                problems.append(f"{line_id}: invalid {field} {port!r}")
                continue
            if port in ports:
                problems.append(f"{line_id}: {field} {port} duplicates {ports[port]}")
            ports[port] = line_id
    return problems


def command_status(args) -> int:
    registry = load_registry(Path(args.registry))
    problems = validate_registry(registry)
    for line_id in sorted(registry["lines"]):
        entry = registry["lines"][line_id]
        print(f"{line_id}\t{entry.get('state')}\t{entry.get('moduleKey')}\t{entry.get('usbPath')}\t"
              f"worker={entry.get('workerPort')}\tinternal={entry.get('internalPort')}\t{entry.get('phoneMasked','')}")
    for module_key, pending in registry["pending"].items():
        print(f"PENDING\t{module_key}\t{pending.get('usbPath')}\t{pending.get('reason')}")
    for problem in problems:
        print(f"problem: {problem}", file=sys.stderr)
    return 1 if problems else 0


def command_seed(args) -> int:
    registry = load_registry(Path(args.registry))
    seed = json.loads(Path(args.input).read_text())
    for item in seed["lines"]:
        seed_line(
            registry,
            item["lineId"], item["moduleKey"], item["usbPath"],
            int(item["workerPort"]), int(item["internalPort"]),
            firmware=item.get("firmware", ""),
        )
    problems = validate_registry(registry)
    if problems:
        for problem in problems:
            print(f"problem: {problem}", file=sys.stderr)
        return 1
    save_registry(Path(args.registry), registry)
    print(f"seeded {len(seed['lines'])} lines")
    return 0


def command_confirm_sim(args) -> int:
    registry = load_registry(Path(args.registry))
    entry = confirm_sim(registry, args.line, args.phone_masked or "", args.number_source)
    save_registry(Path(args.registry), registry)
    print(f"{args.line} active; phone={entry.get('phoneMasked', '')} source={entry.get('numberSource')}")
    return 0


def command_set_number(args) -> int:
    registry = load_registry(Path(args.registry))
    entry = set_number(registry, args.line, args.phone)
    save_registry(Path(args.registry), registry)
    print(f"{args.line} manual number {entry['phoneMasked']}")
    return 0


def command_clear_number(args) -> int:
    registry = load_registry(Path(args.registry))
    entry = clear_manual_number(registry, args.line)
    save_registry(Path(args.registry), registry)
    print(f"{args.line} number override cleared; source={entry['numberSource']}")
    return 0


def command_render_lines(args) -> int:
    registry = load_registry(Path(args.registry))
    output = render_lines_yaml(registry, args.token_file, args.identity_dir)
    if args.out:
        target = Path(args.out)
        target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        temp = target.with_suffix(target.suffix + ".tmp")
        temp.write_text(output)
        temp.chmod(0o600)
        temp.replace(target)
        print(f"wrote {target}")
    else:
        print(output, end="")
    return 0


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--registry", default=str(DEFAULT_REGISTRY))
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("status").set_defaults(func=command_status)

    seed = sub.add_parser("seed")
    seed.add_argument("--input", required=True)
    seed.set_defaults(func=command_seed)

    confirm = sub.add_parser("confirm-sim")
    confirm.add_argument("--line", required=True)
    confirm.add_argument("--phone-masked", default="")
    confirm.add_argument("--number-source", default="", choices=sorted(VALID_NUMBER_SOURCES))
    confirm.set_defaults(func=command_confirm_sim)

    number = sub.add_parser("set-number")
    number.add_argument("--line", required=True)
    number.add_argument("--phone", required=True)
    number.set_defaults(func=command_set_number)

    clear_number = sub.add_parser("clear-number")
    clear_number.add_argument("--line", required=True)
    clear_number.set_defaults(func=command_clear_number)

    render = sub.add_parser("render-lines")
    render.add_argument("--out", default="")
    render.add_argument("--token-file", default=DEFAULT_TOKEN_FILE)
    render.add_argument("--identity-dir", default=DEFAULT_IDENTITY_DIR)
    render.set_defaults(func=command_render_lines)

    args = parser.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
