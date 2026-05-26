package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestSQLiteStore_RecentFailures_OrderAndLimit pins the contract the
// dir2mcp_stats recent_failures field relies on: newest-first by
// mtime_unix, deterministic rel_path tiebreak, deleted rows excluded,
// limit applied. Backs SPEC §15.6.
func TestSQLiteStore_RecentFailures_OrderAndLimit(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Mixed status + a deleted error row + tied mtime so we exercise
	// every clause: status filter, deleted filter, ORDER BY mtime
	// DESC + rel_path ASC tiebreak, LIMIT.
	rows := []model.Document{
		{RelPath: "a-old.pdf", DocType: "pdf", MTimeUnix: 100, Status: "error", ErrorMessage: "old failure"},
		{RelPath: "b-mid.pdf", DocType: "pdf", MTimeUnix: 200, Status: "error", ErrorMessage: "mid failure"},
		{RelPath: "c-new-z.pdf", DocType: "pdf", MTimeUnix: 300, Status: "error", ErrorMessage: "newest z"},
		{RelPath: "c-new-a.pdf", DocType: "pdf", MTimeUnix: 300, Status: "error", ErrorMessage: "newest a"},
		{RelPath: "ok-doc.md", DocType: "md", MTimeUnix: 999, Status: "ok"},
		{RelPath: "tombstone.pdf", DocType: "pdf", MTimeUnix: 999, Status: "error", ErrorMessage: "deleted", Deleted: true},
	}
	for _, d := range rows {
		if err := st.UpsertDocument(ctx, d); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", d.RelPath, err)
		}
	}

	got, err := st.RecentFailures(ctx, 10)
	if err != nil {
		t.Fatalf("RecentFailures: %v", err)
	}
	wantOrder := []string{"c-new-a.pdf", "c-new-z.pdf", "b-mid.pdf", "a-old.pdf"}
	if len(got) != len(wantOrder) {
		t.Fatalf("RecentFailures returned %d rows, want %d (got=%+v)", len(got), len(wantOrder), got)
	}
	for i, want := range wantOrder {
		if got[i].RelPath != want {
			t.Errorf("position %d: got %q, want %q (full=%+v)", i, got[i].RelPath, want, got)
		}
	}

	// limit caps the result; default-limit (<=0) maps to 20 per spec.
	limited, err := st.RecentFailures(ctx, 2)
	if err != nil {
		t.Fatalf("RecentFailures(limit=2): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("limit=2 returned %d rows", len(limited))
	}
	defaulted, err := st.RecentFailures(ctx, 0)
	if err != nil {
		t.Fatalf("RecentFailures(limit=0): %v", err)
	}
	if len(defaulted) != len(wantOrder) {
		t.Errorf("limit=0 (default 20) returned %d rows, want %d", len(defaulted), len(wantOrder))
	}
}

// TestSQLiteStore_RecentFailures_EmptyOnHealthyCorpus pins the "MAY
// omit when no failures" path: an empty result means the stats tool
// will not include recent_failures in its output.
func TestSQLiteStore_RecentFailures_EmptyOnHealthyCorpus(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := st.UpsertDocument(ctx, model.Document{RelPath: "ok.md", DocType: "md", Status: "ok"}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	got, err := st.RecentFailures(ctx, 10)
	if err != nil {
		t.Fatalf("RecentFailures: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("healthy corpus returned %d failures: %+v", len(got), got)
	}
}
