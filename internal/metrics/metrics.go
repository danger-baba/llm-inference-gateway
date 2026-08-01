// Package metrics defines the gateway's Prometheus collectors and
// registers them against the default registry, exactly the 11 metrics
// (names, types, labels) from the README's observability table. Every
// collector is a package-level variable, not something threaded through
// dependency injection: Prometheus instrumentation is cross-cutting by
// nature (a call site in the retry engine, the breaker, the rate
// limiter, and the chat handler all need to touch it), and the global
// registry is the idiomatic way client_golang expects that to work. See
// docs/adr/0014.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// latencyBuckets replaces client_golang's DefBuckets (tuned for generic
// web requests, coarsest right around 100ms-250ms) for all three latency
// histograms here. This is a real, load-test-driven correction, not a
// guess: running loadtest/overhead.js against a mock provider with a
// fixed 200ms latency showed DefBuckets' bucket_quantile-style linear
// interpolation undershooting the true ~202ms median by 25ms+, because
// the entire distribution sat inside DefBuckets' single (0.1s, 0.25s]
// gap with no boundary in between. These buckets add resolution
// specifically across the 25ms-750ms band where LLM gateway overhead and
// end-to-end latency actually live, while still covering sub-millisecond
// cache hits and multi-second slow completions. See docs/adr/0015.
var latencyBuckets = []float64{
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.25,
	0.3, 0.4, 0.5, 0.75, 1, 2, 5, 10,
}

var (
	RequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Total requests handled, by outcome and how they were served.",
	}, []string{"status", "provider", "cache"})

	// RequestDuration and ProxyOverhead carry no labels, matching the
	// README's observability table exactly (no `{}` shown for either) --
	// unlike gateway_requests_total or gateway_cache_hits_total, whose
	// breakdowns are explicitly specified. Per-provider/per-cache detail
	// for these two would come from gateway_provider_duration_seconds and
	// gateway_cache_hits_total instead, so nothing is lost by leaving
	// these two as the plain end-to-end and overhead-only signals.
	RequestDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "gateway_request_duration_seconds",
		Help:    "End-to-end request latency as observed by the client.",
		Buckets: latencyBuckets,
	})

	ProxyOverhead = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "gateway_proxy_overhead_seconds",
		Help:    "Gateway-added latency, excluding time spent inside provider calls.",
		Buckets: latencyBuckets,
	})

	// ProviderDuration is labelled by provider and model despite the
	// table showing no `{}` for it, because its own stated Purpose --
	// "isolates blame" -- is meaningless without per-provider breakdown.
	// A documented, deliberate exception; see docs/adr/0014.
	ProviderDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_provider_duration_seconds",
		Help:    "Upstream provider call latency, isolating provider-side blame.",
		Buckets: latencyBuckets,
	}, []string{"provider", "model"})

	CacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_cache_hits_total",
		Help: "Cache hits by tier.",
	}, []string{"tier"})

	TokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_tokens_total",
		Help: "Token throughput by direction.",
	}, []string{"direction"})

	TokensSavedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_tokens_saved_total",
		Help: "Completion tokens a cache hit avoided generating.",
	})

	CostUSDTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_cost_usd_total",
		Help: "Cumulative provider spend in USD, for burn-rate alerting.",
	})

	BreakerState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gateway_breaker_state",
		Help: "Circuit breaker state per provider: 0 closed, 1 half-open, 2 open.",
	}, []string{"provider"})

	RetriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_retries_total",
		Help: "Retry attempts by failure classification.",
	}, []string{"class"})

	RateLimitRejectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_ratelimit_rejections_total",
		Help: "Rate limit rejections by the scope that bound.",
	}, []string{"scope"})

	// LedgerDroppedTotal is not one of the README's 11 listed metrics,
	// but the README explicitly requires "drop and increment a counter"
	// when the ledger buffer is full (Cost accounting ledger / Failure
	// modes) -- this is that counter, exposed the same way as everything
	// else here since there's no reason to make it harder to alert on.
	LedgerDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_ledger_dropped_total",
		Help: "Usage ledger rows dropped because the write buffer was full.",
	})
)
