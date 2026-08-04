package tests

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #732: ListEmbeddedChunkMetadata applied both of its selective predicates (the
// index_kind filter and the page LIMIT) OUTSIDE the CTE that gathered candidate
// chunks, so every page of the startup warm-load materialized every embedded
// chunk above the keyset cursor and threw all but one page away. A full walk
// therefore cost O(N^2 / pageSize): measured on a synthetic 100k-chunk corpus,
// walking both index kinds at pageSize 500 took 47.4s, and the "text" half alone
// (whose chunk_ids sort first, so each of its pages still dragged the whole
// "code" half through the CTE) took 37.1s against "code"'s 10.2s for the same
// row count.
//
// These tests pin the shape of the fix, which is what keeps it linear: a page
// seeks its rows through an index instead of scanning a table, and it reads
// through the query-only pool rather than the single-connection writer. The
// timings themselves live in TestEmbeddedChunkWalkPerf_732 (opt-in).

// TestEmbeddedChunkPage_BoundsThePageAtCandidateSelection pins the placement of
// the two selective predicates, which is the whole fix. It is a statement-shape
// assertion rather than a plan assertion because the plan alone cannot express
// it: with the kind filter and the LIMIT in the outer query the planner still
// picks a perfectly reasonable index for the CTE, it just runs it over the whole
// corpus and discards the surplus. Placement is what makes a page bounded.
func TestEmbeddedChunkPage_BoundsThePageAtCandidateSelection(t *testing.T) {
	for _, kind := range []string{"text", ""} {
		query, args := store.EmbeddedChunkPageQueryForTest(kind, 500, 42)

		// Everything up to the outer SELECT is the candidate CTE.
		outer := strings.Index(query, "SELECT fc.chunk_id")
		if outer < 0 {
			t.Fatalf("kind=%q: cannot locate the outer SELECT in:\n%s", kind, query)
		}
		cte, tail := query[:outer], query[outer:]

		if !strings.Contains(cte, "LIMIT ?") {
			t.Fatalf("kind=%q: the page LIMIT is not applied at candidate selection; a page will materialize every embedded chunk above the cursor (#732):\n%s", kind, query)
		}
		if strings.Contains(tail, "LIMIT") {
			t.Fatalf("kind=%q: an outer LIMIT is back, so the CTE is unbounded again (#732):\n%s", kind, query)
		}
		if kind != "" {
			if !strings.Contains(cte, "c.index_kind = ?") {
				t.Fatalf("kind=%q: the index_kind filter is not applied at candidate selection (#732):\n%s", kind, query)
			}
			if strings.Contains(tail, "index_kind = ?") {
				t.Fatalf("kind=%q: the index_kind filter is back in the outer query (#732):\n%s", kind, query)
			}
		} else if strings.Contains(query, "index_kind = ?") {
			// The kind is optional and must stay optional: an empty kind means
			// "every embedded chunk" and must not bind a placeholder.
			t.Fatalf("unfiltered page still filters on index_kind:\n%s", query)
		}

		// Moving a predicate moves its placeholder, so the positional arguments
		// have to be rebuilt in step with it: one argument per "?", in order.
		if placeholders := strings.Count(query, "?"); len(args) != placeholders {
			t.Fatalf("kind=%q: %d args for %d placeholders: %v", kind, len(args), placeholders, args)
		}
		if args[0] != "ok" || args[1] != int64(42) {
			t.Fatalf("kind=%q: leading args are %v, want the embedded status then the cursor", kind, args)
		}
		if args[len(args)-1] != 500 {
			t.Fatalf("kind=%q: last arg is %v, want the limit 500", kind, args[len(args)-1])
		}
	}
}

