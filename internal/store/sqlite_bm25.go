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
	// Resolve each hit's real span from the spans table (LEFT JOIN, 1:1 with a
	// chunk) so a BM25 hit carries a genuine line/page/time/region span instead
	// of the degenerate `lines 0-0` placeholder it used to hardcode (issue #403
	// F6). Doing it here — at the query boundary — makes the span correct
	// regardless of whether the hybrid metadata cache happens to be warm; the
	// old code only re-attached a real span on a cache HIT, so a cache miss
	// (reindex/eviction window) leaked a citation pointing at non-existent line 0.
	stmt := `SELECT c.chunk_id, c.rel_path, c.doc_type, c.rep_type, c.text, c.language,
	                COALESCE(d.title, ''), COALESCE(d.mtime_unix, 0),
	                COALESCE(sp.span_kind, ''), COALESCE(sp.start, 0), COALESCE(sp.end, 0),
	                COALESCE(sp.extra_json, ''),
	                bm25(chunks_fts) AS score
	         FROM chunks_fts
	         JOIN chunks c ON c.chunk_id = chunks_fts.rowid
	         LEFT JOIN documents d ON d.rel_path = c.rel_path
	         -- One span row per chunk. The spans table has no UNIQUE(chunk_id) — a
	         -- chunk MAY carry multiple span rows (InsertChunkWithSpans accepts a
	         -- slice), so a bare LEFT JOIN would fan out and count a chunk's BM25
	         -- score N times, consuming the LIMIT with duplicates (optibot #597).
	         -- GROUP BY collapses to a single (primary) span per chunk.
	         LEFT JOIN (SELECT chunk_id, span_kind, start, "end", extra_json
	                    FROM spans GROUP BY chunk_id) sp ON sp.chunk_id = c.chunk_id
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
			chunkID   int64
			relPath   string
			docType   string
			repType   string
			text      string
			language  string
			title     string
			mtimeUnix int64
			spanKind  string
			spanStart int
			spanEnd   int
			spanExtra string
			score     sql.NullFloat64
		)
		if err := rows.Scan(&chunkID, &relPath, &docType, &repType, &text, &language, &title, &mtimeUnix,
			&spanKind, &spanStart, &spanEnd, &spanExtra, &score); err != nil {
			return nil, err
		}
		if chunkID <= 0 {
			continue
		}
		// Reconstruct the persisted span. A chunk with no span row (or an
		// unusable one) reduces to an empty span rather than a misleading
		// `lines 0-0`: bmSpan omits the degenerate lines placeholder so the
		// downstream citation carries no line range instead of line 0 (#403 F6).
		span := bmSpan(spanFromRow(spanKind, spanStart, spanEnd, spanExtra))
		// Negate so higher-is-better matches the vector path. A NULL bm25 score
		// (no usable term statistics for this row) maps to the worst possible
		// final score so it ranks last, consistent with the ORDER BY above.
		hitScore := math.Inf(-1)
		if score.Valid {
			hitScore = -score.Float64
		}
		hits = append(hits, model.SearchHit{
			ChunkID:   uint64(chunkID),
			RelPath:   relPath,
			Title:     title,
			DocType:   docType,
			RepType:   repType,
			Score:     hitScore,
			Snippet:   snippet(text, 240),
			Span:      span,
			Language:  language,
			MTimeUnix: mtimeUnix,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return hits, nil
}

// bmSpan drops a degenerate "lines" span — kind "lines" with a non-positive or
// inverted line range — down to the empty span (issue #403 F6). spanFromRow
// falls back to `Span{Kind:"lines"}` (start/end 0) whenever a stored span is
// missing or unusable; surfacing that as a BM25 hit's span produces a citation
// pointing at line 0, which no client can resolve. Returning the zero Span
// instead makes the hit carry no line range at all (the honest "unknown
// location" state), which downstream serializers render as the schema-valid
// document-level span rather than a phantom `lines 0-0`. Real spans of every
// kind (valid lines, page, time, region) pass through unchanged.
func bmSpan(span model.Span) model.Span {
	if strings.EqualFold(strings.TrimSpace(span.Kind), "lines") &&
		(span.StartLine <= 0 || span.EndLine < span.StartLine) {
		return model.Span{}
	}
	return span
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
