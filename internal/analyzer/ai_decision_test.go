package analyzer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestPlainStringAIFactIsParsedButRejectedWithoutEvidence(t *testing.T) {
	var result AIResult
	raw := `{"schema_version":"competition-audit-v8","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"fit_score":80,"recommendation":"适合开发者","rejection_reason":"","identity":{"name":"2026测试大赛","series":"","edition":"","organizer":"","track":"","group":"","scope":"","region":""},"facts":{"published_at":"","registration_start":"","registration_end":"","competition_start":"","competition_end":"","team_requirement":"","fee":"免费","eligibility":"","competition_contents":""},"events":[]}`
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	validated, err := validateAIResult(result, model.Document{Title: "2026测试大赛", Text: "赛事公告"}, time.Now(), time.Local)
	if err != nil {
		t.Fatal(err)
	}
	if validated.Identity.Name.Value != "" || validated.Facts.Fee.Value != "" || validated.Recommendation.Value != "" {
		t.Fatalf("unsupported plain-string facts survived validation: %#v", validated)
	}
}

func TestAIListingCannotUpdateCanonicalCompetition(t *testing.T) {
	result := AIResult{
		SchemaVersion:           AIAnalyzerVersion,
		DocumentType:            DocumentListing,
		SourceRole:              SourceOfficialPrimary,
		ComputerRelated:         true,
		CompetitionAnnouncement: true,
	}
	if aiDocumentCanUpdateCanonical(result) {
		t.Fatal("listing page was allowed to update canonical competition state")
	}
}

func TestAIRejectsOldEditionRegistrationEvent(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	document := model.Document{
		Title: "中国大学生计算机设计大赛",
		Text:  "2025年赛事报名通道现已开放。2026年赛事安排敬请期待。",
	}
	result := validAIResultFixture()
	result.Identity.Edition = AIFact{Value: "2026", Evidence: "2026年赛事安排敬请期待", Edition: "2026", Confidence: "high"}
	result.Events = []AICompetitionEvent{{
		Type:       AIEventRegistrationOpened,
		Evidence:   "2025年赛事报名通道现已开放",
		Edition:    "2025",
		Confidence: "high",
	}}
	validated, err := validateAIResult(result, document, time.Date(2026, 8, 4, 20, 0, 0, 0, location), location)
	if err != nil {
		t.Fatal(err)
	}
	if len(validated.Events) != 0 {
		t.Fatalf("old-edition event survived validation: %#v", validated.Events)
	}
}

func TestAIAcceptsCurrentExplicitRegistrationWithUnknownDeadline(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	document := model.Document{
		Title: "2026 AI Agent应用创新大赛报名通知",
		Text:  "2026 AI Agent应用创新大赛报名通道现已开放，报名截止时间另行通知。",
	}
	result := validAIResultFixture()
	result.Identity.Name = AIFact{Value: "2026 AI Agent应用创新大赛", Evidence: "2026 AI Agent应用创新大赛", Edition: "2026", Confidence: "high"}
	result.Identity.Edition = AIFact{Value: "2026", Evidence: "2026 AI Agent应用创新大赛", Edition: "2026", Confidence: "high"}
	result.Events = []AICompetitionEvent{{
		Type:       AIEventRegistrationOpened,
		Evidence:   "2026 AI Agent应用创新大赛报名通道现已开放",
		Edition:    "2026",
		Confidence: "high",
	}}
	validated, err := validateAIResult(result, document, time.Date(2026, 8, 4, 20, 0, 0, 0, location), location)
	if err != nil {
		t.Fatal(err)
	}
	registration, competitionPhase, evidence := phasesFromAIEvents(model.RegistrationUnknown, model.CompetitionUnknown, validated.Events)
	status := model.CompositeStatus(registration, competitionPhase)
	if status != model.StatusRegistrationOpen || evidence == "" {
		t.Fatalf("status=%s evidence=%q events=%#v", status, evidence, validated.Events)
	}
}

func TestAIDropsUnsupportedAndInventedEvidence(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	document := model.Document{Title: "2026云原生开发大赛", Text: "比赛面向全国高校。"}
	result := validAIResultFixture()
	result.Identity.Edition = AIFact{Value: "2026", Evidence: "2026云原生开发大赛", Edition: "2026", Confidence: "high"}
	result.Facts.Fee = AIFact{Value: "免费", Evidence: "本次比赛免收报名费", Edition: "2026", Confidence: "high"}
	result.Events = []AICompetitionEvent{{Type: AIEventRegistrationOpened, Evidence: "报名火热进行中", Edition: "2026", Confidence: "high"}}
	validated, err := validateAIResult(result, document, time.Date(2026, 8, 4, 20, 0, 0, 0, location), location)
	if err != nil {
		t.Fatal(err)
	}
	if validated.Facts.Fee.Value != "" || len(validated.Events) != 0 {
		t.Fatalf("invented facts survived validation: fee=%#v events=%#v", validated.Facts.Fee, validated.Events)
	}
}

