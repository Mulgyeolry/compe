package service

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
	"competition-assistant/internal/store"
)

// ccpcE2ECollector is a fake Collector that serves a fixed CCPC archive and
// article set. It never touches the network. It mirrors the output of the real
// ccpc_api adapter (discoverCCPC -> /a/{id}.html candidates, fetchCCPCArticle ->
// model.Document) so the identity/merge path is exercised end to end.
type ccpcE2ECollector struct {
	articles map[string]model.Document // key: public /a/{id}.html URL
}

func (c *ccpcE2ECollector) Discover(_ context.Context, _ config.Source) ([]model.Candidate, error) {
	// Return candidates in publish order (newest first), mirroring the archive API.
	order := []string{
		"https://ccpc.io/a/6.html", // 第11届 郑州站正式报名
		"https://ccpc.io/a/5.html", // 第11届 郑州站报名预告
		"https://ccpc.io/a/4.html", // 第十一届 中文全称总决赛
		"https://ccpc.io/a/3.html", // 第10届 总决赛
		"https://ccpc.io/a/2.html", // 第11届 济南站
		"https://ccpc.io/a/1.html", // 第11届 重庆站
	}
	items := make([]model.Candidate, 0, len(order))
	for _, u := range order {
		doc := c.articles[u]
		items = append(items, model.Candidate{SourceID: "ccpc", SourceName: "CCPC 公告", Title: doc.Title, URL: u, Snippet: ""})
	}
	return items, nil
}

func (c *ccpcE2ECollector) Fetch(_ context.Context, raw string) (model.Document, error) {
	if d, ok := c.articles[raw]; ok {
		return d, nil
	}
	return model.Document{}, errors.New("ccpc e2e: unknown article url " + raw)
}

