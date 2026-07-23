package tests

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// Coverage expansion for hierarchical retrieval (SPEC §5.2 / §9.7, #329).
//
// These tests exercise the parent→child linkage end to end against a real
// sqlite store: the three range kinds, the same-document invariant, and the
// fine-only guarantee.

func newSummaryStore(t *testing.T) (*store.SQLiteStore, context.Context) {
	t.Helper()
	st := store.NewSQLiteStore(filepath.Join(t.TempDir(), "summary.sqlite"))
	ctx := context.Background()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, ctx
}

func mustDoc(t *testing.T, st *store.SQLiteStore, ctx context.Context, relPath, docType string) model.Document {
	t.Helper()
	if err := st.UpsertDocument(ctx, model.Document{RelPath: relPath, DocType: docType}); err != nil {
		t.Fatalf("upsert document %s: %v", relPath, err)
	}
	doc, err := st.GetDocumentByPath(ctx, relPath)
	if err != nil {
		t.Fatalf("get document %s: %v", relPath, err)
	}
	return doc
}

func mustRep(t *testing.T, st *store.SQLiteStore, ctx context.Context, docID int64, repType, metaJSON string) int64 {
	t.Helper()
	repID, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID:    docID,
		RepType:  repType,
		RepHash:  repType + "-hash",
		MetaJSON: metaJSON,
	})
	if err != nil {
		t.Fatalf("upsert representation %s: %v", repType, err)
	}
	return repID
}

// mustChunk inserts one chunk. The store derives the chunk's rel_path/doc_type/
// rep_type from its representation, so callers only supply the rep and body.
func mustChunk(t *testing.T, st *store.SQLiteStore, ctx context.Context, repID int64, ordinal int, text string, spans []model.Span) uint64 {
	t.Helper()
	id, err := st.InsertChunkWithSpans(ctx, model.Chunk{
		RepID:           repID,
		Ordinal:         ordinal,
		Text:            text,
		IndexKind:       "text",
		EmbeddingStatus: "ok",
	}, spans)
	if err != nil {
		t.Fatalf("insert chunk (rep %d ordinal %d): %v", repID, ordinal, err)
	}
	return uint64(id)
}

func summaryMetaJSON(t *testing.T, sourceRepID int64, r model.SummaryCoverageRange) string {
	t.Helper()
	raw, err := json.Marshal(model.SummaryMeta{
		SummaryLevel: model.SummaryLevelDocument,
		Provider:     "test",
		Model:        "test-model",
		Coverage: model.SummaryCoverage{
			SourceRepID: sourceRepID,
			Range:       r,
		},
	})
	if err != nil {
		t.Fatalf("marshal summary meta: %v", err)
	}
	return string(raw)
}

func expandedIDs(t *testing.T, st *store.SQLiteStore, ctx context.Context, chunkID uint64) []uint64 {
	t.Helper()
	covered, err := st.ExpandSummaryChunk(ctx, chunkID)
	if err != nil {
		t.Fatalf("ExpandSummaryChunk(%d): %v", chunkID, err)
	}
	ids := make([]uint64, 0, len(covered))
	for _, meta := range covered {
		ids = append(ids, meta.ChunkID)
	}
	return ids
}

func assertIDs(t *testing.T, got, want []uint64, what string) {
	t.Helper()
	gotSorted := append([]uint64(nil), got...)
	wantSorted := append([]uint64(nil), want...)
	sort.Slice(gotSorted, func(i, j int) bool { return gotSorted[i] < gotSorted[j] })
	sort.Slice(wantSorted, func(i, j int) bool { return wantSorted[i] < wantSorted[j] })
	if len(gotSorted) != len(wantSorted) {
		t.Fatalf("%s: got %v, want %v", what, gotSorted, wantSorted)
	}
	for i := range gotSorted {
		if gotSorted[i] != wantSorted[i] {
			t.Fatalf("%s: got %v, want %v", what, gotSorted, wantSorted)
		}
	}
}

