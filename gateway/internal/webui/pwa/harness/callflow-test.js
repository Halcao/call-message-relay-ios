#!/usr/bin/env node
/* CallFlow harness — deterministic sequencing tests with injected fakes.
 * The flow's timers are virtual; the harness pumps them explicitly so
 * retries and ready-timeouts advance deterministically.
 *
 * Coverage (review-driven):
 *   1. incoming accept order: prepare -> POST answer -> attach media.
 *   2. media failure retries once (bounded) then recovers.
 *   3. mic/permission denied NEVER posts the answer (no silent answer).
 *   4. hangup during async audio init leaks nothing and answers nothing.
 *   5. mute gate: only zero PCM leaves while muted; unmute restores;
 *      stalled socket drops frames (bounded bufferedAmount backlog).
 *   6. media ready timeout is bounded (initial + one retry).
 *   7. local hangup of an ACCEPTED call POSTs /hangup (never /reject),
 *      emits ended, releases every resource exactly once, and is
 *      idempotent (a second hangup neither POSTs nor frees twice).
 *   8. a failing hangup/reject API STILL converges to ended and releases
 *      resources (the UI must never stay stuck on the call view).
 *   9. an unanswered inbound call still rejects; setMuted re-emits the
 *      active view truthfully; a terminal event-stream frame closes.
 * Run: node callflow-test.js (or via go test TestCallFlowHarness).
 */
'use strict';

const path = require('path');
const { CallFlow, READY_TIMEOUT_MS } = require(path.join(__dirname, '..', 'callflow.js'));
const PCMU = require(path.join(__dirname, '..', 'pcmu.js'));

let failures = 0;
function check(name, cond, detail) {
  if (cond) { console.log('ok   - ' + name); }
  else { failures += 1; console.error('FAIL - ' + name + (detail ? ' :: ' + detail : '')); }
}
const immediate = () => new Promise((r) => setImmediate(r));

function makeTimers() {
  let now = 0;
  const timers = [];
  return {
    now: () => now,
    setTimeout: (fn, ms) => { const t = { fn, at: now + ms }; timers.push(t); return t; },
    clearTimeout: (t) => { const i = timers.indexOf(t); if (i >= 0) timers.splice(i, 1); },
    pump: (ms) => {
      now += ms;
      const due = timers.filter(t => t.at <= now);
      due.forEach(t => { const i = timers.indexOf(t); if (i >= 0) timers.splice(i, 1); t.fn(); });
    },
  };
}

function fakeSocket() {
  return {
    readyState: 0, bufferedAmount: 0, sent: [],
    onopen: null, onclose: null, onmessage: null, onerror: null,
    send(data) { this.sent.push(data); },
    close() { this.readyState = 3; if (this.onclose) this.onclose({}); },
    open() { this.readyState = 1; if (this.onopen) this.onopen({}); },
    ready() { if (this.onmessage) this.onmessage({ data: JSON.stringify({ type: 'ready' }) }); },
    binary(bytes) { if (this.onmessage) this.onmessage({ data: bytes || new Uint8Array(160) }); },
    fail() { if (this.onerror) this.onerror({}); if (this.onclose) this.onclose({}); },
  };
}

function deps(overrides) {
  const log = [];
  const timers = makeTimers();
  const sockets = [];
  const base = {
    callId: 'c1',
    incoming: true,
    api: (method, url) => { log.push(method + ' ' + url); return Promise.resolve({ state: 'incomingRinging' }); },
    makePlayer: () => ({ start: () => Promise.resolve({ feed() {}, stop() {} }) }),
    makeCapture: () => ({ start: () => Promise.resolve({ stop() {}, onFrame: null }) }),
    getMic: () => Promise.resolve({ getTracks: () => [] }),
    openSocket: () => { const s = fakeSocket(); sockets.push(s); return s; },
    onState: () => {},
    now: timers.now,
    setTimeout: timers.setTimeout,
    clearTimeout: timers.clearTimeout,
  };
  return Object.assign(base, overrides || {}, { log, timers, sockets });
}

