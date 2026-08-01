package auth

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeCache struct {
	mu   sync.Mutex
	data map[string]string
}

func newFakeCache() *fakeCache {
	return &fakeCache{data: make(map[string]string)}
}

func (f *fakeCache) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.data[key]
	if !ok {
		return "", ErrCacheMiss
	}
	return v, nil
}

func (f *fakeCache) Set(_ context.Context, key, value string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = value
	return nil
}

func (f *fakeCache) Del(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.data, key)
	return nil
}

type fakeResolver struct {
	calls    int
	identity Identity
	revoked  bool
	err      error
}

func (f *fakeResolver) Resolve(_ context.Context, _ []byte) (Identity, bool, error) {
	f.calls++
	return f.identity, f.revoked, f.err
}

func TestCachingResolver_ResolvesFromStoreOnCacheMiss(t *testing.T) {
	id := Identity{OrgID: uuid.New(), TeamID: uuid.New(), KeyID: uuid.New(), AllowedModels: []string{"fast"}}
	store := &fakeResolver{identity: id}
	c := NewCachingResolver(newFakeCache(), store, time.Minute)

	got, err := c.Resolve(context.Background(), "sk-vk-plaintext")
	if err != nil {
		t.Fatalf("Resolve() unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, id) {
		t.Errorf("Resolve() = %+v, want %+v", got, id)
	}
	if store.calls != 1 {
		t.Errorf("store.calls = %d, want 1", store.calls)
	}
}

func TestCachingResolver_SecondCallHitsCacheNotStore(t *testing.T) {
	id := Identity{OrgID: uuid.New(), TeamID: uuid.New(), KeyID: uuid.New()}
	store := &fakeResolver{identity: id}
	c := NewCachingResolver(newFakeCache(), store, time.Minute)

	if _, err := c.Resolve(context.Background(), "sk-vk-plaintext"); err != nil {
		t.Fatalf("first Resolve() unexpected error: %v", err)
	}
	if _, err := c.Resolve(context.Background(), "sk-vk-plaintext"); err != nil {
		t.Fatalf("second Resolve() unexpected error: %v", err)
	}
	if store.calls != 1 {
		t.Errorf("store.calls = %d, want 1 (second call should hit cache)", store.calls)
	}
}

func TestCachingResolver_RevokedKeyReturnsErrRevoked(t *testing.T) {
	id := Identity{OrgID: uuid.New(), TeamID: uuid.New(), KeyID: uuid.New()}
	store := &fakeResolver{identity: id, revoked: true}
	c := NewCachingResolver(newFakeCache(), store, time.Minute)

	_, err := c.Resolve(context.Background(), "sk-vk-plaintext")
	if err != ErrRevoked {
		t.Errorf("Resolve() error = %v, want ErrRevoked", err)
	}
}

func TestCachingResolver_RevokedStatusIsCachedToo(t *testing.T) {
	id := Identity{OrgID: uuid.New(), TeamID: uuid.New(), KeyID: uuid.New()}
	store := &fakeResolver{identity: id, revoked: true}
	c := NewCachingResolver(newFakeCache(), store, time.Minute)

	_, _ = c.Resolve(context.Background(), "sk-vk-plaintext")
	_, err := c.Resolve(context.Background(), "sk-vk-plaintext")
	if err != ErrRevoked {
		t.Errorf("second Resolve() error = %v, want ErrRevoked (from cache)", err)
	}
	if store.calls != 1 {
		t.Errorf("store.calls = %d, want 1 (revoked status should be cached)", store.calls)
	}
}

func TestCachingResolver_NotFoundIsNotCached(t *testing.T) {
	store := &fakeResolver{err: ErrNotFound}
	fc := newFakeCache()
	c := NewCachingResolver(fc, store, time.Minute)

	_, err := c.Resolve(context.Background(), "sk-vk-plaintext")
	if err != ErrNotFound {
		t.Errorf("Resolve() error = %v, want ErrNotFound", err)
	}
	if len(fc.data) != 0 {
		t.Errorf("cache has %d entries, want 0 (a not-found lookup shouldn't be cached)", len(fc.data))
	}
}

func TestCachingResolver_InvalidateForcesFreshLookup(t *testing.T) {
	id := Identity{OrgID: uuid.New(), TeamID: uuid.New(), KeyID: uuid.New()}
	store := &fakeResolver{identity: id}
	c := NewCachingResolver(newFakeCache(), store, time.Minute)

	_, _ = c.Resolve(context.Background(), "sk-vk-plaintext")
	if err := c.Invalidate(context.Background(), hashKey("sk-vk-plaintext")); err != nil {
		t.Fatalf("Invalidate() unexpected error: %v", err)
	}

	store.revoked = true // simulate the revocation that presumably triggered the invalidation
	_, err := c.Resolve(context.Background(), "sk-vk-plaintext")
	if err != ErrRevoked {
		t.Errorf("Resolve() after Invalidate() = %v, want ErrRevoked (fresh lookup, not stale cache)", err)
	}
	if store.calls != 2 {
		t.Errorf("store.calls = %d, want 2 (invalidation must force a fresh lookup)", store.calls)
	}
}
