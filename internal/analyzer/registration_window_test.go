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

// TestRegistrationWindowApplicabilityFromStrongHierarchyEvidence verifies the
// deterministic helper accepts "not_applicable" only when the evidence carries a
// real selection hierarchy AND a progression marker (校→省→国 with 推荐/上推).
func TestRegistrationWindowApplicabilityFromStrongHierarchyEvidence(t *testing.T) {
	doc := model.Document{
		Title: "4C2026通知-中国大学生计算机设计大赛",
		URL:   "https://jsjds.blcu.edu.cn/info/1041/2274.htm",
		Text:  "大赛以校级赛、省级赛、国家级赛三级竞赛形式开展，国赛只接受省级赛上推的参赛作品。",
	}
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	ai := AIFact{
		Value: "not_applicable",
		Evidence: "大赛以校级赛、省级赛、国家级赛三级竞赛形式开展，国赛只接受省级赛上推的参赛作品。",
		Edition: "2026",
		Confidence: "high",
	}
	var rejections []model.AnalysisRejection
	fact, ok := deriveRegistrationWindowApplicabilityFact(ai, doc, model.TrustHigh, now, &rejections)
	if !ok {
		t.Fatalf("strong hierarchy+progression evidence must produce a fact; rejections=%+v", rejections)
	}
	if fact.Value != string(model.RegistrationWindowNotApplicable) {
		t.Fatalf("value=%q, want not_applicable", fact.Value)
	}
	if fact.SourceURL != doc.URL {
		t.Fatalf("source_url=%q, want %q", fact.SourceURL, doc.URL)
	}
	if !strings.Contains(fact.Evidence, "上推") {
		t.Fatalf("evidence %q must carry the progression marker", fact.Evidence)
	}
	if len(rejections) != 0 {
		t.Fatalf("unexpected rejections: %+v", rejections)
	}
}

// TestRegistrationWindowNotApplicableRequiresProgressionEvidence verifies a
// "regional / district / sub-site / school" competition with a unified signup
// window must NOT become not_applicable just because of those words — a real
// hierarchy + progression path is required.
func TestRegistrationWindowNotApplicableRequiresProgressionEvidence(t *testing.T) {
	doc := model.Document{
		Title: "面向上海地区高校统一报名的比赛",
		URL:   "https://example.com/shanghai",
		Text:  "本赛事面向上海地区高校统一报名，报名时间为2026年3月1日至4月15日，设多个分赛区。",
	}
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	ai := AIFact{
		Value: "not_applicable",
		Evidence: "本赛事面向上海地区高校统一报名，报名时间为2026年3月1日至4月15日，设多个分赛区。",
		Edition: "2026",
		Confidence: "high",
	}
	var rejections []model.AnalysisRejection
	_, ok := deriveRegistrationWindowApplicabilityFact(ai, doc, model.TrustHigh, now, &rejections)
	if ok {
		t.Fatal("regional/sub-site wording without progression must NOT become not_applicable")
	}
	if len(rejections) == 0 {
		t.Fatal("must record a rejection for missing progression evidence")
	}
}

// TestRegistrationWindowApplicabilityIgnoresApplicableAndEmpty ensures V1 only
// ever persists "not_applicable": a model claiming "applicable" or an empty value
// does not produce a fact (treated as unknown by the model helper).
func TestRegistrationWindowApplicabilityIgnoresApplicableAndEmpty(t *testing.T) {
	doc := model.Document{Title: "某比赛", URL: "https://example.com/x", Text: "本赛事面向上海地区高校统一报名"}
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	for _, v := range []string{"applicable", "", "centralized"} {
		var rejections []model.AnalysisRejection
		ai := AIFact{Value: v, Evidence: "本赛事面向上海地区高校统一报名", Edition: "2026", Confidence: "high"}
		if fact, ok := deriveRegistrationWindowApplicabilityFact(ai, doc, model.TrustHigh, now, &rejections); ok {
			t.Fatalf("value %q must not produce a fact, got %+v", v, fact)
		}
	}
}

// TestCanonicalPersistsRegistrationWindowNotApplicable is the comp=220 style
// full Analyze regression: v12 re-analysis of the 4C notice (校→省→国 with 上推)
// persists FactRegistrationWindowApplicability=not_applicable with real Document
// evidence and edition 2026.
func TestCanonicalPersistsRegistrationWindowNotApplicable(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v12","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"rejection_reason":""}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v12","identity":{"edition":{"value":"2026","evidence":"4C2026通知-中国大学生计算机设计大赛","edition":"2026","confidence":"high"}},"facts":{"registration_window_applicability":{"value":"not_applicable","evidence":"大赛以校级赛、省级赛、国家级赛三级竞赛形式开展，国赛只接受省级赛上推的参赛作品","edition":"2026","confidence":"high"}},"events":[]}`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	analysis := New(config.Config{Location: shanghai})
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)

	doc := model.Document{
		Title: "4C2026通知-中国大学生计算机设计大赛",
		URL:   "https://jsjds.blcu.edu.cn/info/1041/2274.htm",
		Text:  "大赛以校级赛、省级赛、国家级赛三级竞赛形式开展，国赛只接受省级赛上推的参赛作品。",
	}
	competition, _, err := analysis.Analyze(context.Background(), model.Candidate{Title: doc.Title}, doc, model.TrustHigh, now)
	if err != nil {
		t.Fatal(err)
	}
	fact, ok := competition.Facts[model.FactRegistrationWindowApplicability]
	if !ok {
		t.Fatalf("FactRegistrationWindowApplicability must be persisted; facts=%+v", competition.Facts)
	}
	if fact.Value != string(model.RegistrationWindowNotApplicable) {
		t.Fatalf("value=%q, want not_applicable", fact.Value)
	}
	if fact.Edition != "2026" {
		t.Fatalf("edition=%q, want 2026", fact.Edition)
	}
	if fact.SourceURL != doc.URL {
		t.Fatalf("source_url=%q, want %q", fact.SourceURL, doc.URL)
	}
	if !strings.Contains(fact.Evidence, "上推") {
		t.Fatalf("evidence %q must carry a progression marker", fact.Evidence)
	}
}
