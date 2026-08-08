package service

import (
	"context"
	"sort"
	"time"

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

// runEvidenceResearchDetection is the standalone Evidence Research detection
// phase. It is deliberately independent of processCandidate and of the
// observation "changed" decision: a canonical competition whose original page
// did not change this round can still enter a session when it is missing a
// date. Phase 1 only reads canonical competitions, builds sessions and logs
// counts; it never searches, fetches, calls the LLM, mutates canonical facts,
// produces events or touches notifications.
func (s *Service) runEvidenceResearchDetection(ctx context.Context, now time.Time) error {
	// Read the full canonical set, not the UI/active-list subset. ListActiveCompetitions
	// pre-filters by status (preview/upcoming/registration_open/ongoing), which would
	// drop the canonicals most in need of evidence research: StatusUnknown (all dates
	// unknown) and StatusRegistrationClosed (registration over but competition dates
	// still missing). Research eligibility itself is decided later by
	// evidenceResearchEligible, so no status pre-filter belongs here.
	competitions, err := s.store.ListCompetitions(ctx)
	if err != nil {
		return err
	}
	sessions := buildEvidenceResearchSessions(competitions, now, s.freshnessWindow())
	totalGaps := 0
	for _, session := range sessions {
		totalGaps += len(session.Gaps)
	}
	s.log.Info("evidence research detection",
		"sessions", len(sessions), "gaps", totalGaps)
	return nil
}
