package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dirstral/dir2mcp/internal/model"
)

// Hierarchical retrieval store surface (SPEC §5.2 / §9.7, dir2mcp #329).
//
// A `summary` representation is an ADDITIVE representation: it reuses the
// existing representations/chunks/spans tables unchanged — no migration, no join
// table. Its parent→child linkage lives in the representation's `meta_json`
// (`coverage`, §5.2), so expanding a summary hit to the fine chunks beneath it is
// two indexed lookups: the summary's own representation row, then the covered
// chunks of `coverage.source_rep_id`.
//
// Two invariants are enforced HERE rather than trusted from meta_json:
//
//   - the same-document invariant (§5.2): the source representation MUST belong
//     to the summary's own document, so expansion can never cross documents even
//     if a hand-edited/corrupt meta_json says otherwise;
//   - fine-only expansion (§9.7): a summary never expands to another summary.

// SummarySourceRep identifies one candidate source representation of a document
// for summarization (SPEC §16.2 `source_reps`): its rep_id, its owning doc_id,
// its rep_type, and the index_kind of its chunks. Only active (non-deleted)
// representations that actually carry active chunks are reported, so a summary
// is never derived over an empty representation.
type SummarySourceRep struct {
	RepID     int64
	DocID     int64
	RepType   string
	IndexKind string
	Chunks    int
}

// SummarySourceReps returns the active representations of the document at
// relPath that carry at least one active chunk, ordered by rep_id for
// determinism. `summary` representations are excluded: a summary is never itself
// summarized (SPEC §16.2). An empty slice with a nil error means the document
// has no summarizable representation yet.
func (s *SQLiteStore) SummarySourceReps(ctx context.Context, relPath string) ([]SummarySourceRep, error) {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(
		ctx,
		`SELECT r.rep_id, r.doc_id, r.rep_type,
		        COALESCE(MIN(c.index_kind), ''), COUNT(c.chunk_id)
		   FROM representations r
		   JOIN documents d ON d.doc_id = r.doc_id
		   JOIN chunks c ON c.rep_id = r.rep_id AND c.deleted = 0
		  WHERE d.rel_path = ? AND d.deleted = 0 AND r.deleted = 0
		    AND r.rep_type <> ? AND r.rep_type NOT LIKE ?
		  GROUP BY r.rep_id, r.doc_id, r.rep_type
		  ORDER BY r.rep_id ASC`,
		normalizedPath,
		model.SummaryRepType,
		model.SummaryRepType+"-%",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]SummarySourceRep, 0, 4)
	for rows.Next() {
		var rep SummarySourceRep
		if err := rows.Scan(&rep.RepID, &rep.DocID, &rep.RepType, &rep.IndexKind, &rep.Chunks); err != nil {
			return nil, err
		}
		out = append(out, rep)
	}
	return out, rows.Err()
}

// SummarySourceText returns the active chunk texts of repID in ordinal order,
// which is the material a summary is generated from. Chunks with no text (direct
// media chunks, SPEC §8.1.7) are skipped: they carry bytes, not prose.
func (s *SQLiteStore) SummarySourceText(ctx context.Context, repID int64) ([]string, error) {
	if repID <= 0 {
		return nil, fmt.Errorf("rep_id must be > 0")
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(
		ctx,
		`SELECT text FROM chunks
		  WHERE rep_id = ? AND deleted = 0 AND TRIM(text) <> ''
		  ORDER BY ordinal ASC`,
		repID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]string, 0, 16)
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, rows.Err()
}

// summaryRepRow is the summary representation behind a candidate summary chunk.
type summaryRepRow struct {
	repID    int64
	docID    int64
	repType  string
	metaJSON string
}

