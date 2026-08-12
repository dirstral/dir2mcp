package tests

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index/diskindex"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Issue #674: the batch path rolled a failed append back, the single-record path
// did not. A write or fsync failure left the abandoned bytes in the segment while
// the in-memory append pointer stayed at the old offset, so the next append
// overwrote only part of them and left residue in the middle of the log. The
// tests below drive the failures through the existing fsync seam
// (diskindex.SetSyncHookForTest), so they are deterministic and never depend on
// timing. They share the package state that seam holds, so none of them may run
// in parallel.

// seedPayload674 / seedVector674 build one small, fixed record. Two indexes fed
// the same ids therefore hold byte-identical segments.
func seedPayload674(id uint64) model.IndexPayload {
	return model.IndexPayload{ChunkID: id, RelPath: "seed.txt", DocType: "md"}
}

func seedVector674(id uint64) []float32 {
	return []float32{float32(id), 1, 0, 0}
}

// readSegment674 returns the raw segment bytes.
func readSegment674(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read segment %s: %v", path, err)
	}
	return data
}

// loadSegment674 opens path in a fresh index, exactly as a restart does.
func loadSegment674(t *testing.T, path string) *diskindex.DiskIndex {
	t.Helper()
	idx := diskindex.New(path)
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Load(context.Background(), path); err != nil {
		t.Fatalf("Load %s: %v", path, err)
	}
	return idx
}

// tombstoneSize674 measures one tombstone record on a scratch index built like
// the index under test, so the fault below can be keyed on bytes written.
func tombstoneSize674(t *testing.T) int64 {
	t.Helper()
	scratch, path := newTestIndex(t)
	mustUpsertDisk(t, scratch, seedPayload674(1), seedVector674(1))
	before := mustFileSize(t, path)
	if err := scratch.Delete(context.Background(), []uint64{1}); err != nil {
		t.Fatalf("scratch Delete: %v", err)
	}
	return mustFileSize(t, path) - before
}

// TestUpsert_FailedFsyncLeavesNoBytesInTheLog pins the core of #674 for the
// single-record path: a failed durability barrier must leave the segment at its
// pre-append length, and the append that follows must produce a log that is
// byte-identical to one that never saw the failure.
//
// The abandoned record is much larger than the record that follows it, which is
// the case that leaves residue: the shorter record overwrites only the head of
// the abandoned bytes, and the tail stays in the middle of the log.
func TestUpsert_FailedFsyncLeavesNoBytesInTheLog(t *testing.T) {
	ctx := context.Background()
	idx, segPath := newTestIndex(t)
	mustUpsertDisk(t, idx, seedPayload674(1), seedVector674(1))
	sizeBefore := mustFileSize(t, segPath)
	liveBefore := liveRecords(t, idx, 4)

	big := model.IndexPayload{ChunkID: 2, RelPath: strings.Repeat("p", 4096), DocType: "md"}
	boom := errors.New("simulated fsync failure")
	restore := diskindex.SetSyncHookForTest(func(*os.File) error { return boom })
	err := idx.Upsert(ctx, seedVector674(2), big)
	restore()
	if !errors.Is(err, boom) {
		t.Fatalf("Upsert error = %v, want it to surface %v", err, boom)
	}

	if got := mustFileSize(t, segPath); got != sizeBefore {
		t.Fatalf("segment is %d bytes after a failed upsert, want the pre-append %d: the abandoned record must be truncated away", got, sizeBefore)
	}
	if got := liveRecords(t, idx, 4); got != liveBefore {
		t.Fatalf("live set changed on a failed upsert: %d -> %d", liveBefore, got)
	}

	// The next append must land on a clean log.
	mustUpsertDisk(t, idx, seedPayload674(3), seedVector674(3))

	clean, cleanPath := newTestIndex(t)
	mustUpsertDisk(t, clean, seedPayload674(1), seedVector674(1))
	mustUpsertDisk(t, clean, seedPayload674(3), seedVector674(3))
	got, want := readSegment674(t, segPath), readSegment674(t, cleanPath)
	if !bytes.Equal(got, want) {
		t.Fatalf("segment after a failed upsert is %d bytes, want the %d bytes of a log that never saw the failure: residual bytes remain",
			len(got), len(want))
	}

	reopened := loadSegment674(t, segPath)
	if n := liveRecords(t, reopened, 4); n != 2 {
		t.Fatalf("reloaded live set = %d, want 2 (chunks 1 and 3)", n)
	}
}

// TestUpsert_FailedFirstAppendLeavesNothingToLoad covers the same failure on a
// fresh index, where the append also creates the file and writes the header. The
// header may stay (an empty log is valid), but the record must not: an upsert
// that reported failure must not appear after a restart.
func TestUpsert_FailedFirstAppendLeavesNothingToLoad(t *testing.T) {
	ctx := context.Background()
	idx, segPath := newTestIndex(t)

	boom := errors.New("simulated fsync failure")
	restore := diskindex.SetSyncHookForTest(func(*os.File) error { return boom })
	err := idx.Upsert(ctx, seedVector674(1), seedPayload674(1))
	restore()
	if !errors.Is(err, boom) {
		t.Fatalf("Upsert error = %v, want it to surface %v", err, boom)
	}

	reopened := loadSegment674(t, segPath)
	if n := liveRecords(t, reopened, 4); n != 0 {
		t.Fatalf("reloaded live set = %d, want 0: an upsert that reported failure must not survive a restart", n)
	}

	// The rolled-back segment must still accept records.
	mustUpsertDisk(t, idx, seedPayload674(1), seedVector674(1))
	again := loadSegment674(t, segPath)
	if n := liveRecords(t, again, 4); n != 1 {
		t.Fatalf("live set after the retry = %d, want 1", n)
	}
}

