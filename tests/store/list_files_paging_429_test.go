package tests

import (
	"context"
	"fmt"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #429 F10: ListFiles used to run a `SELECT COUNT(*)` on EVERY page of the
// glob-free path, and to rescan plus re-glob every matching row on EVERY page of
// the glob path. Walking a corpus therefore cost O(files) count scans and
// O(files) glob rescans. These tests pin the reduction and, just as importantly,
// pin that the total ListFiles returns stays exact while it does.

// seedListFilesCorpus writes n documents, alternating between a docs/*.md file
// (which the tests' glob matches) and a src/*.go file (which it does not), and
// returns the rel_paths in rel_path order.
func seedListFilesCorpus(t *testing.T, st *store.SQLiteStore, n int) (all, markdown []string) {
	t.Helper()
	ctx := context.Background()
	for i := range n {
		rel := fmt.Sprintf("src/f%05d.go", i)
		if i%2 == 0 {
			rel = fmt.Sprintf("docs/f%05d.md", i)
		}
		if err := st.UpsertDocument(ctx, model.Document{RelPath: rel, DocType: "text", Status: "ok"}); err != nil {
			t.Fatalf("upsert %q: %v", rel, err)
		}
	}
	// rel_path order: every docs/ path sorts before every src/ path, and the
	// zero-padded counter keeps each group in numeric order.
	for i := 0; i < n; i += 2 {
		markdown = append(markdown, fmt.Sprintf("docs/f%05d.md", i))
	}
	all = append(all, markdown...)
	for i := 1; i < n; i += 2 {
		all = append(all, fmt.Sprintf("src/f%05d.go", i))
	}
	return all, markdown
}

func relPaths(docs []model.Document) []string {
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.RelPath)
	}
	return out
}

func assertSameOrder(t *testing.T, got, want []string, what string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d paths, want %d", what, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: path %d = %q, want %q", what, i, got[i], want[i])
		}
	}
}

// walkListFiles pages through ListFiles exactly the way the ingest, MCP and CLI
// callers do, asserting the total is the same exact value on every page.
func walkListFiles(t *testing.T, st *store.SQLiteStore, glob string, pageSize int, wantTotal int64) []string {
	t.Helper()
	ctx := context.Background()
	var got []string
	for offset := 0; ; offset += pageSize {
		docs, total, err := st.ListFiles(ctx, "", glob, pageSize, offset)
		if err != nil {
			t.Fatalf("ListFiles(offset=%d): %v", offset, err)
		}
		if total != wantTotal {
			t.Fatalf("ListFiles(offset=%d): total = %d, want %d", offset, total, wantTotal)
		}
		got = append(got, relPaths(docs)...)
		if len(docs) < pageSize {
			break
		}
	}
	return got
}

// TestListFiles_PagedWalkDoesNotRecountPerPage is the glob-free half of #429
// F10. Reverting the memo makes CountQueries equal the number of pages.
func TestListFiles_PagedWalkDoesNotRecountPerPage(t *testing.T) {
	st := newTestStore(t)
	all, _ := seedListFilesCorpus(t, st, 50)

	const pageSize = 10
	got := walkListFiles(t, st, "", pageSize, int64(len(all)))
	assertSameOrder(t, got, all, "paged walk")

	stats := st.ListFilesQueryStatsForTest()
	if stats.CountQueries > 1 {
		t.Errorf(
			"walking %d documents in pages of %d ran %d COUNT(*) queries, want at most 1 (#429 F10: it used to be one per page)",
			len(all), pageSize, stats.CountQueries,
		)
	}
}

// TestListFiles_GlobPagedWalkDoesNotRescanPerPage is the glob half of #429 F10.
// Reverting the change makes GlobFullScans equal the number of pages.
func TestListFiles_GlobPagedWalkDoesNotRescanPerPage(t *testing.T) {
	st := newTestStore(t)
	_, markdown := seedListFilesCorpus(t, st, 50)

	const pageSize = 10
	got := walkListFiles(t, st, "docs/*.md", pageSize, int64(len(markdown)))
	assertSameOrder(t, got, markdown, "paged glob walk")

	stats := st.ListFilesQueryStatsForTest()
	if stats.GlobFullScans != 1 {
		t.Errorf(
			"walking %d matches in pages of %d ran %d full glob scans, want exactly 1 (#429 F10: it used to be one per page)",
			len(markdown), pageSize, stats.GlobFullScans,
		)
	}
	if stats.GlobPageScans == 0 {
		t.Errorf("no page resumed from a recorded boundary; the memo was never used")
	}
}

