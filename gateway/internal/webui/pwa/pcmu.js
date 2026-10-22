/* CallRelay PWA — G.711 μ-law codec.
 *
 * This is a character-for-character port of the gateway/iOS PCMUCodec
 * (gateway: internal/sms/../voice + iOS WebRTC/PCMUCodec.swift): identical
 * bias, segment table and sign handling, so a frame encoded in the browser
 * decodes to the same PCM on every other endpoint and vice versa. Pure
 * functions only — no DOM, no audio.
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.PCMU = factory();
  }
}(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  var SEGMENT_ENDPOINTS = [0, 256, 512, 1024, 2048, 4096, 8192, 16384];

  function clamp16(value) {
    value = value | 0;
    return value < -32768 ? -32768 : value > 32767 ? 32767 : value;
  }

  /* 160 int16 samples -> 160 μ-law bytes (same algorithm as the gateway). */
  function encode(samples) {
    var out = new Uint8Array(samples.length);
    for (var i = 0; i < samples.length; i++) {
      var value = clamp16(samples[i]);
      var sign = 0;
      if (value < 0) { sign = 0x80; value = -value; }
      if (value > 32635) { value = 32635; }
      value += 132;
      var segment = 0;
      while (segment < SEGMENT_ENDPOINTS.length - 1 && value >= SEGMENT_ENDPOINTS[segment + 1]) {
        segment += 1;
      }
      var mantissa = (value >> (segment + 3)) & 0x0F;
      out[i] = ~(sign | (segment << 4) | mantissa) & 0xFF;
    }
    return out;
  }

  /* 160 μ-law bytes -> 160 int16 samples. Any byte pattern decodes (G.711
   * is total), so a corrupted frame yields noise, never an exception. */
  function decode(bytes) {
    var out = new Int16Array(bytes.length);
    for (var i = 0; i < bytes.length; i++) {
      var value = (~bytes[i]) & 0xFF;
      var sign = value & 0x80;
      var segment = (value >> 4) & 0x07;
      var mantissa = value & 0x0F;
      var sample = ((mantissa << 3) + 132) << segment;
      out[i] = sign !== 0 ? (132 - sample) : (sample - 132);
    }
    return out;
  }

  /* Float32 [-1,1] -> μ-law bytes (audio capture path). */
  function encodeFloat32(floatSamples) {
    var pcm = new Int16Array(floatSamples.length);
    for (var i = 0; i < floatSamples.length; i++) {
      var v = floatSamples[i];
      if (v > 1) { v = 1; } else if (v < -1) { v = -1; }
      pcm[i] = (v * 32767) | 0;
    }
    return encode(pcm);
  }

  /* μ-law bytes -> Float32 [-1,1] (audio playback path). */
  function decodeToFloat32(bytes) {
    var pcm = decode(bytes);
    var out = new Float32Array(pcm.length);
    for (var i = 0; i < pcm.length; i++) {
      out[i] = pcm[i] / 32768;
    }
    return out;
  }

  return { encode: encode, decode: decode, encodeFloat32: encodeFloat32, decodeToFloat32: decodeToFloat32 };
}));
