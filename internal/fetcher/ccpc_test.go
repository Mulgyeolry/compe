package fetcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"competition-assistant/internal/config"
)

// ccpcTestServer returns a mock server that handles the CCPC archive list and
// article detail API endpoints used by the ccpc_api adapter.
func ccpcTestServer(t *testing.T, listBody, detailBody string, detailStatus int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case r.URL.Path == "/api/archive" && r.URL.Query().Get("id") == "":
			_, _ = w.Write([]byte(listBody))
		case strings.HasPrefix(r.URL.Path, "/api/archive/"):
			w.WriteHeader(detailStatus)
			if detailStatus == http.StatusOK {
				_, _ = w.Write([]byte(detailBody))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":0,"msg":"not found"}`))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

const ccpcListJSON = `{"status":1,"msg":"获取成功","data":[
	{"id":377,"title":"第11届中国大学生程序设计竞赛（CCPC）总决赛","publishtime":1785945600},
	{"id":381,"title":"2026 CCPC 网络赛通知","publishtime":1786032000},
	{"id":386,"title":"组委会成员","publishtime":1786118400}
]}`

const ccpcDetailJSON = `{"status":1,"msg":"获取成功","data":{"archivesInfo":{
	"id":377,
	"title":"第11届中国大学生程序设计竞赛（CCPC）总决赛",
	"publishtime":1785945600,
	"content":"<p>第11届中国大学生程序设计竞赛（CCPC）总决赛将于<strong>2026年4月25~26</strong>在南阳举行，承办学校为南阳理工学院。</p><p>附件：<a href=\"/files/a.pdf\">总决赛名额公示.pdf</a>、<a href=\"/files/b.pdf\">总决赛邀请函.pdf</a></p>"
}}}`

func TestCCPCDiscoverReturnsCandidatesAndLimit(t *testing.T) {
	server := ccpcTestServer(t, ccpcListJSON, "", http.StatusOK)
	collector := newTestFetchCollector(t, server.URL)
	source := config.Source{ID: "ccpc", Name: "CCPC 官网", Kind: "ccpc_api", URL: "https://ccpc.io/", Limit: 2}
	items, err := collector.Discover(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("candidates = %d, want limit 2", len(items))
	}
	// order preserved, public article URL used, no apikey leakage
	if items[0].Title != "第11届中国大学生程序设计竞赛（CCPC）总决赛" {
		t.Errorf("candidate[0].Title = %q", items[0].Title)
	}
	if items[0].URL != "https://ccpc.io/a/377.html" {
		t.Errorf("candidate[0].URL = %q, want public article URL", items[0].URL)
	}
	if items[0].SourceID != "ccpc" {
		t.Errorf("candidate[0].SourceID = %q", items[0].SourceID)
	}
	for _, it := range items {
		if strings.Contains(it.URL, "apikey") || strings.Contains(it.URL, "84dfa45f") || strings.Contains(it.URL, "ccpc.io/api") {
			t.Errorf("candidate URL leaks apikey/API path: %q", it.URL)
		}
	}
}

func TestCCPCFetchDetailToDocument(t *testing.T) {
	server := ccpcTestServer(t, ccpcListJSON, ccpcDetailJSON, http.StatusOK)
	collector := newTestFetchCollector(t, server.URL)
	document, err := collector.Fetch(context.Background(), "https://ccpc.io/a/377.html")
	if err != nil {
		t.Fatal(err)
	}
	if document.Title != "第11届中国大学生程序设计竞赛（CCPC）总决赛" {
		t.Errorf("title = %q", document.Title)
	}
	if document.URL != "https://ccpc.io/a/377.html" {
		t.Errorf("document.URL = %q, must stay public article URL", document.URL)
	}
	if document.IsListing {
		t.Error("CCPC article must not be a listing")
	}
	if !strings.Contains(document.Text, "2026年4月25~26") {
		t.Errorf("text missing competition date: %q", document.Text)
	}
	// HTML stripped: no <p>/<strong>/<a> tags remain
	if strings.Contains(document.Text, "<p>") || strings.Contains(document.Text, "<strong>") || strings.Contains(document.Text, "<a href") {
		t.Errorf("text still contains HTML tags: %q", document.Text)
	}
	// attachment titles preserved
	if !strings.Contains(document.Text, "总决赛名额公示.pdf") || !strings.Contains(document.Text, "总决赛邀请函.pdf") {
		t.Errorf("attachment titles not preserved: %q", document.Text)
	}
	if document.PublishedAtRaw == "" {
		t.Error("PublishedAtRaw should be set from publishtime")
	}
	// no apikey in document URL
	if strings.Contains(document.URL, "apikey") || strings.Contains(document.URL, "84dfa45f") {
		t.Errorf("document URL leaks apikey: %q", document.URL)
	}
}

func TestCCPCInvalidJSONErrors(t *testing.T) {
	server := ccpcTestServer(t, `not json`, `also not json`, http.StatusOK)
	collector := newTestFetchCollector(t, server.URL)
	if _, err := collector.Discover(context.Background(), config.Source{ID: "ccpc", Kind: "ccpc_api", URL: "https://ccpc.io/", Limit: 5}); err == nil {
		t.Fatal("Discover should fail on invalid JSON")
	}
	if _, err := collector.Fetch(context.Background(), "https://ccpc.io/a/377.html"); err == nil {
		t.Fatal("Fetch should fail on invalid JSON")
	}
}

func TestCCPCNonOKStatusErrors(t *testing.T) {
	// status != 1 in payload must error even though HTTP is 200.
	server := ccpcTestServer(t, `{"status":0,"msg":"获取失败"}`, `{"status":0,"msg":"获取失败"}`, http.StatusOK)
	collector := newTestFetchCollector(t, server.URL)
	if _, err := collector.Discover(context.Background(), config.Source{ID: "ccpc", Kind: "ccpc_api", URL: "https://ccpc.io/", Limit: 5}); err == nil {
		t.Fatal("Discover should error on non-1 API status")
	}
	if _, err := collector.Fetch(context.Background(), "https://ccpc.io/a/377.html"); err == nil {
		t.Fatal("Fetch should error on non-1 API status")
	}
}

func TestCCPCNonCCPCURLFallsThrough(t *testing.T) {
	// A non-ccpc.io URL must not be routed to the CCPC API adapter.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head><title>普通页面</title></head><body><main><p>这是一个普通页面的正文内容，报名时间：2026年5月1日。</p></main></body></html>`)
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)
	document, err := collector.Fetch(context.Background(), "https://contest.example.com/a/377.html")
	if err != nil {
		t.Fatal(err)
	}
	// Should be treated as a normal HTML page, not the CCPC detail API.
	if !strings.Contains(document.Text, "普通页面的正文") {
		t.Errorf("non-CCPC URL should fall through to normal fetch, got %q", document.Text)
	}
}
