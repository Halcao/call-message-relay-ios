#!/usr/bin/env node
/* ThreadLogic harness — deterministic rules behind cross-line SMS sends.
 *
 * Reproduces the observed defect: open a line1 thread, switch the header
 * picker to line2, send — the returned line2 canonical thread must open
 * showing the message, the picker must stay on line2, and a SECOND send
 * must also use line2 (no snap-back to the global default). Also locks:
 *   - canonical key follows the gateway MessageView.threadKey;
 *   - the constructed lineId:peer key is only a fallback;
 *   - retry keeps the failed message's ORIGINAL line (no silent fallback);
 *   - the in-flight guard is the caller's boolean (documented here).
 * Run: node threadlogic-test.js (or via go test TestThreadLogicHarness).
 */
'use strict';

const path = require('path');
const TL = require(path.join(__dirname, '..', 'threadlogic.js'));

let failures = 0;
function check(name, cond, detail) {
  if (cond) { console.log('ok   - ' + name); }
  else { failures += 1; console.error('FAIL - ' + name + (detail ? ' :: ' + detail : '')); }
}

/* A faithful in-memory model of client.js send state + the fixture API:
 * the send lands on the chosen line's canonical thread and echoes once.
 * `threads` maps key -> {key, lineId} like the gateway ThreadView list. */
function simulateConversation(scenario) {
  const defaultLineId = scenario.defaultLineId;
  const sendCapable = scenario.lines;
  let prefs = Object.assign({}, scenario.prefs || {});
  const threads = scenario.threads || null;
  const messagesByThread = {};
  let sends = [];

  function preferred(key) {
    const view = threads && threads[key];
    return TL.preferredLine({
      pref: prefs[key] || '',
      thread: view || (key ? { key: key } : null),
      globalDefault: defaultLineId,
      permitted: sendCapable.map(function (id) { return { id: id }; }),
    });
  }
  function apiSend(peer, body, lineId) {
    sends.push({ peer, body, lineId });
    const threadKey = TL.keyFor(lineId, peer);
    const record = { id: 'm' + sends.length, threadKey, lineId,
      direction: 'outbound', peer, body, status: 'sent' };
    (messagesByThread[threadKey] = messagesByThread[threadKey] || []).push(record);
    return Promise.resolve(record);
  }
  /* Mirrors sendFromComposer() with the CURRENT picker value. */
  function send(openKey, peer, pickerValue, body) {
    const lineId = TL.chosenLine(pickerValue, preferred(openKey));
    return apiSend(peer, body, lineId).then(function (record) {
      const canonical = TL.canonicalKey(record, lineId, peer);
      TL.carryPreference(prefs, openKey, canonical, lineId);
      return { canonical, lineId, record };
    });
  }
  /* What the reopened header picker shows: client.js openThread fill. */
  function pickerFor(key) { return preferred(key); }

  return {
    send,
    pickerFor,
    messages: messagesByThread,
    sends: sends,
    prefs: function () { return prefs; },
  };
}

function testCrossLineSticks() {
  const sim = simulateConversation({ defaultLineId: 'line1', lines: ['line1', 'line2'] });
  const peer = '555-0123';
  const oldKey = TL.keyFor('line1', peer);
  // User opened the line1 thread, switched the header picker to line2, sent.
  return sim.send(oldKey, peer, 'line2', 'first').then(function (r1) {
    check('first send uses the picked line2', r1.lineId === 'line2', r1.lineId);
    check('canonical thread is line2:peer', r1.canonical === 'line2:555-0123', r1.canonical);
    check('returned thread picker stays line2', sim.pickerFor(r1.canonical) === 'line2');
    check('old line1 thread preference also remembers line2 choice', sim.pickerFor(oldKey) === 'line2');
    check('new message present exactly once in the line2 thread',
      sim.messages[r1.canonical].length === 1 && sim.messages[r1.canonical][0].body === 'first');
    // Second send from the REOPENED thread: must still be line2.
    return sim.send(r1.canonical, peer, sim.pickerFor(r1.canonical), 'second');
  }).then(function (r2) {
    check('second send also uses line2 (no snap-back)', r2.lineId === 'line2', r2.lineId);
    check('second send lands on the same canonical thread', r2.canonical === 'line2:555-0123');
    check('each message shown exactly once (no duplicate echo)',
      sim.messages['line2:555-0123'].length === 2 && sim.sends.length === 2,
      JSON.stringify(sim.sends));
  });
}

function testCanonicalAuthorityAndFallback() {
  check('returned threadKey is authoritative',
    TL.canonicalKey({ threadKey: 'line9:peer' }, 'line2', 'peer') === 'line9:peer');
  check('lineId:peer is the fallback for a deficient response',
    TL.canonicalKey({}, 'line2', 'peer') === 'line2:peer');
  check('null/undefined record also falls back',
    TL.canonicalKey(null, 'line1', 'x') === 'line1:x');
}

function testChosenLineNeverSubstitutesDefault() {
  check('picker value wins over the fallback',
    TL.chosenLine('line2', 'line1') === 'line2');
  check('empty picker uses the preference fallback',
    TL.chosenLine('', 'line2') === 'line2');
  check('no silent empty fallback', TL.chosenLine('', '') === '');
}

function testRetryProvenance() {
  check('retry keeps the failed message original line',
    TL.retryLine({ lineId: 'line2', threadKey: 'line2:555' }, 'line1') === 'line2');
  check('only a record without provenance uses the preference',
    TL.retryLine({ lineId: '', threadKey: 'line1:555' }, 'line1') === 'line1');
}

