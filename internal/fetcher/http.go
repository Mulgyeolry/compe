package fetcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
	"github.com/PuerkitoBio/goquery"
	"github.com/mmcdole/gofeed"
)

type Collector interface {
	Discover(context.Context, config.Source) ([]model.Candidate, error)
	Fetch(context.Context, string) (model.Document, error)
}

// ErrAntiBot marks a response that was intercepted by an anti-bot challenge
// (CAPTCHA, JavaScript proof-of-work, IP block page) rather than a genuine
// content or server error. Callers treat it as "do not retry blindly and do
// not waste analysis on this page".
var ErrAntiBot = errors.New("request blocked by anti-bot protection")

// ErrUnsafeURL marks an outbound request rejected by the SSRF policy: the
// target host resolves to a loopback, private, link-local, metadata or
// multicast address, or the URL itself is not a safe public http(s) address.
// Callers and tests identify rejected requests with errors.Is(err, ErrUnsafeURL).
var ErrUnsafeURL = errors.New("unsafe outbound URL")

// unsafeNetworks lists the address families that must never be dialed by the
// public crawler. Everything here is either loopback, private, link-local,
// metadata, multicast or otherwise non-public.
var unsafeNetworks = []netip.Prefix{
	// IPv4
	netip.MustParsePrefix("0.0.0.0/8"),      // "this" network
	netip.MustParsePrefix("10.0.0.0/8"),     // private
	netip.MustParsePrefix("100.64.0.0/10"),  // carrier-grade NAT
	netip.MustParsePrefix("127.0.0.0/8"),    // loopback
	netip.MustParsePrefix("169.254.0.0/16"), // link-local / cloud metadata
	netip.MustParsePrefix("172.16.0.0/12"),  // private
	netip.MustParsePrefix("192.0.0.0/24"),   // IETF protocol assignments
	netip.MustParsePrefix("192.168.0.0/16"), // private
	netip.MustParsePrefix("198.18.0.0/15"),  // benchmarking
	netip.MustParsePrefix("224.0.0.0/4"),    // multicast
	netip.MustParsePrefix("240.0.0.0/4"),    // reserved
	// IPv6
	netip.MustParsePrefix("::/128"),    // unspecified
	netip.MustParsePrefix("::1/128"),   // loopback
	netip.MustParsePrefix("fc00::/7"),  // unique local (private)
	netip.MustParsePrefix("fe80::/10"), // link-local
	netip.MustParsePrefix("ff00::/8"),  // multicast
}

// isSafeIP reports whether a validated address is safe to dial. IPv4-mapped
// IPv6 addresses (e.g. ::ffff:127.0.0.1) are un-mapped first so they are judged
// by their real IPv4 semantics.
func isSafeIP(addr netip.Addr) bool {
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if addr.Zone() != "" {
		return false
	}
	if addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsMulticast() {
		return false
	}
	for _, network := range unsafeNetworks {
		if network.Contains(addr) {
			return false
		}
	}
	return true
}

// validatePublicURL performs lightweight syntactic checks on a URL that the
// public crawler may touch. It does not resolve DNS; the secure transport's
// DialContext does that at connection time. Scheme, hostname, userinfo and
// direct-IP safety are enforced here.
func validatePublicURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid URL %q", ErrUnsafeURL, raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrUnsafeURL, parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: empty host", ErrUnsafeURL)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: URL contains userinfo", ErrUnsafeURL)
	}
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return nil, fmt.Errorf("%w: localhost host %q", ErrUnsafeURL, host)
	}
	if port := parsed.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%w: invalid port %q", ErrUnsafeURL, port)
		}
	}
	// A literal IP host can be checked immediately without DNS.
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isSafeIP(addr) {
			return nil, fmt.Errorf("%w: private address %s", ErrUnsafeURL, addr)
		}
	}
	return parsed, nil
}

// isSafeCandidateURL is a light pre-filter for candidate URLs harvested from
// pages, RSS items and search results. It avoids DNS lookups (which could fire
// hundreds of times for one listing page); the secure DialContext performs the
// authoritative check at request time.
func isSafeCandidateURL(raw string) bool {
	// A domain host is only rejected here if it is literally localhost or an
	// unsafe literal IP; its resolved address is checked later by the secure
	// transport's DialContext.
	_, err := validatePublicURL(raw)
	return err == nil
}

