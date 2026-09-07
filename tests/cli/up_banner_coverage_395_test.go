package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// These tests pin the SPEC §7.7 startup diagnostics on the `dir2mcp up` banner
// (#395): the engine list covers the secondary pandoc engine with its reason when
// unavailable, and a "Coverage" section names the corpus formats the durable
// record holds that no active engine reads, with a remediation. Presence is read
// from the durable record over every status, because an uncovered document is
// recorded as skipped (lenient, #584) or error (strict), never as ok.

// lockedBuffer is a bytes.Buffer safe for the concurrent read the banner poll
// performs while `up` is still writing to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runUpUntilBanner starts a foreground `up` in dir, waits until the banner has
// been printed ("Ready for connections"), stops the server, and returns stdout.
func runUpUntilBanner(t *testing.T, dir string) string {
	t.Helper()
	var stdout, stderr lockedBuffer
	app := cli.NewAppWithIO(&stdout, &stderr)
	done := make(chan int, 1)
	withWorkingDir(t, dir, func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			done <- app.RunWithContext(ctx, []string{"up", "--non-interactive", "--foreground", "--listen", "127.0.0.1:0"})
		}()
		deadline := time.After(raceScaled(30 * time.Second))
		for !strings.Contains(stdout.String(), "Ready for connections") {
			select {
			case code := <-done:
				t.Fatalf("up exited (code=%d) before printing the banner\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
			case <-deadline:
				t.Fatalf("banner never printed\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
			case <-time.After(20 * time.Millisecond):
			}
		}
		cancel()
		select {
		case <-done:
		case <-time.After(raceScaled(30 * time.Second)):
			t.Fatal("up did not stop after cancel")
		}
	})
	return stdout.String()
}

// seedStore writes documents into dir/.dir2mcp/meta.sqlite before `up` starts.
func seedStore(t *testing.T, dir string, docs ...model.Document) {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(dir, ".dir2mcp", "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	for _, d := range docs {
		if err := st.UpsertDocument(ctx, d); err != nil {
			t.Fatalf("seed %s: %v", d.RelPath, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Logf("warning: store close failed: %v", err)
	}
}

// Under auto with Mistral OCR as the primary and no pandoc: the banner lists the
// pandoc engine as unavailable with its reason, and the Coverage section names the
// durably skipped .odt and the errored .tiff with a remedy that names both engines
// that would cover them. Neither document is status=ok; a report counting only ok
// rows (the pre-fix doctor) sees nothing.
func TestUpBanner_CoverageNamesDurablyUncoveredFormatsAndPandocEngine(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	t.Setenv("MISTRAL_API_KEY", "test-key-not-a-secret")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "auto")
	t.Setenv("PATH", t.TempDir()) // no docling, no pandoc: auto falls back to Mistral OCR

	seedStore(t, tmp,
		model.Document{RelPath: "report.pdf", DocType: "pdf", Status: "ok"},
		model.Document{RelPath: "minutes.odt", DocType: "document", Status: "skipped", SkipReason: model.SkipReasonUnsupportedFormat},
		model.Document{RelPath: "scan.tiff", DocType: "image", Status: "error", ErrorMessage: "unsupported format for extraction"},
	)

	out := runUpUntilBanner(t, tmp)

	for _, want := range []string{
		"Pandoc:",
		"unavailable (pandoc not found on PATH; T2 engine for .docx/.odt/.rtf/.epub)",
		"Coverage",
		"Uncovered:",
		".odt, .tiff",
		"2 document(s)",
		"never as indexed",
		"Fix:",
		"install docling",
		"install pandoc",
		"ingest.on_unsupported: strict",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q\n%s", want, out)
		}
	}
	// .pdf is read by the active Mistral OCR engine: it must not be named as a gap.
	if i := strings.Index(out, "Uncovered:"); i >= 0 {
		line := out[i:]
		if j := strings.Index(line, "\n"); j >= 0 {
			line = line[:j]
		}
		if strings.Contains(line, ".pdf") {
			t.Errorf("covered .pdf named as uncovered: %s", line)
		}
	}
}

// A pinned Mistral engine with only covered formats prints neither a Pandoc row
// (pandoc is ineligible under the pin) nor a Coverage section (nothing uncovered),
// so a healthy corpus keeps the banner clean.
func TestUpBanner_NoCoverageSectionWhenEverythingCovered(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	t.Setenv("MISTRAL_API_KEY", "test-key-not-a-secret")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "mistral")
	t.Setenv("PATH", t.TempDir())

	seedStore(t, tmp,
		model.Document{RelPath: "report.pdf", DocType: "pdf", Status: "ok"},
		model.Document{RelPath: "big.pdf", DocType: "pdf", Status: "skipped", SkipReason: model.SkipReasonSizeCap},
	)

	out := runUpUntilBanner(t, tmp)
	if !strings.Contains(out, "OCR:") || !strings.Contains(out, "mistral-ocr") {
		t.Fatalf("banner lacks the OCR row: %s", out)
	}
	for _, unwanted := range []string{"Pandoc:", "Coverage", "Uncovered:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("banner must not print %q for a fully covered corpus under a pin:\n%s", unwanted, out)
		}
	}
}

