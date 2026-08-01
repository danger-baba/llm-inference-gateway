# 0009 — Token counting: cl100k for everyone, and a startup network dependency

## Status

Accepted (Phase 5).

## Context

The rate limiter needs a prompt-token count before it can reserve
anything, and needs it fast — this runs on every request, before any
provider is called. The README already accepts token estimation as
approximate (Known Limitations: "Completion-token estimation is a
heuristic"); this ADR extends that same acceptance to prompt counting.

## Decisions

**`cl100k_base` (OpenAI's tokenizer) is used to count tokens for every
provider, including Anthropic**, consistent with the toolchain decision
made back at kickoff. Anthropic's actual tokenizer differs, so this
undercounts or overcounts by some margin for Claude models. The
alternative — a real per-provider tokenizer for each vendor — is
meaningfully more accurate but is also more code, another dependency, and
another thing to keep in sync as providers change their tokenization.
Given the rate limiter already reconciles the reservation against actual
usage after the fact (this same phase's reserve-then-reconcile design),
an approximate prompt estimate is self-correcting within one request's
lifetime — it only ever affects how much headroom briefly disappears
between the reservation and the reconciliation, not the final charged
amount.

**Prompt cost formula**: `sum(tiktoken tokens per message content) + 4
tokens per message + 2 reply-priming tokens`, following OpenAI's own
documented community approximation for counting chat completion tokens.
This is a heuristic on top of a heuristic (approximate even for OpenAI
itself, once multi-part content, tool definitions, or function-call
messages enter the picture — none of which exist in this gateway yet).

## An operational consideration worth naming

`tiktoken-go`'s `GetEncoding("cl100k_base")` downloads its BPE rank file
over the network the first time it's called in a process, unless
`TIKTOKEN_CACHE_DIR` points at a pre-populated cache. That means:

- The gateway's startup now has an implicit network dependency it didn't
  have before this phase — a gateway meant to survive provider outages
  gracefully currently can't finish starting up without reaching
  `openaipublic.blob.core.windows.net` (or whatever tiktoken-go's default
  fetch target is) at least once.
- In a fully air-gapped or network-restricted deployment, this will fail
  at startup unless the cache directory is pre-seeded (e.g. baked into
  the container image at build time) and `TIKTOKEN_CACHE_DIR` is set.

This is a real gap between "the gateway shouldn't depend on the internet
to start" and what actually ships this phase. The fix — vendoring the
rank file into the image and setting `TIKTOKEN_CACHE_DIR` in
`deploy/Dockerfile` — is straightforward but out of this phase's scope;
noted here so it isn't forgotten.
