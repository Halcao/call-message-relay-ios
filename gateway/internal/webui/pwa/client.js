/* CallRelay standalone PWA client — dialer, SMS, in-browser calls.
 *
 * Transport mirrors the native app exactly: v2 HTTPS APIs (cookie session)
 * + the WSS PCMU media socket. Foreground-only by design: the service
 * worker never holds media; a notification tap brings this page forward,
 * and the mic is requested from the answer/dial user gesture. No fake
 * system-call UI, no background autoconnect claims.
 */
(function () {
  'use strict';

  var S = window.CallRelaySession;
  var AU = window.CallRelayAudio;
  var TL = window.CallRelayThreadLogic;
  var CL = window.CallRelayContactsLogic;
  var $ = function (id) { return document.getElementById(id); };
  var el = function (tag, cls, text) {
    var node = document.createElement(tag);
    if (cls) { node.className = cls; }
    if (text !== undefined) { node.textContent = text; }
    return node;
  };

  var FIXTURE = /[?&]fixture=1/.test(location.search);
  var params = new URLSearchParams(location.search);

  /* Fixture-only demo audio: no mic, no AudioContext — review can exercise
   * the full call flow without permissions (clearly marked demo mode). */
  function DemoPlayer() {}
  DemoPlayer.prototype.start = function () { return Promise.resolve(this); };
  DemoPlayer.prototype.feed = function () {};
  DemoPlayer.prototype.stop = function () {};
  function DemoCapture() {}
  DemoCapture.prototype.start = function () { return Promise.resolve(this); };
  DemoCapture.prototype.stop = function () {};
  function demoMic() {
    return Promise.resolve({ getTracks: function () { return []; } });
  }

  var state = {
    session: null,            // /api/v2/device body
    lines: [],
    defaultLineId: '',
    threadLinePrefs: {},      // threadKey -> lineId (UI preference, localStorage OK)
    peers: [],                // history peers for suggestions (no contacts access)
    threads: [],
    threadsError: false,
    openThreadKey: null,
    calls: [],                // /api/v2/calls history (own tab)
    callsError: false,
    activeTab: 'dialer',
    call: null,               // active call controller
    events: null,
    dialNumber: '',
    smsEditing: false,
    smsSelected: {},          // stable thread key -> true
    smsBusy: false,
    contacts: [],             // principal-scoped address book (gateway)
    contactsHasMore: false,   // honest truncation flag for the list page
    contactsQuery: '',
    contactsPreview: null,    // pending vCard text while previewing an import
    contactsAccess: null,     // last list error (for the empty-state hint)
  };

  try { state.threadLinePrefs = JSON.parse(localStorage.getItem('cr_thread_lines') || '{}'); } catch (e) { /* ignore */ }
  function saveLinePrefs() {
    try { localStorage.setItem('cr_thread_lines', JSON.stringify(state.threadLinePrefs)); } catch (e) { /* ignore */ }
  }

  function toast(text) {
    var node = $('toast');
    node.textContent = text;
    node.classList.remove('hidden');
    // Two frames so the transition runs from the hidden state.
    requestAnimationFrame(function () { node.classList.add('show'); });
    clearTimeout(toast.timer);
    clearTimeout(toast.hideTimer);
    toast.timer = setTimeout(function () {
      node.classList.remove('show');
      toast.hideTimer = setTimeout(function () { node.classList.add('hidden'); }, 200);
    }, 3000);
  }

  /* Empty / failure states: icon, short title, one-line hint. */
  var EMPTY_ICONS = {
    messages: '<svg viewBox="0 0 24 24" width="44" height="44" fill="none" stroke="currentColor" stroke-width="1.3"><path d="M4.5 5.5h15a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H9l-4.5 3.4V7.5a2 2 0 0 1 0-2z" stroke-linejoin="round"/></svg>',
    calls: '<svg viewBox="0 0 24 24" width="44" height="44" fill="none" stroke="currentColor" stroke-width="1.3"><circle cx="12" cy="12" r="8.4"/><path d="M12 7.3V12l3.1 1.9" stroke-linecap="round" stroke-linejoin="round"/></svg>',
    contacts: '<svg viewBox="0 0 24 24" width="44" height="44" fill="none" stroke="currentColor" stroke-width="1.3"><circle cx="12" cy="12" r="8.4"/><circle cx="12" cy="10" r="2.7"/><path d="M6.7 18.4c.8-2.3 2.8-3.8 5.3-3.8s4.5 1.5 5.3 3.8" stroke-linecap="round"/></svg>',
  };

  function emptyState(container, icon, title, hint) {
    var wrap = el('div', 'empty');
    var iconNode = el('div', 'empty-icon');
    iconNode.innerHTML = EMPTY_ICONS[icon] || '';
    wrap.appendChild(iconNode);
    wrap.appendChild(el('div', 'empty-title', title));
    if (hint) { wrap.appendChild(el('div', 'empty-hint', hint)); }
    container.appendChild(wrap);
  }

  /* Native action sheet: the secondary actions of a row live here instead of
   * as a stack of blue text links inside the row. */
  function actionSheet(title, items) {
    var backdrop = el('div', 'sheet-backdrop');
    var sheet = el('div', 'sheet');
    if (title) { sheet.appendChild(el('div', 'sheet-title', title)); }
    var group = el('div', 'sheet-group');
    items.forEach(function (item) {
      var btn = el('button', 'sheet-item' + (item.danger ? ' danger' : ''), item.label);
      btn.type = 'button';
      btn.addEventListener('click', function () {
        close();
        if (item.onSelect) { item.onSelect(); }
      });
      group.appendChild(btn);
    });
    var cancel = el('button', 'sheet-cancel', '取消');
    cancel.type = 'button';
    cancel.addEventListener('click', close);
    sheet.appendChild(group);
    sheet.appendChild(cancel);
    backdrop.appendChild(sheet);
    function close() {
      if (backdrop.parentNode) { backdrop.parentNode.removeChild(backdrop); }
    }
    backdrop.addEventListener('click', function (event) {
      if (event.target === backdrop) { close(); }
    });
    document.body.appendChild(backdrop);
    requestAnimationFrame(function () { backdrop.classList.add('open'); });
  }

  /* Native-style destructive confirmation (no browser confirm() so the
   * wording and destructive role match the in-app pattern). */
  function confirmDialog(title, message, confirmLabel, onConfirm) {
    var backdrop = el('div', 'confirm-backdrop');
    var card = el('div', 'confirm-card');
    card.appendChild(el('strong', null, title));
    card.appendChild(el('p', null, message));
    var actions = el('div', 'actions');
    var cancel = el('button', null, '取消');
    cancel.type = 'button';
    cancel.addEventListener('click', function () { document.body.removeChild(backdrop); });
    var confirm = el('button', 'danger', confirmLabel || '确认');
    confirm.type = 'button';
    confirm.addEventListener('click', function () {
      document.body.removeChild(backdrop);
      onConfirm();
    });
    actions.appendChild(cancel);
    actions.appendChild(confirm);
    card.appendChild(actions);
    backdrop.appendChild(card);
    document.body.appendChild(backdrop);
  }

  /* ---------------- boot ---------------- */

  function boot() {
    wirePairing();
    wireTabs();
    wireDialer();
    wireMessages();
    wireContacts();
    // Fixture-only: force the pairing screen so the no-App primary flow is
    // reviewable in the preview (the fixture otherwise auto-enters main).
    if (FIXTURE && params.get('pair') === '1') {
      $('pair-view').classList.remove('hidden');
      return;
    }
    S.current().then(function (session) {
      if (session) { enterMain(session); }
      else { $('pair-view').classList.remove('hidden'); }
    }).catch(function () {
      $('pair-view').classList.remove('hidden');
      toast('无法连接网关');
    });
  }

  function wirePairing() {
    $('bind-form').addEventListener('submit', function (event) {
      event.preventDefault();
      var code = $('bind-code').value.trim();
      if (!code) { return; }
      S.bind(code).then(function (scope) {
        if (scope !== 'client') {
          showPairError('此绑定码仅开启通知。请在 App 中选择“浏览器客户端”绑定码以使用拨打/短信。');
        }
        return S.current();
      }).then(function (session) {
        if (session) { enterMain(session); }
      }).catch(function (err) {
        showPairError(err.message || '绑定失败');
      });
    });
    $('enroll-btn').addEventListener('click', function () {
      S.enrollStandalone($('enroll-key').value, $('enroll-name').value).then(function () {
        return S.current();
      }).then(function (session) {
        if (session) { enterMain(session); }
      }).catch(function (err) {
        showPairError(err.message || '配对失败');
      });
    });
  }

  function showPairError(message) {
    var node = $('pair-error');
    node.textContent = message;
    node.classList.remove('hidden');
  }

  function enterMain(session) {
    state.session = session;
    $('pair-view').classList.add('hidden');
    $('main-view').classList.remove('hidden');
    refreshLines().then(function () {
      refreshPeers();
      refreshContacts();
      connectEvents();
      return refreshThreads();
    }).then(function () {
      // Fixture-only review hook: deep-link straight into a conversation.
      if (FIXTURE && params.get('autopen') === 'thread' && state.threads.length) {
        showTab('messages');
        var t = state.threads[0];
        if (t) { openThread(t.key, t.peer); }
      }
      // Fixture-only review hook: open a specific tab (used by the rendered
      // headless check to capture the contacts preview surface).
      if (FIXTURE && params.get('autotab')) {
        showTab(params.get('autotab'));
      }
    });
    // 通知点击/前台打开直达通话：?call=<gatewayCallId>
    var params = new URLSearchParams(location.search);
    var callId = params.get('call');
    if (callId) {
      history.replaceState(null, '', FIXTURE ? location.pathname + '?fixture=1' : location.pathname);
      openCallScreen(callId, { incoming: true });
    }
  }

  function refreshLines() {
    return S.api('/api/v2/device').then(function (body) {
      state.session = body;
      // Build-49: scope denoise preferences to THIS enrolled device (hash
      // of the id only; the raw id never enters the storage key).
      if (window.CallRelayDenoise && body && body.device && body.device.id) {
        window.CallRelayDenoise.setScope(body.device.id);
      }
      state.defaultLineId = body.device.defaultLineId || '';
      state.lines = (body.lines || []).filter(function (l) { return l.enabled; });
      fillLinePicker($('line-picker'), preferredLine(''), state.lines);
      fillLinePicker($('compose-from'), null, state.lines.filter(function (l) { return l.permissions.sendSms; }));
      return state.lines;
    }).catch(function (err) {
      if (err.status === 401) {
        // Session revoked: drop the dead enrollment's denoise scope before
        // reloading into the pairing flow.
        if (window.CallRelayDenoise) { window.CallRelayDenoise.clear(); }
        location.reload();
      }
    });
  }

  function fillLinePicker(select, selected, lines, shortNames) {
    select.textContent = '';
    lines.forEach(function (line) {
      var opt = el('option');
      opt.value = line.id;
      opt.textContent = shortNames || !line.phoneNumber
        ? line.name
        : line.name + ' · ' + line.phoneNumber;
      select.appendChild(opt);
    });
    if (selected) { select.value = selected; }
  }

  function smsLines() {
    return state.lines.filter(function (l) { return l.permissions.sendSms; });
  }

  /* Default line for a context, in order: the explicit authorized
   * conversation preference, the EXISTING thread's own line (thread.lineId
   * or the prefix of its validated canonical `lineId:peer` key — so a line2
   * conversation with no saved preference never composes from the global
   * line1), then the authorized global default / first permitted line.
   * Every candidate is checked against the currently permitted lines, so an
   * unavailable or unauthorized SIM is never silently selected. New-compose
   * passes no thread and therefore keeps the global-default behaviour. */
  function preferredLine(threadKey, permitted, threadView) {
    var thread = threadView || (threadKey ? { key: threadKey } : null);
    return TL.preferredLine({
      pref: state.threadLinePrefs[threadKey] || '',
      thread: thread,
      globalDefault: state.defaultLineId,
      permitted: permitted || state.lines,
    });
  }

  /* ---------------- events (foreground only) ---------------- */

  function connectEvents() {
    if (state.events) { state.events.close(); state.events = null; }
    var scheme = location.protocol === 'https:' ? 'wss' : 'ws';
    var ws = new WebSocket(scheme + '://' + location.host + '/api/v2/events');
    state.events = ws;
    ws.onopen = function () {
      // Only the degraded state is surfaced; a healthy stream shows nothing.
      $('conn-dot').className = '';
      checkRingingSnapshot();
    };
    ws.onclose = function () {
      $('conn-dot').className = 'busy';
      setTimeout(connectEvents, 4000);
    };
    ws.onmessage = function (event) {
      var msg;
      try { msg = JSON.parse(event.data); } catch (e) { return; }
      handleEvent(msg);
    };
  }

  function handleEvent(msg) {
    var type = msg.type || '';
    if (type.indexOf('message') === 0) {
      refreshThreads();
      if (state.openThreadKey) { refreshThread(state.openThreadKey); }
      return;
    }
    if (type.indexOf('call.') === 0) {
      // Keep an open history list live; a call that ends while the tab is
      // visible must appear without a manual reload.
      if (type === 'call.ended' && state.activeTab === 'calls') { refreshCalls(); }
      if (state.call) {
        state.call.onEvent(msg);
        return;
      }
      var incoming = window.CallRelayCallEvents.parseIncomingCallEvent(
        msg, state.call ? state.call.callId : null);
      if (incoming) { openCallScreen(incoming.id, { incoming: true, peer: incoming.peer }); }
    }
  }

  /* Authorized ringing snapshot on boot/reconnect: the events stream is
   * permission-filtered server-side; a cold foreground reload still needs
   * to surface an already-ringing call. */
  function checkRingingSnapshot() {
    if (state.call) { return Promise.resolve(); }
    return S.api('/api/v2/calls').then(function (body) {
      if (state.call) { return; }
      var calls = body.calls || body || [];
      var ringing = calls.filter(function (c) {
        // Gateway states are snake_case (`incoming_ringing`); camelCase is
        // kept for older fixtures.
        return (c.state === 'incomingRinging' || c.state === 'incoming_ringing') && c.direction === 'inbound';
      })[0];
      if (ringing) {
        openCallScreen(ringing.id, { incoming: true, peer: ringing.peer || '' });
      }
    }).catch(function () { /* transient */ });
  }

  /* ---------------- dialer ---------------- */

  var KEYS = ['1', '2', '3', '4', '5', '6', '7', '8', '9', '*', '0', '#'];

  function wireDialer() {
    var pad = $('keypad');
    var SUBS = { '2': 'ABC', '3': 'DEF', '4': 'GHI', '5': 'JKL', '6': 'MNO', '7': 'PQRS', '8': 'TUV', '9': 'WXYZ', '0': '+' };
    KEYS.forEach(function (key) {
      var btn = el('button', 'key');
      btn.appendChild(el('span', 'key-num', key));
      if (SUBS[key]) { btn.appendChild(el('span', 'key-sub', SUBS[key])); }
      btn.setAttribute('aria-label', key === '*' ? '星号' : key === '#' ? '井号' : key);
      btn.addEventListener('click', function () { dialAppend(key); });
      if (key === '0') {
        btn.addEventListener('contextmenu', function (event) {
          event.preventDefault();
          dialAppend('+');
        });
      }
      pad.appendChild(btn);
    });
    $('dial-delete').addEventListener('click', function () {
      state.dialNumber = state.dialNumber.slice(0, -1);
      renderDial();
    });
    $('dial-call').addEventListener('click', function () {
      var to = state.dialNumber.trim();
      if (!to) { return; }
      startCall(to, preferredLine(''));
    });
    renderDial();
  }

  function dialAppend(key) {
    if (state.dialNumber.length < 32) {
      state.dialNumber += key;
      renderDial();
    }
  }

  function renderDial() {
    $('dial-display').textContent = state.dialNumber || ' ';
    $('dial-call').disabled = !state.dialNumber.trim();
    renderSuggestions($('dial-suggestions'), state.dialNumber, function (peer) {
      state.dialNumber = peer;
      renderDial();
    });
  }

  /* Suggestions come from the principal-scoped gateway contacts (name,
   * number and the pinyin/T9 search key synced from the phone) plus local
   * history peers. Exact number self-dismisses. */
  function renderSuggestions(container, query, onPick) {
    var matches = [];
    var seen = {};
    function add(peer, name, isNameMatch) {
      if (!peer || seen[peer] || matches.length >= 4) { return; }
      seen[peer] = true;
      matches.push({ peer: peer, name: name || peer, isNameMatch: Boolean(isNameMatch) });
    }
    if (CL) {
      CL.suggestions(state.contacts, query, 6).forEach(function (hit) {
        add(hit.phone, hit.name, hit.isNameMatch);
      });
    }
    var q = query.replace(/\D/g, '');
    if (q) {
      state.peers.forEach(function (peer) {
        var digits = peer.replace(/\D/g, '');
        if (digits.indexOf(q) >= 0 && digits !== q) {
          add(peer, peerName(peer) || peer, false);
        }
      });
    }
    container.textContent = '';
    if (!matches.length) { container.classList.add('hidden'); return; }
    matches.forEach(function (match) {
      var btn = el('button', 'suggestion');
      var avatar = el('span', 'avatar', (match.name || '?').charAt(0));
      var body = el('span');
      body.appendChild(el('div', 's-name', match.name));
      body.appendChild(el('div', 's-phone', match.peer));
      btn.appendChild(avatar);
      btn.appendChild(body);
      btn.addEventListener('click', function () { onPick(match.peer); });
      container.appendChild(btn);
    });
    container.classList.remove('hidden');
  }

  function refreshPeers() {
    return Promise.all([
      S.api('/api/v2/threads').catch(function () { return []; }),
      S.api('/api/v2/calls?limit=50').catch(function () { return []; }),
    ]).then(function (results) {
      var set = {};
      results[0].forEach(function (t) { set[t.peer] = true; });
      (results[1].calls || results[1] || []).forEach(function (c) { set[c.peer] = true; });
      state.peers = Object.keys(set);
    });
  }

  function peerName(peer) {
    if (CL) {
      var contact = CL.contactForPhone(state.contacts, peer);
      if (contact) { return CL.displayName(contact); }
    }
    var thread = state.threads.filter(function (t) { return t.peer === peer; })[0];
    return thread ? thread.displayName || peer : null;
  }

  /* ---------------- SMS ---------------- */

  function wireMessages() {
    $('line-picker').addEventListener('change', function () {
      var lineId = $('line-picker').value;
      state.defaultLineId = lineId;
      refreshThreads();
      // Persist the default line to THIS device on the gateway (the same
      // authorized endpoint the native app uses), so other clients and a
      // later session see the same default. A failure is reported and the
      // local selection stays for this session.
      var deviceId = state.session && state.session.device && state.session.device.id;
      if (!deviceId || !lineId) { return; }
      S.api('/api/v2/devices/' + encodeURIComponent(deviceId) + '/preferences', {
        method: 'PUT',
        body: { defaultLineId: lineId },
      }).then(function () {
        toast('默认线路已保存');
      }).catch(function (err) {
        toast('默认线路保存失败：' + (err.message || '请重试'));
      });
    });
    $('thread-back').addEventListener('click', function () {
      state.openThreadKey = null;
      $('thread-view').classList.add('hidden');
      $('thread-list').classList.remove('hidden');
      $('tabbar').classList.remove('hidden');
      $('topbar').classList.remove('hidden');
    });
    $('thread-call').addEventListener('click', function () {
      var thread = currentThread();
      if (thread) {
        startCall(thread.peer, preferredLine(thread.key, state.lines, thread));
      }
    });
    $('composer-input').addEventListener('input', renderSendReady);
    $('composer-send').addEventListener('click', sendFromComposer);
    $('compose-cancel').addEventListener('click', closeCompose);
    $('compose-to').addEventListener('input', function () {
      renderComposeReady();
      renderSuggestions($('compose-suggestions'), $('compose-to').value, function (peer) {
        $('compose-to').value = peer;
        $('compose-suggestions').classList.add('hidden');
        renderComposeReady();
      });
    });
    $('compose-send').addEventListener('click', sendFromCompose);
    $('compose-body').addEventListener('input', renderComposeReady);
    // Compose is the floating button inside the messages tab (iOS Messages
    // convention: actions belong to their tab, not to the global tab bar).
    $('messages-compose').addEventListener('click', openCompose);

    // SMS management: native Edit mode (multi-select / select-all / bulk
    // mark-read / bulk delete) plus the explicit per-conversation menu.
    $('messages-edit').addEventListener('click', function () {
      setSmsEditing(!state.smsEditing);
    });
    $('messages-select-all').addEventListener('click', toggleSelectAll);
    $('sms-mark-read').addEventListener('click', function () {
      confirmDialog('标记为已读？', '将选中的 ' + selectedKeys().length + ' 个对话标记为已读。',
        '标记已读', function () { bulkMarkRead(); });
    });
    $('sms-delete').addEventListener('click', function () {
      confirmDialog('删除选中的 ' + selectedKeys().length + ' 个对话？',
        '删除后所有绑定设备将不再显示这些对话历史。', '删除对话', function () { bulkDelete(); });
    });
    $('thread-menu').addEventListener('click', function () {
      var thread = currentThread();
      if (!thread) { return; }
      confirmDialog('删除与 ' + threadLabel(thread) + ' 的对话？',
        '删除后所有绑定设备将不再显示这段对话历史。', '删除对话', function () {
          deleteThreads([thread.key]);
        });
    });
  }

  /* ---------------- SMS edit mode ---------------- */

  function selectedKeys() {
    return state.threads.map(function (t) { return t.key; })
      .filter(function (key) { return state.smsSelected[key]; });
  }

  function setSmsEditing(on) {
    state.smsEditing = Boolean(on);
    if (!state.smsEditing) { state.smsSelected = {}; }
    $('messages-edit').textContent = state.smsEditing ? '完成' : '编辑';
    $('messages-select-all').classList.toggle('hidden', !state.smsEditing);
    $('sms-editbar').classList.toggle('hidden', !state.smsEditing);
    // The floating compose button would sit on top of the bulk-action bar.
    $('messages-compose').classList.toggle('hidden', state.smsEditing);
    // Personal-settings shortcuts step out during the focused edit mode.
    $('rules-link').classList.toggle('hidden', state.smsEditing);
    $('settings-link').classList.toggle('hidden', state.smsEditing);
    renderThreadList();
  }

  function toggleSelectAll() {
    var keys = state.threads.map(function (t) { return t.key; });
    var all = keys.length > 0 && keys.every(function (k) { return state.smsSelected[k]; });
    state.smsSelected = {};
    if (!all) { keys.forEach(function (k) { state.smsSelected[k] = true; }); }
    $('messages-select-all').textContent = all ? '全选' : '取消全选';
    renderThreadList();
  }

  function updateEditBar() {
    var count = selectedKeys().length;
    $('sms-delete').textContent = count ? ('删除 (' + count + ')') : '删除';
    $('sms-mark-read').disabled = count === 0 || state.smsBusy;
    $('sms-delete').disabled = count === 0 || state.smsBusy;
  }

  /* Marks one conversation's inbound unread messages read (the same
   * per-message authorized endpoint the native app uses); the gateway
   * broadcasts message.updated so other devices converge. */
  function markThreadRead(key, silent) {
    return S.api('/api/v2/messages?threadKey=' + encodeURIComponent(key) + '&limit=100')
      .then(function (page) {
        var unread = (page.messages || page || []).filter(function (m) {
          return m.direction === 'inbound' && m.status !== 'read' && m.id;
        });
        var chain = Promise.resolve();
        unread.forEach(function (m) {
          chain = chain.then(function () {
            return S.api('/api/v2/messages/' + encodeURIComponent(m.id) + '/read',
              { method: 'POST' })
              .catch(function (err) {
                if (!silent) { throw err; }
              });
          });
        });
        return chain.then(function () { return unread.length; });
      });
  }

  function bulkMarkRead() {
    if (state.smsBusy) { return; }
    var keys = selectedKeys();
    if (!keys.length) { return; }
    state.smsBusy = true;
    updateEditBar();
    var ok = 0;
    var failed = 0;
    var chain = Promise.resolve();
    keys.forEach(function (key) {
      chain = chain.then(function () {
        return markThreadRead(key).then(function () { ok += 1; })
          .catch(function () { failed += 1; });
      });
    });
    chain.then(function () {
      state.smsBusy = false;
      state.smsSelected = {};
      setSmsEditing(false);
      return refreshThreads();
    }).then(function () {
      if (failed) {
        toast('已标记 ' + ok + ' 个对话，' + failed + ' 个失败，请重试。');
      } else {
        toast('已将 ' + ok + ' 个对话标记为已读。');
      }
    });
  }

  function deleteThreads(keys) {
    if (state.smsBusy || !keys.length) { return; }
    state.smsBusy = true;
    updateEditBar();
    var ok = 0;
    var failed = 0;
    var chain = Promise.resolve();
    keys.forEach(function (key) {
      chain = chain.then(function () {
        return S.api('/api/v2/threads/' + encodeURIComponent(key), { method: 'DELETE' })
          .then(function () {
            ok += 1;
            // Remove only AFTER the gateway accepted the tombstone.
            state.threads = state.threads.filter(function (t) { return t.key !== key; });
            delete state.smsSelected[key];
            if (state.openThreadKey === key) {
              state.openThreadKey = null;
              $('thread-view').classList.add('hidden');
              $('thread-list').classList.remove('hidden');
              $('tabbar').classList.remove('hidden');
              $('topbar').classList.remove('hidden');
            }
          })
          .catch(function () { failed += 1; });
      });
    });
    chain.then(function () {
      state.smsBusy = false;
      renderThreadList();
      updateEditBar();
      if (!failed) { setSmsEditing(false); }
      return refreshThreads();
    }).then(function () {
      if (failed) {
        toast(ok ? ('已删除 ' + ok + ' 个对话，' + failed + ' 个删除失败，请重试。')
                 : '删除失败，请检查网络后重试。');
      } else {
        toast('已删除 ' + ok + ' 个对话。');
      }
    });
  }

  function renderSendReady() {
    var ready = $('composer-input').value.trim().length > 0;
    $('composer-send').classList.toggle('ready', ready);
  }

  function renderComposeReady() {
    var ready = Boolean($('compose-to').value.trim() && $('compose-body').value.trim());
    $('compose-send').classList.toggle('ready', ready);
  }

  function currentThread() {
    return state.threads.filter(function (t) { return t.key === state.openThreadKey; })[0] || null;
  }

  function threadLabel(thread) {
    return thread.displayName || peerName(thread.peer) || thread.peer;
  }

  function refreshThreads() {
    return S.api('/api/v2/threads').then(function (threads) {
      state.threads = threads || [];
      state.threadsError = false;
      renderThreadList();
      refreshPeers();
      $('messages-edit').classList.toggle('hidden',
        state.activeTab !== 'messages' || state.threads.length === 0);
    }).catch(function () {
      state.threadsError = true;
      if (state.activeTab === 'messages') { renderThreadList(); }
    });
  }

  function renderThreadList() {
    var list = $('thread-list');
    list.textContent = '';
    // Selection uses stable thread keys; a refresh/arrival race can only
    // drop keys that are no longer visible, never shift an action.
    var visible = {};
    state.threads.forEach(function (t) { visible[t.key] = true; });
    Object.keys(state.smsSelected).forEach(function (key) {
      if (!visible[key]) { delete state.smsSelected[key]; }
    });
    updateEditBar();
    if (!state.threads.length) {
      if (state.threadsError) {
        emptyState(list, 'messages', '无法加载短信', '请检查网络，切换到其他标签后再回来重试。');
      } else {
        emptyState(list, 'messages', '暂无短信', '点右下角的 ＋ 开始新对话。');
      }
      return;
    }
    state.threads.forEach(function (thread) {
      var selected = Boolean(state.smsEditing && state.smsSelected[thread.key]);
      var label = threadLabel(thread);
      var btn = el('button', 'thread-row' + (selected ? ' selected' : ''));
      if (state.smsEditing) {
        btn.appendChild(el('span', 'thread-check', selected ? '✓' : ''));
      } else {
        btn.appendChild(el('span', 'avatar', (label || '?').charAt(0)));
      }
      var body = el('span', 't-body');
      var name = el('div', 't-name', label);
      var last = thread.lastMessage || {};
      var preview = (last.direction === 'outbound' ? '✓ ' : '') + (last.body || '');
      body.appendChild(name);
      body.appendChild(el('div', 't-preview', preview));
      btn.appendChild(body);
      var meta = el('span', 't-meta');
      meta.appendChild(el('span', 't-time', formatTime(last.createdAt)));
      if (thread.unreadCount > 0) {
        meta.appendChild(el('span', 't-unread', String(Math.min(thread.unreadCount, 99))));
      }
      btn.appendChild(meta);
      btn.addEventListener('click', function () {
        if (state.smsEditing) {
          if (state.smsSelected[thread.key]) { delete state.smsSelected[thread.key]; }
          else { state.smsSelected[thread.key] = true; }
          renderThreadList();
          return;
        }
        openThread(thread.key, thread.peer);
      });
      list.appendChild(btn);
    });
  }

  function openThread(key, peer) {
    state.openThreadKey = key;
    $('thread-list').classList.add('hidden');
    $('compose-view').classList.add('hidden');
    $('thread-view').classList.remove('hidden');
    // Conversation hides the global tab bar + global line picker (native
    // Messages convention); the line lives in this header instead.
    $('tabbar').classList.add('hidden');
    $('topbar').classList.add('hidden');
    var threadView = state.threads.filter(function (t) { return t.key === key; })[0] || null;
    fillLinePicker($('thread-line'),
      preferredLine(key, smsLines(), threadView), smsLines(), true);
    // A manual pick is a CONVERSATION preference: it survives the canonical
    // re-open after a cross-line send, so the next message keeps this line.
    $('thread-line').onchange = function () {
      state.threadLinePrefs[key] = $('thread-line').value;
      saveLinePrefs();
    };
    var label = peerName(peer) || peer;
    $('thread-name').textContent = label;
    $('thread-avatar').textContent = (label || '?').charAt(0);
    // Opening a conversation marks its inbound unread messages read, exactly
    // like the native app (per-message authorized endpoint; failures stay
    // unread and silent so the badge remains honest).
    refreshThread(key).then(function () {
      return markThreadRead(key, true);
    }).then(function () {
      return refreshThreads();
    });
  }

  function refreshThread(key) {
    return S.api('/api/v2/messages?threadKey=' + encodeURIComponent(key) + '&limit=100')
      .then(function (page) {
        renderMessages(page.messages || page || []);
      }).catch(function () { /* keep existing rows */ });
  }

  function renderMessages(messages) {
    var box = $('thread-messages');
    box.textContent = '';
    var lastDate = '';
    (messages || []).forEach(function (msg) {
      var date = new Date(msg.createdAt);
      var day = date.toDateString();
      if (day !== lastDate) {
        box.appendChild(el('div', 'date-sep', formatDayStamp(date)));
        lastDate = day;
      }
      var wrap = el('div', 'msg ' + (msg.direction === 'outbound' ? 'out' : 'in'));
      var stack = el('div', 'stack');
      var bubble = el('div', 'bubble', msg.body);
      stack.appendChild(bubble);
      if (msg.direction === 'outbound') {
        var status = el('span', 'status ' + (msg.status === 'failed' ? 'failed' : ''),
          statusText(msg.status));
        stack.appendChild(status);
        if (msg.status === 'failed') {
          var retry = el('button', 'retry', '重试');
          retry.addEventListener('click', function () { resendFailed(msg); });
          stack.appendChild(retry);
        }
      }
      wrap.appendChild(stack);
      box.appendChild(wrap);
    });
    box.scrollTop = box.scrollHeight;
  }

  function statusText(status) {
    switch (status) {
      case 'queued': return '排队中…';
      case 'submitted': return '已提交';
      case 'sent': return '已发送';
      case 'failed': return '发送失败';
      case 'read': return '已读';
      default: return '';
    }
  }

  var composerInFlight = false;
  function sendFromComposer() {
    var thread = currentThread();
    if (!thread || composerInFlight) { return; }
    var body = $('composer-input').value.trim();
    if (!body) { return; }
    // Capture the HEADER picker's actual value (a cross-line switch changes
    // only this select, not state.defaultLineId).
    var lineId = TL.chosenLine($('thread-line').value,
      preferredLine(thread.key, smsLines(), thread));
    composerInFlight = true;
    $('composer-send').disabled = true;
    postMessage(thread.peer, body, lineId).then(function (record) {
      $('composer-input').value = '';
      renderSendReady();
      // The message lands on the selected line's canonical thread (the
      // returned MessageView.threadKey is authoritative). Carry the chosen
      // line to BOTH keys so the reopened conversation defaults to ITS OWN
      // line — not the global default — and the next send stays on it.
      var canonical = TL.canonicalKey(record, lineId, thread.peer);
      TL.carryPreference(state.threadLinePrefs, thread.key, canonical, lineId);
      saveLinePrefs();
      if (canonical !== state.openThreadKey) {
        return refreshThreads().then(function () { openThread(canonical, thread.peer); });
      }
      // Same key: re-sync the picker from the persisted preference too.
      fillLinePicker($('thread-line'), lineId,
        state.lines.filter(function (l) { return l.permissions.sendSms; }), true);
      return refreshThread(canonical);
    }).catch(function (err) {
      toast(err.message || '发送失败');
    }).then(function () {
      composerInFlight = false;
      $('composer-send').disabled = false;
    });
  }

  function sendFromCompose() {
    var to = $('compose-to').value.trim();
    var body = $('compose-body').value.trim();
    if (!to || !body) { return; }
    var lineId = TL.chosenLine($('compose-from').value, preferredLine(to));
    postMessage(to, body, lineId).then(function (record) {
      $('compose-to').value = '';
      $('compose-body').value = '';
      renderComposeReady();
      closeCompose();
      // Follow the returned canonical thread and persist the chosen line
      // under ITS key so the header picker does not snap to the default.
      var canonical = TL.canonicalKey(record, lineId, to);
      TL.carryPreference(state.threadLinePrefs, canonical, canonical, lineId);
      saveLinePrefs();
      refreshThreads().then(function () { openThread(canonical, to); });
    }).catch(function (err) {
      toast(err.message || '发送失败');
    });
  }

  function postMessage(to, body, lineId) {
    return S.api('/api/v2/messages', {
      method: 'POST',
      headers: { 'Idempotency-Key': 'web-' + Date.now() + '-' + Math.random().toString(36).slice(2, 10) },
      body: { to: to, body: body, lineId: lineId || undefined },
    });
  }

  function resendFailed(msg) {
    // Retry the message's ORIGINAL line (MessageView.lineId); only records
    // without provenance fall back to the conversation preference (which
    // itself prefers the thread's own line over the global default).
    var lineId = TL.retryLine(msg, preferredLine(msg.threadKey, smsLines()));
    postMessage(msg.peer, msg.body, lineId).then(function () {
      refreshThread(msg.threadKey);
    }).catch(function (err) {
      toast(err.message || '重试失败');
    });
  }

  function openCompose() {
    var senders = state.lines.filter(function (l) { return l.permissions.sendSms; });
    // Nothing to send from: say so instead of opening a composer with an
    // empty line picker that fails on send.
    if (!senders.length) { toast('当前设备没有可发送短信的线路权限'); return; }
    // The composer is an overlay INSIDE #tab-messages. Showing it without
    // switching to that tab (which is display:none from any other tab) hid
    // both the top bar and the tab bar and left a blank, unrecoverable
    // screen — the "进去出不来" report.
    showTab('messages');
    $('thread-list').classList.add('hidden');
    $('thread-view').classList.add('hidden');
    $('compose-view').classList.remove('hidden');
    $('tabbar').classList.add('hidden');
    $('topbar').classList.add('hidden');
    $('messages-compose').classList.add('hidden');
    fillLinePicker($('compose-from'), preferredLine(''), senders);
    renderComposeReady();
    $('compose-to').focus();
  }

  function closeCompose() {
    $('compose-view').classList.add('hidden');
    $('thread-view').classList.add('hidden');
    $('thread-list').classList.remove('hidden');
    $('tabbar').classList.remove('hidden');
    $('topbar').classList.remove('hidden');
    $('messages-compose').classList.remove('hidden');
    showTab('messages');
  }

  /* ---------------- contacts ---------------- */

  function wireContacts() {
    var searchTimer = null;
    $('contacts-search').addEventListener('input', function () {
      clearTimeout(searchTimer);
      searchTimer = setTimeout(function () {
        state.contactsQuery = $('contacts-search').value.trim();
        refreshContacts();
      }, 250);
    });
    $('contacts-import').addEventListener('click', function () { $('contacts-file').click(); });
    $('contacts-file').addEventListener('change', handleContactsFile);
    $('contacts-export').addEventListener('click', exportContacts);
    $('contacts-preview-cancel').addEventListener('click', hideContactsPreview);
    $('contacts-preview-commit').addEventListener('click', commitContactsImport);
  }

  function refreshContacts() {
    var path = '/api/v2/contacts?limit=1000';
    if (state.contactsQuery) { path += '&query=' + encodeURIComponent(state.contactsQuery); }
    return S.api(path).then(function (body) {
      state.contacts = body.contacts || [];
      state.contactsHasMore = Boolean(body.hasMore);
      state.contactsAccess = null;
      renderContacts();
    }).catch(function (err) {
      state.contactsAccess = err.status || -1;
      renderContacts();
    });
  }

  function renderContacts() {
    var list = $('contacts-list');
    var note = $('contacts-note');
    list.textContent = '';
    if (state.contactsAccess) {
      emptyState(list, 'contacts', '无法加载通讯录', state.contactsAccess === 401
        ? '登录状态已失效，请重新打开页面。'
        : '请检查网络，切换到其他标签后再回来重试。');
      note.classList.add('hidden');
      return;
    }
    if (!state.contacts.length) {
      emptyState(list, 'contacts',
        state.contactsQuery ? '没有匹配的联系人' : '暂无联系人',
        state.contactsQuery
          ? '换个姓名、号码或拼音试试。'
          : '可导入 .vcf 文件，或在手机 App 中同步系统通讯录。');
    }
    state.contacts.forEach(function (contact) {
      list.appendChild(contactRow(contact));
    });
    if (state.contactsHasMore) {
      note.textContent = '联系人较多，仅显示前 1000 位；可用搜索查找其余联系人。';
      note.classList.remove('hidden');
    } else {
      note.classList.add('hidden');
    }
  }

  var CONTACT_CALL_ICON = '<svg viewBox="0 0 24 24" width="19" height="19" aria-hidden="true"><path fill="currentColor" d="M6.6 10.8c1.4 2.8 3.8 5.1 6.6 6.6l2.2-2.2c.3-.3.7-.4 1-.2 1.1.4 2.3.6 3.6.6.6 0 1 .4 1 1V20c0 .6-.4 1-1 1C10.8 21 3 13.2 3 4c0-.6.4-1 1-1h3.5c.6 0 1 .4 1 1 0 1.2.2 2.4.6 3.6.1.3 0 .7-.2 1l-2.3 2.2z"/></svg>';
  var CONTACT_SMS_ICON = '<svg viewBox="0 0 24 24" width="19" height="19" aria-hidden="true"><path fill="currentColor" d="M12 3.4c-5.2 0-9.4 3.5-9.4 7.9 0 2.5 1.4 4.7 3.6 6.1-.1 1.2-.7 2.3-1.7 3.2 1.7-.1 3.2-.7 4.4-1.7.9.2 2 .3 3.1.3 5.2 0 9.4-3.5 9.4-7.9S17.2 3.4 12 3.4z"/></svg>';
  var MORE_ICON = '<svg viewBox="0 0 24 24" width="20" height="20" aria-hidden="true"><circle cx="5" cy="12" r="1.8" fill="currentColor"/><circle cx="12" cy="12" r="1.8" fill="currentColor"/><circle cx="19" cy="12" r="1.8" fill="currentColor"/></svg>';

  function contactRow(contact) {
    var label = CL.displayName(contact);
    var phones = CL.phoneValues(contact);
    var row = el('div', 'contact-row');
    row.appendChild(el('span', 'avatar', (label || '?').charAt(0)));
    var body = el('span', 'c-body');
    body.appendChild(el('div', 'c-name', label));
    body.appendChild(el('div', 'c-phone muted', phones.length ? phones[0] : '无号码'));
    if (contact.source === 'sync') {
      body.appendChild(el('div', 'c-source', '手机同步'));
    }
    row.appendChild(body);

    var actions = el('span', 'c-actions');
    if (phones.length) {
      var call = el('button');
      call.type = 'button';
      call.setAttribute('aria-label', '呼叫 ' + label);
      call.innerHTML = CONTACT_CALL_ICON;
      call.addEventListener('click', function (event) {
        event.stopPropagation();
        startCall(phones[0], preferredLine(''));
      });
      actions.appendChild(call);
      var sms = el('button');
      sms.type = 'button';
      sms.setAttribute('aria-label', '给 ' + label + ' 发短信');
      sms.innerHTML = CONTACT_SMS_ICON;
      sms.addEventListener('click', function (event) {
        event.stopPropagation();
        openComposeFor(phones[0]);
      });
      actions.appendChild(sms);
    }
    var more = el('button', 'more');
    more.type = 'button';
    more.setAttribute('aria-label', label + ' 的更多操作');
    more.innerHTML = MORE_ICON;
    more.addEventListener('click', function (event) {
      event.stopPropagation();
      openContactMenu(contact);
    });
    actions.appendChild(more);
    row.appendChild(actions);
    row.addEventListener('click', function () { openContactMenu(contact); });
    return row;
  }

  function openContactMenu(contact) {
    var label = CL.displayName(contact);
    var phones = CL.phoneValues(contact);
    var items = [];
    if (phones.length) {
      items.push({ label: '呼叫 ' + phones[0], onSelect: function () { startCall(phones[0], preferredLine('')); } });
      items.push({ label: '发送短信', onSelect: function () { openComposeFor(phones[0]); } });
    }
    items.push({ label: '编辑联系人', onSelect: function () { openContactEditor(contact); } });
    items.push({
      label: '删除联系人', danger: true,
      onSelect: function () {
        confirmDialog('删除联系人', '将从网关通讯录中删除「' + label + '」，手机系统通讯录不受影响。', '删除', function () {
          S.api('/api/v2/contacts/' + encodeURIComponent(contact.id), { method: 'DELETE' }).then(function () {
            toast('已删除');
            refreshContacts();
          }).catch(function (err) { toast(err.message || '删除失败'); });
        });
      },
    });
    actionSheet(label, items);
  }

  function openComposeFor(phone) {
    showTab('messages');
    openCompose();
    $('compose-to').value = phone;
  }

  function openContactEditor(contact) {
    var backdrop = el('div', 'confirm-backdrop');
    var card = el('div', 'confirm-card');
    card.appendChild(el('strong', null, '编辑联系人'));
    var nameInput = el('input');
    nameInput.value = CL.displayLabel(contact) || '';
    nameInput.placeholder = '姓名';
    var phoneInput = el('input');
    var phones = CL.phoneValues(contact);
    phoneInput.value = phones.length ? phones[0] : '';
    phoneInput.placeholder = '号码';
    phoneInput.inputMode = 'tel';
    card.appendChild(nameInput);
    card.appendChild(phoneInput);
    var error = el('p', 'error hidden');
    card.appendChild(error);
    var actions = el('div', 'actions');
    var cancel = el('button', null, '取消');
    cancel.type = 'button';
    cancel.addEventListener('click', function () { document.body.removeChild(backdrop); });
    var save = el('button', 'primary', '保存');
    save.type = 'button';
    save.addEventListener('click', function () {
      var updated = {
        displayName: nameInput.value.trim(),
      };
      var remaining = (contact.phones || []).slice(1);
      if (phoneInput.value.trim()) {
        updated.phones = [{ label: (contact.phones && contact.phones[0] && contact.phones[0].label) || 'CELL', value: phoneInput.value.trim() }];
        remaining.forEach(function (phone) { updated.phones.push(phone); });
      } else {
        updated.phones = remaining;
      }
      S.api('/api/v2/contacts/' + encodeURIComponent(contact.id), { method: 'PUT', body: updated }).then(function () {
        document.body.removeChild(backdrop);
        toast('已保存');
        refreshContacts();
      }).catch(function (err) {
        error.textContent = err.message || '保存失败';
        error.classList.remove('hidden');
      });
    });
    actions.appendChild(cancel);
    actions.appendChild(save);
    card.appendChild(actions);
    backdrop.appendChild(card);
    document.body.appendChild(backdrop);
  }

  function handleContactsFile() {
    var input = $('contacts-file');
    var file = input.files && input.files[0];
    input.value = '';
    if (!file) { return; }
    if (file.size > 512 * 1024) {
      toast('文件过大：vCard 最大 512 KB');
      return;
    }
    var reader = new FileReader();
    reader.onload = function () { previewContactsImport(String(reader.result || '')); };
    reader.onerror = function () { toast('读取文件失败'); };
    reader.readAsText(file, 'utf-8');
  }

  function previewContactsImport(text) {
    if (!text.trim()) { toast('文件内容为空'); return; }
    S.api('/api/v2/contacts/import', { method: 'POST', body: { vcard: text, preview: true } }).then(function (result) {
      state.contactsPreview = text;
      $('contacts-preview-title').textContent = '导入预览';
      var summary = CL.importSummary(result);
      if (result.warnings && result.warnings.length) {
        summary += '（' + result.warnings.slice(0, 2).join('；') + (result.warnings.length > 2 ? '…' : '') + '）';
      }
      $('contacts-preview-summary').textContent = summary;
      var list = $('contacts-preview-list');
      list.textContent = '';
      (result.entries || []).slice(0, 200).forEach(function (entry) {
        var row = el('div', 'preview-row');
        row.appendChild(el('span', 'preview-text', CL.entryText(entry)));
        if (entry.changes && entry.changes.length) {
          row.appendChild(el('span', 'muted preview-changes', entry.changes.join('、')));
        }
        list.appendChild(row);
      });
      if ((result.entries || []).length > 200) {
        list.appendChild(el('p', 'muted', '仅展示前 200 条，导入时会处理全部。'));
      }
      $('contacts-preview').classList.remove('hidden');
    }).catch(function (err) {
      toast(err.message || '解析失败');
    });
  }

  function hideContactsPreview() {
    state.contactsPreview = null;
    $('contacts-preview').classList.add('hidden');
  }

  function commitContactsImport() {
    if (!state.contactsPreview) { return; }
    var text = state.contactsPreview;
    S.api('/api/v2/contacts/import', { method: 'POST', body: { vcard: text } }).then(function (result) {
      hideContactsPreview();
      toast('导入完成：' + CL.importSummary(result));
      refreshContacts();
    }).catch(function (err) {
      toast(err.message || '导入失败');
    });
  }

  function exportContacts() {
    fetch('/api/v2/contacts/export', { credentials: 'same-origin' }).then(function (resp) {
      if (!resp.ok) { throw new Error('导出失败'); }
      return resp.blob();
    }).then(function (blob) {
      var url = URL.createObjectURL(blob);
      var link = el('a');
      link.href = url;
      link.download = 'callrelay-contacts.vcf';
      document.body.appendChild(link);
      link.click();
      document.body.removeChild(link);
      setTimeout(function () { URL.revokeObjectURL(url); }, 5000);
    }).catch(function (err) { toast(err.message || '导出失败'); });
  }

  /* ---------------- calls ---------------- */

  function startCall(to, lineId) {
    ensureAudioSupport();
    // Open the call screen and START gesture-phase audio preparation
    // synchronously; the gateway call is created by CallFlow only after the
    // microphone is ready (a denied mic must never dial). `null` callId is
    // filled in from the dial response inside the flow.
    openCallScreen(null, {
      outgoing: true,
      peer: to,
      dial: function () {
        var token = 'web-' + Date.now() + '-' + Math.random().toString(36).slice(2, 8);
        return S.api('/api/v2/calls', {
          method: 'POST',
          headers: { 'Idempotency-Key': 'web-dial-' + token },
          body: { to: to, lineId: lineId || undefined, clientCallId: token },
        });
      },
    });
  }

  function ensureAudioSupport() {
    var webrtcOK = Boolean(window.CallRelayWebRTC && window.CallRelayWebRTC.supported());
    if (!webrtcOK && !AU.PcmuPlayer.supported()) {
      toast('此浏览器不支持网页通话音频');
    }
    return webrtcOK;
  }

  /* Fixture-only engine so the WebRTC label/fallback paths are reviewable
   * without a gateway or a real peer connection. `?media=webrtc` succeeds,
   * `?media=fallback` fails once (the flow must then use WSS). */
  function DemoWebRTC(failOnce) {
    this.stream = { getTracks: function () { return []; }, getAudioTracks: function () { return []; } };
    this.muted = false;
    this.failOnce = Boolean(failOnce);
  }
  DemoWebRTC.prototype.prepare = function () { return Promise.resolve(this); };
  DemoWebRTC.prototype.start = function () {
    if (this.failOnce) { this.failOnce = false; return Promise.reject(new Error('fixture webrtc failure')); }
    return Promise.resolve();
  };
  DemoWebRTC.prototype.setMuted = function (muted) { this.muted = Boolean(muted); };
  DemoWebRTC.prototype.release = function () {};
  DemoWebRTC.prototype.stop = function () {};

  /* Browser-native WebRTC media factory (reuses the gateway's existing
   * direct offer/answer path). Null when the browser lacks WebRTC — the
   * flow then uses the WSS PCMU transport exactly as before. */
  function makeWebRTCFactory() {
    if (FIXTURE) {
      var mode = params.get('media');
      if (mode === 'webrtc') { return function () { return new DemoWebRTC(false); }; }
      if (mode === 'fallback') { return function () { return new DemoWebRTC(true); }; }
      return null;
    }
    if (!window.CallRelayWebRTC || !window.CallRelayWebRTC.supported()) { return null; }
    return function (id) {
      return new window.CallRelayWebRTC.WebRTCMedia({
        api: function (method, path, body) { return S.api(path, { method: method, body: body }); },
        callId: id,
        getMic: function () {
          return navigator.mediaDevices.getUserMedia({
            audio: { echoCancellation: true, noiseSuppression: true, channelCount: 1 },
          });
        },
      });
    };
  }

  /* Call screen backed by CallFlow (ordering + teardown guarantees). */
  var PHONE_UP = '<svg viewBox="0 0 24 24" width="30" height="30"><path fill="currentColor" d="M6.6 10.8c1.4 2.8 3.8 5.1 6.6 6.6l2.2-2.2c.3-.3.7-.4 1-.2 1.1.4 2.3.6 3.6.6.6 0 1 .4 1 1V20c0 .6-.4 1-1 1C10.8 21 3 13.2 3 4c0-.6.4-1 1-1h3.5c.6 0 1 .4 1 1 0 1.2.2 2.4.6 3.6.1.3 0 .7-.2 1l-2.3 2.2z"/></svg>';
  var PHONE_DOWN = '<svg viewBox="0 0 24 24" width="30" height="30" style="transform:rotate(135deg)"><path fill="currentColor" d="M6.6 10.8c1.4 2.8 3.8 5.1 6.6 6.6l2.2-2.2c.3-.3.7-.4 1-.2 1.1.4 2.3.6 3.6.6.6 0 1 .4 1 1V20c0 .6-.4 1-1 1C10.8 21 3 13.2 3 4c0-.6.4-1 1-1h3.5c.6 0 1 .4 1 1 0 1.2.2 2.4.6 3.6.1.3 0 .7-.2 1l-2.3 2.2z"/></svg>';
  var PERSON_ICON = '<svg viewBox="0 0 24 24" width="46" height="46" fill="none" stroke="currentColor" stroke-width="1.4"><circle cx="12" cy="9.4" r="3.6"/><path d="M5.6 19.4c.9-3 3.4-4.9 6.4-4.9s5.5 1.9 6.4 4.9" stroke-linecap="round"/></svg>';

  /* Large title = the contact name when the address book knows this number,
   * the number itself otherwise; the number stays visible as the subtitle. */
  function setCallSubject(rawPeer) {
    var peer = String(rawPeer || '').trim();
    if (!peer) {
      $('call-peer').textContent = '未知号码';
      $('call-sub').textContent = '';
      $('call-avatar').innerHTML = PERSON_ICON;
      return;
    }
    var name = peerName(peer);
    var display = (name && name !== peer) ? name : peer;
    $('call-peer').textContent = display;
    $('call-sub').textContent = (display !== peer) ? peer : '';
    // An avatar initial only makes sense for a name; a phone number gets the
    // person glyph instead of a stray digit.
    if (/^[0-9+*#\s()-]+$/.test(display)) {
      $('call-avatar').innerHTML = PERSON_ICON;
    } else {
      $('call-avatar').textContent = display.charAt(0);
    }
  }

  function callControl(className, markup, label, onClick) {
    var wrap = el('div', 'cc');
    var btn = el('button', 'cbtn' + (className ? ' ' + className : ''));
    btn.innerHTML = markup;
    btn.setAttribute('aria-label', label);
    btn.addEventListener('click', onClick);
    wrap.appendChild(btn);
    wrap.appendChild(el('span', null, label));
    return wrap;
  }

  function openCallScreen(callId, options) {
    options = options || {};
    var controls = $('call-controls');
    controls.textContent = '';
    $('call-view').classList.remove('hidden');
    setCallSubject(options.peer || '');
    $('call-state').textContent = options.incoming ? '来电' : '正在呼叫…';

    var flow = new window.CallRelayCallFlow.CallFlow({
      callId: callId || '',
      peer: options.peer || '',
      incoming: Boolean(options.incoming),
      dial: options.dial || null,
      api: function (method, path, body) {
        return S.api(path, { method: method, body: body });
      },
      makePlayer: FIXTURE ? function () { return new DemoPlayer(); }
                          : function () { return new AU.PcmuPlayer(); },
      makeCapture: FIXTURE ? function () { return new DemoCapture(); }
                           : function () { return new AU.PcmuCapture(); },
      getMic: FIXTURE ? demoMic : function () {
        return navigator.mediaDevices.getUserMedia({
          audio: { echoCancellation: true, noiseSuppression: true, channelCount: 1 },
        });
      },
      makeWebRTC: makeWebRTCFactory(),
      openSocket: function (id) {
        var scheme = location.protocol === 'https:' ? 'wss' : 'ws';
        // Build-48: optional directional denoise policy (gateway applies it
        // with SpeexDSP); older gateways ignore the unknown query params.
        var query = window.CallRelayDenoise
          ? '?' + window.CallRelayDenoise.query()
          : '';
        var ws = new WebSocket(scheme + '://' + location.host + '/api/v2/calls/' +
          encodeURIComponent(id) + '/media' + query);
        ws.binaryType = 'arraybuffer';
        // Hot denoise state applies to every fresh media socket (the
        // gateway also re-reads the per-device preferences server-side).
        if (window.CallRelayDenoise) {
          ws.addEventListener('open', function () {
            window.CallRelayDenoise.control(ws);
          });
        }
        return ws;
      },
      onState: renderCallView,
    });
    state.call = flow;

    var PHASE_TEXT = {
      ringing: '来电', dialing: '正在呼叫…', preparing: '准备音频…',
      answering: '接听中…', connecting: '正在接通…', active: '通话中',
      ended: '通话结束', failed: '媒体连接失败',
    };

    function renderCallView(view) {
      if (view.peer) { setCallSubject(view.peer); }
      var phaseText = PHASE_TEXT[view.phase] || view.phase;
      if (view.error === 'mic-denied' || (view.error && view.error.indexOf('权限') >= 0)) {
        phaseText = '麦克风不可用，无法接听';
      }
      $('call-state').textContent = phaseText;
      controls.textContent = '';
      if (view.phase === 'ended') {
        setTimeout(closeCallScreen, 900);
        return;
      }
      if (!view.canHangup) { return; }
      if (view.canAccept && (view.phase === 'ringing' || view.phase === 'preparing')) {
        controls.appendChild(callControl('hangup', PHONE_DOWN, '拒绝', function () {
          flow.hangup('reject');
        }));
        controls.appendChild(callControl('accept', PHONE_UP, '接听', function () {
          flow.accept().then(function (ok) {
            if (!ok && state.call === flow) {
              $('call-state').textContent = '接听未完成：请允许麦克风后重试，或拒绝';
            }
          });
        }));
        return;
      }
      if (view.phase === 'active') {
        var muteLabel = view.muted ? '取消静音' : '静音';
        // Single source of truth: the flow view (re-emitted by setMuted)
        // drives the label, so it updates immediately and can never claim a
        // state the capture gate does not enforce.
        controls.appendChild(callControl(view.muted ? 'on' : '',
          '<svg viewBox="0 0 24 24" width="26" height="26"><path fill="currentColor" d="M12 14a3 3 0 0 0 3-3V5a3 3 0 0 0-6 0v6a3 3 0 0 0 3 3zm5-3a5 5 0 0 1-10 0H5a7 7 0 0 0 6 6.9V21h2v-3.1A7 7 0 0 0 19 11h-2z"/></svg>',
          muteLabel, function () { flow.setMuted(!flow.muted); }));
      }
      controls.appendChild(callControl('hangup', PHONE_DOWN, '挂断', function () {
        flow.hangup();
      }));
    }

    function closeCallScreen() {
      if (state.call !== flow) { return; }
      state.call = null;
      $('call-view').classList.add('hidden');
      refreshThreads();
    }

    if (options.incoming) { flow.beginIncoming(); }
    else {
      flow.beginOutgoing().then(function () {
        if (state.call === flow) { /* polling continues */ }
      });
    }
  }

  // Build-49 hot apply while a call is live on this page: forward the
  // choice to the open media socket (the PATCH from the settings page is
  // the primary channel; this is the same-page fast path).
  if (typeof self !== 'undefined' && self.addEventListener) {
    self.addEventListener('callrelay:denoise', function () {
      var flow = state.call;
      if (flow && flow.socket && flow.socket.readyState === 1 &&
          window.CallRelayDenoise) {
        window.CallRelayDenoise.control(flow.socket);
      }
    });
  }

  /* ---------------- call history ---------------- */

  var CALL_ARROW_IN = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M18 6 8 16M8 9v7h7"/></svg>';
  var CALL_ARROW_OUT = '<svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 18 16 8M9 8h7v7"/></svg>';

  function refreshCalls() {
    return S.api('/api/v2/calls?limit=100').then(function (body) {
      state.calls = (body && body.calls) ? body.calls : (body || []);
      state.callsError = false;
      renderCallList();
    }).catch(function () {
      // A failed fetch must still leave an honest screen — the old silent
      // catch left the tab completely blank.
      state.callsError = true;
      renderCallList();
    });
  }

  function callLineId(call) {
    var line = state.lines.filter(function (l) { return l.id === call.lineId; })[0];
    if (line && line.permissions && line.permissions.dial) { return line.id; }
    return preferredLine('', state.lines);
  }

  /* An inbound call that ended without ever connecting is a missed call.
   * A still-ringing inbound call (state incoming_ringing, no connectedAt)
   * must NOT be marked missed. */
  function callMissed(call) {
    return call.direction === 'inbound' && !call.connectedAt && call.state === 'ended';
  }

  function formatWhen(ms) {
    if (!ms) { return ''; }
    var date = new Date(ms);
    var now = new Date();
    if (date.toDateString() === now.toDateString()) {
      return clockTime(date);
    }
    var yesterday = new Date(now.getTime() - 864e5);
    if (date.toDateString() === yesterday.toDateString()) {
      return '昨天 ' + clockTime(date);
    }
    return (date.getMonth() + 1) + '月' + date.getDate() + '日';
  }

  function formatDuration(call) {
    if (!call.connectedAt) { return ''; }
    var seconds = Math.max(0, Math.round(((call.endedAt || Date.now()) - call.connectedAt) / 1000));
    if (seconds >= 3600) {
      return Math.floor(seconds / 3600) + '小时' + Math.floor((seconds % 3600) / 60) + '分';
    }
    if (seconds >= 60) { return Math.floor(seconds / 60) + '分' + (seconds % 60) + '秒'; }
    return seconds + '秒';
  }

  function renderCallList() {
    var list = $('call-list');
    list.textContent = '';
    var calls = state.calls || [];
    if (!calls.length) {
      if (state.callsError) {
        emptyState(list, 'calls', '无法加载通话记录', '请检查网络，切换到其他标签后再回来重试。');
      } else {
        emptyState(list, 'calls', '暂无通话记录', '拨出或接听电话后会显示在这里。');
      }
      return;
    }
    calls.forEach(function (call) {
      var missed = callMissed(call);
      var row = el('button', 'call-row');
      var dir = el('span', 'call-dir' + (missed ? ' missed' : ''));
      dir.innerHTML = call.direction === 'inbound' ? CALL_ARROW_IN : CALL_ARROW_OUT;
      var body = el('span', 'c-body');
      var name = el('span', 'c-name' + (missed ? ' missed' : ''));
      name.textContent = peerName(call.peer) || call.peer || '未知号码';
      var sub = el('span', 'c-sub');
      var detail = missed ? '未接来电' : (formatDuration(call) || '未接通');
      sub.textContent = [detail, call.lineName].filter(Boolean).join(' · ');
      body.appendChild(name);
      body.appendChild(sub);
      var when = el('span', 'c-time');
      when.textContent = formatWhen(call.startedAt);
      row.appendChild(dir);
      row.appendChild(body);
      row.appendChild(when);
      row.addEventListener('click', function () {
        startCall(call.peer, callLineId(call));
      });
      list.appendChild(row);
    });
  }

  /* ---------------- tabs ---------------- */

  var NAV_TITLES = { dialer: '电话', messages: '短信', calls: '通话记录', contacts: '通讯录' };

  function showTab(name) {
    state.activeTab = name;
    $('nav-title').textContent = NAV_TITLES[name] || '';
    document.querySelectorAll('.tabbtn[data-tab]').forEach(function (b) {
      b.classList.toggle('active', b.dataset.tab === name);
    });
    $('tab-dialer').classList.toggle('hidden', name !== 'dialer');
    $('tab-messages').classList.toggle('hidden', name !== 'messages');
    $('tab-calls').classList.toggle('hidden', name !== 'calls');
    $('tab-contacts').classList.toggle('hidden', name !== 'contacts');
    if (name === 'contacts') { refreshContacts(); }
    if (name === 'calls') { refreshCalls(); }
    // Edit mode is a conversation-list affordance: only on the messages tab,
    // and it exits cleanly when switching away.
    var onMessages = name === 'messages';
    if (!onMessages && state.smsEditing) { setSmsEditing(false); }
    $('messages-edit').classList.toggle('hidden', !onMessages || state.threads.length === 0);
    $('messages-select-all').classList.toggle('hidden', !onMessages || !state.smsEditing);
    // Edit mode is a focused mode: the personal-settings shortcuts step out.
    var editing = onMessages && state.smsEditing;
    $('rules-link').classList.toggle('hidden', editing);
    $('settings-link').classList.toggle('hidden', editing);
  }

  function wireTabs() {
    Array.prototype.forEach.call(document.querySelectorAll('.tabbtn[data-tab]'), function (btn) {
      btn.addEventListener('click', function () { showTab(btn.dataset.tab); });
    });
  }

  function clockTime(date) {
    // 24-hour everywhere: a zh-CN UI reads 15:56, not 03:56 PM.
    return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false });
  }

  function formatTime(ms) {
    if (!ms) { return ''; }
    return clockTime(new Date(ms));
  }

  /* Date stamp at the top of each conversation day. */
  function formatDayStamp(date) {
    var now = new Date();
    var time = clockTime(date);
    if (date.toDateString() === now.toDateString()) { return '今天 ' + time; }
    var yesterday = new Date(now.getTime() - 864e5);
    if (date.toDateString() === yesterday.toDateString()) { return '昨天 ' + time; }
    var stamp = (date.getMonth() + 1) + '月' + date.getDate() + '日';
    if (date.getFullYear() !== now.getFullYear()) {
      stamp = date.getFullYear() + '年' + stamp;
    }
    return stamp + ' ' + time;
  }

  boot();
}());
