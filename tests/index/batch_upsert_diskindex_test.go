package tests

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index/diskindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #429 F8: the disk backend's per-record open+write+fsync+close capped
// ingest at the fsync rate (EmbeddingWorker.indexChunks upserts once per chunk).
// BatchUpsert pays ONE durability barrier for the whole batch. Counting fsyncs
// is the only way to pin that as an observable property, so diskindex exports
// SetSyncHookForTest and these tests live under tests/ per AGENTS.md.

// liveRecords counts the records a segment will actually serve, using the public
// Search path rather than reaching into unexported state. A generous k returns
// every live record, so this is a faithful stand-in for the live-set size.
func liveRecords(t *testing.T, idx *diskindex.DiskIndex, dim int) int {
	t.Helper()
	probe := make([]float32, dim)
	for i := range probe {
		probe[i] = 1
	}
	hits, err := idx.Search(context.Background(), probe, 10000, model.Filter{})
	if err != nil {
		t.Fatalf("Search while counting live records: %v", err)
	}
	return len(hits)
}

// countSyncs replaces the package fsync seam with a counter for the duration of
// the test. Tests using it must not run in parallel (shared package state).
func countSyncs(t *testing.T) *int {
	t.Helper()
	n := 0
	restore := diskindex.SetSyncHookForTest(func(f *os.File) error {
		n++
		return f.Sync()
	})
	t.Cleanup(restore)
	return &n
}

func newTestIndex(t *testing.T) (*diskindex.DiskIndex, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), diskindex.SegmentFileName("text"))
	idx := diskindex.New(path)
	t.Cleanup(func() { _ = idx.Close() })
	return idx, path
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
	batched, batchedPath := newTestIndex(t)
	if err := batched.BatchUpsert(ctx, items); err != nil {
		t.Fatalf("BatchUpsert: %v", err)
	}
	if *syncs != 1 {
		t.Fatalf("BatchUpsert of %d vectors did %d fsyncs, want exactly 1", n, *syncs)
	}

	serial, serialPath := newTestIndex(t)
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
	if a, b := liveRecords(t, batched, 4), liveRecords(t, serial, 4); a != b {
		t.Fatalf("live record counts differ: batched=%d serial=%d", a, b)
	}
	if a, b := mustFileSize(t, batchedPath), mustFileSize(t, serialPath); a != b {
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
	idx, _ := newTestIndex(t)
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
	idx, segPath := newTestIndex(t)
	items := append(batchItems(3), model.IndexUpsert{
		Vector:  []float32{1, 0},
		Payload: model.IndexPayload{ChunkID: 0},
	})
	if err := idx.BatchUpsert(ctx, items); err == nil {
		t.Fatal("BatchUpsert with a zero chunk_id must fail")
	}
	if n := liveRecords(t, idx, 4); n != 0 {
		t.Fatalf("live records = %d, want 0 (nothing may be applied)", n)
	}
	if _, err := os.Stat(segPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("segment must not be created by a rejected batch: stat err = %v", err)
	}
}

// TestBatchUpsert_RollsBackFailedBatch pins the crash-safety guarantee: when the
// batch's durability barrier fails, the segment is truncated back to its
// pre-batch length and the in-memory state is untouched, so the index is never
// left half-applied and remains appendable and loadable.
func TestBatchUpsert_RollsBackFailedBatch(t *testing.T) {
	ctx := context.Background()
	idx, segPath := newTestIndex(t)
	seed := batchItems(2)
	if err := idx.BatchUpsert(ctx, seed); err != nil {
		t.Fatalf("seed BatchUpsert: %v", err)
	}
	sizeBefore := mustFileSize(t, segPath)
	liveBefore := liveRecords(t, idx, 4)

	failing := []model.IndexUpsert{
		{Vector: []float32{1, 0, 0, 0}, Payload: model.IndexPayload{ChunkID: 101, RelPath: "x.txt"}},
		{Vector: []float32{0, 1, 0, 0}, Payload: model.IndexPayload{ChunkID: 102, RelPath: "y.txt"}},
	}
	boom := errors.New("simulated fsync failure")
	restore := diskindex.SetSyncHookForTest(func(*os.File) error { return boom })
	err := idx.BatchUpsert(ctx, failing)
	restore()
	if !errors.Is(err, boom) {
		t.Fatalf("BatchUpsert error = %v, want it to surface %v", err, boom)
	}

	if got := mustFileSize(t, segPath); got != sizeBefore {
		t.Fatalf("segment size = %d after a failed batch, want the pre-batch %d (rolled back)", got, sizeBefore)
	}
	if got := liveRecords(t, idx, 4); got != liveBefore {
		t.Fatalf("live set changed on a failed batch: %d -> %d (the batch must roll back entirely)",
			liveBefore, got)
	}
	if n := liveRecords(t, idx, 4); n != len(seed) {
		t.Fatalf("live records = %d after a failed batch, want the seeded %d", n, len(seed))
	}

	// The rolled-back segment must still be appendable and parseable.
	if err := idx.BatchUpsert(ctx, failing); err != nil {
		t.Fatalf("retry BatchUpsert after rollback: %v", err)
	}
	reopened := diskindex.New(segPath)
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Load(ctx, segPath); err != nil {
		t.Fatalf("Load after rollback+retry: %v", err)
	}
	if n := liveRecords(t, reopened, 4); n != len(seed)+len(failing) {
		t.Fatalf("reloaded live set = %d, want %d", n, len(seed)+len(failing))
	}
}

