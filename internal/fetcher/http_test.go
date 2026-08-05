package fetcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	collector := &HTTPCollector{client: &http.Client{Timeout: time.Second}, maxBytes: 1 << 20}
	document, err := collector.Fetch(context.Background(), server.URL)
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
	collector := &HTTPCollector{client: &http.Client{Timeout: time.Second}, maxBytes: 1 << 20}
	document, err := collector.Fetch(context.Background(), server.URL)
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
	collector := &HTTPCollector{client: &http.Client{Timeout: time.Second}, maxBytes: 1 << 20}
	items, err := collector.Discover(context.Background(), config.Source{ID: "page", Name: "page", Kind: "page", URL: server.URL, Limit: 2})
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

func TestPDFSegmentsPreservePageNumbers(t *testing.T) {
	segments := buildPDFSegments("第一页赛事介绍\f第二页报名截止时间为2026年9月20日")
	if len(segments) != 2 || segments[0].Page != 1 || segments[1].Page != 2 || !strings.Contains(segments[1].Text, "报名截止") {
		t.Fatalf("unexpected PDF segments: %#v", segments)
	}
}
