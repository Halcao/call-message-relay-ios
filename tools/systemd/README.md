# tools/systemd — example units

These are EXAMPLE units. Review every path before installing:

- `callrelayd.service.example` — the unified daemon (loopback listener;
  restarts worker units via systemctl, hence root in the example).
- `callrelay-worker@.service.example` — one cellular worker per line; runs
  `tools/prepare-line.example.sh` first (copy it to
  `/usr/local/lib/callrelay/prepare-line.example.sh` and make it executable,
  or point `ExecStartPre` at your own path).
- `callrelay-adb.service.example` — loopback-only adb server for modules
  that use the qdc507 voice runtime.

The units deliberately do NOT reference any one-time EDL flashing, vendor
credential material, or an external preparation repository: module
provisioning is an operator prerequisite performed BEFORE installation (see
docs/self-hosting.md), and unknown modules are recorded as pending approval
by tools/line_registry.py rather than being flashed automatically.

For the complete installation sequence (binaries, template, registry, console
credential and units), follow [the self-hosting guide](../../docs/self-hosting.md#3-register-lines-and-prepare-workers).
