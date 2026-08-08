package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
	"competition-assistant/internal/store"
	"competition-assistant/internal/subscription"
)

// Run performs a full crawl-and-notify cycle: it discovers and analyses
// competition announcements, advances their lifecycle state, matches fresh
// events against every subscribed user and then delivers due notifications.
// Cleanup of stale data runs once per invocation regardless of scan outcome.
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
	successfulSources, err := s.scanConfiguredSources(ctx, now, bootstrapped, eventMap)
	if err != nil {
		return err
	}
	if successfulSources == 0 {
		return errors.New("all configured sources failed")
	}

	// Evidence Research planning is a standalone phase: it runs after source
	// scanning and before event commit, and is independent of processCandidate
	// and the observation "changed" decision. A canonical competition whose page
	// did not change this round can still be planned because it is missing a
	// date. It is read-only and applies cooldown + per-run budget scheduling: it
	// never searches, fetches, calls the LLM, records attempts, mutates canonical
	// facts, produces events or touches notifications. A planning failure must
	// not break the main scan, so it is only logged as a warning.
	if err := s.runEvidenceResearchPlanning(ctx, now); err != nil {
		s.log.Warn("evidence research planning failed", "error", err)
	}

	users, err := s.store.ListNotificationUsers(ctx)
	if err != nil {
		return err
	}
	if err := s.commitFreshEvents(ctx, now, users, eventMap); err != nil {
		return err
	}
	// Reconcile any events a previous scan may have lost by crashing after
	// upserting a competition but before committing its events. This must run on
	// every scan (including the bootstrap scan) so a first-scan interruption is
	// also recovered. It is idempotent: unrecorded events are the only ones added.
	if err := s.reconcileEvents(ctx, now, users); err != nil {
		return err
	}
	if !bootstrapped {
		if err := s.store.MarkBootstrapped(ctx); err != nil {
			return err
		}
	}
	if err := s.deliverUserPending(ctx, now); err != nil {
		return err
	}
	if alertErr := s.notifyUnhealthySources(ctx, now); alertErr != nil {
		// An alerting failure must not invalidate a successful scan.
		s.log.Error("source health alert failed", "error", alertErr)
	}
	s.log.Info("competition scan completed", "sources", successfulSources)
	return nil
}

// scanConfiguredSources discovers candidates from every configured source and
// feeds them through analysis and ingestion. A source that fails discovery is
// marked unhealthy and the scan continues; the number of sources that produced
// candidates is returned for the "all configured sources failed" gate.
func (s *Service) scanConfiguredSources(ctx context.Context, now time.Time, bootstrapped bool, eventMap map[int64][]model.Event) (int, error) {
	successfulSources := 0
	for _, source := range s.cfg.Sources {
		candidates, err := s.collector.Discover(ctx, source)
		if err != nil {
			s.log.Error("source discovery failed", "source", source.ID, "error", err)
			s.recordSourceHealth(ctx, source, false)
			continue
		}
		successfulSources++
		s.recordSourceHealth(ctx, source, true)
		if err := s.scanSource(ctx, source, candidates, now, bootstrapped, eventMap); err != nil {
			return 0, err
		}
	}
	return successfulSources, nil
}

// scanSource iterates the candidates a single source produced, fetching each
// relevant one and ingesting it. Fetches that fail are skipped; a fatal store
// or analysis error aborts the whole scan.
func (s *Service) scanSource(ctx context.Context, source config.Source, candidates []model.Candidate, now time.Time, bootstrapped bool, eventMap map[int64][]model.Event) error {
	for _, candidate := range candidates {
		if s.analyzer.CandidateScore(candidate.Title, candidate.Snippet) < 15 {
			continue
		}
		doc, err := s.collector.Fetch(ctx, candidate.URL)
		if err != nil {
			s.log.Warn("candidate fetch failed", "source", source.ID, "url", candidate.URL, "error", err)
			continue
		}
		if err := s.processCandidate(ctx, source, candidate, doc, now, bootstrapped, eventMap); err != nil {
			return err
		}
	}
	return nil
}

