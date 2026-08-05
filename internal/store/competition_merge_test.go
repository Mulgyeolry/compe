package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestUpsertMergesDifferentAnnouncementsForSameCompetition(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "merge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))
	preview := model.Competition{
		EntityKey:      "preview-key",
		Name:           "华为ICT大赛2026-2027即将启动，敬请期待！",
		Organizer:      "华为技术有限公司",
		Status:         model.StatusPreview,
		StatusEvidence: "华为ICT大赛2026-2027即将启动，敬请期待",
		Keywords:       []string{"云计算", "人工智能"},
		Analysis: model.CompetitionAnalysis{
			Summary: "聚焦 ICT 技术实践",
		},
		OfficialURL: "https://example.huawei.com/preview",
		Trust:       model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, preview, "huawei-preview", now); err != nil || !isNew {
		t.Fatalf("insert preview isNew=%v err=%v", isNew, err)
	}
	registration := model.Competition{
		EntityKey:      "registration-key",
		Name:           "关于华为ICT大赛2026-2027正式开放报名的通知",
		Organizer:      "华为技术有限公司",
		Status:         model.StatusRegistrationOpen,
		StatusEvidence: "现已正式开放报名",
		Fee:            "50元/人",
		FeeEvidence:    "本届大赛报名费为50元/人",
		RegistrationEnd: func() *time.Time {
			value := now.AddDate(0, 0, 20)
			return &value
		}(),
		RegistrationEndRaw: "2026年8月24日",
		CompetitionStart: func() *time.Time {
			value := now.AddDate(0, 1, 0)
			return &value
		}(),
		CompetitionStartRaw: "2026年9月4日",
		OfficialURL:         "https://example.huawei.com/registration",
		Trust:               model.TrustHigh,
	}
	old, isNew, err := database.UpsertCompetition(ctx, registration, "huawei-registration", now.Add(time.Hour))
	if err != nil || isNew {
		t.Fatalf("merge registration isNew=%v err=%v", isNew, err)
	}
	if old.Status != model.StatusPreview {
		t.Fatalf("unexpected previous status: %s", old.Status)
	}
	competitions, err := database.ListCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(competitions) != 1 {
		t.Fatalf("same competition created %d rows", len(competitions))
	}
	if competitions[0].Status != model.StatusRegistrationOpen || competitions[0].Fee != "50元/人" || competitions[0].RegistrationEnd == nil || competitions[0].CompetitionStart == nil ||
		len(competitions[0].Keywords) != 2 || competitions[0].Analysis.Summary != "聚焦 ICT 技术实践" {
		t.Fatalf("merged facts missing: %#v", competitions[0])
	}
}

func TestUpsertKeepsDifferentCompetitionYearsSeparate(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "years.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Now()
	for index, year := range []string{"2026", "2027"} {
		competition := model.Competition{
			EntityKey:      "csp-" + year,
			Name:           year + "年CCF CSP认证报名通知",
			Organizer:      "中国计算机学会",
			Status:         model.StatusRegistrationOpen,
			StatusEvidence: "报名中",
			OfficialURL:    "https://example.org/csp/" + year,
			Trust:          model.TrustHigh,
		}
		if _, isNew, err := database.UpsertCompetition(ctx, competition, "csp", now.Add(time.Duration(index)*time.Hour)); err != nil || !isNew {
			t.Fatalf("year %s isNew=%v err=%v", year, isNew, err)
		}
	}
	competitions, err := database.ListCompetitions(ctx)
	if err != nil || len(competitions) != 2 {
		t.Fatalf("competitions=%d err=%v", len(competitions), err)
	}
}

func TestUpsertKeepsDifferentEditionsInSameYearSeparate(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "editions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Now()
	for index, edition := range []string{"第十届", "第十一届"} {
		competition := model.Competition{
			EntityKey:      "programming-" + edition,
			Name:           "2026年全国大学生" + edition + "程序设计大赛",
			Organizer:      "程序设计竞赛委员会",
			Status:         model.StatusRegistrationOpen,
			StatusEvidence: "报名中",
			OfficialURL:    "https://example.org/programming/" + edition,
			Trust:          model.TrustHigh,
		}
		if _, isNew, err := database.UpsertCompetition(ctx, competition, "programming", now.Add(time.Duration(index)*time.Hour)); err != nil || !isNew {
			t.Fatalf("edition %s isNew=%v err=%v", edition, isNew, err)
		}
	}
	competitions, err := database.ListCompetitions(ctx)
	if err != nil || len(competitions) != 2 {
		t.Fatalf("competitions=%d err=%v", len(competitions), err)
	}
}

