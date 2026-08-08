package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"competition-assistant/internal/model"
)

// maxEvidenceResearchErrorRunes caps how much of a research attempt's error
// diagnostic is persisted. The evidence_research_state table is a scheduling
// metadata store, so last_error must never hold page bodies or large LLM output
// that an error chain might accidentally include. The limit is measured in
// runes so multi-byte UTF-8 (e.g. Chinese) is never split mid-character.
const maxEvidenceResearchErrorRunes = 500

// truncateEvidenceResearchError normalizes an error diagnostic before it is
// persisted: surrounding whitespace is trimmed and the value is truncated to
// maxEvidenceResearchErrorRunes runes. Truncation is rune-aware, never a byte
// slice cut, so the result is always valid UTF-8.
func truncateEvidenceResearchError(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= maxEvidenceResearchErrorRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxEvidenceResearchErrorRunes])
}

// ListEvidenceResearchStates returns every recorded evidence-research scheduling
// state, ordered by (competition_id, field) for deterministic output. A
// competition/field pair with no row simply means that gap has never been
// researched (implicitly pending); the canonical Competition remains the source
// of truth for whether a gap exists.
func (s *Store) ListEvidenceResearchStates(ctx context.Context) ([]model.EvidenceResearchState, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT competition_id, field, status, attempt_count, last_attempt_at, next_retry_at, last_error, created_at, updated_at
FROM evidence_research_state
ORDER BY competition_id, field`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var states []model.EvidenceResearchState
	for rows.Next() {
		var state model.EvidenceResearchState
		var lastAttempt, nextRetry sql.NullInt64
		var createdAt, updatedAt int64
		if err := rows.Scan(&state.CompetitionID, &state.Field, &state.Status, &state.AttemptCount,
			&lastAttempt, &nextRetry, &state.LastError, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		state.LastAttemptAt = nullTime(lastAttempt)
		state.NextRetryAt = nullTime(nextRetry)
		state.CreatedAt = time.Unix(createdAt, 0)
		state.UpdatedAt = time.Unix(updatedAt, 0)
		states = append(states, state)
	}
	return states, rows.Err()
}

// RecordEvidenceResearchAttempt records one research attempt outcome for a
// competition field using an UPSERT. The first record for a pair sets
// attempt_count to 1; subsequent records increment it (never reset to 1). For
// retryable/unresolved outcomes, next_retry_at is required and must be strictly
// after attempted_at; for resolved/skipped it must be nil. last_error only ever
// holds a short diagnostic string, never page body or large LLM output.
func (s *Store) RecordEvidenceResearchAttempt(
	ctx context.Context,
	competitionID int64,
	field model.EvidenceField,
	status model.ResearchStateStatus,
	attemptedAt time.Time,
	nextRetryAt *time.Time,
	lastError string,
) error {
	if competitionID < 1 {
		return errors.New("invalid competition id")
	}
	if !model.ValidEvidenceField(field) {
		return fmt.Errorf("invalid evidence field %q", field)
	}
	if !model.ValidResearchStateStatus(status) {
		return fmt.Errorf("invalid research state status %q", status)
	}
	switch status {
	case model.ResearchStateRetryable, model.ResearchStateUnresolved:
		if nextRetryAt == nil || !nextRetryAt.After(attemptedAt) {
			return fmt.Errorf("%s requires next_retry_at strictly after attempted_at", status)
		}
	case model.ResearchStateResolved, model.ResearchStateSkipped:
		if nextRetryAt != nil {
			return fmt.Errorf("%s must not carry a next_retry_at", status)
		}
	}

	// Enforce the scheduling-metadata boundary here, at the Store edge, so every
	// insert/upsert goes through the same truncation regardless of who the caller
	// is. The single UPSERT uses the same value for both the insert and the
	// conflict-update branch, so there is exactly one boundary.
	lastError = truncateEvidenceResearchError(lastError)

	_, err := s.db.ExecContext(ctx, `
INSERT INTO evidence_research_state
    (competition_id, field, status, attempt_count, last_attempt_at, next_retry_at, last_error, created_at, updated_at)
VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?)
ON CONFLICT(competition_id, field) DO UPDATE SET
    status = excluded.status,
    attempt_count = evidence_research_state.attempt_count + 1,
    last_attempt_at = excluded.last_attempt_at,
    next_retry_at = excluded.next_retry_at,
    last_error = excluded.last_error,
    updated_at = excluded.updated_at`,
		competitionID, string(field), string(status),
		attemptedAt.Unix(), nullableTime(nextRetryAt), lastError,
		attemptedAt.Unix(), attemptedAt.Unix())
	if err != nil {
		return fmt.Errorf("record evidence research attempt: %w", err)
	}
	return nil
}

// nullTime reconstructs a *time.Time from a nullable unix INTEGER column value.
func nullTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	result := time.Unix(value.Int64, 0)
	return &result
}

// ErrEvidenceResearchSupplementConflict marks a deterministic contradiction
// between an incoming research supplement and the canonical lifecycle (e.g.
// registration_end before the existing registration_start). The reconciler must
// treat this as a long-cooldown unresolved outcome, not a transient retryable.
var ErrEvidenceResearchSupplementConflict = errors.New("evidence research supplement conflicts with canonical lifecycle")

// EvidenceResearchSupplement is the narrow, atomic write requested by the
// reconciler for a single missing lifecycle date field. It deliberately carries
// only the date + raw + fact evidence and the research-derived lifecycle phases;
// it can never touch Name/Organizer/OfficialURL/Trust/AnalyzerVersion/ContentHash/
// EntityKey/FirstSeen/LastSeen/ProblemReleased/etc.
type EvidenceResearchSupplement struct {
	Field             model.EvidenceField
	Date              time.Time
	Raw               string
	Fact              model.FactEvidence
	RegistrationPhase model.RegistrationPhase
	CompetitionPhase  model.CompetitionPhase
	StatusEvidence    string
}

// ApplyEvidenceResearchSupplement atomically supplements one missing lifecycle
// date field of a canonical competition. It reloads the canonical inside the
// transaction, refuses to overwrite a non-nil target, enforces cross-field
// consistency, merges the FactEvidence, and writes the date/raw/facts/lifecycle
// in one commit (no half-state). LastSeen and every other non-research column
// are untouched. It returns the saved competition and whether the write applied.
func (s *Store) ApplyEvidenceResearchSupplement(ctx context.Context, competitionID int64, supplement EvidenceResearchSupplement) (model.Competition, bool, error) {
	if competitionID < 1 {
		return model.Competition{}, false, errors.New("invalid competition id")
	}
	if !model.ValidEvidenceField(supplement.Field) {
		return model.Competition{}, false, fmt.Errorf("invalid evidence field %q", supplement.Field)
	}
	if supplement.Date.IsZero() {
		return model.Competition{}, false, errors.New("evidence supplement requires a non-zero date")
	}
	if strings.TrimSpace(supplement.Raw) == "" || strings.TrimSpace(supplement.Fact.Evidence) == "" ||
		strings.TrimSpace(supplement.Fact.SourceURL) == "" || strings.TrimSpace(supplement.Fact.Edition) == "" {
		return model.Competition{}, false, errors.New("evidence supplement requires raw, evidence, source_url and edition")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Competition{}, false, err
	}
	defer tx.Rollback()

	current, err := loadCompetition(tx.QueryRowContext(ctx, competitionSelect+` WHERE id=?`, competitionID))
	if err != nil {
		return model.Competition{}, false, err
	}

	// Target field must still be nil; research supplements missing facts and
	// must never overwrite an existing value.
	if !researchFieldNil(current, supplement.Field) {
		return model.Competition{}, false, nil
	}

	// Cross-field consistency against the existing canonical lifecycle.
	if err := researchSupplementConsistency(current, supplement); err != nil {
		return model.Competition{}, false, fmt.Errorf("%w: %v", ErrEvidenceResearchSupplementConflict, err)
	}

	// Build the updated in-memory competition and write the narrow columns.
	next := current
	applyResearchDate(&next, supplement.Field, supplement.Date, supplement.Raw)
	next.Facts = cloneFacts(current.Facts)
	if next.Facts == nil {
		next.Facts = make(map[string]model.FactEvidence)
	}
	next.Facts[string(supplement.Field)] = supplement.Fact
	next.RegistrationPhase = supplement.RegistrationPhase
	next.CompetitionPhase = supplement.CompetitionPhase
	next.StatusEvidence = supplement.StatusEvidence
	next.Status = model.CompositeStatus(next.RegistrationPhase, next.CompetitionPhase)

	res, err := tx.ExecContext(ctx, `
