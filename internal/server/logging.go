package server

import (
	"log/slog"
	"net/http"
	"time"
)

// statusCapturingWriter records the status code a handler actually wrote
// so the logging middleware can report it, without changing what the
// client receives. It must implement http.Flusher and delegate to the
// underlying ResponseWriter's Flush -- the streaming handler type-asserts
// for it, and losing that assertion here would silently break streaming
// for every request that passes through logging.
type statusCapturingWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusCapturingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Write covers handlers that never call WriteHeader explicitly --
// net/http itself defaults to 200 in that case, so this does too.
func (w *statusCapturingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusCapturingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withRequestLogging emits one structured JSON log line per request,
// carrying the same request ID returned as X-Request-Id (README,
// Observability), after the handler returns. It never logs request or
// response bodies -- see chatDeps.logRequestBodies in chat.go for the
// separate, explicitly-gated path that can.
func withRequestLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusCapturingWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		logger.Info("request",
			"request_id", requestIDFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
