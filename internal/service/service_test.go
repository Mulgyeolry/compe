package service_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"competition-assistant/internal/analyzer"
	"competition-assistant/internal/authn"
	"competition-assistant/internal/config"
	"competition-assistant/internal/fetcher"
	"competition-assistant/internal/model"
	"competition-assistant/internal/service"
	"competition-assistant/internal/store"
	"competition-assistant/internal/subscription"
)

type sentMail struct{ recipient, subject, body string }

type memorySender struct {
	mu      sync.Mutex
	mails   []sentMail
	fail    bool
	attempt int
}

func (m *memorySender) Send(_ context.Context, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempt++
	if m.fail {
		return errors.New("simulated mail failure")
	}
	m.mails = append(m.mails, sentMail{subject: subject, body: body})
	return nil
}

func (m *memorySender) SendTo(_ context.Context, recipient, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempt++
	if m.fail {
		return errors.New("simulated mail failure")
	}
	m.mails = append(m.mails, sentMail{recipient: recipient, subject: subject, body: body})
	return nil
}

func (m *memorySender) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.mails)
}

func TestCSPRegistrationAndDuplicateSuppression(t *testing.T) {
	page := `<html><head><title>第43次 CCF CSP 认证报名通知</title></head><body>
<p>主办方：中国计算机学会。</p><p>报名已经开始。</p><p>报名时间：2026年8月3日 09:00至2026年8月20日 23:59。</p>
<p>可单人参赛。比赛内容为算法和程序设计。</p></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, page) }))
	defer server.Close()

	app, database, sender := newPageService(t, server.URL, "csp", "CCF CSP", "high")
	defer database.Close()
	ctx := context.Background()
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 || !strings.Contains(sender.mails[0].body, "报名中") || !strings.Contains(sender.mails[0].body, "23:59") || !strings.Contains(sender.mails[0].body, server.URL) {
		t.Fatalf("unexpected first notification: %#v", sender.mails)
	}
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatalf("same content sent repeatedly: %d mails", sender.count())
	}
}

func TestPreviewBecomesRegistration(t *testing.T) {
	var mu sync.RWMutex
	page := `<html><head><title>2026 华为 ICT 大赛</title></head><body><p>主办方：华为技术有限公司。</p>
<p>新一届赛事即将启动，预计2026年9月开放报名，敬请期待。</p><p>比赛聚焦云计算和人工智能。</p></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		_, _ = io.WriteString(w, page)
	}))
	defer server.Close()
	app, database, sender := newPageService(t, server.URL, "huawei", "华为 ICT", "high")
	defer database.Close()
	ctx := context.Background()
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 || !strings.Contains(sender.mails[0].body, "目前是预告，尚未正式开放报名。") {
		t.Fatalf("preview notice missing: %#v", sender.mails)
	}
	mu.Lock()
	page = `<html><head><title>2026 华为 ICT 大赛</title></head><body><p>主办方：华为技术有限公司。</p>
<p>现已开放报名，报名时间：2026年8月4日至2026年10月20日。</p><p>每支队伍3人，比赛聚焦云计算和人工智能。</p></body></html>`
	mu.Unlock()
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 2 || !strings.Contains(sender.mails[1].body, "正式开放报名") || strings.Contains(sender.mails[1].body, "目前是预告") {
		t.Fatalf("registration transition notification incorrect: %#v", sender.mails)
	}
	competitions, err := database.ListCompetitions(ctx)
	if err != nil || len(competitions) != 1 {
		t.Fatalf("preview and registration were not deduplicated: count=%d err=%v", len(competitions), err)
	}
}

