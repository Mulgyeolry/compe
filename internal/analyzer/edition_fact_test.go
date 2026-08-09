package analyzer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
)

// TestDeriveCanonicalEditionFactFromTitle verifies the deterministic helper
// derives the edition from an explicit four-digit year in the Document.Title and
// produces a FactEdition with real title evidence.
func TestDeriveCanonicalEditionFactFromTitle(t *testing.T) {
	doc := model.Document{
		Title: "4C2026通知-关于举办中国大学生计算机设计大赛",
		URL:   "https://jsjds.blcu.edu.cn/info/1041/2274.htm",
		Text:  "4C2026通知-关于举办中国大学生计算机设计大赛 报名启动",
	}
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	var rejections []model.AnalysisRejection
	fact, ok := deriveCanonicalEditionFact(doc, AIFact{}, model.TrustHigh, now, &rejections)
	if !ok {
		t.Fatal("expected a deterministic edition fact from the title year")
	}
	if fact.Value != "2026" || fact.Edition != "2026" {
		t.Fatalf("edition fact value=%q edition=%q, want 2026", fact.Value, fact.Edition)
	}
	if fact.SourceURL != doc.URL {
		t.Fatalf("source_url=%q, want %q", fact.SourceURL, doc.URL)
	}
	if !strings.Contains(fact.Evidence, "4C2026") {
		t.Fatalf("evidence %q must contain the title's year token", fact.Evidence)
	}
	if len(rejections) != 0 {
		t.Fatalf("unexpected rejections: %+v", rejections)
	}
}

// TestDeriveCanonicalEditionFactRejectsModelRelabel verifies a model cannot
// relabel a document whose title is explicitly 2026 as edition 2025: the
// authoritative edition stays 2026 (never 2025), and the relabel attempt is
// recorded as a rejection.
func TestDeriveCanonicalEditionFactRejectsModelRelabel(t *testing.T) {
	doc := model.Document{
		Title: "4C2026通知",
		URL:   "https://example.com/2026",
		Text:  "4C2026通知",
	}
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	// Model claims 2025 but the title says 2026 and the evidence is the title.
	ai := AIFact{Value: "2025", Evidence: "4C2026通知", Edition: "2025", Confidence: "high"}
	var rejections []model.AnalysisRejection
	fact, ok := deriveCanonicalEditionFact(doc, ai, model.TrustHigh, now, &rejections)
	if !ok {
		t.Fatal("title edition must be produced (authoritative)")
	}
	if fact.Value == "2025" {
		t.Fatalf("FactEdition must never become the model-relabeled 2025, got %q", fact.Value)
	}
	if fact.Value != "2026" {
		t.Fatalf("FactEdition value=%q, want 2026 (title is authoritative)", fact.Value)
	}
	if len(rejections) == 0 {
		t.Fatal("model relabel attempt must record a rejection")
	}
}

// TestDeriveCanonicalEditionFactAcceptsValidatedAIOnly when there is no title
// year but a validated AI identity.edition whose Value and Evidence both carry
// the same explicit year.
func TestDeriveCanonicalEditionFactAcceptsValidatedAIOnly(t *testing.T) {
	doc := model.Document{
		Title: "某比赛报名通知", // no year in title
		URL:   "https://example.com/contest",
		Text:  "2026年第十九届比赛现已启动报名",
	}
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	ai := AIFact{Value: "2026", Evidence: "2026年第十九届比赛现已启动", Edition: "2026", Confidence: "high"}
	var rejections []model.AnalysisRejection
	fact, ok := deriveCanonicalEditionFact(doc, ai, model.TrustMedium, now, &rejections)
	if !ok {
		t.Fatal("validated AI edition with same-year evidence should produce a fact")
	}
	if fact.Value != "2026" {
		t.Fatalf("edition value=%q want 2026", fact.Value)
	}
	if !strings.Contains(fact.Evidence, "2026") {
		t.Fatalf("evidence %q must carry the year", fact.Evidence)
	}
}

// TestCanonicalPersistsEditionWhenNormalizedNameDropsYear is the comp=220
// regression: a title with an explicit "4C2026" year, but the model normalizes
// the identity name to a year-less string. The FactEdition must survive, and the
// competition still resolves to edition 2026 even though Name has no year.
func TestCanonicalPersistsEditionWhenNormalizedNameDropsYear(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v11","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"rejection_reason":""}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v11","identity":{"name":{"value":"中国大学生计算机设计大赛","evidence":"中国大学生计算机设计大赛","edition":"2026","confidence":"high"}},"facts":{},"events":[]}`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	analysis := New(config.Config{Location: shanghai})
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)

	doc := model.Document{
		Title: "4C2026通知-关于举办中国大学生计算机设计大赛",
		URL:   "https://jsjds.blcu.edu.cn/info/1041/2274.htm",
		Text:  "4C2026通知-关于举办中国大学生计算机设计大赛 面向全国高校开放报名。",
	}
	competition, _, err := analysis.Analyze(context.Background(), model.Candidate{Title: doc.Title}, doc, model.TrustHigh, now)
	if err != nil {
		t.Fatal(err)
	}
	if competition.Name != "中国大学生计算机设计大赛" {
		t.Fatalf("normalized name=%q, want year-less name", competition.Name)
	}
	fact, ok := competition.Facts[model.FactEdition]
	if !ok {
		t.Fatalf("FactEdition must survive even when Name drops the year; facts=%+v", competition.Facts)
	}
	if fact.Value != "2026" || fact.Edition != "2026" {
		t.Fatalf("FactEdition value=%q edition=%q, want 2026", fact.Value, fact.Edition)
	}
	if !strings.Contains(fact.Evidence, "4C2026") {
		t.Fatalf("FactEdition evidence %q must contain the original title year token", fact.Evidence)
	}
	if fact.SourceURL != doc.URL {
		t.Fatalf("FactEdition source_url=%q, want %q", fact.SourceURL, doc.URL)
	}
}

// TestRuleAnalysisPersistsEditionFact verifies the rules-only path writes
// FactEdition from the title year.
func TestRuleAnalysisPersistsEditionFact(t *testing.T) {
	analysis := New(config.Config{Location: shanghai})
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	doc := model.Document{
		Title: "4C2026通知-中国大学生计算机设计大赛",
		URL:   "https://jsjds.blcu.edu.cn/info/1041/2274.htm",
		Text:  "4C2026通知-中国大学生计算机设计大赛 报名启动",
	}
	// Call the internal ruleAnalysis directly (no LLM).
	comp := analysis.ruleAnalysis(model.Candidate{}, doc, model.TrustHigh, doc.Text, 90, now)
	fact, ok := comp.Facts[model.FactEdition]
	if !ok {
		t.Fatalf("ruleAnalysis must write FactEdition; facts=%+v", comp.Facts)
	}
	if fact.Value != "2026" {
		t.Fatalf("FactEdition value=%q, want 2026", fact.Value)
	}
}
