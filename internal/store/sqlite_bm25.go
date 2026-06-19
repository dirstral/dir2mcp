package store

import (
	"context"
	"fmt"
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
	         WHERE chunks_fts MATCH ? AND c.deleted = 0`
	if kind := strings.TrimSpace(indexKind); kind != "" {
		stmt += ` AND c.index_kind = ?`
		args = append(args, kind)
	}
	stmt += ` ORDER BY score, c.chunk_id ASC LIMIT ?`
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
			score    float64
		)
		if err := rows.Scan(&chunkID, &relPath, &docType, &repType, &text, &language, &title, &score); err != nil {
			return nil, err
		}
		if chunkID <= 0 {
			continue
		}
		hits = append(hits, model.SearchHit{
			ChunkID:  uint64(chunkID),
			RelPath:  relPath,
			Title:    title,
			DocType:  docType,
			RepType:  repType,
			Score:    -score,
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
