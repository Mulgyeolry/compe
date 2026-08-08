package analyzer

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestLLMConsecutiveFailuresOpenCircuit(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "model unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()
	document := model.Document{URL: "https://example.com/contest", Title: "AI Agent 大赛", Text: "报名预告"}
	for attempt := 1; attempt <= 3; attempt++ {
		if _, err := modelClient.Enrich(context.Background(), model.Candidate{Title: document.Title}, document); err == nil {
			t.Fatalf("expected model call %d to fail", attempt)
		}
		if attempt < 3 && !modelClient.Enabled() {
			t.Fatalf("model circuit opened after only %d failure(s)", attempt)
		}
	}
	if modelClient.Enabled() {
		t.Fatal("model circuit remained enabled after three consecutive failures")
	}
	if _, err := modelClient.Enrich(context.Background(), model.Candidate{Title: document.Title}, document); err == nil {
		t.Fatal("expected open circuit to reject another request")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("model endpoint calls=%d, want 3", got)
	}
}

func chatCompletionResponse(content string) string {
	encoded, _ := json.Marshal(content)
	return fmt.Sprintf(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, encoded)
}

func TestClassifyParsesTinyJudgmentResponse(t *testing.T) {
	content := `{"schema_version":"competition-audit-v10","document_type":"listing","source_role":"official_primary","computer_related":false,"competition_announcement":false,"rejection_reason":"聚合列表页"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionResponse(content)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()
	classification, raw, err := modelClient.Classify(context.Background(), model.Candidate{Title: "竞赛动态列表"}, model.Document{Title: "竞赛动态", Text: "赛事列表"})
	if err != nil {
		t.Fatal(err)
	}
	if classification.DocumentType != DocumentListing || classification.CompetitionAnnouncement || classification.RejectionReason == "" {
		t.Fatalf("unexpected classification: %#v", classification)
	}
	if raw == "" {
		t.Fatal("classification raw response was not retained")
	}
}

func TestClassifyRejectsUnknownFields(t *testing.T) {
	content := `{"schema_version":"competition-audit-v10","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"identity":{"name":"x"}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionResponse(content)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()
	if _, _, err := modelClient.Classify(context.Background(), model.Candidate{}, model.Document{}); err == nil {
		t.Fatal("classification with unknown fields was accepted")
	}
}

func TestClassifyFailureCountsTowardCircuitBreaker(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "model unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	modelClient := NewLLMFromEnvironment()
	for attempt := 1; attempt <= 3; attempt++ {
		if _, _, err := modelClient.Classify(context.Background(), model.Candidate{}, model.Document{}); err == nil {
			t.Fatalf("expected classification call %d to fail", attempt)
		}
	}
	if modelClient.Enabled() {
		t.Fatal("circuit stayed open after three classification failures")
	}
}

// TestAnalyzeUsesClassificationGateBeforeExtraction proves the two-pass
// pipeline: a low-value page (listing, campus, post-event) is rejected after
// exactly one tiny classification request, while a genuine announcement pays
// exactly one additional extraction request.
func TestAnalyzeUsesClassificationGateBeforeExtraction(t *testing.T) {
	location := time.FixedZone("CST", 8*3600)
	requests := make(chan string, 16)
	var classificationCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- string(body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "只判断") {
			if classificationCalls.Add(1) == 1 {
				_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10","document_type":"listing","source_role":"official_primary","computer_related":false,"competition_announcement":false,"rejection_reason":"聚合列表页"}`)))
				return
			}
			_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"rejection_reason":""}`)))
			return
		}
		_, _ = w.Write([]byte(chatCompletionResponse(`{"schema_version":"competition-audit-v10","identity":{"name":{"value":"2026全国大学生程序设计大赛","evidence":"2026全国大学生程序设计大赛","edition":"2026","confidence":"high"},"organizer":{"value":"中国计算机学会","evidence":"主办方：中国计算机学会","edition":"2026","confidence":"high"}},"facts":{"fee":{"value":"50元/人","evidence":"报名费为50元/人","edition":"2026","confidence":"high"}},"events":[{"type":"registration_opened","evidence":"本赛事面向全国高校公开报名，现已开放报名","edition":"2026","confidence":"high"}]}`)))
	}))
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	analysis := New(config.Config{Location: location})
	now := time.Date(2026, 8, 4, 20, 0, 0, 0, location)

	listing := model.Document{
		Title: "计算机竞赛活动列表",
		URL:   "https://example.edu.cn/contest/list.htm",
		Text:  "这里是计算机竞赛活动列表，包含多个比赛的链接，点击查看详情。",
	}
	_, relevant, err := analysis.Analyze(context.Background(), model.Candidate{Title: listing.Title}, listing, model.TrustMedium, now)
	if err != nil {
		t.Fatal(err)
	}
	if relevant {
		t.Fatal("listing page passed the classification gate")
	}
	if got := len(requests); got != 1 {
		t.Fatalf("listing page made %d model requests, want exactly 1", got)
	}
	<-requests

	announcement := model.Document{
		Title: "2026全国大学生程序设计大赛报名通知",
		URL:   "https://example.edu.cn/contest/2026.htm",
		Text:  "2026全国大学生程序设计大赛报名通知。本赛事面向全国高校公开报名，现已开放报名。报名费为50元/人。主办方：中国计算机学会。比赛内容为程序设计与算法。",
	}
	competition, relevant, err := analysis.Analyze(context.Background(), model.Candidate{Title: announcement.Title}, announcement, model.TrustHigh, now)
	if err != nil {
		t.Fatal(err)
	}
	if !relevant {
		t.Fatal("official announcement failed the classification gate")
	}
	if got := len(requests); got != 2 {
		t.Fatalf("announcement made %d model requests, want exactly 2 (classification + extraction)", got)
	}
	if competition.Name != "2026全国大学生程序设计大赛" || competition.Fee != "50元/人" {
		t.Fatalf("extracted facts missing: name=%q fee=%q", competition.Name, competition.Fee)
	}
	if competition.RegistrationPhase != model.RegistrationOpen {
		t.Fatalf("registration phase=%s, want open", competition.RegistrationPhase)
	}
	if len(competition.ExtractionAudit.RawResponses) != 2 {
		t.Fatalf("audit retained %d raw responses, want 2 (classification + extraction)", len(competition.ExtractionAudit.RawResponses))
	}
}
