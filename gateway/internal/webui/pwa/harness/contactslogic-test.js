// Node harness for contactslogic.js: suggestion matching by number, Chinese
// name and phone-synced pinyin/T9 search key, ranking/dedupe, and the
// user-facing import summary. Run: node harness/contactslogic-test.js
'use strict';

const assert = require('assert');
const logic = require('../contactslogic.js');

const contacts = [
  { id: 'c1', displayName: '张三', searchKey: 'zhangsan zs 9426',
    phones: [{ label: 'CELL', value: '+86 138 0013 8000' }, { label: 'WORK', value: '010-66668888' }] },
  { id: 'c2', displayName: '李四', searchKey: 'lisi ls 5474',
    phones: [{ label: 'CELL', value: '+86 139 0013 9000' }] },
  { id: 'c3', displayName: 'Alice Wang', searchKey: 'alicewang 8735550100',
    phones: [{ label: 'CELL', value: '+1 873-555-0100' }] },
];

// 1. Numeric fragment matches the formatted phone, suffix ranks first.
let hits = logic.suggestions(contacts, '8000', 4);
assert.strictEqual(hits.length, 1, 'numeric fragment should match one contact');
assert.strictEqual(hits[0].name, '张三');
assert.strictEqual(hits[0].isNameMatch, false);
assert.strictEqual(hits[0].phone, '+86 138 0013 8000');

// 2. Chinese name and pinyin search key both match.
hits = logic.suggestions(contacts, '李', 4);
assert.strictEqual(hits[0].name, '李四');
hits = logic.suggestions(contacts, 'zhang', 4);
assert.strictEqual(hits[0].name, '张三');
assert.strictEqual(hits[0].isNameMatch, true);

// 3. T9 digits embedded in the search key still find the contact.
hits = logic.suggestions(contacts, '9426', 4);
assert.strictEqual(hits[0].name, '张三');

// 4. International + formatted number match.
hits = logic.suggestions(contacts, '873555', 4);
assert.strictEqual(hits[0].name, 'Alice Wang');

// 5. One row per contact even when several of its numbers match; the
// multi-number badge is set for the contact with two numbers.
hits = logic.suggestions(contacts, '001', 4);
assert.strictEqual(hits.length, 2, 'two contacts contain the 001 fragment');
assert.strictEqual(hits.filter((h) => h.name === '张三').length, 1);
const zhang = logic.suggestions(contacts, '8000', 4)[0];
assert.strictEqual(zhang.hasMultipleNumbers, true);

// 6. Empty/no-match queries never return rows.
assert.deepStrictEqual(logic.suggestions(contacts, '', 4), []);
assert.deepStrictEqual(logic.suggestions(contacts, 'zzzz', 4), []);

// 7. contactForPhone decorates thread peers by raw digits.
assert.strictEqual(logic.contactForPhone(contacts, '+8613800138000').id, 'c1');
assert.strictEqual(logic.contactForPhone(contacts, '13900139000').id, 'c2');
assert.strictEqual(logic.contactForPhone(contacts, '555-0000'), null);

// 8. Import summary is concise product text.
assert.strictEqual(logic.importSummary({ created: 2, updated: 1, unchanged: 3, skipped: 0 }), '新建 2 · 合并 1 · 无变化 3');
assert.strictEqual(logic.importSummary({ created: 0, updated: 0, unchanged: 0, skipped: 0 }), '没有可导入的联系人');
assert.strictEqual(logic.importSummary({ created: 1, warnings: ['x'] }), '新建 1（1 条提醒）');
assert.strictEqual(logic.entryText({ action: 'create', name: '张三', phones: ['138'] }), '新建：张三（138）');
assert.strictEqual(logic.entryText({ action: 'unchanged', name: '李四' }), '无变化：李四');

// 9. Chinese names render surname-first without a separator. Only the exact
// "given family" FN rendering of Han-only parts is reordered; western names,
// latin given names and long non-surname "family" fields stay verbatim.
assert.strictEqual(logic.displayName({ displayName: '黛茜 樊', givenName: '黛茜', familyName: '樊' }), '樊黛茜');
assert.strictEqual(logic.displayName({ displayName: ' 昊   曹 ', givenName: '昊', familyName: '曹' }), '曹昊');
assert.strictEqual(logic.displayName({ displayName: '樊黛茜', givenName: '黛茜', familyName: '樊' }), '樊黛茜');
assert.strictEqual(logic.displayName({ displayName: 'Alice Wang', givenName: 'Alice', familyName: 'Wang' }), 'Alice Wang');
assert.strictEqual(logic.displayName({ displayName: 'Noora 崔', givenName: 'Noora', familyName: '崔' }), 'Noora 崔');
assert.strictEqual(logic.displayName({ displayName: '世界 莫名奇妙物语', givenName: '世界', familyName: '莫名奇妙物语' }), '世界 莫名奇妙物语');
assert.strictEqual(logic.displayLabel({ displayName: '', givenName: '', familyName: '' }), '');

// 10. The address book reads in pinyin order (阿 < 陈 < 李 < 欧阳 < 王 < 张 < 赵),
// not by code point — 张 must not sort before 陈 just because U+5F20 > U+9648.
function contact(id, displayName, extra) {
  return Object.assign({ id, displayName }, extra || {});
}
const unsorted = [
  contact('c1', '张三'), contact('c2', '李四'), contact('c3', '王五'),
  contact('c4', '阿明'), contact('c5', '陈'), contact('c6', '欧阳'),
];
assert.deepStrictEqual(
  logic.sortContacts(unsorted).map((c) => c.displayName),
  ['阿明', '陈', '李四', '欧阳', '王五', '张三']
);
// Sorting never reorders the caller's array.
assert.deepStrictEqual(
  unsorted.map((c) => c.displayName),
  ['张三', '李四', '王五', '阿明', '陈', '欧阳']
);
// Equal labels fall back to id, so rows never swap between renders.
assert.deepStrictEqual(
  logic.sortContacts([contact('b', '张三'), contact('a', '张三')]).map((c) => c.id),
  ['a', 'b']
);
// The surname-first label is what sorts: 樊黛茜 (fan) precedes 李四 (li).
assert.deepStrictEqual(
  logic.sortContacts([
    { id: 'x', displayName: '黛茜 樊', givenName: '黛茜', familyName: '樊' },
    contact('y', '李四'),
  ]).map((c) => logic.displayName(c)),
  ['樊黛茜', '李四']
);
// Latin labels follow the Han block, per ICU's zh ordering.
assert.deepStrictEqual(
  logic.sortContacts([contact('l', 'Alice Wang'), contact('h', '张三')]).map((c) => c.id),
  ['h', 'l']
);

console.log('contactslogic harness: all assertions passed');
