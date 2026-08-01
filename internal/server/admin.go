package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
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

type adminDeps struct {
	issuer      keyIssuer
	revoker     keyRevoker
	invalidator cacheInvalidator
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
