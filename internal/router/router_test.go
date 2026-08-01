package router

import (
	"errors"
	"reflect"
	"testing"

	"github.com/danger-baba/llm-inference-gateway/internal/config"
)

func testConfig() *config.Config {
	return &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "openai", Priority: 1, Weight: 7},
			{Name: "anthropic", Priority: 1, Weight: 3},
			{Name: "local-vllm", Priority: 0, Weight: 1},
		},
		ModelAliases: map[string]map[string]string{
			"fast": {
				"openai":     "gpt-4o-mini",
				"anthropic":  "claude-haiku-4-5",
				"local-vllm": "qwen2.5-7b-instruct",
			},
			"anthropic-only": {
				"anthropic": "claude-haiku-4-5",
			},
		},
		FallbackChains: map[string][]string{
			"fast": {"local-vllm", "openai", "anthropic"},
		},
	}
}

func TestResolve_ExplicitChainProducesSingletonTiersInOrder(t *testing.T) {
	r := New(testConfig())

	got, err := r.Resolve("fast")
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	want := []Tier{
		{Candidates: []Candidate{{ProviderName: "local-vllm", Model: "qwen2.5-7b-instruct", Weight: 1}}},
		{Candidates: []Candidate{{ProviderName: "openai", Model: "gpt-4o-mini", Weight: 7}}},
		{Candidates: []Candidate{{ProviderName: "anthropic", Model: "claude-haiku-4-5", Weight: 3}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve(\"fast\") = %+v, want %+v", got, want)
	}
}

func TestResolve_FallsBackToPriorityTiers(t *testing.T) {
	r := New(testConfig())

	got, err := r.Resolve("anthropic-only")
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	want := []Tier{
		{Candidates: []Candidate{{ProviderName: "anthropic", Model: "claude-haiku-4-5", Weight: 3}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve(\"anthropic-only\") = %+v, want %+v", got, want)
	}
}

func TestResolve_GroupsEqualPriorityIntoOneTier(t *testing.T) {
	cfg := testConfig()
	cfg.FallbackChains = nil // force priority-tier fallback for every alias
	r := New(cfg)

	got, err := r.Resolve("fast")
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(tiers) = %d, want 2 (priority 0 tier, priority 1 tier)", len(got))
	}
	if len(got[0].Candidates) != 1 || got[0].Candidates[0].ProviderName != "local-vllm" {
		t.Errorf("tier 0 = %+v, want just local-vllm (priority 0)", got[0])
	}
	if len(got[1].Candidates) != 2 {
		t.Fatalf("tier 1 = %+v, want 2 candidates (openai, anthropic share priority 1)", got[1])
	}
	names := map[string]bool{got[1].Candidates[0].ProviderName: true, got[1].Candidates[1].ProviderName: true}
	if !names["openai"] || !names["anthropic"] {
		t.Errorf("tier 1 candidates = %+v, want openai and anthropic", got[1].Candidates)
	}
}

func TestResolve_UnknownAlias(t *testing.T) {
	r := New(testConfig())

	_, err := r.Resolve("does-not-exist")
	if !errors.Is(err, ErrUnknownModel) {
		t.Errorf("Resolve() error = %v, want ErrUnknownModel", err)
	}
}

func TestResolve_ChainReferencesUnsupportedProvider(t *testing.T) {
	cfg := testConfig()
	cfg.FallbackChains["anthropic-only"] = []string{"openai", "anthropic"} // openai unsupported for this alias
	r := New(cfg)

	got, err := r.Resolve("anthropic-only")
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	want := []Tier{
		{Candidates: []Candidate{{ProviderName: "anthropic", Model: "claude-haiku-4-5", Weight: 3}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve(\"anthropic-only\") = %+v, want %+v (unsupported provider skipped)", got, want)
	}
}

func TestAliases_SortedAndComplete(t *testing.T) {
	r := New(testConfig())
	got := r.Aliases()
	want := []string{"anthropic-only", "fast"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Aliases() = %v, want %v", got, want)
	}
}
