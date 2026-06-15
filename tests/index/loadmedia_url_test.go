package tests

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// urlCorpusFS is a fake CorpusFS that ALSO implements corpusfs.MediaURLProvider,
// modeling an S3 backend that can presign a range-seekable URL. It records
// MediaURL/Localize/Open call counts per relPath so a test can assert that
// audio/video segment extraction prefers the URL (range-read) path and never
// downloads the whole object via Localize (issue #243), and that the URL is
// presigned exactly once per MediaRef within a batch (issue #279 cache).
type urlCorpusFS struct {
	mu          sync.Mutex
	contents    map[string][]byte
	urlCalls    map[string]int
	localCalls  map[string]int
	openCalls   map[string]int
	urlPrefix   string
	mediaURLErr error
	noURL       bool // when true, MediaURL reports ok=false (force the fallback)
}

func newURLCorpusFS(contents map[string][]byte) *urlCorpusFS {
	return &urlCorpusFS{
		contents:   contents,
		urlCalls:   map[string]int{},
		localCalls: map[string]int{},
		openCalls:  map[string]int{},
		urlPrefix:  "https://example-bucket.s3.amazonaws.com/",
	}
}

func (f *urlCorpusFS) Walk(context.Context, string, corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return nil, errors.New("not used")
}

func (f *urlCorpusFS) Open(_ context.Context, relPath string) (io.ReadSeekCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls[relPath]++
	data, ok := f.contents[relPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return nopSeekCloser{bytes.NewReader(data)}, nil
}

func (f *urlCorpusFS) Localize(_ context.Context, relPath string) (string, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.localCalls[relPath]++
	// A whole-object download would happen here on real S3 — the URL path must
	// avoid reaching this for audio/video, so returning an error makes any
	// accidental fallback loud.
	return "", nil, errors.New("urlCorpusFS.Localize must not be used when MediaURL is available")
}

func (f *urlCorpusFS) MediaURL(_ context.Context, relPath string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urlCalls[relPath]++
	if f.mediaURLErr != nil {
		return "", false, f.mediaURLErr
	}
	if f.noURL {
		return "", false, nil
	}
	if _, ok := f.contents[relPath]; !ok {
		return "", false, os.ErrNotExist
	}
	return f.urlPrefix + relPath, true, nil
}

func (f *urlCorpusFS) urls(relPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.urlCalls[relPath]
}

func (f *urlCorpusFS) localizes(relPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.localCalls[relPath]
}

// TestEmbeddingWorker_AudioVideo_RangeReadsViaURL pins issue #243: when the
// corpus backend can presign a range-seekable URL, audio/video segment extraction
// goes through the URL (HTTP range-read) path, NOT Localize (whole-object
// download). The extractor receives the presigned URL and the source extension.
func TestEmbeddingWorker_AudioVideo_RangeReadsViaURL(t *testing.T) {
	fsys := newURLCorpusFS(map[string][]byte{"clip.mp4": []byte("FULLVIDEO")})

	var gotURL, gotExt string
	var gotStart, gotEnd int
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{0.6, 0.8}}}
	worker := &index.EmbeddingWorker{
		Source: &fakeChunkSource{tasks: []model.ChunkTask{
			avTask(11, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 1000, EndMS: 3000}),
		}},
		Index: index.NewHNSWIndex(""), Embedder: emb,
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 4, ModelForText: "gemini-embedding-2",
		ExtractSegmentURLFunc: func(_ context.Context, url, ext string, s, e int) ([]byte, error) {
			gotURL, gotExt, gotStart, gotEnd = url, ext, s, e
			return []byte("URLSEG"), nil
		},
		ExtractSegmentFunc: func(context.Context, string, int, int) ([]byte, error) {
			t.Fatal("local-path extractor must not be called when a presigned URL is available")
			return nil, nil
		},
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("indexed = %d, want 1", n)
	}
	if !strings.HasPrefix(gotURL, "https://") || !strings.HasSuffix(gotURL, "clip.mp4") {
		t.Fatalf("extractor URL = %q, want a presigned https URL for clip.mp4", gotURL)
	}
	if gotExt != ".mp4" {
		t.Errorf("source ext = %q, want .mp4", gotExt)
	}
	if gotStart != 1000 || gotEnd != 3000 {
		t.Errorf("segment span = [%d,%d), want [1000,3000)", gotStart, gotEnd)
	}
	if got := fsys.localizes("clip.mp4"); got != 0 {
		t.Fatalf("Localize(clip.mp4) called %d times, want 0 (URL path must not download the whole object)", got)
	}
	if len(emb.gotMedia) != 1 || string(emb.gotMedia[0].Data) != "URLSEG" {
		t.Fatalf("EmbedMedia got %+v, want the URL-extracted segment", emb.gotMedia)
	}
}

