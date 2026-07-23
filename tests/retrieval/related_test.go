package tests

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/retrieval"
)

// relatedFakeStore is a minimal model.Store that also satisfies the
// relatedChunkStore capability, resolving seed segments from an in-memory map.
type relatedFakeStore struct {
	chunks map[uint64]model.ChunkTask
}

func (f *relatedFakeStore) Init(context.Context) error { return nil }
func (f *relatedFakeStore) UpsertDocument(context.Context, model.Document) error {
	return nil
}
func (f *relatedFakeStore) GetDocumentByPath(context.Context, string) (model.Document, error) {
	return model.Document{}, model.ErrNotFound
}
func (f *relatedFakeStore) ListFiles(context.Context, string, string, int, int) ([]model.Document, int64, error) {
	return nil, 0, nil
}
func (f *relatedFakeStore) Close() error { return nil }

func (f *relatedFakeStore) ChunkTaskByID(_ context.Context, chunkID uint64) (model.ChunkTask, string, error) {
	t, ok := f.chunks[chunkID]
	if !ok {
		return model.ChunkTask{}, "", model.ErrNotFound
	}
	return t, "", nil
}

func (f *relatedFakeStore) EmbeddedChunksByPath(_ context.Context, relPath string) ([]model.ChunkTask, error) {
	ids := make([]uint64, 0, len(f.chunks))
	for id := range f.chunks {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []model.ChunkTask
	for _, id := range ids {
		if f.chunks[id].Metadata.RelPath == relPath {
			out = append(out, f.chunks[id])
		}
	}
	return out, nil
}

// textVecEmbedder maps an exact input text to a fixed vector, so re-embedding a
// stored chunk's text reproduces its indexed vector deterministically.
type textVecEmbedder struct {
	vecByText map[string][]float32
}

func (e *textVecEmbedder) Embed(_ context.Context, _ string, _ model.EmbedRole, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, ok := e.vecByText[t]
		if !ok {
			return nil, fmt.Errorf("textVecEmbedder: no vector for %q", t)
		}
		out[i] = v
	}
	return out, nil
}

func relatedChunk(id uint64, relPath, text string) model.ChunkTask {
	return model.NewChunkTask(id, text, "text", model.ChunkMetadata{
		ChunkID: id,
		RelPath: relPath,
		DocType: "txt",
		RepType: "raw_text",
		Snippet: text,
		Span:    model.Span{Kind: "lines", StartLine: 1, EndLine: 1},
	})
}

// newRelatedService wires an HNSW index + fake store + text->vector embedder over
// a fixed 4-chunk corpus and returns the service ready for Related calls.
func newRelatedService(t *testing.T) *retrieval.Service {
	t.Helper()
	chunks := map[uint64]model.ChunkTask{
		1: relatedChunk(1, "docs/a.txt", "alpha"),
		2: relatedChunk(2, "docs/a.txt", "alpha-two"),
		3: relatedChunk(3, "docs/b.txt", "beta"),
		4: relatedChunk(4, "other/c.txt", "gamma"),
	}
	vecByText := map[string][]float32{
		"alpha":     {1, 0},
		"alpha-two": {0.94, 0.34}, // ~0.94 cosine to alpha, same document
		"beta":      {0.8, 0.6},   // 0.80 cosine to alpha
		"gamma":     {0, 1},       // 0 cosine to alpha
	}
	idx := index.NewHNSWIndex("")
	for id, task := range chunks {
		addVecP(t, idx, id, vecByText[task.Text], task.Metadata.RelPath, task.Metadata.DocType)
	}
	svc := retrieval.NewService(&relatedFakeStore{chunks: chunks}, idx, &textVecEmbedder{vecByText: vecByText}, nil)
	for id, task := range chunks {
		svc.SetChunkMetadata(id, task.Metadata.ToSearchHit())
	}
	return svc
}

func hitIDs(hits []model.SearchHit) []uint64 {
	ids := make([]uint64, len(hits))
	for i, h := range hits {
		ids[i] = h.ChunkID
	}
	return ids
}

