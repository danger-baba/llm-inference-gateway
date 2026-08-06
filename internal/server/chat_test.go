package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/danger-baba/llm-inference-gateway/internal/breaker"
	"github.com/danger-baba/llm-inference-gateway/internal/cache/semantic"
	"github.com/danger-baba/llm-inference-gateway/internal/config"
	"github.com/danger-baba/llm-inference-gateway/internal/ledger"
	"github.com/danger-baba/llm-inference-gateway/internal/metrics"
	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/providers/mock"
	"github.com/danger-baba/llm-inference-gateway/internal/ratelimit"
	"github.com/danger-baba/llm-inference-gateway/internal/retry"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

func lenientBreakerConfig() breaker.Config {
	return breaker.Config{
		ErrorRateThreshold: 0.5,
		MinRequests:        1000, // high enough that a handful of test calls never trips it
		Window:             time.Second,
		Cooldown:           time.Hour,
		CooldownMax:        time.Hour,
		HalfOpenProbes:     1,
	}
}

type fakeTokenCounter struct{ n int }

func (f fakeTokenCounter) CountMessages(_ []providers.Message) int { return f.n }

// CountText counts words rather than returning a fixed number: streaming
// tests need completion-token accounting to actually move as content
// arrives, not stay pinned at a constant.
func (f fakeTokenCounter) CountText(text string) int { return len(strings.Fields(text)) }

type fakeRateLimiter struct {
	allow         bool
	limitingScope string
	retryAfter    time.Duration
	adjustCalls   int
	lastDelta     int64
	reserveCalls  int
}

func (f *fakeRateLimiter) Reserve(_ context.Context, _ ratelimit.Scopes, _ int64) (ratelimit.Decision, error) {
	f.reserveCalls++
	return ratelimit.Decision{Allowed: f.allow, LimitingScope: f.limitingScope, RetryAfter: f.retryAfter, Remaining: 100}, nil
}

func (f *fakeRateLimiter) Adjust(_ context.Context, _ ratelimit.Scopes, delta int64) error {
	f.adjustCalls++
	f.lastDelta = delta
	return nil
}

func alwaysAllowLimiter() *fakeRateLimiter {
	return &fakeRateLimiter{allow: true}
}

func testDepsWithProvider(t *testing.T, name string, p providers.Provider) chatDeps {
	t.Helper()
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: name, Priority: 0, Weight: 1}},
		ModelAliases: map[string]map[string]string{
			"fast": {name: "mock-model-v1"},
		},
	}
	reg := breaker.NewRegistry(lenientBreakerConfig())
	engine := retry.New(reg, map[string]providers.Provider{name: p}, map[string]time.Duration{name: 5 * time.Second}, retry.Config{
		MaxAttemptsPerProvider: 2,
		BaseBackoff:            time.Millisecond,
		MaxBackoff:             5 * time.Millisecond,
	})
	return chatDeps{
		router:                   router.New(cfg),
		engine:                   engine,
		counter:                  fakeTokenCounter{n: 10},
		limiter:                  alwaysAllowLimiter(),
		defaultTPM:               100000,
		estimateCompletionTokens: 512,
	}
}

func testDeps(t *testing.T) chatDeps {
	t.Helper()
	return testDepsWithProvider(t, "mock-provider", mock.New("mock-provider", time.Millisecond, 0, 0))
}

// testIdentity is deliberately deterministic, not random: tests that issue
// multiple requests (e.g. a cache-hit-on-second-request test) need the
// same tenant across calls, which is also what actually happens for a
// real, repeated caller.
func testIdentity() auth.Identity {
	return auth.Identity{
		OrgID:       uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TeamID:      uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		KeyID:       uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		OrgTPMLimit: 1_000_000, TeamTPMLimit: 1_000_000,
	}
}

type fakeExactCache struct {
	data               map[string]*providers.CanonicalResponse
	getCalls, setCalls int
	lastSetTTLOverride *time.Duration
}

func newFakeExactCache() *fakeExactCache {
	return &fakeExactCache{data: make(map[string]*providers.CanonicalResponse)}
}

func (f *fakeExactCache) Get(_ context.Context, key string) (*providers.CanonicalResponse, bool, error) {
	f.getCalls++
	resp, ok := f.data[key]
	return resp, ok, nil
}

