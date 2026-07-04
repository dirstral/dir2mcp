package tests

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #429: the local memory backend's Search now scores in place under the
// read lock and keeps only the running top-k in a bounded heap, instead of
// copying every candidate vector and sorting all scored candidates. These tests
// pin the perf refactor to produce results IDENTICAL to the previous
// full-sort-then-truncate, and pin the dirty-flag autosave (F7) to skip a save
// when nothing changed and to save when it did.

// refCosine replicates internal/index.cosineSimilarity bit-for-bit (same float32
// accumulation order over the query vector) so the reference ranking below scores
// candidates identically to the index under test — otherwise a tie decided by the
// eps tolerance could legitimately diverge and mask a real regression.
func refCosine(a, b []float32) float32 {
	var dot, magA, magB float32
	for idx := range a {
		dot += a[idx] * b[idx]
		magA += a[idx] * a[idx]
		magB += b[idx] * b[idx]
	}
	if magA == 0 || magB == 0 {
		return 0
	}
	return dot / float32(math.Sqrt(float64(magA*magB)))
}

// referenceTopK is the pre-#429 algorithm: score every dimension-matching,
// filter-passing candidate, full-sort by (score desc, eps-tolerant chunkID asc),
// then truncate to k.
func referenceTopK(vecs map[uint64][]float32, payloads map[uint64]model.IndexPayload, query []float32, k int, filter model.Filter) []model.IndexHit {
	const eps = 1e-6
	applyFilter := !filter.IsZero()
	var scored []model.IndexHit
	for id, v := range vecs {
		if len(v) != len(query) {
			continue
		}
		if applyFilter && !filter.Match(payloads[id]) {
			continue
		}
		scored = append(scored, model.IndexHit{ChunkID: id, Score: refCosine(query, v), Payload: payloads[id]})
	}
	sort.Slice(scored, func(a, b int) bool {
		if math.Abs(float64(scored[a].Score)-float64(scored[b].Score)) <= eps {
			return scored[a].ChunkID < scored[b].ChunkID
		}
		return scored[a].Score > scored[b].Score
	})
	if len(scored) > k {
		scored = scored[:k]
	}
	return scored
}

func randVec(r *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = r.Float32()*2 - 1
	}
	return v
}

// TestHNSWIndex_TopKMatchesFullSort proves the heap-based top-k selection returns
// exactly the same ranked hits (chunk IDs, order, and scores) the old full sort
// would have, across many random corpora/queries, k values, and a filter.
func TestHNSWIndex_TopKMatchesFullSort(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	const dim = 8
	for trial := 0; trial < 50; trial++ {
		n := 1 + r.Intn(60)
		idx := index.NewHNSWIndex("")
		vecs := make(map[uint64][]float32, n)
		payloads := make(map[uint64]model.IndexPayload, n)
		for j := 0; j < n; j++ {
			id := uint64(j + 1)
			v := randVec(r, dim)
			// Half the corpus is doc type "md", half "code" so the filtered
			// pass exercises the same predicate path as the reference.
			dt := "md"
			if j%2 == 0 {
				dt = "code"
			}
			p := model.IndexPayload{ChunkID: id, RelPath: fmt.Sprintf("f%d.txt", id), DocType: dt}
			if err := idx.Upsert(context.Background(), v, p); err != nil {
				t.Fatalf("upsert: %v", err)
			}
			vecs[id] = v
			payloads[id] = p
		}

		filters := []model.Filter{{}, {DocTypes: []string{"md"}}}
		for _, filter := range filters {
			for _, k := range []int{1, 3, 7, n, n + 5} {
				query := randVec(r, dim)
				got, err := idx.Search(context.Background(), query, k, filter)
				if err != nil {
					t.Fatalf("search: %v", err)
				}
				want := referenceTopK(vecs, payloads, query, k, filter)
				if len(got) != len(want) {
					t.Fatalf("trial %d k=%d filter=%v: len got=%d want=%d", trial, k, filter, len(got), len(want))
				}
				for i := range want {
					if got[i].ChunkID != want[i].ChunkID || got[i].Score != want[i].Score {
						t.Fatalf("trial %d k=%d filter=%v pos %d: got (id=%d score=%v) want (id=%d score=%v)",
							trial, k, filter, i, got[i].ChunkID, got[i].Score, want[i].ChunkID, want[i].Score)
					}
				}
			}
		}
	}
}

