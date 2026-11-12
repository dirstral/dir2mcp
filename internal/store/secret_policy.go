package store

import (
	"context"
	"errors"
	"os"
	"strings"
)

// SecretPolicyIdentitySetting is the settings key that records the identity of
// the effective security.secret_patterns set the corpus was last fully
// evaluated against (SPEC §7.2, spec 0.93.0). It lives in the same `settings`
// table as pipeline_output_identity, so it survives a restart and costs one read
// per scan.
const SecretPolicyIdentitySetting = "secret_policy_identity"

// SecretPolicyIdentity reads the recorded secret policy identity. An absent
// value returns ("", nil): the caller treats it as "never recorded", which is
// the state of every corpus indexed before the identity existed.
func (s *SQLiteStore) SecretPolicyIdentity(ctx context.Context) (string, error) {
	value, err := s.GetSetting(ctx, SecretPolicyIdentitySetting)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(value), nil
}

// SetSecretPolicyIdentity records the secret policy identity the corpus is now
// evaluated against. The caller writes it only after both halves of the
// re-evaluation completed, so an interrupted pass runs again on the next scan.
func (s *SQLiteStore) SetSecretPolicyIdentity(ctx context.Context, identity string) error {
	return s.SetSetting(ctx, SecretPolicyIdentitySetting, strings.TrimSpace(identity))
}

// ClearSecretExcludedContentHashes makes every withheld document eligible for a
// new secret verdict. It clears documents.content_hash on:
//
//   - every live `secret_excluded` row, and
//   - every live archive container that holds a live `secret_excluded` member,
//     because a member is evaluated again only when its container is
//     re-extracted.
//
// An empty content_hash is the existing "not durably finalized" state: the
// incremental fast path does not skip such a row, the derived-verdict carry
// does not apply to it, and the content gate treats it as changed. So the next
// visit of each cleared document re-reads it and decides again under the
// current patterns. The clear is durable, so a scan interrupted after this call
// still re-evaluates the remaining rows on the next run.
//
// It returns the number of rows it cleared.
func (s *SQLiteStore) ClearSecretExcludedContentHashes(ctx context.Context) (int64, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return 0, err
	}
	defer s.ReleaseDB()

	res, err := db.ExecContext(ctx, `
UPDATE documents
   SET content_hash = ''
 WHERE deleted = 0
   AND content_hash <> ''
   AND (
         status = 'secret_excluded'
      OR (doc_type = 'archive' AND EXISTS (
            SELECT 1 FROM documents AS m
             WHERE m.deleted = 0
               AND m.status = 'secret_excluded'
               AND m.source_type = 'archive_member'
               AND substr(m.rel_path, 1, length(documents.rel_path) + 1) = documents.rel_path || '/'
         ))
   )`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// IndexedDocumentPaths returns the rel_path of every live document with status
// `ok`, ordered by rel_path. These are the documents whose text retrieval can
// return, so they are the ones a broader secret policy must be tested against.
func (s *SQLiteStore) IndexedDocumentPaths(ctx context.Context) ([]string, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(ctx,
		`SELECT rel_path FROM documents WHERE deleted = 0 AND status = 'ok' ORDER BY rel_path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var relPath string
		if err := rows.Scan(&relPath); err != nil {
			return nil, err
		}
		out = append(out, relPath)
	}
	return out, rows.Err()
}

// LiveRepresentationTexts returns the searchable text of each live
// representation of the live document at relPath: the text of its live chunks,
// joined in ordinal order. That is exactly the text `search` and `ask` can
// return for the document, whether it came from the raw bytes or from a derived
// text (extraction, OCR, transcript, translation). The joined form also lets a
// pattern match a value that a chunk boundary splits.
func (s *SQLiteStore) LiveRepresentationTexts(ctx context.Context, relPath string) ([]string, error) {
	normalizedPath, err := normalizeRelPath(relPath)
	if err != nil {
		return nil, err
	}
	db, err := s.ensureDB(ctx)
	if err != nil {
		return nil, err
	}
	defer s.ReleaseDB()

	rows, err := db.QueryContext(ctx, `
SELECT c.rep_id, c.text
  FROM chunks AS c
  JOIN representations AS r ON r.rep_id = c.rep_id
  JOIN documents AS d ON d.doc_id = r.doc_id
 WHERE d.rel_path = ?
   AND d.deleted = 0
   AND r.deleted = 0
   AND c.deleted = 0
 ORDER BY c.rep_id, c.ordinal`, normalizedPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var (
		out     []string
		current int64
		b       strings.Builder
	)
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
		}
		b.Reset()
	}
	for rows.Next() {
		var (
			repID int64
			text  string
		)
		if err := rows.Scan(&repID, &text); err != nil {
			return nil, err
		}
		if repID != current {
			flush()
			current = repID
		}
		b.WriteString(text)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	flush()
	return out, nil
}
