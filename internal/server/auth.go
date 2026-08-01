package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
)

type identityCtxKey struct{}

// identityResolver is the narrow interface the auth middleware needs;
// *auth.CachingResolver satisfies it.
type identityResolver interface {
	Resolve(ctx context.Context, plaintextKey string) (auth.Identity, error)
}

// withBearerAuth gates a handler behind a valid, non-revoked virtual key.
// Only /v1/* client-facing routes go through this — see
// docs/adr/0008-admin-endpoints-are-unauthenticated.md for why /admin/*
// deliberately doesn't yet.
func withBearerAuth(resolver identityResolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			writeJSONError(w, r, http.StatusUnauthorized, "missing or malformed Authorization header")
			return
		}

		id, err := resolver.Resolve(r.Context(), token)
		if err != nil {
			status := http.StatusUnauthorized
			if !errors.Is(err, auth.ErrNotFound) && !errors.Is(err, auth.ErrRevoked) {
				status = http.StatusInternalServerError // e.g. Postgres itself failed, not "bad credentials"
			}
			writeJSONError(w, r, status, "invalid or revoked API key")
			return
		}

		ctx := context.WithValue(r.Context(), identityCtxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) || len(h) <= len(prefix) {
		return "", false
	}
	return h[len(prefix):], true
}

func identityFromContext(ctx context.Context) (auth.Identity, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(auth.Identity)
	return id, ok
}
