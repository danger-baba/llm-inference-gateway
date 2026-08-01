# 0004 — `/v1/models` scope, and what Phase 2 deliberately doesn't do yet

## Status

Accepted (Phase 2).

## Context

Two things needed a decision that the README states but doesn't fully
resolve at the Phase 2 level of detail: what `/v1/models` actually returns,
and how much of routing/retry Phase 2's handler should attempt given that
the breaker and retry engine don't exist until Phase 3.

## Decisions

**`/v1/models` returns `model_aliases` keys, not raw provider model
strings.** The README says it should return "the union of models across
configured providers," but clients never address a provider's raw model
name directly — they only ever send an alias (`"model": "fast"`) per the
config's `model_aliases` map. Listing raw provider strings would advertise
model names the client can't actually request. The returned list is the
sorted set of alias keys, each shaped as an OpenAI model object
(`{id, object: "model", owned_by: "gateway"}`).

**The chat completions handler only ever tries `router.Resolve`'s first
candidate.** `internal/router.Resolve` already returns the *full* ordered
candidate list (fallback chain if configured, else priority order) —
Phase 3's retry/fallback engine is meant to walk it. Phase 2's handler
takes `candidates[0]`, calls it once, and returns whatever happens
(success or the provider's raw error) directly to the client. No retry, no
walking to candidate 2. This is Phase 2's literal scope ("first proxy
hop"); building partial retry logic now, ahead of the circuit breaker it
needs to cooperate with, would be logic Phase 3 immediately has to tear out
and redo correctly.

**`stream: true` is rejected with `501` at the HTTP boundary, even though
every provider's `Stream` method is fully implemented.** Each provider
package (`mock`, `openai`, `anthropic`) implements `Stream` for real —
issuing the request, parsing SSE, forwarding `Delta`s — and is unit-tested
against a fake upstream. What doesn't exist yet is the gateway's own
SSE-writing HTTP handler (flush-per-event, mid-stream-failure handling,
cache-exclusion) that README section 7 specifies for Phase 8. Rejecting
`stream: true` explicitly and loudly, rather than silently buffering or
ignoring the flag, means a client asking for streaming today gets a clear
`501` instead of a response that looks complete but wasn't generated the
way they asked.

## Alternatives considered

- **`/v1/models` returns provider-qualified names** (e.g.
  `openai:gpt-4o-mini`). Rejected: clients would then need gateway-specific
  knowledge to pick a model, defeating the point of aliasing.
- **Silently ignore `stream: true`** and return a buffered response.
  Rejected outright: it's indistinguishable from a correct streaming
  response to a client that isn't checking, which is worse than a loud error.

## Consequences

Phase 3 changes the chat handler to walk the full candidate list with
retry/backoff and breaker checks between attempts, rather than replacing
`Resolve` itself. Phase 8 changes the `501` into a real SSE path built on
top of the `Stream` methods that already exist and are already tested.
