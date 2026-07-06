package tests

import (
	"bytes"
	"context"
	"log"
	"strings"
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

// lcRecordingEmbedder implements both model.Embedder and model.TokenEmbedder and
// records which path the worker actually drove, so a test can prove the
// token-pooling path is deferred (issue #446): even when the capability gate is
// satisfied (dec.Active), the worker must still call Embed (chunk-then-embed)
// and must NOT call EmbedDocumentTokens.
type lcRecordingEmbedder struct {
	embedCalls int
	tokenCalls int
}

func (e *lcRecordingEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	e.embedCalls++
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1}
	}
	return out, nil
}

func (e *lcRecordingEmbedder) EmbedDocumentTokens(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([]model.TokenEmbedding, error) {
	e.tokenCalls++
	out := make([]model.TokenEmbedding, len(inputs))
	for i := range inputs {
		out[i] = model.TokenEmbedding{Vectors: [][]float32{{1}}, Offsets: []int{0}, Ends: []int{1}}
	}
	return out, nil
}

// TestWorker_LateChunkActive_StillEmbedsChunkThenEmbed_HonestLog pins issue #446
// F1: when late chunking is enabled AND the embedder exposes token embeddings
// (dec.Active), the production embed loop is NOT yet wired to the pooling path,
// so the worker must still embed chunk-then-embed (call Embed, never
// EmbedDocumentTokens) and the one-time decision log MUST be honest — it must
// NOT claim the path is "active" (the previous log lied) and must disclose that
// the pooling path is not yet wired.
func TestWorker_LateChunkActive_StillEmbedsChunkThenEmbed_HonestLog(t *testing.T) {
	emb := &lcRecordingEmbedder{}

	// Precondition: the decision is Active (enabled + token embedder present).
	if dec := (&index.EmbeddingWorker{Embedder: emb, LateChunking: true}).LateChunkDecision(); !dec.Active {
		t.Fatal("precondition: decision must be active with a token embedder + late chunking on")
	}

	src := &fakeChunkSource{tasks: []model.ChunkTask{
		model.NewChunkTask(1, "hello", "", model.ChunkMetadata{ChunkID: 1, RelPath: "a.txt", DocType: "text"}),
	}}
	var buf bytes.Buffer
	worker := &index.EmbeddingWorker{
		Source: src, Index: index.NewHNSWIndex(""), Embedder: emb,
		BatchSize: 4, LateChunking: true, Logger: log.New(&buf, "", 0),
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("indexed = %d, want 1", n)
	}

	// The pooling path is deferred (#446): the worker drives Embed, not the
	// token-embedding path.
	if emb.tokenCalls != 0 {
		t.Fatalf("token-pooling path must not run yet (deferred): tokenCalls=%d", emb.tokenCalls)
	}
	if emb.embedCalls == 0 {
		t.Fatal("worker must still embed chunk-then-embed via Embed")
	}

	// The one-time decision log must be honest about the deferred state.
	logged := buf.String()
	if strings.Contains(logged, "enabled and active") {
		t.Fatalf("log must not claim late chunking is active while the pooling path is deferred: %q", logged)
	}
	if !strings.Contains(logged, "not yet wired") {
		t.Fatalf("log must disclose the pooling path is not yet wired: %q", logged)
	}
}
