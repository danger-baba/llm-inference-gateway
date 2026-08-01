// Package tokenizer counts prompt tokens locally so the rate limiter can
// reserve a request's cost before calling any provider. Counting is an
// approximation shared across all providers (see docs/adr/0009): exact
// tokenization differs per vendor and sometimes per model, and getting
// close enough locally beats a network round trip to ask a provider.
package tokenizer

import (
	"fmt"

	"github.com/pkoukk/tiktoken-go"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

// perMessageOverhead and replyPrimingTokens follow OpenAI's own
// documented approximation for counting chat tokens: each message costs
// a few tokens of formatting overhead beyond its content, and the
// assistant's reply is "primed" with a couple more.
const (
	perMessageOverhead = 4
	replyPrimingTokens = 2
)

type Counter struct {
	enc *tiktoken.Tiktoken
}

// New loads the cl100k_base encoding. tiktoken-go fetches its BPE rank
// file over the network on first use unless TIKTOKEN_CACHE_DIR points at
// a pre-populated cache — see docs/adr/0009.
func New() (*Counter, error) {
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		return nil, fmt.Errorf("tokenizer: load cl100k_base encoding: %w", err)
	}
	return &Counter{enc: enc}, nil
}

// CountMessages estimates the prompt token cost of req's messages.
func (c *Counter) CountMessages(messages []providers.Message) int {
	total := replyPrimingTokens
	for _, m := range messages {
		total += perMessageOverhead + len(c.enc.Encode(m.Content, nil, nil))
	}
	return total
}

// CountText counts one string's tokens with no message-formatting
// overhead applied. A streamed completion arrives as a sequence of
// content fragments rather than one message, and — per the README — may
// end without a usage block at all, so the gateway counts each fragment
// on the way past and sums them, rather than tokenizing the whole reply
// only after it's fully assembled.
func (c *Counter) CountText(text string) int {
	return len(c.enc.Encode(text, nil, nil))
}
