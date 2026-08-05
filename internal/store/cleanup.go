package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RetentionPolicy describes data that may be safely removed or compacted.
// Competition facts, source URLs, event keys, and per-user notification keys
// are intentionally outside this policy because they are required for audit
// and duplicate prevention.
type RetentionPolicy struct {
	ObservationAge              time.Duration
	ClosedCompetitionContentAge time.Duration
	ExpiredAuthenticationAge    time.Duration
}

type CleanupReport struct {
	ObservationsDeleted      int64
	CompetitionsCompacted    int64
	VerificationCodesDeleted int64
	SessionsDeleted          int64
}

func (r CleanupReport) Changed() bool {
	return r.ObservationsDeleted+r.CompetitionsCompacted+
		r.VerificationCodesDeleted+r.SessionsDeleted > 0
}

// Cleanup performs one transaction so a cancellation or failure cannot leave
// a partially applied retention pass. For observations, the newest snapshot of
// every source URL is always retained even when it is older than the cutoff.
func (s *Store) Cleanup(ctx context.Context, policy RetentionPolicy, now time.Time) (CleanupReport, error) {
	if err := validateRetentionPolicy(policy); err != nil {
		return CleanupReport{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CleanupReport{}, err
	}
	defer tx.Rollback()

	var report CleanupReport
	observationCutoff := now.Add(-policy.ObservationAge).Unix()
	result, err := tx.ExecContext(ctx, `DELETE FROM observations
WHERE seen_at < ?
  AND id NOT IN (SELECT MAX(id) FROM observations GROUP BY source_id,url)`, observationCutoff)
	if err != nil {
		return CleanupReport{}, fmt.Errorf("delete old observations: %w", err)
	}
	if report.ObservationsDeleted, err = rowsAffected(result); err != nil {
		return CleanupReport{}, err
	}

	competitionCutoff := now.Add(-policy.ClosedCompetitionContentAge).Unix()
	result, err = tx.ExecContext(ctx, `UPDATE competitions SET content=''
WHERE status IN ('registration_closed','finished')
  AND last_seen < ? AND content <> ''
  AND NOT EXISTS (
      SELECT 1 FROM user_notifications n
      WHERE n.competition_id=competitions.id AND n.status IN ('pending','failed')
  )`, competitionCutoff)
	if err != nil {
		return CleanupReport{}, fmt.Errorf("compact closed competitions: %w", err)
	}
	if report.CompetitionsCompacted, err = rowsAffected(result); err != nil {
		return CleanupReport{}, err
	}

	authCutoff := now.Add(-policy.ExpiredAuthenticationAge).Unix()
	result, err = tx.ExecContext(ctx, `DELETE FROM verification_codes WHERE expires_at < ?`, authCutoff)
	if err != nil {
		return CleanupReport{}, fmt.Errorf("delete expired verification codes: %w", err)
	}
	if report.VerificationCodesDeleted, err = rowsAffected(result); err != nil {
		return CleanupReport{}, err
	}
	result, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, authCutoff)
	if err != nil {
		return CleanupReport{}, fmt.Errorf("delete expired sessions: %w", err)
	}
	if report.SessionsDeleted, err = rowsAffected(result); err != nil {
		return CleanupReport{}, err
	}

	if err := tx.Commit(); err != nil {
		return CleanupReport{}, err
	}
	// Optimize planner statistics after deleting rows. SQLite will reuse freed
	// pages automatically; avoiding VACUUM here prevents a long exclusive lock.
	if _, err := s.db.ExecContext(ctx, `PRAGMA optimize`); err != nil {
		return report, fmt.Errorf("optimize database after cleanup: %w", err)
	}
	return report, nil
}

func validateRetentionPolicy(policy RetentionPolicy) error {
	if policy.ObservationAge <= 0 || policy.ClosedCompetitionContentAge <= 0 || policy.ExpiredAuthenticationAge <= 0 {
		return fmt.Errorf("all retention durations must be positive")
	}
	return nil
}

func rowsAffected(result sql.Result) (int64, error) {
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read cleanup affected row count: %w", err)
	}
	return count, nil
}
