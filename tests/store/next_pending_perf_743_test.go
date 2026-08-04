package tests

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dirstral/dir2mcp/internal/store"
)

// TestNextPendingDrainPerf_743 measures what an ingest actually pays: the cost
// of NextPending per embed cycle as a deep queue drains, not the cost of one
// call. It is a measurement harness, not an assertion, and is skipped unless
// DIR2MCP_PERF=1 so `go test ./...` stays fast.
//
// The distinction matters for reading the result. #732's walk was quadratic
// because a keyset cursor compounded the repeated full materialization.
// NextPending has no cursor, so the pre-fix cost is flat-but-wrong: every cycle
// re-materialized the whole remaining pending set. That shows up here as a
// per-cycle time that tracks the DEPTH OF THE QUEUE rather than the batch size,
// and it should fall away as the queue drains. After the fix a cycle touches a
// bounded number of rows, so per-cycle time should be flat and small from the
// first cycle to the last.
//
// The kind split follows key order (low chunk_ids are "text", high ones
// "code"), which is the shape that punished the pre-fix CTE hardest: a "code"
// batch had to drag every pending "text" row through the CTE before the outer
// filter discarded them.
//
//	DIR2MCP_PERF=1 go test ./tests/store -run NextPendingDrainPerf -v
//	DIR2MCP_PERF=1 DIR2MCP_PERF_CHUNKS=200000 go test ./tests/store -run NextPendingDrainPerf -v
func TestNextPendingDrainPerf_743(t *testing.T) {
	if os.Getenv("DIR2MCP_PERF") != "1" {
		t.Skip("set DIR2MCP_PERF=1 to run the #743 drain measurement")
	}
	total := 100000
	if raw := os.Getenv("DIR2MCP_PERF_CHUNKS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("DIR2MCP_PERF_CHUNKS=%q: want a positive integer", raw)
		}
		total = n
	}

	for _, kind := range []string{"code", "text", ""} {
		label := kind
		if label == "" {
			label = "all"
		}
		ctx := context.Background()
		dbPath := filepath.Join(t.TempDir(), "perf.sqlite")
		st := store.NewSQLiteStore(dbPath)
		if err := st.Init(ctx); err != nil {
			t.Fatalf("Init: %v", err)
		}
		seedSyntheticPendingCorpus(t, dbPath, total)

		// A handful of cycles is enough to show the shape: the interesting
		// comparison is the FIRST cycle (deepest queue) against a later one.
		const batchSize = 32
		const cycles = 20
		var first, last time.Duration
		start := time.Now()
		for cycle := 0; cycle < cycles; cycle++ {
			cycleStart := time.Now()
			batch, err := st.NextPending(ctx, batchSize, kind)
			if err != nil {
				t.Fatalf("kind=%s cycle=%d: NextPending: %v", label, cycle, err)
			}
			elapsed := time.Since(cycleStart)
			if cycle == 0 {
				first = elapsed
			}
			last = elapsed
			if len(batch) == 0 {
				break
			}
			labels := make([]uint64, 0, len(batch))
			for _, task := range batch {
				labels = append(labels, task.Metadata.ChunkID)
			}
			if err := st.MarkEmbedded(ctx, labels); err != nil {
				t.Fatalf("kind=%s cycle=%d: MarkEmbedded: %v", label, cycle, err)
			}
		}
		t.Logf("kind=%-4s batch=%d cycles=%d first=%v last=%v total=%v",
			label, batchSize, cycles, first, last, time.Since(start))
		_ = st.Close()
	}
}

// seedSyntheticPendingCorpus bulk-inserts total PENDING chunks (plus their
// documents, representations and one span each) straight into the schema the
// store just created. It writes on its own connection because the goal is a
// large queue in seconds, not exercising the store's write path.
func seedSyntheticPendingCorpus(t *testing.T, dbPath string, total int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)")
	if err != nil {
		t.Fatalf("open seed handle: %v", err)
	}
	defer func() { _ = db.Close() }()

	const chunksPerDoc = 100
	body := strings.Repeat("lorem ipsum dolor sit amet ", 15)
	start := time.Now()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	docStmt, err := tx.Prepare(`INSERT INTO documents (rel_path, doc_type, source_type, title, mtime_unix, status, deleted)
	                            VALUES (?, 'md', 'local', ?, 1700000000, 'ok', 0)`)
	if err != nil {
		t.Fatalf("prepare document insert: %v", err)
	}
	repStmt, err := tx.Prepare(`INSERT INTO representations (rep_id, doc_id, rep_type, rep_hash, created_unix, deleted)
	                            VALUES (?, ?, 'raw_text', 'h', 1700000000, 0)`)
	if err != nil {
		t.Fatalf("prepare representation insert: %v", err)
	}
	chunkStmt, err := tx.Prepare(`INSERT INTO chunks (chunk_id, rep_id, ordinal, rel_path, doc_type, rep_type, text, index_kind, modality, language, embedding_status, deleted)
	                              VALUES (?, ?, ?, ?, 'md', 'raw_text', ?, ?, 'text', 'en', 'pending', 0)`)
	if err != nil {
		t.Fatalf("prepare chunk insert: %v", err)
	}
	spanStmt, err := tx.Prepare(`INSERT INTO spans (chunk_id, span_kind, start, end, extra_json) VALUES (?, 'lines', ?, ?, '')`)
	if err != nil {
		t.Fatalf("prepare span insert: %v", err)
	}

	half := total / 2
	docID := 0
	for i := 1; i <= total; i++ {
		kind := "text"
		if i > half {
			kind = "code"
		}
		if (i-1)%chunksPerDoc == 0 {
			docID++
			relPath := fmt.Sprintf("corpus/%s/doc-%05d.md", kind, docID)
			if _, err := docStmt.Exec(relPath, "Doc "+strconv.Itoa(docID)); err != nil {
				t.Fatalf("insert document %d: %v", docID, err)
			}
			if _, err := repStmt.Exec(docID, docID); err != nil {
				t.Fatalf("insert representation %d: %v", docID, err)
			}
		}
		relPath := fmt.Sprintf("corpus/%s/doc-%05d.md", kind, docID)
		if _, err := chunkStmt.Exec(i, docID, (i-1)%chunksPerDoc, relPath, body, kind); err != nil {
			t.Fatalf("insert chunk %d: %v", i, err)
		}
		if _, err := spanStmt.Exec(i, i, i+5); err != nil {
			t.Fatalf("insert span %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	// Deliberately no ANALYZE, for the reason #732's harness gives: production
	// never runs it, so measuring with sqlite_stat1 present would measure a
	// planner the daemon never has.
	t.Logf("seeded %d pending chunks in %v", total, time.Since(start))
}
