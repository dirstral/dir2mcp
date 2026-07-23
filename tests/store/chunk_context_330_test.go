package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// newContextStore opens a fresh sqlite store with one document, returning the
// store and the document's rel_path.
func newContextStore(t *testing.T) (*store.SQLiteStore, string) {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	const relPath = "docs/a.md"
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: relPath, DocType: "md", SourceType: "local", Status: "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	return st, relPath
}

// insertChunk writes one chunk of a raw_text representation for relPath.
func insertChunk(t *testing.T, st *store.SQLiteStore, relPath string, ordinal int, chunk model.Chunk) {
	t.Helper()
	ctx := context.Background()
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	repID, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID: doc.DocID, RepType: "raw_text", RepHash: "h", CreatedUnix: 1,
	})
	if err != nil {
		t.Fatalf("UpsertRepresentation: %v", err)
	}
	chunk.RepID = repID
	chunk.Ordinal = ordinal
	if chunk.IndexKind == "" {
		chunk.IndexKind = "text"
	}
	if chunk.EmbeddingStatus == "" {
		chunk.EmbeddingStatus = "pending"
	}
	if _, err := st.InsertChunkWithSpans(ctx, chunk, nil); err != nil {
		t.Fatalf("InsertChunkWithSpans: %v", err)
	}
}

// TestChunkContext_RoundTripsThroughNextPending pins SPEC §5.3/§8.1.8: the
// additive chunk_context column round-trips onto the ChunkTask the embed worker
// leases, so the worker prepends the context WITHOUT a provider round-trip —
// while the task's Text (what snippets and citations render) stays raw.
func TestChunkContext_RoundTripsThroughNextPending(t *testing.T) {
	ctx := context.Background()
	st, relPath := newContextStore(t)

	insertChunk(t, st, relPath, 0, model.Chunk{
		Text:          "the raw chunk text",
		TextHash:      "th",
		Context:       "From the Q3 report, discussing revenue.",
		EmbeddingMode: model.EmbeddingModeContextualized,
	})

	tasks, err := st.NextPending(ctx, 10, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 pending task, got %d", len(tasks))
	}
	task := tasks[0]
	if task.Text != "the raw chunk text" {
		t.Errorf("Text must stay the RAW chunk, got %q", task.Text)
	}
	if task.Context != "From the Q3 report, discussing revenue." {
		t.Errorf("Context did not round-trip, got %q", task.Context)
	}
	if want := task.Context + "\n\n" + task.Text; task.EmbedInput() != want {
		t.Errorf("EmbedInput() = %q, want %q", task.EmbedInput(), want)
	}
	// Citation faithfulness: the snippet the store derives is built from the raw
	// text, so the generated context can never reach a hit or a citation (#403).
	if strings.Contains(task.Metadata.Snippet, "Q3 report") {
		t.Errorf("the generated context leaked into the snippet: %q", task.Metadata.Snippet)
	}

	byID, _, err := st.ChunkTaskByID(ctx, task.Label)
	if err != nil {
		t.Fatalf("ChunkTaskByID: %v", err)
	}
	if byID.Context != task.Context || byID.Text != task.Text {
		t.Errorf("ChunkTaskByID lost the raw/context split: %+v", byID)
	}
}

// TestChunkContext_DefaultsAreThePreFeatureShape pins the additive-migration
// contract (SPEC §5.3): a chunk written WITHOUT contextual fields reads back as
// "contextual retrieval was off" — empty context, embedding_mode=disabled — which
// is exactly how a pre-feature index's existing rows must read.
func TestChunkContext_DefaultsAreThePreFeatureShape(t *testing.T) {
	ctx := context.Background()
	st, relPath := newContextStore(t)

	insertChunk(t, st, relPath, 0, model.Chunk{Text: "plain chunk", TextHash: "th"})

	tasks, err := st.NextPending(ctx, 10, "text")
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 pending task, got %d", len(tasks))
	}
	if tasks[0].Context != "" {
		t.Errorf("a pre-feature chunk must carry no context, got %q", tasks[0].Context)
	}
	if tasks[0].EmbedInput() != tasks[0].Text {
		t.Error("with no context the embed input must be the raw text byte-for-byte")
	}
	// And the retry gate sees nothing to heal.
	pending, err := st.HasFallbackContextChunks(ctx, relPath)
	if err != nil {
		t.Fatalf("HasFallbackContextChunks: %v", err)
	}
	if pending {
		t.Error("a corpus with no fallback chunks must not be reported as needing a retry")
	}
}

// TestHasFallbackContextChunks pins the self-heal gate (SPEC §8.1.8): a chunk
// whose context generation failed is durably discoverable, so the next scan
// retries it instead of leaving a silent, permanent coverage hole.
func TestHasFallbackContextChunks(t *testing.T) {
	ctx := context.Background()
	st, relPath := newContextStore(t)

	insertChunk(t, st, relPath, 0, model.Chunk{
		Text: "healthy chunk", TextHash: "h1",
		Context: "situating context", EmbeddingMode: model.EmbeddingModeContextualized,
	})
	insertChunk(t, st, relPath, 1, model.Chunk{
		Text: "failed chunk", TextHash: "h2",
		EmbeddingMode: model.EmbeddingModeFallback,
	})

	pending, err := st.HasFallbackContextChunks(ctx, relPath)
	if err != nil {
		t.Fatalf("HasFallbackContextChunks: %v", err)
	}
	if !pending {
		t.Fatal("a document with a fallback chunk must be reported for retry")
	}

	// A different document is unaffected.
	if pending, err := st.HasFallbackContextChunks(ctx, "docs/other.md"); err != nil || pending {
		t.Fatalf("unrelated document: pending=%v err=%v", pending, err)
	}

	// Healing the chunk clears the gate.
	insertChunk(t, st, relPath, 1, model.Chunk{
		Text: "failed chunk", TextHash: "h2",
		Context: "now situated", EmbeddingMode: model.EmbeddingModeContextualized,
	})
	pending, err = st.HasFallbackContextChunks(ctx, relPath)
	if err != nil {
		t.Fatalf("HasFallbackContextChunks: %v", err)
	}
	if pending {
		t.Fatal("once every chunk is contextualized the retry gate must clear")
	}
}
