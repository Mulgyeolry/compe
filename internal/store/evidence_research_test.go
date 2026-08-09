package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

// persistedLastError reads back the stored last_error for a competition field.
func persistedLastError(t *testing.T, database *Store, competitionID int64, field model.EvidenceField) string {
	t.Helper()
	var value string
	if err := database.db.QueryRow(`SELECT last_error FROM evidence_research_state WHERE competition_id=? AND field=?`, competitionID, string(field)).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// TestTruncateEvidenceResearchErrorShortVerbatim verifies short diagnostics are
// stored unchanged.
func TestTruncateEvidenceResearchErrorShortVerbatim(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "err-short.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)
	attempted := researchTestNow()
	short := "dial tcp: connection refused"
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceCompetitionEnd, model.ResearchStateRetryable, attempted, researchRetryAt(attempted, 6), short); err != nil {
		t.Fatal(err)
	}
	if got := persistedLastError(t, database, competitionID, model.EvidenceCompetitionEnd); got != short {
		t.Fatalf("short last_error = %q, want %q", got, short)
	}
}

// TestTruncateEvidenceResearchErrorLongEnglish verifies an overlong ASCII error
// is truncated to exactly maxEvidenceResearchErrorRunes runes.
func TestTruncateEvidenceResearchErrorLongEnglish(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "err-long-en.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)
	attempted := researchTestNow()
	long := strings.Repeat("a", maxEvidenceResearchErrorRunes+1000)
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceCompetitionEnd, model.ResearchStateRetryable, attempted, researchRetryAt(attempted, 6), long); err != nil {
		t.Fatal(err)
	}
	got := persistedLastError(t, database, competitionID, model.EvidenceCompetitionEnd)
	if len(got) != maxEvidenceResearchErrorRunes {
		t.Fatalf("english last_error len=%d, want %d", len(got), maxEvidenceResearchErrorRunes)
	}
}

// TestTruncateEvidenceResearchErrorLongChineseRuneSafe verifies an overlong
// multi-byte (Chinese) error is truncated by runes, not bytes, and stays valid
// UTF-8.
func TestTruncateEvidenceResearchErrorLongChineseRuneSafe(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "err-long-zh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)
	attempted := researchTestNow()
	// Each Chinese character is 3 bytes in UTF-8, so byte truncation would split
	// one; the rune-aware helper must keep a whole number of runes.
	chinese := strings.Repeat("赛", maxEvidenceResearchErrorRunes+200)
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceCompetitionEnd, model.ResearchStateRetryable, attempted, researchRetryAt(attempted, 6), chinese); err != nil {
		t.Fatal(err)
	}
	got := persistedLastError(t, database, competitionID, model.EvidenceCompetitionEnd)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated last_error is not valid UTF-8: %q", got)
	}
	if runeCount := utf8.RuneCountInString(got); runeCount != maxEvidenceResearchErrorRunes {
		t.Fatalf("chinese last_error rune count=%d, want %d", runeCount, maxEvidenceResearchErrorRunes)
	}
}

// TestTruncateEvidenceResearchErrorSecondUpsertNotBypassed verifies that a
// second UPSERT with an overlong error is also truncated, so a long error cannot
// bypass the limit by arriving on a later update.
func TestTruncateEvidenceResearchErrorSecondUpsertNotBypassed(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "err-second.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)
	attempted := researchTestNow()
	// First attempt with a short error, then a second with an overlong one.
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceCompetitionEnd, model.ResearchStateRetryable, attempted, researchRetryAt(attempted, 6), "first"); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", maxEvidenceResearchErrorRunes+500)
	if err := database.RecordEvidenceResearchAttempt(ctx, competitionID, model.EvidenceCompetitionEnd, model.ResearchStateRetryable, attempted, researchRetryAt(attempted, 6), long); err != nil {
		t.Fatal(err)
	}
	got := persistedLastError(t, database, competitionID, model.EvidenceCompetitionEnd)
	if len(got) != maxEvidenceResearchErrorRunes {
		t.Fatalf("second-upsert last_error len=%d, want %d", len(got), maxEvidenceResearchErrorRunes)
	}
}

