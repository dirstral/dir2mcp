package tests

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// These tests cover site 1 of #830: the embedding worker's whole-file media read
// (image and PDF bytes) held no byte bound, so the bytes of a file that grew after
// it was chunked were pulled into the daemon's memory whole, at whatever size the
// file had reached.
//
// Every case asserts on TWO things: the outcome (embedded, or marked failed with a
// reason that names the cap), and the number of bytes the source was actually asked
// for. A repeated size check could produce the first without the second, and the
// second is the defect.

// generatedReader serves total bytes without ever reporting a size, and counts the
// bytes it actually delivered. It is the shape a size check cannot constrain: the
// caller learns how big the source is only by reading it.
//
// The bytes are generated, never allocated: on the unbounded code the read consumes
// everything the reader offers, so a fixture held in a []byte would have to be
// materialized in full before the test could prove anything about it.
type generatedReader struct {
	total     int64
	pos       int64
	delivered *int64
	mu        *sync.Mutex
}

func (r *generatedReader) Read(p []byte) (int, error) {
	if r.pos >= r.total {
		return 0, io.EOF
	}
	n := int64(len(p))
	if remaining := r.total - r.pos; remaining < n {
		n = remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'x'
	}
	r.pos += n
	r.mu.Lock()
	*r.delivered += n
	r.mu.Unlock()
	return int(n), nil
}

func (r *generatedReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = offset
	case io.SeekCurrent:
		r.pos += offset
	case io.SeekEnd:
		r.pos = r.total + offset
	}
	return r.pos, nil
}

func (r *generatedReader) Close() error { return nil }

// generatingCorpusFS serves each ref as a generatedReader of the configured size
// and records the total bytes every read pulled.
type generatingCorpusFS struct {
	sizes     map[string]int64
	mu        sync.Mutex
	delivered int64
}

func (f *generatingCorpusFS) Open(_ context.Context, relPath string) (io.ReadSeekCloser, error) {
	size, ok := f.sizes[relPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &generatedReader{total: size, delivered: &f.delivered, mu: &f.mu}, nil
}

func (f *generatingCorpusFS) Walk(context.Context, string, corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return nil, errors.New("generatingCorpusFS: Walk not implemented")
}

func (f *generatingCorpusFS) Localize(context.Context, string) (string, func(), error) {
	return "", func() {}, errors.New("generatingCorpusFS: Localize not implemented")
}

func (f *generatingCorpusFS) bytesDelivered() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.delivered
}

const mediaReadTestCap int64 = 64 * 1024

// TestMediaRead_BoundsTheReadOnAnUnsizedSource is the core case: a source that
// reports no size and serves 16x the cap is read to at most cap+1 bytes, and the
// chunk is marked failed with a reason that names the cap.
//
// The bound cannot be replaced by a check. This reader never states a size, so
// "the file is small enough" is not a claim anyone can verify before the read; only
// the limit on the read itself holds.
func TestMediaRead_BoundsTheReadOnAnUnsizedSource(t *testing.T) {
	fsys := &generatingCorpusFS{sizes: map[string]int64{"big.png": mediaReadTestCap * 16}}
	source := &fakeChunkSource{tasks: []model.ChunkTask{mediaTask(1, "big.png", "image")}}
	worker := &index.EmbeddingWorker{
		Source: source, Index: index.NewHNSWIndex(""), Embedder: &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}},
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 4, ModelForText: "gemini-embedding-2",
		MaxFileBytes: mediaReadTestCap,
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err == nil {
		t.Fatal("an over-cap media read must not report success")
	}
	if errors.Is(err, index.ErrFatal) {
		t.Fatalf("one over-cap file must not stop the worker for the whole corpus: %v", err)
	}
	if n != 0 {
		t.Fatalf("indexed = %d, want 0", n)
	}
	if got := fsys.bytesDelivered(); got != mediaReadTestCap+1 {
		t.Fatalf("the source delivered %d bytes, want exactly %d (cap+1): the read is bounded neither short nor long", got, mediaReadTestCap+1)
	}
	if len(source.failedLabels) != 1 || source.failedLabels[0] != 1 {
		t.Fatalf("failed labels = %#v, want [1]", source.failedLabels)
	}
	if !strings.Contains(source.failedReason, "max file size") {
		t.Fatalf("failure reason %q does not name the size cap", source.failedReason)
	}
}

