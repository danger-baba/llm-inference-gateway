package retry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/breaker"
	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/providers/mock"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

// collectingOnDelta returns an onDelta callback that records every delta
// it's given, plus which provider names it was called for.
func collectingOnDelta() (func(string, string, providers.Delta) error, *[]providers.Delta, *[]string) {
	var deltas []providers.Delta
	var names []string
	return func(name, model string, d providers.Delta) error {
		names = append(names, name)
		deltas = append(deltas, d)
		return nil
	}, &deltas, &names
}

func TestExecuteStream_SuccessRelaysEveryDelta(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	p := mock.New("only", 0, 0, 0)
	e := New(reg, map[string]providers.Provider{"only": p}, map[string]time.Duration{"only": time.Second}, fastRetryConfig())

	onDelta, deltas, names := collectingOnDelta()
	result, err := e.ExecuteStream(context.Background(), []router.Tier{singleTier("only", 1)}, testReq(), onDelta)
	if err != nil {
		t.Fatalf("ExecuteStream() unexpected error: %v", err)
	}
	if result.Provider != "only" {
		t.Errorf("Provider = %q, want %q", result.Provider, "only")
	}
	if len(*deltas) == 0 {
		t.Fatal("onDelta was never called")
	}
	for _, n := range *names {
		if n != "only" {
			t.Errorf("onDelta called with provider name %q, want %q", n, "only")
		}
	}
	last := (*deltas)[len(*deltas)-1]
	if last.FinishReason != "stop" {
		t.Errorf("final delta FinishReason = %q, want %q", last.FinishReason, "stop")
	}
	if last.Usage == nil {
		t.Fatal("final delta Usage = nil, want the mock provider's usage")
	}
	if result.Usage == nil || result.Usage.CompletionTokens != last.Usage.CompletionTokens {
		t.Errorf("StreamResult.Usage = %+v, want it to carry the provider's final Usage", result.Usage)
	}
}

