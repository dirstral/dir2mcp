package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// Regression suite for #667: the scan cache compared a directory's mtime and its
// children's mtimes at SECOND resolution, so a change that landed inside the
// recorded Unix second read as no change at all.
//
// Every timestamp below is set with os.Chtimes, and no assertion depends on how
// long the test took. That has to hold in BOTH directions:
//
//   - a test that needs a directory to be SETTLED stamps it with anchor667, years
//     in the past, so no amount of speed can make it unsettled;
//   - a test that needs a directory to be UNSETTLED stamps it with
//     unsettledStamp667, an hour ahead, so no amount of stalling can settle it.
//
// Stamping "now" would satisfy neither: the outcome would then turn on whether the
// walk started inside the settle window, which is a timing coincidence and not the
// property under test.

// anchor667 is a fixed instant with a zero nanosecond part, so a test can place a
// second write at a chosen offset INSIDE the same Unix second. It is years in the
// past, which also makes any directory stamped with it "settled" (see
// mtimeSettleWindow), so these tests exercise the mtime comparison rather than the
// settle guard. TestScanCache667_CoarseTimestampFilesystemStillSeesAnAdd covers
// the guard.
var anchor667 = time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

// setMTime667 stamps a path with an exact instant and confirms the filesystem kept
// it. The confirmation matters: a filesystem that silently truncated the stamp
// would make a test pass for the wrong reason.
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
		t.Skipf("filesystem does not keep the requested mtime for %s: want %s got %s",
			path, ts, info.ModTime())
	}
}

// requireSubSecondMTime667 skips the calling test when the filesystem under root
// cannot hold a sub-second mtime. A test that needs two writes inside one second
// to be distinguishable has nothing to assert on such a filesystem.
func requireSubSecondMTime667(t *testing.T, root string) {
	t.Helper()
	probe := filepath.Join(root, ".mtime-probe-667")
	mustWrite(t, probe, []byte("probe"))
	defer func() { _ = os.Remove(probe) }()
	ts := anchor667.Add(123 * time.Millisecond)
	if err := os.Chtimes(probe, ts, ts); err != nil {
		t.Skipf("chtimes probe: %v", err)
	}
	info, err := os.Stat(probe)
	if err != nil {
		t.Skipf("stat probe: %v", err)
	}
	if info.ModTime().UnixNano()%int64(time.Second) == 0 {
		t.Skip("filesystem keeps whole-second mtimes only; sub-second cases are not observable here")
	}
}

// TestScanCache667_SameSecondAddIsDiscovered is the user-visible defect.
//
// A file added to a directory whose mtime stays in the recorded Unix second was
// never enumerated. Discovery reported the corpus as if the file did not exist,
// and it stayed invisible until something else changed that directory.
func TestScanCache667_SameSecondAddIsDiscovered(t *testing.T) {
	root := t.TempDir()
	requireSubSecondMTime667(t, root)
	dir := filepath.Join(root, "dir")
	mustWrite(t, filepath.Join(dir, "a.txt"), []byte("alpha"))

	// Age the tree so the first walk is allowed to cache it.
	setMTime667(t, dir, anchor667)
	setMTime667(t, root, anchor667)

	cache := newFakeScanCache()
	if got := relPaths(walkWith(t, root, cache)); len(got) != 1 {
		t.Fatalf("first walk: got %v want [dir/a.txt]", got)
	}
	if cache.stores == 0 {
		t.Fatalf("first walk stored no signature, so this test cannot observe a cache hit")
	}

	// Add a file, then put the directory mtime back inside the SAME Unix second at
	// a later nanosecond. This is what a 1-second-granularity clock reading would
	// look like for two writes in one second.
	mustWrite(t, filepath.Join(dir, "b.txt"), []byte("beta"))
	setMTime667(t, dir, anchor667.Add(700*time.Millisecond))

	got := relPaths(walkWith(t, root, cache))
	want := []string{"dir/a.txt", "dir/b.txt"}
	assertRelPaths667(t, "second walk", got, want)
}

