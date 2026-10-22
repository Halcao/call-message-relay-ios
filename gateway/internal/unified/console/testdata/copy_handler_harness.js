'use strict';
// Unit harness for the console key reveal/copy inline handler. It renders the
// real keys page (passed as argv[2]), extracts the inline script, runs it
// against a minimal DOM/clipboard double, and asserts the copy/failure
// semantics. No test succeeds unless the handler itself reports it, so a
// silently rejected clipboard can never be reported as success.
//
// Usage: node copy_handler_harness.js <rendered-keys.html>
// Exits 0 on "ALL COPY HANDLER SCENARIOS PASSED", 1 otherwise.

const fs = require('fs');

const htmlPath = process.argv[2];
if (!htmlPath) {
  console.error('usage: node copy_handler_harness.js <rendered-keys.html>');
  process.exit(2);
}
const html = fs.readFileSync(htmlPath, 'utf8');
const match = html.match(/<script id="key-actions">([\s\S]*?)<\/script>/);
if (!match) {
  console.error('key-actions script missing from rendered keys page');
  process.exit(1);
}
const script = match[1];

// ---------- minimal DOM ----------

function matchesSimple(element, selector) {
  const value = selector.trim();
  if (value.startsWith('.')) {
    return (element.className || '').split(/\s+/).includes(value.slice(1));
  }
  if (value.startsWith('#')) {
    return element.id === value.slice(1);
  }
  return (element.tag || '').toLowerCase() === value.toLowerCase();
}

class Element {
  constructor(tag, className, id) {
    this.tag = tag;
    this.className = className || '';
    this.id = id || '';
    this.value = '';
    this.textContent = '';
    this.hidden = false;
    this.disabled = false;
    this.src = '';
    this.dataset = {};
    this.attributes = {};
    this.listeners = {};
    this.children = [];
    this.selected = false;
    this.focused = false;
  }
  append(child) {
    this.children.push(child);
    child.parent = this;
    return child;
  }
  addEventListener(type, handler) {
    (this.listeners[type] = this.listeners[type] || []).push(handler);
  }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null; }
  removeAttribute(name) {
    delete this.attributes[name];
    // Reflect the attributes the production handler clears.
    if (name === 'src') this.src = '';
  }
  focus() { this.focused = true; }
  select() { this.selected = true; }
  click() {
    for (const handler of this.listeners.click || []) handler.call(this, {});
  }
  descendants() {
    const all = [];
    for (const child of this.children) {
      all.push(child, ...child.descendants());
    }
    return all;
  }
  querySelectorAll(selector) {
    return this.descendants().filter((element) => matchesSimple(element, selector));
  }
  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }
}

function element(tag, className, id) { return new Element(tag, className, id); }

const csrf = element('input', '', 'keys-csrf');
csrf.value = 'fixture-csrf';

// Created-key block: secret and payload are server-rendered and copy buttons
// must report the real clipboard outcome.
const created = element('div', 'card secret key-block', 'created-block');
const createdSecret = created.append(element('textarea', 'mono reveal-secret'));
createdSecret.value = 'key_created.fixture-secret';
const createdPayload = created.append(element('textarea', 'mono reveal-payload'));
createdPayload.value = '{"enrollmentKey":"key_created.fixture-secret"}';
const createdCopySecret = created.append(element('button', 'ghost copy-secret'));
const createdCopyPayload = created.append(element('button', 'ghost copy-payload'));
const createdStatus = created.append(element('div', 'reveal-status muted'));

// Live-key block: reveal via POST + copy + hide.
const live = element('div', 'card key-block', 'live-block');
const liveReveal = live.append(element('button', 'ghost reveal-button'));
liveReveal.setAttribute('data-reveal-url', '/console/keys/key_live/reveal');
const liveFields = live.append(element('div', 'reveal-fields'));
liveFields.hidden = true;
const liveSecret = liveFields.append(element('textarea', 'mono reveal-secret'));
const liveCopySecret = liveFields.append(element('button', 'ghost copy-secret'));
const livePayload = liveFields.append(element('textarea', 'mono reveal-payload'));
const liveCopyPayload = liveFields.append(element('button', 'ghost copy-payload'));
const liveQR = liveFields.append(element('img', 'qr reveal-qr'));
const liveHide = liveFields.append(element('button', 'ghost hide-button'));
const liveStatus = live.append(element('div', 'reveal-status muted'));

const byId = { 'keys-csrf': csrf };
const roots = [created, live];
const allElements = () => {
  const all = [];
  for (const root of roots) all.push(root, ...root.descendants());
  return all;
};

let secureContext = true;
const documentStub = {
  getElementById: (id) => byId[id] || null,
  querySelectorAll: (selector) => allElements().filter((element) => matchesSimple(element, selector)),
  querySelector: (selector) => allElements().filter((element) => matchesSimple(element, selector))[0] || null,
  execCommand: () => false,
};

// ---------- harness state ----------

let clipboard = null;
let copiedTexts = [];
let fetchedData = null;
let fetchCalls = 0;
const navigatorStub = {};
const windowStub = { isSecureContext: true };
const fetchStub = (url, options) => {
  fetchCalls += 1;
  return Promise.resolve({
    status: 200,
    json: () => Promise.resolve(fetchedData || {}),
  });
};

