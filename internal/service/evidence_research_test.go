package service

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/config"
	"competition-assistant/internal/fetcher"
	"competition-assistant/internal/model"
	"competition-assistant/internal/store"
)

// researchNoopSender is a no-op notifier.Sender for phase tests that must not
// produce any notifications.
type researchNoopSender struct{}

func (researchNoopSender) Send(context.Context, string, string) error { return nil }

func researchLocation() *time.Location {
	return time.FixedZone("CST", 8*3600)
}

func researchNow() time.Time {
	return time.Date(2026, 8, 3, 8, 0, 0, 0, researchLocation())
}

func researchFreshness() time.Duration {
	return 30 * 24 * time.Hour
}

func competitionWithDates() model.Competition {
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, researchLocation())
	end := time.Date(2026, 8, 12, 0, 0, 0, 0, researchLocation())
	regStart := time.Date(2026, 6, 1, 0, 0, 0, 0, researchLocation())
	regEnd := time.Date(2026, 7, 31, 0, 0, 0, 0, researchLocation())
	return model.Competition{
		ID:               1,
		Name:             "2026全国大学生程序设计大赛",
		Status:           model.StatusUpcoming,
		StatusEvidence:   "2026年8月开赛",
		OfficialURL:      "https://contest.example.com/2026",
		Trust:            model.TrustHigh,
		RegistrationStart: &regStart,
		RegistrationEnd:   &regEnd,
		CompetitionStart:  &start,
		CompetitionEnd:    &end,
	}
}

func TestDetectEvidenceGapsAllMissing(t *testing.T) {
	competition := competitionWithDates()
	competition.RegistrationStart = nil
	competition.RegistrationEnd = nil
	competition.CompetitionStart = nil
	competition.CompetitionEnd = nil
	gaps := detectEvidenceGaps(competition)
	if len(gaps) != 4 {
		t.Fatalf("expected 4 gaps, got %d: %v", len(gaps), gaps)
	}
}

func TestDetectEvidenceGapsOneMissing(t *testing.T) {
	competition := competitionWithDates()
	competition.CompetitionEnd = nil
	gaps := detectEvidenceGaps(competition)
	if len(gaps) != 1 {
		t.Fatalf("expected 1 gap, got %d: %v", len(gaps), gaps)
	}
	if gaps[0].Field != model.EvidenceCompetitionEnd || gaps[0].Reason != model.ResearchReasonMissing {
		t.Fatalf("unexpected gap: %+v", gaps[0])
	}
}

func TestDetectEvidenceGapsNoneMissing(t *testing.T) {
	gaps := detectEvidenceGaps(competitionWithDates())
	if len(gaps) != 0 {
		t.Fatalf("expected 0 gaps, got %d: %v", len(gaps), gaps)
	}
}

func TestDetectEvidenceGapsStableOrder(t *testing.T) {
	competition := competitionWithDates()
	competition.RegistrationStart = nil
	competition.RegistrationEnd = nil
	competition.CompetitionStart = nil
	competition.CompetitionEnd = nil
	gaps := detectEvidenceGaps(competition)
	wantOrder := []model.EvidenceField{
		model.EvidenceRegistrationStart,
		model.EvidenceRegistrationEnd,
		model.EvidenceCompetitionStart,
		model.EvidenceCompetitionEnd,
	}
	for index, want := range wantOrder {
		if gaps[index].Field != want {
			t.Fatalf("gap[%d].Field = %q, want %q", index, gaps[index].Field, want)
		}
	}
}

func TestBuildEvidenceResearchSessionsSingleCompetitionMultipleGaps(t *testing.T) {
	competition := competitionWithDates()
	competition.RegistrationStart = nil
	competition.RegistrationEnd = nil
	competition.CompetitionStart = nil
	competition.CompetitionEnd = nil
	sessions := buildEvidenceResearchSessions([]model.Competition{competition}, researchNow(), researchFreshness())
	if len(sessions) != 1 {
		t.Fatalf("expected exactly 1 session, got %d", len(sessions))
	}
	if len(sessions[0].Gaps) != 4 {
		t.Fatalf("expected 4 gaps in the single session, got %d", len(sessions[0].Gaps))
	}
}

func TestEvidenceResearchEligibleLowTrust(t *testing.T) {
	competition := competitionWithDates()
	competition.Trust = model.TrustLow
	competition.CompetitionEnd = nil
	if evidenceResearchEligible(competition, researchNow(), researchFreshness()) {
		t.Fatal("low-trust competition must not be eligible")
	}
}

func TestEvidenceResearchEligibleFinished(t *testing.T) {
	competition := competitionWithDates()
	competition.Status = model.StatusFinished
	competition.CompetitionEnd = nil
	if evidenceResearchEligible(competition, researchNow(), researchFreshness()) {
		t.Fatal("finished competition must not be eligible")
	}
}

