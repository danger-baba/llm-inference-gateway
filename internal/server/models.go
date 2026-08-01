package server

import (
	"encoding/json"
	"net/http"
)

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type modelsResponse struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

// handleModels exposes the client-facing model_aliases keys, not raw
// provider model strings: clients only ever request aliases, so that's the
// vocabulary this union should be expressed in. See docs/adr/0004.
func handleModels(aliases []string) http.HandlerFunc {
	data := make([]modelObject, len(aliases))
	for i, id := range aliases {
		data[i] = modelObject{ID: id, Object: "model", OwnedBy: "gateway"}
	}
	resp := modelsResponse{Object: "list", Data: data}

	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
