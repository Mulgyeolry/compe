package store

import (
	"context"
	"fmt"
)

// CompetitionResetReport describes only competition-domain data. User
// accounts, preferences, sessions and verification state are deliberately not
// part of this reset.
type CompetitionResetReport struct {
	Competitions int64
	Observations int64
	Documents    int64
}

// ResetCompetitionData clears the complete discovery baseline transactionally.
// Child events, notifications, source links and user choices are removed by
// foreign-key cascades from competitions.
func (s *Store) ResetCompetitionData(ctx context.Context) (CompetitionResetReport, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CompetitionResetReport{}, err
	}
	defer tx.Rollback()
	report := CompetitionResetReport{}
	for query, destination := range map[string]*int64{
		"SELECT COUNT(*) FROM competitions":     &report.Competitions,
		"SELECT COUNT(*) FROM observations":     &report.Observations,
		"SELECT COUNT(*) FROM source_documents": &report.Documents,
	} {
		if err := tx.QueryRowContext(ctx, query).Scan(destination); err != nil {
			return CompetitionResetReport{}, fmt.Errorf("count competition reset data: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM competitions"); err != nil {
		return CompetitionResetReport{}, fmt.Errorf("clear competitions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM observations"); err != nil {
		return CompetitionResetReport{}, fmt.Errorf("clear observations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM source_documents"); err != nil {
		return CompetitionResetReport{}, fmt.Errorf("clear source documents: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM meta WHERE key='bootstrapped'"); err != nil {
		return CompetitionResetReport{}, fmt.Errorf("reset discovery baseline: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CompetitionResetReport{}, err
	}
	return report, nil
}
