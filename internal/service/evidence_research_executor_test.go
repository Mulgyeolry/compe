package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/fetcher"
	"competition-assistant/internal/model"
)

// --- Fakes ---

type researchToolsFake struct {
	mu        sync.Mutex
	searchFn  func(context.Context, fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error)
	fetchFn   func(context.Context, string) (model.Document, error)
	searchCalls int
	fetchCalls  int
	searches    []fetcher.ResearchSearchRequest
	fetches     []string
}

func (f *researchToolsFake) Search(ctx context.Context, req fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
	f.mu.Lock()
	f.searchCalls++
	f.searches = append(f.searches, req)
	searchFn := f.searchFn
	f.mu.Unlock()
	if searchFn == nil {
		return nil, nil
	}
	return searchFn(ctx, req)
}

func (f *researchToolsFake) Fetch(ctx context.Context, raw string) (model.Document, error) {
	f.mu.Lock()
	f.fetchCalls++
	f.fetches = append(f.fetches, raw)
	fetchFn := f.fetchFn
	f.mu.Unlock()
	if fetchFn == nil {
		return model.Document{}, nil
	}
	return fetchFn(ctx, raw)
}

func (f *researchToolsFake) searchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searchCalls
}

func (f *researchToolsFake) fetchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetchCalls
}

type researchExtractorFake struct {
	mu       sync.Mutex
	extractFn func(analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error)
	calls    []analyzer.ResearchEvidenceRequest
}

func (f *researchExtractorFake) ExtractEvidenceFacts(_ context.Context, req analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	extractFn := f.extractFn
	f.mu.Unlock()
	if extractFn == nil {
		return analyzer.ResearchEvidenceResult{}, nil
	}
	return extractFn(req)
}

func (f *researchExtractorFake) fieldSets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sets []string
	for _, call := range f.calls {
		var fields []string
		for _, field := range call.Fields {
			fields = append(fields, string(field))
		}
		sets = append(sets, strings.Join(fields, ","))
	}
	return sets
}

// --- Helpers ---

func executorTestCompetition() model.Competition {
	return model.Competition{
		ID:          7,
		Name:        "2026全国大学生程序设计大赛",
		OfficialURL: "https://example.com/2026",
	}
}

func executorTestSession(fields ...model.EvidenceField) model.ResearchSession {
	var gaps []model.EvidenceGap
	for _, field := range fields {
		gaps = append(gaps, model.EvidenceGap{Field: field, Reason: model.ResearchReasonMissing})
	}
	return model.ResearchSession{CompetitionID: 7, Gaps: gaps}
}

func factFor(field model.EvidenceField, date time.Time) analyzer.ResearchEvidenceFact {
	return analyzer.ResearchEvidenceFact{Field: field, Date: date, Edition: "2026", SourceURL: "https://example.com/2026"}
}

// --- Validation ---

func TestExecutorValidation(t *testing.T) {
	tools := &researchToolsFake{}
	extractor := &researchExtractorFake{}
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)

	if _, err := executeEvidenceResearchSession(context.Background(), nil, extractor, competition, session); err == nil {
		t.Fatal("nil tools must error")
	}
	if _, err := executeEvidenceResearchSession(context.Background(), tools, nil, competition, session); err == nil {
		t.Fatal("nil extractor must error")
	}
	bad := competition
	bad.ID = 0
	if _, err := executeEvidenceResearchSession(context.Background(), tools, extractor, bad, session); err == nil {
		t.Fatal("invalid competition id must error")
	}
	mismatch := session
	mismatch.CompetitionID = 99
	if _, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, mismatch); err == nil {
		t.Fatal("session competition mismatch must error")
	}
	empty := executorTestSession()
	if _, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, empty); err == nil {
		t.Fatal("empty gaps must error")
	}
	invalid := executorTestSession(model.EvidenceField("fee"))
	if _, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, invalid); err == nil {
		t.Fatal("invalid gap must error")
	}
}

// --- Edition ---

