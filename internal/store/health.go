package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// healthKey returns the meta-table key holding a source's consecutive-failure
// count. The count survives restarts so a source that has been broken for
// several scan cycles can trigger an operator alert without re-arming on
// every process start.
func healthKey(sourceID string) string { return "source_health:" + sourceID }

// GetSourceConsecutiveFailures reads the number of consecutive failed scan
// cycles recorded for a source. A source that has never failed returns zero.
func (s *Store) GetSourceConsecutiveFailures(ctx context.Context, sourceID string) (int, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, healthKey(sourceID)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	failures, err := strconv.Atoi(value)
	if err != nil || failures < 0 {
		return 0, nil
	}
	return failures, nil
}

// RecordSourceResult updates a source's consecutive-failure counter and
// returns the new count. A success resets the counter to zero; a failure
// increments it. The returned count lets the caller decide whether to fire an
// alert for this scan cycle.
func (s *Store) RecordSourceResult(ctx context.Context, sourceID string, ok bool) (int, error) {
	current, err := s.GetSourceConsecutiveFailures(ctx, sourceID)
	if err != nil {
		return 0, err
	}
	next := 0
	if !ok {
		next = current + 1
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=?`,
		healthKey(sourceID), strconv.Itoa(next), strconv.Itoa(next))
	if err != nil {
		return 0, err
	}
	return next, nil
}

// SourceAlertState records whether an operator alert has already been fired
// for a source at a particular failure count, so the same outage is reported
// once instead of on every scan cycle.
type SourceAlertState struct {
	LastAlertedFailures int
}

// GetSourceAlertState returns the failure count at which a source was last
// alerted, or zero if it was never alerted.
func (s *Store) GetSourceAlertState(ctx context.Context, sourceID string) (SourceAlertState, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, alertStateKey(sourceID)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceAlertState{}, nil
	}
	if err != nil {
		return SourceAlertState{}, err
	}
	failures, err := strconv.Atoi(value)
	if err != nil || failures < 0 {
		return SourceAlertState{}, nil
	}
	return SourceAlertState{LastAlertedFailures: failures}, nil
}

// SetSourceAlertState records that an alert was fired for a source. Passing
// zero clears the alert state after the source recovers.
func (s *Store) SetSourceAlertState(ctx context.Context, sourceID string, failures int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=?`,
		alertStateKey(sourceID), strconv.Itoa(failures), strconv.Itoa(failures))
	return err
}

func alertStateKey(sourceID string) string { return "source_alerted:" + sourceID }
