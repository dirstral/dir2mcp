package index

import (
	"context"
	"encoding/gob"
	"errors"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Persisted snapshot basenames (issue #247). The ".v2" marker distinguishes
// the payload-carrying gob shape (hnswSnapshot) from the legacy bare-map files
// (vectors_{text,code}.hnsw); a missing v2 file is a fresh index, repopulated by
// reindex, so legacy files are never decoded.
const (
	TextIndexFileName = "vectors_text.v2.hnsw"
	CodeIndexFileName = "vectors_code.v2.hnsw"
)

// LegacyIndexFileNames are the pre-#247 bare-map snapshot basenames. reindex
// removes them alongside the current files so a stale legacy snapshot cannot
// linger after an upgrade.
var LegacyIndexFileNames = []string{"vectors_text.hnsw", "vectors_code.hnsw"}

// compile-time assertions that HNSWIndex satisfies the core Index contract and
// the optional Persistable / FilteringIndex capabilities (issue #247).
var (
	_ model.Index          = (*HNSWIndex)(nil)
	_ model.Persistable    = (*HNSWIndex)(nil)
	_ model.FilteringIndex = (*HNSWIndex)(nil)
)

type HNSWIndex struct {
	path     string
	mu       sync.RWMutex
	vectors  map[uint64][]float32
	payloads map[uint64]model.IndexPayload
	identity string

	// Logger is optional; if non-nil its Printf method will be used for
	// informational messages. When nil the standard library's log package
	// is used.
	Logger *log.Logger

	// Metrics collects optional counters that callers can inspect. The
	// zero-value of HNSWIndexMetrics is usable, so callers may simply pass a
	// pointer and read it after operations. If Metrics is nil nothing is
	// incremented.
	Metrics *HNSWIndexMetrics
}

// HNSWIndexMetrics holds counters gathered by an index instance.
//
// Additional fields may be added in future if callers require them.
// Only the dimension mismatch counter is currently defined.
type HNSWIndexMetrics struct {
	// DimensionMismatch tracks how many times a provided query vector
	// didn't match the length of a stored vector.  We use atomic.Int64
	// instead of a plain int64 so that metrics can be read concurrently
	// on 32‑bit architectures without data races.
	DimensionMismatch atomic.Int64
}

// hnswSnapshot is the on-disk gob shape (issue #247). It is a distinct,
// self-describing struct rather than a bare map[uint64][]float32 so the payload
// metadata and embed identity persist alongside the vectors. To avoid gob
// decode ambiguity with the legacy bare-map format, the persisted filename is
// versioned (see NewHNSWIndex / vectors_*.v2.hnsw); a missing v2 file is treated
// as a fresh index that reindex will repopulate.
type hnswSnapshot struct {
	Vectors  map[uint64][]float32
	Payloads map[uint64]model.IndexPayload
	Identity string
}

// NewHNSWIndex creates an empty in-memory HNSW index. The optional
// path argument is used by Save/Load; if non-empty those methods will
// persist to the given file.
func NewHNSWIndex(path string) *HNSWIndex {
	return &HNSWIndex{
		path:     path,
		vectors:  make(map[uint64][]float32),
		payloads: make(map[uint64]model.IndexPayload),
	}
}

// Upsert stores (or replaces) the vector and its payload, keyed by
// payload.ChunkID.
func (i *HNSWIndex) Upsert(ctx context.Context, vector []float32, payload model.IndexPayload) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(vector) == 0 {
		return errors.New("vector cannot be empty")
	}
	if payload.ChunkID == 0 {
		return errors.New("payload chunk_id cannot be zero")
	}

	copied := make([]float32, len(vector))
	copy(copied, vector)

	i.mu.Lock()
	defer i.mu.Unlock()
	i.vectors[payload.ChunkID] = copied
	i.payloads[payload.ChunkID] = payload
	return nil
}

// Delete removes the vectors and payloads for the given chunk IDs. Unknown IDs
// are ignored.
func (i *HNSWIndex) Delete(ctx context.Context, chunkIDs []uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(chunkIDs) == 0 {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	for _, id := range chunkIDs {
		delete(i.vectors, id)
		delete(i.payloads, id)
	}
	return nil
}

// scoredCandidate pairs a chunk_id with its cosine score during search.
type scoredCandidate struct {
	chunkID uint64
	score   float32
	payload model.IndexPayload
}

// Search returns the k best matches for vector, filtered by filter. The filter
// is applied inline (CanFilter is always true for the pure-Go HNSW), so callers
// may push it down rather than overfetch-then-filter.
func (i *HNSWIndex) Search(ctx context.Context, vector []float32, k int, filter model.Filter) ([]model.IndexHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(vector) == 0 {
		return nil, errors.New("query vector cannot be empty")
	}
	if k <= 0 {
		return []model.IndexHit{}, nil
	}

	candidates, mismatches := i.collectCandidates(vector, filter)
	for _, m := range mismatches {
		i.logf("dimension mismatch: chunk_id=%d candidate_len=%d query_len=%d", m.chunkID, m.candLen, m.queryLen)
	}

	scored := make([]scoredCandidate, 0, len(candidates))
	for _, c := range candidates {
		c.score = cosineSimilarity(vector, c.vector)
		scored = append(scored, scoredCandidate{chunkID: c.chunkID, score: c.score, payload: c.payload})
	}

	const eps = 1e-6
	sort.Slice(scored, func(a, b int) bool {
		diff := math.Abs(float64(scored[a].score) - float64(scored[b].score))
		if diff <= eps {
			return scored[a].chunkID < scored[b].chunkID
		}
		return scored[a].score > scored[b].score
	})

	if len(scored) > k {
		scored = scored[:k]
	}
	hits := make([]model.IndexHit, len(scored))
	for idx, s := range scored {
		hits[idx] = model.IndexHit{ChunkID: s.chunkID, Score: s.score, Payload: s.payload}
	}
	return hits, nil
}

// searchCandidate carries a copied vector + payload for scoring outside the
// lock.
type searchCandidate struct {
	chunkID uint64
	vector  []float32
	score   float32
	payload model.IndexPayload
}

type dimMismatch struct {
	chunkID  uint64
	candLen  int
	queryLen int
}

// collectCandidates snapshots, under the read lock, the vectors whose dimension
// matches the query and whose payload satisfies the filter. Dimension
// mismatches are returned for logging outside the lock.
func (i *HNSWIndex) collectCandidates(vector []float32, filter model.Filter) ([]searchCandidate, []dimMismatch) {
	var (
		candidates []searchCandidate
		mismatches []dimMismatch
	)
	applyFilter := !filter.IsZero()

	i.mu.RLock()
	for id, cand := range i.vectors {
		if len(cand) != len(vector) {
			mismatches = append(mismatches, dimMismatch{id, len(cand), len(vector)})
			if i.Metrics != nil {
				i.Metrics.DimensionMismatch.Add(1)
			}
			continue
		}
		payload := i.payloads[id]
		if applyFilter && !filter.Match(payload) {
			continue
		}
		copyVec := make([]float32, len(cand))
		copy(copyVec, cand)
		candidates = append(candidates, searchCandidate{chunkID: id, vector: copyVec, payload: payload})
	}
	i.mu.RUnlock()
	return candidates, mismatches
}

// CanFilter reports whether the backend can evaluate the filter itself. The
// pure-Go HNSW evaluates every predicate inline, so it always can.
func (i *HNSWIndex) CanFilter(filter model.Filter) bool {
	return true
}

// Identity returns the recorded corpus-lifetime embed identity, or "" when the
// index is fresh.
func (i *HNSWIndex) Identity(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.identity, nil
}

// Reset clears all vectors/payloads and records identity as the new
// corpus-lifetime embed identity.
func (i *HNSWIndex) Reset(ctx context.Context, identity string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.vectors = make(map[uint64][]float32)
	i.payloads = make(map[uint64]model.IndexPayload)
	i.identity = identity
	return nil
}

// logf is a small helper that routes messages to the configured logger or
// the global log package. It mirrors the helper defined on EmbeddingWorker.
func (i *HNSWIndex) logf(format string, args ...interface{}) {
	if i != nil && i.Logger != nil {
		i.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Save snapshots the index (vectors + payloads + identity) to path via gob,
// using an atomic temp-file rename. See hnswSnapshot for the format rationale.
func (i *HNSWIndex) Save(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if path == "" {
		path = i.path
	}
	if path == "" {
		return errors.New("path is required")
	}

	tmpPath := path + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

	// Snapshot under the read lock so concurrent Upsert/Delete don't block on
	// the gob encoding and file I/O, and so we deep-copy each slice rather than
	// race with callers who might mutate the originals later.
	snapshot := i.snapshot()

	enc := gob.NewEncoder(file)
	if err := enc.Encode(snapshot); err != nil {
		closeErr := file.Close()
		_ = os.Remove(tmpPath)
		return errors.Join(err, closeErr)
	}
	if err := file.Sync(); err != nil {
		closeErr := file.Close()
		_ = os.Remove(tmpPath)
		return errors.Join(err, closeErr)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// snapshot deep-copies the index state under the read lock for persistence.
func (i *HNSWIndex) snapshot() hnswSnapshot {
	i.mu.RLock()
	defer i.mu.RUnlock()
	vectors := make(map[uint64][]float32, len(i.vectors))
	for k, v := range i.vectors {
		copied := make([]float32, len(v))
		copy(copied, v)
		vectors[k] = copied
	}
	payloads := make(map[uint64]model.IndexPayload, len(i.payloads))
	for k, v := range i.payloads {
		payloads[k] = v
	}
	return hnswSnapshot{Vectors: vectors, Payloads: payloads, Identity: i.identity}
}

// Load restores the index from a v2 snapshot file. A missing file is treated as
// a fresh index (no error) — a legacy bare-map file under the old name is simply
// not present at the v2 path, so the corpus is repopulated on the next reindex.
func (i *HNSWIndex) Load(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if path == "" {
		path = i.path
	}
	if path == "" {
		return errors.New("path is required")
	}

	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = file.Close() }()

	var snapshot hnswSnapshot
	dec := gob.NewDecoder(file)
	if err := dec.Decode(&snapshot); err != nil {
		return err
	}
	if snapshot.Vectors == nil {
		snapshot.Vectors = make(map[uint64][]float32)
	}
	if snapshot.Payloads == nil {
		snapshot.Payloads = make(map[uint64]model.IndexPayload)
	}

	i.mu.Lock()
	i.vectors = snapshot.Vectors
	i.payloads = snapshot.Payloads
	i.identity = snapshot.Identity
	i.mu.Unlock()
	return nil
}

func (i *HNSWIndex) Close() error {
	return nil
}

func cosineSimilarity(a, b []float32) float32 {
	var dot float32
	var magA float32
	var magB float32

	for idx := range a {
		dot += a[idx] * b[idx]
		magA += a[idx] * a[idx]
		magB += b[idx] * b[idx]
	}

	if magA == 0 || magB == 0 {
		return 0
	}

	return dot / sqrt32(magA*magB)
}

func sqrt32(v float32) float32 {
	// use standard library math for correctness and simplicity; casting
	// handles 0 implicitly.
	return float32(math.Sqrt(float64(v)))
}