func supplementFor(field model.EvidenceField, date time.Time) EvidenceResearchSupplement {
	return EvidenceResearchSupplement{
		Field: field,
		Date:  date,
		Raw:   "2026年4月9日",
		Fact: model.FactEvidence{
			Value: "2026年4月9日", Raw: "2026年4月9日", Evidence: "报名截止时间为2026年4月9日",
			Edition: "2026", SourceURL: "https://example.com/research", Confidence: "high", ObservedAt: researchTestNow(),
		},
		RegistrationPhase: model.RegistrationClosed,
		CompetitionPhase:  model.CompetitionUnknown,
		StatusEvidence:    "报名截止时间为2026年4月9日",
	}
}

func TestApplyEvidenceResearchSupplementApplies(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "supplement-applies.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)

	supplement := supplementFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC))
	saved, applied, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, supplement)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("expected supplement to apply")
	}
	if saved.RegistrationEnd == nil || !saved.RegistrationEnd.Equal(time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("registration_end not persisted: %v", saved.RegistrationEnd)
	}
	if saved.RegistrationStartRaw != "" || saved.RegistrationEndRaw != "2026年4月9日" {
		t.Fatalf("raw not persisted: start=%q end=%q", saved.RegistrationStartRaw, saved.RegistrationEndRaw)
	}
	// Facts map must contain the mapped FactEvidence.
	fact, ok := saved.Facts[model.FactRegistrationEnd]
	if !ok || fact.SourceURL != "https://example.com/research" {
		t.Fatalf("fact not inserted: %+v", saved.Facts)
	}
}

func TestApplyEvidenceResearchSupplementDoesNotOverwrite(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "supplement-nooverwrite.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)

	// First fill registration_end.
	supplement := supplementFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC))
	if _, applied, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, supplement); err != nil || !applied {
		t.Fatalf("first apply: applied=%v err=%v", applied, err)
	}
	// Second attempt with a different date must NOT overwrite.
	other := supplementFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	if _, applied, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, other); err != nil || applied {
		t.Fatalf("overwrite must be refused: applied=%v err=%v", applied, err)
	}
	saved, err := database.GetCompetitionByID(ctx, competitionID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RegistrationEnd == nil || !saved.RegistrationEnd.Equal(time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("canonical must retain the original date, got %v", saved.RegistrationEnd)
	}
}

func TestApplyEvidenceResearchSupplementCrossFieldConflict(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "supplement-conflict.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)

	// Fill registration_start = 2026-04-20.
	start := supplementFor(model.EvidenceRegistrationStart, time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC))
	start.RegistrationPhase = model.RegistrationOpen
	if _, applied, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, start); err != nil || !applied {
		t.Fatalf("apply start: applied=%v err=%v", applied, err)
	}
	afterStart, _ := database.GetCompetitionByID(ctx, competitionID)
	if afterStart.RegistrationStart == nil || !afterStart.RegistrationStart.Equal(time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("registration_start not persisted before conflict check: %v", afterStart.RegistrationStart)
	}
	// registration_end = 2026-04-10 would be before existing start → conflict.
	end := supplementFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC))
	_, applied, endErr := database.ApplyEvidenceResearchSupplement(ctx, competitionID, end)
	if endErr == nil || applied {
		t.Fatalf("cross-field conflict must error and not apply: applied=%v err=%v", applied, endErr)
	}
	if !errors.Is(endErr, ErrEvidenceResearchSupplementConflict) {
		t.Fatalf("expected ErrEvidenceResearchSupplementConflict, got %v", endErr)
	}
}

