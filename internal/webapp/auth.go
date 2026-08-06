package webapp

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"competition-assistant/internal/model"
	"competition-assistant/internal/notifier"
	"competition-assistant/internal/store"
	"competition-assistant/internal/subscription"
)

// sessionLookup is the consumer-owned, single-method dependency currentUser
// needs to resolve a session token to a user. It deliberately exposes no other
// database operations so the Web layer can be tested without SQLite.
type sessionLookup interface {
	UserBySession(
		ctx context.Context,
		tokenHash string,
		now time.Time,
	) (model.User, error)
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
	user, err := s.sessionLookup.UserBySession(request.Context(), s.auth.SessionHash(cookie.Value), s.now())
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
