# 0006 — Circuit breaker, retry, and fallback design

## Status

Accepted (Phase 3).

## Context

The README specifies the breaker state machine, the two nested retry
loops, and a two-stage router (priority tiers, then weighted selection
within a tier) at a level of detail that still leaves several real
implementation choices open. This ADR records the ones a reviewer would
reasonably push back on.

## Decisions

**`HealthChecker` is a separate, optional interface, not a method on
`Provider`.** The README's `Provider` interface (Phase 2) is fixed and
doesn't include a health-check method, but the background prober needs
some way to ping a provider outside of real traffic. Adding a required
method to `Provider` would force every future implementation to have a
real lightweight endpoint, which isn't guaranteed. Instead,
`providers.HealthChecker` is a one-method interface that `mock`, `openai`,
and `anthropic` all happen to implement, and the prober type-asserts for
it, skipping providers that don't.

**An OPEN breaker's rejection is never routed through `Classify`.** Early
in this phase, treating a breaker's "no" as a normal failure and handing
it to `provider.Classify(err, 0)` sent it through
`providers.ClassifyByStatus(0)`, which falls into the `default: Terminal`
case — meaning an open breaker would abort the *entire* request instead of
falling over to the next candidate, exactly backwards from the point of
having a breaker. The fix: a breaker rejection is a distinct internal
sentinel (`errBreakerRejected`) that the engine recognizes and treats as
"reselect within the tier," never reaching classification and never
appearing in `X-Gateway-Attempts` (consistent with "no network call was
made").

**`Breaker.Ready()` (peek) and `Breaker.Allow()` (consume) are separate
methods.** Filtering candidates by health, then weighted-picking one, then
calling that one requires checking health *before* knowing which candidate
will actually be used — but a half-open breaker only has a small quota of
trial slots, and each call to the state-mutating check would consume one.
Checking eligibility for every candidate in a tier via the mutating call
would burn through the quota on candidates that are never actually
called. `Ready()` reads state without mutating it; `Allow()` is called
exactly once, immediately before the real network call, on whichever
candidate was actually chosen. The background prober uses the same pair,
so a prober probe and a real request compete fairly for the same slots
instead of the prober having its own separate budget.

**Retryable and Fallback failures both count against the breaker;
Terminal does not.** A Terminal failure (malformed request, content
filtered) is the caller's fault, not evidence the provider is unhealthy,
so it shouldn't move the breaker toward opening. Retryable and Fallback
are both "the provider didn't serve this request," regardless of exactly
whose fault it was, so both count.

**An explicit `fallback_chains` entry becomes one singleton `Tier` per
listed provider; the default (no explicit chain) path groups candidates by
ascending priority, multiple per tier.** This lets a single code path
(`pickFromTier`'s weighted selection) serve both cases: weighted-selecting
among one candidate always just returns that candidate, which is exactly
"follow the explicit order," with no special-casing needed.

**A tier whose remaining eligible candidates' weights sum to zero is
treated as fully drained, not uniformly sampled.** `weight: 0` means "keep
configured, send it no traffic" (README, canary/decommission). If every
survivor in a tier is weight-0, that's "this tier currently carries zero
traffic," not "shrug, pick anyone" — the engine advances to the next tier
instead.

## A sharp edge worth naming

Because `weight: 0` is a legitimate, meaningful config value, an
**unset** weight (Go's int zero value) is indistinguishable from an
intentional drain. A config with a single provider and no explicit
`weight:` set makes that provider completely undialable, and the
resulting error (`no healthy provider available`) is identical to what a
real breaker-tripped outage looks like. Every example in the README's own
config sets weight explicitly; this repo's `deploy/config.yaml` does too.
Nothing currently validates "at least one candidate per alias has nonzero
weight," because that requires cross-referencing `providers`, `weight`,
and `model_aliases` together, and Phase 3 didn't build that check. If this
gateway grows a UI or admin tooling, this is the first validation to add.

## Consequences

The retry engine now owns per-attempt timeouts (moved from the Phase 2
handler, since retries need per-attempt, not per-request, timeouts) and
constructs its own child context per `provider.Complete` call. Phase 8's
streaming path will need an analogous breaker/retry-aware wrapper around
`Provider.Stream`, since today only `Complete` goes through the engine.