// With ingest.extractor=off every extractable format is uncovered by choice. The
// banner still names them (a coverage gap is never silent) but the remedy names
// the knob the operator set, not an engine to install.
func TestUpBanner_ExtractorOffNamesTheKnob(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	t.Setenv("MISTRAL_API_KEY", "test-key-not-a-secret")
	t.Setenv("DIR2MCP_AUTH_TOKEN", "test-token")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "off")
	t.Setenv("PATH", t.TempDir())

	seedStore(t, tmp,
		model.Document{RelPath: "report.pdf", DocType: "pdf", Status: "ok"},
	)

	out := runUpUntilBanner(t, tmp)
	for _, want := range []string{"Uncovered:", ".pdf", "1 document(s)", "Fix:", "ingest.extractor is off; set it to auto"} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "install docling") {
		t.Errorf("under off the remedy must name the knob, not an engine to install:\n%s", out)
	}
}

// routingJSONFromBundle runs `support-bundle` in dir and decodes routing.json.
func routingJSONFromBundle(t *testing.T, dir string) []struct {
	Capability string `json:"capability"`
	Provider   string `json:"provider"`
	Reason     string `json:"reason"`
} {
	t.Helper()
	bundlePath := filepath.Join(dir, "bundle.tar.gz")
	withWorkingDir(t, dir, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		if code := app.Run([]string{"support-bundle", "--output", bundlePath}); code != 0 {
			t.Fatalf("support-bundle exit=%d stderr=%q", code, stderr.String())
		}
	})
	entries := extractTarGz(t, bundlePath)
	var routing struct {
		Decisions []struct {
			Capability string `json:"capability"`
			Provider   string `json:"provider"`
			Reason     string `json:"reason"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(entries["routing.json"], &routing); err != nil {
		t.Fatalf("routing.json invalid: %v", err)
	}
	return routing.Decisions
}

// With a functional pandoc on PATH under auto, the engine row reports it as the
// active secondary engine (and the support bundle's routing.json carries the same
// row, so a maintainer sees it without a live daemon). The reason is the redacted
// resolution source, never the binary path.
func TestRoutingDecisions_PandocSecondaryEngineActive(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	t.Setenv("MISTRAL_API_KEY", "test-key-not-a-secret")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "auto")
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "pandoc")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write pandoc stub: %v", err)
	}
	t.Setenv("PATH", binDir) // pandoc resolves, docling does not

	decisions := routingJSONFromBundle(t, tmp)
	var found bool
	for _, d := range decisions {
		if d.Capability != "Pandoc" {
			continue
		}
		found = true
		if d.Provider != "pandoc" {
			t.Errorf("Pandoc provider = %q, want pandoc", d.Provider)
		}
		if !strings.Contains(d.Reason, "secondary engine") || !strings.Contains(d.Reason, "auto-detected on PATH") {
			t.Errorf("Pandoc reason = %q, want the secondary-engine + resolution reason", d.Reason)
		}
		if strings.Contains(d.Reason, binDir) {
			t.Errorf("Pandoc reason leaks the binary directory: %q", d.Reason)
		}
	}
	if !found {
		t.Fatalf("routing.json has no Pandoc decision: %+v", decisions)
	}
}

// Under a docling/mistral pin pandoc is ineligible: no row, even when a pandoc
// binary is present, because the policy never activates it.
func TestRoutingDecisions_NoPandocRowUnderOtherPin(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	t.Setenv("MISTRAL_API_KEY", "test-key-not-a-secret")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "mistral")
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "pandoc"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write pandoc stub: %v", err)
	}
	t.Setenv("PATH", binDir)

	for _, d := range routingJSONFromBundle(t, tmp) {
		if d.Capability == "Pandoc" {
			t.Fatalf("pandoc row present under the mistral pin: %+v", d)
		}
	}
}

// Under the pandoc pin the OCR row already names pandoc as the primary; a second
// Pandoc row would be a duplicate and is omitted.
func TestRoutingDecisions_NoDuplicateRowWhenPandocIsPrimary(t *testing.T) {
	tmp := t.TempDir()
	clearProviderEnv(t)
	t.Setenv("MISTRAL_API_KEY", "test-key-not-a-secret")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "pandoc")
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "pandoc"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write pandoc stub: %v", err)
	}
	t.Setenv("PATH", binDir)

	decisions := routingJSONFromBundle(t, tmp)
	var ocr, pandocRows int
	for _, d := range decisions {
		switch d.Capability {
		case "OCR":
			ocr++
			if d.Provider != "pandoc" {
				t.Errorf("OCR provider = %q, want pandoc under the pandoc pin", d.Provider)
			}
		case "Pandoc":
			pandocRows++
		}
	}
	if ocr != 1 || pandocRows != 0 {
		t.Fatalf("rows: OCR=%d Pandoc=%d, want OCR=1 Pandoc=0: %+v", ocr, pandocRows, decisions)
	}
}
