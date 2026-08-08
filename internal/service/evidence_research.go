package service

import (
	"context"
	"sort"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/fetcher"
	"competition-assistant/internal/model"
	"competition-assistant/internal/subscription"
)

// evidenceFieldsFixedOrder is the stable order in which date evidence gaps are
// reported so session output never varies between runs.
var evidenceFieldsFixedOrder = []model.EvidenceField{
	model.EvidenceRegistrationStart,
	model.EvidenceRegistrationEnd,
	model.EvidenceCompetitionStart,
	model.EvidenceCompetitionEnd,
}

// detectEvidenceGaps is a pure, deterministic V1 gap detector. It only checks
// the four date fields; fee, team requirement, eligibility, organizer and any
// other field are deliberately out of scope. The returned slice is stable:
// registration_start, registration_end, competition_start, competition_end.
func detectEvidenceGaps(c model.Competition) []model.EvidenceGap {
	var gaps []model.EvidenceGap
	dates := []*time.Time{c.RegistrationStart, c.RegistrationEnd, c.CompetitionStart, c.CompetitionEnd}
	for index, field := range evidenceFieldsFixedOrder {
		if dates[index] == nil {
			gaps = append(gaps, model.EvidenceGap{Field: field, Reason: model.ResearchReasonMissing})
		}
	}
	return gaps
}

// evidenceResearchEligible decides, purely deterministically, whether a
// canonical competition may enter an evidence research session in the current
// pass. It reuses the existing catalog/subscription gates rather than duplicating
// "current edition" or "ended" logic:
//
//   - the competition still passes subscription.CatalogEligible;
//   - trust is not low;
//   - the competition has not finished (competitionEnded);
//   - it still belongs to the current edition / valid time range (isCurrentEdition
//     with the service freshness window);
//   - there is at least one evidence gap.
func evidenceResearchEligible(competition model.Competition, now time.Time, freshness time.Duration) bool {
	if !subscription.CatalogEligible(competition) || competition.Trust == model.TrustLow {
		return false
	}
	if competitionEnded(competition, now) {
		return false
	}
	if !isCurrentEdition(competition, now, freshness) {
		return false
	}
	return len(detectEvidenceGaps(competition)) > 0
}

// buildEvidenceResearchSessions maps eligible canonical competitions to
// research sessions, one session per competition. Even a competition missing
// all four date fields produces exactly one session whose Gaps hold four items.
// Output order is stable (sorted by CompetitionID) so tests never flake.
func buildEvidenceResearchSessions(competitions []model.Competition, now time.Time, freshness time.Duration) []model.ResearchSession {
	eligible := make([]model.Competition, 0, len(competitions))
	for _, competition := range competitions {
		if evidenceResearchEligible(competition, now, freshness) {
			eligible = append(eligible, competition)
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].ID < eligible[j].ID })

	sessions := make([]model.ResearchSession, 0, len(eligible))
	for _, competition := range eligible {
		sessions = append(sessions, model.ResearchSession{
			CompetitionID: competition.ID,
			Gaps:          detectEvidenceGaps(competition),
		})
	}
	return sessions
}

// researchNextRetryAt is the centralized, pure cooldown policy. The research
// executor only reports an outcome; the system decides when a field may be
// researched again. retryable (transient failure) gets a short cooldown,
// unresolved (real attempt, no reliable evidence) a long cooldown, and resolved
// / skipped are terminal with no retry.
func researchNextRetryAt(status model.ResearchStateStatus, now time.Time, cfg config.EvidenceResearch) *time.Time {
	switch status {
	case model.ResearchStateRetryable:
		value := now.Add(time.Duration(cfg.RetryCooldownHours) * time.Hour)
		return &value
	case model.ResearchStateUnresolved:
		value := now.Add(time.Duration(cfg.UnresolvedCooldownHours) * time.Hour)
		return &value
	default: // resolved, skipped
		return nil
	}
}

