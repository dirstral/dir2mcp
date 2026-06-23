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

// TestHNSWIndex_BackendRoundTrip is the regression test for issue #375: the
// memory backend is constructed exactly as production does (via NewBackend),
// vectors are saved, and a *fresh* index loaded from the SAME wired path must
// recover them. This pins three invariants that the bug violated:
//
//  1. NewBackend persists to the versioned vectors_<kind>.v2.hnsw snapshot, so
//     the save target, the temp file (<path>.tmp), and the loader's path all
//     agree — a divergent non-v2 tmp/target name (the reported failure) would
//     either leave the v2 file empty or fail the rename.
//  2. The save→reload cycle through that path preserves the vectors, proving
//     they actually persist across restarts (no forced re-embed).
//  3. The temp sidecar is renamed away, leaving only the durable snapshot.
func TestHNSWIndex_BackendRoundTrip(t *testing.T) {
	cases := []struct {
		kind     string
		wantName string
	}{
		{index.KindText, index.TextIndexFileName},
		{index.KindCode, index.CodeIndexFileName},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			stateDir := t.TempDir()

			ix, path := index.NewBackend(index.BackendMemory, stateDir, tc.kind)

			// The wired path must be the versioned snapshot the loader reads.
			if got := filepath.Base(path); got != tc.wantName {
				t.Fatalf("NewBackend path basename = %q, want %q", got, tc.wantName)
			}
			if filepath.Dir(path) != stateDir {
				t.Fatalf("NewBackend path dir = %q, want %q", filepath.Dir(path), stateDir)
			}

			hnsw, ok := ix.(*index.HNSWIndex)
			if !ok {
				t.Fatalf("memory backend should be *HNSWIndex, got %T", ix)
			}
			upsertVec(t, hnsw, 42, []float32{0.4, 0.5, 0.6})
			upsertVec(t, hnsw, 43, []float32{0.7, 0.8, 0.9})

			// Save via the model.Persistable contract using the wired path,
			// exactly as PersistenceManager.SaveAll does.
			p, ok := ix.(model.Persistable)
			if !ok {
				t.Fatalf("memory backend should be model.Persistable, got %T", ix)
			}
			if err := p.Save(context.Background(), path); err != nil {
				t.Fatalf("Save failed: %v", err)
			}

			// The durable snapshot must exist at the v2 path and the temp
			// sidecar must have been renamed away (no leftover .tmp).
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("expected snapshot at wired path %q: %v", path, err)
			}
			if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
				t.Fatalf("expected temp sidecar %q to be gone after rename, stat err=%v", path+".tmp", err)
			}

			// A fresh backend reading the SAME stateDir/kind must recover the
			// vectors — this is the cross-restart persistence the bug broke.
			reopened, reopenedPath := index.NewBackend(index.BackendMemory, stateDir, tc.kind)
			if reopenedPath != path {
				t.Fatalf("reopened path %q != original %q", reopenedPath, path)
			}
			rp := reopened.(model.Persistable)
			if err := rp.Load(context.Background(), reopenedPath); err != nil {
				t.Fatalf("Load failed: %v", err)
			}
			hits, err := reopened.Search(context.Background(), []float32{0.4, 0.5, 0.6}, 2, model.Filter{})
			if err != nil {
				t.Fatalf("Search failed: %v", err)
			}
			if len(hits) != 2 {
				t.Fatalf("expected 2 vectors to survive round-trip, got %d", len(hits))
			}
			if hits[0].ChunkID != 42 {
				t.Fatalf("expected nearest chunk 42, got %d", hits[0].ChunkID)
			}
		})
	}
}

// TestHNSWIndex_SaveCreatesMissingStateDir guards the precise #375 symptom: a
// save whose destination directory does not yet exist must create it rather
// than fail with "no such file or directory" on create/rename, then leave a
// loadable snapshot at the versioned path.
func TestHNSWIndex_SaveCreatesMissingStateDir(t *testing.T) {
	root := t.TempDir()
	// Nested, not-yet-created state dir, mirroring <root>/.dir2mcp.
	stateDir := filepath.Join(root, ".dir2mcp")
	path := filepath.Join(stateDir, index.TextIndexFileName)

	idx := index.NewHNSWIndex(path)
	upsertVec(t, idx, 5, []float32{1, 0, 0})

	if err := idx.Save(context.Background(), ""); err != nil {
		t.Fatalf("Save into missing state dir failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected snapshot created at %q: %v", path, err)
	}

	loaded := index.NewHNSWIndex(path)
	if err := loaded.Load(context.Background(), ""); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	hits, err := loaded.Search(context.Background(), []float32{1, 0, 0}, 1, model.Filter{})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(hits) != 1 || hits[0].ChunkID != 5 {
		t.Fatalf("unexpected round-trip result after creating state dir: %#v", hits)
	}
}