// TestDelete_AppliesEveryTombstoneOrNone pins the multi-id delete contract: all
// ids or none. The old loop made each tombstone durable on its own, so a failure
// on a later id left the earlier ids deleted and still reported failure for the
// whole call.
//
// The fault is keyed on the bytes already in the file, not on a call count or a
// timer, so it hits the same point whether the tombstones are synced one by one
// or once for the whole call.
func TestDelete_AppliesEveryTombstoneOrNone(t *testing.T) {
	ctx := context.Background()
	idx, segPath := newTestIndex(t)
	ids := []uint64{1, 2, 3}
	for _, id := range ids {
		mustUpsertDisk(t, idx, seedPayload674(id), seedVector674(id))
	}
	base := mustFileSize(t, segPath)
	tombstone := tombstoneSize674(t)

	boom := errors.New("simulated fsync failure")
	limit := base + tombstone
	restore := diskindex.SetSyncHookForTest(func(f *os.File) error {
		info, statErr := f.Stat()
		if statErr != nil {
			return statErr
		}
		if info.Size() > limit {
			return boom
		}
		return f.Sync()
	})
	err := idx.Delete(ctx, ids)
	restore()
	if !errors.Is(err, boom) {
		t.Fatalf("Delete error = %v, want it to surface %v", err, boom)
	}

	if n := liveRecords(t, idx, 4); n != len(ids) {
		t.Fatalf("live set = %d after a failed delete, want all %d: a multi-id delete must not half-apply", n, len(ids))
	}
	if got := mustFileSize(t, segPath); got != base {
		t.Fatalf("segment is %d bytes after a failed delete, want the pre-delete %d: the tombstones must be truncated away", got, base)
	}

	reopened := loadSegment674(t, segPath)
	if n := liveRecords(t, reopened, 4); n != len(ids) {
		t.Fatalf("reloaded live set = %d, want all %d: no tombstone of a failed delete may survive a restart", n, len(ids))
	}

	// Once the fault clears, the same delete must remove every id.
	if err := idx.Delete(ctx, ids); err != nil {
		t.Fatalf("retry Delete after rollback: %v", err)
	}
	if n := liveRecords(t, idx, 4); n != 0 {
		t.Fatalf("live set = %d after the retry, want 0", n)
	}
	retried := loadSegment674(t, segPath)
	if n := liveRecords(t, retried, 4); n != 0 {
		t.Fatalf("reloaded live set = %d after the retry, want 0", n)
	}
}

// TestLoad_TreatsAnEmptySegmentFileAsFresh covers the other half of a failed
// first append: the append path creates the segment file before it writes the
// header, so a failure or a crash in that window leaves a file with no bytes.
// Such a file states nothing, so startup must read it as a fresh index instead
// of refusing to open the corpus.
func TestLoad_TreatsAnEmptySegmentFileAsFresh(t *testing.T) {
	ctx := context.Background()
	segPath := filepath.Join(t.TempDir(), diskindex.SegmentFileName("text"))
	if err := os.WriteFile(segPath, nil, 0o600); err != nil {
		t.Fatalf("create empty segment: %v", err)
	}

	idx := diskindex.New(segPath)
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Load(ctx, segPath); err != nil {
		t.Fatalf("Load of an empty segment must succeed, got: %v", err)
	}
	if n := liveRecords(t, idx, 4); n != 0 {
		t.Fatalf("live set = %d on an empty segment, want 0", n)
	}

	// The header is written by the next append, so the index stays usable.
	mustUpsertDisk(t, idx, seedPayload674(1), seedVector674(1))
	reopened := loadSegment674(t, segPath)
	if n := liveRecords(t, reopened, 4); n != 1 {
		t.Fatalf("reloaded live set = %d, want 1", n)
	}
}

// TestLoad_EmptySegmentIgnoresADamagedIdentitySidecar pairs the empty segment
// with a sidecar that cannot be read. A damaged sidecar over a POPULATED segment
// stays a hard error, because it decides the fate of vectors the index still
// holds (#728). An empty segment holds none, so there is nothing to protect and
// nothing to report: the missing-segment path already reads a damaged sidecar
// this way.
func TestLoad_EmptySegmentIgnoresADamagedIdentitySidecar(t *testing.T) {
	ctx := context.Background()
	segPath := filepath.Join(t.TempDir(), diskindex.SegmentFileName("text"))
	if err := os.WriteFile(segPath, nil, 0o600); err != nil {
		t.Fatalf("create empty segment: %v", err)
	}
	if err := os.WriteFile(segPath+diskindex.IdentitySidecarSuffix, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("create damaged sidecar: %v", err)
	}

	idx := diskindex.New(segPath)
	t.Cleanup(func() { _ = idx.Close() })
	if err := idx.Load(ctx, segPath); err != nil {
		t.Fatalf("Load of an empty segment with a damaged sidecar must succeed, got: %v", err)
	}
	identity, err := idx.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if identity != "" {
		t.Fatalf("identity = %q, want empty: a damaged sidecar over an empty segment states nothing", identity)
	}
}