// TestEmbeddingWorker_AudioVideo_URLPresignedOncePerBatch pins that the presigned
// URL is memoized in the per-batch cache (issue #279): three time-windows of the
// same MediaRef presign exactly once and all extract from that one URL.
func TestEmbeddingWorker_AudioVideo_URLPresignedOncePerBatch(t *testing.T) {
	fsys := newURLCorpusFS(map[string][]byte{"clip.mp4": []byte("FULLVIDEO")})

	var extractURLs []string
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}, {0, 1}, {1, 1}}}
	worker := &index.EmbeddingWorker{
		Source: &fakeChunkSource{tasks: []model.ChunkTask{
			avTask(1, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 0, EndMS: 1000}),
			avTask(2, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 1000, EndMS: 2000}),
			avTask(3, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 2000, EndMS: 3000}),
		}},
		Index: index.NewHNSWIndex(""), Embedder: emb,
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 8, ModelForText: "gemini-embedding-2",
		ExtractSegmentURLFunc: func(_ context.Context, url, _ string, _, _ int) ([]byte, error) {
			extractURLs = append(extractURLs, url)
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
	if got := fsys.urls("clip.mp4"); got != 1 {
		t.Fatalf("MediaURL(clip.mp4) called %d times, want 1 (per-batch cache should presign once)", got)
	}
	if got := fsys.localizes("clip.mp4"); got != 0 {
		t.Fatalf("Localize(clip.mp4) called %d times, want 0", got)
	}
	if len(extractURLs) != 3 {
		t.Fatalf("extractor called %d times, want 3", len(extractURLs))
	}
	for i, u := range extractURLs {
		if u != extractURLs[0] {
			t.Fatalf("window %d extracted from %q, want shared URL %q", i, u, extractURLs[0])
		}
	}
}

// TestEmbeddingWorker_AudioVideo_FallsBackToLocalizeWithoutURL confirms that a
// backend which cannot presign a URL (ok=false) still works via the historical
// Localize path — preserving LocalFS-style behavior. countingCorpusFS does NOT
// implement MediaURLProvider, so the URL path is skipped entirely.
func TestEmbeddingWorker_AudioVideo_FallsBackToLocalizeWithoutURL(t *testing.T) {
	fsys := newCountingCorpusFS(map[string][]byte{"clip.mp4": []byte("FULLVIDEO")})

	var localPaths []string
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{0.6, 0.8}}}
	worker := &index.EmbeddingWorker{
		Source: &fakeChunkSource{tasks: []model.ChunkTask{
			avTask(12, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 0, EndMS: 1000}),
		}},
		Index: index.NewHNSWIndex(""), Embedder: emb,
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 4, ModelForText: "gemini-embedding-2",
		ExtractSegmentFunc: func(_ context.Context, path string, _, _ int) ([]byte, error) {
			localPaths = append(localPaths, path)
			return []byte("LOCALSEG"), nil
		},
		ExtractSegmentURLFunc: func(context.Context, string, string, int, int) ([]byte, error) {
			t.Fatal("URL extractor must not be called when the backend cannot presign a URL")
			return nil, nil
		},
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("indexed = %d, want 1", n)
	}
	if got := fsys.localizes("clip.mp4"); got != 1 {
		t.Fatalf("Localize(clip.mp4) = %d, want 1 (fallback path)", got)
	}
	if len(localPaths) != 1 {
		t.Fatalf("local extractor called %d times, want 1", len(localPaths))
	}
	if len(emb.gotMedia) != 1 || string(emb.gotMedia[0].Data) != "LOCALSEG" {
		t.Fatalf("EmbedMedia got %+v, want the Localize-extracted segment", emb.gotMedia)
	}
}