func (f *fakeExactCache) Set(_ context.Context, key string, resp *providers.CanonicalResponse, ttlOverride *time.Duration) error {
	f.setCalls++
	f.data[key] = resp
	f.lastSetTTLOverride = ttlOverride
	return nil
}

// fakeEmbedder returns a fixed vector for any text: real similarity
// behavior is covered by internal/embedding's tests against the actual
// ONNX model, so this only needs to prove the handler wires embed ->
// semantic-cache-lookup correctly.
type fakeEmbedder struct {
	vector    []float32
	err       error
	embedCall int
}

func (f *fakeEmbedder) Embed(_ string) ([]float32, error) {
	f.embedCall++
	return f.vector, f.err
}

type fakeSemanticCache struct {
	hitResponse        *providers.CanonicalResponse
	hit                bool
	getCalls           int
	setCalls           int
	lastSetResp        *providers.CanonicalResponse
	lastSetTTLOverride *time.Duration
}

func (f *fakeSemanticCache) Get(_ semantic.Query) (*providers.CanonicalResponse, float32, bool) {
	f.getCalls++
	if !f.hit {
		return nil, 0, false
	}
	return f.hitResponse, 0.97, true
}

func (f *fakeSemanticCache) Set(_ semantic.Query, resp *providers.CanonicalResponse, ttlOverride *time.Duration) {
	f.setCalls++
	f.lastSetResp = resp
	f.lastSetTTLOverride = ttlOverride
}

type fakeLedger struct {
	entries []ledger.Entry
}

func (f *fakeLedger) Record(e ledger.Entry) {
	f.entries = append(f.entries, e)
}

func doChatRequest(t *testing.T, deps chatDeps, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
	ctx := context.WithValue(req.Context(), identityCtxKey{}, testIdentity())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	withRequestID(handleChatCompletions(deps)).ServeHTTP(rec, req)
	return rec
}

func TestHandleChatCompletions_Success(t *testing.T) {
	deps := testDeps(t)
	rec := doChatRequest(t, deps, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Provider"); got != "mock-provider" {
		t.Errorf("X-Gateway-Provider = %q, want %q", got, "mock-provider")
	}
	if got := rec.Header().Get("X-Gateway-Attempts"); got != "mock-provider:200" {
		t.Errorf("X-Gateway-Attempts = %q, want %q", got, "mock-provider:200")
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("X-Request-Id header is empty")
	}

	var resp providers.CanonicalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Model != "fast" {
		t.Errorf("resp.Model = %q, want the client alias %q, not the provider's model string", resp.Model, "fast")
	}

	limiter := deps.limiter.(*fakeRateLimiter)
	if limiter.reserveCalls != 1 {
		t.Errorf("Reserve called %d times, want 1", limiter.reserveCalls)
	}
	if limiter.adjustCalls != 1 {
		t.Errorf("Adjust called %d times, want 1 (reconciliation after a successful response)", limiter.adjustCalls)
	}
}

func TestHandleChatCompletions_RateLimitRejected(t *testing.T) {
	deps := testDeps(t)
	limiter := &fakeRateLimiter{allow: false, limitingScope: "key", retryAfter: 7 * time.Second}
	deps.limiter = limiter

	rejectionsBefore := testutil.ToFloat64(metrics.RateLimitRejectionsTotal.WithLabelValues("key"))

	rec := doChatRequest(t, deps, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want %q", got, "7")
	}
	if got := rec.Header().Get("X-RateLimit-Scope"); got != "key" {
		t.Errorf("X-RateLimit-Scope = %q, want %q", got, "key")
	}
	if limiter.adjustCalls != 0 {
		t.Errorf("Adjust called %d times, want 0 (nothing was reserved)", limiter.adjustCalls)
	}

	if got := testutil.ToFloat64(metrics.RateLimitRejectionsTotal.WithLabelValues("key")) - rejectionsBefore; got != 1 {
		t.Errorf("gateway_ratelimit_rejections_total{scope=key} increased by %v, want 1", got)
	}
}

