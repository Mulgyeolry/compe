package analyzer

import (
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestSelectAnalysisSegmentsKeepsHighValueLateSection(t *testing.T) {
	document := model.Document{}
	for index := 0; index < 10; index++ {
		text := "赛事背景与普通介绍"
		if index == 9 {
			text = "报名截止时间为2026年9月20日，参赛对象为全国高校研究生。"
		}
		document.Segments = append(document.Segments, model.DocumentSegment{ID: string(rune('a' + index)), Kind: "html", Text: text})
	}
	selected := selectAnalysisSegments(model.Candidate{Title: "2026研究生程序设计大赛"}, document, 4)
	found := false
	for _, segment := range selected {
		if segment.ID == "j" {
			found = true
		}
	}
	if !found {
		t.Fatalf("late deadline segment was omitted: %#v", selected)
	}
}

func TestMergeAIChunkResultsRejectsConflictingFactValues(t *testing.T) {
	first := validAIResultFixture()
	first.Facts.RegistrationEnd = AIFact{Value: "2026年9月10日", Evidence: "报名截止2026年9月10日", Edition: "2026", Confidence: "high"}
	first.RawResponses = []string{"first"}
	first.SegmentIDs = []string{"html-1"}
	second := validAIResultFixture()
	second.Facts.RegistrationEnd = AIFact{Value: "2026年9月20日", Evidence: "报名截止2026年9月20日", Edition: "2026", Confidence: "high"}
	second.RawResponses = []string{"second"}
	second.SegmentIDs = []string{"html-2"}
	merged := mergeAIChunkResults([]AIResult{first, second})
	if merged.Facts.RegistrationEnd.Value != "" {
		t.Fatalf("conflicting deadline survived: %#v", merged.Facts.RegistrationEnd)
	}
	if len(merged.Rejections) != 1 || merged.Rejections[0].Field != "facts.registration_end" {
		t.Fatalf("conflict was not audited: %#v", merged.Rejections)
	}
	if len(merged.RawResponses) != 2 || len(merged.SegmentIDs) != 2 {
		t.Fatalf("chunk audit data missing: %#v", merged)
	}
}

func TestComputedFactConfidenceDoesNotUseModelConfidence(t *testing.T) {
	if got := computedFactConfidence(model.TrustHigh, "报名截止时间为2026年9月20日", "2026", "2026年8月4日"); got != "high" {
		t.Fatalf("high confidence=%q", got)
	}
	if got := computedFactConfidence(model.TrustLow, "报名中", "", ""); got != "low" {
		t.Fatalf("low confidence=%q", got)
	}
}

func TestBuildAnalysisAuditContainsAcceptedFields(t *testing.T) {
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, time.UTC)
	result := validAIResultFixture()
	result.RawResponses = []string{"{}"}
	result.SegmentIDs = []string{"html-1"}
	result.Rejections = []model.AnalysisRejection{{Field: "facts.fee", Reason: "missing evidence"}}
	audit := buildAnalysisAudit(result, model.Document{URL: "https://example.org", Text: "content"}, now, "test-model", map[string]model.FactEvidence{model.FactOrganizer: {Value: "主办方"}})
	if audit.Model != "test-model" || len(audit.AcceptedFields) != 1 || audit.AcceptedFields[0] != model.FactOrganizer || audit.InputHash == "" || len(audit.Rejections) != 1 {
		t.Fatalf("unexpected audit: %#v", audit)
	}
}
