package service

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"competition-assistant/internal/model"
	"competition-assistant/internal/notifier"
	"competition-assistant/internal/subscription"
)

// StartUserDelivery moves one user's unsent notifications to a dedicated
// immediate group and delivers them in the background. Crawling remains a
// system-only scheduled operation.
func (s *Service) StartUserDelivery(ctx context.Context, userID int64) bool {
	if userID < 1 {
		return false
	}
	if !s.operationMu.TryLock() {
		return false
	}
	go func() {
		defer s.operationMu.Unlock()
		now := s.now().In(s.cfg.Location)
		group := fmt.Sprintf("manual:user:%d:%d", userID, now.UnixNano())
		if err := s.store.RescheduleUserPending(ctx, userID, now, group); err != nil {
			s.log.Error("immediate user notification scheduling failed", "user_id", userID, "error", err)
			return
		}
		if err := s.deliverUserPending(ctx, now); err != nil {
			s.log.Error("immediate user notification delivery failed", "user_id", userID, "error", err)
			return
		}
		s.log.Info("immediate user notifications delivered", "user_id", userID)
	}()
	return true
}

// DeliverDue delivers every user notification group whose scheduled time has
// passed. It is invoked independently of the crawl cycle.
func (s *Service) DeliverDue(ctx context.Context) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	now := s.now().In(s.cfg.Location)
	return s.deliverUserPending(ctx, now)
}

func (s *Service) deliverUserPending(ctx context.Context, now time.Time) error {
	groups, err := s.store.PendingUserGroups(ctx, now)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}
	sender, ok := s.notifier.(notifier.RecipientSender)
	if !ok || s.auth == nil || s.publicURL == "" {
		return errors.New("per-user notifications are queued but multi-user delivery is not configured")
	}
	var failures []error
	for _, group := range groups {
		preferences, err := s.store.GetUserPreferences(ctx, group.User.ID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		validItems := make([]model.UserNotificationItem, 0, len(group.Items))
		invalidIDs := make([]int64, 0)
		for _, item := range group.Items {
			matched := subscription.MatchingEventsForUser(preferences, item.Competition, subscription.Profile(item.Competition), []model.Event{item.Event}, item.Decision, now)
			if len(matched) == 0 {
				invalidIDs = append(invalidIDs, item.NotificationID)
				continue
			}
			validItems = append(validItems, item)
		}
		if len(invalidIDs) > 0 {
			if err := s.store.CancelUserNotifications(ctx, group.User.ID, invalidIDs); err != nil {
				failures = append(failures, err)
				continue
			}
		}
		if len(validItems) == 0 {
			continue
		}
		group.Items = validItems
		manageURL, unsubscribeURL := "", ""
		choiceLinks := make(map[int64]notifier.CompetitionChoiceLinks)
		if supportsExternalEmailLinks(s.publicURL) {
			manageURL = s.publicURL + "/preferences"
			unsubscribeToken := s.auth.UnsubscribeToken(group.User.ID, group.User.Email)
			unsubscribeURL = s.publicURL + "/unsubscribe?token=" + url.QueryEscape(unsubscribeToken)
			for _, item := range group.Items {
				if _, exists := choiceLinks[item.Competition.ID]; exists {
					continue
				}
				participateToken := s.auth.CompetitionChoiceToken(group.User.ID, item.Competition.ID, string(model.ParticipationParticipating))
				declineToken := s.auth.CompetitionChoiceToken(group.User.ID, item.Competition.ID, string(model.ParticipationDeclined))
				choiceLinks[item.Competition.ID] = notifier.CompetitionChoiceLinks{
					ParticipateURL: s.publicURL + "/competition-choice?token=" + url.QueryEscape(participateToken),
					DeclineURL:     s.publicURL + "/competition-choice?token=" + url.QueryEscape(declineToken),
				}
			}
		}
		subject, body, err := notifier.RenderUserDelivery(group, manageURL, unsubscribeURL, choiceLinks)
		if err == nil {
			err = sender.SendTo(ctx, group.User.Email, subject, body)
		}
		if err != nil {
			_ = s.store.MarkUserGroupFailed(ctx, group.GroupKey, err, now)
			failures = append(failures, err)
			continue
		}
		if err := s.store.MarkUserGroupSent(ctx, group.GroupKey, now); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func supportsExternalEmailLinks(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if address := net.ParseIP(host); address != nil {
		return !address.IsLoopback() && !address.IsPrivate() && !address.IsUnspecified() && !address.IsLinkLocalUnicast()
	}
	return true
}
