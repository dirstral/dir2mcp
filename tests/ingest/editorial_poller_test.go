package tests

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dirstral/dir2mcp/internal/editorial"
)

// The staged-import poller of a running daemon (SPEC §7.11.1): it applies an
// import with no restart, only after the first scan, never while a scan or a
// watch job writes documents, and it also runs a rebuild a reindex left
// pending.

func (e *editorialEnv) firstScan() {
	e.t.Helper()
	if err := e.svc.Run(e.ctx); err != nil {
		e.t.Fatalf("Run: %v", err)
	}
}

func (e *editorialEnv) poll() bool {
	e.t.Helper()
	ran, err := e.svc.PollEditorialOnce(e.ctx)
	if err != nil {
		e.t.Fatalf("PollEditorialOnce: %v", err)
	}
	return ran
}

func stagedCount(t *testing.T, stateDir string) int {
	t.Helper()
	staged, err := editorial.ListStaged(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return len(staged)
}

func TestEditorialPoller_WaitsForFirstScanAndForWriters(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.stage(edSource, editorial.KeyStem, `{"stem": "nothing-here", "header": "Шапка"}`)
	if e.poll() {
		t.Fatal("the poller applied before the first scan finished")
	}
	e.firstScan() // applies the first import at its end
	e.stage(edSource, editorial.KeyStem, `{"stem": "later", "header": "Шапка"}`)

	release := e.svc.HoldIndexWriteForTest()
	if e.poll() {
		release()
		t.Fatal("the poller applied while a scan held the index write lock")
	}
	if stagedCount(t, e.stateDir) != 1 {
		release()
		t.Fatal("the staged import changed while a scan held the lock")
	}
	release()

	if !e.poll() {
		t.Fatal("the poller did not apply once the scan released the lock")
	}
	if stagedCount(t, e.stateDir) != 0 {
		t.Fatal("the applied import is still staged")
	}
	if e.poll() {
		t.Fatal("the poller ran with no pending work")
	}
}

func TestEditorialPoller_RunsAPendingRebuild(t *testing.T) {
	e := newEditorialEnv(t, nil)
	e.firstScan()
	if err := editorial.MarkRebuildPending(e.stateDir); err != nil {
		t.Fatal(err)
	}
	if !e.svc.HasPendingEditorialWork() {
		t.Fatal("a pending rebuild is not pending work")
	}
	if !e.poll() {
		t.Fatal("the poller skipped a pending rebuild with no staged file")
	}
	if editorial.RebuildPending(e.stateDir) {
		t.Fatal("the poller left the rebuild marker after a successful pass")
	}
}

// A read error leaves the staged import in place for the next pass; only
// content that does not decode is set aside.
func TestEditorialApply_ReadErrorKeepsTheStagedFile(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file mode 000 does not block reads here")
	}
	e := newEditorialEnv(t, nil)
	e.stage(edSource, editorial.KeyStem, `{"stem": "a", "header": "Шапка"}`)
	staged, _ := editorial.ListStaged(e.stateDir)
	if err := os.Chmod(staged[0], 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(staged[0], 0o600) })
	if err := e.svc.ApplyEditorialImports(e.ctx); err == nil {
		t.Fatal("an unreadable staged import gave no error")
	}
	if _, err := os.Lstat(staged[0]); err != nil {
		t.Fatalf("the unreadable staged import was moved: %v", err)
	}
	if _, err := os.Lstat(staged[0] + ".invalid"); err == nil {
		t.Fatal("an unreadable staged import was set aside as invalid")
	}
}

func TestEditorialApply_GarbageStagedFileIsSetAside(t *testing.T) {
	e := newEditorialEnv(t, nil)
	dir := filepath.Join(editorial.ImportsDir(e.stateDir), "staged")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "00000000000000000001.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.stage(edSource, editorial.KeyStem, `{"stem": "missing", "header": "Шапка"}`)
	e.apply()
	if _, err := os.Lstat(bad + ".invalid"); err != nil {
		t.Fatalf("the garbage file was not set aside: %v", err)
	}
	if stagedCount(t, e.stateDir) != 0 {
		t.Fatal("the valid import behind the garbage file was not applied")
	}
}
