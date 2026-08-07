package fetcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"competition-assistant/internal/config"
)

// TestDiscoverPageFindsCSPDetailLinksFromCMSListing proves that the existing
// page collector can discover CSP registration-notice detail links from the
// CMS listing URL that ccf-csp now points at (service-rendered, GET-able),
// unlike the old frameset homepage which yielded no <a> links.
func TestDiscoverPageFindsCSPDetailLinksFromCMSListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head><title>CCF CSP 通知公告</title></head><body>
<main>
<h2>通知公告</h2>
<ul>
<li><a href="https://contest.example.com/cms/show.action?code=publish_detail_a&siteid=100000">第42次CCF CSP认证（2026年5月31日）报名通知</a></li>
<li><a href="https://contest.example.com/cms/show.action?code=publish_detail_b&siteid=100000">第42次CCF CSP认证成绩查询及复议通知</a></li>
<li><a href="https://contest.example.com/about">关于我们</a></li>
</ul>
</main></body></html>`)
	}))
	defer server.Close()
	// Use a public-style hostname for the source so the SSRF pre-filter passes,
	// while routeTransport forwards the request to the httptest server.
	collector := newTestFetchCollector(t, server.URL)
	source := config.Source{ID: "ccf-csp", Name: "CCF CSP 官网", Kind: "page", URL: "https://contest.example.com/cms/show.action?code=publish_list&siteid=100000", Trust: "high", Limit: 10}
	candidates, err := collector.Discover(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, candidate := range candidates {
		if strings.Contains(candidate.Title, "CSP认证") && strings.Contains(candidate.Title, "报名通知") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("discoverPage did not surface the CSP registration-notice detail link from the CMS listing; got %#v", candidates)
	}
}

// TestFetchGetsCSPDetailBody proves the fetcher extracts the full detail body
// (including dates) from a CSP CMS detail page served over plain HTTP.
func TestFetchGetsCSPDetailBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head><title>第42次CCF CSP认证报名通知</title></head><body>
<main class="article-content">
<p>2026年CCF CSP软件能力认证报名通道已开启。</p>
<p>报名时间：2026年5月1日8:00至2026年5月25日17:00。</p>
<p>认证时间：2026年5月31日。</p>
</main></body></html>`)
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)
	document, err := collector.Fetch(context.Background(), testBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if document.IsListing {
		t.Fatalf("CSP detail page misclassified as listing: %#v", document)
	}
	if !strings.Contains(document.Text, "报名时间") || !strings.Contains(document.Text, "2026年5月1日") {
		t.Fatalf("did not extract CSP detail body with dates: %#v", document)
	}
}

// TestCcfCspSourceNoLongerPointsAtFramesetHomepage guards the
// sources.example.yaml change: the ccf-csp page source must use the GET-able
// CMS listing URL and not the frameset homepage. The committed template
// (sources.example.yaml) is checked, not the gitignored local sources.yaml.
func TestCcfCspSourceNoLongerPointsAtFramesetHomepage(t *testing.T) {
	raw, err := os.ReadFile("../../sources.example.yaml")
	if err != nil {
		t.Fatalf("read sources.example.yaml: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "id: ccf-csp") {
		t.Fatal("sources.example.yaml missing ccf-csp source")
	}
	if strings.Contains(content, "id: ccf-csp\n    name: CCF CSP 官网\n    kind: page\n    url: https://www.cspro.org/\n") {
		t.Fatal("ccf-csp still points at the frameset homepage https://www.cspro.org/")
	}
	if !strings.Contains(content, "cms/show.action") {
		t.Fatal("ccf-csp does not reference the GET-able CMS listing URL")
	}
}