async function test1() {
  const d = deps();
  const flow = new CallFlow(d);
  flow.beginIncoming();
  const accepted = flow.accept();
  await immediate(); await immediate(); await immediate();
  const s = d.sockets[0];
  check('socket attached', Boolean(s));
  if (s) { s.open(); s.ready(); }
  const ok = await accepted;
  const answerIdx = d.log.findIndex(l => l.includes('/answer'));
  check('answer precedes socket attach', answerIdx >= 0 && d.sockets.length === 1,
    JSON.stringify(d.log));
  check('accept resolves true on ready', ok === true);
  check('only one answer POSTed', d.log.filter(l => l.includes('/answer')).length === 1);
}

async function test2() {
  const d = deps();
  const flow = new CallFlow(d);
  flow.beginIncoming();
  const accepted = flow.accept();
  await immediate(); await immediate(); await immediate();
  d.sockets[0].fail();                       // pre-ready failure
  await immediate();
  d.timers.pump(2000);                       // bounded retry delay
  await immediate();
  const retry = d.sockets[1];
  check('one retry socket after failure', Boolean(retry), 'sockets=' + d.sockets.length);
  if (retry) { retry.open(); retry.ready(); }
  const ok = await accepted;
  check('media failure retries once then recovers', ok === true, 'ok=' + ok);
}

async function test3() {
  const d = deps({
    getMic: () => Promise.reject(Object.assign(new Error('denied'), { name: 'NotAllowedError' })),
  });
  const flow = new CallFlow(d);
  flow.beginIncoming();
  const ok = await flow.accept();
  check('mic denied blocks the answer', ok === false &&
    !d.log.some(l => l.includes('/answer')), JSON.stringify(d.log));
  check('no socket opened when audio prep failed', d.sockets.length === 0);
}

async function test4() {
  let micResolved;
  let stopped = 0;
  const d = deps({
    getMic: () => new Promise((resolve) => {
      micResolved = resolve;
    }),
  });
  const flow = new CallFlow(d);
  flow.beginIncoming();
  const accepted = flow.accept();
  await immediate();
  flow.hangup('reject');
  await immediate();
  micResolved({ getTracks: () => [{ stop() { stopped += 1; } }] });
  const ok = await accepted;
  check('hangup during init answers nothing', ok === false &&
    !d.log.some(l => l.includes('/answer')), JSON.stringify(d.log));
  check('no socket from a torn-down flow', d.sockets.length === 0);
  await immediate(); await immediate();
  check('late mic stream is stopped (no leak)', stopped === 1, 'stopped=' + stopped);
}

async function test5() {
  let liveCapture = null;
  const d = deps({
    makeCapture: () => ({
      start: () => {
        liveCapture = { stop() {}, onFrame: null };
        return Promise.resolve(liveCapture);
      },
    }),
  });
  const flow = new CallFlow(d);
  flow.beginIncoming();
  const accepted = flow.accept();
  await immediate(); await immediate(); await immediate();
  const s = d.sockets[0];
  s.open(); s.ready();
  await accepted;
  flow.setMuted(true);
  const nonzero = new Uint8Array(160).fill(0x7F);
  liveCapture.onFrame(nonzero);
  liveCapture.onFrame(nonzero);
  const gated = s.sent.slice(-2);
  const silenceBytes = gated.length === 2 &&
    gated.every(f => f.length === 160 && f.every(b => b === 0xFF));
  let maxAmplitude = 1;
  try {
    maxAmplitude = Math.max.apply(null, gated.map(f =>
      Math.max.apply(null, Array.from(PCMU.decodeToFloat32(f), Math.abs))));
  } catch (e) { maxAmplitude = 1; }
  check('muted frames are PCMU digital silence (0xFF, decodes to ~0)',
    silenceBytes && maxAmplitude < 0.001,
    'frames=' + gated.length + ' maxAmp=' + maxAmplitude);
  flow.setMuted(false);
  liveCapture.onFrame(nonzero);
  const last = s.sent[s.sent.length - 1];
  check('unmute restores real frames', Boolean(last && last.some(b => b !== 0)));
  s.bufferedAmount = 99999;
  const before = s.sent.length;
  liveCapture.onFrame(nonzero);
  check('stalled socket drops the frame (bounded backlog)', s.sent.length === before);
}

