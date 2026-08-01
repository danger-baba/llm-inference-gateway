// Package ratelimit implements the token-aware, hierarchical rate
// limiter: one atomic Lua script checks org, team, and key token buckets
// before deducting from any of them, so a request can never partially
// deduct a scope and then get rejected by another.
package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed reserve.lua
var reserveScriptSrc string

//go:embed adjust.lua
var adjustScriptSrc string

var (
	reserveScript = redis.NewScript(reserveScriptSrc)
	adjustScript  = redis.NewScript(adjustScriptSrc)
)

// bucketTTL is twice the token bucket's one-minute refill window, per the
// README's Redis key layout table: idle tenants cost nothing, and a
// missing key is correctly interpreted as a full bucket.
const bucketTTL = 2 * time.Minute

// Scopes identifies the three hierarchy levels a request is checked
// against and each one's capacity in tokens per minute.
type Scopes struct {
	OrgID, TeamID, KeyID                   string
	OrgCapacity, TeamCapacity, KeyCapacity int64
}

type Decision struct {
	Allowed bool
	// RetryAfter and LimitingScope are only meaningful when !Allowed.
	RetryAfter    time.Duration
	LimitingScope string // "org" | "team" | "key"
	// Remaining is the key-level bucket balance after a successful
	// reservation, for X-RateLimit-Remaining.
	Remaining int64
}

// Limiter checks and adjusts token buckets in Redis. When failOpen is
// true, a Redis failure allows the request through instead of rejecting
// it — the README's deliberate availability-over-cost-control trade-off,
// config-switchable because a cost-sensitive deployment should invert it.
type Limiter struct {
	client   *redis.Client
	failOpen bool
}

func New(client *redis.Client, failOpen bool) *Limiter {
	return &Limiter{client: client, failOpen: failOpen}
}

func bucketKey(scope, id string) string {
	return fmt.Sprintf("bucket:%s:%s", scope, id)
}

// Reserve atomically checks all three scopes and, only if every one has
// room, deducts cost from all three in the same script invocation.
func (l *Limiter) Reserve(ctx context.Context, s Scopes, cost int64) (Decision, error) {
	keys := []string{bucketKey("org", s.OrgID), bucketKey("team", s.TeamID), bucketKey("key", s.KeyID)}
	res, err := reserveScript.Run(ctx, l.client, keys,
		s.OrgCapacity, s.TeamCapacity, s.KeyCapacity, cost, int64(bucketTTL.Seconds()),
	).Result()
	if err != nil {
		if l.failOpen {
			return Decision{Allowed: true}, nil
		}
		return Decision{}, fmt.Errorf("ratelimit: reserve: %w", err)
	}

	arr, ok := res.([]interface{})
	if !ok || len(arr) != 4 {
		return Decision{}, fmt.Errorf("ratelimit: unexpected script result: %#v", res)
	}
	allowed, _ := asInt64(arr[0])
	retryAfterMs, _ := asInt64(arr[1])
	scope, _ := arr[2].(string)
	remaining, _ := asInt64(arr[3])

	return Decision{
		Allowed:       allowed == 1,
		RetryAfter:    time.Duration(retryAfterMs) * time.Millisecond,
		LimitingScope: scope,
		Remaining:     remaining,
	}, nil
}

// Adjust reconciles a prior reservation against actual usage: a positive
// delta refunds an over-reservation, a negative delta charges the
// difference when actual usage exceeded the estimate. It never rejects
// and, other than logging-worthy failures, never returns an error that
// should affect a response that has already been served.
func (l *Limiter) Adjust(ctx context.Context, s Scopes, delta int64) error {
	if delta == 0 {
		return nil
	}
	keys := []string{bucketKey("org", s.OrgID), bucketKey("team", s.TeamID), bucketKey("key", s.KeyID)}
	_, err := adjustScript.Run(ctx, l.client, keys,
		s.OrgCapacity, s.TeamCapacity, s.KeyCapacity, delta, int64(bucketTTL.Seconds()),
	).Result()
	if err != nil {
		return fmt.Errorf("ratelimit: adjust: %w", err)
	}
	return nil
}

func asInt64(v interface{}) (int64, bool) {
	n, ok := v.(int64)
	return n, ok
}
