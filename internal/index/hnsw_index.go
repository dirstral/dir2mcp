package index

import (
	"context"
	"encoding/gob"
	"errors"
	"log"
	"math"
	"os"
	"path/filepath"
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

	// version is a monotonically increasing mutation counter, bumped under the
	// write lock on every Upsert/Delete/Reset. savedVersion records the version
	// captured by the last successful Save. When they are equal the in-memory
	// state already matches the on-disk snapshot, so the periodic autosave can
	// skip rewriting the whole index (issue #429 F7). Both are guarded by mu;
	// capturing version together with the snapshot (under the read lock) and only
	// advancing savedVersion after a durable write keeps the dirty check race-free
	// and never drops a concurrent write that landed mid-save.
	version      uint64
	savedVersion uint64

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
	i.version++
	return nil
}

// HasVectors reports, for each requested chunk ID, whether a vector is currently
// present in the index. It implements VectorPresence (issue #402 A2): the
// in-memory HNSW's durability is the periodic snapshot (~15s autosave), so an
// ungraceful crash between MarkEmbedded and the next SaveAll silently drops
// vectors that sqlite still counts embedded. Startup reconciliation calls this
// to re-pend those chunks. A nil/empty request returns an empty map.
func (i *HNSWIndex) HasVectors(ctx context.Context, chunkIDs []uint64) (map[uint64]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	present := make(map[uint64]bool, len(chunkIDs))
	i.mu.RLock()
	defer i.mu.RUnlock()
	for _, id := range chunkIDs {
		_, ok := i.vectors[id]
		present[id] = ok
	}
	return present, nil
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
	i.version++
	return nil
}

// scoreEps is the score-equality tolerance for the ranking tiebreak: two scores
// within eps are treated as equal and ordered by ascending chunk_id, giving a
// deterministic total order over realistic (well-separated) embedding scores.
const scoreEps = 1e-6

// scoredCandidate pairs a chunk_id with its cosine score during search.
type scoredCandidate struct {
	chunkID uint64
	score   float32
	payload model.IndexPayload
}

// candidateBefore reports whether candidate a ranks strictly before b: higher
// score first, with an eps-tolerant ascending chunk_id tiebreak. It is the sole
// ranking comparator, shared by the top-k heap's eviction decision and the final
// ordering of the retained hits, so the heap selects exactly the same k
// candidates the previous full O(N log N) sort would have kept (issue #429 F1).
func candidateBefore(aScore, bScore float32, aID, bID uint64) bool {
	if math.Abs(float64(aScore)-float64(bScore)) <= scoreEps {
		return aID < bID
	}
	return aScore > bScore
}

// Search returns the k best matches for vector, filtered by filter. The filter
// is applied inline (CanFilter is always true for the pure-Go HNSW), so callers
// may push it down rather than overfetch-then-filter.
//
// Scoring runs in place under the read lock — each stored vector is scored where
// it lives rather than copied into a per-query slice first (issue #429 F2) — and
// only the running top-k is retained in a bounded min-heap rather than sorting
// every scored candidate (issue #429 F1). The retained k are then sorted with
// candidateBefore, yielding results identical to the previous full-sort-then-
// truncate for any input the comparator totally orders.
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

	scored, mismatches := i.scoreTopK(vector, k, filter)
	for _, m := range mismatches {
		i.logf("dimension mismatch: chunk_id=%d candidate_len=%d query_len=%d", m.chunkID, m.candLen, m.queryLen)
	}

	sort.Slice(scored, func(a, b int) bool {
		return candidateBefore(scored[a].score, scored[b].score, scored[a].chunkID, scored[b].chunkID)
	})

	hits := make([]model.IndexHit, len(scored))
	for idx, s := range scored {
		hits[idx] = model.IndexHit{ChunkID: s.chunkID, Score: s.score, Payload: s.payload}
	}
	return hits, nil
}

type dimMismatch struct {
	chunkID  uint64
	candLen  int
	queryLen int
}

// scoreTopK scores every dimension-matching, filter-passing vector in place
// under the read lock and returns the (unordered) best k candidates plus any
// dimension mismatches for logging outside the lock. Because it retains at most
// k candidates, it never allocates a per-query copy of the whole candidate set
// nor of any candidate vector; a payload is copied out of the map only when the
// candidate actually enters the heap.
func (i *HNSWIndex) scoreTopK(vector []float32, k int, filter model.Filter) ([]scoredCandidate, []dimMismatch) {
	var mismatches []dimMismatch
	applyFilter := !filter.IsZero()
	h := topKHeap{k: k, items: make([]scoredCandidate, 0, k)}

	i.mu.RLock()
	for id, cand := range i.vectors {
		if len(cand) != len(vector) {
			mismatches = append(mismatches, dimMismatch{id, len(cand), len(vector)})
			if i.Metrics != nil {
				i.Metrics.DimensionMismatch.Add(1)
			}
			continue
		}
		havePayload := false
		var payload model.IndexPayload
		if applyFilter {
			payload = i.payloads[id]
			havePayload = true
			if !filter.Match(payload) {
				continue
			}
		}
		score := cosineSimilarity(vector, cand)
		// Skip the payload copy + heap push for candidates that cannot displace
		// the current worst of a full heap.
		if h.full() && !h.better(score, id) {
			continue
		}
		if !havePayload {
			payload = i.payloads[id]
		}
		h.push(scoredCandidate{chunkID: id, score: score, payload: payload})
	}
	i.mu.RUnlock()
	return h.items, mismatches
}

