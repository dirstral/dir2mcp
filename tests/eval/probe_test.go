package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
)

// TestFixturesLoadAndValidate pins that the shipped testdata corpus + query set
// parse and pass the harness's structural validation (unique non-zero chunk
// ids, matching vector dimensions, queries referencing only known rel_paths).
// A broken fixture fails here with a precise message rather than silently
// skewing the ablation metrics.
func TestFixturesLoadAndValidate(t *testing.T) {
	corpus, err := loadCorpus("testdata")
	if err != nil {
		t.Fatalf("fixtures failed to load/validate: %v", err)
	}
	if len(corpus.docs) == 0 || len(corpus.queries) == 0 {
		t.Fatalf("fixtures empty: docs=%d queries=%d", len(corpus.docs), len(corpus.queries))
	}
}

// TestDictEmbedderIsDeterministic pins the creds-free embedder contract: the
// same query text always yields the same vector, and an unknown text yields a
// stable fallback (so a fixture typo degrades gracefully instead of panicking).
func TestDictEmbedderIsDeterministic(t *testing.T) {
	corpus, err := loadCorpus("testdata")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	emb := newDictEmbedder(corpus.queries, len(corpus.docs[0].Vector))
	q := corpus.queries[0].Text
	a, err := emb.Embed(context.Background(), "any-model", model.EmbedQuery, []string{q, q})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(a) != 2 {
		t.Fatalf("expected one vector per text, got %d", len(a))
	}
	for i := range a[0] {
		if a[0][i] != a[1][i] {
			t.Fatalf("embedder is non-deterministic at dim %d: %v vs %v", i, a[0], a[1])
		}
	}
	unknown, err := emb.Embed(context.Background(), "m", model.EmbedQuery, []string{"no such query text"})
	if err != nil {
		t.Fatalf("embed unknown: %v", err)
	}
	if len(unknown) != 1 || len(unknown[0]) != len(corpus.docs[0].Vector) {
		t.Fatalf("unknown-text fallback must be a dim-correct vector, got %v", unknown)
	}
}

// TestBM25StoreRanksByOverlap pins the in-memory lexical store used to engage
// the hybrid knob without sqlite/FTS5: it ranks by query/document term overlap,
// best-first, and skips zero-overlap documents.
func TestBM25StoreRanksByOverlap(t *testing.T) {
	corpus, err := loadCorpus("testdata")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	store := newBM25Store(corpus.docs)
	hits, err := store.SearchBM25(context.Background(), "plants convert sunlight into energy through photosynthesis", 10, "")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected BM25 hits for a lexically-matching query")
	}
	if hits[0].RelPath != "misc/photosynthesis.md" {
		t.Fatalf("expected photosynthesis doc to rank first lexically, got %q", hits[0].RelPath)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Fatalf("BM25 hits not sorted best-first at %d: %v", i, hits)
		}
	}
}