// TestEmbeddingWorker_AudioVideo_MediaURLErrorIsRetryable confirms a presign
// failure surfaces as a (retryable) read error, not a fatal one — a transient
// presign hiccup should leave the chunk pending rather than mark it failed.
func TestEmbeddingWorker_AudioVideo_MediaURLErrorIsRetryable(t *testing.T) {
	fsys := newURLCorpusFS(map[string][]byte{"clip.mp4": []byte("FULLVIDEO")})
	fsys.mediaURLErr = errors.New("presign throttled")

	src := &fakeChunkSource{tasks: []model.ChunkTask{
		avTask(13, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 0, EndMS: 1000}),
	}}
	worker := &index.EmbeddingWorker{
		Source: src,
		Index:  index.NewHNSWIndex(""), Embedder: &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}},
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 4, ModelForText: "gemini-embedding-2",
		ExtractSegmentURLFunc: func(context.Context, string, string, int, int) ([]byte, error) {
			t.Fatal("extractor must not be called when MediaURL errors")
			return nil, nil
		},
	}

	_, err := worker.RunOnce(context.Background(), "text")
	if err == nil {
		t.Fatal("expected an error when MediaURL fails")
	}
	if errors.Is(err, index.ErrFatal) {
		t.Fatalf("MediaURL failure should be retryable, got fatal: %v", err)
	}
	// Retryable means the chunk stays pending, NOT marked permanently failed,
	// so a later cycle (with a fresh presigned URL) can embed it.
	if len(src.failedLabels) != 0 {
		t.Fatalf("chunk was marked failed %v on a transient presign error; want left pending", src.failedLabels)
	}
}

// TestEmbeddingWorker_AudioVideo_URLExtractErrorIsRetryable pins that an
// extraction failure over the presigned URL (e.g. the URL expired mid-batch and
// ffmpeg gets a 403) leaves the chunk pending rather than permanently failed, so
// the next cycle re-presigns and recovers it (issue #243).
func TestEmbeddingWorker_AudioVideo_URLExtractErrorIsRetryable(t *testing.T) {
	fsys := newURLCorpusFS(map[string][]byte{"clip.mp4": []byte("FULLVIDEO")})

	src := &fakeChunkSource{tasks: []model.ChunkTask{
		avTask(14, "clip.mp4", "video", model.Span{Kind: "time", StartMS: 0, EndMS: 1000}),
	}}
	worker := &index.EmbeddingWorker{
		Source: src,
		Index:  index.NewHNSWIndex(""), Embedder: &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}},
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 4, ModelForText: "gemini-embedding-2",
		ExtractSegmentURLFunc: func(context.Context, string, string, int, int) ([]byte, error) {
			return nil, errors.New("Server returned 403 Forbidden")
		},
	}

	_, err := worker.RunOnce(context.Background(), "text")
	if err == nil {
		t.Fatal("expected an error when URL extraction fails")
	}
	if errors.Is(err, index.ErrFatal) {
		t.Fatalf("URL extract failure should be retryable, got fatal: %v", err)
	}
	if len(src.failedLabels) != 0 {
		t.Fatalf("chunk was marked failed %v on an expired-URL extract error; want left pending", src.failedLabels)
	}
}