// processCandidate analyses a fetched document and ingests the resulting
// competition, collecting any lifecycle events. CandidateScore gating, trust,
// content hashing, the current-edition gate, research enrichment and the
// cross-entity-key upsert merge are all preserved verbatim.
func (s *Service) processCandidate(ctx context.Context, source config.Source, candidate model.Candidate, doc model.Document, now time.Time, bootstrapped bool, eventMap map[int64][]model.Event) error {
	trust := analyzer.TrustForURL(doc.URL, source, s.cfg)
	hash := contentHash(fmt.Sprintf("%s\n[published_at]%s\n[listing]%t", doc.Text, doc.PublishedAtRaw, doc.IsListing))
	changed, err := s.store.RecordObservationVersioned(ctx, source.ID, doc, hash, trust, s.analyzer.Version(), now)
	if err != nil {
		return err
	}
	if !changed || trust == model.TrustLow {
		return nil
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
			return nil
		}
	}
	if !relevant {
		return nil
	}
	// Fill FirstSeen from the page's earliest observation before the
	// timeliness gate. A brand-new competition analysed today may have been
	// first observed in a previous scan (when AI failed); without this, its
	// FirstSeen would be zero at the gate and the year-less/dateless case
	// would be wrongly rejected by the freshness fallback.
	if competition.FirstSeen.IsZero() {
		firstSeen, firstSeenErr := s.store.FirstObservedAt(ctx, doc.URL)
		if firstSeenErr != nil {
			return fmt.Errorf("read first observed time for %s: %w", doc.URL, firstSeenErr)
		}
		competition.FirstSeen = firstSeen
	}
	// Dual-dimension gate at ingest: a competition must satisfy both
	// trust and timeliness to be persisted. A first-time competition
	// from an explicitly past-year edition, or one whose page was
	// published long ago, is archived and must not enter the catalog
	// even though its source is trustworthy.
	if !isCurrentEdition(competition, now, s.freshnessWindow()) {
		s.log.Debug("skipping archived previous-year competition",
			"source", source.ID, "url", doc.URL, "name", competition.Name)
		return nil
	}
	// Only a successful analyzer result may claim the canonical competition has
	// been analysed by the current analyzer version. A deferred/pending attempt
	// (AI EOF, empty response, etc.) must not bump canonical AnalyzerVersion:
	// doing so would make resolveSourceConflicts treat it as an analyzer upgrade
	// and clear previously confirmed v6 lifecycle/date fields. The v7 attempt is
	// still recorded in the observation audit above, and RetryDocumentOnNextScan
	// keeps the page eligible for re-analysis on a later scan.
	if !pendingCandidate {
		competition.AnalyzerVersion = s.analyzer.Version()
	}
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
	if isNew && actionable(competition, now, s.freshnessWindow()) {
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
	events := changeEvents(old, competition, isNew, now, s.freshnessWindow())
	if !bootstrapped && isNew && !actionable(competition, now, s.freshnessWindow()) && !discoverableAnnouncement(competition, now, s.freshnessWindow()) {
		events = nil
	}
	if len(events) > 0 {
		eventMap[competition.ID] = append(eventMap[competition.ID], events...)
	}
	return nil
}

// commitFreshEvents turns each competition's collected events into fresh,
// unrecorded user dispatches and commits them transactionally.
func (s *Service) commitFreshEvents(ctx context.Context, now time.Time, users []model.NotificationUser, eventMap map[int64][]model.Event) error {
	for competitionID, events := range eventMap {
		competition, err := s.store.GetCompetitionByID(ctx, competitionID)
		if err != nil {
			return err
		}
		events = eventsForCurrentState(competition, deduplicateEvents(events))
		if err := s.commitEventsForUsers(ctx, now, users, competition, events); err != nil {
			return err
		}
	}
	return nil
}

