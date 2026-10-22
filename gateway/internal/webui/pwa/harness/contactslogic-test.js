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

console.log('contactslogic harness: all assertions passed');
