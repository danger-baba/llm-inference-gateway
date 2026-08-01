package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/google/uuid"
)

type fakeIdentityResolver struct {
	identity auth.Identity
	err      error
}

func (f fakeIdentityResolver) Resolve(_ context.Context, _ string) (auth.Identity, error) {
	return f.identity, f.err
}

func TestWithBearerAuth_MissingHeader(t *testing.T) {
	resolver := fakeIdentityResolver{}
	handler := withBearerAuth(resolver, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("downstream handler should not run without a valid Authorization header")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWithBearerAuth_MalformedHeader(t *testing.T) {
	resolver := fakeIdentityResolver{}
	handler := withBearerAuth(resolver, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("downstream handler should not run")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "NotBearer sk-vk-abc")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWithBearerAuth_NotFoundOrRevoked(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"not found", auth.ErrNotFound},
		{"revoked", auth.ErrRevoked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := fakeIdentityResolver{err: tt.err}
			handler := withBearerAuth(resolver, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("downstream handler should not run")
			}))

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set("Authorization", "Bearer sk-vk-whatever")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestWithBearerAuth_InfraFailureIsNot401(t *testing.T) {
	resolver := fakeIdentityResolver{err: context.DeadlineExceeded} // stands in for "Postgres itself failed"
	handler := withBearerAuth(resolver, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("downstream handler should not run")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-vk-whatever")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (an infra failure isn't the same claim as bad credentials)", rec.Code)
	}
}

func TestWithBearerAuth_SuccessAttachesIdentityToContext(t *testing.T) {
	want := auth.Identity{OrgID: uuid.New(), TeamID: uuid.New(), KeyID: uuid.New(), AllowedModels: []string{"fast"}}
	resolver := fakeIdentityResolver{identity: want}

	var gotOK bool
	var got auth.Identity
	handler := withBearerAuth(resolver, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotOK = identityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-vk-whatever")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !gotOK {
		t.Fatal("identityFromContext() ok = false, want true")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("identityFromContext() = %+v, want %+v", got, want)
	}
}
