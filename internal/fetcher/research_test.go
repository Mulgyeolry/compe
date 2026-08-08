package fetcher

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"competition-assistant/internal/config"
)

// searxTestServer returns an httptest server that responds to SearXNG's
// /search?format=json endpoint with the given payload, counting hits.
func searxTestServer(t *testing.T, payload string, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(payload))
		}
	}))
	t.Cleanup(server.Close)
	return server, hits
}

// searxResultJSON builds a SearXNG JSON response from the given result tuples
// (title, url, content).
func searxResultJSON(results ...[]string) string {
	type searxItem struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	}
	items := make([]searxItem, 0, len(results))
	for _, r := range results {
		items = append(items, searxItem{Title: r[0], URL: r[1], Content: r[2]})
	}
	encoded, _ := json.Marshal(map[string]any{"results": items})
	return string(encoded)
}

// researchCollector builds an HTTPCollector whose SearXNG is the given httptest
// server. The public client is routed via routeTransport so it would hit the
// same server if a Search ever tried to fetch a result URL (which it must not).
func researchCollector(t *testing.T, server *httptest.Server) *HTTPCollector {
	t.Helper()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &HTTPCollector{
		client: &http.Client{
			Timeout:       20 * time.Second,
			CheckRedirect: publicCheckRedirect,
			Transport:     &routeTransport{to: parsed},
		},
		serviceClient: &http.Client{
			Timeout:       20 * time.Second,
			CheckRedirect: serviceCheckRedirect,
		},
		searxngURL: server.URL,
		maxBytes:   5 << 20,
		maxRetries: 0,
	}
}

func TestResearchSearchValidation(t *testing.T) {
	server, _ := searxTestServer(t, searxResultJSON(), http.StatusOK)
	collector := researchCollector(t, server)
	ctx := context.Background()

	if _, err := collector.Search(ctx, ResearchSearchRequest{Query: "", Limit: 5}); err == nil {
		t.Fatal("empty query must be rejected")
	}
	if _, err := collector.Search(ctx, ResearchSearchRequest{Query: "   \t ", Limit: 5}); err == nil {
		t.Fatal("whitespace-only query must be rejected")
	}
	longQuery := strings.Repeat("a", maxResearchSearchQueryRunes+1)
	if _, err := collector.Search(ctx, ResearchSearchRequest{Query: longQuery, Limit: 5}); err == nil {
		t.Fatal("overlong query must be rejected")
	}
	if _, err := collector.Search(ctx, ResearchSearchRequest{Query: "ccpc 2026", Limit: 0}); err != nil {
		t.Fatalf("Limit=0 should default, got err %v", err)
	}
	if _, err := collector.Search(ctx, ResearchSearchRequest{Query: "ccpc 2026", Limit: 1}); err != nil {
		t.Fatalf("Limit=1 must be accepted, got %v", err)
	}
	if _, err := collector.Search(ctx, ResearchSearchRequest{Query: "ccpc 2026", Limit: maxResearchSearchLimit}); err != nil {
		t.Fatalf("Limit=%d must be accepted, got %v", maxResearchSearchLimit, err)
	}
	if _, err := collector.Search(ctx, ResearchSearchRequest{Query: "ccpc 2026", Limit: maxResearchSearchLimit + 1}); err == nil {
		t.Fatalf("Limit=%d must be rejected", maxResearchSearchLimit+1)
	}
}

func TestResearchSearchLimitDefaultAndReject(t *testing.T) {
	server, _ := searxTestServer(t, searxResultJSON(), http.StatusOK)
	collector := researchCollector(t, server)
	ctx := context.Background()
	// Limit=0 maps to default 10; Limit 1..20 accepted; >20 rejected.
	if err := validateResearchSearchRequestForTest(collector, ctx, 0); err != nil {
		t.Fatalf("Limit=0 (default) failed: %v", err)
	}
	if err := validateResearchSearchRequestForTest(collector, ctx, 20); err != nil {
		t.Fatalf("Limit=20 failed: %v", err)
	}
	if err := validateResearchSearchRequestForTest(collector, ctx, 21); err == nil {
		t.Fatal("Limit=21 must be rejected")
	}
}

