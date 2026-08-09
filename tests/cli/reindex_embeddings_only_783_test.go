package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Issue #783: an embed failure with a provider-side cause (a revoked or rotated
// key) parked every affected chunk in embedding_status='error', and nothing
// moved it back. Restarting with a VALID key changed nothing, because 'error'
// is not 'pending'. The only supported recovery was re-ingesting the documents,
// which re-runs extraction (minutes to hours) purely to redo the embed step
// (seconds). `reindex --embeddings-only` is that recovery.

// seedEmbedFailure creates a state dir whose corpus has one chunk parked in
// embedding_status='error' under the given category, and returns its chunk id.
func seedEmbedFailure(t *testing.T, stateDir, relPath, category string) uint64 {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("seed Init: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "md", SourceType: "filesystem", Status: "ok",
	}); err != nil {
		t.Fatalf("seed UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("seed GetDocumentByPath: %v", err)
	}
	var chunkID int64
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, rerr := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "raw_text", RepHash: "h-" + relPath,
		})
		if rerr != nil {
			return rerr
		}
		id, cerr := tx.InsertChunkWithSpans(ctx, model.Chunk{
			RepID: repID, Ordinal: 0, Text: "body of " + relPath, TextHash: "th-" + relPath,
			IndexKind: "text", EmbeddingStatus: "pending",
		}, nil)
		chunkID = id
		return cerr
	})
	if err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	if err := st.MarkFailedWithCategory(ctx, []uint64{uint64(chunkID)}, category, "401 unauthorized"); err != nil {
		t.Fatalf("seed MarkFailedWithCategory: %v", err)
	}
	return uint64(chunkID)
}

// embedPendingCount reports how many chunks the embed worker would pick up.
func embedPendingCount(t *testing.T, stateDir string) int64 {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("stats Init: %v", err)
	}
	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	return stats.EmbeddedPending
}

// runCLIInDir runs the CLI with args from tmp and returns exit code, stdout and
// stderr.
func runCLIInDir(t *testing.T, tmp string, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	var code int
	withWorkingDir(t, tmp, func() {
		code = app.RunWithContext(context.Background(), args)
	})
	return code, stdout.String(), stderr.String()
}

// TestReindexEmbeddingsOnly_RequeuesProviderFailures is the #783 regression:
// the failed chunks return to the embed queue, and no ingestor is constructed,
// so extraction is never re-run.
func TestReindexEmbeddingsOnly_RequeuesProviderFailures(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	seedEmbedFailure(t, stateDir, "docs/a.md", "auth")

	if got := embedPendingCount(t, stateDir); got != 0 {
		t.Fatalf("precondition: embedded_pending=%d, want 0 (the chunk is parked in error)", got)
	}

	code, _, stderr := runCLIInDir(t, tmp, []string{"reindex", "--embeddings-only"})
	if code != 0 {
		t.Fatalf("reindex --embeddings-only exit=%d stderr=%q", code, stderr)
	}
	if got := embedPendingCount(t, stateDir); got != 1 {
		t.Fatalf("embedded_pending=%d after the retry, want 1", got)
	}
	if !strings.Contains(stderr, "requeued 1 chunk(s)") {
		t.Errorf("expected a requeue summary on stderr, got %q", stderr)
	}
}

// TestReindexEmbeddingsOnly_LeavesTerminalCategories pins that a failure which
// is a property of the stored input is not retried: re-sending identical bytes
// to the same provider re-fails deterministically and only spends quota.
func TestReindexEmbeddingsOnly_LeavesTerminalCategories(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	seedEmbedFailure(t, stateDir, "docs/huge.md", "payload_too_large")

	code, _, stderr := runCLIInDir(t, tmp, []string{"reindex", "--embeddings-only"})
	if code != 0 {
		t.Fatalf("reindex --embeddings-only exit=%d stderr=%q", code, stderr)
	}
	if got := embedPendingCount(t, stateDir); got != 0 {
		t.Fatalf("embedded_pending=%d, want 0: payload_too_large must stay terminal", got)
	}
	if !strings.Contains(stderr, "not retried") {
		t.Errorf("expected the report to name what it left alone, got %q", stderr)
	}
}

// TestReindexEmbeddingsOnly_JSONPayload pins the machine-readable shape a
// recovery script consumes.
func TestReindexEmbeddingsOnly_JSONPayload(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	seedEmbedFailure(t, stateDir, "docs/a.md", "auth")

	code, stdout, stderr := runCLIInDir(t, tmp, []string{"--json", "reindex", "--embeddings-only"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	var payload struct {
		Requeued          int64            `json:"requeued"`
		Categories        []string         `json:"categories"`
		MatchedByCategory map[string]int64 `json:"matched_by_category"`
		EmbeddingsOnly    bool             `json:"embeddings_only"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("unmarshal %q: %v", stdout, err)
	}
	if payload.Requeued != 1 || !payload.EmbeddingsOnly {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if payload.MatchedByCategory["auth"] != 1 {
		t.Errorf("expected the per-category breakdown to explain the total, got %+v", payload.MatchedByCategory)
	}
	if len(payload.Categories) == 0 {
		t.Errorf("expected the retried categories to be reported, got %+v", payload.Categories)
	}
}

// TestReindexEmbeddingsOnly_ErrorCategoryFilter pins the --error-category
// filter and its validation.
func TestReindexEmbeddingsOnly_ErrorCategoryFilter(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	seedEmbedFailure(t, stateDir, "docs/a.md", "auth")
	seedEmbedFailure(t, stateDir, "docs/b.md", "rate_limit")

	code, _, stderr := runCLIInDir(t, tmp, []string{"reindex", "--embeddings-only", "--error-category", "auth"})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if got := embedPendingCount(t, stateDir); got != 1 {
		t.Fatalf("embedded_pending=%d, want 1 (only the auth failure was selected)", got)
	}

	badCode, _, badStderr := runCLIInDir(t, tmp, []string{"reindex", "--embeddings-only", "--error-category", "not-a-category"})
	if badCode != 2 {
		t.Fatalf("unknown category exit=%d, want 2 (stderr=%q)", badCode, badStderr)
	}
	if !strings.Contains(badStderr, "unknown error category") {
		t.Errorf("expected an explicit vocabulary error, got %q", badStderr)
	}
}

// TestReindex_StillRejectsPositionalArguments guards the pre-existing CLI
// contract: adding flags must not turn `reindex extra` into a valid invocation.
func TestReindex_StillRejectsPositionalArguments(t *testing.T) {
	tmp := t.TempDir()
	code, _, stderr := runCLIInDir(t, tmp, []string{"reindex", "extra"})
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "reindex command does not accept arguments") {
		t.Errorf("unexpected message: %q", stderr)
	}
}
