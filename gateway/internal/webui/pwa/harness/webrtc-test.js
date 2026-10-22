#!/usr/bin/env node
/* WebRTC media harness — browser-native direct path + WSS fallback.
 *
 * Covers, with injected fakes (no browser, no network, no audio):
 *   1. start(): GET ice -> offer -> POST /webrtc/offer -> connected; the
 *      gesture phase acquires the mic exactly once and the peer connection
 *      carries the same tracks.
 *   2. mute toggles the real track.enabled (not just a label).
 *   3. stop() closes the peer connection and stops the mic tracks; idempotent.
 *   4. a failed negotiation rejects AND releases the peer connection while
 *      KEEPING the gesture-granted mic stream for the WSS fallback.
 *   5. CallFlow prefers WebRTC: a healthy direct attach never opens the WSS
 *      socket and reports mediaKind=webrtc.
 *   6. CallFlow falls back to WSS on direct failure, reusing ONE mic stream
 *      and reporting mediaKind=wss.
 *   7. abort() cancels in-flight negotiation without touching the mic, so a
 *      late peer answer cannot revive an abandoned transport.
 *
 * Run: node webrtc-test.js (or via go test TestWebRTCHarness).
 */
'use strict';

const path = require('path');
const WebRTC = require(path.join(__dirname, '..', 'webrtc.js'));
const { CallFlow, READY_TIMEOUT_MS } = require(path.join(__dirname, '..', 'callflow.js'));

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
      timers.filter(t => t.at <= now).forEach(t => {
        const i = timers.indexOf(t);
        if (i >= 0) { timers.splice(i, 1); }
        t.fn();
      });
    },
  };
}

function makeStream() {
  const tracks = [{ enabled: true, stopped: false, stop() { this.stopped = true; } }];
  return {
    tracks,
    getTracks: () => tracks,
    getAudioTracks: () => tracks,
  };
}

class FakePC {
  constructor(config) {
    this.config = config;
    this.listeners = {};
    this.iceGatheringState = 'complete';
    this.connectionState = 'new';
    this.addedTracks = [];
    this.closed = false;
    FakePC.instances.push(this);
  }
  addTrack(track, stream) { this.addedTracks.push({ track, stream }); }
  addEventListener(name, fn) { (this.listeners[name] = this.listeners[name] || []).push(fn); }
  removeEventListener(name, fn) {
    const list = this.listeners[name] || [];
    const i = list.indexOf(fn);
    if (i >= 0) { list.splice(i, 1); }
  }
  emit(name) { (this.listeners[name] || []).slice().forEach(fn => fn()); }
  createOffer() {
    if (this.failOffer) { return Promise.reject(new Error('offer failed')); }
    return Promise.resolve({ type: 'offer', sdp: 'sdp-offer' });
  }
  setLocalDescription(offer) { this.localDescription = offer; return Promise.resolve(); }
  setRemoteDescription(answer) {
    this.remoteDescription = answer;
    if (this.ontrack) { this.ontrack({ streams: [makeStream()] }); }
    this.connectionState = 'connected';
    this.emit('connectionstatechange');
    return Promise.resolve();
  }
  close() {
    this.closed = true;
    this.connectionState = 'closed';
    this.emit('connectionstatechange');
  }
}
FakePC.instances = [];

class FakeAudioContext {
  constructor() { this.state = 'suspended'; this.closed = false; }
  resume() { this.state = 'running'; return Promise.resolve(); }
  createMediaStreamSource() { return { connect() {}, disconnect() {} }; }
  close() { this.closed = true; return Promise.resolve(); }
}

function makeMedia(opts) {
  opts = opts || {};
  const calls = [];
  const media = new WebRTC.WebRTCMedia({
    callId: 'call-1',
    PeerConnection: opts.PeerConnection || FakePC,
    AudioContext: FakeAudioContext,
    api: (method, url, body) => {
      calls.push({ method, url, body });
      if (url.indexOf('/ice') >= 0) { return Promise.resolve({ iceServers: [{ urls: 'stun:example' }] }); }
      return Promise.resolve({ sdp: 'sdp-answer', type: 'answer' });
    },
    getMic: () => Promise.resolve(makeStream()),
    setTimeout: (fn, ms) => setTimeout(fn, ms),
    clearTimeout: (t) => clearTimeout(t),
  });
  media.__calls = calls;
  return media;
}

