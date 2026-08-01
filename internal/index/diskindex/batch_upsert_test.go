package diskindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #429 F8: the disk backend's per-record open+write+fsync+close capped
// ingest at the fsync rate (EmbeddingWorker.indexChunks upserts once per chunk).
// BatchUpsert pays ONE durability barrier for the whole batch. These tests are
// in-package because they hook the unexported syncFile seam to count fsyncs.

// countSyncs replaces the package fsync seam with a counter for the duration of
// the test. Tests using it must not run in parallel (shared package state).
func countSyncs(t *testing.T) *int {
	t.Helper()
	orig := syncFile
	n := 0
	syncFile = func(f *os.File) error {
		n++
		return orig(f)
	}
	t.Cleanup(func() { syncFile = orig })
	return &n
}

func newTestIndex(t *testing.T) *DiskIndex {
	t.Helper()
	idx := New(filepath.Join(t.TempDir(), SegmentFileName("text")))
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func batchItems(n int) []model.IndexUpsert {
	items := make([]model.IndexUpsert, n)
	for i := range items {
		id := uint64(i + 1)
		items[i] = model.IndexUpsert{
			Vector:  []float32{float32(i), 1, 0, 0},
			Payload: model.IndexPayload{ChunkID: id, RelPath: "a.txt", DocType: "md"},
		}
	}
	return items
}

func mustFileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// TestBatchUpsert_OneFsyncPerBatch is the regression test for #429 F8: a batch
// of N vectors must cost exactly ONE fsync, where the equivalent per-chunk
// Upsert loop costs N. Reverting BatchUpsert to a loop over appendRecord makes
// the batch count N and fails this test.
func TestBatchUpsert_OneFsyncPerBatch(t *testing.T) {
	ctx := context.Background()
	const n = 50
	items := batchItems(n)

	syncs := countSyncs(t)
	batched := newTestIndex(t)
	if err := batched.BatchUpsert(ctx, items); err != nil {
		t.Fatalf("BatchUpsert: %v", err)
	}
	if *syncs != 1 {
		t.Fatalf("BatchUpsert of %d vectors did %d fsyncs, want exactly 1", n, *syncs)
	}

	serial := newTestIndex(t)
	before := *syncs
	for _, it := range items {
		if err := serial.Upsert(ctx, it.Vector, it.Payload); err != nil {
			t.Fatalf("Upsert(%d): %v", it.Payload.ChunkID, err)
		}
	}
	if got := *syncs - before; got != n {
		t.Fatalf("per-chunk Upsert of %d vectors did %d fsyncs, want %d (the behavior the batch path replaces)", n, got, n)
	}

	// The two paths must produce the same index: same live set, same bytes.
	if len(batched.locators) != len(serial.locators) {
		t.Fatalf("locator counts differ: batched=%d serial=%d", len(batched.locators), len(serial.locators))
	}
	if a, b := mustFileSize(t, batched.path), mustFileSize(t, serial.path); a != b {
		t.Fatalf("segment sizes differ: batched=%d serial=%d", a, b)
	}
	hits, err := batched.Search(ctx, items[7].Vector, 1, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != items[7].Payload.ChunkID {
		t.Fatalf("batched search hits = %+v, want chunk %d", hits, items[7].Payload.ChunkID)
	}
}

// TestBatchUpsert_LastWriterWinsWithinBatch pins that a repeated chunk ID inside
// one batch resolves exactly as a sequence of Upserts would: the last item wins.
func TestBatchUpsert_LastWriterWinsWithinBatch(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)
	items := []model.IndexUpsert{
		{Vector: []float32{1, 0}, Payload: model.IndexPayload{ChunkID: 9, RelPath: "old.txt"}},
		{Vector: []float32{0, 1}, Payload: model.IndexPayload{ChunkID: 9, RelPath: "new.txt"}},
	}
	if err := idx.BatchUpsert(ctx, items); err != nil {
		t.Fatalf("BatchUpsert: %v", err)
	}
	hits, err := idx.Search(ctx, []float32{0, 1}, 5, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %+v, want exactly one record for chunk 9", hits)
	}
	if hits[0].Payload.RelPath != "new.txt" {
		t.Fatalf("rel_path = %q, want the last writer (new.txt)", hits[0].Payload.RelPath)
	}
}

// TestBatchUpsert_ValidationRejectsBeforeAnyWrite pins that a malformed item
// fails the whole batch WITHOUT leaving records behind: validation happens
// before the file is touched.
func TestBatchUpsert_ValidationRejectsBeforeAnyWrite(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)
	items := append(batchItems(3), model.IndexUpsert{
		Vector:  []float32{1, 0},
		Payload: model.IndexPayload{ChunkID: 0},
	})
	if err := idx.BatchUpsert(ctx, items); err == nil {
		t.Fatal("BatchUpsert with a zero chunk_id must fail")
	}
	if len(idx.locators) != 0 {
		t.Fatalf("locators = %d, want 0 (nothing may be applied)", len(idx.locators))
	}
	if _, err := os.Stat(idx.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("segment must not be created by a rejected batch: stat err = %v", err)
	}
}

