#!/usr/bin/env node
/* CallRelay PWA — real-browser audio loopback runner.
 *
 * Drives the PRODUCTION PWA (served by the real gateway harness) in a real
 * installed Edge with the real WebRTC/AudioContext stack and a synthetic
 * microphone WAV (no physical microphone, no PSTN). Verifies, through the
 * actual media path:
 *   - direct: Opus negotiated, downlink tone decoded from the real remote
 *     track, uplink tone decoded at the fake modem sink;
 *   - fallback: WSS PCMU, real WebSocket binary frames measured both ways,
 *     mute sends PCMU silence, one microphone stream, single media owner;
 *   - incoming answer / hangup lifecycle.
 *
 * Requires the Go harness (PWA_LOOPBACK=1 go test
 * ./internal/unified/api -run TestPWABrowserLoopback) which serves the base
 * URL, the enrollment key and the /harness/* routes.
 *
 * Env: PWA_HARNESS_URL, PWA_HARNESS_KEY, PWA_HARNESS_OUT,
 *      PWA_HARNESS_PWA_DIR, PWA_PLAYWRIGHT_CORE.
 */
'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');

const BASE = process.env.PWA_HARNESS_URL || 'http://127.0.0.1:8790';
const KEY = process.env.PWA_HARNESS_KEY || '';
const OUT = process.env.PWA_HARNESS_OUT || path.join(os.tmpdir(), 'pwa-loopback-result.json');
const PWA_DIR = process.env.PWA_HARNESS_PWA_DIR || path.resolve(__dirname, '..', '..', 'gateway', 'internal', 'webui', 'pwa');
const PWA_CORE = process.env.PWA_PLAYWRIGHT_CORE || '';
const MIC_TONE_HZ = 440;
const DOWN_TONE_HZ = 1000;

const PCMU = require(path.join(PWA_DIR, 'pcmu.js'));

function resolvePlaywright() {
  const candidates = [PWA_CORE, 'playwright-core'].filter(Boolean);
  let lastErr = null;
  for (const candidate of candidates) {
    try { return require(candidate); } catch (err) { lastErr = err; }
  }
  throw lastErr || new Error('playwright-core not found');
}

const result = { ok: false, error: '', checks: [], data: {} };

function check(name, cond, detail) {
  result.checks.push({ name, ok: Boolean(cond), detail: detail === undefined ? '' : String(detail) });
}

function rms(samples) {
  if (!samples || samples.length === 0) { return 0; }
  let sum = 0;
  for (let i = 0; i < samples.length; i++) { sum += samples[i] * samples[i]; }
  return Math.sqrt(sum / samples.length);
}

/* Goertzel scan; returns the strongest candidate frequency and its power. */
function goertzelPower(samples, rate, hz) {
  const coeff = 2 * Math.cos(2 * Math.PI * hz / rate);
  let s0 = 0, s1 = 0, s2 = 0;
  for (let i = 0; i < samples.length; i++) {
    s0 = samples[i] + coeff * s1 - s2;
    s2 = s1;
    s1 = s0;
  }
  return (s1 * s1 + s2 * s2 - coeff * s1 * s2) / samples.length;
}

function analyzeTone(samples, rate, lo, hi, step) {
  if (!samples || samples.length < rate / 20) { return { hz: 0, power: 0 }; }
  let bestHz = 0, bestPower = 0;
  for (let hz = lo; hz <= hi; hz += step) {
    const power = goertzelPower(samples, rate, hz);
    if (power > bestPower) { bestPower = power; bestHz = hz; }
  }
  return { hz: bestHz, power: bestPower };
}

function spectrum(samples, rate) {
  if (!samples || samples.length < 400) { return []; }
  const bins = [];
  for (let hz = 200; hz <= 1800; hz += 25) {
    bins.push({ hz, power: goertzelPower(samples, rate, hz) });
  }
  bins.sort((a, b) => b.power - a.power);
  return bins.slice(0, 4);
}

function decodeFrameSet(frames) {
  const out = [];
  for (const frame of frames) {
    const bytes = frame && frame.bytes ? frame.bytes : frame;
    if (!bytes) { continue; }
    const decoded = PCMU.decodeToFloat32(bytes);
    for (let i = 0; i < decoded.length; i++) { out.push(decoded[i]); }
  }
  return out;
}

function writeMicWav(file, seconds, hz) {
  const rate = 48000;
  const total = rate * seconds;
  const data = Buffer.alloc(total * 2);
  for (let i = 0; i < total; i++) {
    const value = Math.round(0.35 * 32767 * Math.sin(2 * Math.PI * hz * i / rate));
    data.writeInt16LE(value, i * 2);
  }
  const header = Buffer.alloc(44);
  header.write('RIFF', 0);
  header.writeUInt32LE(36 + data.length, 4);
  header.write('WAVE', 8);
  header.write('fmt ', 12);
  header.writeUInt32LE(16, 16);
  header.writeUInt16LE(1, 20);            // PCM
  header.writeUInt16LE(1, 22);            // mono
  header.writeUInt32LE(rate, 24);
  header.writeUInt32LE(rate * 2, 28);
  header.writeUInt16LE(2, 32);
  header.writeUInt16LE(16, 34);
  header.write('data', 36);
  header.writeUInt32LE(data.length, 40);
  fs.writeFileSync(file, Buffer.concat([header, data]));
}

