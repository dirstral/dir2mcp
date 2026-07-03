package tests

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/corpusfs"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// These tests lock the fix for issue #488: open_file's OCR/transcript cache
// lookup must key the cache the SAME identity-aware way ingest's writer does
// (SPEC §8.6.7). Ingest folds the active OCR/STT derivation identity into the
// cache key (ingest.Service.ocrCacheKey / transcriptCacheKey); retrieval used to
// key on the source bytes alone, so on every real docling/mistral/STT corpus the
// lookup MISSED the entry ingest wrote and returned OCR_NOT_READY. The cache
// files below are written at ingest's REAL key (ing.OCRCacheKey /
// ing.ReadOrComputeTranscript), so a hit proves the two keys agree.

// fakeKeyStore is a minimal model.Store that always implements the optional
// ocrSourceHashProvider capability (OCRSourceContentHash is defined below). It
// returns ok=true — supplying baseHash — only when baseHash is non-empty, letting
// open_file derive the identity-aware key WITHOUT a full object GET (the #488 perf
// path); with an empty baseHash it reports ok=false so open_file falls back to
// hashing the bytes.
type fakeKeyStore struct {
	baseHash string
}

func (f *fakeKeyStore) Init(context.Context) error                           { return nil }
func (f *fakeKeyStore) UpsertDocument(context.Context, model.Document) error { return nil }
func (f *fakeKeyStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, nil
}
func (f *fakeKeyStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (f *fakeKeyStore) Close() error { return nil }

// OCRSourceContentHash is ALWAYS defined, so *fakeKeyStore always satisfies the
// optional ocrSourceHashProvider capability. It signals whether a base hash is
// actually available via its ok return (false when baseHash is unset), not by the
// method's presence.
func (f *fakeKeyStore) OCRSourceContentHash(_ context.Context, _ string) (string, bool, error) {
	if f.baseHash == "" {
		return "", false, nil
	}
	return f.baseHash, true, nil
}

// countingCorpusFS wraps an object map and counts Open calls, so a test can
// assert open_file skipped the full object GET.
type countingCorpusFS struct {
	objects map[string][]byte
	opens   int
}

func (c *countingCorpusFS) Open(ctx context.Context, relPath string) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.opens++
	data, ok := c.objects[relPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	return readSeekCloser{bytes.NewReader(data)}, nil
}

func (c *countingCorpusFS) Walk(context.Context, string, corpusfs.Options) ([]corpusfs.DiscoveredFile, error) {
	return nil, errors.New("countingCorpusFS: Walk not implemented")
}

func (c *countingCorpusFS) Localize(context.Context, string) (string, func(), error) {
	return "", func() {}, errors.New("countingCorpusFS: Localize not implemented")
}

// ingestOCRCacheKey builds an ingest.Service with the docling extractor and
// returns the on-disk OCR cache key it would write for content — the
// authoritative, identity-aware key retrieval must reproduce.
func ingestOCRCacheKey(t *testing.T, stateDir string, content []byte) string {
	t.Helper()
	cfg := config.Config{RootDir: t.TempDir(), StateDir: stateDir, STTProvider: "off"}
	ing, err := ingest.NewService(cfg, &fakeKeyStore{})
	if err != nil {
		t.Fatalf("ingest.NewService: %v", err)
	}
	ing.SetDocumentExtractor(ingest.NewDoclingExtractor("docling"))
	return ing.OCRCacheKey(content)
}

// TestOpenFile_OCRCacheKey_IdentityAwareHit is the core #488 parity case: a PDF's
// OCR text, written by ingest at its identity-folded key, is FOUND by open_file
// once the active OCR identity is plumbed in — and is MISSED with the old
// bytes-only key (no identity), reproducing the bug.
func TestOpenFile_OCRCacheKey_IdentityAwareHit(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	pdfBytes := []byte("%PDF-1.4 identity-aware body")
	fs := &fakeCorpusFS{objects: map[string][]byte{"docs/a.pdf": pdfBytes}}

	ocrText := "# Extracted\n\nARRANGEMENT OF SECTIONS\n\n1. Short title."
	cacheDir := filepath.Join(stateDir, "cache", "ocr")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir ocr cache: %v", err)
	}
	key := ingestOCRCacheKey(t, stateDir, pdfBytes)
	if err := os.WriteFile(filepath.Join(cacheDir, key+".md"), []byte(ocrText), 0o644); err != nil {
		t.Fatalf("write ocr cache: %v", err)
	}

	// Without the active identity, retrieval keys on the bytes alone and MISSES
	// the identity-folded entry ingest wrote — the pre-fix bug.
	miss := retrieval.NewService(nil, nil, nil, nil)
	miss.SetRootDir(root)
	miss.SetStateDir(stateDir)
	miss.SetCorpusFS(fs)
	if _, err := miss.OpenFile(context.Background(), "docs/a.pdf", model.Span{}, 20000); !errors.Is(err, model.ErrOCRNotReady) {
		t.Fatalf("bytes-only lookup should miss the identity-folded entry, got err=%v", err)
	}

	// With the active OCR identity plumbed in (docling ⇒ provider-only identity,
	// SPEC §8.6.7), retrieval reconstructs ingest's key and HITS.
	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)
	svc.SetDerivationCacheIdentities("ocr|docling|||", "")

	out, err := svc.OpenFile(context.Background(), "docs/a.pdf", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile with active OCR identity returned err: %v", err)
	}
	if out != ocrText {
		t.Fatalf("expected cached OCR text, got %q", out)
	}
}