// publicRoundTripper builds an http.RoundTripper for untrusted public targets.
// It clones the default transport so TLS, connection pooling and timeouts are
// preserved, then installs a DNS-validating DialContext and disables the proxy
// so a proxy cannot re-resolve the target and bypass the local IP check.
func publicRoundTripper(lookupIP func(context.Context, string) ([]net.IPAddr, error), dialContext func(context.Context, string, string) (net.Conn, error)) http.RoundTripper {
	if lookupIP == nil {
		lookupIP = func(ctx context.Context, host string) ([]net.IPAddr, error) {
			return net.DefaultResolver.LookupIPAddr(ctx, host)
		}
	}
	if dialContext == nil {
		dialContext = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot split address %q", ErrUnsafeURL, address)
		}
		ips, err := lookupIP(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("resolve %q: no IP addresses", host)
		}
		// Validate every resolved address. The result is rejected if any of
		// them is unsafe, so a DNS rebinding that mixes public and private
		// addresses cannot slip through.
		validated := make([]netip.Addr, 0, len(ips))
		for _, resolved := range ips {
			if resolved.Zone != "" {
				return nil, fmt.Errorf("%w: zoned address for %q", ErrUnsafeURL, host)
			}
			addr, ok := netip.AddrFromSlice(resolved.IP)
			if !ok {
				return nil, fmt.Errorf("resolve %q: invalid IP result", host)
			}
			addr = addr.Unmap()
			if !isSafeIP(addr) {
				return nil, fmt.Errorf("%w: private address %s for %q", ErrUnsafeURL, addr, host)
			}
			validated = append(validated, addr)
		}
		// Dial each validated IP directly so the transport never re-resolves
		// the hostname (which would defeat the IP check). TLS still verifies
		// the certificate against the original hostname because the request
		// URL is unchanged. Try each address in turn and fall back on failure.
		var lastErr error
		for _, addr := range validated {
			conn, err := dialContext(ctx, network, net.JoinHostPort(addr.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	return transport
}

// retryableStatus returns true when the HTTP status indicates a transient
// failure that may succeed on a later attempt. 4xx client errors (other than
// 408/429) are final and are not retried.
func retryableStatus(statusCode int) bool {
	switch {
	case statusCode == http.StatusRequestTimeout, statusCode == http.StatusTooManyRequests:
		return true
	case statusCode >= 500 && statusCode <= 599:
		return true
	default:
		return false
	}
}

// antiBotPatterns match the page bodies that known bot-protection layers
// return to a non-browser client. They are kept deliberately conservative:
// a genuine competition announcement rarely contains these exact markers.
var antiBotPatterns = []string{
	"just a moment",
	"cf-chl",
	"_cf_chl_opt",
	"请开启javascript",
	"请启用javascript",
	"enable javascript to continue",
	"验证码",
	"安全验证",
	"人机验证",
	"当前环境有风险",
	"unusual traffic",
}

// antiBotDetected reports whether a successful (2xx) response body is actually
// an anti-bot challenge page. Detection is performed on the raw bytes so it
// works before any HTML extraction. Markers are matched both verbatim and
// against a whitespace-stripped copy so that phrasing variations such as
// "请开启 JavaScript" and "请开启javascript" are both recognised.
func antiBotDetected(raw []byte) bool {
	lower := bytes.ToLower(raw)
	// A stripped copy is built lazily only when a verbatim match could
	// plausibly need it; the common case (plain announcements) is cheap.
	stripped := bytes.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		default:
			return r
		}
	}, lower)
	for _, marker := range antiBotPatterns {
		if bytes.Contains(lower, []byte(marker)) {
			return true
		}
		if stripped != nil && bytes.Contains(stripped, []byte(marker)) {
			return true
		}
	}
	return false
}

type HTTPCollector struct {
	client        *http.Client // public, SSRF-protected client for untrusted targets
	serviceClient *http.Client // trusted client used only for c.searxngURL
	searxngURL    string
	maxBytes      int64
	maxRetries    int
}

// publicCheckRedirect is the redirect policy for the public client. It limits
// redirect depth and re-validates every hop so a public page cannot redirect us
// to a private/metadata address.
func publicCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if _, err := validatePublicURL(req.URL.String()); err != nil {
		return err
	}
	return nil
}

// serviceCheckRedirect only limits redirect depth; the trusted SearxNG client
// must be able to follow redirects within the Docker network without the public
// address restrictions.
func serviceCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	return nil
}