// TestExpandSummaryChunkDocumentRange covers the `document` range: a summary
// expands to EVERY active chunk of its source representation and never to
// itself, another representation, or a deleted chunk.
func TestExpandSummaryChunkDocumentRange(t *testing.T) {
	st, ctx := newSummaryStore(t)
	doc := mustDoc(t, st, ctx, "notes.md", "md")

	srcRep := mustRep(t, st, ctx, doc.DocID, "raw_text", "")
	c0 := mustChunk(t, st, ctx, srcRep, 0, "alpha", nil)
	c1 := mustChunk(t, st, ctx, srcRep, 1, "beta", nil)
	c2 := mustChunk(t, st, ctx, srcRep, 2, "gamma", nil)

	// A second representation of the same document must NOT be reached: a summary
	// covers exactly one representation (SPEC §5.2).
	otherRep := mustRep(t, st, ctx, doc.DocID, "annotation_text", "")
	mustChunk(t, st, ctx, otherRep, 0, "unrelated", nil)

	sumRep := mustRep(t, st, ctx, doc.DocID, model.SummaryRepType,
		summaryMetaJSON(t, srcRep, model.SummaryCoverageRange{Kind: model.SummaryRangeDocument}))
	sumChunk := mustChunk(t, st, ctx, sumRep, 0, "a summary of the notes", nil)

	assertIDs(t, expandedIDs(t, st, ctx, sumChunk), []uint64{c0, c1, c2}, "document-range expansion")
}

// TestExpandSummaryChunkOrdinalRangeIsInclusive covers the `ordinals` range: a
// chunk is selected iff start <= ordinal <= end (SPEC §5.2), so both endpoints
// are inside and the neighbours just outside are not.
func TestExpandSummaryChunkOrdinalRangeIsInclusive(t *testing.T) {
	st, ctx := newSummaryStore(t)
	doc := mustDoc(t, st, ctx, "book.md", "md")
	srcRep := mustRep(t, st, ctx, doc.DocID, "raw_text", "")

	ids := make([]uint64, 5)
	for i := range ids {
		ids[i] = mustChunk(t, st, ctx, srcRep, i, "chunk", nil)
	}

	sumRep := mustRep(t, st, ctx, doc.DocID, model.SummaryRepType,
		summaryMetaJSON(t, srcRep, model.SummaryCoverageRange{
			Kind: model.SummaryRangeOrdinals, Start: 1, End: 3,
		}))
	sumChunk := mustChunk(t, st, ctx, sumRep, 0, "section summary", nil)

	assertIDs(t, expandedIDs(t, st, ctx, sumChunk), []uint64{ids[1], ids[2], ids[3]}, "inclusive ordinal range")
}

// TestExpandSummaryChunkTimeRangeUsesOverlapNotContainment is the discriminating
// test for the §5.2 time-range rule: selection is by INTERVAL OVERLAP
// (seg_start_ms <= end_ms AND seg_end_ms >= start_ms), never containment.
//
// The fixture is built so the two rules disagree in BOTH directions:
//
//   - "straddleStart" / "straddleEnd" overlap the window but are NOT contained —
//     containment would drop them, overlap keeps them. Coarse-to-fine must not
//     drop evidence the summary was built from.
//   - "before" / "after" are outside the window entirely — neither rule selects
//     them, so their exclusion proves the predicate is not simply "everything".
//   - "inside" is contained AND overlapping — both rules keep it.
func TestExpandSummaryChunkTimeRangeUsesOverlapNotContainment(t *testing.T) {
	st, ctx := newSummaryStore(t)
	doc := mustDoc(t, st, ctx, "talk.mp3", "audio")
	srcRep := mustRep(t, st, ctx, doc.DocID, "transcript", "")

	timeSpan := func(startMS, endMS int) []model.Span {
		return []model.Span{{Kind: "time", StartMS: startMS, EndMS: endMS}}
	}
	// Window is [10000, 20000].
	before := mustChunk(t, st, ctx, srcRep, 0, "before", timeSpan(0, 5000))
	straddleStart := mustChunk(t, st, ctx, srcRep, 1, "straddles start", timeSpan(8000, 12000))
	inside := mustChunk(t, st, ctx, srcRep, 2, "inside", timeSpan(12000, 15000))
	straddleEnd := mustChunk(t, st, ctx, srcRep, 3, "straddles end", timeSpan(18000, 25000))
	after := mustChunk(t, st, ctx, srcRep, 4, "after", timeSpan(30000, 35000))

	sumRep := mustRep(t, st, ctx, doc.DocID, model.SummaryRepType,
		summaryMetaJSON(t, srcRep, model.SummaryCoverageRange{
			Kind: model.SummaryRangeTime, StartMS: 10000, EndMS: 20000,
		}))
	sumChunk := mustChunk(t, st, ctx, sumRep, 0, "event summary", nil)

	got := expandedIDs(t, st, ctx, sumChunk)
	// Overlap: the straddlers ARE selected (containment would fail here).
	assertIDs(t, got, []uint64{straddleStart, inside, straddleEnd}, "interval-overlap time range")

	for _, excluded := range []uint64{before, after} {
		for _, id := range got {
			if id == excluded {
				t.Fatalf("chunk %d lies outside the window but was selected", excluded)
			}
		}
	}

	// The pure-predicate helper agrees with the SQL, in both directions.
	if !model.SummaryTimeRangeSelects(10000, 20000, 8000, 12000) {
		t.Fatal("overlap helper rejected a straddling segment (containment semantics leaked in)")
	}
	if model.SummaryTimeRangeSelects(10000, 20000, 30000, 35000) {
		t.Fatal("overlap helper selected a segment entirely outside the window")
	}
}