func TestSearchFindsAgentCompetitionAndFiltersIrrelevantPage(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"title":"2026 高校 AI Agent 应用创新赛报名通知","url":"`+server.URL+`/agent","content":"面向大学生的智能体和RAG应用比赛，现已开放报名"}]}`)
	})
	mux.HandleFunc("/agent", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><head><title>2026 高校 AI Agent 应用创新赛</title></head><body><p>主办方：示范大学计算机学院。</p><p>现已开放报名，报名时间：2026年8月1日至2026年9月30日。</p><p>允许组队参赛，围绕AI Agent和RAG开发应用。</p></body></html>`)
	})

	parsed, _ := url.Parse(server.URL)
	cfg := baseConfig(t)
	cfg.SearxngURL = server.URL
	cfg.MediumDomains = []string{parsed.Hostname()}
	cfg.Sources = []config.Source{
		{ID: "agent-search", Name: "Agent discovery", Kind: "search", Query: "AI Agent 比赛 报名", Limit: 10},
		{ID: "cooking", Name: "烹饪比赛", Kind: "page", URL: server.URL + "/cooking", Trust: "high", Limit: 5},
	}
	mux.HandleFunc("/cooking", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><head><title>校园烹饪与厨艺大赛报名通知</title></head><body>欢迎参加厨艺比赛，报名中。</body></html>`)
	})
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	sender := &memorySender{}
	app := service.New(cfg, database, fetcher.NewHTTPCollector(cfg), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	enableAllCategoryTestUser(t, database, app, "agent@example.com", fixedNow())
	app.SetNow(fixedNow)
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 || !strings.Contains(sender.mails[0].body, "AI Agent") {
		t.Fatalf("agent competition not notified: %#v", sender.mails)
	}
	competitions, err := database.ListCompetitions(context.Background())
	if err != nil || len(competitions) != 1 || strings.Contains(competitions[0].Name, "烹饪") {
		t.Fatalf("irrelevant competition was not filtered: %#v err=%v", competitions, err)
	}
}

func TestRegistrationMailFailureIsRetriedWithoutDuplicate(t *testing.T) {
	page := `<html><head><title>2026 云原生软件开发挑战赛</title></head><body><p>主办方：示范云计算协会。</p>
<p>报名中，报名时间：2026年8月1日至2026年8月10日。</p><p>组队参赛，使用Go后端和Kubernetes开发云原生应用。</p></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, page) }))
	defer server.Close()
	app, database, sender := newPageService(t, server.URL, "cloud", "云原生比赛", "high")
	defer database.Close()
	ctx := context.Background()
	sender.fail = true
	if err := app.Run(ctx); err == nil {
		t.Fatal("expected simulated notification failure")
	}
	users, err := database.ListNotificationUsers(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("notification users=%d err=%v", len(users), err)
	}
	if count, _ := database.CountUserPendingNotifications(ctx, users[0].User.ID); count != 1 {
		t.Fatalf("failed delivery did not retain one notification group: %d", count)
	}
	sender.fail = false
	app.SetNow(func() time.Time { return fixedNow().Add(10 * time.Minute) })
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 || !strings.Contains(sender.mails[0].body, "正式开放报名") {
		t.Fatalf("failed notification was not retried: %#v", sender.mails)
	}
	app.SetNow(func() time.Time { return time.Date(2026, 8, 9, 8, 0, 0, 0, time.FixedZone("CST", 8*3600)) })
	if err := app.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatalf("unchanged registration was delivered twice: %#v", sender.mails)
	}
}

func TestLLMFailureStoresAndNotifiesPendingHighConfidenceCompetition(t *testing.T) {
	var llmCalls int
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/competition", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<html><head><title>2026 CCF CSP 报名通知</title></head><body><p>报名已经开始。</p><p>报名时间：2026年8月3日至2026年8月30日。</p><p>算法和程序设计认证。</p></body></html>`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		llmCalls++
		http.Error(w, "simulated model outage", http.StatusServiceUnavailable)
	})
	t.Setenv("OPENAI_BASE_URL", server.URL+"/v1")
	t.Setenv("OPENAI_API_KEY", "test-key")
	t.Setenv("OPENAI_MODEL", "test-model")
	app, database, sender := newPageService(t, server.URL+"/competition", "csp-llm", "CCF CSP", "high")
	defer database.Close()
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if llmCalls == 0 || sender.count() != 1 || !strings.Contains(sender.mails[0].body, "发现新赛事（报名状态待确认）") {
		t.Fatalf("pending high-confidence competition was not retained and delivered: calls=%d mails=%#v", llmCalls, sender.mails)
	}
}