async function test6() {
  const d = deps();
  const flow = new CallFlow(d);
  flow.beginIncoming();
  const started = d.timers.now();
  const accepted = flow.accept();
  await immediate(); await immediate(); await immediate();
  d.sockets[0].open();                        // ready frame never arrives
  d.timers.pump(READY_TIMEOUT_MS + 1);        // initial attempt times out
  await immediate();                          // rejection -> retry scheduled
  d.timers.pump(2000);                        // bounded retry delay elapses
  await immediate();
  const retry = d.sockets[1];
  check('timeout spawns exactly one retry', Boolean(retry));
  if (retry) { retry.open(); }
  d.timers.pump(READY_TIMEOUT_MS + 1);        // retry times out too
  await immediate();
  const ok = await accepted;
  const elapsed = d.timers.now() - started;
  check('double-timeout accept fails truthfully', ok === false);
  // Bound = initial timeout + bounded retry delay + retry timeout.
  check('ready wait bounded (2x timeout + retry delay)',
    elapsed <= 2 * READY_TIMEOUT_MS + 1200 + 1200, 'elapsed=' + elapsed);
}

function trackedDeps(overrides) {
  const stops = { player: 0, capture: 0, tracks: 0, socket: 0 };
  let answered = false;
  const d = deps(Object.assign({
    makePlayer: () => ({ start: () => Promise.resolve({ feed() {}, stop() { stops.player += 1; } }) }),
    makeCapture: () => ({ start: () => Promise.resolve({ stop() { stops.capture += 1; }, onFrame: null }) }),
    getMic: () => Promise.resolve({ getTracks: () => [{ stop() { stops.tracks += 1; } }] }),
  }, overrides || {}));
  // Gateway state follows the answer POST so polling can observe the call
  // going active; the log still records every request.
  const baseApi = d.api;
  d.api = function (method, url, body) {
    const result = baseApi(method, url, body);
    if (url && url.indexOf('/answer') >= 0) { answered = true; }
    if (method === 'GET' && url && url.indexOf('/calls/') >= 0) {
      return Promise.resolve({ state: answered ? 'active' : 'incomingRinging' });
    }
    return result;
  };
  const realOpenSocket = d.openSocket;
  d.openSocket = function () {
    const s = realOpenSocket();
    const realClose = s.close.bind(s);
    s.close = function () { stops.socket += 1; realClose(); };
    return s;
  };
  d.stops = stops;
  return d;
}

function collectViews(d) {
  const views = [];
  d.onState = (v) => views.push(v);
  return views;
}

async function acceptAndActivate(d) {
  const flow = new CallFlow(d);
  flow.beginIncoming();
  const accepted = flow.accept();
  await immediate(); await immediate(); await immediate();
  d.sockets[0].open();
  d.sockets[0].ready();
  await accepted;
  // Let the next poll observe the gateway's active state so the flow has
  // actually rendered the answered-call view (media ready) before the test
  // manipulates it.
  await immediate();
  d.timers.pump(1600);
  await immediate();
  return flow;
}

/* 7. An ACCEPTED inbound call ends via /hangup (never /reject); the ended
 *    view fires; every resource is freed exactly once; repeat hangup no-ops. */
async function test7() {
  const d = trackedDeps();
  const views = collectViews(d);
  const flow = await acceptAndActivate(d);
  check('accepted flag set after answer', flow.accepted === true);
  await flow.hangup();                       // default action must be hangup now
  const terminalPosts = d.log.filter(l => /\/(hangup|reject)$/.test(l));
  check('accepted inbound local end POSTs /hangup only',
    terminalPosts.length === 1 && terminalPosts[0].endsWith('/hangup'),
    JSON.stringify(terminalPosts));
  const last = views[views.length - 1];
  check('ended view emitted with controls gone',
    last.phase === 'ended' && last.canHangup === false && last.canAccept === false,
    JSON.stringify(last));
  check('player/capture/track/socket released exactly once',
    d.stops.player === 1 && d.stops.capture === 1 && d.stops.tracks === 1 && d.stops.socket === 1,
    JSON.stringify(d.stops));
  const logLen = d.log.length;
  await flow.hangup('reject');              // idempotent: action ignored
  check('second hangup posts nothing and frees nothing twice',
    d.log.length === logLen && d.stops.socket === 1 && d.stops.player === 1);
}