// TestBatchUpsert_RollsBackFailedBatch pins the crash-safety guarantee: when the
// batch's durability barrier fails, the segment is truncated back to its
// pre-batch length and the in-memory state is untouched — so the index is never
// left half-applied and remains appendable and loadable.
func TestBatchUpsert_RollsBackFailedBatch(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)
	seed := batchItems(2)
	if err := idx.BatchUpsert(ctx, seed); err != nil {
		t.Fatalf("seed BatchUpsert: %v", err)
	}
	sizeBefore := mustFileSize(t, idx.path)
	endBefore, versionBefore := idx.appendEnd, idx.version

	failing := []model.IndexUpsert{
		{Vector: []float32{1, 0, 0, 0}, Payload: model.IndexPayload{ChunkID: 101, RelPath: "x.txt"}},
		{Vector: []float32{0, 1, 0, 0}, Payload: model.IndexPayload{ChunkID: 102, RelPath: "y.txt"}},
	}
	boom := errors.New("simulated fsync failure")
	orig := syncFile
	syncFile = func(f *os.File) error { return boom }
	err := idx.BatchUpsert(ctx, failing)
	syncFile = orig
	if !errors.Is(err, boom) {
		t.Fatalf("BatchUpsert error = %v, want it to surface %v", err, boom)
	}

	if got := mustFileSize(t, idx.path); got != sizeBefore {
		t.Fatalf("segment size = %d after a failed batch, want the pre-batch %d (rolled back)", got, sizeBefore)
	}
	if idx.appendEnd != endBefore || idx.version != versionBefore {
		t.Fatalf("in-memory state advanced on a failed batch: appendEnd %d->%d version %d->%d",
			endBefore, idx.appendEnd, versionBefore, idx.version)
	}
	if len(idx.locators) != len(seed) {
		t.Fatalf("locators = %d after a failed batch, want the seeded %d", len(idx.locators), len(seed))
	}

	// The rolled-back segment must still be appendable and parseable.
	if err := idx.BatchUpsert(ctx, failing); err != nil {
		t.Fatalf("retry BatchUpsert after rollback: %v", err)
	}
	reopened := New(idx.path)
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Load(ctx, idx.path); err != nil {
		t.Fatalf("Load after rollback+retry: %v", err)
	}
	if len(reopened.locators) != len(seed)+len(failing) {
		t.Fatalf("reloaded live set = %d, want %d", len(reopened.locators), len(seed)+len(failing))
	}
}

// TestLoad_RecoversTornTail pins that a segment whose tail was torn by an
// ungraceful crash mid-batch loads to the last COMPLETE record instead of
// erroring out, and that the torn bytes are dropped so later appends are still
// readable. Batching widens the window in which unsynced bytes can be in flight,
// so this is the crash mode the batch path must survive.
func TestLoad_RecoversTornTail(t *testing.T) {
	ctx := context.Background()
	idx := newTestIndex(t)
	if err := idx.BatchUpsert(ctx, batchItems(3)); err != nil {
		t.Fatalf("BatchUpsert: %v", err)
	}
	path := idx.path
	full := mustFileSize(t, path)
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a crash that left only part of the last record on disk.
	if err := os.Truncate(path, full-6); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	reopened := New(path)
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Load(ctx, path); err != nil {
		t.Fatalf("Load with a torn tail must recover, got: %v", err)
	}
	if len(reopened.locators) != 2 {
		t.Fatalf("recovered live set = %d, want the 2 complete records", len(reopened.locators))
	}
	if got := mustFileSize(t, path); got != reopened.appendEnd {
		t.Fatalf("torn tail not dropped: file size %d, appendEnd %d", got, reopened.appendEnd)
	}

	// Appending after the recovery must survive another reopen.
	if err := reopened.Upsert(ctx, []float32{9, 9, 9, 9}, model.IndexPayload{ChunkID: 77, RelPath: "z.txt"}); err != nil {
		t.Fatalf("Upsert after recovery: %v", err)
	}
	again := New(path)
	t.Cleanup(func() { _ = again.Close() })
	if err := again.Load(ctx, path); err != nil {
		t.Fatalf("reload after recovery: %v", err)
	}
	if _, ok := again.locators[77]; !ok || len(again.locators) != 3 {
		t.Fatalf("post-recovery append lost: locators = %v", again.locators)
	}
}