func TestConflictingOfficialDatesAreNotScheduled(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	page := func(deadline string) string {
		return `<html><head><title>2026 软件开发创新赛</title></head><body><p>主办方：示范计算机协会。</p><p>报名中，报名时间：2026年8月1日至` + deadline + `。</p><p>组队参加软件开发比赛。</p></body></html>`
	}
	mux.HandleFunc("/a", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, page("2026年8月10日")) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, page("2026年8月12日")) })
	cfg := baseConfig(t)
	cfg.Sources = []config.Source{
		{ID: "official-a", Name: "官方来源 A", Kind: "page", URL: server.URL + "/a", Trust: "high", Limit: 5},
		{ID: "official-b", Name: "官方来源 B", Kind: "page", URL: server.URL + "/b", Trust: "high", Limit: 5},
	}
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	sender := &memorySender{}
	app := service.New(cfg, database, fetcher.NewHTTPCollector(cfg), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	enableAllCategoryTestUser(t, database, app, "conflict@example.com", fixedNow())
	app.SetNow(fixedNow)
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	competitions, err := database.ListCompetitions(context.Background())
	if err != nil || len(competitions) != 1 || competitions[0].RegistrationEnd != nil || competitions[0].RegistrationEndRaw != "" {
		t.Fatalf("conflicting deadlines were accepted: %#v err=%v", competitions, err)
	}
	if sender.count() != 1 || !strings.Contains(sender.mails[0].body, "报名截止") || !strings.Contains(sender.mails[0].body, "暂未公布") {
		t.Fatalf("conflict was not exposed as unpublished: %#v", sender.mails)
	}
}

