package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"competition-assistant/internal/model"
)

func (s *Store) SetUserCompetitionDecision(ctx context.Context, userID, competitionID int64, decision model.ParticipationDecision, now time.Time) error {
	if userID < 1 || competitionID < 1 {
		return errors.New("invalid user or competition id")
	}
	if decision != model.ParticipationParticipating && decision != model.ParticipationDeclined {
		return fmt.Errorf("invalid participation decision %q", decision)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO user_competition_choices(user_id,competition_id,decision,updated_at)
SELECT ?,id,?,? FROM competitions WHERE id=?
ON CONFLICT(user_id,competition_id) DO UPDATE SET decision=excluded.decision,updated_at=excluded.updated_at`,
		userID, string(decision), now.Unix(), competitionID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	if decision == model.ParticipationDeclined {
		if _, err := tx.ExecContext(ctx, `UPDATE user_notifications SET status='cancelled',last_error=''
WHERE user_id=? AND competition_id=? AND status IN ('pending','failed')`, userID, competitionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) GetUserCompetitionDecision(ctx context.Context, userID, competitionID int64) (model.ParticipationDecision, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT decision FROM user_competition_choices WHERE user_id=? AND competition_id=?`, userID, competitionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ParticipationUndecided, nil
	}
	if err != nil {
		return "", err
	}
	decision := model.ParticipationDecision(raw)
	if decision != model.ParticipationParticipating && decision != model.ParticipationDeclined {
		return "", fmt.Errorf("database contains invalid participation decision %q", raw)
	}
	return decision, nil
}

func (s *Store) ListUserCompetitionDecisions(ctx context.Context, userID int64) (map[int64]model.ParticipationDecision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT competition_id,decision FROM user_competition_choices WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]model.ParticipationDecision)
	for rows.Next() {
		var competitionID int64
		var raw string
		if err := rows.Scan(&competitionID, &raw); err != nil {
			return nil, err
		}
		decision := model.ParticipationDecision(raw)
		if decision != model.ParticipationParticipating && decision != model.ParticipationDeclined {
			return nil, fmt.Errorf("database contains invalid participation decision %q", raw)
		}
		result[competitionID] = decision
	}
	return result, rows.Err()
}
