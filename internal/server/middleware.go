package server

import (
	"context"
	"net/http"
	"time"
)

// withMaxBody caps request body size so a client can't exhaust memory with
// an oversized payload before any handler-level validation runs.
func withMaxBody(max int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, max)
		next.ServeHTTP(w, r)
	})
}

// withRequestTimeout bounds every request's context to request_timeout.
// This is the deadline every downstream call (provider hop, cache lookup,
// rate-limit check) will derive its own per-attempt timeout from in later
// phases.
func withRequestTimeout(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
