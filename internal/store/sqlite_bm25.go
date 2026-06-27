package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// SearchBM25 implements model.LexicalSearcher. It runs an FTS5 MATCH query
// against the chunks_fts virtual table and joins back to the chunks/documents
// rows to assemble fully-populated SearchHit values. Pass an empty indexKind
// to search across all chunks (text + code).
//
// The Score field is the negated BM25 score so that callers with "higher is
// better" semantics (matching the vector path) can sort consistently. Raw
// FTS5 bm25() returns lower-is-better scores; we flip the sign at the
// boundary. Rank order is preserved either way.
//
// Defensive NULL handling: on some external-content FTS5 indexes (observed in
// the field with modernc.org/sqlite, see #373), bm25() can return NULL for
// matched rows when the index lacks usable per-document term statistics.
// Scanning a NULL into a plain float64 fails with "converting NULL to float64
// is unsupported", which previously killed the entire lexical path and forced
// a vector-only fallback (returning zero results when the vector index was
// empty). We now scan into sql.NullFloat64 and order NULL-scored rows LAST so a
// missing score degrades a single hit's rank rather than failing the query.
//
// Quarantine filter (#439, F1): chunks the quality gate quarantined carry
// embedding_status='error'. They must not surface via the lexical/hybrid path,
// so the WHERE clause excludes them (embedding_status != 'error'). 'pending'
// chunks remain searchable lexically (they need no embedding to match BM25).
func (s *SQLiteStore) SearchBM25(ctx context.Context, query string, k int, indexKind string) ([]model.SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if k <= 0 {
		k = 10
	}
	matchExpr := sanitizeFTSQuery(query)
	if matchExpr == "" {
		return nil, nil
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	args := []any{matchExpr}
	stmt := `SELECT c.chunk_id, c.rel_path, c.doc_type, c.rep_type, c.text, c.language,
	                COALESCE(d.title, ''),
	                bm25(chunks_fts) AS score
	         FROM chunks_fts
	         JOIN chunks c ON c.chunk_id = chunks_fts.rowid
	         LEFT JOIN documents d ON d.rel_path = c.rel_path
	         WHERE chunks_fts MATCH ? AND c.deleted = 0
	               AND c.embedding_status != 'error'`
	if kind := strings.TrimSpace(indexKind); kind != "" {
		stmt += ` AND c.index_kind = ?`
		args = append(args, kind)
	}
	// `score IS NULL` sorts FALSE(0) before TRUE(1), so non-NULL (real) scores
	// come first and any NULL-scored rows sink to the bottom of the lexical
	// ranking. Without this guard SQLite's ASC ordering would float NULLs to the
	// very top (best position), which is exactly backwards for a missing score.
	stmt += ` ORDER BY score IS NULL, score, c.chunk_id ASC LIMIT ?`
	args = append(args, k)

	rows, err := db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("bm25 query: %w", err)
	}
	defer func() { _ = rows.Close() }()

	hits := make([]model.SearchHit, 0, k)
	for rows.Next() {
		var (
			chunkID  int64
			relPath  string
			docType  string
			repType  string
			text     string
			language string
			title    string
			score    sql.NullFloat64
		)
		if err := rows.Scan(&chunkID, &relPath, &docType, &repType, &text, &language, &title, &score); err != nil {
			return nil, err
		}
		if chunkID <= 0 {
			continue
		}
		// Negate so higher-is-better matches the vector path. A NULL bm25 score
		// (no usable term statistics for this row) maps to the worst possible
		// final score so it ranks last, consistent with the ORDER BY above.
		hitScore := math.Inf(-1)
		if score.Valid {
			hitScore = -score.Float64
		}
		hits = append(hits, model.SearchHit{
			ChunkID:  uint64(chunkID),
			RelPath:  relPath,
			Title:    title,
			DocType:  docType,
			RepType:  repType,
			Score:    hitScore,
			Snippet:  snippet(text, 240),
			Span:     model.Span{Kind: "lines"},
			Language: language,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

// sanitizeFTSQuery escapes user input for safe use as the right-hand side of
// an FTS5 MATCH expression. FTS5 treats characters like quotes, parentheses,
// hyphens, and column qualifiers as syntax. The simplest robust approach for
// arbitrary user queries is to wrap each whitespace-separated term in double
// quotes (FTS5 string-literal form) and OR them together. Doubled inner
// quotes escape literal quote characters.
func sanitizeFTSQuery(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		quoted := `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
		parts = append(parts, quoted)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " OR ")
}
