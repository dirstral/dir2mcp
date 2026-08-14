package tests

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// The relevance floor prunes a hit that is much worse than the best one, and
// only that (issue #858, SPEC §9.4.3 "pruning floor").
//
// tests/retrieval/min_score_floor_test.go pins the floor on a WIDE score spread
// (cosine 1.00 / 0.50 / 0.00), where every arithmetic passes. This file pins the
// case that a wide spread hides: candidates whose scores are near-identical. The
// floor used to compare a MIN-MAX normalized score, and min-max maps the lowest
// hit of the set to exactly 0.0 by construction, so `0.0 < floor` held for every
// positive floor and the worst hit was deleted on every search. With the shipped
// default of 0.05 that ran in every deployment, and it cost the most where the
// result set was smallest: 50% of a 2-hit set, 33% of a 3-hit set.
//
// Measured on the pilot corpus: 2 spans carried `event: home_run`, both matched
// the filter and both sat inside k, and min_score 0.05 returned one of them
// while min_score 0 returned both. TestSearch_MinScoreFloor_858_FilteredHybrid
// below is that repro.

// floorDefault is the shipped `retrieval.min_score` default
// (config.defaultRetrievalMinScore). Every test here uses it, because the defect
// is a default-configuration defect.
const floorDefault = 0.05

// vecForCosine returns a unit vector whose cosine against the query vector
// {1, 0} is exactly c (for c in [0,1]), so a test states the score it wants
// rather than a pair of hand-computed coordinates.
func vecForCosine(c float64) []float32 {
	return []float32{float32(c), float32(math.Sqrt(1 - c*c))}
}

// floorService builds a vector-only service (nil store, so hybrid fusion cannot
// engage and each hit's Score is the raw cosine) holding one chunk per cosine,
// with chunk IDs 1..n in the given order.
func floorService(t *testing.T, floor float64, cosines ...float64) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	for i, c := range cosines {
		id := uint64(i + 1)
		addVec(t, idx, id, vecForCosine(c))
		svc.SetChunkMetadata(id, model.SearchHit{ChunkID: id, RelPath: "doc.md", Snippet: "text"})
	}
	svc.SetMinScore(floor)
	return svc
}

// TestSearch_MinScoreFloor_858_NearIdenticalScoresAllSurvive is the assertion
// that would have caught the defect: four candidates within 3% of each other are
// all strong relative to the best one, so the floor must keep every one of them.
// Under min-max the weakest mapped to 0.0 and was deleted on every query.
func TestSearch_MinScoreFloor_858_NearIdenticalScoresAllSurvive(t *testing.T) {
	svc := floorService(t, floorDefault, 0.98, 0.97, 0.96, 0.95)
	got := searchFloorChunkIDs(t, svc)
	if len(got) != 4 {
		t.Fatalf("near-identical candidates must all survive the floor; got %v", got)
	}
}

// TestSearch_MinScoreFloor_858_TwoStrongCandidatesBothSurvive pins the smallest
// and worst-hit case: with two strong candidates and a positive floor, both are
// returned. The old arithmetic returned exactly one, losing half the result set.
func TestSearch_MinScoreFloor_858_TwoStrongCandidatesBothSurvive(t *testing.T) {
	svc := floorService(t, floorDefault, 0.90, 0.88)
	got := searchFloorChunkIDs(t, svc)
	if len(got) != 2 {
		t.Fatalf("two strong candidates must both survive the floor; got %v", got)
	}
}

// TestSearch_MinScoreFloor_858_StillDropsWeakHit is the other half of the
// contract: the floor must keep pruning. A hit scoring 2% of the best is weak in
// its own right, not merely last, so it drops at the shipped default.
func TestSearch_MinScoreFloor_858_StillDropsWeakHit(t *testing.T) {
	svc := floorService(t, floorDefault, 0.99, 0.98, 0.02)
	got := searchFloorChunkIDs(t, svc)
	if len(got) != 2 {
		t.Fatalf("a hit below 5%% of the best must still drop; got %v", got)
	}
	for _, id := range got {
		if id == 3 {
			t.Fatalf("the weak hit (chunk 3) must not survive; got %v", got)
		}
	}
}

