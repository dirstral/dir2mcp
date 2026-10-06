package store

import (
	"context"
	"errors"
	"strings"
)

// EmittedArtifact is one subtitle file the pipeline wrote under subtitle
// write-back (SPEC §8.6.14; df-003 §5.6). It exists so a file dir2mcp wrote is
// never mistaken for an authored sidecar: sidecar discovery and the sidecar
// fingerprint skip a record whose on-disk size and mtime still match.
type EmittedArtifact struct {
	// RelPath is the corpus-relative path of the written file (the primary key).
	RelPath string
	// DocID is the media document the artifact was derived from.
	DocID int64
	// Format is vtt | srt | ttml.
	Format string
	// Lang is the transcript language written; empty for a TTML, which carries
	// its languages inline.
	Lang string
	// SizeBytes and MTimeUnix are the file's size and modification time as
	// observed immediately after the write — the ownership test.
	SizeBytes int64
	MTimeUnix int64
	// ContentSHA256 is the hex SHA-256 of the bytes written; the `refresh`
	// policy compares the current render against it.
	ContentSHA256 string
	// EmittedUnix is when the file was written.
	EmittedUnix int64
}

// UpsertEmittedArtifact records (or refreshes) the ownership row for a written
// subtitle file. rel_path is the identity: writing the same path again for the
// same or another representation replaces the row.
func (s *SQLiteStore) UpsertEmittedArtifact(ctx context.Context, a EmittedArtifact) error {
	relPath, err := normalizeRelPath(a.RelPath)
	if err != nil {
		return err
	}
	if a.DocID <= 0 {
		return errors.New("emitted artifact doc_id must be > 0")
	}
	format := strings.ToLower(strings.TrimSpace(a.Format))
	if format == "" {
		return errors.New("emitted artifact format is required")
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()

	_, err = db.ExecContext(ctx, `
INSERT INTO emitted_artifacts (rel_path, doc_id, format, lang, size_bytes, mtime_unix, content_sha256, emitted_unix)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(rel_path) DO UPDATE SET
  doc_id = excluded.doc_id,
  format = excluded.format,
  lang = excluded.lang,
  size_bytes = excluded.size_bytes,
  mtime_unix = excluded.mtime_unix,
  content_sha256 = excluded.content_sha256,
  emitted_unix = excluded.emitted_unix`,
		relPath, a.DocID, format, strings.TrimSpace(a.Lang), a.SizeBytes, a.MTimeUnix, a.ContentSHA256, a.EmittedUnix)
	return err
}

// DeleteEmittedArtifact drops the ownership row for relPath. Deleting a row that
// does not exist is not an error: the file simply was never owned.
func (s *SQLiteStore) DeleteEmittedArtifact(ctx context.Context, relPath string) error {
	normalized, err := normalizeRelPath(relPath)
	if err != nil {
		return err
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return err
	}
	defer s.ReleaseDB()
	_, err = db.ExecContext(ctx, `DELETE FROM emitted_artifacts WHERE rel_path = ?`, normalized)
	return err
}

// EmittedArtifactsForDoc returns the ownership rows of one document, ordered by
// rel_path for determinism. An empty slice means the document owns no file.
func (s *SQLiteStore) EmittedArtifactsForDoc(ctx context.Context, docID int64) ([]EmittedArtifact, error) {
	if docID <= 0 {
		return nil, errors.New("doc_id must be > 0")
	}
	return s.queryEmittedArtifacts(ctx, `WHERE doc_id = ?`, docID)
}

// AllEmittedArtifacts returns every ownership row, ordered by rel_path. Ingest
// loads it once per scan so sidecar discovery can exclude owned files without a
// query per media document.
func (s *SQLiteStore) AllEmittedArtifacts(ctx context.Context) ([]EmittedArtifact, error) {
	return s.queryEmittedArtifacts(ctx, ``)
}

func (s *SQLiteStore) queryEmittedArtifacts(ctx context.Context, where string, args ...any) ([]EmittedArtifact, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(ctx, `
SELECT rel_path, doc_id, format, lang, size_bytes, mtime_unix, content_sha256, emitted_unix
FROM emitted_artifacts `+where+` ORDER BY rel_path`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := []EmittedArtifact{}
	for rows.Next() {
		var a EmittedArtifact
		if err := rows.Scan(&a.RelPath, &a.DocID, &a.Format, &a.Lang, &a.SizeBytes, &a.MTimeUnix, &a.ContentSHA256, &a.EmittedUnix); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