func TestHandleChatCompletions_CacheHit_IncrementsCacheMetrics(t *testing.T) {
	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)
	deps.cache = newFakeExactCache()

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	doChatRequest(t, deps, body) // primes the cache

	hitsBefore := testutil.ToFloat64(metrics.CacheHitsTotal.WithLabelValues("exact"))
	savedBefore := testutil.ToFloat64(metrics.TokensSavedTotal)

	doChatRequest(t, deps, body)

	if got := testutil.ToFloat64(metrics.CacheHitsTotal.WithLabelValues("exact")) - hitsBefore; got != 1 {
		t.Errorf("gateway_cache_hits_total{tier=exact} increased by %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.TokensSavedTotal) - savedBefore; got <= 0 {
		t.Errorf("gateway_tokens_saved_total increased by %v, want > 0", got)
	}
}

func TestHandleChatCompletions_ValidationErrors(t *testing.T) {
	deps := testDeps(t)
	tests := []struct {
		name string
		body string
		want int
	}{
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest},
		{"empty messages", `{"model":"fast","messages":[]}`, http.StatusBadRequest},
		{"unknown model", `{"model":"nope","messages":[{"role":"user","content":"hi"}]}`, http.StatusBadRequest},
		{"malformed json", `{not json`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doChatRequest(t, deps, tt.body)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}

func TestHandleChatCompletions_ProviderErrorPropagatesStatus(t *testing.T) {
	// Terminal-classified (422) so the engine gives up after one call
	// instead of retrying, and that status is what the client should see.
	deps := testDepsWithProvider(t, "flaky", mock.New("flaky", time.Millisecond, 1, 422))

	rec := doChatRequest(t, deps, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Attempts"); got != "flaky:422" {
		t.Errorf("X-Gateway-Attempts = %q, want %q", got, "flaky:422")
	}

	limiter := deps.limiter.(*fakeRateLimiter)
	if limiter.adjustCalls != 1 {
		t.Errorf("Adjust called %d times, want 1 (a failed request must refund its reservation)", limiter.adjustCalls)
	}
}

func TestHandleChatCompletions_Success_RecordsLedgerEntryWithCost(t *testing.T) {
	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	p.SetPricing(2, 4) // $2/$4 per 1M tokens in/out, so cost is nonzero and checkable
	deps := testDepsWithProvider(t, "mock-provider", p)
	fl := &fakeLedger{}
	deps.ledger = fl

	rec := doChatRequest(t, deps, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp providers.CanonicalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(fl.entries) != 1 {
		t.Fatalf("ledger entries recorded = %d, want 1", len(fl.entries))
	}
	e := fl.entries[0]
	if e.Provider != "mock-provider" {
		t.Errorf("Provider = %q, want %q", e.Provider, "mock-provider")
	}
	if e.Model != "mock-model-v1" {
		t.Errorf("Model = %q, want the vendor model string %q, not the alias", e.Model, "mock-model-v1")
	}
	if e.PromptTokens != resp.Usage.PromptTokens || e.CompletionTokens != resp.Usage.CompletionTokens {
		t.Errorf("token counts = (%d, %d), want (%d, %d) matching the response", e.PromptTokens, e.CompletionTokens, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
	wantCost := float64(resp.Usage.PromptTokens)/1e6*2 + float64(resp.Usage.CompletionTokens)/1e6*4
	if e.CostUSD != wantCost {
		t.Errorf("CostUSD = %v, want %v", e.CostUSD, wantCost)
	}
	if e.CacheTier != "none" {
		t.Errorf("CacheTier = %q, want %q", e.CacheTier, "none")
	}
	if e.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200", e.StatusCode)
	}
	if e.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", e.Attempts)
	}
	if e.OrgID != testIdentity().OrgID {
		t.Errorf("OrgID = %v, want %v", e.OrgID, testIdentity().OrgID)
	}
}

func TestHandleChatCompletions_CacheHit_RecordsLedgerEntryWithTokensSavedAndZeroCost(t *testing.T) {
	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	p.SetPricing(2, 4)
	deps := testDepsWithProvider(t, "mock-provider", p)
	deps.cache = newFakeExactCache()
	fl := &fakeLedger{}
	deps.ledger = fl

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	doChatRequest(t, deps, body) // primes the cache; ledger entry #1

	second := doChatRequest(t, deps, body)
	if second.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200; body = %s", second.Code, second.Body.String())
	}

	if len(fl.entries) != 2 {
		t.Fatalf("ledger entries recorded = %d, want 2 (one per request)", len(fl.entries))
	}
	hit := fl.entries[1]
	if hit.CacheTier != "exact" {
		t.Errorf("CacheTier = %q, want %q", hit.CacheTier, "exact")
	}
	if hit.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0 on a cache hit", hit.CostUSD)
	}
	if hit.TokensSaved == 0 {
		t.Error("TokensSaved = 0, want the cached response's token count")
	}
	if hit.PromptTokens != 0 || hit.CompletionTokens != 0 {
		t.Errorf("PromptTokens/CompletionTokens = %d/%d, want 0/0 on a cache hit (no provider call happened)", hit.PromptTokens, hit.CompletionTokens)
	}
}

