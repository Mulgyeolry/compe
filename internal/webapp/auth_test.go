package webapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"competition-assistant/internal/authn"
	"competition-assistant/internal/model"
)

// fakeSessionLookup records how currentUser resolves a session token without
// any database or SQLite file.
type fakeSessionLookup struct {
	user      model.User
	err       error
	called    int
	tokenHash string
	now       time.Time
}

func (f *fakeSessionLookup) UserBySession(_ context.Context, tokenHash string, now time.Time) (model.User, error) {
	f.called++
	f.tokenHash = tokenHash
	f.now = now
	return f.user, f.err
}

// TestCurrentUserUsesSessionLookup verifies currentUser resolves the session
// through the injected sessionLookup with the hashed token and the fixed now,
// without opening SQLite.
func TestCurrentUserUsesSessionLookup(t *testing.T) {
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 8, 6, 12, 30, 0, 0, time.UTC)
	wantUser := model.User{ID: 7, Email: "student@example.com"}
	fake := &fakeSessionLookup{user: wantUser}
	server := &Server{auth: manager, sessionLookup: fake, now: func() time.Time { return fixedNow }}

	const cookieValue = "cookie-session-token"
	request := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookieValue})

	user, token, ok := server.currentUser(request)
	if !ok {
		t.Fatal("currentUser returned ok=false")
	}
	if user != wantUser {
		t.Fatalf("user=%#v, want %#v", user, wantUser)
	}
	if token != cookieValue {
		t.Fatalf("token=%q, want original cookie %q", token, cookieValue)
	}
	if fake.called != 1 {
		t.Fatalf("fake called %d times, want 1", fake.called)
	}
	if wantHash := manager.SessionHash(cookieValue); fake.tokenHash != wantHash {
		t.Fatalf("lookup tokenHash=%q, want %q (hashed, not raw cookie)", fake.tokenHash, wantHash)
	}
	if !fake.now.Equal(fixedNow) {
		t.Fatalf("lookup now=%v, want %v", fake.now, fixedNow)
	}
}

// TestCurrentUserSessionLookupFailure verifies a lookup error yields a zero
// user, an empty token and ok=false, and is not exposed to the response or log.
func TestCurrentUserSessionLookupFailure(t *testing.T) {
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 8, 6, 12, 30, 0, 0, time.UTC)
	fake := &fakeSessionLookup{err: errors.New("session expired")}
	server := &Server{auth: manager, sessionLookup: fake, now: func() time.Time { return fixedNow }}

	request := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "cookie-session-token"})

	user, token, ok := server.currentUser(request)
	if ok {
		t.Fatal("currentUser returned ok=true on lookup error")
	}
	if user != (model.User{}) {
		t.Fatalf("user not zeroed on error: %#v", user)
	}
	if token != "" {
		t.Fatalf("token not empty on error: %q", token)
	}
	if fake.called != 1 {
		t.Fatalf("fake called %d times, want 1", fake.called)
	}
}
