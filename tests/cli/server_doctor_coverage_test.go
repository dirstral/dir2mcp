package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/cli"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// runDoctorReport runs `dir2mcp --json doctor` in tmp and returns the decoded
// checks. It mirrors the setup the sibling doctor tests use.
func runDoctorReport(t *testing.T, tmp string) []struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
} {
	t.Helper()
	var report struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		if code := app.Run([]string{"--json", "doctor"}); code != 0 && code != 1 {
			t.Fatalf("doctor exit=%d stderr=%q", code, stderr.String())
		}
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("decode doctor JSON: %v body=%q", err, stdout.String())
		}
	})
	return report.Checks
}

// seedDoctorStore initializes the SQLite metadata store under tmp/.dir2mcp and
// runs fn against it, then closes it.
func seedDoctorStore(t *testing.T, tmp string, fn func(ctx context.Context, st *store.SQLiteStore)) {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(tmp, ".dir2mcp", "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	fn(ctx, st)
	if err := st.Close(); err != nil {
		t.Logf("warning: store close failed: %v", err)
	}
}

// TestServerDoctor_ExtractionCoverage_NoExtractor pins the loud diagnostic for
// the lean-build failure mode: extractable documents (PDFs) exist but no
// extractor is available, so they never become searchable. The check must be
// an error and name the remedy.
func TestServerDoctor_ExtractionCoverage_NoExtractor(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "")             // no Mistral OCR fallback
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "off") // force extractor decision to disabled
	t.Setenv("PATH", t.TempDir())               // no docling on PATH

	seedDoctorStore(t, tmp, func(ctx context.Context, st *store.SQLiteStore) {
		for _, p := range []string{"a.pdf", "b.pdf"} {
			if err := st.UpsertDocument(ctx, model.Document{RelPath: p, DocType: "pdf", Status: "ok"}); err != nil {
				t.Fatalf("seed %s: %v", p, err)
			}
		}
	})

	c := findCheck(runDoctorReport(t, tmp), "extraction_coverage")
	if c == nil {
		t.Fatal("extraction_coverage check missing")
	}
	if c.Status != "error" {
		t.Errorf("status = %q, want error", c.Status)
	}
	for _, want := range []string{"2 document(s) need extraction", "no extractor", "reindex"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail missing %q: %s", want, c.Detail)
		}
	}
}

// TestServerDoctor_ExtractionCoverage_NoEmbeddings pins the second silent
// failure: chunks were created but none embedded, so search matches nothing.
func TestServerDoctor_ExtractionCoverage_NoEmbeddings(t *testing.T) {
	tmp := t.TempDir()
	// A docling command makes the extractor "available" so we exercise the
	// embedding branch, not the no-extractor branch. `cat` is a harmless stand-in.
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "docling")
	t.Setenv("DIR2MCP_DOCLING_COMMAND", "cat {input}")

	seedDoctorStore(t, tmp, func(ctx context.Context, st *store.SQLiteStore) {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: "notes.md", DocType: "md", Status: "ok"}); err != nil {
			t.Fatalf("seed doc: %v", err)
		}
		// Fetch the assigned doc_id rather than assuming it is 1, so the test
		// is robust to bootstrap rows or schema changes.
		doc, err := st.GetDocumentByPath(ctx, "notes.md")
		if err != nil {
			t.Fatalf("get seeded doc: %v", err)
		}
		repID, err := st.UpsertRepresentation(ctx, model.Representation{DocID: doc.DocID, RepType: "raw_text", RepHash: "h1"})
		if err != nil {
			t.Fatalf("seed rep: %v", err)
		}
		// A pending (not embedded) chunk: chunks_total=1, embedded_ok=0.
		if _, err := st.InsertChunkWithSpans(ctx, model.Chunk{
			RepID: repID, Ordinal: 0, Text: "hello", TextHash: "t1",
			IndexKind: "text", EmbeddingStatus: "pending",
		}, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 1}}); err != nil {
			t.Fatalf("seed chunk: %v", err)
		}
	})

	c := findCheck(runDoctorReport(t, tmp), "extraction_coverage")
	if c == nil {
		t.Fatal("extraction_coverage check missing")
	}
	if c.Status != "warn" {
		t.Errorf("status = %q, want warn", c.Status)
	}
	for _, want := range []string{"indexed but 0 embedded", "MISTRAL_API_KEY", "reindex"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail missing %q: %s", want, c.Detail)
		}
	}
}

// TestServerDoctor_ExtractionCoverage_SkippedNotCounted pins that only
// index-eligible (status='ok') extractable documents are counted: a skipped
// PDF must not trip the "needs extraction" error on its own.
func TestServerDoctor_ExtractionCoverage_SkippedNotCounted(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "")
	t.Setenv("DIR2MCP_INGEST_EXTRACTOR", "off")
	t.Setenv("PATH", t.TempDir())

	seedDoctorStore(t, tmp, func(ctx context.Context, st *store.SQLiteStore) {
		// One ok PDF (counts) and one skipped PDF (must NOT count).
		if err := st.UpsertDocument(ctx, model.Document{RelPath: "ok.pdf", DocType: "pdf", Status: "ok"}); err != nil {
			t.Fatalf("seed ok pdf: %v", err)
		}
		if err := st.UpsertDocument(ctx, model.Document{RelPath: "big.pdf", DocType: "pdf", Status: "skipped"}); err != nil {
			t.Fatalf("seed skipped pdf: %v", err)
		}
	})

	c := findCheck(runDoctorReport(t, tmp), "extraction_coverage")
	if c == nil {
		t.Fatal("extraction_coverage check missing")
	}
	if c.Status != "error" {
		t.Errorf("status = %q, want error", c.Status)
	}
	// Exactly one document should be counted, not two.
	if !strings.Contains(c.Detail, "1 document(s) need extraction") {
		t.Errorf("expected only the ok pdf counted (1), got: %s", c.Detail)
	}
}

// TestServerDoctor_ExtractionCoverage_NoIndex confirms the check passes
// cleanly when there is no index yet (fresh install, nothing ingested).
func TestServerDoctor_ExtractionCoverage_NoIndex(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	c := findCheck(runDoctorReport(t, tmp), "extraction_coverage")
	if c == nil {
		t.Fatal("extraction_coverage check missing")
	}
	if c.Status != "ok" {
		t.Errorf("status = %q (%s), want ok (no index yet)", c.Status, c.Detail)
	}
}
