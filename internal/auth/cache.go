package auth

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrCacheMiss is returned by cache.Get when the key isn't present. It's
// distinct from a real cache error so callers can tell "fall back to
// Postgres" apart from "Redis itself is having a bad day."
var ErrCacheMiss = errors.New("auth: cache miss")

// cache abstracts the identity cache so CachingResolver is testable
// without a live Redis.
type cache interface {
	Get(ctx context.Context, key string) (string, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	Del(ctx context.Context, key string) error
}

type redisCache struct {
	client *redis.Client
}

func NewRedisCache(client *redis.Client) cache {
	return redisCache{client: client}
}

func (c redisCache) Get(ctx context.Context, key string) (string, error) {
	v, err := c.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrCacheMiss
	}
	return v, err
}

func (c redisCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}

func (c redisCache) Del(ctx context.Context, key string) error {
	return c.client.Del(ctx, key).Err()
}
