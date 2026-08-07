package analyzer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"competition-assistant/internal/model"
)

const AIAnalyzerVersion = "competition-audit-v9"

// PendingCandidateError means the page was credible enough to retain as a
// minimal competition candidate, but AI verification must be retried before
// any extracted lifecycle fact is trusted.
type PendingCandidateError struct{ Err error }

func (e *PendingCandidateError) Error() string { return e.Err.Error() }
func (e *PendingCandidateError) Unwrap() error { return e.Err }

func IsPendingCandidateError(err error) bool {
	var target *PendingCandidateError
	return errors.As(err, &target)
}

// PartialEnrichmentError reports an extraction that produced usable results
// even though not every selected segment was fully analyzed. Callers must
// treat the accompanying AIResult as acceptable for stable fields but must
// schedule a retry before any lifecycle conclusion is trusted. A plain error
// means not enough results were available to use anything.
type PartialEnrichmentError struct {
	// FailedSegments lists the IDs of the segments whose extraction failed.
	FailedSegments []string
	// ConflictedFields lists single-value fact fields whose cross-segment
	// consensus was a tie and therefore could not be resolved.
	ConflictedFields []string
}

func (e *PartialEnrichmentError) Error() string {
	return fmt.Sprintf("llm extraction partially deferred: %d failed segment(s), %d unresolved field(s)",
		len(e.FailedSegments), len(e.ConflictedFields))
}

func IsPartialEnrichmentError(err error) bool {
	var target *PartialEnrichmentError
	return errors.As(err, &target)
}

type AIDocumentType string

const (
	DocumentListing              AIDocumentType = "listing"
	DocumentOfficialAnnouncement AIDocumentType = "official_announcement"
	DocumentRegistrationPage     AIDocumentType = "registration_page"
	DocumentRules                AIDocumentType = "rules_document"
	DocumentCampusInternal       AIDocumentType = "campus_internal"
	DocumentPostEventNews        AIDocumentType = "post_event_news"
	DocumentCommunity            AIDocumentType = "community"
)

type AISourceRole string

const (
	SourceOfficialPrimary AISourceRole = "official_primary"
	SourceOfficialPartner AISourceRole = "official_partner"
	SourceCampusForward   AISourceRole = "campus_forwarding"
	SourceCommunity       AISourceRole = "community"
)

type AIEventType string

const (
	AIEventPreviewed                   AIEventType = "competition_previewed"
	AIEventRegistrationAnnounced       AIEventType = "registration_announced"
	AIEventRegistrationOpened          AIEventType = "registration_opened"
	AIEventRegistrationDeadlineChanged AIEventType = "registration_deadline_changed"
	AIEventRegistrationClosed          AIEventType = "registration_closed"
	AIEventRulesReleased               AIEventType = "rules_released"
	AIEventProblemReleased             AIEventType = "problem_released"
	AIEventCompetitionUpcoming         AIEventType = "competition_upcoming"
	AIEventCompetitionStarted          AIEventType = "competition_started"
	AIEventCompetitionFinished         AIEventType = "competition_finished"
)

type AIFact struct {
	Value      string `json:"value"`
	Evidence   string `json:"evidence"`
	Edition    string `json:"edition"`
	Confidence string `json:"confidence"`
}

// UnmarshalJSON tolerates models that collapse a sourced fact to a plain
// string. The value is retained only long enough for validation: because it
// has no evidence, sanitizeAIFact will reject it before canonical data is
// updated. Object-shaped facts remain strict and reject unknown fields.
func (f *AIFact) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		*f = AIFact{}
		return nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return err
		}
		*f = AIFact{Value: value}
		return nil
	}
	type strictFact AIFact
	var decoded strictFact
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}
	*f = AIFact(decoded)
	return nil
}

type AIIdentity struct {
	Name      AIFact `json:"name"`
	Series    AIFact `json:"series"`
	Edition   AIFact `json:"edition"`
	Organizer AIFact `json:"organizer"`
	Track     AIFact `json:"track"`
	Group     AIFact `json:"group"`
	Scope     AIFact `json:"scope"`
	Region    AIFact `json:"region"`
}

