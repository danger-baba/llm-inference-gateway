// Package anthropic translates between the gateway's canonical
// request/response shapes and Anthropic's Messages API, and issues the
// actual calls.
package anthropic

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

// anthropicVersion is the API version header Anthropic requires on every
// request; it identifies the wire format this client was written against,
// not a client library version.
const anthropicVersion = "2023-06-01"

// defaultMaxTokens is used when the client doesn't send max_tokens, which
// Anthropic's API requires on every request unlike OpenAI's. Phase 5's
// rate_limit.estimate_completion_tokens is a better source for this and
// should probably replace the constant once that config is wired through.
const defaultMaxTokens = 1024

type Provider struct {
	name    string
	baseURL string
	apiKey  string
	client  *http.Client
}

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

type wireRequest struct {
	Model         string        `json:"model"`
	MaxTokens     int           `json:"max_tokens"`
	System        string        `json:"system,omitempty"`
	Messages      []wireMessage `json:"messages"`
	Temperature   *float64      `json:"temperature,omitempty"`
	TopP          *float64      `json:"top_p,omitempty"`
	StopSequences []string      `json:"stop_sequences,omitempty"`
	Stream        bool          `json:"stream,omitempty"`
}

type wireResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type streamEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type errorResponse struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// translateRequest pulls any system-role messages out into Anthropic's
// separate top-level "system" field, since Anthropic's messages array only
// accepts user/assistant turns.
func translateRequest(req *providers.CanonicalRequest) wireRequest {
	var system strings.Builder
	msgs := make([]wireMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == "system" {
			if system.Len() > 0 {
				system.WriteString("\n")
			}
			system.WriteString(m.Content)
			continue
		}
		msgs = append(msgs, wireMessage{Role: m.Role, Content: m.Content})
	}

	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}

	return wireRequest{
		Model:         req.Model,
		MaxTokens:     maxTokens,
		System:        system.String(),
		Messages:      msgs,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: req.Stop,
	}
}

func translateResponse(body []byte) (*providers.CanonicalResponse, error) {
	var r wireResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	var text strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}

	return &providers.CanonicalResponse{
		ID:      r.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(), // Anthropic's response carries no created timestamp
		Model:   r.Model,
		Choices: []providers.Choice{
			{
				Index:        0,
				Message:      providers.Message{Role: "assistant", Content: text.String()},
				FinishReason: mapStopReason(r.StopReason),
			},
		},
		Usage: providers.Usage{
			PromptTokens:     r.Usage.InputTokens,
			CompletionTokens: r.Usage.OutputTokens,
			TotalTokens:      r.Usage.InputTokens + r.Usage.OutputTokens,
		},
	}, nil
}

// mapStopReason narrows Anthropic's stop reasons onto the OpenAI-shaped
// finish_reason vocabulary the gateway's client-facing response uses.
func mapStopReason(sr string) string {
	switch sr {
	case "":
		return ""
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	default:
		return sr
	}
}

func errorMessage(body []byte) string {
	var e errorResponse
	if err := json.Unmarshal(body, &e); err == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return string(body)
}

func (p *Provider) setHeaders(r *http.Request) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("x-api-key", p.apiKey)
	r.Header.Set("anthropic-version", anthropicVersion)
}

func (p *Provider) Complete(ctx context.Context, req *providers.CanonicalRequest) (*providers.CanonicalResponse, error) {
	body, err := json.Marshal(translateRequest(req))
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	p.setHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &providers.APIError{StatusCode: resp.StatusCode, Message: errorMessage(respBody)}
	}
	return translateResponse(respBody)
}

// Stream forwards Anthropic's named SSE events as Deltas: text from
// content_block_delta chunks, then a final Delta carrying FinishReason and
// Usage once message_delta/message_stop arrive.
func (p *Provider) Stream(ctx context.Context, req *providers.CanonicalRequest, out chan<- providers.Delta) error {
	wireReq := translateRequest(req)
	wireReq.Stream = true
	body, err := json.Marshal(wireReq)
	if err != nil {
		return fmt.Errorf("anthropic: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("anthropic: build request: %w", err)
	}
	p.setHeaders(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return &providers.APIError{StatusCode: resp.StatusCode, Message: errorMessage(respBody)}
	}

	var inputTokens int
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok || data == "" {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("anthropic: decode stream event: %w", err)
		}

		switch ev.Type {
		case "message_start":
			inputTokens = ev.Message.Usage.InputTokens
		case "content_block_delta":
			if ev.Delta.Type != "text_delta" {
				continue
			}
			select {
			case out <- providers.Delta{Content: ev.Delta.Text}:
			case <-ctx.Done():
				return ctx.Err()
			}
		case "message_delta":
			final := providers.Delta{
				FinishReason: mapStopReason(ev.Delta.StopReason),
				Usage: &providers.Usage{
					PromptTokens:     inputTokens,
					CompletionTokens: ev.Usage.OutputTokens,
					TotalTokens:      inputTokens + ev.Usage.OutputTokens,
				},
			}
			select {
			case out <- final:
			case <-ctx.Done():
				return ctx.Err()
			}
		case "message_stop":
			return nil
		case "error":
			return &providers.APIError{StatusCode: http.StatusBadGateway, Message: data}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("anthropic: read stream: %w", err)
	}
	return nil
}

func (p *Provider) Classify(_ error, status int) providers.FailureClass {
	return providers.ClassifyByStatus(status)
}

// Pricing is intentionally 0,0: see providers.Provider.Pricing.
func (p *Provider) Pricing(_ string) (float64, float64) { return 0, 0 }
