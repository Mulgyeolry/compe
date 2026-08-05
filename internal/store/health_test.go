package store

import (
	"context"
	"path/filepath"
	"testing"
)

func newHealthStore(t *testing.T) *Store {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "health.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func TestRecordSourceResultTracksConsecutiveFailures(t *testing.T) {
	database := newHealthStore(t)
	ctx := context.Background()

	for _, want := range []int{1, 2, 3} {
		got, err := database.RecordSourceResult(ctx, "ccf-csp", false)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("failure count = %d, want %d", got, want)
		}
	}

	if got, err := database.RecordSourceResult(ctx, "ccf-csp", true); err != nil || got != 0 {
		t.Fatalf("success should reset count, got=%d err=%v", got, err)
	}
	if got, err := database.GetSourceConsecutiveFailures(ctx, "ccf-csp"); err != nil || got != 0 {
		t.Fatalf("count after reset = %d err=%v, want 0", got, err)
	}
}

func TestGetSourceConsecutiveFailuresDefaultsZero(t *testing.T) {
	database := newHealthStore(t)
	got, err := database.GetSourceConsecutiveFailures(context.Background(), "never-seen")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("default failure count = %d, want 0", got)
	}
}

func TestSourceAlertStatePersistsAcrossOperations(t *testing.T) {
	database := newHealthStore(t)
	ctx := context.Background()

	if state, err := database.GetSourceAlertState(ctx, "tianchi"); err != nil || state.LastAlertedFailures != 0 {
		t.Fatalf("default alert state = %+v err=%v", state, err)
	}

	if err := database.SetSourceAlertState(ctx, "tianchi", 5); err != nil {
		t.Fatal(err)
	}
	state, err := database.GetSourceAlertState(ctx, "tianchi")
	if err != nil || state.LastAlertedFailures != 5 {
		t.Fatalf("alert state = %+v err=%v, want LastAlertedFailures=5", state, err)
	}

	if err := database.SetSourceAlertState(ctx, "tianchi", 0); err != nil {
		t.Fatal(err)
	}
	if state, err := database.GetSourceAlertState(ctx, "tianchi"); err != nil || state.LastAlertedFailures != 0 {
		t.Fatalf("reset alert state = %+v err=%v, want 0", state, err)
	}
}
