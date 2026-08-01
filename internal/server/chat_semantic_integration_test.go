package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/cache/semantic"
	"github.com/danger-baba/llm-inference-gateway/internal/embedding"
	"github.com/danger-baba/llm-inference-gateway/internal/providers/mock"
)

// realEmbedder loads the actual ONNX Runtime + all-MiniLM-L6-v2 assets
// (see Makefile's download-embedding-model target). This test is the
// handler-level version of Phase 7's gate: it exercises the real
// embedding pipeline through the real handler, not a fake standing in
// for "similar enough."
func realEmbedder(t *testing.T) *embedding.Embedder {
	t.Helper()

	libPath := os.Getenv("ONNXRUNTIME_LIB_PATH")
	if libPath == "" {
		libPath = filepath.Join("..", "..", ".cache", "onnxruntime-win-x64-1.28.0", "lib", "onnxruntime.dll")
	}
	modelPath := os.Getenv("EMBEDDING_MODEL_PATH")
	if modelPath == "" {
		modelPath = filepath.Join("..", "..", ".cache", "minilm", "model.onnx")
	}
	vocabPath := os.Getenv("EMBEDDING_VOCAB_PATH")
	if vocabPath == "" {
		vocabPath = filepath.Join("..", "..", ".cache", "minilm", "vocab.txt")
	}

	if _, err := os.Stat(libPath); err != nil {
		t.Skipf("onnxruntime shared library not available at %s: %v", libPath, err)
	}
	if _, err := os.Stat(modelPath); err != nil {
		t.Skipf("embedding model not available at %s: %v", modelPath, err)
	}

	e, err := embedding.NewEmbedder(libPath, modelPath, vocabPath)
	if err != nil {
		t.Fatalf("embedding.NewEmbedder() unexpected error: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestHandleChatCompletions_RealSemanticCache_RewordedQuestionHits(t *testing.T) {
	emb := realEmbedder(t)
	// 0.89 is the measured, not guessed, threshold -- see
	// docs/eval/semantic_cache_eval.md and docs/adr/0012. The pair below
	// ("capital of France") was chosen from that eval set because it
	// clears 0.89 with margin (measured 0.9324), unlike a looser
	// paraphrase such as the password-reset pair (measured 0.8568, which
	// does NOT clear 0.89 -- exactly the kind of false assumption the
	// eval set exists to catch).
	semStore := semantic.NewStore(16, 64, 100, 0.89, time.Hour)

	p := mock.New("mock-provider", time.Millisecond, 0, 0)
	deps := testDepsWithProvider(t, "mock-provider", p)
	deps.embedder = emb
	deps.semanticCache = semStore

	first := doChatRequest(t, deps, `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"What is the capital of France?"}]}`)
	if first.Code != 200 {
		t.Fatalf("first request status = %d, want 200; body = %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get("X-Gateway-Cache"); got != "none" {
		t.Fatalf("first request X-Gateway-Cache = %q, want %q", got, "none")
	}
	if p.CallCount() != 1 {
		t.Fatalf("CallCount() after first request = %d, want 1", p.CallCount())
	}

	// A reworded version of the same question should hit the semantic
	// cache: no provider call, X-Gateway-Cache: semantic.
	second := doChatRequest(t, deps, `{"model":"fast","temperature":0,"messages":[{"role":"user","content":"Which city is the capital of France?"}]}`)
	if second.Code != 200 {
		t.Fatalf("second request status = %d, want 200; body = %s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("X-Gateway-Cache"); got != "semantic" {
		t.Errorf("second (reworded) request X-Gateway-Cache = %q, want %q", got, "semantic")
	}
	if p.CallCount() != 1 {
		t.Errorf("CallCount() after reworded request = %d, want still 1 (should have been served from cache)", p.CallCount())
	}

	// A superficially similar question but with a different
	// response_format must miss, not hit, despite high similarity — the
	// guard rail is not optional.
	third := doChatRequest(t, deps, `{"model":"fast","temperature":0,"response_format":{"type":"json_object"},"messages":[{"role":"user","content":"What is the capital of France?"}]}`)
	if third.Code != 200 {
		t.Fatalf("third request status = %d, want 200; body = %s", third.Code, third.Body.String())
	}
	if got := third.Header().Get("X-Gateway-Cache"); got != "none" {
		t.Errorf("third request (different response_format) X-Gateway-Cache = %q, want %q (guard rail must reject despite similarity)", got, "none")
	}
	if p.CallCount() != 2 {
		t.Errorf("CallCount() after third request = %d, want 2", p.CallCount())
	}
}
