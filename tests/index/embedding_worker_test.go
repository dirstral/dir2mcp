package tests

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// fakeMultimodalEmbedder implements model.MultimodalEmbedder for the media path.
type fakeMultimodalEmbedder struct {
	gotMedia  []model.MediaInput
	mediaVecs [][]float32
	textVecs  [][]float32
}

func (e *fakeMultimodalEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, _ []string) ([][]float32, error) {
	return e.textVecs, nil
}

func (e *fakeMultimodalEmbedder) EmbedMedia(_ context.Context, _ string, _ model.EmbedRole, items []model.MediaInput) ([][]float32, error) {
	e.gotMedia = append(e.gotMedia, items...)
	return e.mediaVecs, nil
}

func mediaTask(label uint64, relPath, modality string) model.ChunkTask {
	tk := model.NewChunkTask(label, "", "text", model.ChunkMetadata{ChunkID: label, RelPath: relPath, DocType: modality})
	tk.Modality = modality
	tk.MediaRef = relPath
	return tk
}

// TestEmbeddingWorker_RunOnce_MediaChunk pins SPEC 8.1.7: a media chunk is
// embedded via EmbedMedia from bytes read at RootDir/MediaRef, with the MIME
// inferred from the extension, and its vector is indexed.
func TestEmbeddingWorker_RunOnce_MediaChunk(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pic.png"), []byte("PNGDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := &fakeChunkSource{tasks: []model.ChunkTask{mediaTask(7, "pic.png", "image")}}
	idx := index.NewHNSWIndex("")
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{0.6, 0.8}}}
	worker := &index.EmbeddingWorker{
		Source: source, Index: idx, Embedder: emb,
		RootDir: root, BatchSize: 4, ModelForText: "gemini-embedding-2",
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("indexed = %d, want 1", n)
	}
	if len(emb.gotMedia) != 1 || string(emb.gotMedia[0].Data) != "PNGDATA" {
		t.Fatalf("media bytes not passed to EmbedMedia: %+v", emb.gotMedia)
	}
	if emb.gotMedia[0].MimeType != "image/png" {
		t.Fatalf("mime = %q, want image/png", emb.gotMedia[0].MimeType)
	}
	if len(source.embedded) != 1 || source.embedded[0] != 7 {
		t.Fatalf("embedded labels = %v, want [7]", source.embedded)
	}
}

// TestEmbeddingWorker_MediaChunk_NonMultimodalEmbedderFails: a media chunk with
// a text-only embedder is a fatal config error (validation should prevent it
// upstream, but the worker must not silently mis-embed).
func TestEmbeddingWorker_MediaChunk_NonMultimodalEmbedderFails(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pic.png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := &fakeChunkSource{tasks: []model.ChunkTask{mediaTask(9, "pic.png", "image")}}
	worker := &index.EmbeddingWorker{
		Source: source, Index: index.NewHNSWIndex(""), Embedder: &fakeEmbedder{},
		RootDir: root, BatchSize: 4,
	}
	if _, err := worker.RunOnce(context.Background(), "text"); err == nil {
		t.Fatal("media chunk with a text-only embedder must error")
	}
}

type fakeChunkSource struct {
	tasks          []model.ChunkTask
	embedded       []uint64
	failedLabels   []uint64
	failedReason   string
	failedCategory string
	// markFailedErr, if non-nil, is returned from MarkFailed.
	markFailedErr error
}

func (s *fakeChunkSource) NextPending(_ context.Context, _ int, _ string) ([]model.ChunkTask, error) {
	out := s.tasks
	s.tasks = nil
	return out, nil
}

func (s *fakeChunkSource) MarkEmbedded(_ context.Context, labels []uint64) error {
	s.embedded = append(s.embedded, labels...)
	return nil
}

func (s *fakeChunkSource) MarkFailed(_ context.Context, labels []uint64, reason string) error {
	s.failedLabels = append(s.failedLabels, labels...)
	s.failedReason = reason
	return s.markFailedErr
}

// MarkFailedWithCategory mirrors MarkFailed and additionally records
// the supplied classification for tests that assert on grouping.
func (s *fakeChunkSource) MarkFailedWithCategory(_ context.Context, labels []uint64, category, reason string) error {
	s.failedLabels = append(s.failedLabels, labels...)
	s.failedReason = reason
	s.failedCategory = category
	return s.markFailedErr
}

type fakeEmbedder struct {
	vectors [][]float32
	err     error
}

// testWorker provides a fake RunOnce sequence; it also implements the
// retrying Run loop derived from EmbeddingWorker.Run so tests can exercise
// backoff behaviour without depending on the real worker's internal state.
//
// testWorker implements a custom RunOnce that returns a sequence of
// errors; it is used with an EmbeddingWorker through the RunOnceFunc
// hook so we can exercise the run loop without needing a full source
// or embedder.
type testWorker struct {
	calls int
	errs  []error
}

