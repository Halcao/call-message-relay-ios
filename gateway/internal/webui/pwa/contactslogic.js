// Pure address-book suggestion/summary logic for the web client. Kept as a
// dependency-free module so it can be tested in Node (see
// harness/contactslogic-test.js) and reused verbatim by client.js.
(function (root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
  } else {
    root.CallRelayContactsLogic = factory();
  }
}(typeof self !== 'undefined' ? self : this, function () {
  'use strict';

  function digits(value) {
    return String(value == null ? '' : value).replace(/[^0-9]/g, '');
  }

  // canonicalKeys mirrors the server/iOS conservative rule: mainland
  // +86/0086/national spellings collapse for equality, but nothing is
  // broadened into a prefix match.
  function canonicalKeys(value) {
    var d = digits(value);
    if (!d) return [];
    var keys = [d];
    var local = d;
    if (/^0086[0-9]+$/.test(d) && d.length > 4) { local = d.slice(4); }
    else if (/^86[0-9]+$/.test(d) && d.length > 11 && d.length <= 13) { local = d.slice(2); }
    if (/^1[3-9][0-9]{9}$/.test(local)) {
      [local, '86' + local, '0086' + local].forEach(function (key) {
        if (keys.indexOf(key) < 0) keys.push(key);
      });
    }
    return keys;
  }

  function samePhone(a, b) {
    var left = canonicalKeys(a);
    var right = canonicalKeys(b);
    for (var i = 0; i < left.length; i++) {
      if (right.indexOf(left[i]) >= 0) return true;
    }
    return false;
  }

  function phoneValues(contact) {
    var values = [];
    (contact.phones || []).forEach(function (phone) {
      var value = phone && phone.value ? String(phone.value) : '';
      if (value) values.push(value);
    });
    return values;
  }

  // Chinese names read surname-first with no separator. A phone whose
  // contact display order is "given name, family name" stores the label as
  // "黛茜 樊"; reorder only that exact given+family rendering of two
  // Han-only parts, so nicknames and western names stay verbatim. A real
  // Han surname is 1-2 characters — longer "family names" come from an
  // account/nickname vCard whose N field was split by a space, and
  // reordering those produces nonsense.
  function isHanOnly(value) {
    return /^[㐀-䶿一-鿿豈-﫿]+$/.test(value || '');
  }
  function isHanSurname(value) {
    return isHanOnly(value) && Array.from(value).length <= 2;
  }
  function preferredName(stored, given, family) {
    stored = String(stored == null ? '' : stored).trim();
    given = String(given == null ? '' : given).trim();
    family = String(family == null ? '' : family).trim();
    if (!stored || !given || !family) return stored;
    if (!isHanOnly(given) || !isHanSurname(family)) return stored;
    var collapsed = stored.replace(/\s+/g, ' ');
    if (collapsed !== given + ' ' + family) return stored;
    return family + given;
  }

  // displayLabel is the stored label with only the CJK reordering applied —
  // no organization/phone fallback — for editable fields that must stay
  // empty when the owner gave no name.
  function displayLabel(contact) {
    return preferredName(contact.displayName, contact.givenName, contact.familyName);
  }

  function displayName(contact) {
    var stored = displayLabel(contact);
    if (stored) return stored;
    if (contact.organization) return String(contact.organization);
    var phones = phoneValues(contact);
    return phones.length ? phones[0] : '未命名联系人';
  }

  // matchesContact returns {match, isNameMatch, phone, score}. Numeric queries
  // match number fragments; text queries match the name and the precomputed
  // pinyin/T9 search key the phone app supplies at sync time.
  function matchesContact(contact, query) {
    var trimmed = String(query == null ? '' : query).trim();
    if (!trimmed) return { match: false, isNameMatch: false, phone: '', score: 99 };
    var lower = trimmed.toLowerCase();
    var digitsQuery = digits(trimmed);
    var phones = phoneValues(contact);
    if (digitsQuery && /^[0-9+*# \t-]+$/.test(trimmed)) {
      for (var i = 0; i < phones.length; i++) {
        var phoneDigits = digits(phones[i]);
        if (phoneDigits && phoneDigits.indexOf(digitsQuery) >= 0) {
          var suffix = phoneDigits.slice(-digitsQuery.length) === digitsQuery;
          return { match: true, isNameMatch: false, phone: phones[i], score: suffix ? 0 : 1 };
        }
      }
    }
    var name = displayName(contact).toLowerCase();
    if (name.indexOf(lower) === 0) {
      return { match: true, isNameMatch: true, phone: phones[0] || '', score: 2 };
    }
    if (lower && name.indexOf(lower) >= 0) {
      return { match: true, isNameMatch: true, phone: phones[0] || '', score: 3 };
    }
    var key = String(contact.searchKey || '').toLowerCase();
    if (key) {
      var tokens = key.split(/[\s,;]+/).filter(Boolean);
      for (var t = 0; t < tokens.length; t++) {
        if (tokens[t].indexOf(lower) === 0) {
          return { match: true, isNameMatch: true, phone: phones[0] || '', score: 4 };
        }
      }
      if (key.indexOf(lower) >= 0) {
        return { match: true, isNameMatch: true, phone: phones[0] || '', score: 5 };
      }
    }
    // A numeric query may also be part of the T9 search key.
    if (digitsQuery && key.indexOf(digitsQuery) >= 0) {
      return { match: true, isNameMatch: true, phone: phones[0] || '', score: 6 };
    }
    return { match: false, isNameMatch: false, phone: '', score: 99 };
  }

  function suggestions(contacts, query, limit) {
    var results = [];
    (contacts || []).forEach(function (contact) {
      var verdict = matchesContact(contact, query);
      if (!verdict.match) return;
      results.push({
        contact: contact,
        name: displayName(contact),
        phone: verdict.phone,
        isNameMatch: verdict.isNameMatch,
        hasMultipleNumbers: phoneValues(contact).length > 1,
        score: verdict.score
      });
    });
    results.sort(function (a, b) {
      if (a.score !== b.score) return a.score - b.score;
      return a.name.localeCompare(b.name, 'zh-Hans-CN');
    });
    if (limit && limit > 0) results = results.slice(0, limit);
    return results;
  }

  // contactForPhone finds the contact name to decorate a thread/peer row,
  // collapsing +86/national spellings exactly like the server and iOS.
  function contactForPhone(contacts, peer) {
    if (!digits(peer)) return null;
    for (var i = 0; i < (contacts || []).length; i++) {
      var phones = phoneValues(contacts[i]);
      for (var p = 0; p < phones.length; p++) {
        if (samePhone(phones[p], peer)) return contacts[i];
      }
    }
    return null;
  }

  function importSummary(result) {
    if (!result) return '';
    var parts = [];
    if (result.created) parts.push('新建 ' + result.created);
    if (result.updated) parts.push('合并 ' + result.updated);
    if (result.unchanged) parts.push('无变化 ' + result.unchanged);
    if (result.skipped) parts.push('跳过 ' + result.skipped);
    var text = parts.length ? parts.join(' · ') : '没有可导入的联系人';
    if (result.warnings && result.warnings.length) {
      text += '（' + result.warnings.length + ' 条提醒）';
    }
    return text;
  }

  function entryText(entry) {
    var action = entry && entry.action === 'create' ? '新建'
      : entry && entry.action === 'update' ? '合并'
      : entry && entry.action === 'skipped' ? '跳过（已删除）' : '无变化';
    var phones = (entry && entry.phones || []).join('、');
    return action + '：' + ((entry && entry.name) || '未命名') + (phones ? '（' + phones + '）' : '');
  }

  return {
    digits: digits,
    canonicalKeys: canonicalKeys,
    samePhone: samePhone,
    phoneValues: phoneValues,
    displayName: displayName,
    displayLabel: displayLabel,
    matchesContact: matchesContact,
    suggestions: suggestions,
    contactForPhone: contactForPhone,
    importSummary: importSummary,
    entryText: entryText
  };
}));