func TestHandleChatCompletions_EngineFailure_DoesNotRecordLedgerEntry(t *testing.T) {
	deps := testDepsWithProvider(t, "flaky", mock.New("flaky", time.Millisecond, 1, 422))
	fl := &fakeLedger{}
	deps.ledger = fl

	doChatRequest(t, deps, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)

	if len(fl.entries) != 0 {
		t.Errorf("ledger entries recorded = %d, want 0 (a total failure has no usage to attribute)", len(fl.entries))
	}
}

func TestHandleChatCompletions_CacheMissThenHit(t *testing.T) {
	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)
	deps.cache = newFakeExactCache()

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"hi"}]}`

	first := doChatRequest(t, deps, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200; body = %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get("X-Gateway-Cache"); got != "none" {
		t.Errorf("first request X-Gateway-Cache = %q, want %q", got, "none")
	}
	if p.CallCount() != 1 {
		t.Fatalf("CallCount() after first request = %d, want 1", p.CallCount())
	}

	limiter := deps.limiter.(*fakeRateLimiter)
	reservesBefore := limiter.reserveCalls

	second := doChatRequest(t, deps, body)
	if second.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200; body = %s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("X-Gateway-Cache"); got != "exact" {
		t.Errorf("second request X-Gateway-Cache = %q, want %q", got, "exact")
	}
	if p.CallCount() != 1 {
		t.Errorf("CallCount() after second (cached) request = %d, want still 1", p.CallCount())
	}
	if limiter.reserveCalls != reservesBefore+1 {
		t.Errorf("reserveCalls = %d, want a reservation attempt even on a cache hit", limiter.reserveCalls)
	}
	if limiter.lastDelta != 10+512 { // fakeTokenCounter{n:10} + estimateCompletionTokens 512, fully refunded
		t.Errorf("last Adjust delta = %d, want the full reservation refunded (%d)", limiter.lastDelta, 10+512)
	}
}

func TestHandleChatCompletions_NonzeroTemperatureNeverCached(t *testing.T) {
	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)
	cache := newFakeExactCache()
	deps.cache = cache

	body := `{"model":"fast","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`

	doChatRequest(t, deps, body)
	doChatRequest(t, deps, body)

	if p.CallCount() != 2 {
		t.Errorf("CallCount() = %d, want 2 (nonzero temperature must not be served from cache by default)", p.CallCount())
	}
	if cache.getCalls != 0 || cache.setCalls != 0 {
		t.Errorf("cache Get/Set calls = %d/%d, want 0/0 (cache must not even be consulted)", cache.getCalls, cache.setCalls)
	}
}

func TestHandleChatCompletions_CacheTTLOverride_AppliedWhenWithinCeiling(t *testing.T) {
	deps := testDeps(t)
	cache := newFakeExactCache()
	deps.cache = cache
	deps.cacheMaxClientTTL = time.Hour

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"hi"}],"cache_ttl":"10m"}`
	rec := doChatRequest(t, deps, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Cache-TTL"); got != "10m0s" {
		t.Errorf("X-Gateway-Cache-TTL = %q, want %q", got, "10m0s")
	}
	if cache.lastSetTTLOverride == nil || *cache.lastSetTTLOverride != 10*time.Minute {
		t.Errorf("cache Set() ttlOverride = %v, want a pointer to 10m", cache.lastSetTTLOverride)
	}
}

func TestHandleChatCompletions_CacheTTLOverride_CappedByCeiling(t *testing.T) {
	deps := testDeps(t)
	cache := newFakeExactCache()
	deps.cache = cache
	deps.cacheMaxClientTTL = time.Hour

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"hi"}],"cache_ttl":"48h"}`
	rec := doChatRequest(t, deps, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Cache-TTL"); got != "1h0m0s" {
		t.Errorf("X-Gateway-Cache-TTL = %q, want the operator's ceiling %q, not the client's 48h request", got, "1h0m0s")
	}
	if cache.lastSetTTLOverride == nil || *cache.lastSetTTLOverride != time.Hour {
		t.Errorf("cache Set() ttlOverride = %v, want a pointer to the 1h ceiling", cache.lastSetTTLOverride)
	}
}

