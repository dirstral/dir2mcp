package tests

import (
	"context"
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
	cfg := config.Config{
		MistralAPIKey:  "test-key",
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
