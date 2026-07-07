package tests

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/index/diskindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #429: the on-disk backend's Search now scores with a bounded top-k heap
// (reading each candidate vector into a single reused scratch buffer) instead of
// appending every scored hit and full-sorting, and its Save is dirty-flagged so
// an idle corpus's autosave stops recompacting the whole segment. The periodic
// autosave (AutosaveTick) is throttled so a long ingest doesn't rewrite the
// segment on every 15s tick. These tests pin all three, plus that search
// allocations stay bounded as N grows.

// TestDiskIndex_TopKMatchesFullSort proves the disk backend's heap-based top-k
// returns exactly the ranked hits the previous full-sort-then-truncate produced,
// across random corpora/queries, k values, and a filter. referenceTopK (perf429_
// test.go) scores with refCosine, which matches diskindex.cosineSimilarity bit
// for bit (same query-vector accumulation order), so ties resolve identically.
func TestDiskIndex_TopKMatchesFullSort(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	const dim = 8
	for trial := 0; trial < 30; trial++ {
		n := 1 + r.Intn(50)
		dir := t.TempDir()
		idx := diskindex.New(filepath.Join(dir, diskindex.SegmentFileName("text")))
		vecs := make(map[uint64][]float32, n)
		payloads := make(map[uint64]model.IndexPayload, n)
		for j := 0; j < n; j++ {
			id := uint64(j + 1)
			v := randVec(r, dim)
			// Force a cluster of exact ties every few ids so the eps-tolerant
			// chunkID tiebreak (not just distinct scores) is exercised.
			if j%7 == 0 {
				v = []float32{1, 0, 0, 0, 0, 0, 0, 0}
			}
			dt := "md"
			if j%2 == 0 {
				dt = "code"
			}
			p := model.IndexPayload{ChunkID: id, RelPath: fmt.Sprintf("f%d.txt", id), DocType: dt}
			mustUpsertDisk(t, idx, p, v)
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
		_ = idx.Close()
	}
}

// TestDiskIndex_TopKTieBreakByChunkID pins the disk backend's deterministic
// tiebreak: equal-cosine candidates order by ascending chunk_id and the k
// boundary falls on the lower id.
func TestDiskIndex_TopKTieBreakByChunkID(t *testing.T) {
	dir := t.TempDir()
	idx := diskindex.New(filepath.Join(dir, diskindex.SegmentFileName("text")))
	t.Cleanup(func() { _ = idx.Close() })
	for _, id := range []uint64{30, 10, 20} {
		mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: id, RelPath: "f.txt", DocType: "md"}, []float32{1, 0})
	}
	hits, err := idx.Search(context.Background(), []float32{1, 0}, 2, model.Filter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 || hits[0].ChunkID != 10 || hits[1].ChunkID != 20 {
		t.Fatalf("expected ids [10 20] by ascending tiebreak, got %+v", hits)
	}
}

// TestDiskIndex_DirtyFlagSkipsUnchangedSave proves Save no longer recompacts the
// whole segment when nothing changed (F7): a Save with no intervening mutation
// leaves the segment file untouched (same inode — os.SameFile), while a Save
// after an Upsert rewrites it (new inode from the atomic temp-file rename).
func TestDiskIndex_DirtyFlagSkipsUnchangedSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, diskindex.SegmentFileName("text"))
	idx := diskindex.New(path)
	t.Cleanup(func() { _ = idx.Close() })

	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "f.txt", DocType: "md"}, []float32{1, 0})
	if err := idx.Save(context.Background(), ""); err != nil {
		t.Fatalf("first save: %v", err)
	}
	fi1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first save: %v", err)
	}

	// Second Save with no mutation must be a no-op: the segment is not rewritten.
	if err := idx.Save(context.Background(), ""); err != nil {
		t.Fatalf("clean save: %v", err)
	}
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after clean save: %v", err)
	}
	if !os.SameFile(fi1, fi2) {
		t.Fatalf("clean Save rewrote the segment; dirty flag not honored")
	}

	// A mutation makes it dirty → Save recompacts (new file).
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "f.txt", DocType: "md"}, []float32{0, 1})
	if err := idx.Save(context.Background(), ""); err != nil {
		t.Fatalf("dirty save: %v", err)
	}
	fi3, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after dirty save: %v", err)
	}
	if os.SameFile(fi2, fi3) {
		t.Fatalf("dirty Save did not rewrite the segment")
	}
}

