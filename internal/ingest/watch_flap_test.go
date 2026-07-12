package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
)

// docPresent reports whether a live (non-tombstoned) document with relPath
// exists in the store.
func docPresent(t *testing.T, st *store.SQLiteStore, relPath string) bool {
	t.Helper()
	docs, _, err := st.ListFiles(context.Background(), "", "", 1000, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	for _, d := range docs {
		if d.RelPath == relPath {
			return true
		}
	}
	return false
}

// TestWatchProcess_DeleteRecreateFlapReindexes covers issue #409 item 4: a
// delete job whose path has already been recreated (an atomic-save flap:
// write-temp + rename-over emits a Remove that fires the debounced delete after
// the file is back) must NOT tombstone the live document. process() re-stats and,
// finding the path present and indexable, reindexes instead of deleting — so the
// document is never momentarily absent. A genuine delete (path truly gone) must
// still tombstone.
//
// This is an internal test because it drives the unexported fsWatchLoop.process
// directly, which lets it exercise the delete-vs-reindex decision deterministically
// rather than racing real fsnotify event timing.
func TestWatchProcess_DeleteRecreateFlapReindexes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	target := filepath.Join(root, "note.txt")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.STTProvider = "off" // no external providers needed for a plain text file
	svc, err := NewService(cfg, st)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("initial Run: %v", err)
	}
	if !docPresent(t, st, "note.txt") {
		t.Fatalf("expected note.txt indexed after initial scan")
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}
	w := &fsWatchLoop{
		svc:     svc,
		absRoot: absRoot,
		opts:    DiscoverOptionsFromConfig(cfg),
	}

	// Flap: the file was recreated (here: rewritten) before the debounced delete
	// job runs. A delete job on a path that now exists and is indexable must be
	// treated as a reindex, leaving the document present.
	if err := os.WriteFile(target, []byte("updated content after recreate"), 0o600); err != nil {
		t.Fatalf("recreate target: %v", err)
	}
	w.process(ctx, watchJob{absPath: target, deleted: true})
	if !docPresent(t, st, "note.txt") {
		t.Fatalf("delete→recreate flap tombstoned a live document (issue #409 item 4)")
	}

	// Genuine delete: the path is truly gone, so the delete job must tombstone.
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove target: %v", err)
	}
	w.process(ctx, watchJob{absPath: target, deleted: true})
	if docPresent(t, st, "note.txt") {
		t.Fatalf("genuine delete failed to tombstone the document")
	}
}
