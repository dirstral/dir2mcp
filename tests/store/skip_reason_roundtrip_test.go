package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestSQLiteStore_SkipReasonRoundtrip pins the persistence of the additive
// documents.skip_reason column (#414): it round-trips through both
// GetDocumentByPath and ListFiles, and re-opening the same database file (which
// re-runs the additive migration) is a no-op rather than an error, proving the
// migration is idempotent.
func TestSQLiteStore_SkipReasonRoundtrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")

	st := store.NewSQLiteStore(dbPath)
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	doc := model.Document{
		RelPath:    "vendor/lib.zip",
		DocType:    "archive",
		SizeBytes:  4096,
		MTimeUnix:  1_700_000_000,
		Status:     "skipped",
		SkipReason: model.SkipReasonArchive,
	}
	if err := st.UpsertDocument(ctx, doc); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}

	got, err := st.GetDocumentByPath(ctx, doc.RelPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	if got.SkipReason != model.SkipReasonArchive {
		t.Errorf("GetDocumentByPath skip_reason = %q, want %q", got.SkipReason, model.SkipReasonArchive)
	}

	docs, _, err := st.ListFiles(ctx, "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	found := false
	for _, d := range docs {
		if d.RelPath == doc.RelPath {
			found = true
			if d.SkipReason != model.SkipReasonArchive {
				t.Errorf("ListFiles skip_reason = %q, want %q", d.SkipReason, model.SkipReasonArchive)
			}
		}
	}
	if !found {
		t.Fatalf("ListFiles missing %s", doc.RelPath)
	}

	// Re-opening the same file re-runs applyAdditiveColumnMigrations, which must
	// treat the already-present skip_reason column as a no-op (idempotent).
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st2 := store.NewSQLiteStore(dbPath)
	if err := st2.Init(ctx); err != nil {
		t.Fatalf("re-Init (migration idempotency): %v", err)
	}
	defer func() { _ = st2.Close() }()
	got2, err := st2.GetDocumentByPath(ctx, doc.RelPath)
	if err != nil {
		t.Fatalf("GetDocumentByPath after reopen: %v", err)
	}
	if got2.SkipReason != model.SkipReasonArchive {
		t.Errorf("skip_reason lost across reopen: %q", got2.SkipReason)
	}
}

// TestSQLiteStore_SkipSummary verifies CorpusStats().SkipSummary aggregates
// never-indexed rows by skip_reason across both the 'skipped' and
// 'secret_excluded' lifecycle statuses, and is nil on a corpus with nothing
// skipped so the JSON stays flat.
func TestSQLiteStore_SkipSummary(t *testing.T) {
	ctx := context.Background()
	st := newTempSQLiteStore(t, ctx)
	defer func() { _ = st.Close() }()

	// Nil when nothing has been skipped.
	stats, err := st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats (empty): %v", err)
	}
	if stats.SkipSummary != nil {
		t.Errorf("SkipSummary = %+v, want nil on empty corpus", stats.SkipSummary)
	}

	rows := []model.Document{
		{RelPath: "a.zip", DocType: "archive", Status: "skipped", SkipReason: model.SkipReasonArchive},
		{RelPath: "b.zip", DocType: "archive", Status: "skipped", SkipReason: model.SkipReasonArchive},
		{RelPath: ".env", DocType: "ignore", Status: "skipped", SkipReason: model.SkipReasonIgnoreRule},
		{RelPath: "creds.txt", DocType: "text", Status: "secret_excluded", SkipReason: model.SkipReasonSecretExcluded},
		// An ingested doc and an errored doc must NOT contribute to SkipSummary.
		{RelPath: "ok.txt", DocType: "text", Status: "ok"},
		{RelPath: "broken.pdf", DocType: "pdf", Status: "error", ErrorMessage: "boom"},
	}
	for _, d := range rows {
		if err := st.UpsertDocument(ctx, d); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", d.RelPath, err)
		}
	}

	stats, err = st.CorpusStats(ctx)
	if err != nil {
		t.Fatalf("CorpusStats: %v", err)
	}
	if stats.SkipSummary == nil {
		t.Fatalf("SkipSummary = nil, want populated")
	}
	want := map[string]int64{
		model.SkipReasonArchive:        2,
		model.SkipReasonIgnoreRule:     1,
		model.SkipReasonSecretExcluded: 1,
	}
	for reason, n := range want {
		if got := stats.SkipSummary.Categories[reason]; got != n {
			t.Errorf("SkipSummary.Categories[%q] = %d, want %d", reason, got, n)
		}
	}
	// The ok/error rows must not leak a category into the skip aggregate.
	if _, ok := stats.SkipSummary.Categories[""]; ok {
		t.Errorf("SkipSummary leaked an empty-reason category from ok/error rows: %+v", stats.SkipSummary.Categories)
	}
	if len(stats.SkipSummary.Samples) == 0 {
		t.Errorf("SkipSummary.Samples empty, want representative rows")
	}
}
