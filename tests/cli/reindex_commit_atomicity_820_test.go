package tests

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// Reindex commit atomicity: dir2mcp #820.
//
// commit() unlinks the moved-aside generations one by one. The set of backups
// carries no boundary on disk, so a crash inside that loop leaves a SUBSET, and
// the recovery scan cannot tell "two of three were already discarded" from "a
// rebuild staged two files and died". It adopted the survivors, which put one
// generation's code index back beside another generation's text index.
//
// Ordering cannot close this window. Every prefix of the removals is a legal
// observation, and every prefix looks exactly like a smaller staged set. So the
// commit now writes a marker before the first unlink and removes it after the
// last one. The marker states the fact the file names cannot carry: the rebuild
// is durable, therefore every backup beside it is superseded.
//
// The crash is seeded the way the #727 suite seeds one, by leaving the
// interrupted run's artifacts in place and never calling commit or rollback,
// which is exactly what a crash does. The seed follows the real staging order
// step by step, because a helper that diverged from it once produced a CI
// failure that read as a flake (#825).

const (
	// reindexCommitMarkerFile mirrors reindexCommitMarkerName in internal/cli,
	// which is unexported. Spelled out here for the same reason
	// crashBackupSuffix is: the on-disk name is part of what an interrupted
	// corpus looks like to an operator, so this suite should notice if it
	// silently changes.
	reindexCommitMarkerFile = "reindex-commit"
	// crashRebuiltHash is the content_hash the durable rebuild stamped before
	// the process was killed inside commit().
	crashRebuiltHash = "HASH-AFTER-DURABLE-REBUILD"
)

// stagedIndexGeneration names the index files this suite seeds and stages. Both
// names come from the package that owns them, so a rename cannot leave the test
// staging nothing and passing green.
func stagedIndexGeneration() []string {
	return []string{index.TextIndexFileName, index.CodeIndexFileName}
}

// supersededBytes is the content of the generation the durable rebuild replaced.
// One value per file, so a test can say WHICH slot was rolled back.
func supersededBytes(name string) string { return crashGoodIndex + "/" + name }

// seedCrashedReindexCommit lays down the state a reindex leaves when it is
// killed INSIDE commit(). It walks the real protocol in order:
//
//  1. the corpus before the run: a complete index generation and a document
//     whose content_hash describes it;
//  2. staging, as prepareReindexStore does it: recover, snapshot the hashes,
//     clear them, then move each live index file aside with an atomic rename;
//  3. the rebuild, which succeeds: the sqlite rows carry fresh content hashes.
//     The live index slots stay EMPTY on purpose, because `reindex` rebuilds the
//     rows and leaves the vectors to the daemon's embed worker;
//  4. commit(), which writes the marker and then unlinks. The process dies after
//     the FIRST unlink, so the text generation is gone and the code generation is
//     still in its backup slot.
//
// The content-hash snapshot is deliberately left in place: #796 drops it only
// after commit returns, so a partial commit always keeps it.
func seedCrashedReindexCommit(t *testing.T, stateDir, relPath string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	for _, name := range stagedIndexGeneration() {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte(supersededBytes(name)), 0o600); err != nil {
			t.Fatalf("seed index generation %s: %v", name, err)
		}
	}
	seedDocumentHash(t, stateDir, relPath, crashGoodHash)

	crashHashStateFromCurrent(t, stateDir)
	for _, name := range stagedIndexGeneration() {
		live := filepath.Join(stateDir, name)
		if err := os.Rename(live, live+crashBackupSuffix); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
	}

	seedDocumentHash(t, stateDir, relPath, crashRebuiltHash)

	if err := os.WriteFile(filepath.Join(stateDir, reindexCommitMarkerFile),
		[]byte("dir2mcp: a reindex reached its commit point.\n"), 0o600); err != nil {
		t.Fatalf("seed commit marker: %v", err)
	}
	first := filepath.Join(stateDir, index.TextIndexFileName+crashBackupSuffix)
	if err := os.Remove(first); err != nil {
		t.Fatalf("seed the first unlink of commit(): %v", err)
	}
}

// liveIndexContent returns the bytes in a live index slot, or "" when the slot
// is empty. An empty slot is the normal end state of a `reindex`: the rows are
// rebuilt and the embed worker writes the vectors afterwards.
func liveIndexContent(t *testing.T, stateDir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, name))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read live index %s: %v", name, err)
	}
	return string(data)
}

