package contacts

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"callrelay/gateway/internal/unified/store"
)

// Limits bound one principal's address book and one sync batch.
const (
	MaxContactsPerPrincipal = 5000
	MaxSyncBatch            = 2000
	MaxNameLength           = 200
	MaxFieldLength          = 500
)

// SyncContact is one mobile-uploaded contact. ClientRef is the device's
// stable identifier; the server never treats a missing ref as a deletion.
type SyncContact struct {
	ClientRef    string  `json:"clientRef,omitempty"`
	DisplayName  string  `json:"displayName,omitempty"`
	GivenName    string  `json:"givenName,omitempty"`
	FamilyName   string  `json:"familyName,omitempty"`
	Organization string  `json:"organization,omitempty"`
	Nickname     string  `json:"nickname,omitempty"`
	Note         string  `json:"note,omitempty"`
	SearchKey    string  `json:"searchKey,omitempty"`
	// Restore explicitly re-creates a contact whose deletion tombstone this
	// principal already published. Without it, an upsert from a stale device
	// cannot resurrect a contact deleted on another surface.
	Restore bool    `json:"restore,omitempty"`
	Phones  []Field `json:"phones,omitempty"`
	Emails  []Field `json:"emails,omitempty"`
	URLs    []Field `json:"urls,omitempty"`
}

// SyncResult reports one applied batch plus the principal's new revision.
type SyncResult struct {
	Revision int64  `json:"revision"`
	Created  int    `json:"created"`
	Updated  int    `json:"updated"`
	Unchanged int   `json:"unchanged"`
	Skipped  int    `json:"skipped"`
	Total    int    `json:"total"`
}

