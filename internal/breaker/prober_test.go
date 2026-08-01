package breaker

import (
	"context"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/providers/mock"
)

// fakeNoHealthCheck implements providers.Provider but deliberately not
// providers.HealthChecker, so the prober's type assertion has something
// real to skip over.
type fakeNoHealthCheck struct{}

func (fakeNoHealthCheck) Name() string { return "no-health-check" }
func (fakeNoHealthCheck) Complete(context.Context, *providers.CanonicalRequest) (*providers.CanonicalResponse, error) {
	return nil, nil
}
func (fakeNoHealthCheck) Stream(context.Context, *providers.CanonicalRequest, chan<- providers.Delta) error {
	return nil
}
func (fakeNoHealthCheck) Classify(error, int) providers.FailureClass { return providers.Terminal }
func (fakeNoHealthCheck) Pricing(string) (float64, float64)          { return 0, 0 }

func proberTestBreakerConfig() Config {
	return Config{
		ErrorRateThreshold: 0.5,
		MinRequests:        1,
		Window:             time.Second,
		Cooldown:           20 * time.Millisecond,
		CooldownMax:        200 * time.Millisecond,
		HalfOpenProbes:     1,
	}
}

func TestProber_ProbesReadyOpenBreakerAndCloses(t *testing.T) {
	reg := NewRegistry(proberTestBreakerConfig())
	b := reg.Get("healthy", "m")
	b.RecordFailure() // trips it Open

	healthyProvider := mock.New("healthy", 0, 0, 0)
	prober := NewProber(reg, map[string]providers.Provider{"healthy": healthyProvider}, time.Hour, time.Second)

	time.Sleep(proberTestBreakerConfig().Cooldown + 10*time.Millisecond)
	prober.probeOnce(context.Background())

	if b.State() != Closed {
		t.Errorf("State() = %v, want Closed after a successful probe (HalfOpenProbes=1)", b.State())
	}
}

func TestProber_DoesNotProbeDuringCooldown(t *testing.T) {
	reg := NewRegistry(proberTestBreakerConfig())
	b := reg.Get("healthy", "m")
	b.RecordFailure()

	healthyProvider := mock.New("healthy", 0, 0, 0)
	prober := NewProber(reg, map[string]providers.Provider{"healthy": healthyProvider}, time.Hour, time.Second)

	prober.probeOnce(context.Background()) // cooldown has not elapsed yet

	if b.State() != Open {
		t.Errorf("State() = %v, want Open (cooldown not yet elapsed)", b.State())
	}
	if healthyProvider.CallCount() != 0 {
		t.Errorf("CallCount() = %d, want 0 (must not probe during active cooldown)", healthyProvider.CallCount())
	}
}

func TestProber_SkipsProvidersWithoutHealthChecker(t *testing.T) {
	reg := NewRegistry(proberTestBreakerConfig())
	b := reg.Get("plain", "m")
	b.RecordFailure()

	prober := NewProber(reg, map[string]providers.Provider{"plain": fakeNoHealthCheck{}}, time.Hour, time.Second)

	time.Sleep(proberTestBreakerConfig().Cooldown + 10*time.Millisecond)
	prober.probeOnce(context.Background()) // must not panic

	if b.State() != Open {
		t.Errorf("State() = %v, want unchanged Open (no HealthChecker to probe with)", b.State())
	}
}

func TestProber_FailedProbeReopens(t *testing.T) {
	reg := NewRegistry(proberTestBreakerConfig())
	b := reg.Get("sick", "m")
	b.RecordFailure()

	sickProvider := mock.New("sick", 0, 1, 503) // HealthCheck always fails
	prober := NewProber(reg, map[string]providers.Provider{"sick": sickProvider}, time.Hour, time.Second)

	time.Sleep(proberTestBreakerConfig().Cooldown + 10*time.Millisecond)
	prober.probeOnce(context.Background())

	if b.State() != Open {
		t.Errorf("State() = %v, want Open (a failed half-open probe must reopen)", b.State())
	}
}

func TestProber_Run_StopsOnContextCancel(t *testing.T) {
	reg := NewRegistry(proberTestBreakerConfig())
	prober := NewProber(reg, map[string]providers.Provider{}, time.Millisecond, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		prober.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return promptly after ctx was cancelled")
	}
}