// ---------- scenarios ----------

const results = [];
function check(name, condition, detail) {
  results.push({ name, ok: !!condition, detail: detail || '' });
  if (!condition) console.error(`FAIL ${name}${detail ? ': ' + detail : ''}`);
}

function settle() {
  // A few microtask/macrotask turns let the handler's promise chain finish.
  return new Promise((resolve) => setTimeout(resolve, 0));
}

async function scenarioClipboardRejects() {
  secureContext = true;
  windowStub.isSecureContext = true;
  copiedTexts = [];
  navigatorStub.clipboard = {
    writeText: (text) => { copiedTexts.push(text); return Promise.reject(new Error('virtual clipboard rejected')); },
  };
  createdCopySecret.click();
  await settle();
  check('reject: no success claim', !createdStatus.textContent.includes('已复制'), createdStatus.textContent);
  check('reject: failure reported', createdStatus.textContent.includes('复制失败'), createdStatus.textContent);
  check('reject: error style', createdStatus.className.includes('err'), createdStatus.className);
  check('reject: offered selectable fallback', createdSecret.selected && createdSecret.focused);
}

async function scenarioClipboardResolves() {
  copiedTexts = [];
  navigatorStub.clipboard = {
    writeText: (text) => { copiedTexts.push(text); return Promise.resolve(); },
  };
  createdCopyPayload.click();
  await settle();
  check('resolve: payload copied verbatim', copiedTexts.length === 1 && copiedTexts[0] === createdPayload.value, JSON.stringify(copiedTexts));
  check('resolve: success reported', createdStatus.textContent.includes('已复制'), createdStatus.textContent);
  check('resolve: ok style', createdStatus.className.includes('ok'), createdStatus.className);
}

async function scenarioLegacyFallbackFails() {
  copiedTexts = [];
  delete navigatorStub.clipboard;
  documentStub.execCommand = () => false;
  createdCopySecret.click();
  await settle();
  check('no-clipboard reject: no success claim', !createdStatus.textContent.includes('已复制'), createdStatus.textContent);
  check('no-clipboard reject: failure reported', createdStatus.textContent.includes('复制失败'), createdStatus.textContent);
}

async function scenarioLegacyFallbackSucceeds() {
  delete navigatorStub.clipboard;
  documentStub.execCommand = () => true;
  createdCopyPayload.click();
  await settle();
  check('execCommand: success reported', createdStatus.textContent.includes('已复制'), createdStatus.textContent);
}

async function scenarioRevealThenCopyThenHide() {
  fetchedData = {
    keyId: 'key_live',
    enrollmentKey: 'key_live.revealed-secret',
    pairingPayload: '{"enrollmentKey":"key_live.revealed-secret"}',
    qrDataUri: 'data:image/png;base64,AAAA',
  };
  copiedTexts = [];
  navigatorStub.clipboard = {
    writeText: (text) => { copiedTexts.push(text); return Promise.resolve(); },
  };
  liveReveal.click();
  await settle();
  check('reveal: POST used once', fetchCalls === 1, String(fetchCalls));
  check('reveal: fields shown', liveFields.hidden === false);
  check('reveal: secret populated', liveSecret.value === 'key_live.revealed-secret', liveSecret.value);
  check('reveal: payload separated', livePayload.value.includes('enrollmentKey'));
  check('reveal: qr shown', liveQR.hidden === false && liveQR.src.startsWith('data:image/png;base64,'));
  check('reveal: reveal button hidden', liveReveal.hidden === true);

  liveCopySecret.click();
  await settle();
  check('reveal copy: value copied', copiedTexts.length === 1 && copiedTexts[0] === 'key_live.revealed-secret', JSON.stringify(copiedTexts));
  check('reveal copy: status ok', liveStatus.className.includes('ok'), liveStatus.className);

  liveHide.click();
  await settle();
  check('hide: fields collapsed', liveFields.hidden === true);
  check('hide: secret cleared from DOM', liveSecret.value === '');
  check('hide: payload cleared from DOM', livePayload.value === '');
  check('hide: qr cleared', liveQR.hidden === true && !liveQR.src);
  check('hide: reveal button restored', liveReveal.hidden === false);
}

async function main() {
  // Execute the production inline handler once; it wires all listeners.
  // eslint-disable-next-line no-new-func
  new Function('document', 'window', 'navigator', 'fetch', script)(
    documentStub, windowStub, navigatorStub, fetchStub
  );

  await scenarioClipboardRejects();
  await scenarioClipboardResolves();
  await scenarioLegacyFallbackFails();
  await scenarioLegacyFallbackSucceeds();
  await scenarioRevealThenCopyThenHide();

  const failures = results.filter((result) => !result.ok);
  if (failures.length > 0) {
    console.error(`${failures.length} copy-handler scenario(s) failed`);
    process.exit(1);
  }
  console.log(`ALL COPY HANDLER SCENARIOS PASSED (${results.length} checks)`);
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
