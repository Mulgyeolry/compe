package fetcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"competition-assistant/internal/model"
)

// Evidence Research Phase 3 exposes two controlled tools for a future Research
// Agent: Search and Fetch. This file only defines the bounded Search tool and
// the shared SearXNG core; it does not implement an Agent.

const (
	// maxResearchSearchQueryRunes is the hard upper bound on a research query.
	// A query longer than this is rejected (never silently truncated) so budget
	// mistakes surface early.
	maxResearchSearchQueryRunes = 500
	// defaultResearchSearchLimit applies when ResearchSearchRequest.Limit is 0.
	defaultResearchSearchLimit = 10
	// maxResearchSearchLimit is the hard cap on results returned by one research
	// Search. It bounds how much text can enter the future Agent's context.
	maxResearchSearchLimit = 20
	// maxResearchSearchTitleRunes bounds each result title.
	maxResearchSearchTitleRunes = 300
	// maxResearchSearchSnippetRunes bounds each result snippet.
	maxResearchSearchSnippetRunes = 1000
)

// ResearchSearchRequest is the bounded input to a research Search tool call.
// The caller (future Agent) may control only the query, the result limit and an
// optional domain allow-list. It can never select the search backend: the
// endpoint is always the configured SearXNG instance.
type ResearchSearchRequest struct {
	// Query is the search query. It is trimmed and must be non-empty and at most
	// maxResearchSearchQueryRunes runes.
	Query string
	// Limit is the maximum number of results to return. 0 means
	// defaultResearchSearchLimit (10); values above maxResearchSearchLimit (20)
	// are rejected.
	Limit int
	// AllowedDomains, when non-empty, restricts results to the exact domain or
	// its subdomains (reusing allowedURL). Empty means no domain restriction
	// beyond public http(s) URL safety.
	AllowedDomains []string
}

// ResearchSearchResult is a discovery clue returned by Search. Its Snippet is a
// search-engine excerpt, NOT evidence: canonical evidence must come from a
// subsequent Fetch(url) of the result, which reads the real page through the
// SSRF-protected public client. Storing the snippet as official evidence is
// explicitly disallowed.
type ResearchSearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// ResearchSearcher is the minimal search capability a future Agent may use.
type ResearchSearcher interface {
	Search(context.Context, ResearchSearchRequest) ([]ResearchSearchResult, error)
}

// ResearchTools is the full, controlled tool surface handed to a future Research
// Agent: it may only Search (against the configured SearXNG) and Fetch (through
// the SSRF-protected public client). It never exposes raw HTTP, database writes,
// notifications or canonical updates.
type ResearchTools interface {
	ResearchSearcher
	Fetch(context.Context, string) (model.Document, error)
}

// compile-time assertion: the production collector must keep providing the full
// ResearchTools surface, so a future refactor cannot silently drop it.
var _ ResearchTools = (*HTTPCollector)(nil)

// Search executes a bounded research query against the configured SearXNG
// instance. It validates the request, then delegates to the shared searchSearx
// core. Search returns discovery clues only; it never fetches the result pages
// itself.
func (c *HTTPCollector) Search(ctx context.Context, req ResearchSearchRequest) ([]ResearchSearchResult, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return nil, errors.New("research search query must not be empty")
	}
	if utf8.RuneCountInString(query) > maxResearchSearchQueryRunes {
		return nil, fmt.Errorf("research search query exceeds %d runes", maxResearchSearchQueryRunes)
	}
	limit := req.Limit
	if limit == 0 {
		limit = defaultResearchSearchLimit
	}
	if limit < 1 || limit > maxResearchSearchLimit {
		return nil, fmt.Errorf("research search limit must be between 1 and %d", maxResearchSearchLimit)
	}
	return c.searchSearx(ctx, query, limit, req.AllowedDomains)
}
