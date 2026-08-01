package tests

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/index/diskindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// compile-time assertion that the disk backend satisfies the full contract.
var (
	_ model.Index          = (*diskindex.DiskIndex)(nil)
	_ model.Persistable    = (*diskindex.DiskIndex)(nil)
	_ model.FilteringIndex = (*diskindex.DiskIndex)(nil)
)

func newDiskIndex(t *testing.T) *diskindex.DiskIndex {
	t.Helper()
	dir := t.TempDir()
	idx := diskindex.New(filepath.Join(dir, diskindex.SegmentFileName("text")))
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func mustUpsertDisk(t *testing.T, idx *diskindex.DiskIndex, payload model.IndexPayload, vec []float32) {
	t.Helper()
	if err := idx.Upsert(context.Background(), vec, payload); err != nil {
		t.Fatalf("Upsert(%d): %v", payload.ChunkID, err)
	}
}

func TestDiskIndex_UpsertWithDocTypeFilter(t *testing.T) {
	ctx := context.Background()
	idx := newDiskIndex(t)
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "docs/a.md", DocType: "md"}, []float32{1, 0})
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "src/a.go", DocType: "code"}, []float32{0.99, 0.01})
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 3, RelPath: "docs/b.md", DocType: "md"}, []float32{0.98, 0.02})

	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{DocTypes: []string{"MD"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := chunkIDs(hits)
	if len(got) != 2 {
		t.Fatalf("expected 2 md hits, got %v", got)
	}
	for _, id := range got {
		if id == 2 {
			t.Fatalf("code chunk 2 leaked past doctype filter: %v", got)
		}
	}
}

func TestDiskIndex_SearchWithPathPrefixAndGlobFilter(t *testing.T) {
	ctx := context.Background()
	idx := newDiskIndex(t)
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "docs/a.md", DocType: "md"}, []float32{1, 0})
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "docs/b.md", DocType: "md"}, []float32{0.99, 0.01})
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 3, RelPath: "src/main.go", DocType: "code"}, []float32{0.98, 0.02})

	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{PathPrefix: "docs/", PathGlob: "docs/a.*"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := chunkIDs(hits)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("expected only chunk 1 (docs/a.md), got %v", got)
	}
}

func TestDiskIndex_UpsertReplacesVector(t *testing.T) {
	ctx := context.Background()
	idx := newDiskIndex(t)
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 0})
	// Replace chunk 1 with a different payload/vector; last-writer-wins.
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "renamed.md"}, []float32{0, 1})

	hits, err := idx.Search(ctx, []float32{0, 1}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 1 {
		t.Fatalf("expected single chunk 1, got %v", chunkIDs(hits))
	}
	if hits[0].Payload.RelPath != "renamed.md" {
		t.Fatalf("expected replaced payload rel_path renamed.md, got %q", hits[0].Payload.RelPath)
	}
}

func TestDiskIndex_DeleteRemovesVector(t *testing.T) {
	ctx := context.Background()
	idx := newDiskIndex(t)
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 0})
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "b.md"}, []float32{0.9, 0.1})

	if err := idx.Delete(ctx, []uint64{1}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := chunkIDs(hits)
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("expected only chunk 2 after delete, got %v", got)
	}
	// Deleting an unknown id is a no-op, not an error.
	if err := idx.Delete(ctx, []uint64{999}); err != nil {
		t.Fatalf("Delete unknown id: %v", err)
	}
}

func TestDiskIndex_IdentityAndReset(t *testing.T) {
	ctx := context.Background()
	idx := newDiskIndex(t)

	id, err := idx.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id != "" {
		t.Fatalf("fresh index identity should be empty, got %q", id)
	}

	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 0})
	if err := idx.Reset(ctx, "mistral|mistral-embed|codestral-embed|0|0|off"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	id, err = idx.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity after reset: %v", err)
	}
	if id != "mistral|mistral-embed|codestral-embed|0|0|off" {
		t.Fatalf("unexpected identity after reset: %q", id)
	}
	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search after reset: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected empty index after reset, got %v", chunkIDs(hits))
	}
	// Upsert still works after Reset (segment was truncated to a fresh header).
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 5, RelPath: "c.md"}, []float32{1, 0})
	hits, _ = idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
	if len(hits) != 1 || hits[0].ChunkID != 5 {
		t.Fatalf("expected chunk 5 after post-reset upsert, got %v", chunkIDs(hits))
	}
}

