package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/danger-baba/llm-inference-gateway/internal/cache/exact"
	"github.com/danger-baba/llm-inference-gateway/internal/cache/semantic"
	"github.com/danger-baba/llm-inference-gateway/internal/ledger"
	"github.com/danger-baba/llm-inference-gateway/internal/metrics"
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
	Set(ctx context.Context, key string, resp *providers.CanonicalResponse, ttlOverride *time.Duration) error
}

type embedder interface {
	Embed(text string) ([]float32, error)
}

type semanticCache interface {
	Get(q semantic.Query) (*providers.CanonicalResponse, float32, bool)
	Set(q semantic.Query, resp *providers.CanonicalResponse, ttlOverride *time.Duration)
}

// ledgerRecorder is satisfied by *ledger.Writer; nil when Postgres isn't
// configured, in which case usage simply isn't recorded (metrics still
// are, since those don't depend on Postgres).
type ledgerRecorder interface {
	Record(e ledger.Entry)
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
	// cacheMaxClientTTL is config.CacheConfig.MaxClientTTL: zero disables
	// the per-request cache_ttl hint entirely (docs/adr/0017), so a
	// client-supplied value is accepted but has no effect.
	cacheMaxClientTTL time.Duration

	embedder      embedder      // nil when cache.semantic.enabled is false, or the model failed to load
	semanticCache semanticCache // nil under the same conditions as embedder

	ledger ledgerRecorder // nil when Postgres isn't configured

	logger *slog.Logger
	// logRequestBodies gates a debug-level log of the actual prompt and
	// response content. Off by default -- see docs/adr/0014.
	logRequestBodies bool
}

