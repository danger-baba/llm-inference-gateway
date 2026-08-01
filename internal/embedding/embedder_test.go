package embedding

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// testEmbedder loads the real ONNX Runtime shared library and
// all-MiniLM-L6-v2 model from .cache (see Makefile's
// download-embedding-model target). Tests skip themselves when the assets
// aren't present, same pattern as the Redis integration tests.
func testEmbedder(t *testing.T) *Embedder {
	t.Helper()

	sharedLib := os.Getenv("ONNXRUNTIME_LIB_PATH")
	if sharedLib == "" {
		sharedLib = filepath.Join("..", "..", ".cache", "onnxruntime-win-x64-1.28.0", "lib", "onnxruntime.dll")
	}
	modelPath := os.Getenv("EMBEDDING_MODEL_PATH")
	if modelPath == "" {
		modelPath = filepath.Join("..", "..", ".cache", "minilm", "model.onnx")
	}
	vocabPath := os.Getenv("EMBEDDING_VOCAB_PATH")
	if vocabPath == "" {
		vocabPath = filepath.Join("..", "..", ".cache", "minilm", "vocab.txt")
	}

	if _, err := os.Stat(sharedLib); err != nil {
		t.Skipf("onnxruntime shared library not available at %s: %v", sharedLib, err)
	}
	if _, err := os.Stat(modelPath); err != nil {
		t.Skipf("embedding model not available at %s: %v", modelPath, err)
	}

	e, err := NewEmbedder(sharedLib, modelPath, vocabPath)
	if err != nil {
		t.Fatalf("NewEmbedder() unexpected error: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func TestEmbed_ProducesUnitLengthVector(t *testing.T) {
	e := testEmbedder(t)

	vec, err := e.Embed("The quick brown fox jumps over the lazy dog.")
	if err != nil {
		t.Fatalf("Embed() unexpected error: %v", err)
	}
	if len(vec) != EmbeddingDim {
		t.Fatalf("len(vec) = %d, want %d", len(vec), EmbeddingDim)
	}

	var normSq float64
	for _, v := range vec {
		normSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(normSq)
	if math.Abs(norm-1.0) > 1e-3 {
		t.Errorf("‖vec‖ = %v, want ~1.0 (L2-normalized)", norm)
	}
}

func TestEmbed_SimilarSentencesAreCloser(t *testing.T) {
	e := testEmbedder(t)

	a, err := e.Embed("How do I reset my password?")
	if err != nil {
		t.Fatalf("Embed(a) unexpected error: %v", err)
	}
	b, err := e.Embed("How can I change my password?")
	if err != nil {
		t.Fatalf("Embed(b) unexpected error: %v", err)
	}
	c, err := e.Embed("What is the capital of France?")
	if err != nil {
		t.Fatalf("Embed(c) unexpected error: %v", err)
	}

	simAB := CosineSimilarity(a, b)
	simAC := CosineSimilarity(a, c)
	if simAB <= simAC {
		t.Errorf("cosine(a,b) = %v, cosine(a,c) = %v; want a paraphrase closer than an unrelated question", simAB, simAC)
	}
	t.Logf("cosine(reworded password question) = %v, cosine(unrelated question) = %v", simAB, simAC)
}

func TestEmbed_IdenticalTextIsCosineOne(t *testing.T) {
	e := testEmbedder(t)

	a, err := e.Embed("Explain HNSW briefly.")
	if err != nil {
		t.Fatalf("Embed(a) unexpected error: %v", err)
	}
	b, err := e.Embed("Explain HNSW briefly.")
	if err != nil {
		t.Fatalf("Embed(b) unexpected error: %v", err)
	}

	sim := CosineSimilarity(a, b)
	if math.Abs(float64(sim)-1.0) > 1e-4 {
		t.Errorf("cosine(identical text) = %v, want ~1.0", sim)
	}
}

func TestCosineSimilarity_Orthogonal(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	if got := CosineSimilarity(a, b); got != 0 {
		t.Errorf("CosineSimilarity(orthogonal) = %v, want 0", got)
	}
}