func TestEvidenceResearchEdition(t *testing.T) {
	// Name year.
	comp := executorTestCompetition()
	if edition, err := evidenceResearchEdition(comp); err != nil || edition != "2026" {
		t.Fatalf("name edition = %q err=%v", edition, err)
	}
	// Lifecycle date year (no year in name/URL).
	dateComp := model.Competition{ID: 1, Name: "某某大赛", RegistrationEnd: ptrTime(2026, 4, 9)}
	if edition, err := evidenceResearchEdition(dateComp); err != nil || edition != "2026" {
		t.Fatalf("lifecycle edition = %q err=%v", edition, err)
	}
	// OfficialURL year only.
	urlComp := model.Competition{ID: 2, Name: "某某大赛", OfficialURL: "https://x.com/codecraft2026"}
	if edition, err := evidenceResearchEdition(urlComp); err != nil || edition != "2026" {
		t.Fatalf("url edition = %q err=%v", edition, err)
	}
	// Conflicting explicit years.
	conflict := model.Competition{ID: 3, Name: "2026某某大赛", RegistrationEnd: ptrTime(2025, 4, 9)}
	if _, err := evidenceResearchEdition(conflict); err == nil {
		t.Fatal("conflicting years must error")
	}
	// No explicit year.
	noYear := model.Competition{ID: 4, Name: "某某大赛", FirstSeen: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if _, err := evidenceResearchEdition(noYear); err == nil {
		t.Fatal("no explicit year must error (never use FirstSeen/now)")
	}
}

func ptrTime(year int, month int, day int) *time.Time {
	value := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	return &value
}

func TestExecutorNoEditionIsUnresolvedNoSearch(t *testing.T) {
	competition := model.Competition{ID: 7, Name: "某某大赛", OfficialURL: ""} // no year anywhere
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{}
	extractor := &researchExtractorFake{}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if tools.searchCount() != 0 {
		t.Fatalf("no edition must not search, got %d searches", tools.searchCount())
	}
	if len(execution.Fields) != 1 || execution.Fields[0].Outcome != evidenceResearchUnresolved {
		t.Fatalf("expected unresolved field, got %+v", execution.Fields)
	}
	if execution.Fields[0].LastError != "cannot determine canonical research edition" {
		t.Fatalf("unexpected last_error %q", execution.Fields[0].LastError)
	}
}

// --- OfficialURL first ---

func TestExecutorOfficialURLFetchedBeforeSearch(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd, model.EvidenceCompetitionStart)
	tools := &researchToolsFake{
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			return model.Document{URL: raw, Title: "官方", Text: "报名截止时间为2026年4月9日"}, nil
		},
	}
	extractor := &researchExtractorFake{
		extractFn: func(req analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error) {
			if len(req.Fields) == 1 && req.Fields[0] == model.EvidenceRegistrationEnd {
				return analyzer.ResearchEvidenceResult{Facts: []analyzer.ResearchEvidenceFact{
					factFor(model.EvidenceRegistrationEnd, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)),
				}}, nil
			}
			return analyzer.ResearchEvidenceResult{}, nil
		},
	}
	_, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.fetches) != 1 || tools.fetches[0] != competition.OfficialURL {
		t.Fatalf("official URL must be fetched first, got %v", tools.fetches)
	}
}

func TestExecutorOfficialURLResolvesAllStopsBeforeSearch(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd, model.EvidenceCompetitionStart)
	tools := &researchToolsFake{
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			return model.Document{URL: raw, Title: "官方", Text: "报名截止2026年4月9日，比赛开始2026年8月1日"}, nil
		},
	}
	extractor := &researchExtractorFake{
		extractFn: func(req analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error) {
			var facts []analyzer.ResearchEvidenceFact
			for _, field := range req.Fields {
				switch field {
				case model.EvidenceRegistrationEnd:
					facts = append(facts, factFor(field, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)))
				case model.EvidenceCompetitionStart:
					facts = append(facts, factFor(field, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)))
				}
			}
			return analyzer.ResearchEvidenceResult{Facts: facts}, nil
		},
	}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if tools.searchCount() != 0 {
		t.Fatalf("official page resolved all gaps; must not search, got %d searches", tools.searchCount())
	}
	if len(execution.Fields) != 2 || execution.Fields[0].Outcome != evidenceResearchFound || execution.Fields[1].Outcome != evidenceResearchFound {
		t.Fatalf("expected both found, got %+v", execution.Fields)
	}
}

