package tests

import (
	"context"
	"fmt"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// Refill round loss: dir2mcp #818, a correctness follow-up to #686.
//
// The refill of #686 re-retrieves a WIDER candidate pool when dropped `summary`
// routing nodes leave the result short. A wider pool is not a superset of the
// narrower one: a vector index is approximate, so a larger k explores a
// different part of the graph. The loop kept only its LAST round, so a fine
// chunk that an earlier round legitimately surfaced never reached the caller.
// The loss is worst in the exact case the refill exists to repair: the wider
// round's extra candidates are themselves summaries that expand to nothing, so
// the repair round returns fewer hits than the round it repairs.
//
// The suite drives that shape through the public Search path with an index that
// answers each retrieval round with its own recorded candidate set.

// refillCand is one recorded candidate: a chunk id, its cosine score, and
// whether it is a `summary` routing node. Use fineCand / summaryCand to build
// one, so a round literal states which candidates cost the round a result slot.
type refillCand struct {
	id      uint64
	score   float32
	summary bool
}

func fineCand(id uint64, score float32) refillCand {
	return refillCand{id: id, score: score}
}

func summaryCand(id uint64, score float32) refillCand {
	return refillCand{id: id, score: score, summary: true}
}

// roundedIndex is an APPROXIMATE index stub: it answers the n-th call with the
// n-th recorded candidate set, so a wider round can legitimately return
// candidates the narrower round missed AND miss candidates the narrower round
// found. Calls past the last recorded set return nothing (an exhausted index).
//
// Each retrieval round makes exactly ONE call here: the service's own overfetch
// fanout asks for k*5 candidates, every recorded set is smaller than that, and a
// backend that returns fewer labels than it was asked for counts as exhausted.
// So one recorded set is one refill round, and calls counts the rounds.
type roundedIndex struct {
	rounds [][]refillCand
	calls  int
}

func (r *roundedIndex) Upsert(_ context.Context, _ []float32, _ model.IndexPayload) error {
	return nil
}
func (r *roundedIndex) Delete(_ context.Context, _ []uint64) error { return nil }
func (r *roundedIndex) Search(_ context.Context, _ []float32, k int, _ model.Filter) ([]model.IndexHit, error) {
	r.calls++
	if r.calls > len(r.rounds) {
		return nil, nil
	}
	cands := r.rounds[r.calls-1]
	if k < len(cands) {
		cands = cands[:k]
	}
	out := make([]model.IndexHit, 0, len(cands))
	for _, c := range cands {
		out = append(out, model.IndexHit{ChunkID: c.id, Score: c.score})
	}
	return out, nil
}
func (r *roundedIndex) Identity(context.Context) (string, error) { return "", nil }
func (r *roundedIndex) Reset(context.Context, string) error      { return nil }
func (r *roundedIndex) Close() error                             { return nil }

// roundedService wires a vector-only service over the recorded rounds and
// registers the chunk metadata each recorded candidate needs. Hierarchical
// expansion is OFF, the most common deployment: a summary is then dropped
// unconditionally (citation faithfulness, SPEC §9.7), so it costs its round a
// result slot and triggers the refill.
func roundedService(t *testing.T, rounds [][]refillCand) (*retrieval.Service, *roundedIndex) {
	t.Helper()
	idx := &roundedIndex{rounds: rounds}
	svc := retrieval.NewService(&summaryExpandStore{}, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	svc.SetHybridEnabled(false) // vector-only: deterministic ranking, no BM25 dependency
	for _, round := range rounds {
		for _, c := range round {
			if c.summary {
				svc.SetChunkMetadata(c.id, model.SearchHit{
					ChunkID: c.id, RelPath: "docs/report.md", DocType: "md",
					RepType: model.SummaryRepType, Snippet: "MODEL-GENERATED SUMMARY PROSE",
				})
				continue
			}
			svc.SetChunkMetadata(c.id, model.SearchHit{
				ChunkID: c.id, RelPath: fmt.Sprintf("docs/fine-%d.md", c.id), DocType: "md",
				RepType: "raw_text", Snippet: fmt.Sprintf("fine chunk %d", c.id),
			})
		}
	}
	svc.SetHierarchical(false)
	return svc, idx
}

// assertRounds fails when retrieval ran a different number of rounds than the
// fix allows. Accumulating rounds must never buy recall with extra index calls.
func assertRounds(t *testing.T, idx *roundedIndex, want int) {
	t.Helper()
	if idx.calls != want {
		t.Fatalf("retrieval ran %d rounds, want %d", idx.calls, want)
	}
}

// TestSearch_SummaryRefill_KeepsHitFoundByAnEarlierRound is the headline case of
// #818. Round 0 finds fine chunk 11 and one summary, so the refill widens the
// pool. The wider round 1 is a different neighbourhood: two summaries and NO
// fine chunk. Returning only the last round returns nothing, so chunk 11 (found,
// eligible, and already ranked) is lost.
func TestSearch_SummaryRefill_KeepsHitFoundByAnEarlierRound(t *testing.T) {
	svc, idx := roundedService(t, [][]refillCand{
		{fineCand(11, 0.90), summaryCand(10, 0.80)},
		{summaryCand(10, 0.80), summaryCand(30, 0.70)},
	})

	hits := searchHits(t, svc, 2)
	assertNoSummaryHit(t, hits)
	assertOrder(t, hitIDs(hits), []uint64{11})
	assertRounds(t, idx, 2)
}

// TestSearch_SummaryRefill_RanksTheUnionByScore pins the ranking rule: the
// accumulated pool is ordered by the authoritative Score, never by the round
// that found a hit. Round 0 finds a weak fine chunk (11, 0.50); the wider round
// 1 finds a stronger one (12, 0.95). Both must come back, best first.
func TestSearch_SummaryRefill_RanksTheUnionByScore(t *testing.T) {
	svc, idx := roundedService(t, [][]refillCand{
		{summaryCand(10, 0.99), fineCand(11, 0.50)},
		{summaryCand(10, 0.99), fineCand(12, 0.95), summaryCand(30, 0.60)},
	})

	hits := searchHits(t, svc, 2)
	assertNoSummaryHit(t, hits)
	// Round order would be [11, 12]; score order is [12, 11].
	assertOrder(t, hitIDs(hits), []uint64{12, 11})
	assertRounds(t, idx, 2)
}

// TestSearch_SummaryRefill_UnionIsDeduped pins that accumulation never doubles a
// chunk both rounds found. Chunk 11 is in both recorded rounds.
func TestSearch_SummaryRefill_UnionIsDeduped(t *testing.T) {
	svc, _ := roundedService(t, [][]refillCand{
		{summaryCand(10, 0.99), fineCand(11, 0.50)},
		{summaryCand(10, 0.99), fineCand(11, 0.50), summaryCand(30, 0.40)},
	})

	hits := searchHits(t, svc, 5)
	seen := map[uint64]int{}
	for _, h := range hits {
		seen[h.ChunkID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("chunk %d appears %d times; the accumulated pool must be deduped (ids %v)",
				id, n, hitIDs(hits))
		}
	}
	assertOrder(t, hitIDs(hits), []uint64{11})
}

// TestSearch_SummaryRefill_UnionRespectsCallerK pins the budget: accumulation
// makes the pool BIGGER than any one round, so the final truncation must still
// bound the result at the caller's k. Round 0 contributes one hit, round 1 three
// more, and k is 2.
func TestSearch_SummaryRefill_UnionRespectsCallerK(t *testing.T) {
	svc, idx := roundedService(t, [][]refillCand{
		{summaryCand(10, 0.99), fineCand(11, 0.50)},
		{fineCand(12, 0.95), fineCand(13, 0.94), fineCand(14, 0.93)},
	})

	hits := searchHits(t, svc, 2)
	if len(hits) != 2 {
		t.Fatalf("asked for k=2, got %d hits (ids %v): the accumulated pool must still be truncated to k",
			len(hits), hitIDs(hits))
	}
	assertOrder(t, hitIDs(hits), []uint64{12, 13})
	assertRounds(t, idx, 2)
}

// TestSearch_SummaryRefill_NoSummaryCorpusIsOneUntouchedRound pins the
// regression that would be easiest to cause and hardest to notice: with no
// summary candidate in the pool (every deployment with the feature off) the
// search makes exactly ONE retrieval round and returns it unchanged: same hits,
// same order, same scores. The second recorded round exists only to prove that
// it is never consulted.
func TestSearch_SummaryRefill_NoSummaryCorpusIsOneUntouchedRound(t *testing.T) {
	svc, idx := roundedService(t, [][]refillCand{
		{fineCand(11, 0.90), fineCand(12, 0.80)},
		{fineCand(13, 0.70)},
	})

	hits := searchHits(t, svc, 2)
	assertRounds(t, idx, 1)
	assertOrder(t, hitIDs(hits), []uint64{11, 12})
	// float32 scores widen to float64 exactly as the pipeline does it, so the
	// comparison pins the value the flat path passes through.
	wantScores := []float64{float64(float32(0.90)), float64(float32(0.80))}
	for i, h := range hits {
		if h.Score != wantScores[i] {
			t.Fatalf("hit %d (chunk %d) scored %v, want %v: the flat path must pass its round through untouched",
				i, h.ChunkID, h.Score, wantScores[i])
		}
	}
}
