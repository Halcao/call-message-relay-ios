// Package store owns the unified service's durable state: line registry,
// enrollment keys, device identities/tokens, permissions, call and
// conference arbitration records, the global event log and console sessions.
// It never stores SMS bodies or call media; those stay in the per-line
// worker databases, which the unified service queries and merges.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

var (
	ErrKeyRevoked          = errors.New("enrollment key revoked")
	ErrKeyActive           = errors.New("enrollment key is not revoked")
	ErrInstallationRevoked = errors.New("installation identity revoked")
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS lines (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  worker_url TEXT NOT NULL,
  token_file TEXT NOT NULL DEFAULT '',
  identity_file TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  sms_dry_run INTEGER NOT NULL DEFAULT 1,
  reject_policy TEXT NOT NULL DEFAULT 'none',
  blocked_prefixes TEXT NOT NULL DEFAULT '',
  allow_inbound INTEGER NOT NULL DEFAULT 1,
  allow_outbound INTEGER NOT NULL DEFAULT 1,
  recording_retention_days INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS enrollment_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  secret_hash BLOB NOT NULL,
  -- Operator-recoverable copy of the one-time secret, AES-256-GCM sealed by
  -- the root-only master key. NULL for keys created before this storage
  -- existed (hash-only, irreversible) and while no master key is configured.
  -- Never used for authentication: secret_hash stays authoritative.
  secret_encrypted BLOB,
  all_lines INTEGER NOT NULL DEFAULT 1,
  manage_number INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  revoked_at INTEGER
);

CREATE TABLE IF NOT EXISTS key_lines (
  key_id TEXT NOT NULL,
  line_id TEXT NOT NULL,
  receive_sms INTEGER NOT NULL DEFAULT 1,
  receive_calls INTEGER NOT NULL DEFAULT 1,
  send_sms INTEGER NOT NULL DEFAULT 1,
  dial INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (key_id, line_id)
);

CREATE TABLE IF NOT EXISTS devices (
  id TEXT PRIMARY KEY,
  key_id TEXT NOT NULL,
  name TEXT NOT NULL,
  public_key BLOB NOT NULL,
  default_line_id TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  revoked_at INTEGER,
  apns_token TEXT NOT NULL DEFAULT '',
  voip_token TEXT NOT NULL DEFAULT '',
  push_environment TEXT NOT NULL DEFAULT '',
  push_locale TEXT NOT NULL DEFAULT '',
  push_updated_at INTEGER
);

CREATE TABLE IF NOT EXISTS device_tokens (
  token_hash BLOB PRIMARY KEY,
  device_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  revoked_at INTEGER
);

-- Optional per-device Bark notification bridge. device_key is a secret: only
-- the outbound notifier reads it and the settings API returns just key_hint.
-- Revocation is checked on every read (devices/keys keep their rows).
CREATE TABLE IF NOT EXISTS device_bark (
  device_id TEXT PRIMARY KEY,
  enabled INTEGER NOT NULL DEFAULT 0,
  server_url TEXT NOT NULL DEFAULT '',
  device_key TEXT NOT NULL DEFAULT '',
  key_hint TEXT NOT NULL DEFAULT '',
  allow_private INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);

-- Self-hosted PWA Web Push. Subscriptions belong to one authenticated device
-- (many browsers/phones per device are allowed). auth/p256dh are secrets:
-- only the outbound notifier reads them and no API view ever returns them.
CREATE TABLE IF NOT EXISTS webpush_subscriptions (
  id TEXT PRIMARY KEY,
  device_id TEXT NOT NULL,
  endpoint TEXT NOT NULL UNIQUE,
  auth TEXT NOT NULL,
  p256dh TEXT NOT NULL,
  user_agent TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  revoked_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_webpush_subscriptions_device ON webpush_subscriptions (device_id, revoked_at);

-- Single-use bind tokens: minted by the paired app, exchanged in the browser
-- for a web session. Only the SHA-256 hash is stored.
CREATE TABLE IF NOT EXISTS webpush_bind_tokens (
  token_hash TEXT PRIMARY KEY,
  device_id TEXT NOT NULL,
  -- 'push' (default): web-push subscription management only.
  -- 'client': standalone browser client (call/SMS/lines APIs) in addition.
  scope TEXT NOT NULL DEFAULT 'push',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at INTEGER
);

-- Browser web sessions (derived, revocable, scope-limited credentials).
CREATE TABLE IF NOT EXISTS webpush_sessions (
  token_hash TEXT PRIMARY KEY,
  device_id TEXT NOT NULL,
  -- 'push' or 'client'; see webpush_bind_tokens.scope.
  scope TEXT NOT NULL DEFAULT 'push',
  created_at INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  revoked_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_webpush_sessions_device ON webpush_sessions (device_id);

CREATE TABLE IF NOT EXISTS calls (
  id TEXT PRIMARY KEY,
  line_id TEXT NOT NULL,
  worker_call_id TEXT NOT NULL,
  device_id TEXT NOT NULL DEFAULT '',
  direction TEXT NOT NULL,
  peer TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  held INTEGER NOT NULL DEFAULT 0,
  conference_id TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  answered_at INTEGER,
  ended_at INTEGER,
  end_reason TEXT NOT NULL DEFAULT '',
  declined_by TEXT NOT NULL DEFAULT '',
  recording_id TEXT NOT NULL DEFAULT '',
  recording_state TEXT NOT NULL DEFAULT '',
  recording_duration_ms INTEGER,
  UNIQUE (line_id, worker_call_id)
);

-- Manual call recordings. The index row lives on the eMMC with every other
-- durable artifact; the audio blob path points into the verified SD mount.
-- created_by_device_id records which authenticated device started the
-- recording; read access is still line-permission scoped (like voicemail).
CREATE TABLE IF NOT EXISTS recordings (
  id TEXT PRIMARY KEY,
  line_id TEXT NOT NULL,
  call_id TEXT NOT NULL DEFAULT '',
  -- kind is 'call' for a manual 1:1 segment and 'conference' for a hosted
  -- conference segment; conference_id is set only for the latter (and
  -- mirrors call_id = conference id there).
  kind TEXT NOT NULL DEFAULT 'call',
  conference_id TEXT NOT NULL DEFAULT '',
  device_id TEXT NOT NULL DEFAULT '',
  key_id TEXT NOT NULL DEFAULT '',
  direction TEXT NOT NULL DEFAULT '',
  peer TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'starting',
  trigger_mode TEXT NOT NULL DEFAULT 'manual',
  started_at INTEGER,
  stopped_at INTEGER,
  duration_ms INTEGER,
  file_path TEXT NOT NULL DEFAULT '',
  container TEXT NOT NULL DEFAULT '',
  codec TEXT NOT NULL DEFAULT '',
  sample_rate INTEGER NOT NULL DEFAULT 0,
  channels INTEGER NOT NULL DEFAULT 0,
  bitrate INTEGER NOT NULL DEFAULT 0,
  size_bytes INTEGER,
  sha256 TEXT NOT NULL DEFAULT '',
  waveform_path TEXT NOT NULL DEFAULT '',
  failure_code TEXT NOT NULL DEFAULT '',
  failure_detail TEXT NOT NULL DEFAULT '',
  retention_expires_at INTEGER,
  created_by_device_id TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_recordings_line ON recordings (line_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_recordings_call ON recordings (call_id);

CREATE TABLE IF NOT EXISTS conferences (
  id TEXT PRIMARY KEY,
  host_device_id TEXT NOT NULL,
  state TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  ended_at INTEGER,
  grace_deadline INTEGER
);

CREATE TABLE IF NOT EXISTS conference_legs (
  conference_id TEXT NOT NULL,
  call_id TEXT NOT NULL,
  held INTEGER NOT NULL DEFAULT 0,
  joined_at INTEGER NOT NULL,
  removed_at INTEGER,
  PRIMARY KEY (conference_id, call_id)
);

CREATE TABLE IF NOT EXISTS events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  id TEXT NOT NULL UNIQUE,
  line_id TEXT NOT NULL DEFAULT '',
  device_id TEXT NOT NULL DEFAULT '',
  type TEXT NOT NULL,
  payload TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS console_sessions (
  token_hash BLOB PRIMARY KEY,
  csrf TEXT NOT NULL,
  username TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  remote_addr TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS audit_log (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  target TEXT NOT NULL DEFAULT '',
  result TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS idempotency (
  device_id TEXT NOT NULL,
  operation TEXT NOT NULL,
  request_key TEXT NOT NULL,
  status_code INTEGER NOT NULL DEFAULT 0,
  response_json TEXT,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (device_id, operation, request_key)
);

CREATE TABLE IF NOT EXISTS worker_tokens (
  line_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  token TEXT NOT NULL,
  expires_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (line_id, kind)
);

CREATE TABLE IF NOT EXISTS revoked_public_keys (
  public_key_hash BLOB PRIMARY KEY,
  device_id TEXT NOT NULL DEFAULT '',
  revoked_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS voicemails (
  id TEXT PRIMARY KEY,
  line_id TEXT NOT NULL,
  call_id TEXT NOT NULL DEFAULT '',
  peer TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'recording',
  path TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  size_bytes INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  deleted_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_voicemails_line ON voicemails (line_id, created_at DESC);

-- Per-thread SMS conversation tombstones. The daemon proxies message bodies
-- from the workers and never stores them; a delete is therefore recorded as
-- a tombstone keyed by (line, thread) and applied at list/merge time so
-- every device converges without rewriting worker history. Messages created
-- AFTER the tombstone belong to a new conversation and stay visible.
CREATE TABLE IF NOT EXISTS thread_tombstones (
  line_id TEXT NOT NULL,
  thread_key TEXT NOT NULL,
  device_id TEXT NOT NULL DEFAULT '',
  deleted_at INTEGER NOT NULL,
  PRIMARY KEY (line_id, thread_key)
);

-- Per-enrollment-key harassment (骚扰拦截) policy. Scope is the key, not a
-- device and not a contact principal: every device of one key shares exactly
-- one policy, while a different key on the same physical line is completely
-- isolated (its own settings, lists, imports and history). Columns default
-- to allow: call_blocking/sms_blocking 0 and no entries means every unknown
-- caller rings/delivers, so an upgraded gateway is inert until the owner
-- explicitly enables blocking.
CREATE TABLE IF NOT EXISTS harassment_settings (
  key_id TEXT PRIMARY KEY,
  call_blocking INTEGER NOT NULL DEFAULT 0,
  sms_blocking INTEGER NOT NULL DEFAULT 0,
  -- Treat this key's gateway-synced contacts as trusted, AFTER explicit
  -- owner sender blocks and BEFORE keyword/regex rules. Default off: no
  -- contact is implicitly trusted until the owner opts in.
  contact_whitelist INTEGER NOT NULL DEFAULT 0,
  version INTEGER NOT NULL DEFAULT 1,
  updated_at INTEGER NOT NULL DEFAULT 0
);

-- One number entry per row. match_kind 'exact' compares canonical phone
-- spellings; 'prefix' is an explicit digit prefix chosen by the owner.
-- mode 'trust' always wins over 'block'. action 'reject' suppresses the
-- call/SMS for this key; 'label' only marks it (imported historical lists
-- default to 'label' so a bulk import can never silently start rejecting).
CREATE TABLE IF NOT EXISTS harassment_entries (
  id TEXT PRIMARY KEY,
  key_id TEXT NOT NULL,
  number TEXT NOT NULL,
  match_kind TEXT NOT NULL DEFAULT 'exact',
  mode TEXT NOT NULL DEFAULT 'block',
  action TEXT NOT NULL DEFAULT 'reject',
  source TEXT NOT NULL DEFAULT 'manual',
  list_id TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_harassment_entries_key ON harassment_entries (key_id, enabled);

-- Imported number lists (one explicit, user-triggered import per list). The
-- default action is 'label' unless the owner deliberately sets 'reject'.
CREATE TABLE IF NOT EXISTS harassment_lists (
  id TEXT PRIMARY KEY,
  key_id TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  default_action TEXT NOT NULL DEFAULT 'label',
  source TEXT NOT NULL DEFAULT 'import',
  entry_count INTEGER NOT NULL DEFAULT 0,
  imported_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_harassment_lists_key ON harassment_lists (key_id, imported_at DESC);

-- Block history: one row per suppressed inbound call/SMS per key. Purely
-- informational (the raw SMS/call data stays untouched in the worker), and
-- key-scoped so two keys sharing a line never see each other's decisions.
CREATE TABLE IF NOT EXISTS harassment_events (
  id TEXT PRIMARY KEY,
  key_id TEXT NOT NULL,
  line_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL,
  peer TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  reason TEXT NOT NULL DEFAULT '',
  ref_id TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_harassment_events_key ON harassment_events (key_id, created_at DESC);

-- Per-enrollment-key SMS overlay. The worker databases stay the raw shared
-- source (bodies, timestamps, send status); this overlay is the ONLY place a
-- key's own folder/read/delete decisions live, so restore/read/delete by one
-- key never rewrites worker history or another key's view. folder values:
-- 'normal' (default), 'junk' (explicit or policy-classified), 'restored'
-- (owner marked the sender known -> always normal). hidden + delete_horizon
-- hide a conversation from THIS key only; messages newer than the horizon
-- reopen it. read_horizon is the per-key read watermark used for unread.
CREATE TABLE IF NOT EXISTS sms_key_threads (
  key_id TEXT NOT NULL,
  line_id TEXT NOT NULL,
  thread_key TEXT NOT NULL,
  folder TEXT NOT NULL DEFAULT 'normal',
  hidden INTEGER NOT NULL DEFAULT 0,
  delete_horizon INTEGER NOT NULL DEFAULT 0,
  read_horizon INTEGER NOT NULL DEFAULT 0,
  updated_by TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (key_id, line_id, thread_key)
);
CREATE INDEX IF NOT EXISTS idx_sms_key_threads_key ON sms_key_threads (key_id, line_id);

-- Message -> conversation index. The worker's message id alone does not tell
-- the unified service which thread a read request belongs to; this tiny index
-- is filled from the live event stream (never from a scan) so per-key read
-- state can advance the right thread watermark.
CREATE TABLE IF NOT EXISTS sms_message_threads (
  line_id TEXT NOT NULL,
  message_id TEXT NOT NULL,
  thread_key TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (line_id, message_id)
);

-- Per-key per-message read state. Keyed by the global line:message id so a
-- message read by this key is still unread for every other key.
CREATE TABLE IF NOT EXISTS sms_key_messages (
  key_id TEXT NOT NULL,
  message_id TEXT NOT NULL,
  thread_key TEXT NOT NULL DEFAULT '',
  read_at INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (key_id, message_id)
);

CREATE INDEX IF NOT EXISTS idx_calls_line_state ON calls (line_id, state);
CREATE INDEX IF NOT EXISTS idx_calls_device ON calls (device_id);
CREATE INDEX IF NOT EXISTS idx_events_seq ON events (seq);
CREATE INDEX IF NOT EXISTS idx_device_tokens_device ON device_tokens (device_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_devices_installation ON devices (key_id, public_key) WHERE revoked_at IS NULL;

-- Private per-line SIM/own-number identity. Raw phone numbers, IMSI/ICCID
-- hashes and precise cell identifiers stay here (mode 600 sqlite). Full phone
-- numbers are exposed only to authenticated, line-authorized API/console views.
CREATE TABLE IF NOT EXISTS line_identities (
  line_id TEXT PRIMARY KEY,
  phone_number TEXT NOT NULL DEFAULT '',
  number_source TEXT NOT NULL DEFAULT 'none',
  manual_number TEXT NOT NULL DEFAULT '',
  sim_hash TEXT NOT NULL DEFAULT '',
  imsi_hash TEXT NOT NULL DEFAULT '',
  iccid_masked TEXT NOT NULL DEFAULT '',
  operator_alpha TEXT NOT NULL DEFAULT '',
  operator_numeric TEXT NOT NULL DEFAULT '',
  registration TEXT NOT NULL DEFAULT '',
  access_tech TEXT NOT NULL DEFAULT '',
  cell_id TEXT NOT NULL DEFAULT '',
  lac TEXT NOT NULL DEFAULT '',
  tac TEXT NOT NULL DEFAULT '',
  pci INTEGER NOT NULL DEFAULT 0,
  earfcn INTEGER NOT NULL DEFAULT 0,
  commands_json TEXT NOT NULL DEFAULT '{}',
  updated_at INTEGER NOT NULL DEFAULT 0
);

-- Durable alert state: one key/value row per derived signal (backup-network
-- availability, open incident) so one fault produces one alert even across
-- daemon restarts. Values are small JSON documents owned by the notifier.
CREATE TABLE IF NOT EXISTS alert_state (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);

-- Reserved outbound alerts. A row is inserted exactly once per
-- (incident, phase) before any APNs call: the primary key is the dedupe key,
-- so a crash between reservation and delivery can retry but never duplicate.
-- delivered_at is set once at least one target accepted the push.
CREATE TABLE IF NOT EXISTS alert_events (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL DEFAULT '',
  line_id TEXT NOT NULL DEFAULT '',
  incident_id TEXT NOT NULL DEFAULT '',
  phase TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_attempt_at INTEGER,
  delivered_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_alert_events_pending ON alert_events (delivered_at, attempts, created_at);

-- Per-target delivery ledger for alerts. One device succeeding must not mark
-- the whole alert delivered: every (event, device/subscription) pair keeps its
-- own delivered_at so failed targets are retried without re-sending to the
-- ones that already accepted.
CREATE TABLE IF NOT EXISTS alert_deliveries (
  event_id TEXT NOT NULL,
  target_key TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_attempt_at INTEGER,
  delivered_at INTEGER,
  PRIMARY KEY (event_id, target_key)
);

-- Stable contact principal. A pairing key maps to exactly one principal;
-- explicit key renewal re-points the mapping so contacts survive rotation
-- without ever merging two unrelated keys that happen to hold the same
-- phone numbers.
CREATE TABLE IF NOT EXISTS principals (
  id TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS principal_keys (
  key_id TEXT PRIMARY KEY,
  principal_id TEXT NOT NULL,
  bound_at INTEGER NOT NULL,
  rotated_at INTEGER
);

-- Contacts are owned by a principal and queried only through it. The
-- server-side scope is the query predicate; there is no client-supplied
-- owner field. revision is the per-principal monotonic change counter used
-- by bounded incremental sync.
CREATE TABLE IF NOT EXISTS contacts (
  id TEXT PRIMARY KEY,
  principal_id TEXT NOT NULL,
  display_name TEXT NOT NULL DEFAULT '',
  given_name TEXT NOT NULL DEFAULT '',
  family_name TEXT NOT NULL DEFAULT '',
  organization TEXT NOT NULL DEFAULT '',
  nickname TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  search_key TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT 'web',
  client_ref TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL DEFAULT 1,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  deleted_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_contacts_principal ON contacts (principal_id, deleted_at, updated_at DESC);

CREATE TABLE IF NOT EXISTS contact_fields (
  contact_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  ordinal INTEGER NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  value TEXT NOT NULL DEFAULT '',
  normalized TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (contact_id, kind, ordinal)
);

CREATE TABLE IF NOT EXISTS contact_sync_state (
  principal_id TEXT PRIMARY KEY,
  revision INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);
`

type Store struct {
	DB *sql.DB
	// Secrets seals/opens operator-recoverable enrollment secrets. It may be
	// nil (tests, or a deployment without a master key); authentication never
	// depends on it.
	Secrets *SecretBox
}

func Open(path string) (*Store, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	return &Store{DB: database}, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.DB.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply unified schema: %w", err)
	}
	// Additive migration for databases created before enrollment_keys gained
	// the explicit manage-number capability. Default 0 keeps every existing
	// key (including shared all-lines keys) without it until an operator
	// grants it.
	if err := s.addColumnIfMissing(ctx, "enrollment_keys", "manage_number",
		"ALTER TABLE enrollment_keys ADD COLUMN manage_number INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	for _, column := range []string{"delete_through_id", "read_through_id"} {
		if err := s.addColumnIfMissing(ctx, "sms_key_threads", column,
			"ALTER TABLE sms_key_threads ADD COLUMN "+column+" TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS sms_thread_actions (
      key_id TEXT NOT NULL, operation_id TEXT NOT NULL, payload TEXT NOT NULL,
      created_at INTEGER NOT NULL, PRIMARY KEY(key_id, operation_id))`); err != nil {
		return err
	}
	// Additive migration for databases created before recoverable secret
	// storage. Existing keys stay NULL (honest cannot-recover) and keep
	// authenticating; only newly created keys can be revealed again.
	if err := s.addColumnIfMissing(ctx, "enrollment_keys", "secret_encrypted",
		"ALTER TABLE enrollment_keys ADD COLUMN secret_encrypted BLOB"); err != nil {
		return err
	}
	// Additive migrations for the standalone browser client: existing bind
	// codes and sessions stay scope 'push' (web-push management only) until
	// the owner explicitly mints a client-scoped code.
	if err := s.addColumnIfMissing(ctx, "webpush_bind_tokens", "scope",
		"ALTER TABLE webpush_bind_tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'push'"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, "webpush_sessions", "scope",
		"ALTER TABLE webpush_sessions ADD COLUMN scope TEXT NOT NULL DEFAULT 'push'"); err != nil {
		return err
	}
	// Additive migrations for databases created before manual call
	// recording existed. The recordings table itself is created by the
	// schema const above (CREATE TABLE IF NOT EXISTS); these columns carry
	// the live recording pointer/state on the call row so terminal clients
	// and the call list stay truthful across a restart.
	if err := s.addColumnIfMissing(ctx, "calls", "recording_id",
		"ALTER TABLE calls ADD COLUMN recording_id TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, "calls", "recording_state",
		"ALTER TABLE calls ADD COLUMN recording_state TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, "calls", "recording_duration_ms",
		"ALTER TABLE calls ADD COLUMN recording_duration_ms INTEGER"); err != nil {
		return err
	}
	// Key scope for recordings: a recording belongs to the enrollment key
	// that started it. Devices sharing the LINE through a different key must
	// never list, stream or export it; the column is the enforcement point.
	if err := s.addColumnIfMissing(ctx, "recordings", "key_id",
		"ALTER TABLE recordings ADD COLUMN key_id TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// Contact-whitelist switch for per-key harassment (build 55).
	if err := s.addColumnIfMissing(ctx, "harassment_settings", "contact_whitelist",
		"ALTER TABLE harassment_settings ADD COLUMN contact_whitelist INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Conference recording support (additive): pre-existing rows are 1:1
	// segments, so both defaults keep every old row truthful.
	if err := s.addColumnIfMissing(ctx, "recordings", "kind",
		"ALTER TABLE recordings ADD COLUMN kind TEXT NOT NULL DEFAULT 'call'"); err != nil {
		return err
	}
	if err := s.addColumnIfMissing(ctx, "recordings", "conference_id",
		"ALTER TABLE recordings ADD COLUMN conference_id TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	return nil
}

// addColumnIfMissing runs the DDL only when the table lacks the column. A
// fresh schema already includes it; older deployments need the ALTER once.
func (s *Store) addColumnIfMissing(ctx context.Context, table, column, ddl string) error {
	rows, err := s.DB.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return fmt.Errorf("inspect %s columns: %w", table, err)
	}
	present := false
	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			present = true
		}
	}
	rows.Close()
	if present {
		return nil
	}
	if _, err := s.DB.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("migrate %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) Close() error { return s.DB.Close() }

func nowMillis() int64 { return time.Now().UTC().UnixMilli() }

func millis(t time.Time) int64 { return t.UTC().UnixMilli() }

func fromMillis(value int64) time.Time { return time.UnixMilli(value).UTC() }

// ---------- lines ----------

type Line struct {
	ID                     string
	Name                   string
	WorkerURL              string
	TokenFile              string
	IdentityFile           string
	Enabled                bool
	SMSDryRun              bool
	RejectPolicy           string
	BlockedPrefixes        string
	AllowInbound           bool
	AllowOutbound          bool
	RecordingRetentionDays int
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

func (s *Store) EnsureLine(ctx context.Context, line Line) error {
	now := nowMillis()
	_, err := s.DB.ExecContext(ctx, `
INSERT INTO lines (id, name, worker_url, token_file, identity_file, enabled, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name = excluded.name, worker_url = excluded.worker_url,
  token_file = excluded.token_file, identity_file = excluded.identity_file, updated_at = excluded.updated_at`,
		line.ID, line.Name, line.WorkerURL, line.TokenFile, line.IdentityFile, boolInt(line.Enabled), now, now)
	return err
}

func (s *Store) ListLines(ctx context.Context) ([]Line, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, worker_url, token_file, identity_file, enabled, sms_dry_run,
		reject_policy, blocked_prefixes, allow_inbound, allow_outbound, recording_retention_days, created_at, updated_at
		FROM lines ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []Line
	for rows.Next() {
		line, scanErr := scanLine(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

func (s *Store) GetLine(ctx context.Context, id string) (Line, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT id, name, worker_url, token_file, identity_file, enabled, sms_dry_run,
		reject_policy, blocked_prefixes, allow_inbound, allow_outbound, recording_retention_days, created_at, updated_at
		FROM lines WHERE id = ?`, id)
	return scanLine(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanLine(row rowScanner) (Line, error) {
	var (
		line                               Line
		enabled, dryRun, allowIn, allowOut int
		created, updated                   int64
	)
	err := row.Scan(&line.ID, &line.Name, &line.WorkerURL, &line.TokenFile, &line.IdentityFile, &enabled, &dryRun,
		&line.RejectPolicy, &line.BlockedPrefixes, &allowIn, &allowOut, &line.RecordingRetentionDays, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Line{}, ErrNotFound
	}
	if err != nil {
		return Line{}, err
	}
	line.Enabled = enabled != 0
	line.SMSDryRun = dryRun != 0
	line.AllowInbound = allowIn != 0
	line.AllowOutbound = allowOut != 0
	line.CreatedAt = fromMillis(created)
	line.UpdatedAt = fromMillis(updated)
	return line, nil
}

type LineUpdate struct {
	Name                   *string
	Enabled                *bool
	SMSDryRun              *bool
	RejectPolicy           *string
	BlockedPrefixes        *string
	AllowInbound           *bool
	AllowOutbound          *bool
	RecordingRetentionDays *int
}

func (s *Store) UpdateLine(ctx context.Context, id string, update LineUpdate) error {
	sets := []string{"updated_at = ?"}
	args := []any{nowMillis()}
	add := func(column string, value any) {
		sets = append(sets, column+" = ?")
		args = append(args, value)
	}
	if update.Name != nil {
		add("name", strings.TrimSpace(*update.Name))
	}
	if update.Enabled != nil {
		add("enabled", boolInt(*update.Enabled))
	}
	if update.SMSDryRun != nil {
		add("sms_dry_run", boolInt(*update.SMSDryRun))
	}
	if update.RejectPolicy != nil {
		add("reject_policy", strings.TrimSpace(*update.RejectPolicy))
	}
	if update.BlockedPrefixes != nil {
		add("blocked_prefixes", strings.TrimSpace(*update.BlockedPrefixes))
	}
	if update.AllowInbound != nil {
		add("allow_inbound", boolInt(*update.AllowInbound))
	}
	if update.AllowOutbound != nil {
		add("allow_outbound", boolInt(*update.AllowOutbound))
	}
	if update.RecordingRetentionDays != nil {
		add("recording_retention_days", *update.RecordingRetentionDays)
	}
	args = append(args, id)
	_, err := s.DB.ExecContext(ctx, "UPDATE lines SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
}

// ---------- private SIM identity ----------

// LineIdentity is the private, never-public per-line own-number/SIM record.
// PhoneNumber is exposed only to authenticated, line-authorized views;
// anonymous views omit it. ManualNumber (operator override) always wins.
type LineIdentity struct {
	LineID          string
	PhoneNumber     string
	NumberSource    string
	ManualNumber    string
	SIMHash         string
	IMSIHash        string
	ICCIDMasked     string
	OperatorAlpha   string
	OperatorNumeric string
	Registration    string
	AccessTech      string
	CellID          string
	LAC             string
	TAC             string
	PCI             int
	EARFCN          int
	Commands        map[string]string
	UpdatedAt       time.Time
}

func (s *Store) GetLineIdentity(ctx context.Context, lineID string) (LineIdentity, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT line_id, phone_number, number_source, manual_number, sim_hash,
		imsi_hash, iccid_masked, operator_alpha, operator_numeric, registration, access_tech,
		cell_id, lac, tac, pci, earfcn, commands_json, updated_at
		FROM line_identities WHERE line_id = ?`, lineID)
	var (
		identity LineIdentity
		commands string
		updated  int64
	)
	err := row.Scan(&identity.LineID, &identity.PhoneNumber, &identity.NumberSource, &identity.ManualNumber,
		&identity.SIMHash, &identity.IMSIHash, &identity.ICCIDMasked, &identity.OperatorAlpha,
		&identity.OperatorNumeric, &identity.Registration, &identity.AccessTech, &identity.CellID,
		&identity.LAC, &identity.TAC, &identity.PCI, &identity.EARFCN, &commands, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return LineIdentity{LineID: lineID, NumberSource: "none", Commands: map[string]string{}}, nil
	}
	if err != nil {
		return LineIdentity{}, err
	}
	identity.Commands = map[string]string{}
	if strings.TrimSpace(commands) != "" && commands != "null" {
		_ = json.Unmarshal([]byte(commands), &identity.Commands)
	}
	identity.UpdatedAt = fromMillis(updated)
	return identity, nil
}

func (s *Store) SaveLineIdentity(ctx context.Context, identity LineIdentity) error {
	commands := []byte("{}")
	if len(identity.Commands) > 0 {
		encoded, err := json.Marshal(identity.Commands)
		if err != nil {
			return err
		}
		commands = encoded
	}
	updated := millis(identity.UpdatedAt)
	if updated == 0 {
		updated = nowMillis()
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO line_identities
		(line_id, phone_number, number_source, manual_number, sim_hash, imsi_hash, iccid_masked,
		 operator_alpha, operator_numeric, registration, access_tech, cell_id, lac, tac, pci, earfcn,
		 commands_json, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(line_id) DO UPDATE SET phone_number=excluded.phone_number,
		  number_source=excluded.number_source, manual_number=excluded.manual_number,
		  sim_hash=excluded.sim_hash, imsi_hash=excluded.imsi_hash, iccid_masked=excluded.iccid_masked,
		  operator_alpha=excluded.operator_alpha, operator_numeric=excluded.operator_numeric,
		  registration=excluded.registration, access_tech=excluded.access_tech,
		  cell_id=excluded.cell_id, lac=excluded.lac, tac=excluded.tac, pci=excluded.pci,
		  earfcn=excluded.earfcn, commands_json=excluded.commands_json, updated_at=excluded.updated_at`,
		identity.LineID, identity.PhoneNumber, identity.NumberSource, identity.ManualNumber,
		identity.SIMHash, identity.IMSIHash, identity.ICCIDMasked, identity.OperatorAlpha,
		identity.OperatorNumeric, identity.Registration, identity.AccessTech, identity.CellID,
		identity.LAC, identity.TAC, identity.PCI, identity.EARFCN, string(commands), updated)
	return err
}

// ---------- settings ----------
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return value, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ---------- enrollment keys ----------

type EnrollmentKey struct {
	ID           string
	Name         string
	AllLines     bool
	ManageNumber bool
	CreatedAt    time.Time
	RevokedAt    *time.Time
}

// CreateEnrollmentKey persists a key by its authentication hash. When the
// store has a master key, the plaintext secret is also sealed into
// secret_encrypted in the same statement so the operator can reveal it later;
// without a master key the key is created hash-only (usable, irreversible).
// The plaintext is never written anywhere else.
func (s *Store) CreateEnrollmentKey(ctx context.Context, id, name string, secretHash []byte, secret string, allLines bool) error {
	var sealed []byte
	if s.SecretsEnabled() && strings.TrimSpace(secret) != "" {
		// A sealing failure must fail the creation, never silently create a
		// key the operator believes is recoverable.
		value, err := s.Secrets.Seal([]byte(secret))
		if err != nil {
			return err
		}
		sealed = value
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO enrollment_keys (id, name, secret_hash, secret_encrypted, all_lines, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, id, name, secretHash, sealed, boolInt(allLines), nowMillis())
	return err
}

// KeySecretStored reports whether a recoverable encrypted copy exists for the
// key, without decrypting it. It powers the console's masked/unrecoverable
// status without exposing secret bytes.
func (s *Store) KeySecretStored(ctx context.Context, id string) (bool, error) {
	var stored []byte
	err := s.DB.QueryRowContext(ctx,
		`SELECT secret_encrypted FROM enrollment_keys WHERE id = ?`, id).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return len(stored) > 0, nil
}

// EnrollmentKeySecret decrypts and returns the recoverable secret for a key.
// It returns ErrSecretNotStored for legacy hash-only keys and
// ErrSecretStorageUnavailable when no master key is configured.
func (s *Store) EnrollmentKeySecret(ctx context.Context, id string) (string, error) {
	if !s.SecretsEnabled() {
		return "", ErrSecretStorageUnavailable
	}
	var stored []byte
	err := s.DB.QueryRowContext(ctx,
		`SELECT secret_encrypted FROM enrollment_keys WHERE id = ?`, id).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if len(stored) == 0 {
		return "", ErrSecretNotStored
	}
	plaintext, err := s.Secrets.Open(stored)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// SetKeyManageNumber flips the explicit, off-by-default capability that lets
// devices enrolled under this key edit the own number of the lines they are
// already authorized to use. It never changes line grants themselves.
func (s *Store) SetKeyManageNumber(ctx context.Context, id string, allowed bool) error {
	result, err := s.DB.ExecContext(ctx,
		`UPDATE enrollment_keys SET manage_number = ? WHERE id = ? AND revoked_at IS NULL`,
		boolInt(allowed), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func scanEnrollmentKey(row rowScanner) (EnrollmentKey, []byte, error) {
	var (
		key                    EnrollmentKey
		hash                   []byte
		allLines, manageNumber int
		created                int64
		revokedAt              sql.NullInt64
	)
	if err := row.Scan(&key.ID, &key.Name, &hash, &allLines, &manageNumber, &created, &revokedAt); err != nil {
		return EnrollmentKey{}, nil, err
	}
	key.AllLines = allLines != 0
	key.ManageNumber = manageNumber != 0
	key.CreatedAt = fromMillis(created)
	if revokedAt.Valid {
		value := fromMillis(revokedAt.Int64)
		key.RevokedAt = &value
	}
	return key, hash, nil
}

func (s *Store) GetEnrollmentKey(ctx context.Context, id string) (EnrollmentKey, []byte, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT id, name, secret_hash, all_lines, manage_number, created_at, revoked_at
		FROM enrollment_keys WHERE id = ?`, id)
	key, hash, err := scanEnrollmentKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EnrollmentKey{}, nil, ErrNotFound
	}
	if err != nil {
		return EnrollmentKey{}, nil, err
	}
	return key, hash, nil
}

func (s *Store) ListEnrollmentKeys(ctx context.Context) ([]EnrollmentKey, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, name, secret_hash, all_lines, manage_number, created_at, revoked_at
		FROM enrollment_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []EnrollmentKey
	for rows.Next() {
		key, _, scanErr := scanEnrollmentKey(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (s *Store) SetEnrollmentKeyRevoked(ctx context.Context, id string, revoked bool) error {
	var value any
	if revoked {
		value = nowMillis()
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE enrollment_keys SET revoked_at = ? WHERE id = ?`, value, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRevokedKey permanently removes an already-revoked enrollment key:
// its recoverable secret copy, its authentication hash and every explicit
// line grant. Active keys are never deleted here (ErrKeyActive) — the caller
// must revoke first, which keeps the two-step safety property. Device rows,
// call/SMS history and the revoked-public-key block list are preserved, so a
// deleted key can neither authenticate nor resurrect the devices/tokens that
// belonged to it; leftover tokens are revoked defensively in the same
// transaction.
func (s *Store) DeleteRevokedKey(ctx context.Context, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var revokedAt sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT revoked_at FROM enrollment_keys WHERE id = ?`, id).Scan(&revokedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !revokedAt.Valid {
		return ErrKeyActive
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE device_tokens SET revoked_at = ? WHERE revoked_at IS NULL AND device_id IN
		 (SELECT id FROM devices WHERE key_id = ?)`, nowMillis(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE webpush_sessions SET revoked_at = ? WHERE revoked_at IS NULL AND device_id IN
		 (SELECT id FROM devices WHERE key_id = ?)`, nowMillis(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE webpush_subscriptions SET revoked_at = ? WHERE revoked_at IS NULL AND device_id IN
		 (SELECT id FROM devices WHERE key_id = ?)`, nowMillis(), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_lines WHERE key_id = ?`, id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM enrollment_keys WHERE id = ? AND revoked_at IS NOT NULL`, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrKeyActive
	}
	return tx.Commit()
}

type KeyLine struct {
	KeyID        string
	LineID       string
	ReceiveSMS   bool
	ReceiveCalls bool
	SendSMS      bool
	Dial         bool
}

func (s *Store) SetKeyLine(ctx context.Context, entry KeyLine) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO key_lines (key_id, line_id, receive_sms, receive_calls, send_sms, dial)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_id, line_id) DO UPDATE SET receive_sms = excluded.receive_sms,
		receive_calls = excluded.receive_calls, send_sms = excluded.send_sms, dial = excluded.dial`,
		entry.KeyID, entry.LineID, boolInt(entry.ReceiveSMS), boolInt(entry.ReceiveCalls), boolInt(entry.SendSMS), boolInt(entry.Dial))
	return err
}

func (s *Store) DeleteKeyLine(ctx context.Context, keyID, lineID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM key_lines WHERE key_id = ? AND line_id = ?`, keyID, lineID)
	return err
}

func (s *Store) KeyLines(ctx context.Context, keyID string) ([]KeyLine, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT key_id, line_id, receive_sms, receive_calls, send_sms, dial
		FROM key_lines WHERE key_id = ?`, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []KeyLine
	for rows.Next() {
		var (
			entry                  KeyLine
			sms, calls, send, dial int
		)
		if err := rows.Scan(&entry.KeyID, &entry.LineID, &sms, &calls, &send, &dial); err != nil {
			return nil, err
		}
		entry.ReceiveSMS = sms != 0
		entry.ReceiveCalls = calls != 0
		entry.SendSMS = send != 0
		entry.Dial = dial != 0
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// ---------- devices ----------

type Device struct {
	ID              string
	KeyID           string
	Name            string
	PublicKey       []byte
	DefaultLineID   string
	CreatedAt       time.Time
	RevokedAt       *time.Time
	APNSToken       string
	VoIPToken       string
	PushEnvironment string
	PushLocale      string
}

func (s *Store) CreateDevice(ctx context.Context, device Device) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO devices (id, key_id, name, public_key, default_line_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, device.ID, device.KeyID, device.Name, device.PublicKey, device.DefaultLineID, nowMillis())
	return err
}

func (s *Store) GetDevice(ctx context.Context, id string) (Device, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT id, key_id, name, public_key, default_line_id, created_at, revoked_at
		FROM devices WHERE id = ?`, id)
	return scanDevice(row)
}

func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, key_id, name, public_key, default_line_id, created_at, revoked_at
		FROM devices ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var devices []Device
	for rows.Next() {
		device, scanErr := scanDevice(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func scanDevice(row rowScanner) (Device, error) {
	var (
		device    Device
		created   int64
		revokedAt sql.NullInt64
	)
	err := row.Scan(&device.ID, &device.KeyID, &device.Name, &device.PublicKey, &device.DefaultLineID, &created, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, err
	}
	device.CreatedAt = fromMillis(created)
	if revokedAt.Valid {
		value := fromMillis(revokedAt.Int64)
		device.RevokedAt = &value
	}
	return device, nil
}

func (s *Store) SetDeviceRevoked(ctx context.Context, id string, revoked bool) error {
	var value any
	if revoked {
		value = nowMillis()
	}
	device, lookupErr := s.GetDevice(ctx, id)
	if lookupErr != nil {
		return lookupErr
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE devices SET revoked_at = ? WHERE id = ?`, value, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return ErrNotFound
	}
	if revoked {
		_, _ = s.DB.ExecContext(ctx, `UPDATE device_tokens SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL`, nowMillis(), id)
		// Revocation removes dispatch access: web sessions and browser
		// subscriptions die with the device so a de-authorized device can
		// never receive another web push.
		_, _ = s.DB.ExecContext(ctx, `UPDATE webpush_sessions SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL`, nowMillis(), id)
		_, _ = s.DB.ExecContext(ctx, `UPDATE webpush_subscriptions SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL`, nowMillis(), id)
		hash := sha256.Sum256(device.PublicKey)
		_, _ = s.DB.ExecContext(ctx, `INSERT OR REPLACE INTO revoked_public_keys (public_key_hash, device_id, revoked_at)
			VALUES (?, ?, ?)`, hash[:], id, nowMillis())
	}
	return nil
}

// IsPublicKeyRevoked reports whether this installation identity was
// explicitly revoked by the owner. A synced enrollment/recovery grant must
// not silently re-enroll a revoked installation; the owner has to clear the
// binding or enroll a fresh signing identity deliberately.
func (s *Store) IsPublicKeyRevoked(ctx context.Context, publicKey []byte) (bool, error) {
	if len(publicKey) == 0 {
		return false, nil
	}
	hash := sha256.Sum256(publicKey)
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM revoked_public_keys WHERE public_key_hash = ?`, hash[:]).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// ClearRevokedPublicKey is the explicit owner action that re-allows one
// installation identity to enroll again.
func (s *Store) ClearRevokedPublicKey(ctx context.Context, publicKey []byte) error {
	hash := sha256.Sum256(publicKey)
	_, err := s.DB.ExecContext(ctx, `DELETE FROM revoked_public_keys WHERE public_key_hash = ?`, hash[:])
	return err
}

func (s *Store) SetDeviceDefaultLine(ctx context.Context, id, lineID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE devices SET default_line_id = ? WHERE id = ?`, lineID, id)
	return err
}

type PushRegistration struct {
	APNSToken   string
	VoIPToken   string
	Environment string
	Locale      string
}

// DevicePush returns the stored push registration for one device (empty
// strings when never registered).
func (s *Store) DevicePush(ctx context.Context, id string) (PushRegistration, error) {
	var registration PushRegistration
	var updatedAt sql.NullInt64
	err := s.DB.QueryRowContext(ctx,
		`SELECT apns_token, voip_token, push_environment, push_locale, push_updated_at
		 FROM devices WHERE id = ?`, id).
		Scan(&registration.APNSToken, &registration.VoIPToken, &registration.Environment,
			&registration.Locale, &updatedAt)
	return registration, err
}

func (s *Store) SetDevicePush(ctx context.Context, id string, registration PushRegistration) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE devices SET apns_token = ?, voip_token = ?, push_environment = ?,
		push_locale = ?, push_updated_at = ? WHERE id = ?`,
		registration.APNSToken, registration.VoIPToken, registration.Environment, registration.Locale, nowMillis(), id)
	return err
}

type PushTarget struct {
	DeviceID    string
	APNSToken   string
	VoIPToken   string
	Environment string
	Locale      string
}

func (s *Store) ListPushTargets(ctx context.Context) ([]PushTarget, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id, d.apns_token, d.voip_token, d.push_environment, d.push_locale
		FROM devices d JOIN enrollment_keys k ON k.id = d.key_id
		WHERE d.revoked_at IS NULL AND k.revoked_at IS NULL AND (d.apns_token != '' OR d.voip_token != '')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []PushTarget
	for rows.Next() {
		var target PushTarget
		if err := rows.Scan(&target.DeviceID, &target.APNSToken, &target.VoIPToken, &target.Environment, &target.Locale); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// ---------- optional Bark bridge ----------

// DeviceBark is the per-device opt-in Bark configuration. DeviceKey is
// secret material read only by the notifier path; API views use KeyHint.
type DeviceBark struct {
	DeviceID     string
	Enabled      bool
	ServerURL    string
	DeviceKey    string
	KeyHint      string
	AllowPrivate bool
	UpdatedAt    time.Time
}

// BarkTarget is the notifier's routing view: only live (non-revoked) devices
// with the bridge enabled and complete server/key material.
type BarkTarget struct {
	DeviceID     string
	ServerURL    string
	DeviceKey    string
	AllowPrivate bool
	Locale       string
}

func (s *Store) GetDeviceBark(ctx context.Context, deviceID string) (DeviceBark, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT device_id, enabled, server_url, device_key, key_hint, allow_private, updated_at
		FROM device_bark WHERE device_id = ?`, deviceID)
	var (
		settings  DeviceBark
		enabled   int
		allowPriv int
		updated   int64
	)
	err := row.Scan(&settings.DeviceID, &enabled, &settings.ServerURL, &settings.DeviceKey,
		&settings.KeyHint, &allowPriv, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceBark{}, ErrNotFound
	}
	if err != nil {
		return DeviceBark{}, err
	}
	settings.Enabled = enabled != 0
	settings.AllowPrivate = allowPriv != 0
	settings.UpdatedAt = fromMillis(updated)
	return settings, nil
}

func (s *Store) SetDeviceBark(ctx context.Context, settings DeviceBark) error {
	enabled := 0
	if settings.Enabled {
		enabled = 1
	}
	allowPrivate := 0
	if settings.AllowPrivate {
		allowPrivate = 1
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO device_bark
		(device_id, enabled, server_url, device_key, key_hint, allow_private, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			enabled = excluded.enabled,
			server_url = excluded.server_url,
			device_key = excluded.device_key,
			key_hint = excluded.key_hint,
			allow_private = excluded.allow_private,
			updated_at = excluded.updated_at`,
		settings.DeviceID, enabled, settings.ServerURL, settings.DeviceKey,
		settings.KeyHint, allowPrivate, nowMillis())
	return err
}

func (s *Store) DeleteDeviceBark(ctx context.Context, deviceID string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM device_bark WHERE device_id = ?`, deviceID)
	return err
}

func (s *Store) ListBarkTargets(ctx context.Context) ([]BarkTarget, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id, b.server_url, b.device_key, b.allow_private, d.push_locale
		FROM device_bark b
		JOIN devices d ON d.id = b.device_id
		JOIN enrollment_keys k ON k.id = d.key_id
		WHERE b.enabled = 1 AND b.server_url != '' AND b.device_key != ''
			AND d.revoked_at IS NULL AND k.revoked_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []BarkTarget
	for rows.Next() {
		var (
			target    BarkTarget
			allowPriv int
		)
		if err := rows.Scan(&target.DeviceID, &target.ServerURL, &target.DeviceKey, &allowPriv, &target.Locale); err != nil {
			return nil, err
		}
		target.AllowPrivate = allowPriv != 0
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// ---------- self-hosted PWA Web Push ----------

// Notify modes: how one device receives incoming-call notifications.
const (
	NotifyModeNative = "native" // APNs/PushKit only (default)
	NotifyModeBoth   = "both"   // native + web push
	NotifyModeWeb    = "web"    // web push only
)

// WebPushSubscription is one browser's stored push subscription. Auth and
// P256dh are secret material read only by the outbound notifier.
type WebPushSubscription struct {
	ID         string
	DeviceID   string
	Endpoint   string
	Auth       string
	P256dh     string
	UserAgent  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	LastSeenAt time.Time
	RevokedAt  *time.Time
}

// WebPushTarget is the notifier's routing view: subscriptions of live
// (non-revoked) devices only.
type WebPushTarget struct {
	DeviceID string
	Locale   string
	SubID    string
	Endpoint string
	Auth     string
	P256dh   string
}

// UpsertWebPushSubscription inserts or refreshes a subscription. The id is
// derived from the endpoint so re-subscribing the same browser refreshes the
// same row instead of stacking duplicates.
func (s *Store) UpsertWebPushSubscription(ctx context.Context, sub WebPushSubscription) error {
	sum := sha256.Sum256([]byte(sub.Endpoint))
	id := hex.EncodeToString(sum[:8])
	now := nowMillis()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO webpush_subscriptions
		(id, device_id, endpoint, auth, p256dh, user_agent, created_at, updated_at, last_seen_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT(endpoint) DO UPDATE SET
			device_id = excluded.device_id,
			auth = excluded.auth,
			p256dh = excluded.p256dh,
			user_agent = excluded.user_agent,
			updated_at = excluded.updated_at,
			last_seen_at = excluded.last_seen_at,
			revoked_at = NULL`,
		id, sub.DeviceID, sub.Endpoint, sub.Auth, sub.P256dh, sub.UserAgent, now, now, now)
	return err
}

func scanWebPushSubscription(rows *sql.Rows) (WebPushSubscription, error) {
	var (
		sub      WebPushSubscription
		created  int64
		updated  int64
		lastSeen int64
		revoked  sql.NullInt64
	)
	if err := rows.Scan(&sub.ID, &sub.DeviceID, &sub.Endpoint, &sub.Auth, &sub.P256dh,
		&sub.UserAgent, &created, &updated, &lastSeen, &revoked); err != nil {
		return WebPushSubscription{}, err
	}
	sub.CreatedAt = fromMillis(created)
	sub.UpdatedAt = fromMillis(updated)
	sub.LastSeenAt = fromMillis(lastSeen)
	if revoked.Valid {
		t := fromMillis(revoked.Int64)
		sub.RevokedAt = &t
	}
	return sub, nil
}

const webpushSubscriptionColumns = `id, device_id, endpoint, auth, p256dh, user_agent, created_at, updated_at, last_seen_at, revoked_at`

// ListWebPushSubscriptions returns the active subscriptions of one device.
func (s *Store) ListWebPushSubscriptions(ctx context.Context, deviceID string) ([]WebPushSubscription, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+webpushSubscriptionColumns+`
		FROM webpush_subscriptions WHERE device_id = ? AND revoked_at IS NULL
		ORDER BY created_at`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []WebPushSubscription
	for rows.Next() {
		sub, err := scanWebPushSubscription(rows)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// CountWebPushSubscriptions returns the active subscription count of a device
// (status views never expose subscription secrets).
func (s *Store) CountWebPushSubscriptions(ctx context.Context, deviceID string) (int, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM webpush_subscriptions
		WHERE device_id = ? AND revoked_at IS NULL`, deviceID)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// DeleteWebPushSubscription removes one subscription, but only when it
// belongs to the requesting device.
func (s *Store) DeleteWebPushSubscription(ctx context.Context, deviceID, subID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE webpush_subscriptions SET revoked_at = ?
		WHERE id = ? AND device_id = ? AND revoked_at IS NULL`, nowMillis(), subID, deviceID)
	return err
}

// RevokeWebPushSubscriptionsForDevice removes dispatch access for every
// subscription of the device. Called together with session revocation on
// unpair / key deletion / owner revocation so a de-authorized device can
// never receive another ring.
func (s *Store) RevokeWebPushSubscriptionsForDevice(ctx context.Context, deviceID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE webpush_subscriptions SET revoked_at = ?
		WHERE device_id = ? AND revoked_at IS NULL`, nowMillis(), deviceID)
	return err
}

// ListWebPushTargets returns every active subscription joined to its live
// device, with the device's push locale for localized payloads.
func (s *Store) ListWebPushTargets(ctx context.Context) ([]WebPushTarget, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT d.id, d.push_locale, w.id, w.endpoint, w.auth, w.p256dh
		FROM webpush_subscriptions w
		JOIN devices d ON d.id = w.device_id
		JOIN enrollment_keys k ON k.id = d.key_id
		WHERE w.revoked_at IS NULL AND d.revoked_at IS NULL AND k.revoked_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []WebPushTarget
	for rows.Next() {
		var target WebPushTarget
		if err := rows.Scan(&target.DeviceID, &target.Locale, &target.SubID,
			&target.Endpoint, &target.Auth, &target.P256dh); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

// WebPushTokenStore abstracts the token persistence used by the webpush
// Manager so the manager stays testable against the real SQLite schema.
type WebPushTokenStore interface {
	SaveWebPushBindToken(ctx context.Context, deviceID, tokenHash, scope string, ttl time.Duration) error
	RedeemWebPushBindToken(ctx context.Context, tokenHash string) (string, string, error)
	MarkWebPushBindTokenUsed(ctx context.Context, tokenHash string) error
	SaveWebPushSession(ctx context.Context, deviceID, tokenHash, scope string, ttl time.Duration) error
	TouchWebPushSession(ctx context.Context, tokenHash string, ttl time.Duration) (string, string, error)
	RevokeWebPushSessions(ctx context.Context, deviceID string) error
	RevokeWebPushSession(ctx context.Context, tokenHash string) error
}

// SaveWebPushBindToken stores a fresh single-use bind token hash with its
// scope ("push" or "client").
func (s *Store) SaveWebPushBindToken(ctx context.Context, deviceID, tokenHash, scope string, ttl time.Duration) error {
	now := nowMillis()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO webpush_bind_tokens
		(token_hash, device_id, scope, created_at, expires_at, used_at) VALUES (?, ?, ?, ?, ?, NULL)`,
		tokenHash, deviceID, scope, now, now+ttl.Milliseconds())
	return err
}

// RedeemWebPushBindToken validates a bind token hash WITHOUT consuming it:
// the code stays reusable until ONE successful redemption (the iOS
// install-then-bind flow may need several presentments across browsers).
// Unknown, expired or already-used tokens all fail identically so
// presentment attacks learn nothing. The scope minted with the code is
// returned so the redeemed session inherits exactly it.
func (s *Store) RedeemWebPushBindToken(ctx context.Context, tokenHash string) (string, string, error) {
	now := nowMillis()
	row := s.DB.QueryRowContext(ctx, `SELECT device_id, scope FROM webpush_bind_tokens
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?`, tokenHash, now)
	var deviceID, scope string
	if err := row.Scan(&deviceID, &scope); err != nil {
		return "", "", ErrNotFound
	}
	return deviceID, scope, nil
}

// MarkWebPushBindTokenUsed consumes a bind token after a successful
// redemption. Best effort by the caller: an unconsumed marker only widens
// the redemption window to expiry.
func (s *Store) MarkWebPushBindTokenUsed(ctx context.Context, tokenHash string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE webpush_bind_tokens SET used_at = ?
		WHERE token_hash = ? AND used_at IS NULL`, nowMillis(), tokenHash)
	return err
}

// SaveWebPushSession stores a fresh web session hash with its scope.
func (s *Store) SaveWebPushSession(ctx context.Context, deviceID, tokenHash, scope string, ttl time.Duration) error {
	now := nowMillis()
	_, err := s.DB.ExecContext(ctx, `INSERT INTO webpush_sessions
		(token_hash, device_id, scope, created_at, last_seen_at, expires_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL)`, tokenHash, deviceID, scope, now, now, now+ttl.Milliseconds())
	return err
}

// TouchWebPushSession validates a session hash and slides its expiry forward.
// Unknown, expired or revoked sessions fail identically. The session's scope
// is returned so route wrappers can keep push-scoped sessions away from the
// call/SMS APIs.
func (s *Store) TouchWebPushSession(ctx context.Context, tokenHash string, ttl time.Duration) (string, string, error) {
	now := nowMillis()
	row := s.DB.QueryRowContext(ctx, `SELECT device_id, scope FROM webpush_sessions
		WHERE token_hash = ? AND revoked_at IS NULL AND expires_at > ?`, tokenHash, now)
	var deviceID, scope string
	if err := row.Scan(&deviceID, &scope); err != nil {
		return "", "", ErrNotFound
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE webpush_sessions SET last_seen_at = ?, expires_at = ?
		WHERE token_hash = ?`, now, now+ttl.Milliseconds(), tokenHash); err != nil {
		return "", "", err
	}
	return deviceID, scope, nil
}

// RevokeWebPushSessions revokes every session of the device.
func (s *Store) RevokeWebPushSessions(ctx context.Context, deviceID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE webpush_sessions SET revoked_at = ?
		WHERE device_id = ? AND revoked_at IS NULL`, nowMillis(), deviceID)
	return err
}

// RevokeWebPushSession revokes ONE browser session by its token hash. Other
// sessions of the same device (and every native device credential) are
// untouched — "unbind this browser" must never log anything else out.
func (s *Store) RevokeWebPushSession(ctx context.Context, tokenHash string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE webpush_sessions SET revoked_at = ?
		WHERE token_hash = ? AND revoked_at IS NULL`, nowMillis(), tokenHash)
	return err
}

// DeviceNotifyMode returns the device's notification mode (default native).
func (s *Store) DeviceNotifyMode(ctx context.Context, deviceID string) (string, error) {
	value, err := s.Setting(ctx, "device:"+deviceID+":notify_mode")
	if err != nil {
		return NotifyModeNative, nil
	}
	switch value {
	case NotifyModeNative, NotifyModeBoth, NotifyModeWeb:
		return value, nil
	default:
		return NotifyModeNative, nil
	}
}

// SetDeviceNotifyMode persists the notification mode for one device.
func (s *Store) SetDeviceNotifyMode(ctx context.Context, deviceID, mode string) error {
	switch mode {
	case NotifyModeNative, NotifyModeBoth, NotifyModeWeb:
	default:
		return fmt.Errorf("invalid notify mode %q", mode)
	}
	return s.SetSetting(ctx, "device:"+deviceID+":notify_mode", mode)
}

// ---------- effective access ----------

type LineAccess struct {
	Line        Line
	Permissions KeyLine
}

type Access struct {
	Device Device
	Key    EnrollmentKey
	Lines  []LineAccess
}

// DeviceAccess resolves the live permission set for a device. Revoked keys
// or devices return ErrNotFound, and every request path calls this instead
// of caching, so revocation narrows access immediately.
func (s *Store) DeviceAccess(ctx context.Context, deviceID string) (Access, error) {
	device, err := s.GetDevice(ctx, deviceID)
	if err != nil {
		return Access{}, err
	}
	if device.RevokedAt != nil {
		return Access{}, ErrNotFound
	}
	key, _, err := s.GetEnrollmentKey(ctx, device.KeyID)
	if err != nil {
		return Access{}, err
	}
	if key.RevokedAt != nil {
		return Access{}, ErrNotFound
	}
	lines, err := s.ListLines(ctx)
	if err != nil {
		return Access{}, err
	}
	overrides := map[string]KeyLine{}
	explicit, err := s.KeyLines(ctx, key.ID)
	if err != nil {
		return Access{}, err
	}
	for _, entry := range explicit {
		overrides[entry.LineID] = entry
	}
	access := Access{Device: device, Key: key}
	for _, line := range lines {
		granted := key.AllLines
		entry := KeyLine{KeyID: key.ID, LineID: line.ID, ReceiveSMS: true, ReceiveCalls: true, SendSMS: true, Dial: true}
		if override, ok := overrides[line.ID]; ok {
			granted = true
			entry = override
		}
		if !granted {
			continue
		}
		access.Lines = append(access.Lines, LineAccess{Line: line, Permissions: entry})
	}
	return access, nil
}

// ---------- device tokens ----------

func (s *Store) StoreDeviceToken(ctx context.Context, hash []byte, deviceID, kind string, expiresAt time.Time) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`, hash, deviceID, kind, nowMillis(), millis(expiresAt))
	return err
}

// TokenHashes carries the hashed material for a token pair so callers never
// need to persist raw secrets.
type TokenHashes struct {
	AccessHash       []byte
	RefreshHash      []byte
	AccessExpiresAt  time.Time
	RefreshExpiresAt time.Time
}

func insertTokenPair(ctx context.Context, tx *sql.Tx, deviceID string, hashes TokenHashes) error {
	now := nowMillis()
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at)
		VALUES (?, ?, 'access', ?, ?)`, hashes.AccessHash, deviceID, now, millis(hashes.AccessExpiresAt)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_tokens (token_hash, device_id, kind, created_at, expires_at)
		VALUES (?, ?, 'refresh', ?, ?)`, hashes.RefreshHash, deviceID, now, millis(hashes.RefreshExpiresAt)); err != nil {
		return err
	}
	return nil
}

// CreateDeviceWithTokens atomically resolves or creates the device for one
// installation (key + public key) and issues a token pair. Retrying enroll
// with the same installation identity resumes the same device instead of
// filling the console with duplicates, and key/installation revocation is
// checked inside the transaction so a concurrent revoke cannot be raced.
func (s *Store) CreateDeviceWithTokens(ctx context.Context, keyID, deviceName string, publicKey []byte, newDeviceID string, hashes TokenHashes) (string, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var keyRevoked sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT revoked_at FROM enrollment_keys WHERE id = ?`, keyID).Scan(&keyRevoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, ErrKeyRevoked
		}
		return "", false, err
	}
	if keyRevoked.Valid {
		return "", false, ErrKeyRevoked
	}
	publicKeyHash := sha256.Sum256(publicKey)
	var blocked int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM revoked_public_keys WHERE public_key_hash = ?`, publicKeyHash[:]).Scan(&blocked); err != nil {
		return "", false, err
	}
	if blocked > 0 {
		return "", false, ErrInstallationRevoked
	}

	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM devices WHERE key_id = ? AND public_key = ? AND revoked_at IS NULL LIMIT 1`, keyID, publicKey).Scan(&existingID)
	switch {
	case err == nil:
		if err := insertTokenPair(ctx, tx, existingID, hashes); err != nil {
			return "", false, err
		}
		if err := tx.Commit(); err != nil {
			return "", false, err
		}
		return existingID, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", false, err
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO devices (id, key_id, name, public_key, default_line_id, created_at)
		VALUES (?, ?, ?, ?, '', ?)`, newDeviceID, keyID, deviceName, publicKey, nowMillis())
	if err != nil {
		// A concurrent enroll for the same installation won the unique
		// index; resume that row instead of failing the retry.
		var racedID string
		if queryErr := tx.QueryRowContext(ctx, `SELECT id FROM devices WHERE key_id = ? AND public_key = ? AND revoked_at IS NULL LIMIT 1`, keyID, publicKey).Scan(&racedID); queryErr == nil {
			if insertErr := insertTokenPair(ctx, tx, racedID, hashes); insertErr != nil {
				return "", false, insertErr
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return "", false, commitErr
			}
			return racedID, false, nil
		}
		return "", false, err
	}
	if err := insertTokenPair(ctx, tx, newDeviceID, hashes); err != nil {
		return "", false, err
	}
	if err := tx.Commit(); err != nil {
		return "", false, err
	}
	return newDeviceID, true, nil
}

// RotateRefreshToken atomically consumes a single-use refresh token and
// issues the replacement pair. Exactly one concurrent refresh can win; the
// device and its key must still be live or the transaction rolls back and
// the presented token stays untouched.
func (s *Store) RotateRefreshToken(ctx context.Context, oldRefreshHash, accessHash, newRefreshHash []byte, accessExpiresAt, refreshExpiresAt time.Time) (string, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := nowMillis()
	result, err := tx.ExecContext(ctx, `UPDATE device_tokens SET revoked_at = ?
		WHERE token_hash = ? AND kind = 'refresh' AND revoked_at IS NULL AND expires_at > ?`, now, oldRefreshHash, now)
	if err != nil {
		return "", err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return "", ErrNotFound
	}
	var deviceID string
	if err := tx.QueryRowContext(ctx, `SELECT device_id FROM device_tokens WHERE token_hash = ?`, oldRefreshHash).Scan(&deviceID); err != nil {
		return "", err
	}
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices d JOIN enrollment_keys k ON k.id = d.key_id
		WHERE d.id = ? AND d.revoked_at IS NULL AND k.revoked_at IS NULL`, deviceID).Scan(&live); err != nil {
		return "", err
	}
	if live == 0 {
		return "", ErrNotFound
	}
	hashes := TokenHashes{AccessHash: accessHash, RefreshHash: newRefreshHash, AccessExpiresAt: accessExpiresAt, RefreshExpiresAt: refreshExpiresAt}
	if err := insertTokenPair(ctx, tx, deviceID, hashes); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return deviceID, nil
}

type TokenRecord struct {
	DeviceID  string
	Kind      string
	ExpiresAt time.Time
}

func (s *Store) LookupDeviceToken(ctx context.Context, hash []byte, kind string) (TokenRecord, error) {
	var (
		record    TokenRecord
		expiresAt int64
		revokedAt sql.NullInt64
	)
	err := s.DB.QueryRowContext(ctx, `SELECT device_id, kind, expires_at, revoked_at FROM device_tokens
		WHERE token_hash = ? AND kind = ?`, hash, kind).Scan(&record.DeviceID, &record.Kind, &expiresAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenRecord{}, ErrNotFound
	}
	if err != nil {
		return TokenRecord{}, err
	}
	if revokedAt.Valid || time.Now().UTC().After(fromMillis(expiresAt)) {
		return TokenRecord{}, ErrNotFound
	}
	record.ExpiresAt = fromMillis(expiresAt)
	return record, nil
}

func (s *Store) RevokeDeviceToken(ctx context.Context, hash []byte) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE device_tokens SET revoked_at = ? WHERE token_hash = ?`, nowMillis(), hash)
	return err
}

func (s *Store) RevokeTokensByDevice(ctx context.Context, deviceID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE device_tokens SET revoked_at = ? WHERE device_id = ?`, nowMillis(), deviceID)
	return err
}

func (s *Store) CountDevicesForKey(ctx context.Context, keyID string) (int, error) {
	var count int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE key_id = ? AND revoked_at IS NULL`, keyID).Scan(&count)
	return count, err
}

// ---------- calls ----------

type Call struct {
	ID           string
	LineID       string
	WorkerCallID string
	DeviceID     string
	Direction    string
	Peer         string
	State        string
	Held         bool
	ConferenceID string
	CreatedAt    time.Time
	AnsweredAt   *time.Time
	EndedAt      *time.Time
	EndReason    string
	DeclinedBy   string
	// Recording fields mirror the manual call-recording state owned by the
	// recording manager; they are written only through UpdateCallRecording
	// so ordinary call upserts never clobber them.
	RecordingID         string
	RecordingState      string
	RecordingDurationMs *int64
}

func (s *Store) UpsertCall(ctx context.Context, call Call) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO calls (id, line_id, worker_call_id, device_id, direction, peer, state,
		held, conference_id, created_at, answered_at, ended_at, end_reason, declined_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET device_id = excluded.device_id, direction = excluded.direction,
		peer = excluded.peer, state = excluded.state, held = excluded.held, conference_id = excluded.conference_id,
		answered_at = excluded.answered_at, ended_at = excluded.ended_at, end_reason = excluded.end_reason,
		declined_by = excluded.declined_by`,
		call.ID, call.LineID, call.WorkerCallID, call.DeviceID, call.Direction, call.Peer, call.State,
		boolInt(call.Held), call.ConferenceID, millis(call.CreatedAt), nullableMillis(call.AnsweredAt), nullableMillis(call.EndedAt),
		call.EndReason, call.DeclinedBy)
	return err
}

// ErrCallAlreadyEnded is returned by UpsertLiveCall when the row already
// carries terminal truth: the caller based its write on a stale snapshot
// (GetCall raced a terminal claim) and must re-read instead of overwriting.
var ErrCallAlreadyEnded = errors.New("call already ended")

// UpsertLiveCall inserts a new call or updates a still-live call. It is the
// only safe write for convergence paths that carry stale worker snapshots:
// the UPDATE never matches a terminal row (ended_at IS NOT NULL), so a
// snapshot read before another finisher's terminal claim can neither
// resurrect the call nor clear its ended_at/end_reason, and cannot move
// answered_at backwards (COALESCE keeps the earliest answer time). The
// device_id is also kept monotonic for the answering claim: a ringing
// worker snapshot with an empty device_id never clears the device that just
// claimed the call. A no-op terminal match returns ErrCallAlreadyEnded.
func (s *Store) UpsertLiveCall(ctx context.Context, call Call) error {
	result, err := s.DB.ExecContext(ctx, `INSERT INTO calls (id, line_id, worker_call_id, device_id, direction, peer, state,
		held, conference_id, created_at, answered_at, ended_at, end_reason, declined_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', ?)
		ON CONFLICT(id) DO UPDATE SET
			device_id = CASE WHEN calls.device_id = '' THEN excluded.device_id ELSE calls.device_id END,
			direction = excluded.direction,
			peer = excluded.peer,
			state = excluded.state,
			held = excluded.held,
			conference_id = excluded.conference_id,
			answered_at = COALESCE(calls.answered_at, excluded.answered_at),
			declined_by = excluded.declined_by
		WHERE calls.ended_at IS NULL`,
		call.ID, call.LineID, call.WorkerCallID, call.DeviceID, call.Direction, call.Peer, call.State,
		boolInt(call.Held), call.ConferenceID, millis(call.CreatedAt), nullableMillis(call.AnsweredAt), call.DeclinedBy)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrCallAlreadyEnded
	}
	return nil
}

func (s *Store) GetCall(ctx context.Context, id string) (Call, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT id, line_id, worker_call_id, device_id, direction, peer, state, held,
		conference_id, created_at, answered_at, ended_at, end_reason, declined_by,
		recording_id, recording_state, recording_duration_ms FROM calls WHERE id = ?`, id)
	return scanCall(row)
}

func (s *Store) GetCallByWorker(ctx context.Context, lineID, workerCallID string) (Call, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT id, line_id, worker_call_id, device_id, direction, peer, state, held,
		conference_id, created_at, answered_at, ended_at, end_reason, declined_by,
		recording_id, recording_state, recording_duration_ms FROM calls
		WHERE line_id = ? AND worker_call_id = ?`, lineID, workerCallID)
	return scanCall(row)
}

func (s *Store) ListCalls(ctx context.Context, lineID, deviceID string, activeOnly bool, limit int) ([]Call, error) {
	query := `SELECT id, line_id, worker_call_id, device_id, direction, peer, state, held, conference_id,
		created_at, answered_at, ended_at, end_reason, declined_by,
		recording_id, recording_state, recording_duration_ms FROM calls WHERE 1 = 1`
	args := []any{}
	if lineID != "" {
		query += " AND line_id = ?"
		args = append(args, lineID)
	}
	if deviceID != "" {
		query += " AND device_id = ?"
		args = append(args, deviceID)
	}
	if activeOnly {
		query += " AND ended_at IS NULL"
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []Call
	for rows.Next() {
		call, scanErr := scanCall(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		calls = append(calls, call)
	}
	return calls, rows.Err()
}

// TerminalCallIDs returns the set of call ids that have already ended. The
// durable event log replays from sequence zero for a fresh client, so a
// historical non-terminal call event (incoming/connecting/active) for one of
// these ids must never be replayed as a live call; terminal events stay
// deliverable so a client that saw the call can clear it.
func (s *Store) TerminalCallIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM calls WHERE ended_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

func scanCall(row rowScanner) (Call, error) {
	var (
		call           Call
		held           int
		created        int64
		answeredAt     sql.NullInt64
		endedAt        sql.NullInt64
		recordingDurMs sql.NullInt64
	)
	err := row.Scan(&call.ID, &call.LineID, &call.WorkerCallID, &call.DeviceID, &call.Direction, &call.Peer,
		&call.State, &held, &call.ConferenceID, &created, &answeredAt, &endedAt, &call.EndReason, &call.DeclinedBy,
		&call.RecordingID, &call.RecordingState, &recordingDurMs)
	if errors.Is(err, sql.ErrNoRows) {
		return Call{}, ErrNotFound
	}
	if err != nil {
		return Call{}, err
	}
	call.Held = held != 0
	call.CreatedAt = fromMillis(created)
	if answeredAt.Valid {
		value := fromMillis(answeredAt.Int64)
		call.AnsweredAt = &value
	}
	if endedAt.Valid {
		value := fromMillis(endedAt.Int64)
		call.EndedAt = &value
	}
	if recordingDurMs.Valid {
		value := recordingDurMs.Int64
		call.RecordingDurationMs = &value
	}
	return call, nil
}

// ClaimCall atomically assigns a ringing inbound call to the first device
// that answers. Exactly one concurrent answer can win.
// FinishCallIfOpen atomically claims the terminal transition: exactly one
// caller wins (rows affected == 1) so a call publishes exactly one
// call.ended no matter how many finishers raced (HTTP hangup path, worker
// event path, policy reject). The winner fills in the terminal columns and
// records answeredAt (monotonically — an earlier answer time is never
// overwritten) when it first arrives in the same snapshot; losers must
// treat the call as already ended.
// FinishCallIfOpen claims the ONE terminal transition for a live call. The
// end reason is MONOTONIC and never empty:
//   - a non-empty incoming reason wins (the first real finisher's reason);
//   - an empty incoming reason keeps whatever reason the row already has
//     (e.g. an HTTP hangup that landed first);
//   - if both are empty the neutral "hung_up" is recorded.
//
// A worker terminal snapshot that carries no reason (the observed live race:
// the worker's two-step hangup sequence can win against the unified HTTP
// hangup) must therefore never end a call with a blank reason — the invariant
// "an ended call always has a reason" holds for every finisher.
func (s *Store) FinishCallIfOpen(ctx context.Context, callID string, endedAt time.Time, endReason string, answeredAt *time.Time) (bool, error) {
	if strings.TrimSpace(endReason) == "" {
		endReason = "hung_up"
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE calls
		SET state = 'ended', ended_at = ?, end_reason = ?,
		    answered_at = COALESCE(calls.answered_at, ?)
		WHERE id = ? AND ended_at IS NULL`,
		endedAt.UTC().UnixMilli(), endReason, nullableMillis(answeredAt), callID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// FinishConferenceLeg claims the terminal transition for one conference leg
// while preserving its answered/ownership metadata: it only matches a live
// row, never clears answered_at and keeps the existing device owner. A
// worker snapshot or other finisher that raced the removal cannot resurrect
// the leg or strip its end reason.
func (s *Store) FinishConferenceLeg(ctx context.Context, callID string, endedAt time.Time, endReason string) (bool, error) {
	result, err := s.DB.ExecContext(ctx, `UPDATE calls
		SET state = 'ended', ended_at = ?, conference_id = '',
		    end_reason = CASE WHEN end_reason = '' THEN ? ELSE end_reason END
		WHERE id = ? AND ended_at IS NULL`,
		endedAt.UTC().UnixMilli(), endReason, callID)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

func (s *Store) ClaimCall(ctx context.Context, callID, deviceID string) (bool, error) {
	result, err := s.DB.ExecContext(ctx, `UPDATE calls SET state = 'answering', device_id = ?
		WHERE id = ? AND state = 'incoming_ringing' AND ended_at IS NULL`, deviceID, callID)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	return affected == 1, nil
}

// ReleaseCallClaim undoes a successful ClaimCall when the upstream Answer
// RPC failed: the call returns to incoming_ringing with no owner so other
// devices (and voicemail) can claim it again. The release only matches the
// claiming owner's still-answering row, so a racing terminal claim or a
// concurrent answer is never rewritten.
func (s *Store) ReleaseCallClaim(ctx context.Context, callID, deviceID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE calls SET state = 'incoming_ringing', device_id = ''
		WHERE id = ? AND device_id = ? AND state = 'answering' AND ended_at IS NULL`, callID, deviceID)
	return err
}

// RecordDecline appends a device to the local-decline set and returns the
// updated set. Declining locally never rejects the upstream call by itself.
func (s *Store) RecordDecline(ctx context.Context, callID, deviceID string) (string, error) {
	call, err := s.GetCall(ctx, callID)
	if err != nil {
		return "", err
	}
	declined := map[string]bool{}
	for _, existing := range strings.Split(call.DeclinedBy, ",") {
		if value := strings.TrimSpace(existing); value != "" {
			declined[value] = true
		}
	}
	declined[deviceID] = true
	values := make([]string, 0, len(declined))
	for value := range declined {
		values = append(values, value)
	}
	joined := strings.Join(values, ",")
	if _, err := s.DB.ExecContext(ctx, `UPDATE calls SET declined_by = ? WHERE id = ?`, joined, callID); err != nil {
		return "", err
	}
	return joined, nil
}

func (s *Store) CountLiveCalls(ctx context.Context) (int, error) {
	var count int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM calls WHERE ended_at IS NULL AND state NOT IN ('ended')`).Scan(&count)
	return count, err
}

func (s *Store) CountLiveCallsOnLine(ctx context.Context, lineID string) (int, error) {
	var count int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM calls WHERE line_id = ? AND ended_at IS NULL AND state NOT IN ('ended')`, lineID).Scan(&count)
	return count, err
}

// ---------- conferences ----------

type Conference struct {
	ID            string
	HostDeviceID  string
	State         string
	CreatedAt     time.Time
	EndedAt       *time.Time
	GraceDeadline *time.Time
}

type ConferenceLeg struct {
	ConferenceID string
	CallID       string
	Held         bool
	JoinedAt     time.Time
	RemovedAt    *time.Time
}

func (s *Store) CreateConference(ctx context.Context, conference Conference) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO conferences (id, host_device_id, state, created_at, grace_deadline)
		VALUES (?, ?, ?, ?, ?)`, conference.ID, conference.HostDeviceID, conference.State,
		millis(conference.CreatedAt), nullableMillis(conference.GraceDeadline))
	return err
}

func (s *Store) GetConference(ctx context.Context, id string) (Conference, error) {
	var (
		conference Conference
		created    int64
		endedAt    sql.NullInt64
		deadline   sql.NullInt64
	)
	err := s.DB.QueryRowContext(ctx, `SELECT id, host_device_id, state, created_at, ended_at, grace_deadline
		FROM conferences WHERE id = ?`, id).Scan(&conference.ID, &conference.HostDeviceID, &conference.State,
		&created, &endedAt, &deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return Conference{}, ErrNotFound
	}
	if err != nil {
		return Conference{}, err
	}
	conference.CreatedAt = fromMillis(created)
	if endedAt.Valid {
		value := fromMillis(endedAt.Int64)
		conference.EndedAt = &value
	}
	if deadline.Valid {
		value := fromMillis(deadline.Int64)
		conference.GraceDeadline = &value
	}
	return conference, nil
}

func (s *Store) UpdateConference(ctx context.Context, conference Conference) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE conferences SET state = ?, ended_at = ?, grace_deadline = ? WHERE id = ?`,
		conference.State, nullableMillis(conference.EndedAt), nullableMillis(conference.GraceDeadline), conference.ID)
	return err
}

func (s *Store) AddConferenceLeg(ctx context.Context, leg ConferenceLeg) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO conference_legs (conference_id, call_id, held, joined_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(conference_id, call_id) DO UPDATE SET held = excluded.held, removed_at = NULL`,
		leg.ConferenceID, leg.CallID, boolInt(leg.Held), millis(leg.JoinedAt))
	return err
}

func (s *Store) RemoveConferenceLeg(ctx context.Context, conferenceID, callID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE conference_legs SET removed_at = ? WHERE conference_id = ? AND call_id = ?`,
		nowMillis(), conferenceID, callID)
	return err
}

func (s *Store) SetConferenceLegHeld(ctx context.Context, conferenceID, callID string, held bool) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE conference_legs SET held = ? WHERE conference_id = ? AND call_id = ?`,
		boolInt(held), conferenceID, callID)
	return err
}

func (s *Store) ListConferences(ctx context.Context, activeOnly bool, limit int) ([]Conference, error) {
	query := `SELECT id, host_device_id, state, created_at, ended_at, grace_deadline FROM conferences`
	if activeOnly {
		query += " WHERE ended_at IS NULL"
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	rows, err := s.DB.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var conferences []Conference
	for rows.Next() {
		var (
			conference Conference
			created    int64
			endedAt    sql.NullInt64
			deadline   sql.NullInt64
		)
		if err := rows.Scan(&conference.ID, &conference.HostDeviceID, &conference.State, &created, &endedAt, &deadline); err != nil {
			return nil, err
		}
		conference.CreatedAt = fromMillis(created)
		if endedAt.Valid {
			value := fromMillis(endedAt.Int64)
			conference.EndedAt = &value
		}
		if deadline.Valid {
			value := fromMillis(deadline.Int64)
			conference.GraceDeadline = &value
		}
		conferences = append(conferences, conference)
	}
	return conferences, rows.Err()
}

func (s *Store) ListConferenceLegs(ctx context.Context, conferenceID string) ([]ConferenceLeg, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT conference_id, call_id, held, joined_at, removed_at
		FROM conference_legs WHERE conference_id = ? ORDER BY joined_at`, conferenceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var legs []ConferenceLeg
	for rows.Next() {
		var (
			leg       ConferenceLeg
			held      int
			joined    int64
			removedAt sql.NullInt64
		)
		if err := rows.Scan(&leg.ConferenceID, &leg.CallID, &held, &joined, &removedAt); err != nil {
			return nil, err
		}
		leg.Held = held != 0
		leg.JoinedAt = fromMillis(joined)
		if removedAt.Valid {
			value := fromMillis(removedAt.Int64)
			leg.RemovedAt = &value
		}
		legs = append(legs, leg)
	}
	return legs, rows.Err()
}

// ---------- events ----------

type Event struct {
	Seq       int64
	ID        string
	LineID    string
	DeviceID  string
	Type      string
	Payload   []byte
	CreatedAt time.Time
}

func (s *Store) InsertEvent(ctx context.Context, event Event) (int64, error) {
	result, err := s.DB.ExecContext(ctx, `INSERT INTO events (id, line_id, device_id, type, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, event.ID, event.LineID, event.DeviceID, event.Type, string(event.Payload), millis(event.CreatedAt))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) EventsAfter(ctx context.Context, after int64, limit int) ([]Event, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT seq, id, line_id, device_id, type, payload, created_at
		FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		var (
			event     Event
			payload   string
			createdAt int64
		)
		if err := rows.Scan(&event.Seq, &event.ID, &event.LineID, &event.DeviceID, &event.Type, &payload, &createdAt); err != nil {
			return nil, err
		}
		event.Payload = []byte(payload)
		event.CreatedAt = fromMillis(createdAt)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) LatestEventSeq(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT MAX(seq) FROM events`).Scan(&seq)
	if err != nil {
		return 0, err
	}
	return seq.Int64, nil
}

// ---------- console sessions ----------

type ConsoleSession struct {
	TokenHash  []byte
	CSRF       string
	Username   string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RemoteAddr string
}

func (s *Store) CreateConsoleSession(ctx context.Context, session ConsoleSession) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO console_sessions (token_hash, csrf, username, created_at, expires_at, remote_addr)
		VALUES (?, ?, ?, ?, ?, ?)`, session.TokenHash, session.CSRF, session.Username,
		millis(session.CreatedAt), millis(session.ExpiresAt), session.RemoteAddr)
	return err
}

func (s *Store) LookupConsoleSession(ctx context.Context, tokenHash []byte) (ConsoleSession, error) {
	var (
		session   ConsoleSession
		created   int64
		expiresAt int64
	)
	err := s.DB.QueryRowContext(ctx, `SELECT token_hash, csrf, username, created_at, expires_at, remote_addr
		FROM console_sessions WHERE token_hash = ?`, tokenHash).Scan(&session.TokenHash, &session.CSRF,
		&session.Username, &created, &expiresAt, &session.RemoteAddr)
	if errors.Is(err, sql.ErrNoRows) {
		return ConsoleSession{}, ErrNotFound
	}
	if err != nil {
		return ConsoleSession{}, err
	}
	session.CreatedAt = fromMillis(created)
	session.ExpiresAt = fromMillis(expiresAt)
	if time.Now().UTC().After(session.ExpiresAt) {
		return ConsoleSession{}, ErrNotFound
	}
	return session, nil
}

func (s *Store) DeleteConsoleSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM console_sessions WHERE token_hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteExpiredConsoleSessions(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM console_sessions WHERE expires_at < ?`, nowMillis())
	return err
}

// ---------- audit ----------

func (s *Store) AppendAudit(ctx context.Context, actor, action, target, result string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO audit_log (actor, action, target, result, created_at)
		VALUES (?, ?, ?, ?, ?)`, actor, action, target, result, nowMillis())
	return err
}

type AuditEntry struct {
	ID        int64
	Actor     string
	Action    string
	Target    string
	Result    string
	CreatedAt time.Time
}

func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, actor, action, target, result, created_at FROM audit_log
		ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []AuditEntry
	for rows.Next() {
		var (
			entry     AuditEntry
			createdAt int64
		)
		if err := rows.Scan(&entry.ID, &entry.Actor, &entry.Action, &entry.Target, &entry.Result, &createdAt); err != nil {
			return nil, err
		}
		entry.CreatedAt = fromMillis(createdAt)
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// ---------- idempotency ----------

type IdempotencyRecord struct {
	StatusCode int
	Response   []byte
	Complete   bool
}

func (s *Store) ClaimIdempotency(ctx context.Context, deviceID, operation, key string) (bool, IdempotencyRecord, error) {
	result, err := s.DB.ExecContext(ctx, `INSERT OR IGNORE INTO idempotency (device_id, operation, request_key, created_at)
		VALUES (?, ?, ?, ?)`, deviceID, operation, key, nowMillis())
	if err != nil {
		return false, IdempotencyRecord{}, err
	}
	affected, _ := result.RowsAffected()
	if affected == 1 {
		return true, IdempotencyRecord{}, nil
	}
	var (
		status  int
		payload sql.NullString
	)
	err = s.DB.QueryRowContext(ctx, `SELECT status_code, response_json FROM idempotency
		WHERE device_id = ? AND operation = ? AND request_key = ?`, deviceID, operation, key).Scan(&status, &payload)
	if err != nil {
		return false, IdempotencyRecord{}, err
	}
	record := IdempotencyRecord{StatusCode: status, Complete: status != 0 || payload.Valid}
	if payload.Valid {
		record.Response = []byte(payload.String)
	}
	return false, record, nil
}

func (s *Store) CompleteIdempotency(ctx context.Context, deviceID, operation, key string, status int, response []byte) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE idempotency SET status_code = ?, response_json = ?
		WHERE device_id = ? AND operation = ? AND request_key = ?`, status, string(response), deviceID, operation, key)
	return err
}

// ---------- voicemail ----------

type Voicemail struct {
	ID         string
	LineID     string
	CallID     string
	Peer       string
	State      string
	Path       string
	DurationMs int64
	SizeBytes  int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	DeletedAt  *time.Time
}

// ClaimCallForVoicemail atomically moves a ringing inbound call into
// voicemail ownership. Exactly one of device-answer and voicemail can win.
func (s *Store) ClaimCallForVoicemail(ctx context.Context, callID string) (bool, error) {
	result, err := s.DB.ExecContext(ctx, `UPDATE calls SET state = 'voicemail_answering', device_id = ''
		WHERE id = ? AND state = 'incoming_ringing' AND ended_at IS NULL`, callID)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	return affected == 1, nil
}

// RevertVoicemailClaim returns a voicemail-answering call to ringing only
// while it is still owned by voicemail; an ended or device-answered call is
// never resurrected.
func (s *Store) RevertVoicemailClaim(ctx context.Context, callID string) (bool, error) {
	result, err := s.DB.ExecContext(ctx, `UPDATE calls SET state = 'incoming_ringing', device_id = ''
		WHERE id = ? AND state = 'voicemail_answering' AND ended_at IS NULL`, callID)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	return affected == 1, nil
}

// MarkVoicemailRecording promotes an answering call to recording only while
// it is still owned by voicemail and alive.
func (s *Store) MarkVoicemailRecording(ctx context.Context, callID string, answeredAt time.Time) (bool, error) {
	result, err := s.DB.ExecContext(ctx, `UPDATE calls SET state = 'voicemail_recording', device_id = '', answered_at = ?
		WHERE id = ? AND state = 'voicemail_answering' AND ended_at IS NULL`, millis(answeredAt), callID)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	return affected == 1, nil
}

func (s *Store) InsertVoicemail(ctx context.Context, voicemail Voicemail) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO voicemails (id, line_id, call_id, peer, state, path,
		duration_ms, size_bytes, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		voicemail.ID, voicemail.LineID, voicemail.CallID, voicemail.Peer, voicemail.State, voicemail.Path,
		voicemail.DurationMs, voicemail.SizeBytes, millis(voicemail.CreatedAt), millis(voicemail.ExpiresAt))
	return err
}

func (s *Store) FinalizeVoicemail(ctx context.Context, id, state, path string, durationMs, sizeBytes int64, expiresAt time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE voicemails SET state = ?, path = ?, duration_ms = ?, size_bytes = ?, expires_at = ?
		WHERE id = ?`, state, path, durationMs, sizeBytes, millis(expiresAt), id)
	return err
}

func (s *Store) GetVoicemail(ctx context.Context, id string) (Voicemail, error) {
	row := s.DB.QueryRowContext(ctx, `SELECT id, line_id, call_id, peer, state, path, duration_ms, size_bytes,
		created_at, expires_at, deleted_at FROM voicemails WHERE id = ?`, id)
	return scanVoicemail(row)
}

func (s *Store) ListVoicemails(ctx context.Context, lineID string, limit int) ([]Voicemail, error) {
	query := `SELECT id, line_id, call_id, peer, state, path, duration_ms, size_bytes, created_at, expires_at, deleted_at
		FROM voicemails WHERE deleted_at IS NULL`
	args := []any{}
	if lineID != "" {
		query += " AND line_id = ?"
		args = append(args, lineID)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var voicemails []Voicemail
	for rows.Next() {
		voicemail, scanErr := scanVoicemail(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		voicemails = append(voicemails, voicemail)
	}
	return voicemails, rows.Err()
}

func scanVoicemail(row rowScanner) (Voicemail, error) {
	var (
		voicemail Voicemail
		created   int64
		expires   int64
		deletedAt sql.NullInt64
	)
	err := row.Scan(&voicemail.ID, &voicemail.LineID, &voicemail.CallID, &voicemail.Peer, &voicemail.State,
		&voicemail.Path, &voicemail.DurationMs, &voicemail.SizeBytes, &created, &expires, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Voicemail{}, ErrNotFound
	}
	if err != nil {
		return Voicemail{}, err
	}
	voicemail.CreatedAt = fromMillis(created)
	voicemail.ExpiresAt = fromMillis(expires)
	if deletedAt.Valid {
		value := fromMillis(deletedAt.Int64)
		voicemail.DeletedAt = &value
	}
	return voicemail, nil
}

func (s *Store) DeleteVoicemail(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM voicemails WHERE id = ?`, id)
	return err
}

// SoftDeleteVoicemail marks a still-visible voicemail deleted. It affects at
// most one live row: concurrent/repeated deletes report false instead of
// pretending to delete something that is already gone. The audio blob is
// removed separately by the caller after this commits.
func (s *Store) SoftDeleteVoicemail(ctx context.Context, id string, deletedAt time.Time) (bool, error) {
	result, err := s.DB.ExecContext(ctx,
		`UPDATE voicemails SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`,
		millis(deletedAt), id)
	if err != nil {
		return false, err
	}
	affected, _ := result.RowsAffected()
	return affected == 1, nil
}

func (s *Store) ListExpiredVoicemails(ctx context.Context, now time.Time, limit int) ([]Voicemail, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, line_id, call_id, peer, state, path, duration_ms, size_bytes,
		created_at, expires_at, deleted_at FROM voicemails WHERE deleted_at IS NULL AND expires_at <= ?
		ORDER BY created_at LIMIT ?`, millis(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var expired []Voicemail
	for rows.Next() {
		voicemail, scanErr := scanVoicemail(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		expired = append(expired, voicemail)
	}
	return expired, rows.Err()
}

func (s *Store) VoicemailUsageBytes(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	if err := s.DB.QueryRowContext(ctx, `SELECT SUM(size_bytes) FROM voicemails WHERE deleted_at IS NULL`).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// ListDeletedVoicemails returns soft-deleted rows so retention can eventually
// reclaim their audio blobs once the SD mount is available again.
func (s *Store) ListDeletedVoicemails(ctx context.Context, limit int) ([]Voicemail, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, line_id, call_id, peer, state, path, duration_ms, size_bytes,
		created_at, expires_at, deleted_at FROM voicemails WHERE deleted_at IS NOT NULL ORDER BY deleted_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var voicemails []Voicemail
	for rows.Next() {
		voicemail, scanErr := scanVoicemail(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		voicemails = append(voicemails, voicemail)
	}
	return voicemails, rows.Err()
}

// ---------- recordings ----------

// Recording is one manual call recording. The row is the durable index; the
// audio blob at FilePath lives on the verified SD mount. State follows
// starting -> recording -> finalizing -> ready, or -> failed; deleted rows
// stay as tombstones so retries and the event log remain truthful.
type Recording struct {
	ID     string
	LineID string
	CallID string
	// Kind is "call" (default, manual 1:1) or "conference" (manual hosted
	// conference segment); ConferenceID is set only for conference rows.
	Kind             string
	ConferenceID     string
	DeviceID         string
	KeyID            string
	Direction        string
	Peer             string
	State            string
	TriggerMode      string
	StartedAt        *time.Time
	StoppedAt        *time.Time
	DurationMs       *int64
	FilePath         string
	Container        string
	Codec            string
	SampleRate       int
	Channels         int
	Bitrate          int
	SizeBytes        *int64
	SHA256           string
	WaveformPath     string
	FailureCode      string
	FailureDetail    string
	RetentionExpires *time.Time
	CreatedByDevice  string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

func (s *Store) SaveRecording(ctx context.Context, rec Recording) error {
	kind := rec.Kind
	if kind == "" {
		kind = "call"
	}
	conferenceID := rec.ConferenceID
	if kind != "conference" {
		conferenceID = ""
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO recordings (id, line_id, call_id, kind, conference_id, device_id, key_id, direction, peer,
		state, trigger_mode, started_at, stopped_at, duration_ms, file_path, container, codec, sample_rate,
		channels, bitrate, size_bytes, sha256, waveform_path, failure_code, failure_detail,
		retention_expires_at, created_by_device_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET state = excluded.state, stopped_at = excluded.stopped_at,
		duration_ms = excluded.duration_ms, file_path = excluded.file_path, size_bytes = excluded.size_bytes,
		sha256 = excluded.sha256, waveform_path = excluded.waveform_path, failure_code = excluded.failure_code,
		failure_detail = excluded.failure_detail, retention_expires_at = excluded.retention_expires_at,
		updated_at = excluded.updated_at`,
		rec.ID, rec.LineID, rec.CallID, kind, conferenceID, rec.DeviceID, rec.KeyID, rec.Direction, rec.Peer, rec.State, rec.TriggerMode,
		nullableMillis(rec.StartedAt), nullableMillis(rec.StoppedAt), nullableInt64(rec.DurationMs),
		rec.FilePath, rec.Container, rec.Codec, rec.SampleRate, rec.Channels, rec.Bitrate,
		nullableInt64(rec.SizeBytes), rec.SHA256, rec.WaveformPath, rec.FailureCode, rec.FailureDetail,
		nullableMillis(rec.RetentionExpires), rec.CreatedByDevice, millis(rec.CreatedAt), millis(rec.UpdatedAt))
	return err
}

// UpdateCallRecording writes the recording pointer/state onto the call row.
// It is deliberately separate from the call upserts so a stale call snapshot
// can never clear an active recording. Duration is monotonic: a smaller
// observed value is never allowed to replace a larger one.
func (s *Store) UpdateCallRecording(ctx context.Context, callID, recordingID, state string, durationMs *int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE calls SET recording_id = ?, recording_state = ?,
		recording_duration_ms = CASE WHEN ? IS NULL THEN recording_duration_ms
			WHEN recording_duration_ms IS NULL OR ? > recording_duration_ms THEN ? ELSE recording_duration_ms END
		WHERE id = ?`,
		recordingID, state, nullableInt64(durationMs), nullableInt64(durationMs), nullableInt64(durationMs), callID)
	return err
}

func (s *Store) GetRecording(ctx context.Context, id string) (Recording, error) {
	row := s.DB.QueryRowContext(ctx, recordingSelect+` WHERE id = ?`, id)
	return scanRecording(row)
}

// GetLatestRecordingByCall returns the newest non-deleted recording for a
// call. Multiple segments in one call are allowed; the latest segment is the
// live/current one the call row points at.
func (s *Store) GetLatestRecordingByCall(ctx context.Context, callID string) (Recording, error) {
	row := s.DB.QueryRowContext(ctx, recordingSelect+
		` WHERE call_id = ? AND state != 'deleted' ORDER BY created_at DESC LIMIT 1`, callID)
	return scanRecording(row)
}

func (s *Store) ListRecordings(ctx context.Context, lineID string, limit int) ([]Recording, error) {
	query := recordingSelect + ` WHERE state != 'deleted'`
	args := []any{}
	if lineID != "" {
		query += " AND line_id = ?"
		args = append(args, lineID)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var recordings []Recording
	for rows.Next() {
		rec, scanErr := scanRecording(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		recordings = append(recordings, rec)
	}
	return recordings, rows.Err()
}

// ListRecordingsByState returns rows in one state (crash recovery reconciles
// 'recording'/'finalizing' leftovers after a restart).
func (s *Store) ListRecordingsByState(ctx context.Context, state string, limit int) ([]Recording, error) {
	rows, err := s.DB.QueryContext(ctx, recordingSelect+` WHERE state = ? ORDER BY created_at LIMIT ?`, state, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var recordings []Recording
	for rows.Next() {
		rec, scanErr := scanRecording(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		recordings = append(recordings, rec)
	}
	return recordings, rows.Err()
}

func (s *Store) ListExpiredRecordings(ctx context.Context, now time.Time, limit int) ([]Recording, error) {
	rows, err := s.DB.QueryContext(ctx, recordingSelect+
		` WHERE state = 'ready' AND retention_expires_at IS NOT NULL AND retention_expires_at <= ?
		ORDER BY retention_expires_at LIMIT ?`, millis(now), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var expired []Recording
	for rows.Next() {
		rec, scanErr := scanRecording(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		expired = append(expired, rec)
	}
	return expired, rows.Err()
}

// RecordingUsageBytes sums the verified blob sizes of live recordings.
func (s *Store) RecordingUsageBytes(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT SUM(size_bytes) FROM recordings WHERE state != 'deleted'`).Scan(&total); err != nil {
		return 0, err
	}
	return total.Int64, nil
}

// HardDeleteRecordingRow removes a tombstone after retention has reclaimed
// its blobs. Device-initiated deletes only flip state to 'deleted'.
func (s *Store) HardDeleteRecordingRow(ctx context.Context, id string) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM recordings WHERE id = ?`, id)
	return err
}

const recordingSelect = `SELECT id, line_id, call_id, kind, conference_id, device_id, key_id, direction, peer, state, trigger_mode,
	started_at, stopped_at, duration_ms, file_path, container, codec, sample_rate, channels, bitrate,
	size_bytes, sha256, waveform_path, failure_code, failure_detail, retention_expires_at,
	created_by_device_id, created_at, updated_at FROM recordings`

func scanRecording(row rowScanner) (Recording, error) {
	var (
		rec          Recording
		startedAt    sql.NullInt64
		stoppedAt    sql.NullInt64
		durationMs   sql.NullInt64
		sizeBytes    sql.NullInt64
		retentionEnd sql.NullInt64
		createdAt    int64
		updatedAt    int64
	)
	err := row.Scan(&rec.ID, &rec.LineID, &rec.CallID, &rec.Kind, &rec.ConferenceID, &rec.DeviceID, &rec.KeyID, &rec.Direction, &rec.Peer,
		&rec.State, &rec.TriggerMode, &startedAt, &stoppedAt, &durationMs, &rec.FilePath, &rec.Container, &rec.Codec,
		&rec.SampleRate, &rec.Channels, &rec.Bitrate, &sizeBytes, &rec.SHA256, &rec.WaveformPath,
		&rec.FailureCode, &rec.FailureDetail, &retentionEnd, &rec.CreatedByDevice, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Recording{}, ErrNotFound
	}
	if err != nil {
		return Recording{}, err
	}
	if startedAt.Valid {
		value := fromMillis(startedAt.Int64)
		rec.StartedAt = &value
	}
	if stoppedAt.Valid {
		value := fromMillis(stoppedAt.Int64)
		rec.StoppedAt = &value
	}
	if durationMs.Valid {
		value := durationMs.Int64
		rec.DurationMs = &value
	}
	if sizeBytes.Valid {
		value := sizeBytes.Int64
		rec.SizeBytes = &value
	}
	if retentionEnd.Valid {
		value := fromMillis(retentionEnd.Int64)
		rec.RetentionExpires = &value
	}
	rec.CreatedAt = fromMillis(createdAt)
	rec.UpdatedAt = fromMillis(updatedAt)
	return rec, nil
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// ---------- thread tombstones ----------

// UpsertThreadTombstone records (or refreshes) a conversation delete. A
// repeated delete moves the horizon forward so messages that arrived after
// an earlier delete are also hidden.
func (s *Store) UpsertThreadTombstone(ctx context.Context, lineID, threadKey, deviceID string, deletedAt time.Time) error {
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO thread_tombstones (line_id, thread_key, device_id, deleted_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(line_id, thread_key) DO UPDATE SET deleted_at = excluded.deleted_at,
		 device_id = excluded.device_id`,
		lineID, threadKey, deviceID, millis(deletedAt))
	return err
}

// LineThreadTombstones returns every conversation delete horizon for one
// line so thread listing can filter without per-thread queries.
func (s *Store) LineThreadTombstones(ctx context.Context, lineID string) (map[string]time.Time, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT thread_key, deleted_at FROM thread_tombstones WHERE line_id = ?`, lineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	horizons := map[string]time.Time{}
	for rows.Next() {
		var key string
		var deletedAt int64
		if err := rows.Scan(&key, &deletedAt); err != nil {
			return nil, err
		}
		horizons[key] = fromMillis(deletedAt)
	}
	return horizons, rows.Err()
}

// ThreadTombstone returns the delete horizon for a conversation, or the zero
// time when the conversation was never deleted.
func (s *Store) ThreadTombstone(ctx context.Context, lineID, threadKey string) (time.Time, error) {
	var deletedAt sql.NullInt64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT deleted_at FROM thread_tombstones WHERE line_id = ? AND thread_key = ?`,
		lineID, threadKey).Scan(&deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	if !deletedAt.Valid {
		return time.Time{}, nil
	}
	return fromMillis(deletedAt.Int64), nil
}

// ---------- worker tokens ----------

func (s *Store) SaveWorkerToken(ctx context.Context, lineID, kind, token string, expiresAt time.Time) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO worker_tokens (line_id, kind, token, expires_at, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(line_id, kind) DO UPDATE SET token = excluded.token,
		expires_at = excluded.expires_at, updated_at = excluded.updated_at`,
		lineID, kind, token, millis(expiresAt), nowMillis())
	return err
}

func (s *Store) LoadWorkerToken(ctx context.Context, lineID, kind string) (string, time.Time, error) {
	var (
		token     string
		expiresAt int64
	)
	err := s.DB.QueryRowContext(ctx, `SELECT token, expires_at FROM worker_tokens WHERE line_id = ? AND kind = ?`,
		lineID, kind).Scan(&token, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return token, fromMillis(expiresAt), nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableMillis(value *time.Time) any {
	if value == nil {
		return nil
	}
	return millis(*value)
}
