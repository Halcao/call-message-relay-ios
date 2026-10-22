/* CallRelay gateway PWA — setup/status page.
 *
 * Security properties:
 *  - The bind code arrives via a #fragment (never in any access log) or is
 *    typed by the user. The native app pairing credential never appears here.
 *  - The web session token lives only in this origin's storage, is sent only
 *    as an Authorization bearer to /api/v2/webpush/*, and is rejected by
 *    every other API route.
 *  - No third-party assets, fonts or trackers; referrer is never sent.
 */
(function () {
  'use strict';

  var SESSION_KEY = 'cr_wp_session';
  var session = null;

  function $(id) { return document.getElementById(id); }
  function show(id) { $(id).classList.remove('hidden'); }
  function hide(id) { $(id).classList.add('hidden'); }

  function api(path, options) {
    options = options || {};
    options.credentials = 'same-origin';   // client sessions are cookie-bound
    options.headers = options.headers || {};
    options.headers['Content-Type'] = 'application/json';
    if (session) options.headers['Authorization'] = 'Bearer ' + session;
    return fetch(path, options).then(function (resp) {
      return resp.json().catch(function () { return {}; }).then(function (body) {
        if (!resp.ok) {
          var err = new Error(body.message || ('HTTP ' + resp.status));
          err.status = resp.status;
          throw err;
        }
        return body;
      });
    });
  }

  function supported() {
    // isSecureContext is hostname-aware: true for https origins and for
    // potentially-trustworthy hosts such as localhost/127.0.0.1.
    return ('serviceWorker' in navigator) && ('PushManager' in window) &&
      Boolean(window.isSecureContext);
  }

  function isStandalone() {
    return window.matchMedia('(display-mode: standalone)').matches ||
      window.navigator.standalone === true;
  }

  // ---------- i18n ----------
  var DICT = {
    en: {
      unsupported: 'Notifications are not supported here. On iPhone/iPad, add this page to your Home Screen with Safari first, then try again.',
      installHelp: 'In Safari, add this page to your Home Screen first, then open it from there to bind.',
      bindTitle: 'Turn on call notifications',
      bindHint: 'In the CallRelay app, open Settings → Web Push to generate a bind code, then enter it below.',
      bindPlaceholder: 'BIND CODE',
      bindButton: 'Bind',
      deviceLabel: 'Bound device',
      modeLabel: 'Notify mode',
      subsLabel: 'Browser subscriptions',
      enableButton: 'Allow notifications',
      testButton: 'Send test',
      modeNative: 'System push (app) only',
      modeBoth: 'System + web push',
      modeWeb: 'Web push only',
      unbindTitle: 'Unbind',
      unbindButton: 'Unbind this browser',
      bindFailed: 'Bind failed. Check the code and try again.',
      bound: 'Bound. Allow notifications below.',
      subAdded: 'Notifications are on for this browser.',
      permissionDenied: 'Notifications are blocked. Enable them in Settings → Notifications → CallRelay, then try again.',
      permissionPending: 'Waiting for the permission prompt…',
      testSent: 'Test sent — check this device.',
      testFailed: 'Test failed. Allow notifications and try again.',
      modeSaved: 'Notify mode saved.',
      actionFailed: 'The action did not finish. Please try again.',
      removeSub: 'Remove',
      unknownBrowser: 'browser',
      browserOn: 'On',
      browserOff: 'Not enabled',
      browserDenied: 'Blocked — allow notifications to enable',
      clientBack: '‹ Back to Calls / Messages'
    },
    zh: {},
    zhhant: {
      unsupported: '此瀏覽器不支援通知。請在 iPhone/iPad 上用 Safari 加入主畫面後再試。',
      installHelp: '請先用 Safari 將本頁「加入主畫面」，再從主畫面打開綁定。',
      bindTitle: '開啟來電通知',
      bindHint: '在 App 的「設定 → 網頁通知」中產生綁定碼，輸入到下方。',
      bindPlaceholder: '綁定碼',
      bindButton: '綁定',
      deviceLabel: '已綁定裝置',
      modeLabel: '通知方式',
      subsLabel: '瀏覽器訂閱',
      enableButton: '允許通知',
      testButton: '發送測試',
      modeNative: '僅系統推送（App）',
      modeBoth: '系統推送 + 網頁推送',
      modeWeb: '僅網頁推送',
      unbindTitle: '解除綁定',
      unbindButton: '解除本瀏覽器綁定',
      bindFailed: '綁定失敗：請檢查綁定碼後重試。',
      bound: '綁定成功，請在下方開啟通知。',
      subAdded: '已在此瀏覽器開啟通知。',
      permissionDenied: '通知已被拒絕。請在「設定 → 通知 → CallRelay」中開啟後重試。',
      permissionPending: '等待權限提示…',
      testSent: '測試通知已發送，請留意此裝置。',
      testFailed: '測試發送失敗：請先允許通知後再試。',
      modeSaved: '通知方式已保存。',
      actionFailed: '操作未完成，請稍後重試。',
      removeSub: '移除',
      unknownBrowser: '瀏覽器',
      browserOn: '已開通',
      browserOff: '未開通',
      browserDenied: '通知被阻擋 — 請允許通知後開啟',
      clientBack: '‹ 返回通話 / 簡訊客戶端'
    }
  };
  // zh-Hans is the source language embedded in the markup.
  function lang() {
    var tag = (navigator.language || 'zh-CN').toLowerCase();
    if (tag.indexOf('zh') === 0) {
      return tag.indexOf('hant') !== -1 || tag.indexOf('tw') !== -1 || tag.indexOf('hk') !== -1 ? 'zhhant' : 'zh';
    }
    return 'en';
  }
  var L = lang();
  function t(key) {
    if (L === 'zh') return null; // source strings live in the HTML
    return (DICT[L] && DICT[L][key]) || DICT.en[key] || null;
  }
  function applyI18n() {
    Array.prototype.forEach.call(document.querySelectorAll('[data-i18n]'), function (el) {
      var value = t(el.getAttribute('data-i18n'));
      if (value) el.textContent = value;
    });
    Array.prototype.forEach.call(document.querySelectorAll('[data-i18n-ph]'), function (el) {
      var value = t(el.getAttribute('data-i18n-ph'));
      if (value) el.setAttribute('placeholder', value);
    });
    document.documentElement.lang = L === 'zh' ? 'zh-CN' : (L === 'zhhant' ? 'zh-Hant' : 'en');
  }

  // ---------- views ----------
  function showBind() {
    hide('status-view');
    show('bind-view');
  }

  function urlBase64ToUint8Array(base64String) {
    var padding = '='.repeat((4 - base64String.length % 4) % 4);
    var base64 = (base64String + padding).replace(/-/g, '+').replace(/_/g, '/');
    var raw = atob(base64);
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; ++i) out[i] = raw.charCodeAt(i);
    return out;
  }

  function renderStatus(data) {
    // Build-49: scope the denoise switches to THIS enrolled device (the id
    // is non-secret; only its hash enters the storage key).
    if (window.CallRelayDenoise && data && data.deviceId) {
      window.CallRelayDenoise.setScope(data.deviceId);
    }
    show('status-view');
    hide('bind-view');
    $('st-device').textContent = data.deviceName || '—';
    var mode = data.notifyMode || 'native';
    $('st-mode').textContent = modeText(mode);
    var radios = document.querySelectorAll('input[name="mode"]');
    Array.prototype.forEach.call(radios, function (r) { r.checked = r.value === mode; });
    var subs = data.subscriptions || [];
    $('st-subs').textContent = String(subs.length);
    renderBrowserState();
    var list = $('sub-list');
    list.innerHTML = '';
    subs.forEach(function (sub) {
      var li = document.createElement('li');
      var span = document.createElement('span');
      span.className = 'host';
      span.textContent = (sub.endpointHost || '') + ' · ' + (sub.userAgent || t('unknownBrowser') || '浏览器');
      var btn = document.createElement('button');
      btn.textContent = t('removeSub') || '移除';
      btn.addEventListener('click', function () {
        api('/api/v2/webpush/subscription/' + encodeURIComponent(sub.id), { method: 'DELETE' })
          .then(function (data) {
            // If this browser's own subscription was the one removed, drop
            // the local PushSubscription too so the state line stays honest.
            syncLocalSubscription(data.subscriptions || []);
            renderStatus(data);
          })
          .catch(showActionError);
      });
      li.appendChild(span);
      li.appendChild(btn);
      list.appendChild(li);
    });
  }

  function modeText(mode) {
    var map = { native: 'modeNative', both: 'modeBoth', web: 'modeWeb' };
    return t(map[mode]) || ({ native: '仅系统推送（App）', both: '系统推送 + 网页推送', web: '仅网页推送' })[mode] || mode;
  }

  /// If the local PushSubscription's endpoint no longer appears in the
  /// server-side list (removed here or revoked server-side), unsubscribe
  /// locally so the browser state line reflects reality.
  function syncLocalSubscription(serverSubs) {
    if (!('serviceWorker' in navigator)) return;
    navigator.serviceWorker.getRegistration('/pwa/').then(function (reg) {
      if (!reg || !reg.pushManager) return;
      return reg.pushManager.getSubscription().then(function (sub) {
        if (!sub) return;
        var host = '';
        try { host = new URL(sub.endpoint).host; } catch (e) { return; }
        var stillListed = serverSubs.some(function (s) { return s.endpointHost === host; });
        if (!stillListed) sub.unsubscribe().catch(function () {});
      });
    }).catch(function () {});
  }

  /// This browser's own enablement state — a bound session is NOT "enabled"
  /// until a real PushSubscription exists here. Blocked permission is shown
  /// explicitly so the owner can fix it in system settings.
  function renderBrowserState() {
    var el = $('st-browser');
    function paint(state) {
      if (state === 'on') { el.textContent = t('browserOn') || '已开通'; el.className = 'ok'; }
      else if (state === 'denied') { el.textContent = t('browserDenied') || '通知被阻止'; el.className = 'error'; }
      else { el.textContent = t('browserOff') || '未开通'; el.className = ''; }
    }
    if (!('serviceWorker' in navigator)) { paint('off'); return; }
    navigator.serviceWorker.getRegistration('/pwa/').then(function (reg) {
      if (!reg || !reg.pushManager) { paint('off'); return; }
      return reg.pushManager.getSubscription().then(function (sub) {
        if (sub) { paint('on'); return; }
        paint(window.Notification && Notification.permission === 'denied' ? 'denied' : 'off');
      });
    }).catch(function () { paint('off'); });
  }

  function showActionError(err) {
    var el = $('action-error');
    var message = (err && err.message) ? err.message : '';
    // DOMException messages are engine text ("Error", "AbortError"); never
    // show them raw.
    if (!message || message === 'Error' || /^[A-Za-z]*Error$/.test(message)) {
      message = t('actionFailed') || '操作未完成，请稍后重试。';
    }
    el.textContent = message;
    show('action-error');
    hide('action-ok');
  }
  function showActionOk(message) {
    var el = $('action-ok');
    el.textContent = message;
    show('action-ok');
    hide('action-error');
  }

  function loadStatus() {
    return api('/api/v2/webpush/session').then(renderStatus).catch(function (err) {
      if (err.status === 401) {
        session = null;
        localStorage.removeItem(SESSION_KEY);
        // Revoked session: drop the dead enrollment's denoise choice too.
        if (window.CallRelayDenoise) { window.CallRelayDenoise.clear(); }
        showBind();
        return;
      }
      showBind();
    });
  }

  // ---------- actions ----------
  function bind(code) {
    return api('/api/v2/webpush/bind', {
      method: 'POST',
      body: JSON.stringify({ code: code })
    }).then(function (body) {
      // Only the LEGACY push-scope flow returns a usable bearer. A
      // client-scope session is cookie-only: never store an empty token.
      if (body.sessionToken) {
        session = body.sessionToken;
        localStorage.setItem(SESSION_KEY, session);
      }
      // Land on the status view immediately; the push subscription then
      // resolves (or surfaces its own error) without blocking the UI on a
      // possibly-dismissed permission prompt.
      return loadStatus().then(function () {
        showActionOk(t('bound') || '绑定成功，请在下方开启通知。');
        return registerServiceWorker(body.vapidPublicKey).then(function () {
          showActionOk(t('subAdded') || '已在此浏览器开启通知。');
          return loadStatus();
        }).catch(function (err) {
          if (err && err.name === 'NotAllowedError') {
            showActionError(new Error(t('permissionDenied') || ''));
          } else if (err && err.name === 'AbortError') {
            showActionError(new Error(t('permissionPending') || ''));
          } else {
            showActionError(err);
          }
        });
      });
    }).catch(function () {
      var el = $('bind-error');
      el.textContent = t('bindFailed') || '绑定失败：请检查绑定码后重试。';
      show('bind-error');
    });
  }

  function registerServiceWorker(vapidKey) {
    return navigator.serviceWorker.register('/pwa/sw.js', { scope: '/pwa/' }).then(function (reg) {
      return reg.pushManager.getSubscription().then(function (existing) {
        if (existing) return existing;
        return reg.pushManager.subscribe({
          userVisibleOnly: true,
          applicationServerKey: urlBase64ToUint8Array(vapidKey)
        });
      });
    }).then(function (sub) {
      var json = sub.toJSON();
      return api('/api/v2/webpush/subscription', {
        method: 'PUT',
        body: JSON.stringify({
          endpoint: sub.endpoint,
          keys: { p256dh: json.keys.p256dh, auth: json.keys.auth },
          userAgent: navigator.userAgent
        })
      });
    });
  }

  function enablePush() {
    // iOS grants push permission only from a user gesture: request it
    // IMMEDIATELY in the click, before any async work. Denied stays
    // actionable; granted continues into SW registration + subscribe.
    var permission = Promise.resolve('granted');
    if (window.Notification && typeof Notification.requestPermission === 'function') {
      try {
        permission = Promise.resolve(Notification.requestPermission());
      } catch (e) {
        permission = Promise.reject(e);
      }
    }
    permission.then(function (state) {
      if (state !== 'granted') {
        throw Object.assign(new Error('denied'), { name: 'NotAllowedError' });
      }
      showActionOk(t('permissionPending') || '正在开启通知…');
      return api('/api/v2/webpush/session');
    }).then(function (data) {
      return registerServiceWorker(data.vapidPublicKey).then(function () {
        showActionOk(t('subAdded') || '已在此浏览器开启通知。');
        return loadStatus();
      });
    }).catch(function (err) {
      if (err && err.name === 'NotAllowedError') {
        showActionError(new Error(t('permissionDenied') || ''));
      } else if (err && err.name === 'AbortError') {
        showActionError(new Error(t('permissionPending') || ''));
      } else if (err && err.status === 401) {
        session = null;
        localStorage.removeItem(SESSION_KEY);
        if (window.CallRelayDenoise) { window.CallRelayDenoise.clear(); }
        showBind();
      } else {
        showActionError(err);
      }
    });
  }

  function testPush() {
    api('/api/v2/webpush/test', { method: 'POST', body: '{}' })
      .then(function () { showActionOk(t('testSent') || '测试通知已发送，请留意此装置。'); })
      .catch(function (err) { showActionError(new Error(t('testFailed') || '')); });
  }

  function setMode(mode) {
    api('/api/v2/webpush/notify-mode', { method: 'PUT', body: JSON.stringify({ mode: mode }) })
      .then(function () { showActionOk(t('modeSaved') || '通知方式已保存。'); return loadStatus(); })
      .catch(showActionError);
  }

  function unbind() {
    // Server-backed revocation of THIS browser's session (the HttpOnly
    // cookie would otherwise survive and silently reauthenticate every
    // cold reload). Native devices and other browsers are untouched.
    api('/api/v2/webpush/session/revoke', { method: 'POST' }).catch(function () {
      /* an already-dead session still lands on the bind screen */
    }).then(function () {
      localStorage.removeItem(SESSION_KEY);
      session = null;
      // Build-49: logout drops this enrollment's denoise choice so the next
      // enrollment starts at the default-OFF policy.
      if (window.CallRelayDenoise) { window.CallRelayDenoise.clear(); }
      showBind();
    });
  }

  // Build-49 hot apply: push a switch change to this device's gateway
  // preferences (applied live to its sessions by the gateway). The
  // identity + choice are captured BEFORE the async round trip; a
  // completion that arrives after logout/re-enroll (scope changed) is
  // discarded. A gateway that cannot store the field gets a plain,
  // jargon-free note — the local choice still applies to this browser.
  function setDenoiseStatus(text) {
    var el = document.getElementById('denoise-status');
    if (el) { el.textContent = text || ''; }
  }
  if (typeof self !== 'undefined' && self.addEventListener) {
    self.addEventListener('callrelay:denoise', function () {
      var D = window.CallRelayDenoise;
      if (!D) { return; }
      var capturedScope = D.scope();
      var choice = D.read();
      api('/api/v2/device').then(function (body) {
        var id = body && body.device && body.device.id;
        if (!id) { return; }
        if (D.scope() !== capturedScope) { return; } // stale after logout
        api('/api/v2/devices/' + encodeURIComponent(id) + '/preferences', {
          method: 'PUT',
          body: JSON.stringify({ uplinkDenoise: choice.uplink, downlinkDenoise: choice.downlink }),
        }).then(function () {
          if (D.scope() === capturedScope) { setDenoiseStatus(''); }
        }).catch(function () {
          if (D.scope() !== capturedScope) { return; }
          setDenoiseStatus('网关未能保存，设置仅在本机生效');
        });
      }).catch(function () { /* not enrolled yet: the local choice applies */ });
    });
  }

  // ---------- boot ----------
  function boot() {
    applyI18n();
    if (!supported()) {
      show('unsupported');
      return;
    }
    var match = /#bind=([0-9A-Za-z-]+)/.exec(location.hash || '');
    if (match) {
      $('bind-code').value = match[1];
    }
    if (!isStandalone() && /iP(hone|ad|od)/.test(navigator.userAgent)) {
      show('install-help');
    }
    // Session resolution: legacy bearer first, then the cookie-bound
    // session (standalone client scope included) — the webpush/session
    // route authenticates either, so a cold reopen of this page shows the
    // status (and push enable/unbind) without the native app.
    session = localStorage.getItem(SESSION_KEY);
    loadStatus();

    $('bind-form').addEventListener('submit', function (event) {
      event.preventDefault();
      hide('bind-error');
      bind($('bind-code').value.trim());
    });
    $('enable-push').addEventListener('click', enablePush);
    $('test-push').addEventListener('click', testPush);
    $('unbind').addEventListener('click', unbind);
    Array.prototype.forEach.call(document.querySelectorAll('input[name="mode"]'), function (radio) {
      radio.addEventListener('change', function () { setMode(radio.value); });
    });
    // Build-48/49 directional denoise: two independent switches, default off,
    // persisted per gateway origin and applied by the gateway at the media
    // attach (and live via the WSS control message for an active call).
    if (window.CallRelayDenoise) {
      window.CallRelayDenoise.bind($('denoise-uplink'), $('denoise-downlink'));
    }
  }

  boot();
})();
