package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AlertEvent is one reserved outbound notification. The ID is the dedupe key
// (for example "wan:inc_abc:unavailable"): ReserveAlertEvent inserts exactly
// once, so restarts and repeated observations can never duplicate an alert.
// Detail carries the push payload fields minus credentials/tokens.
type AlertEvent struct {
	ID          string
	Kind        string
	LineID      string
	IncidentID  string
	Phase       string
	Detail      string
	CreatedAt   time.Time
	Attempts    int
	DeliveredAt *time.Time
}

// AlertValue reads one durable notifier state document.
func (s *Store) AlertValue(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM alert_state WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read alert state: %w", err)
	}
	return value, true, nil
}

// SetAlertValue writes one durable notifier state document.
func (s *Store) SetAlertValue(ctx context.Context, key, value string) error {
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO alert_state (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, nowMillis()); err != nil {
		return fmt.Errorf("write alert state: %w", err)
	}
	return nil
}

// ReserveAlertEvent claims the alert ID. It reports false when the event was
// already reserved (or delivered) earlier, which is the persistent
// one-fault-one-alert guarantee across restarts.
func (s *Store) ReserveAlertEvent(ctx context.Context, event AlertEvent) (bool, error) {
	created := event.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	result, err := s.DB.ExecContext(ctx, `
		INSERT OR IGNORE INTO alert_events (id, kind, line_id, incident_id, phase, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.Kind, event.LineID, event.IncidentID, event.Phase, event.Detail, millis(created))
	if err != nil {
		return false, fmt.Errorf("reserve alert: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reserve alert: %w", err)
	}
	return rows == 1, nil
}

// MarkAlertAttempt records one delivery attempt without marking the alert
// delivered: the reservation stays pending until at least one target accepts.
func (s *Store) MarkAlertAttempt(ctx context.Context, id string) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE alert_events SET attempts = attempts + 1, last_attempt_at = ? WHERE id = ?`,
		nowMillis(), id); err != nil {
		return fmt.Errorf("mark alert attempt: %w", err)
	}
	return nil
}

// MarkAlertDelivered retires the reservation after a target accepted it.
func (s *Store) MarkAlertDelivered(ctx context.Context, id string) error {
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE alert_events SET delivered_at = ? WHERE id = ?`, nowMillis(), id); err != nil {
		return fmt.Errorf("mark alert delivered: %w", err)
	}
	return nil
}

// AlertDelivery is one target's delivery state for a reserved alert.
type AlertDelivery struct {
	TargetKey     string
	Attempts      int
	LastAttemptAt *time.Time
	DeliveredAt   *time.Time
}

// ListAlertDeliveries returns the per-target delivery ledger for an alert.
func (s *Store) ListAlertDeliveries(ctx context.Context, eventID string) (map[string]AlertDelivery, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT target_key, attempts, last_attempt_at, delivered_at FROM alert_deliveries WHERE event_id = ?`, eventID)
	if err != nil {
		return nil, fmt.Errorf("list alert deliveries: %w", err)
	}
	defer rows.Close()
	deliveries := map[string]AlertDelivery{}
	for rows.Next() {
		var delivery AlertDelivery
		var lastAttempt, delivered sql.NullInt64
		if err := rows.Scan(&delivery.TargetKey, &delivery.Attempts, &lastAttempt, &delivered); err != nil {
			return nil, err
		}
		if lastAttempt.Valid {
			t := fromMillis(lastAttempt.Int64)
			delivery.LastAttemptAt = &t
		}
		if delivered.Valid {
			t := fromMillis(delivered.Int64)
			delivery.DeliveredAt = &t
		}
		deliveries[delivery.TargetKey] = delivery
	}
	return deliveries, rows.Err()
}

// RecordAlertDelivery records one attempt for a target; delivered=true also
// retires that target so retries never duplicate a notification.
func (s *Store) RecordAlertDelivery(ctx context.Context, eventID, targetKey string, delivered bool) error {
	deliveredAt := sql.NullInt64{}
	if delivered {
		deliveredAt = sql.NullInt64{Int64: nowMillis(), Valid: true}
	}
	if _, err := s.DB.ExecContext(ctx, `
		INSERT INTO alert_deliveries (event_id, target_key, attempts, last_attempt_at, delivered_at)
		VALUES (?, ?, 1, ?, ?)
		ON CONFLICT(event_id, target_key) DO UPDATE SET
			attempts = attempts + 1,
			last_attempt_at = excluded.last_attempt_at,
			delivered_at = COALESCE(alert_deliveries.delivered_at, excluded.delivered_at)`,
		eventID, targetKey, nowMillis(), deliveredAt); err != nil {
		return fmt.Errorf("record alert delivery: %w", err)
	}
	return nil
}

// PendingAlertEvents returns reserved-but-undelivered alerts with fewer than
// maxAttempts attempts, oldest first, so a failed first fan-out is retried
// while the incident is still active.
func (s *Store) PendingAlertEvents(ctx context.Context, maxAttempts, limit int) ([]AlertEvent, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, kind, line_id, incident_id, phase, detail, created_at, attempts
		FROM alert_events
		WHERE delivered_at IS NULL AND attempts < ?
		ORDER BY created_at ASC LIMIT ?`, maxAttempts, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending alerts: %w", err)
	}
	defer rows.Close()
	var events []AlertEvent
	for rows.Next() {
		var event AlertEvent
		var createdAt int64
		if err := rows.Scan(&event.ID, &event.Kind, &event.LineID, &event.IncidentID,
			&event.Phase, &event.Detail, &createdAt, &event.Attempts); err != nil {
			return nil, err
		}
		event.CreatedAt = fromMillis(createdAt)
		events = append(events, event)
	}
	return events, rows.Err()
}
