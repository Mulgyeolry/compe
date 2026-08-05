package service

import (
	"context"
	"fmt"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/notifier"
)

// recordSourceHealth updates a source's persistent consecutive-failure count
// after a scan cycle. A success clears any prior alert; a failure is recorded
// so the source can be flagged once it crosses the configured threshold.
func (s *Service) recordSourceHealth(ctx context.Context, source config.Source, ok bool) {
	if !s.cfg.Alert.Enabled {
		return
	}
	failures, err := s.store.RecordSourceResult(ctx, source.ID, ok)
	if err != nil {
		s.log.Warn("source health tracking failed", "source", source.ID, "error", err)
		return
	}
	if ok && failures == 0 {
		if err := s.store.SetSourceAlertState(ctx, source.ID, 0); err != nil {
			s.log.Warn("source alert state reset failed", "source", source.ID, "error", err)
		}
	}
}

// notifyUnhealthySources sends one operator alert listing every source whose
// consecutive failures have crossed the configured threshold and that has not
// already been reported at this failure level. The alert is fired at most once
// per outage; it re-arms only after the source recovers.
func (s *Service) notifyUnhealthySources(ctx context.Context, now time.Time) error {
	if !s.cfg.Alert.Enabled || s.notifier == nil {
		return nil
	}
	limit := s.cfg.Alert.ConsecutiveFailureLimit
	var problems []notifier.SourceHealthProblem
	for _, source := range s.cfg.Sources {
		failures, err := s.store.GetSourceConsecutiveFailures(ctx, source.ID)
		if err != nil {
			return err
		}
		if failures < limit {
			continue
		}
		state, err := s.store.GetSourceAlertState(ctx, source.ID)
		if err != nil {
			return err
		}
		// Report once per outage level; a growing outage is not re-paged.
		if state.LastAlertedFailures >= failures {
			continue
		}
		problems = append(problems, notifier.SourceHealthProblem{
			ID:           source.ID,
			Name:         source.Name,
			FailureCount: failures,
			FailureLimit: limit,
		})
	}
	if len(problems) == 0 {
		return nil
	}
	subject, body, err := notifier.RenderSourceAlert(problems, now.In(s.cfg.Location).Format("2006-01-02 15:04:05 MST"))
	if err != nil {
		return err
	}
	if err := s.notifier.Send(ctx, subject, body); err != nil {
		return fmt.Errorf("send source health alert: %w", err)
	}
	for _, problem := range problems {
		if err := s.store.SetSourceAlertState(ctx, problem.ID, problem.FailureCount); err != nil {
			s.log.Warn("source alert state persist failed", "source", problem.ID, "error", err)
		}
	}
	s.log.Warn("source health alert sent", "sources", len(problems))
	return nil
}
