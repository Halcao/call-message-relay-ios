package runtime

import (
	"context"
	"log/slog"

	"callrelay/gateway/internal/unified/contacts"
	"callrelay/gateway/internal/unified/harass"
	"callrelay/gateway/internal/unified/store"
)

// HarassmentDecision evaluates one enrollment key's harassment policy for an
// inbound peer (and, for SMS, the message body). Store errors fail OPEN: an
// unreadable policy must never turn into a silent call/SMS block; only an
// explicit rule may suppress delivery.
func (m *Manager) HarassmentDecision(ctx context.Context, keyID string, kind harass.Kind, peer, body string) harass.Decision {
	policy, err := m.Store.HarassmentPolicy(ctx, keyID)
	if err != nil {
		slog.Warn("harassment policy unavailable", "key_id", keyID, "kind", string(kind), "error", err)
		return harass.Decision{Allow: true, Action: "allow", Reason: "policy_unavailable"}
	}
	entries := make([]harass.Entry, 0, len(policy.Entries))
	for _, entry := range policy.Entries {
		entries = append(entries, harass.Entry{
			ID: entry.ID, Kind: entry.MatchKind, Pattern: entry.Number, Mode: entry.Mode,
			Action: entry.Action, ListID: entry.ListID, Source: entry.Source,
		})
	}
	policyView := harass.Policy{
		CallBlocking: policy.CallBlocking, SMSBlocking: policy.SMSBlocking,
		ContactWhitelist: policy.ContactWhitelist, Entries: entries,
	}
	// Contact trust is resolved lazily and only when the owner enabled it;
	// the contacts come from the key's own gateway-synced address book (the
	// existing principal mapping), never from an implicit upload.
	if policy.ContactWhitelist {
		policyView.ContactTrusted = func(candidate string) bool {
			return m.contactTrusted(ctx, keyID, candidate)
		}
	}
	return policyView.Evaluate(kind, peer, body)
}

// contactTrusted reports whether one peer matches a phone in this key's
// synced contacts.
func (m *Manager) contactTrusted(ctx context.Context, keyID, peer string) bool {
	phones, err := m.Store.PrincipalContactPhones(ctx, keyID)
	if err != nil || len(phones) == 0 {
		return false
	}
	for _, key := range contacts.CanonicalKeys(peer) {
		if phones[key] {
			return true
		}
	}
	return false
}

// HarassSuppressedForKey reports whether the key's policy denies this inbound
// item. Used by push fan-out and the event stream so a suppressing key never
// receives an event/ring/VoIP push for it (another key on the same line is
// unaffected).
func (m *Manager) HarassSuppressedForKey(ctx context.Context, keyID string, kind harass.Kind, peer, body string) bool {
	return m.HarassmentDecision(ctx, keyID, kind, peer, body).Suppressed
}

// recordHarassEvent writes one key-scoped block-history row. Explicit manual
// allows (trusted) and unremarkable defaults are not history: only genuine
// rejects and label decisions are recorded.
func (m *Manager) recordHarassEvent(ctx context.Context, keyID, lineID string, kind harass.Kind, peer string, decision harass.Decision, refID string) {
	if decision.Action != "reject" && decision.Action != "label" {
		return
	}
	_ = m.Store.AppendHarassmentEvent(ctx, store.HarassmentEvent{
		KeyID: keyID, LineID: lineID, Kind: string(kind), Peer: peer,
		Action: decision.Action, Reason: decision.Reason, RefID: refID,
	})
}

// harassmentCallReceivers partitions the keys that normally receive inbound
// calls on a line into policy-allowed and policy-suppressed sets. Receivers
// include offline devices and all_lines keys: a merely-offline device is
// NEVER treated as a denial. The suppressed history rows are written here so
// a blocked call leaves one truthful record per suppressing key.
func (m *Manager) harassmentCallReceivers(ctx context.Context, lineID, peer, callID string) (allowed, suppressed []string) {
	keys, err := m.Store.LineReceiverKeys(ctx, lineID, true)
	if err != nil {
		return nil, nil
	}
	for _, keyID := range keys {
		decision := m.HarassmentDecision(ctx, keyID, harass.KindCall, peer, "")
		if decision.Suppressed {
			suppressed = append(suppressed, keyID)
			m.recordHarassEvent(ctx, keyID, lineID, harass.KindCall, peer, decision, callID)
			continue
		}
		allowed = append(allowed, keyID)
	}
	return allowed, suppressed
}

