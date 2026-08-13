package tests

import (
	"context"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// The recognition entity/event filter on the LEXICAL and FUSED paths (issue
// #856, dirstral-spec design 0004 §7).
//
// tests/retrieval/annotation_entity_filter_test.go pins the semantics on the
// vector path. Its fixture passes a nil store, so the store satisfies no
// model.LexicalSearcher and hybrid retrieval degrades to vector-only. Every
// assertion there therefore never reached the BM25 half of the pipeline, and a
// filter that a fused candidate escaped still looked correct.
//
// That escape is what #856 reports: on a live corpus of 394 recognition chunks,
// `events=["no_such_event_xyz"]` returned 19 hits. The vector candidates were
// filtered to none, then unfiltered BM25 candidates were fused back in and
// became the whole result. The filter said "nothing matches" and the answer said
// "here are 19 things".
//
// The fixture below is hybrid-enabled: a store that serves BM25, plus a vector
// index over the same chunks. Each test states which path carries the candidate
// it asserts on.

const (
	sfgID       = "team:san-francisco-giants"
	hrBatterID  = "player:heliot-ramos"
	hrPitcherID = "player:logan-webb"
	homeRun     = "home_run"
)

// lexicalHitStore is a minimal model.Store that also satisfies
// model.LexicalSearcher. SearchBM25 returns its seeded hits verbatim (up to k),
// mirroring the reference store, whose FTS query joins the spans table and so
// returns hits that already carry the annotation attribution.
type lexicalHitStore struct {
	hits    []model.SearchHit
	queries []string
}

func (s *lexicalHitStore) SearchBM25(_ context.Context, query string, k int, _ string) ([]model.SearchHit, error) {
	s.queries = append(s.queries, query)
	if k > 0 && len(s.hits) > k {
		return append([]model.SearchHit(nil), s.hits[:k]...), nil
	}
	return append([]model.SearchHit(nil), s.hits...), nil
}

func (s *lexicalHitStore) Init(context.Context) error                           { return nil }
func (s *lexicalHitStore) UpsertDocument(context.Context, model.Document) error { return nil }
func (s *lexicalHitStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotFound
}
func (s *lexicalHitStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (s *lexicalHitStore) Close() error { return nil }

// annotationHit is one recognition candidate: a "time" span carrying the entity
// ids and the backend-declared event, exactly as ingestion persists it and as
// both the store's BM25 query and the in-memory chunk metadata return it.
func annotationHit(id uint64, event string, entities []string) model.SearchHit {
	return model.SearchHit{
		ChunkID: id,
		RelPath: "game.mp4",
		DocType: "video",
		RepType: "recognition",
		Snippet: "a pitch to the plate",
		Span: model.Span{
			Kind: "time", StartMS: int(id) * 1000, EndMS: int(id)*1000 + 800,
			Entities: entities, Event: event,
		},
	}
}

// The pilot corpus shape: five home runs, other events around them, one chunk
// with no attribution at all, and one home run that ONLY the lexical retriever
// returns (its vector is absent from the index). The last one is the guard
// against "fix the leak by dropping every lexical candidate": that would pass
// every zero-hit assertion below and silently halve recall.
var (
	homeRunIDs      = []uint64{1, 2, 3, 4, 5}
	lexicalOnlyID   = uint64(9)
	homeRunTotalIDs = []uint64{1, 2, 3, 4, 5, 9}
)

// hybridAnnotationService builds the fixture. Every chunk is registered as
// in-memory metadata (what the vector path materialises) AND seeded into the
// lexical store (what BM25 returns), so a candidate can be reached on either
// path.
func hybridAnnotationService(t *testing.T) (*retrieval.Service, *lexicalHitStore) {
	t.Helper()
	idx := index.NewHNSWIndex("")

	hits := make([]model.SearchHit, 0, 8)
	for _, id := range homeRunIDs {
		hits = append(hits, annotationHit(id, homeRun, []string{hrBatterID, sfgID}))
	}
	// Two other events on the same corpus: a non-empty event filter must not
	// admit them.
	hits = append(hits,
		annotationHit(6, "pitch", []string{hrPitcherID, sfgID}),
		annotationHit(7, "at_bat", []string{hrBatterID, sfgID}),
	)
	// A plain text chunk: no attribution, so it never matches a non-empty filter.
	hits = append(hits, model.SearchHit{
		ChunkID: 8, RelPath: "notes.md", DocType: "md", RepType: "raw_text",
		Snippet: "a note about the game",
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 4},
	})

	for _, hit := range hits {
		addAnnotationVector(t, idx, hit)
	}

	// The lexical retriever additionally returns a home run the vector index does
	// not hold.
	lexicalOnly := annotationHit(lexicalOnlyID, homeRun, []string{hrBatterID, sfgID})
	st := &lexicalHitStore{hits: append(append([]model.SearchHit(nil), hits...), lexicalOnly)}

	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	for _, hit := range hits {
		svc.SetChunkMetadata(hit.ChunkID, hit)
	}
	svc.SetChunkMetadata(lexicalOnly.ChunkID, lexicalOnly)
	return svc, st
}

// addAnnotationVector upserts the hit's vector with the payload ingestion would
// have stored, so the index-side filter sees what the Go-side re-check sees.
func addAnnotationVector(t *testing.T, idx *index.HNSWIndex, hit model.SearchHit) {
	t.Helper()
	payload := model.IndexPayload{
		ChunkID: hit.ChunkID, RelPath: hit.RelPath, DocType: hit.DocType,
		RepType: hit.RepType, StartMS: hit.Span.StartMS, EndMS: hit.Span.EndMS,
		Span: hit.Span,
	}
	// Near-identical vectors: every chunk is a candidate, so a hit that is absent
	// from a result was removed by the filter and not by ranking.
	vec := []float32{1, float32(hit.ChunkID) / 1000}
	if err := idx.Upsert(context.Background(), vec, payload); err != nil {
		t.Fatalf("Upsert(%d): %v", hit.ChunkID, err)
	}
}

func hybridSearchIDs(t *testing.T, svc *retrieval.Service, q model.SearchQuery) []uint64 {
	t.Helper()
	if q.Query == "" {
		q.Query = "pitch"
	}
	if q.K == 0 {
		q.K = 20
	}
	hits, err := svc.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	out := make([]uint64, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.ChunkID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sameIDs(got, want []uint64) bool {
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

// TestAnImpossibleFilterValueReturnsZeroHits is the assertion #856 asks for, and
// the one the previous suite lacked: a value that CANNOT match must return
// nothing. It fails before the fix because the fused pool carries unfiltered
// lexical candidates.
func TestAnImpossibleFilterValueReturnsZeroHits(t *testing.T) {
	svc, st := hybridAnnotationService(t)
	for _, q := range []model.SearchQuery{
		{Events: []string{"no_such_event_xyz"}},
		{Entities: []string{"player:nobody-at-all"}},
		{Entities: []string{sfgID}, Events: []string{"no_such_event_xyz"}},
	} {
		if got := hybridSearchIDs(t, svc, q); len(got) != 0 {
			t.Fatalf("%+v returned %v; a value that matches nothing must return no hits", q, got)
		}
	}
	if len(st.queries) == 0 {
		t.Fatal("the lexical retriever was never called; this fixture must exercise the hybrid path")
	}
}

// TestTheEventFilterSelectsEveryAnnotationWhateverTheQueryText is the pilot's
// aggregate question: five home runs are in the corpus, the query text says
// "pitch", and the filter must still return exactly the home runs (including the
// one only BM25 reaches).
func TestTheEventFilterSelectsEveryAnnotationWhateverTheQueryText(t *testing.T) {
	svc, _ := hybridAnnotationService(t)
	for _, queryText := range []string{"pitch", "home run", "who scored"} {
		got := hybridSearchIDs(t, svc, model.SearchQuery{Query: queryText, Events: []string{homeRun}})
		if !sameIDs(got, homeRunTotalIDs) {
			t.Fatalf("query %q with events=[home_run] = %v, want %v", queryText, got, homeRunTotalIDs)
		}
	}
}

// TestALexicalOnlyCandidateSurvivesAMatchingFilter is the recall guard: the
// filter must REMOVE non-matching lexical candidates, never the matching ones.
func TestALexicalOnlyCandidateSurvivesAMatchingFilter(t *testing.T) {
	svc, _ := hybridAnnotationService(t)
	got := hybridSearchIDs(t, svc, model.SearchQuery{Entities: []string{hrBatterID}, Events: []string{homeRun}})
	if !sameIDs(got, homeRunTotalIDs) {
		t.Fatalf("entities=[batter] events=[home_run] = %v, want %v including the lexical-only chunk %d",
			got, homeRunTotalIDs, lexicalOnlyID)
	}
}

// TestTheFilterAppliesOnTheVectorPathToo pins the same behaviour with hybrid
// off, so the answer never depends on which retriever ran. Chunk 9 has no
// vector, so it is correctly absent here.
func TestTheFilterAppliesOnTheVectorPathToo(t *testing.T) {
	svc, _ := hybridAnnotationService(t)
	svc.SetHybridEnabled(false)
	if got := hybridSearchIDs(t, svc, model.SearchQuery{Events: []string{"no_such_event_xyz"}}); len(got) != 0 {
		t.Fatalf("vector-only search with an impossible event returned %v", got)
	}
	got := hybridSearchIDs(t, svc, model.SearchQuery{Events: []string{homeRun}})
	if !sameIDs(got, homeRunIDs) {
		t.Fatalf("vector-only search with events=[home_run] = %v, want %v", got, homeRunIDs)
	}
}

// TestAnAbsentFilterStillFusesEveryLexicalCandidate is the compatibility guard:
// with no filter the fused pool is unchanged, so the fix removes candidates only
// when a filter asks for it.
func TestAnAbsentFilterStillFusesEveryLexicalCandidate(t *testing.T) {
	svc, _ := hybridAnnotationService(t)
	got := hybridSearchIDs(t, svc, model.SearchQuery{})
	want := []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9}
	if !sameIDs(got, want) {
		t.Fatalf("unfiltered hybrid search = %v, want every chunk %v", got, want)
	}
}

// TestAskAppliesTheFilterAndCitesOnlyMatches covers the second tool that
// advertises the parameters. `ask` is the surface the pilot uses, and a citation
// to a filtered-out chunk is a confident wrong answer.
func TestAskAppliesTheFilterAndCitesOnlyMatches(t *testing.T) {
	svc, _ := hybridAnnotationService(t)

	res, err := svc.Ask(context.Background(), "which events happened?", model.SearchQuery{
		Events: []string{"no_such_event_xyz"}, K: 20,
	})
	if err != nil {
		t.Fatalf("Ask (impossible event): %v", err)
	}
	if len(res.Hits) != 0 || len(res.Citations) != 0 {
		t.Fatalf("ask with an impossible event returned %d hits / %d citations, want none",
			len(res.Hits), len(res.Citations))
	}

	res, err = svc.Ask(context.Background(), "who hit home runs?", model.SearchQuery{
		Events: []string{homeRun}, K: 20,
	})
	if err != nil {
		t.Fatalf("Ask (home_run): %v", err)
	}
	for _, cited := range res.Citations {
		if cited.Span.Event != homeRun {
			t.Fatalf("ask cited chunk %d with event %q; the filter admits home_run only",
				cited.ChunkID, cited.Span.Event)
		}
	}
	if len(res.Citations) != len(homeRunTotalIDs) {
		t.Fatalf("ask cited %d chunks, want %d (every home run, including the lexical-only one)",
			len(res.Citations), len(homeRunTotalIDs))
	}
}
