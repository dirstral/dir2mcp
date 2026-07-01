package tests

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/pdfutil"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	pdfmodel "github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

// makeWorkerPDF renders an n-page PDF for the worker media-path test.
func makeWorkerPDF(t *testing.T, n int) []byte {
	t.Helper()
	api.DisableConfigDir()
	parts := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		parts = append(parts, fmt.Sprintf(`"%d":{"content":{"text":[{"value":"page","position":[100,700],"font":{"name":"Helvetica","size":12}}]}}`, i))
	}
	js := `{"pages":{` + strings.Join(parts, ",") + `}}`
	var buf bytes.Buffer
	if err := api.Create(nil, strings.NewReader(js), &buf, pdfmodel.NewDefaultConfiguration()); err != nil {
		t.Fatalf("create %d-page pdf: %v", n, err)
	}
	return buf.Bytes()
}

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

// TestEmbeddingWorker_RunOnce_PdfPageExtracted pins SPEC 8.1.7: a PDF media
// chunk is embedded as a single-page PDF — the worker extracts the page named
// by the chunk's page span before calling EmbedMedia.
func TestEmbeddingWorker_RunOnce_PdfPageExtracted(t *testing.T) {
	root := t.TempDir()
	pdf := makeWorkerPDF(t, 3)
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	tk := model.NewChunkTask(5, "", "text", model.ChunkMetadata{
		ChunkID: 5, RelPath: "doc.pdf", DocType: "pdf",
		Span: model.Span{Kind: "page", Page: 2},
	})
	tk.Modality = "pdf"
	tk.MediaRef = "doc.pdf"

	source := &fakeChunkSource{tasks: []model.ChunkTask{tk}}
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}}
	worker := &index.EmbeddingWorker{
		Source: source, Index: index.NewHNSWIndex(""), Embedder: emb,
		RootDir: root, BatchSize: 4, ModelForText: "gemini-embedding-2",
	}
	if _, err := worker.RunOnce(context.Background(), "text"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(emb.gotMedia) != 1 {
		t.Fatalf("EmbedMedia items = %d, want 1", len(emb.gotMedia))
	}
	if emb.gotMedia[0].MimeType != "application/pdf" {
		t.Errorf("mime = %q, want application/pdf", emb.gotMedia[0].MimeType)
	}
	// The embedded payload must be the single extracted page, not the whole doc.
	if n, err := pdfutil.PageCount(emb.gotMedia[0].Data); err != nil || n != 1 {
		t.Fatalf("embedded PDF page count = %d (err %v), want 1", n, err)
	}
}