func TestExecutorOfficialFetchFailureFallsBackToSearch(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			return model.Document{}, errors.New("fetch failed")
		},
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			return nil, nil
		},
	}
	extractor := &researchExtractorFake{}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if tools.searchCount() == 0 {
		t.Fatal("official fetch failure must fall back to search")
	}
	if execution.Fields[0].Outcome != evidenceResearchRetryable {
		t.Fatalf("field with operational error must be retryable, got %q", execution.Fields[0].Outcome)
	}
}

// --- Snippet never becomes evidence ---

func TestExecutorSearchSnippetNeverBecomesEvidence(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			// Real document has NO date, despite the snippet claiming 2099-01-01.
			return model.Document{URL: raw, Title: "页面", Text: "暂无报名时间"}, nil
		},
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			return []fetcher.ResearchSearchResult{{URL: "https://example.com/reg", Snippet: "报名截止 2099-01-01"}}, nil
		},
	}
	extractor := &researchExtractorFake{
		extractFn: func(req analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error) {
			// The extractor receives the real Document (no date) → no facts.
			return analyzer.ResearchEvidenceResult{}, nil
		},
	}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Fields[0].Outcome == evidenceResearchFound {
		t.Fatalf("search snippet must never become evidence; got found")
	}
	// The extractor must have been called with the real document, not the snippet.
	if len(extractor.calls) == 0 {
		t.Fatal("extractor was not called")
	}
	for _, call := range extractor.calls {
		if strings.Contains(call.Document.Text, "2099-01-01") {
			t.Fatalf("extractor must receive the real document, not the snippet text")
		}
	}
}

// --- Adaptive remaining fields ---

func TestExecutorAdaptiveRemainingFields(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd, model.EvidenceCompetitionStart)
	tools := &researchToolsFake{
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			return []fetcher.ResearchSearchResult{{URL: "https://example.com/a"}, {URL: "https://example.com/b"}}, nil
		},
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			return model.Document{URL: raw, Title: "页", Text: "报名截止2026年4月9日。"}, nil
		},
	}
	extractor := &researchExtractorFake{
		extractFn: func(req analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error) {
			var facts []analyzer.ResearchEvidenceFact
			for _, field := range req.Fields {
				if field == model.EvidenceRegistrationEnd {
					facts = append(facts, factFor(field, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)))
				}
			}
			return analyzer.ResearchEvidenceResult{Facts: facts}, nil
		},
	}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Fields[0].Outcome != evidenceResearchFound {
		t.Fatalf("registration_end should be found, got %+v", execution.Fields[0])
	}
	// The second extractor call (after registration_end resolved) must request only
	// competition_start.
	sets := extractor.fieldSets()
	foundRegEndRound := false
	for _, set := range sets {
		if set == "competition_start" {
			foundRegEndRound = true
		}
	}
	if !foundRegEndRound {
		t.Fatalf("after resolving registration_end, extractor must be called with only competition_start, got %v", sets)
	}
}

// --- Stop early ---

func TestExecutorStopsEarlyWhenAllResolved(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			return model.Document{URL: raw, Title: "官方", Text: "报名截止2026年4月9日"}, nil
		},
	}
	extractor := &researchExtractorFake{
		extractFn: func(req analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error) {
			var facts []analyzer.ResearchEvidenceFact
			for _, field := range req.Fields {
				if field == model.EvidenceRegistrationEnd {
					facts = append(facts, factFor(field, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)))
				}
			}
			return analyzer.ResearchEvidenceResult{Facts: facts}, nil
		},
	}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if tools.searchCount() != 0 || tools.fetchCount() != 1 {
		t.Fatalf("after official page resolved the field, must stop (0 searches, 1 fetch), got %d searches %d fetches", tools.searchCount(), tools.fetchCount())
	}
	if execution.Fields[0].Outcome != evidenceResearchFound {
		t.Fatalf("expected found, got %q", execution.Fields[0].Outcome)
	}
}

// --- Search / Fetch budgets ---

