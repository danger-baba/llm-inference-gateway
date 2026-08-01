// Command loadtest-seed-key seeds a real Redis instance with a virtual-
// key identity at exactly the cache key and JSON shape
// internal/auth.CachingResolver expects (vk:{sha256-hex} -> the Identity
// plus a revoked flag), so a load-testing run of the real gateway binary
// never needs a live Postgres for auth: the cache always hits.
//
// This is a benchmarking convenience only. It bypasses the real
// issuance flow (POST /admin/keys) entirely and must never be pointed at
// a production Redis -- see README, Load testing.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// seedEntry mirrors the unexported cachedEntry shape in internal/auth:
// the full Identity, plus "revoked", marshaled flat via embedding.
type seedEntry struct {
	OrgID         uuid.UUID
	TeamID        uuid.UUID
	KeyID         uuid.UUID
	AllowedModels []string
	OrgTPMLimit   int64
	TeamTPMLimit  int64
	KeyTPMLimit   *int64
	Revoked       bool `json:"revoked"`
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: loadtest-seed-key <redis-addr> <plaintext-key>")
		os.Exit(1)
	}
	redisAddr := os.Args[1]
	plaintext := os.Args[2]

	sum := sha256.Sum256([]byte(plaintext))
	cacheKey := "vk:" + hex.EncodeToString(sum[:])

	entry := seedEntry{
		OrgID:        uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		TeamID:       uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		KeyID:        uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		OrgTPMLimit:  1_000_000_000,
		TeamTPMLimit: 1_000_000_000,
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest-seed-key: marshal:", err)
		os.Exit(1)
	}

	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Set(ctx, cacheKey, string(payload), 24*time.Hour).Err(); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest-seed-key: redis SET:", err)
		os.Exit(1)
	}
	fmt.Printf("seeded %s (bearer token: %s)\n", cacheKey, plaintext)
}
