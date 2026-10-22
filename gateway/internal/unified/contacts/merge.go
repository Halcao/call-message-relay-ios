package contacts

import (
	"strings"
	"unicode"

	"callrelay/gateway/internal/unified/store"
)

// Contact is the planner's normalized view of an address-book entry. It maps
// 1:1 onto store.Contact; IDs only exist for entries that already persisted.
type Contact struct {
	ID           string
	DisplayName  string
	GivenName    string
	FamilyName   string
	Organization string
	Nickname     string
	Note         string
	SearchKey    string
	Source       string
	ClientRef    string
	Phones       []Field
	Emails       []Field
	URLs         []Field
}

// FromStore converts a persisted contact into the planner model.
func FromStore(contact store.Contact) Contact {
	convert := func(fields []store.ContactField) []Field {
		out := make([]Field, 0, len(fields))
		for _, field := range fields {
			out = append(out, Field{Label: field.Label, Value: field.Value})
		}
		return out
	}
	return Contact{
		ID: contact.ID, DisplayName: contact.DisplayName, GivenName: contact.GivenName,
		FamilyName: contact.FamilyName, Organization: contact.Organization,
		Nickname: contact.Nickname, Note: contact.Note, SearchKey: contact.SearchKey,
		Source: contact.Source, ClientRef: contact.ClientRef,
		Phones: convert(contact.Phones), Emails: convert(contact.Emails), URLs: convert(contact.URLs),
	}
}

// ToStore converts a planner contact back to the persistence model, filling
// the server-side normalized match forms.
func (c Contact) ToStore(principalID string) store.Contact {
	phones := make([]store.ContactField, 0, len(c.Phones))
	for i, field := range c.Phones {
		phones = append(phones, store.ContactField{Kind: "phone", Ordinal: i, Label: field.Label,
			Value: field.Value, Normalized: CanonicalPhone(field.Value)})
	}
	emails := make([]store.ContactField, 0, len(c.Emails))
	for i, field := range c.Emails {
		emails = append(emails, store.ContactField{Kind: "email", Ordinal: i, Label: field.Label,
			Value: field.Value, Normalized: strings.ToLower(strings.TrimSpace(field.Value))})
	}
	urls := make([]store.ContactField, 0, len(c.URLs))
	for i, field := range c.URLs {
		urls = append(urls, store.ContactField{Kind: "url", Ordinal: i, Label: field.Label,
			Value: field.Value, Normalized: strings.ToLower(strings.TrimSpace(field.Value))})
	}
	return store.Contact{
		ID: c.ID, PrincipalID: principalID, DisplayName: c.DisplayName, GivenName: c.GivenName,
		FamilyName: c.FamilyName, Organization: c.Organization, Nickname: c.Nickname,
		Note: c.Note, SearchKey: c.SearchKey, Source: c.Source, ClientRef: c.ClientRef,
		Phones: phones, Emails: emails, URLs: urls,
	}
}

// Action is what the plan will do with one imported contact.
type Action string

const (
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
	ActionUnchanged Action = "unchanged"
	// ActionSkipped marks an entry that was planned as an update but whose
	// target was deleted before the write. It is never recreated here: that
	// would resurrect an explicit deletion.
	ActionSkipped Action = "skipped"
)

// Entry is one planned import operation.
type Entry struct {
	Action     Action
	ExistingID string
	Contact    Contact
	Changes    []string
	Conflicts  []string
}

// Plan is the complete result of planning an import.
type Plan struct {
	Entries  []Entry
	Skipped  int
	Warnings []string
}

// Summary counts entries by action.
func (p Plan) Summary() map[string]int {
	counts := map[string]int{"create": 0, "update": 0, "unchanged": 0, "skipped": p.Skipped}
	for _, entry := range p.Entries {
		counts[string(entry.Action)]++
	}
	return counts
}

