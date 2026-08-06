# 0017 — Per-request cache TTL hint

## Status

Accepted.

## Context

Both cache tiers (Tier-1 exact, Tier-2 semantic) have always used one fixed
TTL per tier, set once in config and applied to every entry that tier
ever stores. That's a reasonable default, but it treats every cached
answer as equally perishable. In practice a client often knows more about
a specific request than the operator's one global number can express: an
answer to "what's the capital of France" is good for a year, an answer to
"what's trending right now" is stale in an hour, and an answer built from
data the client considers sensitive-for-this-call-only shouldn't be
cached at all.

The request that motivated this: could a client influence how long its
own answer gets remembered? The design has to answer three questions:
where does the client express that, does the operator get any say, and
how does "don't cache this" fit in without complicating the cache
stores' own contract.

## Decisions

**A new optional request field, not a header.** `cache_ttl` joins the
JSON body as a plain Go duration string (e.g. `"24h"`), on
`providers.CanonicalRequest`. It's marked gateway-only and stripped by
construction: no provider's translate-request function reads it, so it
can never leak into what's actually sent to OpenAI or Anthropic. A header
was the other option on the table, but the body already carries every
other per-request tuning knob this API has (`temperature`, `max_tokens`,
`stop`), and a plain duration string reuses `time.ParseDuration` directly
instead of inventing a second wire format alongside the YAML-side
`Duration` type config already uses.

**The operator sets a ceiling; the client can only ask for less.**
`cache.max_client_ttl` (`config.CacheConfig.MaxClientTTL`) is the longest
TTL a client's hint can ever produce. A request asking for longer than
the ceiling is silently capped to it -- never rejected, since asking for
"as long as possible" and getting the operator's actual maximum is a
reasonable outcome, not an error. This mirrors HTTP's own `Cache-Control`
precedent: a client can shorten an entry's life or opt out, but it never
gets to force a cache to hold something longer than whoever runs that
cache allows.

**Zero (the default) disables the whole feature, silently.** Same
feature-flag-by-zero-value pattern as `cache.semantic.enabled` elsewhere
in this config. When `max_client_ttl` is unset, a client's `cache_ttl`
hint is accepted but has no effect at all -- not a validation error. A
request built for a deployment that has opted into this feature should
not break against one that hasn't; the field is a hint, and an ignored
hint is the correct behavior for a hint.

**A non-positive value means "don't cache this response at all,"
handled by the caller, not by `Store.Set`.** `resolveCacheTTLOverride` in
`internal/server/chat.go` turns the raw string into an
`*time.Duration` -- `nil` for "no override, use the tier's own default,"
a pointer to a positive duration for an explicit request, capped to the
ceiling. `storeInCaches` checks for a non-positive override *before*
calling either tier's `Set` and skips both entirely when it sees one.
`exact.Store.Set` and `semantic.Store.Set` themselves only ever receive
`nil` or a positive override -- their contract stays "store this now, for
this TTL," never "maybe don't." Keeping the opt-out check at the call site
means neither cache store had to grow a "should I even store this" branch
of its own.

**A malformed hint is a 400, but only when the feature is enabled.**
Parsing happens once, early in `handleChatCompletions`, right after the
existing model/messages validation and before anything expensive
(routing, rate-limit reservation, cache lookup) runs. If
`max_client_ttl` is zero, an unparseable `cache_ttl` is never even looked
at -- the whole field is inert. If it's nonzero and the client sent
something `time.ParseDuration` can't read, that's a client error worth
surfacing immediately, the same as an unknown model or an empty
`messages` array.

**A new response header reports what was actually applied.**
`X-Gateway-Cache-TTL` is set (non-streaming and streaming both) whenever
the resolved override is non-nil -- `"0"` for "not cached at all,"
otherwise the applied `time.Duration`'s own `String()` form (e.g.
`"1h0m0s"` when a client's `"48h"` request got capped to a 1-hour
ceiling). The header is absent whenever there was nothing to report:
the client sent no hint, or the feature is disabled. This follows the
same transparency pattern as `X-Gateway-Provider` and `X-Gateway-Cache`
elsewhere in this API -- the client can always see what the gateway
actually decided, not just what it asked for.

## Consequences

- `exact.Store.Set` and `semantic.Store.Set` both gained a
  `ttlOverride *time.Duration` parameter. Every existing caller (both
  stores' own tests, `storeInCaches`) was updated to pass `nil` for "use
  the configured default," which is the same behavior as before this
  change for any request that never sets `cache_ttl`.
- A deployment that never sets `cache.max_client_ttl` sees no behavior
  change at all: the field is accepted, parsed only far enough to know
  the feature is off, and otherwise ignored.
- A client that wants a specific answer never cached (e.g. it's about to
  ask something time-sensitive under a model alias that's normally safe
  to cache) can now say so per-request, without the operator needing to
  disable caching for that tenant or model entirely.