// TestLoad_RecoversTornTail pins that a segment whose tail was torn by an
// ungraceful crash mid-batch loads to the last COMPLETE record instead of
// erroring out, and that the torn bytes are dropped so later appends are still
// readable. Batching widens the window in which unsynced bytes can be in flight,
// so this is the crash mode the batch path must survive.
func TestLoad_RecoversTornTail(t *testing.T) {
	ctx := context.Background()
	idx, segPath := newTestIndex(t)
	if err := idx.BatchUpsert(ctx, batchItems(3)); err != nil {
		t.Fatalf("BatchUpsert: %v", err)
	}
	path := segPath
	full := mustFileSize(t, path)
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Simulate a crash that left only part of the last record on disk.
	if err := os.Truncate(path, full-6); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	reopened := diskindex.New(path)
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Load(ctx, path); err != nil {
		t.Fatalf("Load with a torn tail must recover, got: %v", err)
	}
	if n := liveRecords(t, reopened, 4); n != 2 {
		t.Fatalf("recovered live set = %d, want the 2 complete records", n)
	}
	// The torn tail is dropped rather than served: the count above already
	// excludes it, and the post-recovery append below proves the segment is
	// writable again from the recovered offset.

	// Appending after the recovery must survive another reopen.
	if err := reopened.Upsert(ctx, []float32{9, 9, 9, 9}, model.IndexPayload{ChunkID: 77, RelPath: "z.txt"}); err != nil {
		t.Fatalf("Upsert after recovery: %v", err)
	}
	again := diskindex.New(path)
	t.Cleanup(func() { _ = again.Close() })
	if err := again.Load(ctx, path); err != nil {
		t.Fatalf("reload after recovery: %v", err)
	}
	if n := liveRecords(t, again, 4); n != 3 {
		t.Fatalf("post-recovery append lost: live records = %d, want 3", n)
	}
}

// TestLoad_CorruptOversizedHeaderDoesNotPanic covers a corrupt record header
// whose vecLen/payloadLen encode a body far larger than the file.
//
// Both are uint32, so the computed body length can approach 21.5 GB. Discard
// takes an int, 32-bit on some platforms, where that value wraps and can go
// negative; bufio panics on a negative count, so Load would crash rather than
// fail cleanly. The scan bounds the length first and treats an impossible
// record as a torn tail, which is what it already does for every other
// unreadable record.
//
// Honest limitation: on a 64-bit host int is wide enough that the value stays
// positive and Discard merely hits EOF, so this test passes with or without the
// bound here. It pins the recoverable OUTCOME on every platform and documents
// the corrupt-header case; the panic it guards against is reachable only where
// int is 32 bits.
func TestLoad_CorruptOversizedHeaderDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	idx, segPath := newTestIndex(t)
	mustUpsertDisk(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "good.txt"}, []float32{1, 0, 0, 0})

	raw, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	// Append a header claiming the largest body two uint32 fields can express.
	hdr := make([]byte, 17)
	binary.LittleEndian.PutUint64(hdr[0:8], 99)
	hdr[8] = 0
	binary.LittleEndian.PutUint32(hdr[9:13], ^uint32(0))
	binary.LittleEndian.PutUint32(hdr[13:17], ^uint32(0))
	if err := os.WriteFile(segPath, append(raw, hdr...), 0o600); err != nil {
		t.Fatalf("write corrupt segment: %v", err)
	}

	reopened := diskindex.New(segPath)
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Load(ctx, segPath); err != nil {
		t.Fatalf("Load must recover from a corrupt oversized header, got: %v", err)
	}
	if n := liveRecords(t, reopened, 4); n != 1 {
		t.Fatalf("live records = %d, want the 1 good record before the corruption", n)
	}
}
