// Package router resolves a client-facing model alias to the ordered
// tiers of candidates the retry/fallback engine should try. It only
// builds structure; breaker-health filtering and weighted selection among
// live candidates happen at call time in internal/retry, since health can
// change between two requests for the same alias.
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
	Weight       int
}

// Tier groups candidates that should be chosen among by weight. An
// explicit fallback_chains entry produces one candidate per tier (see
// Resolve), which makes "pick the only candidate in this tier" degenerate
// correctly into "follow the chain's exact order" without special-casing.
type Tier struct {
	Candidates []Candidate
}

type Router struct {
	modelAliases     map[string]map[string]string
	fallbackChains   map[string][]string
	providerOrder    []string
	providerPriority map[string]int
	providerWeight   map[string]int
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
	priority := make(map[string]int, len(idx))
	weight := make(map[string]int, len(idx))
	for i, j := range idx {
		p := cfg.Providers[j]
		order[i] = p.Name
		priority[p.Name] = p.Priority
		weight[p.Name] = p.Weight
	}

	return &Router{
		modelAliases:     cfg.ModelAliases,
		fallbackChains:   cfg.FallbackChains,
		providerOrder:    order,
		providerPriority: priority,
		providerWeight:   weight,
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

// Resolve returns alias's candidates grouped into tiers, in the order
// they should be tried. If an explicit fallback_chains entry exists, each
// listed provider becomes its own singleton tier, preserving exact order —
// weight is irrelevant to an explicit chain. Otherwise, candidates are
// grouped by ascending priority; providers sharing a priority land in the
// same tier, to be chosen among by weight at call time. Providers absent
// from the alias's model_aliases map are skipped rather than erroring,
// since a chain or tier may legitimately name providers this alias
// doesn't support.
func (r *Router) Resolve(alias string) ([]Tier, error) {
	providerModels, ok := r.modelAliases[alias]
	if !ok || len(providerModels) == 0 {
		return nil, ErrUnknownModel
	}

	if chain, ok := r.fallbackChains[alias]; ok && len(chain) > 0 {
		tiers := make([]Tier, 0, len(chain))
		for _, name := range chain {
			model, ok := providerModels[name]
			if !ok {
				continue
			}
			tiers = append(tiers, Tier{Candidates: []Candidate{{
				ProviderName: name, Model: model, Weight: r.providerWeight[name],
			}}})
		}
		if len(tiers) == 0 {
			return nil, ErrUnknownModel
		}
		return tiers, nil
	}

	var tiers []Tier
	var lastPriority int
	haveTier := false
	for _, name := range r.providerOrder {
		model, ok := providerModels[name]
		if !ok {
			continue
		}
		p := r.providerPriority[name]
		cand := Candidate{ProviderName: name, Model: model, Weight: r.providerWeight[name]}
		if haveTier && p == lastPriority {
			tiers[len(tiers)-1].Candidates = append(tiers[len(tiers)-1].Candidates, cand)
		} else {
			tiers = append(tiers, Tier{Candidates: []Candidate{cand}})
			lastPriority = p
			haveTier = true
		}
	}
	if len(tiers) == 0 {
		return nil, ErrUnknownModel
	}
	return tiers, nil
}