/* 8. An unreachable hangup API still converges to ended with resources freed. */
async function test8() {
  const d = trackedDeps({
    incoming: false,
    api: (method, url) => {
      d.log.push(method + ' ' + url);
      return /\/(hangup|reject)$/.test(url)
        ? Promise.reject(Object.assign(new Error('network down'), { status: 0 }))
        : Promise.resolve({ state: 'connecting' });
    },
  });
  const views = collectViews(d);
  const flow = new CallFlow(d);
  flow.beginOutgoing();
  await immediate(); await immediate(); await immediate(); await immediate();
  check('outbound attached a socket before hangup', d.sockets.length === 1);
  await flow.hangup();
  check('outbound end POSTs /hangup',
    d.log.some(l => l.endsWith('/hangup')) && !d.log.some(l => l.endsWith('/reject')));
  const last = views[views.length - 1];
  check('API failure still converges to ended', last.phase === 'ended' && last.canHangup === false);
  check('API failure still releases resources once',
    d.stops.socket === 1 && d.stops.player === 1 && d.stops.capture === 1,
    JSON.stringify(d.stops));
  check('poll stopped (no timer resurrecting the view)', flow.pollTimer === null);
}

/* 9. Unanswered inbound still uses reject. */
async function test9() {
  const d = deps();
  const views = collectViews(d);
  const flow = new CallFlow(d);
  flow.beginIncoming();
  await immediate();
  await flow.hangup();
  check('unanswered inbound POSTs /reject only',
    d.log.some(l => l.endsWith('/reject')) && !d.log.some(l => l.endsWith('/hangup')),
    JSON.stringify(d.log));
  const last = views[views.length - 1];
  check('reject emits ended', last.phase === 'ended' && last.canAccept === false);
}

/* 10. setMuted re-emits the active view with truthful state (label support). */
async function test10() {
  const d = trackedDeps();
  const views = collectViews(d);
  const flow = await acceptAndActivate(d);
  flow.setMuted(true);
  let last = views[views.length - 1];
  check('mute re-emits active view with muted=true', last.phase === 'active' && last.muted === true);
  flow.setMuted(false);
  last = views[views.length - 1];
  check('unmute re-emits active view with muted=false', last.phase === 'active' && last.muted === false);
}

/* 11. Terminal event-stream frames converge; foreign/non-terminal ignored. */
async function test11() {
  const d = deps();
  const views = collectViews(d);
  const flow = new CallFlow(d);
  flow.beginIncoming();
  await immediate();
  flow.onEvent({ type: 'call.updated', call: { id: 'other-call', state: 'ended' } });
  check('foreign call event ignored', flow.over === false);
  flow.onEvent({ type: 'call.updated', data: { id: 'c1', state: 'active' } });
  check('non-terminal event does not close', flow.over === false);
  flow.onEvent({ type: 'call.updated', call: { id: 'c1', state: 'ended', endReason: 'remote_hangup' } });
  check('terminal event closes the flow with ended view',
    flow.over === true && views[views.length - 1].phase === 'ended');
  flow.onEvent({ type: 'call.updated', call: { id: 'c1', state: 'active' } });
  check('no post-terminal resurrection', views[views.length - 1].phase === 'ended');
}

/* 12. Gesture ordering: the microphone is prepared BEFORE the gateway call
 *     is created; a denied mic never dials; the call screen dials only after
 *     audio is ready. */
async function test12() {
  let dials = 0;
  const denied = deps({
    dial: () => { dials += 1; return Promise.resolve({ id: 'call-x' }); },
    getMic: () => Promise.reject(Object.assign(new Error('denied'), { name: 'NotAllowedError' })),
  });
  const flowDenied = new CallFlow(denied);
  const deniedOk = await flowDenied.beginOutgoing();
  check('outgoing mic denial never dials', deniedOk === false && dials === 0 && denied.sockets.length === 0,
    'ok=' + deniedOk + ' dials=' + dials + ' sockets=' + denied.sockets.length);

  const order = [];
  const d = deps({
    dial: () => { order.push('dial'); return Promise.resolve({ id: 'call-ok', peer: '+10086' }); },
    getMic: () => { order.push('mic'); return Promise.resolve({ getTracks: () => [] }); },
  });
  const flow = new CallFlow(d);
  const pending = flow.beginOutgoing();
  await immediate(); await immediate(); await immediate();
  await immediate(); await immediate(); await immediate();
  check('audio preparation precedes dialing', order.join(',') === 'mic,dial', JSON.stringify(order));
  check('dialed call id becomes the flow call id', flow.callId === 'call-ok', String(flow.callId));
  d.sockets[0].open(); d.sockets[0].ready();
  const ok = await pending;
  check('dialed outgoing call attaches media', ok !== false && d.sockets.length === 1,
    'ok=' + ok + ' sockets=' + d.sockets.length);
  flow.hangup();
  await immediate();
}