// ExpandSummaryChunk resolves a `summary` chunk to the FINE chunks it covers
// (SPEC §9.7 step 2). It returns (nil, nil) — never an error — whenever the chunk
// is not a usable summary: an unknown/deleted chunk, a non-summary chunk, an
// unparseable or structurally invalid `coverage`, or a coverage that points
// outside the summary's own document. Coarse-to-fine expansion is a retrieval
// enhancement, so an unusable summary degrades to flat retrieval rather than
// failing the query.
//
// The returned chunks are ordered by ordinal (deterministic) and exclude
// summaries, deleted chunks, and chunks the embedding pipeline rejected
// (embedding_status='error', e.g. quality-gate quarantine) so expansion never
// resurrects content flat retrieval would not surface.
func (s *SQLiteStore) ExpandSummaryChunk(ctx context.Context, chunkID uint64) ([]model.ChunkMetadata, error) {
	if chunkID == 0 {
		return nil, nil
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	var rep summaryRepRow
	err = db.QueryRowContext(
		ctx,
		`SELECT r.rep_id, r.doc_id, r.rep_type, COALESCE(r.meta_json, '')
		   FROM chunks c
		   JOIN representations r ON r.rep_id = c.rep_id
		  WHERE c.chunk_id = ? AND c.deleted = 0 AND r.deleted = 0`,
		int64(chunkID),
	).Scan(&rep.repID, &rep.docID, &rep.repType, &rep.metaJSON)
	if err != nil {
		// A missing row (or a chunk with no representation) is simply "not a
		// summary"; there is nothing to expand.
		return nil, nil
	}
	if !model.IsSummaryRepType(rep.repType) {
		return nil, nil
	}

	var meta model.SummaryMeta
	if err := json.Unmarshal([]byte(rep.metaJSON), &meta); err != nil {
		return nil, nil
	}
	if !meta.Coverage.Valid() || meta.Coverage.SourceRepID == rep.repID {
		return nil, nil
	}
	return s.coveredChunks(ctx, db, rep.docID, meta.Coverage)
}

// coveredChunks selects the fine chunks of coverage.SourceRepID that fall in the
// coverage range (SPEC §5.2). The source representation is re-joined against
// docID so the same-document invariant is enforced by the query itself.
func (s *SQLiteStore) coveredChunks(ctx context.Context, db dbQueryHandle, docID int64, coverage model.SummaryCoverage) ([]model.ChunkMetadata, error) {
	predicate, args := summaryRangePredicate(coverage.Range)
	query := `WITH covered_chunks AS (
	            SELECT c.chunk_id, c.rel_path, c.doc_type, c.rep_type, c.text, c.index_kind,
	                   c.modality, c.media_ref, c.language, c.ordinal
	              FROM chunks c
	              JOIN representations r ON r.rep_id = c.rep_id
	             WHERE c.rep_id = ? AND c.deleted = 0 AND c.embedding_status <> 'error'
	               AND c.rep_type <> ? AND c.rep_type NOT LIKE ?
	               AND r.deleted = 0 AND r.doc_id = ?` + predicate + `
	          ),
	          ranked_spans AS (
	            SELECT s.chunk_id, s.span_kind, s.start, s."end", s.extra_json,
	                   ROW_NUMBER() OVER (PARTITION BY s.chunk_id ORDER BY s.span_id) AS rn
	              FROM spans s
	              JOIN covered_chunks cc ON cc.chunk_id = s.chunk_id
	          )
	          SELECT cc.chunk_id, cc.rel_path, cc.doc_type, cc.rep_type, cc.text, cc.index_kind,
	                 COALESCE(sp.span_kind, ''), COALESCE(sp.start, 0), COALESCE(sp.end, 0),
	                 COALESCE(sp.extra_json, ''), COALESCE(d.title, ''),
	                 cc.modality, cc.media_ref, cc.language, COALESCE(d.mtime_unix, 0)
	            FROM covered_chunks cc
	            LEFT JOIN ranked_spans sp ON sp.chunk_id = cc.chunk_id AND sp.rn = 1
	            LEFT JOIN documents d ON d.rel_path = cc.rel_path
	           ORDER BY cc.ordinal ASC, cc.chunk_id ASC`

	fullArgs := append([]any{coverage.SourceRepID, model.SummaryRepType, model.SummaryRepType + "-%", docID}, args...)
	rows, err := db.QueryContext(ctx, query, fullArgs...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]model.ChunkMetadata, 0, 16)
	for rows.Next() {
		var (
			chunkID   int64
			relPath   string
			docType   string
			repType   string
			text      string
			indexKind string
			spanK     string
			spanS     int
			spanE     int
			spanExtra string
			title     string
			modality  string
			mediaRef  string
			language  string
			mtimeUnix int64
		)
		if err := rows.Scan(&chunkID, &relPath, &docType, &repType, &text, &indexKind,
			&spanK, &spanS, &spanE, &spanExtra, &title, &modality, &mediaRef, &language, &mtimeUnix); err != nil {
			return nil, err
		}
		if chunkID <= 0 {
			return nil, fmt.Errorf("invalid non-positive chunk_id from database: %d", chunkID)
		}
		out = append(out, model.ChunkMetadata{
			ChunkID:   uint64(chunkID),
			RelPath:   relPath,
			Title:     title,
			DocType:   docType,
			RepType:   repType,
			Snippet:   snippet(text, 240),
			Span:      spanFromRow(spanK, spanS, spanE, spanExtra),
			Modality:  modality,
			MediaRef:  mediaRef,
			Language:  language,
			MTimeUnix: mtimeUnix,
		})
	}
	return out, rows.Err()
}

// summaryRangePredicate renders the SQL predicate (and its bind args) that
// selects the fine units of a coverage range (SPEC §5.2):
//
//   - document — no additional predicate: every active chunk of the source rep.
//   - ordinals — the INCLUSIVE ordinal range `start <= ordinal <= end`.
//   - time     — INTERVAL OVERLAP against the chunk's time span:
//     `seg_start_ms <= end_ms AND seg_end_ms >= start_ms`. Overlap, not
//     containment: a segment straddling a window endpoint is evidence the
//     summary was built from and must not be dropped.
//
// The zero/unknown kind is caught by SummaryCoverage.Valid before we get here.
func summaryRangePredicate(r model.SummaryCoverageRange) (string, []any) {
	switch r.Kind {
	case model.SummaryRangeOrdinals:
		return ` AND c.ordinal >= ? AND c.ordinal <= ?`, []any{r.Start, r.End}
	case model.SummaryRangeTime:
		return ` AND EXISTS (
		              SELECT 1 FROM spans sp
		               WHERE sp.chunk_id = c.chunk_id AND sp.span_kind = 'time'
		                 AND sp.start <= ? AND sp."end" >= ?
		            )`, []any{r.EndMS, r.StartMS}
	default: // model.SummaryRangeDocument
		return ``, nil
	}
}
