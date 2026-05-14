package tests

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dirstral/dir2mcp/internal/index"
)

func TestHNSWIndex_AddAndSearch(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := idx.Add(2, []float32{0.9, 0.1}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := idx.Add(3, []float32{0, 1}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	labels, scores, err := idx.Search([]float32{1, 0}, 2)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	// check we got the right number of results
	if len(labels) != 2 || len(scores) != 2 {
		t.Fatalf("unexpected result lengths: labels=%d scores=%d", len(labels), len(scores))
	}

	// verify both returned labels are correct and in expected order
	if labels[0] != 1 {
		t.Fatalf("expected top label 1, got %d", labels[0])
	}
	if labels[1] != 2 {
		t.Fatalf("expected second label 2, got %d", labels[1])
	}

	// for cosine similarity higher score is better, so results should be
	// non‑increasing
	if scores[0] < scores[1] {
		t.Fatalf("expected scores[0] >= scores[1], got %v and %v", scores[0], scores[1])
	}
}

func TestHNSWIndex_DimensionMismatch(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// capture logs and provide metrics
	var buf bytes.Buffer
	idx.Logger = log.New(&buf, "", 0)
	idx.Metrics = &index.HNSWIndexMetrics{}

	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	// add a vector with incorrect dimension
	if err := idx.Add(2, []float32{1, 0, 0}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	labels, _, err := idx.Search([]float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(labels) != 1 || labels[0] != 1 {
		t.Fatalf("unexpected labels after mismatch: %v", labels)
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
	if err := idx.Add(7, []float32{0.1, 0.2, 0.3}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := idx.Save(""); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("expected saved file: %v", err)
	}

	loaded := index.NewHNSWIndex(file)
	if err := loaded.Load(""); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	labels, _, err := loaded.Search([]float32{0.1, 0.2, 0.3}, 1)
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(labels) != 1 || labels[0] != 7 {
		t.Fatalf("unexpected loaded search result: %#v", labels)
	}
}

func TestHNSWIndex_SearchEmptyIndex(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// should not panic and should return empty slices
	labels, scores, err := idx.Search([]float32{1, 0}, 1)
	if err != nil {
		t.Fatalf("expected no error searching empty index, got %v", err)
	}
	if len(labels) != 0 || len(scores) != 0 {
		t.Fatalf("expected empty results from empty index, got labels=%v scores=%v", labels, scores)
	}
}

func TestHNSWIndex_KGreaterThanItems(t *testing.T) {
	idx := index.NewHNSWIndex("")
	if err := idx.Add(10, []float32{1, 0}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	if err := idx.Add(20, []float32{0, 1}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	labels, scores, err := idx.Search([]float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(labels) != 2 || len(scores) != 2 {
		t.Fatalf("expected 2 results when k>items, got %d", len(labels))
	}
}

func TestHNSWIndex_AddDuplicateLabels(t *testing.T) {
	idx := index.NewHNSWIndex("")
	// add the same label twice with different vectors; second add should
	// overwrite the first
	if err := idx.Add(1, []float32{1, 0}); err != nil {
		t.Fatalf("add failed: %v", err)
	}
	if err := idx.Add(1, []float32{0, 1}); err != nil {
		t.Fatalf("second add failed: %v", err)
	}

	// search for a vector similar to the second addition and ensure the
	// returned score reflects the overwritten vector
	labels, scores, err := idx.Search([]float32{0, 1}, 1)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(labels) != 1 {
		t.Fatalf("expected 1 label after duplicate add, got %d", len(labels))
	}
	if labels[0] != 1 {
		t.Fatalf("expected label 1, got %d", labels[0])
	}
	if scores[0] < 0.9 { // cosine similarity for identical vectors should be 1
		t.Fatalf("expected high score after overwrite, got %v", scores[0])
	}
}

func TestHNSWIndex_LoadNonExistentFile(t *testing.T) {
	idx := index.NewHNSWIndex("/nonexistent")
	err := idx.Load("")
	if err != nil {
		t.Fatalf("expected nil for nonexistent file, got %v", err)
	}
}