func TestDiskIndex_CanFilterAlwaysTrue(t *testing.T) {
	idx := newDiskIndex(t)
	if !idx.CanFilter(model.Filter{}) {
		t.Fatal("disk backend should report CanFilter true for the zero filter")
	}
	if !idx.CanFilter(model.Filter{PathPrefix: "docs/", DocTypes: []string{"md"}}) {
		t.Fatal("disk backend should report CanFilter true for a populated filter")
	}
}

func TestDiskIndex_EnsureIdentityResetsOnMismatch(t *testing.T) {
	ctx := context.Background()
	idx := newDiskIndex(t)
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 0})

	// Fresh (empty identity): reconcile resets to the configured identity.
	if err := index.EnsureIdentity(ctx, idx, "ident-1"); err != nil {
		t.Fatalf("EnsureIdentity (fresh): %v", err)
	}
	if id, _ := idx.Identity(ctx); id != "ident-1" {
		t.Fatalf("expected identity ident-1, got %q", id)
	}
	if hits, _ := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{}); len(hits) != 0 {
		t.Fatalf("expected vectors cleared on fresh reconcile, got %v", chunkIDs(hits))
	}

	// Matching identity is a no-op: vectors are preserved.
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "b.md"}, []float32{1, 0})
	if err := index.EnsureIdentity(ctx, idx, "ident-1"); err != nil {
		t.Fatalf("EnsureIdentity (match): %v", err)
	}
	if hits, _ := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{}); len(hits) != 1 {
		t.Fatalf("matching identity must not clear vectors, got %v", chunkIDs(hits))
	}

	// Changed identity resets again.
	if err := index.EnsureIdentity(ctx, idx, "ident-2"); err != nil {
		t.Fatalf("EnsureIdentity (mismatch): %v", err)
	}
	if hits, _ := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{}); len(hits) != 0 {
		t.Fatalf("expected vectors cleared on identity change, got %v", chunkIDs(hits))
	}
}

