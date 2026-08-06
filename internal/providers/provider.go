// Package providers defines the contract every LLM backend implements and
// the canonical request/response shapes the gateway translates to and from.
package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// CanonicalRequest is the gateway's internal representation of a chat
// completion request, translated from the client's OpenAI-shaped body and
// translated again into each provider's own dialect before the call.
type CanonicalRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	Stop           []string        `json:"stop,omitempty"`
	Seed           *int            `json:"seed,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	Tools          json.RawMessage `json:"tools,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	User           string          `json:"user,omitempty"`

	// CacheTTL is a gateway-only extension, never sent to a provider: an
	// optional client hint for how long a fresh answer to this request
	// should be remembered, as a Go duration string (e.g. "24h"). A
	// zero-or-negative duration means "don't cache this response at
	// all." The operator has final say — see
	// config.CacheConfig.MaxClientTTL and docs/adr/0017.
	CacheTTL string `json:"cache_ttl,omitempty"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CanonicalResponse is always OpenAI-shaped on the way out, regardless of
// which provider actually served the request.
type CanonicalResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Delta is one incremental chunk of a streamed completion. Usage is only
// populated on the final delta, if the provider sends one at all.
type Delta struct {
	Content      string
	FinishReason string
	Usage        *Usage
	Err          error
}

// FailureClass is the gateway's retry taxonomy, independent of any single
// provider's status codes.
type FailureClass int

const (
	Terminal FailureClass = iota
	Retryable
	Fallback
)

func (f FailureClass) String() string {
	switch f {
	case Retryable:
		return "retryable"
	case Fallback:
		return "fallback"
	default:
		return "terminal"
	}
}

// APIError carries the upstream HTTP status back through Complete/Stream so
// Classify and the retry engine can act on it without re-parsing
// provider-specific error bodies. RetryAfter is non-zero only when the
// provider sent a Retry-After header, and takes precedence over computed
// backoff when present.
type APIError struct {
	StatusCode int
	Message    string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("provider error (status %d): %s", e.StatusCode, e.Message)
}

// ClassifyByStatus is the shared, coarse classification described in the
// README's failure-mode table. A provider's own Classify may special-case
// beyond this, but every provider in this repo currently delegates to it.
func ClassifyByStatus(status int) FailureClass {
	switch {
	case status == 429 || status >= 500:
		return Retryable
	case status == 400 || status == 401 || status == 403 || status == 404:
		return Fallback
	default:
		return Terminal
	}
}

// ParseRetryAfter interprets a Retry-After header value, which the HTTP
// spec allows as either a delay in seconds or an HTTP-date. It returns 0
// for an empty, unparseable, or already-past value.
func ParseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// Provider is implemented by every LLM backend the gateway can route to.
type Provider interface {
	Name() string
	Complete(ctx context.Context, req *CanonicalRequest) (*CanonicalResponse, error)
	Stream(ctx context.Context, req *CanonicalRequest, out chan<- Delta) error
	Classify(err error, status int) FailureClass
	// Pricing is wired up in Phase 9 when the ledger needs it; every
	// implementation returns (0, 0) until then rather than fabricating
	// numbers nothing yet consumes.
	Pricing(model string) (inPerMTok, outPerMTok float64)
}

// HealthChecker is implemented by providers that expose a cheap
// out-of-band way to check reachability. It's deliberately not part of
// Provider itself — adding it there would force every implementation to
// have a real health endpoint, which isn't true of every possible
// provider — so the background prober type-asserts for it instead. See
// docs/adr/0006.
type HealthChecker interface {
	HealthCheck(ctx context.Context) error
}
