package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// TestDoclingExtractor_Extract_FileOutputPath covers the {output} path (issue
// #376): docling does not stream to stdout — it writes a file into the directory
// given to --output. A template containing {output} must therefore run the
// command, then read the produced file back, rather than reading stdout. `cp
// {input} {output}` simulates docling: cp into a directory keeps the input
// basename, so the extractor reads that file's contents.
func TestDoclingExtractor_Extract_FileOutputPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	extractor := ingest.NewDoclingExtractor("cp {input} {output}")
	out, err := extractor.Extract(context.Background(), "sample.md", []byte("hello from file output"))
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if strings.TrimSpace(out) != "hello from file output" {
		t.Fatalf("unexpected output: %q", out)
	}
}

// TestDoclingExtractor_Extract_FileOutputEmpty verifies that a {output} command
// which writes nothing surfaces the empty-output error (issue #376) instead of
// silently succeeding — `true` runs successfully but leaves the output dir empty.
func TestDoclingExtractor_Extract_FileOutputEmpty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	extractor := ingest.NewDoclingExtractor("true {input} {output}")
	_, err := extractor.Extract(context.Background(), "sample.md", []byte("data"))
	if err == nil || !strings.Contains(err.Error(), "empty output") {
		t.Fatalf("expected empty-output error, got: %v", err)
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
	healthyURL := healthyDoclingServeURL(t)

	// Explicit docling-serve with a reachable URL is selected.
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor:       "docling-serve",
		IngestDoclingServeURL: healthyURL,
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

	// Explicit docling-serve with an unreachable URL is unavailable.
	d = ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor:       "docling-serve",
		IngestDoclingServeURL: unreachableDoclingServeURL(t),
	})
	if d.Name != "" || d.Source != "disabled" {
		t.Fatalf("docling-serve with unreachable URL should be disabled: got name=%q source=%q", d.Name, d.Source)
	}
}

func TestDescribeDocumentExtractor_AutoUsesServeURLWhenNoCLI(t *testing.T) {
	if _, err := exec.LookPath("docling"); err == nil {
		t.Skip("docling CLI present on PATH; auto would prefer it")
	}
	t.Setenv("MISTRAL_API_KEY", "")
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor:       "auto",
		IngestDoclingServeURL: healthyDoclingServeURL(t),
	})
	if d.Name != "docling-serve" {
		t.Fatalf("auto with serve_url and no CLI: got name=%q (%s)", d.Name, d.Reason)
	}
}

func TestDescribeDocumentExtractor_AutoPrefersServeOverMistral(t *testing.T) {
	if _, err := exec.LookPath("docling"); err == nil {
		t.Skip("docling CLI present on PATH; auto would prefer it")
	}
	t.Setenv("MISTRAL_API_KEY", "fake-key")
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor:       "auto",
		IngestDoclingServeURL: healthyDoclingServeURL(t),
	})
	if d.Name != "docling-serve" {
		t.Fatalf("auto with serve_url and Mistral credential: got name=%q (%s)", d.Name, d.Reason)
	}
}

func TestDescribeDocumentExtractor_AutoFallsBackWhenServeUnreachable(t *testing.T) {
	if _, err := exec.LookPath("docling"); err == nil {
		t.Skip("docling CLI present on PATH; auto would prefer it")
	}
	t.Setenv("MISTRAL_API_KEY", "fake-key")
	d := ingest.DescribeDocumentExtractor(config.Config{
		IngestExtractor:       "auto",
		IngestDoclingServeURL: unreachableDoclingServeURL(t),
	})
	if d.Name != "mistral-ocr" || d.Source != "fallback" {
		t.Fatalf("auto with unreachable serve_url should fall back to Mistral: got name=%q source=%q (%s)", d.Name, d.Source, d.Reason)
	}
}

func TestDocumentExtractorFromConfig_DoclingServe(t *testing.T) {
	healthyURL := healthyDoclingServeURL(t)
	ext := ingest.DocumentExtractorFromConfig(config.Config{
		IngestExtractor:       "docling-serve",
		IngestDoclingServeURL: healthyURL,
	})
	if ext == nil {
		t.Fatal("expected a docling-serve extractor")
	}
	// Empty URL -> no extractor (disabled), so up.go can reject the combination.
	if ingest.DocumentExtractorFromConfig(config.Config{IngestExtractor: "docling-serve"}) != nil {
		t.Fatal("docling-serve with empty URL should resolve to no extractor")
	}
	if ingest.DocumentExtractorFromConfig(config.Config{
		IngestExtractor:       "docling-serve",
		IngestDoclingServeURL: unreachableDoclingServeURL(t),
	}) != nil {
		t.Fatal("docling-serve with unreachable URL should resolve to no extractor")
	}
}

func healthyDoclingServeURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func unreachableDoclingServeURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	url := srv.URL
	srv.Close()
	return url
}
