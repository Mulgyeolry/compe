package analyzer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
)

// TestEnrichSegmentFailureDoesNotCancelSiblings is the core regression for the
// first defect: a failing segment must not cancel the successful siblings, and
// the resulting partial result must retain stable facts while withholding
// lifecycle facts and events.
func TestEnrichSegmentFailureDoesNotCancelSiblings(t *testing.T) {
	var segmentACalls, segmentBCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// The extraction prompt embeds the segment ID; the classification path
		// is not exercised here because Enrich is called directly.
		if strings.Contains(string(body), "segment-a") {
			segmentACalls.Add(1)
			http.Error(w, "segmented model outage", http.StatusInternalServerError)
			return
		}
		if strings.Contains(string(body), "segment-b") {
			segmentBCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10","identity":{"organizer":{"value":"中国计算机学会","evidence":"主办方：中国计算机学会","edition":"2026","confidence":"high"}},"facts":{"registration_end":{"value":"2026年9月20日","evidence":"报名截止时间为2026年9月20日","edition":"2026","confidence":"high"}},"events":[{"type":"registration_opened","evidence":"本赛事现已开放报名","edition":"2026","confidence":"high"}]}`)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10"}`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()
	document := model.Document{
		Title: "2026大赛报名通知",
		Text:  "主办方：中国计算机学会。报名截止时间为2026年9月20日。",
		Segments: []model.DocumentSegment{
			{ID: "segment-a", Kind: "html", Text: "本段请求将失败"},
			{ID: "segment-b", Kind: "html", Text: "主办方：中国计算机学会。报名截止时间为2026年9月20日。"},
		},
	}
	result, err := modelClient.Enrich(context.Background(), model.Candidate{Title: document.Title}, document)
	if err == nil {
		t.Fatal("expected a partial enrichment error")
	}
	if !IsPartialEnrichmentError(err) {
		t.Fatalf("error is not a partial enrichment error: %v", err)
	}
	if segmentACalls.Load() != 1 {
		t.Fatalf("segment-a calls=%d, want 1", segmentACalls.Load())
	}
	if segmentBCalls.Load() != 1 {
		t.Fatalf("segment-b was cancelled or not called: calls=%d, want 1", segmentBCalls.Load())
	}
	// The successful segment's stable fact must be retained.
	if result.Identity.Organizer.Value != "中国计算机学会" {
		t.Fatalf("organizer was lost: %#v", result.Identity.Organizer)
	}
	// Lifecycle facts and events must be withheld.
	if result.Facts.RegistrationEnd.Value != "" {
		t.Fatalf("lifecycle fact not withheld: %#v", result.Facts.RegistrationEnd)
	}
	if len(result.Events) != 0 {
		t.Fatalf("lifecycle events not withheld: %#v", result.Events)
	}
	// Audit must record both the failed segment and the lifecycle withholding.
	if !containsRejection(result.Rejections, "segments.segment-a") {
		t.Fatalf("failed segment rejection missing: %#v", result.Rejections)
	}
	if !containsRejection(result.Rejections, "lifecycle") {
		t.Fatalf("lifecycle withholding rejection missing: %#v", result.Rejections)
	}
}

// TestEnrichInsufficientSuccessReturnsPlainError verifies the minimum success
// threshold: 1 of 4 successful segments is not enough and must return a plain
// error that is not a partial enrichment error.
func TestEnrichInsufficientSuccessReturnsPlainError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "segment-good") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10","identity":{"organizer":{"value":"主办方","evidence":"主办方：主办方","edition":"2026","confidence":"high"}}}`)))
			return
		}
		http.Error(w, "model unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()
	document := model.Document{
		Title: "2026大赛",
		Text:  "主办方：主办方。",
		Segments: []model.DocumentSegment{
			{ID: "segment-good", Kind: "html", Text: "主办方：主办方。"},
			{ID: "segment-bad-1", Kind: "html", Text: "x"},
			{ID: "segment-bad-2", Kind: "html", Text: "y"},
			{ID: "segment-bad-3", Kind: "html", Text: "z"},
		},
	}
	result, err := modelClient.Enrich(context.Background(), model.Candidate{Title: document.Title}, document)
	if err == nil {
		t.Fatal("expected a plain error")
	}
	if IsPartialEnrichmentError(err) {
		t.Fatalf("insufficient result was mislabeled as partial: %v", err)
	}
	if result.Facts.RegistrationEnd.Value != "" || result.Identity.Organizer.Value != "" {
		t.Fatalf("insufficient result was not zeroed: %#v", result)
	}
	if calls.Load() != 4 {
		t.Fatalf("expected all 4 segments to complete, calls=%d", calls.Load())
	}
}

