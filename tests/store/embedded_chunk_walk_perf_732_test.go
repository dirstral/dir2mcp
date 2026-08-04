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

// TestEmbeddedChunkWalkPerf_732 measures the cost of a full keyset walk of
// ListEmbeddedChunkMetadata over a synthetic corpus, per index kind and per page
// size. It is a measurement harness, not an assertion: it is skipped unless
// DIR2MCP_PERF=1 so `go test ./...` stays fast, and it exists so the #732 fix can
// be defended with numbers instead of a plan reading.
//
// Corpus shape mirrors the #731 measurement: half the chunks are "text" with the
// LOW chunk_ids and half are "code" with the high ones, which is what makes the
// pre-fix query shape visibly quadratic (every text page still had to drag all
// 50k code rows through the CTE).
//
//	DIR2MCP_PERF=1 go test ./tests/store -run EmbeddedChunkWalkPerf -v
//	DIR2MCP_PERF=1 DIR2MCP_PERF_CHUNKS=200000 go test ./tests/store -run EmbeddedChunkWalkPerf -v
func TestEmbeddedChunkWalkPerf_732(t *testing.T) {
	if os.Getenv("DIR2MCP_PERF") != "1" {
		t.Skip("set DIR2MCP_PERF=1 to run the #732 walk measurement")
	}
	total := 100000
	if raw := os.Getenv("DIR2MCP_PERF_CHUNKS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("DIR2MCP_PERF_CHUNKS=%q: want a positive integer", raw)
		}
		total = n
	}

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "perf.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	seedSyntheticEmbeddedCorpus(t, dbPath, total)

	for _, pageSize := range []int{500, 5000} {
		for _, kind := range []string{"text", "code"} {
			start := time.Now()
			rows, pages := walkEmbedded(ctx, t, st, kind, pageSize)
			t.Logf("page=%-5d kind=%-4s rows=%-7d pages=%-4d elapsed=%v", pageSize, kind, rows, pages, time.Since(start))
		}
		start := time.Now()
		rows, pages := walkEmbedded(ctx, t, st, "", pageSize)
		t.Logf("page=%-5d kind=%-4s rows=%-7d pages=%-4d elapsed=%v", pageSize, "all", rows, pages, time.Since(start))
	}
}

// walkEmbedded runs one full keyset walk and returns the rows and pages read.
func walkEmbedded(ctx context.Context, t *testing.T, st *store.SQLiteStore, kind string, pageSize int) (rows, pages int) {
	t.Helper()
	var afterChunkID int64
	for {
		page, err := st.ListEmbeddedChunkMetadata(ctx, kind, pageSize, afterChunkID)
		if err != nil {
			t.Fatalf("ListEmbeddedChunkMetadata(kind=%q, after=%d): %v", kind, afterChunkID, err)
		}
		rows += len(page)
		pages++
		if len(page) < pageSize {
			return rows, pages
		}
		afterChunkID = int64(page[len(page)-1].Metadata.ChunkID)
	}
}

// seedSyntheticEmbeddedCorpus bulk-inserts total embedded chunks (plus their
// documents, representations and one span each) straight into the schema the
// store just created. It writes on its own connection because the goal is a large
// corpus in seconds, not exercising the store's write path.
func seedSyntheticEmbeddedCorpus(t *testing.T, dbPath string, total int) {
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
	                              VALUES (?, ?, ?, ?, 'md', 'raw_text', ?, ?, 'text', 'en', 'ok', 0)`)
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
		// chunk_ids 1..half are "text", the rest "code": the kind split follows the
		// key order, which is the shape the pre-fix CTE punished.
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
	// Deliberately no ANALYZE: production never runs it, so sqlite_stat1 is absent
	// there and the planner works from its default heuristics. Collecting stats
	// here would measure a planner the daemon never has.
	t.Logf("seeded %d embedded chunks in %v", total, time.Since(start))
}
