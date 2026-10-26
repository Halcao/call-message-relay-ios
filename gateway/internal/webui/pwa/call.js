/* CallRelay gateway PWA — incoming-call screen.
 *
 * Flow: the notification navigate URL carries the single-use handoff token in
 * the URL fragment (#h=...), which never reached any access log. This page
 * redeems it (POST body, never URL) with the browser's web session, then:
 *
 *   1. Attempts the automatic callrelay:// scheme handoff once, scoped to the
 *      exact call (call + gateway identity). The native app re-validates the
 *      call against its own paired credential before presenting anything —
 *      the URL is only a hint. This auto-attempt is best-effort: if iOS
 *      blocks the scheme transfer, the page stays visible and the explicit
 *      recovery button appears. One-tap-to-native is NOT claimed without
 *      physical proof.
 *   2. While ringing, polls the per-call status; ended/stale calls show the
 *      honest ended state.
 *
 * Generation handling: a second notification can arrive while this page is
 * already open. A same-path fragment change does NOT reload the page, so we
 * listen for hashchange (and service-worker navigation messages) and restart
 * the flow with a new generation; stale async responses and old poll timers
 * from the previous generation are discarded.
 */
(function () {
  'use strict';

  var SESSION_KEY = 'cr_wp_session';
  var POLL_MS = 2000;
  var POLL_LIMIT = 40; // ~80s, bounded
  var ATTEMPT_GRACE_MS = 1700;

  var generation = 0;

  function $(id) { return document.getElementById(id); }
  function show(id) { $(id).classList.remove('hidden'); }
  function hide(id) { $(id).classList.add('hidden'); }

  var DICT = {
    en: {
      ringing: 'Incoming call…',
      ended: 'This call has ended.',
      openApp: 'Open in CallRelay',
      autoHint: 'Trying to open CallRelay automatically… If it does not switch, tap the button above.',
      noAppHint: 'If the CallRelay app is not installed, this button may do nothing.',
      needBind: 'This browser is not bound yet. Generate a bind code in the app first.',
      openSetup: 'Open setup',
      openAppAnyway: 'Open the app anyway',
      stateRinging: 'Ringing'
    },
    zh: {},
    zhhant: {
      ringing: '來電響鈴中…',
      ended: '該來電已結束。',
      openApp: '在 CallRelay 中打開',
      autoHint: '正在嘗試自動打開 CallRelay……如果沒有跳轉，請點上方按鈕。',
      noAppHint: '未安裝 App 時此按鈕可能無反應。',
      needBind: '此瀏覽器尚未綁定，請先在 App 中產生綁定碼。',
      openSetup: '打開設定',
      openAppAnyway: '仍要打開 App',
      stateRinging: '響鈴中'
    }
  };
  function lang() {
    var tag = (navigator.language || 'zh-CN').toLowerCase();
    if (tag.indexOf('zh') === 0) {
      return tag.indexOf('hant') !== -1 || tag.indexOf('tw') !== -1 || tag.indexOf('hk') !== -1 ? 'zhhant' : 'zh';
    }
    return 'en';
  }
  var L = lang();
  function t(key) {
    if (L === 'zh') return null;
    return (DICT[L] && DICT[L][key]) || DICT.en[key] || null;
  }
  function applyI18n() {
    Array.prototype.forEach.call(document.querySelectorAll('[data-i18n]'), function (el) {
      var value = t(el.getAttribute('data-i18n'));
      if (value) el.textContent = value;
    });
    var open = $('open-app');
    if (open) open.textContent = t('openApp') || '在 CallRelay 中打开';
    document.documentElement.lang = L === 'zh' ? 'zh-CN' : (L === 'zhhant' ? 'zh-Hant' : 'en');
  }

  function api(path, session, body) {
    // A standalone (client-scope) browser binding never leaves a bearer in
    // localStorage: its only credential is the HttpOnly cookie, so the
    // Authorization header must be omitted rather than filled with "null".
    var options = {
      credentials: 'same-origin',
      headers: {
        'Content-Type': 'application/json'
      }
    };
    if (session) options.headers['Authorization'] = 'Bearer ' + session;
    if (body !== undefined) {
      options.method = 'POST';
      options.body = JSON.stringify(body);
    }
    return fetch(path, options).then(function (resp) {
      return resp.json().catch(function () { return {}; }).then(function (data) {
        if (!resp.ok) {
          var err = new Error(data.message || ('HTTP ' + resp.status));
          err.status = resp.status;
          throw err;
        }
        return data;
      });
    });
  }

  function handoffToken() {
    var match = /#h=([0-9A-Za-z_-]+)/.exec(location.hash || '');
    return match ? match[1] : null;
  }

  /// Builds the scoped native deeplink. The native app treats call/gateway as
  /// an untrusted hint: it only presents the call after re-checking with its
  /// own paired credential that the call is ringing and authorized — a stale
  /// or foreign hint degrades to the normal full check, never a wrong call.
  function nativeDeepLink(call) {
    var url = 'callrelay://incoming?call=' + encodeURIComponent(call.callId || '');
    if (call.gatewayId) url += '&g=' + encodeURIComponent(call.gatewayId);
    return url;
  }

  function showEnded(gen) {
    if (gen !== generation) return;
    hide('ringing');
    show('ended');
  }

  function showNeedBind(gen) {
    if (gen !== generation) return;
    hide('ringing');
    show('need-bind');
  }

  function attemptSchemeHandoff(target, gen) {
    // One automatic attempt only. If the platform performs the scheme
    // transfer, the page is hidden before the grace timer fires and the
    // recovery UI never renders; if it stays visible, we fall back to the
    // explicit button. The timer is generation-guarded so a stale attempt
    // can never reveal recovery UI over a newer call.
    try {
      window.location.href = target;
    } catch (e) { /* blocked: recovery UI appears via the timer */ }
    setTimeout(function () {
      if (gen !== generation) return;
      if (document.visibilityState !== 'visible') return;
      var hint = $('auto-hint');
      if (hint) hint.classList.add('hidden');
      show('open-app');
      show('no-app-hint');
    }, ATTEMPT_GRACE_MS);
  }

  function pollStatus(session, callId, gen) {
    var polls = 0;
    function tick() {
      if (gen !== generation) return;
      api('/api/v2/webpush/calls/' + encodeURIComponent(callId), session).then(function (data) {
        if (gen !== generation) return;
        if (!data.ringing) { showEnded(gen); return; }
        polls++;
        if (polls < POLL_LIMIT) setTimeout(tick, POLL_MS);
      }).catch(function () {
        if (gen !== generation) return;
        polls++;
        if (polls < POLL_LIMIT) setTimeout(tick, POLL_MS);
      });
    }
    tick();
  }

  function resetViews() {
    hide('ended');
    hide('need-bind');
    hide('open-app');
    hide('no-app-hint');
    var hint = $('auto-hint');
    if (hint) hint.classList.remove('hidden');
    $('peer').textContent = '—';
    $('line-name').textContent = '';
  }

  function start(gen) {
    if (gen !== generation) return;
    resetViews();
    var token = handoffToken();
    var session = localStorage.getItem(SESSION_KEY);
    if (!token) { showEnded(gen); return; }
    // Standalone branch: a bound browser CLIENT session (cookie) answers
    // IN THE PAGE — the tap foregrounds this page and the answer button
    // requests the mic via the user gesture. It never forces the native
    // scheme handoff. Redeem the handoff with the client cookie only to
    // learn WHICH call is ringing; the client then owns the flow.
    //
    // The cookie check MUST come before the legacy-session check: a
    // client-scope bind deliberately never returns a bearer, so requiring
    // cr_wp_session here made every standalone binding land on "not bound"
    // instead of the answer screen.
    CallRelaySession.current().then(function (client) {
      if (gen !== generation) return;
      if (client) {
        api('/api/v2/webpush/handoff', session, { token: token }).then(function (data) {
          if (gen !== generation) return;
          show('ringing');
          $('peer').textContent = data.peer || '—';
          $('line-name').textContent = data.line || '';
          $('state-text').textContent = (t('stateRinging') || '响铃中');
          show('browser-answer');
          var open = $('browser-answer-open');
          open.setAttribute('href', '/pwa/client.html?call=' + encodeURIComponent(data.callId || ''));
          pollStatus(session, data.callId, gen);
        }).catch(function (err) {
          if (gen !== generation) return;
          if (err.status === 401) { showNeedBind(gen); } else { showEnded(gen); }
        });
        return;
      }
      // No cookie client and no legacy push session: nothing can redeem the
      // handoff, so ask for a bind instead of showing a dead call screen.
      if (!session) { showNeedBind(gen); return; }
      // Native handoff flow (existing): redeem, ring, try the scheme once.
      api('/api/v2/webpush/handoff', session, { token: token }).then(function (data) {
        if (gen !== generation) return;
        show('ringing');
        $('peer').textContent = data.peer || '—';
        $('line-name').textContent = data.line || '';
        $('state-text').textContent = (t('stateRinging') || '响铃中');
        var deepLink = nativeDeepLink(data);
        $('open-app').setAttribute('href', deepLink);
        attemptSchemeHandoff(deepLink, gen);
        pollStatus(session, data.callId, gen);
      }).catch(function (err) {
        if (gen !== generation) return;
        if (err.status === 401) { showNeedBind(gen); } else { showEnded(gen); }
      });
    }).catch(function () {
      if (gen !== generation) return;
      showEnded(gen);
    });
  }

  function boot() {
    generation++;
    applyI18n();
    start(generation);
  }

  // A second notification while this page is open arrives as a fragment
  // change (same-document) — restart the flow for the new token.
  window.addEventListener('hashchange', boot);
  // Service workers deliver a navigation message when they focus an existing
  // window instead of reloading; treat it exactly like a fresh navigation.
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.addEventListener('message', function (event) {
      var data = event.data || {};
      if (data.type === 'callrelay-navigate' && typeof data.hash === 'string') {
        if (('#' + data.hash.replace(/^#/, '')) !== location.hash) {
          location.hash = data.hash;
        } else {
          boot();
        }
      }
    });
  }

  boot();
})();
