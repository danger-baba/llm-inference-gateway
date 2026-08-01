package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/danger-baba/llm-inference-gateway/internal/breaker"
	"github.com/danger-baba/llm-inference-gateway/internal/config"
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
}

func newFakeExactCache() *fakeExactCache {
	return &fakeExactCache{data: make(map[string]*providers.CanonicalResponse)}
}

func (f *fakeExactCache) Get(_ context.Context, key string) (*providers.CanonicalResponse, bool, error) {
	f.getCalls++
	resp, ok := f.data[key]
	return resp, ok, nil
}

func (f *fakeExactCache) Set(_ context.Context, key string, resp *providers.CanonicalResponse) error {
	f.setCalls++
	f.data[key] = resp
	return nil
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
		{"streaming not yet supported", `{"model":"fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`, http.StatusNotImplemented},
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
