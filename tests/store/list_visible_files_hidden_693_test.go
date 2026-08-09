package tests

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/relpath"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #693: `list_files include_hidden=false` hid a path only when its FIRST
// segment started with a dot. A dotfile below a visible directory
// (`docs/.env`, `docs/.git/config`) was listed as an ordinary file, so the
// flag's meaning changed with directory depth.
//
// The rule is now "hide a path when ANY segment starts with a dot", and it
// exists in two forms: relpath.IsHidden for the handler's walk fallback, and
// relpath.NotHiddenSQL for the store's paged query. The tests below pin the Go
// form on its own and then hold the SQL form against it, so the pair cannot
// drift (#716).

// hiddenCorpus693 is the shared fixture: every path plus the answer the rule
// must give for it.
var hiddenCorpus693 = []struct {
	relPath string
	hidden  bool
}{
	{relPath: "a.md", hidden: false},
	{relPath: "dir/sub/c.md", hidden: false},
	{relPath: "docs/v1.2/file.md", hidden: false},           // dots INSIDE names
	{relPath: "docs/report.final.md", hidden: false},        // a dotted file name
	{relPath: "docs/v1..2/notes.md", hidden: false},         // not a `..` segment
	{relPath: ".env", hidden: true},                         // first segment
	{relPath: ".claude/settings.json", hidden: true},        // first segment
	{relPath: "docs/.env", hidden: true},                    // depth 2
	{relPath: "docs/.git/config", hidden: true},             // dot DIRECTORY
	{relPath: "docs/public/.private/key.txt", hidden: true}, // depth 3
	{relPath: "a/b/c/.deep/d/e.md", hidden: true},           // depth 4
}

func TestHiddenRelPathRuleChecksEverySegment_693(t *testing.T) {
	for _, tc := range hiddenCorpus693 {
		if got := relpath.IsHidden(tc.relPath); got != tc.hidden {
			t.Errorf("relpath.IsHidden(%q)=%v want %v", tc.relPath, got, tc.hidden)
		}
	}

	// Paths that name no file, and paths that are not valid rel_paths. A
	// listing filter must answer them without panicking, and must fail toward
	// hiding when the path is malformed.
	for _, tc := range []struct {
		relPath string
		hidden  bool
	}{
		{relPath: "", hidden: false},
		{relPath: "   ", hidden: false},
		{relPath: ".", hidden: false},
		{relPath: "./docs/readme.md", hidden: false},
		{relPath: "./docs/.env", hidden: true},
		{relPath: "docs/./.env", hidden: true},
		{relPath: "docs/sub/../.env", hidden: true},
		{relPath: "docs/.git/../readme.md", hidden: false},
		{relPath: "..", hidden: true},
		{relPath: "../outside.md", hidden: true},
	} {
		if got := relpath.IsHidden(tc.relPath); got != tc.hidden {
			t.Errorf("relpath.IsHidden(%q)=%v want %v", tc.relPath, got, tc.hidden)
		}
	}
}

// TestListVisibleFilesHidesEverySegment_693 holds the SQL form against the Go
// form over the same corpus. Both listing paths of the store are covered: the
// plain prefix query and the Go-side glob scan.
func TestListVisibleFilesHidesEverySegment_693(t *testing.T) {
	ctx := context.Background()
	st := seedHiddenCorpus693(t)

	var wantVisible, wantAll []string
	for _, tc := range hiddenCorpus693 {
		wantAll = append(wantAll, tc.relPath)
		if !relpath.IsHidden(tc.relPath) {
			wantVisible = append(wantVisible, tc.relPath)
		}
	}

	for _, arm := range []struct{ name, glob string }{
		{name: "sql path", glob: ""},
		{name: "glob path", glob: "**"},
	} {
		t.Run(arm.name, func(t *testing.T) {
			docs, total, err := st.ListVisibleFiles(ctx, "", arm.glob, 50, 0, false)
			if err != nil {
				t.Fatalf("ListVisibleFiles: %v", err)
			}
			assertRelPaths693(t, "include_hidden=false", docs, wantVisible)
			if total != int64(len(wantVisible)) {
				t.Fatalf("include_hidden=false total=%d want %d; the total must count "+
					"the same rows the page is drawn from (#693)", total, len(wantVisible))
			}

			docs, total, err = st.ListVisibleFiles(ctx, "", arm.glob, 50, 0, true)
			if err != nil {
				t.Fatalf("ListVisibleFiles: %v", err)
			}
			assertRelPaths693(t, "include_hidden=true", docs, wantAll)
			if total != int64(len(wantAll)) {
				t.Fatalf("include_hidden=true total=%d want %d", total, len(wantAll))
			}
		})
	}
}

// TestListVisibleFilesPagesTheVisibleSet_693 walks the listing one row at a
// time. limit, offset and total must all describe the filtered set, so the walk
// must see every visible path exactly once and no hidden path at all.
func TestListVisibleFilesPagesTheVisibleSet_693(t *testing.T) {
	ctx := context.Background()
	st := seedHiddenCorpus693(t)

	var wantVisible []string
	for _, tc := range hiddenCorpus693 {
		if !relpath.IsHidden(tc.relPath) {
			wantVisible = append(wantVisible, tc.relPath)
		}
	}

	var walked []string
	for offset := 0; ; offset++ {
		docs, total, err := st.ListVisibleFiles(ctx, "", "", 1, offset, false)
		if err != nil {
			t.Fatalf("ListVisibleFiles offset=%d: %v", offset, err)
		}
		if total != int64(len(wantVisible)) {
			t.Fatalf("offset=%d total=%d want %d", offset, total, len(wantVisible))
		}
		if len(docs) == 0 {
			break
		}
		walked = append(walked, docs[0].RelPath)
		if offset > len(hiddenCorpus693) {
			t.Fatalf("the walk did not end after %d pages", offset)
		}
	}

	sort.Strings(walked)
	want := append([]string{}, wantVisible...)
	sort.Strings(want)
	if len(walked) != len(want) {
		t.Fatalf("the walk saw %v, want %v", walked, want)
	}
	for i := range want {
		if walked[i] != want[i] {
			t.Fatalf("the walk saw %v, want %v", walked, want)
		}
	}
}

func seedHiddenCorpus693(t *testing.T) *store.SQLiteStore {
	t.Helper()
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	if err := st.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for i, tc := range hiddenCorpus693 {
		if err := st.UpsertDocument(ctx, model.Document{
			RelPath: tc.relPath, DocType: "md", MTimeUnix: int64(100 * (i + 1)), Status: "ok",
		}); err != nil {
			t.Fatalf("seed %s: %v", tc.relPath, err)
		}
	}
	return st
}

func assertRelPaths693(t *testing.T, label string, docs []model.Document, want []string) {
	t.Helper()
	got := make(map[string]bool, len(docs))
	for _, doc := range docs {
		got[doc.RelPath] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("%s: %q is missing from the listing", label, w)
		}
		delete(got, w)
	}
	for leftover := range got {
		t.Errorf("%s: %q must not be listed (#693)", label, leftover)
	}
}