func (w *testWorker) RunOnce(ctx context.Context, indexKind string) (int, error) {
	w.calls++
	if w.calls <= len(w.errs) {
		return 0, w.errs[w.calls-1]
	}
	return 1, nil
}

func (e *fakeEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, _ []string) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.vectors, nil
}

func TestEmbeddingWorker_RunOnce_Success(t *testing.T) {
	source := &fakeChunkSource{
		tasks: []model.ChunkTask{
			model.NewChunkTask(11, "alpha", "", model.ChunkMetadata{ChunkID: 11, RelPath: "a.txt", DocType: "text"}),
			model.NewChunkTask(22, "beta", "", model.ChunkMetadata{ChunkID: 22, RelPath: "b.go", DocType: "code"}),
		},
	}

	idx := index.NewHNSWIndex("")
	embedder := &fakeEmbedder{
		vectors: [][]float32{
			{1, 0},
			{0, 1},
		},
	}

	indexed := make(map[uint64]model.ChunkMetadata)
	worker := &index.EmbeddingWorker{
		Source:       source,
		Index:        idx,
		Embedder:     embedder,
		BatchSize:    2,
		ModelForText: "mistral-embed",
		OnIndexedChunk: func(label uint64, metadata model.ChunkMetadata) {
			indexed[label] = metadata
		},
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}
	if n != 2 {
		t.Fatalf("unexpected indexed count: %d", n)
	}
	if len(source.embedded) != 2 {
		t.Fatalf("expected 2 embedded labels, got %d", len(source.embedded))
	}
	if indexed[11].RelPath != "a.txt" || indexed[22].RelPath != "b.go" {
		t.Fatalf("metadata callback mismatch: %#v", indexed)
	}
}

func TestEmbeddingWorker_RunOnce_EmbeddingFailure(t *testing.T) {
	source := &fakeChunkSource{
		tasks: []model.ChunkTask{
			model.NewChunkTask(99, "fail", "", model.ChunkMetadata{}),
		},
	}

	worker := &index.EmbeddingWorker{
		Source:    source,
		Index:     index.NewHNSWIndex(""),
		Embedder:  &fakeEmbedder{err: errors.New("upstream failed")},
		BatchSize: 1, // explicitly ensure batching occurs
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err == nil {
		t.Fatal("expected error")
	}
	if n != 0 {
		t.Fatalf("unexpected indexed count: %d", n)
	}
	if len(source.failedLabels) != 1 || source.failedLabels[0] != 99 {
		t.Fatalf("expected failed label 99, got %#v", source.failedLabels)
	}
	if source.failedReason != "upstream failed" {
		t.Fatalf("expected failedReason 'upstream failed', got %q", source.failedReason)
	}
}

// transient error cases should leave chunks pending rather than marking them
// failed; RunOnce still returns the underlying error so the run loop can
// apply its retry/backoff policy.
func TestEmbeddingWorker_RunOnce_EmbeddingTransient(t *testing.T) {
	// split into two independent subtests to avoid shared source state
	// causing confusing failures or hidden dependencies.

	t.Run("rate-limit", func(t *testing.T) {
		source := &fakeChunkSource{
			tasks: []model.ChunkTask{model.NewChunkTask(42, "maybe", "", model.ChunkMetadata{ChunkID: 42})},
		}

		// a simple rate-limit style message should be treated as transient
		rateErr := errors.New("rate limit exceeded")
		worker := &index.EmbeddingWorker{
			Source:    source,
			Index:     index.NewHNSWIndex(""),
			Embedder:  &fakeEmbedder{err: rateErr},
			BatchSize: 1,
		}
		n, err := worker.RunOnce(context.Background(), "text")
		if err != rateErr {
			t.Fatalf("expected same error back, got %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 indexed tasks, got %d", n)
		}
		if len(source.failedLabels) != 0 {
			t.Fatalf("transient error should not mark failed, got %v", source.failedLabels)
		}
	})

	t.Run("net-temporary", func(t *testing.T) {
		// net.Error with Temporary() true (without timeout) is also transient.
		source := &fakeChunkSource{
			tasks: []model.ChunkTask{model.NewChunkTask(43, "again", "", model.ChunkMetadata{ChunkID: 43})},
		}
		tmpErr := &net.DNSError{IsTemporary: true}
		worker := &index.EmbeddingWorker{
			Source:    source,
			Index:     index.NewHNSWIndex(""),
			Embedder:  &fakeEmbedder{err: tmpErr},
			BatchSize: 1,
		}
		n, err := worker.RunOnce(context.Background(), "text")
		if err != tmpErr {
			t.Fatalf("expected temp error back, got %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 indexed tasks, got %d", n)
		}
		if len(source.failedLabels) != 0 {
			t.Fatalf("net temporary error should not mark failed, got %v", source.failedLabels)
		}
	})
}

// panicEmbedder is used to ensure that Embed is never called when a
// zero/invalid label is detected before embedding begins; zeros are treated as
// corrupt by the worker.
type panicEmbedder struct{}

func (p *panicEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, _ []string) ([][]float32, error) {
	panic("embedder should not be invoked for zero-label batches")
}

func TestEmbeddingWorker_RunOnce_NegativeLabel(t *testing.T) {
	t.Run("single-negative", func(t *testing.T) {
		// single zero/invalid label
		source := &fakeChunkSource{
			tasks: []model.ChunkTask{model.NewChunkTask(0, "oops", "", model.ChunkMetadata{})},
		}

		worker := &index.EmbeddingWorker{
			Source:    source,
			Index:     index.NewHNSWIndex(""),
			Embedder:  &panicEmbedder{},
			BatchSize: 1,
		}

		n, err := worker.RunOnce(context.Background(), "text")
		if err == nil {
			t.Fatal("expected error for negative label")
		}
		if !errors.Is(err, index.ErrFatal) {
			t.Fatalf("expected fatal error, got %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 tasks processed, got %d", n)
		}
		// MarkFailed must NOT be called for negative labels.
		if len(source.failedLabels) != 0 {
			t.Fatalf("expected no MarkFailed call for negative label, got %#v", source.failedLabels)
		}
		if source.failedReason != "" {
			t.Fatalf("expected no failure reason, got %q", source.failedReason)
		}
	})

	t.Run("mixed-batch", func(t *testing.T) {
		// mix of positive then zero; embedder still must not be called and
		// MarkFailed must not be called with the corrupt ID.
		source := &fakeChunkSource{
			tasks: []model.ChunkTask{
				model.NewChunkTask(10, "ok", "", model.ChunkMetadata{}),
				model.NewChunkTask(0, "bad", "", model.ChunkMetadata{}),
			},
		}

		worker := &index.EmbeddingWorker{
			Source:    source,
			Index:     index.NewHNSWIndex(""),
			Embedder:  &panicEmbedder{},
			BatchSize: 1,
		}

		n, err := worker.RunOnce(context.Background(), "text")
		if err == nil {
			t.Fatal("expected error for negative label in mixed batch")
		}
		if !errors.Is(err, index.ErrFatal) {
			t.Fatalf("expected fatal error for mixed batch, got %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 tasks processed on mixed batch, got %d", n)
		}
		if len(source.failedLabels) != 0 {
			t.Fatalf("expected no MarkFailed call for negative label in mixed batch, got %#v", source.failedLabels)
		}
		if source.failedReason != "" {
			t.Fatalf("expected no failure reason on mixed batch, got %q", source.failedReason)
		}
	})
}

func TestEmbeddingWorker_Run_RetryableErrors(t *testing.T) {
	// first two invocations return retryable errors; Run should keep looping
	// until the context expires and we should see at least three calls.
	tw := &testWorker{errs: []error{errors.New("transient1"), errors.New("transient2")}}
	ew := &index.EmbeddingWorker{RunOnceFunc: tw.RunOnce}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := ew.Run(ctx, 1*time.Millisecond, "text")
	if err == nil || (!errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled)) {
		t.Fatalf("expected context error, got %v", err)
	}
	if tw.calls < 3 {
		t.Fatalf("expected at least 3 RunOnce calls, got %d", tw.calls)
	}
}

