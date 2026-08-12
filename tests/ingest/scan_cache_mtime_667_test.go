package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Regression suite for #667, end to end through a real scan with the sqlite scan
// cache enabled. The corpusfs suite pins the walker; this pins what an operator
// sees: which documents the run indexed.
//
// Every timestamp is set with os.Chtimes, and no assertion depends on how long the
// test took. A directory that must be SETTLED is stamped years in the past
// (anchor667); a directory that must be UNSETTLED is stamped an hour ahead
// (unsettledStamp667). Stamping "now" would make the outcome turn on whether the
// scan started inside the settle window.

// anchor667 is a fixed instant with a zero nanosecond part, so a second write can
// be placed inside the same Unix second. It is years in the past, so a directory
// stamped with it is settled and therefore cacheable.
var anchor667 = time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

func setMTime667(t *testing.T, path string, ts time.Time) {
	t.Helper()
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.ModTime().Equal(ts) {
		t.Skipf("filesystem does not keep the requested mtime for %s: want %s got %s", path, ts, info.ModTime())
	}
}

// TestServiceRun667_SameSecondAddIsIndexed is the operator-visible defect.
//
// A file added to a directory whose stamp stayed in the recorded Unix second was
// never enumerated, so the run never indexed it, never skipped it, and never
// errored on it. `search` and `ask` could not reach it, and `dir2mcp status`
// reported a clean, complete scan. It stayed that way until something else changed
// that directory or the cache file was deleted.
func TestServiceRun667_SameSecondAddIsIndexed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	mustWriteFile(t, filepath.Join(dir, "a.txt"), []byte("alpha text"))
	setMTime667(t, dir, anchor667)
	setMTime667(t, root, anchor667)

	cfg := scanCacheTestConfig(t, root)

	st := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st).Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, ok := activeDocPaths(st)["dir/a.txt"]; !ok {
		t.Fatalf("first run did not index dir/a.txt: %v", activeDocPaths(st))
	}

	// Add a file and put the directory stamp back inside the same Unix second.
	mustWriteFile(t, filepath.Join(dir, "b.txt"), []byte("beta text"))
	setMTime667(t, dir, anchor667.Add(700*time.Millisecond))

	st2 := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st2).Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	active := activeDocPaths(st2)
	if _, ok := active["dir/b.txt"]; !ok {
		t.Fatalf("a file added inside the recorded mtime second was never indexed: %v", active)
	}
	if _, ok := active["dir/a.txt"]; !ok {
		t.Fatalf("the pre-existing file was lost: %v", active)
	}
}

// unsettledStamp667 returns a stamp that no scan in this test run can consider
// settled, whatever the host does between the Chtimes call and the scan.
//
// "Not settled" is ONE predicate with two ways in: a stamp too RECENT (what a
// coarse-granularity filesystem reports for a write happening now) and a stamp in
// the FUTURE (a corpus on a mount whose clock runs ahead of this host). Both reach
// the same comparison.
//
// Using time.Now() instead would make the assertion depend on the scan starting
// within the settle window. A host that stalled for longer would settle the
// directory, a CORRECT walker would cache it, and the test would go red with no
// defect present.
func unsettledStamp667() time.Time {
	return time.Now().Add(time.Hour).Truncate(time.Second)
}

// TestServiceRun667_CoarseTimestampAddIsIndexed is the same defect on a filesystem
// whose mtime granularity is a whole second or coarser, where no comparison of
// stamps can separate the two writes. The settle guard is what covers it: a
// directory whose stamp is not settled is not cached, so the next run reads it and
// finds the new file.
//
// The coarse filesystem is simulated in the part that matters and can be
// controlled: the stamp does not move across the add, and it is not settled when
// the first scan reads it. See unsettledStamp667 for why the stamp is not "now".
func TestServiceRun667_CoarseTimestampAddIsIndexed(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	mustWriteFile(t, filepath.Join(dir, "a.txt"), []byte("alpha text"))

	bucket := unsettledStamp667()
	setMTime667(t, dir, bucket)
	setMTime667(t, root, bucket)

	cfg := scanCacheTestConfig(t, root)
	st := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st).Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// A write later in the same tick leaves the coarse stamp exactly where it was.
	mustWriteFile(t, filepath.Join(dir, "b.txt"), []byte("beta text"))
	setMTime667(t, dir, bucket)

	st2 := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st2).Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if _, ok := activeDocPaths(st2)["dir/b.txt"]; !ok {
		t.Fatalf("a file added inside a coarse mtime tick was never indexed: %v", activeDocPaths(st2))
	}
}

// TestServiceRun667_SameSizeSameSecondEditUpdatesContentHash constructs the exact
// case the issue names (same size, same Unix second, different bytes) and pins the
// answer at the level an operator cares about: the stored content_hash.
//
// This case is NOT a content-staleness bug, and this test passes on main. The scan
// cache never gated the byte read: a validated child is emitted with its live stat,
// and SPEC §7.8 makes content_hash the confirm step for a local corpus, so the
// edit is caught by the hash whatever the cache decided. The test is here to keep
// that true, and to record that the same root cause is only a correctness defect
// where a cached child LIST is served, not where a cached child stamp is compared.
func TestServiceRun667_SameSizeSameSecondEditUpdatesContentHash(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	target := filepath.Join(dir, "a.txt")
	mustWriteFile(t, target, []byte("AAAAAAAAAA"))
	setMTime667(t, target, anchor667)
	setMTime667(t, dir, anchor667)
	setMTime667(t, root, anchor667)

	cfg := scanCacheTestConfig(t, root)
	st := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st).Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	before := st.docs["dir/a.txt"].ContentHash
	if before == "" {
		t.Fatalf("first run recorded no content hash for dir/a.txt")
	}

	mustWriteFile(t, target, []byte("BBBBBBBBBB")) // same length, different bytes
	setMTime667(t, target, anchor667.Add(400*time.Millisecond))
	setMTime667(t, dir, anchor667)

	st2 := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st2).Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	after := st2.docs["dir/a.txt"].ContentHash
	if after == "" {
		t.Fatalf("dir/a.txt is missing after the edit")
	}
	if after == before {
		t.Fatalf("a same-size edit inside one mtime second was treated as unchanged: hash still %s", after)
	}
}
