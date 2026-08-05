package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestParticipationChoiceCanChangeAndDeclineCancelsPending(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "choices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	userID := insertTestUser(t, database, now)
	competitionID := insertTestCompetition(t, database, now)
	_, err = database.EnqueueUserCompetitionEvents(ctx, competitionID, []UserEventDispatch{{
		UserID: userID, Event: model.Event{Type: "registration_opened", Key: "open"}, GroupKey: "choice-test", DueAt: now,
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetUserCompetitionDecision(ctx, userID, competitionID, model.ParticipationParticipating, now); err != nil {
		t.Fatal(err)
	}
	if got, err := database.GetUserCompetitionDecision(ctx, userID, competitionID); err != nil || got != model.ParticipationParticipating {
		t.Fatalf("participating choice=%q err=%v", got, err)
	}
	if err := database.SetUserCompetitionDecision(ctx, userID, competitionID, model.ParticipationDeclined, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got, err := database.GetUserCompetitionDecision(ctx, userID, competitionID); err != nil || got != model.ParticipationDeclined {
		t.Fatalf("declined choice=%q err=%v", got, err)
	}
	if pending, err := database.CountUserPendingNotifications(ctx, userID); err != nil || pending != 0 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
}

func TestResetCompetitionDataPreservesUsersAndPreferences(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "reset.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	userID := insertTestUser(t, database, now)
	competitionID := insertTestCompetition(t, database, now)
	if err := database.SetUserCompetitionDecision(ctx, userID, competitionID, model.ParticipationParticipating, now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `INSERT INTO observations(source_id,url,title,content_hash,content,trust,seen_at) VALUES('s','https://example.com','t','h','c','high',?)`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, `INSERT INTO source_documents(source_id,url,last_hash,last_seen) VALUES('s','https://example.com','h',?)`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkBootstrapped(ctx); err != nil {
		t.Fatal(err)
	}
	report, err := database.ResetCompetitionData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Competitions != 1 || report.Observations != 1 || report.Documents != 1 {
		t.Fatalf("unexpected reset report: %#v", report)
	}
	var enabledUsers int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE enabled=1`).Scan(&enabledUsers); err != nil || enabledUsers != 1 {
		t.Fatalf("user was not preserved: count=%d err=%v", enabledUsers, err)
	}
	if _, err := database.GetUserPreferences(ctx, userID); err != nil {
		t.Fatalf("preferences were not preserved: %v", err)
	}
	if competitions, err := database.ListCompetitions(ctx); err != nil || len(competitions) != 0 {
		t.Fatalf("competitions remain: %#v err=%v", competitions, err)
	}
	if bootstrapped, err := database.IsBootstrapped(ctx); err != nil || bootstrapped {
		t.Fatalf("discovery baseline was not reset: bootstrapped=%v err=%v", bootstrapped, err)
	}
}

func TestTransientAnalysisFailureCanRetryUnchangedDocument(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	document := model.Document{URL: "https://example.com/retry", Title: "AI Agent 大赛", Text: "报名预告"}
	changed, err := database.RecordObservationVersioned(ctx, "search", document, "same-hash", model.TrustMedium, "", now)
	if err != nil || !changed {
		t.Fatalf("first observation changed=%v err=%v", changed, err)
	}
	if err := database.RetryDocumentOnNextScan(ctx, "search", document.URL); err != nil {
		t.Fatal(err)
	}
	changed, err = database.RecordObservationVersioned(ctx, "search", document, "same-hash", model.TrustMedium, "", now.Add(time.Hour))
	if err != nil || !changed {
		t.Fatalf("retry observation changed=%v err=%v", changed, err)
	}
}

func insertTestUser(t *testing.T, database *Store, now time.Time) int64 {
	t.Helper()
	result, err := database.db.Exec(`INSERT INTO users(email,verified_at,enabled,created_at,updated_at) VALUES('test@example.com',?,1,?,?)`, now.Unix(), now.Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	userID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO user_preferences(user_id) VALUES(?)`, userID); err != nil {
		t.Fatal(err)
	}
	return userID
}

func insertTestCompetition(t *testing.T, database *Store, now time.Time) int64 {
	t.Helper()
	competition := model.Competition{
		EntityKey: "test-competition", Name: "测试程序设计大赛", Status: model.StatusRegistrationOpen,
		OfficialURL: "https://example.com/competition", Trust: model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(context.Background(), competition, "test", now); err != nil || !isNew {
		t.Fatalf("insert competition: isNew=%v err=%v", isNew, err)
	}
	saved, err := database.GetCompetition(context.Background(), competition.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	return saved.ID
}