type AIFacts struct {
	PublishedAt         AIFact `json:"published_at"`
	RegistrationStart   AIFact `json:"registration_start"`
	RegistrationEnd     AIFact `json:"registration_end"`
	CompetitionStart    AIFact `json:"competition_start"`
	CompetitionEnd      AIFact `json:"competition_end"`
	TeamRequirement     AIFact `json:"team_requirement"`
	Fee                 AIFact `json:"fee"`
	Eligibility         AIFact `json:"eligibility"`
	CompetitionContents AIFact `json:"competition_contents"`
}

type AICompetitionEvent struct {
	Type       AIEventType `json:"type"`
	Evidence   string      `json:"evidence"`
	Edition    string      `json:"edition"`
	Confidence string      `json:"confidence"`
}

// AIResult is deliberately an extraction result, not the canonical database
// model. In particular it contains no final status field. The application
// derives lifecycle state from validated events and dates.
type AIResult struct {
	SchemaVersion           string                    `json:"schema_version"`
	DocumentType            AIDocumentType            `json:"document_type"`
	SourceRole              AISourceRole              `json:"source_role"`
	ComputerRelated         bool                      `json:"computer_related"`
	CompetitionAnnouncement bool                      `json:"competition_announcement"`
	FitScore                int                       `json:"fit_score"`
	Recommendation          AIFact                    `json:"recommendation"`
	RejectionReason         string                    `json:"rejection_reason"`
	Identity                AIIdentity                `json:"identity"`
	Facts                   AIFacts                   `json:"facts"`
	Events                  []AICompetitionEvent      `json:"events"`
	RawResponses            []string                  `json:"-"`
	SegmentIDs              []string                  `json:"-"`
	Rejections              []model.AnalysisRejection `json:"-"`
}

var fourDigitYear = regexp.MustCompile(`(?:19|20)\d{2}`)

