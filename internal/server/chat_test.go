package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/breaker"
	"github.com/danger-baba/llm-inference-gateway/internal/config"
	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/providers/mock"
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
	return chatDeps{router: router.New(cfg), engine: engine}
}

func testDeps(t *testing.T) chatDeps {
	t.Helper()
	return testDepsWithProvider(t, "mock-provider", mock.New("mock-provider", time.Millisecond, 0, 0))
}

func doChatRequest(t *testing.T, deps chatDeps, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body))
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
	deps := chatDeps{router: router.New(cfg), engine: engine}

	rec := doChatRequest(t, deps, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if p.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0 (breaker was already open)", p.CallCount())
	}
}