// researchStateKey addresses one evidence-research state row.
type researchStateKey struct {
	competitionID int64
	field         model.EvidenceField
}

// researchPlanStats holds the scheduling accounting for one planning pass so the
// phase can log a compact picture of what is due and what is deferred.
type researchPlanStats struct {
	detectedSessions int
	detectedGaps     int
	dueSessions      int
	dueGaps          int
	deferredCooldown int
	deferredBudget   int
}

// planDueResearchSessions is the pure, deterministic due-research planner. It
// takes the eligible sessions from buildEvidenceResearchSessions plus the
// recorded EvidenceResearchState and decides, field-by-field, which gaps are due
// this round, then applies the per-run competition budget. Field-level rules:
//
//   - no state for a gap → never researched → due now;
//   - retryable / unresolved → due when next_retry_at <= now, otherwise cooldown;
//   - resolved / skipped → the canonical field is still nil (gap exists), so the
//     historical terminal state must NOT permanently lock it out; it is re-due.
//
// A session contains only its due gaps, so one field's cooldown is never
// bypassed by another gap. A competition counts as one budget unit regardless of
// how many due gaps it has. Ordering is deterministic: never-researched sessions
// first, then due-retry sessions, tie-broken by CompetitionID.
func planDueResearchSessions(sessions []model.ResearchSession, states []model.EvidenceResearchState, now time.Time, maxCompetitionsPerRun int) ([]model.ResearchSession, researchPlanStats) {
	stateByKey := make(map[researchStateKey]model.EvidenceResearchState, len(states))
	for _, state := range states {
		stateByKey[researchStateKey{competitionID: state.CompetitionID, field: state.Field}] = state
	}

	var stats researchPlanStats
	stats.detectedSessions = len(sessions)
	for _, session := range sessions {
		stats.detectedGaps += len(session.Gaps)
	}

	type candidate struct {
		session         model.ResearchSession
		neverResearched bool
	}
	var candidates []candidate
	for _, session := range sessions {
		var dueGaps []model.EvidenceGap
		hasNeverResearched := false
		hasDue := false
		for _, gap := range session.Gaps {
			state, ok := stateByKey[researchStateKey{competitionID: session.CompetitionID, field: gap.Field}]
			if !ok {
				dueGaps = append(dueGaps, gap)
				hasNeverResearched = true
				hasDue = true
				continue
			}
			switch state.Status {
			case model.ResearchStateRetryable, model.ResearchStateUnresolved:
				if state.NextRetryAt != nil && !state.NextRetryAt.After(now) {
					dueGaps = append(dueGaps, gap)
					hasDue = true
				}
				// else: next_retry_at in the future → cooldown, gap is not due.
			case model.ResearchStateResolved, model.ResearchStateSkipped:
				// The gap still exists (canonical field == nil), so the old
				// terminal state must not permanently block re-research.
				dueGaps = append(dueGaps, gap)
				hasDue = true
			}
		}
		if !hasDue {
			stats.deferredCooldown++
			continue
		}
		candidates = append(candidates, candidate{
			session:         model.ResearchSession{CompetitionID: session.CompetitionID, Gaps: dueGaps},
			neverResearched: hasNeverResearched,
		})
	}

	// Deterministic ordering: never-researched first, then due-retry; stable by
	// CompetitionID. No map iteration drives the order.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].neverResearched != candidates[j].neverResearched {
			return candidates[i].neverResearched
		}
		return candidates[i].session.CompetitionID < candidates[j].session.CompetitionID
	})

	result := make([]model.ResearchSession, 0, len(candidates))
	for index, c := range candidates {
		if index >= maxCompetitionsPerRun {
			stats.deferredBudget++
			continue
		}
		result = append(result, c.session)
	}
	stats.dueSessions = len(result)
	for _, session := range result {
		stats.dueGaps += len(session.Gaps)
	}
	return result, stats
}