// TestEmbeddedChunkPage_PlanSeeksRatherThanScans backs the placement assertion
// with the plan the placement buys: every table the page touches is reached by a
// seek, so the work per page is proportional to the page, not to the corpus.
func TestEmbeddedChunkPage_PlanSeeksRatherThanScans(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedEmbeddedPageCorpus(t, st)

	t.Run("kind scoped page seeks on the kind too", func(t *testing.T) {
		plan := explainPage(ctx, t, st, "text")
		// index_kind can only appear in a seek term if the filter reached the
		// candidate scan; with the pre-#732 outer filter it never can.
		if !planHas(plan, "index_kind=?") {
			t.Fatalf("kind-scoped page does not seek on index_kind; plan:\n%s", strings.Join(plan, "\n"))
		}
		if !planHas(plan, "idx_chunks_embedded_kind_seek") {
			t.Fatalf("kind-scoped page does not use idx_chunks_embedded_kind_seek; plan:\n%s", strings.Join(plan, "\n"))
		}
		assertNoTableScan(t, plan)
	})

	// The kind argument is optional and must stay that way: the caller that passes
	// "" gets every embedded chunk, and that page has to seek too (through the
	// leading embedding_status column of idx_chunks_embedding_status plus the
	// implicit rowid, since index_kind is unconstrained).
	t.Run("unfiltered page still seeks", func(t *testing.T) {
		plan := explainPage(ctx, t, st, "")
		assertNoTableScan(t, plan)
	})
}

