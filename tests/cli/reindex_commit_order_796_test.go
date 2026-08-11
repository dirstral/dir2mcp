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
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Reindex commit order: dir2mcp #796.
//
// A reindex stages two undo records. `*.reindex-old` undoes the index move, and
// the content_hash snapshot undoes the cleared incremental gate. A crash between
// them decides which half the next run's recovery sees, so their ORDER decides
// which way the corpus ends up wrong.
//
// The old order dropped the snapshot first. A crash in that window left hashes
// that say "new" beside an index backup that says "roll me back", and
// recoverInterruptedReindex adopts the backup. The corpus then serves the
// PREVIOUS index generation while its own hashes assert that the new content is
// already ingested, so the gate skips those documents on every later run.
// Nothing re-ingests them and nothing reports it.
//
// Committing the index first inverts the failure: recovery restores hashes that
// under-state the corpus, the next scan redoes work it did not need to, and the
// corpus converges. Redundant work is recoverable. A gate set too far ahead is
// not.

// hashUndoOrderStore records the on-disk state at the moment the reindex drops
// the content-hash snapshot. That is the observation the ordering rule is about:
// once the snapshot is gone, an index backup still on disk is a live crash
// window.
type hashUndoOrderStore struct {
	*store.SQLiteStore

	stateDir string
	// indexBackupsAtDiscard names the *.reindex-old files that still existed
	// when DiscardContentHashBackup was called. It is nil when the discard
	// never ran.
	indexBackupsAtDiscard []string
	discardCalls          int
}

func (s *hashUndoOrderStore) DiscardContentHashBackup(ctx context.Context) error {
	s.discardCalls++
	s.indexBackupsAtDiscard = indexBackupsIn(s.stateDir)
	return s.SQLiteStore.DiscardContentHashBackup(ctx)
}

// seedIndexGeneration writes the index files a reindex moves aside, so the
// staging window this file tests actually opens. The names come from the
// package that owns them, so a rename cannot leave the test silently staging
// nothing and passing.
func seedIndexGeneration(t *testing.T, stateDir string) {
	t.Helper()
	for _, name := range []string{index.TextIndexFileName, index.CodeIndexFileName} {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte("previous generation"), 0o600); err != nil {
			t.Fatalf("seed index file %s: %v", name, err)
		}
	}
}

// indexBackupsIn lists the moved-aside index generations in a state directory.
func indexBackupsIn(stateDir string) []string {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return nil
	}
	var found []string
	for _, entry := range entries {
		if name := entry.Name(); filepath.Ext(name) == ".reindex-old" {
			found = append(found, name)
		}
	}
	return found
}

// TestReindex_CommitsTheIndexBeforeDroppingTheHashUndo is the #796 contract. A
// successful reindex must not drop the content-hash snapshot while an index
// backup is still on disk, because that pair is exactly the state recovery
// reads as "roll the index back, the hashes are already current".
func TestReindex_CommitsTheIndexBeforeDroppingTheHashUndo(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	const relPath = "docs/a.md"
	seedDocumentHash(t, stateDir, relPath, "HASH-BEFORE-REINDEX")

	// A previous index generation must exist, or nothing is moved aside and the
	// window under test cannot open.
	seedIndexGeneration(t, stateDir)

	var recorder *hashUndoOrderStore
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewStore: func(cfg config.Config) model.Store {
			recorder = &hashUndoOrderStore{
				SQLiteStore: store.NewSQLiteStore(filepath.Join(cfg.StateDir, "meta.sqlite")),
				stateDir:    cfg.StateDir,
			}
			return recorder
		},
		NewIngestor: func(_ config.Config, st model.Store) (model.Ingestor, error) {
			return reindexRebuildingIngestor{st: st, relPath: relPath, newHash: "HASH-AFTER-REBUILD"}, nil
		},
	})

	var code int
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		code = app.RunWithContext(ctx, []string{"reindex"})
	})
	if code != 0 {
		t.Fatalf("reindex should succeed; got exit %d stderr=%q", code, stderr.String())
	}
	if recorder == nil || recorder.discardCalls == 0 {
		t.Fatalf("the reindex never dropped the content-hash snapshot; the ordering window was not exercised (stderr=%q)", stderr.String())
	}
	if len(recorder.indexBackupsAtDiscard) != 0 {
		t.Errorf("the content-hash snapshot was dropped while %v still existed: a crash in that window leaves recovery adopting the previous index generation over the new hashes, and the gate then skips those documents for ever (#796)",
			recorder.indexBackupsAtDiscard)
	}
}

// TestReindex_LeavesNoUndoRecordsAfterSuccess states the end condition the
// ordering serves: a committed reindex owns both halves, so neither undo record
// survives it. A leftover of either kind is a crash window that stayed open.
func TestReindex_LeavesNoUndoRecordsAfterSuccess(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	const relPath = "docs/a.md"
	const rebuilt = "HASH-AFTER-REBUILD"
	seedDocumentHash(t, stateDir, relPath, "HASH-BEFORE-REINDEX")
	seedIndexGeneration(t, stateDir)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(_ config.Config, st model.Store) (model.Ingestor, error) {
			return reindexRebuildingIngestor{st: st, relPath: relPath, newHash: rebuilt}, nil
		},
	})

	var code int
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		code = app.RunWithContext(ctx, []string{"reindex"})
	})
	if code != 0 {
		t.Fatalf("reindex should succeed; got exit %d stderr=%q", code, stderr.String())
	}
	if left := indexBackupsIn(stateDir); len(left) != 0 {
		t.Errorf("a committed reindex left the index backup(s) %v behind; the next run's recovery would adopt them over the rebuilt corpus", left)
	}
	if got := readDocumentHash(t, stateDir, relPath); got != rebuilt {
		t.Errorf("content_hash = %q, want the rebuild's %q", got, rebuilt)
	}
}
