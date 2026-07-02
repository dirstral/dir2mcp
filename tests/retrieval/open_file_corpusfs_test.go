package tests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// fakeCorpusFS is an in-memory corpusfs.CorpusFS whose objects live only in a
// map — never on the local filesystem. It stands in for an object-store backend
// (S3) so open_file's read path can be exercised without any local source file,
// which is exactly the condition that broke S3 corpora (#432): retrieval used to
// resolve against the local FS and fail because the bytes are remote.
type fakeCorpusFS struct {
	objects map[string][]byte
}

// readSeekCloser adapts a *bytes.Reader (which has no Close) to the
// io.ReadSeekCloser the CorpusFS.Open contract returns.
type readSeekCloser struct {
	*bytes.Reader
}

func (readSeekCloser) Close() error { return nil }

func (f *fakeCorpusFS) Open(ctx context.Context, relPath string) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := f.objects[relPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return readSeekCloser{bytes.NewReader(data)}, nil
}

func (f *fakeCorpusFS) Walk(context.Context, string, corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return nil, errors.New("fakeCorpusFS: Walk not implemented")
}

func (f *fakeCorpusFS) Localize(context.Context, string) (string, func(), error) {
	return "", func() {}, errors.New("fakeCorpusFS: Localize not implemented")
}

// TestOpenFile_CorpusFS_TextReturnsContent verifies that open_file on a text
// document served by a CorpusFS backend returns the object's bytes even though
// no local file exists under RootDir — the S3 raw-text read path (#432).
func TestOpenFile_CorpusFS_TextReturnsContent(t *testing.T) {
	// RootDir points at an empty temp dir: there is deliberately NO local file,
	// so a regression to the local-FS read path would fail with not-found.
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	body := "# Remote Doc\n\nServed from the object store, not the local disk."
	fs := &fakeCorpusFS{objects: map[string][]byte{"docs/remote.md": []byte(body)}}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)

	out, err := svc.OpenFile(context.Background(), "docs/remote.md", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile over CorpusFS returned err: %v", err)
	}
	if out != body {
		t.Fatalf("expected full remote body, got %q", out)
	}
}

// TestOpenFile_CorpusFS_PDFReturnsOCRMarkdown verifies that open_file on a
// binary document (PDF) with no span reads the local OCR cache keyed by the
// sha256 of the object's bytes, where those bytes are hashed by STREAMING them
// through the CorpusFS Open seam (not os.Open of a local path). This is the
// OCR/transcript read path that was silently broken on S3 (#432): os.Stat of the
// non-existent local path failed before the (present) cache could be consulted.
func TestOpenFile_CorpusFS_PDFReturnsOCRMarkdown(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	pdfBytes := []byte("%PDF-1.4\r%\xe2\xe3\xcf\xd3\r\n1 0 obj<</Type/Catalog>>endobj\n%%EOF")
	fs := &fakeCorpusFS{objects: map[string][]byte{"docs/remote.pdf": pdfBytes}}

	ocrText := "# Remote OCR\n\nARRANGEMENT OF SECTIONS\n\n1. Short title."
	cacheDir := filepath.Join(stateDir, "cache", "ocr")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir ocr cache: %v", err)
	}
	sum := sha256.Sum256(pdfBytes)
	cachePath := filepath.Join(cacheDir, hex.EncodeToString(sum[:])+".md")
	if err := os.WriteFile(cachePath, []byte(ocrText), 0o644); err != nil {
		t.Fatalf("write ocr cache: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)

	out, err := svc.OpenFile(context.Background(), "docs/remote.pdf", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile no-span on remote pdf returned err: %v", err)
	}
	if strings.HasPrefix(out, "%PDF") {
		t.Fatalf("expected OCR markdown, got raw PDF bytes: %q", out[:min(80, len(out))])
	}
	if !strings.Contains(out, "ARRANGEMENT OF SECTIONS") {
		t.Fatalf("expected OCR markdown content, got %q", out)
	}
}

// TestOpenFile_CorpusFS_PDFNoCacheReturnsOCRNotReady verifies that when the OCR
// cache has not been written yet (ingest still running), the CorpusFS read path
// preserves the retryable ErrOCRNotReady contract rather than leaking a
// backend read error.
func TestOpenFile_CorpusFS_PDFNoCacheReturnsOCRNotReady(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	pdfBytes := []byte("%PDF-1.4 no-cache")
	fs := &fakeCorpusFS{objects: map[string][]byte{"docs/pending.pdf": pdfBytes}}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)

	_, err := svc.OpenFile(context.Background(), "docs/pending.pdf", model.Span{}, 200)
	if !errors.Is(err, model.ErrOCRNotReady) {
		t.Fatalf("expected ErrOCRNotReady, got %v", err)
	}
}