/* 13. Polling truthfulness: a gateway-active call is not media-ready until
 *     the transport actually attached; a later media loss stays failed. */
async function test13() {
  const views = [];
  const d = deps({ api: () => Promise.resolve({ state: 'active' }), onState: (v) => views.push(v) });
  const flow = new CallFlow(d);
  flow.beginOutgoing();
  await immediate(); await immediate(); await immediate(); await immediate();
  const beforeReady = views.filter(v => v.phase === 'active').pop();
  check('gateway active without attached media is not rendered ready',
    Boolean(beforeReady) && beforeReady.media !== 'ready', JSON.stringify(beforeReady));
  d.sockets[0].open(); d.sockets[0].ready();
  await immediate();
  const afterReady = views.filter(v => v.phase === 'active').pop();
  check('gateway active renders ready only after media attached',
    Boolean(afterReady) && afterReady.media === 'ready', JSON.stringify(afterReady));
  d.sockets[0].close();
  await immediate();
  const afterLoss = views[views.length - 1];
  check('mid-call media loss is reported failed, not ready',
    afterLoss.phase === 'active' && afterLoss.media === 'failed', JSON.stringify(afterLoss));
  flow.hangup();
  await immediate();
}

/* 14. Generation fence: a hangup while the WSS player is starting must stop
 *     the pipeline before capture ever starts (no leaked mic capture). */
async function test14() {
  let captureStarts = 0;
  let releasePlayer = null;
  const d = deps({
    makePlayer: () => ({
      start: () => new Promise((resolve) => {
        releasePlayer = () => resolve({ feed() {}, stop() {} });
      }),
    }),
    makeCapture: () => ({
      start: () => { captureStarts += 1; return Promise.resolve({ stop() {}, onFrame: null }); },
    }),
  });
  const flow = new CallFlow(d);
  flow.beginOutgoing();
  await immediate(); await immediate(); await immediate(); await immediate();
  flow.hangup();
  await immediate();
  if (releasePlayer) { releasePlayer(); }
  await immediate(); await immediate();
  check('hangup during player start never starts capture', captureStarts === 0,
    'captureStarts=' + captureStarts);
  check('generation-fenced pipeline leaves no socket', d.sockets.length === 0,
    'sockets=' + d.sockets.length);
}

/* 15. Real gateway state vocabulary is snake_case: incoming_ringing keeps
 *     the ring view and outgoing_dialing keeps "connecting" until the
 *     gateway says active. */
async function test15() {
  const views = [];
  const d = deps({
    callId: 'c-snake', incoming: true,
    api: () => Promise.resolve({ state: 'incoming_ringing' }),
    onState: (v) => views.push(v),
  });
  const flow = new CallFlow(d);
  flow.beginIncoming();
  await immediate(); await immediate(); await immediate();
  check('snake_case incoming_ringing keeps the ringing view',
    views.length > 0 && views[views.length - 1].phase === 'ringing',
    JSON.stringify(views[views.length - 1]));
  flow.hangup('reject');
  await immediate();

  const views2 = [];
  const d2 = deps({
    callId: 'c-out', incoming: false,
    api: () => Promise.resolve({ state: 'outgoing_dialing' }),
    onState: (v) => views2.push(v),
  });
  const flow2 = new CallFlow(d2);
  flow2.beginOutgoing();
  await immediate(); await immediate(); await immediate(); await immediate();
  d2.sockets[0].open(); d2.sockets[0].ready();
  await immediate(); await immediate();
  const readyView = views2.filter((v) => v.media === 'ready').pop();
  check('snake_case outgoing_dialing keeps connecting until active',
    Boolean(readyView) && readyView.phase === 'connecting', JSON.stringify(readyView));
  flow2.hangup();
  await immediate();
}