func TestMultiUserCategoryMatching(t *testing.T) {
	page := `<html><head><title>第43次 CCF CSP 认证报名通知</title></head><body>
<p>主办方：中国计算机学会。</p><p>报名已经开始。</p><p>报名时间：2026年8月3日至2026年8月20日。</p>
<p>可单人参赛。比赛内容为算法和程序设计。</p></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, page) }))
	defer server.Close()
	cfg := baseConfig(t)
	cfg.Sources = []config.Source{{ID: "csp", Name: "CCF CSP", Kind: "page", URL: server.URL, Trust: "high", Limit: 10}}
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	now := fixedNow()
	algorithmUser := createTestUser(t, database, "algorithm@example.com", "algorithm", now)
	_ = createTestUser(t, database, "cloud@example.com", "cloud_native", now)
	sender := &memorySender{}
	app := service.New(cfg, database, fetcher.NewHTTPCollector(cfg), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	app.EnableMultiUser("https://competitions.example.com", manager)
	app.SetNow(fixedNow)
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 || sender.mails[0].recipient != algorithmUser.Email {
		t.Fatalf("competition was not delivered only to matching user: %#v", sender.mails)
	}
	if err := app.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatalf("multi-user event was delivered twice: %#v", sender.mails)
	}
}

func TestQueuedRegistrationIsCancelledWhenDeadlinePassesBeforeDelivery(t *testing.T) {
	cfg := baseConfig(t)
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	ctx := context.Background()
	now := fixedNow()
	user := createTestUser(t, database, "deadline@example.com", "algorithm", now)
	deadline := now.AddDate(0, 0, 1)
	competition := model.Competition{
		EntityKey: "deadline-recheck", Name: "2026 全国程序设计大赛", Status: model.StatusRegistrationOpen,
		RegistrationEnd: &deadline, RegistrationEndRaw: "2026年8月4日", OfficialURL: "https://example.org/deadline", Trust: model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(ctx, competition, "official", now); err != nil || !isNew {
		t.Fatalf("insert competition isNew=%v err=%v", isNew, err)
	}
	saved, err := database.GetCompetition(ctx, competition.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	event := model.Event{Type: "registration_opened", Key: "registration_open"}
	if err := database.CommitCompetitionEvents(ctx, saved.ID, []model.Event{event}, []store.UserEventDispatch{{UserID: user.ID, Event: event, GroupKey: "deadline-group", DueAt: now}}, now); err != nil {
		t.Fatal(err)
	}
	sender := &memorySender{}
	app := service.New(cfg, database, fetcher.NewHTTPCollector(cfg), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	app.EnableMultiUser("https://competitions.example.com", manager)
	app.SetNow(func() time.Time { return now.AddDate(0, 0, 2) })
	if err := app.DeliverDue(ctx); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 0 {
		t.Fatalf("expired queued registration was sent: %#v", sender.mails)
	}
	if pending, err := database.CountUserPendingNotifications(ctx, user.ID); err != nil || pending != 0 {
		t.Fatalf("expired queue pending=%d err=%v", pending, err)
	}
}

func TestStartupPurgeRemovesLegacyCampusCompetition(t *testing.T) {
	cfg := baseConfig(t)
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	now := fixedNow()
	competition := model.Competition{
		EntityKey: "legacy-campus-round", Name: "关于举办2026华为软件精英挑战赛中南大学校内赛的通知-中南大学计算机学院",
		Status: model.StatusRegistrationOpen, OfficialURL: "https://cse.example.edu.cn/notice", Trust: model.TrustMedium,
	}
	if _, isNew, err := database.UpsertCompetition(context.Background(), competition, "legacy", now); err != nil || !isNew {
		t.Fatalf("insert legacy competition isNew=%v err=%v", isNew, err)
	}
	app := service.New(cfg, database, fetcher.NewHTTPCollector(cfg), analyzer.New(cfg), &memorySender{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	removed, err := app.PurgeIneligibleCompetitions(context.Background())
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	competitions, err := database.ListCompetitions(context.Background())
	if err != nil || len(competitions) != 0 {
		t.Fatalf("legacy campus competition remains: %#v err=%v", competitions, err)
	}
}

func TestBackfillQueuesExistingCompetitionOnceForNewUser(t *testing.T) {
	cfg := baseConfig(t)
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	now := fixedNow()
	competition := model.Competition{
		EntityKey:      "existing-backend-competition",
		Name:           "2026云端应用开发挑战赛",
		Status:         model.StatusRegistrationOpen,
		StatusEvidence: "现已开放报名",
		Keywords:       []string{"服务端"},
		Content:        "开发云端应用",
		OfficialURL:    "https://competition.example.com/2026",
		Trust:          model.TrustHigh,
	}
	if _, isNew, err := database.UpsertCompetition(context.Background(), competition, "existing", now.Add(-time.Hour)); err != nil || !isNew {
		t.Fatalf("insert competition isNew=%v err=%v", isNew, err)
	}
	saved, err := database.GetCompetition(context.Background(), competition.EntityKey)
	if err != nil {
		t.Fatal(err)
	}
	event := model.Event{Type: "registration_opened", Key: "registration_open"}
	if err := database.CommitCompetitionEvents(context.Background(), saved.ID, []model.Event{event}, nil, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	user := createTestUser(t, database, "new-user@example.com", "development", now)
	preferences, err := database.GetUserPreferences(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	preferences.IncludeKeywords = []string{"backend"}
	if err := database.SaveUserPreferences(context.Background(), preferences, now); err != nil {
		t.Fatal(err)
	}
	app := service.New(cfg, database, fetcher.NewHTTPCollector(cfg), analyzer.New(cfg), &memorySender{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	app.SetNow(fixedNow)
	inserted, err := app.BackfillUser(context.Background(), user.ID)
	if err != nil || inserted != 1 {
		t.Fatalf("backfill inserted=%d err=%v", inserted, err)
	}
	pending, err := database.CountUserPendingNotifications(context.Background(), user.ID)
	if err != nil || pending != 1 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	inserted, err = app.BackfillUser(context.Background(), user.ID)
	if err != nil || inserted != 0 {
		t.Fatalf("duplicate backfill inserted=%d err=%v", inserted, err)
	}
}

func createTestUser(t *testing.T, database *store.Store, email, category string, now time.Time) model.User {
	t.Helper()
	if err := database.RequestVerification(context.Background(), email, "test-hash", now, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	user, err := database.ConsumeVerification(context.Background(), email, "test-hash", now, []string{category})
	if err != nil {
		t.Fatal(err)
	}
	preferences, err := database.GetUserPreferences(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	preferences.Frequency = model.DeliveryImmediate
	preferences.Categories = []string{category}
	if err := database.SaveUserPreferences(context.Background(), preferences, now); err != nil {
		t.Fatal(err)
	}
	return user
}

func enableAllCategoryTestUser(t *testing.T, database *store.Store, app *service.Service, email string, now time.Time) model.User {
	t.Helper()
	categories := subscription.CategoryIDs()
	if err := database.RequestVerification(context.Background(), email, "test-hash", now, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	user, err := database.ConsumeVerification(context.Background(), email, "test-hash", now, categories)
	if err != nil {
		t.Fatal(err)
	}
	preferences, err := database.GetUserPreferences(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	preferences.Frequency = model.DeliveryImmediate
	preferences.Categories = categories
	if err := database.SaveUserPreferences(context.Background(), preferences, now); err != nil {
		t.Fatal(err)
	}
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	app.EnableMultiUser("https://competitions.example.com", manager)
	return user
}

func newPageService(t *testing.T, pageURL, id, name, trust string) (*service.Service, *store.Store, *memorySender) {
	t.Helper()
	cfg := baseConfig(t)
	cfg.Sources = []config.Source{{ID: id, Name: name, Kind: "page", URL: pageURL, Trust: trust, Limit: 10}}
	database := openStore(t, cfg.DBPath)
	sender := &memorySender{}
	app := service.New(cfg, database, fetcher.NewHTTPCollector(cfg), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	enableAllCategoryTestUser(t, database, app, "fixture@example.com", fixedNow())
	app.SetNow(fixedNow)
	return app, database, sender
}

func baseConfig(t *testing.T) config.Config {
	t.Helper()
	loc := time.FixedZone("CST", 8*3600)
	return config.Config{
		Schedule: "0 8 * * *", Timezone: "Asia/Shanghai", Location: loc,
		DBPath: filepath.Join(t.TempDir(), "test.db"), SearxngURL: "http://invalid", AppriseURL: "http://invalid",
		Fetch: config.Fetch{TimeoutSeconds: 3, MaxBytes: 1024 * 1024, MaxCandidates: 20},
	}
}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	database, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return database
}

func fixedNow() time.Time {
	return time.Date(2026, 8, 3, 8, 0, 0, 0, time.FixedZone("CST", 8*3600))
}

// TestCancelledNotificationIsRestoredOnReoptIn exercises the real service
// flow: opting in enqueues a start notice, opting out cancels it, and opting
// back in restores it to pending instead of leaving it cancelled.
func TestCancelledNotificationIsRestoredOnReoptIn(t *testing.T) {
	cfg := baseConfig(t)
	cfg.Sources = nil
	database := openStore(t, cfg.DBPath)
	defer database.Close()
	sender := &memorySender{}
	app := service.New(cfg, database, fetcher.NewHTTPCollector(cfg), analyzer.New(cfg), sender, slog.New(slog.NewTextHandler(io.Discard, nil)))
	user := enableAllCategoryTestUser(t, database, app, "restore@example.com", fixedNow())
	app.SetNow(fixedNow)

	// Ensure start notifications are enabled for this user.
	preferences, err := database.GetUserPreferences(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	preferences.NotifyStarted = true
	if err := database.SaveUserPreferences(context.Background(), preferences, fixedNow()); err != nil {
		t.Fatal(err)
	}

	// Create an ongoing competition.
	now := fixedNow()
	if _, _, err := database.UpsertCompetition(context.Background(), model.Competition{
		EntityKey:   "ongoing-2026",
		Name:        "2026 全国大学生软件开发大赛",
		Status:      model.StatusOngoing,
		OfficialURL: "https://contest.example.com/2026",
		Trust:       model.TrustHigh,
	}, "test-src", now); err != nil {
		t.Fatal(err)
	}
	competition, err := database.GetCompetition(context.Background(), "ongoing-2026")
	if err != nil {
		t.Fatal(err)
	}

	// Opt in: enqueues the start notice as pending.
	if err := app.SetUserCompetitionDecision(context.Background(), user.ID, competition.ID, model.ParticipationParticipating); err != nil {
		t.Fatal(err)
	}
	assertPendingCount(t, database, user.ID, 1)
	// Opt out: the store cancels the pending notice.
	if err := app.SetUserCompetitionDecision(context.Background(), user.ID, competition.ID, model.ParticipationDeclined); err != nil {
		t.Fatal(err)
	}
	assertPendingCount(t, database, user.ID, 0)

	// Opt back in: the cancelled notice must be restored to pending (the
	// regression under test); before the fix it stayed cancelled and this
	// assertion failed.
	if err := app.SetUserCompetitionDecision(context.Background(), user.ID, competition.ID, model.ParticipationParticipating); err != nil {
		t.Fatal(err)
	}
	assertPendingCount(t, database, user.ID, 1)
}

// assertPendingCount verifies the number of pending notification groups for a
// user, which reflects whether the cancelled notice was restored.
func assertPendingCount(t *testing.T, database *store.Store, userID int64, want int) {
	t.Helper()
	items, err := database.ListUserPendingItems(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != want {
		t.Fatalf("pending notification groups = %d, want %d", len(items), want)
	}
}

// TestYearlessFreshlyObservedCompetitionIsIngested is the regression guard for
// the FirstSeen fallback. A brand-new competition with no year, no dates and
// no publish date must not be rejected by isCurrentEdition just because its
// FirstSeen is zero before upsert. The fix fills FirstSeen from the page's
// earliest observation time, so the freshness fallback has a value.
func TestYearlessFreshlyObservedCompetitionIsIngested(t *testing.T) {
	// Title deliberately carries no year, and the body has no dates.
	page := `<html><head><title>华为软件精英挑战赛官网</title></head><body>
<p>华为软件精英挑战赛，面向全球在校学生开放的算法竞技赛事。</p>
<p>赛题围绕真实云场景下的资源调度与优化问题展开。</p></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, page) }))
	defer server.Close()

	app, database, _ := newPageService(t, server.URL, "huawei-competition", "华为软件精英挑战赛", "high")
	defer database.Close()

	if err := app.Run(context.Background()); err != nil {
		t.Fatalf("Run() returned an error (yearless competition should be ingested, not rejected): %v", err)
	}
	competitions, err := database.ListCompetitions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(competitions) != 1 {
		t.Fatalf("yearless competition produced %d rows, want 1", len(competitions))
	}
	if got := competitions[0].Name; got != "华为软件精英挑战赛官网" {
		t.Fatalf("ingested name = %q, want the yearless competition", got)
	}
	if competitions[0].FirstSeen.IsZero() {
		t.Fatal("ingested competition must have a non-zero FirstSeen from its observation")
	}
}