// TestMediaRead_FileAtTheCapIsStillEmbedded is the off-by-one guard, and it is why
// the read asks for cap+1 rather than cap: a file of exactly
// `ingest.max_file_mb` is inside the operator's policy and must embed cleanly.
func TestMediaRead_FileAtTheCapIsStillEmbedded(t *testing.T) {
	fsys := &generatingCorpusFS{sizes: map[string]int64{"exact.png": mediaReadTestCap}}
	source := &fakeChunkSource{tasks: []model.ChunkTask{mediaTask(1, "exact.png", "image")}}
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}}
	worker := &index.EmbeddingWorker{
		Source: source, Index: index.NewHNSWIndex(""), Embedder: emb,
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 4, ModelForText: "gemini-embedding-2",
		MaxFileBytes: mediaReadTestCap,
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("a file of exactly the cap must embed, got err: %v", err)
	}
	if n != 1 {
		t.Fatalf("indexed = %d, want 1", n)
	}
	if len(emb.gotMedia) != 1 || int64(len(emb.gotMedia[0].Data)) != mediaReadTestCap {
		t.Fatalf("embedder received %d item(s) with %d bytes, want 1 with %d", len(emb.gotMedia), len(emb.gotMedia[0].Data), mediaReadTestCap)
	}
	if got := fsys.bytesDelivered(); got != mediaReadTestCap {
		t.Fatalf("the source delivered %d bytes, want %d", got, mediaReadTestCap)
	}
}

// TestMediaRead_LocalFileThatGrewAfterChunkingIsRefused is the production shape of
// the defect, on the real LocalFS: the chunk row was written when the file was
// small, the file is large by the time the embed loop reaches it, and nothing
// between the two re-measures it. The read is the only place the growth can be
// caught.
//
// It also pins the blast radius: a healthy sibling chunk in the same batch still
// embeds, so one over-cap file costs one chunk, not the batch and not the worker.
func TestMediaRead_LocalFileThatGrewAfterChunkingIsRefused(t *testing.T) {
	root := t.TempDir()
	// Chunked while small (the row that survives), then grown past the cap.
	small := filepath.Join(root, "small.png")
	if err := os.WriteFile(small, []byte("PNGDATA"), 0o600); err != nil {
		t.Fatal(err)
	}
	grown := filepath.Join(root, "grown.png")
	if err := os.WriteFile(grown, make([]byte, mediaReadTestCap*4), 0o600); err != nil {
		t.Fatal(err)
	}

	source := &fakeChunkSource{tasks: []model.ChunkTask{
		mediaTask(1, "grown.png", "image"),
		mediaTask(2, "small.png", "image"),
	}}
	emb := &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}}
	worker := &index.EmbeddingWorker{
		Source: source, Index: index.NewHNSWIndex(""), Embedder: emb,
		RootDir: root, BatchSize: 4, ModelForText: "gemini-embedding-2",
		MaxFileBytes: mediaReadTestCap,
	}

	n, err := worker.RunOnce(context.Background(), "text")
	if err != nil {
		t.Fatalf("the healthy sibling must still embed: %v", err)
	}
	if n != 1 {
		t.Fatalf("indexed = %d, want 1 (the small file only)", n)
	}
	if len(source.failedLabels) != 1 || source.failedLabels[0] != 1 {
		t.Fatalf("failed labels = %#v, want [1] (the grown file only)", source.failedLabels)
	}
	if !strings.Contains(source.failedReason, "max file size") {
		t.Fatalf("failure reason %q does not name the size cap", source.failedReason)
	}
	// The grown file's bytes must never reach the embedder.
	for _, item := range emb.gotMedia {
		if int64(len(item.Data)) > mediaReadTestCap {
			t.Fatalf("the embedder received %d bytes, past the %d-byte cap", len(item.Data), mediaReadTestCap)
		}
	}
}

// TestMediaRead_UnsetCapStillBoundsTheRead pins the fail-closed default: a worker
// built without MaxFileBytes reads under the shared default bound, not without one.
func TestMediaRead_UnsetCapStillBoundsTheRead(t *testing.T) {
	defaultCap := corpusfs.DefaultMaxFileSizeBytes()
	fsys := &generatingCorpusFS{sizes: map[string]int64{"huge.png": defaultCap * 2}}
	source := &fakeChunkSource{tasks: []model.ChunkTask{mediaTask(1, "huge.png", "image")}}
	worker := &index.EmbeddingWorker{
		Source: source, Index: index.NewHNSWIndex(""), Embedder: &fakeMultimodalEmbedder{mediaVecs: [][]float32{{1, 0}}},
		Corpus: fsys, RootDir: t.TempDir(), BatchSize: 4, ModelForText: "gemini-embedding-2",
		// MaxFileBytes deliberately unset.
	}

	if _, err := worker.RunOnce(context.Background(), "text"); err == nil {
		t.Fatal("an unset cap must not mean an unbounded read")
	}
	if got := fsys.bytesDelivered(); got != defaultCap+1 {
		t.Fatalf("the source delivered %d bytes, want %d (default cap+1)", got, defaultCap+1)
	}
}
