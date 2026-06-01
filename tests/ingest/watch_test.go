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

// waitForPaths polls the store until want is present (present=true) or absent
// (present=false), failing the test if the condition is not met before timeout.
func waitForPaths(t *testing.T, st *store.SQLiteStore, relPath string, present bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if docPaths(t, st)[relPath] == present {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := docPaths(t, st)
	t.Fatalf("timed out waiting for %q present=%v; got paths: %v", relPath, present, got)
}

// startWatcher runs svc.Watch in the background, returns once the watcher has
// had a moment to register directory watches, and registers cleanup.
func startWatcher(t *testing.T, ctx context.Context, svc interface {
	Watch(context.Context) error
}) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = svc.Watch(ctx)
	}()
	t.Cleanup(func() { <-done })
	// Give the watcher time to walk the tree and register watches before the
	// test mutates files, so the first event is not missed.
	time.Sleep(100 * time.Millisecond)
}

func TestWatch_IndexesNewFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed"), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

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

	startWatcher(t, ctx, svc)

	if err := os.WriteFile(filepath.Join(root, "added.md"), []byte("# new"), 0o600); err != nil {
		t.Fatalf("write added: %v", err)
	}
	waitForPaths(t, st, "added.md", true)
}

func TestWatch_TombstonesDeletedFile(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()

	target := filepath.Join(root, "doomed.txt")
	if err := os.WriteFile(target, []byte("here today"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}

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
	if !docPaths(t, st)["doomed.txt"] {
		t.Fatalf("expected doomed.txt indexed after initial scan")
	}

	startWatcher(t, ctx, svc)

	if err := os.Remove(target); err != nil {
		t.Fatalf("remove target: %v", err)
	}
	waitForPaths(t, st, "doomed.txt", false)
}

func TestWatch_IndexesFileInNewSubdir(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()

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

	startWatcher(t, ctx, svc)

	// Create a subdirectory after the watcher started; the watcher must add a
	// watch for it so files created within are picked up.
	sub := filepath.Join(root, "nested")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Settle so the new-directory watch registers before the child is written.
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(sub, "deep.txt"), []byte("deep"), 0o600); err != nil {
		t.Fatalf("write deep: %v", err)
	}
	waitForPaths(t, st, "nested/deep.txt", true)
}
