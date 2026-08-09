package store

import (
	"context"
	"strings"
	"time"
)

// embeddingFailureStamp returns the value to persist in
// chunks.embedding_failed_unix for a row moving into embedding_status. Only a
// failed row carries a timestamp; every other status clears it, so the column
// never outlives the failure it describes (issue #783).
func embeddingFailureStamp(status string, now time.Time) int64 {
	if strings.TrimSpace(status) != "error" {
		return 0
	}
	return now.UTC().Unix()
}

// RequeueFailedChunks moves chunks that are parked in embedding_status='error'
// back to 'pending' so the embed worker picks them up again through
// NextPending, clearing the stale error text, category and failure timestamp
// as it goes. Only chunks whose (normalized) error_category appears in
// categories are touched; passing no categories is a no-op that reports 0.
// The returned count is the number of chunks actually moved.
//
// This is the recovery half of a provider-side failure (issue #783). Before
// it existed the ONLY statement that reset a chunk to pending was the chunk
// upsert, so a corpus stranded by a revoked/rotated credential could only be
// recovered by re-ingesting every affected document — re-running extraction
// (the expensive half, up to a full recognition cascade over a video) purely
// to redo the embed step (seconds). The startup credential probe added for
// issue #399 prevents the failure from starting, but cannot recover a corpus
// whose credential died after the daemon was already up.
//
// Nothing else has to be undone: a chunk in 'error' never had a vector written
// for it, so no index entry is left behind to reconcile. The caller decides
// which categories are worth retrying (see IsRequeueableCategory).
func (s *SQLiteStore) RequeueFailedChunks(ctx context.Context, categories []string) (int64, error) {
	normalized := normalizeRequeueCategories(categories)
	if len(normalized) == 0 {
		return 0, nil
	}

	db, err := s.ensureDB(ctx)
	if err != nil {
		return 0, err
	}
	defer s.ReleaseDB()

	args := make([]any, 0, len(normalized))
	for _, category := range normalized {
		args = append(args, category)
	}
	// The IN list is built from placeholders only; the category values travel as
	// bound parameters, so an operator-supplied name can never reach the SQL text.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(normalized)), ",")
	res, err := db.ExecContext(ctx, `
		UPDATE chunks
		   SET embedding_status = 'pending',
		       embedding_error = '',
		       error_category = '',
		       embedding_failed_unix = 0
		 WHERE deleted = 0
		   AND embedding_status = 'error'
		   AND COALESCE(NULLIF(error_category, ''), 'unknown') IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// normalizeRequeueCategories canonicalizes and de-duplicates the requested
// categories, dropping empties. The empty-to-"unknown" fold matches the SQL
// aggregate in loadFailureCategories, so a category the status output names is
// exactly the category the retry matches.
func normalizeRequeueCategories(categories []string) []string {
	seen := make(map[ErrorCategory]struct{}, len(categories))
	out := make([]string, 0, len(categories))
	for _, raw := range categories {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		category := NormalizeErrorCategory(raw)
		if _, dup := seen[category]; dup {
			continue
		}
		seen[category] = struct{}{}
		out = append(out, string(category))
	}
	return out
}
