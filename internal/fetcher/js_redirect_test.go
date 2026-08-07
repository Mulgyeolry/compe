package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestJSRedirectTargetIgnoresUnrelatedVariables: only the accumulated variable
// actually referenced by the location.href right-hand side is concatenated; an
// unrelated self-accumulating variable must not leak into the URL.
func TestJSRedirectTargetIgnoresUnrelatedVariables(t *testing.T) {
	shell := []byte(`<script>
	var a = "";
	a = a + "&foo=1";
	var b = "";
	b = b + "&bar=2";
	window.location.href="/cms/show.action?code=abc" + a;
</script>`)
	target, ok := jsRedirectTarget(shell)
	if !ok {
		t.Fatal("jsRedirectTarget did not detect a redirect")
	}
	want := "/cms/show.action?code=abc&foo=1"
	if target != want {
		t.Fatalf("jsRedirectTarget = %q, want %q (unrelated variable must not be included)", target, want)
	}
}

// TestJSRedirectTargetIgnoresCommentedHref: a commented-out location.href must
// not be treated as the real redirect.
func TestJSRedirectTargetIgnoresCommentedHref(t *testing.T) {
	shell := []byte(`<script>
	// window.location.href="/cms/show.action?code=commented";
	window.location.href="/cms/show.action?code=real" + "&newsid=7";
</script>`)
	target, ok := jsRedirectTarget(shell)
	if !ok {
		t.Fatal("jsRedirectTarget did not detect the real redirect")
	}
	want := "/cms/show.action?code=real&newsid=7"
	if target != want {
		t.Fatalf("jsRedirectTarget = %q, want %q (commented href must be skipped)", target, want)
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

// trackingReadCloser counts Close calls on an http response body, bucketing by
// whether the request was the original shell or the JS-redirect target.
type trackingReadCloser struct {
	io.ReadCloser
	closed *int32
}

func (t *trackingReadCloser) Close() error {
	atomic.AddInt32(t.closed, 1)
	return t.ReadCloser.Close()
}

// trackingTransport forwards requests to a mock server while wrapping every
// response body so Close calls can be counted per bucket (shell vs target).
type trackingTransport struct {
	to           *url.URL
	closedShell  *int32
	closedTarget *int32
}

func (t *trackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := req.Clone(req.Context())
	next.URL.Scheme = t.to.Scheme
	next.URL.Host = t.to.Host
	resp, err := http.DefaultTransport.RoundTrip(next)
	if err != nil {
		return nil, err
	}
	// Preserve the original public hostname on the response (like routeTransport)
	// so the JS-redirect resolve/follow target stays public and the SSRF
	// pre-filter passes, while the request actually hits the test server.
	resp.Request = req
	counter := t.closedTarget
	if strings.Contains(req.URL.Path, "/shell") {
		counter = t.closedShell
	}
	resp.Body = &trackingReadCloser{ReadCloser: resp.Body, closed: counter}
	return resp, nil
}

// TestJSRedirectClosesSecondBody verifies that after a JS redirect follow, both
// the original shell response body and the redirected response body are closed
// (no leak of the second response body).
func TestJSRedirectClosesSecondBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/cms/shell":
			_, _ = fmt.Fprint(w, `<html><head><title>shell</title></head><body><script>window.location.href="/cms/show.action?code=abc" + "&newsid=7";</script></body></html>`)
		case "/cms/show.action":
			_, _ = fmt.Fprint(w, `<html><head><title>第42次CCF CSP认证报名通知</title></head><body><main><p>报名时间：2026年5月4日9:00至2026年5月24日23:59。</p></main></body></html>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	to, _ := url.Parse(server.URL)
	var closedShell, closedTarget int32
	collector := &HTTPCollector{
		client:     &http.Client{Transport: &trackingTransport{to: to, closedShell: &closedShell, closedTarget: &closedTarget}},
		maxBytes:   5 * 1024 * 1024,
		maxRetries: 0,
	}

	document, _, err := collector.fetchHTML(context.Background(), "https://contest.example.com/cms/shell")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(document.Text, "报名时间") {
		t.Fatalf("did not follow the JS redirect to real body: %#v", document)
	}
	// Each response body has independent, explicit ownership: the shell body is
	// closed at least once, and the redirected (target) body is closed exactly
	// once — never leaked and never double-closed.
	if got := atomic.LoadInt32(&closedShell); got < 1 {
		t.Errorf("shell body close calls = %d, want >= 1", got)
	}
	if got := atomic.LoadInt32(&closedTarget); got != 1 {
		t.Errorf("redirect target body close calls = %d, want exactly 1", got)
	}
}
