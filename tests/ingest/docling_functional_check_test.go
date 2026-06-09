package tests

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// makeDoclingStub writes an executable named "docling" (so it is treated as a
// real docling binary) that ignores its arguments and exits with exitCode. It
// returns the absolute path. Each call uses a fresh temp dir, so the per-binary
// functional-check cache never collides across tests.
func makeDoclingStub(t *testing.T, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "docling")
	script := "#!/bin/sh\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write docling stub: %v", err)
	}
	return path
}

func TestDescribeDocumentExtractor_AutoSelectsFunctionalDocling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX stub script")
	}
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor: "auto",
		DoclingCommand:  makeDoclingStub(t, 0),
	})
	if d.Name != "docling" || d.Source != "auto" {
		t.Fatalf("functional docling should be selected under auto: got name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
}

func TestDescribeDocumentExtractor_AutoSkipsBrokenDocling(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX stub script")
	}
	// No Mistral credential and no serve URL: a broken docling under auto must
	// not be selected, leaving no extractor (rather than failing every doc).
	t.Setenv("MISTRAL_API_KEY", "")
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor: "auto",
		DoclingCommand:  makeDoclingStub(t, 1),
	})
	if d.Name == "docling" {
		t.Fatalf("broken docling must not be selected under auto: got name=%q (%s)", d.Name, d.Reason)
	}
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("expected disabled with no fallback available: got name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
	if !strings.Contains(d.Reason, "functional check") {
		t.Errorf("reason should explain the functional-check failure: %q", d.Reason)
	}
}

func TestDescribeDocumentExtractor_AutoBrokenDoclingFallsBackToMistral(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX stub script")
	}
	t.Setenv("MISTRAL_API_KEY", "fake-key")
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor: "auto",
		DoclingCommand:  makeDoclingStub(t, 1),
	})
	if d.Name != "mistral-ocr" || d.Source != "fallback" {
		t.Fatalf("broken docling under auto should fall back to Mistral: got name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
}

func TestDescribeDocumentExtractor_ExplicitBrokenDoclingDisables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX stub script")
	}
	// Even with a Mistral credential present, explicit docling must NOT silently
	// fall back — a present-but-broken command disables extraction.
	t.Setenv("MISTRAL_API_KEY", "fake-key")
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor: "docling",
		DoclingCommand:  makeDoclingStub(t, 1),
	})
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("explicit broken docling should be disabled (no fallback): got name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
	if !strings.Contains(d.Reason, "functional check") {
		t.Errorf("reason should explain the functional-check failure: %q", d.Reason)
	}
}

func TestDescribeDocumentExtractor_ExplicitFunctionalDoclingSelected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX stub script")
	}
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor: "docling",
		DoclingCommand:  makeDoclingStub(t, 0),
	})
	if d.Name != "docling" || d.Source != "explicit" {
		t.Fatalf("functional docling should be selected: got name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
}

func TestDescribeDocumentExtractor_CustomNonDoclingCommandNotProbed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command test")
	}
	// A custom command whose basename is not "docling" is not functional-checked
	// (it may not understand --version); it stays available if configured, as
	// before. "/bin/false" would fail a --version probe, but is not probed here.
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor: "docling",
		DoclingCommand:  "/bin/false {input}",
	})
	if d.Name != "docling" || d.Source != "explicit" {
		t.Fatalf("non-docling custom command must not be probed: got name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
}
