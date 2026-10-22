/* CallRelay PWA — gateway call-event envelope parsing (pure, testable).
 * A call.* envelope presents an incoming call ONLY while no call screen is
 * active; an active call is never stolen. */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.CallRelayCallEvents = factory();
  }
}(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  /* Returns {id, peer, lineName} when the envelope is an inbound ringing
   * call, else null. activeCallId guards against re-presenting (or
   * stealing) the tracked call. The gateway's unified state is snake_case
   * (`incoming_ringing`); older fixtures use camelCase — both are accepted. */
  function parseIncomingCallEvent(msg, activeCallId) {
    if (!msg || typeof msg.type !== 'string' || msg.type.indexOf('call.') !== 0) {
      return null;
    }
    var data = msg.data || msg.call || {};
    var state = data.state;
    if (state !== 'incomingRinging' && state !== 'incoming_ringing') {
      return null;
    }
    if (data.direction !== 'inbound') {
      return null;
    }
    if (!data.id || data.id === activeCallId) {
      return null;
    }
    return { id: data.id, peer: data.peer || '', lineName: data.lineName || '' };
  }

  return { parseIncomingCallEvent: parseIncomingCallEvent };
}));
