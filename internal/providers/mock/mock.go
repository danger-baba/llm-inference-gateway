// Package mock is a fake provider for exercising the proxy path (and, in
// later phases, caching and failover) without calling a real LLM API.
package mock

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

type Provider struct {
	name          string
	latency       time.Duration
	failureRate   float64
	failureStatus int
	rng           *rand.Rand
	calls         atomic.Int64

	// midStreamFailAfter/midStreamFailStatus configure Stream to fail
	// partway through instead of finishing normally, via FailMidStream.
	// Zero (the default) means "don't."
	midStreamFailAfter  int
	midStreamFailStatus int

	// streamDeltaLatency, set via StreamDeltaLatency, delays each
	// individual delta rather than just the call as a whole -- a real
	// upstream paces tokens over real time, and proving the gateway
	// forwards them incrementally rather than batching needs a fixture
	// that also arrives incrementally.
	streamDeltaLatency time.Duration

	// inPerMTok/outPerMTok default to 0,0 (no real vendor cost); set via
	// SetPricing.
	inPerMTok, outPerMTok float64
}

// New builds a mock provider that sleeps latency before every call and
// fails a failureRate fraction of them (0 = never, 1 = always) with
// failureStatus (0 defaults to 500).
func New(name string, latency time.Duration, failureRate float64, failureStatus int) *Provider {
	if failureStatus == 0 {
		failureStatus = 500
	}
	return &Provider{
		name:          name,
		latency:       latency,
		failureRate:   failureRate,
		failureStatus: failureStatus,
		rng:           rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (p *Provider) Name() string { return p.name }

// FailMidStream makes Stream send exactly afterDeltas word-deltas and
// then fail instead of finishing normally — the "provider dies mid-
// stream" scenario the README's Phase 8 gate requires a real test for.
func (p *Provider) FailMidStream(afterDeltas, status int) {
	p.midStreamFailAfter = afterDeltas
	p.midStreamFailStatus = status
}

// StreamDeltaLatency makes Stream sleep d before sending each word-delta,
// simulating a real provider's token-by-token pacing instead of handing
// back every delta in one tight loop.
func (p *Provider) StreamDeltaLatency(d time.Duration) {
	p.streamDeltaLatency = d
}

// CallCount lets tests (and, from Phase 6 on, the cache gate) assert
// whether the upstream was actually hit.
func (p *Provider) CallCount() int64 { return p.calls.Load() }

func (p *Provider) Complete(ctx context.Context, req *providers.CanonicalRequest) (*providers.CanonicalResponse, error) {
	p.calls.Add(1)

	select {
	case <-time.After(p.latency):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if p.shouldFail() {
		return nil, &providers.APIError{StatusCode: p.failureStatus, Message: "mock: injected failure"}
	}

	content := lastUserMessage(req.Messages)
	return &providers.CanonicalResponse{
		ID:      fmt.Sprintf("mock-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []providers.Choice{
			{
				Index:        0,
				Message:      providers.Message{Role: "assistant", Content: "mock response to: " + content},
				FinishReason: "stop",
			},
		},
		Usage: providers.Usage{
			PromptTokens:     len(content),
			CompletionTokens: 20,
			TotalTokens:      len(content) + 20,
		},
	}, nil
}

// Stream splits the same content Complete would return into a handful of
// word-sized deltas, so Phase 8's SSE writer has something realistic to
// forward before it exists.
func (p *Provider) Stream(ctx context.Context, req *providers.CanonicalRequest, out chan<- providers.Delta) error {
	p.calls.Add(1)

	select {
	case <-time.After(p.latency):
	case <-ctx.Done():
		return ctx.Err()
	}

	if p.shouldFail() {
		return &providers.APIError{StatusCode: p.failureStatus, Message: "mock: injected failure"}
	}

	content := lastUserMessage(req.Messages)
	words := strings.Fields("mock response to: " + content)

	for i, w := range words {
		if p.midStreamFailAfter > 0 && i == p.midStreamFailAfter {
			return &providers.APIError{StatusCode: p.midStreamFailStatus, Message: "mock: injected mid-stream failure"}
		}
		if p.streamDeltaLatency > 0 {
			select {
			case <-time.After(p.streamDeltaLatency):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		chunk := w
		if i < len(words)-1 {
			chunk += " "
		}
		select {
		case out <- providers.Delta{Content: chunk}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	final := providers.Delta{
		FinishReason: "stop",
		Usage: &providers.Usage{
			PromptTokens:     len(content),
			CompletionTokens: len(words),
			TotalTokens:      len(content) + len(words),
		},
	}
	select {
	case out <- final:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (p *Provider) Classify(err error, status int) providers.FailureClass {
	return providers.ClassifyByStatus(status)
}

// HealthCheck reuses the same failure injection as Complete/Stream, so
// tests can drive the background prober's recovery detection without a
// separate knob.
func (p *Provider) HealthCheck(ctx context.Context) error {
	select {
	case <-time.After(p.latency):
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.shouldFail() {
		return &providers.APIError{StatusCode: p.failureStatus, Message: "mock: injected health-check failure"}
	}
	return nil
}

// SetPricing lets tests exercise real cost computation against a mock
// provider without needing a real vendor price table.
func (p *Provider) SetPricing(inPerMTok, outPerMTok float64) {
	p.inPerMTok, p.outPerMTok = inPerMTok, outPerMTok
}

// Pricing defaults to 0,0 -- a mock provider has no real vendor cost --
// unless a test opts in via SetPricing.
func (p *Provider) Pricing(_ string) (float64, float64) { return p.inPerMTok, p.outPerMTok }

func (p *Provider) shouldFail() bool {
	if p.failureRate <= 0 {
		return false
	}
	if p.failureRate >= 1 {
		return true
	}
	return p.rng.Float64() < p.failureRate
}

func lastUserMessage(messages []providers.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}
