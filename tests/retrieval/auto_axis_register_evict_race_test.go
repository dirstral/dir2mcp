package tests

import (
	"context"
	"sync"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// fixedLabelIndex returns the same label for every search, so a hit comes back
// exactly when the service still holds that label's chunk metadata.
type fixedLabelIndex struct{ label uint64 }

func (f *fixedLabelIndex) Upsert(context.Context, []float32, model.IndexPayload) error { return nil }
func (f *fixedLabelIndex) Delete(context.Context, []uint64) error                      { return nil }
func (f *fixedLabelIndex) Search(_ context.Context, _ []float32, _ int, _ model.Filter) ([]model.IndexHit, error) {
	return []model.IndexHit{{ChunkID: f.label, Score: 1}}, nil
}
func (f *fixedLabelIndex) Identity(context.Context) (string, error) { return "", nil }
func (f *fixedLabelIndex) Reset(context.Context, string) error      { return nil }
func (f *fixedLabelIndex) Close() error                             { return nil }

// TestSearchWithAxis_RegisterAndEvictRace pins that registering a chunk's
// metadata and recording its axis happen in one critical section. When the two
// were separate, an eviction between them removed the metadata and then saw
// the axis recorded again: index=auto counted a code chunk that no longer
// existed and routed a prose question to "both". After each race the axis
// count and the metadata must agree: auto picks "both" exactly when the code
// chunk is still searchable.
//
// The window is two lock releases wide, so the loop is long: against the split
// version the check failed in each of two runs (at iterations 28286 and 6174),
// and with 2000 iterations it never failed. It takes about 6 s.
func TestSearchWithAxis_RegisterAndEvictRace(t *testing.T) {
	const codeLabel = 2
	prose := model.SearchQuery{Index: "auto", Query: "how is the signature verified"}
	codeOnly := model.SearchQuery{Index: "code", Query: "how is the signature verified", K: 5}
	emb := &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{"mistral-embed": {1, 0}}}

	for i := 0; i < 50000; i++ {
		svc := retrieval.NewService(nil, &fixedLabelIndex{label: codeLabel}, emb, nil)
		svc.SetChunkMetadataForIndex("text", 1, model.SearchHit{ChunkID: 1, RelPath: "notes.md"})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			svc.SetChunkMetadataForIndex("code", codeLabel, model.SearchHit{ChunkID: codeLabel, RelPath: "main.go"})
		}()
		go func() {
			defer wg.Done()
			svc.EvictChunks([]uint64{codeLabel})
		}()
		wg.Wait()

		hits, err := svc.Search(context.Background(), codeOnly)
		if err != nil {
			t.Fatalf("iteration %d: code search: %v", i, err)
		}
		codeLive := false
		for _, h := range hits {
			if h.ChunkID == codeLabel {
				codeLive = true
			}
		}
		_, axis, err := svc.SearchWithAxis(context.Background(), prose)
		if err != nil {
			t.Fatalf("iteration %d: auto search: %v", i, err)
		}
		want := "text"
		if codeLive {
			want = "both"
		}
		if axis != want {
			t.Fatalf("iteration %d: auto axis = %q with code chunk live=%v, want %q", i, axis, codeLive, want)
		}
	}
}
