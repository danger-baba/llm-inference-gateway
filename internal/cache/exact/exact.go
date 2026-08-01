// Package exact implements the Tier-1 exact-match cache: a canonical form
// of the request hashed into a tenant-prefixed Redis key.
package exact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

// canonicalFields is deliberately its own type, not providers.CanonicalRequest:
// only these fields participate in the cache key. stream, user, metadata,
// and any trace headers are excluded because none of them change the
// generated content.
type canonicalFields struct {
	Model          string              `json:"model"`
	Messages       []providers.Message `json:"messages"`
	Temperature    *float64            `json:"temperature,omitempty"`
	TopP           *float64            `json:"top_p,omitempty"`
	MaxTokens      *int                `json:"max_tokens,omitempty"`
	Stop           []string            `json:"stop,omitempty"`
	Seed           *int                `json:"seed,omitempty"`
	ResponseFormat json.RawMessage     `json:"response_format,omitempty"`
	Tools          json.RawMessage     `json:"tools,omitempty"`
}

// Canonicalize builds the deterministic JSON form of req used to derive
// the cache key. model is the request's own model field (the client-facing
// alias), used as-is: routing (which provider ultimately serves the
// request) happens after the cache check in the request lifecycle, so the
// key can't depend on which provider ends up handling it. See
// docs/adr/0011.
//
// JSON object keys are sorted recursively — round-tripping response_format
// and tools through interface{} relies on encoding/json always sorting
// map[string]interface{} keys on Marshal, which it does at every nesting
// level, not just the top one — so two requests differing only in field
// order produce byte-identical canonical output.
func Canonicalize(req *providers.CanonicalRequest) ([]byte, error) {
	responseFormat, err := CanonicalizeJSON(req.ResponseFormat)
	if err != nil {
		return nil, fmt.Errorf("exact: canonicalize response_format: %w", err)
	}
	tools, err := CanonicalizeJSON(req.Tools)
	if err != nil {
		return nil, fmt.Errorf("exact: canonicalize tools: %w", err)
	}

	fields := canonicalFields{
		Model:          req.Model,
		Messages:       req.Messages,
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		MaxTokens:      req.MaxTokens,
		Stop:           req.Stop,
		Seed:           req.Seed,
		ResponseFormat: responseFormat,
		Tools:          tools,
	}
	return json.Marshal(fields)
}

// CanonicalizeJSON sorts an arbitrary JSON value's object keys recursively
// (round-tripping through interface{} relies on encoding/json always
// sorting map[string]interface{} keys on Marshal). Exported because the
// semantic cache's guard rail needs the same canonical form for tools and
// response_format that the exact cache's key already uses.
func CanonicalizeJSON(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// Eligible reports whether req should even be looked up/stored in the
// exact cache. A nil Temperature means "use the provider's own default,"
// which is commonly 1.0 (creative, non-deterministic) — not 0 — so it is
// treated the same as any other nonzero temperature unless the deployment
// has opted into caching nonzero-temperature requests.
func Eligible(req *providers.CanonicalRequest, cacheNonzeroTemperature bool) bool {
	if cacheNonzeroTemperature {
		return true
	}
	return req.Temperature != nil && *req.Temperature == 0
}

// Key derives the tenant-scoped cache key from a canonical JSON form.
func Key(tenantID string, canonicalJSON []byte) string {
	sum := sha256.Sum256(canonicalJSON)
	return fmt.Sprintf("cache:exact:%s:%s", tenantID, hex.EncodeToString(sum[:]))
}

// Store reads and writes cached responses in Redis.
type Store struct {
	client *redis.Client
	ttl    time.Duration
}

func NewStore(client *redis.Client, ttl time.Duration) *Store {
	return &Store{client: client, ttl: ttl}
}

func (s *Store) Get(ctx context.Context, key string) (*providers.CanonicalResponse, bool, error) {
	raw, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("exact: get: %w", err)
	}

	var resp providers.CanonicalResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		return nil, false, fmt.Errorf("exact: decode cached response: %w", err)
	}
	return &resp, true, nil
}

func (s *Store) Set(ctx context.Context, key string, resp *providers.CanonicalResponse) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("exact: encode response: %w", err)
	}
	if err := s.client.Set(ctx, key, payload, s.ttl).Err(); err != nil {
		return fmt.Errorf("exact: set: %w", err)
	}
	return nil
}

// PurgeTenant deletes every cached entry for tenantID, via SCAN rather
// than KEYS so a large keyspace doesn't block Redis while purging.
func (s *Store) PurgeTenant(ctx context.Context, tenantID string) (int, error) {
	pattern := fmt.Sprintf("cache:exact:%s:*", tenantID)
	var deleted int
	iter := s.client.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		if err := s.client.Del(ctx, iter.Val()).Err(); err != nil {
			return deleted, fmt.Errorf("exact: purge: %w", err)
		}
		deleted++
	}
	if err := iter.Err(); err != nil {
		return deleted, fmt.Errorf("exact: purge scan: %w", err)
	}
	return deleted, nil
}