func TestHandleChatCompletions_CacheTTLOverride_ZeroSkipsCachingEntirely(t *testing.T) {
	deps := testDeps(t)
	cache := newFakeExactCache()
	deps.cache = cache
	deps.cacheMaxClientTTL = time.Hour

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"hi"}],"cache_ttl":"-1s"}`
	rec := doChatRequest(t, deps, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Cache-TTL"); got != "0" {
		t.Errorf("X-Gateway-Cache-TTL = %q, want %q", got, "0")
	}
	if cache.setCalls != 0 {
		t.Errorf("cache Set() called %d times, want 0 (a non-positive cache_ttl opts out of caching entirely)", cache.setCalls)
	}
}

func TestHandleChatCompletions_CacheTTLHint_IgnoredWhenFeatureDisabled(t *testing.T) {
	deps := testDeps(t) // cacheMaxClientTTL left at its zero value: feature disabled
	cache := newFakeExactCache()
	deps.cache = cache

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"hi"}],"cache_ttl":"24h"}`
	rec := doChatRequest(t, deps, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a client hint must never break a deployment that hasn't enabled it); body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Cache-TTL"); got != "" {
		t.Errorf("X-Gateway-Cache-TTL = %q, want no header when the feature is disabled", got)
	}
	if cache.setCalls != 1 {
		t.Fatalf("cache Set() called %d times, want 1", cache.setCalls)
	}
	if cache.lastSetTTLOverride != nil {
		t.Errorf("cache Set() ttlOverride = %v, want nil (ignored hint falls back to the store's own default)", cache.lastSetTTLOverride)
	}
}

func TestHandleChatCompletions_CacheTTLHint_MalformedIsBadRequestWhenEnabled(t *testing.T) {
	deps := testDeps(t)
	deps.cache = newFakeExactCache()
	deps.cacheMaxClientTTL = time.Hour

	body := `{"model":"fast","messages":[{"role":"user","content":"hi"}],"cache_ttl":"not-a-duration"}`
	rec := doChatRequest(t, deps, body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestHandleChatCompletions_SemanticCacheHit_ReleasesReservationAndSkipsProvider(t *testing.T) {
	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)
	deps.embedder = &fakeEmbedder{vector: []float32{1, 0, 0}}
	semCache := &fakeSemanticCache{hit: true, hitResponse: &providers.CanonicalResponse{ID: "cached-semantic"}}
	deps.semanticCache = semCache

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"How do I reset my password?"}]}`
	rec := doChatRequest(t, deps, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Cache"); got != "semantic" {
		t.Errorf("X-Gateway-Cache = %q, want %q", got, "semantic")
	}
	if p.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0 (a semantic hit must not call the provider)", p.CallCount())
	}

	limiter := deps.limiter.(*fakeRateLimiter)
	if limiter.adjustCalls != 1 {
		t.Errorf("Adjust called %d times, want 1 (a semantic hit releases the whole reservation)", limiter.adjustCalls)
	}
	if limiter.lastDelta != 10+512 {
		t.Errorf("last Adjust delta = %d, want the full reservation refunded (%d)", limiter.lastDelta, 10+512)
	}
}

func TestHandleChatCompletions_SemanticCacheMiss_StoresOnSuccess(t *testing.T) {
	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)
	deps.embedder = &fakeEmbedder{vector: []float32{1, 0, 0}}
	semCache := &fakeSemanticCache{hit: false}
	deps.semanticCache = semCache

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"How do I reset my password?"}]}`
	rec := doChatRequest(t, deps, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Cache"); got != "none" {
		t.Errorf("X-Gateway-Cache = %q, want %q", got, "none")
	}
	if p.CallCount() != 1 {
		t.Errorf("CallCount() = %d, want 1", p.CallCount())
	}
	if semCache.getCalls != 1 {
		t.Errorf("Get called %d times, want 1", semCache.getCalls)
	}
	if semCache.setCalls != 1 {
		t.Errorf("Set called %d times, want 1 (a miss that succeeds should populate the semantic cache)", semCache.setCalls)
	}
}