// TestListFiles_GlobPagesMatchUnpagedListing pins that resuming from a recorded
// boundary returns the same rows the old full-rescan-per-page code did, for
// offsets that are NOT multiples of the boundary stride as well as those that
// are, and for offsets past the end.
func TestListFiles_GlobPagesMatchUnpagedListing(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	_, markdown := seedListFilesCorpus(t, st, 1200)
	total := int64(len(markdown))

	// Prime the memo (and its boundary index) with a first page.
	if _, got, err := listFilesPage(ctx, st, "docs/*.md", 100, 0); err != nil {
		t.Fatalf("prime: %v", err)
	} else if got != total {
		t.Fatalf("prime: total = %d, want %d", got, total)
	}

	for _, tc := range []struct{ offset, limit int }{
		{0, 100},
		{1, 100},
		{99, 7},
		{100, 100},
		{255, 100},
		{256, 100},
		{257, 3},
		{300, 100},
		{599, 1},
		{len(markdown) - 1, 100},
		{len(markdown), 100},
		{len(markdown) + 500, 100},
	} {
		docs, got, err := listFilesPage(ctx, st, "docs/*.md", tc.limit, tc.offset)
		if err != nil {
			t.Fatalf("ListFiles(limit=%d offset=%d): %v", tc.limit, tc.offset, err)
		}
		if got != total {
			t.Errorf("ListFiles(limit=%d offset=%d): total = %d, want %d", tc.limit, tc.offset, got, total)
		}
		want := markdown[min(tc.offset, len(markdown)):]
		if len(want) > tc.limit {
			want = want[:tc.limit]
		}
		assertSameOrder(t, relPaths(docs), want, fmt.Sprintf("page(limit=%d offset=%d)", tc.limit, tc.offset))
	}

	// An offset the memo already proves is past the end must not scan at all.
	before := st.ListFilesQueryStatsForTest()
	if _, total, err := listFilesPage(ctx, st, "docs/*.md", 100, len(markdown)+1000); err != nil {
		t.Fatalf("far past-the-end page: %v", err)
	} else if total != int64(len(markdown)) {
		t.Errorf("far past-the-end page: total = %d, want %d", total, len(markdown))
	}
	if after := st.ListFilesQueryStatsForTest(); after != before {
		t.Errorf("a page the memo proves is past the end still queried: %+v -> %+v", before, after)
	}
}

// listFilesPage is ListFiles with no path prefix, so the tests read as one call
// site for both the glob-free and the glob filter.
func listFilesPage(ctx context.Context, st *store.SQLiteStore, glob string, limit, offset int) ([]model.Document, int64, error) {
	return st.ListFiles(ctx, "", glob, limit, offset)
}

// TestListFiles_TotalFollowsWrites is the correctness guard on the memo. The
// total is memoized only while SQLite's data_version is unchanged, so the first
// commit from the writer must be reflected immediately: a stale total here would
// be worse than the per-page COUNT the memo replaces.
func TestListFiles_TotalFollowsWrites(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	all, markdown := seedListFilesCorpus(t, st, 30)

	if _, total, err := listFilesPage(ctx, st, "", 10, 0); err != nil {
		t.Fatalf("first page: %v", err)
	} else if total != int64(len(all)) {
		t.Fatalf("first page: total = %d, want %d", total, len(all))
	}
	if _, total, err := listFilesPage(ctx, st, "docs/*.md", 10, 0); err != nil {
		t.Fatalf("first glob page: %v", err)
	} else if total != int64(len(markdown)) {
		t.Fatalf("first glob page: total = %d, want %d", total, len(markdown))
	}

	if err := st.UpsertDocument(ctx, model.Document{RelPath: "docs/zz-new.md", DocType: "text", Status: "ok"}); err != nil {
		t.Fatalf("upsert new document: %v", err)
	}

	if _, total, err := listFilesPage(ctx, st, "", 10, 0); err != nil {
		t.Fatalf("page after insert: %v", err)
	} else if total != int64(len(all)+1) {
		t.Errorf("page after insert: total = %d, want %d (memo outlived a commit)", total, len(all)+1)
	}
	if _, total, err := listFilesPage(ctx, st, "docs/*.md", 10, 0); err != nil {
		t.Fatalf("glob page after insert: %v", err)
	} else if total != int64(len(markdown)+1) {
		t.Errorf("glob page after insert: total = %d, want %d (memo outlived a commit)", total, len(markdown)+1)
	}

	// A tombstone drops the document from the listing the same way.
	if err := st.MarkDocumentDeleted(ctx, "docs/zz-new.md"); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	if _, total, err := listFilesPage(ctx, st, "", 10, 0); err != nil {
		t.Fatalf("page after delete: %v", err)
	} else if total != int64(len(all)) {
		t.Errorf("page after delete: total = %d, want %d (memo outlived a commit)", total, len(all))
	}
	if _, total, err := listFilesPage(ctx, st, "docs/*.md", 10, 0); err != nil {
		t.Fatalf("glob page after delete: %v", err)
	} else if total != int64(len(markdown)) {
		t.Errorf("glob page after delete: total = %d, want %d (memo outlived a commit)", total, len(markdown))
	}
}

