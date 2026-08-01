package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

func floatPtr(f float64) *float64 { return &f }
func intPtr(i int) *int           { return &i }

func TestTranslateRequest(t *testing.T) {
	req := &providers.CanonicalRequest{
		Model: "gpt-4o-mini",
		Messages: []providers.Message{
			{Role: "user", Content: "hi"},
		},
		Temperature: floatPtr(0.7),
		TopP:        floatPtr(0.9),
		MaxTokens:   intPtr(256),
		Stop:        []string{"\n"},
		Seed:        intPtr(42),
		User:        "tenant-1",
	}

	got := translateRequest(req)

	if got.Model != "gpt-4o-mini" {
		t.Errorf("Model = %q, want %q", got.Model, "gpt-4o-mini")
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "hi" {
		t.Errorf("Messages = %+v, want one message with content %q", got.Messages, "hi")
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 256 {
		t.Errorf("MaxTokens = %v, want 256", got.MaxTokens)
	}
	if got.Stream {
		t.Error("Stream = true, want false for translateRequest (Complete path)")
	}
}

func TestTranslateResponse(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-abc",
		"object": "chat.completion",
		"created": 1700000000,
		"model": "gpt-4o-mini",
		"choices": [
			{"index": 0, "message": {"role": "assistant", "content": "hello"}, "finish_reason": "stop"}
		],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	}`)

	resp, err := translateResponse(body)
	if err != nil {
		t.Fatalf("translateResponse() unexpected error: %v", err)
	}
	if resp.ID != "chatcmpl-abc" {
		t.Errorf("ID = %q, want %q", resp.ID, "chatcmpl-abc")
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello" {
		t.Errorf("Choices = %+v", resp.Choices)
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("Usage.TotalTokens = %d, want 15", resp.Usage.TotalTokens)
	}
}

func TestTranslateResponse_MalformedJSON(t *testing.T) {
	_, err := translateResponse([]byte("not json"))
	if err == nil {
		t.Fatal("translateResponse() expected error for malformed JSON, got nil")
	}
}

func TestComplete_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
		}
		var wireReq chatRequest
		if err := json.NewDecoder(r.Body).Decode(&wireReq); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if wireReq.Stream {
			t.Error("Stream = true on the Complete() path, want false")
		}
		fmt.Fprint(w, `{
			"id": "chatcmpl-1", "object": "chat.completion", "created": 1,
			"model": "gpt-4o-mini",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "hi back"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		}`)
	}))
	defer srv.Close()

	p := New("openai", srv.URL, "test-key")
	resp, err := p.Complete(context.Background(), &providers.CanonicalRequest{
		Model:    "gpt-4o-mini",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete() unexpected error: %v", err)
	}
	if resp.Choices[0].Message.Content != "hi back" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "hi back")
	}
}

func TestComplete_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error": {"message": "rate limited"}}`)
	}))
	defer srv.Close()

	p := New("openai", srv.URL, "test-key")
	_, err := p.Complete(context.Background(), &providers.CanonicalRequest{
		Model:    "gpt-4o-mini",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Complete() expected an error, got nil")
	}
	var apiErr *providers.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Complete() error = %v, want *providers.APIError", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusTooManyRequests)
	}
	if apiErr.Message != "rate limited" {
		t.Errorf("Message = %q, want %q", apiErr.Message, "rate limited")
	}
}

func TestComplete_ParsesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error": {"message": "rate limited"}}`)
	}))
	defer srv.Close()

	p := New("openai", srv.URL, "test-key")
	_, err := p.Complete(context.Background(), &providers.CanonicalRequest{
		Model:    "gpt-4o-mini",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	var apiErr *providers.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Complete() error = %v, want *providers.APIError", err)
	}
	if apiErr.RetryAfter != 17*time.Second {
		t.Errorf("RetryAfter = %v, want 17s", apiErr.RetryAfter)
	}
}

func TestHealthCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New("openai", srv.URL, "test-key")
	if err := p.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() = %v, want nil", err)
	}
}

func TestHealthCheck_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := New("openai", srv.URL, "test-key")
	err := p.HealthCheck(context.Background())
	var apiErr *providers.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("HealthCheck() = %v, want *providers.APIError{StatusCode: 503}", err)
	}
}

func TestStream_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wireReq chatRequest
		if err := json.NewDecoder(r.Body).Decode(&wireReq); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if !wireReq.Stream {
			t.Error("Stream = false on the Stream() path, want true")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"hel"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "%s\n\n", c)
		}
	}))
	defer srv.Close()

	p := New("openai", srv.URL, "test-key")
	out := make(chan providers.Delta, 8)
	err := p.Stream(context.Background(), &providers.CanonicalRequest{
		Model:    "gpt-4o-mini",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	}, out)
	close(out)
	if err != nil {
		t.Fatalf("Stream() unexpected error: %v", err)
	}

	var content string
	var finish string
	for d := range out {
		content += d.Content
		if d.FinishReason != "" {
			finish = d.FinishReason
		}
	}
	if content != "hello" {
		t.Errorf("reassembled content = %q, want %q", content, "hello")
	}
	if finish != "stop" {
		t.Errorf("finish reason = %q, want %q", finish, "stop")
	}
}

func TestClassify(t *testing.T) {
	p := New("openai", "http://example.invalid", "key")
	tests := []struct {
		status int
		want   providers.FailureClass
	}{
		{429, providers.Retryable},
		{500, providers.Retryable},
		{404, providers.Fallback},
		{401, providers.Fallback},
		{422, providers.Terminal},
	}
	for _, tt := range tests {
		if got := p.Classify(nil, tt.status); got != tt.want {
			t.Errorf("Classify(_, %d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestPricing(t *testing.T) {
	p := New("openai", "http://example.invalid", "key")

	in, out := p.Pricing("gpt-4o-mini")
	if in != 0.15 || out != 0.60 {
		t.Errorf("Pricing(%q) = (%v, %v), want (0.15, 0.60)", "gpt-4o-mini", in, out)
	}

	in, out = p.Pricing("some-model-not-in-the-table")
	if in != 0 || out != 0 {
		t.Errorf("Pricing() for an unrecognized model = (%v, %v), want (0, 0), not a guess", in, out)
	}
}
