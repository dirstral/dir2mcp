package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// chunkIDs collects the chunk IDs from a hit slice, preserving order.
func chunkIDs(hits []model.IndexHit) []uint64 {
	out := make([]uint64, len(hits))
	for i, h := range hits {
		out[i] = h.ChunkID
	}
	return out
}

func TestHNSWIndex_SearchWithDocTypeFilter(t *testing.T) {
	ctx := context.Background()
	idx := index.NewHNSWIndex("")
	mustUpsert(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "docs/a.md", DocType: "md"}, []float32{1, 0})
	mustUpsert(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "src/a.go", DocType: "code"}, []float32{0.99, 0.01})
	mustUpsert(t, idx, model.IndexPayload{ChunkID: 3, RelPath: "docs/b.md", DocType: "md"}, []float32{0.98, 0.02})

	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{DocTypes: []string{"MD"}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := chunkIDs(hits)
	if len(got) != 2 {
		t.Fatalf("expected 2 md hits, got %v", got)
	}
	for _, id := range got {
		if id == 2 {
			t.Fatalf("code chunk 2 leaked past doctype filter: %v", got)
		}
	}
}

func TestHNSWIndex_SearchWithPathPrefixAndGlobFilter(t *testing.T) {
	ctx := context.Background()
	idx := index.NewHNSWIndex("")
	mustUpsert(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "docs/a.md", DocType: "md"}, []float32{1, 0})
	mustUpsert(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "docs/b.md", DocType: "md"}, []float32{0.99, 0.01})
	mustUpsert(t, idx, model.IndexPayload{ChunkID: 3, RelPath: "src/main.go", DocType: "code"}, []float32{0.98, 0.02})

	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{PathPrefix: "docs/", PathGlob: "docs/a.*"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := chunkIDs(hits)
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("expected only chunk 1 (docs/a.md), got %v", got)
	}
}

func TestHNSWIndex_DeleteRemovesVector(t *testing.T) {
	ctx := context.Background()
	idx := index.NewHNSWIndex("")
	mustUpsert(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 0})
	mustUpsert(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "b.md"}, []float32{0.9, 0.1})

	if err := idx.Delete(ctx, []uint64{1}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	got := chunkIDs(hits)
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("expected only chunk 2 after delete, got %v", got)
	}
	// Deleting an unknown id is a no-op, not an error.
	if err := idx.Delete(ctx, []uint64{999}); err != nil {
		t.Fatalf("Delete unknown id: %v", err)
	}
}

func TestHNSWIndex_IdentityAndReset(t *testing.T) {
	ctx := context.Background()
	idx := index.NewHNSWIndex("")

	id, err := idx.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	if id != "" {
		t.Fatalf("fresh index identity should be empty, got %q", id)
	}

	mustUpsert(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 0})
	if err := idx.Reset(ctx, "mistral|mistral-embed|codestral-embed|0|0|off"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	id, err = idx.Identity(ctx)
	if err != nil {
		t.Fatalf("Identity after reset: %v", err)
	}
	if id != "mistral|mistral-embed|codestral-embed|0|0|off" {
		t.Fatalf("unexpected identity after reset: %q", id)
	}
	// Reset clears all vectors.
	hits, err := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search after reset: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected empty index after reset, got %v", chunkIDs(hits))
	}
}

func TestEnsureIdentity_ResetsOnMismatchAndEmpty(t *testing.T) {
	ctx := context.Background()
	idx := index.NewHNSWIndex("")
	mustUpsert(t, idx, model.IndexPayload{ChunkID: 1, RelPath: "a.md"}, []float32{1, 0})

	// Fresh index (empty identity) reconciles to the configured one and is reset.
	if err := index.EnsureIdentity(ctx, idx, "ident-1"); err != nil {
		t.Fatalf("EnsureIdentity (fresh): %v", err)
	}
	if id, _ := idx.Identity(ctx); id != "ident-1" {
		t.Fatalf("expected identity ident-1, got %q", id)
	}
	if hits, _ := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{}); len(hits) != 0 {
		t.Fatalf("expected vectors cleared on fresh reconcile, got %v", chunkIDs(hits))
	}

	// A matching identity is a no-op: vectors are preserved.
	mustUpsert(t, idx, model.IndexPayload{ChunkID: 2, RelPath: "b.md"}, []float32{1, 0})
	if err := index.EnsureIdentity(ctx, idx, "ident-1"); err != nil {
		t.Fatalf("EnsureIdentity (match): %v", err)
	}
	if hits, _ := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{}); len(hits) != 1 {
		t.Fatalf("matching identity must not clear vectors, got %v", chunkIDs(hits))
	}

	// A changed identity resets again (vector space changed).
	if err := index.EnsureIdentity(ctx, idx, "ident-2"); err != nil {
		t.Fatalf("EnsureIdentity (mismatch): %v", err)
	}
	if id, _ := idx.Identity(ctx); id != "ident-2" {
		t.Fatalf("expected identity ident-2, got %q", id)
	}
	if hits, _ := idx.Search(ctx, []float32{1, 0}, 10, model.Filter{}); len(hits) != 0 {
		t.Fatalf("expected vectors cleared on identity change, got %v", chunkIDs(hits))
	}
}

func TestHNSWIndex_CanFilterAlwaysTrue(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if !idx.CanFilter(model.Filter{}) {
		t.Fatal("pure-Go HNSW should report CanFilter true for the zero filter")
	}
	if !idx.CanFilter(model.Filter{PathPrefix: "docs/", DocTypes: []string{"md"}}) {
		t.Fatal("pure-Go HNSW should report CanFilter true for a populated filter")
	}
}

func mustUpsert(t *testing.T, idx *index.HNSWIndex, payload model.IndexPayload, vec []float32) {
	t.Helper()
	if err := idx.Upsert(context.Background(), vec, payload); err != nil {
		t.Fatalf("Upsert(%d): %v", payload.ChunkID, err)
	}
}