// TestEnrichParentCancellationIsNotPartial verifies that cancelling the parent
// context is surfaced as a context error and never as usable partial results.
func TestEnrichParentCancellationIsNotPartial(t *testing.T) {
	requestReceived := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "segment-good") {
			select {
			case requestReceived <- struct{}{}:
			default:
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10"}`)))
			return
		}
		http.Error(w, "model unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()
	document := model.Document{
		Title: "2026大赛",
		Text:  "正文",
		Segments: []model.DocumentSegment{
			{ID: "segment-good", Kind: "html", Text: "主办方：中国计算机学会。"},
			{ID: "segment-bad", Kind: "html", Text: "x"},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan struct {
		result AIResult
		err    error
	})
	go func() {
		result, err := modelClient.Enrich(ctx, model.Candidate{Title: document.Title}, document)
		resultCh <- struct {
			result AIResult
			err    error
		}{result, err}
	}()
	// Wait until at least one segment request reaches the server, then cancel.
	<-requestReceived
	cancel()
	out := <-resultCh
	if out.err == nil {
		t.Fatal("expected a context error")
	}
	if !errors.Is(out.err, context.Canceled) && !errors.Is(out.err, context.DeadlineExceeded) {
		t.Fatalf("expected context cancellation, got: %v", out.err)
	}
	if IsPartialEnrichmentError(out.err) {
		t.Fatalf("parent cancellation was wrapped as partial enrichment: %v", out.err)
	}
	if out.result.Facts.RegistrationEnd.Value != "" {
		t.Fatalf("cancellation produced usable partial result: %#v", out.result)
	}
}

func containsRejection(rejections []model.AnalysisRejection, field string) bool {
	for _, rejection := range rejections {
		if rejection.Field == field {
			return true
		}
	}
	return false
}

func TestMergeAIMajorityConsensusWins(t *testing.T) {
	parts := []AIResult{
		withRegistrationEnd("2026年9月20日"),
		withRegistrationEnd("2026年9月10日"),
		withRegistrationEnd("2026年9月20日"),
	}
	merged, conflicted := mergeAIChunkResults(parts)
	if merged.Facts.RegistrationEnd.Value != "2026年9月20日" {
		t.Fatalf("majority value not adopted: %#v", merged.Facts.RegistrationEnd)
	}
	if len(conflicted) != 0 {
		t.Fatalf("unexpected conflict fields: %v", conflicted)
	}
	if !hasRejectionReason(merged.Rejections, "minority conflicting values discarded by cross-segment consensus") {
		t.Fatalf("minority discard not audited: %#v", merged.Rejections)
	}
}

func TestMergeAIMajorityConsensusIsOrderIndependent(t *testing.T) {
	parts := []AIResult{
		withRegistrationEnd("2026年9月20日"),
		withRegistrationEnd("2026年9月10日"),
		withRegistrationEnd("2026年9月20日"),
	}
	mergedA, conflictedA := mergeAIChunkResults(append([]AIResult{}, parts...))
	shuffled := []AIResult{parts[1], parts[2], parts[0]}
	mergedB, conflictedB := mergeAIChunkResults(shuffled)
	if mergedA.Facts.RegistrationEnd.Value != mergedB.Facts.RegistrationEnd.Value {
		t.Fatalf("value depends on order: %q vs %q", mergedA.Facts.RegistrationEnd.Value, mergedB.Facts.RegistrationEnd.Value)
	}
	if !equalRejections(mergedA.Rejections, mergedB.Rejections) {
		t.Fatalf("rejections depend on order:\nA=%#v\nB=%#v", mergedA.Rejections, mergedB.Rejections)
	}
	if !equalStrings(conflictedA, conflictedB) {
		t.Fatalf("conflicted fields depend on order: %v vs %v", conflictedA, conflictedB)
	}
}

func TestMergeAIUnresolvedTieClearsField(t *testing.T) {
	parts := []AIResult{
		withRegistrationEnd("2026年9月20日"),
		withRegistrationEnd("2026年9月10日"),
	}
	merged, conflicted := mergeAIChunkResults(parts)
	if merged.Facts.RegistrationEnd.Value != "" {
		t.Fatalf("tied field was not cleared: %#v", merged.Facts.RegistrationEnd)
	}
	if !containsString(conflicted, "facts.registration_end") {
		t.Fatalf("conflicted fields missing registration_end: %v", conflicted)
	}
	if !hasRejectionReason(merged.Rejections, "unresolved conflicting values across document segments") {
		t.Fatalf("unresolved conflict not audited: %#v", merged.Rejections)
	}
	// A tie over a lifecycle field must drive a partial enrichment error once
	// it reaches Enrich, so the retry is scheduled.
	partial := &PartialEnrichmentError{ConflictedFields: conflicted}
	if !IsPartialEnrichmentError(partial) {
		t.Fatal("unresolved lifecycle conflict did not produce a partial enrichment error")
	}
}

func TestMergeAISameValueChoosesBetterEvidence(t *testing.T) {
	parts := []AIResult{
		{
			SchemaVersion: AIAnalyzerVersion,
			Facts: AIFacts{Fee: AIFact{
				Value: "50元/人", Edition: "2026", Confidence: "low",
				Evidence: "报名费为50元/人",
			}},
		},
		{
			SchemaVersion: AIAnalyzerVersion,
			Facts: AIFacts{Fee: AIFact{
				Value: "50元/人", Edition: "2026", Confidence: "high",
				Evidence: "报名费为50元/人，缴费后确认参赛资格",
			}},
		},
	}
	merged, conflicted := mergeAIChunkResults(parts)
	if merged.Facts.Fee.Value != "50元/人" {
		t.Fatalf("shared value lost: %#v", merged.Facts.Fee)
	}
	if len(conflicted) != 0 {
		t.Fatalf("same-value facts wrongly conflicted: %v", conflicted)
	}
	if merged.Facts.Fee.Confidence != "high" {
		t.Fatalf("higher-confidence representative not selected: %#v", merged.Facts.Fee)
	}
}

// TestConsensusSameValueOrderIndependent proves that swapping the order of
// same-value candidates yields an identical full representative AIFact and an
// identical set of rejections.
func TestConsensusSameValueOrderIndependent(t *testing.T) {
	a := AIResult{
		SchemaVersion: AIAnalyzerVersion,
		Facts: AIFacts{Fee: AIFact{
			Value: "50元/人", Edition: "2026", Confidence: "medium",
			Evidence: "报名费为50元/人",
		}},
	}
	b := AIResult{
		SchemaVersion: AIAnalyzerVersion,
		Facts: AIFacts{Fee: AIFact{
			Value: "50元/人", Edition: "2026", Confidence: "high",
			Evidence: "报名费为50元/人，缴费后确认参赛资格",
		}},
	}
	// Exercise the representative-selection tiebreak with three same-value
	// candidates so the full fact is decided by confidence then evidence length.
	c := AIResult{
		SchemaVersion: AIAnalyzerVersion,
		Facts: AIFacts{Fee: AIFact{
			Value: "50元/人", Edition: "2026", Confidence: "high",
			Evidence: "报名费为50元/人，缴费后确认参赛资格，费用含税",
		}},
	}
	forward, conflictedForward := mergeAIChunkResults([]AIResult{a, b, c})
	reversed, conflictedReversed := mergeAIChunkResults([]AIResult{c, b, a})
	if forward.Facts.Fee != reversed.Facts.Fee {
		t.Fatalf("full AIFact depends on part order:\nA=%#v\nB=%#v", forward.Facts.Fee, reversed.Facts.Fee)
	}
	if !equalRejections(forward.Rejections, reversed.Rejections) {
		t.Fatalf("rejections depend on part order:\nA=%#v\nB=%#v", forward.Rejections, reversed.Rejections)
	}
	if !equalStrings(conflictedForward, conflictedReversed) {
		t.Fatalf("conflicted fields depend on order: %v vs %v", conflictedForward, conflictedReversed)
	}
	// The longest, highest-confidence evidence must win as the representative.
	if forward.Facts.Fee.Evidence != "报名费为50元/人，缴费后确认参赛资格，费用含税" {
		t.Fatalf("best representative not selected: %#v", forward.Facts.Fee)
	}
}

// TestConsensusDerivesEditionFromEvidence proves that when the winning fact's
// Edition is empty but the group was keyed by a year derived from evidence, the
// derived year is written back so a later edition check does not reject it.
func TestConsensusDerivesEditionFromEvidence(t *testing.T) {
	parts := []AIResult{
		{
			SchemaVersion: AIAnalyzerVersion,
			Facts: AIFacts{Fee: AIFact{
				Value: "50元/人", Evidence: "2026年报名费为50元/人", Confidence: "high",
			}},
		},
		{
			SchemaVersion: AIAnalyzerVersion,
			Facts: AIFacts{Fee: AIFact{
				Value: "50元/人", Evidence: "2026年报名费为50元/人", Confidence: "high",
			}},
		},
	}
	merged, conflicted := mergeAIChunkResults(parts)
	if len(conflicted) != 0 {
		t.Fatalf("unexpected conflict: %v", conflicted)
	}
	if merged.Facts.Fee.Value != "50元/人" {
		t.Fatalf("value lost: %#v", merged.Facts.Fee)
	}
	if merged.Facts.Fee.Edition != "2026" {
		t.Fatalf("derived edition not written back: %#v", merged.Facts.Fee)
	}
}

// TestConsensusMixedEditionBoundary proves that within one group the first
// candidate may carry an explicit edition while the eventual representative has
// an empty Edition (with a year derivable from its evidence). The derived year
// must still be written back, independent of candidate order.
func TestConsensusMixedEditionBoundary(t *testing.T) {
	explicit := AIResult{
		SchemaVersion: AIAnalyzerVersion,
		Facts: AIFacts{Fee: AIFact{
			Value: "50元/人", Edition: "2026", Confidence: "low",
			Evidence: "报名费为50元/人",
		}},
	}
	derived := AIResult{
		SchemaVersion: AIAnalyzerVersion,
		Facts: AIFacts{Fee: AIFact{
			Value: "50元/人", Evidence: "2026年报名费为50元/人", Confidence: "high",
		}},
	}
	forward, conflictedForward := mergeAIChunkResults([]AIResult{explicit, derived})
	reversed, conflictedReversed := mergeAIChunkResults([]AIResult{derived, explicit})
	if forward.Facts.Fee != reversed.Facts.Fee {
		t.Fatalf("full AIFact depends on part order:\nA=%#v\nB=%#v", forward.Facts.Fee, reversed.Facts.Fee)
	}
	if !equalRejections(forward.Rejections, reversed.Rejections) {
		t.Fatalf("rejections depend on part order:\nA=%#v\nB=%#v", forward.Rejections, reversed.Rejections)
	}
	if !equalStrings(conflictedForward, conflictedReversed) {
		t.Fatalf("conflicted fields depend on order: %v vs %v", conflictedForward, conflictedReversed)
	}
	// The high-confidence candidate with empty Edition must win the
	// representative slot, and the key edition must be written back to it.
	if forward.Facts.Fee.Confidence != "high" {
		t.Fatalf("high-confidence candidate not selected as representative: %#v", forward.Facts.Fee)
	}
	if forward.Facts.Fee.Edition != "2026" {
		t.Fatalf("key edition not written back to representative: %#v", forward.Facts.Fee)
	}
}

// TestConsensusSingleNonEmptyValueNoMinorityRejection proves that one non-empty
// value alongside empty segments is not a conflict and produces no minority
// rejection.
func TestConsensusSingleNonEmptyValueNoMinorityRejection(t *testing.T) {
	parts := []AIResult{
		withRegistrationEnd("2026年9月20日"),
		{SchemaVersion: AIAnalyzerVersion},
		{SchemaVersion: AIAnalyzerVersion},
		{SchemaVersion: AIAnalyzerVersion},
	}
	merged, conflicted := mergeAIChunkResults(parts)
	if len(conflicted) != 0 {
		t.Fatalf("unexpected conflict: %v", conflicted)
	}
	if merged.Facts.RegistrationEnd.Value != "2026年9月20日" {
		t.Fatalf("single value not retained: %#v", merged.Facts.RegistrationEnd)
	}
	if hasRejectionReason(merged.Rejections, "minority conflicting values discarded by cross-segment consensus") {
		t.Fatalf("empty segments were treated as minority conflict: %#v", merged.Rejections)
	}
}

func TestAnalyzeRetainsStableFieldsOnPartialExtraction(t *testing.T) {
	location := time.FixedZone("CST", 8*3600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw := string(body)
		if strings.Contains(raw, "只判断") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"rejection_reason":""}`)))
			return
		}
		if strings.Contains(raw, "segment-a") {
			http.Error(w, "segmented model outage", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10","identity":{"edition":{"value":"2026","evidence":"2026全国大学生程序设计大赛","edition":"2026","confidence":"high"},"organizer":{"value":"中国计算机学会","evidence":"主办方：中国计算机学会","edition":"2026","confidence":"high"}},"facts":{"fee":{"value":"50元/人","evidence":"报名费为50元/人","edition":"2026","confidence":"high"}}}`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	analysis := New(config.Config{Location: location})
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, location)
	document := model.Document{
		Title: "2026全国大学生程序设计大赛报名通知",
		URL:   "https://example.edu.cn/contest/2026.htm",
		Text:  "面向全国高校公开报名，现已开放报名。主办方：中国计算机学会。报名费为50元/人。本段请求将失败。",
		Segments: []model.DocumentSegment{
			{ID: "segment-a", Kind: "html", Text: "本段请求将失败"},
			{ID: "segment-b", Kind: "html", Text: "面向全国高校公开报名，现已开放报名。主办方：中国计算机学会。报名费为50元/人。"},
		},
	}
	competition, relevant, err := analysis.Analyze(context.Background(), model.Candidate{Title: document.Title}, document, model.TrustHigh, now)
	if !relevant {
		t.Fatal("partial result was not considered relevant")
	}
	var pending *PendingCandidateError
	if !errors.As(err, &pending) {
		t.Fatalf("expected PendingCandidateError, got: %v", err)
	}
	if competition.Organizer != "中国计算机学会" || competition.Fee != "50元/人" {
		t.Fatalf("stable facts were not preserved: organizer=%q fee=%q", competition.Organizer, competition.Fee)
	}
	if competition.RegistrationPhase != model.RegistrationUnknown || competition.CompetitionPhase != model.CompetitionUnknown {
		t.Fatalf("lifecycle advanced on partial result: reg=%s comp=%s", competition.RegistrationPhase, competition.CompetitionPhase)
	}
	if competition.ExtractionAudit.Error == "" {
		t.Fatal("ExtractionAudit.Error was not set on partial result")
	}
	if !containsRejection(competition.ExtractionAudit.Rejections, "segments.segment-a") {
		t.Fatalf("failed segment not audited: %#v", competition.ExtractionAudit.Rejections)
	}
}

// withRegistrationEnd builds a valid extraction result carrying a single
// registration-end fact, useful for consensus tests.
func withRegistrationEnd(value string) AIResult {
	return AIResult{
		SchemaVersion: AIAnalyzerVersion,
		Facts: AIFacts{RegistrationEnd: AIFact{
			Value: value, Edition: "2026", Confidence: "high",
			Evidence: "报名截止" + value,
		}},
	}
}

func hasRejectionReason(rejections []model.AnalysisRejection, reason string) bool {
	for _, rejection := range rejections {
		if rejection.Reason == reason {
			return true
		}
	}
	return false
}

func equalRejections(left, right []model.AnalysisRejection) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Field != right[index].Field || left[index].Reason != right[index].Reason || left[index].Value != right[index].Value {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
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