func validateAIResult(result AIResult, doc model.Document, now time.Time, location *time.Location) (AIResult, error) {
	if result.SchemaVersion != AIAnalyzerVersion {
		return AIResult{}, fmt.Errorf("unsupported ai schema version %q", result.SchemaVersion)
	}
	if !validDocumentType(result.DocumentType) {
		return AIResult{}, fmt.Errorf("invalid document_type %q", result.DocumentType)
	}
	if !validSourceRole(result.SourceRole) {
		return AIResult{}, fmt.Errorf("invalid source_role %q", result.SourceRole)
	}
	result.FitScore = min(100, max(0, result.FitScore))
	text := normalize(doc.Title + " " + doc.Text)
	result.Recommendation = sanitizeAIFact("recommendation", result.Recommendation, text, &result.Rejections)
	result.Identity.Name = sanitizeAIFact("identity.name", result.Identity.Name, text, &result.Rejections)
	result.Identity.Series = sanitizeAIFact("identity.series", result.Identity.Series, text, &result.Rejections)
	result.Identity.Edition = sanitizeAIFact("identity.edition", result.Identity.Edition, text, &result.Rejections)
	result.Identity.Organizer = sanitizeAIFact("identity.organizer", result.Identity.Organizer, text, &result.Rejections)
	result.Identity.Track = sanitizeAIFact("identity.track", result.Identity.Track, text, &result.Rejections)
	result.Identity.Group = sanitizeAIFact("identity.group", result.Identity.Group, text, &result.Rejections)
	result.Identity.Scope = sanitizeAIFact("identity.scope", result.Identity.Scope, text, &result.Rejections)
	result.Identity.Region = sanitizeAIFact("identity.region", result.Identity.Region, text, &result.Rejections)
	result.Facts.PublishedAt = sanitizeAIFact("facts.published_at", result.Facts.PublishedAt, text, &result.Rejections)
	result.Facts.RegistrationStart = sanitizeAIFact("facts.registration_start", result.Facts.RegistrationStart, text, &result.Rejections)
	result.Facts.RegistrationEnd = sanitizeAIFact("facts.registration_end", result.Facts.RegistrationEnd, text, &result.Rejections)
	result.Facts.CompetitionStart = sanitizeAIFact("facts.competition_start", result.Facts.CompetitionStart, text, &result.Rejections)
	result.Facts.CompetitionEnd = sanitizeAIFact("facts.competition_end", result.Facts.CompetitionEnd, text, &result.Rejections)
	result.Facts.TeamRequirement = sanitizeAIFact("facts.team_requirement", result.Facts.TeamRequirement, text, &result.Rejections)
	result.Facts.Fee = sanitizeAIFact("facts.fee", result.Facts.Fee, text, &result.Rejections)
	result.Facts.Eligibility = sanitizeAIFact("facts.eligibility", result.Facts.Eligibility, text, &result.Rejections)
	result.Facts.CompetitionContents = sanitizeAIFact("facts.competition_contents", result.Facts.CompetitionContents, text, &result.Rejections)

	documentEdition := firstNonEmpty(result.Identity.Edition.Value, result.Identity.Name.Edition, result.Identity.Series.Edition)
	if documentEdition == "" {
		if year := yearIn(doc.Title); year != 0 {
			documentEdition = strconv.Itoa(year)
		}
	}
	result.Facts.PublishedAt = editionBoundFact("facts.published_at", result.Facts.PublishedAt, documentEdition, false, &result.Rejections)
	result.Recommendation = editionBoundFact("recommendation", result.Recommendation, documentEdition, true, &result.Rejections)
	result.Facts.RegistrationStart = editionBoundFact("facts.registration_start", result.Facts.RegistrationStart, documentEdition, true, &result.Rejections)
	result.Facts.RegistrationEnd = editionBoundFact("facts.registration_end", result.Facts.RegistrationEnd, documentEdition, true, &result.Rejections)
	result.Facts.CompetitionStart = editionBoundFact("facts.competition_start", result.Facts.CompetitionStart, documentEdition, true, &result.Rejections)
	result.Facts.CompetitionEnd = editionBoundFact("facts.competition_end", result.Facts.CompetitionEnd, documentEdition, true, &result.Rejections)
	result.Facts.TeamRequirement = editionBoundFact("facts.team_requirement", result.Facts.TeamRequirement, documentEdition, true, &result.Rejections)
	result.Facts.Fee = editionBoundFact("facts.fee", result.Facts.Fee, documentEdition, true, &result.Rejections)
	result.Facts.Eligibility = editionBoundFact("facts.eligibility", result.Facts.Eligibility, documentEdition, true, &result.Rejections)
	result.Facts.CompetitionContents = editionBoundFact("facts.competition_contents", result.Facts.CompetitionContents, documentEdition, true, &result.Rejections)
	seen := make(map[AIEventType]bool)
	validatedEvents := make([]AICompetitionEvent, 0, len(result.Events))
	for _, event := range result.Events {
		event.Evidence = normalize(event.Evidence)
		event.Edition = strings.TrimSpace(event.Edition)
		event.Confidence = normalizeAIConfidence(event.Confidence)
		if !validAIEventType(event.Type) {
			result.Rejections = append(result.Rejections, model.AnalysisRejection{Field: "events", Reason: "unsupported event type", Value: string(event.Type)})
			continue
		}
		if seen[event.Type] {
			result.Rejections = append(result.Rejections, model.AnalysisRejection{Field: "events." + string(event.Type), Reason: "duplicate lifecycle event"})
			continue
		}
		if event.Evidence == "" || !strings.Contains(text, event.Evidence) {
			result.Rejections = append(result.Rejections, model.AnalysisRejection{Field: "events." + string(event.Type), Reason: "event evidence is missing from document", Value: event.Evidence})
			continue
		}
		if event.Edition == "" {
			if year := yearIn(event.Evidence); year != 0 {
				event.Edition = strconv.Itoa(year)
			}
		}
		if documentEdition != "" {
			if event.Edition == "" || !sameEdition(documentEdition, event.Edition) {
				result.Rejections = append(result.Rejections, model.AnalysisRejection{Field: "events." + string(event.Type), Reason: "event is not bound to the current edition", Value: event.Edition})
				continue
			}
		}
		if !eventSemanticsSupported(event) {
			result.Rejections = append(result.Rejections, model.AnalysisRejection{Field: "events." + string(event.Type), Reason: "event evidence does not explicitly support its semantics", Value: event.Evidence})
			continue
		}
		if event.Type == AIEventRegistrationOpened && !registrationEvidenceIsCurrent(result, event, doc.PublishedAtRaw, now, location) {
			result.Rejections = append(result.Rejections, model.AnalysisRejection{Field: "events.registration_opened", Reason: "registration event is not demonstrably current", Value: event.Edition})
			continue
		}
		seen[event.Type] = true
		validatedEvents = append(validatedEvents, event)
	}
	result.Events = validatedEvents
	if result.Facts.RegistrationEnd.Value != "" && len(registrationEndDates(text, location)) > 1 && !deadlineChangeSupports(result) {
		result.Rejections = append(result.Rejections, model.AnalysisRejection{Field: "facts.registration_end", Reason: "multiple registration deadlines appear without an explicit validated deadline change", Value: result.Facts.RegistrationEnd.Value})
		result.Facts.RegistrationEnd = AIFact{}
	}
	return result, nil
}

