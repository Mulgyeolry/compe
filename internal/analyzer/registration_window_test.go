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

// TestRegistrationWindowNotApplicableRejectsUnifiedNationalSignup is a false-positive
// regression: even if the AI incorrectly proposes not_applicable for a page that
// states a unified national signup through the official site, the strong gate must
// reject it. The sentence has only a single national level and an explicit
// centralized signup intent, so FactRegistrationWindowApplicability must be absent.
func TestRegistrationWindowNotApplicableRejectsUnifiedNationalSignup(t *testing.T) {
	doc := model.Document{
		Title: "全国大学生XX大赛2026报名通知",
		URL:   "https://example.com/national",
		Text:  "全国赛报名参加方式统一通过官网进行，报名时间为2026年3月1日至4月15日。",
	}
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	ai := AIFact{
		Value:      "not_applicable",
		Evidence:   "全国赛报名参加方式统一通过官网进行",
		Edition:    "2026",
		Confidence: "high",
	}
	var rejections []model.AnalysisRejection
	if fact, ok := deriveRegistrationWindowApplicabilityFact(ai, doc, model.TrustHigh, now, &rejections); ok {
		t.Fatalf("unified national signup must NOT become not_applicable, got %+v", fact)
	}
	if len(rejections) == 0 {
		t.Fatal("must record a rejection for the centralized signup evidence")
	}
}

// TestRegistrationWindowNotApplicableRejectsMultiSiteUnifiedSignup is a false-positive
// regression: multiple sub-sites (分赛区) with a unified online signup is a centralized
// registration model, not "no unified registration window".
func TestRegistrationWindowNotApplicableRejectsMultiSiteUnifiedSignup(t *testing.T) {
	doc := model.Document{
		Title: "XX大赛2026报名通知",
		URL:   "https://example.com/multisite",
		Text:  "赛事设置多个分赛区，所有参赛者统一通过官网报名。",
	}
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	ai := AIFact{
		Value:      "not_applicable",
		Evidence:   "赛事设置多个分赛区，所有参赛者统一通过官网报名",
		Edition:    "2026",
		Confidence: "high",
	}
	var rejections []model.AnalysisRejection
	if fact, ok := deriveRegistrationWindowApplicabilityFact(ai, doc, model.TrustHigh, now, &rejections); ok {
		t.Fatalf("multi-site unified signup must NOT become not_applicable, got %+v", fact)
	}
	if len(rejections) == 0 {
		t.Fatal("must record a rejection for the multi-site unified signup evidence")
	}
}

// TestRegistrationWindowNotApplicableRequiresMultipleLevels is a false-positive
// regression: a single national level with a progression token ("全国赛推荐报名")
// must FAIL because not_applicable requires at least two distinct competition
// levels.
func TestRegistrationWindowNotApplicableRequiresMultipleLevels(t *testing.T) {
	doc := model.Document{
		Title: "全国赛2026通知",
		URL:   "https://example.com/single-level",
		Text:  "全国赛推荐报名。",
	}
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	ai := AIFact{
		Value:      "not_applicable",
		Evidence:   "全国赛推荐报名",
		Edition:    "2026",
		Confidence: "high",
	}
	var rejections []model.AnalysisRejection
	if fact, ok := deriveRegistrationWindowApplicabilityFact(ai, doc, model.TrustHigh, now, &rejections); ok {
		t.Fatalf("single-level evidence must NOT become not_applicable, got %+v", fact)
	}
	if len(rejections) == 0 {
		t.Fatal("must record a rejection for the single-level evidence")
	}
}

// TestRegistrationWindowNotApplicableRejectsUnifiedProvincialSelection verifies the
// contradiction guard fires even when multiple levels and a progression token are
// present: explicit unified signup wording still forces unknown.
func TestRegistrationWindowNotApplicableRejectsUnifiedProvincialSelection(t *testing.T) {
	doc := model.Document{
		Title: "XX大赛2026通知",
		URL:   "https://example.com/provincial-unified",
		Text:  "各省设立赛区，但全国统一网上报名，推荐晋级选手参加国赛。",
	}
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	ai := AIFact{
		Value:      "not_applicable",
		Evidence:   "各省设立赛区，但全国统一网上报名",
		Edition:    "2026",
		Confidence: "high",
	}
	var rejections []model.AnalysisRejection
	if fact, ok := deriveRegistrationWindowApplicabilityFact(ai, doc, model.TrustHigh, now, &rejections); ok {
		t.Fatalf("centralized signup wording must override level/progression tokens, got %+v", fact)
	}
	if len(rejections) == 0 {
		t.Fatal("must record a rejection for the centralized signup contradiction")
	}
}

// TestRulesOnlyDoesNotInferRegistrationWindowApplicability verifies the rules-only
// path never auto-infers FactRegistrationWindowApplicability, even for strong
// comp=220-style hierarchy/progression text: not_applicable requires a validated AI
// proposal, so without AI the field stays absent (unknown) and registration remains
// researchable.
func TestRulesOnlyDoesNotInferRegistrationWindowApplicability(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, shanghai)
	doc := model.Document{
		Title: "4C2026通知-中国大学生计算机设计大赛",
		URL:   "https://jsjds.blcu.edu.cn/info/1041/2274.htm",
		Text:  "大赛以校级赛、省级赛、国家级赛三级竞赛形式开展，国赛只接受省级赛上推的参赛作品。",
	}
	// No LLM configured: only the deterministic rules path runs.
	analysis := New(config.Config{Location: shanghai})
	competition, _, err := analysis.Analyze(context.Background(), model.Candidate{Title: doc.Title}, doc, model.TrustHigh, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := competition.Facts[model.FactRegistrationWindowApplicability]; ok {
		t.Fatalf("rules-only path must NOT infer not_applicable; facts=%+v", competition.Facts)
	}
}

// TestRegistrationWindowNotApplicableRejectsWrongEdition verifies the edition-bound
// validation: when the canonical/document edition is 2026 but the model proposes the
// applicability fact for edition 2025, FactRegistrationWindowApplicability is not
// written even though the evidence has strong hierarchy+progression. The audit must
// carry an edition rejection.
func TestRegistrationWindowNotApplicableRejectsWrongEdition(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v12","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"rejection_reason":""}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v12","identity":{"edition":{"value":"2026","evidence":"4C2026通知-中国大学生计算机设计大赛","edition":"2026","confidence":"high"}},"facts":{"registration_window_applicability":{"value":"not_applicable","evidence":"大赛以校级赛、省级赛、国家级赛三级竞赛形式开展，国赛只接受省级赛上推的参赛作品","edition":"2025","confidence":"high"}},"events":[]}`)))
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
	if _, ok := competition.Facts[model.FactRegistrationWindowApplicability]; ok {
		t.Fatalf("wrong-edition applicability must NOT be persisted; facts=%+v", competition.Facts)
	}
	// The audit must carry an edition rejection for the field.
	found := false
	for _, rejection := range competition.ExtractionAudit.Rejections {
		if rejection.Field == "facts.registration_window_applicability" && strings.Contains(rejection.Reason, "edition") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("audit must include an edition rejection for %s; rejections=%+v", "facts.registration_window_applicability", competition.ExtractionAudit.Rejections)
	}
}
