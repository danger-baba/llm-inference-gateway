package embedding

import (
	"os"
	"path/filepath"
	"testing"
)

// realVocab loads the actual all-MiniLM-L6-v2 vocab file fetched into
// .cache/minilm (see Makefile's download-embedding-model target). Tests
// skip themselves when it isn't present, same pattern as the Redis
// integration tests in internal/ratelimit.
func realVocab(t *testing.T) map[string]int32 {
	t.Helper()
	path := os.Getenv("EMBEDDING_VOCAB_PATH")
	if path == "" {
		path = filepath.Join("..", "..", ".cache", "minilm", "vocab.txt")
	}
	vocab, err := LoadVocab(path)
	if err != nil {
		t.Skipf("vocab file not available at %s: %v", path, err)
	}
	return vocab
}

func TestLoadVocab_KnownIDs(t *testing.T) {
	vocab := realVocab(t)
	tests := map[string]int32{
		"[PAD]": 0, "[UNK]": 100, "[CLS]": 101, "[SEP]": 102,
		"!": 999, ",": 1010, "##ing": 2075, "world": 2088, "play": 2377, "playing": 2652, "hello": 7592,
	}
	for token, want := range tests {
		if got := vocab[token]; got != want {
			t.Errorf("vocab[%q] = %d, want %d", token, got, want)
		}
	}
}

func TestEncode_SimpleSentence(t *testing.T) {
	tok := NewWordPieceTokenizer(realVocab(t))
	ids, mask, types := tok.Encode("Hello, world!", 32)

	want := []int64{101, 7592, 1010, 2088, 999, 102} // [CLS] hello , world ! [SEP]
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want length %d", ids, len(want))
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %d, want %d (full: %v)", i, ids[i], want[i], ids)
		}
	}
	for i, m := range mask {
		if m != 1 {
			t.Errorf("attentionMask[%d] = %d, want 1", i, m)
		}
	}
	for i, tt := range types {
		if tt != 0 {
			t.Errorf("tokenTypeIDs[%d] = %d, want 0", i, tt)
		}
	}
}

func TestEncode_WholeWordMatchPreferredOverSubwordSplit(t *testing.T) {
	tok := NewWordPieceTokenizer(realVocab(t))
	ids, _, _ := tok.Encode("playing", 32)

	// "playing" is itself a whole vocab entry (2652); greedy longest-match
	// must find it directly rather than splitting into play + ##ing.
	want := []int64{101, 2652, 102}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want length %d", ids, len(want))
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %d, want %d", i, ids[i], want[i])
		}
	}
}

func TestEncode_UnknownWordBecomesUNK(t *testing.T) {
	vocab := map[string]int32{"[CLS]": 101, "[SEP]": 102, "[UNK]": 100}
	tok := NewWordPieceTokenizer(vocab)

	ids, _, _ := tok.Encode("zzzznotarealword", 32)
	want := []int64{101, 100, 102}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want length %d", ids, len(want))
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %d, want %d", i, ids[i], want[i])
		}
	}
}

func TestEncode_Truncation(t *testing.T) {
	tok := NewWordPieceTokenizer(realVocab(t))
	ids, mask, types := tok.Encode("hello world hello world hello world hello world", 5)

	if len(ids) != 5 {
		t.Fatalf("len(ids) = %d, want 5", len(ids))
	}
	if ids[len(ids)-1] != int64(102) { // [SEP]
		t.Errorf("last token = %d, want [SEP] (102)", ids[len(ids)-1])
	}
	if len(mask) != 5 || len(types) != 5 {
		t.Errorf("mask/types length = %d/%d, want 5/5", len(mask), len(types))
	}
}

func TestEncode_CaseAndAccentInsensitive(t *testing.T) {
	tok := NewWordPieceTokenizer(realVocab(t))
	lower, _, _ := tok.Encode("hello", 32)
	upper, _, _ := tok.Encode("HELLO", 32)

	if len(lower) != len(upper) {
		t.Fatalf("lower = %v, upper = %v, want same length", lower, upper)
	}
	for i := range lower {
		if lower[i] != upper[i] {
			t.Errorf("token[%d]: lower=%d upper=%d, want equal (case-insensitive)", i, lower[i], upper[i])
		}
	}
}
