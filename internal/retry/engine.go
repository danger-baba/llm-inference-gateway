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
	"github.com/danger-baba/llm-inference-gateway/internal/metrics"
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
	// Model is the actual vendor model string the winning candidate
	// used, not the client-facing alias -- what the ledger's cost
	// computation needs, since pricing is per real model, not per alias.
	Model    string
	Attempts []Attempt
	// ProviderDuration is summed across every attempt this call made
	// (including failed ones before the winning candidate, if any), so
	// callers can compute gateway_proxy_overhead_seconds as their own
	// total latency minus this.
	ProviderDuration time.Duration
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
	var providerDuration time.Duration
	var lastErr error = ErrNoHealthyProvider

	for _, tier := range tiers {
		tried := make(map[string]bool)
		for {
			candidate, ok := e.pickFromTier(tier, tried)
			if !ok {
				break // tier exhausted or fully drained; advance to next tier
			}
			tried[candidate.ProviderName] = true

			resp, status, err := e.tryCandidate(ctx, candidate, req, &attempts, &providerDuration)
			if err == nil {
				return &Result{
					Response: resp, Provider: candidate.ProviderName, Model: candidate.Model,
					Attempts: attempts, ProviderDuration: providerDuration,
				}, nil
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
func (e *Engine) tryCandidate(ctx context.Context, c router.Candidate, req *providers.CanonicalRequest, attempts *[]Attempt, totalProviderDuration *time.Duration) (*providers.CanonicalResponse, int, error) {
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
		callStart := time.Now()
		resp, err := provider.Complete(attemptCtx, &providerReq)
		callDuration := time.Since(callStart)
		cancel()

		*totalProviderDuration += callDuration
		metrics.ProviderDuration.WithLabelValues(c.ProviderName, c.Model).Observe(callDuration.Seconds())

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
		metrics.RetriesTotal.WithLabelValues(class.String()).Inc()
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

// StreamResult mirrors Result for a completed streaming call: there is no
// final Response, since the content already went out incrementally, but
// Usage carries whatever the winning provider's own final Delta reported
// (nil if it never sent one — see providers.Delta's doc comment).
type StreamResult struct {
	Provider string
	// Model is the actual vendor model string the winning candidate
	// used -- see Result.Model's doc comment.
	Model            string
	Attempts         []Attempt
	Usage            *providers.Usage
	ProviderDuration time.Duration
}

// StreamError is Error's streaming counterpart. Flushed records whether
// any content ever reached onDelta successfully before Err occurred — the
// caller needs this to know whether the client already has partial
// content in hand (and must be told the stream ended in error) or whether
// nothing was ever sent (and a normal error response is still possible).
type StreamError struct {
	Err      error
	Attempts []Attempt
	Flushed  bool
}

func (e *StreamError) Error() string { return e.Err.Error() }
func (e *StreamError) Unwrap() error { return e.Err }

// ExecuteStream walks tiers exactly as Execute does, but for a streaming
// completion: each provider Delta is handed to onDelta as soon as it
// arrives rather than assembled into one response. Per the README, this
// changes the failover contract the moment onDelta first succeeds with
// real content: before that point a failure behaves exactly like Execute
// (reselect within the tier, fall over to the next tier, or abort on a
// Terminal classification); after that point, ExecuteStream will not try
// a second provider under any circumstances, because splicing two
// providers' output into one response would be silently wrong, not
// merely imperfect. It instead returns immediately with Flushed: true so
// the caller can end the stream with an explicit error event.
func (e *Engine) ExecuteStream(ctx context.Context, tiers []router.Tier, req *providers.CanonicalRequest, onDelta func(providerName, model string, d providers.Delta) error) (*StreamResult, error) {
	var attempts []Attempt
	var providerDuration time.Duration
	var lastErr error = ErrNoHealthyProvider

	for _, tier := range tiers {
		tried := make(map[string]bool)
		for {
			candidate, ok := e.pickFromTier(tier, tried)
			if !ok {
				break
			}
			tried[candidate.ProviderName] = true

			usage, flushed, status, err := e.tryCandidateStream(ctx, candidate, req, onDelta, &attempts, &providerDuration)
			if err == nil {
				return &StreamResult{
					Provider: candidate.ProviderName, Model: candidate.Model,
					Attempts: attempts, Usage: usage, ProviderDuration: providerDuration,
				}, nil
			}
			if errors.Is(err, errBreakerRejected) {
				continue
			}
			if flushed {
				return nil, &StreamError{Err: err, Attempts: attempts, Flushed: true}
			}
			lastErr = err

			class := e.providers[candidate.ProviderName].Classify(err, status)
			if class == providers.Terminal {
				return nil, &StreamError{Err: err, Attempts: attempts}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, &StreamError{Err: ctxErr, Attempts: attempts}
			}
		}
	}

	return nil, &StreamError{Err: lastErr, Attempts: attempts}
}

// tryCandidateStream is tryCandidate's streaming counterpart: same
// breaker gate, same per-provider attempt/backoff loop, but once any
// attempt's relayStream call reports flushed, every subsequent failure
// (including from a later attempt of the same candidate) is returned
// immediately rather than retried, since content already reached the
// client.
func (e *Engine) tryCandidateStream(ctx context.Context, c router.Candidate, req *providers.CanonicalRequest, onDelta func(string, string, providers.Delta) error, attempts *[]Attempt, totalProviderDuration *time.Duration) (*providers.Usage, bool, int, error) {
	provider := e.providers[c.ProviderName]
	br := e.breakers.Get(c.ProviderName, c.Model)

	if !br.Allow() {
		return nil, false, 0, errBreakerRejected
	}

	providerReq := *req
	providerReq.Model = c.Model

	var lastErr error
	var lastStatus int
	flushed := false

	for attempt := 0; attempt < e.cfg.MaxAttemptsPerProvider; attempt++ {
		if attempt > 0 {
			wait := backoffFor(attempt-1, e.cfg.BaseBackoff, e.cfg.MaxBackoff, lastErr)
			if !sleepWithinDeadline(ctx, wait) {
				return nil, flushed, lastStatus, lastErr
			}
		}

		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout(ctx, e.providerTimeout[c.ProviderName]))
		callStart := time.Now()
		usage, attemptFlushed, err := e.relayStream(attemptCtx, provider, &providerReq, c.ProviderName, c.Model, onDelta)
		callDuration := time.Since(callStart)
		cancel()
		if attemptFlushed {
			flushed = true
		}

		*totalProviderDuration += callDuration
		metrics.ProviderDuration.WithLabelValues(c.ProviderName, c.Model).Observe(callDuration.Seconds())

		if err == nil {
			br.RecordSuccess()
			*attempts = append(*attempts, Attempt{Provider: c.ProviderName, Status: 200})
			return usage, flushed, 200, nil
		}

		status := 0
		var apiErr *providers.APIError
		if errors.As(err, &apiErr) {
			status = apiErr.StatusCode
		}
		*attempts = append(*attempts, Attempt{Provider: c.ProviderName, Status: status})

		if flushed {
			// Content already reached the client through this candidate:
			// no more attempts, no reclassification, no failover.
			return nil, true, status, err
		}

		class := provider.Classify(err, status)
		metrics.RetriesTotal.WithLabelValues(class.String()).Inc()
		if class != providers.Terminal {
			br.RecordFailure()
		}
		lastErr, lastStatus = err, status

		if class != providers.Retryable {
			return nil, false, status, err
		}
	}
	return nil, flushed, lastStatus, lastErr
}

// relayStream runs one Stream call and forwards each Delta to onDelta as
// it arrives. providers.Provider.Stream never closes out itself (every
// implementation leaves channel lifecycle to its caller), so relayStream
// owns that here: it closes the provider-facing channel once Stream
// returns and only then reports Stream's own error.
func (e *Engine) relayStream(ctx context.Context, provider providers.Provider, req *providers.CanonicalRequest, providerName, model string, onDelta func(string, string, providers.Delta) error) (*providers.Usage, bool, error) {
	providerChan := make(chan providers.Delta)
	errCh := make(chan error, 1)
	go func() {
		err := provider.Stream(ctx, req, providerChan)
		close(providerChan)
		errCh <- err
	}()

	var usage *providers.Usage
	flushed := false
	for delta := range providerChan {
		if delta.Usage != nil {
			usage = delta.Usage
		}
		if err := onDelta(providerName, model, delta); err != nil {
			// Drain so the goroutine above never blocks on a send nobody
			// will receive; its own Stream call will still return on its
			// own terms (every implementation selects on ctx being done).
			for range providerChan {
			}
			<-errCh
			return usage, flushed, err
		}
		if delta.Content != "" {
			flushed = true
		}
	}
	return usage, flushed, <-errCh
}

// Pricing looks up a provider's per-model USD price, for callers (the
// ledger) that only see a Result/StreamResult's Provider/Model strings
// and have no other way to reach the concrete provider instance the
// engine already holds.
func (e *Engine) Pricing(providerName, model string) (inPerMTok, outPerMTok float64) {
	p, ok := e.providers[providerName]
	if !ok {
		return 0, 0
	}
	return p.Pricing(model)
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
