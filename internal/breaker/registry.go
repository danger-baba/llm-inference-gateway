package breaker

import "sync"

// Registry lazily creates and hands out one Breaker per (provider, model)
// pair, sharing config across all of them.
type Registry struct {
	mu       sync.Mutex
	cfg      Config
	breakers map[string]*Breaker
}

func NewRegistry(cfg Config) *Registry {
	return &Registry{cfg: cfg, breakers: make(map[string]*Breaker)}
}

func key(providerName, model string) string {
	return providerName + "\x00" + model
}

func (r *Registry) Get(providerName, model string) *Breaker {
	k := key(providerName, model)

	r.mu.Lock()
	defer r.mu.Unlock()
	if b, ok := r.breakers[k]; ok {
		return b
	}
	b := newBreaker(providerName, model, r.cfg)
	r.breakers[k] = b
	return b
}

// Snapshot returns every breaker created so far. The background prober
// uses it to find OPEN breakers worth probing without needing to already
// know every (provider, model) pair up front.
func (r *Registry) Snapshot() []*Breaker {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Breaker, 0, len(r.breakers))
	for _, b := range r.breakers {
		out = append(out, b)
	}
	return out
}
