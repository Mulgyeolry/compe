package analyzer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"competition-assistant/internal/model"
)

// captureRequestBody starts a server that records every request body before
// replying with the given completion content, so the HTTP payload can be
// asserted without trusting the Go structs.
func captureRequestBody(t *testing.T, content string) (*httptest.Server, *[]string) {
	t.Helper()
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionResponse(content)))
	}))
	t.Cleanup(server.Close)
	return server, &bodies
}

func TestChatParamsRequestBodyDisablesThinking(t *testing.T) {
	// Test A: the outgoing HTTP JSON body must carry both
	// "thinking":{"type":"disabled"} and "response_format":{"type":"json_object"}.
	content := `{"schema_version":"competition-audit-v7","document_type":"listing","source_role":"official_primary","computer_related":false,"competition_announcement":false,"rejection_reason":"列表页"}`
	server, bodies := captureRequestBody(t, content)
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()

	if _, _, err := modelClient.Classify(context.Background(), model.Candidate{Title: "竞赛列表"}, model.Document{Title: "竞赛", Text: "列表"}); err != nil {
		t.Fatalf("classify: %v", err)
	}
	if len(*bodies) == 0 {
		t.Fatal("no HTTP request body captured")
	}
	body := (*bodies)[0]
	if !strings.Contains(body, `"thinking":{"type":"disabled"}`) {
		t.Errorf("request body missing thinking disabled: %s", body)
	}
	if !strings.Contains(body, `"response_format":{"type":"json_object"}`) {
		t.Errorf("request body missing response_format json_object: %s", body)
	}
	if strings.Contains(body, `"reasoning_content"`) {
		t.Errorf("request body should not reference reasoning_content: %s", body)
	}
}

func TestClassifyRetriesOnceOnEmptyContentThenSucceeds(t *testing.T) {
	// Test B: first response is an empty message.content, the retry returns a
	// valid classification JSON. Exactly 2 calls, success, no failure.
	valid := `{"schema_version":"competition-audit-v7","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"rejection_reason":""}`
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(chatCompletionResponse("")))
			return
		}
		_, _ = w.Write([]byte(chatCompletionResponse(valid)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()

	classification, raw, err := modelClient.Classify(context.Background(), model.Candidate{Title: "华为杯"}, model.Document{Title: "2026华为杯", Text: "报名"})
	if err != nil {
		t.Fatalf("classify should recover after one empty response: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("endpoint calls=%d, want exactly 2", calls.Load())
	}
	if !classification.CompetitionAnnouncement {
		t.Errorf("expected a valid announcement classification: %#v", classification)
	}
	if raw == "" {
		t.Error("raw classification response not retained")
	}
	if !modelClient.Enabled() {
		t.Error("circuit should not be tripped after a successful retry")
	}
}

func TestClassifyPersistentEmptyContentFailsAfterTwoCalls(t *testing.T) {
	// Test C: both responses are empty content. Exactly 2 calls, a clear
	// empty-content error, and no unbounded retry.
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionResponse("")))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()

	_, _, err := modelClient.Classify(context.Background(), model.Candidate{Title: "华为杯"}, model.Document{Title: "2026华为杯", Text: "报名"})
	if err == nil {
		t.Fatal("expected persistent empty content to fail")
	}
	if !strings.Contains(err.Error(), "empty classification content") {
		t.Errorf("error should be a clear empty-content error, got: %v", err)
	}
	if strings.Contains(err.Error(), "EOF") {
		t.Errorf("empty content must not surface as a vague EOF error, got: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("endpoint calls=%d, want exactly 2 (bounded retry)", calls.Load())
	}
}

func TestEnrichSegmentRetriesOnceOnEmptyContentThenSucceeds(t *testing.T) {
	// Test D: a single extraction segment gets an empty first response and a
	// valid second one; the segment succeeds and is not counted as failed.
	valid := `{"schema_version":"competition-audit-v7","identity":{"organizer":{"value":"主办方","evidence":"主办方：组委会","edition":"2026","confidence":"high"}},"facts":{},"events":[]}`
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(chatCompletionResponse("")))
			return
		}
		_, _ = w.Write([]byte(chatCompletionResponse(valid)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()

	result, raw, err := modelClient.enrichSegment(context.Background(),
		model.Candidate{Title: "华为杯"},
		model.Document{URL: "https://example.com/x", Title: "2026华为杯", Text: "主办方：组委会"},
		model.DocumentSegment{ID: "seg-1", Kind: "text", Text: "主办方：组委会"})
	if err != nil {
		t.Fatalf("enrichSegment should recover after one empty response: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("endpoint calls=%d, want exactly 2", calls.Load())
	}
	if result.Identity.Organizer.Value != "主办方" {
		t.Errorf("expected organizer extracted, got %q", result.Identity.Organizer.Value)
	}
	if raw == "" {
		t.Error("raw extraction content not retained")
	}
}

func TestClassifyNonEmptyInvalidJSONIsNotRetried(t *testing.T) {
	// Test E: a non-empty but invalid JSON content must NOT be blindly retried.
	// It keeps the existing validation/deferred behaviour (single call).
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()

	if _, _, err := modelClient.Classify(context.Background(), model.Candidate{Title: "华为杯"}, model.Document{Title: "2026华为杯", Text: "报名"}); err == nil {
		t.Fatal("expected invalid JSON to fail")
	}
	if calls.Load() != 1 {
		t.Errorf("endpoint calls=%d, want 1 (non-empty invalid JSON must not retry)", calls.Load())
	}
}
