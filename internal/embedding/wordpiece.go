// Package embedding turns text into vectors locally: a hand-rolled BERT
// WordPiece tokenizer feeding an ONNX Runtime session running
// all-MiniLM-L6-v2. No network call is involved in either step — the
// whole point of local embedding is that the round trip an API call would
// cost is exactly the latency this cache tier exists to save.
package embedding

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

const (
	clsToken = "[CLS]"
	sepToken = "[SEP]"
	unkToken = "[UNK]"

	maxInputCharsPerWord = 100
)

// LoadVocab reads a BERT vocab.txt file: one token per line, line index
// (0-based) is the token's ID.
func LoadVocab(path string) (map[string]int32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("embedding: open vocab: %w", err)
	}
	defer f.Close()

	vocab := make(map[string]int32)
	scanner := bufio.NewScanner(f)
	var id int32
	for scanner.Scan() {
		vocab[scanner.Text()] = id
		id++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("embedding: read vocab: %w", err)
	}
	return vocab, nil
}

// WordPieceTokenizer implements BERT's tokenization scheme: lowercase,
// strip accents, split on whitespace/punctuation, then greedily match the
// longest known subword at each position (prefixing continuation pieces
// with "##"), falling back to [UNK] for anything unmatched.
type WordPieceTokenizer struct {
	vocab map[string]int32
}

func NewWordPieceTokenizer(vocab map[string]int32) *WordPieceTokenizer {
	return &WordPieceTokenizer{vocab: vocab}
}

// Encode returns input_ids, attention_mask, and token_type_ids for text,
// truncated to maxSeqLen tokens (always ending in [SEP] if truncated).
// token_type_ids is all zeros: every request here is a single sentence,
// never a sentence pair.
func (t *WordPieceTokenizer) Encode(text string, maxSeqLen int) (inputIDs, attentionMask, tokenTypeIDs []int64) {
	pieces := make([]string, 0, 32)
	pieces = append(pieces, clsToken)
	for _, tok := range t.basicTokenize(stripAccents(strings.ToLower(text))) {
		pieces = append(pieces, t.wordpiece(tok)...)
	}
	pieces = append(pieces, sepToken)

	if len(pieces) > maxSeqLen {
		pieces = pieces[:maxSeqLen-1]
		pieces = append(pieces, sepToken)
	}

	inputIDs = make([]int64, len(pieces))
	for i, p := range pieces {
		id, ok := t.vocab[p]
		if !ok {
			id = t.vocab[unkToken]
		}
		inputIDs[i] = int64(id)
	}
	attentionMask = make([]int64, len(inputIDs))
	for i := range attentionMask {
		attentionMask[i] = 1
	}
	tokenTypeIDs = make([]int64, len(inputIDs))
	return inputIDs, attentionMask, tokenTypeIDs
}

// wordpiece splits one already-basic-tokenized word into vocab subwords
// via greedy longest-match-first, per BERT's reference algorithm.
func (t *WordPieceTokenizer) wordpiece(word string) []string {
	runes := []rune(word)
	if len(runes) > maxInputCharsPerWord {
		return []string{unkToken}
	}

	var output []string
	start := 0
	for start < len(runes) {
		end := len(runes)
		var matched string
		found := false
		for end > start {
			substr := string(runes[start:end])
			if start > 0 {
				substr = "##" + substr
			}
			if _, ok := t.vocab[substr]; ok {
				matched = substr
				found = true
				break
			}
			end--
		}
		if !found {
			return []string{unkToken}
		}
		output = append(output, matched)
		start = end
	}
	return output
}

// basicTokenize splits on whitespace and punctuation, emitting each
// punctuation character as its own token, per BERT's BasicTokenizer.
func (t *WordPieceTokenizer) basicTokenize(text string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range cleanText(text) {
		switch {
		case isWhitespace(r):
			flush()
		case isPunct(r):
			flush()
			tokens = append(tokens, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}

func stripAccents(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func cleanText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == 0 || r == 0xFFFD || isControl(r):
			continue
		case isWhitespace(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isWhitespace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

func isControl(r rune) bool {
	switch r {
	case '\t', '\n', '\r':
		return false
	}
	return unicode.IsControl(r)
}

// isPunct follows BERT's own rule of treating all ASCII non-alphanumeric
// characters as punctuation (matching its reference implementation
// exactly), in addition to whatever Unicode already classifies as
// punctuation or symbol.
func isPunct(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return unicode.IsPunct(r) || unicode.IsSymbol(r)
}
