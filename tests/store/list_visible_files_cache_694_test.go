package tests

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #694 pushed the list_files hidden-path policy into the ListFiles query so the
// MCP handler stops walking the whole corpus per page. The trap that comes with
// it is the #429 F10 memo: it caches the exact total per listing, guarded by
// SQLite's data_version, and two visibility policies over the SAME prefix/glob
// are two DIFFERENT listings with two different totals.
//
// If the memo key carries only the filter, the first call's total is served to
// the second for as long as no commit lands. The page rows come from a fresh
// query and are correct, so the response is internally contradictory: a
// hidden-inclusive page reported with a hidden-excluded total (or the reverse),
// which is worse than either being wrong on its own — a client paging on
// `offset < total` silently stops early and never sees the hidden rows.
//
// These calls are deliberately back-to-back with no write in between, because
// that is the only window in which the memo is live.

func TestListVisibleFilesTotalIsNotCrossedBetweenVisibilityPolicies_694(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	seed := []string{
		"a.md",
		"dir/sub/c.md",
		"visible/.config/b.md", // dot in a LATER segment: hidden since #693
		".hidden/a.md",
		".x",
	}
	for i, relPath := range seed {
		if err := st.UpsertDocument(ctx, model.Document{
			RelPath: relPath, DocType: "md", MTimeUnix: int64(100 * (i + 1)), Status: "ok",
		}); err != nil {
			t.Fatalf("seed %s: %v", relPath, err)
		}
	}
	const wantVisible, wantAll = 2, 5

	for _, tc := range []struct {
		name string
		glob string
	}{
		// Both ListFiles paths memoize, and they build their keys separately.
		{name: "sql path", glob: ""},
		{name: "glob path", glob: "**"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, order := range []struct {
				name  string
				first bool // includeHidden of the FIRST call
			}{
				{name: "visible then all", first: false},
				{name: "all then visible", first: true},
			} {
				t.Run(order.name, func(t *testing.T) {
					firstWant, secondWant := int64(wantVisible), int64(wantAll)
					if order.first {
						firstWant, secondWant = int64(wantAll), int64(wantVisible)
					}

					docs, total, err := st.ListVisibleFiles(ctx, "", tc.glob, 50, 0, order.first)
					if err != nil {
						t.Fatalf("first ListVisibleFiles: %v", err)
					}
					if total != firstWant || int64(len(docs)) != firstWant {
						t.Fatalf("first call: rows=%d total=%d want %d", len(docs), total, firstWant)
					}

					docs, total, err = st.ListVisibleFiles(ctx, "", tc.glob, 50, 0, !order.first)
					if err != nil {
						t.Fatalf("second ListVisibleFiles: %v", err)
					}
					if int64(len(docs)) != secondWant {
						t.Fatalf("second call returned %d rows, want %d", len(docs), secondWant)
					}
					if total != secondWant {
						t.Fatalf("second call returned %d rows but total=%d (want %d): the "+
							"memoized total of the previous visibility policy was reused, so the "+
							"page and its total describe different listings (#694)",
							len(docs), total, secondWant)
					}
				})
			}
		})
	}
}

// TestListFilesStaysHiddenInclusive_694 pins the delegation: model.Store's
// ListFiles is unchanged for its ~30 existing callers, so it must keep
// returning dot-prefixed rows.
func TestListFilesStaysHiddenInclusive_694(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for i, relPath := range []string{"a.md", ".hidden/a.md"} {
		if err := st.UpsertDocument(ctx, model.Document{
			RelPath: relPath, DocType: "md", MTimeUnix: int64(100 * (i + 1)), Status: "ok",
		}); err != nil {
			t.Fatalf("seed %s: %v", relPath, err)
		}
	}

	docs, total, err := st.ListFiles(ctx, "", "", 50, 0)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if total != 2 || len(docs) != 2 {
		t.Fatalf("ListFiles rows=%d total=%d, want 2/2: ListFiles must stay hidden-inclusive", len(docs), total)
	}
}
