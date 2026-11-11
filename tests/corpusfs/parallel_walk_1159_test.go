package tests

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
)

// SPEC §7.1 (0.92.0), issue #1159: ingest.discovery_workers lets the LocalFS
// walker read directories concurrently. Discovery's result must not depend on
// the value: the same files in the same order, the same skip reports (order
// aside), the same ignore and symlink rules and the same scan-cache contents.

// buildHashTree writes a small version of the archive's layout: a hash tree
// three levels deep, one media folder per leaf, plus the cases discovery
// filters: an oversized file, a SMIL backup, an excluded directory, a
// .gitignore'd file and (where the platform allows it) a symlink. haveLink
// reports whether the symlink exists. under nests the whole tree below one
// directory of that name ("" for none).
func buildHashTree(t *testing.T, under string) (root string, haveLink bool) {
	t.Helper()
	top := t.TempDir()
	root = top
	if under != "" {
		root = filepath.Join(top, under)
	}
	write := func(rel string, size int) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for a := 0; a < 6; a++ {
		for b := 0; b < 5; b++ {
			for c := 0; c < 4; c++ {
				dir := fmt.Sprintf("%02x/%02x/%02x", a, b, c)
				stem := fmt.Sprintf("%02x%02x%02x", a, b, c)
				write(dir+"/"+stem+"_720p.mp4", 64)
				write(dir+"/"+stem+".smil", 32)
				write(dir+"/"+stem+".ru.vtt", 16)
			}
		}
	}
	write("00/00/00/huge.mp4", 4096)                // over the cap below
	write("00/00/00/x.smil.bak.20260101_000000", 8) // SMIL backup: never discovered
	write("01/node_modules/dep.js", 8)              // excluded directory
	if err := os.WriteFile(filepath.Join(root, "02", ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write("02/00/00/noise.log", 8) // gitignored
	// Windows without the symlink privilege cannot create one; the tests then
	// check everything else.
	haveLink = os.Symlink(filepath.Join(root, "03"), filepath.Join(root, "04", "link-to-03")) == nil
	// The scan cache stores a directory only once its mtime has settled (2 s),
	// so age every directory; otherwise the cache paths would never run.
	old := time.Now().Add(-time.Hour)
	if err := filepath.WalkDir(top, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		return os.Chtimes(p, old, old)
	}); err != nil {
		t.Fatal(err)
	}
	return top, haveLink
}

// walkReport is one walk's observable outcome.
type walkReport struct {
	files    []corpusfs.DiscoveredFile
	oversize []string
	symlinks []string
	cache    map[string]corpusfs.CachedDirSignature
}

func walkWorkers(t *testing.T, root string, workers int, follow bool, cache *fakeScanCache) walkReport {
	t.Helper()
	var r walkReport
	var mu sync.Mutex
	opts := corpusfs.Options{
		MaxSizeBytes:   1024,
		UseGitIgnore:   true,
		FollowSymlinks: follow,
		Workers:        workers,
		OnOversize: func(rel string, _ int64) {
			mu.Lock()
			r.oversize = append(r.oversize, rel)
			mu.Unlock()
		},
		OnSkippedSymlink: func(rel string) {
			mu.Lock()
			r.symlinks = append(r.symlinks, rel)
			mu.Unlock()
		},
	}
	if cache != nil {
		opts.ScanCache = cache
	}
	files, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, opts)
	if err != nil {
		t.Fatalf("Walk(workers=%d): %v", workers, err)
	}
	r.files = files
	sort.Strings(r.oversize)
	sort.Strings(r.symlinks)
	if cache != nil {
		r.cache = cache.sigs
	}
	return r
}

