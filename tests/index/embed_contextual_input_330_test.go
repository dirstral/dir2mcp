package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestEmbedAndIndex_ContextIsPrependedToEmbedInputOnly pins the SPEC §8.1.8
// embed-input transform: the embedder receives `context + "\n\n" + chunk`, while
// the chunk's own text — the text every display and citation path reads — is
// untouched. A chunk with no context embeds byte-identically to before.
func TestEmbedAndIndex_ContextIsPrependedToEmbedInputOnly(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := index.NewHNSWIndex("")
	emb := &recordingEmbedder{}
	worker := &index.EmbeddingWorker{Source: src, Index: idx, Embedder: emb, BatchSize: 8}

	contextualized := textTask(1, "ad revenue grew 12%")
	contextualized.Context = "From the Q3 2026 earnings call."
	raw := textTask(2, "no context here")

	n, err := worker.EmbedAndIndex(ctx, "text", []model.ChunkTask{contextualized, raw})
	if err != nil {
		t.Fatalf("EmbedAndIndex: %v", err)
	}
	if n != 2 {
		t.Fatalf("indexed %d chunks, want 2", n)
	}
	if len(emb.seen) != 2 {
		t.Fatalf("embedder saw %d inputs, want 2: %q", len(emb.seen), emb.seen)
	}
	if want := "From the Q3 2026 earnings call.\n\nad revenue grew 12%"; emb.seen[0] != want {
		t.Errorf("contextualized embed input = %q, want %q", emb.seen[0], want)
	}
	// The raw chunk is unchanged — the default (feature-off) path.
	if emb.seen[1] != "no context here" {
		t.Errorf("uncontextualized embed input = %q, want the raw text", emb.seen[1])
	}
	// The task's own text (and therefore its snippet/citation) never gained the
	// context: only EmbedInput joins them (#403).
	if strings.Contains(contextualized.Text, "earnings call") {
		t.Error("the generated context must never be written into the chunk text")
	}
}