// runEvidenceResearchPlanning is the standalone Evidence Research planning
// phase. It is deliberately independent of processCandidate and of the
// observation "changed" decision: a canonical competition whose original page
// did not change this round can still be planned for research when it is
// missing a date. It reads the full canonical set, builds eligible sessions,
// filters by recorded research state and cooldown, applies the per-run budget,
// and logs scheduling stats. It never searches, fetches, calls the LLM, mutates
// canonical facts, produces events or touches notifications. Crucially, it must
// NOT call RecordEvidenceResearchAttempt: being planned is not the same as
// having executed a research attempt, so attempt_count is only ever incremented
// by a future research executor.
// prepareEvidenceResearch is the shared planning core used by both the
// read-only planning pass and the production research phase. It lists the full
// canonical set, builds eligible sessions, applies cooldown + budget, and
// returns the due sessions alongside the competitions keyed by id.
func (s *Service) prepareEvidenceResearch(ctx context.Context, now time.Time) ([]model.ResearchSession, map[int64]model.Competition, researchPlanStats, error) {
	// Read the full canonical set, not the UI/active-list subset. ListActiveCompetitions
	// pre-filters by status (preview/upcoming/registration_open/ongoing), which would
	// drop the canonicals most in need of evidence research: StatusUnknown (all dates
	// unknown) and StatusRegistrationClosed (registration over but competition dates
	// still missing). Research eligibility itself is decided later by
	// evidenceResearchEligible, so no status pre-filter belongs here.
	competitions, err := s.store.ListCompetitions(ctx)
	if err != nil {
		return nil, nil, researchPlanStats{}, err
	}
	byID := make(map[int64]model.Competition, len(competitions))
	for _, competition := range competitions {
		byID[competition.ID] = competition
	}
	sessions := buildEvidenceResearchSessions(competitions, now, s.freshnessWindow())
	states, err := s.store.ListEvidenceResearchStates(ctx)
	if err != nil {
		return nil, nil, researchPlanStats{}, err
	}
	dueSessions, stats := planDueResearchSessions(sessions, states, now, s.cfg.EvidenceResearch.MaxCompetitionsPerRun)
	return dueSessions, byID, stats, nil
}

// runEvidenceResearchPlanning is the read-only planning pass. It shares
// prepareEvidenceResearch with the production phase but performs no Search/Fetch/
// Extractor/ResearchState/canonical work.
func (s *Service) runEvidenceResearchPlanning(ctx context.Context, now time.Time) error {
	dueSessions, _, stats, err := s.prepareEvidenceResearch(ctx, now)
	if err != nil {
		return err
	}
	s.log.Info("evidence research planning",
		"detected_sessions", stats.detectedSessions,
		"detected_gaps", stats.detectedGaps,
		"due_sessions", stats.dueSessions,
		"due_gaps", stats.dueGaps,
		"deferred_cooldown", stats.deferredCooldown,
		"deferred_budget", stats.deferredBudget)
	_ = dueSessions
	return nil
}

// evidenceResearchPhaseTimeout caps the whole production research phase across
// all sessions. Sessions run sequentially (no worker pool).
const evidenceResearchPhaseTimeout = 120 * time.Second

// runEvidenceResearch is the production research phase. It only runs when
// EvidenceResearch.Enabled is true and the collector exposes ResearchTools. It
// plans due sessions, executes each sequentially, reconciles each found fact
// into a narrow canonical supplement, records ResearchState, and feeds accepted
// lifecycle changes into the existing eventMap. It is fail-soft: errors are
// logged and must never break the main source scan.
func (s *Service) runEvidenceResearch(ctx context.Context, now time.Time, eventMap map[int64][]model.Event) error {
	if !s.cfg.EvidenceResearch.Enabled {
		return nil
	}
	tools, ok := s.collector.(fetcher.ResearchTools)
	if !ok {
		s.log.Warn("evidence research skipped: collector does not expose ResearchTools")
		return nil
	}
	if !s.analyzer.ResearchEnabled() {
		s.log.Warn("evidence research skipped: research capability is not enabled")
		return nil
	}
	return s.runEvidenceResearchWithTools(ctx, now, eventMap, tools, s.analyzer)
}

