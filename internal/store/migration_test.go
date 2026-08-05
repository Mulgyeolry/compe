package store

import (
	"context"
	"database/sql"
	"path/filepath"
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
}
