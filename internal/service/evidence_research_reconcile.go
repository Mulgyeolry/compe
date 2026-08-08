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
func researchLifecycleAfterSupplement(competition model.Competition, now time.Time) (model.Competition, bool) {
	today := model.DayStart(now)
	changed := false

	// Registration phase: only when unknown.
	if competition.RegistrationPhase == model.RegistrationUnknown {
		if competition.RegistrationEnd != nil && !model.DayStart(*competition.RegistrationEnd).After(today) {
			competition.RegistrationPhase = model.RegistrationClosed
			changed = true
		} else if competition.RegistrationStart != nil && model.DayStart(*competition.RegistrationStart).After(today) {
			competition.RegistrationPhase = model.RegistrationPreview
			changed = true
		} else if competition.RegistrationStart != nil && competition.RegistrationEnd != nil &&
			!model.DayStart(*competition.RegistrationStart).After(today) && !model.DayStart(*competition.RegistrationEnd).Before(today) {
			competition.RegistrationPhase = model.RegistrationOpen
			changed = true
		}
		// past start only (no end), or future end only (no start): stay unknown.
	}

	// Competition phase: only when unknown.
	if competition.CompetitionPhase == model.CompetitionUnknown {
		if competition.CompetitionEnd != nil && !model.DayStart(*competition.CompetitionEnd).After(today) {
			competition.CompetitionPhase = model.CompetitionFinished
			changed = true
		} else if competition.CompetitionStart != nil && model.DayStart(*competition.CompetitionStart).After(today) {
			competition.CompetitionPhase = model.CompetitionUpcoming
			changed = true
		} else if competition.CompetitionStart != nil && competition.CompetitionEnd != nil &&
			!model.DayStart(*competition.CompetitionStart).After(today) && !model.DayStart(*competition.CompetitionEnd).Before(today) {
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

	// Field mismatch is an invariant error.
	if fact.Field != fieldResult.Field || fact.Date.IsZero() || !model.ValidEvidenceField(fact.Field) {
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchConflict,
			ResearchState: model.ResearchStateUnresolved,
			NextRetryAt:   researchNextRetryAt(model.ResearchStateUnresolved, now, cfg),
			LastError:     "research fact field mismatch or invalid (invariant)",
		}
	}

	// Edition re-check: execution.Edition == fact.Edition == fact.Date.Year(),
	// and matches the current canonical edition.
	canonicalEdition, edErr := evidenceResearchEdition(competition)
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

	// Authority gate: same official authority only. Cross-domain is never written.
	if !sameResearchAuthority(fact.SourceURL, competition.OfficialURL) {
		return reject("research source is outside canonical official authority")
	}

	// Reload canonical to avoid TOCTOU and detect whether the target was filled
	// by another path since planning.
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
	if !researchFieldNil(current, fieldResult.Field) {
		existing := researchFieldDate(current, fieldResult.Field)
		if existing != nil && model.DayStart(*existing).Equal(model.DayStart(fact.Date)) {
			// Canonical already holds the same date → resolved, no write.
			return researchReconcileFieldResult{
				Field:         fieldResult.Field,
				Outcome:       evidenceResearchAlreadyPresent,
				ResearchState: model.ResearchStateResolved,
				SavedCompetition: &current,
			}
		}
		// Canonical holds a different date → do not overwrite, skip.
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchConflict,
			ResearchState: model.ResearchStateSkipped,
			LastError:     "canonical field already populated with different value",
		}
	}

	// Build the supplement + lifecycle inference.
	supplement := store.EvidenceResearchSupplement{
		Field: fieldResult.Field,
		Date:  model.DayStart(fact.Date),
		Raw:   strings.TrimSpace(fact.Raw),
		Fact: model.FactEvidence{
			Value:      strings.TrimSpace(fact.Raw),
			Raw:        strings.TrimSpace(fact.Raw),
			Evidence:   fact.Evidence,
			Edition:    fact.Edition,
			SourceURL:  fact.SourceURL,
			Confidence: researchFactConfidence(competition.Trust),
			ObservedAt: now,
		},
	}
	next := current
	applyResearchDate(&next, fieldResult.Field, model.DayStart(fact.Date), strings.TrimSpace(fact.Raw))
	lifecycle, phaseChanged := researchLifecycleAfterSupplement(next, now)
	supplement.RegistrationPhase = lifecycle.RegistrationPhase
	supplement.CompetitionPhase = lifecycle.CompetitionPhase
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
		// Another path filled the field between reload and write (race); treat as
		// already present / resolved conservatively.
		return researchReconcileFieldResult{
			Field:         fieldResult.Field,
			Outcome:       evidenceResearchAlreadyPresent,
			ResearchState: model.ResearchStateResolved,
			SavedCompetition: &saved,
		}
	}

	// Verify the saved canonical actually holds the research date before
	// recording resolved.
	savedDate := researchFieldDate(saved, fieldResult.Field)
	if savedDate == nil || !model.DayStart(*savedDate).Equal(model.DayStart(fact.Date)) {
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
