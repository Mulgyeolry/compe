package fetcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestJSRedirectFollowsEmptyShellToRealBody covers the CCF/CSPro-style empty
// shell that only contains `window.location.href = "..."`; the fetcher must
// follow it once to the same-host relative target and return the real body.
func TestJSRedirectFollowsEmptyShellToRealBody(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/cms/shell":
			_, _ = fmt.Fprint(w, `<html><head><title>shell</title></head><body><script>window.location.href="/cms/show.action?code=abc&newsid=42";</script></body></html>`)
		case "/cms/show.action":
			_, _ = fmt.Fprint(w, `<html><head><title>第42次CCF CSP认证报名通知</title></head><body><main><p>报名时间：2026年5月4日9:00至2026年5月24日23:59。</p><p>考试时间：2026年5月31日13:30。</p></main></body></html>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)

	document, err := collector.Fetch(context.Background(), "https://contest.example.com/cms/shell")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 requests (shell + redirect target), got %d", calls.Load())
	}
	if document.Title != "第42次CCF CSP认证报名通知" {
		t.Errorf("title = %q, want real body title", document.Title)
	}
	if !strings.Contains(document.Text, "报名时间") || !strings.Contains(document.Text, "2026年5月31日") {
		t.Errorf("did not extract real body text with dates: %#v", document)
	}
}

// TestJSRedirectDoesNotFollowWhenBodyPresent: a page with meaningful visible
// text must not be treated as a shell, even if it also contains a location.href
// assignment.
func TestJSRedirectDoesNotFollowWhenBodyPresent(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head><title>有正文的公告</title></head><body><main><p>这是一个内容完整的报名通知，报名时间：2026年5月4日9:00至2026年5月24日23:59。请广大考生按时报名参加认证考试。</p></main><script>window.location.href="/should-not-follow";</script></body></html>`)
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)

	document, err := collector.Fetch(context.Background(), "https://contest.example.com/article")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 request (body present, no follow), got %d", calls.Load())
	}
	if !strings.Contains(document.Text, "报名时间") {
		t.Errorf("expected the present body text to be kept: %#v", document)
	}
}

// TestJSRedirectDoesNotFollowCrossHost: a shell that redirects to a different
// host must be ignored (no open redirect).
func TestJSRedirectDoesNotFollowCrossHost(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head><title>shell</title></head><body><script>window.location.href="https://evil.example.com/phish";</script></body></html>`)
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)

	document, err := collector.Fetch(context.Background(), "https://contest.example.com/shell")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 request (cross-host not followed), got %d", calls.Load())
	}
	// The shell must not fetch the cross-host target; only its own title text
	// (which contains no phish content) may appear.
	if strings.Contains(document.Text, "phish") {
		t.Errorf("cross-host redirect was wrongly followed, got text %q", document.Text)
	}
}

// TestJSRedirectTargetConcatenatesStringLiterals verifies the literal
// concatenation used by CSPro CMS shells (window.location.href = "base" + "&x").
func TestJSRedirectTargetConcatenatesStringLiterals(t *testing.T) {
	shell := []byte(`<script>window.location.href="/cms/show.action?code=publish_a" + "&siteid=100000" + "&newsid=42";</script>`)
	target, ok := jsRedirectTarget(shell)
	if !ok {
		t.Fatal("jsRedirectTarget did not detect a redirect")
	}
	want := "/cms/show.action?code=publish_a&siteid=100000&newsid=42"
	if target != want {
		t.Fatalf("jsRedirectTarget = %q, want %q", target, want)
	}
}

// TestJSRedirectTargetRealCSProShell matches the real CSPro shell markup seen
// on cspro.org, where query params are accumulated into an `argument` variable
// before being concatenated into location.href (with a commented-out line).
func TestJSRedirectTargetRealCSProShell(t *testing.T) {
	shell := []byte(`<script>

	var argument = "";
			argument = argument +"&siteid=100000"
		argument = argument +"&newsid=99c860ff686b4abc8c1a7ee14a745b1d"
			//argument = argument +"&channelid=0000000103"
			argument = argument +"&channelid=0000000103"
	window.location.href="/cms/show.action?code=publish_8ac21fad9d27f22a019f7e5a821c0129" + argument;

</script>`)
	target, ok := jsRedirectTarget(shell)
	if !ok {
		t.Fatal("jsRedirectTarget did not detect a redirect")
	}
	want := "/cms/show.action?code=publish_8ac21fad9d27f22a019f7e5a821c0129&siteid=100000&newsid=99c860ff686b4abc8c1a7ee14a745b1d&channelid=0000000103"
	if target != want {
		t.Fatalf("jsRedirectTarget = %q\nwant %q", target, want)
	}
}

// TestJSRedirectFollowsAtMostOnce: even if the redirect target is itself
// another shell, only a single follow happens; it must not recurse.
func TestJSRedirectFollowsAtMostOnce(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/shell1":
			_, _ = fmt.Fprint(w, `<html><head><title>s1</title></head><body><script>window.location.href="/shell2";</script></body></html>`)
		case "/shell2":
			_, _ = fmt.Fprint(w, `<html><head><title>s2</title></head><body><script>window.location.href="/shell3";</script></body></html>`)
		default:
			_, _ = fmt.Fprint(w, `<html><head><title>s3</title></head><body>deep</body></html>`)
		}
	}))
	defer server.Close()
	collector := newTestFetchCollector(t, server.URL)

	document, err := collector.Fetch(context.Background(), "https://contest.example.com/shell1")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected exactly 2 requests (one follow, no recursion), got %d", calls.Load())
	}
	if document.Title != "s2" {
		t.Errorf("title = %q, want the single-follow target s2 (not deeper s3)", document.Title)
	}
}
