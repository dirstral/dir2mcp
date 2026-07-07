package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// recordingEmbedder returns deterministic vectors and records every input it was
// asked to embed, so a test can assert an oversize chunk was NEVER sent to the
// provider (issue #399 item 2).
type recordingEmbedder struct {
	seen []string
}

func (e *recordingEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	e.seen = append(e.seen, inputs...)
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// TestEmbedAndIndex_OversizeInputSkippedNotSent pins issue #399 item 2: a text
// chunk longer than the per-input rune cap is skipped up-front — marked
// embedding_failure with a clear reason — and is NEVER sent to the provider,
// while its healthy siblings still embed and index.
func TestEmbedAndIndex_OversizeInputSkippedNotSent(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := index.NewHNSWIndex("")
	emb := &recordingEmbedder{}
	// Small cap so the test's "oversize" input is unambiguously over it while the
	// healthy inputs stay well under.
	worker := &index.EmbeddingWorker{Source: src, Index: idx, Embedder: emb, BatchSize: 8, MaxInputRunes: 100}

	oversize := strings.Repeat("x", 500)
	tasks := []model.ChunkTask{
		textTask(1, "healthy one"),
		textTask(2, oversize), // over the 100-rune cap
		textTask(3, "healthy three"),
	}

	n, err := worker.EmbedAndIndex(ctx, "text", tasks)
	if err != nil {
		t.Fatalf("EmbedAndIndex returned error after skipping oversize chunk: %v", err)
	}
	if n != 2 {
		t.Fatalf("indexed = %d, want 2 (both healthy siblings)", n)
	}

	// The oversize input must never have reached the provider.
	for _, in := range emb.seen {
		if strings.Contains(in, oversize) {
			t.Fatalf("oversize input was sent to the embedder; it must be skipped before sending")
		}
	}

	if len(src.failedLabels) != 1 || src.failedLabels[0] != 2 {
		t.Fatalf("failed labels = %v, want exactly [2] (only the oversize chunk)", src.failedLabels)
	}
	if !strings.Contains(src.failedReason, "exceeds max embed input size") {
		t.Fatalf("failed reason = %q, want a clear oversize skip-reason", src.failedReason)
	}
	if src.failedCategory == "" {
		t.Fatalf("oversize chunk should be marked failed with a classification category, got empty")
	}

	wantEmbedded := map[uint64]bool{1: true, 3: true}
	if len(src.embedded) != 2 {
		t.Fatalf("embedded labels = %v, want the 2 healthy chunks", src.embedded)
	}
	for _, l := range src.embedded {
		if !wantEmbedded[l] {
			t.Fatalf("embedded contains unexpected label %d: %v", l, src.embedded)
		}
	}
}
