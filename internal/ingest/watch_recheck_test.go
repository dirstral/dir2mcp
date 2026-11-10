package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// The tests below cover dir2mcp_recheck_path (SPEC §15.13) at the watch-loop
// level. They are internal because they drive the unexported recheckPath and
// processRecheck directly: the call-time half (normalize, classify, enqueue)
// and the worker-side half (reconcile what is there now) run in sequence, with
// no worker goroutine and no fsnotify timing in between.

// recheckHarness is a scanned corpus plus a watch loop built the way Watch
// builds it, minus the fsnotify watcher and the worker goroutine.
type recheckHarness struct {
	ctx  context.Context
	root string
	st   *store.SQLiteStore
	svc  *Service
	loop *fsWatchLoop
}

func newRecheckHarness(t *testing.T, mutate func(cfg *config.Config)) *recheckHarness {
	t.Helper()
	ctx := context.Background()
	root := testutil.TempDir(t)
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.RootDir = root
	cfg.STTProvider = "off"
	if mutate != nil {
		mutate(&cfg)
	}
	svc, err := NewService(cfg, st)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}
	opts := DiscoverOptionsFromConfig(cfg)
	loop := &fsWatchLoop{
		svc:      svc,
		absRoot:  absRoot,
		opts:     opts,
		excluded: opts.ExcludedDirs(),
		rechecks: newRecheckQueue(),
	}
	return &recheckHarness{ctx: ctx, root: root, st: st, svc: svc, loop: loop}
}

// scan runs the initial scan, so the store holds the documents that exist now.
func (h *recheckHarness) scan(t *testing.T) {
	t.Helper()
	if err := h.svc.Run(h.ctx); err != nil {
		t.Fatalf("initial Run: %v", err)
	}
}

