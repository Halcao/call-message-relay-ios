#!/usr/bin/env node
/* Denoise preference harness (build-49):
 * scoped per-enrollment storage, default-OFF, strict opt-in parsing, logout
 * clearing, private-mode memory fallback, change notification, attach query
 * and hot control payloads.
 * Run: node denoise-test.js (or via go test TestDenoiseHarness).
 */
'use strict';

const path = require('path');
const Module = require('module');

let failures = 0;
function check(name, cond, detail) {
  if (cond) { console.log('ok   - ' + name); }
  else { failures += 1; console.error('FAIL - ' + name + (detail ? ' :: ' + detail : '')); }
}

function makeRoot(storage) {
  const listeners = {};
  return {
    localStorage: storage,
    dispatchEvent: (ev) => {
      (listeners[ev.type] || []).forEach((fn) => fn(ev));
    },
    addEventListener: (type, fn) => {
      (listeners[type] = listeners[type] || []).push(fn);
    },
    _listeners: listeners,
  };
}

function loadDenoise(root) {
  // denoise.js binds to `self`; eval it against our fake root.
  const src = require('fs').readFileSync(path.join(__dirname, '..', 'denoise.js'), 'utf8');
  const fn = new Function('self', src);
  fn(root);
  return root.CallRelayDenoise;
}

function memoryStorage() {
  const map = new Map();
  return {
    getItem: (k) => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => map.set(k, String(v)),
    removeItem: (k) => map.delete(k),
    _map: map,
  };
}

// 1. Default OFF, strict opt-in, unscoped key.
{
  const storage = memoryStorage();
  const D = loadDenoise(makeRoot(storage));
  const v = D.read();
  check('default is OFF for both directions', v.uplink === false && v.downlink === false,
    JSON.stringify(v));
  storage.setItem('callrelay.denoise.v1.anon', '{}');
  const empty = D.read();
  check('empty stored object stays OFF (strict opt-in)',
    empty.uplink === false && empty.downlink === false, JSON.stringify(empty));
  storage.setItem('callrelay.denoise.v1.anon', '{"uplink":true,"downlink":false}');
  const one = D.read();
  check('explicit stored choice wins', one.uplink === true && one.downlink === false,
    JSON.stringify(one));
  check('query reflects the choice', D.query() === 'uplinkDenoise=1&downlinkDenoise=0', D.query());
}

// 2. Scoped storage: different enrollments get different keys; raw seed is
//    never the key; clear() removes only the scoped entry.
{
  const storage = memoryStorage();
  const root = makeRoot(storage);
  const D = loadDenoise(root);
  D.setScope('device-A');
  D.write({ uplink: true, downlink: false });
  const keysA = [...storage._map.keys()];
  // Device ids are non-secret and encoded directly (only raw TOKENS are
  // hashed — see the token test below).
  check('scoped key is enrollment-specific',
    keysA.length === 1 && keysA[0] === 'callrelay.denoise.v1.d.device-A', keysA.join(','));
  D.setScope('device-B');
  check('other enrollment starts at OFF', D.read().uplink === false);
  D.setScope('device-A');
  check('original enrollment keeps its choice', D.read().uplink === true);
  D.clear();
  check('clear() removes the scoped entry', storage._map.size === 0);
  D.setScope('device-B');
  D.write({ uplink: false, downlink: true });
  D.setScope('device-A');
  check('clear() only cleared the logged-out enrollment', D.read().uplink === false);
  check('unscoped default is OFF', D.read().downlink === false);
}

// 3. Change notification + control payload.
{
  const storage = memoryStorage();
  const root = makeRoot(storage);
  const D = loadDenoise(root);
  D.setScope('dev-1');
  let events = 0;
  let lastDetail = null;
  root.addEventListener('callrelay:denoise', (ev) => { events += 1; lastDetail = ev.detail; });
  const up = { checked: false, addEventListener: (t, fn) => { up._fn = fn; } };
  const down = { checked: true, addEventListener: (t, fn) => { down._fn = fn; } };
  D.bind(up, down);
  check('bind reflects the stored (default OFF) state', up.checked === false && down.checked === false, up.checked + '/' + down.checked);
  up.checked = true;
  up._fn();
  check('change emits an event with the new choice',
    events === 1 && lastDetail && lastDetail.uplink === true, JSON.stringify(lastDetail));
  check('change persisted', D.read().uplink === true);
  const sent = [];
  const openSocket = { readyState: 1, send: (m) => sent.push(m) };
  D.control(openSocket);
  check('control payload carries both directions',
    sent.length === 1 && JSON.parse(sent[0]).type === 'denoise' &&
    JSON.parse(sent[0]).uplink === true, sent.join(','));
  D.control({ readyState: 0, send: () => { throw new Error('must not send'); } });
  check('control is a no-op on a closed socket', true);
}

