/* CallRelay PWA — 骚扰拦截 & 录音库 (build 55).
 *
 * Uses the same key-scoped v2 APIs as the native app; the gateway derives the
 * enrollment key from the browser session, so a different key on the same
 * line can neither read nor modify these rows. On an older gateway every
 * harassment/recording route answers 404 and this panel says so honestly
 * instead of pretending. No credentials or message bodies are stored here.
 */
(function () {
  'use strict';

  var S = window.CallRelaySession;
  var $ = function (id) { return document.getElementById(id); };
  var state = {
    deviceId: '',
    settings: null,
    entries: [],
    lists: [],
    history: [],
    recordings: [],
    recordingSettings: null,
    playingId: null,
    audio: null,
    objectUrl: null,
    supported: false
  };

  function api(path, options) { return S.api(path, options); }

  function status(text) { $('rules-status').textContent = text || ''; }

  function row(text, buttons) {
    var li = document.createElement('li');
    var label = document.createElement('span');
    label.textContent = text;
    li.appendChild(label);
    (buttons || []).forEach(function (spec) {
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.textContent = spec.label;
      if (spec.danger) { btn.className = 'danger'; }
      btn.addEventListener('click', spec.onClick);
      li.appendChild(btn);
    });
    return li;
  }

  function fillList(id, items, render) {
    var list = $(id);
    while (list.firstChild) { list.removeChild(list.firstChild); }
    (items || []).forEach(function (item) { list.appendChild(render(item)); });
  }

  function open() {
    $('main-view').classList.add('hidden');
    $('rules-view').classList.remove('hidden');
    refresh();
  }

  function close() {
    stopPlayback();
    $('rules-view').classList.add('hidden');
    $('main-view').classList.remove('hidden');
  }

  function refresh() {
    status('载入中…');
    S.current().then(function (session) {
      if (!session) { status('会话已失效，请重新配对。'); return; }
      state.deviceId = session.device && session.device.id ? session.device.id : '';
      return api('/api/v2/harassment/settings').then(function (body) {
        state.supported = true;
        state.settings = body.settings || {};
        $('rules-unsupported').classList.add('hidden');
        return Promise.all([
          api('/api/v2/harassment/entries').then(function (b) { state.entries = b.entries || []; }),
          api('/api/v2/harassment/lists').then(function (b) { state.lists = b.lists || []; }),
          api('/api/v2/harassment/history').then(function (b) { state.history = b.events || []; }),
          api('/api/v2/recordings').then(function (b) { state.recordings = Array.isArray(b) ? b : (b.recordings || []); }),
          api('/api/v2/recordings/settings').then(function (b) { state.recordingSettings = b; }),
          refreshThreads()
        ]);
      }).then(function () {
        render();
        status('');
      }).catch(function (err) {
        if (err.status === 404) {
          state.supported = false;
          $('rules-unsupported').classList.remove('hidden');
          status('');
          render();
          return;
        }
        status(err.message || '载入失败');
      });
    }).catch(function (err) {
      status(err.message || '载入失败');
    });
  }

  var smsThreads = [];

  function refreshThreads() {
    var folder = $('rules-sms-folder').value;
    return api('/api/v2/threads?folder=' + encodeURIComponent(folder)).then(function (body) {
      smsThreads = Array.isArray(body) ? body : [];
    }).catch(function () {
      smsThreads = [];
    });
  }

  function render() {
    if (!state.supported) { return; }
    var settings = state.settings || {};
    $('rules-call-toggle').checked = !!settings.callBlocking;
    $('rules-sms-toggle').checked = !!settings.smsBlocking;

    fillList('rules-blocked', state.entries.filter(function (e) { return e.mode !== 'trust'; }), function (entry) {
      return row(entry.number + (entry.matchKind === 'prefix' ? '（前缀）' : '') + (entry.action === 'label' ? '（仅标记）' : ''), [{
        label: '删除', danger: true, onClick: function () { deleteEntry(entry.id); }
      }]);
    });
    fillList('rules-trusted', state.entries.filter(function (e) { return e.mode === 'trust'; }), function (entry) {
      return row(entry.number, [{ label: '删除', danger: true, onClick: function () { deleteEntry(entry.id); } }]);
    });
    fillList('rules-lists', state.lists, function (list) {
      return row(list.name + ' · ' + list.entryCount + ' 个号码 · ' + (list.defaultAction === 'reject' ? '拦截' : '仅标记'), [{
        label: '删除', danger: true, onClick: function () { deleteList(list.id); }
      }]);
    });
    fillList('rules-history', (state.history || []).slice(0, 30), function (event) {
      return row((event.kind === 'call' ? '来电' : '短信') + ' · ' + event.peer + ' · ' +
        (event.action === 'label' ? '仅标记' : '已拦截') + ' · ' + new Date(event.createdAt).toLocaleString(), []);
    });
    fillList('rules-threads', smsThreads, function (thread) {
      var buttons = [];
      if (thread.folder === 'junk') {
        buttons.push({ label: '恢复', onClick: function () { restoreThread(thread.key); } });
      }
      buttons.push({ label: '删除', danger: true, onClick: function () { deleteThread(thread.key); } });
      return row(thread.peer + '：' + (thread.lastMessage ? thread.lastMessage.body : ''), buttons);
    });

    var recordingSettings = state.recordingSettings || {};
    $('rules-auto-record').checked = !!recordingSettings.deviceAutoRecord;
    $('rules-auto-hint').textContent = recordingSettings.autoMode === 'off'
      ? '网关未开启自动录音。' : '';
    fillList('rules-recordings', state.recordings, function (recording) {
      var date = new Date(recording.createdAt || 0).toLocaleString();
      var label = (recording.peer || '未知号码') + ' · ' + date + (recording.kind === 'conference' ? ' · 会议' : '');
      return row(label, [
        { label: state.playingId === recording.id ? '停止' : '播放', onClick: function () { togglePlayback(recording); } },
        { label: '删除', danger: true, onClick: function () { deleteRecording(recording.id); } }
      ]);
    });
  }

  function updateToggles() {
    if (!state.supported) { return; }
    var body = {
      callBlocking: $('rules-call-toggle').checked,
      smsBlocking: $('rules-sms-toggle').checked,
      version: state.settings.version
    };
    api('/api/v2/harassment/settings', { method: 'PUT', body: body }).then(function (result) {
      state.settings = result.settings || {};
      status('');
    }).catch(function (err) {
      status(err.status === 409 ? '设置已在其他设备修改，请刷新后重试。' : (err.message || '保存失败'));
      refresh();
    });
  }

  function addEntry(number, matchKind, mode) {
    if (!number) { return; }
    api('/api/v2/harassment/entries', { method: 'POST', body: { number: number, matchKind: matchKind, mode: mode } }).then(function () {
      refresh();
    }).catch(function (err) { status(err.message || '添加失败'); });
  }

  function deleteEntry(id) {
    api('/api/v2/harassment/entries/' + encodeURIComponent(id), { method: 'DELETE' }).then(refresh)
      .catch(function (err) { status(err.message || '删除失败'); });
  }

  function deleteList(id) {
    api('/api/v2/harassment/lists/' + encodeURIComponent(id), { method: 'DELETE' }).then(refresh)
      .catch(function (err) { status(err.message || '删除失败'); });
  }

  function importList() {
    var text = $('rules-import-text').value || '';
    var numbers = text.split(/\r?\n/).map(function (line) { return line.trim(); }).filter(Boolean);
    if (!numbers.length) { status('请先粘贴号码。'); return; }
    api('/api/v2/harassment/lists/import', {
      method: 'POST',
      body: { name: '网页导入', defaultAction: 'label', numbers: numbers }
    }).then(function () {
      $('rules-import-text').value = '';
      status('已导入（默认仅标记）。');
      refresh();
    }).catch(function (err) { status(err.message || '导入失败'); });
  }

  function restoreThread(key) {
    api('/api/v2/threads/' + encodeURIComponent(key) + '/restore', { method: 'POST', body: {} }).then(refresh)
      .catch(function (err) { status(err.message || '恢复失败'); });
  }

  function deleteThread(key) {
    api('/api/v2/threads/' + encodeURIComponent(key), { method: 'DELETE' }).then(refresh)
      .catch(function (err) { status(err.message || '删除失败'); });
  }

  function toggleAutoRecord() {
    if (!state.deviceId) { return; }
    var enabled = $('rules-auto-record').checked;
    api('/api/v2/devices/' + encodeURIComponent(state.deviceId) + '/preferences', {
      method: 'PUT', body: { autoRecord: enabled }
    }).then(function () {
      state.recordingSettings = state.recordingSettings || {};
      state.recordingSettings.deviceAutoRecord = enabled;
      render();
    }).catch(function (err) {
      $('rules-auto-record').checked = !enabled;
      status(err.message || '保存失败');
    });
  }

  function stopPlayback() {
    if (state.audio) { state.audio.pause(); state.audio = null; }
    if (state.objectUrl) { URL.revokeObjectURL(state.objectUrl); state.objectUrl = null; }
    state.playingId = null;
  }

  function togglePlayback(recording) {
    if (state.playingId === recording.id) { stopPlayback(); render(); return; }
    stopPlayback();
    fetch('/api/v2/recordings/' + encodeURIComponent(recording.id) + '/audio', { credentials: 'same-origin' })
      .then(function (resp) {
        if (!resp.ok) { throw new Error('HTTP ' + resp.status); }
        return resp.blob();
      })
      .then(function (blob) {
        state.objectUrl = URL.createObjectURL(blob);
        state.audio = new Audio(state.objectUrl);
        state.audio.addEventListener('ended', function () { stopPlayback(); render(); });
        state.playingId = recording.id;
        state.audio.play();
        render();
      })
      .catch(function (err) { status('无法播放录音：' + (err.message || err)); });
  }

  function deleteRecording(id) {
    api('/api/v2/recordings/' + encodeURIComponent(id), { method: 'DELETE' }).then(refresh)
      .catch(function (err) { status(err.message || '删除失败'); });
  }

  function clearHistory() {
    api('/api/v2/harassment/history', { method: 'DELETE' }).then(refresh)
      .catch(function (err) { status(err.message || '清空失败'); });
  }

  function wire() {
    $('rules-link').addEventListener('click', open);
    $('rules-back').addEventListener('click', close);
    $('rules-call-toggle').addEventListener('change', updateToggles);
    $('rules-sms-toggle').addEventListener('change', updateToggles);
    $('rules-block-add').addEventListener('click', function () {
      addEntry($('rules-block-number').value.trim(), $('rules-block-kind').value, 'block');
      $('rules-block-number').value = '';
    });
    $('rules-trust-add').addEventListener('click', function () {
      addEntry($('rules-trust-number').value.trim(), 'exact', 'trust');
      $('rules-trust-number').value = '';
    });
    $('rules-import').addEventListener('click', importList);
    $('rules-history-clear').addEventListener('click', clearHistory);
    $('rules-sms-refresh').addEventListener('click', function () {
      refreshThreads().then(function () { render(); });
    });
    $('rules-sms-folder').addEventListener('change', function () {
      refreshThreads().then(function () { render(); });
    });
    $('rules-auto-record').addEventListener('change', toggleAutoRecord);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', wire);
  } else {
    wire();
  }
}());
