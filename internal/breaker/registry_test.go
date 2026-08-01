package breaker

import "testing"

func TestRegistry_SameKeyReturnsSameBreaker(t *testing.T) {
	r := NewRegistry(testConfig())

	a := r.Get("openai", "gpt-4o-mini")
	b := r.Get("openai", "gpt-4o-mini")
	if a != b {
		t.Error("Get() returned different instances for the same (provider, model)")
	}
}

func TestRegistry_DifferentModelIsDifferentBreaker(t *testing.T) {
	r := NewRegistry(testConfig())

	a := r.Get("openai", "gpt-4o-mini")
	b := r.Get("openai", "gpt-4o")
	if a == b {
		t.Error("Get() returned the same instance for two different models on the same provider")
	}
}

func TestRegistry_SnapshotIncludesAllCreated(t *testing.T) {
	r := NewRegistry(testConfig())
	r.Get("openai", "gpt-4o-mini")
	r.Get("anthropic", "claude-haiku-4-5")

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot() = %d breakers, want 2", len(snap))
	}
}
