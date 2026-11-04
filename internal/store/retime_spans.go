package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dirstral/dir2mcp/internal/model"
)

// ChunkTimeSpan is one live chunk of a representation together with its time
// span, in ordinal order. The §8.6.2 re-time pass reads a translated transcript
// through it, and the source transcript's per-word timing (Span.Words).
type ChunkTimeSpan struct {
	ChunkID uint64
	Ordinal int
	Text    string
	Span    model.Span
}

// ChunkSpanUpdate rewrites one chunk's time span in place: the same chunk row,
// the same text, the same embedding; only the span row changes.
type ChunkSpanUpdate struct {
	ChunkID uint64
	Span    model.Span
}

// ChunkTimeSpans returns the live chunks of repID that carry a "time" span, in
// ordinal order. A representation without time spans yields an empty slice.
func (s *SQLiteStore) ChunkTimeSpans(ctx context.Context, repID int64) ([]ChunkTimeSpan, error) {
	if repID <= 0 {
		return nil, errors.New("rep_id must be > 0")
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(
		ctx,
		`SELECT c.chunk_id, c.ordinal, c.text, sp.span_kind, sp.start, sp.end, COALESCE(sp.extra_json, '')
		 FROM chunks c
		 JOIN spans sp ON sp.chunk_id = c.chunk_id
		 WHERE c.rep_id = ? AND c.deleted = 0 AND sp.span_kind = 'time'
		 ORDER BY c.ordinal ASC`,
		repID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]ChunkTimeSpan, 0)
	for rows.Next() {
		var (
			chunkID   uint64
			ordinal   int
			text      string
			kind      string
			start     int
			end       int
			extraJSON string
		)
		if err := rows.Scan(&chunkID, &ordinal, &text, &kind, &start, &end, &extraJSON); err != nil {
			return nil, err
		}
		out = append(out, ChunkTimeSpan{
			ChunkID: chunkID,
			Ordinal: ordinal,
			Text:    text,
			Span:    spanFromRow(kind, start, end, extraJSON),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RetimeRepresentation rewrites the time spans of the given chunks of repID and
// the representation's meta_json in one transaction, so the span_rule marker
// and the spans it describes commit together (SPEC §8.6.2). Every update must
// name a live chunk of repID with a time span; otherwise nothing is written.
// The chunk rows, their text, their embeddings and the FTS index are untouched.
func (s *SQLiteStore) RetimeRepresentation(ctx context.Context, repID int64, metaJSON string, updates []ChunkSpanUpdate) error {
	if repID <= 0 {
		return errors.New("rep_id must be > 0")
	}
	if strings.TrimSpace(metaJSON) == "" {
		return errors.New("meta_json must not be empty")
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, u := range updates {
		if err := retimeChunkSpan(ctx, tx, repID, u); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE representations SET meta_json = ? WHERE rep_id = ? AND deleted = 0`, metaJSON, repID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("representation %d is not live", repID)
	}
	return tx.Commit()
}

// retimeChunkSpan rewrites one chunk's time span row inside tx.
func retimeChunkSpan(ctx context.Context, tx dbExecutor, repID int64, u ChunkSpanUpdate) error {
	kind, start, end, extraJSON, err := spanToRow(u.Span)
	if err != nil {
		return fmt.Errorf("chunk %d: %w", u.ChunkID, err)
	}
	if kind != "time" {
		return fmt.Errorf("chunk %d: a re-timed span must be a time span, got %q", u.ChunkID, kind)
	}
	var extra any // NULL when the span carries no metadata, as the insert does
	if extraJSON != "" {
		extra = extraJSON
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE spans SET start = ?, end = ?, extra_json = ?
		 WHERE span_kind = 'time' AND chunk_id IN (
		   SELECT chunk_id FROM chunks WHERE chunk_id = ? AND rep_id = ? AND deleted = 0)`,
		start, end, extra, u.ChunkID, repID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("chunk %d is not a live time-span chunk of representation %d", u.ChunkID, repID)
	}
	return nil
}
