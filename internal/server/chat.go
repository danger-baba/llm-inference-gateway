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

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/danger-baba/llm-inference-gateway/internal/cache/exact"
	"github.com/danger-baba/llm-inference-gateway/internal/cache/semantic"
	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/ratelimit"
	"github.com/danger-baba/llm-inference-gateway/internal/retry"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

// tokenCounter, rateLimiter, exactCache, embedder, and semanticCache are
// narrow interfaces so the handler is testable without tiktoken-go, a
// live Redis, or a loaded ONNX model; the concrete
// tokenizer/ratelimit/cache/exact/embedding/cache-semantic types satisfy
// them respectively.
type tokenCounter interface {
	CountMessages(messages []providers.Message) int
	// CountText counts one fragment on its own, with no message-framing
	// overhead -- what a streaming completion needs, since it arrives as
	// a sequence of content fragments, not one message. See
	// docs/adr/0013.
	CountText(text string) int
}

type rateLimiter interface {
	Reserve(ctx context.Context, s ratelimit.Scopes, cost int64) (ratelimit.Decision, error)
	Adjust(ctx context.Context, s ratelimit.Scopes, delta int64) error
}

type exactCache interface {
	Get(ctx context.Context, key string) (*providers.CanonicalResponse, bool, error)
	Set(ctx context.Context, key string, resp *providers.CanonicalResponse) error
}

type embedder interface {
	Embed(text string) ([]float32, error)
}

type semanticCache interface {
	Get(q semantic.Query) (*providers.CanonicalResponse, float32, bool)
	Set(q semantic.Query, resp *providers.CanonicalResponse)
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

	cache                   exactCache // nil when cache.exact.enabled is false
	cacheNonzeroTemperature bool

	embedder      embedder      // nil when cache.semantic.enabled is false, or the model failed to load
	semanticCache semanticCache // nil under the same conditions as embedder
}

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

		promptTokens := deps.counter.CountMessages(req.Messages)
		estimatedCompletion := deps.estimateCompletionTokens
		if req.MaxTokens != nil {
			estimatedCompletion = int64(*req.MaxTokens)
		}
		cost := int64(promptTokens) + estimatedCompletion

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

		if req.Stream {
			handleStreamingChatCompletion(w, r, deps, &req, identity, tiers, cost, scopes, promptTokens)
			return
		}

		// Cache lookups happen regardless of the stream flag -- it
		// deliberately doesn't participate in the cache key (README,
		// Tier-1 cache) -- so a non-streaming request can be served from
		// an entry a streaming request populated, and vice versa.
		cached, tier, lookup := checkCaches(r.Context(), deps, &req, identity)
		if cached != nil {
			_ = deps.limiter.Adjust(r.Context(), scopes, cost)
			writeCachedResponse(w, tier, cached)
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
		w.Header().Set("X-Gateway-Cache", "none")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(result.Response)

		// Everything from here happens after the response is already on
		// the wire, and never affects it: caching a completed response
		// and reconciling its reservation are both settle-asynchronously
		// concerns, not response-path ones.
		storeInCaches(r.Context(), deps, lookup, result.Response)
		actual := int64(result.Response.Usage.PromptTokens + result.Response.Usage.CompletionTokens)
		_ = deps.limiter.Adjust(r.Context(), scopes, cost-actual)
	}
}

// cacheLookup carries whatever checkCaches learned about a request's
// cache eligibility forward to storeInCaches, so a fresh completion
// (streamed or not) can populate the same tiers a lookup just missed.
type cacheLookup struct {
	exactCacheKey    string
	semanticQuery    semantic.Query
	semanticEligible bool
}

// checkCaches consults Tier-1, then Tier-2 on a Tier-1 miss, exactly as
// the request lifecycle describes. It returns a hit's response and which
// tier served it ("exact" or "semantic"), or (nil, "", ...) on a miss --
// in which case lookup carries what a caller needs to populate the cache
// once it has a real response to store.
func checkCaches(ctx context.Context, deps chatDeps, req *providers.CanonicalRequest, identity auth.Identity) (*providers.CanonicalResponse, string, cacheLookup) {
	var lookup cacheLookup
	cacheEligible := exact.Eligible(req, deps.cacheNonzeroTemperature)

	if deps.cache != nil && cacheEligible {
		if canon, err := exact.Canonicalize(req); err == nil {
			lookup.exactCacheKey = exact.Key(identity.OrgID.String(), canon)
			if cached, hit, err := deps.cache.Get(ctx, lookup.exactCacheKey); err == nil && hit {
				return cached, "exact", lookup
			}
		}
	}

	// A vector hit is rejected unless the non-semantic parameters (model,
	// tools, response_format) also match exactly -- similarity alone is
	// insufficient (README, Tier-2 cache).
	lookup.semanticEligible = deps.embedder != nil && deps.semanticCache != nil && cacheEligible
	if lookup.semanticEligible {
		toolsCanon, _ := exact.CanonicalizeJSON(req.Tools)
		formatCanon, _ := exact.CanonicalizeJSON(req.ResponseFormat)
		if vector, err := deps.embedder.Embed(concatUserTurns(req.Messages)); err == nil {
			lookup.semanticQuery = semantic.Query{
				TenantID: identity.OrgID.String(), Model: req.Model,
				ToolsCanonical: string(toolsCanon), ResponseFormatCanonical: string(formatCanon),
				Vector: vector,
			}
			if cached, _, hit := deps.semanticCache.Get(lookup.semanticQuery); hit {
				return cached, "semantic", lookup
			}
		} else {
			lookup.semanticEligible = false // embedding this request failed; don't try to Set() below either
		}
	}

	return nil, "", lookup
}

// storeInCaches populates whichever tiers checkCaches found this request
// eligible for. Only called once resp is a genuinely complete response --
// never for a partial stream (README, Tier-2 cache: "never cache a
// partial stream").
func storeInCaches(ctx context.Context, deps chatDeps, lookup cacheLookup, resp *providers.CanonicalResponse) {
	if lookup.exactCacheKey != "" && deps.cache != nil {
		_ = deps.cache.Set(ctx, lookup.exactCacheKey, resp)
	}
	if lookup.semanticEligible {
		deps.semanticCache.Set(lookup.semanticQuery, resp)
	}
}

// concatUserTurns embeds only the user's own words: system prompts and
// prior assistant replies would pull the embedding toward whatever the
// gateway or the model said, not what this particular question was about.
func concatUserTurns(messages []providers.Message) string {
	var b strings.Builder
	for _, m := range messages {
		if m.Role != "user" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(m.Content)
	}
	return b.String()
}

func writeCachedResponse(w http.ResponseWriter, tier string, resp *providers.CanonicalResponse) {
	w.Header().Set("X-Gateway-Cache", tier)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
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
