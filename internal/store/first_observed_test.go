package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestFirstObservedAtReturnsEarliestObservation(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "first-observed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	const url = "https://contest.example.com/notice"

	t1 := time.Date(2026, 8, 1, 9, 0, 0, 0, time.FixedZone("CST", 8*3600))
	t2 := time.Date(2026, 8, 3, 18, 30, 0, 0, time.FixedZone("CST", 8*3600))

	doc1 := model.Document{URL: url, Title: "旧内容", Text: "第一版内容"}
	if _, err := database.RecordObservationVersioned(ctx, "src-a", doc1, "hash-a", model.TrustHigh, "v1", t1); err != nil {
		t.Fatal(err)
	}
	doc2 := model.Document{URL: url, Title: "新内容", Text: "第二版内容"}
	if _, err := database.RecordObservationVersioned(ctx, "src-b", doc2, "hash-b", model.TrustHigh, "v1", t2); err != nil {
		t.Fatal(err)
	}

	got, err := database.FirstObservedAt(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(t1) {
		t.Fatalf("FirstObservedAt() = %v, want earliest %v", got, t1)
	}
}

func TestFirstObservedAtReturnsErrNoRowsWhenNeverSeen(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "first-observed-empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	_, err = database.FirstObservedAt(context.Background(), "https://never-seen.example.com/x")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("FirstObservedAt() err = %v, want sql.ErrNoRows", err)
	}
}

func TestFirstObservedAtRejectsEmptyURL(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "first-observed-blank.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	if _, err := database.FirstObservedAt(context.Background(), ""); err == nil {
		t.Fatal("empty URL must be rejected")
	}
}

func TestUpsertPreservesProvidedFirstSeen(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "upsert-firstseen.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()

	t1 := time.Date(2026, 8, 1, 9, 0, 0, 0, time.FixedZone("CST", 8*3600))  // FirstSeen
	t2 := time.Date(2026, 8, 5, 20, 0, 0, 0, time.FixedZone("CST", 8*3600)) // Upsert time
	if !t1.Before(t2) {
		t.Fatal("test requires t1 < t2")
	}

	value := model.Competition{
		EntityKey:   "huawei-2026",
		Name:        "2026 华为软件精英挑战赛",
		Status:      model.StatusRegistrationOpen,
		FirstSeen:   t1,
		OfficialURL: "https://contest.example.com/notice",
		Trust:       model.TrustHigh,
	}
	_, isNew, err := database.UpsertCompetition(ctx, value, "huawei", t2)
	if err != nil || !isNew {
		t.Fatalf("UpsertCompetition isNew=%v err=%v", isNew, err)
	}
	// The returned value is the old (empty) row when isNew is true, so the
	// persisted FirstSeen/LastSeen must be verified by reading the row back.
	persisted, err := database.GetCompetition(ctx, value.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.FirstSeen.Equal(t1) {
		t.Fatalf("persisted FirstSeen = %v, want preserved %v", persisted.FirstSeen, t1)
	}
	if !persisted.LastSeen.Equal(t2) {
		t.Fatalf("persisted LastSeen = %v, want %v", persisted.LastSeen, t2)
	}
}
