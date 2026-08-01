package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/retry"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

// chatDeps is everything the chat completions handler needs to route and
// execute a request. It's a struct rather than loose Options fields so
// tests can build one directly without going through the full Server.
type chatDeps struct {
	router *router.Router
	engine *retry.Engine
}

const streamNotYetSupportedMsg = "streaming is not supported yet (lands in Phase 8); send stream:false or omit it"

func handleChatCompletions(deps chatDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req providers.CanonicalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, r, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if req.Model == "" {
			writeJSONError(w, r, http.StatusBadRequest, "model is required")
			return
		}
		if len(req.Messages) == 0 {
			writeJSONError(w, r, http.StatusBadRequest, "messages must not be empty")
			return
		}
		if req.Stream {
			writeJSONError(w, r, http.StatusNotImplemented, streamNotYetSupportedMsg)
			return
		}

		tiers, err := deps.router.Resolve(req.Model)
		if err != nil || len(tiers) == 0 {
			writeJSONError(w, r, http.StatusBadRequest, fmt.Sprintf("unknown model %q", req.Model))
			return
		}

		result, err := deps.engine.Execute(r.Context(), tiers, &req)
		if err != nil {
			writeEngineError(w, r, err)
			return
		}

		result.Response.Model = req.Model // report back what the client asked for, not the vendor's internal name

		w.Header().Set("X-Gateway-Provider", result.Provider)
		w.Header().Set("X-Gateway-Attempts", formatAttempts(result.Attempts))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(result.Response)
	}
}

func formatAttempts(attempts []retry.Attempt) string {
	parts := make([]string, len(attempts))
	for i, a := range attempts {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

// writeEngineError maps a retry.Error to an HTTP status: the underlying
// provider's own status when there is one, 503 when every candidate's
// breaker was open, 502 for anything else (e.g. a raw network error).
func writeEngineError(w http.ResponseWriter, r *http.Request, err error) {
	var retryErr *retry.Error
	var attempts []retry.Attempt
	if errors.As(err, &retryErr) {
		attempts = retryErr.Attempts
	}
	if len(attempts) > 0 {
		w.Header().Set("X-Gateway-Attempts", formatAttempts(attempts))
	}

	status := http.StatusBadGateway
	switch {
	case errors.Is(err, retry.ErrNoHealthyProvider):
		status = http.StatusServiceUnavailable
	default:
		var apiErr *providers.APIError
		if errors.As(err, &apiErr) {
			status = apiErr.StatusCode
		}
	}
	writeJSONError(w, r, status, err.Error())
}

func writeJSONError(w http.ResponseWriter, r *http.Request, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message":    message,
			"request_id": requestIDFromContext(r.Context()),
		},
	})
}
