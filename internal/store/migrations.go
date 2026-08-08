package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// migration represents a single, versioned, forward-only schema migration. Each
// migration runs in its own transaction; a version is recorded only after its
// up func succeeds, so a failed migration rolls back atomically and is retried
// on the next open.
type migration struct {
	version int
	name    string
	up      func(*sql.Tx) error
}

// migrations is the ordered, immutable set of schema migrations supported by
// this build. Versions must be strictly increasing and unique; validation at
// open enforces those invariants. Only forward migrations exist; there is no
// down migration.
var migrations = []migration{
	{
		version: 1,
		name:    "baseline_current_schema",
		up:      baselineCurrentSchemaUp,
	},
	{
		version: 2,
		name:    "evidence_research_state",
		up:      evidenceResearchStateUp,
	},
}

// validateMigrations checks the static migration definitions before any database
// work: versions must be positive, strictly increasing, unique, and each name
// must be non-empty.
func validateMigrations() error {
	seen := make(map[int]bool)
	prev := 0
	for _, m := range migrations {
		if m.version <= 0 {
			return fmt.Errorf("migration version must be greater than zero: got %d", m.version)
		}
		if m.version <= prev {
			return fmt.Errorf("migration versions must be strictly increasing: %d after %d", m.version, prev)
		}
		if m.name == "" {
			return fmt.Errorf("migration %d must have a non-empty name", m.version)
		}
		if seen[m.version] {
			return fmt.Errorf("duplicate migration version %d", m.version)
		}
		seen[m.version] = true
		prev = m.version
	}
	return nil
}

// runMigrations applies every pending migration, in order, each inside its own
// transaction. A database whose highest recorded version exceeds the newest
// supported migration is rejected so an older program cannot open a database
// that a newer program upgraded.
func (s *Store) runMigrations() error {
	if err := validateMigrations(); err != nil {
		return err
	}
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at INTEGER NOT NULL
)`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}
	var current int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read latest migration version: %w", err)
	}
	maxVersion := migrations[len(migrations)-1].version
	if current > maxVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", current, maxVersion)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := s.applyMigration(m); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration executes a single migration's up func inside a transaction and
// records the version in schema_migrations before committing. Any error rolls
// back both the schema/data changes and the version record.
func (s *Store) applyMigration(m migration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %d (%s): %w", m.version, m.name, err)
	}
	defer tx.Rollback()
	if err := m.up(tx); err != nil {
		return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?, ?, ?)`, m.version, m.name, time.Now().Unix()); err != nil {
		return fmt.Errorf("record migration %d (%s): %w", m.version, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d (%s): %w", m.version, m.name, err)
	}
	return nil
}

