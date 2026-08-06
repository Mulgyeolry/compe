package webapp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"competition-assistant/internal/authn"
	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
	"competition-assistant/internal/store"
	"competition-assistant/internal/subscription"
)

type capturedSender struct {
	recipient string
	subject   string
	body      string
	calls     int
}

func TestImmediateDeliveryDoesNotRequireScheduleFields(t *testing.T) {
	values := url.Values{
		"categories":         {"algorithm"},
		"organizer_types":    {"enterprise"},
		"competition_scopes": {"national"},
		"frequency":          {"immediate"},
		"timezone":           {"Asia/Shanghai"},
		"min_trust":          {"medium"},
		"notify_preview":     {"1"},
	}
	request := httptest.NewRequest(http.MethodPost, "/preferences", strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := request.ParseForm(); err != nil {
		t.Fatal(err)
	}
	preferences, err := parsePreferences(request, 1)
	if err != nil {
		t.Fatal(err)
	}
	if preferences.Frequency != model.DeliveryImmediate || preferences.DeliveryTime != "08:00" || preferences.WeeklyDay != time.Monday {
		t.Fatalf("unexpected immediate preferences: %#v", preferences)
	}
}

func (s *capturedSender) Send(context.Context, string, string) error { return nil }

func (s *capturedSender) SendTo(_ context.Context, recipient, subject, body string) error {
	s.recipient = recipient
	s.subject = subject
	s.body = body
	s.calls++
	return nil
}

func TestEmailLoginAndPreferenceUpdate(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	sender := &capturedSender{}
	webConfig := config.Web{
		Enabled: true, ListenAddr: ":8080", PublicBaseURL: "http://example.test",
		AppSecret: "0123456789abcdef0123456789abcdef", AppriseSenderURL: "mailtos://sender:code@smtp.example.test:465",
		VerificationTTL: 10 * time.Minute, SessionTTL: 30 * 24 * time.Hour,
	}
	server, err := New(database, sender, manager, webConfig, time.FixedZone("CST", 8*3600), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 8, 3, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
	server.SetNow(func() time.Time { return fixedNow })
	pushStarts := 0
	var pushedUserID int64
	server.SetPushTrigger(func(userID int64) bool {
		pushStarts++
		pushedUserID = userID
		return true
	})
	backfillCalls := 0
	var backfilledUserID int64
	server.SetBackfillTrigger(func(_ context.Context, userID int64) (int, error) {
		backfillCalls++
		backfilledUserID = userID
		return 2, nil
	})
	choiceCalls := 0
	server.SetCompetitionChoiceTrigger(func(ctx context.Context, userID, competitionID int64, decision model.ParticipationDecision) error {
		choiceCalls++
		return database.SetUserCompetitionDecision(ctx, userID, competitionID, decision, fixedNow)
	})
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	loginBody := getBody(t, client, httpServer.URL+"/")
	if !strings.Contains(loginBody, "不错过值得参加的") || strings.Contains(loginBody, "事实保护") || strings.Contains(loginBody, "AI API Key") || strings.Contains(loginBody, "邮箱授权码") {
		t.Fatalf("login page contains stale or unnecessary copy: %s", loginBody)
	}
	guestCSRF := hiddenValue(t, loginBody, "csrf")
	response := postForm(t, client, httpServer.URL+"/auth/request", url.Values{"csrf": {guestCSRF}, "email": {"Student@Example.com"}})
	verificationPage := readResponse(t, response)
	if response.StatusCode != http.StatusOK || sender.recipient != "student@example.com" {
		t.Fatalf("request code status=%d recipient=%q body=%s", response.StatusCode, sender.recipient, verificationPage)
	}
	codeMatch := regexp.MustCompile(`>([0-9]{6})<`).FindStringSubmatch(sender.body)
	if len(codeMatch) != 2 {
		t.Fatalf("verification code not found in email: %s", sender.body)
	}
	verifyCSRF := hiddenValue(t, verificationPage, "csrf")
	response = postForm(t, client, httpServer.URL+"/auth/verify", url.Values{
		"csrf": {verifyCSRF}, "email": {"student@example.com"}, "code": {codeMatch[1]},
	})
	preferencesPage := readResponse(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(response.Request.URL.Path, "/preferences") {
		t.Fatalf("verify flow ended at %s status=%d body=%s", response.Request.URL, response.StatusCode, preferencesPage)
	}

	sessionCSRF := hiddenValue(t, preferencesPage, "csrf")
	values := url.Values{
		"csrf": {sessionCSRF}, "categories": {"algorithm", "ai_data"}, "frequency": {"weekly"},
		"organizer_types": {"enterprise", "government_society"}, "competition_scopes": {"national", "regional"},
		"regions":       {"重庆，四川"},
		"delivery_time": {"09:30"}, "weekly_day": {"5"}, "timezone": {"Asia/Shanghai"}, "min_trust": {"high"},
		"include_keywords": {"RAG，Go 后端"}, "exclude_keywords": {"中学生"}, "notify_preview": {"1"},
		"notify_registration": {"1"}, "allow_eligibility_risk": {"1"},
	}
	response = postForm(t, client, httpServer.URL+"/preferences", values)
	savedPage := readResponse(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(savedPage, "设置已保存") {
		t.Fatalf("save preferences status=%d body=%s", response.StatusCode, savedPage)
	}
	if backfillCalls != 1 || backfilledUserID < 1 || !strings.Contains(savedPage, "已补充 2 条") {
		t.Fatalf("preference backfill was not triggered: calls=%d user=%d body=%s", backfillCalls, backfilledUserID, savedPage)
	}
	users, err := database.ListNotificationUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatalf("users=%d err=%v", len(users), err)
	}
	preferences := users[0].Preferences
	if preferences.Frequency != "weekly" || preferences.DeliveryTime != "09:30" || preferences.NotifyUpcoming || preferences.NotifyStarted || len(preferences.Categories) != 2 || len(preferences.IncludeKeywords) != 2 || len(preferences.OrganizerTypes) != 2 || len(preferences.CompetitionScopes) != 2 || len(preferences.Regions) != 2 {
		t.Fatalf("preferences not persisted: %#v", preferences)
	}
	competition := model.Competition{EntityKey: "web-push-test", Name: "2026全国AI Agent开发大赛", Status: model.StatusPreview, StatusEvidence: "即将启动", OfficialURL: "https://example.org/competition", Trust: model.TrustHigh}
	if _, _, err := database.UpsertCompetition(context.Background(), competition, "test", fixedNow); err != nil {
		t.Fatal(err)
	}
	savedCompetition, err := database.GetCompetition(context.Background(), competition.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := store.UserEventDispatch{UserID: users[0].User.ID, Event: model.Event{Type: "preview_detected", Key: "preview"}, GroupKey: "future", DueAt: fixedNow.Add(24 * time.Hour)}
	if err := database.CommitCompetitionEvents(context.Background(), savedCompetition.ID, []model.Event{dispatch.Event}, []store.UserEventDispatch{dispatch}, fixedNow); err != nil {
		t.Fatal(err)
	}

	dashboardPage := getBody(t, client, httpServer.URL+"/dashboard")
	if !strings.Contains(dashboardPage, "比赛资讯助手") || !strings.Contains(dashboardPage, "待发送通知") {
		t.Fatalf("dashboard is missing the updated brand or pending count: %s", dashboardPage)
	}
	dashboardCSRF := hiddenValue(t, dashboardPage, "csrf")
	response = postForm(t, client, httpServer.URL+"/competitions/choice", url.Values{"csrf": {dashboardCSRF}, "competition_id": {fmt.Sprint(savedCompetition.ID)}, "decision": {string(model.ParticipationParticipating)}})
	choicePage := readResponse(t, response)
	if choiceCalls != 1 || !strings.Contains(choicePage, "参赛选择已保存") {
		t.Fatalf("website choice calls=%d body=%s", choiceCalls, choicePage)
	}
	if decision, err := database.GetUserCompetitionDecision(context.Background(), users[0].User.ID, savedCompetition.ID); err != nil || decision != model.ParticipationParticipating {
		t.Fatalf("website choice=%q err=%v", decision, err)
	}
	response = postForm(t, client, httpServer.URL+"/actions/test-email", url.Values{"csrf": {dashboardCSRF}})
	testMailPage := readResponse(t, response)
	if response.StatusCode != http.StatusOK || sender.calls != 2 || !strings.Contains(sender.subject, "测试邮件") || !strings.Contains(testMailPage, "测试邮件已提交") {
		t.Fatalf("test mail calls=%d subject=%q status=%d body=%s", sender.calls, sender.subject, response.StatusCode, testMailPage)
	}
	response = postForm(t, client, httpServer.URL+"/actions/test-email", url.Values{"csrf": {dashboardCSRF}})
	rateLimitPage := readResponse(t, response)
	if sender.calls != 2 || !strings.Contains(rateLimitPage, "发送过于频繁") {
		t.Fatalf("test email rate limit failed: calls=%d body=%s", sender.calls, rateLimitPage)
	}
	response = postForm(t, client, httpServer.URL+"/actions/push", url.Values{"csrf": {dashboardCSRF}})
	pushPage := readResponse(t, response)
	if pushStarts != 1 || pushedUserID != users[0].User.ID || !strings.Contains(pushPage, "正在推送当前待发送通知") {
		t.Fatalf("push starts=%d user=%d body=%s", pushStarts, pushedUserID, pushPage)
	}
	declineToken := manager.CompetitionChoiceToken(users[0].User.ID, savedCompetition.ID, string(model.ParticipationDeclined))
	confirmationPage := getBody(t, client, httpServer.URL+"/competition-choice?token="+url.QueryEscape(declineToken))
	if !strings.Contains(confirmationPage, "确认参赛选择") {
		t.Fatalf("email choice confirmation missing: %s", confirmationPage)
	}
	if decision, err := database.GetUserCompetitionDecision(context.Background(), users[0].User.ID, savedCompetition.ID); err != nil || decision != model.ParticipationParticipating {
		t.Fatalf("GET email link changed state: choice=%q err=%v", decision, err)
	}
	response = postForm(t, client, httpServer.URL+"/competition-choice", url.Values{"token": {declineToken}})
	confirmationSaved := readResponse(t, response)
	if choiceCalls != 2 || !strings.Contains(confirmationSaved, "参赛选择已保存") {
		t.Fatalf("email choice calls=%d body=%s", choiceCalls, confirmationSaved)
	}
	response = postForm(t, client, httpServer.URL+"/actions/scan", url.Values{"csrf": {dashboardCSRF}})
	if (response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusMethodNotAllowed) || pushStarts != 1 {
		t.Fatalf("removed scan action is still reachable: status=%d pushStarts=%d", response.StatusCode, pushStarts)
	}
}

func getBody(t *testing.T, client *http.Client, address string) string {
	t.Helper()
	response, err := client.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	return readResponse(t, response)
}

func postForm(t *testing.T, client *http.Client, address string, values url.Values) *http.Response {
	t.Helper()
	response, err := client.PostForm(address, values)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func readResponse(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func hiddenValue(t *testing.T, body, name string) string {
	t.Helper()
	pattern := regexp.MustCompile(`name="` + regexp.QuoteMeta(name) + `" value="([^"]+)"`)
	match := pattern.FindStringSubmatch(body)
	if len(match) != 2 {
		t.Fatalf("hidden field %q not found in body: %s", name, body)
	}
	return match[1]
}

func TestDashboardShowsUnconfirmedSection(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "dashboard-unconfirmed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 8, 4, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))

	unconfirmed := model.Competition{
		EntityKey:   "unconfirmed-key",
		Name:        "2026云原生开发者大赛",
		OfficialURL: "https://example.org/cloud-native",
		Trust:       model.TrustMedium,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, unconfirmed, "test", fixedNow); err != nil || !isNew {
		t.Fatalf("insert unconfirmed isNew=%v err=%v", isNew, err)
	}
	preview := model.Competition{
		EntityKey:      "preview-key",
		Name:           "2027华为ICT大赛预告",
		Status:         model.StatusPreview,
		StatusEvidence: "即将启动",
		OfficialURL:    "https://example.org/huawei",
		Trust:          model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, preview, "test", fixedNow); err != nil || !isNew {
		t.Fatalf("insert preview isNew=%v err=%v", isNew, err)
	}

	email := "student@example.com"
	code := "123456"
	if err := database.RequestVerification(ctx, email, manager.VerificationHash(email, code), fixedNow, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	user, err := database.ConsumeVerification(ctx, email, manager.VerificationHash(email, code), fixedNow, subscription.CategoryIDs())
	if err != nil {
		t.Fatal(err)
	}
	sessionToken := "dashboard-test-session"
	if err := database.CreateSession(ctx, user.ID, manager.SessionHash(sessionToken), fixedNow.Add(24*time.Hour), fixedNow); err != nil {
		t.Fatal(err)
	}

	server, err := New(database, &capturedSender{}, manager, config.Web{Enabled: true, PublicBaseURL: "http://example.test"}, time.FixedZone("CST", 8*3600), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server.SetNow(func() time.Time { return fixedNow })
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	jar, _ := cookiejar.New(nil)
	parsedURL, _ := url.Parse(httpServer.URL)
	jar.SetCookies(parsedURL, []*http.Cookie{{Name: sessionCookie, Value: sessionToken, Path: "/"}})
	client := &http.Client{Jar: jar}

	page := getBody(t, client, httpServer.URL+"/dashboard")
	if !strings.Contains(page, "待确认赛事") {
		t.Fatalf("dashboard missing unconfirmed section: %s", page)
	}
	latestIndex := strings.Index(page, "最新赛事")
	unconfirmedIndex := strings.Index(page, "待确认赛事")
	previewIndex := strings.Index(page, "2027华为ICT大赛预告")
	unknownIndex := strings.Index(page, "2026云原生开发者大赛")
	if latestIndex < 0 || unconfirmedIndex < 0 || previewIndex < 0 || unknownIndex < 0 {
		t.Fatalf("dashboard sections not rendered: latest=%d unconfirmed=%d preview=%d unknown=%d", latestIndex, unconfirmedIndex, previewIndex, unknownIndex)
	}
	if !(latestIndex < previewIndex && previewIndex < unconfirmedIndex && unconfirmedIndex < unknownIndex) {
		t.Fatalf("confirmed and unconfirmed competitions are mixed up: latest=%d preview=%d unconfirmed=%d unknown=%d", latestIndex, previewIndex, unconfirmedIndex, unknownIndex)
	}
}

// TestWebUsesInjectedTimezone proves the Web layer uses the location passed to
// New instead of a hardcoded UTC+8 zone. The test-mail timestamp is rendered in
// America/New_York (EDT during a summer date), so the body must show EDT and
// never CST.
func TestWebUsesInjectedTimezone(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "web-timezone.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 8, 3, 8, 0, 0, 0, ny)

	email := "student@example.com"
	code := "123456"
	if err := database.RequestVerification(ctx, email, manager.VerificationHash(email, code), fixedNow, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	user, err := database.ConsumeVerification(ctx, email, manager.VerificationHash(email, code), fixedNow, subscription.CategoryIDs())
	if err != nil {
		t.Fatal(err)
	}
	sessionToken := "timezone-test-session"
	if err := database.CreateSession(ctx, user.ID, manager.SessionHash(sessionToken), fixedNow.Add(24*time.Hour), fixedNow); err != nil {
		t.Fatal(err)
	}

	sender := &capturedSender{}
	server, err := New(database, sender, manager, config.Web{Enabled: true, PublicBaseURL: "http://example.test"}, ny, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server.SetNow(func() time.Time { return fixedNow })
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	jar, _ := cookiejar.New(nil)
	parsedURL, _ := url.Parse(httpServer.URL)
	jar.SetCookies(parsedURL, []*http.Cookie{{Name: sessionCookie, Value: sessionToken, Path: "/"}})
	client := &http.Client{Jar: jar}

	dashboardPage := getBody(t, client, httpServer.URL+"/dashboard")
	dashboardCSRF := hiddenValue(t, dashboardPage, "csrf")
	response := postForm(t, client, httpServer.URL+"/actions/test-email", url.Values{"csrf": {dashboardCSRF}})
	_ = readResponse(t, response)
	if sender.calls != 1 {
		t.Fatalf("test mail calls=%d, want 1", sender.calls)
	}
	if !strings.Contains(sender.body, "EDT") {
		t.Fatalf("test mail timestamp not rendered in injected timezone (America/New_York): %q", sender.body)
	}
	if strings.Contains(sender.body, "CST") {
		t.Fatalf("test mail timestamp still uses hardcoded UTC+8: %q", sender.body)
	}
}

// TestHealthAndReadiness verifies /healthz stays up regardless of database
// state while /readyz reflects database availability, without leaking internal
// error details in the failure response.
func TestHealthAndReadiness(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "readiness.db"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(database, &capturedSender{}, manager, config.Web{Enabled: true, PublicBaseURL: "http://example.test"}, time.UTC, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := httpServer.Client()

	// Healthy database: both endpoints report ready.
	assertPlainResponse(t, client, httpServer.URL+"/healthz", http.StatusOK, "ok\n")
	assertPlainResponse(t, client, httpServer.URL+"/readyz", http.StatusOK, "ready\n")

	// Shut the database down: liveness stays up, readiness becomes 503.
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	assertPlainResponse(t, client, httpServer.URL+"/healthz", http.StatusOK, "ok\n")
	body := assertPlainResponse(t, client, httpServer.URL+"/readyz", http.StatusServiceUnavailable, "not ready\n")
	if strings.Contains(body, "sqlite") || strings.Contains(body, "ping failed") {
		t.Fatalf("readiness failure leaked internal database details: %q", body)
	}
}

func assertPlainResponse(t *testing.T, client *http.Client, address string, wantStatus int, wantBody string) string {
	t.Helper()
	response, err := client.Get(address)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != wantStatus {
		t.Fatalf("%s status=%d, want %d", address, response.StatusCode, wantStatus)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Fatalf("%s content-type=%q, want %q", address, contentType, "text/plain; charset=utf-8")
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != wantBody {
		t.Fatalf("%s body=%q, want %q", address, string(body), wantBody)
	}
	return string(body)
}
