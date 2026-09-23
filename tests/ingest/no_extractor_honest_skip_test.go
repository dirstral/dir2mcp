package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// SPEC §7.4.B.2 (#584): a document no active engine can read MUST NOT stay at
// status=ok; under the lenient default it is a durable skip with an
// unsupported-format reason. The rule was applied only when SOME extractor was
// configured but could not read the format. With no extractor available at
// all, the default on a fresh machine with no docling and no Mistral key, a PDF
// was recorded status=ok with no representation: unsearchable, and invisible in
// `dir2mcp status`.
func TestNoExtractorAvailableIsAnHonestSkip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "notes.md"), "The budget meeting moved to Thursday.\n")
	writeFile(t, filepath.Join(root, "report.pdf"), "%PDF-1.4\n% no text layer, no extractor\n")

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	defer func() { _ = st.Close() }()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = t.TempDir()
	cfg.STTProvider = "off"
	cfg.IngestExtractor = "off"
	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	doc := mustGetDoc(t, st, "report.pdf")
	if doc.Status != "skipped" || doc.SkipReason != model.SkipReasonUnsupportedFormat {
		t.Fatalf("report.pdf: status=%q skip_reason=%q, want skipped/%s", doc.Status, doc.SkipReason, model.SkipReasonUnsupportedFormat)
	}
	if d := mustGetDoc(t, st, "notes.md"); d.Status != "ok" {
		t.Errorf("notes.md status = %q, want ok", d.Status)
	}
}
