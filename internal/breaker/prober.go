package breaker

import (
	"context"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

// Prober periodically gives OPEN breakers whose cooldown has elapsed a
// synthetic trial via the provider's optional HealthChecker, so a
// provider that recovers while receiving no real traffic is still
// discovered — per the README's design. A prober probe competes for the
// same half-open slot real traffic would use, via the same Ready/Allow
// pair tryCandidate uses, so it never probes during an active cooldown
// and never double-spends a slot a real request already claimed.
type Prober struct {
	registry  *Registry
	providers map[string]providers.Provider
	interval  time.Duration
	timeout   time.Duration
}

func NewProber(registry *Registry, provs map[string]providers.Provider, interval, timeout time.Duration) *Prober {
	return &Prober{registry: registry, providers: provs, interval: interval, timeout: timeout}
}

// Run blocks, probing on interval until ctx is done.
func (p *Prober) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.probeOnce(ctx)
		}
	}
}

func (p *Prober) probeOnce(ctx context.Context) {
	for _, b := range p.registry.Snapshot() {
		if b.State() != Open || !b.Ready() {
			continue
		}
		provider, ok := p.providers[b.ProviderName]
		if !ok {
			continue
		}
		hc, ok := provider.(providers.HealthChecker)
		if !ok {
			continue
		}
		if !b.Allow() {
			continue // lost the race for a half-open slot to real traffic
		}

		probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
		err := hc.HealthCheck(probeCtx)
		cancel()

		if err != nil {
			b.RecordFailure()
		} else {
			b.RecordSuccess()
		}
	}
}
