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
