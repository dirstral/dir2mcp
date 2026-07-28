package ingest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
)

// TestManifestErrorCode pins the §14.4 classification: translation, OCR, and
// transcript provider failures each map to their distinct canonical code, and
// any other failure falls back to the generic EXTRACT_FAILED. The sentinels are
// matched through fmt.Errorf %w wrapping (as the real failure sites wrap them).
func TestManifestErrorCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "translate provider failure -> TRANSLATE_FAILED",
			err:  fmt.Errorf("%w: translate transcript into %q: %w", ErrTranslateProviderFailure, "en", errors.New("chat down")),
			want: manifestErrTranslateFailed,
		},
		{
			name: "ocr provider failure -> OCR_FAILED",
			err:  fmt.Errorf("%w: document extract %s: %w", ErrOCRProviderFailure, "scan.pdf", errors.New("ocr down")),
			want: manifestErrOCRFailed,
		},
		{
			name: "transcript provider failure -> TRANSCRIBE_FAILED",
			err:  fmt.Errorf("%w: transcribe %s: %w", ErrTranscriptProviderFailure, "talk.mp3", errors.New("stt down")),
			want: manifestErrTranscribeFailed,
		},
		{
			name: "other failure -> EXTRACT_FAILED",
			err:  errors.New("some generic derivation failure"),
			want: manifestErrExtractFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := manifestErrorCode(tc.err); got != tc.want {
				t.Fatalf("manifestErrorCode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestManifestErrorCode_FileTooLarge proves an over-cap file error (wrapped
// ErrFileTooLarge, as the size-check sites wrap it) maps to the canonical §14.4
// FILE_TOO_LARGE code rather than the generic EXTRACT_FAILED.
func TestManifestErrorCode_FileTooLarge(t *testing.T) {
	err := fmt.Errorf("%w: file %s too large (%d bytes); limit %d", ErrFileTooLarge, "big.txt", 20_000_000, 10_485_760)
	if got := manifestErrorCode(err); got != manifestErrFileTooLarge {
		t.Fatalf("manifestErrorCode = %q, want %q", got, manifestErrFileTooLarge)
	}
}

// TestGenerateRawTextFromContent_OversizeTaggedFileTooLarge proves the raw-text
// size check wraps ErrFileTooLarge, so an oversize document is classified
// FILE_TOO_LARGE on the run manifest.
func TestGenerateRawTextFromContent_OversizeTaggedFileTooLarge(t *testing.T) {
	rg := &RepresentationGenerator{} // size check runs before any store access
	content := make([]byte, defaultMaxFileSizeBytes+1)
	err := rg.GenerateRawTextFromContent(context.Background(), model.Document{RelPath: "big.txt"}, content)
	if err == nil {
		t.Fatal("expected an oversize error, got nil")
	}
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("error not tagged ErrFileTooLarge: %v", err)
	}
	if got := manifestErrorCode(err); got != manifestErrFileTooLarge {
		t.Fatalf("manifestErrorCode = %q, want %q", got, manifestErrFileTooLarge)
	}
}

// TestAssetOutcome_BinarySkippedCode proves a binary-skip records a skipped
// manifest outcome carrying the canonical §14.4 BINARY_SKIPPED code, so a
// non-textual binary is machine-visible in the run manifest.
func TestAssetOutcome_BinarySkippedCode(t *testing.T) {
	o := newAssetOutcome("blob.bin")
	o.markSkippedWithCode(manifestErrBinarySkipped)
	rec := o.record("")
	if rec.Status != batchStatusSkipped {
		t.Fatalf("status = %q, want %q", rec.Status, batchStatusSkipped)
	}
	if rec.ErrorCode != manifestErrBinarySkipped {
		t.Fatalf("error_code = %q, want %q", rec.ErrorCode, manifestErrBinarySkipped)
	}
	// A recorded error must win over a later skip signal (no silent downgrade).
	o2 := newAssetOutcome("blob.bin")
	o2.markErrorIfUnset(manifestErrExtractFailed, "boom")
	o2.markSkippedWithCode(manifestErrBinarySkipped)
	if got := o2.record(""); got.Status != batchStatusError || got.ErrorCode != manifestErrExtractFailed {
		t.Fatalf("error must win: status=%q code=%q", got.Status, got.ErrorCode)
	}
}

// failingExtractor is a model.DocumentExtractor whose Extract always errors,
// simulating an OCR provider/transport failure.
type failingExtractor struct{}

func (failingExtractor) Extract(_ context.Context, _ string, _ []byte) (string, error) {
	return "", errors.New("ocr provider unavailable")
}

// TestReadOrComputeOCR_ProviderFailureTaggedOCRFailed proves the OCR compute path
// tags a provider/transport failure with ErrOCRProviderFailure, so the run
// manifest classifies it OCR_FAILED (§14.4) rather than the generic
// EXTRACT_FAILED.
func TestReadOrComputeOCR_ProviderFailureTaggedOCRFailed(t *testing.T) {
	s := &Service{
		cfg:       config.Config{StateDir: t.TempDir()},
		extractor: failingExtractor{},
	}
	_, err := s.readOrComputeOCR(context.Background(), model.Document{RelPath: "scan.pdf"}, []byte("bytes"))
	if err == nil {
		t.Fatal("expected an OCR provider failure, got nil")
	}
	if !errors.Is(err, ErrOCRProviderFailure) {
		t.Fatalf("error not tagged ErrOCRProviderFailure: %v", err)
	}
	if got := manifestErrorCode(err); got != manifestErrOCRFailed {
		t.Fatalf("manifestErrorCode = %q, want %q", got, manifestErrOCRFailed)
	}
}

// failingGenerator is a model.Generator whose Generate always errors, simulating
// a chat translation provider failure.
type failingGenerator struct{}

func (failingGenerator) Generate(_ context.Context, _ string) (string, error) {
	return "", errors.New("chat provider unavailable")
}

// TestTranslateOneTranscript_ProviderFailureTaggedTranslateFailed proves the
// translation path tags a provider/transport failure with
// ErrTranslateProviderFailure, so the run manifest classifies it TRANSLATE_FAILED
// (§14.4) — distinct from the transcript's TRANSCRIBE_FAILED.
func TestTranslateOneTranscript_ProviderFailureTaggedTranslateFailed(t *testing.T) {
	s := &Service{
		cfg:        config.Config{StateDir: t.TempDir()},
		translator: failingGenerator{},
	}
	doc := model.Document{RelPath: "audio/talk.mp3", DocType: "audio"}
	err := s.translateOneTranscript(context.Background(), doc, []byte("audio"), "[00:00] hallo", "de", "en", time.Second, 0, 0)
	if err == nil {
		t.Fatal("expected a translation provider failure, got nil")
	}
	if !errors.Is(err, ErrTranslateProviderFailure) {
		t.Fatalf("error not tagged ErrTranslateProviderFailure: %v", err)
	}
	if got := manifestErrorCode(err); got != manifestErrTranslateFailed {
		t.Fatalf("manifestErrorCode = %q, want %q", got, manifestErrTranslateFailed)
	}
}
