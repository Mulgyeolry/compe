package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestListUnconfirmedCompetitionsReturnsOnlyUnknownStatus(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "unconfirmed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))

	unconfirmed := model.Competition{
		EntityKey:   "unconfirmed-key",
		Name:        "2026云原生开发者大赛",
		OfficialURL: "https://example.org/cloud-native",
		Trust:       model.TrustMedium,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, unconfirmed, "test", now); err != nil || !isNew {
		t.Fatalf("insert unconfirmed isNew=%v err=%v", isNew, err)
	}
	preview := model.Competition{
		EntityKey:      "preview-key",
		Name:           "2027华为ICT大赛预告",
		Status:         model.StatusPreview,
		StatusEvidence: "即将启动",
		OfficialURL:    "https://example.org/huawei",
		Trust:          model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, preview, "test", now); err != nil || !isNew {
		t.Fatalf("insert preview isNew=%v err=%v", isNew, err)
	}
	finished := model.Competition{
		EntityKey:      "finished-key",
		Name:           "2025全国大学生程序设计大赛",
		Status:         model.StatusFinished,
		StatusEvidence: "赛事已结束",
		OfficialURL:    "https://example.org/finished",
		Trust:          model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, finished, "test", now); err != nil || !isNew {
		t.Fatalf("insert finished isNew=%v err=%v", isNew, err)
	}

	result, err := database.ListUnconfirmedCompetitions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("unconfirmed count=%d, want 1: %#v", len(result), result)
	}
	if result[0].EntityKey != "unconfirmed-key" || result[0].Status != model.StatusUnknown {
		t.Fatalf("unexpected unconfirmed competition: %#v", result[0])
	}
}

func TestListUnconfirmedCompetitionsRespectsLimit(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "unconfirmed-limit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))
	names := []string{"2026云原生开发者大赛", "2027云原生开发者大赛", "2028云原生开发者大赛", "2029云原生开发者大赛", "2030云原生开发者大赛"}
	for index, name := range names {
		competition := model.Competition{
			EntityKey:   "unconfirmed-" + string(rune('a'+index)),
			Name:        name,
			OfficialURL: "https://example.org/" + string(rune('a'+index)),
			Trust:       model.TrustMedium,
		}
		if _, isNew, err := database.UpsertCompetition(ctx, competition, "test", now); err != nil || !isNew {
			t.Fatalf("insert %d isNew=%v err=%v", index, isNew, err)
		}
	}
	result, err := database.ListUnconfirmedCompetitions(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("unconfirmed count=%d, want 2", len(result))
	}
}