func TestHandleChatCompletions_SemanticCache_NotConsultedWhenEmbeddingFails(t *testing.T) {
	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)
	deps.embedder = &fakeEmbedder{err: fmt.Errorf("model not loaded")}
	semCache := &fakeSemanticCache{hit: true, hitResponse: &providers.CanonicalResponse{ID: "should-not-be-served"}}
	deps.semanticCache = semCache

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	rec := doChatRequest(t, deps, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Cache"); got != "none" {
		t.Errorf("X-Gateway-Cache = %q, want %q (embedding failed, so this must fall through to the provider)", got, "none")
	}
	if p.CallCount() != 1 {
		t.Errorf("CallCount() = %d, want 1", p.CallCount())
	}
	if semCache.getCalls != 0 {
		t.Errorf("Get called %d times, want 0 (no vector to search with)", semCache.getCalls)
	}
	if semCache.setCalls != 0 {
		t.Errorf("Set called %d times, want 0 (embedding failed for this request, don't cache under a bad/no vector)", semCache.setCalls)
	}
}

type sseEvent struct {
	event string
	data  string
}

func parseSSE(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(strings.TrimRight(body, "\n"), "\n\n") {
		if block == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
		events = append(events, ev)
	}
	return events
}

func TestHandleChatCompletions_Streaming_Success(t *testing.T) {
	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	p.SetPricing(2, 4)
	deps := testDepsWithProvider(t, "mock-provider", p)
	fl := &fakeLedger{}
	deps.ledger = fl

	rec := doChatRequest(t, deps, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want %q; body = %s", got, "text/event-stream", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-cache")
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want %q", got, "no")
	}
	if got := rec.Header().Get("X-Gateway-Provider"); got != "mock-provider" {
		t.Errorf("X-Gateway-Provider = %q, want %q", got, "mock-provider")
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) < 2 {
		t.Fatalf("got %d SSE events, want at least 2 (content + [DONE]); body = %s", len(events), rec.Body.String())
	}
	last := events[len(events)-1]
	if last.data != "[DONE]" {
		t.Errorf("final event data = %q, want %q", last.data, "[DONE]")
	}

	var assembled strings.Builder
	sawFinish := false
	for _, ev := range events[:len(events)-1] { // every event except the final [DONE]
		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(ev.data), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", ev.data, err)
		}
		if len(chunk.Choices) != 1 {
			t.Fatalf("chunk.Choices = %+v, want exactly 1", chunk.Choices)
		}
		assembled.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].FinishReason != nil {
			sawFinish = true
			if *chunk.Choices[0].FinishReason != "stop" {
				t.Errorf("FinishReason = %q, want %q", *chunk.Choices[0].FinishReason, "stop")
			}
		}
	}
	if !sawFinish {
		t.Error("no chunk carried a non-nil finish_reason before [DONE]")
	}
	if got := assembled.String(); got != "mock response to: hi" {
		t.Errorf("assembled content = %q, want %q", got, "mock response to: hi")
	}

	if p.CallCount() != 1 {
		t.Errorf("CallCount() = %d, want 1", p.CallCount())
	}
	limiter := deps.limiter.(*fakeRateLimiter)
	if limiter.adjustCalls != 1 {
		t.Errorf("Adjust called %d times, want 1", limiter.adjustCalls)
	}
	// mock's own final Delta.Usage (PromptTokens=len("hi")=2,
	// CompletionTokens=4 words) is what should have been reconciled,
	// not the CountText-based estimate, since the provider did send one.
	wantActual := int64(2 + 4)
	wantCost := int64(10) + 512 // fakeTokenCounter{n:10} + estimateCompletionTokens
	if limiter.lastDelta != wantCost-wantActual {
		t.Errorf("last Adjust delta = %d, want %d", limiter.lastDelta, wantCost-wantActual)
	}

	if len(fl.entries) != 1 {
		t.Fatalf("ledger entries recorded = %d, want 1", len(fl.entries))
	}
	e := fl.entries[0]
	if e.Provider != "mock-provider" || e.Model != "mock-model-v1" {
		t.Errorf("Provider/Model = %q/%q, want %q/%q", e.Provider, e.Model, "mock-provider", "mock-model-v1")
	}
	if e.PromptTokens != 2 || e.CompletionTokens != 4 {
		t.Errorf("token counts = (%d, %d), want (2, 4) from the provider's own final usage", e.PromptTokens, e.CompletionTokens)
	}
	wantCostUSD := float64(2)/1e6*2 + float64(4)/1e6*4
	if e.CostUSD != wantCostUSD {
		t.Errorf("CostUSD = %v, want %v", e.CostUSD, wantCostUSD)
	}
}