func NewHTTPCollector(cfg config.Config) *HTTPCollector {
	timeout := time.Duration(cfg.Fetch.TimeoutSeconds) * time.Second
	return &HTTPCollector{
		client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: publicCheckRedirect,
			Transport:     publicRoundTripper(nil, nil),
		},
		serviceClient: &http.Client{
			Timeout:       timeout,
			CheckRedirect: serviceCheckRedirect,
		},
		searxngURL: strings.TrimRight(cfg.SearxngURL, "/"),
		maxBytes:   cfg.Fetch.MaxBytes,
		maxRetries: cfg.Fetch.MaxRetries,
	}
}

// doRequest executes a public, SSRF-protected request. The initial target is
// validated before any retry or dial, so an unsafe URL is rejected before any
// network I/O.
func (c *HTTPCollector) doRequest(ctx context.Context, target string, headers map[string]string) (*http.Response, error) {
	if _, err := validatePublicURL(target); err != nil {
		return nil, err
	}
	return c.doRequestWithClient(ctx, c.client, target, headers)
}

// doServiceRequest executes a request through the trusted client, used only to
// talk to the configured SearxNG instance inside the trusted network. It must
// NOT apply the public URL validation because SearxNG may live on a private
// in-network address (e.g. http://searxng:8080).
func (c *HTTPCollector) doServiceRequest(ctx context.Context, target string, headers map[string]string) (*http.Response, error) {
	return c.doRequestWithClient(ctx, c.serviceClient, target, headers)
}

// doRequestWithClient runs the shared retry loop over the given client. It
// retries transient network errors and retryable HTTP statuses, and surfaces
// ErrAntiBot when a 2xx response is actually an anti-bot challenge. A request
// rejected by the SSRF policy (ErrUnsafeURL) returns immediately without
// retrying, because a security rejection is not a transient network failure.
func (c *HTTPCollector) doRequestWithClient(ctx context.Context, client *http.Client, target string, headers map[string]string) (*http.Response, error) {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<(attempt-1)) * time.Second
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, err
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		resp, err := client.Do(req)
		if err != nil {
			if errors.Is(err, ErrUnsafeURL) {
				return nil, err
			}
			lastErr = err
			// Network errors are transient unless the context was cancelled.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode/100 == 2 {
			return resp, nil
		}
		if !retryableStatus(resp.StatusCode) || attempt == c.maxRetries {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			if antiBotDetected(body) {
				return nil, fmt.Errorf("%w (%s)", ErrAntiBot, resp.Status)
			}
			return nil, fmt.Errorf("request returned %s", resp.Status)
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 0))
		resp.Body.Close()
		lastErr = fmt.Errorf("request returned %s", resp.Status)
	}
	return nil, lastErr
}

// botHeaders are the request headers shared by every outgoing HTTP request.
// A descriptive User-Agent keeps the crawler honest while still being treated
// like a real client by naive UA filtering.
func botHeaders() map[string]string {
	return map[string]string{
		"User-Agent":      "competition-assistant/1.0 (+competition monitoring; daily)",
		"Accept":          "text/html,application/xhtml+xml,application/pdf,application/rss+xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
	}
}

func (c *HTTPCollector) Discover(ctx context.Context, source config.Source) ([]model.Candidate, error) {
	switch source.Kind {
	case "page":
		return c.discoverPage(ctx, source)
	case "rss":
		return c.discoverRSS(ctx, source)
	case "search":
		return c.discoverSearch(ctx, source)
	case "ccpc_api":
		return c.discoverCCPC(ctx, source)
	default:
		return nil, fmt.Errorf("unsupported source kind %q", source.Kind)
	}
}

func (c *HTTPCollector) discoverPage(ctx context.Context, source config.Source) ([]model.Candidate, error) {
	doc, raw, err := c.fetchHTML(ctx, source.URL)
	if err != nil {
		return nil, err
	}
	items := make([]model.Candidate, 0, source.Limit)
	if !doc.IsListing {
		items = append(items, model.Candidate{SourceID: source.ID, SourceName: source.Name, Title: doc.Title, URL: doc.URL, Snippet: truncate(doc.Text, 500)})
	}
	links := make([]model.Candidate, 0, min(500, source.Limit*5))
	base, _ := url.Parse(doc.URL)
	raw.Find("a[href]").EachWithBreak(func(_ int, selection *goquery.Selection) bool {
		if len(links) >= 500 {
			return false
		}
		href, _ := selection.Attr("href")
		parsed, err := url.Parse(strings.TrimSpace(href))
		if err != nil || href == "" || strings.HasPrefix(href, "javascript:") || strings.HasPrefix(href, "mailto:") {
			return true
		}
		absolute := base.ResolveReference(parsed)
		title := normalizeSpace(selection.Text())
		if title == "" || len([]rune(title)) < 4 {
			return true
		}
		if !isSafeCandidateURL(absolute.String()) {
			return true
		}
		links = append(links, model.Candidate{SourceID: source.ID, SourceName: source.Name, Title: title, URL: canonicalURL(absolute.String()), Snippet: title})
		return true
	})
	sort.SliceStable(links, func(i, j int) bool {
		return candidateLinkPriority(links[i].Title) > candidateLinkPriority(links[j].Title)
	})
	for _, candidate := range links {
		if len(items) >= source.Limit {
			break
		}
		items = append(items, candidate)
	}
	return deduplicate(items), nil
}