func deadlineChangeSupports(result AIResult) bool {
	for _, event := range result.Events {
		if event.Type == AIEventRegistrationDeadlineChanged && (strings.Contains(event.Evidence, result.Facts.RegistrationEnd.Value) || strings.Contains(event.Evidence, result.Facts.RegistrationEnd.Evidence)) {
			return true
		}
	}
	return false
}

func sanitizeAIFact(field string, fact AIFact, text string, rejections *[]model.AnalysisRejection) AIFact {
	fact.Value = strings.TrimSpace(fact.Value)
	fact.Evidence = normalize(fact.Evidence)
	fact.Edition = strings.TrimSpace(fact.Edition)
	fact.Confidence = normalizeAIConfidence(fact.Confidence)
	if fact.Value == "" || fact.Evidence == "" || !strings.Contains(text, fact.Evidence) {
		if fact.Value != "" || fact.Evidence != "" {
			*rejections = append(*rejections, model.AnalysisRejection{Field: field, Reason: "fact evidence is missing from document", Value: fact.Value})
		}
		return AIFact{}
	}
	return fact
}

func editionBoundFact(field string, fact AIFact, documentEdition string, requireBinding bool, rejections *[]model.AnalysisRejection) AIFact {
	if fact.Value == "" || documentEdition == "" {
		return fact
	}
	factEdition := fact.Edition
	if factEdition == "" {
		if year := yearIn(fact.Evidence); year != 0 {
			factEdition = strconv.Itoa(year)
		}
	}
	if factEdition == "" {
		if requireBinding {
			*rejections = append(*rejections, model.AnalysisRejection{Field: field, Reason: "fact cannot be bound to the current edition", Value: fact.Value})
			return AIFact{}
		}
		return fact
	}
	if !sameEdition(documentEdition, factEdition) {
		*rejections = append(*rejections, model.AnalysisRejection{Field: field, Reason: "fact belongs to a different edition", Value: factEdition})
		return AIFact{}
	}
	fact.Edition = factEdition
	return fact
}

func normalizeAIConfidence(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "high", "medium", "low":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "low"
	}
}

func validDocumentType(value AIDocumentType) bool {
	switch value {
	case DocumentListing, DocumentOfficialAnnouncement, DocumentRegistrationPage, DocumentRules, DocumentCampusInternal, DocumentPostEventNews, DocumentCommunity:
		return true
	default:
		return false
	}
}

func validSourceRole(value AISourceRole) bool {
	switch value {
	case SourceOfficialPrimary, SourceOfficialPartner, SourceCampusForward, SourceCommunity:
		return true
	default:
		return false
	}
}

