package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/auth"
	"github.com/danger-baba/llm-inference-gateway/internal/ledger"
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

type fakeUsageAggregator struct {
	got struct {
		scope        ledger.Scope
		id           uuid.UUID
		since, until time.Time
	}
	result ledger.UsageAggregate
	err    error
}

func (f *fakeUsageAggregator) Aggregate(_ context.Context, scope ledger.Scope, id uuid.UUID, since, until time.Time) (ledger.UsageAggregate, error) {
	f.got.scope, f.got.id, f.got.since, f.got.until = scope, id, since, until
	return f.result, f.err
}

func TestHandleUsage_Success(t *testing.T) {
	orgID := uuid.New()
	agg := &fakeUsageAggregator{result: ledger.UsageAggregate{
		Requests: 42, PromptTokens: 100, CompletionTokens: 200, TokensSaved: 30, CostUSD: 1.23,
		CacheHits: map[string]int64{"none": 40, "exact": 1, "semantic": 1},
	}}
	deps := adminDeps{usage: agg}

	req := httptest.NewRequest(http.MethodGet, "/admin/usage?scope=org&id="+orgID.String(), nil)
	rec := httptest.NewRecorder()
	handleUsage(deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if agg.got.scope != ledger.ScopeOrg || agg.got.id != orgID {
		t.Errorf("Aggregate called with scope=%v id=%v, want org/%v", agg.got.scope, agg.got.id, orgID)
	}
	if !agg.got.since.Before(agg.got.until) {
		t.Errorf("since (%v) is not before until (%v) with default window", agg.got.since, agg.got.until)
	}
	if got := agg.got.until.Sub(agg.got.since); got != 24*time.Hour {
		t.Errorf("default window = %v, want 24h", got)
	}

	var resp usageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Requests != 42 || resp.CostUSD != 1.23 || resp.TokensSaved != 30 {
		t.Errorf("resp = %+v, want Requests=42 CostUSD=1.23 TokensSaved=30", resp)
	}
	if resp.CacheHits["exact"] != 1 {
		t.Errorf("resp.CacheHits[exact] = %d, want 1", resp.CacheHits["exact"])
	}
}

func TestHandleUsage_ExplicitWindow(t *testing.T) {
	orgID := uuid.New()
	agg := &fakeUsageAggregator{}
	deps := adminDeps{usage: agg}

	since := "2026-01-01T00:00:00Z"
	until := "2026-02-01T00:00:00Z"
	req := httptest.NewRequest(http.MethodGet, "/admin/usage?scope=org&id="+orgID.String()+"&since="+since+"&until="+until, nil)
	rec := httptest.NewRecorder()
	handleUsage(deps)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	wantSince, _ := time.Parse(time.RFC3339, since)
	wantUntil, _ := time.Parse(time.RFC3339, until)
	if !agg.got.since.Equal(wantSince) || !agg.got.until.Equal(wantUntil) {
		t.Errorf("window = [%v, %v], want [%v, %v]", agg.got.since, agg.got.until, wantSince, wantUntil)
	}
}

func TestHandleUsage_InvalidScope(t *testing.T) {
	deps := adminDeps{usage: &fakeUsageAggregator{}}
	req := httptest.NewRequest(http.MethodGet, "/admin/usage?scope=bogus&id="+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handleUsage(deps)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleUsage_InvalidID(t *testing.T) {
	deps := adminDeps{usage: &fakeUsageAggregator{}}
	req := httptest.NewRequest(http.MethodGet, "/admin/usage?scope=org&id=not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handleUsage(deps)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleUsage_SinceAfterUntilRejected(t *testing.T) {
	deps := adminDeps{usage: &fakeUsageAggregator{}}
	req := httptest.NewRequest(http.MethodGet, "/admin/usage?scope=org&id="+uuid.New().String()+"&since=2026-02-01T00:00:00Z&until=2026-01-01T00:00:00Z", nil)
	rec := httptest.NewRecorder()
	handleUsage(deps)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestHandleUsage_NoAggregatorConfigured(t *testing.T) {
	deps := adminDeps{} // usage left nil, as when Postgres isn't configured
	req := httptest.NewRequest(http.MethodGet, "/admin/usage?scope=org&id="+uuid.New().String(), nil)
	rec := httptest.NewRecorder()
	handleUsage(deps)(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}