// ImportEntry is one preview row. It never contains more than the contact's
// own fields (already scoped to the principal).
type ImportEntry struct {
	Action    string   `json:"action"`
	ContactID string   `json:"contactId,omitempty"`
	Name      string   `json:"name"`
	Phones    []string `json:"phones,omitempty"`
	Changes   []string `json:"changes,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// ImportResult is the outcome of a vCard import (preview or committed).
type ImportResult struct {
	Preview  bool           `json:"preview"`
	Created  int            `json:"created"`
	Updated  int            `json:"updated"`
	Unchanged int           `json:"unchanged"`
	Skipped  int            `json:"skipped"`
	Total    int            `json:"total"`
	Revision int64          `json:"revision"`
	Entries  []ImportEntry  `json:"entries"`
	Warnings []string       `json:"warnings,omitempty"`
}

// Service applies import/sync plans against the store. Every method takes the
// principal id resolved from the authenticated key; there is no API path that
// lets a client supply its own owner id.
type Service struct {
	Store *store.Store

	// keyLocks serializes read-plan-write mutations per principal. Without
	// it two devices on the same pairing key can both read "no match" and
	// both create the same contact (or one can write between another's plan
	// and apply). The store has a single SQLite connection, so an in-process
	// lock is sufficient and never deadlocks across principals.
	keyLocks sync.Map // principalID -> *sync.Mutex

	// beforeApply is an unexported test seam: it runs inside the principal
	// lock after the merge plan is built and before writes, so tests can
	// simulate an out-of-band deletion. Production leaves it nil.
	beforeApply func()
}

func NewService(st *store.Store) *Service { return &Service{Store: st} }

func (s *Service) lockFor(principalID string) *sync.Mutex {
	value, _ := s.keyLocks.LoadOrStore(principalID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

// withPrincipalLock runs fn while holding the principal's mutation lock.
func (s *Service) withPrincipalLock(principalID string, fn func() (ImportResult, error)) (ImportResult, error) {
	lock := s.lockFor(principalID)
	lock.Lock()
	defer lock.Unlock()
	return fn()
}

// ImportVCard parses data and either previews or commits the merge plan.
// Repeated imports are idempotent: matching contacts are merged, never
// duplicated, and unchanged entries are not rewritten.
func (s *Service) ImportVCard(ctx context.Context, principalID string, data []byte, commit bool) (ImportResult, error) {
	parsed, warnings, err := ParseVCards(data)
	if err != nil {
		return ImportResult{}, err
	}
	return s.withPrincipalLock(principalID, func() (ImportResult, error) {
		return s.planAndApply(ctx, principalID, parsed, warnings, "vcard", commit)
	})
}

func (s *Service) planAndApply(ctx context.Context, principalID string, parsed []ParsedContact, warnings []string, source string, commit bool) (ImportResult, error) {
	existing, revision, err := s.Store.AllContacts(ctx, principalID)
	if err != nil {
		return ImportResult{}, err
	}
	model := make([]Contact, 0, len(existing))
	for _, contact := range existing {
		model = append(model, FromStore(contact))
	}
	plan := BuildPlan(model, parsed, warnings)
	creates := 0
	for i := range plan.Entries {
		plan.Entries[i].Contact.Source = source
		if plan.Entries[i].Action == ActionCreate {
			creates++
		}
	}
	if len(existing)+creates > MaxContactsPerPrincipal {
		return ImportResult{}, fmt.Errorf("contact limit reached (%d)", MaxContactsPerPrincipal)
	}

	result := ImportResult{
		Preview: !commit, Revision: revision, Skipped: plan.Skipped, Warnings: plan.Warnings,
	}
	for _, entry := range plan.Entries {
		preview := ImportEntry{
			Action: string(entry.Action), ContactID: entry.ExistingID, Name: contactLabel(entry.Contact),
			Changes: entry.Changes, Conflicts: entry.Conflicts,
		}
		for _, phone := range entry.Contact.Phones {
			preview.Phones = append(preview.Phones, phone.Value)
		}
		result.Entries = append(result.Entries, preview)
		switch entry.Action {
		case ActionCreate:
			result.Created++
		case ActionUpdate:
			result.Updated++
		default:
			result.Unchanged++
		}
	}
	result.Total = len(plan.Entries)
	if !commit {
		return result, nil
	}
	if s.beforeApply != nil {
		s.beforeApply()
	}

	appliedCreate, appliedUpdate, appliedUnchanged, appliedSkipped := 0, 0, 0, 0
	for index, entry := range plan.Entries {
		record := entry.Contact.ToStore(principalID)
		switch entry.Action {
		case ActionCreate:
			created, err := s.Store.CreateContact(ctx, record)
			if err != nil {
				return ImportResult{}, err
			}
			result.Entries[index].ContactID = created.ID
			appliedCreate++
		case ActionUpdate:
			updated, err := s.Store.UpdateContact(ctx, record)
			if err != nil {
				if err == store.ErrNotFound {
					// The contact was deleted between plan and apply. Recreating
					// it here would resurrect an explicit deletion, so the entry
					// is skipped with an honest count; the owner can re-add it
					// deliberately.
					result.Entries[index].Action = string(ActionSkipped)
					appliedSkipped++
					continue
				}
				return ImportResult{}, err
			}
			result.Entries[index].ContactID = updated.ID
			appliedUpdate++
		default:
			appliedUnchanged++
		}
	}
	result.Created, result.Updated, result.Unchanged = appliedCreate, appliedUpdate, appliedUnchanged
	result.Skipped += appliedSkipped
	if revision, err := s.Store.PrincipalRevision(ctx, principalID); err == nil {
		result.Revision = revision
	}
	return result, nil
}

// SyncContacts merges a mobile batch. The operation is upsert-only: contacts
// missing from the batch are kept, so a phone can never delete gateway data
// by omission. Contacts whose tombstone already exists are skipped unless the
// client explicitly sets restore, so a stale device cannot resurrect a
// contact the owner deleted elsewhere.
func (s *Service) SyncContacts(ctx context.Context, principalID string, incoming []SyncContact) (SyncResult, error) {
	if len(incoming) > MaxSyncBatch {
		return SyncResult{}, fmt.Errorf("sync batch exceeds %d contacts", MaxSyncBatch)
	}
	lock := s.lockFor(principalID)
	lock.Lock()
	defer lock.Unlock()
	tombstones, err := s.Store.TombstonedClientRefs(ctx, principalID)
	if err != nil {
		return SyncResult{}, err
	}
	parsed := make([]ParsedContact, 0, len(incoming))
	skipped := 0
	for _, contact := range incoming {
		if contact.ClientRef != "" && tombstones[contact.ClientRef] && !contact.Restore {
			skipped++
			continue
		}
		parsed = append(parsed, ParsedContact{
			DisplayName:  clip(contact.DisplayName, MaxNameLength),
			GivenName:    clip(contact.GivenName, MaxNameLength),
			FamilyName:   clip(contact.FamilyName, MaxNameLength),
			Organization: clip(contact.Organization, MaxNameLength),
			Nickname:     clip(contact.Nickname, MaxNameLength),
			Note:         clip(contact.Note, MaxFieldLength),
			SearchKey:    clip(contact.SearchKey, MaxFieldLength),
			ClientRef:    clip(contact.ClientRef, 128),
			Phones:       fieldsFromWire(contact.Phones),
			Emails:       fieldsFromWire(contact.Emails),
			URLs:         fieldsFromWire(contact.URLs),
		})
	}
	result, err := s.planAndApply(ctx, principalID, parsed, nil, "sync", true)
	if err != nil {
		return SyncResult{}, err
	}
	return SyncResult{
		Revision: result.Revision, Created: result.Created, Updated: result.Updated,
		Unchanged: result.Unchanged, Skipped: result.Skipped + skipped, Total: result.Total + skipped,
	}, nil
}

// List returns one display/search page and its honest cursor.
func (s *Service) List(ctx context.Context, principalID, query string, limit int) (store.ContactPage, error) {
	return s.Store.ListContacts(ctx, principalID, query, limit)
}

// Pull returns one incremental change page (tombstones included). Callers
// must keep paging while HasMore is true before advancing their revision.
func (s *Service) Pull(ctx context.Context, principalID string, since int64, limit int) (store.ContactPage, error) {
	return s.Store.ContactChanges(ctx, principalID, since, limit)
}

// Update rewrites one principal-owned contact from the web editor.
func (s *Service) Update(ctx context.Context, principalID string, contact store.Contact) (store.Contact, error) {
	contact.PrincipalID = principalID
	for i := range contact.Phones {
		contact.Phones[i].Value = clip(contact.Phones[i].Value, MaxFieldLength)
		contact.Phones[i].Normalized = CanonicalPhone(contact.Phones[i].Value)
	}
	for i := range contact.Emails {
		contact.Emails[i].Value = clip(contact.Emails[i].Value, MaxFieldLength)
		contact.Emails[i].Normalized = strings.ToLower(strings.TrimSpace(contact.Emails[i].Value))
	}
	contact.DisplayName = clip(contact.DisplayName, MaxNameLength)
	contact.Organization = clip(contact.Organization, MaxNameLength)
	contact.Source = "web"
	lock := s.lockFor(principalID)
	lock.Lock()
	defer lock.Unlock()
	return s.Store.UpdateContact(ctx, contact)
}

// Delete removes one principal-owned contact.
func (s *Service) Delete(ctx context.Context, principalID, contactID string) error {
	lock := s.lockFor(principalID)
	lock.Lock()
	defer lock.Unlock()
	return s.Store.DeleteContact(ctx, principalID, contactID)
}

// ExportVCard renders only the principal's visible contacts as vCard 3.0.
// AllContacts returns the complete owner set (bounded by the per-principal
// limit), so an export is never silently truncated by a display page.
func (s *Service) ExportVCard(ctx context.Context, principalID string) (string, error) {
	contacts, _, err := s.Store.AllContacts(ctx, principalID)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, contact := range contacts {
		builder.WriteString("BEGIN:VCARD\r\nVERSION:3.0\r\n")
		name := contactLabel(FromStore(contact))
		builder.WriteString("FN:" + escapeText(name) + "\r\n")
		builder.WriteString("N:" + escapeText(contact.FamilyName) + ";" + escapeText(contact.GivenName) + ";;;\r\n")
		if contact.Organization != "" {
			builder.WriteString("ORG:" + escapeText(contact.Organization) + "\r\n")
		}
		if contact.Nickname != "" {
			builder.WriteString("NICKNAME:" + escapeText(contact.Nickname) + "\r\n")
		}
		for _, phone := range contact.Phones {
			label := strings.ToUpper(phone.Label)
			if label == "" {
				label = "CELL"
			}
			builder.WriteString("TEL;TYPE=" + label + ":" + escapeText(phone.Value) + "\r\n")
		}
		for _, email := range contact.Emails {
			builder.WriteString("EMAIL:" + escapeText(email.Value) + "\r\n")
		}
		for _, url := range contact.URLs {
			builder.WriteString("URL:" + escapeText(url.Value) + "\r\n")
		}
		if contact.Note != "" {
			builder.WriteString("NOTE:" + escapeText(contact.Note) + "\r\n")
		}
		builder.WriteString("END:VCARD\r\n")
	}
	return builder.String(), nil
}

func fieldsFromWire(fields []Field) []Field {
	out := make([]Field, 0, len(fields))
	for _, field := range fields {
		value := clip(strings.TrimSpace(field.Value), MaxFieldLength)
		if value == "" {
			continue
		}
		out = append(out, Field{Label: clip(field.Label, 64), Value: value})
	}
	return out
}

func contactLabel(contact Contact) string {
	if name := preferredDisplayName(contact.DisplayName, contact.GivenName, contact.FamilyName); name != "" {
		return name
	}
	if composed := composeDisplayName(contact.GivenName, contact.FamilyName); composed != "" {
		return composed
	}
	if contact.Organization != "" {
		return contact.Organization
	}
	if len(contact.Phones) > 0 {
		return contact.Phones[0].Value
	}
	return "未命名联系人"
}

func clip(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func escapeText(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", ";", "\\;", ",", "\\,", "\n", "\\n")
	return replacer.Replace(value)
}