// TestDiskIndex_PersistenceRoundTrip is the core Tier-B proof: Save to disk,
// reopen a brand-new instance from the segment file (which only rebuilds the
// fixed-size locator map — the vectors themselves stay memory-mapped on disk),
// and verify a filtered search still returns the right payloads. The reopened
// index never loads the whole corpus into a Go vector map, demonstrating the
// removed all-vectors-in-RAM ceiling.
func TestDiskIndex_PersistenceRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	segPath := filepath.Join(dir, diskindex.SegmentFileName("text"))

	idx := diskindex.New(segPath)
	if err := idx.Reset(ctx, "ident-rt"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "docs/a.md", DocType: "md", Snippet: "alpha"}, []float32{1, 0, 0})
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "docs/b.md", DocType: "md", Snippet: "beta"}, []float32{0, 1, 0})
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 3, RelPath: "src/c.go", DocType: "code", Snippet: "gamma"}, []float32{0, 0, 1})
	// Delete one so compaction must drop a tombstoned record on Save.
	if err := idx.Delete(ctx, []uint64{3}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := idx.Save(ctx, segPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen from disk in a fresh instance.
	reopened := diskindex.New(segPath)
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Load(ctx, segPath); err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertReopenedDiskIndex(t, reopened)
}

// assertReopenedDiskIndex verifies that a disk index reopened from its segment
// preserves the identity, live vectors, payloads, filtering, and the deletion
// applied before Save.
func assertReopenedDiskIndex(t *testing.T, reopened *diskindex.DiskIndex) {
	t.Helper()
	ctx := context.Background()

	if id, _ := reopened.Identity(ctx); id != "ident-rt" {
		t.Fatalf("identity did not survive round-trip, got %q", id)
	}

	// Vectors survive: searching for the exact vector of chunk 1 returns it
	// with its full payload, sourced from the memory-mapped segment.
	hits, err := reopened.Search(ctx, []float32{1, 0, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search after reopen: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 live vectors after reopen (chunk 3 deleted), got %v", chunkIDs(hits))
	}
	if hits[0].ChunkID != 1 {
		t.Fatalf("expected chunk 1 best match for its own vector, got %v", chunkIDs(hits))
	}
	if hits[0].Payload.Snippet != "alpha" || hits[0].Payload.RelPath != "docs/a.md" {
		t.Fatalf("payload did not survive round-trip: %+v", hits[0].Payload)
	}
	// The deleted chunk must not reappear.
	for _, h := range hits {
		if h.ChunkID == 3 {
			t.Fatalf("deleted chunk 3 reappeared after reopen")
		}
	}

	// Filtering still works on the reopened, disk-backed payloads.
	mdHits, err := reopened.Search(ctx, []float32{1, 0, 0}, 10, model.Filter{DocTypes: []string{"md"}})
	if err != nil {
		t.Fatalf("filtered Search after reopen: %v", err)
	}
	if len(mdHits) != 2 {
		t.Fatalf("expected 2 md hits after reopen, got %v", chunkIDs(mdHits))
	}
}

// TestDiskIndex_LoadMissingSegmentIsFresh verifies a missing segment file is
// treated as a fresh index (no error), matching the HNSW reference.
func TestDiskIndex_LoadMissingSegmentIsFresh(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	segPath := filepath.Join(dir, diskindex.SegmentFileName("text"))

	idx := diskindex.New(segPath)
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Load(ctx, segPath); err != nil {
		t.Fatalf("Load missing segment should be nil error, got %v", err)
	}
	if _, statErr := os.Stat(segPath); !os.IsNotExist(statErr) {
		t.Fatalf("Load must not create the segment file")
	}
	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search on fresh index: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("fresh index should have no hits, got %v", chunkIDs(hits))
	}
}

func TestDiskIndex_UpsertRejectsEmptyVectorAndZeroID(t *testing.T) {
	ctx := context.Background()
	idx := newDiskIndex(t)
	if err := idx.Upsert(ctx, nil, model.IndexPayload{ChunkID: 1}); err == nil {
		t.Fatal("expected error on empty vector")
	}
	if err := idx.Upsert(ctx, []float32{1, 0}, model.IndexPayload{ChunkID: 0}); err == nil {
		t.Fatal("expected error on zero chunk_id")
	}
}

// TestDiskIndex_LoadBadMagicErrors verifies a segment whose header is not the
// expected magic is reported as an error (graceful, not a panic), so a corrupt
// or alien file is never silently treated as a valid index.
func TestDiskIndex_LoadBadMagicErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	segPath := filepath.Join(dir, diskindex.SegmentFileName("text"))
	if err := os.WriteFile(segPath, []byte("NOTMAGIC\x01\x00\x00\x00garbage"), 0o644); err != nil {
		t.Fatalf("seed corrupt segment: %v", err)
	}
	idx := diskindex.New(segPath)
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Load(ctx, segPath); err == nil {
		t.Fatal("expected an error loading a segment with bad magic, got nil")
	}
}

