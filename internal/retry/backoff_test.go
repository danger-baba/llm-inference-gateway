package retry

import (
	"context"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

func TestFullJitterBackoff_WithinBounds(t *testing.T) {
	base := 10 * time.Millisecond
	maxBackoff := 100 * time.Millisecond
	for attempt := 0; attempt < 10; attempt++ {
		for i := 0; i < 50; i++ {
			got := FullJitterBackoff(attempt, base, maxBackoff)
			if got < 0 || got >= maxBackoff+time.Millisecond {
				t.Fatalf("FullJitterBackoff(%d, ...) = %v, want in [0, %v)", attempt, got, maxBackoff)
			}
		}
	}
}

func TestFullJitterBackoff_CapsAtMaxBackoff(t *testing.T) {
	base := time.Second
	maxBackoff := 50 * time.Millisecond
	for i := 0; i < 20; i++ {
		got := FullJitterBackoff(5, base, maxBackoff) // base*2^5 would hugely exceed maxBackoff
		if got >= maxBackoff {
			t.Errorf("FullJitterBackoff() = %v, want < maxBackoff (%v)", got, maxBackoff)
		}
	}
}

func TestFullJitterBackoff_ZeroCapReturnsZero(t *testing.T) {
	if got := FullJitterBackoff(0, time.Second, 0); got != 0 {
		t.Errorf("FullJitterBackoff() = %v, want 0", got)
	}
}

func TestBackoffFor_HonoursRetryAfter(t *testing.T) {
	err := &providers.APIError{StatusCode: 429, RetryAfter: 3 * time.Second}
	got := backoffFor(0, time.Millisecond, time.Second, err)
	if got != 3*time.Second {
		t.Errorf("backoffFor() = %v, want the Retry-After value (3s)", got)
	}
}

func TestBackoffFor_FallsBackToFullJitterWithoutRetryAfter(t *testing.T) {
	err := &providers.APIError{StatusCode: 500}
	got := backoffFor(0, time.Millisecond, 10*time.Millisecond, err)
	if got < 0 || got >= 10*time.Millisecond {
		t.Errorf("backoffFor() = %v, want within the full-jitter bound", got)
	}
}

func TestSleepWithinDeadline_RefusesWhenNotEnoughBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if sleepWithinDeadline(ctx, time.Hour) {
		t.Error("sleepWithinDeadline() = true, want false (not enough budget)")
	}
}

func TestSleepWithinDeadline_SleepsWhenBudgetAllows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	if !sleepWithinDeadline(ctx, 20*time.Millisecond) {
		t.Fatal("sleepWithinDeadline() = false, want true")
	}
	if elapsed := time.Since(start); elapsed < 15*time.Millisecond {
		t.Errorf("elapsed = %v, want roughly >= 20ms", elapsed)
	}
}

func TestSleepWithinDeadline_ZeroReturnsImmediately(t *testing.T) {
	if !sleepWithinDeadline(context.Background(), 0) {
		t.Error("sleepWithinDeadline(0) = false, want true")
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
