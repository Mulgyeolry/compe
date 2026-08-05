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
	// Registration (lifecycle) facts live in the first paragraph, stable facts
	// in the second. The page is long enough to be split into two analysis
	// segments by the fetcher.
	registrationParagraph := "2026全国大学生程序设计大赛报名通知，本赛事面向全国高校公开报名，现已开放报名，报名时间为2026年8月1日至2026年9月20日。"
	stableParagraph := "主办方：中国计算机学会。报名费为50元/人。比赛内容为算法与程序设计。"
	// Pad both paragraphs so their combined rune count exceeds the 2800-rune
	// chunk boundary, guaranteeing two analysis segments.
	registrationParagraph = registrationParagraph + strings.Repeat("本次比赛面向广大高校学生开放，欢迎踊跃报名。", 80)
	stableParagraph = stableParagraph + strings.Repeat("大赛关注计算机核心能力的培养与实战。", 80)

	page := "<html><head><title>2026全国大学生程序设计大赛报名通知</title></head><body>" +
		"<p>" + registrationParagraph + "</p><p>" + stableParagraph + "</p></body></html>"

	var classificationCalls int32
	var extractionCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/competition", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, page)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
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
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")

	app, database, _ := newPageService(t, server.URL+"/competition", "csp-retry", "CCF CSP", "high")
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
