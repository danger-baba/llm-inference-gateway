package tokenizer

import (
	"testing"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

func TestCountMessages_NonZeroAndMonotonic(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	short := c.CountMessages([]providers.Message{{Role: "user", Content: "hi"}})
	long := c.CountMessages([]providers.Message{{Role: "user", Content: "hi there, this is a much longer message with many more words in it"}})

	if short <= 0 {
		t.Errorf("CountMessages(short) = %d, want > 0", short)
	}
	if long <= short {
		t.Errorf("CountMessages(long) = %d, want > CountMessages(short) = %d", long, short)
	}
}

func TestCountMessages_EmptyMessagesStillCostsOverhead(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	got := c.CountMessages(nil)
	if got != replyPrimingTokens {
		t.Errorf("CountMessages(nil) = %d, want %d (reply priming only)", got, replyPrimingTokens)
	}
}

func TestCountMessages_MultipleMessagesSumOverhead(t *testing.T) {
	c, err := New()
	if err != nil {
		t.Fatalf("New() unexpected error: %v", err)
	}

	one := c.CountMessages([]providers.Message{{Role: "user", Content: "hi"}})
	two := c.CountMessages([]providers.Message{{Role: "user", Content: "hi"}, {Role: "user", Content: "hi"}})

	hiTokens := len(c.enc.Encode("hi", nil, nil))
	want := one + perMessageOverhead + hiTokens
	if two != want {
		t.Errorf("CountMessages(two identical messages) = %d, want %d", two, want)
	}
}
