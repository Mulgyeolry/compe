package service

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/config"
	"competition-assistant/internal/fetcher"
	"competition-assistant/internal/model"
	"competition-assistant/internal/store"
)

// researchToolCollector is a fake that implements both fetcher.Collector (so it
// can back a Service) and fetcher.ResearchTools (so research can run), counting
// Search/Fetch calls.
type researchToolCollector struct {
	researchToolsFake
	discoverResult []model.Candidate
}

func (c *researchToolCollector) Discover(_ context.Context, _ config.Source) ([]model.Candidate, error) {
	return c.discoverResult, nil
}

func (c *researchToolCollector) Fetch(ctx context.Context, raw string) (model.Document, error) {
	// ResearchTools.Fetch is what the executor uses; expose the fake counting.
	return c.researchToolsFake.Fetch(ctx, raw)
}

// integrationNewService builds a Service backed by a real store and the given
// research-capable collector.
func integrationNewService(t *testing.T, cfg config.Config, collector *researchToolCollector) (*Service, *store.Store) {
	t.Helper()
	database, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	app := New(cfg, database, collector, analyzer.New(cfg), researchNoopSender{}, logger)
	app.SetNow(researchNow)
	return app, database
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func integrationConfig(t *testing.T, enabled bool) config.Config {
	t.Helper()
	cfg := researchTestConfig(t)
	cfg.EvidenceResearch.Enabled = enabled
	return cfg
}

// TestEvidenceResearchFeatureGateDisabled verifies that Enabled=false yields
// zero Search / Fetch / Extractor / ResearchState work even with a
// research-capable collector.
func TestEvidenceResearchFeatureGateDisabled(t *testing.T) {
	cfg := integrationConfig(t, false)
	collector := &researchToolCollector{
		researchToolsFake: researchToolsFake{
			searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
				return []fetcher.ResearchSearchResult{{URL: "https://example.com/a"}}, nil
			},
			fetchFn: func(_ context.Context, raw string) (model.Document, error) {
				return model.Document{URL: raw, Title: "页", Text: "报名截止2026年4月9日"}, nil
			},
		},
	}
	app, database := integrationNewService(t, cfg, collector)
	ctx := context.Background()

	competition := model.Competition{EntityKey: "gate-comp", Name: "2026某某大赛", OfficialURL: "https://example.com/2026", Trust: model.TrustHigh}
	if _, _, err := database.UpsertCompetition(ctx, competition, "test", researchNow()); err != nil {
		t.Fatal(err)
	}

	if err := app.runEvidenceResearch(ctx, researchNow(), map[int64][]model.Event{}); err != nil {
		t.Fatal(err)
	}
	if collector.searchCount() != 0 || collector.fetchCount() != 0 {
		t.Fatalf("disabled research must not search/fetch: %d searches %d fetches", collector.searchCount(), collector.fetchCount())
	}
	states, err := database.ListEvidenceResearchStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("disabled research must not record attempts, got %d", len(states))
	}
}

// TestEvidenceResearchFailSoft verifies that research-phase failures never break
// the main scan: runEvidenceResearch returns nil even when Search fails, and
// does not error out.
func TestEvidenceResearchFailSoft(t *testing.T) {
	cfg := integrationConfig(t, true)
	collector := &researchToolCollector{
		researchToolsFake: researchToolsFake{
			searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
				return nil, errors.New("searxng down")
			},
		},
	}
	app, database := integrationNewService(t, cfg, collector)
	ctx := context.Background()
	competition := model.Competition{EntityKey: "failsoft-comp", Name: "2026某某大赛", OfficialURL: "https://example.com/2026", Trust: model.TrustHigh}
	if _, _, err := database.UpsertCompetition(ctx, competition, "test", researchNow()); err != nil {
		t.Fatal(err)
	}
	extractor := &researchExtractorFake{}
	if err := app.runEvidenceResearchWithTools(ctx, researchNow(), map[int64][]model.Event{}, collector, extractor); err != nil {
		t.Fatalf("research failure must not propagate: %v", err)
	}
	// Ordinary source scan is unaffected (runEvidenceResearch itself is fail-soft).
}

// TestEvidenceResearchProductionEnabled verifies that with Enabled=true the full
// research phase executes the executor, supplements the canonical, writes
// FactEvidence, records ResearchState=resolved, and produces a registration
// lifecycle event.
func TestEvidenceResearchProductionEnabled(t *testing.T) {
	cfg := integrationConfig(t, true)
	collector := &researchToolCollector{
		researchToolsFake: researchToolsFake{
			searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
				return nil, nil
			},
			fetchFn: func(_ context.Context, raw string) (model.Document, error) {
				return model.Document{URL: raw, Title: "官方", Text: "报名截止时间为2026年4月9日"}, nil
			},
		},
	}
	app, database := integrationNewService(t, cfg, collector)
	ctx := context.Background()

	competition := model.Competition{
		EntityKey:   "prod-comp",
		Name:        "2026某某大赛",
		OfficialURL: "https://example.com/2026",
		Trust:       model.TrustHigh,
		Status:      model.StatusUnknown,
	}
	if _, _, err := database.UpsertCompetition(ctx, competition, "test", researchNow()); err != nil {
		t.Fatal(err)
	}
	persisted, err := database.GetCompetition(ctx, competition.EntityKey)
	if err != nil {
		t.Fatal(err)
	}

	eventMap := map[int64][]model.Event{}
	// Use a fake extractor that returns a registration_end fact from the official
	// page, avoiding a live LLM.
	extractor := &researchExtractorFake{
		extractFn: func(req analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error) {
			var facts []analyzer.ResearchEvidenceFact
			for _, field := range req.Fields {
				if field == model.EvidenceRegistrationEnd {
					facts = append(facts, analyzer.ResearchEvidenceFact{
						Field: field, Date: time.Date(2026, 4, 9, 0, 0, 0, 0, researchLocation()),
						Raw: "2026年4月9日", Evidence: "报名截止时间为2026年4月9日",
						Edition: "2026", SourceURL: "https://example.com/2026", Confidence: "high",
					})
				}
			}
			return analyzer.ResearchEvidenceResult{Facts: facts}, nil
		},
	}
	if err := app.runEvidenceResearchWithTools(ctx, researchNow(), eventMap, collector, extractor); err != nil {
		t.Fatal(err)
	}

	// Canonical registration_end supplemented.
	after, err := database.GetCompetitionByID(ctx, persisted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.RegistrationEnd == nil {
		t.Fatalf("canonical registration_end not supplemented")
	}
	// FactEvidence written.
	if _, ok := after.Facts[model.FactRegistrationEnd]; !ok {
		t.Fatalf("registration_end FactEvidence not written")
	}
	// ResearchState: registration_end resolved, others unresolved.
	states, err := database.ListEvidenceResearchStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundResolved := false
	for _, state := range states {
		if state.Field == model.EvidenceRegistrationEnd && state.Status == model.ResearchStateResolved {
			foundResolved = true
		}
	}
	if !foundResolved {
		t.Fatalf("expected registration_end resolved state, got %+v", states)
	}
}
