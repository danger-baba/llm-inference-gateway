package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/config"
	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/providers/mock"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

func testDeps(t *testing.T) chatDeps {
	t.Helper()
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "mock-provider", Priority: 0}},
		ModelAliases: map[string]map[string]string{
			"fast": {"mock-provider": "mock-model-v1"},
		},
	}
	return chatDeps{
		router: router.New(cfg),
		providers: map[string]providers.Provider{
			"mock-provider": mock.New("mock-provider", time.Millisecond, 0),
		},
		providerTimeout: map[string]time.Duration{"mock-provider": 5 * time.Second},
	}
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
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "flaky", Priority: 0}},
		ModelAliases: map[string]map[string]string{
			"fast": {"flaky": "mock-model-v1"},
		},
	}
	deps := chatDeps{
		router: router.New(cfg),
		providers: map[string]providers.Provider{
			"flaky": mock.New("flaky", time.Millisecond, 1), // always injects a 500
		},
		providerTimeout: map[string]time.Duration{"flaky": 5 * time.Second},
	}

	rec := doChatRequest(t, deps, `{"model":"fast","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
}

func TestAttemptTimeout(t *testing.T) {
	t.Run("shorter request deadline wins", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		got := attemptTimeout(ctx, time.Hour)
		if got > 10*time.Millisecond || got <= 0 {
			t.Errorf("attemptTimeout() = %v, want <= 10ms and > 0", got)
		}
	})

	t.Run("configured timeout wins when shorter", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		got := attemptTimeout(ctx, 50*time.Millisecond)
		if got != 50*time.Millisecond {
			t.Errorf("attemptTimeout() = %v, want 50ms", got)
		}
	})

	t.Run("no deadline on context", func(t *testing.T) {
		got := attemptTimeout(context.Background(), 50*time.Millisecond)
		if got != 50*time.Millisecond {
			t.Errorf("attemptTimeout() = %v, want 50ms", got)
		}
	})

	t.Run("already past deadline clamps to zero, not negative", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()
		got := attemptTimeout(ctx, time.Hour)
		if got != 0 {
			t.Errorf("attemptTimeout() = %v, want 0", got)
		}
	})
}
