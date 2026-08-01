package breaker

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/danger-baba/llm-inference-gateway/internal/metrics"
)

func testConfig() Config {
	return Config{
		ErrorRateThreshold: 0.5,
		MinRequests:        4,
		Window:             time.Second, // one bucket; no sleeping needed to test trips
		Cooldown:           30 * time.Millisecond,
		CooldownMax:        200 * time.Millisecond,
		HalfOpenProbes:     2,
	}
}

func TestClosed_AlwaysAllows(t *testing.T) {
	b := newBreaker("p", "m", testConfig())
	for i := 0; i < 5; i++ {
		if !b.Allow() {
			t.Fatalf("Allow() = false in closed state on call %d", i)
		}
	}
}

func TestTrips_OnErrorRateAboveThresholdWithVolume(t *testing.T) {
	b := newBreaker("p", "m", testConfig())

	b.RecordSuccess()
	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure() // 3/4 failures = 0.75 >= 0.5, volume 4 >= MinRequests 4

	if b.State() != Open {
		t.Fatalf("State() = %v, want Open after tripping", b.State())
	}
	if b.Allow() {
		t.Error("Allow() = true immediately after tripping, want false (cooldown not elapsed)")
	}
}

func TestSetState_UpdatesBreakerStateGauge(t *testing.T) {
	// A distinct provider name keeps this test's gauge reads from
	// colliding with every other test in this file writing to the same
	// process-global metric under labels "p"/"flaky"/etc.
	const provider = "gauge-test-provider"
	gauge := func() float64 { return testutil.ToFloat64(metrics.BreakerState.WithLabelValues(provider)) }

	b := newBreaker(provider, "m", testConfig())
	if got := gauge(); got != 0 {
		t.Fatalf("gauge on creation = %v, want 0 (Closed)", got)
	}

	b.RecordSuccess()
	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure() // trips: 3/4 failures >= threshold, volume >= MinRequests
	if got := gauge(); got != 2 {
		t.Fatalf("gauge after tripping = %v, want 2 (Open)", got)
	}

	time.Sleep(testConfig().Cooldown + 5*time.Millisecond)
	b.Allow() // Open -> HalfOpen once cooldown elapses
	if got := gauge(); got != 1 {
		t.Fatalf("gauge after cooldown elapses = %v, want 1 (HalfOpen)", got)
	}

	b.RecordSuccess()
	b.RecordSuccess() // HalfOpenProbes=2 consecutive successes -> Closed
	if got := gauge(); got != 0 {
		t.Fatalf("gauge after recovering = %v, want 0 (Closed)", got)
	}
}

func TestDoesNotTrip_BelowMinimumVolume(t *testing.T) {
	b := newBreaker("p", "m", testConfig())

	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure() // 100% failure, but only 3 requests < MinRequests 4

	if b.State() != Closed {
		t.Fatalf("State() = %v, want Closed (below minimum volume)", b.State())
	}
	if !b.Allow() {
		t.Error("Allow() = false while still Closed")
	}
}

func TestOpen_ShortCircuitsUntilCooldownElapses(t *testing.T) {
	cfg := testConfig()
	b := newBreaker("p", "m", cfg)
	tripBreaker(b)

	if b.Allow() {
		t.Fatal("Allow() = true before cooldown elapsed")
	}

	time.Sleep(cfg.Cooldown + 10*time.Millisecond)

	if !b.Allow() {
		t.Fatal("Allow() = false after cooldown elapsed, want true (half-open probe admitted)")
	}
	if b.State() != HalfOpen {
		t.Fatalf("State() = %v, want HalfOpen", b.State())
	}
}

func TestHalfOpen_ConsecutiveSuccessesClose(t *testing.T) {
	cfg := testConfig()
	b := newBreaker("p", "m", cfg)
	tripBreaker(b)
	time.Sleep(cfg.Cooldown + 10*time.Millisecond)

	for i := 0; i < cfg.HalfOpenProbes; i++ {
		if !b.Allow() {
			t.Fatalf("Allow() = false on half-open probe %d", i)
		}
		b.RecordSuccess()
	}

	if b.State() != Closed {
		t.Fatalf("State() = %v, want Closed after %d consecutive half-open successes", b.State(), cfg.HalfOpenProbes)
	}
}

func TestHalfOpen_AnyFailureReopensAndDoublesCooldown(t *testing.T) {
	cfg := testConfig()
	b := newBreaker("p", "m", cfg)
	tripBreaker(b)
	time.Sleep(cfg.Cooldown + 10*time.Millisecond)

	if !b.Allow() {
		t.Fatal("Allow() = false on first half-open probe")
	}
	b.RecordFailure()

	if b.State() != Open {
		t.Fatalf("State() = %v, want Open after a half-open failure", b.State())
	}

	// Original cooldown has elapsed again, but it should have doubled, so
	// the breaker must still refuse.
	time.Sleep(cfg.Cooldown + 10*time.Millisecond)
	if b.Allow() {
		t.Fatal("Allow() = true after only the original cooldown; cooldown should have doubled")
	}

	// Now wait past the doubled cooldown too.
	time.Sleep(cfg.Cooldown + 20*time.Millisecond)
	if !b.Allow() {
		t.Fatal("Allow() = false after the doubled cooldown elapsed")
	}
}

func TestHalfOpen_ProbeQuotaIsBounded(t *testing.T) {
	cfg := testConfig()
	b := newBreaker("p", "m", cfg)
	tripBreaker(b)
	time.Sleep(cfg.Cooldown + 10*time.Millisecond)

	admitted := 0
	for i := 0; i < cfg.HalfOpenProbes+3; i++ {
		if b.Allow() {
			admitted++
		}
	}
	if admitted != cfg.HalfOpenProbes {
		t.Errorf("admitted = %d, want exactly HalfOpenProbes (%d)", admitted, cfg.HalfOpenProbes)
	}
}

func TestReady_DoesNotConsumeHalfOpenQuota(t *testing.T) {
	cfg := testConfig()
	b := newBreaker("p", "m", cfg)
	tripBreaker(b)
	time.Sleep(cfg.Cooldown + 10*time.Millisecond)

	for i := 0; i < 10; i++ {
		if !b.Ready() {
			t.Fatalf("Ready() = false on call %d, want true (cooldown elapsed)", i)
		}
	}

	// Ready() must not have consumed any probe slots or forced the
	// half-open transition itself in a way that drains the quota.
	admitted := 0
	for i := 0; i < cfg.HalfOpenProbes+3; i++ {
		if b.Allow() {
			admitted++
		}
	}
	if admitted != cfg.HalfOpenProbes {
		t.Errorf("admitted = %d after repeated Ready() calls, want %d (Ready must not consume quota)", admitted, cfg.HalfOpenProbes)
	}
}

func tripBreaker(b *Breaker) {
	b.RecordSuccess()
	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure()
}
