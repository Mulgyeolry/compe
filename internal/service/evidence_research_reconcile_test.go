package service

import (
	"context"
	"testing"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/model"
)

// --- sameResearchAuthority ---

func TestSameResearchAuthority(t *testing.T) {
	cases := []struct {
		source, official string
		want             bool
	}{
		{"https://example.com/a", "https://example.com/b", true},
		{"https://www.example.com/a", "https://example.com/b", true},
		{"https://notice.example.com/a", "https://example.com/b", true},
		{"https://example.com", "https://evil-example.com", false},
		{"https://example.com", "https://example.com.evil.net", false},
		{"http://localhost/x", "https://example.com/b", false},
		{"not a url", "https://example.com/b", false},
		{"ftp://example.com/a", "https://example.com/b", false},
	}
	for _, tc := range cases {
		if got := sameResearchAuthority(tc.source, tc.official); got != tc.want {
			t.Fatalf("sameResearchAuthority(%q,%q)=%v want %v", tc.source, tc.official, got, tc.want)
		}
	}
}

// --- researchFactConfidence ---

func TestResearchFactConfidence(t *testing.T) {
	if got := researchFactConfidence(model.TrustHigh); got != "high" {
		t.Fatalf("high trust confidence=%q", got)
	}
	if got := researchFactConfidence(model.TrustMedium); got != "medium" {
		t.Fatalf("medium trust confidence=%q", got)
	}
	if got := researchFactConfidence(model.TrustLow); got != "" {
		t.Fatalf("low trust confidence=%q, want empty", got)
	}
}

// --- lifecycle inference ---

func researchCompWithDates(regStart, regEnd, compStart, compEnd *time.Time) model.Competition {
	return model.Competition{
		RegistrationStart:    regStart,
		RegistrationEnd:      regEnd,
		CompetitionStart:     compStart,
		CompetitionEnd:       compEnd,
		RegistrationPhase:    model.RegistrationUnknown,
		CompetitionPhase:     model.CompetitionUnknown,
	}
}

func researchPtr(date time.Time) *time.Time { return &date }

func TestResearchLifecycleAfterSupplement(t *testing.T) {
	now := researchNow()
	past := researchPtr(time.Date(2026, 7, 1, 0, 0, 0, 0, researchLocation()))
	future := researchPtr(time.Date(2026, 9, 1, 0, 0, 0, 0, researchLocation()))
	insideStart := researchPtr(time.Date(2026, 8, 1, 0, 0, 0, 0, researchLocation()))
	insideEnd := researchPtr(time.Date(2026, 8, 10, 0, 0, 0, 0, researchLocation()))

	// Registration: unknown + past end → closed.
	comp, _ := researchLifecycleAfterSupplement(researchCompWithDates(nil, past, nil, nil), now)
	if comp.RegistrationPhase != model.RegistrationClosed {
		t.Fatalf("past reg end should be closed, got %s", comp.RegistrationPhase)
	}
	// Registration: unknown + future start → preview.
	comp, _ = researchLifecycleAfterSupplement(researchCompWithDates(future, nil, nil, nil), now)
	if comp.RegistrationPhase != model.RegistrationPreview {
		t.Fatalf("future reg start should be preview, got %s", comp.RegistrationPhase)
	}
	// Registration: unknown + start<=today<=end → open.
	comp, _ = researchLifecycleAfterSupplement(researchCompWithDates(insideStart, insideEnd, nil, nil), now)
	if comp.RegistrationPhase != model.RegistrationOpen {
		t.Fatalf("interval reg should be open, got %s", comp.RegistrationPhase)
	}
	// Registration: unknown + past start only → stays unknown.
	comp, _ = researchLifecycleAfterSupplement(researchCompWithDates(past, nil, nil, nil), now)
	if comp.RegistrationPhase != model.RegistrationUnknown {
		t.Fatalf("past start only should stay unknown, got %s", comp.RegistrationPhase)
	}
	// Registration: unknown + future end only → stays unknown.
	comp, _ = researchLifecycleAfterSupplement(researchCompWithDates(nil, future, nil, nil), now)
	if comp.RegistrationPhase != model.RegistrationUnknown {
		t.Fatalf("future end only should stay unknown, got %s", comp.RegistrationPhase)
	}
	// Competition: unknown + past end → finished.
	comp, _ = researchLifecycleAfterSupplement(researchCompWithDates(nil, nil, nil, past), now)
	if comp.CompetitionPhase != model.CompetitionFinished {
		t.Fatalf("past comp end should be finished, got %s", comp.CompetitionPhase)
	}
	// Competition: unknown + future start → upcoming.
	comp, _ = researchLifecycleAfterSupplement(researchCompWithDates(nil, nil, future, nil), now)
	if comp.CompetitionPhase != model.CompetitionUpcoming {
		t.Fatalf("future comp start should be upcoming, got %s", comp.CompetitionPhase)
	}
	// Competition: unknown + start<=today<=end → ongoing.
	comp, _ = researchLifecycleAfterSupplement(researchCompWithDates(nil, nil, insideStart, insideEnd), now)
	if comp.CompetitionPhase != model.CompetitionOngoing {
		t.Fatalf("interval comp should be ongoing, got %s", comp.CompetitionPhase)
	}
	// Existing non-unknown phase must not regress.
	existing := researchCompWithDates(nil, past, nil, nil)
	existing.RegistrationPhase = model.RegistrationOpen
	comp, _ = researchLifecycleAfterSupplement(existing, now)
	if comp.RegistrationPhase != model.RegistrationOpen {
		t.Fatalf("existing open phase must not regress, got %s", comp.RegistrationPhase)
	}
}

