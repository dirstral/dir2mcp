package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// poisonText is the sentinel input that the poisonEmbedder rejects. It models a
// provider-side 400 (input over the model's max context / bad encoding) which
// fails the whole HTTP batch — openai/gemini/omniembed all embed by batch.
const poisonText = "POISON-oversized-input"

// poisonEmbedder returns a non-transient error for any batch that contains
// poisonText (mirroring a provider that 400s the entire request on one bad
// input) and deterministic vectors otherwise. It records the number of Embed
// calls so a test can confirm the worker bisected rather than one-shotting.
type poisonEmbedder struct {
	calls int
}

func (e *poisonEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	e.calls++
	for _, in := range inputs {
		if strings.Contains(in, poisonText) {
			// Non-transient, non-fatal: the poison-chunk case from issue #399.
			return nil, errors.New("400 bad request: input exceeds maximum context length")
		}
	}
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func textTask(id uint64, text string) model.ChunkTask {
	return model.NewChunkTask(id, text, "text", model.ChunkMetadata{ChunkID: id, RelPath: "doc.txt"})
}

// TestEmbedAndIndex_PoisonChunkIsolated pins issue #399 item 1: a single
// provider-rejected "poison" input in a batch must not fail its healthy
// siblings. The worker bisects the failing batch, isolates the bad chunk (marks
// it embedding_status=error), and still embeds+indexes every other chunk.
func TestEmbedAndIndex_PoisonChunkIsolated(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := index.NewHNSWIndex("")
	emb := &poisonEmbedder{}
	worker := &index.EmbeddingWorker{Source: src, Index: idx, Embedder: emb, BatchSize: 8}

	tasks := []model.ChunkTask{
		textTask(1, "healthy one"),
		textTask(2, "healthy two"),
		textTask(3, poisonText), // the poison chunk
		textTask(4, "healthy four"),
	}

	n, err := worker.EmbedAndIndex(ctx, "text", tasks)
	if err != nil {
		t.Fatalf("EmbedAndIndex returned error after isolating poison chunk: %v", err)
	}
	if n != 3 {
		t.Fatalf("indexed = %d, want 3 (all healthy siblings)", n)
	}

	wantEmbedded := map[uint64]bool{1: true, 2: true, 4: true}
	if len(src.embedded) != 3 {
		t.Fatalf("embedded labels = %v, want the 3 healthy chunks", src.embedded)
	}
	for _, l := range src.embedded {
		if !wantEmbedded[l] {
			t.Fatalf("embedded contains unexpected label %d (poison must not be embedded): %v", l, src.embedded)
		}
	}

	if len(src.failedLabels) != 1 || src.failedLabels[0] != 3 {
		t.Fatalf("failed labels = %v, want exactly [3] (only the poison chunk)", src.failedLabels)
	}
	if src.failedCategory == "" {
		t.Fatalf("poison chunk should be marked failed with a classification category, got empty")
	}
	if emb.calls < 2 {
		t.Fatalf("embed calls = %d, want >=2 (the worker must bisect, not one-shot)", emb.calls)
	}
}

// TestEmbedAndIndex_AllPoisonMarksEachFailed pins that a batch where every input
// is rejected marks each chunk failed individually (no healthy siblings lost,
// none left silently pending) and reports no run-loop error to avoid needless
// backoff once every chunk has reached a terminal state.
func TestEmbedAndIndex_AllPoisonMarksEachFailed(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := index.NewHNSWIndex("")
	worker := &index.EmbeddingWorker{Source: src, Index: idx, Embedder: &poisonEmbedder{}, BatchSize: 8}

	tasks := []model.ChunkTask{
		textTask(10, poisonText+"-a"),
		textTask(11, poisonText+"-b"),
	}
	n, err := worker.EmbedAndIndex(ctx, "text", tasks)
	if err != nil {
		t.Fatalf("EmbedAndIndex error = %v, want nil (all chunks terminally marked failed)", err)
	}
	if n != 0 {
		t.Fatalf("indexed = %d, want 0", n)
	}
	if len(src.failedLabels) != 2 {
		t.Fatalf("failed labels = %v, want both chunks marked failed", src.failedLabels)
	}
	if len(src.embedded) != 0 {
		t.Fatalf("embedded = %v, want none", src.embedded)
	}
}

// transientBatchEmbedder always fails with a transient error, so no chunk should
// ever be marked failed (issue #412 contract, preserved by the bisection path).
type transientBatchEmbedder struct{}

func (transientBatchEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, _ []string) ([][]float32, error) {
	return nil, errors.New("503 service unavailable")
}

// TestEmbedAndIndex_TransientBatchLeavesAllPending pins that the poison-isolation
// path does not touch transient errors: a transient batch failure leaves every
// chunk PENDING (no MarkFailed) and surfaces the error so the run loop retries.
func TestEmbedAndIndex_TransientBatchLeavesAllPending(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := index.NewHNSWIndex("")
	worker := &index.EmbeddingWorker{Source: src, Index: idx, Embedder: transientBatchEmbedder{}, BatchSize: 8}

	tasks := []model.ChunkTask{textTask(20, "a"), textTask(21, "b"), textTask(22, "c")}
	n, err := worker.EmbedAndIndex(ctx, "text", tasks)
	if err == nil {
		t.Fatal("expected a transient error to be surfaced for retry")
	}
	if n != 0 {
		t.Fatalf("indexed = %d, want 0", n)
	}
	if len(src.failedLabels) != 0 {
		t.Fatalf("failed labels = %v, want none (transient errors must not mark chunks failed)", src.failedLabels)
	}
	if len(src.embedded) != 0 {
		t.Fatalf("embedded = %v, want none", src.embedded)
	}
}