UPDATE competitions SET
    registration_start=?, registration_start_raw=?,
    registration_end=?, registration_end_raw=?,
    competition_start=?, competition_start_raw=?,
    competition_end=?, competition_end_raw=?,
    facts_json=?, registration_phase=?, competition_phase=?, status=?, status_evidence=?
WHERE id=?`,
		nullableTime(next.RegistrationStart), next.RegistrationStartRaw,
		nullableTime(next.RegistrationEnd), next.RegistrationEndRaw,
		nullableTime(next.CompetitionStart), next.CompetitionStartRaw,
		nullableTime(next.CompetitionEnd), next.CompetitionEndRaw,
		encodeJSON(next.Facts, "{}"), string(next.RegistrationPhase), string(next.CompetitionPhase), string(next.Status), next.StatusEvidence,
		competitionID)
	if err != nil {
		return model.Competition{}, false, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return model.Competition{}, false, errors.New("evidence research supplement affected no rows")
	}
	if err := tx.Commit(); err != nil {
		return model.Competition{}, false, err
	}

	saved, err := s.GetCompetitionByID(ctx, competitionID)
	if err != nil {
		return model.Competition{}, false, err
	}
	return saved, true, nil
}

// researchFieldNil reports whether the target field is currently nil on the
// competition.
func researchFieldNil(competition model.Competition, field model.EvidenceField) bool {
	switch field {
	case model.EvidenceRegistrationStart:
		return competition.RegistrationStart == nil
	case model.EvidenceRegistrationEnd:
		return competition.RegistrationEnd == nil
	case model.EvidenceCompetitionStart:
		return competition.CompetitionStart == nil
	case model.EvidenceCompetitionEnd:
		return competition.CompetitionEnd == nil
	default:
		return true
	}
}

// applyResearchDate sets the target date column and raw on a copy.
func applyResearchDate(competition *model.Competition, field model.EvidenceField, date time.Time, raw string) {
	switch field {
	case model.EvidenceRegistrationStart:
		competition.RegistrationStart = &date
		competition.RegistrationStartRaw = raw
	case model.EvidenceRegistrationEnd:
		competition.RegistrationEnd = &date
		competition.RegistrationEndRaw = raw
	case model.EvidenceCompetitionStart:
		competition.CompetitionStart = &date
		competition.CompetitionStartRaw = raw
	case model.EvidenceCompetitionEnd:
		competition.CompetitionEnd = &date
		competition.CompetitionEndRaw = raw
	}
}

// researchDay normalizes a time to its UTC calendar day. Dates are stored as
// unix instants and reloaded in UTC, so all calendar comparisons must use UTC
// days to be independent of the timezone a caller constructed the date with.
func researchDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// researchSupplementConsistency checks the incoming date against the existing
// canonical lifecycle (registration_start <= registration_end and
// competition_start <= competition_end). Comparison is on UTC calendar days.
func researchSupplementConsistency(competition model.Competition, supplement EvidenceResearchSupplement) error {
	date := researchDay(supplement.Date)
	switch supplement.Field {
	case model.EvidenceRegistrationStart:
		if competition.RegistrationEnd != nil && researchDay(*competition.RegistrationEnd).Before(date) {
			return errors.New("registration_start would be after existing registration_end")
		}
	case model.EvidenceRegistrationEnd:
		if competition.RegistrationStart != nil && researchDay(*competition.RegistrationStart).After(date) {
			return errors.New("registration_end would be before existing registration_start")
		}
	case model.EvidenceCompetitionStart:
		if competition.CompetitionEnd != nil && researchDay(*competition.CompetitionEnd).Before(date) {
			return errors.New("competition_start would be after existing competition_end")
		}
	case model.EvidenceCompetitionEnd:
		if competition.CompetitionStart != nil && researchDay(*competition.CompetitionStart).After(date) {
			return errors.New("competition_end would be before existing competition_start")
		}
	}
	return nil
}

