package auth

import (
	"context"
	"encoding/json"
	"time"
)

// identityResolver is satisfied by *Store; CachingResolver depends on
// this narrow interface instead so it's testable without Postgres.
type identityResolver interface {
	Resolve(ctx context.Context, hash []byte) (Identity, bool, error)
}

// cachedEntry is the JSON shape stored at vk:{sha256}: the full Identity
// (including the TPM limits the rate limiter needs, so it never has to
// re-query Postgres for them) plus whether the key is revoked. Revoked is
// cached too — resolving a revoked key hits the cache and gets ErrRevoked
// instead of round-tripping to Postgres on every request for it.
type cachedEntry struct {
	Identity
	Revoked bool `json:"revoked"`
}

// CachingResolver resolves a presented plaintext key to an Identity,
// checking cache first and falling back to Postgres on a miss, per the
// README's auth design.
type CachingResolver struct {
	cache    cache
	resolver identityResolver
	ttl      time.Duration
}

func NewCachingResolver(c cache, r identityResolver, ttl time.Duration) *CachingResolver {
	return &CachingResolver{cache: c, resolver: r, ttl: ttl}
}

func (c *CachingResolver) Resolve(ctx context.Context, plaintextKey string) (Identity, error) {
	hash := hashKey(plaintextKey)
	key := cacheKey(hash)

	if raw, err := c.cache.Get(ctx, key); err == nil {
		var entry cachedEntry
		if jsonErr := json.Unmarshal([]byte(raw), &entry); jsonErr == nil {
			if entry.Revoked {
				return Identity{}, ErrRevoked
			}
			return entry.Identity, nil
		}
		// Corrupt cache entry: fall through and resolve from Postgres.
	}

	id, revoked, err := c.resolver.Resolve(ctx, hash)
	if err != nil {
		return Identity{}, err // ErrNotFound, or a real failure — nothing to cache either way
	}

	entry := cachedEntry{Identity: id, Revoked: revoked}
	if payload, jsonErr := json.Marshal(entry); jsonErr == nil {
		// Best-effort: a cache write failure shouldn't fail the request
		// that triggered it, only cost the next request a Postgres round trip.
		_ = c.cache.Set(ctx, key, string(payload), c.ttl)
	}

	if revoked {
		return Identity{}, ErrRevoked
	}
	return id, nil
}

// Invalidate removes hash's cache entry immediately. Revocation calls
// this so a revoked key stops working right away instead of waiting out
// the cache TTL.
func (c *CachingResolver) Invalidate(ctx context.Context, hash []byte) error {
	return c.cache.Del(ctx, cacheKey(hash))
}
