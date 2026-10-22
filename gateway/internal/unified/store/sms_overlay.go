package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SMS folders in the per-key overlay.
const (
	SMSFolderNormal   = "normal"
	SMSFolderJunk     = "junk"
	SMSFolderRestored = "restored"
)

// KeyThreadState is one enrollment key's overlay row for one conversation.
// The zero value means "no explicit decision": the runtime then derives the
// folder from the key's harassment policy and uses worker unread counts.
type KeyThreadState struct {
	KeyID           string
	LineID          string
	ThreadKey       string
	Folder          string
	Hidden          bool
	DeleteHorizon   time.Time
	DeleteThroughID string
	ReadHorizon     time.Time
	ReadThroughID   string
	UpdatedBy       string
	UpdatedAt       time.Time
}

func scanKeyThreadState(row rowScanner) (KeyThreadState, error) {
	var (
		state                      KeyThreadState
		hidden                     int
		deleteHorizon, readHorizon int64
		updatedAt                  int64
	)
	err := row.Scan(&state.KeyID, &state.LineID, &state.ThreadKey, &state.Folder, &hidden,
		&deleteHorizon, &readHorizon, &state.DeleteThroughID, &state.ReadThroughID, &state.UpdatedBy, &updatedAt)
	if err != nil {
		return KeyThreadState{}, err
	}
	state.Hidden = hidden != 0
	state.DeleteHorizon = fromMillis(deleteHorizon)
	state.ReadHorizon = fromMillis(readHorizon)
	state.UpdatedAt = fromMillis(updatedAt)
	return state, nil
}

// KeyThreadState returns one key's overlay row, or the zero value when the
// key never made a decision (never an error).
func (s *Store) KeyThreadState(ctx context.Context, keyID, lineID, threadKey string) (KeyThreadState, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT key_id, line_id, thread_key, folder, hidden, delete_horizon, read_horizon, delete_through_id, read_through_id, updated_by, updated_at
		FROM sms_key_threads WHERE key_id = ? AND line_id = ? AND thread_key = ?`, keyID, lineID, threadKey)
	state, err := scanKeyThreadState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return KeyThreadState{}, nil
	}
	if err != nil {
		return KeyThreadState{}, err
	}
	return state, nil
}

// KeyThreadStates loads every overlay row for one key/line in one query.
func (s *Store) KeyThreadStates(ctx context.Context, keyID, lineID string) (map[string]KeyThreadState, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT key_id, line_id, thread_key, folder, hidden, delete_horizon, read_horizon, delete_through_id, read_through_id, updated_by, updated_at
		FROM sms_key_threads WHERE key_id = ? AND line_id = ?`, keyID, lineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]KeyThreadState{}
	for rows.Next() {
		state, scanErr := scanKeyThreadState(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		states[state.ThreadKey] = state
	}
	return states, rows.Err()
}