func candidateLinkPriority(title string) int {
	priority := 0
	for _, marker := range []string{"比赛", "大赛", "竞赛", "挑战赛", "程序设计", "报名", "预告", "赛题", "开赛", "启动"} {
		if strings.Contains(title, marker) {
			priority += 10
		}
	}
	for _, marker := range []string{"首页", "关于我们", "联系我们", "登录", "注册", "隐私", "招聘"} {
		if strings.Contains(title, marker) {
			priority -= 20
		}
	}
	return priority
}

func (c *HTTPCollector) discoverRSS(ctx context.Context, source config.Source) ([]model.Candidate, error) {
	resp, err := c.doRequest(ctx, source.URL, botHeaders())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	feed, err := gofeed.NewParser().Parse(io.LimitReader(resp.Body, c.maxBytes))
	if err != nil {
		return nil, err
	}
	items := make([]model.Candidate, 0, min(len(feed.Items), source.Limit))
	for _, item := range feed.Items {
		if len(items) >= source.Limit {
			break
		}
		if !isSafeCandidateURL(item.Link) {
			continue
		}
		items = append(items, model.Candidate{SourceID: source.ID, SourceName: source.Name, Title: item.Title, URL: canonicalURL(item.Link), Snippet: normalizeSpace(item.Description)})
	}
	return deduplicate(items), nil
}

func (c *HTTPCollector) discoverSearch(ctx context.Context, source config.Source) ([]model.Candidate, error) {
	query := strings.ReplaceAll(source.Query, "{year}", strconv.Itoa(time.Now().Year()))
	results, err := c.searchSearx(ctx, query, source.Limit, source.AllowedDomains)
	if err != nil {
		return nil, err
	}
	items := make([]model.Candidate, 0, len(results))
	for _, result := range results {
		items = append(items, model.Candidate{
			SourceID:   source.ID,
			SourceName: source.Name,
			Title:      result.Title,
			URL:        result.URL,
			Snippet:    result.Snippet,
		})
	}
	return items, nil
}

// searchSearx is the shared, bounded SearXNG search core used by both
// discoverSearch (configured sources) and the research Search tool. It builds
// the endpoint from the configured c.searxngURL, talks to SearXNG only through
// the trusted service client (which may address a private in-network host), and
// returns filtered, canonicalized, deduplicated discovery clues. It never fetches
// the result URLs itself.
//
// The caller controls the result count via limit; this core applies no 20-result
// cap, so the configured-source path (source.Limit may exceed 20) is unaffected
// by the research API's stricter bound. Output fields are bounded here so no
// unbounded text can enter a future Agent's context.
func (c *HTTPCollector) searchSearx(ctx context.Context, query string, limit int, allowedDomains []string) ([]ResearchSearchResult, error) {
	endpoint := c.searxngURL + "/search?format=json&q=" + url.QueryEscape(query)
	// SearxNG is a trusted in-network service; only this request may use the
	// service client. The result URLs it returns are untrusted and are fetched
	// later through the public client, never here.
	resp, err := c.doServiceRequest(ctx, endpoint, botHeaders())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, c.maxBytes)).Decode(&payload); err != nil {
		return nil, err
	}
	results := make([]ResearchSearchResult, 0, min(len(payload.Results), limit))
	seen := make(map[string]bool, len(payload.Results))
	for _, result := range payload.Results {
		if len(results) >= limit {
			break
		}
		if !allowedURL(result.URL, allowedDomains) || !isSafeCandidateURL(result.URL) {
			continue
		}
		canonical := canonicalURL(result.URL)
		// The canonical URL is produced from untrusted input (e.g. a wrapper like
		// mp.weixin.qq.com whose target_url points somewhere else). Re-validate it
		// in full: it must still be an allowed, safe public http(s) URL. This
		// prevents a raw wrapper that passes the first check from unwrapping to a
		// loopback/localhost/private destination or a non-allowed domain. Dedup
		// remains keyed on the canonical URL.
		if canonical == "" ||
			!allowedURL(canonical, allowedDomains) ||
			!isSafeCandidateURL(canonical) ||
			seen[canonical] {
			continue
		}
		seen[canonical] = true
		results = append(results, ResearchSearchResult{
			Title:   truncate(normalizeSpace(result.Title), maxResearchSearchTitleRunes),
			URL:     canonical,
			Snippet: truncate(normalizeSpace(result.Content), maxResearchSearchSnippetRunes),
		})
	}
	return results, nil
}

