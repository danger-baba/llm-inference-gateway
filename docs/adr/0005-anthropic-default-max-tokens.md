# 0005 — Anthropic default `max_tokens`

## Status

Accepted (Phase 2). Likely revisited in Phase 5.

## Context

Unlike OpenAI's chat completions API, Anthropic's Messages API requires
`max_tokens` on every request — there's no server-side default. The
gateway's `CanonicalRequest.MaxTokens` is optional (`*int`, following
OpenAI's own contract), so a client can legitimately send a request with
no `max_tokens` at all. Something has to supply a number before the
request can reach Anthropic.

## Decision

`internal/providers/anthropic` defines a local constant,
`defaultMaxTokens = 1024`, used only when `req.MaxTokens` is nil.

## Alternatives considered

- **Reject the request with 400 if `max_tokens` is missing and the
  resolved provider is Anthropic.** Rejected: it would leak a
  provider-specific requirement into request validation that's supposed to
  be provider-agnostic, and it would make behavior depend on which
  provider the router happens to pick — the same client request could
  succeed against OpenAI and fail against Anthropic with no visible reason.
- **Source the default from `rate_limit.estimate_completion_tokens`**
  (the README's config field for estimating completion length, default
  512). This is arguably the *more correct* answer — it's already the
  gateway's stated notion of "how many tokens should we assume a completion
  costs when we don't know" — but that config section isn't wired into
  anything yet (Phase 5 builds the rate limiter that reads it), and
  reaching into an unrelated, unconsumed config section from a provider
  package felt like the wrong direction to couple things.

## Consequences

This constant and `rate_limit.estimate_completion_tokens` are, at the
moment, two independent numbers answering the same question ("how much
completion should we assume if the client didn't say"). Phase 5 should
either thread `estimate_completion_tokens` down into the provider layer to
replace this constant, or explicitly decide they're allowed to diverge and
say why.