// BuildPlan merges parsed vCards into the principal's existing contacts.
// Matching is conservative: a shared canonical phone or email always merges;
// a name-only match additionally requires the same non-empty organization,
// or completely empty phone/email fields on both sides. A card for a
// different person with the same name is therefore reported as a new
// contact, not silently folded into the existing one.
func BuildPlan(existing []Contact, parsed []ParsedContact, warnings []string) Plan {
	working := make([]Contact, len(existing))
	copy(working, existing)
	plan := Plan{Warnings: append([]string(nil), warnings...)}

	for _, card := range parsed {
		incoming := contactFromParsed(card)
		if !hasIdentity(incoming) {
			plan.Skipped++
			continue
		}
		index := matchContact(working, incoming)
		if index < 0 {
			plan.Entries = append(plan.Entries, Entry{Action: ActionCreate, Contact: incoming,
				Changes: []string{"新增联系人"}})
			working = append(working, incoming)
			continue
		}
		merged, changes, conflicts := mergeContact(working[index], incoming)
		if len(changes) == 0 {
			plan.Entries = append(plan.Entries, Entry{Action: ActionUnchanged, ExistingID: working[index].ID,
				Contact: merged, Conflicts: conflicts})
		} else {
			plan.Entries = append(plan.Entries, Entry{Action: ActionUpdate, ExistingID: working[index].ID,
				Contact: merged, Changes: changes, Conflicts: conflicts})
		}
		if merged.ID != "" {
			working[index] = merged
		}
	}
	return plan
}

func contactFromParsed(card ParsedContact) Contact {
	return Contact{
		DisplayName: card.DisplayName, GivenName: card.GivenName, FamilyName: card.FamilyName,
		Organization: card.Organization, Nickname: card.Nickname, Note: card.Note,
		SearchKey: card.SearchKey, ClientRef: card.ClientRef,
		Source: "vcard", Phones: card.Phones, Emails: card.Emails, URLs: card.URLs,
	}
}

func hasIdentity(contact Contact) bool {
	return contact.DisplayName != "" || contact.GivenName != "" || contact.FamilyName != "" ||
		contact.Organization != "" || contact.Nickname != "" || len(contact.Phones) > 0 || len(contact.Emails) > 0
}

func matchContact(existing []Contact, incoming Contact) int {
	for i := range existing {
		for _, candidate := range existing[i].Phones {
			for _, phone := range incoming.Phones {
				if SamePhone(candidate.Value, phone.Value) {
					return i
				}
			}
		}
		for _, candidate := range existing[i].Emails {
			for _, email := range incoming.Emails {
				if strings.EqualFold(strings.TrimSpace(candidate.Value), strings.TrimSpace(email.Value)) {
					return i
				}
			}
		}
	}
	incomingName := normalizedName(incoming.DisplayName)
	if incomingName == "" {
		incomingName = normalizedName(composeDisplayName(incoming.GivenName, incoming.FamilyName))
	}
	if incomingName == "" {
		return -1
	}
	for i := range existing {
		existingName := normalizedName(existing[i].DisplayName)
		if existingName == "" {
			existingName = normalizedName(composeDisplayName(existing[i].GivenName, existing[i].FamilyName))
		}
		if existingName != incomingName {
			continue
		}
		existingOrg := normalizedName(existing[i].Organization)
		incomingOrg := normalizedName(incoming.Organization)
		if existingOrg != "" && incomingOrg != "" {
			if existingOrg == incomingOrg {
				return i
			}
			continue
		}
		// Name-only match is allowed only when both sides carry no number or
		// email at all: otherwise a shared common name could erase a contact.
		if len(existing[i].Phones) == 0 && len(existing[i].Emails) == 0 &&
			len(incoming.Phones) == 0 && len(incoming.Emails) == 0 {
			return i
		}
	}
	return -1
}

