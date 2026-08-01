package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
)

// Pinger is satisfied by any dependency /readyz must check. The gateway's
// real Redis and Postgres clients are adapted to this in cmd/gateway, so
// this package never needs to import either driver.
type Pinger interface {
	Ping(ctx context.Context) error
}

func newMux(redis, postgres Pinger, chat chatDeps, resolver identityResolver, admin adminDeps) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", handleReadyz(redis, postgres))
	mux.Handle("/v1/chat/completions", withBearerAuth(resolver, handleChatCompletions(chat)))
	mux.HandleFunc("/v1/models", handleModels(chat.router.Aliases()))
	mux.HandleFunc("POST /admin/keys", handleIssueKey(admin))
	mux.HandleFunc("DELETE /admin/keys/{id}", handleRevokeKey(admin))
	mux.HandleFunc("POST /admin/cache/purge", handlePurgeCache(admin))
	return mux
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz pings both dependencies concurrently. Each Pinger is
// expected to apply its own bound (redis.dial_timeout, postgres.ping_timeout)
// internally, so a single slow dependency can't stall the other check.
func handleReadyz(redis, postgres Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var redisErr, postgresErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			redisErr = redis.Ping(r.Context())
		}()
		go func() {
			defer wg.Done()
			postgresErr = postgres.Ping(r.Context())
		}()
		wg.Wait()

		if err := errors.Join(redisErr, postgresErr); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "not ready: %v\n", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