async function testStartNegotiation() {
  FakePC.instances = [];
  const media = makeMedia();
  await media.start();
  const pc = FakePC.instances[0];
  check('ice fetched then offer posted',
    media.__calls.length === 2 &&
    media.__calls[0].url.indexOf('/ice') >= 0 && media.__calls[0].method === 'GET' &&
    media.__calls[1].url.indexOf('/webrtc/offer') >= 0 && media.__calls[1].method === 'POST',
    JSON.stringify(media.__calls));
  check('offer carries transport=ice', media.__calls[1].body && media.__calls[1].body.transport === 'ice');
  check('local mic tracks added to the peer connection',
    pc.addedTracks.length === 1 && pc.addedTracks[0].stream === media.stream);
  check('ice servers passed to the peer connection',
    pc.config.iceServers.length === 1 && pc.config.iceServers[0].urls === 'stun:example');
  check('audio context resumed and remote route attached',
    media.ctx && media.ctx.state === 'running' && Boolean(media.remoteNode));
}

async function testMuteAndStop() {
  FakePC.instances = [];
  const media = makeMedia();
  await media.start();
  const pc = FakePC.instances[0];
  media.setMuted(true);
  check('mute disables the live track', media.stream.getAudioTracks()[0].enabled === false);
  media.setMuted(false);
  check('unmute re-enables the live track', media.stream.getAudioTracks()[0].enabled === true);
  const stream = media.stream;
  media.stop();
  check('stop closes the peer connection', pc.closed === true);
  check('stop stops the mic track', stream.getTracks()[0].stopped === true);
  media.stop();
  check('stop is idempotent', pc.closed === true);
}

async function testNegotiationFailureReleases() {
  FakePC.instances = [];
  class FailingPC extends FakePC {
    constructor(config) { super(config); this.failOffer = true; }
  }
  const media = makeMedia({ PeerConnection: FailingPC });
  let rejected = false;
  await media.start().catch(() => { rejected = true; });
  check('failed negotiation rejects', rejected);
  check('failed negotiation closes the peer connection',
    FailingPC.instances[0].closed === true);
  check('failed negotiation KEEPS the shared mic stream alive',
    media.stream && media.stream.getTracks()[0].stopped === false,
    'stopped=' + (media.stream && media.stream.getTracks()[0].stopped));
  check('failed negotiation posts no offer after the failed offer step',
    media.__calls.length === 1, JSON.stringify(media.__calls));
  media.stop();
  check('terminal stop after failure stops the mic track',
    media.stream === null || media.stream.getTracks()[0].stopped === true);
}

async function testAbortCancelsInFlightNegotiation() {
  FakePC.instances = [];
  let releaseOffer = null;
  class HangingPC extends FakePC {
    createOffer() {
      return new Promise((resolve) => { releaseOffer = () => resolve({ type: 'offer', sdp: 'late' }); });
    }
  }
  const media = makeMedia({ PeerConnection: HangingPC });
  let rejected = false;
  const pending = media.start().catch(() => { rejected = true; });
  await immediate();
  media.abort();
  if (releaseOffer) { releaseOffer(); }
  await pending;
  await immediate();
  check('abort rejects the in-flight start', rejected);
  check('abort does not touch the mic stream',
    media.stream && media.stream.getTracks()[0].stopped === false);
  check('abort stops the late offer from posting', media.__calls.length === 1,
    JSON.stringify(media.__calls));
  check('abort closes the abandoned peer connection',
    HangingPC.instances[0] && HangingPC.instances[0].closed === true);
  media.stop();
  check('stop after abort still stops the mic',
    media.stream === null || media.stream.getTracks()[0].stopped === true);
}

function callflowDeps(overrides) {
  const log = [];
  const timers = makeTimers();
  const sockets = [];
  const streams = [];
  const base = {
    callId: 'c1',
    incoming: false,
    api: (method, url) => { log.push(method + ' ' + url); return Promise.resolve({ state: 'active' }); },
    makePlayer: () => ({ start: () => Promise.resolve({ feed() {}, stop() {} }) }),
    makeCapture: () => ({ start: () => Promise.resolve({ stop() {}, onFrame: null }) }),
    getMic: () => { const s = makeStream(); streams.push(s); return Promise.resolve(s); },
    openSocket: () => { const s = fakeSocket(); sockets.push(s); return s; },
    onState: () => {},
    now: timers.now,
    setTimeout: timers.setTimeout,
    clearTimeout: timers.clearTimeout,
  };
  const deps = Object.assign(base, overrides || {}, { log, timers, sockets, streams });
  return deps;
}

function fakeSocket() {
  return {
    readyState: 0, bufferedAmount: 0, sent: [],
    onopen: null, onclose: null, onmessage: null, onerror: null,
    send(data) { this.sent.push(data); },
    close() { this.readyState = 3; if (this.onclose) { this.onclose({}); } },
    ready() { if (this.onmessage) { this.onmessage({ data: JSON.stringify({ type: 'ready' }) }); } },
    fail() { if (this.onerror) { this.onerror({}); } },
  };
}

