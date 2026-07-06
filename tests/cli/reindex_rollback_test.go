package tests

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// reindexFailingIngestor's Reindex fails, standing in for an interrupted or
// errored rebuild so the reindex rollback path (issue #418) can be exercised.
type reindexFailingIngestor struct{}

func (reindexFailingIngestor) Run(context.Context) error { return nil }
func (reindexFailingIngestor) Reindex(context.Context) error {
	return errors.New("forced reindex failure")
}

// reindexRebuildingIngestor stands in for a durable rebuild: its Reindex writes
// a fresh content_hash for the seeded document, so tests can assert a committed
// reindex keeps the rebuild's hash rather than restoring the pre-clear one.
type reindexRebuildingIngestor struct {
	st      model.Store
	relPath string
	newHash string
}

func (reindexRebuildingIngestor) Run(context.Context) error { return nil }
func (i reindexRebuildingIngestor) Reindex(ctx context.Context) error {
	return i.st.UpsertDocument(ctx, model.Document{
		RelPath:     i.relPath,
		DocType:     "md",
		ContentHash: i.newHash,
		Status:      "ready",
	})
}

// seedDocumentHash opens the reindex state store, writes a document with the
// given content_hash, and closes it so the reindex command can reopen the same
// meta.sqlite.
func seedDocumentHash(t *testing.T, stateDir, relPath, hash string) {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	ctx := context.Background()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("seed Init: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:     relPath,
		DocType:     "md",
		ContentHash: hash,
		Status:      "ready",
	}); err != nil {
		t.Fatalf("seed UpsertDocument: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
}

// readDocumentHash reopens the state store and returns the document's stored
// content_hash.
func readDocumentHash(t *testing.T, stateDir, relPath string) string {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("read Init: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("read GetDocumentByPath: %v", err)
	}
	return doc.ContentHash
}

// TestReindex_RestoresContentHashesOnFailure verifies the issue #418 fix that a
// failed/interrupted reindex restores documents.content_hash. prepareReindexStore
// clears the hashes before the rebuild; when the rebuild errors, the pre-clear
// snapshot must be restored so the next incremental sync keeps its "already
// indexed" gate instead of reprocessing the whole corpus.
func TestReindex_RestoresContentHashesOnFailure(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	const relPath = "docs/a.md"
	const original = "HASH-BEFORE-REINDEX"
	seedDocumentHash(t, stateDir, relPath, original)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(_ config.Config, _ model.Store) (model.Ingestor, error) {
			return reindexFailingIngestor{}, nil
		},
	})

	var code int
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		code = app.RunWithContext(ctx, []string{"reindex"})
	})
	if code == 0 {
		t.Fatalf("reindex should fail when the ingestor errors; got exit 0 stderr=%q", stderr.String())
	}

	if got := readDocumentHash(t, stateDir, relPath); got != original {
		t.Errorf("content_hash must be restored after a failed reindex; want %q got %q", original, got)
	}
}

// TestReindex_DiscardsContentHashBackupOnSuccess verifies the commit side of the
// issue #418 fix: after a durable rebuild the pre-clear snapshot is discarded,
// so the rebuild's fresh content_hash survives and is not overwritten by a
// lingering backup.
func TestReindex_DiscardsContentHashBackupOnSuccess(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	const relPath = "docs/a.md"
	const original = "HASH-BEFORE-REINDEX"
	const rebuilt = "HASH-AFTER-REBUILD"
	seedDocumentHash(t, stateDir, relPath, original)

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(_ config.Config, st model.Store) (model.Ingestor, error) {
			return reindexRebuildingIngestor{st: st, relPath: relPath, newHash: rebuilt}, nil
		},
	})

	var code int
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		code = app.RunWithContext(ctx, []string{"reindex"})
	})
	if code != 0 {
		t.Fatalf("reindex should succeed; got exit %d stderr=%q", code, stderr.String())
	}

	// The rebuild's hash must survive (backup discarded, not restored over it).
	if got := readDocumentHash(t, stateDir, relPath); got != rebuilt {
		t.Errorf("content_hash should reflect the rebuild after commit; want %q got %q", rebuilt, got)
	}

	// A subsequent restore call must be a no-op: the snapshot table is gone.
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("post Init: %v", err)
	}
	if err := st.RestoreContentHashes(ctx); err != nil {
		t.Fatalf("post-commit RestoreContentHashes should be a no-op: %v", err)
	}
	if got := readDocumentHash(t, stateDir, relPath); got != rebuilt {
		t.Errorf("no-op restore must not resurrect the pre-clear hash; want %q got %q", rebuilt, got)
	}
}

// TestReindex_RestoresIndexOnFailure verifies the issue #418 safe-ordering
// fix: reindex moves the previous on-disk index aside (rename, not delete) and
// restores it when the rebuild fails, so an interrupted reindex leaves the
// corpus's working index in place instead of an empty/half-built one. The old
// prepareReindexStore deleted the index up front, so a failed rebuild left the
// corpus with nothing.
func TestReindex_RestoresIndexOnFailure(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	indexFile := filepath.Join(stateDir, "vectors_text.v2.hnsw")
	const original = "PREVIOUS-INDEX"
	if err := os.WriteFile(indexFile, []byte(original), 0o600); err != nil {
		t.Fatalf("seed index file: %v", err)
	}

	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIOAndHooks(&stdout, &stderr, cli.RuntimeHooks{
		NewIngestor: func(_ config.Config, _ model.Store) (model.Ingestor, error) {
			return reindexFailingIngestor{}, nil
		},
	})

	var code int
	withWorkingDir(t, tmp, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		code = app.RunWithContext(ctx, []string{"reindex"})
	})
	if code == 0 {
		t.Fatalf("reindex should fail when the ingestor errors; got exit 0 stderr=%q", stderr.String())
	}

	// The previous index must be restored byte-for-byte.
	data, err := os.ReadFile(indexFile)
	if err != nil {
		t.Fatalf("index file must be restored after a failed reindex; read err=%v", err)
	}
	if string(data) != original {
		t.Errorf("restored index content: want %q got %q", original, string(data))
	}
	// The moved-aside backup sidecar must not linger after rollback.
	if _, err := os.Stat(indexFile + ".reindex-old"); !os.IsNotExist(err) {
		t.Errorf("backup sidecar should be gone after rollback; stat err=%v", err)
	}
}
