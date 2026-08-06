package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestLegacyNotificationsAreMigratedAndDropped(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy-notifications.db")
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)

	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	competition := model.Competition{
		EntityKey:   "legacy-notification-migration",
		Name:        "Legacy notification migration",
		Status:      model.StatusRegistrationOpen,
		OfficialURL: "https://example.com/legacy",
		Trust:       model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(context.Background(), competition, "legacy", now); err != nil || !isNew {
		t.Fatalf("insert competition: isNew=%v err=%v", isNew, err)
	}
	saved, err := database.GetCompetition(context.Background(), competition.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-migration database: the schema has been built but the
	// migration record is absent, so the next open re-runs version 1 and
	// migrates the legacy notifications outbox.
	if _, err := database.db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE notifications (
id INTEGER PRIMARY KEY AUTOINCREMENT,
competition_id INTEGER NOT NULL REFERENCES competitions(id) ON DELETE CASCADE,
event_type TEXT NOT NULL,event_key TEXT NOT NULL,delivery_group TEXT NOT NULL,
subject TEXT NOT NULL,body TEXT NOT NULL,status TEXT NOT NULL,last_error TEXT NOT NULL,
created_at INTEGER NOT NULL,sent_at INTEGER,UNIQUE(competition_id,event_type,event_key))`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO notifications(competition_id,event_type,event_key,delivery_group,subject,body,status,last_error,created_at)
VALUES(?,?,?,?,?,?,'pending','',?)`, saved.ID, "registration_opened", "registration_open", "legacy-group", "subject", "body", now.Unix()); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	var eventCount int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM competition_events
WHERE competition_id=? AND event_type='registration_opened' AND event_key='registration_open'`, saved.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("migrated event count=%d, want 1", eventCount)
	}
	assertCount(t, database, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='notifications'`, 0)
	assertCount(t, database, `SELECT COUNT(*) FROM schema_migrations WHERE version=1 AND name='baseline_current_schema'`, 1)
}

// TestFreshDatabaseMigratesToVersion1 verifies a brand-new database gets the
// complete schema and records exactly one migration: version 1.
func TestFreshDatabaseMigratesToVersion1(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	coreTables := []string{
		"meta", "source_documents", "observations", "competitions",
		"competition_sources", "competition_events", "users", "user_preferences",
		"user_categories", "user_organizer_types", "user_competition_scopes",
		"user_regions", "user_keywords", "verification_codes", "sessions",
		"user_notifications", "user_competition_choices",
	}
	for _, table := range coreTables {
		assertCount(t, database, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='`+table+`'`, 1)
	}
	assertCount(t, database, `SELECT COUNT(*) FROM schema_migrations`, 1)
	assertCount(t, database, `SELECT COUNT(*) FROM schema_migrations WHERE version=1 AND name='baseline_current_schema'`, 1)
}

// TestReopenDoesNotRerunMigration verifies closing and reopening a database does
// not re-apply migrations; version 1 stays a single record.
func TestReopenDoesNotRerunMigration(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "reopen.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	assertCount(t, database, `SELECT COUNT(*) FROM schema_migrations`, 1)
	assertCount(t, database, `SELECT COUNT(*) FROM schema_migrations WHERE version=1 AND name='baseline_current_schema'`, 1)
}

// TestLegacyDatabaseGetsMissingColumns verifies a pre-migration database that
// lacks columns introduced over time is brought up to date by version 1, while
// its existing data is preserved.
func TestLegacyDatabaseGetsMissingColumns(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy-columns.db")
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)

	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Insert a row, then drop a column that ensureColumn adds, and clear the
	// migration record so version 1 re-runs as if on an older database.
	competition := model.Competition{
		EntityKey:   "legacy-columns",
		Name:        "Legacy columns",
		Status:      model.StatusPreview,
		OfficialURL: "https://example.com/legacy-columns",
		Trust:       model.TrustMedium,
	}
	if _, isNew, err := database.UpsertCompetition(context.Background(), competition, "legacy", now); err != nil || !isNew {
		t.Fatalf("insert competition: isNew=%v err=%v", isNew, err)
	}
	if _, err := database.db.Exec(`ALTER TABLE competitions DROP COLUMN facts_json`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DELETE FROM schema_migrations`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	assertCount(t, database, `SELECT COUNT(*) FROM pragma_table_info('competitions') WHERE name='facts_json'`, 1)
	assertCount(t, database, `SELECT COUNT(*) FROM schema_migrations WHERE version=1 AND name='baseline_current_schema'`, 1)
}

// TestFutureSchemaVersionRejected verifies opening a database whose recorded
// version is newer than the supported maximum fails with a clear error and
// does not modify the database further.
func TestFutureSchemaVersionRejected(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "future.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO schema_migrations(version, name, applied_at) VALUES(?,?,?)`, 3, "future", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(path)
	if err == nil || !strings.Contains(err.Error(), "newer than supported version 1") {
		t.Fatalf("expected future-version error containing 'newer than supported version 1', got: %v", err)
	}
}

// TestMigrationFailureRollsBack verifies a failed migration does not record its
// version and rolls back any structural changes already made inside the
// transaction.
func TestMigrationFailureRollsBack(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	err = database.applyMigration(migration{
		version: 999,
		name:    "test-fail",
		up: func(tx *sql.Tx) error {
			if _, err := tx.Exec(`CREATE TABLE should_not_persist (x INTEGER)`); err != nil {
				return err
			}
			return errors.New("simulated migration failure")
		},
	})
	if err == nil {
		t.Fatal("expected migration to fail")
	}
	assertCount(t, database, `SELECT COUNT(*) FROM schema_migrations WHERE version=999`, 0)
	assertCount(t, database, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='should_not_persist'`, 0)
}