func TestEmbeddingWorker_RunOnce_PdfWithoutValidPageSpanFails(t *testing.T) {
	root := t.TempDir()
	pdf := makeWorkerPDF(t, 2)
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), pdf, 0o600); err != nil {
		t.Fatal(err)
	}
	tk := model.NewChunkTask(6, "", "text", model.ChunkMetadata{
		ChunkID: 6, RelPath: "doc.pdf", DocType: "pdf",
		Span: model.Span{Kind: "", Page: 0},
	})
	tk.Modality = "pdf"
	tk.MediaRef = "doc.pdf"

	source := &fakeChunkSource{tasks: []model.ChunkTask{tk}}
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}}
	worker := &index.EmbeddingWorker{
		Source: source, Index: index.NewHNSWIndex(""), Embedder: emb,
		RootDir: root, BatchSize: 4, ModelForText: "gemini-embedding-2",
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err == nil {
		t.Fatal("expected error for invalid pdf page span")
	}
	if !errors.Is(err, index.ErrFatal) {
		t.Fatalf("expected fatal error, got %v", err)
	}
	if n != 0 {
		t.Fatalf("indexed = %d, want 0", n)
	}
	if len(source.failedLabels) != 1 || source.failedLabels[0] != 6 {
		t.Fatalf("failed labels = %#v, want [6]", source.failedLabels)
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
	// gotModel records the model name passed to the most recent Embed call so
	// tests can assert on modelForKind's resolution (issue #396).
	gotModel string
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

func (e *fakeEmbedder) Embed(_ context.Context, modelName string, _ model.EmbedRole, _ []string) ([][]float32, error) {
	e.gotModel = modelName
	if e.err != nil {
		return nil, e.err
	}
	return e.vectors, nil
}

// fakeUpsertIndex is a minimal model.Index whose Upsert returns a configurable
// error, so tests can exercise the index-error path (issue #412) without a real
// vector backend. All other methods are no-ops.
type fakeUpsertIndex struct {
	upsertErr error
}

func (f *fakeUpsertIndex) Upsert(_ context.Context, _ []float32, _ model.IndexPayload) error {
	return f.upsertErr
}
func (f *fakeUpsertIndex) Delete(_ context.Context, _ []uint64) error { return nil }
func (f *fakeUpsertIndex) Search(_ context.Context, _ []float32, _ int, _ model.Filter) ([]model.IndexHit, error) {
	return nil, nil
}
func (f *fakeUpsertIndex) Identity(_ context.Context) (string, error) { return "", nil }
func (f *fakeUpsertIndex) Reset(_ context.Context, _ string) error    { return nil }
func (f *fakeUpsertIndex) Close() error                               { return nil }

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

// TestEmbeddingWorker_RunOnce_ProjectsPayload pins issue #247: the worker
// Upserts each vector with an IndexPayload projected from the chunk's metadata
// (rel_path/doc_type/span/modality), so the index itself can serve filtered
// search. We assert by querying the index and inspecting the returned payload.
func TestEmbeddingWorker_RunOnce_ProjectsPayload(t *testing.T) {
	source := &fakeChunkSource{
		tasks: []model.ChunkTask{
			model.NewChunkTask(31, "spoken", "", model.ChunkMetadata{
				ChunkID: 31, RelPath: "audio/talk.mp3", DocType: "audio",
				Snippet: "spoken words",
				Span:    model.Span{Kind: "time", StartMS: 1000, EndMS: 5000},
			}),
		},
	}
	idx := index.NewHNSWIndex("")
	worker := &index.EmbeddingWorker{
		Source:       source,
		Index:        idx,
		Embedder:     &fakeEmbedder{vectors: [][]float32{{1, 0}}},
		BatchSize:    4,
		ModelForText: "mistral-embed",
	}

	if _, err := worker.RunOnce(context.Background(), "text"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// A doctype-filtered search proves the payload doc_type was stored, and the
	// returned payload proves the rest of the projection round-tripped.
	hits, err := idx.Search(context.Background(), []float32{1, 0}, 5, model.Filter{DocTypes: []string{"audio"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 31 {
		t.Fatalf("expected chunk 31 via doctype-filtered search, got %v", hits)
	}
	p := hits[0].Payload
	if p.RelPath != "audio/talk.mp3" || p.DocType != "audio" || p.Snippet != "spoken words" {
		t.Fatalf("payload not projected from metadata: %#v", p)
	}
	if p.Span.Kind != "time" || p.StartMS != 1000 || p.EndMS != 5000 {
		t.Fatalf("span/time bounds not projected: %#v", p)
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

// TestEmbeddingWorker_RunOnce_TransientEmbedErrorsLeavePending pins issue #412:
// the broadened transient classifier (connection refused / 503 / overloaded /
// EOF) must keep the chunk PENDING for retry rather than permanently failing it.
func TestEmbeddingWorker_RunOnce_TransientEmbedErrorsLeavePending(t *testing.T) {
	cases := []struct{ name, msg string }{
		{"connection-refused", "dial tcp 10.0.0.1:443: connect: connection refused"},
		{"http-503", "embedding request failed: 503 service unavailable"},
		{"overloaded", "upstream returned error: overloaded"},
		{"unexpected-eof", "post /v1/embeddings: unexpected EOF"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := &fakeChunkSource{
				tasks: []model.ChunkTask{model.NewChunkTask(70, "x", "", model.ChunkMetadata{ChunkID: 70})},
			}
			embErr := errors.New(tc.msg)
			worker := &index.EmbeddingWorker{
				Source: source, Index: index.NewHNSWIndex(""),
				Embedder: &fakeEmbedder{err: embErr}, BatchSize: 1,
			}
			n, err := worker.RunOnce(context.Background(), "text")
			if !errors.Is(err, embErr) {
				t.Fatalf("err = %v, want %v", err, embErr)
			}
			if n != 0 {
				t.Fatalf("indexed = %d, want 0", n)
			}
			if len(source.failedLabels) != 0 {
				t.Fatalf("transient %q must not mark failed, got %v", tc.msg, source.failedLabels)
			}
		})
	}

	// A wrapped io.EOF (structural match, where the string form may be lost)
	// is also transient.
	t.Run("wrapped-io-eof", func(t *testing.T) {
		source := &fakeChunkSource{
			tasks: []model.ChunkTask{model.NewChunkTask(71, "y", "", model.ChunkMetadata{ChunkID: 71})},
		}
		embErr := fmt.Errorf("embed batch failed: %w", io.EOF)
		worker := &index.EmbeddingWorker{
			Source: source, Index: index.NewHNSWIndex(""),
			Embedder: &fakeEmbedder{err: embErr}, BatchSize: 1,
		}
		if _, err := worker.RunOnce(context.Background(), "text"); !errors.Is(err, embErr) {
			t.Fatalf("err = %v, want %v", err, embErr)
		}
		if len(source.failedLabels) != 0 {
			t.Fatalf("wrapped EOF must not mark failed, got %v", source.failedLabels)
		}
	})
}

// TestEmbeddingWorker_RunOnce_TransientIndexErrorLeavesPending pins issue #412
// part 2: a transient vector-index Upsert error must NOT mark the chunk failed —
// it stays pending so the next cycle retries once the store recovers.
func TestEmbeddingWorker_RunOnce_TransientIndexErrorLeavesPending(t *testing.T) {
	source := &fakeChunkSource{
		tasks: []model.ChunkTask{model.NewChunkTask(80, "z", "", model.ChunkMetadata{ChunkID: 80})},
	}
	upErr := errors.New("upsert vector: 503 service unavailable")
	worker := &index.EmbeddingWorker{
		Source: source, Index: &fakeUpsertIndex{upsertErr: upErr},
		Embedder: &fakeEmbedder{vectors: [][]float32{{1, 0}}}, BatchSize: 1,
	}
	n, err := worker.RunOnce(context.Background(), "text")
	if !errors.Is(err, upErr) {
		t.Fatalf("err = %v, want %v", err, upErr)
	}
	if n != 0 {
		t.Fatalf("indexed = %d, want 0", n)
	}
	if len(source.failedLabels) != 0 {
		t.Fatalf("transient index error must leave chunk pending, got failed %v", source.failedLabels)
	}
	if len(source.embedded) != 0 {
		t.Fatalf("nothing should be marked embedded on a failed-only batch, got %v", source.embedded)
	}
}

// TestEmbeddingWorker_RunOnce_PermanentIndexErrorMarksFailed is the regression
// guard for the other side of the #412 guard: a NON-transient index error still
// marks the chunk failed (so genuinely broken chunks are not retried forever).
func TestEmbeddingWorker_RunOnce_PermanentIndexErrorMarksFailed(t *testing.T) {
	source := &fakeChunkSource{
		tasks: []model.ChunkTask{model.NewChunkTask(81, "z", "", model.ChunkMetadata{ChunkID: 81})},
	}
	upErr := errors.New("vector dimension mismatch")
	worker := &index.EmbeddingWorker{
		Source: source, Index: &fakeUpsertIndex{upsertErr: upErr},
		Embedder: &fakeEmbedder{vectors: [][]float32{{1, 0}}}, BatchSize: 1,
	}
	if _, err := worker.RunOnce(context.Background(), "text"); !errors.Is(err, upErr) {
		t.Fatalf("err = %v, want %v", err, upErr)
	}
	if len(source.failedLabels) != 1 || source.failedLabels[0] != 81 {
		t.Fatalf("permanent index error must mark chunk failed, got %v", source.failedLabels)
	}
}

// TestEmbeddingWorker_ModelForKind_EmptyDefersToAdapter pins issue #396: with no
// configured embed model, the worker passes "" to the embedder so the provider
// ADAPTER applies its own default — it must NOT fall back to a Mistral model id
// (which would fail an OpenAI/Cohere/Gemini corpus).
func TestEmbeddingWorker_ModelForKind_EmptyDefersToAdapter(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		source := &fakeChunkSource{
			tasks: []model.ChunkTask{model.NewChunkTask(90, "hi", "", model.ChunkMetadata{ChunkID: 90, DocType: "text"})},
		}
		emb := &fakeEmbedder{vectors: [][]float32{{1, 0}}} // ModelForText intentionally unset
		worker := &index.EmbeddingWorker{Source: source, Index: index.NewHNSWIndex(""), Embedder: emb, BatchSize: 1}
		if _, err := worker.RunOnce(context.Background(), "text"); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if emb.gotModel != "" {
			t.Fatalf("text embed model = %q, want empty (adapter default), not a mistral literal", emb.gotModel)
		}
	})
	t.Run("code", func(t *testing.T) {
		source := &fakeChunkSource{
			tasks: []model.ChunkTask{model.NewChunkTask(91, "hi", "", model.ChunkMetadata{ChunkID: 91, DocType: "code"})},
		}
		emb := &fakeEmbedder{vectors: [][]float32{{1, 0}}} // ModelForCode intentionally unset
		worker := &index.EmbeddingWorker{Source: source, Index: index.NewHNSWIndex(""), Embedder: emb, BatchSize: 1}
		if _, err := worker.RunOnce(context.Background(), "code"); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if emb.gotModel != "" {
			t.Fatalf("code embed model = %q, want empty (adapter default), not a codestral literal", emb.gotModel)
		}
	})
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
