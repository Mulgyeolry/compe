package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
	"competition-assistant/internal/store"
)

// Evidence Research Phase 6: a deterministic reconciler that turns executor
// candidate facts into narrow canonical supplements + ResearchState outcomes.
// found ≠ resolved: a fact only becomes resolved once the canonical has actually
// been supplemented (or already holds the same value).

// evidenceResearchReconcileOutcome is the internal per-field reconcile result.
type evidenceResearchReconcileOutcome string

const (
	evidenceResearchAccepted       evidenceResearchReconcileOutcome = "accepted"
	evidenceResearchAlreadyPresent evidenceResearchReconcileOutcome = "already_present"
	evidenceResearchRejected       evidenceResearchReconcileOutcome = "rejected"
	evidenceResearchConflict       evidenceResearchReconcileOutcome = "conflict"
)

// researchReconcileFieldResult is the reconciler's decision for one field.
type researchReconcileFieldResult struct {
	Field             model.EvidenceField
	Outcome           evidenceResearchReconcileOutcome
	ResearchState     model.ResearchStateStatus
	NextRetryAt       *time.Time
	LastError         string
	CanonicalChanged  bool
	SavedCompetition  *model.Competition
}

// sameResearchAuthority reports whether a research fact source URL belongs to
// the same official authority as the canonical OfficialURL. It is a strict
// exact-host / real-subdomain check: example.com, www.example.com and
// a.example.com are the same authority, while example.com, evil-example.com and
// example.com.evil.net are not. This is NOT an SSRF check.
func sameResearchAuthority(sourceURL, officialURL string) bool {
	sourceHost := researchAuthorityHost(sourceURL)
	officialHost := researchAuthorityHost(officialURL)
	if sourceHost == "" || officialHost == "" {
		return false
	}
	return sourceHost == officialHost || strings.HasSuffix(sourceHost, "."+officialHost) || strings.HasSuffix(officialHost, "."+sourceHost)
}

// researchAuthorityHost lower-cases a URL hostname, strips an optional leading
// "www.", and returns "" for non-http(s) or malformed URLs.
func researchAuthorityHost(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	host = strings.TrimPrefix(host, "www.")
	if host == "" || host == "localhost" {
		return ""
	}
	return host
}

// researchLifecycleAfterSupplement applies conservative, Research-only lifecycle
// inference to a canonical copy after a research date is added. It only fills
// Unknown phases and never regresses an existing non-Unknown phase. It mutates
// the passed copy and returns it (plus the new StatusEvidence if a phase changed).
//
// All calendar-day comparisons use the business/config location (the run's
// `now.Location()`), never the UTC instant day. A competition is only closed /
// finished when its deadline is strictly before today: on the deadline day
// itself it is still open / ongoing (start <= today <= end).
func researchLifecycleAfterSupplement(competition model.Competition, now time.Time) (model.Competition, bool) {
	loc := now.Location()
	today := calendarDayIn(now, loc)
	changed := false

	// Registration phase: only when unknown.
	if competition.RegistrationPhase == model.RegistrationUnknown {
		if competition.RegistrationEnd != nil && calendarDayIn(*competition.RegistrationEnd, loc).Before(today) {
			competition.RegistrationPhase = model.RegistrationClosed
			changed = true
		} else if competition.RegistrationStart != nil && calendarDayIn(*competition.RegistrationStart, loc).After(today) {
			competition.RegistrationPhase = model.RegistrationPreview
			changed = true
		} else if competition.RegistrationStart != nil && competition.RegistrationEnd != nil &&
			!calendarDayIn(*competition.RegistrationStart, loc).After(today) && !calendarDayIn(*competition.RegistrationEnd, loc).Before(today) {
			competition.RegistrationPhase = model.RegistrationOpen
			changed = true
		}
		// past start only (no end), or future end only (no start): stay unknown.
	}

	// Competition phase: only when unknown.
	if competition.CompetitionPhase == model.CompetitionUnknown {
		if competition.CompetitionEnd != nil && calendarDayIn(*competition.CompetitionEnd, loc).Before(today) {
			competition.CompetitionPhase = model.CompetitionFinished
			changed = true
		} else if competition.CompetitionStart != nil && calendarDayIn(*competition.CompetitionStart, loc).After(today) {
			competition.CompetitionPhase = model.CompetitionUpcoming
			changed = true
		} else if competition.CompetitionStart != nil && competition.CompetitionEnd != nil &&
			!calendarDayIn(*competition.CompetitionStart, loc).After(today) && !calendarDayIn(*competition.CompetitionEnd, loc).Before(today) {
			competition.CompetitionPhase = model.CompetitionOngoing
			changed = true
		}
	}

	competition.Status = model.CompositeStatus(competition.RegistrationPhase, competition.CompetitionPhase)
	return competition, changed
}

