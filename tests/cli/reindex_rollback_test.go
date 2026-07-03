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
)

// reindexFailingIngestor's Reindex fails, standing in for an interrupted or
// errored rebuild so the reindex rollback path (issue #418) can be exercised.
type reindexFailingIngestor struct{}

func (reindexFailingIngestor) Run(context.Context) error { return nil }
func (reindexFailingIngestor) Reindex(context.Context) error {
	return errors.New("forced reindex failure")
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
