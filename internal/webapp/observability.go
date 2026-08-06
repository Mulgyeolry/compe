package webapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// normalizeRequestID accepts a client-supplied X-Request-ID only when it is
// well-formed; otherwise it generates a fresh random ID.
func normalizeRequestID(provided string) string {
	if validRequestID(provided) {
		return provided
	}
	return newRequestID()
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

// newRequestID returns a 32-character lowercase hex string from 16 random
// bytes. It uses crypto/rand exclusively; no timestamp or counter is involved.
func newRequestID() string {
	var buffer [16]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		// crypto/rand.Read on a fixed-size buffer never fails on supported
		// platforms; keep the zero buffer rather than falling back to a
		// non-cryptographic source.
		return hex.EncodeToString(buffer[:])
	}
	return hex.EncodeToString(buffer[:])
}

// responseRecorder wraps an http.ResponseWriter to capture the response status
// and body byte count for access logging. Unwrap exposes the underlying writer
// so http.ResponseController keeps working.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// requestObservability assigns every request a request ID, echoes it in the
// X-Request-ID response header, places it in the request context, and records a
// structured access log after the handler runs. Probe endpoints are suppressed
// to avoid log spam.
func (s *Server) requestObservability(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		id := normalizeRequestID(request.Header.Get("X-Request-ID"))
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