func TestExecuteStream_PreFlushFailureFallsOverToSecondary(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	// Fails before sending any delta at all (shouldFail() is checked
	// before the word loop), so this is squarely a pre-flush failure.
	primary := mock.New("primary", 0, 1, 429)
	secondary := mock.New("secondary", 0, 0, 0)
	provs := map[string]providers.Provider{"primary": primary, "secondary": secondary}
	timeouts := map[string]time.Duration{"primary": time.Second, "secondary": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	onDelta, deltas, names := collectingOnDelta()
	tiers := []router.Tier{singleTier("primary", 1), singleTier("secondary", 1)}
	result, err := e.ExecuteStream(context.Background(), tiers, testReq(), onDelta)
	if err != nil {
		t.Fatalf("ExecuteStream() unexpected error: %v", err)
	}
	if result.Provider != "secondary" {
		t.Errorf("Provider = %q, want %q (should fail over before anything is flushed)", result.Provider, "secondary")
	}
	if len(*deltas) == 0 {
		t.Fatal("onDelta was never called for the fallback provider")
	}
	for _, n := range *names {
		if n != "secondary" {
			t.Errorf("onDelta called with provider name %q, want only %q (primary sent nothing)", n, "secondary")
		}
	}
}

func TestExecuteStream_MidStreamFailure_DoesNotFailOverAndReportsFlushed(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	primary := mock.New("primary", 0, 0, 0)
	primary.FailMidStream(2, 500) // sends 2 word-deltas, then dies
	secondary := mock.New("secondary", 0, 0, 0)
	provs := map[string]providers.Provider{"primary": primary, "secondary": secondary}
	timeouts := map[string]time.Duration{"primary": time.Second, "secondary": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	onDelta, deltas, names := collectingOnDelta()
	tiers := []router.Tier{singleTier("primary", 1), singleTier("secondary", 1)}
	_, err := e.ExecuteStream(context.Background(), tiers, testReq(), onDelta)
	if err == nil {
		t.Fatal("ExecuteStream() expected an error after a mid-stream death, got nil")
	}

	var streamErr *StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error = %T, want *StreamError", err)
	}
	if !streamErr.Flushed {
		t.Error("StreamError.Flushed = false, want true (2 deltas were already forwarded)")
	}
	if len(*deltas) != 2 {
		t.Errorf("onDelta was called %d times, want exactly 2 (the two deltas sent before death)", len(*deltas))
	}
	if secondary.CallCount() != 0 {
		t.Errorf("secondary.CallCount() = %d, want 0 -- must not splice a second provider's output into an already-flushed stream", secondary.CallCount())
	}
	for _, n := range *names {
		if n != "primary" {
			t.Errorf("onDelta called with provider name %q, want only %q", n, "primary")
		}
	}
	// Content actually reached the "client" (onDelta), so a retry of the
	// same candidate must not happen either.
	if primary.CallCount() != 1 {
		t.Errorf("primary.CallCount() = %d, want 1 (no retry once flushed, even against the same provider)", primary.CallCount())
	}
}

func TestExecuteStream_TerminalPreFlushAbortsImmediately(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	primary := mock.New("primary", 0, 1, 422) // Terminal-classified, fails before any delta
	secondary := mock.New("secondary", 0, 0, 0)
	provs := map[string]providers.Provider{"primary": primary, "secondary": secondary}
	timeouts := map[string]time.Duration{"primary": time.Second, "secondary": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	onDelta, _, _ := collectingOnDelta()
	tiers := []router.Tier{singleTier("primary", 1), singleTier("secondary", 1)}
	_, err := e.ExecuteStream(context.Background(), tiers, testReq(), onDelta)
	if err == nil {
		t.Fatal("ExecuteStream() expected a Terminal error, got nil")
	}
	if secondary.CallCount() != 0 {
		t.Errorf("secondary.CallCount() = %d, want 0 (Terminal must not fall over)", secondary.CallCount())
	}
}

func TestExecuteStream_OpenBreakerShortCircuitsWithZeroNetworkCalls(t *testing.T) {
	reg := breaker.NewRegistry(breaker.Config{
		ErrorRateThreshold: 0.5,
		MinRequests:        1,
		Window:             time.Second,
		Cooldown:           time.Hour,
		CooldownMax:        time.Hour,
		HalfOpenProbes:     1,
	})
	reg.Get("flaky", "m").RecordFailure()

	flaky := mock.New("flaky", 0, 0, 0)
	healthy := mock.New("healthy", 0, 0, 0)
	provs := map[string]providers.Provider{"flaky": flaky, "healthy": healthy}
	timeouts := map[string]time.Duration{"flaky": time.Second, "healthy": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	onDelta, _, _ := collectingOnDelta()
	tiers := []router.Tier{singleTier("flaky", 1), singleTier("healthy", 1)}
	result, err := e.ExecuteStream(context.Background(), tiers, testReq(), onDelta)
	if err != nil {
		t.Fatalf("ExecuteStream() unexpected error: %v", err)
	}
	if result.Provider != "healthy" {
		t.Errorf("Provider = %q, want %q", result.Provider, "healthy")
	}
	if flaky.CallCount() != 0 {
		t.Errorf("flaky.CallCount() = %d, want 0", flaky.CallCount())
	}
}

func TestExecuteStream_OnDeltaErrorStopsRelayAndIsReturned(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	p := mock.New("only", 0, 0, 0)
	e := New(reg, map[string]providers.Provider{"only": p}, map[string]time.Duration{"only": time.Second}, fastRetryConfig())

	boom := errors.New("write: broken pipe")
	calls := 0
	onDelta := func(string, string, providers.Delta) error {
		calls++
		if calls == 1 {
			return boom
		}
		return nil
	}

	_, err := e.ExecuteStream(context.Background(), []router.Tier{singleTier("only", 1)}, testReq(), onDelta)
	if err == nil {
		t.Fatal("ExecuteStream() expected an error, got nil")
	}
	var streamErr *StreamError
	if !errors.As(err, &streamErr) || !errors.Is(streamErr.Err, boom) {
		t.Errorf("error = %v, want it to wrap the onDelta error %v", err, boom)
	}
	if calls != 1 {
		t.Errorf("onDelta called %d times, want exactly 1 (relay must stop on the first error)", calls)
	}
}
