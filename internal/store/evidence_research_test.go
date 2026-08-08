package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func researchTestNow() time.Time {
	return time.Date(2026, 8, 4, 8, 0, 0, 0, time.UTC)
}

// insertResearchCompetition inserts a minimal canonical competition and returns
// its ID for use in research-state tests. UpsertCompetition returns the prior
// row (empty for a new insert), so the ID is read back via GetCompetition.
func insertResearchCompetition(t *testing.T, database *Store) int64 {
	t.Helper()
	competition := model.Competition{
		EntityKey:   "research-test-competition",
		Name:        "Research test competition",
		Status:      model.StatusRegistrationOpen,
		OfficialURL: "https://example.com/research",
		Trust:       model.TrustHigh,
	}
	_, isNew, err := database.UpsertCompetition(context.Background(), competition, "test", researchTestNow())
	if err != nil || !isNew {
		t.Fatalf("insert competition: isNew=%v err=%v", isNew, err)
	}
	saved, err := database.GetCompetition(context.Background(), competition.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	return saved.ID
}

// TestEvidenceResearchMigrationV1ToV2Upgrade verifies that a database which has
// only ever applied migration v1 is upgraded to v2, that the
// evidence_research_state table is created, and that pre-existing competition
// data is preserved.
func TestEvidenceResearchMigrationV1ToV2Upgrade(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "upgrade-v1-v2.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	competitionID := insertResearchCompetition(t, database)
	// Simulate a v1-only database: remove the v2 record and the table it created.
	if _, err := database.db.Exec(`DELETE FROM schema_migrations WHERE version=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`DROP TABLE evidence_research_state`); err != nil {
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
	assertCount(t, database, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='evidence_research_state'`, 1)
	assertCount(t, database, `SELECT COUNT(*) FROM schema_migrations WHERE version=2 AND name='evidence_research_state'`, 1)
	// Pre-existing competition data must survive the upgrade.
	assertCountWithArgs(t, database, `SELECT COUNT(*) FROM competitions WHERE id=?`, competitionID, 1)
}

// assertCountWithArgs runs a parameterized COUNT query and asserts the result.
func assertCountWithArgs(t *testing.T, database *Store, query string, arg any, want int) {
	t.Helper()
	var got int
	if err := database.db.QueryRow(query, arg).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("query %q returned %d, want %d", query, got, want)
	}
}

// TestEvidenceResearchMigrationIdempotentReopen verifies reopening an already
// migrated database does not re-apply migration v2 or duplicate its record.
func TestEvidenceResearchMigrationIdempotentReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "idempotent.db")
	database, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	insertResearchCompetition(t, database)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	assertCount(t, database, `SELECT COUNT(*) FROM schema_migrations WHERE version=2 AND name='evidence_research_state'`, 1)
}

// TestEvidenceResearchStateCascadeDelete verifies deleting a competition cascades
// to its evidence_research_state rows via the foreign key.
func TestEvidenceResearchStateCascadeDelete(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "cascade.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceRegistrationEnd, model.ResearchStateUnresolved, researchTestNow(), researchRetryAt(researchTestNow(), 72), ""); err != nil {
		t.Fatal(err)
	}
	assertCountWithArgs(t, database, `SELECT COUNT(*) FROM evidence_research_state WHERE competition_id=?`, competitionID, 1)
	if err := database.DeleteCompetition(ctx, competitionID); err != nil {
		t.Fatal(err)
	}
	assertCountWithArgs(t, database, `SELECT COUNT(*) FROM evidence_research_state WHERE competition_id=?`, competitionID, 0)
}

// researchRetryAt returns a *time.Time strictly after base.
func researchRetryAt(base time.Time, hours int) *time.Time {
	value := base.Add(time.Duration(hours) * time.Hour)
	return &value
}

// TestRecordEvidenceResearchAttemptFirstAttempt verifies the first record for a
// pair sets attempt_count to 1.
func TestRecordEvidenceResearchAttemptFirstAttempt(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "attempt-first.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)
	attempted := researchTestNow()
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceCompetitionEnd, model.ResearchStateRetryable, attempted, researchRetryAt(attempted, 6), "dial timeout"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.db.QueryRow(`SELECT attempt_count FROM evidence_research_state WHERE competition_id=? AND field='competition_end'`, competitionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("first attempt_count=%d, want 1", count)
	}
}