// runEvidenceResearchWithTools is the testable core of the production research
// phase. It plans due sessions, executes each sequentially under a phase
// timeout, reconciles found facts into narrow canonical supplements, records
// ResearchState, and feeds accepted lifecycle changes into the eventMap. It is
// fail-soft: errors are logged and must never break the main source scan.
func (s *Service) runEvidenceResearchWithTools(ctx context.Context, now time.Time, eventMap map[int64][]model.Event, tools fetcher.ResearchTools, extractor evidenceResearchExtractor) error {
	dueSessions, byID, _, err := s.prepareEvidenceResearch(ctx, now)
	if err != nil {
		s.log.Warn("evidence research planning failed", "error", err)
		return nil
	}
	if len(dueSessions) == 0 {
		return nil
	}

	phaseCtx, cancel := context.WithTimeout(ctx, evidenceResearchPhaseTimeout)
	defer cancel()

	for _, session := range dueSessions {
		if phaseCtx.Err() != nil {
			// Sessions that never started must not record attempts.
			break
		}
		competition, ok := byID[session.CompetitionID]
		if !ok {
			continue
		}
		execution, execErr := executeEvidenceResearchSession(phaseCtx, tools, extractor, competition, session)
		if execErr != nil {
			// Fatal executor error (e.g. edition conflict). Mark this session's
			// fields unresolved with a long cooldown and continue.
			s.log.Warn("evidence research execution failed", "competition_id", competition.ID, "error", execErr)
			s.recordUnresolvedForSession(phaseCtx, now, session, execErr.Error())
			continue
		}
		s.reconcileAndRecordExecution(phaseCtx, competition, execution, now, eventMap)
	}
	return nil
}

// recordUnresolvedForSession records every gap field of an un-executable session
// as unresolved (long cooldown) — used for fatal executor errors like edition
// conflict, which should not hot-loop every 6 hours.
func (s *Service) recordUnresolvedForSession(ctx context.Context, now time.Time, session model.ResearchSession, reason string) {
	cfg := s.cfg.EvidenceResearch
	retry := researchNextRetryAt(model.ResearchStateUnresolved, now, cfg)
	for _, gap := range session.Gaps {
		if !model.ValidEvidenceField(gap.Field) {
			continue
		}
		_ = s.store.RecordEvidenceResearchAttempt(ctx, session.CompetitionID, gap.Field, model.ResearchStateUnresolved, now, retry, firstNonEmpty(reason, "executor fatal error"))
	}
}

// reconcileAndRecordExecution reconciles one session execution and records
// ResearchState + events.
func (s *Service) reconcileAndRecordExecution(ctx context.Context, competition model.Competition, execution evidenceResearchExecution, now time.Time, eventMap map[int64][]model.Event) {
	cfg := s.cfg.EvidenceResearch
	for _, fieldResult := range execution.Fields {
		if phaseCtxErr := ctx.Err(); phaseCtxErr != nil {
			// Global phase deadline reached: fields not yet reconciled are not
			// recorded (planning != execution) and stay due next round.
			break
		}
		decision := s.reconcileEvidenceResearchField(ctx, competition, execution, fieldResult, now, cfg)
		_ = s.store.RecordEvidenceResearchAttempt(ctx, competition.ID, decision.Field, decision.ResearchState, now, decision.NextRetryAt, firstNonEmpty(decision.LastError, ""))
		if decision.CanonicalChanged && decision.SavedCompetition != nil {
			events := changeEvents(competition, *decision.SavedCompetition, false, now, s.freshnessWindow())
			if len(events) > 0 {
				eventMap[competition.ID] = append(eventMap[competition.ID], events...)
			}
		}
	}
}
