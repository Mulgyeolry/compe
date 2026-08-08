package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/fetcher"
	"competition-assistant/internal/model"
)

// Evidence Research Phase 5: a bounded, deterministic+adaptive executor that
// drives Search → Fetch → Evidence Extractor for a due research session. It does
// NOT touch canonical data, research state, events or notifications; production
// Service.Run() is unchanged (integration lands in the next PR).

// Hard budget constants for one competition session (code-level, not config).
const (
	maxEvidenceResearchSearchRounds = 3
	maxEvidenceResearchFetches       = 5
	evidenceResearchSearchLimit      = 8
	evidenceResearchSessionTimeout   = 60 * time.Second
	// maxEvidenceResearchNameRunes caps the competition name used to build a
	// query (canonical name is external data).
	maxEvidenceResearchNameRunes = 200
	// maxEvidenceResearchQueryRunes caps the final generated query.
	maxEvidenceResearchQueryRunes = 500
	// maxEvidenceResearchErrorRunes caps the short operational-error diagnostic
	// stored on a field result.
	maxEvidenceResearchErrorRunes = 500
)

// evidenceResearchFieldOutcome is the per-field classification of an execution.
type evidenceResearchFieldOutcome string

const (
	// evidenceResearchFound means the executor obtained a deterministic-validated
	// candidate fact. found ≠ resolved: the canonical has not accepted it yet.
	evidenceResearchFound evidenceResearchFieldOutcome = "found"
	// evidenceResearchUnresolved means research completed normally but produced
	// no acceptable fact for the field.
	evidenceResearchUnresolved evidenceResearchFieldOutcome = "unresolved"
	// evidenceResearchRetryable means an operational error prevented a reliable
	// outcome (search/fetch/extractor error, context cancellation, deadline).
	evidenceResearchRetryable evidenceResearchFieldOutcome = "retryable"
)

// evidenceResearchFieldResult is the outcome for one requested field.
type evidenceResearchFieldResult struct {
	Field     model.EvidenceField
	Outcome   evidenceResearchFieldOutcome
	Fact      *analyzer.ResearchEvidenceFact
	LastError string
}

// evidenceResearchExecution is the full result of one session execution. A future
// Reconciler consumes it to decide canonical acceptance.
type evidenceResearchExecution struct {
	CompetitionID int64
	Edition       string

	Fields []evidenceResearchFieldResult

	Queries     []string
	SearchCalls int
	FetchCalls  int
}

// evidenceResearchExtractor is the minimal evidence-extraction dependency of the
// executor. *analyzer.Analyzer implements it; tests may use a fake.
type evidenceResearchExtractor interface {
	ExtractEvidenceFacts(context.Context, analyzer.ResearchEvidenceRequest) (analyzer.ResearchEvidenceResult, error)
}

// evidenceResearchFieldsFixedOrder is the stable order for gap normalization and
// execution output.
var evidenceResearchFieldsFixedOrder = []model.EvidenceField{
	model.EvidenceRegistrationStart,
	model.EvidenceRegistrationEnd,
	model.EvidenceCompetitionStart,
	model.EvidenceCompetitionEnd,
}

// evidenceResearchFieldKeywords maps each lifecycle field to the search keywords
// used to build deterministic queries.
var evidenceResearchFieldKeywords = map[model.EvidenceField][]string{
	model.EvidenceRegistrationStart: {"报名开始", "开放报名"},
	model.EvidenceRegistrationEnd:   {"报名截止"},
	model.EvidenceCompetitionStart:  {"比赛开始", "开赛", "赛程"},
	model.EvidenceCompetitionEnd:    {"比赛结束", "赛程"},
}

var researchYearPattern = regexp.MustCompile(`(?:19|20)\d{2}`)

// yearFromResearchText returns the first 4-digit year found in text (0 if none).
func yearFromResearchText(text string) int {
	if match := researchYearPattern.FindString(text); match != "" {
		year, _ := strconv.Atoi(match)
		return year
	}
	return 0
}

