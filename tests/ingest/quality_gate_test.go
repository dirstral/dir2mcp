package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestQualityGate_QuarantinesDegenerateTranscript verifies that when the output
// quality gate is enabled, a degenerate transcript (a repetition loop, the
// classic STT hallucination) is quarantined: its chunks are inserted already
// failed (embedding_status=error, error_category=quality_gate) so the embedding
// worker — which only picks up embedding_status='pending' — never embeds them.
func TestQualityGate_QuarantinesDegenerateTranscript(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir, QualityGatesEnabled: true}, st)
	svc.SetTranscriber(&fakeTranscriber{text: strings.Repeat("thank you ", 60)})

	doc := model.Document{DocID: 101, RelPath: "audio/loop.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio-bytes")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation failed: %v", err)
	}

	if len(st.chunks) == 0 {
		t.Fatal("expected transcript chunks to be persisted (quarantined), got none")
	}
	for i, c := range st.chunks {
		if c.EmbeddingStatus != "error" {
			t.Fatalf("chunk %d: expected embedding_status=error, got %q", i, c.EmbeddingStatus)
		}
		if c.EmbeddingStatus == "pending" {
			t.Fatalf("chunk %d: quarantined chunk must not be pending (worker would embed it)", i)
		}
		if c.ErrorCategory != string(store.ErrorCategoryQualityGate) {
			t.Fatalf("chunk %d: expected error_category=%q, got %q", i, store.ErrorCategoryQualityGate, c.ErrorCategory)
		}
		if strings.TrimSpace(c.EmbeddingError) == "" {
			t.Fatalf("chunk %d: expected a content-free embedding_error reason, got empty", i)
		}
	}
}

// TestQualityGate_CleanTranscriptNotQuarantined is the companion case: a clean
// transcript passes the gate, so its chunks are inserted pending and will be
// embedded normally.
func TestQualityGate_CleanTranscriptNotQuarantined(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir, QualityGatesEnabled: true}, st)
	svc.SetTranscriber(&fakeTranscriber{text: "[00:00] welcome to the lecture today\n[00:02] we discuss several distinct topics in depth\n[00:05] including history geography and science across many regions"})

	doc := model.Document{DocID: 102, RelPath: "audio/clean.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio-bytes")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation failed: %v", err)
	}

	if len(st.chunks) == 0 {
		t.Fatal("expected transcript chunks to be persisted, got none")
	}
	for i, c := range st.chunks {
		if c.EmbeddingStatus != "pending" {
			t.Fatalf("chunk %d: expected embedding_status=pending for clean output, got %q", i, c.EmbeddingStatus)
		}
		if c.ErrorCategory != "" {
			t.Fatalf("chunk %d: clean chunk must have no error_category, got %q", i, c.ErrorCategory)
		}
		if c.EmbeddingError != "" {
			t.Fatalf("chunk %d: clean chunk must have no embedding_error, got %q", i, c.EmbeddingError)
		}
	}
}

// TestQualityGate_DisabledViaSetter proves the master switch: with the gate
// disabled (nil), even degenerate output is inserted pending — screening is
// fully skipped, preserving pre-0.16.0 behaviour.
func TestQualityGate_DisabledViaSetter(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir, QualityGatesEnabled: true}, st)
	svc.SetQualityGate(nil) // disable screening
	svc.SetTranscriber(&fakeTranscriber{text: strings.Repeat("thank you ", 60)})

	doc := model.Document{DocID: 103, RelPath: "audio/loop2.mp3", DocType: "audio"}
	if err := svc.GenerateTranscriptRepresentation(context.Background(), doc, []byte("audio-bytes")); err != nil {
		t.Fatalf("GenerateTranscriptRepresentation failed: %v", err)
	}

	if len(st.chunks) == 0 {
		t.Fatal("expected transcript chunks, got none")
	}
	for i, c := range st.chunks {
		if c.EmbeddingStatus != "pending" {
			t.Fatalf("chunk %d: gate disabled, expected pending, got %q", i, c.EmbeddingStatus)
		}
		if c.ErrorCategory != "" {
			t.Fatalf("chunk %d: gate disabled, expected empty error_category, got %q", i, c.ErrorCategory)
		}
		if c.EmbeddingError != "" {
			t.Fatalf("chunk %d: gate disabled, expected empty embedding_error, got %q", i, c.EmbeddingError)
		}
	}
}

// TestQualityGate_QuarantinesDegenerateOCR exercises the OCR flat path: a
// degenerate OCR result is quarantined identically to the transcript path.
func TestQualityGate_QuarantinesDegenerateOCR(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	st := &fakeIngestStore{}
	svc := mustNewIngestService(t, config.Config{StateDir: stateDir, QualityGatesEnabled: true}, st)
	svc.SetOCR(&fakeOCR{text: strings.Repeat("page header ", 80)})

	doc := model.Document{DocID: 104, RelPath: "docs/loop.pdf", DocType: "pdf"}
	if err := svc.GenerateOCRMarkdownRepresentation(context.Background(), doc, []byte("pdf-bytes")); err != nil {
		t.Fatalf("GenerateOCRMarkdownRepresentation failed: %v", err)
	}

	if len(st.chunks) == 0 {
		t.Fatal("expected OCR chunks to be persisted (quarantined), got none")
	}
	for i, c := range st.chunks {
		if c.EmbeddingStatus != "error" {
			t.Fatalf("chunk %d: expected embedding_status=error, got %q", i, c.EmbeddingStatus)
		}
		if c.ErrorCategory != string(store.ErrorCategoryQualityGate) {
			t.Fatalf("chunk %d: expected error_category=%q, got %q", i, store.ErrorCategoryQualityGate, c.ErrorCategory)
		}
	}
}
