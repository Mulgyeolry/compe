package webapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"time"
)

// requestIDKey is an unexported context key for the per-request ID, keeping the
// value out of the way of other context consumers.
type requestIDKey struct{}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// requestIDFromContext returns the request ID associated with ctx, or an empty
// string if none is present.
func requestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// validRequestID reports whether id is 1..64 ASCII letters, digits, '-', '_' or
// '.'. Anything else (including empty or oversized values) is rejected so the
// value cannot be abused as a log-injection vector.
func validRequestID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for index := 0; index < len(id); index++ {
		r := id[index]
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// generateRequestID reads 16 random bytes from reader and returns them as a
// 32-character lowercase hex string. It uses io.ReadFull so a short or failed
// read is surfaced as an error rather than yielding a truncated or zero ID.
func generateRequestID(reader io.Reader) (string, error) {
	var buffer [16]byte
	if _, err := io.ReadFull(reader, buffer[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer[:]), nil
}

// newRequestID returns a fresh 32-character lowercase hex ID from crypto/rand.
// Callers must handle the error instead of falling back to a fixed value.
func newRequestID() (string, error) {
	return generateRequestID(rand.Reader)
}

// resolveRequestID returns a client-supplied ID when it is well-formed,
// otherwise a freshly generated random one via generate. If generation fails
// the error is returned so the caller can fail the request rather than continue
// with a predictable ID.
func resolveRequestID(provided string, generate func() (string, error)) (string, error) {
	if validRequestID(provided) {
		return provided, nil
	}
	return generate()
}

// errRequestIDGeneration signals that no usable request ID could be produced.
var errRequestIDGeneration = errors.New("request ID generation failed")

// responseRecorder wraps an http.ResponseWriter to capture the first effective
// response status and the body byte count for access logging. The status is
// recorded only once: later WriteHeader calls and implicit-200 writes must not
// overwrite an already-committed status. Unwrap exposes the underlying writer
// so http.ResponseController keeps working.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) Flush() {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// requestObservability assigns every request a request ID, echoes it in the
// X-Request-ID response header, places it in the request context, and records a
// structured access log after the handler runs. Probe endpoints are suppressed
// to avoid log spam.
func (s *Server) requestObservability(next http.Handler) http.Handler {
	return s.requestObservabilityWithGenerator(next, newRequestID)
}

// requestObservabilityWithGenerator is requestObservability with an injectable
// request ID generator for testing. It produces no fixed or zero ID on failure:
// a generator error is logged internally and answered with a generic HTTP 500
// before the downstream handler runs.
func (s *Server) requestObservabilityWithGenerator(next http.Handler, generate func() (string, error)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		id, err := resolveRequestID(request.Header.Get("X-Request-ID"), generate)
		if err != nil {
			s.log.Error(errRequestIDGeneration.Error(), "error", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		request = request.WithContext(withRequestID(request.Context(), id))
		w.Header().Set("X-Request-ID", id)
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(recorder, request)
		if shouldLogAccess(request.URL.Path, recorder.status) {
			s.log.Info("http request",
				"request_id", id,
				"method", request.Method,
				"path", request.URL.Path,
				"status", recorder.status,
				"bytes", recorder.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		}
	})
}

// shouldLogAccess decides whether to write an access log line for a request.
// Liveness probes and healthy readiness probes are skipped to avoid spam; a
// failing readiness probe is still logged by its own error path.
func shouldLogAccess(path string, status int) bool {
	if path == "/healthz" {
		return false
	}
	if path == "/readyz" && status == http.StatusOK {
		return false
	}
	return true
}