func TestExecutorSearchBudgetMaxThree(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			return []fetcher.ResearchSearchResult{{URL: "https://example.com/x"}}, nil
		},
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			return model.Document{URL: raw, Title: "页", Text: "无日期"}, nil
		},
	}
	extractor := &researchExtractorFake{}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if tools.searchCount() != maxEvidenceResearchSearchRounds {
		t.Fatalf("search calls=%d want %d", tools.searchCount(), maxEvidenceResearchSearchRounds)
	}
	if execution.Fields[0].Outcome != evidenceResearchUnresolved {
		t.Fatalf("expected unresolved after normal completion, got %q", execution.Fields[0].Outcome)
	}
}

func TestExecutorSearchLimitAlwaysEight(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			return nil, nil
		},
	}
	extractor := &researchExtractorFake{}
	if _, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session); err != nil {
		t.Fatal(err)
	}
	for _, search := range tools.searches {
		if search.Limit != evidenceResearchSearchLimit {
			t.Fatalf("search limit=%d want %d", search.Limit, evidenceResearchSearchLimit)
		}
	}
}

func TestExecutorSearchQueryUsesOfficialDomainRound1(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			return nil, nil
		},
	}
	extractor := &researchExtractorFake{}
	if _, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session); err != nil {
		t.Fatal(err)
	}
	if len(tools.searches) == 0 {
		t.Fatal("no search was made")
	}
	first := tools.searches[0]
	if len(first.AllowedDomains) != 1 || first.AllowedDomains[0] != "example.com" {
		t.Fatalf("round-1 search must restrict to official domain, got %v", first.AllowedDomains)
	}
}

func TestExecutorFetchBudgetMaxFive(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd, model.EvidenceCompetitionStart, model.EvidenceRegistrationStart, model.EvidenceCompetitionEnd)
	tools := &researchToolsFake{
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			// Return 10 distinct results so fetch budget is the limiter.
			var results []fetcher.ResearchSearchResult
			for i := 0; i < 10; i++ {
				results = append(results, fetcher.ResearchSearchResult{URL: "https://example.com/" + string(rune('a'+i))})
			}
			return results, nil
		},
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			return model.Document{URL: raw, Title: "页", Text: "无日期"}, nil
		},
	}
	extractor := &researchExtractorFake{}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	// 1 official fetch + up to 4 more = max 5.
	if tools.fetchCount() > maxEvidenceResearchFetches {
		t.Fatalf("fetch calls=%d exceed max %d", tools.fetchCount(), maxEvidenceResearchFetches)
	}
	if execution.FetchCalls > maxEvidenceResearchFetches {
		t.Fatalf("execution FetchCalls=%d exceed max %d", execution.FetchCalls, maxEvidenceResearchFetches)
	}
}

func TestExecutorFetchErrorCountsTowardBudget(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd, model.EvidenceCompetitionStart, model.EvidenceRegistrationStart, model.EvidenceCompetitionEnd)
	tools := &researchToolsFake{
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			var results []fetcher.ResearchSearchResult
			for i := 0; i < 10; i++ {
				results = append(results, fetcher.ResearchSearchResult{URL: "https://example.com/" + string(rune('a'+i))})
			}
			return results, nil
		},
		fetchFn: func(_ context.Context, _ string) (model.Document, error) {
			return model.Document{}, errors.New("always fails")
		},
	}
	extractor := &researchExtractorFake{}
	if _, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session); err != nil {
		t.Fatal(err)
	}
	// Fetch errors still consume budget (1 official + up to 4 search fetches).
	if tools.fetchCount() != maxEvidenceResearchFetches {
		t.Fatalf("fetch errors must still consume budget, got %d fetches", tools.fetchCount())
	}
}

func TestExecutorDuplicateURLFetchedOnce(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	competition.OfficialURL = "https://example.com/2026"
	tools := &researchToolsFake{
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			// The official URL also appears in search results.
			return []fetcher.ResearchSearchResult{{URL: "https://example.com/2026"}, {URL: "https://example.com/other"}}, nil
		},
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			return model.Document{URL: raw, Title: "页", Text: "无日期"}, nil
		},
	}
	extractor := &researchExtractorFake{}
	if _, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session); err != nil {
		t.Fatal(err)
	}
	// official fetched once; search result same URL must not be re-fetched.
	countOfficial := 0
	for _, fetched := range tools.fetches {
		if fetched == "https://example.com/2026" {
			countOfficial++
		}
	}
	if countOfficial != 1 {
		t.Fatalf("official URL fetched %d times, want 1", countOfficial)
	}
}

