#!/usr/bin/env node
/* Gateway call-event envelope parsing: an actual runtime envelope
 * ({type:'call.updated', data: CallView}) must surface an inbound ringing
 * call while idle, and must NEVER steal/dup the active call.
 */
'use strict';

const path = require('path');
const { parseIncomingCallEvent } = require(path.join(__dirname, '..', 'callevents.js'));

let failures = 0;
function check(name, cond, detail) {
  if (cond) { console.log('ok   - ' + name); }
  else { failures += 1; console.error('FAIL - ' + name + (detail ? ' :: ' + detail : '')); }
}

const ringing = {
  type: 'call.updated',
  lineId: 'line1',
  data: { id: 'call_x', direction: 'inbound', state: 'incomingRinging', peer: '555-0123', lineName: '主卡' },
};

check('runtime envelope -> incoming presented',
  parseIncomingCallEvent(ringing, null) &&
  parseIncomingCallEvent(ringing, null).id === 'call_x');

check('active call is not stolen',
  parseIncomingCallEvent(ringing, 'call_x') === null);

check('active OTHER call still surfaces this one',
  parseIncomingCallEvent(ringing, 'call_other') !== null);

check('outgoing leg ignored',
  parseIncomingCallEvent({ type: 'call.updated', data: { id: 'o1', direction: 'outbound', state: 'outgoingDialing' } }, null) === null);

check('connected call ignored',
  parseIncomingCallEvent({ type: 'call.updated', data: { id: 'c2', direction: 'inbound', state: 'active' } }, null) === null);

check('non-call envelope ignored',
  parseIncomingCallEvent({ type: 'message.created', data: {} }, null) === null);

check('null envelope safe', parseIncomingCallEvent(null, null) === null);

// Field regression: the real unified gateway emits snake_case states
// (`incoming_ringing`); the PWA must present those rings too.
const snakeRinging = {
  type: 'call.incoming',
  data: { id: 'call_snake', direction: 'inbound', state: 'incoming_ringing', peer: '+8613800000042' },
};
const snakeParsed = parseIncomingCallEvent(snakeRinging, null);
check('snake_case incoming_ringing -> incoming presented',
  Boolean(snakeParsed) && snakeParsed.id === 'call_snake',
  JSON.stringify(snakeParsed));
check('snake_case active call is not stolen',
  parseIncomingCallEvent(snakeRinging, 'call_snake') === null);

console.log(failures === 0 ? 'ALL PASS' : failures + ' FAILURES');
process.exit(failures === 0 ? 0 : 1);