func TestHandleChatCompletions_Streaming_PreFlushFailureFallsOverAndStillStreams(t *testing.T) {
	primary := mock.New("primary", 0, 1, 429) // fails before sending any delta
	secondary := mock.New("secondary", 0, 0, 0)

	reg := breaker.NewRegistry(lenientBreakerConfig())
	provs := map[string]providers.Provider{"primary": primary, "secondary": secondary}
	timeouts := map[string]time.Duration{"primary": time.Second, "secondary": time.Second}
	engine := retry.New(reg, provs, timeouts, retry.Config{MaxAttemptsPerProvider: 2, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})

	rcfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "primary", Priority: 0, Weight: 1}, {Name: "secondary", Priority: 1, Weight: 1}},
		ModelAliases: map[string]map[string]string{
			"fast": {"primary": "mock-model-v1", "secondary": "mock-model-v1"},
		},
	}
	deps := chatDeps{
		router: router.New(rcfg), engine: engine,
		counter: fakeTokenCounter{n: 10}, limiter: alwaysAllowLimiter(),
		defaultTPM: 100000, estimateCompletionTokens: 512,
	}

	rec := doChatRequest(t, deps, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if got := rec.Header().Get("X-Gateway-Provider"); got != "secondary" {
		t.Fatalf("X-Gateway-Provider = %q, want %q; body = %s", got, "secondary", rec.Body.String())
	}
	events := parseSSE(t, rec.Body.String())
	if len(events) < 2 || events[len(events)-1].data != "[DONE]" {
		t.Fatalf("stream did not complete normally; body = %s", rec.Body.String())
	}
	if secondary.CallCount() != 1 {
		t.Errorf("secondary.CallCount() = %d, want 1", secondary.CallCount())
	}
}

func TestHandleChatCompletions_Streaming_MidStreamFailureTerminatesWithErrorEvent(t *testing.T) {
	primary := mock.New("primary", 0, 0, 0)
	primary.FailMidStream(2, 500)
	primary.SetPricing(2, 4)
	deps := testDepsWithProvider(t, "primary", primary)
	fl := &fakeLedger{}
	deps.ledger = fl

	rec := doChatRequest(t, deps, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want %q (headers must already have committed once content flushed); body = %s", got, "text/event-stream", rec.Body.String())
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) == 0 {
		t.Fatal("no SSE events at all")
	}
	last := events[len(events)-1]
	if last.event != "error" {
		t.Fatalf("final event = %q, want an %q event, not a silently truncated-looking-complete stream; body = %s", last.event, "error", rec.Body.String())
	}
	for _, ev := range events {
		if ev.data == "[DONE]" {
			t.Error("stream contains [DONE] despite a mid-stream failure -- must not look complete")
		}
	}

	if primary.CallCount() != 1 {
		t.Errorf("primary.CallCount() = %d, want 1 (no retry once content was flushed)", primary.CallCount())
	}

	limiter := deps.limiter.(*fakeRateLimiter)
	if limiter.adjustCalls != 1 {
		t.Fatalf("Adjust called %d times, want 1", limiter.adjustCalls)
	}
	// 2 deltas flushed ("mock ", "response "): CountText("mock ")=1 +
	// CountText("response ")=1 = 2 completion tokens actually generated.
	wantActual := int64(10) + 2 // fakeTokenCounter{n:10} prompt tokens + 2 completion tokens counted on the way past
	wantCost := int64(10) + 512
	if limiter.lastDelta != wantCost-wantActual {
		t.Errorf("last Adjust delta = %d, want %d (charge for partial generation, refund the rest of the estimate)", limiter.lastDelta, wantCost-wantActual)
	}

	if len(fl.entries) != 1 {
		t.Fatalf("ledger entries recorded = %d, want 1 (a partial stream still gets a row, charged for what was actually generated)", len(fl.entries))
	}
	e := fl.entries[0]
	if e.PromptTokens != 10 || e.CompletionTokens != 2 {
		t.Errorf("token counts = (%d, %d), want (10, 2)", e.PromptTokens, e.CompletionTokens)
	}
	wantCostUSD := float64(10)/1e6*2 + float64(2)/1e6*4
	if e.CostUSD != wantCostUSD {
		t.Errorf("CostUSD = %v, want %v", e.CostUSD, wantCostUSD)
	}
	if e.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want 200 (the wire-level status already committed)", e.StatusCode)
	}
}

