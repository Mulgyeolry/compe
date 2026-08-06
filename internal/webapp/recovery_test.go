package webapp

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// buildRecoveryChain wraps downstream with the production middleware order:
// securityHeaders -> requestObservability -> recoverPanics.
func buildRecoveryChain(t *testing.T, logger *slog.Logger, downstream http.Handler) http.Handler {
	t.Helper()
	server := &Server{log: logger}
	return server.securityHeaders(
		server.requestObservability(
			server.recoverPanics(downstream),
		),
	)
}

// TestPanicBeforeWriteReturns500 verifies a panic before any response is written
// returns a generic HTTP 500 with security headers and a request ID, logs the
// panic without leaking secrets, and records status 500 in the access log.
func TestPanicBeforeWriteReturns500(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	handler := buildRecoveryChain(t, logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom panic")
	}))
	request := httptest.NewRequest(http.MethodPost, "/some/path?secret=query", nil)
	request.Header.Set("Cookie", "session=secret-cookie")
	request.Header.Set("Authorization", "Bearer secret-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", response.Code)
	}
	if strings.Contains(response.Body.String(), "boom panic") {
		t.Fatalf("panic leaked in response body: %q", response.Body.String())
	}
	if csp := response.Header().Get("Content-Security-Policy"); csp == "" {
		t.Fatal("missing Content-Security-Policy header")
	}
	headerID := response.Header().Get("X-Request-ID")
	if len(headerID) != 32 {
		t.Fatalf("missing 32-char X-Request-ID: %q", headerID)
	}

	logs := decodeLogLines(t, buffer.Bytes())
	panicLog := findLog(t, logs, "web panic recovered")
	accessLog := findLog(t, logs, "http request")

	if panicLog["request_id"] != headerID {
		t.Fatalf("panic log request_id %v != response header %q", panicLog["request_id"], headerID)
	}
	if panicLog["method"] != http.MethodPost {
		t.Fatalf("panic log method=%v, want POST", panicLog["method"])
	}
	if panicLog["path"] != "/some/path" {
		t.Fatalf("panic log path=%v, want /some/path (no query)", panicLog["path"])
	}
	if panicLog["panic"] != "boom panic" {
		t.Fatalf("panic log value=%v, want boom panic", panicLog["panic"])
	}
	if stack, ok := panicLog["stack"].(string); !ok || stack == "" {
		t.Fatalf("panic log missing stack: %v", panicLog["stack"])
	}
	if panicLog["response_committed"] != false {
		t.Fatalf("panic log response_committed=%v, want false", panicLog["response_committed"])
	}
	if statusOf(accessLog["status"]) != http.StatusInternalServerError {
		t.Fatalf("access log status=%v, want 500", accessLog["status"])
	}
	if accessLog["request_id"] != headerID {
		t.Fatalf("access log request_id %v != header %q", accessLog["request_id"], headerID)
	}

	rawLog := buffer.String()
	for _, forbidden := range []string{"secret", "secret-cookie", "secret-token", "query", "Cookie", "Authorization"} {
		if strings.Contains(rawLog, forbidden) {
			t.Fatalf("log leaked %q: %s", forbidden, rawLog)
		}
	}
}

// TestPanicAfterPartialResponse verifies a panic after a partial response is
// written does not append error text, preserves the first status code, re-panics
// with http.ErrAbortHandler and marks response_committed=true.
func TestPanicAfterPartialResponse(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	handler := buildRecoveryChain(t, logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("partial"))
		panic("boom after write")
	}))
	request := httptest.NewRequest(http.MethodGet, "/some/path", nil)
	response := httptest.NewRecorder()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		handler.ServeHTTP(response, request)
	}()

	if recovered != http.ErrAbortHandler {
		t.Fatalf("expected re-panic with http.ErrAbortHandler, got %v", recovered)
	}
	if response.Code != http.StatusCreated {
		t.Fatalf("first status not preserved: code=%d, want 201", response.Code)
	}
	if response.Body.String() != "partial" {
		t.Fatalf("unexpected body after partial write: %q, want %q (no error text appended)", response.Body.String(), "partial")
	}
	logs := decodeLogLines(t, buffer.Bytes())
	panicLog := findLog(t, logs, "web panic recovered")
	if panicLog["response_committed"] != true {
		t.Fatalf("panic log response_committed=%v, want true", panicLog["response_committed"])
	}
	if panicLog["panic"] != "boom after write" {
		t.Fatalf("panic log value=%v, want boom after write", panicLog["panic"])
	}
}

// TestPanicRecoveryNormalRequest verifies a normal request is unaffected by the
// recovery middleware.
func TestPanicRecoveryNormalRequest(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	handler := buildRecoveryChain(t, logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fine"))
	}))
	request := httptest.NewRequest(http.MethodGet, "/ok", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != "fine" {
		t.Fatalf("normal request altered: code=%d body=%q", response.Code, response.Body.String())
	}
	logs := decodeLogLines(t, buffer.Bytes())
	if hasLogMessage(t, logs, "web panic recovered") {
		t.Fatal("normal request produced a panic log")
	}
}

// TestPanicRecoveryNoSecrets verifies the panic log never contains query
// strings, cookies or authorization headers.
func TestPanicRecoveryNoSecrets(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	handler := buildRecoveryChain(t, logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	request := httptest.NewRequest(http.MethodGet, "/a/b?token=abc123&x=1", nil)
	request.Header.Set("Cookie", "auth=cookie-value;other=y")
	request.Header.Set("Authorization", "Bearer topsecret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	rawLog := buffer.String()
	for _, forbidden := range []string{"token=abc123", "abc123", "cookie-value", "topsecret", "Bearer", "Authorization", "Cookie"} {
		if strings.Contains(rawLog, forbidden) {
			t.Fatalf("panic log leaked %q: %s", forbidden, rawLog)
		}
	}
}

func decodeLogLines(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var logs []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		logs = append(logs, entry)
	}
	return logs
}

func findLog(t *testing.T, logs []map[string]any, message string) map[string]any {
	t.Helper()
	for _, entry := range logs {
		if entry["msg"] == message {
			return entry
		}
	}
	t.Fatalf("log message %q not found: %v", message, logs)
	return nil
}

func hasLogMessage(t *testing.T, logs []map[string]any, message string) bool {
	t.Helper()
	for _, entry := range logs {
		if entry["msg"] == message {
			return true
		}
	}
	return false
}
