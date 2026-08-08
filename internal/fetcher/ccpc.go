package fetcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
)

// CCPC's official competition site (ccpc.io) is a UmiJS SPA: plain HTTP GET of
// /placard or /a/{id}.html returns an empty shell, while the real data is
// served by a public REST API (/api/archive). This file implements a narrow
// ccpc_api source kind that calls that API directly so CCPC competitions can be
// discovered and fetched without running the SPA.
//
// The apikey below is the public client key hard-coded in CCPC's umi bundle; it
// is not a secret. It is used only to build internal API URLs and must never
// appear in candidate/document URLs or logs.
const (
	ccpcAPIBase = "https://ccpc.io/api"
	ccpcAPIKey  = "84dfa45fd954ca8421904123b676c5e2"
	ccpcPublic  = "https://ccpc.io"
)

// ccpcArticlePath matches a CCPC public article URL: /a/{id}.html
var ccpcArticlePath = regexp.MustCompile(`^/a/(\d+)\.html$`)

// discoverCCPC fetches the official archive list API and turns each item into a
// candidate whose URL is the public article page (never the apikey-bearing API
// URL), respecting source.Limit.
func (c *HTTPCollector) discoverCCPC(ctx context.Context, source config.Source) ([]model.Candidate, error) {
	apiURL := ccpcAPIBase + "/archive?apikey=" + ccpcAPIKey + "&page=1&pageSize=200"
	resp, err := c.doRequest(ctx, apiURL, botHeaders())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.maxBytes {
		return nil, fmt.Errorf("ccpc archive response exceeds %d bytes", c.maxBytes)
	}
	var payload struct {
		Status int `json:"status"`
		Data   []struct {
			ID          int64  `json:"id"`
			Title       string `json:"title"`
			Publishtime int64  `json:"publishtime"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("ccpc archive: invalid JSON: %w", err)
	}
	if payload.Status != 1 {
		return nil, fmt.Errorf("ccpc archive api returned status %d", payload.Status)
	}
	items := make([]model.Candidate, 0, min(len(payload.Data), source.Limit))
	for _, it := range payload.Data {
		if len(items) >= source.Limit {
			break
		}
		title := strings.TrimSpace(it.Title)
		if it.ID <= 0 || title == "" {
			continue
		}
		items = append(items, model.Candidate{
			SourceID:   source.ID,
			SourceName: source.Name,
			Title:      title,
			URL:        fmt.Sprintf("%s/a/%d.html", ccpcPublic, it.ID),
			Snippet:    "",
		})
	}
	return items, nil
}

// fetchCCPCArticle fetches a single CCPC article via the archive detail API and
// converts the JSON content into a model.Document whose URL is the public
// article page.
func (c *HTTPCollector) fetchCCPCArticle(ctx context.Context, id int64, publicURL string) (model.Document, error) {
	apiURL := fmt.Sprintf("%s/archive/%d?apikey=%s", ccpcAPIBase, id, ccpcAPIKey)
	resp, err := c.doRequest(ctx, apiURL, botHeaders())
	if err != nil {
		return model.Document{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return model.Document{}, err
	}
	if int64(len(body)) > c.maxBytes {
		return model.Document{}, fmt.Errorf("ccpc article response exceeds %d bytes", c.maxBytes)
	}
	var payload struct {
		Status int `json:"status"`
		Data   struct {
			ArchivesInfo struct {
				Title       string `json:"title"`
				Content     string `json:"content"`
				Publishtime int64  `json:"publishtime"`
			} `json:"archivesInfo"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return model.Document{}, fmt.Errorf("ccpc article: invalid JSON: %w", err)
	}
	if payload.Status != 1 {
		return model.Document{}, fmt.Errorf("ccpc archive detail api returned status %d", payload.Status)
	}
	published := ""
	if payload.Data.ArchivesInfo.Publishtime > 0 {
		published = time.Unix(payload.Data.ArchivesInfo.Publishtime, 0).Format("2006-01-02 15:04:05")
	}
	text := normalizeSpace(ccpcStripHTML(payload.Data.ArchivesInfo.Content))
	return model.Document{
		Title:          strings.TrimSpace(payload.Data.ArchivesInfo.Title),
		URL:            publicURL,
		Text:           text,
		RawText:        text,
		PublishedAtRaw: published,
		IsListing:      false,
		ContentType:    "text/html",
	}, nil
}

// ccpcArticleID reports whether target is a CCPC public article URL and returns
// its id.
func ccpcArticleID(target string) (int64, bool) {
	u, err := url.Parse(target)
	if err != nil {
		return 0, false
	}
	if !strings.EqualFold(u.Host, "ccpc.io") {
		return 0, false
	}
	m := ccpcArticlePath.FindStringSubmatch(u.Path)
	if len(m) != 2 {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// ccpcStripHTML removes HTML markup from CCPC article content, preserving text
// and attachment titles, and converting block tags to whitespace so paragraphs
// do not run together.
func ccpcStripHTML(s string) string {
	// Convert block-level/line tags to spaces first so text does not merge.
	for _, tag := range []string{"<p", "</p", "<br", "<div", "</div", "<li", "</li", "<tr", "</tr", "<h1", "<h2", "<h3", "<h4", "</h1", "</h2", "</h3", "</h4"} {
		s = strings.ReplaceAll(s, tag, tag+" ")
	}
	re := regexp.MustCompile(`(?s)<[^>]+>`)
	return re.ReplaceAllString(s, " ")
}