func TestAIStateUsesLifecyclePriorityNotArrayOrder(t *testing.T) {
	events := []AICompetitionEvent{
		{Type: AIEventCompetitionStarted, Evidence: "比赛正式开赛"},
		{Type: AIEventRegistrationClosed, Evidence: "报名已经截止"},
		{Type: AIEventRegistrationOpened, Evidence: "报名通道现已开放"},
	}
	registration, competitionPhase, _ := phasesFromAIEvents(model.RegistrationUnknown, model.CompetitionUnknown, events)
	status := model.CompositeStatus(registration, competitionPhase)
	if status != model.StatusOngoing {
		t.Fatalf("status=%s, want ongoing", status)
	}
}

func TestAIKeepsRegistrationAndCompetitionPhasesIndependently(t *testing.T) {
	events := []AICompetitionEvent{
		{Type: AIEventRegistrationOpened, Evidence: "报名通道现已开放"},
		{Type: AIEventCompetitionUpcoming, Evidence: "比赛即将开赛"},
	}
	registration, competition, _ := phasesFromAIEvents(model.RegistrationUnknown, model.CompetitionUnknown, events)
	if registration != model.RegistrationOpen || competition != model.CompetitionUpcoming {
		t.Fatalf("registration=%s competition=%s", registration, competition)
	}
}

func TestMergeAIRejectsOpenStateBeforeRegistrationStart(t *testing.T) {
	location := time.FixedZone("CST", 8*60*60)
	analysis := &Analyzer{location: location}
	result := validAIResultFixture()
	result.Facts.RegistrationStart = AIFact{Value: "2026年9月1日", Evidence: "报名开始时间为2026年9月1日", Edition: "2026", Confidence: "high"}
	result.Events = []AICompetitionEvent{{Type: AIEventRegistrationOpened, Evidence: "2026年赛事报名通道现已开放", Edition: "2026", Confidence: "high"}}
	competition := analysis.mergeAI(model.Competition{Name: "2026测试赛事"}, result, model.Document{URL: "https://example.org/2026"}, time.Date(2026, 8, 4, 20, 0, 0, 0, location))
	if competition.Status != model.StatusUnknown || competition.StatusEvidence != "" {
		t.Fatalf("contradictory open state was accepted: %#v", competition)
	}
}

func validAIResultFixture() AIResult {
	return AIResult{
		SchemaVersion:           AIAnalyzerVersion,
		DocumentType:            DocumentOfficialAnnouncement,
		SourceRole:              SourceOfficialPrimary,
		ComputerRelated:         true,
		CompetitionAnnouncement: true,
		FitScore:                90,
	}
}

func TestValidateClassificationAcceptsValidJudgment(t *testing.T) {
	if err := validateClassification(AIClassification{
		SchemaVersion:           AIAnalyzerVersion,
		DocumentType:            DocumentOfficialAnnouncement,
		SourceRole:              SourceOfficialPrimary,
		ComputerRelated:         true,
		CompetitionAnnouncement: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateClassificationRejectsUnknownDocumentType(t *testing.T) {
	classification := AIClassification{SchemaVersion: AIAnalyzerVersion, DocumentType: "something_else", SourceRole: SourceOfficialPrimary}
	if err := validateClassification(classification); err == nil {
		t.Fatal("invalid document_type was accepted")
	}
}

func TestValidateClassificationRejectsWrongSchemaVersion(t *testing.T) {
	classification := AIClassification{SchemaVersion: "competition-audit-v4", DocumentType: DocumentOfficialAnnouncement, SourceRole: SourceOfficialPrimary}
	if err := validateClassification(classification); err == nil {
		t.Fatal("old schema version was accepted")
	}
}

func TestApplyClassificationFillsFirstPassFields(t *testing.T) {
	classification := AIClassification{
		SchemaVersion:           AIAnalyzerVersion,
		DocumentType:            DocumentListing,
		SourceRole:              SourceCommunity,
		ComputerRelated:         false,
		CompetitionAnnouncement: false,
		RejectionReason:         "聚合列表页",
	}
	result := validAIResultFixture()
	applyClassification(&result, classification)
	if result.DocumentType != DocumentListing || result.SourceRole != SourceCommunity ||
		result.ComputerRelated || result.CompetitionAnnouncement || result.RejectionReason != "聚合列表页" {
		t.Fatalf("classification fields were not merged: %#v", result)
	}
}