// TestSearch_MinScoreFloor_858_AntiCorrelatedHitDrops pins the sign rule on the
// cosine scale: a negative cosine is anti-correlated with the query, so its
// ratio to a positive best is negative and it drops. The two near-identical
// positive hits stay.
func TestSearch_MinScoreFloor_858_AntiCorrelatedHitDrops(t *testing.T) {
	idx := index.NewHNSWIndex("")
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	for id, vec := range map[uint64][]float32{
		1: {1, 0},
		2: vecForCosine(0.99),
		3: {-1, 0}, // cosine -1
	} {
		addVec(t, idx, id, vec)
		svc.SetChunkMetadata(id, model.SearchHit{ChunkID: id, RelPath: "doc.md", Snippet: "text"})
	}
	svc.SetMinScore(floorDefault)
	got := searchFloorChunkIDs(t, svc)
	if len(got) != 2 {
		t.Fatalf("anti-correlated hit must drop and the near-equal pair must stay; got %v", got)
	}
	for _, id := range got {
		if id == 3 {
			t.Fatalf("anti-correlated hit (chunk 3) must not survive; got %v", got)
		}
	}
}

// TestSearch_MinScoreFloor_858_BothIndexNearIdentical covers the index=both
// path, where the same defect had a second source. searchBothIndices rescales
// each axis before merging, and that rescaling was also min-max, so it
// manufactured an exact 0.0 for each axis's weakest hit and the floor deleted it
// downstream. Chunks 1 and 2 are near-identical in the text axis; all three
// candidates must survive.
func TestSearch_MinScoreFloor_858_BothIndexNearIdentical(t *testing.T) {
	textIdx := index.NewHNSWIndex("")
	codeIdx := index.NewHNSWIndex("")
	addVec(t, textIdx, 1, vecForCosine(0.98))
	addVec(t, textIdx, 2, vecForCosine(0.97))
	addVec(t, codeIdx, 3, vecForCosine(0.60))

	svc := retrieval.NewService(nil, textIdx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed":   {1, 0},
		"codestral-embed": {1, 0},
	}}, nil)
	svc.SetCodeIndex(codeIdx)
	svc.SetChunkMetadata(1, model.SearchHit{ChunkID: 1, RelPath: "docs/1.md", DocType: "md"})
	svc.SetChunkMetadata(2, model.SearchHit{ChunkID: 2, RelPath: "docs/2.md", DocType: "md"})
	svc.SetChunkMetadata(3, model.SearchHit{ChunkID: 3, RelPath: "src/3.go", DocType: "code"})
	svc.SetMinScore(floorDefault)

	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 10, Index: "both"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("index=both must not drop a near-identical candidate; got %d hits", len(hits))
	}
}

// scoredReranker returns the caller's own relevance scores for the documents it
// is given, in order, so a test can put the result set on a rerank scale of its
// choosing. A provider scale is arbitrary: these values stand for a
// cross-encoder that rates three passages as almost equally relevant.
type scoredReranker struct {
	scores []float64
}

func (r *scoredReranker) Rerank(_ context.Context, _ string, _ string, docs []string, _ int) ([]model.Reranked, error) {
	out := make([]model.Reranked, 0, len(docs))
	for i := range docs {
		score := 0.0
		if i < len(r.scores) {
			score = r.scores[i]
		}
		out = append(out, model.Reranked{Index: i, RelevanceScore: score})
	}
	return out, nil
}

// TestSearch_MinScoreFloor_858_RerankScaleNearIdentical pins the rerank mode: a
// provider that scores three passages 0.91 / 0.90 / 0.89 has said they are
// equally good, so the floor must return all three. The reranker OVERWRITES
// Score (§9.1.1), so this is the provider's scale and not the cosine one.
func TestSearch_MinScoreFloor_858_RerankScaleNearIdentical(t *testing.T) {
	svc := rerankTestService(t)
	svc.SetReranker(&scoredReranker{scores: []float64{0.91, 0.90, 0.89}}, "m", 50)
	svc.SetRerankEnabled(true)
	svc.SetMinScore(floorDefault)

	if got := search(t, svc, 10); len(got) != 3 {
		t.Fatalf("near-identical rerank scores must all survive the floor; got %d hits", len(got))
	}
}

// TestSearch_MinScoreFloor_858_RerankScaleIsScaleFree pins the #411 property on
// that same rerank path: the identical RATIOS on a hundred-fold larger provider
// scale give the identical outcome, so one configured floor keeps one meaning
// whatever scale the provider reports on.
func TestSearch_MinScoreFloor_858_RerankScaleIsScaleFree(t *testing.T) {
	svc := rerankTestService(t)
	svc.SetReranker(&scoredReranker{scores: []float64{91, 90, 89}}, "m", 50)
	svc.SetRerankEnabled(true)
	svc.SetMinScore(floorDefault)

	if got := search(t, svc, 10); len(got) != 3 {
		t.Fatalf("a 100x rerank scale must behave identically; got %d hits", len(got))
	}

	// And the floor still prunes on that scale: 1 is ~1% of 91.
	weak := rerankTestService(t)
	weak.SetReranker(&scoredReranker{scores: []float64{91, 90, 1}}, "m", 50)
	weak.SetRerankEnabled(true)
	weak.SetMinScore(floorDefault)
	if got := search(t, weak, 10); len(got) != 2 {
		t.Fatalf("a hit at 1%% of the best rerank score must drop; got %d hits", len(got))
	}
}

