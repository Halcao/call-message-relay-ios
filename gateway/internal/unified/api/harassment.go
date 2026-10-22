package api

import (
	"errors"
	"net/http"
	"strings"

	"callrelay/gateway/internal/unified/harass"
	"callrelay/gateway/internal/unified/store"
)

// The harassment API is strictly key-scoped: every handler derives the
// enrollment key from the authenticated access, never from the request body
// or URL. Two keys sharing one physical line cannot read or modify each
// other's settings, lists, imports or history (a foreign id is 404, not
// 403-with-existence). Same-key devices share one policy; writes carry the
// version they were based on so concurrent edits from two devices of the
// same key conflict visibly instead of silently overwriting.

type harassSettingsView struct {
	CallBlocking bool `json:"callBlocking"`
	SMSBlocking  bool `json:"smsBlocking"`
	// ContactWhitelist treats this key's gateway-synced contacts as trusted
	// AFTER explicit owner blocks and BEFORE keyword/regex rules. The
	// contacts themselves are never uploaded implicitly.
	ContactWhitelist bool  `json:"contactWhitelist"`
	Version          int64 `json:"version"`
	UpdatedAt        int64 `json:"updatedAt,omitempty"`
}

func harassSettingsFrom(settings store.HarassmentSettings) harassSettingsView {
	return harassSettingsView{
		CallBlocking: settings.CallBlocking, SMSBlocking: settings.SMSBlocking,
		ContactWhitelist: settings.ContactWhitelist,
		Version:          settings.Version, UpdatedAt: settings.UpdatedAt.UnixMilli(),
	}
}

func (s *Server) harassSettings(w http.ResponseWriter, r *http.Request, access store.Access) {
	settings, err := s.Store.HarassmentSettings(r.Context(), access.Key.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": harassSettingsFrom(settings),
		// Unknown callers default to allow; no external classification.
		"unknownAction": "allow",
	})
}

func (s *Server) updateHarassSettings(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		CallBlocking     bool   `json:"callBlocking"`
		SMSBlocking      bool   `json:"smsBlocking"`
		ContactWhitelist *bool  `json:"contactWhitelist"`
		Version          *int64 `json:"version"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	update := store.HarassmentSettingsUpdate{
		CallBlocking: request.CallBlocking, SMSBlocking: request.SMSBlocking,
	}
	if request.ContactWhitelist != nil {
		update.ContactWhitelist = *request.ContactWhitelist
		update.HasContactWhitelist = true
	}
	settings, err := s.Store.UpdateHarassmentSettings(r.Context(), access.Key.ID, update, request.Version)
	if err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			writeError(w, http.StatusConflict, "CB-V2-409V", "设置已在其他设备修改，请刷新后重试")
			return
		}
		writeFailure(w, err)
		return
	}
	_ = s.Store.AppendAudit(r.Context(), access.Device.ID, "harassment.settings", access.Key.ID, "updated")
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": harassSettingsFrom(settings), "unknownAction": "allow",
	})
}

type harassEntryView struct {
	ID        string `json:"id"`
	Number    string `json:"number"`
	MatchKind string `json:"matchKind"`
	Mode      string `json:"mode"`
	Action    string `json:"action"`
	Source    string `json:"source"`
	ListID    string `json:"listId,omitempty"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

func harassEntryFrom(entry store.HarassmentEntry) harassEntryView {
	return harassEntryView{
		ID: entry.ID, Number: entry.Number, MatchKind: entry.MatchKind, Mode: entry.Mode,
		Action: entry.Action, Source: entry.Source, ListID: entry.ListID, Enabled: entry.Enabled,
		CreatedAt: entry.CreatedAt.UnixMilli(),
	}
}

func (s *Server) harassEntries(w http.ResponseWriter, r *http.Request, access store.Access) {
	entries, err := s.Store.ListHarassmentEntries(r.Context(), access.Key.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	views := make([]harassEntryView, 0, len(entries))
	for _, entry := range entries {
		views = append(views, harassEntryFrom(entry))
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": views})
}

func (s *Server) addHarassEntry(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		Number    string `json:"number"`
		MatchKind string `json:"matchKind"`
		Mode      string `json:"mode"`
		Action    string `json:"action"`
		// ReplaceOpposite is the contextual-action flag: it removes the
		// canonically-equivalent opposite-mode exact entry for THIS key in
		// the same transaction so Trust->Block and Block->Trust are real.
		// Bulk imports leave it false and keep the configured precedence.
		ReplaceOpposite bool `json:"replaceOpposite"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	number := strings.TrimSpace(request.Number)
	if number == "" || len(number) > 200 {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "号码或规则内容不能为空")
		return
	}
	if request.Mode != store.HarassModeTrust && request.Mode != store.HarassModeBlock {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "mode 必须是 block 或 trust")
		return
	}
	if request.Action != "" && request.Action != store.HarassActionReject && request.Action != store.HarassActionLabel {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "action 必须是 reject 或 label")
		return
	}
	result, err := s.Manager.AddHarassmentEntry(r.Context(), access.Key.ID, store.HarassmentEntry{
		Number: number, MatchKind: request.MatchKind,
		Mode: request.Mode, Action: request.Action,
	}, request.ReplaceOpposite)
	if err != nil {
		writeFailure(w, err)
		return
	}
	_ = s.Store.AppendAudit(r.Context(), access.Device.ID, "harassment.entry.add", result.Entry.ID, result.Entry.Mode)
	response := map[string]any{
		"entry": harassEntryFrom(result.Entry), "replaced": result.Replaced, "effective": result.Effective,
	}
	if result.Note != "" {
		response["note"] = result.Note
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) deleteHarassEntry(w http.ResponseWriter, r *http.Request, access store.Access) {
	id := r.PathValue("id")
	if err := s.Store.DeleteHarassmentEntry(r.Context(), access.Key.ID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "CB-V2-404", "规则不存在")
			return
		}
		writeFailure(w, err)
		return
	}
	_ = s.Store.AppendAudit(r.Context(), access.Device.ID, "harassment.entry.delete", id, "deleted")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

type harassListView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	DefaultAction string `json:"defaultAction"`
	Source        string `json:"source"`
	EntryCount    int    `json:"entryCount"`
	ImportedAt    int64  `json:"importedAt,omitempty"`
}

func (s *Server) harassLists(w http.ResponseWriter, r *http.Request, access store.Access) {
	lists, err := s.Store.ListHarassmentLists(r.Context(), access.Key.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	views := make([]harassListView, 0, len(lists))
	for _, list := range lists {
		views = append(views, harassListView{
			ID: list.ID, Name: list.Name, DefaultAction: list.DefaultAction, Source: list.Source,
			EntryCount: list.EntryCount, ImportedAt: list.ImportedAt.UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"lists": views})
}

// importHarassList is the ONE explicit, user-triggered import path. It never
// runs automatically: the client must send the numbers the user chose, and
// the default action is label (mark only) unless the user deliberately
// selected reject. There is no auto-upload or overwrite of gateway rules.
func (s *Server) importHarassList(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		Name          string   `json:"name"`
		DefaultAction string   `json:"defaultAction"`
		Numbers       []string `json:"numbers"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	if len(request.Numbers) == 0 {
		writeError(w, http.StatusBadRequest, "CB-V2-400", "导入名单为空")
		return
	}
	list, err := s.Store.ImportHarassmentList(r.Context(), access.Key.ID, request.Name, request.DefaultAction, request.Numbers)
	if err != nil {
		writeFailure(w, err)
		return
	}
	_ = s.Store.AppendAudit(r.Context(), access.Device.ID, "harassment.list.import", list.ID, "imported")
	writeJSON(w, http.StatusOK, map[string]any{"list": harassListView{
		ID: list.ID, Name: list.Name, DefaultAction: list.DefaultAction, Source: list.Source,
		EntryCount: list.EntryCount, ImportedAt: list.ImportedAt.UnixMilli(),
	}})
}

func (s *Server) deleteHarassList(w http.ResponseWriter, r *http.Request, access store.Access) {
	id := r.PathValue("id")
	removed, err := s.Store.DeleteHarassmentList(r.Context(), access.Key.ID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "CB-V2-404", "名单不存在")
			return
		}
		writeFailure(w, err)
		return
	}
	_ = s.Store.AppendAudit(r.Context(), access.Device.ID, "harassment.list.delete", id, "deleted")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "entriesRemoved": removed})
}

