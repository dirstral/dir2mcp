package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// SPEC §9.10 semantics through the real retrieval service: OR within a key,
// AND across keys, literal equality, annotation-only eligibility, and the
// empty forms disabling rather than matching nothing. tests/mcp pins that the
// argument arrives; this pins that arriving changes the result set.

func attrRetrievalService(t *testing.T) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, &fakeGenerator{out: "ok"})
	spans := []model.Span{
		{Kind: "time", StartMS: 0, EndMS: 1, Event: "pitch",
			Attributes: map[string]string{"inning": "8", "half": "bottom"}},
		{Kind: "time", StartMS: 2, EndMS: 3, Event: "pitch",
			Attributes: map[string]string{"inning": "8", "half": "top"}},
		{Kind: "time", StartMS: 4, EndMS: 5, Event: "pitch",
			Attributes: map[string]string{"inning": "6"}},
		// A transcript span: no attributes, must never match a non-empty filter.
		{Kind: "time", StartMS: 6, EndMS: 7},
	}
	for i, span := range spans {
		id := uint64(i + 1)
		// The payload carries the span, as payloadFromTask does at ingestion:
		// the FilteringIndex pushdown evaluates the annotation predicate against
		// the payload, so a rig that omits it would test a pushdown that
		// production never runs.
		if err := idx.Upsert(context.Background(), []float32{1, 0}, model.IndexPayload{
			ChunkID: id, RelPath: "game.mp4", Snippet: "pitch text", Span: span,
		}); err != nil {
			t.Fatalf("Upsert(%d): %v", id, err)
		}
		svc.SetChunkMetadata(id, model.SearchHit{
			ChunkID: id, RelPath: "game.mp4", Snippet: "pitch text",
			Span: span,
		})
	}
	return svc
}

func attrSearchIDs(t *testing.T, svc *retrieval.Service, attrs map[string][]string) map[uint64]bool {
	t.Helper()
	hits, err := svc.Search(context.Background(), model.SearchQuery{
		Query: "pitch", K: 10, Attributes: attrs,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	out := map[uint64]bool{}
	for _, h := range hits {
		out[h.ChunkID] = true
	}
	return out
}

func TestAttributes928_EqualityAndKeyMissRules(t *testing.T) {
	svc := attrRetrievalService(t)
	// inning=8 keeps the two 8th-inning spans, drops the 6th and the
	// attribute-less transcript span (a missing key does not match).
	got := attrSearchIDs(t, svc, map[string][]string{"inning": {"8"}})
	if !got[1] || !got[2] || got[3] || got[4] {
		t.Fatalf("inning=8 selected %v, want {1,2}", got)
	}
}

func TestAttributes928_ORWithinAKey(t *testing.T) {
	svc := attrRetrievalService(t)
	got := attrSearchIDs(t, svc, map[string][]string{"inning": {"6", "8"}})
	if !got[1] || !got[2] || !got[3] || got[4] {
		t.Fatalf("inning IN (6,8) selected %v, want {1,2,3}", got)
	}
}

func TestAttributes928_ANDAcrossKeys(t *testing.T) {
	svc := attrRetrievalService(t)
	got := attrSearchIDs(t, svc, map[string][]string{
		"inning": {"8"}, "half": {"bottom"},
	})
	if !got[1] || got[2] || got[3] || got[4] {
		t.Fatalf("inning=8 AND half=bottom selected %v, want {1}", got)
	}
}

func TestAttributes928_LiteralCaseSensitiveMatch(t *testing.T) {
	svc := attrRetrievalService(t)
	// Case is NEVER folded: attribute values are opaque producer tokens, and
	// folding could collide two the producer considers distinct (the same
	// argument MatchesAnyLiteral documents for entity ids).
	if got := attrSearchIDs(t, svc, map[string][]string{"half": {"BOTTOM"}}); len(got) != 0 {
		t.Fatalf("case-insensitive match crept in: %v", got)
	}
	// Whitespace at the FILTER edge is forgiven, deliberately: the shared
	// matcher trims both sides, which is the exact shipped semantics of the
	// §9.9 entities/events filter that §9.10 mirrors, and stored values are
	// trim-normalized at ingestion so trimming cannot conflate two distinct
	// stored values.
	if got := attrSearchIDs(t, svc, map[string][]string{"inning": {" 8"}}); !got[1] || !got[2] {
		t.Fatalf("filter-edge whitespace was not forgiven (diverges from the §9.9 matcher): %v", got)
	}
}

func TestAttributes928_UnknownValueDoesNotPoisonTheOR(t *testing.T) {
	svc := attrRetrievalService(t)
	got := attrSearchIDs(t, svc, map[string][]string{"inning": {"99", "8"}})
	if !got[1] || !got[2] || got[3] {
		t.Fatalf("an unknown sibling value poisoned the OR: %v", got)
	}
	// A filter whose every constraint matches nothing is a normal empty result.
	if got := attrSearchIDs(t, svc, map[string][]string{"inning": {"99"}}); len(got) != 0 {
		t.Fatalf("unknown-only value returned hits: %v", got)
	}
}

func TestAttributes928_EmptyFormsDisable(t *testing.T) {
	svc := attrRetrievalService(t)
	all := attrSearchIDs(t, svc, nil)
	if len(all) != 4 {
		t.Fatalf("baseline should return all 4, got %v", all)
	}
	for name, attrs := range map[string]map[string][]string{
		"empty map":       {},
		"key empty array": {"inning": {}},
	} {
		got := attrSearchIDs(t, svc, attrs)
		if len(got) != 4 {
			t.Fatalf("%s narrowed the result set: %v", name, got)
		}
	}
	// An empty-array key beside a real constraint is ignored, not match-nothing.
	got := attrSearchIDs(t, svc, map[string][]string{"inning": {"8"}, "half": {}})
	if !got[1] || !got[2] || got[3] {
		t.Fatalf("an empty-array key changed a sibling constraint: %v", got)
	}
}
