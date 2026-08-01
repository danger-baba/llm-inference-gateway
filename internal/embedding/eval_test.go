package embedding

import (
	"os"
	"testing"
)

// evalPair is one row of the semantic-cache eval set described in
// docs/eval/semantic_cache_eval.md. wantHit records whether a and b
// describe the same request (a true paraphrase) or a merely
// topic-adjacent but distinct request.
type evalPair struct {
	a, b    string
	wantHit bool
}

// evalSet is the 20-pair eval set required before tuning the semantic
// cache's cosine threshold away from an arbitrary default (see
// docs/adr/0012). Half are paraphrases of the same request; half are
// distinct requests chosen to be superficially similar (shared topic
// words, shared sentence shape) so that a naive threshold is tempted to
// treat them as a hit.
var evalSet = []evalPair{
	{"How do I reset my password?", "How can I change my password?", true},
	{"How do I reset my password?", "What steps do I take to reset a forgotten password?", true},
	{"What's the weather like today?", "Can you tell me today's weather?", true},
	{"Summarize this article for me.", "Can you give me a summary of this article?", true},
	{"Translate this sentence to French.", "Please convert this sentence into French.", true},
	{"What is the capital of France?", "Which city is the capital of France?", true},
	{"How much RAM does my laptop have?", "What is the amount of memory installed on my laptop?", true},
	{"Write a poem about the ocean.", "Compose a poem about the sea.", true},
	{"Explain quantum entanglement simply.", "Can you explain quantum entanglement in simple terms?", true},
	{"List three benefits of exercise.", "What are three advantages of exercising?", true},
	{"How do I reset my password?", "How do I reset my email?", false},
	{"What's the weather like today?", "What's the stock market doing today?", false},
	{"Summarize this article for me.", "Translate this article for me.", false},
	{"What is the capital of France?", "What is the population of France?", false},
	{"Write a poem about the ocean.", "Write a story about the ocean.", false},
	{"Explain quantum entanglement simply.", "Explain general relativity simply.", false},
	{"List three benefits of exercise.", "List three risks of exercise.", false},
	{"How much RAM does my laptop have?", "How much storage does my laptop have?", false},
	{"Can you help me debug this Python function?", "Can you help me debug this JavaScript function?", false},
	{"What time is it in Tokyo?", "What time is it in London?", false},
}

// TestEval_PrecisionAtConfiguredThreshold measures precision and recall
// of the eval set at the gateway's default semantic-cache threshold
// (see deploy/config.yaml, docs/adr/0012) against the real embedding
// model. It is the reproducible source of the numbers reported in
// docs/eval/semantic_cache_eval.md — re-run it any time the model,
// tokenizer, or threshold changes.
//
// Skips when the ONNX Runtime / model assets are not available, same as
// every other real-embedder test in this package.
func TestEval_PrecisionAtConfiguredThreshold(t *testing.T) {
	const threshold = 0.89

	libPath := os.Getenv("ONNXRUNTIME_LIB_PATH")
	modelPath := os.Getenv("EMBEDDING_MODEL_PATH")
	vocabPath := os.Getenv("EMBEDDING_VOCAB_PATH")
	if libPath == "" || modelPath == "" || vocabPath == "" {
		t.Skip("ONNXRUNTIME_LIB_PATH / EMBEDDING_MODEL_PATH / EMBEDDING_VOCAB_PATH not set")
	}
	if _, err := os.Stat(libPath); err != nil {
		t.Skipf("onnxruntime shared library not available: %v", err)
	}
	if _, err := os.Stat(modelPath); err != nil {
		t.Skipf("embedding model not available: %v", err)
	}

	e, err := NewEmbedder(libPath, modelPath, vocabPath)
	if err != nil {
		t.Fatalf("NewEmbedder() unexpected error: %v", err)
	}
	defer e.Close()

	var truePos, falsePos, falseNeg, trueNeg int
	for _, p := range evalSet {
		va, err := e.Embed(p.a)
		if err != nil {
			t.Fatalf("Embed(%q): %v", p.a, err)
		}
		vb, err := e.Embed(p.b)
		if err != nil {
			t.Fatalf("Embed(%q): %v", p.b, err)
		}
		sim := CosineSimilarity(va, vb)
		hit := sim >= threshold
		t.Logf("wantHit=%-5v hit=%-5v sim=%.4f  a=%q  b=%q", p.wantHit, hit, sim, p.a, p.b)

		switch {
		case p.wantHit && hit:
			truePos++
		case p.wantHit && !hit:
			falseNeg++
		case !p.wantHit && hit:
			falsePos++
		case !p.wantHit && !hit:
			trueNeg++
		}
	}

	var precision float64
	if truePos+falsePos > 0 {
		precision = float64(truePos) / float64(truePos+falsePos)
	} else {
		precision = 1 // no hits at all -> vacuously no wrong hits
	}
	recall := float64(truePos) / float64(truePos+falseNeg)
	t.Logf("threshold=%.2f  TP=%d FP=%d FN=%d TN=%d  precision=%.2f recall=%.2f",
		threshold, truePos, falsePos, falseNeg, trueNeg, precision, recall)

	// The gateway's stated priority is precision over recall (README:
	// "the failure mode of an over-permissive semantic cache is a wrong
	// answer served confidently"). At the chosen threshold this eval set
	// must show zero false positives; recall is allowed to be partial.
	if falsePos > 0 {
		t.Errorf("false positives = %d at threshold %.2f, want 0 (a wrong cache hit is worse than a cache miss)", falsePos, threshold)
	}
}
