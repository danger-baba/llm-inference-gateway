package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleModels(t *testing.T) {
	handler := handleModels([]string{"fast", "cheap"})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp modelsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("Object = %q, want %q", resp.Object, "list")
	}
	if len(resp.Data) != 2 {
		t.Fatalf("Data = %d entries, want 2", len(resp.Data))
	}
	if resp.Data[0].ID != "fast" || resp.Data[1].ID != "cheap" {
		t.Errorf("Data = %+v, want ids in the order passed in", resp.Data)
	}
}
