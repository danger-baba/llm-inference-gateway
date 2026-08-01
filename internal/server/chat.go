package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/ratelimit"
	"github.com/danger-baba/llm-inference-gateway/internal/retry"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

// tokenCounter and rateLimiter are narrow interfaces so the handler is
// testable without tiktoken-go or a live Redis; *tokenizer.Counter and
// *ratelimit.Limiter satisfy them respectively.
type tokenCounter interface {
	CountMessages(messages []providers.Message) int
}

type rateLimiter interface {
	Reserve(ctx context.Context, s ratelimit.Scopes, cost int64) (ratelimit.Decision, error)
	Adjust(ctx context.Context, s ratelimit.Scopes, delta int64) error
}

// chatDeps is everything the chat completions handler needs to route and
// execute a request. It's a struct rather than loose Options fields so
// tests can build one directly without going through the full Server.
type chatDeps struct {
	router                   *router.Router
	engine                   *retry.Engine
	counter                  tokenCounter
	limiter                  rateLimiter
	defaultTPM               int64
	estimateCompletionTokens int64
}

const streamNotYetSupportedMsg = "streaming is not supported yet (lands in Phase 8); send stream:false or omit it"

func handleChatCompletions(deps chatDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req providers.CanonicalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, r, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if req.Model == "" {
			writeJSONError(w, r, http.StatusBadRequest, "model is required")
			return
		}
		if len(req.Messages) == 0 {
			writeJSONError(w, r, http.StatusBadRequest, "messages must not be empty")
			return
		}
		if req.Stream {
			writeJSONError(w, r, http.StatusNotImplemented, streamNotYetSupportedMsg)
			return
		}

		tiers, err := deps.router.Resolve(req.Model)
		if err != nil || len(tiers) == 0 {
			writeJSONError(w, r, http.StatusBadRequest, fmt.Sprintf("unknown model %q", req.Model))
			return
		}

		identity, ok := identityFromContext(r.Context())
		if !ok {
			// withBearerAuth always runs first in the real mux; reaching
			// here without an identity means the handler was wired wrong,
			// not that the client did anything bad.
			writeJSONError(w, r, http.StatusInternalServerError, "no identity resolved for this request")
			return
		}

		estimatedCompletion := deps.estimateCompletionTokens
		if req.MaxTokens != nil {
			estimatedCompletion = int64(*req.MaxTokens)
		}
		cost := int64(deps.counter.CountMessages(req.Messages)) + estimatedCompletion

		keyCapacity := deps.defaultTPM
		if identity.KeyTPMLimit != nil {
			keyCapacity = *identity.KeyTPMLimit
		}
		scopes := ratelimit.Scopes{
			OrgID: identity.OrgID.String(), TeamID: identity.TeamID.String(), KeyID: identity.KeyID.String(),
			OrgCapacity: identity.OrgTPMLimit, TeamCapacity: identity.TeamTPMLimit, KeyCapacity: keyCapacity,
		}

		decision, err := deps.limiter.Reserve(r.Context(), scopes, cost)
		if err != nil {
			writeJSONError(w, r, http.StatusInternalServerError, "rate limit check failed: "+err.Error())
			return
		}
		if !decision.Allowed {
			writeRateLimitRejection(w, r, decision)
			return
		}

		result, err := deps.engine.Execute(r.Context(), tiers, &req)
		if err != nil {
			// Nothing was served: refund the entire reservation rather
			// than letting a failed request sit charged against the budget.
			_ = deps.limiter.Adjust(r.Context(), scopes, cost)
			writeEngineError(w, r, err)
			return
		}

		result.Response.Model = req.Model // report back what the client asked for, not the vendor's internal name

		w.Header().Set("X-Gateway-Provider", result.Provider)
		w.Header().Set("X-Gateway-Attempts", formatAttempts(result.Attempts))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(result.Response)

		// Reconcile after the response is on the wire: refund the
		// difference if the estimate was too high, charge it if actual
		// usage ran over. Over-reserving briefly is correct; the
		// generation this reservation guarded against has already
		// happened either way, so this never blocks or fails the response.
		actual := int64(result.Response.Usage.PromptTokens + result.Response.Usage.CompletionTokens)
		_ = deps.limiter.Adjust(r.Context(), scopes, cost-actual)
	}
}

func writeRateLimitRejection(w http.ResponseWriter, r *http.Request, d ratelimit.Decision) {
	retryAfterSeconds := int(math.Ceil(d.RetryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
	w.Header().Set("X-RateLimit-Scope", d.LimitingScope)
	w.Header().Set("X-RateLimit-Remaining", "0")
	writeJSONError(w, r, http.StatusTooManyRequests, fmt.Sprintf("rate limit exceeded at %s scope", d.LimitingScope))
}

func formatAttempts(attempts []retry.Attempt) string {
	parts := make([]string, len(attempts))
	for i, a := range attempts {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

// writeEngineError maps a retry.Error to an HTTP status: the underlying
// provider's own status when there is one, 503 when every candidate's
// breaker was open, 502 for anything else (e.g. a raw network error).
func writeEngineError(w http.ResponseWriter, r *http.Request, err error) {
	var retryErr *retry.Error
	var attempts []retry.Attempt
	if errors.As(err, &retryErr) {
		attempts = retryErr.Attempts
	}
	if len(attempts) > 0 {
		w.Header().Set("X-Gateway-Attempts", formatAttempts(attempts))
	}

	status := http.StatusBadGateway
	switch {
	case errors.Is(err, retry.ErrNoHealthyProvider):
		status = http.StatusServiceUnavailable
	default:
		var apiErr *providers.APIError
		if errors.As(err, &apiErr) {
			status = apiErr.StatusCode
		}
	}
	writeJSONError(w, r, status, err.Error())
}

func writeJSONError(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message":    message,
			"request_id": requestIDFromContext(r.Context()),
		},
	})
}