func (c *HTTPCollector) Fetch(ctx context.Context, target string) (model.Document, error) {
	// CCPC article URLs (/a/{id}.html) are served by the public archive API
	// instead of the SPA shell.
	if id, ok := ccpcArticleID(target); ok {
		return c.fetchCCPCArticle(ctx, id, target)
	}
	doc, _, err := c.fetchHTML(ctx, target)
	return doc, err
}

// jsHrefAssign matches an assignment to (window.)location.href.
var jsHrefAssign = regexp.MustCompile(`(?i)(?:window\.)?location\.href\s*=\s*`)

// jsStringLit matches a single- or double-quoted JS string literal.
var jsStringLit = regexp.MustCompile(`"([^"]*)"|'([^']*)'`)

// jsAccumPattern matches a self-accumulating string assignment like
// `argument = argument + "&siteid=100000"` or `argument += "&newsid=..."`, which
// is how some CMS shells (e.g. CSPro) build the query string before assigning
// it to location.href. Go's regexp has no backreferences, so the variable name
// is captured twice and compared in code. Only the string literal is extracted;
// no JS is executed.
var jsAccumPattern = regexp.MustCompile(`(?m)(\w+)\s*(?:\+=|=)\s*(\w+)\s*\+\s*"([^"]*)"`)

// jsLineCommentPattern matches a // line comment so commented-out assignments
// are not double-counted.
var jsLineCommentPattern = regexp.MustCompile(`(?m)//[^\r\n]*`)

// jsRedirectTarget extracts the literal URL assigned to location.href. It only
// parses simple string literals (concatenated with "+", and also the common
// `argument = argument + "..."` accumulation used by CSPro CMS); it never
// executes JavaScript. It returns the raw URL as written.
// jsHrefRHSVar matches a variable appended after the leading literal on the
// right-hand side, e.g. `location.href = "base" + argument`.
var jsHrefRHSVar = regexp.MustCompile(`\+\s*(\w+)`)

func jsRedirectTarget(body []byte) (string, bool) {
	text := string(body)
	searchFrom := 0
	for {
		loc := jsHrefAssign.FindStringIndex(text[searchFrom:])
		if loc == nil {
			return "", false
		}
		absStart := searchFrom + loc[0]
		if isLineCommented(text, absStart) {
			// A commented-out location.href is not a real redirect.
			searchFrom = absStart + (loc[1] - loc[0])
			continue
		}
		rest := text[searchFrom+loc[1]:]
		base, varName := jsHrefBaseAndVar(rest)
		if base == "" {
			return "", false
		}
		// Only concatenate literals accumulated into the variable that the
		// location.href right-hand side actually references.
		before := text[:absStart]
		accumulated := jsAccumulatedLiterals(before, varName)
		return base + accumulated, true
	}
}

// isLineCommented reports whether the byte at idx in s lies inside a // line
// comment.
func isLineCommented(s string, idx int) bool {
	lineStart := strings.LastIndex(s[:idx], "\n") + 1
	return strings.Contains(s[lineStart:idx], "//")
}

// jsHrefBaseAndVar extracts the leading string literal assigned to
// location.href and, if the right-hand side then concatenates a variable
// (`location.href = "base" + argument`), the name of that variable.
func jsHrefBaseAndVar(rest string) (string, string) {
	var builder strings.Builder
	pos := 0
	for pos < len(rest) {
		m := jsStringLit.FindStringSubmatchIndex(rest[pos:])
		if m == nil {
			break
		}
		// The text between the previous literal and this one must be only
		// whitespace/`+`; otherwise the literal run ended (e.g. at `;`).
		if strings.TrimSpace(strings.ReplaceAll(rest[pos:pos+m[0]], "+", "")) != "" {
			break
		}
		var val string
		if m[2] >= 0 {
			val = rest[pos+m[2] : pos+m[3]]
		} else {
			val = rest[pos+m[4] : pos+m[5]]
		}
		builder.WriteString(val)
		pos += m[1]
	}
	base := builder.String()
	if m := jsHrefRHSVar.FindStringSubmatch(rest[pos:]); len(m) == 2 {
		return base, m[1]
	}
	return base, ""
}

