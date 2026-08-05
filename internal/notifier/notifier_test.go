package notifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"competition-assistant/internal/model"
)

func TestAppriseNoConfigurationIsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := NewApprise(server.URL).Send(context.Background(), "test", "body"); err == nil {
		t.Fatal("HTTP 204 must remain retryable instead of being marked sent")
	}
}

func TestRenderTestMail(t *testing.T) {
	subject, body, err := RenderTest(time.Date(2026, 8, 4, 13, 30, 0, 0, time.FixedZone("CST", 8*3600)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(subject, "比赛资讯助手") || !strings.Contains(body, "2026-08-04 13:30:00") || !strings.Contains(body, "不会创建赛事通知记录") {
		t.Fatalf("unexpected test mail subject=%q body=%s", subject, body)
	}
}

func TestAppriseSendToOverridesRecipient(t *testing.T) {
	var payload map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	senderURL := "mailto://sender:authorization-code@smtp.example.com:465/?from=sender%40example.com&to=old%40example.com"
	if err := NewApprise(server.URL, senderURL).SendTo(context.Background(), "new@example.com", "test", "body"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload["urls"], "to=new%40example.com") {
		t.Fatalf("recipient was not replaced: %q", payload["urls"])
	}
	if !strings.Contains(payload["urls"], "authorization-code") {
		t.Fatal("sender credential was unexpectedly changed")
	}
}

func TestRenderUserDeliveryIncludesPreviewNotice(t *testing.T) {
	group := model.UserDeliveryGroup{Items: []model.UserNotificationItem{{
		Competition: model.Competition{
			ID: 1, Name: "华为开发者大赛", Status: model.StatusPreview, Fee: "50元/队", OfficialURL: "https://example.com", Trust: model.TrustHigh,
			Keywords: []string{"云原生", "后端开发"},
			Analysis: model.CompetitionAnalysis{Summary: "围绕工程实践完成参赛作品", Confidence: "medium", References: []model.AnalysisReference{{Title: "参赛说明", URL: "https://example.com/research", Evidence: "工程实践"}}},
		},
		Event: model.Event{Type: "preview_detected", Key: "preview"},
	}}}
	_, body, err := RenderUserDelivery(group, "https://example.com/preferences", "https://example.com/unsubscribe", map[int64]CompetitionChoiceLinks{1: {ParticipateURL: "https://example.com/yes", DeclineURL: "https://example.com/no"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "目前是预告，尚未正式开放报名。") {
		t.Fatal("required preview notice is missing")
	}
	if !strings.Contains(body, "比赛费用") || !strings.Contains(body, "50元/队") {
		t.Fatal("competition fee is missing")
	}
	if !strings.Contains(body, "AI 赛事分析") || !strings.Contains(body, "后端开发") || !strings.Contains(body, "https://example.com/research") {
		t.Fatal("traceable competition analysis is missing")
	}
}

func TestRenderUserDeliveryDoesNotEmitBrokenLocalActionLinks(t *testing.T) {
	group := model.UserDeliveryGroup{Items: []model.UserNotificationItem{{
		Competition: model.Competition{ID: 1, Name: "程序设计大赛", Status: model.StatusRegistrationOpen, OfficialURL: "https://example.com", Trust: model.TrustHigh},
		Event:       model.Event{Type: "registration_opened", Key: "open"},
	}}}
	_, body, err := RenderUserDelivery(group, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, `href=""`) || strings.Contains(body, "参加比赛</a>") || !strings.Contains(body, "当前是本地或内网部署") {
		t.Fatalf("local delivery exposed broken action links: %s", body)
	}
}

func TestRenderSourceAlertListsEveryProblem(t *testing.T) {
	subject, body, err := RenderSourceAlert([]SourceHealthProblem{
		{ID: "ccf-csp", Name: "CCF CSP 官网", FailureCount: 5, FailureLimit: 3},
		{ID: "tianchi", Name: "阿里云天池", FailureCount: 3, FailureLimit: 3},
	}, "2026-08-05 20:00:00 CST")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(subject, "数据源连续失败告警") {
		t.Fatalf("unexpected subject %q", subject)
	}
	for _, marker := range []string{"CCF CSP 官网", "阿里云天池", "连续失败 5 次", "连续失败 3 次", "2026-08-05 20:00:00"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("source alert body missing %q: %s", marker, body)
		}
	}
}

func TestRenderSourceAlertRejectsEmptyProblems(t *testing.T) {
	if _, _, err := RenderSourceAlert(nil, "now"); err == nil {
		t.Fatal("empty problem list must be rejected")
	}
}
