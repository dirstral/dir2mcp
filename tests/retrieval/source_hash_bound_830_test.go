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
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// These tests cover site 2 of #830: open_file's source-byte hash (hashSourceBytes),
// the fallback that derives the OCR/transcript cache key when the store cannot
// report the base content hash.
//
// The severity differs from the memory cases and the reason is worth stating,
// because it is the reason this bound exists at all. The hash streams into a
// hasher, so it holds constant memory and cannot OOM the daemon. What it consumed
// without a bound was BANDWIDTH: one open_file call pulled a whole remote object of
// any size, so an unprivileged client could make the server download tens of
// gigabytes of metered egress per request. Same class, lower severity: a size check
// is still only a measurement, and the object still chooses how many bytes it
// serves.

const sourceHashTestCap int64 = 64 * 1024

// unsizedSourceFS is a CorpusFS whose objects report no size. It serves either
// explicit bytes or a generated run of a given length, and counts every byte it
// delivered so a test can assert on what the read actually pulled rather than on
// what a check believed.
type unsizedSourceFS struct {
	contents map[string][]byte
	sizes    map[string]int64

	mu        sync.Mutex
	delivered int64
}

func (f *unsizedSourceFS) Open(_ context.Context, relPath string) (io.ReadSeekCloser, error) {
	if data, ok := f.contents[relPath]; ok {
		return &countingSource{inner: bytes.NewReader(data), fs: f}, nil
	}
	if size, ok := f.sizes[relPath]; ok {
		return &countingSource{inner: &runReader{total: size}, fs: f}, nil
	}
	return nil, os.ErrNotExist
}

func (f *unsizedSourceFS) Walk(context.Context, string, corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return nil, errors.New("unsizedSourceFS: Walk not implemented")
}

func (f *unsizedSourceFS) Localize(context.Context, string) (string, func(), error) {
	return "", func() {}, errors.New("unsizedSourceFS: Localize not implemented")
}

func (f *unsizedSourceFS) bytesDelivered() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.delivered
}

// runReader generates total bytes of filler. The bytes are generated rather than
// allocated: on the unbounded code the hash consumes the whole object, so a fixture
// held in a []byte would have to be materialized in full first.
type runReader struct {
	total int64
	pos   int64
}

func (r *runReader) Read(p []byte) (int, error) {
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
	return int(n), nil
}

func (r *runReader) Seek(offset int64, whence int) (int64, error) {
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

// countingSource records every byte read through it on the owning filesystem.
type countingSource struct {
	inner io.ReadSeeker
	fs    *unsizedSourceFS
}

func (c *countingSource) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	c.fs.mu.Lock()
	c.fs.delivered += int64(n)
	c.fs.mu.Unlock()
	return n, err
}

func (c *countingSource) Seek(offset int64, whence int) (int64, error) {
	return c.inner.Seek(offset, whence)
}

func (c *countingSource) Close() error { return nil }

// newBoundedHashService builds an open_file service with the cap plumbed in the way
// the CLI plumbs it (from ingest.ResolvedMaxFileBytes) and no store, so the
// source-hash fallback is the path under test.
func newBoundedHashService(t *testing.T, root, stateDir string, fsys corpusfs.CorpusFS, capBytes int64) *retrieval.Service {
	t.Helper()
	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fsys)
	svc.SetMaxFileBytes(capBytes)
	svc.SetDerivationCacheIdentities("ocr|docling|||", "")
	return svc
}

// TestOpenFileSourceHash_BoundsTheReadOnAnUnsizedSource is the core case: an object
// that reports no size and serves 16x the cap is read to at most cap+1 bytes, and
// the refusal names the cap through the existing sentinel (which the MCP layer maps
// to §14.4 FILE_TOO_LARGE) rather than a retryable INTERNAL_ERROR.
func TestOpenFileSourceHash_BoundsTheReadOnAnUnsizedSource(t *testing.T) {
	root := t.TempDir()
	fsys := &unsizedSourceFS{sizes: map[string]int64{"docs/huge.pdf": sourceHashTestCap * 16}}
	svc := newBoundedHashService(t, root, filepath.Join(root, ".dir2mcp"), fsys, sourceHashTestCap)

	_, err := svc.OpenFile(context.Background(), "docs/huge.pdf", model.Span{}, 20000)
	if err == nil {
		t.Fatal("an over-cap source must not be hashed to completion")
	}
	if !errors.Is(err, corpusfs.ErrObjectTooLarge) {
		t.Fatalf("error is not the cap sentinel: %v", err)
	}
	if got := fsys.bytesDelivered(); got != sourceHashTestCap+1 {
		t.Fatalf("the source delivered %d bytes, want exactly %d (cap+1): the read is bounded neither short nor long", got, sourceHashTestCap+1)
	}
}

// TestOpenFileSourceHash_AtTheCapHashesTheWholeFile is the off-by-one guard AND the
// correctness guard: a source of exactly the cap must be hashed in full, and the
// digest must still be ingest's, so the cached OCR text is FOUND. A bound that
// stopped at the cap, or one that hashed a prefix, would miss the entry.
func TestOpenFileSourceHash_AtTheCapHashesTheWholeFile(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	content := bytes.Repeat([]byte("x"), int(sourceHashTestCap))
	fsys := &unsizedSourceFS{contents: map[string][]byte{"docs/exact.pdf": content}}

	const ocrText = "# Extracted\n\nthe cached text for a file of exactly the cap"
	cacheDir := filepath.Join(stateDir, "cache", "ocr")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir ocr cache: %v", err)
	}
	key := ingestOCRCacheKey(t, stateDir, content)
	if err := os.WriteFile(filepath.Join(cacheDir, key+".md"), []byte(ocrText), 0o644); err != nil {
		t.Fatalf("write ocr cache: %v", err)
	}

	svc := newBoundedHashService(t, root, stateDir, fsys, sourceHashTestCap)
	out, err := svc.OpenFile(context.Background(), "docs/exact.pdf", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("a source of exactly the cap must be hashed and served: %v", err)
	}
	if out != ocrText {
		t.Fatalf("open_file returned %q, want the cached OCR text (the digest must be over the WHOLE file)", out)
	}
	if got := fsys.bytesDelivered(); got != sourceHashTestCap {
		t.Fatalf("the source delivered %d bytes, want %d", got, sourceHashTestCap)
	}
}

// TestOpenFileSourceHash_UnsetCapStillBoundsTheRead pins the fail-closed default: a
// service with no cap plumbed in reads under the shared default bound, not without
// one.
func TestOpenFileSourceHash_UnsetCapStillBoundsTheRead(t *testing.T) {
	root := t.TempDir()
	defaultCap := corpusfs.DefaultMaxFileSizeBytes()
	fsys := &unsizedSourceFS{sizes: map[string]int64{"docs/huge.pdf": defaultCap * 2}}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(filepath.Join(root, ".dir2mcp"))
	svc.SetCorpusFS(fsys)
	// SetMaxFileBytes deliberately not called.

	if _, err := svc.OpenFile(context.Background(), "docs/huge.pdf", model.Span{}, 20000); !errors.Is(err, corpusfs.ErrObjectTooLarge) {
		t.Fatalf("an unset cap must not mean an unbounded read, got err: %v", err)
	}
	if got := fsys.bytesDelivered(); got != defaultCap+1 {
		t.Fatalf("the source delivered %d bytes, want %d (default cap+1)", got, defaultCap+1)
	}
}
