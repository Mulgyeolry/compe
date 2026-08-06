package webapp

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"competition-assistant/internal/authn"
	"competition-assistant/internal/config"
	"competition-assistant/internal/model"
	"competition-assistant/internal/notifier"
	"competition-assistant/internal/store"
	"competition-assistant/internal/subscription"
)

const (
	sessionCookie = "competition_session"
	guestCookie   = "competition_guest"
	maxFormBytes  = 64 << 10
)

//go:embed templates/*.html static/*
var assets embed.FS

// readinessChecker is the consumer-owned, single-method dependency the
// readiness endpoint needs to probe the critical local dependency.
type readinessChecker interface {
	Ping(context.Context) error
}

// userDisabler is the consumer-owned, single-method dependency the unsubscribe
// flow needs to disable a user.
type userDisabler interface {
	DisableUser(
		ctx context.Context,
		userID int64,
		now time.Time,
	) error
}

type Server struct {
	store         *store.Store
	sessionLookup sessionLookup
	readiness     readinessChecker
	userDisabler  userDisabler
	sender        notifier.RecipientSender
	auth          *authn.Manager
	web           config.Web
	log           *slog.Logger
	location      *time.Location
	now           func() time.Time
	template      *template.Template
	handler       http.Handler
	pushTrigger   func(int64) bool
	backfill      func(context.Context, int64) (int, error)
	setChoice     func(context.Context, int64, int64, model.ParticipationDecision) error
	testMailMu    sync.Mutex
	lastTestMail  map[int64]time.Time
}

type pageData struct {
	Title              string
	CSRF               string
	Email              string
	Error              string
	Message            string
	User               model.User
	Preferences        model.UserPreferences
	Categories         []subscription.Category
	OrganizerTypes     []subscription.FilterOption
	Scopes             []subscription.FilterOption
	SelectedCategories map[string]bool
	SelectedOrganizers map[string]bool
	SelectedScopes     map[string]bool
	IncludeText        string
	ExcludeText        string
	RegionsText        string
	Competitions       []competitionCard
	Unconfirmed        []competitionCard
	History            []historyCard
	UnsubscribeKey     string
	PendingCount       int
	ChoiceCompetition  string
	ChoiceDecision     string
	ChoiceToken        string
}

type competitionCard struct {
	ID            int64
	Name          string
	Organizer     string
	Status        string
	StatusClass   string
	Deadline      string
	Fee           string
	OfficialURL   string
	Trust         string
	Tags          []string
	Analysis      model.CompetitionAnalysis
	AnalysisTrust string
	LinkNote      string
	AlternateURL  string
	Decision      model.ParticipationDecision
}

type historyCard struct {
	CompetitionName string
	Event           string
	Status          string
	Time            string
}

func New(database *store.Store, sender notifier.RecipientSender, manager *authn.Manager, web config.Web, location *time.Location, logger *slog.Logger) (*Server, error) {
	if location == nil {
		return nil, errors.New("webapp: location must not be nil")
	}
	templates, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse web templates: %w", err)
	}
	server := &Server{store: database, sessionLookup: database, readiness: database, userDisabler: database, sender: sender, auth: manager, web: web, log: logger, location: location, now: time.Now, template: templates, lastTestMail: make(map[int64]time.Time)}
	mux := http.NewServeMux()
	staticFiles, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("load static files: %w", err)
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFiles))))
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /readyz", server.ready)
	mux.HandleFunc("GET /", server.home)
	mux.HandleFunc("POST /auth/request", server.requestCode)
	mux.HandleFunc("POST /auth/verify", server.verifyCode)
	mux.HandleFunc("GET /dashboard", server.dashboard)
	mux.HandleFunc("GET /preferences", server.preferences)
	mux.HandleFunc("POST /preferences", server.savePreferences)
	mux.HandleFunc("POST /actions/test-email", server.testEmail)
	mux.HandleFunc("POST /actions/push", server.pushPending)
	mux.HandleFunc("POST /competitions/choice", server.setWebsiteCompetitionChoice)
	mux.HandleFunc("GET /competition-choice", server.competitionChoicePage)
	mux.HandleFunc("POST /competition-choice", server.confirmCompetitionChoice)
	mux.HandleFunc("POST /logout", server.logout)
	mux.HandleFunc("GET /unsubscribe", server.unsubscribePage)
	mux.HandleFunc("POST /unsubscribe", server.unsubscribe)
	server.handler = server.securityHeaders(
		server.requestObservability(
			server.recoverPanics(mux),
		),
	)
	return server, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) SetNow(now func() time.Time) { s.now = now }

func (s *Server) SetPushTrigger(trigger func(int64) bool) { s.pushTrigger = trigger }

