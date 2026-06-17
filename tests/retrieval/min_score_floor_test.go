package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// newMinScoreFloorService builds a vector-only retrieval service (nil store
// keeps hybrid fusion from engaging, so each hit's Score is the raw cosine
// similarity) with the server-side relevance floor configured.
//
// The three candidates have deterministic cosine scores against the query
// vector {1, 0}: chunk 1 → 1.00, chunk 2 → 0.60, chunk 3 → 0.28.
func newMinScoreFloorService(t *testing.T, floor float64) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	addVec(t, idx, 1, []float32{1, 0})       // cosine 1.00
	addVec(t, idx, 2, []float32{0.6, 0.8})   // cosine 0.60 (unit vector)
	addVec(t, idx, 3, []float32{0.28, 0.96}) // cosine 0.28 (unit vector)

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetChunkMetadata(1, model.SearchHit{RelPath: "a.md", Snippet: "alpha"})
	svc.SetChunkMetadata(2, model.SearchHit{RelPath: "b.md", Snippet: "beta"})
	svc.SetChunkMetadata(3, model.SearchHit{RelPath: "c.md", Snippet: "gamma"})
	svc.SetMinScore(floor)
	return svc
}

func searchFloorChunkIDs(t *testing.T, svc *retrieval.Service) []uint64 {
	t.Helper()
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := make([]uint64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ChunkID)
	}
	return ids
}

// TestSearch_MinScoreFloor_DropsSubThresholdHits pins the core behavior: with a
// floor of 0.5, the two strong hits (scores 1.00 and 0.60) survive and the weak
// hit (0.28) is dropped before results reach the model.
func TestSearch_MinScoreFloor_DropsSubThresholdHits(t *testing.T) {
	svc := newMinScoreFloorService(t, 0.5)
	got := searchFloorChunkIDs(t, svc)
	want := []uint64{1, 2}
	if len(got) != len(want) {
		t.Fatalf("expected %v survivors, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order/survivor mismatch: want %v, got %v", want, got)
		}
	}
}

// TestSearch_MinScoreFloor_DisabledIsPassThrough pins the default-off behavior:
// floor 0 returns every candidate unchanged, including the weak (0.28) hit.
func TestSearch_MinScoreFloor_DisabledIsPassThrough(t *testing.T) {
	svc := newMinScoreFloorService(t, 0)
	got := searchFloorChunkIDs(t, svc)
	if len(got) != 3 {
		t.Fatalf("floor=0 must pass through all candidates; got %v", got)
	}
}

// TestSearch_MinScoreFloor_NegativeIsPassThrough pins that a non-positive floor
// (defensive: validation rejects negatives, but the service itself treats <= 0
// as disabled) is a pass-through rather than dropping everything.
func TestSearch_MinScoreFloor_NegativeIsPassThrough(t *testing.T) {
	svc := newMinScoreFloorService(t, -1)
	got := searchFloorChunkIDs(t, svc)
	if len(got) != 3 {
		t.Fatalf("floor<=0 must pass through all candidates; got %v", got)
	}
}

// TestSearch_MinScoreFloor_BoundaryKeepsEqual pins the cutoff semantics: a hit
// whose score equals the floor is KEPT (strict less-than drops). With floor
// 0.60, chunks 1 (1.00) and 2 (0.60, equal) survive; chunk 3 (0.28) is dropped.
func TestSearch_MinScoreFloor_BoundaryKeepsEqual(t *testing.T) {
	svc := newMinScoreFloorService(t, 0.6)
	got := searchFloorChunkIDs(t, svc)
	want := []uint64{1, 2}
	if len(got) != len(want) {
		t.Fatalf("expected %v survivors (score == floor kept), got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("boundary mismatch: want %v, got %v", want, got)
		}
	}
}

// TestSearch_MinScoreFloor_DropsAllWhenAboveEveryScore pins that an aggressive
// floor above every candidate's score yields zero hits (the floor may legitimately
// return fewer than k — even zero — hits).
func TestSearch_MinScoreFloor_DropsAllWhenAboveEveryScore(t *testing.T) {
	svc := newMinScoreFloorService(t, 1.5)
	got := searchFloorChunkIDs(t, svc)
	if len(got) != 0 {
		t.Fatalf("floor above all scores must drop everything; got %v", got)
	}
}
