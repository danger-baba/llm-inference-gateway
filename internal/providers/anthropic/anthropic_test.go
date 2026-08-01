package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

func TestTranslateRequest_ExtractsSystemMessage(t *testing.T) {
	req := &providers.CanonicalRequest{
		Model: "claude-haiku-4-5",
		Messages: []providers.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello"},
			{Role: "user", Content: "how are you"},
		},
	}

	got := translateRequest(req)

	if got.System != "be terse" {
		t.Errorf("System = %q, want %q", got.System, "be terse")
	}
	if len(got.Messages) != 3 {
		t.Fatalf("Messages = %d, want 3 (system message extracted)", len(got.Messages))
	}
	for _, m := range got.Messages {
		if m.Role == "system" {
			t.Errorf("system message leaked into Messages: %+v", m)
		}
	}
}

func TestTranslateRequest_DefaultMaxTokens(t *testing.T) {
	req := &providers.CanonicalRequest{
		Model:    "claude-haiku-4-5",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	}
	got := translateRequest(req)
	if got.MaxTokens != defaultMaxTokens {
		t.Errorf("MaxTokens = %d, want default %d", got.MaxTokens, defaultMaxTokens)
	}
}

func TestTranslateRequest_ExplicitMaxTokens(t *testing.T) {
	want := 256
	req := &providers.CanonicalRequest{
		Model:     "claude-haiku-4-5",
		Messages:  []providers.Message{{Role: "user", Content: "hi"}},
		MaxTokens: &want,
	}
	got := translateRequest(req)
	if got.MaxTokens != want {
		t.Errorf("MaxTokens = %d, want %d", got.MaxTokens, want)
	}
}

func TestTranslateResponse(t *testing.T) {
	body := []byte(`{
		"id": "msg_123",
		"model": "claude-haiku-4-5",
		"content": [{"type": "text", "text": "hello there"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 10, "output_tokens": 4}
	}`)

	resp, err := translateResponse(body)
	if err != nil {
		t.Fatalf("translateResponse() unexpected error: %v", err)
	}
	if resp.Choices[0].Message.Content != "hello there" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "hello there")
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", resp.Choices[0].FinishReason, "stop")
	}
	if resp.Usage.TotalTokens != 14 {
		t.Errorf("TotalTokens = %d, want 14", resp.Usage.TotalTokens)
	}
}

func TestMapStopReason(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"end_turn", "stop"},
		{"stop_sequence", "stop"},
		{"max_tokens", "length"},
		{"", ""},
		{"tool_use", "tool_use"},
	}
	for _, tt := range tests {
		if got := mapStopReason(tt.in); got != tt.want {
			t.Errorf("mapStopReason(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestComplete_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Errorf("path = %q, want /messages", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q, want %q", got, "test-key")
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
			t.Errorf("anthropic-version = %q, want %q", got, anthropicVersion)
		}
		var wireReq wireRequest
		if err := json.NewDecoder(r.Body).Decode(&wireReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if wireReq.System != "be terse" {
			t.Errorf("System = %q, want %q", wireReq.System, "be terse")
		}
		fmt.Fprint(w, `{
			"id": "msg_1", "model": "claude-haiku-4-5",
			"content": [{"type": "text", "text": "ok"}],
			"stop_reason": "end_turn",
			"usage": {"input_tokens": 5, "output_tokens": 1}
		}`)
	}))
	defer srv.Close()

	p := New("anthropic", srv.URL, "test-key")
	resp, err := p.Complete(context.Background(), &providers.CanonicalRequest{
		Model: "claude-haiku-4-5",
		Messages: []providers.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("Complete() unexpected error: %v", err)
	}
	if resp.Choices[0].Message.Content != "ok" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "ok")
	}
}

func TestComplete_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"type": "error", "error": {"type": "authentication_error", "message": "invalid api key"}}`)
	}))
	defer srv.Close()

	p := New("anthropic", srv.URL, "bad-key")
	_, err := p.Complete(context.Background(), &providers.CanonicalRequest{
		Model:    "claude-haiku-4-5",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	var apiErr *providers.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Complete() error = %v, want *providers.APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusUnauthorized)
	}
	if apiErr.Message != "invalid api key" {
		t.Errorf("Message = %q, want %q", apiErr.Message, "invalid api key")
	}
}

func TestStream_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}`,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`data: {"type":"message_stop"}`,
		}
		for _, e := range events {
			fmt.Fprintf(w, "%s\n\n", e)
		}
	}))
	defer srv.Close()

	p := New("anthropic", srv.URL, "test-key")
	out := make(chan providers.Delta, 8)
	err := p.Stream(context.Background(), &providers.CanonicalRequest{
		Model:    "claude-haiku-4-5",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	}, out)
	close(out)
	if err != nil {
		t.Fatalf("Stream() unexpected error: %v", err)
	}

	var content, finish string
	var usage *providers.Usage
	for d := range out {
		content += d.Content
		if d.FinishReason != "" {
			finish = d.FinishReason
		}
		if d.Usage != nil {
			usage = d.Usage
		}
	}
	if content != "hello" {
		t.Errorf("content = %q, want %q", content, "hello")
	}
	if finish != "stop" {
		t.Errorf("finish = %q, want %q", finish, "stop")
	}
	if usage == nil || usage.PromptTokens != 7 || usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v, want prompt=7 completion=2", usage)
	}
}

func TestClassify(t *testing.T) {
	p := New("anthropic", "http://example.invalid", "key")
	tests := []struct {
		status int
		want   providers.FailureClass
	}{
		{429, providers.Retryable},
		{529, providers.Retryable},
		{404, providers.Fallback},
		{401, providers.Fallback},
	}
	for _, tt := range tests {
		if got := p.Classify(nil, tt.status); got != tt.want {
			t.Errorf("Classify(_, %d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}
