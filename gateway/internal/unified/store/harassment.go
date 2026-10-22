package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrVersionConflict reports a stale same-key settings write: two devices of
// one enrollment key edited the harassment policy concurrently and the later
// write did not carry the version it was based on. The caller re-reads and
// retries instead of silently overwriting the other device's change.
var ErrVersionConflict = errors.New("harassment settings version conflict")

// Harassment settings/list constants. Everything defaults to allow so an
// existing deployment stays inert until the owner explicitly opts in.
const (
	HarassMatchExact    = "exact"
	HarassMatchPrefix   = "prefix"
	HarassModeBlock     = "block"
	HarassModeTrust     = "trust"
	HarassActionReject  = "reject"
	HarassActionLabel   = "label"
	HarassActionSilence = "silence"
)

// HarassmentSettings is the per-enrollment-key master policy. Version is a
// monotonic counter for same-key devices: an update carries the version it
// was based on and is rejected with ErrVersionConflict when another device
// already moved it.
type HarassmentSettings struct {
	KeyID            string
	CallBlocking     bool
	SMSBlocking      bool
	ContactWhitelist bool
	Version          int64
	UpdatedAt        time.Time
}

// HarassmentSettingsUpdate is one settings write. HasContactWhitelist lets a
// legacy two-switch write keep the contact switch unchanged.
type HarassmentSettingsUpdate struct {
	CallBlocking        bool
	SMSBlocking         bool
	ContactWhitelist    bool
	HasContactWhitelist bool
}

func newHarassID(prefix string) string {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	return prefix + hex.EncodeToString(raw)
}

// HarassmentSettings returns the key's policy row, or all-default allow
// values when the key never configured one. Missing rows are never an error:
// a new/upgraded key is simply inert.
func (s *Store) HarassmentSettings(ctx context.Context, keyID string) (HarassmentSettings, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT key_id, call_blocking, sms_blocking, contact_whitelist, version, updated_at
		FROM harassment_settings WHERE key_id = ?`, keyID)
	return scanHarassmentSettings(row)
}

func scanHarassmentSettings(row rowScanner) (HarassmentSettings, error) {
	var (
		settings           HarassmentSettings
		call, sms, contact int
		updatedAt          int64
	)
	err := row.Scan(&settings.KeyID, &call, &sms, &contact, &settings.Version, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return HarassmentSettings{}, nil
	}
	if err != nil {
		return HarassmentSettings{}, err
	}
	settings.CallBlocking = call != 0
	settings.SMSBlocking = sms != 0
	settings.ContactWhitelist = contact != 0
	settings.UpdatedAt = fromMillis(updatedAt)
	return settings, nil
}

// SetHarassmentSettings persists the master call/SMS toggles and keeps the
// contact-whitelist switch unchanged (legacy two-switch callers).
func (s *Store) SetHarassmentSettings(ctx context.Context, keyID string, callBlocking, smsBlocking bool, baseVersion *int64) (HarassmentSettings, error) {
	return s.UpdateHarassmentSettings(ctx, keyID, HarassmentSettingsUpdate{
		CallBlocking: callBlocking, SMSBlocking: smsBlocking,
	}, baseVersion)
}

// UpdateHarassmentSettings persists a settings patch. baseVersion is
// optional: when non-nil a stale base is rejected with ErrVersionConflict;
// when nil (console/operator or first write) the write wins and bumps the
// version. All devices of one key share the row.
func (s *Store) UpdateHarassmentSettings(ctx context.Context, keyID string, update HarassmentSettingsUpdate, baseVersion *int64) (HarassmentSettings, error) {
	now := nowMillis()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return HarassmentSettings{}, err
	}
	defer tx.Rollback()
	current, contactCurrent, err := readHarassmentVersion(ctx, tx, keyID)
	if err != nil {
		return HarassmentSettings{}, err
	}
	if baseVersion != nil && *baseVersion != current {
		return HarassmentSettings{}, ErrVersionConflict
	}
	contact := update.ContactWhitelist
	if !update.HasContactWhitelist {
		contact = contactCurrent
	}
	next := current + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO harassment_settings (key_id, call_blocking, sms_blocking, contact_whitelist, version, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_id) DO UPDATE SET call_blocking = excluded.call_blocking,
		sms_blocking = excluded.sms_blocking, contact_whitelist = excluded.contact_whitelist,
		version = excluded.version, updated_at = excluded.updated_at`,
		keyID, boolInt(update.CallBlocking), boolInt(update.SMSBlocking), boolInt(contact), next, now); err != nil {
		return HarassmentSettings{}, err
	}
	if err := tx.Commit(); err != nil {
		return HarassmentSettings{}, err
	}
	return HarassmentSettings{
		KeyID: keyID, CallBlocking: update.CallBlocking, SMSBlocking: update.SMSBlocking,
		ContactWhitelist: contact, Version: next, UpdatedAt: fromMillis(now),
	}, nil
}

