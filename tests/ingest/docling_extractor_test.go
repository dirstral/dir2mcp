package tests

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

func TestDoclingExtractor_Extract_UsesConfiguredCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	extractor := ingest.NewDoclingExtractor("cat {input}")
	out, err := extractor.Extract(context.Background(), "sample.txt", []byte("hello docling"))
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if strings.TrimSpace(out) != "hello docling" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestDocumentExtractorFromConfig_PrefersDoclingCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	// A Mistral credential is present (resolves the OCR profile via the
	// built-in ${MISTRAL_API_KEY} placeholder post clean-break #38), yet
	// the configured docling command must still take precedence.
	t.Setenv("MISTRAL_API_KEY", "test-key")
	cfg := config.Config{
		DoclingCommand: "cat {input}",
	}

	extractor := ingest.DocumentExtractorFromConfig(cfg)
	if extractor == nil {
		t.Fatalf("expected extractor")
	}
	out, err := extractor.Extract(context.Background(), "sample.txt", []byte("docling preferred"))
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if strings.TrimSpace(out) != "docling preferred" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestDescribeDocumentExtractor_DoclingServe(t *testing.T) {
	// Explicit docling-serve with a URL is selected (reachability is a doctor
	// concern, not a Describe concern).
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor:       "docling-serve",
		IngestDoclingServeURL: "http://127.0.0.1:5001",
	})
	if d.Name != "docling-serve" || d.Source != "explicit" {
		t.Fatalf("explicit docling-serve: got name=%q source=%q", d.Name, d.Source)
	}

	// Explicit docling-serve with an empty URL is disabled (no silent CLI
	// fallback) per spec 0.10.0 §7.4.B.
	d = ingest.DescribeDocumentExtractor(config.Config{IngestExtractor: "docling-serve"})
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("docling-serve without URL should be disabled: got name=%q source=%q", d.Name, d.Source)
	}
}

func TestDescribeDocumentExtractor_AutoUsesServeURLWhenNoCLI(t *testing.T) {
	if _, err := exec.LookPath("docling"); err == nil {
		t.Skip("docling CLI present on PATH; auto would prefer it")
	}
	t.Setenv("MISTRAL_API_KEY", "")
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor:       "auto",
		IngestDoclingServeURL: "http://127.0.0.1:5001",
	})
	if d.Name != "docling-serve" {
		t.Fatalf("auto with serve_url and no CLI: got name=%q (%s)", d.Name, d.Reason)
	}
}

func TestDocumentExtractorFromConfig_DoclingServe(t *testing.T) {
	ext := ingest.DocumentExtractorFromConfig(config.Config{
		IngestExtractor:       "docling-serve",
		IngestDoclingServeURL: "http://127.0.0.1:5001",
	})
	if ext == nil {
		t.Fatal("expected a docling-serve extractor")
	}
	// Empty URL -> no extractor (disabled), so up.go can reject the combination.
	if ingest.DocumentExtractorFromConfig(config.Config{IngestExtractor: "docling-serve"}) != nil {
		t.Fatal("docling-serve with empty URL should resolve to no extractor")
	}
}