// researchFactConfidence derives the canonical FactEvidence confidence from the
// canonical Trust, never from the model's self-reported confidence.
func researchFactConfidence(trust model.Trust) string {
	switch trust {
	case model.TrustHigh:
		return "high"
	case model.TrustMedium:
		return "medium"
	default:
		return ""
	}
}

// reconcileEvidenceResearchField reconciles one executor field result into a
// narrow canonical supplement and a ResearchState outcome. It returns the
// reconcile decision; the caller records the ResearchState and events.
func (s *Service) reconcileEvidenceResearchField(
	ctx context.Context,
	competition model.Competition,
	execution evidenceResearchExecution,
	fieldResult evidenceResearchFieldResult,
	now time.Time,
	cfg config.EvidenceResearch,
) researchReconcileFieldResult {
	reject := func(reason string) researchReconcileFieldResult {
		retry := researchNextRetryAt(model.ResearchStateUnresolved, now, cfg)
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchRejected,
			ResearchState: model.ResearchStateUnresolved,
			NextRetryAt:   retry,
			LastError:     reason,
		}
	}

	// Non-found outcomes map directly to unresolved / retryable state.
	switch fieldResult.Outcome {
	case evidenceResearchRetryable:
		retry := researchNextRetryAt(model.ResearchStateRetryable, now, cfg)
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchRejected,
			ResearchState: model.ResearchStateRetryable,
			NextRetryAt:   retry,
			LastError:     firstNonEmpty(fieldResult.LastError, "operational research error"),
		}
	case evidenceResearchUnresolved:
		retry := researchNextRetryAt(model.ResearchStateUnresolved, now, cfg)
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchRejected,
			ResearchState: model.ResearchStateUnresolved,
			NextRetryAt:   retry,
			LastError:     firstNonEmpty(fieldResult.LastError, "no acceptable evidence found"),
		}
	}

	// found path.
	if fieldResult.Fact == nil {
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchRejected,
			ResearchState: model.ResearchStateUnresolved,
			NextRetryAt:   researchNextRetryAt(model.ResearchStateUnresolved, now, cfg),
			LastError:     "found field has no fact (invariant)",
		}
	}
	fact := *fieldResult.Fact
	// Business/config location: research fact dates carry the correct semantic
	// calendar day in this location; never reinterpret them as a UTC day.
	loc := now.Location()

	// Structural invariant check.
	if fact.Field != fieldResult.Field || fact.Date.IsZero() || !model.ValidEvidenceField(fact.Field) {
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchConflict,
			ResearchState: model.ResearchStateUnresolved,
			NextRetryAt:   researchNextRetryAt(model.ResearchStateUnresolved, now, cfg),
			LastError:     "research fact field mismatch or invalid (invariant)",
		}
	}

	// Reload the CURRENT canonical. Final admission decisions (edition, authority,
	// trust, target population) must be based on the reloaded canonical, not the
	// planning-time copy that may be stale.
	current, err := s.store.GetCompetitionByID(ctx, competition.ID)
	if err != nil {
		retry := researchNextRetryAt(model.ResearchStateRetryable, now, cfg)
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchRejected,
			ResearchState: model.ResearchStateRetryable,
			NextRetryAt:   retry,
			LastError:     firstNonEmpty(err.Error(), "reload canonical failed"),
		}
	}

	// Derive the current canonical edition from the reloaded current.
	canonicalEdition, edErr := evidenceResearchEdition(current)
	if edErr != nil || execution.Edition == "" || fact.Edition == "" ||
		fmt.Sprintf("%d", fact.Date.Year()) != fact.Edition ||
		execution.Edition != fact.Edition ||
		canonicalEdition != fact.Edition {
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchRejected,
			ResearchState: model.ResearchStateUnresolved,
			NextRetryAt:   researchNextRetryAt(model.ResearchStateUnresolved, now, cfg),
			LastError:     "research fact edition does not match canonical edition",
		}
	}

	// Authority gate against the reloaded current.OfficialURL: cross-domain is
	// never written.
	if !sameResearchAuthority(fact.SourceURL, current.OfficialURL) {
		return reject("research source is outside canonical official authority")
	}

	// TrustLow hard-reject: the reconciler is the final canonical safety boundary.
	// Even if the planner filtered low-trust canonicals, we must not write a
	// canonical for one that became low-trust. This is an unresolved (long
	// cooldown) outcome, never a retryable.
	if current.Trust == model.TrustLow {
		return reject("canonical trust is low")
	}

	// Target already populated? Same date → already_present/resolved, no write.
	// Different date → conflict/skipped, never overwrite.
	if !researchFieldNil(current, fieldResult.Field) {
		existing := researchFieldDate(current, fieldResult.Field)
		if existing != nil && researchSameCalendarDate(*existing, fact.Date, loc) {
			return researchReconcileFieldResult{
				Field:            fieldResult.Field,
				Outcome:          evidenceResearchAlreadyPresent,
				ResearchState:    model.ResearchStateResolved,
				SavedCompetition: &current,
			}
		}
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchConflict,
			ResearchState: model.ResearchStateSkipped,
			LastError:     "canonical field already populated with different value",
		}
	}

	// Build the supplement. The stored date is the calendar day of the research
	// fact IN THE BUSINESS LOCATION, preserving its original semantic day.
	supplementDate := researchCalendarDay(fact.Date, loc)
	supplement := store.EvidenceResearchSupplement{
		Field: fieldResult.Field,
		Date:  supplementDate,
		Raw:   strings.TrimSpace(fact.Raw),
		Fact: model.FactEvidence{
			Value:      strings.TrimSpace(fact.Raw),
			Raw:        strings.TrimSpace(fact.Raw),
			Evidence:   fact.Evidence,
			Edition:    fact.Edition,
			SourceURL:  fact.SourceURL,
			Confidence: researchFactConfidence(current.Trust),
			ObservedAt: now,
		},
	}
	next := current
	applyResearchDate(&next, fieldResult.Field, supplementDate, strings.TrimSpace(fact.Raw))
	lifecycle, phaseChanged := researchLifecycleAfterSupplement(next, now)
	supplement.RegistrationPhase = lifecycle.RegistrationPhase
	supplement.CompetitionPhase = lifecycle.CompetitionPhase
	// Preserve the existing StatusEvidence when the phase is unchanged; only when
	// an Unknown phase moves to a concrete one do we set it from the research fact.
	supplement.StatusEvidence = current.StatusEvidence
	if phaseChanged {
		supplement.StatusEvidence = fact.Evidence
	}

	saved, applied, err := s.store.ApplyEvidenceResearchSupplement(ctx, competition.ID, supplement)
	if err != nil {
		if errors.Is(err, store.ErrEvidenceResearchSupplementConflict) {
			retry := researchNextRetryAt(model.ResearchStateUnresolved, now, cfg)
			return researchReconcileFieldResult{
				Field:         fieldResult.Field,
				Outcome:       evidenceResearchConflict,
				ResearchState: model.ResearchStateUnresolved,
				NextRetryAt:   retry,
				LastError:     "research supplement conflicts with canonical lifecycle",
			}
		}
		retry := researchNextRetryAt(model.ResearchStateRetryable, now, cfg)
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchRejected,
			ResearchState: model.ResearchStateRetryable,
			NextRetryAt:   retry,
			LastError:     firstNonEmpty(err.Error(), "supplement write failed"),
		}
	}
	if !applied {
		// Race: another path filled the field between reload and write. Decide
		// based on what the canonical now holds: same date → already_present/
		// resolved; different date → conflict/skipped. Never unconditionally
		// resolve on applied=false.
		raceDate := researchFieldDate(saved, fieldResult.Field)
		if raceDate != nil && researchSameCalendarDate(*raceDate, fact.Date, loc) {
			return researchReconcileFieldResult{
				Field:            fieldResult.Field,
				Outcome:          evidenceResearchAlreadyPresent,
				ResearchState:    model.ResearchStateResolved,
				SavedCompetition: &saved,
			}
		}
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchConflict,
			ResearchState: model.ResearchStateSkipped,
			LastError:     "canonical field populated by concurrent path with different value",
		}
	}

	// Verify the saved canonical actually holds the research date before
	// recording resolved.
	savedDate := researchFieldDate(saved, fieldResult.Field)
	if savedDate == nil || !researchSameCalendarDate(*savedDate, fact.Date, loc) {
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchRejected,
			ResearchState: model.ResearchStateRetryable,
			NextRetryAt:   researchNextRetryAt(model.ResearchStateRetryable, now, cfg),
			LastError:     "saved canonical does not reflect the research date",
		}
	}
	return researchReconcileFieldResult{
		Field:            fieldResult.Field,
		Outcome:          evidenceResearchAccepted,
		ResearchState:    model.ResearchStateResolved,
		CanonicalChanged: true,
		SavedCompetition: &saved,
	}
}

