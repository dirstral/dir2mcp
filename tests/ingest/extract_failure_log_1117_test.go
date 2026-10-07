package tests

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/ingest"
)

// Issue #1117: when an extraction error ends only in the document row, the
// daemon log gets exactly one line at that moment. The line names the document
// by its relative path. A docling timeout line also names the limit. The line
// never carries document text.

// docTextMarker is a string in each test document body. No log line may
// contain it.
const docTextMarker = "BODY-TEXT-MARKER-1117"

// logLines returns the non-empty log lines written into sink so far. It
// reuses syncBuffer from fanout_caps_test.go.
func logLines(sink *syncBuffer) []string {
	var out []string
	for _, l := range strings.Split(sink.String(), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// captureLogger sets a logger on svc that writes into a buffer the test reads.
func captureLogger(svc *ingest.Service) *syncBuffer {
	sink := &syncBuffer{}
	svc.SetLogger(log.New(sink, "", 0))
	return sink
}

// linesContaining returns the log lines that contain substr.
func linesContaining(lines []string, substr string) []string {
	var out []string
	for _, l := range lines {
		if strings.Contains(l, substr) {
			out = append(out, l)
		}
	}
	return out
}

// assertNoDocText fails when any log line carries the document body.
func assertNoDocText(t *testing.T, lines []string) {
	t.Helper()
	for _, l := range lines {
		if strings.Contains(l, docTextMarker) {
			t.Fatalf("a log line carries document text: %q", l)
		}
	}
}

// writeCorpusDoc writes one document under a new corpus root and returns the
// root and the DiscoveredFile for it.
func writeCorpusDoc(t *testing.T, rel string) (string, ingest.DiscoveredFile) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "%PDF-1.4 " + docTextMarker
	writeFile(t, filepath.Join(root, rel), body)
	return root, ingest.DiscoveredFile{RelPath: rel, SizeBytes: int64(len(body)), MTimeUnix: time.Now().Unix()}
}

// TestExtractFailureLog_DoclingTimeoutLogsOneLine runs a fake slow docling
// command against a short limit. The document row carries the timeout, and the
// log carries exactly one line with the path and the limit.
func TestExtractFailureLog_DoclingTimeoutLogsOneLine(t *testing.T) {
	skipOnWindows(t, "shell-script fake docling needs a POSIX sh")
	slow := writeSlowDocling(t)
	const rel = "reports/annual-tables.pdf"
	root, f := writeCorpusDoc(t, rel)

	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}, st)
	svc.SetDocumentExtractor(ingest.NewDoclingExtractorWithTimeout(slow+" --output {output} {input}", 300*time.Millisecond))
	sink := captureLogger(svc)

	if err := svc.ProcessDocument(context.Background(), f, nil, false); err == nil {
		t.Fatal("ProcessDocument must return the timeout error")
	}
	if doc := documentByPath(t, st, rel); doc.Status != "error" || !strings.Contains(doc.ErrorMessage, "timed out") {
		t.Fatalf("row = %q / %q, want status error with the timeout", doc.Status, doc.ErrorMessage)
	}

	lines := logLines(sink)
	want := "docling: timed out on " + rel + " after 300ms (ingest.docling.timeout_sec=0.3); the document is recorded as failed"
	if got := linesContaining(lines, "timed out"); len(got) != 1 || got[0] != want {
		t.Fatalf("timeout log lines = %q, want exactly [%q]", got, want)
	}
	if got := linesContaining(lines, rel); len(got) != 1 {
		t.Fatalf("log lines that name the document = %q, want exactly one", got)
	}
	assertNoDocText(t, lines)
}

