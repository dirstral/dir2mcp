package tests

import (
	"context"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// #743: NextPending carried the same defect #732 fixed in
// ListEmbeddedChunkMetadata. The index_kind filter and the batch LIMIT sat
// OUTSIDE the CTE that gathered candidate chunks, so every call materialized
// the entire pending set and threw all but one batch away.
//
// The blast radius differs. The metadata walk is driven by a keyset cursor, so
// the repeated materialization compounded into O(N^2) across a startup walk.
// NextPending has no cursor, so a call is O(pending) rather than quadratic. But
// it runs once per embed cycle for the life of an ingest, so a deep queue paid
// a full scan of everything still pending on every batch.
//
// These tests pin the shape of the fix and the contract it had to preserve.

// TestNextPending_BoundsTheBatchAtCandidateSelection pins the placement of the
// two selective predicates, which is the whole fix. As #732 established this
// has to be a statement-shape assertion rather than a plan assertion: with the
// kind filter and the LIMIT in the outer query the planner still picks a
// perfectly reasonable index for the CTE, it just runs it over the whole
// pending set and discards the surplus, so a plan-only test passes against
// unfixed code.
func TestNextPending_BoundsTheBatchAtCandidateSelection(t *testing.T) {
	for _, kind := range []string{"text", ""} {
		query, args := store.NextPendingQueryForTest(kind, 32)

		// Everything up to the outer SELECT is the candidate CTE.
		outer := strings.Index(query, "SELECT fc.chunk_id")
		if outer < 0 {
			t.Fatalf("kind=%q: cannot locate the outer SELECT in:\n%s", kind, query)
		}
		cte, tail := query[:outer], query[outer:]

		if !strings.Contains(cte, "LIMIT ?") {
			t.Fatalf("kind=%q: the batch LIMIT is not applied at candidate selection; every embed cycle will materialize the whole pending set (#743):\n%s", kind, query)
		}
		if strings.Contains(tail, "LIMIT") {
			t.Fatalf("kind=%q: an outer LIMIT is back, so the CTE is unbounded again (#743):\n%s", kind, query)
		}
		// The ROW_NUMBER() window is what #742 measured driving the spans join
		// off a full table scan once the LIMIT moved down.
		if strings.Contains(query, "ROW_NUMBER()") {
			t.Fatalf("kind=%q: the ROW_NUMBER() window is back; with the LIMIT pushed down it makes SQLite scan the whole spans table (#742, #743):\n%s", kind, query)
		}
		if kind != "" {
			if !strings.Contains(cte, "c.index_kind = ?") {
				t.Fatalf("kind=%q: the index_kind filter is not applied at candidate selection (#743):\n%s", kind, query)
			}
			if strings.Contains(tail, "index_kind = ?") {
				t.Fatalf("kind=%q: the index_kind filter is back in the outer query (#743):\n%s", kind, query)
			}
		} else if strings.Contains(query, "index_kind = ?") {
			// The kind is optional and must stay optional: an empty kind means
			// "every pending chunk" and must not bind a placeholder.
			t.Fatalf("unfiltered batch still filters on index_kind:\n%s", query)
		}

		// Moving a predicate moves its placeholder, so the positional arguments
		// have to be rebuilt in step with it: one argument per "?", in order.
		if placeholders := strings.Count(query, "?"); len(args) != placeholders {
			t.Fatalf("kind=%q: %d args for %d placeholders: %v", kind, len(args), placeholders, args)
		}
		if args[0] != "pending" {
			t.Fatalf("kind=%q: leading arg is %v, want the pending status", kind, args[0])
		}
		if args[len(args)-1] != 32 {
			t.Fatalf("kind=%q: last arg is %v, want the limit 32", kind, args[len(args)-1])
		}
	}
}

// TestNextPending_PlanSeeksRatherThanScans backs the placement assertion with
// the plan it buys. #743 predicted idx_chunks_embedded_kind_seek (added by
// #742) would already serve this access path; this pins that it does, so no
// second index was needed.
func TestNextPending_PlanSeeksRatherThanScans(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedPendingCorpus(t, st)

	t.Run("kind scoped batch seeks on the kind too", func(t *testing.T) {
		plan, err := st.ExplainNextPendingForTest(ctx, "text", 32)
		if err != nil {
			t.Fatalf("ExplainNextPendingForTest: %v", err)
		}
		if !planHas(plan, "index_kind=?") {
			t.Fatalf("kind-scoped batch does not seek on index_kind; plan:\n%s", strings.Join(plan, "\n"))
		}
		if !planHas(plan, "idx_chunks_embedded_kind_seek") {
			t.Fatalf("kind-scoped batch does not use idx_chunks_embedded_kind_seek, so #742's index does not cover #743 after all; plan:\n%s", strings.Join(plan, "\n"))
		}
		assertNoTableScan(t, plan)
	})

	t.Run("unfiltered batch still seeks", func(t *testing.T) {
		plan, err := st.ExplainNextPendingForTest(ctx, "", 32)
		if err != nil {
			t.Fatalf("ExplainNextPendingForTest: %v", err)
		}
		assertNoTableScan(t, plan)
	})
}

// TestNextPending_LimitCountsMatchingRows pins the half of the contract the
// pushed-down LIMIT could most easily have broken: the LIMIT counts rows that
// MATCH the kind, not rows examined before the kind filter. The seeded corpus
// interleaves kinds by chunk_id, so a 2-row "code" batch must still return two
// code chunks rather than whatever the first two pending rows happened to be.
func TestNextPending_LimitCountsMatchingRows(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seed := seedPendingCorpus(t, st)

	batch, err := st.NextPending(ctx, 2, "code")
	if err != nil {
		t.Fatalf("NextPending(code): %v", err)
	}
	if got := chunkIDs(batch); len(got) != 2 || got[0] != seed.code[0] || got[1] != seed.code[1] {
		t.Fatalf("code batch = %v, want the first two pending code chunks %v", got, seed.code[:2])
	}
	for _, task := range batch {
		if task.IndexKind != "code" {
			t.Fatalf("chunk %d has index_kind %q on a code batch", task.Metadata.ChunkID, task.IndexKind)
		}
	}
}

// TestNextPending_ExcludesChunksOfErroredOrDeletedDocuments pins the predicate
// that had to stay INSIDE the CTE when the LIMIT moved in beside it. A chunk
// whose parent document is errored or tombstoned must never reach the embed
// worker, and a chunk with no document row at all must still be handed over.
func TestNextPending_ExcludesChunksOfErroredOrDeletedDocuments(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seed := seedPendingCorpus(t, st)

	batch, err := st.NextPending(ctx, 100, "")
	if err != nil {
		t.Fatalf("NextPending(all): %v", err)
	}
	got := map[int64]bool{}
	for _, id := range chunkIDs(batch) {
		got[id] = true
	}
	if got[seed.erroredDoc] {
		t.Fatalf("chunk %d of an errored document was handed to the embed worker", seed.erroredDoc)
	}
	if got[seed.deletedDoc] {
		t.Fatalf("chunk %d of a tombstoned document was handed to the embed worker", seed.deletedDoc)
	}
	if !got[seed.orphan] {
		t.Fatalf("chunk %d has no document row and must still be embeddable", seed.orphan)
	}
}

// TestNextPending_RowPayloadUnchanged pins the per-row payload across the join
// rewrite: the FIRST span of a multi-span chunk (the lowest span_id, which is
// the row the previous ROW_NUMBER() window picked), the joined document mtime,
// and a span-less chunk still being returned.
func TestNextPending_RowPayloadUnchanged(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seed := seedPendingCorpus(t, st)

	batch, err := st.NextPending(ctx, 100, "")
	if err != nil {
		t.Fatalf("NextPending(all): %v", err)
	}
	byID := map[uint64]model.ChunkTask{}
	for _, task := range batch {
		byID[task.Metadata.ChunkID] = task
	}

	multi, ok := byID[uint64(seed.multiSpan)]
	if !ok {
		t.Fatalf("multi-span chunk %d missing from the batch", seed.multiSpan)
	}
	if multi.Metadata.Span.StartLine != 10 || multi.Metadata.Span.EndLine != 11 {
		t.Fatalf("multi-span chunk carries span %+v, want the first span (lines 10-11)", multi.Metadata.Span)
	}
	if multi.Metadata.MTimeUnix != seedMTime {
		t.Fatalf("mtime = %d, want %d", multi.Metadata.MTimeUnix, seedMTime)
	}

	none, ok := byID[uint64(seed.noSpan)]
	if !ok {
		t.Fatalf("span-less chunk %d missing from the batch", seed.noSpan)
	}
	if none.Metadata.Span.StartLine != 0 || none.Metadata.Span.EndLine != 0 {
		t.Fatalf("span-less chunk carries span %+v, want an empty lines span", none.Metadata.Span)
	}
}

// TestNextPending_DrainsEveryPendingChunkOnce pins that repeatedly asking for
// the next batch, marking each one embedded as the worker does, still yields
// every pending chunk exactly once and in ascending chunk_id order, at any
// batch size.
func TestNextPending_DrainsEveryPendingChunkOnce(t *testing.T) {
	for _, batchSize := range []int{1, 2, 100} {
		ctx := context.Background()
		st := newTestStore(t)
		seed := seedPendingCorpus(t, st)
		want := mergeSorted(seed.text, seed.code)

		var drained []int64
		for {
			batch, err := st.NextPending(ctx, batchSize, "")
			if err != nil {
				t.Fatalf("batchSize=%d: NextPending: %v", batchSize, err)
			}
			if len(batch) == 0 {
				break
			}
			labels := make([]uint64, 0, len(batch))
			for _, task := range batch {
				drained = append(drained, int64(task.Metadata.ChunkID))
				labels = append(labels, task.Metadata.ChunkID)
			}
			if err := st.MarkEmbedded(ctx, labels); err != nil {
				t.Fatalf("batchSize=%d: MarkEmbedded: %v", batchSize, err)
			}
		}
		if !sameIDs(drained, want) {
			t.Fatalf("batchSize=%d: drained %v, want %v", batchSize, drained, want)
		}
	}
}

// pendingSeed records the chunk ids the corpus below was built from.
type pendingSeed struct {
	text       []int64
	code       []int64
	multiSpan  int64
	noSpan     int64
	orphan     int64
	erroredDoc int64
	deletedDoc int64
}

// chunkInserter inserts seed chunks and carries the first error, so the seed
// below reads as the corpus it describes rather than as one error check per
// row. `add` returns 0 once an error has been recorded; the caller checks `err`
// once at the end.
type chunkInserter struct {
	ctx     context.Context
	tx      model.RepresentationStore
	reps    map[string]int64
	ordinal int
	err     error
}

func (ci *chunkInserter) add(relPath, kind, status string, deleted bool, spans []model.Span) int64 {
	if ci.err != nil {
		return 0
	}
	ci.ordinal++
	id, err := ci.tx.InsertChunkWithSpans(ci.ctx, model.Chunk{
		RepID: ci.reps[relPath], Ordinal: ci.ordinal, Text: "chunk " + kind, TextHash: "h",
		IndexKind: kind, EmbeddingStatus: status, Deleted: deleted,
	}, spans)
	if err != nil {
		ci.err = err
		return 0
	}
	return id
}

// seedPendingDocuments creates the three parent documents the corpus needs: a
// healthy one, an errored one and a tombstoned one.
func seedPendingDocuments(t *testing.T, st *store.SQLiteStore) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	docs := []struct {
		relPath string
		status  string
		deleted bool
	}{
		{"notes/a.md", "ok", false},
		{"notes/bad.md", "error", false},
		{"notes/gone.md", "ok", true},
	}
	ids := map[string]int64{}
	for _, d := range docs {
		if err := st.UpsertDocument(ctx, model.Document{
			RelPath: d.relPath, DocType: "md", SourceType: "local", Status: d.status,
			Title: "Doc", MTimeUnix: seedMTime, Deleted: d.deleted,
		}); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", d.relPath, err)
		}
		doc, err := st.GetDocumentByPath(ctx, d.relPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(%s): %v", d.relPath, err)
		}
		ids[d.relPath] = doc.DocID
	}
	return ids
}