// TestEmbeddedChunkPage_ReadsThroughReadPool pins the second half of #732: the
// listing is SELECT-only, so it belongs on the #631 query-only pool. On the
// single-connection writer handle this test deadlocks until the held write
// transaction is rolled back, because there is exactly one writer connection.
func TestEmbeddedChunkPage_ReadsThroughReadPool(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedEmbeddedPageCorpus(t, st)

	db, _, _, err := st.HandlesForTest(ctx)
	if err != nil {
		t.Fatalf("HandlesForTest: %v", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin write tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE documents SET title = ? WHERE rel_path = ?`, "held", "notes/a.md"); err != nil {
		t.Fatalf("write inside tx: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, rerr := st.ListEmbeddedChunkMetadata(readCtx, "text", 10, 0)
		done <- rerr
	}()

	select {
	case rerr := <-done:
		if rerr != nil {
			t.Fatalf("listing during an open write tx failed: %v", rerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listing blocked behind an open write transaction; it is still on the writer handle")
	}
}

// TestEmbeddedChunkPage_SeekIndexIsCreatedOnAnExistingDatabase pins that a corpus
// indexed before #732 picks the new index up on the next open rather than needing
// a reindex: the index ships in the same idempotent schema block as every other
// one, which runs on every Init. Dropping it stands in for such a database.
func TestEmbeddedChunkPage_SeekIndexIsCreatedOnAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	old := store.NewSQLiteStore(dbPath)
	if err := old.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	db, _, _, err := old.HandlesForTest(ctx)
	if err != nil {
		t.Fatalf("HandlesForTest: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP INDEX idx_chunks_embedded_kind_seek`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := store.NewSQLiteStore(dbPath)
	defer func() { _ = reopened.Close() }()
	if err := reopened.Init(ctx); err != nil {
		t.Fatalf("reopen a pre-#732 database: %v", err)
	}
	plan, err := reopened.ExplainEmbeddedChunkPageForTest(ctx, "text", 500, 0)
	if err != nil {
		t.Fatalf("ExplainEmbeddedChunkPageForTest: %v", err)
	}
	if !planHas(plan, "idx_chunks_embedded_kind_seek") {
		t.Fatalf("reopening a pre-#732 database did not create the seek index; plan:\n%s", strings.Join(plan, "\n"))
	}
}

// TestEmbeddedChunkPage_LimitCountsMatchingRows pins the half of the contract the
// pushed-down LIMIT could most easily have broken: the LIMIT counts rows that
// MATCH the kind, not rows examined before the kind filter. The seeded corpus
// interleaves kinds by chunk_id, so a 2-row "code" page from the start of the
// walk must still return two code chunks.
func TestEmbeddedChunkPage_LimitCountsMatchingRows(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seed := seedEmbeddedPageCorpus(t, st)

	page, err := st.ListEmbeddedChunkMetadata(ctx, "code", 2, 0)
	if err != nil {
		t.Fatalf("ListEmbeddedChunkMetadata(code): %v", err)
	}
	if got := chunkIDs(page); len(got) != 2 || got[0] != seed.code[0] || got[1] != seed.code[1] {
		t.Fatalf("code page = %v, want the first two code chunks %v", got, seed.code[:2])
	}
	for _, task := range page {
		if task.IndexKind != "code" {
			t.Fatalf("chunk %d has index_kind %q on a code page", task.Metadata.ChunkID, task.IndexKind)
		}
	}
}

// TestEmbeddedChunkPage_WalkReturnsEveryRowOnce pins that a full keyset walk
// still yields exactly the embedded chunks of the requested kind, once each, in
// ascending chunk_id order and independent of page size. Pending, failed and
// soft-deleted chunks stay out; an empty kind means every kind.
func TestEmbeddedChunkPage_WalkReturnsEveryRowOnce(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seed := seedEmbeddedPageCorpus(t, st)

	for _, pageSize := range []int{1, 2, 100} {
		if got := walkKind(ctx, t, st, "text", pageSize); !sameIDs(got, seed.text) {
			t.Fatalf("text walk at pageSize %d = %v, want %v", pageSize, got, seed.text)
		}
		if got := walkKind(ctx, t, st, "code", pageSize); !sameIDs(got, seed.code) {
			t.Fatalf("code walk at pageSize %d = %v, want %v", pageSize, got, seed.code)
		}
		want := mergeSorted(seed.text, seed.code)
		if got := walkKind(ctx, t, st, "", pageSize); !sameIDs(got, want) {
			t.Fatalf("unfiltered walk at pageSize %d = %v, want %v", pageSize, got, want)
		}
	}
}

// TestEmbeddedChunkPage_RowPayloadUnchanged pins the per-row payload across the
// join rewrite: the joined document title/mtime, the FIRST span of a multi-span
// chunk (the lowest span_id, which is the row the previous ROW_NUMBER() window
// picked), and a span-less chunk still being returned.
func TestEmbeddedChunkPage_RowPayloadUnchanged(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seed := seedEmbeddedPageCorpus(t, st)

	all, err := st.ListEmbeddedChunkMetadata(ctx, "", 100, 0)
	if err != nil {
		t.Fatalf("ListEmbeddedChunkMetadata(all): %v", err)
	}
	byID := map[uint64]model.ChunkTask{}
	for _, task := range all {
		byID[task.Metadata.ChunkID] = task
	}
	multi, ok := byID[uint64(seed.multiSpan)]
	if !ok {
		t.Fatalf("multi-span chunk %d missing from the listing", seed.multiSpan)
	}
	if multi.Metadata.Span.StartLine != 10 || multi.Metadata.Span.EndLine != 11 {
		t.Fatalf("multi-span chunk carries span %+v, want the first span (lines 10-11)", multi.Metadata.Span)
	}
	if multi.Metadata.Title != "Notes A" {
		t.Fatalf("title = %q, want %q", multi.Metadata.Title, "Notes A")
	}
	if multi.Metadata.MTimeUnix != seedMTime {
		t.Fatalf("mtime = %d, want %d", multi.Metadata.MTimeUnix, seedMTime)
	}

	// A chunk with no spans at all must still be returned, with the zero span the
	// COALESCE defaults produce.
	none, ok := byID[uint64(seed.noSpan)]
	if !ok {
		t.Fatalf("span-less chunk %d missing from the listing", seed.noSpan)
	}
	if none.Metadata.Span.StartLine != 0 || none.Metadata.Span.EndLine != 0 {
		t.Fatalf("span-less chunk carries span %+v, want an empty lines span", none.Metadata.Span)
	}
}

const seedMTime = 1700000000

// embeddedPageSeed records the chunk ids the corpus below was built from.
type embeddedPageSeed struct {
	text      []int64
	code      []int64
	multiSpan int64
	noSpan    int64
}

// seedEmbeddedPageCorpus builds a small corpus whose kinds INTERLEAVE by
// chunk_id (text, code, text, code, ...) plus the rows a page must exclude: a
// pending chunk, a failed chunk and a soft-deleted one.
func seedEmbeddedPageCorpus(t *testing.T, st *store.SQLiteStore) embeddedPageSeed {
	t.Helper()
	ctx := context.Background()
	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: "notes/a.md", DocType: "md", SourceType: "local", Status: "ok",
		Title: "Notes A", MTimeUnix: seedMTime,
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, "notes/a.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}

	var seed embeddedPageSeed
	err = st.WithTx(ctx, func(tx model.RepresentationStore) error {
		repID, rerr := tx.UpsertRepresentation(ctx, model.Representation{
			DocID: doc.DocID, RepType: "raw_text", RepHash: "h1",
		})
		if rerr != nil {
			return rerr
		}
		insert := func(ordinal int, kind, status string, deleted bool, spans []model.Span) (int64, error) {
			return tx.InsertChunkWithSpans(ctx, model.Chunk{
				RepID: repID, Ordinal: ordinal, Text: "chunk " + kind, TextHash: "h",
				IndexKind: kind, EmbeddingStatus: status, Deleted: deleted,
			}, spans)
		}
		ordinal := 0
		for i := 0; i < 3; i++ {
			id, cerr := insert(ordinal, "text", "ok", false, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 2}})
			if cerr != nil {
				return cerr
			}
			seed.text = append(seed.text, id)
			ordinal++
			id, cerr = insert(ordinal, "code", "ok", false, []model.Span{{Kind: "lines", StartLine: 3, EndLine: 4}})
			if cerr != nil {
				return cerr
			}
			seed.code = append(seed.code, id)
			ordinal++
		}
		// Rows a page must never return.
		if _, cerr := insert(ordinal, "text", "pending", false, nil); cerr != nil {
			return cerr
		}
		ordinal++
		if _, cerr := insert(ordinal, "text", "failed", false, nil); cerr != nil {
			return cerr
		}
		ordinal++
		if _, cerr := insert(ordinal, "text", "ok", true, nil); cerr != nil {
			return cerr
		}
		ordinal++
		// A multi-span chunk (the page must carry its FIRST span) and a span-less one.
		id, cerr := insert(ordinal, "text", "ok", false, []model.Span{
			{Kind: "lines", StartLine: 10, EndLine: 11},
			{Kind: "lines", StartLine: 20, EndLine: 21},
		})
		if cerr != nil {
			return cerr
		}
		seed.multiSpan = id
		seed.text = append(seed.text, id)
		ordinal++
		id, cerr = insert(ordinal, "text", "ok", false, nil)
		if cerr != nil {
			return cerr
		}
		seed.noSpan = id
		seed.text = append(seed.text, id)
		return nil
	})
	if err != nil {
		t.Fatalf("seed corpus: %v", err)
	}
	return seed
}