// jsAccumulatedLiterals concatenates the string literals from self-accumulating
// assignments of the referenced variable (X = X + "lit" / X += "lit"), in
// order, ignoring line comments and unrelated variables.
func jsAccumulatedLiterals(s, varName string) string {
	if varName == "" {
		return ""
	}
	clean := jsLineCommentPattern.ReplaceAllString(s, "")
	var builder strings.Builder
	for _, m := range jsAccumPattern.FindAllStringSubmatch(clean, -1) {
		if m[1] == varName && m[2] == varName {
			builder.WriteString(m[3])
		}
	}
	return builder.String()
}

// htmlShellEmpty reports whether an HTML body has no meaningful visible text
// (after removing scripts/styles), i.e. it looks like an empty shell that may
// only contain a JS redirect.
func htmlShellEmpty(body []byte) bool {
	document, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return false
	}
	document.Find("script,style,noscript,svg").Remove()
	text := normalizeSpace(document.Find("body").Text())
	return len(text) < 50
}

// resolveSameHostJSRedirect resolves a raw JS redirect URL against base and
// requires the result to stay on the same host, to prevent an open redirect.
func resolveSameHostJSRedirect(base, raw string) (string, error) {
	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	resolved := baseURL.ResolveReference(ref)
	if resolved.Scheme != "http" && resolved.Scheme != "https" {
		return "", fmt.Errorf("unsupported JS redirect scheme %q", resolved.Scheme)
	}
	if resolved.Host != baseURL.Host {
		return "", fmt.Errorf("cross-host JS redirect to %q", resolved.Host)
	}
	return resolved.String(), nil
}

func (c *HTTPCollector) fetchHTML(ctx context.Context, target string) (model.Document, *goquery.Document, error) {
	resp, err := c.doRequest(ctx, target, botHeaders())
	if err != nil {
		return model.Document{}, nil, err
	}
	// Explicit ownership: the original (shell) response body is closed exactly
	// once by this deferred closure, regardless of whether a JS redirect follow
	// happens or any early return occurs.
	shellBody := resp.Body
	defer func() { _ = shellBody.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return model.Document{}, nil, err
	}
	if int64(len(body)) > c.maxBytes {
		return model.Document{}, nil, fmt.Errorf("response exceeds %d bytes", c.maxBytes)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	finalURL := canonicalURL(resp.Request.URL.String())

	// Follow a single simple JS redirect when the page is an empty/near-empty
	// HTML shell. Some CMS (e.g. CSPro) serve a shell whose only meaningful
	// content is `window.location.href = "..."`; the real body is at that
	// literal URL. We never execute JS: we only resolve the literal, require it
	// to stay on the same host, and fetch it through the SSRF-protected
	// doRequest. A shell is followed at most once (no recursive redirects).
	if htmlShellEmpty(body) {
		if rawTarget, ok := jsRedirectTarget(body); ok {
			if resolved, err := resolveSameHostJSRedirect(finalURL, rawTarget); err == nil {
				resp2, err2 := c.doRequest(ctx, resolved, botHeaders())
				if err2 == nil {
					// resp2 has its own independent ownership: its body is closed
					// exactly once by this deferred closure on every path (early
					// returns included). The original shell body stays owned by
					// shellBody's closure above, so nothing is double-closed or
					// leaked, and we never rely on reassigning resp.
					targetBody := resp2.Body
					defer func() { _ = targetBody.Close() }()
					body, err = io.ReadAll(io.LimitReader(resp2.Body, c.maxBytes+1))
					if err != nil {
						return model.Document{}, nil, err
					}
					if int64(len(body)) > c.maxBytes {
						return model.Document{}, nil, fmt.Errorf("response exceeds %d bytes", c.maxBytes)
					}
					resp = resp2
					contentType = strings.ToLower(resp2.Header.Get("Content-Type"))
					finalURL = canonicalURL(resp2.Request.URL.String())
				}
			}
		}
	}

	if strings.Contains(contentType, "pdf") || strings.HasSuffix(strings.ToLower(resp.Request.URL.Path), ".pdf") {
		text, err := extractPDF(ctx, body)
		if err != nil {
			return model.Document{}, nil, err
		}
		normalized := normalizeSpace(text)
		return model.Document{Title: pathTitle(resp.Request.URL.Path), URL: finalURL, Text: normalized, RawText: strings.TrimSpace(text), ContentType: "application/pdf", Segments: buildPDFSegments(text)}, goquery.NewDocumentFromNode(nil), nil
	}
	parsed, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return model.Document{}, nil, err
	}
	publishedAt := extractPublishedAt(parsed)
	parsed.Find("script,style,noscript,svg,nav,footer,aside").Remove()
	title := normalizeSpace(parsed.Find("title").First().Text())
	rawText := normalizeSpace(parsed.Find("body").Text())
	// Anti-bot detection runs on the visible page text, never on the raw
	// HTML, so resource URLs or script names that merely mention "验证码" or
	// "安全验证" do not cause false positives on legitimate pages.
	if antiBotDetected([]byte(rawText)) {
		return model.Document{}, nil, fmt.Errorf("%w (%s)", ErrAntiBot, canonicalURL(target))
	}
	mainText, segments, listing := extractMainContent(parsed)
	if mainText == "" {
		mainText = rawText
	}
	if title != "" && !strings.Contains(mainText, title) {
		mainText = title + " " + mainText
	}
	return model.Document{
		Title:          title,
		URL:            finalURL,
		Text:           mainText,
		RawText:        rawText,
		PublishedAtRaw: publishedAt,
		IsListing:      listing,
		ContentType:    contentType,
		Segments:       segments,
	}, parsed, nil
}

