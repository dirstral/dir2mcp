package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// addVecLang upserts a vector whose payload carries the representation's recorded
// effective language, mirroring what payloadFromTask stores in production so the
// HNSW pushdown filter (model.Filter.Match over the payload) sees it.
func addVecLang(t *testing.T, idx *index.HNSWIndex, id uint64, vec []float32, relPath, docType, language string) {
	t.Helper()
	payload := model.IndexPayload{ChunkID: id, RelPath: relPath, DocType: docType, Language: language}
	if err := idx.Upsert(context.Background(), vec, payload); err != nil {
		t.Fatalf("Upsert(%d): %v", id, err)
	}
}

// newLangService builds a vector-only retrieval service over idx and registers
// the in-memory chunk metadata (carrying Language) for each id, mirroring the
// production warm/on-index path so matchFilters sees the recorded language.
func newLangService(t *testing.T, idx *index.HNSWIndex, meta map[uint64]model.SearchHit) *retrieval.Service {
	t.Helper()
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	for id, hit := range meta {
		svc.SetChunkMetadata(id, hit)
	}
	return svc
}

func langSearchIDs(t *testing.T, svc *retrieval.Service, q model.SearchQuery) []uint64 {
	t.Helper()
	hits, err := svc.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := make([]uint64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ChunkID)
	}
	return ids
}

// TestSearch_LanguageFilter_RestrictsToRequested pins SPEC §9.5: a languages
// filter restricts hits to representations recorded in any requested language
// (logical OR), excluding other languages.
func TestSearch_LanguageFilter_RestrictsToRequested(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecLang(t, idx, 1, []float32{1, 0}, "en.txt", "text", "en")
	addVecLang(t, idx, 2, []float32{0.96, 0.04}, "pt.txt", "text", "pt-BR")
	addVecLang(t, idx, 3, []float32{0.92, 0.08}, "es.txt", "text", "es")
	svc := newLangService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "en.txt", DocType: "text", Snippet: "english", Language: "en", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		2: {RelPath: "pt.txt", DocType: "text", Snippet: "portuguese", Language: "pt-BR", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		3: {RelPath: "es.txt", DocType: "text", Snippet: "spanish", Language: "es", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})

	got := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, Languages: []string{"pt", "es"}})
	if len(got) != 2 {
		t.Fatalf("languages [pt es] should yield 2 hits, got %d: %v", len(got), got)
	}
	for _, id := range got {
		if id == 1 {
			t.Fatalf("english hit must be excluded by a [pt es] filter, got %v", got)
		}
	}
}

// TestSearch_LanguageFilter_PrimarySubtagCaseInsensitive pins §9.5: matching is
// on the BCP-47 primary subtag, case-insensitively, so a request for "EN"
// matches both "en" and "en-US".
func TestSearch_LanguageFilter_PrimarySubtagCaseInsensitive(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecLang(t, idx, 1, []float32{1, 0}, "a.txt", "text", "en")
	addVecLang(t, idx, 2, []float32{0.96, 0.04}, "b.txt", "text", "en-US")
	svc := newLangService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "a.txt", DocType: "text", Snippet: "x", Language: "en", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		2: {RelPath: "b.txt", DocType: "text", Snippet: "y", Language: "en-US", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})

	got := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, Languages: []string{"EN"}})
	if len(got) != 2 {
		t.Fatalf("request EN should match en and en-US (primary subtag), got %d: %v", len(got), got)
	}
}

// TestSearch_LanguageFilter_UnknownExcludedBySpecificFilterButUnaffectedWithout
// pins §8.8/§9.5: an unknown-language representation never matches a specific
// filter, but is returned unchanged when no filter is set.
func TestSearch_LanguageFilter_UnknownExcludedBySpecificFilterButUnaffectedWithout(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecLang(t, idx, 1, []float32{1, 0}, "en.txt", "text", "en")
	addVecLang(t, idx, 2, []float32{0.96, 0.04}, "unknown.txt", "text", "") // no recorded language
	svc := newLangService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "en.txt", DocType: "text", Snippet: "x", Language: "en", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		2: {RelPath: "unknown.txt", DocType: "text", Snippet: "y", Language: "", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})

	// Specific filter excludes the unknown-language hit.
	filtered := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, Languages: []string{"en"}})
	if len(filtered) != 1 || filtered[0] != 1 {
		t.Fatalf("a specific [en] filter must exclude the unknown-language hit, got %v", filtered)
	}

	// No filter: both hits returned (behaviour unchanged).
	unfiltered := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10})
	if len(unfiltered) != 2 {
		t.Fatalf("absent filter must be a no-op; expected 2 hits, got %d: %v", len(unfiltered), unfiltered)
	}
}

// TestSearch_LanguageFilter_AbsentIsUnchanged confirms an absent/empty filter
// leaves results identical to no filter (additive, off by default, §9.5).
func TestSearch_LanguageFilter_AbsentIsUnchanged(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecLang(t, idx, 1, []float32{1, 0}, "en.txt", "text", "en")
	addVecLang(t, idx, 2, []float32{0.96, 0.04}, "pt.txt", "text", "pt")
	svc := newLangService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "en.txt", DocType: "text", Snippet: "x", Language: "en", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		2: {RelPath: "pt.txt", DocType: "text", Snippet: "y", Language: "pt", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})

	none := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10})
	empty := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, Languages: []string{}})
	if len(none) != 2 || len(empty) != 2 {
		t.Fatalf("absent vs empty languages must both be no-ops; none=%v empty=%v", none, empty)
	}
}