func (s *Server) SetBackfillTrigger(trigger func(context.Context, int64) (int, error)) {
	s.backfill = trigger
}

func (s *Server) SetCompetitionChoiceTrigger(trigger func(context.Context, int64, int64, model.ParticipationDecision) error) {
	s.setChoice = trigger
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// ready reports whether the application is ready to handle normal requests by
// probing the single critical local dependency, SQLite. External dependencies
// (LLM, Apprise, search, fetched sites) are deliberately excluded so a brief
// failure of any of them does not mark the whole application unready. The
// probe is bounded by a two-second timeout and the HTTP response never exposes
// internal database errors.
func (s *Server) ready(w http.ResponseWriter, request *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	if err := s.readiness.Ping(ctx); err != nil {
		s.log.Error("readiness probe failed", "request_id", requestIDFromContext(request.Context()), "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ready\n"))
}

func (s *Server) home(w http.ResponseWriter, request *http.Request) {
	if _, _, ok := s.currentUser(request); ok {
		http.Redirect(w, request, "/dashboard", http.StatusSeeOther)
		return
	}
	guestToken, err := s.ensureGuestToken(w, request)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	s.render(w, http.StatusOK, "login.html", pageData{Title: "登录", CSRF: s.auth.CSRFToken(guestToken)})
}

func (s *Server) dashboard(w http.ResponseWriter, request *http.Request) {
	user, sessionToken, ok := s.requireUser(w, request)
	if !ok {
		return
	}
	preferences, err := s.store.GetUserPreferences(request.Context(), user.ID)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	competitions, err := s.store.ListActionableCompetitions(request.Context(), s.now().In(s.location), 50)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	unconfirmed, err := s.store.ListUnconfirmedCompetitions(request.Context(), 30)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	decisions, err := s.store.ListUserCompetitionDecisions(request.Context(), user.ID)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	history, err := s.store.ListUserNotificationHistory(request.Context(), user.ID, 20)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	pendingCount, err := s.store.CountUserPendingNotifications(request.Context(), user.ID)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	message, errorMessage := dashboardResultMessage(request.URL.Query().Get("result"))
	s.render(w, http.StatusOK, "dashboard.html", pageData{Title: "订阅概览", CSRF: s.auth.CSRFToken(sessionToken), User: user, Preferences: preferences, Competitions: competitionCards(competitions, preferences, decisions), Unconfirmed: unconfirmedCards(unconfirmed), History: historyCards(history), PendingCount: pendingCount, Message: message, Error: errorMessage})
}

func (s *Server) testEmail(w http.ResponseWriter, request *http.Request) {
	user, sessionToken, ok := s.requireUser(w, request)
	if !ok {
		return
	}
	if !s.parseAndVerifyCSRF(w, request, sessionToken) {
		return
	}
	if !s.reserveTestMail(user.ID) {
		http.Redirect(w, request, "/dashboard?result=test-rate-limited", http.StatusSeeOther)
		return
	}
	subject, body, err := notifier.RenderTest(s.now().In(s.location))
	if err == nil {
		err = s.sender.SendTo(request.Context(), user.Email, subject, body)
	}
	if err != nil {
		s.releaseTestMail(user.ID)
		s.log.Error("test email delivery failed", "user_id", user.ID, "error", err)
		http.Redirect(w, request, "/dashboard?result=test-failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, request, "/dashboard?result=test-sent", http.StatusSeeOther)
}

func (s *Server) pushPending(w http.ResponseWriter, request *http.Request) {
	user, sessionToken, ok := s.requireUser(w, request)
	if !ok {
		return
	}
	if !s.parseAndVerifyCSRF(w, request, sessionToken) {
		return
	}
	pendingCount, err := s.store.CountUserPendingNotifications(request.Context(), user.ID)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	if pendingCount == 0 && s.backfill != nil {
		if _, err := s.backfill(request.Context(), user.ID); err != nil {
			s.log.Error("user competition backfill before immediate push failed", "user_id", user.ID, "error", err)
			s.internalError(w, request, err)
			return
		}
		pendingCount, err = s.store.CountUserPendingNotifications(request.Context(), user.ID)
		if err != nil {
			s.internalError(w, request, err)
			return
		}
	}
	if pendingCount == 0 {
		http.Redirect(w, request, "/dashboard?result=push-empty", http.StatusSeeOther)
		return
	}
	if s.pushTrigger == nil {
		http.Redirect(w, request, "/dashboard?result=push-unavailable", http.StatusSeeOther)
		return
	}
	if !s.pushTrigger(user.ID) {
		http.Redirect(w, request, "/dashboard?result=push-busy", http.StatusSeeOther)
		return
	}
	http.Redirect(w, request, "/dashboard?result=push-started", http.StatusSeeOther)
}

func (s *Server) reserveTestMail(userID int64) bool {
	s.testMailMu.Lock()
	defer s.testMailMu.Unlock()
	now := s.now()
	if last, ok := s.lastTestMail[userID]; ok && now.Sub(last) < time.Minute {
		return false
	}
	s.lastTestMail[userID] = now
	return true
}

func (s *Server) releaseTestMail(userID int64) {
	s.testMailMu.Lock()
	defer s.testMailMu.Unlock()
	delete(s.lastTestMail, userID)
}

func dashboardResultMessage(result string) (string, string) {
	switch result {
	case "test-sent":
		return "测试邮件已提交，请检查收件箱和垃圾邮件。", ""
	case "push-started":
		return "正在推送当前待发送通知，请稍后检查邮箱。", ""
	case "push-empty":
		return "当前没有新增且尚未发送的比赛通知。", ""
	case "push-busy":
		return "系统正在扫描或投递通知，请稍后再试。", ""
	case "test-rate-limited":
		return "", "测试邮件发送过于频繁，请 1 分钟后再试。"
	case "test-failed":
		return "", "测试邮件发送失败，请检查 Apprise 日志和发件邮箱配置。"
	case "push-unavailable":
		return "", "当前运行模式不支持网页立即推送。"
	case "choice-saved":
		return "参赛选择已保存，可随时修改。", ""
	case "choice-failed":
		return "", "参赛选择保存失败，比赛可能已经结束。"
	case "choice-unavailable":
		return "", "当前运行模式不支持保存参赛选择。"
	default:
		return "", ""
	}
}

func (s *Server) unsubscribePage(w http.ResponseWriter, request *http.Request) {
	token := request.URL.Query().Get("token")
	_, email, valid := s.auth.VerifyUnsubscribeToken(token)
	if !valid {
		http.Error(w, "退订链接无效或已损坏。", http.StatusBadRequest)
		return
	}
	s.render(w, http.StatusOK, "unsubscribe.html", pageData{Title: "停止接收提醒", Email: email, UnsubscribeKey: token})
}

func (s *Server) unsubscribe(w http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(w, request.Body, maxFormBytes)
	if err := request.ParseForm(); err != nil {
		http.Error(w, "请求无效。", http.StatusBadRequest)
		return
	}
	userID, _, valid := s.auth.VerifyUnsubscribeToken(request.FormValue("token"))
	if !valid {
		http.Error(w, "退订链接无效或已损坏。", http.StatusBadRequest)
		return
	}
	if err := s.userDisabler.DisableUser(request.Context(), userID, s.now()); err != nil {
		s.internalError(w, request, err)
		return
	}
	s.clearCookie(w, sessionCookie)
	s.render(w, http.StatusOK, "unsubscribed.html", pageData{Title: "已停止提醒"})
}

func competitionCards(competitions []model.Competition, preferences model.UserPreferences, decisions map[int64]model.ParticipationDecision) []competitionCard {
	result := make([]competitionCard, 0, min(len(competitions), 16))
	for _, competition := range competitions {
		decision := decisions[competition.ID]
		if decision == "" {
			decision = model.ParticipationUndecided
		}
		model.NormalizeLifecycle(&competition)
		if decision != model.ParticipationParticipating && competition.RegistrationPhase != model.RegistrationPreview && competition.RegistrationPhase != model.RegistrationOpen {
			continue
		}
		profile := subscription.Profile(competition)
		if !subscription.MatchesCompetition(preferences, competition, profile) {
			continue
		}
		card := competitionCard{
			ID:            competition.ID,
			Name:          competition.Name,
			Organizer:     missingWeb(competition.Organizer),
			Status:        statusWeb(competition.Status),
			StatusClass:   string(competition.Status),
			Deadline:      missingWeb(competition.RegistrationEndRaw),
			Fee:           missingWeb(competition.Fee),
			OfficialURL:   competition.OfficialURL,
			Trust:         trustWeb(competition.Trust),
			Analysis:      competition.Analysis,
			AnalysisTrust: analysisTrustWeb(competition.Analysis.Confidence),
			Decision:      decision,
		}
		if parsed, err := url.Parse(competition.OfficialURL); err == nil && strings.EqualFold(parsed.Hostname(), "e.huawei.com") {
			card.LinkNote = "华为官网可能要求浏览器验证"
			card.AlternateURL = "https://e.huawei.com/cn/talent/portal/"
		}
		for _, category := range profile.Categories {
			card.Tags = append(card.Tags, subscription.CategoryName(category))
			if len(card.Tags) == 2 {
				break
			}
		}
		if len(profile.OrganizerTypes) > 0 {
			card.Tags = append(card.Tags, subscription.OrganizerTypeName(profile.OrganizerTypes[0]))
		}
		if len(profile.Scopes) > 0 {
			card.Tags = append(card.Tags, subscription.CompetitionScopeName(profile.Scopes[0]))
		}
		card.Tags = append(card.Tags, profile.Regions...)
		for _, keyword := range competition.Keywords {
			if !containsString(card.Tags, keyword) {
				card.Tags = append(card.Tags, keyword)
			}
			if len(card.Tags) >= 8 {
				break
			}
		}
		result = append(result, card)
		if len(result) == 16 {
			break
		}
	}
	return result
}

// unconfirmedCards renders competitions whose lifecycle state is not yet
// confirmed. Preferences are deliberately not applied here: their profile is
// incomplete (status, dates and often organizer are missing), so preference
// matching would either hide most rows or silently keep filtering them out of
// the confirmed list. The section's job is to show crawl progress until a
// later scan confirms a phase.
func unconfirmedCards(competitions []model.Competition) []competitionCard {
	result := make([]competitionCard, 0, min(len(competitions), 30))
	for _, competition := range competitions {
		model.NormalizeLifecycle(&competition)
		card := competitionCard{
			ID:          competition.ID,
			Name:        competition.Name,
			Organizer:   missingWeb(competition.Organizer),
			Status:      statusWeb(competition.Status),
			StatusClass: string(competition.Status),
			OfficialURL: competition.OfficialURL,
			Trust:       trustWeb(competition.Trust),
		}
		if parsed, err := url.Parse(competition.OfficialURL); err == nil && strings.EqualFold(parsed.Hostname(), "e.huawei.com") {
			card.LinkNote = "华为官网可能要求浏览器验证"
			card.AlternateURL = "https://e.huawei.com/cn/talent/portal/"
		}
		result = append(result, card)
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func analysisTrustWeb(confidence string) string {
	switch confidence {
	case "high":
		return "较高可信"
	case "medium":
		return "中等可信"
	case "low":
		return "低可信，仅供参考"
	default:
		return "尚未评估"
	}
}

func historyCards(history []model.UserNotificationHistory) []historyCard {
	result := make([]historyCard, 0, len(history))
	for _, item := range history {
		when := item.DueAt
		if item.SentAt != nil {
			when = *item.SentAt
		}
		result = append(result, historyCard{CompetitionName: item.CompetitionName, Event: eventWeb(item.EventType), Status: notificationStatusWeb(item.Status), Time: when.Format("01-02 15:04")})
	}
	return result
}

func statusWeb(status model.Status) string {
	switch status {
	case model.StatusPreview:
		return "预告"
	case model.StatusUpcoming:
		return "即将开赛"
	case model.StatusRegistrationOpen:
		return "报名中"
	case model.StatusOngoing:
		return "进行中"
	case model.StatusRegistrationClosed:
		return "报名已截止"
	case model.StatusFinished:
		return "已结束"
	default:
		return "待确认"
	}
}

func eventWeb(event string) string {
	switch event {
	case "competition_discovered":
		return "发现新赛事（报名状态待确认）"
	case "preview_detected":
		return "赛事预告"
	case "registration_opened":
		return "开放报名"
	case "competition_upcoming":
		return "即将开赛"
	case "competition_started":
		return "正式开赛"
	case "problem_released":
		return "赛题发布"
	case "deadline_7d":
		return "截止前 7 天"
	case "deadline_1d":
		return "截止前 1 天"
	case "important_update":
		return "重要更新"
	default:
		return event
	}
}

func notificationStatusWeb(status string) string {
	switch status {
	case "sent":
		return "已发送"
	case "pending":
		return "等待发送"
	case "failed":
		return "稍后重试"
	case "cancelled":
		return "已取消"
	default:
		return status
	}
}

func trustWeb(trust model.Trust) string {
	if trust == model.TrustHigh {
		return "官方来源"
	}
	return "可信来源"
}

func missingWeb(value string) string {
	if strings.TrimSpace(value) == "" {
		return "暂未公布"
	}
	return value
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.template.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render web template failed", "template", name, "error", err)
	}
}

func (s *Server) internalError(w http.ResponseWriter, request *http.Request, err error) {
	s.log.Error("web request failed", "method", request.Method, "path", request.URL.Path, "request_id", requestIDFromContext(request.Context()), "error", err)
	http.Error(w, "服务器暂时无法处理该请求，请稍后重试。", http.StatusInternalServerError)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, request)
	})
}
