# Semantic cache threshold eval

The build plan for Phase 7 is explicit: the semantic cache's cosine threshold starts at
0.95 "conservative by default," and must not be lowered without a measured eval set. This
is that eval set, and the numbers below are what actually came out of running it against
the real all-MiniLM-L6-v2 model via `internal/embedding`'s ONNX Runtime session — not
estimates.

Reproduce with:

```
go test ./internal/embedding/ -run TestEval_PrecisionAtConfiguredThreshold -v
```

(requires `ONNXRUNTIME_LIB_PATH`, `EMBEDDING_MODEL_PATH`, `EMBEDDING_VOCAB_PATH` — see
`make download-embedding-model`). The pairs themselves live in
`internal/embedding/eval_test.go` (`evalSet`), so this document and the test can't drift
apart silently.

## The 20 pairs

10 are paraphrases of the same request (`wantHit = true`); 10 are distinct requests chosen
to be superficially similar — shared topic words or sentence shape — specifically to stress
a threshold that's too permissive.

| a | b | want hit | measured cosine similarity |
|---|---|---|---|
| How do I reset my password? | How can I change my password? | yes | 0.8568 |
| How do I reset my password? | What steps do I take to reset a forgotten password? | yes | 0.8436 |
| What's the weather like today? | Can you tell me today's weather? | yes | 0.8408 |
| Summarize this article for me. | Can you give me a summary of this article? | yes | 0.7490 |
| Translate this sentence to French. | Please convert this sentence into French. | yes | 0.8994 |
| What is the capital of France? | Which city is the capital of France? | yes | 0.9324 |
| How much RAM does my laptop have? | What is the amount of memory installed on my laptop? | yes | 0.8734 |
| Write a poem about the ocean. | Compose a poem about the sea. | yes | 0.8716 |
| Explain quantum entanglement simply. | Can you explain quantum entanglement in simple terms? | yes | 0.9146 |
| List three benefits of exercise. | What are three advantages of exercising? | yes | 0.8595 |
| How do I reset my password? | How do I reset my email? | no | 0.6857 |
| What's the weather like today? | What's the stock market doing today? | no | 0.3894 |
| Summarize this article for me. | Translate this article for me. | no | 0.6565 |
| What is the capital of France? | What is the population of France? | no | 0.7182 |
| Write a poem about the ocean. | Write a story about the ocean. | no | **0.8866** |
| Explain quantum entanglement simply. | Explain general relativity simply. | no | 0.3484 |
| List three benefits of exercise. | List three risks of exercise. | no | 0.7439 |
| How much RAM does my laptop have? | How much storage does my laptop have? | no | 0.7287 |
| Can you help me debug this Python function? | Can you help me debug this JavaScript function? | no | 0.5643 |
| What time is it in Tokyo? | What time is it in London? | no | 0.5725 |

The bolded row is the one that mattered for picking the threshold: "a poem about the
ocean" and "a story about the ocean" are different requests (different output format
entirely), but all-MiniLM-L6-v2 scores them almost as similar as genuine paraphrases,
because short-sentence mean-pooled embeddings lean heavily on shared topic words over
sentence structure. This is a real limitation of the model, not a bug in the tokenizer or
pooling code — see the Known Limitations note below.

## Precision/recall at candidate thresholds

| Threshold | True positives | False positives | Recall | Precision |
|---|---|---|---|---|
| 0.95 (original default) | 0 | 0 | 0.00 | n/a — no hits at all |
| 0.90 | 2 | 0 | 0.20 | 1.00 |
| **0.89 (chosen default)** | **3** | **0** | **0.30** | **1.00** |
| 0.87 | 5 | 1 | 0.50 | 0.83 |
| 0.85 | 7 | 1 | 0.70 | 0.88 |

0.95 is not merely "conservative" — on this eval set it is dead weight: no paraphrase in
the set ever reaches it, so the tier would never fire on reworded-but-equivalent chat
questions, only on near-byte-identical resubmissions (which Tier-1's exact cache already
catches). 0.89 is the highest threshold below the one measured false positive (0.8866),
which keeps precision at 1.00 on this set while recovering a modest amount of recall (the
three closest paraphrases: "capital of France," "quantum entanglement," and "translate to
French," all ≥ 0.8994).

## Why 0.89 and not lower

Every threshold below 0.8866 (exclusive) admits the poem/story false positive, and the
build's own stated priority is explicit: a wrong cache hit returns a confident wrong
answer, which is strictly worse than the latency/cost of a cache miss (README, "Design
decisions and trade-offs"). 0.89 was chosen over exactly 0.8867 for a small safety margin
against embedding-level numerical noise across ONNX Runtime versions/hardware; it is still
only ~0.003 above the observed false positive, so this is a thin margin, not a solved
problem — see Known Limitations.

## Honest limitation this eval surfaced

20 pairs is enough to make a threshold decision defensible, not enough to bound the false
positive rate tightly. The margin above the single observed false positive is thin (0.89 vs
0.8866), and it is easy to imagine other content/format-mismatched pairs scoring in the same
0.87–0.90 band this eval set didn't sample (e.g. "write a haiku about X" vs "write a limerick
about X"). If this is deployed against real traffic, watch the semantic-cache-hit metric
against a sample of manually-graded hits before trusting the threshold at higher volume, and
grow this eval set from real near-miss traffic rather than more hand-written pairs.