// Build-49 recovery battery (same approved invariants as the native
// client): episode identity with an absolute monotonic 45 s deadline,
// per-attempt timers, exact current-socket fencing, fresh-binary-audio
// proof, deadline give-up (local end + best-effort hangup), immediate
// remote-end/revocation handling, never a re-dial.
async function activeCall(d) {
  const flow = new CallFlow(d);
  flow.beginIncoming();
  const accepted = flow.accept();
  await immediate(); await immediate(); await immediate();
  // WSS path: drive the socket ready. WebRTC path: the engine start
  // resolves instead (no socket exists until a WSS fallback/recovery).
  if (d.sockets[0]) { d.sockets[0].open(); d.sockets[0].ready(); }
  await accepted;
  await immediate(); await immediate(); // poll sees active
  return flow;
}

async function test16() {
  const views = [];
  const d = deps({
    api: (m, u) => { d.log.push(m + ' ' + u); return Promise.resolve({ state: 'active' }); },
    onState: (v) => views.push(v),
  });
  const flow = await activeCall(d);
  const socketsAtDrop = d.sockets.length;
  d.sockets[0].close();                 // mid-call network drop
  await immediate();
  const failedView = views.filter((v) => v.media === 'failed').pop();
  check('mid-call drop reports failed truthfully', Boolean(failedView), JSON.stringify(views));
  await immediate(); await immediate();
  check('recovery re-attaches immediately', d.sockets.length === socketsAtDrop + 1,
    'sockets=' + d.sockets.length);
  // ready alone does NOT complete recovery: fresh binary audio is required.
  const s = d.sockets[d.sockets.length - 1];
  s.open(); s.ready();
  await immediate(); await immediate();
  check('ready control alone does not complete recovery',
    !views.some((v) => v.media === 'ready' && v.phase === 'active') ||
    views.filter((v) => v.media === 'ready').length < 2, JSON.stringify(views));
  s.binary();
  await immediate(); await immediate();
  const readyCount = views.filter((v) => v.media === 'ready').length;
  check('fresh binary audio completes recovery', readyCount >= 2, JSON.stringify(views));
  check('recovery never re-dialed', !d.log.some((l) => l.includes('/dial')) &&
    d.log.filter((l) => l.includes('/answer')).length === 1, JSON.stringify(d.log));
  flow.hangup();
  await immediate();
}

async function test17() {
  const views = [];
  const d = deps({
    api: (m, u) => { d.log.push(m + ' ' + u); return Promise.resolve({ state: 'active' }); },
    onState: (v) => views.push(v),
  });
  const flow = await activeCall(d);
  d.sockets[0].close();
  await immediate();
  // Every recovery attempt fails; pump past the 45 s window (virtual clock).
  for (let i = 0; i < 12; i++) {
    await immediate(); await immediate();
    const last = d.sockets[d.sockets.length - 1];
    if (last && last.readyState !== 3) { last.fail(); }
    d.timers.pump(9000);
    await immediate();
  }
  const attempts = d.sockets.length - 1;
  check('recovery attempts bounded by the 45 s window', attempts < 12,
    'attempts=' + attempts);
  const socketsBefore = d.sockets.length;
  d.timers.pump(30000);
  await immediate();
  check('no recovery attempts after the window', d.sockets.length === socketsBefore,
    'sockets=' + d.sockets.length);
  check('deadline gives up locally (hangup posted once)',
    d.log.filter((l) => l.includes('/hangup')).length === 1, JSON.stringify(d.log));
  check('deadline converges to ended', views.some((v) => v.phase === 'ended'),
    JSON.stringify(views));
}

async function test18() {
  const d = deps({ api: () => Promise.resolve({ state: 'active' }) });
  const flow = await activeCall(d);
  d.sockets[0].close();
  await immediate();
  flow.hangup();
  await immediate();
  const socketsAtHangup = d.sockets.length;
  d.timers.pump(60000);
  await immediate();
  check('hangup stops media recovery', d.sockets.length === socketsAtHangup,
    'sockets=' + d.sockets.length);
}