// hybridTwoHomeRunService reproduces the pilot corpus that exposed the defect:
// exactly two spans carry `event: home_run`, one reachable on the vector path
// and one only on the lexical path, plus unrelated events. Hybrid fusion is on,
// so the result set is on the RRF scale.
func hybridTwoHomeRunService(t *testing.T) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	vectorHits := []model.SearchHit{
		annotationHit(1, homeRun, []string{hrBatterID, sfgID}),
		annotationHit(2, "pitch", []string{hrPitcherID, sfgID}),
		annotationHit(3, "at_bat", []string{hrBatterID, sfgID}),
	}
	for _, hit := range vectorHits {
		addAnnotationVector(t, idx, hit)
	}
	lexicalOnly := annotationHit(4, homeRun, []string{hrBatterID, sfgID})
	st := &lexicalHitStore{hits: append(append([]model.SearchHit(nil), vectorHits...), lexicalOnly)}

	svc := retrieval.NewService(st, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	for _, hit := range vectorHits {
		svc.SetChunkMetadata(hit.ChunkID, hit)
	}
	svc.SetChunkMetadata(lexicalOnly.ChunkID, lexicalOnly)
	return svc
}

// TestSearch_MinScoreFloor_858_FilteredHybrid is the reported repro: a filtered
// enumeration over a corpus with exactly two matching spans, k well above the
// match count, and the shipped floor. Both spans must come back. The floor
// returned one of them and the answer named one home run hitter of two, which
// read as complete.
func TestSearch_MinScoreFloor_858_FilteredHybrid(t *testing.T) {
	svc := hybridTwoHomeRunService(t)
	svc.SetMinScore(floorDefault)

	got := hybridSearchIDs(t, svc, model.SearchQuery{Events: []string{homeRun}, K: 50})
	if !sameIDs(got, []uint64{1, 4}) {
		t.Fatalf("both home_run spans must survive the floor; want [1 4], got %v", got)
	}

	// The floor is the only variable: disabling it must not change the answer.
	svc.SetMinScore(0)
	if unfloored := hybridSearchIDs(t, svc, model.SearchQuery{Events: []string{homeRun}, K: 50}); !sameIDs(unfloored, got) {
		t.Fatalf("floor 0.05 and floor 0 must agree on this set; %v vs %v", got, unfloored)
	}
}

// TestAsk_MinScoreFloor_858_AbstentionStaysAbsolute pins that the two §9.4.3
// controls stay independent. The relative floor now keeps a set of near-equal
// candidates in full, which is correct: it can only rank. Judging that set too
// weak to answer from is the ABSOLUTE threshold's job (classifyEvidence,
// evidenceThresholds), and it is unchanged, so uniformly weak candidates still
// abstain even though every one of them clears the relative floor.
func TestAsk_MinScoreFloor_858_AbstentionStaysAbsolute(t *testing.T) {
	idx := index.NewHNSWIndex("")
	gen := &fakeGenerator{out: "a confident, sourced-looking answer [docs/weak1.md]"}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, gen)
	// Two near-identical candidates, both far below the absolute cosine
	// threshold (0.05): near-equal to each other, weak in absolute terms.
	for id, cosine := range map[uint64]float64{1: 0.02, 2: 0.019} {
		addVec(t, idx, id, vecForCosine(cosine))
		svc.SetChunkMetadata(id, model.SearchHit{
			ChunkID: id, RelPath: "docs/weak.md", Snippet: "barely related text",
			Span: model.Span{Kind: "lines", StartLine: 1, EndLine: 2},
		})
	}
	svc.SetMinScore(floorDefault)

	got, err := svc.Ask(context.Background(), "q", model.SearchQuery{K: 5})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if gen.lastPrompt != "" {
		t.Fatalf("generator must not run on absolutely-weak evidence, prompt was %q", gen.lastPrompt)
	}
	if !strings.Contains(got.Answer, "Insufficient evidence") {
		t.Fatalf("expected an insufficient-evidence answer, got %q", got.Answer)
	}
	if len(got.Citations) != 0 {
		t.Fatalf("abstention must return an empty citations array, got %d", len(got.Citations))
	}
	// The relative floor kept both candidates: it is the absolute test that
	// rejected them, and the rejected set stays visible to the caller.
	if len(got.Hits) != 2 {
		t.Fatalf("the relative floor must keep both near-equal candidates; got %d hits", len(got.Hits))
	}
}