func validAIEventType(value AIEventType) bool {
	switch value {
	case AIEventPreviewed, AIEventRegistrationAnnounced, AIEventRegistrationOpened, AIEventRegistrationDeadlineChanged,
		AIEventRegistrationClosed, AIEventRulesReleased, AIEventProblemReleased, AIEventCompetitionUpcoming,
		AIEventCompetitionStarted, AIEventCompetitionFinished:
		return true
	default:
		return false
	}
}

func eventSemanticsSupported(event AICompetitionEvent) bool {
	switch event.Type {
	case AIEventPreviewed, AIEventRegistrationAnnounced:
		return containsAny(event.Evidence, []string{"预告", "即将启动", "敬请期待", "预约报名", "开放报名", "启动仪式", "新一届"})
	case AIEventRegistrationOpened:
		return strings.Contains(event.Evidence, "报名") && containsAny(event.Evidence, []string{"开始报名", "开放报名", "报名启动", "报名通道", "即日起", "现已开放", "正式开放"})
	case AIEventRegistrationDeadlineChanged:
		return strings.Contains(event.Evidence, "报名") && containsAny(event.Evidence, []string{"延期", "延长", "调整", "变更", "截止"})
	case AIEventRegistrationClosed:
		return strings.Contains(event.Evidence, "报名") && containsAny(event.Evidence, []string{"截止", "结束", "关闭"})
	case AIEventRulesReleased:
		return containsAny(event.Evidence, []string{"规则发布", "竞赛规程", "比赛规则", "赛事规则"})
	case AIEventProblemReleased:
		return containsAny(event.Evidence, []string{"赛题发布", "题目发布", "赛题已发布", "赛题正式发布"}) && !strings.Contains(event.Evidence, "发布会")
	case AIEventCompetitionUpcoming:
		return containsAny(event.Evidence, []string{"即将开赛", "即将开始", "将于"})
	case AIEventCompetitionStarted:
		return containsAny(event.Evidence, []string{"正式开赛", "正式开始", "今日开赛", "比赛开始"})
	case AIEventCompetitionFinished:
		return containsAny(event.Evidence, []string{"比赛结束", "赛事结束", "总决赛落幕", "总决赛闭幕"})
	default:
		return false
	}
}

func registrationEvidenceIsCurrent(result AIResult, event AICompetitionEvent, documentPublishedAt string, now time.Time, location *time.Location) bool {
	if location == nil {
		location = time.Local
	}
	now = now.In(location)
	for _, value := range []string{event.Edition, result.Identity.Edition.Value, result.Identity.Name.Value, result.Identity.Series.Value} {
		if year := yearIn(value); year != 0 {
			return year >= now.Year()
		}
	}
	if result.Facts.PublishedAt.Value != "" {
		if published := parseDate(result.Facts.PublishedAt.Value, location); published != nil {
			age := now.Sub(*published)
			return age >= -31*24*time.Hour && age <= 370*24*time.Hour
		}
	}
	if documentPublishedAt != "" {
		if published := parseDate(documentPublishedAt, location); published != nil {
			age := now.Sub(*published)
			return age >= -31*24*time.Hour && age <= 370*24*time.Hour
		}
	}
	return false
}

func yearIn(value string) int {
	raw := fourDigitYear.FindString(value)
	if raw == "" {
		return 0
	}
	year, _ := strconv.Atoi(raw)
	return year
}

func sameEdition(left, right string) bool {
	leftYear, rightYear := yearIn(left), yearIn(right)
	if leftYear != 0 && rightYear != 0 {
		return leftYear == rightYear
	}
	return normalizeIdentity(left) == normalizeIdentity(right)
}

func normalizeIdentity(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), ""))
}

func aiDocumentCanUpdateCanonical(result AIResult) bool {
	if !result.CompetitionAnnouncement || !result.ComputerRelated {
		return false
	}
	if result.SourceRole != SourceOfficialPrimary && result.SourceRole != SourceOfficialPartner {
		return false
	}
	switch result.DocumentType {
	case DocumentOfficialAnnouncement, DocumentRegistrationPage, DocumentRules:
		return true
	default:
		return false
	}
}

