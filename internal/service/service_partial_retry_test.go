package service_test

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

// TestPartialResultIsRetriedOnNextScan proves that a partially analyzed page
// (one extraction segment failed) is persisted with its stable fields while
// lifecycle stays unknown, and that the identical content is re-analyzed on the
// next scan so the missing lifecycle facts can be recovered.
func TestPartialResultIsRetriedOnNextScan(t *testing.T) {
	// Registration (lifecycle) facts live in the first segment, stable facts in
	// the second. The document carries explicit segments so the LLM routing can
	// fail exactly one of them on the first scan.
	registrationText := "2026全国大学生程序设计大赛报名通知，本赛事面向全国高校公开报名，现已开放报名，报名时间为2026年8月1日至2026年9月20日。"
	stableText := "面向全国高校公开报名。主办方：中国计算机学会。报名费为50元/人。比赛内容为算法与程序设计。"
	doc := model.Document{
		Title: "2026全国大学生程序设计大赛报名通知",
		URL:   testPageBase,
		Text:  registrationText + stableText,
		Segments: []model.DocumentSegment{
			{ID: "html-1", Kind: "html", Text: registrationText},
			{ID: "html-2", Kind: "html", Text: stableText},
		},
	}

	var classificationCalls int32
	var extractionCalls int32
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw := string(body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(raw, "只判断") {
			atomic.AddInt32(&classificationCalls, 1)
			_, _ = io.WriteString(w, chatCompletionTestResponse(`{"schema_version":"competition-audit-v6","document_type":"official_announcement","source_role":"official_primary","computer_related":true,"competition_announcement":true,"rejection_reason":""}`))
			return
		}
		atomic.AddInt32(&extractionCalls, 1)
		// First scan: the registration (html-1) segment fails so lifecycle is
		// withheld. Subsequent scans succeed all segments.
		if atomic.LoadInt32(&classificationCalls) <= 1 && strings.Contains(raw, "html-1") {
			http.Error(w, "segmented model outage", http.StatusInternalServerError)
			return
		}
		var content string
		if strings.Contains(raw, "html-1") {
			content = `{"schema_version":"competition-audit-v6","identity":{"edition":{"value":"2026","evidence":"2026全国大学生程序设计大赛","edition":"2026","confidence":"high"}},"facts":{"registration_start":{"value":"2026年8月1日","evidence":"报名时间为2026年8月1日至2026年9月20日","edition":"2026","confidence":"high"},"registration_end":{"value":"2026年9月20日","evidence":"报名时间为2026年8月1日至2026年9月20日","edition":"2026","confidence":"high"}},"events":[{"type":"registration_opened","evidence":"本赛事面向全国高校公开报名，现已开放报名","edition":"2026","confidence":"high"}]}`
		} else {
			content = `{"schema_version":"competition-audit-v6","identity":{"edition":{"value":"2026","evidence":"2026全国大学生程序设计大赛","edition":"2026","confidence":"high"},"organizer":{"value":"中国计算机学会","evidence":"主办方：中国计算机学会","edition":"2026","confidence":"high"}},"facts":{"fee":{"value":"50元/人","evidence":"报名费为50元/人","edition":"2026","confidence":"high"}}}`
		}
		_, _ = io.WriteString(w, chatCompletionTestResponse(content))
	}))
	defer llm.Close()
	t.Setenv("OPENAI_BASE_URL", llm.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")

	app, database, _ := newPageService(t, func() model.Document { return doc }, "csp-retry", "CCF CSP", "high")
	defer database.Close()

	// First scan: partial result persisted, lifecycle unknown.
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	competitions, err := database.ListCompetitions(context.Background())
	if err != nil || len(competitions) != 1 {
		t.Fatalf("competitions=%d err=%v", len(competitions), err)
	}
	first := competitions[0]
	if first.Organizer != "中国计算机学会" || first.Fee != "50元/人" {
		t.Fatalf("stable fields not saved on first scan: organizer=%q fee=%q", first.Organizer, first.Fee)
	}
	if first.RegistrationPhase != model.RegistrationUnknown || first.RegistrationEnd != nil {
		t.Fatalf("lifecycle advanced on partial first scan: phase=%s end=%v", first.RegistrationPhase, first.RegistrationEnd)
	}

	// Second scan with identical content: the failed segment must be re-analyzed
	// and the missing lifecycle facts recovered.
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	competitions, err = database.ListCompetitions(context.Background())
	if err != nil || len(competitions) != 1 {
		t.Fatalf("competitions after retry=%d err=%v", len(competitions), err)
	}
	second := competitions[0]
	if second.RegistrationPhase != model.RegistrationOpen || second.RegistrationEnd == nil {
		t.Fatalf("lifecycle not recovered on retry: phase=%s end=%v", second.RegistrationPhase, second.RegistrationEnd)
	}
	// Extraction must have happened again on the second scan.
	if atomic.LoadInt32(&extractionCalls) <= 1 {
		t.Fatalf("expected extraction to re-run on retry, calls=%d", extractionCalls)
	}
}

func chatCompletionTestResponse(content string) string {
	return `{"id":"c","object":"chat.completion","model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"` + strings.ReplaceAll(content, `"`, `\"`) + `"},"finish_reason":"stop"}]}`
}
