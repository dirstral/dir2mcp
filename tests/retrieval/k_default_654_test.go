package tests

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// newKDefaultService builds a vector-only service over `count` candidates, so a
// search that returns fewer than `count` hits was truncated by k and nothing
// else.
func newKDefaultService(t *testing.T, count int) *retrieval.Service {
	t.Helper()
	idx := index.NewHNSWIndex("")
	for i := 1; i <= count; i++ {
		// Fan the candidates around the query vector {1, 0} so every one of them
		// scores above zero and their order is deterministic.
		angle := float64(i) * (math.Pi / 4) / float64(count+1)
		addVec(t, idx, uint64(i), []float32{float32(math.Cos(angle)), float32(math.Sin(angle))})
	}
	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	for i := 1; i <= count; i++ {
		svc.SetChunkMetadata(uint64(i), model.SearchHit{RelPath: fmt.Sprintf("doc-%d.md", i), Snippet: "text"})
	}
	return svc
}

func searchHitCount(t *testing.T, svc *retrieval.Service, k int) int {
	t.Helper()
	hits, err := svc.Search(context.Background(), model.SearchQuery{Query: "alpha", K: k})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return len(hits)
}

// TestService_DefaultKIsConfigurable pins the retrieval half of issue #654: the
// service resolves a query that carries no k of its own against the operator's
// rag.k_default, which the engine plumbs in with SetDefaultK. On main the
// service replaced an absent k with a hardcoded 15.
func TestService_DefaultKIsConfigurable(t *testing.T) {
	svc := newKDefaultService(t, 20)
	svc.SetDefaultK(3)
	if got := searchHitCount(t, svc, 0); got != 3 {
		t.Fatalf("search with no k returned %d hits, want the configured default 3", got)
	}
}

// TestService_QueryKWinsOverDefaultK keeps the precedence: a query that names
// its own k is never overridden by the configured default.
func TestService_QueryKWinsOverDefaultK(t *testing.T) {
	svc := newKDefaultService(t, 20)
	svc.SetDefaultK(3)
	if got := searchHitCount(t, svc, 7); got != 7 {
		t.Fatalf("search with k=7 returned %d hits, want 7", got)
	}
}

// TestService_UnsetDefaultKIsTheShippedFallback pins step 3 of the precedence: a
// service the engine never configured retrieves the shipped fallback, so an
// embedder that skips SetDefaultK keeps today's behavior.
func TestService_UnsetDefaultKIsTheShippedFallback(t *testing.T) {
	svc := newKDefaultService(t, 20)
	if got := searchHitCount(t, svc, 0); got != config.RAGKFallback {
		t.Fatalf("search with no k returned %d hits, want the shipped fallback %d", got, config.RAGKFallback)
	}
}

// TestService_OutOfBoundDefaultKFallsBack guards the setter: config validation
// rejects an out-of-bound rag.k_default at load, so a value that still arrives
// here comes from a hand-built Config and must not silently retrieve nothing.
func TestService_OutOfBoundDefaultKFallsBack(t *testing.T) {
	for _, k := range []int{0, -5, config.RAGKMax + 1} {
		t.Run(fmt.Sprintf("k=%d", k), func(t *testing.T) {
			svc := newKDefaultService(t, 20)
			svc.SetDefaultK(k)
			if got := searchHitCount(t, svc, 0); got != config.RAGKFallback {
				t.Fatalf("SetDefaultK(%d): search returned %d hits, want the fallback %d", k, got, config.RAGKFallback)
			}
		})
	}
}