func (s *Store) upsertKeyThread(ctx context.Context, keyID, lineID, threadKey, deviceID string, mutate func(*KeyThreadState)) error {
	state, err := s.KeyThreadState(ctx, keyID, lineID, threadKey)
	if err != nil {
		return err
	}
	if state.KeyID == "" {
		state = KeyThreadState{KeyID: keyID, LineID: lineID, ThreadKey: threadKey, Folder: SMSFolderNormal}
	}
	mutate(&state)
	_, err = s.DB.ExecContext(ctx, `INSERT INTO sms_key_threads
		(key_id, line_id, thread_key, folder, hidden, delete_horizon, read_horizon, delete_through_id, read_through_id, updated_by, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_id, line_id, thread_key) DO UPDATE SET
		folder = excluded.folder, hidden = excluded.hidden, delete_horizon = excluded.delete_horizon,
		read_horizon = excluded.read_horizon, delete_through_id = excluded.delete_through_id, read_through_id = excluded.read_through_id, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		state.KeyID, state.LineID, state.ThreadKey, state.Folder, boolInt(state.Hidden),
		millis(state.DeleteHorizon), millis(state.ReadHorizon), state.DeleteThroughID, state.ReadThroughID, deviceID, nowMillis())
	return err
}

// SetKeyThreadFolder persists an explicit folder decision (junk/normal/
// restored) for THIS key only.
func (s *Store) SetKeyThreadFolder(ctx context.Context, keyID, lineID, threadKey, folder, deviceID string) error {
	switch folder {
	case SMSFolderNormal, SMSFolderJunk, SMSFolderRestored:
	default:
		folder = SMSFolderNormal
	}
	return s.upsertKeyThread(ctx, keyID, lineID, threadKey, deviceID, func(state *KeyThreadState) {
		state.Folder = folder
	})
}

// HideKeyThread marks a conversation deleted for this key: hidden plus a
// delete horizon at `at`. Messages created after the horizon reopen it. The
// worker's raw history is untouched and every other key keeps its view.
func (s *Store) HideKeyThread(ctx context.Context, keyID, lineID, threadKey, deviceID string, at time.Time) error {
	return s.upsertKeyThread(ctx, keyID, lineID, threadKey, deviceID, func(state *KeyThreadState) {
		state.Hidden = true
		state.DeleteHorizon = at
		state.DeleteThroughID = ""
	})
}

// RestoreKeyThread clears the key's hidden/delete state and pins the folder
// to restored, so neither a refresh nor a later inbound message can move the
// conversation back to junk.
func (s *Store) RestoreKeyThread(ctx context.Context, keyID, lineID, threadKey, deviceID string) error {
	return s.upsertKeyThread(ctx, keyID, lineID, threadKey, deviceID, func(state *KeyThreadState) {
		state.Folder = SMSFolderRestored
		state.Hidden = false
		state.DeleteHorizon = time.Time{}
		state.DeleteThroughID = ""
	})
}

// MarkKeyThreadRead advances this key's read watermark for the thread.
func (s *Store) MarkKeyThreadRead(ctx context.Context, keyID, lineID, threadKey, deviceID string, horizon time.Time) error {
	return s.upsertKeyThread(ctx, keyID, lineID, threadKey, deviceID, func(state *KeyThreadState) {
		if horizon.After(state.ReadHorizon) || horizon.Equal(state.ReadHorizon) && state.ReadThroughID != "" {
			state.ReadHorizon = horizon
			state.ReadThroughID = ""
		}
	})
}

// MarkKeyMessageRead records one message as read for one key and advances the
// thread watermark to at least its timestamp. The worker is never touched:
// read receipts are per-key state, not a global mutation.
func (s *Store) MarkKeyMessageRead(ctx context.Context, keyID, lineID, messageID, threadKey string, createdAt time.Time) error {
	now := nowMillis()
	messageAt := millis(createdAt)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO sms_key_messages (key_id, message_id, thread_key, read_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key_id, message_id) DO UPDATE SET read_at = excluded.read_at, updated_at = excluded.updated_at`,
		keyID, messageID, threadKey, messageAt, now); err != nil {
		return err
	}
	if threadKey == "" || createdAt.IsZero() {
		return nil
	}
	return s.MarkKeyThreadRead(ctx, keyID, lineID, threadKey, "", createdAt)
}

// KeyMessageRead reports whether this key already read one message.
func (s *Store) KeyMessageRead(ctx context.Context, keyID, messageID string) (bool, error) {
	var readAt int64
	err := s.DB.QueryRowContext(ctx, `SELECT read_at FROM sms_key_messages WHERE key_id = ? AND message_id = ?`,
		keyID, messageID).Scan(&readAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return readAt > 0, nil
}

// RecordMessageThread indexes one message's conversation from the live event
// stream so a later read request can advance the correct thread watermark.
func (s *Store) RecordMessageThread(ctx context.Context, lineID, messageID, threadKey string, createdAt time.Time) error {
	if messageID == "" || threadKey == "" {
		return nil
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO sms_message_threads (line_id, message_id, thread_key, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(line_id, message_id) DO UPDATE SET thread_key = excluded.thread_key, created_at = excluded.created_at
        WHERE sms_message_threads.thread_key != excluded.thread_key OR sms_message_threads.created_at != excluded.created_at`,
		lineID, messageID, threadKey, millis(createdAt))
	return err
}

// MessageThread resolves the conversation for one worker message id.
func (s *Store) MessageThread(ctx context.Context, lineID, messageID string) (threadKey string, createdAt time.Time, ok bool) {
	var created int64
	err := s.DB.QueryRowContext(ctx, `SELECT thread_key, created_at FROM sms_message_threads
		WHERE line_id = ? AND message_id = ?`, lineID, messageID).Scan(&threadKey, &created)
	if err != nil {
		return "", time.Time{}, false
	}
	return threadKey, fromMillis(created), true
}