// TestExtractFailureLog_DoclingCommandFailureLogsOneLine covers a docling run
// that exits with an error. The structured run and the flat run both fail, and
// the log still carries one line, with the first stderr line as the reason.
func TestExtractFailureLog_DoclingCommandFailureLogsOneLine(t *testing.T) {
	skipOnWindows(t, "shell-script fake docling needs a POSIX sh")
	fake := filepath.Join(t.TempDir(), "failing-docling")
	script := "#!/bin/sh\necho 'conversion error: bad xref table' >&2\necho 'second stderr line' >&2\nexit 3\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docling: %v", err)
	}
	const rel = "scans/broken.pdf"
	root, f := writeCorpusDoc(t, rel)

	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}, st)
	svc.SetDocumentExtractor(ingest.NewDoclingExtractorWithTimeout(fake+" --output {output} {input}", 10*time.Second))
	sink := captureLogger(svc)

	if err := svc.ProcessDocument(context.Background(), f, nil, false); err == nil {
		t.Fatal("ProcessDocument must return the docling error")
	}
	if doc := documentByPath(t, st, rel); doc.Status != "error" {
		t.Fatalf("status = %q, want error", doc.Status)
	}

	lines := logLines(sink)
	got := linesContaining(lines, rel)
	if len(got) != 1 {
		t.Fatalf("log lines that name the document = %q, want exactly one", got)
	}
	line := got[0]
	for _, want := range []string{"docling: extraction failed on " + rel, "the document is recorded as failed", "bad xref table"} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line %q must contain %q", line, want)
		}
	}
	if got := linesContaining(lines, "second stderr line"); len(got) != 0 {
		t.Fatalf("the log must stop at the first stderr line break, got %q", got)
	}
	if got := linesContaining(lines, "extraction failed"); len(got) != 1 {
		t.Fatalf("extraction-failure lines = %q, want exactly one", got)
	}
	assertNoDocText(t, lines)
}

// failingExtractor is a flat extractor (for example Mistral OCR) whose
// provider call fails.
type failingExtractor struct{ msg string }

func (e failingExtractor) Extract(context.Context, string, []byte) (string, error) {
	return "", errors.New(e.msg)
}

// TestExtractFailureLog_ProviderFailureLogsOneRedactedLine covers a flat
// extraction provider that fails. The line names the document, and the
// credential redactors run on the reason.
func TestExtractFailureLog_ProviderFailureLogsOneRedactedLine(t *testing.T) {
	const rel = "inbox/receipt.pdf"
	root, f := writeCorpusDoc(t, rel)

	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}, st)
	const token = "sk-proj-abcdefghijklmnopqrstuvwxyz0123"
	svc.SetDocumentExtractor(failingExtractor{msg: "upstream status 401: invalid key " + token})
	sink := captureLogger(svc)

	if err := svc.ProcessDocument(context.Background(), f, nil, false); err == nil {
		t.Fatal("ProcessDocument must return the provider error")
	}

	lines := logLines(sink)
	got := linesContaining(lines, rel)
	if len(got) != 1 {
		t.Fatalf("log lines that name the document = %q, want exactly one", got)
	}
	for _, want := range []string{"custom: extraction failed on " + rel, "status 401", "[REDACTED]"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("log line %q must contain %q", got[0], want)
		}
	}
	for _, l := range lines {
		if strings.Contains(l, token) {
			t.Fatalf("a log line carries the credential: %q", l)
		}
	}
	assertNoDocText(t, lines)
}

// TestExtractFailureLog_PandocFailureLogsOneLine covers the pandoc engine: a
// pandoc that passes the functional check but fails on the document.
func TestExtractFailureLog_PandocFailureLogsOneLine(t *testing.T) {
	stub := writePandocStub(t, "pandoc",
		"case \"$1\" in --version) echo 'pandoc 3.1'; exit 0;; esac\necho 'pandoc: cannot read the archive' >&2\nexit 64")
	const rel = "letters/offer.docx"
	root, f := writeCorpusDoc(t, rel)

	cfg := config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off", IngestExtractor: "pandoc", IngestPandocCommand: stub}
	st := newRealStore(t)
	svc := mustNewIngestService(t, cfg, st)
	ex := ingest.DocumentExtractorFromConfig(cfg)
	if ex == nil {
		t.Fatal("pandoc stub did not resolve as an extractor")
	}
	svc.SetDocumentExtractor(ex)
	svc.ActivatePandocEngine(cfg)
	sink := captureLogger(svc)

	if err := svc.ProcessDocument(context.Background(), f, nil, false); err == nil {
		t.Fatal("ProcessDocument must return the pandoc error")
	}
	if doc := documentByPath(t, st, rel); doc.Status != "error" {
		t.Fatalf("status = %q, want error", doc.Status)
	}

	lines := logLines(sink)
	got := linesContaining(lines, rel)
	if len(got) != 1 {
		t.Fatalf("log lines that name the document = %q, want exactly one", got)
	}
	if !strings.Contains(got[0], "pandoc: extraction failed on "+rel) || !strings.Contains(got[0], "the document is recorded as failed") {
		t.Fatalf("unexpected pandoc log line: %q", got[0])
	}
	assertNoDocText(t, lines)
}

