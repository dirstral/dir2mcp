package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestServerDoctor_ExtractionCoverage_NamesDurablySkippedUnsupportedFormats pins
// the §7.7 honest-coverage contract on the doctor AFTER the run that found the
// gap has ended (#395, closing the #584 blind spot on this surface).
//
// #584 made a lenient unsupported-format outcome a durable status="skipped"
// (skip_reason=unsupported_format), and strict mode records status="error". The
// doctor's uncovered-format check used to count only status="ok" extractable
// documents, so the exact documents that ARE the coverage gap became invisible
// to it the moment they were recorded honestly: `doctor` said nothing about the
// .odt files a mistral-only install can never read. The check must name every
// present-but-uncovered format regardless of the status the run stamped on it.
func TestServerDoctor_ExtractionCoverage_NamesDurablySkippedUnsupportedFormats(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key-not-a-secret")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "mistral") // pinned flat OCR: reads pdf/png/jpg/jpeg/webp only
	t.Setenv("PATH", t.TempDir())                   // no docling, no pandoc

	seedDoctorStore(t, tmp, func(ctx context.Context, st *store.SQLiteStore) {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: "report.pdf", DocType: "pdf", Status: "ok"}); err != nil {
			t.Fatalf("seed ok pdf: %v", err)
		}
		// The durable lenient outcome (#584): skipped, reason unsupported_format.
		// Two of them, so the document count (3) and the format count (2) differ
		// and a report that confuses the two cannot pass.
		for _, p := range []string{"minutes.odt", "agenda.odt"} {
			if err := st.UpsertDocument(ctx, model.Document{RelPath: p, DocType: "document", Status: "skipped", SkipReason: model.SkipReasonUnsupportedFormat}); err != nil {
				t.Fatalf("seed skipped %s: %v", p, err)
			}
		}
		// The strict outcome: a non-fatal per-document error.
		if err := st.UpsertDocument(ctx, model.Document{RelPath: "scan.tiff", DocType: "image", Status: "error", ErrorMessage: "unsupported format for extraction"}); err != nil {
			t.Fatalf("seed errored tiff: %v", err)
		}
	})

	c := findCheck(runDoctorReport(t, tmp), "extraction_coverage")
	if c == nil {
		t.Fatal("extraction_coverage check missing")
	}
	if c.Status != "warn" {
		t.Errorf("status = %q (%s), want warn: the corpus holds formats the pinned mistral engine cannot read", c.Status, c.Detail)
	}
	for _, want := range []string{".odt", ".tiff", "3 document(s) in 2 format(s)", "never as indexed", "install pandoc", "install docling"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail missing %q: %s", want, c.Detail)
		}
	}
	if strings.Contains(c.Detail, ".pdf") {
		t.Errorf("covered .pdf named as uncovered: %s", c.Detail)
	}
}

// TestServerDoctor_ExtractionCoverage_OnlyDurablySkippedDocs pins the harder
// case: EVERY extractable document is uncovered, so there is no status="ok"
// extractable row at all. A coverage check gated on "index-eligible extractable
// documents exist" never runs here and reports the corpus healthy; the check must
// still name the gap.
func TestServerDoctor_ExtractionCoverage_OnlyDurablySkippedDocs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key-not-a-secret")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "mistral")
	t.Setenv("PATH", t.TempDir())

	seedDoctorStore(t, tmp, func(ctx context.Context, st *store.SQLiteStore) {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: "notes.md", DocType: "md", Status: "ok"}); err != nil {
			t.Fatalf("seed md: %v", err)
		}
		if err := st.UpsertDocument(ctx, model.Document{RelPath: "minutes.odt", DocType: "document", Status: "skipped", SkipReason: model.SkipReasonUnsupportedFormat}); err != nil {
			t.Fatalf("seed skipped odt: %v", err)
		}
	})

	c := findCheck(runDoctorReport(t, tmp), "extraction_coverage")
	if c == nil {
		t.Fatal("extraction_coverage check missing")
	}
	if c.Status != "warn" {
		t.Errorf("status = %q (%s), want warn: the only extractable document is uncovered", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "1 document(s) in 1 format(s)") || !strings.Contains(c.Detail, ".odt") {
		t.Errorf("detail must name the .odt gap: %s", c.Detail)
	}
}

// TestServerDoctor_ExtractionCoverage_NoExtractorOnlySkippedDocs pins the lean
// install whose only extractable documents are already durably skipped: no
// extractor resolves AND no extractable row is status="ok". The "N documents need
// extraction" error needs ok rows, so it must not fire with a count of 0; the
// coverage half must still run and name the .odt, leading with why no engine is
// available.
func TestServerDoctor_ExtractionCoverage_NoExtractorOnlySkippedDocs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "auto")
	t.Setenv("PATH", t.TempDir()) // no docling, no pandoc, no credential: nothing resolves

	seedDoctorStore(t, tmp, func(ctx context.Context, st *store.SQLiteStore) {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: "minutes.odt", DocType: "document", Status: "skipped", SkipReason: model.SkipReasonUnsupportedFormat}); err != nil {
			t.Fatalf("seed skipped odt: %v", err)
		}
	})

	c := findCheck(runDoctorReport(t, tmp), "extraction_coverage")
	if c == nil {
		t.Fatal("extraction_coverage check missing")
	}
	if c.Status != "warn" {
		t.Errorf("status = %q (%s), want warn (not error: no ok row needs extraction; not ok: the .odt is uncovered)", c.Status, c.Detail)
	}
	for _, want := range []string{"1 document(s) in 1 format(s)", "(none)", ".odt", "No extractor is available", "install pandoc"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail missing %q: %s", want, c.Detail)
		}
	}
	if strings.Contains(c.Detail, "0 document(s) need extraction") {
		t.Errorf("a zero-count dead-end error is noise, not a diagnostic: %s", c.Detail)
	}
}

// TestServerDoctor_ExtractionCoverage_PandocClosesTheGap pins the remediation
// loop end to end: the same durably skipped .odt is NOT a gap once a functional
// pandoc (T2, #393) is on PATH under auto, because the verdict consults the same
// per-format router indexing uses, pandoc tier included. The doctor must not keep
// warning about a format the operator has just covered.
func TestServerDoctor_ExtractionCoverage_PandocClosesTheGap(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key-not-a-secret")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "auto")
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "pandoc"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write pandoc stub: %v", err)
	}
	t.Setenv("PATH", binDir) // pandoc resolves; docling does not

	seedDoctorStore(t, tmp, func(ctx context.Context, st *store.SQLiteStore) {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: "report.pdf", DocType: "pdf", Status: "ok"}); err != nil {
			t.Fatalf("seed pdf: %v", err)
		}
		if err := st.UpsertDocument(ctx, model.Document{RelPath: "minutes.odt", DocType: "document", Status: "skipped", SkipReason: model.SkipReasonUnsupportedFormat}); err != nil {
			t.Fatalf("seed skipped odt: %v", err)
		}
	})

	c := findCheck(runDoctorReport(t, tmp), "extraction_coverage")
	if c == nil {
		t.Fatal("extraction_coverage check missing")
	}
	if c.Status != "ok" {
		t.Errorf("status = %q (%s), want ok: pandoc covers .odt and Mistral OCR covers .pdf", c.Status, c.Detail)
	}
	if strings.Contains(c.Detail, ".odt") {
		t.Errorf("a pandoc-covered .odt must not be named as a gap: %s", c.Detail)
	}
}
