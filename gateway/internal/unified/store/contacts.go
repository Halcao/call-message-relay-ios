package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"callrelay/gateway/internal/id"
)

// ErrPrincipalConflict is returned when an explicit key rotation would merge
// a principal with one that already owns contacts. Two unrelated keys are
// never merged, even if their phone values happen to look identical.
var ErrPrincipalConflict = errors.New("principal key conflict")

// ContactField is one labeled value (phone number, email, URL) of a contact.
// Normalized is the server-computed match form (digits for phones, lowercased
// address for emails); it never changes the value shown to the owner.
type ContactField struct {
	Kind       string
	Ordinal    int
	Label      string
	Value      string
	Normalized string
}

// Contact is a principal-owned address-book entry. Deleted is only set when
// the entry is returned by the sync change feed (a revisioned tombstone);
// display queries never include tombstones.
type Contact struct {
	ID           string
	PrincipalID  string
	DisplayName  string
	GivenName    string
	FamilyName   string
	Organization string
	Nickname     string
	Note         string
	SearchKey    string
	Source       string
	ClientRef    string
	Revision     int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Deleted      bool
	Phones       []ContactField
	Emails       []ContactField
	URLs         []ContactField
}

// ContactPage is one page of contacts with an honest cursor. Contacts holds
// the page; HasMore means the page was truncated; NextSince is the cursor to
// pass to the next call (never advance a client to Revision while HasMore is
// true, or truncated entries are silently lost).
type ContactPage struct {
	Contacts  []Contact
	Revision  int64
	HasMore   bool
	NextSince int64
}

// PrincipalForKey resolves the stable contact principal for a pairing key,
// creating it on first use. Concurrent first use is safe: the bind insert is
// a no-op on an existing row and the winner's mapping is re-read, so two
// devices enrolling at the same time always converge on one principal.
func (s *Store) PrincipalForKey(ctx context.Context, keyID string) (string, error) {
	if strings.TrimSpace(keyID) == "" {
		return "", fmt.Errorf("principal key is required")
	}
	if principalID, ok, err := s.PrincipalBinding(ctx, keyID); err != nil || ok {
		return principalID, err
	}
	principalID, err := id.New("prn_", 12)
	if err != nil {
		return "", err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin principal: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO principals (id, created_at) VALUES (?, ?) ON CONFLICT(id) DO NOTHING`,
		principalID, nowMillis()); err != nil {
		return "", fmt.Errorf("create principal: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO principal_keys (key_id, principal_id, bound_at) VALUES (?, ?, ?)
		 ON CONFLICT(key_id) DO NOTHING`,
		keyID, principalID, nowMillis()); err != nil {
		return "", fmt.Errorf("bind principal key: %w", err)
	}
	var bound string
	if err := tx.QueryRowContext(ctx,
		`SELECT principal_id FROM principal_keys WHERE key_id = ?`, keyID).Scan(&bound); err != nil {
		return "", fmt.Errorf("read principal mapping: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit principal: %w", err)
	}
	return bound, nil
}

// PrincipalBinding reports the existing mapping for a key without creating one.
func (s *Store) PrincipalBinding(ctx context.Context, keyID string) (string, bool, error) {
	var principalID string
	err := s.DB.QueryRowContext(ctx,
		`SELECT principal_id FROM principal_keys WHERE key_id = ?`, keyID).Scan(&principalID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read principal mapping: %w", err)
	}
	return principalID, true, nil
}

// RotatePrincipalKey re-points a new pairing key at the principal of an
// explicitly renewed old key, preserving contacts across rotation. It refuses
// to merge two principals that both own contacts.
func (s *Store) RotatePrincipalKey(ctx context.Context, oldKeyID, newKeyID string) error {
	if strings.TrimSpace(oldKeyID) == "" || strings.TrimSpace(newKeyID) == "" {
		return fmt.Errorf("rotation keys are required")
	}
	var principalID string
	err := s.DB.QueryRowContext(ctx,
		`SELECT principal_id FROM principal_keys WHERE key_id = ?`, oldKeyID).Scan(&principalID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read principal mapping: %w", err)
	}
	existing, ok, err := s.PrincipalBinding(ctx, newKeyID)
	if err != nil {
		return err
	}
	if ok && existing != principalID {
		return ErrPrincipalConflict
	}
	if ok {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rotation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE principal_keys SET rotated_at = ? WHERE key_id = ?`, nowMillis(), oldKeyID); err != nil {
		return fmt.Errorf("mark rotated key: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO principal_keys (key_id, principal_id, bound_at) VALUES (?, ?, ?)`,
		newKeyID, principalID, nowMillis()); err != nil {
		return fmt.Errorf("bind rotated key: %w", err)
	}
	return tx.Commit()
}

