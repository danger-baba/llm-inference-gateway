// Package semantic implements the Tier-2 semantic cache: an in-memory
// HNSW index over locally-computed sentence embeddings, consulted only
// on a Tier-1 (exact-match) miss.
package semantic

import (
	"container/list"
	"encoding/gob"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/coder/hnsw"
	"github.com/google/uuid"

	"github.com/danger-baba/llm-inference-gateway/internal/embedding"
	"github.com/danger-baba/llm-inference-gateway/internal/providers"
)

// searchK is how many nearest neighbours to pull from the graph before
// filtering by tenant and the guard rail. The graph holds every tenant's
// vectors together (see docs/adr/0012 for why), so a generous k keeps a
// same-tenant match from being crowded out by other tenants' nearer ones.
const searchK = 20

type entry struct {
	response                *providers.CanonicalResponse
	tenantID                string
	model                   string
	toolsCanonical          string
	responseFormatCanonical string
	vector                  []float32
	expiresAt               time.Time
	lruElem                 *list.Element
}

// Query carries a candidate lookup's embedding plus the exact-match guard
// rail fields: similarity alone is never sufficient (README, Tier-2 cache).
type Query struct {
	TenantID                string
	Model                   string
	ToolsCanonical          string
	ResponseFormatCanonical string
	Vector                  []float32
}

// Store is an LRU-bounded HNSW index. All methods are safe for concurrent
// use.
type Store struct {
	mu         sync.Mutex
	graph      *hnsw.Graph[string]
	entries    map[string]*entry
	lru        *list.List // front = most recently used
	maxVectors int
	ttl        time.Duration
	threshold  float32
}

func NewStore(m, efSearch, maxVectors int, threshold float32, ttl time.Duration) *Store {
	g := hnsw.NewGraph[string]()
	g.M = m
	g.EfSearch = efSearch
	return &Store{
		graph:      g,
		entries:    make(map[string]*entry),
		lru:        list.New(),
		maxVectors: maxVectors,
		ttl:        ttl,
		threshold:  threshold,
	}
}

// Get returns the nearest same-tenant, guard-rail-matching entry at or
// above the configured cosine threshold, if any. Search results are
// sorted by decreasing similarity, so the first candidate under threshold
// ends the search — nothing further out can qualify either.
func (s *Store) Get(q Query) (*providers.CanonicalResponse, float32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) == 0 {
		return nil, 0, false
	}

	now := time.Now()
	for _, node := range s.graph.Search(q.Vector, searchK) {
		e, ok := s.entries[node.Key]
		if !ok {
			continue
		}
		sim := embedding.CosineSimilarity(q.Vector, node.Value)
		if sim < s.threshold {
			break
		}
		if now.After(e.expiresAt) {
			continue
		}
		if e.tenantID != q.TenantID || e.model != q.Model ||
			e.toolsCanonical != q.ToolsCanonical || e.responseFormatCanonical != q.ResponseFormatCanonical {
			continue
		}
		s.lru.MoveToFront(e.lruElem)
		return e.response, sim, true
	}
	return nil, 0, false
}

// Set stores resp under vector with q's guard-rail fields, evicting the
// least-recently-used entry if this insert pushes the store past
// maxVectors. It expires after s.ttl, or after ttlOverride when non-nil --
// a per-request hint honored by the caller (README, per-request cache
// TTL; docs/adr/0017). Callers never pass an override <= 0: that means
// "don't cache this at all," which is the caller's job to check before
// ever reaching Set.
func (s *Store) Set(q Query, resp *providers.CanonicalResponse, ttlOverride *time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ttl := s.ttl
	if ttlOverride != nil {
		ttl = *ttlOverride
	}

	key := uuid.NewString()
	e := &entry{
		response:                resp,
		tenantID:                q.TenantID,
		model:                   q.Model,
		toolsCanonical:          q.ToolsCanonical,
		responseFormatCanonical: q.ResponseFormatCanonical,
		vector:                  q.Vector,
		expiresAt:               time.Now().Add(ttl),
	}
	e.lruElem = s.lru.PushFront(key)
	s.entries[key] = e
	s.graph.Add(hnsw.MakeNode(key, q.Vector))

	for len(s.entries) > s.maxVectors {
		s.evictOldest()
	}
}

// evictOldest drops the least-recently-used entry from the bookkeeping
// map, but deliberately does not call the underlying graph's Delete: that
// method has a real bug (see docs/adr/0012) where deleting the sole
// remaining node in an upper layer leaves Search with a nil entry point,
// crashing on the next call. The orphaned graph node is otherwise
// harmless — Get already skips any search result whose key isn't in
// s.entries — so it's left in place, and rebuildGraphIfNeeded periodically
// clears the accumulated orphans in one pass instead.
func (s *Store) evictOldest() {
	oldest := s.lru.Back()
	if oldest == nil {
		return
	}
	key := oldest.Value.(string)
	s.lru.Remove(oldest)
	delete(s.entries, key)
	s.rebuildGraphIfNeeded()
}

// rebuildGraphIfNeeded discards accumulated orphaned nodes once the
// underlying graph has grown to roughly twice the number of live entries,
// bounding worst-case memory to about 2x maxVectors instead of letting it
// grow without limit over a long-running process with heavy eviction churn.
func (s *Store) rebuildGraphIfNeeded() {
	orphans := s.graph.Len() - len(s.entries)
	if orphans <= s.maxVectors {
		return
	}
	fresh := hnsw.NewGraph[string]()
	fresh.M = s.graph.M
	fresh.EfSearch = s.graph.EfSearch
	for key, e := range s.entries {
		fresh.Add(hnsw.MakeNode(key, e.vector))
	}
	s.graph = fresh
}

// Len reports the current number of stored vectors.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// persistedEntry is the on-disk shape written by SaveToDisk. The graph
// itself isn't serialized directly — on load, every non-expired vector is
// just re-added to a fresh graph, which is simpler than keeping two
// serialization formats (the index and the metadata) in sync.
type persistedEntry struct {
	Key                     string
	Response                *providers.CanonicalResponse
	TenantID                string
	Model                   string
	ToolsCanonical          string
	ResponseFormatCanonical string
	Vector                  []float32
	ExpiresAt               time.Time
}

func (s *Store) SaveToDisk(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	persisted := make([]persistedEntry, 0, len(s.entries))
	for key, e := range s.entries {
		persisted = append(persisted, persistedEntry{
			Key: key, Response: e.response, TenantID: e.tenantID, Model: e.model,
			ToolsCanonical: e.toolsCanonical, ResponseFormatCanonical: e.responseFormatCanonical,
			Vector: e.vector, ExpiresAt: e.expiresAt,
		})
	}
	return gob.NewEncoder(f).Encode(persisted)
}

// LoadFromDisk restores entries saved by SaveToDisk. A missing file is
// not an error — there's simply nothing to restore yet, which is the
// normal case on a fresh deployment.
func (s *Store) LoadFromDisk(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	var persisted []persistedEntry
	if err := gob.NewDecoder(f).Decode(&persisted); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, pe := range persisted {
		if now.After(pe.ExpiresAt) {
			continue
		}
		e := &entry{
			response: pe.Response, tenantID: pe.TenantID, model: pe.Model,
			toolsCanonical: pe.ToolsCanonical, responseFormatCanonical: pe.ResponseFormatCanonical,
			vector: pe.Vector, expiresAt: pe.ExpiresAt,
		}
		e.lruElem = s.lru.PushFront(pe.Key)
		s.entries[pe.Key] = e
		s.graph.Add(hnsw.MakeNode(pe.Key, pe.Vector))
	}
	return nil
}