func TestHandleChatCompletions_Streaming_TotalFailureBeforeFlush_ReturnsNormalErrorResponse(t *testing.T) {
	// Terminal-classified and fails before sending any delta: nothing is
	// ever flushed, so this must look exactly like a non-streaming error
	// -- plain JSON, correct status code -- not an SSE response.
	deps := testDepsWithProvider(t, "flaky", mock.New("flaky", time.Millisecond, 1, 422))

	rec := doChatRequest(t, deps, `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q (nothing was ever flushed)", got, "application/json")
	}
}

func TestHandleChatCompletions_Streaming_CacheHitServesSynthesizedSSE(t *testing.T) {
	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)
	deps.cache = newFakeExactCache()

	body := `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"hi"}]}`
	streamBody := `{"model":"fast","temperature":0,"stream":true,"messages":[{"role":"user","content":"hi"}]}`

	// First, a non-streaming request populates the cache (the key
	// excludes `stream`, so a later streaming request can hit it).
	first := doChatRequest(t, deps, body)
	if first.Code != http.StatusOK {
		t.Fatalf("priming request status = %d, want 200; body = %s", first.Code, first.Body.String())
	}
	if p.CallCount() != 1 {
		t.Fatalf("CallCount() after priming request = %d, want 1", p.CallCount())
	}

	rec := doChatRequest(t, deps, streamBody)
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want %q; body = %s", got, "text/event-stream", rec.Body.String())
	}
	if got := rec.Header().Get("X-Gateway-Cache"); got != "exact" {
		t.Errorf("X-Gateway-Cache = %q, want %q", got, "exact")
	}
	if p.CallCount() != 1 {
		t.Errorf("CallCount() after cached streaming request = %d, want still 1", p.CallCount())
	}

	events := parseSSE(t, rec.Body.String())
	if len(events) < 2 || events[len(events)-1].data != "[DONE]" {
		t.Fatalf("cached stream did not terminate with [DONE]; body = %s", rec.Body.String())
	}
}

func TestHandleChatCompletions_NoHealthyProviderReturns503(t *testing.T) {
	reg := breaker.NewRegistry(breaker.Config{
		ErrorRateThreshold: 0.5,
		MinRequests:        1,
		Window:             time.Second,
		Cooldown:           time.Hour,
		CooldownMax:        time.Hour,
		HalfOpenProbes:     1,
	})
	reg.Get("down", "mock-model-v1").RecordFailure() // trip it before any request arrives

	p := mock.New("down", time.Millisecond, 0, 0)
	cfg := &config.Config{
		Providers:    []config.ProviderConfig{{Name: "down", Priority: 0, Weight: 1}},
		ModelAliases: map[string]map[string]string{"fast": {"down": "mock-model-v1"}},
	}
	engine := retry.New(reg, map[string]providers.Provider{"down": p}, map[string]time.Duration{"down": time.Second}, retry.Config{
		MaxAttemptsPerProvider: 1, BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond,
	})
	deps := chatDeps{
		router: router.New(cfg), engine: engine,
		counter: fakeTokenCounter{n: 10}, limiter: alwaysAllowLimiter(),
		defaultTPM: 100000, estimateCompletionTokens: 512,
	}

	rec := doChatRequest(t, deps, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if p.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0 (breaker was already open)", p.CallCount())
	}
}
