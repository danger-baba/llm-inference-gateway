# 0014 — Ledger, metrics, and observability: design choices Phase 9 had to make itself

## Status

Accepted (Phase 9).

## Context

The README specifies Phase 9's *behavior* precisely (buffered ledger
writer, drop-and-count on a full buffer, the 11 named metrics, structured
logs, prompt bodies off by default) but leaves several concrete
mechanisms unspecified. This records what filled those gaps in.

## Decisions

**Prometheus collectors are package-level variables in `internal/metrics`,
registered against the default registry via `promauto`, not something
threaded through dependency injection.** Instrumentation is genuinely
cross-cutting here — call sites live in the chat handler, the streaming
handler, the retry engine, and the circuit breaker, none of which share a
natural common ancestor to inject a metrics object through. A global
registry is what `client_golang` itself is built around, and fighting
that with DI would mean either a much larger constructor-threading change
across four packages or a metrics interface duplicating what Prometheus
already provides. `/metrics` just calls `promhttp.Handler()`.

**`gateway_request_duration_seconds` and `gateway_proxy_overhead_seconds`
are unlabelled**, matching the README's table literally (no `{}` shown
for either) — unlike `gateway_requests_total` or `gateway_cache_hits_total`,
whose breakdowns are spelled out explicitly. **`gateway_provider_duration_seconds`
is the one deliberate exception**: it's labelled `{provider,model}` despite
the table showing no `{}` for it either, because the metric's own stated
Purpose — "isolates blame" — is meaningless without a per-provider
breakdown. Follow the letter where the table is explicit, follow the
evident intent where being literal would make the metric useless.

**`gateway_breaker_state`'s gauge is updated at the exact moment a
breaker transitions state** (`Breaker.setState`, the only place
`b.state` is ever assigned), not sampled on a timer. It's labelled only
by `provider`, matching the README's literal metric definition — if two
models share a provider name, whichever transitions last wins that
provider's gauge value. An accepted imprecision, not a bug: this
project's breakers are per-(provider, model), but the metric isn't, by
the README's own choice.

**`internal/providers/{openai,anthropic}`'s `Pricing` tables are a
point-in-time snapshot**, not fetched live from either vendor. They will
drift out of date; a real deployment needs to update them against the
vendor's published pricing page periodically. `mock.Provider` defaults to
`(0, 0)` (no real vendor cost) with an explicit `SetPricing` for tests
that need a nonzero cost to assert against — matching the existing
`FailMidStream`/`StreamDeltaLatency` pattern of small, additive test
knobs rather than a second fake type.

**`X-Request-Id` (and everywhere else a request ID is generated) is now
a real UUID (`uuid.NewString()`), not the arbitrary 32-hex-character
string it was through Phase 8** (`internal/server/requestid.go`). The
change is forced by `usage_ledger.request_id` being a UUID column: the
old generator's output isn't valid UUID syntax and would fail to insert.
Still just a unique random ID from the client's perspective — only the
formatting changed.

**`internal/ledger.Writer` never closes its input channel to shut down.**
`Record` is a plain buffered-channel send with a `select`/`default` drop;
`Close` cancels an internal context instead, and the background loop
drains whatever's already buffered before returning. Closing the channel
from `Close` while `Record` might still be sending concurrently is a
data race waiting to happen; this design has no such window, at the cost
of relying on a real ordering guarantee elsewhere — `Close` is only ever
called in `cmd/gateway/main.go` after `srv.Run(ctx)` has already
returned, i.e. after the HTTP server has stopped accepting new requests
and drained the ones in flight, so no `Record` call can start after
`Close` begins.

**The batch insert uses `pgx.CopyFrom`, not a multi-row `INSERT`.**
`usage_ledger` writes are append-only with no conflict handling and no
`RETURNING` value needed — exactly `CopyFrom`'s intended use, and
meaningfully faster than building a parameterized multi-row `INSERT`
string at the batch sizes a busy gateway will actually produce.

**`ledger.buffer_size`/`batch_size`/`flush_interval` are this project's
own config knobs, not in the README's example.** The README specifies the
ledger writer's behavior, not its tuning surface. Defaults used in
`deploy/config.yaml`: a 10,000-entry buffer, 200-row batches, 1s max
partial-batch wait.

**`GET /admin/usage`'s response shape is this project's own design.**
The README's entire spec for it is the API-surface-table row: "Ledger
aggregates by scope and window." There is no example JSON, no query
parameter list, anywhere in the README. The implementation:
`?scope=org|team|key&id=<uuid>&since=<RFC3339>&until=<RFC3339>` (`since`/
`until` default to the last 24 hours when omitted), returning request
count, prompt/completion tokens, tokens saved, cost, and a hit-count
breakdown by cache tier for that one scope value and window. `scope`
maps to a column name (`org_id`/`team_id`/`virtual_key_id`) through a
closed whitelist in `internal/ledger/aggregate.go` — never through a
caller-supplied string interpolated into SQL.

**Migration `0002` adds real monthly partitions for calendar year 2026**,
so the `usage_ledger_default` catch-all partition from migration `0001`
stays empty in practice rather than becoming an unbounded, unindexed
dumping ground — exactly what that migration's own comment asked Phase 9
to do. It is a static list of `CREATE TABLE ... PARTITION OF` statements,
which cannot keep up with real time forever; a production deployment
needs a scheduled job (`pg_partman`, or a small cron creating next
month's partition ahead of need) that this repo does not provide.

**Request/response body logging is a separate, narrowly-scoped path from
the general request-logging middleware, gated by its own check at every
call site.** `withRequestLogging` (method, path, status, duration,
request ID) never sees parsed message content and can't leak it by
construction. `maybeLogPromptBody`/`maybeLogResponseBody` in
`internal/server/chat.go` are the *only* two functions in this codebase
that can ever write prompt or completion text to a log, and both no-op
unless `observability.log_request_bodies` is explicitly `true` — kept as
named functions specifically so a future auditor can `grep` for every
site that could leak user data into logs, in one place, rather than
finding scattered inline `if logRequestBodies` checks.

## Consequences

- No Postgres was available in this development environment (the same
  Docker Desktop / native-install blockers noted since Phase 4), so
  `internal/ledger`'s buffering/batching/dropping logic is verified by
  comprehensive unit tests against a fake `Inserter`
  (`internal/ledger/ledger_test.go`), and `PGInserter`/`PGAggregator`'s
  actual SQL has not been exercised against a live database. This is a
  real, disclosed gap, not a silently-skipped one — consistent with how
  Phase 4's Postgres-dependent auth code was handled.
- Every metric referenced by every panel in
  `deploy/grafana/dashboards/gateway-overview.json` was confirmed to
  carry real, non-empty data during development, by driving a mix of
  traffic (success, cache hit, rate-limit rejection, a tripped breaker)
  through the real handler and reading the resulting `/metrics` output
  directly — the closest approximation available to "open Grafana" in an
  environment where Grafana itself can't run.