// TestRecordEvidenceResearchAttemptIncrements verifies a second record for the
// same competition+field increments attempt_count rather than resetting it.
func TestRecordEvidenceResearchAttemptIncrements(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "attempt-increment.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)
	attempted := researchTestNow()
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceCompetitionEnd, model.ResearchStateRetryable, attempted, researchRetryAt(attempted, 6), ""); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceCompetitionEnd, model.ResearchStateRetryable, attempted, researchRetryAt(attempted, 6), ""); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.db.QueryRow(`SELECT attempt_count FROM evidence_research_state WHERE competition_id=? AND field='competition_end'`, competitionID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("second attempt_count=%d, want 2", count)
	}
}

// TestRecordEvidenceResearchAttemptStoresRetryAt verifies retryable/unresolved
// outcomes persist next_retry_at.
func TestRecordEvidenceResearchAttemptStoresRetryAt(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "attempt-retry-at.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)
	attempted := researchTestNow()
	retryAt := researchRetryAt(attempted, 72)
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceRegistrationStart, model.ResearchStateUnresolved, attempted, retryAt, "no official evidence"); err != nil {
		t.Fatal(err)
	}
	var nextRetry int64
	var status string
	if err := database.db.QueryRow(`SELECT next_retry_at, status FROM evidence_research_state WHERE competition_id=? AND field='registration_start'`, competitionID).Scan(&nextRetry, &status); err != nil {
		t.Fatal(err)
	}
	if nextRetry != retryAt.Unix() || status != "unresolved" {
		t.Fatalf("next_retry_at=%d status=%q, want %d unresolved", nextRetry, status, retryAt.Unix())
	}
}

// TestRecordEvidenceResearchAttemptResolvedClearsRetryAt verifies resolved /
// skipped outcomes do not persist next_retry_at.
func TestRecordEvidenceResearchAttemptResolvedClearsRetryAt(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "attempt-resolved.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)
	attempted := researchTestNow()
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceCompetitionStart, model.ResearchStateResolved, attempted, nil, ""); err != nil {
		t.Fatal(err)
	}
	var nextRetry sql.NullInt64
	if err := database.db.QueryRow(`SELECT next_retry_at FROM evidence_research_state WHERE competition_id=? AND field='competition_start'`, competitionID).Scan(&nextRetry); err != nil {
		t.Fatal(err)
	}
	if nextRetry.Valid {
		t.Fatalf("resolved state must not persist next_retry_at, got %d", nextRetry.Int64)
	}
}

// TestRecordEvidenceResearchAttemptRejectsInvalidInput covers the validation
// rules: bad competition id, bad field, bad status, and the retry-after rule.
func TestRecordEvidenceResearchAttemptRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "attempt-invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	attempted := researchTestNow()

	if err := database.RecordEvidenceResearchAttempt(ctx, 0, model.EvidenceCompetitionEnd, model.ResearchStateRetryable, attempted, researchRetryAt(attempted, 6), ""); err == nil {
		t.Fatal("expected rejection for invalid competition id")
	}
	if err := database.RecordEvidenceResearchAttempt(ctx, 1, model.EvidenceField("fee"), model.ResearchStateRetryable, attempted, researchRetryAt(attempted, 6), ""); err == nil {
		t.Fatal("expected rejection for invalid field")
	}
	if err := database.RecordEvidenceResearchAttempt(ctx, 1, model.EvidenceCompetitionEnd, model.ResearchStateStatus("bad"), attempted, researchRetryAt(attempted, 6), ""); err == nil {
		t.Fatal("expected rejection for invalid status")
	}
	// retryable/unresolved require next_retry_at strictly after attempted_at.
	if err := database.RecordEvidenceResearchAttempt(ctx, 1, model.EvidenceCompetitionEnd, model.ResearchStateRetryable, attempted, nil, ""); err == nil {
		t.Fatal("expected rejection for retryable without next_retry_at")
	}
	before := attempted.Add(-time.Hour)
	if err := database.RecordEvidenceResearchAttempt(ctx, 1, model.EvidenceCompetitionEnd, model.ResearchStateUnresolved, attempted, &before, ""); err == nil {
		t.Fatal("expected rejection for next_retry_at before attempted_at")
	}
}

// TestListEvidenceResearchStatesEmpty verifies an empty state table returns an
// empty slice with no error.
func TestListEvidenceResearchStatesEmpty(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "states-empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	states, err := database.ListEvidenceResearchStates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("expected no states, got %d", len(states))
	}
}