func TestRelated_ChunkID_ExcludesSameDocument(t *testing.T) {
	svc := newRelatedService(t)
	res, err := svc.Related(context.Background(), model.RelatedQuery{
		SourceChunkID:       1,
		K:                   10,
		ExcludeSameDocument: true,
	})
	if err != nil {
		t.Fatalf("Related: %v", err)
	}
	if !res.HasSourceChunkID || res.SourceChunkID != 1 {
		t.Fatalf("source_chunk_id echo = (%v,%d), want (true,1)", res.HasSourceChunkID, res.SourceChunkID)
	}
	if res.SourceRelPath != "docs/a.txt" {
		t.Fatalf("source_rel_path = %q, want docs/a.txt", res.SourceRelPath)
	}
	if res.IndexUsed != "text" {
		t.Fatalf("index_used = %q, want text", res.IndexUsed)
	}
	ids := hitIDs(res.Hits)
	// The source chunk (1) and its same-document sibling (2) are both excluded.
	if want := []uint64{3, 4}; fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("hits = %v, want %v (source + same-document excluded)", ids, want)
	}
}

func TestRelated_ChunkID_KeepsSameDocumentWhenNotExcluded(t *testing.T) {
	svc := newRelatedService(t)
	res, err := svc.Related(context.Background(), model.RelatedQuery{
		SourceChunkID:       1,
		K:                   10,
		ExcludeSameDocument: false,
	})
	if err != nil {
		t.Fatalf("Related: %v", err)
	}
	ids := hitIDs(res.Hits)
	if containsID(ids, 1) {
		t.Fatalf("hits = %v, must never contain the source chunk itself", ids)
	}
	if !containsID(ids, 2) {
		t.Fatalf("hits = %v, expected same-document sibling (2) when exclude_same_document=false", ids)
	}
	// Highest-similarity survivor is the same-document sibling.
	if ids[0] != 2 {
		t.Fatalf("hits[0] = %d, want 2 (nearest neighbour)", ids[0])
	}
}

func TestRelated_RelPath_ExcludesWholeDocument(t *testing.T) {
	svc := newRelatedService(t)
	res, err := svc.Related(context.Background(), model.RelatedQuery{
		SourceRelPath: "docs/a.txt",
		K:             10,
		// exclude_same_document is a no-op for a rel_path request even when false.
		ExcludeSameDocument: false,
	})
	if err != nil {
		t.Fatalf("Related: %v", err)
	}
	if res.HasSourceChunkID {
		t.Fatalf("source_chunk_id must be omitted for a rel_path request")
	}
	if res.SourceRelPath != "docs/a.txt" {
		t.Fatalf("source_rel_path = %q, want docs/a.txt", res.SourceRelPath)
	}
	ids := hitIDs(res.Hits)
	if containsID(ids, 1) || containsID(ids, 2) {
		t.Fatalf("hits = %v, a document is never related to itself (chunks 1,2 must be excluded)", ids)
	}
	if want := []uint64{3, 4}; fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("hits = %v, want %v", ids, want)
	}
}

func TestRelated_FilterPassthrough(t *testing.T) {
	svc := newRelatedService(t)
	res, err := svc.Related(context.Background(), model.RelatedQuery{
		SourceChunkID:       1,
		K:                   10,
		ExcludeSameDocument: true,
		PathPrefix:          "other",
	})
	if err != nil {
		t.Fatalf("Related: %v", err)
	}
	ids := hitIDs(res.Hits)
	if want := []uint64{4}; fmt.Sprint(ids) != fmt.Sprint(want) {
		t.Fatalf("hits = %v, want %v (path_prefix=other keeps only other/c.txt)", ids, want)
	}
}

func TestRelated_UnknownChunkIDIsSourceNotFound(t *testing.T) {
	svc := newRelatedService(t)
	_, err := svc.Related(context.Background(), model.RelatedQuery{SourceChunkID: 999, K: 5})
	if err == nil {
		t.Fatal("expected error for unknown chunk_id")
	}
	if err != model.ErrRelatedSourceNotFound {
		t.Fatalf("err = %v, want ErrRelatedSourceNotFound", err)
	}
}

func TestRelated_UnknownRelPathIsSourceNotFound(t *testing.T) {
	svc := newRelatedService(t)
	_, err := svc.Related(context.Background(), model.RelatedQuery{SourceRelPath: "nope/missing.txt", K: 5})
	if err != model.ErrRelatedSourceNotFound {
		t.Fatalf("err = %v, want ErrRelatedSourceNotFound", err)
	}
}

func TestRelated_NotSupportedStore(t *testing.T) {
	// A store that does not implement relatedChunkStore degrades to not-supported.
	idx := index.NewHNSWIndex("")
	svc := retrieval.NewService(nil, idx, &textVecEmbedder{vecByText: map[string][]float32{}}, nil)
	_, err := svc.Related(context.Background(), model.RelatedQuery{SourceChunkID: 1, K: 5})
	if err != model.ErrRelatedNotSupported {
		t.Fatalf("err = %v, want ErrRelatedNotSupported", err)
	}
}
