package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/authn"
	"competition-assistant/internal/config"
	"competition-assistant/internal/fetcher"
	"competition-assistant/internal/model"
	"competition-assistant/internal/notifier"
	"competition-assistant/internal/store"
	"competition-assistant/internal/subscription"
)

type Service struct {
	cfg         config.Config
	store       *store.Store
	collector   fetcher.Collector
	analyzer    *analyzer.Analyzer
	notifier    notifier.Sender
	now         func() time.Time
	log         *slog.Logger
	auth        *authn.Manager
	publicURL   string
	operationMu sync.Mutex
}

var ErrCompetitionExpired = errors.New("competition has already ended")

func New(cfg config.Config, database *store.Store, collector fetcher.Collector, analysis *analyzer.Analyzer, sender notifier.Sender, logger *slog.Logger) *Service {
	return &Service{cfg: cfg, store: database, collector: collector, analyzer: analysis, notifier: sender, now: time.Now, log: logger}
}

func (s *Service) SetNow(now func() time.Time) { s.now = now }

func (s *Service) EnableMultiUser(publicURL string, manager *authn.Manager) {
	s.publicURL = strings.TrimRight(publicURL, "/")
	s.auth = manager
	if !supportsExternalEmailLinks(s.publicURL) {
		s.log.Warn("email action links disabled for local or private public URL", "public_url", s.publicURL)
	}
}

func (s *Service) Run(ctx context.Context) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	scanErr := s.run(ctx)
	cleanupErr := s.cleanup(ctx)
	return errors.Join(scanErr, cleanupErr)
}

func (s *Service) cleanup(ctx context.Context) error {
	if !s.cfg.Retention.Enabled {
		return nil
	}
	policy := store.RetentionPolicy{
		ObservationAge:              time.Duration(s.cfg.Retention.ObservationDays) * 24 * time.Hour,
		ClosedCompetitionContentAge: time.Duration(s.cfg.Retention.ClosedCompetitionContentDays) * 24 * time.Hour,
		ExpiredAuthenticationAge:    time.Duration(s.cfg.Retention.ExpiredAuthenticationDays) * 24 * time.Hour,
	}
	report, err := s.store.Cleanup(ctx, policy, s.now().In(s.cfg.Location))
	if err != nil {
		return fmt.Errorf("database cleanup: %w", err)
	}
	if report.Changed() {
		s.log.Info("database cleanup completed",
			"observations_deleted", report.ObservationsDeleted,
			"competitions_compacted", report.CompetitionsCompacted,
			"verification_codes_deleted", report.VerificationCodesDeleted,
			"sessions_deleted", report.SessionsDeleted,
		)
	}
	return nil
}

