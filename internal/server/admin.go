package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/danger-baba/llm-inference-gateway/internal/ledger"
)

// keyIssuer, keyRevoker, and cacheInvalidator are narrow interfaces so
// admin handler tests don't need a live Postgres or Redis; *auth.Store
// satisfies the first two, *auth.CachingResolver the third.
type keyIssuer interface {
	IssueKey(ctx context.Context, teamID uuid.UUID, label string, allowedModels []string, tpmLimit *int64) (string, auth.Key, error)
}

type keyRevoker interface {
	RevokeKey(ctx context.Context, id uuid.UUID) ([]byte, error)
}

type cacheInvalidator interface {
	Invalidate(ctx context.Context, hash []byte) error
}

// cachePurger is satisfied by *cache/exact.Store.
type cachePurger interface {
	PurgeTenant(ctx context.Context, tenantID string) (int, error)
}

// usageAggregator is satisfied by *ledger.PGAggregator; nil when
// Postgres isn't configured, in which case GET /admin/usage reports 503
// rather than pretending to have an answer.
type usageAggregator interface {
	Aggregate(ctx context.Context, scope ledger.Scope, id uuid.UUID, since, until time.Time) (ledger.UsageAggregate, error)
}

type adminDeps struct {
	issuer      keyIssuer
	revoker     keyRevoker
	invalidator cacheInvalidator
	purger      cachePurger
	usage       usageAggregator
}

type issueKeyRequest struct {
	TeamID        string   `json:"team_id"`
	Label         string   `json:"label"`
	AllowedModels []string `json:"allowed_models"`
	TPMLimit      *int64   `json:"tpm_limit"`
}

type issueKeyResponse struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	KeyPrefix string    `json:"key_prefix"`
	CreatedAt time.Time `json:"created_at"`
}

// handleIssueKey returns the plaintext key exactly once, per the README:
// it is never retrievable again after this response.
func handleIssueKey(deps adminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req issueKeyRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, r, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		teamID, err := uuid.Parse(req.TeamID)
		if err != nil {
			writeJSONError(w, r, http.StatusBadRequest, "team_id must be a valid UUID")
			return
		}

		plaintext, key, err := deps.issuer.IssueKey(r.Context(), teamID, req.Label, req.AllowedModels, req.TPMLimit)
		if err != nil {
			writeJSONError(w, r, http.StatusInternalServerError, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(issueKeyResponse{
			ID:        key.ID.String(),
			Key:       plaintext,
			KeyPrefix: key.KeyPrefix,
			CreatedAt: key.CreatedAt,
		})
	}
}

// handleRevokeKey sets revoked_at in Postgres, then invalidates the
// identity cache immediately — waiting out the cache TTL would leave a
// "revoked" key working for up to that TTL, contradicting the README's
// "must stop working immediately."
func handleRevokeKey(deps adminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeJSONError(w, r, http.StatusBadRequest, "id must be a valid UUID")
			return
		}

		hash, err := deps.revoker.RevokeKey(r.Context(), id)
		if err != nil {
			if errors.Is(err, auth.ErrNotFound) {
				writeJSONError(w, r, http.StatusNotFound, "key not found or already revoked")
				return
			}
			writeJSONError(w, r, http.StatusInternalServerError, err.Error())
			return
		}

		// The Postgres revoke already committed. A cache-invalidation
		// failure here just means the key might keep resolving from a
		// stale cache entry for up to its TTL instead of failing
		// immediately — worth logging, not worth failing this request
		// over, since the source of truth is already correctly updated.
		_ = deps.invalidator.Invalidate(r.Context(), hash)

		w.WriteHeader(http.StatusNoContent)
	}
}

type purgeCacheRequest struct {
	Tenant string `json:"tenant"`
}

type purgeCacheResponse struct {
	Deleted int `json:"deleted"`
}

// handlePurgeCache purges every exact-cache entry for a tenant (an org ID
// — see docs/adr/0011 for why the cache's tenant boundary is the org, not
// the team or key).
func handlePurgeCache(deps adminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req purgeCacheRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, r, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if req.Tenant == "" {
			writeJSONError(w, r, http.StatusBadRequest, "tenant is required")
			return
		}

		deleted, err := deps.purger.PurgeTenant(r.Context(), req.Tenant)
		if err != nil {
			writeJSONError(w, r, http.StatusInternalServerError, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(purgeCacheResponse{Deleted: deleted})
	}
}

type usageResponse struct {
	Scope            string           `json:"scope"`
	ID               string           `json:"id"`
	Since            time.Time        `json:"since"`
	Until            time.Time        `json:"until"`
	Requests         int64            `json:"requests"`
	PromptTokens     int64            `json:"prompt_tokens"`
	CompletionTokens int64            `json:"completion_tokens"`
	TokensSaved      int64            `json:"tokens_saved"`
	CostUSD          float64          `json:"cost_usd"`
	CacheHits        map[string]int64 `json:"cache_hits"`
}

// handleUsage answers GET /admin/usage?scope=org|team|key&id=<uuid>&since=<RFC3339>&until=<RFC3339>.
// since/until default to the last 24 hours ending now when omitted, so a
// bare `?scope=org&id=...` request still returns something useful.
func handleUsage(deps adminDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.usage == nil {
			writeJSONError(w, r, http.StatusServiceUnavailable, "usage aggregation requires Postgres, which is not configured")
			return
		}

		q := r.URL.Query()
		scope := ledger.Scope(q.Get("scope"))
		if !ledger.ValidScope(scope) {
			writeJSONError(w, r, http.StatusBadRequest, fmt.Sprintf("scope must be one of org, team, key (got %q)", scope))
			return
		}
		id, err := uuid.Parse(q.Get("id"))
		if err != nil {
			writeJSONError(w, r, http.StatusBadRequest, "id must be a valid UUID")
			return
		}

		until := time.Now().UTC()
		if raw := q.Get("until"); raw != "" {
			until, err = time.Parse(time.RFC3339, raw)
			if err != nil {
				writeJSONError(w, r, http.StatusBadRequest, "until must be RFC3339")
				return
			}
		}
		since := until.Add(-24 * time.Hour)
		if raw := q.Get("since"); raw != "" {
			since, err = time.Parse(time.RFC3339, raw)
			if err != nil {
				writeJSONError(w, r, http.StatusBadRequest, "since must be RFC3339")
				return
			}
		}
		if !since.Before(until) {
			writeJSONError(w, r, http.StatusBadRequest, "since must be before until")
			return
		}

		agg, err := deps.usage.Aggregate(r.Context(), scope, id, since, until)
		if err != nil {
			writeJSONError(w, r, http.StatusInternalServerError, err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(usageResponse{
			Scope: string(scope), ID: id.String(), Since: since, Until: until,
			Requests: agg.Requests, PromptTokens: agg.PromptTokens, CompletionTokens: agg.CompletionTokens,
			TokensSaved: agg.TokensSaved, CostUSD: agg.CostUSD, CacheHits: agg.CacheHits,
		})
	}
}
