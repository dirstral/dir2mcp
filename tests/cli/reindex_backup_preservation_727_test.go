package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/index/diskindex"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Issue #727: a reindex that CRASHED (SIGKILL / power loss) never runs its
// in-process rollback, so it leaves the last-known-good generation on disk:
//
//   - <index file>.reindex-old   — the previous, complete index (atomic rename)
//   - _reindex_hash_backup       — the pre-clear documents.content_hash snapshot
//
// while the live index file is the crashed run's partial output and the live
// content hashes are cleared. The next `reindex` used to treat those recovery
// copies as its own leftovers: it deleted the good `.reindex-old` file and
// rotated the partial live file into its place, and re-snapshotted the cleared
// hashes over the good snapshot. The first retry after a crash therefore
// destroyed the only good generation.
//
// These tests simulate a crash the way a crash actually happens — by leaving
// the interrupted run's artifacts in place, never by calling the rollback path,
// which is precisely what does not run on a crash.

const (
	crashGoodIndex    = "LAST-KNOWN-GOOD-INDEX"
	crashPartialIndex = "PARTIAL-REBUILD-FROM-CRASHED-RUN"
	crashGoodHash     = "HASH-BEFORE-CRASHED-REINDEX"
)

// seedCrashedReindexState lays down the on-disk and in-store state a crashed
// reindex leaves behind: the good index moved aside to *.reindex-old with the
// crashed run's partial output live, plus the content-hash snapshot table with
// the live hashes cleared. Returns the live index path.
func seedCrashedReindexState(t *testing.T, stateDir, relPath string) string {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	live := filepath.Join(stateDir, index.TextIndexFileName)
	// The crashed run had already renamed the good index aside...
	if err := os.WriteFile(live+".reindex-old", []byte(crashGoodIndex), 0o600); err != nil {
		t.Fatalf("seed backup index: %v", err)
	}
	// ...and had written part of the new one before it was killed.
	if err := os.WriteFile(live, []byte(crashPartialIndex), 0o600); err != nil {
		t.Fatalf("seed partial index: %v", err)
	}
	seedCrashedHashState(t, stateDir, relPath)
	return live
}

// seedCrashedHashState reproduces the store side of a crashed reindex: a
// document whose good hash is captured in _reindex_hash_backup while the live
// documents.content_hash has been cleared. It drives the real store calls the
// staging path makes (BackupContentHashes then ClearDocumentContentHashes) and
// then simply stops, standing in for the process being killed.
func seedCrashedHashState(t *testing.T, stateDir, relPath string) {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("crash-seed Init: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:     relPath,
		DocType:     "md",
		ContentHash: crashGoodHash,
		Status:      "ready",
	}); err != nil {
		t.Fatalf("crash-seed UpsertDocument: %v", err)
	}
	if err := st.BackupContentHashes(ctx); err != nil {
		t.Fatalf("crash-seed BackupContentHashes: %v", err)
	}
	if err := st.ClearDocumentContentHashes(ctx); err != nil {
		t.Fatalf("crash-seed ClearDocumentContentHashes: %v", err)
	}
	// No rollback, no commit: the process "dies" here.
	if err := st.Close(); err != nil {
		t.Fatalf("crash-seed Close: %v", err)
	}
}

// runReindexInDir runs `dir2mcp reindex` with the given ingestor hook from tmp
// as the working directory and returns the exit code and stderr.
func runReindexInDir(t *testing.T, tmp string, newIngestor func(config.Config, model.Store) (model.Ingestor, error)) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{NewIngestor: newIngestor})
	var code int
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		code = app.RunWithContext(ctx, []string{"reindex"})
	})
	return code, stderr.String()
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestReindex_AfterCrash_RetryPreservesLastKnownGood is the core #727
// regression: a retry after a crash must not rotate the crashed run's partial
// output into the recovery slot. When that retry itself fails, what is left
// live must be the ORIGINAL good index and the ORIGINAL content hashes, not the
// partial rebuild the crash left behind.
func TestReindex_AfterCrash_RetryPreservesLastKnownGood(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	const relPath = "docs/a.md"
	live := seedCrashedReindexState(t, stateDir, relPath)

	code, stderr := runReindexInDir(t, tmp, func(config.Config, model.Store) (model.Ingestor, error) {
		return reindexFailingIngestor{}, nil
	})
	if code == 0 {
		t.Fatalf("reindex should fail when the ingestor errors; got exit 0 stderr=%q", stderr)
	}

	if got := readFileString(t, live); got != crashGoodIndex {
		t.Errorf("live index after a failed retry must be the last-known-good generation left by the crashed run; want %q got %q", crashGoodIndex, got)
	}
	if got := readDocumentHash(t, stateDir, relPath); got != crashGoodHash {
		t.Errorf("content_hash after a failed retry must be the pre-crash snapshot; want %q got %q", crashGoodHash, got)
	}
	if _, err := os.Stat(live + ".reindex-old"); !os.IsNotExist(err) {
		t.Errorf("no backup sidecar should linger after the retry rolled back; stat err=%v", err)
	}
}