// crossKeyRingingRemains reports whether any OTHER enrollment key still has a
// device that is authorized, not suppressed by its own policy, and not the
// actor, on this ringing call. A device-scoped hangup/reject must never kill
// a call another key is still offered.
func (m *Manager) crossKeyRingingRemains(ctx context.Context, lineID, peer, actorDeviceID string) bool {
	actor, err := m.Store.GetDevice(ctx, actorDeviceID)
	if err != nil {
		return false
	}
	devices, err := m.Store.ListDevices(ctx)
	if err != nil {
		return false
	}
	for _, device := range devices {
		if device.ID == actorDeviceID || device.RevokedAt != nil || device.KeyID == actor.KeyID {
			continue
		}
		access, accessErr := m.Store.DeviceAccess(ctx, device.ID)
		if accessErr != nil {
			continue
		}
		entry, ok := linePermission(access, lineID)
		if !ok || !entry.Permissions.ReceiveCalls {
			continue
		}
		if m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindCall, peer, "") {
			continue
		}
		return true
	}
	return false
}

// HarassmentEntryResult is the truthful outcome of one entry write. Effective
// is false when a broader TRUST rule (or another key-independent override)
// still allows the number, so the API/UI never reports a contextual block as
// success while the caller is in fact still allowed.
type HarassmentEntryResult struct {
	Entry     store.HarassmentEntry
	Replaced  int
	Effective bool
	Note      string
}

// AddHarassmentEntry writes one key-scoped rule. With replaceOpposite the
// canonically-equivalent OPPOSITE-mode exact rules of the same key are
// removed in the same transaction (contextual block replaces trust and vice
// versa); broader rules and other keys are never touched. Bulk imports use
// replaceOpposite=false to preserve the configured precedence.
func (m *Manager) AddHarassmentEntry(ctx context.Context, keyID string, entry store.HarassmentEntry, replaceOpposite bool) (HarassmentEntryResult, error) {
	entry.KeyID = keyID
	removed := 0
	var saved store.HarassmentEntry
	var err error
	if replaceOpposite && (entry.MatchKind == "" || entry.MatchKind == store.HarassMatchExact) {
		existing, listErr := m.Store.ListHarassmentEntries(ctx, keyID)
		if listErr != nil {
			return HarassmentEntryResult{}, listErr
		}
		opposite := store.HarassModeBlock
		if entry.Mode == store.HarassModeBlock {
			opposite = store.HarassModeTrust
		}
		var removeIDs []string
		for _, candidate := range existing {
			if candidate.Mode != opposite || candidate.MatchKind != store.HarassMatchExact {
				continue
			}
			if contacts.SamePhone(candidate.Number, entry.Number) {
				removeIDs = append(removeIDs, candidate.ID)
			}
		}
		saved, removed, err = m.Store.ReplaceHarassmentEntries(ctx, entry, removeIDs)
	} else {
		saved, err = m.Store.UpsertHarassmentEntry(ctx, entry)
	}
	if err != nil {
		return HarassmentEntryResult{}, err
	}
	result := HarassmentEntryResult{Entry: saved, Replaced: removed, Effective: true}
	if saved.Mode == store.HarassModeBlock {
		settings, settingsErr := m.Store.HarassmentSettings(ctx, keyID)
		kind := harass.KindCall
		if settingsErr == nil && !settings.CallBlocking && settings.SMSBlocking {
			kind = harass.KindSMS
		}
		decision := m.HarassmentDecision(ctx, keyID, kind, saved.Number, "")
		if decision.Allow {
			result.Effective = false
			if decision.Reason == "trusted" && decision.Pattern != "" {
				result.Note = "已有更宽的信任规则（" + decision.Pattern + "），该号码仍会放行。"
			} else {
				result.Note = "当前策略下该号码仍会放行。"
			}
		}
	}
	return result, nil
}
