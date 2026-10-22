package runtime

import (
	"callrelay/gateway/internal/unified/store"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type SMSThreadAction struct {
	ID               string `json:"id"`
	Kind             string `json:"kind"`
	ThreadKey        string `json:"threadKey"`
	ThroughCreatedAt int64  `json:"throughCreatedAt"`
	ThroughID        string `json:"throughID"`
}
type SMSActionError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}
type SMSActionResult struct {
	ID    string          `json:"id"`
	OK    bool            `json:"ok"`
	Error *SMSActionError `json:"error,omitempty"`
}

func (m *Manager) ThreadActions(ctx context.Context, deviceID string, actions []SMSThreadAction) []SMSActionResult {
	results := make([]SMSActionResult, 0, len(actions))
	for _, action := range actions {
		result := SMSActionResult{ID: action.ID}
		err := m.applyThreadAction(ctx, deviceID, action)
		if err == nil {
			result.OK = true
		} else {
			code, message, retryable := "CB-V2-500", "暂时无法同步", true
			var failure *Failure
			if errors.As(err, &failure) {
				code, message, retryable = failure.Code, failure.Message, failure.Status >= 500 || failure.Status == 429
			}
			result.Error = &SMSActionError{Code: code, Message: message, Retryable: retryable}
		}
		results = append(results, result)
	}
	return results
}

func (m *Manager) applyThreadAction(ctx context.Context, deviceID string, action SMSThreadAction) error {
	lineID, workerKey, ok := splitGlobalID(action.ThreadKey)
	messageLine, workerID, messageOK := splitGlobalID(action.ThroughID)
	if !ok || !messageOK || messageLine != lineID || strings.TrimSpace(action.ID) == "" || len(action.ID) > 128 || (action.Kind != "read" && action.Kind != "delete") || action.ThroughCreatedAt <= 0 {
		return fail(400, "CB-V2-400", "操作或消息边界无效")
	}
	access, _, line, err := m.requireLine(ctx, deviceID, lineID, func(p store.KeyLine) bool { return p.ReceiveSMS })
	if err != nil {
		return err
	}
	// Resolve exact tuple from the event/history index. A cold gateway may need
	// worker pagination, with no four-page ceiling; no unknown->now fallback.
	thread, at, indexed := m.Store.MessageThread(ctx, lineID, workerID)
	if !indexed {
		if line == nil {
			return fail(410, CodeLineUnavailable, "线路未启用")
		}
		var before int64
		var beforeID string
		for {
			messages, more, fetchErr := line.Client.ThreadMessages(ctx, workerKey, before, beforeID, 200)
			if fetchErr != nil {
				return mapWorkerFailure(fetchErr)
			}
			for _, message := range messages {
				_ = m.Store.RecordMessageThread(ctx, lineID, message.ID, message.ThreadKey, time.UnixMilli(message.CreatedAt))
				if message.ID == workerID {
					thread = message.ThreadKey
					at = time.UnixMilli(message.CreatedAt)
					indexed = true
				}
			}
			if indexed || !more || len(messages) == 0 {
				break
			}
			last := messages[0]
			for _, candidate := range messages[1:] {
				if candidate.CreatedAt < last.CreatedAt || candidate.CreatedAt == last.CreatedAt && candidate.ID < last.ID {
					last = candidate
				}
			}
			if before == last.CreatedAt && beforeID == last.ID {
				return fail(502, "CB-V2-502", "消息分页暂时不可用")
			}
			before, beforeID = last.CreatedAt, last.ID
		}
	}
	if !indexed || thread != workerKey || at.UnixMilli() != action.ThroughCreatedAt {
		return fail(400, "CB-V2-400", "消息边界不属于此会话")
	}
	// The cold-index scan may take multiple requests; recheck current grants
	// immediately before committing, including revocation during pagination.
	access, _, _, err = m.requireLine(ctx, deviceID, lineID, func(p store.KeyLine) bool { return p.ReceiveSMS })
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(action)
	applied, err := m.Store.ApplyKeyThreadAction(ctx, access.Key.ID, deviceID, action.ID, string(payload), action.Kind, lineID, workerKey, at, workerID)
	if errors.Is(err, store.ErrSMSActionConflict) {
		return fail(409, "CB-V2-409", "操作编号已用于其他内容")
	}
	if err != nil {
		return err
	}
	// A replay must not emit an old deletion after a later restore. A gap
	// between commit and publish is reconciled by conditional history refresh.
	if !applied {
		return nil
	}
	event := "thread.read"
	data := map[string]any{"key": action.ThreadKey, "lineId": lineID, "keyId": access.Key.ID, "throughCreatedAt": action.ThroughCreatedAt, "throughID": action.ThroughID}
	if action.Kind == "delete" {
		event = "thread.deleted"
		data["deletedAt"] = action.ThroughCreatedAt
	}
	m.publish(lineID, "", event, data)
	return nil
}
