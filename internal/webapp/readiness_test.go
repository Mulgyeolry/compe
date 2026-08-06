package webapp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeReadinessChecker records how the readiness handler probes the dependency
// without any database or SQLite file.
type fakeReadinessChecker struct {
	err         error
	called      int
	ctx         context.Context
	hasDeadline bool
}

func (f *fakeReadinessChecker) Ping(ctx context.Context) error {
	f.called++
	f.ctx = ctx
	_, f.hasDeadline = ctx.Deadline()
	return f.err
}

// TestReadySuccess verifies a healthy readiness probe returns HTTP 200 with a
// strict "ready\n" body, correct content type, a deadline-bounded context that
// is cancelled after the handler returns, and only one Ping call.
func TestReadySuccess(t *testing.T) {
	fake := &fakeReadinessChecker{}
	server := &Server{
		readiness: fake,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	server.ready(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", response.Code)
	}
	if response.Body.String() != "ready\n" {
		t.Fatalf("body=%q, want %q", response.Body.String(), "ready\n")
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/plain; charset=utf-8" {
		t.Fatalf("content-type=%q, want text/plain; charset=utf-8", contentType)
	}
	if fake.called != 1 {
		t.Fatalf("Ping called %d times, want 1", fake.called)
	}
	if !fake.hasDeadline {
		t.Fatal("readiness context had no deadline")
	}
	if fake.ctx.Err() != context.Canceled {
		t.Fatalf("readiness context not cancelled after handler returned: %v", fake.ctx.Err())
	}
}

// TestReadyFailure verifies a failing readiness probe returns HTTP 503 with a
// strict "not ready\n" body that never leaks the underlying error, and only one
// Ping call.
func TestReadyFailure(t *testing.T) {
	fake := &fakeReadinessChecker{err: errors.New("database password=secret")}
	server := &Server{
		readiness: fake,
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	server.ready(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", response.Code)
	}
	if response.Body.String() != "not ready\n" {
		t.Fatalf("body=%q, want %q", response.Body.String(), "not ready\n")
	}
	if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "password") {
		t.Fatalf("readiness failure leaked underlying error: %q", response.Body.String())
	}
	if fake.called != 1 {
		t.Fatalf("Ping called %d times, want 1", fake.called)
	}
}
