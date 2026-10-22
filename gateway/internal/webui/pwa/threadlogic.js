/* CallRelay PWA — pure thread/line selection rules used by client.js.
 *
 * Extracted as a framework-free UMD module so the cross-line SMS rules are
 * deterministically testable in node (harness/threadlogic-test.js):
 *
 *  - the line a message is sent from is the conversation header picker's
 *    CURRENT value (the user may switch lines mid-thread), never a silent
 *    substitution by the global default;
 *  - the thread to reopen after a send is the canonical threadKey returned
 *    by the gateway (MessageView.threadKey), falling back to lineId:peer
 *    only when the response omits it;
 *  - the chosen line is persisted under BOTH the key the user was viewing
 *    and the canonical key, so the reopened thread defaults to its OWN line
 *    and the next send stays on that line (no snap-back to the default).
 *
 * Default-line resolution for an EXISTING thread (preferredLine) is ordered:
 *  1. the explicit, user-authorized conversation preference;
 *  2. the thread's own line (thread.lineId, or the prefix of a validated
 *     canonical `lineId:peer` key) — so a line2 conversation with no saved
 *     preference never silently composes from the global line1;
 *  3. the authorized global default, then the first currently permitted
 *     line. Every candidate is checked against the currently permitted line
 *     set, so an unavailable/unauthorized SIM is never selected.
 * New-compose defaults are intentionally unchanged (no thread candidate).
 */
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.CallRelayThreadLogic = factory();
  }
}(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  function keyFor(lineId, peer) {
    return String(lineId) + ':' + String(peer);
  }

  /* The gateway's returned MessageView.threadKey is authoritative; the
   * constructed key is only a fallback for a deficient response. */
  function canonicalKey(record, lineId, peer) {
    return (record && record.threadKey) || keyFor(lineId, peer);
  }

  /* The send line is the picker's current value; the preference is a
   * fallback only for an empty/unloaded picker (e.g. no send-capable line
   * option). The global default must never override the user's choice. */
  function chosenLine(pickerValue, fallback) {
    return pickerValue || fallback || '';
  }

  /* Persist the captured line under both keys (mutates and returns `prefs`,
   * which the caller saves to localStorage). */
  function carryPreference(prefs, oldKey, canonical, lineId) {
    if (!lineId) { return prefs; }
    prefs[oldKey] = lineId;
    prefs[canonical] = lineId;
    return prefs;
  }

  /* Retry provenance: the failed message keeps its ORIGINAL line; only a
   * record without provenance uses the conversation preference. */
  function retryLine(message, fallback) {
    return (message && message.lineId) || fallback || '';
  }

  /* The line encoded by a canonical global thread key (`lineId:peer`).
   * Returns '' for anything that is not exactly one ':' with non-empty
   * halves (peer numbers may contain no ':'; the gateway enforces the same
   * shape in splitGlobalID). */
  function lineFromKey(threadKey) {
    if (typeof threadKey !== 'string') { return ''; }
    var idx = threadKey.indexOf(':');
    if (idx <= 0 || idx === threadKey.length - 1) { return ''; }
    if (threadKey.indexOf(':', idx + 1) !== -1) { return ''; }
    return threadKey.slice(0, idx);
  }

  function findLine(lineId, permitted) {
    if (!lineId) { return null; }
    for (var i = 0; i < permitted.length; i += 1) {
      if (permitted[i] && permitted[i].id === lineId) { return permitted[i]; }
    }
    return null;
  }

  /* Resolve the default line for a context, in priority order:
   *   explicit preference -> thread own line -> global default -> first
   * Every returned id is guaranteed to be the id of a currently permitted
   * line; a stale preference or a thread on an unavailable/unauthorized SIM
   * is skipped rather than selected. Returns '' only when no permitted
   * line exists. `thread` may be a ThreadView ({key, lineId}) or null. */
  function preferredLine(options) {
    options = options || {};
    var permitted = options.permitted || [];
    var candidates = [];
    if (options.pref) { candidates.push(options.pref); }
    if (options.thread) {
      candidates.push(options.thread.lineId || lineFromKey(options.thread.key));
    }
    candidates.push(options.globalDefault);
    for (var i = 0; i < candidates.length; i += 1) {
      var id = candidates[i];
      if (id && findLine(id, permitted)) { return id; }
    }
    return (permitted[0] && permitted[0].id) || '';
  }

  return {
    keyFor: keyFor,
    canonicalKey: canonicalKey,
    chosenLine: chosenLine,
    carryPreference: carryPreference,
    retryLine: retryLine,
    lineFromKey: lineFromKey,
    preferredLine: preferredLine,
  };
}));
