package service_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
	"competition-assistant/internal/service"
)

// TestCampusRulesFallbackRecoversPublicCompetition is the service-level
// regression for the public-campus rules-only fallback: a previously v6,
// date-less, unknown Huawei-style canonical is re-analysed under v8. The AI
// rejects the page as campus forwarding, but the deterministic fallback keeps
// its rules facts. The single canonical row upgrades to v8 with correct dates,
// trust stays medium, the URL is untouched, and no duplicate discovered event
// is produced.
func TestCampusRulesFallbackRecoversPublicCompetition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionTestResponse(`{"schema_version":"competition-audit-v10","document_type":"campus_internal","source_role":"campus_forwarding","computer_related":true,"competition_announcement":false,"rejection_reason":"校内转发通知，非官方主办方发布的有效公告"}`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")

	doc := func() model.Document {
		return model.Document{
			URL:            testPageBase + "/huawei-2026",
			Title:          "关于2026年华为杯第二十三届中国研究生数学建模竞赛报名的通知",
			Text:           "中国研究生数学建模竞赛：参赛团队报名时间：2026年6月1日8:00至9月19日17:00。参赛缴费时间：2026年6月1日8:00至9月21日17:00。竞赛时间：2026年9月23日8:00至9月27日12:00。本赛事面向全国高校公开报名。",
			PublishedAtRaw: "2026-05-20",
			IsListing:      false,
		}
	}

	cfg := baseConfig(t)
	cfg.Sources = []config.Source{{ID: "graduate", Name: "研究生数学建模", Kind: "page", URL: testPageBase + "/huawei-2026", Trust: "medium", Limit: 10}}
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	sender := &memorySender{}
	app := service.New(cfg, database, pageCollector(doc), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	enableAllCategoryTestUser(t, database, app, "fixture@example.com", fixedNow())
	app.SetNow(fixedNow)

	now := fixedNow()
	docVal := doc()
	hash := contentHashFor(docVal)

	// Seed a v6 canonical with no dates / unknown status and a v6 observation
	// so the only change is the analyzer version (v6 -> v8).
	if _, err := database.RecordObservationVersioned(context.Background(), "graduate", docVal, hash, model.TrustMedium, "competition-audit-v6", now); err != nil {
		t.Fatalf("seed v6 observation: %v", err)
	}
	if _, _, err := database.UpsertCompetition(context.Background(), model.Competition{
		EntityKey:         "huawei-graduate-2026",
		Name:              "关于2026年华为杯第二十三届中国研究生数学建模竞赛报名的通知",
		OfficialURL:       docVal.URL,
		Trust:             model.TrustMedium,
		Status:            model.StatusUnknown,
		RegistrationPhase: model.RegistrationUnknown,
		AnalyzerVersion:   "competition-audit-v6",
		ContentHash:       hash,
	}, "graduate", now); err != nil {
		t.Fatalf("seed v6 canonical: %v", err)
	}

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	saved, err := database.GetCompetition(context.Background(), "huawei-graduate-2026")
	if err != nil {
		t.Fatalf("load canonical: %v", err)
	}
	if saved.AnalyzerVersion != "competition-audit-v10" {
		t.Errorf("canonical analyzer_version = %q, want v8", saved.AnalyzerVersion)
	}
	if saved.Trust != model.TrustMedium {
		t.Errorf("canonical trust = %v, want medium (fallback must not promote)", saved.Trust)
	}
	if saved.OfficialURL != docVal.URL {
		t.Errorf("canonical official_url changed to %q", saved.OfficialURL)
	}
	loc := time.FixedZone("CST", 8*3600)
	if saved.RegistrationStart == nil || !saved.RegistrationStart.Equal(time.Date(2026, 6, 1, 8, 0, 0, 0, loc)) {
		t.Errorf("registration_start = %v, want 2026-06-01 08:00", saved.RegistrationStart)
	}
	if saved.RegistrationEnd == nil || !saved.RegistrationEnd.Equal(time.Date(2026, 9, 19, 17, 0, 0, 0, loc)) {
		t.Errorf("registration_end = %v, want 2026-09-19 17:00", saved.RegistrationEnd)
	}
	if saved.CompetitionStart == nil || !saved.CompetitionStart.Equal(time.Date(2026, 9, 23, 8, 0, 0, 0, loc)) {
		t.Errorf("competition_start = %v, want 2026-09-23 08:00", saved.CompetitionStart)
	}
	if saved.CompetitionEnd == nil || !saved.CompetitionEnd.Equal(time.Date(2026, 9, 27, 12, 0, 0, 0, loc)) {
		t.Errorf("competition_end = %v, want 2026-09-27 12:00", saved.CompetitionEnd)
	}

	// No duplicate discovered event: the canonical must yield exactly one
	// competition_discovered row for this competition.
	raw, err := openRawDB(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var disc int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM competition_events WHERE competition_id=? AND event_type='competition_discovered'`, saved.ID).Scan(&disc); err != nil {
		t.Fatal(err)
	}
	if disc != 1 {
		t.Errorf("competition_discovered count = %d, want exactly 1 (no duplicates)", disc)
	}
}

// TestAnalyzerVersionBumpReanalyzesSameHash verifies the change-detection
// mechanism: a source_document recorded under analyzer_version v7 with the same
// content hash is re-analysed once the current analyzer is v8. This does not
// change the Store implementation; it proves the existing version-keyed
// re-analysis is triggered by the v8 bump.
func TestAnalyzerVersionBumpReanalyzesSameHash(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionTestResponse(`{"schema_version":"competition-audit-v10","document_type":"campus_internal","source_role":"campus_forwarding","computer_related":true,"competition_announcement":false,"rejection_reason":"校内转发通知"}`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")

	doc := func() model.Document {
		return model.Document{
			URL:            testPageBase + "/huawei-2026-version",
			Title:          "关于2026年华为杯第二十三届中国研究生数学建模竞赛报名的通知",
			Text:           "中国研究生数学建模竞赛：参赛团队报名时间：2026年6月1日8:00至9月19日17:00。本赛事面向全国高校公开报名。",
			PublishedAtRaw: "2026-05-20",
			IsListing:      false,
		}
	}

	cfg := baseConfig(t)
	cfg.Sources = []config.Source{{ID: "graduate", Name: "研究生数学建模", Kind: "page", URL: testPageBase + "/huawei-2026-version", Trust: "medium", Limit: 10}}
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	sender := &memorySender{}
	app := service.New(cfg, database, pageCollector(doc), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	enableAllCategoryTestUser(t, database, app, "fixture@example.com", fixedNow())
	app.SetNow(fixedNow)

	now := fixedNow()
	docVal := doc()
	hash := contentHashFor(docVal)

	// Seed the same page under v7 only (no canonical). Under v8 the same hash
	// must be re-analysed (version changed) and produce a canonical row.
	if _, err := database.RecordObservationVersioned(context.Background(), "graduate", docVal, hash, model.TrustMedium, "competition-audit-v7", now); err != nil {
		t.Fatalf("seed v7 observation: %v", err)
	}

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	// The v8 analysis (via fallback) must have produced a canonical competition
	// for this URL.
	raw, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM competitions WHERE official_url=?`, docVal.URL).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("expected the same-hash page to be re-analysed under v8 and produce a canonical")
	}
}

// openRawDB returns a *sql.DB over the sqlite file at path.
func openRawDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	return db, nil
}
