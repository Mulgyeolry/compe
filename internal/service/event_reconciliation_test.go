package service_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
	"competition-assistant/internal/service"
)

// purgeCommittedEvents simulates a scan that was interrupted after upserting a
// competition but before its events, notifications and bootstrap marker were
// durably committed. It deletes only the event, notification and (optionally)
// bootstrap rows, leaving the competition and its change-detection observation
// intact so the normal scan path skips it.
func purgeCommittedEvents(t *testing.T, path string, purgeBootstrap bool) {
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
	if purgeBootstrap {
		if _, err := raw.Exec("DELETE FROM meta WHERE key='bootstrapped'"); err != nil {
			t.Fatalf("purge bootstrap: %v", err)
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

// emptyCollector yields no candidates, so a scan with it performs no normal-path
// ingestion and only the scan-end event reconciliation touches the database.
func emptyCollector() *scriptedCollector {
	return &scriptedCollector{
		discover: func(context.Context, config.Source) ([]model.Candidate, error) { return nil, nil },
		fetch:    func(context.Context, string) (model.Document, error) { return model.Document{}, nil },
	}
}

// TestReconcileSkipsIneligibleCompetitions proves reconciliation never builds
// events or notifications for a registration-open competition that is low-trust
// or catalog-ineligible, while still reconciling an eligible control.
func TestReconcileSkipsIneligibleCompetitions(t *testing.T) {
	start := fixedNow()
	future := start.Add(7 * 24 * time.Hour)
	cases := []struct {
		name        string
		competition model.Competition
		wantEvents  int
	}{
		{
			name: "eligible control",
			competition: model.Competition{
				EntityKey:         "eligible-control",
				Name:              "2026 程序设计大赛报名通知",
				Status:            model.StatusRegistrationOpen,
				RegistrationPhase: model.RegistrationOpen,
				CompetitionPhase:  model.CompetitionUnknown,
				RegistrationStart: &start,
				RegistrationEnd:   &future,
				OfficialURL:       testPageBase,
				Trust:             model.TrustHigh,
				FirstSeen:         fixedNow(),
				LastSeen:          fixedNow(),
				AnalyzerVersion:   "test",
				ContentHash:       "hash-control",
			},
			wantEvents: 1,
		},
		{
			name: "low trust",
			competition: model.Competition{
				EntityKey:         "low-trust",
				Name:              "2026 程序设计大赛报名通知",
				Status:            model.StatusRegistrationOpen,
				RegistrationPhase: model.RegistrationOpen,
				CompetitionPhase:  model.CompetitionUnknown,
				RegistrationStart: &start,
				RegistrationEnd:   &future,
				OfficialURL:       testPageBase,
				Trust:             model.TrustLow,
				FirstSeen:         fixedNow(),
				LastSeen:          fixedNow(),
				AnalyzerVersion:   "test",
				ContentHash:       "hash-low",
			},
			wantEvents: 0,
		},
		{
			name: "catalog ineligible",
			competition: model.Competition{
				EntityKey:         "catalog-ineligible",
				Name:              "2026 程序设计大赛获奖名单",
				Status:            model.StatusRegistrationOpen,
				RegistrationPhase: model.RegistrationOpen,
				CompetitionPhase:  model.CompetitionUnknown,
				RegistrationStart: &start,
				RegistrationEnd:   &future,
				OfficialURL:       testPageBase,
				Trust:             model.TrustHigh,
				FirstSeen:         fixedNow(),
				LastSeen:          fixedNow(),
				AnalyzerVersion:   "test",
				ContentHash:       "hash-cat",
			},
			wantEvents: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig(t)
			cfg.Sources = []config.Source{{ID: "dummy", Name: "dummy", Kind: "page", URL: testPageBase, Trust: "high", Limit: 1}}
			database := openStore(t, cfg.DBPath)
			defer database.Close()
			ctx := context.Background()
			if err := database.MarkBootstrapped(ctx); err != nil {
				t.Fatal(err)
			}
			if _, isNew, err := database.UpsertCompetition(ctx, tc.competition, "source", fixedNow()); err != nil || !isNew {
				t.Fatalf("insert competition: isNew=%v err=%v", isNew, err)
			}
			sender := &memorySender{}
			app := service.New(cfg, database, emptyCollector(), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
			enableAllCategoryTestUser(t, database, app, "fixture@example.com", fixedNow())
			app.SetNow(fixedNow)
			if err := app.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events"); got != tc.wantEvents {
				t.Fatalf("competition_events=%d, want %d", got, tc.wantEvents)
			}
			if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM user_notifications"); got != tc.wantEvents {
				t.Fatalf("user_notifications=%d, want %d", got, tc.wantEvents)
			}
		})
	}
}

func isBootstrapped(t *testing.T, path string) bool {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var count int
	if err := raw.QueryRow("SELECT COUNT(*) FROM meta WHERE key='bootstrapped'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count > 0
}

// TestReconcileRecoversRegistrationOpenedOnBootstrap proves a scan-end
// reconciliation restores a registration_opened event and a single user
// notification, and re-writes the bootstrap marker, when the very first scan was
// interrupted after ingesting the competition but before committing anything.
func TestReconcileRecoversRegistrationOpenedOnBootstrap(t *testing.T) {
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

	// First scan ingests the competition, commits its registration_opened event
	// and notification, and marks the system bootstrapped.
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events WHERE event_type='registration_opened'"); got != 1 {
		t.Fatalf("initial registration_opened events=%d, want 1", got)
	}
	if !isBootstrapped(t, cfg.DBPath) {
		t.Fatal("system should be bootstrapped after first scan")
	}

	// Simulate the interrupted bootstrap scan: the competition and its
	// observation remain, but the events, notifications and bootstrap marker
	// were never durably written.
	purgeCommittedEvents(t, cfg.DBPath, true)
	if isBootstrapped(t, cfg.DBPath) {
		t.Fatal("bootstrapped should be false after purge")
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events"); got != 0 {
		t.Fatalf("after purge events=%d, want 0", got)
	}

	// Second scan: content hash and analyzer version are unchanged, so the normal
	// path skips the document; reconciliation (now unconditional) restores the
	// missing event, notification and bootstrap marker in the same scan.
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM competition_events WHERE event_type='registration_opened'"); got != 1 {
		t.Fatalf("reconciled registration_opened events=%d, want 1", got)
	}
	if got := countRows(t, cfg.DBPath, "SELECT COUNT(*) FROM user_notifications"); got != 1 {
		t.Fatalf("reconciled user_notifications=%d, want 1", got)
	}
	if !isBootstrapped(t, cfg.DBPath) {
		t.Fatal("bootstrapped should be re-written after recovery scan")
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
// competition_discovered event for a current unknown/unknown announcement after
// the system is already bootstrapped (normal-operation recovery).
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

	// Keep the system bootstrapped; only the events and notifications are lost,
	// which mirrors a crash during a normal (post-bootstrap) scan.
	purgeCommittedEvents(t, cfg.DBPath, false)

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
