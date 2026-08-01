package mock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

func req(userContent string) *providers.CanonicalRequest {
	return &providers.CanonicalRequest{
		Model: "fast",
		Messages: []providers.Message{
			{Role: "system", Content: "be helpful"},
			{Role: "user", Content: userContent},
		},
	}
}

func TestComplete_Success(t *testing.T) {
	p := New("mock", time.Millisecond, 0, 0)

	resp, err := p.Complete(context.Background(), req("hello there"))
	if err != nil {
		t.Fatalf("Complete() unexpected error: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("Choices = %d, want 1", len(resp.Choices))
	}
	if got := resp.Choices[0].Message.Content; got != "mock response to: hello there" {
		t.Errorf("content = %q, want %q", got, "mock response to: hello there")
	}
	if p.CallCount() != 1 {
		t.Errorf("CallCount() = %d, want 1", p.CallCount())
	}
}

func TestComplete_InjectedFailure(t *testing.T) {
	p := New("mock", time.Millisecond, 1, 0) // always fails

	_, err := p.Complete(context.Background(), req("hi"))
	if err == nil {
		t.Fatal("Complete() expected an injected failure, got nil")
	}
	var apiErr *providers.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Complete() error = %v, want *providers.APIError", err)
	}
	if apiErr.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want 500", apiErr.StatusCode)
	}
}

func TestComplete_ContextCancelled(t *testing.T) {
	p := New("mock", time.Hour, 0, 0) // latency far longer than the test should wait

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := p.Complete(ctx, req("hi"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Complete() error = %v, want context.Canceled", err)
	}
}

func TestStream_Success(t *testing.T) {
	p := New("mock", time.Millisecond, 0, 0)
	out := make(chan providers.Delta, 32)

	err := p.Stream(context.Background(), req("hello there"), out)
	close(out)
	if err != nil {
		t.Fatalf("Stream() unexpected error: %v", err)
	}

	var content string
	var sawFinal bool
	for d := range out {
		content += d.Content
		if d.FinishReason != "" {
			sawFinal = true
			if d.Usage == nil {
				t.Error("final delta missing Usage")
			}
		}
	}
	if !sawFinal {
		t.Error("stream never sent a delta with FinishReason set")
	}
	if content != "mock response to: hello there" {
		t.Errorf("reassembled content = %q, want %q", content, "mock response to: hello there")
	}
}

func TestStream_InjectedFailure(t *testing.T) {
	p := New("mock", time.Millisecond, 1, 0)
	out := make(chan providers.Delta, 32)

	err := p.Stream(context.Background(), req("hi"), out)
	if err == nil {
		t.Fatal("Stream() expected an injected failure, got nil")
	}
}

func TestClassify(t *testing.T) {
	p := New("mock", time.Millisecond, 0, 0)

	tests := []struct {
		status int
		want   providers.FailureClass
	}{
		{429, providers.Retryable},
		{500, providers.Retryable},
		{503, providers.Retryable},
		{400, providers.Fallback},
		{404, providers.Fallback},
		{422, providers.Terminal},
	}
	for _, tt := range tests {
		if got := p.Classify(nil, tt.status); got != tt.want {
			t.Errorf("Classify(_, %d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestPricing_ZeroUntilPhase9(t *testing.T) {
	p := New("mock", time.Millisecond, 0, 0)
	in, out := p.Pricing("anything")
	if in != 0 || out != 0 {
		t.Errorf("Pricing() = (%v, %v), want (0, 0)", in, out)
	}
}

func TestComplete_ConfigurableFailureStatus(t *testing.T) {
	p := New("mock", time.Millisecond, 1, 429)

	_, err := p.Complete(context.Background(), req("hi"))
	var apiErr *providers.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Complete() error = %v, want *providers.APIError", err)
	}
	if apiErr.StatusCode != 429 {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
}

func TestComplete_ZeroFailureStatusDefaultsTo500(t *testing.T) {
	p := New("mock", time.Millisecond, 1, 0)

	_, err := p.Complete(context.Background(), req("hi"))
	var apiErr *providers.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Complete() error = %v, want *providers.APIError", err)
	}
	if apiErr.StatusCode != 500 {
		t.Errorf("StatusCode = %d, want default 500", apiErr.StatusCode)
	}
}

func TestHealthCheck(t *testing.T) {
	healthy := New("mock", time.Millisecond, 0, 0)
	if err := healthy.HealthCheck(context.Background()); err != nil {
		t.Errorf("HealthCheck() = %v, want nil for a healthy mock", err)
	}

	sick := New("mock", time.Millisecond, 1, 503)
	err := sick.HealthCheck(context.Background())
	var apiErr *providers.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 503 {
		t.Errorf("HealthCheck() = %v, want *providers.APIError{StatusCode: 503}", err)
	}
}