// reconcileEvents recovers competition events that were lost when a previous
// scan was interrupted after upserting a competition but before committing its
// events. For every persisted competition it re-derives the events implied by
// the current canonical state, adds only those not already recorded (unique
// constraints make this idempotent), and writes matching user notifications in
// the same transaction. It never fabricates events for competitions that are
// ended, low-trust, stale or catalog-ineligible.
func (s *Service) reconcileEvents(ctx context.Context, now time.Time, users []model.NotificationUser) error {
	competitions, err := s.store.ListCompetitions(ctx)
	if err != nil {
		return err
	}
	freshness := s.freshnessWindow()
	for _, competition := range competitions {
		if !reconcileEligible(competition, now, freshness) {
			continue
		}
		events := eventsForCurrentState(competition, deduplicateEvents(backfillEvents(competition, now, s.cfg.Location, freshness)))
		if err := s.commitEventsForUsers(ctx, now, users, competition, events); err != nil {
			return err
		}
	}
	return nil
}

// reconcileEligible reports whether a persisted competition may have events
// missing and therefore qualifies for scan-end reconciliation. A competition is
// only reconciled if it is still catalog-eligible, not low-trust and not ended;
// otherwise a historical or unqualified competition would be given global
// events. Within that gate, either an actionable competition or a current
// discoverable announcement (which may still need competition_discovered) is
// reconciled.
func reconcileEligible(competition model.Competition, now time.Time, freshness time.Duration) bool {
	if !subscription.CatalogEligible(competition) ||
		competition.Trust == model.TrustLow ||
		competitionEnded(competition, now) {
		return false
	}
	return actionable(competition, now, freshness) || discoverableAnnouncement(competition, now, freshness)
}

