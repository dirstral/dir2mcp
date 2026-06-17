package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/scancache"
)

// scanCacheTestConfig builds a config with the scan cache enabled and an
// isolated state dir so the cache sqlite file is created under a temp tree.
func scanCacheTestConfig(t *testing.T, root string) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = filepath.Join(t.TempDir(), "state")
	cfg.IngestScanCache = true
	return cfg
}

// TestServiceRun_ScanCacheUnchangedTreeYieldsSameDocs verifies that a second
// run over an unchanged tree (served from the persisted scan cache) produces the
// same active documents as the first run, and that the cache file is created.
func TestServiceRun_ScanCacheUnchangedTreeYieldsSameDocs(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), []byte("alpha text"))
	mustWriteFile(t, filepath.Join(root, "sub", "b.txt"), []byte("beta text"))

	cfg := scanCacheTestConfig(t, root)

	st := newMemoryStore()
	svc := mustNewIngestService(t, cfg, st)
	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	cachePath := scancache.DefaultPath(cfg.StateDir)
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("scan cache file not created at %s: %v", cachePath, err)
	}

	firstActive := activeDocPaths(st)

	// Second run with the cache warm.
	st2 := newMemoryStore()
	svc2 := mustNewIngestService(t, cfg, st2)
	if err := svc2.Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	secondActive := activeDocPaths(st2)

	assertStringSetsEqual(t, "unchanged tree active docs", firstActive, secondActive)
	if _, ok := secondActive["a.txt"]; !ok {
		t.Fatalf("expected a.txt active after cached run, got %v", secondActive)
	}
	if _, ok := secondActive["sub/b.txt"]; !ok {
		t.Fatalf("expected sub/b.txt active after cached run, got %v", secondActive)
	}
}

// TestServiceRun_ScanCacheDetectsModifiedFile verifies that an in-place
// modification (which does NOT bump the parent directory mtime) is still picked
// up on a cached re-run: the document's content hash changes.
func TestServiceRun_ScanCacheDetectsModifiedFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "sub", "b.txt")
	mustWriteFile(t, filepath.Join(root, "a.txt"), []byte("alpha"))
	mustWriteFile(t, target, []byte("beta original"))

	cfg := scanCacheTestConfig(t, root)
	st := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st).Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	beforeHash := st.docs["sub/b.txt"].ContentHash
	if beforeHash == "" {
		t.Fatalf("expected a content hash for sub/b.txt after first run")
	}

	// Modify in place with a strictly newer mtime to defeat mtime granularity.
	mustWriteFile(t, target, []byte("beta CHANGED and much longer than before"))
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(target, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	st2 := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st2).Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	afterHash := st2.docs["sub/b.txt"].ContentHash
	if afterHash == "" {
		t.Fatalf("sub/b.txt missing after modification re-run")
	}
	if afterHash == beforeHash {
		t.Fatalf("modified file not detected through scan cache: hash unchanged (%s)", afterHash)
	}
}

// TestServiceRun_ScanCacheDetectsAddAndRemove verifies that adding and removing
// files (both bump the parent directory mtime, invalidating the cached
// signature) are reflected on a cached re-run.
func TestServiceRun_ScanCacheDetectsAddAndRemove(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "dir", "keep.txt"), []byte("keep"))
	mustWriteFile(t, filepath.Join(root, "dir", "remove.txt"), []byte("bye"))

	cfg := scanCacheTestConfig(t, root)
	st := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st).Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	mustWriteFile(t, filepath.Join(root, "dir", "added.txt"), []byte("new file"))
	if err := os.Remove(filepath.Join(root, "dir", "remove.txt")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	st2 := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st2).Run(context.Background()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	active := activeDocPaths(st2)
	if _, ok := active["dir/added.txt"]; !ok {
		t.Fatalf("added file not detected through scan cache: %v", active)
	}
	if _, ok := active["dir/remove.txt"]; ok {
		t.Fatalf("removed file still active after scan cache re-run: %v", active)
	}
	if _, ok := active["dir/keep.txt"]; !ok {
		t.Fatalf("kept file missing after scan cache re-run: %v", active)
	}
}

// TestServiceRun_ScanCacheDisabledNoCacheFile verifies the default (cache off)
// performs a full walk and never creates the cache file.
func TestServiceRun_ScanCacheDisabledNoCacheFile(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), []byte("alpha"))
	mustWriteFile(t, filepath.Join(root, "sub", "b.txt"), []byte("beta"))

	cfg := scanCacheTestConfig(t, root)
	cfg.IngestScanCache = false // explicit: default behavior, full walk.

	st := newMemoryStore()
	if err := mustNewIngestService(t, cfg, st).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(scancache.DefaultPath(cfg.StateDir)); !os.IsNotExist(err) {
		t.Fatalf("scan cache file should not exist when disabled (err=%v)", err)
	}

	active := activeDocPaths(st)
	if _, ok := active["a.txt"]; !ok {
		t.Fatalf("a.txt missing on full walk: %v", active)
	}
	if _, ok := active["sub/b.txt"]; !ok {
		t.Fatalf("sub/b.txt missing on full walk: %v", active)
	}
}

func activeDocPaths(st *memoryStore) map[string]struct{} {
	out := map[string]struct{}{}
	for relPath, doc := range st.docs {
		if doc.Deleted {
			continue
		}
		out[relPath] = struct{}{}
	}
	return out
}

func assertStringSetsEqual(t *testing.T, label string, want, got map[string]struct{}) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: set size mismatch want=%v got=%v", label, want, got)
	}
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Fatalf("%s: missing %q in got=%v", label, k, got)
		}
	}
}