// newCCPCE2EService builds a Service backed by a real store and the mock CCPC
// collector, with a rules-only analyzer (no LLM) so the identity merge is fully
// deterministic. The DB path is returned so idempotency can be checked through a
// fresh read connection.
func newCCPCE2EService(t *testing.T) (*Service, *store.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ccpc.db")
	cfg := config.Config{
		Schedule: "0 8 * * *", Timezone: "Asia/Shanghai", Location: researchLocation(),
		DBPath: dbPath,
		Fetch:  config.Fetch{TimeoutSeconds: 3, MaxBytes: 1024 * 1024, MaxCandidates: 20},
		Keywords: config.Keywords{
			Focus:    []string{"ccpc", "程序设计竞赛", "程序设计大赛"},
			Positive: []string{"报名", "通知", "预告", "总决赛", "分站赛", "站"},
		},
		Sources: []config.Source{{ID: "ccpc", Name: "CCPC 公告", Kind: "ccpc_api", URL: "https://ccpc.io/", Trust: "high"}},
	}
	collector := &ccpcE2ECollector{articles: ccpcE2EArticles()}
	database, err := store.Open(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	app := New(cfg, database, collector, analyzer.New(cfg), researchNoopSender{}, logger)
	app.SetNow(researchNow)
	return app, database, dbPath
}

// ccpcE2EArticles returns the fixed CCPC article set used by the end-to-end test.
func ccpcE2EArticles() map[string]model.Document {
	now := time.Now().Unix()
	return map[string]model.Document{
		"https://ccpc.io/a/1.html": {
			URL: "https://ccpc.io/a/1.html", Title: "第11届中国大学生程序设计竞赛（CCPC）重庆站",
			Text:           "第11届中国大学生程序设计竞赛（CCPC）重庆站于2026年4月25日举办，报名截止2026年4月1日，承办学校为重庆大学。",
			PublishedAtRaw: time.Unix(now, 0).Format("2006-01-02 15:04:05"), IsListing: false,
		},
		"https://ccpc.io/a/2.html": {
			URL: "https://ccpc.io/a/2.html", Title: "第11届中国大学生程序设计竞赛（CCPC）济南站",
			Text:           "第11届中国大学生程序设计竞赛（CCPC）济南站于2026年4月25日举办，报名截止2026年4月1日，承办学校为山东大学。",
			PublishedAtRaw: time.Unix(now, 0).Format("2006-01-02 15:04:05"), IsListing: false,
		},
		"https://ccpc.io/a/3.html": {
			URL: "https://ccpc.io/a/3.html", Title: "第10届中国大学生程序设计竞赛（CCPC）总决赛",
			Text:           "第10届中国大学生程序设计竞赛（CCPC）总决赛于2025年举办。",
			PublishedAtRaw: time.Unix(now, 0).Format("2006-01-02 15:04:05"), IsListing: false,
		},
		"https://ccpc.io/a/4.html": {
			URL: "https://ccpc.io/a/4.html", Title: "第十一届中国大学生程序设计竞赛总决赛",
			Text:           "第十一届中国大学生程序设计竞赛总决赛（第11届）将于2026年举办，报名截止2026年4月19日。",
			PublishedAtRaw: time.Unix(now, 0).Format("2006-01-02 15:04:05"), IsListing: false,
		},
		"https://ccpc.io/a/5.html": {
			URL: "https://ccpc.io/a/5.html", Title: "第11届CCPC郑州站报名预告",
			Text:           "第11届CCPC郑州站报名即将启动，敬请期待。",
			PublishedAtRaw: time.Unix(now, 0).Format("2006-01-02 15:04:05"), IsListing: false,
		},
		"https://ccpc.io/a/6.html": {
			URL: "https://ccpc.io/a/6.html", Title: "第11届CCPC郑州站正式报名通知",
			Text:           "第11届CCPC郑州站正式报名将于2026年4月1日开始，报名截止2026年4月19日，报名费200元。",
			PublishedAtRaw: time.Unix(now, 0).Format("2006-01-02 15:04:05"), IsListing: false,
		},
	}
}

// TestCCPCCanonicalEndToEnd drives the full pipeline twice (Discover -> Fetch ->
// Analyze -> Upsert -> rescan) against a mock CCPC archive and asserts that the
// canonical identity boundaries hold and that the scan is idempotent.
func TestCCPCCanonicalEndToEnd(t *testing.T) {
	app, database, dbPath := newCCPCE2EService(t)
	ctx := context.Background()

	// First scan.
	if err := app.run(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	// Expected entities:
	//   1. 第11届 重庆站
	//   2. 第11届 济南站
	//   3. 第10届 总决赛
	//   4. 第11届 总决赛 (中文全称 a/4, no separate ccpc-缩写 article in the set)
	//   5. 第11届 郑州站 (预告 a/5 + 正式报名 a/6 merge)
	//
	// Expected distinct rows = 5.
	comps, err := database.ListCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(comps) != 5 {
		t.Fatalf("expected 5 canonical entities after first scan, got %d", len(comps))
	}

	// 重庆站 and 济南站 must be distinct.
	ids := map[string]int64{}
	for _, c := range comps {
		ids[c.Name] = c.ID
	}
	if ids["第11届中国大学生程序设计竞赛（CCPC）重庆站"] == ids["第11届中国大学生程序设计竞赛（CCPC）济南站"] {
		t.Fatal("重庆站 and 济南站 must be separate entities")
	}
	// 中文全称 总决赛 must NOT merge with 第10届 总决赛.
	if ids["第十一届中国大学生程序设计竞赛总决赛"] == ids["第10届中国大学生程序设计竞赛（CCPC）总决赛"] {
		t.Fatal("中文全称 总决赛 must not merge into 第10届 总决赛")
	}
	// 郑州站 预告 + 正式报名 merge into ONE 郑州站 entity.
	zhengzhouCount := 0
	for _, c := range comps {
		if c.Name == "第11届CCPC郑州站报名预告" || c.Name == "第11届CCPC郑州站正式报名通知" {
			zhengzhouCount++
		}
	}
	if zhengzhouCount > 1 {
		t.Fatalf("郑州站 预告/正式报名 must merge, got %d rows", zhengzhouCount)
	}

	// Facts must not cross entities: the 郑州站 registration deadline must not
	// leak into 重庆站 or 济南站. The 郑州站 entity may be titled by either the
	// preview or the formal notice after the merge, so locate it by series/edition.
	zz := findZhengzhou(t, comps)
	if zz.RegistrationEnd == nil {
		t.Fatal("郑州站 registration_end should be set")
	}
	// The 郑州站 deadline (2026-04-19) must not leak into 重庆站/济南站. Each
	// station has its own deadline (2026-04-01) which it must keep.
	zhengzhouEnd := time.Date(2026, 4, 19, 0, 0, 0, 0, researchLocation())
	if !zz.RegistrationEnd.Equal(zhengzhouEnd) {
		t.Fatalf("郑州站 deadline = %v, want 2026-04-19", zz.RegistrationEnd)
	}
	for _, c := range comps {
		if c.Name == "第11届中国大学生程序设计竞赛（CCPC）重庆站" || c.Name == "第11届中国大学生程序设计竞赛（CCPC）济南站" {
			if c.RegistrationEnd == nil || !c.RegistrationEnd.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, researchLocation())) {
				t.Fatalf("重庆/济南 must keep their own 04-01 deadline, got %v", c.RegistrationEnd)
			}
		}
	}

	// Second scan must be idempotent: no new entities, no duplicate events.
	beforeEvents := countEvents(t, dbPath)
	if err := app.run(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	comps2, err := database.ListCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(comps2) != 5 {
		t.Fatalf("rescan must not add entities: got %d, want 5", len(comps2))
	}
	afterEvents := countEvents(t, dbPath)
	if afterEvents != beforeEvents {
		t.Fatalf("rescan added events: before=%d after=%d", beforeEvents, afterEvents)
	}
}

// findZhengzhou locates the single 第11届 郑州站 entity regardless of whether the
// merge kept the preview or the formal-notice title.
func findZhengzhou(t *testing.T, comps []model.Competition) model.Competition {
	t.Helper()
	for _, c := range comps {
		if c.Name == "第11届CCPC郑州站报名预告" || c.Name == "第11届CCPC郑州站正式报名通知" {
			return c
		}
	}
	t.Fatalf("郑州站 entity not found")
	return model.Competition{}
}

// countEvents opens a fresh read connection to the DB file and counts rows in
// competition_events, so event idempotency can be verified across rescans.
func countEvents(t *testing.T, dbPath string) int {
	t.Helper()
	ro, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	return ro.CountCompetitionEvents(context.Background())
}
