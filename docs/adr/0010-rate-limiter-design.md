# 0010 — Token bucket rate limiter design

## Status

Accepted (Phase 5).

## Context

This is the phase the build plan itself flags as highest-risk: atomicity
across three hierarchy levels in one Lua script, lazy refill without a
background ticker, and reserve-then-reconcile against actual usage. A few
choices needed deciding beyond what the README specifies directly.

## Decisions

**TPM limits ride along in `auth.Identity`, fetched once at auth
resolution time, not re-queried per rate-limit check.** `Store.Resolve`'s
SQL now joins `orgs` too and selects all three `tpm_limit` columns
alongside the identity; `CachingResolver`'s cache entry carries them
through the Redis cache as well. The alternative — a separate Postgres
query at rate-limit time — would mean paying a second database round
trip on every single request, which defeats the purpose of caching
identity resolution in the first place. The cost is that a TPM limit
changed in Postgres takes up to the identity cache's TTL (5 minutes) to
take effect, the same staleness window every other cached identity field
already has.

**`Ready()`/`Allow()` split, reused from the breaker (0006), extends
naturally here too**: the retry engine already established the pattern
of a non-mutating peek versus a mutating consume for a scarce resource.
The rate limiter doesn't need this split itself (a single Lua script call
either succeeds or doesn't, with no separate quota to peek at), but the
same *engineering instinct* — don't mutate shared state you're only
inspecting — is what makes the reserve script check all three scopes
before writing any of them.

**Reconciliation (`Adjust`) never blocks or fails a response.** Both the
full-refund-on-failure path and the refund-or-charge-the-difference path
on success run *after* the response has already been written to the
client. A `Redis` failure during reconciliation is swallowed (not
surfaced as a request error) for the same reason `RecordFailure` in the
breaker doesn't retry: the thing reconciliation is correcting for has
already happened. Losing a reconciliation write costs some rate-limit
accuracy, not correctness of the response the client received.

**`fail_open` isn't in the README's example config**, unlike most of this
build's additions to the schema, which is a deliberate readback of the
README's own text: "Redis unreachable must FAIL OPEN... Make this
behaviour config-switchable" (Failure modes table) is unambiguous that
the switch must exist, but the README's example config snippet is
explicitly illustrative, not exhaustive, and simply predates this phase.
Default is `false` (fail closed) when omitted, the more conservative
reading of an unset boolean; `deploy/config.yaml` sets it to `true`
explicitly, matching the README's own stated preference ("This build
chooses availability").

## Consequences

Every rate-limit integration test in `internal/ratelimit` runs against a
**real, running Redis** rather than a mock — Lua-script atomicity is
exactly the kind of property a hand-rolled fake would just assert by
construction rather than actually prove. `RATELIMIT_TEST_REDIS_ADDR` gates
these tests; CI now runs a `redis:7-alpine` service container so they
execute there too, not just wherever a developer happens to have Redis
installed locally.
