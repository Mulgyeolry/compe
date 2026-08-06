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
	"net/mail"
	"net/url"
	"strconv"
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

type Server struct {
	store        *store.Store
	sender       notifier.RecipientSender
	auth         *authn.Manager
	web          config.Web
	log          *slog.Logger
	location     *time.Location
	now          func() time.Time
	template     *template.Template
	handler      http.Handler
	pushTrigger  func(int64) bool
	backfill     func(context.Context, int64) (int, error)
	setChoice    func(context.Context, int64, int64, model.ParticipationDecision) error
	testMailMu   sync.Mutex
	lastTestMail map[int64]time.Time
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
	server := &Server{store: database, sender: sender, auth: manager, web: web, log: logger, location: location, now: time.Now, template: templates, lastTestMail: make(map[int64]time.Time)}
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
	server.handler = server.requestObservability(server.securityHeaders(mux))
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
	if err := s.store.Ping(ctx); err != nil {
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

func (s *Server) requestCode(w http.ResponseWriter, request *http.Request) {
	guestToken, err := s.guestToken(request)
	if err != nil {
		http.Error(w, "页面已过期，请返回首页重试。", http.StatusForbidden)
		return
	}
	if !s.parseAndVerifyCSRF(w, request, guestToken) {
		return
	}
	email, err := normalizeEmail(request.FormValue("email"))
	if err != nil {
		s.render(w, http.StatusBadRequest, "login.html", pageData{Title: "登录", CSRF: s.auth.CSRFToken(guestToken), Error: "请输入有效的邮箱地址。"})
		return
	}
	code, err := s.auth.NewVerificationCode()
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	now := s.now()
	if err := s.store.RequestVerification(request.Context(), email, s.auth.VerificationHash(email, code), now, s.web.VerificationTTL); err != nil {
		if errors.Is(err, store.ErrVerificationRateLimited) {
			s.render(w, http.StatusTooManyRequests, "login.html", pageData{Title: "登录", CSRF: s.auth.CSRFToken(guestToken), Error: "请求过于频繁，请 1 分钟后再试。"})
			return
		}
		s.internalError(w, request, err)
		return
	}
	subject, body, err := notifier.RenderVerification(code, s.web.VerificationTTL)
	if err == nil {
		err = s.sender.SendTo(request.Context(), email, subject, body)
	}
	if err != nil {
		s.log.Error("verification email delivery failed", "error", err)
		s.render(w, http.StatusBadGateway, "login.html", pageData{Title: "登录", CSRF: s.auth.CSRFToken(guestToken), Error: "验证码邮件发送失败，请稍后重试。"})
		return
	}
	s.render(w, http.StatusOK, "verify.html", pageData{Title: "输入验证码", CSRF: s.auth.CSRFToken(guestToken), Email: email, Message: "验证码已发送，请检查收件箱和垃圾邮件。"})
}

func (s *Server) verifyCode(w http.ResponseWriter, request *http.Request) {
	guestToken, err := s.guestToken(request)
	if err != nil {
		http.Error(w, "页面已过期，请返回首页重试。", http.StatusForbidden)
		return
	}
	if !s.parseAndVerifyCSRF(w, request, guestToken) {
		return
	}
	email, emailErr := normalizeEmail(request.FormValue("email"))
	code := strings.TrimSpace(request.FormValue("code"))
	if emailErr != nil || len(code) != 6 || strings.Trim(code, "0123456789") != "" {
		s.render(w, http.StatusBadRequest, "verify.html", pageData{Title: "输入验证码", CSRF: s.auth.CSRFToken(guestToken), Email: email, Error: "验证码无效，请重新输入。"})
		return
	}
	now := s.now()
	user, err := s.store.ConsumeVerification(request.Context(), email, s.auth.VerificationHash(email, code), now, subscription.CategoryIDs())
	if err != nil {
		if errors.Is(err, store.ErrInvalidVerificationCode) {
			s.render(w, http.StatusUnauthorized, "verify.html", pageData{Title: "输入验证码", CSRF: s.auth.CSRFToken(guestToken), Email: email, Error: "验证码错误或已过期。"})
			return
		}
		s.internalError(w, request, err)
		return
	}
	sessionToken, err := s.auth.NewSessionToken()
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	if err := s.store.CreateSession(request.Context(), user.ID, s.auth.SessionHash(sessionToken), now.Add(s.web.SessionTTL), now); err != nil {
		s.internalError(w, request, err)
		return
	}
	s.setCookie(w, sessionCookie, sessionToken, s.web.SessionTTL)
	s.clearCookie(w, guestCookie)
	http.Redirect(w, request, "/preferences?welcome=1", http.StatusSeeOther)
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

func (s *Server) setWebsiteCompetitionChoice(w http.ResponseWriter, request *http.Request) {
	user, sessionToken, ok := s.requireUser(w, request)
	if !ok {
		return
	}
	if !s.parseAndVerifyCSRF(w, request, sessionToken) {
		return
	}
	competitionID, err := strconv.ParseInt(request.FormValue("competition_id"), 10, 64)
	decision := model.ParticipationDecision(request.FormValue("decision"))
	if err != nil || competitionID < 1 || !validParticipationDecision(decision) {
		http.Error(w, "参赛选择无效。", http.StatusBadRequest)
		return
	}
	if s.setChoice == nil {
		http.Redirect(w, request, "/dashboard?result=choice-unavailable", http.StatusSeeOther)
		return
	}
	if err := s.setChoice(request.Context(), user.ID, competitionID, decision); err != nil {
		s.log.Warn("website competition choice failed", "user_id", user.ID, "competition_id", competitionID, "error", err)
		http.Redirect(w, request, "/dashboard?result=choice-failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, request, "/dashboard?result=choice-saved", http.StatusSeeOther)
}

func (s *Server) competitionChoicePage(w http.ResponseWriter, request *http.Request) {
	token := strings.TrimSpace(request.URL.Query().Get("token"))
	userID, competitionID, decisionRaw, valid := s.auth.VerifyCompetitionChoiceToken(token)
	if !valid {
		http.Error(w, "链接无效或已损坏。", http.StatusBadRequest)
		return
	}
	competition, err := s.store.GetCompetitionByID(request.Context(), competitionID)
	if err != nil {
		http.Error(w, "比赛不存在或已清理。", http.StatusNotFound)
		return
	}
	if _, err := s.store.GetUserPreferences(request.Context(), userID); err != nil {
		http.Error(w, "用户不存在。", http.StatusNotFound)
		return
	}
	s.render(w, http.StatusOK, "competition-choice.html", pageData{Title: "确认参赛选择", ChoiceCompetition: competition.Name, ChoiceDecision: participationDecisionLabel(model.ParticipationDecision(decisionRaw)), ChoiceToken: token})
}

func (s *Server) confirmCompetitionChoice(w http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(w, request.Body, maxFormBytes)
	if err := request.ParseForm(); err != nil {
		http.Error(w, "请求无效。", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(request.FormValue("token"))
	userID, competitionID, decisionRaw, valid := s.auth.VerifyCompetitionChoiceToken(token)
	decision := model.ParticipationDecision(decisionRaw)
	if !valid || !validParticipationDecision(decision) {
		http.Error(w, "链接无效或已损坏。", http.StatusBadRequest)
		return
	}
	if s.setChoice == nil {
		http.Error(w, "当前无法保存参赛选择。", http.StatusServiceUnavailable)
		return
	}
	if err := s.setChoice(request.Context(), userID, competitionID, decision); err != nil {
		s.log.Warn("email competition choice failed", "user_id", userID, "competition_id", competitionID, "error", err)
		http.Error(w, "比赛已经结束，或当前无法保存选择。", http.StatusConflict)
		return
	}
	s.render(w, http.StatusOK, "competition-choice-saved.html", pageData{Title: "参赛选择已保存", ChoiceDecision: participationDecisionLabel(decision)})
}

func validParticipationDecision(decision model.ParticipationDecision) bool {
	return decision == model.ParticipationParticipating || decision == model.ParticipationDeclined
}

func participationDecisionLabel(decision model.ParticipationDecision) string {
	if decision == model.ParticipationParticipating {
		return "参加比赛"
	}
	return "不参加比赛"
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

func (s *Server) preferences(w http.ResponseWriter, request *http.Request) {
	user, sessionToken, ok := s.requireUser(w, request)
	if !ok {
		return
	}
	preferences, err := s.store.GetUserPreferences(request.Context(), user.ID)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	message := ""
	if request.URL.Query().Get("welcome") == "1" {
		message = "邮箱验证成功。请确认你想关注的比赛类型和提醒频率。"
	}
	s.renderPreferences(w, http.StatusOK, user, sessionToken, preferences, message, "")
}

func (s *Server) savePreferences(w http.ResponseWriter, request *http.Request) {
	user, sessionToken, ok := s.requireUser(w, request)
	if !ok {
		return
	}
	if !s.parseAndVerifyCSRF(w, request, sessionToken) {
		return
	}
	preferences, err := parsePreferences(request, user.ID)
	if err != nil {
		s.renderPreferences(w, http.StatusBadRequest, user, sessionToken, preferences, "", err.Error())
		return
	}
	now := s.now()
	if err := s.store.SaveUserPreferences(request.Context(), preferences, now); err != nil {
		s.internalError(w, request, err)
		return
	}
	pending, err := s.store.ListUserPendingItems(request.Context(), user.ID)
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	var cancelled []int64
	for _, item := range pending {
		classification := subscription.Profile(item.Competition)
		if len(subscription.MatchingEventsForUser(preferences, item.Competition, classification, []model.Event{item.Event}, item.Decision, s.now())) == 0 {
			cancelled = append(cancelled, item.NotificationID)
		}
	}
	if err := s.store.CancelUserNotifications(request.Context(), user.ID, cancelled); err != nil {
		s.internalError(w, request, err)
		return
	}
	dueAt, err := subscription.NextDelivery(now, preferences)
	if err == nil {
		group := subscription.DeliveryGroupKey(user.ID, 0, preferences.Frequency, dueAt, "rescheduled")
		err = s.store.RescheduleUserPending(request.Context(), user.ID, dueAt, group)
	}
	if err != nil {
		s.internalError(w, request, err)
		return
	}
	backfilled := 0
	if s.backfill != nil {
		backfilled, err = s.backfill(request.Context(), user.ID)
		if err != nil {
			s.log.Error("user competition backfill failed", "user_id", user.ID, "error", err)
			s.renderPreferences(w, http.StatusInternalServerError, user, sessionToken, preferences, "设置已保存。", "赛事匹配刷新失败，请稍后重新保存设置。")
			return
		}
	}
	message := "设置已保存。"
	if backfilled > 0 {
		message = fmt.Sprintf("设置已保存，已补充 %d 条尚未向你推送的有效赛事动态。", backfilled)
	}
	s.renderPreferences(w, http.StatusOK, user, sessionToken, preferences, message, "")
}

func (s *Server) logout(w http.ResponseWriter, request *http.Request) {
	_, sessionToken, ok := s.requireUser(w, request)
	if !ok {
		return
	}
	if !s.parseAndVerifyCSRF(w, request, sessionToken) {
		return
	}
	if err := s.store.DeleteSession(request.Context(), s.auth.SessionHash(sessionToken)); err != nil {
		s.internalError(w, request, err)
		return
	}
	s.clearCookie(w, sessionCookie)
	http.Redirect(w, request, "/", http.StatusSeeOther)
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
	if err := s.store.DisableUser(request.Context(), userID, s.now()); err != nil {
		s.internalError(w, request, err)
		return
	}
	s.clearCookie(w, sessionCookie)
	s.render(w, http.StatusOK, "unsubscribed.html", pageData{Title: "已停止提醒"})
}

func (s *Server) renderPreferences(w http.ResponseWriter, status int, user model.User, sessionToken string, preferences model.UserPreferences, message, errorMessage string) {
	selectedCategories := make(map[string]bool, len(preferences.Categories))
	for _, category := range preferences.Categories {
		selectedCategories[category] = true
	}
	selectedOrganizers := selectedOptions(preferences.OrganizerTypes, subscription.OrganizerTypeIDs())
	selectedScopes := selectedOptions(preferences.CompetitionScopes, subscription.CompetitionScopeIDs())
	if len(preferences.Categories) == 0 {
		for _, category := range subscription.CategoryIDs() {
			selectedCategories[category] = true
		}
	}
	s.render(w, status, "preferences.html", pageData{
		Title:              "提醒偏好",
		CSRF:               s.auth.CSRFToken(sessionToken),
		User:               user,
		Preferences:        preferences,
		Categories:         subscription.Categories(),
		OrganizerTypes:     subscription.OrganizerTypes(),
		Scopes:             subscription.CompetitionScopes(),
		SelectedCategories: selectedCategories,
		SelectedOrganizers: selectedOrganizers,
		SelectedScopes:     selectedScopes,
		IncludeText:        strings.Join(preferences.IncludeKeywords, "，"),
		ExcludeText:        strings.Join(preferences.ExcludeKeywords, "，"),
		RegionsText:        strings.Join(preferences.Regions, "，"),
		Message:            message,
		Error:              errorMessage,
	})
}

func parsePreferences(request *http.Request, userID int64) (model.UserPreferences, error) {
	categories := request.Form["categories"]
	seenCategories := map[string]bool{}
	validCategories := make([]string, 0, len(categories))
	for _, category := range categories {
		if !subscription.ValidCategory(category) || seenCategories[category] {
			continue
		}
		seenCategories[category] = true
		validCategories = append(validCategories, category)
	}
	if len(validCategories) == 0 {
		return model.UserPreferences{UserID: userID}, errors.New("请至少选择一种比赛类型。")
	}
	organizerTypes := validFormOptions(request.Form["organizer_types"], subscription.ValidOrganizerType)
	if len(organizerTypes) == 0 {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, errors.New("请至少选择一种主办方类型。")
	}
	competitionScopes := validFormOptions(request.Form["competition_scopes"], subscription.ValidCompetitionScope)
	if len(competitionScopes) == 0 {
		return model.UserPreferences{UserID: userID, Categories: validCategories, OrganizerTypes: organizerTypes}, errors.New("请至少选择一种赛事范围。")
	}
	regions, err := subscription.NormalizeRegions([]string{request.FormValue("regions")})
	if err != nil {
		return model.UserPreferences{UserID: userID, Categories: validCategories, OrganizerTypes: organizerTypes, CompetitionScopes: competitionScopes}, err
	}
	frequency := model.DeliveryFrequency(request.FormValue("frequency"))
	if frequency != model.DeliveryImmediate && frequency != model.DeliveryDaily && frequency != model.DeliveryWeekly {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, errors.New("提醒频率无效。")
	}
	deliveryTime := "08:00"
	if frequency != model.DeliveryImmediate {
		deliveryTime = strings.TrimSpace(request.FormValue("delivery_time"))
		if _, err := time.Parse("15:04", deliveryTime); err != nil {
			return model.UserPreferences{UserID: userID, Categories: validCategories, Frequency: frequency}, errors.New("提醒时间无效。")
		}
	}
	weeklyDay := 1
	if frequency == model.DeliveryWeekly {
		parsedWeeklyDay, err := strconv.Atoi(request.FormValue("weekly_day"))
		if err != nil || parsedWeeklyDay < 0 || parsedWeeklyDay > 6 {
			return model.UserPreferences{UserID: userID, Categories: validCategories, Frequency: frequency, DeliveryTime: deliveryTime}, errors.New("每周投递日期无效。")
		}
		weeklyDay = parsedWeeklyDay
	}
	timezone := strings.TrimSpace(request.FormValue("timezone"))
	if _, err := time.LoadLocation(timezone); err != nil || len(timezone) > 64 {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, errors.New("时区无效。")
	}
	trust := model.Trust(request.FormValue("min_trust"))
	if trust != model.TrustHigh && trust != model.TrustMedium {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, errors.New("可信度设置无效。")
	}
	includeKeywords, err := subscription.NormalizeKeywords([]string{request.FormValue("include_keywords")})
	if err != nil {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, err
	}
	excludeKeywords, err := subscription.NormalizeKeywords([]string{request.FormValue("exclude_keywords")})
	if err != nil {
		return model.UserPreferences{UserID: userID, Categories: validCategories}, err
	}
	preferences := model.UserPreferences{
		UserID:                userID,
		Frequency:             frequency,
		DeliveryTime:          deliveryTime,
		WeeklyDay:             time.Weekday(weeklyDay),
		Timezone:              timezone,
		MinTrust:              trust,
		AllowEligibilityRisk:  request.FormValue("allow_eligibility_risk") == "1",
		NotifyPreview:         request.FormValue("notify_preview") == "1",
		NotifyRegistration:    request.FormValue("notify_registration") == "1",
		NotifyUpcoming:        request.FormValue("notify_upcoming") == "1",
		NotifyStarted:         request.FormValue("notify_started") == "1",
		NotifyProblemRelease:  request.FormValue("notify_problem_release") == "1",
		NotifyDeadline7Days:   request.FormValue("notify_deadline_7d") == "1",
		NotifyDeadline1Day:    request.FormValue("notify_deadline_1d") == "1",
		NotifyImportantUpdate: request.FormValue("notify_important_update") == "1",
		Categories:            validCategories,
		OrganizerTypes:        organizerTypes,
		CompetitionScopes:     competitionScopes,
		Regions:               regions,
		IncludeKeywords:       includeKeywords,
		ExcludeKeywords:       excludeKeywords,
	}
	if !preferences.NotifyPreview && !preferences.NotifyRegistration && !preferences.NotifyUpcoming && !preferences.NotifyStarted && !preferences.NotifyProblemRelease &&
		!preferences.NotifyDeadline7Days && !preferences.NotifyDeadline1Day && !preferences.NotifyImportantUpdate {
		return preferences, errors.New("请至少选择一种需要提醒的赛事动态。")
	}
	return preferences, nil
}

func selectedOptions(selected, defaults []string) map[string]bool {
	if len(selected) == 0 {
		selected = defaults
	}
	result := make(map[string]bool, len(selected))
	for _, value := range selected {
		result[value] = true
	}
	return result
}

func validFormOptions(values []string, valid func(string) bool) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if valid(value) && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
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

func (s *Server) parseAndVerifyCSRF(w http.ResponseWriter, request *http.Request, cookieToken string) bool {
	request.Body = http.MaxBytesReader(w, request.Body, maxFormBytes)
	if err := request.ParseForm(); err != nil {
		http.Error(w, "请求内容无效。", http.StatusBadRequest)
		return false
	}
	if !s.auth.VerifyCSRF(cookieToken, request.FormValue("csrf")) {
		http.Error(w, "页面已过期，请刷新后重试。", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) currentUser(request *http.Request) (model.User, string, bool) {
	cookie, err := request.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return model.User{}, "", false
	}
	user, err := s.store.UserBySession(request.Context(), s.auth.SessionHash(cookie.Value), s.now())
	if err != nil {
		return model.User{}, "", false
	}
	return user, cookie.Value, true
}

func (s *Server) requireUser(w http.ResponseWriter, request *http.Request) (model.User, string, bool) {
	user, token, ok := s.currentUser(request)
	if !ok {
		http.Redirect(w, request, "/", http.StatusSeeOther)
	}
	return user, token, ok
}

func (s *Server) ensureGuestToken(w http.ResponseWriter, request *http.Request) (string, error) {
	if cookie, err := request.Cookie(guestCookie); err == nil && cookie.Value != "" {
		return cookie.Value, nil
	}
	token, err := s.auth.NewSessionToken()
	if err != nil {
		return "", err
	}
	s.setCookie(w, guestCookie, token, 20*time.Minute)
	return token, nil
}

func (s *Server) guestToken(request *http.Request) (string, error) {
	cookie, err := request.Cookie(guestCookie)
	if err != nil || cookie.Value == "" {
		return "", errors.New("missing guest session")
	}
	return cookie.Value, nil
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration) {
	baseURL, _ := url.Parse(s.web.PublicBaseURL)
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: baseURL.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: int(ttl.Seconds()), Expires: s.now().Add(ttl)})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: strings.HasPrefix(s.web.PublicBaseURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: -1, Expires: time.Unix(1, 0)})
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

func normalizeEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 254 {
		return "", errors.New("email is too long")
	}
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value || !strings.Contains(value, "@") {
		return "", errors.New("invalid email")
	}
	return value, nil
}