func (h *recheckHarness) write(t *testing.T, rel, content string) {
	t.Helper()
	abs := filepath.Join(h.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// recheck issues the call and returns its result.
func (h *recheckHarness) recheck(t *testing.T, rel string, recursive bool) RecheckResult {
	t.Helper()
	res, err := h.loop.recheckPath(h.ctx, rel, recursive)
	if err != nil {
		t.Fatalf("recheckPath(%q): %v", rel, err)
	}
	return res
}

// runOne performs the work the worker performs for one wake-up: pop the oldest
// request and reconcile it. It fails when nothing is pending.
func (h *recheckHarness) runOne(t *testing.T) recheckRequest {
	t.Helper()
	req, ok := h.loop.rechecks.pop()
	if !ok {
		t.Fatal("no recheck request pending")
	}
	h.loop.processRecheck(h.ctx, req)
	return req
}

func expectResult(t *testing.T, got RecheckResult, rel, kind string, queued bool, pending int) {
	t.Helper()
	want := RecheckResult{RelPath: rel, Kind: kind, Queued: queued, Pending: pending}
	if got != want {
		t.Fatalf("recheck result = %+v, want %+v", got, want)
	}
}

// TestRecheckPath_FileIsQueuedAndIndexed is the motivating case: a file that
// appeared with no filesystem event (an NFS write from another host) is indexed
// on request, not at the next safety rescan.
func TestRecheckPath_FileIsQueuedAndIndexed(t *testing.T) {
	h := newRecheckHarness(t, nil)
	h.write(t, "seed.txt", "seed")
	h.scan(t)

	h.write(t, "arrived.md", "# arrived without an event")
	if docPresent(t, h.st, "arrived.md") {
		t.Fatal("precondition: the new file must not be indexed before the recheck")
	}

	expectResult(t, h.recheck(t, "arrived.md", true), "arrived.md", RecheckKindFile, true, 1)
	h.runOne(t)
	if !docPresent(t, h.st, "arrived.md") {
		t.Fatal("the rechecked file was not indexed")
	}
	if n := h.loop.rechecks.size(); n != 0 {
		t.Fatalf("pending after the reconcile = %d, want 0", n)
	}
}

// TestRecheckPath_RepeatIsNotQueuedTwice covers §15.13 step 4: one pending
// request per normalized path. The repeat reports queued=false and the pending
// count does not grow; a second path does grow it.
func TestRecheckPath_RepeatIsNotQueuedTwice(t *testing.T) {
	h := newRecheckHarness(t, nil)
	h.write(t, "a.txt", "a")
	h.write(t, "b.txt", "b")

	expectResult(t, h.recheck(t, "a.txt", true), "a.txt", RecheckKindFile, true, 1)
	expectResult(t, h.recheck(t, "./a.txt", true), "a.txt", RecheckKindFile, false, 1)
	expectResult(t, h.recheck(t, "b.txt", true), "b.txt", RecheckKindFile, true, 2)
	expectResult(t, h.recheck(t, "a.txt", true), "a.txt", RecheckKindFile, false, 2)
}

// TestRecheckPath_RecursiveRepeatUpgradesPendingRequest: a recursive request
// for a path with a pending non-recursive request makes that request
// recursive, and still reports queued=false.
func TestRecheckPath_RecursiveRepeatUpgradesPendingRequest(t *testing.T) {
	h := newRecheckHarness(t, nil)
	h.write(t, "docs/a.txt", "a")

	expectResult(t, h.recheck(t, "docs", false), "docs", RecheckKindDirectory, true, 1)
	expectResult(t, h.recheck(t, "docs", true), "docs", RecheckKindDirectory, false, 1)
	req, ok := h.loop.rechecks.pop()
	if !ok {
		t.Fatal("no request pending")
	}
	if !req.recursive {
		t.Fatal("a recursive repeat did not upgrade the pending non-recursive request")
	}
	// The reverse does not downgrade.
	h.recheck(t, "docs", true)
	h.recheck(t, "docs", false)
	req, _ = h.loop.rechecks.pop()
	if !req.recursive {
		t.Fatal("a non-recursive repeat downgraded a pending recursive request")
	}
}

// TestRecheckPath_DirectoryRecursiveAndNot covers the directory rule: a
// non-recursive request reconciles the direct children only; a recursive one
// reconciles the subtree.
func TestRecheckPath_DirectoryRecursiveAndNot(t *testing.T) {
	h := newRecheckHarness(t, nil)
	h.write(t, "seed.txt", "seed")
	h.scan(t)

	h.write(t, "docs/direct.md", "direct child")
	h.write(t, "docs/nested/deep.md", "nested child")

	expectResult(t, h.recheck(t, "docs", false), "docs", RecheckKindDirectory, true, 1)
	h.runOne(t)
	if !docPresent(t, h.st, "docs/direct.md") {
		t.Fatal("non-recursive recheck did not index the direct child")
	}
	if docPresent(t, h.st, "docs/nested/deep.md") {
		t.Fatal("non-recursive recheck indexed a nested file")
	}

	expectResult(t, h.recheck(t, "docs", true), "docs", RecheckKindDirectory, true, 1)
	h.runOne(t)
	if !docPresent(t, h.st, "docs/nested/deep.md") {
		t.Fatal("recursive recheck did not index the nested file")
	}
}

// TestRecheckPath_RootRecursesWholeCorpus: "." names the root and a recursive
// recheck of it covers every file.
func TestRecheckPath_RootRecursesWholeCorpus(t *testing.T) {
	h := newRecheckHarness(t, nil)
	h.write(t, "seed.txt", "seed")
	h.scan(t)
	h.write(t, "a/b/c.md", "deep")

	expectResult(t, h.recheck(t, ".", true), RecheckRoot, RecheckKindDirectory, true, 1)
	h.runOne(t)
	if !docPresent(t, h.st, "a/b/c.md") {
		t.Fatal("recursive recheck of the root did not index a deep file")
	}
}

// TestRecheckPath_DirectoryTombstonesGoneFiles: a document indexed under the
// directory whose file is gone is tombstoned, while a file still there keeps
// its document. Non-recursive: a gone nested file stays until a recursive
// request covers it.
func TestRecheckPath_DirectoryTombstonesGoneFiles(t *testing.T) {
	h := newRecheckHarness(t, nil)
	h.write(t, "docs/keep.md", "keep")
	h.write(t, "docs/gone.md", "gone")
	h.write(t, "docs/nested/gone-deep.md", "gone deep")
	h.scan(t)
	for _, rel := range []string{"docs/keep.md", "docs/gone.md", "docs/nested/gone-deep.md"} {
		if !docPresent(t, h.st, rel) {
			t.Fatalf("precondition: %s not indexed", rel)
		}
	}
	if err := os.Remove(filepath.Join(h.root, "docs", "gone.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.root, "docs", "nested", "gone-deep.md")); err != nil {
		t.Fatal(err)
	}

	h.recheck(t, "docs", false)
	h.runOne(t)
	if docPresent(t, h.st, "docs/gone.md") {
		t.Fatal("a direct child whose file is gone was not tombstoned")
	}
	if !docPresent(t, h.st, "docs/keep.md") {
		t.Fatal("a direct child whose file exists was tombstoned")
	}
	if !docPresent(t, h.st, "docs/nested/gone-deep.md") {
		t.Fatal("a non-recursive recheck tombstoned a nested document")
	}

	h.recheck(t, "docs", true)
	h.runOne(t)
	if docPresent(t, h.st, "docs/nested/gone-deep.md") {
		t.Fatal("a recursive recheck did not tombstone a nested document whose file is gone")
	}
	if !docPresent(t, h.st, "docs/keep.md") {
		t.Fatal("the recursive recheck tombstoned a document whose file exists")
	}
}

// TestRecheckPath_MissingPathTombstones covers the missing rule: a path that is
// gone at call time and still gone at reconcile time retires the document at
// that path and every document under it. A missing path is not an error.
func TestRecheckPath_MissingPathTombstones(t *testing.T) {
	h := newRecheckHarness(t, nil)
	h.write(t, "solo.txt", "solo")
	h.write(t, "tree/one.md", "one")
	h.write(t, "tree/sub/two.md", "two")
	h.scan(t)

	if err := os.Remove(filepath.Join(h.root, "solo.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(h.root, "tree")); err != nil {
		t.Fatal(err)
	}

	expectResult(t, h.recheck(t, "solo.txt", true), "solo.txt", RecheckKindMissing, true, 1)
	expectResult(t, h.recheck(t, "tree", true), "tree", RecheckKindMissing, true, 2)
	h.runOne(t)
	h.runOne(t)
	for _, rel := range []string{"solo.txt", "tree/one.md", "tree/sub/two.md"} {
		if docPresent(t, h.st, rel) {
			t.Fatalf("%s still indexed after a recheck of a missing path", rel)
		}
	}
}

// TestRecheckPath_MissingAtCallFileAtReconcile: the call-time kind does not
// bind the worker. A file that appears between the call and the reconcile is
// indexed, not tombstoned.
func TestRecheckPath_MissingAtCallFileAtReconcile(t *testing.T) {
	h := newRecheckHarness(t, nil)
	h.write(t, "seed.txt", "seed")
	h.scan(t)

	expectResult(t, h.recheck(t, "late.md", true), "late.md", RecheckKindMissing, true, 1)
	h.write(t, "late.md", "appeared after the call")
	h.runOne(t)
	if !docPresent(t, h.st, "late.md") {
		t.Fatal("a file that appeared before the reconcile was not indexed")
	}
}

// TestRecheckPath_ExcludedPathIsRetired: an excluded path is accepted, and a
// document indexed at it is retired as the watcher retires it on a write. Here
// the file becomes gitignored after the scan, so the recheck tombstones it.
func TestRecheckPath_ExcludedPathIsRetired(t *testing.T) {
	h := newRecheckHarness(t, func(cfg *config.Config) { cfg.IngestGitignore = true })
	h.write(t, "notes.md", "notes")
	h.scan(t)
	if !docPresent(t, h.st, "notes.md") {
		t.Fatal("precondition: notes.md not indexed")
	}

	h.write(t, ".gitignore", "notes.md\n")
	expectResult(t, h.recheck(t, "notes.md", true), "notes.md", RecheckKindFile, true, 1)
	h.runOne(t)
	if docPresent(t, h.st, "notes.md") {
		t.Fatal("a newly gitignored document was not retired by the recheck")
	}
}

// TestRecheckPath_RejectsPathsOutsideRoot covers §15.13 step 1: traversal and
// absolute paths are PATH_OUTSIDE_ROOT; an empty path is a missing field.
func TestRecheckPath_RejectsPathsOutsideRoot(t *testing.T) {
	h := newRecheckHarness(t, nil)
	for _, rel := range []string{"..", "../x.txt", "a/../../x.txt", "/etc/passwd"} {
		if _, err := h.loop.recheckPath(h.ctx, rel, true); !errors.Is(err, model.ErrPathOutsideRoot) {
			t.Errorf("recheckPath(%q) error = %v, want ErrPathOutsideRoot", rel, err)
		}
	}
	for _, rel := range []string{"", "   "} {
		if _, err := h.loop.recheckPath(h.ctx, rel, true); !errors.Is(err, ErrRecheckPathRequired) {
			t.Errorf("recheckPath(%q) error = %v, want ErrRecheckPathRequired", rel, err)
		}
	}
	if n := h.loop.rechecks.size(); n != 0 {
		t.Fatalf("a rejected request was queued: pending = %d", n)
	}
}

// TestRecheckPath_SymlinkOutsideRootIsRejected: the §17 symlink rule applies to
// the resolved path, for an existing link and for a child of a linked directory
// that does not exist yet.
func TestRecheckPath_SymlinkOutsideRootIsRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	h := newRecheckHarness(t, nil)
	outside := testutil.TempDir(t)
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(h.root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(h.root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"link.txt", "linkdir", "linkdir/secret.txt", "linkdir/not-yet.txt"} {
		if _, err := h.loop.recheckPath(h.ctx, rel, true); !errors.Is(err, model.ErrPathOutsideRoot) {
			t.Errorf("recheckPath(%q) error = %v, want ErrPathOutsideRoot", rel, err)
		}
	}
}

// TestRecheckPath_LinkedDirectoryRepointedOutsideRootIsNotWalked: the worker
// judges the path as it is at reconcile time. A directory that became a
// symlink to a tree outside the corpus after the call is not walked, under
// either symlink policy, so no outside file enters the index.
func TestRecheckPath_LinkedDirectoryRepointedOutsideRootIsNotWalked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	for _, follow := range []bool{false, true} {
		h := newRecheckHarness(t, func(cfg *config.Config) { cfg.IngestFollowSymlinks = follow })
		h.write(t, "seed.txt", "seed")
		h.write(t, "docs/inside.md", "inside")
		h.scan(t)
		outside := testutil.TempDir(t)
		if err := os.WriteFile(filepath.Join(outside, "leak.md"), []byte("outside"), 0o600); err != nil {
			t.Fatal(err)
		}

		expectResult(t, h.recheck(t, "docs", true), "docs", RecheckKindDirectory, true, 1)
		// Between the call and the reconcile the directory is replaced by a
		// link to the outside tree.
		if err := os.RemoveAll(filepath.Join(h.root, "docs")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(h.root, "docs")); err != nil {
			t.Fatal(err)
		}
		h.runOne(t)
		if docPresent(t, h.st, "docs/leak.md") {
			t.Fatalf("follow=%v: a file outside the root was indexed through a repointed link", follow)
		}
	}
}

// TestRecheckPath_LinkedDirectoryInsideRootIsWalkedWhenFollowed: with
// following on, a link to a directory inside the corpus is a directory for the
// recheck, as it is for discovery.
func TestRecheckPath_LinkedDirectoryInsideRootIsWalkedWhenFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	h := newRecheckHarness(t, func(cfg *config.Config) { cfg.IngestFollowSymlinks = true })
	h.write(t, "seed.txt", "seed")
	h.scan(t)
	h.write(t, "real/new.md", "new")
	if err := os.Symlink(filepath.Join(h.root, "real"), filepath.Join(h.root, "alias")); err != nil {
		t.Fatal(err)
	}
	expectResult(t, h.recheck(t, "alias", true), "alias", RecheckKindDirectory, true, 1)
	h.runOne(t)
	if !docPresent(t, h.st, "alias/new.md") && !docPresent(t, h.st, "real/new.md") {
		t.Fatal("a recheck of a followed in-root directory link indexed nothing under it")
	}
}

// TestRecheckPath_NormalizesRelPath: the open_file grammar, with "." for the
// root and a cleaned form in the response.
func TestRecheckPath_NormalizesRelPath(t *testing.T) {
	h := newRecheckHarness(t, nil)
	h.write(t, "a/b.txt", "b")
	expectResult(t, h.recheck(t, "./a//b.txt", true), "a/b.txt", RecheckKindFile, true, 1)
	expectResult(t, h.recheck(t, "a/./b.txt", true), "a/b.txt", RecheckKindFile, false, 1)
	expectResult(t, h.recheck(t, "./", true), RecheckRoot, RecheckKindDirectory, true, 2)
	expectResult(t, h.recheck(t, "a/", true), "a", RecheckKindDirectory, true, 3)
}

// TestServiceRecheckPath_WithoutWatcher: Service.RecheckPath reports
// ErrWatchNotRunning until Watch publishes a loop, and again after it withdraws
// the loop.
func TestServiceRecheckPath_WithoutWatcher(t *testing.T) {
	h := newRecheckHarness(t, nil)
	h.write(t, "a.txt", "a")
	if _, err := h.svc.RecheckPath(h.ctx, "a.txt", true); !errors.Is(err, ErrWatchNotRunning) {
		t.Fatalf("RecheckPath before Watch: error = %v, want ErrWatchNotRunning", err)
	}
	h.svc.setRecheckLoop(h.loop)
	res, err := h.svc.RecheckPath(h.ctx, "a.txt", true)
	if err != nil {
		t.Fatalf("RecheckPath with a loop: %v", err)
	}
	expectResult(t, res, "a.txt", RecheckKindFile, true, 1)
	h.svc.setRecheckLoop(nil)
	if _, err := h.svc.RecheckPath(h.ctx, "a.txt", true); !errors.Is(err, ErrWatchNotRunning) {
		t.Fatalf("RecheckPath after the loop left: error = %v, want ErrWatchNotRunning", err)
	}
}

// TestRecheckQueue_WakesWorkerOncePerBurst: the signal channel coalesces, and
// pop returns requests in call order.
func TestRecheckQueue_WakesWorkerOncePerBurst(t *testing.T) {
	q := newRecheckQueue()
	q.add("b.txt", true)
	q.add("a.txt", false)
	q.add("b.txt", false)
	select {
	case <-q.signal:
	default:
		t.Fatal("no wake-up after the first add")
	}
	select {
	case <-q.signal:
		t.Fatal("a burst of adds produced more than one wake-up")
	default:
	}
	first, _ := q.pop()
	second, _ := q.pop()
	if first.rel != "b.txt" || second.rel != "a.txt" {
		t.Fatalf("pop order = %q, %q; want b.txt, a.txt", first.rel, second.rel)
	}
	if _, ok := q.pop(); ok {
		t.Fatal("pop on an empty queue returned a request")
	}
}
