package tests

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// openRaw opens a second, independent connection to the same sqlite file so a
// test can inspect or perturb the database out-of-band from the store handle.
// foreign_keys is a per-connection pragma, so callers that need cascades on
// this connection must enable it themselves.
func openRaw(t *testing.T, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestSchemaVersionStamped pins that Init records a non-zero PRAGMA
// user_version into the database file header (#405). Before this, openDB set
// only busy_timeout+journal_mode and the schema carried no version floor, so a
// future non-additive migration had no tripwire.
func TestSchemaVersionStamped(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_ = st.Close()

	// user_version lives in the file header, so any connection reads it back.
	raw := openRaw(t, dbPath)
	var v int64
	if err := raw.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if v < 1 {
		t.Fatalf("expected user_version >= 1 after Init, got %d", v)
	}
}

// TestSchemaVersionDowngradeGuard pins the tripwire: a database stamped with a
// version newer than this binary understands must be refused rather than
// migrated in place (#405).
func TestSchemaVersionDowngradeGuard(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")

	// Initialize normally, then forge a far-future schema version.
	st := store.NewSQLiteStore(dbPath)
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	_ = st.Close()

	raw := openRaw(t, dbPath)
	if _, err := raw.ExecContext(ctx, `PRAGMA user_version = 99999`); err != nil {
		t.Fatalf("forge user_version: %v", err)
	}

	// A fresh store must refuse to open the future database.
	st2 := store.NewSQLiteStore(dbPath)
	defer func() { _ = st2.Close() }()
	if err := st2.Init(ctx); err == nil {
		t.Fatal("expected Init to reject a database with a newer schema version")
	}
}

// TestForeignKeyCascade pins that the declared ON DELETE CASCADE constraints
// actually fire once foreign_keys is enforced (#405): a hard delete of a
// document tears down its representations, chunks, and spans. The store never
// hard-deletes documents itself (it soft-deletes), so this exercises the
// constraint through a raw connection with foreign_keys ON — proving the
// schema's cascades are correctly declared rather than misleading.
func TestForeignKeyCascade(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	if err := st.UpsertDocument(ctx, model.Document{
		RelPath: "docs/case.md",
		DocType: "md",
		Status:  "ok",
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	doc, err := st.GetDocumentByPath(ctx, "docs/case.md")
	if err != nil {
		t.Fatalf("GetDocumentByPath: %v", err)
	}
	repID, err := st.UpsertRepresentation(ctx, model.Representation{
		DocID:   doc.DocID,
		RepType: "raw_text",
		RepHash: "hash-1",
	})
	if err != nil {
		t.Fatalf("UpsertRepresentation: %v", err)
	}
	if _, err := st.InsertChunkWithSpans(ctx, model.Chunk{
		RepID:   repID,
		Ordinal: 0,
		Text:    "the quick brown fox",
	}, []model.Span{{Kind: "lines", StartLine: 1, EndLine: 1}}); err != nil {
		t.Fatalf("InsertChunkWithSpans: %v", err)
	}
	_ = st.Close()

	// Hard-delete the document on a connection that enforces foreign keys.
	raw := openRaw(t, dbPath)
	if _, err := raw.ExecContext(ctx, `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatalf("enable foreign_keys: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `DELETE FROM documents WHERE doc_id = ?`, doc.DocID); err != nil {
		t.Fatalf("DELETE FROM documents: %v", err)
	}

	for _, tbl := range []struct {
		name  string
		query string
	}{
		{"representations", `SELECT COUNT(*) FROM representations`},
		{"chunks", `SELECT COUNT(*) FROM chunks`},
		{"spans", `SELECT COUNT(*) FROM spans`},
	} {
		var n int64
		if err := raw.QueryRowContext(ctx, tbl.query).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl.name, err)
		}
		if n != 0 {
			t.Errorf("cascade left %d orphaned %s rows, want 0", n, tbl.name)
		}
	}
}

// TestFTSPartialDriftRepaired pins that a PARTIALLY-populated chunks_fts (the
// crash-mid-rebuild / trigger-bypass case) is detected and rebuilt on the next
// Init (#405). The prior "rebuild only when fully empty" probe waved partial
// drift through, silently losing lexical recall for the missing chunks.
func TestFTSPartialDriftRepaired(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	chunks := []struct {
		label uint64
		text  string
	}{
		{1, "alpha reporting suspicious transactions"},
		{2, "bravo drug trafficking penalties criminal conduct"},
		{3, "charlie training qualifications reporting officer"},
	}
	for _, c := range chunks {
		task := model.NewChunkTask(c.label, c.text, "text", model.ChunkMetadata{
			ChunkID: c.label,
			RelPath: "docs/case.md",
			DocType: "md",
			RepType: "raw_text",
		})
		if err := st.UpsertChunkTask(ctx, task); err != nil {
			t.Fatalf("UpsertChunkTask(%d): %v", c.label, err)
		}
	}
	_ = st.Close()

	// Corrupt the FTS index: evict a single chunk's row via the external-content
	// 'delete' command, leaving chunks_fts partially populated (2 of 3 rows).
	raw := openRaw(t, dbPath)
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO chunks_fts(chunks_fts, rowid, text) VALUES('delete', ?, ?)`,
		int64(2), "bravo drug trafficking penalties criminal conduct",
	); err != nil {
		t.Fatalf("evict FTS row: %v", err)
	}
	// chunks_fts_docsize holds one row per actually-indexed document, so it
	// reflects the eviction (2 of 3) — unlike COUNT(*) FROM chunks_fts, which
	// proxies to the content table and would still read 3.
	var indexedCount int64
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks_fts_docsize`).Scan(&indexedCount); err != nil {
		t.Fatalf("count chunks_fts_docsize: %v", err)
	}
	if indexedCount != 2 {
		t.Fatalf("setup: expected 2 indexed FTS rows after eviction, got %d", indexedCount)
	}

	// Reopening the store must detect the count drift and rebuild.
	st2 := store.NewSQLiteStore(dbPath)
	defer func() { _ = st2.Close() }()
	if err := st2.Init(ctx); err != nil {
		t.Fatalf("re-Init: %v", err)
	}

	ls, ok := interface{}(st2).(model.LexicalSearcher)
	if !ok {
		t.Fatalf("SQLiteStore must implement model.LexicalSearcher")
	}
	hits, err := ls.SearchBM25(ctx, "trafficking", 5, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("evicted chunk not searchable after re-Init; partial FTS drift was not repaired")
	}
	if hits[0].ChunkID != 2 {
		t.Errorf("expected repaired chunk 2 as top hit, got %d", hits[0].ChunkID)
	}
}
