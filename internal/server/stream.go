package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/ratelimit"
	"github.com/danger-baba/llm-inference-gateway/internal/retry"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

// chatCompletionChunk is the OpenAI-shaped "chat.completion.chunk" object
// every SSE data event carries, whether it originated from a live
// provider stream or was synthesized from a complete cached response.
type chatCompletionChunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []chunkChoice    `json:"choices"`
	Usage   *providers.Usage `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// writeSSEEvent writes one SSE frame: an optional named event line, then
// a single-line JSON data payload, then the blank line that terminates
// an SSE event. It does not flush -- callers flush once per event,
// exactly as the README requires.
func writeSSEEvent(w http.ResponseWriter, event string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	return nil
}

func writeSSEDone(w http.ResponseWriter) error {
	_, err := fmt.Fprint(w, "data: [DONE]\n\n")
	return err
}

// handleStreamingChatCompletion is handleChatCompletions' branch for
// stream:true. It's a separate function, not a branch mixed into the
// non-streaming body, because the two response contracts diverge in a
// way that's load-bearing, not cosmetic: this one may only commit its
// HTTP status once a provider has actually produced content, must flush
// after every event, and must never fail over once that first flush has
// happened (README, Streaming (SSE) passthrough).
func handleStreamingChatCompletion(
	w http.ResponseWriter, r *http.Request, deps chatDeps,
	req *providers.CanonicalRequest, identity auth.Identity,
	tiers []router.Tier, cost int64, scopes ratelimit.Scopes, promptTokens int,
) {
	ctx := r.Context()

	cached, tier, lookup := checkCaches(ctx, deps, req, identity)
	if cached != nil {
		_ = deps.limiter.Adjust(ctx, scopes, cost)
		writeCachedStream(w, tier, cached)
		return
	}

	flusher, canFlush := w.(http.Flusher)
	id := "chatcmpl-" + newRequestID()
	created := time.Now().Unix()

	var fullContent strings.Builder
	completionTokens := 0
	finishReason := ""
	headersSent := false

	// onDelta is called for every Delta a provider sends, in order, for
	// as long as ExecuteStream keeps choosing candidates. The very first
	// successful call is also the moment this handler commits to being
	// an SSE response at all -- see the doc comment above.
	onDelta := func(providerName string, d providers.Delta) error {
		if d.Content != "" {
			fullContent.WriteString(d.Content)
			completionTokens += deps.counter.CountText(d.Content)
		}
		if d.FinishReason != "" {
			finishReason = d.FinishReason
		}

		if !headersSent {
			if !canFlush {
				return errors.New("streaming unsupported: response writer does not implement http.Flusher")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.Header().Set("X-Gateway-Provider", providerName)
			w.Header().Set("X-Gateway-Cache", "none")
			w.WriteHeader(http.StatusOK)
			headersSent = true
		}

		if err := writeSSEEvent(w, "", chatCompletionChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{Content: d.Content}, FinishReason: nilIfEmpty(d.FinishReason)}},
		}); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	result, err := deps.engine.ExecuteStream(ctx, tiers, req, onDelta)
	if err != nil {
		handleStreamError(w, r, deps, scopes, cost, promptTokens, completionTokens, headersSent, err)
		return
	}

	if finishReason == "" {
		finishReason = "stop"
	}
	usage := result.Usage
	if usage == nil {
		// This provider never sent a final usage block (the README warns
		// this can happen); fall back to what was counted on the way past.
		usage = &providers.Usage{
			PromptTokens: promptTokens, CompletionTokens: completionTokens,
			TotalTokens: promptTokens + completionTokens,
		}
	}

	_ = writeSSEDone(w)
	if canFlush {
		flusher.Flush()
	}

	actual := int64(usage.PromptTokens + usage.CompletionTokens)
	_ = deps.limiter.Adjust(ctx, scopes, cost-actual)

	// The stream completed in full: this is exactly the "complete
	// stream" the README says may be cached, built from what was
	// actually forwarded, not from the estimate.
	storeInCaches(ctx, deps, lookup, &providers.CanonicalResponse{
		ID: id, Object: "chat.completion", Created: created, Model: req.Model,
		Choices: []providers.Choice{{Index: 0, Message: providers.Message{Role: "assistant", Content: fullContent.String()}, FinishReason: finishReason}},
		Usage:   *usage,
	})
}

// handleStreamError reconciles the reservation and finishes the response
// once ExecuteStream has failed. Whether that's a normal error response
// or a terminal SSE error event depends entirely on whether anything was
// ever flushed -- once it has, this cannot become a different status
// code, because the 200 and the event-stream headers already went out.
func handleStreamError(
	w http.ResponseWriter, r *http.Request, deps chatDeps, scopes ratelimit.Scopes,
	cost int64, promptTokens, completionTokens int, headersSent bool, err error,
) {
	flushed := headersSent
	var streamErr *retry.StreamError
	if errors.As(err, &streamErr) {
		flushed = streamErr.Flushed
	}

	if !flushed {
		_ = deps.limiter.Adjust(r.Context(), scopes, cost)
		writeEngineError(w, r, err)
		return
	}

	// Charge for what was actually generated before things went wrong;
	// refund the rest of the original estimate.
	actual := int64(promptTokens + completionTokens)
	_ = deps.limiter.Adjust(r.Context(), scopes, cost-actual)

	_ = writeSSEEvent(w, "error", map[string]any{
		"error": map[string]string{
			"message":    err.Error(),
			"request_id": requestIDFromContext(r.Context()),
		},
	})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// writeCachedStream serves a complete cached response (from either tier)
// as a synthesized SSE stream, so a client that asked for stream:true
// gets the content type it asked for even though nothing was actually
// streamed from a provider this time.
func writeCachedStream(w http.ResponseWriter, tier string, resp *providers.CanonicalResponse) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeCachedResponse(w, tier, resp)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Gateway-Cache", tier)
	w.WriteHeader(http.StatusOK)

	id := "chatcmpl-" + newRequestID()
	created := time.Now().Unix()

	content := ""
	finish := "stop"
	if len(resp.Choices) > 0 {
		content = resp.Choices[0].Message.Content
		if resp.Choices[0].FinishReason != "" {
			finish = resp.Choices[0].FinishReason
		}
	}

	_ = writeSSEEvent(w, "", chatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: resp.Model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{Role: "assistant", Content: content}}},
	})
	flusher.Flush()

	_ = writeSSEEvent(w, "", chatCompletionChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: resp.Model,
		Choices: []chunkChoice{{Index: 0, Delta: chunkDelta{}, FinishReason: &finish}},
	})
	flusher.Flush()

	_ = writeSSEDone(w)
	flusher.Flush()
}