// TestHNSWIndex_TopKTieBreakByChunkID pins the deterministic tiebreak: candidates
// with equal cosine score are ordered by ascending chunk_id, and the k boundary
// falls on the lower id.
func TestHNSWIndex_TopKTieBreakByChunkID(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// Three vectors pointing the same direction → identical cosine to the query,
	// so ordering is decided purely by the chunkID tiebreak.
	for _, id := range []uint64{30, 10, 20} {
		upsertVec(t, idx, id, []float32{1, 0})
	}
	hits, err := idx.Search(context.Background(), []float32{1, 0}, 2, model.Filter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 || hits[0].ChunkID != 10 || hits[1].ChunkID != 20 {
		t.Fatalf("expected ids [10 20] by ascending tiebreak, got %+v", hits)
	}
}

// TestHNSWIndex_DirtyFlagSkipsUnchangedSave proves the autosave no longer rewrites
// the whole index when nothing changed (F7): a Save with no intervening mutation
// performs no write, while a Save after a mutation does. The file is removed
// between saves so a skipped write is observable as an absent file.
func TestHNSWIndex_DirtyFlagSkipsUnchangedSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), index.TextIndexFileName)
	idx := index.NewHNSWIndex(path)

	// Fresh, never-mutated index: nothing to persist.
	if err := idx.Save(context.Background(), ""); err != nil {
		t.Fatalf("save fresh: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh index should not have written a snapshot, stat err=%v", err)
	}

	// First real write after a mutation.
	upsertVec(t, idx, 1, []float32{1, 0, 0})
	if err := idx.Save(context.Background(), ""); err != nil {
		t.Fatalf("save after upsert: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected snapshot after mutation: %v", err)
	}

	// Remove the file, then Save with nothing changed: must be a no-op (skipped).
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := idx.Save(context.Background(), ""); err != nil {
		t.Fatalf("save unchanged: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unchanged index re-wrote the snapshot; dirty flag not honored (stat err=%v)", err)
	}

	// Mutate again → dirty → Save writes.
	upsertVec(t, idx, 2, []float32{0, 1, 0})
	if err := idx.Save(context.Background(), ""); err != nil {
		t.Fatalf("save after second upsert: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected snapshot after second mutation: %v", err)
	}
}

// TestHNSWIndex_DirtyFlagDeleteAndResetMarkDirty pins that Delete and Reset are
// treated as mutations that require a save.
func TestHNSWIndex_DirtyFlagDeleteAndResetMarkDirty(t *testing.T) {
	// Each case gets its OWN fresh index seeded with vector 1, so the "delete"
	// case always targets an existing vector. Sharing one index across a
	// randomly-ordered map made this flaky: if "reset" (which clears vectors)
	// ran first, the later "delete" hit an already-empty index and — correctly —
	// left it clean (deleting an absent id is not a mutation), spuriously failing.
	cases := map[string]func(idx *index.HNSWIndex) error{
		"delete": func(idx *index.HNSWIndex) error { return idx.Delete(context.Background(), []uint64{1}) },
		"reset":  func(idx *index.HNSWIndex) error { return idx.Reset(context.Background(), "id-2") },
	}
	for name, mutate := range cases {
		mutate := mutate
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), index.TextIndexFileName)
			idx := index.NewHNSWIndex(path)
			upsertVec(t, idx, 1, []float32{1, 0})
			if err := idx.Save(context.Background(), ""); err != nil {
				t.Fatalf("initial save: %v", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove initial snapshot: %v", err)
			}
			if err := mutate(idx); err != nil {
				t.Fatalf("mutate: %v", err)
			}
			if err := idx.Save(context.Background(), ""); err != nil {
				t.Fatalf("save: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("%s did not mark the index dirty; snapshot missing: %v", name, err)
			}
		})
	}
}

// TestHNSWIndex_LoadMarksClean proves a freshly loaded index matches disk and so
// a subsequent Save with no mutation is skipped.
func TestHNSWIndex_LoadMarksClean(t *testing.T) {
	path := filepath.Join(t.TempDir(), index.TextIndexFileName)
	src := index.NewHNSWIndex(path)
	upsertVec(t, src, 1, []float32{1, 0})
	if err := src.Save(context.Background(), ""); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	loaded := index.NewHNSWIndex(path)
	if err := loaded.Load(context.Background(), ""); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := loaded.Save(context.Background(), ""); err != nil {
		t.Fatalf("save after load: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loaded-then-unchanged index re-wrote the snapshot (stat err=%v)", err)
	}
}

// TestHNSWIndex_ConcurrentSearchUpsertSave exercises the query/embed-worker/
// autosave concurrency the dirty flag must be race-free under. Run with -race.
func TestHNSWIndex_ConcurrentSearchUpsertSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), index.TextIndexFileName)
	idx := index.NewHNSWIndex(path)
	for id := uint64(1); id <= 20; id++ {
		upsertVec(t, idx, id, []float32{float32(id), 1})
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers (embed worker analogue).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for id := uint64(21); ; id++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := idx.Upsert(context.Background(), []float32{float32(id), 2}, model.IndexPayload{ChunkID: id, RelPath: "c.txt", DocType: "md"}); err != nil {
				t.Errorf("concurrent Upsert: %v", err)
				return
			}
		}
	}()

	// Autosave analogue.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if err := idx.Save(context.Background(), ""); err != nil {
					t.Errorf("concurrent Save: %v", err)
					return
				}
			}
		}
	}()

	// Readers.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := idx.Search(context.Background(), []float32{1, 1}, 5, model.Filter{}); err != nil {
					t.Errorf("search: %v", err)
					return
				}
			}
		}()
	}

	// Let the readers run to completion, then stop writers/saver.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			if _, err := idx.Search(context.Background(), []float32{2, 1}, 3, model.Filter{}); err != nil {
				t.Errorf("search: %v", err)
				break
			}
		}
		close(done)
	}()
	<-done
	close(stop)
	wg.Wait()
}