func TestEvidenceResearchEligiblePreviousEdition(t *testing.T) {
	competition := competitionWithDates()
	competition.Name = "2024全国大学生程序设计大赛"
	competition.CompetitionEnd = nil
	if evidenceResearchEligible(competition, researchNow(), researchFreshness()) {
		t.Fatal("previous-edition competition must not be eligible")
	}
}

func TestEvidenceResearchEligibleCurrentHighTrustWithGap(t *testing.T) {
	competition := competitionWithDates()
	competition.CompetitionEnd = nil
	if !evidenceResearchEligible(competition, researchNow(), researchFreshness()) {
		t.Fatal("current high-trust competition with a missing date must be eligible")
	}
}

func TestEvidenceResearchEligibleNoGap(t *testing.T) {
	if evidenceResearchEligible(competitionWithDates(), researchNow(), researchFreshness()) {
		t.Fatal("competition with all dates present must not be eligible")
	}
}

func TestBuildEvidenceResearchSessionsStableOrder(t *testing.T) {
	competitionA := competitionWithDates()
	competitionA.ID = 10
	competitionA.CompetitionEnd = nil
	competitionB := competitionWithDates()
	competitionB.ID = 2
	competitionB.CompetitionEnd = nil
	// Feed in reverse ID order; output must be sorted by ID.
	sessions := buildEvidenceResearchSessions([]model.Competition{competitionA, competitionB}, researchNow(), researchFreshness())
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if sessions[0].CompetitionID != 2 || sessions[1].CompetitionID != 10 {
		t.Fatalf("sessions not sorted by CompetitionID: %d then %d", sessions[0].CompetitionID, sessions[1].CompetitionID)
	}
}

// researchTestService builds a Service backed by a real store, mirroring the
// external test helpers but available to the internal package.
func researchTestService(t *testing.T, cfg config.Config) (*Service, *store.Store) {
	t.Helper()
	database, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := New(cfg, database, fetcher.NewHTTPCollector(cfg), analyzer.New(cfg), researchNoopSender{}, logger)
	app.SetNow(researchNow)
	return app, database
}

func researchTestConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		Schedule: "0 8 * * *", Timezone: "Asia/Shanghai", Location: researchLocation(),
		DBPath: filepath.Join(t.TempDir(), "research.db"),
		Discovery: config.Discovery{AnnouncementFreshnessDays: 30},
		Fetch:     config.Fetch{TimeoutSeconds: 3, MaxBytes: 1024 * 1024, MaxCandidates: 20},
	}
}

// TestRunEvidenceResearchDetectionFindsPersistedCanonicalWithoutObservationChange
// proves that detection reads the persisted canonical set directly: a
// competition inserted via UpsertCompetition — with no new candidate or
// observation change in this pass — is still surfaced as a research session.
func TestRunEvidenceResearchDetectionFindsPersistedCanonicalWithoutObservationChange(t *testing.T) {
	cfg := researchTestConfig(t)
	app, database := researchTestService(t, cfg)
	ctx := context.Background()

	competition := competitionWithDates()
	competition.RegistrationStart = nil
	competition.RegistrationEnd = nil
	competition.CompetitionStart = nil
	competition.CompetitionEnd = nil
	if _, isNew, err := database.UpsertCompetition(ctx, competition, "test", researchNow()); err != nil || !isNew {
		t.Fatalf("upsert canonical: isNew=%v err=%v", isNew, err)
	}

	active, err := database.ListActiveCompetitions(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 active canonical, got %d err=%v", len(active), err)
	}

	sessions := buildEvidenceResearchSessions(active, researchNow(), researchFreshness())
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for the persisted canonical, got %d", len(sessions))
	}
	if len(sessions[0].Gaps) != 4 {
		t.Fatalf("expected 4 gaps, got %d", len(sessions[0].Gaps))
	}

	// The phase is read-only: no events and no notifications are produced.
	if err := app.runEvidenceResearchDetection(ctx, researchNow()); err != nil {
		t.Fatalf("runEvidenceResearchDetection: %v", err)
	}
	activeAfter, err := database.ListActiveCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(activeAfter) != 1 {
		t.Fatalf("detection phase must not mutate the canonical set, got %d", len(activeAfter))
	}
}

// TestEvidenceResearchIncludesStatusUnknownCanonical proves that a canonical
// with StatusUnknown (both phases unknown, all four dates missing) is part of
// the full set used by detection — even though ListActiveCompetitions would
// drop it — and becomes a research session.
func TestEvidenceResearchIncludesStatusUnknownCanonical(t *testing.T) {
	cfg := researchTestConfig(t)
	_, database := researchTestService(t, cfg)
	ctx := context.Background()

	competition := competitionWithDates()
	competition.Name = "2026 XXX 大赛"
	competition.RegistrationPhase = model.RegistrationUnknown
	competition.CompetitionPhase = model.CompetitionUnknown
	competition.Status = model.StatusUnknown
	competition.RegistrationStart = nil
	competition.RegistrationEnd = nil
	competition.CompetitionStart = nil
	competition.CompetitionEnd = nil
	competition.Trust = model.TrustHigh
	if _, isNew, err := database.UpsertCompetition(ctx, competition, "test", researchNow()); err != nil || !isNew {
		t.Fatalf("upsert canonical: isNew=%v err=%v", isNew, err)
	}

	// The UI/active-list path would have missed it (proving the bug), while the
	// full-set path detection now uses must include it.
	active, err := database.ListActiveCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range active {
		if c.ID == competition.ID {
			t.Fatalf("StatusUnknown canonical must NOT be in the active/UI list")
		}
	}
	all, err := database.ListCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessions := buildEvidenceResearchSessions(all, researchNow(), researchFreshness())
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for StatusUnknown canonical, got %d", len(sessions))
	}
	if len(sessions[0].Gaps) != 4 {
		t.Fatalf("expected 4 gaps for all-missing StatusUnknown canonical, got %d", len(sessions[0].Gaps))
	}
}

