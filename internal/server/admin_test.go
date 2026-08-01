package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/google/uuid"
)

type fakeIssuer struct {
	key       auth.Key
	plaintext string
	err       error
	gotTeamID uuid.UUID
}

func (f *fakeIssuer) IssueKey(_ context.Context, teamID uuid.UUID, _ string, _ []string, _ *int64) (string, auth.Key, error) {
	f.gotTeamID = teamID
	return f.plaintext, f.key, f.err
}

type fakeRevoker struct {
	hash []byte
	err  error
}

func (f *fakeRevoker) RevokeKey(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return f.hash, f.err
}

type fakeInvalidator struct {
	calls int
}

func (f *fakeInvalidator) Invalidate(_ context.Context, _ []byte) error {
	f.calls++
	return nil
}

func TestHandleIssueKey_Success(t *testing.T) {
	teamID := uuid.New()
	issuer := &fakeIssuer{
		plaintext: "sk-vk-abc123",
		key:       auth.Key{ID: uuid.New(), TeamID: teamID, KeyPrefix: "sk-vk-a"},
	}
	deps := adminDeps{issuer: issuer}

	body, _ := json.Marshal(issueKeyRequest{TeamID: teamID.String(), Label: "ci-key"})
	req := httptest.NewRequest(http.MethodPost, "/admin/keys", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleIssueKey(deps)(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	if issuer.gotTeamID != teamID {
		t.Errorf("IssueKey called with team_id %v, want %v", issuer.gotTeamID, teamID)
	}

	var resp issueKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Key != "sk-vk-abc123" {
		t.Errorf("Key = %q, want the plaintext key", resp.Key)
	}
}

func TestHandleIssueKey_InvalidTeamID(t *testing.T) {
	deps := adminDeps{issuer: &fakeIssuer{}}
	body, _ := json.Marshal(issueKeyRequest{TeamID: "not-a-uuid"})
	req := httptest.NewRequest(http.MethodPost, "/admin/keys", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleIssueKey(deps)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleRevokeKey_Success(t *testing.T) {
	revoker := &fakeRevoker{hash: []byte("some-hash")}
	invalidator := &fakeInvalidator{}
	deps := adminDeps{revoker: revoker, invalidator: invalidator}

	req := httptest.NewRequest(http.MethodDelete, "/admin/keys/"+uuid.New().String(), nil)
	req.SetPathValue("id", uuid.New().String())
	rec := httptest.NewRecorder()
	handleRevokeKey(deps)(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if invalidator.calls != 1 {
		t.Errorf("Invalidate called %d times, want 1 (revocation must invalidate the cache immediately)", invalidator.calls)
	}
}

func TestHandleRevokeKey_NotFound(t *testing.T) {
	revoker := &fakeRevoker{err: auth.ErrNotFound}
	invalidator := &fakeInvalidator{}
	deps := adminDeps{revoker: revoker, invalidator: invalidator}

	req := httptest.NewRequest(http.MethodDelete, "/admin/keys/x", nil)
	req.SetPathValue("id", uuid.New().String())
	rec := httptest.NewRecorder()
	handleRevokeKey(deps)(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if invalidator.calls != 0 {
		t.Errorf("Invalidate called %d times, want 0 (nothing was actually revoked)", invalidator.calls)
	}
}

func TestHandleRevokeKey_InvalidID(t *testing.T) {
	deps := adminDeps{revoker: &fakeRevoker{}, invalidator: &fakeInvalidator{}}

	req := httptest.NewRequest(http.MethodDelete, "/admin/keys/not-a-uuid", nil)
	req.SetPathValue("id", "not-a-uuid")
	rec := httptest.NewRecorder()
	handleRevokeKey(deps)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

type fakePurger struct {
	gotTenant string
	deleted   int
	err       error
}

func (f *fakePurger) PurgeTenant(_ context.Context, tenantID string) (int, error) {
	f.gotTenant = tenantID
	return f.deleted, f.err
}

func TestHandlePurgeCache_Success(t *testing.T) {
	purger := &fakePurger{deleted: 3}
	deps := adminDeps{purger: purger}

	body, _ := json.Marshal(purgeCacheRequest{Tenant: "org-123"})
	req := httptest.NewRequest(http.MethodPost, "/admin/cache/purge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handlePurgeCache(deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if purger.gotTenant != "org-123" {
		t.Errorf("PurgeTenant called with %q, want %q", purger.gotTenant, "org-123")
	}
	var resp purgeCacheResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Deleted != 3 {
		t.Errorf("Deleted = %d, want 3", resp.Deleted)
	}
}

func TestHandlePurgeCache_MissingTenant(t *testing.T) {
	deps := adminDeps{purger: &fakePurger{}}

	body, _ := json.Marshal(purgeCacheRequest{})
	req := httptest.NewRequest(http.MethodPost, "/admin/cache/purge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handlePurgeCache(deps)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}
