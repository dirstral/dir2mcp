package retrieval

import (
	"context"
	"fmt"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

type uniformEmbedder struct {
	vec []float32
}

func (u *uniformEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		vec := make([]float32, len(u.vec))
		copy(vec, u.vec)
		out[i] = vec
	}
	return out, nil
}

func TestSearch_BothMode_LargeCorpus_NoDuplicates(t *testing.T) {
	svc := buildLargeCorpusService(t, 240)
	hits, err := svc.Search(context.Background(), model.SearchQuery{
		Query: "find relevant context",
		K:     80,
		Index: "both",
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected non-empty result set")
	}
	if len(hits) > 80 {
		t.Fatalf("expected at most 80 hits, got %d", len(hits))
	}

	seen := make(map[uint64]struct{}, len(hits))
	for _, hit := range hits {
		if _, ok := seen[hit.ChunkID]; ok {
			t.Fatalf("duplicate chunk id in both-mode results: %d", hit.ChunkID)
		}
		seen[hit.ChunkID] = struct{}{}
		if hit.Score < 0 || hit.Score > 1 {
			t.Fatalf("score out of normalized range [0,1]: %f", hit.Score)
		}
	}
}

func BenchmarkSearchBothLargeCorpus(b *testing.B) {
	// ensure any failures during index population are surfaced; previously we
	// passed nil which ignored Add errors and could let the benchmark run on
	// incomplete data.
	svc := buildLargeCorpusService(b, 300)
	ctx := context.Background()
	query := model.SearchQuery{Query: "find relevant context", K: 60, Index: "both"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		hits, err := svc.Search(ctx, query)
		if err != nil {
			b.Fatalf("Search failed: %v", err)
		}
		if len(hits) == 0 {
			b.Fatal("expected non-empty hits")
		}
	}
}

func buildLargeCorpusService(tb testing.TB, total int) *Service {
	textIdx := index.NewHNSWIndex("")
	codeIdx := index.NewHNSWIndex("")

	ctx := context.Background()
	for i := 1; i <= total; i++ {
		label := uint64(i)
		vec := []float32{1, 0}
		if i%2 == 0 {
			vec = []float32{0.95, 0.05}
		}
		textPayload := model.IndexPayload{
			ChunkID: label,
			RelPath: fmt.Sprintf("docs/%03d.md", i),
			DocType: "md",
			Snippet: fmt.Sprintf("snippet-%d", i),
		}
		if err := textIdx.Upsert(ctx, vec, textPayload); err != nil {
			tb.Fatalf("textIdx.Upsert(%d) failed: %v", i, err)
		}
		if i%3 == 0 {
			// we intentionally reuse the same `label` value here that was already
			// added to `textIdx` above. this creates duplicate IDs across the two
			// indexes (every third label appears in both).
			// the test later searches in "both" mode and the Service deduplicates
			// hits coming from textIdx and codeIdx. documenting the design choice
			// helps future readers understand why some labels appear in both
			// indexes and ensures coverage of the deduplication logic.
			if err := codeIdx.Upsert(ctx, []float32{1, 0}, textPayload); err != nil {
				tb.Fatalf("codeIdx.Upsert(%d) failed: %v", i, err)
			}
		}
	}

	svc := NewService(nil, textIdx, &uniformEmbedder{vec: []float32{1, 0}}, nil)
	svc.SetCodeIndex(codeIdx)
	for i := 1; i <= total; i++ {
		label := uint64(i)
		svc.SetChunkMetadataForIndex("text", label, model.SearchHit{
			RelPath: fmt.Sprintf("docs/%03d.md", i),
			DocType: "md",
			Snippet: fmt.Sprintf("snippet-%d", i),
		})
		if i%3 == 0 {
			svc.SetChunkMetadataForIndex("code", label, model.SearchHit{
				RelPath: fmt.Sprintf("src/%03d.go", i),
				DocType: "code",
				Snippet: fmt.Sprintf("code-%d", i),
			})
		}
	}
	return svc
}
