package semantic

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

func testQuery(vector []float32) Query {
	return Query{
		TenantID: "tenant-a", Model: "fast",
		ToolsCanonical: "", ResponseFormatCanonical: "",
		Vector: vector,
	}
}

func testResponse(id string) *providers.CanonicalResponse {
	return &providers.CanonicalResponse{ID: id}
}

func TestGet_MissOnEmptyStore(t *testing.T) {
	s := NewStore(16, 20, 100, 0.95, time.Hour)
	_, _, hit := s.Get(testQuery([]float32{1, 0, 0}))
	if hit {
		t.Error("Get() on empty store hit = true, want false")
	}
}

func TestSetThenGet_IdenticalVectorHits(t *testing.T) {
	s := NewStore(16, 20, 100, 0.95, time.Hour)
	q := testQuery([]float32{1, 0, 0})
	s.Set(q, testResponse("abc"))

	resp, sim, hit := s.Get(q)
	if !hit {
		t.Fatal("Get() hit = false, want true for an identical vector")
	}
	if resp.ID != "abc" {
		t.Errorf("resp.ID = %q, want %q", resp.ID, "abc")
	}
	if sim < 0.999 {
		t.Errorf("similarity = %v, want ~1.0", sim)
	}
}

func TestGet_BelowThresholdMisses(t *testing.T) {
	s := NewStore(16, 20, 100, 0.95, time.Hour)
	s.Set(testQuery([]float32{1, 0, 0}), testResponse("abc"))

	// Orthogonal vector: cosine similarity 0, far below the 0.95 threshold.
	_, _, hit := s.Get(testQuery([]float32{0, 1, 0}))
	if hit {
		t.Error("Get() hit = true, want false (below threshold)")
	}
}

func TestGet_GuardRailRejectsModelMismatch(t *testing.T) {
	s := NewStore(16, 20, 100, 0.95, time.Hour)
	stored := testQuery([]float32{1, 0, 0})
	stored.Model = "fast"
	s.Set(stored, testResponse("abc"))

	lookup := testQuery([]float32{1, 0, 0})
	lookup.Model = "cheap" // same vector, different model
	_, _, hit := s.Get(lookup)
	if hit {
		t.Error("Get() hit = true, want false (model mismatch must reject despite similarity 1.0)")
	}
}

func TestGet_GuardRailRejectsResponseFormatMismatch(t *testing.T) {
	s := NewStore(16, 20, 100, 0.95, time.Hour)
	stored := testQuery([]float32{1, 0, 0})
	stored.ResponseFormatCanonical = `{"type":"text"}`
	s.Set(stored, testResponse("abc"))

	lookup := testQuery([]float32{1, 0, 0})
	lookup.ResponseFormatCanonical = `{"type":"json_object"}`
	_, _, hit := s.Get(lookup)
	if hit {
		t.Error("Get() hit = true, want false (response_format mismatch must reject)")
	}
}

func TestGet_GuardRailRejectsToolsMismatch(t *testing.T) {
	s := NewStore(16, 20, 100, 0.95, time.Hour)
	stored := testQuery([]float32{1, 0, 0})
	stored.ToolsCanonical = `[{"name":"a"}]`
	s.Set(stored, testResponse("abc"))

	lookup := testQuery([]float32{1, 0, 0})
	lookup.ToolsCanonical = `[{"name":"b"}]`
	_, _, hit := s.Get(lookup)
	if hit {
		t.Error("Get() hit = true, want false (tools mismatch must reject)")
	}
}

func TestGet_TenantIsolation(t *testing.T) {
	s := NewStore(16, 20, 100, 0.95, time.Hour)
	stored := testQuery([]float32{1, 0, 0})
	stored.TenantID = "tenant-a"
	s.Set(stored, testResponse("abc"))

	lookup := testQuery([]float32{1, 0, 0})
	lookup.TenantID = "tenant-b"
	_, _, hit := s.Get(lookup)
	if hit {
		t.Error("Get() hit = true, want false (a different tenant must never see this entry)")
	}
}

func TestGet_ExpiredEntryNotServed(t *testing.T) {
	s := NewStore(16, 20, 100, 0.95, time.Millisecond)
	q := testQuery([]float32{1, 0, 0})
	s.Set(q, testResponse("abc"))

	time.Sleep(10 * time.Millisecond)

	_, _, hit := s.Get(q)
	if hit {
		t.Error("Get() hit = true, want false (entry has expired)")
	}
}

