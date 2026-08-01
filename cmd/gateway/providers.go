package main

import (
	"fmt"
	"os"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/config"
	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/providers/anthropic"
	"github.com/danger-baba/llm-inference-gateway/internal/providers/mock"
	"github.com/danger-baba/llm-inference-gateway/internal/providers/openai"
)

// buildProviders instantiates one Provider per configured entry and
// collects each one's configured attempt timeout, keyed by provider name.
// The default case is unreachable in practice since config.Load already
// rejects any type outside openai/anthropic/mock; it's kept as a guard in
// case that invariant ever drifts.
func buildProviders(cfgs []config.ProviderConfig) (map[string]providers.Provider, map[string]time.Duration, error) {
	built := make(map[string]providers.Provider, len(cfgs))
	timeouts := make(map[string]time.Duration, len(cfgs))

	for _, c := range cfgs {
		switch c.Type {
		case "mock":
			built[c.Name] = mock.New(c.Name, c.Latency.Std(), c.FailureRate)
		case "openai":
			built[c.Name] = openai.New(c.Name, c.BaseURL, os.Getenv(c.APIKeyEnv))
		case "anthropic":
			built[c.Name] = anthropic.New(c.Name, c.BaseURL, os.Getenv(c.APIKeyEnv))
		default:
			return nil, nil, fmt.Errorf("main: provider %q: unknown type %q", c.Name, c.Type)
		}
		timeouts[c.Name] = c.Timeout.Std()
	}
	return built, timeouts, nil
}