// PurgeIneligibleCompetitions removes legacy rows that would be rejected by
// the current final catalog gate. Foreign keys cascade to their events,
// pending notifications, source links and per-user choices.
func (s *Service) PurgeIneligibleCompetitions(ctx context.Context) (int, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	competitions, err := s.store.ListCompetitions(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, competition := range competitions {
		if subscription.CatalogEligible(competition) {
			continue
		}
		if err := s.store.DeleteCompetition(ctx, competition.ID); err != nil {
			return removed, err
		}
		removed++
	}
	if removed > 0 {
		s.log.Info("ineligible competition data purged", "competitions", removed)
	}
	return removed, nil
}

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

// BackfillUser queues the current actionable state of competitions that match
// the user's latest preferences. It performs no crawling and is idempotent.
func (s *Service) BackfillUser(ctx context.Context, userID int64) (int, error) {
	if userID < 1 {
		return 0, errors.New("invalid user id")
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	now := s.now().In(s.cfg.Location)
	preferences, err := s.store.GetUserPreferences(ctx, userID)
	if err != nil {
		return 0, err
	}
	competitions, err := s.store.ListActiveCompetitions(ctx)
	if err != nil {
		return 0, err
	}
	dueAt, err := subscription.NextDelivery(now, preferences)
	if err != nil {
		return 0, err
	}
	inserted := 0
	for _, competition := range competitions {
		if !actionable(competition, now) {
			continue
		}
		keywords := s.analyzer.KeywordsForCompetition(competition)
		if !sameStrings(competition.Keywords, keywords) {
			competition.Keywords = keywords
			if err := s.store.UpdateCompetitionEnrichment(ctx, competition.ID, keywords, competition.Analysis); err != nil {
				return inserted, err
			}
		}
		events := backfillEvents(competition, now, s.cfg.Location)
		decision, err := s.store.GetUserCompetitionDecision(ctx, userID, competition.ID)
		if err != nil {
			return inserted, err
		}
		matched := subscription.MatchingEventsForUser(preferences, competition, subscription.Profile(competition), events, decision, now)
		if len(matched) == 0 {
			continue
		}
		nonce := "backfill:" + matched[0].Type + ":" + matched[0].Key
		group := subscription.DeliveryGroupKey(userID, competition.ID, preferences.Frequency, dueAt, nonce)
		dispatches := make([]store.UserEventDispatch, 0, len(matched))
		for _, event := range matched {
			dispatches = append(dispatches, store.UserEventDispatch{UserID: userID, Event: event, GroupKey: group, DueAt: dueAt})
		}
		count, err := s.store.EnqueueUserCompetitionEvents(ctx, competition.ID, dispatches, now)
		if err != nil {
			return inserted, err
		}
		inserted += count
	}
	return inserted, nil
}

func (s *Service) SetUserCompetitionDecision(ctx context.Context, userID, competitionID int64, decision model.ParticipationDecision) error {
	if userID < 1 || competitionID < 1 {
		return errors.New("invalid user or competition id")
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	now := s.now().In(s.cfg.Location)
	competition, err := s.store.GetCompetitionByID(ctx, competitionID)
	if err != nil {
		return err
	}
	if decision == model.ParticipationParticipating && competitionEnded(competition, now) {
		return ErrCompetitionExpired
	}
	if err := s.store.SetUserCompetitionDecision(ctx, userID, competitionID, decision, now); err != nil {
		return err
	}
	model.NormalizeLifecycle(&competition)
	if decision != model.ParticipationParticipating {
		return nil
	}
	preferences, err := s.store.GetUserPreferences(ctx, userID)
	if err != nil {
		return err
	}
	var currentEvents []model.Event
	if competition.CompetitionPhase == model.CompetitionUpcoming {
		currentEvents = append(currentEvents, model.Event{Type: "competition_upcoming", Key: "upcoming"})
	}
	if competition.CompetitionPhase == model.CompetitionOngoing {
		currentEvents = append(currentEvents, model.Event{Type: "competition_started", Key: "started"})
	}
	if competition.ProblemReleased {
		currentEvents = append(currentEvents, model.Event{Type: "problem_released", Key: "problem_released"})
	}
	matched := subscription.MatchingEventsForUser(preferences, competition, subscription.Profile(competition), currentEvents, decision, now)
	if len(matched) == 0 {
		return nil
	}
	dueAt, err := subscription.NextDelivery(now, preferences)
	if err != nil {
		return err
	}
	group := subscription.DeliveryGroupKey(userID, competitionID, preferences.Frequency, dueAt, "choice:"+matched[0].Type+":"+matched[0].Key)
	dispatches := make([]store.UserEventDispatch, 0, len(matched))
	for _, event := range matched {
		dispatches = append(dispatches, store.UserEventDispatch{UserID: userID, Event: event, GroupKey: group, DueAt: dueAt})
	}
	_, err = s.store.EnqueueUserCompetitionEvents(ctx, competitionID, dispatches, now)
	return err
}

func (s *Service) run(ctx context.Context) error {
	now := s.now().In(s.cfg.Location)
	s.log.Info("competition scan started", "time", now.Format(time.RFC3339))
	if err := s.deliverUserPending(ctx, now); err != nil {
		s.log.Warn("previous notifications remain pending", "error", err)
	}
	bootstrapped, err := s.store.IsBootstrapped(ctx)
	if err != nil {
		return err
	}
	eventMap := map[int64][]model.Event{}
	successfulSources := 0
	for _, source := range s.cfg.Sources {
		candidates, err := s.collector.Discover(ctx, source)
		if err != nil {
			s.log.Error("source discovery failed", "source", source.ID, "error", err)
			continue
		}
		successfulSources++
		for _, candidate := range candidates {
			if s.analyzer.CandidateScore(candidate.Title, candidate.Snippet) < 15 {
				continue
			}
			doc, err := s.collector.Fetch(ctx, candidate.URL)
			if err != nil {
				s.log.Warn("candidate fetch failed", "source", source.ID, "url", candidate.URL, "error", err)
				continue
			}
			trust := analyzer.TrustForURL(doc.URL, source, s.cfg)
			hash := contentHash(fmt.Sprintf("%s\n[published_at]%s\n[listing]%t", doc.Text, doc.PublishedAtRaw, doc.IsListing))
			changed, err := s.store.RecordObservationVersioned(ctx, source.ID, doc, hash, trust, s.analyzer.Version(), now)
			if err != nil {
				return err
			}
			if !changed || trust == model.TrustLow {
				continue
			}
			competition, relevant, err := s.analyzer.Analyze(ctx, candidate, doc, trust, now)
			pendingCandidate := relevant && analyzer.IsPendingCandidateError(err)
			audit := competition.ExtractionAudit
			if err != nil && audit.AnalyzerVersion == "" {
				audit = model.AnalysisAudit{AnalyzerVersion: s.analyzer.Version(), InputHash: hash, Error: err.Error(), AnalyzedAt: now}
			}
			if audit.AnalyzerVersion != "" {
				if auditErr := s.store.RecordAnalysisAudit(ctx, source.ID, doc.URL, hash, audit); auditErr != nil {
					return fmt.Errorf("record analysis audit: %w", auditErr)
				}
			}
			if err != nil {
				if retryErr := s.store.RetryDocumentOnNextScan(ctx, source.ID, doc.URL); retryErr != nil {
					s.log.Warn("candidate retry baseline reset failed", "source", source.ID, "url", doc.URL, "error", retryErr)
				}
				s.log.Warn("candidate analysis deferred", "url", doc.URL, "error", err)
				if !pendingCandidate {
					continue
				}
			}
			if !relevant {
				continue
			}
			competition.AnalyzerVersion = s.analyzer.Version()
			competition.ContentHash = hash
			old, isNew, err := s.store.UpsertCompetition(ctx, competition, source.ID, now)
			if err != nil {
				return err
			}
			var saved model.Competition
			if isNew {
				saved, err = s.store.GetCompetition(ctx, competition.EntityKey)
			} else {
				// A changed announcement title can be merged into an existing
				// competition whose canonical entity key is different.
				saved, err = s.store.GetCompetitionByID(ctx, old.ID)
			}
			if err != nil {
				return err
			}
			competition = saved
			if isNew && actionable(competition, now) {
				secondary := s.collectResearchSources(ctx, competition)
				analysis, keywords, analysisErr := s.analyzer.AnalyzeResearch(ctx, competition, doc, secondary, now)
				if analysisErr != nil {
					s.log.Warn("competition qualitative analysis fell back to official content", "competition_id", competition.ID, "error", analysisErr)
				}
				if err := s.store.UpdateCompetitionEnrichment(ctx, competition.ID, keywords, analysis); err != nil {
					return err
				}
				competition, err = s.store.GetCompetitionByID(ctx, competition.ID)
				if err != nil {
					return err
				}
			}
			events := changeEvents(old, competition, isNew, now)
			if !bootstrapped && isNew && !actionable(competition, now) && !discoverableAnnouncement(competition, now) {
				events = nil
			}
			if len(events) > 0 {
				eventMap[competition.ID] = append(eventMap[competition.ID], events...)
			}
		}
	}
	if successfulSources == 0 {
		return errors.New("all configured sources failed")
	}

	users, err := s.store.ListNotificationUsers(ctx)
	if err != nil {
		return err
	}
	for competitionID, events := range eventMap {
		competition, err := s.store.GetCompetitionByID(ctx, competitionID)
		if err != nil {
			return err
		}
		events = eventsForCurrentState(competition, deduplicateEvents(events))
		fresh, err := s.store.UnrecordedCompetitionEvents(ctx, competitionID, events)
		if err != nil {
			return err
		}
		if len(fresh) == 0 {
			continue
		}
		var userDispatches []store.UserEventDispatch
		competitionProfile := subscription.Profile(competition)
		for _, user := range users {
			decision, err := s.store.GetUserCompetitionDecision(ctx, user.User.ID, competitionID)
			if err != nil {
				return err
			}
			matched := subscription.MatchingEventsForUser(user.Preferences, competition, competitionProfile, fresh, decision, now)
			if len(matched) == 0 {
				continue
			}
			dueAt, err := subscription.NextDelivery(now, user.Preferences)
			if err != nil {
				s.log.Warn("invalid user delivery preference", "user_id", user.User.ID, "error", err)
				continue
			}
			nonce := matched[0].Type + ":" + matched[0].Key
			group := subscription.DeliveryGroupKey(user.User.ID, competitionID, user.Preferences.Frequency, dueAt, nonce)
			for _, event := range matched {
				userDispatches = append(userDispatches, store.UserEventDispatch{UserID: user.User.ID, Event: event, GroupKey: group, DueAt: dueAt})
			}
		}
		if err := s.store.CommitCompetitionEvents(ctx, competitionID, fresh, userDispatches, now); err != nil {
			return err
		}
	}
	if !bootstrapped {
		if err := s.store.MarkBootstrapped(ctx); err != nil {
			return err
		}
	}
	if err := s.deliverUserPending(ctx, now); err != nil {
		return err
	}
	s.log.Info("competition scan completed", "sources", successfulSources)
	return nil
}

// eventsForCurrentState prevents an earlier source observed in the same scan
// from producing a stale lifecycle notice after a later, more complete source
// has already advanced the canonical competition row.
func eventsForCurrentState(competition model.Competition, events []model.Event) []model.Event {
	model.NormalizeLifecycle(&competition)
	result := make([]model.Event, 0, len(events))
	for _, event := range events {
		switch event.Type {
		case "competition_discovered":
			if competition.RegistrationPhase == model.RegistrationUnknown && competition.CompetitionPhase == model.CompetitionUnknown {
				result = append(result, event)
			}
		case "preview_detected":
			if competition.RegistrationPhase == model.RegistrationPreview {
				result = append(result, event)
			}
		case "registration_opened":
			if competition.RegistrationPhase == model.RegistrationOpen {
				result = append(result, event)
			}
		case "competition_upcoming":
			if competition.CompetitionPhase == model.CompetitionUpcoming {
				result = append(result, event)
			}
		case "competition_started":
			if competition.CompetitionPhase == model.CompetitionOngoing {
				result = append(result, event)
			}
		case "problem_released":
			if competition.ProblemReleased {
				result = append(result, event)
			}
		}
	}
	return result
}

func (s *Service) collectResearchSources(ctx context.Context, competition model.Competition) []model.ResearchSource {
	if !s.cfg.Enrichment.Enabled || !s.analyzer.ResearchEnabled() || s.cfg.Enrichment.MaxSources < 1 {
		return nil
	}
	researchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	query := fmt.Sprintf(`"%s" (参赛体验 OR 比赛难度 OR 参赛经验 OR 赛题分析 OR 项目实践)`, competition.Name)
	source := config.Source{
		ID:             "qualitative-research",
		Name:           "赛事社区资料",
		Kind:           "search",
		Query:          query,
		Limit:          s.cfg.Enrichment.MaxSources,
		AllowedDomains: s.cfg.Enrichment.AllowedDomains,
	}
	candidates, err := s.collector.Discover(researchCtx, source)
	if err != nil {
		s.log.Warn("competition research search failed", "competition_id", competition.ID, "error", err)
		return nil
	}
	result := make([]model.ResearchSource, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.URL == competition.OfficialURL || !sameResearchEdition(competition.Name, candidate.Title+" "+candidate.Snippet) {
			continue
		}
		text := candidate.Snippet
		if document, fetchErr := s.collector.Fetch(researchCtx, candidate.URL); fetchErr == nil && strings.TrimSpace(document.Text) != "" {
			text = document.Text
		}
		if runes := []rune(text); len(runes) > 5000 {
			text = string(runes[:5000])
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		result = append(result, model.ResearchSource{Title: candidate.Title, URL: candidate.URL, Text: text, Kind: "community"})
	}
	return result
}

func sameResearchEdition(competitionName, researchText string) bool {
	competitionYear := analyzer.YearFromText(competitionName)
	researchYear := analyzer.YearFromText(researchText)
	return competitionYear == 0 || researchYear == 0 || competitionYear == researchYear
}

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

func changeEvents(old, current model.Competition, isNew bool, now time.Time) []model.Event {
	model.NormalizeLifecycle(&old)
	model.NormalizeLifecycle(&current)
	if !actionable(current, now) && !(isNew && discoverableAnnouncement(current, now)) {
		return nil
	}
	var events []model.Event
	if isNew {
		if current.RegistrationPhase == model.RegistrationUnknown && current.CompetitionPhase == model.CompetitionUnknown {
			events = append(events, model.Event{Type: "competition_discovered", Key: "discovered"})
		}
		switch current.RegistrationPhase {
		case model.RegistrationPreview:
			events = append(events, model.Event{Type: "preview_detected", Key: "preview"})
		case model.RegistrationOpen:
			events = append(events, model.Event{Type: "registration_opened", Key: "registration_open"})
		}
		switch current.CompetitionPhase {
		case model.CompetitionUpcoming:
			events = append(events, model.Event{Type: "competition_upcoming", Key: "upcoming"})
		case model.CompetitionOngoing:
			events = append(events, model.Event{Type: "competition_started", Key: "started"})
		}
		if current.ProblemReleased {
			events = append(events, model.Event{Type: "problem_released", Key: "problem_released"})
		}
		return events
	}
	if old.RegistrationPhase != current.RegistrationPhase && current.RegistrationPhase == model.RegistrationPreview {
		events = append(events, model.Event{Type: "preview_detected", Key: "preview"})
	}
	if old.RegistrationPhase != current.RegistrationPhase && current.RegistrationPhase == model.RegistrationOpen {
		events = append(events, model.Event{Type: "registration_opened", Key: "registration_open"})
	}
	if old.CompetitionPhase != current.CompetitionPhase && current.CompetitionPhase == model.CompetitionUpcoming {
		events = append(events, model.Event{Type: "competition_upcoming", Key: "upcoming"})
	}
	if old.CompetitionPhase != current.CompetitionPhase && current.CompetitionPhase == model.CompetitionOngoing {
		events = append(events, model.Event{Type: "competition_started", Key: "started"})
	}
	if !old.ProblemReleased && current.ProblemReleased {
		events = append(events, model.Event{Type: "problem_released", Key: "problem_released"})
	}
	return events
}

func backfillEvents(competition model.Competition, now time.Time, _ *time.Location) []model.Event {
	model.NormalizeLifecycle(&competition)
	var events []model.Event
	if competition.RegistrationPhase == model.RegistrationUnknown && competition.CompetitionPhase == model.CompetitionUnknown && discoverableAnnouncement(competition, now) {
		events = append(events, model.Event{Type: "competition_discovered", Key: "discovered"})
	}
	switch competition.RegistrationPhase {
	case model.RegistrationPreview:
		events = append(events, model.Event{Type: "preview_detected", Key: "preview"})
	case model.RegistrationOpen:
		events = append(events, model.Event{Type: "registration_opened", Key: "registration_open"})
	}
	switch competition.CompetitionPhase {
	case model.CompetitionUpcoming:
		events = append(events, model.Event{Type: "competition_upcoming", Key: "upcoming"})
	case model.CompetitionOngoing:
		events = append(events, model.Event{Type: "competition_started", Key: "started"})
	}
	if competition.ProblemReleased {
		events = append(events, model.Event{Type: "problem_released", Key: "problem_released"})
	}
	return deduplicateEvents(events)
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func actionable(competition model.Competition, now time.Time) bool {
	model.NormalizeLifecycle(&competition)
	if competition.RegistrationPhase != model.RegistrationPreview && competition.RegistrationPhase != model.RegistrationOpen && competition.CompetitionPhase != model.CompetitionUpcoming && competition.CompetitionPhase != model.CompetitionOngoing {
		return false
	}
	// Registration deadlines only make an open-registration notice stale.
	// A competition may legitimately announce its upcoming or official start
	// after registration has already closed.
	if competition.RegistrationPhase == model.RegistrationOpen && competition.RegistrationEnd != nil && competition.RegistrationEnd.Before(dayStart(now)) {
		return false
	}
	if competitionEnded(competition, now) {
		return false
	}
	year := analyzer.YearFromText(competition.Name + " " + competition.StatusEvidence)
	return year == 0 || year >= now.Year()
}

// discoverableAnnouncement permits one deliberately softer notification for
// a concrete current-edition competition whose registration state is not yet
// known. It does not relax the catalog, trust, expiry or user preference gates.
func discoverableAnnouncement(competition model.Competition, now time.Time) bool {
	model.NormalizeLifecycle(&competition)
	if !subscription.CatalogEligible(competition) || competition.Trust == model.TrustLow || competitionEnded(competition, now) {
		return false
	}
	year := analyzer.YearFromText(competition.Name)
	return year >= now.Year() && year <= now.Year()+1 && strings.TrimSpace(competition.OfficialURL) != ""
}

func competitionEnded(competition model.Competition, now time.Time) bool {
	model.NormalizeLifecycle(&competition)
	if competition.CompetitionPhase == model.CompetitionFinished {
		return true
	}
	return competition.CompetitionEnd != nil && competition.CompetitionEnd.In(now.Location()).Before(dayStart(now))
}

func contentHash(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:])
}

func dayStart(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, value.Location())
}

func deduplicateEvents(events []model.Event) []model.Event {
	seen := map[string]bool{}
	result := make([]model.Event, 0, len(events))
	for _, event := range events {
		key := event.Type + "|" + event.Key
		if !seen[key] {
			seen[key] = true
			result = append(result, event)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Type < result[j].Type })
	return result
}

func (s *Service) String() string {
	return fmt.Sprintf("competition service with %d sources", len(s.cfg.Sources))
}