// TestExpandSummaryChunkEnforcesSameDocumentInvariant asserts the §5.2
// same-document invariant is enforced by the STORE, not merely by the writer: a
// coverage that names a representation of ANOTHER document expands to nothing,
// so a corrupt or hand-edited meta_json can never leak another document's
// content into this document's citations.
func TestExpandSummaryChunkEnforcesSameDocumentInvariant(t *testing.T) {
	st, ctx := newSummaryStore(t)
	own := mustDoc(t, st, ctx, "own.md", "md")
	foreign := mustDoc(t, st, ctx, "foreign.md", "md")

	ownRep := mustRep(t, st, ctx, own.DocID, "raw_text", "")
	ownChunk := mustChunk(t, st, ctx, ownRep, 0, "own content", nil)

	foreignRep := mustRep(t, st, ctx, foreign.DocID, "raw_text", "")
	mustChunk(t, st, ctx, foreignRep, 0, "foreign content", nil)

	// A summary on `own.md` whose coverage points at `foreign.md`'s representation.
	crossRep := mustRep(t, st, ctx, own.DocID, model.SummaryRepType,
		summaryMetaJSON(t, foreignRep, model.SummaryCoverageRange{Kind: model.SummaryRangeDocument}))
	crossChunk := mustChunk(t, st, ctx, crossRep, 0, "cross-document summary", nil)

	if got := expandedIDs(t, st, ctx, crossChunk); len(got) != 0 {
		t.Fatalf("cross-document coverage expanded to %v; the same-document invariant must yield no chunks", got)
	}

	// Sanity: the same summary pointed at its OWN document's representation does
	// expand, so the empty result above is the invariant and not a broken query.
	sameRep := mustRep(t, st, ctx, own.DocID, model.SummaryRepType+"-raw_text",
		summaryMetaJSON(t, ownRep, model.SummaryCoverageRange{Kind: model.SummaryRangeDocument}))
	sameChunk := mustChunk(t, st, ctx, sameRep, 0, "own summary", nil)
	assertIDs(t, expandedIDs(t, st, ctx, sameChunk), []uint64{ownChunk}, "same-document expansion")
}