// researchFieldNil reports whether the target field is currently nil on the
// competition (service-side helper mirroring the store's check).
func researchFieldNil(competition model.Competition, field model.EvidenceField) bool {
	switch field {
	case model.EvidenceRegistrationStart:
		return competition.RegistrationStart == nil
	case model.EvidenceRegistrationEnd:
		return competition.RegistrationEnd == nil
	case model.EvidenceCompetitionStart:
		return competition.CompetitionStart == nil
	case model.EvidenceCompetitionEnd:
		return competition.CompetitionEnd == nil
	default:
		return true
	}
}

// applyResearchDate sets the target date column and raw on a copy
// (service-side helper mirroring the store's write).
func applyResearchDate(competition *model.Competition, field model.EvidenceField, date time.Time, raw string) {
	switch field {
	case model.EvidenceRegistrationStart:
		competition.RegistrationStart = &date
		competition.RegistrationStartRaw = raw
	case model.EvidenceRegistrationEnd:
		competition.RegistrationEnd = &date
		competition.RegistrationEndRaw = raw
	case model.EvidenceCompetitionStart:
		competition.CompetitionStart = &date
		competition.CompetitionStartRaw = raw
	case model.EvidenceCompetitionEnd:
		competition.CompetitionEnd = &date
		competition.CompetitionEndRaw = raw
	}
}

