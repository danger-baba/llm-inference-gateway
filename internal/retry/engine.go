// Package retry walks the router's tiers, retrying a chosen candidate
// through its own inner loop before either reselecting within the tier or
// falling forward to the next one — exactly the two nested loops the
// README specifies. A Terminal-classified failure aborts immediately: the
// problem is the request, not the provider.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/breaker"
	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

// ErrNoHealthyProvider means every tier was exhausted without a single
// candidate ever being ready to try — every breaker in the chain was open.
var ErrNoHealthyProvider = errors.New("retry: no healthy provider available")

// errBreakerRejected signals that Allow() was lost in the rare race
// against Ready()'s filtering; the caller must treat this as "reselect
// within the tier," not as a real, attempt-counting failure.
var errBreakerRejected = errors.New("retry: breaker rejected the attempt")

type Attempt struct {
	Provider string
	Status   int // 0 when no HTTP status is available (e.g. a timeout)
}

func (a Attempt) String() string {
	return fmt.Sprintf("%s:%d", a.Provider, a.Status)
}

type Result struct {
	Response *providers.CanonicalResponse
	Provider string
	Attempts []Attempt
}

// Error wraps the terminal cause of a failed Execute call together with
// whatever attempts were actually made, so callers can still report
// X-Gateway-Attempts on a failure response.
type Error struct {
	Err      error
	Attempts []Attempt
}

func (e *Error) Error() string { return e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

type Config struct {
	MaxAttemptsPerProvider int
	BaseBackoff            time.Duration
	MaxBackoff             time.Duration
}

type Engine struct {
	breakers        *breaker.Registry
	providers       map[string]providers.Provider
	providerTimeout map[string]time.Duration
	cfg             Config
}

func New(breakers *breaker.Registry, provs map[string]providers.Provider, providerTimeout map[string]time.Duration, cfg Config) *Engine {
	return &Engine{breakers: breakers, providers: provs, providerTimeout: providerTimeout, cfg: cfg}
}

// Execute tries tiers in order. Within a tier, it repeatedly weighted-picks
// among untried, breaker-ready candidates; when a pick's own retry budget
// is exhausted or it's Fallback-classified, it reselects among whoever's
// left in the tier before moving on to the next one.
func (e *Engine) Execute(ctx context.Context, tiers []router.Tier, req *providers.CanonicalRequest) (*Result, error) {
	var attempts []Attempt
	var lastErr error = ErrNoHealthyProvider

	for _, tier := range tiers {
		tried := make(map[string]bool)
		for {
			candidate, ok := e.pickFromTier(tier, tried)
			if !ok {
				break // tier exhausted or fully drained; advance to next tier
			}
			tried[candidate.ProviderName] = true

			resp, status, err := e.tryCandidate(ctx, candidate, req, &attempts)
			if err == nil {
				return &Result{Response: resp, Provider: candidate.ProviderName, Attempts: attempts}, nil
			}
			if errors.Is(err, errBreakerRejected) {
				continue // never counted as an attempt; try someone else in the tier
			}
			lastErr = err

			class := e.providers[candidate.ProviderName].Classify(err, status)
			if class == providers.Terminal {
				return nil, &Error{Err: err, Attempts: attempts}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, &Error{Err: ctxErr, Attempts: attempts}
			}
			// Retryable-exhausted or Fallback: loop reselects within the
			// tier, or pickFromTier returns false and we advance.
		}
	}

	return nil, &Error{Err: lastErr, Attempts: attempts}
}

// tryCandidate runs the inner retry loop for one candidate: up to
// MaxAttemptsPerProvider tries, full-jitter backoff between them (or
// Retry-After when the provider sent one), stopping the moment a failure
// isn't classified Retryable.
func (e *Engine) tryCandidate(ctx context.Context, c router.Candidate, req *providers.CanonicalRequest, attempts *[]Attempt) (*providers.CanonicalResponse, int, error) {
	provider := e.providers[c.ProviderName]
	br := e.breakers.Get(c.ProviderName, c.Model)

	if !br.Allow() {
		return nil, 0, errBreakerRejected
	}

	providerReq := *req
	providerReq.Model = c.Model

	var lastErr error
	var lastStatus int
	for attempt := 0; attempt < e.cfg.MaxAttemptsPerProvider; attempt++ {
		if attempt > 0 {
			wait := backoffFor(attempt-1, e.cfg.BaseBackoff, e.cfg.MaxBackoff, lastErr)
			if !sleepWithinDeadline(ctx, wait) {
				// No new network call was made, so no new attempt is
				// recorded and the breaker isn't charged again for the
				// same underlying failure already recorded last iteration.
				// Return lastErr, not ctx.Err(): the context may not have
				// actually expired yet, only turned out too close to fit
				// the backoff, and ctx.Err() would be nil in that case.
				return nil, lastStatus, lastErr
			}
		}

		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout(ctx, e.providerTimeout[c.ProviderName]))
		resp, err := provider.Complete(attemptCtx, &providerReq)
		cancel()

		if err == nil {
			br.RecordSuccess()
			*attempts = append(*attempts, Attempt{Provider: c.ProviderName, Status: 200})
			return resp, 200, nil
		}

		status := 0
		var apiErr *providers.APIError
		if errors.As(err, &apiErr) {
			status = apiErr.StatusCode
		}
		*attempts = append(*attempts, Attempt{Provider: c.ProviderName, Status: status})

		class := provider.Classify(err, status)
		if class != providers.Terminal {
			br.RecordFailure() // Retryable and Fallback both count against provider health.
		}
		lastErr, lastStatus = err, status

		if class != providers.Retryable {
			return nil, status, err // Fallback or Terminal: retrying this provider is pointless.
		}
	}
	return nil, lastStatus, lastErr
}

// pickFromTier weighted-selects among candidates not yet tried this
// request and currently Ready() (a peek, not a consuming Allow()). If the
// remaining eligible candidates' weights sum to zero, the tier is treated
// as fully drained rather than falling back to a uniform pick.
func (e *Engine) pickFromTier(tier router.Tier, tried map[string]bool) (router.Candidate, bool) {
	var eligible []router.Candidate
	var totalWeight int
	for _, c := range tier.Candidates {
		if tried[c.ProviderName] {
			continue
		}
		if _, ok := e.providers[c.ProviderName]; !ok {
			continue // configured in the router but not wired up; skip defensively
		}
		if !e.breakers.Get(c.ProviderName, c.Model).Ready() {
			continue
		}
		eligible = append(eligible, c)
		totalWeight += c.Weight
	}
	if len(eligible) == 0 || totalWeight <= 0 {
		return router.Candidate{}, false
	}

	pick := rand.Intn(totalWeight)
	for _, c := range eligible {
		if pick < c.Weight {
			return c, true
		}
		pick -= c.Weight
	}
	return eligible[len(eligible)-1], true // unreachable in practice; defensive
}
