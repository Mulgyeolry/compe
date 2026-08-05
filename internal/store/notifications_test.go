package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

// notificationTestStore opens a clean store with one user and one competition
// so notification enqueue tests satisfy the foreign keys.
func notificationTestStore(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "notifications.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))

	// Create a user through the verification flow.
	if err := database.RequestVerification(ctx, "tester@example.com", "hash1", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	user, err := database.ConsumeVerification(ctx, "tester@example.com", "hash1", now, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Create a competition and read back its real persisted ID (the returned
	// value is the old empty row when the insert is new).
	if _, _, err := database.UpsertCompetition(ctx, model.Competition{
		EntityKey:   "test-2026",
		Name:        "2026 测试大赛",
		Status:      model.StatusRegistrationOpen,
		OfficialURL: "https://contest.example.com/2026",
		Trust:       model.TrustHigh,
	}, "test-src", now); err != nil {
		t.Fatal(err)
	}
	persisted, err := database.GetCompetition(ctx, "test-2026")
	if err != nil {
		t.Fatal(err)
	}
	return database, user.ID, persisted.ID
}

func startDispatch(userID, competitionID int64, group string, dueAt time.Time) UserEventDispatch {
	return UserEventDispatch{
		UserID:   userID,
		Event:    model.Event{Type: "competition_started", Key: "started"},
		GroupKey: group,
		DueAt:    dueAt,
	}
}

func TestEnqueueCancelledNotificationIsRestored(t *testing.T) {
	database, userID, competitionID := notificationTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))

	count, err := database.EnqueueUserCompetitionEvents(ctx, competitionID,
		[]UserEventDispatch{startDispatch(userID, competitionID, "group-1", now.Add(time.Hour))}, now)
	if err != nil || count != 1 {
		t.Fatalf("first enqueue count=%d err=%v, want 1", count, err)
	}

	// Cancel the notification.
	var notificationID int64
	if err := database.db.QueryRowContext(ctx,
		`SELECT id FROM user_notifications WHERE user_id=? AND competition_id=?`, userID, competitionID).Scan(&notificationID); err != nil {
		t.Fatal(err)
	}
	if err := database.CancelUserNotifications(ctx, userID, []int64{notificationID}); err != nil {
		t.Fatal(err)
	}

	// Re-enqueue with a new group and due time.
	newDue := now.Add(24 * time.Hour)
	count, err = database.EnqueueUserCompetitionEvents(ctx, competitionID,
		[]UserEventDispatch{startDispatch(userID, competitionID, "group-2", newDue)}, now)
	if err != nil || count != 1 {
		t.Fatalf("re-enqueue count=%d err=%v, want 1 (cancelled restored)", count, err)
	}

	var status, group, lastError string
	var attemptCount int
	var dueAt int64
	if err := database.db.QueryRowContext(ctx,
		`SELECT status,delivery_group,last_error,attempt_count,due_at FROM user_notifications WHERE id=?`, notificationID).
		Scan(&status, &group, &lastError, &attemptCount, &dueAt); err != nil {
		t.Fatal(err)
	}
	if status != "pending" {
		t.Fatalf("status = %s, want pending", status)
	}
	if group != "group-2" {
		t.Fatalf("delivery_group = %s, want group-2", group)
	}
	if lastError != "" {
		t.Fatalf("last_error = %q, want empty", lastError)
	}
	if attemptCount != 0 {
		t.Fatalf("attempt_count = %d, want 0", attemptCount)
	}
	if dueAt != newDue.Unix() {
		t.Fatalf("due_at = %d, want %d", dueAt, newDue.Unix())
	}
}

func TestEnqueuePendingNotificationIsNotDuplicated(t *testing.T) {
	database, userID, competitionID := notificationTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))

	first, err := database.EnqueueUserCompetitionEvents(ctx, competitionID,
		[]UserEventDispatch{startDispatch(userID, competitionID, "group-1", now.Add(time.Hour))}, now)
	if err != nil || first != 1 {
		t.Fatalf("first enqueue count=%d err=%v", first, err)
	}
	second, err := database.EnqueueUserCompetitionEvents(ctx, competitionID,
		[]UserEventDispatch{startDispatch(userID, competitionID, "group-2", now.Add(2*time.Hour))}, now)
	if err != nil || second != 0 {
		t.Fatalf("second enqueue count=%d err=%v, want 0 (no duplicate)", second, err)
	}
	var total int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_notifications WHERE user_id=?`, userID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("notification rows = %d, want 1", total)
	}
}

func TestEnqueueSentNotificationIsNeverRestored(t *testing.T) {
	database, userID, competitionID := notificationTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))

	if _, err := database.EnqueueUserCompetitionEvents(ctx, competitionID,
		[]UserEventDispatch{startDispatch(userID, competitionID, "group-1", now.Add(time.Hour))}, now); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkUserGroupSent(ctx, "group-1", now); err != nil {
		t.Fatal(err)
	}

	count, err := database.EnqueueUserCompetitionEvents(ctx, competitionID,
		[]UserEventDispatch{startDispatch(userID, competitionID, "group-2", now.Add(24*time.Hour))}, now)
	if err != nil || count != 0 {
		t.Fatalf("re-enqueue sent count=%d err=%v, want 0", count, err)
	}
	var status string
	var sentAt *int64
	if err := database.db.QueryRowContext(ctx,
		`SELECT status,sent_at FROM user_notifications WHERE user_id=? AND competition_id=?`, userID, competitionID).
		Scan(&status, &sentAt); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("status = %s, want sent", status)
	}
	if sentAt == nil {
		t.Fatal("sent_at must not be cleared for a sent notification")
	}
}

func TestEnqueueFailedNotificationIsNotReset(t *testing.T) {
	database, userID, competitionID := notificationTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 20, 0, 0, 0, time.FixedZone("CST", 8*3600))

	if _, err := database.EnqueueUserCompetitionEvents(ctx, competitionID,
		[]UserEventDispatch{startDispatch(userID, competitionID, "group-1", now.Add(time.Hour))}, now); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkUserGroupFailed(ctx, "group-1", errors.New("smtp down"), now); err != nil {
		t.Fatal(err)
	}

	count, err := database.EnqueueUserCompetitionEvents(ctx, competitionID,
		[]UserEventDispatch{startDispatch(userID, competitionID, "group-2", now.Add(24*time.Hour))}, now)
	if err != nil || count != 0 {
		t.Fatalf("re-enqueue failed count=%d err=%v, want 0", count, err)
	}
	var status, lastError string
	var attemptCount int
	if err := database.db.QueryRowContext(ctx,
		`SELECT status,last_error,attempt_count FROM user_notifications WHERE user_id=? AND competition_id=?`, userID, competitionID).
		Scan(&status, &lastError, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("status = %s, want failed", status)
	}
	if lastError != "smtp down" {
		t.Fatalf("last_error = %q, want preserved 'smtp down'", lastError)
	}
	if attemptCount != 1 {
		t.Fatalf("attempt_count = %d, want 1 (not reset)", attemptCount)
	}
}