func validateResearchSearchRequestForTest(c *HTTPCollector, ctx context.Context, limit int) error {
	_, err := c.Search(ctx, ResearchSearchRequest{Query: "ccpc 2026", Limit: limit})
	return err
}

func TestResearchSearchUsesServiceClientAndEncodesQuery(t *testing.T) {
	// Dedicated server that captures the raw request query so we can assert the
	// collector URL-encodes the query on the wire.
	var rawQuery string
	payload := searxResultJSON(
		[]string{"CCPC 2026", "https://official.example.com/a", "内容一"},
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(server.Close)

	collector := researchCollector(t, server)
	ctx := context.Background()
	query := "CCPC 总决赛 2026"
	if _, err := collector.Search(ctx, ResearchSearchRequest{Query: query, Limit: 5}); err != nil {
		t.Fatal(err)
	}
	// The raw query on the wire must be URL-encoded: no literal space, and the
	// encoded form must equal url.QueryEscape(query).
	expected := "format=json&q=" + url.QueryEscape(query)
	if rawQuery != expected {
		t.Fatalf("raw query = %q, want %q (query must be URL-encoded)", rawQuery, expected)
	}
}

func TestResearchSearchMalformedJSON(t *testing.T) {
	server, _ := searxTestServer(t, `not json`, http.StatusOK)
	collector := researchCollector(t, server)
	if _, err := collector.Search(context.Background(), ResearchSearchRequest{Query: "ccpc", Limit: 5}); err == nil {
		t.Fatal("malformed JSON must error")
	}
}

func TestResearchSearchNon2xx(t *testing.T) {
	server, _ := searxTestServer(t, `{}`, http.StatusInternalServerError)
	collector := researchCollector(t, server)
	if _, err := collector.Search(context.Background(), ResearchSearchRequest{Query: "ccpc", Limit: 5}); err == nil {
		t.Fatal("non-2xx must error")
	}
}

func TestResearchSearchContextCancellation(t *testing.T) {
	// A handler that blocks until the context is cancelled.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	collector := researchCollector(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := collector.Search(ctx, ResearchSearchRequest{Query: "ccpc", Limit: 5})
	if err == nil {
		t.Fatal("cancelled context must return an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestResearchSearchFiltersUnsafeURLs(t *testing.T) {
	payload := searxResultJSON(
		[]string{"safe1", "https://official.example.com/a", "s1"},
		[]string{"safe2", "https://sub.official.example.com/b", "s2"},
		[]string{"js", "javascript:alert(1)", "s3"},
		[]string{"ftp", "ftp://example.com/f", "s4"},
		[]string{"localhost", "http://localhost/x", "s5"},
		[]string{"loopback", "http://127.0.0.1/x", "s6"},
	)
	server, _ := searxTestServer(t, payload, http.StatusOK)
	collector := researchCollector(t, server)
	results, err := collector.Search(context.Background(), ResearchSearchRequest{Query: "ccpc", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, r := range results {
		urls = append(urls, r.URL)
	}
	if len(urls) != 2 || urls[0] != "https://official.example.com/a" || urls[1] != "https://sub.official.example.com/b" {
		t.Fatalf("expected only 2 safe public URLs, got %v", urls)
	}
}

func TestResearchSearchAllowedDomains(t *testing.T) {
	payload := searxResultJSON(
		[]string{"official", "https://official.example.com/a", "s1"},
		[]string{"sub", "https://sub.official.example.com/b", "s2"},
		[]string{"evil-suffix", "https://official.example.com.evil.test/c", "s3"},
		[]string{"evil-prefix", "https://evilofficial.example.com/d", "s4"},
	)
	server, _ := searxTestServer(t, payload, http.StatusOK)
	collector := researchCollector(t, server)
	results, err := collector.Search(context.Background(), ResearchSearchRequest{
		Query:          "ccpc",
		Limit:          20,
		AllowedDomains: []string{"official.example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var urls []string
	for _, r := range results {
		urls = append(urls, r.URL)
	}
	if len(urls) != 2 || urls[0] != "https://official.example.com/a" || urls[1] != "https://sub.official.example.com/b" {
		t.Fatalf("allowed-domains filtering wrong, got %v", urls)
	}
}

func TestResearchSearchNoImplicitFetch(t *testing.T) {
	payload := searxResultJSON(
		[]string{"result", "https://example.com/result", "content"},
	)
	server, hits := searxTestServer(t, payload, http.StatusOK)
	collector := researchCollector(t, server)
	_, err := collector.Search(context.Background(), ResearchSearchRequest{Query: "ccpc", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	// Only the SearXNG /search request may have been made; the result URL must
	// NOT have been fetched (the public routeTransport would also hit this
	// server if it had tried).
	if hits.Load() != 1 {
		t.Fatalf("Search must not fetch result URLs, got %d requests", hits.Load())
	}
}

func TestResearchSearchDeduplicatesCanonicalURLs(t *testing.T) {
	payload := searxResultJSON(
		[]string{"a", "https://example.com/page?utm_source=x", "content a"},
		[]string{"b", "https://example.com/page", "content b"},
		[]string{"c", "https://example.com/page#frag", "content c"},
	)
	server, _ := searxTestServer(t, payload, http.StatusOK)
	collector := researchCollector(t, server)
	results, err := collector.Search(context.Background(), ResearchSearchRequest{Query: "ccpc", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 deduplicated result, got %d: %+v", len(results), results)
	}
	if results[0].URL != "https://example.com/page" {
		t.Fatalf("canonical URL wrong: %q", results[0].URL)
	}
	// SearXNG original order preserved: the first occurrence wins.
	if results[0].Title != "a" {
		t.Fatalf("first occurrence should win, got title %q", results[0].Title)
	}
}

func TestResearchSearchOutputBounds(t *testing.T) {
	longTitle := strings.Repeat("标", 400)
	longSnippet := strings.Repeat("测", 1200)
	payload := searxResultJSON(
		[]string{longTitle, "https://example.com/a", longSnippet},
	)
	server, _ := searxTestServer(t, payload, http.StatusOK)
	collector := researchCollector(t, server)
	results, err := collector.Search(context.Background(), ResearchSearchRequest{Query: "ccpc", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if utf8.RuneCountInString(results[0].Title) != maxResearchSearchTitleRunes {
		t.Fatalf("title runes=%d, want %d", utf8.RuneCountInString(results[0].Title), maxResearchSearchTitleRunes)
	}
	if utf8.RuneCountInString(results[0].Snippet) != maxResearchSearchSnippetRunes {
		t.Fatalf("snippet runes=%d, want %d", utf8.RuneCountInString(results[0].Snippet), maxResearchSearchSnippetRunes)
	}
	if !utf8.ValidString(results[0].Title) || !utf8.ValidString(results[0].Snippet) {
		t.Fatalf("truncated title/snippet are not valid UTF-8")
	}
}

func TestDiscoverSearchReusesSharedCoreNoRegression(t *testing.T) {
	payload := searxResultJSON(
		[]string{"CCPC 2026 总决赛", "https://ccpc.example.com/2026", "2026 CCPC 总决赛通知"},
		[]string{"other", "https://other.example.com/x", "其他内容"},
	)
	server, _ := searxTestServer(t, payload, http.StatusOK)
	collector := researchCollector(t, server)
	// source.Limit above the research cap (20) must still apply to the
	// configured-source path: a limit of 40 must not be clamped to 20.
	source := config.Source{
		ID:             "ccpc-search",
		Name:           "CCPC 搜索",
		Kind:           "search",
		Query:          "{year} CCPC 总决赛",
		Limit:          40,
		AllowedDomains: []string{"ccpc.example.com"},
	}
	candidates, err := collector.Discover(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate after allowed-domain filter, got %d", len(candidates))
	}
	if candidates[0].SourceID != "ccpc-search" || candidates[0].SourceName != "CCPC 搜索" {
		t.Fatalf("source id/name not preserved: %+v", candidates[0])
	}
	if candidates[0].URL != "https://ccpc.example.com/2026" {
		t.Fatalf("unexpected candidate URL: %q", candidates[0].URL)
	}
}

func TestResearchToolsInterfaceImplemented(t *testing.T) {
	// Compile-time guarantee: HTTPCollector must satisfy ResearchTools. This
	// compiles only if the production collector still provides Search + Fetch.
	var _ ResearchTools = (*HTTPCollector)(nil)
}

func TestResearchToolsExposesFetch(t *testing.T) {
	// Fetch remains available through ResearchTools and returns a Document.
	docURL := "https://contest.example.com/article"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>官方公告</title></head><body><main><p>2026年4月25日比赛开始。</p></main></body></html>`))
	}))
	t.Cleanup(server.Close)
	collector := researchCollector(t, server)
	doc, err := collector.Fetch(context.Background(), docURL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Text, "比赛开始") {
		t.Fatalf("Fetch via ResearchTools did not return the page text: %q", doc.Text)
	}
	if doc.URL != docURL {
		t.Fatalf("document URL should remain the public target, got %q", doc.URL)
	}
}

// TestResearchSearchRejectsCanonicalLoopback verifies that a raw wrapper URL
// whose canonical target unwraps to a loopback address is rejected.
func TestResearchSearchRejectsCanonicalLoopback(t *testing.T) {
	payload := searxResultJSON(
		[]string{"wrapped", "https://mp.weixin.qq.com/wappoc_appmsgcaptcha?target_url=http%3A%2F%2F127.0.0.1%2Fsecret", "content"},
	)
	server, _ := searxTestServer(t, payload, http.StatusOK)
	collector := researchCollector(t, server)
	results, err := collector.Search(context.Background(), ResearchSearchRequest{Query: "ccpc", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("canonical loopback must be rejected, got %+v", results)
	}
}

// TestResearchSearchRejectsCanonicalLocalhost verifies a canonical target that
// unwraps to localhost is rejected.
func TestResearchSearchRejectsCanonicalLocalhost(t *testing.T) {
	payload := searxResultJSON(
		[]string{"wrapped", "https://mp.weixin.qq.com/wappoc_appmsgcaptcha?target_url=http%3A%2F%2Flocalhost%2Fsecret", "content"},
	)
	server, _ := searxTestServer(t, payload, http.StatusOK)
	collector := researchCollector(t, server)
	results, err := collector.Search(context.Background(), ResearchSearchRequest{Query: "ccpc", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("canonical localhost must be rejected, got %+v", results)
	}
}

// TestResearchSearchRejectsCanonicalOutsideAllowedDomains verifies that a raw
// wrapper whose canonical destination belongs to a non-allowed domain is
// rejected even when the raw wrapper itself matches the allow-list.
func TestResearchSearchRejectsCanonicalOutsideAllowedDomains(t *testing.T) {
	// canonicalURL unwraps the WeChat captcha wrapper's target_url. The raw URL
	// is on mp.weixin.qq.com (which the allow-list permits), so the raw check
	// passes; the canonical destination evil.example.net must then be rejected by
	// the post-validation because it is not in the allow-list.
	payload := searxResultJSON(
		[]string{"wrapped", "https://mp.weixin.qq.com/wappoc_appmsgcaptcha?target_url=https%3A%2F%2Fevil.example.net%2Fpage", "content"},
	)
	server, _ := searxTestServer(t, payload, http.StatusOK)
	collector := researchCollector(t, server)
	results, err := collector.Search(context.Background(), ResearchSearchRequest{
		Query:          "ccpc",
		Limit:          20,
		AllowedDomains: []string{"mp.weixin.qq.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("canonical destination outside allowed domains must be rejected, got %+v", results)
	}
}

// TestResearchSearchCanonicalTrackingURLStillWorks verifies normal tracking URL
// canonicalization still returns the clean destination.
func TestResearchSearchCanonicalTrackingURLStillWorks(t *testing.T) {
	payload := searxResultJSON(
		[]string{"page", "https://example.com/page?utm_source=x#frag", "content"},
	)
	server, _ := searxTestServer(t, payload, http.StatusOK)
	collector := researchCollector(t, server)
	results, err := collector.Search(context.Background(), ResearchSearchRequest{Query: "ccpc", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].URL != "https://example.com/page" {
		t.Fatalf("canonical tracking URL should normalize, got %+v", results)
	}
}