// --- Outcomes ---

func TestExecutorNoFactsIsUnresolved(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			return []fetcher.ResearchSearchResult{{URL: "https://example.com/a"}}, nil
		},
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			return model.Document{URL: raw, Title: "页", Text: "无日期"}, nil
		},
	}
	extractor := &researchExtractorFake{}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Fields[0].Outcome != evidenceResearchUnresolved {
		t.Fatalf("no facts after successful research must be unresolved, got %q", execution.Fields[0].Outcome)
	}
}

func TestExecutorSearchErrorIsRetryable(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			return nil, errors.New("search down")
		},
	}
	extractor := &researchExtractorFake{}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Fields[0].Outcome != evidenceResearchRetryable {
		t.Fatalf("search error must be retryable, got %q", execution.Fields[0].Outcome)
	}
}

func TestExecutorExtractorErrorIsRetryable(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			return model.Document{URL: raw, Title: "页", Text: "内容"}, nil
		},
	}
	extractor := &researchExtractorFake{
		extractFn: func(_ analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error) {
			return analyzer.ResearchEvidenceResult{}, errors.New("llm down")
		},
	}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Fields[0].Outcome != evidenceResearchRetryable {
		t.Fatalf("extractor error must be retryable, got %q", execution.Fields[0].Outcome)
	}
}

func TestExecutorPartialFoundNotOverwrittenByLaterError(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd, model.EvidenceCompetitionStart)
	tools := &researchToolsFake{
		fetchFn: func(_ context.Context, raw string) (model.Document, error) {
			if strings.Contains(raw, "competition_start") {
				return model.Document{}, errors.New("fetch error for competition page")
			}
			return model.Document{URL: raw, Title: "页", Text: "报名截止2026年4月9日"}, nil
		},
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			return []fetcher.ResearchSearchResult{{URL: "https://example.com/reg"}, {URL: "https://example.com/competition_start"}}, nil
		},
	}
	extractor := &researchExtractorFake{
		extractFn: func(req analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error) {
			var facts []analyzer.ResearchEvidenceFact
			for _, field := range req.Fields {
				if field == model.EvidenceRegistrationEnd {
					facts = append(facts, factFor(field, time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)))
				}
			}
			return analyzer.ResearchEvidenceResult{Facts: facts}, nil
		},
	}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Fields[0].Field != model.EvidenceRegistrationEnd || execution.Fields[0].Outcome != evidenceResearchFound {
		t.Fatalf("registration_end must stay found, got %+v", execution.Fields[0])
	}
	if execution.Fields[1].Field != model.EvidenceCompetitionStart || execution.Fields[1].Outcome != evidenceResearchRetryable {
		t.Fatalf("competition_start (fetch error) must be retryable, got %+v", execution.Fields[1])
	}
}

// --- Context / ordering ---