// topKHeap is a bounded min-heap that retains the best k candidates seen so far,
// ordered by candidateBefore. The root is the *worst* retained candidate, so a
// newly scored candidate that ranks before the root evicts it in O(log k).
type topKHeap struct {
	items []scoredCandidate
	k     int
}

func (h *topKHeap) full() bool { return len(h.items) >= h.k }

// better reports whether a candidate with (score, id) ranks before the current
// worst retained candidate (the root). Only meaningful when the heap is full.
func (h *topKHeap) better(score float32, id uint64) bool {
	root := h.items[0]
	return candidateBefore(score, root.score, id, root.chunkID)
}

// worse reports whether item a ranks after item b (a is the poorer match).
func (h *topKHeap) worse(a, b int) bool {
	x, y := h.items[a], h.items[b]
	return candidateBefore(y.score, x.score, y.chunkID, x.chunkID)
}

// push adds c when the heap is under capacity, otherwise replaces the worst
// retained candidate. Callers must only push a candidate that is under capacity
// or better than the root (see scoreTopK).
func (h *topKHeap) push(c scoredCandidate) {
	if len(h.items) < h.k {
		h.items = append(h.items, c)
		h.siftUp(len(h.items) - 1)
		return
	}
	h.items[0] = c
	h.siftDown(0)
}

func (h *topKHeap) siftUp(n int) {
	for n > 0 {
		parent := (n - 1) / 2
		if !h.worse(n, parent) {
			break
		}
		h.items[n], h.items[parent] = h.items[parent], h.items[n]
		n = parent
	}
}

func (h *topKHeap) siftDown(n int) {
	size := len(h.items)
	for {
		left := 2*n + 1
		if left >= size {
			return
		}
		worst := left
		if right := left + 1; right < size && h.worse(right, left) {
			worst = right
		}
		if !h.worse(worst, n) {
			return
		}
		h.items[n], h.items[worst] = h.items[worst], h.items[n]
		n = worst
	}
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
	i.version++
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

	// Snapshot under the read lock so concurrent Upsert/Delete don't block on
	// the gob encoding and file I/O, and so we deep-copy each slice rather than
	// race with callers who might mutate the originals later. The version is
	// captured atomically with the snapshot; when it equals the last saved
	// version nothing has changed since the previous durable write, so we skip
	// re-encoding and rewriting the whole index — no snapshot copy, no mkdir, no
	// I/O (issue #429 F7).
	snapshot, version, dirty := i.snapshotIfDirty()
	if !dirty {
		return nil
	}

	// Ensure the destination directory exists before writing. The temp file is
	// created in the *same* directory as path (path+".tmp") so the subsequent
	// rename is atomic on a single filesystem; if the state directory is missing
	// both os.Create and os.Rename would otherwise fail with "no such file or
	// directory" (issue #375). MkdirAll on an existing directory is a no-op.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	tmpPath := path + ".tmp"
	file, err := os.Create(tmpPath)
	if err != nil {
		return err
	}

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
	// The snapshot just landed durably; record the version it captured so a
	// subsequent Save with no intervening mutation is a no-op. Any Upsert/Delete
	// that raced this write bumped version past the captured value, so it stays
	// dirty and the next Save persists it.
	i.markSaved(version)
	return nil
}

// snapshotIfDirty captures, under the read lock, the mutation version and — only
// when it differs from the last saved version — a deep copy of the index state
// for persistence. The bool reports whether a save is needed; when false the
// returned snapshot is empty and callers must skip the write. Capturing the
// version and the copied state under the same lock makes the dirty decision
// race-free: a concurrent Upsert/Delete either lands before the copy (included,
// version advanced) or after (excluded, version advanced past the captured
// value so the next Save re-persists it).
func (i *HNSWIndex) snapshotIfDirty() (hnswSnapshot, uint64, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if i.version == i.savedVersion {
		return hnswSnapshot{}, i.version, false
	}
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
	return hnswSnapshot{Vectors: vectors, Payloads: payloads, Identity: i.identity}, i.version, true
}

// markSaved advances savedVersion to the version captured by a successful Save.
// The guard tolerates out-of-order calls (savedVersion only ever moves forward)
// though PersistenceManager already serializes saves.
func (i *HNSWIndex) markSaved(version uint64) {
	i.mu.Lock()
	if version > i.savedVersion {
		i.savedVersion = version
	}
	i.mu.Unlock()
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
	// The freshly loaded state is exactly what is on disk, so mark it clean: a
	// Save with no intervening mutation must not rewrite an identical snapshot.
	i.version++
	i.savedVersion = i.version
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
