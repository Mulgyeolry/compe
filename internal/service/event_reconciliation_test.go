package service_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
	"competition-assistant/internal/service"
)

// purgeCommittedEvents simulates a scan that was interrupted after upserting a
// competition but before its events and notifications were durably committed.
// It deletes only the event and notification rows, leaving the competition and
// its change-detection observation intact so the normal scan path skips it.
func purgeCommittedEvents(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, table := range []string{"competition_events", "user_notifications"} {
		if _, err := raw.Exec("DELETE FROM " + table); err != nil {
			t.Fatalf("purge %s: %v", table, err)
		}
	}
}

func countRows(t *testing.T, path, query string) int {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var count int
	if err := raw.QueryRow(query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestReconcileRecoversRegistrationOpened proves a scan-end reconciliation
// restores a registration_opened event and a single user notification that a
// previous scan lost when it crashed after ingesting the competition.
func TestReconcileRecoversRegistrationOpened(t *testing.T) {
	doc := model.Document{
		Title: "2026 全国程序设计大赛报名通知",
		URL:   testPageBase,
		Text:  "主办方：中国计算机学会。报名已经开始。报名时间：2026年8月3日至2026年8月20日。可单人参赛。比赛内容为算法和程序设计。",
	}
	cfg := baseConfig(t)
	cfg.Sources = []config.Source{{ID: "csp", Name: "CCF CSP", Kind: "page", URL: testPageBase, Trust: "high", Limit: 10}}
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	sender := &memorySender{}
	app := service.New(cfg, database, pageCollector(func() model.Document { return doc }), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	enableAllCategoryTestUser(t, database, app, "fixture@example.com", fixedNow())
	app.SetNow(fixedNow)
	ctx := context.Background()

	// First scan ingests the competition and commits its registration_opened
	// event and notification.
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events WHERE event_type='registration_opened'"); got != 1 {
		t.Fatalf("initial registration_opened events=%d, want 1", got)
	}

	// Simulate the interrupted scan: the competition and its observation remain,
	// but the events and notifications were never durably written.
	purgeCommittedEvents(t, cfg.DBPath)
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events"); got != 0 {
		t.Fatalf("after purge events=%d, want 0", got)
	}

	// Second scan: content hash and analyzer version are unchanged, so the normal
	// path skips the document; reconciliation restores the missing event.
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events WHERE event_type='registration_opened'"); got != 1 {
		t.Fatalf("reconciled registration_opened events=%d, want 1", got)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM user_notifications"); got != 1 {
		t.Fatalf("reconciled user_notifications=%d, want 1", got)
	}

	// Third scan must not duplicate the event or notification.
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events WHERE event_type='registration_opened'"); got != 1 {
		t.Fatalf("post-reconcile registration_opened events=%d, want 1 (idempotent)", got)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM user_notifications"); got != 1 {
		t.Fatalf("post-reconcile user_notifications=%d, want 1 (idempotent)", got)
	}
}

// TestReconcileRecoversCompetitionDiscovered proves reconciliation restores the
// competition_discovered event for a current unknown/unknown announcement whose
// event was lost to an interrupted scan.
func TestReconcileRecoversCompetitionDiscovered(t *testing.T) {
	// A year-less, dateless announcement is analysed as unknown/unknown but is a
	// current discoverable announcement, so it qualifies for competition_discovered.
	doc := model.Document{
		Title: "华为软件精英挑战赛官网",
		URL:   testPageBase,
		Text:  "华为软件精英挑战赛，面向全球在校学生开放的算法竞技赛事。赛题围绕真实云场景下的资源调度与优化问题展开。",
	}
	cfg := baseConfig(t)
	cfg.Sources = []config.Source{{ID: "huawei", Name: "华为挑战赛", Kind: "page", URL: testPageBase, Trust: "high", Limit: 10}}
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	sender := &memorySender{}
	app := service.New(cfg, database, pageCollector(func() model.Document { return doc }), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	enableAllCategoryTestUser(t, database, app, "fixture@example.com", fixedNow())
	app.SetNow(fixedNow)
	ctx := context.Background()

	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events WHERE event_type='competition_discovered'"); got != 1 {
		t.Fatalf("initial competition_discovered events=%d, want 1", got)
	}

	purgeCommittedEvents(t, cfg.DBPath)

	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events WHERE event_type='competition_discovered'"); got != 1 {
		t.Fatalf("reconciled competition_discovered events=%d, want 1", got)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM user_notifications"); got != 1 {
		t.Fatalf("reconciled user_notifications=%d, want 1", got)
	}

	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events WHERE event_type='competition_discovered'"); got != 1 {
		t.Fatalf("post-reconcile competition_discovered events=%d, want 1 (idempotent)", got)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM user_notifications"); got != 1 {
		t.Fatalf("post-reconcile user_notifications=%d, want 1 (idempotent)", got)
	}
}