// TestExtractFailureLog_CancelledContextLogsNothing checks that a shutdown is
// not logged as a failed document: the caller context is cancelled, so the
// extraction error is not a fault of the document.
func TestExtractFailureLog_CancelledContextLogsNothing(t *testing.T) {
	const rel = "inbox/late.pdf"
	root, f := writeCorpusDoc(t, rel)

	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}, st)
	ctx, cancel := context.WithCancel(context.Background())
	called := false
	svc.SetDocumentExtractor(cancelingExtractor{cancel: cancel, called: &called})
	sink := captureLogger(svc)

	_ = svc.ProcessDocument(ctx, f, nil, false)
	if !called {
		t.Fatal("the extractor was not called, so the test proves nothing")
	}
	if got := linesContaining(logLines(sink), "extraction failed"); len(got) != 0 {
		t.Fatalf("a cancelled run must not log an extraction failure, got %q", got)
	}
}

// cancelingExtractor cancels the caller context and then fails, the way an
// extractor fails when the daemon shuts down during a call.
type cancelingExtractor struct {
	cancel context.CancelFunc
	called *bool
}

func (e cancelingExtractor) Extract(ctx context.Context, _ string, _ []byte) (string, error) {
	*e.called = true
	e.cancel()
	<-ctx.Done()
	return "", ctx.Err()
}

// TestExtractFailureLog_RunLogsOneLinePerFailedDocument covers the batch scan
// (svc.Run), the path of the 2026-10-06 release gate: before #1117 it counted
// the error and wrote the row, and logged nothing.
func TestExtractFailureLog_RunLogsOneLinePerFailedDocument(t *testing.T) {
	const rel = "reports/q3.pdf"
	root, _ := writeCorpusDoc(t, rel)

	st := newRealStore(t)
	svc := mustNewIngestService(t, config.Config{RootDir: root, StateDir: t.TempDir(), STTProvider: "off"}, st)
	svc.SetDocumentExtractor(failingExtractor{msg: "upstream status 503"})
	sink := captureLogger(svc)

	if err := svc.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if doc := documentByPath(t, st, rel); doc.Status != "error" {
		t.Fatalf("status = %q, want error", doc.Status)
	}
	got := linesContaining(logLines(sink), "extraction failed on "+rel)
	if len(got) != 1 {
		t.Fatalf("extraction-failure lines for %s = %q, want exactly one", rel, got)
	}
}

// TestExtractFailureLog_WatchLogsOneLine covers the watcher: it logs every
// failed document itself ("watch: index ..."), so an extraction failure that
// already has its line must not get a second one.
func TestExtractFailureLog_WatchLogsOneLine(t *testing.T) {
	requireWatchIntegration(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "seed.txt"), "seed")

	st := newRealStore(t)
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.STTProvider = "off"
	cfg.IngestWatchDebounce = 20 * time.Millisecond
	svc := mustNewIngestService(t, cfg, st)
	svc.SetDocumentExtractor(failingExtractor{msg: "upstream status 503"})
	state := appstate.NewIndexingState(appstate.ModeIncremental)
	svc.SetIndexingState(state)
	sink := captureLogger(svc)
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	baseErrors := state.Snapshot().Errors

	// Run the watcher in its own goroutine, so the test can wait for Watch to
	// return. Watch returns only after its worker has finished the current
	// document, so every log line of that document is written by then.
	watchDone := make(chan error, 1)
	go func() { watchDone <- svc.Watch(ctx) }()
	time.Sleep(100 * time.Millisecond)

	const rel = "late/arrival.pdf"
	if err := os.MkdirAll(filepath.Join(root, "late"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(root, rel), "%PDF-1.4 "+docTextMarker)

	// The watcher counts the error before it decides whether to log the
	// failure. Wait for the count, then stop the watcher and wait for Watch to
	// return, so the watcher has finished with the document.
	deadline := time.Now().Add(5 * time.Second)
	for state.Snapshot().Errors == baseErrors {
		if time.Now().After(deadline) {
			t.Fatal("the watcher did not record the failed document")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-watchDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Watch: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Watch did not return after cancel")
	}

	lines := logLines(sink)
	got := linesContaining(lines, rel)
	if len(got) != 1 || !strings.Contains(got[0], "extraction failed on "+rel) {
		t.Fatalf("log lines that name %s = %q, want exactly one extraction-failure line", rel, got)
	}
	assertNoDocText(t, lines)
}