var publishedDatePattern = regexp.MustCompile(`(?:19|20)\d{2}[-/.年]\s*\d{1,2}[-/.月]\s*\d{1,2}(?:日)?(?:\s+\d{1,2}:\d{2}(?::\d{2})?)?`)

func extractPublishedAt(document *goquery.Document) string {
	metaKeys := []string{"article:published_time", "publishdate", "pubdate", "date", "dc.date", "weibo:article:create_at"}
	for _, key := range metaKeys {
		selector := fmt.Sprintf(`meta[property="%s"],meta[name="%s"]`, key, key)
		if value, exists := document.Find(selector).First().Attr("content"); exists {
			if match := publishedDatePattern.FindString(normalizeSpace(value)); match != "" {
				return match
			}
		}
	}
	selectors := []string{"time[datetime]", "time", ".publish-time", ".pubtime", ".article-time", ".news-time", ".date", ".time", ".info"}
	for _, selector := range selectors {
		selection := document.Find(selector).First()
		value, _ := selection.Attr("datetime")
		if value == "" {
			value = selection.Text()
		}
		if match := publishedDatePattern.FindString(normalizeSpace(value)); match != "" {
			return match
		}
	}
	return ""
}

func extractMainContent(document *goquery.Document) (string, []model.DocumentSegment, bool) {
	type contentCandidate struct {
		text       string
		score      int
		links      int
		paragraphs int
		linkRunes  int
		selection  *goquery.Selection
	}
	selectors := []string{
		"article", "main", "[role=main]", ".article-content", ".article_content", ".news-content",
		".detail-content", ".detail", ".TRS_Editor", ".v_news_content", "#content", ".content",
	}
	best := contentCandidate{}
	for selectorIndex, selector := range selectors {
		document.Find(selector).Each(func(_ int, selection *goquery.Selection) {
			text := normalizeSpace(selection.Text())
			length := len([]rune(text))
			if length < 40 {
				return
			}
			paragraphs := selection.Find("p").Length()
			links, linkRunes := 0, 0
			selection.Find("a[href]").Each(func(_ int, link *goquery.Selection) {
				links++
				linkRunes += len([]rune(normalizeSpace(link.Text())))
			})
			priorityBonus := max(0, 700-selectorIndex*50)
			score := length + paragraphs*80 + priorityBonus - links*35 - linkRunes
			if score > best.score {
				best = contentCandidate{text: text, score: score, links: links, paragraphs: paragraphs, linkRunes: linkRunes, selection: selection}
			}
		})
	}
	if best.text == "" {
		body := document.Find("body").First()
		best.text = normalizeSpace(body.Text())
		best.selection = body
		best.paragraphs = body.Find("p").Length()
		body.Find("a[href]").Each(func(_ int, link *goquery.Selection) {
			best.links++
			best.linkRunes += len([]rune(normalizeSpace(link.Text())))
		})
	}
	textRunes := len([]rune(best.text))
	linkRatio := 0.0
	if textRunes > 0 {
		linkRatio = float64(best.linkRunes) / float64(textRunes)
	}
	isListing := (best.links >= 12 && linkRatio >= 0.30) || (best.links >= 20 && best.paragraphs < 5)
	repeatedArticles := document.Find("article").Length()
	linkedListItems := document.Find("li a[href]").Length()
	if repeatedArticles >= 4 || (linkedListItems >= 15 && best.links >= 8) {
		isListing = true
	}
	return best.text, buildHTMLSegments(best.selection, best.text), isListing
}

