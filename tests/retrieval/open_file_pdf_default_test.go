package tests

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// writePDFAndOCRCache writes a fake PDF at relPath under root and seeds the
// OCR cache (under stateDir) with markdown keyed by sha256 of the PDF bytes.
// Returns the byte content of the source so callers can compute their own
// assertions if needed.
func writePDFAndOCRCache(t *testing.T, root, stateDir, relPath, ocrMarkdown string) []byte {
	t.Helper()
	pdfBytes := []byte("%PDF-1.4\r%\xe2\xe3\xcf\xd3\r\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\ntrailer\n%%EOF")
	absPath := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.WriteFile(absPath, pdfBytes, 0o644); err != nil {
		t.Fatalf("write pdf: %v", err)
	}
	if ocrMarkdown != "" {
		cacheDir := filepath.Join(stateDir, "cache", "ocr")
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			t.Fatalf("mkdir ocr cache: %v", err)
		}
		sum := sha256.Sum256(pdfBytes)
		cachePath := filepath.Join(cacheDir, hex.EncodeToString(sum[:])+".md")
		if err := os.WriteFile(cachePath, []byte(ocrMarkdown), 0o644); err != nil {
			t.Fatalf("write ocr cache: %v", err)
		}
	}
	return pdfBytes
}

func TestOpenFile_PDFNoSpan_ReturnsOCRMarkdown(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")
	ocrText := "# Financial Investigation Agency Amendment Act\n\nARRANGEMENT OF SECTIONS\n\n1. Short title."
	writePDFAndOCRCache(t, root, stateDir, "docs/act.pdf", ocrText)

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)

	out, err := svc.OpenFile(context.Background(), "docs/act.pdf", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile no-span on pdf returned err: %v", err)
	}
	if strings.HasPrefix(out, "%PDF") {
		t.Fatalf("expected OCR markdown, got raw PDF bytes (first 80 chars): %q", out[:min(80, len(out))])
	}
	if !strings.Contains(out, "ARRANGEMENT OF SECTIONS") {
		t.Fatalf("expected OCR markdown content, got %q", out)
	}
}

func TestOpenFile_PDFNoSpan_TruncatesByMaxChars(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")
	ocrText := strings.Repeat("x", 5000)
	writePDFAndOCRCache(t, root, stateDir, "docs/long.pdf", ocrText)

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)

	// OpenFile (no -WithMeta) still applies maxChars internally.
	out, err := svc.OpenFile(context.Background(), "docs/long.pdf", model.Span{}, 500)
	if err != nil {
		t.Fatalf("OpenFile err: %v", err)
	}
	if len([]rune(out)) != 500 {
		t.Fatalf("expected exactly 500 chars after truncation, got %d", len([]rune(out)))
	}

	withMeta, _, truncated, err := openFileWithMeta(t, svc, "docs/long.pdf", model.Span{}, 500)
	if err != nil {
		t.Fatalf("OpenFileWithMeta err: %v", err)
	}
	if !truncated {
		t.Fatalf("expected truncated=true when max_chars < cache length, got false (len=%d)", len([]rune(withMeta)))
	}
}

func TestOpenFile_PDFNoCache_ReturnsErrOCRNotReady(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")
	// Intentionally pass empty ocrMarkdown so the cache file is not created.
	writePDFAndOCRCache(t, root, stateDir, "docs/missing.pdf", "")

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)

	_, err := svc.OpenFile(context.Background(), "docs/missing.pdf", model.Span{}, 200)
	if !errors.Is(err, model.ErrOCRNotReady) {
		t.Fatalf("expected ErrOCRNotReady, got %v", err)
	}
}

func TestOpenFile_PDFWithPage_StillReadsRawBytesPath(t *testing.T) {
	// With page=N the service routes through the existing page-slicing path
	// rather than the new OCR-cache fallback. Use a text file that contains
	// form-feed page separators to mimic OCR'd content the page slicer
	// understands; the .pdf extension on the rel_path triggers the
	// "binary doc type" branch only when no span is set.
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")
	filePath := filepath.Join(root, "docs", "paged.pdf")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("page1-content\fpage2-content\fpage3-content"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)

	out, err := svc.OpenFile(context.Background(), "docs/paged.pdf", model.Span{Kind: "page", Page: 2}, 200)
	if err != nil {
		t.Fatalf("OpenFile with page=2 err: %v", err)
	}
	if out != "page2-content" {
		t.Fatalf("page=2 should yield page 2 content, got %q", out)
	}
}

func TestOpenFile_MarkdownDefault_UnchangedBehavior(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")
	filePath := filepath.Join(root, "docs", "readme.md")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "# Title\n\nHello world."
	if err := os.WriteFile(filePath, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)

	out, err := svc.OpenFile(context.Background(), "docs/readme.md", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile md no-span err: %v", err)
	}
	if out != body {
		t.Fatalf("expected full markdown body, got %q", out)
	}
}

// TestOpenFile_PDFExtensionOnDirectory_ReturnsErrDocTypeUnsupported asserts
// that open_file on a path with a binary extension that actually resolves to a
// directory returns ErrDocTypeUnsupported (mapped to DOC_TYPE_UNSUPPORTED at
// the MCP layer) rather than bubbling an opaque OS error up as an
// INTERNAL_ERROR. This mirrors how openFileFromResolvedPath rejects directory
// targets and prevents the OCR-cache fallback from leaking EISDIR.
func TestOpenFile_PDFExtensionOnDirectory_ReturnsErrDocTypeUnsupported(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")
	// A directory whose name happens to end in .pdf — this would otherwise
	// satisfy isBinaryDocType and route through openFileFromOCRCache.
	dirPath := filepath.Join(root, "docs", "weird.pdf")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatalf("mkdir directory target: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)

	_, err := svc.OpenFile(context.Background(), "docs/weird.pdf", model.Span{}, 200)
	if !errors.Is(err, model.ErrDocTypeUnsupported) {
		t.Fatalf("expected ErrDocTypeUnsupported for a directory target, got %v", err)
	}
}

// openFileWithMeta calls the *WithMeta variant via the interface assertion the
// MCP handler uses, returning the truncation flag.
func openFileWithMeta(t *testing.T, svc *retrieval.Service, relPath string, span model.Span, maxChars int) (string, bool, bool, error) {
	t.Helper()
	type withMeta interface {
		OpenFileWithMeta(ctx context.Context, relPath string, span model.Span, maxChars int) (string, bool, error)
	}
	w, ok := interface{}(svc).(withMeta)
	if !ok {
		t.Fatalf("retrieval.Service does not implement OpenFileWithMeta")
	}
	content, truncated, err := w.OpenFileWithMeta(context.Background(), relPath, span, maxChars)
	return content, false, truncated, err
}