// seedPendingCorpus builds a corpus whose kinds INTERLEAVE by chunk_id (text,
// code, text, code, ...) plus the rows a batch must exclude: already-embedded
// and errored chunks, a soft-deleted chunk, and chunks whose parent document is
// errored or tombstoned.
func seedPendingCorpus(t *testing.T, st *store.SQLiteStore) pendingSeed {
	t.Helper()
	ctx := context.Background()

	docIDs := seedPendingDocuments(t, st)

	var seed pendingSeed
	err := st.WithTx(ctx, func(tx model.RepresentationStore) error {
		ins := &chunkInserter{ctx: ctx, tx: tx, reps: map[string]int64{}}
		for relPath, docID := range docIDs {
			repID, rerr := tx.UpsertRepresentation(ctx, model.Representation{
				DocID: docID, RepType: "raw_text", RepHash: "h-" + relPath,
			})
			if rerr != nil {
				return rerr
			}
			ins.reps[relPath] = repID
		}
		for i := 0; i < 3; i++ {
			seed.text = append(seed.text, ins.add("notes/a.md", "text", "pending", false,
				[]model.Span{{Kind: "lines", StartLine: 1, EndLine: 2}}))
			seed.code = append(seed.code, ins.add("notes/a.md", "code", "pending", false,
				[]model.Span{{Kind: "lines", StartLine: 3, EndLine: 4}}))
		}
		// Rows a batch must never return. "error", not "failed":
		// normalizeEmbeddingStatus knows only ok/error/pending and maps anything
		// else to pending, so a chunk seeded as "failed" is a PENDING chunk and
		// would not test what it looks like it tests.
		ins.add("notes/a.md", "text", "ok", false, nil)
		ins.add("notes/a.md", "text", "error", false, nil)
		ins.add("notes/a.md", "text", "pending", true, nil)
		seed.erroredDoc = ins.add("notes/bad.md", "text", "pending", false, nil)
		seed.deletedDoc = ins.add("notes/gone.md", "text", "pending", false, nil)

		// A multi-span chunk (the batch must carry its FIRST span) and a
		// span-less one.
		seed.multiSpan = ins.add("notes/a.md", "text", "pending", false, []model.Span{
			{Kind: "lines", StartLine: 10, EndLine: 11},
			{Kind: "lines", StartLine: 20, EndLine: 21},
		})
		seed.text = append(seed.text, seed.multiSpan)
		seed.noSpan = ins.add("notes/a.md", "text", "pending", false, nil)
		seed.text = append(seed.text, seed.noSpan)
		return ins.err
	})
	if err != nil {
		t.Fatalf("seed corpus: %v", err)
	}

	// A chunk with no document row at all: UpsertChunkTask seeds bare chunks
	// against a caller-chosen id, and the NULL guard in the CTE exists to keep
	// them embeddable. The id sorts after every chunk above so the drain order
	// stays ascending.
	const orphanID = 900001
	if err := st.UpsertChunkTask(ctx, model.ChunkTask{
		Label: orphanID, Text: "orphan chunk", IndexKind: "text",
		Metadata: model.ChunkMetadata{ChunkID: orphanID, RelPath: "notes/orphan.md"},
	}); err != nil {
		t.Fatalf("UpsertChunkTask: %v", err)
	}
	seed.orphan = orphanID
	seed.text = append(seed.text, orphanID)
	return seed
}
