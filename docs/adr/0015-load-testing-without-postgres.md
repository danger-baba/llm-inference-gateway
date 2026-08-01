# 0015 — Load testing without a live Postgres, and what running it for real actually found

## Status

Accepted (Phase 10).

## Context

Phase 10 requires driving real k6 traffic against a real, running gateway
and reporting measured numbers -- "do not invent or estimate any number."
This environment has no reachable Postgres (the same Docker Desktop /
native-install blocker noted since Phase 4), and `/v1/chat/completions`
requires a resolved auth identity, which normally comes from Postgres on
a cache miss. Getting real numbers required a real way to run the real
gateway anyway, plus several bugs in the load-test scripts themselves
were only found by actually running them -- both are recorded here.

## Decisions

**The real, unmodified `cmd/gateway` binary is used for load testing --
no separate harness binary.** Two properties of the existing code make
this possible without touching a single line of production auth/ledger
code:

1. `pgxpool.New` doesn't eagerly connect. A syntactically valid but
   unreachable Postgres DSN (`postgres://fake:fake@127.0.0.1:1/fakedb`)
   lets `cmd/gateway` start and serve normally; the pool is only ever
   touched by the ledger's async batch insert, which fails and drops the
   batch exactly per its designed failure mode (docs/adr/0014) -- it
   never blocks or fails a request.
2. `auth.CachingResolver` checks Redis before falling back to Postgres.
   `cmd/loadtest-seed-key` writes one entry directly into Redis at the
   exact cache key and JSON shape the resolver expects (`vk:{sha256}` ->
   `{Identity fields..., "revoked": false}`), computed the same way
   `internal/auth`'s own unexported `hashKey`/`cacheKey` do. Every
   auth lookup during a load test hits that cache entry and Postgres is
   never queried for auth either.

Both are legitimate uses of already-existing, intentional design
properties (lazy pool connection, cache-before-Postgres resolution) --
not a workaround bolted on top of them.

**`deploy/loadtest.config.yaml` is a new committed config** (single
always-healthy `mock1` provider, fixed 200ms latency, semantic cache
enabled) for `overhead.js`/`cache.js`. `failover.js` reuses
`deploy/config.yaml` as-is, since it already sets up exactly the
primary-always-429/secondary-succeeds scenario this needed.

**k6 itself needed the same workaround pattern as Docker and Postgres.**
`winget install GrafanaLabs.k6` downloaded successfully but hung
indefinitely at `msiexec` waiting on a UAC elevation prompt that can't be
answered in this non-interactive session -- confirmed via a `consent.exe`
process at a higher integrity level than the shell, un-killable from it.
Fetching k6's portable `.zip` release directly (same executable, no
installer) resolved it, matching the earlier resolution for Redis and
the MinGW-w64 toolchain: prefer a portable binary distribution over an
installer wherever the tool ships one.

**`gateway_request_duration_seconds`/`gateway_proxy_overhead_seconds`/
`gateway_provider_duration_seconds` moved from `prometheus.DefBuckets` to
a hand-picked bucket set (`internal/metrics/metrics.go`'s
`latencyBuckets`), because running `overhead.js` against the mock
provider's fixed 200ms latency exposed a real measurement problem, not a
code bug: `DefBuckets`' only boundaries near that latency are 0.1s and
0.25s, so the *entire* distribution (200-260ms) sat inside one bucket gap
with no boundary in between, and `histogram_quantile`-style linear
interpolation across that gap put the estimated median at ~175-225ms
depending on the exact run -- a 25ms+ error against k6's own precise
per-request client-side measurement (~202ms). The new buckets add
resolution specifically across 25ms-750ms, where LLM gateway overhead and
end-to-end latency actually live. This is a real, disclosed limitation of
Prometheus histograms generally (bucket_quantile trades exact precision
for bounded cardinality), not something achievable to eliminate entirely
-- see the Benchmarks section's own footnote about the gap that remains
even with the corrected buckets.