// baselineCurrentSchemaUp builds the complete current database structure and
// reconciles existing databases: it creates all tables and indexes, adds any
// columns that were introduced over time, migrates the legacy notifications
// outbox into competition_events, and drops that obsolete table. It is exactly
// the previous unversioned startup schema now wrapped as version 1, so every
// compatibility behaviour is preserved.
func baselineCurrentSchemaUp(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS source_documents (
    source_id TEXT NOT NULL,
    url TEXT NOT NULL,
    last_hash TEXT NOT NULL,
    analyzer_version TEXT NOT NULL DEFAULT '',
    last_seen INTEGER NOT NULL,
    PRIMARY KEY(source_id, url)
);
CREATE TABLE IF NOT EXISTS observations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id TEXT NOT NULL,
    url TEXT NOT NULL,
    title TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    content TEXT NOT NULL,
    trust TEXT NOT NULL,
    analyzer_version TEXT NOT NULL DEFAULT '',
    analysis_result_json TEXT NOT NULL DEFAULT '{}',
    seen_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_observations_url_seen ON observations(url, seen_at DESC);
CREATE TABLE IF NOT EXISTS competitions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_key TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    organizer TEXT NOT NULL,
    status TEXT NOT NULL,
    status_evidence TEXT NOT NULL,
    registration_phase TEXT NOT NULL DEFAULT 'unknown',
    competition_phase TEXT NOT NULL DEFAULT 'unknown',
    registration_start INTEGER,
    registration_start_raw TEXT NOT NULL,
    registration_end INTEGER,
    registration_end_raw TEXT NOT NULL,
    competition_start INTEGER,
    competition_start_raw TEXT NOT NULL DEFAULT '',
    competition_end INTEGER,
    competition_end_raw TEXT NOT NULL DEFAULT '',
    team_requirement TEXT NOT NULL,
    fee TEXT NOT NULL DEFAULT '',
    fee_evidence TEXT NOT NULL DEFAULT '',
    keywords_json TEXT NOT NULL DEFAULT '[]',
    analysis_json TEXT NOT NULL DEFAULT '{}',
    facts_json TEXT NOT NULL DEFAULT '{}',
    content TEXT NOT NULL,
    fit_score INTEGER NOT NULL,
    fit_reason TEXT NOT NULL,
    eligibility_note TEXT NOT NULL,
    official_url TEXT NOT NULL,
    trust TEXT NOT NULL,
    problem_released INTEGER NOT NULL,
    analyzer_version TEXT NOT NULL DEFAULT '',
    content_hash TEXT NOT NULL,
    first_seen INTEGER NOT NULL,
    last_seen INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS competition_sources (
    competition_id INTEGER NOT NULL REFERENCES competitions(id) ON DELETE CASCADE,
    source_id TEXT NOT NULL,
    url TEXT NOT NULL,
    trust TEXT NOT NULL,
    last_seen INTEGER NOT NULL,
    UNIQUE(competition_id, url)
);
CREATE TABLE IF NOT EXISTS competition_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    competition_id INTEGER NOT NULL REFERENCES competitions(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    event_key TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE(competition_id, event_type, event_key)
);
CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT NOT NULL COLLATE NOCASE UNIQUE,
    verified_at INTEGER NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS user_preferences (
    user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    frequency TEXT NOT NULL DEFAULT 'daily',
    delivery_time TEXT NOT NULL DEFAULT '08:00',
    weekly_day INTEGER NOT NULL DEFAULT 1,
    timezone TEXT NOT NULL DEFAULT 'Asia/Shanghai',
    min_trust TEXT NOT NULL DEFAULT 'medium',
    allow_eligibility_risk INTEGER NOT NULL DEFAULT 1,
    notify_preview INTEGER NOT NULL DEFAULT 1,
    notify_registration INTEGER NOT NULL DEFAULT 1,
    notify_upcoming INTEGER NOT NULL DEFAULT 1,
    notify_started INTEGER NOT NULL DEFAULT 1,
    notify_problem_release INTEGER NOT NULL DEFAULT 1,
    notify_deadline_7d INTEGER NOT NULL DEFAULT 1,
    notify_deadline_1d INTEGER NOT NULL DEFAULT 1,
    notify_important_update INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS user_categories (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    category TEXT NOT NULL,
    PRIMARY KEY(user_id, category)
);
CREATE TABLE IF NOT EXISTS user_organizer_types (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    organizer_type TEXT NOT NULL,
    PRIMARY KEY(user_id, organizer_type)
);
CREATE TABLE IF NOT EXISTS user_competition_scopes (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    scope TEXT NOT NULL,
    PRIMARY KEY(user_id, scope)
);
CREATE TABLE IF NOT EXISTS user_regions (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    region TEXT NOT NULL,
    PRIMARY KEY(user_id, region)
);
CREATE TABLE IF NOT EXISTS user_keywords (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK(kind IN ('include', 'exclude')),
    keyword TEXT NOT NULL COLLATE NOCASE,
    PRIMARY KEY(user_id, kind, keyword)
);
CREATE TABLE IF NOT EXISTS verification_codes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    email TEXT NOT NULL COLLATE NOCASE,
    code_hash TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    consumed_at INTEGER,
    attempts INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_verification_codes_email ON verification_codes(email, created_at DESC);
CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS user_notifications (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    competition_id INTEGER NOT NULL REFERENCES competitions(id) ON DELETE CASCADE,
    event_type TEXT NOT NULL,
    event_key TEXT NOT NULL,
    delivery_group TEXT NOT NULL,
    status TEXT NOT NULL,
    last_error TEXT NOT NULL,
    due_at INTEGER NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    sent_at INTEGER,
    UNIQUE(user_id, competition_id, event_type, event_key)
);
CREATE INDEX IF NOT EXISTS idx_user_notifications_due ON user_notifications(status, due_at, delivery_group);
CREATE TABLE IF NOT EXISTS user_competition_choices (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    competition_id INTEGER NOT NULL REFERENCES competitions(id) ON DELETE CASCADE,
    decision TEXT NOT NULL CHECK(decision IN ('participating','declined')),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY(user_id, competition_id)
);
CREATE INDEX IF NOT EXISTS idx_user_competition_choices_competition ON user_competition_choices(competition_id, decision);
`)
	if err != nil {
		return err
	}
	for _, column := range []struct {
		table      string
		name       string
		definition string
	}{
		{"competitions", "fee", "TEXT NOT NULL DEFAULT ''"},
		{"competitions", "fee_evidence", "TEXT NOT NULL DEFAULT ''"},
		{"competitions", "keywords_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"competitions", "analysis_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"competitions", "competition_start", "INTEGER"},
		{"competitions", "competition_start_raw", "TEXT NOT NULL DEFAULT ''"},
		{"competitions", "competition_end", "INTEGER"},
		{"competitions", "competition_end_raw", "TEXT NOT NULL DEFAULT ''"},
		{"competitions", "analyzer_version", "TEXT NOT NULL DEFAULT ''"},
		{"competitions", "registration_phase", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"competitions", "competition_phase", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"competitions", "facts_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"source_documents", "analyzer_version", "TEXT NOT NULL DEFAULT ''"},
		{"observations", "analyzer_version", "TEXT NOT NULL DEFAULT ''"},
		{"observations", "analysis_result_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"user_preferences", "notify_upcoming", "INTEGER NOT NULL DEFAULT 1"},
		{"user_preferences", "notify_started", "INTEGER NOT NULL DEFAULT 1"},
	} {
		if err := ensureColumn(tx, column.table, column.name, column.definition); err != nil {
			return err
		}
	}
	return removeLegacyNotifications(tx)
}

// evidenceResearchStateUp creates the evidence_research_state table that records
// the scheduling history of individual evidence-research attempts for a
// competition field. It is deliberately a separate forward-only migration so the
// version 1 baseline history is untouched. The table stores only attempt
// outcomes / cooldown scheduling — it is not a second source of truth for
// competition data, and never holds evidence body text (the canonical
// Competition remains the source of truth; real evidence lives in the
// FactEvidence / research-audit chain). A competition delete cascades to its
// research-state rows via the foreign key.
func evidenceResearchStateUp(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS evidence_research_state (
    competition_id INTEGER NOT NULL REFERENCES competitions(id) ON DELETE CASCADE,
    field TEXT NOT NULL,
    status TEXT NOT NULL,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    last_attempt_at INTEGER,
    next_retry_at INTEGER,
    last_error TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (competition_id, field),
    CHECK (attempt_count >= 0),
    CHECK (field IN ('registration_start','registration_end','competition_start','competition_end')),
    CHECK (status IN ('retryable','unresolved','resolved','skipped'))
)
`)
	return err
}

// removeLegacyNotifications migrates deduplication keys from the pre-user
// notification outbox into competition_events and drops that obsolete table.
// Current deliveries are stored only in user_notifications. It runs inside the
// caller's migration transaction; all identifiers are fixed in the migration
// definition and never derived from user input.
func removeLegacyNotifications(tx *sql.Tx) error {
	var exists int
	err := tx.QueryRow(`SELECT 1 FROM sqlite_master WHERE type='table' AND name='notifications'`).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO competition_events(competition_id,event_type,event_key,created_at)
SELECT competition_id,event_type,event_key,created_at FROM notifications`); err != nil {
		return fmt.Errorf("migrate legacy notification events: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE notifications`); err != nil {
		return fmt.Errorf("drop legacy notifications table: %w", err)
	}
	return nil
}

// ensureColumn adds a column to a table if it does not already exist, so older
// databases are brought up to the current schema without data loss. It runs
// against the caller's migration transaction; table and column identifiers come
// from the fixed migration definitions, never from user input.
func ensureColumn(tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}
