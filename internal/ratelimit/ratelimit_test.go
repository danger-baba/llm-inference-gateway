package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// These are integration tests against a real, running Redis instance —
// the whole point of this package is Lua-script atomicity, which cannot
// be faithfully verified against a fake. Set RATELIMIT_TEST_REDIS_ADDR to
// point at one; tests skip themselves if it's unset.

func testClient(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("RATELIMIT_TEST_REDIS_ADDR")
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

// randID gives each test its own tenant IDs so runs never collide, since
// bucket keys live in shared Redis state.
func randID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func testScopes(t *testing.T, orgCap, teamCap, keyCap int64) Scopes {
	t.Helper()
	return Scopes{
		OrgID: randID(t), TeamID: randID(t), KeyID: randID(t),
		OrgCapacity: orgCap, TeamCapacity: teamCap, KeyCapacity: keyCap,
	}
}

func TestReserve_SuccessDeductsAllThreeScopes(t *testing.T) {
	client := testClient(t)
	l := New(client, false)
	scopes := testScopes(t, 1000, 1000, 1000)

	d, err := l.Reserve(context.Background(), scopes, 100)
	if err != nil {
		t.Fatalf("Reserve() unexpected error: %v", err)
	}
	if !d.Allowed {
		t.Fatal("Reserve() Allowed = false, want true")
	}
	if d.Remaining != 900 {
		t.Errorf("Remaining = %d, want 900", d.Remaining)
	}
}

func TestReserve_MissingBucketIsFullCapacity(t *testing.T) {
	client := testClient(t)
	l := New(client, false)
	scopes := testScopes(t, 100, 100, 100)

	// First-ever reservation for a brand-new tenant: the bucket doesn't
	// exist yet, and must be treated as full, not empty.
	d, err := l.Reserve(context.Background(), scopes, 100)
	if err != nil {
		t.Fatalf("Reserve() unexpected error: %v", err)
	}
	if !d.Allowed {
		t.Fatal("Reserve() Allowed = false on a fresh tenant's first request, want true")
	}

	d2, err := l.Reserve(context.Background(), scopes, 1)
	if err != nil {
		t.Fatalf("Reserve() unexpected error: %v", err)
	}
	if d2.Allowed {
		t.Error("second Reserve() Allowed = true, want false (capacity just exhausted)")
	}
}

func TestReserve_RejectionDeductsNothing(t *testing.T) {
	client := testClient(t)
	l := New(client, false)
	scopes := testScopes(t, 1000, 1000, 10) // key capacity too small for the request

	d, err := l.Reserve(context.Background(), scopes, 500)
	if err != nil {
		t.Fatalf("Reserve() unexpected error: %v", err)
	}
	if d.Allowed {
		t.Fatal("Reserve() Allowed = true, want false")
	}
	if d.LimitingScope != "key" {
		t.Errorf("LimitingScope = %q, want %q", d.LimitingScope, "key")
	}

	// Nothing should have been written for ANY scope: the org and team
	// buckets had plenty of room, but a rejected reservation must not
	// touch them either.
	ctx := context.Background()
	for _, key := range []string{bucketKey("org", scopes.OrgID), bucketKey("team", scopes.TeamID), bucketKey("key", scopes.KeyID)} {
		exists, err := client.Exists(ctx, key).Result()
		if err != nil {
			t.Fatalf("EXISTS %s: %v", key, err)
		}
		if exists != 0 {
			t.Errorf("bucket %s exists after a rejected reservation, want no write at all", key)
		}
	}
}

func TestReserve_OuterScopeExhausted_InnerScopeUntouched(t *testing.T) {
	client := testClient(t)
	l := New(client, false)
	ctx := context.Background()
	scopes := testScopes(t, 50, 100000, 100000) // org is the tight one

	// Exhaust the org bucket directly, without going through Reserve —
	// a successful Reserve would deduct team/key too, contaminating the
	// very thing this test needs to prove was never touched.
	if err := client.HSet(ctx, bucketKey("org", scopes.OrgID), "tokens", 0, "last_refill", float64(time.Now().Unix())).Err(); err != nil {
		t.Fatalf("seed org bucket: %v", err)
	}

	// This request has plenty of room at team/key, but org has none left.
	d2, err := l.Reserve(ctx, scopes, 10)
	if err != nil {
		t.Fatalf("Reserve() unexpected error: %v", err)
	}
	if d2.Allowed {
		t.Fatal("Reserve() Allowed = true, want false (org is exhausted)")
	}
	if d2.LimitingScope != "org" {
		t.Errorf("LimitingScope = %q, want %q", d2.LimitingScope, "org")
	}

	// Team and key must still show their full, untouched capacity —
	// proving the inner scopes were never deducted just because the
	// outer one rejected the request.
	for _, key := range []string{bucketKey("team", scopes.TeamID), bucketKey("key", scopes.KeyID)} {
		exists, err := client.Exists(ctx, key).Result()
		if err != nil {
			t.Fatalf("EXISTS %s: %v", key, err)
		}
		if exists != 0 {
			t.Errorf("bucket %s exists after org rejected the request, want inner scopes untouched", key)
		}
	}
}

func TestReserve_ConcurrentHammering_NeverExceedsCapacity(t *testing.T) {
	client := testClient(t)
	l := New(client, false)

	const capacity = int64(500)
	const cost = int64(10)
	const goroutines = 100 // 100*cost = 1000 total demand, well over capacity

	scopes := testScopes(t, 1_000_000, 1_000_000, capacity) // key is the binding scope

	var wg sync.WaitGroup
	var admitted atomic.Int64
	var admittedTokens atomic.Int64

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := l.Reserve(context.Background(), scopes, cost)
			if err != nil {
				t.Errorf("Reserve() unexpected error: %v", err)
				return
			}
			if d.Allowed {
				admitted.Add(1)
				admittedTokens.Add(cost)
			}
		}()
	}
	wg.Wait()

	if got := admittedTokens.Load(); got > capacity {
		t.Fatalf("admitted %d tokens total, want <= capacity (%d)", got, capacity)
	}
	if admitted.Load() == 0 {
		t.Error("nothing was admitted at all, test isn't exercising the limiter")
	}
	if admitted.Load() == goroutines {
		t.Error("everything was admitted, test isn't exercising rejection")
	}
}