// TestDiskIndex_AutosaveThrottled is the ingest-cliff regression guard (C-a): with
// a per-upsert tick (the worst case — one autosave tick per mutation) the number
// of real compactions is bounded by ceil(K/threshold), NOT one per tick. A real
// compaction is observed as a segment-file inode change (Save renames a fresh
// temp file into place; appends keep the same inode).
func TestDiskIndex_AutosaveThrottled(t *testing.T) {
	const (
		k         = 25
		threshold = 10
	)
	dir := t.TempDir()
	path := filepath.Join(dir, diskindex.SegmentFileName("text"))
	idx := diskindex.New(path)
	t.Cleanup(func() { _ = idx.Close() })
	// Large max interval so only the mutation threshold — never wall-time — can
	// trigger a save within the test, making the bound tight and deterministic.
	idx.SetAutosavePolicy(threshold, time.Hour)

	saves := 0
	var base os.FileInfo
	for j := 1; j <= k; j++ {
		mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: uint64(j), RelPath: "f.txt", DocType: "md"}, []float32{float32(j), 1})
		if err := idx.AutosaveTick(context.Background(), ""); err != nil {
			t.Fatalf("autosave tick %d: %v", j, err)
		}
		cur, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if base == nil {
			// First observation is the append-created log, not a compaction.
			base = cur
			continue
		}
		if !os.SameFile(base, cur) {
			saves++
			base = cur
		}
	}

	maxSaves := (k + threshold - 1) / threshold // ceil(K/threshold)
	if saves < 1 {
		t.Fatalf("expected at least one throttled compaction over %d upserts, got 0", k)
	}
	if saves > maxSaves {
		t.Fatalf("compactions=%d exceed ceil(K/threshold)=%d; throttle not bounding", saves, maxSaves)
	}
	if saves >= k {
		t.Fatalf("compactions=%d ~ one-per-tick (%d ticks); autosave not throttled", saves, k)
	}
}

// TestHNSWIndex_AutosaveThrottled is the memory-backend twin of the disk guard:
// a per-upsert tick yields a bounded number of snapshots, not one per tick. The
// memory snapshot file only exists after a real Save, so its first appearance and
// each subsequent inode change count a save.
func TestHNSWIndex_AutosaveThrottled(t *testing.T) {
	const (
		k         = 25
		threshold = 10
	)
	path := filepath.Join(t.TempDir(), index.TextIndexFileName)
	idx := index.NewHNSWIndex(path)
	idx.SetAutosavePolicy(threshold, time.Hour)

	saves := 0
	var prev os.FileInfo
	for j := 1; j <= k; j++ {
		upsertVec(t, idx, uint64(j), []float32{float32(j), 1})
		if err := idx.AutosaveTick(context.Background(), ""); err != nil {
			t.Fatalf("autosave tick %d: %v", j, err)
		}
		cur, err := os.Stat(path)
		if err != nil {
			continue // no snapshot yet: this tick was throttled
		}
		if prev == nil || !os.SameFile(prev, cur) {
			saves++
			prev = cur
		}
	}

	maxSaves := (k + threshold - 1) / threshold
	if saves < 1 {
		t.Fatalf("expected at least one throttled snapshot over %d upserts, got 0", k)
	}
	if saves > maxSaves {
		t.Fatalf("snapshots=%d exceed ceil(K/threshold)=%d; throttle not bounding", saves, maxSaves)
	}
	if saves >= k {
		t.Fatalf("snapshots=%d ~ one-per-tick (%d ticks); autosave not throttled", saves, k)
	}
}

// TestSearch_AllocationsStayBounded guards issue #429 F1/F2: Search allocations
// must stay O(k), not O(N). A 4x larger corpus must not roughly-4x the per-query
// allocations. Skipped under -short so `make ci` stays fast.
func TestSearch_AllocationsStayBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation guard is slow (builds a few thousand-vector corpora); skipped under -short")
	}
	const (
		dim   = 16
		small = 300
		large = 1200 // 4x
		k     = 10
	)
	r := rand.New(rand.NewSource(11))
	query := randVec(r, dim)

	memAllocs := func(n int) float64 {
		idx := index.NewHNSWIndex("")
		for j := 1; j <= n; j++ {
			upsertVec(t, idx, uint64(j), randVec(r, dim))
		}
		return testing.AllocsPerRun(10, func() {
			if _, err := idx.Search(context.Background(), query, k, model.Filter{}); err != nil {
				t.Fatalf("mem search: %v", err)
			}
		})
	}
	diskAllocs := func(n int) float64 {
		dir := t.TempDir()
		idx := diskindex.New(filepath.Join(dir, diskindex.SegmentFileName("text")))
		defer func() { _ = idx.Close() }()
		for j := 1; j <= n; j++ {
			mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: uint64(j), RelPath: "f.txt", DocType: "md"}, randVec(r, dim))
		}
		return testing.AllocsPerRun(10, func() {
			if _, err := idx.Search(context.Background(), query, k, model.Filter{}); err != nil {
				t.Fatalf("disk search: %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		alloc func(int) float64
	}{
		{"memory", memAllocs},
		{"disk", diskAllocs},
	} {
		aSmall := tc.alloc(small)
		aLarge := tc.alloc(large)
		// O(N) would scale ~4x with the corpus; O(k) stays roughly flat. Allow a
		// 2x slack for measurement noise and the constant hits-slice/sort work.
		if aLarge > aSmall*2 {
			t.Fatalf("%s: Search allocs scale with N (allocs %.0f@N=%d vs %.0f@N=%d, >2x); expected O(k)",
				tc.name, aLarge, large, aSmall, small)
		}
	}
}
