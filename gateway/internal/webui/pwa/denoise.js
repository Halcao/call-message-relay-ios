/* Build-48/49 directional denoise preferences for the PWA.
 *
 * Two independent switches ("我的声音降噪" = uplink mic, "对方声音降噪" =
 * downlink remote/modem). Build-49 policy: default OFF (authorized
 * human-speech quality acceptance was not established); both switches stay
 * fully usable and an explicit choice always wins.
 *
 * Storage: scoped per ENROLLED DEVICE. The caller passes the gateway device
 * id (non-secret, sanitized into the key directly); any other seed is hashed
 * so a raw token can NEVER enter a storage key. Logout or re-enroll calls
 * clear() so the next enrollment starts at the OFF default and one
 * principal's choice can never leak into another. The legacy origin-global
 * key (callrelay.denoise.v1, the build-48 default-on era) is deliberately
 * never read: unscoped state uses its own anonymous key and starts OFF.
 * Private-mode storage failures fall back to a session-only in-memory value
 * (never silently dropped).
 *
 * Applying a change: bind() persists locally, refreshes from storage on
 * every scope change, and emits a 'callrelay:denoise' event; the settings
 * page PATCHes the per-device preferences (the gateway applies it live to
 * sessions owned by that device, covering the WebRTC and the WSS media
 * paths) and surfaces a plain, jargon-free note when the gateway could not
 * store it. The call page additionally sends the hot WSS control when a
 * media socket is open. An older gateway ignores unknown attach params,
 * unknown control types and unknown preference fields, so every path
 * degrades gracefully.
 *
 * The processing runs on the GATEWAY (SpeexDSP); the PWA only communicates
 * the choice.
 */
(function (root) {
  'use strict';
  var KEY_BASE = 'callrelay.denoise.v1';
  var scope = '';            // '' = unscoped (pre-login) -> '.anon' key
  var memory = null;         // private-mode session-only fallback
  var bound = null;          // single checkbox pair registration

  /* FNV-1a 32-bit for non-device seeds (a raw token can never be used
   * directly). Device ids are non-secret and pass through sanitized. */
  function hashSeed(text) {
    var h = 0x811c9dc5;
    for (var i = 0; i < text.length; i++) {
      h ^= text.charCodeAt(i);
      h = (h * 0x01000193) >>> 0;
    }
    return ('0000000' + h.toString(16)).slice(-8);
  }

  /* Device ids are non-secret: encode directly (sanitized, length-bound).
   * Anything else is hashed so a raw token can never enter the key. */
  function normalizeSeed(seed) {
    var text = String(seed || '');
    if (/^[A-Za-z0-9._:@-]+$/.test(text) && text.length <= 96) { return 'd.' + text; }
    return 'h.' + hashSeed(text);
  }

  function storageKey() {
    return KEY_BASE + '.' + (scope || 'anon');
  }

  function currentScope() { return scope; }

  function setScope(seed) {
    var next = normalizeSeed(seed);
    if (next === scope) {
      // Same enrollment (e.g. a repeated refresh): keep any private-mode
      // in-memory value and just resync the bound checkboxes.
      refreshBound();
      return;
    }
    scope = next;
    memory = null;
    refreshBound();
  }

  /* Logout / re-enroll: remove this enrollment's stored choice so the next
   * principal starts at the OFF default. */
  function clear() {
    try { root.localStorage.removeItem(storageKey()); } catch (e) { /* ignore */ }
    memory = null;
    scope = '';
    refreshBound();
  }

  function read() {
    if (memory) { return { uplink: memory.uplink, downlink: memory.downlink }; }
    try {
      var raw = root.localStorage.getItem(storageKey());
      if (!raw) { return { uplink: false, downlink: false }; }
      var parsed = JSON.parse(raw);
      // Strict opt-in: only an explicit true enables a direction (an empty
      // or corrupt object must not resurrect the old default-on).
      return {
        uplink: parsed.uplink === true,
        downlink: parsed.downlink === true,
      };
    } catch (e) {
      return { uplink: false, downlink: false };
    }
  }

  function write(value) {
    var normalized = {
      uplink: value.uplink === true,
      downlink: value.downlink === true,
    };
    memory = normalized;
    try {
      root.localStorage.setItem(storageKey(), JSON.stringify(normalized));
    } catch (e) { /* private mode: memory fallback keeps the session value */ }
  }

  function query() {
    var v = read();
    return 'uplinkDenoise=' + (v.uplink ? '1' : '0') +
      '&downlinkDenoise=' + (v.downlink ? '1' : '0');
  }

  /* Best-effort hot control on an OPEN media socket (the call page). */
  function control(socket) {
    if (!socket || socket.readyState !== 1) { return; }
    var v = read();
    try {
      socket.send(JSON.stringify({ type: 'denoise', uplink: v.uplink, downlink: v.downlink }));
    } catch (e) { /* control is best-effort; the PATCH already applied */ }
  }

  function notify() {
    var v = read();
    try {
      root.dispatchEvent(new CustomEvent('callrelay:denoise', {
        detail: { uplink: v.uplink, downlink: v.downlink },
      }));
    } catch (e) { /* ancient engines: persistence still applied */ }
  }

  function refreshBound() {
    if (!bound) { return; }
    var v = read();
    if (bound.up) { bound.up.checked = v.uplink; }
    if (bound.down) { bound.down.checked = v.downlink; }
  }

  function persist() {
    // Fresh read for the UNTOUCHED direction: changing one switch can never
    // overwrite the other's stored choice.
    var current = read();
    write({
      uplink: bound.up ? bound.up.checked : current.uplink,
      downlink: bound.down ? bound.down.checked : current.downlink,
    });
    notify();
  }

  /* Binds a checkbox pair (settings page). One registration: every later
   * setScope() (login, refresh, re-enroll) re-syncs the boxes from the
   * scoped store without adding listeners. */
  function bind(uplinkEl, downlinkEl) {
    bound = { up: uplinkEl || null, down: downlinkEl || null };
    if (uplinkEl) { uplinkEl.addEventListener('change', persist); }
    if (downlinkEl) { downlinkEl.addEventListener('change', persist); }
    refreshBound();
  }

  root.CallRelayDenoise = {
    setScope: setScope,
    scope: currentScope,
    clear: clear,
    read: read,
    write: write,
    query: query,
    control: control,
    bind: bind,
  };
}(typeof self !== 'undefined' ? self : this));
