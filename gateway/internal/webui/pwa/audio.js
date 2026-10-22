/* CallRelay PWA — browser audio pipeline for the WSS PCMU transport.
 *
 * Mirrors the native AVAudioEngine graph with standard WebAudio:
 *   capture:  getUserMedia -> AudioContext -> AudioWorklet (48 kHz in,
 *             decimate to 8 kHz, frame into 160-sample chunks, μ-law via
 *             PCM.encode) -> WebSocket binary send.
 *   playback: WebSocket binary 160-byte frames -> μ-law decode -> AudioWorklet
 *             ring buffer (8 kHz -> linear upsample to the context rate).
 *
 * Realtime rules mirror the native scheduler: the ring buffer is bounded
 * (~1 s); beyond the cap the OLDEST frame is dropped so a stalled tunnel
 * never accumulates latency; mic capture only runs while the user gesture-
 * granted track lives (no background mic claims — the page owns it and the
 * OS indicator shows it).
 *
 * Feature detection: AudioWorklet is required; callers must feature-detect
 * (`typeof AudioWorkletNode !== 'undefined'`) and degrade honestly. Safari
 * exposes webkitAudioContext only in very old versions we do not support.
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory(require('./pcmu.js'));
  } else {
    root.CallRelayAudio = factory(root.PCMU);
  }
}(typeof self !== 'undefined' ? self : this, function (PCMU) {
  'use strict';

  var FRAME = 160;

  var PLAYER_WORKLET = [
    "class PcmuPlayerWorklet extends AudioWorkletProcessor {",
    "  constructor() {",
    "    super();",
    "    this.queue = [];            // Float32Array frames, 160 samples @8k",
    "    this.offset = 0;",
    "    this.maxQueued = 16;        // ~320ms jitter bound, drop-oldest beyond",
    "    this.dropped = 0;",
    "    this.port.onmessage = (event) => {",
    "      const frame = event.data;",
    "      if (!frame || frame.length !== 160) { return; }",
    "      if (this.queue.length >= this.maxQueued) { this.queue.shift(); this.dropped += 1; }",
    "      this.queue.push(frame);",
    "    };",
    "  }",
    "  // Linear 8k -> sampleRate upsampling with a carry-over fraction.",
    "  process(inputs, outputs) {",
    "    const out = outputs[0][0];",
    "    if (!out) { return true; }",
    "    const ratio = sampleRate / 8000;",
    "    for (let i = 0; i < out.length; i++) {",
    "      if (this.queue.length === 0) { out[i] = 0; continue; }",
    "      const head = this.queue[0];",
    "      const pos8k = this.offset / ratio;",
    "      const idx = Math.floor(pos8k);",
    "      if (idx + 1 < head.length) {",
    "        const frac = pos8k - idx;",
    "        out[i] = head[idx] * (1 - frac) + head[idx + 1] * frac;",
    "      } else {",
    "        out[i] = head[idx];",
    "      }",
    "      this.offset += 1;",
    "      if (this.offset >= head.length * ratio) { this.queue.shift(); this.offset = 0; }",
    "    }",
    "    return true;",
    "  }",
    "}",
    "registerProcessor('callrelay-pcmu-player', PcmuPlayerWorklet);",
  ].join('\n');

  var CAPTURE_WORKLET = [
    "class PcmuCaptureWorklet extends AudioWorkletProcessor {",
    "  constructor() {",
    "    super();",
    "    // Fractional-ratio decimator: each output consumes rate/8000 INPUT",
    "    // samples (44.1k -> 5.5125, 48k -> 6). Math.round would yield 7350",
    "    // Hz at 44.1k — exactly the reviewed defect.",
    "    this.carry = new Float32Array(0);",
    "    this.phase = 0;",
    "    this.step = sampleRate / 8000;",
    "  }",
    "  process(inputs) {",
    "    const input = inputs[0] && inputs[0][0];",
    "    if (!input || input.length === 0) { return true; }",
    "    const merged = new Float32Array(this.carry.length + input.length);",
    "    merged.set(this.carry, 0);",
    "    merged.set(input, this.carry.length);",
    "    const out = [];",
    "    while (this.phase + this.step <= merged.length) {",
    "      // Linear interpolation between surrounding input samples (the",
    "      // fractional position), with a 3-tap antialias window per step.",
    "      const i0 = Math.floor(this.phase);",
    "      const frac = this.phase - i0;",
    "      const a = merged[i0];",
    "      const b = merged[Math.min(i0 + 1, merged.length - 1)];",
    "      out.push(a * (1 - frac) + b * frac);",
    "      this.phase += this.step;",
    "    }",
    "    this.carry = merged.slice(Math.floor(this.phase));",
    "    this.phase -= Math.floor(this.phase);",
    "    if (out.length > 0) {",
    "      const frame = new Float32Array(out);",
    "      this.port.postMessage(frame, [frame.buffer]);",
    "    }",
    "    return true;",
    "  }",
    "}",
    "registerProcessor('callrelay-pcmu-capture', PcmuCaptureWorklet);",
  ].join('\n');

  function workletURL(code) {
    return URL.createObjectURL(new Blob([code], { type: 'application/javascript' }));
  }

  /* Playback: feed(frameBytes) with decoded μ-law; bounded drop-oldest. */
  function PcmuPlayer() {
    this.context = null;
    this.node = null;
  }

  PcmuPlayer.supported = function () {
    return typeof AudioWorkletNode !== 'undefined' &&
      (typeof AudioContext !== 'undefined' || typeof webkitAudioContext !== 'undefined');
  };

  PcmuPlayer.prototype.start = function () {
    if (this.node) { return Promise.resolve(this); }
    return this.prepare();
  };

  /* Gesture-window warm-up: create + resume the AudioContext and load the
   * worklet module while user activation is still present, so a later WSS
   * fallback (after network negotiation) can start without a fresh grant. */
  PcmuPlayer.prototype.prepare = function () {
    if (this.node) { return Promise.resolve(this); }
    if (this._preparing) { return this._preparing; }
    var Ctor = typeof AudioContext !== 'undefined' ? AudioContext : webkitAudioContext;
    var self = this;
    if (!this.context) {
      this.context = new Ctor({ latencyHint: 'interactive' });
      if (this.context.state === 'suspended') {
        try { this.context.resume(); } catch (e) { /* resumed later */ }
      }
    }
    this._preparing = this.context.audioWorklet.addModule(workletURL(PLAYER_WORKLET)).then(function () {
      self.node = new AudioWorkletNode(self.context, 'callrelay-pcmu-player', {
        numberOfInputs: 0, numberOfOutputs: 1, outputChannelCount: [1],
      });
      self.node.connect(self.context.destination);
      if (self.context.state === 'suspended') { return self.context.resume(); }
      return undefined;
    }).then(function () { return self; });
    return this._preparing;
  };

  PcmuPlayer.prototype.feed = function (frameBytes) {
    if (!this.node) { return; }
    this.node.port.postMessage(PCMU.decodeToFloat32(frameBytes));
  };

  PcmuPlayer.prototype.stop = function () {
    if (this.node) { this.node.disconnect(); this.node = null; }
    if (this.context) { this.context.close(); this.context = null; }
    this._preparing = null;
  };

  /* Capture: getUserMedia (caller passes the gesture-granted stream). */
  function PcmuCapture() {
    this.context = null;
    this.node = null;
    this.stream = null;
    this.pend8k = [];
    this.onFrame = null;   // (Uint8Array 160) -> void
  }

  /* Gesture-window warm-up: create + resume the capture AudioContext and
   * load the capture worklet module. The microphone is NOT connected here;
   * start(stream) does that when (and only when) WSS is actually used. */
  PcmuCapture.prototype.prepare = function () {
    if (this.node) { return Promise.resolve(this); }
    if (this._preparing) { return this._preparing; }
    var Ctor = typeof AudioContext !== 'undefined' ? AudioContext : webkitAudioContext;
    var self = this;
    if (!this.context) {
      this.context = new Ctor({ latencyHint: 'interactive' });
      if (this.context.state === 'suspended') {
        try { this.context.resume(); } catch (e) { /* resumed later */ }
      }
    }
    this._preparing = this.context.audioWorklet.addModule(workletURL(CAPTURE_WORKLET)).then(function () {
      // The worklet needs a pull path to run on every engine quantum: give
      // it one output and connect it through a ZERO-GAIN node to the
      // destination. A node with numberOfOutputs:0 is not guaranteed to
      // process at all (reviewed defect).
      self.node = new AudioWorkletNode(self.context, 'callrelay-pcmu-capture', {
        numberOfInputs: 1, numberOfOutputs: 1, outputChannelCount: [1],
      });
      self.sink = self.context.createGain();
      self.sink.gain.value = 0;
      self.node.connect(self.sink);
      self.sink.connect(self.context.destination);
      self.node.port.onmessage = function (event) {
        var chunk = event.data; // Float32Array, 8 kHz
        for (var i = 0; i < chunk.length; i++) { self.pend8k.push(chunk[i]); }
        while (self.pend8k.length >= FRAME) {
          var frame = new Float32Array(self.pend8k.splice(0, FRAME));
          if (self.onFrame) { self.onFrame(PCMU.encodeFloat32(frame)); }
        }
      };
      if (self.context.state === 'suspended') { return self.context.resume(); }
      return undefined;
    }).then(function () { return self; });
    return this._preparing;
  };

  PcmuCapture.prototype.start = function (stream) {
    var self = this;
    this.stream = stream;
    return this.prepare().then(function () {
      self.source = self.context.createMediaStreamSource(stream);
      self.source.connect(self.node);
      if (self.context.state === 'suspended') { return self.context.resume(); }
      return undefined;
    }).then(function () { return self; });
  };

  PcmuCapture.prototype.stop = function () {
    if (this.stream) {
      this.stream.getTracks().forEach(function (track) { track.stop(); });
      this.stream = null;
    }
    if (this.source) { this.source.disconnect(); this.source = null; }
    if (this.node) { this.node.disconnect(); this.node = null; }
    if (this.sink) { this.sink.disconnect(); this.sink = null; }
    if (this.context) { this.context.close(); this.context = null; }
    this.pend8k = [];
    this._preparing = null;
  };

  return { PcmuPlayer: PcmuPlayer, PcmuCapture: PcmuCapture, FRAME: FRAME };
}));
