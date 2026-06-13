package tests

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
	"github.com/dirstral/dir2mcp/internal/model"
)

// upsertVec is a small helper that stores a vector keyed by id with a minimal
// payload (rel_path derived from id so the chunk is not treated as an orphan
// under a filter). It keeps the direct-index tests terse after the #247 API
// change from Add(label, vec) to Upsert(ctx, vec, payload).
func upsertVec(t *testing.T, idx *index.HNSWIndex, id uint64, vec []float32) {
	t.Helper()
	payload := model.IndexPayload{ChunkID: id, RelPath: "doc.txt", DocType: "md"}
	if err := idx.Upsert(context.Background(), vec, payload); err != nil {
		t.Fatalf("Upsert(%d) failed: %v", id, err)
	}
}

func TestHNSWIndex_UpsertAndSearch(t *testing.T) {
	idx := index.NewHNSWIndex("")
	upsertVec(t, idx, 1, []float32{1, 0})
	upsertVec(t, idx, 2, []float32{0.9, 0.1})
	upsertVec(t, idx, 3, []float32{0, 1})

	hits, err := idx.Search(context.Background(), []float32{1, 0}, 2, model.Filter{})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("unexpected result length: %d", len(hits))
	}
	if hits[0].ChunkID != 1 {
		t.Fatalf("expected top chunk 1, got %d", hits[0].ChunkID)
	}
	if hits[1].ChunkID != 2 {
		t.Fatalf("expected second chunk 2, got %d", hits[1].ChunkID)
	}
	// for cosine similarity higher score is better, so results should be
	// non‑increasing
	if hits[0].Score < hits[1].Score {
		t.Fatalf("expected scores[0] >= scores[1], got %v and %v", hits[0].Score, hits[1].Score)
	}
}

func TestHNSWIndex_DimensionMismatch(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// capture logs and provide metrics
	var buf bytes.Buffer
	idx.Logger = log.New(&buf, "", 0)
	idx.Metrics = &index.HNSWIndexMetrics{}

	upsertVec(t, idx, 1, []float32{1, 0})
	// add a vector with incorrect dimension
	upsertVec(t, idx, 2, []float32{1, 0, 0})

	hits, err := idx.Search(context.Background(), []float32{1, 0}, 10, model.Filter{})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 1 {
		t.Fatalf("unexpected hits after mismatch: %v", hits)
	}
	// read via Load to respect the atomic.Int64 API
	if idx.Metrics.DimensionMismatch.Load() != 1 {
		t.Fatalf("expected metric increment, got %d", idx.Metrics.DimensionMismatch.Load())
	}
	if !strings.Contains(buf.String(), "dimension mismatch") {
		t.Fatalf("expected log message, got %q", buf.String())
	}
}

func TestHNSWIndex_SaveAndLoad(t *testing.T) {
	tmp := t.TempDir()
	file := filepath.Join(tmp, "idx.bin")

	idx := index.NewHNSWIndex(file)
	upsertVec(t, idx, 7, []float32{0.1, 0.2, 0.3})
	if err := idx.Save(context.Background(), ""); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("expected saved file: %v", err)
	}

	loaded := index.NewHNSWIndex(file)
	if err := loaded.Load(context.Background(), ""); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	hits, err := loaded.Search(context.Background(), []float32{0.1, 0.2, 0.3}, 1, model.Filter{})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 7 {
		t.Fatalf("unexpected loaded search result: %#v", hits)
	}
	// the persisted payload round-trips alongside the vector.
	if hits[0].Payload.RelPath != "doc.txt" {
		t.Fatalf("expected payload to survive save/load, got %q", hits[0].Payload.RelPath)
	}
}

func TestHNSWIndex_SearchEmptyIndex(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// should not panic and should return empty slice
	hits, err := idx.Search(context.Background(), []float32{1, 0}, 1, model.Filter{})
	if err != nil {
		t.Fatalf("expected no error searching empty index, got %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected empty results from empty index, got %v", hits)
	}
}

func TestHNSWIndex_KGreaterThanItems(t *testing.T) {
	idx := index.NewHNSWIndex("")
	upsertVec(t, idx, 10, []float32{1, 0})
	upsertVec(t, idx, 20, []float32{0, 1})

	hits, err := idx.Search(context.Background(), []float32{1, 0}, 5, model.Filter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 results when k>items, got %d", len(hits))
	}
}

func TestHNSWIndex_UpsertDuplicateChunkIDs(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// upsert the same chunk_id twice with different vectors; the second upsert
	// should overwrite the first
	upsertVec(t, idx, 1, []float32{1, 0})
	upsertVec(t, idx, 1, []float32{0, 1})

	// search for a vector similar to the second upsert and ensure the returned
	// score reflects the overwritten vector
	hits, err := idx.Search(context.Background(), []float32{0, 1}, 1, model.Filter{})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit after duplicate upsert, got %d", len(hits))
	}
	if hits[0].ChunkID != 1 {
		t.Fatalf("expected chunk 1, got %d", hits[0].ChunkID)
	}
	if hits[0].Score < 0.9 { // cosine similarity for identical vectors should be 1
		t.Fatalf("expected high score after overwrite, got %v", hits[0].Score)
	}
}

func TestHNSWIndex_LoadNonExistentFile(t *testing.T) {
	idx := index.NewHNSWIndex("/nonexistent")
	if err := idx.Load(context.Background(), ""); err != nil {
		t.Fatalf("expected nil for nonexistent file, got %v", err)
	}
}
