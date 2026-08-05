package tests

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/store"
)

// requireSymlinkWatch skips on platforms where this scenario is not meaningful.
// Unlike the older watcher tests these are NOT gated behind RUN_INTEGRATION_TESTS:
// this is a root-isolation regression (SPEC §1/§7.1), so it must run on every
// `make check`. The positive controls (a plain file and an in-root symlink must
// still be indexed) fail loudly if the watcher never fires, so a missed event
// cannot masquerade as a pass.
func requireSymlinkWatch(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX symlink watch test on Windows")
	}
}

// watchSymlinkFixture spins up an indexed corpus with a live watcher and returns
// the store plus the captured ingest log.
func watchSymlinkFixture(t *testing.T, ctx context.Context, root string, followSymlinks bool) (*store.SQLiteStore, *syncBuffer) {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.RootDir = root
	cfg.IngestFollowSymlinks = followSymlinks
	cfg.IngestWatchDebounce = 20 * time.Millisecond
	cfg.STTProvider = "off"
	svc := mustNewIngestService(t, cfg, st)

	logs := &syncBuffer{}
	svc.SetLogger(log.New(logs, "", 0))

	if err := svc.Run(ctx); err != nil {
		t.Fatalf("initial Run: %v", err)
	}
	startWatcher(t, ctx, svc)
	return st, logs
}

