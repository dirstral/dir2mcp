package tests

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	"github.com/dirstral/dir2mcp/internal/model"
	"github.com/dirstral/dir2mcp/internal/store"
)

// TestSQLiteStore_SearchBM25_BasicAndBackfill verifies the FTS5-backed BM25
// search method (added in #158) returns hits for matching queries. The same
// test exercises the chunks_fts auto-populate path: the FTS table is filled
// via the AFTER INSERT trigger on chunks, so a SearchBM25 call right after a
// few inserts must already see them.
func TestSQLiteStore_SearchBM25_BasicAndBackfill(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	chunks := []struct {
		label uint64
		text  string
	}{
		{1, "the quick brown fox jumps over the lazy dog"},
		{2, "section 35 reporting suspicious transactions financial institution"},
		{3, "discussion of training, qualifications, and reporting officer roles"},
		{4, "drug trafficking penalties under criminal conduct act"},
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

	ls, ok := interface{}(st).(model.LexicalSearcher)
	if !ok {
		t.Fatalf("SQLiteStore must implement model.LexicalSearcher")
	}

	hits, err := ls.SearchBM25(ctx, "section 35 suspicious", 5, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) == 0 {
		t.Fatalf("expected at least one BM25 hit for an exact-term query")
	}
	if hits[0].ChunkID != 2 {
		t.Errorf("expected chunk 2 to be top BM25 hit; got chunk %d (snippet=%q)", hits[0].ChunkID, hits[0].Snippet)
	}

	// Empty query returns no hits, no error.
	hits, err = ls.SearchBM25(ctx, "   ", 5, "text")
	if err != nil {
		t.Fatalf("SearchBM25(empty): %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("empty query should return no hits, got %d", len(hits))
	}

	// indexKind filter excludes mismatched chunks.
	hits, err = ls.SearchBM25(ctx, "fox", 5, "code")
	if err != nil {
		t.Fatalf("SearchBM25(code-only): %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("index_kind filter should have excluded text chunks, got %d hits", len(hits))
	}
}

// TestSQLiteStore_SearchBM25_PopulatesTitle verifies BM25 hits carry the
// document title (added in #166) via the LEFT JOIN in SearchBM25. Hits for a
// document without a title still return successfully with an empty Title.
func TestSQLiteStore_SearchBM25_PopulatesTitle(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	titled := model.Document{
		RelPath: "acts/proliferation.pdf",
		DocType: "pdf",
		Title:   "Proliferation Financing (Prohibition) Act, 2021",
		Status:  "ok",
	}
	untitled := model.Document{
		RelPath: "notes/raw.md",
		DocType: "md",
		Status:  "ok",
	}
	for _, d := range []model.Document{titled, untitled} {
		if err := st.UpsertDocument(ctx, d); err != nil {
			t.Fatalf("UpsertDocument(%s): %v", d.RelPath, err)
		}
	}

	chunks := []struct {
		label   uint64
		relPath string
		docType string
		text    string
	}{
		{10, titled.RelPath, titled.DocType, "section 35 reporting suspicious transactions"},
		{11, untitled.RelPath, untitled.DocType, "untitled note about reporting"},
	}
	for _, c := range chunks {
		task := model.NewChunkTask(c.label, c.text, "text", model.ChunkMetadata{
			ChunkID: c.label,
			RelPath: c.relPath,
			DocType: c.docType,
			RepType: "raw_text",
		})
		if err := st.UpsertChunkTask(ctx, task); err != nil {
			t.Fatalf("UpsertChunkTask(%d): %v", c.label, err)
		}
	}

	ls, ok := interface{}(st).(model.LexicalSearcher)
	if !ok {
		t.Fatalf("store does not implement model.LexicalSearcher")
	}
	hits, err := ls.SearchBM25(ctx, "reporting", 5, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("expected at least two hits, got %d", len(hits))
	}
	for _, h := range hits {
		switch h.RelPath {
		case titled.RelPath:
			if h.Title != titled.Title {
				t.Errorf("titled hit should carry title; got %q want %q", h.Title, titled.Title)
			}
		case untitled.RelPath:
			if h.Title != "" {
				t.Errorf("untitled hit should have empty title, got %q", h.Title)
			}
		}
	}
}

// TestSQLiteStore_SearchBM25_OrdersMatches is an end-to-end regression guard for
// #373: on the production external-content chunks_fts schema, SearchBM25 must
// return every matched row without error and rank them sensibly. It does not
// itself force the NULL bm25() condition (modernc.org/sqlite computes a real
// score for every in-process index), but it locks in that the lexical path
// returns multiple matches in a stable, defined order — the behaviour the NULL
// regression broke by erroring out the whole query.
func TestSQLiteStore_SearchBM25_OrdersMatches(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "meta.sqlite")
	st := store.NewSQLiteStore(dbPath)
	defer func() { _ = st.Close() }()
	if err := st.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	chunks := []struct {
		label uint64
		text  string
	}{
		{1, "annual report covering financial results"},
		{2, "another report about the weather report"},
		{3, "a third report note here"},
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

	ls, ok := interface{}(st).(model.LexicalSearcher)
	if !ok {
		t.Fatalf("SQLiteStore must implement model.LexicalSearcher")
	}
	hits, err := ls.SearchBM25(ctx, "report", 10, "text")
	if err != nil {
		t.Fatalf("SearchBM25: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected all 3 matched chunks, got %d", len(hits))
	}
	// Scores are higher-is-better after negation; ensure they are sorted
	// descending and finite (no NULL placeholder leaked through).
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score < hits[i].Score {
			t.Errorf("hits not sorted by descending score: %v then %v", hits[i-1].Score, hits[i].Score)
		}
		if math.IsInf(hits[i].Score, 0) {
			t.Errorf("hit %d has non-finite score with a real bm25 index: %v", hits[i].ChunkID, hits[i].Score)
		}
	}
}

// TestSearchBM25_NullScoreContract pins the defensive contract added for #373.
// On some external-content FTS5 indexes (observed in the field on persisted
// corpora) bm25() returns NULL for matched rows, which made the old
// `score float64` scan fail with "converting NULL to float64 is unsupported"
// and collapsed the whole lexical path. modernc.org/sqlite never produces that
// NULL for an in-process index, so this test exercises the exact scan + ORDER
// BY shape SearchBM25 relies on against a table whose score column is literally
// NULL, asserting (a) the sql.NullFloat64 scan does not error on NULL and
// (b) NULL-scored rows sort LAST rather than jumping to the top.
func TestSearchBM25_NullScoreContract(t *testing.T) {
	// Driver "sqlite" is registered transitively via the internal/store import.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Stand-in for the SearchBM25 result set: a real (non-NULL) bm25 score, plus
	// a row whose score is NULL to mimic the broken index condition.
	const setup = `
CREATE TABLE hits(chunk_id INTEGER PRIMARY KEY, score REAL);
INSERT INTO hits(chunk_id, score) VALUES (1, 1.5), (2, NULL), (3, 0.5);`
	if _, err := db.Exec(setup); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Same ORDER BY clause SearchBM25 uses: NULL scores sink to the bottom, real
	// scores ascend (lower raw bm25 is better), ties broken by chunk_id.
	rows, err := db.Query(`SELECT chunk_id, score FROM hits ORDER BY score IS NULL, score, chunk_id ASC`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	type result struct {
		id    int64
		score float64
	}
	var got []result
	for rows.Next() {
		var id int64
		var score sql.NullFloat64
		// This is the load-bearing assertion: scanning a NULL into NullFloat64
		// must not error the way a plain float64 did.
		if err := rows.Scan(&id, &score); err != nil {
			t.Fatalf("scan (NULL must be tolerated): %v", err)
		}
		s := math.Inf(-1)
		if score.Valid {
			s = -score.Float64
		}
		got = append(got, result{id: id, score: s})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	// Real scores first (chunk 3 has the better raw bm25 of 0.5, then chunk 1),
	// NULL-scored chunk 2 ranked last.
	wantOrder := []int64{3, 1, 2}
	for i, w := range wantOrder {
		if got[i].id != w {
			t.Errorf("order[%d] = chunk %d, want chunk %d (full order: %+v)", i, got[i].id, w, got)
		}
	}
	if !math.IsInf(got[2].score, -1) {
		t.Errorf("NULL-scored row should map to -Inf (worst), got %v", got[2].score)
	}
}
