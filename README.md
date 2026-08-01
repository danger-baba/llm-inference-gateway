# LLM Inference Gateway

A production-grade reverse proxy for large language model APIs, written in Go.

One OpenAI-compatible endpoint in front of many providers, with health-aware routing,
automatic failover, two-tier caching, and token-aware rate limiting.

![Go](https://img.shields.io/badge/Go-1.24-00ADD8)
![License](https://img.shields.io/badge/license-MIT-green)
![Status](https://img.shields.io/badge/status-in%20progress-yellow)

---

## Build status

This repository is being built in numbered phases, each gated on passing tests
before moving to the next. Everything below this section describes the full
target design; treat it as the spec being built toward, not a claim that every
piece already exists.

| Phase | Scope | Status |
|---|---|---|
| 1 | Project skeleton, strict config validation, `/healthz` + `/readyz`, graceful shutdown, Docker Compose stack, CI | ✅ Done |
| 2 | Provider abstraction (mock/OpenAI/Anthropic), first proxy hop | ✅ Done |
| 3 | Circuit breaker, retry, fallback | ✅ Done |
| 4 | Auth, tenants, Postgres migrations | ✅ Done (admin API auth is a known gap — see Known Limitations) |
| 5 | Token-aware rate limiter | ✅ Done |
| 6 | Exact-match cache | ✅ Done |
| 7 | Semantic cache | ✅ Done |
| 8 | Streaming (SSE) | ⏳ Planned |
| 9 | Ledger, metrics, dashboards | ⏳ Planned |
| 10 | Load tests and benchmarks | ⏳ Planned |
| 11 | Publish polish | ⏳ Planned |

Architecture decisions made along the way, including deviations from this
README's original config example, are recorded in [`docs/adr/`](docs/adr/).

---

## Table of contents

- [The problem](#the-problem)
- [What this does](#what-this-does)
- [Benchmarks](#benchmarks)
- [Architecture](#architecture)
- [Request lifecycle](#request-lifecycle)
- [Low-level design](#low-level-design)
  - [1. Provider registry and router](#1-provider-registry-and-router)
  - [2. Circuit breaker and health checker](#2-circuit-breaker-and-health-checker)
  - [3. Retry and fallback engine](#3-retry-and-fallback-engine)
  - [4. Tier-1 cache: exact match](#4-tier-1-cache-exact-match)
  - [5. Tier-2 cache: semantic match](#5-tier-2-cache-semantic-match)
  - [6. Token-aware rate limiter](#6-token-aware-rate-limiter)
  - [7. Streaming (SSE) passthrough](#7-streaming-sse-passthrough)
  - [8. Request batching](#8-request-batching)
  - [9. Cost accounting ledger](#9-cost-accounting-ledger)
  - [10. Observability](#10-observability)
- [Data model](#data-model)
- [Redis key layout](#redis-key-layout)
- [Concurrency model](#concurrency-model)
- [API surface](#api-surface)
- [Configuration](#configuration)
- [Failure modes](#failure-modes)
- [Design decisions and trade-offs](#design-decisions-and-trade-offs)
- [Getting started](#getting-started)
- [Load testing](#load-testing)
- [Repository structure](#repository-structure)
- [Known limitations](#known-limitations)
- [Roadmap](#roadmap)

---

## The problem

Applications that call LLM APIs directly inherit three failure modes that traditional
API clients never had to think about:

1. **Providers throttle and go down.** Every major provider enforces tiered
   requests-per-minute and tokens-per-minute ceilings, and every one of them has logged
   multi-hour incidents. A `429` or `503` mid-workflow breaks the request unless
   something underneath absorbs it.

2. **Cost is unbounded and invisible.** Two calls to the same endpoint can differ by
   orders of magnitude in price. Request-count rate limiting cannot control spend,
   because the unit of cost is tokens, not requests.

3. **The same questions get asked repeatedly.** Identical and near-identical prompts are
   re-sent constantly, each one paying full latency and full token cost.

Solving these inside every application means every team reimplements retry logic, cache
invalidation, and budget tracking. This gateway centralises them in one hop.

## What this does

| Capability | Summary |
|---|---|
| **Unified API** | One OpenAI-compatible `/v1/chat/completions` endpoint fronting multiple providers. Swap providers with a config change, not a code change. |
| **Health-aware routing** | Per-provider circuit breakers. Unhealthy providers are removed from the pool and probed for recovery. |
| **Automatic failover** | Retries on the same provider for retryable status codes, then walks a configured fallback chain. The client sees a success and a header naming who served it. |
| **Two-tier caching** | Exact-match lookup in Redis, then semantic lookup over an HNSW vector index at a conservative cosine threshold. |
| **Token-aware limiting** | Token-bucket limiters keyed on tokens per minute, not requests per minute, with hierarchical org → team → key budgets. |
| **Streaming** | Server-sent-events passthrough with correct handling of mid-stream provider failure. |
| **Cost attribution** | Append-only usage ledger recording tokens and cost per request, per tenant. |
| **Observability** | Prometheus metrics for cache hit rate, tokens saved, proxy overhead, breaker state, and per-provider error rates. |

---

## Benchmarks

> **These tables are unpopulated on purpose.** Fill them from your own `k6` runs using the
> method below — see [Load testing](#load-testing). Numbers copied from somewhere else are
> worse than no numbers.

**Environment:** _record CPU, RAM, Go version, Redis version, whether providers are mocked._

### Proxy overhead

Latency added by the gateway itself, measured against a mock provider with fixed response
time so provider latency cancels out.

| Metric | Value |
|---|---|
| Sustained throughput (RPS) | _tbd_ |
| p50 added latency | _tbd_ |
| p95 added latency | _tbd_ |
| p99 added latency | _tbd_ |
| Memory at steady state | _tbd_ |

### Cache effectiveness

| Metric | Value |
|---|---|
| Exact-match hit rate | _tbd_ |
| Semantic hit rate (threshold 0.89) | _tbd_ |
| Combined hit rate | _tbd_ |
| Tokens avoided | _tbd_ |
| Mean latency, cache hit vs miss | _tbd_ |

### Failover

| Scenario | Result |
|---|---|
| Primary returns 100% `429` | _tbd_ % requests still succeed |
| Primary killed mid-run | _tbd_ ms to route around |
| All providers down | Fails fast with `503`, no hang |

---

## Architecture

```
                    ┌──────────────────────────────────────────┐
   client ─────────▶│  HTTP server  (net/http, TLS terminated) │
   OpenAI-compatible└──────────────────┬───────────────────────┘
   request                             │
                                       ▼
                        ┌──────────────────────────────┐
                        │  Auth: virtual key → tenant  │
                        └──────────────┬───────────────┘
                                       ▼
                        ┌──────────────────────────────┐
                        │  Rate limiter (token bucket) │──── reject 429
                        │  org → team → key hierarchy  │
                        └──────────────┬───────────────┘
                                       ▼
                        ┌──────────────────────────────┐
                        │  Tier-1 cache: Redis exact   │──── HIT ──▶ return
                        └──────────────┬───────────────┘
                                       ▼ MISS
                        ┌──────────────────────────────┐
                        │  Tier-2 cache: HNSW semantic │──── HIT ──▶ return
                        └──────────────┬───────────────┘
                                       ▼ MISS
                        ┌──────────────────────────────┐
                        │  Router: pick healthy        │
                        │  provider by weight/priority │
                        └──────────────┬───────────────┘
                                       ▼
                     ┌─────────────────────────────────────┐
                     │  Retry + fallback engine            │
                     │  ┌───────────┐   ┌───────────┐      │
                     │  │ Provider A│──▶│ Provider B│─ ... │
                     │  └───────────┘   └───────────┘      │
                     │  circuit breaker per provider       │
                     └─────────────────┬───────────────────┘
                                       ▼
                     ┌─────────────────────────────────────┐
                     │  Response path                      │
                     │  • write both cache tiers           │
                     │  • append usage ledger (async)      │
                     │  • emit Prometheus metrics           │
                     └─────────────────┬───────────────────┘
                                       ▼
                                    client

   Side stores:  Redis (cache, buckets)   PostgreSQL (tenants, keys, ledger)
```

---

## Request lifecycle

The complete path of a non-streaming chat completion:

1. **Accept.** `net/http` handler reads the body with a size cap. A `context.Context`
   carrying the request deadline is created here and threaded through every downstream call.
2. **Authenticate.** The `Authorization: Bearer sk-vk-...` virtual key is hashed and looked
   up in Redis (falling back to PostgreSQL on miss). Resolves to `tenant_id`, `team_id`,
   `org_id`, allowed models, and budget references.
3. **Normalise.** The request is canonicalised into a stable form for cache keying —
   see [Tier-1 cache](#4-tier-1-cache-exact-match) for exactly which fields participate.
4. **Estimate tokens.** Prompt tokens are counted locally with a BPE tokenizer. Completion
   tokens are estimated from `max_tokens`, or a configured default. The sum is the
   *reservation* the limiter will hold.
5. **Rate limit.** Token buckets are decremented atomically at three levels. Any level
   without capacity returns `429` with `Retry-After` and `X-RateLimit-*` headers. Nothing
   downstream runs.
6. **Tier-1 cache.** `GET cache:exact:<hash>`. On hit: return immediately, release the
   token reservation, emit a `cache=exact` header.
7. **Tier-2 cache.** Embed the normalised prompt, query the HNSW index for the nearest
   neighbour. If cosine similarity ≥ threshold **and** all non-semantic parameters match,
   return the stored response.
8. **Route.** The router asks each candidate provider's circuit breaker whether it will
   accept traffic, then selects among the healthy set by priority, then weight.
9. **Call.** The request is translated to the provider's dialect and issued with a
   per-attempt timeout derived from the remaining request deadline.
10. **Retry / fall back.** Retryable failures retry the same provider with jittered
    backoff up to a cap, then advance to the next provider in the chain. See
    [Retry and fallback engine](#3-retry-and-fallback-engine).
11. **Respond.** The provider response is translated back to OpenAI shape,
    `X-Gateway-Provider` and `X-Gateway-Cache` headers are attached, and it is written to
    the client.
12. **Settle, asynchronously.** After the response is flushed: reconcile the token
    reservation against actual usage, write both cache tiers, append the ledger row, and
    record metrics. None of this blocks the client.

---

## Low-level design

### 1. Provider registry and router

Each provider is a config-declared entry implementing a narrow interface:

```go
type Provider interface {
    Name() string
    // Translate, issue, and translate back. Must honour ctx cancellation.
    Complete(ctx context.Context, req *CanonicalRequest) (*CanonicalResponse, error)
    // Streaming variant emits deltas on the channel until closed or ctx done.
    Stream(ctx context.Context, req *CanonicalRequest, out chan<- Delta) error
    // Classifies a provider error into the gateway's retry taxonomy.
    Classify(err error, status int) FailureClass
    Pricing(model string) (inPerMTok, outPerMTok float64)
}
```

Routing is two-stage and deliberately boring:

- **Priority tiers.** Providers carry an integer priority. The router only considers the
  lowest-numbered tier that has at least one healthy member. This makes "prefer the cheap
  self-hosted model, fall back to the expensive hosted one" expressible in config.
- **Weighted choice within a tier.** Selection uses a cumulative-weight scan over healthy
  members. With weights `{A:7, B:3}` traffic splits 70/30. Weight 0 drains a provider
  without removing it, which is how canary rollouts and safe decommissioning work.

Model aliasing lives here too: a client asking for `gpt-4o-mini` may be served by whatever
each provider calls its equivalent, resolved through a config map so application code never
hardcodes a vendor model string.

### 2. Circuit breaker and health checker

One breaker per `(provider, model)` pair, because a provider can be healthy for one model
and rate-limited on another.

Three states:

```
                 failures ≥ threshold
                 within window
      ┌────────┐ ─────────────────────▶ ┌────────┐
      │ CLOSED │                        │  OPEN  │
      └────────┘ ◀───────────────────── └────────┘
           ▲      successes ≥ required       │
           │                                 │ cooldown elapsed
           │      ┌───────────┐              │
           └───── │ HALF_OPEN │ ◀────────────┘
                  └───────────┘
                        │ any failure
                        └──────────────▶ back to OPEN
```

- **CLOSED** — traffic flows. A sliding window counts failures; `error_rate ≥ threshold`
  over a minimum request volume trips the breaker. Volume gating prevents a single failure
  on a quiet provider from opening it.
- **OPEN** — all requests short-circuit instantly with `ErrBreakerOpen`, so the router skips
  this provider without paying a network timeout. This is the whole point: an open breaker
  converts a 30-second hang into a nanosecond decision.
- **HALF_OPEN** — after cooldown, a small quota of probe requests is admitted. Enough
  consecutive successes close the breaker; any failure reopens it and doubles the cooldown
  up to a ceiling.

The sliding window is a ring of per-second buckets holding success and failure counts,
which keeps the state O(window) and avoids the "one bad minute poisons an hour" behaviour
of a naive cumulative counter. Breaker state is per-instance in memory; see
[Known limitations](#known-limitations).

An independent background prober calls each provider's cheapest health endpoint on an
interval, so a provider that recovers while receiving no traffic is still discovered.

### 3. Retry and fallback engine

Failures are classified before anything is decided:

| Class | Examples | Action |
|---|---|---|
| `Retryable` | `429`, `500`, `502`, `503`, `504`, connection reset, dial timeout | Retry same provider, then fall back |
| `Fallback` | model not found, context length exceeded, provider auth failure | Skip retry, go straight to next provider |
| `Terminal` | malformed request, content filtered, client cancelled | Return to client immediately |

Two nested loops: an inner retry loop per provider and an outer walk along the fallback
chain. Both are bounded, and both check the request deadline before sleeping — there is no
point waiting 4 seconds when 800 ms of budget remains.

Backoff uses **full jitter**, which matters more than the base delay:

```go
// attempt is 0-indexed
backoff := min(cap, base*(1<<attempt))
sleep := time.Duration(rand.Int63n(int64(backoff)))
```

Deterministic backoff synchronises every client that failed at the same instant into a
thundering herd that retries in lockstep. Full jitter spreads them across the interval.
The `Retry-After` header, when a provider sends one, overrides the computed value.

Every attempt is recorded on the request span so the response can report the full path:
`X-Gateway-Attempts: openai:429, openai:429, anthropic:200`.

### 4. Tier-1 cache: exact match

**Key construction.** The cache key must be stable across semantically identical requests
and must never collide across different ones. The request is canonicalised first:

- JSON object keys sorted recursively, so field order cannot change the key
- Fields that **do** participate: `model` (post-aliasing), the full `messages` array,
  `temperature`, `top_p`, `max_tokens`, `stop`, `seed`, `response_format`, `tools`
- Fields that **do not** participate: `stream`, `user`, `metadata`, request ID, trace
  headers — none of these change the generated content
- Tenant identity is prefixed into the key, so caches are isolated per tenant by default

```
cache:exact:{tenant}:{sha256(canonical_json)}  →  serialised CanonicalResponse
```

**Correctness rule.** Requests with `temperature > 0` are *not* cached by default. A
non-zero temperature means the caller asked for variety, and silently returning the same
completion violates that contract. This is configurable per route, but the default is the
conservative one.

**Invalidation.** TTL-based, per model, because there is no meaningful write event to
invalidate on. `DEL` by tenant prefix is exposed as an admin endpoint for manual purges.

### 5. Tier-2 cache: semantic match

The expensive tier, consulted only on a Tier-1 miss.

- **Embedding.** The concatenated user-turn text is embedded with a small local
  sentence-transformer model. Local matters: a network call to embed would add most of the
  latency the cache is meant to save.
- **Index.** HNSW, with vectors held in memory and persisted to disk for restart. Tuning
  parameters are `M` (graph degree), `efConstruction` (build-time candidate breadth), and
  `efSearch` (query-time breadth). `efSearch` is the recall/latency dial and is the one to
  tune under load.
- **Threshold.** Cosine similarity **≥ 0.89** to count as a hit. This is deliberately
  conservative, and it is a measured number, not a guess: a 20-pair eval set
  (`docs/eval/semantic_cache_eval.md`, reproduced by
  `internal/embedding/eval_test.go`) showed that all-MiniLM-L6-v2 puts true paraphrases of a
  short chat question in the 0.75–0.93 cosine range, while the closest topically-adjacent
  but genuinely different pair we tried (a poem vs. a story about the ocean) scored 0.887.
  0.89 sits above that one, at the cost of missing some looser paraphrases — the intended
  trade, since a wrong cache hit is a far worse failure than a cache miss. See docs/adr/0012
  before changing it.
- **Guard rail.** Vector similarity alone is not sufficient. A candidate hit is rejected
  unless the non-semantic parameters also match exactly — same model, same tool
  definitions, same `response_format`. A 0.97 cosine match against a request that asked for
  JSON when this one asked for prose is not a hit.
- **Eviction.** LRU over the vector store with a configured memory ceiling, plus the same
  TTL as Tier-1.

### 6. Token-aware rate limiter

Counting requests is the wrong unit. One request can cost a thousandth of another, so RPM
limits either throttle cheap traffic needlessly or let expensive traffic bankrupt the
budget. This limiter meters **tokens**.

**Algorithm.** Token bucket, one bucket per scope per window:

```
capacity     = tokens_per_minute
refill_rate  = capacity / 60  tokens per second
```

Buckets are lazily refilled — no background ticker. On each check, elapsed time since
`last_refill` is multiplied by the rate and added, clamped to capacity. This makes the
limiter O(1) per request and stateless between calls apart from two Redis fields.

**Atomicity.** Read-modify-write across three scopes cannot be done with separate
commands without a race. The whole operation is a single Lua script evaluated server-side
in Redis, which is atomic by construction:

```lua
-- KEYS: org, team, key buckets.  ARGV: now_ms, cost, capacities, rates
-- 1. refill all three buckets
-- 2. if any has balance < cost  →  return {0, retry_after_ms, limiting_scope}
-- 3. otherwise deduct cost from all three, return {1, remaining}
```

Checking all three before deducting any is what prevents partial deduction when an inner
scope has room but an outer one does not.

**Reservation and reconciliation.** Completion length is unknown before the call, so the
limiter reserves `prompt_tokens + estimated_completion_tokens` up front. After the response
arrives, the provider's authoritative `usage` block is compared to the estimate and the
difference is refunded or additionally charged. Over-reserving briefly is correct; letting
an unbounded generation through is not.

**Hierarchy.** `org → team → virtual_key`, evaluated outermost-first. Response headers name
which scope rejected, because "you are rate limited" without saying by whom is unactionable.

### 7. Streaming (SSE) passthrough

Streaming is where naive gateways get it wrong, so this is documented precisely.

- The handler asserts `http.Flusher`, sets `Content-Type: text/event-stream`,
  `Cache-Control: no-cache`, `X-Accel-Buffering: no`, and flushes after every event. Without
  the flush, a buffering layer defeats streaming entirely.
- Deltas are read from the provider and forwarded as they arrive. Tokens are counted
  incrementally on the way past, because a stream may end without a `usage` block.
- **Mid-stream failure is not transparently recoverable.** Once the first byte of content
  has been flushed to the client, the gateway cannot silently retry on another provider —
  doing so would splice two different completions into one response and produce incoherent
  output. The honest behaviours are: fail over *only* if nothing has been flushed yet; and
  once flushed, terminate the stream with an explicit error event so the client knows the
  response is truncated rather than complete. Pretending otherwise is a correctness bug,
  not a feature.
- Client disconnect cancels the request context, which propagates to the provider call and
  stops paying for tokens nobody will read.
- Cache writes happen only on a complete stream. Partial streams are never cached.

### 8. Request batching

For providers exposing a batch endpoint, or for a self-hosted backend where batching raises
GPU utilisation, requests are grouped by a micro-batching collector:

- Requests eligible for batching (same model, same non-semantic params, non-streaming) go
  into a per-key queue
- The queue flushes when **either** `max_batch_size` requests accumulate **or**
  `max_wait` elapses, whichever comes first
- Each waiter holds a channel; the collector fans results back out by index

`max_wait` is the latency/throughput dial and should be set from the p50 you can afford to
add. It is off by default, because for hosted providers batching adds latency without
adding throughput.

### 9. Cost accounting ledger

An append-only table, never updated in place, because billing data that can be mutated
cannot be audited.

Per request the gateway records: tenant scopes, provider and model actually used, prompt and
completion tokens, computed cost from the provider's price table, cache tier that served it,
attempt count, and end-to-end latency. Cache hits are written with `cost = 0` and a
`tokens_saved` value equal to what the call would have cost — that field is what makes the
cache's value measurable rather than assumed.

Writes go through a buffered channel to a batch inserter so the hot path never waits on
PostgreSQL. The buffer is drained on shutdown.

### 10. Observability

Prometheus metrics, all labelled by `provider`, `model`, and `tenant` where cardinality
permits:

| Metric | Type | Purpose |
|---|---|---|
| `gateway_requests_total{status,provider,cache}` | counter | Traffic and outcome mix |
| `gateway_request_duration_seconds` | histogram | End-to-end latency |
| `gateway_proxy_overhead_seconds` | histogram | Gateway cost excluding provider time |
| `gateway_provider_duration_seconds` | histogram | Upstream latency, isolates blame |
| `gateway_cache_hits_total{tier}` | counter | Hit rate by tier |
| `gateway_tokens_total{direction}` | counter | Token throughput |
| `gateway_tokens_saved_total` | counter | Cache value in the unit that costs money |
| `gateway_cost_usd_total` | counter | Spend, for burn-rate alerting |
| `gateway_breaker_state{provider}` | gauge | 0 closed, 1 half-open, 2 open |
| `gateway_retries_total{class}` | counter | Retry pressure by failure class |
| `gateway_ratelimit_rejections_total{scope}` | counter | Which scope is binding |

Structured JSON logs carry a request ID propagated to every log line and returned in
`X-Request-Id`. Prompt and completion bodies are **not** logged by default; a config flag
enables it for debugging with an explicit warning, because prompts contain user data.

---

## Data model

PostgreSQL, for anything that must outlive a Redis flush.

```sql
CREATE TABLE orgs (
    id          UUID PRIMARY KEY,
    name        TEXT NOT NULL,
    tpm_limit   BIGINT NOT NULL,
    monthly_budget_usd NUMERIC(12,4),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE teams (
    id          UUID PRIMARY KEY,
    org_id      UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    tpm_limit   BIGINT NOT NULL,
    monthly_budget_usd NUMERIC(12,4),
    UNIQUE (org_id, name)
);

-- Virtual keys are what clients present. The raw key is never stored.
CREATE TABLE virtual_keys (
    id            UUID PRIMARY KEY,
    team_id       UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    key_hash      BYTEA NOT NULL UNIQUE,       -- sha256 of the presented key
    key_prefix    TEXT  NOT NULL,              -- first 8 chars, for display only
    label         TEXT,
    allowed_models TEXT[] NOT NULL DEFAULT '{}',
    tpm_limit     BIGINT,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON virtual_keys (key_hash) WHERE revoked_at IS NULL;

-- Append-only. No UPDATE, no DELETE.
CREATE TABLE usage_ledger (
    id                BIGSERIAL PRIMARY KEY,
    request_id        UUID NOT NULL,
    org_id            UUID NOT NULL,
    team_id           UUID NOT NULL,
    virtual_key_id    UUID NOT NULL,
    provider          TEXT NOT NULL,
    model             TEXT NOT NULL,
    prompt_tokens     INT  NOT NULL,
    completion_tokens INT  NOT NULL,
    tokens_saved      INT  NOT NULL DEFAULT 0,
    cost_usd          NUMERIC(12,6) NOT NULL,
    cache_tier        TEXT NOT NULL,           -- none | exact | semantic
    attempts          SMALLINT NOT NULL,
    status_code       SMALLINT NOT NULL,
    latency_ms        INT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
) PARTITION BY RANGE (created_at);

-- Monthly partitions keep the hot partition small and make retention a DROP.
CREATE INDEX ON usage_ledger (org_id, created_at DESC);
CREATE INDEX ON usage_ledger (virtual_key_id, created_at DESC);
```

Design notes worth stating out loud:

- **Keys are stored hashed**, never in plaintext, so a database leak does not hand over
  working credentials. `key_prefix` exists only so a UI can show `sk-vk-a1b2…` in a list.
- **Revocation is a timestamp, not a delete**, and the lookup index is partial on
  `revoked_at IS NULL` — revoked keys stop working without losing their ledger history.
- **The ledger is range-partitioned by month.** Retention becomes `DROP TABLE` instead of a
  long-running `DELETE`, and queries for the current month never scan history.
- **Money is `NUMERIC`, never `FLOAT`.** Binary floating point cannot represent decimal
  fractions exactly and accumulated error in a billing column is indefensible.

## Redis key layout

```
vk:{sha256}                      → JSON tenant resolution      TTL 5m
cache:exact:{tenant}:{hash}      → serialised response         TTL per model
bucket:org:{org_id}              → HASH {tokens, last_refill}  TTL 2× window
bucket:team:{team_id}            → HASH {tokens, last_refill}  TTL 2× window
bucket:key:{key_id}              → HASH {tokens, last_refill}  TTL 2× window
budget:month:{scope}:{yyyymm}    → accumulated spend           TTL 40d
```

Buckets carry a TTL of twice the window so idle tenants cost nothing, and a missing key is
correctly interpreted as a full bucket.

## Concurrency model

- **One goroutine per request**, courtesy of `net/http`. No custom accept loop.
- **`context.Context` is the only cancellation mechanism.** It is created at accept with the
  request deadline and passed to every function that can block. Client disconnect,
  timeout, and shutdown all collapse into the same signal.
- **Bounded worker pools, not unbounded `go` statements.** Embedding, ledger writes, and
  batch flushing each run on a fixed pool fed by a buffered channel. An unbounded spawn
  under load is how a proxy turns a provider outage into its own OOM.
- **Provider HTTP clients are shared and reused**, with tuned `MaxIdleConnsPerHost` and
  `IdleConnTimeout`. A fresh client per request means a fresh TLS handshake per request.
- **Breaker state is guarded by `sync/atomic` and a `RWMutex`** on the ring buffer, since it
  is read on every request and written only on state transitions.
- **Graceful shutdown**: stop accepting, drain in-flight requests up to a timeout, flush the
  ledger buffer, persist the HNSW index, close pools.

## API surface

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/chat/completions` | OpenAI-compatible completions, streaming and not |
| `POST` | `/v1/embeddings` | OpenAI-compatible embeddings |
| `GET` | `/v1/models` | Union of models across configured providers |
| `GET` | `/healthz` | Liveness; process is up |
| `GET` | `/readyz` | Readiness; Redis and Postgres reachable |
| `GET` | `/metrics` | Prometheus scrape endpoint |
| `GET` | `/admin/usage` | Ledger aggregates by scope and window |
| `POST` | `/admin/keys` | Issue a virtual key; returns plaintext once |
| `DELETE` | `/admin/keys/{id}` | Revoke a virtual key |
| `POST` | `/admin/cache/purge` | Purge cache by tenant prefix |

Response headers on every proxied call:

```
X-Request-Id:        uuid
X-Gateway-Provider:  which provider actually served it
X-Gateway-Cache:     none | exact | semantic
X-Gateway-Attempts:  openai:429, anthropic:200
X-RateLimit-Scope:   org | team | key      (on 429)
Retry-After:         seconds                (on 429)
```

## Configuration

```yaml
server:
  addr: ":8080"
  read_timeout: 30s
  request_timeout: 120s
  max_body_bytes: 1048576

providers:
  - name: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
    priority: 1
    weight: 7
    timeout: 60s
  - name: anthropic
    type: anthropic
    base_url: https://api.anthropic.com/v1
    api_key_env: ANTHROPIC_API_KEY
    priority: 1
    weight: 3
    timeout: 60s
  - name: local-vllm
    type: openai            # vLLM is OpenAI-compatible
    base_url: http://vllm:8000/v1
    priority: 0             # preferred: cheapest
    weight: 1

model_aliases:
  fast:
    openai: gpt-4o-mini
    anthropic: claude-haiku-4-5
    local-vllm: qwen2.5-7b-instruct

fallback_chains:
  fast: [local-vllm, openai, anthropic]

breaker:
  error_rate_threshold: 0.5
  min_requests: 20
  window: 30s
  cooldown: 10s
  cooldown_max: 5m
  half_open_probes: 3

retry:
  max_attempts_per_provider: 2
  base_backoff: 200ms
  max_backoff: 5s

cache:
  exact:
    enabled: true
    ttl: 1h
    cache_nonzero_temperature: false
  semantic:
    enabled: true
    threshold: 0.89 # measured, see docs/eval/semantic_cache_eval.md and docs/adr/0012
    embedding_model: all-MiniLM-L6-v2
    hnsw: { m: 16, ef_construction: 200, ef_search: 64 }
    max_vectors: 500000
    ttl: 1h

rate_limit:
  default_tpm: 200000
  estimate_completion_tokens: 512

batching:
  enabled: false
  max_batch_size: 16
  max_wait: 20ms

observability:
  log_level: info
  log_request_bodies: false     # contains user data; enable only to debug
```

## Failure modes

Documenting what breaks and what the gateway does about it is the point of the exercise.

| Failure | Detection | Behaviour |
|---|---|---|
| Provider returns `429` | Status classification | Honour `Retry-After`, retry, then fall back |
| Provider returns `5xx` | Status classification | Jittered retry, then fall back |
| Provider hangs | Per-attempt timeout | Cancel via context, fall back |
| Provider degraded, not down | Breaker error-rate window | Breaker opens, traffic shifts, prober watches for recovery |
| All providers open | Router finds no healthy member | Fail fast `503`, no queueing, no hang |
| Redis down | Ping failure | **Fail open**: skip cache, allow request. Availability over cost control |
| PostgreSQL down | Ping failure | Serve from Redis key cache; buffer ledger writes; reject admin writes |
| Ledger buffer full | Channel at capacity | Drop with a counter increment. Losing a billing row beats stalling the proxy |
| Client disconnects mid-stream | Context cancelled | Cancel upstream, stop paying for tokens |
| Provider fails mid-stream | Read error after first flush | Emit terminal error event; do **not** splice a second completion |
| Embedding model unavailable | Init failure | Semantic tier disabled, exact tier continues |
| Clock skew across instances | — | Buckets use Redis server time, not instance time |

The **Redis fail-open** decision deserves emphasis because it is a genuine trade-off, not an
oversight: if Redis is unreachable the gateway cannot enforce rate limits. Failing closed
would take the whole platform down whenever the cache blinks. Failing open risks a spend
overrun during the outage. This build chooses availability and alerts loudly. A
cost-sensitive deployment should invert it, and the config exposes the switch.

## Design decisions and trade-offs

**Go rather than Python.** The gateway sits in the hot path of every LLM call, so its own
tail latency is pure overhead. Python-based gateways are known to degrade badly under
concurrency — published comparisons show p99 latency in the tens of seconds at a few hundred
RPS where Go-based alternatives stay in the low single digits on the same hardware. Go's
goroutine-per-request model with a shared connection pool fits a proxy workload almost
exactly, and `context` gives uniform cancellation for free.

**Semantic caching at 0.89, not 0.85.** The failure mode of an over-permissive semantic
cache is returning a confident answer to a question the user did not ask, which is
undetectable to them and corrosive to trust. The failure mode of an over-strict one is a
slightly larger bill. Those are not symmetric, so the threshold starts conservative — and,
per the build plan's own rule, was only moved off an arbitrary starting point (0.95) after
measuring against a real eval set. See docs/adr/0012 and docs/eval/semantic_cache_eval.md.

**Tokens, not requests, as the limiting unit.** Request-count limits cannot bound spend when
per-request cost varies by three orders of magnitude. The cost of this choice is having to
estimate completion length before the call and reconcile afterwards, which is real
complexity accepted deliberately.

**Reserve-then-reconcile instead of charge-on-completion.** Charging only after the response
lets unlimited concurrent requests through a limiter that has not yet learned they are
expensive. Reserving up front is occasionally too pessimistic and always safe.

**No transparent mid-stream failover.** It would be easy to implement and quietly wrong.
Splicing two completions produces text that reads as a single coherent answer while being
neither. Truncating with an explicit error is less impressive and more honest.

**Append-only ledger with `NUMERIC` money.** Mutable billing rows cannot be audited, and
floating-point currency accumulates error. Both are cheap to get right at design time and
expensive to retrofit.

**In-memory breaker state.** Per-instance rather than shared in Redis, accepting that N
instances each need to observe failures independently. Shared breaker state would add a
Redis round trip to the hot path to save a small amount of duplicated learning. Revisit if
the fleet grows large — noted in [Known limitations](#known-limitations).

## Getting started

**Requirements:** Go 1.24+, Docker, Docker Compose.

```bash
git clone https://github.com/danger-baba/llm-inference-gateway
cd llm-inference-gateway

cp .env.example .env
# set OPENAI_API_KEY / ANTHROPIC_API_KEY, or leave empty to use the mock provider

docker compose up -d          # gateway, redis, postgres, prometheus, grafana
make migrate                  # apply schema
make seed                     # create a demo org/team and print a virtual key
```

Send a request:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $VK" \
  -H "Content-Type: application/json" \
  -d '{"model":"fast","messages":[{"role":"user","content":"Explain HNSW briefly."}]}' -i
```

Send it twice and compare `X-Gateway-Cache` — the second should report `exact`. Reword the
question slightly and it should report `semantic`.

Grafana is on `:3000` with the gateway dashboard preloaded.

```bash
make test          # unit tests
make test-race     # race detector
make bench         # Go benchmarks
make chaos         # kill providers mid-run, assert no lost requests
```

## Load testing

Reproducible harness in `loadtest/`. Run against the **mock provider** so measurements
isolate gateway overhead from provider latency.

```bash
make mock-provider                     # fixed 200ms responses
k6 run loadtest/overhead.js            # ramp to find sustainable RPS
k6 run loadtest/cache.js               # 70% repeat traffic, measures hit rate
k6 run loadtest/failover.js            # primary returns 429, assert success rate
```

`loadtest/overhead.js` ramps arrival rate and records `gateway_proxy_overhead_seconds`
alongside k6's own latency, so the gateway's contribution is separable from total time.

**Then fill in [Benchmarks](#benchmarks) with what you measured**, and record the hardware
you measured it on. A benchmark without an environment is not a benchmark.

## Repository structure

```
cmd/gateway/            entrypoint, flag parsing, wiring
internal/
  server/               http handlers, middleware, SSE writer
  auth/                 virtual key resolution, hashing
  router/               priority + weighted selection, model aliasing
  breaker/              circuit breaker, sliding window, prober
  retry/                classification, backoff, fallback walk
  cache/
    exact/              canonicalisation, Redis get/set
    semantic/           embedding, HNSW index, threshold guard
  ratelimit/            token bucket, Lua scripts, reconciliation
  providers/
    openai/  anthropic/  mock/
  ledger/               buffered batch writer
  tokenizer/            BPE counting, completion estimation
  metrics/              Prometheus collectors
  config/               yaml load + validation
migrations/             numbered SQL migrations
loadtest/               k6 scripts
deploy/                 docker-compose, Grafana dashboards, Prometheus rules
docs/                   architecture.md, adr/
```

## Known limitations

Stated plainly, because pretending a system has no edges is the least credible thing a
README can do.

1. **Breaker state is per-instance.** With N replicas, each learns provider health
   independently, so the first failures after a provider degrades are paid N times.
2. **Rate limits are eventually consistent under Redis partition.** The Lua script is atomic
   against a single Redis, but a partitioned cluster can briefly over-admit.
3. **Semantic cache recall is only as good as the embedding model, and this is measured, not
   assumed.** A 20-pair eval (`docs/eval/semantic_cache_eval.md`) found all-MiniLM-L6-v2 puts
   real chat-question paraphrases at 0.75–0.93 cosine similarity — meaning the threshold
   (0.89) catches the closer paraphrases and misses looser ones by design, trading recall
   for the precision the README argues for. See `docs/adr/0012`.
4. **Completion-token estimation is a heuristic.** Absent `max_tokens`, the configured
   default can over-reserve and needlessly throttle a tenant.
5. **No prompt-injection or PII filtering yet.** Real gateways in this space treat both as
   first-class. See roadmap.
6. **Batching only helps self-hosted backends.** Against hosted providers it adds latency
   for no throughput gain, hence disabled by default.
7. **Single-region.** No cross-region replication of cache or ledger.
8. **`/admin/*` has no authentication.** `POST /admin/keys` and `DELETE /admin/keys/{id}`
   are reachable by anyone who can reach the gateway's HTTP port — there is no admin
   credential yet. Fine for local development; not fine for any shared or
   internet-reachable deployment. See `docs/adr/0008`.
9. **`weight: 0` and an unset weight look identical.** A provider that's the only member of
   its tier with no explicit `weight:` set becomes completely undialable (Go's int zero
   value collides with the deliberate "drain this provider" signal), and the resulting error
   is indistinguishable from a real outage. Always set `weight` explicitly. See `docs/adr/0006`.
10. **Prompt token counts use OpenAI's tokenizer for every provider, including Anthropic.**
    Self-correcting within a request via reserve-then-reconcile, but not exact. See `docs/adr/0009`.
11. **The tokenizer needs network access on first startup** to fetch its BPE rank file,
    unless `TIKTOKEN_CACHE_DIR` points at a pre-populated cache. A gateway meant to survive
    provider outages currently can't finish starting up fully air-gapped. See `docs/adr/0009`.
12. **A TPM limit changed in Postgres takes up to 5 minutes to take effect**, the same
    staleness window as every other identity field cached at `vk:{sha256}`.
13. **The semantic cache's tenant isolation is a post-filter over one shared HNSW graph, not
    one graph per tenant.** Correct, but every search does some wasted work scanning
    candidates that belong to other tenants before discarding them. Fine at moderate tenant
    counts; revisit if tenant cardinality grows large. See `docs/adr/0012`.
14. **The semantic cache's memory can run up to ~2x `max_vectors`.** Evicting the oldest
    entry doesn't delete it from the HNSW graph (a real upstream bug in `coder/hnsw` — see
    `docs/adr/0012`), so orphaned vectors accumulate until a periodic full rebuild discards
    them.
15. **The semantic cache's assets (ONNX Runtime shared library, MiniLM model, vocab) must be
    present on disk before startup**, fetched at Docker build time or via
    `make download-embedding-model` for local runs — there is no first-run network fetch at
    startup the way the tokenizer has. If they're missing, the gateway logs a warning and
    runs with Tier-2 disabled rather than failing to start.

## Roadmap

- [ ] Admin API authentication (static token, checked via constant-time comparison)
- [ ] Shared breaker state via Redis pub/sub, with in-memory fast path
- [ ] PII redaction and prompt-injection classification before egress
- [ ] Per-tenant hard budget enforcement with pre-emptive alerting at 80%
- [ ] Response-shape validation and automatic retry on malformed structured output
- [ ] OpenTelemetry traces spanning gateway → provider
- [ ] Admin UI for keys, budgets, and usage
- [ ] Kubernetes manifests and an HPA keyed on queue depth rather than CPU

---

## License

MIT. See [LICENSE](LICENSE).

## Author

**Vishal Kumar Jha** — Backend Engineer
[LinkedIn](https://www.linkedin.com/in/vishal-jha-bb2ba1228) ·
[GitHub](https://github.com/danger-baba)

Built to explore the serving-layer problems behind production LLM traffic: routing,
failover, caching, and cost control. If you want the reasoning behind a specific decision,
the [design decisions](#design-decisions-and-trade-offs) section and `docs/adr/` cover the
ones that were close calls.