// TestReindex_AfterCrash_RepeatedCrashesDoNotRotatePartialState covers the
// "repeated retries" case from the issue: however many times the rebuild
// fails, the recovery slot must still hold the original good generation and
// never a partial one.
func TestReindex_AfterCrash_RepeatedCrashesDoNotRotatePartialState(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	const relPath = "docs/a.md"
	live := seedCrashedReindexState(t, stateDir, relPath)

	for attempt := 1; attempt <= 3; attempt++ {
		code, stderr := runReindexInDir(t, tmp, func(config.Config, model.Store) (model.Ingestor, error) {
			return reindexFailingIngestor{}, nil
		})
		if code == 0 {
			t.Fatalf("attempt %d: reindex should fail when the ingestor errors; stderr=%q", attempt, stderr)
		}
		if got := readFileString(t, live); got != crashGoodIndex {
			t.Fatalf("attempt %d: live index must still be the last-known-good generation; want %q got %q", attempt, crashGoodIndex, got)
		}
		if got := readDocumentHash(t, stateDir, relPath); got != crashGoodHash {
			t.Fatalf("attempt %d: content_hash must still be the pre-crash snapshot; want %q got %q", attempt, crashGoodHash, got)
		}
	}
}

// TestReindex_AfterCrash_RecoversEveryLeftoverGeneration covers the issue's
// "recovery covers HNSW, disk segment + identity sidecar, and all stale/legacy
// index files" requirement. The leftovers here include names that
// index.StaleIndexFiles does NOT return for the configured (default, memory)
// backend — the disk backend's segment and its identity sidecar — because a
// crash under one backend can be retried under another, and those files are
// just as much part of the last-known-good set.
func TestReindex_AfterCrash_RecoversEveryLeftoverGeneration(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	seg := diskindex.SegmentFileName(index.KindText)
	leftovers := []string{
		index.TextIndexFileName,
		index.CodeIndexFileName,
		index.LegacyIndexFileNames[0],
		seg,
		seg + diskindex.IdentitySidecarSuffix,
	}
	for _, name := range leftovers {
		path := filepath.Join(stateDir, name)
		if err := os.WriteFile(path+".reindex-old", []byte(crashGoodIndex+"/"+name), 0o600); err != nil {
			t.Fatalf("seed backup %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(crashPartialIndex), 0o600); err != nil {
			t.Fatalf("seed partial %s: %v", name, err)
		}
	}

	code, stderr := runReindexInDir(t, tmp, func(config.Config, model.Store) (model.Ingestor, error) {
		return reindexFailingIngestor{}, nil
	})
	if code == 0 {
		t.Fatalf("reindex should fail when the ingestor errors; got exit 0 stderr=%q", stderr)
	}

	for _, name := range leftovers {
		path := filepath.Join(stateDir, name)
		want := crashGoodIndex + "/" + name
		if got := readFileString(t, path); got != want {
			t.Errorf("%s must be restored from the crashed run's backup; want %q got %q", name, want, got)
		}
		// Recovered files stay owner-only (#726): the state tree is hardened
		// before recovery and a rename preserves the mode.
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("recovered %s must stay owner-only; got mode %o", name, perm)
		}
	}
}

// TestReindex_AfterCrash_SuccessfulRetryClearsRecoveredBackups pins the commit
// side: once a retry rebuilds durably, the recovered generation is no longer
// needed and must not be left behind as a permanent second copy of the index.
func TestReindex_AfterCrash_SuccessfulRetryClearsRecoveredBackups(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	const relPath = "docs/a.md"
	const rebuilt = "HASH-AFTER-REBUILD"
	live := seedCrashedReindexState(t, stateDir, relPath)

	code, stderr := runReindexInDir(t, tmp, func(_ config.Config, st model.Store) (model.Ingestor, error) {
		return reindexRebuildingIngestor{st: st, relPath: relPath, newHash: rebuilt}, nil
	})
	if code != 0 {
		t.Fatalf("reindex should succeed; got exit %d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(live + ".reindex-old"); !os.IsNotExist(err) {
		t.Errorf("committed reindex must not leave a recovery copy behind; stat err=%v", err)
	}
	if got := readDocumentHash(t, stateDir, relPath); got != rebuilt {
		t.Errorf("committed reindex must keep the rebuild's hash; want %q got %q", rebuilt, got)
	}
	// The recovered generation was adopted as this run's rollback target and
	// then committed away; the live file is whatever the rebuild left (here the
	// stub writes none), never the crashed run's partial output.
	if data, err := os.ReadFile(live); err == nil && string(data) == crashPartialIndex {
		t.Errorf("the crashed run's partial index must not survive a committed retry")
	}
}
