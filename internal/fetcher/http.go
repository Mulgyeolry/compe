package fetcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	client     *http.Client
	searxngURL string
	maxBytes   int64
	maxRetries int
}

func NewHTTPCollector(cfg config.Config) *HTTPCollector {
	return &HTTPCollector{
		client: &http.Client{
			Timeout: time.Duration(cfg.Fetch.TimeoutSeconds) * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("too many redirects")
				}
				return nil
			},
		},
		searxngURL: strings.TrimRight(cfg.SearxngURL, "/"),
		maxBytes:   cfg.Fetch.MaxBytes,
		maxRetries: cfg.Fetch.MaxRetries,
	}
}

// doRequest executes the request with exponential-backoff retries. It retries
// transient network errors and retryable HTTP statuses, and surfaces ErrAntiBot
// when a 2xx response is actually an anti-bot challenge. The request body is
// nil for every caller, so a clone per attempt is unnecessary; callers must
// construct a fresh request each round if they need retry support.
func (c *HTTPCollector) doRequest(ctx context.Context, target string, headers map[string]string) (*http.Response, error) {
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
		resp, err := c.client.Do(req)
		if err != nil {
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
		items = append(items, model.Candidate{SourceID: source.ID, SourceName: source.Name, Title: item.Title, URL: canonicalURL(item.Link), Snippet: normalizeSpace(item.Description)})
	}
	return deduplicate(items), nil
}

func (c *HTTPCollector) discoverSearch(ctx context.Context, source config.Source) ([]model.Candidate, error) {
	query := strings.ReplaceAll(source.Query, "{year}", strconv.Itoa(time.Now().Year()))
	endpoint := c.searxngURL + "/search?format=json&q=" + url.QueryEscape(query)
	resp, err := c.doRequest(ctx, endpoint, botHeaders())
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
	items := make([]model.Candidate, 0, min(len(payload.Results), source.Limit))
	for _, result := range payload.Results {
		if len(items) >= source.Limit || !allowedURL(result.URL, source.AllowedDomains) {
			continue
		}
		items = append(items, model.Candidate{SourceID: source.ID, SourceName: source.Name, Title: result.Title, URL: canonicalURL(result.URL), Snippet: normalizeSpace(result.Content)})
	}
	return deduplicate(items), nil
}

func (c *HTTPCollector) Fetch(ctx context.Context, target string) (model.Document, error) {
	doc, _, err := c.fetchHTML(ctx, target)
	return doc, err
}

func (c *HTTPCollector) fetchHTML(ctx context.Context, target string) (model.Document, *goquery.Document, error) {
	resp, err := c.doRequest(ctx, target, botHeaders())
	if err != nil {
		return model.Document{}, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return model.Document{}, nil, err
	}
	if int64(len(body)) > c.maxBytes {
		return model.Document{}, nil, fmt.Errorf("response exceeds %d bytes", c.maxBytes)
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	finalURL := canonicalURL(resp.Request.URL.String())
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
	if len(domains) == 0 {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
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
