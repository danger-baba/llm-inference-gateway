package embedding

import (
	"fmt"
	"math"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// EmbeddingDim is all-MiniLM-L6-v2's sentence embedding width.
const EmbeddingDim = 384

// maxSeqLen bounds tokenized input length. 256 comfortably covers a
// concatenated user turn without the quadratic attention cost of the
// model's full 512-token limit.
const maxSeqLen = 256

// Embedder wraps an ONNX Runtime session running all-MiniLM-L6-v2,
// turning text into a mean-pooled, L2-normalized sentence vector — cosine
// similarity between two such vectors is then just a dot product.
//
// onnxruntime_go sessions are not documented as safe for concurrent Run
// calls from multiple goroutines, so every call to Embed serializes
// through a mutex. This caps embedding throughput at one request at a
// time, which is an acceptable trade for correctness until profiling
// shows it's actually a bottleneck — see docs/adr/0012.
type Embedder struct {
	mu        sync.Mutex
	session   *ort.DynamicAdvancedSession
	tokenizer *WordPieceTokenizer
}

// NewEmbedder loads vocabPath and modelPath and starts an ONNX Runtime
// session against the shared library at sharedLibPath. It initializes the
// (process-wide) ONNX Runtime environment on first call only.
func NewEmbedder(sharedLibPath, modelPath, vocabPath string) (*Embedder, error) {
	if !ort.IsInitialized() {
		ort.SetSharedLibraryPath(sharedLibPath)
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("embedding: initialize onnxruntime: %w", err)
		}
	}

	vocab, err := LoadVocab(vocabPath)
	if err != nil {
		return nil, err
	}

	session, err := ort.NewDynamicAdvancedSession(
		modelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("embedding: create session: %w", err)
	}

	return &Embedder{session: session, tokenizer: NewWordPieceTokenizer(vocab)}, nil
}

func (e *Embedder) Close() error {
	return e.session.Destroy()
}

// Embed tokenizes text, runs one forward pass, and returns its
// mean-pooled, L2-normalized sentence embedding.
func (e *Embedder) Embed(text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	inputIDs, attentionMask, tokenTypeIDs := e.tokenizer.Encode(text, maxSeqLen)
	seqLen := int64(len(inputIDs))
	inputShape := ort.NewShape(1, seqLen)

	inputIDsTensor, err := ort.NewTensor(inputShape, inputIDs)
	if err != nil {
		return nil, fmt.Errorf("embedding: input_ids tensor: %w", err)
	}
	defer func() { _ = inputIDsTensor.Destroy() }()

	attentionTensor, err := ort.NewTensor(inputShape, attentionMask)
	if err != nil {
		return nil, fmt.Errorf("embedding: attention_mask tensor: %w", err)
	}
	defer func() { _ = attentionTensor.Destroy() }()

	tokenTypeTensor, err := ort.NewTensor(inputShape, tokenTypeIDs)
	if err != nil {
		return nil, fmt.Errorf("embedding: token_type_ids tensor: %w", err)
	}
	defer func() { _ = tokenTypeTensor.Destroy() }()

	outputShape := ort.NewShape(1, seqLen, EmbeddingDim)
	output, err := ort.NewEmptyTensor[float32](outputShape)
	if err != nil {
		return nil, fmt.Errorf("embedding: allocate output tensor: %w", err)
	}
	defer func() { _ = output.Destroy() }()

	if err := e.session.Run(
		[]ort.Value{inputIDsTensor, attentionTensor, tokenTypeTensor},
		[]ort.Value{output},
	); err != nil {
		return nil, fmt.Errorf("embedding: run session: %w", err)
	}

	return meanPoolAndNormalize(output.GetData(), attentionMask, int(seqLen)), nil
}

// meanPoolAndNormalize averages each dimension of the token embeddings
// over only the real (non-padding) tokens the attention mask marks, then
// L2-normalizes the result — the standard sentence-transformers recipe
// for turning per-token BERT output into one sentence vector.
func meanPoolAndNormalize(hidden []float32, attentionMask []int64, seqLen int) []float32 {
	sums := make([]float32, EmbeddingDim)
	var count float32
	for t := 0; t < seqLen; t++ {
		if attentionMask[t] == 0 {
			continue
		}
		count++
		base := t * EmbeddingDim
		for d := 0; d < EmbeddingDim; d++ {
			sums[d] += hidden[base+d]
		}
	}
	if count == 0 {
		count = 1
	}
	for d := range sums {
		sums[d] /= count
	}

	var normSq float64
	for _, v := range sums {
		normSq += float64(v) * float64(v)
	}
	norm := math.Sqrt(normSq)
	if norm == 0 {
		norm = 1
	}
	for d := range sums {
		sums[d] = float32(float64(sums[d]) / norm)
	}
	return sums
}

// CosineSimilarity assumes both vectors are already L2-normalized (as
// Embed's output always is), so it's just a dot product.
func CosineSimilarity(a, b []float32) float32 {
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}
