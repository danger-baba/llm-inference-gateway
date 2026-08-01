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

func lenientBreakerConfig() breaker.Config {
	// High enough MinRequests that a handful of test calls never trips it;
	// tests that want tripping do so explicitly via RecordFailure.
	return breaker.Config{
		ErrorRateThreshold: 0.5,
		MinRequests:        1000,
		Window:             time.Second,
		Cooldown:           time.Hour,
		CooldownMax:        time.Hour,
		HalfOpenProbes:     1,
	}
}

func fastRetryConfig() Config {
	return Config{
		MaxAttemptsPerProvider: 2,
		BaseBackoff:            time.Millisecond,
		MaxBackoff:             5 * time.Millisecond,
	}
}

func testReq() *providers.CanonicalRequest {
	return &providers.CanonicalRequest{
		Model:    "fast",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	}
}

func singleTier(name string, weight int) router.Tier {
	return router.Tier{Candidates: []router.Candidate{{ProviderName: name, Model: "m", Weight: weight}}}
}

func TestExecute_SuccessOnFirstTry(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	p := mock.New("only", 0, 0, 0)
	e := New(reg, map[string]providers.Provider{"only": p}, map[string]time.Duration{"only": time.Second}, fastRetryConfig())

	result, err := e.Execute(context.Background(), []router.Tier{singleTier("only", 1)}, testReq())
	if err != nil {
		t.Fatalf("Execute() unexpected error: %v", err)
	}
	if result.Provider != "only" {
		t.Errorf("Provider = %q, want %q", result.Provider, "only")
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Status != 200 {
		t.Errorf("Attempts = %+v, want a single 200", result.Attempts)
	}
}

func TestExecute_RetriesRetryableWithinSameProvider(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	// Fails every call at 500 (Retryable); with MaxAttemptsPerProvider=2 it
	// should be called exactly twice before giving up on this candidate.
	p := mock.New("flaky", 0, 1, 500)
	e := New(reg, map[string]providers.Provider{"flaky": p}, map[string]time.Duration{"flaky": time.Second}, fastRetryConfig())

	_, err := e.Execute(context.Background(), []router.Tier{singleTier("flaky", 1)}, testReq())
	if err == nil {
		t.Fatal("Execute() expected an error, got nil")
	}
	if p.CallCount() != 2 {
		t.Errorf("CallCount() = %d, want 2 (MaxAttemptsPerProvider)", p.CallCount())
	}
}

func TestExecute_ChaosFallback_FailingPrimaryStillYieldsSuccess(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	primary := mock.New("primary", 0, 1, 429) // always 429, Retryable
	secondary := mock.New("secondary", 0, 0, 0)
	provs := map[string]providers.Provider{"primary": primary, "secondary": secondary}
	timeouts := map[string]time.Duration{"primary": time.Second, "secondary": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	tiers := []router.Tier{singleTier("primary", 1), singleTier("secondary", 1)}
	result, err := e.Execute(context.Background(), tiers, testReq())
	if err != nil {
		t.Fatalf("Execute() unexpected error: %v", err)
	}
	if result.Provider != "secondary" {
		t.Errorf("Provider = %q, want %q (client should still get a 200 from the fallback)", result.Provider, "secondary")
	}
	if primary.CallCount() != 2 { // MaxAttemptsPerProvider
		t.Errorf("primary.CallCount() = %d, want 2", primary.CallCount())
	}
	if secondary.CallCount() != 1 {
		t.Errorf("secondary.CallCount() = %d, want 1", secondary.CallCount())
	}

	wantAttempts := []Attempt{
		{Provider: "primary", Status: 429},
		{Provider: "primary", Status: 429},
		{Provider: "secondary", Status: 200},
	}
	if len(result.Attempts) != len(wantAttempts) {
		t.Fatalf("Attempts = %+v, want %+v", result.Attempts, wantAttempts)
	}
	for i, a := range wantAttempts {
		if result.Attempts[i] != a {
			t.Errorf("Attempts[%d] = %+v, want %+v", i, result.Attempts[i], a)
		}
	}
}

func TestExecute_FallbackClassifiedSkipsRetryAdvancesTier(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	primary := mock.New("primary", 0, 1, 404) // Fallback-classified, must not retry
	secondary := mock.New("secondary", 0, 0, 0)
	provs := map[string]providers.Provider{"primary": primary, "secondary": secondary}
	timeouts := map[string]time.Duration{"primary": time.Second, "secondary": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	tiers := []router.Tier{singleTier("primary", 1), singleTier("secondary", 1)}
	result, err := e.Execute(context.Background(), tiers, testReq())
	if err != nil {
		t.Fatalf("Execute() unexpected error: %v", err)
	}
	if result.Provider != "secondary" {
		t.Errorf("Provider = %q, want %q", result.Provider, "secondary")
	}
	if primary.CallCount() != 1 {
		t.Errorf("primary.CallCount() = %d, want 1 (Fallback class must not retry)", primary.CallCount())
	}
}

func TestExecute_TerminalAbortsImmediately(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	primary := mock.New("primary", 0, 1, 422) // Terminal-classified
	secondary := mock.New("secondary", 0, 0, 0)
	provs := map[string]providers.Provider{"primary": primary, "secondary": secondary}
	timeouts := map[string]time.Duration{"primary": time.Second, "secondary": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	tiers := []router.Tier{singleTier("primary", 1), singleTier("secondary", 1)}
	_, err := e.Execute(context.Background(), tiers, testReq())
	if err == nil {
		t.Fatal("Execute() expected a Terminal error, got nil")
	}
	if primary.CallCount() != 1 {
		t.Errorf("primary.CallCount() = %d, want 1", primary.CallCount())
	}
	if secondary.CallCount() != 0 {
		t.Errorf("secondary.CallCount() = %d, want 0 (Terminal must not fall over to a healthy provider)", secondary.CallCount())
	}
}

func TestExecute_OpenBreakerShortCircuitsWithZeroNetworkCalls(t *testing.T) {
	reg := breaker.NewRegistry(breaker.Config{
		ErrorRateThreshold: 0.5,
		MinRequests:        1,
		Window:             time.Second,
		Cooldown:           time.Hour, // long enough it won't half-open mid-test
		CooldownMax:        time.Hour,
		HalfOpenProbes:     1,
	})
	// Trip the breaker for "flaky" before Execute ever runs.
	reg.Get("flaky", "m").RecordFailure()

	flaky := mock.New("flaky", 0, 0, 0) // would succeed if ever actually called
	healthy := mock.New("healthy", 0, 0, 0)
	provs := map[string]providers.Provider{"flaky": flaky, "healthy": healthy}
	timeouts := map[string]time.Duration{"flaky": time.Second, "healthy": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	tiers := []router.Tier{singleTier("flaky", 1), singleTier("healthy", 1)}
	result, err := e.Execute(context.Background(), tiers, testReq())
	if err != nil {
		t.Fatalf("Execute() unexpected error: %v", err)
	}
	if result.Provider != "healthy" {
		t.Errorf("Provider = %q, want %q", result.Provider, "healthy")
	}
	if flaky.CallCount() != 0 {
		t.Errorf("flaky.CallCount() = %d, want 0 (an OPEN breaker must short-circuit without a network call)", flaky.CallCount())
	}
	// The open breaker's skip must not appear as a recorded attempt either.
	for _, a := range result.Attempts {
		if a.Provider == "flaky" {
			t.Errorf("Attempts contains a record for the open-breaker provider: %+v", result.Attempts)
		}
	}
}

func TestExecute_AllProvidersOpen_ReturnsNoHealthyProvider(t *testing.T) {
	reg := breaker.NewRegistry(breaker.Config{
		ErrorRateThreshold: 0.5,
		MinRequests:        1,
		Window:             time.Second,
		Cooldown:           time.Hour,
		CooldownMax:        time.Hour,
		HalfOpenProbes:     1,
	})
	reg.Get("a", "m").RecordFailure()
	reg.Get("b", "m").RecordFailure()

	pa := mock.New("a", 0, 0, 0)
	pb := mock.New("b", 0, 0, 0)
	provs := map[string]providers.Provider{"a": pa, "b": pb}
	timeouts := map[string]time.Duration{"a": time.Second, "b": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	tiers := []router.Tier{singleTier("a", 1), singleTier("b", 1)}
	_, err := e.Execute(context.Background(), tiers, testReq())
	if err == nil {
		t.Fatal("Execute() expected an error, got nil")
	}
	if !errors.Is(err, ErrNoHealthyProvider) {
		t.Errorf("Execute() error = %v, want ErrNoHealthyProvider", err)
	}
	if pa.CallCount() != 0 || pb.CallCount() != 0 {
		t.Errorf("CallCount()s = %d, %d, want 0, 0", pa.CallCount(), pb.CallCount())
	}
}

func TestExecute_WeightZeroNeverSelectedButDoesNotBreakTheTier(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	drained := mock.New("drained", 0, 0, 0)
	active := mock.New("active", 0, 0, 0)
	provs := map[string]providers.Provider{"drained": drained, "active": active}
	timeouts := map[string]time.Duration{"drained": time.Second, "active": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	tier := router.Tier{Candidates: []router.Candidate{
		{ProviderName: "drained", Model: "m", Weight: 0},
		{ProviderName: "active", Model: "m", Weight: 5},
	}}

	for i := 0; i < 25; i++ {
		result, err := e.Execute(context.Background(), []router.Tier{tier}, testReq())
		if err != nil {
			t.Fatalf("Execute() unexpected error on iteration %d: %v", i, err)
		}
		if result.Provider != "active" {
			t.Fatalf("Provider = %q on iteration %d, want %q (weight 0 must never be picked)", result.Provider, i, "active")
		}
	}
	if drained.CallCount() != 0 {
		t.Errorf("drained.CallCount() = %d, want 0", drained.CallCount())
	}
}

func TestExecute_TierFullyDrainedAtWeightZero_AdvancesToNextTier(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	drained := mock.New("drained", 0, 0, 0)
	fallback := mock.New("fallback", 0, 0, 0)
	provs := map[string]providers.Provider{"drained": drained, "fallback": fallback}
	timeouts := map[string]time.Duration{"drained": time.Second, "fallback": time.Second}
	e := New(reg, provs, timeouts, fastRetryConfig())

	tiers := []router.Tier{
		{Candidates: []router.Candidate{{ProviderName: "drained", Model: "m", Weight: 0}}},
		singleTier("fallback", 1),
	}
	result, err := e.Execute(context.Background(), tiers, testReq())
	if err != nil {
		t.Fatalf("Execute() unexpected error: %v", err)
	}
	if result.Provider != "fallback" {
		t.Errorf("Provider = %q, want %q", result.Provider, "fallback")
	}
	if drained.CallCount() != 0 {
		t.Errorf("drained.CallCount() = %d, want 0 (an all-weight-0 tier must be skipped entirely)", drained.CallCount())
	}
}

func TestExecute_DeadlineExceededStopsRetryingWithoutOversleeping(t *testing.T) {
	reg := breaker.NewRegistry(lenientBreakerConfig())
	p := mock.New("flaky", 0, 1, 500)
	cfg := Config{MaxAttemptsPerProvider: 5, BaseBackoff: time.Hour, MaxBackoff: time.Hour} // deliberately huge backoff
	e := New(reg, map[string]providers.Provider{"flaky": p}, map[string]time.Duration{"flaky": time.Second}, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := e.Execute(ctx, []router.Tier{singleTier("flaky", 1)}, testReq())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Execute() expected an error, got nil")
	}
	if elapsed > time.Second {
		t.Errorf("Execute() took %v, want it to give up quickly once the deadline is exceeded, not sleep the full backoff", elapsed)
	}
	if p.CallCount() != 1 {
		t.Errorf("CallCount() = %d, want 1 (must not sleep past the deadline for a second attempt)", p.CallCount())
	}
}