// TestEvidenceResearchIncludesRegistrationClosedCanonical proves that a
// registration_closed canonical whose competition dates are still missing is
// part of the full detection set and yields a session whose gaps at least cover
// competition_start/competition_end.
func TestEvidenceResearchIncludesRegistrationClosedCanonical(t *testing.T) {
	cfg := researchTestConfig(t)
	_, database := researchTestService(t, cfg)
	ctx := context.Background()

	regStart := time.Date(2026, 5, 1, 0, 0, 0, 0, researchLocation())
	regEnd := time.Date(2026, 6, 30, 0, 0, 0, 0, researchLocation())
	competition := model.Competition{
		ID:               2,
		Name:             "2026 XXX 大赛",
		StatusEvidence:   "2026年8月开赛",
		OfficialURL:      "https://contest.example.com/2026",
		Trust:            model.TrustHigh,
		RegistrationPhase: model.RegistrationClosed,
		CompetitionPhase:  model.CompetitionUnknown,
		Status:            model.StatusRegistrationClosed,
		RegistrationStart: &regStart,
		RegistrationEnd:   &regEnd,
		CompetitionStart:  nil,
		CompetitionEnd:    nil,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, competition, "test", researchNow()); err != nil || !isNew {
		t.Fatalf("upsert canonical: isNew=%v err=%v", isNew, err)
	}

	all, err := database.ListCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessions := buildEvidenceResearchSessions(all, researchNow(), researchFreshness())
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session for registration_closed canonical, got %d", len(sessions))
	}
	seenCompStart, seenCompEnd := false, false
	for _, gap := range sessions[0].Gaps {
		if gap.Field == model.EvidenceCompetitionStart {
			seenCompStart = true
		}
		if gap.Field == model.EvidenceCompetitionEnd {
			seenCompEnd = true
		}
	}
	if !seenCompStart || !seenCompEnd {
		t.Fatalf("registration_closed canonical gaps must cover competition dates, got %+v", sessions[0].Gaps)
	}
}

// TestEvidenceResearchExcludesInvalidCanonicalsFromFullSet confirms that
// switching detection to the full canonical set does not pull finished,
// previous-edition, or low-trust competitions into research sessions.
func TestEvidenceResearchExcludesInvalidCanonicalsFromFullSet(t *testing.T) {
	cfg := researchTestConfig(t)
	_, database := researchTestService(t, cfg)
	ctx := context.Background()

	// finished
	finished := competitionWithDates()
	finished.ID = 101
	finished.EntityKey = "finished-2026"
	finished.Name = "2026 XXX 大赛"
	finished.OfficialURL = "https://contest.example.com/finished"
	finished.CompetitionPhase = model.CompetitionFinished
	finished.Status = model.StatusFinished
	finished.CompetitionEnd = nil
	if _, _, err := database.UpsertCompetition(ctx, finished, "test", researchNow()); err != nil {
		t.Fatal(err)
	}
	// previous edition
	previous := competitionWithDates()
	previous.ID = 102
	previous.EntityKey = "previous-2024"
	previous.Name = "2024 XXX 大赛"
	previous.OfficialURL = "https://contest.example.com/previous"
	previous.CompetitionEnd = nil
	if _, _, err := database.UpsertCompetition(ctx, previous, "test", researchNow()); err != nil {
		t.Fatal(err)
	}
	// low trust
	lowTrust := competitionWithDates()
	lowTrust.ID = 103
	lowTrust.EntityKey = "lowtrust-2026"
	lowTrust.Name = "2026 XXX 大赛"
	lowTrust.OfficialURL = "https://contest.example.com/lowtrust"
	lowTrust.Trust = model.TrustLow
	lowTrust.CompetitionEnd = nil
	if _, _, err := database.UpsertCompetition(ctx, lowTrust, "test", researchNow()); err != nil {
		t.Fatal(err)
	}

	all, err := database.ListCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 persisted canonicals, got %d", len(all))
	}
	sessions := buildEvidenceResearchSessions(all, researchNow(), researchFreshness())
	if len(sessions) != 0 {
		t.Fatalf("finished/previous-edition/low-trust must not enter research, got %d sessions", len(sessions))
	}
}