function testCarryBothKeys() {
  const prefs = {};
  TL.carryPreference(prefs, 'line1:peer', 'line2:peer', 'line2');
  check('preference carried to both the viewed and canonical key',
    prefs['line1:peer'] === 'line2' && prefs['line2:peer'] === 'line2');
  TL.carryPreference(prefs, 'a', 'b', '');
  check('empty line never overwrites preferences',
    prefs.a === undefined && prefs.b === undefined);
}

/* The final omission: an EXISTING line2 conversation, with no explicit
 * preference and global default line1, must default to its OWN line2 —
 * not silently compose from line1. Explicit authorized preference still
 * wins; unavailable/unauthorized/stale candidates are skipped. */
function testExistingThreadOwnLineDefault() {
  const lines = [{ id: 'line1' }, { id: 'line2' }];

  check('existing line2 thread + no pref + global line1 => line2 (own line)',
    TL.preferredLine({
      pref: '', thread: { key: 'line2:555-0789', lineId: 'line2' },
      globalDefault: 'line1', permitted: lines,
    }) === 'line2');

  check('own line also recovered from the validated canonical key alone',
    TL.preferredLine({
      pref: '', thread: { key: 'line2:555-0789' },
      globalDefault: 'line1', permitted: lines,
    }) === 'line2');

  check('existing line1 thread keeps line1 (same answer either way)',
    TL.preferredLine({
      pref: '', thread: { key: 'line1:555-0123', lineId: 'line1' },
      globalDefault: 'line1', permitted: lines,
    }) === 'line1');

  check('explicit authorized override is retained over the thread own line',
    TL.preferredLine({
      pref: 'line1', thread: { key: 'line2:555-0789', lineId: 'line2' },
      globalDefault: 'line1', permitted: lines,
    }) === 'line1');

  check('explicit override to line2 on a line1 thread is retained',
    TL.preferredLine({
      pref: 'line2', thread: { key: 'line1:555-0123', lineId: 'line1' },
      globalDefault: 'line1', permitted: lines,
    }) === 'line2');

  check('new compose (no thread, no pref) uses the global default',
    TL.preferredLine({
      pref: '', thread: null,
      globalDefault: 'line1', permitted: lines,
    }) === 'line1');

  check('no global default falls through to the first permitted line',
    TL.preferredLine({
      pref: '', thread: null, globalDefault: '', permitted: lines,
    }) === 'line1');
}

function testUnavailableUnauthorizedLinesSkipped() {
  const both = [{ id: 'line1' }, { id: 'line2' }];

  check('stale pref for an unavailable SIM is skipped -> thread own line',
    TL.preferredLine({
      pref: 'line3', thread: { key: 'line2:555', lineId: 'line2' },
      globalDefault: 'line1', permitted: both,
    }) === 'line2');

  check('thread own line unavailable (SIM removed) -> global default',
    TL.preferredLine({
      pref: '', thread: { key: 'line3:555', lineId: 'line3' },
      globalDefault: 'line1', permitted: both,
    }) === 'line1');

  check('line in the canonical key but unauthorized is never selected',
    TL.preferredLine({
      pref: '', thread: { key: 'line2:555' },
      globalDefault: 'line1',
      // only line1 is currently permitted (e.g. line2 lost sendSms):
      permitted: [{ id: 'line1' }],
    }) === 'line1');

  check('global default itself unavailable -> first permitted line',
    TL.preferredLine({
      pref: '', thread: null, globalDefault: 'line2',
      permitted: [{ id: 'line1' }],
    }) === 'line1');

  check('no permitted lines yields empty string, never a foreign id',
    TL.preferredLine({
      pref: 'line2', thread: { key: 'line2:555', lineId: 'line2' },
      globalDefault: 'line1', permitted: [],
    }) === '');

  check('malformed key yields no own line and falls to the default',
    TL.preferredLine({
      pref: '', thread: { key: 'not-a-canonical-key' },
      globalDefault: 'line1', permitted: both,
    }) === 'line1');

  check('empty/colon-only key halves yield no own line',
    TL.lineFromKey(':peer') === '' && TL.lineFromKey('line2:') === '' &&
    TL.lineFromKey('line2') === '' && TL.lineFromKey('a:b:c') === '' &&
    TL.lineFromKey('line2:peer') === 'line2');
}

/* End-to-end through the send simulation: reopen an existing line2 thread
 * with no preference under a line1 global default and send — the message
 * must go from line2, and a second send must stay there. */
function testExistingLine2ThreadEndToEnd() {
  const peer = '555-0789';
  const key = TL.keyFor('line2', peer);
  const sim = simulateConversation({
    defaultLineId: 'line1',
    lines: ['line1', 'line2'],
    threads: { 'line2:555-0789': { key: key, lineId: 'line2' } },
  });
  check('picker opens on the thread own line2 despite global line1',
    sim.pickerFor(key) === 'line2', sim.pickerFor(key));
  return sim.send(key, peer, sim.pickerFor(key), 'hello').then(function (r1) {
    check('first send from the existing thread uses line2', r1.lineId === 'line2');
    return sim.send(r1.canonical, peer, sim.pickerFor(r1.canonical), 'again');
  }).then(function (r2) {
    check('second send stays on line2', r2.lineId === 'line2');
    check('both messages shown once', sim.sends.length === 2);
  });
}

(async () => {
  try {
    await testCrossLineSticks();
    testCanonicalAuthorityAndFallback();
    testChosenLineNeverSubstitutesDefault();
    testRetryProvenance();
    testCarryBothKeys();
    testExistingThreadOwnLineDefault();
    testUnavailableUnauthorizedLinesSkipped();
    await testExistingLine2ThreadEndToEnd();
  } catch (err) {
    failures += 1;
    console.error('FAIL - harness exception: ' + (err && err.stack || err));
  }
  console.log(failures === 0 ? 'ALL PASS' : failures + ' FAILURES');
  process.exit(failures === 0 ? 0 : 1);
})();