// TestScanCache667_SameSizeSameSecondEditIsReObserved constructs the exact case
// the issue names: same size, same Unix second, different bytes.
//
// The cached child entry matched on (size, second), so the directory kept its
// cache hit and the cache kept its stale stamp for that file. Both must change:
// the directory is re-read, and the signature now records the new mtime.
func TestScanCache667_SameSizeSameSecondEditIsReObserved(t *testing.T) {
	root := t.TempDir()
	requireSubSecondMTime667(t, root)
	dir := filepath.Join(root, "dir")
	target := filepath.Join(dir, "a.txt")
	mustWrite(t, target, []byte("AAAAA"))

	setMTime667(t, target, anchor667)
	setMTime667(t, dir, anchor667)
	setMTime667(t, root, anchor667)

	cache := newFakeScanCache()
	_ = walkWith(t, root, cache)
	if cache.stores == 0 {
		t.Fatalf("first walk stored no signature, so this test cannot observe a cache hit")
	}

	// Same length, different bytes, mtime moved only below the second.
	mustWrite(t, target, []byte("BBBBB"))
	edited := anchor667.Add(400 * time.Millisecond)
	setMTime667(t, target, edited)
	setMTime667(t, dir, anchor667) // an in-place write leaves the parent stamp alone

	cache.hits, cache.misses, cache.stores, cache.lookups = 0, 0, 0, 0
	assertRelPaths667(t, "second walk", relPaths(walkWith(t, root, cache)), []string{"dir/a.txt"})

	if cache.stores == 0 {
		t.Fatalf("the edited file was accepted as unchanged: the directory was served from cache (stores=0)")
	}
	sig, ok := cache.sigs["dir"]
	if !ok || len(sig.Entries) != 1 {
		t.Fatalf("no refreshed signature for dir: %+v", sig)
	}
	if got, want := sig.Entries[0].MTimeUnixNano, edited.UnixNano(); got != want {
		t.Fatalf("cache kept a stale stamp for the edited file: got %d want %d", got, want)
	}
}

// unsettledStamp667 returns a stamp that no walk in this test run can consider
// settled, whatever the host does between the Chtimes call and the walk.
//
// "Not settled" is ONE predicate with two ways in: a stamp too RECENT (what a
// coarse-granularity filesystem reports for a write happening now) and a stamp in
// the FUTURE (a corpus on a mount whose clock runs ahead of this host). Both reach
// the same comparison, so a future stamp exercises the same branch.
//
// Using time.Now() instead would make the assertion depend on the walk starting
// within the settle window. A host that stalled for longer would settle the
// directory, a CORRECT walker would then cache it, and the test would go red with
// no defect present. That is the same failure shape this PR is about: an outcome
// decided by a timing coincidence rather than by the property under test. The
// offset is an hour, so only an hour-long stall could reach it.
func unsettledStamp667() time.Time {
	return time.Now().Add(time.Hour).Truncate(time.Second)
}

// TestScanCache667_CoarseTimestampFilesystemStillSeesAnAdd covers what nanosecond
// comparison alone cannot: a filesystem whose mtime granularity is a whole second
// (ext3, many NFS mounts) or two seconds (FAT32).
//
// On such a filesystem both writes below produce the SAME stamp, so no comparison
// of stamps can tell them apart. The settle guard is what saves it: a directory
// whose stamp is not settled is not cached at all, so the next walk reads it and
// sees the new file.
//
// The coarse filesystem is simulated in the part that matters and can be
// controlled: the stamp does not move across the add, and it is not settled when
// the first walk reads it. See unsettledStamp667 for why the stamp is not "now".
func TestScanCache667_CoarseTimestampFilesystemStillSeesAnAdd(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	mustWrite(t, filepath.Join(dir, "a.txt"), []byte("alpha"))

	bucket := unsettledStamp667()
	setMTime667(t, dir, bucket)
	setMTime667(t, root, bucket)

	cache := newFakeScanCache()
	if got := relPaths(walkWith(t, root, cache)); len(got) != 1 {
		t.Fatalf("first walk: got %v want [dir/a.txt]", got)
	}

	// A write later in the same tick, reported by the same coarse clock: the stamp
	// does not move at all.
	mustWrite(t, filepath.Join(dir, "b.txt"), []byte("beta"))
	setMTime667(t, dir, bucket)

	got := relPaths(walkWith(t, root, cache))
	assertRelPaths667(t, "second walk", got, []string{"dir/a.txt", "dir/b.txt"})
}

