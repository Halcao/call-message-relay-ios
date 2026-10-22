package runtime

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"callrelay/gateway/internal/sms"
	"callrelay/gateway/internal/unified/harass"
	"callrelay/gateway/internal/unified/store"
	"callrelay/gateway/internal/unified/worker"
)

type LinePermissionsView struct {
	ReceiveSMS   bool `json:"receiveSms"`
	ReceiveCalls bool `json:"receiveCalls"`
	SendSMS      bool `json:"sendSms"`
	Dial         bool `json:"dial"`
}

type SignalView struct {
	RSSI int `json:"rssi"`
	Bars int `json:"bars"`
}

type LineView struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Enabled      bool                `json:"enabled"`
	Online       bool                `json:"online"`
	SIM          string              `json:"sim"`
	Operator     string              `json:"operator"`
	Registration string              `json:"registration"`
	Voice        string              `json:"voice"`
	SMS          string              `json:"sms"`
	Signal       SignalView          `json:"signal"`
	ActiveCallID *string             `json:"activeCallId,omitempty"`
	Permissions  LinePermissionsView `json:"permissions"`
	SMSLive      bool                `json:"smsLive"`
	Identity     LineIdentity        `json:"identity"`
	LastError    string              `json:"lastError,omitempty"`
	// PhoneNumber is the FULL effective own number (manual override or
	// SIM-derived). It is populated only on authenticated, line-authorized
	// views (paired device grants / operator console); anonymous identity,
	// health and broadcast events never carry it. Identity.PhoneMasked stays
	// for surfaces that should not reveal the full number.
	PhoneNumber string `json:"phoneNumber,omitempty"`
	// CanManageNumber reports whether THIS caller's enrollment key may edit
	// the line's own number (explicit, off-by-default capability). An
	// ordinary granted/shared key sees the number but cannot change it.
	CanManageNumber bool `json:"canManageNumber"`
}

// callAllowed enforces the four-flag permission matrix per call direction:
// inbound call records/events need receive_calls, outbound ones need dial.
func callAllowed(entry store.LineAccess, call store.Call, _ string) bool {
	if call.Direction == "inbound" {
		return entry.Permissions.ReceiveCalls
	}
	return entry.Permissions.Dial
}

