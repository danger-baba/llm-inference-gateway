package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/danger-baba/llm-inference-gateway/internal/ledger"
	"github.com/danger-baba/llm-inference-gateway/internal/metrics"
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
	tiers []router.Tier, cost int64, scopes ratelimit.Scopes, promptTokens int, start time.Time,
) {
	ctx := r.Context()

	cached, tier, lookup := checkCaches(ctx, deps, req, identity)
	if cached != nil {
		_ = deps.limiter.Adjust(ctx, scopes, cost)
		writeCachedStream(w, tier, cached)
		recordCacheHitObservability(ctx, deps, identity, tier, cached, time.Since(start))
		if len(cached.Choices) > 0 {
			maybeLogResponseBody(ctx, deps, cached.Choices[0].Message.Content)
		}
		return
	}

	flusher, canFlush := w.(http.Flusher)
	id := "chatcmpl-" + newRequestID()
	created := time.Now().Unix()

	var fullContent strings.Builder
	completionTokens := 0
	finishReason := ""
	headersSent := false
	currentProvider, currentModel := "", ""

	// onDelta is called for every Delta a provider sends, in order, for
	// as long as ExecuteStream keeps choosing candidates. The very first
	// successful call is also the moment this handler commits to being
	// an SSE response at all -- see the doc comment above.
	onDelta := func(providerName, model string, d providers.Delta) error {
		currentProvider, currentModel = providerName, model
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
		handleStreamError(ctx, w, r, deps, identity, scopes, cost, promptTokens, completionTokens, headersSent, currentProvider, currentModel, start, err)
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
	recordSuccessObservability(ctx, deps, identity, result.Provider, result.Model,
		*usage, len(result.Attempts), time.Since(start), result.ProviderDuration)
	maybeLogResponseBody(ctx, deps, fullContent.String())

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
	ctx context.Context, w http.ResponseWriter, r *http.Request, deps chatDeps, identity auth.Identity, scopes ratelimit.Scopes,
	cost int64, promptTokens, completionTokens int, headersSent bool, currentProvider, currentModel string, start time.Time, err error,
) {
	flushed := headersSent
	var streamErr *retry.StreamError
	if errors.As(err, &streamErr) {
		flushed = streamErr.Flushed
	}

	if !flushed {
		_ = deps.limiter.Adjust(ctx, scopes, cost)
		status, lastProvider := writeEngineError(w, r, err)
		recordFailureObservability(status, lastProvider)
		return
	}

	// Charge for what was actually generated before things went wrong;
	// refund the rest of the original estimate.
	actual := int64(promptTokens + completionTokens)
	_ = deps.limiter.Adjust(ctx, scopes, cost-actual)

	_ = writeSSEEvent(w, "error", map[string]any{
		"error": map[string]string{
			"message":    err.Error(),
			"request_id": requestIDFromContext(ctx),
		},
	})
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// The wire-level status really was 200 -- headers already committed
	// before this failure happened -- so that's what's recorded here; the
	// logical failure is captured in the error event's own content, not
	// in this status code, because there's no other status left to give.
	recordPartialStreamObservability(ctx, deps, identity, currentProvider, currentModel, promptTokens, completionTokens, time.Since(start))
}

// recordPartialStreamObservability handles the one shape of row neither
// recordSuccessObservability nor recordFailureObservability fit: a
// stream that reached the client (so it wire-level succeeded, hence
// StatusCode 200 below) but ended before finishing (so cost and usage
// only cover what was actually generated, not the full estimate).
func recordPartialStreamObservability(
	ctx context.Context, deps chatDeps, identity auth.Identity,
	provider, model string, promptTokens, completionTokens int, elapsed time.Duration,
) {
	metrics.RequestsTotal.WithLabelValues(strconv.Itoa(http.StatusOK), provider, "none").Inc()
	metrics.RequestDuration.Observe(elapsed.Seconds())
	metrics.TokensTotal.WithLabelValues("prompt").Add(float64(promptTokens))
	metrics.TokensTotal.WithLabelValues("completion").Add(float64(completionTokens))

	inPerMTok, outPerMTok := deps.engine.Pricing(provider, model)
	costUSD := float64(promptTokens)/1e6*inPerMTok + float64(completionTokens)/1e6*outPerMTok
	metrics.CostUSDTotal.Add(costUSD)

	if deps.ledger == nil {
		return
	}
	deps.ledger.Record(ledger.Entry{
		RequestID: requestUUID(ctx), OrgID: identity.OrgID, TeamID: identity.TeamID, VirtualKeyID: identity.KeyID,
		Provider: provider, Model: model,
		PromptTokens: promptTokens, CompletionTokens: completionTokens,
		CostUSD: costUSD, CacheTier: "none",
		Attempts: 1, StatusCode: http.StatusOK, LatencyMS: int(elapsed.Milliseconds()),
	})
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
