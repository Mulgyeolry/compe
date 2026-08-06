package webapp

import (
	"context"
	"errors"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"competition-assistant/internal/authn"
	"competition-assistant/internal/config"
)

// fakeUserDisabler records how the unsubscribe flow disables a user without any
// database or SQLite file.
type fakeUserDisabler struct {
	err    error
	called int
	userID int64
	now    time.Time
}

func (f *fakeUserDisabler) DisableUser(_ context.Context, userID int64, now time.Time) error {
	f.called++
	f.userID = userID
	f.now = now
	return f.err
}

// newUnsubscribeTestServer builds a minimal Server with only the dependencies
// the unsubscribe handler needs, without opening SQLite.
func newUnsubscribeTestServer(t *testing.T, manager *authn.Manager, disabler *fakeUserDisabler, fixedNow time.Time) *Server {
	t.Helper()
	templates, err := template.ParseFS(assets, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		auth:         manager,
		userDisabler: disabler,
		now:          func() time.Time { return fixedNow },
		log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		web:          config.Web{PublicBaseURL: "https://example.test"},
		template:     templates,
	}
}

// TestUnsubscribeUsesUserDisabler verifies a valid unsubscribe token disables
// the user through the injected userDisabler and clears the session cookie.
func TestUnsubscribeUsesUserDisabler(t *testing.T) {
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 8, 6, 14, 15, 30, 0, time.UTC)
	disabler := &fakeUserDisabler{}
	server := newUnsubscribeTestServer(t, manager, disabler, fixedNow)

	token := manager.UnsubscribeToken(42, "student@example.com")
	request := httptest.NewRequest(http.MethodPost, "/unsubscribe", strings.NewReader("token="+token))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.unsubscribe(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), "你的订阅已经停用") {
		t.Fatalf("page missing success text: %s", response.Body.String())
	}
	if disabler.called != 1 {
		t.Fatalf("DisableUser called %d times, want 1", disabler.called)
	}
	if disabler.userID != 42 {
		t.Fatalf("userID=%d, want 42", disabler.userID)
	}
	if !disabler.now.Equal(fixedNow) {
		t.Fatalf("now=%v, want %v", disabler.now, fixedNow)
	}

	cookies := response.Result().Cookies()
	var cleared *http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == sessionCookie {
			cleared = cookie
			break
		}
	}
	if cleared == nil {
		t.Fatal("sessionCookie was not cleared in response")
	}
	if cleared.Path != "/" {
		t.Fatalf("cookie path=%q, want /", cleared.Path)
	}
	if !cleared.HttpOnly {
		t.Fatal("cookie HttpOnly=false, want true")
	}
	if !cleared.Secure {
		t.Fatal("cookie Secure=false, want true")
	}
	if cleared.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie SameSite=%v, want Lax", cleared.SameSite)
	}
	if cleared.MaxAge >= 0 {
		t.Fatalf("cookie MaxAge=%d, want < 0", cleared.MaxAge)
	}
}

// TestUnsubscribeRejectsInvalidToken verifies a tampered token is rejected with
// 400 and the user disabler is never invoked.
func TestUnsubscribeRejectsInvalidToken(t *testing.T) {
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	disabler := &fakeUserDisabler{}
	server := newUnsubscribeTestServer(t, manager, disabler, time.Now())

	request := httptest.NewRequest(http.MethodPost, "/unsubscribe", strings.NewReader("token=tampered.invalid"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.unsubscribe(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", response.Code)
	}
	if disabler.called != 0 {
		t.Fatalf("DisableUser called %d times, want 0", disabler.called)
	}
	if strings.Contains(response.Body.String(), "tampered") {
		t.Fatalf("invalid token leaked internal detail: %q", response.Body.String())
	}
}

// TestUnsubscribeStorageFailure verifies a storage error yields a generic 500
// without leaking the underlying error or sensitive placeholder.
func TestUnsubscribeStorageFailure(t *testing.T) {
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 8, 6, 14, 15, 30, 0, time.UTC)
	disabler := &fakeUserDisabler{err: errors.New("database password=secret")}
	server := newUnsubscribeTestServer(t, manager, disabler, fixedNow)

	token := manager.UnsubscribeToken(42, "student@example.com")
	request := httptest.NewRequest(http.MethodPost, "/unsubscribe", strings.NewReader("token="+token))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.unsubscribe(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", response.Code)
	}
	if disabler.called != 1 {
		t.Fatalf("DisableUser called %d times, want 1", disabler.called)
	}
	if !strings.Contains(response.Body.String(), "服务器暂时无法处理该请求") {
		t.Fatalf("generic error text missing: %q", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "password") {
		t.Fatalf("storage error leaked in response: %q", response.Body.String())
	}
}

// TestUnsubscribeRejectsOversizedForm verifies a body over maxFormBytes is
// rejected with 400 and the user disabler is never invoked.
func TestUnsubscribeRejectsOversizedForm(t *testing.T) {
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	disabler := &fakeUserDisabler{}
	server := newUnsubscribeTestServer(t, manager, disabler, time.Now())

	oversized := "token=" + strings.Repeat("a", maxFormBytes+1)
	request := httptest.NewRequest(http.MethodPost, "/unsubscribe", strings.NewReader(oversized))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.unsubscribe(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", response.Code)
	}
	if disabler.called != 0 {
		t.Fatalf("DisableUser called %d times, want 0", disabler.called)
	}
}