type harassEventView struct {
	ID        string `json:"id"`
	LineID    string `json:"lineId,omitempty"`
	Kind      string `json:"kind"`
	Peer      string `json:"peer"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
	RefID     string `json:"refId,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

func (s *Server) harassHistory(w http.ResponseWriter, r *http.Request, access store.Access) {
	events, err := s.Store.ListHarassmentEvents(r.Context(), access.Key.ID, 200)
	if err != nil {
		writeFailure(w, err)
		return
	}
	views := make([]harassEventView, 0, len(events))
	for _, event := range events {
		views = append(views, harassEventView{
			ID: event.ID, LineID: event.LineID, Kind: event.Kind, Peer: event.Peer,
			Action: event.Action, Reason: event.Reason, RefID: event.RefID, CreatedAt: event.CreatedAt.UnixMilli(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": views})
}

func (s *Server) clearHarassHistory(w http.ResponseWriter, r *http.Request, access store.Access) {
	removed, err := s.Store.ClearHarassmentEvents(r.Context(), access.Key.ID)
	if err != nil {
		writeFailure(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "count": removed})
}

// harassTest runs the key's CURRENT policy against a synthetic input without
// sending anything; the advanced rules screen uses it for live feedback.
func (s *Server) harassTest(w http.ResponseWriter, r *http.Request, access store.Access) {
	var request struct {
		Kind   string `json:"kind"`
		Number string `json:"number"`
		Body   string `json:"body"`
	}
	if !decodeBody(w, r, &request) {
		return
	}
	kind := harass.KindSMS
	if request.Kind == string(harass.KindCall) {
		kind = harass.KindCall
	}
	decision := s.Manager.HarassmentDecision(r.Context(), access.Key.ID, kind, request.Number, request.Body)
	writeJSON(w, http.StatusOK, map[string]any{
		"allow": decision.Allow, "action": decision.Action, "reason": decision.Reason,
		"matchedBy": decision.MatchedBy, "pattern": decision.Pattern,
	})
}