// TestDiskIndex_LoadTruncatedBodyRecovers verifies a segment truncated
// mid-record (a torn write) loads to the last COMPLETE record instead of
// panicking or erroring.
//
// This deliberately replaces the older "a torn tail is an error" expectation
// (issue #429 F8). The batch path (BatchUpsert) trades one fsync per record for
// one per batch, so an ungraceful crash can leave a prefix of a batch in the
// file; failing the load would make the whole index unusable (the daemon exits
// with exitIndexLoadFailure) after a crash that lost nothing. It loses nothing
// because BatchUpsert returns only after its fsync and the embed worker marks
// chunks embedded strictly after that returns: every record in a torn tail
// belongs to a chunk still PENDING in sqlite, which is simply re-embedded. A
// structurally wrong file (bad magic/version) is still a hard error; see
// TestDiskIndex_LoadBadMagicErrors.
func TestDiskIndex_LoadTruncatedBodyRecovers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	segPath := filepath.Join(dir, diskindex.SegmentFileName("text"))

	idx := diskindex.New(segPath)
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 2, 3})
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := os.Stat(segPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// Lop bytes off the end so the final record's body is incomplete.
	if err := os.Truncate(segPath, info.Size()-5); err != nil {
		t.Fatalf("Truncate: %v", err)
	}

	reopened := diskindex.New(segPath)
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Load(ctx, segPath); err != nil {
		t.Fatalf("loading a segment with a torn tail must recover, got: %v", err)
	}
	// The torn record is dropped (it is the only one here), and the recovered
	// segment is usable: a fresh append lands and survives another reopen.
	hits, err := reopened.Search(ctx, []float32{1, 2, 3}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search after recovery: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits = %+v, want none (the torn record must not be served)", hits)
	}
	mustUpsertDisk(t, reopened, model.IndexPayload{ChunkID: 2, RelPath: "b.md"}, []float32{4, 5, 6})
	again := diskindex.New(segPath)
	t.Cleanup(func() { _ = again.Close() })
	if err := again.Load(ctx, segPath); err != nil {
		t.Fatalf("Load after post-recovery append: %v", err)
	}
	hits, err = again.Search(ctx, []float32{4, 5, 6}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search after reopen: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 2 {
		t.Fatalf("hits = %+v, want the post-recovery record for chunk 2", hits)
	}
}

// TestDiskIndex_ConcurrentSearch exercises Search from many goroutines after the
// cached mmap view has been dropped, so the lazy re-open path runs concurrently.
// It is meaningful under `go test -race`, guarding the d.reader assignment.
func TestDiskIndex_ConcurrentSearch(t *testing.T) {
	ctx := context.Background()
	idx := newDiskIndex(t)
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 0})
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "b.md"}, []float32{0, 1})
	// Drop the cached reader so the first concurrent Search lazily re-opens it.
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := idx.Search(ctx, []float32{1, 0}, 5, model.Filter{}); err != nil {
				t.Errorf("concurrent Search: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestNewBackend_Dispatch verifies the index.backend selection seam (issue
// #246): "disk" constructs the on-disk backend and a default/unknown value
// falls back to the in-memory HNSW.
func TestNewBackend_Dispatch(t *testing.T) {
	dir := t.TempDir()

	memIx, memPath := index.NewBackend(index.BackendMemory, dir, index.KindText)
	t.Cleanup(func() { _ = memIx.Close() })
	if _, ok := memIx.(*index.HNSWIndex); !ok {
		t.Fatalf("memory backend should construct *index.HNSWIndex, got %T", memIx)
	}
	if filepath.Base(memPath) != index.TextIndexFileName {
		t.Fatalf("memory text path = %q, want basename %q", memPath, index.TextIndexFileName)
	}

	diskIx, diskPath := index.NewBackend(index.BackendDisk, dir, index.KindCode)
	t.Cleanup(func() { _ = diskIx.Close() })
	if _, ok := diskIx.(*diskindex.DiskIndex); !ok {
		t.Fatalf("disk backend should construct *diskindex.DiskIndex, got %T", diskIx)
	}
	if filepath.Base(diskPath) != diskindex.SegmentFileName(index.KindCode) {
		t.Fatalf("disk code path = %q, want basename %q", diskPath, diskindex.SegmentFileName(index.KindCode))
	}

	// Empty/unknown falls back to memory (config validation rejects unknowns).
	fallbackIx, _ := index.NewBackend("", dir, index.KindText)
	t.Cleanup(func() { _ = fallbackIx.Close() })
	if _, ok := fallbackIx.(*index.HNSWIndex); !ok {
		t.Fatalf("empty backend should fall back to *index.HNSWIndex, got %T", fallbackIx)
	}
}

// TestStaleIndexFiles_IncludesDiskSegments verifies reindex cleanup names the
// disk backend's segment + identity sidecar when "disk" is selected.
func TestStaleIndexFiles_IncludesDiskSegments(t *testing.T) {
	memNames := index.StaleIndexFiles(index.BackendMemory)
	for _, name := range memNames {
		if name == diskindex.SegmentFileName(index.KindText) {
			t.Fatalf("memory backend should not list disk segments: %v", memNames)
		}
	}

	diskNames := index.StaleIndexFiles(index.BackendDisk)
	wantSeg := diskindex.SegmentFileName(index.KindText)
	wantSidecar := wantSeg + diskindex.IdentitySidecarSuffix
	if !containsStr(diskNames, wantSeg) || !containsStr(diskNames, wantSidecar) {
		t.Fatalf("disk stale files missing segment/sidecar: %v", diskNames)
	}
	// The HNSW snapshots are always included so a prior memory build is cleaned.
	if !containsStr(diskNames, index.TextIndexFileName) {
		t.Fatalf("disk stale files should also clean HNSW snapshots: %v", diskNames)
	}
}

func containsStr(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
