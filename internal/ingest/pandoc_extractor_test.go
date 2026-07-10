package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPandocExtractor_Extract_ReturnsStdoutMarkdown(t *testing.T) {
	// A stub `pandoc` that ignores its args and echoes fixed Markdown to stdout,
	// exactly as pandoc streams `-t gfm` output. No real pandoc binary required.
	dir := t.TempDir()
	stub := filepath.Join(dir, "pandoc")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '# Title\\n\\nBody text.\\n'\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	ex := NewPandocExtractor(stub)
	out, err := ex.Extract(context.Background(), "notes.odt", []byte("irrelevant bytes"))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if want := "# Title\n\nBody text."; out != want {
		t.Errorf("Extract() = %q, want %q (trimmed stdout)", out, want)
	}
}

func TestPandocExtractor_Extract_EmptyOutputIsError(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "pandoc")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	if _, err := NewPandocExtractor(stub).Extract(context.Background(), "empty.odt", []byte("x")); err == nil {
		t.Error("empty pandoc output must be an error, not a silent empty representation")
	}
}

func TestPandocExtractor_Extract_NonZeroExitIsError(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "pandoc")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 'boom' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	_, err := NewPandocExtractor(stub).Extract(context.Background(), "bad.odt", []byte("x"))
	if err == nil {
		t.Fatal("a failing pandoc must return an error")
	}
	// The user-supplied command/path must never appear literally in the diagnostic.
	if strings.Contains(err.Error(), stub) {
		t.Errorf("error %q leaks the pandoc binary path", err.Error())
	}
}

func TestPandocExtractor_Extract_TimesOut(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "pandoc")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	orig := pandocExtractTimeout
	pandocExtractTimeout = 100 * time.Millisecond
	defer func() { pandocExtractTimeout = orig }()
	_, err := NewPandocExtractor(stub).Extract(context.Background(), "slow.odt", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected a timeout error, got %v", err)
	}
}