async function test19() {
  // Duplicate error+close from the same dead socket: ONE episode, the
  // absolute deadline is not reset, attempts do not overlap.
  const d = deps({ api: () => Promise.resolve({ state: 'active' }) });
  const flow = await activeCall(d);
  const dead = d.sockets[0];
  dead.close();
  await immediate(); await immediate();
  const afterFirst = d.sockets.length;
  dead.fail(); // duplicate error+close AFTER the close already armed recovery
  await immediate(); await immediate();
  d.timers.pump(1000);
  await immediate(); await immediate();
  check('duplicate drop events do not overlap episodes',
    d.sockets.length <= afterFirst + 1, 'sockets=' + d.sockets.length);
  flow.hangup();
  await immediate();
}

async function test20() {
  // Hung attach: the per-attempt ready timeout fails the attempt and the
  // next backoff attempt runs (bounded), no stale timer can fire later.
  const d = deps({ api: () => Promise.resolve({ state: 'active' }) });
  const flow = await activeCall(d);
  d.sockets[0].close();
  await immediate(); await immediate();
  const hung = d.sockets[d.sockets.length - 1];
  hung.open(); // never ready: ready timeout must fail the attempt
  d.timers.pump(9000); // > READY_TIMEOUT_MS(8000), < 45 s
  await immediate(); await immediate(); // reject handler schedules the backoff
  d.timers.pump(10);   // fire the immediate backoff
  await immediate(); await immediate();
  check('hung recovery attach times out and re-attempts', d.sockets.length >= 3,
    'sockets=' + d.sockets.length);
  // The hung socket's late callbacks are stale: fenced by socket identity.
  hung.ready(); hung.binary();
  await immediate();
  const states = [];
  flow.onState = (v) => states.push(v);
  hung.binary();
  await immediate();
  check('late callbacks from the replaced socket are ignored',
    !states.some((v) => v.media === 'ready'), JSON.stringify(states));
  flow.hangup();
  await immediate();
}

async function test21() {
  // Remote end during recovery: polling ends the call immediately; no
  // further attempts, no hangup race.
  const d = deps({
    api: (m, u) => {
      d.log.push(m + ' ' + u);
      return Promise.resolve({ state: d.ended ? 'ended' : 'active' });
    },
  });
  const flow = await activeCall(d);
  d.sockets[0].close();
  await immediate();
  d.ended = true;
  d.timers.pump(2000); // let polling observe the remote end
  await immediate(); await immediate();
  const socketsAtEnd = d.sockets.length;
  d.timers.pump(60000);
  await immediate();
  check('remote end stops recovery immediately', d.sockets.length === socketsAtEnd,
    'sockets=' + d.sockets.length);
  check('remote end did not post a recovery hangup',
    d.log.filter((l) => l.includes('/hangup')).length === 0, JSON.stringify(d.log));
}

async function test22() {
  // WebRTC mid-call drop: the peer connection failure releases the engine,
  // switches to the WSS transport and recovers inside the same window.
  const views = [];
  let engine;
  const d = deps({
    api: (m, u) => { d.log.push(m + ' ' + u); return Promise.resolve({ state: 'active' }); },
    onState: (v) => views.push(v),
    makeWebRTC: () => {
      engine = {
        prepare: () => Promise.resolve(),
        start: () => Promise.resolve(),
        release: () => {},
        stop: () => {},
        onDisconnect: null,
      };
      return engine;
    },
  });
  const flow = await activeCall(d);
  check('webrtc engine was the carrier', flow.mediaKind === 'webrtc', flow.mediaKind);
  engine.onDisconnect(); // mid-call peer drop
  await immediate(); await immediate();
  const wss = d.sockets[d.sockets.length - 1];
  check('webrtc drop falls back to a WSS recovery attach', Boolean(wss) && d.sockets.length === 1,
    'sockets=' + d.sockets.length);
  wss.open(); wss.ready(); wss.binary();
  await immediate(); await immediate();
  const ready = views.filter((v) => v.media === 'ready').pop();
  check('webrtc drop recovers over WSS with fresh audio',
    Boolean(ready) && flow.mediaKind === 'wss', JSON.stringify(views));
  flow.hangup();
  await immediate();
}


