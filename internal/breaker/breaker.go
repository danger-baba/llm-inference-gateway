// Package breaker implements one circuit breaker per (provider, model)
// pair, exactly as specified in the README: a sliding window of per-second
// success/failure counts, an error-rate trip gated on minimum volume, a
// cooldown that doubles to a ceiling, and a half-open probe quota.
package breaker

import (
	"sync"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/metrics"
)

type State int

const (
	Closed State = iota
	HalfOpen
	Open
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

type Config struct {
	ErrorRateThreshold float64
	MinRequests        int
	Window             time.Duration
	Cooldown           time.Duration
	CooldownMax        time.Duration
	HalfOpenProbes     int
}

type bucket struct {
	unixSec   int64
	successes int64
	failures  int64
}

// Breaker guards calls to one (provider, model) pair. All exported methods
// are safe for concurrent use, since a breaker is read on every request
// and written on every outcome.
type Breaker struct {
	ProviderName string
	Model        string

	cfg Config

	mu            sync.Mutex
	state         State
	buckets       []bucket
	windowSeconds int64

	curCooldown     time.Duration
	openedAt        time.Time
	halfOpenAllowed int
	halfOpenWins    int
}

func newBreaker(providerName, model string, cfg Config) *Breaker {
	windowSeconds := int64(cfg.Window.Seconds())
	if windowSeconds < 1 {
		windowSeconds = 1
	}
	b := &Breaker{
		ProviderName:  providerName,
		Model:         model,
		cfg:           cfg,
		buckets:       make([]bucket, windowSeconds),
		windowSeconds: windowSeconds,
		curCooldown:   cfg.Cooldown,
	}
	metrics.BreakerState.WithLabelValues(providerName).Set(float64(Closed))
	return b
}

// setState is the only place b.state is ever assigned, so
// gateway_breaker_state stays in lockstep with every real transition
// instead of being sampled on a timer. The metric is labelled only by
// provider (matching the README's literal metric definition), so if two
// models share one provider name, whichever transitions last wins the
// gauge value for that provider -- an accepted, documented imprecision
// rather than a per-(provider,model) cardinality blowup. See docs/adr/0014.
func (b *Breaker) setState(s State) {
	b.state = s
	metrics.BreakerState.WithLabelValues(b.ProviderName).Set(float64(s))
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Ready reports whether a call to Allow would currently succeed, without
// consuming a half-open probe slot. Use it to filter candidates before
// picking one; call Allow exactly once, immediately before the real call,
// on whichever candidate is actually chosen.
func (b *Breaker) Ready() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return true
	case Open:
		return time.Since(b.openedAt) >= b.curCooldown
	case HalfOpen:
		return b.halfOpenAllowed > 0
	default:
		return false
	}
}

// Allow reports whether this call may proceed, and — unlike Ready —
// mutates state: an OPEN breaker whose cooldown has elapsed transitions to
// HALF_OPEN and consumes one probe slot in the same call. A caller that
// gets false back must not make the network call it was asking about.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == Open && time.Since(b.openedAt) >= b.curCooldown {
		b.setState(HalfOpen)
		b.halfOpenAllowed = b.cfg.HalfOpenProbes
		b.halfOpenWins = 0
	}

	switch b.state {
	case Closed:
		return true
	case HalfOpen:
		if b.halfOpenAllowed <= 0 {
			return false
		}
		b.halfOpenAllowed--
		return true
	default: // Open, cooldown not yet elapsed
		return false
	}
}

// RecordSuccess reports a successful call made after Allow returned true.
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case HalfOpen:
		b.halfOpenWins++
		if b.halfOpenWins >= b.cfg.HalfOpenProbes {
			b.setState(Closed)
			b.curCooldown = b.cfg.Cooldown
			b.resetWindow()
		}
	case Closed:
		b.record(true)
	}
}

// RecordFailure reports a failed call made after Allow returned true.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case HalfOpen:
		// Any half-open failure reopens immediately and doubles the
		// cooldown, up to the configured ceiling.
		b.curCooldown = min(b.curCooldown*2, b.cfg.CooldownMax)
		b.setState(Open)
		b.openedAt = time.Now()
	case Closed:
		b.record(false)
		if b.shouldTrip() {
			b.curCooldown = b.cfg.Cooldown
			b.setState(Open)
			b.openedAt = time.Now()
		}
	}
}

func (b *Breaker) record(success bool) {
	now := time.Now().Unix()
	idx := now % b.windowSeconds
	if b.buckets[idx].unixSec != now {
		b.buckets[idx] = bucket{unixSec: now}
	}
	if success {
		b.buckets[idx].successes++
	} else {
		b.buckets[idx].failures++
	}
}

// shouldTrip sums only buckets whose timestamp actually falls within the
// last windowSeconds; a bucket from a previous, not-yet-overwritten cycle
// is stale and must not count.
func (b *Breaker) shouldTrip() bool {
	now := time.Now().Unix()
	var total, failures int64
	for _, bk := range b.buckets {
		age := now - bk.unixSec
		if age >= 0 && age < b.windowSeconds {
			total += bk.successes + bk.failures
			failures += bk.failures
		}
	}
	if total < int64(b.cfg.MinRequests) {
		return false
	}
	return float64(failures)/float64(total) >= b.cfg.ErrorRateThreshold
}

func (b *Breaker) resetWindow() {
	for i := range b.buckets {
		b.buckets[i] = bucket{}
	}
}