func handleChatCompletions(deps chatDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
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
		ttlOverride, err := resolveCacheTTLOverride(req.CacheTTL, deps.cacheMaxClientTTL)
		if err != nil {
			writeJSONError(w, r, http.StatusBadRequest, err.Error())
			return
		}
		maybeLogPromptBody(r.Context(), deps, req.Messages)

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
			handleStreamingChatCompletion(w, r, deps, &req, identity, tiers, cost, scopes, promptTokens, start, ttlOverride)
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
			recordCacheHitObservability(r.Context(), deps, identity, tier, cached, time.Since(start))
			if len(cached.Choices) > 0 {
				maybeLogResponseBody(r.Context(), deps, cached.Choices[0].Message.Content)
			}
			return
		}

		result, err := deps.engine.Execute(r.Context(), tiers, &req)
		if err != nil {
			// Nothing was served: refund the entire reservation rather
			// than letting a failed request sit charged against the budget.
			_ = deps.limiter.Adjust(r.Context(), scopes, cost)
			status, lastProvider := writeEngineError(w, r, err)
			recordFailureObservability(status, lastProvider)
			return
		}

		result.Response.Model = req.Model // report back what the client asked for, not the vendor's internal name

		w.Header().Set("X-Gateway-Provider", result.Provider)
		w.Header().Set("X-Gateway-Attempts", formatAttempts(result.Attempts))
		w.Header().Set("X-Gateway-Cache", "none")
		if ttlOverride != nil {
			w.Header().Set("X-Gateway-Cache-TTL", formatCacheTTL(*ttlOverride))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(result.Response)

		// Everything from here happens after the response is already on
		// the wire, and never affects it: caching a completed response
		// and reconciling its reservation are both settle-asynchronously
		// concerns, not response-path ones.
		storeInCaches(r.Context(), deps, lookup, result.Response, ttlOverride)
		actual := int64(result.Response.Usage.PromptTokens + result.Response.Usage.CompletionTokens)
		_ = deps.limiter.Adjust(r.Context(), scopes, cost-actual)
		recordSuccessObservability(r.Context(), deps, identity, result.Provider, result.Model,
			result.Response.Usage, len(result.Attempts), time.Since(start), result.ProviderDuration)
		if len(result.Response.Choices) > 0 {
			maybeLogResponseBody(r.Context(), deps, result.Response.Choices[0].Message.Content)
		}
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
// partial stream"). ttlOverride is what resolveCacheTTLOverride computed
// for this request; a value <= 0 means the client asked for this
// response not to be cached at all (docs/adr/0017), so neither tier is
// written.
func storeInCaches(ctx context.Context, deps chatDeps, lookup cacheLookup, resp *providers.CanonicalResponse, ttlOverride *time.Duration) {
	if ttlOverride != nil && *ttlOverride <= 0 {
		return
	}
	if lookup.exactCacheKey != "" && deps.cache != nil {
		_ = deps.cache.Set(ctx, lookup.exactCacheKey, resp, ttlOverride)
	}
	if lookup.semanticEligible {
		deps.semanticCache.Set(lookup.semanticQuery, resp, ttlOverride)
	}
}

// resolveCacheTTLOverride parses a client's cache_ttl request hint (see
// providers.CanonicalRequest.CacheTTL) into what should actually be
// applied: nil means "no override -- use the tier's own configured TTL."
// The hint is silently ignored, never an error, whenever the operator
// hasn't opted into this feature (maxClientTTL <= 0, config.CacheConfig's
// zero value) -- a client hint aimed at a deployment that never enabled
// it shouldn't break requests to one that hasn't. A parsed value <= 0
// means "don't cache this response at all"; a value over maxClientTTL is
// capped to it, since the operator's ceiling always wins (docs/adr/0017).
func resolveCacheTTLOverride(raw string, maxClientTTL time.Duration) (*time.Duration, error) {
	if raw == "" || maxClientTTL <= 0 {
		return nil, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid cache_ttl %q: %w", raw, err)
	}
	if d > maxClientTTL {
		d = maxClientTTL
	}
	return &d, nil
}

// formatCacheTTL renders the applied TTL for X-Gateway-Cache-TTL: "0" for
// "not cached at all," otherwise the duration's own String() form (e.g.
// "24h0m0s"), matching the same unambiguous format the client sent.
func formatCacheTTL(d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	return d.String()
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
	metrics.RateLimitRejectionsTotal.WithLabelValues(d.LimitingScope).Inc()
	metrics.RequestsTotal.WithLabelValues(strconv.Itoa(http.StatusTooManyRequests), "", "none").Inc()
}

// requestUUID parses the request ID context already carries (a real
// UUID as of docs/adr/0013's requestid.go change) for use as
// ledger.Entry.RequestID. A parse failure can't happen in practice since
// this gateway is the one that generated the ID, but degrading to
// uuid.Nil rather than panicking costs nothing.
func requestUUID(ctx context.Context) uuid.UUID {
	id, _ := uuid.Parse(requestIDFromContext(ctx))
	return id
}

// recordCacheHitObservability is shared by the streaming and
// non-streaming cache-hit paths: same metrics, same ledger row shape,
// since a cache hit looks identical to an observer regardless of which
// response format the client asked for.
func recordCacheHitObservability(ctx context.Context, deps chatDeps, identity auth.Identity, tier string, cached *providers.CanonicalResponse, elapsed time.Duration) {
	metrics.CacheHitsTotal.WithLabelValues(tier).Inc()
	metrics.RequestsTotal.WithLabelValues(strconv.Itoa(http.StatusOK), "", tier).Inc()
	metrics.RequestDuration.Observe(elapsed.Seconds())

	tokensSaved := cached.Usage.PromptTokens + cached.Usage.CompletionTokens
	metrics.TokensSavedTotal.Add(float64(tokensSaved))

	if deps.ledger == nil {
		return
	}
	deps.ledger.Record(ledger.Entry{
		RequestID: requestUUID(ctx), OrgID: identity.OrgID, TeamID: identity.TeamID, VirtualKeyID: identity.KeyID,
		Model:       cached.Model,
		TokensSaved: tokensSaved, CacheTier: tier,
		StatusCode: http.StatusOK, LatencyMS: int(elapsed.Milliseconds()),
	})
}

// recordSuccessObservability is shared by the streaming and
// non-streaming success paths (a stream that completes cleanly is, from
// the ledger's point of view, exactly the same shape of row as a
// non-streaming success).
func recordSuccessObservability(
	ctx context.Context, deps chatDeps, identity auth.Identity,
	provider, model string, usage providers.Usage, attempts int,
	elapsed, providerDuration time.Duration,
) {
	overhead := elapsed - providerDuration
	if overhead < 0 {
		overhead = 0 // clock/measurement noise, not a real negative cost
	}
	metrics.RequestsTotal.WithLabelValues(strconv.Itoa(http.StatusOK), provider, "none").Inc()
	metrics.RequestDuration.Observe(elapsed.Seconds())
	metrics.ProxyOverhead.Observe(overhead.Seconds())
	metrics.TokensTotal.WithLabelValues("prompt").Add(float64(usage.PromptTokens))
	metrics.TokensTotal.WithLabelValues("completion").Add(float64(usage.CompletionTokens))

	inPerMTok, outPerMTok := deps.engine.Pricing(provider, model)
	costUSD := float64(usage.PromptTokens)/1e6*inPerMTok + float64(usage.CompletionTokens)/1e6*outPerMTok
	metrics.CostUSDTotal.Add(costUSD)

	if deps.ledger == nil {
		return
	}
	deps.ledger.Record(ledger.Entry{
		RequestID: requestUUID(ctx), OrgID: identity.OrgID, TeamID: identity.TeamID, VirtualKeyID: identity.KeyID,
		Provider: provider, Model: model,
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		CostUSD: costUSD, CacheTier: "none",
		Attempts: attempts, StatusCode: http.StatusOK, LatencyMS: int(elapsed.Milliseconds()),
	})
}

// recordFailureObservability covers a request that never got a
// response at all -- no ledger row, since there's no meaningful
// provider/model/usage to attribute one to; only the outcome mix metric
// moves.
func recordFailureObservability(status int, lastProvider string) {
	metrics.RequestsTotal.WithLabelValues(strconv.Itoa(status), lastProvider, "none").Inc()
}

// maybeLogPromptBody and maybeLogResponseBody are the only two places in
// this package that can ever write prompt or completion content to logs,
// and both are no-ops unless logRequestBodies is explicitly true (README,
// Observability: "not logged by default; a config flag enables it for
// debugging"). Kept as named, single-purpose functions rather than
// inlined checks so grep for "logRequestBodies" finds every site that
// could leak user data into logs, in one place.
func maybeLogPromptBody(ctx context.Context, deps chatDeps, messages []providers.Message) {
	if !deps.logRequestBodies {
		return
	}
	deps.logger.Debug("prompt body", "request_id", requestUUID(ctx), "messages", messages)
}

func maybeLogResponseBody(ctx context.Context, deps chatDeps, content string) {
	if !deps.logRequestBodies {
		return
	}
	deps.logger.Debug("response body", "request_id", requestUUID(ctx), "content", content)
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
// breaker was open, 502 for anything else (e.g. a raw network error). It
// returns that status and the last provider actually attempted (empty if
// none were, e.g. every breaker was open) so the caller can record
// gateway_requests_total without re-deriving either.
func writeEngineError(w http.ResponseWriter, r *http.Request, err error) (status int, lastProvider string) {
	var retryErr *retry.Error
	var attempts []retry.Attempt
	if errors.As(err, &retryErr) {
		attempts = retryErr.Attempts
	}
	if len(attempts) > 0 {
		w.Header().Set("X-Gateway-Attempts", formatAttempts(attempts))
		lastProvider = attempts[len(attempts)-1].Provider
	}

	status = http.StatusBadGateway
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
	return status, lastProvider
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