**Three real bugs in the load-test scripts themselves were found only by
running them, not by reading them:**

1. `cache.js`'s first version summed `gateway_requests_total` label
   combinations across the gateway's *entire process lifetime* to
   compute a hit-rate denominator. Run second (after `overhead.js`
   against the same still-running process), it inherited ~8,000 leftover
   requests and reported a nonsense 12.7% combined hit rate. Fixed by
   snapshotting the relevant counters in `setup()` and diffing against a
   post-run snapshot in `teardown()` -- correct regardless of whether the
   gateway was restarted fresh, not just when a user remembers to.
2. `cache.js`'s "fresh, should-never-repeat" traffic first used a shared
   sentence prefix with only a trailing number varying
   (`"one-off question ... 3-45-1785584000123"`). The semantic cache
   correctly recognized these as near-duplicates of each other,
   inflating the measured semantic hit rate toward the exact-cache hit
   rate. Fixed by drawing from a real spread of subjects and templates
   instead -- though even that still measures a real semantic hit rate
   higher than a naive intuition would predict, because all-MiniLM-L6-v2
   clusters short, structurally-similar sentences together regardless of
   subject, exactly the characteristic `docs/adr/0012`'s eval set already
   found ("poem" vs. "story" about the ocean scoring 0.887). This isn't a
   new finding contradicting the old one; it's the same one, confirmed
   again under load.
3. `failover.js` and `chaos.js` first used unthrottled executors
   (`constant-vus` with no per-iteration delay). Against
   `deploy/config.yaml`'s `rate_limit.default_tpm: 200000` -- sized for a
   manual-curl demo, not an unbounded loop across 10+ VUs -- this drove
   ~1,700 req/s and made the gateway's own token-bucket rejections, not
   provider failure, the dominant source of non-200 responses (89% of
   requests failed; the actual failover behavior was correct the whole
   time). Fixed by throttling `failover.js` to a realistic arrival rate.

**`chaos.js`/`chaos.sh` do not gate on a raw per-request success rate,**
even after the throttling fix above, for a different and more fundamental
reason: while the gateway process is actually down, a connection-refused
failure returns in microseconds, while a real success takes as long as
the provider's configured latency (~200ms here). A handful of VUs looping
tightly therefore fire a hugely disproportionate number of failed
attempts during even a sub-second outage, making the aggregate rate look
far worse than the real downtime duration. `chaos.sh` instead sends one
direct probe request immediately before the kill and one immediately
after the restart, and passes only if both return 200 -- the property
that actually matters ("did the service recover") isn't the same as "what
fraction of requests during the whole window happened to succeed."

**"Kill providers mid-run" (README, Load testing) is adapted to "kill and
restart the gateway process."** Providers in this codebase are in-process
(`internal/providers`), not separate services with their own lifecycle to
disrupt independently of the gateway. Killing and restarting the whole
process is the closest honest analog this architecture has.

## Consequences

- `internal/ledger`'s real Postgres SQL (`PGInserter.InsertBatch`,
  `PGAggregator.Aggregate`) still has not been exercised against a live
  database in this environment -- the load-test runs confirmed the
  request path tolerates Postgres being unreachable exactly as designed
  (batch inserts fail and drop, without affecting requests), which is a
  real, useful confirmation, but it is not the same as confirming the SQL
  itself is correct against a real `usage_ledger` table.
- "No request... double-charged" (README, Phase 10's chaos gate) is not
  verified in this environment: checking that would mean reading back
  ledger rows, which needs the same live Postgres this environment
  doesn't have. What is verified: the gateway recovers and serves real
  requests again immediately after a mid-run restart, and no request
  observed during any run hung indefinitely.
- The Benchmarks section's numbers reflect this specific machine, this
  specific mock-provider configuration, and (for the cache numbers) this
  specific synthetic traffic mix -- not a claim about any other
  environment or a real provider's actual latency.
