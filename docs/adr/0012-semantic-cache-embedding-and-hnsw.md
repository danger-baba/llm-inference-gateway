# 0012 — Semantic cache: ONNX embedding, hand-rolled tokenizer, HNSW workarounds, and the measured threshold

## Status

Accepted (Phase 7).

## Context

Phase 7 is the README's own most demanding phase, and it required several
decisions and two real upstream bug workarounds that aren't visible from
reading the final code without an explanation of why it looks the way it
does.

## Decisions

**Embedding via ONNX Runtime + cgo, not a hosted embeddings API.** The
README is explicit that embedding must be local because a network round
trip would eat the latency the cache exists to save. `all-MiniLM-L6-v2`
run through `github.com/yalue/onnxruntime_go` (cgo bindings over the real
ONNX Runtime C API) gives a ~22M-parameter model that embeds a short chat
turn in low single-digit milliseconds on CPU. The cost is a real one:
cgo requires a matching C toolchain to build (a 64-bit MinGW-w64 gcc on
Windows; the box's pre-installed MinGW was 32-bit only and had to be
replaced), and the Docker image needed a base-image change from Alpine to
Debian-bookworm because the prebuilt `onnxruntime.so` links against glibc,
not musl. Both are documented as a build-environment cost, not deferred
silently.

**Hand-rolled BERT WordPiece tokenizer, not a wrapped library.**
`all-MiniLM-L6-v2` expects BERT's exact WordPiece scheme (basic
tokenization → accent stripping → greedy longest-match subword lookup
against `vocab.txt`), and there was no dependency-free, already-vetted Go
implementation that matched it precisely enough to trust blindly.
`internal/embedding/wordpiece.go` implements it directly against the
model's own `vocab.txt`, which also means the tokenizer and the model
files are versioned together — there is no risk of a generic
tokenizer library silently drifting from what the checked-in model
actually expects.

**`coder/hnsw` for the vector index, with two workarounds for real
upstream bugs.**

1. `coder/hnsw`'s own `go.mod` requires `google/renameio` at a version
   (`v1.0.1`) that does not export the `TempFile` function its
   `encode.go` calls — this is broken in the library as published across
   at least `v0.5.0`–`v0.6.1`. `go.mod` carries `replace
   github.com/google/renameio => github.com/google/renameio v0.1.0`,
   which does export a `TempFile(dir, path string) (*PendingFile,
   error)` matching the call site. `replace` directives bypass Go's
   normal minimum-version selection, so this builds cleanly without
   forking the dependency.
2. `Graph.Delete(key)` can leave an upper layer's entry point nil (its
   `layer.entry()` returns an arbitrary map entry, or nil if the layer's
   map is now empty), and `Graph.Search()` does not guard against that,
   producing a reproducible nil-pointer panic inside `layerNode.search`
   on the very next search after a delete empties a layer. Confirmed via
   a real crash under `TestSet_LRUEvictionAtCapacity` before the fix.
   `internal/cache/semantic/semantic.go`'s `evictOldest()` therefore
   never calls `Delete` at all — it only removes the entry from
   `s.entries` and `s.lru`, leaving an orphaned-but-harmless graph node
   (`Get()` already filters search results by checking `s.entries`, so
   an orphan is simply never returned). `rebuildGraphIfNeeded()`
   rebuilds the graph from scratch once orphans exceed `maxVectors`, so
   worst-case memory is bounded at roughly 2x `maxVectors`, not
   unbounded. Covered by both the original crash repro (now passing) and
   a 500-iteration heavy-churn stress test.

**Tenant isolation is a post-filter over one global graph, not one graph
per tenant.** `Get()` searches the single shared HNSW index and rejects
candidates whose stored tenant doesn't match the query's tenant, rather
than maintaining a separate graph per org. This trades a small amount of
wasted search work (candidates from other tenants are found, then
discarded) for not having to size, persist, and rebuild N independent
graphs as the tenant set grows and shrinks. If tenant cardinality becomes
large enough that this wasted work matters, this is the line to revisit.

**Config carries four fields the README's example doesn't show:**
`onnxruntime_lib_path`, `model_path`, `vocab_path`, and `persist_path`
under `cache.semantic`, each falling back to an environment variable of
the same name (`ONNXRUNTIME_LIB_PATH`, `EMBEDDING_MODEL_PATH`,
`EMBEDDING_VOCAB_PATH`) when empty in YAML. This mirrors the existing
`POSTGRES_DSN`-via-env pattern from ADR 0002: these are deployment-environment
specifics (where the shared library and model files landed on this
particular machine or image), not tuning knobs, so they don't belong
hardcoded into the checked-in `deploy/config.yaml` — the Dockerfile sets
them as image environment variables instead, pointing at assets it fetches
at build time.

`hnsw.ef_construction` is accepted and validated in config but has no
effect on `coder/hnsw`'s actual API — the library's `Graph` type doesn't
expose a build-time candidate-list-size parameter to set independently of
`ef_search`. It's kept in the config schema because the README's example
config lists it and removing it would be a silent, undocumented
deviation; a comment at the config struct says so.

**The cosine threshold is 0.89, measured — not the README's literal
0.95, and not guessed.** The README's own rule for Phase 7 is "do not
lower it without an eval set." `docs/eval/semantic_cache_eval.md` is that
eval set: 20 hand-picked pairs (10 true paraphrases, 10 topically-similar
but distinct requests) run through the real embedder. The measured
result: 0.95 has zero recall on this set (no true paraphrase ever
reaches it — Tier-1's exact cache already covers byte-identical
resubmissions, so a threshold this high makes Tier-2 dead weight for
genuinely reworded questions). The tightest safe threshold — the highest
value below the single observed false positive ("a poem about the
ocean" vs. "a story about the ocean," 0.8866, scored deceptively close to
genuine paraphrases because short mean-pooled sentence embeddings lean on
shared topic words over sentence structure) — is 0.89, which keeps
measured precision at 1.00 while recovering some recall (0.30 on this
set). See the eval doc for the full pair-by-pair data and the
precision/recall table across several candidate thresholds.

## Consequences

- The 0.89 margin above the one observed false positive is thin
  (0.0034). This is disclosed, not hidden: `docs/eval/semantic_cache_eval.md`
  says outright that 20 pairs bounds the decision, not the false-positive
  rate, and that real-traffic monitoring should back this up before
  trusting it at higher volume.
- If `coder/hnsw` ships a fixed `Delete` in a future release, the
  eviction workaround should be revisited — it's a deliberate avoidance
  of a real bug, not a preferred design.
- The build-environment cost of cgo (a real C toolchain, glibc base
  images) is a one-time tax paid by whoever builds this repo or its
  Docker image, not by request latency; it was accepted because the
  local-embedding requirement in the README is architectural, not
  incidental.
