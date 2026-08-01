package exact

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

func floatPtr(f float64) *float64 { return &f }

func TestCanonicalize_FieldOrderDoesNotMatter(t *testing.T) {
	// Both requests are logically identical; only Go struct field order
	// differs, which is exactly the ambiguity JSON field ordering models.
	a := &providers.CanonicalRequest{
		Model:    "fast",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Stop:     []string{"\n"},
	}
	b := &providers.CanonicalRequest{
		Stop:     []string{"\n"},
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Model:    "fast",
	}

	ca, err := Canonicalize(a)
	if err != nil {
		t.Fatalf("Canonicalize(a) unexpected error: %v", err)
	}
	cb, err := Canonicalize(b)
	if err != nil {
		t.Fatalf("Canonicalize(b) unexpected error: %v", err)
	}
	if string(ca) != string(cb) {
		t.Errorf("Canonicalize() differs for field-order-only variation:\n a=%s\n b=%s", ca, cb)
	}
}

func TestCanonicalize_NestedJSONKeyOrderDoesNotMatter(t *testing.T) {
	a := &providers.CanonicalRequest{
		Model:          "fast",
		Messages:       []providers.Message{{Role: "user", Content: "hi"}},
		ResponseFormat: json.RawMessage(`{"type":"json_object","strict":true}`),
	}
	b := &providers.CanonicalRequest{
		Model:          "fast",
		Messages:       []providers.Message{{Role: "user", Content: "hi"}},
		ResponseFormat: json.RawMessage(`{"strict":true,"type":"json_object"}`),
	}

	ca, err := Canonicalize(a)
	if err != nil {
		t.Fatalf("Canonicalize(a) unexpected error: %v", err)
	}
	cb, err := Canonicalize(b)
	if err != nil {
		t.Fatalf("Canonicalize(b) unexpected error: %v", err)
	}
	if string(ca) != string(cb) {
		t.Errorf("Canonicalize() differs for nested-key-order-only variation:\n a=%s\n b=%s", ca, cb)
	}
}

func TestCanonicalize_TemperatureChangesKey(t *testing.T) {
	base := &providers.CanonicalRequest{
		Model:    "fast",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	}
	zero := *base
	zero.Temperature = floatPtr(0)
	nonzero := *base
	nonzero.Temperature = floatPtr(0.7)

	cZero, err := Canonicalize(&zero)
	if err != nil {
		t.Fatalf("Canonicalize(zero) unexpected error: %v", err)
	}
	cNonzero, err := Canonicalize(&nonzero)
	if err != nil {
		t.Fatalf("Canonicalize(nonzero) unexpected error: %v", err)
	}
	if string(cZero) == string(cNonzero) {
		t.Error("Canonicalize() produced the same output for different temperatures")
	}
}

func TestCanonicalize_ExcludedFieldsDoNotAffectKey(t *testing.T) {
	a := &providers.CanonicalRequest{
		Model:    "fast",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Stream:   false,
		User:     "user-1",
	}
	b := &providers.CanonicalRequest{
		Model:    "fast",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Stream:   true, // excluded field, must not affect the key
		User:     "user-2",
	}

	ca, err := Canonicalize(a)
	if err != nil {
		t.Fatalf("Canonicalize(a) unexpected error: %v", err)
	}
	cb, err := Canonicalize(b)
	if err != nil {
		t.Fatalf("Canonicalize(b) unexpected error: %v", err)
	}
	if string(ca) != string(cb) {
		t.Errorf("Canonicalize() was affected by stream/user, which must be excluded:\n a=%s\n b=%s", ca, cb)
	}
}

func TestKey_TenantPrefixIsolatesSameCanonicalForm(t *testing.T) {
	canon := []byte(`{"model":"fast"}`)
	if Key("tenant-a", canon) == Key("tenant-b", canon) {
		t.Error("Key() produced the same key for two different tenants with identical requests")
	}
}

func TestKey_DifferentCanonicalFormsDifferentKeys(t *testing.T) {
	if Key("tenant-a", []byte(`{"model":"fast"}`)) == Key("tenant-a", []byte(`{"model":"cheap"}`)) {
		t.Error("Key() produced the same key for two different canonical forms")
	}
}

func TestEligible(t *testing.T) {
	tests := []struct {
		name                    string
		temperature             *float64
		cacheNonzeroTemperature bool
		want                    bool
	}{
		{"nil temperature, default policy", nil, false, false},
		{"zero temperature, default policy", floatPtr(0), false, true},
		{"nonzero temperature, default policy", floatPtr(0.7), false, false},
		{"nil temperature, nonzero allowed", nil, true, true},
		{"nonzero temperature, nonzero allowed", floatPtr(0.7), true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &providers.CanonicalRequest{Temperature: tt.temperature}
			if got := Eligible(req, tt.cacheNonzeroTemperature); got != tt.want {
				t.Errorf("Eligible() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Store integration tests, against a real Redis ---

func testStoreClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("RATELIMIT_TEST_REDIS_ADDR") // reuse the same env var as internal/ratelimit
	if addr == "" {
		t.Skip("RATELIMIT_TEST_REDIS_ADDR not set; skipping Redis integration test")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skipf("cannot reach Redis at %s: %v", addr, err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func TestStore_SetThenGet(t *testing.T) {
	client := testStoreClient(t)
	store := NewStore(client, time.Minute)
	key := Key("test-tenant-"+t.Name(), []byte(`{"model":"fast"}`))

	resp := &providers.CanonicalResponse{ID: "abc", Model: "fast", Choices: []providers.Choice{{Message: providers.Message{Role: "assistant", Content: "hi"}}}}
	if err := store.Set(context.Background(), key, resp); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	got, hit, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if !hit {
		t.Fatal("Get() hit = false, want true")
	}
	if got.ID != "abc" {
		t.Errorf("got.ID = %q, want %q", got.ID, "abc")
	}
}

func TestStore_GetMiss(t *testing.T) {
	client := testStoreClient(t)
	store := NewStore(client, time.Minute)

	_, hit, err := store.Get(context.Background(), Key("nonexistent-tenant-"+t.Name(), []byte(`{"model":"nope"}`)))
	if err != nil {
		t.Fatalf("Get() unexpected error: %v", err)
	}
	if hit {
		t.Error("Get() hit = true, want false")
	}
}

func TestStore_PurgeTenant(t *testing.T) {
	client := testStoreClient(t)
	store := NewStore(client, time.Minute)
	tenant := "purge-tenant-" + t.Name()

	for _, canon := range [][]byte{[]byte(`{"model":"a"}`), []byte(`{"model":"b"}`)} {
		key := Key(tenant, canon)
		if err := store.Set(context.Background(), key, &providers.CanonicalResponse{ID: "x"}); err != nil {
			t.Fatalf("Set() unexpected error: %v", err)
		}
	}
	otherKey := Key("other-tenant-"+t.Name(), []byte(`{"model":"a"}`))
	if err := store.Set(context.Background(), otherKey, &providers.CanonicalResponse{ID: "y"}); err != nil {
		t.Fatalf("Set() unexpected error: %v", err)
	}

	deleted, err := store.PurgeTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("PurgeTenant() unexpected error: %v", err)
	}
	if deleted != 2 {
		t.Errorf("PurgeTenant() deleted = %d, want 2", deleted)
	}

	if _, hit, _ := store.Get(context.Background(), Key(tenant, []byte(`{"model":"a"}`))); hit {
		t.Error("purged tenant's key still hits")
	}
	if _, hit, _ := store.Get(context.Background(), otherKey); !hit {
		t.Error("PurgeTenant() deleted a key belonging to a different tenant")
	}
}
