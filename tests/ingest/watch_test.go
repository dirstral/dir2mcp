package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
)

// requireWatchIntegration gates the watcher tests behind RUN_INTEGRATION_TESTS:
// they drive the real filesystem + store end-to-end and depend on OS event
// timing, so they only run when integration tests are explicitly enabled.
func requireWatchIntegration(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-oriented filesystem watch test on Windows")
	}
	if os.Getenv("RUN_INTEGRATION_TESTS") == "" {
		t.Skip("set RUN_INTEGRATION_TESTS=1 to run integration tests")
	}
}

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
	var watchErr error
	go func() {
		defer close(done)
		watchErr = svc.Watch(ctx)
	}()
	t.Cleanup(func() {
		<-done
		// A genuine setup failure must fail the test rather than surfacing as a
		// later polling timeout; ignore the expected context cancellation.
		if watchErr != nil && !errors.Is(watchErr, context.Canceled) && !errors.Is(watchErr, context.DeadlineExceeded) {
			t.Errorf("Watch returned error: %v", watchErr)
		}
	})
	// Give the watcher time to walk the tree and register watches before the
	// test mutates files, so the first event is not missed.
	time.Sleep(100 * time.Millisecond)
}

func TestWatch_IndexesNewFile(t *testing.T) {
	requireWatchIntegration(t)
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
	requireWatchIntegration(t)
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
	requireWatchIntegration(t)
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

// TestWatch_IndexesFilesInMovedInTree verifies that a directory tree created
// with nested children already present (e.g. mkdir -p a/b + a file) is picked
// up: the watcher must register the whole new subtree and index existing files.
func TestWatch_IndexesFilesInMovedInTree(t *testing.T) {
	requireWatchIntegration(t)
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

	// Build a nested tree with a file already inside, then create its top dir
	// under the watched root in one shot.
	staging := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(staging, "a", "b"), 0o755); err != nil {
		t.Fatalf("mkdirall: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "a", "b", "buried.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("write buried: %v", err)
	}
	if err := os.Rename(staging, filepath.Join(root, "tree")); err != nil {
		t.Fatalf("rename tree into root: %v", err)
	}
	waitForPaths(t, st, "tree/a/b/buried.txt", true)
}

// TestWatch_RespectsGitignoreAndSizeCap verifies the watcher applies the same
// discovery filters as the initial scan: a freshly created file matching a
// .gitignore rule, or one above the size cap, must not be indexed.
func TestWatch_RespectsGitignoreAndSizeCap(t *testing.T) {
	requireWatchIntegration(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()

	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.txt\n"), 0o600); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}

	cfg := config.Default()
	cfg.RootDir = root
	cfg.IngestGitignore = true
	cfg.IngestMaxFileMB = 1
	cfg.IngestWatchDebounce = 20 * time.Millisecond
	svc := mustNewIngestService(t, cfg, st)

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("initial Run: %v", err)
	}

	startWatcher(t, ctx, svc)

	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("nope"), 0o600); err != nil {
		t.Fatalf("write ignored: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), make([]byte, 2*1024*1024), 0o600); err != nil {
		t.Fatalf("write big: %v", err)
	}
	// A control file that should be indexed, used to confirm the watcher is
	// alive and to bound the wait before asserting the others are absent.
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("yes"), 0o600); err != nil {
		t.Fatalf("write ok: %v", err)
	}
	waitForPaths(t, st, "ok.txt", true)

	paths := docPaths(t, st)
	if paths["ignored.txt"] {
		t.Errorf("gitignored file was indexed by the watcher")
	}
	if paths["big.txt"] {
		t.Errorf("over-size file was indexed by the watcher")
	}
}