func mergeContact(existing, incoming Contact) (Contact, []string, []string) {
	merged := existing
	var changes []string
	var conflicts []string

	if nameScore(incoming) > nameScore(existing) {
		if incoming.DisplayName != "" && incoming.DisplayName != existing.DisplayName {
			merged.DisplayName = incoming.DisplayName
			changes = append(changes, "更新姓名")
		}
		if incoming.GivenName != "" {
			merged.GivenName = incoming.GivenName
		}
		if incoming.FamilyName != "" {
			merged.FamilyName = incoming.FamilyName
		}
	} else {
		if merged.DisplayName == "" && incoming.DisplayName != "" {
			merged.DisplayName = incoming.DisplayName
			changes = append(changes, "补充姓名")
		}
		if merged.GivenName == "" && incoming.GivenName != "" {
			merged.GivenName = incoming.GivenName
			changes = append(changes, "补充名")
		}
		if merged.FamilyName == "" && incoming.FamilyName != "" {
			merged.FamilyName = incoming.FamilyName
			changes = append(changes, "补充姓")
		}
	}
	if merged.DisplayName == "" {
		merged.DisplayName = composeDisplayName(merged.GivenName, merged.FamilyName)
	}

	fill := func(current *string, value, label string) {
		if *current == "" && value != "" {
			*current = value
			changes = append(changes, "补充"+label)
		} else if *current != "" && value != "" && *current != value {
			conflicts = append(conflicts, label+"不一致，保留原值")
		}
	}
	fill(&merged.Organization, incoming.Organization, "单位")
	fill(&merged.Nickname, incoming.Nickname, "昵称")
	fill(&merged.Note, incoming.Note, "备注")
	if merged.SearchKey == "" && incoming.SearchKey != "" {
		merged.SearchKey = incoming.SearchKey
	}
	if merged.ClientRef == "" && incoming.ClientRef != "" {
		merged.ClientRef = incoming.ClientRef
	}

	phones, added := unionFields(merged.Phones, incoming.Phones, func(a, b string) bool { return SamePhone(a, b) })
	if added > 0 {
		changes = append(changes, plural("新增号码", added))
	}
	merged.Phones = phones
	emails, added := unionFields(merged.Emails, incoming.Emails, func(a, b string) bool {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	})
	if added > 0 {
		changes = append(changes, plural("新增邮箱", added))
	}
	merged.Emails = emails
	urls, added := unionFields(merged.URLs, incoming.URLs, func(a, b string) bool {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	})
	if added > 0 {
		changes = append(changes, plural("新增网址", added))
	}
	merged.URLs = urls

	if merged.Source == "" {
		merged.Source = incoming.Source
	}
	return merged, changes, conflicts
}

func plural(label string, count int) string {
	if count <= 1 {
		return label
	}
	return label + "×" + itoa(count)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// unionFields appends incoming values that do not match any existing value
// (by same), keeping the existing spelling and label. The first non-empty
// incoming label fills an empty existing label.
func unionFields(existing, incoming []Field, same func(a, b string) bool) ([]Field, int) {
	out := append([]Field(nil), existing...)
	added := 0
	for _, candidate := range incoming {
		found := -1
		for i := range out {
			if same(out[i].Value, candidate.Value) {
				found = i
				break
			}
		}
		if found >= 0 {
			if out[found].Label == "" && candidate.Label != "" {
				out[found].Label = candidate.Label
			}
			continue
		}
		out = append(out, candidate)
		added++
	}
	return out, added
}

func nameScore(contact Contact) int {
	score := 0
	if contact.GivenName != "" {
		score += 2
	}
	if contact.FamilyName != "" {
		score += 2
	}
	if contact.DisplayName != "" {
		score++
	}
	return score
}

func normalizedName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// composeDisplayName joins given/family using CJK ordering when either part
// contains Han characters (family first, no space) and Western ordering
// otherwise.
func composeDisplayName(given, family string) string {
	given = strings.TrimSpace(given)
	family = strings.TrimSpace(family)
	switch {
	case given == "":
		return family
	case family == "":
		return given
	case hasCJK(given) || hasCJK(family):
		return family + given
	default:
		return given + " " + family
	}
}

func hasCJK(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
