package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
	"github.com/danger-baba/llm-inference-gateway/internal/router"
)

// chatDeps is everything the chat completions handler needs to pick a
// provider and call it. It's a struct rather than loose Options fields so
// tests can build one directly without going through the full Server.
type chatDeps struct {
	router          *router.Router
	providers       map[string]providers.Provider
	providerTimeout map[string]time.Duration
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

		candidates, err := deps.router.Resolve(req.Model)
		if err != nil || len(candidates) == 0 {
			writeJSONError(w, r, http.StatusBadRequest, fmt.Sprintf("unknown model %q", req.Model))
			return
		}
		// Phase 2 takes only the first candidate; walking the rest of the
		// chain on failure is Phase 3's retry/fallback engine.
		candidate := candidates[0]

		provider, ok := deps.providers[candidate.ProviderName]
		if !ok {
			writeJSONError(w, r, http.StatusInternalServerError,
				fmt.Sprintf("provider %q is in the router but wasn't wired up", candidate.ProviderName))
			return
		}

		attemptCtx, cancel := context.WithTimeout(r.Context(), attemptTimeout(r.Context(), deps.providerTimeout[candidate.ProviderName]))
		defer cancel()

		providerReq := req
		providerReq.Model = candidate.Model // client alias -> provider's own model string

		resp, err := provider.Complete(attemptCtx, &providerReq)
		if err != nil {
			writeProviderError(w, r, err)
			return
		}

		resp.Model = req.Model // report back what the client asked for, not the vendor's internal name

		w.Header().Set("X-Gateway-Provider", candidate.ProviderName)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// attemptTimeout caps configured at whatever's left on the request's own
// deadline, so a per-attempt wait never outlives the client's budget.
func attemptTimeout(ctx context.Context, configured time.Duration) time.Duration {
	timeout := configured
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	if timeout < 0 {
		timeout = 0
	}
	return timeout
}

func writeProviderError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway
	var apiErr *providers.APIError
	if errors.As(err, &apiErr) {
		status = apiErr.StatusCode
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
