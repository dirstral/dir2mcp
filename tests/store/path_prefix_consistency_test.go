package tests

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestListFilesAndSearchPrefixAgree is the cross-check for issue #286 Bug B:
// list_files (the store's normalized LIKE 'prefix%' query) and search/ask (the
// shared model.MatchesPathPrefix matcher) must return the same set of rel_paths
// for the same path_prefix. Driving both off the same seeded corpus proves the
// two code paths can no longer drift on prefix semantics.
func TestListFilesAndSearchPrefixAgree(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "meta.sqlite"))
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	corpus := []string{
		"acts/foo.pdf",
		"acts/sub/bar.pdf",
		"acts2/baz.pdf",
		"Acts/upper.pdf",
		"other/qux.pdf",
	}
	for _, rel := range corpus {
		if err := st.UpsertDocument(ctx, model.Document{
			RelPath:    rel,
			DocType:    "pdf",
			SourceType: "filesystem",
			SizeBytes:  1,
			MTimeUnix:  1,
			Status:     "ok",
		}); err != nil {
			t.Fatalf("upsert %s: %v", rel, err)
		}
	}

	prefixes := []string{"", "acts", "acts/", "./acts", "/acts", "ACTS", "act", "acts/sub", "other", "xyz", "acts/foo.pdf"}
	for _, prefix := range prefixes {
		// list_files side: store LIKE query.
		docs, _, err := st.ListFiles(ctx, prefix, "", 1000, 0)
		if err != nil {
			t.Fatalf("ListFiles(%q): %v", prefix, err)
		}
		listSet := make([]string, 0, len(docs))
		for _, d := range docs {
			listSet = append(listSet, d.RelPath)
		}
		sort.Strings(listSet)

		// search/ask side: shared matcher over the same corpus.
		searchSet := make([]string, 0, len(corpus))
		for _, rel := range corpus {
			if model.MatchesPathPrefix(rel, prefix) {
				searchSet = append(searchSet, rel)
			}
		}
		sort.Strings(searchSet)

		if !equalStrings(listSet, searchSet) {
			t.Errorf("prefix %q: list_files=%v but search=%v (must agree)", prefix, listSet, searchSet)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