func TestExecutorCancelledContextBeforeStart(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd)
	tools := &researchToolsFake{}
	extractor := &researchExtractorFake{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	execution, err := executeEvidenceResearchSession(ctx, tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	if tools.searchCount() != 0 || tools.fetchCount() != 0 {
		t.Fatalf("cancelled context must not search/fetch")
	}
	if execution.Fields[0].Outcome != evidenceResearchRetryable {
		t.Fatalf("cancelled field must be retryable, got %q", execution.Fields[0].Outcome)
	}
}

func TestExecutorSessionContextCancellationStopsLoop(t *testing.T) {
	competition := executorTestCompetition()
	competition.OfficialURL = "" // go straight to search so the test can observe a search call
	session := executorTestSession(model.EvidenceRegistrationEnd, model.EvidenceCompetitionStart)
	ctx, cancel := context.WithCancel(context.Background())
	searchCalled := make(chan struct{}, 1)
	block := true
	tools := &researchToolsFake{
		searchFn: func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
			// First search returns one result; subsequent calls should not happen.
			select {
			case searchCalled <- struct{}{}:
			default:
			}
			return []fetcher.ResearchSearchResult{{URL: "https://example.com/a"}}, nil
		},
		fetchFn: func(c context.Context, _ string) (model.Document, error) {
			if block {
				<-c.Done() // block until context is cancelled
				return model.Document{}, c.Err()
			}
			return model.Document{URL: "x", Title: "页", Text: "无日期"}, nil
		},
	}
	extractor := &researchExtractorFake{}
	done := make(chan struct{})
	var execution evidenceResearchExecution
	var execErr error
	go func() {
		execution, execErr = executeEvidenceResearchSession(ctx, tools, extractor, competition, session)
		close(done)
	}()
	// Wait for the first fetch to start blocking, then cancel the parent context.
	<-searchCalled
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if execErr != nil {
		t.Fatal(execErr)
	}
	if execution.Fields[0].Outcome != evidenceResearchRetryable {
		t.Fatalf("cancelled mid-loop field must be retryable, got %q", execution.Fields[0].Outcome)
	}
	// After cancellation, no additional searches should be made.
	if tools.searchCount() != 1 {
		t.Fatalf("search calls=%d, expected exactly 1 (no extra after cancellation)", tools.searchCount())
	}
}

func TestExecutorDeterministicOutputOrder(t *testing.T) {
	competition := executorTestCompetition()
	// Request fields out of order; execution.Fields must be in fixed order.
	session := executorTestSession(model.EvidenceCompetitionEnd, model.EvidenceRegistrationEnd, model.EvidenceRegistrationStart, model.EvidenceCompetitionStart)
	tools := &researchToolsFake{}
	extractor := &researchExtractorFake{}
	// No edition fetch path issues: official URL fetch returns no facts; then search returns nothing.
	tools.fetchFn = func(_ context.Context, raw string) (model.Document, error) {
		return model.Document{URL: raw, Title: "页", Text: "无日期"}, nil
	}
	tools.searchFn = func(_ context.Context, _ fetcher.ResearchSearchRequest) ([]fetcher.ResearchSearchResult, error) {
		return nil, nil
	}
	execution, err := executeEvidenceResearchSession(context.Background(), tools, extractor, competition, session)
	if err != nil {
		t.Fatal(err)
	}
	want := []model.EvidenceField{
		model.EvidenceRegistrationStart,
		model.EvidenceRegistrationEnd,
		model.EvidenceCompetitionStart,
		model.EvidenceCompetitionEnd,
	}
	if len(execution.Fields) != len(want) {
		t.Fatalf("fields len=%d want %d", len(execution.Fields), len(want))
	}
	for i := range want {
		if execution.Fields[i].Field != want[i] {
			t.Fatalf("fields[%d]=%q want %q", i, execution.Fields[i].Field, want[i])
		}
	}
}

// --- Query bound ---

func TestExecutorLongChineseNameQueryBounded(t *testing.T) {
	longName := strings.Repeat("赛", 300) // 300 runes > 200 cap
	query := buildEvidenceResearchQuery(longName, "2026", []model.EvidenceField{model.EvidenceRegistrationEnd}, 1, "example.com")
	if utf8.RuneCountInString(query) > maxEvidenceResearchQueryRunes {
		t.Fatalf("query runes=%d exceed %d", utf8.RuneCountInString(query), maxEvidenceResearchQueryRunes)
	}
	// The query text itself must not contain the full unbounded name.
	if strings.Contains(query, longName) {
		t.Fatalf("unbounded name leaked into query")
	}
}

func TestExecutorQueryDeterministic(t *testing.T) {
	competition := executorTestCompetition()
	session := executorTestSession(model.EvidenceRegistrationEnd, model.EvidenceCompetitionStart)
	remaining := normalizeEvidenceResearchGaps(session.Gaps)
	q1 := buildEvidenceResearchQuery(competition.Name, "2026", remaining, 1, "example.com")
	q2 := buildEvidenceResearchQuery(competition.Name, "2026", remaining, 1, "example.com")
	if q1 != q2 {
		t.Fatalf("query must be deterministic: %q vs %q", q1, q2)
	}
}
