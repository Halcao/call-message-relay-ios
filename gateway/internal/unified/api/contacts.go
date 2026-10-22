package api

import (
	"net/http"
	"strconv"
	"strings"

	"callrelay/gateway/internal/unified/contacts"
	"callrelay/gateway/internal/unified/store"
)

// ---------- contacts ----------

type contactFieldView struct {
	Label string `json:"label,omitempty"`
	Value string `json:"value"`
}

type contactView struct {
	ID           string `json:"id"`
	DisplayName  string `json:"displayName"`
	GivenName    string `json:"givenName,omitempty"`
	FamilyName   string `json:"familyName,omitempty"`
	Organization string `json:"organization,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	Note         string `json:"note,omitempty"`
	SearchKey    string `json:"searchKey,omitempty"`
	Source       string `json:"source"`
	Revision     int64  `json:"revision"`
	UpdatedAt    int64  `json:"updatedAt"`
	// Deleted marks a revisioned tombstone in the incremental change feed.
	// Display responses never set it.
	Deleted bool               `json:"deleted,omitempty"`
	Phones  []contactFieldView `json:"phones,omitempty"`
	Emails  []contactFieldView `json:"emails,omitempty"`
	URLs    []contactFieldView `json:"urls,omitempty"`
}

type contactListResponse struct {
	Contacts []contactView `json:"contacts"`
	// Revision is the current principal revision. HasMore means this page was
	// truncated by limit; a sync client must page via /contacts/sync using
	// nextSince and only advance its stored revision once HasMore is false.
	Revision  int64 `json:"revision"`
	NextSince int64 `json:"nextSince"`
	HasMore   bool  `json:"hasMore"`
}

func contactFields(fields []store.ContactField) []contactFieldView {
	views := make([]contactFieldView, 0, len(fields))
	for _, field := range fields {
		views = append(views, contactFieldView{Label: field.Label, Value: field.Value})
	}
	return views
}

func contactFromStore(contact store.Contact) contactView {
	return contactView{
		ID: contact.ID, DisplayName: contact.DisplayName, GivenName: contact.GivenName,
		FamilyName: contact.FamilyName, Organization: contact.Organization,
		Nickname: contact.Nickname, Note: contact.Note, SearchKey: contact.SearchKey,
		Source: contact.Source, Revision: contact.Revision, UpdatedAt: contact.UpdatedAt.UnixMilli(),
		Deleted: contact.Deleted,
		Phones:  contactFields(contact.Phones), Emails: contactFields(contact.Emails), URLs: contactFields(contact.URLs),
	}
}

// principal resolves the authenticated pairing key to its stable contact
// principal. Every contacts handler goes through this: the owner is never
// taken from the request body or query.
func (s *Server) principal(w http.ResponseWriter, r *http.Request, access store.Access) (string, bool) {
	if s.Contacts == nil {
		writeError(w, http.StatusServiceUnavailable, "CB-V2-503", "通讯录服务未启用")
		return "", false
	}
	principalID, err := s.Contacts.Store.PrincipalForKey(r.Context(), access.Key.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CB-V2-500", "服务内部错误")
		return "", false
	}
	return principalID, true
}

func contactLimit(r *http.Request) int {
	limit := 500
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 2000 {
			limit = parsed
		}
	}
	return limit
}

func (s *Server) listContacts(w http.ResponseWriter, r *http.Request, access store.Access) {
	principalID, ok := s.principal(w, r, access)
	if !ok {
		return
	}
	page, err := s.Contacts.List(r.Context(), principalID,
		strings.TrimSpace(r.URL.Query().Get("query")), contactLimit(r))
	if err != nil {
		writeFailure(w, err)
		return
	}
	response := contactListResponse{
		Contacts: make([]contactView, 0, len(page.Contacts)), Revision: page.Revision,
		NextSince: page.NextSince, HasMore: page.HasMore,
	}
	for _, record := range page.Contacts {
		response.Contacts = append(response.Contacts, contactFromStore(record))
	}
	writeJSON(w, http.StatusOK, response)
}

// pullContacts is the incremental sync feed. It includes deletion tombstones
// and an honest nextSince cursor: a truncated page is never presented as the
// full change set.
func (s *Server) pullContacts(w http.ResponseWriter, r *http.Request, access store.Access) {
	principalID, ok := s.principal(w, r, access)
	if !ok {
		return
	}
	since := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed >= 0 {
			since = parsed
		}
	}
	page, err := s.Contacts.Pull(r.Context(), principalID, since, contactLimit(r))
	if err != nil {
		writeFailure(w, err)
		return
	}
	response := contactListResponse{
		Contacts: make([]contactView, 0, len(page.Contacts)), Revision: page.Revision,
		NextSince: page.NextSince, HasMore: page.HasMore,
	}
	for _, record := range page.Contacts {
		response.Contacts = append(response.Contacts, contactFromStore(record))
	}
	writeJSON(w, http.StatusOK, response)
}

type importRequest struct {
	VCard   string `json:"vcard"`
	Preview bool   `json:"preview"`
}

func (s *Server) importContacts(w http.ResponseWriter, r *http.Request, access store.Access) {
	principalID, ok := s.principal(w, r, access)
	if !ok {
		return
	}
	var request importRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.VCard) == "" {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "缺少 vCard 内容")
		return
	}
	result, err := s.Contacts.ImportVCard(r.Context(), principalID, []byte(request.VCard), !request.Preview)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "exceeds limits"):
			writeError(w, http.StatusRequestEntityTooLarge, "CB-CONTACTS-413", "文件过大或联系人数量超过限制")
			return
		case strings.Contains(err.Error(), "contact limit"):
			writeError(w, http.StatusConflict, "CB-CONTACTS-409", "通讯录数量已达上限，无法继续新增")
			return
		}
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type syncRequest struct {
	Contacts []contacts.SyncContact `json:"contacts"`
}

func (s *Server) syncContacts(w http.ResponseWriter, r *http.Request, access store.Access) {
	principalID, ok := s.principal(w, r, access)
	if !ok {
		return
	}
	var request syncRequest
	if !decodeBody(w, r, &request) {
		return
	}
	result, err := s.Contacts.SyncContacts(r.Context(), principalID, request.Contacts)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "exceeds"):
			writeError(w, http.StatusRequestEntityTooLarge, "CB-CONTACTS-413", "单次同步联系人数量超过限制")
			return
		case strings.Contains(err.Error(), "contact limit"):
			writeError(w, http.StatusConflict, "CB-CONTACTS-409", "通讯录数量已达上限，无法继续新增")
			return
		}
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type contactUpdateRequest struct {
	DisplayName  *string          `json:"displayName"`
	GivenName    *string          `json:"givenName"`
	FamilyName   *string          `json:"familyName"`
	Organization *string          `json:"organization"`
	Nickname     *string          `json:"nickname"`
	Note         *string          `json:"note"`
	Phones       []contacts.Field `json:"phones"`
	Emails       []contacts.Field `json:"emails"`
	URLs         []contacts.Field `json:"urls"`
}

func (s *Server) updateContact(w http.ResponseWriter, r *http.Request, access store.Access) {
	principalID, ok := s.principal(w, r, access)
	if !ok {
		return
	}
	var request contactUpdateRequest
	if !decodeBody(w, r, &request) {
		return
	}
	existing, err := s.Contacts.Store.GetContact(r.Context(), principalID, r.PathValue("id"))
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "CB-CONTACTS-404", "联系人不存在")
			return
		}
		writeFailure(w, err)
		return
	}
	set := func(target *string, value *string) {
		if value != nil {
			*target = *value
		}
	}
	set(&existing.DisplayName, request.DisplayName)
	set(&existing.GivenName, request.GivenName)
	set(&existing.FamilyName, request.FamilyName)
	set(&existing.Organization, request.Organization)
	set(&existing.Nickname, request.Nickname)
	set(&existing.Note, request.Note)
	if request.Phones != nil {
		existing.Phones = wireFields(request.Phones, "phone")
	}
	if request.Emails != nil {
		existing.Emails = wireFields(request.Emails, "email")
	}
	if request.URLs != nil {
		existing.URLs = wireFields(request.URLs, "url")
	}
	updated, err := s.Contacts.Update(r.Context(), principalID, existing)
	if err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "CB-CONTACTS-404", "联系人不存在")
			return
		}
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, contactFromStore(updated))
}

func wireFields(fields []contacts.Field, kind string) []store.ContactField {
	out := make([]store.ContactField, 0, len(fields))
	for index, field := range fields {
		value := strings.TrimSpace(field.Value)
		if value == "" {
			continue
		}
		entry := store.ContactField{Kind: kind, Ordinal: index, Label: field.Label, Value: value}
		switch kind {
		case "phone":
			entry.Normalized = contacts.CanonicalPhone(value)
		default:
			entry.Normalized = strings.ToLower(value)
		}
		out = append(out, entry)
	}
	return out
}

func (s *Server) deleteContact(w http.ResponseWriter, r *http.Request, access store.Access) {
	principalID, ok := s.principal(w, r, access)
	if !ok {
		return
	}
	if err := s.Contacts.Delete(r.Context(), principalID, r.PathValue("id")); err != nil {
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "CB-CONTACTS-404", "联系人不存在")
			return
		}
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) exportContacts(w http.ResponseWriter, r *http.Request, access store.Access) {
	principalID, ok := s.principal(w, r, access)
	if !ok {
		return
	}
	document, err := s.Contacts.ExportVCard(r.Context(), principalID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="callrelay-contacts.vcf"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(document))
}

// notificationsStatus reports the designated-backup availability view. A
// device may only see the line name/tail when its key is authorized for that
// line; unauthorized devices get the state without line details.
func (s *Server) notificationsStatus(w http.ResponseWriter, r *http.Request, access store.Access) {
	if s.Notify == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	status := s.Notify.Status(r.Context())
	authorized := false
	for _, entry := range access.Lines {
		if entry.Line.ID == status.LineID {
			authorized = true
			break
		}
	}
	if !authorized {
		status.LineName = ""
		status.LineTail = ""
	}
	writeJSON(w, http.StatusOK, status)
}
