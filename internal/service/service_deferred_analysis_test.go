package service_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
	"competition-assistant/internal/service"
)

// contentHashFor mirrors the service's contentHash computation so a test can
// pre-seed a change-detection observation with the exact hash the scan will
// compute for the same document.
func contentHashFor(doc model.Document) string {
	text := fmt.Sprintf("%s\n[published_at]%s\n[listing]%t", doc.Text, doc.PublishedAtRaw, doc.IsListing)
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:])
}

// TestPendingAnalysisPreservesExistingCanonicalData is the v6->v7 deferred
// upgrade regression test: a confirmed v6 canonical competition whose page is
// re-analysed under v7, but whose v7 AI pass fails with EOF, must NOT be
// downgraded. The pending attempt records a v7 audit and resets the retry
// baseline, but the canonical lifecycle/date/version fields survive.
func TestPendingAnalysisPreservesExistingCanonicalData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionTestResponse("")))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")

	doc := func() model.Document {
		return model.Document{
			URL:            testPageBase + "/huawei-graduate-2026",
			Title:          "关于2026年中国研究生数学建模竞赛报名的通知",
			Text:           "中国研究生数学建模竞赛：参赛团队报名时间：2026年6月1日8:00至9月19日17:00。竞赛时间：2026年9月23日8:00至9月27日12:00。",
			PublishedAtRaw: "2026-05-20",
			IsListing:      false,
		}
	}

	cfg := baseConfig(t)
	cfg.Sources = []config.Source{{ID: "graduate", Name: "研究生数学建模", Kind: "page", URL: testPageBase + "/huawei-graduate-2026", Trust: "high", Limit: 10}}
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	sender := &memorySender{}
	app := service.New(cfg, database, pageCollector(doc), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	enableAllCategoryTestUser(t, database, app, "fixture@example.com", fixedNow())
	app.SetNow(fixedNow)

	now := fixedNow()
	loc := time.FixedZone("CST", 8*3600)
	start := time.Date(2026, 6, 1, 8, 0, 0, 0, loc)
	end := time.Date(2026, 9, 19, 17, 0, 0, 0, loc)
	docVal := doc()
	hash := contentHashFor(docVal)

	// Seed a v6-confirmed canonical row and a v6 observation so the only change
	// between the seed and the upcoming v7 scan is the analyzer version.
	if _, err := database.RecordObservationVersioned(context.Background(), "graduate", docVal, hash, model.TrustHigh, "competition-audit-v6", now); err != nil {
		t.Fatalf("seed v6 observation: %v", err)
	}
	if _, _, err := database.UpsertCompetition(context.Background(), model.Competition{
		EntityKey:            "huawei-graduate-2026",
		Name:                 "关于2026年中国研究生数学建模竞赛报名的通知",
		OfficialURL:          docVal.URL,
		Trust:                model.TrustHigh,
		Status:               model.StatusRegistrationOpen,
		StatusEvidence:       "参赛团队报名时间：2026年6月1日8:00至9月19日17:00",
		RegistrationPhase:    model.RegistrationOpen,
		Organizer:            "中国研究生数学建模竞赛组织委员会",
		RegistrationStart:    &start,
		RegistrationEnd:      &end,
		RegistrationStartRaw: "2026年6月1日8:00",
		RegistrationEndRaw:   "9月19日17:00",
		AnalyzerVersion:      "competition-audit-v6",
		ContentHash:          hash,
	}, "graduate", now); err != nil {
		t.Fatalf("seed v6 canonical: %v", err)
	}

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	// 1. Competition ID and v6 canonical data must survive the pending v7 pass.
	saved, err := database.GetCompetition(context.Background(), "huawei-graduate-2026")
	if err != nil {
		t.Fatalf("load canonical: %v", err)
	}
	if saved.ID == 0 {
		t.Fatal("competition id lost")
	}
	if saved.AnalyzerVersion != "competition-audit-v6" {
		t.Errorf("canonical analyzer_version = %q, want competition-audit-v6 (pending must not claim v7)", saved.AnalyzerVersion)
	}
	if saved.Status != model.StatusRegistrationOpen {
		t.Errorf("canonical status = %q, want registration_open", saved.Status)
	}
	if saved.StatusEvidence == "" {
		t.Error("canonical status_evidence was cleared by pending analysis")
	}
	if saved.RegistrationPhase != model.RegistrationOpen {
		t.Errorf("canonical registration_phase = %q, want open", saved.RegistrationPhase)
	}
	if saved.RegistrationStartRaw != "2026年6月1日8:00" {
		t.Errorf("canonical registration_start_raw = %q, want 2026年6月1日8:00", saved.RegistrationStartRaw)
	}
	if saved.RegistrationEndRaw != "9月19日17:00" {
		t.Errorf("canonical registration_end_raw = %q, want 9月19日17:00", saved.RegistrationEndRaw)
	}
	if saved.Organizer != "中国研究生数学建模竞赛组织委员会" {
		t.Errorf("canonical organizer = %q, want preserved", saved.Organizer)
	}
	if saved.ContentHash != hash {
		t.Errorf("canonical content_hash = %q, want %q", saved.ContentHash, hash)
	}

	// 2. The observation audit must record the v7 failure. The empty-content
	// retry now surfaces a clear message instead of a vague EOF.
	raw, err := sql.Open("sqlite", cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var audit string
	if err := raw.QueryRow(`SELECT analysis_result_json FROM observations WHERE url=? ORDER BY seen_at DESC,id DESC LIMIT 1`, docVal.URL).Scan(&audit); err != nil {
		t.Fatalf("read latest observation audit: %v", err)
	}
	if !strings.Contains(audit, "competition-audit-v9") {
		t.Errorf("latest observation audit does not record v7: %s", audit)
	}
	if !strings.Contains(audit, "empty classification content") {
		t.Errorf("latest observation audit does not record the empty-content failure: %s", audit)
	}

	// 3. The retry baseline must be cleared so a later scan re-analyses the page.
	var docCount int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM source_documents WHERE source_id='graduate' AND url=?`, docVal.URL).Scan(&docCount); err != nil {
		t.Fatal(err)
	}
	if docCount != 0 {
		t.Errorf("expected retry baseline cleared (source_documents count=%d), want 0", docCount)
	}
}

// TestSuccessfulUpgradeStillClearsInvalidatedFacts is the success control: when
// the v7 analysis succeeds and returns fresh canonical facts, the canonical
// analyzer_version must advance to v7 and the old v6 facts must be cleared,
// preserving the analyzer-upgrade-clears-invalidated-facts semantics.
func TestSuccessfulUpgradeStillClearsInvalidatedFacts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			// First call is the classification pass.
			_, _ = w.Write([]byte(chatCompletionTestResponse(`{"schema_version":"competition-audit-v9","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"rejection_reason":""}`)))
			return
		}
		// Subsequent calls are the enrichment pass; they omit the stale v6
		// registration end so a successful v7 upgrade may clear it.
		_, _ = w.Write([]byte(chatCompletionTestResponse(`{"schema_version":"competition-audit-v9","identity":{"organizer":{"value":"中国研究生数学建模竞赛组织委员会","evidence":"主办方：中国研究生数学建模竞赛组织委员会","edition":"2026","confidence":"high"}},"facts":{"registration_start":{"value":"2026年6月1日8:00","evidence":"报名时间：2026年6月1日8:00","edition":"2026","confidence":"high"}},"events":[{"type":"registration_opened","evidence":"本赛事现已开放报名","edition":"2026","confidence":"high"}]}`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")

	doc := func() model.Document {
		return model.Document{
			URL:            testPageBase + "/huawei-graduate-2026-success",
			Title:          "关于2026年中国研究生数学建模竞赛报名的通知",
			Text:           "中国研究生数学建模竞赛：主办方：中国研究生数学建模竞赛组织委员会。参赛团队报名时间：2026年6月1日8:00至9月19日17:00。",
			PublishedAtRaw: "2026-05-20",
			IsListing:      false,
		}
	}

	cfg := baseConfig(t)
	cfg.Sources = []config.Source{{ID: "graduate", Name: "研究生数学建模", Kind: "page", URL: testPageBase + "/huawei-graduate-2026-success", Trust: "high", Limit: 10}}
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	sender := &memorySender{}
	app := service.New(cfg, database, pageCollector(doc), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	enableAllCategoryTestUser(t, database, app, "fixture@example.com", fixedNow())
	app.SetNow(fixedNow)

	now := fixedNow()
	loc := time.FixedZone("CST", 8*3600)
	start := time.Date(2026, 6, 1, 8, 0, 0, 0, loc)
	docVal := doc()
	hash := contentHashFor(docVal)

	// Seed a v6 canonical with stale facts and dates that the v7 success must
	// be allowed to clear.
	if _, err := database.RecordObservationVersioned(context.Background(), "graduate", docVal, hash, model.TrustHigh, "competition-audit-v6", now); err != nil {
		t.Fatalf("seed v6 observation: %v", err)
	}
	if _, _, err := database.UpsertCompetition(context.Background(), model.Competition{
		EntityKey:            "huawei-graduate-2026-success",
		Name:                 "关于2026年中国研究生数学建模竞赛报名的通知",
		OfficialURL:          docVal.URL,
		Trust:                model.TrustHigh,
		Status:               model.StatusRegistrationOpen,
		StatusEvidence:       "旧版本报名截止2025年9月20日",
		RegistrationPhase:    model.RegistrationOpen,
		Organizer:            "旧主办方",
		RegistrationStart:    &start,
		RegistrationStartRaw: "2026年6月1日8:00",
		RegistrationEnd:      &start,
		RegistrationEndRaw:   "2025年9月20日",
		AnalyzerVersion:      "competition-audit-v6",
		ContentHash:          hash,
	}, "graduate", now); err != nil {
		t.Fatalf("seed v6 canonical: %v", err)
	}

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	saved, err := database.GetCompetition(context.Background(), "huawei-graduate-2026-success")
	if err != nil {
		t.Fatalf("load canonical: %v", err)
	}
	if saved.AnalyzerVersion != analyzer.AIAnalyzerVersion {
		t.Errorf("canonical analyzer_version = %q, want %q (success must advance to v7)", saved.AnalyzerVersion, analyzer.AIAnalyzerVersion)
	}
	// A successful v7 result may clear stale v6 facts (old date/organizer).
	if saved.RegistrationEndRaw != "" {
		t.Errorf("stale v6 registration end %q was not cleared by successful v7 upgrade", saved.RegistrationEndRaw)
	}
	if saved.Organizer == "旧主办方" {
		t.Errorf("stale v6 organizer was not replaced by successful v7 upgrade")
	}
	if saved.Organizer != "中国研究生数学建模竞赛组织委员会" {
		t.Errorf("canonical organizer = %q, want new v7 organizer", saved.Organizer)
	}
}
