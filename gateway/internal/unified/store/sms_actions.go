package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrSMSActionConflict = errors.New("SMS operation payload differs")

// SMSBoundaryCovers compares worker pagination tuples. Empty stored IDs are
// legacy timestamp watermarks, which historically cover every tie.
func SMSBoundaryCovers(horizon time.Time, throughID string, createdAt int64, messageID string) bool {
	if horizon.IsZero() {
		return false
	}
	return createdAt < horizon.UnixMilli() || createdAt == horizon.UnixMilli() && (throughID == "" || messageID <= throughID)
}

// ApplyKeyThreadAction commits the overlay and replay record atomically. A
// replay never advances its horizon to a newer message, even after restart.
func (s *Store) ApplyKeyThreadAction(ctx context.Context, keyID, deviceID, operationID, payload, kind, lineID, threadKey string, at time.Time, throughID string) (bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var prior string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM sms_thread_actions WHERE key_id=? AND operation_id=?`, keyID, operationID).Scan(&prior)
	if err == nil {
		if prior != payload {
			return false, ErrSMSActionConflict
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	state, err := scanKeyThreadState(tx.QueryRowContext(ctx, `SELECT key_id,line_id,thread_key,folder,hidden,delete_horizon,read_horizon,delete_through_id,read_through_id,updated_by,updated_at FROM sms_key_threads WHERE key_id=? AND line_id=? AND thread_key=?`, keyID, lineID, threadKey))
	if errors.Is(err, sql.ErrNoRows) {
		state = KeyThreadState{KeyID: keyID, LineID: lineID, ThreadKey: threadKey, Folder: SMSFolderNormal}
	} else if err != nil {
		return false, err
	}
	if kind == "read" {
		if !SMSBoundaryCovers(state.ReadHorizon, state.ReadThroughID, at.UnixMilli(), throughID) {
			state.ReadHorizon = at
			state.ReadThroughID = throughID
		}
	} else {
		state.Hidden = true
		if !SMSBoundaryCovers(state.DeleteHorizon, state.DeleteThroughID, at.UnixMilli(), throughID) {
			state.DeleteHorizon = at
			state.DeleteThroughID = throughID
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sms_key_threads (key_id,line_id,thread_key,folder,hidden,delete_horizon,read_horizon,delete_through_id,read_through_id,updated_by,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(key_id,line_id,thread_key) DO UPDATE SET hidden=excluded.hidden,delete_horizon=excluded.delete_horizon,read_horizon=excluded.read_horizon,delete_through_id=excluded.delete_through_id,read_through_id=excluded.read_through_id,updated_by=excluded.updated_by,updated_at=excluded.updated_at`, keyID, lineID, threadKey, state.Folder, boolInt(state.Hidden), millis(state.DeleteHorizon), millis(state.ReadHorizon), state.DeleteThroughID, state.ReadThroughID, deviceID, nowMillis())
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sms_thread_actions (key_id,operation_id,payload,created_at) VALUES (?,?,?,?)`, keyID, operationID, payload, nowMillis()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