func phasesFromAIEvents(registration model.RegistrationPhase, competition model.CompetitionPhase, events []AICompetitionEvent) (model.RegistrationPhase, model.CompetitionPhase, string) {
	registrationRank := map[model.RegistrationPhase]int{
		model.RegistrationUnknown: 0,
		model.RegistrationPreview: 10,
		model.RegistrationOpen:    20,
		model.RegistrationClosed:  30,
	}
	competitionRank := map[model.CompetitionPhase]int{
		model.CompetitionUnknown:  0,
		model.CompetitionUpcoming: 10,
		model.CompetitionOngoing:  20,
		model.CompetitionFinished: 30,
	}
	selectedEvidence, selectedPriority := "", 0
	for _, event := range events {
		switch event.Type {
		case AIEventPreviewed, AIEventRegistrationAnnounced:
			if registrationRank[model.RegistrationPreview] >= registrationRank[registration] {
				registration = model.RegistrationPreview
			}
		case AIEventRegistrationOpened:
			if registrationRank[model.RegistrationOpen] >= registrationRank[registration] {
				registration = model.RegistrationOpen
			}
		case AIEventRegistrationClosed:
			registration = model.RegistrationClosed
		case AIEventCompetitionUpcoming:
			if competitionRank[model.CompetitionUpcoming] >= competitionRank[competition] {
				competition = model.CompetitionUpcoming
			}
		case AIEventCompetitionStarted:
			if competitionRank[model.CompetitionOngoing] >= competitionRank[competition] {
				competition = model.CompetitionOngoing
			}
		case AIEventCompetitionFinished:
			competition = model.CompetitionFinished
		}
		priority := registrationRank[registration]
		if competition != model.CompetitionUnknown {
			priority = 100 + competitionRank[competition]
		}
		if priority > selectedPriority {
			selectedPriority = priority
			selectedEvidence = event.Evidence
		}
	}
	return registration, competition, selectedEvidence
}

// AIClassification is the minimal first-pass judgment produced before any
// extraction request. It only decides whether the document is a trackable
// computer-competition announcement. Pages that fail it never pay for the
// longer extraction call, which is the main lever against long-context time
// outs and truncated JSON from weak models.
type AIClassification struct {
	SchemaVersion           string         `json:"schema_version"`
	DocumentType            AIDocumentType `json:"document_type"`
	SourceRole              AISourceRole   `json:"source_role"`
	ComputerRelated         bool           `json:"computer_related"`
	CompetitionAnnouncement bool           `json:"competition_announcement"`
	RejectionReason         string         `json:"rejection_reason"`
}

func validateClassification(result AIClassification) error {
	if result.SchemaVersion != AIAnalyzerVersion {
		return fmt.Errorf("unsupported ai schema version %q", result.SchemaVersion)
	}
	if !validDocumentType(result.DocumentType) {
		return fmt.Errorf("invalid document_type %q", result.DocumentType)
	}
	if !validSourceRole(result.SourceRole) {
		return fmt.Errorf("invalid source_role %q", result.SourceRole)
	}
	return nil
}

// applyClassification merges the first-pass judgment into an extraction
// result so the existing validation and canonical-update gates stay intact.
func applyClassification(result *AIResult, classification AIClassification) {
	result.SchemaVersion = AIAnalyzerVersion
	result.DocumentType = classification.DocumentType
	result.SourceRole = classification.SourceRole
	result.ComputerRelated = classification.ComputerRelated
	result.CompetitionAnnouncement = classification.CompetitionAnnouncement
	result.RejectionReason = classification.RejectionReason
}

// classificationCanUpdateCanonical is the first-pass gate. It decides whether
// extraction is worth paying for at all: only official announcements of one
// of the trackable document types pass, so listing, campus and post-event
// pages stop after the single tiny classification request.
func classificationCanUpdateCanonical(classification AIClassification) bool {
	return aiDocumentCanUpdateCanonical(AIResult{
		DocumentType:            classification.DocumentType,
		SourceRole:              classification.SourceRole,
		ComputerRelated:         classification.ComputerRelated,
		CompetitionAnnouncement: classification.CompetitionAnnouncement,
	})
}