// 4. Private-mode fallback: writes survive in memory for the session.
{
  const badStorage = {
    getItem: () => { throw new Error('blocked'); },
    setItem: () => { throw new Error('blocked'); },
    removeItem: () => { throw new Error('blocked'); },
  };
  const D = loadDenoise(makeRoot(badStorage));
  D.write({ uplink: true, downlink: true });
  check('private mode keeps the choice in memory', D.read().uplink === true);
  check('private mode query still works', D.query() === 'uplinkDenoise=1&downlinkDenoise=1');
}


// 5. Boot order (actual page sequence): the switches bind BEFORE the async
//    session status arrives; setScope must resync them without new listeners,
//    and a same-scope refresh must keep the private-mode memory value.
{
  const storage = memoryStorage();
  const root = makeRoot(storage);
  const D = loadDenoise(root);
  const up = { checked: true, listeners: [], addEventListener(t, fn) { this.listeners.push(fn); } };
  const down = { checked: true, listeners: [], addEventListener(t, fn) { this.listeners.push(fn); } };
  D.bind(up, down);
  check('pre-login bind shows the OFF default', up.checked === false && down.checked === false);
  // Session status arrives with device-A; its stored choice was uplink ON.
  D.setScope('device-A');
  storage.setItem('callrelay.denoise.v1.d.device-A', '{"uplink":true,"downlink":false}');
  D.setScope('device-A'); // same-scope refresh (idempotent)
  check('setScope resyncs the bound switches', up.checked === true && down.checked === false,
    up.checked + '/' + down.checked);
  check('no duplicate listeners after resync', up.listeners.length === 1, up.listeners.length);
  // Changing ONE direction preserves the other's stored choice.
  up.checked = false;
  up.listeners[0]();
  check('changing uplink preserves the stored downlink choice',
    D.read().uplink === false && D.read().downlink === false, JSON.stringify(D.read()));
  // Private mode: same-scope refresh keeps the in-memory value.
  const badRoot = makeRoot({
    getItem: () => { throw new Error('blocked'); },
    setItem: () => { throw new Error('blocked'); },
    removeItem: () => { throw new Error('blocked'); },
  });
  const D2 = loadDenoise(badRoot);
  D2.setScope('dev-x');
  D2.write({ uplink: true, downlink: false });
  D2.setScope('dev-x');
  check('same-scope refresh keeps the private-mode value', D2.read().uplink === true);
}

// 6. Legacy origin-global values (the build-48 default-on era) are never
//    inherited by unscoped or scoped state.
{
  const storage = memoryStorage();
  storage.setItem('callrelay.denoise.v1', '{"uplink":true,"downlink":true}');
  const D = loadDenoise(makeRoot(storage));
  const unscoped = D.read();
  check('legacy origin-global value is not inherited (unscoped OFF)',
    unscoped.uplink === false && unscoped.downlink === false, JSON.stringify(unscoped));
  D.setScope('dev-new');
  D.write({ uplink: true, downlink: false });
  check('scoped write never lands on the legacy key',
    storage.getItem('callrelay.denoise.v1') === '{"uplink":true,"downlink":true}' &&
    storage.getItem('callrelay.denoise.v1.d.dev-new') === '{"uplink":true,"downlink":false}');
  // Raw-token seeds are hashed, never stored raw.
  D.setScope('tok-enrollment-secret-value');
  const keys = [...storage._map.keys()];
  check('token seed never appears raw in a key',
    keys.every((k) => !k.includes('tok-enrollment-secret-value')), keys.join(','));
}

console.log(failures === 0 ? 'ALL PASS' : failures + ' FAILURES');
process.exit(failures === 0 ? 0 : 1);
