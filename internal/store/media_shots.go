package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dirstral/dir2mcp/internal/shots"
)

// VideoShotState is one active video document with the state of its shot set
// (SPEC §5.7, §8.10.6): what the shots pass needs to decide whether to detect.
type VideoShotState struct {
	DocID       int64
	RelPath     string
	ContentHash string // the document's done marker; empty while withheld
	HasSet      bool
	// The stored set, when HasSet.
	IdentityJSON   string
	SetContentHash string
	FailedUnix     int64
}

// VideoShotStates lists every active, ok video document with its shot set, in
// rel_path order.
func (s *SQLiteStore) VideoShotStates(ctx context.Context) ([]VideoShotState, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()
	rows, err := db.QueryContext(ctx, `
		SELECT d.doc_id, d.rel_path, COALESCE(d.content_hash, ''),
		       m.doc_id IS NOT NULL,
		       COALESCE(m.identity_json, ''), COALESCE(m.content_hash, ''), COALESCE(m.failed_unix, 0)
		  FROM documents d
		  LEFT JOIN media_shot_sets m ON m.doc_id = d.doc_id
		 WHERE d.deleted = 0 AND d.doc_type = 'video' AND d.status = 'ok'
		 ORDER BY d.rel_path ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []VideoShotState
	for rows.Next() {
		var st VideoShotState
		if err := rows.Scan(&st.DocID, &st.RelPath, &st.ContentHash, &st.HasSet,
			&st.IdentityJSON, &st.SetContentHash, &st.FailedUnix); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ReplaceShots stores a successful detection: the set row (failure cleared)
// and all shots of the document, in one transaction, so a reader never sees
// shots from two detections (SPEC §5.7).
func (s *SQLiteStore) ReplaceShots(ctx context.Context, docID int64, identity shots.Identity, contentHash string, nowUnix int64, list []shots.Shot) error {
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
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO media_shot_sets (doc_id, detector, identity_json, content_hash, created_unix, failed_unix)
		VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT(doc_id) DO UPDATE SET
		  detector = excluded.detector, identity_json = excluded.identity_json,
		  content_hash = excluded.content_hash, created_unix = excluded.created_unix,
		  failed_unix = 0`,
		docID, identity.Detector, identity.JSON(), contentHash, nowUnix); err != nil {
		return fmt.Errorf("store shot set: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM media_shots WHERE doc_id = ?`, docID); err != nil {
		return fmt.Errorf("clear shots: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO media_shots (doc_id, shot_index, start_ms, end_ms) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, sh := range list {
		if _, err := stmt.ExecContext(ctx, docID, sh.Index, sh.StartMS, sh.EndMS); err != nil {
			return fmt.Errorf("store shot %d: %w", sh.Index, err)
		}
	}
	return tx.Commit()
}

// MarkShotsFailed records that the last detection of the document failed
// (SPEC §8.10.7). It keeps the shots of an earlier detection; a document with
// none gets a set row with only the failure.
func (s *SQLiteStore) MarkShotsFailed(ctx context.Context, docID int64, nowUnix int64) error {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()
	_, err = db.ExecContext(ctx, `
		INSERT INTO media_shot_sets (doc_id, failed_unix) VALUES (?, ?)
		ON CONFLICT(doc_id) DO UPDATE SET failed_unix = excluded.failed_unix`,
		docID, nowUnix)
	return err
}

// Shots returns the stored shots of a document in index order. A document
// with no shots returns an empty slice.
func (s *SQLiteStore) Shots(ctx context.Context, docID int64) ([]shots.Shot, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()
	rows, err := db.QueryContext(ctx,
		`SELECT shot_index, start_ms, end_ms FROM media_shots WHERE doc_id = ? ORDER BY shot_index ASC`, docID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []shots.Shot{}
	for rows.Next() {
		var sh shots.Shot
		if err := rows.Scan(&sh.Index, &sh.StartMS, &sh.EndMS); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// deleteShotsForPath removes the shots and the shot set of a tombstoned
// document (SPEC §8.10.6), inside the tombstone transaction.
func deleteShotsForPath(ctx context.Context, tx *sql.Tx, normalizedPath string) error {
	for _, table := range []string{"media_shots", "media_shot_sets"} {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE doc_id IN (SELECT doc_id FROM documents WHERE rel_path = ?)`,
			normalizedPath); err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
	}
	return nil
}