// PrincipalRevision returns the monotonic change counter for a principal.
func (s *Store) PrincipalRevision(ctx context.Context, principalID string) (int64, error) {
	var revision int64
	err := s.DB.QueryRowContext(ctx,
		`SELECT revision FROM contact_sync_state WHERE principal_id = ?`, principalID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read contact revision: %w", err)
	}
	return revision, nil
}

func bumpContactRevision(ctx context.Context, tx *sql.Tx, principalID string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contact_sync_state (principal_id, revision, updated_at) VALUES (?, 1, ?)
		ON CONFLICT(principal_id) DO UPDATE SET revision = revision + 1, updated_at = excluded.updated_at`,
		principalID, nowMillis()); err != nil {
		return 0, fmt.Errorf("bump contact revision: %w", err)
	}
	var revision int64
	if err := tx.QueryRowContext(ctx,
		`SELECT revision FROM contact_sync_state WHERE principal_id = ?`, principalID).Scan(&revision); err != nil {
		return 0, fmt.Errorf("read contact revision: %w", err)
	}
	return revision, nil
}

func insertContactFields(ctx context.Context, tx *sql.Tx, contactID string, fields []ContactField) error {
	for _, field := range fields {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO contact_fields (contact_id, kind, ordinal, label, value, normalized)
			VALUES (?, ?, ?, ?, ?, ?)`,
			contactID, field.Kind, field.Ordinal, field.Label, field.Value, field.Normalized); err != nil {
			return fmt.Errorf("insert contact field: %w", err)
		}
	}
	return nil
}