// commitEventsForUsers records any unrecorded competition events and, for each
// currently matching user, writes the corresponding notification outbox rows in
// the same transaction. Events are derived from the competition's current state
// and filtered by the store's unique constraints, so repeated calls are
// idempotent.
func (s *Service) commitEventsForUsers(ctx context.Context, now time.Time, users []model.NotificationUser, competition model.Competition, events []model.Event) error {
	fresh, err := s.store.UnrecordedCompetitionEvents(ctx, competition.ID, events)
	if err != nil {
		return err
	}
	if len(fresh) == 0 {
		return nil
	}
	var userDispatches []store.UserEventDispatch
	competitionProfile := subscription.Profile(competition)
	for _, user := range users {
		decision, err := s.store.GetUserCompetitionDecision(ctx, user.User.ID, competition.ID)
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
		group := subscription.DeliveryGroupKey(user.User.ID, competition.ID, user.Preferences.Frequency, dueAt, nonce)
		for _, event := range matched {
			userDispatches = append(userDispatches, store.UserEventDispatch{UserID: user.User.ID, Event: event, GroupKey: group, DueAt: dueAt})
		}
	}
	if err := s.store.CommitCompetitionEvents(ctx, competition.ID, fresh, userDispatches, now); err != nil {
		return err
	}
	return nil
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

func changeEvents(old, current model.Competition, isNew bool, now time.Time, freshness time.Duration) []model.Event {
	model.NormalizeLifecycle(&old)
	model.NormalizeLifecycle(&current)
	if !actionable(current, now, freshness) && !(isNew && discoverableAnnouncement(current, now, freshness)) {
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

func backfillEvents(competition model.Competition, now time.Time, _ *time.Location, freshness time.Duration) []model.Event {
	model.NormalizeLifecycle(&competition)
	var events []model.Event
	if competition.RegistrationPhase == model.RegistrationUnknown && competition.CompetitionPhase == model.CompetitionUnknown && discoverableAnnouncement(competition, now, freshness) {
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

func actionable(competition model.Competition, now time.Time, freshness time.Duration) bool {
	model.NormalizeLifecycle(&competition)
	if competition.RegistrationPhase != model.RegistrationPreview && competition.RegistrationPhase != model.RegistrationOpen && competition.CompetitionPhase != model.CompetitionUpcoming && competition.CompetitionPhase != model.CompetitionOngoing {
		return false
	}
	// Registration deadlines only make an open-registration notice stale.
	// A competition may legitimately announce its upcoming or official start
	// after registration has already closed.
	if competition.RegistrationPhase == model.RegistrationOpen && competition.RegistrationEnd != nil && competition.RegistrationEnd.Before(model.DayStart(now)) {
		return false
	}
	if competitionEnded(competition, now) {
		return false
	}
	// An open-registration notice with no explicit deadline is the risky case:
	// a bare "报名中" on a page from a previous year would otherwise be treated
	// as current. Guard it with the freshness window so a genuinely current
	// competition still fires, while an old archived page does not.
	if competition.RegistrationPhase == model.RegistrationOpen && competition.RegistrationEnd == nil {
		return isCurrentEdition(competition, now, freshness)
	}
	// Other actionable states (preview / upcoming / ongoing) already carry
	// concrete timing information, so the lenient year rule is safe there.
	year := analyzer.YearFromText(competition.Name + " " + competition.StatusEvidence)
	return year == 0 || year >= now.Year()
}

// isCurrentEdition implements the timeliness half of the dual-dimension gate
// (trust + timeliness). It decides whether a competition represents a current
// edition rather than an archived page from a previous year. Signals are
// evaluated strongest-first:
//
//  1. An explicit year in the title or status is authoritative: only the
//     current or next year qualifies, so a page that names 2022-2025 is
//     rejected even when the system first crawled it today.
//  2. A registration or competition end date in the future proves the edition
//     is still live, even when the title carries no year (e.g. "第43次CSP").
//  3. The AI-extracted page publish date (published_at fact) rejects pages
//     published more than the freshness window ago as archived.
//  4. FirstSeen is the weakest signal: it only proves the page is new to the
//     system, so it is used only as a last resort for year-less, dateless pages.
func isCurrentEdition(competition model.Competition, now time.Time, freshness time.Duration) bool {
	year := analyzer.YearFromText(competition.Name + " " + competition.StatusEvidence)
	if year != 0 {
		return year >= now.Year() && year <= now.Year()+1
	}
	if competition.RegistrationEnd != nil && competition.RegistrationEnd.After(now) {
		return true
	}
	if competition.CompetitionEnd != nil && competition.CompetitionEnd.After(now) {
		return true
	}
	if published, ok := competition.Facts[model.FactPublishedAt]; ok {
		parsed, err := time.Parse("2006-01-02", published.Value)
		if err == nil {
			return now.Sub(parsed) <= freshness
		}
	}
	return !competition.FirstSeen.IsZero() && now.Sub(competition.FirstSeen) <= freshness
}

// discoverableAnnouncement permits one deliberately softer notification for
// a concrete current-edition competition whose registration state is not yet
// known. It does not relax the catalog, trust, expiry or user preference gates.
//
// A competition is considered a current announcement when either its title
// names the current or next year, or the page was first seen within the
// freshness window. The latter catches genuine new competitions whose title
// carries no year while still suppressing archived pages from previous years,
// which the system saw long ago.
func discoverableAnnouncement(competition model.Competition, now time.Time, freshness time.Duration) bool {
	model.NormalizeLifecycle(&competition)
	if !subscription.CatalogEligible(competition) || competition.Trust == model.TrustLow || competitionEnded(competition, now) {
		return false
	}
	if strings.TrimSpace(competition.OfficialURL) == "" {
		return false
	}
	// Timeliness is judged independently of trust: an explicitly past-year
	// edition is rejected even if the page was crawled today for the first
	// time, while a genuinely current edition (current/next year) or a
	// year-less page first seen recently may still be announced.
	return isCurrentEdition(competition, now, freshness)
}

func competitionEnded(competition model.Competition, now time.Time) bool {
	model.NormalizeLifecycle(&competition)
	if competition.CompetitionPhase == model.CompetitionFinished {
		return true
	}
	return competition.CompetitionEnd != nil && competition.CompetitionEnd.In(now.Location()).Before(model.DayStart(now))
}

func contentHash(text string) string {
	sum := sha256.Sum256([]byte(strings.Join(strings.Fields(text), " ")))
	return hex.EncodeToString(sum[:])
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