// TestWatch_RefusesSymlinkEscapingRoot_717 covers issue #717: with
// ingest.follow_symlinks on, a symlink CREATED AFTER the watcher started that
// points at a regular file outside the corpus root must not be indexed. The
// initial discovery walk already refuses it (corpusfs enforces resolved-root
// containment), so before the fix the same link was accepted or rejected purely
// on when it appeared, which is a root-isolation hole (SPEC §1/§7.1).
//
// The test also pins the two things a lazy fix would break: an in-root symlink
// must still be followed and indexed, and a plain regular file must still be
// indexed.
func TestWatch_RefusesSymlinkEscapingRoot_717(t *testing.T) {
	requireSymlinkWatch(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("out-of-root secret material"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	inRootTarget := filepath.Join(root, "real.txt")
	if err := os.WriteFile(inRootTarget, []byte("in-root content"), 0o600); err != nil {
		t.Fatalf("write in-root target: %v", err)
	}

	st, logs := watchSymlinkFixture(t, ctx, root, true)

	// One mutation at a time: a burst of creations can be coalesced by the
	// platform's event backend (macOS/kqueue re-reads the directory and reports
	// the diff), which would leave it ambiguous whether the watcher declined the
	// link or never saw it.
	if err := os.Symlink(secret, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatalf("symlink leak: %v", err)
	}
	// The refusal must be observable, otherwise it is indistinguishable from
	// "the watcher missed my file". Waiting for it also proves the watcher DID
	// see the link, so the "not indexed" assertion below is not vacuous.
	refusalLogged := waitForRefusal(logs, "leak.txt")

	if err := os.Symlink(inRootTarget, filepath.Join(root, "link_ok.txt")); err != nil {
		t.Fatalf("symlink link_ok: %v", err)
	}
	waitForPaths(t, st, "link_ok.txt", true)

	if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("plain"), 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	waitForPaths(t, st, "plain.txt", true)

	if docPaths(t, st)["leak.txt"] {
		t.Errorf("watcher indexed leak.txt, a symlink to a file outside the corpus root (issue #717)")
	}
	if !refusalLogged {
		t.Errorf("no log line refused leak.txt; a silent drop is indistinguishable from a missed event. Log:\n%s", logs.String())
	}
}

// waitForRefusal polls the captured ingest log for up to five seconds for a
// single line that both names the path and states it was refused, so an
// unrelated line that merely mentions the path does not satisfy the
// observability assertion. It reports whether such a line appeared.
func waitForRefusal(logs *syncBuffer, path string) bool {
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, path) && strings.Contains(line, "refusing") {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestWatch_RefusesRetargetedSymlink_717 covers the TOCTOU half of #717: a link
// that is in-root when first indexed and is later repointed outside the root
// must never have the out-of-root bytes read into its document. Containment is
// re-evaluated when the event is handled, not cached from the first sighting.
func TestWatch_RefusesRetargetedSymlink_717(t *testing.T) {
	requireSymlinkWatch(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("out-of-root secret material"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	inRootTarget := filepath.Join(root, "real.txt")
	if err := os.WriteFile(inRootTarget, []byte("in-root content"), 0o600); err != nil {
		t.Fatalf("write in-root target: %v", err)
	}
	link := filepath.Join(root, "movable.txt")
	if err := os.Symlink(inRootTarget, link); err != nil {
		t.Fatalf("symlink movable: %v", err)
	}

	st, _ := watchSymlinkFixture(t, ctx, root, true)
	// Indexed by the initial scan through its in-root target.
	before := documentByPath(t, st, "movable.txt")
	if before.ContentHash == "" {
		t.Fatalf("expected a content hash for the initially in-root symlink")
	}

	// Repoint the link at the out-of-root file atomically (what `ln -sfn` does:
	// create a temp link, rename it over the old one). A remove+recreate would
	// let a delete job tombstone the document and mask the leak; the rename
	// leaves the path continuously present, so the only way the document can
	// change is the watcher reading through the repointed link.
	tmpLink := filepath.Join(root, "movable.txt.tmp")
	if err := os.Symlink(secret, tmpLink); err != nil {
		t.Fatalf("stage repointed link: %v", err)
	}
	if err := os.Rename(tmpLink, link); err != nil {
		t.Fatalf("repoint link: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "barrier.txt"), []byte("barrier"), 0o600); err != nil {
		t.Fatalf("write barrier: %v", err)
	}
	waitForPaths(t, st, "barrier.txt", true)
	// Extra settle time: the barrier only proves the watcher drained an event
	// armed after the repoint, not that the repoint's own job already ran.
	time.Sleep(300 * time.Millisecond)

	// Either outcome is acceptable (the document may have been tombstoned by the
	// remove), except taking on the out-of-root content.
	if !docPaths(t, st)["movable.txt"] {
		return
	}
	if got := documentByPath(t, st, "movable.txt"); got.ContentHash != before.ContentHash {
		t.Errorf("document content changed after the symlink was repointed outside the root: %q -> %q (issue #717)",
			before.ContentHash, got.ContentHash)
	}
}

// TestWatch_SymlinkPolicyUnchangedWhenNotFollowing_717 pins that the fix is
// scoped to follow_symlinks=true: with following off the watcher still refuses
// every symlink (in-root ones included) and still indexes regular files.
func TestWatch_SymlinkPolicyUnchangedWhenNotFollowing_717(t *testing.T) {
	requireSymlinkWatch(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("out-of-root secret material"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	inRootTarget := filepath.Join(root, "real.txt")
	if err := os.WriteFile(inRootTarget, []byte("in-root content"), 0o600); err != nil {
		t.Fatalf("write in-root target: %v", err)
	}

	st, _ := watchSymlinkFixture(t, ctx, root, false)

	if err := os.Symlink(secret, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatalf("symlink leak: %v", err)
	}
	if err := os.Symlink(inRootTarget, filepath.Join(root, "link_ok.txt")); err != nil {
		t.Fatalf("symlink link_ok: %v", err)
	}
	// Settle past the debounce so the links' events have been drained before the
	// barrier file is written, rather than riding on the same coalesced batch.
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("plain"), 0o600); err != nil {
		t.Fatalf("write plain: %v", err)
	}
	waitForPaths(t, st, "plain.txt", true)

	paths := docPaths(t, st)
	if paths["leak.txt"] {
		t.Errorf("follow_symlinks=false: watcher indexed an out-of-root symlink")
	}
	if paths["link_ok.txt"] {
		t.Errorf("follow_symlinks=false: watcher indexed an in-root symlink; symlink policy changed")
	}
}
