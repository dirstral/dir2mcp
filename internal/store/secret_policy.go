package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

// SecretPolicyIdentitySetting is the settings key that records the identity of
// the effective security.secret_patterns set the corpus is evaluated against
// (SPEC §7.2, spec 0.93.0). It lives in the same `settings` table as
// pipeline_output_identity, so it survives a restart and costs one read per
// scan.
const SecretPolicyIdentitySetting = "secret_policy_identity"

// SecretPolicyPatternsSetting is the settings key that records the sorted
// pattern list behind SecretPolicyIdentitySetting, as a JSON array. The next
// change compares against it to tell a narrower set from a broader one.
// Patterns are configuration, not credentials.
const SecretPolicyPatternsSetting = "secret_policy_patterns"

// SecretPolicyIdentity reads the recorded secret policy identity. An absent
// value returns ("", nil): GetSetting reports an absent key as os.ErrNotExist,
// and the caller treats "" as "never recorded", which is the state of every
// corpus indexed before the identity existed.
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

// SecretPolicyPatterns reads the recorded pattern list. ok is false when no
// list is recorded or the recorded value does not parse; the caller then cannot
// prove that the new set is narrower.
func (s *SQLiteStore) SecretPolicyPatterns(ctx context.Context) ([]string, bool, error) {
	value, err := s.GetSetting(ctx, SecretPolicyPatternsSetting)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	var patterns []string
	if err := json.Unmarshal([]byte(value), &patterns); err != nil {
		return nil, false, nil
	}
	return patterns, true, nil
}

// ResetForSecretPolicy makes the documents a changed secret policy can affect
// eligible for a new verdict, and records the new policy. The clear and the two
// settings commit in ONE transaction, so the store never records a policy
// without the reset that applies it.
//
// It clears documents.content_hash on:
//
//   - every live `secret_excluded` row, and every live archive container that
//     holds a live `secret_excluded` member (a member is evaluated again only
//     when its container is re-extracted);
//   - when reevaluateIndexed is true, also every live `ok` row and every live
//     archive container. That is the broader-policy direction: a new pattern
//     can match an indexed document, and only a fresh read of its source and
//     its derived text decides that exactly.
//
// An empty content_hash is the existing "not durably finalized" state: the
// incremental fast path does not skip such a row, the derived-verdict carry
// does not apply to it, and the content gate treats it as changed. So the next
// visit re-reads the document and decides under the current patterns. The
// state is durable per row: a document that fails or is not reached keeps its
// empty hash and is processed again on a later scan, so the recorded policy
// never claims a verdict that was not reached.
//
// It returns the number of rows it cleared.
func (s *SQLiteStore) ResetForSecretPolicy(ctx context.Context, identity string, patterns []string, reevaluateIndexed bool) (int64, error) {
	encoded, err := json.Marshal(patterns)
	if err != nil {
		return 0, err
	}
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

	indexedClause := "0"
	if reevaluateIndexed {
		indexedClause = "(status = 'ok' OR doc_type = 'archive')"
	}
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
      OR `+indexedClause+`
   )`)
	if err != nil {
		return 0, err
	}
	cleared, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	for _, kv := range [][2]string{
		{SecretPolicyIdentitySetting, strings.TrimSpace(identity)},
		{SecretPolicyPatternsSetting, string(encoded)},
	} {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			kv[0], kv[1]); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return cleared, nil
}
