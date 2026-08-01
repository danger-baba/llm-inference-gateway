# 0013 — Streaming: deferred header commit, callback relay, and what "flushed" means

## Status

Accepted (Phase 8).

## Context

The README is unusually prescriptive about streaming's failure contract:
fail over only if nothing has been flushed yet; once flushed, terminate
with an explicit error event rather than splicing a second provider's
output into the response. Implementing that precisely required a few
decisions the README doesn't spell out at the code level.

## Decisions

**The HTTP status and SSE headers are not sent until the first delta is
about to be forwarded, not at the top of the handler.** A non-streaming
request that fails before calling a provider gets a normal JSON error
with the right status code. A naive streaming implementation loses that:
if `Content-Type: text/event-stream` and `200` go out immediately, then
*every* candidate across every tier failing (total outage, nothing ever
sent) has no way to become anything but a broken empty stream, because
the status code already committed. Instead, `handleStreamingChatCompletion`
(`internal/server/stream.go`) defers `w.WriteHeader` and the SSE-specific
headers to the first successful call inside `onDelta` — which is exactly
the same moment `retry.Engine.ExecuteStream` considers content "flushed."
The two concepts collapsing into one moment is not a coincidence: it's
what makes "fail over only if nothing has been flushed yet" and "an
all-providers-failed request still gets a normal error response" the same
guarantee instead of two guarantees that could drift apart.

**`ExecuteStream` takes a callback (`func(providerName string, d
providers.Delta) error`), not a channel.** The provider-facing side of
`Provider.Stream` is necessarily a channel (that's the existing interface
from Phase 2/3), but the engine-to-handler boundary doesn't need to be:
there's exactly one active caller, delivery is synchronous, and
backpressure is just "the callback hasn't returned yet." A callback also
gives the handler a natural way to signal a write failure straight back
into the retry loop (`internal/retry/engine.go`'s `relayStream` stops and
returns the callback's error immediately) without inventing a second
channel or a `select` the provider side never asked for. `Delta` still
flows the same way it always did from `Provider.Stream` outward, just
handed off differently past that first boundary.

**Every provider `Delta.Content != ""` marks the stream flushed only
after the callback returns successfully, not before.** If the client
write itself fails (broken pipe, disconnected socket) on what would have
been the first byte, `flushed` stays false for retry-decision purposes,
even though `headersSent` (a `chat.go`/`stream.go`-local variable) may
already be `true`. This is a real, narrow inconsistency: in that specific
race, the engine might in principle consider retrying while the HTTP
response has already committed a status code that can't change. It's not
engineered around, because there's no client left to observe the
difference either way — the connection is already gone. Documented rather
than "fixed" with speculative complexity.

**Completion-token counting during a stream sums `Counter.CountText`
over each `Delta.Content` fragment, but a provider's own final `Usage` (if
it sends one) always wins.** The README warns a stream may end without a
usage block — confirmed in practice: OpenAI's real streaming API never
sends one, while the mock provider and (per its own `Stream`
implementation) Anthropic's `message_delta` event do. `CountText` (added
to `internal/tokenizer`) has no message-framing overhead applied, unlike
`CountMessages` — it's counting raw fragments, not a formatted message.

**A cache hit for `stream:true` is served as a synthesized SSE stream,
not a plain JSON body.** The cache key deliberately excludes `stream`
(README, Tier-1 cache key), meaning a streaming request can legitimately
hit an entry a non-streaming request wrote, and vice versa. Serving that
hit as raw JSON would silently violate the `Accept`/`Content-Type`
contract the client's `stream:true` asked for. `writeCachedStream`
(`stream.go`) instead sends the complete cached content as one content
chunk, one finish-reason chunk, and `[DONE]` — a fast burst rather than
paced tokens, since there's nothing left to wait for. This is an
intentional, if slightly unusual, choice: it means a client watching for
"realistic" pacing can tell a cache hit from a live generation by how
fast it arrives. That's an acceptable, honestly-documented side effect,
not a bug.

**Only a stream that finishes cleanly gets cached; a mid-stream failure
never writes a partial completion into either tier.** `storeInCaches` is
only called from the success path in `handleStreamingChatCompletion`,
after `ExecuteStream` returns with no error — matching the README's
"never cache a partial stream" literally.

**`internal/providers/mock`'s `Provider` gained `FailMidStream` and
`StreamDeltaLatency`.** Phase 8's own gate requires a test where "the
provider dies after N tokens" and a demonstration that a client actually
receives output incrementally, not in one flush at the end. The existing
mock provider could only fail before sending anything or succeed
entirely; proving either gate criterion needed a fixture that can die
partway through, or that paces its deltas over real wall-clock time.
Both are small, additive fields on the existing type, not a new fake.

## Consequences

- `X-Gateway-Attempts` is not set on a streaming response. Non-streaming
  responses report it because the full attempt history is known before
  anything is written; a streaming response commits its headers at the
  first flush, before the winning candidate's own attempt is even
  recorded as successful (that only happens once its `Stream` call
  returns `nil`, which is after the last delta). Adding it would mean
  either buffering the whole response (defeating streaming) or emitting
  attempts as a trailer, which most HTTP/1.1 clients and intermediary
  proxies don't support. `X-Gateway-Provider` is still set, at header-commit
  time, since the provider name is already known by then.
- `TestHandleChatCompletions_Streaming_RealSocketDeliversIncrementally`
  (`internal/server/stream_socket_test.go`) is the automated, permanent
  proof of "not one buffered blob" — it runs the real handler behind a
  real `net/http` server on a real TCP socket and asserts the client
  actually observes a time gap between the first and last SSE line. A
  manual `curl -N` run against the same setup during development showed
  each of 9 tokens arriving roughly 400ms apart, matching the injected
  per-delta latency almost exactly.
