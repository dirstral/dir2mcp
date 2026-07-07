package tests

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestListFiles_GlobCanonicalSemantics pins that the list_files glob now uses the
// SAME canonical matcher as the search/ask file_glob filter (issue #441): `*` is
// segment-aware (does NOT cross `/`) and `**` is recursive. Previously list_files
// used SQLite GLOB where `*` crossed `/`, so `*.pdf` and `docs/*` returned a
// different file set than the identical file_glob on search/ask.
func TestListFiles_GlobCanonicalSemantics(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	relPaths := []string{
		"root.pdf",
		"docs/guide.pdf",
		"docs/notes.txt",
		"docs/sub/deep.pdf",
		"src/main.go",
	}
	for _, rp := range relPaths {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: rp, DocType: "text", Status: "ok"}); err != nil {
			t.Fatalf("upsert %q: %v", rp, err)
		}
	}

	listGlob := func(glob string) []string {
		docs, total, err := st.ListFiles(ctx, "", glob, 1000, 0)
		if err != nil {
			t.Fatalf("ListFiles(glob=%q): %v", glob, err)
		}
		got := make([]string, 0, len(docs))
		for _, d := range docs {
			got = append(got, d.RelPath)
		}
		sort.Strings(got)
		if int64(len(got)) != total {
			t.Fatalf("ListFiles(glob=%q): len(docs)=%d != total=%d", glob, len(got), total)
		}
		return got
	}

	cases := []struct {
		glob string
		want []string
	}{
		// `*` is segment-aware: `*.pdf` matches only root-level PDFs.
		{"*.pdf", []string{"root.pdf"}},
		// `docs/*` matches direct children of docs/, not nested.
		{"docs/*", []string{"docs/guide.pdf", "docs/notes.txt"}},
		// `**` recurses across segments.
		{"**/*.pdf", []string{"docs/guide.pdf", "docs/sub/deep.pdf", "root.pdf"}},
		{"docs/**", []string{"docs/guide.pdf", "docs/notes.txt", "docs/sub/deep.pdf"}},
	}
	for _, c := range cases {
		got := listGlob(c.glob)
		sort.Strings(c.want)
		if len(got) != len(c.want) {
			t.Errorf("glob %q: got %v want %v", c.glob, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("glob %q: got %v want %v", c.glob, got, c.want)
				break
			}
		}
	}

	// Cross-check: list_files and the file_glob matcher agree on every path.
	for _, c := range cases {
		want := map[string]bool{}
		for _, w := range c.want {
			want[w] = true
		}
		for _, rp := range relPaths {
			viaFilter, err := model.MatchGlob(c.glob, rp)
			if err != nil {
				t.Fatalf("MatchGlob(%q,%q): %v", c.glob, rp, err)
			}
			if viaFilter != want[rp] {
				t.Errorf("divergence glob %q path %q: file_glob=%v list_files=%v", c.glob, rp, viaFilter, want[rp])
			}
		}
	}
}

// TestListFiles_GlobPagination pins that Go-side glob pagination reports the full
// matched total while returning only the requested page.
func TestListFiles_GlobPagination(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for _, rp := range []string{"a.pdf", "b.pdf", "c.pdf", "d.txt"} {
		if err := st.UpsertDocument(ctx, model.Document{RelPath: rp, DocType: "text", Status: "ok"}); err != nil {
			t.Fatalf("upsert %q: %v", rp, err)
		}
	}

	docs, total, err := st.ListFiles(ctx, "", "*.pdf", 2, 1)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if total != 3 {
		t.Errorf("total=%d want 3 (a,b,c .pdf)", total)
	}
	if len(docs) != 2 {
		t.Fatalf("page len=%d want 2", len(docs))
	}
	// Ordered by rel_path, offset 1 -> b.pdf, c.pdf.
	if docs[0].RelPath != "b.pdf" || docs[1].RelPath != "c.pdf" {
		t.Errorf("page = %q,%q want b.pdf,c.pdf", docs[0].RelPath, docs[1].RelPath)
	}
}