func readHarassmentVersion(ctx context.Context, tx *sql.Tx, keyID string) (version int64, contact bool, err error) {
	var contactInt int
	err = tx.QueryRowContext(ctx, `SELECT version, contact_whitelist FROM harassment_settings WHERE key_id = ?`, keyID).
		Scan(&version, &contactInt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return version, contactInt != 0, nil
}

// HarassmentEntry is one number rule. Imported entries carry list_id and
// source 'import' (default action label) so a bulk import is visibly
// distinguishable from manual rules and can be deleted as a unit.
type HarassmentEntry struct {
	ID        string
	KeyID     string
	Number    string
	MatchKind string
	Mode      string
	Action    string
	Source    string
	ListID    string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

func scanHarassmentEntry(row rowScanner) (HarassmentEntry, error) {
	var (
		entry            HarassmentEntry
		enabled          int
		created, updated int64
	)
	err := row.Scan(&entry.ID, &entry.KeyID, &entry.Number, &entry.MatchKind, &entry.Mode,
		&entry.Action, &entry.Source, &entry.ListID, &enabled, &created, &updated)
	if err != nil {
		return HarassmentEntry{}, err
	}
	entry.Enabled = enabled != 0
	entry.CreatedAt = fromMillis(created)
	entry.UpdatedAt = fromMillis(updated)
	return entry, nil
}

func (s *Store) ListHarassmentEntries(ctx context.Context, keyID string) ([]HarassmentEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, key_id, number, match_kind, mode, action, source, list_id, enabled, created_at, updated_at
		FROM harassment_entries WHERE key_id = ? ORDER BY created_at DESC`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []HarassmentEntry
	for rows.Next() {
		entry, scanErr := scanHarassmentEntry(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// normalizeHarassmentEntry applies the wire defaults and validation shared by
// the plain upsert and the contextual replace path.
func normalizeHarassmentEntry(entry HarassmentEntry) (HarassmentEntry, error) {
	entry.Number = strings.TrimSpace(entry.Number)
	if entry.Number == "" {
		return HarassmentEntry{}, fmt.Errorf("number required")
	}
	switch entry.MatchKind {
	case HarassMatchPrefix, "keyword", "regex":
	default:
		entry.MatchKind = HarassMatchExact
	}
	if entry.Mode != HarassModeTrust {
		entry.Mode = HarassModeBlock
	}
	if entry.Mode == HarassModeTrust {
		entry.Action = "allow"
	} else if entry.Action != HarassActionLabel {
		entry.Action = HarassActionReject
	}
	return entry, nil
}

// UpsertHarassmentEntry adds or reasserts one manual number rule. The same
// (number, match kind, mode) triple is deduplicated so repeated context-menu
// taps converge instead of stacking duplicates.
func (s *Store) UpsertHarassmentEntry(ctx context.Context, entry HarassmentEntry) (HarassmentEntry, error) {
	normalized, err := normalizeHarassmentEntry(entry)
	if err != nil {
		return HarassmentEntry{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return HarassmentEntry{}, err
	}
	defer tx.Rollback()
	saved, err := upsertHarassmentEntryTx(ctx, tx, normalized)
	if err != nil {
		return HarassmentEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return HarassmentEntry{}, err
	}
	return saved, nil
}

// ReplaceHarassmentEntries atomically removes the given same-key entries and
// upserts the new one in ONE transaction. The contextual block/trust actions
// use it with the canonical-equivalent opposite-mode exact entries so
// "Trust then Block" (and the reverse) cannot leave the opposite rule behind
// and falsely report success. Entries of other keys and unrelated/broader
// rules are never touched (the id list is scoped by key here as well).
func (s *Store) ReplaceHarassmentEntries(ctx context.Context, entry HarassmentEntry, removeIDs []string) (HarassmentEntry, int, error) {
	normalized, err := normalizeHarassmentEntry(entry)
	if err != nil {
		return HarassmentEntry{}, 0, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return HarassmentEntry{}, 0, err
	}
	defer tx.Rollback()
	removed := 0
	for _, id := range removeIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		result, delErr := tx.ExecContext(ctx, `DELETE FROM harassment_entries WHERE id = ? AND key_id = ?`, id, normalized.KeyID)
		if delErr != nil {
			return HarassmentEntry{}, 0, delErr
		}
		if affected, _ := result.RowsAffected(); affected > 0 {
			removed++
		}
	}
	saved, err := upsertHarassmentEntryTx(ctx, tx, normalized)
	if err != nil {
		return HarassmentEntry{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return HarassmentEntry{}, 0, err
	}
	return saved, removed, nil
}

// upsertHarassmentEntryTx is the shared insert/update used inside a caller's
// transaction.
func upsertHarassmentEntryTx(ctx context.Context, tx *sql.Tx, entry HarassmentEntry) (HarassmentEntry, error) {
	now := nowMillis()
	var existingID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM harassment_entries
		WHERE key_id = ? AND number = ? AND match_kind = ? AND mode = ?`,
		entry.KeyID, entry.Number, entry.MatchKind, entry.Mode).Scan(&existingID)
	if errors.Is(err, sql.ErrNoRows) {
		entry.ID = newHarassID("hx_")
		_, err = tx.ExecContext(ctx, `INSERT INTO harassment_entries
			(id, key_id, number, match_kind, mode, action, source, list_id, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 'manual', '', 1, ?, ?)`,
			entry.ID, entry.KeyID, entry.Number, entry.MatchKind, entry.Mode, entry.Action, now, now)
		if err != nil {
			return HarassmentEntry{}, err
		}
		entry.Source = "manual"
		entry.ListID = ""
		entry.Enabled = true
		entry.CreatedAt = fromMillis(now)
		entry.UpdatedAt = fromMillis(now)
		return entry, nil
	}
	if err != nil {
		return HarassmentEntry{}, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE harassment_entries SET action = ?, enabled = 1, source = 'manual', list_id = '', updated_at = ?
		WHERE id = ? AND key_id = ?`, entry.Action, now, existingID, entry.KeyID)
	if err != nil {
		return HarassmentEntry{}, err
	}
	entry.ID = existingID
	entry.Enabled = true
	entry.Source = "manual"
	entry.UpdatedAt = fromMillis(now)
	known, err := getHarassmentEntryTx(ctx, tx, entry.KeyID, existingID)
	if err != nil {
		return HarassmentEntry{}, err
	}
	return known, nil
}

func getHarassmentEntryTx(ctx context.Context, tx *sql.Tx, keyID, id string) (HarassmentEntry, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, key_id, number, match_kind, mode, action, source, list_id, enabled, created_at, updated_at
		FROM harassment_entries WHERE id = ? AND key_id = ?`, id, keyID)
	entry, err := scanHarassmentEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return HarassmentEntry{}, ErrNotFound
	}
	if err != nil {
		return HarassmentEntry{}, err
	}
	return entry, nil
}