// TestExpandSummaryChunkNonSummaryAndInvalidCoverage asserts the fail-open
// contract (§9.7): a fine chunk, an unknown chunk, an unparseable meta_json, and
// a structurally invalid coverage all expand to nothing WITHOUT an error, so an
// unusable summary degrades to flat retrieval instead of failing the query.
func TestExpandSummaryChunkNonSummaryAndInvalidCoverage(t *testing.T) {
	st, ctx := newSummaryStore(t)
	doc := mustDoc(t, st, ctx, "notes.md", "md")
	srcRep := mustRep(t, st, ctx, doc.DocID, "raw_text", "")
	fineChunk := mustChunk(t, st, ctx, srcRep, 0, "alpha", nil)

	for _, tc := range []struct {
		name    string
		chunkID uint64
	}{
		{"fine chunk is not a summary", fineChunk},
		{"unknown chunk id", 999999},
		{"zero chunk id", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			covered, err := st.ExpandSummaryChunk(ctx, tc.chunkID)
			if err != nil {
				t.Fatalf("ExpandSummaryChunk: unexpected error: %v", err)
			}
			if len(covered) != 0 {
				t.Fatalf("expected no expansion, got %d chunks", len(covered))
			}
		})
	}

	t.Run("unparseable meta_json", func(t *testing.T) {
		rep := mustRep(t, st, ctx, doc.DocID, model.SummaryRepType, "{not json")
		id := mustChunk(t, st, ctx, rep, 0, "summary", nil)
		covered, err := st.ExpandSummaryChunk(ctx, id)
		if err != nil {
			t.Fatalf("ExpandSummaryChunk: unexpected error: %v", err)
		}
		if len(covered) != 0 {
			t.Fatalf("unparseable meta expanded to %d chunks", len(covered))
		}
	})

	t.Run("inverted ordinal range", func(t *testing.T) {
		rep := mustRep(t, st, ctx, doc.DocID, model.SummaryRepType+"-inverted",
			summaryMetaJSON(t, srcRep, model.SummaryCoverageRange{
				Kind: model.SummaryRangeOrdinals, Start: 5, End: 1,
			}))
		id := mustChunk(t, st, ctx, rep, 0, "summary", nil)
		covered, err := st.ExpandSummaryChunk(ctx, id)
		if err != nil {
			t.Fatalf("ExpandSummaryChunk: unexpected error: %v", err)
		}
		if len(covered) != 0 {
			t.Fatalf("inverted range expanded to %d chunks", len(covered))
		}
	})
}

// TestSummarySourceRepsExcludesSummaries asserts a summary is never itself a
// summarization source (SPEC §16.2), and that only representations carrying
// active chunks are reported.
func TestSummarySourceRepsExcludesSummaries(t *testing.T) {
	st, ctx := newSummaryStore(t)
	doc := mustDoc(t, st, ctx, "notes.md", "md")

	srcRep := mustRep(t, st, ctx, doc.DocID, "raw_text", "")
	mustChunk(t, st, ctx, srcRep, 0, "alpha", nil)

	// An empty representation (no chunks) is not a summarization source.
	mustRep(t, st, ctx, doc.DocID, "annotation_text", "")

	sumRep := mustRep(t, st, ctx, doc.DocID, model.SummaryRepType,
		summaryMetaJSON(t, srcRep, model.SummaryCoverageRange{Kind: model.SummaryRangeDocument}))
	mustChunk(t, st, ctx, sumRep, 0, "a summary", nil)
	suffixed := mustRep(t, st, ctx, doc.DocID, model.SummaryRepType+"-raw_text",
		summaryMetaJSON(t, srcRep, model.SummaryCoverageRange{Kind: model.SummaryRangeDocument}))
	mustChunk(t, st, ctx, suffixed, 0, "another summary", nil)

	reps, err := st.SummarySourceReps(ctx, "notes.md")
	if err != nil {
		t.Fatalf("SummarySourceReps: %v", err)
	}
	if len(reps) != 1 {
		t.Fatalf("expected exactly one summarizable representation, got %d (%+v)", len(reps), reps)
	}
	if reps[0].RepID != srcRep || reps[0].RepType != "raw_text" {
		t.Fatalf("unexpected source representation: %+v", reps[0])
	}
	if reps[0].DocID != doc.DocID {
		t.Fatalf("source rep doc_id = %d, want %d", reps[0].DocID, doc.DocID)
	}

	texts, err := st.SummarySourceText(ctx, srcRep)
	if err != nil {
		t.Fatalf("SummarySourceText: %v", err)
	}
	if len(texts) != 1 || texts[0] != "alpha" {
		t.Fatalf("SummarySourceText = %v, want [alpha]", texts)
	}
}
