// Package router resolves a client-facing model alias to an ordered list
// of candidate (provider, provider-model) pairs. Phase 2's handler only
// ever calls the first candidate; walking the rest on failure is Phase 3's
// retry/fallback engine.
package router

import (
	"errors"
	"sort"

	"github.com/danger-baba/llm-inference-gateway/internal/config"
)

var ErrUnknownModel = errors.New("router: unknown model alias")

type Candidate struct {
	ProviderName string
	Model        string
}

type Router struct {
	modelAliases   map[string]map[string]string
	fallbackChains map[string][]string
	providerOrder  []string
}

// New builds the default provider order once, ascending by priority and
// stable on ties so declaration order in config breaks ties deterministically.
func New(cfg *config.Config) *Router {
	idx := make([]int, len(cfg.Providers))
	for i := range cfg.Providers {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		return cfg.Providers[idx[a]].Priority < cfg.Providers[idx[b]].Priority
	})

	order := make([]string, len(idx))
	for i, j := range idx {
		order[i] = cfg.Providers[j].Name
	}

	return &Router{
		modelAliases:   cfg.ModelAliases,
		fallbackChains: cfg.FallbackChains,
		providerOrder:  order,
	}
}

// Aliases returns the client-facing model names this router knows about,
// sorted for a stable /v1/models listing.
func (r *Router) Aliases() []string {
	ids := make([]string, 0, len(r.modelAliases))
	for id := range r.modelAliases {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Resolve returns candidates for alias in the order they should be tried:
// the alias's fallback_chains entry if one exists, else priority order.
// Providers absent from the alias's model_aliases map are skipped rather
// than erroring, since a chain may legitimately name providers this alias
// doesn't support.
func (r *Router) Resolve(alias string) ([]Candidate, error) {
	providerModels, ok := r.modelAliases[alias]
	if !ok || len(providerModels) == 0 {
		return nil, ErrUnknownModel
	}

	order := r.fallbackChains[alias]
	if len(order) == 0 {
		order = r.providerOrder
	}

	candidates := make([]Candidate, 0, len(order))
	for _, name := range order {
		model, ok := providerModels[name]
		if !ok {
			continue
		}
		candidates = append(candidates, Candidate{ProviderName: name, Model: model})
	}
	if len(candidates) == 0 {
		return nil, ErrUnknownModel
	}
	return candidates, nil
}
