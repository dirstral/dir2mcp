package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestWatch_RecheckPathIndexesFileWithoutEvent drives dir2mcp_recheck_path
// (SPEC §15.13) end to end against a running watcher: Service.RecheckPath
// forwards to the live loop, the call classifies and queues the request, and
// the watcher's worker performs the reconcile well inside the test timeout,
// not at the ten-minute safety rescan. It covers a file, a missing path and a
// directory. A real filesystem also reports events for these writes, so the
// test asserts the outcome and the call contract, not which path got there
// first; the deterministic no-event cases live in the in-package tests.
func TestWatch_RecheckPathIndexesFileWithoutEvent(t *testing.T) {
	requireWatchIntegration(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := testutil.TempDir(t)
	mustWrite(t, filepath.Join(root, "seed.txt"), "seed")

	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
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

	// Before Watch runs, the service has no loop to forward to.
	if _, err := svc.RecheckPath(ctx, "seed.txt", true); err == nil {
		t.Fatal("RecheckPath before Watch must fail")
	}

	startWatcher(t, ctx, svc)

	// A new file: the result names it as a queued file and the document
	// appears.
	mustWrite(t, filepath.Join(root, "added.md"), "# new")
	res := mustRecheck(t, ctx, svc, "added.md")
	if res.RelPath != "added.md" || res.Kind != ingest.RecheckKindFile || !res.Queued || res.Pending < 1 {
		t.Fatalf("unexpected recheck result: %+v", res)
	}
	waitForPaths(t, st, "added.md", true)

	// A missing path is accepted, not an error, and reports kind=missing.
	if err := os.Remove(filepath.Join(root, "seed.txt")); err != nil {
		t.Fatalf("remove seed: %v", err)
	}
	waitForPaths(t, st, "seed.txt", false)
	if res := mustRecheck(t, ctx, svc, "seed.txt"); res.Kind != ingest.RecheckKindMissing {
		t.Fatalf("kind = %q, want missing", res.Kind)
	}

	// A directory recheck indexes a file under it.
	mustWrite(t, filepath.Join(root, "sub", "deep.md"), "deep")
	if res := mustRecheck(t, ctx, svc, "sub"); res.Kind != ingest.RecheckKindDirectory {
		t.Fatalf("kind = %q, want directory", res.Kind)
	}
	waitForPaths(t, st, "sub/deep.md", true)
}

// mustWrite writes content to path, creating parent directories.
func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mustRecheck issues a recursive RecheckPath and fails the test on an error.
func mustRecheck(t *testing.T, ctx context.Context, svc *ingest.Service, rel string) ingest.RecheckResult {
	t.Helper()
	res, err := svc.RecheckPath(ctx, rel, true)
	if err != nil {
		t.Fatalf("RecheckPath(%q): %v", rel, err)
	}
	return res
}
