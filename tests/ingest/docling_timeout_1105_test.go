package tests

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// Issue #1105: the per-document docling limit comes from
// ingest.docling.timeout_sec, and a timeout error names the document and the
// limit, so an operator can find the document and raise the limit.

// writeSlowDocling writes a fake docling command that sleeps far longer than
// any limit in these tests. The name is not "docling", so the functional
// probe does not run it. exec replaces the shell with sleep, so the kill on
// timeout closes the pipes at once.
func writeSlowDocling(t *testing.T) string {
	t.Helper()
	fake := filepath.Join(testutil.TempDir(t), "slow-docling")
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docling: %v", err)
	}
	return fake
}

func TestDoclingTimeout_ErrorNamesDocumentAndLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	fake := writeSlowDocling(t)

	for _, tpl := range []string{
		fake + " --output {output} {input}", // file-output path
		fake + " {input}",                   // stdout path (no {output})
	} {
		t.Run(tpl[len(fake):], func(t *testing.T) {
			ext := ingest.NewDoclingExtractorWithTimeout(tpl, 300*time.Millisecond)

			start := time.Now()
			_, err := ext.Extract(context.Background(), "reports/annual-tables.pdf", []byte("%PDF-1.4 fake"))
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("expected a timeout error, got nil")
			}
			msg := err.Error()
			for _, want := range []string{"timed out", "reports/annual-tables.pdf", "300ms", "ingest.docling.timeout_sec"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("timeout error must contain %q, got: %v", want, msg)
				}
			}
			if elapsed > 10*time.Second {
				t.Fatalf("command was not stopped promptly by the limit (took %s)", elapsed)
			}
		})
	}
}

// TestDoclingTimeout_ConfigWiring proves DocumentExtractorFromConfig applies
// cfg.IngestDoclingTimeoutSec: a 1-second limit stops the 30-second command
// and the error reports the configured limit.
func TestDoclingTimeout_ConfigWiring(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	fake := writeSlowDocling(t)
	cfg := config.Config{
		DoclingCommand:          fake + " --output {output} {input}",
		IngestDoclingTimeoutSec: 1,
	}

	ext := ingest.DocumentExtractorFromConfig(cfg)
	if ext == nil {
		t.Fatal("expected a docling extractor from the configured command")
	}

	start := time.Now()
	_, err := ext.Extract(context.Background(), "big.pdf", []byte("%PDF-1.4 fake"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "big.pdf") || !strings.Contains(err.Error(), "after 1s") {
		t.Fatalf("timeout error must name the document and the configured 1s limit, got: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("configured limit was not applied (took %s)", elapsed)
	}
}

// TestDoclingTimeout_CallerDeadlineDoesNotBlameLimit checks that a deadline on
// the caller's context is not reported as the per-document limit. The limit
// here is one hour, so only the caller's deadline can stop the command.
func TestDoclingTimeout_CallerDeadlineDoesNotBlameLimit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	fake := writeSlowDocling(t)
	ext := ingest.NewDoclingExtractorWithTimeout(fake+" --output {output} {input}", time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := ext.Extract(ctx, "sample.pdf", []byte("%PDF-1.4 fake"))
	if err == nil {
		t.Fatal("expected an error after the caller deadline, got nil")
	}
	if strings.Contains(err.Error(), "ingest.docling.timeout_sec") {
		t.Fatalf("a caller deadline must not be reported as the per-document limit, got: %v", err)
	}
	if !strings.Contains(err.Error(), "docling command failed") {
		t.Fatalf("expected the generic failure error, got: %v", err)
	}
}

// TestDoclingTimeout_ZeroUsesDefault checks that a zero timeout falls back to
// the default limit: a fast command still succeeds.
func TestDoclingTimeout_ZeroUsesDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	ext := ingest.NewDoclingExtractorWithTimeout("cp {input} {output}", 0)
	out, err := ext.Extract(context.Background(), "sample.md", []byte("fast output"))
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}
	if strings.TrimSpace(out) != "fast output" {
		t.Fatalf("unexpected output: %q", out)
	}
}

// TestDoclingTimeout_ProcessDocumentRunsDoclingOnce checks the ingest path:
// a PDF whose docling run hits the limit gets a per-document error that names
// the document and the limit, and docling runs once. Before #1105 the flat
// fallback ran the same command again after a structured-path timeout, which
// doubled the time for one document.
func TestDoclingTimeout_ProcessDocumentRunsDoclingOnce(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping POSIX-only command test on Windows")
	}
	dir := testutil.TempDir(t)
	counter := filepath.Join(dir, "runs.txt")
	fake := filepath.Join(dir, "counting-docling")
	script := "#!/bin/sh\necho run >> '" + counter + "'\nexec sleep 30\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docling: %v", err)
	}

	root := testutil.TempDir(t)
	const rel = "reports/annual-tables.pdf"
	if err := os.MkdirAll(filepath.Join(root, "reports"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(root, rel), "%PDF-1.4 fake")
	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: testutil.TempDir(t), STTProvider: "off"}, st)
	svc.SetDocumentExtractor(ingest.NewDoclingExtractorWithTimeout(fake+" --output {output} {input}", 300*time.Millisecond))

	f := ingest.DiscoveredFile{RelPath: rel, SizeBytes: 13, MTimeUnix: time.Now().Unix()}
	start := time.Now()
	perr := svc.ProcessDocument(context.Background(), f, nil, false)
	elapsed := time.Since(start)
	t.Logf("ProcessDocument returned: %v", perr)

	doc := documentByPath(t, st, rel)
	if doc.Status != "error" {
		t.Fatalf("status = %q, want \"error\"", doc.Status)
	}
	for _, want := range []string{"timed out", rel, "300ms", "ingest.docling.timeout_sec"} {
		if !strings.Contains(doc.ErrorMessage, want) {
			t.Fatalf("error_message must contain %q, got: %q", want, doc.ErrorMessage)
		}
	}

	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read run counter: %v", err)
	}
	if runs := strings.Count(string(raw), "run"); runs != 1 {
		t.Fatalf("docling ran %d times, want 1 (no second run after a timeout)", runs)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("document was not stopped promptly by the limit (took %s)", elapsed)
	}
}