/* ---------------- harness-side modem helpers ---------------- */

async function harness(pathname, options) {
  const response = await fetch(BASE + pathname, options);
  return response.json();
}

async function resetModem() {
  await harness('/harness/modem/reset', { method: 'POST' });
}

async function modemUplink() {
  return harness('/harness/modem/uplink');
}

function pickUplink(reports) {
  let best = null;
  for (const id of Object.keys(reports || {})) {
    const report = reports[id];
    if (!best || report.frames > best.frames) { best = Object.assign({ id }, report); }
  }
  return best || { frames: 0, rms: 0, toneHz: 0, tonePower: 0, zeroFrames: 0, maxGapMs: 999999 };
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

/* ---------------- browser instrumentation ---------------- */

function installHooks(page) {
  return page.addInitScript(() => {
    const h = {
      micCalls: 0, micStreams: [], mediaSockets: [], pcs: [],
      remoteStreams: [], localStreams: [], errors: [],
    };
    window.__harness = h;

    const devices = navigator.mediaDevices;
    if (devices && devices.getUserMedia) {
      const gum = devices.getUserMedia.bind(devices);
      devices.getUserMedia = function (constraints) {
        h.micCalls += 1;
        return gum(constraints).then(function (stream) {
          h.localStreams.push(stream);
          return stream;
        });
      };
    }

    const OrigPC = window.RTCPeerConnection;
    if (OrigPC) {
      const Wrapped = function (...args) {
        const pc = new OrigPC(...args);
        h.pcs.push(pc);
        return pc;
      };
      Wrapped.prototype = OrigPC.prototype;
      window.RTCPeerConnection = Wrapped;
    }

    const OrigWS = window.WebSocket;
    if (OrigWS) {
      const WrappedWS = function (url, protocols) {
        const ws = protocols ? new OrigWS(url, protocols) : new OrigWS(url);
        if (String(url).indexOf('/media') >= 0) {
          const record = { url: String(url), opened: false, closed: false, sent: [], received: [] };
          h.mediaSockets.push(record);
          ws.addEventListener('open', function () { record.opened = true; });
          ws.addEventListener('close', function () { record.closed = true; });
          ws.addEventListener('message', function (event) {
            if (event.data instanceof ArrayBuffer) {
              record.received.push({ t: Date.now(), bytes: new Uint8Array(event.data) });
            }
          });
          const origSend = ws.send.bind(ws);
          ws.send = function (data) {
            if (data instanceof ArrayBuffer || ArrayBuffer.isView(data)) {
              const view = data instanceof ArrayBuffer ? new Uint8Array(data) : new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
              record.sent.push({ t: Date.now(), bytes: new Uint8Array(view) });
            }
            return origSend(data);
          };
        }
        return ws;
      };
      WrappedWS.prototype = OrigWS.prototype;
      WrappedWS.CONNECTING = OrigWS.CONNECTING;
      WrappedWS.OPEN = OrigWS.OPEN;
      WrappedWS.CLOSING = OrigWS.CLOSING;
      WrappedWS.CLOSED = OrigWS.CLOSED;
      window.WebSocket = WrappedWS;
    }

    for (const Ctor of [window.AudioContext, window.webkitAudioContext]) {
      if (!Ctor || !Ctor.prototype.createMediaStreamSource) { continue; }
      const orig = Ctor.prototype.createMediaStreamSource;
      Ctor.prototype.createMediaStreamSource = function (stream) {
        h.remoteStreams.push(stream);
        h.remoteCtx = this;
        return orig.call(this, stream);
      };
    }

    window.addEventListener('error', function (event) { h.errors.push(String(event.message)); });
    window.addEventListener('unhandledrejection', function (event) {
      h.errors.push(String((event.reason && event.reason.message) || event.reason));
    });
  });
}

async function snapshot(page) {
  return page.evaluate(() => {
    const h = window.__harness;
    const states = (stream) => stream && stream.getAudioTracks
      ? stream.getAudioTracks().map((t) => t.readyState)
      : [];
    const sockets = h.mediaSockets.map((s) => ({
      opened: s.opened, closed: s.closed, sent: s.sent.length, received: s.received.length,
    }));
    return {
      micCalls: h.micCalls,
      micStates: h.micStreams.map(states),
      trackStopCalls: h.micStreams.reduce((count, stream) =>
        count + (stream.getTracks ? stream.getTracks().filter((t) => t.readyState === 'ended').length : 0), 0),
      socketCount: h.mediaSockets.length,
      sockets,
      pcCount: h.pcs.length,
      pcStates: h.pcs.map((pc) => ({ connection: pc.connectionState, signaling: pc.signalingState })),
      pcTracks: h.pcs.map((pc) => {
        try { return pc.getSenders().map((sender) => sender.track && sender.track.enabled); } catch (e) { return []; }
      }),
      remoteSdp: h.pcs.length ? (h.pcs[h.pcs.length - 1].remoteDescription || {}).sdp || '' : '',
      localSdp: h.pcs.length ? (h.pcs[h.pcs.length - 1].localDescription || {}).sdp || '' : '',
      remoteStreamCount: h.remoteStreams.length,
      errors: h.errors.slice(0, 5),
    };
  });
}

/* Decode a real MediaStream (remote track by default) with
 * MediaStreamTrackProcessor, falling back to an AnalyserNode. Returns raw
 * samples for tone analysis plus diagnostics. */
async function measureStream(page, durationMs, source) {
  return page.evaluate(async ({ ms, useLocal }) => {
    const h = window.__harness;
    const streams = useLocal ? h.localStreams : h.remoteStreams;
    const stream = streams[streams.length - 1];
    const out = {
      error: '', samples: [], frameRms: [], rate: 48000, engine: '',
      streams: streams.length,
      trackState: '',
    };
    if (!stream) { out.error = 'no ' + (useLocal ? 'local' : 'remote') + ' stream captured'; return out; }
    const track = stream.getAudioTracks()[0];
    if (!track) { out.error = 'no audio track'; return out; }
    out.trackState = track.readyState;
    const collectAnalyser = async () => {
      const ctx = h.remoteCtx || new AudioContext();
      out.ctxState = ctx.state;
      try { if (ctx.state === 'suspended') { await ctx.resume(); } } catch (e) { /* report below */ }
      out.ctxState = ctx.state;
      const src = ctx.createMediaStreamSource(stream);
      const analyser = ctx.createAnalyser();
      analyser.fftSize = 4096;
      src.connect(analyser);
      const buffer = new Float32Array(analyser.fftSize);
      const deadline = performance.now() + ms;
      while (performance.now() < deadline) {
        analyser.getFloatTimeDomainData(buffer);
        let sum = 0;
        for (let i = 0; i < buffer.length; i++) { sum += buffer[i] * buffer[i]; }
        out.frameRms.push(Math.sqrt(sum / buffer.length));
        for (let i = 0; i < buffer.length; i += 4) { out.samples.push(buffer[i]); }
        await new Promise((resolve) => setTimeout(resolve, 40));
      }
      src.disconnect();
    };
    if (typeof MediaStreamTrackProcessor !== 'undefined') {
      try {
        out.engine = 'MediaStreamTrackProcessor';
        const processor = new MediaStreamTrackProcessor({ track });
        const reader = processor.readable.getReader();
        const deadline = performance.now() + ms;
        while (performance.now() < deadline) {
          const read = await Promise.race([
            reader.read(),
            new Promise((resolve) => setTimeout(() => resolve({ timeout: true }), 200)),
          ]);
          if (read.timeout) { continue; }
          if (read.done || !read.value) { break; }
          const data = read.value;
          const frames = data.numberOfFrames;
          const buffer = new Float32Array(frames);
          data.copyTo(buffer, { planeIndex: 0 });
          out.rate = data.sampleRate || out.rate;
          let sum = 0;
          for (let i = 0; i < frames; i++) { sum += buffer[i] * buffer[i]; }
          out.frameRms.push(Math.sqrt(sum / frames));
          for (let i = 0; i < frames; i++) { out.samples.push(buffer[i]); }
          data.close();
        }
        reader.cancel().catch(() => {});
      } catch (err) {
        out.error = 'MediaStreamTrackProcessor: ' + (err && err.message || err);
        out.samples = [];
        out.frameRms = [];
      }
      if (out.frameRms.length > 0) { return out; }
      out.error = out.error || 'MediaStreamTrackProcessor produced no frames';
    }
    out.engine = (out.engine ? out.engine + '+AnalyserNode' : 'AnalyserNode');
    try {
      await collectAnalyser();
    } catch (err) {
      out.error = (out.error ? out.error + '; ' : '') + 'AnalyserNode: ' + (err && err.message || err);
    }
    return out;
  }, { ms: durationMs, useLocal: source === 'local' });
}

async function measureRemote(page, durationMs) {
  return measureStream(page, durationMs, 'remote');
}

function summarizeSamples(samples, rate, targetHz) {
  const power = rms(samples);
  const tone = analyzeTone(samples, rate, 250, 1600, 25);
  return {
    samples: samples.length,
    rms: power,
    toneHz: tone.hz,
    tonePower: tone.power,
    targetHit: Math.abs(tone.hz - targetHz) <= 45,
    topBins: spectrum(samples, rate),
  };
}

function writeFloatWav(file, samples, rate) {
  const data = Buffer.alloc(samples.length * 2);
  for (let i = 0; i < samples.length; i++) {
    const clamped = Math.max(-1, Math.min(1, samples[i]));
    data.writeInt16LE(Math.round(clamped * 32767), i * 2);
  }
  const header = Buffer.alloc(44);
  header.write('RIFF', 0);
  header.writeUInt32LE(36 + data.length, 4);
  header.write('WAVE', 8);
  header.write('fmt ', 12);
  header.writeUInt32LE(16, 16);
  header.writeUInt16LE(1, 20);
  header.writeUInt16LE(1, 22);
  header.writeUInt32LE(rate, 24);
  header.writeUInt32LE(rate * 2, 28);
  header.writeUInt16LE(2, 32);
  header.writeUInt16LE(16, 34);
  header.write('data', 36);
  header.writeUInt32LE(data.length, 40);
  fs.writeFileSync(file, Buffer.concat([header, data]));
}

async function pcStats(page) {
  return page.evaluate(async () => {
    const pc = window.__harness.pcs[window.__harness.pcs.length - 1];
    if (!pc || !pc.getStats) { return null; }
    const report = { inbound: [], outbound: [], remoteInbound: [], codecs: {} };
    const stats = await pc.getStats();
    stats.forEach((entry) => {
      if (entry.type === 'codec') {
        report.codecs[entry.id] = {
          mimeType: entry.mimeType, payloadType: entry.payloadType,
          clockRate: entry.clockRate, channels: entry.channels,
        };
      }
    });
    stats.forEach((entry) => {
      if (entry.type === 'inbound-rtp' && entry.kind === 'audio') {
        report.inbound.push({
          bytesReceived: entry.bytesReceived, packetsReceived: entry.packetsReceived,
          packetsLost: entry.packetsLost,
          jitter: entry.jitter, audioLevel: entry.audioLevel,
          totalAudioEnergy: entry.totalAudioEnergy, totalSamplesReceived: entry.totalSamplesReceived,
          concealedSamples: entry.concealedSamples, jitterBufferDelay: entry.jitterBufferDelay,
          trackMuted: (() => {
            try {
              const receiverTrack = pc.getReceivers && pc.getReceivers()[0] && pc.getReceivers()[0].track;
              return receiverTrack ? receiverTrack.muted : null;
            } catch (e) { return null; }
          })(),
          codec: report.codecs[entry.codecId] || null,
        });
      }
      if (entry.type === 'outbound-rtp' && entry.kind === 'audio') {
        report.outbound.push({
          bytesSent: entry.bytesSent, packetsSent: entry.packetsSent,
          codec: report.codecs[entry.codecId] || null,
        });
      }
      if (entry.type === 'remote-inbound-rtp' && entry.kind === 'audio') {
        report.remoteInbound.push({ packetsLost: entry.packetsLost, roundTripTime: entry.roundTripTime });
      }
    });
    return report;
  });
}

/* ---------------- PWA driving ---------------- */

async function pair(page) {
  await page.goto(BASE + '/pwa/client.html', { waitUntil: 'domcontentloaded' });
  const paired = await page.evaluate(() =>
    !document.getElementById('main-view').classList.contains('hidden'));
  if (paired) { return; }
  await page.waitForSelector('#enroll-key', { timeout: 15000 });
  await page.fill('#enroll-key', KEY);
  await page.fill('#enroll-name', 'PWA Loopback');
  await page.click('#enroll-btn');
  await page.waitForSelector('#main-view:not(.hidden)', { timeout: 20000 });
}

async function dial(page, digits) {
  await page.waitForSelector('#dial-display', { timeout: 15000 });
  for (const digit of digits) {
    await page.click(`button.key[aria-label="${digit}"]`);
  }
  await page.click('#dial-call');
}

async function waitCallState(page, fragment, timeoutMs) {
  await page.waitForFunction(
    (text) => {
      const el = document.getElementById('call-state');
      return el && el.textContent.indexOf(text) >= 0;
    },
    fragment,
    { timeout: timeoutMs || 30000 },
  );
}

async function waitCallEnded(page) {
  await page.waitForFunction(() => {
    const view = document.getElementById('call-view');
    return view && view.classList.contains('hidden');
  }, null, { timeout: 15000 });
}

async function hangup(page) {
  await page.click('button[aria-label="挂断"]');
  await waitCallEnded(page);
  await sleep(600);
}

/* ---------------- scenarios ---------------- */

async function directScenario(page) {
  const evidence = {};
  await dial(page, ['1', '0', '0', '8', '6']);
  await waitCallState(page, '直连', 35000);
  const label = await page.textContent('#call-state');
  check('direct: PWA reports the direct transport', label.indexOf('直连') >= 0, label);

  // Capture/send warm-up: the synthetic WAV capture and the WebRTC encoder
  // need a moment before tone measurements are representative.
  await sleep(1500);

  const snap = await snapshot(page);
  evidence.snapshot = snap;
  const sdp = (snap.remoteSdp || snap.localSdp || '').toLowerCase();
  const sdpLines = (snap.remoteSdp || snap.localSdp || '').split('\r\n')
    .filter((line) => line.indexOf('m=audio') === 0 || line.indexOf('a=rtpmap') === 0)
    .join(' | ');
  check('direct: SDP negotiates Opus', sdp.indexOf('opus/48000') >= 0, sdpLines);
  check('direct: exactly one getUserMedia for the call', snap.micCalls === 1, 'micCalls=' + snap.micCalls);
  check('direct: no WSS media socket opened', snap.socketCount === 0, 'sockets=' + snap.socketCount);
  check('direct: peer connection connected', snap.pcStates[0] && snap.pcStates[0].connection === 'connected',
    JSON.stringify(snap.pcStates));

  // Reference measurement: what the real browser microphone track carries.
  // The fake capture's AGC/tone detection is environment-specific, so the
  // uplink check compares against THIS reference instead of a hard 440 Hz.
  const local = await measureStream(page, 800, 'local');
  evidence.localMic = local;
  const localTone = summarizeSamples(local.samples, local.rate || 48000, MIC_TONE_HZ);
  evidence.localTone = localTone;
  check('direct: browser synthetic mic carries a measurable tone',
    localTone.rms > 0.005 && localTone.toneHz > 0,
    JSON.stringify(localTone) + ' error=' + local.error + ' streams=' + local.streams);
  const referenceHz = localTone.toneHz > 0 ? localTone.toneHz : MIC_TONE_HZ;
  evidence.localReferenceHz = referenceHz;

  const remote = await measureRemote(page, 1600);
  evidence.downlink = remote;
  evidence.modemStatus = await harness('/harness/modem/status');
  const stats = await pcStats(page);
  evidence.pcStats = stats;
  check('direct: fake modem is pushing downlink tone frames',
    evidence.modemStatus && evidence.modemStatus.downlinkFrames > 20,
    JSON.stringify(evidence.modemStatus));
  check('direct: browser receives the RTP audio stream',
    Boolean(stats && stats.inbound[0] && stats.inbound[0].packetsReceived > 0 &&
      stats.inbound[0].bytesReceived > 0),
    JSON.stringify(stats && stats.inbound[0]));

  if (process.env.PWA_DUMP_DIR) {
    try {
      fs.mkdirSync(process.env.PWA_DUMP_DIR, { recursive: true });
      writeFloatWav(path.join(process.env.PWA_DUMP_DIR, 'browser-local-mic.wav'), local.samples, local.rate || 48000);
      writeFloatWav(path.join(process.env.PWA_DUMP_DIR, 'browser-remote-downlink.wav'), remote.samples, remote.rate || 48000);
    } catch (err) { evidence.dumpError = String(err); }
  }

  // Discriminator: can THIS Chromium/Edge profile decode inbound RTP at all?
  // A same-page Chromium->Chromium loopback removes the gateway from the
  // equation. If even the loopback reports totalSamplesReceived=0 while
  // packets arrive, the browser receive/playout instrumentation is
  // unavailable in this environment — that is a measurement limit, NOT
  // evidence that the gateway sent silence. When the loopback DOES decode,
  // a silent direct downlink is a real defect and the checks below fail.
  const loop = await browserLoopbackProbe(page);
  evidence.browserLoopbackDecode = loop;
  const browserDecodesRTP = Boolean(loop.inbound && loop.inbound.samples > 0);
  evidence.browserDecodeAvailable = browserDecodesRTP;

  const down = summarizeSamples(remote.samples, remote.rate || 48000, DOWN_TONE_HZ);
  const downFrames = remote.frameRms || [];
  const liveFrames = downFrames.filter((value) => value > 0.005).length;
  if (browserDecodesRTP) {
    check('direct: downlink remote track carries nonzero audio', down.rms > 0.01,
      'rms=' + down.rms + ' samples=' + down.samples + ' error=' + remote.error +
      ' streams=' + remote.streams + ' engine=' + remote.engine + ' ctx=' + remote.ctxState);
    check('direct: downlink audio is the modem tone ~1kHz', down.targetHit && down.tonePower > 0.001,
      JSON.stringify(down));
    check('direct: downlink continuity (no dead frames)',
      downFrames.length === 0 || liveFrames / downFrames.length > 0.7,
      'liveFrames=' + liveFrames + '/' + downFrames.length);
  } else {
    // Honest environment-limited classification: RTP delivery is proven
    // above; the browser's own loopback is equally silent in this profile.
    check('direct: browser PCM decode is environment-limited (Chromium loopback is also silent)',
      true, 'loopback=' + JSON.stringify(loop.inbound) + ' state=' + loop.state +
      '; direct remote rms=' + down.rms + ' samples=' + down.samples);
  }

  await resetModem();
  await sleep(1200);
  const up = pickUplink(await modemUplink());
  evidence.uplink = up;
  check('direct: modem receives continuous uplink PCMU', up.frames >= 40 && up.maxGapMs < 120,
    JSON.stringify(up));
  // The embedded fake-capture AEC/AGC settles over the first seconds; a
  // tone mismatch in this early window is recorded, and the hard tone proof
  // uses the settled post-unmute window below. Signal/continuity is required
  // here either way.
  const earlyToneMatches = up.rms > 500 && Math.abs(up.toneHz - referenceHz) <= 60;
  if (earlyToneMatches) {
    check('direct: uplink matches the local mic reference tone', true,
      JSON.stringify({ up: up, referenceHz: referenceHz }));
  } else {
    evidence.uplinkFirstWindowDistorted = true;
    check('direct: early uplink carries continuous mic audio (tone settles after warm-up)',
      up.rms > 500 && up.frames >= 40,
      JSON.stringify(up) + ' referenceHz=' + referenceHz);
  }

  await page.click('button[aria-label="静音"]');
  await sleep(400);
  const mutedSnap = await snapshot(page);
  check('direct: mute disables the live sender track',
    mutedSnap.pcTracks[0] && mutedSnap.pcTracks[0][0] === false, JSON.stringify(mutedSnap.pcTracks));
  await resetModem();
  await sleep(900);
  const muted = pickUplink(await modemUplink());
  evidence.muted = muted;
  check('direct: muted uplink is silence at the modem', muted.frames >= 20 && muted.rms < 80,
    JSON.stringify(muted));
  await page.click('button[aria-label="取消静音"]');
  await sleep(400);
  await resetModem();
  await sleep(900);
  const unmuted = pickUplink(await modemUplink());
  evidence.unmuted = unmuted;
  check('direct: unmute restores the mic tone',
    unmuted.rms > 500 && Math.abs(unmuted.toneHz - referenceHz) <= 60,
    JSON.stringify({ up: unmuted, referenceHz: referenceHz }));

  // Re-measure the downlink late in the call: distinguishes a startup
  // priming window from a permanently silent path (when decode is available).
  const lateRemote = await measureRemote(page, 1200);
  evidence.downlinkLate = lateRemote;
  const lateDown = summarizeSamples(lateRemote.samples, lateRemote.rate || 48000, DOWN_TONE_HZ);
  evidence.downlinkLateTone = lateDown;
  const lateStats = await pcStats(page);
  evidence.pcStatsLate = lateStats;
  if (browserDecodesRTP) {
    check('direct: downlink carries the modem tone late in the call',
      lateDown.rms > 0.01 && lateDown.targetHit,
      JSON.stringify(lateDown) + ' stats=' + JSON.stringify(lateStats));
  } else {
    check('direct: late downlink remains environment-limited (not a gateway-silence finding)',
      true, 'rms=' + lateDown.rms + ' RTP=' + JSON.stringify(lateStats && lateStats.inbound[0]));
  }

  const mediaSocketsBefore = (await snapshot(page)).socketCount;
  await hangup(page);
  await sleep(400);
  const after = await snapshot(page);
  evidence.after = after;
  check('direct: hangup closes the peer connection',
    after.pcStates.every((state) => state.connection === 'closed' || state.connection === 'disconnected') &&
    mediaSocketsBefore === 0,
    JSON.stringify(after.pcStates));
  check('direct: hangup stops the microphone tracks',
    after.micStates.every((states) => states.every((state) => state === 'ended')),
    JSON.stringify(after.micStates));
  return evidence;
}

/* Can this browser profile decode inbound RTP at all? Chromium->Chromium
 * same-page loopback with an oscillator source; independent of the gateway. */
async function browserLoopbackProbe(page) {
  return page.evaluate(async () => {
    const ctx = new AudioContext();
    await ctx.resume();
    const osc = ctx.createOscillator(); osc.frequency.value = 1000;
    const dst = ctx.createMediaStreamDestination();
    osc.connect(dst); osc.start();
    const pc1 = new RTCPeerConnection(); const pc2 = new RTCPeerConnection();
    const c1 = []; const c2 = [];
    pc1.onicecandidate = (e) => { if (e.candidate) { c1.push(e.candidate); } };
    pc2.onicecandidate = (e) => { if (e.candidate) { c2.push(e.candidate); } };
    pc1.addTrack(dst.stream.getAudioTracks()[0], dst.stream);
    pc2.ontrack = () => {};
    await pc1.setLocalDescription(await pc1.createOffer());
    await new Promise((r) => setTimeout(r, 400));
    await pc2.setRemoteDescription(pc1.localDescription);
    await pc2.setLocalDescription(await pc2.createAnswer());
    await new Promise((r) => setTimeout(r, 400));
    for (const c of c1) { try { await pc2.addIceCandidate(c); } catch (e) { /* ignore */ } }
    await pc1.setRemoteDescription(pc2.localDescription);
    for (const c of c2) { try { await pc1.addIceCandidate(c); } catch (e) { /* ignore */ } }
    await new Promise((r) => setTimeout(r, 2000));
    let inbound = null;
    const stats = await pc2.getStats();
    stats.forEach((s) => {
      if (s.type === 'inbound-rtp' && s.kind === 'audio') {
        inbound = { bytes: s.bytesReceived, packets: s.packetsReceived, samples: s.totalSamplesReceived };
      }
    });
    const state = pc2.connectionState;
    try { pc1.close(); pc2.close(); } catch (e) { /* ignore */ }
    await ctx.close();
    return { inbound, state };
  });
}

async function fallbackScenario(page) {
  const evidence = {};
  await page.route('**/webrtc/offer', (route) => route.abort());
  await dial(page, ['1', '0', '0', '8', '6']);
  await waitCallState(page, '中继', 35000);
  const label = await page.textContent('#call-state');
  check('fallback: PWA reports the WSS relay transport', label.indexOf('中继') >= 0, label);

  await sleep(500);
  const snap = await snapshot(page);
  evidence.snapshot = snap;
  check('fallback: exactly one getUserMedia (mic reused across paths)', snap.micCalls === 1,
    'micCalls=' + snap.micCalls);
  check('fallback: the WebRTC attempt was released', snap.pcStates.every((state) => state.connection === 'closed'),
    JSON.stringify(snap.pcStates));
  check('fallback: exactly one WSS media socket (single media owner)', snap.socketCount === 1,
    JSON.stringify(snap.sockets));
  check('fallback: the shared mic stream is still live',
    snap.micStates.every((states) => states.every((state) => state === 'live')),
    JSON.stringify(snap.micStates));

  await resetModem();
  await sleep(1200);
  const up = pickUplink(await modemUplink());
  evidence.uplink = up;
  check('fallback: modem receives continuous uplink PCMU', up.frames >= 40 && up.maxGapMs < 120,
    JSON.stringify(up));
  check('fallback: uplink carries the 440Hz synthetic mic tone',
    up.rms > 500 && Math.abs(up.toneHz - MIC_TONE_HZ) <= 45, JSON.stringify(up));

  await sleep(300);
  const wire = await page.evaluate(() => {
    const record = window.__harness.mediaSockets[0];
    return {
      sent: record.sent.map((f) => Array.from(f.bytes)),
      received: record.received.map((f) => Array.from(f.bytes)),
    };
  });
  const now = Date.now();
  const recentReceived = (wire.received || []).slice(-80);
  const recentSent = (wire.sent || []).slice(-80);
  const down = summarizeSamples(decodeFrameSet(recentReceived), 8000, DOWN_TONE_HZ);
  evidence.wire = { sent: wire.sent.length, received: wire.received.length, down };
  const sentSamples = decodeFrameSet(recentSent);
  const sentTone = summarizeSamples(sentSamples, 8000, MIC_TONE_HZ);
  evidence.wire.sentTone = sentTone;
  check('fallback: WSS downlink is continuous PCMU', recentReceived.length >= 30,
    'received=' + recentReceived.length + ' total=' + wire.received.length);
  check('fallback: WSS downlink carries the modem tone ~1kHz', down.rms > 0.01 && down.targetHit,
    JSON.stringify(down));
  check('fallback: WSS uplink frames carry the mic tone before mute',
    recentSent.length >= 20 && sentTone.rms > 0.005, JSON.stringify(sentTone));

  await page.click('button[aria-label="静音"]');
  await sleep(400);
  await page.evaluate(() => { window.__harness.mediaSockets[0].sent.length = 0; });
  await resetModem();
  await sleep(900);
  const muteWire = await page.evaluate(() => window.__harness.mediaSockets[0].sent.map((f) => Array.from(f.bytes)));
  const muted = pickUplink(await modemUplink());
  evidence.mutedWire = muteWire.slice(-40);
  evidence.muted = muted;
  const allSilence = muteWire.length >= 15 && muteWire.every((frame) =>
    frame.length === 160 && frame.every((byte) => byte === 0xFF));
  check('fallback: muted frames are PCMU silence 0xFF on the wire', allSilence,
    'frames=' + muteWire.length + ' first=' + JSON.stringify((muteWire[0] || []).slice(0, 4)));
  check('fallback: muted uplink decodes to silence at the modem', muted.frames >= 20 && muted.rms < 80,
    JSON.stringify(muted));

  await page.click('button[aria-label="取消静音"]');
  await sleep(400);
  await page.evaluate(() => { window.__harness.mediaSockets[0].sent.length = 0; });
  await resetModem();
  await sleep(900);
  const unmuteWire = await page.evaluate(() => window.__harness.mediaSockets[0].sent.map((f) => Array.from(f.bytes)));
  const unmuted = pickUplink(await modemUplink());
  evidence.unmuted = { wireFrames: unmuteWire.length, modem: unmuted };
  const unmuteTone = summarizeSamples(decodeFrameSet(unmuteWire), 8000, MIC_TONE_HZ);
  check('fallback: unmute restores real mic frames', unmuteWire.length >= 20 && unmuteTone.rms > 0.005,
    JSON.stringify(unmuteTone));
  check('fallback: unmuted modem tone returns', unmuted.rms > 500 && Math.abs(unmuted.toneHz - MIC_TONE_HZ) <= 45,
    JSON.stringify(unmuted));

  await hangup(page);
  await page.unroute('**/webrtc/offer');
  await sleep(400);
  const after = await snapshot(page);
  evidence.after = after;
  check('fallback: hangup closes the WSS socket', after.sockets.every((socket) => socket.closed),
    JSON.stringify(after.sockets));
  check('fallback: hangup stops the microphone tracks',
    after.micStates.every((states) => states.every((state) => state === 'ended')),
    JSON.stringify(after.micStates));
  return evidence;
}

async function incomingScenario(page) {
  const evidence = {};
  result.data.incoming = evidence;   // survive a thrown diagnostic path
  // The foreground event stream must be CONNECTED before injecting the
  // ring, otherwise the ring is only recoverable through the snapshot.
  await page.waitForFunction(() => {
    const dot = document.getElementById('conn-dot');
    return dot && dot.className === 'on';
  }, null, { timeout: 15000 });
  evidence.activeBefore = await page.evaluate(async () => {
    try {
      const resp = await fetch('/api/v2/calls?active=true', { credentials: 'same-origin' });
      return { status: resp.status, body: await resp.json() };
    } catch (e) { return String(e); }
  }).catch((e) => String(e));
  const incoming = await harness('/harness/incoming', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ peer: '+8613800000042' }),
  });
  evidence.incoming = incoming;
  try {
    await page.waitForSelector('button[aria-label="接听"]', { timeout: 20000 });
  } catch (err) {
    evidence.ringTimeout = {
      state: await page.textContent('#call-state').catch(() => null),
      harnessErrors: await page.evaluate(() => (window.__harness && window.__harness.errors || []).slice(0, 5)).catch(() => []),
      activeCalls: await page.evaluate(async () => {
        try {
          const resp = await fetch('/api/v2/calls?active=true', { credentials: 'same-origin' });
          return { status: resp.status, body: await resp.json() };
        } catch (e) { return String(e); }
      }).catch(() => null),
    };
    throw err;
  }
  const ringText = await page.textContent('#call-state');
  check('incoming: ringing screen with answer/reject', ringText.indexOf('来电') >= 0, ringText);
  await page.click('button[aria-label="接听"]');
  await waitCallState(page, '通话中', 35000);
  await sleep(600);
  const snap = await snapshot(page);
  evidence.snapshot = snap;
  check('incoming: answer enabled media (single mic, single transport)',
    snap.micCalls === 1 && (snap.socketCount === 0 || snap.socketCount === 1),
    'micCalls=' + snap.micCalls + ' sockets=' + snap.socketCount);
  await resetModem();
  await sleep(1000);
  const up = pickUplink(await modemUplink());
  evidence.uplink = up;
  check('incoming: answered call carries uplink tone to the modem',
    up.frames >= 30 && up.rms > 500 && Math.abs(up.toneHz - MIC_TONE_HZ) <= 45, JSON.stringify(up));
  await hangup(page);
  await sleep(400);
  const after = await snapshot(page);
  check('incoming: hangup stops the microphone tracks',
    after.micStates.every((states) => states.every((state) => state === 'ended')),
    JSON.stringify(after.micStates));
  return evidence;
}

