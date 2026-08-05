package store

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"competition-assistant/internal/model"
)

var (
	ErrVerificationRateLimited = errors.New("verification code requested too frequently")
	ErrInvalidVerificationCode = errors.New("invalid or expired verification code")
)

type UserEventDispatch struct {
	UserID   int64
	Event    model.Event
	GroupKey string
	DueAt    time.Time
}

const maxVerificationAttempts = 5

func (s *Store) RequestVerification(ctx context.Context, email, codeHash string, now time.Time, ttl time.Duration) error {
	email = strings.ToLower(strings.TrimSpace(email))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var lastCreated int64
	err = tx.QueryRowContext(ctx, `SELECT created_at FROM verification_codes WHERE email=? ORDER BY created_at DESC LIMIT 1`, email).Scan(&lastCreated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && now.Unix()-lastCreated < 60 {
		return ErrVerificationRateLimited
	}
	if _, err := tx.ExecContext(ctx, `UPDATE verification_codes SET consumed_at=? WHERE email=? AND consumed_at IS NULL`, now.Unix(), email); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO verification_codes(email,code_hash,expires_at,created_at) VALUES(?,?,?,?)`,
		email, codeHash, now.Add(ttl).Unix(), now.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ConsumeVerification(ctx context.Context, email, codeHash string, now time.Time, defaultCategories []string) (model.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.User{}, err
	}
	defer tx.Rollback()

	var codeID, expiresAt int64
	var storedHash string
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT id,code_hash,expires_at,attempts FROM verification_codes
WHERE email=? AND consumed_at IS NULL ORDER BY created_at DESC LIMIT 1`, email).Scan(&codeID, &storedHash, &expiresAt, &attempts)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.User{}, ErrInvalidVerificationCode
		}
		return model.User{}, err
	}
	validHash := subtle.ConstantTimeCompare([]byte(storedHash), []byte(codeHash)) == 1
	if expiresAt <= now.Unix() || attempts >= maxVerificationAttempts || !validHash {
		_, _ = tx.ExecContext(ctx, `UPDATE verification_codes SET attempts=attempts+1 WHERE id=?`, codeID)
		if err := tx.Commit(); err != nil {
			return model.User{}, err
		}
		return model.User{}, ErrInvalidVerificationCode
	}
	if _, err := tx.ExecContext(ctx, `UPDATE verification_codes SET consumed_at=? WHERE id=?`, now.Unix(), codeID); err != nil {
		return model.User{}, err
	}

	var user model.User
	var verifiedAt, createdAt, updatedAt int64
	var enabled int
	err = tx.QueryRowContext(ctx, `INSERT INTO users(email,verified_at,enabled,created_at,updated_at) VALUES(?,?,1,?,?)
ON CONFLICT(email) DO UPDATE SET verified_at=excluded.verified_at,enabled=1,updated_at=excluded.updated_at
RETURNING id,email,verified_at,enabled,created_at,updated_at`, email, now.Unix(), now.Unix(), now.Unix()).
		Scan(&user.ID, &user.Email, &verifiedAt, &enabled, &createdAt, &updatedAt)
	if err != nil {
		return model.User{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO user_preferences(user_id) VALUES(?)`, user.ID); err != nil {
		return model.User{}, err
	}
	for _, category := range defaultCategories {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO user_categories(user_id,category) VALUES(?,?)`, user.ID, category); err != nil {
			return model.User{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.User{}, err
	}
	user.VerifiedAt = time.Unix(verifiedAt, 0)
	user.Enabled = enabled == 1
	user.CreatedAt = time.Unix(createdAt, 0)
	user.UpdatedAt = time.Unix(updatedAt, 0)
	return user, nil
}

func (s *Store) CreateSession(ctx context.Context, userID int64, tokenHash string, expiresAt, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at<=?`, now.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,expires_at,created_at) VALUES(?,?,?,?)`, tokenHash, userID, expiresAt.Unix(), now.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UserBySession(ctx context.Context, tokenHash string, now time.Time) (model.User, error) {
	var user model.User
	var verifiedAt, createdAt, updatedAt int64
	var enabled int
	err := s.db.QueryRowContext(ctx, `SELECT u.id,u.email,u.verified_at,u.enabled,u.created_at,u.updated_at
FROM sessions s JOIN users u ON u.id=s.user_id
WHERE s.token_hash=? AND s.expires_at>? AND u.enabled=1`, tokenHash, now.Unix()).
		Scan(&user.ID, &user.Email, &verifiedAt, &enabled, &createdAt, &updatedAt)
	if err != nil {
		return model.User{}, err
	}
	user.VerifiedAt = time.Unix(verifiedAt, 0)
	user.Enabled = enabled == 1
	user.CreatedAt = time.Unix(createdAt, 0)
	user.UpdatedAt = time.Unix(updatedAt, 0)
	return user, nil
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

func (s *Store) GetUserPreferences(ctx context.Context, userID int64) (model.UserPreferences, error) {
	preferences := model.UserPreferences{UserID: userID}
	var frequency, trust string
	var weeklyDay int
	var allowRisk, preview, registration, upcoming, started, problem, deadline7, deadline1, important int
	err := s.db.QueryRowContext(ctx, `SELECT frequency,delivery_time,weekly_day,timezone,min_trust,allow_eligibility_risk,
notify_preview,notify_registration,notify_upcoming,notify_started,notify_problem_release,notify_deadline_7d,notify_deadline_1d,notify_important_update
FROM user_preferences WHERE user_id=?`, userID).Scan(&frequency, &preferences.DeliveryTime, &weeklyDay, &preferences.Timezone, &trust,
		&allowRisk, &preview, &registration, &upcoming, &started, &problem, &deadline7, &deadline1, &important)
	if err != nil {
		return model.UserPreferences{}, err
	}
	preferences.Frequency = model.DeliveryFrequency(frequency)
	preferences.WeeklyDay = time.Weekday(weeklyDay)
	preferences.MinTrust = model.Trust(trust)
	preferences.AllowEligibilityRisk = allowRisk == 1
	preferences.NotifyPreview = preview == 1
	preferences.NotifyRegistration = registration == 1
	preferences.NotifyUpcoming = upcoming == 1
	preferences.NotifyStarted = started == 1
	preferences.NotifyProblemRelease = problem == 1
	preferences.NotifyDeadline7Days = deadline7 == 1
	preferences.NotifyDeadline1Day = deadline1 == 1
	preferences.NotifyImportantUpdate = important == 1

	preferences.Categories, err = s.listUserStrings(ctx, `SELECT category FROM user_categories WHERE user_id=? ORDER BY category`, userID)
	if err != nil {
		return model.UserPreferences{}, err
	}
	preferences.OrganizerTypes, err = s.listUserStrings(ctx, `SELECT organizer_type FROM user_organizer_types WHERE user_id=? ORDER BY organizer_type`, userID)
	if err != nil {
		return model.UserPreferences{}, err
	}
	preferences.CompetitionScopes, err = s.listUserStrings(ctx, `SELECT scope FROM user_competition_scopes WHERE user_id=? ORDER BY scope`, userID)
	if err != nil {
		return model.UserPreferences{}, err
	}
	preferences.Regions, err = s.listUserStrings(ctx, `SELECT region FROM user_regions WHERE user_id=? ORDER BY region`, userID)
	if err != nil {
		return model.UserPreferences{}, err
	}
	preferences.IncludeKeywords, err = s.listUserStrings(ctx, `SELECT keyword FROM user_keywords WHERE user_id=? AND kind='include' ORDER BY keyword`, userID)
	if err != nil {
		return model.UserPreferences{}, err
	}
	preferences.ExcludeKeywords, err = s.listUserStrings(ctx, `SELECT keyword FROM user_keywords WHERE user_id=? AND kind='exclude' ORDER BY keyword`, userID)
	return preferences, err
}

func (s *Store) SaveUserPreferences(ctx context.Context, preferences model.UserPreferences, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE user_preferences SET frequency=?,delivery_time=?,weekly_day=?,timezone=?,min_trust=?,
allow_eligibility_risk=?,notify_preview=?,notify_registration=?,notify_upcoming=?,notify_started=?,notify_problem_release=?,notify_deadline_7d=?,notify_deadline_1d=?,notify_important_update=?
WHERE user_id=?`, string(preferences.Frequency), preferences.DeliveryTime, int(preferences.WeeklyDay), preferences.Timezone, string(preferences.MinTrust),
		boolInt(preferences.AllowEligibilityRisk), boolInt(preferences.NotifyPreview), boolInt(preferences.NotifyRegistration),
		boolInt(preferences.NotifyUpcoming), boolInt(preferences.NotifyStarted), boolInt(preferences.NotifyProblemRelease), boolInt(preferences.NotifyDeadline7Days), boolInt(preferences.NotifyDeadline1Day),
		boolInt(preferences.NotifyImportantUpdate), preferences.UserID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_categories WHERE user_id=?`, preferences.UserID); err != nil {
		return err
	}
	for _, category := range preferences.Categories {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_categories(user_id,category) VALUES(?,?)`, preferences.UserID, category); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_organizer_types WHERE user_id=?`, preferences.UserID); err != nil {
		return err
	}
	for _, organizerType := range preferences.OrganizerTypes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_organizer_types(user_id,organizer_type) VALUES(?,?)`, preferences.UserID, organizerType); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_competition_scopes WHERE user_id=?`, preferences.UserID); err != nil {
		return err
	}
	for _, scope := range preferences.CompetitionScopes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_competition_scopes(user_id,scope) VALUES(?,?)`, preferences.UserID, scope); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_regions WHERE user_id=?`, preferences.UserID); err != nil {
		return err
	}
	for _, region := range preferences.Regions {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_regions(user_id,region) VALUES(?,?)`, preferences.UserID, region); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM user_keywords WHERE user_id=?`, preferences.UserID); err != nil {
		return err
	}
	for _, value := range preferences.IncludeKeywords {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_keywords(user_id,kind,keyword) VALUES(?,'include',?)`, preferences.UserID, value); err != nil {
			return err
		}
	}
	for _, value := range preferences.ExcludeKeywords {
		if _, err := tx.ExecContext(ctx, `INSERT INTO user_keywords(user_id,kind,keyword) VALUES(?,'exclude',?)`, preferences.UserID, value); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET updated_at=? WHERE id=?`, now.Unix(), preferences.UserID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListNotificationUsers(ctx context.Context) ([]model.NotificationUser, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,email,verified_at,enabled,created_at,updated_at FROM users WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var users []model.User
	for rows.Next() {
		var user model.User
		var verifiedAt, createdAt, updatedAt int64
		var enabled int
		if err := rows.Scan(&user.ID, &user.Email, &verifiedAt, &enabled, &createdAt, &updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		user.VerifiedAt = time.Unix(verifiedAt, 0)
		user.Enabled = enabled == 1
		user.CreatedAt = time.Unix(createdAt, 0)
		user.UpdatedAt = time.Unix(updatedAt, 0)
		users = append(users, user)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]model.NotificationUser, 0, len(users))
	for _, user := range users {
		preferences, err := s.GetUserPreferences(ctx, user.ID)
		if err != nil {
			return nil, err
		}
		result = append(result, model.NotificationUser{User: user, Preferences: preferences})
	}
	return result, nil
}

func (s *Store) DisableUser(ctx context.Context, userID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET enabled=0,updated_at=? WHERE id=?`, now.Unix(), userID)
	return err
}

func (s *Store) UnrecordedCompetitionEvents(ctx context.Context, competitionID int64, events []model.Event) ([]model.Event, error) {
	fresh := make([]model.Event, 0, len(events))
	for _, event := range events {
		var exists int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM competition_events WHERE competition_id=? AND event_type=? AND event_key=?`,
			competitionID, event.Type, event.Key).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			fresh = append(fresh, event)
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return fresh, nil
}

// CommitCompetitionEvents atomically records new competition events and writes every
// corresponding notification outbox row. This prevents a process interruption from
// marking an event handled before its user notifications are durable.
func (s *Store) CommitCompetitionEvents(ctx context.Context, competitionID int64, events []model.Event, userDispatches []UserEventDispatch, now time.Time) error {
	if len(events) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	fresh := make(map[string]bool, len(events))
	for _, event := range events {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO competition_events(competition_id,event_type,event_key,created_at) VALUES(?,?,?,?)`,
			competitionID, event.Type, event.Key, now.Unix())
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 1 {
			fresh[event.Type+"\x00"+event.Key] = true
		}
	}
	if len(fresh) == 0 {
		return tx.Commit()
	}
	for _, dispatch := range userDispatches {
		if !fresh[dispatch.Event.Type+"\x00"+dispatch.Event.Key] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO user_notifications(
user_id,competition_id,event_type,event_key,delivery_group,status,last_error,due_at,created_at)
VALUES(?,?,?,?,?,'pending','',?,?)`, dispatch.UserID, competitionID, dispatch.Event.Type, dispatch.Event.Key,
			dispatch.GroupKey, dispatch.DueAt.Unix(), now.Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EnqueueUserCompetitionEvents backfills a user's personal outbox without
// recreating global competition events. The unique user event key keeps this
// safe to call after every preference change.
func (s *Store) EnqueueUserCompetitionEvents(ctx context.Context, competitionID int64, dispatches []UserEventDispatch, now time.Time) (int, error) {
	if competitionID < 1 || len(dispatches) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	inserted := 0
	for _, dispatch := range dispatches {
		if dispatch.UserID < 1 || dispatch.Event.Type == "" || dispatch.Event.Key == "" {
			continue
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO user_notifications(
user_id,competition_id,event_type,event_key,delivery_group,status,last_error,due_at,created_at)
VALUES(?,?,?,?,?,'pending','',?,?)`, dispatch.UserID, competitionID, dispatch.Event.Type, dispatch.Event.Key,
			dispatch.GroupKey, dispatch.DueAt.Unix(), now.Unix())
		if err != nil {
			return 0, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		inserted += int(affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return inserted, nil
}

func (s *Store) PendingUserGroups(ctx context.Context, now time.Time) ([]model.UserDeliveryGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT n.delivery_group,u.id,u.email,u.verified_at,u.enabled,u.created_at,u.updated_at
FROM user_notifications n JOIN users u ON u.id=n.user_id
WHERE n.status IN ('pending','failed') AND n.due_at<=? AND u.enabled=1
ORDER BY n.due_at,n.delivery_group`, now.Unix())
	if err != nil {
		return nil, err
	}
	var groups []model.UserDeliveryGroup
	for rows.Next() {
		var group model.UserDeliveryGroup
		var verifiedAt, createdAt, updatedAt int64
		var enabled int
		if err := rows.Scan(&group.GroupKey, &group.User.ID, &group.User.Email, &verifiedAt, &enabled, &createdAt, &updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		group.User.VerifiedAt = time.Unix(verifiedAt, 0)
		group.User.Enabled = enabled == 1
		group.User.CreatedAt = time.Unix(createdAt, 0)
		group.User.UpdatedAt = time.Unix(updatedAt, 0)
		groups = append(groups, group)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range groups {
		items, err := s.loadUserGroupItems(ctx, groups[index].GroupKey)
		if err != nil {
			return nil, err
		}
		groups[index].Items = items
	}
	return groups, nil
}

func (s *Store) MarkUserGroupSent(ctx context.Context, group string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_notifications SET status='sent',last_error='',sent_at=? WHERE delivery_group=?`, now.Unix(), group)
	return err
}

func (s *Store) MarkUserGroupFailed(ctx context.Context, group string, failure error, now time.Time) error {
	message := failure.Error()
	if len(message) > 1000 {
		message = message[:1000]
	}
	_, err := s.db.ExecContext(ctx, `UPDATE user_notifications SET status='failed',last_error=?,attempt_count=attempt_count+1,
due_at=? + MIN(86400,300 * (1 << MIN(attempt_count,8))) WHERE delivery_group=?`, message, now.Unix(), group)
	return err
}

func (s *Store) RescheduleUserPending(ctx context.Context, userID int64, dueAt time.Time, group string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE user_notifications SET due_at=?,delivery_group=? WHERE user_id=? AND status IN ('pending','failed')`,
		dueAt.Unix(), group, userID)
	return err
}

func (s *Store) ListUserPendingItems(ctx context.Context, userID int64) ([]model.UserNotificationItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT delivery_group FROM user_notifications
WHERE user_id=? AND status IN ('pending','failed') ORDER BY created_at`, userID)
	if err != nil {
		return nil, err
	}
	var groups []string
	for rows.Next() {
		var group string
		if err := rows.Scan(&group); err != nil {
			rows.Close()
			return nil, err
		}
		groups = append(groups, group)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var result []model.UserNotificationItem
	for _, group := range groups {
		items, err := s.loadUserGroupItems(ctx, group)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item.NotificationID != 0 {
				result = append(result, item)
			}
		}
	}
	return result, nil
}

func (s *Store) CancelUserNotifications(ctx context.Context, userID int64, notificationIDs []int64) error {
	if len(notificationIDs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, notificationID := range notificationIDs {
		if notificationID < 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE user_notifications SET status='cancelled',last_error='' WHERE id=? AND user_id=? AND status IN ('pending','failed')`, notificationID, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListActionableCompetitions(ctx context.Context, now time.Time, limit int) ([]model.Competition, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	rows, err := s.db.QueryContext(ctx, competitionSelect+`
WHERE status IN ('preview','upcoming','registration_open','ongoing')
  AND (competition_end IS NULL OR competition_end>=?)
  AND (status IN ('upcoming','ongoing') OR registration_end IS NULL OR registration_end>=?)
ORDER BY CASE status WHEN 'registration_open' THEN 0 WHEN 'upcoming' THEN 1 WHEN 'preview' THEN 2 ELSE 3 END,last_seen DESC LIMIT ?`, dayStart, dayStart, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.Competition
	for rows.Next() {
		competition, err := loadCompetition(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, competition)
	}
	return result, rows.Err()
}

// ListUnconfirmedCompetitions returns cataloged competitions whose lifecycle
// state has not been confirmed yet (composite status unknown). They feed a
// separate dashboard section as evidence of crawl progress and never produce
// notifications: the actionable pipeline only emits events for confirmed
// phases. Once a later scan confirms preview/open/upcoming/ongoing, the row
// moves into the actionable list through the normal change-event flow.
func (s *Store) ListUnconfirmedCompetitions(ctx context.Context, limit int) ([]model.Competition, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, competitionSelect+` WHERE status='unknown' ORDER BY last_seen DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.Competition
	for rows.Next() {
		competition, err := loadCompetition(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, competition)
	}
	return result, rows.Err()
}

func (s *Store) ListUserNotificationHistory(ctx context.Context, userID int64, limit int) ([]model.UserNotificationHistory, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `SELECT n.id,c.name,n.event_type,n.status,n.due_at,n.sent_at,n.last_error
FROM user_notifications n JOIN competitions c ON c.id=n.competition_id
WHERE n.user_id=? ORDER BY n.created_at DESC LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.UserNotificationHistory
	for rows.Next() {
		var item model.UserNotificationHistory
		var dueAt int64
		var sentAt sql.NullInt64
		if err := rows.Scan(&item.ID, &item.CompetitionName, &item.EventType, &item.Status, &dueAt, &sentAt, &item.LastError); err != nil {
			return nil, err
		}
		item.DueAt = time.Unix(dueAt, 0)
		if sentAt.Valid {
			value := time.Unix(sentAt.Int64, 0)
			item.SentAt = &value
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CountUserPendingNotifications(ctx context.Context, userID int64) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_notifications WHERE user_id=? AND status IN ('pending','failed')`, userID).Scan(&count)
	return count, err
}

func (s *Store) listUserStrings(ctx context.Context, query string, userID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (s *Store) loadUserGroupItems(ctx context.Context, group string) ([]model.UserNotificationItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id,competitions.id,competitions.entity_key,competitions.name,competitions.organizer,
competitions.status,competitions.status_evidence,competitions.registration_phase,competitions.competition_phase,competitions.registration_start,competitions.registration_start_raw,
competitions.registration_end,competitions.registration_end_raw,competitions.competition_start,competitions.competition_start_raw,competitions.competition_end,competitions.competition_end_raw,competitions.team_requirement,competitions.fee,competitions.fee_evidence,competitions.keywords_json,competitions.analysis_json,competitions.content,
competitions.fit_score,competitions.fit_reason,competitions.eligibility_note,competitions.official_url,competitions.trust,
competitions.facts_json,competitions.problem_released,competitions.analyzer_version,competitions.content_hash,competitions.first_seen,competitions.last_seen,n.event_type,n.event_key,COALESCE(choice.decision,'undecided')
FROM user_notifications n JOIN competitions ON competitions.id=n.competition_id
LEFT JOIN user_competition_choices choice ON choice.user_id=n.user_id AND choice.competition_id=n.competition_id
WHERE n.delivery_group=? AND n.status IN ('pending','failed') ORDER BY n.id`, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []model.UserNotificationItem
	for rows.Next() {
		var item model.UserNotificationItem
		var status, registrationPhase, competitionPhase, trust string
		var keywordsJSON, analysisJSON, factsJSON string
		var start, end, competitionStart, competitionEnd sql.NullInt64
		var decision string
		var problem int
		var firstSeen, lastSeen int64
		if err := rows.Scan(&item.NotificationID, &item.Competition.ID, &item.Competition.EntityKey, &item.Competition.Name, &item.Competition.Organizer,
			&status, &item.Competition.StatusEvidence, &registrationPhase, &competitionPhase, &start, &item.Competition.RegistrationStartRaw, &end, &item.Competition.RegistrationEndRaw,
			&competitionStart, &item.Competition.CompetitionStartRaw, &competitionEnd, &item.Competition.CompetitionEndRaw, &item.Competition.TeamRequirement, &item.Competition.Fee, &item.Competition.FeeEvidence, &keywordsJSON, &analysisJSON, &item.Competition.Content, &item.Competition.FitScore, &item.Competition.FitReason,
			&item.Competition.EligibilityNote, &item.Competition.OfficialURL, &trust, &factsJSON, &problem, &item.Competition.AnalyzerVersion, &item.Competition.ContentHash,
			&firstSeen, &lastSeen, &item.Event.Type, &item.Event.Key, &decision); err != nil {
			return nil, err
		}
		item.Competition.Status = model.Status(status)
		item.Competition.RegistrationPhase = model.RegistrationPhase(registrationPhase)
		item.Competition.CompetitionPhase = model.CompetitionPhase(competitionPhase)
		item.Competition.Trust = model.Trust(trust)
		item.Competition.ProblemReleased = problem == 1
		item.Decision = model.ParticipationDecision(decision)
		if err := json.Unmarshal([]byte(keywordsJSON), &item.Competition.Keywords); err != nil {
			return nil, fmt.Errorf("decode notification competition keywords: %w", err)
		}
		if err := json.Unmarshal([]byte(analysisJSON), &item.Competition.Analysis); err != nil {
			return nil, fmt.Errorf("decode notification competition analysis: %w", err)
		}
		if err := json.Unmarshal([]byte(factsJSON), &item.Competition.Facts); err != nil {
			return nil, fmt.Errorf("decode notification competition facts: %w", err)
		}
		item.Competition.FirstSeen = time.Unix(firstSeen, 0)
		item.Competition.LastSeen = time.Unix(lastSeen, 0)
		if start.Valid {
			value := time.Unix(start.Int64, 0)
			item.Competition.RegistrationStart = &value
		}
		if end.Valid {
			value := time.Unix(end.Int64, 0)
			item.Competition.RegistrationEnd = &value
		}
		if competitionStart.Valid {
			value := time.Unix(competitionStart.Int64, 0)
			item.Competition.CompetitionStart = &value
		}
		if competitionEnd.Valid {
			value := time.Unix(competitionEnd.Int64, 0)
			item.Competition.CompetitionEnd = &value
		}
		model.NormalizeLifecycle(&item.Competition)
		result = append(result, item)
	}
	return result, rows.Err()
}
