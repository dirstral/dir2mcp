package tests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// These tests lock the END-TO-END wiring for issue #488. #517 gave the retriever
// SetDerivationCacheIdentities but left it UNCALLED, so open_file still defaulted
// to the bytes-only cache key and missed every identity-aware entry ingest wrote.
// The CLI now passes ingest's ACTIVE OCR/transcript identities into the retriever
// at construction; these tests prove the identities the retriever receives —
// straight from ingest's exported getters (ActiveOCRIdentity/ActiveTranscriptIdentity)
// and the config helper (ActiveDerivationIdentities) — reconstruct the exact key
// ingest's writer folded, so open_file HITS.

// TestOpenFile_DerivationIdentityWiring_OCR is the core wiring case: an OCR entry
// ingest wrote at its identity-folded key is FOUND by open_file when the identity
// is sourced straight from ingest's ActiveOCRIdentity getter (the daemon path's
// wiring), and MISSED with the pre-wiring bytes-only key.
func TestOpenFile_DerivationIdentityWiring_OCR(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	pdfBytes := []byte("%PDF-1.4 wiring body")
	fs := &fakeCorpusFS{objects: map[string][]byte{"docs/a.pdf": pdfBytes}}

	cfg := config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}
	ing, err := ingest.NewService(cfg, &fakeKeyStore{})
	if err != nil {
		t.Fatalf("ingest.NewService: %v", err)
	}
	ing.SetDocumentExtractor(ingest.NewDoclingExtractor("docling"))

	// The exported getter must equal the identity that reconstructs ingest's key,
	// and (STT off) the transcript getter must be empty so the retriever does not
	// fold a spurious transcript identity for this OCR corpus.
	if got := ing.ActiveOCRIdentity(); got != "ocr|docling|||" {
		t.Fatalf("ActiveOCRIdentity = %q, want %q", got, "ocr|docling|||")
	}
	if got := ing.ActiveTranscriptIdentity(); got != "" {
		t.Fatalf("ActiveTranscriptIdentity with STT off = %q, want empty", got)
	}

	// ingest writes the OCR cache at its identity-aware key.
	ocrText := "# wired OCR text"
	cacheDir := filepath.Join(stateDir, "cache", "ocr")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("mkdir ocr cache: %v", err)
	}
	key := ing.OCRCacheKey(pdfBytes)
	if err := os.WriteFile(filepath.Join(cacheDir, key+".md"), []byte(ocrText), 0o644); err != nil {
		t.Fatalf("write ocr cache: %v", err)
	}

	// THE WIRING under test: identities come straight from ingest's getters, as the
	// CLI now plumbs them into the retriever.
	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)
	svc.SetDerivationCacheIdentities(ing.ActiveOCRIdentity(), ing.ActiveTranscriptIdentity())

	out, err := svc.OpenFile(context.Background(), "docs/a.pdf", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile after wiring returned err: %v", err)
	}
	if out != ocrText {
		t.Fatalf("expected cached OCR text, got %q", out)
	}

	// Sanity: an UNWIRED retriever (the pre-#488 default) keys on the bytes alone
	// and MISSES the identity-folded entry ingest wrote.
	miss := retrieval.NewService(nil, nil, nil, nil)
	miss.SetRootDir(root)
	miss.SetStateDir(stateDir)
	miss.SetCorpusFS(fs)
	if _, err := miss.OpenFile(context.Background(), "docs/a.pdf", model.Span{}, 20000); !errors.Is(err, model.ErrOCRNotReady) {
		t.Fatalf("unwired (bytes-only) lookup should miss, got err=%v", err)
	}
}