// explainPage returns the query plan for one page, failing the test on error.
func explainPage(ctx context.Context, t *testing.T, st *store.SQLiteStore, kind string) []string {
	t.Helper()
	plan, err := st.ExplainEmbeddedChunkPageForTest(ctx, kind, 500, 0)
	if err != nil {
		t.Fatalf("ExplainEmbeddedChunkPageForTest(kind=%q): %v", kind, err)
	}
	if len(plan) == 0 {
		t.Fatalf("empty query plan for kind=%q", kind)
	}
	return plan
}

func planHas(plan []string, needle string) bool {
	for _, line := range plan {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

// assertNoTableScan fails if the plan scans any of the three base tables the page
// touches. Scanning the materialized page itself ("SCAN fc") is fine: it holds at
// most one page of rows.
func assertNoTableScan(t *testing.T, plan []string) {
	t.Helper()
	scanned := map[string]string{
		"SCAN c":         "chunks",
		"SCAN chunks":    "chunks",
		"SCAN s":         "spans",
		"SCAN sp":        "spans",
		"SCAN spans":     "spans",
		"SCAN d":         "documents",
		"SCAN documents": "documents",
	}
	for _, line := range plan {
		for prefix, table := range scanned {
			if strings.HasPrefix(line, prefix+" ") || line == prefix {
				t.Fatalf("page scans the whole %s table (%q); a page must seek. Full plan:\n%s",
					table, line, strings.Join(plan, "\n"))
			}
		}
	}
}

// walkKind pages through one kind with the caller's keyset protocol and returns
// the chunk ids in the order they were read.
func walkKind(ctx context.Context, t *testing.T, st *store.SQLiteStore, kind string, pageSize int) []int64 {
	t.Helper()
	var (
		out          []int64
		afterChunkID int64
	)
	for {
		page, err := st.ListEmbeddedChunkMetadata(ctx, kind, pageSize, afterChunkID)
		if err != nil {
			t.Fatalf("ListEmbeddedChunkMetadata(kind=%q, after=%d): %v", kind, afterChunkID, err)
		}
		out = append(out, chunkIDs(page)...)
		if len(page) < pageSize {
			return out
		}
		afterChunkID = int64(page[len(page)-1].Metadata.ChunkID)
	}
}

func chunkIDs(tasks []model.ChunkTask) []int64 {
	out := make([]int64, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, int64(task.Metadata.ChunkID))
	}
	return out
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// mergeSorted merges two ascending id lists into one ascending list.
func mergeSorted(a, b []int64) []int64 {
	out := make([]int64, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] <= b[j] {
			out = append(out, a[i])
			i++
			continue
		}
		out = append(out, b[j])
		j++
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}