func TestAdjust_RefundIncreasesBalance(t *testing.T) {
	client := testClient(t)
	l := New(client, false)
	scopes := testScopes(t, 1000, 1000, 1000)

	d, err := l.Reserve(context.Background(), scopes, 200) // reserved as if estimate were 200
	if err != nil || !d.Allowed {
		t.Fatalf("Reserve() failed: allowed=%v err=%v", d.Allowed, err)
	}
	if d.Remaining != 800 {
		t.Fatalf("Remaining = %d, want 800", d.Remaining)
	}

	// Actual usage was only 120: refund the 80-token difference.
	if err := l.Adjust(context.Background(), scopes, 80); err != nil {
		t.Fatalf("Adjust() unexpected error: %v", err)
	}

	// Confirm the refund landed: a request for 880 should now succeed
	// against the key bucket (800 + 80 refunded).
	d2, err := l.Reserve(context.Background(), scopes, 880)
	if err != nil {
		t.Fatalf("Reserve() unexpected error: %v", err)
	}
	if !d2.Allowed {
		t.Error("Reserve() after refund Allowed = false, want true")
	}
}

func TestAdjust_ChargeDecreasesBalance(t *testing.T) {
	client := testClient(t)
	l := New(client, false)
	scopes := testScopes(t, 1000, 1000, 1000)

	d, err := l.Reserve(context.Background(), scopes, 200)
	if err != nil || !d.Allowed {
		t.Fatalf("Reserve() failed: allowed=%v err=%v", d.Allowed, err)
	}

	// Actual usage exceeded the estimate by 50 tokens: charge the difference.
	if err := l.Adjust(context.Background(), scopes, -50); err != nil {
		t.Fatalf("Adjust() unexpected error: %v", err)
	}

	// Remaining should now be 800 - 50 = 750; a request for 751 must fail.
	d2, err := l.Reserve(context.Background(), scopes, 751)
	if err != nil {
		t.Fatalf("Reserve() unexpected error: %v", err)
	}
	if d2.Allowed {
		t.Error("Reserve() for 751 after a 50-token extra charge Allowed = true, want false")
	}
}

func TestAdjust_RefundClampsAtCapacity(t *testing.T) {
	client := testClient(t)
	l := New(client, false)
	scopes := testScopes(t, 1000, 1000, 1000)

	// Refund more than was ever reserved; balance must clamp at capacity,
	// not exceed it.
	if err := l.Adjust(context.Background(), scopes, 5000); err != nil {
		t.Fatalf("Adjust() unexpected error: %v", err)
	}

	d, err := l.Reserve(context.Background(), scopes, 1001)
	if err != nil {
		t.Fatalf("Reserve() unexpected error: %v", err)
	}
	if d.Allowed {
		t.Error("Reserve() for capacity+1 after an over-sized refund Allowed = true, want false (clamp failed)")
	}
}

func TestReserve_LazyRefillOverTime(t *testing.T) {
	client := testClient(t)
	l := New(client, false)
	// 600 tokens/min = 10 tokens/sec, so ~100ms should refill ~1 token.
	scopes := testScopes(t, 600, 600, 600)

	d, err := l.Reserve(context.Background(), scopes, 600) // drain it completely
	if err != nil || !d.Allowed {
		t.Fatalf("Reserve() failed: allowed=%v err=%v", d.Allowed, err)
	}

	d2, err := l.Reserve(context.Background(), scopes, 1)
	if err != nil {
		t.Fatalf("Reserve() unexpected error: %v", err)
	}
	if d2.Allowed {
		t.Fatal("Reserve() immediately after draining Allowed = true, want false")
	}

	time.Sleep(200 * time.Millisecond) // ~2 tokens should have refilled

	d3, err := l.Reserve(context.Background(), scopes, 1)
	if err != nil {
		t.Fatalf("Reserve() unexpected error: %v", err)
	}
	if !d3.Allowed {
		t.Error("Reserve() after waiting for refill Allowed = false, want true")
	}
}

func TestReserve_FailOpenOnRedisDown(t *testing.T) {
	deadClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	defer deadClient.Close()

	l := New(deadClient, true)
	d, err := l.Reserve(context.Background(), testScopes(t, 100, 100, 100), 10)
	if err != nil {
		t.Fatalf("Reserve() with fail-open unexpected error: %v", err)
	}
	if !d.Allowed {
		t.Error("Reserve() with Redis down and failOpen=true Allowed = false, want true")
	}
}

func TestReserve_FailClosedOnRedisDown(t *testing.T) {
	deadClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	defer deadClient.Close()

	l := New(deadClient, false)
	_, err := l.Reserve(context.Background(), testScopes(t, 100, 100, 100), 10)
	if err == nil {
		t.Error("Reserve() with Redis down and failOpen=false expected an error, got nil")
	}
}
