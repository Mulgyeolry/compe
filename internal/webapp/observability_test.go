package webapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"competition-assistant/internal/authn"
	"competition-assistant/internal/config"
	"competition-assistant/internal/store"
)

// TestRequestIDGeneratedWithoutHeader verifies a request without X-Request-ID
// receives a non-empty 32-char lowercase hex ID in both the response header and
// the request context.
func TestRequestIDGeneratedWithoutHeader(t *testing.T) {
	var captured string
	handler := (&Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}).requestObservability(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = requestIDFromContext(r.Context())
		_, _ = w.Write([]byte("ok"))
	}))
	request := httptest.NewRequest(http.MethodGet, "/some-path", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	headerID := response.Header().Get("X-Request-ID")
	if headerID == "" {
		t.Fatal("X-Request-ID header is empty")
	}
	if len(headerID) != 32 {
		t.Fatalf("generated request id length=%d, want 32", len(headerID))
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(headerID) {
		t.Fatalf("generated request id is not lowercase hex: %q", headerID)
	}
	if captured != headerID {
		t.Fatalf("context id %q != header id %q", captured, headerID)
	}
}

// TestRequestIDAcceptsValidClientID verifies a well-formed client ID is kept
// verbatim and appears in both the context and the response header.
func TestRequestIDAcceptsValidClientID(t *testing.T) {
	const clientID = "abc-123_DEF.xYz"
	var captured string
	handler := (&Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}).requestObservability(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = requestIDFromContext(r.Context())
		_, _ = w.Write([]byte("ok"))
	}))
	request := httptest.NewRequest(http.MethodGet, "/some-path", nil)
	request.Header.Set("X-Request-ID", clientID)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if got := response.Header().Get("X-Request-ID"); got != clientID {
		t.Fatalf("header id %q, want %q", got, clientID)
	}
	if captured != clientID {
		t.Fatalf("context id %q, want %q", captured, clientID)
	}
}

// TestRequestIDRejectsInvalidOrOversized verifies invalid or too-long client
// IDs are ignored and replaced with a freshly generated safe ID.
func TestRequestIDRejectsInvalidOrOversized(t *testing.T) {
	invalidIDs := []string{
		"",
		"has space",
		"contains;injection",
		"line\nbreak",
		"uni" + "code界",
		strings.Repeat("a", 65),
	}
	for _, provided := range invalidIDs {
		handler := (&Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}).requestObservability(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}))
		request := httptest.NewRequest(http.MethodGet, "/some-path", nil)
		request.Header.Set("X-Request-ID", provided)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		got := response.Header().Get("X-Request-ID")
		if got == provided {
			t.Fatalf("invalid client id %q was accepted verbatim", provided)
		}
		if len(got) != 32 || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(got) {
			t.Fatalf("replacement id %q is not a 32-char lowercase hex", got)
		}
	}
}

// TestAccessLogFields verifies the access log records method, path, status,
// bytes, duration and request_id while never leaking query strings, cookies or
// authorization headers.
func TestAccessLogFields(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	server := &Server{log: logger}
	handler := server.requestObservability(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}))
	request := httptest.NewRequest(http.MethodPost, "/some/path?secret=query-token", nil)
	request.Header.Set("Cookie", "session=super-secret-cookie")
	request.Header.Set("Authorization", "Bearer super-secret-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	entry := decodeAccessLog(t, buffer.Bytes())
	rawLog := buffer.String()
	if entry["method"] != http.MethodPost {
		t.Fatalf("method=%v, want POST", entry["method"])
	}
	if entry["path"] != "/some/path" {
		t.Fatalf("path=%v, want /some/path", entry["path"])
	}
	if statusOf(entry["status"]) != http.StatusCreated {
		t.Fatalf("status=%v, want 201", entry["status"])
	}
	if statusOf(entry["bytes"]) != 5 {
		t.Fatalf("bytes=%v, want 5", entry["bytes"])
	}
	if _, ok := entry["duration_ms"]; !ok {
		t.Fatalf("duration_ms missing: %v", entry)
	}
	if _, ok := entry["request_id"]; !ok {
		t.Fatalf("request_id missing: %v", entry)
	}
	for _, forbidden := range []string{"secret", "query-token", "super-secret-cookie", "super-secret-token", "Bearer", "session="} {
		if strings.Contains(rawLog, forbidden) {
			t.Fatalf("access log leaked %q: %s", forbidden, rawLog)
		}
	}
}

