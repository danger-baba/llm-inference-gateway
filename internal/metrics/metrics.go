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
		Buckets: prometheus.DefBuckets,
	})

	ProxyOverhead = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "gateway_proxy_overhead_seconds",
		Help:    "Gateway-added latency, excluding time spent inside provider calls.",
		Buckets: prometheus.DefBuckets,
	})

	// ProviderDuration is labelled by provider and model despite the
	// table showing no `{}` for it, because its own stated Purpose --
	// "isolates blame" -- is meaningless without per-provider breakdown.
	// A documented, deliberate exception; see docs/adr/0014.
	ProviderDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_provider_duration_seconds",
		Help:    "Upstream provider call latency, isolating provider-side blame.",
		Buckets: prometheus.DefBuckets,
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
