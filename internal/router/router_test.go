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
			{Name: "openai", Priority: 1},
			{Name: "anthropic", Priority: 1},
			{Name: "local-vllm", Priority: 0},
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

func TestResolve_UsesExplicitFallbackChain(t *testing.T) {
	r := New(testConfig())

	got, err := r.Resolve("fast")
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	want := []Candidate{
		{ProviderName: "local-vllm", Model: "qwen2.5-7b-instruct"},
		{ProviderName: "openai", Model: "gpt-4o-mini"},
		{ProviderName: "anthropic", Model: "claude-haiku-4-5"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve(\"fast\") = %+v, want %+v", got, want)
	}
}

func TestResolve_FallsBackToPriorityOrder(t *testing.T) {
	r := New(testConfig())

	got, err := r.Resolve("anthropic-only")
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	want := []Candidate{{ProviderName: "anthropic", Model: "claude-haiku-4-5"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve(\"anthropic-only\") = %+v, want %+v", got, want)
	}
}

func TestResolve_PriorityOrderIsAscendingAndStable(t *testing.T) {
	cfg := testConfig()
	cfg.FallbackChains = nil // force priority-order fallback for every alias
	r := New(cfg)

	got, err := r.Resolve("fast")
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	want := []Candidate{
		{ProviderName: "local-vllm", Model: "qwen2.5-7b-instruct"}, // priority 0
		{ProviderName: "openai", Model: "gpt-4o-mini"},             // priority 1, declared first
		{ProviderName: "anthropic", Model: "claude-haiku-4-5"},     // priority 1, declared second
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve(\"fast\") = %+v, want %+v", got, want)
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
	want := []Candidate{{ProviderName: "anthropic", Model: "claude-haiku-4-5"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve(\"anthropic-only\") = %+v, want %+v (unsupported provider skipped)", got, want)
	}
}
