package tests

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dirstral/dir2mcp/internal/config"
	"github.com/dirstral/dir2mcp/internal/mcp"
	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/protocol"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestListFilesPagePerf_694 measures what a list_files caller actually pays for
// ONE page, before and after #694, over the same synthetic corpus in the same
// process. It is a measurement harness, not an assertion, and is skipped unless
// DIR2MCP_PERF=1 so `go test ./...` stays fast.
//
// The "before" arm is not a reimplementation: the handler keeps the pre-#694
// walk as its fallback for stores that cannot push the hidden-path predicate
// into their query, so hiding ListVisibleFiles behind a plain model.Store shim
// routes the real handler down the real old code path. The two arms therefore
// differ in exactly the thing the issue is about.
//
// Three numbers are logged per arm:
//   - elapsed: wall time for one tools/call round trip.
//   - reads:   how many times the handler asked the store for rows. The old
//     walk needs ceil(total/500); the fix needs 1.
//   - rows:    how many document rows the store handed back, which is also the
//     number of rows the handler ran filepath.EvalSymlinks over. This is the
//     syscall count, exactly, and it is the dominant cost on a network
//     filesystem.
//
// The corpus is 10% hidden (dot-prefixed first segment) so the pushed-down
// predicate is doing real work rather than matching everything.
//
//	DIR2MCP_PERF=1 go test ./tests/mcp -run ListFilesPagePerf -v
//	DIR2MCP_PERF=1 DIR2MCP_PERF_FILES=50000 go test ./tests/mcp -run ListFilesPagePerf -v
func TestListFilesPagePerf_694(t *testing.T) {
	if os.Getenv("DIR2MCP_PERF") != "1" {
		t.Skip("set DIR2MCP_PERF=1 to run the #694 page-cost measurement")
	}
	total := 20000
	if raw := os.Getenv("DIR2MCP_PERF_FILES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("DIR2MCP_PERF_FILES=%q: want a positive integer", raw)
		}
		total = n
	}
	walkPages := 10
	if raw := os.Getenv("DIR2MCP_PERF_WALK_PAGES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			t.Fatalf("DIR2MCP_PERF_WALK_PAGES=%q: want a non-negative integer", raw)
		}
		walkPages = n
	}

	ctx := context.Background()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "meta.sqlite")
	root := filepath.Join(tmp, "corpus")
	st := store.NewSQLiteStore(dbPath)
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedPerfCorpus694(t, dbPath, root, total)

	counting := &perfStore694{SQLiteStore: st}
	const pageSize = 50

	for _, arm := range []struct {
		label string
		store model.Store
	}{
		// walkOnly694 hides ListVisibleFiles, so the handler falls back to the
		// pre-#694 corpus walk. Same handler, same store, old path.
		{label: "before", store: walkOnly694{counting}},
		{label: "after", store: counting},
	} {
		url, sessionID := perfSession694(t, tmp, root, arm.store)

		// One warm-up call: the first tools/call of a session also pays for
		// SQLite's page cache filling, which would otherwise be charged to the
		// first measurement and to the "before" arm only.
		visible := perfListFiles694(t, url, sessionID, pageSize, 0).Total
		deepOffset := int(visible) - 2*pageSize
		if deepOffset < 0 {
			deepOffset = 0
		}

		for _, offset := range []int{0, deepOffset} {
			counting.reset()
			start := time.Now()
			page := perfListFiles694(t, url, sessionID, pageSize, offset)
			elapsed := time.Since(start)
			t.Logf("%-6s offset=%-6d files=%-3d total=%-6d reads=%-4d rows=%-7d elapsed=%v",
				arm.label, offset, len(page.Files), page.Total,
				counting.calls.Load(), counting.rows.Load(), elapsed)
		}

		// The quadratic part: a client paging through the listing pays the
		// per-page cost again for every page.
		if walkPages > 0 {
			counting.reset()
			start := time.Now()
			for p := 0; p < walkPages; p++ {
				perfListFiles694(t, url, sessionID, pageSize, p*pageSize)
			}
			t.Logf("%-6s walk pages=%-4d reads=%-5d rows=%-8d elapsed=%v",
				arm.label, walkPages, counting.calls.Load(), counting.rows.Load(), time.Since(start))
		}
	}
}