// TestOpenFile_OCRCacheKey_DifferentIdentityMisses locks the no-cross-identity
// bleed: an entry written under one OCR identity must NOT be returned when a
// different identity is active (a model/provider swap the re-ingest gate treats
// as stale).
func TestOpenFile_OCRCacheKey_DifferentIdentityMisses(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	pdfBytes := []byte("%PDF-1.4 mismatch body")
	fs := &fakeCorpusFS{objects: map[string][]byte{"docs/b.pdf": pdfBytes}}

	cacheDir := filepath.Join(stateDir, "cache", "ocr")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir ocr cache: %v", err)
	}
	key := ingestOCRCacheKey(t, stateDir, pdfBytes)
	if err := os.WriteFile(filepath.Join(cacheDir, key+".md"), []byte("# docling output"), 0o644); err != nil {
		t.Fatalf("write ocr cache: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)
	// A DIFFERENT active identity (mistral, not docling) must not read docling's
	// cached output.
	svc.SetDerivationCacheIdentities("ocr|mistral|pixtral||", "")

	if _, err := svc.OpenFile(context.Background(), "docs/b.pdf", model.Span{}, 20000); !errors.Is(err, model.ErrOCRNotReady) {
		t.Fatalf("different-identity lookup must miss, got err=%v", err)
	}
}

// TestOpenFile_TranscriptCacheKey_IdentityAwareHit is the transcript analogue:
// ingest transcribes an audio file, writing the transcript at its STT
// identity-folded key (incl. the language suffix); open_file returns it once the
// active transcript identity is plumbed in.
func TestOpenFile_TranscriptCacheKey_IdentityAwareHit(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	audioBytes := []byte("fake-audio-bytes")
	fs := &fakeCorpusFS{objects: map[string][]byte{"media/talk.mp3": audioBytes}}

	const transcript = "[00:00] hello from the cached transcript"

	// Drive the real ingest transcript pipeline so the cache file lands at
	// ingest's authoritative identity-aware key.
	cfg := config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}
	ing, err := ingest.NewService(cfg, &fakeKeyStore{})
	if err != nil {
		t.Fatalf("ingest.NewService: %v", err)
	}
	ing.SetTranscriber(&fakeTranscriber{text: transcript})
	ing.SetSTTIdentity("whisper", "whisper-large-v3")
	ing.SetTranscriptLanguage("en")
	doc := model.Document{RelPath: "media/talk.mp3"}
	if _, err := ing.ReadOrComputeTranscript(context.Background(), doc, audioBytes, "en"); err != nil {
		t.Fatalf("ReadOrComputeTranscript: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)
	svc.SetDerivationCacheIdentities("", "stt|whisper|whisper-large-v3||en")

	out, err := svc.OpenFile(context.Background(), "media/talk.mp3", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile with active transcript identity returned err: %v", err)
	}
	if out != transcript {
		t.Fatalf("expected cached transcript, got %q", out)
	}

	// A different STT identity must miss the cached transcript.
	miss := retrieval.NewService(nil, nil, nil, nil)
	miss.SetRootDir(root)
	miss.SetStateDir(stateDir)
	miss.SetCorpusFS(fs)
	miss.SetDerivationCacheIdentities("", "stt|whisper|whisper-large-v2||en")
	if _, err := miss.OpenFile(context.Background(), "media/talk.mp3", model.Span{}, 20000); !errors.Is(err, model.ErrOCRNotReady) {
		t.Fatalf("different STT identity must miss, got err=%v", err)
	}
}

// TestOpenFile_OCRCacheKey_SkipsObjectGETWhenStoreHasHash locks the #488 perf
// path: when the store can report the base content hash (ocrSourceHashProvider),
// open_file derives the identity-aware key WITHOUT streaming the object — zero
// CorpusFS Open calls — and still returns the cached text.
func TestOpenFile_OCRCacheKey_SkipsObjectGETWhenStoreHasHash(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	pdfBytes := []byte("%PDF-1.4 perf body")
	fs := &countingCorpusFS{objects: map[string][]byte{"docs/c.pdf": pdfBytes}}

	ocrText := "# No-GET OCR text"
	cacheDir := filepath.Join(stateDir, "cache", "ocr")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir ocr cache: %v", err)
	}
	key := ingestOCRCacheKey(t, stateDir, pdfBytes)
	if err := os.WriteFile(filepath.Join(cacheDir, key+".md"), []byte(ocrText), 0o644); err != nil {
		t.Fatalf("write ocr cache: %v", err)
	}

	store := &fakeKeyStore{baseHash: ingest.ComputeContentHash(pdfBytes)}
	svc := retrieval.NewService(store, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)
	svc.SetDerivationCacheIdentities("ocr|docling|||", "")

	out, err := svc.OpenFile(context.Background(), "docs/c.pdf", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile (store-hash path) returned err: %v", err)
	}
	if out != ocrText {
		t.Fatalf("expected cached OCR text, got %q", out)
	}
	if fs.opens != 0 {
		t.Fatalf("expected zero object GETs when the store supplies the base hash, got %d", fs.opens)
	}
}

// TestOpenFile_OCRCacheKey_WhitespacePaddedIdentityHits locks the FIX-1 trim:
// SetDerivationCacheIdentities must TrimSpace its args so a caller that passes a
// whitespace-padded identity still reconstructs ingest's byte-identical key and
// HITS the cache, instead of silently missing on the stray whitespace.
func TestOpenFile_OCRCacheKey_WhitespacePaddedIdentityHits(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	pdfBytes := []byte("%PDF-1.4 whitespace body")
	fs := &fakeCorpusFS{objects: map[string][]byte{"docs/w.pdf": pdfBytes}}

	ocrText := "# Padded identity OCR text"
	cacheDir := filepath.Join(stateDir, "cache", "ocr")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir ocr cache: %v", err)
	}
	key := ingestOCRCacheKey(t, stateDir, pdfBytes)
	if err := os.WriteFile(filepath.Join(cacheDir, key+".md"), []byte(ocrText), 0o644); err != nil {
		t.Fatalf("write ocr cache: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)
	// Same active OCR identity as the hit case, but padded with leading/trailing
	// whitespace — the setter must trim it to stay byte-identical to ingest's key.
	svc.SetDerivationCacheIdentities("  ocr|docling|||\n", "\t")

	out, err := svc.OpenFile(context.Background(), "docs/w.pdf", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile with whitespace-padded identity returned err: %v", err)
	}
	if out != ocrText {
		t.Fatalf("expected cached OCR text with padded identity, got %q", out)
	}
}

// TestOpenFile_OCRCacheKey_MalformedStoreHashFallsBack locks the FIX-2 validation:
// a store that reports a non-sha256 base hash (wrong shape) must NOT be trusted —
// open_file falls back to hashing the source bytes (a real object GET) and still
// reconstructs ingest's key and HITS, instead of folding the bogus hash and
// regressing to OCR_NOT_READY.
func TestOpenFile_OCRCacheKey_MalformedStoreHashFallsBack(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	pdfBytes := []byte("%PDF-1.4 malformed-hash body")
	fs := &countingCorpusFS{objects: map[string][]byte{"docs/m.pdf": pdfBytes}}

	ocrText := "# Fallback OCR text"
	cacheDir := filepath.Join(stateDir, "cache", "ocr")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir ocr cache: %v", err)
	}
	key := ingestOCRCacheKey(t, stateDir, pdfBytes)
	if err := os.WriteFile(filepath.Join(cacheDir, key+".md"), []byte(ocrText), 0o644); err != nil {
		t.Fatalf("write ocr cache: %v", err)
	}

	// The store reports a bogus, non-sha256 base hash: open_file must reject its
	// shape and hash the bytes instead.
	store := &fakeKeyStore{baseHash: "not-a-valid-sha256-hash"}
	svc := retrieval.NewService(store, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)
	svc.SetDerivationCacheIdentities("ocr|docling|||", "")

	out, err := svc.OpenFile(context.Background(), "docs/m.pdf", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile (malformed store-hash fallback) returned err: %v", err)
	}
	if out != ocrText {
		t.Fatalf("expected cached OCR text via byte-hash fallback, got %q", out)
	}
	if fs.opens == 0 {
		t.Fatalf("expected a byte-hashing object GET when the store hash is malformed, got %d", fs.opens)
	}
}

// fakeTranscriber is a minimal model.Transcriber returning a fixed transcript.
type fakeTranscriber struct {
	text string
}

func (f *fakeTranscriber) Transcribe(context.Context, string, []byte) (string, error) {
	return f.text, nil
}