func (m *Manager) LineViews(ctx context.Context, access store.Access, lineFilter string) []LineView {
	views := make([]LineView, 0, len(access.Lines))
	for _, entry := range access.Lines {
		if lineFilter != "" && entry.Line.ID != lineFilter {
			continue
		}
		view := LineView{
			ID: entry.Line.ID, Name: entry.Line.Name, Enabled: entry.Line.Enabled,
			Permissions: LinePermissionsView{
				ReceiveSMS: entry.Permissions.ReceiveSMS, ReceiveCalls: entry.Permissions.ReceiveCalls,
				SendSMS: entry.Permissions.SendSMS, Dial: entry.Permissions.Dial,
			},
			// Only devices whose key carries the explicit capability may edit
			// shared line numbers; merely seeing the number grants nothing.
			CanManageNumber: access.Key.ManageNumber,
		}
		if line := m.Line(entry.Line.ID); line != nil {
			status, reachable, _ := line.Status()
			view.Online = reachable
			view.SIM = status.SIM
			view.Registration = status.Registration
			view.Voice = status.Voice
			view.SMS = status.SMS
			view.Signal = SignalView{RSSI: status.Signal.RSSI, Bars: status.Signal.Bars}
			view.ActiveCallID = status.ActiveCallID
			dryRun, _ := line.SMSDryRun()
			view.SMSLive = !dryRun
			view.Identity = m.LineIdentity(line)
			view.Operator = resolvedOperator(
				status.Operator, view.Identity.OperatorAlpha, view.Identity.OperatorNumeric)
			// Authenticated, line-authorized paired device: show the full own
			// number so the user can actually read it in the app.
			view.PhoneNumber = m.lineFullNumber(line)
			line.mu.RLock()
			view.LastError = line.lastError
			line.mu.RUnlock()
		} else {
			view.Online = false
			view.Enabled = entry.Line.Enabled
		}
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	return views
}

// AdminLineViews returns every configured line with full permissions for
// the operator console.
func (m *Manager) AdminLineViews(ctx context.Context) []LineView {
	lines, err := m.Store.ListLines(ctx)
	if err != nil {
		return nil
	}
	views := make([]LineView, 0, len(lines))
	for _, line := range lines {
		view := LineView{
			ID: line.ID, Name: line.Name, Enabled: line.Enabled,
			Permissions: LinePermissionsView{ReceiveSMS: true, ReceiveCalls: true, SendSMS: true, Dial: true},
		}
		if runtimeLine := m.Line(line.ID); runtimeLine != nil {
			status, reachable, _ := runtimeLine.Status()
			view.Online = reachable
			view.SIM = status.SIM
			view.Registration = status.Registration
			view.Voice = status.Voice
			view.SMS = status.SMS
			view.Signal = SignalView{RSSI: status.Signal.RSSI, Bars: status.Signal.Bars}
			view.ActiveCallID = status.ActiveCallID
			dryRun, _ := runtimeLine.SMSDryRun()
			view.SMSLive = !dryRun
			view.Identity = m.LineIdentity(runtimeLine)
			view.Operator = resolvedOperator(
				status.Operator, view.Identity.OperatorAlpha, view.Identity.OperatorNumeric)
			// Authenticated operator console: show the full own number.
			view.PhoneNumber = m.lineFullNumber(runtimeLine)
			runtimeLine.mu.RLock()
			view.LastError = runtimeLine.lastError
			runtimeLine.mu.RUnlock()
		} else {
			// No live runtime object (worker down): still show the persisted
			// own number/source from the private identity row.
			identity, full := m.storedLineIdentity(line.ID)
			view.Identity = identity
			view.PhoneNumber = full
		}
		views = append(views, view)
	}
	return views
}

type ThreadView struct {
	Key         string `json:"key"`
	LineID      string `json:"lineId"`
	LineName    string `json:"lineName"`
	Peer        string `json:"peer"`
	UnreadCount int    `json:"unreadCount"`
	// Folder is THIS key's effective folder: normal | junk. Same-key devices
	// share it; a different key on the same line computes its own.
	Folder      string      `json:"folder"`
	LastMessage MessageView `json:"lastMessage"`
}

// Threads lists conversations for the authenticated key. `folder` selects
// "normal" (default), "junk" or "all". Junk is per-key policy classification
// (reject entries) overlaid with the key's explicit restore state; restored
// conversations never re-quarantine on refresh. The worker's raw history is
// never modified and another key's folder/read/delete state is untouched.
func (m *Manager) Threads(ctx context.Context, deviceID, lineFilter, folder string) ([]ThreadView, error) {
	access, err := m.Store.DeviceAccess(ctx, deviceID)
	if err != nil {
		return nil, fail(401, "CB-V2-401", "登录状态已失效")
	}
	folder = strings.TrimSpace(folder)
	if folder != "junk" && folder != "all" {
		folder = "normal"
	}
	var views []ThreadView
	for _, entry := range access.Lines {
		if !entry.Permissions.ReceiveSMS {
			continue
		}
		if lineFilter != "" && entry.Line.ID != lineFilter {
			continue
		}
		line := m.Line(entry.Line.ID)
		if line == nil {
			continue
		}
		threads, threadErr := line.Client.Threads(ctx)
		if threadErr != nil {
			continue
		}
		// Legacy line-wide tombstones (rows written before per-key state
		// existed) remain a floor; new deletes are key-scoped below.
		tombstones, tombErr := m.Store.LineThreadTombstones(ctx, entry.Line.ID)
		if tombErr != nil {
			tombstones = nil
		}
		keyStates, stateErr := m.Store.KeyThreadStates(ctx, access.Key.ID, entry.Line.ID)
		if stateErr != nil {
			keyStates = nil
		}
		for _, thread := range threads {
			lastCreated := thread.LastMessage.CreatedAt
			if horizon, deleted := tombstones[thread.Key]; deleted && lastCreated <= horizon.UnixMilli() {
				continue
			}
			state := keyStates[thread.Key]
			if state.Hidden && store.SMSBoundaryCovers(state.DeleteHorizon, state.DeleteThroughID, lastCreated, thread.LastMessage.ID) {
				continue
			}
			effective := state.Folder
			if effective != store.SMSFolderJunk && effective != store.SMSFolderRestored {
				effective = ""
			}
			if effective == "" {
				effective = store.SMSFolderNormal
				if thread.LastMessage.Direction == "inbound" &&
					m.HarassSuppressedForKey(ctx, access.Key.ID, harass.KindSMS, thread.Peer, thread.LastMessage.Body) {
					effective = store.SMSFolderJunk
				}
			} else if effective == store.SMSFolderRestored {
				effective = store.SMSFolderNormal
			}
			if folder == "normal" && effective != store.SMSFolderNormal {
				continue
			}
			if folder == "junk" && effective != store.SMSFolderJunk {
				continue
			}
			// Per-key unread: the read watermark suppresses the worker's
			// global count only when it covers the newest message.
			unread := thread.UnreadCount
			if !state.ReadHorizon.IsZero() && store.SMSBoundaryCovers(state.ReadHorizon, state.ReadThroughID, lastCreated, thread.LastMessage.ID) {
				unread = 0
			}
			views = append(views, ThreadView{
				Key: entry.Line.ID + ":" + thread.Key, LineID: entry.Line.ID, LineName: entry.Line.Name,
				Peer: thread.Peer, UnreadCount: unread, Folder: effective,
				LastMessage: m.messageView(entry.Line.ID, thread.LastMessage),
			})
		}
	}
	sort.Slice(views, func(i, j int) bool {
		return views[i].LastMessage.CreatedAt > views[j].LastMessage.CreatedAt
	})
	return views, nil
}

func splitGlobalID(globalID string) (string, string, bool) {
	lineID, workerID, ok := strings.Cut(globalID, ":")
	if !ok || lineID == "" || workerID == "" {
		return "", "", false
	}
	return lineID, workerID, true
}

func (m *Manager) ThreadMessages(ctx context.Context, deviceID, globalThreadKey string, before int64, beforeID string, limit int) ([]MessageView, bool, error) {
	lineID, workerKey, ok := splitGlobalID(globalThreadKey)
	if !ok {
		return nil, false, fail(400, "CB-V2-400", "threadKey 格式错误")
	}
	access, _, _, err := m.requireLine(ctx, deviceID, lineID, func(permission store.KeyLine) bool { return permission.ReceiveSMS })
	if err != nil {
		return nil, false, err
	}
	_ = access
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	line := m.Line(lineID)
	if line == nil {
		return nil, false, fail(410, CodeLineUnavailable, "线路未启用")
	}
	workerBeforeID := ""
	if beforeID != "" {
		if _, wid, valid := splitGlobalID(beforeID); valid {
			workerBeforeID = wid
		}
	}
	messages, hasMore, err := line.Client.ThreadMessages(ctx, workerKey, before, workerBeforeID, limit)
	if err != nil {
		return nil, false, mapWorkerFailure(err)
	}
	views := make([]MessageView, 0, len(messages))
	views, hasMore = m.applyThreadTombstone(ctx, views, messages, lineID, workerKey, hasMore, 0)
	// Per-key delete overlay: hidden history is filtered for THIS key only.
	// Quarantined (junk) messages are deliberately NOT filtered here — an
	// authorized key must be able to read its own quarantined text.
	if state, stateErr := m.Store.KeyThreadState(ctx, access.Key.ID, lineID, workerKey); stateErr == nil && state.Hidden {
		filtered := views[:0]
		for _, view := range views {
			_, workerID, _ := splitGlobalID(view.ID)
			if store.SMSBoundaryCovers(state.DeleteHorizon, state.DeleteThroughID, view.CreatedAt, workerID) {
				continue
			}
			filtered = append(filtered, view)
		}
		views = filtered
	}
	if state, stateErr := m.Store.KeyThreadState(ctx, access.Key.ID, lineID, workerKey); stateErr == nil {
		for i := range views {
			_, workerID, _ := splitGlobalID(views[i].ID)
			if views[i].Direction == "inbound" && store.SMSBoundaryCovers(state.ReadHorizon, state.ReadThroughID, views[i].CreatedAt, workerID) {
				views[i].Status = "read"
			}
		}
	}
	return views, hasMore, nil
}

// applyThreadTombstone filters messages at or before the conversation's
// delete horizon. Tombstoned rows still occupy worker pagination slots, so
// when a page filters to nothing but more exist, the next page is fetched
// (bounded) instead of handing the client a permanently empty page.
func (m *Manager) applyThreadTombstone(ctx context.Context, views []MessageView, messages []worker.Message, lineID, workerKey string, hasMore bool, depth int) ([]MessageView, bool) {
	horizon, err := m.Store.ThreadTombstone(ctx, lineID, workerKey)
	if err != nil || horizon.IsZero() {
		for _, message := range messages {
			views = append(views, m.messageView(lineID, message))
		}
		return views, hasMore
	}
	cutoff := horizon.UnixMilli()
	var filtered int
	for _, message := range messages {
		if message.CreatedAt <= cutoff {
			filtered++
			continue
		}
		views = append(views, m.messageView(lineID, message))
	}
	if filtered > 0 && len(messages) > 0 && len(views) == 0 && hasMore && depth < 3 {
		line := m.Line(lineID)
		if line != nil {
			nextBefore := messages[len(messages)-1].CreatedAt
			nextBeforeID := messages[len(messages)-1].ID
			next, nextHasMore, fetchErr := line.Client.ThreadMessages(ctx, workerKey, nextBefore, nextBeforeID, 50)
			if fetchErr == nil {
				return m.applyThreadTombstone(ctx, views, next, lineID, workerKey, nextHasMore, depth+1)
			}
		}
	}
	return views, hasMore
}

// DeleteThread hides a conversation for the authenticated KEY only. The raw
// worker history is untouched and every other key keeps its own view; all
// devices of the same key share the deletion. Messages received after the
// delete horizon reopen the conversation. The event carries the owning key id
// so a different key on the same line never applies it.
func (m *Manager) DeleteThread(ctx context.Context, deviceID, globalThreadKey string) error {
	lineID, workerKey, ok := splitGlobalID(globalThreadKey)
	if !ok {
		return fail(400, "CB-V2-400", "threadKey 格式错误")
	}
	access, _, _, err := m.requireLine(ctx, deviceID, lineID, func(permission store.KeyLine) bool { return permission.ReceiveSMS })
	if err != nil {
		return err
	}
	deletedAt := time.Now().UTC()
	if err := m.Store.HideKeyThread(ctx, access.Key.ID, lineID, workerKey, deviceID, deletedAt); err != nil {
		return err
	}
	_ = m.Store.AppendAudit(ctx, deviceID, "thread.delete", globalThreadKey, "accepted")
	m.publish(lineID, "", "thread.deleted", map[string]any{
		"key": globalThreadKey, "lineId": lineID, "deletedAt": deletedAt.UnixMilli(), "keyId": access.Key.ID,
	})
	return nil
}

// RestoreThread returns a quarantined conversation to this key's normal list
// and pins it as restored so a refresh (or a later message) cannot move it
// back to junk. Key-scoped like every other overlay decision.
func (m *Manager) RestoreThread(ctx context.Context, deviceID, globalThreadKey string) error {
	lineID, workerKey, ok := splitGlobalID(globalThreadKey)
	if !ok {
		return fail(400, "CB-V2-400", "threadKey 格式错误")
	}
	access, _, _, err := m.requireLine(ctx, deviceID, lineID, func(permission store.KeyLine) bool { return permission.ReceiveSMS })
	if err != nil {
		return err
	}
	if err := m.Store.RestoreKeyThread(ctx, access.Key.ID, lineID, workerKey, deviceID); err != nil {
		return err
	}
	_ = m.Store.AppendAudit(ctx, deviceID, "thread.restore", globalThreadKey, "accepted")
	m.publish(lineID, "", "thread.restored", map[string]any{
		"key": globalThreadKey, "lineId": lineID, "keyId": access.Key.ID, "restoredAt": time.Now().UTC().UnixMilli(),
	})
	return nil
}

func (m *Manager) SendMessage(ctx context.Context, deviceID, lineID, to, body, idempotencyKey string) (MessageView, error) {
	_, _, line, err := m.requireLine(ctx, deviceID, lineID, func(permission store.KeyLine) bool { return permission.SendSMS })
	if err != nil {
		return MessageView{}, err
	}
	if strings.TrimSpace(to) == "" || strings.TrimSpace(body) == "" {
		return MessageView{}, fail(400, "CB-V2-400", "收件人和内容不能为空")
	}
	// Normalize the dialable destination ("+86 130 0313 2132" -> +8613003132132)
	// before forwarding to the worker; otherwise its PDU encoder rejects the
	// send before persisting the message. Extensions/pauses/letters are
	// rejected explicitly rather than silently concatenated.
	var validDestination bool
	if to, validDestination = sms.ValidateDestination(to); !validDestination {
		return MessageView{}, fail(400, "CB-V2-400", "收件号码无效")
	}
	message, err := line.Client.SendMessage(ctx, to, body, idempotencyKey)
	if err != nil {
		return MessageView{}, mapWorkerFailure(err)
	}
	return m.messageView(lineID, message), nil
}

func (m *Manager) ReadMessage(ctx context.Context, deviceID, globalMessageID, idempotencyKey string) error {
	lineID, workerID, ok := splitGlobalID(globalMessageID)
	if !ok {
		return fail(400, "CB-V2-400", "消息 id 格式错误")
	}
	if _, _, _, err := m.requireLine(ctx, deviceID, lineID, func(permission store.KeyLine) bool { return permission.ReceiveSMS }); err != nil {
		return err
	}
	line := m.Line(lineID)
	if line == nil {
		return fail(410, CodeLineUnavailable, "线路未启用")
	}
	access, _, _, err := m.requireLine(ctx, deviceID, lineID, func(permission store.KeyLine) bool { return permission.ReceiveSMS })
	if err != nil {
		return err
	}
	_ = idempotencyKey
	// Per-key read state only: the worker's global unread marker is shared by
	// every key/device and must not be flipped by one key. The thread read
	// watermark advances to the message timestamp so this key's unread count
	// drops while another key's stays.
	threadKey, createdAt, indexed := m.Store.MessageThread(ctx, lineID, workerID)
	if !indexed {
		// Unknown IDs may be individually recorded, but never advance a
		// conversation watermark to now.
		createdAt = time.Time{}
	}
	if err := m.Store.MarkKeyMessageRead(ctx, access.Key.ID, lineID, lineID+":"+workerID, threadKey, createdAt); err != nil {
		return err
	}
	return nil
}

// AdminCallView exposes the call view for the console.
func (m *Manager) AdminCallView(call store.Call) CallView { return m.view(call) }

// AdminVoicemailView exposes voicemail metadata for the console.
func (m *Manager) AdminVoicemailView(voicemail store.Voicemail) VoicemailView {
	return m.voicemailView(voicemail)
}

// PushConfigured reports whether an APNs/push broker is wired.
func (m *Manager) PushConfigured() bool { return m.Push != nil }

// TURNPolicy reports the effective client ICE policy without exposing any
// credential material.
func (m *Manager) TURNPolicy() string {
	_, policy := m.ICEConfiguration()
	return policy
}

func (m *Manager) MaxConcurrentCalls(ctx context.Context) int {
	if value, err := m.Store.Setting(ctx, "max_concurrent_calls"); err == nil {
		if parsed, parseErr := strconv.Atoi(strings.TrimSpace(value)); parseErr == nil && parsed > 0 && parsed <= 32 {
			return parsed
		}
	}
	return m.Cfg.Settings.MaxConcurrentCalls
}

func (m *Manager) RecentEventsForConsole(ctx context.Context, limit int) ([]map[string]any, error) {
	stored, err := m.Store.EventsAfter(ctx, 0, limit)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(stored))
	for i := len(stored) - 1; i >= 0; i-- {
		event := stored[i]
		result = append(result, map[string]any{
			"seq": event.Seq, "type": event.Type, "lineId": event.LineID,
			"createdAt": event.CreatedAt.UnixMilli(), "payload": string(event.Payload),
		})
	}
	return result, nil
}

var _ = worker.Message{}
var _ = time.Now
