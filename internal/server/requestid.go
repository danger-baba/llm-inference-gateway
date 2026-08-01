package server

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

type requestIDKey struct{}

// newRequestID returns a real UUID, not just an arbitrary unique
// string: internal/ledger's usage_ledger.request_id column is typed
// UUID, so the ID handed out here must already be one, not something
// reformatted later.
func newRequestID() string {
	return uuid.NewString()
}

// withRequestID assigns every request a unique ID, returned as
// X-Request-Id and available to handlers/logging via context.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}