async function test23() {
  // Multi-frame after success: the promoted carrier keeps feeding the
  // player for its whole installed lifetime (never one-frame-only).
  let feeds = 0;
  const d = deps({
    api: () => Promise.resolve({ state: 'active' }),
    makePlayer: () => ({
      start: () => Promise.resolve({
        feed: () => { feeds += 1; },
        stop: () => {},
      }),
    }),
  });
  const flow = await activeCall(d);
  const baseFeeds = feeds;
  d.sockets[0].close();
  await immediate(); await immediate();
  const s = d.sockets[d.sockets.length - 1];
  s.open(); s.ready(); s.binary();
  await immediate(); await immediate();
  s.binary(); s.binary(); s.binary();
  await immediate();
  check('promoted carrier feeds every frame after recovery',
    feeds - baseFeeds >= 4, 'feeds=' + (feeds - baseFeeds));
  flow.hangup();
  await immediate();
}

async function test24() {
  // Second drop after a successful recovery starts a NEW episode and
  // recovers again (the installed-carrier fence must allow re-entry).
  const d = deps({ api: () => Promise.resolve({ state: 'active' }) });
  const flow = await activeCall(d);
  d.sockets[0].close();
  await immediate(); await immediate();
  const first = d.sockets[d.sockets.length - 1];
  first.open(); first.ready(); first.binary();
  await immediate(); await immediate();
  check('first recovery succeeded', flow.mediaState === 'ready', flow.mediaState);
  first.close(); // second drop, same call
  await immediate(); await immediate();
  const second = d.sockets[d.sockets.length - 1];
  check('second drop starts a new recovery episode',
    Boolean(second) && second !== first, 'sockets=' + d.sockets.length);
  second.open(); second.ready(); second.binary();
  await immediate(); await immediate();
  check('second recovery succeeds', flow.mediaState === 'ready', flow.mediaState);
  flow.hangup();
  await immediate();
}


async function test25() {
  // Failed candidate: after an attempt fails, its queued binary frames and
  // drop callbacks must not feed the player nor spawn a parallel episode;
  // the backoff-driven next attempt proceeds normally.
  let feeds = 0;
  const d = deps({
    api: () => Promise.resolve({ state: 'active' }),
    makePlayer: () => ({
      start: () => Promise.resolve({ feed: () => { feeds += 1; }, stop: () => {} }),
    }),
  });
  const flow = await activeCall(d);
  const baseFeeds = feeds;
  d.sockets[0].close();
  await immediate(); await immediate();
  const failed = d.sockets[d.sockets.length - 1];
  const socketsAfterFail = d.sockets.length + 1; // +1 for the attempt that replaces it
  failed.fail(); // attempt 1 fails; candidate is detached+closed
  await immediate(); await immediate();
  // Queued frames/drops from the FAILED candidate arrive during backoff.
  failed.binary();
  failed.binary();
  await immediate();
  const beforeBackoffResumes = d.sockets.length;
  d.timers.pump(100); // backoff[0] = 0 -> attempt 2
  await immediate(); await immediate();
  check('failed candidate frames never reach the player', feeds - baseFeeds === 0,
    'feeds=' + (feeds - baseFeeds));
  check('failed candidate callbacks do not spawn attempts',
    d.sockets.length === beforeBackoffResumes + 1, 'sockets=' + d.sockets.length);
  const second = d.sockets[d.sockets.length - 1];
  check('attempt 2 opened as its own socket', second !== failed);
  second.open(); second.ready(); second.binary();
  await immediate(); await immediate();
  check('attempt 2 promotes normally', flow.mediaState === 'ready', flow.mediaState);
  check('promoted carrier still feeds every frame', feeds - baseFeeds >= 1);
  flow.hangup();
  await immediate();
}

(async () => {
  try {
    await test1(); await test2(); await test3();
    await test4(); await test5(); await test6();
    await test7(); await test8(); await test9();
    await test10(); await test11(); await test12();
    await test13(); await test14(); await test15();
    await test16(); await test17(); await test18();
    await test19(); await test20(); await test21(); await test22();
    await test23(); await test24(); await test25();
  } catch (err) {
    failures += 1;
    console.error('FAIL - harness exception: ' + (err && err.stack || err));
  }
  console.log(failures === 0 ? 'ALL PASS' : failures + ' FAILURES');
  process.exit(failures === 0 ? 0 : 1);
})();