func (s *Store) GetHarassmentEntry(ctx context.Context, keyID, id string) (HarassmentEntry, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT id, key_id, number, match_kind, mode, action, source, list_id, enabled, created_at, updated_at
		FROM harassment_entries WHERE id = ? AND key_id = ?`, id, keyID)
	entry, err := scanHarassmentEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return HarassmentEntry{}, ErrNotFound
	}
	if err != nil {
		return HarassmentEntry{}, err
	}
	return entry, nil
}

// DeleteHarassmentEntry removes one rule. A cross-key id is ErrNotFound so
// another key's rule can never be deleted (or even probed) through this API.
func (s *Store) DeleteHarassmentEntry(ctx context.Context, keyID, id string) error {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM harassment_entries WHERE id = ? AND key_id = ?`, id, keyID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// HarassmentList is one explicit import batch.
type HarassmentList struct {
	ID            string
	KeyID         string
	Name          string
	DefaultAction string
	Source        string
	EntryCount    int
	ImportedAt    time.Time
}

func (s *Store) ListHarassmentLists(ctx context.Context, keyID string) ([]HarassmentList, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, key_id, name, default_action, source, entry_count, imported_at
		FROM harassment_lists WHERE key_id = ? ORDER BY imported_at DESC`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lists []HarassmentList
	for rows.Next() {
		var (
			list       HarassmentList
			importedAt int64
		)
		if err := rows.Scan(&list.ID, &list.KeyID, &list.Name, &list.DefaultAction, &list.Source, &list.EntryCount, &importedAt); err != nil {
			return nil, err
		}
		list.ImportedAt = fromMillis(importedAt)
		lists = append(lists, list)
	}
	return lists, rows.Err()
}

// ImportHarassmentList inserts an explicit, user-triggered list. defaultAction
// defaults to label (mark only): a historical list never starts rejecting
// calls or SMS unless the owner deliberately chose reject. Duplicate numbers
// are inserted once, and an empty number set is refused.
func (s *Store) ImportHarassmentList(ctx context.Context, keyID, name, defaultAction string, numbers []string) (HarassmentList, error) {
	if defaultAction != HarassActionReject {
		defaultAction = HarassActionLabel
	}
	seen := map[string]bool{}
	cleaned := make([]string, 0, len(numbers))
	for _, raw := range numbers {
		value := strings.TrimSpace(raw)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		cleaned = append(cleaned, value)
	}
	if len(cleaned) == 0 {
		return HarassmentList{}, fmt.Errorf("empty list")
	}
	if len(cleaned) > 5000 {
		cleaned = cleaned[:5000]
	}
	if strings.TrimSpace(name) == "" {
		name = "导入名单"
	}
	now := nowMillis()
	list := HarassmentList{ID: newHarassID("hl_"), KeyID: keyID, Name: strings.TrimSpace(name),
		DefaultAction: defaultAction, Source: "import", EntryCount: len(cleaned), ImportedAt: fromMillis(now)}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return HarassmentList{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO harassment_lists (id, key_id, name, default_action, source, entry_count, imported_at)
		VALUES (?, ?, ?, ?, 'import', ?, ?)`, list.ID, keyID, list.Name, defaultAction, len(cleaned), now); err != nil {
		return HarassmentList{}, err
	}
	statement, err := tx.PrepareContext(ctx, `INSERT INTO harassment_entries
		(id, key_id, number, match_kind, mode, action, source, list_id, enabled, created_at, updated_at)
		VALUES (?, ?, ?, 'exact', 'block', ?, 'import', ?, 1, ?, ?)`)
	if err != nil {
		return HarassmentList{}, err
	}
	defer statement.Close()
	for _, number := range cleaned {
		if _, err := statement.ExecContext(ctx, newHarassID("hx_"), keyID, number, defaultAction, list.ID, now, now); err != nil {
			return HarassmentList{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return HarassmentList{}, err
	}
	return list, nil
}

// DeleteHarassmentList removes the list and every entry that came from it.
// Manual rules the owner added are untouched (they carry source 'manual').
func (s *Store) DeleteHarassmentList(ctx context.Context, keyID, listID string) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM harassment_entries WHERE key_id = ? AND list_id = ?`, keyID, listID)
	if err != nil {
		return 0, err
	}
	removed, _ := result.RowsAffected()
	del, err := tx.ExecContext(ctx, `DELETE FROM harassment_lists WHERE id = ? AND key_id = ?`, listID, keyID)
	if err != nil {
		return 0, err
	}
	if affected, _ := del.RowsAffected(); affected == 0 {
		return 0, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return removed, nil
}

// HarassmentEvent is one block-history row.
type HarassmentEvent struct {
	ID        string
	KeyID     string
	LineID    string
	Kind      string
	Peer      string
	Action    string
	Reason    string
	RefID     string
	CreatedAt time.Time
}

func (s *Store) AppendHarassmentEvent(ctx context.Context, event HarassmentEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO harassment_events
		(id, key_id, line_id, kind, peer, action, reason, ref_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newHarassID("he_"), event.KeyID, event.LineID, event.Kind, event.Peer, event.Action, event.Reason, event.RefID, millis(event.CreatedAt))
	return err
}

func (s *Store) ListHarassmentEvents(ctx context.Context, keyID string, limit int) ([]HarassmentEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id, key_id, line_id, kind, peer, action, reason, ref_id, created_at
		FROM harassment_events WHERE key_id = ? ORDER BY created_at DESC LIMIT ?`, keyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []HarassmentEvent
	for rows.Next() {
		var (
			event     HarassmentEvent
			createdAt int64
		)
		if err := rows.Scan(&event.ID, &event.KeyID, &event.LineID, &event.Kind, &event.Peer,
			&event.Action, &event.Reason, &event.RefID, &createdAt); err != nil {
			return nil, err
		}
		event.CreatedAt = fromMillis(createdAt)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) ClearHarassmentEvents(ctx context.Context, keyID string) (int64, error) {
	result, err := s.DB.ExecContext(ctx, `DELETE FROM harassment_events WHERE key_id = ?`, keyID)
	if err != nil {
		return 0, err
	}
	affected, _ := result.RowsAffected()
	return affected, nil
}

// HarassmentPolicy is the complete evaluation input for one key: settings
// plus enabled entries. Loaded with two queries per inbound decision.
type HarassmentPolicy struct {
	CallBlocking     bool
	SMSBlocking      bool
	ContactWhitelist bool
	Entries          []HarassmentEntry
}

func (s *Store) HarassmentPolicy(ctx context.Context, keyID string) (HarassmentPolicy, error) {
	settings, err := s.HarassmentSettings(ctx, keyID)
	if err != nil {
		return HarassmentPolicy{}, err
	}
	entries, err := s.ListHarassmentEntries(ctx, keyID)
	if err != nil {
		return HarassmentPolicy{}, err
	}
	policy := HarassmentPolicy{
		CallBlocking: settings.CallBlocking, SMSBlocking: settings.SMSBlocking,
		ContactWhitelist: settings.ContactWhitelist,
	}
	for _, entry := range entries {
		if entry.Enabled {
			policy.Entries = append(policy.Entries, entry)
		}
	}
	return policy, nil
}

// LineReceiverKeys lists the enrollment keys that effectively hold the
// receive_calls (or receive_sms) grant on one line. all_lines keys hold it
// unless an explicit override removes it; an explicit grant always wins over
// the all_lines default remember. Revoked keys are excluded, so revocation
// immediately removes their devices from ringing/push evaluation.
func (s *Store) LineReceiverKeys(ctx context.Context, lineID string, calls bool) ([]string, error) {
	column := "receive_sms"
	if calls {
		column = "receive_calls"
	}
	overrides := map[string]bool{}
	rows, err := s.DB.QueryContext(ctx, `SELECT key_id, `+column+` FROM key_lines WHERE line_id = ?`, lineID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			keyID   string
			allowed int
		)
		if err := rows.Scan(&keyID, &allowed); err != nil {
			rows.Close()
			return nil, err
		}
		overrides[keyID] = allowed != 0
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keys, err := s.ListEnrollmentKeys(ctx)
	if err != nil {
		return nil, err
	}
	var receivers []string
	for _, key := range keys {
		if key.RevokedAt != nil {
			continue
		}
		if override, ok := overrides[key.ID]; ok {
			if override {
				receivers = append(receivers, key.ID)
			}
			continue
		}
		if key.AllLines {
			receivers = append(receivers, key.ID)
		}
	}
	return receivers, nil
}

// HasExplicitKeyLine reports whether a key has an explicit grant row for the
// line (used by tests and diagnostics; the effective decision lives in
// LineReceiverKeys/DeviceAccess).
func (s *Store) HasExplicitKeyLine(ctx context.Context, keyID, lineID string) (bool, error) {
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM key_lines WHERE key_id = ? AND line_id = ?`,
		keyID, lineID).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// PrincipalContactPhones returns the normalized phone values of this key's
// gateway-synced contacts (the principal the key already maps to). It never
// creates a principal and never uploads anything: an empty map simply means
// the owner has not synced contacts to this gateway.
func (s *Store) PrincipalContactPhones(ctx context.Context, keyID string) (map[string]bool, error) {
	principalID, ok, err := s.PrincipalBinding(ctx, keyID)
	if err != nil || !ok {
		return map[string]bool{}, err
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT f.normalized FROM contact_fields f
		JOIN contacts c ON c.id = f.contact_id
		WHERE c.principal_id = ? AND c.deleted_at IS NULL AND f.kind = 'phone' AND f.normalized != ''`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	phones := map[string]bool{}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		phones[value] = true
	}
	return phones, rows.Err()
}
