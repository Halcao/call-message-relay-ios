'use strict';
/* Harness for the PWA service worker's notificationclick routing.
 *
 * Run: node sw-notificationclick-test.js
 *
 * Loads sw.js with a mock `self` and drives the notificationclick listener
 * with fake window clients, asserting:
 *   1. A click when the call page is already open (same pathname) restarts
 *      the page via ONE postMessage and focuses it — never navigate(), so
 *      the single-use handoff token is redeemed exactly once and a second
 *      notification with a new token still succeeds.
 *   2. A click with no open same-path client navigates (full reload path)
 *      and never posts a message.
 *   3. Targets without a fragment never produce a bogus hash message.
 */
const fs = require('fs');
const path = require('path');
const vm = require('vm');

const swSource = fs.readFileSync(path.join(__dirname, '..', 'sw.js'), 'utf8');

function loadSW() {
  const listeners = {};
  const sandbox = {
    self: {
      registration: { scope: 'https://gw.example/pwa/' },
      addEventListener: (type, fn) => { listeners[type] = fn; },
      clients: undefined // set per scenario
    },
    URL,
    Promise,
    console
  };
  sandbox.self.self = sandbox.self;
  vm.runInContext(swSource, vm.createContext(sandbox));
  return { listeners, sandbox };
}

function fakeClient(url, calls) {
  return {
    url,
    focus: () => { calls.focus++; return Promise.resolve(undefined); },
    navigate: (target) => { calls.navigate.push(target); return Promise.resolve({ focus: () => { calls.focus++; return Promise.resolve(undefined); } }); },
    postMessage: (msg) => { calls.messages.push(msg); }
  };
}

function click(listeners, target, clients) {
  const sandboxClients = {
    matchAll: () => Promise.resolve(clients),
    openWindow: (t) => { calls.openWindow.push(t); return Promise.resolve(undefined); }
  };
  const calls = { focus: 0, navigate: [], messages: [], openWindow: [] };
  // Wire clients into the sandbox via a closure: sw.js references self.clients.
  // Re-load with per-scenario clients.
  return { sandboxClients, calls };
}

async function runScenario(target, clientURLs) {
  const { listeners, sandbox } = loadSW();
  const calls = { focus: 0, navigate: [], messages: [], openWindow: [] };
  const clients = clientURLs.map((u) => fakeClient(u, calls));
  sandbox.self.clients = {
    matchAll: () => Promise.resolve(clients),
    openWindow: (t) => { calls.openWindow.push(t); return Promise.resolve(undefined); }
  };
  const event = {
    notification: { close: () => {}, data: { navigate: target } },
    waitUntil: (p) => p
  };
  await listeners.notificationclick(event);
  return calls;
}

async function main() {
  let failed = false;
  function assert(name, cond, detail) {
    if (cond) { console.log('PASS ' + name); } else { failed = true; console.log('FAIL ' + name + ' :: ' + JSON.stringify(detail)); }
  }

  // 1. Same call page already open, second notification with a NEW token:
  //    exactly one message, zero navigations (single redemption), focused.
  let calls = await runScenario(
    'https://gw.example/pwa/call#h=TOKEN_NEW',
    ['https://gw.example/pwa/call?t=9#h=TOKEN_OLD']
  );
  assert('same-page: one message', calls.messages.length === 1, calls.messages);
  assert('same-page: message carries the new hash', calls.messages[0] && calls.messages[0].hash === '#h=TOKEN_NEW', calls.messages);
  assert('same-page: no navigate (single redemption)', calls.navigate.length === 0, calls.navigate);
  assert('same-page: focused once', calls.focus === 1, calls.focus);
  assert('same-page: no openWindow', calls.openWindow.length === 0, calls.openWindow);

  // 2. Setup page open (different pathname): navigate full reload, no message.
  calls = await runScenario(
    'https://gw.example/pwa/call#h=TOKEN_1',
    ['https://gw.example/pwa/']
  );
  assert('diff-page: one navigate', calls.navigate.length === 1 && calls.navigate[0] === 'https://gw.example/pwa/call#h=TOKEN_1', calls.navigate);
  assert('diff-page: no message', calls.messages.length === 0, calls.messages);
  assert('diff-page: focused', calls.focus === 1, calls.focus);

  // 3. No client at all: openWindow, no message, no navigate.
  calls = await runScenario('https://gw.example/pwa/call#h=TOKEN_2', []);
  assert('no-client: openWindow', calls.openWindow.length === 1, calls.openWindow);
  assert('no-client: no message', calls.messages.length === 0, calls.messages);

  // 4. Target without fragment never emits a bogus hash message.
  calls = await runScenario('https://gw.example/pwa/', ['https://gw.example/pwa/']);
  assert('no-fragment: no bogus hash message', calls.messages.length === 0, calls.messages);
  assert('no-fragment: focused', calls.focus === 1, calls.focus);

  process.exit(failed ? 1 : 0);
}

main().catch((err) => { console.error(err); process.exit(1); });
