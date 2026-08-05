package fetcher

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"competition-assistant/internal/config"
)

func TestCanonicalURLUnwrapsWeChatCaptcha(t *testing.T) {
	raw := "https://mp.weixin.qq.com/mp/wappoc_appmsgcaptcha?target_url=https%3A%2F%2Fmp.weixin.qq.com%2Fs%2Fofficial123&utm_source=test"
	got := canonicalURL(raw)
	want := "https://mp.weixin.qq.com/s/official123"
	if got != want {
		t.Fatalf("canonicalURL() = %q, want %q", got, want)
	}
}

func TestCanonicalURLDoesNotTrustNonHTTPDestination(t *testing.T) {
	raw := "https://mp.weixin.qq.com/mp/wappoc_appmsgcaptcha?target_url=javascript%3Aalert%281%29"
	if got := canonicalURL(raw); got == "javascript:alert%281%29" || got == "javascript:alert(1)" {
		t.Fatalf("unsafe destination accepted: %q", got)
	}
}

func TestFetchExtractsArticleAndPublicationDate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><html><head><title>2026 AI Agent大赛预告</title><meta property="article:published_time" content="2026-08-03 10:30:00"></head><body>
<nav>2024年赛事报名通道现已开放 报名截止2024年6月1日</nav>
<article><h1>2026 AI Agent大赛预告</h1><p>新一届赛事即将启动，敬请期待。</p><p>赛事面向全国高校，聚焦智能体应用开发与RAG实践。</p></article>
</body></html>`)
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)
	document, err := collector.Fetch(context.Background(), testBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if document.PublishedAtRaw != "2026-08-03 10:30:00" {
		t.Fatalf("published_at=%q", document.PublishedAtRaw)
	}
	if document.IsListing || !strings.Contains(document.Text, "新一届赛事即将启动") || strings.Contains(document.Text, "2024年赛事") {
		t.Fatalf("unexpected article extraction: %#v", document)
	}
}

func TestFetchMarksLinkDensePageAsListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, "<html><head><title>赛事公告列表</title></head><body><main>")
		for index := 0; index < 24; index++ {
			_, _ = fmt.Fprintf(w, `<a href="/notice/%d">2026年第%d项程序设计赛事报名通知</a>`, index, index)
		}
		_, _ = fmt.Fprint(w, "</main></body></html>")
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)
	document, err := collector.Fetch(context.Background(), testBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if !document.IsListing {
		t.Fatalf("link-dense page was not classified as listing: %#v", document)
	}
}

func TestDiscoverPagePrioritizesCompetitionDetailLinks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head><title>活动列表</title></head><body><main>`)
		for index := 0; index < 20; index++ {
			_, _ = fmt.Fprintf(w, `<a href="/news/%d">校园普通新闻第%d条</a>`, index, index)
		}
		_, _ = fmt.Fprint(w, `<a href="/contest/2026">2026全国大学生程序设计大赛报名通知</a></main></body></html>`)
	}))
	defer server.Close()
	serverURL, _ := url.Parse(server.URL)
	// Use a public-style hostname so the extracted candidate links pass the
	// SSRF pre-filter, while routing actual requests to the httptest server.
	collector := &HTTPCollector{client: &http.Client{
		Timeout:   time.Second,
		Transport: &routeTransport{to: serverURL},
	}, maxBytes: 1 << 20}
	items, err := collector.Discover(context.Background(), config.Source{ID: "page", Name: "page", Kind: "page", URL: "https://contest.example.com/list", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if strings.Contains(item.Title, "程序设计大赛报名通知") {
			found = true
		}
	}
	if !found {
		t.Fatalf("competition detail link was crowded out: %#v", items)
	}
}

// routeTransport forwards every request to a fixed server while preserving the
// original path and query, letting tests exercise link extraction without real
// DNS or the SSRF transport.
type routeTransport struct{ to *url.URL }

func (r *routeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	proxy := *r.to
	proxy.Path = req.URL.Path
	proxy.RawQuery = req.URL.RawQuery
	cloned := req.Clone(req.Context())
	cloned.URL = &proxy
	resp, err := http.DefaultTransport.RoundTrip(cloned)
	if err != nil {
		return nil, err
	}
	// Keep the original public hostname on the response so doc.URL and the
	// extracted candidate links resolve to a public-style host, which lets the
	// SSRF pre-filter pass while requests actually hit the httptest server.
	resp.Request = req
	return resp, nil
}

