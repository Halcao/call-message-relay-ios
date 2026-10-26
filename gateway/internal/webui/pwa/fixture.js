/* CallRelay PWA — NON-SENSITIVE PREVIEW FIXTURE.
 *
 * Activates ONLY with ?fixture=1 in the URL. Replaces fetch() and
 * WebSocket with in-memory stubs so the coordinator can review every
 * screen (paired dialer, incoming call, SMS thread/compose, line picker,
 * settings) with ZERO real calls, ZERO real SMS, ZERO network sends, and
 * ZERO real data — every number is a reserved synthetic 555. A visible
 * banner marks the demo state; nothing here touches a real gateway.
 */
(function () {
  'use strict';

  if (!/[?&]fixture=1/.test(location.search)) { return; }
  window.CALLRELAY_FIXTURE = true;

  /* ---------------- synthetic data (555-only) ---------------- */

  var LINES = [
    { id: 'line1', name: '主卡', phoneNumber: '+15550161111', enabled: true, online: true,
      sim: 'ready', registration: 'registered', voice: 'ready', sms: 'ready', smsLive: true,
      permissions: { receiveSms: true, receiveCalls: true, sendSms: true, dial: true } },
    { id: 'line2', name: '流量卡', phoneNumber: '+15550162222', enabled: true, online: true,
      sim: 'ready', registration: 'registered', voice: 'ready', sms: 'ready', smsLive: true,
      permissions: { receiveSms: true, receiveCalls: true, sendSms: true, dial: true } },
  ];

  var NOW = Date.now();
  var THREADS = [
    { key: 'line1:555-0123', lineId: 'line1', peer: '555-0123', displayName: '张三', unreadCount: 1,
      lastMessage: { id: 'm3', direction: 'inbound', body: '明天见', status: 'sent', createdAt: NOW - 4 * 60000 } },
    { key: 'line1:555-0456', lineId: 'line1', peer: '555-0456', displayName: null, unreadCount: 0,
      lastMessage: { id: 'm6', direction: 'outbound', body: '收到，谢谢！', status: 'sent', createdAt: NOW - 2 * 3600000 } },
    { key: 'line2:555-0789', lineId: 'line2', peer: '555-0789', displayName: '演示联系人', unreadCount: 0,
      lastMessage: { id: 'm9', direction: 'inbound', body: '这是一条演示短信。', status: 'read', createdAt: NOW - 26 * 3600000 } },
  ];

  var MESSAGES = {
    'line1:555-0123': [
      { id: 'm1', threadKey: 'line1:555-0123', direction: 'inbound', peer: '555-0123', body: '你好，这是一条演示短信，网关离线时也能查看。', status: 'read', createdAt: NOW - 3600000 },
      { id: 'm2', threadKey: 'line1:555-0123', direction: 'outbound', peer: '555-0123', body: '收到，我们明天下午两点联系。', status: 'sent', createdAt: NOW - 3500000 },
      { id: 'm3', threadKey: 'line1:555-0123', direction: 'inbound', peer: '555-0123', body: '明天见', status: 'sent', createdAt: NOW - 4 * 60000 },
    ],
    'line1:555-0456': [
      { id: 'm4', threadKey: 'line1:555-0456', direction: 'inbound', peer: '555-0456', body: '资料已发到邮箱。', status: 'read', createdAt: NOW - 3 * 3600000 },
      { id: 'm6', threadKey: 'line1:555-0456', direction: 'outbound', peer: '555-0456', body: '收到，谢谢！', status: 'sent', createdAt: NOW - 2 * 3600000 },
    ],
    'line2:555-0789': [
      { id: 'm7', threadKey: 'line2:555-0789', direction: 'inbound', peer: '555-0789', body: '这是一条演示短信。', status: 'read', createdAt: NOW - 26 * 3600000 },
    ],
  };

  var CONTACTS = [
    { id: 'ct_fixture_1', displayName: '张三', searchKey: 'zhangsan zs 9426', source: 'sync',
      phones: [{ label: 'CELL', value: '+15550160001' }, { label: 'WORK', value: '555-0123' }] },
    { id: 'ct_fixture_2', displayName: '李四', searchKey: 'lisi ls 5474', source: 'vcard',
      phones: [{ label: 'HOME', value: '555-0789' }] },
  ];
  var CONTACT_REVISION = 2;

  function parseFixtureVCards(text) {
    return String(text).split(/BEGIN:VCARD/i).slice(1).map(function (chunk, index) {
      var fn = (chunk.match(/FN[^:]*:([^\r\n]*)/i) || [])[1] || ('导入联系人 ' + (index + 1));
      var phones = [];
      chunk.replace(/TEL[^:]*:([^\r\n]*)/gi, function (_, value) {
        var trimmed = String(value).trim();
        if (trimmed) { phones.push({ label: 'CELL', value: trimmed }); }
        return '';
      });
      return { displayName: String(fn).trim(), searchKey: '', phones: phones };
    }).filter(function (card) { return card.displayName || card.phones.length; });
  }

  var CALLS = {};   // id -> simulated call record
  // Synthetic history so the call-log tab is reviewable without placing a
  // real call. Every number is a reserved 555 number.
  var CALL_HISTORY = [
    { id: 'demo-call-h1', lineId: 'line1', lineName: '主卡', peer: '555-0123',
      direction: 'inbound', state: 'ended', startedAt: NOW - 8 * 60000,
      connectedAt: null, endedAt: NOW - 8 * 60000, endReason: 'missed' },
    { id: 'demo-call-h2', lineId: 'line1', lineName: '主卡', peer: '555-0456',
      direction: 'outbound', state: 'ended', startedAt: NOW - 3 * 3600000,
      connectedAt: NOW - 3 * 3600000 + 4000, endedAt: NOW - 3 * 3600000 + 68000,
      endReason: 'hangup' },
    { id: 'demo-call-h3', lineId: 'line2', lineName: '流量卡', peer: '555-0789',
      direction: 'inbound', state: 'ended', startedAt: NOW - 26 * 3600000,
      connectedAt: NOW - 26 * 3600000 + 1000, endedAt: NOW - 26 * 3600000 + 121000,
      endReason: 'hangup' },
  ];
  var EVENT_LISTENERS = [];

  function fixtureCall(id, peer, direction) {
    return { id: id, lineId: 'line1', peer: peer, direction: direction,
      state: direction === 'inbound' ? 'incomingRinging' : 'outgoingDialing',
      startedAt: Date.now(), connectedAt: null, endedAt: null, endReason: null };
  }

  function emit(event) {
    setTimeout(function () {
      EVENT_LISTENERS.forEach(function (listener) { listener(event); });
    }, 30);
  }

  /* ---------------- fetch stub ---------------- */

  var realFetch = window.fetch;
  window.fetch = function (input, options) {
    var url = typeof input === 'string' ? input : input.url;
    var path = url.replace(/^https?:\/\/[^/]+/, '');
    options = options || {};
    var method = options.method || 'GET';

    function json(status, body) {
      return Promise.resolve(new Response(JSON.stringify(body), {
        status: status, headers: { 'Content-Type': 'application/json' },
      }));
    }
    function notFound() { return json(404, { code: 'CB-V2-404', message: 'not found' }); }

    var body = null;
    try { body = options.body ? JSON.parse(options.body) : null; } catch (e) { /* form body */ }

    if (path === '/api/v2/identity') {
      return json(200, { gatewayId: 'gw-preview', gatewayName: 'CallRelay 预览', apiVersion: 'v2' });
    }
    if (path === '/api/v2/device') {
      return json(200, {
        device: { id: 'dev_preview', name: '预览浏览器', defaultLineId: 'line1' },
        key: { id: 'key_preview', name: 'preview-key', allLines: true },
        lines: LINES,
      });
    }
    if (path === '/api/v2/lines') { return json(200, LINES); }
    if (path === '/api/v2/threads') { return json(200, THREADS); }
    // Conversation tombstone (synthetic): hides the thread for the preview,
    // exactly like the gateway's authorized DELETE.
    var deleteThread = path.match(/^\/api\/v2\/threads\/(.+)$/);
    if (deleteThread && method === 'DELETE') {
      var deleteKey = decodeURIComponent(deleteThread[1]);
      for (var i = THREADS.length - 1; i >= 0; i--) {
        if (THREADS[i].key === deleteKey) { THREADS.splice(i, 1); }
      }
      delete MESSAGES[deleteKey];
      return json(200, { key: deleteKey, deleted: true });
    }
    /* Principal-scoped contacts preview data. The real server enforces the
     * key scope; the fixture only makes the import/preview UI reviewable. */
    if (path.indexOf('/api/v2/contacts') === 0) {
      var contactList = path.split('?')[0];
      if (contactList === '/api/v2/contacts' && method === 'GET') {
        var query = decodeURIComponent((path.split('query=')[1] || '').split('&')[0] || '').toLowerCase();
        var digits = query.replace(/\D/g, '');
        var filtered = CONTACTS.filter(function (contact) {
          if (!query) { return true; }
          if ((contact.displayName || '').toLowerCase().indexOf(query) >= 0) { return true; }
          if ((contact.searchKey || '').toLowerCase().indexOf(query) >= 0) { return true; }
          return (contact.phones || []).some(function (phone) {
            return digits && String(phone.value).replace(/\D/g, '').indexOf(digits) >= 0;
          });
        });
        return json(200, { contacts: filtered, revision: CONTACT_REVISION, nextSince: CONTACT_REVISION, hasMore: false });
      }
      if (contactList === '/api/v2/contacts/sync' && method === 'GET') {
        return json(200, { contacts: CONTACTS, revision: CONTACT_REVISION, nextSince: CONTACT_REVISION, hasMore: false });
      }
      if (contactList === '/api/v2/contacts/import' && method === 'POST') {
        var parsedCards = parseFixtureVCards((body && body.vcard) || '');
        if (body && body.preview) {
          return json(200, {
            preview: true, created: parsedCards.length, updated: 0, unchanged: 0,
            skipped: 0, total: parsedCards.length, revision: CONTACT_REVISION,
            entries: parsedCards.map(function (card) {
              return { action: 'create', name: card.displayName, phones: card.phones.map(function (p) { return p.value; }), changes: ['新增联系人'] };
            }),
          });
        }
        parsedCards.forEach(function (card) {
          card.id = 'ct_fixture_' + (CONTACTS.length + 1);
          card.source = 'vcard';
          CONTACTS.push(card);
        });
        CONTACT_REVISION += parsedCards.length;
        return json(200, { created: parsedCards.length, updated: 0, unchanged: 0, skipped: 0, total: parsedCards.length, revision: CONTACT_REVISION, entries: [] });
      }
      if (contactList === '/api/v2/contacts/export' && method === 'GET') {
        var lines = CONTACTS.map(function (contact) {
          var card = 'BEGIN:VCARD\r\nVERSION:3.0\r\nFN:' + contact.displayName + '\r\n';
          (contact.phones || []).forEach(function (phone) { card += 'TEL;TYPE=CELL:' + phone.value + '\r\n'; });
          return card + 'END:VCARD\r\n';
        }).join('');
        return Promise.resolve(new Response(lines, { status: 200, headers: { 'Content-Type': 'text/vcard' } }));
      }
      var contactMatch = path.match(/^\/api\/v2\/contacts\/([^/]+)$/);
      if (contactMatch && method === 'PUT') {
        var editID = decodeURIComponent(contactMatch[1]);
        var target = CONTACTS.filter(function (c) { return c.id === editID; })[0];
        if (!target) { return notFound(); }
        if (body && typeof body.displayName === 'string') { target.displayName = body.displayName; }
        if (body && body.phones) { target.phones = body.phones; }
        CONTACT_REVISION += 1;
        return json(200, target);
      }
      if (contactMatch && method === 'DELETE') {
        var removeID = decodeURIComponent(contactMatch[1]);
        for (var ci = CONTACTS.length - 1; ci >= 0; ci--) {
          if (CONTACTS[ci].id === removeID) { CONTACTS.splice(ci, 1); }
        }
        CONTACT_REVISION += 1;
        return json(200, { deleted: true });
      }
    }
    if (path.indexOf('/api/v2/messages') === 0 && method === 'GET') {
      var key = (path.split('threadKey=')[1] || '').split('&')[0];
      key = decodeURIComponent(key);
      return json(200, { messages: MESSAGES[key] || [], hasMore: false });
    }
    // Per-message read receipt (synthetic): flips the message and refreshes
    // the thread's unread badge so the preview shows real convergence.
    var readMatch = path.match(/^\/api\/v2\/messages\/([^/]+)\/read$/);
    if (readMatch && method === 'POST') {
      var msgId = decodeURIComponent(readMatch[1]);
      Object.keys(MESSAGES).forEach(function (threadKey) {
        MESSAGES[threadKey].forEach(function (m) {
          if (m.id === msgId) {
            m.status = 'read';
            var thread = THREADS.filter(function (t) { return t.key === threadKey; })[0];
            if (thread) {
              thread.unreadCount = MESSAGES[threadKey].filter(function (x) {
                return x.direction === 'inbound' && x.status !== 'read';
              }).length;
            }
          }
        });
      });
      return Promise.resolve(new Response(null, { status: 204 }));
    }
    if (method === 'PUT' && /^\/api\/v2\/devices\/[^/]+\/preferences$/.test(path)) {
      var defaultLine = (body && body.defaultLineId) || '';
      LINES.forEach(function (line) { line.isDefault = line.id === defaultLine; });
      return json(200, { defaultLineId: defaultLine });
    }
    if (path === '/api/v2/messages' && method === 'POST') {
      var lineId = (body && body.lineId) || 'line1';
      var to = (body && body.to) || '555-0000';
      var threadKey = lineId + ':' + to;
      var record = { id: 'demo-' + Date.now(), threadKey: threadKey, direction: 'outbound',
        peer: to, body: (body && body.body) || '', status: 'sent', createdAt: Date.now() };
      (MESSAGES[threadKey] = MESSAGES[threadKey] || []).push(record);
      var thread = THREADS.filter(function (t) { return t.key === threadKey; })[0];
      if (!thread) {
        thread = { key: threadKey, lineId: lineId, peer: to, displayName: null, unreadCount: 0,
          lastMessage: record };
        THREADS.unshift(thread);
      } else {
        thread.lastMessage = record;
      }
      setTimeout(function () {
        emit({ type: 'message.created', lineId: lineId, threadKey: threadKey });
      }, 600);
      return json(201, record);
    }
    if (path === '/api/v2/calls' && method === 'POST') {
      var id = 'demo-call-' + Date.now();
      var peer2 = (body && body.to) || '555-0000';
      CALLS[id] = fixtureCall(id, peer2, 'outbound');
      var call = CALLS[id];
      setTimeout(function () {
        call.state = 'connecting';
        emit({ type: 'call.updated', call: { id: id, state: 'connecting' } });
      }, 1500);
      setTimeout(function () {
        call.state = 'active';
        call.connectedAt = Date.now();
        emit({ type: 'call.updated', call: { id: id, state: 'active' } });
      }, 4500);
      setTimeout(function () {
        call.state = 'ended';
        call.endedAt = Date.now();
        call.endReason = 'hangup';
        emit({ type: 'call.updated', call: { id: id, state: 'ended' } });
      }, 30000);
      return json(201, call);
    }
    if (path.split('?')[0] === '/api/v2/calls' && method === 'GET') {
      var live = Object.keys(CALLS).map(function (k) { return CALLS[k]; });
      return json(200, { calls: live.concat(CALL_HISTORY) });
    }
    var callMatch = path.match(/^\/api\/v2\/calls\/([^/]+)(?:\/(answer|reject|hangup))?$/);
    if (callMatch) {
      var callId = decodeURIComponent(callMatch[1]);
      if (callMatch[2]) {
        var target = CALLS[callId] || (CALLS[callId] = fixtureCall(callId, '555-0123', 'inbound'));
        if (callMatch[2] === 'answer') {
          target.state = 'connecting';
          setTimeout(function () {
            target.state = 'active';
            target.connectedAt = Date.now();
            emit({ type: 'call.updated', call: { id: callId, state: 'active' } });
          }, 1200);
        } else {
          target.state = 'ended';
          target.endedAt = Date.now();
          target.endReason = callMatch[2] === 'reject' ? 'rejected' : 'hangup';
        }
        return json(200, target);
      }
      var existing = CALLS[callId];
      if (existing) { return json(200, existing); }
      // Incoming demo call for the preview.
      var incoming = fixtureCall(callId, '555-0123', 'inbound');
      CALLS[callId] = incoming;
      return json(200, incoming);
    }
    if (path === '/api/v2/webpush/bind' && method === 'POST') {
      document.cookie = 'cr_ws_session=preview; Path=/api/v2';
      return json(200, { sessionToken: 'preview', vapidPublicKey: 'preview', scope: 'client' });
    }
    if (path === '/api/v2/webpush/session' && method === 'GET') {
      // ?unbound=1 previews the first-run bind screen.
      if (/[?&]unbound=1/.test(location.search)) {
        return json(401, { code: 'CB-V2-401', message: 'not enrolled' });
      }
      return json(200, { subscriptionCount: 0, notifyMode: 'web',
        vapidPublicKey: 'preview-vapid', deviceId: 'dev_preview',
        deviceName: '预览浏览器', subscriptions: [] });
    }
    if (path === '/api/v2/webpush/subscription' && method === 'PUT') {
      return json(200, { ok: true });
    }
    if (path === '/api/v2/webpush/test' && method === 'POST') {
      return json(200, { ok: true });
    }
    if (path.indexOf('/api/v2/webpush/') === 0) {
      return json(200, { subscriptionCount: 0, notifyMode: 'web', ringing: true,
        callId: 'demo-call-1', peer: '555-0123', line: '主卡' });
    }
    // Unknown fixture path: FAIL CLOSED. The preview promises ZERO real
    // network sends, so anything outside the demo surface is a 404 — never
    // a silent passthrough that could touch a real gateway.
    return json(404, { code: 'CB-FIXTURE-404', message: 'fixture: unknown API path ' + path });
  };

  /* ---------------- WebSocket stub ---------------- */

  var RealWS = window.WebSocket;
  window.WebSocket = function (url, protocols) {
    if (url.indexOf('/api/v2/') === -1 && url.indexOf('demo') === -1) {
      return new RealWS(url, protocols);
    }
    var ws = {
      url: url, readyState: 0, binaryType: 'blob',
      onopen: null, onclose: null, onmessage: null, onerror: null,
      send: function () { /* demo sink: drops mic frames, never transmits */ },
      close: function () {
        ws.readyState = 3;
        if (ws.onclose) { ws.onclose({}); }
      },
    };
    setTimeout(function () {
      ws.readyState = 1;
      if (ws.onopen) { ws.onopen({}); }
      if (url.indexOf('/media') >= 0) {
        var callId = (url.match(/calls\/([^/]+)\/media/) || [])[1];
        var call = callId && CALLS[decodeURIComponent(callId)];
        if (call && call.state === 'incomingRinging') {
          // Pre-answer media attach is rejected, exactly like the gateway.
          setTimeout(function () {
            if (ws.onclose) { ws.onclose({}); }
          }, 150);
          return ws;
        }
        // Media demo: ready, then 160-byte silence frames at 20 ms.
        setTimeout(function () {
          if (ws.onmessage) { ws.onmessage({ data: JSON.stringify({ type: 'ready' }) }); }
        }, 300);
        var ticks = 0;
        var timer = setInterval(function () {
          if (ws.readyState !== 1) { clearInterval(timer); return; }
          ticks += 1;
          if (ticks > 250) { clearInterval(timer); return; } // ~5 s demo audio
          if (ws.onmessage) {
            ws.onmessage({ data: new Uint8Array(160).buffer });
          }
        }, 20);
      }
    }, 120);
    return ws;
  };

  /* ---------------- demo chrome ---------------- */

  function demoChrome() {
    var banner = document.createElement('div');
    banner.textContent = '演示模式 · 合成数据（555 号码），不会真实拨打或发送';
    banner.style.cssText = 'position:fixed;top:0;left:0;right:0;z-index:99;background:#ff9500;' +
      'color:#fff;text-align:center;font-size:12px;padding:6px 150px 6px 8px;';
    document.body.appendChild(banner);
    document.body.style.paddingTop = '28px';

    // Compact links INSIDE the banner so the preview never covers the dial
    // pad, call controls or the tab bar.
    function demoButton(label, href) {
      var a = document.createElement('a');
      a.textContent = label;
      a.href = href;
      a.style.cssText = 'background:rgba(255,255,255,.22);color:#fff;border-radius:10px;' +
        'padding:2px 8px;font-size:11px;text-decoration:none;';
      return a;
    }
    var bar = document.createElement('span');
    bar.style.cssText = 'position:absolute;right:6px;top:3px;display:flex;gap:6px;';
    bar.appendChild(demoButton('模拟来电', location.pathname + '?fixture=1&call=demo-call-1'));
    bar.appendChild(demoButton('新短信', location.pathname + '?fixture=1#compose'));
    banner.appendChild(bar);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', demoChrome);
  } else {
    demoChrome();
  }
}());
