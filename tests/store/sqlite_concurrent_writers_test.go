package tests

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestSQLiteStore_ConcurrentWritersNoBusy guards against the SQLITE_BUSY
// regression observed during fresh indexing of moderately-sized corpora. Two
// kinds of writers run in parallel against the same store:
//   - chunk writers (UpsertChunkTask) — simulating the ingest goroutine
//   - embedding markers (MarkEmbedded) — simulating the embedding worker
//
// Without a single-writer connection or a busy_timeout, the in-process
// contention between these two paths used to produce SQLITE_BUSY (5) errors
// that were retried up to 3 times. The fix in initLocked sets
// SetMaxOpenConns(1) and a busy_timeout pragma so this test must complete with
// zero errors from any writer goroutine.
func TestSQLiteStore_ConcurrentWritersNoBusy(t *testing.T) {
	const (
		chunkWriters     = 8
		writesPerWriter  = 200
		embeddingMarkers = 4
		marksPerMarker   = 50
	)

	// Bound the test so a regression that re-introduces lock contention or
	// blocking causes a fast, descriptive failure rather than an unbounded
	// CI hang. 30s is generous for the workload below on cold cache. The bound
	// guards against a genuine hang, not throughput: this test asserts the
	// absence of SQLITE_BUSY under contention, so the wall-clock ceiling is
	// incidental to what it checks. raceScaled expands the deadline under
	// `-race` (5-20x slowdown), where full-suite contention on a shared CI
	// runner would otherwise trip the 30s ceiling and surface as a spurious
	// "context deadline exceeded" — a timeout, not a busy error (issue #614).
	ctx, cancel := context.WithTimeout(context.Background(), raceScaled(30*time.Second))
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()

	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var (
		wg       sync.WaitGroup
		busyHits int64
		anyErr   atomic.Value
	)

	recordErr := func(stage string, err error) {
		if err == nil {
			return
		}
		// Track BUSY specifically so a regression fails with a clear signal.
		if strings.Contains(strings.ToLower(err.Error()), "busy") {
			atomic.AddInt64(&busyHits, 1)
		}
		anyErr.Store(stage + ": " + err.Error())
	}

	// Chunk writers
	for w := 0; w < chunkWriters; w++ {
		writerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < writesPerWriter; i++ {
				label := uint64(writerID*writesPerWriter + i + 1)
				task := model.NewChunkTask(label, "concurrent text", "text", model.ChunkMetadata{
					ChunkID: label,
					RelPath: "docs/concurrent.md",
					DocType: "md",
					RepType: "raw_text",
				})
				if err := st.UpsertChunkTask(ctx, task); err != nil {
					recordErr("UpsertChunkTask", err)
					return
				}
			}
		}()
	}

	// Embedding markers — try to mark labels as embedded as soon as they may
	// have been written. These calls run concurrently with the writers above
	// to maximize lock contention on the underlying database.
	for m := 0; m < embeddingMarkers; m++ {
		markerID := m
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < marksPerMarker; i++ {
				labels := []uint64{uint64(markerID*marksPerMarker + i + 1)}
				if err := st.MarkEmbedded(ctx, labels); err != nil {
					recordErr("MarkEmbedded", err)
					return
				}
			}
		}()
	}

	wg.Wait()

	if hits := atomic.LoadInt64(&busyHits); hits > 0 {
		t.Fatalf("SQLITE_BUSY regression: %d BUSY error(s) under concurrent writers", hits)
	}
	if err := anyErr.Load(); err != nil {
		t.Fatalf("unexpected writer error: %v", err)
	}
}
