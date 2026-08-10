package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
)

// RepresentationRow is a lightweight view of one ACTIVE (non-tombstoned)
// representation of a document: enough for a caller to decide whether the active
// pipeline still asks for that output, without loading its chunks or text.
//
// It is the read half of the output-set reconciliation (dir2mcp #692). Ingest
// generates the outputs the CURRENT configuration asks for, then compares this
// list against that desired set and retires whatever is left over. MetaJSON is
// the recorded provenance (§5.2): the caller reads `source`, `language`, and the
// derivation identity from it, so a policy decision never has to parse a
// rep_type string.
type RepresentationRow struct {
	RepID    int64
	RepType  string
	MetaJSON string
}

// ActiveRepresentations returns every active representation of the document at
// relPath, ordered by rep_id for determinism. An empty slice (with a nil error)
// means the document exists but has no representations. The document-missing
// case is reported as os.ErrNotExist, for parity with TranscriptRepresentations
// and RepresentationMetaByType.
//
// This is deliberately a general enumeration rather than a per-type query: the
// caller reconciles the document's WHOLE output set against the active
// pipeline's desired set, so it must see outputs the current pipeline no longer
// produces. A type-scoped query can only ever return the types the caller
// already thought to ask for, which is exactly the blind spot #692 describes.
func (s *SQLiteStore) ActiveRepresentations(ctx context.Context, relPath string) ([]RepresentationRow, error) {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return nil, err
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	var docID int64
	if err := db.QueryRowContext(
		ctx,
		`SELECT doc_id FROM documents WHERE rel_path = ? AND deleted = 0 LIMIT 1`,
		normalizedPath,
	).Scan(&docID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}

	rows, err := db.QueryContext(
		ctx,
		`SELECT rep_id, rep_type, COALESCE(meta_json, '')
		   FROM representations
		  WHERE doc_id = ? AND deleted = 0
		  ORDER BY rep_id ASC`,
		docID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]RepresentationRow, 0)
	for rows.Next() {
		var rep RepresentationRow
		if err := rows.Scan(&rep.RepID, &rep.RepType, &rep.MetaJSON); err != nil {
			return nil, err
		}
		out = append(out, rep)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// SoftDeleteRepresentations tombstones (deleted = 1) the named representations
// of the document at relPath together with their chunks, in ONE transaction, and
// returns how many representations were retired. It is the write half of the
// output-set reconciliation (#692) and the general form of the targeted
// SoftDeleteSidecarTranscripts path.
//
// The chunk tombstone is what removes the retired output from retrieval: SQLite
// `deleted = 1` is the source of truth a vector hit is tested against (§6.6), so
// the stale vectors disappear from the current session AND stay gone after a
// restart, with no separate vector-index eviction step.
//
// repIDs are scoped to relPath, so an id that belongs to another document (or to
// no document) is ignored rather than retired. An empty repIDs list is a no-op.
// The document-missing case is reported as os.ErrNotExist.
func (s *SQLiteStore) SoftDeleteRepresentations(ctx context.Context, relPath string, repIDs []int64) (int, error) {
	if len(repIDs) == 0 {
		return 0, nil
	}
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return 0, err
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return 0, err
	}
	defer s.ReleaseDB()

	var docID int64
	if err := db.QueryRowContext(
		ctx,
		`SELECT doc_id FROM documents WHERE rel_path = ? AND deleted = 0 LIMIT 1`,
		normalizedPath,
	).Scan(&docID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, os.ErrNotExist
		}
		return 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	retired := 0
	for _, repID := range repIDs {
		// Confirm the representation belongs to this document AND is still active
		// before touching it. Re-checking inside the transaction keeps the operation
		// idempotent: a representation another path already retired is not counted
		// twice.
		var owned int64
		if err := tx.QueryRowContext(
			ctx,
			`SELECT rep_id FROM representations WHERE rep_id = ? AND doc_id = ? AND deleted = 0`,
			repID, docID,
		).Scan(&owned); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return 0, err
		}
		// Chunks first, then the representation: a crash between the two leaves the
		// representation live with no live chunks, which retrieval already treats as
		// "nothing to return", rather than a live chunk under a retired parent.
		if _, err := tx.ExecContext(ctx, `UPDATE chunks SET deleted = 1 WHERE rep_id = ?`, owned); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE representations SET deleted = 1 WHERE rep_id = ?`, owned); err != nil {
			return 0, err
		}
		retired++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return retired, nil
}

// PipelineOutputIdentitySetting is the settings key that records the pipeline
// output identity (#692) the corpus was last reconciled against. It lives in the
// same `settings` table as index_format_version, so it survives a restart and
// costs one read per scan.
const PipelineOutputIdentitySetting = "pipeline_output_identity"

// PipelineOutputIdentity reads the recorded pipeline output identity. An absent
// value returns ("", nil): the caller treats it as "never recorded".
func (s *SQLiteStore) PipelineOutputIdentity(ctx context.Context) (string, error) {
	value, err := s.GetSetting(ctx, PipelineOutputIdentitySetting)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// SetPipelineOutputIdentity records the pipeline output identity the corpus is
// now reconciled against. It is written only after a scan completes, so an
// interrupted scan reconciles again on the next run.
func (s *SQLiteStore) SetPipelineOutputIdentity(ctx context.Context, identity string) error {
	return s.SetSetting(ctx, PipelineOutputIdentitySetting, strings.TrimSpace(identity))
}
