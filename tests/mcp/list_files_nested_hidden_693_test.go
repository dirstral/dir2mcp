package tests

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #693: `list_files` with `include_hidden=false` still returned nested
// dotfiles and dot-directories, because the hidden-path predicate tested the
// FIRST path segment only. A caller that asked not to see hidden entries saw
// `docs/.env`, `docs/.git/config` and `docs/public/.private/key.txt`, and the
// `total` counted them too.
//
// The rule now tests every segment. These tests drive the real tool, and they
// drive it over BOTH listing paths: the SQL pushdown that production takes, and
// the walk fallback for a store without the ListVisibleFiles capability. The
// two paths hold two copies of the rule, and a caller cannot tell which one
// served the page.

// hiddenCorpus693 pairs each seeded path with the visibility the tool must give
// it when include_hidden=false.
var hiddenCorpus693 = []struct {
	relPath string
	hidden  bool
}{
	{relPath: "a.md", hidden: false},
	{relPath: "docs/readme.md", hidden: false},
	{relPath: "docs/v1.2/file.md", hidden: false}, // a dot INSIDE a name stays visible
	{relPath: ".env", hidden: true},
	{relPath: ".claude/settings.json", hidden: true},
	{relPath: "docs/.env", hidden: true},
	{relPath: "docs/.git/config", hidden: true},
	{relPath: "docs/public/.private/key.txt", hidden: true},
	{relPath: "a/b/c/.deep/d/e.md", hidden: true},
}

func TestListFilesHidesNestedDotPaths_693(t *testing.T) {
	for _, arm := range []struct {
		name     string
		walkOnly bool
	}{
		{name: "sql pushdown", walkOnly: false},
		{name: "walk fallback", walkOnly: true},
	} {
		t.Run(arm.name, func(t *testing.T) {
			tmp, root, st := seedHiddenCorpus693(t)
			var lister model.Store = st
			if arm.walkOnly {
				lister = walkOnly694{st}
			}

			var wantVisible, wantAll []string
			for _, doc := range hiddenCorpus693 {
				wantAll = append(wantAll, doc.relPath)
				if !doc.hidden {
					wantVisible = append(wantVisible, doc.relPath)
				}
			}

			excluded := listFilesPage694(t, tmp, root, lister, `"limit":50,"offset":0,"include_hidden":false`)
			assertRelPathSet694(t, "include_hidden=false", excluded, wantVisible)
			if excluded.Total != int64(len(wantVisible)) {
				t.Fatalf("include_hidden=false total=%d want %d; the total must count the "+
					"same rows the page is drawn from (#693)", excluded.Total, len(wantVisible))
			}

			// include_hidden=true keeps the inclusive behavior it always had.
			included := listFilesPage694(t, tmp, root, lister, `"limit":50,"offset":0,"include_hidden":true`)
			assertRelPathSet694(t, "include_hidden=true", included, wantAll)
			if included.Total != int64(len(wantAll)) {
				t.Fatalf("include_hidden=true total=%d want %d", included.Total, len(wantAll))
			}
		})
	}
}

// TestListFilesPagesOnlyVisibleRows_693 walks the default listing one row per
// page. `files`, `total`, `limit` and `offset` must all describe the same
// filtered set, so a client looping `while offset < total` sees every visible
// file once and no hidden file at all.
func TestListFilesPagesOnlyVisibleRows_693(t *testing.T) {
	tmp, root, st := seedHiddenCorpus693(t)

	var wantVisible []string
	for _, doc := range hiddenCorpus693 {
		if !doc.hidden {
			wantVisible = append(wantVisible, doc.relPath)
		}
	}

	seen := make(map[string]bool, len(wantVisible))
	for offset := 0; offset < len(hiddenCorpus693)+1; offset++ {
		page := listFilesPage694(t, tmp, root, st,
			`"limit":1,"offset":`+strconv.Itoa(offset)+`,"include_hidden":false`)
		if page.Total != int64(len(wantVisible)) {
			t.Fatalf("offset=%d total=%d want %d", offset, page.Total, len(wantVisible))
		}
		if int64(offset) >= page.Total {
			break
		}
		for _, f := range page.Files {
			if seen[f.RelPath] {
				t.Fatalf("offset=%d repeated %q", offset, f.RelPath)
			}
			seen[f.RelPath] = true
		}
	}

	for _, want := range wantVisible {
		if !seen[want] {
			t.Fatalf("the page walk never saw %q (saw %v)", want, keysOf694(seen))
		}
	}
	if len(seen) != len(wantVisible) {
		t.Fatalf("the page walk saw %v, want %v", keysOf694(seen), wantVisible)
	}
}

func seedHiddenCorpus693(t *testing.T) (stateDir, root string, st *store.SQLiteStore) {
	t.Helper()
	tmp := t.TempDir()
	st = store.NewSQLiteStore(filepath.Join(tmp, "meta.sqlite"))
	if err := st.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	root = filepath.Join(tmp, "corpus")
	docs := make([]model.Document, 0, len(hiddenCorpus693))
	for i, doc := range hiddenCorpus693 {
		docs = append(docs, model.Document{
			RelPath: doc.relPath, DocType: "md", MTimeUnix: int64(100 * (i + 1)), Status: "ok",
		})
	}
	seedCorpus(t, st, root, docs)
	return tmp, root, st
}
