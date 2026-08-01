package auth

import (
	"strings"
	"testing"
)

func TestGenerateKey_UniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		k, err := generateKey()
		if err != nil {
			t.Fatalf("generateKey() unexpected error: %v", err)
		}
		if !strings.HasPrefix(k, keyPrefixLabel) {
			t.Fatalf("generateKey() = %q, want prefix %q", k, keyPrefixLabel)
		}
		if seen[k] {
			t.Fatalf("generateKey() produced a duplicate: %q", k)
		}
		seen[k] = true
	}
}

func TestHashKey_DeterministicAndDistinct(t *testing.T) {
	h1 := hashKey("sk-vk-aaaa")
	h2 := hashKey("sk-vk-aaaa")
	h3 := hashKey("sk-vk-bbbb")

	if string(h1) != string(h2) {
		t.Error("hashKey() is not deterministic for the same input")
	}
	if string(h1) == string(h3) {
		t.Error("hashKey() produced the same hash for different inputs")
	}
}

func TestDisplayPrefix(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"sk-vk-abcdefgh12345", "sk-vk-ab"},
		{"short", "short"},
	}
	for _, tt := range tests {
		if got := displayPrefix(tt.in); got != tt.want {
			t.Errorf("displayPrefix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCacheKey_HexEncodedWithPrefix(t *testing.T) {
	got := cacheKey(hashKey("sk-vk-aaaa"))
	if !strings.HasPrefix(got, "vk:") {
		t.Errorf("cacheKey() = %q, want prefix %q", got, "vk:")
	}
	if len(got) != len("vk:")+64 { // sha256 -> 32 bytes -> 64 hex chars
		t.Errorf("cacheKey() = %q, unexpected length %d", got, len(got))
	}
}
