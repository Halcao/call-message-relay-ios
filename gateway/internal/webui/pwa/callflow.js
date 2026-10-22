/* CallRelay PWA — call flow controller (framework-agnostic, injectable).
 *
 * Ordering contract (fixed defects from review):
 *   1. prepareAudio() runs from the user gesture: mic permission + player
 *      start, cached. A failure here BLOCKS the answer — the caller is never
 *      answered silently; reject stays available.
 *   2. Only after audio is ready does answer() POST. The media socket is
 *      attached AFTER the answer is accepted, because CheckWSMediaAccess
 *      rejects media attaches while the call is still StateRinging (409).
 *   3. attachMedia() has a bounded ready timeout; a socket error/timeout
 *      surfaces 媒体连接失败 with hangup still available.
 *   4. Every await boundary re-checks the generation: hangup during async
 *      init tears down partial player/capture/stream with no leaks.
 *   5. Hangup is offered from the outset (dialing/answering included).
 *
 * In fixture mode the audio factory returns the demo sink (no mic, no
 * AudioContext) so the flow is reviewable without permissions.
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.CallRelayCallFlow = factory();
  }
}(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  var READY_TIMEOUT_MS = 8000;
  var RETRY_DELAY_MS = 1200;
  // Build-49: mid-call media recovery window. A network blip must not
  // permanently kill call audio: while the gateway call leg is still alive
  // (45 s media grace, disconnect_grace_seconds), the client re-attaches the
  // SAME call's media socket with immediate + 1/2/4/8 s bounded backoff and
  // NEVER re-dials. After the window the media is reported truthfully dead.
  var MEDIA_RECOVERY_WINDOW_MS = 45000;
  var MEDIA_RECOVERY_BACKOFF_MS = [0, 1000, 2000, 4000, 8000];
  // Recovery success needs FRESH BINARY AUDIO on the new socket, not the
  // ready control alone: after ready, the first media frame must arrive
  // within this window or the attempt fails (same fresh-media proof rule as
  // the native recovery).
  var RECOVERY_AUDIO_PROOF_MS = 3000;
  // Send-path bounds: beyond ~8 frames of queued bytes the tunnel is adding
  // latency, not throughput — drop the stale frame (realtime audio rule,
  // mirrors the native drop-stale uplink queue).
  var MAX_BUFFERED_BYTES = 8 * 160;

  function silentFrame() {
    var frame = new Uint8Array(160);
    // μ-law zero is 0xFF (0x00 decodes to near negative full scale): a muted
    // frame must be digital silence, never a loud DC value.
    frame.fill(0xFF);
    return frame;
  }

  function CallFlow(deps) {
    this.api = deps.api;                       // (method, path, body?) -> Promise
    this.makePlayer = deps.makePlayer;         // () -> {start()->Promise, feed(bytes), stop()}
    this.makeCapture = deps.makeCapture;       // () -> {start(stream)->Promise, stop(), onFrame}
    this.getMic = deps.getMic;                 // () -> Promise<stream>
    this.openSocket = deps.openSocket;         // (callId) -> socket (WebSocket-like)
    /* Optional gesture-time dial: when present, beginOutgoing prepares the
     * microphone BEFORE awaiting the network, so a permission failure never
     * dials and iOS keeps the user activation. Returns the call record. */
    this.dial = deps.dial || null;             // () -> Promise<{id, peer?}>
    /* Optional browser-native WebRTC path (reuses the gateway's existing
     * direct offer/answer endpoints). When present it is tried FIRST; the
     * WSS transport stays the fallback. */
    this.makeWebRTC = deps.makeWebRTC || null; // (callId) -> {prepare,start,setMuted,release,stop}
    this.onState = deps.onState || function () {};   // (view) -> void, view: {phase,peer,media}
    this.now = deps.now || function () { return Date.now(); };
    // Wrap the natives in closures: assigning the raw global as a method
    // makes `this` the instance and WebKit throws "Illegal invocation".
    this.setTimeout = deps.setTimeout || function (fn, ms) { return setTimeout(fn, ms); };
    this.clearTimeout = deps.clearTimeout || function (t) { clearTimeout(t); };

    this.callId = deps.callId;
    this.peer = deps.peer || '';
    this.incoming = Boolean(deps.incoming);
    this.accepted = false;   // flips true only once the answer POST is accepted
    this.generation = 0;
    this.over = false;
    this.player = null;
    this.capture = null;
    this.stream = null;
    this.socket = null;
    this.webrtc = null;       // active WebRTC engine, if any
    this.mediaKind = 'idle';  // 'idle' -> 'webrtc' | 'wss'
    // Transport state is tracked independently from the gateway call state:
    // a connected host/peer is NOT an answered call, and a gateway "active"
    // must never render media as ready before the transport attached.
    this.mediaState = 'idle'; // idle|preparing|attaching|ready|failed
    this.callState = '';      // latest gateway call state
    this.audioReady = null;   // cached prepareAudio promise
    this.wssReady = false;    // WSS player+capture actually started
    this.pollTimer = null;
    // Build-49 mid-call media recovery: one episode per drop burst. The
    // episode owns its absolute monotonic deadline (45 s, same-call grace),
    // its attempt counter, the in-flight attempt id and its backoff timer —
    // duplicate error/close events from replaced sockets can neither reset
    // the deadline nor start overlapping attempts.
    this.recovery = null;
    this.readyTimer = null;
    this.retryTimer = null;
    this.muted = false;
    this.lastPhase = '';
    this.lastMedia = 'idle';
  }

  CallFlow.prototype.view = function (phase, media, error) {
    this.lastPhase = phase;
    if (media) { this.lastMedia = media; }
    this.onState({
      phase: phase,                    // ringing|dialing|preparing|answering|connecting|active|ended|failed
      peer: this.peer,
      media: media || this.lastMedia || 'idle',  // idle|preparing|attaching|ready|failed
      mediaKind: this.mediaKind || 'idle',       // idle|webrtc|wss
      incoming: this.incoming,
      canAccept: this.incoming && !this.over,
      canHangup: !this.over,
      error: error ? (error.message || String(error)) : undefined,
      muted: this.muted,
    });
  };

  /* ---------------- public controls ---------------- */

  /* Outgoing: audio preparation runs FIRST in the same user gesture, before
   * the call is created on the gateway. A denied microphone must never dial.
   * `dial` (when provided) returns the gateway call record; the flow then
   * polls and attaches media. */
  CallFlow.prototype.beginOutgoing = function () {
    var self = this;
    this.view('preparing', 'preparing');
    return this.prepareAudio().then(function () {
      if (self.over) { return false; }
      if (!self.dial) {
        // Call already exists (tests / externally created leg): proceed.
        self.view('dialing', 'preparing');
        return null;
      }
      self.view('dialing', 'preparing');
      return self.dial().then(function (call) {
        if (self.over) { return false; }
        if (!call || !call.id) { throw new Error('呼叫失败'); }
        self.callId = call.id;
        if (self.webrtc && self.webrtc.setCallId) { self.webrtc.setCallId(call.id); }
        if (call.peer) { self.peer = call.peer; }
        return call;
      });
    }).then(function (result) {
      if (self.over || result === false) { return false; }
      self.view('connecting', 'attaching');
      self.startPolling();
      return self.attachMediaWithRetry();
    }).catch(function (err) {
      if (!self.over) { self.view('failed', 'failed', err); }
      return false;
    });
  };

  /* Incoming: poll only; accept() drives prepare -> answer -> attach. */
  CallFlow.prototype.beginIncoming = function () {
    this.view('ringing');
    this.startPolling();
  };

  CallFlow.prototype.accept = function () {
    var self = this;
    if (!this.incoming || this.over) { return Promise.resolve(false); }
    this.view('preparing', 'preparing');
    // Gesture-phase audio prep FIRST; a failure never answers the call.
    return this.prepareAudio().then(function () {
      if (self.over) { return false; }
      self.view('answering');
      return self.api('POST', '/api/v2/calls/' + enc(self.callId) + '/answer', {})
        .then(function () {
          if (self.over) { return false; }
          // The call is answered: from here a local end is a HANGUP, never
          // a reject (media may still fail/retry — reject is ringing-only).
          self.accepted = true;
          self.view('connecting');
          return self.attachMediaWithRetry();
        });
    }).then(function (ok) {
      return ok !== false;
    }).catch(function (err) {
      if (!self.over) {
        // ONE view carrying the error: accept/reject controls stay up.
        self.view('ringing', 'idle', err || new Error('audio'));
      }
      return false;
    });
  };

  /* Truthful mute: gates captured frames to silence (see onFrame). The
   * current view is re-emitted so every consumer's control reflects the
   * real state immediately (the client also toggles its local label). */
  CallFlow.prototype.setMuted = function (muted) {
    this.muted = Boolean(muted);
    if (this.webrtc && this.webrtc.setMuted) { this.webrtc.setMuted(this.muted); }
    if (!this.over && this.lastPhase === 'active') {
      this.view('active', this.activeMediaView());
    }
  };

  /* Local hangup ALWAYS converges the UI to ended: resources are released
   * exactly once (idempotent teardown), the terminal view is emitted even
   * if the API is unreachable, and the returned promise only settles after
   * the request has completed (or failed) so callers can await it.
   *
   * Action choice is state-based, not incoming-based: an UNANSWERED inbound
   * call rejects (tells the modem to reject the ringing leg); every other
   * case — outbound, accepted/connecting, active — hangs up. After accept
   * the caller has been answered, so a default "reject" would be wrong. */
  CallFlow.prototype.hangup = function (action) {
    if (this.over) { return Promise.resolve(); }
    var self = this;
    var unansweredIncoming = this.incoming && !this.accepted;
    var act = action || (unansweredIncoming ? 'reject' : 'hangup');
    // Terminal state FIRST: release resources exactly once (idempotent
    // teardown stops poll/ready/retry timers, socket and audio), THEN emit
    // the ended view so its canHangup is already false. A stopped poll can
    // never race back in and mask a media failure.
    this.endedReason = act === 'reject' ? 'rejected' : 'local_hangup';
    this.teardown();
    this.view('ended');
    return this.api('POST', '/api/v2/calls/' + enc(this.callId) + '/' + act, {})
      .catch(function () { /* call may already be gone; UI stays ended */ })
      .then(function () { return undefined; });
  };

  /* Foreground event-stream convergence (client.js forwards call.* here).
   * A terminal event from the peer/worker closes the screen even between
   * polls; non-terminal events are ignored because polling owns the phase
   * machine. Idempotent: a local hangup already set `over`. */
  CallFlow.prototype.onEvent = function (msg) {
    if (this.over || !msg || typeof msg.type !== 'string' || msg.type.indexOf('call.') !== 0) {
      return;
    }
    var data = msg.data || msg.call || {};
    if (data.id && String(data.id) !== String(this.callId)) { return; }
    if (data.state === 'ended' || data.state === 'idle') {
      this.finishEnded(data.endReason || null);
    }
  };

  /* ---------------- internals ---------------- */

  CallFlow.prototype.prepareAudio = function () {
    var self = this;
    if (this.audioReady) { return this.audioReady; }
    var gen = this.generation;
    this.mediaState = 'preparing';
    this.audioReady = Promise.resolve().then(function () {
      if (self.makeWebRTC) {
        var engine = self.makeWebRTC(self.callId);
        // Build-49: a mid-call peer-connection drop is a media drop like a
        // socket close: report it and recover over the WSS transport inside
        // the bounded same-call window (the dead engine is released first;
        // recovery never re-probes WebRTC).
        engine.onDisconnect = function () {
          if (self.over || self.webrtc !== engine || self.mediaKind !== 'webrtc') { return; }
          self.mediaState = 'failed';
          self.webrtc = null;
          self.mediaKind = 'wss';
          try { engine.release(); } catch (e) { /* ignore */ }
          if (self.callState === 'active') { self.view('active', 'failed'); }
          else { self.view('connecting', 'failed'); }
          self.onMediaDrop();
        };
        self.webrtc = engine;
        return engine.prepare();
      }
      return null;
    }).then(function () {
      self.checkGen(gen);
      // The WebRTC engine owns the gesture-acquired stream; the WSS
      // fallback reuses it instead of asking for a second mic grant.
      if (self.webrtc && self.webrtc.stream) { self.stream = self.webrtc.stream; }
      return self.ensureMic();
    }).then(function () {
      self.checkGen(gen);
      // Gesture-window pre-warm: the WSS player/capture contexts are created
      // and resumed NOW, so a fallback later in the call never needs a fresh
      // user activation (creating them after negotiation loses it on iOS).
      return self.prepareWSSContexts(gen);
    }).then(function () {
      self.checkGen(gen);
      return true;
    }).catch(function (err) {
      self.cleanupAudio();
      if (self.webrtc) { self.webrtc.stop(); self.webrtc = null; }
      self.audioReady = null;
      throw err;
    });
    return this.audioReady;
  };

  /* Pre-create the WSS player/capture and let them claim/resume their
   * AudioContext inside the user gesture. Capture does not touch the mic
   * until start(stream) and no frames are sent. */
  CallFlow.prototype.prepareWSSContexts = function (gen) {
    var self = this;
    if (!this.makePlayer || !this.makeCapture) { return Promise.resolve(); }
    if (!this.player) { this.player = this.makePlayer(); }
    if (!this.capture) { this.capture = this.makeCapture(); }
    var warm = [];
    [this.player, this.capture].forEach(function (node) {
      if (node && typeof node.prepare === 'function') {
        warm.push(Promise.resolve(node.prepare()).then(function () { self.checkGen(gen); }));
      }
    });
    return Promise.all(warm);
  };

  /* Media view for an answered call: readiness must come from the actual
   * transport, never from the gateway state alone. */
  CallFlow.prototype.activeMediaView = function () {
    if (this.mediaState === 'failed') { return 'failed'; }
    if (this.mediaState === 'ready') { return 'ready'; }
    return 'attaching';
  };

  /* One microphone acquisition per call, shared by both media paths. */
  CallFlow.prototype.ensureMic = function () {
    var self = this;
    if (this.stream) { return Promise.resolve(this.stream); }
    return this.getMic().then(function (stream) {
      self.stream = stream;
      return stream;
    });
  };

  /* WSS (PCMU) pipeline: player + capture worklets over the existing mic
   * stream. Contexts may already have been pre-warmed in the user gesture;
   * they are only STARTED here, and only when WebRTC is not carrying the
   * call. Every await re-checks the generation so a hangup during start
   * cannot leave a live capture behind. */
  CallFlow.prototype.prepareWSSPipeline = function () {
    var self = this;
    var gen = this.generation;
    if (this.wssReady) { return Promise.resolve(true); }
    return Promise.resolve().then(function () {
      self.checkGen(gen);
      if (!self.player) { self.player = self.makePlayer(); }
      return self.player.start ? self.player.start() : self.player;
    }).then(function (started) {
      self.checkGen(gen);
      if (started) { self.player = started; }
      if (!self.capture) { self.capture = self.makeCapture(); }
      return self.ensureMic();
    }).then(function (stream) {
      self.checkGen(gen);
      return self.capture.start ? self.capture.start(stream) : self.capture;
    }).then(function (startedCapture) {
      self.checkGen(gen);
      if (startedCapture) { self.capture = startedCapture; }
      self.wireCapture(self.capture);
      self.wssReady = true;
      return true;
    }).catch(function (err) {
      self.cleanupAudio();
      throw err;
    });
  };

  CallFlow.prototype.wireCapture = function (capture) {
    var self = this;
    capture.onFrame = function (frame) {
      if (self.over) { return; }
      // Mute is a CAPTURE GATE: while muted only μ-law silence leaves, so
      // the mic can never transmit while the UI claims muted (the gateway
      // media socket only understands ping text — a JSON mute command
      // would be ignored). Unmute restores real frames immediately.
      var outbound = self.muted ? silentFrame() : frame;
      if (self.socket && self.socket.readyState === 1) {
        // Bounded send backlog: drop the frame instead of growing latency.
        if (self.socket.bufferedAmount !== undefined &&
            self.socket.bufferedAmount > MAX_BUFFERED_BYTES) {
          return;
        }
        self.socket.send(outbound);
      }
    };
  };

  /* Attach with fallback: browser-native WebRTC first when available, then
   * the existing WSS PCMU transport (cellular / no reachable ICE / no
   * WebRTC support). The chosen transport is reported in `mediaKind`. */
  CallFlow.prototype.attachMediaWithRetry = function () {
    var self = this;
    if (this.webrtc && this.mediaKind !== 'wss') {
      return this.attachWebRTC().catch(function (err) {
        if (self.over) { throw new Error('ended'); }
        if (err && err.message === 'ended') { throw err; }
        // Truthful fallback: drop the peer connection but KEEP the mic
        // stream alive for the WSS pipeline.
        self.mediaKind = 'wss';
        var engine = self.webrtc;
        self.webrtc = null;
        if (engine) {
          if (engine.release) { engine.release(); } else { engine.stop(); }
        }
        self.view('connecting', 'attaching');
        return self.prepareWSSPipeline().then(function () {
          return self.attachWSSWithRetry();
        });
      });
    }
    return this.prepareWSSPipeline().then(function () {
      return self.attachWSSWithRetry();
    });
  };

  /* Browser WebRTC attach: ready means the peer connection is connected.
   * The outer bound CANCELS the engine so a late success can never flip the
   * flow back off the WSS fallback or touch the shared mic. */
  CallFlow.prototype.attachWebRTC = function () {
    var self = this;
    var gen = this.generation;
    var engine = this.webrtc;
    this.view('connecting', 'attaching');
    return new Promise(function (resolve, reject) {
      var settled = false;
      var finish = function (ok, err) {
        if (settled) { return; }
        settled = true;
        if (self.readyTimer) { self.clearTimeout(self.readyTimer); self.readyTimer = null; }
        if (ok) { resolve(true); } else { reject(err || new Error('webrtc')); }
      };
      self.readyTimer = self.setTimeout(function () {
        if (engine && engine.abort) { try { engine.abort(); } catch (e) { /* ignore */ } }
        finish(false, new Error('直连超时'));
      }, READY_TIMEOUT_MS);
      engine.start().then(function () {
        if (self.over || gen !== self.generation || engine !== self.webrtc) {
          finish(false, new Error('ended'));
          return;
        }
        self.mediaKind = 'webrtc';
        self.mediaState = 'ready';
        // WebRTC carries the call: release the pre-warmed WSS contexts so
        // only one audio owner (the peer connection) stays live.
        self.releaseIdleWSS();
        // A connected peer connection is media-ready, not an answered call:
        // only the gateway state decides when the telephony UI says active.
        if (self.callState === 'active') { self.view('active', 'ready'); }
        else { self.view('connecting', 'ready'); }
        finish(true);
      }).catch(function (err) { finish(false, err); });
    });
  };

  /* WSS attach retry (the existing bounded retry). */
  CallFlow.prototype.attachWSSWithRetry = function () {
    var self = this;
    return this.attachMedia().catch(function () {
      if (self.over) { throw new Error('ended'); }
      self.mediaState = 'failed';
      self.view('connecting', 'failed');
      return new Promise(function (resolve, reject) {
        self.retryTimer = self.setTimeout(function () {
          self.attachMedia().then(resolve, reject);
        }, RETRY_DELAY_MS);
      });
    });
  };

  /* Mid-call media drop (socket closed/errored after the transport was
   * established, or the WebRTC peer connection dropped): re-attach the SAME
   * call inside the 45 s gateway grace window. The recovery EPISODE owns an
   * absolute monotonic deadline (never reset by duplicate drop events), a
   * per-attempt identity, per-attempt timers, and exact current-socket
   * fencing — stale/replaced sockets can never feed the player, flip state,
   * clear another attempt's timer, or restart the window. Success requires
   * fresh binary audio on the new socket. At the deadline the call is ended
   * locally once (best-effort hangup) instead of lingering with dead media.
   * Never a new dial; remote end / revoked auth end immediately via polling.
   */
  CallFlow.prototype.onMediaDrop = function () {
    if (this.over) { return; }
    if (this.callState !== 'active' && this.callState !== 'connecting') { return; }
    if (this.recovery) {
      if (this.recovery.done) { this.recovery = null; }
      else { return; } // episode running: deadline and attempts untouched
    }
    var self = this;
    var recovery = this.recovery = {
      deadline: this.now() + MEDIA_RECOVERY_WINDOW_MS,
      attempt: 0,
      inFlight: null,
      backoffTimer: null,
      deadlineTimer: null,
      done: false,
    };
    // Episode-owned absolute deadline: a hung pipeline/attach or a backoff
    // gap can never outlive the window. Cancelled on success/terminal.
    recovery.deadlineTimer = this.setTimeout(function () {
      recovery.deadlineTimer = null;
      if (self.recovery === recovery && !recovery.done) { self.recoveryGiveUp(); }
    }, MEDIA_RECOVERY_WINDOW_MS);
    this.driveRecovery();
  };

  CallFlow.prototype.driveRecovery = function () {
    var self = this;
    var recovery = this.recovery;
    if (!recovery || recovery.done) { return; }
    if (this.over) { return; }
    if (this.callState !== 'active' && this.callState !== 'connecting') { return; }
    if (recovery.inFlight !== null) { return; }
    if (recovery.backoffTimer) { return; }
    if (this.now() > recovery.deadline) { this.recoveryGiveUp(); return; }
    recovery.attempt += 1;
    var attempt = recovery.attempt;
    recovery.inFlight = attempt;
    this.view(this.callState === 'active' ? 'active' : 'connecting', 'attaching');
    // A WebRTC drop released the idle WSS pipeline: rebuild it (player +
    // capture on the kept mic stream) before the first WSS attempt so a
    // successful attach can actually carry audio.
    var pipeline = this.wssReady ? Promise.resolve(true) : this.prepareWSSPipeline();
    pipeline.then(function () {
      if (self.recovery !== recovery || recovery.inFlight !== attempt) { return; }
      self.recoveryAttach(attempt).then(function () {
        if (self.recovery === recovery) {
          recovery.done = true;
          if (recovery.deadlineTimer) { self.clearTimeout(recovery.deadlineTimer); recovery.deadlineTimer = null; }
          recovery.inFlight = null;
          self.recovery = null;
        }
      }, function () {
        if (self.recovery !== recovery) { return; }
        recovery.inFlight = null;
        if (self.over) { return; }
        if (self.callState !== 'active' && self.callState !== 'connecting') { return; }
        if (self.now() > recovery.deadline) { self.recoveryGiveUp(); return; }
        var backoff = MEDIA_RECOVERY_BACKOFF_MS[Math.min(recovery.attempt - 1, MEDIA_RECOVERY_BACKOFF_MS.length - 1)];
        recovery.backoffTimer = self.setTimeout(function () {
          recovery.backoffTimer = null;
          self.driveRecovery();
        }, backoff);
      });
    }, function () {
      if (self.recovery !== recovery) { return; }
      recovery.inFlight = null;
      if (self.over) { return; }
      if (self.now() > recovery.deadline) { self.recoveryGiveUp(); return; }
      recovery.backoffTimer = self.setTimeout(function () {
        recovery.backoffTimer = null;
        self.driveRecovery();
      }, 1000);
    });
  };

  /* One fenced recovery attempt. TWO fences:
   *  - candidate(): only the in-flight attempt may be promoted or failed
   *    (recovery episode + attempt id + exact socket + generation).
   *  - installed(): after promotion the SAME socket serves the call for its
   *    whole carrier lifetime — every later frame feeds the player, and a
   *    later drop begins a NEW episode. Replacing the socket (newer
   *    attempt) or teardown silently retires the old callbacks.
   * Success requires fresh binary audio on this socket before the absolute
   * monotonic deadline; the ready control alone only arms the proof. */
  CallFlow.prototype.recoveryAttach = function (attempt) {
    var self = this;
    var gen = this.generation;
    var recovery = this.recovery;
    var socket;
    try {
      socket = this.openSocket(this.callId);
    } catch (err) {
      return Promise.reject(err);
    }
    // The previous candidate (if any) is stale the moment a new attempt
    // opens: close it so its queued callbacks can never touch the new one.
    if (this.socket && this.socket !== socket) {
      try { this.socket.close(); } catch (e) { /* ignore */ }
    }
    this.socket = socket;
    this.mediaKind = 'wss';
    this.mediaState = 'attaching';
    return new Promise(function (resolve, reject) {
      var settled = false;
      // promoted is set ONLY by promote(): a FAILED candidate (settled
      // without promotion) is never an installed carrier, so its queued
      // binary frames and drop callbacks can never feed the player or
      // spawn a parallel episode during the backoff gap.
      var promoted = false;
      var candidate = function () {
        return !promoted && !self.over && gen === self.generation &&
          self.recovery === recovery && recovery.inFlight === attempt &&
          self.socket === socket;
      };
      var installed = function () {
        return promoted && !self.over && gen === self.generation && self.socket === socket;
      };
      var failAttempt = function (err) {
        if (settled) { return; }
        settled = true;
        self.clearTimeout(readyTimer);
        self.clearTimeout(audioTimer);
        // Detach the failed candidate so nothing can treat it as current
        // while the backoff runs, then close it.
        if (self.socket === socket) { self.socket = null; }
        try { socket.close(); } catch (e) { /* ignore */ }
        reject(err || new Error('media'));
      };
      var promote = function () {
        if (settled) { return; }
        // A late proof past the absolute deadline never promotes.
        if (self.now() > recovery.deadline) {
          failAttempt(new Error('media-deadline'));
          return;
        }
        promoted = true;
        settled = true;
        self.clearTimeout(readyTimer);
        self.clearTimeout(audioTimer);
        self.mediaState = 'ready';
        if (self.callState === 'active') { self.view('active', 'ready'); }
        else { self.view('connecting', 'ready'); }
        resolve();
      };
      var readyTimer = self.setTimeout(function () {
        if (candidate()) { failAttempt(new Error('media-timeout')); }
      }, READY_TIMEOUT_MS);
      var audioTimer = null;
      socket.onopen = function () { /* ready frame drives state */ };
      socket.onmessage = function (event) {
        if (typeof event.data === 'string') {
          if (!candidate()) { return; }
          var ctrl;
          try { ctrl = JSON.parse(event.data); } catch (e) { return; }
          if (ctrl.type === 'ready') {
            // Arm only: binary audio on THIS socket is the actual proof.
            self.mediaState = 'ready-pending-audio';
            self.clearTimeout(audioTimer);
            audioTimer = self.setTimeout(function () {
              if (candidate()) { failAttempt(new Error('media-audio-proof-timeout')); }
            }, RECOVERY_AUDIO_PROOF_MS);
          }
          return;
        }
        if (promoted) {
          // Promoted carrier: every frame feeds the player for the socket's
          // whole installed lifetime (never one-frame-only).
          if (installed() && self.player) { self.player.feed(new Uint8Array(event.data)); }
          return;
        }
        if (!candidate()) { return; }
        // Binary BEFORE the ready control is not proof and is never fed.
        if (self.mediaState === 'ready-pending-audio') {
          promote();
          // The proving frame plays too.
          if (self.player) { self.player.feed(new Uint8Array(event.data)); }
        }
      };
      socket.onerror = function () {
        if (!settled) {
          if (candidate()) { failAttempt(new Error('media-error')); }
          return;
        }
        if (installed()) { self.onMediaDrop(); } // carrier died: new episode
      };
      socket.onclose = function () {
        if (!settled) {
          if (candidate()) { failAttempt(new Error('media-closed')); }
          return;
        }
        if (installed()) {
          self.mediaState = 'failed';
          if (self.callState === 'active') { self.view('active', 'failed'); }
          else { self.view('connecting', 'failed'); }
          self.onMediaDrop();
        }
      };
    });
  };

  /* Deadline reached: end the call locally ONCE (idempotent), best-effort
   * hangup, and converge to the terminal ended view — the same truthfulness
   * rule as the native client (never leave dead media alive past the
   * absolute recovery deadline). */
  CallFlow.prototype.recoveryGiveUp = function () {
    var recovery = this.recovery;
    if (recovery) {
      recovery.done = true;
      if (recovery.deadlineTimer) { this.clearTimeout(recovery.deadlineTimer); recovery.deadlineTimer = null; }
      if (recovery.backoffTimer) { this.clearTimeout(recovery.backoffTimer); recovery.backoffTimer = null; }
      this.recovery = null;
    }
    if (this.over) { return; }
    var id = this.callId;
    if (id) {
      try {
        this.api('POST', '/api/v2/calls/' + enc(id) + '/hangup', {});
      } catch (e) { /* best-effort */ }
    }
    this.finishEnded('media-recovery-timeout');
  };

  CallFlow.prototype.attachMedia = function () {
    var self = this;
    var gen = this.generation;
    this.mediaState = 'attaching';
    this.view('connecting', 'attaching');
    return new Promise(function (resolve, reject) {
      var socket;
      try {
        socket = self.openSocket(self.callId);
      } catch (err) {
        reject(err);
        return;
      }
      self.socket = socket;
      var settled = false;
      var finish = function (ok, err) {
        if (settled) { return; }
        settled = true;
        self.clearTimeout(readyTimer);
        if (ok) { resolve(); } else { reject(err || new Error('media')); }
      };
      var readyTimer = self.setTimeout(function () {
        finish(false, new Error('media-timeout'));
      }, READY_TIMEOUT_MS);
      socket.onopen = function () { /* ready frame drives state */ };
      socket.onmessage = function (event) {
        if (self.over || gen !== self.generation || self.socket !== socket) { return; }
        if (typeof event.data === 'string') {
          var ctrl;
          try { ctrl = JSON.parse(event.data); } catch (e) { return; }
          if (ctrl.type === 'ready') {
            self.mediaKind = 'wss';
            self.mediaState = 'ready';
            if (self.callState === 'active') { self.view('active', 'ready'); }
            else { self.view('connecting', 'ready'); }
            finish(true);
          }
          return;
        }
        if (self.player) { self.player.feed(new Uint8Array(event.data)); }
      };
      socket.onerror = function () {
        if (self.over || self.socket !== socket) { return; }
        // Pre-ready failure rejects the attach (bounded retry handles it);
        // a settled socket error mid-call is a media drop.
        if (!settled) { finish(false, new Error('media-error')); return; }
        self.mediaState = 'failed';
        if (self.callState === 'active') { self.view('active', 'failed'); }
        else { self.view('connecting', 'failed'); }
        self.onMediaDrop();
      };
      socket.onclose = function () {
        if (self.over || self.socket !== socket) { return; }
        if (!settled) { finish(false, new Error('media-closed')); return; }
        // Mid-call close: report truthfully (poll may still say active),
        // then re-attach within the bounded same-call recovery window.
        self.mediaState = 'failed';
        if (self.callState === 'active') { self.view('active', 'failed'); }
        else { self.view('connecting', 'failed'); }
        self.onMediaDrop();
      };
    });
  };

  CallFlow.prototype.startPolling = function () {
    var self = this;
    var tick = function () {
      if (self.over) { return; }
      self.api('GET', '/api/v2/calls/' + enc(self.callId)).then(function (record) {
        if (self.over) { return; }
        if (record.peer) { self.peer = record.peer; }
        self.callState = record.state || '';
        switch (record.state) {
          case 'active':
            // The gateway leg being active must NOT mask broken or not yet
            // attached media: the transport state owns the media view.
            self.view('active', self.activeMediaView());
            break;
          case 'ended':
          case 'idle':
            self.finishEnded(record.endReason);
            return;
          case 'incomingRinging':
          case 'incoming_ringing':
            if (self.incoming) { self.view('ringing'); }
            break;
          case 'connecting':
          case 'outgoingDialing':
          case 'outgoing_dialing':
            if (self.mediaState === 'ready') { self.view('connecting', 'ready'); }
            else if (self.mediaState === 'failed') { self.view('connecting', 'failed'); }
            break;
          default:
            break;
        }
        self.pollTimer = self.setTimeout(tick, 1500);
      }).catch(function (err) {
        if (self.over) { return; }
        // Revoked/disallowed auth and a vanished call are terminal: never
        // recover media for a call the gateway already ended or refuses.
        if (err && (err.status === 404 || err.status === 410 ||
                    err.status === 401 || err.status === 403)) {
          self.finishEnded(null);
          return;
        }
        self.pollTimer = self.setTimeout(tick, 3000);
      });
    };
    tick();
  };

  CallFlow.prototype.finishEnded = function (endReason) {
    if (this.over) { return; }
    this.endedReason = endReason;
    this.view('ended');
    this.teardown();
  };

  CallFlow.prototype.checkGen = function (gen) {
    if (this.over || gen !== this.generation) {
      throw new Error('ended');
    }
  };

  /* Release WSS player/capture that were pre-warmed but are not carrying the
   * call (WebRTC won). Never touches the mic stream. */
  CallFlow.prototype.releaseIdleWSS = function () {
    if (this.wssReady) { return; }
    if (this.capture) {
      if (typeof this.capture.stop === 'function') { try { this.capture.stop(); } catch (e) { /* ignore */ } }
      this.capture = null;
    }
    if (this.player) {
      if (typeof this.player.stop === 'function') { try { this.player.stop(); } catch (e) { /* ignore */ } }
      this.player = null;
    }
  };

  CallFlow.prototype.cleanupAudio = function () {
    if (this.capture) {
      if (typeof this.capture.stop === 'function') { this.capture.stop(); }
      this.capture = null;
    }
    if (this.player) {
      if (typeof this.player.stop === 'function') { this.player.stop(); }
      this.player = null;
    }
    if (this.stream) {
      this.stream.getTracks().forEach(function (track) { track.stop(); });
      this.stream = null;
    }
  };

  CallFlow.prototype.teardown = function () {
    if (this.over) { return; }
    this.over = true;
    this.generation += 1;
    if (this.pollTimer) { this.clearTimeout(this.pollTimer); this.pollTimer = null; }
    if (this.readyTimer) { this.clearTimeout(this.readyTimer); this.readyTimer = null; }
    if (this.retryTimer) { this.clearTimeout(this.retryTimer); this.retryTimer = null; }
    if (this.recovery) {
      if (this.recovery.deadlineTimer) { this.clearTimeout(this.recovery.deadlineTimer); }
      if (this.recovery.backoffTimer) { this.clearTimeout(this.recovery.backoffTimer); }
      this.recovery = null;
    }
    if (this.socket) {
      try { this.socket.close(); } catch (e) { /* ignore */ }
      this.socket = null;
    }
    if (this.webrtc) {
      try { this.webrtc.stop(); } catch (e) { /* ignore */ }
      this.webrtc = null;
    }
    this.cleanupAudio();
  };

  function enc(value) { return encodeURIComponent(value); }

  return { CallFlow: CallFlow, READY_TIMEOUT_MS: READY_TIMEOUT_MS };
}));
