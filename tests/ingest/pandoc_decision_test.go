package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// writePandocStub writes an executable script named exactly `name` and returns its
// path. Naming it "pandoc" makes the functional check probe it.
func writePandocStub(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return p
}

// TestDescribeDocumentExtractor_PandocExplicitAvailable pins that ingest.extractor=
// pandoc with a functional pandoc resolves to an explicit pandoc decision, and the
// builder returns a non-nil extractor in lockstep.
func TestDescribeDocumentExtractor_PandocExplicitAvailable(t *testing.T) {
	stub := writePandocStub(t, "pandoc", "exit 0")
	cfg := config.Config{IngestExtractor: "pandoc", IngestPandocCommand: stub}
	d := ingest.DescribeDocumentExtractor(cfg)
	if d.Name != "pandoc" || d.Source != "explicit" {
		t.Fatalf("describe = %s/%s, want pandoc/explicit (reason %q)", d.Name, d.Source, d.Reason)
	}
	if ex := ingest.DocumentExtractorFromConfig(cfg); ex == nil {
		t.Error("builder returned nil for an available pandoc pin")
	}
}

// TestDescribeDocumentExtractor_PandocExplicitUnavailable pins that a present-but-
// broken pandoc (non-zero --version) disables extraction — no silent fallback.
func TestDescribeDocumentExtractor_PandocExplicitUnavailable(t *testing.T) {
	stub := writePandocStub(t, "pandoc", "exit 1")
	d := ingest.DescribeDocumentExtractor(config.Config{IngestExtractor: "pandoc", IngestPandocCommand: stub})
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("describe = %s/%s, want disabled", d.Name, d.Source)
	}
}

// TestDescribeDocumentExtractor_AutoOnlyPandoc pins that under auto, when no
// docling/OCR is available but pandoc is, pandoc becomes the resolved primary so
// born-digital formats stay covered.
func TestDescribeDocumentExtractor_AutoOnlyPandoc(t *testing.T) {
	if _, err := exec.LookPath("docling"); err == nil {
		t.Skip("docling CLI present on PATH; auto would select it over pandoc")
	}
	t.Setenv("MISTRAL_API_KEY", "") // no OCR credential, so the cascade bottoms out
	stub := writePandocStub(t, "pandoc", "exit 0")
	cfg := config.Config{IngestExtractor: "auto", IngestPandocCommand: stub}
	d := ingest.DescribeDocumentExtractor(cfg)
	if d.Name != "pandoc" || d.Source != "auto" {
		t.Fatalf("auto-only-pandoc describe = %s/%s, want pandoc/auto (reason %q)", d.Name, d.Source, d.Reason)
	}
	if ex := ingest.DocumentExtractorFromConfig(cfg); ex == nil {
		t.Error("builder returned nil when pandoc is the only available auto engine")
	}
}