// evidenceResearchEdition derives the research edition deterministically from
// the canonical competition (Name year, known lifecycle dates, OfficialURL year).
// It never guesses with FirstSeen.Year() or time.Now().Year(). Conflicting
// explicit years make the session non-executable.
func evidenceResearchEdition(competition model.Competition) (string, error) {
	seen := make(map[int]bool)
	var years []int
	addYear := func(year int) {
		if year != 0 && !seen[year] {
			seen[year] = true
			years = append(years, year)
		}
	}
	addYear(yearFromResearchText(competition.Name))
	if competition.RegistrationStart != nil {
		addYear(competition.RegistrationStart.Year())
	}
	if competition.RegistrationEnd != nil {
		addYear(competition.RegistrationEnd.Year())
	}
	if competition.CompetitionStart != nil {
		addYear(competition.CompetitionStart.Year())
	}
	if competition.CompetitionEnd != nil {
		addYear(competition.CompetitionEnd.Year())
	}
	addYear(yearFromResearchText(competition.OfficialURL))
	if len(years) == 0 {
		return "", errors.New("cannot determine canonical research edition")
	}
	if len(years) > 1 {
		return "", fmt.Errorf("conflicting research edition years: %v", years)
	}
	return strconv.Itoa(years[0]), nil
}

// researchOfficialDomain extracts a Search AllowedDomains host from an OfficialURL.
// It only inspects http/https hostnames (lower-cased, port stripped, optional
// leading "www." removed). A parse failure yields "" and never aborts the
// executor. This is NOT a Fetch SSRF guard; real Fetch still relies on the
// SSRF-protected public client.
func researchOfficialDomain(officialURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(officialURL))
	if err != nil {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	host = strings.TrimPrefix(host, "www.")
	return host
}

// normalizeEvidenceResearchGaps deduplicates and fixes the order of a session's
// gap fields.
func normalizeEvidenceResearchGaps(gaps []model.EvidenceGap) []model.EvidenceField {
	present := make(map[model.EvidenceField]bool, len(gaps))
	for _, gap := range gaps {
		present[gap.Field] = true
	}
	var fields []model.EvidenceField
	for _, field := range evidenceResearchFieldsFixedOrder {
		if present[field] {
			fields = append(fields, field)
		}
	}
	return fields
}

// evidenceResearchQueryKeywords returns the deduplicated search keywords for a
// set of remaining fields, in field order.
func evidenceResearchQueryKeywords(fields []model.EvidenceField) []string {
	seen := make(map[string]bool)
	var keywords []string
	for _, field := range evidenceResearchFieldsFixedOrder {
		if !containsResearchField(fields, field) {
			continue
		}
		for _, kw := range evidenceResearchFieldKeywords[field] {
			if !seen[kw] {
				seen[kw] = true
				keywords = append(keywords, kw)
			}
		}
	}
	return keywords
}

func containsResearchField(fields []model.EvidenceField, target model.EvidenceField) bool {
	for _, field := range fields {
		if field == target {
			return true
		}
	}
	return false
}

