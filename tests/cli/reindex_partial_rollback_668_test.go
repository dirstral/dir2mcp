package tests

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Partial reindex rollback: dir2mcp #668.
//
// The staging owns two artifacts and restores both of them: the on-disk index
// generation and the content-hash gate. It owns nothing else. The rebuild runs
// through the ordinary ingest pipeline, which rewrites documents,
// representations and chunks IN PLACE, and none of that is staged. So the corpus
// after a rollback is not the corpus from before the run, and the command used
// to imply that it was.
//
// The fix reports rather than completes. Completing the rollback means
// snapshotting the whole sqlite graph per run and giving the networked backends
// a generation swap they do not expose. A rollback that restores only some of
// what changed is worse than one that says what it could not restore.
//
// These tests pin the report, because the report IS the fix.

// rollbackReportPhrases are the claims the report must carry on every backend:
// that it is partial, which rows it did not restore, and what the operator does
// next.
var rollbackReportPhrases = []string{
	"this rollback is partial",
	"did not restore the document, representation and chunk rows",
	"run `dir2mcp reindex` again",
}

// reindexUnbuildableIngestor stands in for a rebuild that never STARTS: the
// ingestor itself fails to construct. Nothing is rewritten in that case, so the
// rollback really is complete and must not claim otherwise.
func reindexUnbuildableIngestor(config.Config, model.Store) (model.Ingestor, error) {
	return nil, errors.New("forced ingestor construction failure")
}

// writeBackendConfig points the corpus at a vector-index backend. The networked
// backends never keep a local index file, so a rollback has nothing to put back
// for them. The endpoint is never contacted: `reindex` opens no index at all, so
// it only has to satisfy config validation.
func writeBackendConfig(t *testing.T, tmp, backend string) {
	t.Helper()
	lines := []string{"index_backend: " + backend}
	if backend == "qdrant" {
		lines = append(lines,
			"index:",
			"  qdrant:",
			"    url: http://qdrant.invalid:6334",
			"    collection: docs",
		)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(tmp, ".dir2mcp.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func assertRollbackReport(t *testing.T, stderr string) {
	t.Helper()
	for _, phrase := range rollbackReportPhrases {
		if !strings.Contains(stderr, phrase) {
			t.Errorf("a failed reindex must say what its rollback could not restore; stderr is missing %q\nstderr=%q", phrase, stderr)
		}
	}
}

// TestReindex_FailedRebuild_ReportsWhatTheRollbackDidNotRestore is the core #668
// regression on a backend that DOES keep a local index generation. Both staged
// artifacts go back, and the command must still say that the metadata rows did
// not.
func TestReindex_FailedRebuild_ReportsWhatTheRollbackDidNotRestore(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	const relPath = "docs/a.md"
	seedDocumentHash(t, stateDir, relPath, "HASH-BEFORE-REINDEX")
	seedIndexGeneration(t, stateDir)

	code, stderr := runReindexInDir(t, tmp, func(config.Config, model.Store) (model.Ingestor, error) {
		return reindexFailingIngestor{}, nil
	})
	if code == 0 {
		t.Fatalf("reindex should fail when the ingestor errors; got exit 0 stderr=%q", stderr)
	}
	assertRollbackReport(t, stderr)
	// The rollback still does its own half, so the report is an addition to the
	// #418 behaviour and not a replacement for it.
	if got := readDocumentHash(t, stateDir, relPath); got != "HASH-BEFORE-REINDEX" {
		t.Errorf("the content-hash gate must still be restored; want %q got %q", "HASH-BEFORE-REINDEX", got)
	}
	// The server-side sentence belongs to the networked backends only. On a
	// local backend the index generation WAS restored, so claiming otherwise
	// would be the opposite dishonesty.
	if strings.Contains(stderr, "keeps its vectors on the server") {
		t.Errorf("a local backend restored its index generation, so the report must not claim a server-side one; stderr=%q", stderr)
	}
}

// TestReindex_FailedRebuild_NetworkedBackend_ReportsTheUnrollbackableIndex
// covers the half of #668 the issue leads with. A networked backend keeps no
// local index file, so `index.StaleIndexFiles` stages nothing and the rollback
// has no generation to put back. The corpus is left with vectors from before the
// run beside a chunk graph that has moved on, and only the report says so.
func TestReindex_FailedRebuild_NetworkedBackend_ReportsTheUnrollbackableIndex(t *testing.T) {
	for _, backend := range []string{"qdrant", "pgvector"} {
		t.Run(backend, func(t *testing.T) {
			tmp := t.TempDir()
			stateDir := filepath.Join(tmp, ".dir2mcp")
			if err := os.MkdirAll(stateDir, 0o700); err != nil {
				t.Fatalf("mkdir state dir: %v", err)
			}
			writeBackendConfig(t, tmp, backend)
			seedDocumentHash(t, stateDir, "docs/a.md", "HASH-BEFORE-REINDEX")

			code, stderr := runReindexInDir(t, tmp, func(config.Config, model.Store) (model.Ingestor, error) {
				return reindexFailingIngestor{}, nil
			})
			if code == 0 {
				t.Fatalf("reindex should fail when the ingestor errors; got exit 0 stderr=%q", stderr)
			}
			assertRollbackReport(t, stderr)
			if !strings.Contains(stderr, "keeps its vectors on the server") || !strings.Contains(stderr, backend) {
				t.Errorf("%s keeps no local index generation, so the rollback must say it could not restore one; stderr=%q", backend, stderr)
			}
		})
	}
}

// TestReindex_SuccessfulRebuild_ReportsNoPartialRollback is a guard. The report
// is about a rollback, so a run that committed must not print it. A warning that
// appears on a healthy run is a warning operators learn to ignore.
func TestReindex_SuccessfulRebuild_ReportsNoPartialRollback(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	const relPath = "docs/a.md"
	seedDocumentHash(t, stateDir, relPath, "HASH-BEFORE-REINDEX")
	seedIndexGeneration(t, stateDir)

	code, stderr := runReindexInDir(t, tmp, func(_ config.Config, st model.Store) (model.Ingestor, error) {
		return reindexRebuildingIngestor{st: st, relPath: relPath, newHash: "HASH-AFTER-REBUILD"}, nil
	})
	if code != 0 {
		t.Fatalf("reindex should succeed; got exit %d stderr=%q", code, stderr)
	}
	if strings.Contains(stderr, "this rollback is partial") {
		t.Errorf("a committed reindex rolled nothing back, so it must not report a partial rollback; stderr=%q", stderr)
	}
}

// TestReindex_UnbuiltIngestor_ReportsNoPartialRollback draws the other boundary.
// When the ingestor cannot even be constructed the rebuild never runs, so no row
// was rewritten and the rollback IS complete. Reporting a partial rollback there
// would make the message untrue in the opposite direction.
func TestReindex_UnbuiltIngestor_ReportsNoPartialRollback(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	const relPath = "docs/a.md"
	const original = "HASH-BEFORE-REINDEX"
	seedDocumentHash(t, stateDir, relPath, original)
	seedIndexGeneration(t, stateDir)

	code, stderr := runReindexInDir(t, tmp, reindexUnbuildableIngestor)
	if code == 0 {
		t.Fatalf("reindex should fail when the ingestor cannot be built; got exit 0 stderr=%q", stderr)
	}
	if strings.Contains(stderr, "this rollback is partial") {
		t.Errorf("no rebuild ran, so nothing was left unrestored; stderr=%q", stderr)
	}
	if got := readDocumentHash(t, stateDir, relPath); got != original {
		t.Errorf("the content-hash gate must be restored when the rebuild never started; want %q got %q", original, got)
	}
}