// assertCommitFinished states the end condition of an interrupted commit: no
// superseded generation is live, no backup survives, and the marker is gone.
func assertCommitFinished(t *testing.T, stateDir, stderr string) {
	t.Helper()
	for _, name := range stagedIndexGeneration() {
		if got := liveIndexContent(t, stateDir, name); got == supersededBytes(name) {
			t.Errorf("%s holds the superseded generation %q: an interrupted commit was read as a rollback target, so this corpus now mixes index generations (#820)", name, got)
		}
	}
	if left := indexBackupsIn(stateDir); len(left) != 0 {
		t.Errorf("an interrupted commit must be finished, not adopted; %v are still on disk", left)
	}
	if _, err := os.Stat(filepath.Join(stateDir, reindexCommitMarkerFile)); !os.IsNotExist(err) {
		t.Errorf("the commit marker must be cleared once the commit is finished; stat err=%v", err)
	}
	// "discarded" as well as the name: a warning that merely NAMES the file is
	// also what the adopt path prints, so the wording is what separates
	// "finished the commit" from "rolled the corpus back".
	if !strings.Contains(stderr, "discarded") || !strings.Contains(stderr, index.CodeIndexFileName) {
		t.Errorf("the operator must be told which superseded generation was discarded; stderr=%q", stderr)
	}
}

// TestReindex_AfterCrashedCommit_DiscardsTheSupersededGeneration is the #820
// regression on the `reindex` side. The next run must finish the commit the
// crashed run started. Adopting what is left puts the previous CODE index back
// beside the new TEXT slot, and both then describe different chunk graphs.
//
// The retry fails on purpose. A failing retry rolls back whatever it staged, so
// what is live at the end is exactly what recovery decided, with nothing written
// over it by a fresh rebuild.
func TestReindex_AfterCrashedCommit_DiscardsTheSupersededGeneration(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	const relPath = "docs/a.md"
	seedCrashedReindexCommit(t, stateDir, relPath)

	code, stderr := runReindexInDir(t, tmp, func(config.Config, model.Store) (model.Ingestor, error) {
		return reindexFailingIngestor{}, nil
	})
	if code == 0 {
		t.Fatalf("reindex should fail when the ingestor errors; got exit 0 stderr=%q", stderr)
	}
	assertCommitFinished(t, stateDir, stderr)
	// The snapshot outlives the commit by design (#796), so recovery restores it.
	// The gate then UNDER-states the corpus and the next scan redoes work it did
	// not strictly need to redo, which is the recoverable direction.
	if got := readDocumentHash(t, stateDir, relPath); got != crashGoodHash {
		t.Errorf("the content-hash snapshot must still be restored after an interrupted commit; want %q got %q", crashGoodHash, got)
	}
}

// TestUp_AfterCrashedCommit_DoesNotServeTheSupersededGeneration asks the
// question the issue is really about: not which files are on disk afterwards,
// but which generation the daemon opened to serve from. A daemon that adopts a
// superseded backup answers from an index whose vectors key chunk rows the
// durable rebuild already replaced.
func TestUp_AfterCrashedCommit_DoesNotServeTheSupersededGeneration(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	const relPath = "docs/a.md"
	seedCrashedReindexCommit(t, stateDir, relPath)

	code, stderr, indices := upStartupProbe(t, tmp)
	if code != 0 {
		t.Fatalf("up should start after finishing an interrupted reindex commit; got exit %d stderr=%q", code, stderr)
	}
	for _, kind := range []string{index.KindText, index.KindCode} {
		ix := indices[kind]
		if ix == nil {
			t.Fatalf("no %s index was built; stderr=%q", kind, stderr)
		}
		name := indexFileNameForKind(kind)
		if got := ix.loadedString(); got == supersededBytes(name) {
			t.Errorf("the daemon served the superseded %s generation %q; the durable rebuild's rows no longer match it (#820)", kind, got)
		}
	}
	assertCommitFinished(t, stateDir, stderr)
}

// TestReindex_SuccessfulRun_LeavesNoCommitMarker is the guard on the other side:
// the marker is transient. A marker that outlives its commit would make the next
// recovery discard a generation that is a genuine rollback target, which is the
// #727 destruction with a new cause.
func TestReindex_SuccessfulRun_LeavesNoCommitMarker(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	const relPath = "docs/a.md"
	const rebuilt = "HASH-AFTER-REBUILD"
	seedDocumentHash(t, stateDir, relPath, "HASH-BEFORE-REINDEX")
	seedIndexGeneration(t, stateDir)

	code, stderr := runReindexInDir(t, tmp, func(_ config.Config, st model.Store) (model.Ingestor, error) {
		return reindexRebuildingIngestor{st: st, relPath: relPath, newHash: rebuilt}, nil
	})
	if code != 0 {
		t.Fatalf("reindex should succeed; got exit %d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(stateDir, reindexCommitMarkerFile)); !os.IsNotExist(err) {
		t.Errorf("a committed reindex must remove its commit marker; stat err=%v", err)
	}
	if left := indexBackupsIn(stateDir); len(left) != 0 {
		t.Errorf("a committed reindex must leave no backup behind; got %v", left)
	}
}