// TestParallelWalk_ResultDoesNotDependOnWorkers pins the §7.1 rule for the
// default symlink policy (360 media files plus the .gitignore itself), with and
// without a scan cache, on a first and a
// cached second walk. Run under -race it also proves the walker serialises
// the caller's hooks and the (non-thread-safe) cache.
func TestParallelWalk_ResultDoesNotDependOnWorkers(t *testing.T) {
	// "" is the archive's shape (many top-level folders); "only" puts the whole
	// tree below one directory, where a scheduler that pins a reader to a
	// subtree would fall back to a sequential walk.
	for _, under := range []string{"", "only"} {
		t.Run("under="+under, func(t *testing.T) { checkWorkersEquivalent(t, under) })
	}
}

func checkWorkersEquivalent(t *testing.T, under string) {
	root, haveLink := buildHashTree(t, under)
	wantLinks := 0
	if haveLink {
		wantLinks = 1
	}
	want := walkWorkers(t, root, 1, false, nil)
	if len(want.files) != 6*5*4*3+1 || len(want.oversize) != 1 || len(want.symlinks) != wantLinks {
		t.Fatalf("fixture: %d files, oversize %v, symlinks %v", len(want.files), want.oversize, want.symlinks)
	}
	for _, workers := range []int{0, 2, 8, 64} {
		got := walkWorkers(t, root, workers, false, nil)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("workers=%d: result differs from the sequential walk", workers)
		}
	}

	seqCache := newFakeScanCache()
	wantCached := walkWorkers(t, root, 1, false, seqCache)
	wantCachedAgain := walkWorkers(t, root, 1, false, seqCache)
	if len(wantCached.cache) == 0 || seqCache.hits == 0 {
		t.Fatalf("fixture: the sequential walk stored %d directory signatures and hit %d; the cache paths are not exercised", len(wantCached.cache), seqCache.hits)
	}
	for _, workers := range []int{4, 16} {
		cache := newFakeScanCache()
		first := walkWorkers(t, root, workers, false, cache)
		if !reflect.DeepEqual(first.files, wantCached.files) || !reflect.DeepEqual(first.cache, wantCached.cache) {
			t.Fatalf("workers=%d: first cached walk differs (files or cache contents)", workers)
		}
		second := walkWorkers(t, root, workers, false, cache)
		if !reflect.DeepEqual(second.files, wantCachedAgain.files) || cache.hits == 0 {
			t.Fatalf("workers=%d: cached re-walk differs or never used the cache (hits=%d)", workers, cache.hits)
		}
	}
}

// TestParallelWalk_FollowSymlinksStaysSequential pins that following symlinks
// gives the sequential result for any worker count: which path claims a
// directory first decides its files' rel_path, so it must not race.
func TestParallelWalk_FollowSymlinksStaysSequential(t *testing.T) {
	root, haveLink := buildHashTree(t, "")
	if !haveLink {
		t.Skip("symlinks cannot be created here")
	}
	want := walkWorkers(t, root, 1, true, nil)
	for _, workers := range []int{8, 64} {
		if got := walkWorkers(t, root, workers, true, nil); !reflect.DeepEqual(got, want) {
			t.Fatalf("workers=%d with follow_symlinks: result differs from the sequential walk", workers)
		}
	}
}

// TestParallelWalk_ErrorStopsTheWalk pins that a directory the walk cannot read
// fails discovery for any worker count, as the sequential walk does.
func TestParallelWalk_ErrorStopsTheWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod does not deny directory reads on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	root, _ := buildHashTree(t, "")
	locked := filepath.Join(root, "05", "03")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	for _, workers := range []int{1, 8} {
		_, err := corpusfs.NewLocalFS(root).Walk(context.Background(), root, corpusfs.Options{
			MaxSizeBytes: 1024, Workers: workers,
		})
		if err == nil || !errors.Is(err, os.ErrPermission) {
			t.Fatalf("workers=%d: want a permission error, got %v", workers, err)
		}
	}
}

// TestParallelWalk_HonoursCancellation pins that a cancelled context stops a
// parallel walk with the context's error.
func TestParallelWalk_HonoursCancellation(t *testing.T) {
	root, _ := buildHashTree(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := corpusfs.NewLocalFS(root).Walk(ctx, root, corpusfs.Options{MaxSizeBytes: 1024, Workers: 8})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