// --- Reconciler outcomes ---

func reconcileReconcilerCompetition() model.Competition {
	return model.Competition{
		ID:          7,
		Name:        "2026全国大学生程序设计大赛",
		OfficialURL: "https://example.com/2026",
		Trust:       model.TrustHigh,
	}
}

func reconcileFoundFact(field model.EvidenceField) evidenceResearchFieldResult {
	date := time.Date(2026, 4, 9, 0, 0, 0, 0, researchLocation())
	fact := analyzer.ResearchEvidenceFact{
		Field: field, Date: date, Raw: "2026年4月9日", Evidence: "报名截止时间为2026年4月9日",
		Edition: "2026", SourceURL: "https://example.com/2026", Confidence: "high",
	}
	return evidenceResearchFieldResult{Field: field, Outcome: evidenceResearchFound, Fact: &fact}
}

func TestReconcileFoundSameAuthorityAccepted(t *testing.T) {
	cfg := researchTestConfig(t)
	app, database := researchTestService(t, cfg)
	ctx := context.Background()
	// Persist a competition with a registration_end gap.
	competition := reconcileReconcilerCompetition()
	competition.ID = 0
	competition.EntityKey = "reconcile-accept"
	competition.OfficialURL = "https://example.com/2026"
	competition.Name = "2026全国大学生程序设计大赛"
	_, _, err := database.UpsertCompetition(ctx, competition, "test", researchNow())
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := database.GetCompetition(ctx, competition.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	execution := evidenceResearchExecution{CompetitionID: persisted.ID, Edition: "2026"}

	decision := app.reconcileEvidenceResearchField(ctx, persisted, execution, reconcileFoundFact(model.EvidenceRegistrationEnd), researchNow(), cfg.EvidenceResearch)
	if decision.ResearchState != model.ResearchStateResolved {
		t.Fatalf("expected resolved, got %s (outcome %s)", decision.ResearchState, decision.Outcome)
	}
	if !decision.CanonicalChanged {
		t.Fatalf("expected canonical change")
	}
	// Verify canonical now holds the calendar date.
	after, err := database.GetCompetitionByID(ctx, persisted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RegistrationEnd == nil || !researchSameCalendarDate(*after.RegistrationEnd, time.Date(2026, 4, 9, 0, 0, 0, 0, researchLocation())) {
		t.Fatalf("registration_end not supplemented: %v", after.RegistrationEnd)
	}
}

func TestReconcileCrossDomainRejected(t *testing.T) {
	cfg := researchTestConfig(t)
	app, database := researchTestService(t, cfg)
	ctx := context.Background()
	competition := reconcileReconcilerCompetition()
	competition.EntityKey = "reconcile-crossdomain"
	_, _, _ = database.UpsertCompetition(ctx, competition, "test", researchNow())
	persisted, _ := database.GetCompetition(ctx, competition.EntityKey)
	execution := evidenceResearchExecution{CompetitionID: persisted.ID, Edition: "2026"}

	field := reconcileFoundFact(model.EvidenceRegistrationEnd)
	field.Fact.SourceURL = "https://other-site.org/2026" // genuinely different authority
	decision := app.reconcileEvidenceResearchField(ctx, persisted, execution, field, researchNow(), cfg.EvidenceResearch)
	if decision.ResearchState != model.ResearchStateUnresolved {
		t.Fatalf("cross-domain should be unresolved, got %s", decision.ResearchState)
	}
	after, _ := database.GetCompetitionByID(ctx, persisted.ID)
	if after.RegistrationEnd != nil {
		t.Fatalf("cross-domain must not write canonical")
	}
}

func TestReconcileWrongEditionRejected(t *testing.T) {
	cfg := researchTestConfig(t)
	app, database := researchTestService(t, cfg)
	ctx := context.Background()
	competition := reconcileReconcilerCompetition()
	competition.EntityKey = "reconcile-wrongedition"
	_, _, _ = database.UpsertCompetition(ctx, competition, "test", researchNow())
	persisted, _ := database.GetCompetition(ctx, competition.EntityKey)
	execution := evidenceResearchExecution{CompetitionID: persisted.ID, Edition: "2026"}

	field := reconcileFoundFact(model.EvidenceRegistrationEnd)
	field.Fact.Edition = "2025" // wrong edition
	decision := app.reconcileEvidenceResearchField(ctx, persisted, execution, field, researchNow(), cfg.EvidenceResearch)
	if decision.ResearchState != model.ResearchStateUnresolved {
		t.Fatalf("wrong edition should be unresolved, got %s", decision.ResearchState)
	}
}

func TestReconcileFieldMismatchInvariant(t *testing.T) {
	cfg := researchTestConfig(t)
	app, database := researchTestService(t, cfg)
	ctx := context.Background()
	competition := reconcileReconcilerCompetition()
	competition.EntityKey = "reconcile-fmismatch"
	_, _, _ = database.UpsertCompetition(ctx, competition, "test", researchNow())
	persisted, _ := database.GetCompetition(ctx, competition.EntityKey)
	execution := evidenceResearchExecution{CompetitionID: persisted.ID, Edition: "2026"}

	// Field registration_end but fact.Field competition_start.
	field := reconcileFoundFact(model.EvidenceRegistrationEnd)
	field.Fact.Field = model.EvidenceCompetitionStart
	decision := app.reconcileEvidenceResearchField(ctx, persisted, execution, field, researchNow(), cfg.EvidenceResearch)
	if decision.Outcome != evidenceResearchConflict {
		t.Fatalf("field mismatch should be conflict/invariant, got %s", decision.Outcome)
	}
}

func TestReconcileAlreadySameDateResolved(t *testing.T) {
	cfg := researchTestConfig(t)
	app, database := researchTestService(t, cfg)
	ctx := context.Background()
	competition := reconcileReconcilerCompetition()
	competition.EntityKey = "reconcile-already"
	// Pre-fill registration_end with the same date.
	date := time.Date(2026, 4, 9, 0, 0, 0, 0, researchLocation())
	competition.RegistrationEnd = &date
	_, _, _ = database.UpsertCompetition(ctx, competition, "test", researchNow())
	persisted, _ := database.GetCompetition(ctx, competition.EntityKey)
	execution := evidenceResearchExecution{CompetitionID: persisted.ID, Edition: "2026"}

	decision := app.reconcileEvidenceResearchField(ctx, persisted, execution, reconcileFoundFact(model.EvidenceRegistrationEnd), researchNow(), cfg.EvidenceResearch)
	if decision.Outcome != evidenceResearchAlreadyPresent || decision.ResearchState != model.ResearchStateResolved {
		t.Fatalf("already-present same date should be resolved, got outcome=%s state=%s", decision.Outcome, decision.ResearchState)
	}
	if decision.CanonicalChanged {
		t.Fatalf("no canonical write expected for already-present")
	}
}

func TestReconcileAlreadyDifferentDateSkipped(t *testing.T) {
	cfg := researchTestConfig(t)
	app, database := researchTestService(t, cfg)
	ctx := context.Background()
	competition := reconcileReconcilerCompetition()
	competition.EntityKey = "reconcile-different"
	other := time.Date(2026, 4, 15, 0, 0, 0, 0, researchLocation())
	competition.RegistrationEnd = &other
	_, _, _ = database.UpsertCompetition(ctx, competition, "test", researchNow())
	persisted, _ := database.GetCompetition(ctx, competition.EntityKey)
	execution := evidenceResearchExecution{CompetitionID: persisted.ID, Edition: "2026"}

	decision := app.reconcileEvidenceResearchField(ctx, persisted, execution, reconcileFoundFact(model.EvidenceRegistrationEnd), researchNow(), cfg.EvidenceResearch)
	if decision.ResearchState != model.ResearchStateSkipped {
		t.Fatalf("different-date already present should be skipped, got %s", decision.ResearchState)
	}
	if decision.CanonicalChanged {
		t.Fatalf("no canonical write expected for different-date already present")
	}
}