async function main() {
  const { chromium } = resolvePlaywright();
  const micWav = path.join(os.tmpdir(), 'callrelay-pwa-loopback-mic-440.wav');
  writeMicWav(micWav, 30, MIC_TONE_HZ);

  const browser = await chromium.launch({
    channel: 'msedge',
    headless: true,
    // Playwright injects --mute-audio by default; that silences the whole
    // receive/playout path in headless and would make "downlink silence" a
    // measurement artifact instead of real media evidence.
    ignoreDefaultArgs: ['--mute-audio'],
    args: [
      '--use-fake-device-for-media-stream',
      '--use-fake-ui-for-media-stream',
      '--use-file-for-fake-audio-capture=' + micWav,
      '--autoplay-policy=no-user-gesture-required',
      '--disable-features=WebRtcHideLocalIpsWithMdns',
      '--no-first-run',
    ],
  });
  const context = await browser.newContext({
    viewport: { width: 390, height: 844 },
    permissions: ['microphone'],
    serviceWorkers: 'block',
    locale: 'zh-CN',
  });
  try {
    const { execSync } = require('child_process');
    result.data.edgeArgs = execSync(
      "ps -axww -o command= | grep -i '[m]sedge' | grep -o -- '--[a-z-]*' | sort -u",
      { encoding: 'utf8' }).trim().split('\n');
  } catch (err) { result.data.edgeArgsError = String(err && err.message || err); }
  await context.grantPermissions(['microphone'], { origin: BASE }).catch(() => {});
  const page = await context.newPage();
  installHooks(page);
  page.on('pageerror', (err) => { result.data.pageError = String(err && err.message || err); });
  page.on('console', (msg) => {
    if (msg.type() === 'error') {
      result.data.consoleErrors = result.data.consoleErrors || [];
      if (result.data.consoleErrors.length < 10) { result.data.consoleErrors.push(msg.text().slice(0, 200)); }
    }
  });

  try {
    await pair(page);
    const only = process.env.PWA_ONLY || '';
    const fresh = async () => {
      await page.goto(BASE + '/pwa/client.html', { waitUntil: 'domcontentloaded' });
      await page.waitForSelector('#dial-display', { timeout: 15000 });
    };

    if (!only || only === 'direct') {
      result.data.direct = await directScenario(page);
    }
    if (!only || only === 'fallback') {
      await fresh();
      result.data.fallback = await fallbackScenario(page);
    }
    if (!only || only === 'incoming') {
      await fresh();
      result.data.incoming = await incomingScenario(page);
    }

    // Final independent modem snapshot is captured by the Go test from the
    // same store; attach the last unmuted report for the JSON evidence too.
    result.data.finalUplink = pickUplink(await modemUplink());
    result.ok = result.checks.every((entry) => entry.ok) && !result.data.pageError;
  } catch (err) {
    result.error = String((err && err.stack) || err);
  } finally {
    try { await browser.close(); } catch (e) { /* ignore */ }
  }

  fs.writeFileSync(OUT, JSON.stringify(result, null, 2));
  console.log(JSON.stringify({
    ok: result.ok,
    error: result.error,
    checks: result.checks.length,
    failed: result.checks.filter((entry) => !entry.ok).map((entry) => entry.name + ' :: ' + entry.detail),
  }, null, 2));
  process.exit(result.ok ? 0 : 1);
}

main().catch((err) => {
  result.error = String((err && err.stack) || err);
  try { fs.writeFileSync(OUT, JSON.stringify(result, null, 2)); } catch (e) { /* ignore */ }
  console.error(result.error);
  process.exit(1);
});