// TestScanCache667_UnsettledDirectoryIsNotCached pins the settle guard directly. A
// directory whose stamp is not settled cannot be cached safely, because a further
// write in the same timestamp tick would leave that stamp unchanged.
//
// The refusal must be scoped to the affected directory, so the settled root in the
// same tree must still take the cheap path.
func TestScanCache667_UnsettledDirectoryIsNotCached(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	mustWrite(t, filepath.Join(dir, "a.txt"), []byte("alpha"))
	settleDirs667(t, root)
	setMTime667(t, dir, unsettledStamp667())

	cache := newFakeScanCache()
	if got := relPaths(walkWith(t, root, cache)); len(got) != 1 {
		t.Fatalf("walk: got %v want [dir/a.txt]", got)
	}
	if sig, ok := cache.sigs["dir"]; ok {
		t.Fatalf("an unsettled directory was cached: %+v", sig)
	}
	if _, ok := cache.sigs[""]; !ok {
		t.Fatalf("the settled root was not cached; the refusal must be scoped to the affected directory")
	}
}

// TestScanCache667_SettledTreeKeepsTheCheapPath is the false-positive guard. The
// fix must not cost the case the cache exists for: an unchanged, settled corpus
// is still served entirely from the cache, with no directory re-read and no
// re-store.
func TestScanCache667_SettledTreeKeepsTheCheapPath(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), []byte("alpha"))
	mustWrite(t, filepath.Join(root, "sub", "b.txt"), []byte("beta"))
	mustWrite(t, filepath.Join(root, "sub", "deep", "c.txt"), []byte("gamma"))
	settleDirs667(t, root)

	cache := newFakeScanCache()
	first := relPaths(walkWith(t, root, cache))
	if cache.stores != 3 {
		t.Fatalf("settled tree: want a signature for all 3 directories, got %d", cache.stores)
	}

	cache.hits, cache.misses, cache.stores, cache.lookups = 0, 0, 0, 0
	second := relPaths(walkWith(t, root, cache))

	assertRelPaths667(t, "re-walk", second, first)
	if cache.misses != 0 {
		t.Fatalf("settled unchanged tree: want 0 cache misses, got %d (hits=%d)", cache.misses, cache.hits)
	}
	if cache.hits == 0 {
		t.Fatalf("settled unchanged tree: want cache hits on the re-walk, got 0")
	}
	if cache.stores != 0 {
		t.Fatalf("settled unchanged tree: want no re-stores, got %d", cache.stores)
	}
}

// TestScanCache667_SecondsSignatureIsNotTrusted covers the upgrade path. A
// signature written by an older build states seconds where this build reads
// nanoseconds. It must not be trusted, and the directory must be re-read and
// re-stored with the full precision.
func TestScanCache667_SecondsSignatureIsNotTrusted(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), []byte("alpha"))
	settleDirs667(t, root)

	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	cache := newFakeScanCache()
	cache.sigs[""] = corpusfsSecondsSignature667(t, root, info.ModTime())

	got := relPaths(walkWith(t, root, cache))
	assertRelPaths667(t, "walk over a seconds signature", got, []string{"a.txt"})
	if cache.stores == 0 {
		t.Fatalf("a seconds signature was trusted: the directory was served from cache")
	}
	if got, want := cache.sigs[""].DirMTimeUnixNano, info.ModTime().UnixNano(); got != want {
		t.Fatalf("signature not rewritten in nanoseconds: got %d want %d", got, want)
	}
}

// settleDirs667 stamps every directory in the tree with a fixed instant years in
// the past, so the walk is allowed to cache them. Only a directory's own stamp
// gates the child-list cache, so files are left alone. It runs after the files are
// written, because writing a file bumps its parent's stamp.
func settleDirs667(t *testing.T, root string) {
	t.Helper()
	var dirs []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for _, dir := range dirs {
		setMTime667(t, dir, anchor667)
	}
}

// corpusfsSecondsSignature667 builds the signature an older build would have
// written for dir: every timestamp in whole SECONDS, in the fields this build
// reads as nanoseconds.
func corpusfsSecondsSignature667(t *testing.T, dir string, dirMTime time.Time) corpusfs.CachedDirSignature {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	sig := corpusfs.CachedDirSignature{DirMTimeUnixNano: dirMTime.Unix()}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		sig.Entries = append(sig.Entries, corpusfs.CachedDirEntry{
			Name:          e.Name(),
			IsDir:         e.IsDir(),
			SizeBytes:     info.Size(),
			MTimeUnixNano: info.ModTime().Unix(),
			Mode:          uint32(info.Mode()),
		})
	}
	return sig
}

func assertRelPaths667(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v want %v", label, got, want)
		}
	}
}
