package tests

import (
	"context"
	"errors"
	"fmt"
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

// fatalText marks an input that embeds fatally when isolated (a local
// media/config failure, index.ErrFatal) — as opposed to poisonText, which is a
// provider input rejection. A multi-item batch containing it fails
// non-transiently but non-fatally so the worker bisects; only the isolated
// single input surfaces ErrFatal.
const fatalText = "FATAL-local-config"

type fatalOnIsolateEmbedder struct{ calls int }

func (e *fatalOnIsolateEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, inputs []string) ([][]float32, error) {
	e.calls++
	hasFatal := false
	for _, in := range inputs {
		if strings.Contains(in, fatalText) {
			hasFatal = true
		}
	}
	if hasFatal {
		if len(inputs) == 1 {
			// Isolated fatal input: local media/config failure must stay loud.
			return nil, fmt.Errorf("%w: local media/config failure", index.ErrFatal)
		}
		// Multi-item batch containing it: non-transient, non-fatal → bisect.
		return nil, errors.New("400 bad request")
	}
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// TestBisect_FatalErrorPropagates pins the #412 contract through the #399
// bisection path: an ErrFatal surfacing in a bisected sub-batch must propagate
// (so the run loop stops) rather than be swallowed as batch success.
func TestBisect_FatalErrorPropagates(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := index.NewHNSWIndex("")
	emb := &fatalOnIsolateEmbedder{}
	worker := &index.EmbeddingWorker{Source: src, Index: idx, Embedder: emb, BatchSize: 8}

	tasks := []model.ChunkTask{
		textTask(1, "healthy one"),
		textTask(2, fatalText),
	}
	_, err := worker.EmbedAndIndex(ctx, "text", tasks)
	if !errors.Is(err, index.ErrFatal) {
		t.Fatalf("err = %v, want ErrFatal to propagate out of bisection", err)
	}
}

// authEmbedder always rejects with a 401 (a corpus-wide auth failure) and counts
// its calls so a test can assert bisection did NOT fan out into O(n) calls.
type authEmbedder struct{ calls int }

func (e *authEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, _ []string) ([][]float32, error) {
	e.calls++
	return nil, errors.New("401 unauthorized: invalid api key")
}

// TestAuthError_NotBisected pins that a corpus-wide auth rejection is not
// bisected (that would explode one batch into O(n) embed calls without isolating
// anything). It marks the whole batch failed in a single embed call.
func TestAuthError_NotBisected(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	idx := index.NewHNSWIndex("")
	emb := &authEmbedder{}
	worker := &index.EmbeddingWorker{Source: src, Index: idx, Embedder: emb, BatchSize: 8}

	tasks := []model.ChunkTask{
		textTask(1, "a"), textTask(2, "b"), textTask(3, "c"), textTask(4, "d"),
	}
	if _, err := worker.EmbedAndIndex(ctx, "text", tasks); err == nil {
		t.Fatal("auth failure should surface an error, got nil")
	}
	if emb.calls != 1 {
		t.Fatalf("embed calls = %d, want 1 (auth must not bisect into O(n) calls)", emb.calls)
	}
	if len(src.failedLabels) != 4 {
		t.Fatalf("failed labels = %v, want all 4 marked failed (whole-batch behavior)", src.failedLabels)
	}
}

// TestBisect_PostEmbedWriteErrorPropagates pins the greptile P1 finding: a
// non-transient index/upsert (post-embed write) failure in a bisected sub-batch
// must propagate so the run loop backs off — otherwise the chunks stay PENDING
// while the batch reports success, causing a tight re-embed loop.
func TestBisect_PostEmbedWriteErrorPropagates(t *testing.T) {
	ctx := context.Background()
	src := &fakeChunkSource{}
	writeErr := errors.New("vector dimension mismatch")
	emb := &poisonEmbedder{}
	worker := &index.EmbeddingWorker{
		Source: src, Index: &fakeUpsertIndex{upsertErr: writeErr}, Embedder: emb, BatchSize: 8,
	}

	tasks := []model.ChunkTask{
		textTask(1, "healthy one"), // will embed then fail the index write
		textTask(2, poisonText),    // isolated + marked failed by bisection
	}
	_, err := worker.EmbedAndIndex(ctx, "text", tasks)
	if !errors.Is(err, writeErr) {
		t.Fatalf("err = %v, want the post-embed write error to propagate out of bisection", err)
	}
}
