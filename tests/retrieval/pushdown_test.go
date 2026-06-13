package tests

import (
	"context"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// payloadFilteringIndex is a stand-in for an external/on-disk backend (issue
// #247): it stores payloads, evaluates the pushed-down Filter itself
// (CanFilter true), and returns hits with populated payloads. Crucially the
// test registers NO in-memory chunk metadata on the service, so retrieval must
// materialise SearchHits from the backend payload — exercising the
// FilteringIndex push-down path end to end.
type payloadFilteringIndex struct {
	vectors  map[uint64][]float32
	payloads map[uint64]model.IndexPayload
	// lastFilter records the filter the service pushed down on the last Search.
	lastFilter model.Filter
}

func newPayloadFilteringIndex() *payloadFilteringIndex {
	return &payloadFilteringIndex{
		vectors:  map[uint64][]float32{},
		payloads: map[uint64]model.IndexPayload{},
	}
}

func (p *payloadFilteringIndex) Upsert(_ context.Context, vec []float32, payload model.IndexPayload) error {
	p.vectors[payload.ChunkID] = vec
	p.payloads[payload.ChunkID] = payload
	return nil
}
func (p *payloadFilteringIndex) Delete(_ context.Context, ids []uint64) error {
	for _, id := range ids {
		delete(p.vectors, id)
		delete(p.payloads, id)
	}
	return nil
}
func (p *payloadFilteringIndex) CanFilter(model.Filter) bool { return true }
func (p *payloadFilteringIndex) Search(_ context.Context, _ []float32, k int, filter model.Filter) ([]model.IndexHit, error) {
	p.lastFilter = filter
	hits := make([]model.IndexHit, 0, len(p.payloads))
	for id, payload := range p.payloads {
		if !filter.Match(payload) {
			continue
		}
		hits = append(hits, model.IndexHit{ChunkID: id, Score: 1, Payload: payload})
		if len(hits) >= k {
			break
		}
	}
	return hits, nil
}
func (p *payloadFilteringIndex) Identity(context.Context) (string, error) { return "", nil }
func (p *payloadFilteringIndex) Reset(context.Context, string) error      { return nil }
func (p *payloadFilteringIndex) Close() error                             { return nil }

func TestSearch_PushesFilterDownAndMaterialisesFromPayload(t *testing.T) {
	idx := newPayloadFilteringIndex()
	ctx := context.Background()
	_ = idx.Upsert(ctx, []float32{1, 0}, model.IndexPayload{ChunkID: 1, RelPath: "docs/a.md", DocType: "md", Snippet: "alpha"})
	_ = idx.Upsert(ctx, []float32{1, 0}, model.IndexPayload{ChunkID: 2, RelPath: "src/main.go", DocType: "code", Snippet: "beta"})

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)
	// Deliberately register NO chunk metadata — the backend payload is the only
	// source of rel_path/doc_type/snippet.

	hits, err := svc.Search(ctx, model.SearchQuery{
		Query:    "alpha",
		K:        5,
		DocTypes: []string{"md"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 md hit from push-down, got %d (%v)", len(hits), hits)
	}
	if hits[0].RelPath != "docs/a.md" || hits[0].Snippet != "alpha" {
		t.Fatalf("hit not materialised from payload: %#v", hits[0])
	}
	// The doctype predicate must have been pushed to the backend.
	if len(idx.lastFilter.DocTypes) != 1 || idx.lastFilter.DocTypes[0] != "md" {
		t.Fatalf("expected doctype filter pushed down, got %#v", idx.lastFilter)
	}
}

func TestSearch_PushDownExcludesNonMatchingPaths(t *testing.T) {
	idx := newPayloadFilteringIndex()
	ctx := context.Background()
	_ = idx.Upsert(ctx, []float32{1, 0}, model.IndexPayload{ChunkID: 1, RelPath: "docs/a.md", DocType: "md", Snippet: "a"})
	_ = idx.Upsert(ctx, []float32{1, 0}, model.IndexPayload{ChunkID: 2, RelPath: "other/b.md", DocType: "md", Snippet: "b"})

	svc := retrieval.NewService(nil, idx, &fakeRetrievalEmbedder{vectorsByModel: map[string][]float32{
		"mistral-embed": {1, 0},
	}}, nil)

	hits, err := svc.Search(ctx, model.SearchQuery{Query: "q", K: 5, PathPrefix: "docs/"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].RelPath != "docs/a.md" {
		t.Fatalf("expected only docs/a.md via push-down prefix, got %v", hits)
	}
}