// CreateContact inserts a new principal-owned contact and returns it with its
// assigned id and revision.
func (s *Store) CreateContact(ctx context.Context, contact Contact) (Contact, error) {
	if contact.PrincipalID == "" {
		return Contact{}, fmt.Errorf("contact principal is required")
	}
	contactID, err := id.New("ct_", 12)
	if err != nil {
		return Contact{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Contact{}, fmt.Errorf("begin contact: %w", err)
	}
	defer tx.Rollback()
	revision, err := bumpContactRevision(ctx, tx, contact.PrincipalID)
	if err != nil {
		return Contact{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO contacts (id, principal_id, display_name, given_name, family_name, organization,
			nickname, note, search_key, source, client_ref, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		contactID, contact.PrincipalID, contact.DisplayName, contact.GivenName, contact.FamilyName,
		contact.Organization, contact.Nickname, contact.Note, contact.SearchKey, contact.Source,
		contact.ClientRef, revision, millis(now), millis(now)); err != nil {
		return Contact{}, fmt.Errorf("insert contact: %w", err)
	}
	if err := insertContactFields(ctx, tx, contactID, contact.Phones); err != nil {
		return Contact{}, err
	}
	if err := insertContactFields(ctx, tx, contactID, contact.Emails); err != nil {
		return Contact{}, err
	}
	if err := insertContactFields(ctx, tx, contactID, contact.URLs); err != nil {
		return Contact{}, err
	}
	if err := tx.Commit(); err != nil {
		return Contact{}, fmt.Errorf("commit contact: %w", err)
	}
	contact.ID = contactID
	contact.Revision = revision
	contact.CreatedAt = now
	contact.UpdatedAt = now
	return contact, nil
}

// UpdateContact replaces the mutable fields of one principal-owned contact.
// The principal predicate is enforced in SQL: another principal's contact id
// can never match.
func (s *Store) UpdateContact(ctx context.Context, contact Contact) (Contact, error) {
	if contact.PrincipalID == "" || contact.ID == "" {
		return Contact{}, fmt.Errorf("contact principal and id are required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Contact{}, fmt.Errorf("begin contact: %w", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM contacts WHERE id = ? AND principal_id = ? AND deleted_at IS NULL`,
		contact.ID, contact.PrincipalID).Scan(&exists); err != nil {
		return Contact{}, err
	}
	if exists == 0 {
		return Contact{}, ErrNotFound
	}
	revision, err := bumpContactRevision(ctx, tx, contact.PrincipalID)
	if err != nil {
		return Contact{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE contacts SET display_name = ?, given_name = ?, family_name = ?, organization = ?,
			nickname = ?, note = ?, search_key = ?, source = ?, client_ref = ?, revision = ?, updated_at = ?
		WHERE id = ? AND principal_id = ?`,
		contact.DisplayName, contact.GivenName, contact.FamilyName, contact.Organization,
		contact.Nickname, contact.Note, contact.SearchKey, contact.Source, contact.ClientRef,
		revision, millis(now), contact.ID, contact.PrincipalID); err != nil {
		return Contact{}, fmt.Errorf("update contact: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM contact_fields WHERE contact_id = ?`, contact.ID); err != nil {
		return Contact{}, fmt.Errorf("replace contact fields: %w", err)
	}
	if err := insertContactFields(ctx, tx, contact.ID, contact.Phones); err != nil {
		return Contact{}, err
	}
	if err := insertContactFields(ctx, tx, contact.ID, contact.Emails); err != nil {
		return Contact{}, err
	}
	if err := insertContactFields(ctx, tx, contact.ID, contact.URLs); err != nil {
		return Contact{}, err
	}
	if err := tx.Commit(); err != nil {
		return Contact{}, fmt.Errorf("commit contact: %w", err)
	}
	contact.Revision = revision
	contact.UpdatedAt = now
	return contact, nil
}

// DeleteContact soft-deletes one principal-owned contact. Contacts from a
// different principal can never be addressed: the principal predicate is part
// of every statement. The tombstone carries the bumped principal revision so
// incremental sync clients learn about the deletion instead of keeping a
// stale local copy that would later be re-uploaded (resurrection).
func (s *Store) DeleteContact(ctx context.Context, principalID, contactID string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete: %w", err)
	}
	defer tx.Rollback()
	revision, err := bumpContactRevision(ctx, tx, principalID)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE contacts SET deleted_at = ?, revision = ?, updated_at = ?
		 WHERE id = ? AND principal_id = ? AND deleted_at IS NULL`,
		nowMillis(), revision, nowMillis(), contactID, principalID)
	if err != nil {
		return fmt.Errorf("delete contact: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// TombstonedClientRefs returns the client refs of this principal's deleted
// contacts, so a mobile upsert can refuse to resurrect a contact the owner
// deleted on another surface unless it explicitly asks to restore it.
func (s *Store) TombstonedClientRefs(ctx context.Context, principalID string) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT client_ref FROM contacts WHERE principal_id = ? AND deleted_at IS NOT NULL AND client_ref != ''`,
		principalID)
	if err != nil {
		return nil, fmt.Errorf("list contact tombstones: %w", err)
	}
	defer rows.Close()
	refs := map[string]bool{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs[ref] = true
	}
	return refs, rows.Err()
}

// AllContacts returns every active contact of a principal. It is bounded by
// the per-principal limit enforced at the service layer; matching a full
// address book must never run against a truncated page.
func (s *Store) AllContacts(ctx context.Context, principalID string) ([]Contact, int64, error) {
	var contacts []Contact
	revision, err := s.PrincipalRevision(ctx, principalID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT c.id, c.principal_id, c.display_name, c.given_name, c.family_name, c.organization,
			c.nickname, c.note, c.search_key, c.source, c.client_ref, c.revision, c.created_at, c.updated_at
		FROM contacts c WHERE c.principal_id = ? AND c.deleted_at IS NULL
		ORDER BY c.updated_at DESC`, principalID)
	if err != nil {
		return nil, 0, fmt.Errorf("list contacts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var contact Contact
		var createdAt, updatedAt int64
		if err := rows.Scan(&contact.ID, &contact.PrincipalID, &contact.DisplayName, &contact.GivenName,
			&contact.FamilyName, &contact.Organization, &contact.Nickname, &contact.Note,
			&contact.SearchKey, &contact.Source, &contact.ClientRef, &contact.Revision,
			&createdAt, &updatedAt); err != nil {
			return nil, 0, err
		}
		contact.CreatedAt = fromMillis(createdAt)
		contact.UpdatedAt = fromMillis(updatedAt)
		contacts = append(contacts, contact)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(contacts) > 0 {
		if err := s.attachContactFields(ctx, contacts); err != nil {
			return nil, 0, err
		}
	}
	return contacts, revision, nil
}

// ListContacts returns one display/search page (newest first, tombstones
// excluded). HasMore is honest: a truncated page is never reported as
// complete, and NextSince carries the current principal revision for callers
// that only need the UI's "more" hint.
func (s *Store) ListContacts(ctx context.Context, principalID, query string, limit int) (ContactPage, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	revision, err := s.PrincipalRevision(ctx, principalID)
	if err != nil {
		return ContactPage{}, err
	}
	args := []any{principalID}
	clauses := []string{"c.principal_id = ?", "c.deleted_at IS NULL"}
	query = strings.TrimSpace(query)
	if query != "" {
		pattern := "%" + strings.ToLower(query) + "%"
		digits := normalizeDigits(query)
		clauses = append(clauses, `(
			LOWER(c.display_name) LIKE ? OR LOWER(c.organization) LIKE ? OR
			LOWER(c.search_key) LIKE ? OR LOWER(c.nickname) LIKE ? OR
			EXISTS (SELECT 1 FROM contact_fields f WHERE f.contact_id = c.id AND
				(f.kind = 'phone' AND (? != '' AND f.normalized LIKE '%' || ? || '%') OR
				 f.kind = 'email' AND LOWER(f.value) LIKE ?)))`)
		args = append(args, pattern, pattern, pattern, pattern, digits, digits, pattern)
	}
	args = append(args, limit+1)
	rows, err := s.DB.QueryContext(ctx, `
		SELECT c.id, c.principal_id, c.display_name, c.given_name, c.family_name, c.organization,
			c.nickname, c.note, c.search_key, c.source, c.client_ref, c.revision, c.created_at, c.updated_at
		FROM contacts c WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY c.updated_at DESC, c.id DESC LIMIT ?`, args...)
	if err != nil {
		return ContactPage{}, fmt.Errorf("list contacts: %w", err)
	}
	contacts, err := scanContactRows(rows)
	if err != nil {
		return ContactPage{}, err
	}
	page := ContactPage{Revision: revision, NextSince: revision}
	if len(contacts) > limit {
		page.HasMore = true
		contacts = contacts[:limit]
	}
	page.Contacts = contacts
	if err := s.attachContactFields(ctx, page.Contacts); err != nil {
		return ContactPage{}, err
	}
	return page, nil
}

// ContactChanges is the incremental pull feed: every change with
// revision > since, including deletion tombstones, ordered by revision so
// NextSince is a safe cursor. A caller must keep paging while HasMore is
// true; only then may it advance its stored revision to page.Revision.
func (s *Store) ContactChanges(ctx context.Context, principalID string, since int64, limit int) (ContactPage, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	revision, err := s.PrincipalRevision(ctx, principalID)
	if err != nil {
		return ContactPage{}, err
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT c.id, c.principal_id, c.display_name, c.given_name, c.family_name, c.organization,
			c.nickname, c.note, c.search_key, c.source, c.client_ref, c.revision, c.created_at, c.updated_at,
			CASE WHEN c.deleted_at IS NULL THEN 0 ELSE 1 END AS deleted
		FROM contacts c WHERE c.principal_id = ? AND c.revision > ?
		ORDER BY c.revision ASC, c.id ASC LIMIT ?`, principalID, since, limit+1)
	if err != nil {
		return ContactPage{}, fmt.Errorf("list contact changes: %w", err)
	}
	contacts, err := scanContactRows(rows)
	if err != nil {
		return ContactPage{}, err
	}
	page := ContactPage{Revision: revision, NextSince: since}
	if len(contacts) > limit {
		page.HasMore = true
		contacts = contacts[:limit]
	}
	if len(contacts) > 0 {
		page.NextSince = contacts[len(contacts)-1].Revision
		if !page.HasMore {
			page.NextSince = revision
		}
	} else {
		page.NextSince = revision
	}
	// Tombstones have no field rows worth returning; skip the field load.
	page.Contacts = contacts
	active := make([]Contact, 0, len(contacts))
	indices := make([]int, 0, len(contacts))
	for i := range contacts {
		if !contacts[i].Deleted {
			active = append(active, contacts[i])
			indices = append(indices, i)
		}
	}
	if len(active) > 0 {
		if err := s.attachContactFields(ctx, active); err != nil {
			return ContactPage{}, err
		}
		for i, contact := range active {
			page.Contacts[indices[i]] = contact
		}
	}
	return page, nil
}

func scanContactRows(rows *sql.Rows) ([]Contact, error) {
	names, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	hasDeleted := false
	for _, name := range names {
		if name == "deleted" {
			hasDeleted = true
		}
	}
	defer rows.Close()
	var contacts []Contact
	for rows.Next() {
		var contact Contact
		var createdAt, updatedAt int64
		var deleted int
		columns := []any{&contact.ID, &contact.PrincipalID, &contact.DisplayName, &contact.GivenName,
			&contact.FamilyName, &contact.Organization, &contact.Nickname, &contact.Note,
			&contact.SearchKey, &contact.Source, &contact.ClientRef, &contact.Revision,
			&createdAt, &updatedAt}
		if hasDeleted {
			columns = append(columns, &deleted)
		}
		if err := rows.Scan(columns...); err != nil {
			return nil, err
		}
		contact.CreatedAt = fromMillis(createdAt)
		contact.UpdatedAt = fromMillis(updatedAt)
		contact.Deleted = deleted == 1
		contacts = append(contacts, contact)
	}
	return contacts, rows.Err()
}

// GetContact loads one principal-owned contact.
func (s *Store) GetContact(ctx context.Context, principalID, contactID string) (Contact, error) {
	var contact Contact
	var createdAt, updatedAt int64
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, principal_id, display_name, given_name, family_name, organization, nickname,
			note, search_key, source, client_ref, revision, created_at, updated_at
		FROM contacts WHERE id = ? AND principal_id = ? AND deleted_at IS NULL`,
		contactID, principalID).Scan(&contact.ID, &contact.PrincipalID, &contact.DisplayName,
		&contact.GivenName, &contact.FamilyName, &contact.Organization, &contact.Nickname,
		&contact.Note, &contact.SearchKey, &contact.Source, &contact.ClientRef, &contact.Revision,
		&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Contact{}, ErrNotFound
	}
	if err != nil {
		return Contact{}, fmt.Errorf("read contact: %w", err)
	}
	contact.CreatedAt = fromMillis(createdAt)
	contact.UpdatedAt = fromMillis(updatedAt)
	contacts := []Contact{contact}
	if err := s.attachContactFields(ctx, contacts); err != nil {
		return Contact{}, err
	}
	return contacts[0], nil
}

func (s *Store) attachContactFields(ctx context.Context, contacts []Contact) error {
	index := make(map[string]int, len(contacts))
	placeholders := make([]string, 0, len(contacts))
	args := make([]any, 0, len(contacts))
	for i := range contacts {
		index[contacts[i].ID] = i
		placeholders = append(placeholders, "?")
		args = append(args, contacts[i].ID)
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT contact_id, kind, ordinal, label, value, normalized FROM contact_fields
		WHERE contact_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY contact_id, kind, ordinal`, args...)
	if err != nil {
		return fmt.Errorf("list contact fields: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var contactID string
		var field ContactField
		if err := rows.Scan(&contactID, &field.Kind, &field.Ordinal, &field.Label, &field.Value, &field.Normalized); err != nil {
			return err
		}
		i, ok := index[contactID]
		if !ok {
			continue
		}
		switch field.Kind {
		case "phone":
			contacts[i].Phones = append(contacts[i].Phones, field)
		case "email":
			contacts[i].Emails = append(contacts[i].Emails, field)
		default:
			contacts[i].URLs = append(contacts[i].URLs, field)
		}
	}
	return rows.Err()
}

// normalizeDigits is the store-side canonical phone match form: keep digits
// only, so +86 138... and 138... compare predictably in queries. The
// authoritative merge normalization lives in the contacts package.
func normalizeDigits(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}
