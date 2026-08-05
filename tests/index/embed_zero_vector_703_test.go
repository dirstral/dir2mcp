package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// zeroVectorText is the sentinel input the zeroVectorEmbedder answers with an
// all-zero vector — a SUCCESSFUL response (no error) carrying an unusable
// embedding, which is exactly how this failure arrives in production.
const zeroVectorText = "ZERO-VECTOR-INPUT"

// zeroVectorEmbedder models a provider/proxy that returns 200 OK with an
// all-zero vector for one input (quota exhaustion, a model that failed to load,
// a truncated input) and healthy vectors for the rest.
type zeroVectorEmbedder struct{}

func (e *zeroVectorEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		if strings.Contains(in, zeroVectorText) {
			out[i] = []float32{0, 0}
			continue
		}
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// TestEmbedAndIndex_ZeroVectorNeverIndexed pins issue #703 at the worker: a
// zero-norm vector must never be written to an index or marked embedded, and it
// must not take its healthy batch siblings down with it.
//
// The worker is the LAST gate: it accepts any model.Embedder, including one this
// process does not own (a distributed worker, a self-hosted service), so the
// per-adapter checks alone cannot guarantee the index stays clean.
func TestEmbedAndIndex_ZeroVectorNeverIndexed(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := index.NewHNSWIndex("")
	worker := &index.EmbeddingWorker{Source: src, Index: idx, Embedder: &zeroVectorEmbedder{}, BatchSize: 8}

	tasks := []model.ChunkTask{
		textTask(1, "healthy one"),
		textTask(2, zeroVectorText), // the undirected vector
		textTask(3, "healthy three"),
	}

	n, err := worker.EmbedAndIndex(ctx, "text", tasks)
	if err != nil {
		t.Fatalf("EmbedAndIndex: %v", err)
	}
	if n != 2 {
		t.Fatalf("indexed = %d, want 2 (both healthy siblings, not the zero vector)", n)
	}

	// The zero-vector chunk is recorded as a per-document FAILURE with a reason,
	// exactly like a provider-rejected poison chunk (#399) — never as embedded.
	if len(src.failedLabels) != 1 || src.failedLabels[0] != 2 {
		t.Fatalf("failed labels = %v, want exactly [2]", src.failedLabels)
	}
	if src.failedCategory != "embedding_failure" {
		t.Fatalf("failed category = %q, want embedding_failure", src.failedCategory)
	}
	if !strings.Contains(src.failedReason, "zero") {
		t.Fatalf("failed reason = %q, want it to name the zero-norm vector", src.failedReason)
	}
	for _, l := range src.embedded {
		if l == 2 {
			t.Fatalf("the zero-vector chunk was marked embedded: %v", src.embedded)
		}
	}
	if len(src.embedded) != 2 {
		t.Fatalf("embedded = %v, want the 2 healthy chunks", src.embedded)
	}

	// And nothing zero-norm reached the index: every stored vector must have a
	// direction. A zero vector in the index is invisible to every query while
	// looking like healthy indexed data.
	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("index holds %d vectors, want 2 (the zero vector must not be stored)", len(hits))
	}
	for _, h := range hits {
		if h.ChunkID == 2 {
			t.Fatalf("the zero vector was indexed: %+v", h)
		}
	}
}