// calendarDayIn truncates t to the start of its calendar day in loc. This is the
// business/config-location calendar day, never the UTC instant day. Persisting a
// date as a unix instant does NOT mean business calendar dates should be defined
// by UTC: the ResearchFact.Date already carries the correct semantic calendar
// day in the config location, and storage/DB-internal UTC must not shift it.
func calendarDayIn(t time.Time, loc *time.Location) time.Time {
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// sameCalendarDateIn reports whether two times fall on the same calendar day in
// the given location.
func sameCalendarDateIn(a, b time.Time, loc *time.Location) bool {
	return calendarDayIn(a, loc).Equal(calendarDayIn(b, loc))
}

// researchCalendarDay returns the calendar day (start of day) of t in the given
// business location, preserving the original semantic day. It replaces the old
// UTC-based helper: the research fact date must keep its original calendar day
// (e.g. 2026-04-09 stays 04-09), never be shifted back a day by a UTC cut.
func researchCalendarDay(t time.Time, loc *time.Location) time.Time {
	return calendarDayIn(t, loc)
}

// researchSameCalendarDate reports whether two times fall on the same business
// calendar day in loc.
func researchSameCalendarDate(a, b time.Time, loc *time.Location) bool {
	return sameCalendarDateIn(a, b, loc)
}

// researchFieldDate returns the current date pointer for a field (nil if none).
func researchFieldDate(competition model.Competition, field model.EvidenceField) *time.Time {
	switch field {
	case model.EvidenceRegistrationStart:
		return competition.RegistrationStart
	case model.EvidenceRegistrationEnd:
		return competition.RegistrationEnd
	case model.EvidenceCompetitionStart:
		return competition.CompetitionStart
	case model.EvidenceCompetitionEnd:
		return competition.CompetitionEnd
	default:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