func TestEmbeddingWorker_RunOnce_MarkFailedLogging(t *testing.T) {
	source := &fakeChunkSource{
		tasks:         []model.ChunkTask{model.NewChunkTask(123, "err", "", model.ChunkMetadata{})},
		markFailedErr: errors.New("db down"),
	}

	// embedder returns error to trigger MarkFailed
	embErr := errors.New("embed fail")
	worker := &index.EmbeddingWorker{
		Source:    source,
		Index:     index.NewHNSWIndex(""),
		Embedder:  &fakeEmbedder{err: embErr},
		BatchSize: 1,
	}

	var buf bytes.Buffer
	worker.Logger = log.New(&buf, "", 0)

	n, err := worker.RunOnce(context.Background(), "text")
	if !errors.Is(err, embErr) {
		t.Fatalf("expected embed error returned, got %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 tasks processed, got %d", n)
	}

	logged := buf.String()
	if !strings.Contains(logged, "mark failed update error") || !strings.Contains(logged, "db down") {
		t.Fatalf("expected log message about mark failed error, got %q", logged)
	}
}

func TestEmbeddingWorker_Run_FatalErrorStops(t *testing.T) {
	fatal := index.ErrFatal
	tw := &testWorker{errs: []error{fatal}}
	ew := &index.EmbeddingWorker{RunOnceFunc: tw.RunOnce, ErrCh: make(chan error, 1)}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err := ew.Run(ctx, 1*time.Millisecond, "text")
	if !errors.Is(err, fatal) {
		t.Fatalf("expected fatal error returned, got %v", err)
	}
	select {
	case e := <-ew.ErrCh:
		if !errors.Is(e, fatal) {
			t.Fatalf("expected fatal on errCh, got %v", e)
		}
	default:
		t.Fatal("expected error in errCh")
	}
}
