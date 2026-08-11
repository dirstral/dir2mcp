package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// waitForNoActiveReps polls until relPath has no searchable representation
// left. activeRepTypes filters both the document tombstone and the
// representation tombstone, so an empty result means nothing of the document
// can reach search or ask.
func waitForNoActiveReps(t *testing.T, st *store.SQLiteStore, relPath string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(activeRepTypes(t, st, relPath)) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q to lose its chunks; still active: %v", relPath, activeRepTypes(t, st, relPath))
}

// TestWatch_EvictsChunksWhenFileGrowsPastSizeCap covers the size-cap half of
// issue #680.
//
// The file still exists, so no delete event describes it. Before the fix the
// watcher dropped the unindexable write and left the old chunks searchable
// until the ten-minute safety rescan.
func TestWatch_EvictsChunksWhenFileGrowsPastSizeCap(t *testing.T) {
	requireWatchIntegration(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()

	target := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(target, []byte("small and indexable"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("an unrelated control"), 0o600); err != nil {
		t.Fatalf("write keep: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.IngestMaxFileMB = 1
	cfg.IngestWatchDebounce = 20 * time.Millisecond
	svc := mustNewIngestService(t, cfg, st)

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("initial Run: %v", err)
	}
	if len(activeRepTypes(t, st, "notes.txt")) == 0 {
		t.Fatalf("expected notes.txt to hold chunks after the initial scan")
	}

	startWatcher(t, ctx, svc)

	// Grow the file past ingest.max_file_mb. It is now outside ingest policy.
	if err := os.WriteFile(target, make([]byte, 2*1024*1024), 0o600); err != nil {
		t.Fatalf("grow notes: %v", err)
	}
	waitForNoActiveReps(t, st, "notes.txt")

	// The path is still on disk, so discovery keeps a durable skipped row for it
	// and the operator can see why it left the index.
	doc := documentByPath(t, st, "notes.txt")
	if doc.Deleted {
		t.Errorf("notes.txt was tombstoned; a path that is still on disk must keep a visible skipped row")
	}
	if doc.Status != "skipped" {
		t.Errorf("notes.txt status = %q, want %q", doc.Status, "skipped")
	}
	if doc.SkipReason != model.SkipReasonSizeCap {
		t.Errorf("notes.txt skip_reason = %q, want %q", doc.SkipReason, model.SkipReasonSizeCap)
	}

	// A still-eligible document must never lose its chunks.
	if len(activeRepTypes(t, st, "keep.txt")) == 0 {
		t.Errorf("keep.txt lost its chunks; the reconciliation is too broad")
	}

	// The transition must be reversible. A file that comes back under the cap is
	// eligible again, so it must become searchable again.
	if err := os.WriteFile(target, []byte("small and indexable once more"), 0o600); err != nil {
		t.Fatalf("shrink notes: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(activeRepTypes(t, st, "notes.txt")) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if len(activeRepTypes(t, st, "notes.txt")) == 0 {
		t.Errorf("notes.txt stayed retired after it came back under the size cap")
	}
}

// TestWatch_EvictsChunksWhenPathBecomesGitignored covers the ignore-list half of
// issue #680.
//
// Only the .gitignore file changes, so no event names the paths whose
// eligibility flipped. Before the fix the excluded content stayed retrievable
// until the ten-minute safety rescan, which is a policy surprise: the operator
// excluded the path and it still answered queries.
func TestWatch_EvictsChunksWhenPathBecomesGitignored(t *testing.T) {
	requireWatchIntegration(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()

	if err := os.Mkdir(filepath.Join(root, "private"), 0o755); err != nil {
		t.Fatalf("mkdir private: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "private", "notes.txt"), []byte("secret plans"), 0o600); err != nil {
		t.Fatalf("write private notes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("an unrelated control"), 0o600); err != nil {
		t.Fatalf("write keep: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.IngestGitignore = true
	cfg.IngestWatchDebounce = 20 * time.Millisecond
	svc := mustNewIngestService(t, cfg, st)

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("initial Run: %v", err)
	}
	if len(activeRepTypes(t, st, "private/notes.txt")) == 0 {
		t.Fatalf("expected private/notes.txt to hold chunks after the initial scan")
	}

	startWatcher(t, ctx, svc)

	// Exclude the directory. The rule file is the only path that changes.
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("private/\n"), 0o600); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}

	waitForPaths(t, st, "private/notes.txt", false)
	waitForNoActiveReps(t, st, "private/notes.txt")

	// A still-eligible document must never lose its chunks.
	if !docPaths(t, st)["keep.txt"] {
		t.Errorf("keep.txt was tombstoned; the reconciliation is too broad")
	}
	if len(activeRepTypes(t, st, "keep.txt")) == 0 {
		t.Errorf("keep.txt lost its chunks; the reconciliation is too broad")
	}
}
