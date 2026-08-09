package service

import (
	"testing"
	"time"

	"competition-assistant/internal/model"
)

// TestDetectEvidenceGapsSkipsRegistrationWhenNotApplicable verifies that when the
// canonical has registration_window_applicability=not_applicable, registration_start
// and registration_end are NOT reported as gaps, while competition dates still are.
func TestDetectEvidenceGapsSkipsRegistrationWhenNotApplicable(t *testing.T) {
	c := model.Competition{
		Facts: map[string]model.FactEvidence{
			model.FactRegistrationWindowApplicability: {Value: string(model.RegistrationWindowNotApplicable), Edition: "2026"},
		},
	}
	gaps := detectEvidenceGaps(c)
	if len(gaps) != 2 {
		t.Fatalf("expected 2 competition gaps, got %d: %+v", len(gaps), gaps)
	}
	for _, gap := range gaps {
		if gap.Field == model.EvidenceRegistrationStart || gap.Field == model.EvidenceRegistrationEnd {
			t.Fatalf("registration gaps must be skipped when not_applicable, got %+v", gaps)
		}
	}
}

// TestDetectEvidenceGapsUnknownKeepsRegistrationGaps verifies that unknown /
// applicable registration applicability keeps the registration gaps (old behavior).
func TestDetectEvidenceGapsUnknownKeepsRegistrationGaps(t *testing.T) {
	for _, app := range []string{"", string(model.RegistrationWindowUnknown), string(model.RegistrationWindowApplicable), "bogus"} {
		c := model.Competition{}
		if app != "" {
			c.Facts = map[string]model.FactEvidence{model.FactRegistrationWindowApplicability: {Value: app}}
		}
		gaps := detectEvidenceGaps(c)
		if len(gaps) != 4 {
			t.Fatalf("applicability %q must keep 4 gaps, got %d: %+v", app, len(gaps), gaps)
		}
	}
}

// TestDetectEvidenceGapsNotApplicableWithCompetitionDatesFullySuppressed verifies
// that once both competition dates exist and registration is not_applicable, the
// gap list is empty (comp=220 end state).
func TestDetectEvidenceGapsNotApplicableWithCompetitionDatesFullySuppressed(t *testing.T) {
	cs := time.Date(2026, 7, 17, 0, 0, 0, 0, researchLocation())
	ce := time.Date(2026, 8, 16, 0, 0, 0, 0, researchLocation())
	c := model.Competition{
		CompetitionStart: &cs,
		CompetitionEnd:   &ce,
		Facts: map[string]model.FactEvidence{
			model.FactRegistrationWindowApplicability: {Value: string(model.RegistrationWindowNotApplicable), Edition: "2026"},
		},
	}
	gaps := detectEvidenceGaps(c)
	if len(gaps) != 0 {
		t.Fatalf("expected 0 gaps, got %d: %+v", len(gaps), gaps)
	}
}

// TestEvidenceResearchPlannerExcludesNotApplicableRegistrationGaps verifies the
// full build+plan path: even with historical unresolved ResearchState rows for
// registration_start/end, a not_applicable competition's due session must only
// contain competition_start/competition_end.
func TestEvidenceResearchPlannerExcludesNotApplicableRegistrationGaps(t *testing.T) {
	now := researchNow()
	// Competition with not_applicable registration and both competition dates nil.
	// A recent FirstSeen makes it current-edition eligible despite a year-less name.
	competition := model.Competition{
		ID:          220,
		Name:        "中国大学生计算机设计大赛",
		OfficialURL: "https://jsjds.blcu.edu.cn/info/1041/2274.htm",
		Trust:       model.TrustHigh,
		FirstSeen:   now.AddDate(0, 0, -2),
		Facts: map[string]model.FactEvidence{
			model.FactEdition:                         {Value: "2026", Edition: "2026"},
			model.FactRegistrationWindowApplicability: {Value: string(model.RegistrationWindowNotApplicable), Edition: "2026"},
		},
	}
	sessions := buildEvidenceResearchSessions([]model.Competition{competition}, now, researchFreshness())
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	// The session gaps must exclude registration fields.
	fields := map[model.EvidenceField]bool{}
	for _, gap := range sessions[0].Gaps {
		fields[gap.Field] = true
	}
	if fields[model.EvidenceRegistrationStart] || fields[model.EvidenceRegistrationEnd] {
		t.Fatalf("not_applicable registration must not be in session gaps, got %+v", sessions[0].Gaps)
	}
	if !fields[model.EvidenceCompetitionStart] || !fields[model.EvidenceCompetitionEnd] {
		t.Fatalf("competition gaps must be present, got %+v", sessions[0].Gaps)
	}

	// Historical unresolved ResearchState for registration fields must not bring
	// them back into the due plan.
	states := []model.EvidenceResearchState{
		{CompetitionID: 220, Field: model.EvidenceRegistrationStart, Status: model.ResearchStateUnresolved, NextRetryAt: researchRetryAtService(now.Add(-time.Hour), 0)},
		{CompetitionID: 220, Field: model.EvidenceRegistrationEnd, Status: model.ResearchStateUnresolved, NextRetryAt: researchRetryAtService(now.Add(-time.Hour), 0)},
	}
	due, _ := planDueResearchSessions(sessions, states, now, 5)
	if len(due) != 1 {
		t.Fatalf("expected 1 due session, got %d", len(due))
	}
	dueFields := map[model.EvidenceField]bool{}
	for _, gap := range due[0].Gaps {
		dueFields[gap.Field] = true
	}
	if dueFields[model.EvidenceRegistrationStart] || dueFields[model.EvidenceRegistrationEnd] {
		t.Fatalf("registration fields must never re-appear in due plan, got %+v", due[0].Gaps)
	}
	if len(due[0].Gaps) != 2 {
		t.Fatalf("expected only competition_start/competition_end due, got %+v", due[0].Gaps)
	}
}

// TestRegistrationWindowApplicabilityOf verifies the model helper default behavior.
func TestRegistrationWindowApplicabilityOf(t *testing.T) {
	if got := model.RegistrationWindowApplicabilityOf(model.Competition{}); got != model.RegistrationWindowUnknown {
		t.Fatalf("no fact must be unknown, got %s", got)
	}
	c := model.Competition{Facts: map[string]model.FactEvidence{model.FactRegistrationWindowApplicability: {Value: "not_applicable"}}}
	if got := model.RegistrationWindowApplicabilityOf(c); got != model.RegistrationWindowNotApplicable {
		t.Fatalf("expected not_applicable, got %s", got)
	}
	c = model.Competition{Facts: map[string]model.FactEvidence{model.FactRegistrationWindowApplicability: {Value: "applicable"}}}
	if got := model.RegistrationWindowApplicabilityOf(c); got != model.RegistrationWindowApplicable {
		t.Fatalf("expected applicable, got %s", got)
	}
	c = model.Competition{Facts: map[string]model.FactEvidence{model.FactRegistrationWindowApplicability: {Value: "bogus"}}}
	if got := model.RegistrationWindowApplicabilityOf(c); got != model.RegistrationWindowUnknown {
		t.Fatalf("invalid value must be unknown, got %s", got)
	}
}
