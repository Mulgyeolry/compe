package service

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/store"
)

// recordingSender captures alert emails so tests can assert delivery without
// touching the real notification backend.
type recordingSender struct {
	subject string
	body    string
	sent    bool
}

func (r *recordingSender) Send(_ context.Context, subject, body string) error {
	r.subject = subject
	r.body = body
	r.sent = true
	return nil
}

func observabilityTestService(t *testing.T, alertEnabled bool) *Service {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "obs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return &Service{
		cfg: config.Config{
			Alert: config.Alert{Enabled: alertEnabled, ConsecutiveFailureLimit: 3},
			Sources: []config.Source{
				{ID: "broken", Name: "失效源"},
				{ID: "healthy", Name: "正常源"},
			},
			Location: time.FixedZone("CST", 8*3600),
		},
		store: database,
		now:   func() time.Time { return time.Date(2026, 8, 5, 20, 0, 0, 0, time.FixedZone("CST", 8*3600)) },
		log:   slog.Default(),
	}
}

func TestNotifyUnhealthySourcesFiresAlertOnce(t *testing.T) {
	ctx := context.Background()
	svc := observabilityTestService(t, true)
	sender := &recordingSender{}
	svc.notifier = sender

	// Record three consecutive failures for "broken".
	for range 3 {
		svc.recordSourceHealth(ctx, svc.cfg.Sources[0], false)
	}
	// "healthy" succeeds once.
	svc.recordSourceHealth(ctx, svc.cfg.Sources[1], true)

	if err := svc.notifyUnhealthySources(ctx, svc.now()); err != nil {
		t.Fatal(err)
	}
	if !sender.sent {
		t.Fatal("expected a source alert to be sent")
	}
	if len(sender.body) == 0 || !contains(sender.body, "失效源") {
		t.Fatalf("alert body missing broken source: %s", sender.body)
	}
	if contains(sender.body, "正常源") {
		t.Fatalf("healthy source should not be alerted: %s", sender.body)
	}

	// A second call must not re-alert for the same failure count.
	sender.sent = false
	if err := svc.notifyUnhealthySources(ctx, svc.now()); err != nil {
		t.Fatal(err)
	}
	if sender.sent {
		t.Fatal("alert was fired more than once for the same outage")
	}
}

func TestNotifyUnhealthySourcesResetsAfterRecovery(t *testing.T) {
	ctx := context.Background()
	svc := observabilityTestService(t, true)
	sender := &recordingSender{}
	svc.notifier = sender

	for range 3 {
		svc.recordSourceHealth(ctx, svc.cfg.Sources[0], false)
	}
	if err := svc.notifyUnhealthySources(ctx, svc.now()); err != nil {
		t.Fatal(err)
	}
	if !sender.sent {
		t.Fatal("first outage was not alerted")
	}

	// Source recovers, then fails again to the threshold.
	svc.recordSourceHealth(ctx, svc.cfg.Sources[0], true)
	sender.sent = false
	for range 3 {
		svc.recordSourceHealth(ctx, svc.cfg.Sources[0], false)
	}
	if err := svc.notifyUnhealthySources(ctx, svc.now()); err != nil {
		t.Fatal(err)
	}
	if !sender.sent {
		t.Fatal("recurring outage after recovery was not re-alerted")
	}
}

func TestNotifyUnhealthySourcesSkipsBelowThreshold(t *testing.T) {
	ctx := context.Background()
	svc := observabilityTestService(t, true)
	sender := &recordingSender{}
	svc.notifier = sender

	svc.recordSourceHealth(ctx, svc.cfg.Sources[0], false)
	svc.recordSourceHealth(ctx, svc.cfg.Sources[0], false)
	if err := svc.notifyUnhealthySources(ctx, svc.now()); err != nil {
		t.Fatal(err)
	}
	if sender.sent {
		t.Fatal("alert must not fire below the failure threshold")
	}
}

func TestRecordSourceHealthDisabledWhenAlertOff(t *testing.T) {
	ctx := context.Background()
	svc := observabilityTestService(t, false)
	sender := &recordingSender{}
	svc.notifier = sender

	svc.recordSourceHealth(ctx, svc.cfg.Sources[0], false)
	// No state should be written, so nothing crosses the threshold.
	if err := svc.notifyUnhealthySources(ctx, svc.now()); err != nil {
		t.Fatal(err)
	}
	if sender.sent {
		t.Fatal("alerting must be fully disabled when alert.enabled is false")
	}
}

func TestNotifyUnhealthySourcesHandlesNilNotifier(t *testing.T) {
	ctx := context.Background()
	svc := observabilityTestService(t, true)
	svc.notifier = nil
	// Should be a no-op rather than a nil-dereference panic.
	if err := svc.notifyUnhealthySources(ctx, svc.now()); err != nil {
		t.Fatal(err)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
