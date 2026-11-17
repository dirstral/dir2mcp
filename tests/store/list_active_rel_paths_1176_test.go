package tests

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
	"github.com/dirstral/dir2mcp/tests/testutil"
)

// TestListActiveRelPaths_MatchesThePagedWalk pins #1176: the one-query listing
// the scan uses returns exactly the documents a full ListFiles walk returns,
// deleted documents excluded, across more than one ListFiles page.
func TestListActiveRelPaths_MatchesThePagedWalk(t *testing.T) {
	ctx := context.Background()
	st := store.NewSQLiteStore(filepath.Join(testutil.TempDir(t), "meta.sqlite"))
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	const n = 1203
	deleted := 0
	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("d%02d/f%04d.txt", i%17, i)
		if err := st.UpsertDocument(ctx, model.Document{RelPath: rel, DocType: "text", Status: "ok"}); err != nil {
			t.Fatalf("upsert %s: %v", rel, err)
		}
		if i%10 == 0 {
			if err := st.MarkDocumentDeleted(ctx, rel); err != nil {
				t.Fatalf("mark deleted %s: %v", rel, err)
			}
			deleted++
		}
	}

	got, err := st.ListActiveRelPaths(ctx)
	if err != nil {
		t.Fatalf("ListActiveRelPaths: %v", err)
	}
	sort.Strings(got)

	var want []string
	for offset := 0; ; {
		docs, total, err := st.ListFiles(ctx, "", "", 500, offset)
		if err != nil {
			t.Fatalf("ListFiles: %v", err)
		}
		for _, d := range docs {
			if !d.Deleted {
				want = append(want, d.RelPath)
			}
		}
		offset += len(docs)
		if len(docs) == 0 || int64(offset) >= total {
			break
		}
	}
	sort.Strings(want)

	if len(got) != n-deleted || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("ListActiveRelPaths returned %d paths, the paged walk %d, want %d; the sets must match",
			len(got), len(want), n-deleted)
	}
}
