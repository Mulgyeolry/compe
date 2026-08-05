package service

import (
	"context"
	"errors"

	"competition-assistant/internal/model"
	"competition-assistant/internal/store"
	"competition-assistant/internal/subscription"
)

// BackfillUser queues the current actionable state of competitions that match
// the user's latest preferences. It performs no crawling and is idempotent.
func (s *Service) BackfillUser(ctx context.Context, userID int64) (int, error) {
	if userID < 1 {
		return 0, errors.New("invalid user id")
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	now := s.now().In(s.cfg.Location)
	preferences, err := s.store.GetUserPreferences(ctx, userID)
	if err != nil {
		return 0, err
	}
	competitions, err := s.store.ListActiveCompetitions(ctx)
	if err != nil {
		return 0, err
	}
	dueAt, err := subscription.NextDelivery(now, preferences)
	if err != nil {
		return 0, err
	}
	inserted := 0
	for _, competition := range competitions {
		if !actionable(competition, now, s.freshnessWindow()) {
			continue
		}
		keywords := s.analyzer.KeywordsForCompetition(competition)
		if !sameStrings(competition.Keywords, keywords) {
			competition.Keywords = keywords
			if err := s.store.UpdateCompetitionEnrichment(ctx, competition.ID, keywords, competition.Analysis); err != nil {
				return inserted, err
			}
		}
		events := backfillEvents(competition, now, s.cfg.Location, s.freshnessWindow())
		decision, err := s.store.GetUserCompetitionDecision(ctx, userID, competition.ID)
		if err != nil {
			return inserted, err
		}
		matched := subscription.MatchingEventsForUser(preferences, competition, subscription.Profile(competition), events, decision, now)
		if len(matched) == 0 {
			continue
		}
		nonce := "backfill:" + matched[0].Type + ":" + matched[0].Key
		group := subscription.DeliveryGroupKey(userID, competition.ID, preferences.Frequency, dueAt, nonce)
		dispatches := make([]store.UserEventDispatch, 0, len(matched))
		for _, event := range matched {
			dispatches = append(dispatches, store.UserEventDispatch{UserID: userID, Event: event, GroupKey: group, DueAt: dueAt})
		}
		count, err := s.store.EnqueueUserCompetitionEvents(ctx, competition.ID, dispatches, now)
		if err != nil {
			return inserted, err
		}
		inserted += count
	}
	return inserted, nil
}

// SetUserCompetitionDecision records whether the user will take part in a
// competition and queues the immediate events that follow from a "participating"
// choice (upcoming / started / problem-released).
func (s *Service) SetUserCompetitionDecision(ctx context.Context, userID, competitionID int64, decision model.ParticipationDecision) error {
	if userID < 1 || competitionID < 1 {
		return errors.New("invalid user or competition id")
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	now := s.now().In(s.cfg.Location)
	competition, err := s.store.GetCompetitionByID(ctx, competitionID)
	if err != nil {
		return err
	}
	if decision == model.ParticipationParticipating && competitionEnded(competition, now) {
		return ErrCompetitionExpired
	}
	if err := s.store.SetUserCompetitionDecision(ctx, userID, competitionID, decision, now); err != nil {
		return err
	}
	model.NormalizeLifecycle(&competition)
	if decision != model.ParticipationParticipating {
		return nil
	}
	preferences, err := s.store.GetUserPreferences(ctx, userID)
	if err != nil {
		return err
	}
	var currentEvents []model.Event
	if competition.CompetitionPhase == model.CompetitionUpcoming {
		currentEvents = append(currentEvents, model.Event{Type: "competition_upcoming", Key: "upcoming"})
	}
	if competition.CompetitionPhase == model.CompetitionOngoing {
		currentEvents = append(currentEvents, model.Event{Type: "competition_started", Key: "started"})
	}
	if competition.ProblemReleased {
		currentEvents = append(currentEvents, model.Event{Type: "problem_released", Key: "problem_released"})
	}
	matched := subscription.MatchingEventsForUser(preferences, competition, subscription.Profile(competition), currentEvents, decision, now)
	if len(matched) == 0 {
		return nil
	}
	dueAt, err := subscription.NextDelivery(now, preferences)
	if err != nil {
		return err
	}
	group := subscription.DeliveryGroupKey(userID, competitionID, preferences.Frequency, dueAt, "choice:"+matched[0].Type+":"+matched[0].Key)
	dispatches := make([]store.UserEventDispatch, 0, len(matched))
	for _, event := range matched {
		dispatches = append(dispatches, store.UserEventDispatch{UserID: userID, Event: event, GroupKey: group, DueAt: dueAt})
	}
	_, err = s.store.EnqueueUserCompetitionEvents(ctx, competitionID, dispatches, now)
	return err
}
