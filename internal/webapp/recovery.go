package webapp

import (
	"fmt"
	"net/http"
	"runtime/debug"
)

// recoveryWriter tracks whether the downstream handler has committed a
// response, so the panic handler can decide between returning a clean 500 and
// aborting an already-corrupted connection.
type recoveryWriter struct {
	http.ResponseWriter
	committed *bool
}

func (r *recoveryWriter) WriteHeader(status int) {
	*r.committed = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *recoveryWriter) Write(p []byte) (int, error) {
	*r.committed = true
	return r.ResponseWriter.Write(p)
}

func (r *recoveryWriter) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recoveryWriter) Flush() {
	*r.committed = true
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// recoverPanics turns a panicking handler into a structured error response. If
// the response was not yet committed it answers a generic HTTP 500 (leaking no
// panic detail) and lets the outer access log record 500. If a partial response
// was already sent it logs and re-panics with http.ErrAbortHandler so net/http
// aborts the connection instead of appending error text to a committed body.
// An explicit http.ErrAbortHandler from the downstream handler is re-panicked
// verbatim without conversion or duplicate logging.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		committed := false
		recorder := &recoveryWriter{ResponseWriter: w, committed: &committed}
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}
			s.log.Error("web panic recovered",
				"request_id", requestIDFromContext(request.Context()),
				"method", request.Method,
				"path", request.URL.Path,
				"panic", fmt.Sprintf("%v", recovered),
				"stack", string(debug.Stack()),
				"response_committed", committed,
			)
			if committed {
				panic(http.ErrAbortHandler)
			}
			http.Error(w, "服务器暂时无法处理该请求，请稍后重试。", http.StatusInternalServerError)
		}()
		next.ServeHTTP(recorder, request)
	})
}