// newTestFetchCollector returns a collector whose public client routes every
// request to the given (httptest) server. Tests that exercise HTML extraction,
// anti-bot or PDF handling fetch through a public-style base URL so the initial
// URL validation passes while the request lands on the test server.
func newTestFetchCollector(t *testing.T, serverURL string) *HTTPCollector {
	t.Helper()
	parsed, err := url.Parse(serverURL)
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
		searxngURL: parsed.String(),
		maxBytes:   5 << 20,
		maxRetries: 2,
	}
}

// testBaseURL is a public-style base used as the fetch target in HTML/PDF tests;
// newTestFetchCollector routes it to the httptest server.
const testBaseURL = "https://contest.example.com"

func TestPDFSegmentsPreservePageNumbers(t *testing.T) {
	segments := buildPDFSegments("第一页赛事介绍\f第二页报名截止时间为2026年9月20日")
	if len(segments) != 2 || segments[0].Page != 1 || segments[1].Page != 2 || !strings.Contains(segments[1].Text, "报名截止") {
		t.Fatalf("unexpected PDF segments: %#v", segments)
	}
}

func TestAntiBotDetectedRecognizesChallengePages(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		blocked bool
	}{
		{"normal announcement", `<html><title>2026 ICPC 报名通知</title><p>欢迎报名</p>`, false},
		{"cloudflare just a moment", `<html><body>Checking your browser... Just a moment</body>`, true},
		{"chinese captcha", `<html><body>请完成验证码，以继续访问</body>`, true},
		{"js challenge", `<html><body>Please enable JavaScript to continue</body>`, true},
		{"security verification", `<html><body>当前环境有风险，请完成安全验证</body>`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := antiBotDetected([]byte(tc.body)); got != tc.blocked {
				t.Fatalf("antiBotDetected() = %v, want %v", got, tc.blocked)
			}
		})
	}
}

func TestFetchSurfacesErrAntiBotForChallengePage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><body>请开启 JavaScript 以继续</body></html>`)
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)
	_, err := collector.Fetch(context.Background(), testBaseURL)
	if !errors.Is(err, ErrAntiBot) {
		t.Fatalf("Fetch() error = %v, want ErrAntiBot", err)
	}
}

func TestRetryableStatus(t *testing.T) {
	cases := map[int]bool{
		http.StatusOK:                  false,
		http.StatusNotFound:            false,
		http.StatusForbidden:           false,
		http.StatusTooManyRequests:     true,
		http.StatusRequestTimeout:      true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
	}
	for status, want := range cases {
		if got := retryableStatus(status); got != want {
			t.Fatalf("retryableStatus(%d) = %v, want %v", status, got, want)
		}
	}
}

// TestFetchDoesNotMistakeResourceURLsForAntiBot guards against a regression
// where "安全验证" / "验证码" appearing in a script or stylesheet URL on an
// otherwise legitimate page caused a false ErrAntiBot. Detection must run on
// the visible page text, never on the raw HTML.
func TestFetchDoesNotMistakeResourceURLsForAntiBot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head>
<link rel="stylesheet" href="/static/js/security_verify/js/index.css">
<script src="/static/js/captcha_verify.js"></script>
</head><body><article>2026 CCF CCSP 竞赛报名通知，报名时间 2026-09-15 至 2026-10-12。</article></body></html>`)
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)
	doc, err := collector.Fetch(context.Background(), testBaseURL)
	if err != nil {
		t.Fatalf("Fetch() unexpectedly rejected legitimate page: %v", err)
	}
	if !strings.Contains(doc.Text, "CCSP") {
		t.Fatalf("expected page body to be extracted, got %q", doc.Text)
	}
}

// TestFetchBlocksChallengePageInVisibleText confirms a page whose visible
// text really is a challenge (not just a resource URL) is still rejected.
func TestFetchBlocksChallengePageInVisibleText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head><script src="/static/app.js"></script></head>
<body><p>请完成安全验证，以继续访问该页面</p></body></html>`)
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)
	_, err := collector.Fetch(context.Background(), testBaseURL)
	if !errors.Is(err, ErrAntiBot) {
		t.Fatalf("Fetch() error = %v, want ErrAntiBot", err)
	}
}