func TestAnalyzerUpgradeCanClearInvalidatedFactsFromSameSource(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "analysis-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	deadline := now.AddDate(0, 1, 0)
	legacy := model.Competition{
		EntityKey:          "competition-2026",
		Name:               "2026计算机设计大赛",
		Organizer:          "旧页面误提取的学院",
		Status:             model.StatusRegistrationOpen,
		StatusEvidence:     "报名中",
		RegistrationEnd:    &deadline,
		RegistrationEndRaw: "2026年9月4日",
		Fee:                "100元",
		FeeEvidence:        "报名费100元",
		OfficialURL:        "https://example.org/competition",
		Trust:              model.TrustHigh,
		AnalyzerVersion:    "competition-status-v1",
	}
	if _, isNew, err := database.UpsertCompetition(ctx, legacy, "official", now); err != nil || !isNew {
		t.Fatalf("insert legacy isNew=%v err=%v", isNew, err)
	}
	corrected := model.Competition{
		EntityKey:       legacy.EntityKey,
		Name:            legacy.Name,
		Status:          model.StatusUnknown,
		OfficialURL:     legacy.OfficialURL,
		Trust:           model.TrustHigh,
		AnalyzerVersion: "competition-events-v2",
	}
	if _, isNew, err := database.UpsertCompetition(ctx, corrected, "official", now.Add(time.Hour)); err != nil || isNew {
		t.Fatalf("upgrade isNew=%v err=%v", isNew, err)
	}
	saved, err := database.GetCompetition(ctx, legacy.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != model.StatusUnknown || saved.StatusEvidence != "" || saved.RegistrationEnd != nil || saved.RegistrationEndRaw != "" || saved.Fee != "" || saved.Organizer != "" {
		t.Fatalf("invalidated v1 facts survived analyzer upgrade: %#v", saved)
	}
}

// TestUpsertReusedURLCreatesSeparateCompetitionForNewYear verifies that when
// an official site reuses the same URL for a new edition in a different year,
// the new competition is created as a separate row instead of silently merging
// into the existing year's row (which would corrupt status and per-user choices).
func TestUpsertReusedURLCreatesSeparateCompetitionForNewYear(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "url-years.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))
	const sharedURL = "https://contest.example.com/notice"

	first := model.Competition{
		EntityKey:   "huawei-2026",
		Name:        "2026 华为软件精英挑战赛",
		Status:      model.StatusRegistrationOpen,
		OfficialURL: sharedURL,
		Trust:       model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, first, "huawei", now); err != nil || !isNew {
		t.Fatalf("insert 2026 isNew=%v err=%v", isNew, err)
	}

	second := model.Competition{
		EntityKey:   "huawei-2027",
		Name:        "2027 华为软件精英挑战赛",
		Status:      model.StatusRegistrationOpen,
		OfficialURL: sharedURL,
		Trust:       model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, second, "huawei", now.Add(time.Hour)); err != nil || !isNew {
		t.Fatalf("insert 2027 isNew=%v err=%v (expected a new row for the new year)", isNew, err)
	}

	competitions, err := database.ListCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(competitions) != 2 {
		t.Fatalf("reused URL with different years produced %d rows, want 2", len(competitions))
	}
	if competitions[0].ID == competitions[1].ID {
		t.Fatal("2026 and 2027 must be distinct competition rows")
	}
	// The 2026 row must not have been overwritten by the 2027 data.
	if competitions[0].Name != "2026 华为软件精英挑战赛" && competitions[1].Name != "2026 华为软件精英挑战赛" {
		t.Fatalf("2026 competition name was not preserved: %#v", competitions)
	}
}

// TestUpsertReusedURLUpdatesSameYearChangedTitle verifies that a different
// announcement for the SAME year (same URL, same year, changed title) still
// merges into the existing row.
func TestUpsertReusedURLUpdatesSameYearChangedTitle(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "url-sameyear.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))
	const sharedURL = "https://contest.example.com/notice"

	first := model.Competition{
		EntityKey:   "huawei-2026-a",
		Name:        "2026 华为软件精英挑战赛报名预告",
		Status:      model.StatusPreview,
		OfficialURL: sharedURL,
		Trust:       model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, first, "huawei", now); err != nil || !isNew {
		t.Fatalf("insert preview isNew=%v err=%v", isNew, err)
	}

	second := model.Competition{
		EntityKey:   "huawei-2026-b",
		Name:        "2026 华为软件精英挑战赛正式报名通知",
		Status:      model.StatusRegistrationOpen,
		OfficialURL: sharedURL,
		Trust:       model.TrustHigh,
	}
	old, isNew, err := database.UpsertCompetition(ctx, second, "huawei", now.Add(time.Hour))
	if err != nil || isNew {
		t.Fatalf("same-year changed title isNew=%v err=%v (expected update, not new row)", isNew, err)
	}
	if old.Status != model.StatusPreview {
		t.Fatalf("expected previous status preview, got %s", old.Status)
	}

	competitions, err := database.ListCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(competitions) != 1 {
		t.Fatalf("same-year reused URL created %d rows, want 1", len(competitions))
	}
	if competitions[0].Status != model.StatusRegistrationOpen {
		t.Fatalf("new announcement did not update the existing competition: %#v", competitions[0])
	}
}

// TestUpsertReusedURLCreatesSeparateCompetitionForDifferentEdition verifies
// that two editions with explicit different ordinals ("第十届" vs "第十一届")
// on the same URL are kept as separate competitions.
func TestUpsertReusedURLCreatesSeparateCompetitionForDifferentEdition(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "url-editions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Now()
	const sharedURL = "https://contest.example.com/cloud"
	editions := []string{"第十届全国大学生云计算大赛", "第十一届全国大学生云计算大赛"}
	for index, name := range editions {
		competition := model.Competition{
			EntityKey:   fmt.Sprintf("cloud-%d", index),
			Name:        name,
			Status:      model.StatusRegistrationOpen,
			OfficialURL: sharedURL,
			Trust:       model.TrustHigh,
		}
		if _, isNew, err := database.UpsertCompetition(ctx, competition, "cloud", now.Add(time.Duration(index)*time.Hour)); err != nil || !isNew {
			t.Fatalf("edition %q isNew=%v err=%v", name, isNew, err)
		}
	}
	competitions, err := database.ListCompetitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(competitions) != 2 {
		t.Fatalf("different explicit editions on same URL produced %d rows, want 2", len(competitions))
	}
	if competitions[0].ID == competitions[1].ID {
		t.Fatal("the two editions must be distinct rows")
	}
}

func TestCompetitionFactsAndLifecyclePhasesRoundTrip(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "facts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	competition := model.Competition{
		EntityKey:         "facts-2026",
		Name:              "2026 AI Agent大赛",
		RegistrationPhase: model.RegistrationOpen,
		CompetitionPhase:  model.CompetitionUpcoming,
		OfficialURL:       "https://example.org/contest",
		Trust:             model.TrustHigh,
		Facts: map[string]model.FactEvidence{
			model.FactRegistrationEnd: {
				Value: "2026-09-01", Evidence: "报名截止时间为2026年9月1日", Edition: "2026",
				SourceURL: "https://example.org/contest", Confidence: "high", ObservedAt: now,
			},
		},
	}
	if _, isNew, err := database.UpsertCompetition(ctx, competition, "official", now); err != nil || !isNew {
		t.Fatalf("insert isNew=%v err=%v", isNew, err)
	}
	saved, err := database.GetCompetition(ctx, competition.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != model.StatusUpcoming || saved.RegistrationPhase != model.RegistrationOpen || saved.CompetitionPhase != model.CompetitionUpcoming {
		t.Fatalf("lifecycle did not round-trip: %#v", saved)
	}
	if saved.Facts[model.FactRegistrationEnd].Evidence != "报名截止时间为2026年9月1日" {
		t.Fatalf("facts did not round-trip: %#v", saved.Facts)
	}
}
