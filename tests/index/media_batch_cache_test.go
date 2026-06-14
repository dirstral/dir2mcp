package tests

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// countingCorpusFS is a fake corpusfs.CorpusFS that records how many times Open
// and Localize are invoked per relPath so a per-batch cache (issue #279) can be
// asserted to collapse sibling-chunk fetches of the same MediaRef into one.
type countingCorpusFS struct {
	mu          sync.Mutex
	contents    map[string][]byte
	openCalls   map[string]int
	localCalls  map[string]int
	liveTemps   map[string]bool // temp paths still present (cleanup not yet run)
	openErr     error
	localizeErr error
}

func newCountingCorpusFS(contents map[string][]byte) *countingCorpusFS {
	return &countingCorpusFS{
		contents:   contents,
		openCalls:  map[string]int{},
		localCalls: map[string]int{},
		liveTemps:  map[string]bool{},
	}
}

func (f *countingCorpusFS) Walk(context.Context, string, corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return nil, errors.New("not used")
}

func (f *countingCorpusFS) Open(_ context.Context, relPath string) (io.ReadSeekCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls[relPath]++
	if f.openErr != nil {
		return nil, f.openErr
	}
	data, ok := f.contents[relPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return nopSeekCloser{bytes.NewReader(data)}, nil
}

func (f *countingCorpusFS) Localize(_ context.Context, relPath string) (string, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.localCalls[relPath]++
	if f.localizeErr != nil {
		return "", nil, f.localizeErr
	}
	data, ok := f.contents[relPath]
	if !ok {
		return "", nil, os.ErrNotExist
	}
	// Materialize a temp file (mimicking an S3 download) preserving the
	// extension so muxer inference works, and track it so the test can assert
	// the cleanup func removed it at batch end.
	dir, err := os.MkdirTemp("", "media-batch-cache")
	if err != nil {
		return "", nil, err
	}
	tmp := filepath.Join(dir, filepath.Base(relPath))
	if werr := os.WriteFile(tmp, data, 0o600); werr != nil {
		return "", nil, werr
	}
	f.liveTemps[tmp] = true
	cleanup := func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = os.RemoveAll(dir)
		delete(f.liveTemps, tmp)
	}
	return tmp, cleanup, nil
}

func (f *countingCorpusFS) opens(relPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openCalls[relPath]
}

func (f *countingCorpusFS) localizes(relPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.localCalls[relPath]
}

func (f *countingCorpusFS) liveTempCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.liveTemps)
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

// TestEmbeddingWorker_BatchCache_PDFWholeFileSharedAcrossPages pins issue #279:
// three PDF chunks that are sibling pages of the SAME MediaRef must trigger
// exactly one whole-file Open for that ref within a batch, not one per page.
func TestEmbeddingWorker_BatchCache_PDFWholeFileSharedAcrossPages(t *testing.T) {
	pdf := makeWorkerPDF(t, 3)
	fsys := newCountingCorpusFS(map[string][]byte{"doc.pdf": pdf})

	tasks := []model.ChunkTask{
		avTask(1, "doc.pdf", "pdf", model.Span{Kind: "page", Page: 1}),
		avTask(2, "doc.pdf", "pdf", model.Span{Kind: "page", Page: 2}),
		avTask(3, "doc.pdf", "pdf", model.Span{Kind: "page", Page: 3}),
	}
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}, {0, 1}, {1, 1}}}
	worker := &index.EmbeddingWorker{
		Source: &fakeChunkSource{tasks: tasks}, Index: index.NewHNSWIndex(""), Embedder: emb,
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 8, ModelForText: "gemini-embedding-2",
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("indexed = %d, want 3", n)
	}
	if got := fsys.opens("doc.pdf"); got != 1 {
		t.Fatalf("Open(doc.pdf) called %d times, want 1 (per-batch cache should share the whole-file read)", got)
	}
	if len(emb.gotMedia) != 3 {
		t.Fatalf("EmbedMedia got %d items, want 3", len(emb.gotMedia))
	}
}