// truncateRunesRuneSafe truncates a string to at most limit runes.
func truncateRunesRuneSafe(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// buildEvidenceResearchQuery produces a deterministic query for a search round
// based on the competition name, edition, remaining gaps, and round. Round 1 uses
// official-domain restriction when possible; later rounds relax it. The name is
// capped and the final query is bounded.
func buildEvidenceResearchQuery(name, edition string, remaining []model.EvidenceField, round int, officialDomain string) string {
	name = truncateRunesRuneSafe(strings.TrimSpace(name), maxEvidenceResearchNameRunes)
	keywords := evidenceResearchQueryKeywords(remaining)
	var base string
	switch round {
	case 1:
		base = fmt.Sprintf("%q %s 报名 截止 赛程", name, edition)
	case 2:
		base = fmt.Sprintf("%q %s 报名通知 比赛时间 赛程", name, edition)
	default: // round >= 3 fallback
		base = fmt.Sprintf("%q %s 报名截止 开赛 结束 官方 公告", name, edition)
	}
	if len(keywords) > 0 {
		base += " " + strings.Join(keywords, " ")
	}
	_ = officialDomain // domain restriction is applied at the Search call, not in the query text
	return truncateRunesRuneSafe(base, maxEvidenceResearchQueryRunes)
}

// executeEvidenceResearchSession drives one research session through the
// deterministic+adaptive agent loop. It returns a field-level execution result
// and never returns an error for normal research conditions (no facts found,
// empty search, etc.); only invalid input or broken invariants return an error.
func executeEvidenceResearchSession(
	ctx context.Context,
	tools fetcher.ResearchTools,
	extractor evidenceResearchExtractor,
	competition model.Competition,
	session model.ResearchSession,
) (evidenceResearchExecution, error) {
	if tools == nil {
		return evidenceResearchExecution{}, errors.New("evidence executor: nil tools")
	}
	if extractor == nil {
		return evidenceResearchExecution{}, errors.New("evidence executor: nil extractor")
	}
	if competition.ID < 1 {
		return evidenceResearchExecution{}, errors.New("evidence executor: invalid competition id")
	}
	if session.CompetitionID != competition.ID {
		return evidenceResearchExecution{}, fmt.Errorf("evidence executor: session competition %d does not match competition %d", session.CompetitionID, competition.ID)
	}
	if len(session.Gaps) == 0 {
		return evidenceResearchExecution{}, errors.New("evidence executor: session has no gaps")
	}
	for _, gap := range session.Gaps {
		if !model.ValidEvidenceField(gap.Field) {
			return evidenceResearchExecution{}, fmt.Errorf("evidence executor: invalid gap field %q", gap.Field)
		}
	}

	fields := normalizeEvidenceResearchGaps(session.Gaps)
	edition, err := evidenceResearchEdition(competition)
	if err != nil {
		// Cannot research deterministically: no explicit year anywhere. Do not
		// search; report every requested field as unresolved with a short reason.
		execution := evidenceResearchExecution{CompetitionID: competition.ID, Edition: ""}
		for _, field := range fields {
			execution.Fields = append(execution.Fields, evidenceResearchFieldResult{
				Field:     field,
				Outcome:   evidenceResearchUnresolved,
				LastError: "cannot determine canonical research edition",
			})
		}
		return execution, nil
	}

	sessionCtx, cancel := context.WithTimeout(ctx, evidenceResearchSessionTimeout)
	defer cancel()

	execution := evidenceResearchExecution{CompetitionID: competition.ID, Edition: edition}
	remaining := append([]model.EvidenceField{}, fields...)
	results := make(map[model.EvidenceField]*evidenceResearchFieldResult, len(fields))
	for _, field := range fields {
		results[field] = &evidenceResearchFieldResult{Field: field, Outcome: evidenceResearchUnresolved}
	}
	visited := make(map[string]bool)
	sawOperationalError := false

	stop := func() bool { return sessionCtx.Err() != nil }
	markErrorForAllRemaining := func(diagnostic string) {
		// Operational error affects every field that is still missing, and
		// pushes them toward retryable.
		sawOperationalError = true
		for _, field := range remaining {
			results[field].LastError = truncateRunesRuneSafe(diagnostic, maxEvidenceResearchErrorRunes)
		}
	}

	// Extract remaining fields from a fetched document and update results.
	extractAndResolve := func(doc model.Document) {
		if len(remaining) == 0 {
			return
		}
		result, extractErr := extractor.ExtractEvidenceFacts(sessionCtx, analyzer.ResearchEvidenceRequest{
			CompetitionName: competition.Name,
			Edition:         edition,
			Fields:          append([]model.EvidenceField{}, remaining...),
			Document:        doc,
		})
		if extractErr != nil {
			if sessionCtx.Err() != nil {
				markErrorForAllRemaining(sessionCtx.Err().Error())
				return
			}
			markErrorForAllRemaining(fmt.Sprintf("extractor error: %v", extractErr))
			return
		}
		for _, fact := range result.Facts {
			if !containsResearchField(remaining, fact.Field) {
				continue
			}
			accepted := fact
			results[fact.Field] = &evidenceResearchFieldResult{
				Field:   fact.Field,
				Outcome: evidenceResearchFound,
				Fact:    &accepted,
			}
			remaining = removeResearchField(remaining, fact.Field)
		}
	}

	tryFetch := func(rawURL string) {
		if stop() {
			markErrorForAllRemaining(sessionCtx.Err().Error())
			return
		}
		if visited[rawURL] {
			return
		}
		visited[rawURL] = true
		if execution.FetchCalls >= maxEvidenceResearchFetches {
			return
		}
		execution.FetchCalls++
		doc, fetchErr := tools.Fetch(sessionCtx, rawURL)
		if fetchErr != nil {
			if sessionCtx.Err() != nil {
				markErrorForAllRemaining(sessionCtx.Err().Error())
				return
			}
			markErrorForAllRemaining(fmt.Sprintf("fetch error: %v", fetchErr))
			return
		}
		extractAndResolve(doc)
	}

	// Official URL first: the cheapest, most trusted attempt. Counts toward the
	// Fetch budget. A failure falls through to Search.
	if strings.TrimSpace(competition.OfficialURL) != "" && !stop() {
		tryFetch(competition.OfficialURL)
	}

	// Search rounds, while gaps remain.
	round := 1
	for len(remaining) > 0 && round <= maxEvidenceResearchSearchRounds && !stop() {
		if execution.SearchCalls >= maxEvidenceResearchSearchRounds {
			break
		}
		domain := ""
		if round == 1 {
			domain = researchOfficialDomain(competition.OfficialURL)
		}
		query := buildEvidenceResearchQuery(competition.Name, edition, remaining, round, domain)
		execution.Queries = append(execution.Queries, query)
		execution.SearchCalls++
		resultsPayload, searchErr := tools.Search(sessionCtx, fetcher.ResearchSearchRequest{
			Query:          query,
			Limit:          evidenceResearchSearchLimit,
			AllowedDomains: nilIfEmpty(domain),
		})
		if searchErr != nil {
			if sessionCtx.Err() != nil {
				markErrorForAllRemaining(sessionCtx.Err().Error())
				break
			}
			markErrorForAllRemaining(fmt.Sprintf("search error: %v", searchErr))
			break
		}
		for _, result := range resultsPayload {
			if len(remaining) == 0 {
				break
			}
			if stop() {
				break
			}
			tryFetch(result.URL)
		}
		round++
	}

	// If context cancelled / deadline, mark still-missing fields retryable.
	if sessionCtx.Err() != nil {
		markErrorForAllRemaining(sessionCtx.Err().Error())
	}

	// Finalize outcome classification for still-unresolved fields.
	for _, field := range fields {
		res := results[field]
		if res.Outcome == evidenceResearchFound {
			continue
		}
		if sawOperationalError {
			res.Outcome = evidenceResearchRetryable
		} else {
			res.Outcome = evidenceResearchUnresolved
		}
	}

	// Deterministic output order: requested fields in fixed order.
	for _, field := range fields {
		execution.Fields = append(execution.Fields, *results[field])
	}
	return execution, nil
}

func nilIfEmpty(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

func removeResearchField(fields []model.EvidenceField, target model.EvidenceField) []model.EvidenceField {
	var result []model.EvidenceField
	for _, field := range fields {
		if field != target {
			result = append(result, field)
		}
	}
	return result
}