// TestListFiles_TotalExactWithoutCount pins the second half of the reduction:
// a page that comes back shorter than its limit proves where the result set
// ends, so no COUNT is needed at all. An EMPTY page past the end proves nothing,
// and must still report the real total rather than the offset it was asked for.
func TestListFiles_TotalExactWithoutCount(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	all, _ := seedListFilesCorpus(t, st, 7)

	docs, total, err := listFilesPage(ctx, st, "", 100, 0)
	if err != nil {
		t.Fatalf("single page: %v", err)
	}
	if total != int64(len(all)) {
		t.Fatalf("single page: total = %d, want %d", total, len(all))
	}
	assertSameOrder(t, relPaths(docs), all, "single page")
	if stats := st.ListFilesQueryStatsForTest(); stats.CountQueries != 0 {
		t.Errorf("a page shorter than its limit ran %d COUNT(*) queries, want 0", stats.CountQueries)
	}

	// Fresh store so this exercises the uncached path: an offset past the end
	// must not report the offset as the total.
	other := newTestStore(t)
	otherAll, _ := seedListFilesCorpus(t, other, 9)
	docs, total, err = listFilesPage(ctx, other, "", 10, 1000)
	if err != nil {
		t.Fatalf("past-the-end page: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("past-the-end page returned %d documents, want 0", len(docs))
	}
	if total != int64(len(otherAll)) {
		t.Errorf("past-the-end page: total = %d, want %d", total, len(otherAll))
	}
}

// TestGlobBoundsIndex_SurvivesCompaction pins the invariant the glob resume path
// depends on: bounds[i] is always the match at index i*stride, including after
// the index has been compacted (which halves it and doubles the stride). In
// production the cap is 4096 boundaries, i.e. corpora of millions of matching
// documents, so the cap is lowered here through the test seam.
func TestGlobBoundsIndex_SurvivesCompaction(t *testing.T) {
	matches := make([]string, 5000)
	for i := range matches {
		matches[i] = fmt.Sprintf("docs/f%05d.md", i)
	}

	for _, probeOffset := range []int{0, 1, 255, 256, 999, 4999, 6000} {
		stride, bounds, start, skip := store.GlobBoundsIndexForTest(1, 8, probeOffset, matches)
		if stride <= 0 {
			t.Fatalf("stride = %d, want > 0", stride)
		}
		if len(bounds) > 8 {
			t.Fatalf("bounds = %d entries, want at most the cap of 8", len(bounds))
		}
		for i, b := range bounds {
			if want := matches[i*stride]; b != want {
				t.Fatalf("bounds[%d] = %q, want %q (stride=%d)", i, b, want, stride)
			}
		}
		// Resuming at start and discarding skip matches must land exactly on the
		// requested offset, which is what makes a resumed page identical to a
		// full rescan.
		if start == "" {
			if skip != probeOffset {
				t.Fatalf("no boundary for offset %d: skip = %d, want %d", probeOffset, skip, probeOffset)
			}
			continue
		}
		idx := -1
		for i, m := range matches {
			if m == start {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("boundary %q is not a match", start)
		}
		if idx+skip != probeOffset {
			t.Fatalf("resume for offset %d lands on %d (start index %d + skip %d)", probeOffset, idx+skip, idx, skip)
		}
	}
}
