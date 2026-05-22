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

// TestServerDoctor_JSONReportSurfacesOCRFallback runs `dir2mcp
// --json doctor` (no client arg => daemon-side preflight) under the
// same conditions that triggered the original client's failed
// install: a Mistral API key is set but docling is not on PATH. The
// doctor must flag the extractor as a warn-level fallback so the
// operator sees the diagnostic before they ever try to index.
func TestServerDoctor_JSONReportSurfacesOCRFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("PATH", t.TempDir()) // no docling on PATH

	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"--json", "doctor"})
		if code != 0 {
			t.Fatalf("doctor exit=%d stderr=%q", code, stderr.String())
		}

		var report struct {
			OK     bool `json:"ok"`
			Checks []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
				Detail string `json:"detail"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("decode doctor JSON: %v body=%q", err, stdout.String())
		}
		if !report.OK {
			t.Errorf("ok=false in report; expected warns only, got %+v", report.Checks)
		}
		extractor := findCheck(report.Checks, "extractor")
		if extractor == nil {
			t.Fatalf("doctor report missing extractor check: %+v", report.Checks)
		}
		if extractor.Status != "warn" {
			t.Errorf("extractor status = %q, want warn (fallback to mistral-ocr)", extractor.Status)
		}
		if !strings.Contains(extractor.Detail, "docling not found") {
			t.Errorf("extractor detail = %q, want fallback explanation (substring 'docling not found')", extractor.Detail)
		}

		// Sanity: the embed provider should resolve since
		// MISTRAL_API_KEY is set; the indexing_failures check
		// should report "no index yet" on a fresh state dir.
		if embed := findCheck(report.Checks, "provider.embed"); embed == nil || embed.Status != "ok" {
			t.Errorf("expected provider.embed=ok, got %+v", embed)
		}
		if failures := findCheck(report.Checks, "indexing_failures"); failures == nil || failures.Status != "ok" {
			t.Errorf("expected indexing_failures=ok on fresh state, got %+v", failures)
		}
	})
}

// TestServerDoctor_SurfacesDocumentLevelFailures pins the fix for
// the original "0 failures" lie. Before the fix, doctor only counted
// chunk-level embedding failures. A corpus where every PDF failed
// extraction (and so never produced chunks) was reported as
// "0 failures" even though the entire run had collapsed at the
// document stage. The check must now report doc-level errors too.
func TestServerDoctor_SurfacesDocumentLevelFailures(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, ".dir2mcp")
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("PATH", t.TempDir())

	// Seed the store with a failed-at-extraction document; no chunks
	// are ever created, mirroring the docling-failed-on-every-PDF
	// scenario from the original client install.
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(stateDir, "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath:   "doomed.pdf",
		DocType:   "pdf",
		SizeBytes: 1234,
		Status:    "error",
	}); err != nil {
		t.Fatalf("upsert error doc: %v", err)
	}
	_ = st.Close()

	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"--json", "doctor"})
		if code != 0 {
			t.Fatalf("doctor exit=%d stderr=%q", code, stderr.String())
		}
		var report struct {
			Checks []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
				Detail string `json:"detail"`
			} `json:"checks"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("decode doctor JSON: %v", err)
		}
		failures := findCheck(report.Checks, "indexing_failures")
		if failures == nil {
			t.Fatalf("indexing_failures check missing")
		}
		if failures.Status != "warn" {
			t.Errorf("indexing_failures status = %q, want warn (1 doc failed)", failures.Status)
		}
		if !strings.Contains(failures.Detail, "1 document") || !strings.Contains(failures.Detail, "failed extraction") {
			t.Errorf("indexing_failures detail = %q, want substring '1 document(s) failed extraction'", failures.Detail)
		}
	})
}

// TestServerDoctor_NoArgRoutesToDaemon ensures `dir2mcp doctor`
// without a positional argument hits the new daemon-side preflight
// path (not the legacy client-arg-required error).
func TestServerDoctor_NoArgRoutesToDaemon(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MISTRAL_API_KEY", "test-key")
	t.Setenv("PATH", t.TempDir())

	testutil.WithWorkingDir(t, tmp, func() {
		var stdout, stderr bytes.Buffer
		app := cli.NewAppWithIO(&stdout, &stderr)
		code := app.Run([]string{"doctor"})
		if code != 0 {
			t.Fatalf("doctor exit=%d stderr=%q", code, stderr.String())
		}
		out := stdout.String()
		if !strings.Contains(out, "config") || !strings.Contains(out, "extractor") {
			t.Errorf("doctor stdout missing daemon-check rows: %q", out)
		}
		if strings.Contains(stderr.String(), "requires a client name") {
			t.Errorf("no-arg invocation hit the client-arg branch: %q", stderr.String())
		}
	})
}

type doctorCheckRow struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func findCheck(checks []struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}, name string) *doctorCheckRow {
	for _, c := range checks {
		if c.Name == name {
			return &doctorCheckRow{Name: c.Name, Status: c.Status, Detail: c.Detail}
		}
	}
	return nil
}