func TestApplyEvidenceResearchSupplementUnchangedFields(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "supplement-unchanged.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)
	before, err := database.GetCompetitionByID(ctx, competitionID)
	if err != nil {
		t.Fatal(err)
	}

	supplement := supplementFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC))
	if _, applied, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, supplement); err != nil || !applied {
		t.Fatalf("apply: applied=%v err=%v", applied, err)
	}
	after, err := database.GetCompetitionByID(ctx, competitionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OfficialURL != before.OfficialURL || after.Trust != before.Trust || after.AnalyzerVersion != before.AnalyzerVersion {
		t.Fatalf("research must not change OfficialURL/Trust/AnalyzerVersion")
	}
	if after.Name != before.Name || after.EntityKey != before.EntityKey {
		t.Fatalf("research must not change identity fields")
	}
	if !after.LastSeen.Equal(before.LastSeen) || !after.FirstSeen.Equal(before.FirstSeen) {
		t.Fatalf("research must not change LastSeen/FirstSeen")
	}
}

func TestApplyEvidenceResearchSupplementInvalidInput(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "supplement-invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)

	if _, _, err := database.ApplyEvidenceResearchSupplement(ctx, 0, supplementFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC))); err == nil {
		t.Fatal("invalid competition id must error")
	}
	badField := supplementFor(model.EvidenceField("fee"), time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC))
	if _, _, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, badField); err == nil {
		t.Fatal("invalid field must error")
	}
	zero := supplementFor(model.EvidenceRegistrationEnd, time.Time{})
	if _, _, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, zero); err == nil {
		t.Fatal("zero date must error")
	}
	missing := supplementFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC))
	missing.Raw = ""
	if _, _, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, missing); err == nil {
		t.Fatal("missing raw must error")
	}
}

// TestApplyEvidenceResearchSupplementReturnsCurrentWhenTargetPopulated verifies
// that when the target field is already populated, Apply returns the reloaded
// current competition with applied=false (never an empty zero-value), so the
// Reconciler can decide same-date vs different-date from the actual canonical.
func TestApplyEvidenceResearchSupplementReturnsCurrentWhenTargetPopulated(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "supplement-race-return.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)

	first := supplementFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC))
	if _, applied, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, first); err != nil || !applied {
		t.Fatalf("first apply: applied=%v err=%v", applied, err)
	}
	// Second apply targets the now-populated field.
	second := supplementFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC))
	saved, applied, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, second)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("overwrite must be refused (applied=false)")
	}
	if saved.RegistrationEnd == nil || !saved.RegistrationEnd.Equal(time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("applied=false must return the reloaded current (original 04-09), got %v", saved.RegistrationEnd)
	}
}

// TestApplyEvidenceResearchSupplementPreservesBusinessCalendarDay verifies that a
// supplement whose date is 2026-04-09 00:00 +08:00 is persisted and reloaded as
// the SAME business calendar day 04-09, never shifted to 04-08 by a UTC
// reinterpretation. The store uses the supplement date's own location as the
// semantic authority for its calendar day.
func TestApplyEvidenceResearchSupplementPreservesBusinessCalendarDay(t *testing.T) {
	t.Parallel()
	database, err := Open(filepath.Join(t.TempDir(), "supplement-biz-day.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	competitionID := insertResearchCompetition(t, database)

	loc := time.FixedZone("CST", 8*3600)
	supplement := supplementFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 9, 0, 0, 0, 0, loc))
	if _, applied, err := database.ApplyEvidenceResearchSupplement(ctx, competitionID, supplement); err != nil || !applied {
		t.Fatalf("apply: applied=%v err=%v", applied, err)
	}
	saved, err := database.GetCompetitionByID(ctx, competitionID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RegistrationEnd == nil {
		t.Fatal("registration_end not persisted")
	}
	local := saved.RegistrationEnd.In(loc)
	if local.Year() != 2026 || local.Month() != 4 || local.Day() != 9 {
		t.Fatalf("canonical date in business location = %d-%02d-%02d, want 2026-04-09", local.Year(), local.Month(), local.Day())
	}
}