// TestEmbeddingWorker_BatchCache_VideoLocalizeSharedAcrossWindows pins issue
// #279 for the Localize path: three video chunks that are different time-windows
// of the SAME MediaRef must trigger exactly one Localize (one download), and the
// materialized temp file must be cleaned up after the batch completes.
func TestEmbeddingWorker_BatchCache_VideoLocalizeSharedAcrossWindows(t *testing.T) {
	fsys := newCountingCorpusFS(map[string][]byte{"clip.mp4": []byte("FULLVIDEO")})

	tasks := []model.ChunkTask{
		avTask(1, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 0, EndMS: 1000}),
		avTask(2, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 1000, EndMS: 2000}),
		avTask(3, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 2000, EndMS: 3000}),
	}
	var extractPaths []string
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}, {0, 1}, {1, 1}}}
	worker := &index.EmbeddingWorker{
		Source: &fakeChunkSource{tasks: tasks}, Index: index.NewHNSWIndex(""), Embedder: emb,
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 8, ModelForText: "gemini-embedding-2",
		ExtractSegmentFunc: func(_ context.Context, path string, _, _ int) ([]byte, error) {
			extractPaths = append(extractPaths, path)
			return []byte("SEG"), nil
		},
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("indexed = %d, want 3", n)
	}
	if got := fsys.localizes("clip.mp4"); got != 1 {
		t.Fatalf("Localize(clip.mp4) called %d times, want 1 (per-batch cache should share the download)", got)
	}
	if len(extractPaths) != 3 {
		t.Fatalf("extractor called %d times, want 3", len(extractPaths))
	}
	// All three windows must have been cut from the one materialized path.
	for i, p := range extractPaths {
		if p != extractPaths[0] {
			t.Fatalf("window %d extracted from %q, want shared path %q", i, p, extractPaths[0])
		}
	}
	if got := fsys.liveTempCount(); got != 0 {
		t.Fatalf("%d Localize temp files survived the batch, want 0 (cleanup must run at batch end)", got)
	}
}

// TestEmbeddingWorker_BatchCache_DifferentRefsFetchIndependently confirms the
// cache keys on MediaRef: distinct refs in one batch each fetch once.
func TestEmbeddingWorker_BatchCache_DifferentRefsFetchIndependently(t *testing.T) {
	fsys := newCountingCorpusFS(map[string][]byte{
		"a.png": []byte("AAA"),
		"b.png": []byte("BBB"),
	})
	tasks := []model.ChunkTask{
		mediaTask(1, "a.png", "image"),
		mediaTask(2, "a.png", "image"),
		mediaTask(3, "b.png", "image"),
	}
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}, {0, 1}, {1, 1}}}
	worker := &index.EmbeddingWorker{
		Source: &fakeChunkSource{tasks: tasks}, Index: index.NewHNSWIndex(""), Embedder: emb,
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 8, ModelForText: "gemini-embedding-2",
	}
	if _, err := worker.RunOnce(context.Background(), "text"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got := fsys.opens("a.png"); got != 1 {
		t.Fatalf("Open(a.png) = %d, want 1 (two sibling chunks share one read)", got)
	}
	if got := fsys.opens("b.png"); got != 1 {
		t.Fatalf("Open(b.png) = %d, want 1", got)
	}
}

// TestEmbeddingWorker_BatchCache_EscapeStillFatal confirms the cache does not
// disturb fatal classification: a ref that escapes the corpus root is still a
// permanent (ErrFatal) failure. Exercised against the real LocalFS containment
// check (the cache wraps it without altering the error).
func TestEmbeddingWorker_BatchCache_EscapeStillFatal(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.png"), []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	escapeRef := "../" + filepath.Base(outside) + "/secret.png"

	source := &fakeChunkSource{tasks: []model.ChunkTask{mediaTask(1, escapeRef, "image")}}
	worker := &index.EmbeddingWorker{
		Source: source, Index: index.NewHNSWIndex(""), Embedder: &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}},
		RootDir: root, BatchSize: 4, ModelForText: "gemini-embedding-2",
	}
	n, err := worker.RunOnce(context.Background(), "text")
	if !errors.Is(err, index.ErrFatal) {
		t.Fatalf("expected fatal error on escaping media_ref, got %v", err)
	}
	if n != 0 {
		t.Fatalf("indexed = %d, want 0", n)
	}
	if len(source.failedLabels) != 1 || source.failedLabels[0] != 1 {
		t.Fatalf("failed labels = %#v, want [1]", source.failedLabels)
	}
}