function fakeWebRTC(opts) {
  opts = opts || {};
  return function () {
    return {
      stream: null,
      prepared: 0,
      started: 0,
      released: 0,
      stopped: 0,
      aborted: 0,
      getMic: opts.getMic,
      setCallId() {},
      prepare() {
        this.prepared += 1;
        var self = this;
        return Promise.resolve().then(function () { return self.getMic(); })
          .then(function (stream) { self.stream = stream; return self; });
      },
      start() {
        this.started += 1;
        if (opts.hang) { return new Promise(function () {}); }
        if (opts.fail) { return Promise.reject(new Error('direct failed')); }
        return Promise.resolve();
      },
      setMuted() {},
      abort() { this.aborted += 1; },
      release() { this.released += 1; },
      stop() { this.stopped += 1; },
    };
  };
}

async function testCallFlowPrefersWebRTC() {
  const states = [];
  const d = callflowDeps({ onState: (v) => states.push(v) });
  d.makeWebRTC = fakeWebRTC({ getMic: d.getMic });
  const flow = new CallFlow(d);
  const ok = await flow.beginOutgoing();
  await immediate();
  check('webrtc call becomes active without a WSS socket',
    ok !== false && d.sockets.length === 0, 'ok=' + ok + ' sockets=' + d.sockets.length);
  check('mic acquired exactly once', d.streams.length === 1, 'streams=' + d.streams.length);
  const active = states.filter(s => s.phase === 'active').pop();
  check('active view reports mediaKind=webrtc',
    active && active.mediaKind === 'webrtc' && active.media === 'ready', JSON.stringify(active));
  flow.hangup();
  await immediate();
}

async function testCallFlowFallsBackToWSS() {
  const states = [];
  const d = callflowDeps({ onState: (v) => states.push(v) });
  d.makeWebRTC = fakeWebRTC({ fail: true, getMic: d.getMic });
  const flow = new CallFlow(d);
  const pending = flow.beginOutgoing();
  await immediate(); await immediate(); await immediate();
  check('fallback opens the WSS socket', d.sockets.length === 1, 'sockets=' + d.sockets.length);
  if (d.sockets[0]) { d.sockets[0].ready(); }
  const ok = await pending;
  check('fallback call becomes active', ok !== false, 'ok=' + ok);
  check('fallback reuses the single mic stream', d.streams.length === 1,
    'streams=' + d.streams.length);
  const active = states.filter(s => s.phase === 'active').pop();
  check('active view reports mediaKind=wss',
    active && active.mediaKind === 'wss', JSON.stringify(active));
  flow.hangup();
  await immediate();
}

async function testCallFlowTimeoutCancelsWebRTC() {
  const states = [];
  const d = callflowDeps({ onState: (v) => states.push(v) });
  let engine = null;
  d.makeWebRTC = function () { engine = fakeWebRTC({ hang: true, getMic: d.getMic })(); return engine; };
  const flow = new CallFlow(d);
  const pending = flow.beginOutgoing();
  await immediate(); await immediate(); await immediate();
  d.timers.pump(READY_TIMEOUT_MS + 1);        // outer attach timeout
  await immediate(); await immediate(); await immediate();
  check('direct timeout aborts the engine', engine && engine.aborted === 1,
    'aborted=' + (engine && engine.aborted));
  check('direct timeout falls back to WSS', d.sockets.length === 1,
    'sockets=' + d.sockets.length);
  if (d.sockets[0]) { d.sockets[0].ready(); }
  const ok = await pending;
  check('timeout fallback stays on WSS with the single mic',
    ok !== false && d.streams.length === 1, 'ok=' + ok + ' streams=' + d.streams.length);
  const active = states.filter(s => s.phase === 'active').pop();
  check('timeout fallback reports mediaKind=wss',
    active && active.mediaKind === 'wss', JSON.stringify(active));
  flow.hangup();
  await immediate();
}

async function main() {
  await testStartNegotiation();
  await testMuteAndStop();
  await testNegotiationFailureReleases();
  await testAbortCancelsInFlightNegotiation();
  await testCallFlowPrefersWebRTC();
  await testCallFlowFallsBackToWSS();
  await testCallFlowTimeoutCancelsWebRTC();
  if (failures > 0) { console.error(failures + ' FAILURE(S)'); process.exit(1); }
  console.log('ALL PASS');
  process.exit(0);
}

main().catch((err) => { console.error(err); process.exit(1); });
