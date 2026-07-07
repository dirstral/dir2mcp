package tests

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/appstate"
	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestSkipReasonCoverage is the honest-coverage regression guard for #414: a
// corpus mixing every skip class must record a stable, distinguishable
// skip_reason on each persisted row, the CorpusStats.SkipSummary aggregate must
// group them, and the numerous path-excluded drop (which persists no row) must
// surface via the ingestor's in-run per-reason counter instead.
func TestSkipReasonCoverage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	// An ingestable file (indexed, no skip_reason).
	writeCorpusFile(t, root, "notes.txt", []byte("just some plain prose that indexes fine"))
	// An archive container -> skip_reason=archive.
	writeCorpusFile(t, root, "bundle.zip", emptyZip(t))
	// A .env file -> classified "ignore" -> skip_reason=ignore_rule.
	writeCorpusFile(t, root, ".env", []byte("HARMLESS=1\n"))
	// A file whose content matches a secret pattern -> skip_reason=secret_excluded.
	writeCorpusFile(t, root, "creds.txt", []byte("value TOPSECRETTOKEN here"))
	// An over-cap file dropped at discovery -> skip_reason=size_cap.
	writeCorpusFile(t, root, "big.bin", bytes.Repeat([]byte("x"), 2*1024*1024))
	// A path-excluded file -> counted in-run, NOT persisted.
	writeCorpusFile(t, root, filepath.Join("skipdir", "ignored.txt"), []byte("excluded by glob"))

	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	defer func() { _ = st.Close() }()

	cfg := config.Default()
	cfg.RootDir = root
	cfg.STTProvider = "off"
	cfg.IngestMaxFileMB = 1 // the 2 MiB file must be dropped
	cfg.SecretPatterns = []string{"TOPSECRETTOKEN"}
	cfg.PathExcludes = []string{"skipdir/**"}

	svc := mustNewIngestService(t, cfg, st)
	svc.SetIndexingState(appstate.NewIndexingState(appstate.ModeIncremental))
	if err := svc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Each persisted skip row carries its stable reason.
	assertSkipReason(t, ctx, st, "bundle.zip", "skipped", model.SkipReasonArchive)
	assertSkipReason(t, ctx, st, ".env", "skipped", model.SkipReasonIgnoreRule)
	assertSkipReason(t, ctx, st, "creds.txt", "secret_excluded", model.SkipReasonSecretExcluded)
	assertSkipReason(t, ctx, st, "big.bin", "skipped", model.SkipReasonSizeCap)

	// The ingestable file is NOT skipped and carries no skip_reason.
	if doc, err := st.GetDocumentByPath(ctx, "notes.txt"); err != nil {
		t.Fatalf("GetDocumentByPath(notes.txt): %v", err)
	} else if doc.SkipReason != "" {
		t.Errorf("notes.txt skip_reason = %q, want empty", doc.SkipReason)
	}

	// The path-excluded file persists no row.
	if _, err := st.GetDocumentByPath(ctx, "skipdir/ignored.txt"); err == nil {
		t.Errorf("skipdir/ignored.txt should not have a persisted row (path-excluded)")
	}

	// The durable aggregate groups the persisted skips.
	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.SkipSummary == nil {
		t.Fatalf("SkipSummary = nil, want populated")
	}
	for reason, want := range map[string]int64{
		model.SkipReasonArchive:        1,
		model.SkipReasonIgnoreRule:     1,
		model.SkipReasonSecretExcluded: 1,
		model.SkipReasonSizeCap:        1,
	} {
		if got := stats.SkipSummary.Categories[reason]; got != want {
			t.Errorf("SkipSummary.Categories[%q] = %d, want %d (full=%+v)", reason, got, want, stats.SkipSummary.Categories)
		}
	}
	// path_excluded is NOT durable, so it must not appear in the store aggregate.
	if got := stats.SkipSummary.Categories[model.SkipReasonPathExcluded]; got != 0 {
		t.Errorf("SkipSummary should not contain path_excluded (non-persisted), got %d", got)
	}

	// The in-run counter surfaces the path-excluded drop.
	inRun := svc.SkipReasonCounts()
	if inRun[model.SkipReasonPathExcluded] != 1 {
		t.Errorf("SkipReasonCounts()[path_excluded] = %d, want 1 (full=%+v)", inRun[model.SkipReasonPathExcluded], inRun)
	}

	// No file in this corpus should have errored.
	if stats.Errors != 0 {
		t.Errorf("CorpusStats.Errors = %d, want 0", stats.Errors)
	}
}

func assertSkipReason(t *testing.T, ctx context.Context, st *store.SQLiteStore, relPath, wantStatus, wantReason string) {
	t.Helper()
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath(%s): %v", relPath, err)
	}
	if doc.Status != wantStatus {
		t.Errorf("%s status = %q, want %q", relPath, doc.Status, wantStatus)
	}
	if doc.SkipReason != wantReason {
		t.Errorf("%s skip_reason = %q, want %q", relPath, doc.SkipReason, wantReason)
	}
}

func writeCorpusFile(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, content, 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// emptyZip returns the bytes of a valid, empty zip archive.
func emptyZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}
