package tests

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// fakeScanCache is an in-memory corpusfs.ScanCache used to observe the LocalFS
// walker's fast-path behavior without touching sqlite. It records per-directory
// signatures and counts hits/misses so tests can assert when the walker reused a
// cached directory versus re-read it.
type fakeScanCache struct {
	sigs    map[string]corpusfs.CachedDirSignature
	hits    int
	misses  int
	stores  int
	lookups int
}

func newFakeScanCache() *fakeScanCache {
	return &fakeScanCache{sigs: map[string]corpusfs.CachedDirSignature{}}
}

func (c *fakeScanCache) LookupDir(relDir string) (corpusfs.CachedDirSignature, bool, error) {
	c.lookups++
	sig, ok := c.sigs[relDir]
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	return sig, ok, nil
}

func (c *fakeScanCache) StoreDir(relDir string, sig corpusfs.CachedDirSignature) error {
	c.stores++
	c.sigs[relDir] = sig
	return nil
}

func relPaths(files []corpusfs.DiscoveredFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.RelPath)
	}
	sort.Strings(out)
	return out
}

func walkWith(t *testing.T, root string, cache corpusfs.ScanCache) []corpusfs.DiscoveredFile {
	t.Helper()
	files, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, corpusfs.Options{
		MaxSizeBytes: 1 << 20,
		ScanCache:    cache,
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return files
}

// TestScanCache_UnchangedTreeSkipsReWalk verifies that a second walk of an
// unchanged tree is served from the cache (every directory is a hit and no
// directory is re-stored) and returns byte-identical discovery results.
func TestScanCache_UnchangedTreeSkipsReWalk(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), []byte("alpha"))
	mustWrite(t, filepath.Join(root, "sub", "b.txt"), []byte("beta"))
	mustWrite(t, filepath.Join(root, "sub", "deep", "c.txt"), []byte("gamma"))

	cache := newFakeScanCache()

	first := walkWith(t, root, cache)
	if cache.stores == 0 {
		t.Fatalf("first walk should populate the cache, got 0 stores")
	}
	firstStores := cache.stores

	// Reset only the counters; keep the populated signatures.
	cache.hits, cache.misses, cache.stores, cache.lookups = 0, 0, 0, 0

	second := walkWith(t, root, cache)

	if cache.misses != 0 {
		t.Fatalf("unchanged tree: expected 0 cache misses, got %d (hits=%d)", cache.misses, cache.hits)
	}
	if cache.hits == 0 {
		t.Fatalf("unchanged tree: expected cache hits on the re-walk, got 0")
	}
	if cache.stores != 0 {
		t.Fatalf("unchanged tree: expected no re-stores on a fully-cached walk, got %d (first=%d)", cache.stores, firstStores)
	}

	if got, want := relPaths(second), relPaths(first); len(got) != len(want) {
		t.Fatalf("result size changed: first=%v second=%v", want, got)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("result mismatch at %d: first=%q second=%q", i, want[i], got[i])
			}
		}
	}
}

// TestScanCache_DetectsModifiedFile verifies that an in-place modification (which
// does NOT bump the parent directory mtime) is still detected: the affected
// directory falls back to a full re-read and the new size is reported.
func TestScanCache_DetectsModifiedFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sub", "b.txt")
	mustWrite(t, filepath.Join(root, "a.txt"), []byte("alpha"))
	mustWrite(t, target, []byte("beta"))

	cache := newFakeScanCache()
	_ = walkWith(t, root, cache)

	// Modify b.txt in place with a strictly newer mtime so the change is
	// unambiguous regardless of filesystem mtime granularity.
	mustWrite(t, target, []byte("beta-modified-and-longer"))
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	files := walkWith(t, root, cache)
	var found bool
	for _, f := range files {
		if f.RelPath == "sub/b.txt" {
			found = true
			if f.SizeBytes != int64(len("beta-modified-and-longer")) {
				t.Fatalf("modified file size not detected: got %d", f.SizeBytes)
			}
		}
	}
	if !found {
		t.Fatalf("modified file sub/b.txt missing from results")
	}
}

// TestScanCache_DetectsAddedAndRemovedFile verifies that adding and removing a
// file (both bump the parent directory mtime) invalidate the cached directory so
// the change is reflected on the next walk.
func TestScanCache_DetectsAddedAndRemovedFile(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "dir", "keep.txt"), []byte("keep"))
	mustWrite(t, filepath.Join(root, "dir", "remove.txt"), []byte("bye"))

	cache := newFakeScanCache()
	_ = walkWith(t, root, cache)

	// Add one file and remove another in the same directory.
	mustWrite(t, filepath.Join(root, "dir", "added.txt"), []byte("new"))
	if err := os.Remove(filepath.Join(root, "dir", "remove.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	got := relPaths(walkWith(t, root, cache))
	want := []string{"dir/added.txt", "dir/keep.txt"}
	if len(got) != len(want) {
		t.Fatalf("add/remove not detected: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("add/remove mismatch: got %v want %v", got, want)
		}
	}
}

// TestScanCache_DisabledMatchesFullWalk verifies that a nil cache (the default)
// reproduces the exact discovery output of a cache-backed walk over the same
// tree: the cache is a pure optimization with no effect on results.
func TestScanCache_DisabledMatchesFullWalk(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), []byte("alpha"))
	mustWrite(t, filepath.Join(root, "sub", "b.txt"), []byte("beta"))
	mustWrite(t, filepath.Join(root, "sub", "deep", "c.txt"), []byte("gamma"))

	nilCacheWalk := relPaths(walkWith(t, root, nil))

	cache := newFakeScanCache()
	_ = walkWith(t, root, cache)                     // populate
	cachedWalk := relPaths(walkWith(t, root, cache)) // served from cache

	if len(nilCacheWalk) != len(cachedWalk) {
		t.Fatalf("cache changed result set: nil=%v cached=%v", nilCacheWalk, cachedWalk)
	}
	for i := range nilCacheWalk {
		if nilCacheWalk[i] != cachedWalk[i] {
			t.Fatalf("cache changed result at %d: nil=%q cached=%q", i, nilCacheWalk[i], cachedWalk[i])
		}
	}
}