// perfStore694 counts store reads and the rows they yield. Both listing methods
// are overridden: embedding *store.SQLiteStore would otherwise satisfy
// ListVisibleFiles silently and the "after" arm would count nothing.
type perfStore694 struct {
	*store.SQLiteStore
	calls atomic.Int64
	rows  atomic.Int64
}

func (p *perfStore694) reset() {
	p.calls.Store(0)
	p.rows.Store(0)
}

func (p *perfStore694) ListFiles(ctx context.Context, prefix, glob string, limit, offset int) ([]model.Document, int64, error) {
	p.calls.Add(1)
	docs, total, err := p.SQLiteStore.ListFiles(ctx, prefix, glob, limit, offset)
	p.rows.Add(int64(len(docs)))
	return docs, total, err
}

func (p *perfStore694) ListVisibleFiles(ctx context.Context, prefix, glob string, limit, offset int, includeHidden bool) ([]model.Document, int64, error) {
	p.calls.Add(1)
	docs, total, err := p.SQLiteStore.ListVisibleFiles(ctx, prefix, glob, limit, offset, includeHidden)
	p.rows.Add(int64(len(docs)))
	return docs, total, err
}

// walkOnly694 narrows a store to exactly model.Store, hiding the optional
// ListVisibleFiles capability so the handler takes its pre-#694 fallback.
type walkOnly694 struct{ model.Store }

func perfSession694(t *testing.T, stateDir, root string, st model.Store) (string, string) {
	t.Helper()
	cfg := config.Default()
	cfg.RootDir = root
	cfg.StateDir = stateDir
	cfg.MCPPath = protocol.DefaultMCPPath
	cfg.AuthMode = "none"

	server := httptest.NewServer(mcp.NewServer(cfg, nil, mcp.WithStore(st)).Handler())
	t.Cleanup(server.Close)
	url := server.URL + cfg.MCPPath
	return url, initializeSession(t, url)
}

func perfListFiles694(t *testing.T, url, sessionID string, limit, offset int) listFilesPageResult694 {
	t.Helper()
	body := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"dir2mcp_list_files","arguments":{"limit":%d,"offset":%d}}}`,
		limit, offset)
	resp := postRPC(t, url, sessionID, body)
	defer func() { _ = resp.Body.Close() }()
	return decodeListFilesEnvelope694(t, resp.Body)
}

// seedPerfCorpus694 writes a real file per document and bulk-inserts the rows on
// its own connection. Real files are mandatory: the handler drops rows that do
// not resolve under the root, and a store-only corpus would measure a listing
// that returns nothing. Rows go in directly rather than through UpsertDocument
// because the goal is a large corpus in seconds, not exercising the write path.
func seedPerfCorpus694(t *testing.T, dbPath, root string, total int) {
	t.Helper()
	start := time.Now()

	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)")
	if err != nil {
		t.Fatalf("open seed handle: %v", err)
	}
	defer func() { _ = db.Close() }()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin seed tx: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO documents (rel_path, doc_type, source_type, title, size_bytes, mtime_unix, status, deleted)
	                         VALUES (?, 'md', 'local', '', 1, 1700000000, 'ok', 0)`)
	if err != nil {
		t.Fatalf("prepare document insert: %v", err)
	}

	const perDir = 500
	body := []byte("x")
	for i := 0; i < total; i++ {
		// Every tenth document is hidden, so the pushed-down predicate has a
		// real result set to shrink rather than a no-op.
		prefix := "corpus"
		if i%10 == 0 {
			prefix = ".cache"
		}
		relPath := fmt.Sprintf("%s/d%04d/f%07d.md", prefix, i/perDir, i)
		full := filepath.Join(root, filepath.FromSlash(relPath))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", relPath, err)
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", relPath, err)
		}
		if _, err := stmt.Exec(relPath); err != nil {
			t.Fatalf("insert %s: %v", relPath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit seed tx: %v", err)
	}
	t.Logf("seeded %d documents (files + rows) in %v", total, time.Since(start))
}
