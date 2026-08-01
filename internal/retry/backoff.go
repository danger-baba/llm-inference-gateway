package retry

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

// FullJitterBackoff returns a random duration in [0, min(base*2^attempt,
// maxBackoff)). The random floor of zero — not base/2 — is what actually
// breaks up a thundering herd: every client that failed at the same
// instant lands somewhere different in the interval, not synchronized
// around a shared midpoint.
func FullJitterBackoff(attempt int, base, maxBackoff time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 62 { // guard against overflow in the shift below
		attempt = 62
	}
	backoff := base * (1 << attempt)
	if backoff <= 0 || backoff > maxBackoff {
		backoff = maxBackoff
	}
	if backoff <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(backoff)))
}

// backoffFor honours a provider's Retry-After when present; only falls
// back to the computed full-jitter delay otherwise.
func backoffFor(attempt int, base, maxBackoff time.Duration, err error) time.Duration {
	var apiErr *providers.APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		return apiErr.RetryAfter
	}
	return FullJitterBackoff(attempt, base, maxBackoff)
}

// sleepWithinDeadline waits d, but never past ctx's deadline: if d itself
// wouldn't fit in the time remaining, it refuses to sleep at all rather
// than blocking past the caller's budget only to fail anyway.
func sleepWithinDeadline(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	if deadline, ok := ctx.Deadline(); ok {
		if time.Until(deadline) < d {
			return false
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// attemptTimeout caps configured at whatever's left on ctx's own deadline,
// so a single attempt can never outlive the caller's overall budget.
func attemptTimeout(ctx context.Context, configured time.Duration) time.Duration {
	timeout := configured
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout < 0 {
		timeout = 0
	}
	return timeout
}
