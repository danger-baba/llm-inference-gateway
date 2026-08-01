# 0011 — Exact cache: model field before routing, tenant is the org

## Status

Accepted (Phase 6).

## Context

Two things needed deciding that the README states at a level that leaves
a real implementation gap: what "model (post-aliasing)" means when the
cache is checked *before* routing happens, and what "tenant" means when
there are three levels (org, team, key) any of which could plausibly be
the cache's isolation boundary.

## Decisions

**The cache key uses the request's own `model` field (the client-facing
alias) as-is, not a post-routing, provider-specific model string.** The
request lifecycle is explicit that the cache is checked *before*
routing — "Tier-1 cache... Route... Call" — so at the point the cache key
is computed, no provider has been chosen yet, and with fallback in play,
the *same* alias can be served by different providers on different
requests. Keying on the eventually-chosen provider's model string would
mean the cache key can't even be computed until after the expensive work
(routing, calling a provider) the cache exists to avoid. The README's
"model (post-aliasing)" is read here as "the model field, normalized the
way aliasing already normalizes it" — i.e., the alias string itself,
consistently — not "the vendor-specific string whichever provider
happens to serve it."

**The cache's tenant boundary is the org, not the team or the virtual
key.** `cache:exact:{tenant}:{hash}` needs `tenant` to be something every
request already carries by the time the cache is checked — which the
resolved `auth.Identity` provides at all three levels (org, team, key).
Org was chosen because:

- It's the natural billing/isolation boundary this system already treats
  as authoritative elsewhere (`orgs.tpm_limit`, `orgs.monthly_budget_usd`).
- Scoping tighter (per-team or per-key) would fragment the cache
  needlessly: two teams in the same org asking the identical question
  get separate cache entries for no isolation benefit, since they're
  already the same trust boundary.
- The admin purge endpoint's `tenant` parameter is consequently an org
  ID, not a team or key ID — worth knowing before calling it.

## Consequences

If a future phase needs cache isolation *below* the org level (e.g. two
teams in one org must never see each other's cached completions even
though they're billed together), the key scheme needs revisiting — this
decision optimizes for hit rate within a trust boundary, not for
isolation stricter than that boundary already provides.
