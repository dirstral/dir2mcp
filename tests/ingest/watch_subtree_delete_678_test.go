package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
)

// seedSubtreeCorpus builds the corpus the #678 tests use and returns the
// watched root.
//
// "docs/a/b/deep.md" is the descendant that must stop being searchable.
// "docs/bundle.zip" holds a member whose rel_path is synthetic
// ("docs/bundle.zip/inner.txt"); no filesystem event ever names it, so it is
// only reachable through the subtree pass.
// "docsets/keep.md" is the trap: the store lists documents by a LIKE prefix,
// which drops the trailing slash and compares ASCII letters case-insensitively,
// so a naive prefix match on "docs" would tombstone it too. "keep.txt" is an
// unrelated control.
func seedSubtreeCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs", "a", "b"), 0o755); err != nil {
		t.Fatalf("mkdir docs tree: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "docsets"), 0o755); err != nil {
		t.Fatalf("mkdir docsets: %v", err)
	}
	writes := map[string][]byte{
		filepath.Join(root, "docs", "top.md"):            []byte("top of the removed tree"),
		filepath.Join(root, "docs", "a", "b", "deep.md"): []byte("buried in the removed tree"),
		filepath.Join(root, "docs", "bundle.zip"):        buildZip(t, map[string]string{"inner.txt": "packed in the removed tree"}),
		filepath.Join(root, "docsets", "keep.md"):        []byte("a sibling that must survive"),
		filepath.Join(root, "keep.txt"):                  []byte("an unrelated control"),
	}
	for path, body := range writes {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root
}

// subtreeSeededPaths lists every document the seeded corpus must hold before a
// test mutates it.
var subtreeSeededPaths = []string{
	"docs/top.md",
	"docs/a/b/deep.md",
	"docs/bundle.zip/inner.txt",
	"docsets/keep.md",
	"keep.txt",
}

// subtreeRemovedPaths lists every document that must stop being searchable once
// the "docs" directory leaves the corpus.
var subtreeRemovedPaths = []string{
	"docs/top.md",
	"docs/a/b/deep.md",
	"docs/bundle.zip/inner.txt",
}

// startSubtreeWatcher runs the initial scan, asserts the seeded corpus is
// indexed, and starts the watcher.
func startSubtreeWatcher(t *testing.T, ctx context.Context, root string) *store.SQLiteStore {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.IngestWatchDebounce = 20 * time.Millisecond
	svc := mustNewIngestService(t, cfg, st)

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("initial Run: %v", err)
	}
	indexed := docPaths(t, st)
	for _, rel := range subtreeSeededPaths {
		if !indexed[rel] {
			t.Fatalf("expected %s indexed after initial scan; got %v", rel, indexed)
		}
	}

	startWatcher(t, ctx, svc)
	return st
}

// assertSurvivorsKept fails when the subtree pass took a document it must not
// take. To remove chunks is destructive, so the sibling that shares a LIKE
// prefix and the unrelated control must both stay searchable.
func assertSurvivorsKept(t *testing.T, st *store.SQLiteStore) {
	t.Helper()
	paths := docPaths(t, st)
	if !paths["docsets/keep.md"] {
		t.Errorf("docsets/keep.md was tombstoned; the subtree pass matched a prefix instead of a path")
	}
	if !paths["keep.txt"] {
		t.Errorf("keep.txt was tombstoned; the subtree pass is too broad")
	}
}

// TestWatch_TombstonesDescendantsOfMovedOutDir covers issue #678.
//
// A directory renamed out of the watched root produces ONE fsnotify event, for
// the directory itself. The store holds no document at that path, only
// documents below it, so before the fix every descendant stayed searchable
// until the ten-minute safety rescan.
func TestWatch_TombstonesDescendantsOfMovedOutDir(t *testing.T) {
	requireWatchIntegration(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := seedSubtreeCorpus(t)
	st := startSubtreeWatcher(t, ctx, root)

	// Move the tree out of the corpus in one operation. The kernel reports the
	// rename of "docs"; it reports nothing about the files inside it.
	outside := filepath.Join(t.TempDir(), "moved-out")
	if err := os.Rename(filepath.Join(root, "docs"), outside); err != nil {
		t.Fatalf("rename docs out of root: %v", err)
	}

	for _, rel := range subtreeRemovedPaths {
		waitForPaths(t, st, rel, false)
	}
	assertSurvivorsKept(t, st)
}

// TestWatch_TombstonesDescendantsOfRenamedDir covers the in-corpus rename half
// of #678. The content is still in the corpus, but it is no longer at the old
// path, so a citation to the old path is stale and must be retired.
func TestWatch_TombstonesDescendantsOfRenamedDir(t *testing.T) {
	requireWatchIntegration(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := seedSubtreeCorpus(t)
	st := startSubtreeWatcher(t, ctx, root)

	if err := os.Rename(filepath.Join(root, "docs"), filepath.Join(root, "archive")); err != nil {
		t.Fatalf("rename docs within root: %v", err)
	}

	for _, rel := range subtreeRemovedPaths {
		waitForPaths(t, st, rel, false)
	}
	// The same content must reappear under the new path, so the rename costs no
	// coverage.
	waitForPaths(t, st, "archive/a/b/deep.md", true)
	assertSurvivorsKept(t, st)
}

// TestWatch_TombstonesDescendantsOfRemovedDir covers the delete half of #678.
//
// A recursive delete does emit a per-child event on most platforms, so the
// plain files can be retired without the subtree pass. The archive member
// cannot: its rel_path names no file on disk, so no event ever describes it.
func TestWatch_TombstonesDescendantsOfRemovedDir(t *testing.T) {
	requireWatchIntegration(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := seedSubtreeCorpus(t)
	st := startSubtreeWatcher(t, ctx, root)

	if err := os.RemoveAll(filepath.Join(root, "docs")); err != nil {
		t.Fatalf("remove docs tree: %v", err)
	}

	for _, rel := range subtreeRemovedPaths {
		waitForPaths(t, st, rel, false)
	}
	assertSurvivorsKept(t, st)
}
