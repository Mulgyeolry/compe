package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestCleanupRetainsDedupeDataAndLatestObservation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	database, err := Open(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	old := now.AddDate(0, -8, 0).Unix()
	recent := now.AddDate(0, 0, -2).Unix()
	for _, row := range []struct {
		url  string
		hash string
		seen int64
	}{
		{"https://example.com/a", "old-a-1", old - 10},
		{"https://example.com/a", "old-a-2", old},
		{"https://example.com/b", "recent-b", recent},
	} {
		if _, err := database.db.ExecContext(ctx, `INSERT INTO observations(source_id,url,title,content_hash,content,trust,seen_at)
VALUES('source',?,'title',?,'large page body','high',?)`, row.url, row.hash, row.seen); err != nil {
			t.Fatal(err)
		}
	}

	closed := testClosedCompetition("old-closed", "https://example.com/old", "body to compact")
	if _, _, err := database.UpsertCompetition(ctx, closed, "source", time.Unix(old, 0)); err != nil {
		t.Fatal(err)
	}
	closed, err = database.GetCompetition(ctx, closed.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	pending := testClosedCompetition("old-pending", "https://example.com/pending", "must remain until delivery")
	if _, _, err := database.UpsertCompetition(ctx, pending, "source", time.Unix(old, 0)); err != nil {
		t.Fatal(err)
	}
	pending, err = database.GetCompetition(ctx, pending.EntityKey)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := database.db.ExecContext(ctx, `INSERT INTO competition_events(competition_id,event_type,event_key,created_at)
VALUES(?,'finished','once',?)`, closed.ID, old); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `INSERT INTO users(email,verified_at,enabled,created_at,updated_at)
VALUES('old@example.com',?,1,?,?)`, old, old, old); err != nil {
		t.Fatal(err)
	}
	var userID int64
	if err := database.db.QueryRowContext(ctx, `SELECT id FROM users WHERE email='old@example.com'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `INSERT INTO user_notifications(
user_id,competition_id,event_type,event_key,delivery_group,status,last_error,due_at,created_at)
VALUES(?,?, 'finished','pending','pending-group','pending','',?,?)`, userID, pending.ID, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `INSERT INTO verification_codes(email,code_hash,expires_at,created_at)
VALUES('old@example.com','hash',?,?)`, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at,created_at)
VALUES('token',?,?,?)`, userID, old, old); err != nil {
		t.Fatal(err)
	}

	report, err := database.Cleanup(ctx, RetentionPolicy{
		ObservationAge:              30 * 24 * time.Hour,
		ClosedCompetitionContentAge: 180 * 24 * time.Hour,
		ExpiredAuthenticationAge:    7 * 24 * time.Hour,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ObservationsDeleted != 1 || report.CompetitionsCompacted != 1 ||
		report.VerificationCodesDeleted != 1 || report.SessionsDeleted != 1 {
		t.Fatalf("unexpected cleanup report: %+v", report)
	}

	assertCount(t, database, `SELECT COUNT(*) FROM observations`, 2)
	assertCount(t, database, `SELECT COUNT(*) FROM competition_events`, 1)
	assertCount(t, database, `SELECT COUNT(*) FROM verification_codes`, 0)
	assertCount(t, database, `SELECT COUNT(*) FROM sessions`, 0)
	assertText(t, database, `SELECT content FROM competitions WHERE id=?`, closed.ID, "")
	assertText(t, database, `SELECT content FROM competitions WHERE id=?`, pending.ID, "must remain until delivery")
}

func testClosedCompetition(key, url, content string) model.Competition {
	return model.Competition{
		EntityKey:      key,
		Name:           key,
		Status:         model.StatusFinished,
		Content:        content,
		OfficialURL:    url,
		Trust:          model.TrustHigh,
		ContentHash:    key + "-hash",
		StatusEvidence: "比赛已结束",
	}
}

func assertCount(t *testing.T, database *Store, query string, want int) {
	t.Helper()
	var got int
	if err := database.db.QueryRow(query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query %q returned %d, want %d", query, got, want)
	}
}

func assertText(t *testing.T, database *Store, query string, arg any, want string) {
	t.Helper()
	var got string
	if err := database.db.QueryRow(query, arg).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query %q returned %q, want %q", query, got, want)
	}
}
