#!/usr/bin/env node
/* Capture-resampler determinism test: the AudioWorklet source in audio.js
 * must deliver EXACTLY 8000 samples per second of input at ANY context rate
 * (the reviewed Math.round defect yielded 7350 Hz at 44.1 kHz). The worklet
 * body is extracted and executed against fake worklet globals; a 1-second
 * 440 Hz sine at 44.1 kHz and 48 kHz each must produce 8000 samples with
 * phase-coherent reconstruction.
 */
'use strict';

const fs = require('fs');
const path = require('path');

let failures = 0;
function check(name, cond, detail) {
  if (cond) { console.log('ok   - ' + name); }
  else { failures += 1; console.error('FAIL - ' + name + (detail ? ' :: ' + detail : '')); }
}

const audioSrc = fs.readFileSync(path.join(__dirname, '..', 'audio.js'), 'utf8');
const match = audioSrc.match(/var CAPTURE_WORKLET = \[([\s\S]*?)\]\.join\('\\n'\);/);
if (!match) { console.error('FAIL - could not extract CAPTURE_WORKLET'); process.exit(1); }
// Rebuild the worklet JS from the string-array source.
const arraySrc = '[' + match[1] + ']';
const lines = eval(arraySrc);
const workletCode = lines.join('\n');

function runWorklet(sampleRate, inputChunks) {
  const posted = [];
  class FakeProcessor {
    constructor() { this.port = { postMessage: (m) => posted.push(m) }; }
  }
  const registry = {};
  const sandbox = {
    AudioWorkletProcessor: FakeProcessor,
    registerProcessor: (name, cls) => { registry[name] = cls; },
    sampleRate,
  };
  const fn = new Function('AudioWorkletProcessor', 'registerProcessor', 'sampleRate', workletCode);
  fn(sandbox.AudioWorkletProcessor, sandbox.registerProcessor, sandbox.sampleRate);
  const proc = new registry['callrelay-pcmu-capture']();
  inputChunks.forEach((chunk) => {
    proc.process([[chunk]], [], []);
  });
  return posted;
}

/* 1 s of 440 Hz sine at the given rate, in 100 ms chunks. */
function sineChunks(sampleRate) {
  const chunks = [];
  const chunkLen = Math.floor(sampleRate / 10);
  for (let c = 0; c < 10; c += 1) {
    const chunk = new Float32Array(chunkLen);
    for (let i = 0; i < chunkLen; i += 1) {
      chunk[i] = Math.sin(2 * Math.PI * 440 * (c * chunkLen + i) / sampleRate) * 0.5;
    }
    chunks.push(chunk);
  }
  return chunks;
}

for (const rate of [44100, 48000]) {
  const posted = runWorklet(rate, sineChunks(rate));
  const total = posted.reduce((n, f) => n + f.length, 0);
  check(`${rate} Hz: 1 s of input yields exactly 8000 samples`, total === 8000,
    'got ' + total);
  // Phase continuity: concatenate and measure the dominant period of the
  // reconstructed 440 Hz tone (8000/440 ≈ 18.18 samples/cycle).
  const out = new Float32Array(total);
  let at = 0;
  posted.forEach((f) => { out.set(f, at); at += f.length; });
  // Zero-crossing rate over the middle second half.
  let crossings = 0;
  for (let i = 4000; i < 7900; i += 1) {
    if ((out[i] >= 0) !== (out[i + 1] >= 0)) { crossings += 1; }
  }
  const measuredHz = crossings / 2 / ((7900 - 4000) / 8000);
  check(`${rate} Hz: reconstructed tone ≈ 440 Hz (±15 Hz)`, Math.abs(measuredHz - 440) < 15,
    'measured ' + measuredHz.toFixed(1) + ' Hz');
}

/* Carry-over across chunk boundaries: 44100 Hz, two tiny chunks produce a
 * contiguous stream (no sample duplicated or dropped at the seam). */
{
  const rate = 44100;
  const chunkLen = Math.floor(rate / 10);
  const chunks = [
    new Float32Array(chunkLen).fill(0.25),
    new Float32Array(chunkLen).fill(0.25),
  ];
  const posted = runWorklet(rate, chunks);
  const total = posted.reduce((n, f) => n + f.length, 0);
  check('44.1 kHz: 200 ms of input yields exactly 1600 samples', total === 1600,
    'got ' + total);
  const values = [];
  posted.forEach((f) => f.forEach((v) => values.push(v)));
  check('carry-over keeps constant input constant (no seam glitch)',
    values.every((v) => Math.abs(v - 0.25) < 0.02),
    'min=' + Math.min(...values) + ' max=' + Math.max(...values));
}

console.log(failures === 0 ? 'ALL PASS' : failures + ' FAILURES');
process.exit(failures === 0 ? 0 : 1);