// TestAccessLogNonDefaultStatus verifies a custom handler returning a non-200
// status is captured correctly for both status and bytes.
func TestAccessLogNonDefaultStatus(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	server := &Server{log: logger}
	handler := server.requestObservability(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	request := httptest.NewRequest(http.MethodGet, "/boom", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	entry := decodeAccessLog(t, buffer.Bytes())
	if statusOf(entry["status"]) != http.StatusInternalServerError {
		t.Fatalf("status=%v, want 500", entry["status"])
	}
	if statusOf(entry["bytes"]) <= 0 {
		t.Fatalf("bytes=%v, want > 0", entry["bytes"])
	}
}

// TestHealthAndReadinessCarryRequestID verifies the probe endpoints still return
// their correct business results and both carry an X-Request-ID response header.
func TestHealthAndReadinessCarryRequestID(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "obs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
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

	wantBody := map[string]string{"/healthz": "ok\n", "/readyz": "ready\n"}
	for endpoint, body := range wantBody {
		response, err := client.Get(httpServer.URL + endpoint)
		if err != nil {
			t.Fatal(err)
		}
		rawBody, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if string(rawBody) != body {
			t.Fatalf("%s body=%q, want %q", endpoint, string(rawBody), body)
		}
		if id := response.Header.Get("X-Request-ID"); len(id) != 32 {
			t.Fatalf("%s missing 32-char X-Request-ID: %q", endpoint, id)
		}
	}
}

// TestResponseRecorderFirstStatusWins verifies the recorder keeps the first
// effective status and never lets later WriteHeader or implicit-200 writes
// overwrite it.
func TestResponseRecorderFirstStatusWins(t *testing.T) {
	cases := []struct {
		name   string
		handle func(http.ResponseWriter)
		want   int
	}{
		{"WriteHeader then WriteHeader", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			w.WriteHeader(http.StatusInternalServerError)
		}, http.StatusCreated},
		{"Write then WriteHeader", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte("body"))
			w.WriteHeader(http.StatusInternalServerError)
		}, http.StatusOK},
		{"no explicit write", func(http.ResponseWriter) {}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buffer bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buffer, nil))
			server := &Server{log: logger}
			handler := server.requestObservability(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.handle(w)
			}))
			request := httptest.NewRequest(http.MethodGet, "/some-path", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			entry := decodeAccessLog(t, buffer.Bytes())
			if statusOf(entry["status"]) != tc.want {
				t.Fatalf("status=%v, want %d", entry["status"], tc.want)
			}
		})
	}
}

// TestGenerateRequestIDFailure verifies generateRequestID surfaces a failed or
// short read as an error (never a zero ID) and produces 32 lowercase hex on the
// normal path.
func TestGenerateRequestIDFailure(t *testing.T) {
	if id, err := generateRequestID(failingReader{}); err == nil || id != "" {
		t.Fatalf("expected error and empty id, got id=%q err=%v", id, err)
	}
	id, err := generateRequestID(strings.NewReader("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 32 || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("unexpected normal id: %q", id)
	}
}

// TestProbeEndpointsSkipAccessLog verifies /healthz and a successful /readyz do
// not write an "http request" access log, while a failing /readyz still records
// the readiness error.
func TestProbeEndpointsSkipAccessLog(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	database, err := store.Open(filepath.Join(t.TempDir(), "probe.log.db"))
	if err != nil {
		t.Fatal(err)
	}
	manager, err := authn.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(database, &capturedSender{}, manager, config.Web{Enabled: true, PublicBaseURL: "http://example.test"}, time.UTC, logger)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	client := httpServer.Client()

	if _, err := client.Get(httpServer.URL + "/healthz"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(httpServer.URL + "/readyz"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buffer.String(), "http request") {
		t.Fatalf("probe endpoints produced access log: %s", buffer.String())
	}

	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(httpServer.URL + "/readyz"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buffer.String(), "readiness probe failed") {
		t.Fatalf("failing readyz did not record readiness error: %s", buffer.String())
	}
}

// TestObservabilityFailureKeepsSecurityHeaders verifies that when request ID
// generation fails the downstream handler does not run, the response is a
// generic HTTP 500 that leaks no internal error and still carries the security
// headers, and no forged request ID is echoed.
func TestObservabilityFailureKeepsSecurityHeaders(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buffer, nil))
	server := &Server{log: logger}

	downstreamRan := false
	handler := server.securityHeaders(server.requestObservabilityWithGenerator(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			downstreamRan = true
		}),
		func() (string, error) { return "", errors.New("boom") },
	))

	request := httptest.NewRequest(http.MethodGet, "/some-path", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if downstreamRan {
		t.Fatal("downstream handler ran despite request ID generation failure")
	}
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", response.Code)
	}
	body := response.Body.String()
	if strings.Contains(body, "boom") {
		t.Fatalf("internal error leaked in response body: %q", body)
	}
	if strings.TrimSpace(body) != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("body=%q, want generic %q", body, http.StatusText(http.StatusInternalServerError))
	}
	if id := response.Header().Get("X-Request-ID"); id != "" {
		t.Fatalf("forged/fixed X-Request-ID returned on failure: %q", id)
	}
	if csp := response.Header().Get("Content-Security-Policy"); csp == "" {
		t.Fatal("Content-Security-Policy header missing on failure response")
	}
	if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options=%q, want nosniff", got)
	}
	if got := response.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options=%q, want DENY", got)
	}
	if !strings.Contains(buffer.String(), "request ID generation failed") {
		t.Fatalf("access log missing generation failure: %s", buffer.String())
	}
}

// failingReader always returns an error, simulating a crypto/rand failure.
type failingReader struct{}

func (failingReader) Read(_ []byte) (int, error) { return 0, errors.New("boom") }

func decodeAccessLog(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("decode access log: %v", err)
	}
	return entry
}

func statusOf(value any) int {
	if number, ok := value.(float64); ok {
		return int(number)
	}
	return -1
}