func buildHTMLSegments(selection *goquery.Selection, fallback string) []model.DocumentSegment {
	if selection == nil {
		return chunkSegments("html", 0, fallback, 2800)
	}
	var blocks []string
	selection.Find("h1,h2,h3,h4,p,li,tr,pre").Each(func(_ int, block *goquery.Selection) {
		text := normalizeSpace(block.Text())
		if len([]rune(text)) >= 8 {
			blocks = append(blocks, text)
		}
	})
	if len(blocks) == 0 {
		return chunkSegments("html", 0, fallback, 2800)
	}
	return chunkSegments("html", 0, strings.Join(blocks, "\n"), 2800)
}

func buildPDFSegments(text string) []model.DocumentSegment {
	pages := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\f")
	var result []model.DocumentSegment
	for index, page := range pages {
		result = append(result, chunkSegments("pdf", index+1, normalizeSpace(page), 2800)...)
	}
	return result
}

func chunkSegments(kind string, page int, text string, maxRunes int) []model.DocumentSegment {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	paragraphs := strings.Split(strings.ReplaceAll(text, "。", "。\n"), "\n")
	var chunks []string
	var current strings.Builder
	flush := func() {
		value := normalizeSpace(current.String())
		if value != "" {
			chunks = append(chunks, value)
		}
		current.Reset()
	}
	for _, paragraph := range paragraphs {
		paragraph = normalizeSpace(paragraph)
		if paragraph == "" {
			continue
		}
		if current.Len() > 0 && len([]rune(current.String()))+len([]rune(paragraph)) > maxRunes {
			flush()
		}
		if len([]rune(paragraph)) > maxRunes {
			runes := []rune(paragraph)
			for len(runes) > 0 {
				size := min(maxRunes, len(runes))
				chunks = append(chunks, string(runes[:size]))
				runes = runes[size:]
			}
			continue
		}
		if current.Len() > 0 {
			current.WriteString(" ")
		}
		current.WriteString(paragraph)
	}
	flush()
	result := make([]model.DocumentSegment, 0, len(chunks))
	for index, chunk := range chunks {
		id := fmt.Sprintf("%s-%d", kind, index+1)
		if page > 0 {
			id = fmt.Sprintf("%s-p%d-%d", kind, page, index+1)
		}
		result = append(result, model.DocumentSegment{ID: id, Kind: kind, Page: page, Text: chunk})
	}
	return result
}

func extractPDF(ctx context.Context, data []byte) (string, error) {
	cmd := exec.CommandContext(ctx, "pdftotext", "-", "-")
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func allowedURL(raw string, domains []string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	if parsed.Hostname() == "" {
		return false
	}
	if len(domains) == 0 {
		return true
	}
	host := strings.ToLower(parsed.Hostname())
	for _, domain := range domains {
		domain = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func canonicalURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	// WeChat may return an anti-abuse interstitial while preserving the real
	// official article in target_url. Store and show the destination instead of
	// the unusable captcha address.
	if strings.EqualFold(parsed.Hostname(), "mp.weixin.qq.com") && strings.Contains(parsed.Path, "wappoc_appmsgcaptcha") {
		if target := parsed.Query().Get("target_url"); target != "" {
			if destination, targetErr := url.Parse(target); targetErr == nil && (destination.Scheme == "http" || destination.Scheme == "https") {
				return canonicalURL(target)
			}
		}
	}
	parsed.Fragment = ""
	query := parsed.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") || lower == "spm" || lower == "from" || lower == "source" {
			query.Del(key)
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func normalizeSpace(value string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(value, "\u00a0", " ")), " ")
}

func truncate(value string, size int) string {
	runes := []rune(value)
	if len(runes) <= size {
		return value
	}
	return string(runes[:size])
}

func pathTitle(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return "PDF 公告"
	}
	return parts[len(parts)-1]
}

func deduplicate(items []model.Candidate) []model.Candidate {
	seen := map[string]bool{}
	result := make([]model.Candidate, 0, len(items))
	for _, item := range items {
		if item.URL == "" || seen[item.URL] {
			continue
		}
		seen[item.URL] = true
		result = append(result, item)
	}
	return result
}
