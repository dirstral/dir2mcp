package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/latechunk"
	"github.com/dirstral/dir2mcp/internal/model"
)

// lcPlainEmbedder implements only model.Embedder (one pooled vector per input),
// like every shipped provider: it can never serve the late-chunking path.
type lcPlainEmbedder struct{}

func (lcPlainEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1}
	}
	return out, nil
}

// lcTokenEmbedder additionally implements model.TokenEmbedder, so it can serve
// the late-chunking path.
type lcTokenEmbedder struct{ lcPlainEmbedder }

func (lcTokenEmbedder) EmbedDocumentTokens(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([]model.TokenEmbedding, error) {
	out := make([]model.TokenEmbedding, len(inputs))
	for i := range inputs {
		out[i] = model.TokenEmbedding{Vectors: [][]float32{{1}}, Offsets: []int{0}, Ends: []int{1}}
	}
	return out, nil
}

// TestWorker_LateChunkDecision_DisabledByDefault asserts a worker with the
// feature off (the default) reports an inactive, disabled decision regardless of
// embedder capability — so behavior is unchanged unless explicitly enabled.
func TestWorker_LateChunkDecision_DisabledByDefault(t *testing.T) {
	w := &index.EmbeddingWorker{Embedder: lcTokenEmbedder{}}
	dec := w.LateChunkDecision()
	if dec.Active {
		t.Fatal("late chunking must be inactive by default")
	}
	if dec.Fallback != latechunk.FallbackDisabled {
		t.Fatalf("fallback = %q, want %q", dec.Fallback, latechunk.FallbackDisabled)
	}
}

// TestWorker_LateChunkDecision_FallsBackWithoutTokenEmbedder asserts that
// enabling the feature with a plain embedder (every shipped provider today)
// gracefully falls back to chunk-then-embed rather than activating.
func TestWorker_LateChunkDecision_FallsBackWithoutTokenEmbedder(t *testing.T) {
	w := &index.EmbeddingWorker{Embedder: lcPlainEmbedder{}, LateChunking: true}
	dec := w.LateChunkDecision()
	if dec.Active {
		t.Fatal("late chunking must fall back when the embedder lacks token embeddings")
	}
	if dec.Fallback != latechunk.FallbackNoTokenEmbedder {
		t.Fatalf("fallback = %q, want %q", dec.Fallback, latechunk.FallbackNoTokenEmbedder)
	}
}

// TestWorker_LateChunkDecision_ActiveWithTokenEmbedder asserts the path
// activates when enabled and the configured embedder exposes token embeddings —
// the seam a future self-hosted token-embedding backend plugs into.
func TestWorker_LateChunkDecision_ActiveWithTokenEmbedder(t *testing.T) {
	w := &index.EmbeddingWorker{Embedder: lcTokenEmbedder{}, LateChunking: true}
	dec := w.LateChunkDecision()
	if !dec.Active {
		t.Fatal("late chunking should activate with a token embedder when enabled")
	}
	if dec.Embedder == nil {
		t.Fatal("active decision must carry a non-nil token embedder")
	}
}
