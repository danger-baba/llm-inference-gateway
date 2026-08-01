// Package openai translates between the gateway's canonical request/response
// shapes and OpenAI's chat completions API, and issues the actual calls.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

type Provider struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

// New builds an OpenAI-dialect provider with a shared, connection-reusing
// http.Client. No client-level timeout is set: per-attempt deadlines are
// derived from the request's remaining budget and applied via ctx by the
// caller, not baked into the client itself.
func New(name, baseURL, apiKey string) *Provider {
	return &Provider{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (p *Provider) Name() string { return p.name }

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []wireMessage   `json:"messages"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	Stop           []string        `json:"stop,omitempty"`
	Seed           *int            `json:"seed,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	Tools          json.RawMessage `json:"tools,omitempty"`
	Stream         bool            `json:"stream,omitempty"`
	User           string          `json:"user,omitempty"`
}

type chatResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int         `json:"index"`
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

type errorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func translateRequest(req *providers.CanonicalRequest) chatRequest {
	msgs := make([]wireMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = wireMessage{Role: m.Role, Content: m.Content}
	}
	return chatRequest{
		Model:          req.Model,
		Messages:       msgs,
		Temperature:    req.Temperature,
		TopP:           req.TopP,
		MaxTokens:      req.MaxTokens,
		Stop:           req.Stop,
		Seed:           req.Seed,
		ResponseFormat: req.ResponseFormat,
		Tools:          req.Tools,
		User:           req.User,
	}
}

func translateResponse(body []byte) (*providers.CanonicalResponse, error) {
	var r chatResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("openai: decode response: %w", err)
	}
	choices := make([]providers.Choice, len(r.Choices))
	for i, c := range r.Choices {
		choices[i] = providers.Choice{
			Index:        c.Index,
			Message:      providers.Message{Role: c.Message.Role, Content: c.Message.Content},
			FinishReason: c.FinishReason,
		}
	}
	return &providers.CanonicalResponse{
		ID:      r.ID,
		Object:  r.Object,
		Created: r.Created,
		Model:   r.Model,
		Choices: choices,
		Usage: providers.Usage{
			PromptTokens:     r.Usage.PromptTokens,
			CompletionTokens: r.Usage.CompletionTokens,
			TotalTokens:      r.Usage.TotalTokens,
		},
	}, nil
}

func errorMessage(body []byte) string {
	var e errorResponse
	if err := json.Unmarshal(body, &e); err == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return string(body)
}

func (p *Provider) Complete(ctx context.Context, req *providers.CanonicalRequest) (*providers.CanonicalResponse, error) {
	body, err := json.Marshal(translateRequest(req))
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("openai: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &providers.APIError{
			StatusCode: resp.StatusCode,
			Message:    errorMessage(respBody),
			RetryAfter: providers.ParseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}
	return translateResponse(respBody)
}

// HealthCheck hits GET /models, which is cheap and requires no completion
// tokens — about as close to "free" as an authenticated OpenAI-compatible
// endpoint gets.
func (p *Provider) HealthCheck(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return fmt.Errorf("openai: build health check request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return &providers.APIError{StatusCode: resp.StatusCode, Message: errorMessage(body)}
	}
	return nil
}

// Stream issues the same request with stream:true and forwards OpenAI's
// SSE chunks as Deltas until the "[DONE]" sentinel or ctx is done.
func (p *Provider) Stream(ctx context.Context, req *providers.CanonicalRequest, out chan<- providers.Delta) error {
	wireReq := translateRequest(req)
	wireReq.Stream = true
	body, err := json.Marshal(wireReq)
	if err != nil {
		return fmt.Errorf("openai: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("openai: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return &providers.APIError{
			StatusCode: resp.StatusCode,
			Message:    errorMessage(respBody),
			RetryAfter: providers.ParseRetryAfter(resp.Header.Get("Retry-After")),
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || data == "" {
			continue
		}
		if data == "[DONE]" {
			return nil
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("openai: decode stream chunk: %w", err)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := providers.Delta{
			Content:      chunk.Choices[0].Delta.Content,
			FinishReason: chunk.Choices[0].FinishReason,
		}
		select {
		case out <- delta:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("openai: read stream: %w", err)
	}
	return nil
}

func (p *Provider) Classify(_ error, status int) providers.FailureClass {
	return providers.ClassifyByStatus(status)
}

// Pricing is intentionally 0,0: see providers.Provider.Pricing.
func (p *Provider) Pricing(_ string) (float64, float64) { return 0, 0 }
