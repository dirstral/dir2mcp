package store

import (
	"context"
	"errors"
	"os"
	"strings"
)

// SecretPolicyIdentitySetting is the settings key that records the identity of
// the effective security.secret_patterns set the corpus's `secret_excluded`
// verdicts were reached under. It lives in the same `settings` table as
// pipeline_output_identity, so it survives a restart and costs one read per
// scan.
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

// ResetSecretExcludedForPolicy makes every withheld document eligible for a new
// secret verdict and records identity as the policy the corpus is now evaluated
// under. Both writes commit in one transaction.
//
// It clears documents.content_hash on:
//
//   - every live `secret_excluded` row, and
//   - every live archive container that holds a live `secret_excluded` member,
//     because a member is re-evaluated only when its container is re-extracted.
//
// An empty content_hash is the existing "not durably finalized" state: the
// incremental fast path does not skip such a row, the derived-verdict carry
// does not apply to it, and the content gate treats it as changed. So the next
// visit of each cleared document re-reads it and decides again under the
// current patterns. The clear is durable, so a scan interrupted after this call
// still re-evaluates the remaining rows on the next run.
//
// It returns the number of rows it cleared.
func (s *SQLiteStore) ResetSecretExcludedForPolicy(ctx context.Context, identity string) (int64, error) {
	db, err := s.ensureDB(ctx)
	if err != nil {
		return 0, err
	}
	defer s.ReleaseDB()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
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
	cleared, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		SecretPolicyIdentitySetting,
		strings.TrimSpace(identity),
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return cleared, nil
}