// TestSearch_LanguageFilter_StrictRegionNarrowing pins §9.5 opt-in narrowing:
// with language_match="strict" a pt-BR request keeps pt-BR (and pt-BR-…) hits but
// excludes bare pt and pt-PT, whereas the default primary mode keeps them all.
func TestSearch_LanguageFilter_StrictRegionNarrowing(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecLang(t, idx, 1, []float32{1, 0}, "br.txt", "text", "pt-BR")
	addVecLang(t, idx, 2, []float32{0.97, 0.03}, "pt.txt", "text", "pt-PT")
	addVecLang(t, idx, 3, []float32{0.94, 0.06}, "bare.txt", "text", "pt")
	svc := newLangService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "br.txt", DocType: "text", Snippet: "brazilian", Language: "pt-BR", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		2: {RelPath: "pt.txt", DocType: "text", Snippet: "european", Language: "pt-PT", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		3: {RelPath: "bare.txt", DocType: "text", Snippet: "generic", Language: "pt", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})

	// Strict: only the pt-BR hit survives a pt-BR request.
	strict := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, Languages: []string{"pt-BR"}, LanguageMatch: "strict"})
	if len(strict) != 1 || strict[0] != 1 {
		t.Fatalf("strict [pt-BR] must yield only the pt-BR hit, got %v", strict)
	}

	// Default (primary) with the same tag matches all three by primary subtag.
	primary := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, Languages: []string{"pt-BR"}})
	if len(primary) != 3 {
		t.Fatalf("default primary [pt-BR] must match all pt* hits, got %d: %v", len(primary), primary)
	}
}

// TestSearch_LanguageFilter_StrictBarePrimaryStillBroad pins §9.5: under strict,
// a request that carries only a primary subtag still matches all its region/script
// extensions — narrowing occurs only to the precision the caller supplies.
func TestSearch_LanguageFilter_StrictBarePrimaryStillBroad(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecLang(t, idx, 1, []float32{1, 0}, "br.txt", "text", "pt-BR")
	addVecLang(t, idx, 2, []float32{0.97, 0.03}, "pt.txt", "text", "pt-PT")
	addVecLang(t, idx, 3, []float32{0.94, 0.06}, "es.txt", "text", "es")
	svc := newLangService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "br.txt", DocType: "text", Snippet: "a", Language: "pt-BR", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		2: {RelPath: "pt.txt", DocType: "text", Snippet: "b", Language: "pt-PT", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		3: {RelPath: "es.txt", DocType: "text", Snippet: "c", Language: "es", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})

	got := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, Languages: []string{"pt"}, LanguageMatch: "strict"})
	if len(got) != 2 {
		t.Fatalf("strict bare [pt] must match both pt-BR and pt-PT (not es), got %d: %v", len(got), got)
	}
	for _, id := range got {
		if id == 3 {
			t.Fatalf("es hit must be excluded by a strict [pt] filter, got %v", got)
		}
	}
}

// TestSearch_LanguageFilter_StrictUnknownExcluded confirms strict mode also never
// matches an unknown-language representation (§8.8/§9.5).
func TestSearch_LanguageFilter_StrictUnknownExcluded(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecLang(t, idx, 1, []float32{1, 0}, "br.txt", "text", "pt-BR")
	addVecLang(t, idx, 2, []float32{0.96, 0.04}, "unknown.txt", "text", "")
	svc := newLangService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "br.txt", DocType: "text", Snippet: "a", Language: "pt-BR", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		2: {RelPath: "unknown.txt", DocType: "text", Snippet: "b", Language: "", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})

	got := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, Languages: []string{"pt-BR"}, LanguageMatch: "strict"})
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("strict filter must exclude the unknown-language hit, got %v", got)
	}
}

// TestSearch_LanguageMatch_UnsetIsPrimary confirms leaving LanguageMatch unset is
// identical to explicit "primary" — no regression when the option is not supplied.
func TestSearch_LanguageMatch_UnsetIsPrimary(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecLang(t, idx, 1, []float32{1, 0}, "br.txt", "text", "pt-BR")
	addVecLang(t, idx, 2, []float32{0.96, 0.04}, "pt.txt", "text", "pt-PT")
	svc := newLangService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "br.txt", DocType: "text", Snippet: "a", Language: "pt-BR", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
		2: {RelPath: "pt.txt", DocType: "text", Snippet: "b", Language: "pt-PT", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})

	unset := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, Languages: []string{"pt"}})
	primary := langSearchIDs(t, svc, model.SearchQuery{Query: "x", K: 10, Languages: []string{"pt"}, LanguageMatch: "primary"})
	if len(unset) != 2 || len(primary) != 2 {
		t.Fatalf("unset and explicit primary must both match both pt* hits; unset=%v primary=%v", unset, primary)
	}
}

// TestSearch_LanguageFilter_NoMatchIsEmptyNotError pins §9.5: a syntactically
// valid tag that matches nothing in the corpus returns an empty hit list, not an
// error.
func TestSearch_LanguageFilter_NoMatchIsEmptyNotError(t *testing.T) {
	idx := index.NewHNSWIndex("")
	addVecLang(t, idx, 1, []float32{1, 0}, "en.txt", "text", "en")
	svc := newLangService(t, idx, map[uint64]model.SearchHit{
		1: {RelPath: "en.txt", DocType: "text", Snippet: "x", Language: "en", Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 1}},
	})

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "x", K: 10, Languages: []string{"ja"}})
	if err != nil {
		t.Fatalf("a no-match language filter must not error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("a no-match language filter must return an empty hit list, got %d", len(hits))
	}
}