func TestSet_LRUEvictionAtCapacity(t *testing.T) {
	s := NewStore(16, 20, 2, 0.95, time.Hour) // maxVectors=2

	first := testQuery([]float32{1, 0, 0})
	first.Model = "first"
	s.Set(first, testResponse("first"))

	second := testQuery([]float32{0, 1, 0})
	second.Model = "second"
	s.Set(second, testResponse("second"))

	third := testQuery([]float32{0, 0, 1})
	third.Model = "third"
	s.Set(third, testResponse("third")) // should evict "first" (least recently used)

	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", s.Len())
	}
	if _, _, hit := s.Get(first); hit {
		t.Error("the oldest entry should have been evicted, but still hits")
	}
	if _, _, hit := s.Get(third); !hit {
		t.Error("the newest entry should still be present")
	}
}

func TestSet_GetTouchesLRUOrder(t *testing.T) {
	s := NewStore(16, 20, 2, 0.95, time.Hour)

	a := testQuery([]float32{1, 0, 0})
	a.Model = "a"
	s.Set(a, testResponse("a"))

	b := testQuery([]float32{0, 1, 0})
	b.Model = "b"
	s.Set(b, testResponse("b"))

	// Touch "a" so "b" becomes the least-recently-used one instead.
	s.Get(a)

	c := testQuery([]float32{0, 0, 1})
	c.Model = "c"
	s.Set(c, testResponse("c")) // should evict "b", not "a"

	if _, _, hit := s.Get(a); !hit {
		t.Error("\"a\" was recently touched and should not have been evicted")
	}
	if _, _, hit := s.Get(b); hit {
		t.Error("\"b\" was the least-recently-used entry and should have been evicted")
	}
}

// TestSet_HeavyEvictionChurnDoesNotCrash exercises far more evictions than
// maxVectors, which is exactly the pattern that triggers coder/hnsw's
// Delete/Search nil-entry-point bug (docs/adr/0012) if evictOldest ever
// called the library's Delete directly instead of working around it.
func TestSet_HeavyEvictionChurnDoesNotCrash(t *testing.T) {
	s := NewStore(16, 20, 5, 0.95, time.Hour)

	for i := 0; i < 500; i++ {
		q := testQuery([]float32{float32(i), float32(i + 1), float32(i + 2)})
		s.Set(q, testResponse("x"))
		// Also exercise Get on every iteration, since that's what
		// actually calls the buggy Search path.
		s.Get(testQuery([]float32{1, 2, 3}))
	}

	if s.Len() != 5 {
		t.Errorf("Len() = %d, want 5 (maxVectors)", s.Len())
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "semantic.gob")

	original := NewStore(16, 20, 100, 0.95, time.Hour)
	q := testQuery([]float32{1, 0, 0})
	original.Set(q, testResponse("abc"))

	if err := original.SaveToDisk(path); err != nil {
		t.Fatalf("SaveToDisk() unexpected error: %v", err)
	}

	restored := NewStore(16, 20, 100, 0.95, time.Hour)
	if err := restored.LoadFromDisk(path); err != nil {
		t.Fatalf("LoadFromDisk() unexpected error: %v", err)
	}

	resp, _, hit := restored.Get(q)
	if !hit {
		t.Fatal("Get() after LoadFromDisk() hit = false, want true")
	}
	if resp.ID != "abc" {
		t.Errorf("resp.ID = %q, want %q", resp.ID, "abc")
	}
}

func TestLoadFromDisk_MissingFileIsNotAnError(t *testing.T) {
	s := NewStore(16, 20, 100, 0.95, time.Hour)
	if err := s.LoadFromDisk(filepath.Join(t.TempDir(), "does-not-exist.gob")); err != nil {
		t.Errorf("LoadFromDisk() unexpected error for a missing file: %v", err)
	}
}

func TestSaveLoad_ExpiredEntriesAreDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "semantic.gob")

	original := NewStore(16, 20, 100, 0.95, time.Millisecond)
	q := testQuery([]float32{1, 0, 0})
	original.Set(q, testResponse("abc"))
	time.Sleep(10 * time.Millisecond)

	if err := original.SaveToDisk(path); err != nil {
		t.Fatalf("SaveToDisk() unexpected error: %v", err)
	}

	restored := NewStore(16, 20, 100, 0.95, time.Hour)
	if err := restored.LoadFromDisk(path); err != nil {
		t.Fatalf("LoadFromDisk() unexpected error: %v", err)
	}
	if restored.Len() != 0 {
		t.Errorf("Len() = %d, want 0 (the persisted entry had already expired)", restored.Len())
	}
}
