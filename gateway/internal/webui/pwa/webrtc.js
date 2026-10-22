/* CallRelay PWA — browser-native WebRTC direct media path.
 *
 * Reuses the gateway's EXISTING direct path (the same endpoints the native
 * app uses): GET /api/v2/calls/{id}/ice returns the ICE servers, and
 * POST /api/v2/calls/{id}/webrtc/offer attaches this browser as the call's
 * host transport with a standard SDP offer/answer. Audio is then real
 * bidirectional Opus handled by the browser's own WebRTC stack — no custom
 * media engine. The WSS PCMU transport remains the fallback (cellular /
 * no reachable ICE candidate / browser without WebRTC).
 *
 * Gesture contract: `prepare()` must run inside the user gesture (click).
 * It resumes the AudioContext and acquires the microphone; the remote track
 * is later routed through that already-resumed context, which avoids the
 * iOS autoplay restrictions on <audio> elements.
 *
 * All natives are injectable so the flow is unit-testable without a browser.
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.CallRelayWebRTC = factory();
  }
}(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  var READY_TIMEOUT_MS = 10000;
  var ICE_GATHER_TIMEOUT_MS = 3000;

  function enc(value) { return encodeURIComponent(value); }

  function defaultMic() {
    return navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true, channelCount: 1 },
    });
  }

  function stopStream(stream) {
    if (stream && stream.getTracks) {
      stream.getTracks().forEach(function (track) { track.stop(); });
    }
  }

  function WebRTCMedia(opts) {
    opts = opts || {};
    this.api = opts.api;                       // (method, path, body?) -> Promise<json>
    this.callId = String(opts.callId || '');
    this.getMic = opts.getMic || defaultMic;
    this.PeerConnection = opts.PeerConnection ||
      (typeof RTCPeerConnection !== 'undefined' ? RTCPeerConnection : null);
    this.AudioContextCtor = opts.AudioContext ||
      (typeof AudioContext !== 'undefined' ? AudioContext :
        (typeof webkitAudioContext !== 'undefined' ? webkitAudioContext : null));
    this.setTimeout = opts.setTimeout || function (fn, ms) { return setTimeout(fn, ms); };
    this.clearTimeout = opts.clearTimeout || function (t) { clearTimeout(t); };
    this.readyTimeoutMs = opts.readyTimeoutMs || READY_TIMEOUT_MS;
    this.iceGatherTimeoutMs = opts.iceGatherTimeoutMs || ICE_GATHER_TIMEOUT_MS;

    this.pc = null;
    this.stream = null;
    this.ctx = null;
    this.remoteNode = null;
    this.muted = false;
    this.closed = false;
    // aborted: the transport was abandoned (timeout/superseded) while the
    // gesture-granted microphone stream must stay alive for the WSS fallback.
    this.aborted = false;
    this.prepared = null;
  }

  WebRTCMedia.supported = function () {
    return typeof RTCPeerConnection !== 'undefined';
  };

  /* Gesture-time preparation. No peer connection yet. */
  WebRTCMedia.prototype.prepare = function () {
    var self = this;
    if (this.prepared) { return this.prepared; }
    if (this.closed || this.aborted) {
      return Promise.reject(new Error('此直连已取消'));
    }
    this.prepared = Promise.resolve().then(function () {
      if (self.closed || self.aborted) { throw new Error('此直连已取消'); }
      if (!self.AudioContextCtor) { throw new Error('此浏览器不支持网页音频'); }
      self.ctx = new self.AudioContextCtor({ latencyHint: 'interactive' });
      return self.ctx.state === 'suspended' ? self.ctx.resume() : undefined;
    }).then(function () {
      if (self.closed || self.aborted) { throw new Error('此直连已取消'); }
      return self.getMic();
    }).then(function (stream) {
      if (self.closed || self.aborted) { stopStream(stream); throw new Error('此直连已取消'); }
      self.stream = stream;
      return self;
    }).catch(function (err) {
      // A failed PREPARATION owns nothing reusable; release everything
      // including any stream that arrived.
      self.stop();
      throw err;
    });
    return this.prepared;
  };

  WebRTCMedia.prototype.setCallId = function (callId) {
    this.callId = String(callId || '');
  };

  /* Full attach: ICE config -> offer -> gateway answer -> connected.
   *
   * A failure here is recoverable: the peer connection/context are dropped
   * but the gesture-granted microphone stream is KEPT so the WSS fallback
   * can reuse it (one getUserMedia per call). `abort()`/`release()` stop
   * any in-flight negotiation from later touching a flow that has already
   * fallen back. */
  WebRTCMedia.prototype.start = function () {
    var self = this;
    if (!this.PeerConnection) {
      return Promise.reject(new Error('此浏览器不支持 WebRTC'));
    }
    return this.prepare().then(function () {
      if (self.closed || self.aborted) { throw new Error('此直连已取消'); }
      return self.api('GET', '/api/v2/calls/' + enc(self.callId) + '/ice').catch(function () {
        return { iceServers: [] };
      });
    }).then(function (ice) {
      if (self.closed || self.aborted) { throw new Error('此直连已取消'); }
      var servers = (ice && ice.iceServers) || [];
      var pc = new self.PeerConnection({ iceServers: servers, bundlePolicy: 'max-bundle' });
      self.pc = pc;
      pc.ontrack = function (event) { self.attachRemote(event); };
      self.stream.getTracks().forEach(function (track) { pc.addTrack(track, self.stream); });
      // Build-49 mid-call drop detection: after start() settles, a terminal
      // connection state is a media drop (the flow recovers via the WSS
      // transport inside the same 45 s window). Fenced so abort/release or
      // a pre-settle failure can never fire it, and it fires at most once.
      self.disconnectFired = false;
      var onTerminal = function () {
        var state = pc.connectionState;
        var terminal = state === 'failed' || state === 'closed';
        if (!terminal || self.closed || self.aborted || self.disconnectFired) { return; }
        self.disconnectFired = true;
        if (self.onDisconnect) { self.onDisconnect(); }
      };
      if (pc.addEventListener) { pc.addEventListener('connectionstatechange', onTerminal); }
      else { pc.onconnectionstatechange = onTerminal; }
      return self.negotiate(pc);
    }).catch(function (err) {
      // Keep the microphone: the caller falls back to WSS with the SAME
      // gesture-granted stream. Only the WebRTC transport is released.
      if (!self.closed) {
        self.aborted = true;
        self.releaseTransport();
        self.prepared = null;
      }
      throw err;
    });
  };

  WebRTCMedia.prototype.negotiate = function (pc) {
    var self = this;
    return pc.createOffer({ offerToReceiveAudio: true }).then(function (offer) {
      if (self.closed || self.aborted) { throw new Error('此直连已取消'); }
      return pc.setLocalDescription(offer);
    }).then(function () {
      return self.waitIceGathered(pc);
    }).then(function () {
      if (self.closed || self.aborted) { throw new Error('此直连已取消'); }
      return self.api('POST', '/api/v2/calls/' + enc(self.callId) + '/webrtc/offer', {
        sdp: pc.localDescription.sdp,
        type: 'offer',
        transport: 'ice',
      });
    }).then(function (answer) {
      if (self.closed || self.aborted) { throw new Error('此直连已取消'); }
      if (!answer || !answer.sdp) { throw new Error('网关未返回媒体应答'); }
      return pc.setRemoteDescription({ type: answer.type || 'answer', sdp: answer.sdp });
    }).then(function () {
      if (self.closed || self.aborted) { throw new Error('此直连已取消'); }
      return self.waitConnected(pc);
    });
  };

  /* ICE gathering is best effort: a short bounded wait collects host/srflx
   * candidates; if it never completes the offer still goes out with whatever
   * was gathered (the gateway can generate a peer-reflexive candidate). */
  WebRTCMedia.prototype.waitIceGathered = function (pc) {
    var self = this;
    if (pc.iceGatheringState === 'complete') { return Promise.resolve(); }
    return new Promise(function (resolve) {
      var done = false;
      var finish = function () {
        if (done) { return; }
        done = true;
        if (timer) { self.clearTimeout(timer); }
        pc.removeEventListener('icegatheringstatechange', onChange);
        resolve();
      };
      var onChange = function () {
        if (pc.iceGatheringState === 'complete') { finish(); }
      };
      var timer = self.setTimeout(finish, self.iceGatherTimeoutMs);
      if (pc.addEventListener) { pc.addEventListener('icegatheringstatechange', onChange); }
      else { pc.onicegatheringstatechange = onChange; }
      if (pc.iceGatheringState === 'complete') { finish(); }
    });
  };

  WebRTCMedia.prototype.waitConnected = function (pc) {
    var self = this;
    return new Promise(function (resolve, reject) {
      var settled = false;
      var finish = function (err) {
        if (settled) { return; }
        settled = true;
        if (timer) { self.clearTimeout(timer); }
        if (pc.removeEventListener) { pc.removeEventListener('connectionstatechange', onChange); }
        if (err) { reject(err); } else { resolve(self); }
      };
      var onChange = function () {
        if (pc.connectionState === 'connected' || pc.connectionState === 'completed') { finish(null); }
        if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
          finish(new Error('直连建立失败'));
        }
      };
      var timer = self.setTimeout(function () { finish(new Error('直连超时')); }, self.readyTimeoutMs);
      if (pc.addEventListener) { pc.addEventListener('connectionstatechange', onChange); }
      else { pc.onconnectionstatechange = onChange; }
      onChange();
    });
  };

  /* Remote audio: routed through the gesture-resumed AudioContext. */
  WebRTCMedia.prototype.attachRemote = function (event) {
    var stream = (event.streams && event.streams[0]) || null;
    if (!stream && event.track) {
      if (!this._remoteStream) { this._remoteStream = new MediaStream(); }
      this._remoteStream.addTrack(event.track);
      stream = this._remoteStream;
    }
    if (!stream || !this.ctx) { return; }
    try {
      this.remoteNode = this.ctx.createMediaStreamSource(stream);
      this.remoteNode.connect(this.ctx.destination);
    } catch (err) {
      // A failed playback route still leaves the connection usable; the
      // caller shows the media state truthfully.
    }
  };

  WebRTCMedia.prototype.setMuted = function (muted) {
    var self = this;
    this.muted = Boolean(muted);
    if (this.stream && this.stream.getAudioTracks) {
      this.stream.getAudioTracks().forEach(function (track) { track.enabled = !self.muted; });
    }
  };

  /* Drops only the peer connection/context. Never touches the microphone
   * stream: the caller owns it and may hand it to the WSS fallback. */
  WebRTCMedia.prototype.releaseTransport = function () {
    if (this.pc) {
      try { this.pc.close(); } catch (e) { /* ignore */ }
      this.pc = null;
    }
    if (this.remoteNode) {
      try { this.remoteNode.disconnect(); } catch (e) { /* ignore */ }
      this.remoteNode = null;
    }
    if (this.ctx) {
      try { this.ctx.close(); } catch (e) { /* ignore */ }
      this.ctx = null;
    }
  };

  /* Cancel an in-flight/finished WebRTC attempt but KEEP the mic stream
   * alive for the WSS fallback. Idempotent. */
  WebRTCMedia.prototype.abort = function () {
    this.aborted = true;
    this.releaseTransport();
    this.prepared = null;
  };

  /* Drops the peer connection/context but keeps the mic stream alive so the
   * WSS fallback can reuse it. Also cancels in-flight negotiation so a late
   * answer can never revive the abandoned peer connection. */
  WebRTCMedia.prototype.release = function () {
    this.abort();
  };

  /* Terminal release: peer connection + context + microphone tracks. */
  WebRTCMedia.prototype.stop = function () {
    this.closed = true;
    this.aborted = true;
    this.releaseTransport();
    this.prepared = null;
    stopStream(this.stream);
    this.stream = null;
    this._remoteStream = null;
  };

  return {
    WebRTCMedia: WebRTCMedia,
    supported: WebRTCMedia.supported,
    READY_TIMEOUT_MS: READY_TIMEOUT_MS,
  };
}));