// TestOpenFile_DerivationIdentityWiring_Transcript is the transcript analogue:
// a transcript ingest wrote at its STT identity-folded key is FOUND by open_file
// when the identity is sourced from ingest's ActiveTranscriptIdentity getter.
func TestOpenFile_DerivationIdentityWiring_Transcript(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")

	audioBytes := []byte("fake-audio-bytes")
	fs := &fakeCorpusFS{objects: map[string][]byte{"media/talk.mp3": audioBytes}}

	const transcript = "[00:00] hello from the wired transcript"

	cfg := config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}
	ing, err := ingest.NewService(cfg, &fakeKeyStore{})
	if err != nil {
		t.Fatalf("ingest.NewService: %v", err)
	}
	ing.SetTranscriber(&fakeTranscriber{text: transcript})
	ing.SetSTTIdentity("whisper", "whisper-large-v3")
	ing.SetTranscriptLanguage("en")

	if got := ing.ActiveTranscriptIdentity(); got != "stt|whisper|whisper-large-v3||en" {
		t.Fatalf("ActiveTranscriptIdentity = %q, want %q", got, "stt|whisper|whisper-large-v3||en")
	}

	doc := model.Document{RelPath: "media/talk.mp3"}
	if _, err := ing.ReadOrComputeTranscript(context.Background(), doc, audioBytes, "en"); err != nil {
		t.Fatalf("ReadOrComputeTranscript: %v", err)
	}

	svc := retrieval.NewService(nil, nil, nil, nil)
	svc.SetRootDir(root)
	svc.SetStateDir(stateDir)
	svc.SetCorpusFS(fs)
	svc.SetDerivationCacheIdentities(ing.ActiveOCRIdentity(), ing.ActiveTranscriptIdentity())

	out, err := svc.OpenFile(context.Background(), "media/talk.mp3", model.Span{}, 20000)
	if err != nil {
		t.Fatalf("OpenFile after wiring returned err: %v", err)
	}
	if out != transcript {
		t.Fatalf("expected cached transcript, got %q", out)
	}

	// Sanity: an unwired retriever misses the identity-folded transcript.
	miss := retrieval.NewService(nil, nil, nil, nil)
	miss.SetRootDir(root)
	miss.SetStateDir(stateDir)
	miss.SetCorpusFS(fs)
	if _, err := miss.OpenFile(context.Background(), "media/talk.mp3", model.Span{}, 20000); !errors.Is(err, model.ErrOCRNotReady) {
		t.Fatalf("unwired (bytes-only) transcript lookup should miss, got err=%v", err)
	}
}

// TestActiveDerivationIdentities_MatchesServiceGetters locks the ask-path helper
// to the exact recipe the CLI builds its ingestor with: for any cfg,
// ingest.ActiveDerivationIdentities(cfg) must return byte-identical strings to a
// full Service constructed from the same cfg (NewService + the config extractor).
// The ask CLI has no ingest Service, so it relies on this helper being a faithful
// stand-in — a drift here would silently reintroduce the #488 miss on `ask`.
func TestActiveDerivationIdentities_MatchesServiceGetters(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".dir2mcp")
	cfg := config.Config{RootDir: root, StateDir: stateDir, STTProvider: "off"}

	// Build a Service exactly as the CLI's default ingestor does: NewService plus
	// the config-resolved document extractor.
	recipe, err := ingest.NewService(cfg, &fakeKeyStore{})
	if err != nil {
		t.Fatalf("ingest.NewService: %v", err)
	}
	if ex := ingest.DocumentExtractorFromConfig(cfg); ex != nil {
		recipe.SetDocumentExtractor(ex)
	}
	wantOCR := recipe.ActiveOCRIdentity()
	wantTranscript := recipe.ActiveTranscriptIdentity()

	gotOCR, gotTranscript := ingest.ActiveDerivationIdentities(cfg)
	if gotOCR != wantOCR {
		t.Fatalf("ActiveDerivationIdentities OCR = %q, want %q", gotOCR, wantOCR)
	}
	if gotTranscript != wantTranscript {
		t.Fatalf("ActiveDerivationIdentities transcript = %q, want %q", gotTranscript, wantTranscript)
	}
}
